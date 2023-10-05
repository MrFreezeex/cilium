package clustermesh

import (
	"fmt"
	"time"

	serviceStore "github.com/cilium/cilium/pkg/service/store"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	mcsapiv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

// newEndpoint returns an Endpoint object generated from an address
func newEndpoint(address string) *discovery.Endpoint {
	ready := true
	serving := true
	terminating := false

	return &discovery.Endpoint{
		Addresses: []string{address},
		Conditions: discovery.EndpointConditions{
			Ready:       &ready,
			Serving:     &serving,
			Terminating: &terminating,
		},
	}
}

func isMatchingAddressType(address string, addressType discovery.AddressType) bool {
	isAddressIPv6 := utilnet.IsIPv6String(address)
	return isAddressIPv6 == (addressType == discovery.AddressTypeIPv6)
}

// getEndpointPorts returns a list of EndpointPorts generated from a Service
// and a Global Service.
func getEndpointPorts(service *v1.Service, portConfiguration serviceStore.PortConfiguration) []discovery.EndpointPort {
	endpointPorts := []discovery.EndpointPort{}

	// Allow headless service not to have ports.
	if len(service.Spec.Ports) == 0 && service.Spec.ClusterIP == v1.ClusterIPNone {
		return endpointPorts
	}

	for i := range service.Spec.Ports {
		servicePort := &service.Spec.Ports[i]

		portName := servicePort.Name
		portProto := servicePort.Protocol
		portNum, err := findPort(portConfiguration, servicePort)
		if err != nil {
			log.Info("Failed to find port for service", "service", klog.KObj(service), "err", err)
			continue
		}

		endpointPorts = append(endpointPorts, discovery.EndpointPort{
			Name:        &portName,
			Port:        &portNum,
			Protocol:    &portProto,
			AppProtocol: servicePort.AppProtocol,
		})

	}

	return endpointPorts
}

// newEndpointSlice returns an EndpointSlice generated from a service and
// endpointMeta.
func newEndpointSlice(service *v1.Service, endpointMeta *endpointMeta, clusterName string, controllerName string) *discovery.EndpointSlice {
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	ownerRef := metav1.NewControllerRef(service, gvk)
	epSlice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          map[string]string{},
			GenerateName:    getEndpointSlicePrefix(service.Name, clusterName),
			OwnerReferences: []metav1.OwnerReference{*ownerRef},
			Namespace:       service.Namespace,
		},
		Ports:       endpointMeta.ports,
		AddressType: endpointMeta.addressType,
		Endpoints:   []discovery.Endpoint{},
	}
	// add parent service labels
	epSlice.Labels, _ = setEndpointSliceLabels(epSlice, service, clusterName, controllerName)

	return epSlice
}

// getEndpointSlicePrefix returns a suitable prefix for an EndpointSlice name.
func getEndpointSlicePrefix(serviceName, clusterName string) string {
	// try to use a prettier name (if the name isn't too long)
	prefix := fmt.Sprintf("%s-%s-", serviceName, clusterName)
	if len(apimachineryvalidation.NameIsDNSSubdomain(prefix, true)) != 0 {
		prefix = fmt.Sprintf("%s-%s", serviceName, clusterName)
	}
	if len(apimachineryvalidation.NameIsDNSSubdomain(prefix, true)) != 0 {
		prefix = fmt.Sprintf("%s-", serviceName)
	}
	if len(apimachineryvalidation.NameIsDNSSubdomain(prefix, true)) != 0 {
		prefix = serviceName
	}
	return prefix
}

// ownedBy returns true if the provided EndpointSlice is owned by the provided
// Service.
func ownedBy(endpointSlice *discovery.EndpointSlice, svc *v1.Service) bool {
	for _, o := range endpointSlice.OwnerReferences {
		if o.UID == svc.UID && o.Kind == "Service" && o.APIVersion == "v1" {
			return true
		}
	}
	return false
}

