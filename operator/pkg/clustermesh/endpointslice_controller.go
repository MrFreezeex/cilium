package clustermesh

import (
	"context"
	"fmt"
	"time"

	"github.com/cilium/cilium/pkg/k8s"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/k8s/informer"
	"github.com/cilium/cilium/pkg/k8s/utils"
	"github.com/cilium/cilium/pkg/logging/logfields"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	endpointsliceutil "k8s.io/endpointslice/util"
	endpointslicepkg "k8s.io/kubernetes/pkg/controller/util/endpointslice"
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
	key types.NamespacedName
}
type serviceClusterSyncEvent struct {
	key     types.NamespacedName
	cluster string
}

func NewEndpointSliceController(
	clientset k8sClient.Clientset,
	maxEndpointsPerSlice int,
	endpointUpdatesBatchPeriod time.Duration,
	workers int,
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
		workerLoopPeriod: time.Second,
		workers:          workers,
	}

	c.serviceStore, c.serviceInformer = informer.NewInformer(
		utils.ListerWatcherFromTyped[*v1.ServiceList](clientset.CoreV1().Services(v1.NamespaceAll)),
		&v1.Service{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if service := k8s.CastInformerEvent[v1.Service](obj); service != nil {
					key := types.NamespacedName{Name: service.Name, Namespace: service.Namespace}
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
				key := types.NamespacedName{Name: newService.Name, Namespace: newService.Namespace}
				c.queue.Add(serviceSyncEvent{key: key})
			},
			DeleteFunc: func(obj interface{}) {
				if service := k8s.CastInformerEvent[v1.Service](obj); service != nil {
					key := types.NamespacedName{Name: service.Name, Namespace: service.Namespace}
					c.queue.Add(serviceSyncEvent{key: key})
				}
			},
		},
		nil,
	)

	c.endpointSliceStore, c.endpointSliceInformer = informer.NewInformer(
		utils.ListerWatcherFromTyped[*discoveryv1.EndpointSliceList](clientset.DiscoveryV1().EndpointSlices(v1.NamespaceAll)),
		&discoveryv1.EndpointSlice{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				endpointSlice := k8s.CastInformerEvent[discoveryv1.EndpointSlice](obj)
				if endpointSlice == nil {
					return
				}
				cluster := endpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster]
				endpointSliceTracker := c.endpointSliceTrackers[cluster]

				if c.reconciler.ManagedByController(endpointSlice) {
					if endpointSliceTracker == nil || endpointSliceTracker.ShouldSync(endpointSlice) {
						c.queueServiceForEndpointSlice(endpointSlice)
					}
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldEndpointSlice := k8s.CastInformerEvent[discoveryv1.EndpointSlice](oldObj)
				if oldEndpointSlice == nil {
					return
				}
				newEndpointSlice := k8s.CastInformerEvent[discoveryv1.EndpointSlice](newObj)
				if newEndpointSlice == nil {
					return
				}
				// EndpointSlice generation does not change when labels change. Although the
				// controller will never change LabelServiceName, users might. This check
				// ensures that we handle changes to this label.
				oldSvcName := oldEndpointSlice.Labels[discoveryv1.LabelServiceName]
				newSvcName := newEndpointSlice.Labels[discoveryv1.LabelServiceName]
				if oldSvcName != newSvcName {
					c.queueServiceForEndpointSlice(oldEndpointSlice)
					c.queueServiceForEndpointSlice(newEndpointSlice)
					return
				}

				cluster := newEndpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster]
				endpointSliceTracker := c.endpointSliceTrackers[cluster]
				if c.reconciler.ManagedByChanged(oldEndpointSlice, newEndpointSlice) ||
					c.reconciler.ManagedByController(newEndpointSlice) {

					if endpointSliceTracker == nil || endpointSliceTracker.ShouldSync(newEndpointSlice) {
						c.queueServiceForEndpointSlice(newEndpointSlice)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				endpointSlice := k8s.CastInformerEvent[discoveryv1.EndpointSlice](obj)
				if endpointSlice == nil {
					return
				}

				cluster := endpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster]
				endpointSliceTracker := c.endpointSliceTrackers[cluster]
				if c.reconciler.ManagedByController(endpointSlice) {
					if endpointSliceTracker == nil {
						c.queueServiceForEndpointSlice(endpointSlice)
						return
					}

					// This returns false if we didn't expect the EndpointSlice to be
					// deleted. If that is the case, we queue the Service for another sync.
					if endpointSliceTracker.Has(endpointSlice) && !endpointSliceTracker.HandleDeletion(endpointSlice) {
						c.queueServiceForEndpointSlice(endpointSlice)
					}
				}
			},
		},
		nil,
	)

	c.maxEndpointsPerSlice = maxEndpointsPerSlice

	c.triggerTimeTracker = NewTriggerTimeTracker()
	c.endpointSliceTrackers = make(map[string]*endpointsliceutil.EndpointSliceTracker)

	c.endpointUpdatesBatchPeriod = endpointUpdatesBatchPeriod

	c.reconciler = NewEndpointSliceReconciler(
		c.clientset,
		c.maxEndpointsPerSlice,
		controllerName,
	)

	return c
}

