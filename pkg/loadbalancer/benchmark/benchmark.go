// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package benchmark

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/cilium/hive/cell"
	"github.com/cilium/statedb"
	"github.com/cilium/statedb/reconciler"
	k8sRuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"

	daemonk8s "github.com/cilium/cilium/daemon/k8s"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/datapath/tables"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/k8s"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client/testutils"
	"github.com/cilium/cilium/pkg/k8s/resource"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_discovery_v1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"
	k8sTestUtils "github.com/cilium/cilium/pkg/k8s/testutils"
	"github.com/cilium/cilium/pkg/loadbalancer"
	lbmaps "github.com/cilium/cilium/pkg/loadbalancer/maps"
	lbreconciler "github.com/cilium/cilium/pkg/loadbalancer/reconciler"
	"github.com/cilium/cilium/pkg/loadbalancer/reflectors"
	"github.com/cilium/cilium/pkg/loadbalancer/writer"
	"github.com/cilium/cilium/pkg/maglev"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/source"
	"github.com/cilium/cilium/pkg/testutils"
	"github.com/cilium/cilium/pkg/time"
)

var (
	//go:embed testdata/service.yaml
	serviceYaml []byte

	//go:embed testdata/endpointslice.yaml
	endpointSliceYaml []byte

	maglevConfig, _ = maglev.UserConfig{
		TableSize: 1021,
		HashSeed:  maglev.DefaultHashSeed,
	}.ToConfig()
)

