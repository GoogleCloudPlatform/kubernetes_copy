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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// ConversionStrategyType describes different conversion types.
type ConversionStrategyType string

const (
	// KubeAPIApprovedAnnotation is an annotation that must be set to create a CRD for the k8s.io, *.k8s.io, kubernetes.io, or *.kubernetes.io namespaces.
	// The value should be a link to a URL where the current spec was approved, so updates to the spec should also update the URL.
	// If the API is unapproved, you may set the annotation to a string starting with `"unapproved"`.  For instance, `"unapproved, temporarily squatting"` or `"unapproved, experimental-only"`.  This is discouraged.
	KubeAPIApprovedAnnotation = "api-approved.kubernetes.io"

	// NoneConverter is a converter that only sets apiversion of the CR and leave everything else unchanged.
	NoneConverter ConversionStrategyType = "None"
	// WebhookConverter is a converter that calls to an external webhook to convert the CR.
	WebhookConverter ConversionStrategyType = "Webhook"
)

// CustomResourceDefinitionSpec describes how a user wants their resource to appear
type CustomResourceDefinitionSpec struct {
	// group is the API group of the defined custom resource.
	// The custom resources are served under `/apis/<group>/...`.
	// Must match the name of the CustomResourceDefinition (in the form `<names.plural>.<group>`).
	Group string `json:"group" protobuf:"bytes,1,opt,name=group"`
	// version is the API version of the defined custom resource.
	// The custom resources are served under `/apis/<group>/<version>/...`.
	// Must match the name of the first item in the `versions` list if `version` and `versions` are both specified.
	// Optional if `versions` is specified.
	// Deprecated: use `versions` instead.
	// +optional
	Version string `json:"version,omitempty" protobuf:"bytes,2,opt,name=version"`
	// names specify the resource and kind names for the custom resource.
	Names CustomResourceDefinitionNames `json:"names" protobuf:"bytes,3,opt,name=names"`
	// scope indicates whether the defined custom resource is cluster- or namespace-scoped.
	// Allowed values are `Cluster` and `Namespaced`. Default is `Namespaced`.
	Scope ResourceScope `json:"scope" protobuf:"bytes,4,opt,name=scope,casttype=ResourceScope"`
	// validation describes the schema used for validation and pruning of the custom resource.
	// If present, this validation schema is used to validate all versions.
	// Top-level and per-version schemas are mutually exclusive.
	// +optional
	Validation *CustomResourceValidation `json:"validation,omitempty" protobuf:"bytes,5,opt,name=validation"`
	// subresources specify what subresources the defined custom resource has.
	// If present, this field configures subresources for all versions.
	// Top-level and per-version subresources are mutually exclusive.
	// +optional
	Subresources *CustomResourceSubresources `json:"subresources,omitempty" protobuf:"bytes,6,opt,name=subresources"`
	// versions is the list of all API versions of the defined custom resource.
	// Optional if `version` is specified.
	// The name of the first item in the `versions` list must match the `version` field if `version` and `versions` are both specified.
	// Version names are used to compute the order in which served versions are listed in API discovery.
	// If the version string is "kube-like", it will sort above non "kube-like" version strings, which are ordered
	// lexicographically. "Kube-like" versions start with a "v", then are followed by a number (the major version),
	// then optionally the string "alpha" or "beta" and another number (the minor version). These are sorted first
	// by GA > beta > alpha (where GA is a version with no suffix such as beta or alpha), and then by comparing
	// major version, then minor version. An example sorted list of versions:
	// v10, v2, v1, v11beta2, v10beta3, v3beta1, v12alpha1, v11alpha2, foo1, foo10.
	// +optional
	// +listType=atomic
	Versions []CustomResourceDefinitionVersion `json:"versions,omitempty" protobuf:"bytes,7,rep,name=versions"`
	// additionalPrinterColumns specifies additional columns returned in Table output.
	// See https://kubernetes.io/docs/reference/using-api/api-concepts/#receiving-resources-as-tables for details.
	// If present, this field configures columns for all versions.
	// Top-level and per-version columns are mutually exclusive.
	// If no top-level or per-version columns are specified, a single column displaying the age of the custom resource is used.
	// +optional
	// +listType=atomic
	AdditionalPrinterColumns []CustomResourceColumnDefinition `json:"additionalPrinterColumns,omitempty" protobuf:"bytes,8,rep,name=additionalPrinterColumns"`

	// selectableFields specifies paths to fields that may be used as field selectors.
	// See https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors
	//
	// +featureGate=CustomResourceFieldSelectors
	// +optional
	// +listType=atomic
	SelectableFields []SelectableField `json:"selectableFields,omitempty" protobuf:"bytes,11,rep,name=selectableFields"`

	// conversion defines conversion settings for the CRD.
	// +optional
	Conversion *CustomResourceConversion `json:"conversion,omitempty" protobuf:"bytes,9,opt,name=conversion"`

	// preserveUnknownFields indicates that object fields which are not specified
	// in the OpenAPI schema should be preserved when persisting to storage.
	// apiVersion, kind, metadata and known fields inside metadata are always preserved.
	// If false, schemas must be defined for all versions.
	// Defaults to true in v1beta for backwards compatibility.
	// Deprecated: will be required to be false in v1. Preservation of unknown fields can be specified
	// in the validation schema using the `x-kubernetes-preserve-unknown-fields: true` extension.
	// See https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#field-pruning for details.
	// +optional
	PreserveUnknownFields *bool `json:"preserveUnknownFields,omitempty" protobuf:"varint,10,opt,name=preserveUnknownFields"`
}

