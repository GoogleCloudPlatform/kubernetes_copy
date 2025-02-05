/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fuzzer

import (
	fuzz "github.com/google/gofuzz"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/kubernetes/pkg/apis/resource"
)

// Funcs contains the fuzzer functions for the resource group.
//
// Leaving fields empty which then get replaced by the default
// leads to errors during roundtrip tests.
var Funcs = func(codecs runtimeserializer.CodecFactory) []interface{} {
	return []interface{}{
		func(r *resource.DeviceRequest, c fuzz.Continue) {
			c.FuzzNoCustom(r) // fuzz self without calling this function again

			if r.AllocationMode == "" {
				r.AllocationMode = []resource.DeviceAllocationMode{
					resource.DeviceAllocationModeAll,
					resource.DeviceAllocationModeExactCount,
				}[c.Int31n(2)]
			}
		},
		func(r *resource.DeviceAllocationConfiguration, c fuzz.Continue) {
			c.FuzzNoCustom(r)
			if r.Source == "" {
				r.Source = []resource.AllocationConfigSource{
					resource.AllocationConfigSourceClass,
					resource.AllocationConfigSourceClaim,
				}[c.Int31n(2)]
			}
		},
		func(r *resource.OpaqueDeviceConfiguration, c fuzz.Continue) {
			c.FuzzNoCustom(r)
			// Match the fuzzer default content for runtime.Object.
			//
			// This is necessary because randomly generated content
			// might be valid JSON which changes during re-encoding.
			r.Parameters = runtime.RawExtension{Raw: []byte(`{"apiVersion":"unknown.group/unknown","kind":"Something","someKey":"someValue"}`)}
		},
		func(r *resource.AllocatedDeviceStatus, c fuzz.Continue) {
			c.FuzzNoCustom(r)
			// Match the fuzzer default content for runtime.Object.
			//
			// This is necessary because randomly generated content
			// might be valid JSON which changes during re-encoding.
			r.Data = &runtime.RawExtension{Raw: []byte(`{"apiVersion":"unknown.group/unknown","kind":"Something","someKey":"someValue"}`)}
		},
	}
}