func RunBenchmark(testSize int, numEndpoints int, iterations int, loglevel slog.Level, validate bool) {
	option.Config.EnableIPv4 = true
	option.Config.EnableIPv6 = true

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: loglevel}))

	svcs, epSlices := ServicesAndSlices(log, testSize, numEndpoints)

	var maps lbmaps.LBMaps
	if testutils.IsPrivileged() {
		bpfMaps := &lbmaps.BPFLBMaps{
			Log:    log,
			Pinned: false,
			Cfg: loadbalancer.Config{
				UserConfig: loadbalancer.UserConfig{
					RetryBackoffMin:         time.Second,
					RetryBackoffMax:         time.Second,
					LBMapEntries:            3 * testSize,
					LBServiceMapEntries:     3 * testSize,
					LBBackendMapEntries:     3 * testSize,
					LBRevNatEntries:         3 * testSize,
					LBAffinityMapEntries:    3 * testSize,
					LBSourceRangeAllTypes:   false,
					LBSourceRangeMapEntries: 3 * testSize,
					LBMaglevMapEntries:      3 * testSize,
					LBSockRevNatEntries:     3 * testSize,
				},
				NodePortMin: loadbalancer.NodePortMinDefault,
				NodePortMax: loadbalancer.NodePortMaxDefault,
			},
			ExtCfg: loadbalancer.ExternalConfig{
				ZoneMapper:           &option.DaemonConfig{},
				EnableIPv4:           true,
				EnableIPv6:           true,
				KubeProxyReplacement: true,
			},
			MaglevCfg: maglevConfig,
		}
		bpfMaps.Start(context.TODO())
		maps = bpfMaps
	} else {
		maps = lbmaps.NewFakeLBMaps()
	}

	services := make(chan resource.Event[*slim_corev1.Service], 1000)
	endpoints := make(chan resource.Event[*k8s.Endpoints], 1000)

	var (
		writer *writer.Writer
		db     *statedb.DB
		bo     *lbreconciler.BPFOps
	)
	h := testHive(maps, services, endpoints, &writer, &db, &bo)

	if err := h.Start(log, context.TODO()); err != nil {
		panic(err)
	}
	defer func() {
		if err := h.Stop(log, context.TODO()); err != nil {
			panic(err)
		}
	}()

	var runs []run

	for i := range iterations {
		runtime.GC()
		var memory testutils.MemoryPair
		runtime.ReadMemStats(&memory.Before)

		start := time.Now()

		//
		// Feed in all the test objects
		//
		fmt.Printf("Iteration %d: upsert ", i)
		for _, slice := range epSlices {
			endpoints <- upsertEvent(slice)
		}
		for _, svc := range svcs {
			services <- upsertEvent(svc)
		}

		fmt.Print("wait ")
		nextRevision := statedb.Revision(0)
		reconciled := false
		for waitStart := time.Now(); time.Now().Sub(waitStart) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
			reconciled, nextRevision = fastCheckTables(db, writer, testSize, nextRevision)
			if reconciled {
				break
			}
		}
		if !reconciled {
			panic("Timeout waiting for reconciliation.")
		}

		if validate {
			if err := checkTables(db, writer, svcs, epSlices); err != nil {
				fmt.Printf("checking tables failed with error: %v", err)
				panic("")
			} else {
				fmt.Printf("table check succeeded ")
			}
		}

		insertDuration := time.Since(start)

		//
		// Feed in all the test objects again to measure churn by updating the
		// first backend of the first endpoint slice per service (set ready=false)
		//
		fmt.Print("churn ")
		seenServices := sets.New[loadbalancer.ServiceName]()
		startChurn := time.Now()
		for _, epSlice := range epSlices {
			if !seenServices.Has(epSlice.ServiceName) {
				seenServices.Insert(epSlice.ServiceName)
				for addr, backend := range epSlice.Backends {
					backend.Conditions = backend.Conditions &^ k8s.BackendConditionReady
					epSlice.Backends[addr] = backend
					break // Stop after first backend
				}
			}
			endpoints <- upsertEvent(epSlice)
		}
		for _, svc := range svcs {
			services <- upsertEvent(svc)
		}

		fmt.Print("wait ")
		reconciled = false
		for waitStart := time.Now(); time.Now().Sub(waitStart) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
			reconciled, nextRevision = fastCheckTables(db, writer, testSize, nextRevision)
			if reconciled {
				break
			}
		}
		if !reconciled {
			panic("Timeout waiting for churn reconciliation.")
		}
		churnDuration := time.Since(startChurn)

		runtime.GC()
		runtime.ReadMemStats(&memory.After)

		startDelete := time.Now()

		fmt.Print("delete ")
		//
		// Feed in deletions of all objects.
		//
		for _, svc := range svcs {
			services <- deleteEvent(svc)
		}

		for _, slice := range epSlices {
			endpoints <- deleteEvent(slice)
		}

		fmt.Printf("wait ")
		// Tables and maps should now be empty.
		cleanedUp := false
		for waitStart := time.Now(); time.Now().Sub(waitStart) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
			cleanedUp = fastCheckEmptyTablesAndState(db, writer, bo)
			cleanedUp = cleanedUp && bo.LBMaps.IsEmpty()
			if cleanedUp {
				break
			}
		}
		if !cleanedUp {
			dump := lbmaps.DumpLBMaps(bo.LBMaps, false, nil)
			panic(fmt.Sprintf("Expected BPF maps to be empty, instead they contain %d entries:\n%s", len(dump), strings.Join(dump, "\n")))
		}
		fmt.Println("ok.")

		runs = append(
			runs,
			run{
				insertDuration: insertDuration,
				churnDuration:  churnDuration,
				deleteDuration: time.Since(startDelete),
				memstats:       &memory,
			},
		)
	}

	fmt.Println()
	fmt.Printf("Memory statistics from N=%d iterations:\n", iterations)
	testutils.PrintMemoryStats(testutils.MapFunc(runs, run.mem), testSize)
	fmt.Println()

	fmt.Printf("Insert statistics from N=%d iterations:\n", iterations)
	testutils.PrintTimeStats(testutils.MapFunc(runs, run.insert), testSize)

	fmt.Println()
	fmt.Printf("Churn statistics from N=%d iterations (re-update same objects):\n", iterations)
	testutils.PrintTimeStats(testutils.MapFunc(runs, run.churn), testSize)

	fmt.Println()
	fmt.Printf("Delete statistics from N=%d iterations:\n", iterations)
	testutils.PrintTimeStats(testutils.MapFunc(runs, run.delete), testSize)
}

type run struct {
	insertDuration time.Duration
	churnDuration  time.Duration
	deleteDuration time.Duration
	memstats       *testutils.MemoryPair
}

