// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"context"
	"errors"

	clustermesh "github.com/cilium/cilium/pkg/clustermesh"
	"github.com/cilium/cilium/pkg/clustermesh/common"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/kvstore/store"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
)

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

func newClusterMesh(lc hive.Lifecycle, params clusterMeshParams) *clusterMesh {
	if !params.Clientset.IsEnabled() || params.ClusterMeshConfig == "" {
		return nil
	}

	cm := clusterMesh{
		Metrics: params.Metrics,
		globalServices: newGlobalServiceCache(
			params.Metrics.TotalGlobalServices.WithLabelValues(params.ClusterInfo.Name),
		),
		stopCh:       make(chan struct{}),
		storeFactory: params.StoreFactory,
	}
	cm.EndpointSliceController = NewEndpointSliceController(
		&cm,
		params.Clientset,
		params.Cfg.ClusterMeshEndpointsPerSlice,
		params.Cfg.ClusterMeshEndpointUpdatesBatchPeriod,
		params.Cfg.ConurrentClusterMeshEndpointSync,
	)
	cm.common = common.NewClusterMesh(common.Configuration{
		Config:           params.Config,
		ClusterInfo:      params.ClusterInfo,
		NewRemoteCluster: cm.newRemoteCluster,
		Metrics:          params.CommonMetrics,
	})

	lc.Append(&cm.common)
	lc.Append(&cm)
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
		&remoteServiceObserver{remoteCluster: rc},
		store.RWSWithOnSyncCallback(func(ctx context.Context) { close(rc.synced.services) }),
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
