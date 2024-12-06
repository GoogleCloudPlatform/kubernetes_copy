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

// TODO: move everything in this file to pkg/api/rest
package meta

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Implements RESTScope interface
type restScope struct {
	name RESTScopeName
}

func (r *restScope) Name() RESTScopeName {
	return r.name
}

var RESTScopeNamespace = &restScope{
	name: RESTScopeNameNamespace,
}

var RESTScopeRoot = &restScope{
	name: RESTScopeNameRoot,
}

// DefaultRESTMapper exposes mappings between the types defined in a
// runtime.Scheme. It assumes that all types defined the provided scheme
// can be mapped with the provided MetadataAccessor and Codec interfaces.
//
// The resource name of a Kind is defined as the lowercase,
// English-plural version of the Kind string.
// When converting from resource to Kind, the singular version of the
// resource name is also accepted for convenience.
//
// TODO: Only accept plural for some operations for increased control?
// (`get pod bar` vs `get pods bar`)
type DefaultRESTMapper struct {
	defaultGroupVersions []schema.GroupVersion

	resourceToKind       map[schema.GroupVersionResource]schema.GroupVersionKind
	kindToPluralResource map[schema.GroupVersionKind]schema.GroupVersionResource
	kindToScope          map[schema.GroupVersionKind]RESTScope
	singularToPlural     map[schema.GroupVersionResource]schema.GroupVersionResource
	pluralToSingular     map[schema.GroupVersionResource]schema.GroupVersionResource
}

func (m *DefaultRESTMapper) String() string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("DefaultRESTMapper{kindToPluralResource=%v}", m.kindToPluralResource)
}

var _ RESTMapper = &DefaultRESTMapper{}
var _ RESTMapperWithContext = &DefaultRESTMapper{}

// NewDefaultRESTMapper initializes a mapping between Kind and APIVersion
// to a resource name and back based on the objects in a runtime.Scheme
// and the Kubernetes API conventions. Takes a group name, a priority list of the versions
// to search when an object has no default version (set empty to return an error),
// and a function that retrieves the correct metadata for a given version.
func NewDefaultRESTMapper(defaultGroupVersions []schema.GroupVersion) *DefaultRESTMapper {
	resourceToKind := make(map[schema.GroupVersionResource]schema.GroupVersionKind)
	kindToPluralResource := make(map[schema.GroupVersionKind]schema.GroupVersionResource)
	kindToScope := make(map[schema.GroupVersionKind]RESTScope)
	singularToPlural := make(map[schema.GroupVersionResource]schema.GroupVersionResource)
	pluralToSingular := make(map[schema.GroupVersionResource]schema.GroupVersionResource)
	// TODO: verify name mappings work correctly when versions differ

	return &DefaultRESTMapper{
		resourceToKind:       resourceToKind,
		kindToPluralResource: kindToPluralResource,
		kindToScope:          kindToScope,
		defaultGroupVersions: defaultGroupVersions,
		singularToPlural:     singularToPlural,
		pluralToSingular:     pluralToSingular,
	}
}

func (m *DefaultRESTMapper) Add(kind schema.GroupVersionKind, scope RESTScope) {
	plural, singular := UnsafeGuessKindToResource(kind)
	m.AddSpecific(kind, plural, singular, scope)
}

func (m *DefaultRESTMapper) AddSpecific(kind schema.GroupVersionKind, plural, singular schema.GroupVersionResource, scope RESTScope) {
	m.singularToPlural[singular] = plural
	m.pluralToSingular[plural] = singular

	m.resourceToKind[singular] = kind
	m.resourceToKind[plural] = kind

	m.kindToPluralResource[kind] = plural
	m.kindToScope[kind] = scope
}

// unpluralizedSuffixes is a list of resource suffixes that are the same plural and singular
// This is only is only necessary because some bits of code are lazy and don't actually use the RESTMapper like they should.
// TODO eliminate this so that different callers can correctly map to resources.  This probably means updating all
// callers to use the RESTMapper they mean.
var unpluralizedSuffixes = []string{
	"endpoints",
}

