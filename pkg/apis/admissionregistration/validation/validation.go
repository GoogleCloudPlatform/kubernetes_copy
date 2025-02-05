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

package validation

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/equality"
	genericvalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	plugincel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/mutating/patch"
	validatingadmissionpolicy "k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	"k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/client-go/util/jsonpath"

	"k8s.io/kubernetes/pkg/apis/admissionregistration"
	admissionregistrationv1 "k8s.io/kubernetes/pkg/apis/admissionregistration/v1"
	admissionregistrationv1beta1 "k8s.io/kubernetes/pkg/apis/admissionregistration/v1beta1"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
)

func hasWildcard(slice []string) bool {
	for _, s := range slice {
		if s == "*" {
			return true
		}
	}
	return false
}

func validateResources(resources []string, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if len(resources) == 0 {
		allErrors = append(allErrors, field.Required(fldPath, ""))
	}

	// x/*
	resourcesWithWildcardSubresources := sets.New[string]()
	// */x
	subResourcesWithWildcardResource := sets.New[string]()
	// */*
	hasDoubleWildcard := false
	// *
	hasSingleWildcard := false
	// x
	hasResourceWithoutSubresource := false

	for i, resSub := range resources {
		if resSub == "" {
			allErrors = append(allErrors, field.Required(fldPath.Index(i), ""))
			continue
		}
		if resSub == "*/*" {
			hasDoubleWildcard = true
		}
		if resSub == "*" {
			hasSingleWildcard = true
		}
		parts := strings.SplitN(resSub, "/", 2)
		if len(parts) == 1 {
			hasResourceWithoutSubresource = resSub != "*"
			continue
		}
		res, sub := parts[0], parts[1]
		if _, ok := resourcesWithWildcardSubresources[res]; ok {
			allErrors = append(allErrors, field.Invalid(fldPath.Index(i), resSub, fmt.Sprintf("if '%s/*' is present, must not specify %s", res, resSub)))
		}
		if _, ok := subResourcesWithWildcardResource[sub]; ok {
			allErrors = append(allErrors, field.Invalid(fldPath.Index(i), resSub, fmt.Sprintf("if '*/%s' is present, must not specify %s", sub, resSub)))
		}
		if sub == "*" {
			resourcesWithWildcardSubresources.Insert(res)
		}
		if res == "*" {
			subResourcesWithWildcardResource.Insert(sub)
		}
	}
	if len(resources) > 1 && hasDoubleWildcard {
		allErrors = append(allErrors, field.Invalid(fldPath, resources, "if '*/*' is present, must not specify other resources"))
	}
	if hasSingleWildcard && hasResourceWithoutSubresource {
		allErrors = append(allErrors, field.Invalid(fldPath, resources, "if '*' is present, must not specify other resources without subresources"))
	}
	return allErrors
}

func validateResourcesNoSubResources(resources []string, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if len(resources) == 0 {
		allErrors = append(allErrors, field.Required(fldPath, ""))
	}
	for i, resource := range resources {
		if resource == "" {
			allErrors = append(allErrors, field.Required(fldPath.Index(i), ""))
		}
		if strings.Contains(resource, "/") {
			allErrors = append(allErrors, field.Invalid(fldPath.Index(i), resource, "must not specify subresources"))
		}
	}
	if len(resources) > 1 && hasWildcard(resources) {
		allErrors = append(allErrors, field.Invalid(fldPath, resources, "if '*' is present, must not specify other resources"))
	}
	return allErrors
}

var validScopes = sets.NewString(
	string(admissionregistration.ClusterScope),
	string(admissionregistration.NamespacedScope),
	string(admissionregistration.AllScopes),
)

func validateRule(rule *admissionregistration.Rule, fldPath *field.Path, allowSubResource bool) field.ErrorList {
	var allErrors field.ErrorList
	if len(rule.APIGroups) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("apiGroups"), ""))
	}
	if len(rule.APIGroups) > 1 && hasWildcard(rule.APIGroups) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("apiGroups"), rule.APIGroups, "if '*' is present, must not specify other API groups"))
	}
	// Note: group could be empty, e.g., the legacy "v1" API
	if len(rule.APIVersions) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("apiVersions"), ""))
	}
	if len(rule.APIVersions) > 1 && hasWildcard(rule.APIVersions) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("apiVersions"), rule.APIVersions, "if '*' is present, must not specify other API versions"))
	}
	for i, version := range rule.APIVersions {
		if version == "" {
			allErrors = append(allErrors, field.Required(fldPath.Child("apiVersions").Index(i), ""))
		}
	}
	if allowSubResource {
		allErrors = append(allErrors, validateResources(rule.Resources, fldPath.Child("resources"))...)
	} else {
		allErrors = append(allErrors, validateResourcesNoSubResources(rule.Resources, fldPath.Child("resources"))...)
	}
	if rule.Scope != nil && !validScopes.Has(string(*rule.Scope)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("scope"), *rule.Scope, validScopes.List()))
	}
	return allErrors
}

// AcceptedAdmissionReviewVersions contains the list of AdmissionReview versions the *prior* version of the API server understands.
// 1.15: server understands v1beta1; accepted versions are ["v1beta1"]
// 1.16: server understands v1, v1beta1; accepted versions are ["v1beta1"]
// 1.17+: server understands v1, v1beta1; accepted versions are ["v1","v1beta1"]
var AcceptedAdmissionReviewVersions = []string{admissionregistrationv1.SchemeGroupVersion.Version, admissionregistrationv1beta1.SchemeGroupVersion.Version}

func isAcceptedAdmissionReviewVersion(v string) bool {
	for _, version := range AcceptedAdmissionReviewVersions {
		if v == version {
			return true
		}
	}
	return false
}

func validateAdmissionReviewVersions(versions []string, requireRecognizedAdmissionReviewVersion bool, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}

	// Currently only v1beta1 accepted in AdmissionReviewVersions
	if len(versions) < 1 {
		allErrors = append(allErrors, field.Required(fldPath, fmt.Sprintf("must specify one of %v", strings.Join(AcceptedAdmissionReviewVersions, ", "))))
	} else {
		seen := map[string]bool{}
		hasAcceptedVersion := false
		for i, v := range versions {
			if seen[v] {
				allErrors = append(allErrors, field.Invalid(fldPath.Index(i), v, "duplicate version"))
				continue
			}
			seen[v] = true
			for _, errString := range utilvalidation.IsDNS1035Label(v) {
				allErrors = append(allErrors, field.Invalid(fldPath.Index(i), v, errString))
			}
			if isAcceptedAdmissionReviewVersion(v) {
				hasAcceptedVersion = true
			}
		}
		if requireRecognizedAdmissionReviewVersion && !hasAcceptedVersion {
			allErrors = append(allErrors, field.Invalid(
				fldPath, versions,
				fmt.Sprintf("must include at least one of %v",
					strings.Join(AcceptedAdmissionReviewVersions, ", "))))
		}
	}
	return allErrors
}

// ValidateValidatingWebhookConfiguration validates a webhook before creation.
func ValidateValidatingWebhookConfiguration(e *admissionregistration.ValidatingWebhookConfiguration) field.ErrorList {
	return validateValidatingWebhookConfiguration(e, validationOptions{
		ignoreMatchConditions:                   false,
		allowParamsInMatchConditions:            false,
		requireNoSideEffects:                    true,
		requireRecognizedAdmissionReviewVersion: true,
		requireUniqueWebhookNames:               true,
		allowInvalidLabelValueInSelector:        false,
		strictCostEnforcement:                   utilfeature.DefaultFeatureGate.Enabled(features.StrictCostEnforcementForWebhooks),
	})
}

