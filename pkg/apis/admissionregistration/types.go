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

package admissionregistration

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Rule is a tuple of APIGroups, APIVersion, and Resources.It is recommended
// to make sure that all the tuple expansions are valid.
type Rule struct {
	// APIGroups is the API groups the resources belong to. '*' is all groups.
	// If '*' is present, the length of the slice must be one.
	// Required.
	APIGroups []string

	// APIVersions is the API versions the resources belong to. '*' is all versions.
	// If '*' is present, the length of the slice must be one.
	// Required.
	APIVersions []string

	// Resources is a list of resources this rule applies to.
	//
	// For example:
	// 'pods' means pods.
	// 'pods/log' means the log subresource of pods.
	// '*' means all resources, but not subresources.
	// 'pods/*' means all subresources of pods.
	// '*/scale' means all scale subresources.
	// '*/*' means all resources and their subresources.
	//
	// If wildcard is present, the validation rule will ensure resources do not
	// overlap with each other.
	//
	// Depending on the enclosing object, subresources might not be allowed.
	// Required.
	Resources []string

	// scope specifies the scope of this rule.
	// Valid values are "Cluster", "Namespaced", and "*"
	// "Cluster" means that only cluster-scoped resources will match this rule.
	// Namespace API objects are cluster-scoped.
	// "Namespaced" means that only namespaced resources will match this rule.
	// "*" means that there are no scope restrictions.
	// Subresources match the scope of their parent resource.
	// Default is "*".
	//
	// +optional
	Scope *ScopeType
}

// ScopeType specifies the type of scope being used
type ScopeType string

const (
	// ClusterScope means that scope is limited to cluster-scoped objects.
	// Namespace objects are cluster-scoped.
	ClusterScope ScopeType = "Cluster"
	// NamespacedScope means that scope is limited to namespaced objects.
	NamespacedScope ScopeType = "Namespaced"
	// AllScopes means that all scopes are included.
	AllScopes ScopeType = "*"
)

// ParameterNotFoundActionType specifies a failure policy that defines how a binding
// is evaluated when the param referred by its perNamespaceParamRef is not found.
type ParameterNotFoundActionType string

const (
	// Allow means all requests will be admitted if no param resources
	// could be found.
	AllowAction ParameterNotFoundActionType = "Allow"
	// Deny means all requests will be denied if no param resources are found.
	DenyAction ParameterNotFoundActionType = "Deny"
)

// FailurePolicyType specifies the type of failure policy
type FailurePolicyType string

const (
	// Ignore means that an error calling the admission webhook or admission policy is ignored.
	Ignore FailurePolicyType = "Ignore"
	// Fail means that an error calling the admission webhook or admission policy causes resource admission to fail.
	Fail FailurePolicyType = "Fail"
)

// MatchPolicyType specifies the type of match policy
type MatchPolicyType string

const (
	// Exact means requests should only be sent to the admission webhook or admission policy if they exactly match a given rule.
	Exact MatchPolicyType = "Exact"
	// Equivalent means requests should be sent to the admission webhook or admission policy if they modify a resource listed
	// in rules via another API group or version.
	Equivalent MatchPolicyType = "Equivalent"
)

// SideEffectClass denotes the type of side effects resulting from calling the webhook
type SideEffectClass string

