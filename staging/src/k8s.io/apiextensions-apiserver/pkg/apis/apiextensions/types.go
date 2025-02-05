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

package apiextensions

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConversionStrategyType describes different conversion types.
type ConversionStrategyType string

const (
	// NoneConverter is a converter that only sets apiversion of the CR and leave everything else unchanged.
	NoneConverter ConversionStrategyType = "None"
	// WebhookConverter is a converter that calls to an external webhook to convert the CR.
	WebhookConverter ConversionStrategyType = "Webhook"
)

// CustomResourceDefinitionSpec describes how a user wants their resource to appear
type CustomResourceDefinitionSpec struct {
	// Group is the group this resource belongs in
	Group string
	// Version is the version this resource belongs in
	// Should be always first item in Versions field if provided.
	// Optional, but at least one of Version or Versions must be set.
	// Deprecated: Please use `Versions`.
	Version string
	// Names are the names used to describe this custom resource
	Names CustomResourceDefinitionNames
	// Scope indicates whether this resource is cluster or namespace scoped.  Default is namespaced
	Scope ResourceScope
	// Validation describes the validation methods for CustomResources
	// Optional, the global validation schema for all versions.
	// Top-level and per-version schemas are mutually exclusive.
	// +optional
	Validation *CustomResourceValidation
	// Subresources describes the subresources for CustomResource
	// Optional, the global subresources for all versions.
	// Top-level and per-version subresources are mutually exclusive.
	// +optional
	Subresources *CustomResourceSubresources
	// Versions is the list of all supported versions for this resource.
	// If Version field is provided, this field is optional.
	// Validation: All versions must use the same validation schema for now. i.e., top
	// level Validation field is applied to all of these versions.
	// Order: The version name will be used to compute the order.
	// If the version string is "kube-like", it will sort above non "kube-like" version strings, which are ordered
	// lexicographically. "Kube-like" versions start with a "v", then are followed by a number (the major version),
	// then optionally the string "alpha" or "beta" and another number (the minor version). These are sorted first
	// by GA > beta > alpha (where GA is a version with no suffix such as beta or alpha), and then by comparing
	// major version, then minor version. An example sorted list of versions:
	// v10, v2, v1, v11beta2, v10beta3, v3beta1, v12alpha1, v11alpha2, foo1, foo10.
	Versions []CustomResourceDefinitionVersion
	// AdditionalPrinterColumns are additional columns shown e.g. in kubectl next to the name. Defaults to a created-at column.
	// Optional, the global columns for all versions.
	// Top-level and per-version columns are mutually exclusive.
	// +optional
	AdditionalPrinterColumns []CustomResourceColumnDefinition
	// selectableFields specifies paths to fields that may be used as field selectors.
	// A maximum of 8 selectable fields are allowed.
	// See https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors
	// Top-level and per-version columns are mutually exclusive.
	// +optional
	SelectableFields []SelectableField

	// `conversion` defines conversion settings for the CRD.
	Conversion *CustomResourceConversion

	// preserveUnknownFields disables pruning of object fields which are not
	// specified in the OpenAPI schema. apiVersion, kind, metadata and known
	// fields inside metadata are always preserved.
	// Defaults to true in v1beta and will default to false in v1.
	PreserveUnknownFields *bool
}

// CustomResourceConversion describes how to convert different versions of a CR.
type CustomResourceConversion struct {
	// `strategy` specifies the conversion strategy. Allowed values are:
	// - `None`: The converter only change the apiVersion and would not touch any other field in the CR.
	// - `Webhook`: API Server will call to an external webhook to do the conversion. Additional information
	//   is needed for this option. This requires spec.preserveUnknownFields to be false.
	Strategy ConversionStrategyType

	// `webhookClientConfig` is the instructions for how to call the webhook if strategy is `Webhook`.
	WebhookClientConfig *WebhookClientConfig

	// ConversionReviewVersions is an ordered list of preferred `ConversionReview`
	// versions the Webhook expects. API server will try to use first version in
	// the list which it supports. If none of the versions specified in this list
	// supported by API server, conversion will fail for this object.
	// If a persisted Webhook configuration specifies allowed versions and does not
	// include any versions known to the API Server, calls to the webhook will fail.
	// +optional
	ConversionReviewVersions []string
}

// WebhookClientConfig contains the information to make a TLS
// connection with the webhook. It has the same field as admissionregistration.internal.WebhookClientConfig.
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

