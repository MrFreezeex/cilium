package clustermesh

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/metrics/metric"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/component-base/metrics/testutil"
	endpointslicemetrics "k8s.io/endpointslice/metrics"
	endpointsliceutil "k8s.io/endpointslice/util"
	"k8s.io/utils/pointer"
	mcsapiv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

func expectAction(t *testing.T, actions []k8stesting.Action, index int, verb, resource string) {
	t.Helper()
	if len(actions) <= index {
		t.Fatalf("Expected at least %d actions, got %d", index+1, len(actions))
	}

	action := actions[index]
	if action.GetVerb() != verb {
		t.Errorf("Expected action %d verb to be %s, got %s", index, verb, action.GetVerb())
	}

	if action.GetResource().Resource != resource {
		t.Errorf("Expected action %d resource to be %s, got %s", index, resource, action.GetResource().Resource)
	}
}

// cacheMutationCheck helps ensure that cached objects have not been changed
// in any way throughout a test run.
type cacheMutationCheck struct {
	objects []cacheObject
}

// cacheObject stores a reference to an original object as well as a deep copy
// of that object to track any mutations in the original object.
type cacheObject struct {
	original runtime.Object
	deepCopy runtime.Object
}

// newCacheMutationCheck initializes a cacheMutationCheck with EndpointSlices.
func newCacheMutationCheck(endpointSlices []*discovery.EndpointSlice) cacheMutationCheck {
	cmc := cacheMutationCheck{}
	for _, endpointSlice := range endpointSlices {
		cmc.Add(endpointSlice)
	}
	return cmc
}

// Add appends a runtime.Object and a deep copy of that object into the
// cacheMutationCheck.
func (cmc *cacheMutationCheck) Add(o runtime.Object) {
	cmc.objects = append(cmc.objects, cacheObject{
		original: o,
		deepCopy: o.DeepCopyObject(),
	})
}

// Check verifies that no objects in the cacheMutationCheck have been mutated.
func (cmc *cacheMutationCheck) Check(t *testing.T) {
	for _, o := range cmc.objects {
		if !reflect.DeepEqual(o.original, o.deepCopy) {
			// Cached objects can't be safely mutated and instead should be deep
			// copied before changed in any way.
			t.Errorf("Cached object was unexpectedly mutated. Original: %+v, Mutated: %+v", o.deepCopy, o.original)
		}
	}
}

const (
	defaultMaxEndpointsPerSlice = 100
	defaultClusterName          = "cluster1"
)

func TestPlaceHolderSliceCompare(t *testing.T) {
	testCases := []struct {
		desc string
		x    *discovery.EndpointSlice
		y    *discovery.EndpointSlice
		want bool
	}{
		{
			desc: "Both nil",
			want: true,
		},
		{
			desc: "Y is nil",
			x:    &discovery.EndpointSlice{},
			want: false,
		},
		{
			desc: "X is nil",
			y:    &discovery.EndpointSlice{},
			want: false,
		},
		{
			desc: "Both are empty and non-nil",
			x:    &discovery.EndpointSlice{},
			y:    &discovery.EndpointSlice{},
			want: true,
		},
		{
			desc: "Only ObjectMeta.Name has diff",
			x: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			}},
			y: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Name: "bar",
			}},
			want: true,
		},
		{
			desc: "Only ObjectMeta.Labels has diff",
			x: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"foo": "true",
				},
			}},
			y: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"bar": "true",
				},
			}},
			want: false,
		},
		{
			desc: "Creation time is different",
			x: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.Unix(1, 0),
			}},
			y: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.Unix(2, 0),
			}},
			want: true,
		},
		{
			desc: "Different except for ObjectMeta",
			x:    &discovery.EndpointSlice{AddressType: discovery.AddressTypeIPv4},
			y:    &discovery.EndpointSlice{AddressType: discovery.AddressTypeIPv6},
			want: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			got := placeholderSliceCompare.DeepEqual(tc.x, tc.y)
			if got != tc.want {
				t.Errorf("sliceEqual(%v, %v) = %t, want %t", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

// Even when there are no backends, we want to have a placeholder slice for each service
func TestReconcileEmpty(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, &serviceStore.ClusterService{Cluster: defaultClusterName}, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)
	expectActions(t, client.Actions(), 1, "create", "endpointslices")

	slices := fetchEndpointSlices(t, client, namespace)
	assert.Len(t, slices, 1, "Expected 1 endpoint slices")

	assert.Regexp(t, "^"+svc.Name, slices[0].Name)
	assert.Equal(t, svc.Name, slices[0].Labels[discovery.LabelServiceName])
	assert.Equal(t, defaultClusterName, slices[0].Labels[mcsapiv1alpha1.LabelSourceCluster])
	assert.EqualValues(t, []discovery.EndpointPort{}, slices[0].Ports)
	assert.EqualValues(t, []discovery.Endpoint{}, slices[0].Endpoints)
	expectTrackedGeneration(t, endpointSliceTracker, &slices[0], 1)
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 1, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 1})
}