const (
	// SideEffectClassUnknown means that no information is known about the side effects of calling the webhook.
	// If a request with the dry-run attribute would trigger a call to this webhook, the request will instead fail.
	SideEffectClassUnknown SideEffectClass = "Unknown"
	// SideEffectClassNone means that calling the webhook will have no side effects.
	SideEffectClassNone SideEffectClass = "None"
	// SideEffectClassSome means that calling the webhook will possibly have side effects.
	// If a request with the dry-run attribute would trigger a call to this webhook, the request will instead fail.
	SideEffectClassSome SideEffectClass = "Some"
	// SideEffectClassNoneOnDryRun means that calling the webhook will possibly have side effects, but if the
	// request being reviewed has the dry-run attribute, the side effects will be suppressed.
	SideEffectClassNoneOnDryRun SideEffectClass = "NoneOnDryRun"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ValidatingAdmissionPolicy describes the definition of an admission validation policy that accepts or rejects an object without changing it.
type ValidatingAdmissionPolicy struct {
	metav1.TypeMeta
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta
	// Specification of the desired behavior of the ValidatingAdmissionPolicy.
	Spec ValidatingAdmissionPolicySpec
	// The status of the ValidatingAdmissionPolicy, including warnings that are useful to determine if the policy
	// behaves in the expected way.
	// Populated by the system.
	// Read-only.
	// +optional
	Status ValidatingAdmissionPolicyStatus
}

// ValidatingAdmissionPolicyStatus represents the status of an admission validation policy.
type ValidatingAdmissionPolicyStatus struct {
	// The generation observed by the controller.
	// +optional
	ObservedGeneration int64
	// The results of type checking for each expression.
	// Presence of this field indicates the completion of the type checking.
	// +optional
	TypeChecking *TypeChecking
	// The conditions represent the latest available observations of a policy's current state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition
}

// ValidatingAdmissionPolicyConditionType is the condition type of admission validation policy.
type ValidatingAdmissionPolicyConditionType string

// TypeChecking contains results of type checking the expressions in the
// ValidatingAdmissionPolicy
type TypeChecking struct {
	// The type checking warnings for each expression.
	// +optional
	// +listType=atomic
	ExpressionWarnings []ExpressionWarning
}

// ExpressionWarning is a warning information that targets a specific expression.
type ExpressionWarning struct {
	// The path to the field that refers the expression.
	// For example, the reference to the expression of the first item of
	// validations is "spec.validations[0].expression"
	FieldRef string
	// The content of type checking information in a human-readable form.
	// Each line of the warning contains the type that the expression is checked
	// against, followed by the type check error from the compiler.
	Warning string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ValidatingAdmissionPolicyList is a list of ValidatingAdmissionPolicy.
type ValidatingAdmissionPolicyList struct {
	metav1.TypeMeta
	// Standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// +optional
	metav1.ListMeta
	// List of ValidatingAdmissionPolicy.
	Items []ValidatingAdmissionPolicy
}

// ValidatingAdmissionPolicySpec is the specification of the desired behavior of the AdmissionPolicy.
type ValidatingAdmissionPolicySpec struct {
	// ParamKind specifies the kind of resources used to parameterize this policy.
	// If absent, there are no parameters for this policy and the param CEL variable will not be provided to validation expressions.
	// If ParamKind refers to a non-existent kind, this policy definition is mis-configured and the FailurePolicy is applied.
	// If paramKind is specified but paramRef is unset in ValidatingAdmissionPolicyBinding, the params variable will be null.
	// +optional
	ParamKind *ParamKind

	// MatchConstraints specifies what resources this policy is designed to validate.
	// The MutatingAdmissionPolicy cares about a request if it matches _all_ Constraint.
	// However, in order to prevent clusters from being put into an unstable state that cannot be recovered from via the API
	// ValidatingAdmissionPolicy cannot match ValidatingAdmissionPolicy and ValidatingAdmissionPolicyBinding.
	// Required.
	MatchConstraints *MatchResources

	// validations contain CEL expressions which are used to validate admission requests.
	// validations and auditAnnotations may not both be empty; a minimum of one validations or auditAnnotations is
	// required.
	// +optional
	Validations []Validation

	// MatchConditions is a list of conditions that must be met for a request to be validated.
	// Match conditions filter requests that have already been matched by the rules,
	// namespaceSelector, and objectSelector. An empty list of matchConditions matches all requests.
	// There are a maximum of 64 match conditions allowed.
	//
	// If a parameter object is provided, it can be accessed via the `params` handle in the same
	// manner as validation expressions.
	//
	// The exact matching logic is (in order):
	//   1. If ANY matchCondition evaluates to FALSE, the policy is skipped.
	//   2. If ALL matchConditions evaluate to TRUE, the policy is evaluated.
	//   3. If any matchCondition evaluates to an error (but none are FALSE):
	//      - If failurePolicy=Fail, reject the request
	//      - If failurePolicy=Ignore, the policy is skipped
	//
	// +optional
	MatchConditions []MatchCondition

	// failurePolicy defines how to handle failures for the admission policy. Failures can
	// occur from CEL expression parse errors, type check errors, runtime errors and invalid
	// or mis-configured policy definitions or bindings.
	//
	// A policy is invalid if spec.paramKind refers to a non-existent Kind.
	// A binding is invalid if spec.paramRef.name refers to a non-existent resource.
	//
	// failurePolicy does not define how validations that evaluate to false are handled.
	//
	// When failurePolicy is set to Fail, ValidatingAdmissionPolicyBinding validationActions
	// define how failures are enforced.
	//
	// Allowed values are Ignore or Fail. Defaults to Fail.
	// +optional
	FailurePolicy *FailurePolicyType

	// auditAnnotations contains CEL expressions which are used to produce audit
	// annotations for the audit event of the API request.
	// validations and auditAnnotations may not both be empty; a least one of validations or auditAnnotations is
	// required.
	// A maximum of 20 auditAnnotation are allowed per ValidatingAdmissionPolicy.
	// +optional
	AuditAnnotations []AuditAnnotation

	// Variables contain definitions of variables that can be used in composition of other expressions.
	// Each variable is defined as a named CEL expression.
	// The variables defined here will be available under `variables` in other expressions of the policy
	// except MatchConditions because MatchConditions are evaluated before the rest of the policy.
	//
	// The expression of a variable can refer to other variables defined earlier in the list but not those after.
	// Thus, Variables must be sorted by the order of first appearance and acyclic.
	// +listType=atomic
	// +optional
	Variables []Variable
}

// ParamKind is a tuple of Group Kind and Version.
type ParamKind struct {
	// APIVersion is the API group version the resources belong to.
	// In format of "group/version".
	// Required.
	APIVersion string

	// Kind is the API kind the resources belong to.
	// Required.
	Kind string
}

// Validation specifies the CEL expression which is used to apply the validation.
type Validation struct {
	// Expression represents the expression which will be evaluated by CEL.
	// ref: https://github.com/google/cel-spec
	// CEL expressions have access to the contents of the API request/response, organized into CEL variables as well as some other useful variables:
	//
	//'object' - The object from the incoming request. The value is null for DELETE requests.
	//'oldObject' - The existing object. The value is null for CREATE requests.
	//'request' - Attributes of the API request([ref](/pkg/apis/admission/types.go#AdmissionRequest)).
	//'params' - Parameter resource referred to by the policy binding being evaluated. Only populated if the policy has a ParamKind.
	//'namespaceObject' - The namespace object that the incoming object belongs to. The value is null for cluster-scoped resources.
	//'variables' - Map of composited variables, from its name to its lazily evaluated value.
	//  For example, a variable named 'foo' can be accessed as 'variables.foo'
	// - 'authorizer' - A CEL Authorizer. May be used to perform authorization checks for the principal (user or service account) of the request.
	//   See https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Authz
	// - 'authorizer.requestResource' - A CEL ResourceCheck constructed from the 'authorizer' and configured with the
	//   request resource.
	//
	// The `apiVersion`, `kind`, `metadata.name` and `metadata.generateName` are always accessible from the root of the
	// object. No other metadata properties are accessible.
	//
	// Only property names of the form `[a-zA-Z_.-/][a-zA-Z0-9_.-/]*` are accessible.
	// Accessible property names are escaped according to the following rules when accessed in the expression:
	// - '__' escapes to '__underscores__'
	// - '.' escapes to '__dot__'
	// - '-' escapes to '__dash__'
	// - '/' escapes to '__slash__'
	// - Property names that exactly match a CEL RESERVED keyword escape to '__{keyword}__'. The keywords are:
	//	  "true", "false", "null", "in", "as", "break", "const", "continue", "else", "for", "function", "if",
	//	  "import", "let", "loop", "package", "namespace", "return".
	// Examples:
	//   - Expression accessing a property named "namespace": {"Expression": "object.__namespace__ > 0"}
	//   - Expression accessing a property named "x-prop": {"Expression": "object.x__dash__prop > 0"}
	//   - Expression accessing a property named "redact__d": {"Expression": "object.redact__underscores__d > 0"}
	//
	// Equality on arrays with list type of 'set' or 'map' ignores element order, i.e. [1, 2] == [2, 1].
	// Concatenation on arrays with x-kubernetes-list-type use the semantics of the list type:
	//   - 'set': `X + Y` performs a union where the array positions of all elements in `X` are preserved and
	//     non-intersecting elements in `Y` are appended, retaining their partial order.
	//   - 'map': `X + Y` performs a merge where the array positions of all keys in `X` are preserved but the values
	//     are overwritten by values in `Y` when the key sets of `X` and `Y` intersect. Elements in `Y` with
	//     non-intersecting keys are appended, retaining their partial order.
	// Required.
	Expression string
	// Message represents the message displayed when validation fails. The message is required if the Expression contains
	// line breaks. The message must not contain line breaks.
	// If unset, the message is "failed rule: {Rule}".
	// e.g. "must be a URL with the host matching spec.host"
	// If ExpressMessage is specified, Message will be ignored
	// If the Expression contains line breaks. Eith Message or ExpressMessage is required.
	// The message must not contain line breaks.
	// If unset, the message is "failed Expression: {Expression}".
	// +optional
	Message string
	// Reason represents a machine-readable description of why this validation failed.
	// If this is the first validation in the list to fail, this reason, as well as the
	// corresponding HTTP response code, are used in the
	// HTTP response to the client.
	// The currently supported reasons are: "Unauthorized", "Forbidden", "Invalid", "RequestEntityTooLarge".
	// If not set, StatusReasonInvalid is used in the response to the client.
	// +optional
	Reason *metav1.StatusReason
	// messageExpression declares a CEL expression that evaluates to the validation failure message that is returned when this rule fails.
	// Since messageExpression is used as a failure message, it must evaluate to a string.
	// If both message and messageExpression are present on a validation, then messageExpression will be used if validation fails.
	// If messageExpression results in a runtime error, the runtime error is logged, and the validation failure message is produced
	// as if the messageExpression field were unset. If messageExpression evaluates to an empty string, a string with only spaces, or a string
	// that contains line breaks, then the validation failure message will also be produced as if the messageExpression field were unset, and
	// the fact that messageExpression produced an empty string/string with only spaces/string with line breaks will be logged.
	// messageExpression has access to all the same variables as the `expression` except for 'authorizer' and 'authorizer.requestResource'.
	// Example:
	// "object.x must be less than max ("+string(params.max)+")"
	// +optional
	MessageExpression string
}

// Variable is the definition of a variable that is used for composition. A variable is defined as a named expression.
// +structType=atomic
type Variable struct {
	// Name is the name of the variable. The name must be a valid CEL identifier and unique among all variables.
	// The variable can be accessed in other expressions through `variables`
	// For example, if name is "foo", the variable will be available as `variables.foo`
	Name string

	// Expression is the expression that will be evaluated as the value of the variable.
	// The CEL expression has access to the same identifiers as the CEL expressions in Validation.
	Expression string
}

// AuditAnnotation describes how to produce an audit annotation for an API request.
type AuditAnnotation struct {
	// key specifies the audit annotation key. The audit annotation keys of
	// a ValidatingAdmissionPolicy must be unique. The key must be a qualified
	// name ([A-Za-z0-9][-A-Za-z0-9_.]*) no more than 63 bytes in length.
	//
	// The key is combined with the resource name of the
	// ValidatingAdmissionPolicy to construct an audit annotation key:
	// "{ValidatingAdmissionPolicy name}/{key}".
	//
	// If an admission webhook uses the same resource name as this ValidatingAdmissionPolicy
	// and the same audit annotation key, the annotation key will be identical.
	// In this case, the first annotation written with the key will be included
	// in the audit event and all subsequent annotations with the same key
	// will be discarded.
	//
	// Required.
	Key string

	// valueExpression represents the expression which is evaluated by CEL to
	// produce an audit annotation value. The expression must evaluate to either
	// a string or null value. If the expression evaluates to a string, the
	// audit annotation is included with the string value. If the expression
	// evaluates to null or empty string the audit annotation will be omitted.
	// The valueExpression may be no longer than 5kb in length.
	// If the result of the valueExpression is more than 10kb in length, it
	// will be truncated to 10kb.
	//
	// If multiple ValidatingAdmissionPolicyBinding resources match an
	// API request, then the valueExpression will be evaluated for
	// each binding. All unique values produced by the valueExpressions
	// will be joined together in a comma-separated list.
	//
	// Required.
	ValueExpression string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ValidatingAdmissionPolicyBinding binds the ValidatingAdmissionPolicy with paramerized resources.
// ValidatingAdmissionPolicyBinding and parameter CRDs together define how cluster administrators configure policies for clusters.
//
// For a given admission request, each binding will cause its policy to be
// evaluated N times, where N is 1 for policies/bindings that don't use
// params, otherwise N is the number of parameters selected by the binding.
//
// The CEL expressions of a policy must have a computed CEL cost below the maximum
// CEL budget. Each evaluation of the policy is given an independent CEL cost budget.
// Adding/removing policies, bindings, or params can not affect whether a
// given (policy, binding, param) combination is within its own CEL budget.
type ValidatingAdmissionPolicyBinding struct {
	metav1.TypeMeta
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta
	// Specification of the desired behavior of the ValidatingAdmissionPolicyBinding.
	Spec ValidatingAdmissionPolicyBindingSpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ValidatingAdmissionPolicyBindingList is a list of PolicyBinding.
type ValidatingAdmissionPolicyBindingList struct {
	metav1.TypeMeta
	// Standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// +optional
	metav1.ListMeta
	// List of PolicyBinding.
	Items []ValidatingAdmissionPolicyBinding
}

// ValidatingAdmissionPolicyBindingSpec is the specification of the ValidatingAdmissionPolicyBinding.
type ValidatingAdmissionPolicyBindingSpec struct {
	// PolicyName references a ValidatingAdmissionPolicy name which the ValidatingAdmissionPolicyBinding binds to.
	// If the referenced resource does not exist, this binding is considered invalid and will be ignored
	// Required.
	PolicyName string

	// paramRef specifies the parameter resource used to configure the admission control policy.
	// It should point to a resource of the type specified in ParamKind of the bound ValidatingAdmissionPolicy.
	// If the policy specifies a ParamKind and the resource referred to by ParamRef does not exist, this binding is considered mis-configured and the FailurePolicy of the ValidatingAdmissionPolicy applied.
	// If the policy does not specify a ParamKind then this field is ignored, and the rules are evaluated without a param.
	// +optional
	ParamRef *ParamRef

	// MatchResources declares what resources match this binding and will be validated by it.
	// Note that this is intersected with the policy's matchConstraints, so only requests that are matched by the policy can be selected by this.
	// If this is unset, all resources matched by the policy are validated by this binding
	// When resourceRules is unset, it does not constrain resource matching. If a resource is matched by the other fields of this object, it will be validated.
	// Note that this is differs from ValidatingAdmissionPolicy matchConstraints, where resourceRules are required.
	// +optional
	MatchResources *MatchResources

	// validationActions declares how Validations of the referenced ValidatingAdmissionPolicy are enforced.
	// If a validation evaluates to false it is always enforced according to these actions.
	//
	// Failures defined by the ValidatingAdmissionPolicy's FailurePolicy are enforced according
	// to these actions only if the FailurePolicy is set to Fail, otherwise the failures are
	// ignored. This includes compilation errors, runtime errors and misconfigurations of the policy.
	//
	// validationActions is declared as a set of action values. Order does
	// not matter. validationActions may not contain duplicates of the same action.
	//
	// The supported actions values are:
	//
	// "Deny" specifies that a validation failure results in a denied request.
	//
	// "Warn" specifies that a validation failure is reported to the request client
	// in HTTP Warning headers, with a warning code of 299. Warnings can be sent
	// both for allowed or denied admission responses.
	//
	// "Audit" specifies that a validation failure is included in the published
	// audit event for the request. The audit event will contain a
	// `validation.policy.admission.k8s.io/validation_failure` audit annotation
	// with a value containing the details of the validation failures, formatted as
	// a JSON list of objects, each with the following fields:
	// - message: The validation failure message string
	// - policy: The resource name of the ValidatingAdmissionPolicy
	// - binding: The resource name of the ValidatingAdmissionPolicyBinding
	// - expressionIndex: The index of the failed validations in the ValidatingAdmissionPolicy
	// - validationActions: The enforcement actions enacted for the validation failure
	// Example audit annotation:
	// `"validation.policy.admission.k8s.io/validation_failure": "[{\"message\": \"Invalid value\", {\"policy\": \"policy.example.com\", {\"binding\": \"policybinding.example.com\", {\"expressionIndex\": \"1\", {\"validationActions\": [\"Audit\"]}]"`
	//
	// Clients should expect to handle additional values by ignoring
	// any values not recognized.
	//
	// "Deny" and "Warn" may not be used together since this combination
	// needlessly duplicates the validation failure both in the
	// API response body and the HTTP warning headers.
	//
	// Required.
	ValidationActions []ValidationAction
}

// ParamRef describes how to locate the params to be used as input to
// expressions of rules applied by a policy binding.
// +structType=atomic
type ParamRef struct {
	// name is the name of the resource being referenced.
	//
	// One of `name` or `selector` must be set, but `name` and `selector` are
	// mutually exclusive properties. If one is set, the other must be unset.
	//
	// A single parameter used for all admission requests can be configured
	// by setting the `name` field, leaving `selector` blank, and setting namespace
	// if `paramKind` is namespace-scoped.
	//
	// +optional
	Name string

	// namespace is the namespace of the referenced resource. Allows limiting
	// the search for params to a specific namespace. Applies to both `name` and
	// `selector` fields.
	//
	// A per-namespace parameter may be used by specifying a namespace-scoped
	// `paramKind` in the policy and leaving this field empty.
	//
	// - If `paramKind` is cluster-scoped, this field MUST be unset. Setting this
	// field results in a configuration error.
	//
	// - If `paramKind` is namespace-scoped, the namespace of the object being
	// evaluated for admission will be used when this field is left unset. Take
	// care that if this is left empty the binding must not match any cluster-scoped
	// resources, which will result in an error.
	//
	// +optional
	Namespace string

	// selector can be used to match multiple param objects based on their labels.
	// Supply selector: {} to match all resources of the ParamKind.
	//
	// If multiple params are found, they are all evaluated with the policy expressions
	// and the results are ANDed together.
	//
	// One of `name` or `selector` must be set, but `name` and `selector` are
	// mutually exclusive properties. If one is set, the other must be unset.
	//
	// +optional
	Selector *metav1.LabelSelector

	// parameterNotFoundAction controls the behavior of the binding when the resource
	// exists, and name or selector is valid, but there are no parameters
	// matched by the binding. If the value is set to `Allow`, then no
	// matched parameters will be treated as successful validation by the binding.
	// If set to `Deny`, then no matched parameters will be subject to the
	// `failurePolicy` of the policy.
	//
	// Allowed values are `Allow` or `Deny`
	//
	// Required
	ParameterNotFoundAction *ParameterNotFoundActionType
}

// MatchResources decides whether to run the admission control policy on an object based
// on whether it meets the match criteria.
// The exclude rules take precedence over include rules (if a resource matches both, it is excluded)
type MatchResources struct {
	// NamespaceSelector decides whether to run the admission control policy on an object based
	// on whether the namespace for that object matches the selector. If the
	// object itself is a namespace, the matching is performed on
	// object.metadata.labels. If the object is another cluster scoped resource,
	// it never skips the policy.
	//
	// For example, to run the webhook on any objects whose namespace is not
	// associated with "runlevel" of "0" or "1";  you will set the selector as
	// follows:
	// "namespaceSelector": {
	//   "matchExpressions": [
	//     {
	//       "key": "runlevel",
	//       "operator": "NotIn",
	//       "values": [
	//         "0",
	//         "1"
	//       ]
	//     }
	//   ]
	// }
	//
	// If instead you want to only run the policy on any objects whose
	// namespace is associated with the "environment" of "prod" or "staging";
	// you will set the selector as follows:
	// "namespaceSelector": {
	//   "matchExpressions": [
	//     {
	//       "key": "environment",
	//       "operator": "In",
	//       "values": [
	//         "prod",
	//         "staging"
	//       ]
	//     }
	//   ]
	// }
	//
	// See
	// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/
	// for more examples of label selectors.
	//
	// Default to the empty LabelSelector, which matches everything.
	// +optional
	NamespaceSelector *metav1.LabelSelector
	// ObjectSelector decides whether to run the policy based on if the
	// object has matching labels. objectSelector is evaluated against both
	// the oldObject and newObject that would be sent to the cel policy, and
	// is considered to match if either object matches the selector. A null
	// object (oldObject in the case of create, or newObject in the case of
	// delete) or an object that cannot have labels (like a
	// DeploymentRollback or a PodProxyOptions object) is not considered to
	// match.
	// Use the object selector only if the webhook is opt-in, because end
	// users may skip the admission webhook by setting the labels.
	// Default to the empty LabelSelector, which matches everything.
	// +optional
	ObjectSelector *metav1.LabelSelector
	// ResourceRules describes what operations on what resources/subresources the policy matches.
	// The policy cares about an operation if it matches _any_ Rule.
	// +listType=atomic
	// +optional
	ResourceRules []NamedRuleWithOperations
	// ExcludeResourceRules describes what operations on what resources/subresources the policy should not care about.
	// The exclude rules take precedence over include rules (if a resource matches both, it is excluded)
	// +listType=atomic
	// +optional
	ExcludeResourceRules []NamedRuleWithOperations
	// matchPolicy defines how the "MatchResources" list is used to match incoming requests.
	// Allowed values are "Exact" or "Equivalent".
	//
	// - Exact: match a request only if it exactly matches a specified rule.
	// For example, if deployments can be modified via apps/v1, apps/v1beta1, and extensions/v1beta1,
	// but "rules" only included `apiGroups:["apps"], apiVersions:["v1"], resources: ["deployments"]`,
	// a request to apps/v1beta1 or extensions/v1beta1 would not be sent to the policy.
	//
	// - Equivalent: match a request if modifies a resource listed in rules, even via another API group or version.
	// For example, if deployments can be modified via apps/v1, apps/v1beta1, and extensions/v1beta1,
	// and "rules" only included `apiGroups:["apps"], apiVersions:["v1"], resources: ["deployments"]`,
	// a request to apps/v1beta1 or extensions/v1beta1 would be converted to apps/v1 and sent to the policy.
	//
	// Defaults to "Equivalent"
	// +optional
	MatchPolicy *MatchPolicyType
}

// ValidationAction specifies a policy enforcement action.
type ValidationAction string

const (
	// Deny specifies that a validation failure results in a denied request.
	Deny ValidationAction = "Deny"
	// Warn specifies that a validation failure is reported to the request client
	// in HTTP Warning headers, with a warning code of 299. Warnings can be sent
	// both for allowed or denied admission responses.
	Warn ValidationAction = "Warn"
	// Audit specifies that a validation failure is included in the published
	// audit event for the request. The audit event will contain a
	// `validation.policy.admission.k8s.io/validation_failure` audit annotation
	// with a value containing the details of the validation failure.
	Audit ValidationAction = "Audit"
)

// NamedRuleWithOperations is a tuple of Operations and Resources with ResourceNames.
type NamedRuleWithOperations struct {
	// ResourceNames is an optional white list of names that the rule applies to.  An empty set means that everything is allowed.
	// +optional
	ResourceNames []string
	// RuleWithOperations is a tuple of Operations and Resources.
	RuleWithOperations RuleWithOperations
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ValidatingWebhookConfiguration describes the configuration of an admission webhook that accepts or rejects and object without changing it.
type ValidatingWebhookConfiguration struct {
	metav1.TypeMeta
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta
	// Webhooks is a list of webhooks and the affected resources and operations.
	// +optional
	Webhooks []ValidatingWebhook
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ValidatingWebhookConfigurationList is a list of ValidatingWebhookConfiguration.
type ValidatingWebhookConfigurationList struct {
	metav1.TypeMeta
	// Standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// +optional
	metav1.ListMeta
	// List of ValidatingWebhookConfigurations.
	Items []ValidatingWebhookConfiguration
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MutatingWebhookConfiguration describes the configuration of and admission webhook that accept or reject and may change the object.
type MutatingWebhookConfiguration struct {
	metav1.TypeMeta
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta
	// Webhooks is a list of webhooks and the affected resources and operations.
	// +optional
	Webhooks []MutatingWebhook
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MutatingWebhookConfigurationList is a list of MutatingWebhookConfiguration.
type MutatingWebhookConfigurationList struct {
	metav1.TypeMeta
	// Standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// +optional
	metav1.ListMeta
	// List of MutatingWebhookConfiguration.
	Items []MutatingWebhookConfiguration
}

// ValidatingWebhook describes an admission webhook and the resources and operations it applies to.
type ValidatingWebhook struct {
	// The name of the admission webhook.
	// Name should be fully qualified, e.g., imagepolicy.kubernetes.io, where
	// "imagepolicy" is the name of the webhook, and kubernetes.io is the name
	// of the organization.
	// Required.
	Name string

	// ClientConfig defines how to communicate with the hook.
	// Required
	ClientConfig WebhookClientConfig

	// Rules describes what operations on what resources/subresources the webhook cares about.
	// The webhook cares about an operation if it matches _any_ Rule.
	Rules []RuleWithOperations

	// FailurePolicy defines how unrecognized errors from the admission endpoint are handled -
	// allowed values are Ignore or Fail. Defaults to Ignore.
	// +optional
	FailurePolicy *FailurePolicyType

	// matchPolicy defines how the "rules" list is used to match incoming requests.
	// Allowed values are "Exact" or "Equivalent".
	//
	// - Exact: match a request only if it exactly matches a specified rule.
	// For example, if deployments can be modified via apps/v1, apps/v1beta1, and extensions/v1beta1,
	// but "rules" only included `apiGroups:["apps"], apiVersions:["v1"], resources: ["deployments"]`,
	// a request to apps/v1beta1 or extensions/v1beta1 would not be sent to the webhook.
	//
	// - Equivalent: match a request if modifies a resource listed in rules, even via another API group or version.
	// For example, if deployments can be modified via apps/v1, apps/v1beta1, and extensions/v1beta1,
	// and "rules" only included `apiGroups:["apps"], apiVersions:["v1"], resources: ["deployments"]`,
	// a request to apps/v1beta1 or extensions/v1beta1 would be converted to apps/v1 and sent to the webhook.
	//
	// +optional
	MatchPolicy *MatchPolicyType

	// NamespaceSelector decides whether to run the webhook on an object based
	// on whether the namespace for that object matches the selector. If the
	// object itself is a namespace, the matching is performed on
	// object.metadata.labels. If the object is another cluster scoped resource,
	// it never skips the webhook.
	//
	// For example, to run the webhook on any objects whose namespace is not
	// associated with "runlevel" of "0" or "1";  you will set the selector as
	// follows:
	// "namespaceSelector": {
	//   "matchExpressions": [
	//     {
	//       "key": "runlevel",
	//       "operator": "NotIn",
	//       "values": [
	//         "0",
	//         "1"
	//       ]
	//     }
	//   ]
	// }
	//
	// If instead you want to only run the webhook on any objects whose
	// namespace is associated with the "environment" of "prod" or "staging";
	// you will set the selector as follows:
	// "namespaceSelector": {
	//   "matchExpressions": [
	//     {
	//       "key": "environment",
	//       "operator": "In",
	//       "values": [
	//         "prod",
	//         "staging"
	//       ]
	//     }
	//   ]
	// }
	//
	// See
	// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/
	// for more examples of label selectors.
	//
	// Default to the empty LabelSelector, which matches everything.
	// +optional
	NamespaceSelector *metav1.LabelSelector

	// ObjectSelector decides whether to run the webhook based on if the
	// object has matching labels. objectSelector is evaluated against both
	// the oldObject and newObject that would be sent to the webhook, and
	// is considered to match if either object matches the selector. A null
	// object (oldObject in the case of create, or newObject in the case of
	// delete) or an object that cannot have labels (like a
	// DeploymentRollback or a PodProxyOptions object) is not considered to
	// match.
	// Use the object selector only if the webhook is opt-in, because end
	// users may skip the admission webhook by setting the labels.
	// Default to the empty LabelSelector, which matches everything.
	// +optional
	ObjectSelector *metav1.LabelSelector

	// SideEffects states whether this webhook has side effects.
	// Acceptable values are: Unknown, None, Some, NoneOnDryRun
	// Webhooks with side effects MUST implement a reconciliation system, since a request may be
	// rejected by a future step in the admission chain and the side effects therefore need to be undone.
	// Requests with the dryRun attribute will be auto-rejected if they match a webhook with
	// sideEffects == Unknown or Some. Defaults to Unknown.
	// +optional
	SideEffects *SideEffectClass

	// TimeoutSeconds specifies the timeout for this webhook. After the timeout passes,
	// the webhook call will be ignored or the API call will fail based on the
	// failure policy.
	// The timeout value must be between 1 and 30 seconds.
	// +optional
	TimeoutSeconds *int32

	// AdmissionReviewVersions is an ordered list of preferred `AdmissionReview`
	// versions the Webhook expects. API server will try to use first version in
	// the list which it supports. If none of the versions specified in this list
	// supported by API server, validation will fail for this object.
	// If the webhook configuration has already been persisted with a version apiserver
	// does not understand, calls to the webhook will fail and be subject to the failure policy.
	// +optional
	AdmissionReviewVersions []string

	// MatchConditions is a list of conditions that must be met for a request to be sent to this
	// webhook. Match conditions filter requests that have already been matched by the rules,
	// namespaceSelector, and objectSelector. An empty list of matchConditions matches all requests.
	// There are a maximum of 64 match conditions allowed.
	//
	// The exact matching logic is (in order):
	//   1. If ANY matchCondition evaluates to FALSE, the webhook is skipped.
	//   2. If ALL matchConditions evaluate to TRUE, the webhook is called.
	//   3. If any matchCondition evaluates to an error (but none are FALSE):
	//      - If failurePolicy=Fail, reject the request
	//      - If failurePolicy=Ignore, the error is ignored and the webhook is skipped
	//
	// +optional
	MatchConditions []MatchCondition
}

// MutatingWebhook describes an admission webhook and the resources and operations it applies to.
type MutatingWebhook struct {
	// The name of the admission webhook.
	// Name should be fully qualified, e.g., imagepolicy.kubernetes.io, where
	// "imagepolicy" is the name of the webhook, and kubernetes.io is the name
	// of the organization.
	// Required.
	Name string

	// ClientConfig defines how to communicate with the hook.
	// Required
	ClientConfig WebhookClientConfig

	// Rules describes what operations on what resources/subresources the webhook cares about.
	// The webhook cares about an operation if it matches _any_ Rule.
	Rules []RuleWithOperations

	// FailurePolicy defines how unrecognized errors from the admission endpoint are handled -
	// allowed values are Ignore or Fail. Defaults to Ignore.
	// +optional
	FailurePolicy *FailurePolicyType

	// matchPolicy defines how the "rules" list is used to match incoming requests.
	// Allowed values are "Exact" or "Equivalent".
	//
	// - Exact: match a request only if it exactly matches a specified rule.
	// For example, if deployments can be modified via apps/v1, apps/v1beta1, and extensions/v1beta1,
	// but "rules" only included `apiGroups:["apps"], apiVersions:["v1"], resources: ["deployments"]`,
	// a request to apps/v1beta1 or extensions/v1beta1 would not be sent to the webhook.
	//
	// - Equivalent: match a request if modifies a resource listed in rules, even via another API group or version.
	// For example, if deployments can be modified via apps/v1, apps/v1beta1, and extensions/v1beta1,
	// and "rules" only included `apiGroups:["apps"], apiVersions:["v1"], resources: ["deployments"]`,
	// a request to apps/v1beta1 or extensions/v1beta1 would be converted to apps/v1 and sent to the webhook.
	//
	// +optional
	MatchPolicy *MatchPolicyType

	// NamespaceSelector decides whether to run the webhook on an object based
	// on whether the namespace for that object matches the selector. If the
	// object itself is a namespace, the matching is performed on
	// object.metadata.labels. If the object is another cluster scoped resource,
	// it never skips the webhook.
	//
	// For example, to run the webhook on any objects whose namespace is not
	// associated with "runlevel" of "0" or "1";  you will set the selector as
	// follows:
	// "namespaceSelector": {
	//   "matchExpressions": [
	//     {
	//       "key": "runlevel",
	//       "operator": "NotIn",
	//       "values": [
	//         "0",
	//         "1"
	//       ]
	//     }
	//   ]
	// }
	//
	// If instead you want to only run the webhook on any objects whose
	// namespace is associated with the "environment" of "prod" or "staging";
	// you will set the selector as follows:
	// "namespaceSelector": {
	//   "matchExpressions": [
	//     {
	//       "key": "environment",
	//       "operator": "In",
	//       "values": [
	//         "prod",
	//         "staging"
	//       ]
	//     }
	//   ]
	// }
	//
	// See
	// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/
	// for more examples of label selectors.
	//
	// Default to the empty LabelSelector, which matches everything.
	// +optional
	NamespaceSelector *metav1.LabelSelector

	// ObjectSelector decides whether to run the webhook based on if the
	// object has matching labels. objectSelector is evaluated against both
	// the oldObject and newObject that would be sent to the webhook, and
	// is considered to match if either object matches the selector. A null
	// object (oldObject in the case of create, or newObject in the case of
	// delete) or an object that cannot have labels (like a
	// DeploymentRollback or a PodProxyOptions object) is not considered to
	// match.
	// Use the object selector only if the webhook is opt-in, because end
	// users may skip the admission webhook by setting the labels.
	// Default to the empty LabelSelector, which matches everything.
	// +optional
	ObjectSelector *metav1.LabelSelector

	// SideEffects states whether this webhook has side effects.
	// Acceptable values are: Unknown, None, Some, NoneOnDryRun
	// Webhooks with side effects MUST implement a reconciliation system, since a request may be
	// rejected by a future step in the admission chain and the side effects therefore need to be undone.
	// Requests with the dryRun attribute will be auto-rejected if they match a webhook with
	// sideEffects == Unknown or Some. Defaults to Unknown.
	// +optional
	SideEffects *SideEffectClass

	// TimeoutSeconds specifies the timeout for this webhook. After the timeout passes,
	// the webhook call will be ignored or the API call will fail based on the
	// failure policy.
	// The timeout value must be between 1 and 30 seconds.
	// +optional
	TimeoutSeconds *int32

	// AdmissionReviewVersions is an ordered list of preferred `AdmissionReview`
	// versions the Webhook expects. API server will try to use first version in
	// the list which it supports. If none of the versions specified in this list
	// supported by API server, validation will fail for this object.
	// If the webhook configuration has already been persisted with a version apiserver
	// does not understand, calls to the webhook will fail and be subject to the failure policy.
	// +optional
	AdmissionReviewVersions []string

	// reinvocationPolicy indicates whether this webhook should be called multiple times as part of a single admission evaluation.
	// Allowed values are "Never" and "IfNeeded".
	//
	// Never: the webhook will not be called more than once in a single admission evaluation.
	//
	// IfNeeded: the webhook will be called at least one additional time as part of the admission evaluation
	// if the object being admitted is modified by other admission plugins after the initial webhook call.
	// Webhooks that specify this option *must* be idempotent, and hence able to process objects they previously admitted.
	// Note:
	// * the number of additional invocations is not guaranteed to be exactly one.
	// * if additional invocations result in further modifications to the object, webhooks are not guaranteed to be invoked again.
	// * webhooks that use this option may be reordered to minimize the number of additional invocations.
	// * to validate an object after all mutations are guaranteed complete, use a validating admission webhook instead.
	//
	// Defaults to "Never".
	// +optional
	ReinvocationPolicy *ReinvocationPolicyType

	// MatchConditions is a list of conditions that must be met for a request to be sent to this
	// webhook. Match conditions filter requests that have already been matched by the rules,
	// namespaceSelector, and objectSelector. An empty list of matchConditions matches all requests.
	// There are a maximum of 64 match conditions allowed.
	//
	// The exact matching logic is (in order):
	//   1. If ANY matchCondition evaluates to FALSE, the webhook is skipped.
	//   2. If ALL matchConditions evaluate to TRUE, the webhook is called.
	//   3. If any matchCondition evaluates to an error (but none are FALSE):
	//      - If failurePolicy=Fail, reject the request
	//      - If failurePolicy=Ignore, the error is ignored and the webhook is skipped
	//
	// +optional
	MatchConditions []MatchCondition
}

// ReinvocationPolicyType specifies what type of policy the admission hook uses.
type ReinvocationPolicyType string

var (
	// NeverReinvocationPolicy indicates that the webhook must not be called more than once in a
	// single admission evaluation.
	NeverReinvocationPolicy ReinvocationPolicyType = "Never"
	// IfNeededReinvocationPolicy indicates that the webhook may be called at least one
	// additional time as part of the admission evaluation if the object being admitted is
	// modified by other admission plugins after the initial webhook call.
	IfNeededReinvocationPolicy ReinvocationPolicyType = "IfNeeded"
)

// RuleWithOperations is a tuple of Operations and Resources. It is recommended to make
// sure that all the tuple expansions are valid.
type RuleWithOperations struct {
	// Operations is the operations the admission hook cares about - CREATE, UPDATE, or *
	// for all operations.
	// If '*' is present, the length of the slice must be one.
	// Required.
	Operations []OperationType
	// Rule is embedded, it describes other criteria of the rule, like
	// APIGroups, APIVersions, Resources, etc.
	Rule
}

// OperationType specifies what type of operation the admission hook cares about.
type OperationType string

// The constants should be kept in sync with those defined in k8s.io/kubernetes/pkg/admission/interface.go.
const (
	OperationAll OperationType = "*"
	Create       OperationType = "CREATE"
	Update       OperationType = "UPDATE"
	Delete       OperationType = "DELETE"
	Connect      OperationType = "CONNECT"
)

// WebhookClientConfig contains the information to make a TLS
// connection with the webhook
type WebhookClientConfig struct {
	// `url` gives the location of the webhook, in standard URL form
	// (`scheme://host:port/path`). Exactly one of `url` or `service`
	// must be specified.
	//
	// The `host` should not refer to a service running in the cluster; use
	// the `service` field instead. The host might be resolved via external
	// DNS in some apiservers (e.g., `kube-apiserver` cannot resolve
	// in-cluster DNS as that would be a layering violation). `host` may
	// also be an IP address.
	//
	// Please note that using `localhost` or `127.0.0.1` as a `host` is
	// risky unless you take great care to run this webhook on all hosts
	// which run an apiserver which might need to make calls to this
	// webhook. Such installs are likely to be non-portable, i.e., not easy
	// to turn up in a new cluster.
	//
	// The scheme must be "https"; the URL must begin with "https://".
	//
	// A path is optional, and if present may be any string permissible in
	// a URL. You may use the path to pass an arbitrary string to the
	// webhook, for example, a cluster identifier.
	//
	// Attempting to use a user or basic auth e.g. "user:password@" is not
	// allowed. Fragments ("#...") and query parameters ("?...") are not
	// allowed, either.
	//
	// +optional
	URL *string

	// `service` is a reference to the service for this webhook. Either
	// `service` or `url` must be specified.
	//
	// If the webhook is running within the cluster, then you should use `service`.
	//
	// +optional
	Service *ServiceReference

	// `caBundle` is a PEM encoded CA bundle which will be used to validate the webhook's server certificate.
	// If unspecified, system trust roots on the apiserver are used.
	// +optional
	CABundle []byte
}

// ServiceReference holds a reference to Service.legacy.k8s.io
type ServiceReference struct {
	// `namespace` is the namespace of the service.
	// Required
	Namespace string
	// `name` is the name of the service.
	// Required
	Name string

	// `path` is an optional URL path which will be sent in any request to
	// this service.
	// +optional
	Path *string

	// If specified, the port on the service that hosting webhook.
	// `port` should be a valid port number (1-65535, inclusive).
	// +optional
	Port int32
}

// MatchCondition represents a condition which must by fulfilled for a request to be sent to a webhook.
type MatchCondition struct {
	// Name is an identifier for this match condition, used for strategic merging of MatchConditions,
	// as well as providing an identifier for logging purposes. A good name should be descriptive of
	// the associated expression.
	// Name must be a qualified name consisting of alphanumeric characters, '-', '_' or '.', and
	// must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or
	// '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]') with an
	// optional DNS subdomain prefix and '/' (e.g. 'example.com/MyName')
	//
	// Required.
	Name string

	// Expression represents the expression which will be evaluated by CEL. Must evaluate to bool.
	// CEL expressions have access to the contents of the AdmissionRequest and Authorizer, organized into CEL variables:
	//
	// 'object' - The object from the incoming request. The value is null for DELETE requests.
	// 'oldObject' - The existing object. The value is null for CREATE requests.
	// 'request' - Attributes of the admission request(/pkg/apis/admission/types.go#AdmissionRequest).
	// 'authorizer' - A CEL Authorizer. May be used to perform authorization checks for the principal (user or service account) of the request.
	//   See https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Authz
	// 'authorizer.requestResource' - A CEL ResourceCheck constructed from the 'authorizer' and configured with the
	//   request resource.
	// 'variables' - Map of composited variables, from its name to its lazily evaluated value.
	//   For example, a variable named 'foo' can be access as 'variables.foo'
	// Documentation on CEL: https://kubernetes.io/docs/reference/using-api/cel/
	//
	// Required.
	Expression string
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.32

// MutatingAdmissionPolicy describes an admission policy that may mutate an object.
type MutatingAdmissionPolicy struct {
	metav1.TypeMeta
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta
	// Specification of the desired behavior of the MutatingAdmissionPolicy.
	Spec MutatingAdmissionPolicySpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.32

// MutatingAdmissionPolicyList is a list of MutatingAdmissionPolicy.
type MutatingAdmissionPolicyList struct {
	metav1.TypeMeta
	// Standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// +optional
	metav1.ListMeta
	// List of ValidatingAdmissionPolicy.
	Items []MutatingAdmissionPolicy
}

// MutatingAdmissionPolicySpec is the specification of the desired behavior of the admission policy.
type MutatingAdmissionPolicySpec struct {
	// paramKind specifies the kind of resources used to parameterize this policy.
	// If absent, there are no parameters for this policy and the param CEL variable will not be provided to validation expressions.
	// If paramKind refers to a non-existent kind, this policy definition is mis-configured and the FailurePolicy is applied.
	// If paramKind is specified but paramRef is unset in MutatingAdmissionPolicyBinding, the params variable will be null.
	// +optional
	ParamKind *ParamKind

	// matchConstraints specifies what resources this policy is designed to validate.
	// The AdmissionPolicy cares about a request if it matches _all_ Constraints.
	// However, in order to prevent clusters from being put into an unstable state that cannot be recovered from via the API
	// MutatingAdmissionPolicy cannot match MutatingAdmissionPolicy and MutatingAdmissionPolicyBinding.
	// Only the CREATE, UPDATE, and CONNECT operations are allowed.
	// '*' matches only CREATE, UPDATE, and CONNECT.
	// Required.
	MatchConstraints *MatchResources

	// variables contain definitions of variables that can be used in composition of other expressions.
	// Each variable is defined as a named CEL expression.
	// The variables defined here will be available under `variables` in other expressions of the policy
	// except matchConditions because matchConditions are evaluated before the rest of the policy.
	//
	// The expression of a variable can refer to other variables defined earlier in the list but not those after.
	// Thus, variables must be sorted by the order of first appearance and acyclic.
	// +listType=atomic
	// +optional
	Variables []Variable

	// mutations contain operations to perform on matching objects.
	// mutations may not be empty; a minimum of one mutation is required.
	// mutations are evaluated in order, and are reinvoked according to
	// the reinvocationPolicy.
	// The mutations of a policy are invoked for each binding of this policy
	// and reinvocation of mutations occurs on a per binding basis.
	//
	// +listType=atomic
	// +optional
	Mutations []Mutation

	// failurePolicy defines how to handle failures for the admission policy. Failures can
	// occur from CEL expression parse errors, type check errors, runtime errors and invalid
	// or mis-configured policy definitions or bindings.
	//
	// A policy is invalid if paramKind refers to a non-existent Kind.
	// A binding is invalid if paramRef.name refers to a non-existent resource.
	//
	// failurePolicy does not define how validations that evaluate to false are handled.
	//
	// Allowed values are Ignore or Fail. Defaults to Fail.
	// +optional
	FailurePolicy *FailurePolicyType

	// matchConditions is a list of conditions that must be met for a request to be validated.
	// Match conditions filter requests that have already been matched by the matchConstraints,
	// An empty list of matchConditions matches all requests.
	// There are a maximum of 64 match conditions allowed.
	//
	// If a parameter object is provided, it can be accessed via the `params` handle in the same
	// manner as validation expressions.
	//
	// The exact matching logic is (in order):
	//   1. If ANY matchCondition evaluates to FALSE, the policy is skipped.
	//   2. If ALL matchConditions evaluate to TRUE, the policy is evaluated.
	//   3. If any matchCondition evaluates to an error (but none are FALSE):
	//      - If failurePolicy=Fail, reject the request
	//      - If failurePolicy=Ignore, the policy is skipped
	//
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	// +optional
	MatchConditions []MatchCondition

	// reinvocationPolicy indicates whether mutations may be called multiple times per MutatingAdmissionPolicyBinding
	// as part of a single admission evaluation.
	// Allowed values are "Never" and "IfNeeded".
	//
	// Never: These mutations will not be called more than once per binding in a single admission evaluation.
	//
	// IfNeeded: These mutations may be invoked more than once per binding for a single admission request and there is no guarantee of
	// order with respect to other admission plugins, admission webhooks, bindings of this policy and admission policies.  Mutations are only
	// reinvoked when mutations change the object after this mutation is invoked.
	// Required.
	ReinvocationPolicy ReinvocationPolicyType
}

// Mutation specifies the operation that performs a Mutation.
type Mutation struct {
	// patchType indicates the patch strategy used.
	// Allowed values are "ApplyConfiguration" and "JSONPatch".
	// Required.
	//
	// +unionDiscriminator
	PatchType PatchType

	// applyConfiguration defines the desired configuration values of an object.
	// The configuration is applied to the admission object using
	// [structured merge diff](https://github.com/kubernetes-sigs/structured-merge-diff).
	// A CEL expression is used to create apply configuration.
	ApplyConfiguration *ApplyConfiguration

	// jsonPatch defines a [JSON patch](https://jsonpatch.com/) to perform a mutation to the object.
	// A CEL expression is used to create the JSON patch.
	JSONPatch *JSONPatch
}

// PatchType specifies the type of patch operation for a mutation.
// +enum
type PatchType string

const (
	// ApplyConfiguration indicates that the mutation is using apply configuration to mutate the object.
	PatchTypeApplyConfiguration PatchType = "ApplyConfiguration"
	// JSONPatch indicates that the object is mutated through JSON Patch.
	PatchTypeJSONPatch PatchType = "JSONPatch"
)

// ApplyConfiguration defines the desired configuration values of an object.
type ApplyConfiguration struct {
	// expression will be evaluated by CEL to create an apply configuration.
	// ref: https://github.com/google/cel-spec
	//
	// Apply configurations are declared in CEL using object initialization. For example, this CEL expression
	// returns an apply configuration to set a single field:
	//
	//	Object{
	//	  spec: Object.spec{
	//	    serviceAccountName: "example"
	//	  }
	//	}
	//
	// Apply configurations may not modify atomic structs, maps or arrays due to the risk of accidental deletion of
	// values not included in the apply configuration.
	//
	// CEL expressions have access to the object types needed to create apply configurations:
	// - 'Object' - CEL type of the resource object.
	// - 'Object.<fieldName>' - CEL type of object field (such as 'Object.spec')
	// - 'Object.<fieldName1>.<fieldName2>...<fieldNameN>` - CEL type of nested field (such as 'Object.spec.containers')
	//
	// CEL expressions have access to the contents of the API request, organized into CEL variables as well as some other useful variables:
	//
	// - 'object' - The object from the incoming request. The value is null for DELETE requests.
	// - 'oldObject' - The existing object. The value is null for CREATE requests.
	// - 'request' - Attributes of the API request([ref](/pkg/apis/admission/types.go#AdmissionRequest)).
	// - 'params' - Parameter resource referred to by the policy binding being evaluated. Only populated if the policy has a ParamKind.
	// - 'namespaceObject' - The namespace object that the incoming object belongs to. The value is null for cluster-scoped resources.
	// - 'variables' - Map of composited variables, from its name to its lazily evaluated value.
	//   For example, a variable named 'foo' can be accessed as 'variables.foo'.
	// - 'authorizer' - A CEL Authorizer. May be used to perform authorization checks for the principal (user or service account) of the request.
	//   See https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Authz
	// - 'authorizer.requestResource' - A CEL ResourceCheck constructed from the 'authorizer' and configured with the
	//   request resource.
	//
	// The `apiVersion`, `kind`, `metadata.name` and `metadata.generateName` are always accessible from the root of the
	// object. No other metadata properties are accessible.
	//
	// Only property names of the form `[a-zA-Z_.-/][a-zA-Z0-9_.-/]*` are accessible.
	// Required.
	Expression string
}

// JSONPatch defines a JSON Patch.
type JSONPatch struct {
	// expression will be evaluated by CEL to create a [JSON patch](https://jsonpatch.com/).
	// ref: https://github.com/google/cel-spec
	//
	// expression must return an array of JSONPatch values.
	//
	// For example, this CEL expression returns a JSON patch to conditionally modify a value:
	//
	//	  [
	//	    JSONPatch{op: "test", path: "/spec/example", value: "Red"},
	//	    JSONPatch{op: "replace", path: "/spec/example", value: "Green"}
	//	  ]
	//
	// To define an object for the patch value, use Object types. For example:
	//
	//	  [
	//	    JSONPatch{
	//	      op: "add",
	//	      path: "/spec/selector",
	//	      value: Object.spec.selector{matchLabels: {"environment": "test"}}
	//	    }
	//	  ]
	//
	// To use strings containing '/' and '~' as JSONPatch path keys, use "jsonpatch.escapeKey". For example:
	//
	//	  [
	//	    JSONPatch{
	//	      op: "add",
	//	      path: "/metadata/labels/" + jsonpatch.escapeKey("example.com/environment"),
	//	      value: "test"
	//	    },
	//	  ]
	//
	// CEL expressions have access to the types needed to create JSON patches and objects:
	//
	// - 'JSONPatch' - CEL type of JSON Patch operations. JSONPatch has the fields 'op', 'from', 'path' and 'value'.
	//   See [JSON patch](https://jsonpatch.com/) for more details. The 'value' field may be set to any of: string,
	//   integer, array, map or object.  If set, the 'path' and 'from' fields must be set to a
	//   [JSON pointer](https://datatracker.ietf.org/doc/html/rfc6901/) string, where the 'jsonpatch.escapeKey()' CEL
	//   function may be used to escape path keys containing '/' and '~'.
	// - 'Object' - CEL type of the resource object.
	// - 'Object.<fieldName>' - CEL type of object field (such as 'Object.spec')
	// - 'Object.<fieldName1>.<fieldName2>...<fieldNameN>` - CEL type of nested field (such as 'Object.spec.containers')
	//
	// CEL expressions have access to the contents of the API request, organized into CEL variables as well as some other useful variables:
	//
	// - 'object' - The object from the incoming request. The value is null for DELETE requests.
	// - 'oldObject' - The existing object. The value is null for CREATE requests.
	// - 'request' - Attributes of the API request([ref](/pkg/apis/admission/types.go#AdmissionRequest)).
	// - 'params' - Parameter resource referred to by the policy binding being evaluated. Only populated if the policy has a ParamKind.
	// - 'namespaceObject' - The namespace object that the incoming object belongs to. The value is null for cluster-scoped resources.
	// - 'variables' - Map of composited variables, from its name to its lazily evaluated value.
	//   For example, a variable named 'foo' can be accessed as 'variables.foo'.
	// - 'authorizer' - A CEL Authorizer. May be used to perform authorization checks for the principal (user or service account) of the request.
	//   See https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Authz
	// - 'authorizer.requestResource' - A CEL ResourceCheck constructed from the 'authorizer' and configured with the
	//   request resource.
	//
	// CEL expressions have access to [Kubernetes CEL function libraries](https://kubernetes.io/docs/reference/using-api/cel/#cel-options-language-features-and-libraries)
	// as well as:
	//
	// - 'jsonpatch.escapeKey' - Performs JSONPatch key escaping. '~' and  '/' are escaped as '~0' and `~1' respectively).
	//
	//
	// Only property names of the form `[a-zA-Z_.-/][a-zA-Z0-9_.-/]*` are accessible.
	// Required.
	Expression string
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.32

// MutatingAdmissionPolicyBinding binds the MutatingAdmissionPolicy with parametrized resources.
// MutatingAdmissionPolicyBinding and the optional parameter resource together define how cluster administrators
// configure policies for clusters.
//
// For a given admission request, each binding will cause its policy to be
// evaluated N times, where N is 1 for policies/bindings that don't use
// params, otherwise N is the number of parameters selected by the binding.
// Each evaluation is constrained by a [runtime cost budget](https://kubernetes.io/docs/reference/using-api/cel/#runtime-cost-budget).
//
// Adding/removing policies, bindings, or params can not affect whether a
// given (policy, binding, param) combination is within its own CEL budget.
type MutatingAdmissionPolicyBinding struct {
	metav1.TypeMeta
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta
	// Specification of the desired behavior of the MutatingAdmissionPolicyBinding.
	Spec MutatingAdmissionPolicyBindingSpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.32

// MutatingAdmissionPolicyBindingList is a list of MutatingAdmissionPolicyBinding.
type MutatingAdmissionPolicyBindingList struct {
	metav1.TypeMeta
	// Standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// +optional
	metav1.ListMeta
	// List of PolicyBinding.
	Items []MutatingAdmissionPolicyBinding
}

// MutatingAdmissionPolicyBindingSpec is the specification of the MutatingAdmissionPolicyBinding.
type MutatingAdmissionPolicyBindingSpec struct {
	// policyName references a MutatingAdmissionPolicy name which the MutatingAdmissionPolicyBinding binds to.
	// If the referenced resource does not exist, this binding is considered invalid and will be ignored
	// Required.
	PolicyName string
	// paramRef specifies the parameter resource used to configure the admission control policy.
	// It should point to a resource of the type specified in spec.ParamKind of the bound MutatingAdmissionPolicy.
	// If the policy specifies a ParamKind and the resource referred to by ParamRef does not exist, this binding is considered mis-configured and the FailurePolicy of the MutatingAdmissionPolicy applied.
	// If the policy does not specify a ParamKind then this field is ignored, and the rules are evaluated without a param.
	// +optional
	ParamRef *ParamRef

	// matchResources limits what resources match this binding and may be mutated by it.
	// Note that if matchResources matches a resource, the resource must also match a policy's matchConstraints and
	// matchConditions before the resource may be mutated.
	// When matchResources is unset, it does not constrain resource matching, and only the policy's matchConstraints
	// and matchConditions must match for the resource to be mutated.
	// Additionally, matchResources.resourceRules are optional and do not constraint matching when unset.
	// Note that this is differs from MutatingAdmissionPolicy matchConstraints, where resourceRules are required.
	// The CREATE, UPDATE and CONNECT operations are allowed.  The DELETE operation may not be matched.
	// '*' matches CREATE, UPDATE and CONNECT.
	// +optional
	MatchResources *MatchResources
}
