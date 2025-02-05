/*
Copyright 2016 The Kubernetes Authors.

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

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	AnyGroup    = "*"
	AnyVersion  = "*"
	AnyResource = "*"
	AnyKind     = "*"
)

var (
	_ ResettableRESTMapper            = PriorityRESTMapper{}
	_ ResettableRESTMapperWithContext = PriorityRESTMapperWithContext{}
)

// PriorityRESTMapper is a wrapper for automatically choosing a particular Resource or Kind
// when multiple matches are possible
//
// Deprecated: use PriorityRESTMapperWithContext instead.
type PriorityRESTMapper struct {
	// Delegate is the RESTMapper to use to locate all the Kind and Resource matches
	Delegate RESTMapper

	// ResourcePriority is a list of priority patterns to apply to matching resources.
	// The list of all matching resources is narrowed based on the patterns until only one remains.
	// A pattern with no matches is skipped.  A pattern with more than one match uses its
	// matches as the list to continue matching against.
	ResourcePriority []schema.GroupVersionResource

	// KindPriority is a list of priority patterns to apply to matching kinds.
	// The list of all matching kinds is narrowed based on the patterns until only one remains.
	// A pattern with no matches is skipped.  A pattern with more than one match uses its
	// matches as the list to continue matching against.
	KindPriority []schema.GroupVersionKind
}

func (m PriorityRESTMapper) String() string {
	return fmt.Sprintf("PriorityRESTMapper{\n\t%v\n\t%v\n\t%v\n}", m.ResourcePriority, m.KindPriority, m.Delegate)
}

// PriorityRESTMapperWithContext is a wrapper for automatically choosing a particular Resource or Kind
// when multiple matches are possible
type PriorityRESTMapperWithContext struct {
	// Delegate is the RESTMapperWithContext to use to locate all the Kind and Resource matches
	Delegate RESTMapperWithContext

	// ResourcePriority is a list of priority patterns to apply to matching resources.
	// The list of all matching resources is narrowed based on the patterns until only one remains.
	// A pattern with no matches is skipped.  A pattern with more than one match uses its
	// matches as the list to continue matching against.
	ResourcePriority []schema.GroupVersionResource

	// KindPriority is a list of priority patterns to apply to matching kinds.
	// The list of all matching kinds is narrowed based on the patterns until only one remains.
	// A pattern with no matches is skipped.  A pattern with more than one match uses its
	// matches as the list to continue matching against.
	KindPriority []schema.GroupVersionKind
}

func (m PriorityRESTMapperWithContext) String() string {
	return fmt.Sprintf("PriorityRESTMapperWithContext{\n\t%v\n\t%v\n\t%v\n}", m.ResourcePriority, m.KindPriority, m.Delegate)
}

func ToPriorityRESTMapperWithContext(m PriorityRESTMapper) PriorityRESTMapperWithContext {
	return PriorityRESTMapperWithContext{
		Delegate:         ToRESTMapperWithContext(m.Delegate),
		ResourcePriority: m.ResourcePriority,
		KindPriority:     m.KindPriority,
	}
}

// ResourceFor finds all resources, then passes them through the ResourcePriority patterns to find a single matching hit.
//
// Deprecated: use PriorityRESTMapperWithContext.ResourceForWithContext instead.
func (m PriorityRESTMapper) ResourceFor(partiallySpecifiedResource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return ToPriorityRESTMapperWithContext(m).ResourceForWithContext(context.Background(), partiallySpecifiedResource)
}

// KindFor finds all kinds, then passes them through the KindPriority patterns to find a single matching hit.
//
// Deprecated: use PriorityRESTMapperWithContext.KindForWithContext instead.
func (m PriorityRESTMapper) KindFor(partiallySpecifiedResource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return ToPriorityRESTMapperWithContext(m).KindForWithContext(context.Background(), partiallySpecifiedResource)
}

// Deprecated: use PriorityRESTMapperWithContext.RESTMappingWithContext instead.
func (m PriorityRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (mapping *RESTMapping, err error) {
	return ToPriorityRESTMapperWithContext(m).RESTMappingWithContext(context.Background(), gk, versions...)
}

// Deprecated: use PriorityRESTMapperWithContext.RESTMappingsWithContext instead.
func (m PriorityRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	return m.Delegate.RESTMappings(gk, versions...)
}

// Deprecated: use PriorityRESTMapperWithContext.ResourceSingularizerWithContext instead.
func (m PriorityRESTMapper) ResourceSingularizer(resource string) (singular string, err error) {
	return m.Delegate.ResourceSingularizer(resource)
}

// Deprecated: use PriorityRESTMapperWithContext.ResourcesForWithContext instead.
func (m PriorityRESTMapper) ResourcesFor(partiallySpecifiedResource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return m.Delegate.ResourcesFor(partiallySpecifiedResource)
}

// Deprecated: use PriorityRESTMapperWithContext.KindsForWithContext instead.
func (m PriorityRESTMapper) KindsFor(partiallySpecifiedResource schema.GroupVersionResource) (gvk []schema.GroupVersionKind, err error) {
	return m.Delegate.KindsFor(partiallySpecifiedResource)
}

// Deprecated: use PriorityRESTMapperWithContext.ResetWithContext instead.
func (m PriorityRESTMapper) Reset() {
	MaybeResetRESTMapper(m.Delegate)
}

// ResourceFor finds all resources, then passes them through the ResourcePriority patterns to find a single matching hit.
func (m PriorityRESTMapperWithContext) ResourceForWithContext(ctx context.Context, partiallySpecifiedResource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	originalGVRs, originalErr := m.Delegate.ResourcesForWithContext(ctx, partiallySpecifiedResource)
	if originalErr != nil && len(originalGVRs) == 0 {
		return schema.GroupVersionResource{}, originalErr
	}
	if len(originalGVRs) == 1 {
		return originalGVRs[0], originalErr
	}

	remainingGVRs := append([]schema.GroupVersionResource{}, originalGVRs...)
	for _, pattern := range m.ResourcePriority {
		matchedGVRs := []schema.GroupVersionResource{}
		for _, gvr := range remainingGVRs {
			if resourceMatches(pattern, gvr) {
				matchedGVRs = append(matchedGVRs, gvr)
			}
		}

		switch len(matchedGVRs) {
		case 0:
			// if you have no matches, then nothing matched this pattern just move to the next
			continue
		case 1:
			// one match, return
			return matchedGVRs[0], originalErr
		default:
			// more than one match, use the matched hits as the list moving to the next pattern.
			// this way you can have a series of selection criteria
			remainingGVRs = matchedGVRs
		}
	}

	return schema.GroupVersionResource{}, &AmbiguousResourceError{PartialResource: partiallySpecifiedResource, MatchingResources: originalGVRs}
}

// KindFor finds all kinds, then passes them through the KindPriority patterns to find a single matching hit.
func (m PriorityRESTMapperWithContext) KindForWithContext(ctx context.Context, partiallySpecifiedResource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	originalGVKs, originalErr := m.Delegate.KindsForWithContext(ctx, partiallySpecifiedResource)
	if originalErr != nil && len(originalGVKs) == 0 {
		return schema.GroupVersionKind{}, originalErr
	}
	if len(originalGVKs) == 1 {
		return originalGVKs[0], originalErr
	}

	remainingGVKs := append([]schema.GroupVersionKind{}, originalGVKs...)
	for _, pattern := range m.KindPriority {
		matchedGVKs := []schema.GroupVersionKind{}
		for _, gvr := range remainingGVKs {
			if kindMatches(pattern, gvr) {
				matchedGVKs = append(matchedGVKs, gvr)
			}
		}

		switch len(matchedGVKs) {
		case 0:
			// if you have no matches, then nothing matched this pattern just move to the next
			continue
		case 1:
			// one match, return
			return matchedGVKs[0], originalErr
		default:
			// more than one match, use the matched hits as the list moving to the next pattern.
			// this way you can have a series of selection criteria
			remainingGVKs = matchedGVKs
		}
	}

	return schema.GroupVersionKind{}, &AmbiguousResourceError{PartialResource: partiallySpecifiedResource, MatchingKinds: originalGVKs}
}

func resourceMatches(pattern schema.GroupVersionResource, resource schema.GroupVersionResource) bool {
	if pattern.Group != AnyGroup && pattern.Group != resource.Group {
		return false
	}
	if pattern.Version != AnyVersion && pattern.Version != resource.Version {
		return false
	}
	if pattern.Resource != AnyResource && pattern.Resource != resource.Resource {
		return false
	}

	return true
}

func kindMatches(pattern schema.GroupVersionKind, kind schema.GroupVersionKind) bool {
	if pattern.Group != AnyGroup && pattern.Group != kind.Group {
		return false
	}
	if pattern.Version != AnyVersion && pattern.Version != kind.Version {
		return false
	}
	if pattern.Kind != AnyKind && pattern.Kind != kind.Kind {
		return false
	}

	return true
}

func (m PriorityRESTMapperWithContext) RESTMappingWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) (mapping *RESTMapping, err error) {
	mappings, originalErr := m.Delegate.RESTMappingsWithContext(ctx, gk, versions...)
	if originalErr != nil && len(mappings) == 0 {
		return nil, originalErr
	}

	// any versions the user provides take priority
	priorities := m.KindPriority
	if len(versions) > 0 {
		priorities = make([]schema.GroupVersionKind, 0, len(m.KindPriority)+len(versions))
		for _, version := range versions {
			gv := schema.GroupVersion{
				Version: version,
				Group:   gk.Group,
			}
			priorities = append(priorities, gv.WithKind(AnyKind))
		}
		priorities = append(priorities, m.KindPriority...)
	}

	remaining := append([]*RESTMapping{}, mappings...)
	for _, pattern := range priorities {
		var matching []*RESTMapping
		for _, m := range remaining {
			if kindMatches(pattern, m.GroupVersionKind) {
				matching = append(matching, m)
			}
		}

		switch len(matching) {
		case 0:
			// if you have no matches, then nothing matched this pattern just move to the next
			continue
		case 1:
			// one match, return
			return matching[0], originalErr
		default:
			// more than one match, use the matched hits as the list moving to the next pattern.
			// this way you can have a series of selection criteria
			remaining = matching
		}
	}
	if len(remaining) == 1 {
		return remaining[0], originalErr
	}

	var kinds []schema.GroupVersionKind
	for _, m := range mappings {
		kinds = append(kinds, m.GroupVersionKind)
	}
	return nil, &AmbiguousKindError{PartialKind: gk.WithVersion(""), MatchingKinds: kinds}
}

func (m PriorityRESTMapperWithContext) RESTMappingsWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	return m.Delegate.RESTMappingsWithContext(ctx, gk, versions...)
}

func (m PriorityRESTMapperWithContext) ResourceSingularizerWithContext(ctx context.Context, resource string) (singular string, err error) {
	return m.Delegate.ResourceSingularizerWithContext(ctx, resource)
}

func (m PriorityRESTMapperWithContext) ResourcesForWithContext(ctx context.Context, partiallySpecifiedResource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return m.Delegate.ResourcesForWithContext(ctx, partiallySpecifiedResource)
}

func (m PriorityRESTMapperWithContext) KindsForWithContext(ctx context.Context, partiallySpecifiedResource schema.GroupVersionResource) (gvk []schema.GroupVersionKind, err error) {
	return m.Delegate.KindsForWithContext(ctx, partiallySpecifiedResource)
}

func (m PriorityRESTMapperWithContext) ResetWithContext(ctx context.Context) {
	MaybeResetRESTMapperWithContext(ctx, m.Delegate)
}