// Given a simple cluster service and no existing endpoint slices,
// a slice should be created
func TestReconcileSimple(t *testing.T) {
	namespace := "test"
	noFamilyService, _ := newServiceAndEndpointMeta("foo", namespace)
	noFamilyService.Spec.ClusterIP = "10.0.0.10"
	noFamilyService.Spec.IPFamilies = nil

	svcv4, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4ClusterIP, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4ClusterIP.Spec.ClusterIP = "1.1.1.1"
	svcv4Labels, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4Labels.Labels = map[string]string{"foo": "bar"}
	svcv4BadLabels, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4BadLabels.Labels = map[string]string{discovery.LabelServiceName: "bad",
		discovery.LabelManagedBy: "actor", corev1.IsHeadlessService: "invalid"}
	svcv6, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv6.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}
	svcv6ClusterIP, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv6ClusterIP.Spec.ClusterIP = "1234::5678:0000:0000:9abc:def1"
	// newServiceAndEndpointMeta generates v4 single stack
	svcv6ClusterIP.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}

	// dual stack
	dualStackSvc, _ := newServiceAndEndpointMeta("foo", namespace)
	dualStackSvc.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}
	dualStackSvc.Spec.ClusterIP = "10.0.0.10"
	dualStackSvc.Spec.ClusterIPs = []string{"10.0.0.10", "2000::1"}

	clusterSvc1 := newEmptyClusterSvc()
	clusterSvc1.Backends = map[string]serviceStore.PortConfiguration{
		"1.2.3.4":                        map[string]*loadbalancer.L4Addr{},
		"1234::5678:0000:0000:9abc:def0": map[string]*loadbalancer.L4Addr{},
	}

	testCases := map[string]struct {
		service                  corev1.Service
		expectedAddressType      discovery.AddressType
		expectedEndpoint         discovery.Endpoint
		expectedLabels           map[string]string
		expectedEndpointPerSlice map[discovery.AddressType][]discovery.Endpoint
	}{
		"no-family-service": {
			service: noFamilyService,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
			},
		},
		"ipv4": {
			service: svcv4,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
				corev1.IsHeadlessService:          "",
			},
		},
		"ipv4-clusterip": {
			service: svcv4ClusterIP,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedAddressType: discovery.AddressTypeIPv4,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.4"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
			},
		},
		"ipv4-labels": {
			service: svcv4Labels,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedAddressType: discovery.AddressTypeIPv4,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.4"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
				"foo":                             "bar",
				corev1.IsHeadlessService:          "",
			},
		},
		"ipv4-bad-labels": {
			service: svcv4BadLabels,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedAddressType: discovery.AddressTypeIPv4,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.4"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
				corev1.IsHeadlessService:          "",
			},
		},

		"ipv6": {
			service: svcv6,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv6: {
					{
						Addresses: []string{"1234::5678:0000:0000:9abc:def0"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
				corev1.IsHeadlessService:          "",
			},
		},

		"ipv6-clusterip": {
			service: svcv6ClusterIP,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv6: {
					{
						Addresses: []string{"1234::5678:0000:0000:9abc:def0"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
			},
		},

		"dualstack-service": {
			service: dualStackSvc,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv6: {
					{
						Addresses: []string{"1234::5678:0000:0000:9abc:def0"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:          controllerName,
				mcsapiv1alpha1.LabelSourceCluster: defaultClusterName,
				discovery.LabelServiceName:        "foo",
			},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			client := newClientset()
			triggerTime := time.Now().UTC()
			endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
			r := newReconciler(client, defaultMaxEndpointsPerSlice)

			reconcileHelper(t, r, &testCase.service, clusterSvc1, []*discovery.EndpointSlice{}, triggerTime, endpointSliceTracker)

			if len(client.Actions()) != len(testCase.expectedEndpointPerSlice) {
				t.Errorf("Expected %v clientset action, got %d", len(testCase.expectedEndpointPerSlice), len(client.Actions()))
			}

			slices := fetchEndpointSlices(t, client, namespace)

			if len(slices) != len(testCase.expectedEndpointPerSlice) {
				t.Fatalf("Expected %v EndpointSlice, got %d", len(testCase.expectedEndpointPerSlice), len(slices))
			}

			for _, slice := range slices {
				if !strings.HasPrefix(slice.Name, testCase.service.Name) {
					t.Fatalf("Expected EndpointSlice name to start with %s, got %s", testCase.service.Name, slice.Name)
				}

				if !reflect.DeepEqual(testCase.expectedLabels, slice.Labels) {
					t.Errorf("Expected EndpointSlice to have labels: %v , got %v", testCase.expectedLabels, slice.Labels)
				}
				if slice.Labels[discovery.LabelServiceName] != testCase.service.Name {
					t.Fatalf("Expected EndpointSlice to have label set with %s value, got %s", testCase.service.Name, slice.Labels[discovery.LabelServiceName])
				}

				if slice.Annotations[corev1.EndpointsLastChangeTriggerTime] != triggerTime.Format(time.RFC3339Nano) {
					t.Fatalf("Expected EndpointSlice trigger time annotation to be %s, got %s", triggerTime.Format(time.RFC3339Nano), slice.Annotations[corev1.EndpointsLastChangeTriggerTime])
				}

				// validate that this slice has address type matching expected
				expectedEndPointList := testCase.expectedEndpointPerSlice[slice.AddressType]
				if expectedEndPointList == nil {
					t.Fatalf("address type %v is not expected", slice.AddressType)
				}

				if len(slice.Endpoints) != len(expectedEndPointList) {
					t.Fatalf("Expected %v Endpoint, got %d", len(expectedEndPointList), len(slice.Endpoints))
				}

				// test is limited to *ONE* endpoint
				endpoint := slice.Endpoints[0]
				if !reflect.DeepEqual(endpoint, expectedEndPointList[0]) {
					t.Fatalf("Expected endpoint: %+v, got: %+v", expectedEndPointList[0], endpoint)
				}

				expectTrackedGeneration(t, endpointSliceTracker, &slice, 1)

				expectSlicesChangedPerSync := 1
				if testCase.service.Spec.IPFamilies != nil && len(testCase.service.Spec.IPFamilies) > 0 {
					expectSlicesChangedPerSync = len(testCase.service.Spec.IPFamilies)
				}
				expectMetrics(t,
					r.metrics,
					expectedMetrics{
						desiredSlices:        1,
						actualSlices:         1,
						desiredEndpoints:     1,
						addedPerSync:         len(testCase.expectedEndpointPerSlice),
						removedPerSync:       0,
						numCreated:           len(testCase.expectedEndpointPerSlice),
						numUpdated:           0,
						numDeleted:           0,
						slicesChangedPerSync: expectSlicesChangedPerSync,
					})
			}
		})
	}
}

// given an existing placeholder endpoint slice and no backend in the cluster service, the existing
// slice should not change the placeholder
func TestReconcile1EndpointSlice(t *testing.T) {
	namespace := "test"
	svc, epMeta := newServiceAndEndpointMeta("foo", namespace)
	emptySlice := newEmptyEndpointSlice(1, namespace, epMeta, svc)
	emptySlice.ObjectMeta.Labels = map[string]string{"bar": "baz"}

	testCases := []struct {
		desc        string
		existing    *discovery.EndpointSlice
		wantUpdate  bool
		wantMetrics expectedMetrics
	}{
		{
			desc:        "No existing placeholder",
			wantUpdate:  true,
			wantMetrics: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 1, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 1},
		},
		{
			desc:        "Existing placeholder that's the same",
			existing:    newEndpointSlice(&svc, &endpointMeta{ports: []discovery.EndpointPort{}, addressType: discovery.AddressTypeIPv4}, defaultClusterName, controllerName),
			wantMetrics: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 0, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 0},
		},
		{
			desc:        "Existing placeholder that's different",
			existing:    emptySlice,
			wantUpdate:  true,
			wantMetrics: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 0, numUpdated: 1, numDeleted: 0, slicesChangedPerSync: 1},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			client := newClientset()

			existingSlices := []*discovery.EndpointSlice{}
			if tc.existing != nil {
				existingSlices = append(existingSlices, tc.existing)
				_, createErr := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), tc.existing, metav1.CreateOptions{})
				assert.Nil(t, createErr, "Expected no error creating endpoint slice")
			}

			numActionsBefore := len(client.Actions())
			r := newReconciler(client, defaultMaxEndpointsPerSlice)
			endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
			reconcileHelper(t, r, &svc, newEmptyClusterSvc(), existingSlices, time.Now(), endpointSliceTracker)

			var numUpdates int
			if tc.wantUpdate {
				numUpdates = 1
			}
			wantActions := numActionsBefore + numUpdates
			assert.Len(t, client.Actions(), wantActions, "Expected %d additional clientset actions", numUpdates)

			slices := fetchEndpointSlices(t, client, namespace)
			assert.Len(t, slices, 1, "Expected 1 endpoint slices")

			if !tc.wantUpdate {
				assert.Regexp(t, "^"+svc.Name, slices[0].Name)
				assert.Equal(t, svc.Name, slices[0].Labels[discovery.LabelServiceName])
				assert.EqualValues(t, []discovery.EndpointPort{}, slices[0].Ports)
				assert.EqualValues(t, []discovery.Endpoint{}, slices[0].Endpoints)
				if tc.existing == nil {
					expectTrackedGeneration(t, endpointSliceTracker, &slices[0], 1)
				}
			}
			expectMetrics(t, r.metrics, tc.wantMetrics)
		})
	}
}

