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

package restmapper

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// shortcutExpander is a RESTMapper that can be used for Kubernetes resources.   It expands the resource first, then invokes the wrapped
type shortcutExpander struct {
	RESTMapper meta.RESTMapperWithContext

	discoveryClient discovery.DiscoveryInterfaceWithContext

	warningHandler func(string)
}

var _ meta.ResettableRESTMapper = shortcutExpander{}
var _ meta.ResettableRESTMapperWithContext = shortcutExpander{}

// NewShortcutExpander wraps a restmapper in a layer that expands shortcuts found via discovery
//
// Deprecated: use NewShortcutExpanderWithContext instead.
func NewShortcutExpander(delegate meta.RESTMapper, client discovery.DiscoveryInterface, warningHandler func(string)) meta.RESTMapper {
	return newShortcutExpander(meta.ToRESTMapperWithContext(delegate), discovery.ToDiscoveryInterfaceWithContext(client), warningHandler)
}

// NewShortcutExpanderWithContext wraps a restmapper in a layer that expands shortcuts found via discovery
func NewShortcutExpanderWithContext(delegate meta.RESTMapperWithContext, client discovery.DiscoveryInterfaceWithContext, warningHandler func(string)) meta.RESTMapperWithContext {
	return newShortcutExpander(delegate, client, warningHandler)
}

func newShortcutExpander(delegate meta.RESTMapperWithContext, client discovery.DiscoveryInterfaceWithContext, warningHandler func(string)) shortcutExpander {
	return shortcutExpander{RESTMapper: delegate, discoveryClient: client, warningHandler: warningHandler}
}

// KindFor fulfills meta.RESTMapper
func (e shortcutExpander) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return e.KindForWithContext(context.Background(), resource)
}

// KindForWithContext fulfills meta.RESTMapperWithContext
func (e shortcutExpander) KindForWithContext(ctx context.Context, resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	// expandResourceShortcut works with current API resources as read from discovery cache.
	// In case of new CRDs this means we potentially don't have current state of discovery.
	// In the current wiring in k8s.io/cli-runtime/pkg/genericclioptions/config_flags.go#toRESTMapper,
	// we are using DeferredDiscoveryRESTMapper which on KindFor failure will clear the
	// cache and fetch all data from a cluster (see k8s.io/client-go/restmapper/discovery.go#KindFor).
	// Thus another call to expandResourceShortcut, after a NoMatchError should successfully
	// read Kind to the user or an error.
	gvk, err := e.RESTMapper.KindForWithContext(ctx, e.expandResourceShortcut(ctx, resource))
	if meta.IsNoMatchError(err) {
		return e.RESTMapper.KindForWithContext(ctx, e.expandResourceShortcut(ctx, resource))
	}
	return gvk, err
}

// KindsFor fulfills meta.RESTMapper
//
// Deprecated: use KindsForWithContext instead.
func (e shortcutExpander) KindsFor(resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return e.RESTMapper.KindsForWithContext(context.Background(), resource)
}

// KindsForWithContext fulfills meta.RESTMapperWithContext
func (e shortcutExpander) KindsForWithContext(ctx context.Context, resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return e.RESTMapper.KindsForWithContext(ctx, e.expandResourceShortcut(ctx, resource))
}

// ResourcesFor fulfills meta.RESTMapper
//
// Deprecated: use ResourcesForWithContext instead.
func (e shortcutExpander) ResourcesFor(resource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return e.ResourcesForWithContext(context.Background(), resource)
}

// ResourcesForWithContext fulfills meta.RESTMapperWithContext
func (e shortcutExpander) ResourcesForWithContext(ctx context.Context, resource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return e.RESTMapper.ResourcesForWithContext(ctx, e.expandResourceShortcut(ctx, resource))
}

// ResourceFor fulfills meta.RESTMapper
//
// Deprecated: use ResourceForWithContext instead.
func (e shortcutExpander) ResourceFor(resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return e.ResourceForWithContext(context.Background(), resource)
}

// ResourceForWithContext fulfills meta.RESTMapperWithContext
func (e shortcutExpander) ResourceForWithContext(ctx context.Context, resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return e.RESTMapper.ResourceForWithContext(ctx, e.expandResourceShortcut(ctx, resource))
}

// ResourceSingularizer fulfills meta.RESTMapper
//
// Deprecated: use ResourceSingularizerWithContext instead.
func (e shortcutExpander) ResourceSingularizer(resource string) (string, error) {
	return e.ResourceSingularizerWithContext(context.Background(), resource)
}

// ResourceSingularizerWithContext fulfills meta.RESTMapperWithContext
func (e shortcutExpander) ResourceSingularizerWithContext(ctx context.Context, resource string) (string, error) {
	return e.RESTMapper.ResourceSingularizerWithContext(ctx, e.expandResourceShortcut(ctx, schema.GroupVersionResource{Resource: resource}).Resource)
}

// RESTMapping fulfills meta.RESTMapper
//
// Deprecated: use RESTMappingWithContext instead.
func (e shortcutExpander) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return e.RESTMappingWithContext(context.Background(), gk, versions...)
}

// RESTMappingWithContext fulfills meta.RESTMapperWithContext
func (e shortcutExpander) RESTMappingWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return e.RESTMapper.RESTMappingWithContext(ctx, gk, versions...)
}

