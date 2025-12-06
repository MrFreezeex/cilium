// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package benchmark

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"runtime"

	"github.com/cilium/hive/cell"
	"github.com/cilium/statedb"
	"github.com/cilium/statedb/reconciler"

	daemonk8s "github.com/cilium/cilium/daemon/k8s"
	"github.com/cilium/cilium/pkg/clustermesh"
	"github.com/cilium/cilium/pkg/clustermesh/common"
	serviceStore "github.com/cilium/cilium/pkg/clustermesh/store"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/datapath/tables"
	"github.com/cilium/cilium/pkg/hive"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client/testutils"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	k8sTestUtils "github.com/cilium/cilium/pkg/k8s/testutils"
	"github.com/cilium/cilium/pkg/kvstore/store"
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

	maglevConfig, _ = maglev.UserConfig{
		TableSize: 1021,
		HashSeed:  maglev.DefaultHashSeed,
	}.ToConfig()
)

func RunBenchmark(testSize int, backends int, iterations int, loglevel slog.Level, validate bool) {
	option.Config.EnableIPv4 = true
	option.Config.EnableIPv6 = true
	option.Config.ClusterID = 1

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: loglevel}))

	svcs := GenerateServices(testSize)
	clusterServices := GenerateClusterServices(svcs, "remote-cluster", 2, backends)

	type bytesClusterService struct {
		bytes []byte
		key   string
	}
	bytesClusterServices := make([]bytesClusterService, 0, len(clusterServices))
	for _, cs := range clusterServices {
		jsonBytes, err := cs.Marshal()
		if err != nil {
			panic(fmt.Sprintf("Failed to marshal ClusterService: %v", err))
		}
		bytesClusterServices = append(bytesClusterServices, bytesClusterService{
			bytes: jsonBytes,
			key:   cs.Namespace + "/" + cs.Name,
		})
	}

	maps := lbmaps.NewFakeLBMaps()

	var (
		writer         *writer.Writer
		db             *statedb.DB
		bo             *lbreconciler.BPFOps
		observer       store.Observer
		keyCreator     store.KeyCreator
		globalServices *common.GlobalServiceCache
	)
	h := testHive(maps, &writer, &db, &bo, &observer, &keyCreator, &globalServices)

	if err := h.Start(log, context.TODO()); err != nil {
		panic(err)
	}
	defer func() {
		if err := h.Stop(log, context.TODO()); err != nil {
			panic(err)
		}
	}()

	// Create the service and frontend entries simulating the local cluster and
	// wait for them to be reconciled
	fmt.Print("Setup local cluster services/frontends ")
	for _, svc := range svcs {
		createServiceAndFrontends(writer, svc)
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
		panic("Timeout waiting for local cluster services/frontends reconciliation.")
	}
	fmt.Println("ok.")

	var runs []run

	for range iterations {
		runtime.GC()
		var memory testutils.MemoryPair
		runtime.ReadMemStats(&memory.Before)

		start := time.Now()

		//
		// Feed remote ClusterServices
		//
		fmt.Print("Iteration: upsert remote ClusterServices ")
		for _, bcs := range bytesClusterServices {
			key := keyCreator()
			if err := key.Unmarshal(bcs.key, bcs.bytes); err != nil {
				panic(fmt.Sprintf("Failed to unmarshal ClusterService: %v", err))
			}

			observer.OnUpdate(key)
		}

		//
		// Feed remote ClusterServices again to check churn on existing services
		//
		fmt.Print("churn ")
		startChurn := time.Now()
		for _, bcs := range bytesClusterServices {
			key := keyCreator()
			if err := key.Unmarshal(bcs.key, bcs.bytes); err != nil {
				panic(fmt.Sprintf("Failed to unmarshal ClusterService: %v", err))
			}

			observer.OnUpdate(key)
		}
		churnDuration := time.Since(startChurn)

		fmt.Print("wait ")
		reconciled = false
		for waitStart := time.Now(); time.Now().Sub(waitStart) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
			reconciled, nextRevision = fastCheckTables(db, writer, testSize, nextRevision)
			if reconciled {
				break
			}
		}
		if !reconciled {
			panic("Timeout waiting for remote ClusterServices reconciliation.")
		}

		insertDuration := time.Since(start)

		if validate {
			fmt.Print("validate ")
			if err := checkTables(db, writer, svcs, clusterServices); err != nil {
				fmt.Printf("checking tables failed with error: %v\n", err)
				panic("")
			}
		}

		runtime.GC()
		runtime.ReadMemStats(&memory.After)

		startDelete := time.Now()

		fmt.Print("delete ")
		//
		// Feed deletion of remote ClusterServices
		//
		for _, bcs := range bytesClusterServices {
			key := keyCreator()
			if err := key.Unmarshal(bcs.key, bcs.bytes); err != nil {
				panic(fmt.Sprintf("Failed to unmarshal ClusterService: %v", err))
			}

			observer.OnDelete(key)
		}

		fmt.Printf("wait ")
		// Check if backends are deleted (frontends and services are kept since
		// we are deletion of remote ClusterServices not the local services/frontends)
		backendsCleaned := false
		for waitStart := time.Now(); time.Now().Sub(waitStart) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
			backendsCleaned = fastCheckBackendsDeleted(db, writer)
			if backendsCleaned {
				break
			}
		}
		if !backendsCleaned {
			txn := db.ReadTxn()
			numBackends := writer.Backends().NumObjects(txn)
			panic(fmt.Sprintf("Expected backends to be deleted, but %d backends remain", numBackends))
		}

		deleteDuration := time.Since(startDelete)

		fmt.Println("ok.")

		runs = append(
			runs,
			run{
				insertDuration: insertDuration,
				churnDuration:  churnDuration,
				deleteDuration: deleteDuration,
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

	fmt.Printf("Churn statistics from N=%d iterations (re-update same ClusterServices):\n", iterations)
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

func GenerateServices(testSize int) []*slim_corev1.Service {
	svcs := make([]*slim_corev1.Service, 0, testSize)

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
		svcs = append(svcs, &tmpSvc)
	}

	return svcs
}

func GenerateClusterServices(svcs []*slim_corev1.Service, clusterName string, clusterID uint32, numBackends int) []*serviceStore.ClusterService {
	clusterServices := make([]*serviceStore.ClusterService, 0, len(svcs))

	for _, svc := range svcs {
		cs := serviceStore.NewClusterService(svc.Name, svc.Namespace)
		cs.Cluster = clusterName
		cs.ClusterID = clusterID
		cs.Shared = true

		// Generate backend IPs from service IP
		svcAddr, err := netip.ParseAddr(svc.Spec.ClusterIP)
		if err != nil {
			panic(err)
		}

		// Generate multiple backends per service
		for i := 0; i < numBackends; i++ {
			backendAddrAs4 := svcAddr.As4()
			backendAddrAs4[0] = 11 + byte(i/256) // Vary first octet for many backends
			backendAddrAs4[1] += byte(i % 256)   // Vary second octet
			backendIPString := netip.AddrFrom4(backendAddrAs4).String()

			cs.Backends[backendIPString] = serviceStore.PortConfiguration{
				svc.Spec.Ports[0].Name: &loadbalancer.L4Addr{
					Protocol: loadbalancer.L4Type(svc.Spec.Ports[0].Protocol),
					Port:     uint16(svc.Spec.Ports[0].Port),
				},
			}
		}

		clusterServices = append(clusterServices, &cs)
	}

	return clusterServices
}

var nodePortAddrs = []netip.Addr{
	netip.MustParseAddr("10.0.0.3"),
	netip.MustParseAddr("2002::1"),
}

func createServiceAndFrontends(writer *writer.Writer, svc *slim_corev1.Service) {
	name := loadbalancer.NewServiceName(svc.Namespace, svc.Name)

	txn := writer.WriteTxn()
	defer txn.Commit()

	// Create the service entry
	writer.UpsertService(txn, &loadbalancer.Service{
		Name:   name,
		Source: source.Kubernetes,
	})

	// Create frontend from the K8s Service
	addr, err := netip.ParseAddr(svc.Spec.ClusterIP)
	if err != nil {
		return
	}

	for _, port := range svc.Spec.Ports {
		writer.UpsertFrontend(txn, loadbalancer.FrontendParams{
			ServiceName: name,
			PortName:    loadbalancer.FEPortName(port.Name),
			Address: loadbalancer.NewL3n4Addr(
				loadbalancer.L4Type(port.Protocol),
				cmtypes.AddrClusterFrom(addr, 0),
				uint16(port.Port),
				loadbalancer.ScopeExternal,
			),
			Type: loadbalancer.SVCType(svc.Spec.Type),
		})
	}
}

func testHive(maps lbmaps.LBMaps,
	writerPtr **writer.Writer,
	db **statedb.DB,
	bo **lbreconciler.BPFOps,
	observerPtr *store.Observer,
	keyCreatorPtr *store.KeyCreator,
	globalServicesPtr **common.GlobalServiceCache,
) *hive.Hive {
	extConfig := loadbalancer.ExternalConfig{
		ZoneMapper: &option.DaemonConfig{},
		EnableIPv4: true,
		EnableIPv6: true,
	}

	return hive.New(
		cell.Module(
			"clustermesh-benchmark",
			"Clustermesh benchmark module",

			k8sClient.FakeClientCell(),
			node.LocalNodeStoreTestCell,

			// StoreFactory needed for watch stores
			store.Cell,

			cell.Provide(
				func() cmtypes.ClusterInfo {
					return cmtypes.ClusterInfo{
						ID:   1,
						Name: "local-cluster",
					}
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

				// Provide clustermesh config
				func() common.Config {
					return common.Config{}
				},

				// Use the production serviceMerger via the exported constructor
				func(clusterInfo cmtypes.ClusterInfo, w *writer.Writer) clustermesh.ServiceMerger {
					return clustermesh.NewServiceMergerForTesting(clusterInfo, w)
				},
			),

			daemonk8s.PodTableCell,

			cell.Invoke(func(
				db_ *statedb.DB,
				w *writer.Writer,
				bo_ *lbreconciler.BPFOps,
				logger *slog.Logger,
				sm clustermesh.ServiceMerger,
				storeFactory store.Factory,
			) {
				*db = db_
				*writerPtr = w
				*bo = bo_

				// Create the global services cache
				globalServices := common.NewGlobalServiceCache(logger)
				*globalServicesPtr = globalServices

				// Create the key creator with validators
				clusterID := uint32(2)
				*keyCreatorPtr = serviceStore.KeyCreator(
					serviceStore.ClusterNameValidator("remote-cluster"),
					serviceStore.NamespacedNameValidator(),
					serviceStore.ClusterIDValidator(&clusterID),
				)

				// Create the shared service observer
				*observerPtr = common.NewSharedServicesObserver(
					logger,
					globalServices,
					sm.MergeExternalServiceUpdate,
					sm.MergeExternalServiceDelete,
				)
			}),

			// Provides [Writer] API and the load-balancing tables.
			writer.Cell,

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

func fastCheckBackendsDeleted(db *statedb.DB, writer *writer.Writer) bool {
	txn := db.ReadTxn()
	return writer.Backends().NumObjects(txn) == 0
}

func checkTables(db *statedb.DB, writer *writer.Writer, svcs []*slim_corev1.Service, clusterServices []*serviceStore.ClusterService) error {
	txn := db.ReadTxn()
	var err error
	type expectedBackend struct {
		clusterID uint32
		portName  string
	}
	type backendKey struct {
		serviceName loadbalancer.ServiceName
		address     loadbalancer.L3n4Addr
	}

	// Build maps for lookup
	csMap := make(map[string]*serviceStore.ClusterService, len(clusterServices))
	expectedBackends := make(map[backendKey]expectedBackend)
	for _, cs := range clusterServices {
		key := cs.Namespace + "/" + cs.Name
		csMap[key] = cs

		serviceName := loadbalancer.NewServiceName(cs.Namespace, cs.Name)
		for backendIP, portConfig := range cs.Backends {
			addrCluster := cmtypes.MustParseAddrCluster(backendIP)
			for portName, l4Addr := range portConfig {
				expectedBackends[backendKey{
					serviceName: serviceName,
					address: loadbalancer.NewL3n4Addr(
						l4Addr.Protocol,
						addrCluster,
						l4Addr.Port,
						loadbalancer.ScopeExternal,
					),
				}] = expectedBackend{
					clusterID: cs.ClusterID,
					portName:  portName,
				}
			}
		}
	}

	svcMap := make(map[string]*slim_corev1.Service, len(svcs))
	for _, svc := range svcs {
		key := svc.Namespace + "/" + svc.Name
		svcMap[key] = svc
	}

	// Check services
	{
		expectedServices := len(clusterServices)
		if servicesNo := writer.Services().NumObjects(txn); servicesNo != expectedServices {
			err = errors.Join(err, fmt.Errorf("Incorrect number of services, got %d, want %d", servicesNo, expectedServices))
		} else {
			for svc := range writer.Services().All(txn) {
				key := svc.Name.Namespace() + "/" + svc.Name.Name()
				want, ok := csMap[key]
				if !ok {
					err = errors.Join(err, fmt.Errorf("Service %s not found in expected ClusterServices", key))
					continue
				}
				if svc.Name.Namespace() != want.Namespace {
					err = errors.Join(err, fmt.Errorf("Incorrect namespace for service %s, got %q, want %q", key, svc.Name.Namespace(), want.Namespace))
				}
				if svc.Name.Name() != want.Name {
					err = errors.Join(err, fmt.Errorf("Incorrect name for service %s, got %q, want %q", key, svc.Name.Name(), want.Name))
				}
				if svc.Source != source.Kubernetes {
					err = errors.Join(err, fmt.Errorf("Incorrect source for service %s, got %q, want %q", key, svc.Source, source.Kubernetes))
				}
			}
		}
	}

	// Check frontends
	{
		expectedFrontends := len(svcs)
		if frontendsNo := writer.Frontends().NumObjects(txn); frontendsNo != expectedFrontends {
			err = errors.Join(err, fmt.Errorf("Incorrect number of frontends, got %d, want %d", frontendsNo, expectedFrontends))
		} else {
			for fe := range writer.Frontends().All(txn) {
				key := fe.ServiceName.Namespace() + "/" + fe.ServiceName.Name()
				want, ok := svcMap[key]
				if !ok {
					err = errors.Join(err, fmt.Errorf("Frontend service %s not found in expected K8s Services", key))
					continue
				}

				// Check frontend address matches the service ClusterIP
				wantIP, _ := netip.ParseAddr(want.Spec.ClusterIP)
				if fe.Address.Addr() != wantIP {
					err = errors.Join(err, fmt.Errorf("Incorrect address for frontend %s, got %v, want %v", key, fe.Address.Addr(), wantIP))
				}

				// Check port matches
				if fe.PortName != loadbalancer.FEPortName(want.Spec.Ports[0].Name) {
					err = errors.Join(err, fmt.Errorf("Incorrect port name for frontend %s, got %v, want %v", key, fe.PortName, loadbalancer.FEPortName(want.Spec.Ports[0].Name)))
				}
				if fe.Address.Port() != uint16(want.Spec.Ports[0].Port) {
					err = errors.Join(err, fmt.Errorf("Incorrect port for frontend %s, got %v, want %v", key, fe.Address.Port(), want.Spec.Ports[0].Port))
				}
				if fe.Address.Protocol() != loadbalancer.L4Type(want.Spec.Ports[0].Protocol) {
					err = errors.Join(err, fmt.Errorf("Incorrect protocol for frontend %s, got %v, want %v", key, fe.Address.Protocol(), want.Spec.Ports[0].Protocol))
				}

				if fe.Status.Kind != reconciler.StatusKindDone {
					err = errors.Join(err, fmt.Errorf("Incorrect status for frontend %s, got %v, want %v", key, fe.Status.Kind, reconciler.StatusKindDone))
				}
			}
		}
	}

	// Check backends
	{
		expectedNumBackends := len(expectedBackends)
		if backendsNo := writer.Backends().NumObjects(txn); backendsNo != expectedNumBackends {
			err = errors.Join(err, fmt.Errorf("Incorrect number of backends, got %d, want %d", backendsNo, expectedNumBackends))
		} else {
			for be := range writer.Backends().All(txn) {
				want, found := expectedBackends[backendKey{serviceName: be.ServiceName, address: be.Address}]
				if !found {
					err = errors.Join(err, fmt.Errorf("Unexpected backend %s for service %s", be.Address.StringWithProtocol(), be.ServiceName.String()))
					continue
				}

				if state, tmpErr := be.State.String(); tmpErr != nil || state != "active" {
					err = errors.Join(err, fmt.Errorf("Incorrect state for backend %v, got %q, want %q", be.Address.AddrCluster(), state, "active"))
				}
				if be.Source != source.ClusterMesh {
					err = errors.Join(err, fmt.Errorf("Incorrect source for backend %v, got %q, want %q", be.Address.AddrCluster(), be.Source, source.ClusterMesh))
				}
				if be.ClusterID != want.clusterID {
					err = errors.Join(err, fmt.Errorf("Incorrect cluster ID for backend %v, got %d, want %d", be.Address.AddrCluster(), be.ClusterID, want.clusterID))
				}
				if want.portName == "" {
					if len(be.PortNames) != 0 {
						err = errors.Join(err, fmt.Errorf("Incorrect port names for backend %v, got %v, want none", be.Address.AddrCluster(), be.PortNames))
					}
				} else if len(be.PortNames) != 1 || be.PortNames[0] != want.portName {
					err = errors.Join(err, fmt.Errorf("Incorrect port names for backend %v, got %v, want [%s]", be.Address.AddrCluster(), be.PortNames, want.portName))
				}
			}
		}
	}

	return err
}