func (r run) insert() time.Duration      { return r.insertDuration }
func (r run) churn() time.Duration       { return r.churnDuration }
func (r run) delete() time.Duration      { return r.deleteDuration }
func (r run) mem() *testutils.MemoryPair { return r.memstats }

func ServicesAndSlices(logger *slog.Logger, testSize int, numEndpoints int) (svcs []*slim_corev1.Service, epSlices []*k8s.Endpoints) {
	svcs = make([]*slim_corev1.Service, 0, testSize)
	epSlices = make([]*k8s.Endpoints, 0, testSize)

	obj, err := k8sTestUtils.DecodeObject(serviceYaml)
	if err != nil {
		panic(err)
	}
	svc := obj.(*slim_corev1.Service)

	svcAddr, err := netip.ParseAddr(svc.Spec.ClusterIP)
	if err != nil {
		panic(err)
	}
	svcAddrAs4 := svcAddr.As4()
	for j := range testSize {
		tmpSvc := *svc
		tmpSvcAddr := svcAddrAs4
		tmpSvcAddr[2] += byte(j / 256)
		tmpSvcAddr[3] += byte(j % 256)
		tmpSvcIPString := netip.AddrFrom4(tmpSvcAddr).String()
		tmpSvc.Spec.ClusterIP = tmpSvcIPString
		tmpSvc.Spec.ClusterIPs = []string{tmpSvcIPString}

		tmpSvc.Name = fmt.Sprintf("%s-%06d", svc.Name, j)

		tmpSvc.Spec.Selector = maps.Clone(svc.Spec.Selector)
		tmpSvc.Spec.Selector["name"] = fmt.Sprintf("%s-%06d", svc.Spec.Selector["name"], j)

		svcs = append(svcs, &tmpSvc)
	}

	obj, err = k8sTestUtils.DecodeObject(endpointSliceYaml)
	if err != nil {
		panic(err)
	}
	slice := obj.(*slim_discovery_v1.EndpointSlice)

	sliceAddr, err := netip.ParseAddr(slice.Endpoints[0].Addresses[0])
	if err != nil {
		panic(err)
	}
	sliceAddrAs4 := sliceAddr.As4()

	const maxEndpointsPerSlice = 100

	for j := range testSize {
		// Calculate how many slices we need for this service
		numSlices := (numEndpoints + maxEndpointsPerSlice - 1) / maxEndpointsPerSlice

		for sliceIdx := 0; sliceIdx < numSlices; sliceIdx++ {
			tmpSlice := *slice

			// Calculate how many endpoints go in this slice
			startEndpoint := sliceIdx * maxEndpointsPerSlice
			endEndpoint := min((sliceIdx+1)*maxEndpointsPerSlice, numEndpoints)
			endpointsInThisSlice := endEndpoint - startEndpoint

			// Create endpoints for this slice
			tmpSlice.Endpoints = make([]slim_discovery_v1.Endpoint, endpointsInThisSlice)
			for i := 0; i < endpointsInThisSlice; i++ {
				globalEndpointIdx := startEndpoint + i
				tmpSliceAddr := sliceAddrAs4
				tmpSliceAddr[0] = 11 + byte(globalEndpointIdx/256) // Vary first octet for many endpoints
				tmpSliceAddr[1] += byte(globalEndpointIdx % 256)   // Vary second octet
				tmpSliceAddr[2] += byte(j / 256)                   // Vary based on service index
				tmpSliceAddr[3] += byte(j % 256)                   // Vary based on service index
				tmpSliceIPString := netip.AddrFrom4(tmpSliceAddr).String()

				// Clone the endpoint from the template
				tmpSlice.Endpoints[i] = *slice.Endpoints[0].DeepCopy()
				tmpSlice.Endpoints[i].Addresses = []string{tmpSliceIPString}
			}

			tmpSlice.Labels = maps.Clone(slice.Labels)
			tmpSlice.Labels["kubernetes.io/service-name"] = fmt.Sprintf("%s-%06d", slice.Labels["kubernetes.io/service-name"], j)

			// Include slice index in name if we have multiple slices per service
			if numSlices > 1 {
				tmpSlice.Name = fmt.Sprintf("%s-%06d-%d", slice.Name, j, sliceIdx)
			} else {
				tmpSlice.Name = fmt.Sprintf("%s-%06d", slice.Name, j)
			}

			epSlices = append(epSlices, k8s.ParseEndpointSliceV1(logger, &tmpSlice))
		}
	}
	return
}

