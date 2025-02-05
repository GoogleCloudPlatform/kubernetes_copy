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
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/conversion"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func streamingEncode(obj runtime.Object, w io.Writer) (bool, error) {
	typeMeta, listMeta, items, err := getListMeta(obj)
	if err == nil {
		return true, streamingEncodeList(w, typeMeta, listMeta, items)
	}
	list, ok := obj.(*unstructured.UnstructuredList)
	if ok {
		return true, streamingEncodeUnstructuredList(w, list)
	}
	return false, nil
}

func getListMeta(list runtime.Object) (metav1.TypeMeta, metav1.ListMeta, []runtime.Object, error) {
	listValue, err := conversion.EnforcePtr(list)
	if err != nil {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, err
	}
	listType := listValue.Type()
	if listType.NumField() != 3 {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, fmt.Errorf("expected ListType to have 3 fields")
	}
	// TypeMeta
	if listType.Field(0).Tag.Get("json") != ",inline" {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, fmt.Errorf(`expected TypeMeta json field tag to be ",inline"`)
	}
	typeMeta, ok := listValue.Field(0).Interface().(metav1.TypeMeta)
	if !ok {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, fmt.Errorf("expected TypeMeta field to have TypeMeta type")
	}
	// ListMeta
	if listType.Field(1).Tag.Get("json") != "metadata,omitempty" {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, fmt.Errorf(`expected ListMeta json field tag to be "metadata,omitempty"`)
	}
	listMeta, ok := listValue.Field(1).Interface().(metav1.ListMeta)
	if !ok {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, fmt.Errorf("expected ListMeta field to have ListMeta type")
	}
	// Items
	if listType.Field(2).Tag.Get("json") != "items" {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, fmt.Errorf(`expected Items json field tag to be "items"`)
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return metav1.TypeMeta{}, metav1.ListMeta{}, nil, err
	}
	return typeMeta, listMeta, items, nil
}

func streamingEncodeList(w io.Writer, typeMeta metav1.TypeMeta, listMeta metav1.ListMeta, items []runtime.Object) error {
	// Start
	if _, err := w.Write([]byte(`{`)); err != nil {
		return err
	}

	// TypeMeta
	if typeMeta.Kind != "" {
		if err := encodeKeyValuePair(w, "kind", typeMeta.Kind, []byte(",")); err != nil {
			return err
		}
	}
	if typeMeta.APIVersion != "" {
		if err := encodeKeyValuePair(w, "apiVersion", typeMeta.APIVersion, []byte(",")); err != nil {
			return err
		}
	}

	// ListMeta
	if err := encodeKeyValuePair(w, "metadata", listMeta, []byte(",")); err != nil {
		return err
	}

	// Items
	if err := encodeItemsObjectSlice(w, items); err != nil {
		return err
	}

	// End
	_, err := w.Write([]byte("}\n"))
	return err
}

func encodeItemsObjectSlice(w io.Writer, items []runtime.Object) (err error) {
	if items == nil {
		err := encodeKeyValuePair(w, "items", nil, nil)
		return err
	}
	_, err = w.Write([]byte(`"items":[`))
	if err != nil {
		return err
	}
	suffix := []byte(",")
	for i, item := range items {
		if i == len(items)-1 {
			suffix = nil
		}
		err := encodeValue(w, item, suffix)
		if err != nil {
			return err
		}
	}
	_, err = w.Write([]byte("]"))
	if err != nil {
		return err
	}
	return err
}

func streamingEncodeUnstructuredList(w io.Writer, list *unstructured.UnstructuredList) error {
	_, err := w.Write([]byte(`{`))
	if err != nil {
		return err
	}
	keys := slices.Collect(maps.Keys(list.Object))
	if _, exists := list.Object["items"]; !exists {
		keys = append(keys, "items")
	}
	sort.Strings(keys)

	suffix := []byte(",")
	for i, key := range keys {
		if i == len(keys)-1 {
			suffix = nil
		}
		if key == "items" {
			err = encodeItemsUnstructuredSlice(w, list.Items, suffix)
		} else {
			err = encodeKeyValuePair(w, key, list.Object[key], suffix)
		}
		if err != nil {
			return err
		}
	}
	_, err = w.Write([]byte("}\n"))
	return err
}

func encodeItemsUnstructuredSlice(w io.Writer, items []unstructured.Unstructured, suffix []byte) (err error) {
	_, err = w.Write([]byte(`"items":[`))
	if err != nil {
		return err
	}
	comma := []byte(",")
	for i, item := range items {
		if i == len(items)-1 {
			comma = nil
		}
		err := encodeValue(w, item.Object, comma)
		if err != nil {
			return err
		}
	}
	_, err = w.Write([]byte("]"))
	if err != nil {
		return err
	}
	if len(suffix) > 0 {
		_, err = w.Write(suffix)
	}
	return err
}

func encodeKeyValuePair(w io.Writer, key string, value any, suffix []byte) (err error) {
	err = encodeValue(w, key, []byte(":"))
	if err != nil {
		return err
	}
	err = encodeValue(w, value, suffix)
	if err != nil {
		return err
	}
	return err
}

func encodeValue(w io.Writer, value any, suffix []byte) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	if err != nil {
		return err
	}
	if len(suffix) > 0 {
		_, err = w.Write(suffix)
	}
	return err
}
