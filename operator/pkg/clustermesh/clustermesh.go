// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"context"
	"errors"

	"github.com/cilium/cilium/pkg/clustermesh/common"
	"github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/hive/cell"
	clustermesh "github.com/cilium/cilium/pkg/clustermesh"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/promise"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
)

const subsystem = "operator-clustermesh"

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, subsystem)

// clusterMesh is a cache of multiple remote clusters
type clusterMesh struct {
	// common implements the common logic to connect to remote clusters.
	common common.ClusterMesh

	// globalServices is a list of all global services. The datastructure
	// is protected by its own mutex inside the structure.
	globalServices *globalServiceCache

	storeFactory store.Factory
}

type params struct {
	cell.In

	common.Config

	// ClusterInfo is the id/name of the local cluster. This is used for logging and metrics
	ClusterInfo types.ClusterInfo

	BackendPromise promise.Promise[kvstore.BackendOperations]

	Metrics       Metrics
	CommonMetrics common.Metrics
	StoreFactory  store.Factory
}

func newClusterMesh(lc hive.Lifecycle, params params) *clusterMesh {
	cm := clusterMesh{
		globalServices: newGlobalServiceCache(
			params.Metrics.TotalGlobalServices.WithLabelValues(params.ClusterInfo.Name),
		),
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