func validateValidatingWebhookConfiguration(e *admissionregistration.ValidatingWebhookConfiguration, opts validationOptions) field.ErrorList {
	allErrors := genericvalidation.ValidateObjectMeta(&e.ObjectMeta, false, genericvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	hookNames := sets.NewString()
	for i, hook := range e.Webhooks {
		allErrors = append(allErrors, validateValidatingWebhook(&hook, opts, field.NewPath("webhooks").Index(i))...)
		allErrors = append(allErrors, validateAdmissionReviewVersions(hook.AdmissionReviewVersions, opts.requireRecognizedAdmissionReviewVersion, field.NewPath("webhooks").Index(i).Child("admissionReviewVersions"))...)
		if opts.requireUniqueWebhookNames && len(hook.Name) > 0 {
			if hookNames.Has(hook.Name) {
				allErrors = append(allErrors, field.Duplicate(field.NewPath("webhooks").Index(i).Child("name"), hook.Name))
			} else {
				hookNames.Insert(hook.Name)
			}
		}
	}
	return allErrors
}

// ValidateMutatingWebhookConfiguration validates a webhook before creation.
func ValidateMutatingWebhookConfiguration(e *admissionregistration.MutatingWebhookConfiguration) field.ErrorList {
	return validateMutatingWebhookConfiguration(e, validationOptions{
		ignoreMatchConditions:                   false,
		allowParamsInMatchConditions:            false,
		requireNoSideEffects:                    true,
		requireRecognizedAdmissionReviewVersion: true,
		requireUniqueWebhookNames:               true,
		allowInvalidLabelValueInSelector:        false,
		strictCostEnforcement:                   utilfeature.DefaultFeatureGate.Enabled(features.StrictCostEnforcementForWebhooks),
	})
}

type validationOptions struct {
	ignoreMatchConditions                   bool
	allowParamsInMatchConditions            bool
	requireNoSideEffects                    bool
	requireRecognizedAdmissionReviewVersion bool
	requireUniqueWebhookNames               bool
	allowInvalidLabelValueInSelector        bool
	preexistingExpressions                  preexistingExpressions
	strictCostEnforcement                   bool
}

type preexistingExpressions struct {
	matchConditionExpressions        sets.Set[string]
	validationExpressions            sets.Set[string]
	validationMessageExpressions     sets.Set[string]
	auditAnnotationValuesExpressions sets.Set[string]
	applyConfigurationExpressions    sets.Set[string]
	jsonPatchExpressions             sets.Set[string]
}

func newPreexistingExpressions() preexistingExpressions {
	return preexistingExpressions{
		matchConditionExpressions:        sets.New[string](),
		validationExpressions:            sets.New[string](),
		validationMessageExpressions:     sets.New[string](),
		auditAnnotationValuesExpressions: sets.New[string](),
		applyConfigurationExpressions:    sets.New[string](),
		jsonPatchExpressions:             sets.New[string](),
	}
}

func findMutatingPreexistingExpressions(mutating *admissionregistration.MutatingWebhookConfiguration) preexistingExpressions {
	preexisting := newPreexistingExpressions()
	for _, wh := range mutating.Webhooks {
		for _, mc := range wh.MatchConditions {
			preexisting.matchConditionExpressions.Insert(mc.Expression)
		}
	}
	return preexisting
}

func findValidatingPreexistingExpressions(validating *admissionregistration.ValidatingWebhookConfiguration) preexistingExpressions {
	preexisting := newPreexistingExpressions()
	for _, wh := range validating.Webhooks {
		for _, mc := range wh.MatchConditions {
			preexisting.matchConditionExpressions.Insert(mc.Expression)
		}
	}
	return preexisting
}

func findValidatingPolicyPreexistingExpressions(validatingPolicy *admissionregistration.ValidatingAdmissionPolicy) preexistingExpressions {
	preexisting := newPreexistingExpressions()
	for _, mc := range validatingPolicy.Spec.MatchConditions {
		preexisting.matchConditionExpressions.Insert(mc.Expression)
	}
	for _, v := range validatingPolicy.Spec.Validations {
		preexisting.validationExpressions.Insert(v.Expression)
		if len(v.MessageExpression) > 0 {
			preexisting.validationMessageExpressions.Insert(v.MessageExpression)
		}
	}
	for _, a := range validatingPolicy.Spec.AuditAnnotations {
		preexisting.auditAnnotationValuesExpressions.Insert(a.ValueExpression)
	}
	return preexisting
}

func findMutatingPolicyPreexistingExpressions(mutatingPolicy *admissionregistration.MutatingAdmissionPolicy) preexistingExpressions {
	preexisting := newPreexistingExpressions()
	for _, mc := range mutatingPolicy.Spec.MatchConditions {
		preexisting.matchConditionExpressions.Insert(mc.Expression)
	}
	for _, v := range mutatingPolicy.Spec.Mutations {
		if v.ApplyConfiguration != nil {
			preexisting.applyConfigurationExpressions.Insert(v.ApplyConfiguration.Expression)
		}
		if v.JSONPatch != nil {
			preexisting.jsonPatchExpressions.Insert(v.JSONPatch.Expression)
		}
	}
	return preexisting
}

func validateMutatingWebhookConfiguration(e *admissionregistration.MutatingWebhookConfiguration, opts validationOptions) field.ErrorList {
	allErrors := genericvalidation.ValidateObjectMeta(&e.ObjectMeta, false, genericvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))

	hookNames := sets.NewString()
	for i, hook := range e.Webhooks {
		allErrors = append(allErrors, validateMutatingWebhook(&hook, opts, field.NewPath("webhooks").Index(i))...)
		allErrors = append(allErrors, validateAdmissionReviewVersions(hook.AdmissionReviewVersions, opts.requireRecognizedAdmissionReviewVersion, field.NewPath("webhooks").Index(i).Child("admissionReviewVersions"))...)
		if opts.requireUniqueWebhookNames && len(hook.Name) > 0 {
			if hookNames.Has(hook.Name) {
				allErrors = append(allErrors, field.Duplicate(field.NewPath("webhooks").Index(i).Child("name"), hook.Name))
			} else {
				hookNames.Insert(hook.Name)
			}
		}
	}
	return allErrors
}

func validateValidatingWebhook(hook *admissionregistration.ValidatingWebhook, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	// hook.Name must be fully qualified
	allErrors = append(allErrors, utilvalidation.IsFullyQualifiedName(fldPath.Child("name"), hook.Name)...)
	labelSelectorValidationOpts := metav1validation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: opts.allowInvalidLabelValueInSelector,
	}

	for i, rule := range hook.Rules {
		allErrors = append(allErrors, validateRuleWithOperations(&rule, fldPath.Child("rules").Index(i))...)
	}
	if hook.FailurePolicy != nil && !supportedFailurePolicies.Has(string(*hook.FailurePolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("failurePolicy"), *hook.FailurePolicy, supportedFailurePolicies.List()))
	}
	if hook.MatchPolicy != nil && !supportedMatchPolicies.Has(string(*hook.MatchPolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("matchPolicy"), *hook.MatchPolicy, supportedMatchPolicies.List()))
	}
	allowedSideEffects := supportedSideEffectClasses
	if opts.requireNoSideEffects {
		allowedSideEffects = noSideEffectClasses
	}
	if hook.SideEffects == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("sideEffects"), fmt.Sprintf("must specify one of %v", strings.Join(allowedSideEffects.List(), ", "))))
	}
	if hook.SideEffects != nil && !allowedSideEffects.Has(string(*hook.SideEffects)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("sideEffects"), *hook.SideEffects, allowedSideEffects.List()))
	}
	if hook.TimeoutSeconds != nil && (*hook.TimeoutSeconds > 30 || *hook.TimeoutSeconds < 1) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("timeoutSeconds"), *hook.TimeoutSeconds, "the timeout value must be between 1 and 30 seconds"))
	}

	if hook.NamespaceSelector != nil {
		allErrors = append(allErrors, metav1validation.ValidateLabelSelector(hook.NamespaceSelector, labelSelectorValidationOpts, fldPath.Child("namespaceSelector"))...)
	}

	if hook.ObjectSelector != nil {
		allErrors = append(allErrors, metav1validation.ValidateLabelSelector(hook.ObjectSelector, labelSelectorValidationOpts, fldPath.Child("objectSelector"))...)
	}

	cc := hook.ClientConfig
	switch {
	case (cc.URL == nil) == (cc.Service == nil):
		allErrors = append(allErrors, field.Required(fldPath.Child("clientConfig"), "exactly one of url or service is required"))
	case cc.URL != nil:
		allErrors = append(allErrors, webhook.ValidateWebhookURL(fldPath.Child("clientConfig").Child("url"), *cc.URL, true)...)
	case cc.Service != nil:
		allErrors = append(allErrors, webhook.ValidateWebhookService(fldPath.Child("clientConfig").Child("service"), cc.Service.Name, cc.Service.Namespace, cc.Service.Path, cc.Service.Port)...)
	}

	if !opts.ignoreMatchConditions {
		allErrors = append(allErrors, validateMatchConditions(hook.MatchConditions, opts, fldPath.Child("matchConditions"))...)
	}

	return allErrors
}