// CustomResourceConversion describes how to convert different versions of a CR.
type CustomResourceConversion struct {
	// strategy specifies how custom resources are converted between versions. Allowed values are:
	// - `None`: The converter only change the apiVersion and would not touch any other field in the custom resource.
	// - `Webhook`: API Server will call to an external webhook to do the conversion. Additional information
	//   is needed for this option. This requires spec.preserveUnknownFields to be false, and spec.conversion.webhookClientConfig to be set.
	Strategy ConversionStrategyType `json:"strategy" protobuf:"bytes,1,name=strategy"`

	// webhookClientConfig is the instructions for how to call the webhook if strategy is `Webhook`.
	// Required when `strategy` is set to `Webhook`.
	// +optional
	WebhookClientConfig *WebhookClientConfig `json:"webhookClientConfig,omitempty" protobuf:"bytes,2,name=webhookClientConfig"`

	// conversionReviewVersions is an ordered list of preferred `ConversionReview`
	// versions the Webhook expects. The API server will use the first version in
	// the list which it supports. If none of the versions specified in this list
	// are supported by API server, conversion will fail for the custom resource.
	// If a persisted Webhook configuration specifies allowed versions and does not
	// include any versions known to the API Server, calls to the webhook will fail.
	// Defaults to `["v1beta1"]`.
	// +optional
	// +listType=atomic
	ConversionReviewVersions []string `json:"conversionReviewVersions,omitempty" protobuf:"bytes,3,rep,name=conversionReviewVersions"`
}

// WebhookClientConfig contains the information to make a TLS connection with the webhook.
type WebhookClientConfig struct {
	// url gives the location of the webhook, in standard URL form
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
	URL *string `json:"url,omitempty" protobuf:"bytes,3,opt,name=url"`

	// service is a reference to the service for this webhook. Either
	// service or url must be specified.
	//
	// If the webhook is running within the cluster, then you should use `service`.
	//
	// +optional
	Service *ServiceReference `json:"service,omitempty" protobuf:"bytes,1,opt,name=service"`

	// caBundle is a PEM encoded CA bundle which will be used to validate the webhook's server certificate.
	// If unspecified, system trust roots on the apiserver are used.
	// +optional
	CABundle []byte `json:"caBundle,omitempty" protobuf:"bytes,2,opt,name=caBundle"`
}

