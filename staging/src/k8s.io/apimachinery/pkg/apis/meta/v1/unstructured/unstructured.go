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
	"bytes"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// Unstructured allows objects that do not have Golang structs registered to be manipulated
// generically. This can be used to deal with the API objects from a plug-in. Unstructured
// objects still have functioning TypeMeta features-- kind, version, etc.
//
// WARNING: This object has accessors for the v1 standard metadata. You *MUST NOT* use this
// type if you are dealing with objects that are not in the server meta v1 schema.
//
// TODO: make the serialization part of this type distinct from the field accessors.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:deepcopy-gen=true
type Unstructured struct {
	// Object is a JSON compatible map with string, float, int, bool, []interface{}, or
	// map[string]interface{}
	// children.
	Object map[string]interface{}
}

var _ metav1.Object = &Unstructured{}
var _ runtime.Unstructured = &Unstructured{}
var _ metav1.ListInterface = &Unstructured{}

func (obj *Unstructured) GetObjectKind() schema.ObjectKind { return obj }

func (obj *Unstructured) IsList() bool {
	field, ok := obj.Object["items"]
	if !ok {
		return false
	}
	_, ok = field.([]interface{})
	return ok
}
func (obj *Unstructured) ToList() (*UnstructuredList, error) {
	if !obj.IsList() {
		// return an empty list back
		return &UnstructuredList{Object: obj.Object}, nil
	}

	ret := &UnstructuredList{}
	ret.Object = obj.Object

	err := obj.EachListItem(func(item runtime.Object) error {
		castItem := item.(*Unstructured)
		ret.Items = append(ret.Items, *castItem)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return ret, nil
}

func (obj *Unstructured) EachListItem(fn func(runtime.Object) error) error {
	field, ok := obj.Object["items"]
	if !ok {
		return errors.New("content is not a list")
	}
	items, ok := field.([]interface{})
	if !ok {
		return fmt.Errorf("content is not a list: %T", field)
	}
	for _, item := range items {
		child, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("items member is not an object: %T", child)
		}
		if err := fn(&Unstructured{Object: child}); err != nil {
			return err
		}
	}
	return nil
}

func (obj *Unstructured) EachListItemWithAlloc(fn func(runtime.Object) error) error {
	// EachListItem has allocated a new Object for the user, we can use it directly.
	return obj.EachListItem(fn)
}

func (obj *Unstructured) UnstructuredContent() map[string]interface{} {
	if obj.Object == nil {
		return make(map[string]interface{})
	}
	return obj.Object
}

func (obj *Unstructured) SetUnstructuredContent(content map[string]interface{}) {
	obj.Object = content
}

// MarshalJSON ensures that the unstructured object produces proper
// JSON when passed to Go's standard JSON library.
func (u *Unstructured) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	err := UnstructuredJSONScheme.Encode(u, &buf)
	return buf.Bytes(), err
}

// UnmarshalJSON ensures that the unstructured object properly decodes
// JSON when passed to Go's standard JSON library.
func (u *Unstructured) UnmarshalJSON(b []byte) error {
	_, _, err := UnstructuredJSONScheme.Decode(b, nil, u)
	return err
}

// NewEmptyInstance returns a new instance of the concrete type containing only kind/apiVersion and no other data.
// This should be called instead of reflect.New() for unstructured types because the go type alone does not preserve kind/apiVersion info.
func (in *Unstructured) NewEmptyInstance() runtime.Unstructured {
	out := new(Unstructured)
	if in != nil {
		out.GetObjectKind().SetGroupVersionKind(in.GetObjectKind().GroupVersionKind())
	}
	return out
}

func (in *Unstructured) DeepCopy() *Unstructured {
	if in == nil {
		return nil
	}
	out := new(Unstructured)
	*out = *in
	out.Object = runtime.DeepCopyJSON(in.Object)
	return out
}

func (u *Unstructured) setNestedField(value interface{}, fields ...string) {
	if u.Object == nil {
		u.Object = make(map[string]interface{})
	}
	SetNestedField(u.Object, value, fields...)
}

func (u *Unstructured) setNestedStringSlice(value []string, fields ...string) {
	if u.Object == nil {
		u.Object = make(map[string]interface{})
	}
	SetNestedStringSlice(u.Object, value, fields...)
}

func (u *Unstructured) setNestedSlice(value []interface{}, fields ...string) {
	if u.Object == nil {
		u.Object = make(map[string]interface{})
	}
	SetNestedSlice(u.Object, value, fields...)
}

func (u *Unstructured) setNestedMap(value map[string]string, fields ...string) {
	if u.Object == nil {
		u.Object = make(map[string]interface{})
	}
	SetNestedStringMap(u.Object, value, fields...)
}

