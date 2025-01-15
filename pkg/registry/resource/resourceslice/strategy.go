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

package resourceslice

import (
	"context"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/apis/resource/validation"
	"k8s.io/kubernetes/pkg/features"
)

// resourceSliceStrategy implements behavior for ResourceSlice objects
type resourceSliceStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

var Strategy = resourceSliceStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

func (resourceSliceStrategy) NamespaceScoped() bool {
	return false
}

func (resourceSliceStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	slice := obj.(*resource.ResourceSlice)
	slice.Generation = 1

	dropDisabledFields(slice, nil)
}

func (resourceSliceStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	slice := obj.(*resource.ResourceSlice)
	return validation.ValidateResourceSlice(slice)
}

func (resourceSliceStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

func (resourceSliceStrategy) Canonicalize(obj runtime.Object) {
}

func (resourceSliceStrategy) AllowCreateOnUpdate() bool {
	return false
}

func (resourceSliceStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	slice := obj.(*resource.ResourceSlice)
	oldSlice := old.(*resource.ResourceSlice)

	// Any changes to the spec increment the generation number.
	if !apiequality.Semantic.DeepEqual(oldSlice.Spec, slice.Spec) {
		slice.Generation = oldSlice.Generation + 1
	}

	dropDisabledFields(slice, oldSlice)
}

func (resourceSliceStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateResourceSliceUpdate(obj.(*resource.ResourceSlice), old.(*resource.ResourceSlice))
}

func (resourceSliceStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func (resourceSliceStrategy) AllowUnconditionalUpdate() bool {
	return true
}

var TriggerFunc = map[string]storage.IndexerFunc{
	// Only one index is supported:
	// https://github.com/kubernetes/kubernetes/blob/3aa8c59fec0bf339e67ca80ea7905c817baeca85/staging/src/k8s.io/apiserver/pkg/storage/cacher/cacher.go#L346-L350
	resource.ResourceSliceSelectorNodeName: nodeNameTriggerFunc,
}

func nodeNameTriggerFunc(obj runtime.Object) string {
	return obj.(*resource.ResourceSlice).Spec.NodeName
}

// Indexers returns the indexers for ResourceSlice.
func Indexers() *cache.Indexers {
	return &cache.Indexers{
		storage.FieldIndex(resource.ResourceSliceSelectorNodeName): nodeNameIndexFunc,
	}
}

func nodeNameIndexFunc(obj interface{}) ([]string, error) {
	slice, ok := obj.(*resource.ResourceSlice)
	if !ok {
		return nil, fmt.Errorf("not a ResourceSlice")
	}
	return []string{slice.Spec.NodeName}, nil
}

// GetAttrs returns labels and fields of a given object for filtering purposes.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	slice, ok := obj.(*resource.ResourceSlice)
	if !ok {
		return nil, nil, fmt.Errorf("not a ResourceSlice")
	}
	return labels.Set(slice.ObjectMeta.Labels), toSelectableFields(slice), nil
}

// Match returns a generic matcher for a given label and field selector.
func Match(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:       label,
		Field:       field,
		GetAttrs:    GetAttrs,
		IndexFields: []string{resource.ResourceSliceSelectorNodeName},
	}
}

// toSelectableFields returns a field set that represents the object
// TODO: fields are not labels, and the validation rules for them do not apply.
func toSelectableFields(slice *resource.ResourceSlice) fields.Set {
	// The purpose of allocation with a given number of elements is to reduce
	// amount of allocations needed to create the fields.Set. If you add any
	// field here or the number of object-meta related fields changes, this should
	// be adjusted.
	fields := make(fields.Set, 3)
	fields[resource.ResourceSliceSelectorNodeName] = slice.Spec.NodeName
	fields[resource.ResourceSliceSelectorDriver] = slice.Spec.Driver

	// Adds one field.
	return generic.AddObjectMetaFieldsSet(fields, &slice.ObjectMeta, false)
}

// dropDisabledFields removes fields which are covered by a feature gate.
func dropDisabledFields(newSlice, oldSlice *resource.ResourceSlice) {
	dropDisabledDRAPartitionableDevices(newSlice, oldSlice)
}

func dropDisabledDRAPartitionableDevices(newSlice, oldSlice *resource.ResourceSlice) {
	if utilfeature.DefaultFeatureGate.Enabled(features.DRAPartitionableDevices) {
		return
	}

	if DRAPartitionableDevicesFeatureInUse(oldSlice) {
		return
	}

	newSlice.Spec.DeviceMixins = nil
	for i := range newSlice.Spec.Devices {
		newSlice.Spec.Devices[i].Composite = nil
	}
}

func DRAPartitionableDevicesFeatureInUse(slice *resource.ResourceSlice) bool {
	if slice == nil {
		return false
	}

	if len(slice.Spec.DeviceMixins) > 0 {
		return true
	}

	for _, device := range slice.Spec.Devices {
		if device.Composite != nil {
			return true
		}
	}
	return false
}