// ServiceReference holds a reference to Service.legacy.k8s.io
type ServiceReference struct {
	// namespace is the namespace of the service.
	// Required
	Namespace string `json:"namespace" protobuf:"bytes,1,opt,name=namespace"`
	// name is the name of the service.
	// Required
	Name string `json:"name" protobuf:"bytes,2,opt,name=name"`

	// path is an optional URL path at which the webhook will be contacted.
	// +optional
	Path *string `json:"path,omitempty" protobuf:"bytes,3,opt,name=path"`

	// port is an optional service port at which the webhook will be contacted.
	// `port` should be a valid port number (1-65535, inclusive).
	// Defaults to 443 for backward compatibility.
	// +optional
	Port *int32 `json:"port,omitempty" protobuf:"varint,4,opt,name=port"`
}

// CustomResourceDefinitionVersion describes a version for CRD.
type CustomResourceDefinitionVersion struct {
	// name is the version name, e.g. “v1”, “v2beta1”, etc.
	// The custom resources are served under this version at `/apis/<group>/<version>/...` if `served` is true.
	Name string `json:"name" protobuf:"bytes,1,opt,name=name"`
	// served is a flag enabling/disabling this version from being served via REST APIs
	Served bool `json:"served" protobuf:"varint,2,opt,name=served"`
	// storage indicates this version should be used when persisting custom resources to storage.
	// There must be exactly one version with storage=true.
	Storage bool `json:"storage" protobuf:"varint,3,opt,name=storage"`
	// deprecated indicates this version of the custom resource API is deprecated.
	// When set to true, API requests to this version receive a warning header in the server response.
	// Defaults to false.
	// +optional
	Deprecated bool `json:"deprecated,omitempty" protobuf:"varint,7,opt,name=deprecated"`
	// deprecationWarning overrides the default warning returned to API clients.
	// May only be set when `deprecated` is true.
	// The default warning indicates this version is deprecated and recommends use
	// of the newest served version of equal or greater stability, if one exists.
	// +optional
	DeprecationWarning *string `json:"deprecationWarning,omitempty" protobuf:"bytes,8,opt,name=deprecationWarning"`
	// schema describes the schema used for validation and pruning of this version of the custom resource.
	// Top-level and per-version schemas are mutually exclusive.
	// Per-version schemas must not all be set to identical values (top-level validation schema should be used instead).
	// +optional
	Schema *CustomResourceValidation `json:"schema,omitempty" protobuf:"bytes,4,opt,name=schema"`
	// subresources specify what subresources this version of the defined custom resource have.
	// Top-level and per-version subresources are mutually exclusive.
	// Per-version subresources must not all be set to identical values (top-level subresources should be used instead).
	// +optional
	Subresources *CustomResourceSubresources `json:"subresources,omitempty" protobuf:"bytes,5,opt,name=subresources"`
	// additionalPrinterColumns specifies additional columns returned in Table output.
	// See https://kubernetes.io/docs/reference/using-api/api-concepts/#receiving-resources-as-tables for details.
	// Top-level and per-version columns are mutually exclusive.
	// Per-version columns must not all be set to identical values (top-level columns should be used instead).
	// If no top-level or per-version columns are specified, a single column displaying the age of the custom resource is used.
	// +optional
	// +listType=atomic
	AdditionalPrinterColumns []CustomResourceColumnDefinition `json:"additionalPrinterColumns,omitempty" protobuf:"bytes,6,rep,name=additionalPrinterColumns"`

	// selectableFields specifies paths to fields that may be used as field selectors.
	// See https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors
	//
	// +featureGate=CustomResourceFieldSelectors
	// +optional
	// +listType=atomic
	SelectableFields []SelectableField `json:"selectableFields,omitempty" protobuf:"bytes,9,rep,name=selectableFields"`
}

