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

package validatingadmissionpolicy

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

func (v *validatingAdmissionPolicyStrategy) authorizeCreate(ctx context.Context, obj runtime.Object) error {
	policy := obj.(*admissionregistration.ValidatingAdmissionPolicy)
	if policy.Spec.ParamKind == nil {
		// no paramRef in new object
		return nil
	}

	return v.authorize(ctx, policy)
}

func (v *validatingAdmissionPolicyStrategy) authorizeUpdate(ctx context.Context, obj, old runtime.Object) error {
	policy := obj.(*admissionregistration.ValidatingAdmissionPolicy)
	if policy.Spec.ParamKind == nil {
		// no paramRef in new object
		return nil
	}

	oldPolicy := old.(*admissionregistration.ValidatingAdmissionPolicy)
	if oldPolicy.Spec.ParamKind != nil && *oldPolicy.Spec.ParamKind == *policy.Spec.ParamKind {
		// identical paramKind to old object
		return nil
	}

	return v.authorize(ctx, policy)
}

func (v *validatingAdmissionPolicyStrategy) authorize(ctx context.Context, policy *admissionregistration.ValidatingAdmissionPolicy) error {
	if v.authorizer == nil || policy.Spec.ParamKind == nil {
		return nil
	}

	user, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return fmt.Errorf("cannot identify user to authorize read access to paramKind resources")
	}

	// for superuser, skip all checks
	isSystemAdmin := sets.New(user.GetGroups()...).Has(userpkg.SystemPrivilegedGroup)
	if isSystemAdmin {
		return nil
	}

	paramKind := policy.Spec.ParamKind
	// default to requiring permissions on all group/version/resources
	resource, apiGroup, apiVersion := "*", "*", "*"
	if gv, err := schema.ParseGroupVersion(paramKind.APIVersion); err == nil {
		// we only need to authorize the parsed group/version
		apiGroup = gv.Group
		apiVersion = gv.Version
		if gvr, err := v.resourceResolver.Resolve(gv.WithKind(paramKind.Kind)); err == nil {
			// we only need to authorize the resolved resource
			resource = gvr.Resource
		}
	}

	// require that the user can read (verb "get") the referred kind.
	attrs := authorizer.AttributesRecord{
		User:            user,
		Verb:            "get",
		ResourceRequest: true,
		Name:            "*",
		Namespace:       "*",
		APIGroup:        apiGroup,
		APIVersion:      apiVersion,
		Resource:        resource,
	}

	d, _, err := v.authorizer.Authorize(ctx, attrs)
	if err != nil {
		return err
	}
	if d != authorizer.DecisionAllow {
		return fmt.Errorf(`user %v must have "get" permission on all objects of the referenced paramKind (kind=%s, apiVersion=%s)`, user, paramKind.Kind, paramKind.APIVersion)
	}
	return nil
}
