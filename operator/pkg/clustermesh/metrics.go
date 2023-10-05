// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"reflect"

	"github.com/blang/semver/v4"

	"github.com/prometheus/client_golang/prometheus"
	endpointslicemetrics "k8s.io/endpointslice/metrics"

	"github.com/cilium/cilium/operator/metrics"
	agentmetrics "github.com/cilium/cilium/pkg/metrics"
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
	// EndpointSlicesChangedPerSync observes the number of EndpointSlices
	// changed per sync.
	EndpointSlicesChangedPerSync metric.Vec[metric.Observer]
	// EndpointSliceChanges tracks the number of changes to Endpoint Slices.
	EndpointSliceChanges metric.Vec[metric.Counter]
	// EndpointSliceSyncs tracks the number of sync operations the controller
	// runs along with their result.
	EndpointSliceSyncs metric.Vec[metric.Counter]
	// NumEndpointSlices tracks the number of EndpointSlices in a cluster.
	NumEndpointSlices metric.Vec[metric.Gauge]
	// DesiredEndpointSlices tracks the number of EndpointSlices that would
	// exist with perfect endpoint allocation.
	DesiredEndpointSlices metric.Vec[metric.Gauge]
	// EndpointsDesired tracks the total number of desired endpoints.
	EndpointsDesired metric.Vec[metric.Gauge]
}

func NewMetrics() Metrics {
	// Some metrics are referenced within k8s code so we have to override
	// the prometheus vec inside kubernetes
	version := semver.MustParse("1.0.0") // This is not really used as we override it later anyway

	numEndpointSlices := metric.NewGaugeVec(
		metric.GaugeOpts{
			ConfigName: metrics.Namespace + "_" + subsystem + "_num_endpoint_slices",
			Subsystem:  subsystem,
			Name:       "endpoints_desired",
			Help:       "Number of endpoints desired",
		},
		[]string{},
	)

	endpointslicemetrics.NumEndpointSlices.Create(&version)
	endpointslicemetrics.NumEndpointSlices.GaugeVec = (*prometheus.GaugeVec)(reflect.ValueOf(numEndpointSlices).Elem().FieldByName("GaugeVec").UnsafePointer())

	desiredEndpointSlices := metric.NewGaugeVec(
		metric.GaugeOpts{
			ConfigName: metrics.Namespace + "_" + subsystem + "_desired_endpoint_slices",
			Subsystem:  subsystem,
			Help:       "Number of EndpointSlices that would exist with perfect endpoint allocation",
		},
		[]string{},
	)
	endpointslicemetrics.DesiredEndpointSlices.Create(&version)
	endpointslicemetrics.DesiredEndpointSlices.GaugeVec = (*prometheus.GaugeVec)(reflect.ValueOf(desiredEndpointSlices).Elem().FieldByName("GaugeVec").UnsafePointer())

	endpointsDesired := metric.NewGaugeVec(
		metric.GaugeOpts{
			ConfigName: metrics.Namespace + "_" + subsystem + "_endpoints_desired",
			Subsystem:  subsystem,
			Name:       "endpoints_desired",
			Help:       "Number of endpoints desired",
		},
		[]string{},
	)
	endpointslicemetrics.EndpointsDesired.Create(&version)
	endpointslicemetrics.EndpointsDesired.GaugeVec = (*prometheus.GaugeVec)(reflect.ValueOf(endpointsDesired).Elem().FieldByName("GaugeVec").UnsafePointer())

	return Metrics{
		TotalGlobalServices: metric.NewGaugeVec(metric.GaugeOpts{
			ConfigName: metrics.Namespace + "_" + subsystem + "_global_services",
			Namespace:  metrics.Namespace,
			Subsystem:  subsystem,
			Name:       "global_services",
			Help:       "The total number of global services in the cluster mesh",
		}, []string{agentmetrics.LabelSourceCluster}),
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

		EndpointSliceChanges: metric.NewCounterVec(
			metric.CounterOpts{
				ConfigName: metrics.Namespace + "_" + subsystem + "_endpoint_slice_changes",
				Subsystem:  subsystem,
				Help:       "Number of EndpointSlice changes",
			},
			[]string{"operation"},
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

		NumEndpointSlices:     numEndpointSlices,
		DesiredEndpointSlices: desiredEndpointSlices,
		EndpointsDesired:      endpointsDesired,
	}
}