// SelectableField specifies the JSON path of a field that may be used with field selectors.
type SelectableField struct {
	// jsonPath is a simple JSON path which is evaluated against each custom resource to produce a
	// field selector value.
	// Only JSON paths without the array notation are allowed.
	// Must point to a field of type string, boolean or integer. Types with enum values
	// and strings with formats are allowed.
	// If jsonPath refers to absent field in a resource, the jsonPath evaluates to an empty string.
	// Must not point to metadata fields.
	// Required.
	JSONPath string `json:"jsonPath" protobuf:"bytes,1,opt,name=jsonPath"`
}

// CustomResourceColumnDefinition specifies a column for server side printing.
type CustomResourceColumnDefinition struct {
	// name is a human readable name for the column.
	Name string `json:"name" protobuf:"bytes,1,opt,name=name"`
	// type is an OpenAPI type definition for this column.
	// See https://github.com/OAI/OpenAPI-Specification/blob/master/versions/2.0.md#data-types for details.
	Type string `json:"type" protobuf:"bytes,2,opt,name=type"`
	// format is an optional OpenAPI type definition for this column. The 'name' format is applied
	// to the primary identifier column to assist in clients identifying column is the resource name.
	// See https://github.com/OAI/OpenAPI-Specification/blob/master/versions/2.0.md#data-types for details.
	// +optional
	Format string `json:"format,omitempty" protobuf:"bytes,3,opt,name=format"`
	// description is a human readable description of this column.
	// +optional
	Description string `json:"description,omitempty" protobuf:"bytes,4,opt,name=description"`
	// priority is an integer defining the relative importance of this column compared to others. Lower
	// numbers are considered higher priority. Columns that may be omitted in limited space scenarios
	// should be given a priority greater than 0.
	// +optional
	Priority int32 `json:"priority,omitempty" protobuf:"bytes,5,opt,name=priority"`
	// JSONPath is a simple JSON path (i.e. with array notation) which is evaluated against
	// each custom resource to produce the value for this column.
	JSONPath string `json:"JSONPath" protobuf:"bytes,6,opt,name=JSONPath"`
}

// CustomResourceDefinitionNames indicates the names to serve this CustomResourceDefinition
type CustomResourceDefinitionNames struct {
	// plural is the plural name of the resource to serve.
	// The custom resources are served under `/apis/<group>/<version>/.../<plural>`.
	// Must match the name of the CustomResourceDefinition (in the form `<names.plural>.<group>`).
	// Must be all lowercase.
	Plural string `json:"plural" protobuf:"bytes,1,opt,name=plural"`
	// singular is the singular name of the resource. It must be all lowercase. Defaults to lowercased `kind`.
	// +optional
	Singular string `json:"singular,omitempty" protobuf:"bytes,2,opt,name=singular"`
	// shortNames are short names for the resource, exposed in API discovery documents,
	// and used by clients to support invocations like `kubectl get <shortname>`.
	// It must be all lowercase.
	// +optional
	// +listType=atomic
	ShortNames []string `json:"shortNames,omitempty" protobuf:"bytes,3,opt,name=shortNames"`
	// kind is the serialized kind of the resource. It is normally CamelCase and singular.
	// Custom resource instances will use this value as the `kind` attribute in API calls.
	Kind string `json:"kind" protobuf:"bytes,4,opt,name=kind"`
	// listKind is the serialized kind of the list for this resource. Defaults to "`kind`List".
	// +optional
	ListKind string `json:"listKind,omitempty" protobuf:"bytes,5,opt,name=listKind"`
	// categories is a list of grouped resources this custom resource belongs to (e.g. 'all').
	// This is published in API discovery documents, and used by clients to support invocations like
	// `kubectl get all`.
	// +optional
	// +listType=atomic
	Categories []string `json:"categories,omitempty" protobuf:"bytes,6,rep,name=categories"`
}

// ResourceScope is an enum defining the different scopes available to a custom resource
type ResourceScope string

const (
	ClusterScoped   ResourceScope = "Cluster"
	NamespaceScoped ResourceScope = "Namespaced"
)

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

// CustomResourceDefinitionConditionType is a valid value for CustomResourceDefinitionCondition.Type
type CustomResourceDefinitionConditionType string