func upsertEvent[Obj k8sRuntime.Object](obj Obj) resource.Event[Obj] {
	return resource.Event[Obj]{
		Object: obj,
		Key:    resource.NewKey(obj),
		Kind:   resource.Upsert,
		Done:   func(error) {},
	}
}

func deleteEvent[Obj k8sRuntime.Object](obj Obj) resource.Event[Obj] {
	return resource.Event[Obj]{
		Object: obj,
		Key:    resource.NewKey(obj),
		Kind:   resource.Delete,
		Done:   func(error) {},
	}
}

func checkTables(db *statedb.DB, writer *writer.Writer, svcs []*slim_corev1.Service, epSlices []*k8s.Endpoints) error {
	txn := db.ReadTxn()
	var err error

	type backendKey struct {
		serviceName loadbalancer.ServiceName
		addr        cmtypes.AddrCluster
	}

	serviceBackends := make(map[loadbalancer.ServiceName]map[cmtypes.AddrCluster]struct{}, len(svcs))
	expectedBackends := make(map[backendKey]*k8s.Backend)
	for _, ep := range epSlices {
		if _, exists := serviceBackends[ep.ServiceName]; !exists {
			serviceBackends[ep.ServiceName] = make(map[cmtypes.AddrCluster]struct{}, len(ep.Backends))
		}
		for addr, backend := range ep.Backends {
			serviceBackends[ep.ServiceName][addr] = struct{}{}
			expectedBackends[backendKey{serviceName: ep.ServiceName, addr: addr}] = backend
		}
	}

	{
		if servicesNo := writer.Services().NumObjects(txn); servicesNo != len(svcs) {
			err = errors.Join(err, fmt.Errorf("Incorrect number of services, got %d, want %d", servicesNo, len(svcs)))
		} else {
			i := 0
			for svc := range writer.Services().All(txn) {
				want := svcs[i]
				if svc.Name.Namespace() != want.Namespace {
					err = errors.Join(err, fmt.Errorf("Incorrect namespace for service #%06d, got %q, want %q", i, svc.Name.Namespace(), want.Namespace))
				}
				if svc.Name.Name() != want.Name {
					err = errors.Join(err, fmt.Errorf("Incorrect name for service #%06d, got %q, want %q", i, svc.Name.Name(), want.Name))
				}
				if svc.Source != "k8s" {
					err = errors.Join(err, fmt.Errorf("Incorrect source for service #%06d, got %q, want %q", i, svc.Source, "k8s"))
				}
				if svc.ExtTrafficPolicy != loadbalancer.SVCTrafficPolicyCluster {
					err = errors.Join(err, fmt.Errorf("Incorrect external traffic policy for service #%06d, got %q, want %q", i, svc.ExtTrafficPolicy, loadbalancer.SVCTrafficPolicyCluster))
				}
				if svc.IntTrafficPolicy != loadbalancer.SVCTrafficPolicyCluster {
					err = errors.Join(err, fmt.Errorf("Incorrect internal traffic policy for service #%06d, got %q, want %q", i, svc.IntTrafficPolicy, loadbalancer.SVCTrafficPolicyCluster))
				}

				i++
			}
		}
	}

	{
		if frontendsNo := writer.Frontends().NumObjects(txn); frontendsNo != len(svcs) {
			err = errors.Join(err, fmt.Errorf("Incorrect number of frontends, got %d, want %d", frontendsNo, len(svcs)))
		} else {
			i := 0
			for fe := range writer.Frontends().All(txn) {
				want := svcs[i]
				if fe.ServiceName.Namespace() != want.Namespace {
					err = errors.Join(err, fmt.Errorf("Incorrect namespace for frontend #%06d, got %q, want %q", i, fe.ServiceName.Namespace(), want.Namespace))
				}
				if fe.ServiceName.Name() != want.Name {
					err = errors.Join(err, fmt.Errorf("Incorrect name for frontend #%06d, got %q, want %q", i, fe.ServiceName.Name(), want.Name))
				}
				wantIP, _ := netip.ParseAddr(want.Spec.ClusterIP)
				if fe.Address.Addr() != wantIP {
					err = errors.Join(err, fmt.Errorf("Incorrect address for frontend #%06d, got %v, want %v", i, fe.Address.Addr(), wantIP))
				}
				if fe.Type != loadbalancer.SVCType(want.Spec.Type) {
					err = errors.Join(err, fmt.Errorf("Incorrect service type for frontend #%06d, got %v, want %v", i, fe.Type, loadbalancer.SVCType(want.Spec.Type)))
				}
				if fe.PortName != loadbalancer.FEPortName(want.Spec.Ports[0].Name) {
					err = errors.Join(err, fmt.Errorf("Incorrect port name for frontend #%06d, got %v, want %v", i, fe.PortName, loadbalancer.FEPortName(want.Spec.Ports[0].Name)))
				}
				if fe.Status.Kind != reconciler.StatusKindDone {
					err = errors.Join(err, fmt.Errorf("Incorrect status for frontend #%06d, got %v, want %v", i, fe.Status.Kind, "Done"))
				}

				backends := slices.Collect(statedb.ToSeq(iter.Seq2[*loadbalancer.Backend, statedb.Revision](fe.Backends)))
				expectedSvcBackends, found := serviceBackends[fe.ServiceName]
				if !found {
					err = errors.Join(err, fmt.Errorf("Unexpected backend service for frontend #%06d: %s", i, fe.ServiceName.String()))
				} else {
					expectedNumBackends := len(expectedSvcBackends)
					if len(backends) != expectedNumBackends {
						err = errors.Join(err, fmt.Errorf("Incorrect number of backends for frontend #%06d, got %d, want %d", i, len(backends), expectedNumBackends))
					} else {
						for wantAddr := range expectedSvcBackends {
							found := false
							for _, be := range backends {
								if be.Address.AddrCluster() == wantAddr {
									found = true
									break
								}
							}
							if !found {
								err = errors.Join(err, fmt.Errorf("Expected backend address %v not found for frontend #%06d", wantAddr, i))
							}
						}
					}
				}

				i++
			}
		}
	}

	{
		expectedNumBackends := 0
		for _, ep := range epSlices {
			expectedNumBackends += len(ep.Backends)
		}
		if backendsNo := writer.Backends().NumObjects(txn); backendsNo != expectedNumBackends {
			err = errors.Join(err, fmt.Errorf("Incorrect number of backends, got %d, want %d", backendsNo, expectedNumBackends))
		} else {
			for be := range writer.Backends().All(txn) {
				wantBe, found := expectedBackends[backendKey{serviceName: be.ServiceName, addr: be.Address.AddrCluster()}]
				if !found {
					err = errors.Join(err, fmt.Errorf("Backend %s/%v not found in expected endpoints", be.ServiceName.String(), be.Address.AddrCluster()))
					continue
				}

				wantPortNames, found := wantBe.Ports[loadbalancer.NewL4Addr(be.Address.Protocol(), be.Address.Port())]
				if !found {
					err = errors.Join(err, fmt.Errorf("Backend port %s not found in expected ports for %v", be.Address.StringWithProtocol(), be.Address.AddrCluster()))
					continue
				}

				if state, tmpErr := be.State.String(); tmpErr != nil || state != "active" {
					err = errors.Join(err, fmt.Errorf("Incorrect state for backend %v, got %q, want %q", be.Address.AddrCluster(), state, "active"))
				}
				if !slices.Equal(be.PortNames, wantPortNames) {
					err = errors.Join(err, fmt.Errorf("Incorrect backend port names for backend %v, got %v, want %v", be.Address.AddrCluster(), be.PortNames, wantPortNames))
				}
			}
		}
	}

	return err
}

