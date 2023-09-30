// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/metrics/metric"
)

type Metrics struct {
	// TotalGlobalServices tracks the total number of global services.
	TotalGlobalServices metric.Vec[metric.Gauge]
	// EndpointsAddedPerSync tracks the number of endpoints added on each
	// Service sync.
	EndpointsAddedPerSync metric.Vec[metric.Observer]
	// EndpointsRemovedPerSync tracks the number of endpoints removed on each
	// Service sync.
	EndpointsRemovedPerSync metric.Vec[metric.Observer]
	// EndpointsDesired tracks the total number of desired endpoints.
	EndpointsDesired metric.Vec[metric.Gauge]
	// EndpointSlicesChangedPerSync observes the number of EndpointSlices
	// changed per sync.
	EndpointSlicesChangedPerSync metric.Vec[metric.Observer]
	// EndpointSliceSyncs tracks the number of sync operations the controller
	// runs along with their result.
	EndpointSliceSyncs metric.Vec[metric.Counter]
}

func NewMetrics() Metrics {
	return Metrics{
		TotalGlobalServices: metric.NewGaugeVec(metric.GaugeOpts{
			ConfigName: metrics.Namespace + "_" + subsystem + "_global_services",
			Namespace:  metrics.Namespace,
			Subsystem:  subsystem,
			Name:       "global_services",
			Help:       "The total number of global services in the cluster mesh",
		}, []string{metrics.LabelSourceCluster}),
		// EndpointsAddedPerSync tracks the number of endpoints added on each
		// Service sync.
		EndpointsAddedPerSync: metric.NewHistogramVec(
			metric.HistogramOpts{
				ConfigName:                     metrics.Namespace + "_" + subsystem + "_endpoints_added_per_sync",
				Namespace:                      metrics.Namespace,
				Subsystem:                      subsystem,
				Name:                           "endpoints_added_per_sync",
				Help:                           "Number of endpoints added on each Service sync",
				NativeHistogramBucketFactor:    2,
				NativeHistogramZeroThreshold:   2,
				NativeHistogramMaxBucketNumber: 15,
			},
			[]string{},
		),
		// EndpointsRemovedPerSync tracks the number of endpoints removed on each
		// Service sync.
		EndpointsRemovedPerSync: metric.NewHistogramVec(
			metric.HistogramOpts{
				ConfigName:                     metrics.Namespace + "_" + subsystem + "_endpoints_removed_per_sync",
				Subsystem:                      subsystem,
				Name:                           "endpoints_removed_per_sync",
				Help:                           "Number of endpoints removed on each Service sync",
				NativeHistogramBucketFactor:    2,
				NativeHistogramZeroThreshold:   2,
				NativeHistogramMaxBucketNumber: 15,
			},
			[]string{},
		),
		EndpointsDesired: metric.NewGaugeVec(
			metric.GaugeOpts{
				ConfigName: metrics.Namespace + "_" + subsystem + "_endpoints_desired",
				Subsystem:  subsystem,
				Name:       "endpoints_desired",
				Help:       "Number of endpoints desired",
			},
			[]string{},
		),

		// EndpointSlicesChangedPerSync observes the number of EndpointSlices
		// changed per sync.
		EndpointSlicesChangedPerSync: metric.NewHistogramVec(
			metric.HistogramOpts{
				ConfigName: metrics.Namespace + "_" + subsystem + "_endpointslices_changed_per_sync",
				Subsystem:  subsystem,
				Name:       "endpointslices_changed_per_sync",
				Help:       "Number of EndpointSlices changed on each Service sync",
			},
			[]string{},
		),

		// EndpointSliceSyncs tracks the number of sync operations the controller
		// runs along with their result.
		EndpointSliceSyncs: metric.NewCounterVec(
			metric.CounterOpts{
				ConfigName: metrics.Namespace + "_" + subsystem + "_syncs",
				Subsystem:  subsystem,
				Name:       "syncs",
				Help:       "Number of EndpointSlice syncs",
			},
			[]string{"result"}, // either "success", "stale", or "error"
		),
	}
}
