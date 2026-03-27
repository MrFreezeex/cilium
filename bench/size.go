// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/cilium/cilium/pkg/k8s"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_discoveryv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/fxamacker/cbor/v2"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	"github.com/cilium/cilium/api/v1/clustermesh"
	"github.com/cilium/cilium/pkg/clustermesh/store"
)

var deterministicCBOREncMode, _ = cbor.CoreDetEncOptions().EncMode()

func getIP(i int) net.IP {
	return net.IPv4(10, byte(i/256/256), byte(i/256%256), byte(i%256))
}

func getEndpoints(backendNum int) []*clustermesh.EndpointSlice {
	endpoints := make([]*clustermesh.EndpointSlice, 0, backendNum/100+1)
	var currEndpoint *clustermesh.EndpointSlice

	for i := 0; i < backendNum; i++ {
		if currEndpoint == nil {
			currEndpoint = &clustermesh.EndpointSlice{}
			currEndpoint.SetEndpointSliceName("my-service-backend-" + rand.String(5))

			port1 := clustermesh.Port{}
			port1.SetProtocol(clustermesh.L4Type_L4_TYPE_TCP)
			port1.SetPort(8080)
			port1.SetNameIndex(0)

			port2 := clustermesh.Port{}
			port2.SetProtocol(clustermesh.L4Type_L4_TYPE_TCP)
			port2.SetPort(8081)
			port2.SetNameIndex(1)

			currEndpoint.SetPorts([]*clustermesh.Port{&port1, &port2})
			currEndpoint.SetBackends(make([]*clustermesh.Backend, 0, 100))
		}

		currEndpoint.SetBackends(append(
			currEndpoint.GetBackends(),
			clustermesh.Backend_builder{
				Address:              ptr.To(getIP(i).String()),
				Conditions:           ptr.To(uint32(k8s.BackendConditionReady | k8s.BackendConditionServing)),
				ZoneIndex:            ptr.To(uint32(i % 3)),
				HintsForZonesIndexes: []uint32{uint32(i % 3)},
			}.Build(),
		))

		if (i+1)%100 == 0 {
			endpoints = append(endpoints, currEndpoint)
			currEndpoint = nil
		}
	}

	if currEndpoint != nil {
		endpoints = append(endpoints, currEndpoint)
	}
	return endpoints
}

func getClusterServiceProtobuf(backendNum int) *clustermesh.ClusterService {
	return clustermesh.ClusterService_builder{
		Cluster:        ptr.To("cluster-1"),
		ClusterId:      ptr.To(uint32(42)),
		Namespace:      ptr.To("default"),
		Name:           ptr.To("my-service-backend"),
		PortNamesTable: []string{"http", "http2"},
		ZoneNamesTable: []string{"zone-1", "zone-2", "zone-3"},
		EndpointSlices: getEndpoints(backendNum),
	}.Build()
}

func getClusterServiceProtobufBytes(clusterSvc *clustermesh.ClusterService) []byte {
	b, err := proto.Marshal(clusterSvc)
	if err != nil {
		panic(err)
	}
	return b
}

func zstdCompress(data []byte) []byte {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		panic(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(data, nil)
}

var decoderPool = sync.Pool{
	New: func() interface{} {
		// The reader passed to NewReader can be nil if we are only using DecodeAll.
		d, _ := zstd.NewReader(nil)
		return d
	},
}

func zstdDecompressWithPool(compressedData []byte) []byte {
	decoder := decoderPool.Get().(*zstd.Decoder)
	defer decoderPool.Put(decoder)

	decompressedData, err := decoder.DecodeAll(compressedData, nil)
	if err != nil {
		panic(err)
	}

	return decompressedData
}

func zstdDecompress(data []byte) []byte {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		panic(err)
	}
	defer decoder.Close()

	b, err := decoder.DecodeAll(data, nil)
	if err != nil {
		panic(err)
	}
	return b
}

func lz4Compress(data []byte) []byte {
	var buf bytes.Buffer
	writer := lz4.NewWriter(&buf)
	writer.Write(data)
	writer.Close()
	return buf.Bytes()
}

