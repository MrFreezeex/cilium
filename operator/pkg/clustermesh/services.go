// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/exp/maps"

	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/metrics/metric"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
)

type clusterServiceWithTime struct {
	svc        *serviceStore.ClusterService
	lastSynced time.Time
}

type globalService struct {
	clusterServices map[string]*clusterServiceWithTime
}

func newGlobalService() *globalService {
	return &globalService{
		clusterServices: map[string]*clusterServiceWithTime{},
	}
}

type globalServiceCache struct {
	mutex  lock.RWMutex
	byName map[string]*globalService

	// metricTotalGlobalServices is the gauge metric for total of global services
	metricTotalGlobalServices metric.Gauge
}

func newGlobalServiceCache(metricTotalGlobalServices metric.Gauge) *globalServiceCache {
	return &globalServiceCache{
		byName:                    map[string]*globalService{},
		metricTotalGlobalServices: metricTotalGlobalServices,
	}
}

// has returns whether a given service is present in the cache.
func (c *globalServiceCache) has(svc *serviceStore.ClusterService) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if globalSvc, ok := c.byName[svc.NamespaceServiceName()]; ok {
		_, ok = globalSvc.clusterServices[svc.Cluster]
		return ok
	}

	return false
}

// getClusters returns the list of clusters that a specified service exports
func (c *globalServiceCache) getClusters(key string) []string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if globalSvc, ok := c.byName[key]; ok {
		return maps.Keys(globalSvc.clusterServices)
	}

	return []string{}
}

// getService returns the service for a specific cluster
func (c *globalServiceCache) getService(key string, cluster string) *clusterServiceWithTime {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if globalSvc, ok := c.byName[key]; ok {
		if svc, ok := globalSvc.clusterServices[cluster]; ok {
			return svc
		}
	}

	return nil
}

func (c *globalServiceCache) onUpdate(svc *serviceStore.ClusterService) {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.ServiceName: svc.String(),
		logfields.ClusterName: svc.Cluster,
	})

	c.mutex.Lock()

	// Validate that the global service is known
	globalSvc, ok := c.byName[svc.NamespaceServiceName()]
	if !ok {
		globalSvc = newGlobalService()
		c.byName[svc.NamespaceServiceName()] = globalSvc
		scopedLog.Debugf("Created global service %s", svc.NamespaceServiceName())
		c.metricTotalGlobalServices.Set(float64(len(c.byName)))
	}

	scopedLog.Debugf("Updated service definition of remote cluster %#v", svc)

	globalSvc.clusterServices[svc.Cluster] = &clusterServiceWithTime{svc: svc, lastSynced: time.Now()}
	c.mutex.Unlock()
}

// must be called with c.mutex held
func (c *globalServiceCache) delete(globalService *globalService, clusterName, serviceName string) bool {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.ServiceName: serviceName,
		logfields.ClusterName: clusterName,
	})

	if _, ok := globalService.clusterServices[clusterName]; !ok {
		scopedLog.Debug("Ignoring delete request for unknown cluster")
		return false
	}

	scopedLog.Debugf("Deleted service definition of remote cluster")
	delete(globalService.clusterServices, clusterName)

	// After the last cluster service is removed, remove the
	// global service
	if len(globalService.clusterServices) == 0 {
		scopedLog.Debugf("Deleted global service %s", serviceName)
		delete(c.byName, serviceName)
		c.metricTotalGlobalServices.Set(float64(len(c.byName)))
	}

	return true
}

func (c *globalServiceCache) onDelete(svc *serviceStore.ClusterService) bool {
	scopedLog := log.WithFields(logrus.Fields{logfields.ServiceName: svc.String()})
	scopedLog.Debug("Delete event for service")

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if globalService, ok := c.byName[svc.NamespaceServiceName()]; ok {
		return c.delete(globalService, svc.Cluster, svc.NamespaceServiceName())
	} else {
		scopedLog.Debugf("Ignoring delete request for unknown global service")
		return false
	}
}

// size returns the number of global services in the cache
func (c *globalServiceCache) size() (num int) {
	c.mutex.RLock()
	num = len(c.byName)
	c.mutex.RUnlock()
	return
}

type remoteServiceObserver struct {
	remoteCluster *remoteCluster
}

// OnUpdate is called when a service in a remote cluster is updated
func (r *remoteServiceObserver) OnUpdate(key store.Key) {
	if svc, ok := key.(*serviceStore.ClusterService); ok {
		scopedLog := log.WithFields(logrus.Fields{logfields.ServiceName: svc.String()})
		scopedLog.Debugf("Update event of remote service %#v", svc)

		mesh := r.remoteCluster.mesh

		// Short-circuit the handling of non-shared services
		if !svc.Shared {
			if mesh.globalServices.has(svc) {
				scopedLog.Debug("Previously shared service is no longer shared: triggering deletion event")
				r.OnDelete(key)
			} else {
				scopedLog.Debug("Ignoring remote service update: service is not shared")
			}
			return
		}

		mesh.globalServices.onUpdate(svc)
		mesh.EndpointSliceController.onClusterServiceUpdate(svc)
	} else {
		log.Warningf("Received unexpected remote service update object %+v", key)
	}
}

// OnDelete is called when a service in a remote cluster is deleted
func (r *remoteServiceObserver) OnDelete(key store.NamedKey) {
	if svc, ok := key.(*serviceStore.ClusterService); ok {
		scopedLog := log.WithFields(logrus.Fields{logfields.ServiceName: svc.String()})
		scopedLog.Debugf("Delete event of remote service %#v", svc)

		mesh := r.remoteCluster.mesh
		// Short-circuit the deletion logic if the service was not present (i.e., not shared)
		if !mesh.globalServices.onDelete(svc) {
			scopedLog.Debugf("Ignoring remote service delete. Service was not shared")
			return
		}

		mesh.EndpointSliceController.onClusterServiceUpdate(svc)
	} else {
		log.Warningf("Received unexpected remote service delete object %+v", key)
	}
}
