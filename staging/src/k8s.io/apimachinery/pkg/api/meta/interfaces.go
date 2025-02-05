/*
Copyright 2014 The Kubernetes Authors.

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

package meta

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type ListMetaAccessor interface {
	GetListMeta() List
}

// List lets you work with list metadata from any of the versioned or
// internal API objects. Attempting to set or retrieve a field on an object that does
// not support that field will be a no-op and return a default value.
type List metav1.ListInterface

// Type exposes the type and APIVersion of versioned or internal API objects.
type Type metav1.Type

// MetadataAccessor lets you work with object and list metadata from any of the versioned or
// internal API objects. Attempting to set or retrieve a field on an object that does
// not support that field (Name, UID, Namespace on lists) will be a no-op and return
// a default value.
//
// MetadataAccessor exposes Interface in a way that can be used with multiple objects.
type MetadataAccessor interface {
	APIVersion(obj runtime.Object) (string, error)
	SetAPIVersion(obj runtime.Object, version string) error

	Kind(obj runtime.Object) (string, error)
	SetKind(obj runtime.Object, kind string) error

	Namespace(obj runtime.Object) (string, error)
	SetNamespace(obj runtime.Object, namespace string) error

	Name(obj runtime.Object) (string, error)
	SetName(obj runtime.Object, name string) error

	GenerateName(obj runtime.Object) (string, error)
	SetGenerateName(obj runtime.Object, name string) error

	UID(obj runtime.Object) (types.UID, error)
	SetUID(obj runtime.Object, uid types.UID) error

	SelfLink(obj runtime.Object) (string, error)
	SetSelfLink(obj runtime.Object, selfLink string) error

	Labels(obj runtime.Object) (map[string]string, error)
	SetLabels(obj runtime.Object, labels map[string]string) error

	Annotations(obj runtime.Object) (map[string]string, error)
	SetAnnotations(obj runtime.Object, annotations map[string]string) error

	Continue(obj runtime.Object) (string, error)
	SetContinue(obj runtime.Object, c string) error

	runtime.ResourceVersioner
}

type RESTScopeName string

const (
	RESTScopeNameNamespace RESTScopeName = "namespace"
	RESTScopeNameRoot      RESTScopeName = "root"
)

// RESTScope contains the information needed to deal with REST resources that are in a resource hierarchy
type RESTScope interface {
	// Name of the scope
	Name() RESTScopeName
}

// RESTMapping contains the information needed to deal with objects of a specific
// resource and kind in a RESTful manner.
type RESTMapping struct {
	// Resource is the GroupVersionResource (location) for this endpoint
	Resource schema.GroupVersionResource

	// GroupVersionKind is the GroupVersionKind (data format) to submit to this endpoint
	GroupVersionKind schema.GroupVersionKind

	// Scope contains the information needed to deal with REST Resources that are in a resource hierarchy
	Scope RESTScope
}

// RESTMapper allows clients to map resources to kind, and map kind and version
// to interfaces for manipulating those objects. It is primarily intended for
// consumers of Kubernetes compatible REST APIs as defined in docs/devel/api-conventions.md.
//
// The Kubernetes API provides versioned resources and object kinds which are scoped
// to API groups. In other words, kinds and resources should not be assumed to be
// unique across groups.
//
// Deprecated: use RESTMapperWithContext instead.
type RESTMapper interface {
	// KindFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
	KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error)

	// KindsFor takes a partial resource and returns the list of potential kinds in priority order
	KindsFor(resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error)

	// ResourceFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
	ResourceFor(input schema.GroupVersionResource) (schema.GroupVersionResource, error)

	// ResourcesFor takes a partial resource and returns the list of potential resource in priority order
	ResourcesFor(input schema.GroupVersionResource) ([]schema.GroupVersionResource, error)

	// RESTMapping identifies a preferred resource mapping for the provided group kind.
	RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error)
	// RESTMappings returns all resource mappings for the provided group kind if no
	// version search is provided. Otherwise identifies a preferred resource mapping for
	// the provided version(s).
	RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error)

	ResourceSingularizer(resource string) (singular string, err error)
}

// RESTMapperWithContext allows clients to map resources to kind, and map kind and version
// to interfaces for manipulating those objects. It is primarily intended for
// consumers of Kubernetes compatible REST APIs as defined in docs/devel/api-conventions.md.
//
// The Kubernetes API provides versioned resources and object kinds which are scoped
// to API groups. In other words, kinds and resources should not be assumed to be
// unique across groups.
type RESTMapperWithContext interface {
	// KindFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
	KindForWithContext(ctx context.Context, resource schema.GroupVersionResource) (schema.GroupVersionKind, error)

	// KindsFor takes a partial resource and returns the list of potential kinds in priority order
	KindsForWithContext(ctx context.Context, resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error)

	// ResourceFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
	ResourceForWithContext(ctx context.Context, input schema.GroupVersionResource) (schema.GroupVersionResource, error)

	// ResourcesFor takes a partial resource and returns the list of potential resource in priority order
	ResourcesForWithContext(ctx context.Context, input schema.GroupVersionResource) ([]schema.GroupVersionResource, error)

	// RESTMapping identifies a preferred resource mapping for the provided group kind.
	RESTMappingWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) (*RESTMapping, error)
	// RESTMappings returns all resource mappings for the provided group kind if no
	// version search is provided. Otherwise identifies a preferred resource mapping for
	// the provided version(s).
	RESTMappingsWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) ([]*RESTMapping, error)

	ResourceSingularizerWithContext(ctx context.Context, resource string) (singular string, err error)
}

func ToRESTMapperWithContext(m RESTMapper) RESTMapperWithContext {
	if m == nil {
		return nil
	}
	if m, ok := m.(RESTMapperWithContext); ok {
		return m
	}
	return &restMapperWrapper{
		delegate: m,
	}
}

type restMapperWrapper struct {
	delegate RESTMapper
}

func (m *restMapperWrapper) KindForWithContext(ctx context.Context, resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return m.delegate.KindFor(resource)
}
func (m *restMapperWrapper) KindsForWithContext(ctx context.Context, resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return m.delegate.KindsFor(resource)
}
func (m *restMapperWrapper) ResourceForWithContext(ctx context.Context, input schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return m.delegate.ResourceFor(input)
}
func (m *restMapperWrapper) ResourcesForWithContext(ctx context.Context, input schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return m.delegate.ResourcesFor(input)
}
func (m *restMapperWrapper) RESTMappingWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	return m.delegate.RESTMapping(gk, versions...)
}
func (m *restMapperWrapper) RESTMappingsWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	return m.delegate.RESTMappings(gk, versions...)
}
func (m *restMapperWrapper) ResourceSingularizerWithContext(ctx context.Context, resource string) (singular string, err error) {
	return m.delegate.ResourceSingularizer(resource)
}

// ResettableRESTMapper is a RESTMapper which is capable of resetting itself
// from discovery.
// All rest mappers that delegate to other rest mappers must implement this interface and dynamically
// check if the delegate mapper supports the Reset() operation.
//
// Deprecated: use ResettableRESTMapperWithContext instead.
type ResettableRESTMapper interface {
	RESTMapper
	Reset()
}

// ResettableRESTMapperWithContext is a RESTMapper which is capable of resetting itself
// from discovery.
// All rest mappers that delegate to other rest mappers must implement this interface and dynamically
// check if the delegate mapper supports the ResetWithContext() operation.
type ResettableRESTMapperWithContext interface {
	RESTMapperWithContext
	ResetWithContext(ctx context.Context)
}

func ToResettableRESTMapperWithContext(m ResettableRESTMapper) ResettableRESTMapperWithContext {
	if m == nil {
		return nil
	}
	if m, ok := m.(ResettableRESTMapperWithContext); ok {
		return m
	}
	return &resettableRESTMapperWrapper{
		RESTMapperWithContext: ToRESTMapperWithContext(m),
		delegate:              m,
	}
}

type resettableRESTMapperWrapper struct {
	RESTMapperWithContext
	delegate ResettableRESTMapper
}

func (m *resettableRESTMapperWrapper) ResetWithContext(ctx context.Context) {
	m.delegate.Reset()
}
