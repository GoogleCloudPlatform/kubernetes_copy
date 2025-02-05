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

package resourceclaim

import (
	"context"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/apis/resource/validation"
	"k8s.io/kubernetes/pkg/features"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

// resourceclaimStrategy implements behavior for ResourceClaim objects
type resourceclaimStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Strategy is the default logic that applies when creating and updating
// ResourceClaim objects via the REST API.
var Strategy = resourceclaimStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

func (resourceclaimStrategy) NamespaceScoped() bool {
	return true
}

// GetResetFields returns the set of fields that get reset by the strategy and
// should not be modified by the user. For a new ResourceClaim that is the
// status.
func (resourceclaimStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{
		"resource.k8s.io/v1alpha3": fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		),
		"resource.k8s.io/v1beta1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		),
	}

	return fields
}

func (resourceclaimStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	claim := obj.(*resource.ResourceClaim)
	// Status must not be set by user on create.
	claim.Status = resource.ResourceClaimStatus{}

	dropDisabledFields(claim, nil)
}

func (resourceclaimStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	claim := obj.(*resource.ResourceClaim)
	return validation.ValidateResourceClaim(claim)
}

func (resourceclaimStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

func (resourceclaimStrategy) Canonicalize(obj runtime.Object) {
}

func (resourceclaimStrategy) AllowCreateOnUpdate() bool {
	return false
}

func (resourceclaimStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newClaim := obj.(*resource.ResourceClaim)
	oldClaim := old.(*resource.ResourceClaim)
	newClaim.Status = oldClaim.Status

	dropDisabledFields(newClaim, oldClaim)
}

func (resourceclaimStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	newClaim := obj.(*resource.ResourceClaim)
	oldClaim := old.(*resource.ResourceClaim)
	errorList := validation.ValidateResourceClaim(newClaim)
	return append(errorList, validation.ValidateResourceClaimUpdate(newClaim, oldClaim)...)
}

func (resourceclaimStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func (resourceclaimStrategy) AllowUnconditionalUpdate() bool {
	return true
}

type resourceclaimStatusStrategy struct {
	resourceclaimStrategy
}

var StatusStrategy = resourceclaimStatusStrategy{Strategy}

// GetResetFields returns the set of fields that get reset by the strategy and
// should not be modified by the user. For a status update that is the spec.
func (resourceclaimStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{
		"resource.k8s.io/v1alpha3": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
		"resource.k8s.io/v1beta1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
	}

	return fields
}

func (resourceclaimStatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newClaim := obj.(*resource.ResourceClaim)
	oldClaim := old.(*resource.ResourceClaim)
	newClaim.Spec = oldClaim.Spec
	metav1.ResetObjectMetaForStatus(&newClaim.ObjectMeta, &oldClaim.ObjectMeta)

	dropDeallocatedStatusDevices(newClaim, oldClaim)
	dropDisabledFields(newClaim, oldClaim)
}

func (resourceclaimStatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	newClaim := obj.(*resource.ResourceClaim)
	oldClaim := old.(*resource.ResourceClaim)
	return validation.ValidateResourceClaimStatusUpdate(newClaim, oldClaim)
}

// WarningsOnUpdate returns warnings for the given update.
func (resourceclaimStatusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// Match returns a generic matcher for a given label and field selector.
func Match(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: GetAttrs,
	}
}

// GetAttrs returns labels and fields of a given object for filtering purposes.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	claim, ok := obj.(*resource.ResourceClaim)
	if !ok {
		return nil, nil, errors.New("not a resourceclaim")
	}
	return labels.Set(claim.Labels), toSelectableFields(claim), nil
}

// toSelectableFields returns a field set that represents the object
func toSelectableFields(claim *resource.ResourceClaim) fields.Set {
	fields := generic.ObjectMetaFieldsSet(&claim.ObjectMeta, true)
	return fields
}

// dropDisabledFields removes fields which are covered by a feature gate.
func dropDisabledFields(newClaim, oldClaim *resource.ResourceClaim) {
	dropDisabledDRAPrioritizedListFields(newClaim, oldClaim)
	dropDisabledDRAAdminAccessFields(newClaim, oldClaim)
	dropDisabledDRAResourceClaimDeviceStatusFields(newClaim, oldClaim)
}

func dropDisabledDRAPrioritizedListFields(newClaim, oldClaim *resource.ResourceClaim) {
	if utilfeature.DefaultFeatureGate.Enabled(features.DRAPrioritizedList) {
		return
	}
	if draPrioritizedListFeatureInUse(oldClaim) {
		return
	}

	for i := range newClaim.Spec.Devices.Requests {
		newClaim.Spec.Devices.Requests[i].FirstAvailable = nil
	}
}

func draPrioritizedListFeatureInUse(claim *resource.ResourceClaim) bool {
	if claim == nil {
		return false
	}

	for _, request := range claim.Spec.Devices.Requests {
		if len(request.FirstAvailable) > 0 {
			return true
		}
	}

	return false
}

func dropDisabledDRAAdminAccessFields(newClaim, oldClaim *resource.ResourceClaim) {
	if utilfeature.DefaultFeatureGate.Enabled(features.DRAAdminAccess) {
		// No need to drop anything.
		return
	}
	if draAdminAccessFeatureInUse(oldClaim) {
		// If anything was set in the past, then fields must not get
		// dropped on potentially unrelated updates and, for example,
		// adding a status with AdminAccess=true is allowed. The
		// scheduler typically doesn't do that (it also checks the
		// feature gate and refuses to schedule), but the apiserver
		// would allow it.
		return
	}

	for i := range newClaim.Spec.Devices.Requests {
		newClaim.Spec.Devices.Requests[i].AdminAccess = nil
	}

	if newClaim.Status.Allocation == nil {
		return
	}
	for i := range newClaim.Status.Allocation.Devices.Results {
		newClaim.Status.Allocation.Devices.Results[i].AdminAccess = nil
	}
}

func draAdminAccessFeatureInUse(claim *resource.ResourceClaim) bool {
	if claim == nil {
		return false
	}

	for _, request := range claim.Spec.Devices.Requests {
		if request.AdminAccess != nil {
			return true
		}
	}

	if allocation := claim.Status.Allocation; allocation != nil {
		for _, result := range allocation.Devices.Results {
			if result.AdminAccess != nil {
				return true
			}
		}
	}

	return false
}

func dropDisabledDRAResourceClaimDeviceStatusFields(newClaim, oldClaim *resource.ResourceClaim) {
	isDRAResourceClaimDeviceStatusInUse := (oldClaim != nil && len(oldClaim.Status.Devices) > 0)
	// drop resourceClaim.Status.Devices field if feature gate is not enabled and it was not in use
	if !utilfeature.DefaultFeatureGate.Enabled(features.DRAResourceClaimDeviceStatus) && !isDRAResourceClaimDeviceStatusInUse {
		newClaim.Status.Devices = nil
	}
}

// dropDeallocatedStatusDevices removes the status.devices that were allocated
// in the oldClaim and that have been removed in the newClaim.
func dropDeallocatedStatusDevices(newClaim, oldClaim *resource.ResourceClaim) {
	isDRAResourceClaimDeviceStatusInUse := (oldClaim != nil && len(oldClaim.Status.Devices) > 0)
	if !utilfeature.DefaultFeatureGate.Enabled(features.DRAResourceClaimDeviceStatus) && !isDRAResourceClaimDeviceStatusInUse {
		return
	}

	deallocatedDevices := sets.New[structured.DeviceID]()

	if oldClaim.Status.Allocation != nil {
		// Get all devices in the oldClaim.
		for _, result := range oldClaim.Status.Allocation.Devices.Results {
			deviceID := structured.MakeDeviceID(result.Driver, result.Pool, result.Device)
			deallocatedDevices.Insert(deviceID)
		}
	}

	// Remove devices from deallocatedDevices that are still in newClaim.
	if newClaim.Status.Allocation != nil {
		for _, result := range newClaim.Status.Allocation.Devices.Results {
			deviceID := structured.MakeDeviceID(result.Driver, result.Pool, result.Device)
			deallocatedDevices.Delete(deviceID)
		}
	}

	// Remove from newClaim.Status.Devices.
	n := 0
	for _, device := range newClaim.Status.Devices {
		deviceID := structured.MakeDeviceID(device.Driver, device.Pool, device.Device)
		if !deallocatedDevices.Has(deviceID) {
			newClaim.Status.Devices[n] = device
			n++
		}
	}
	newClaim.Status.Devices = newClaim.Status.Devices[:n]

	if len(newClaim.Status.Devices) == 0 {
		newClaim.Status.Devices = nil
	}
}