// CustomResourceDefinitionVersion describes a version for CRD.
type CustomResourceDefinitionVersion struct {
	// Name is the version name, e.g. “v1”, “v2beta1”, etc.
	Name string
	// Served is a flag enabling/disabling this version from being served via REST APIs
	Served bool
	// Storage flags the version as storage version. There must be exactly one flagged
	// as storage version.
	Storage bool
	// deprecated indicates this version of the custom resource API is deprecated.
	// When set to true, API requests to this version receive a warning header in the server response.
	// Defaults to false.
	Deprecated bool
	// deprecationWarning overrides the default warning returned to API clients.
	// May only be set when `deprecated` is true.
	// The default warning indicates this version is deprecated and recommends use
	// of the newest served version of equal or greater stability, if one exists.
	DeprecationWarning *string
	// Schema describes the schema for CustomResource used in validation, pruning, and defaulting.
	// Top-level and per-version schemas are mutually exclusive.
	// Per-version schemas must not all be set to identical values (top-level validation schema should be used instead)
	// This field is alpha-level and is only honored by servers that enable the CustomResourceWebhookConversion feature.
	// +optional
	Schema *CustomResourceValidation
	// Subresources describes the subresources for CustomResource
	// Top-level and per-version subresources are mutually exclusive.
	// Per-version subresources must not all be set to identical values (top-level subresources should be used instead)
	// This field is alpha-level and is only honored by servers that enable the CustomResourceWebhookConversion feature.
	// +optional
	Subresources *CustomResourceSubresources
	// AdditionalPrinterColumns are additional columns shown e.g. in kubectl next to the name. Defaults to a created-at column.
	// Top-level and per-version columns are mutually exclusive.
	// Per-version columns must not all be set to identical values (top-level columns should be used instead)
	// This field is alpha-level and is only honored by servers that enable the CustomResourceWebhookConversion feature.
	// NOTE: CRDs created prior to 1.13 populated the top-level additionalPrinterColumns field by default. To apply an
	// update that changes to per-version additionalPrinterColumns, the top-level additionalPrinterColumns field must
	// be explicitly set to null
	// +optional
	AdditionalPrinterColumns []CustomResourceColumnDefinition

	// selectableFields specifies paths to fields that may be used as field selectors.
	// A maximum of 8 selectable fields are allowed.
	// See https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors
	// +optional
	SelectableFields []SelectableField
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
	JSONPath string
}

// CustomResourceColumnDefinition specifies a column for server side printing.
type CustomResourceColumnDefinition struct {
	// name is a human readable name for the column.
	Name string
	// type is an OpenAPI type definition for this column.
	// See https://github.com/OAI/OpenAPI-Specification/blob/master/versions/2.0.md#data-types for more.
	Type string
	// format is an optional OpenAPI type definition for this column. The 'name' format is applied
	// to the primary identifier column to assist in clients identifying column is the resource name.
	// See https://github.com/OAI/OpenAPI-Specification/blob/master/versions/2.0.md#data-types for more.
	Format string
	// description is a human readable description of this column.
	Description string
	// priority is an integer defining the relative importance of this column compared to others. Lower
	// numbers are considered higher priority. Columns that may be omitted in limited space scenarios
	// should be given a higher priority.
	Priority int32

	// JSONPath is a simple JSON path, i.e. without array notation.
	JSONPath string
}

// CustomResourceDefinitionNames indicates the names to serve this CustomResourceDefinition
type CustomResourceDefinitionNames struct {
	// Plural is the plural name of the resource to serve.  It must match the name of the CustomResourceDefinition-registration
	// too: plural.group and it must be all lowercase.
	Plural string
	// Singular is the singular name of the resource.  It must be all lowercase  Defaults to lowercased <kind>
	Singular string
	// ShortNames are short names for the resource.  It must be all lowercase.
	ShortNames []string
	// Kind is the serialized kind of the resource.  It is normally CamelCase and singular.
	Kind string
	// ListKind is the serialized kind of the list for this resource.  Defaults to <kind>List.
	ListKind string
	// Categories is a list of grouped resources custom resources belong to (e.g. 'all')
	// +optional
	Categories []string
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
	// Type is the type of the condition. Types include Established, NamesAccepted and Terminating.
	Type CustomResourceDefinitionConditionType
	// Status is the status of the condition.
	// Can be True, False, Unknown.
	Status ConditionStatus
	// Last time the condition transitioned from one status to another.
	// +optional
	LastTransitionTime metav1.Time
	// Unique, one-word, CamelCase reason for the condition's last transition.
	// +optional
	Reason string
	// Human-readable message indicating details about last transition.
	// +optional
	Message string
}

