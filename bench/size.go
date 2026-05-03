// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/fxamacker/cbor/v2"
	"github.com/klauspost/compress/zstd"

	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_discoveryv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"
)

const endpointCount = 100

var deterministicCBOREncMode, _ = cbor.CoreDetEncOptions().EncMode()

var decoderPool = sync.Pool{
	New: func() interface{} {
		d, _ := zstd.NewReader(nil)
		return d
	},
}

func getIP(i int) net.IP {
	return net.IPv4(10, byte(i/256/256), byte(i/256%256), byte(i%256))
}

func getEndpointSlice() *slim_discoveryv1.EndpointSlice {
	endpointSlice := &slim_discoveryv1.EndpointSlice{
		AddressType: slim_discoveryv1.AddressTypeIPv4,
		Ports: []slim_discoveryv1.EndpointPort{
			{Name: new("port0"), Protocol: new(slim_corev1.ProtocolTCP), Port: new(int32(8080))},
			{Name: new("port1"), Protocol: new(slim_corev1.ProtocolTCP), Port: new(int32(8081))},
		},
		Endpoints: make([]slim_discoveryv1.Endpoint, 0, endpointCount),
	}

	for i := 0; i < endpointCount; i++ {
		endpointSlice.Endpoints = append(endpointSlice.Endpoints, slim_discoveryv1.Endpoint{
			Addresses: []string{getIP(i).String()},
			Conditions: slim_discoveryv1.EndpointConditions{
				Ready:       new(true),
				Serving:     new(true),
				Terminating: new(false),
			},
			Zone: new("zone-" + fmt.Sprint(i%3+1)),
		})
	}

	return endpointSlice
}

func getEndpointSliceJSONBytes(endpointSlice *slim_discoveryv1.EndpointSlice) []byte {
	b, err := json.Marshal(endpointSlice)
	if err != nil {
		panic(err)
	}
	return b
}

func getEndpointSliceCBORBytes(endpointSlice *slim_discoveryv1.EndpointSlice) []byte {
	b, err := deterministicCBOREncMode.Marshal(endpointSlice)
	if err != nil {
		panic(err)
	}
	return b
}

func getEndpointSliceProtobufBytes(endpointSlice *slim_discoveryv1.EndpointSlice) []byte {
	b, err := endpointSlice.Marshal()
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

func zstdDecompressWithPool(compressedData []byte) []byte {
	decoder := decoderPool.Get().(*zstd.Decoder)
	defer decoderPool.Put(decoder)

	decompressedData, err := decoder.DecodeAll(compressedData, nil)
	if err != nil {
		panic(err)
	}

	return decompressedData
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
	endpointSlice := getEndpointSlice()
	jsonBytes := getEndpointSliceJSONBytes(endpointSlice)
	cborBytes := getEndpointSliceCBORBytes(endpointSlice)
	protoBytes := getEndpointSliceProtobufBytes(endpointSlice)

	fmt.Println("| Format   | Raw      | zstd     |")
	fmt.Println("| -------- | -------- | -------- |")
	fmt.Printf("| JSON     | %8s | %8s |\n", getPrettySize(jsonBytes), getPrettySize(zstdCompress(jsonBytes)))
	fmt.Printf("| CBOR     | %8s | %8s |\n", getPrettySize(cborBytes), getPrettySize(zstdCompress(cborBytes)))
	fmt.Printf("| Protobuf | %8s | %8s |\n", getPrettySize(protoBytes), getPrettySize(zstdCompress(protoBytes)))
}