// a simple use case with a cluster service with 250 backends and no existing slices
// reconcile should create 3 slices, completely filling 2 of them
func TestReconcileManyBackeds(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 250; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)

	// This is an ideal scenario where only 3 actions are required, and they're all creates
	assert.Len(t, client.Actions(), 3, "Expected 3 additional clientset actions")
	expectActions(t, client.Actions(), 3, "create", "endpointslices")

	// Two endpoint slices should be completely full, the remainder should be in another one
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{100, 100, 50})
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 3, actualSlices: 3, desiredEndpoints: 250, addedPerSync: 250, removedPerSync: 0, numCreated: 3, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 3})
}

// now with preexisting slices, we have 250 backends matching a service
// the first endpoint slice contains 62 endpoints, all desired
// the second endpoint slice contains 61 endpoints, all desired
// that leaves 127 to add
// to minimize writes, our strategy is to create new slices for multiples of 100
// that leaves 27 to drop in an existing slice
// dropping them in the first slice will result in the slice being closest to full
// this approach requires 1 update + 1 create instead of 2 updates + 1 create
func TestReconcileEndpointSlicesSomePreexisting(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 250; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	// have approximately 1/4 in first slice
	endpointSlice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 1; i < len(clusterSvc.Backends)-4; i += 4 {
		endpointSlice1.Endpoints = append(endpointSlice1.Endpoints, *newEndpoint(newAddress(i)))
	}

	// have approximately 1/4 in second slice
	endpointSlice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 3; i < len(clusterSvc.Backends)-4; i += 4 {
		endpointSlice2.Endpoints = append(endpointSlice2.Endpoints, *newEndpoint(newAddress(i)))
	}

	existingSlices := []*discovery.EndpointSlice{endpointSlice1, endpointSlice2}
	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	numActionsBefore := len(client.Actions())
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	reconcileHelper(t, r, &svc, clusterSvc, existingSlices, time.Now(), endpointSliceTracker)

	actions := client.Actions()
	assert.Equal(t, numActionsBefore+2, len(actions), "Expected 2 additional client actions as part of reconcile")
	assert.True(t, actions[numActionsBefore].Matches("create", "endpointslices"), "First action should be create endpoint slice")
	assert.True(t, actions[numActionsBefore+1].Matches("update", "endpointslices"), "Second action should be update endpoint slice")

	// 1 new slice (0->100) + 1 updated slice (62->89)
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{89, 61, 100})
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 3, actualSlices: 3, desiredEndpoints: 250, addedPerSync: 127, removedPerSync: 0, numCreated: 1, numUpdated: 1, numDeleted: 0, slicesChangedPerSync: 2})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// now with preexisting slices, we have 300 backends matching a service
// this scenario will show some less ideal allocation
// the first endpoint slice contains 74 endpoints, all desired
// the second endpoint slice contains 74 endpoints, all desired
// that leaves 152 to add
// to minimize writes, our strategy is to create new slices for multiples of 100
// that leaves 52 to drop in an existing slice
// that capacity could fit if split in the 2 existing slices
// to minimize writes though, reconcile create a new slice with those 52 endpoints
// this approach requires 2 creates instead of 2 updates + 1 create
func TestReconcileEndpointSlicesSomePreexistingWorseAllocation(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	// start with 300 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 300; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	// have approximately 1/4 in first slice
	endpointSlice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 1; i < len(clusterSvc.Backends)-4; i += 4 {
		endpointSlice1.Endpoints = append(endpointSlice1.Endpoints, *newEndpoint(newAddress(i)))
	}

	// have approximately 1/4 in second slice
	endpointSlice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 3; i < len(clusterSvc.Backends)-4; i += 4 {
		endpointSlice2.Endpoints = append(endpointSlice2.Endpoints, *newEndpoint(newAddress(i)))
	}

	existingSlices := []*discovery.EndpointSlice{endpointSlice1, endpointSlice2}
	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	numActionsBefore := len(client.Actions())
	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, existingSlices, time.Now(), endpointSliceTracker)

	actions := client.Actions()
	assert.Equal(t, numActionsBefore+2, len(actions), "Expected 2 additional client actions as part of reconcile")
	expectActions(t, client.Actions(), 2, "create", "endpointslices")

	// 2 new slices (100, 52) in addition to existing slices (74, 74)
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{74, 74, 100, 52})
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 3, actualSlices: 4, desiredEndpoints: 300, addedPerSync: 152, removedPerSync: 0, numCreated: 2, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 2})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In some cases, such as a service port change, all slices for that service will require a change
// This test ensures that we are updating those slices and not calling create + delete for each
func TestReconcileEndpointSlicesUpdating(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 250; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)
	numActionsExpected := 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")

	slices := fetchEndpointSlices(t, client, namespace)
	numActionsExpected++
	expectUnorderedSlicesWithLengths(t, slices, []int{100, 100, 50})

	svc.Spec.Ports[0].TargetPort.IntVal = 81
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{&slices[0], &slices[1], &slices[2]}, time.Now(), endpointSliceTracker)

	numActionsExpected += 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")
	expectActions(t, client.Actions(), 3, "update", "endpointslices")

	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{100, 100, 50})
}

// In some cases, such as service labels updates, all slices for that service will require a change
// This test ensures that we are updating those slices and not calling create + delete for each
func TestReconcileEndpointSlicesServicesLabelsUpdating(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 250; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)
	numActionsExpected := 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")

	slices := fetchEndpointSlices(t, client, namespace)
	numActionsExpected++
	expectUnorderedSlicesWithLengths(t, slices, []int{100, 100, 50})

	// update service with new labels
	svc.Labels = map[string]string{"foo": "bar"}
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{&slices[0], &slices[1], &slices[2]}, time.Now(), endpointSliceTracker)

	numActionsExpected += 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")
	expectActions(t, client.Actions(), 3, "update", "endpointslices")

	newSlices := fetchEndpointSlices(t, client, namespace)
	expectUnorderedSlicesWithLengths(t, newSlices, []int{100, 100, 50})
	// check that the labels were updated
	for _, slice := range newSlices {
		w, ok := slice.Labels["foo"]
		if !ok {
			t.Errorf("Expected label \"foo\" from parent service not found")
		} else if "bar" != w {
			t.Errorf("Expected EndpointSlice to have parent service labels: have %s value, expected bar", w)
		}
	}
}