func lz4Decompress(data []byte) []byte {
	var buf bytes.Buffer

	reader := lz4.NewReader(bytes.NewReader(data))
	_, err := buf.ReadFrom(reader)
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func lz4CompressFast(originalData []byte) []byte {
	// Allocate a buffer to hold the compressed data.
	// The lz4.CompressBlockBound returns the maximum size the compressed data can be.
	compressedData := make([]byte, lz4.CompressBlockBound(len(originalData)))

	// Compress the data.
	n, err := lz4.CompressBlock(originalData, compressedData, nil)
	if err != nil {
		panic(err)
	}
	if n == 0 {
		// Handle case where data is incompressible, lz4 might return 0.
		// You might want to store the original data directly with a flag.
		// For this example, we'll proceed, but be aware of this.
	}

	// Create a final buffer to store everything.
	// We use 4 bytes (uint32) for the original size.
	var buf bytes.Buffer
	sizeBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeBytes, uint32(len(originalData)))

	// Write the original size, then the compressed data.
	buf.Write(sizeBytes)
	buf.Write(compressedData[:n]) // Write only the valid compressed part

	return buf.Bytes()
}

func lz4DecompressFast(dataWithHeader []byte) []byte {
	if len(dataWithHeader) < 4 {
		panic(fmt.Errorf("invalid data: too short to contain size header"))
	}

	// 1. Read the original size from the first 4 bytes.
	originalSize := binary.BigEndian.Uint32(dataWithHeader[:4])

	// 2. Create the destination buffer of the *exact* required size.
	decompressed := make([]byte, originalSize)

	// 3. Decompress directly into the buffer.
	// The compressed data is everything *after* the 4-byte header.
	compressedData := dataWithHeader[4:]
	n, err := lz4.UncompressBlock(compressedData, decompressed)
	if err != nil {
		panic(err)
	}

	if n != int(originalSize) {
		panic(fmt.Errorf("decompression size mismatch: expected %d, got %d", originalSize, n))
	}

	return decompressed
}

const jsonPortCount = 2

func getBackends(backendNum int) map[string]store.PortConfiguration {
	ports := store.PortConfiguration{}
	for i := 0; i < jsonPortCount; i++ {
		ports["port"+strconv.Itoa(i)] = &loadbalancer.L4Addr{
			Protocol: loadbalancer.TCP,
			Port:     uint16(8080 + i),
		}
	}

	backends := make(map[string]store.PortConfiguration, backendNum)
	for i := 0; i < backendNum; i++ {
		backends[getIP(i).String()] = ports
	}
	return backends
}

func getSlimEndpointSlices(backendNum int) []slim_discoveryv1.EndpointSlice {
	endpointSlices := make([]slim_discoveryv1.EndpointSlice, 0, backendNum/100+1)
	protocol := slim_corev1.ProtocolTCP
	port8080 := int32(8080)
	port8081 := int32(8081)
	portName0 := "port0"
	portName1 := "port1"

	var current *slim_discoveryv1.EndpointSlice
	for i := 0; i < backendNum; i++ {
		if current == nil {
			current = &slim_discoveryv1.EndpointSlice{
				AddressType: slim_discoveryv1.AddressTypeIPv4,
				Ports: []slim_discoveryv1.EndpointPort{
					{Name: &portName0, Protocol: &protocol, Port: &port8080},
					{Name: &portName1, Protocol: &protocol, Port: &port8081},
				},
				Endpoints: make([]slim_discoveryv1.Endpoint, 0, 100),
			}
		}

		zone := "zone-" + strconv.Itoa(i%3+1)
		current.Endpoints = append(current.Endpoints, slim_discoveryv1.Endpoint{
			Addresses: []string{getIP(i).String()},
			Zone:      &zone,
		})

		if (i+1)%100 == 0 {
			endpointSlices = append(endpointSlices, *current)
			current = nil
		}
	}

	if current != nil {
		endpointSlices = append(endpointSlices, *current)
	}

	return endpointSlices
}

