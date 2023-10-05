// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Copyright 2019 The Kubernetes Authors.

// Most of the logic here are extracted from Kubernetes endpointslice
// controller/reconciler and adapted for Cilium clustermesh use case.

package clustermesh

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	mcsapiv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

func TestNewEndpointSlice(t *testing.T) {
	ipAddressType := discovery.AddressTypeIPv4
	portName := "foo"
	clusterName := "cluster1"
	protocol := v1.ProtocolTCP
	endpointMeta := endpointMeta{
		ports:       []discovery.EndpointPort{{Name: &portName, Protocol: &protocol}},
		addressType: ipAddressType,
	}
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec: v1.ServiceSpec{
			ClusterIP: "1.1.1.1",
			Ports:     []v1.ServicePort{{Port: 80}},
			Selector:  map[string]string{"foo": "bar"},
		},
	}

	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	ownerRef := metav1.NewControllerRef(&service, gvk)

	testCases := []struct {
		name          string
		updateSvc     func(svc v1.Service) v1.Service // given basic valid services, each test case can customize them
		expectedSlice *discovery.EndpointSlice
	}{
		{
			name: "Service without labels",
			updateSvc: func(svc v1.Service) v1.Service {
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName:        service.Name,
						mcsapiv1alpha1.LabelSourceCluster: clusterName,
						discovery.LabelManagedBy:          controllerName,
					},
					GenerateName:    fmt.Sprintf("%s-%s-", service.Name, clusterName),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Service with labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Labels = labels
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName:        service.Name,
						mcsapiv1alpha1.LabelSourceCluster: clusterName,
						discovery.LabelManagedBy:          controllerName,
						"foo":                             "bar",
					},
					GenerateName:    fmt.Sprintf("%s-%s-", service.Name, clusterName),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Headless Service with labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Labels = labels
				svc.Spec.ClusterIP = v1.ClusterIPNone
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName:        service.Name,
						mcsapiv1alpha1.LabelSourceCluster: clusterName,
						discovery.LabelManagedBy:          controllerName,
						v1.IsHeadlessService:              "",
						"foo":                             "bar",
					},
					GenerateName:    fmt.Sprintf("%s-%s-", service.Name, clusterName),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Service with multiple labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar", "foo2": "bar2"}
				svc.Labels = labels
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName:        service.Name,
						mcsapiv1alpha1.LabelSourceCluster: clusterName,
						discovery.LabelManagedBy:          controllerName,
						"foo":                             "bar",
						"foo2":                            "bar2",
					},
					GenerateName:    fmt.Sprintf("%s-%s-", service.Name, clusterName),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Evil service hijacking endpoint slices labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{
					discovery.LabelServiceName:        "bad",
					mcsapiv1alpha1.LabelSourceCluster: clusterName,
					discovery.LabelManagedBy:          "actor",
					"foo":                             "bar",
				}
				svc.Labels = labels
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName:        service.Name,
						mcsapiv1alpha1.LabelSourceCluster: clusterName,
						discovery.LabelManagedBy:          controllerName,
						"foo":                             "bar",
					},
					GenerateName:    fmt.Sprintf("%s-%s-", service.Name, clusterName),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Service with annotations",
			updateSvc: func(svc v1.Service) v1.Service {
				annotations := map[string]string{"foo": "bar"}
				svc.Annotations = annotations
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName:        service.Name,
						mcsapiv1alpha1.LabelSourceCluster: clusterName,
						discovery.LabelManagedBy:          controllerName,
					},
					GenerateName:    fmt.Sprintf("%s-%s-", service.Name, clusterName),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			svc := tc.updateSvc(service)
			generatedSlice := newEndpointSlice(&svc, &endpointMeta, clusterName, controllerName)
			assert.EqualValues(t, tc.expectedSlice, generatedSlice)
		})
	}

}
