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

package fake

import (
	"context"
	"fmt"
	"net/http"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/openapi"
	kubeversion "k8s.io/client-go/pkg/version"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/testing"
)

// FakeDiscovery implements discovery.DiscoveryInterface and discovery.DiscoveryInterfaceWithContext.
// It sometimes calls testing.Fake.Invoke with an action,
// but doesn't respect the return value if any. There is a way to fake static values like ServerVersion by using the Faked... fields on the struct.
type FakeDiscovery struct {
	*testing.Fake
	FakedServerVersion *version.Info
}

var (
	_ discovery.DiscoveryInterface            = &FakeDiscovery{}
	_ discovery.DiscoveryInterfaceWithContext = &FakeDiscovery{}
)

// ServerResourcesForGroupVersion returns the supported resources for a group
// and version.
//
// Deprecated: use ServerResourcesForGroupVersionWithContext instead.
func (c *FakeDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	return c.ServerResourcesForGroupVersionWithContext(context.Background(), groupVersion)
}

// ServerResourcesForGroupVersionWithContext returns the supported resources for a group
// and version.
func (c *FakeDiscovery) ServerResourcesForGroupVersionWithContext(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	action := testing.ActionImpl{
		Verb:     "get",
		Resource: schema.GroupVersionResource{Resource: "resource"},
	}
	if _, err := c.Invokes(action, nil); err != nil {
		return nil, err
	}
	for _, resourceList := range c.Resources {
		if resourceList.GroupVersion == groupVersion {
			return resourceList, nil
		}
	}
	return nil, &errors.StatusError{
		ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusNotFound,
			Reason:  metav1.StatusReasonNotFound,
			Message: fmt.Sprintf("the server could not find the requested resource, GroupVersion %q not found", groupVersion),
		}}
}

// ServerGroupsAndResources returns the supported groups and resources for all groups and versions.
//
// Deprecated: use ServerGroupsAndResourcesWithContext instead.
func (c *FakeDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return c.ServerGroupsAndResourcesWithContext(context.Background())
}

// ServerGroupsAndResourcesWithContext returns the supported groups and resources for all groups and versions.
func (c *FakeDiscovery) ServerGroupsAndResourcesWithContext(ctx context.Context) ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	sgs, err := c.ServerGroupsWithContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	resultGroups := []*metav1.APIGroup{}
	for i := range sgs.Groups {
		resultGroups = append(resultGroups, &sgs.Groups[i])
	}

	action := testing.ActionImpl{
		Verb:     "get",
		Resource: schema.GroupVersionResource{Resource: "resource"},
	}
	if _, err = c.Invokes(action, nil); err != nil {
		return resultGroups, c.Resources, err
	}
	return resultGroups, c.Resources, nil
}

// ServerPreferredResources returns the supported resources with the version
// preferred by the server.
//
// Deprecated: use ServerPreferredResourcesWithContext instead.
func (c *FakeDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return c.ServerPreferredResourcesWithContext(context.Background())
}

// ServerPreferredResourcesWithContext returns the supported resources with the version
// preferred by the server.
func (c *FakeDiscovery) ServerPreferredResourcesWithContext(ctx context.Context) ([]*metav1.APIResourceList, error) {
	return nil, nil
}

// ServerPreferredNamespacedResources returns the supported namespaced resources
// with the version preferred by the server.
//
// Deprecated: use ServerPreferredNamespacedResourcesWithContext instead.
func (c *FakeDiscovery) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return c.ServerPreferredNamespacedResourcesWithContext(context.Background())
}

// ServerPreferredNamespacedResourcesWithContext returns the supported namespaced resources
// with the version preferred by the server.
func (c *FakeDiscovery) ServerPreferredNamespacedResourcesWithContext(ctx context.Context) ([]*metav1.APIResourceList, error) {
	return nil, nil
}

// ServerGroups returns the supported groups, with information like supported
// versions and the preferred version.
//
// Deprecated: use ServerGroupsWithContext instead.
func (c *FakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return c.ServerGroupsWithContext(context.Background())
}

// ServerGroupsWithContext returns the supported groups, with information like supported
// versions and the preferred version.
func (c *FakeDiscovery) ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error) {
	action := testing.ActionImpl{
		Verb:     "get",
		Resource: schema.GroupVersionResource{Resource: "group"},
	}
	if _, err := c.Invokes(action, nil); err != nil {
		return nil, err
	}

	groups := map[string]*metav1.APIGroup{}

	for _, res := range c.Resources {
		gv, err := schema.ParseGroupVersion(res.GroupVersion)
		if err != nil {
			return nil, err
		}
		group := groups[gv.Group]
		if group == nil {
			group = &metav1.APIGroup{
				Name: gv.Group,
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: res.GroupVersion,
					Version:      gv.Version,
				},
			}
			groups[gv.Group] = group
		}

		group.Versions = append(group.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: res.GroupVersion,
			Version:      gv.Version,
		})
	}

	list := &metav1.APIGroupList{}
	for _, apiGroup := range groups {
		list.Groups = append(list.Groups, *apiGroup)
	}

	return list, nil

}

// ServerVersion retrieves and parses the server's version.
//
// Deprecated: use ServerVersionWithContext instead.
func (c *FakeDiscovery) ServerVersion() (*version.Info, error) {
	return c.ServerVersionWithContext(context.Background())
}

// ServerVersionWithContext retrieves and parses the server's version.
func (c *FakeDiscovery) ServerVersionWithContext(ctx context.Context) (*version.Info, error) {
	action := testing.ActionImpl{}
	action.Verb = "get"
	action.Resource = schema.GroupVersionResource{Resource: "version"}
	_, err := c.Invokes(action, nil)
	if err != nil {
		return nil, err
	}

	if c.FakedServerVersion != nil {
		return c.FakedServerVersion, nil
	}

	versionInfo := kubeversion.Get()
	return &versionInfo, nil
}

// OpenAPISchema retrieves and parses the swagger API schema the server supports.
//
// Deprecated: use OpenAPISchemaWithContext instead.
func (c *FakeDiscovery) OpenAPISchema() (*openapi_v2.Document, error) {
	return c.OpenAPISchemaWithContext(context.Background())
}

// OpenAPISchemaWithContext retrieves and parses the swagger API schema the server supports.
func (c *FakeDiscovery) OpenAPISchemaWithContext(ctx context.Context) (*openapi_v2.Document, error) {
	return &openapi_v2.Document{}, nil
}

// Deprecated: use OpenAPIV3WithContext instead.
func (c *FakeDiscovery) OpenAPIV3() openapi.Client {
	panic("unimplemented")
}

func (c *FakeDiscovery) OpenAPIV3WithContext(ctx context.Context) openapi.ClientWithContext {
	panic("unimplemented")
}

// RESTClient returns a RESTClient that is used to communicate with API server
// by this client implementation.
func (c *FakeDiscovery) RESTClient() restclient.Interface {
	return nil
}

// Deprecated: use WithLegacyWithContext instead.
func (c *FakeDiscovery) WithLegacy() discovery.DiscoveryInterface {
	panic("unimplemented")
}

func (c *FakeDiscovery) WithLegacyWithContext(ctx context.Context) discovery.DiscoveryInterfaceWithContext {
	panic("unimplemented")
}
