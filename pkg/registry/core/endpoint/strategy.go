/*
Copyright 2014 The Kubernetes Authors.

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

package endpoint

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/validation"
)

// endpointsStrategy implements behavior for Endpoints
type endpointsStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Strategy is the default logic that applies when creating and updating Endpoint
// objects via the REST API.
var Strategy = endpointsStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

// NamespaceScoped is true for endpoints.
func (endpointsStrategy) NamespaceScoped() bool {
	return true
}

// PrepareForCreate clears fields that are not allowed to be set by end users on creation.
func (endpointsStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (endpointsStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
}

// Validate validates a new endpoints.
func (endpointsStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return validation.ValidateEndpointsCreate(obj.(*api.Endpoints))
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (endpointsStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return endpointsWarnings(obj.(*api.Endpoints))
}

// Canonicalize normalizes the object after validation.
func (endpointsStrategy) Canonicalize(obj runtime.Object) {
}

// AllowCreateOnUpdate is true for endpoints.
func (endpointsStrategy) AllowCreateOnUpdate() bool {
	return true
}

// ValidateUpdate is the default update validation for an end user.
func (endpointsStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateEndpointsUpdate(obj.(*api.Endpoints), old.(*api.Endpoints))
}

// WarningsOnUpdate returns warnings for the given update.
func (endpointsStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return endpointsWarnings(obj.(*api.Endpoints))
}

func (endpointsStrategy) AllowUnconditionalUpdate() bool {
	return true
}

func endpointsWarnings(endpoints *api.Endpoints) []string {
	// Don't bother checking for warnings if it looks like the request is coming from
	// the EndpointController.
	if endpoints.Annotations[api.EndpointsLastChangeTriggerTime] != "" {
		return nil
	}

	var warnings []string
	for i := range endpoints.Subsets {
		for j := range endpoints.Subsets[i].Addresses {
			fldPath := field.NewPath("subsets").Index(i).Child("addresses").Index(j).Child("ip")
			warnings = append(warnings, utilvalidation.GetWarningsForIP(fldPath, endpoints.Subsets[i].Addresses[j].IP)...)
		}
		for j := range endpoints.Subsets[i].NotReadyAddresses {
			fldPath := field.NewPath("subsets").Index(i).Child("notReadyAddresses").Index(j).Child("ip")
			warnings = append(warnings, utilvalidation.GetWarningsForIP(fldPath, endpoints.Subsets[i].NotReadyAddresses[j].IP)...)
		}
	}
	return warnings
}