func validateMutatingWebhook(hook *admissionregistration.MutatingWebhook, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	// hook.Name must be fully qualified
	allErrors = append(allErrors, utilvalidation.IsFullyQualifiedName(fldPath.Child("name"), hook.Name)...)
	labelSelectorValidationOpts := metav1validation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: opts.allowInvalidLabelValueInSelector,
	}

	for i, rule := range hook.Rules {
		allErrors = append(allErrors, validateRuleWithOperations(&rule, fldPath.Child("rules").Index(i))...)
	}
	if hook.FailurePolicy != nil && !supportedFailurePolicies.Has(string(*hook.FailurePolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("failurePolicy"), *hook.FailurePolicy, supportedFailurePolicies.List()))
	}
	if hook.MatchPolicy != nil && !supportedMatchPolicies.Has(string(*hook.MatchPolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("matchPolicy"), *hook.MatchPolicy, supportedMatchPolicies.List()))
	}
	allowedSideEffects := supportedSideEffectClasses
	if opts.requireNoSideEffects {
		allowedSideEffects = noSideEffectClasses
	}
	if hook.SideEffects == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("sideEffects"), fmt.Sprintf("must specify one of %v", strings.Join(allowedSideEffects.List(), ", "))))
	}
	if hook.SideEffects != nil && !allowedSideEffects.Has(string(*hook.SideEffects)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("sideEffects"), *hook.SideEffects, allowedSideEffects.List()))
	}
	if hook.TimeoutSeconds != nil && (*hook.TimeoutSeconds > 30 || *hook.TimeoutSeconds < 1) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("timeoutSeconds"), *hook.TimeoutSeconds, "the timeout value must be between 1 and 30 seconds"))
	}

	if hook.NamespaceSelector != nil {
		allErrors = append(allErrors, metav1validation.ValidateLabelSelector(hook.NamespaceSelector, labelSelectorValidationOpts, fldPath.Child("namespaceSelector"))...)
	}
	if hook.ObjectSelector != nil {
		allErrors = append(allErrors, metav1validation.ValidateLabelSelector(hook.ObjectSelector, labelSelectorValidationOpts, fldPath.Child("objectSelector"))...)
	}
	if hook.ReinvocationPolicy != nil && !supportedReinvocationPolicies.Has(string(*hook.ReinvocationPolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("reinvocationPolicy"), *hook.ReinvocationPolicy, supportedReinvocationPolicies.List()))
	}

	cc := hook.ClientConfig
	switch {
	case (cc.URL == nil) == (cc.Service == nil):
		allErrors = append(allErrors, field.Required(fldPath.Child("clientConfig"), "exactly one of url or service is required"))
	case cc.URL != nil:
		allErrors = append(allErrors, webhook.ValidateWebhookURL(fldPath.Child("clientConfig").Child("url"), *cc.URL, true)...)
	case cc.Service != nil:
		allErrors = append(allErrors, webhook.ValidateWebhookService(fldPath.Child("clientConfig").Child("service"), cc.Service.Name, cc.Service.Namespace, cc.Service.Path, cc.Service.Port)...)
	}

	if !opts.ignoreMatchConditions {
		allErrors = append(allErrors, validateMatchConditions(hook.MatchConditions, opts, fldPath.Child("matchConditions"))...)
	}

	return allErrors
}

var supportedFailurePolicies = sets.NewString(
	string(admissionregistration.Ignore),
	string(admissionregistration.Fail),
)

var supportedMatchPolicies = sets.NewString(
	string(admissionregistration.Exact),
	string(admissionregistration.Equivalent),
)

var supportedSideEffectClasses = sets.NewString(
	string(admissionregistration.SideEffectClassUnknown),
	string(admissionregistration.SideEffectClassNone),
	string(admissionregistration.SideEffectClassSome),
	string(admissionregistration.SideEffectClassNoneOnDryRun),
)

var noSideEffectClasses = sets.NewString(
	string(admissionregistration.SideEffectClassNone),
	string(admissionregistration.SideEffectClassNoneOnDryRun),
)

var supportedOperations = sets.NewString(
	string(admissionregistration.OperationAll),
	string(admissionregistration.Create),
	string(admissionregistration.Update),
	string(admissionregistration.Delete),
	string(admissionregistration.Connect),
)

var supportedReinvocationPolicies = sets.NewString(
	string(admissionregistration.NeverReinvocationPolicy),
	string(admissionregistration.IfNeededReinvocationPolicy),
)

var supportedValidationPolicyReason = sets.NewString(
	string(metav1.StatusReasonForbidden),
	string(metav1.StatusReasonInvalid),
	string(metav1.StatusReasonRequestEntityTooLarge),
)

var supportedPatchType = sets.NewString(
	string(admissionregistration.PatchTypeApplyConfiguration),
	string(admissionregistration.PatchTypeJSONPatch),
)

// MutatatingAdmissionPolicy does not support DELETE
var supportedMutatingOperations = sets.NewString(
	string(admissionregistration.OperationAll),
	string(admissionregistration.Create),
	string(admissionregistration.Update),
	string(admissionregistration.Connect),
)

func hasWildcardOperation(operations []admissionregistration.OperationType) bool {
	for _, o := range operations {
		if o == admissionregistration.OperationAll {
			return true
		}
	}
	return false
}

func validateRuleWithOperations(ruleWithOperations *admissionregistration.RuleWithOperations, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if len(ruleWithOperations.Operations) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("operations"), ""))
	}
	if len(ruleWithOperations.Operations) > 1 && hasWildcardOperation(ruleWithOperations.Operations) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("operations"), ruleWithOperations.Operations, "if '*' is present, must not specify other operations"))
	}
	for i, operation := range ruleWithOperations.Operations {
		if !supportedOperations.Has(string(operation)) {
			allErrors = append(allErrors, field.NotSupported(fldPath.Child("operations").Index(i), operation, supportedOperations.List()))
		}
	}
	allowSubResource := true
	allErrors = append(allErrors, validateRule(&ruleWithOperations.Rule, fldPath, allowSubResource)...)
	return allErrors
}

// mutatingHasAcceptedAdmissionReviewVersions returns true if all webhooks have at least one
// admission review version this apiserver accepts.
func mutatingHasAcceptedAdmissionReviewVersions(webhooks []admissionregistration.MutatingWebhook) bool {
	for _, hook := range webhooks {
		hasRecognizedVersion := false
		for _, version := range hook.AdmissionReviewVersions {
			if isAcceptedAdmissionReviewVersion(version) {
				hasRecognizedVersion = true
				break
			}
		}
		if !hasRecognizedVersion && len(hook.AdmissionReviewVersions) > 0 {
			return false
		}
	}
	return true
}

// validatingHasAcceptedAdmissionReviewVersions returns true if all webhooks have at least one
// admission review version this apiserver accepts.
func validatingHasAcceptedAdmissionReviewVersions(webhooks []admissionregistration.ValidatingWebhook) bool {
	for _, hook := range webhooks {
		hasRecognizedVersion := false
		for _, version := range hook.AdmissionReviewVersions {
			if isAcceptedAdmissionReviewVersion(version) {
				hasRecognizedVersion = true
				break
			}
		}
		if !hasRecognizedVersion && len(hook.AdmissionReviewVersions) > 0 {
			return false
		}
	}
	return true
}

// ignoreMatchConditions returns false if any change to match conditions
func ignoreMutatingWebhookMatchConditions(new, old []admissionregistration.MutatingWebhook) bool {
	if len(new) != len(old) {
		return false
	}
	for i := range old {
		if !reflect.DeepEqual(new[i].MatchConditions, old[i].MatchConditions) {
			return false
		}
	}

	return true
}

// ignoreMatchConditions returns true if any new expressions are added
func ignoreValidatingWebhookMatchConditions(new, old []admissionregistration.ValidatingWebhook) bool {
	if len(new) != len(old) {
		return false
	}
	for i := range old {
		if !reflect.DeepEqual(new[i].MatchConditions, old[i].MatchConditions) {
			return false
		}
	}

	return true
}

// ignoreValidatingAdmissionPolicyMatchConditions returns true if there have been no updates that could invalidate previously-valid match conditions
func ignoreValidatingAdmissionPolicyMatchConditions(new, old *admissionregistration.ValidatingAdmissionPolicy) bool {
	if !equality.Semantic.DeepEqual(new.Spec.ParamKind, old.Spec.ParamKind) {
		return false
	}
	if !equality.Semantic.DeepEqual(new.Spec.MatchConditions, old.Spec.MatchConditions) {
		return false
	}
	return true
}

// ignoreMutatingAdmissionPolicyMatchConditions returns true if there have been no updates that could invalidate previously-valid match conditions
func ignoreMutatingAdmissionPolicyMatchConditions(new, old *admissionregistration.MutatingAdmissionPolicy) bool {
	if !equality.Semantic.DeepEqual(new.Spec.ParamKind, old.Spec.ParamKind) {
		return false
	}
	if !equality.Semantic.DeepEqual(new.Spec.MatchConditions, old.Spec.MatchConditions) {
		return false
	}
	return true
}

