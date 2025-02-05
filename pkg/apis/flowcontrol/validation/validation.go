/*
Copyright 2019 The Kubernetes Authors.

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
	"strings"

	flowcontrolv1beta1 "k8s.io/api/flowcontrol/v1beta1"
	flowcontrolv1beta2 "k8s.io/api/flowcontrol/v1beta2"
	flowcontrolv1beta3 "k8s.io/api/flowcontrol/v1beta3"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/util/shufflesharding"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/apis/flowcontrol"
	"k8s.io/kubernetes/pkg/apis/flowcontrol/internalbootstrap"
)

// ValidateFlowSchemaName validates name for flow-schema.
var ValidateFlowSchemaName = apimachineryvalidation.NameIsDNSSubdomain

// ValidatePriorityLevelConfigurationName validates name for priority-level-configuration.
var ValidatePriorityLevelConfigurationName = apimachineryvalidation.NameIsDNSSubdomain

var supportedDistinguisherMethods = sets.NewString(
	string(flowcontrol.FlowDistinguisherMethodByNamespaceType),
	string(flowcontrol.FlowDistinguisherMethodByUserType),
)

var priorityLevelConfigurationQueuingMaxQueues int32 = 10 * 1000 * 1000 // 10^7

var supportedVerbs = sets.NewString(
	"get",
	"list",
	"create",
	"update",
	"delete",
	"deletecollection",
	"patch",
	"watch",
	"proxy",
)

var supportedSubjectKinds = sets.NewString(
	string(flowcontrol.SubjectKindServiceAccount),
	string(flowcontrol.SubjectKindGroup),
	string(flowcontrol.SubjectKindUser),
)

var supportedPriorityLevelEnablement = sets.NewString(
	string(flowcontrol.PriorityLevelEnablementExempt),
	string(flowcontrol.PriorityLevelEnablementLimited),
)

var supportedLimitResponseType = sets.NewString(
	string(flowcontrol.LimitResponseTypeQueue),
	string(flowcontrol.LimitResponseTypeReject),
)

// PriorityLevelValidationOptions holds the validation options for a priority level object
type PriorityLevelValidationOptions struct{}

// ValidateFlowSchema validates the content of flow-schema
func ValidateFlowSchema(fs *flowcontrol.FlowSchema) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&fs.ObjectMeta, false, ValidateFlowSchemaName, field.NewPath("metadata"))
	specPath := field.NewPath("spec")
	allErrs = append(allErrs, ValidateFlowSchemaSpec(fs.Name, &fs.Spec, specPath)...)
	if mand, ok := internalbootstrap.MandatoryFlowSchemas[fs.Name]; ok {
		// Check for almost exact equality.  This is a pretty
		// strict test, and it is OK in this context because both
		// sides of this comparison are intended to ultimately
		// come from the same code.
		if !apiequality.Semantic.DeepEqual(fs.Spec, mand.Spec) {
			allErrs = append(allErrs, field.Invalid(specPath, fs.Spec, fmt.Sprintf("spec of '%s' must equal the fixed value", fs.Name)))
		}
	}
	allErrs = append(allErrs, ValidateFlowSchemaStatus(&fs.Status, field.NewPath("status"))...)
	return allErrs
}

// ValidateFlowSchemaUpdate validates the update of flow-schema
func ValidateFlowSchemaUpdate(old, fs *flowcontrol.FlowSchema) field.ErrorList {
	return ValidateFlowSchema(fs)
}

// ValidateFlowSchemaSpec validates the content of flow-schema's spec
func ValidateFlowSchemaSpec(fsName string, spec *flowcontrol.FlowSchemaSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if spec.MatchingPrecedence <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("matchingPrecedence"), spec.MatchingPrecedence, "must be a positive value"))
	}
	if spec.MatchingPrecedence > flowcontrol.FlowSchemaMaxMatchingPrecedence {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("matchingPrecedence"), spec.MatchingPrecedence, fmt.Sprintf("must not be greater than %v", flowcontrol.FlowSchemaMaxMatchingPrecedence)))
	}
	if (spec.MatchingPrecedence == 1) && (fsName != flowcontrol.FlowSchemaNameExempt) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("matchingPrecedence"), spec.MatchingPrecedence, "only the schema named 'exempt' may have matchingPrecedence 1"))
	}
	if spec.DistinguisherMethod != nil {
		if !supportedDistinguisherMethods.Has(string(spec.DistinguisherMethod.Type)) {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("distinguisherMethod").Child("type"), spec.DistinguisherMethod, supportedDistinguisherMethods.List()))
		}
	}
	if len(spec.PriorityLevelConfiguration.Name) > 0 {
		for _, msg := range ValidatePriorityLevelConfigurationName(spec.PriorityLevelConfiguration.Name, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("priorityLevelConfiguration").Child("name"), spec.PriorityLevelConfiguration.Name, msg))
		}
	} else {
		allErrs = append(allErrs, field.Required(fldPath.Child("priorityLevelConfiguration").Child("name"), "must reference a priority level"))
	}
	for i, rule := range spec.Rules {
		allErrs = append(allErrs, ValidateFlowSchemaPolicyRulesWithSubjects(&rule, fldPath.Child("rules").Index(i))...)
	}
	return allErrs
}

// ValidateFlowSchemaPolicyRulesWithSubjects validates policy-rule-with-subjects object.
func ValidateFlowSchemaPolicyRulesWithSubjects(rule *flowcontrol.PolicyRulesWithSubjects, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(rule.Subjects) > 0 {
		for i, subject := range rule.Subjects {
			allErrs = append(allErrs, ValidateFlowSchemaSubject(&subject, fldPath.Child("subjects").Index(i))...)
		}
	} else {
		allErrs = append(allErrs, field.Required(fldPath.Child("subjects"), "subjects must contain at least one value"))
	}

	if len(rule.ResourceRules) == 0 && len(rule.NonResourceRules) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "at least one of resourceRules and nonResourceRules has to be non-empty"))
	}
	for i, resourceRule := range rule.ResourceRules {
		allErrs = append(allErrs, ValidateFlowSchemaResourcePolicyRule(&resourceRule, fldPath.Child("resourceRules").Index(i))...)
	}
	for i, nonResourceRule := range rule.NonResourceRules {
		allErrs = append(allErrs, ValidateFlowSchemaNonResourcePolicyRule(&nonResourceRule, fldPath.Child("nonResourceRules").Index(i))...)
	}
	return allErrs
}

// ValidateFlowSchemaSubject validates flow-schema's subject object.
func ValidateFlowSchemaSubject(subject *flowcontrol.Subject, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch subject.Kind {
	case flowcontrol.SubjectKindServiceAccount:
		allErrs = append(allErrs, ValidateServiceAccountSubject(subject.ServiceAccount, fldPath.Child("serviceAccount"))...)
		if subject.User != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("user"), "user is forbidden when subject kind is not 'User'"))
		}
		if subject.Group != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("group"), "group is forbidden when subject kind is not 'Group'"))
		}
	case flowcontrol.SubjectKindUser:
		allErrs = append(allErrs, ValidateUserSubject(subject.User, fldPath.Child("user"))...)
		if subject.ServiceAccount != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("serviceAccount"), "serviceAccount is forbidden when subject kind is not 'ServiceAccount'"))
		}
		if subject.Group != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("group"), "group is forbidden when subject kind is not 'Group'"))
		}
	case flowcontrol.SubjectKindGroup:
		allErrs = append(allErrs, ValidateGroupSubject(subject.Group, fldPath.Child("group"))...)
		if subject.ServiceAccount != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("serviceAccount"), "serviceAccount is forbidden when subject kind is not 'ServiceAccount'"))
		}
		if subject.User != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("user"), "user is forbidden when subject kind is not 'User'"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("kind"), subject.Kind, supportedSubjectKinds.List()))
	}
	return allErrs
}

// ValidateServiceAccountSubject validates subject of "ServiceAccount" kind
func ValidateServiceAccountSubject(subject *flowcontrol.ServiceAccountSubject, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if subject == nil {
		return append(allErrs, field.Required(fldPath, "serviceAccount is required when subject kind is 'ServiceAccount'"))
	}
	if len(subject.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else if subject.Name != flowcontrol.NameAll {
		for _, msg := range apimachineryvalidation.ValidateServiceAccountName(subject.Name, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), subject.Name, msg))
		}
	}

	if len(subject.Namespace) > 0 {
		for _, msg := range apimachineryvalidation.ValidateNamespaceName(subject.Namespace, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), subject.Namespace, msg))
		}
	} else {
		allErrs = append(allErrs, field.Required(fldPath.Child("namespace"), "must specify namespace for service account"))
	}

	return allErrs
}

// ValidateUserSubject validates subject of "User" kind
func ValidateUserSubject(subject *flowcontrol.UserSubject, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if subject == nil {
		return append(allErrs, field.Required(fldPath, "user is required when subject kind is 'User'"))
	}
	if len(subject.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	return allErrs
}

// ValidateGroupSubject validates subject of "Group" kind
func ValidateGroupSubject(subject *flowcontrol.GroupSubject, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if subject == nil {
		return append(allErrs, field.Required(fldPath, "group is required when subject kind is 'Group'"))
	}
	if len(subject.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	return allErrs
}

// ValidateFlowSchemaNonResourcePolicyRule validates non-resource policy-rule in the flow-schema.
func ValidateFlowSchemaNonResourcePolicyRule(rule *flowcontrol.NonResourcePolicyRule, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(rule.Verbs) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("verbs"), "verbs must contain at least one value"))
	} else if hasWildcard(rule.Verbs) {
		if len(rule.Verbs) > 1 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("verbs"), rule.Verbs, "if '*' is present, must not specify other verbs"))
		}
	} else if !supportedVerbs.IsSuperset(sets.NewString(rule.Verbs...)) {
		// only supported verbs are allowed
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("verbs"), rule.Verbs, supportedVerbs.List()))
	}

	if len(rule.NonResourceURLs) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("nonResourceURLs"), "nonResourceURLs must contain at least one value"))
	} else if hasWildcard(rule.NonResourceURLs) {
		if len(rule.NonResourceURLs) > 1 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nonResourceURLs"), rule.NonResourceURLs, "if '*' is present, must not specify other non-resource URLs"))
		}
	} else {
		for i, nonResourceURL := range rule.NonResourceURLs {
			if err := ValidateNonResourceURLPath(nonResourceURL, fldPath.Child("nonResourceURLs").Index(i)); err != nil {
				allErrs = append(allErrs, err)
			}
		}
	}

	return allErrs
}

// ValidateFlowSchemaResourcePolicyRule validates resource policy-rule in the flow-schema.
func ValidateFlowSchemaResourcePolicyRule(rule *flowcontrol.ResourcePolicyRule, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(rule.Verbs) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("verbs"), "verbs must contain at least one value"))
	} else if hasWildcard(rule.Verbs) {
		if len(rule.Verbs) > 1 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("verbs"), rule.Verbs, "if '*' is present, must not specify other verbs"))
		}
	} else if !supportedVerbs.IsSuperset(sets.NewString(rule.Verbs...)) {
		// only supported verbs are allowed
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("verbs"), rule.Verbs, supportedVerbs.List()))
	}

	if len(rule.APIGroups) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("apiGroups"), "resource rules must supply at least one api group"))
	} else if len(rule.APIGroups) > 1 && hasWildcard(rule.APIGroups) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("apiGroups"), rule.APIGroups, "if '*' is present, must not specify other api groups"))
	}

	if len(rule.Resources) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("resources"), "resource rules must supply at least one resource"))
	} else if len(rule.Resources) > 1 && hasWildcard(rule.Resources) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("resources"), rule.Resources, "if '*' is present, must not specify other resources"))
	}

	if len(rule.Namespaces) == 0 && !rule.ClusterScope {
		allErrs = append(allErrs, field.Required(fldPath.Child("namespaces"), "resource rules that are not cluster scoped must supply at least one namespace"))
	} else if hasWildcard(rule.Namespaces) {
		if len(rule.Namespaces) > 1 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespaces"), rule.Namespaces, "if '*' is present, must not specify other namespaces"))
		}
	} else {
		for idx, tgtNS := range rule.Namespaces {
			for _, msg := range apimachineryvalidation.ValidateNamespaceName(tgtNS, false) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("namespaces").Index(idx), tgtNS, nsErrIntro+msg))
			}
		}
	}

	return allErrs
}

const nsErrIntro = "each member of this list must be '*' or a DNS-1123 label; "

// ValidateFlowSchemaStatus validates status for the flow-schema.
func ValidateFlowSchemaStatus(status *flowcontrol.FlowSchemaStatus, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	keys := sets.NewString()
	for i, condition := range status.Conditions {
		if keys.Has(string(condition.Type)) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Child("conditions").Index(i).Child("type"), condition.Type))
		}
		keys.Insert(string(condition.Type))
		allErrs = append(allErrs, ValidateFlowSchemaCondition(&condition, fldPath.Child("conditions").Index(i))...)
	}
	return allErrs
}

// ValidateFlowSchemaStatusUpdate validates the update of status for the flow-schema.
func ValidateFlowSchemaStatusUpdate(old, fs *flowcontrol.FlowSchema) field.ErrorList {
	return ValidateFlowSchemaStatus(&fs.Status, field.NewPath("status"))
}

// ValidateFlowSchemaCondition validates condition in the flow-schema's status.
func ValidateFlowSchemaCondition(condition *flowcontrol.FlowSchemaCondition, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(condition.Type) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("type"), "must not be empty"))
	}
	return allErrs
}

// ValidatePriorityLevelConfiguration validates priority-level-configuration.
func ValidatePriorityLevelConfiguration(pl *flowcontrol.PriorityLevelConfiguration, requestGV schema.GroupVersion, opts PriorityLevelValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&pl.ObjectMeta, false, ValidatePriorityLevelConfigurationName, field.NewPath("metadata"))

	// the roundtrip annotation is only for use in v1beta3, and after
	// conversion, the internal object should not have the roundtrip
	// annotation, so we should forbid it, if it's set.
	if _, ok := pl.ObjectMeta.Annotations[flowcontrolv1beta3.PriorityLevelPreserveZeroConcurrencySharesKey]; ok {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata").Child("annotations"), fmt.Sprintf("annotation '%s' is forbidden", flowcontrolv1beta3.PriorityLevelPreserveZeroConcurrencySharesKey)))
	}

	specPath := field.NewPath("spec")
	allErrs = append(allErrs, ValidatePriorityLevelConfigurationSpec(&pl.Spec, requestGV, pl.Name, specPath, opts)...)
	allErrs = append(allErrs, ValidateIfMandatoryPriorityLevelConfigurationObject(pl, specPath)...)
	allErrs = append(allErrs, ValidatePriorityLevelConfigurationStatus(&pl.Status, field.NewPath("status"))...)
	return allErrs
}

func ValidateIfMandatoryPriorityLevelConfigurationObject(pl *flowcontrol.PriorityLevelConfiguration, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	mand, ok := internalbootstrap.MandatoryPriorityLevelConfigurations[pl.Name]
	if !ok {
		return allErrs
	}

	if pl.Name == flowcontrol.PriorityLevelConfigurationNameExempt {
		// we allow the admin to change the contents of the 'Exempt' field of
		// the singleton 'exempt' priority level object, every other fields of
		// the Spec should not be allowed to change.
		want := &mand.Spec
		have := pl.Spec.DeepCopy()
		have.Exempt = want.Exempt
		if !apiequality.Semantic.DeepEqual(want, have) {
			allErrs = append(allErrs, field.Invalid(fldPath, pl.Spec, fmt.Sprintf("spec of '%s' except the 'spec.exempt' field must equal the fixed value", pl.Name)))
		}
		return allErrs
	}

	// Check for almost exact equality.  This is a pretty
	// strict test, and it is OK in this context because both
	// sides of this comparison are intended to ultimately
	// come from the same code.
	if !apiequality.Semantic.DeepEqual(pl.Spec, mand.Spec) {
		allErrs = append(allErrs, field.Invalid(fldPath, pl.Spec, fmt.Sprintf("spec of '%s' must equal the fixed value", pl.Name)))
	}
	return allErrs
}

// ValidatePriorityLevelConfigurationSpec validates priority-level-configuration's spec.
func ValidatePriorityLevelConfigurationSpec(spec *flowcontrol.PriorityLevelConfigurationSpec, requestGV schema.GroupVersion, name string, fldPath *field.Path, opts PriorityLevelValidationOptions) field.ErrorList {
	var allErrs field.ErrorList
	if (name == flowcontrol.PriorityLevelConfigurationNameExempt) != (spec.Type == flowcontrol.PriorityLevelEnablementExempt) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("type"), spec.Type, "type must be 'Exempt' if and only if name is 'exempt'"))
	}
	switch spec.Type {
	case flowcontrol.PriorityLevelEnablementExempt:
		if spec.Limited != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("limited"), "must be nil if the type is not Limited"))
		}
		if spec.Exempt != nil {
			allErrs = append(allErrs, ValidateExemptPriorityLevelConfiguration(spec.Exempt, fldPath.Child("exempt"))...)
		}
	case flowcontrol.PriorityLevelEnablementLimited:
		if spec.Exempt != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("exempt"), "must be nil if the type is Limited"))
		}

		if spec.Limited == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("limited"), "must not be empty when type is Limited"))
		} else {
			allErrs = append(allErrs, ValidateLimitedPriorityLevelConfiguration(spec.Limited, requestGV, fldPath.Child("limited"), opts)...)
		}
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("type"), spec.Type, supportedPriorityLevelEnablement.List()))
	}
	return allErrs
}

// ValidateLimitedPriorityLevelConfiguration validates the configuration for an execution-limited priority level
func ValidateLimitedPriorityLevelConfiguration(lplc *flowcontrol.LimitedPriorityLevelConfiguration, requestGV schema.GroupVersion, fldPath *field.Path, opts PriorityLevelValidationOptions) field.ErrorList {
	var allErrs field.ErrorList
	if lplc.NominalConcurrencyShares < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child(getVersionedFieldNameForConcurrencyShares(requestGV)), lplc.NominalConcurrencyShares, "must be a non-negative integer"))
	}
	allErrs = append(allErrs, ValidateLimitResponse(lplc.LimitResponse, fldPath.Child("limitResponse"))...)

	if lplc.LendablePercent != nil && !(*lplc.LendablePercent >= 0 && *lplc.LendablePercent <= 100) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("lendablePercent"), *lplc.LendablePercent, "must be between 0 and 100, inclusive"))
	}
	if lplc.BorrowingLimitPercent != nil && *lplc.BorrowingLimitPercent < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("borrowingLimitPercent"), *lplc.BorrowingLimitPercent, "if specified, must be a non-negative integer"))
	}
	if lplc.Weight != nil && (*lplc.Weight < 1 || *lplc.Weight > 1000) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("weight"), *lplc.Weight, "if specified, must be an integer in the range 1--1000"))
	}

	return allErrs
}

func ValidateExemptPriorityLevelConfiguration(eplc *flowcontrol.ExemptPriorityLevelConfiguration, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if eplc.NominalConcurrencyShares != nil && *eplc.NominalConcurrencyShares < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("nominalConcurrencyShares"), *eplc.NominalConcurrencyShares, "must be a non-negative integer"))
	}
	if eplc.LendablePercent != nil && !(*eplc.LendablePercent >= 0 && *eplc.LendablePercent <= 100) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("lendablePercent"), *eplc.LendablePercent, "must be between 0 and 100, inclusive"))
	}
	return allErrs
}

func getVersionedFieldNameForConcurrencyShares(requestGV schema.GroupVersion) string {
	switch {
	case requestGV == flowcontrolv1beta1.SchemeGroupVersion ||
		requestGV == flowcontrolv1beta2.SchemeGroupVersion:
		return "assuredConcurrencyShares"
	default:
		return "nominalConcurrencyShares"
	}
}

// ValidateLimitResponse validates a LimitResponse
func ValidateLimitResponse(lr flowcontrol.LimitResponse, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch lr.Type {
	case flowcontrol.LimitResponseTypeReject:
		if lr.Queuing != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("queuing"), "must be nil if limited.limitResponse.type is not Limited"))
		}
	case flowcontrol.LimitResponseTypeQueue:
		if lr.Queuing == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("queuing"), "must not be empty if limited.limitResponse.type is Limited"))
		} else {
			allErrs = append(allErrs, ValidatePriorityLevelQueuingConfiguration(lr.Queuing, fldPath.Child("queuing"))...)
		}
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("type"), lr.Type, supportedLimitResponseType.List()))
	}
	return allErrs
}

// ValidatePriorityLevelQueuingConfiguration validates queuing-configuration for a priority-level
func ValidatePriorityLevelQueuingConfiguration(queuing *flowcontrol.QueuingConfiguration, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if queuing.QueueLengthLimit <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("queueLengthLimit"), queuing.QueueLengthLimit, "must be positive"))
	}

	// validate input arguments for shuffle-sharding
	if queuing.Queues <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("queues"), queuing.Queues, "must be positive"))
	} else if queuing.Queues > priorityLevelConfigurationQueuingMaxQueues {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("queues"), queuing.Queues,
			fmt.Sprintf("must not be greater than %d", priorityLevelConfigurationQueuingMaxQueues)))
	}

	if queuing.HandSize <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("handSize"), queuing.HandSize, "must be positive"))
	} else if queuing.HandSize > queuing.Queues {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("handSize"), queuing.HandSize,
			fmt.Sprintf("should not be greater than queues (%d)", queuing.Queues)))
	} else if entropy := shufflesharding.RequiredEntropyBits(int(queuing.Queues), int(queuing.HandSize)); entropy > shufflesharding.MaxHashBits {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("handSize"), queuing.HandSize,
			fmt.Sprintf("required entropy bits of deckSize %d and handSize %d should not be greater than %d", queuing.Queues, queuing.HandSize, shufflesharding.MaxHashBits)))
	}
	return allErrs
}

// ValidatePriorityLevelConfigurationStatus validates priority-level-configuration's status.
func ValidatePriorityLevelConfigurationStatus(status *flowcontrol.PriorityLevelConfigurationStatus, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	keys := sets.NewString()
	for i, condition := range status.Conditions {
		if keys.Has(string(condition.Type)) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Child("conditions").Index(i).Child("type"), condition.Type))
		}
		keys.Insert(string(condition.Type))
		allErrs = append(allErrs, ValidatePriorityLevelConfigurationCondition(&condition, fldPath.Child("conditions").Index(i))...)
	}
	return allErrs
}

// ValidatePriorityLevelConfigurationStatusUpdate validates the update of priority-level-configuration's status.
func ValidatePriorityLevelConfigurationStatusUpdate(old, pl *flowcontrol.PriorityLevelConfiguration) field.ErrorList {
	return ValidatePriorityLevelConfigurationStatus(&pl.Status, field.NewPath("status"))
}

// ValidatePriorityLevelConfigurationCondition validates condition in priority-level-configuration's status.
func ValidatePriorityLevelConfigurationCondition(condition *flowcontrol.PriorityLevelConfigurationCondition, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(condition.Type) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("type"), "must not be empty"))
	}
	return allErrs
}

// ValidateNonResourceURLPath validates non-resource-url path by following rules:
//  1. Slash must be the leading character of the path
//  2. White-space is forbidden in the path
//  3. Continuous/double slash is forbidden in the path
//  4. Wildcard "*" should only do suffix glob matching. Note that wildcard also matches slashes.
func ValidateNonResourceURLPath(path string, fldPath *field.Path) *field.Error {
	if len(path) == 0 {
		return field.Invalid(fldPath, path, "must not be empty")
	}
	if path == "/" { // root path
		return nil
	}

	if !strings.HasPrefix(path, "/") {
		return field.Invalid(fldPath, path, "must start with slash")
	}
	if strings.Contains(path, " ") {
		return field.Invalid(fldPath, path, "must not contain white-space")
	}
	if strings.Contains(path, "//") {
		return field.Invalid(fldPath, path, "must not contain double slash")
	}
	wildcardCount := strings.Count(path, "*")
	if wildcardCount > 1 || (wildcardCount == 1 && path[len(path)-2:] != "/*") {
		return field.Invalid(fldPath, path, "wildcard can only do suffix matching")
	}
	return nil
}

func hasWildcard(operations []string) bool {
	return memberInList("*", operations...)
}

func memberInList(seek string, a ...string) bool {
	for _, ai := range a {
		if ai == seek {
			return true
		}
	}
	return false
}
