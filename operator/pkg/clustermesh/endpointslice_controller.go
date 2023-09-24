package clustermesh

import (
	"context"
	"fmt"
	"time"

	"github.com/cilium/cilium/pkg/k8s"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/k8s/informer"
	slim_discoveryv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"
	"github.com/cilium/cilium/pkg/k8s/utils"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	mcsapiv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

const (
	// maxRetries is the number of times a service will be retried before it is
	// dropped out of the queue. Any sync error, such as a failure to create or
	// update an EndpointSlice could trigger a retry. With the current
	// rate-limiter in use (1s*2^(numRetries-1)) the following numbers represent
	// the sequence of delays between successive queuings of a service.
	//
	// 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 512s, 1000s (max)
	maxRetries = 15

	// endpointSliceChangeMinSyncDelay indicates the mininum delay before
	// queuing a syncService call after an EndpointSlice changes. If
	// endpointUpdatesBatchPeriod is greater than this value, it will be used
	// instead. This helps batch processing of changes to multiple
	// EndpointSlices.
	endpointSliceChangeMinSyncDelay = 1 * time.Second

	// defaultSyncBackOff is the default backoff period for syncService calls.
	defaultSyncBackOff = 1 * time.Second
	// maxSyncBackOff is the max backoff period for syncService calls.
	maxSyncBackOff = 1000 * time.Second

	// controllerName is a unique value used with LabelManagedBy to indicated
	// the component managing an EndpointSlice.
	controllerName = "endpointslice-mesh-controller.cilium.io"
)

// event types
type serviceSyncEvent struct {
	key string
}
type serviceClusterSyncEvent struct {
	key     string
	cluster string
}

// NewController creates and initializes a new Controller
func NewEndpointSliceController(
	clientset k8sClient.Clientset,
	maxEndpointsPerSlice int32,
	endpointUpdatesBatchPeriod time.Duration,
) *EndpointSliceController {
	c := &EndpointSliceController{
		clientset: clientset,
		// This is similar to the DefaultControllerRateLimiter, just with a
		// significantly higher default backoff (1s vs 5ms). This controller
		// processes events that can require significant EndpointSlice changes,
		// such as an update to a Service or Deployment. A more significant
		// rate limit back off here helps ensure that the Controller does not
		// overwhelm the API Server.
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewMaxOfRateLimiter(
			workqueue.NewItemExponentialFailureRateLimiter(defaultSyncBackOff, maxSyncBackOff),
			// 10 qps, 100 bucket size. This is only for retry speed and its
			// only the overall factor (not per item).
			&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
		), "endpoint_slice_mesh"),
	}

	c.serviceStore, c.serviceInformer = informer.NewInformer(
		utils.ListerWatcherFromTyped[*v1.ServiceList](clientset.CoreV1().Services(v1.NamespaceAll)),
		&v1.Service{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if service := k8s.CastInformerEvent[v1.Service](obj); service != nil {
					key, err := cache.MetaNamespaceKeyFunc(service)
					if err != nil {
						return
					}
					c.queue.Add(serviceSyncEvent{key: key})
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldService := k8s.CastInformerEvent[v1.Service](oldObj)
				if oldService == nil {
					return
				}
				newService := k8s.CastInformerEvent[v1.Service](newObj)
				if newService == nil {
					return
				}
				if oldService.DeepEqual(newService) {
					return
				}
				key, err := cache.MetaNamespaceKeyFunc(newService)
				if err != nil {
					return
				}
				c.queue.Add(serviceSyncEvent{key: key})
			},
			DeleteFunc: func(obj interface{}) {
				if service := k8s.CastInformerEvent[v1.Service](obj); service != nil {
					key, err := cache.MetaNamespaceKeyFunc(service)
					if err != nil {
						return
					}
					c.queue.Add(serviceSyncEvent{key: key})
				}
			},
		},
		nil,
	)

	c.endpointSliceStore, c.endpointSliceInformer = informer.NewInformer(
		utils.ListerWatcherFromTyped[*slim_discoveryv1.EndpointSliceList](clientset.Slim().DiscoveryV1().EndpointSlices(v1.NamespaceAll)),
		&slim_discoveryv1.EndpointSlice{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				endpointSlice := k8s.CastInformerEvent[slim_discoveryv1.EndpointSlice](obj)
				if endpointSlice == nil {
					return
				}
				if c.reconciler.ManagedByController(endpointSlice) && c.endpointSliceTracker.ShouldSync(endpointSlice) {
					c.queueServiceForEndpointSlice(endpointSlice)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldEndpointSlice := k8s.CastInformerEvent[slim_discoveryv1.EndpointSlice](oldObj)
				if oldEndpointSlice == nil {
					return
				}
				newEndpointSlice := k8s.CastInformerEvent[slim_discoveryv1.EndpointSlice](newObj)
				if newEndpointSlice == nil {
					return
				}
				if oldEndpointSlice.DeepEqual(newEndpointSlice) {
					return
				}
				// EndpointSlice generation does not change when labels change. Although the
				// controller will never change LabelServiceName, users might. This check
				// ensures that we handle changes to this label.
				oldSvcName := oldEndpointSlice.Labels[slim_discoveryv1.LabelServiceName]
				newSvcName := newEndpointSlice.Labels[slim_discoveryv1.LabelServiceName]
				if oldSvcName != newSvcName {
					c.queueServiceForEndpointSlice(oldEndpointSlice)
					c.queueServiceForEndpointSlice(newEndpointSlice)
					return
				}
				if c.reconciler.ManagedByChanged(oldEndpointSlice, newEndpointSlice) ||
					(c.reconciler.ManagedByController(newEndpointSlice) && c.endpointSliceTracker.ShouldSync(newEndpointSlice)) {
					c.queueServiceForEndpointSlice(newEndpointSlice)
				}
			},
			DeleteFunc: func(obj interface{}) {
				endpointSlice := k8s.CastInformerEvent[slim_discoveryv1.EndpointSlice](obj)
				if endpointSlice == nil {
					return
				}
				if c.reconciler.ManagedByController(endpointSlice) && c.endpointSliceTracker.Has(endpointSlice) {
					// This returns false if we didn't expect the EndpointSlice to be
					// deleted. If that is the case, we queue the Service for another sync.
					if !c.endpointSliceTracker.HandleDeletion(endpointSlice) {
						c.queueServiceForEndpointSlice(endpointSlice)
					}
				}
			},
		},
		nil,
	)

	c.maxEndpointsPerSlice = maxEndpointsPerSlice

	c.triggerTimeTracker = NewTriggerTimeTracker()
	c.endpointSliceTracker = NewEndpointSliceTracker()

	c.endpointUpdatesBatchPeriod = endpointUpdatesBatchPeriod

	c.reconciler = NewEndpointSliceReconciler(
		c.clientset,
		c.maxEndpointsPerSlice,
		c.endpointSliceTracker,
	)

	return c
}