// mutatingHasUniqueWebhookNames returns true if all webhooks have unique names
func mutatingHasUniqueWebhookNames(webhooks []admissionregistration.MutatingWebhook) bool {
	names := sets.NewString()
	for _, hook := range webhooks {
		if names.Has(hook.Name) {
			return false
		}
		names.Insert(hook.Name)
	}
	return true
}

// validatingHasUniqueWebhookNames returns true if all webhooks have unique names
func validatingHasUniqueWebhookNames(webhooks []admissionregistration.ValidatingWebhook) bool {
	names := sets.NewString()
	for _, hook := range webhooks {
		if names.Has(hook.Name) {
			return false
		}
		names.Insert(hook.Name)
	}
	return true
}

// mutatingHasNoSideEffects returns true if all webhooks have no side effects
func mutatingHasNoSideEffects(webhooks []admissionregistration.MutatingWebhook) bool {
	for _, hook := range webhooks {
		if hook.SideEffects == nil || !noSideEffectClasses.Has(string(*hook.SideEffects)) {
			return false
		}
	}
	return true
}

// validatingHasNoSideEffects returns true if all webhooks have no side effects
func validatingHasNoSideEffects(webhooks []admissionregistration.ValidatingWebhook) bool {
	for _, hook := range webhooks {
		if hook.SideEffects == nil || !noSideEffectClasses.Has(string(*hook.SideEffects)) {
			return false
		}
	}
	return true
}

// validatingWebhookAllowInvalidLabelValueInSelector returns true if all webhooksallow invalid label value in selector
func validatingWebhookHasInvalidLabelValueInSelector(webhooks []admissionregistration.ValidatingWebhook) bool {
	labelSelectorValidationOpts := metav1validation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: false,
	}

	for _, hook := range webhooks {
		if hook.NamespaceSelector != nil {
			if len(metav1validation.ValidateLabelSelector(hook.NamespaceSelector, labelSelectorValidationOpts, nil)) > 0 {
				return true
			}
		}
		if hook.ObjectSelector != nil {
			if len(metav1validation.ValidateLabelSelector(hook.ObjectSelector, labelSelectorValidationOpts, nil)) > 0 {
				return true
			}
		}
	}
	return false
}

// mutatingWebhookAllowInvalidLabelValueInSelector returns true if all webhooks allow invalid label value in selector
func mutatingWebhookHasInvalidLabelValueInSelector(webhooks []admissionregistration.MutatingWebhook) bool {
	labelSelectorValidationOpts := metav1validation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: false,
	}

	for _, hook := range webhooks {
		if hook.NamespaceSelector != nil {
			if len(metav1validation.ValidateLabelSelector(hook.NamespaceSelector, labelSelectorValidationOpts, nil)) > 0 {
				return true
			}
		}
		if hook.ObjectSelector != nil {
			if len(metav1validation.ValidateLabelSelector(hook.ObjectSelector, labelSelectorValidationOpts, nil)) > 0 {
				return true
			}
		}
	}
	return false
}

// ValidateValidatingWebhookConfigurationUpdate validates update of validating webhook configuration
func ValidateValidatingWebhookConfigurationUpdate(newC, oldC *admissionregistration.ValidatingWebhookConfiguration) field.ErrorList {
	return validateValidatingWebhookConfiguration(newC, validationOptions{
		ignoreMatchConditions:                   ignoreValidatingWebhookMatchConditions(newC.Webhooks, oldC.Webhooks),
		allowParamsInMatchConditions:            false,
		requireNoSideEffects:                    validatingHasNoSideEffects(oldC.Webhooks),
		requireRecognizedAdmissionReviewVersion: validatingHasAcceptedAdmissionReviewVersions(oldC.Webhooks),
		requireUniqueWebhookNames:               validatingHasUniqueWebhookNames(oldC.Webhooks),
		allowInvalidLabelValueInSelector:        validatingWebhookHasInvalidLabelValueInSelector(oldC.Webhooks),
		preexistingExpressions:                  findValidatingPreexistingExpressions(oldC),
		strictCostEnforcement:                   utilfeature.DefaultFeatureGate.Enabled(features.StrictCostEnforcementForWebhooks),
	})
}

// ValidateMutatingWebhookConfigurationUpdate validates update of mutating webhook configuration
func ValidateMutatingWebhookConfigurationUpdate(newC, oldC *admissionregistration.MutatingWebhookConfiguration) field.ErrorList {
	return validateMutatingWebhookConfiguration(newC, validationOptions{
		ignoreMatchConditions:                   ignoreMutatingWebhookMatchConditions(newC.Webhooks, oldC.Webhooks),
		allowParamsInMatchConditions:            false,
		requireNoSideEffects:                    mutatingHasNoSideEffects(oldC.Webhooks),
		requireRecognizedAdmissionReviewVersion: mutatingHasAcceptedAdmissionReviewVersions(oldC.Webhooks),
		requireUniqueWebhookNames:               mutatingHasUniqueWebhookNames(oldC.Webhooks),
		allowInvalidLabelValueInSelector:        mutatingWebhookHasInvalidLabelValueInSelector(oldC.Webhooks),
		preexistingExpressions:                  findMutatingPreexistingExpressions(oldC),
		strictCostEnforcement:                   utilfeature.DefaultFeatureGate.Enabled(features.StrictCostEnforcementForWebhooks),
	})
}

const (
	maxAuditAnnotations = 20
	// use a 5kb limit the CEL expression, note that this is less than the length limit
	// for the audit annotation value limit (10kb) since an expressions that concatenates
	// strings will often produce a longer value than the expression
	maxAuditAnnotationValueExpressionLength = 5 * 1024
)

// ValidateValidatingAdmissionPolicy validates a ValidatingAdmissionPolicy before creation.
func ValidateValidatingAdmissionPolicy(p *admissionregistration.ValidatingAdmissionPolicy) field.ErrorList {
	return validateValidatingAdmissionPolicy(p, validationOptions{ignoreMatchConditions: false, strictCostEnforcement: utilfeature.DefaultFeatureGate.Enabled(features.StrictCostEnforcementForVAP)})
}

func validateValidatingAdmissionPolicy(p *admissionregistration.ValidatingAdmissionPolicy, opts validationOptions) field.ErrorList {
	allErrors := genericvalidation.ValidateObjectMeta(&p.ObjectMeta, false, genericvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	allErrors = append(allErrors, validateValidatingAdmissionPolicySpec(p.ObjectMeta, &p.Spec, opts, field.NewPath("spec"))...)
	return allErrors
}

func validateValidatingAdmissionPolicySpec(meta metav1.ObjectMeta, spec *admissionregistration.ValidatingAdmissionPolicySpec, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	var compiler plugincel.Compiler // composition compiler is stateful, create one lazily per policy
	getCompiler := func() plugincel.Compiler {
		if compiler == nil {
			needsComposition := len(spec.Variables) > 0
			compiler = createCompiler(needsComposition, opts.strictCostEnforcement)
		}
		return compiler
	}
	if spec.FailurePolicy == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("failurePolicy"), ""))
	} else if !supportedFailurePolicies.Has(string(*spec.FailurePolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("failurePolicy"), *spec.FailurePolicy, supportedFailurePolicies.List()))
	}
	if spec.ParamKind != nil {
		opts.allowParamsInMatchConditions = true
		allErrors = append(allErrors, validateParamKind(*spec.ParamKind, fldPath.Child("paramKind"))...)
	}
	if spec.MatchConstraints == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("matchConstraints"), ""))
	} else {
		allErrors = append(allErrors, validateMatchResources(spec.MatchConstraints, fldPath.Child("matchConstraints"))...)
		// at least one resourceRule must be defined to provide type information
		if len(spec.MatchConstraints.ResourceRules) == 0 {
			allErrors = append(allErrors, field.Required(fldPath.Child("matchConstraints", "resourceRules"), ""))
		}
	}
	if !opts.ignoreMatchConditions {
		allErrors = append(allErrors, validateMatchConditions(spec.MatchConditions, opts, fldPath.Child("matchConditions"))...)
	}
	if len(spec.Variables) > 0 {
		for i, variable := range spec.Variables {
			allErrors = append(allErrors, validateVariable(getCompiler(), &variable, spec.ParamKind, opts, fldPath.Child("variables").Index(i))...)
		}
	}
	if len(spec.Validations) == 0 && len(spec.AuditAnnotations) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("validations"), "validations or auditAnnotations must contain at least one item"))
		allErrors = append(allErrors, field.Required(fldPath.Child("auditAnnotations"), "validations or auditAnnotations must contain at least one item"))
	} else {
		for i, validation := range spec.Validations {
			allErrors = append(allErrors, validateValidation(getCompiler(), &validation, spec.ParamKind, opts, fldPath.Child("validations").Index(i))...)
		}
		if spec.AuditAnnotations != nil {
			keys := sets.NewString()
			if len(spec.AuditAnnotations) > maxAuditAnnotations {
				allErrors = append(allErrors, field.Invalid(fldPath.Child("auditAnnotations"), spec.AuditAnnotations, fmt.Sprintf("must not have more than %d auditAnnotations", maxAuditAnnotations)))
			}
			for i, auditAnnotation := range spec.AuditAnnotations {
				allErrors = append(allErrors, validateAuditAnnotation(getCompiler(), meta, &auditAnnotation, spec.ParamKind, opts, fldPath.Child("auditAnnotations").Index(i))...)
				if keys.Has(auditAnnotation.Key) {
					allErrors = append(allErrors, field.Duplicate(fldPath.Child("auditAnnotations").Index(i).Child("key"), auditAnnotation.Key))
				}
				keys.Insert(auditAnnotation.Key)
			}
		}
	}
	return allErrors
}