const (
	// Established means that the resource has become active. A resource is established when all names are
	// accepted without a conflict for the first time. A resource stays established until deleted, even during
	// a later NamesAccepted due to changed names. Note that not all names can be changed.
	Established CustomResourceDefinitionConditionType = "Established"
	// NamesAccepted means the names chosen for this CustomResourceDefinition do not conflict with others in
	// the group and are therefore accepted.
	NamesAccepted CustomResourceDefinitionConditionType = "NamesAccepted"
	// NonStructuralSchema means that one or more OpenAPI schema is not structural.
	//
	// A schema is structural if it specifies types for all values, with the only exceptions of those with
	// - x-kubernetes-int-or-string: true — for fields which can be integer or string
	// - x-kubernetes-preserve-unknown-fields: true — for raw, unspecified JSON values
	// and there is no type, additionalProperties, default, nullable or x-kubernetes-* vendor extensions
	// specified under allOf, anyOf, oneOf or not.
	//
	// Non-structural schemas will not be allowed anymore in v1 API groups. Moreover, new features will not be
	// available for non-structural CRDs:
	// - pruning
	// - defaulting
	// - read-only
	// - OpenAPI publishing
	// - webhook conversion
	NonStructuralSchema CustomResourceDefinitionConditionType = "NonStructuralSchema"
	// Terminating means that the CustomResourceDefinition has been deleted and is cleaning up.
	Terminating CustomResourceDefinitionConditionType = "Terminating"
	// KubernetesAPIApprovalPolicyConformant indicates that an API in *.k8s.io or *.kubernetes.io is or is not approved.  For CRDs
	// outside those groups, this condition will not be set.  For CRDs inside those groups, the condition will
	// be true if .metadata.annotations["api-approved.kubernetes.io"] is set to a URL, otherwise it will be false.
	// See https://github.com/kubernetes/enhancements/pull/1111 for more details.
	KubernetesAPIApprovalPolicyConformant CustomResourceDefinitionConditionType = "KubernetesAPIApprovalPolicyConformant"
)

// CustomResourceDefinitionCondition contains details for the current condition of this pod.
type CustomResourceDefinitionCondition struct {
	// type is the type of the condition. Types include Established, NamesAccepted and Terminating.
	Type CustomResourceDefinitionConditionType `json:"type" protobuf:"bytes,1,opt,name=type,casttype=CustomResourceDefinitionConditionType"`
	// status is the status of the condition.
	// Can be True, False, Unknown.
	Status ConditionStatus `json:"status" protobuf:"bytes,2,opt,name=status,casttype=ConditionStatus"`
	// lastTransitionTime last time the condition transitioned from one status to another.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty" protobuf:"bytes,3,opt,name=lastTransitionTime"`
	// reason is a unique, one-word, CamelCase reason for the condition's last transition.
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,4,opt,name=reason"`
	// message is a human-readable message indicating details about last transition.
	// +optional
	Message string `json:"message,omitempty" protobuf:"bytes,5,opt,name=message"`
}

// CustomResourceDefinitionStatus indicates the state of the CustomResourceDefinition
type CustomResourceDefinitionStatus struct {
	// conditions indicate state for particular aspects of a CustomResourceDefinition
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []CustomResourceDefinitionCondition `json:"conditions" protobuf:"bytes,1,opt,name=conditions"`

	// acceptedNames are the names that are actually being used to serve discovery.
	// They may be different than the names in spec.
	// +optional
	AcceptedNames CustomResourceDefinitionNames `json:"acceptedNames" protobuf:"bytes,2,opt,name=acceptedNames"`

	// storedVersions lists all versions of CustomResources that were ever persisted. Tracking these
	// versions allows a migration path for stored versions in etcd. The field is mutable
	// so a migration controller can finish a migration to another version (ensuring
	// no old objects are left in storage), and then remove the rest of the
	// versions from this list.
	// Versions may not be removed from `spec.versions` while they exist in this list.
	// +optional
	// +listType=atomic
	StoredVersions []string `json:"storedVersions" protobuf:"bytes,3,rep,name=storedVersions"`
}

