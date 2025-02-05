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

package apidiscovery

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// APIGroupDiscoveryList is a resource containing a list of APIGroupDiscovery.
// This is one of the types able to be returned from the /api and /apis endpoint and contains an aggregated
// list of API resources (built-ins, Custom Resource Definitions, resources from aggregated servers)
// that a cluster supports.
type APIGroupDiscoveryList struct {
	v1.TypeMeta `json:",inline"`
	// ResourceVersion will not be set, because this does not have a replayable ordering among multiple apiservers.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	v1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	// items is the list of groups for discovery. The groups are listed in priority order.
	Items []APIGroupDiscovery `json:"items,omitempty" protobuf:"bytes,2,opt,name=items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// APIGroupDiscovery holds information about which resources are being served for all version of the API Group.
// It contains a list of APIVersionDiscovery that holds a list of APIResourceDiscovery types served for a version.
// Versions are in descending order of preference, with the first version being the preferred entry.
type APIGroupDiscovery struct {
	v1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// The only field completed will be name. For instance, resourceVersion will be empty.
	// name is the name of the API group whose discovery information is presented here.
	// name is allowed to be "" to represent the legacy, ungroupified resources.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	v1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	// versions are the versions supported in this group. They are sorted in descending order of preference,
	// with the preferred version being the first entry.
	// +listType=map
	// +listMapKey=version
	Versions []APIVersionDiscovery `json:"versions,omitempty" protobuf:"bytes,2,opt,name=versions"`
}

// APIVersionDiscovery holds a list of APIResourceDiscovery types that are served for a particular version within an API Group.
type APIVersionDiscovery struct {
	// version is the name of the version within a group version.
	Version string `json:"version,omitempty" protobuf:"bytes,1,opt,name=version"`
	// resources is a list of APIResourceDiscovery objects for the corresponding group version.
	// +listType=map
	// +listMapKey=resource
	Resources []APIResourceDiscovery `json:"resources,omitempty" protobuf:"bytes,2,opt,name=resources"`
	// freshness marks whether a group version's discovery document is up to date.
	// "Current" indicates the discovery document was recently
	// refreshed. "Stale" indicates the discovery document could not
	// be retrieved and the returned discovery document may be
	// significantly out of date. Clients that require the latest
	// version of the discovery information be retrieved before
	// performing an operation should not use the aggregated document
	// and instead retrieve the necessary version docs directly.
	Freshness DiscoveryFreshness `json:"freshness,omitempty" protobuf:"bytes,3,opt,name=freshness"`
}

// APIResourceDiscovery provides information about an API resource for discovery.
type APIResourceDiscovery struct {
	// resource is the plural name of the resource.  This is used in the URL path and is the unique identifier
	// for this resource across all versions in the API group.
	// Resources with non-empty groups are located at /apis/<APIGroupDiscovery.objectMeta.name>/<APIVersionDiscovery.version>/<APIResourceDiscovery.Resource>
	// Resources with empty groups are located at /api/v1/<APIResourceDiscovery.Resource>
	Resource string `json:"resource,omitempty" protobuf:"bytes,1,opt,name=resource"`
	// responseKind describes the group, version, and kind of the serialization schema for the object type this endpoint typically returns.
	// APIs may return other objects types at their discretion, such as error conditions, requests for alternate representations, or other operation specific behavior.
	// This value will be null or empty if an APIService reports subresources but supports no operations on the parent resource
	ResponseKind *v1.GroupVersionKind `json:"responseKind,omitempty" protobuf:"bytes,2,opt,name=responseKind"`
	// scope indicates the scope of a resource, either Cluster or Namespaced
	Scope ResourceScope `json:"scope,omitempty" protobuf:"bytes,3,opt,name=scope"`
	// singularResource is the singular name of the resource.  This allows clients to handle plural and singular opaquely.
	// For many clients the singular form of the resource will be more understandable to users reading messages and should be used when integrating the name of the resource into a sentence.
	// The command line tool kubectl, for example, allows use of the singular resource name in place of plurals.
	// The singular form of a resource should always be an optional element - when in doubt use the canonical resource name.
	SingularResource string `json:"singularResource,omitempty" protobuf:"bytes,4,opt,name=singularResource"`
	// verbs is a list of supported API operation types (this includes
	// but is not limited to get, list, watch, create, update, patch,
	// delete, deletecollection, and proxy).
	// +listType=set
	Verbs []string `json:"verbs,omitempty" protobuf:"bytes,5,opt,name=verbs"`
	// shortNames is a list of suggested short names of the resource.
	// +listType=set
	ShortNames []string `json:"shortNames,omitempty" protobuf:"bytes,6,opt,name=shortNames"`
	// categories is a list of the grouped resources this resource belongs to (e.g. 'all').
	// Clients may use this to simplify acting on multiple resource types at once.
	// +listType=set
	Categories []string `json:"categories,omitempty" protobuf:"bytes,7,opt,name=categories"`
	// subresources is a list of subresources provided by this resource. Subresources are located at /apis/<APIGroupDiscovery.objectMeta.name>/<APIVersionDiscovery.version>/<APIResourceDiscovery.Resource>/name-of-instance/<APIResourceDiscovery.subresources[i].subresource>
	// +listType=map
	// +listMapKey=subresource
	Subresources []APISubresourceDiscovery `json:"subresources,omitempty" protobuf:"bytes,8,opt,name=subresources"`
}

// ResourceScope is an enum defining the different scopes available to a resource.
type ResourceScope string

const (
	ScopeCluster   ResourceScope = "Cluster"
	ScopeNamespace ResourceScope = "Namespaced"
)

// DiscoveryFreshness is an enum defining whether the Discovery document published by an apiservice is up to date (fresh).
type DiscoveryFreshness string

const (
	DiscoveryFreshnessCurrent DiscoveryFreshness = "Current"
	DiscoveryFreshnessStale   DiscoveryFreshness = "Stale"
)

// APISubresourceDiscovery provides information about an API subresource for discovery.
type APISubresourceDiscovery struct {
	// subresource is the name of the subresource.  This is used in the URL path and is the unique identifier
	// for this resource across all versions.
	Subresource string `json:"subresource,omitempty" protobuf:"bytes,1,opt,name=subresource"`
	// responseKind describes the group, version, and kind of the serialization schema for the object type this endpoint typically returns.
	// Some subresources do not return normal resources, these will have null or empty return types.
	ResponseKind *v1.GroupVersionKind `json:"responseKind,omitempty" protobuf:"bytes,2,opt,name=responseKind"`
	// acceptedTypes describes the kinds that this endpoint accepts.
	// Subresources may accept the standard content types or define
	// custom negotiation schemes. The list may not be exhaustive for
	// all operations.
	// +listType=map
	// +listMapKey=group
	// +listMapKey=version
	// +listMapKey=kind
	AcceptedTypes []v1.GroupVersionKind `json:"acceptedTypes,omitempty" protobuf:"bytes,3,opt,name=acceptedTypes"`
	// verbs is a list of supported API operation types (this includes
	// but is not limited to get, list, watch, create, update, patch,
	// delete, deletecollection, and proxy). Subresources may define
	// custom verbs outside the standard Kubernetes verb set. Clients
	// should expect the behavior of standard verbs to align with
	// Kubernetes interaction conventions.
	// +listType=set
	Verbs []string `json:"verbs,omitempty" protobuf:"bytes,4,opt,name=verbs"`
}
