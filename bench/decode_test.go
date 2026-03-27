// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package main

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"google.golang.org/protobuf/proto"

	clustermeshapi "github.com/cilium/cilium/api/v1/clustermesh"
	"github.com/cilium/cilium/pkg/clustermesh/store"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/loadbalancer"
)

func ClusterServiceToBackendParams(service *clustermeshapi.ClusterService) (beps []loadbalancer.BackendParams) {
	for _, endpoint := range service.GetEndpointSlices() {
		portNames := make([]string, 0, len(endpoint.GetPorts()))
		for _, port := range endpoint.GetPorts() {
			portNames = append(portNames, service.GetPortNamesTable()[port.GetNameIndex()])
		}

		for _, backend := range endpoint.GetBackends() {
			if !backend.HasAddress() {
				panic("backend has no v4 address")
			}
			addrCluster := cmtypes.MustAddrClusterFromIP(net.ParseIP(backend.GetAddress()))

			backendZone := &loadbalancer.BackendZone{
				Zone:     service.GetZoneNamesTable()[backend.GetZoneIndex()],
				ForZones: []string{},
			}
			for backendHintIndex := range backend.GetHintsForZonesIndexes() {
				backendZone.ForZones = append(backendZone.ForZones, service.GetZoneNamesTable()[backend.GetHintsForZonesIndexes()[backendHintIndex]])
			}

			for _, port := range endpoint.GetPorts() {
				bep := loadbalancer.BackendParams{
					Address: loadbalancer.NewL3n4Addr(
						// FIXME
						loadbalancer.TCP,
						addrCluster,
						uint16(port.GetPort()),
						loadbalancer.ScopeExternal,
					),
					PortNames: portNames,
					Weight:    loadbalancer.DefaultBackendWeight,
					ClusterID: service.GetClusterId(),
					// TODO: fixme
					State: loadbalancer.BackendStateActive,
					Zone:  backendZone,
				}
				beps = append(beps, bep)
			}
		}
	}
	return
}

func BenchmarkDecoding(b *testing.B) {
	var getBytes func(count int) []byte
	var decode func([]byte)

	mode, _ := os.LookupEnv("MODE")
	mode = strings.ToLower(mode)
	switch mode {
	case "existing_json":
		getBytes = func(count int) []byte {
			return getClusterServiceJSONBytes(getClusterServiceJSON(count))
		}
		decode = func(b []byte) {
			clusterSvc := store.ClusterService{}
			err := clusterSvc.Unmarshal("", b)
			if err != nil {
				panic("unmarshal failed")
			}
			// if len(clustermesh.ClusterServiceToBackendParams(&clusterSvc)) == 0 {
			// 	panic("unexpected number of backends")
			// }
		}
	case "existing_json_zstd":
		getBytes = func(count int) []byte {
			return zstdCompress(getClusterServiceJSONBytes(getClusterServiceJSON(count)))
		}
		decode = func(b []byte) {
			clusterSvc := store.ClusterService{}
			err := clusterSvc.Unmarshal("", zstdDecompressWithPool(b))
			if err != nil {
				panic("unmarshal failed")
			}
			// if len(clustermesh.ClusterServiceToBackendParams(&clusterSvc)) == 0 {
			// 	panic("unexpected number of backends")
			// }
		}
	case "intermediary_json":
		getBytes = func(count int) []byte {
			return getClusterServiceJSONBytes(getClusterServiceIntermediaryJSON(count))
		}
		decode = func(b []byte) {
			clusterSvc := store.ClusterService{}
			err := clusterSvc.Unmarshal("", b)
			if err != nil {
				panic("unmarshal failed")
			}
		}
	case "existing_cbor":
		getBytes = func(count int) []byte {
			return getClusterServiceCBORBytes(getClusterServiceJSON(count))
		}
		decode = func(b []byte) {
			clusterSvc := store.ClusterService{}
			err := cbor.Unmarshal(b, &clusterSvc)
			if err != nil {
				panic("unmarshal failed")
			}
			// if len(clustermesh.ClusterServiceToBackendParams(&clusterSvc)) == 0 {
			// 	panic("unexpected number of backends")
			// }
		}
	case "target_cbor":
		getBytes = func(count int) []byte {
			return getClusterServiceCBORTargetBytes(getClusterServiceIntermediaryJSON(count))
		}
		decode = func(b []byte) {
			clusterSvc := store.ClusterService{}
			err := cbor.Unmarshal(b, &clusterSvc)
			if err != nil {
				panic("unmarshal failed")
			}
		}
	case "target_cbor_zstd":
		getBytes = func(count int) []byte {
			return zstdCompress(getClusterServiceCBORTargetBytes(getClusterServiceIntermediaryJSON(count)))
		}
		decode = func(b []byte) {
			clusterSvc := store.ClusterService{}
			err := cbor.Unmarshal(zstdDecompressWithPool(b), &clusterSvc)
			if err != nil {
				panic("unmarshal failed")
			}
		}
	case "protobuf":
		getBytes = func(count int) []byte {
			return getClusterServiceProtobufBytes(getClusterServiceProtobuf(count))
		}
		decode = func(b []byte) {
			clusterSvc := clustermeshapi.ClusterService{}
			err := proto.Unmarshal(b, &clusterSvc)
			if err != nil {
				panic(err)
			}
			// if len(ClusterServiceToBackendParams(&clusterSvc)) == 0 {
			// 	panic("unexpected number of backends")
			// }
		}
	case "protobuf_zstd":
		getBytes = func(count int) []byte {
			return zstdCompress(getClusterServiceProtobufBytes(getClusterServiceProtobuf(count)))
		}
		decode = func(b []byte) {
			clusterSvc := clustermeshapi.ClusterService{}
			err := proto.Unmarshal(zstdDecompressWithPool(b), &clusterSvc)
			if err != nil {
				panic(err)
			}
			// if len(ClusterServiceToBackendParams(&clusterSvc)) == 0 {
			// 	panic("unexpected number of backends")
			// }
		}
	default:
		panic("unknown MODE, must be one of: existing_json, existing_json_zstd, intermediary_json, existing_cbor, target_cbor, target_cbor_zstd, protobuf, protobuf_zstd")
	}
	for _, count := range []int{1, 10, 100, 1_000, 5_000, 10_000, 50_000} {
		data := getBytes(count)

		b.Run(strconv.Itoa(count)+" backends", func(b *testing.B) {
			for b.Loop() {
				decode(data)
			}
		})
	}
}
