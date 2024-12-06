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

package restmapper

import (
	"context"
	"iter"
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// CategoryExpander maps category strings to GroupResources.
// Categories are classification or 'tag' of a group of resources.
//
// Deprecated: use CategoryExpanderWithContext instead.
type CategoryExpander interface {
	Expand(category string) ([]schema.GroupResource, bool)
}

// CategoryExpanderWithContext maps category strings to GroupResources.
// Categories are classification or 'tag' of a group of resources.
type CategoryExpanderWithContext interface {
	ExpandWithContext(ctx context.Context, category string) ([]schema.GroupResource, bool)
}

func ToCategoryExpanderWithContext(c CategoryExpander) CategoryExpanderWithContext {
	if c == nil {
		return nil
	}
	if c == nil {
		return nil
	}
	if c, ok := c.(CategoryExpanderWithContext); ok {
		return c
	}
	return &categoryExpanderWrapper{
		delegate: c,
	}
}

type categoryExpanderWrapper struct {
	delegate CategoryExpander
}

func (c *categoryExpanderWrapper) ExpandWithContext(ctx context.Context, category string) ([]schema.GroupResource, bool) {
	return c.delegate.Expand(category)
}

// SimpleCategoryExpander implements CategoryExpander and CategoryExpanderWithContext interface
// using a static mapping of categories to GroupResource mapping.
type SimpleCategoryExpander struct {
	Expansions map[string][]schema.GroupResource
}

var (
	_ CategoryExpander            = SimpleCategoryExpander{}
	_ CategoryExpanderWithContext = SimpleCategoryExpander{}
)

// Expand fulfills CategoryExpander
func (e SimpleCategoryExpander) Expand(category string) ([]schema.GroupResource, bool) {
	ret, ok := e.Expansions[category]
	return ret, ok
}

// Expand fulfills CategoryExpanderWithContext
func (e SimpleCategoryExpander) ExpandWithContext(ctx context.Context, category string) ([]schema.GroupResource, bool) {
	return e.Expand(category)
}

// discoveryCategoryExpander struct lets a REST Client wrapper (discoveryClient) to retrieve list of APIResourceList,
// and then convert to fallbackExpander
type discoveryCategoryExpander struct {
	discoveryClient discovery.DiscoveryInterfaceWithContext
}

// NewDiscoveryCategoryExpander returns a category expander that makes use of the "categories" fields from
// the API, found through the discovery client. In case of any error or no category found (which likely
// means we're at a cluster prior to categories support, fallback to the expander provided.
//
// Deprecated: use NewDiscoveryCategoryExpanderWithContext instead.
func NewDiscoveryCategoryExpander(client discovery.DiscoveryInterface) CategoryExpander {
	return newDiscoveryCategoryExpander(discovery.ToDiscoveryInterfaceWithContext(client))
}

// NewDiscoveryCategoryExpanderWithContext returns a category expander that makes use of the "categories" fields from
// the API, found through the discovery client. In case of any error or no category found (which likely
// means we're at a cluster prior to categories support, fallback to the expander provided.
func NewDiscoveryCategoryExpanderWithContext(client discovery.DiscoveryInterfaceWithContext) CategoryExpanderWithContext {
	return newDiscoveryCategoryExpander(client)
}

func newDiscoveryCategoryExpander(client discovery.DiscoveryInterfaceWithContext) discoveryCategoryExpander {
	if client == nil {
		panic("Please provide discovery client to shortcut expander")
	}
	return discoveryCategoryExpander{discoveryClient: client}
}

// Expand fulfills CategoryExpander
//
// Deprecated: use ExpandWithContext instead.
func (e discoveryCategoryExpander) Expand(category string) ([]schema.GroupResource, bool) {
	return e.ExpandWithContext(context.Background(), category)
}

// ExpandWithContext fulfills CategoryExpanderWithContext
func (e discoveryCategoryExpander) ExpandWithContext(ctx context.Context, category string) ([]schema.GroupResource, bool) {
	// Get all supported resources for groups and versions from server, if no resource found, fallback anyway.
	_, apiResourceLists, _ := e.discoveryClient.ServerGroupsAndResourcesWithContext(ctx)
	if len(apiResourceLists) == 0 {
		return nil, false
	}

	discoveredExpansions := map[string][]schema.GroupResource{}
	for _, apiResourceList := range apiResourceLists {
		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			continue
		}
		// Collect GroupVersions by categories
		for _, apiResource := range apiResourceList.APIResources {
			if categories := apiResource.Categories; len(categories) > 0 {
				for _, category := range categories {
					groupResource := schema.GroupResource{
						Group:    gv.Group,
						Resource: apiResource.Name,
					}
					discoveredExpansions[category] = append(discoveredExpansions[category], groupResource)
				}
			}
		}
	}

	ret, ok := discoveredExpansions[category]
	return ret, ok
}

// UnionCategoryExpander implements CategoryExpander interface.
// It maps given category string to union of expansions returned by all the CategoryExpanders in the list.
//
// Deprecated: use UnionCategoryWithContext instead.
type UnionCategoryExpander []CategoryExpander

var _ CategoryExpander = UnionCategoryExpander{}

// Expand fulfills CategoryExpander
func (u UnionCategoryExpander) Expand(category string) ([]schema.GroupResource, bool) {
	return unionExpand(context.Background(), func(yield func(i int, expander CategoryExpanderWithContext) bool) {
		for i, expander := range u {
			if !yield(i, ToCategoryExpanderWithContext(expander)) {
				break
			}
		}
	}, category)
}

// UnionCategoryExpanderWithContext implements CategoryExpanderWithContext interface.
// It maps given category string to union of expansions returned by all the CategoryExpanders in the list.
type UnionCategoryExpanderWithContext []CategoryExpanderWithContext

var _ CategoryExpanderWithContext = UnionCategoryExpanderWithContext{}

// ExpandWithContext fulfills CategoryExpanderWithContext
func (u UnionCategoryExpanderWithContext) ExpandWithContext(ctx context.Context, category string) ([]schema.GroupResource, bool) {
	return unionExpand(ctx, slices.All(u), category)
}

func unionExpand(ctx context.Context, expanders iter.Seq2[int, CategoryExpanderWithContext], category string) ([]schema.GroupResource, bool) {
	ret := []schema.GroupResource{}
	ok := false

	// Expand the category for each CategoryExpander in the list and merge/combine the results.
	for _, expansion := range expanders {
		curr, currOk := expansion.ExpandWithContext(ctx, category)

		for _, currGR := range curr {
			found := false
			for _, existing := range ret {
				if existing == currGR {
					found = true
					break
				}
			}
			if !found {
				ret = append(ret, currGR)
			}
		}
		ok = ok || currOk
	}

	return ret, ok
}
