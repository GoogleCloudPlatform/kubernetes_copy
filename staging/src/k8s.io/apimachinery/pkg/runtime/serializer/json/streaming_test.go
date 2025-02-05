/*
Copyright 2025 The Kubernetes Authors.

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

package json

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	fuzz "github.com/google/gofuzz"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	testapigroupv1 "k8s.io/apimachinery/pkg/apis/testapigroup/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestStreamingCollectionsEncoding(t *testing.T) {
	var streamingBuffer bytes.Buffer
	var normalBuffer bytes.Buffer
	normalSerializer := NewSerializerWithOptions(DefaultMetaFactory, nil, nil, SerializerOptions{StreamingCollectionsEncoding: false})
	var remainingItems int64 = 1
	for _, tc := range []struct {
		name       string
		in         runtime.Object
		exactMatch bool
	}{
		{
			name:       "List empty",
			exactMatch: true,
			in:         &testapigroupv1.CarpList{},
		},
		{
			name:       "List just kind",
			exactMatch: true,
			in: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind: "List",
				},
			},
		},
		{
			name:       "List just apiVersion",
			exactMatch: true,
			in: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
				},
			},
		},
		{
			name:       "List no elements",
			exactMatch: true,
			in: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind:       "List",
					APIVersion: "v1",
				},
				ListMeta: metav1.ListMeta{
					ResourceVersion: "2345",
				},
				Items: []testapigroupv1.Carp{},
			},
		},
		{
			name:       "List one element with continue",
			exactMatch: true,
			in: &testapigroupv1.CarpList{
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
					{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"}, ObjectMeta: metav1.ObjectMeta{
						Name:      "pod",
						Namespace: "default",
					}},
				},
			},
		},
		{
			name:       "List two elements",
			exactMatch: true,
			in: &testapigroupv1.CarpList{
				TypeMeta: metav1.TypeMeta{
					Kind:       "List",
					APIVersion: "v1",
				},
				ListMeta: metav1.ListMeta{
					ResourceVersion: "2345",
				},
				Items: []testapigroupv1.Carp{
					{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"}, ObjectMeta: metav1.ObjectMeta{
						Name:      "pod",
						Namespace: "default",
					}},
					{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Carp"}, ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default2",
					}},
				},
			},
		},
		{
			name:       "UnstructuredList empty",
			exactMatch: true,
			in:         &unstructured.UnstructuredList{},
		},
		{
			name: "UnstructuredList just kind",
			in: &unstructured.UnstructuredList{
				Object: map[string]interface{}{"kind": "List"},
			},
		},
		{
			name: "UnstructuredList just apiVersion",
			in: &unstructured.UnstructuredList{
				Object: map[string]interface{}{"apiVersion": "v1"},
			},
		},
		{
			name: "UnstructuredList no elements",
			in: &unstructured.UnstructuredList{
				Object: map[string]interface{}{"kind": "List", "apiVersion": "v1", "metadata": map[string]interface{}{"resourceVersion": "2345"}},
				Items:  []unstructured.Unstructured{},
			},
		},
		{
			name: "UnstructuredList one element with continue",
			in: &unstructured.UnstructuredList{
				Object: map[string]interface{}{"kind": "List", "apiVersion": "v1", "metadata": map[string]interface{}{
					"resourceVersion":    "2345",
					"continue":           "abc",
					"remainingItemCount": "1",
				}},
				Items: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "Carp",
							"metadata": map[string]interface{}{
								"name":      "pod",
								"namespace": "default",
							},
						},
					},
				},
			},
		},
		{
			name: "UnstructuredList two elements",
			in: &unstructured.UnstructuredList{
				Object: map[string]interface{}{"kind": "List", "apiVersion": "v1", "metadata": map[string]interface{}{
					"resourceVersion": "2345",
				}},
				Items: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "Carp",
							"metadata": map[string]interface{}{
								"name":      "pod",
								"namespace": "default",
							},
						},
					},
					{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "Carp",
							"metadata": map[string]interface{}{
								"name":      "pod2",
								"namespace": "default",
							},
						},
					},
				},
			},
		},
		{
			name:       "UnstructuredList conflict on items",
			exactMatch: true,
			in: &unstructured.UnstructuredList{
				Object: map[string]interface{}{"items": []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"name": "pod",
						},
					},
				},
				},
				Items: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"name": "pod2",
						},
					},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			normalBuffer.Reset()
			streamingBuffer.Reset()
			ok, err := streamingEncode(tc.in, &streamingBuffer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ok {
				_, _, _, err := getListMeta(tc.in)
				t.Errorf("getListMeta err: %s", err)
				t.Fatalf("expected streaming encoder to encode %T", tc.in)
			}
			if err := normalSerializer.Encode(tc.in, &normalBuffer); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			t.Logf("streaming: %s", streamingBuffer.String())
			t.Logf("normal: %s", normalBuffer.String())
			if diff := cmp.Diff(normalBuffer.String(), streamingBuffer.String()); diff != "" {
				t.Errorf("not matching:\n%s", diff)
			}
		})
	}
}

func TestFuzzStreamingCollectionsEncoding(t *testing.T) {
	disableFuzzFieldsV1 := func(field *metav1.FieldsV1, c fuzz.Continue) {}
	fuzzUnstructuredList := func(list *unstructured.UnstructuredList, c fuzz.Continue) {
		list.Object = map[string]interface{}{
			"kind":         "List",
			"apiVersion":   "v1",
			c.RandString(): c.RandString(),
			c.RandString(): c.RandUint64(),
			c.RandString(): c.RandBool(),
			"metadata": map[string]interface{}{
				"resourceVersion":    fmt.Sprintf("%d", c.RandUint64()),
				"continue":           c.RandString(),
				"remainingItemCount": fmt.Sprintf("%d", c.RandUint64()),
				c.RandString():       c.RandString(),
			}}
		c.Fuzz(&list.Items)
	}
	fuzzMap := func(kvs map[string]interface{}, c fuzz.Continue) {
		kvs[c.RandString()] = c.RandBool()
		kvs[c.RandString()] = c.RandUint64()
		kvs[c.RandString()] = c.RandString()
	}
	f := fuzz.New().Funcs(disableFuzzFieldsV1, fuzzUnstructuredList, fuzzMap)
	streamingBuffer := &bytes.Buffer{}
	normalSerializer := NewSerializerWithOptions(DefaultMetaFactory, nil, nil, SerializerOptions{StreamingCollectionsEncoding: false})
	normalBuffer := &bytes.Buffer{}
	t.Run("CarpList", func(t *testing.T) {
		for i := 0; i < 1000; i++ {
			list := &testapigroupv1.CarpList{}
			f.Fuzz(list)
			streamingBuffer.Reset()
			normalBuffer.Reset()
			ok, err := streamingEncode(list, streamingBuffer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ok {
				t.Fatalf("expected streaming encoder to encode %T", list)
			}
			if err := normalSerializer.Encode(list, normalBuffer); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(normalBuffer.String(), streamingBuffer.String()); diff != "" {
				t.Logf("normal: %s", normalBuffer.String())
				t.Logf("streaming: %s", streamingBuffer.String())
				t.Errorf("not matching:\n%s", diff)
			}
		}
	})
	t.Run("UnstructuredList", func(t *testing.T) {
		for i := 0; i < 1000; i++ {
			list := &unstructured.UnstructuredList{}
			f.Fuzz(list)
			streamingBuffer.Reset()
			normalBuffer.Reset()
			ok, err := streamingEncode(list, streamingBuffer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ok {
				t.Fatalf("expected streaming encoder to encode %T", list)
			}
			if err := normalSerializer.Encode(list, normalBuffer); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(normalBuffer.String(), streamingBuffer.String()); diff != "" {
				t.Logf("normal: %s", normalBuffer.String())
				t.Logf("streaming: %s", streamingBuffer.String())
				t.Errorf("not matching:\n%s", diff)
			}
		}
	})
}
