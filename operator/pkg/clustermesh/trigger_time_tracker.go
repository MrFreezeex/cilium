package clustermesh

import (
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TriggerTimeTracker is used to compute an EndpointsLastChangeTriggerTime
// annotation. See the documentation for that annotation for more details.
//
// Please note that this util may compute a wrong EndpointsLastChangeTriggerTime
// if the same object changes multiple times between two consecutive syncs.
// We're aware of this limitation but we decided to accept it, as fixing it
// would require a major rewrite of the endpoint(Slice) controller and
// Informer framework. Such situations, i.e. frequent updates of the same object
// in a single sync period, should be relatively rare and therefore this util
// should provide a good approximation of the EndpointsLastChangeTriggerTime.
type TriggerTimeTracker struct {
	// ServiceStates is a map, indexed by Service object key, storing the last
	// known Service object state observed during the most recent call of the
	// ComputeEndpointLastChangeTriggerTime function.
	ServiceStates map[types.NamespacedName]ServiceState

	// mutex guarding the serviceStates map.
	mutex sync.Mutex
}

// NewTriggerTimeTracker creates a new instance of the TriggerTimeTracker.
func NewTriggerTimeTracker() *TriggerTimeTracker {
	return &TriggerTimeTracker{
		ServiceStates: make(map[types.NamespacedName]ServiceState),
	}
}

// ServiceState represents a state of an Service object that is known to this util.
type ServiceState struct {
	// lastServiceTriggerTime is a service trigger time observed most recently.
	lastServiceTriggerTime time.Time
	// lastClusterSyncedTimes is a map (Cluster name -> time) storing the remote
	// cluster synced times that were observed during the most recent call of the
	// ComputeEndpointLastChangeTriggerTime function.
	lastClusterSyncedTime map[string]time.Time
}

// ComputeEndpointLastChangeTriggerTime updates the state of the Service/Endpoint
// object being synced and returns the time that should be exported as the
// EndpointsLastChangeTriggerTime annotation.
//
// If the method returns a 'zero' time the EndpointsLastChangeTriggerTime
// annotation shouldn't be exported.
//
// Please note that this function may compute a wrong value if the same object
// (pod/service) changes multiple times between two consecutive syncs.
//
// Important: This method is go-routing safe but only when called for different
// keys. The method shouldn't be called concurrently for the same key! This
// contract is fulfilled in the current implementation of the endpoint(slice)
// controller.
func (t *TriggerTimeTracker) ComputeEndpointLastChangeTriggerTime(
	key types.NamespacedName, service *v1.Service, cluster string, clusterSyncedTime time.Time) time.Time {

	// As there won't be any concurrent calls for the same key, we need to guard
	// access only to the serviceStates map.
	t.mutex.Lock()
	state, wasKnown := t.ServiceStates[key]
	t.mutex.Unlock()

	// Update the state before returning.
	defer func() {
		t.mutex.Lock()
		t.ServiceStates[key] = state
		t.mutex.Unlock()
	}()

	// minChangedTriggerTime is the min trigger time of all trigger times that
	// have changed since the last sync.
	var minChangedTriggerTime time.Time
	if clusterSyncedTime.After(state.lastClusterSyncedTime[cluster]) {
		// Remote endpoints last synced time has changed since the last sync, update minChangedTriggerTime.
		minChangedTriggerTime = min(minChangedTriggerTime, clusterSyncedTime)
	}
	serviceTriggerTime := getServiceTriggerTime(service)
	if serviceTriggerTime.After(state.lastServiceTriggerTime) {
		// Service trigger time has changed since the last sync, update minChangedTriggerTime.
		minChangedTriggerTime = min(minChangedTriggerTime, serviceTriggerTime)
	}

	if state.lastClusterSyncedTime == nil {
		state.lastClusterSyncedTime = make(map[string]time.Time)
	}

	state.lastClusterSyncedTime[cluster] = clusterSyncedTime
	state.lastServiceTriggerTime = serviceTriggerTime

	if !wasKnown {
		// New Service, use Service creationTimestamp.
		return service.CreationTimestamp.Time
	}

	// Regular update of endpoint objects, return min of changed trigger times.
	return minChangedTriggerTime
}

// DeleteService deletes service state stored in this util.
func (t *TriggerTimeTracker) DeleteService(key types.NamespacedName) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	delete(t.ServiceStates, key)
}

// DeleteiClusterService deletes service state stored in this util.
func (t *TriggerTimeTracker) DeleteClusterService(key types.NamespacedName, cluster string) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	state, ok := t.ServiceStates[key]
	if !ok {
		return
	}
	delete(state.lastClusterSyncedTime, cluster)
}

// getServiceTriggerTime returns the time of the service change (trigger) that
// resulted or will result in the endpoint change.
func getServiceTriggerTime(service *v1.Service) (triggerTime time.Time) {
	return service.CreationTimestamp.Time
}

// min returns minimum of the currentMin and newValue or newValue if the currentMin is not set.
func min(currentMin, newValue time.Time) time.Time {
	if currentMin.IsZero() || newValue.Before(currentMin) {
		return newValue
	}
	return currentMin
}
