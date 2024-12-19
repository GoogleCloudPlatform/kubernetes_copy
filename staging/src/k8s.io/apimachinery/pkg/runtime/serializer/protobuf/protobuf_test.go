/*
Copyright 2019 The Kubernetes Authors.

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

package protobuf

import (
	"bytes"
	"k8s.io/apimachinery/pkg/util/rand"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	fuzz "github.com/google/gofuzz"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testapigroupv1 "k8s.io/apimachinery/pkg/apis/testapigroup/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	runtimetesting "k8s.io/apimachinery/pkg/runtime/testing"
)

func TestCacheableObject(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "group", Version: "version", Kind: "MockCacheableObject"}
	creater := &mockCreater{obj: &runtimetesting.MockCacheableObject{}}
	typer := &mockTyper{gvk: &gvk}

	encoders := []runtime.Encoder{
		NewSerializer(creater, typer, SerializerOptions{}),
		NewRawSerializer(creater, typer),
	}

	for _, encoder := range encoders {
		runtimetesting.CacheableObjectTest(t, encoder)
	}
}

type mockCreater struct {
	apiVersion string
	kind       string
	err        error
	obj        runtime.Object
}

func (c *mockCreater) New(kind schema.GroupVersionKind) (runtime.Object, error) {
	c.apiVersion, c.kind = kind.GroupVersion().String(), kind.Kind
	return c.obj, c.err
}

type mockTyper struct {
	gvk *schema.GroupVersionKind
	err error
}

func (t *mockTyper) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	if t.gvk == nil {
		return nil, false, t.err
	}
	return []schema.GroupVersionKind{*t.gvk}, false, t.err
}

func (t *mockTyper) Recognizes(_ schema.GroupVersionKind) bool {
	return false
}

func TestSerializerEncodeWithAllocator(t *testing.T) {
	testCases := []struct {
		name string
		obj  runtime.Object
	}{
		{
			name: "encode a bufferedMarshaller obj",
			obj: &testapigroupv1.Carp{
				TypeMeta: metav1.TypeMeta{APIVersion: "group/version", Kind: "Carp"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: testapigroupv1.CarpSpec{
					Subdomain: "carp.k8s.io",
				},
			},
		},

		{
			name: "encode a runtime.Unknown obj",
			obj:  &runtime.Unknown{TypeMeta: runtime.TypeMeta{APIVersion: "group/version", Kind: "Unknown"}, Raw: []byte("hello world")},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			target := NewSerializer(nil, nil, SerializerOptions{})

			writer := &bytes.Buffer{}
			if err := target.Encode(tc.obj, writer); err != nil {
				t.Fatal(err)
			}

			writer2 := &bytes.Buffer{}
			alloc := &testAllocator{}
			if err := target.EncodeWithAllocator(tc.obj, writer2, alloc); err != nil {
				t.Fatal(err)
			}
			if alloc.allocateCount != 1 {
				t.Fatalf("expected the Allocate method to be called exactly 1 but it was executed: %v times ", alloc.allocateCount)
			}

			// to ensure compatibility of the new method with the old one, serialized data must be equal
			// also we are not testing decoding since "roundtripping" is tested elsewhere for all known types
			if !reflect.DeepEqual(writer.Bytes(), writer2.Bytes()) {
				t.Fatal("data mismatch, data serialized with the Encode method is different than serialized with the EncodeWithAllocator method")
			}
		})
	}
}

func TestRawSerializerEncodeWithAllocator(t *testing.T) {
	testCases := []struct {
		name string
		obj  runtime.Object
	}{
		{
			name: "encode a bufferedReverseMarshaller obj",
			obj: &testapigroupv1.Carp{
				TypeMeta: metav1.TypeMeta{APIVersion: "group/version", Kind: "Carp"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: testapigroupv1.CarpSpec{
					Subdomain: "carp.k8s.io",
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			writer := &bytes.Buffer{}
			target := NewRawSerializer(nil, nil)

			if err := target.Encode(tc.obj, writer); err != nil {
				t.Fatal(err)
			}

			writer2 := &bytes.Buffer{}
			alloc := &testAllocator{}
			if err := target.EncodeWithAllocator(tc.obj, writer2, alloc); err != nil {
				t.Fatal(err)
			}
			if alloc.allocateCount != 1 {
				t.Fatalf("expected the Allocate method to be called exactly 1 but it was executed: %v times ", alloc.allocateCount)
			}

			// to ensure compatibility of the new method with the old one, serialized data must be equal
			// also we are not testing decoding since "roundtripping" is tested elsewhere for all known types
			if !reflect.DeepEqual(writer.Bytes(), writer2.Bytes()) {
				t.Fatal("data mismatch, data serialized with the Encode method is different than serialized with the EncodeWithAllocator method")
			}
		})
	}
}

type testAllocator struct {
	buf           []byte
	allocateCount int
}

func (ta *testAllocator) Allocate(n uint64) []byte {
	ta.buf = make([]byte, n)
	ta.allocateCount++
	return ta.buf
}

func TestSerializerList(t *testing.T) {
	var remainingItems int64 = 1
	testCases := []struct {
		name string
		list *testapigroupv1.CarpList
	}{
		{
			name: "empty",
			list: &testapigroupv1.CarpList{
				Items: []testapigroupv1.Carp{},
			},
		},
		{
			name: "just kind",
			list: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind: "List",
				},
			},
		},
		{
			name: "just apiVersion",
			list: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
				},
			},
		},
		{
			name: "no elements",
			list: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind:       "List",
					APIVersion: "v1",
				},
				ListMeta: metav1.ListMeta{
					ResourceVersion: "2345",
				},
			},
		},
		{
			name: "one element with continue",
			list: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind:       "List",
					APIVersion: "v1",
				},
				ListMeta: metav1.ListMeta{
					ResourceVersion:    "2345",
					Continue:           "abc",
					RemainingItemCount: &remainingItems,
				},
				Items: []testapigroupv1.Carp{
					{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"},
						ObjectMeta: metav1.ObjectMeta{
							Name:      "pod",
							Namespace: "default",
						},
					},
				},
			},
		},
		{
			name: "two elements",
			list: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind:       "List",
					APIVersion: "v1",
				},
				ListMeta: metav1.ListMeta{
					ResourceVersion: "2345",
				},
				Items: []testapigroupv1.Carp{
					{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"},
						ObjectMeta: metav1.ObjectMeta{
							Name:      "pod",
							Namespace: "default",
						},
					},
					{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"},
						ObjectMeta: metav1.ObjectMeta{
							Name:      "pod2",
							Namespace: "default2",
						},
					},
				},
			},
		},
		{
			name: "large type meta",
			list: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind: rand.String(1000),
				},
			},
		},
		{
			name: "large list meta",
			list: &testapigroupv1.CarpList{
				ListMeta: metav1.ListMeta{
					ResourceVersion: rand.String(1000),
				},
			},
		},
		{
			name: "large item",
			list: &testapigroupv1.CarpList{
				Items: []testapigroupv1.Carp{
					{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"},
						ObjectMeta: metav1.ObjectMeta{
							Name: rand.String(1000),
						},
					},
					{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"},
						ObjectMeta: metav1.ObjectMeta{
							Name: rand.String(1000),
						},
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("Normal", func(t *testing.T) {
				streamingEncoder := NewSerializer(nil, nil, SerializerOptions{StreamingCollections: true})

				streamingBuffer := &bytes.Buffer{}
				if err := streamingEncoder.Encode(tc.list, streamingBuffer); err != nil {
					t.Fatal(err)
				}

				normalEncoder := NewSerializer(nil, nil, SerializerOptions{StreamingCollections: false})

				normalBuffer := &bytes.Buffer{}
				if err := normalEncoder.Encode(tc.list, normalBuffer); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(streamingBuffer.String(), normalBuffer.String()); diff != "" {
					t.Errorf("unexpected output:\n%s", diff)
				}
			})
		})
	}
}

func TestFuzzCollection(t *testing.T) {
	f := fuzz.New()
	streamingEncoder := NewSerializer(nil, nil, SerializerOptions{StreamingCollections: true})
	streamingBuffer := &bytes.Buffer{}
	normalEncoder := NewSerializer(nil, nil, SerializerOptions{StreamingCollections: false})
	normalBuffer := &bytes.Buffer{}
	for i := 0; i < 10000; i++ {
		list := &testapigroupv1.CarpList{}
		f.Fuzz(list)
		streamingBuffer.Reset()
		normalBuffer.Reset()
		if err := streamingEncoder.Encode(list, streamingBuffer); err != nil {
			t.Fatal(err)
		}
		if err := normalEncoder.Encode(list, normalBuffer); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(streamingBuffer.String(), normalBuffer.String()); diff != "" {
			t.Fatalf("unexpected output:\n%s", diff)
		}
	}
}