// In some cases, such as service labels updates, all slices for that service will require a change
// However, this should not happen for reserved labels
func TestReconcileEndpointSlicesServicesReservedLabels(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 250; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)
	numActionsExpected := 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")
	slices := fetchEndpointSlices(t, client, namespace)
	numActionsExpected++
	expectUnorderedSlicesWithLengths(t, slices, []int{100, 100, 50})

	// update service with new labels
	svc.Labels = map[string]string{
		discovery.LabelServiceName: "bad", discovery.LabelManagedBy: "actor",
		corev1.IsHeadlessService: "invalid", mcsapiv1alpha1.LabelSourceCluster: "notacluster",
	}
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{&slices[0], &slices[1], &slices[2]}, time.Now(), endpointSliceTracker)
	assert.Len(t, client.Actions(), numActionsExpected, "Expected no additional clientset actions")

	newSlices := fetchEndpointSlices(t, client, namespace)
	expectUnorderedSlicesWithLengths(t, newSlices, []int{100, 100, 50})
}

// In this test, we start with 10 slices that only have 30 endpoints each
// An initial reconcile makes no changes (as desired to limit writes)
// When we change a service port, all slices will need to be updated in some way
// reconcile repacks the endpoints into 3 slices, and deletes the extras
func TestReconcileEndpointSlicesRecycling(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	// start with 300 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 300; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	// generate 10 existing slices with 30 endpoints each
	existingSlices := []*discovery.EndpointSlice{}
	i := 0
	for address := range clusterSvc.Backends {
		sliceNum := i / 30
		if i%30 == 0 {
			existingSlices = append(existingSlices, newEmptyEndpointSlice(sliceNum, namespace, endpointMeta, svc))
		}
		existingSlices[sliceNum].Endpoints = append(existingSlices[sliceNum].Endpoints, *newEndpoint(address))
		i++
	}

	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	numActionsBefore := len(client.Actions())
	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, existingSlices, time.Now(), endpointSliceTracker)
	// initial reconcile should be a no op, all backends are accounted for in slices, no repacking should be done
	assert.Equal(t, numActionsBefore+0, len(client.Actions()), "Expected 0 additional client actions as part of reconcile")

	// changing a service port should require all slices to be updated, time for a repack
	svc.Spec.Ports[0].TargetPort.IntVal = 81
	reconcileHelper(t, r, &svc, clusterSvc, existingSlices, time.Now(), endpointSliceTracker)

	// this should reflect 3 updates + 7 deletes
	assert.Equal(t, numActionsBefore+10, len(client.Actions()), "Expected 10 additional client actions as part of reconcile")

	// thanks to recycling, we get a free repack of endpoints, resulting in 3 full slices instead of 10 mostly empty slices
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{100, 100, 100})
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 3, actualSlices: 3, desiredEndpoints: 300, addedPerSync: 300, removedPerSync: 0, numCreated: 0, numUpdated: 3, numDeleted: 7, slicesChangedPerSync: 10})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In this test, we want to verify that endpoints are added to a slice that will
// be closest to full after the operation, even when slices are already marked
// for update.
func TestReconcileEndpointSlicesUpdatePacking(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	existingSlices := []*discovery.EndpointSlice{}
	clusterSvc := newEmptyClusterSvc()

	slice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 0; i < 80; i++ {
		address := newAddress(i)
		slice1.Endpoints = append(slice1.Endpoints, *newEndpoint(address))
		clusterSvc.Backends[address] = map[string]*loadbalancer.L4Addr{}
	}
	existingSlices = append(existingSlices, slice1)

	slice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 100; i < 120; i++ {
		address := newAddress(i)
		slice2.Endpoints = append(slice2.Endpoints, *newEndpoint(address))
		clusterSvc.Backends[address] = map[string]*loadbalancer.L4Addr{}
	}
	existingSlices = append(existingSlices, slice2)

	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	// ensure that endpoints in each slice will be marked for update.
	svc.Labels = map[string]string{"update": "true"}

	// add a few additional endpoints - no more than could fit in either slice.
	for i := 200; i < 215; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, existingSlices, time.Now(), endpointSliceTracker)

	// ensure that both endpoint slices have been updated
	expectActions(t, client.Actions(), 2, "update", "endpointslices")
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 2, actualSlices: 2, desiredEndpoints: 115, addedPerSync: 15, removedPerSync: 0, numCreated: 0, numUpdated: 2, numDeleted: 0, slicesChangedPerSync: 2})

	// additional backends should get added to fuller slice
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{95, 20})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In this test, we want to verify that old EndpointSlices with a deprecated IP
// address type will be replaced with a newer IPv4 type.
func TestReconcileEndpointSlicesReplaceDeprecated(t *testing.T) {
	client := newClientset()
	namespace := "test"

	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)
	// "IP" is a deprecated address type, ensuring that it is handled properly.
	endpointMeta.addressType = discovery.AddressType("IP")

	existingSlices := []*discovery.EndpointSlice{}
	clusterSvc := newEmptyClusterSvc()

	slice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 0; i < 80; i++ {
		address := newAddress(i)
		slice1.Endpoints = append(slice1.Endpoints, *newEndpoint(address))
		clusterSvc.Backends[address] = map[string]*loadbalancer.L4Addr{}
	}
	existingSlices = append(existingSlices, slice1)

	slice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 100; i < 150; i++ {
		address := newAddress(i)
		slice2.Endpoints = append(slice2.Endpoints, *newEndpoint(address))
		clusterSvc.Backends[address] = map[string]*loadbalancer.L4Addr{}
	}
	existingSlices = append(existingSlices, slice2)

	createEndpointSlices(t, client, namespace, existingSlices)

	cmc := newCacheMutationCheck(existingSlices)
	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, existingSlices, time.Now(), endpointSliceTracker)

	// ensure that both original endpoint slices have been deleted
	expectActions(t, client.Actions(), 2, "delete", "endpointslices")

	endpointSlices := fetchEndpointSlices(t, client, namespace)

	// since this involved replacing both EndpointSlices, the result should be
	// perfectly packed.
	expectUnorderedSlicesWithLengths(t, endpointSlices, []int{100, 30})

	for _, endpointSlice := range endpointSlices {
		if endpointSlice.AddressType != discovery.AddressTypeIPv4 {
			t.Errorf("Expected address type to be IPv4, got %s", endpointSlice.AddressType)
		}
	}

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In this test, we want to verify that a Service recreation will result in new
// EndpointSlices being created.
func TestReconcileEndpointSlicesRecreation(t *testing.T) {
	testCases := []struct {
		name           string
		ownedByService bool
		expectChanges  bool
	}{
		{
			name:           "slice owned by Service",
			ownedByService: true,
			expectChanges:  false,
		}, {
			name:           "slice owned by other Service UID",
			ownedByService: false,
			expectChanges:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClientset()
			namespace := "test"

			svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)
			slice := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)

			address := newAddress(1)
			clusterSvc := newEmptyClusterSvc()
			clusterSvc.Backends[address] = map[string]*loadbalancer.L4Addr{}
			slice.Endpoints = append(slice.Endpoints, *newEndpoint(address))

			if !tc.ownedByService {
				slice.OwnerReferences[0].UID = "different"
			}
			existingSlices := []*discovery.EndpointSlice{slice}
			createEndpointSlices(t, client, namespace, existingSlices)

			cmc := newCacheMutationCheck(existingSlices)

			numActionsBefore := len(client.Actions())
			endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
			r := newReconciler(client, defaultMaxEndpointsPerSlice)
			reconcileHelper(t, r, &svc, clusterSvc, existingSlices, time.Now(), endpointSliceTracker)

			if tc.expectChanges {
				if len(client.Actions()) != numActionsBefore+2 {
					t.Fatalf("Expected 2 additional actions, got %d", len(client.Actions())-numActionsBefore)
				}

				expectAction(t, client.Actions(), numActionsBefore, "create", "endpointslices")
				expectAction(t, client.Actions(), numActionsBefore+1, "delete", "endpointslices")

				fetchedSlices := fetchEndpointSlices(t, client, namespace)

				if len(fetchedSlices) != 1 {
					t.Fatalf("Expected 1 EndpointSlice to exist, got %d", len(fetchedSlices))
				}
			} else {
				if len(client.Actions()) != numActionsBefore {
					t.Errorf("Expected no additional actions, got %d", len(client.Actions())-numActionsBefore)
				}
			}
			// ensure cache mutation has not occurred
			cmc.Check(t)
		})
	}
}

