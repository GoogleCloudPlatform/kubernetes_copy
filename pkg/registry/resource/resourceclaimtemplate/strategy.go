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

package resourceclaimtemplate

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage/names"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/apis/resource/validation"
	"k8s.io/kubernetes/pkg/features"
)

// resourceClaimTemplateStrategy implements behavior for ResourceClaimTemplate objects
type resourceClaimTemplateStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

var Strategy = resourceClaimTemplateStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

func (resourceClaimTemplateStrategy) NamespaceScoped() bool {
	return true
}

func (resourceClaimTemplateStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	claimTemplate := obj.(*resource.ResourceClaimTemplate)
	dropDisabledFields(claimTemplate, nil)
}

func (resourceClaimTemplateStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	resourceClaimTemplate := obj.(*resource.ResourceClaimTemplate)
	return validation.ValidateResourceClaimTemplate(resourceClaimTemplate)
}

func (resourceClaimTemplateStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

func (resourceClaimTemplateStrategy) Canonicalize(obj runtime.Object) {
}

func (resourceClaimTemplateStrategy) AllowCreateOnUpdate() bool {
	return false
}

func (resourceClaimTemplateStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	claimTemplate, oldClaimTemplate := obj.(*resource.ResourceClaimTemplate), old.(*resource.ResourceClaimTemplate)
	dropDisabledFields(claimTemplate, oldClaimTemplate)
}

func (resourceClaimTemplateStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	errorList := validation.ValidateResourceClaimTemplate(obj.(*resource.ResourceClaimTemplate))
	return append(errorList, validation.ValidateResourceClaimTemplateUpdate(obj.(*resource.ResourceClaimTemplate), old.(*resource.ResourceClaimTemplate))...)
}

func (resourceClaimTemplateStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func (resourceClaimTemplateStrategy) AllowUnconditionalUpdate() bool {
	return true
}

// GetAttrs returns labels and fields of a given object for filtering purposes.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	template, ok := obj.(*resource.ResourceClaimTemplate)
	if !ok {
		return nil, nil, errors.New("not a resourceclaimtemplate")
	}
	return labels.Set(template.Labels), toSelectableFields(template), nil
}

// toSelectableFields returns a field set that represents the object
func toSelectableFields(template *resource.ResourceClaimTemplate) fields.Set {
	fields := generic.ObjectMetaFieldsSet(&template.ObjectMeta, true)
	return fields
}

func dropDisabledFields(newClaimTemplate, oldClaimTemplate *resource.ResourceClaimTemplate) {
	dropDisabledDRAPrioritizedListFields(newClaimTemplate, oldClaimTemplate)
	dropDisabledDRAAdminAccessFields(newClaimTemplate, oldClaimTemplate)
}

func dropDisabledDRAPrioritizedListFields(newClaimTemplate, oldClaimTemplate *resource.ResourceClaimTemplate) {
	if utilfeature.DefaultFeatureGate.Enabled(features.DRAPrioritizedList) {
		return
	}
	if draPrioritizedListFeatureInUse(oldClaimTemplate) {
		return
	}

	for i := range newClaimTemplate.Spec.Spec.Devices.Requests {
		newClaimTemplate.Spec.Spec.Devices.Requests[i].FirstAvailable = nil
	}
}

func draPrioritizedListFeatureInUse(claimTemplate *resource.ResourceClaimTemplate) bool {
	if claimTemplate == nil {
		return false
	}

	for _, request := range claimTemplate.Spec.Spec.Devices.Requests {
		if len(request.FirstAvailable) > 0 {
			return true
		}
	}

	return false
}

func dropDisabledDRAAdminAccessFields(newClaimTemplate, oldClaimTemplate *resource.ResourceClaimTemplate) {
	if utilfeature.DefaultFeatureGate.Enabled(features.DRAAdminAccess) {
		// No need to drop anything.
		return
	}
	if draAdminAccessFeatureInUse(oldClaimTemplate) {
		// If anything was set in the past, then fields must not get
		// dropped on potentially unrelated updates.
		return
	}

	for i := range newClaimTemplate.Spec.Spec.Devices.Requests {
		newClaimTemplate.Spec.Spec.Devices.Requests[i].AdminAccess = nil
	}
}

func draAdminAccessFeatureInUse(claimTemplate *resource.ResourceClaimTemplate) bool {
	if claimTemplate == nil {
		return false
	}

	for _, request := range claimTemplate.Spec.Spec.Devices.Requests {
		if request.AdminAccess != nil {
			return true
		}
	}

	return false
}