// CustomResourceCleanupFinalizer is the name of the finalizer which will delete instances of
// a CustomResourceDefinition
const CustomResourceCleanupFinalizer = "customresourcecleanup.apiextensions.k8s.io"

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.7
// +k8s:prerelease-lifecycle-gen:deprecated=1.16
// +k8s:prerelease-lifecycle-gen:removed=1.22
// +k8s:prerelease-lifecycle-gen:replacement=apiextensions.k8s.io,v1,CustomResourceDefinition

// CustomResourceDefinition represents a resource that should be exposed on the API server.  Its name MUST be in the format
// <.spec.name>.<.spec.group>.
// Deprecated in v1.16, planned for removal in v1.22. Use apiextensions.k8s.io/v1 CustomResourceDefinition instead.
type CustomResourceDefinition struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec describes how the user wants the resources to appear
	Spec CustomResourceDefinitionSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`
	// status indicates the actual state of the CustomResourceDefinition
	// +optional
	Status CustomResourceDefinitionStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.7
// +k8s:prerelease-lifecycle-gen:deprecated=1.16
// +k8s:prerelease-lifecycle-gen:removed=1.22
// +k8s:prerelease-lifecycle-gen:replacement=apiextensions.k8s.io,v1,CustomResourceDefinitionList

// CustomResourceDefinitionList is a list of CustomResourceDefinition objects.
type CustomResourceDefinitionList struct {
	metav1.TypeMeta `json:",inline"`

	// Standard object's metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items list individual CustomResourceDefinition objects
	Items []CustomResourceDefinition `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// CustomResourceValidation is a list of validation methods for CustomResources.
type CustomResourceValidation struct {
	// openAPIV3Schema is the OpenAPI v3 schema to use for validation and pruning.
	// +optional
	OpenAPIV3Schema *JSONSchemaProps `json:"openAPIV3Schema,omitempty" protobuf:"bytes,1,opt,name=openAPIV3Schema"`
}

// CustomResourceSubresources defines the status and scale subresources for CustomResources.
type CustomResourceSubresources struct {
	// status indicates the custom resource should serve a `/status` subresource.
	// When enabled:
	// 1. requests to the custom resource primary endpoint ignore changes to the `status` stanza of the object.
	// 2. requests to the custom resource `/status` subresource ignore changes to anything other than the `status` stanza of the object.
	// +optional
	Status *CustomResourceSubresourceStatus `json:"status,omitempty" protobuf:"bytes,1,opt,name=status"`
	// scale indicates the custom resource should serve a `/scale` subresource that returns an `autoscaling/v1` Scale object.
	// +optional
	Scale *CustomResourceSubresourceScale `json:"scale,omitempty" protobuf:"bytes,2,opt,name=scale"`
}

// CustomResourceSubresourceStatus defines how to serve the status subresource for CustomResources.
// Status is represented by the `.status` JSON path inside of a CustomResource. When set,
// * exposes a /status subresource for the custom resource
// * PUT requests to the /status subresource take a custom resource object, and ignore changes to anything except the status stanza
// * PUT/POST/PATCH requests to the custom resource ignore changes to the status stanza
type CustomResourceSubresourceStatus struct{}

