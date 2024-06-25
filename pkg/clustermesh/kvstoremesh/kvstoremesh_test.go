// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kvstoremesh

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/clustermesh-apiserver/syncstate"
	"github.com/cilium/cilium/pkg/clustermesh/common"
	"github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/clustermesh/utils"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/testutils"
)

// Configure a generous timeout to prevent flakes when running in a noisy CI environment.
var (
	tick    = 10 * time.Millisecond
	timeout = 5 * time.Second
)

type remoteEtcdClientWrapper struct {
	kvstore.BackendOperations
	name   string
	cached bool

	kvs map[string]string
	mu  lock.Mutex

	syncedCanariesWatched bool
}

// Override the ListAndWatch method so that we can propagate whatever event we want without key conflicts with
// those eventually created by kvstoremesh. Additionally, this also allows to track which prefixes have been watched.
func (w *remoteEtcdClientWrapper) ListAndWatch(ctx context.Context, prefix string, chanSize int) *kvstore.Watcher {
	events := make(kvstore.EventChan, 10)

	w.mu.Lock()
	defer w.mu.Unlock()

	if prefix == fmt.Sprintf("cilium/synced/%s/", w.name) {
		state := "state"
		if w.cached {
			state = "cache"
		}

		w.syncedCanariesWatched = true
		events <- kvstore.KeyValueEvent{Typ: kvstore.EventTypeCreate, Key: fmt.Sprintf("cilium/synced/%s/cilium/%s/nodes/v1", w.name, state)}
		events <- kvstore.KeyValueEvent{Typ: kvstore.EventTypeCreate, Key: fmt.Sprintf("cilium/synced/%s/cilium/%s/services/v1", w.name, state)}
		events <- kvstore.KeyValueEvent{Typ: kvstore.EventTypeCreate, Key: fmt.Sprintf("cilium/synced/%s/cilium/%s/serviceexports/v1", w.name, state)}
		events <- kvstore.KeyValueEvent{Typ: kvstore.EventTypeCreate, Key: fmt.Sprintf("cilium/synced/%s/cilium/%s/identities/v1", w.name, state)}
		events <- kvstore.KeyValueEvent{Typ: kvstore.EventTypeCreate, Key: fmt.Sprintf("cilium/synced/%s/cilium/%s/ip/v1", w.name, state)}
	} else {
		for key, value := range w.kvs {
			var found bool
			if strings.HasPrefix(key, prefix) {
				events <- kvstore.KeyValueEvent{Typ: kvstore.EventTypeCreate, Key: key, Value: []byte(value)}
				found = true
				delete(w.kvs, key)
			}

			if found {
				events <- kvstore.KeyValueEvent{Typ: kvstore.EventTypeListDone}
			}
		}
	}

	go func() {
		<-ctx.Done()
		close(events)
	}()

	return &kvstore.Watcher{Events: events}
}

func ptrBool(b bool) *bool {
	return &b
}

