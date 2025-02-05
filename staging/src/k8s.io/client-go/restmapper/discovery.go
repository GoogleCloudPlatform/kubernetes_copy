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
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"k8s.io/klog/v2"
)

// APIGroupResources is an API group with a mapping of versions to
// resources.
type APIGroupResources struct {
	Group metav1.APIGroup
	// A mapping of version string to a slice of APIResources for
	// that version.
	VersionedResources map[string][]metav1.APIResource
}

// NewDiscoveryRESTMapper returns a PriorityRESTMapper based on the discovered
// groups and resources passed in.
//
// Deprecated: use NewDiscoveryRESTMapperWithContext instead.
func NewDiscoveryRESTMapper(groupResources []*APIGroupResources) meta.RESTMapper {
	mappers, resourcePriority, kindPriority := newDiscoveryRESTMapper(groupResources)
	multiMapper := make(meta.MultiRESTMapper, len(mappers))
	for i, m := range mappers {
		multiMapper[i] = m
	}
	return &meta.PriorityRESTMapper{
		Delegate:         multiMapper,
		ResourcePriority: resourcePriority,
		KindPriority:     kindPriority,
	}
}

// NewDiscoveryRESTMapperWithContext returns a PriorityRESTMapper based on the discovered
// groups and resources passed in.
func NewDiscoveryRESTMapperWithContext(groupResources []*APIGroupResources) meta.RESTMapperWithContext {
	mappers, resourcePriority, kindPriority := newDiscoveryRESTMapper(groupResources)
	multiMapper := make(meta.MultiRESTMapperWithContext, len(mappers))
	for i, m := range mappers {
		multiMapper[i] = m
	}
	return &meta.PriorityRESTMapperWithContext{
		Delegate:         multiMapper,
		ResourcePriority: resourcePriority,
		KindPriority:     kindPriority,
	}
}

func newDiscoveryRESTMapper(groupResources []*APIGroupResources) ([]*meta.DefaultRESTMapper, []schema.GroupVersionResource, []schema.GroupVersionKind) {
	var unionMapper []*meta.DefaultRESTMapper

	var groupPriority []string
	// /v1 is special.  It should always come first
	resourcePriority := []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: meta.AnyResource}}
	kindPriority := []schema.GroupVersionKind{{Group: "", Version: "v1", Kind: meta.AnyKind}}

	for _, group := range groupResources {
		groupPriority = append(groupPriority, group.Group.Name)

		// Make sure the preferred version comes first
		if len(group.Group.PreferredVersion.Version) != 0 {
			preferred := group.Group.PreferredVersion.Version
			if _, ok := group.VersionedResources[preferred]; ok {
				resourcePriority = append(resourcePriority, schema.GroupVersionResource{
					Group:    group.Group.Name,
					Version:  group.Group.PreferredVersion.Version,
					Resource: meta.AnyResource,
				})

				kindPriority = append(kindPriority, schema.GroupVersionKind{
					Group:   group.Group.Name,
					Version: group.Group.PreferredVersion.Version,
					Kind:    meta.AnyKind,
				})
			}
		}

		for _, discoveryVersion := range group.Group.Versions {
			resources, ok := group.VersionedResources[discoveryVersion.Version]
			if !ok {
				continue
			}

			// Add non-preferred versions after the preferred version, in case there are resources that only exist in those versions
			if discoveryVersion.Version != group.Group.PreferredVersion.Version {
				resourcePriority = append(resourcePriority, schema.GroupVersionResource{
					Group:    group.Group.Name,
					Version:  discoveryVersion.Version,
					Resource: meta.AnyResource,
				})

				kindPriority = append(kindPriority, schema.GroupVersionKind{
					Group:   group.Group.Name,
					Version: discoveryVersion.Version,
					Kind:    meta.AnyKind,
				})
			}

			gv := schema.GroupVersion{Group: group.Group.Name, Version: discoveryVersion.Version}
			versionMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})

			for _, resource := range resources {
				scope := meta.RESTScopeNamespace
				if !resource.Namespaced {
					scope = meta.RESTScopeRoot
				}

				// if we have a slash, then this is a subresource and we shouldn't create mappings for those.
				if strings.Contains(resource.Name, "/") {
					continue
				}

				plural := gv.WithResource(resource.Name)
				singular := gv.WithResource(resource.SingularName)
				// this is for legacy resources and servers which don't list singular forms.  For those we must still guess.
				if len(resource.SingularName) == 0 {
					_, singular = meta.UnsafeGuessKindToResource(gv.WithKind(resource.Kind))
				}

				versionMapper.AddSpecific(gv.WithKind(strings.ToLower(resource.Kind)), plural, singular, scope)
				versionMapper.AddSpecific(gv.WithKind(resource.Kind), plural, singular, scope)
				// TODO this is producing unsafe guesses that don't actually work, but it matches previous behavior
				versionMapper.Add(gv.WithKind(resource.Kind+"List"), scope)
			}
			// TODO why is this type not in discovery (at least for "v1")
			versionMapper.Add(gv.WithKind("List"), meta.RESTScopeRoot)
			unionMapper = append(unionMapper, versionMapper)
		}
	}

	for _, group := range groupPriority {
		resourcePriority = append(resourcePriority, schema.GroupVersionResource{
			Group:    group,
			Version:  meta.AnyVersion,
			Resource: meta.AnyResource,
		})
		kindPriority = append(kindPriority, schema.GroupVersionKind{
			Group:   group,
			Version: meta.AnyVersion,
			Kind:    meta.AnyKind,
		})
	}

	return unionMapper, resourcePriority, kindPriority
}