// CustomResourceDefinitionStatus indicates the state of the CustomResourceDefinition
type CustomResourceDefinitionStatus struct {
	// Conditions indicate state for particular aspects of a CustomResourceDefinition
	// +listType=map
	// +listMapKey=type
	Conditions []CustomResourceDefinitionCondition

	// AcceptedNames are the names that are actually being used to serve discovery
	// They may be different than the names in spec.
	AcceptedNames CustomResourceDefinitionNames

	// StoredVersions are all versions of CustomResources that were ever persisted. Tracking these
	// versions allows a migration path for stored versions in etcd. The field is mutable
	// so the migration controller can first finish a migration to another version (i.e.
	// that no old objects are left in the storage), and then remove the rest of the
	// versions from this list.
	// None of the versions in this list can be removed from the spec.Versions field.
	StoredVersions []string
}

// CustomResourceCleanupFinalizer is the name of the finalizer which will delete instances of
// a CustomResourceDefinition
const CustomResourceCleanupFinalizer = "customresourcecleanup.apiextensions.k8s.io"

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CustomResourceDefinition represents a resource that should be exposed on the API server.  Its name MUST be in the format
// <.spec.name>.<.spec.group>.
type CustomResourceDefinition struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	// Spec describes how the user wants the resources to appear
	Spec CustomResourceDefinitionSpec
	// Status indicates the actual state of the CustomResourceDefinition
	Status CustomResourceDefinitionStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CustomResourceDefinitionList is a list of CustomResourceDefinition objects.
type CustomResourceDefinitionList struct {
	metav1.TypeMeta
	metav1.ListMeta

	// Items individual CustomResourceDefinitions
	Items []CustomResourceDefinition
}

// CustomResourceValidation is a list of validation methods for CustomResources.
type CustomResourceValidation struct {
	// OpenAPIV3Schema is the OpenAPI v3 schema to be validated against.
	OpenAPIV3Schema *JSONSchemaProps
}

// CustomResourceSubresources defines the status and scale subresources for CustomResources.
type CustomResourceSubresources struct {
	// Status denotes the status subresource for CustomResources
	Status *CustomResourceSubresourceStatus
	// Scale denotes the scale subresource for CustomResources
	Scale *CustomResourceSubresourceScale
}

// CustomResourceSubresourceStatus defines how to serve the status subresource for CustomResources.
// Status is represented by the `.status` JSON path inside of a CustomResource. When set,
// * exposes a /status subresource for the custom resource
// * PUT requests to the /status subresource take a custom resource object, and ignore changes to anything except the status stanza
// * PUT/POST/PATCH requests to the custom resource ignore changes to the status stanza
type CustomResourceSubresourceStatus struct{}

// CustomResourceSubresourceScale defines how to serve the scale subresource for CustomResources.
type CustomResourceSubresourceScale struct {
	// SpecReplicasPath defines the JSON path inside of a CustomResource that corresponds to Scale.Spec.Replicas.
	// Only JSON paths without the array notation are allowed.
	// Must be a JSON Path under .spec.
	// If there is no value under the given path in the CustomResource, the /scale subresource will return an error on GET.
	SpecReplicasPath string
	// StatusReplicasPath defines the JSON path inside of a CustomResource that corresponds to Scale.Status.Replicas.
	// Only JSON paths without the array notation are allowed.
	// Must be a JSON Path under .status.
	// If there is no value under the given path in the CustomResource, the status replica value in the /scale subresource
	// will default to 0.
	StatusReplicasPath string
	// LabelSelectorPath defines the JSON path inside of a CustomResource that corresponds to Scale.Status.Selector.
	// Only JSON paths without the array notation are allowed.
	// Must be a JSON Path under .status or .spec.
	// Must be set to work with HPA.
	// The field pointed by this JSON path must be a string field (not a complex selector struct)
	// which contains a serialized label selector in string form.
	// More info: https://kubernetes.io/docs/tasks/access-kubernetes-api/custom-resources/custom-resource-definitions#scale-subresource
	// If there is no value under the given path in the CustomResource, the status label selector value in the /scale
	// subresource will default to the empty string.
	// +optional
	LabelSelectorPath *string
}
