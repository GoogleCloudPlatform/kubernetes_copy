/*
Copyright 2024 The Kubernetes Authors.

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

package generic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	webhookgeneric "k8s.io/apiserver/pkg/admission/plugin/webhook/generic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// PolicyInvocation is a single policy-binding-param tuple from a Policy Hook
// in the context of a specific request. The params have already been resolved
// and any error in configuration or setting up the invocation is stored in
// the Error field.
type PolicyInvocation[P runtime.Object, B runtime.Object, E Evaluator] struct {
	// Relevant policy for this hook.
	// This field is always populated
	Policy P

	// Matched Kind for the request given the policy's matchconstraints
	// May be empty if there was an error matching the resource
	Kind schema.GroupVersionKind

	// Matched Resource for the request given the policy's matchconstraints
	// May be empty if there was an error matching the resource
	Resource schema.GroupVersionResource

	// Relevant binding for this hook.
	// May be empty if there was an error with the policy's configuration itself
	Binding B

	// Compiled policy evaluator
	Evaluator E

	// Params fetched by the binding to use to evaluate the policy
	Param runtime.Object
}

// dispatcherDelegate is called during a request with a pre-filtered list
// of (Policy, Binding, Param) tuples that are active and match the request.
// The dispatcher delegate is responsible for updating the object on the
// admission attributes in the case of mutation, or returning a status error in
// the case of validation.
//
// The delegate provides the "validation" or "mutation" aspect of dispatcher functionality
// (in contrast to generic.PolicyDispatcher which only selects active policies and params)
type dispatcherDelegate[P, B runtime.Object, E Evaluator] func(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces, versionedAttributes webhookgeneric.VersionedAttributeAccessor, invocations []PolicyInvocation[P, B, E]) ([]PolicyError, *apierrors.StatusError)

type policyDispatcher[P runtime.Object, B runtime.Object, E Evaluator] struct {
	newPolicyAccessor  func(P) PolicyAccessor
	newBindingAccessor func(B) BindingAccessor
	matcher            PolicyMatcher
	delegate           dispatcherDelegate[P, B, E]
}

func NewPolicyDispatcher[P runtime.Object, B runtime.Object, E Evaluator](
	newPolicyAccessor func(P) PolicyAccessor,
	newBindingAccessor func(B) BindingAccessor,
	matcher *matching.Matcher,
	delegate dispatcherDelegate[P, B, E],
) Dispatcher[PolicyHook[P, B, E]] {
	return &policyDispatcher[P, B, E]{
		newPolicyAccessor:  newPolicyAccessor,
		newBindingAccessor: newBindingAccessor,
		matcher:            NewPolicyMatcher(matcher),
		delegate:           delegate,
	}
}

// Dispatch implements generic.Dispatcher. It loops through all active hooks
// (policy x binding pairs) and selects those which are active for the current
// request. It then resolves all params and creates an Invocation for each
// matching policy-binding-param tuple. The delegate is then called with the
// list of tuples.
func (d *policyDispatcher[P, B, E]) Start(ctx context.Context) error {
	return nil
}

// Note: MatchConditions expressions are not evaluated here. The dispatcher delegate
// is expected to ignore the result of any policies whose match conditions don't pass.
// This may be possible to refactor so matchconditions are checked here instead.
func (d *policyDispatcher[P, B, E]) Dispatch(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces, hooks []PolicyHook[P, B, E]) error {
	var relevantHooks []PolicyInvocation[P, B, E]
	// Construct all the versions we need to call our webhooks
	versionedAttrAccessor := &versionedAttributeAccessor{
		versionedAttrs:   map[schema.GroupVersionKind]*admission.VersionedAttributes{},
		attr:             a,
		objectInterfaces: o,
	}

	var policyErrors []PolicyError
	addConfigError := func(err error, definition PolicyAccessor, binding BindingAccessor) {
		var message error
		if binding == nil {
			message = fmt.Errorf("failed to configure policy: %w", err)
		} else {
			message = fmt.Errorf("failed to configure binding: %w", err)
		}

		policyErrors = append(policyErrors, PolicyError{
			Policy:  definition,
			Binding: binding,
			Message: message,
		})
	}

	for _, hook := range hooks {
		policyAccessor := d.newPolicyAccessor(hook.Policy)
		matches, matchGVR, matchGVK, err := d.matcher.DefinitionMatches(a, o, policyAccessor)
		if err != nil {
			// There was an error evaluating if this policy matches anything.
			addConfigError(err, policyAccessor, nil)
			continue
		} else if !matches {
			continue
		} else if hook.ConfigurationError != nil {
			addConfigError(hook.ConfigurationError, policyAccessor, nil)
			continue
		}

		for _, binding := range hook.Bindings {
			bindingAccessor := d.newBindingAccessor(binding)
			matches, err = d.matcher.BindingMatches(a, o, bindingAccessor)
			if err != nil {
				// There was an error evaluating if this binding matches anything.
				addConfigError(err, policyAccessor, bindingAccessor)
				continue
			} else if !matches {
				continue
			}

			// here the binding matches.
			// VersionedAttr result will be cached and reused later during parallel
			// hook calls.
			if _, err = versionedAttrAccessor.VersionedAttribute(matchGVK); err != nil {
				// VersionedAttr result will be cached and reused later during parallel
				// hook calls.
				addConfigError(err, policyAccessor, nil)
				continue
			}

			// Collect params for this binding
			params, err := CollectParams(
				policyAccessor.GetParamKind(),
				hook.ParamInformer,
				hook.ParamScope,
				bindingAccessor.GetParamRef(),
				a.GetNamespace(),
			)
			if err != nil {
				// There was an error collecting params for this binding.
				addConfigError(err, policyAccessor, bindingAccessor)
				continue
			}

			// If params is empty and there was no error, that means that
			// ParamNotFoundAction is ignore, so it shouldn't be added to list
			for _, param := range params {
				relevantHooks = append(relevantHooks, PolicyInvocation[P, B, E]{
					Policy:    hook.Policy,
					Binding:   binding,
					Kind:      matchGVK,
					Resource:  matchGVR,
					Param:     param,
					Evaluator: hook.Evaluator,
				})
			}
		}
	}

	if len(relevantHooks) > 0 {
		extraPolicyErrors, statusError := d.delegate(ctx, a, o, versionedAttrAccessor, relevantHooks)
		if statusError != nil {
			return statusError
		}
		policyErrors = append(policyErrors, extraPolicyErrors...)
	}

	var filteredErrors []PolicyError
	for _, e := range policyErrors {
		// we always default the FailurePolicy if it is unset and validate it in API level
		var policy v1.FailurePolicyType
		if fp := e.Policy.GetFailurePolicy(); fp == nil {
			policy = v1.Fail
		} else {
			policy = *fp
		}

		switch policy {
		case v1.Ignore:
			// TODO: add metrics for ignored error here
			continue
		case v1.Fail:
			filteredErrors = append(filteredErrors, e)
		default:
			filteredErrors = append(filteredErrors, e)
		}
	}

	if len(filteredErrors) > 0 {

		forbiddenErr := admission.NewForbidden(a, fmt.Errorf("admission request denied by policy"))

		// The forbiddenErr is always a StatusError.
		var err *apierrors.StatusError
		if !errors.As(forbiddenErr, &err) {
			// Should never happen.
			return apierrors.NewInternalError(fmt.Errorf("failed to create status error"))
		}
		err.ErrStatus.Message = ""

		for _, policyError := range filteredErrors {
			message := policyError.Error()

			// If this is the first denied decision, use its message and reason
			// for the status error message.
			if err.ErrStatus.Message == "" {
				err.ErrStatus.Message = message
				if policyError.Reason != "" {
					err.ErrStatus.Reason = policyError.Reason
				}
			}

			// Add the denied decision's message to the status error's details
			err.ErrStatus.Details.Causes = append(
				err.ErrStatus.Details.Causes,
				metav1.StatusCause{Message: message})
		}

		return err
	}

	return nil
}

// Returns params to use to evaluate a policy-binding with given param
// configuration. If the policy-binding has no param configuration, it
// returns a single-element list with a nil param.
func CollectParams(
	paramKind *v1.ParamKind,
	paramInformer informers.GenericInformer,
	paramScope meta.RESTScope,
	paramRef *v1.ParamRef,
	namespace string,
) ([]runtime.Object, error) {
	// If definition has paramKind, paramRef is required in binding.
	// If definition has no paramKind, paramRef set in binding will be ignored.
	var params []runtime.Object
	var paramStore cache.GenericNamespaceLister

	// Make sure the param kind is ready to use
	if paramKind != nil && paramRef != nil {
		if paramInformer == nil {
			return nil, fmt.Errorf("paramKind kind `%v` not known",
				paramKind.String())
		}

		// Set up cluster-scoped, or namespaced access to the params
		// "default" if not provided, and paramKind is namespaced
		paramStore = paramInformer.Lister()
		if paramScope.Name() == meta.RESTScopeNameNamespace {
			paramsNamespace := namespace
			if len(paramRef.Namespace) > 0 {
				paramsNamespace = paramRef.Namespace
			} else if len(paramsNamespace) == 0 {
				// You must supply namespace if your matcher can possibly
				// match a cluster-scoped resource
				return nil, fmt.Errorf("cannot use namespaced paramRef in policy binding that matches cluster-scoped resources")
			}

			paramStore = paramInformer.Lister().ByNamespace(paramsNamespace)
		}

		// If the param informer for this admission policy has not yet
		// had time to perform an initial listing, don't attempt to use
		// it.
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		if !cache.WaitForCacheSync(timeoutCtx.Done(), paramInformer.Informer().HasSynced) {
			return nil, fmt.Errorf("paramKind kind `%v` not yet synced to use for admission",
				paramKind.String())
		}
	}

	// Find params to use with policy
	switch {
	case paramKind == nil:
		// ParamKind is unset. Ignore any globalParamRef or namespaceParamRef
		// setting.
		return []runtime.Object{nil}, nil
	case paramRef == nil:
		// Policy ParamKind is set, but binding does not use it.
		// Validate with nil params
		return []runtime.Object{nil}, nil
	case len(paramRef.Namespace) > 0 && paramScope.Name() == meta.RESTScopeRoot.Name():
		// Not allowed to set namespace for cluster-scoped param
		return nil, fmt.Errorf("paramRef.namespace must not be provided for a cluster-scoped `paramKind`")

	case len(paramRef.Name) > 0:
		if paramRef.Selector != nil {
			// This should be validated, but just in case.
			return nil, fmt.Errorf("paramRef.name and paramRef.selector are mutually exclusive")
		}

		switch param, err := paramStore.Get(paramRef.Name); {
		case err == nil:
			params = []runtime.Object{param}
		case apierrors.IsNotFound(err):
			// Param not yet available. User may need to wait a bit
			// before being able to use it for validation.
			//
			// Set params to nil to prepare for not found action
			params = nil
		case apierrors.IsInvalid(err):
			// Param mis-configured
			// require to set namespace for namespaced resource
			// and unset namespace for cluster scoped resource
			return nil, err
		default:
			// Internal error
			utilruntime.HandleError(err)
			return nil, err
		}
	case paramRef.Selector != nil:
		// Select everything by default if empty name and selector
		selector, err := metav1.LabelSelectorAsSelector(paramRef.Selector)
		if err != nil {
			// Cannot parse label selector: configuration error
			return nil, err

		}

		paramList, err := paramStore.List(selector)
		if err != nil {
			// There was a bad internal error
			utilruntime.HandleError(err)
			return nil, err
		}

		// Successfully grabbed params
		params = paramList
	default:
		// Should be unreachable due to validation
		return nil, fmt.Errorf("one of name or selector must be provided")
	}

	// Apply fail action for params not found case
	if len(params) == 0 && paramRef.ParameterNotFoundAction != nil && *paramRef.ParameterNotFoundAction == v1.DenyAction {
		return nil, errors.New("no params found for policy binding with `Deny` parameterNotFoundAction")
	}

	return params, nil
}

var _ webhookgeneric.VersionedAttributeAccessor = &versionedAttributeAccessor{}

type versionedAttributeAccessor struct {
	versionedAttrs   map[schema.GroupVersionKind]*admission.VersionedAttributes
	attr             admission.Attributes
	objectInterfaces admission.ObjectInterfaces
}

func (v *versionedAttributeAccessor) VersionedAttribute(gvk schema.GroupVersionKind) (*admission.VersionedAttributes, error) {
	if val, ok := v.versionedAttrs[gvk]; ok {
		return val, nil
	}
	versionedAttr, err := admission.NewVersionedAttributes(v.attr, gvk, v.objectInterfaces)
	if err != nil {
		return nil, err
	}
	v.versionedAttrs[gvk] = versionedAttr
	return versionedAttr, nil
}

type PolicyError struct {
	Policy  PolicyAccessor
	Binding BindingAccessor
	Message error
	Reason  metav1.StatusReason
}

func (c PolicyError) Error() string {
	if c.Binding != nil {
		return fmt.Sprintf("policy '%s' with binding '%s' denied request: %s", c.Policy.GetName(), c.Binding.GetName(), c.Message.Error())
	}

	return fmt.Sprintf("policy %q denied request: %s", c.Policy.GetName(), c.Message.Error())
}