func getClusterServiceJSON(backendNum int) *store.ClusterService {
	return &store.ClusterService{
		Cluster:   "cluster-1",
		Namespace: "default",
		Name:      "my-service-backend",
		Frontends: map[string]store.PortConfiguration{"10.42.42.42": {
			"http": &loadbalancer.L4Addr{
				Protocol: loadbalancer.TCP,
				Port:     80,
			},
		}},
		Backends:        getBackends(backendNum),
		ClusterID:       42,
		Labels:          map[string]string{"name": "my-service-backend"},
		Selector:        map[string]string{"name": "my-service-backend"},
		IncludeExternal: false,
		Shared:          false,
	}
}

func getClusterServiceIntermediaryJSON(backendNum int) *store.ClusterService {
	clusterSvc := getClusterServiceJSON(backendNum)
	clusterSvc.EndpointSlices = getSlimEndpointSlices(backendNum)
	return clusterSvc
}

func getClusterServiceJSONZones(backendNum int) map[string]store.BackendZone {
	zones := make(map[string]store.BackendZone, backendNum)
	for i := 0; i < backendNum; i++ {
		zones[getIP(i).String()] = store.BackendZone{
			Zone:     "eu-west-1",
			ForZones: []store.ForZone{{Name: "eu-west-1"}},
		}
	}
	return zones
}

func getClusterServiceJSONBytes(clusterSvc *store.ClusterService) []byte {
	b, err := clusterSvc.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

func getClusterServiceCBORBytes(clusterSvc *store.ClusterService) []byte {
	b, err := deterministicCBOREncMode.Marshal(clusterSvc)
	if err != nil {
		panic(err)
	}
	return b
}

func getClusterServiceCBORTargetBytes(clusterSvc *store.ClusterService) []byte {
	target := clusterSvc.DeepCopy()
	target.Backends = nil
	target.Zones = nil
	target.Hostnames = nil

	b, err := deterministicCBOREncMode.Marshal(target)
	if err != nil {
		panic(err)
	}
	return b
}

func getPrettySize(bytes []byte) string {
	size := len(bytes)
	switch {
	case size > (1024 * 1024):
		return fmt.Sprintf("%.2fMiB", float64(size)/(1024*1024))
	case size > 1024:
		return fmt.Sprintf("%.2fKiB", float64(size)/1024)
	default:
		return fmt.Sprintf("%dB", size)
	}
}

func main() {
	// protoJSON, _ := getClusterServiceProtobuf(10).MarshalJSON()
	// fmt.Println(string(protoJSON))

	fmt.Println("| Backend Count | Existing JSON | Intermediary JSON | Existing CBOR | Target CBOR | Target CBOR zstd | Protobuf  | Protobuf zstd |")
	fmt.Println("| ------------- | ------------- | ----------------- | ------------- | ----------- | ---------------- | --------- | ------------- |")
	for _, count := range []int{1, 10, 100, 1_000, 5_000, 10_000, 50_000, 100_000, 150_000} {
		fmt.Printf("| %13d |", count)
		existingJSON := getClusterServiceJSON(count)
		existingJSON.Zones = getClusterServiceJSONZones(count)
		fmt.Printf(" %13s |", getPrettySize(getClusterServiceJSONBytes(existingJSON)))

		intermediaryJSON := getClusterServiceIntermediaryJSON(count)
		intermediaryJSON.Zones = getClusterServiceJSONZones(count)
		fmt.Printf(" %17s |", getPrettySize(getClusterServiceJSONBytes(intermediaryJSON)))
		fmt.Printf(" %13s |", getPrettySize(getClusterServiceCBORBytes(existingJSON)))
		fmt.Printf(" %11s |", getPrettySize(getClusterServiceCBORTargetBytes(intermediaryJSON)))
		fmt.Printf(" %16s |", getPrettySize(zstdCompress(getClusterServiceCBORTargetBytes(intermediaryJSON))))

		protoBytes := getClusterServiceProtobufBytes(getClusterServiceProtobuf(count))
		fmt.Printf(" %9s |", getPrettySize(protoBytes))
		fmt.Printf(" %13s |", getPrettySize(zstdCompress(protoBytes)))

		fmt.Println()
	}
}
