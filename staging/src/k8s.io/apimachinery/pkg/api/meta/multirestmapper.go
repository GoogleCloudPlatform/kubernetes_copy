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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

var (
	_ ResettableRESTMapper            = MultiRESTMapper{}
	_ fmt.Stringer                    = MultiRESTMapper{}
	_ ResettableRESTMapperWithContext = MultiRESTMapperWithContext{}
	_ fmt.Stringer                    = MultiRESTMapperWithContext{}
)

// MultiRESTMapper is a wrapper for multiple RESTMappers.
//
// Deprecated: use MultiRESTMapperWithContext instead.
type MultiRESTMapper []RESTMapper

// MultiRESTMapperWithContext is a wrapper for multiple RESTMapperWithContext instances.
type MultiRESTMapperWithContext []RESTMapperWithContext

func (m MultiRESTMapper) String() string {
	return stringifyMapper("MultiRESTMapper", m)
}

func (m MultiRESTMapperWithContext) String() string {
	return stringifyMapper("MultiRESTMapperWithContext", m)
}

func stringifyMapper[T any](typeName string, m []T) string {
	nested := make([]string, 0, len(m))
	for _, t := range m {
		currString := fmt.Sprintf("%v", t)
		splitStrings := strings.Split(currString, "\n")
		nested = append(nested, strings.Join(splitStrings, "\n\t"))
	}

	return fmt.Sprintf("%s{\n\t%s\n}", typeName, strings.Join(nested, "\n\t"))
}

func ToMultiRESTMapperWithContext(m MultiRESTMapper) MultiRESTMapperWithContext {
	if m == nil {
		return nil
	}
	mc := make(MultiRESTMapperWithContext, len(m))
	for i, m := range m {
		mc[i] = ToRESTMapperWithContext(m)
	}
	return mc
}

// ResourceSingularizer converts a REST resource name from plural to singular (e.g., from pods to pod)
// This implementation supports multiple REST schemas and return the first match.
//
// Deprecated: use MultiRESTMapperWithContext.ResourceSingularizerWithContext instead.
func (m MultiRESTMapper) ResourceSingularizer(resource string) (singular string, err error) {
	return ToMultiRESTMapperWithContext(m).ResourceSingularizerWithContext(context.Background(), resource)
}

// Deprecated: use MultiRESTMapperWithContext.ResourcesForWithContext instead.
func (m MultiRESTMapper) ResourcesFor(resource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return ToMultiRESTMapperWithContext(m).ResourcesForWithContext(context.Background(), resource)
}

// Deprecated: use MultiRESTMapperWithContext.KindsForWithContext instead.
func (m MultiRESTMapper) KindsFor(resource schema.GroupVersionResource) (gvk []schema.GroupVersionKind, err error) {
	return ToMultiRESTMapperWithContext(m).KindsForWithContext(context.Background(), resource)
}

// Deprecated: use MultiRESTMapperWithContext.ResourceForWithContext instead.
func (m MultiRESTMapper) ResourceFor(resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return ToMultiRESTMapperWithContext(m).ResourceForWithContext(context.Background(), resource)
}

// Deprecated: use MultiRESTMapperWithContext.KindForWithContext instead.
func (m MultiRESTMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return ToMultiRESTMapperWithContext(m).KindForWithContext(context.Background(), resource)
}

// RESTMapping provides the REST mapping for the resource based on the
// kind and version. This implementation supports multiple REST schemas and
// return the first match.
//
// Deprecated: use MultiRESTMapperWithContext.RESTMappingWithContext instead.
func (m MultiRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	return ToMultiRESTMapperWithContext(m).RESTMappingWithContext(context.Background(), gk, versions...)
}

// RESTMappings returns all possible RESTMappings for the provided group kind, or an error
// if the type is not recognized.
//
// Deprecated: use MultiRESTMapperWithContext.RESTMappingsWithContext instead.
func (m MultiRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	return ToMultiRESTMapperWithContext(m).RESTMappingsWithContext(context.Background(), gk, versions...)
}

// Deprecated: use MultiRESTMapperWithContext.Reset instead.
func (m MultiRESTMapper) Reset() {
	for _, t := range m {
		MaybeResetRESTMapper(t)
	}
}