func (u *Unstructured) GetOwnerReferences() []metav1.OwnerReference {
	field, found, err := NestedFieldNoCopy(u.Object, "metadata", "ownerReferences")
	if !found || err != nil {
		return nil
	}
	original, ok := field.([]interface{})
	if !ok {
		return nil
	}
	ret := make([]metav1.OwnerReference, 0, len(original))
	for _, obj := range original {
		o, ok := obj.(map[string]interface{})
		if !ok {
			// expected map[string]interface{}, got something else
			return nil
		}
		ret = append(ret, extractOwnerReference(o))
	}
	return ret
}

func (u *Unstructured) SetOwnerReferences(references []metav1.OwnerReference) {
	if references == nil {
		RemoveNestedField(u.Object, "metadata", "ownerReferences")
		return
	}

	newReferences := make([]interface{}, 0, len(references))
	for _, reference := range references {
		out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&reference)
		if err != nil {
			//nolint:logcheck // Should not happen?
			utilruntime.HandleError(fmt.Errorf("unable to convert Owner Reference: %v", err))
			continue
		}
		newReferences = append(newReferences, out)
	}
	u.setNestedField(newReferences, "metadata", "ownerReferences")
}

func (u *Unstructured) GetAPIVersion() string {
	return getNestedString(u.Object, "apiVersion")
}

func (u *Unstructured) SetAPIVersion(version string) {
	u.setNestedField(version, "apiVersion")
}

func (u *Unstructured) GetKind() string {
	return getNestedString(u.Object, "kind")
}

func (u *Unstructured) SetKind(kind string) {
	u.setNestedField(kind, "kind")
}

func (u *Unstructured) GetNamespace() string {
	return getNestedString(u.Object, "metadata", "namespace")
}

func (u *Unstructured) SetNamespace(namespace string) {
	if len(namespace) == 0 {
		RemoveNestedField(u.Object, "metadata", "namespace")
		return
	}
	u.setNestedField(namespace, "metadata", "namespace")
}

func (u *Unstructured) GetName() string {
	return getNestedString(u.Object, "metadata", "name")
}

func (u *Unstructured) SetName(name string) {
	if len(name) == 0 {
		RemoveNestedField(u.Object, "metadata", "name")
		return
	}
	u.setNestedField(name, "metadata", "name")
}

func (u *Unstructured) GetGenerateName() string {
	return getNestedString(u.Object, "metadata", "generateName")
}

func (u *Unstructured) SetGenerateName(generateName string) {
	if len(generateName) == 0 {
		RemoveNestedField(u.Object, "metadata", "generateName")
		return
	}
	u.setNestedField(generateName, "metadata", "generateName")
}

func (u *Unstructured) GetUID() types.UID {
	return types.UID(getNestedString(u.Object, "metadata", "uid"))
}

func (u *Unstructured) SetUID(uid types.UID) {
	if len(string(uid)) == 0 {
		RemoveNestedField(u.Object, "metadata", "uid")
		return
	}
	u.setNestedField(string(uid), "metadata", "uid")
}

func (u *Unstructured) GetResourceVersion() string {
	return getNestedString(u.Object, "metadata", "resourceVersion")
}

func (u *Unstructured) SetResourceVersion(resourceVersion string) {
	if len(resourceVersion) == 0 {
		RemoveNestedField(u.Object, "metadata", "resourceVersion")
		return
	}
	u.setNestedField(resourceVersion, "metadata", "resourceVersion")
}

func (u *Unstructured) GetGeneration() int64 {
	val, found, err := NestedInt64(u.Object, "metadata", "generation")
	if !found || err != nil {
		return 0
	}
	return val
}

func (u *Unstructured) SetGeneration(generation int64) {
	if generation == 0 {
		RemoveNestedField(u.Object, "metadata", "generation")
		return
	}
	u.setNestedField(generation, "metadata", "generation")
}

func (u *Unstructured) GetSelfLink() string {
	return getNestedString(u.Object, "metadata", "selfLink")
}

func (u *Unstructured) SetSelfLink(selfLink string) {
	if len(selfLink) == 0 {
		RemoveNestedField(u.Object, "metadata", "selfLink")
		return
	}
	u.setNestedField(selfLink, "metadata", "selfLink")
}

func (u *Unstructured) GetContinue() string {
	return getNestedString(u.Object, "metadata", "continue")
}

func (u *Unstructured) SetContinue(c string) {
	if len(c) == 0 {
		RemoveNestedField(u.Object, "metadata", "continue")
		return
	}
	u.setNestedField(c, "metadata", "continue")
}

func (u *Unstructured) GetRemainingItemCount() *int64 {
	return getNestedInt64Pointer(u.Object, "metadata", "remainingItemCount")
}

func (u *Unstructured) SetRemainingItemCount(c *int64) {
	if c == nil {
		RemoveNestedField(u.Object, "metadata", "remainingItemCount")
	} else {
		u.setNestedField(*c, "metadata", "remainingItemCount")
	}
}

func (u *Unstructured) GetCreationTimestamp() metav1.Time {
	var timestamp metav1.Time
	timestamp.UnmarshalQueryParameter(getNestedString(u.Object, "metadata", "creationTimestamp"))
	return timestamp
}