func TestRemoteClusterRun(t *testing.T) {
	testutils.IntegrationTest(t)

	kvstore.SetupDummyWithConfigOpts(t, "etcd",
		// Explicitly set higher QPS than the default to speedup the test
		map[string]string{kvstore.EtcdRateLimitOption: "100"},
	)

	tests := []struct {
		name   string
		srccfg types.CiliumClusterConfig
		dstcfg types.CiliumClusterConfig
		kvs    map[string]string
	}{
		{
			name:   "remote cluster has empty cluster config",
			srccfg: types.CiliumClusterConfig{},
			dstcfg: types.CiliumClusterConfig{
				Capabilities: types.CiliumClusterConfigCapabilities{
					SyncedCanaries: true,
					Cached:         true,
				},
			},
			kvs: map[string]string{
				"cilium/state/nodes/v1/foo/bar":          "qux1",
				"cilium/state/services/v1/foo/bar":       "qux2",
				"cilium/state/serviceexports/v1/foo/bar": "qux3",
				"cilium/state/identities/v1/bar":         "qux4",
				"cilium/state/ip/v1/default/bar":         "qux5",
			},
		},
		{
			name: "remote cluster supports the synced canaries",
			srccfg: types.CiliumClusterConfig{
				Capabilities: types.CiliumClusterConfigCapabilities{
					ServiceExportsEnabled: ptrBool(true),
					SyncedCanaries:        true,
				},
			},
			dstcfg: types.CiliumClusterConfig{
				Capabilities: types.CiliumClusterConfigCapabilities{
					SyncedCanaries:        true,
					Cached:                true,
					ServiceExportsEnabled: ptrBool(true),
				},
			},
			kvs: map[string]string{
				"cilium/state/nodes/v1/foo/bar":          "qux1",
				"cilium/state/services/v1/foo/bar":       "qux2",
				"cilium/state/serviceexports/v1/foo/bar": "qux3",
				"cilium/state/identities/v1/bar":         "qux4",
				"cilium/state/ip/v1/default/bar":         "qux5",
			},
		},
		{
			name: "remote cluster supports the cached prefixes",
			srccfg: types.CiliumClusterConfig{
				ID: 10,
				Capabilities: types.CiliumClusterConfigCapabilities{
					Cached:                true,
					ServiceExportsEnabled: ptrBool(false),
				},
			},
			dstcfg: types.CiliumClusterConfig{
				ID: 10,
				Capabilities: types.CiliumClusterConfigCapabilities{
					SyncedCanaries:        true,
					Cached:                true,
					ServiceExportsEnabled: ptrBool(false),
				},
			},
			kvs: map[string]string{
				"cilium/cache/nodes/v1/foo/bar":          "qux1",
				"cilium/cache/services/v1/foo/bar":       "qux2",
				"cilium/cache/serviceexports/v1/foo/bar": "qux3",
				"cilium/cache/identities/v1/foo/bar":     "qux4",
				"cilium/cache/ip/v1/foo/bar":             "qux5",
			},
		},
		{
			name: "remote cluster supports both synced canaries and cached prefixes",
			srccfg: types.CiliumClusterConfig{
				ID: 10,
				Capabilities: types.CiliumClusterConfigCapabilities{
					SyncedCanaries:        true,
					Cached:                true,
					ServiceExportsEnabled: ptrBool(true),
				},
			},
			dstcfg: types.CiliumClusterConfig{
				ID: 10,
				Capabilities: types.CiliumClusterConfigCapabilities{
					SyncedCanaries:        true,
					Cached:                true,
					ServiceExportsEnabled: ptrBool(true),
				},
			},
			kvs: map[string]string{
				"cilium/cache/nodes/v1/foo/bar":          "qux1",
				"cilium/cache/services/v1/foo/bar":       "qux2",
				"cilium/cache/serviceexports/v1/foo/bar": "qux3",
				"cilium/cache/identities/v1/foo/bar":     "qux4",
				"cilium/cache/ip/v1/foo/bar":             "qux5",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wg sync.WaitGroup
			ctx, cancel := context.WithCancel(context.Background())

			t.Cleanup(func() {
				cancel()
				wg.Wait()

				require.NoError(t, kvstore.Client().DeletePrefix(context.Background(), kvstore.BaseKeyPrefix))
			})

			remoteClient := &remoteEtcdClientWrapper{
				BackendOperations: kvstore.Client(),
				name:              "foo",
				cached:            tt.srccfg.Capabilities.Cached,
				kvs:               tt.kvs,
			}
			st := store.NewFactory(store.MetricsProvider())
			km := KVStoreMesh{backend: kvstore.Client(), storeFactory: st, logger: logrus.New()}

			rc := km.newRemoteCluster("foo", nil)
			ready := make(chan error)

			wg.Add(1)
			go func() {
				rc.Run(ctx, remoteClient, tt.srccfg, ready)
				rc.Stop()
				wg.Done()
			}()

			require.NoError(t, <-ready, "rc.Run() failed")

			// Assert that the cluster config got properly propagated
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				cfg, err := utils.GetClusterConfig(ctx, "foo", kvstore.Client())
				assert.NoError(c, err)
				assert.Equal(c, tt.dstcfg, cfg)
			}, timeout, tick, "Failed to retrieve the cluster config")

			expectedReflected := map[string]string{
				"cilium/cache/nodes/v1/foo/bar":      "qux1",
				"cilium/cache/services/v1/foo/bar":   "qux2",
				"cilium/cache/identities/v1/foo/bar": "qux4",
				"cilium/cache/ip/v1/foo/bar":         "qux5",
			}
			if tt.srccfg.Capabilities.ServiceExportsEnabled != nil {
				expectedReflected["cilium/cache/serviceexports/v1/foo/bar"] = "qux3"
			}
			// Assert that the keys have been properly reflected
			for key, value := range expectedReflected {
				require.EventuallyWithTf(t, func(c *assert.CollectT) {
					v, err := kvstore.Client().Get(ctx, key)
					assert.NoError(c, err)
					assert.Equal(c, value, string(v))
				}, timeout, tick, "Expected key %q does not seem to have the correct value %q", key, value)
			}

			expectedSyncedCanaries := []string{
				"cilium/synced/foo/cilium/cache/nodes/v1",
				"cilium/synced/foo/cilium/cache/services/v1",
				"cilium/synced/foo/cilium/cache/identities/v1",
				"cilium/synced/foo/cilium/cache/ip/v1",
			}
			if tt.srccfg.Capabilities.ServiceExportsEnabled != nil {
				expectedSyncedCanaries = append(expectedSyncedCanaries, "cilium/synced/foo/cilium/cache/serviceexports/v1")
			}
			// Assert that the sync canaries have been properly set
			for _, key := range expectedSyncedCanaries {
				require.EventuallyWithTf(t, func(c *assert.CollectT) {
					v, err := kvstore.Client().Get(ctx, key)
					assert.NoError(c, err)
					assert.NotEmpty(c, string(v))
				}, timeout, tick, "Expected sync canary %q is not correctly present", key)
			}

			// Assert that synced canaries have been watched if expected
			require.Equal(t, tt.srccfg.Capabilities.SyncedCanaries, remoteClient.syncedCanariesWatched)
		})
	}
}

