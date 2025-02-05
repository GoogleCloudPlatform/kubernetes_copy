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

package serializer

import (
	"crypto/rand"
	"io/ioutil"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testapigroupv1 "k8s.io/apimachinery/pkg/apis/testapigroup/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
)

func BenchmarkProtobufEncoder(b *testing.B) {
	benchmarkEncodeFor(b, protobuf.NewSerializer(nil, nil, protobuf.SerializerOptions{}))
}

func BenchmarkProtobufEncodeWithAllocator(b *testing.B) {
	benchmarkEncodeWithAllocatorFor(b, protobuf.NewSerializer(nil, nil, protobuf.SerializerOptions{}))
}

func BenchmarkRawProtobufEncoder(b *testing.B) {
	benchmarkEncodeFor(b, protobuf.NewRawSerializer(nil, nil))
}

func BenchmarkRawProtobufEncodeWithAllocator(b *testing.B) {
	benchmarkEncodeWithAllocatorFor(b, protobuf.NewRawSerializer(nil, nil))
}

func benchmarkEncodeFor(b *testing.B, target runtime.Encoder) {
	for _, tc := range benchTestCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				err := target.Encode(tc.obj, ioutil.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkEncodeWithAllocatorFor(b *testing.B, target runtime.EncoderWithAllocator) {
	for _, tc := range benchTestCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			allocator := &runtime.Allocator{}
			for n := 0; n < b.N; n++ {
				err := target.EncodeWithAllocator(tc.obj, ioutil.Discard, allocator)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type benchTestCase struct {
	name string
	obj  runtime.Object
}

func benchTestCases() []benchTestCase {
	return []benchTestCase{
		{
			name: "an obj with 1kB payload",
			obj: func() runtime.Object {
				carpPayload := make([]byte, 1000) // 1 kB
				if _, err := rand.Read(carpPayload); err != nil {
					panic(err)
				}
				return carpWithPayload(carpPayload)
			}(),
		},
		{
			name: "an obj with 10kB payload",
			obj: func() runtime.Object {
				carpPayload := make([]byte, 10000) // 10 kB
				if _, err := rand.Read(carpPayload); err != nil {
					panic(err)
				}
				return carpWithPayload(carpPayload)
			}(),
		},
		{
			name: "an obj with 100kB payload",
			obj: func() runtime.Object {
				carpPayload := make([]byte, 100000) // 100 kB
				if _, err := rand.Read(carpPayload); err != nil {
					panic(err)
				}
				return carpWithPayload(carpPayload)
			}(),
		},
		{
			name: "an obj with 1MB payload",
			obj: func() runtime.Object {
				carpPayload := make([]byte, 1000000) // 1 MB
				if _, err := rand.Read(carpPayload); err != nil {
					panic(err)
				}
				return carpWithPayload(carpPayload)
			}(),
		},
	}
}

func carpWithPayload(carpPayload []byte) *testapigroupv1.Carp {
	gvk := &schema.GroupVersionKind{Group: "group", Version: "version", Kind: "Carp"}
	return &testapigroupv1.Carp{
		TypeMeta: metav1.TypeMeta{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "name",
			Namespace: "namespace",
		},
		Spec: testapigroupv1.CarpSpec{
			Subdomain:    "carp.k8s.io",
			NodeSelector: map[string]string{"payload": string(carpPayload)},
		},
	}
}
