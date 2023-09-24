// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"context"
	"errors"
	"time"

	clustermesh "github.com/cilium/cilium/pkg/clustermesh"
	"github.com/cilium/cilium/pkg/clustermesh/common"
	"github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/hive/cell"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
	"github.com/spf13/pflag"
)

const subsystem = "operator-clustermesh"

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, subsystem)

// Cell is the cell for the Operator ClusterMesh
var Cell = cell.Module(
	"operator-clustermesh",
	"Operator ClusterMesh is the Cilium multicluster compnent in the Cilium operator",
	cell.Config(ClusterMeshConfig{}),
	cell.Provide(newClusterMesh),
)

// ClusterMeshCnfig contains the configuration for ClusterMesh inside the operator.
type ClusterMeshConfig struct {
	// SyncClusterMeshEndpointSlice synchronizes endpoint slices from other clusters in the mesh
	SyncClusterMeshEndpointSlices bool `mapstructure:"synchronize-cluster-mesh-endpoint-slices"`
	// ConurrentClusterMeshEndpointSync the number of service endpoint syncing operations
	// that will be done concurrently by the EndpointSlice Cluster Mesh controller.
	ConurrentClusterMeshEndpointSync int `mapstructure:"concurrent-service-endpoint-syncs"`
	// ClusterMeshEndpointUpdatesBatchPeriod describes the length of endpoint updates batching period.
	// Processing of pod changes will be delayed by this duration to join them with potential
	// upcoming updates and reduce the overall number of endpoints updates.
	ClusterMeshEndpointUpdatesBatchPeriod time.Duration `mapstructure:"cluster-mesh-endpoint-updates-batch-period"`
	// ClusterMeshEndpointsPerSlice is the maximum number of endpoints that
	// will be added to an EndpointSlice synced from a remote cluster.
	// More endpoints per slice will result in less endpoint slices, but larger resources. Defaults to 100.
	ClusterMeshEndpointsPerSlice int `mapstructure:"cluster-mesh-endpoints-per-slice"`
}

// Flags adds the flags used by ClientConfig.
func (cfg ClusterMeshConfig) Flags(flags *pflag.FlagSet) {
	flags.BoolVar(&cfg.SyncClusterMeshEndpointSlices,
		"synchronize-cluster-mesh-endpoint-slices",
		false,
		"Synchronize endpoint slices from other clusters in the mesh")
	flags.IntVar(&cfg.ConurrentClusterMeshEndpointSync,
		"concurrent-service-endpoint-syncs",
		5,
		"Number of service endpoint syncing operations that will be done concurrently for ")
	flags.DurationVar(&cfg.ClusterMeshEndpointUpdatesBatchPeriod,
		"mesh-auth-spire-agent-socket",
		time.Millisecond*100,
		"Describes the length of endpoint updates batching period")
	flags.IntVar(&cfg.ClusterMeshEndpointsPerSlice,
		"mesh-auth-spire-server-address",
		100,
		"Maximum number of endpoints that will be added to an endpoint slice synchronized from other clusters in the mesh")
}

// clusterMesh is a cache of multiple remote clusters
type clusterMesh struct {
	// common implements the common logic to connect to remote clusters.
	common common.ClusterMesh

	Metrics Metrics
	// globalServices is a list of all global services. The datastructure
	// is protected by its own mutex inside the structure.
	globalServices          *globalServiceCache
	EndpointSliceController *EndpointSliceController
	stopCh                  chan struct{}

	storeFactory store.Factory
}

type params struct {
	cell.In

	common.Config
	Cfg ClusterMeshConfig

	// ClusterInfo is the id/name of the local cluster. This is used for logging and metrics
	ClusterInfo types.ClusterInfo

	Clientset k8sClient.Clientset

	Metrics       Metrics
	CommonMetrics common.Metrics
	StoreFactory  store.Factory
}

func newClusterMesh(lc hive.Lifecycle, params params) *clusterMesh {
	if !params.Cfg.SyncClusterMeshEndpointSlices {
		return nil
	}

	cm := clusterMesh{
		Metrics: params.Metrics,
		globalServices: newGlobalServiceCache(
			params.Metrics.TotalGlobalServices.WithLabelValues(params.ClusterInfo.Name),
		),
		EndpointSliceController: NewEndpointSliceController(
			params.Clientset,
			params.Cfg.ClusterMeshEndpointsPerSlice,
			params.Cfg.ClusterMeshEndpointUpdatesBatchPeriod,
			params.Cfg.ConurrentClusterMeshEndpointSync,
		),
		stopCh:       make(chan struct{}),
		storeFactory: params.StoreFactory,
	}
	cm.common = common.NewClusterMesh(common.Configuration{
		Config:           params.Config,
		ClusterInfo:      params.ClusterInfo,
		NewRemoteCluster: cm.newRemoteCluster,
		Metrics:          params.CommonMetrics,
	})

	lc.Append(&cm.common)
	return &cm
}

func (cm *clusterMesh) newRemoteCluster(name string, status common.StatusFunc) common.RemoteCluster {
	rc := &remoteCluster{
		name:         name,
		mesh:         cm,
		storeFactory: cm.storeFactory,
		synced:       newSynced(),
	}

	rc.remoteServices = cm.storeFactory.NewWatchStore(
		name,
		func() store.Key { return new(serviceStore.ClusterService) },
		&remoteServiceObserver{remoteCluster: rc, swg: rc.synced.services},
		store.RWSWithOnSyncCallback(func(ctx context.Context) { rc.synced.services.Stop() }),
	)

	return rc
}

func (cm *clusterMesh) Start(hive.HookContext) error {
	go cm.EndpointSliceController.Run(cm.stopCh)
	return nil
}

func (cm *clusterMesh) Stop(hive.HookContext) error {
	close(cm.stopCh)
	return nil
}

// SyncedWaitFn is the type of a function to wait for the initial synchronization
// of a given resource type from all remote clusters.
type SyncedWaitFn func(ctx context.Context) error

// ServicesSynced returns after that the initial list of shared services has been
// received from all remote clusters, and synchronized with the BPF datapath.
func (cm *clusterMesh) ServicesSynced(ctx context.Context) error {
	return cm.synced(ctx, func(rc *remoteCluster) SyncedWaitFn { return rc.synced.Services })
}

func (cm *clusterMesh) synced(ctx context.Context, toWaitFn func(*remoteCluster) SyncedWaitFn) error {
	waiters := make([]SyncedWaitFn, 0)
	cm.common.ForEachRemoteCluster(func(rci common.RemoteCluster) error {
		rc := rci.(*remoteCluster)
		waiters = append(waiters, toWaitFn(rc))
		return nil
	})

	for _, wait := range waiters {
		err := wait(ctx)

		// Ignore the error in case the given cluster was disconnected in
		// the meanwhile, as we do not longer care about it.
		if err != nil && !errors.Is(err, clustermesh.ErrRemoteClusterDisconnected) {
			return err
		}
	}
	return nil
}
