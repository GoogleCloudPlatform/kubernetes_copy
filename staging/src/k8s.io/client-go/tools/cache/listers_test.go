/*
Copyright 2024 The Kubernetes Authors.

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

package cache

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
)

func TestListersListAll(t *testing.T) {
	mkObj := func(id string, val string) testStoreObject {
		return testStoreObject{id: id, val: val}
	}

	store := NewStore(testStoreKeyFunc)

	err := store.Add(mkObj("foo", "bar"))
	if err != nil {
		t.Errorf("store obj add failed")
	}

	err = store.Add(mkObj("foo-1", "bar-1"))
	if err != nil {
		t.Errorf("store obj add failed")
	}

	expectedOutput := []testStoreObject{
		mkObj("foo", "bar"),
		mkObj("foo-1", "bar-1"),
	}
	actualOutput := []testStoreObject{}

	err = ListAll(store, labels.Everything(), func(obj interface{}) {
		actualOutput = append(actualOutput, obj.(testStoreObject))
	})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(expectedOutput, actualOutput) {
		t.Fatalf("unexpected object has been returned expected = %v actual = %v, diff = %v", expectedOutput, actualOutput, cmp.Diff(expectedOutput, actualOutput))
	}
}

func TestListersListAllByNamespace(t *testing.T) {
	objs := []*unstructured.Unstructured{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo1"),
		newUnstructured("group/version", "TheKind", "ns-bar", "name-bar"),
	}
	indexer := NewIndexer(MetaNamespaceKeyFunc, Indexers{})
	for _, obj := range objs {
		err := indexer.Add(obj)
		if err != nil {
			t.Fatal(err)
		}
	}

	expectedOutput := []*unstructured.Unstructured{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo1"),
	}
	namespaceToList := "ns-foo"

	actualOutput := []*unstructured.Unstructured{}
	appendFn := func(obj interface{}) {
		actualOutput = append(actualOutput, obj.(*unstructured.Unstructured))
	}

	err := ListAllByNamespace(indexer, namespaceToList, labels.Everything(), appendFn)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expectedOutput, actualOutput) {
		t.Fatalf("unexpected object has been returned expected = %v actual = %v, diff = %v", expectedOutput, actualOutput, cmp.Diff(expectedOutput, actualOutput))
	}
}

func TestGenericListerListMethod(t *testing.T) {
	objs := []*unstructured.Unstructured{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo1"),
		newUnstructured("group/version", "TheKind", "ns-bar", "name-bar"),
	}
	indexer := NewIndexer(MetaNamespaceKeyFunc, Indexers{})
	for _, obj := range objs {
		err := indexer.Add(obj)
		if err != nil {
			t.Fatal(err)
		}
	}
	target := NewGenericLister(indexer, schema.GroupResource{Group: "group", Resource: "resource"})

	expectedOutput := []runtime.Object{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo1"),
		newUnstructured("group/version", "TheKind", "ns-bar", "name-bar"),
	}
	actualOutput, err := target.List(labels.Everything())

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expectedOutput, actualOutput) {
		t.Fatalf("unexpected object has been returned expected = %v actual = %v, diff = %v", expectedOutput, actualOutput, cmp.Diff(expectedOutput, actualOutput))
	}
}

func TestGenericListerByNamespaceMethod(t *testing.T) {
	objs := []*unstructured.Unstructured{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		newUnstructured("group/version", "TheKind", "ns-faa", "name-faa1"),
		newUnstructured("group/version", "TheKind", "ns-bar", "name-bar"),
	}
	indexer := NewIndexer(MetaNamespaceKeyFunc, Indexers{})
	for _, obj := range objs {
		err := indexer.Add(obj)
		if err != nil {
			t.Fatal(err)
		}
	}
	target := NewGenericLister(indexer, schema.GroupResource{Group: "group", Resource: "resource"})

	expectedOutput := []runtime.Object{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
	}
	namespaceToList := "ns-foo"
	actualOutput, err := target.ByNamespace(namespaceToList).List(labels.Everything())

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expectedOutput, actualOutput) {
		t.Fatalf("unexpected object has been returned expected = %v actual = %v, diff = %v", expectedOutput, actualOutput, cmp.Diff(expectedOutput, actualOutput))
	}
}

func TestGenericListerGetMethod(t *testing.T) {
	objs := []*unstructured.Unstructured{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		newUnstructured("group/version", "TheKind", "ns-faa", "name-faa1"),
		newUnstructured("group/version", "TheKind", "ns-bar", "name-bar"),
	}
	indexer := NewIndexer(MetaNamespaceKeyFunc, Indexers{})
	for _, obj := range objs {
		err := indexer.Add(obj)
		if err != nil {
			t.Fatal(err)
		}
	}
	target := NewGenericLister(indexer, schema.GroupResource{Group: "group", Resource: "resource"})

	expectedOutput := []runtime.Object{
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
	}[0]
	key := "ns-foo/name-foo"
	actualOutput, err := target.Get(key)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expectedOutput, actualOutput) {
		t.Fatalf("unexpected object has been returned expected = %v actual = %v, diff = %v", expectedOutput, actualOutput, cmp.Diff(expectedOutput, actualOutput))
	}
}

func newUnstructured(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]interface{}{
				"namespace": namespace,
				"name":      name,
			},
		},
	}
}
