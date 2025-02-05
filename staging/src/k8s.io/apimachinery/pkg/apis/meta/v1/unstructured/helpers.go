/*
Copyright 2015 The Kubernetes Authors.

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

package unstructured

import (
	gojson "encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/klog/v2"
)

// NestedFieldCopy returns a deep copy of the value of a nested field.
// Returns false if the value is missing.
// No error is returned for a nil field.
//
// Note: fields passed to this function are treated as keys within the passed
// object; no array/slice syntax is supported.
func NestedFieldCopy(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	return runtime.DeepCopyJSONValue(val), true, nil
}

// NestedFieldNoCopy returns a reference to a nested field.
// Returns false if value is not found and an error if unable
// to traverse obj.
//
// Note: fields passed to this function are treated as keys within the passed
// object; no array/slice syntax is supported.
func NestedFieldNoCopy(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
	var val interface{} = obj

	for i, field := range fields {
		if val == nil {
			return nil, false, nil
		}
		if m, ok := val.(map[string]interface{}); ok {
			val, ok = m[field]
			if !ok {
				return nil, false, nil
			}
		} else {
			return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected map[string]interface{}", jsonPath(fields[:i+1]), val, val)
		}
	}
	return val, true, nil
}

// NestedString returns the string value of a nested field.
// Returns false if value is not found and an error if not a string.
func NestedString(obj map[string]interface{}, fields ...string) (string, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return "", found, err
	}
	s, ok := val.(string)
	if !ok {
		return "", false, fmt.Errorf("%v accessor error: %v is of the type %T, expected string", jsonPath(fields), val, val)
	}
	return s, true, nil
}

// NestedBool returns the bool value of a nested field.
// Returns false if value is not found and an error if not a bool.
func NestedBool(obj map[string]interface{}, fields ...string) (bool, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return false, found, err
	}
	b, ok := val.(bool)
	if !ok {
		return false, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected bool", jsonPath(fields), val, val)
	}
	return b, true, nil
}

// NestedFloat64 returns the float64 value of a nested field.
// Returns false if value is not found and an error if not a float64.
func NestedFloat64(obj map[string]interface{}, fields ...string) (float64, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return 0, found, err
	}
	f, ok := val.(float64)
	if !ok {
		return 0, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected float64", jsonPath(fields), val, val)
	}
	return f, true, nil
}

// NestedInt64 returns the int64 value of a nested field.
// Returns false if value is not found and an error if not an int64.
func NestedInt64(obj map[string]interface{}, fields ...string) (int64, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return 0, found, err
	}
	i, ok := val.(int64)
	if !ok {
		return 0, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected int64", jsonPath(fields), val, val)
	}
	return i, true, nil
}

// NestedNumberAsFloat64 returns the float64 value of a nested field. If the field's value is a
// float64, it is returned. If the field's value is an int64 that can be losslessly converted to
// float64, it will be converted and returned.  Returns false if value is not found and an error if
// not a float64 or an int64 that can be accurately represented as a float64.
func NestedNumberAsFloat64(obj map[string]interface{}, fields ...string) (float64, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return 0, found, err
	}
	switch x := val.(type) {
	case int64:
		f, accuracy := big.NewInt(x).Float64()
		if accuracy != big.Exact {
			return 0, false, fmt.Errorf("%v accessor error: int64 value %v cannot be losslessly converted to float64", jsonPath(fields), x)
		}
		return f, true, nil
	case float64:
		return x, true, nil
	default:
		return 0, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected float64 or int64", jsonPath(fields), val, val)
	}
}

// NestedStringSlice returns a copy of []string value of a nested field.
// Returns false if value is not found and an error if not a []interface{} or contains non-string items in the slice.
func NestedStringSlice(obj map[string]interface{}, fields ...string) ([]string, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	m, ok := val.([]interface{})
	if !ok {
		return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected []interface{}", jsonPath(fields), val, val)
	}
	strSlice := make([]string, 0, len(m))
	for _, v := range m {
		if str, ok := v.(string); ok {
			strSlice = append(strSlice, str)
		} else {
			return nil, false, fmt.Errorf("%v accessor error: contains non-string key in the slice: %v is of the type %T, expected string", jsonPath(fields), v, v)
		}
	}
	return strSlice, true, nil
}

// NestedSlice returns a deep copy of []interface{} value of a nested field.
// Returns false if value is not found and an error if not a []interface{}.
func NestedSlice(obj map[string]interface{}, fields ...string) ([]interface{}, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	_, ok := val.([]interface{})
	if !ok {
		return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected []interface{}", jsonPath(fields), val, val)
	}
	return runtime.DeepCopyJSONValue(val).([]interface{}), true, nil
}

// NestedStringMap returns a copy of map[string]string value of a nested field.
// Returns false if value is not found and an error if not a map[string]interface{} or contains non-string values in the map.
func NestedStringMap(obj map[string]interface{}, fields ...string) (map[string]string, bool, error) {
	m, found, err := nestedMapNoCopy(obj, false, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	strMap := make(map[string]string, len(m))
	for k, v := range m {
		if str, ok := v.(string); ok {
			strMap[k] = str
		} else {
			return nil, false, fmt.Errorf("%v accessor error: contains non-string value in the map under key %q: %v is of the type %T, expected string", jsonPath(fields), k, v, v)
		}
	}
	return strMap, true, nil
}

// NestedNullCoercingStringMap returns a copy of map[string]string value of a nested field.
// Returns `nil, true, nil` if the value exists and is explicitly null.
// Returns `nil, false, err` if the value is not a map or a null value, or is a map and contains non-string non-null values.
// Null values in the map are coerced to "" to match json decoding behavior.
func NestedNullCoercingStringMap(obj map[string]interface{}, fields ...string) (map[string]string, bool, error) {
	m, found, err := nestedMapNoCopy(obj, true, fields...)
	if !found || err != nil || m == nil {
		return nil, found, err
	}
	strMap := make(map[string]string, len(m))
	for k, v := range m {
		if str, ok := v.(string); ok {
			strMap[k] = str
		} else if v == nil {
			strMap[k] = ""
		} else {
			return nil, false, fmt.Errorf("%v accessor error: contains non-string value in the map under key %q: %v is of the type %T, expected string", jsonPath(fields), k, v, v)
		}
	}
	return strMap, true, nil
}

// NestedMap returns a deep copy of map[string]interface{} value of a nested field.
// Returns false if value is not found and an error if not a map[string]interface{}.
func NestedMap(obj map[string]interface{}, fields ...string) (map[string]interface{}, bool, error) {
	m, found, err := nestedMapNoCopy(obj, false, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	return runtime.DeepCopyJSON(m), true, nil
}

// nestedMapNoCopy returns a map[string]interface{} value of a nested field.
// Returns false if value is not found and an error if not a map[string]interface{}.
func nestedMapNoCopy(obj map[string]interface{}, tolerateNil bool, fields ...string) (map[string]interface{}, bool, error) {
	val, found, err := NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	if val == nil && tolerateNil {
		return nil, true, nil
	}
	m, ok := val.(map[string]interface{})
	if !ok {
		return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected map[string]interface{}", jsonPath(fields), val, val)
	}
	return m, true, nil
}

// SetNestedField sets the value of a nested field to a deep copy of the value provided.
// Returns an error if value cannot be set because one of the nesting levels is not a map[string]interface{}.
func SetNestedField(obj map[string]interface{}, value interface{}, fields ...string) error {
	return setNestedFieldNoCopy(obj, runtime.DeepCopyJSONValue(value), fields...)
}

func setNestedFieldNoCopy(obj map[string]interface{}, value interface{}, fields ...string) error {
	m := obj

	for i, field := range fields[:len(fields)-1] {
		if val, ok := m[field]; ok {
			if valMap, ok := val.(map[string]interface{}); ok {
				m = valMap
			} else {
				return fmt.Errorf("value cannot be set because %v is not a map[string]interface{}", jsonPath(fields[:i+1]))
			}
		} else {
			newVal := make(map[string]interface{})
			m[field] = newVal
			m = newVal
		}
	}
	m[fields[len(fields)-1]] = value
	return nil
}

// SetNestedStringSlice sets the string slice value of a nested field.
// Returns an error if value cannot be set because one of the nesting levels is not a map[string]interface{}.
func SetNestedStringSlice(obj map[string]interface{}, value []string, fields ...string) error {
	m := make([]interface{}, 0, len(value)) // convert []string into []interface{}
	for _, v := range value {
		m = append(m, v)
	}
	return setNestedFieldNoCopy(obj, m, fields...)
}

// SetNestedSlice sets the slice value of a nested field.
// Returns an error if value cannot be set because one of the nesting levels is not a map[string]interface{}.
func SetNestedSlice(obj map[string]interface{}, value []interface{}, fields ...string) error {
	return SetNestedField(obj, value, fields...)
}

// SetNestedStringMap sets the map[string]string value of a nested field.
// Returns an error if value cannot be set because one of the nesting levels is not a map[string]interface{}.
func SetNestedStringMap(obj map[string]interface{}, value map[string]string, fields ...string) error {
	m := make(map[string]interface{}, len(value)) // convert map[string]string into map[string]interface{}
	for k, v := range value {
		m[k] = v
	}
	return setNestedFieldNoCopy(obj, m, fields...)
}

// SetNestedMap sets the map[string]interface{} value of a nested field.
// Returns an error if value cannot be set because one of the nesting levels is not a map[string]interface{}.
func SetNestedMap(obj map[string]interface{}, value map[string]interface{}, fields ...string) error {
	return SetNestedField(obj, value, fields...)
}

// RemoveNestedField removes the nested field from the obj.
func RemoveNestedField(obj map[string]interface{}, fields ...string) {
	m := obj
	for _, field := range fields[:len(fields)-1] {
		if x, ok := m[field].(map[string]interface{}); ok {
			m = x
		} else {
			return
		}
	}
	delete(m, fields[len(fields)-1])
}

func getNestedString(obj map[string]interface{}, fields ...string) string {
	val, found, err := NestedString(obj, fields...)
	if !found || err != nil {
		return ""
	}
	return val
}

func getNestedInt64Pointer(obj map[string]interface{}, fields ...string) *int64 {
	val, found, err := NestedInt64(obj, fields...)
	if !found || err != nil {
		return nil
	}
	return &val
}

func jsonPath(fields []string) string {
	return "." + strings.Join(fields, ".")
}

func extractOwnerReference(v map[string]interface{}) metav1.OwnerReference {
	// though this field is a *bool, but when decoded from JSON, it's
	// unmarshalled as bool.
	var controllerPtr *bool
	if controller, found, err := NestedBool(v, "controller"); err == nil && found {
		controllerPtr = &controller
	}
	var blockOwnerDeletionPtr *bool
	if blockOwnerDeletion, found, err := NestedBool(v, "blockOwnerDeletion"); err == nil && found {
		blockOwnerDeletionPtr = &blockOwnerDeletion
	}
	return metav1.OwnerReference{
		Kind:               getNestedString(v, "kind"),
		Name:               getNestedString(v, "name"),
		APIVersion:         getNestedString(v, "apiVersion"),
		UID:                types.UID(getNestedString(v, "uid")),
		Controller:         controllerPtr,
		BlockOwnerDeletion: blockOwnerDeletionPtr,
	}
}

// UnstructuredJSONScheme is capable of converting JSON data into the Unstructured
// type, which can be used for generic access to objects without a predefined scheme.
// TODO: move into serializer/json.
var UnstructuredJSONScheme runtime.Codec = unstructuredJSONScheme{}

type unstructuredJSONScheme struct{}

const unstructuredJSONSchemeIdentifier runtime.Identifier = "unstructuredJSON"

func (s unstructuredJSONScheme) Decode(data []byte, _ *schema.GroupVersionKind, obj runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	var err error
	if obj != nil {
		err = s.decodeInto(data, obj)
	} else {
		obj, err = s.decode(data)
	}

	if err != nil {
		return nil, nil, err
	}

	gvk := obj.GetObjectKind().GroupVersionKind()
	if len(gvk.Kind) == 0 {
		return nil, &gvk, runtime.NewMissingKindErr(string(data))
	}
	// TODO(109023): require apiVersion here as well

	return obj, &gvk, nil
}

func (s unstructuredJSONScheme) Encode(obj runtime.Object, w io.Writer) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(s.Identifier(), s.doEncode, w)
	}
	return s.doEncode(obj, w)
}

func (unstructuredJSONScheme) doEncode(obj runtime.Object, w io.Writer) error {
	switch t := obj.(type) {
	case *Unstructured:
		return json.NewEncoder(w).Encode(t.Object)
	case *UnstructuredList:
		items := make([]interface{}, 0, len(t.Items))
		for _, i := range t.Items {
			items = append(items, i.Object)
		}
		listObj := make(map[string]interface{}, len(t.Object)+1)
		for k, v := range t.Object { // Make a shallow copy
			listObj[k] = v
		}
		listObj["items"] = items
		return json.NewEncoder(w).Encode(listObj)
	case *runtime.Unknown:
		// TODO: Unstructured needs to deal with ContentType.
		_, err := w.Write(t.Raw)
		return err
	default:
		return json.NewEncoder(w).Encode(t)
	}
}

// Identifier implements runtime.Encoder interface.
func (unstructuredJSONScheme) Identifier() runtime.Identifier {
	return unstructuredJSONSchemeIdentifier
}

func (s unstructuredJSONScheme) decode(data []byte) (runtime.Object, error) {
	type detector struct {
		Items gojson.RawMessage `json:"items"`
	}
	var det detector
	if err := json.Unmarshal(data, &det); err != nil {
		return nil, err
	}

	if det.Items != nil {
		list := &UnstructuredList{}
		err := s.decodeToList(data, list)
		return list, err
	}

	// No Items field, so it wasn't a list.
	unstruct := &Unstructured{}
	err := s.decodeToUnstructured(data, unstruct)
	return unstruct, err
}

func (s unstructuredJSONScheme) decodeInto(data []byte, obj runtime.Object) error {
	switch x := obj.(type) {
	case *Unstructured:
		return s.decodeToUnstructured(data, x)
	case *UnstructuredList:
		return s.decodeToList(data, x)
	default:
		return json.Unmarshal(data, x)
	}
}

func (unstructuredJSONScheme) decodeToUnstructured(data []byte, unstruct *Unstructured) error {
	m := make(map[string]interface{})
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	unstruct.Object = m

	return nil
}

func (s unstructuredJSONScheme) decodeToList(data []byte, list *UnstructuredList) error {
	type decodeList struct {
		Items []gojson.RawMessage `json:"items"`
	}

	var dList decodeList
	if err := json.Unmarshal(data, &dList); err != nil {
		return err
	}

	if err := json.Unmarshal(data, &list.Object); err != nil {
		return err
	}

	// For typed lists, e.g., a PodList, API server doesn't set each item's
	// APIVersion and Kind. We need to set it.
	listAPIVersion := list.GetAPIVersion()
	listKind := list.GetKind()
	itemKind := strings.TrimSuffix(listKind, "List")

	delete(list.Object, "items")
	list.Items = make([]Unstructured, 0, len(dList.Items))
	for _, i := range dList.Items {
		unstruct := &Unstructured{}
		if err := s.decodeToUnstructured([]byte(i), unstruct); err != nil {
			return err
		}
		// This is hacky. Set the item's Kind and APIVersion to those inferred
		// from the List.
		if len(unstruct.GetKind()) == 0 && len(unstruct.GetAPIVersion()) == 0 {
			unstruct.SetKind(itemKind)
			unstruct.SetAPIVersion(listAPIVersion)
		}
		list.Items = append(list.Items, *unstruct)
	}
	return nil
}

type jsonFallbackEncoder struct {
	encoder    runtime.Encoder
	identifier runtime.Identifier
}

func NewJSONFallbackEncoder(encoder runtime.Encoder) runtime.Encoder {
	result := map[string]string{
		"name": "fallback",
		"base": string(encoder.Identifier()),
	}
	identifier, err := gojson.Marshal(result)
	if err != nil {
		//nolint:logcheck // Hopefully not reached, it would kill the process.
		klog.Fatalf("Failed marshaling identifier for jsonFallbackEncoder: %v", err)
	}
	return &jsonFallbackEncoder{
		encoder:    encoder,
		identifier: runtime.Identifier(identifier),
	}
}

func (c *jsonFallbackEncoder) Encode(obj runtime.Object, w io.Writer) error {
	// There is no need to handle runtime.CacheableObject, as we only
	// fallback to other encoders here.
	err := c.encoder.Encode(obj, w)
	if runtime.IsNotRegisteredError(err) {
		switch obj.(type) {
		case *Unstructured, *UnstructuredList:
			return UnstructuredJSONScheme.Encode(obj, w)
		}
	}
	return err
}

// Identifier implements runtime.Encoder interface.
func (c *jsonFallbackEncoder) Identifier() runtime.Identifier {
	return c.identifier
}