// CustomResourceSubresourceScale defines how to serve the scale subresource for CustomResources.
type CustomResourceSubresourceScale struct {
	// specReplicasPath defines the JSON path inside of a custom resource that corresponds to Scale `spec.replicas`.
	// Only JSON paths without the array notation are allowed.
	// Must be a JSON Path under `.spec`.
	// If there is no value under the given path in the custom resource, the `/scale` subresource will return an error on GET.
	SpecReplicasPath string `json:"specReplicasPath" protobuf:"bytes,1,name=specReplicasPath"`
	// statusReplicasPath defines the JSON path inside of a custom resource that corresponds to Scale `status.replicas`.
	// Only JSON paths without the array notation are allowed.
	// Must be a JSON Path under `.status`.
	// If there is no value under the given path in the custom resource, the `status.replicas` value in the `/scale` subresource
	// will default to 0.
	StatusReplicasPath string `json:"statusReplicasPath" protobuf:"bytes,2,opt,name=statusReplicasPath"`
	// labelSelectorPath defines the JSON path inside of a custom resource that corresponds to Scale `status.selector`.
	// Only JSON paths without the array notation are allowed.
	// Must be a JSON Path under `.status` or `.spec`.
	// Must be set to work with HorizontalPodAutoscaler.
	// The field pointed by this JSON path must be a string field (not a complex selector struct)
	// which contains a serialized label selector in string form.
	// More info: https://kubernetes.io/docs/tasks/access-kubernetes-api/custom-resources/custom-resource-definitions#scale-subresource
	// If there is no value under the given path in the custom resource, the `status.selector` value in the `/scale`
	// subresource will default to the empty string.
	// +optional
	LabelSelectorPath *string `json:"labelSelectorPath,omitempty" protobuf:"bytes,3,opt,name=labelSelectorPath"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.13
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// This API is never served.  It is used for outbound requests from apiservers.  This will ensure it never gets served accidentally
// and having the generator against this group will protect future APIs which may be served.
// +k8s:prerelease-lifecycle-gen:replacement=apiextensions.k8s.io,v1,ConversionReview

// ConversionReview describes a conversion request/response.
type ConversionReview struct {
	metav1.TypeMeta `json:",inline"`
	// request describes the attributes for the conversion request.
	// +optional
	Request *ConversionRequest `json:"request,omitempty" protobuf:"bytes,1,opt,name=request"`
	// response describes the attributes for the conversion response.
	// +optional
	Response *ConversionResponse `json:"response,omitempty" protobuf:"bytes,2,opt,name=response"`
}

// ConversionRequest describes the conversion request parameters.
type ConversionRequest struct {
	// uid is an identifier for the individual request/response. It allows distinguishing instances of requests which are
	// otherwise identical (parallel requests, etc).
	// The UID is meant to track the round trip (request/response) between the Kubernetes API server and the webhook, not the user request.
	// It is suitable for correlating log entries between the webhook and apiserver, for either auditing or debugging.
	UID types.UID `json:"uid" protobuf:"bytes,1,name=uid"`
	// desiredAPIVersion is the version to convert given objects to. e.g. "myapi.example.com/v1"
	DesiredAPIVersion string `json:"desiredAPIVersion" protobuf:"bytes,2,name=desiredAPIVersion"`
	// objects is the list of custom resource objects to be converted.
	// +listType=atomic
	Objects []runtime.RawExtension `json:"objects" protobuf:"bytes,3,rep,name=objects"`
}

// ConversionResponse describes a conversion response.
type ConversionResponse struct {
	// uid is an identifier for the individual request/response.
	// This should be copied over from the corresponding `request.uid`.
	UID types.UID `json:"uid" protobuf:"bytes,1,name=uid"`
	// convertedObjects is the list of converted version of `request.objects` if the `result` is successful, otherwise empty.
	// The webhook is expected to set `apiVersion` of these objects to the `request.desiredAPIVersion`. The list
	// must also have the same size as the input list with the same objects in the same order (equal kind, metadata.uid, metadata.name and metadata.namespace).
	// The webhook is allowed to mutate labels and annotations. Any other change to the metadata is silently ignored.
	// +listType=atomic
	ConvertedObjects []runtime.RawExtension `json:"convertedObjects" protobuf:"bytes,2,rep,name=convertedObjects"`
	// result contains the result of conversion with extra details if the conversion failed. `result.status` determines if
	// the conversion failed or succeeded. The `result.status` field is required and represents the success or failure of the
	// conversion. A successful conversion must set `result.status` to `Success`. A failed conversion must set
	// `result.status` to `Failure` and provide more details in `result.message` and return http status 200. The `result.message`
	// will be used to construct an error message for the end user.
	Result metav1.Status `json:"result" protobuf:"bytes,3,name=result"`
}