// GetAPIGroupResources uses the provided discovery client to gather
// discovery information and populate a slice of APIGroupResources.
//
// Deprecated: use GetAPIGroupResourcesWithContext instead.
func GetAPIGroupResources(cl discovery.DiscoveryInterface) ([]*APIGroupResources, error) {
	return GetAPIGroupResourcesWithContext(context.Background(), discovery.ToDiscoveryInterfaceWithContext(cl))
}

// GetAPIGroupResourcesWithContext uses the provided discovery client to gather
// discovery information and populate a slice of APIGroupResources.
func GetAPIGroupResourcesWithContext(ctx context.Context, cl discovery.DiscoveryInterfaceWithContext) ([]*APIGroupResources, error) {
	gs, rs, err := cl.ServerGroupsAndResourcesWithContext(ctx)
	if rs == nil || gs == nil {
		return nil, err
		// TODO track the errors and update callers to handle partial errors.
	}
	rsm := map[string]*metav1.APIResourceList{}
	for _, r := range rs {
		rsm[r.GroupVersion] = r
	}

	var result []*APIGroupResources
	for _, group := range gs {
		groupResources := &APIGroupResources{
			Group:              *group,
			VersionedResources: make(map[string][]metav1.APIResource),
		}
		for _, version := range group.Versions {
			resources, ok := rsm[version.GroupVersion]
			if !ok {
				continue
			}
			groupResources.VersionedResources[version.Version] = resources.APIResources
		}
		result = append(result, groupResources)
	}
	return result, nil
}

// DeferredDiscoveryRESTMapper is a RESTMapper and RESTMapperContext that will defer
// initialization of the RESTMapper until the first mapping is
// requested.
type DeferredDiscoveryRESTMapper struct {
	initMu   sync.Mutex
	delegate meta.RESTMapperWithContext
	cl       discovery.CachedDiscoveryInterfaceWithContext
}

var (
	_ meta.ResettableRESTMapper            = &DeferredDiscoveryRESTMapper{}
	_ meta.ResettableRESTMapperWithContext = &DeferredDiscoveryRESTMapper{}
	_ fmt.Stringer                         = &DeferredDiscoveryRESTMapper{}
)

// NewDeferredDiscoveryRESTMapper returns a
// DeferredDiscoveryRESTMapper that will lazily query the provided
// client for discovery information to do REST mappings.
//
// Deprecated: use NewDeferredDiscoveryRESTMapperWithContext instead. NewDeferredDiscoveryRESTMapper will try to convert cl to discovery.CachedDiscoveryInterfaceWithContext and use a wrapper if that is not possible, but NewDeferredDiscoveryRESTMapperWithContext ensures that no such conversion is necessary.
func NewDeferredDiscoveryRESTMapper(cl discovery.CachedDiscoveryInterface) *DeferredDiscoveryRESTMapper {
	return &DeferredDiscoveryRESTMapper{
		cl: discovery.ToCachedDiscoveryInterfaceWithContext(cl),
	}
}

