// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package poc

import slim_discovery_v1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"

type ClusterEndpointSlice struct {
	Cluster   string `json:"cluster" protobuf:"bytes,1,opt,name=cluster"`
	ClusterID uint32 `json:"clusterID" protobuf:"varint,2,opt,name=clusterID"`

	Namespace string `json:"namespace" protobuf:"bytes,3,opt,name=namespace"`
	Name      string `json:"name" protobuf:"bytes,4,opt,name=name"`

	Labels      map[string]string `json:"labels,omitempty" protobuf:"bytes,5,rep,name=labels"`
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,6,rep,name=annotations"`

	AddressType slim_discovery_v1.AddressType    `json:"addressType" protobuf:"bytes,7,rep,name=addressType"`
	Endpoints   []slim_discovery_v1.Endpoint     `json:"endpoints" protobuf:"bytes,8,rep,name=endpoints"`
	Ports       []slim_discovery_v1.EndpointPort `json:"ports" protobuf:"bytes,9,rep,name=ports"`
}
