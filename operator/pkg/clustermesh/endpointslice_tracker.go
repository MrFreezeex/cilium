package clustermesh

// This file is adapted from the endpointslice_tracker.go in Kubernetes Upstream
// used for the upstream endpointslice controller

import (
	"sync"

	slim_discoveryv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

const (
	deletionExpected = -1
)

// GenerationsBySlice tracks expected EndpointSlice generations by EndpointSlice
// uid. A value of deletionExpected (-1) may be used here to indicate that we
// expect this EndpointSlice to be deleted.
type GenerationsBySlice map[types.UID]int64

// EndpointSliceTracker tracks EndpointSlices and their associated generation to
// help determine if a change to an EndpointSlice has been processed by the
// EndpointSlice controller.
type EndpointSliceTracker struct {
	// lock protects generationsByService.
	lock sync.Mutex
	// generationsByService tracks the generations of EndpointSlices for each
	// Service.
	generationsByService map[string]GenerationsBySlice
}

// NewEndpointSliceTracker creates and initializes a new endpointSliceTracker.
func NewEndpointSliceTracker() *EndpointSliceTracker {
	return &EndpointSliceTracker{
		generationsByService: map[string]GenerationsBySlice{},
	}
}

// Has returns true if the endpointSliceTracker has a generation for the
// provided EndpointSlice.
func (est *EndpointSliceTracker) Has(endpointSlice *slim_discoveryv1.EndpointSlice) bool {
	est.lock.Lock()
	defer est.lock.Unlock()

	gfs, ok := est.GenerationsForSliceUnsafe(endpointSlice)
	if !ok {
		return false
	}
	_, ok = gfs[endpointSlice.UID]
	return ok
}

// ShouldSync returns true if this endpointSliceTracker does not have a
// generation for the provided EndpointSlice or it is greater than the
// generation of the tracked EndpointSlice.
func (est *EndpointSliceTracker) ShouldSync(endpointSlice *slim_discoveryv1.EndpointSlice) bool {
	est.lock.Lock()
	defer est.lock.Unlock()

	gfs, ok := est.GenerationsForSliceUnsafe(endpointSlice)
	if !ok {
		return true
	}
	g, ok := gfs[endpointSlice.UID]
	return !ok || endpointSlice.Generation > g
}

// StaleSlices returns true if any of the following are true:
//  1. One or more of the provided EndpointSlices have older generations than the
//     corresponding tracked ones.
//  2. The tracker is expecting one or more of the provided EndpointSlices to be
//     deleted. (EndpointSlices that have already been marked for deletion are ignored here.)
//  3. The tracker is tracking EndpointSlices that have not been provided.
func (est *EndpointSliceTracker) StaleSlices(service *v1.Service, endpointSlices []*slim_discoveryv1.EndpointSlice) bool {
	est.lock.Lock()
	defer est.lock.Unlock()

	key, err := cache.MetaNamespaceKeyFunc(service)
	if err != nil {
		return false
	}
	gfs, ok := est.generationsByService[key]
	if !ok {
		return false
	}
	providedSlices := map[types.UID]int64{}
	for _, endpointSlice := range endpointSlices {
		providedSlices[endpointSlice.UID] = endpointSlice.Generation
		g, ok := gfs[endpointSlice.UID]
		if ok && (g == deletionExpected || g > endpointSlice.Generation) {
			return true
		}
	}
	for uid, generation := range gfs {
		if generation == deletionExpected {
			continue
		}
		_, ok := providedSlices[uid]
		if !ok {
			return true
		}
	}
	return false
}

// Update adds or updates the generation in this endpointSliceTracker for the
// provided EndpointSlice.
func (est *EndpointSliceTracker) Update(endpointSlice *slim_discoveryv1.EndpointSlice) {
	est.lock.Lock()
	defer est.lock.Unlock()

	gfs, ok := est.GenerationsForSliceUnsafe(endpointSlice)

	if !ok {
		gfs = GenerationsBySlice{}
		est.generationsByService[getServiceKey(endpointSlice)] = gfs
	}
	gfs[endpointSlice.UID] = endpointSlice.Generation
}

// DeleteService removes the set of generations tracked for the Service.
func (est *EndpointSliceTracker) DeleteService(key string) {
	est.lock.Lock()
	defer est.lock.Unlock()

	delete(est.generationsByService, key)
}

// ExpectDeletion sets the generation to deletionExpected in this
// endpointSliceTracker for the provided EndpointSlice.
func (est *EndpointSliceTracker) ExpectDeletion(endpointSlice *slim_discoveryv1.EndpointSlice) {
	est.lock.Lock()
	defer est.lock.Unlock()

	gfs, ok := est.GenerationsForSliceUnsafe(endpointSlice)

	if !ok {
		gfs = GenerationsBySlice{}
		est.generationsByService[getServiceKey(endpointSlice)] = gfs
	}
	gfs[endpointSlice.UID] = deletionExpected
}

// HandleDeletion removes the generation in this endpointSliceTracker for the
// provided EndpointSlice. This returns true if the tracker expected this
// EndpointSlice to be deleted and false if not.
func (est *EndpointSliceTracker) HandleDeletion(endpointSlice *slim_discoveryv1.EndpointSlice) bool {
	est.lock.Lock()
	defer est.lock.Unlock()

	gfs, ok := est.GenerationsForSliceUnsafe(endpointSlice)

	if ok {
		g, ok := gfs[endpointSlice.UID]
		delete(gfs, endpointSlice.UID)
		if ok && g != deletionExpected {
			return false
		}
	}

	return true
}

// GenerationsForSliceUnsafe returns the generations for the Service
// corresponding to the provided EndpointSlice, and a bool to indicate if it
// exists. A lock must be applied before calling this function.
func (est *EndpointSliceTracker) GenerationsForSliceUnsafe(endpointSlice *slim_discoveryv1.EndpointSlice) (GenerationsBySlice, bool) {
	key := getServiceKey(endpointSlice)
	generations, ok := est.generationsByService[key]
	return generations, ok
}

// getServiceNN returns a namespaced name for the Service corresponding to the
// provided EndpointSlice.
func getServiceKey(endpointSlice *slim_discoveryv1.EndpointSlice) string {
	key := endpointSlice.Labels[slim_discoveryv1.LabelServiceName]
	if endpointSlice.Namespace != "" {
		key += "/" + endpointSlice.Namespace
	}
	return key
}