// UnsafeGuessKindToResource converts Kind to a resource name.
// Broken. This method only "sort of" works when used outside of this package.  It assumes that Kinds and Resources match
// and they aren't guaranteed to do so.
func UnsafeGuessKindToResource(kind schema.GroupVersionKind) ( /*plural*/ schema.GroupVersionResource /*singular*/, schema.GroupVersionResource) {
	kindName := kind.Kind
	if len(kindName) == 0 {
		return schema.GroupVersionResource{}, schema.GroupVersionResource{}
	}
	singularName := strings.ToLower(kindName)
	singular := kind.GroupVersion().WithResource(singularName)

	for _, skip := range unpluralizedSuffixes {
		if strings.HasSuffix(singularName, skip) {
			return singular, singular
		}
	}

	switch string(singularName[len(singularName)-1]) {
	case "s":
		return kind.GroupVersion().WithResource(singularName + "es"), singular
	case "y":
		return kind.GroupVersion().WithResource(strings.TrimSuffix(singularName, "y") + "ies"), singular
	}

	return kind.GroupVersion().WithResource(singularName + "s"), singular
}

// ResourceSingularizer implements RESTMapper
// It converts a resource name from plural to singular (e.g., from pods to pod)
func (m *DefaultRESTMapper) ResourceSingularizer(resourceType string) (string, error) {
	partialResource := schema.GroupVersionResource{Resource: resourceType}
	resources, err := m.ResourcesFor(partialResource)
	if err != nil {
		return resourceType, err
	}

	singular := schema.GroupVersionResource{}
	for _, curr := range resources {
		currSingular, ok := m.pluralToSingular[curr]
		if !ok {
			continue
		}
		if singular.Empty() {
			singular = currSingular
			continue
		}

		if currSingular.Resource != singular.Resource {
			return resourceType, fmt.Errorf("multiple possible singular resources (%v) found for %v", resources, resourceType)
		}
	}

	if singular.Empty() {
		return resourceType, fmt.Errorf("no singular of resource %v has been defined", resourceType)
	}

	return singular.Resource, nil
}

func (m *DefaultRESTMapper) ResourceSingularizerWithContext(_ context.Context, resourceType string) (string, error) {
	return m.ResourceSingularizer(resourceType)
}

// coerceResourceForMatching makes the resource lower case and converts internal versions to unspecified (legacy behavior)
func coerceResourceForMatching(resource schema.GroupVersionResource) schema.GroupVersionResource {
	resource.Resource = strings.ToLower(resource.Resource)
	if resource.Version == runtime.APIVersionInternal {
		resource.Version = ""
	}

	return resource
}

func (m *DefaultRESTMapper) ResourcesFor(input schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	resource := coerceResourceForMatching(input)

	hasResource := len(resource.Resource) > 0
	hasGroup := len(resource.Group) > 0
	hasVersion := len(resource.Version) > 0

	if !hasResource {
		return nil, fmt.Errorf("a resource must be present, got: %v", resource)
	}

	ret := []schema.GroupVersionResource{}
	switch {
	case hasGroup && hasVersion:
		// fully qualified.  Find the exact match
		for plural, singular := range m.pluralToSingular {
			if singular == resource {
				ret = append(ret, plural)
				break
			}
			if plural == resource {
				ret = append(ret, plural)
				break
			}
		}

	case hasGroup:
		// given a group, prefer an exact match.  If you don't find one, resort to a prefix match on group
		foundExactMatch := false
		requestedGroupResource := resource.GroupResource()
		for plural, singular := range m.pluralToSingular {
			if singular.GroupResource() == requestedGroupResource {
				foundExactMatch = true
				ret = append(ret, plural)
			}
			if plural.GroupResource() == requestedGroupResource {
				foundExactMatch = true
				ret = append(ret, plural)
			}
		}

		// if you didn't find an exact match, match on group prefixing. This allows storageclass.storage to match
		// storageclass.storage.k8s.io
		if !foundExactMatch {
			for plural, singular := range m.pluralToSingular {
				if !strings.HasPrefix(plural.Group, requestedGroupResource.Group) {
					continue
				}
				if singular.Resource == requestedGroupResource.Resource {
					ret = append(ret, plural)
				}
				if plural.Resource == requestedGroupResource.Resource {
					ret = append(ret, plural)
				}
			}

		}

	case hasVersion:
		for plural, singular := range m.pluralToSingular {
			if singular.Version == resource.Version && singular.Resource == resource.Resource {
				ret = append(ret, plural)
			}
			if plural.Version == resource.Version && plural.Resource == resource.Resource {
				ret = append(ret, plural)
			}
		}

	default:
		for plural, singular := range m.pluralToSingular {
			if singular.Resource == resource.Resource {
				ret = append(ret, plural)
			}
			if plural.Resource == resource.Resource {
				ret = append(ret, plural)
			}
		}
	}

	if len(ret) == 0 {
		return nil, &NoResourceMatchError{PartialResource: resource}
	}

	sort.Sort(resourceByPreferredGroupVersion{ret, m.defaultGroupVersions})
	return ret, nil
}