// NewDeferredDiscoveryRESTMapperWithContext returns a
// DeferredDiscoveryRESTMapper that will lazily query the provided
// client for discovery information to do REST mappings.
func NewDeferredDiscoveryRESTMapperWithContext(cl discovery.CachedDiscoveryInterfaceWithContext) *DeferredDiscoveryRESTMapper {
	return &DeferredDiscoveryRESTMapper{
		cl: cl,
	}
}

func (d *DeferredDiscoveryRESTMapper) getDelegate(ctx context.Context) (meta.RESTMapperWithContext, error) {
	d.initMu.Lock()
	defer d.initMu.Unlock()

	if d.delegate != nil {
		return d.delegate, nil
	}

	groupResources, err := GetAPIGroupResourcesWithContext(ctx, d.cl)
	if err != nil {
		return nil, err
	}

	d.delegate = NewDiscoveryRESTMapperWithContext(groupResources)
	return d.delegate, nil
}

// Reset resets the internally cached Discovery information and will
// cause the next mapping request to re-discover.
func (d *DeferredDiscoveryRESTMapper) Reset() {
	d.ResetWithContext(context.Background())
}

// ResetWithContext resets the internally cached Discovery information and will
// cause the next mapping request to re-discover.
func (d *DeferredDiscoveryRESTMapper) ResetWithContext(ctx context.Context) {
	klog.FromContext(ctx).V(5).Info("Invalidating discovery information")

	d.initMu.Lock()
	defer d.initMu.Unlock()

	d.cl.InvalidateWithContext(ctx)
	d.delegate = nil
}

// KindFor takes a partial resource and returns back the single match.
// It returns an error if there are multiple matches.
//
// Deprecated: use KindForWithContext instead.
func (d *DeferredDiscoveryRESTMapper) KindFor(resource schema.GroupVersionResource) (gvk schema.GroupVersionKind, err error) {
	return d.KindForWithContext(context.Background(), resource)
}

// KindForWithContext takes a partial resource and returns back the single match.
// It returns an error if there are multiple matches.
func (d *DeferredDiscoveryRESTMapper) KindForWithContext(ctx context.Context, resource schema.GroupVersionResource) (gvk schema.GroupVersionKind, err error) {
	del, err := d.getDelegate(ctx)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	gvk, err = del.KindForWithContext(ctx, resource)
	if err != nil && !d.cl.FreshWithContext(ctx) {
		d.ResetWithContext(ctx)
		gvk, err = d.KindForWithContext(ctx, resource)
	}
	return
}

// KindsFor takes a partial resource and returns back the list of
// potential kinds in priority order.
//
// Deprecated: use KindsForWithContext instead.
func (d *DeferredDiscoveryRESTMapper) KindsFor(resource schema.GroupVersionResource) (gvks []schema.GroupVersionKind, err error) {
	return d.KindsForWithContext(context.Background(), resource)
}

// KindsForWithContext takes a partial resource and returns back the list of
// potential kinds in priority order.
func (d *DeferredDiscoveryRESTMapper) KindsForWithContext(ctx context.Context, resource schema.GroupVersionResource) (gvks []schema.GroupVersionKind, err error) {
	del, err := d.getDelegate(ctx)
	if err != nil {
		return nil, err
	}
	gvks, err = del.KindsForWithContext(ctx, resource)
	if len(gvks) == 0 && !d.cl.FreshWithContext(ctx) {
		d.ResetWithContext(ctx)
		gvks, err = d.KindsForWithContext(ctx, resource)
	}
	return
}

// ResourceFor takes a partial resource and returns back the single
// match. It returns an error if there are multiple matches.
//
// Deprecated: use ResourceForWithContext instead.
func (d *DeferredDiscoveryRESTMapper) ResourceFor(input schema.GroupVersionResource) (gvr schema.GroupVersionResource, err error) {
	return d.ResourceForWithContext(context.Background(), input)
}

// ResourceForWithContext takes a partial resource and returns back the single
// match. It returns an error if there are multiple matches.
func (d *DeferredDiscoveryRESTMapper) ResourceForWithContext(ctx context.Context, input schema.GroupVersionResource) (gvr schema.GroupVersionResource, err error) {
	del, err := d.getDelegate(ctx)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	gvr, err = del.ResourceForWithContext(ctx, input)
	if err != nil && !d.cl.FreshWithContext(ctx) {
		d.ResetWithContext(ctx)
		gvr, err = d.ResourceForWithContext(ctx, input)
	}
	return
}

