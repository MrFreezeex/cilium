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

var defaultMaxEndpointsPerSlice = 100

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

// Even when there are no pods, we want to have a placeholder slice for each service
func TestReconcileEmpty(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)
	endpointSliceTracker := endpointsliceutil.NewEndpointSliceTracker()

	r := newReconciler(client, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, &serviceStore.ClusterService{Cluster: "cluster1"}, []*discovery.EndpointSlice{}, time.Now(), endpointSliceTracker)
	expectActions(t, client.Actions(), 1, "create", "endpointslices")

	slices := fetchEndpointSlices(t, client, namespace)
	assert.Len(t, slices, 1, "Expected 1 endpoint slices")

	assert.Regexp(t, "^"+svc.Name, slices[0].Name)
	assert.Equal(t, svc.Name, slices[0].Labels[discovery.LabelServiceName])
	assert.Equal(t, "cluster1", slices[0].Labels[mcsapiv1alpha1.LabelSourceCluster])
	assert.EqualValues(t, []discovery.EndpointPort{}, slices[0].Ports)
	assert.EqualValues(t, []discovery.Endpoint{}, slices[0].Endpoints)
	expectTrackedGeneration(t, endpointSliceTracker, &slices[0], 1)
	expectMetrics(t, r.metrics, expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 1, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 1})
}

// Given a single pod matching a service selector and no existing endpoint slices,
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

	clusterSvc1 := newClusterSvc(1)
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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
				mcsapiv1alpha1.LabelSourceCluster: "cluster1",
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

// Test Helpers

func newClusterSvc(n int) *serviceStore.ClusterService {
	return &serviceStore.ClusterService{
		Cluster: "cluster1",
		Backends: map[string]serviceStore.PortConfiguration{
			fmt.Sprintf("1.2.3.%d", 4+n): map[string]*loadbalancer.L4Addr{},
		},
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
