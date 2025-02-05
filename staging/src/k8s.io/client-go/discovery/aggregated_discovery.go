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

package discovery

import (
	"fmt"

	apidiscovery "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// StaleGroupVersionError encasulates failed GroupVersion marked "stale"
// in the returned AggregatedDiscovery format.
type StaleGroupVersionError struct {
	gv schema.GroupVersion
}

func (s StaleGroupVersionError) Error() string {
	return fmt.Sprintf("stale GroupVersion discovery: %v", s.gv)
}

// SplitGroupsAndResources transforms "aggregated" discovery top-level structure into
// the previous "unaggregated" discovery groups and resources.
func SplitGroupsAndResources(aggregatedGroups apidiscovery.APIGroupDiscoveryList) (
	*metav1.APIGroupList,
	map[schema.GroupVersion]*metav1.APIResourceList,
	map[schema.GroupVersion]error) {
	// Aggregated group list will contain the entirety of discovery, including
	// groups, versions, and resources. GroupVersions marked "stale" are failed.
	groups := []*metav1.APIGroup{}
	failedGVs := map[schema.GroupVersion]error{}
	resourcesByGV := map[schema.GroupVersion]*metav1.APIResourceList{}
	for _, aggGroup := range aggregatedGroups.Items {
		group, resources, failed := convertAPIGroup(aggGroup)
		groups = append(groups, group)
		for gv, resourceList := range resources {
			resourcesByGV[gv] = resourceList
		}
		for gv, err := range failed {
			failedGVs[gv] = err
		}
	}
	// Transform slice of groups to group list before returning.
	groupList := &metav1.APIGroupList{}
	groupList.Groups = make([]metav1.APIGroup, 0, len(groups))
	for _, group := range groups {
		groupList.Groups = append(groupList.Groups, *group)
	}
	return groupList, resourcesByGV, failedGVs
}

// convertAPIGroup tranforms an "aggregated" APIGroupDiscovery to an "legacy" APIGroup,
// also returning the map of APIResourceList for resources within GroupVersions.
func convertAPIGroup(g apidiscovery.APIGroupDiscovery) (
	*metav1.APIGroup,
	map[schema.GroupVersion]*metav1.APIResourceList,
	map[schema.GroupVersion]error) {
	// Iterate through versions to convert to group and resources.
	group := &metav1.APIGroup{}
	gvResources := map[schema.GroupVersion]*metav1.APIResourceList{}
	failedGVs := map[schema.GroupVersion]error{}
	group.Name = g.ObjectMeta.Name
	for _, v := range g.Versions {
		gv := schema.GroupVersion{Group: g.Name, Version: v.Version}
		if v.Freshness == apidiscovery.DiscoveryFreshnessStale {
			failedGVs[gv] = StaleGroupVersionError{gv: gv}
			continue
		}
		version := metav1.GroupVersionForDiscovery{}
		version.GroupVersion = gv.String()
		version.Version = v.Version
		group.Versions = append(group.Versions, version)
		// PreferredVersion is first non-stale Version
		if group.PreferredVersion == (metav1.GroupVersionForDiscovery{}) {
			group.PreferredVersion = version
		}
		resourceList := &metav1.APIResourceList{}
		resourceList.GroupVersion = gv.String()
		for _, r := range v.Resources {
			resource, err := convertAPIResource(r)
			if err == nil {
				resourceList.APIResources = append(resourceList.APIResources, resource)
			}
			// Subresources field in new format get transformed into full APIResources.
			// It is possible a partial result with an error was returned to be used
			// as the parent resource for the subresource.
			for _, subresource := range r.Subresources {
				sr, err := convertAPISubresource(resource, subresource)
				if err == nil {
					resourceList.APIResources = append(resourceList.APIResources, sr)
				}
			}
		}
		gvResources[gv] = resourceList
	}
	return group, gvResources, failedGVs
}

var emptyKind = metav1.GroupVersionKind{}

// convertAPIResource tranforms a APIResourceDiscovery to an APIResource. We are
// resilient to missing GVK, since this resource might be the parent resource
// for a subresource. If the parent is missing a GVK, it is not returned in
// discovery, and the subresource MUST have the GVK.
func convertAPIResource(in apidiscovery.APIResourceDiscovery) (metav1.APIResource, error) {
	result := metav1.APIResource{
		Name:         in.Resource,
		SingularName: in.SingularResource,
		Namespaced:   in.Scope == apidiscovery.ScopeNamespace,
		Verbs:        in.Verbs,
		ShortNames:   in.ShortNames,
		Categories:   in.Categories,
	}
	var err error
	if in.ResponseKind != nil && (*in.ResponseKind) != emptyKind {
		result.Group = in.ResponseKind.Group
		result.Version = in.ResponseKind.Version
		result.Kind = in.ResponseKind.Kind
	} else {
		err = fmt.Errorf("discovery resource %s missing GVK", in.Resource)
	}
	// Can return partial result with error, which can be the parent for a
	// subresource. Do not add this result to the returned discovery resources.
	return result, err
}

// convertAPISubresource tranforms a APISubresourceDiscovery to an APIResource.
func convertAPISubresource(parent metav1.APIResource, in apidiscovery.APISubresourceDiscovery) (metav1.APIResource, error) {
	result := metav1.APIResource{}
	if in.ResponseKind == nil || (*in.ResponseKind) == emptyKind {
		return result, fmt.Errorf("subresource %s/%s missing GVK", parent.Name, in.Subresource)
	}
	result.Name = fmt.Sprintf("%s/%s", parent.Name, in.Subresource)
	result.SingularName = parent.SingularName
	result.Namespaced = parent.Namespaced
	result.Group = in.ResponseKind.Group
	result.Version = in.ResponseKind.Version
	result.Kind = in.ResponseKind.Kind
	result.Verbs = in.Verbs
	return result, nil
}