func (c *EndpointSliceController) onClusterAdd(cluster string) {
	c.endpointSliceTrackers[cluster] = endpointsliceutil.NewEndpointSliceTracker()
}

func (c *EndpointSliceController) onClusterDelete(cluster string) {
	// We can just remove the endpointslice trackers here and we will be notified
	// after that of each services updates deleted
	delete(c.endpointSliceTrackers, cluster)
}

func (c *EndpointSliceController) onGlobalServiceUpdate(globalSvc *serviceStore.ClusterService) {
	c.queue.AddAfter(serviceClusterSyncEvent{
		key:     types.NamespacedName{Name: globalSvc.Name, Namespace: globalSvc.Namespace},
		cluster: globalSvc.Cluster,
	}, c.endpointUpdatesBatchPeriod)

}

// Controller manages selector-based service endpoint slices
type EndpointSliceController struct {
	clientset k8sClient.Clientset

	serviceInformer cache.Controller
	serviceStore    cache.Store

	endpointSliceInformer cache.Controller
	endpointSliceStore    cache.Store

	cm *clusterMesh

	// endpointSliceTrackers is a map referencing cluster name -> EndpointSliceTracker.
	// An endpointSliceTracker tracks the list of EndpointSlices and associated
	// resource versions expected for each Service. It can help determine if a
	// cached EndpointSlice is out of date.
	endpointSliceTrackers map[string]*endpointsliceutil.EndpointSliceTracker

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
	maxEndpointsPerSlice int

	// workerLoopPeriod is the time between worker runs. The workers
	// process the queue of service changes.
	workerLoopPeriod time.Duration

	// numbers of workers that are concurrently launched
	workers int

	// endpointUpdatesBatchPeriod is an artificial delay added to all service syncs triggered by pod changes.
	// This can be used to reduce overall number of all endpoint slice updates.
	endpointUpdatesBatchPeriod time.Duration
}

func (c *EndpointSliceController) queueServiceForEndpointSlice(endpointSlice *discoveryv1.EndpointSlice) {
	delay := endpointSliceChangeMinSyncDelay
	if c.endpointUpdatesBatchPeriod > delay {
		delay = c.endpointUpdatesBatchPeriod
	}

	if endpointSlice == nil {
		return
	}
	key := getServiceNN(endpointSlice)

	clusterName, ok := endpointSlice.Labels[discoveryv1.LabelServiceName]
	if !ok || clusterName == "" {
		c.queue.AddAfter(serviceSyncEvent{key: key}, delay)
		return
	}

	c.queue.AddAfter(serviceClusterSyncEvent{key: key, cluster: clusterName}, delay)
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same ces
// at the same time
func (c *EndpointSliceController) worker() {
	for c.processEvent() {
	}
}

// Run kicks off the controlled loop
func (c *EndpointSliceController) Run(stopCh <-chan struct{}) {
	log.Info("Bootstrap endpointslice mesh sync controller")
	defer c.queue.ShutDown()
	defer utilruntime.HandleCrash()
	go c.serviceInformer.Run(stopCh)

	go c.endpointSliceInformer.Run(stopCh)

	// Wait for services to be synced
	c.cm.ServicesSynced(context.Background())

	if !cache.WaitForCacheSync(stopCh, c.serviceInformer.HasSynced) {
		return
	}
	if !cache.WaitForCacheSync(stopCh, c.endpointSliceInformer.HasSynced) {
		return
	}

	log.WithField("workers", c.workers).Info("Starting worker threads")
	for i := 0; i < c.workers; i++ {
		go wait.Until(c.worker, c.workerLoopPeriod, stopCh)
	}

	<-stopCh
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
	metricLabel := "success"
	if err != nil {
		if endpointslicepkg.IsStaleInformerCacheErr(err) {
			metricLabel = "stale"
		} else {
			metricLabel = "error"
		}
	}
	c.cm.Metrics.EndpointSliceSyncs.WithLabelValues(metricLabel).Inc()
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

func (c *EndpointSliceController) getClustersForService(service v1.Service) ([]string, error) {
	var clusters []string
	for _, objFromCache := range c.endpointSliceStore.List() {
		endpointSlice, ok := objFromCache.(*discoveryv1.EndpointSlice)
		if !ok {
			return nil, fmt.Errorf("invalid endpointSlice type")
		}
		if endpointSlice.Labels[discoveryv1.LabelServiceName] != service.Name ||
			endpointSlice.Labels[discoveryv1.LabelManagedBy] != controllerName ||
			endpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster] == "" {
			continue
		}
		clusters = append(clusters, endpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster])
	}
	return clusters, nil
}