func (m *DefaultRESTMapper) ResourcesForWithContext(_ context.Context, input schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return m.ResourcesFor(input)
}

func (m *DefaultRESTMapper) ResourceFor(resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	resources, err := m.ResourcesFor(resource)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	if len(resources) == 1 {
		return resources[0], nil
	}

	return schema.GroupVersionResource{}, &AmbiguousResourceError{PartialResource: resource, MatchingResources: resources}
}

func (m *DefaultRESTMapper) ResourceForWithContext(_ context.Context, resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return m.ResourceFor(resource)
}

func (m *DefaultRESTMapper) KindsFor(input schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	resource := coerceResourceForMatching(input)

	hasResource := len(resource.Resource) > 0
	hasGroup := len(resource.Group) > 0
	hasVersion := len(resource.Version) > 0

	if !hasResource {
		return nil, fmt.Errorf("a resource must be present, got: %v", resource)
	}

	ret := []schema.GroupVersionKind{}
	switch {
	// fully qualified.  Find the exact match
	case hasGroup && hasVersion:
		kind, exists := m.resourceToKind[resource]
		if exists {
			ret = append(ret, kind)
		}

	case hasGroup:
		foundExactMatch := false
		requestedGroupResource := resource.GroupResource()
		for currResource, currKind := range m.resourceToKind {
			if currResource.GroupResource() == requestedGroupResource {
				foundExactMatch = true
				ret = append(ret, currKind)
			}
		}

		// if you didn't find an exact match, match on group prefixing. This allows storageclass.storage to match
		// storageclass.storage.k8s.io
		if !foundExactMatch {
			for currResource, currKind := range m.resourceToKind {
				if !strings.HasPrefix(currResource.Group, requestedGroupResource.Group) {
					continue
				}
				if currResource.Resource == requestedGroupResource.Resource {
					ret = append(ret, currKind)
				}
			}

		}

	case hasVersion:
		for currResource, currKind := range m.resourceToKind {
			if currResource.Version == resource.Version && currResource.Resource == resource.Resource {
				ret = append(ret, currKind)
			}
		}

	default:
		for currResource, currKind := range m.resourceToKind {
			if currResource.Resource == resource.Resource {
				ret = append(ret, currKind)
			}
		}
	}

	if len(ret) == 0 {
		return nil, &NoResourceMatchError{PartialResource: input}
	}

	sort.Sort(kindByPreferredGroupVersion{ret, m.defaultGroupVersions})
	return ret, nil
}

func (m *DefaultRESTMapper) KindsForWithContext(_ context.Context, input schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return m.KindsFor(input)
}

func (m *DefaultRESTMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	kinds, err := m.KindsFor(resource)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	if len(kinds) == 1 {
		return kinds[0], nil
	}

	return schema.GroupVersionKind{}, &AmbiguousResourceError{PartialResource: resource, MatchingKinds: kinds}
}

func (m *DefaultRESTMapper) KindForWithContext(_ context.Context, resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return m.KindFor(resource)
}

type kindByPreferredGroupVersion struct {
	list      []schema.GroupVersionKind
	sortOrder []schema.GroupVersion
}

func (o kindByPreferredGroupVersion) Len() int      { return len(o.list) }
func (o kindByPreferredGroupVersion) Swap(i, j int) { o.list[i], o.list[j] = o.list[j], o.list[i] }
func (o kindByPreferredGroupVersion) Less(i, j int) bool {
	lhs := o.list[i]
	rhs := o.list[j]
	if lhs == rhs {
		return false
	}

	if lhs.GroupVersion() == rhs.GroupVersion() {
		return lhs.Kind < rhs.Kind
	}

	// otherwise, the difference is in the GroupVersion, so we need to sort with respect to the preferred order
	lhsIndex := -1
	rhsIndex := -1

	for i := range o.sortOrder {
		if o.sortOrder[i] == lhs.GroupVersion() {
			lhsIndex = i
		}
		if o.sortOrder[i] == rhs.GroupVersion() {
			rhsIndex = i
		}
	}

	if rhsIndex == -1 {
		return true
	}

	return lhsIndex < rhsIndex
}

type resourceByPreferredGroupVersion struct {
	list      []schema.GroupVersionResource
	sortOrder []schema.GroupVersion
}

