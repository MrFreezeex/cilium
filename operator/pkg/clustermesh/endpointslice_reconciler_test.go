package clustermesh

import (
	"context"
	"reflect"
	"testing"
	"time"

	serviceStore "github.com/cilium/cilium/pkg/service/store"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/component-base/metrics/testutil"
	endpointsliceutil "k8s.io/endpointslice/util"
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

// Test Helpers

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
	desiredSlices                int
	actualSlices                 int
	desiredEndpoints             int
	addedPerSync                 int
	removedPerSync               int
	numCreated                   int
	numUpdated                   int
	numDeleted                   int
	slicesChangedPerSync         int
	slicesChangedPerSyncTopology int
	syncSuccesses                int
	syncErrors                   int
}

func expectMetrics(t *testing.T, metrics *Metrics, em expectedMetrics) {
	t.Helper()

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

	actualAddedPerSync, err := testutil.GetHistogramMetricValue(metrics.EndpointsAddedPerSync.WithLabelValues())
	handleErr(t, err, "endpointsAddedPerSync")
	if actualAddedPerSync != float64(em.addedPerSync) {
		t.Errorf("Expected endpointsAddedPerSync to be %d, got %v", em.addedPerSync, actualAddedPerSync)
	}

	actualRemovedPerSync, err := testutil.GetHistogramMetricValue(metrics.EndpointsRemovedPerSync.WithLabelValues())
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

	actualSlicesChangedPerSync, err := testutil.GetHistogramMetricValue(metrics.EndpointSlicesChangedPerSync.WithLabelValues("Disabled"))
	handleErr(t, err, "slicesChangedPerSync")
	if actualSlicesChangedPerSync != float64(em.slicesChangedPerSync) {
		t.Errorf("Expected slicesChangedPerSync to be %d, got %v", em.slicesChangedPerSync, actualSlicesChangedPerSync)
	}

	actualSlicesChangedPerSyncTopology, err := testutil.GetHistogramMetricValue(metrics.EndpointSlicesChangedPerSync.WithLabelValues("Auto"))
	handleErr(t, err, "slicesChangedPerSyncTopology")
	if actualSlicesChangedPerSyncTopology != float64(em.slicesChangedPerSyncTopology) {
		t.Errorf("Expected slicesChangedPerSyncTopology to be %d, got %v", em.slicesChangedPerSyncTopology, actualSlicesChangedPerSyncTopology)
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

func handleErr(t *testing.T, err error, metricName string) {
	if err != nil {
		t.Errorf("Failed to get %s value, err: %v", metricName, err)
	}
}