func validateParamKind(gvk admissionregistration.ParamKind, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if len(gvk.APIVersion) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("apiVersion"), ""))
	} else if gv, err := parseGroupVersion(gvk.APIVersion); err != nil {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("apiVersion"), gvk.APIVersion, err.Error()))
	} else {
		// this matches the APIService group field validation
		if len(gv.Group) > 0 {
			if errs := utilvalidation.IsDNS1123Subdomain(gv.Group); len(errs) > 0 {
				allErrors = append(allErrors, field.Invalid(fldPath.Child("apiVersion"), gv.Group, strings.Join(errs, ",")))
			}
		}
		// this matches the APIService version field validation
		if len(gv.Version) == 0 {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("apiVersion"), gvk.APIVersion, "version must be specified"))
		} else {
			if errs := utilvalidation.IsDNS1035Label(gv.Version); len(errs) > 0 {
				allErrors = append(allErrors, field.Invalid(fldPath.Child("apiVersion"), gv.Version, strings.Join(errs, ",")))
			}
		}
	}
	if len(gvk.Kind) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("kind"), ""))
	} else if errs := utilvalidation.IsDNS1035Label(strings.ToLower(gvk.Kind)); len(errs) > 0 {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("kind"), gvk.Kind, "may have mixed case, but should otherwise match: "+strings.Join(errs, ",")))
	}

	return allErrors
}

type groupVersion struct {
	Group   string
	Version string
}

// parseGroupVersion turns "group/version" string into a groupVersion struct. It reports error
// if it cannot parse the string.
func parseGroupVersion(gv string) (groupVersion, error) {
	if (len(gv) == 0) || (gv == "/") {
		return groupVersion{}, nil
	}

	switch strings.Count(gv, "/") {
	case 0:
		return groupVersion{"", gv}, nil
	case 1:
		i := strings.Index(gv, "/")
		return groupVersion{gv[:i], gv[i+1:]}, nil
	default:
		return groupVersion{}, fmt.Errorf("unexpected GroupVersion string: %v", gv)
	}
}

func validateMatchResources(mc *admissionregistration.MatchResources, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if mc == nil {
		return allErrors
	}
	if mc.MatchPolicy == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("matchPolicy"), ""))
	} else if !supportedMatchPolicies.Has(string(*mc.MatchPolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("matchPolicy"), *mc.MatchPolicy, supportedMatchPolicies.List()))
	}
	if mc.NamespaceSelector == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("namespaceSelector"), ""))
	} else {
		// validate selector strictly, this type was released after issue #99139 was resolved
		allErrors = append(allErrors, metav1validation.ValidateLabelSelector(mc.NamespaceSelector, metav1validation.LabelSelectorValidationOptions{}, fldPath.Child("namespaceSelector"))...)
	}

	if mc.ObjectSelector == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("objectSelector"), ""))
	} else {
		// validate selector strictly, this type was released after issue #99139 was resolved
		allErrors = append(allErrors, metav1validation.ValidateLabelSelector(mc.ObjectSelector, metav1validation.LabelSelectorValidationOptions{}, fldPath.Child("objectSelector"))...)
	}

	for i, namedRuleWithOperations := range mc.ResourceRules {
		allErrors = append(allErrors, validateNamedRuleWithOperations(&namedRuleWithOperations, fldPath.Child("resourceRules").Index(i))...)
	}

	for i, namedRuleWithOperations := range mc.ExcludeResourceRules {
		allErrors = append(allErrors, validateNamedRuleWithOperations(&namedRuleWithOperations, fldPath.Child("excludeResourceRules").Index(i))...)
	}
	return allErrors
}

var validValidationActions = sets.NewString(
	string(admissionregistration.Deny),
	string(admissionregistration.Warn),
	string(admissionregistration.Audit),
)

func validateValidationActions(va []admissionregistration.ValidationAction, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	actions := sets.NewString()
	for i, action := range va {
		if !validValidationActions.Has(string(action)) {
			allErrors = append(allErrors, field.NotSupported(fldPath.Index(i), action, validValidationActions.List()))
		}
		if actions.Has(string(action)) {
			allErrors = append(allErrors, field.Duplicate(fldPath.Index(i), action))
		}
		actions.Insert(string(action))
	}
	if actions.Has(string(admissionregistration.Deny)) && actions.Has(string(admissionregistration.Warn)) {
		allErrors = append(allErrors, field.Invalid(fldPath, va, "must not contain both Deny and Warn (repeating the same validation failure information in the API response and headers serves no purpose)"))
	}
	if len(actions) == 0 {
		allErrors = append(allErrors, field.Required(fldPath, "at least one validation action is required"))
	}
	return allErrors
}

func validateNamedRuleWithOperations(n *admissionregistration.NamedRuleWithOperations, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	resourceNames := sets.NewString()
	for i, rName := range n.ResourceNames {
		for _, msg := range path.ValidatePathSegmentName(rName, false) {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("resourceNames").Index(i), rName, msg))
		}
		if resourceNames.Has(rName) {
			allErrors = append(allErrors, field.Duplicate(fldPath.Child("resourceNames").Index(i), rName))
		} else {
			resourceNames.Insert(rName)
		}
	}
	allErrors = append(allErrors, validateRuleWithOperations(&n.RuleWithOperations, fldPath)...)
	return allErrors
}

func validateMatchConditions(m []admissionregistration.MatchCondition, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	conditionNames := sets.NewString()
	if len(m) > 64 {
		allErrors = append(allErrors, field.TooMany(fldPath, len(m), 64))
	}
	for i, matchCondition := range m {
		allErrors = append(allErrors, validateMatchCondition(&matchCondition, opts, fldPath.Index(i))...)
		if len(matchCondition.Name) > 0 {
			if conditionNames.Has(matchCondition.Name) {
				allErrors = append(allErrors, field.Duplicate(fldPath.Index(i).Child("name"), matchCondition.Name))
			} else {
				conditionNames.Insert(matchCondition.Name)
			}
		}
	}
	return allErrors
}

func validateMatchCondition(v *admissionregistration.MatchCondition, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	trimmedExpression := strings.TrimSpace(v.Expression)
	if len(trimmedExpression) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("expression"), ""))
	} else {
		allErrors = append(allErrors, validateMatchConditionsExpression(trimmedExpression, opts, fldPath.Child("expression"))...)
	}
	if len(v.Name) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("name"), ""))
	} else {
		allErrors = append(allErrors, apivalidation.ValidateQualifiedName(v.Name, fldPath.Child("name"))...)
	}
	return allErrors
}

func validateVariable(compiler plugincel.Compiler, v *admissionregistration.Variable, paramKind *admissionregistration.ParamKind, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if len(v.Name) == 0 || strings.TrimSpace(v.Name) == "" {
		allErrors = append(allErrors, field.Required(fldPath.Child("name"), "name is not specified"))
	} else {
		if !isCELIdentifier(v.Name) {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("name"), v.Name, "name is not a valid CEL identifier"))
		}
	}
	if len(v.Expression) == 0 || strings.TrimSpace(v.Expression) == "" {
		allErrors = append(allErrors, field.Required(fldPath.Child("expression"), "expression is not specified"))
	} else {
		if compiler, ok := compiler.(*plugincel.CompositedCompiler); ok {
			envType := environment.NewExpressions
			if opts.preexistingExpressions.validationExpressions.Has(v.Expression) {
				envType = environment.StoredExpressions
			}
			variable := &validatingadmissionpolicy.Variable{
				Name:       v.Name,
				Expression: v.Expression,
			}
			result := compiler.CompileAndStoreVariable(variable, plugincel.OptionalVariableDeclarations{
				HasParams:     paramKind != nil,
				HasAuthorizer: true,
				StrictCost:    opts.strictCostEnforcement,
			}, envType)
			if result.Error != nil {
				allErrors = append(allErrors, convertCELErrorToValidationError(fldPath.Child("expression"), variable, result.Error))
			}
		} else {
			allErrors = append(allErrors, field.InternalError(fldPath, fmt.Errorf("variable composition is not allowed")))
		}
	}
	return allErrors
}

