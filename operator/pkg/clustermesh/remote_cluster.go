// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"context"
	"path"

	cm "github.com/cilium/cilium/pkg/clustermesh"
	"github.com/cilium/cilium/pkg/clustermesh/types"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/lock"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
)

// remoteCluster implements the clustermesh business logic on top of
// common.RemoteCluster.
type remoteCluster struct {
	// name is the name of the cluster
	name string

	// mesh is the cluster mesh this remote cluster belongs to
	mesh *clusterMesh

	// remoteServices is the shared store representing services in remote
	// clusters
	remoteServices store.WatchStore

	storeFactory store.Factory

	// synced tracks the initial synchronization with the remote cluster.
	synced synced
}

func (rc *remoteCluster) Run(ctx context.Context, backend kvstore.BackendOperations, config *cmtypes.CiliumClusterConfig, ready chan<- error) {
	var capabilities types.CiliumClusterConfigCapabilities
	if config != nil {
		capabilities = config.Capabilities
	}

	var mgr store.WatchStoreManager
	if capabilities.SyncedCanaries {
		mgr = rc.storeFactory.NewWatchStoreManager(backend, rc.name)
	} else {
		mgr = store.NewWatchStoreManagerImmediate(rc.name)
	}

	adapter := func(prefix string) string { return prefix }
	if capabilities.Cached {
		adapter = kvstore.StateToCachePrefix
	}

	mgr.Register(adapter(serviceStore.ServiceStorePrefix), func(ctx context.Context) {
		rc.remoteServices.Watch(ctx, backend, path.Join(adapter(serviceStore.ServiceStorePrefix), rc.name))
	})

	close(ready)
	mgr.Run(ctx)
}

func (rc *remoteCluster) Stop() {
	rc.synced.stop()
}

func (rc *remoteCluster) Remove() {
	// Draining shall occur only when the configuration for the remote cluster
	// is removed, and not in case the operator is shutting down, otherwise we
	// would break existing connections on restart.
	rc.remoteServices.Drain()

	// TODO
	// rc.mesh.globalServices.onClusterDelete(rc.name)
}

func (rc *remoteCluster) ClusterConfigRequired() bool { return false }

type synced struct {
	services *lock.StoppableWaitGroup
	stopped  chan struct{}
}

func newSynced() synced {
	// Use a StoppableWaitGroup for identities, instead of a plain channel to
	// avoid having to deal with the possibility of a closed channel if already
	// synced (as the callback is executed every time the etcd connection
	// is restarted, differently from the other resource types).
	idswg := lock.NewStoppableWaitGroup()
	idswg.Add()
	idswg.Stop()

	return synced{
		services: lock.NewStoppableWaitGroup(),
		stopped:  make(chan struct{}),
	}
}

// Services returns after that the initial list of shared services has been
// received from the remote cluster, and synchronized with the BPF datapath,
// the remote cluster is disconnected, or the given context is canceled.
func (s *synced) Services(ctx context.Context) error {
	return s.wait(ctx, s.services.WaitChannel())
}

func (s *synced) wait(ctx context.Context, chs ...<-chan struct{}) error {
	for _, ch := range chs {
		select {
		case <-ch:
			continue
		case <-s.stopped:
			return cm.ErrRemoteClusterDisconnected
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (s *synced) stop() {
	close(s.stopped)
}
