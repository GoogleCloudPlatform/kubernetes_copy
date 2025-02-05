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

package validatingadmissionpolicybinding

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	userpkg "k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/kubernetes/pkg/apis/admissionregistration"
)

func (v *validatingAdmissionPolicyBindingStrategy) authorizeCreate(ctx context.Context, obj runtime.Object) error {
	binding := obj.(*admissionregistration.ValidatingAdmissionPolicyBinding)
	if binding.Spec.ParamRef == nil {
		// no paramRef in new object
		return nil
	}

	return v.authorize(ctx, binding)
}

func (v *validatingAdmissionPolicyBindingStrategy) authorizeUpdate(ctx context.Context, obj, old runtime.Object) error {
	binding := obj.(*admissionregistration.ValidatingAdmissionPolicyBinding)
	if binding.Spec.ParamRef == nil {
		// no paramRef in new object
		return nil
	}

	oldBinding := old.(*admissionregistration.ValidatingAdmissionPolicyBinding)
	if oldBinding.Spec.ParamRef != nil && *oldBinding.Spec.ParamRef == *binding.Spec.ParamRef && oldBinding.Spec.PolicyName == binding.Spec.PolicyName {
		// identical paramRef and policy to old object
		return nil
	}

	return v.authorize(ctx, binding)
}

func (v *validatingAdmissionPolicyBindingStrategy) authorize(ctx context.Context, binding *admissionregistration.ValidatingAdmissionPolicyBinding) error {
	if v.resourceResolver == nil {
		return fmt.Errorf(`unexpected internal error: resourceResolver is nil`)
	}
	if v.authorizer == nil || binding.Spec.ParamRef == nil {
		return nil
	}

	user, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return fmt.Errorf("cannot identify user to authorize read access to paramRef object")
	}

	// for superuser, skip all checks
	isSystemAdmin := sets.New(user.GetGroups()...).Has(userpkg.SystemPrivilegedGroup)
	if isSystemAdmin {
		return nil
	}

	// default to requiring permissions on all group/version/resources
	resource, apiGroup, apiVersion := "*", "*", "*"

	var policyErr, gvParseErr, gvrResolveErr error

	var policy *admissionregistration.ValidatingAdmissionPolicy
	policy, policyErr = v.policyGetter.GetValidatingAdmissionPolicy(ctx, binding.Spec.PolicyName)
	if policyErr == nil && policy.Spec.ParamKind != nil {
		paramKind := policy.Spec.ParamKind
		var gv schema.GroupVersion
		gv, gvParseErr = schema.ParseGroupVersion(paramKind.APIVersion)
		if gvParseErr == nil {
			// we only need to authorize the parsed group/version
			apiGroup = gv.Group
			apiVersion = gv.Version
			var gvr schema.GroupVersionResource
			gvr, gvrResolveErr = v.resourceResolver.Resolve(gv.WithKind(paramKind.Kind))
			if gvrResolveErr == nil {
				// we only need to authorize the resolved resource
				resource = gvr.Resource
			}
		}
	}

	var attrs authorizer.AttributesRecord

	paramRef := binding.Spec.ParamRef
	verb := "get"

	if len(paramRef.Name) == 0 {
		verb = "list"
	}

	attrs = authorizer.AttributesRecord{
		User:            user,
		Verb:            verb,
		ResourceRequest: true,
		Name:            paramRef.Name,
		Namespace:       paramRef.Namespace, // if empty, no namespace indicates get across all namespaces
		APIGroup:        apiGroup,
		APIVersion:      apiVersion,
		Resource:        resource,
	}

	d, _, err := v.authorizer.Authorize(ctx, attrs)
	if err != nil {
		return fmt.Errorf(`failed to authorize request: %w`, err)
	}
	if d != authorizer.DecisionAllow {
		if policyErr != nil {
			return fmt.Errorf(`unable to get policy %s to determine minimum required permissions and user %v does not have "%v" permission for all groups, versions and resources`, binding.Spec.PolicyName, user, verb)
		}
		if gvParseErr != nil {
			return fmt.Errorf(`unable to parse paramKind %v to determine minimum required permissions and user %v does not have "%v" permission for all groups, versions and resources`, policy.Spec.ParamKind, user, verb)
		}
		if gvrResolveErr != nil {
			return fmt.Errorf(`unable to resolve paramKind %v to determine minimum required permissions and user %v does not have "%v" permission for all groups, versions and resources`, policy.Spec.ParamKind, user, verb)
		}
		return fmt.Errorf(`user %v does not have "%v" permission on the object referenced by paramRef`, user, verb)
	}

	return nil
}