func TestRemoteClusterStatus(t *testing.T) {
	testutils.IntegrationTest(t)

	kvstore.SetupDummy(t, "etcd")

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())

	t.Cleanup(func() {
		cancel()
		wg.Wait()

		require.NoError(t, kvstore.Client().DeletePrefix(context.Background(), kvstore.BaseKeyPrefix))
	})

	remoteClient := &remoteEtcdClientWrapper{
		BackendOperations: kvstore.Client(),
		name:              "foo",
		kvs: map[string]string{
			"cilium/state/nodes/v1/foo/bar":          "qux0",
			"cilium/state/nodes/v1/foo/baz":          "qux1",
			"cilium/state/services/v1/foo/bar":       "qux2",
			"cilium/state/services/v1/foo/baz":       "qux3",
			"cilium/state/services/v1/foo/qux":       "qux4",
			"cilium/state/serviceexports/v1/foo/qux": "qux5",
			"cilium/state/identities/v1/bar":         "qux6",
			"cilium/state/ip/v1/default/fred":        "qux7",
			"cilium/state/ip/v1/default/bar":         "qux8",
			"cilium/state/ip/v1/default/baz":         "qux9",
			"cilium/state/ip/v1/default/qux":         "qux10",
		},
	}
	st := store.NewFactory(store.MetricsProvider())
	km := KVStoreMesh{backend: kvstore.Client(), storeFactory: st, logger: logrus.New()}

	rc := km.newRemoteCluster("foo", func() *models.RemoteCluster {
		return &models.RemoteCluster{Ready: true}
	})
	cfg := types.CiliumClusterConfig{
		ID: 10, Capabilities: types.CiliumClusterConfigCapabilities{
			SyncedCanaries: true, ServiceExportsEnabled: ptrBool(true),
		},
	}
	ready := make(chan error)

	// Validate the status before watching the remote cluster.
	status := rc.(*remoteCluster).Status()
	require.False(t, status.Ready, "Status should not be ready")

	require.False(t, status.Synced.Nodes, "Nodes should not be synced")
	require.False(t, status.Synced.Services, "Services should not be synced")
	require.False(t, status.Synced.ServiceExports, "Service Exports should not be synced")
	require.False(t, status.Synced.Identities, "Identities should not be synced")
	require.False(t, status.Synced.Endpoints, "Endpoints should not be synced")

	require.EqualValues(t, 0, status.NumNodes, "Incorrect number of nodes")
	require.EqualValues(t, 0, status.NumSharedServices, "Incorrect number of services")
	require.EqualValues(t, 0, status.NumServiceExports, "Incorrect number of service exports")
	require.EqualValues(t, 0, status.NumIdentities, "Incorrect number of identities")
	require.EqualValues(t, 0, status.NumEndpoints, "Incorrect number of endpoints")

	wg.Add(1)
	go func() {
		rc.Run(ctx, remoteClient, cfg, ready)
		rc.Stop()
		wg.Done()
	}()

	require.NoError(t, <-ready, "rc.Run() failed")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status := rc.(*remoteCluster).Status()
		assert.True(c, status.Ready, "Status should be ready")

		assert.True(c, status.Synced.Nodes, "Nodes should be synced")
		assert.True(c, status.Synced.Services, "Services should be synced")
		assert.True(c, status.Synced.ServiceExports, "Service exports should be synced")
		assert.True(c, status.Synced.Identities, "Identities should be synced")
		assert.True(c, status.Synced.Endpoints, "Endpoints should be synced")

		assert.EqualValues(c, 2, status.NumNodes, "Incorrect number of nodes")
		assert.EqualValues(c, 3, status.NumSharedServices, "Incorrect number of services")
		assert.EqualValues(c, 1, status.NumServiceExports, "Incorrect number of service exports")
		assert.EqualValues(c, 1, status.NumIdentities, "Incorrect number of identities")
		assert.EqualValues(c, 4, status.NumEndpoints, "Incorrect number of endpoints")
	}, timeout, tick, "Reported status is not correct")
}