func (u *Unstructured) SetCreationTimestamp(timestamp metav1.Time) {
	ts, _ := timestamp.MarshalQueryParameter()
	if len(ts) == 0 || timestamp.Time.IsZero() {
		RemoveNestedField(u.Object, "metadata", "creationTimestamp")
		return
	}
	u.setNestedField(ts, "metadata", "creationTimestamp")
}

func (u *Unstructured) GetDeletionTimestamp() *metav1.Time {
	var timestamp metav1.Time
	timestamp.UnmarshalQueryParameter(getNestedString(u.Object, "metadata", "deletionTimestamp"))
	if timestamp.IsZero() {
		return nil
	}
	return &timestamp
}

func (u *Unstructured) SetDeletionTimestamp(timestamp *metav1.Time) {
	if timestamp == nil {
		RemoveNestedField(u.Object, "metadata", "deletionTimestamp")
		return
	}
	ts, _ := timestamp.MarshalQueryParameter()
	u.setNestedField(ts, "metadata", "deletionTimestamp")
}

func (u *Unstructured) GetDeletionGracePeriodSeconds() *int64 {
	val, found, err := NestedInt64(u.Object, "metadata", "deletionGracePeriodSeconds")
	if !found || err != nil {
		return nil
	}
	return &val
}

func (u *Unstructured) SetDeletionGracePeriodSeconds(deletionGracePeriodSeconds *int64) {
	if deletionGracePeriodSeconds == nil {
		RemoveNestedField(u.Object, "metadata", "deletionGracePeriodSeconds")
		return
	}
	u.setNestedField(*deletionGracePeriodSeconds, "metadata", "deletionGracePeriodSeconds")
}

func (u *Unstructured) GetLabels() map[string]string {
	m, _, _ := NestedNullCoercingStringMap(u.Object, "metadata", "labels")
	return m
}

func (u *Unstructured) SetLabels(labels map[string]string) {
	if labels == nil {
		RemoveNestedField(u.Object, "metadata", "labels")
		return
	}
	u.setNestedMap(labels, "metadata", "labels")
}

func (u *Unstructured) GetAnnotations() map[string]string {
	m, _, _ := NestedNullCoercingStringMap(u.Object, "metadata", "annotations")
	return m
}

func (u *Unstructured) SetAnnotations(annotations map[string]string) {
	if annotations == nil {
		RemoveNestedField(u.Object, "metadata", "annotations")
		return
	}
	u.setNestedMap(annotations, "metadata", "annotations")
}

func (u *Unstructured) SetGroupVersionKind(gvk schema.GroupVersionKind) {
	u.SetAPIVersion(gvk.GroupVersion().String())
	u.SetKind(gvk.Kind)
}

func (u *Unstructured) GroupVersionKind() schema.GroupVersionKind {
	gv, err := schema.ParseGroupVersion(u.GetAPIVersion())
	if err != nil {
		return schema.GroupVersionKind{}
	}
	gvk := gv.WithKind(u.GetKind())
	return gvk
}

func (u *Unstructured) GetFinalizers() []string {
	val, _, _ := NestedStringSlice(u.Object, "metadata", "finalizers")
	return val
}

func (u *Unstructured) SetFinalizers(finalizers []string) {
	if finalizers == nil {
		RemoveNestedField(u.Object, "metadata", "finalizers")
		return
	}
	u.setNestedStringSlice(finalizers, "metadata", "finalizers")
}

func (u *Unstructured) GetManagedFields() []metav1.ManagedFieldsEntry {
	v, found, err := NestedFieldNoCopy(u.Object, "metadata", "managedFields")
	if !found || err != nil {
		return nil
	}
	items, ok := v.([]interface{})
	if !ok {
		return nil
	}
	managedFields := []metav1.ManagedFieldsEntry{}
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			//nolint:logcheck // Hopefully never happens.
			utilruntime.HandleError(fmt.Errorf("unable to retrieve managedFields for object, item %v is not a map", item))
			return nil
		}
		out := metav1.ManagedFieldsEntry{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &out); err != nil {
			//nolint:logcheck // Hopefully never happens.
			utilruntime.HandleError(fmt.Errorf("unable to retrieve managedFields for object: %v", err))
			return nil
		}
		managedFields = append(managedFields, out)
	}
	return managedFields
}

func (u *Unstructured) SetManagedFields(managedFields []metav1.ManagedFieldsEntry) {
	if managedFields == nil {
		RemoveNestedField(u.Object, "metadata", "managedFields")
		return
	}
	items := []interface{}{}
	for _, managedFieldsEntry := range managedFields {
		out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&managedFieldsEntry)
		if err != nil {
			//nolint:logcheck // Hopefully never happens.
			utilruntime.HandleError(fmt.Errorf("unable to set managedFields for object: %v", err))
			return
		}
		items = append(items, out)
	}
	u.setNestedSlice(items, "metadata", "managedFields")
}
