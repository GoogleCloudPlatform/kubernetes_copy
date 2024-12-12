/*
Copyright 2018 The Kubernetes Authors.

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

package table

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
)

// MetaToTableRow converts a list or object into one or more table rows. The provided rowFn is invoked for
// each accessed item, with name and age being passed to each.
func MetaToTableRow(obj runtime.Object, rowFn func(obj runtime.Object, m metav1.Object, name, age string) ([]interface{}, error)) ([]metav1.TableRow, error) {
	if meta.IsListType(obj) {
		rows := make([]metav1.TableRow, 0, 16)
		err := meta.EachListItem(obj, func(obj runtime.Object) error {
			nestedRows, err := MetaToTableRow(obj, rowFn)
			if err != nil {
				return err
			}
			rows = append(rows, nestedRows...)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return rows, nil
	}

	rows := make([]metav1.TableRow, 0, 1)
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	row := metav1.TableRow{
		Object: runtime.RawExtension{Object: obj},
	}
	row.Cells, err = rowFn(obj, m, m.GetName(), ConvertToHumanReadableDateType(m.GetCreationTimestamp()))
	if err != nil {
		return nil, err
	}
	rows = append(rows, row)
	return rows, nil
}

// ConvertToHumanReadableDateType returns the elapsed time since timestamp in
// human-readable approximation.
func ConvertToHumanReadableDateType(timestamp metav1.Time) string {
	if timestamp.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(timestamp.Time))
}

// IsTable returns if the provided Object is a Table
func IsTable(obj runtime.Object) bool {
	visitUnstructured := func(gvk schema.GroupVersionKind) bool {
		if gvk.Kind != "Table" {
			return false
		}

		return gvk.GroupVersion() == metav1.SchemeGroupVersion || gvk.GroupVersion() == metav1beta1.SchemeGroupVersion
	}

	switch t := obj.(type) {
	case *metav1.Table: // metav1beta1.Table is an alias of metav1.Table
		return true
	case *unstructured.UnstructuredList:
		return visitUnstructured(t.GroupVersionKind())
	case *unstructured.Unstructured:
		return visitUnstructured(t.GroupVersionKind())
	default:
		return false
	}
}

// SetTableRows set the provided rows in to given Table object.
func SetTableRows(obj runtime.Object, rows []any) error {
	switch t := obj.(type) {
	case *metav1.Table:
		var err error
		t.Rows, err = convertToSlice[metav1.TableRow](rows)
		if err != nil {
			return err
		}
	case *unstructured.UnstructuredList:
		t.Object["rows"] = rows
	case *unstructured.Unstructured:
		t.Object["rows"] = rows
	default:
		return fmt.Errorf("unsupported type %T", obj)
	}

	return nil
}

// EachTableRow invokes fn on each row in the Table. Any error immediately terminates the loop.
func EachTableRow(obj runtime.Object, fn func(any) error) error {
	visitUnstructured := func(object map[string]any) error {
		items, ok := object["rows"]
		if !ok {
			return nil
		}

		rows, ok := items.([]any)
		if !ok {
			return errors.New("content is not a table")
		}
		for i := range rows {
			if err := fn(rows[i]); err != nil {
				return err
			}
		}

		return nil
	}

	switch t := obj.(type) {
	case *metav1.Table:
		for i := range t.Rows {
			if err := fn(t.Rows[i]); err != nil {
				return err
			}
		}
	case *unstructured.UnstructuredList:
		return visitUnstructured(t.Object)
	case *unstructured.Unstructured:
		return visitUnstructured(t.Object)
	default:
		return fmt.Errorf("unsupported type %T", obj)
	}

	return nil
}

// EachTableRowObject invokes fn on runtime.Object embed in each row of the Table.
// Any error immediately terminates the loop.
func EachTableRowObject(obj runtime.Object, fn func(object runtime.Object) error) error {
	return EachTableRow(obj, func(row any) error {
		switch t := row.(type) {
		case metav1.TableRow:
			if t.Object.Object != nil {
				return fn(t.Object.Object)
			}

			return fn(&runtime.Unknown{Raw: t.Object.Raw})
		case map[string]any:
			var innerObject = &unstructured.Unstructured{}

			if obj, ok := t["object"]; ok {
				var found bool
				innerObject.Object, found = obj.(map[string]any)
				if !found {
					return fmt.Errorf("unexpected object type %T", obj)
				}
			}

			return fn(innerObject)
		default:
			return fmt.Errorf("unsupported type %T", row)
		}
	})
}

// GetColumnDefinitions returns columnDefinitions of the given Table.
func GetColumnDefinitions(obj runtime.Object) (any, error) {
	switch t := obj.(type) {
	case *metav1.Table:
		return t.ColumnDefinitions, nil
	case *unstructured.UnstructuredList:
		return t.Object["columnDefinitions"], nil
	case *unstructured.Unstructured:
		return t.Object["columnDefinitions"], nil
	default:
		return nil, fmt.Errorf("unsupported type %T", obj)
	}
}

// SetColumnDefinitions set the passed columnDefinitions to the given Object.
func SetColumnDefinitions(obj runtime.Object, columnDefinitions any) error {
	switch t := obj.(type) {
	case *metav1.Table:
		var err error
		t.ColumnDefinitions, err = convertToSlice[metav1.TableColumnDefinition](columnDefinitions)
		if err != nil {
			return fmt.Errorf("unexpected table column definitions type %T", columnDefinitions)
		}
	case *unstructured.Unstructured:
		t.Object["columnDefinitions"] = columnDefinitions
	case *unstructured.UnstructuredList:
		t.Object["columnDefinitions"] = columnDefinitions
	default:
		return fmt.Errorf("unsupported type %T", obj)
	}

	return nil
}

func convertToSlice[T any](in any) ([]T, error) {
	switch v := in.(type) {
	case nil:
		return nil, nil
	case []T:
		return v, nil
	case []any:
		out := make([]T, len(v))
		for i := range v {
			var ok bool
			out[i], ok = v[i].(T)
			if !ok {
				return nil, fmt.Errorf("unexpected convert %T to %T", v[i], new(T))
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported type %T", in)
	}
}