// Named ports can map to different port numbers on different backends.
// This test ensures that EndpointSlices are grouped correctly in that case.
func TestReconcileEndpointSlicesNamedPorts(t *testing.T) {
	client := newClientset()
	namespace := "test"

	portNameIntStr := intstr.IntOrString{
		Type:   intstr.String,
		StrVal: "http",
	}

	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "named-port-example", Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				TargetPort: portNameIntStr,
				Protocol:   corev1.ProtocolTCP,
			}},
			Selector:   map[string]string{"foo": "bar"},
			IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol},
		},
	}

	// start with 300 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 300; i++ {
		portOffset := i % 5
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{
			portNameIntStr.StrVal: {
				Protocol: "TCP",
				Port:     uint16(8080 + portOffset),
			},
		}
	}

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)

	// reconcile should create 5 endpoint slices
	assert.Equal(t, 5, len(client.Actions()), "Expected 5 client actions as part of reconcile")
	expectActions(t, client.Actions(), 5, "create", "endpointslices")
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 5, actualSlices: 5, desiredEndpoints: 300, addedPerSync: 300, removedPerSync: 0, numCreated: 5, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 5})

	fetchedSlices := fetchEndpointSlices(t, client, namespace)

	// each slice should have 60 endpoints to match 5 unique variations of named port mapping
	expectUnorderedSlicesWithLengths(t, fetchedSlices, []int{60, 60, 60, 60, 60})

	// generate data structures for expected slice ports and address types
	protoTCP := corev1.ProtocolTCP
	expectedSlices := []discovery.EndpointSlice{}
	for i := range fetchedSlices {
		expectedSlices = append(expectedSlices, discovery.EndpointSlice{
			Ports: []discovery.EndpointPort{{
				Name:     pointer.String(""),
				Protocol: &protoTCP,
				Port:     pointer.Int32(int32(8080 + i)),
			}},
			AddressType: discovery.AddressTypeIPv4,
		})
	}

	// slices fetched should match expected address type and ports
	expectUnorderedSlicesWithTopLevelAttrs(t, fetchedSlices, expectedSlices)
}

// This test ensures that maxEndpointsPerSlice configuration results in
// appropriate endpoints distribution among slices
func TestReconcileMaxEndpointsPerSlice(t *testing.T) {
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 250; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	testCases := []struct {
		maxEndpointsPerSlice int
		expectedSliceLengths []int
		expectedMetricValues expectedMetrics
	}{
		{
			maxEndpointsPerSlice: 50,
			expectedSliceLengths: []int{50, 50, 50, 50, 50},
			expectedMetricValues: expectedMetrics{desiredSlices: 5, actualSlices: 5, desiredEndpoints: 250, addedPerSync: 250, numCreated: 5, slicesChangedPerSync: 5},
		}, {
			maxEndpointsPerSlice: 80,
			expectedSliceLengths: []int{80, 80, 80, 10},
			expectedMetricValues: expectedMetrics{desiredSlices: 4, actualSlices: 4, desiredEndpoints: 250, addedPerSync: 250, numCreated: 4, slicesChangedPerSync: 4},
		}, {
			maxEndpointsPerSlice: 150,
			expectedSliceLengths: []int{150, 100},
			expectedMetricValues: expectedMetrics{desiredSlices: 2, actualSlices: 2, desiredEndpoints: 250, addedPerSync: 250, numCreated: 2, slicesChangedPerSync: 2},
		}, {
			maxEndpointsPerSlice: 250,
			expectedSliceLengths: []int{250},
			expectedMetricValues: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 250, addedPerSync: 250, numCreated: 1, slicesChangedPerSync: 1},
		}, {
			maxEndpointsPerSlice: 500,
			expectedSliceLengths: []int{250},
			expectedMetricValues: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 250, addedPerSync: 250, numCreated: 1, slicesChangedPerSync: 1},
		},
	}

	for _, testCase := range testCases {
		t.Run(fmt.Sprintf("maxEndpointsPerSlice: %d", testCase.maxEndpointsPerSlice), func(t *testing.T) {
			client := newClientset()
			endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
			r := newReconciler(client, testCase.maxEndpointsPerSlice)
			reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)
			expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), testCase.expectedSliceLengths)
			expectMetrics(t, r.metrics, testCase.expectedMetricValues)
		})
	}
}