// Controller manages selector-based service endpoint slices
type EndpointSliceController struct {
	clientset k8sClient.Clientset

	serviceInformer cache.Controller
	serviceStore    cache.Store

	endpointSliceInformer cache.Controller
	endpointSliceStore    cache.Store

	cm *clusterMesh

	// endpointSliceTracker tracks the list of EndpointSlices and associated
	// resource versions expected for each Service. It can help determine if a
	// cached EndpointSlice is out of date.
	endpointSliceTracker *EndpointSliceTracker

	// reconciler is an util used to reconcile EndpointSlice changes.
	reconciler *EndpointSliceReconciler

	// triggerTimeTracker is an util used to compute and export the
	// EndpointsLastChangeTriggerTime annotation.
	triggerTimeTracker *TriggerTimeTracker

	// Services that need to be updated. A channel is inappropriate here,
	// because it allows services with lots of pods to be serviced much
	// more often than services with few pods; it also would cause a
	// service that's inserted multiple times to be processed more than
	// necessary.
	queue workqueue.RateLimitingInterface

	// maxEndpointsPerSlice references the maximum number of endpoints that
	// should be added to an EndpointSlice
	maxEndpointsPerSlice int32

	// endpointUpdatesBatchPeriod is an artificial delay added to all service syncs triggered by pod changes.
	// This can be used to reduce overall number of all endpoint slice updates.
	endpointUpdatesBatchPeriod time.Duration
}

func (c *EndpointSliceController) queueServiceForEndpointSlice(endpointSlice *slim_discoveryv1.EndpointSlice) {
	delay := endpointSliceChangeMinSyncDelay
	if c.endpointUpdatesBatchPeriod > delay {
		delay = c.endpointUpdatesBatchPeriod
	}

	if endpointSlice == nil {
		return
	}
	key := getServiceKey(endpointSlice)

	clusterName, ok := endpointSlice.Labels[slim_discoveryv1.LabelServiceName]
	if !ok || clusterName == "" {
		c.queue.AddAfter(serviceSyncEvent{key: key}, delay)
		return
	}

	c.queue.AddAfter(serviceClusterSyncEvent{key: key, cluster: clusterName}, delay)
}

// Run kicks off the controlled loop
func (c *EndpointSliceController) Run() {
	defer c.queue.ShutDown()
	go c.serviceInformer.Run(wait.NeverStop)
	go c.endpointSliceInformer.Run(wait.NeverStop)
	c.cm.ServicesSynced(context.Background())

	if !cache.WaitForCacheSync(wait.NeverStop, c.serviceInformer.HasSynced) {
		return
	}
	if !cache.WaitForCacheSync(wait.NeverStop, c.endpointSliceInformer.HasSynced) {
		return
	}

	for c.processEvent() {
	}
}

