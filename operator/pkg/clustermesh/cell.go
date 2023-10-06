// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"time"

	"github.com/spf13/pflag"

	"github.com/cilium/cilium/pkg/clustermesh/common"
	"github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/hive/cell"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
)

const subsystem = "clustermesh"

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, subsystem)

// Cell is the cell for the Operator ClusterMesh
var Cell = cell.Module(
	"operator-clustermesh",
	"Operator ClusterMesh is the Cilium multicluster compnent in the Cilium operator",
	cell.Config(ClusterMeshConfig{}),
	cell.Provide(newClusterMesh),
	// Invoke an empty function which takes an clusterMesh to force its construction.
	cell.Invoke(func(*clusterMesh) {}),

	cell.Config(common.Config{}),

	cell.Metric(NewMetrics),
	cell.Metric(common.MetricsProvider(subsystem)),
)

type clusterMeshParams struct {
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

// ClusterMeshCnfig contains the configuration for ClusterMesh inside the operator.
type ClusterMeshConfig struct {
	// ConurrentClusterMeshEndpointSync the number of service endpoint syncing operations
	// that will be done concurrently by the EndpointSlice Cluster Mesh controller.
	ConurrentClusterMeshEndpointSync int `mapstructure:"concurrent-service-endpoint-syncs"`
	// ClusterMeshEndpointUpdatesBatchPeriod describes the length of endpoint updates batching period.
	// Processing of cluster service changes will be delayed by this duration to join them with potential
	// upcoming updates and reduce the overall number of endpoints updates.
	ClusterMeshEndpointUpdatesBatchPeriod time.Duration `mapstructure:"cluster-mesh-endpoint-updates-batch-period"`
	// ClusterMeshEndpointsPerSlice is the maximum number of endpoints that
	// will be added to an EndpointSlice synced from a remote cluster.
	// More endpoints per slice will result in less endpoint slices, but larger resources. Defaults to 100.
	ClusterMeshEndpointsPerSlice int `mapstructure:"cluster-mesh-endpoints-per-slice"`
}

// Flags adds the flags used by ClientConfig.
func (cfg ClusterMeshConfig) Flags(flags *pflag.FlagSet) {
	flags.IntVar(&cfg.ConurrentClusterMeshEndpointSync,
		"concurrent-service-endpoint-syncs",
		5,
		"Number of service endpoint syncing operations that will be done concurrently for ")
	flags.DurationVar(&cfg.ClusterMeshEndpointUpdatesBatchPeriod,
		"cluster-mesh-endpoint-updates-batch-period",
		time.Millisecond*100,
		"Describes the length of endpoint updates batching period")
	flags.IntVar(&cfg.ClusterMeshEndpointsPerSlice,
		"cluster-mesh-endpoints-per-slice",
		100,
		"Maximum number of endpoints that will be added to an endpoint slice synchronized from other clusters in the mesh")
}