// mockClusterMesh is a mock implementation of the common.ClusterMesh interface
// allowing for direct manipulation of the clusters
type mockClusterMesh struct {
	clusters map[string]*remoteCluster
}

// ForEachRemoteCluster is a mirrored implementation of ClusterMesh.ForEachRemoteCluster that operates on the mocked clusters.
func (m *mockClusterMesh) ForEachRemoteCluster(fn func(common.RemoteCluster) error) error {
	for _, cluster := range m.clusters {
		if err := fn(cluster); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockClusterMesh) NumReadyClusters() int {
	return len(m.clusters)
}

func (m *mockClusterMesh) Start(cell.HookContext) error {
	return nil
}

func (m *mockClusterMesh) Stop(cell.HookContext) error {
	return nil
}

func TestRemoteClusterSync(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		connect bool
		sync    bool
	}{
		{
			name:    "remote cluster successfully syncs",
			config:  DefaultConfig,
			connect: true,
			sync:    true,
		},
		{
			name: "remote cluster fails to connect",
			// use very low timeouts to speed up the test since we expect failures
			config:  Config{PerClusterReadyTimeout: 1 * time.Millisecond, GlobalReadyTimeout: 1 * time.Millisecond},
			connect: false,
			sync:    false,
		},
		{
			name: "remote cluster connects but fails to sync",
			// use a low timeout only for global sync to avoid racing the connected signal
			config:  Config{PerClusterReadyTimeout: 5 * time.Second, GlobalReadyTimeout: 1 * time.Millisecond},
			connect: true,
			sync:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			mockClusterMesh := &mockClusterMesh{
				clusters: make(map[string]*remoteCluster),
			}
			km := KVStoreMesh{
				config: tt.config,
				common: mockClusterMesh,
				logger: logrus.New(),
			}

			rc := &remoteCluster{
				name:         "foo",
				synced:       newSynced(),
				readyTimeout: tt.config.PerClusterReadyTimeout,
				logger:       km.logger.WithField(logfields.ClusterName, "foo"),
			}
			rc.synced.resources.Add()
			rc.synced.resources.Stop()

			mockClusterMesh.clusters[rc.name] = rc

			if tt.connect {
				close(rc.synced.connected)
			}

			// trigger the readiness timeout
			rc.waitForConnection(ctx)

			clusterSyncComplete := func() bool {
				select {
				case <-rc.synced.resources.WaitChannel():
					return true
				default:
					return false
				}
			}

			if tt.connect {
				require.False(t, clusterSyncComplete(), "Cluster sync should not be complete until all resources are done")
				rc.synced.resources.Done()
			}

			require.NoError(t, rc.synced.Resources(ctx), "Still waiting for remote cluster resources")

			ss := syncstate.SyncState{StoppableWaitGroup: lock.NewStoppableWaitGroup()}
			require.False(t, ss.Complete())

			markCompleted := ss.WaitForResource()
			syncedCallback := func(ctx context.Context) {
				markCompleted(ctx)
				ss.Stop()
			}

			if !tt.sync {
				// reset the cluster's synced object so we can simulate a resource never syncing
				rc.synced = newSynced()
				rc.synced.resources.Add()
				rc.synced.resources.Stop()
				require.ErrorIs(t, km.synced(ctx, syncedCallback), context.DeadlineExceeded, "Expected timeout waiting for sync")
			} else {
				require.NoError(t, km.synced(ctx, syncedCallback), "Sync should have completed")
			}

			require.True(t, ss.Complete(), "Global sync not completed")
		})
	}
}