func (c *EndpointSliceController) processEvent() bool {
	event, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(event)
	err := c.handleEvent(event)
	if err == nil {
		c.queue.Forget(event)
	} else if c.queue.NumRequeues(event) < maxRetries {
		c.queue.AddRateLimited(event)
	} else {
		log.WithError(err).Errorf("Failed to process EndpointSlice event, skipping: %s", event)
		c.queue.Forget(event)
	}
	return true
}

func (c *EndpointSliceController) handleEvent(event interface{}) error {
	var err error
	switch ev := event.(type) {
	case serviceSyncEvent:
		log.WithField(logfields.ServiceKey, ev.key).Debug("Handling ingress added event")
		err = c.handleServiceSyncEvent(ev.key, nil)
	case serviceClusterSyncEvent:
		log.WithField(logfields.ServiceKey, ev.key).WithField("cluster", ev.cluster).Debug("Handling ingress added event")
		err = c.handleServiceSyncEvent(ev.key, &ev.cluster)
	default:
		err = fmt.Errorf("received an unknown event: %t", ev)
	}
	return err
}

func (c *EndpointSliceController) listEndpointSlicesForService(service v1.Service) ([]*slim_discoveryv1.EndpointSlice, error) {
	var endpointSlices []*slim_discoveryv1.EndpointSlice
	for _, objFromCache := range c.endpointSliceStore.List() {
		endpointSlice, ok := objFromCache.(*slim_discoveryv1.EndpointSlice)
		if !ok {
			return nil, fmt.Errorf("invalid endpointSlice type")
		}
		if endpointSlice.Labels[slim_discoveryv1.LabelServiceName] != service.Name ||
			endpointSlice.Labels[discoveryv1.LabelManagedBy] != controllerName ||
			endpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster] == "" {
			continue
		}
		endpointSlices = append(endpointSlices, endpointSlice)
	}
	return endpointSlices, nil
}

func (c *EndpointSliceController) listEndpointSlicesForServiceInCluster(service v1.Service, cluster string) ([]*slim_discoveryv1.EndpointSlice, error) {
	var endpointSlices []*slim_discoveryv1.EndpointSlice
	for _, objFromCache := range c.endpointSliceStore.List() {
		endpointSlice, ok := objFromCache.(*slim_discoveryv1.EndpointSlice)
		if !ok {
			return nil, fmt.Errorf("invalid endpointSlice type")
		}
		if endpointSlice.Labels[slim_discoveryv1.LabelServiceName] != service.Name ||
			endpointSlice.Labels[discoveryv1.LabelManagedBy] != controllerName ||
			endpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster] != cluster {
			continue
		}
		endpointSlices = append(endpointSlices, endpointSlice)
	}
	return endpointSlices, nil
}

func dropEndpointSlicesPendingDeletion(endpointSlices []*slim_discoveryv1.EndpointSlice) []*slim_discoveryv1.EndpointSlice {
	n := 0
	for _, endpointSlice := range endpointSlices {
		if endpointSlice.DeletionTimestamp == nil {
			endpointSlices[n] = endpointSlice
			n++
		}
	}
	return endpointSlices[:n]
}

func (c *EndpointSliceController) handleServiceSyncEvent(key string, cluster *string) error {
	objFromCache, exists, err := c.serviceStore.GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		c.triggerTimeTracker.DeleteService(key)
		c.reconciler.DeleteService(key)
		c.endpointSliceTracker.DeleteService(key)
		// The service has been deleted, return nil so that it won't be retried.
		return nil
	}

	service, ok := objFromCache.(*v1.Service)
	if !ok {
		return fmt.Errorf("service '%s' not found", key)
	}

	var clusters []string
	if cluster != nil {
		clusters = []string{*cluster}
	} else {
		clusters = c.cm.globalServices.getClusters(key)
	}

	for _, cluster := range clusters {
		endpointSlices, err := c.listEndpointSlicesForServiceInCluster(*service, cluster)
		if err != nil {
			return err
		}

		// Drop EndpointSlices that have been marked for deletion to prevent the controller from getting stuck.
		endpointSlices = dropEndpointSlicesPendingDeletion(endpointSlices)

		if c.endpointSliceTracker.StaleSlices(service, endpointSlices) {
			return fmt.Errorf("EndpointSlice informer cache is out of date")
		}

		// We call ComputeEndpointLastChangeTriggerTime here to make sure that the
		// state of the trigger time tracker gets updated even if the sync turns out
		// to be no-op and we don't update the EndpointSlice objects.
		lastChangeTriggerTime := c.triggerTimeTracker.
			ComputeEndpointLastChangeTriggerTime(key, service, pods)

		err = c.reconciler.Reconcile(service, endpointSlices, lastChangeTriggerTime)
		if err != nil {
			return err
		}

		return nil
	}
}