func validateValidation(compiler plugincel.Compiler, v *admissionregistration.Validation, paramKind *admissionregistration.ParamKind, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	trimmedExpression := strings.TrimSpace(v.Expression)
	trimmedMsg := strings.TrimSpace(v.Message)
	trimmedMessageExpression := strings.TrimSpace(v.MessageExpression)
	if len(trimmedExpression) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("expression"), "expression is not specified"))
	} else {
		allErrors = append(allErrors, validateValidationExpression(compiler, v.Expression, paramKind != nil, opts, fldPath.Child("expression"))...)
	}
	if len(v.MessageExpression) > 0 && len(trimmedMessageExpression) == 0 {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("messageExpression"), v.MessageExpression, "must be non-empty if specified"))
	} else if len(trimmedMessageExpression) != 0 {
		// use v.MessageExpression instead of trimmedMessageExpression so that
		// the compiler output shows the correct column.
		allErrors = append(allErrors, validateMessageExpression(compiler, v.MessageExpression, opts, fldPath.Child("messageExpression"))...)
	}
	if len(v.Message) > 0 && len(trimmedMsg) == 0 {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("message"), v.Message, "message must be non-empty if specified"))
	} else if hasNewlines(trimmedMsg) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("message"), v.Message, "message must not contain line breaks"))
	} else if hasNewlines(trimmedMsg) && trimmedMsg == "" {
		allErrors = append(allErrors, field.Required(fldPath.Child("message"), "message must be specified if expression contains line breaks"))
	}
	if v.Reason != nil && !supportedValidationPolicyReason.Has(string(*v.Reason)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("reason"), *v.Reason, supportedValidationPolicyReason.List()))
	}
	return allErrors
}

func validateCELCondition(compiler plugincel.Compiler, expression plugincel.ExpressionAccessor, variables plugincel.OptionalVariableDeclarations, envType environment.Type, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	result := compiler.CompileCELExpression(expression, variables, envType)
	if result.Error != nil {
		allErrors = append(allErrors, convertCELErrorToValidationError(fldPath, expression, result.Error))
	}
	return allErrors
}

func convertCELErrorToValidationError(fldPath *field.Path, expression plugincel.ExpressionAccessor, err error) *field.Error {
	if celErr, ok := err.(*cel.Error); ok {
		switch celErr.Type {
		case cel.ErrorTypeRequired:
			return field.Required(fldPath, celErr.Detail)
		case cel.ErrorTypeInvalid:
			return field.Invalid(fldPath, expression.GetExpression(), celErr.Detail)
		case cel.ErrorTypeInternal:
			return field.InternalError(fldPath, celErr)
		}
	}
	return field.InternalError(fldPath, fmt.Errorf("unsupported error type: %w", err))
}

func validateValidationExpression(compiler plugincel.Compiler, expression string, hasParams bool, opts validationOptions, fldPath *field.Path) field.ErrorList {
	envType := environment.NewExpressions
	if opts.preexistingExpressions.validationExpressions.Has(expression) {
		envType = environment.StoredExpressions
	}
	return validateCELCondition(compiler, &validatingadmissionpolicy.ValidationCondition{
		Expression: expression,
	}, plugincel.OptionalVariableDeclarations{
		HasParams:     hasParams,
		HasAuthorizer: true,
		StrictCost:    opts.strictCostEnforcement,
	}, envType, fldPath)
}

func validateMatchConditionsExpression(expression string, opts validationOptions, fldPath *field.Path) field.ErrorList {
	envType := environment.NewExpressions
	if opts.preexistingExpressions.matchConditionExpressions.Has(expression) {
		envType = environment.StoredExpressions
	}
	var compiler plugincel.Compiler
	if opts.strictCostEnforcement {
		compiler = getStrictStatelessCELCompiler()
	} else {
		compiler = getNonStrictStatelessCELCompiler()
	}
	return validateCELCondition(compiler, &matchconditions.MatchCondition{
		Expression: expression,
	}, plugincel.OptionalVariableDeclarations{
		HasParams:     opts.allowParamsInMatchConditions,
		HasAuthorizer: true,
		StrictCost:    opts.strictCostEnforcement,
	}, envType, fldPath)
}

func validateMessageExpression(compiler plugincel.Compiler, expression string, opts validationOptions, fldPath *field.Path) field.ErrorList {
	envType := environment.NewExpressions
	if opts.preexistingExpressions.validationMessageExpressions.Has(expression) {
		envType = environment.StoredExpressions
	}
	return validateCELCondition(compiler, &validatingadmissionpolicy.MessageExpressionCondition{
		MessageExpression: expression,
	}, plugincel.OptionalVariableDeclarations{
		HasParams:     opts.allowParamsInMatchConditions,
		HasAuthorizer: false,
		StrictCost:    opts.strictCostEnforcement,
	}, envType, fldPath)
}

func validateAuditAnnotation(compiler plugincel.Compiler, meta metav1.ObjectMeta, v *admissionregistration.AuditAnnotation, paramKind *admissionregistration.ParamKind, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if len(meta.GetName()) != 0 {
		name := meta.GetName()
		allErrors = append(allErrors, apivalidation.ValidateQualifiedName(name+"/"+v.Key, fldPath.Child("key"))...)
	} else {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("key"), v.Key, "requires metadata.name be non-empty"))
	}

	trimmedValueExpression := strings.TrimSpace(v.ValueExpression)
	if len(trimmedValueExpression) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("valueExpression"), "valueExpression is not specified"))
	} else if len(trimmedValueExpression) > maxAuditAnnotationValueExpressionLength {
		allErrors = append(allErrors, field.Required(fldPath.Child("valueExpression"), fmt.Sprintf("must not exceed %d bytes in length", maxAuditAnnotationValueExpressionLength)))
	} else {
		envType := environment.NewExpressions
		if opts.preexistingExpressions.auditAnnotationValuesExpressions.Has(v.ValueExpression) {
			envType = environment.StoredExpressions
		}
		result := compiler.CompileCELExpression(&validatingadmissionpolicy.AuditAnnotationCondition{
			ValueExpression: trimmedValueExpression,
		}, plugincel.OptionalVariableDeclarations{HasParams: paramKind != nil, HasAuthorizer: true, StrictCost: opts.strictCostEnforcement}, envType)
		if result.Error != nil {
			switch result.Error.Type {
			case cel.ErrorTypeRequired:
				allErrors = append(allErrors, field.Required(fldPath.Child("valueExpression"), result.Error.Detail))
			case cel.ErrorTypeInvalid:
				allErrors = append(allErrors, field.Invalid(fldPath.Child("valueExpression"), v.ValueExpression, result.Error.Detail))
			default:
				allErrors = append(allErrors, field.InternalError(fldPath.Child("valueExpression"), result.Error))
			}
		}
	}
	return allErrors
}

var newlineMatcher = regexp.MustCompile(`[\n\r]+`) // valid newline chars in CEL grammar
func hasNewlines(s string) bool {
	return newlineMatcher.MatchString(s)
}

// ValidateValidatingAdmissionPolicyBinding validates a ValidatingAdmissionPolicyBinding before create.
func ValidateValidatingAdmissionPolicyBinding(pb *admissionregistration.ValidatingAdmissionPolicyBinding) field.ErrorList {
	return validateValidatingAdmissionPolicyBinding(pb)
}

func validateValidatingAdmissionPolicyBinding(pb *admissionregistration.ValidatingAdmissionPolicyBinding) field.ErrorList {
	allErrors := genericvalidation.ValidateObjectMeta(&pb.ObjectMeta, false, genericvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	allErrors = append(allErrors, validateValidatingAdmissionPolicyBindingSpec(&pb.Spec, field.NewPath("spec"))...)

	return allErrors
}

func validateValidatingAdmissionPolicyBindingSpec(spec *admissionregistration.ValidatingAdmissionPolicyBindingSpec, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList

	if len(spec.PolicyName) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("policyName"), ""))
	} else {
		for _, msg := range genericvalidation.NameIsDNSSubdomain(spec.PolicyName, false) {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("policyName"), spec.PolicyName, msg))
		}
	}
	allErrors = append(allErrors, validateParamRef(spec.ParamRef, fldPath.Child("paramRef"))...)
	allErrors = append(allErrors, validateMatchResources(spec.MatchResources, fldPath.Child("matchResources"))...)
	allErrors = append(allErrors, validateValidationActions(spec.ValidationActions, fldPath.Child("validationActions"))...)

	return allErrors
}