func TestReconcileEndpointSlicesMetrics(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 20 backends
	clusterSvc := newEmptyClusterSvc()
	for i := 0; i < 20; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)

	actions := client.Actions()
	assert.Equal(t, 1, len(actions), "Expected 1 additional client actions as part of reconcile")
	assert.True(t, actions[0].Matches("create", "endpointslices"), "First action should be create endpoint slice")

	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 20, addedPerSync: 20, removedPerSync: 0, numCreated: 1, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 1})

	fetchedSlices := fetchEndpointSlices(t, client, namespace)

	// reset to 10 backends
	clusterSvc.Backends = map[string]serviceStore.PortConfiguration{}
	for i := 0; i < 10; i++ {
		clusterSvc.Backends[newAddress(i)] = map[string]*loadbalancer.L4Addr{}
	}

	reconcileHelper(t, r, &svc, clusterSvc, []*discovery.EndpointSlice{&fetchedSlices[0]}, time.Now(), endpointSliceTracker)
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 10, addedPerSync: 20, removedPerSync: 10, numCreated: 1, numUpdated: 1, numDeleted: 0, slicesChangedPerSync: 2})
}

// When a Service has a non-nil deletionTimestamp we want to avoid creating any
// new EndpointSlices but continue to allow updates and deletes through. This
// test uses 3 EndpointSlices, 1 "to-create", 1 "to-update", and 1 "to-delete".
// Each test case exercises different combinations of calls to finalize with
// those resources.
func TestReconcilerFinalizeSvcDeletionTimestamp(t *testing.T) {
	now := metav1.Now()

	testCases := []struct {
		name               string
		deletionTimestamp  *metav1.Time
		attemptCreate      bool
		attemptUpdate      bool
		attemptDelete      bool
		expectCreatedSlice bool
		expectUpdatedSlice bool
		expectDeletedSlice bool
	}{{
		name:               "Attempt create and update, nil deletion timestamp",
		deletionTimestamp:  nil,
		attemptCreate:      true,
		attemptUpdate:      true,
		expectCreatedSlice: true,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		name:               "Attempt create and update, deletion timestamp set",
		deletionTimestamp:  &now,
		attemptCreate:      true,
		attemptUpdate:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		// Slice scheduled for creation is transitioned to update of Slice
		// scheduled for deletion.
		name:               "Attempt create, update, and delete, nil deletion timestamp, recycling in action",
		deletionTimestamp:  nil,
		attemptCreate:      true,
		attemptUpdate:      true,
		attemptDelete:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		// Slice scheduled for creation is transitioned to update of Slice
		// scheduled for deletion.
		name:               "Attempt create, update, and delete, deletion timestamp set, recycling in action",
		deletionTimestamp:  &now,
		attemptCreate:      true,
		attemptUpdate:      true,
		attemptDelete:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		// Update and delete continue to work when deletionTimestamp is set.
		name:               "Attempt update delete, deletion timestamp set",
		deletionTimestamp:  &now,
		attemptCreate:      false,
		attemptUpdate:      true,
		attemptDelete:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: false,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClientset()
			endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()
			r := newReconciler(client, defaultMaxEndpointsPerSlice)

			namespace := "test"
			svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)
			svc.DeletionTimestamp = tc.deletionTimestamp
			gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
			ownerRef := metav1.NewControllerRef(&svc, gvk)

			esToCreate := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "to-create",
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
				},
				AddressType: endpointMeta.addressType,
				Ports:       endpointMeta.ports,
			}

			// Add EndpointSlice that can be updated.
			esToUpdate, err := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "to-update",
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
				},
				AddressType: endpointMeta.addressType,
				Ports:       endpointMeta.ports,
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Expected no error creating EndpointSlice during test setup, got %v", err)
			}
			// Add an endpoint so we can see if this has actually been updated by
			// finalize func.
			esToUpdate.Endpoints = []discovery.Endpoint{{Addresses: []string{"10.2.3.4"}}}

			// Add EndpointSlice that can be deleted.
			esToDelete, err := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "to-delete",
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
				},
				AddressType: endpointMeta.addressType,
				Ports:       endpointMeta.ports,
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Expected no error creating EndpointSlice during test setup, got %v", err)
			}

			slicesToCreate := []*discovery.EndpointSlice{}
			if tc.attemptCreate {
				slicesToCreate = append(slicesToCreate, esToCreate.DeepCopy())
			}
			slicesToUpdate := []*discovery.EndpointSlice{}
			if tc.attemptUpdate {
				slicesToUpdate = append(slicesToUpdate, esToUpdate.DeepCopy())
			}
			slicesToDelete := []*discovery.EndpointSlice{}
			if tc.attemptDelete {
				slicesToDelete = append(slicesToDelete, esToDelete.DeepCopy())
			}

			err = r.finalize(&svc, slicesToCreate, slicesToUpdate, slicesToDelete, time.Now(), endpointSliceTracker)
			if err != nil {
				t.Errorf("Error calling r.finalize(): %v", err)
			}

			fetchedSlices := fetchEndpointSlices(t, client, namespace)

			createdSliceFound := false
			updatedSliceFound := false
			deletedSliceFound := false
			for _, epSlice := range fetchedSlices {
				if epSlice.Name == esToCreate.Name {
					createdSliceFound = true
				}
				if epSlice.Name == esToUpdate.Name {
					updatedSliceFound = true
					if tc.attemptUpdate && len(epSlice.Endpoints) != len(esToUpdate.Endpoints) {
						t.Errorf("Expected EndpointSlice to be updated with %d endpoints, got %d endpoints", len(esToUpdate.Endpoints), len(epSlice.Endpoints))
					}
				}
				if epSlice.Name == esToDelete.Name {
					deletedSliceFound = true
				}
			}

			if createdSliceFound != tc.expectCreatedSlice {
				t.Errorf("Expected created EndpointSlice existence to be %t, got %t", tc.expectCreatedSlice, createdSliceFound)
			}

			if updatedSliceFound != tc.expectUpdatedSlice {
				t.Errorf("Expected updated EndpointSlice existence to be %t, got %t", tc.expectUpdatedSlice, updatedSliceFound)
			}

			if deletedSliceFound != tc.expectDeletedSlice {
				t.Errorf("Expected deleted EndpointSlice existence to be %t, got %t", tc.expectDeletedSlice, deletedSliceFound)
			}
		})
	}
}

// Test Helpers

func newEmptyClusterSvc() *serviceStore.ClusterService {
	return &serviceStore.ClusterService{
		Cluster:  defaultClusterName,
		Backends: map[string]serviceStore.PortConfiguration{},
	}
}

func newClientset() *fake.Clientset {
	client := fake.NewSimpleClientset()

	client.PrependReactor("create", "endpointslices", k8stesting.ReactionFunc(func(action k8stesting.Action) (bool, runtime.Object, error) {
		endpointSlice := action.(k8stesting.CreateAction).GetObject().(*discovery.EndpointSlice)

		if endpointSlice.ObjectMeta.GenerateName != "" {
			endpointSlice.ObjectMeta.Name = fmt.Sprintf("%s-%s", endpointSlice.ObjectMeta.GenerateName, rand.String(8))
			endpointSlice.ObjectMeta.GenerateName = ""
		}
		endpointSlice.ObjectMeta.Generation = 1

		return false, endpointSlice, nil
	}))
	client.PrependReactor("update", "endpointslices", k8stesting.ReactionFunc(func(action k8stesting.Action) (bool, runtime.Object, error) {
		endpointSlice := action.(k8stesting.CreateAction).GetObject().(*discovery.EndpointSlice)
		endpointSlice.ObjectMeta.Generation++
		return false, endpointSlice, nil
	}))

	return client
}

