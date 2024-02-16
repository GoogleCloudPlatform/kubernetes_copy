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

package ingress

import (
	"context"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/networking"
	"k8s.io/kubernetes/pkg/apis/networking/validation"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

const (
	annotationIngressClass = "kubernetes.io/ingress.class"
)

// ingressStrategy implements verification logic for Replication Ingress.
type ingressStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Strategy is the default logic that applies when creating and updating Replication Ingress objects.
var Strategy = ingressStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

// NamespaceScoped returns true because all Ingress' need to be within a namespace.
func (ingressStrategy) NamespaceScoped() bool {
	return true
}

// GetResetFields returns the set of fields that get reset by the strategy
// and should not be modified by the user.
func (ingressStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{
		"extensions/v1beta1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		),
		"networking.k8s.io/v1beta1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		),
		"networking.k8s.io/v1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		),
	}

	return fields
}

// PrepareForCreate clears the status of an Ingress before creation.
func (ingressStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	ingress := obj.(*networking.Ingress)
	// create cannot set status
	ingress.Status = networking.IngressStatus{}

	ingress.Generation = 1
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (ingressStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newIngress := obj.(*networking.Ingress)
	oldIngress := old.(*networking.Ingress)
	// Update is not allowed to set status
	newIngress.Status = oldIngress.Status

	// Any changes to the spec increment the generation number, any changes to the
	// status should reflect the generation number of the corresponding object.
	// See metav1.ObjectMeta description for more information on Generation.
	if !apiequality.Semantic.DeepEqual(oldIngress.Spec, newIngress.Spec) {
		newIngress.Generation = oldIngress.Generation + 1
	}

}

// Validate validates ingresses on create.
func (ingressStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	ingress := obj.(*networking.Ingress)
	return validation.ValidateIngressCreate(ingress)
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (ingressStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	var warnings []string
	ingress := obj.(*networking.Ingress)
	_, annotationIsSet := ingress.Annotations[annotationIngressClass]
	if annotationIsSet && ingress.Spec.IngressClassName == nil {
		warnings = append(warnings, fmt.Sprintf("annotation %q is deprecated, please use 'spec.ingressClassName' instead", annotationIngressClass))
	}
	return warnings
}

// Canonicalize normalizes the object after validation.
func (ingressStrategy) Canonicalize(obj runtime.Object) {
}

// AllowCreateOnUpdate is false for Ingress; this means POST is needed to create one.
func (ingressStrategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate validates ingresses on update.
func (ingressStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateIngressUpdate(obj.(*networking.Ingress), old.(*networking.Ingress))
}

// WarningsOnUpdate returns warnings for the given update.
func (ingressStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// AllowUnconditionalUpdate is the default update policy for Ingress objects.
func (ingressStrategy) AllowUnconditionalUpdate() bool {
	return true
}

type ingressStatusStrategy struct {
	ingressStrategy
}

// StatusStrategy implements logic used to validate and prepare for updates of the status subresource
var StatusStrategy = ingressStatusStrategy{Strategy}

// GetResetFields returns the set of fields that get reset by the strategy
// and should not be modified by the user.
func (ingressStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{
		"extensions/v1beta1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
		"networking.k8s.io/v1beta1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
		"networking.k8s.io/v1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
	}

	return fields
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update of status
func (ingressStatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newIngress := obj.(*networking.Ingress)
	oldIngress := old.(*networking.Ingress)
	// status changes are not allowed to update spec
	newIngress.Spec = oldIngress.Spec
}

// ValidateUpdate is the default update validation for an end user updating status
func (ingressStatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateIngressStatusUpdate(obj.(*networking.Ingress), old.(*networking.Ingress))
}

// WarningsOnUpdate returns warnings for the given update.
func (ingressStatusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	newIngress := obj.(*networking.Ingress)
	var warnings []string

	if len(newIngress.Status.LoadBalancer.Ingress) > 0 {
		fieldPath := field.NewPath("status", "loadBalancer", "ingress")
		for i, ingress := range newIngress.Status.LoadBalancer.Ingress {
			if len(ingress.IP) > 0 {
				warnings = append(warnings, utilvalidation.GetWarningsForIP(fieldPath.Index(i), ingress.IP)...)
			}
		}
	}

	return warnings
}
