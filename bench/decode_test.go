// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"

	slim_discoveryv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"
)

func BenchmarkDecoding(b *testing.B) {
	var data []byte
	var decode func([]byte)

	endpointSlice := getEndpointSlice()
	mode, _ := os.LookupEnv("MODE")
	mode = strings.ToLower(mode)

	switch mode {
	case "json":
		data = getEndpointSliceJSONBytes(endpointSlice)
		decode = func(b []byte) {
			endpointSlice := slim_discoveryv1.EndpointSlice{}
			if err := json.Unmarshal(b, &endpointSlice); err != nil {
				panic("unmarshal failed")
			}
		}
	case "json_zstd":
		data = zstdCompress(getEndpointSliceJSONBytes(endpointSlice))
		decode = func(b []byte) {
			endpointSlice := slim_discoveryv1.EndpointSlice{}
			if err := json.Unmarshal(zstdDecompressWithPool(b), &endpointSlice); err != nil {
				panic("unmarshal failed")
			}
		}
	case "cbor":
		data = getEndpointSliceCBORBytes(endpointSlice)
		decode = func(b []byte) {
			endpointSlice := slim_discoveryv1.EndpointSlice{}
			if err := cbor.Unmarshal(b, &endpointSlice); err != nil {
				panic("unmarshal failed")
			}
		}
	case "cbor_zstd":
		data = zstdCompress(getEndpointSliceCBORBytes(endpointSlice))
		decode = func(b []byte) {
			endpointSlice := slim_discoveryv1.EndpointSlice{}
			if err := cbor.Unmarshal(zstdDecompressWithPool(b), &endpointSlice); err != nil {
				panic("unmarshal failed")
			}
		}
	case "protobuf":
		data = getEndpointSliceProtobufBytes(endpointSlice)
		decode = func(b []byte) {
			endpointSlice := slim_discoveryv1.EndpointSlice{}
			if err := endpointSlice.Unmarshal(b); err != nil {
				panic("unmarshal failed")
			}
		}
	case "protobuf_zstd":
		data = zstdCompress(getEndpointSliceProtobufBytes(endpointSlice))
		decode = func(b []byte) {
			endpointSlice := slim_discoveryv1.EndpointSlice{}
			if err := endpointSlice.Unmarshal(zstdDecompressWithPool(b)); err != nil {
				panic("unmarshal failed")
			}
		}
	default:
		panic("unknown MODE, must be one of: json, json_zstd, cbor, cbor_zstd, protobuf, protobuf_zstd")
	}

	b.Run("endpoint_slice", func(b *testing.B) {
		for b.Loop() {
			decode(data)
		}
	})
}