func (c *EndpointSliceController) listEndpointSlicesForServiceInCluster(service v1.Service, cluster string) ([]*discoveryv1.EndpointSlice, error) {
	var endpointSlices []*discoveryv1.EndpointSlice
	for _, objFromCache := range c.endpointSliceStore.List() {
		endpointSlice, ok := objFromCache.(*discoveryv1.EndpointSlice)
		if !ok {
			return nil, fmt.Errorf("invalid endpointSlice type")
		}
		if endpointSlice.Labels[discoveryv1.LabelServiceName] != service.Name ||
			endpointSlice.Labels[discoveryv1.LabelManagedBy] != controllerName ||
			endpointSlice.Labels[mcsapiv1alpha1.LabelSourceCluster] != cluster {
			continue
		}
		endpointSlices = append(endpointSlices, endpointSlice)
	}
	return endpointSlices, nil
}

func (c *EndpointSliceController) deleteEndpointSlicesForServiceInCluster(service v1.Service, cluster string) error {
	return c.clientset.DiscoveryV1().EndpointSlices(service.Namespace).DeleteCollection(
		context.Background(),
		metav1.DeleteOptions{},
		metav1.ListOptions{
			LabelSelector: discoveryv1.LabelManagedBy + "=" + controllerName + "," +
				discoveryv1.LabelServiceName + "=" + service.Name + "," +
				mcsapiv1alpha1.LabelSourceCluster + "=" + cluster,
		},
	)
}

func dropEndpointSlicesPendingDeletion(endpointSlices []*discoveryv1.EndpointSlice) []*discoveryv1.EndpointSlice {
	n := 0
	for _, endpointSlice := range endpointSlices {
		if endpointSlice.DeletionTimestamp == nil {
			endpointSlices[n] = endpointSlice
			n++
		}
	}
	return endpointSlices[:n]
}

func (c *EndpointSliceController) handleServiceSyncEvent(key types.NamespacedName, cluster *string) error {
	objFromCache, exists, err := c.serviceStore.GetByKey(key.String())
	if err != nil {
		return err
	}

	if !exists {
		c.triggerTimeTracker.DeleteService(key)
		c.reconciler.DeleteService(key.Namespace, key.Name)
		for _, endpointSliceTracker := range c.endpointSliceTrackers {
			endpointSliceTracker.DeleteService(key.Namespace, key.Name)
		}
		// The service has been deleted, return nil so that it won't be retried.
		return nil
	}

	service, ok := objFromCache.(*v1.Service)
	if !ok {
		return fmt.Errorf("service '%s' not found", key)
	}

	clusters := map[string]struct{}{}
	if cluster != nil {
		clusters[*cluster] = struct{}{}
	} else {
		// We get all the clusters refereced by this service in globalService cache
		// and inside our cluster
		for _, cluster := range c.cm.globalServices.getClusters(key.String()) {
			clusters[cluster] = struct{}{}
		}
		clustersForService, err := c.getClustersForService(*service)
		if err != nil {
			return err
		}
		for _, cluster := range clustersForService {
			clusters[cluster] = struct{}{}
		}
	}

	for cluster := range clusters {
		endpointSliceTracker, ok := c.endpointSliceTrackers[cluster]
		if !ok || endpointSliceTracker == nil {
			// If there is no endpoint slice tracker we assume that the cluster
			// have been deleted
			c.triggerTimeTracker.DeleteClusterService(key, cluster)
			c.deleteEndpointSlicesForServiceInCluster(*service, cluster)
			return nil
		}

		endpointSlices, err := c.listEndpointSlicesForServiceInCluster(*service, cluster)
		if err != nil {
			return err
		}

		// Drop EndpointSlices that have been marked for deletion to prevent the controller from getting stuck.
		endpointSlices = dropEndpointSlicesPendingDeletion(endpointSlices)
		if endpointSliceTracker.StaleSlices(service, endpointSlices) {
			return endpointslicepkg.NewStaleInformerCache("EndpointSlice informer cache is out of date")
		}

		globalSvc := c.cm.globalServices.getService(key.String(), cluster)

		// We call ComputeEndpointLastChangeTriggerTime here to make sure that the
		// state of the trigger time tracker gets updated even if the sync turns out
		// to be no-op and we don't update the EndpointSlice objects.
		lastChangeTriggerTime := c.triggerTimeTracker.
			ComputeEndpointLastChangeTriggerTime(key, service, cluster, globalSvc.lastSynced)

		err = c.reconciler.Reconcile(service, globalSvc.svc, endpointSlices, lastChangeTriggerTime, endpointSliceTracker)
		if err != nil {
			return err
		}
	}

	return nil
}

// getServiceNN returns a namespaced name for the Service corresponding to the
// provided EndpointSlice.
func getServiceNN(endpointSlice *discoveryv1.EndpointSlice) types.NamespacedName {
	name := endpointSlice.Labels[discoveryv1.LabelServiceName]
	return types.NamespacedName{Name: name, Namespace: endpointSlice.Namespace}
}