func newServiceAndEndpointMeta(name, namespace string) (v1.Service, endpointMeta) {
	portNum := int32(80)
	portNameIntStr := intstr.IntOrString{
		Type:   intstr.Int,
		IntVal: portNum,
	}

	svc := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(namespace + "-" + name),
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				TargetPort: portNameIntStr,
				Protocol:   v1.ProtocolTCP,
				Name:       name,
			}},
			Selector:   map[string]string{"foo": "bar"},
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
		},
	}

	addressType := discovery.AddressTypeIPv4
	protocol := v1.ProtocolTCP
	endpointMeta := endpointMeta{
		addressType: addressType,
		ports:       []discovery.EndpointPort{{Name: &name, Port: &portNum, Protocol: &protocol}},
	}

	return svc, endpointMeta
}

func newReconciler(client *fake.Clientset, maxEndpointsPerSlice int) *EndpointSliceReconciler {
	metrics := NewMetrics()
	return newEndpointSliceReconciler(
		client,
		maxEndpointsPerSlice,
		&metrics,
		controllerName,
	)
}

// ensures endpoint slices exist with the desired set of lengths
func expectUnorderedSlicesWithLengths(t *testing.T, endpointSlices []discovery.EndpointSlice, expectedLengths []int) {
	assert.Len(t, endpointSlices, len(expectedLengths), "Expected %d endpoint slices", len(expectedLengths))

	lengthsWithNoMatch := []int{}
	desiredLengths := expectedLengths
	actualLengths := []int{}
	for _, endpointSlice := range endpointSlices {
		actualLen := len(endpointSlice.Endpoints)
		actualLengths = append(actualLengths, actualLen)
		matchFound := false
		for i := 0; i < len(desiredLengths); i++ {
			if desiredLengths[i] == actualLen {
				matchFound = true
				desiredLengths = append(desiredLengths[:i], desiredLengths[i+1:]...)
				break
			}
		}

		if !matchFound {
			lengthsWithNoMatch = append(lengthsWithNoMatch, actualLen)
		}
	}

	if len(lengthsWithNoMatch) > 0 || len(desiredLengths) > 0 {
		t.Errorf("Actual slice lengths (%v) don't match expected (%v)", actualLengths, expectedLengths)
	}
}

// ensures endpoint slices exist with the desired set of ports and address types
func expectUnorderedSlicesWithTopLevelAttrs(t *testing.T, endpointSlices []discovery.EndpointSlice, expectedSlices []discovery.EndpointSlice) {
	t.Helper()
	assert.Len(t, endpointSlices, len(expectedSlices), "Expected %d endpoint slices", len(expectedSlices))

	slicesWithNoMatch := []discovery.EndpointSlice{}
	for _, endpointSlice := range endpointSlices {
		matchFound := false
		for i := 0; i < len(expectedSlices); i++ {
			if portsAndAddressTypeEqual(expectedSlices[i], endpointSlice) {
				matchFound = true
				expectedSlices = append(expectedSlices[:i], expectedSlices[i+1:]...)
				break
			}
		}

		if !matchFound {
			slicesWithNoMatch = append(slicesWithNoMatch, endpointSlice)
		}
	}

	assert.Len(t, slicesWithNoMatch, 0, "EndpointSlice(s) found without matching attributes")
	assert.Len(t, expectedSlices, 0, "Expected slices(s) not found in EndpointSlices")
}

func expectActions(t *testing.T, actions []k8stesting.Action, num int, verb, resource string) {
	t.Helper()
	// if actions are less the below logic will panic
	if num > len(actions) {
		t.Fatalf("len of actions %v is unexpected. Expected to be at least %v", len(actions), num+1)
	}

	for i := 0; i < num; i++ {
		relativePos := len(actions) - i - 1
		assert.Equal(t, verb, actions[relativePos].GetVerb(), "Expected action -%d verb to be %s", i, verb)
		assert.Equal(t, resource, actions[relativePos].GetResource().Resource, "Expected action -%d resource to be %s", i, resource)
	}
}

func expectTrackedGeneration(t *testing.T, tracker *endpointsliceutil.EndpointSliceTracker, slice *discovery.EndpointSlice, expectedGeneration int64) {
	gfs, ok := tracker.GenerationsForSliceUnsafe(slice)
	if !ok {
		t.Fatalf("Expected Service to be tracked for EndpointSlices %s", slice.Name)
	}
	generation, ok := gfs[slice.UID]
	if !ok {
		t.Fatalf("Expected EndpointSlice %s to be tracked", slice.Name)
	}
	if generation != expectedGeneration {
		t.Errorf("Expected Generation of %s to be %d, got %d", slice.Name, expectedGeneration, generation)
	}
}

func portsAndAddressTypeEqual(slice1, slice2 discovery.EndpointSlice) bool {
	return apiequality.Semantic.DeepEqual(slice1.Ports, slice2.Ports) && apiequality.Semantic.DeepEqual(slice1.AddressType, slice2.AddressType)
}

func createEndpointSlices(t *testing.T, client *fake.Clientset, namespace string, endpointSlices []*discovery.EndpointSlice) {
	t.Helper()
	for _, endpointSlice := range endpointSlices {
		_, err := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), endpointSlice, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Expected no error creating Endpoint Slice, got: %v", err)
		}
	}
}

func fetchEndpointSlices(t *testing.T, client *fake.Clientset, namespace string) []discovery.EndpointSlice {
	t.Helper()
	fetchedSlices, err := client.DiscoveryV1().EndpointSlices(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Expected no error fetching Endpoint Slices, got: %v", err)
		return []discovery.EndpointSlice{}
	}
	return fetchedSlices.Items
}

func reconcileHelper(t *testing.T, r *EndpointSliceReconciler, service *corev1.Service, clusterSvc *serviceStore.ClusterService, existingSlices []*discovery.EndpointSlice, triggerTime time.Time, endpointSliceTracker *endpointsliceutil.EndpointSliceTracker) {
	t.Helper()
	err := r.Reconcile(service, clusterSvc, existingSlices, triggerTime, endpointSliceTracker)
	if err != nil {
		t.Fatalf("Expected no error reconciling Endpoint Slices, got: %v", err)
	}
}

func newEmptyEndpointSlice(n int, namespace string, endpointMeta endpointMeta, svc v1.Service) *discovery.EndpointSlice {
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	ownerRef := metav1.NewControllerRef(&svc, gvk)

	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-%d", svc.Name, n),
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{*ownerRef},
		},
		Ports:       endpointMeta.ports,
		AddressType: endpointMeta.addressType,
		Endpoints:   []discovery.Endpoint{},
	}
}