// RESTMappings fulfills meta.RESTMapper
//
// Deprecated: use KindForWithContext instead.
func (e shortcutExpander) RESTMappings(gk schema.GroupKind, versions ...string) ([]*meta.RESTMapping, error) {
	return e.RESTMappingsWithContext(context.Background(), gk, versions...)
}

// RESTMappingsWithContext fulfills meta.RESTMapperWithContext
func (e shortcutExpander) RESTMappingsWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) ([]*meta.RESTMapping, error) {
	return e.RESTMapper.RESTMappingsWithContext(ctx, gk, versions...)
}

// getShortcutMappings returns a set of tuples which holds short names for resources.
// First the list of potential resources will be taken from the API server.
// Next we will append the hardcoded list of resources - to be backward compatible with old servers.
// NOTE that the list is ordered by group priority.
func (e shortcutExpander) getShortcutMappings(ctx context.Context) ([]*metav1.APIResourceList, []resourceShortcuts, error) {
	res := []resourceShortcuts{}
	// get server resources
	// This can return an error *and* the results it was able to find.  We don't need to fail on the error.
	_, apiResList, err := e.discoveryClient.ServerGroupsAndResourcesWithContext(ctx)
	if err != nil {
		klog.FromContext(ctx).V(1).Info("Error loading discovery information", "err", err)
	}
	for _, apiResources := range apiResList {
		gv, err := schema.ParseGroupVersion(apiResources.GroupVersion)
		if err != nil {
			klog.FromContext(ctx).V(1).Info("Unable to parse group/version", "groupVersion", apiResources.GroupVersion, "err", err.Error())
			continue
		}
		for _, apiRes := range apiResources.APIResources {
			for _, shortName := range apiRes.ShortNames {
				rs := resourceShortcuts{
					ShortForm: schema.GroupResource{Group: gv.Group, Resource: shortName},
					LongForm:  schema.GroupResource{Group: gv.Group, Resource: apiRes.Name},
				}
				res = append(res, rs)
			}
		}
	}

	return apiResList, res, nil
}

// expandResourceShortcut will return the expanded version of resource
// (something that a pkg/api/meta.RESTMapper can understand), if it is
// indeed a shortcut. If no match has been found, we will match on group prefixing.
// Lastly we will return resource unmodified.
func (e shortcutExpander) expandResourceShortcut(ctx context.Context, resource schema.GroupVersionResource) schema.GroupVersionResource {
	// get the shortcut mappings and return on first match.
	if allResources, shortcutResources, err := e.getShortcutMappings(ctx); err == nil {
		// avoid expanding if there's an exact match to a full resource name
		for _, apiResources := range allResources {
			gv, err := schema.ParseGroupVersion(apiResources.GroupVersion)
			if err != nil {
				continue
			}
			if len(resource.Group) != 0 && resource.Group != gv.Group {
				continue
			}
			for _, apiRes := range apiResources.APIResources {
				if resource.Resource == apiRes.Name {
					return resource
				}
				if resource.Resource == apiRes.SingularName {
					return resource
				}
			}
		}

		found := false
		var rsc schema.GroupVersionResource
		warnedAmbiguousShortcut := make(map[schema.GroupResource]bool)
		for _, item := range shortcutResources {
			if len(resource.Group) != 0 && resource.Group != item.ShortForm.Group {
				continue
			}
			if resource.Resource == item.ShortForm.Resource {
				if found {
					if item.LongForm.Group == rsc.Group && item.LongForm.Resource == rsc.Resource {
						// It is common and acceptable that group/resource has multiple
						// versions registered in cluster. This does not introduce ambiguity
						// in terms of shortname usage.
						continue
					}
					if !warnedAmbiguousShortcut[item.LongForm] {
						if e.warningHandler != nil {
							e.warningHandler(fmt.Sprintf("short name %q could also match lower priority resource %s", resource.Resource, item.LongForm.String()))
						}
						warnedAmbiguousShortcut[item.LongForm] = true
					}
					continue
				}
				rsc.Resource = item.LongForm.Resource
				rsc.Group = item.LongForm.Group
				found = true
			}
		}
		if found {
			return rsc
		}

		// we didn't find exact match so match on group prefixing. This allows autoscal to match autoscaling
		if len(resource.Group) == 0 {
			return resource
		}
		for _, item := range shortcutResources {
			if !strings.HasPrefix(item.ShortForm.Group, resource.Group) {
				continue
			}
			if resource.Resource == item.ShortForm.Resource {
				resource.Resource = item.LongForm.Resource
				resource.Group = item.LongForm.Group
				return resource
			}
		}
	}

	return resource
}

// Deprecated: use ResetWithContext instead.
func (e shortcutExpander) Reset() {
	e.ResetWithContext(context.Background())
}

func (e shortcutExpander) ResetWithContext(ctx context.Context) {
	meta.MaybeResetRESTMapperWithContext(ctx, e.RESTMapper)
}

// ResourceShortcuts represents a structure that holds the information how to
// transition from resource's shortcut to its full name.
type resourceShortcuts struct {
	ShortForm schema.GroupResource
	LongForm  schema.GroupResource
}