// ResourceSingularizer converts a REST resource name from plural to singular (e.g., from pods to pod)
// This implementation supports multiple REST schemas and return the first match.
func (m MultiRESTMapperWithContext) ResourceSingularizerWithContext(ctx context.Context, resource string) (singular string, err error) {
	for _, t := range m {
		singular, err = t.ResourceSingularizerWithContext(ctx, resource)
		if err == nil {
			return
		}
	}
	return
}

func (m MultiRESTMapperWithContext) ResourcesForWithContext(ctx context.Context, resource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	allGVRs := []schema.GroupVersionResource{}
	for _, t := range m {
		gvrs, err := t.ResourcesForWithContext(ctx, resource)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}

		// walk the existing values to de-dup
		for _, curr := range gvrs {
			found := false
			for _, existing := range allGVRs {
				if curr == existing {
					found = true
					break
				}
			}

			if !found {
				allGVRs = append(allGVRs, curr)
			}
		}
	}

	if len(allGVRs) == 0 {
		return nil, &NoResourceMatchError{PartialResource: resource}
	}

	return allGVRs, nil
}

func (m MultiRESTMapperWithContext) KindsForWithContext(ctx context.Context, resource schema.GroupVersionResource) (gvk []schema.GroupVersionKind, err error) {
	allGVKs := []schema.GroupVersionKind{}
	for _, t := range m {
		gvks, err := t.KindsForWithContext(ctx, resource)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}

		// walk the existing values to de-dup
		for _, curr := range gvks {
			found := false
			for _, existing := range allGVKs {
				if curr == existing {
					found = true
					break
				}
			}

			if !found {
				allGVKs = append(allGVKs, curr)
			}
		}
	}

	if len(allGVKs) == 0 {
		return nil, &NoResourceMatchError{PartialResource: resource}
	}

	return allGVKs, nil
}

func (m MultiRESTMapperWithContext) ResourceForWithContext(ctx context.Context, resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	resources, err := m.ResourcesForWithContext(ctx, resource)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	if len(resources) == 1 {
		return resources[0], nil
	}

	return schema.GroupVersionResource{}, &AmbiguousResourceError{PartialResource: resource, MatchingResources: resources}
}

func (m MultiRESTMapperWithContext) KindForWithContext(ctx context.Context, resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	kinds, err := m.KindsForWithContext(ctx, resource)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	if len(kinds) == 1 {
		return kinds[0], nil
	}

	return schema.GroupVersionKind{}, &AmbiguousResourceError{PartialResource: resource, MatchingKinds: kinds}
}

// RESTMapping provides the REST mapping for the resource based on the
// kind and version. This implementation supports multiple REST schemas and
// return the first match.
func (m MultiRESTMapperWithContext) RESTMappingWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	allMappings := []*RESTMapping{}
	errors := []error{}

	for _, t := range m {
		currMapping, err := t.RESTMappingWithContext(ctx, gk, versions...)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			errors = append(errors, err)
			continue
		}

		allMappings = append(allMappings, currMapping)
	}

	// if we got exactly one mapping, then use it even if other requested failed
	if len(allMappings) == 1 {
		return allMappings[0], nil
	}
	if len(allMappings) > 1 {
		var kinds []schema.GroupVersionKind
		for _, m := range allMappings {
			kinds = append(kinds, m.GroupVersionKind)
		}
		return nil, &AmbiguousKindError{PartialKind: gk.WithVersion(""), MatchingKinds: kinds}
	}
	if len(errors) > 0 {
		return nil, utilerrors.NewAggregate(errors)
	}
	return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
}

// RESTMappings returns all possible RESTMappings for the provided group kind, or an error
// if the type is not recognized.
func (m MultiRESTMapperWithContext) RESTMappingsWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	var allMappings []*RESTMapping
	var errors []error

	for _, t := range m {
		currMappings, err := t.RESTMappingsWithContext(ctx, gk, versions...)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			errors = append(errors, err)
			continue
		}
		allMappings = append(allMappings, currMappings...)
	}
	if len(errors) > 0 {
		return nil, utilerrors.NewAggregate(errors)
	}
	if len(allMappings) == 0 {
		return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	return allMappings, nil
}

func (m MultiRESTMapperWithContext) ResetWithContext(ctx context.Context) {
	for _, t := range m {
		MaybeResetRESTMapperWithContext(ctx, t)
	}
}