func newAddress(n int) string {
	return fmt.Sprintf("1.2.3.%d", 4+n)
}

// Metrics helpers

type expectedMetrics struct {
	desiredSlices        int
	actualSlices         int
	desiredEndpoints     int
	addedPerSync         int
	removedPerSync       int
	numCreated           int
	numUpdated           int
	numDeleted           int
	slicesChangedPerSync int
	syncSuccesses        int
	syncErrors           int
}

func expectMetrics(t *testing.T, metrics *Metrics, em expectedMetrics) {
	t.Helper()

	// Global Kubernetes fields tests
	k8sActualDesiredSlices, err := testutil.GetGaugeMetricValue(endpointslicemetrics.DesiredEndpointSlices.WithLabelValues())
	handleErr(t, err, "k8sDesiredEndpointSlices")
	if k8sActualDesiredSlices != float64(em.desiredSlices) {
		t.Errorf("Expected Kubernetes desiredEndpointSlices to be %d, got %v", em.desiredSlices, k8sActualDesiredSlices)
	}

	k8sActualNumSlices, err := testutil.GetGaugeMetricValue(endpointslicemetrics.NumEndpointSlices.WithLabelValues())
	handleErr(t, err, "k8sNumEndpointSlices")
	if k8sActualNumSlices != float64(em.actualSlices) {
		t.Errorf("Expected Kubernetes numEndpointSlices to be %d, got %v", em.actualSlices, k8sActualNumSlices)
	}

	k8sActualEndpointsDesired, err := testutil.GetGaugeMetricValue(endpointslicemetrics.EndpointsDesired.WithLabelValues())
	handleErr(t, err, "k8sDesiredEndpoints")
	if k8sActualEndpointsDesired != float64(em.desiredEndpoints) {
		t.Errorf("Expected Kubernetes desiredEndpoints to be %d, got %v", em.desiredEndpoints, k8sActualEndpointsDesired)
	}

	// Cilium tests
	actualDesiredSlices, err := testutil.GetGaugeMetricValue(metrics.DesiredEndpointSlices.WithLabelValues())
	handleErr(t, err, "desiredEndpointSlices")
	if actualDesiredSlices != float64(em.desiredSlices) {
		t.Errorf("Expected desiredEndpointSlices to be %d, got %v", em.desiredSlices, actualDesiredSlices)
	}

	actualNumSlices, err := testutil.GetGaugeMetricValue(metrics.NumEndpointSlices.WithLabelValues())
	handleErr(t, err, "numEndpointSlices")
	if actualNumSlices != float64(em.actualSlices) {
		t.Errorf("Expected numEndpointSlices to be %d, got %v", em.actualSlices, actualNumSlices)
	}

	actualEndpointsDesired, err := testutil.GetGaugeMetricValue(metrics.EndpointsDesired.WithLabelValues())
	handleErr(t, err, "desiredEndpoints")
	if actualEndpointsDesired != float64(em.desiredEndpoints) {
		t.Errorf("Expected desiredEndpoints to be %d, got %v", em.desiredEndpoints, actualEndpointsDesired)
	}

	actualAddedPerSync, err := testutil.GetHistogramMetricValue(toPrometheusHistogramVec(metrics.EndpointsAddedPerSync).WithLabelValues())
	handleErr(t, err, "endpointsAddedPerSync")
	if actualAddedPerSync != float64(em.addedPerSync) {
		t.Errorf("Expected endpointsAddedPerSync to be %d, got %v", em.addedPerSync, actualAddedPerSync)
	}

	actualRemovedPerSync, err := testutil.GetHistogramMetricValue(toPrometheusHistogramVec(metrics.EndpointsRemovedPerSync).WithLabelValues())
	handleErr(t, err, "endpointsRemovedPerSync")
	if actualRemovedPerSync != float64(em.removedPerSync) {
		t.Errorf("Expected endpointsRemovedPerSync to be %d, got %v", em.removedPerSync, actualRemovedPerSync)
	}

	actualCreated, err := testutil.GetCounterMetricValue(metrics.EndpointSliceChanges.WithLabelValues("create"))
	handleErr(t, err, "endpointSliceChangesCreated")
	if actualCreated != float64(em.numCreated) {
		t.Errorf("Expected endpointSliceChangesCreated to be %d, got %v", em.numCreated, actualCreated)
	}

	actualUpdated, err := testutil.GetCounterMetricValue(metrics.EndpointSliceChanges.WithLabelValues("update"))
	handleErr(t, err, "endpointSliceChangesUpdated")
	if actualUpdated != float64(em.numUpdated) {
		t.Errorf("Expected endpointSliceChangesUpdated to be %d, got %v", em.numUpdated, actualUpdated)
	}

	actualDeleted, err := testutil.GetCounterMetricValue(metrics.EndpointSliceChanges.WithLabelValues("delete"))
	handleErr(t, err, "desiredEndpointSlices")
	if actualDeleted != float64(em.numDeleted) {
		t.Errorf("Expected endpointSliceChangesDeleted to be %d, got %v", em.numDeleted, actualDeleted)
	}

	actualSlicesChangedPerSync, err := testutil.GetHistogramMetricValue(toPrometheusHistogramVec(metrics.EndpointSlicesChangedPerSync).WithLabelValues())
	handleErr(t, err, "slicesChangedPerSync")
	if actualSlicesChangedPerSync != float64(em.slicesChangedPerSync) {
		t.Errorf("Expected slicesChangedPerSync to be %d, got %v", em.slicesChangedPerSync, actualSlicesChangedPerSync)
	}

	actualSyncSuccesses, err := testutil.GetCounterMetricValue(metrics.EndpointSliceSyncs.WithLabelValues("success"))
	handleErr(t, err, "syncSuccesses")
	if actualSyncSuccesses != float64(em.syncSuccesses) {
		t.Errorf("Expected endpointSliceSyncSuccesses to be %d, got %v", em.syncSuccesses, actualSyncSuccesses)
	}

	actualSyncErrors, err := testutil.GetCounterMetricValue(metrics.EndpointSliceSyncs.WithLabelValues("error"))
	handleErr(t, err, "syncErrors")
	if actualSyncErrors != float64(em.syncErrors) {
		t.Errorf("Expected endpointSliceSyncErrors to be %d, got %v", em.syncErrors, actualSyncErrors)
	}
}

func toPrometheusHistogramVec(histogram metric.Vec[metric.Observer]) *prometheus.HistogramVec {
	return (*prometheus.HistogramVec)(reflect.ValueOf(histogram).Elem().FieldByName("ObserverVec").Elem().UnsafePointer())
}

func handleErr(t *testing.T, err error, metricName string) {
	if err != nil {
		t.Errorf("Failed to get %s value, err: %v", metricName, err)
	}
}