// getSliceToFill will return the EndpointSlice that will be closest to full
// when numEndpoints are added. If no EndpointSlice can be found, a nil pointer
// will be returned.
func getSliceToFill(endpointSlices []*discovery.EndpointSlice, numEndpoints, maxEndpoints int) (slice *discovery.EndpointSlice) {
	closestDiff := maxEndpoints
	var closestSlice *discovery.EndpointSlice
	for _, endpointSlice := range endpointSlices {
		currentDiff := maxEndpoints - (numEndpoints + len(endpointSlice.Endpoints))
		if currentDiff >= 0 && currentDiff < closestDiff {
			closestDiff = currentDiff
			closestSlice = endpointSlice
			if closestDiff == 0 {
				return closestSlice
			}
		}
	}
	return closestSlice
}

// addTriggerTimeAnnotation adds a triggerTime annotation to an EndpointSlice
func addTriggerTimeAnnotation(endpointSlice *discovery.EndpointSlice, triggerTime time.Time) {
	if endpointSlice.Annotations == nil {
		endpointSlice.Annotations = make(map[string]string)
	}

	if !triggerTime.IsZero() {
		endpointSlice.Annotations[v1.EndpointsLastChangeTriggerTime] = triggerTime.UTC().Format(time.RFC3339Nano)
	} else { // No new trigger time, clear the annotation.
		delete(endpointSlice.Annotations, v1.EndpointsLastChangeTriggerTime)
	}
}

// ServiceControllerKey returns a controller key for a Service but derived from
// an EndpointSlice.
func ServiceControllerKey(endpointSlice *discovery.EndpointSlice) (string, error) {
	if endpointSlice == nil {
		return "", fmt.Errorf("nil EndpointSlice passed to ServiceControllerKey()")
	}
	serviceName, ok := endpointSlice.Labels[discovery.LabelServiceName]
	if !ok || serviceName == "" {
		return "", fmt.Errorf("EndpointSlice missing %s label", discovery.LabelServiceName)
	}
	return fmt.Sprintf("%s/%s", endpointSlice.Namespace, serviceName), nil
}

// setEndpointSliceLabels returns a map with the new endpoint slices labels and true if there was an update.
// Slices labels must be equivalent to the Service labels except for the reserved IsHeadlessService, LabelServiceName and LabelManagedBy labels
// Changes to IsHeadlessService, LabelServiceName and LabelManagedBy labels on the Service do not result in updates to EndpointSlice labels.
func setEndpointSliceLabels(epSlice *discovery.EndpointSlice, service *v1.Service, clusterName string, controllerName string) (map[string]string, bool) {
	updated := false
	epLabels := make(map[string]string)
	svcLabels := make(map[string]string)

	// check if the endpoint slice and the service have the same labels
	// clone current slice labels except the reserved labels
	for key, value := range epSlice.Labels {
		if isReservedLabelKey(key) {
			continue
		}
		// copy endpoint slice labels
		epLabels[key] = value
	}

	for key, value := range service.Labels {
		if isReservedLabelKey(key) {
			log.Info("Service using reserved endpoint slices label service ", klog.KObj(service), " skipping ", key, " label ", value)
			continue
		}
		// copy service labels
		svcLabels[key] = value
	}

	// if the labels are not identical update the slice with the corresponding service labels
	if !apiequality.Semantic.DeepEqual(epLabels, svcLabels) {
		updated = true
	}

	// add or remove headless label depending on the service Type
	if !isServiceIPSet(service) {
		svcLabels[v1.IsHeadlessService] = ""
	} else {
		delete(svcLabels, v1.IsHeadlessService)
	}

	// override endpoint slices reserved labels
	svcLabels[discovery.LabelServiceName] = service.Name
	svcLabels[mcsapiv1alpha1.LabelSourceCluster] = clusterName
	svcLabels[discovery.LabelManagedBy] = controllerName

	return svcLabels, updated
}

// isReservedLabelKey return true if the label is one of the reserved label for slices
func isReservedLabelKey(label string) bool {
	if label == discovery.LabelServiceName ||
		label == discovery.LabelManagedBy ||
		label == v1.IsHeadlessService ||
		label == mcsapiv1alpha1.LabelSourceCluster {
		return true
	}
	return false
}

// endpointSliceEndpointLen helps sort endpoint slices by the number of
// endpoints they contain.
type endpointSliceEndpointLen []*discovery.EndpointSlice