func (o resourceByPreferredGroupVersion) Len() int      { return len(o.list) }
func (o resourceByPreferredGroupVersion) Swap(i, j int) { o.list[i], o.list[j] = o.list[j], o.list[i] }
func (o resourceByPreferredGroupVersion) Less(i, j int) bool {
	lhs := o.list[i]
	rhs := o.list[j]
	if lhs == rhs {
		return false
	}

	if lhs.GroupVersion() == rhs.GroupVersion() {
		return lhs.Resource < rhs.Resource
	}

	// otherwise, the difference is in the GroupVersion, so we need to sort with respect to the preferred order
	lhsIndex := -1
	rhsIndex := -1

	for i := range o.sortOrder {
		if o.sortOrder[i] == lhs.GroupVersion() {
			lhsIndex = i
		}
		if o.sortOrder[i] == rhs.GroupVersion() {
			rhsIndex = i
		}
	}

	if rhsIndex == -1 {
		return true
	}

	return lhsIndex < rhsIndex
}

// RESTMapping returns a struct representing the resource path and conversion interfaces a
// RESTClient should use to operate on the provided group/kind in order of versions. If a version search
// order is not provided, the search order provided to DefaultRESTMapper will be used to resolve which
// version should be used to access the named group/kind.
func (m *DefaultRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	mappings, err := m.RESTMappings(gk, versions...)
	if err != nil {
		return nil, err
	}
	if len(mappings) == 0 {
		return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	// since we rely on RESTMappings method
	// take the first match and return to the caller
	// as this was the existing behavior.
	return mappings[0], nil
}

func (m *DefaultRESTMapper) RESTMappingWithContext(_ context.Context, gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	return m.RESTMapping(gk, versions...)
}

// RESTMappings returns the RESTMappings for the provided group kind. If a version search order
// is not provided, the search order provided to DefaultRESTMapper will be used.
func (m *DefaultRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	mappings := make([]*RESTMapping, 0)
	potentialGVK := make([]schema.GroupVersionKind, 0)
	hadVersion := false

	// Pick an appropriate version
	for _, version := range versions {
		if len(version) == 0 || version == runtime.APIVersionInternal {
			continue
		}
		currGVK := gk.WithVersion(version)
		hadVersion = true
		if _, ok := m.kindToPluralResource[currGVK]; ok {
			potentialGVK = append(potentialGVK, currGVK)
			break
		}
	}
	// Use the default preferred versions
	if !hadVersion && len(potentialGVK) == 0 {
		for _, gv := range m.defaultGroupVersions {
			if gv.Group != gk.Group {
				continue
			}
			potentialGVK = append(potentialGVK, gk.WithVersion(gv.Version))
		}
	}

	if len(potentialGVK) == 0 {
		return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}

	for _, gvk := range potentialGVK {
		//Ensure we have a REST mapping
		res, ok := m.kindToPluralResource[gvk]
		if !ok {
			continue
		}

		// Ensure we have a REST scope
		scope, ok := m.kindToScope[gvk]
		if !ok {
			return nil, fmt.Errorf("the provided version %q and kind %q cannot be mapped to a supported scope", gvk.GroupVersion(), gvk.Kind)
		}

		mappings = append(mappings, &RESTMapping{
			Resource:         res,
			GroupVersionKind: gvk,
			Scope:            scope,
		})
	}

	if len(mappings) == 0 {
		return nil, &NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: gk.Group, Resource: gk.Kind}}
	}
	return mappings, nil
}

func (m *DefaultRESTMapper) RESTMappingsWithContext(_ context.Context, gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	return m.RESTMappings(gk, versions...)
}

// MaybeResetRESTMapper calls Reset() on the mapper if it is a ResettableRESTMapper or
// ResetWithContext() if it is a ResettableRESTMapperWithContext.
//
// Deprecated: use MaybeResetRESTMapperWithContext instead.
func MaybeResetRESTMapper(mapper RESTMapper) {
	maybeReset(context.Background(), mapper)
}

// MaybeResetRESTMapperWithContext calls Reset() on the mapper if it is a ResettableRESTMapper or
// ResetWithContext() if it is a ResettableRESTMapperWithContext.
func MaybeResetRESTMapperWithContext(ctx context.Context, mapper RESTMapperWithContext) {
	maybeReset(ctx, mapper)
}

func maybeReset(ctx context.Context, mapper any) {
	if m, ok := mapper.(ResettableRESTMapperWithContext); ok {
		m.ResetWithContext(ctx)
		return
	}
	if m, ok := mapper.(ResettableRESTMapper); ok {
		m.Reset()
		return
	}
}
