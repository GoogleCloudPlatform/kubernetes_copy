/*
Copyright 2017 The Kubernetes Authors.

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

package networkpolicy

import (
	"context"
	"reflect"

	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/networking"
	"k8s.io/kubernetes/pkg/apis/networking/validation"
)

// networkPolicyStrategy implements verification logic for NetworkPolicies
type networkPolicyStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Strategy is the default logic that applies when creating and updating NetworkPolicy objects.
var Strategy = networkPolicyStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

// NamespaceScoped returns true because all NetworkPolicies need to be within a namespace.
func (networkPolicyStrategy) NamespaceScoped() bool {
	return true
}

// PrepareForCreate clears the status of a NetworkPolicy before creation.
func (networkPolicyStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	networkPolicy := obj.(*networking.NetworkPolicy)
	networkPolicy.Generation = 1
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (networkPolicyStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newNetworkPolicy := obj.(*networking.NetworkPolicy)
	oldNetworkPolicy := old.(*networking.NetworkPolicy)

	// Any changes to the spec increment the generation number, any changes to the
	// status should reflect the generation number of the corresponding object.
	// See metav1.ObjectMeta description for more information on Generation.
	if !reflect.DeepEqual(oldNetworkPolicy.Spec, newNetworkPolicy.Spec) {
		newNetworkPolicy.Generation = oldNetworkPolicy.Generation + 1
	}
}

// Validate validates a new NetworkPolicy.
func (networkPolicyStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	networkPolicy := obj.(*networking.NetworkPolicy)
	ops := validation.ValidationOptionsForNetworking(networkPolicy, nil)
	return validation.ValidateNetworkPolicy(networkPolicy, ops)
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (networkPolicyStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return networkPolicyWarnings(obj.(*networking.NetworkPolicy))
}

// Canonicalize normalizes the object after validation.
func (networkPolicyStrategy) Canonicalize(obj runtime.Object) {}

// AllowCreateOnUpdate is false for NetworkPolicy; this means POST is needed to create one.
func (networkPolicyStrategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate is the default update validation for an end user.
func (networkPolicyStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	opts := validation.ValidationOptionsForNetworking(obj.(*networking.NetworkPolicy), old.(*networking.NetworkPolicy))
	validationErrorList := validation.ValidateNetworkPolicy(obj.(*networking.NetworkPolicy), opts)
	updateErrorList := validation.ValidateNetworkPolicyUpdate(obj.(*networking.NetworkPolicy), old.(*networking.NetworkPolicy), opts)
	return append(validationErrorList, updateErrorList...)
}

// WarningsOnUpdate returns warnings for the given update.
func (networkPolicyStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return networkPolicyWarnings(obj.(*networking.NetworkPolicy))
}

// AllowUnconditionalUpdate is the default update policy for NetworkPolicy objects.
func (networkPolicyStrategy) AllowUnconditionalUpdate() bool {
	return true
}

func networkPolicyWarnings(networkPolicy *networking.NetworkPolicy) []string {
	var warnings []string
	for i := range networkPolicy.Spec.Ingress {
		for j := range networkPolicy.Spec.Ingress[i].From {
			ipBlock := networkPolicy.Spec.Ingress[i].From[j].IPBlock
			if ipBlock == nil {
				continue
			}
			fldPath := field.NewPath("spec").Child("ingress").Index(i).Child("from").Index(j).Child("ipBlock")
			warnings = append(warnings, utilvalidation.GetWarningsForCIDR(fldPath.Child("cidr"), ipBlock.CIDR)...)
			for k, except := range ipBlock.Except {
				warnings = append(warnings, utilvalidation.GetWarningsForCIDR(fldPath.Child("except").Index(k), except)...)
			}
		}
	}
	for i := range networkPolicy.Spec.Egress {
		for j := range networkPolicy.Spec.Egress[i].To {
			ipBlock := networkPolicy.Spec.Egress[i].To[j].IPBlock
			if ipBlock == nil {
				continue
			}
			fldPath := field.NewPath("spec").Child("egress").Index(i).Child("to").Index(j).Child("ipBlock")
			warnings = append(warnings, utilvalidation.GetWarningsForCIDR(fldPath.Child("cidr"), ipBlock.CIDR)...)
			for k, except := range ipBlock.Except {
				warnings = append(warnings, utilvalidation.GetWarningsForCIDR(fldPath.Child("except").Index(k), except)...)
			}
		}
	}
	return warnings
}
