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

package flowcontrol

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These are valid wildcards.
const (
	APIGroupAll    = "*"
	ResourceAll    = "*"
	VerbAll        = "*"
	NonResourceAll = "*"
	NameAll        = "*"

	NamespaceEvery = "*" // matches every particular namespace
)

// System preset priority level names
const (
	PriorityLevelConfigurationNameExempt   = "exempt"
	PriorityLevelConfigurationNameCatchAll = "catch-all"
	FlowSchemaNameExempt                   = "exempt"
	FlowSchemaNameCatchAll                 = "catch-all"
)

// Conditions
const (
	FlowSchemaConditionDangling = "Dangling"

	PriorityLevelConfigurationConditionConcurrencyShared = "ConcurrencyShared"
)

// Constants used by api validation.
const (
	FlowSchemaMaxMatchingPrecedence int32 = 10000
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FlowSchema defines the schema of a group of flows. Note that a flow is made up of a set of inbound API requests with
// similar attributes and is identified by a pair of strings: the name of the FlowSchema and a "flow distinguisher".
type FlowSchema struct {
	metav1.TypeMeta
	// `metadata` is the standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta
	// `spec` is the specification of the desired behavior of a FlowSchema.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	// +optional
	Spec FlowSchemaSpec
	// `status` is the current status of a FlowSchema.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	// +optional
	Status FlowSchemaStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FlowSchemaList is a list of FlowSchema objects.
type FlowSchemaList struct {
	metav1.TypeMeta
	// `metadata` is the standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta

	// `items` is a list of FlowSchemas.
	Items []FlowSchema
}

// FlowSchemaSpec describes how the FlowSchema's specification looks like.
type FlowSchemaSpec struct {
	// `priorityLevelConfiguration` should reference a PriorityLevelConfiguration in the cluster. If the reference cannot
	// be resolved, the FlowSchema will be ignored and marked as invalid in its status.
	// Required.
	PriorityLevelConfiguration PriorityLevelConfigurationReference
	// `matchingPrecedence` is used to choose among the FlowSchemas that match a given request. The chosen
	// FlowSchema is among those with the numerically lowest (which we take to be logically highest)
	// MatchingPrecedence.  Each MatchingPrecedence value must be ranged in [1,10000].
	// Note that if the precedence is not specified, it will be set to 1000 as default.
	// +optional
	MatchingPrecedence int32
	// `distinguisherMethod` defines how to compute the flow distinguisher for requests that match this schema.
	// `nil` specifies that the distinguisher is disabled and thus will always be the empty string.
	// +optional
	DistinguisherMethod *FlowDistinguisherMethod
	// `rules` describes which requests will match this flow schema. This FlowSchema matches a request if and only if
	// at least one member of rules matches the request.
	// if it is an empty slice, there will be no requests matching the FlowSchema.
	// +listType=set
	// +optional
	Rules []PolicyRulesWithSubjects
}

// FlowDistinguisherMethodType is the type of flow distinguisher method
type FlowDistinguisherMethodType string

// These are valid flow-distinguisher methods.
const (
	// FlowDistinguisherMethodByUserType specifies that the flow distinguisher is the username in the request.
	// This type is used to provide some insulation between users.
	FlowDistinguisherMethodByUserType FlowDistinguisherMethodType = "ByUser"

	// FlowDistinguisherMethodByNamespaceType specifies that the flow distinguisher is the namespace of the
	// object that the request acts upon. If the object is not namespaced, or if the request is a non-resource
	// request, then the distinguisher will be the empty string. An example usage of this type is to provide
	// some insulation between tenants in a situation where there are multiple tenants and each namespace
	// is dedicated to a tenant.
	FlowDistinguisherMethodByNamespaceType FlowDistinguisherMethodType = "ByNamespace"
)

// FlowDistinguisherMethod specifies the method of a flow distinguisher.
type FlowDistinguisherMethod struct {
	// `type` is the type of flow distinguisher method
	// The supported types are "ByUser" and "ByNamespace".
	// Required.
	Type FlowDistinguisherMethodType
}

// PriorityLevelConfigurationReference contains information that points to the "request-priority" being used.
type PriorityLevelConfigurationReference struct {
	// `name` is the name of the priority level configuration being referenced
	// Required.
	Name string
}

// PolicyRulesWithSubjects prescribes a test that applies to a request to an apiserver. The test considers the subject
// making the request, the verb being requested, and the resource to be acted upon. This PolicyRulesWithSubjects matches
// a request if and only if both (a) at least one member of subjects matches the request and (b) at least one member
// of resourceRules or nonResourceRules matches the request.
type PolicyRulesWithSubjects struct {
	// subjects is the list of normal user, serviceaccount, or group that this rule cares about.
	// There must be at least one member in this slice.
	// A slice that includes both the system:authenticated and system:unauthenticated user groups matches every request.
	// +listType=set
	// Required.
	Subjects []Subject
	// `resourceRules` is a slice of ResourcePolicyRules that identify matching requests according to their verb and the
	// target resource.
	// At least one of `resourceRules` and `nonResourceRules` has to be non-empty.
	// +listType=set
	// +optional
	ResourceRules []ResourcePolicyRule
	// `nonResourceRules` is a list of NonResourcePolicyRules that identify matching requests according to their verb
	// and the target non-resource URL.
	// +listType=set
	// +optional
	NonResourceRules []NonResourcePolicyRule
}

// Subject matches the originator of a request, as identified by the request authentication system. There are three
// ways of matching an originator; by user, group, or service account.
// +union
type Subject struct {
	// `kind` indicates which one of the other fields is non-empty.
	// Required
	// +unionDiscriminator
	Kind SubjectKind
	// `user` matches based on username.
	// +optional
	User *UserSubject
	// `group` matches based on user group name.
	// +optional
	Group *GroupSubject
	// `serviceAccount` matches ServiceAccounts.
	// +optional
	ServiceAccount *ServiceAccountSubject
}

// SubjectKind is the kind of subject.
type SubjectKind string

// Supported subject's kinds.
const (
	SubjectKindUser           SubjectKind = "User"
	SubjectKindGroup          SubjectKind = "Group"
	SubjectKindServiceAccount SubjectKind = "ServiceAccount"
)

// UserSubject holds detailed information for user-kind subject.
type UserSubject struct {
	// `name` is the username that matches, or "*" to match all usernames.
	// Required.
	Name string
}

// GroupSubject holds detailed information for group-kind subject.
type GroupSubject struct {
	// name is the user group that matches, or "*" to match all user groups.
	// See https://github.com/kubernetes/apiserver/blob/master/pkg/authentication/user/user.go for some
	// well-known group names.
	// Required.
	Name string
}

// ServiceAccountSubject holds detailed information for service-account-kind subject.
type ServiceAccountSubject struct {
	// `namespace` is the namespace of matching ServiceAccount objects.
	// Required.
	Namespace string
	// `name` is the name of matching ServiceAccount objects, or "*" to match regardless of name.
	// Required.
	Name string
}

// ResourcePolicyRule is a predicate that matches some resource
// requests, testing the request's verb and the target resource. A
// ResourcePolicyRule matches a resource request if and only if: (a)
// at least one member of verbs matches the request, (b) at least one
// member of apiGroups matches the request, (c) at least one member of
// resources matches the request, and (d) least one member of
// namespaces matches the request.
type ResourcePolicyRule struct {
	// `verbs` is a list of matching verbs and may not be empty.
	// "*" matches all verbs and, if present, must be the only entry.
	// +listType=set
	// Required.
	Verbs []string

	// `apiGroups` is a list of matching API groups and may not be empty.
	// "*" matches all API groups and, if present, must be the only entry.
	// +listType=set
	// Required.
	APIGroups []string

	// `resources` is a list of matching resources (i.e., lowercase
	// and plural) with, if desired, subresource.  For example, [
	// "services", "nodes/status" ].  This list may not be empty.
	// "*" matches all resources and, if present, must be the only entry.
	// Required.
	// +listType=set
	Resources []string

	// `clusterScope` indicates whether to match requests that do not
	// specify a namespace (which happens either because the resource
	// is not namespaced or the request targets all namespaces).
	// If this field is omitted or false then the `namespaces` field
	// must contain a non-empty list.
	// +optional
	ClusterScope bool

	// `namespaces` is a list of target namespaces that restricts
	// matches.  A request that specifies a target namespace matches
	// only if either (a) this list contains that target namespace or
	// (b) this list contains "*".  Note that "*" matches any
	// specified namespace but does not match a request that _does
	// not specify_ a namespace (see the `clusterScope` field for
	// that).
	// This list may be empty, but only if `clusterScope` is true.
	// +optional
	// +listType=set
	Namespaces []string
}

// NonResourcePolicyRule is a predicate that matches non-resource requests according to their verb and the
// target non-resource URL. A NonResourcePolicyRule matches a request if and only if both (a) at least one member
// of verbs matches the request and (b) at least one member of nonResourceURLs matches the request.
type NonResourcePolicyRule struct {
	// `verbs` is a list of matching verbs and may not be empty.
	// "*" matches all verbs. If it is present, it must be the only entry.
	// +listType=set
	// Required.
	Verbs []string
	// `nonResourceURLs` is a set of url prefixes that a user should have access to and may not be empty.
	// For example:
	//   - "/healthz" is legal
	//   - "/hea*" is illegal
	//   - "/hea" is legal but matches nothing
	//   - "/hea/*" also matches nothing
	//   - "/healthz/*" matches all per-component health checks.
	// "*" matches all non-resource urls. if it is present, it must be the only entry.
	// +listType=set
	// Required.
	NonResourceURLs []string
}

// FlowSchemaStatus represents the current state of a FlowSchema.
type FlowSchemaStatus struct {
	// `conditions` is a list of the current states of FlowSchema.
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []FlowSchemaCondition
}

// FlowSchemaCondition describes conditions for a FlowSchema.
type FlowSchemaCondition struct {
	// `type` is the type of the condition.
	// Required.
	Type FlowSchemaConditionType
	// `status` is the status of the condition.
	// Can be True, False, Unknown.
	// Required.
	Status ConditionStatus
	// `lastTransitionTime` is the last time the condition transitioned from one status to another.
	LastTransitionTime metav1.Time
	// `reason` is a unique, one-word, CamelCase reason for the condition's last transition.
	Reason string
	// `message` is a human-readable message indicating details about last transition.
	Message string
}

// FlowSchemaConditionType is a valid value for FlowSchemaStatusCondition.Type
type FlowSchemaConditionType string

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PriorityLevelConfiguration represents the configuration of a priority level.
type PriorityLevelConfiguration struct {
	metav1.TypeMeta
	// `metadata` is the standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta
	// `spec` is the specification of the desired behavior of a "request-priority".
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	// +optional
	Spec PriorityLevelConfigurationSpec
	// `status` is the current status of a "request-priority".
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	// +optional
	Status PriorityLevelConfigurationStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PriorityLevelConfigurationList is a list of PriorityLevelConfiguration objects.
type PriorityLevelConfigurationList struct {
	metav1.TypeMeta
	// `metadata` is the standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta
	// `items` is a list of request-priorities.
	Items []PriorityLevelConfiguration
}

// PriorityLevelConfigurationSpec specifies the configuration of a priority level.
// +union
type PriorityLevelConfigurationSpec struct {
	// `type` indicates whether this priority level is subject to
	// limitation on request execution.  A value of `"Exempt"` means
	// that requests of this priority level are not subject to a limit
	// (and thus are never queued) and do not detract from the
	// capacity made available to other priority levels.  A value of
	// `"Limited"` means that (a) requests of this priority level
	// _are_ subject to limits and (b) some of the server's limited
	// capacity is made available exclusively to this priority level.
	// Required.
	// +unionDiscriminator
	Type PriorityLevelEnablement

	// `limited` specifies how requests are handled for a Limited priority level.
	// This field must be non-empty if and only if `type` is `"Limited"`.
	// +optional
	Limited *LimitedPriorityLevelConfiguration

	// `exempt` specifies how requests are handled for an exempt priority level.
	// This field MUST be empty if `type` is `"Limited"`.
	// This field MAY be non-empty if `type` is `"Exempt"`.
	// If empty and `type` is `"Exempt"` then the default values
	// for `ExemptPriorityLevelConfiguration` apply.
	// +optional
	Exempt *ExemptPriorityLevelConfiguration
}

// PriorityLevelEnablement indicates whether limits on execution are enabled for the priority level
type PriorityLevelEnablement string

// Supported priority level enablement values.
const (
	// PriorityLevelEnablementExempt means that requests are not subject to limits
	PriorityLevelEnablementExempt PriorityLevelEnablement = "Exempt"

	// PriorityLevelEnablementLimited means that requests are subject to limits
	PriorityLevelEnablementLimited PriorityLevelEnablement = "Limited"
)

// LimitedPriorityLevelConfiguration specifies how to handle requests that are subject to limits.
// It addresses two issues:
//   - How are requests for this priority level limited?
//   - What should be done with requests that exceed the limit?
type LimitedPriorityLevelConfiguration struct {
	// `nominalConcurrencyShares` (NCS) contributes to the computation of the
	// NominalConcurrencyLimit (NominalCL) of this level.
	// This is the number of execution seats available at this priority level.
	// This is used both for requests dispatched from this priority level
	// as well as requests dispatched from other priority levels
	// borrowing seats from this level.
	// The server's concurrency limit (ServerCL) is divided among the
	// Limited priority levels in proportion to their NCS values:
	//
	// NominalCL(i)  = ceil( ServerCL * NCS(i) / sum_ncs )
	// sum_ncs = sum[priority level k] NCS(k)
	//
	// Bigger numbers mean a larger nominal concurrency limit,
	// at the expense of every other priority level.
	// This field has a default value of 30.
	//
	// Setting this field to zero supports the construction of a
	// "jail" for this priority level that is used to hold some request(s)
	//
	// +optional
	NominalConcurrencyShares int32

	// `limitResponse` indicates what to do with requests that can not be executed right now
	LimitResponse LimitResponse

	// `lendablePercent` prescribes the fraction of the level's NominalCL that
	// can be borrowed by other priority levels. The value of this
	// field must be between 0 and 100, inclusive, and it defaults to 0.
	// The number of seats that other levels can borrow from this level, known
	// as this level's LendableConcurrencyLimit (LendableCL), is defined as follows.
	//
	// LendableCL(i) = round( NominalCL(i) * lendablePercent(i)/100.0 )
	//
	// +optional
	LendablePercent *int32

	// `borrowingLimitPercent`, if present, configures a limit on how many
	// seats this priority level can borrow from other priority levels.
	// The limit is known as this level's BorrowingConcurrencyLimit
	// (BorrowingCL) and is a limit on the total number of seats that this
	// level may borrow at any one time.
	// This field holds the ratio of that limit to the level's nominal
	// concurrency limit. When this field is non-nil, it must hold a
	// non-negative integer and the limit is calculated as follows.
	//
	// BorrowingCL(i) = round( NominalCL(i) * borrowingLimitPercent(i)/100.0 )
	//
	// The value of this field can be more than 100, implying that this
	// priority level can borrow a number of seats that is greater than
	// its own nominal concurrency limit (NominalCL).
	// When this field is left `nil`, the limit is effectively infinite.
	// +optional
	BorrowingLimitPercent *int32

	// `weight` gives this priority level more or less priority in the borrowing
	// of concurrency.
	// The default configuration objects use values in the range 10--100.
	// Must be in the range 1--1000.
	// The default value is 30.
	// +optional
	Weight *int32
}

// ExemptPriorityLevelConfiguration describes the configurable aspects
// of the handling of exempt requests.
// In the mandatory exempt configuration object the values in the fields
// here can be modified by authorized users, unlike the rest of the `spec`.
type ExemptPriorityLevelConfiguration struct {
	// `nominalConcurrencyShares` (NCS) contributes to the computation of the
	// NominalConcurrencyLimit (NominalCL) of this level.
	// This is the number of execution seats nominally reserved for this priority level.
	// This DOES NOT limit the dispatching from this priority level
	// but affects the other priority levels through the borrowing mechanism.
	// The server's concurrency limit (ServerCL) is divided among all the
	// priority levels in proportion to their NCS values:
	//
	// NominalCL(i)  = ceil( ServerCL * NCS(i) / sum_ncs )
	// sum_ncs = sum[priority level k] NCS(k)
	//
	// Bigger numbers mean a larger nominal concurrency limit,
	// at the expense of every other priority level.
	// This field has a default value of zero.
	// +optional
	NominalConcurrencyShares *int32
	// `lendablePercent` prescribes the fraction of the level's NominalCL that
	// can be borrowed by other priority levels.  This value of this
	// field must be between 0 and 100, inclusive, and it defaults to 0.
	// The number of seats that other levels can borrow from this level, known
	// as this level's LendableConcurrencyLimit (LendableCL), is defined as follows.
	//
	// LendableCL(i) = round( NominalCL(i) * lendablePercent(i)/100.0 )
	//
	// +optional
	LendablePercent *int32
	// The `BorrowingCL` of an Exempt priority level is implicitly `ServerCL`.
	// In other words, an exempt priority level
	// has no meaningful limit on how much it borrows.
	// There is no explicit representation of that here.
}

// LimitResponse defines how to handle requests that can not be executed right now.
// +union
type LimitResponse struct {
	// `type` is "Queue" or "Reject".
	// "Queue" means that requests that can not be executed upon arrival
	// are held in a queue until they can be executed or a queuing limit
	// is reached.
	// "Reject" means that requests that can not be executed upon arrival
	// are rejected.
	// Required.
	// +unionDiscriminator
	Type LimitResponseType

	// `queuing` holds the configuration parameters for queuing.
	// This field may be non-empty only if `type` is `"Queue"`.
	// +optional
	Queuing *QueuingConfiguration
}

// LimitResponseType identifies how a Limited priority level handles a request that can not be executed right now
type LimitResponseType string

// Supported limit responses.
const (
	// LimitResponseTypeQueue means that requests that can not be executed right now are queued until they can be executed or a queuing limit is hit
	LimitResponseTypeQueue LimitResponseType = "Queue"

	// LimitResponseTypeReject means that requests that can not be executed right now are rejected
	LimitResponseTypeReject LimitResponseType = "Reject"
)

// QueuingConfiguration holds the configuration parameters for queuing
type QueuingConfiguration struct {
	// `queues` is the number of queues for this priority level. The
	// queues exist independently at each apiserver. The value must be
	// positive.  Setting it to 1 effectively precludes
	// shufflesharding and thus makes the distinguisher method of
	// associated flow schemas irrelevant.  This field has a default
	// value of 64.
	// +optional
	Queues int32

	// `handSize` is a small positive number that configures the
	// shuffle sharding of requests into queues.  When enqueuing a request
	// at this priority level the request's flow identifier (a string
	// pair) is hashed and the hash value is used to shuffle the list
	// of queues and deal a hand of the size specified here.  The
	// request is put into one of the shortest queues in that hand.
	// `handSize` must be no larger than `queues`, and should be
	// significantly smaller (so that a few heavy flows do not
	// saturate most of the queues).  See the user-facing
	// documentation for more extensive guidance on setting this
	// field.  This field has a default value of 8.
	// +optional
	HandSize int32

	// `queueLengthLimit` is the maximum number of requests allowed to
	// be waiting in a given queue of this priority level at a time;
	// excess requests are rejected.  This value must be positive.  If
	// not specified, it will be defaulted to 50.
	// +optional
	QueueLengthLimit int32
}

// PriorityLevelConfigurationConditionType is a valid value for PriorityLevelConfigurationStatusCondition.Type
type PriorityLevelConfigurationConditionType string

// PriorityLevelConfigurationStatus represents the current state of a "request-priority".
type PriorityLevelConfigurationStatus struct {
	// `conditions` is the current state of "request-priority".
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []PriorityLevelConfigurationCondition
}

// PriorityLevelConfigurationCondition defines the condition of priority level.
type PriorityLevelConfigurationCondition struct {
	// `type` is the type of the condition.
	// Required.
	Type PriorityLevelConfigurationConditionType
	// `status` is the status of the condition.
	// Can be True, False, Unknown.
	// Required.
	Status ConditionStatus
	// `lastTransitionTime` is the last time the condition transitioned from one status to another.
	LastTransitionTime metav1.Time
	// `reason` is a unique, one-word, CamelCase reason for the condition's last transition.
	Reason string
	// `message` is a human-readable message indicating details about last transition.
	Message string
}

// ConditionStatus is the status of the condition.
type ConditionStatus string

// These are valid condition statuses. "ConditionTrue" means a resource is in the condition.
// "ConditionFalse" means a resource is not in the condition. "ConditionUnknown" means kubernetes
// can't decide if a resource is in the condition or not. In the future, we could add other
// intermediate conditions, e.g. ConditionDegraded.
const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)