var (
	nodePortAddrs = []netip.Addr{
		netip.MustParseAddr("10.0.0.3"),
		netip.MustParseAddr("2002::1"),
	}
)

func testHive(maps lbmaps.LBMaps,
	services chan resource.Event[*slim_corev1.Service],
	endpoints chan resource.Event[*k8s.Endpoints],
	writerPtr **writer.Writer,
	db **statedb.DB,
	bo **lbreconciler.BPFOps,
) *hive.Hive {
	extConfig := loadbalancer.ExternalConfig{
		ZoneMapper: &option.DaemonConfig{},
		EnableIPv4: true,
		EnableIPv6: true,
	}

	return hive.New(
		cell.Module(
			"loadbalancer-test",
			"Test module",

			k8sClient.FakeClientCell(),
			node.LocalNodeStoreTestCell,

			cell.Provide(
				func() cmtypes.ClusterInfo {
					return cmtypes.ClusterInfo{}
				},
				func() loadbalancer.Config {
					return loadbalancer.Config{
						UserConfig:  loadbalancer.DefaultUserConfig,
						NodePortMin: loadbalancer.NodePortMinDefault,
						NodePortMax: loadbalancer.NodePortMaxDefault,
					}
				},
				func() loadbalancer.ExternalConfig { return extConfig },

				func(lc cell.Lifecycle) lbmaps.LBMaps {
					if rm, ok := maps.(*lbmaps.BPFLBMaps); ok {
						lc.Append(rm)
					}
					return maps
				},

				func(lc cell.Lifecycle) (*maglev.Maglev, maglev.Config) {
					m := maglev.New(maglevConfig, lc)
					return m, maglevConfig
				},

				func() (<-chan resource.Event[*slim_corev1.Service], <-chan resource.Event[*k8s.Endpoints]) {
					return services, endpoints
				},
				reflectors.EventStreamForBenchmark,
			),

			daemonk8s.PodTableCell,

			cell.Invoke(func(db_ *statedb.DB, w *writer.Writer, bo_ *lbreconciler.BPFOps) {
				*db = db_
				*writerPtr = w
				*bo = bo_
			}),

			// Provides [Writer] API and the load-balancing tables.
			writer.Cell,

			// Reflects Kubernetes services and endpoints to the load-balancing tables
			// using the [Writer].
			cell.Invoke(reflectors.RegisterK8sReflector),

			// Reconcile tables to BPF maps
			lbreconciler.Cell,

			cell.Provide(reflectors.NetnsCookieSupportFunc),

			cell.Provide(
				tables.NewNodeAddressTable,
				statedb.RWTable[tables.NodeAddress].ToTable,
				source.NewSources,
			),
			cell.Invoke(func(db *statedb.DB, nodeAddrs statedb.RWTable[tables.NodeAddress]) {
				txn := db.WriteTxn(nodeAddrs)

				for _, addr := range nodePortAddrs {
					nodeAddrs.Insert(
						txn,
						tables.NodeAddress{
							Addr:       addr,
							NodePort:   true,
							Primary:    true,
							DeviceName: "eth0",
						},
					)
					nodeAddrs.Insert(
						txn,
						tables.NodeAddress{
							Addr:       addr,
							NodePort:   true,
							Primary:    true,
							DeviceName: "eth0",
						},
					)
				}
				txn.Commit()

			}),
		),
	)
}

func fastCheckTables(db *statedb.DB, writer *writer.Writer, expectedFrontends int, lastPendingRevision statedb.Revision) (reconciled bool, nextRevision statedb.Revision) {
	txn := db.ReadTxn()
	if writer.Frontends().NumObjects(txn) < expectedFrontends {
		return false, 0
	}
	var rev uint64
	var fe *loadbalancer.Frontend
	for fe, rev = range writer.Frontends().LowerBound(txn, statedb.ByRevision[*loadbalancer.Frontend](lastPendingRevision)) {
		if fe.Status.Kind != reconciler.StatusKindDone {
			return false, rev
		}
	}
	return true, rev // Here, it is the last reconciled revision rather than the first non-reconciled revision.
}

func fastCheckEmptyTablesAndState(db *statedb.DB, writer *writer.Writer, bo *lbreconciler.BPFOps) bool {
	txn := db.ReadTxn()
	if writer.Frontends().NumObjects(txn) > 0 || writer.Backends().NumObjects(txn) > 0 || writer.Services().NumObjects(txn) > 0 {
		return false
	}
	return bo.StateIsEmpty()
}