func validateParamRef(pr *admissionregistration.ParamRef, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if pr == nil {
		return allErrors
	}

	if len(pr.Name) > 0 {
		for _, msg := range path.ValidatePathSegmentName(pr.Name, false) {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("name"), pr.Name, msg))
		}

		if pr.Selector != nil {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("name"), `name and selector are mutually exclusive`))
		}
	}

	if pr.Selector != nil {
		labelSelectorValidationOpts := metav1validation.LabelSelectorValidationOptions{}
		allErrors = append(allErrors, metav1validation.ValidateLabelSelector(pr.Selector, labelSelectorValidationOpts, fldPath.Child("selector"))...)

		if len(pr.Name) > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("selector"), `name and selector are mutually exclusive`))
		}
	}

	if len(pr.Name) == 0 && pr.Selector == nil {
		allErrors = append(allErrors, field.Required(fldPath, `one of name or selector must be specified`))
	}

	if pr.ParameterNotFoundAction == nil || len(*pr.ParameterNotFoundAction) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("parameterNotFoundAction"), ""))
	} else {
		if *pr.ParameterNotFoundAction != admissionregistration.DenyAction && *pr.ParameterNotFoundAction != admissionregistration.AllowAction {
			allErrors = append(allErrors, field.NotSupported(fldPath.Child("parameterNotFoundAction"), pr.ParameterNotFoundAction, []string{string(admissionregistration.DenyAction), string(admissionregistration.AllowAction)}))
		}
	}

	return allErrors
}

// ValidateValidatingAdmissionPolicyUpdate validates update of validating admission policy
func ValidateValidatingAdmissionPolicyUpdate(newC, oldC *admissionregistration.ValidatingAdmissionPolicy) field.ErrorList {
	return validateValidatingAdmissionPolicy(newC, validationOptions{
		ignoreMatchConditions:  ignoreValidatingAdmissionPolicyMatchConditions(newC, oldC),
		preexistingExpressions: findValidatingPolicyPreexistingExpressions(oldC),
		strictCostEnforcement:  utilfeature.DefaultFeatureGate.Enabled(features.StrictCostEnforcementForVAP),
	})
}

// ValidateValidatingAdmissionPolicyStatusUpdate validates update of status of validating admission policy
func ValidateValidatingAdmissionPolicyStatusUpdate(newC, oldC *admissionregistration.ValidatingAdmissionPolicy) field.ErrorList {
	return validateValidatingAdmissionPolicyStatus(&newC.Status, field.NewPath("status"))
}

// ValidateValidatingAdmissionPolicyBindingUpdate validates update of validating admission policy
func ValidateValidatingAdmissionPolicyBindingUpdate(newC, oldC *admissionregistration.ValidatingAdmissionPolicyBinding) field.ErrorList {
	return validateValidatingAdmissionPolicyBinding(newC)
}

func validateValidatingAdmissionPolicyStatus(status *admissionregistration.ValidatingAdmissionPolicyStatus, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	allErrors = append(allErrors, validateTypeChecking(status.TypeChecking, fldPath.Child("typeChecking"))...)
	allErrors = append(allErrors, metav1validation.ValidateConditions(status.Conditions, fldPath.Child("conditions"))...)
	return allErrors
}

func validateTypeChecking(typeChecking *admissionregistration.TypeChecking, fldPath *field.Path) field.ErrorList {
	if typeChecking == nil {
		return nil
	}
	return validateExpressionWarnings(typeChecking.ExpressionWarnings, fldPath.Child("expressionWarnings"))
}

func validateExpressionWarnings(expressionWarnings []admissionregistration.ExpressionWarning, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	for i, warning := range expressionWarnings {
		allErrors = append(allErrors, validateExpressionWarning(&warning, fldPath.Index(i))...)
	}
	return allErrors
}

func validateExpressionWarning(expressionWarning *admissionregistration.ExpressionWarning, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList
	if expressionWarning.Warning == "" {
		allErrors = append(allErrors, field.Required(fldPath.Child("warning"), ""))
	}
	allErrors = append(allErrors, validateFieldRef(expressionWarning.FieldRef, fldPath.Child("fieldRef"))...)
	return allErrors
}

func validateFieldRef(fieldRef string, fldPath *field.Path) field.ErrorList {
	fieldRef = strings.TrimSpace(fieldRef)
	if fieldRef == "" {
		return field.ErrorList{field.Required(fldPath, "")}
	}
	jsonPath := jsonpath.New("spec")
	if err := jsonPath.Parse(fmt.Sprintf("{%s}", fieldRef)); err != nil {
		return field.ErrorList{field.Invalid(fldPath, fieldRef, fmt.Sprintf("invalid JSONPath: %v", err))}
	}
	// no further checks, for an easier upgrade/rollback
	return nil
}

// statelessCELCompiler does not support variable composition (and thus is stateless). It should be used when
// variable composition is not allowed, for example, when validating MatchConditions.
// strictStatelessCELCompiler is a cel Compiler that enforces strict cost enforcement.
// nonStrictStatelessCELCompiler is a cel Compiler that does not enforce strict cost enforcement.
var (
	lazyStrictStatelessCELCompilerInit sync.Once
	lazyStrictStatelessCELCompiler     plugincel.Compiler

	lazyNonStrictStatelessCELCompilerInit sync.Once
	lazyNonStrictStatelessCELCompiler     plugincel.Compiler
)

func getStrictStatelessCELCompiler() plugincel.Compiler {
	lazyStrictStatelessCELCompilerInit.Do(func() {
		lazyStrictStatelessCELCompiler = plugincel.NewCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true))
	})
	return lazyStrictStatelessCELCompiler
}

func getNonStrictStatelessCELCompiler() plugincel.Compiler {
	lazyNonStrictStatelessCELCompilerInit.Do(func() {
		lazyNonStrictStatelessCELCompiler = plugincel.NewCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), false))
	})
	return lazyNonStrictStatelessCELCompiler
}

func createCompiler(allowComposition, strictCost bool) plugincel.Compiler {
	if !allowComposition {
		if strictCost {
			return getStrictStatelessCELCompiler()
		} else {
			return getNonStrictStatelessCELCompiler()
		}
	}
	compiler, err := plugincel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), strictCost))
	if err != nil {
		// should never happen, but cannot panic either.
		utilruntime.HandleError(err)
		return plugincel.NewCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), strictCost))
	}
	return compiler
}

var celIdentRegex = regexp.MustCompile("^[_a-zA-Z][_a-zA-Z0-9]*$")
var celReserved = sets.NewString("true", "false", "null", "in",
	"as", "break", "const", "continue", "else",
	"for", "function", "if", "import", "let",
	"loop", "package", "namespace", "return",
	"var", "void", "while")

func isCELIdentifier(name string) bool {
	// IDENT          ::= [_a-zA-Z][_a-zA-Z0-9]* - RESERVED
	// BOOL_LIT       ::= "true" | "false"
	// NULL_LIT       ::= "null"
	// RESERVED       ::= BOOL_LIT | NULL_LIT | "in"
	// 	 | "as" | "break" | "const" | "continue" | "else"
	// 	 | "for" | "function" | "if" | "import" | "let"
	// 	 | "loop" | "package" | "namespace" | "return"
	// 	 | "var" | "void" | "while"
	return celIdentRegex.MatchString(name) && !celReserved.Has(name)
}

// ValidateMutatingAdmissionPolicyUpdate validates update of mutating admission policy
func ValidateMutatingAdmissionPolicyUpdate(newC, oldC *admissionregistration.MutatingAdmissionPolicy) field.ErrorList {
	return validateMutatingAdmissionPolicy(newC, validationOptions{
		ignoreMatchConditions:  ignoreMutatingAdmissionPolicyMatchConditions(newC, oldC),
		preexistingExpressions: findMutatingPolicyPreexistingExpressions(oldC),
		strictCostEnforcement:  true,
	})
}

// ValidateMutatingAdmissionPolicyBindingUpdate validates update of mutating admission policy
func ValidateMutatingAdmissionPolicyBindingUpdate(newC, oldC *admissionregistration.MutatingAdmissionPolicyBinding) field.ErrorList {
	return validateMutatingAdmissionPolicyBinding(newC)
}

// ValidateMutatingAdmissionPolicy validates a MutatingAdmissionPolicy before creation.
func ValidateMutatingAdmissionPolicy(p *admissionregistration.MutatingAdmissionPolicy) field.ErrorList {
	return validateMutatingAdmissionPolicy(p, validationOptions{ignoreMatchConditions: false, strictCostEnforcement: true})
}