// ResourcesFor takes a partial resource and returns back the list of
// potential resource in priority order.
//
// Deprecated: use ResourcesForWithContext instead.
func (d *DeferredDiscoveryRESTMapper) ResourcesFor(input schema.GroupVersionResource) (gvrs []schema.GroupVersionResource, err error) {
	return d.ResourcesForWithContext(context.Background(), input)
}

// ResourcesForWithContext takes a partial resource and returns back the list of
// potential resource in priority order.
func (d *DeferredDiscoveryRESTMapper) ResourcesForWithContext(ctx context.Context, input schema.GroupVersionResource) (gvrs []schema.GroupVersionResource, err error) {
	del, err := d.getDelegate(ctx)
	if err != nil {
		return nil, err
	}
	gvrs, err = del.ResourcesForWithContext(ctx, input)
	if len(gvrs) == 0 && !d.cl.FreshWithContext(ctx) {
		d.ResetWithContext(ctx)
		gvrs, err = d.ResourcesForWithContext(ctx, input)
	}
	return
}

// RESTMapping identifies a preferred resource mapping for the
// provided group kind.
//
// Deprecated: use RESTMappingWithContext instead.
func (d *DeferredDiscoveryRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (m *meta.RESTMapping, err error) {
	return d.RESTMappingWithContext(context.Background(), gk, versions...)
}

// RESTMappingWithContext identifies a preferred resource mapping for the
// provided group kind.
func (d *DeferredDiscoveryRESTMapper) RESTMappingWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) (m *meta.RESTMapping, err error) {
	del, err := d.getDelegate(ctx)
	if err != nil {
		return nil, err
	}
	m, err = del.RESTMappingWithContext(ctx, gk, versions...)
	if err != nil && !d.cl.FreshWithContext(ctx) {
		d.ResetWithContext(ctx)
		m, err = d.RESTMappingWithContext(ctx, gk, versions...)
	}
	return
}

// RESTMappings returns the RESTMappings for the provided group kind
// in a rough internal preferred order. If no kind is found, it will
// return a NoResourceMatchError.
//
// Deprecated: use RESTMappingsWithContext instead.
func (d *DeferredDiscoveryRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) (ms []*meta.RESTMapping, err error) {
	return d.RESTMappingsWithContext(context.Background(), gk, versions...)
}

// RESTMappingsWithContext returns the RESTMappings for the provided group kind
// in a rough internal preferred order. If no kind is found, it will
// return a NoResourceMatchError.
func (d *DeferredDiscoveryRESTMapper) RESTMappingsWithContext(ctx context.Context, gk schema.GroupKind, versions ...string) (ms []*meta.RESTMapping, err error) {
	del, err := d.getDelegate(ctx)
	if err != nil {
		return nil, err
	}
	ms, err = del.RESTMappingsWithContext(ctx, gk, versions...)
	if len(ms) == 0 && !d.cl.FreshWithContext(ctx) {
		d.ResetWithContext(ctx)
		ms, err = d.RESTMappingsWithContext(ctx, gk, versions...)
	}
	return
}

// ResourceSingularizer converts a resource name from plural to
// singular (e.g., from pods to pod).
//
// Deprecated: use ResourceSingularizerWithContext instead.
func (d *DeferredDiscoveryRESTMapper) ResourceSingularizer(resource string) (singular string, err error) {
	return d.ResourceSingularizerWithContext(context.Background(), resource)
}

// ResourceSingularizerWithContext converts a resource name from plural to
// singular (e.g., from pods to pod).
func (d *DeferredDiscoveryRESTMapper) ResourceSingularizerWithContext(ctx context.Context, resource string) (singular string, err error) {
	del, err := d.getDelegate(ctx)
	if err != nil {
		return resource, err
	}
	singular, err = del.ResourceSingularizerWithContext(ctx, resource)
	if err != nil && !d.cl.FreshWithContext(ctx) {
		d.ResetWithContext(ctx)
		singular, err = d.ResourceSingularizerWithContext(ctx, resource)
	}
	return
}

func (d *DeferredDiscoveryRESTMapper) String() string {
	del, err := d.getDelegate(context.Background() /* hopefully we already have a delegate and don't need the context */)
	if err != nil {
		return fmt.Sprintf("DeferredDiscoveryRESTMapper{%v}", err)
	}
	return fmt.Sprintf("DeferredDiscoveryRESTMapper{\n\t%v\n}", del)
}
