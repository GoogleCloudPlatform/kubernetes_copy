/*
Copyright 2018 The Kubernetes Authors.

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

package v1

import (
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Header keys used by the audit system.
const (
	// Header to hold the audit ID as the request is propagated through the serving hierarchy. The
	// Audit-ID header should be set by the first server to receive the request (e.g. the federation
	// server or kube-aggregator).
	HeaderAuditID = "Audit-ID"
)

// Level defines the amount of information logged during auditing
type Level string

// Valid audit levels
const (
	// LevelNone disables auditing
	LevelNone Level = "None"
	// LevelMetadata provides the basic level of auditing.
	LevelMetadata Level = "Metadata"
	// LevelRequest provides Metadata level of auditing, and additionally
	// logs the request object (does not apply for non-resource requests).
	LevelRequest Level = "Request"
	// LevelRequestResponse provides Request level of auditing, and additionally
	// logs the response object (does not apply for non-resource requests).
	LevelRequestResponse Level = "RequestResponse"
)

// Stage defines the stages in request handling that audit events may be generated.
type Stage string

// Valid audit stages.
const (
	// The stage for events generated as soon as the audit handler receives the request, and before it
	// is delegated down the handler chain.
	StageRequestReceived Stage = "RequestReceived"
	// The stage for events generated once the response headers are sent, but before the response body
	// is sent. This stage is only generated for long-running requests (e.g. watch).
	StageResponseStarted Stage = "ResponseStarted"
	// The stage for events generated once the response body has been completed, and no more bytes
	// will be sent.
	StageResponseComplete Stage = "ResponseComplete"
	// The stage for events generated when a panic occurred.
	StagePanic Stage = "Panic"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Event captures all the information that can be included in an API audit log.
type Event struct {
	metav1.TypeMeta `json:",inline"`

	// AuditLevel at which event was generated
	Level Level `json:"level" protobuf:"bytes,1,opt,name=level,casttype=Level"`

	// Unique audit ID, generated for each request.
	AuditID types.UID `json:"auditID" protobuf:"bytes,2,opt,name=auditID,casttype=k8s.io/apimachinery/pkg/types.UID"`
	// Stage of the request handling when this event instance was generated.
	Stage Stage `json:"stage" protobuf:"bytes,3,opt,name=stage,casttype=Stage"`

	// RequestURI is the request URI as sent by the client to a server.
	RequestURI string `json:"requestURI" protobuf:"bytes,4,opt,name=requestURI"`
	// Verb is the kubernetes verb associated with the request.
	// For non-resource requests, this is the lower-cased HTTP method.
	Verb string `json:"verb" protobuf:"bytes,5,opt,name=verb"`
	// Authenticated user information.
	User authnv1.UserInfo `json:"user" protobuf:"bytes,6,opt,name=user"`
	// Impersonated user information.
	// +optional
	ImpersonatedUser *authnv1.UserInfo `json:"impersonatedUser,omitempty" protobuf:"bytes,7,opt,name=impersonatedUser"`
	// Source IPs, from where the request originated and intermediate proxies.
	// The source IPs are listed from (in order):
	// 1. X-Forwarded-For request header IPs
	// 2. X-Real-Ip header, if not present in the X-Forwarded-For list
	// 3. The remote address for the connection, if it doesn't match the last
	//    IP in the list up to here (X-Forwarded-For or X-Real-Ip).
	// Note: All but the last IP can be arbitrarily set by the client.
	// +optional
	// +listType=atomic
	SourceIPs []string `json:"sourceIPs,omitempty" protobuf:"bytes,8,rep,name=sourceIPs"`
	// UserAgent records the user agent string reported by the client.
	// Note that the UserAgent is provided by the client, and must not be trusted.
	// +optional
	UserAgent string `json:"userAgent,omitempty" protobuf:"bytes,16,opt,name=userAgent"`
	// Object reference this request is targeted at.
	// Does not apply for List-type requests, or non-resource requests.
	// +optional
	ObjectRef *ObjectReference `json:"objectRef,omitempty" protobuf:"bytes,9,opt,name=objectRef"`
	// The response status, populated even when the ResponseObject is not a Status type.
	// For successful responses, this will only include the Code and StatusSuccess.
	// For non-status type error responses, this will be auto-populated with the error Message.
	// +optional
	ResponseStatus *metav1.Status `json:"responseStatus,omitempty" protobuf:"bytes,10,opt,name=responseStatus"`

	// API object from the request, in JSON format. The RequestObject is recorded as-is in the request
	// (possibly re-encoded as JSON), prior to version conversion, defaulting, admission or
	// merging. It is an external versioned object type, and may not be a valid object on its own.
	// Omitted for non-resource requests.  Only logged at Request Level and higher.
	// +optional
	RequestObject *runtime.Unknown `json:"requestObject,omitempty" protobuf:"bytes,11,opt,name=requestObject"`
	// API object returned in the response, in JSON. The ResponseObject is recorded after conversion
	// to the external type, and serialized as JSON.  Omitted for non-resource requests.  Only logged
	// at Response Level.
	// +optional
	ResponseObject *runtime.Unknown `json:"responseObject,omitempty" protobuf:"bytes,12,opt,name=responseObject"`
	// Time the request reached the apiserver.
	// +optional
	RequestReceivedTimestamp metav1.MicroTime `json:"requestReceivedTimestamp" protobuf:"bytes,13,opt,name=requestReceivedTimestamp"`
	// Time the request reached current audit stage.
	// +optional
	StageTimestamp metav1.MicroTime `json:"stageTimestamp" protobuf:"bytes,14,opt,name=stageTimestamp"`

	// Annotations is an unstructured key value map stored with an audit event that may be set by
	// plugins invoked in the request serving chain, including authentication, authorization and
	// admission plugins. Note that these annotations are for the audit event, and do not correspond
	// to the metadata.annotations of the submitted object. Keys should uniquely identify the informing
	// component to avoid name collisions (e.g. podsecuritypolicy.admission.k8s.io/policy). Values
	// should be short. Annotations are included in the Metadata level.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,15,rep,name=annotations"`

	// Time indicates the expiration date of peerCertificates.
	// +optional
	TLSExpiration metav1.MicroTime `json:"tlsExpiration" protobuf:"bytes,17,opt,name=tlsExpiration"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EventList is a list of audit Events.
type EventList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Event `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Policy defines the configuration of audit logging, and the rules for how different request
// categories are logged.
type Policy struct {
	metav1.TypeMeta `json:",inline"`
	// ObjectMeta is included for interoperability with API infrastructure.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Rules specify the audit Level a request should be recorded at.
	// A request may match multiple rules, in which case the FIRST matching rule is used.
	// The default audit level is None, but can be overridden by a catch-all rule at the end of the list.
	// PolicyRules are strictly ordered.
	// +listType=atomic
	Rules []PolicyRule `json:"rules" protobuf:"bytes,2,rep,name=rules"`

	// OmitStages is a list of stages for which no events are created. Note that this can also
	// be specified per rule in which case the union of both are omitted.
	// +optional
	// +listType=atomic
	OmitStages []Stage `json:"omitStages,omitempty" protobuf:"bytes,3,rep,name=omitStages"`

	// OmitManagedFields indicates whether to omit the managed fields of the request
	// and response bodies from being written to the API audit log.
	// This is used as a global default - a value of 'true' will omit the managed fileds,
	// otherwise the managed fields will be included in the API audit log.
	// Note that this can also be specified per rule in which case the value specified
	// in a rule will override the global default.
	// +optional
	OmitManagedFields bool `json:"omitManagedFields,omitempty" protobuf:"varint,4,opt,name=omitManagedFields"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PolicyList is a list of audit Policies.
type PolicyList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Policy `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// PolicyRule maps requests based off metadata to an audit Level.
// Requests must match the rules of every field (an intersection of rules).
type PolicyRule struct {
	// The Level that requests matching this rule are recorded at.
	Level Level `json:"level" protobuf:"bytes,1,opt,name=level,casttype=Level"`

	// The users (by authenticated user name) this rule applies to.
	// An empty list implies every user.
	// +optional
	// +listType=atomic
	Users []string `json:"users,omitempty" protobuf:"bytes,2,rep,name=users"`
	// The user groups this rule applies to. A user is considered matching
	// if it is a member of any of the UserGroups.
	// An empty list implies every user group.
	// +optional
	// +listType=atomic
	UserGroups []string `json:"userGroups,omitempty" protobuf:"bytes,3,rep,name=userGroups"`

	// The verbs that match this rule.
	// An empty list implies every verb.
	// +optional
	// +listType=atomic
	Verbs []string `json:"verbs,omitempty" protobuf:"bytes,4,rep,name=verbs"`

	// Rules can apply to API resources (such as "pods" or "secrets"),
	// non-resource URL paths (such as "/api"), or neither, but not both.
	// If neither is specified, the rule is treated as a default for all URLs.

	// Resources that this rule matches. An empty list implies all kinds in all API groups.
	// +optional
	// +listType=atomic
	Resources []GroupResources `json:"resources,omitempty" protobuf:"bytes,5,rep,name=resources"`
	// Namespaces that this rule matches.
	// The empty string "" matches non-namespaced resources.
	// An empty list implies every namespace.
	// +optional
	// +listType=atomic
	Namespaces []string `json:"namespaces,omitempty" protobuf:"bytes,6,rep,name=namespaces"`

	// NonResourceURLs is a set of URL paths that should be audited.
	// `*`s are allowed, but only as the full, final step in the path.
	// Examples:
	// - `/metrics` - Log requests for apiserver metrics
	// - `/healthz*` - Log all health checks
	// +optional
	// +listType=atomic
	NonResourceURLs []string `json:"nonResourceURLs,omitempty" protobuf:"bytes,7,rep,name=nonResourceURLs"`

	// OmitStages is a list of stages for which no events are created. Note that this can also
	// be specified policy wide in which case the union of both are omitted.
	// An empty list means no restrictions will apply.
	// +optional
	// +listType=atomic
	OmitStages []Stage `json:"omitStages,omitempty" protobuf:"bytes,8,rep,name=omitStages"`

	// OmitManagedFields indicates whether to omit the managed fields of the request
	// and response bodies from being written to the API audit log.
	// - a value of 'true' will drop the managed fields from the API audit log
	// - a value of 'false' indicates that the managed fileds should be included
	//   in the API audit log
	// Note that the value, if specified, in this rule will override the global default
	// If a value is not specified then the global default specified in
	// Policy.OmitManagedFields will stand.
	// +optional
	OmitManagedFields *bool `json:"omitManagedFields,omitempty" protobuf:"varint,9,opt,name=omitManagedFields"`
}

// GroupResources represents resource kinds in an API group.
type GroupResources struct {
	// Group is the name of the API group that contains the resources.
	// The empty string represents the core API group.
	// +optional
	Group string `json:"group,omitempty" protobuf:"bytes,1,opt,name=group"`
	// Resources is a list of resources this rule applies to.
	//
	// For example:
	// - `pods` matches pods.
	// - `pods/log` matches the log subresource of pods.
	// - `*` matches all resources and their subresources.
	// - `pods/*` matches all subresources of pods.
	// - `*/scale` matches all scale subresources.
	//
	// If wildcard is present, the validation rule will ensure resources do not
	// overlap with each other.
	//
	// An empty list implies all resources and subresources in this API groups apply.
	// +optional
	// +listType=atomic
	Resources []string `json:"resources,omitempty" protobuf:"bytes,2,rep,name=resources"`
	// ResourceNames is a list of resource instance names that the policy matches.
	// Using this field requires Resources to be specified.
	// An empty list implies that every instance of the resource is matched.
	// +optional
	// +listType=atomic
	ResourceNames []string `json:"resourceNames,omitempty" protobuf:"bytes,3,rep,name=resourceNames"`
}

// ObjectReference contains enough information to let you inspect or modify the referred object.
type ObjectReference struct {
	// +optional
	Resource string `json:"resource,omitempty" protobuf:"bytes,1,opt,name=resource"`
	// +optional
	Namespace string `json:"namespace,omitempty" protobuf:"bytes,2,opt,name=namespace"`
	// +optional
	Name string `json:"name,omitempty" protobuf:"bytes,3,opt,name=name"`
	// +optional
	UID types.UID `json:"uid,omitempty" protobuf:"bytes,4,opt,name=uid,casttype=k8s.io/apimachinery/pkg/types.UID"`
	// APIGroup is the name of the API group that contains the referred object.
	// The empty string represents the core API group.
	// +optional
	APIGroup string `json:"apiGroup,omitempty" protobuf:"bytes,5,opt,name=apiGroup"`
	// APIVersion is the version of the API group that contains the referred object.
	// +optional
	APIVersion string `json:"apiVersion,omitempty" protobuf:"bytes,6,opt,name=apiVersion"`
	// +optional
	ResourceVersion string `json:"resourceVersion,omitempty" protobuf:"bytes,7,opt,name=resourceVersion"`
	// +optional
	Subresource string `json:"subresource,omitempty" protobuf:"bytes,8,opt,name=subresource"`
}