func (sl endpointSliceEndpointLen) Len() int      { return len(sl) }
func (sl endpointSliceEndpointLen) Swap(i, j int) { sl[i], sl[j] = sl[j], sl[i] }
func (sl endpointSliceEndpointLen) Less(i, j int) bool {
	return len(sl[i].Endpoints) > len(sl[j].Endpoints)
}

// returns a map of address types used by a service
func getAddressTypesForService(service *v1.Service) sets.Set[discovery.AddressType] {
	serviceSupportedAddresses := sets.New[discovery.AddressType]()
	// TODO: (khenidak) when address types are removed in favor of
	// v1.IPFamily this will need to be removed, and work directly with
	// v1.IPFamily types

	// IMPORTANT: we assume that IP of (discovery.AddressType enum) is never in use
	// as it gets deprecated
	for _, family := range service.Spec.IPFamilies {
		if family == v1.IPv4Protocol {
			serviceSupportedAddresses.Insert(discovery.AddressTypeIPv4)
		}

		if family == v1.IPv6Protocol {
			serviceSupportedAddresses.Insert(discovery.AddressTypeIPv6)
		}
	}

	if serviceSupportedAddresses.Len() > 0 {
		return serviceSupportedAddresses // we have found families for this service
	}

	// TODO (khenidak) remove when (1) dual stack becomes
	// enabled by default (2) v1.19 falls off supported versions

	// Why do we need this:
	// a cluster being upgraded to the new apis
	// will have service.spec.IPFamilies: nil
	// if the controller manager connected to old api
	// server. This will have the nasty side effect of
	// removing all slices already created for this service.
	// this will disable all routing to service vip (ClusterIP)
	// this ensures that this does not happen. Same for headless services
	// we assume it is dual stack, until they get defaulted by *new* api-server
	// this ensures that traffic is not disrupted  until then. But *may*
	// include undesired families for headless services until then.

	if len(service.Spec.ClusterIP) > 0 && service.Spec.ClusterIP != v1.ClusterIPNone { // headfull
		addrType := discovery.AddressTypeIPv4
		if utilnet.IsIPv6String(service.Spec.ClusterIP) {
			addrType = discovery.AddressTypeIPv6
		}
		serviceSupportedAddresses.Insert(addrType)
		log.Info("Couldn't find ipfamilies for service. This could happen if controller manager is connected to an old apiserver that does not support ip families yet. EndpointSlices for this Service will use addressType as the IP Family based on familyOf(ClusterIP).", "service", klog.KObj(service), "addressType", addrType, "clusterIP", service.Spec.ClusterIP)
		return serviceSupportedAddresses
	}

	// We assume two familised for headless services
	serviceSupportedAddresses.Insert(discovery.AddressTypeIPv4)
	serviceSupportedAddresses.Insert(discovery.AddressTypeIPv6)
	log.Info("Couldn't find ipfamilies for headless service, likely because controller manager is likely connected to an old apiserver that does not support ip families yet. The service endpoint slice will use dual stack families until api-server default it correctly", "service", klog.KObj(service))
	return serviceSupportedAddresses
}

// isServiceIPSet aims to check if the service's ClusterIP is set or not
// the objective is not to perform validation here
// copied from k8s.io/kubernetes/pkg/apis/core/v1/helper
func isServiceIPSet(service *v1.Service) bool {
	return service.Spec.ClusterIP != v1.ClusterIPNone && service.Spec.ClusterIP != ""
}

// findPort locates the port for the given portConfiguration from cluster service and
// service port.  If the targetPort is a number, use that.  If the targetPort is a string, look that
// string up in all named ports.  If no match is found, fail.
// copied and adapted for Cilium from k8s.io/kubernetes/pkg/api/v1/pod
func findPort(portConfig serviceStore.PortConfiguration, svcPort *v1.ServicePort) (int32, error) {
	portName := svcPort.TargetPort
	switch portName.Type {
	case intstr.String:
		name := portName.StrVal
		port, ok := portConfig[name]
		if ok {
			return int32(port.Port), nil
		}
	case intstr.Int:
		return int32(portName.IntValue()), nil
	}

	return 0, fmt.Errorf("no suitable port found")
}