func validateMutatingAdmissionPolicy(p *admissionregistration.MutatingAdmissionPolicy, opts validationOptions) field.ErrorList {
	allErrors := genericvalidation.ValidateObjectMeta(&p.ObjectMeta, false, genericvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	allErrors = append(allErrors, validateMutatingAdmissionPolicySpec(p.ObjectMeta, &p.Spec, opts, field.NewPath("spec"))...)
	return allErrors
}

func validateMutatingAdmissionPolicySpec(meta metav1.ObjectMeta, spec *admissionregistration.MutatingAdmissionPolicySpec, opts validationOptions, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList

	compiler := createCompiler(true, true)

	if spec.FailurePolicy == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("failurePolicy"), ""))
	} else if !supportedFailurePolicies.Has(string(*spec.FailurePolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("failurePolicy"), *spec.FailurePolicy, supportedFailurePolicies.List()))
	}
	if spec.ParamKind != nil {
		opts.allowParamsInMatchConditions = true
		allErrors = append(allErrors, validateParamKind(*spec.ParamKind, fldPath.Child("paramKind"))...)
	}
	if spec.MatchConstraints == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("matchConstraints"), ""))
	} else {
		allErrors = append(allErrors, validateMatchResources(spec.MatchConstraints, fldPath.Child("matchConstraints"))...)
		// at least one resourceRule must be defined to provide type information
		if len(spec.MatchConstraints.ResourceRules) == 0 {
			allErrors = append(allErrors, field.Required(fldPath.Child("matchConstraints", "resourceRules"), ""))
		}

		// It is only possible to mutate create and update requests
		for _, rule := range spec.MatchConstraints.ResourceRules {
			for _, op := range rule.RuleWithOperations.Operations {
				if !supportedMutatingOperations.Has(string(op)) {
					allErrors = append(allErrors, field.NotSupported(fldPath.Child("matchConstraints", "resourceRules", "operations"), op, supportedMutatingOperations.List()))
				}
			}
		}
	}
	if !opts.ignoreMatchConditions {
		allErrors = append(allErrors, validateMatchConditions(spec.MatchConditions, opts, fldPath.Child("matchConditions"))...)
	}
	for i, variable := range spec.Variables {
		allErrors = append(allErrors, validateVariable(compiler, &variable, spec.ParamKind, opts, fldPath.Child("variables").Index(i))...)
	}
	if len(spec.Mutations) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("mutations"), "mutations must contain at least one item"))
	} else {
		for i, mutation := range spec.Mutations {
			allErrors = append(allErrors, validateMutation(compiler, &mutation, spec.ParamKind, opts, fldPath.Child("mutations").Index(i))...)
		}
	}
	if len(spec.ReinvocationPolicy) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("reinvocationPolicy"), ""))
	} else if !supportedReinvocationPolicies.Has(string(spec.ReinvocationPolicy)) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("reinvocationPolicy"), spec.ReinvocationPolicy, supportedReinvocationPolicies.List()))
	}
	return allErrors
}

func validateMutation(compiler plugincel.Compiler, m *admissionregistration.Mutation, paramKind *admissionregistration.ParamKind, opts validationOptions, fldPath *field.Path) (allErrors field.ErrorList) {
	if len(m.PatchType) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("patchType"), ""))
	} else {
		switch m.PatchType {
		case admissionregistration.PatchTypeJSONPatch:
			if m.JSONPatch == nil {
				allErrors = append(allErrors, field.Required(fldPath.Child("jsonPatch"), "must be specified when patchType is JSONPatch"))
			} else {
				allErrors = append(allErrors, validateJSONPatch(compiler, m.JSONPatch, paramKind, opts, fldPath.Child("jsonPatch"))...)
			}
			if m.ApplyConfiguration != nil {
				allErrors = append(allErrors, field.Invalid(fldPath.Child("applyConfiguration"), "{applyConfiguration}", "must not be specified when patchType is JSONPatch"))
			}
		case admissionregistration.PatchTypeApplyConfiguration:
			if m.ApplyConfiguration == nil {
				allErrors = append(allErrors, field.Required(fldPath.Child("applyConfiguration"), "must be specified when patchType is ApplyConfiguration"))
			} else {
				allErrors = append(allErrors, validateApplyConfiguration(compiler, m.ApplyConfiguration, paramKind, opts, fldPath.Child("applyConfiguration"))...)
			}
			if m.JSONPatch != nil {
				allErrors = append(allErrors, field.Invalid(fldPath.Child("jsonPatch"), "{jsonPatch}", "must not be specified when patchType is ApplyConfiguration"))
			}
		default:
			allErrors = append(allErrors, field.NotSupported(fldPath.Child("patchType"), m.PatchType, supportedPatchType.List()))
		}
	}
	return allErrors
}

func validateApplyConfiguration(compiler plugincel.Compiler, applyConfig *admissionregistration.ApplyConfiguration, paramKind *admissionregistration.ParamKind, opts validationOptions, fldPath *field.Path) (allErrors field.ErrorList) {
	trimmedExpression := strings.TrimSpace(applyConfig.Expression)
	if len(trimmedExpression) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("expression"), ""))
	} else {

		envType := environment.NewExpressions
		if opts.preexistingExpressions.applyConfigurationExpressions.Has(applyConfig.Expression) {
			envType = environment.StoredExpressions
		}
		accessor := &patch.ApplyConfigurationCondition{
			Expression: trimmedExpression,
		}
		opts := plugincel.OptionalVariableDeclarations{HasParams: paramKind != nil, HasAuthorizer: true, StrictCost: true, HasPatchTypes: true}
		result := compiler.CompileCELExpression(accessor, opts, envType)

		if result.Error != nil {
			allErrors = append(allErrors, convertCELErrorToValidationError(fldPath.Child("expression"), accessor, result.Error))
		}
	}
	return allErrors
}

func validateJSONPatch(compiler plugincel.Compiler, jsonPatch *admissionregistration.JSONPatch, paramKind *admissionregistration.ParamKind, opts validationOptions, fldPath *field.Path) (allErrors field.ErrorList) {
	trimmedExpression := strings.TrimSpace(jsonPatch.Expression)
	if len(trimmedExpression) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("expression"), ""))
	} else {

		envType := environment.NewExpressions
		if opts.preexistingExpressions.applyConfigurationExpressions.Has(jsonPatch.Expression) {
			envType = environment.StoredExpressions
		}
		accessor := &patch.JSONPatchCondition{
			Expression: trimmedExpression,
		}
		opts := plugincel.OptionalVariableDeclarations{HasParams: paramKind != nil, HasAuthorizer: true, StrictCost: true, HasPatchTypes: true}
		result := compiler.CompileCELExpression(accessor, opts, envType)

		if result.Error != nil {
			allErrors = append(allErrors, convertCELErrorToValidationError(fldPath.Child("expression"), accessor, result.Error))
		}
	}
	return allErrors
}

// ValidateMutatingAdmissionPolicyBinding validates a MutatingAdmissionPolicyBinding before create.
func ValidateMutatingAdmissionPolicyBinding(pb *admissionregistration.MutatingAdmissionPolicyBinding) field.ErrorList {
	return validateMutatingAdmissionPolicyBinding(pb)
}

func validateMutatingAdmissionPolicyBinding(pb *admissionregistration.MutatingAdmissionPolicyBinding) field.ErrorList {
	allErrors := genericvalidation.ValidateObjectMeta(&pb.ObjectMeta, false, genericvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	allErrors = append(allErrors, validateMutatingAdmissionPolicyBindingSpec(&pb.Spec, field.NewPath("spec"))...)

	return allErrors
}

func validateMutatingAdmissionPolicyBindingSpec(spec *admissionregistration.MutatingAdmissionPolicyBindingSpec, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList

	if len(spec.PolicyName) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("policyName"), ""))
	} else {
		for _, msg := range genericvalidation.NameIsDNSSubdomain(spec.PolicyName, false) {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("policyName"), spec.PolicyName, msg))
		}
	}
	allErrors = append(allErrors, validateParamRef(spec.ParamRef, fldPath.Child("paramRef"))...)
	allErrors = append(allErrors, validateMatchResources(spec.MatchResources, fldPath.Child("matchResources"))...)
	if spec.MatchResources != nil {
		// It is only possible to mutate create and update requests
		for _, rule := range spec.MatchResources.ResourceRules {
			for _, op := range rule.RuleWithOperations.Operations {
				if !supportedMutatingOperations.Has(string(op)) {
					allErrors = append(allErrors, field.NotSupported(fldPath.Child("matchResources", "resourceRules", "operations"), op, supportedMutatingOperations.List()))
				}
			}
		}
	}

	return allErrors
}
