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

package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apidiscovery "k8s.io/api/apidiscovery/v2"
	errorsutil "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/openapi"
	"k8s.io/client-go/rest"
	testutil "k8s.io/client-go/util/testing"
	"k8s.io/klog/v2/ktesting"
)

type resourceMapEntry struct {
	list *metav1.APIResourceList
	err  error
}

type fakeDiscovery struct {
	// Intentionally limited to discovery.DiscoveryInterface because that is all that this
	// code knows about and partly overrides. *fake.FakeDiscovery would inherit
	// e.g. ServerGroupsWithContext which then would have to be overridden.
	discovery.DiscoveryInterface

	lock         sync.Mutex
	groupList    *metav1.APIGroupList
	groupListErr error
	resourceMap  map[string]*resourceMapEntry
}

func (c *fakeDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if rl, ok := c.resourceMap[groupVersion]; ok {
		return rl.list, rl.err
	}
	return nil, errors.New("doesn't exist")
}

func (c *fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.groupList == nil {
		return nil, errors.New("doesn't exist")
	}
	return c.groupList, c.groupListErr
}

func TestClient(t *testing.T) {
	fake := &fakeDiscovery{
		groupList: &metav1.APIGroupList{
			Groups: []metav1.APIGroup{{
				Name: "astronomy",
				Versions: []metav1.GroupVersionForDiscovery{{
					GroupVersion: "astronomy/v8beta1",
					Version:      "v8beta1",
				}},
			}},
		},
		resourceMap: map[string]*resourceMapEntry{
			"astronomy/v8beta1": {
				list: &metav1.APIResourceList{
					GroupVersion: "astronomy/v8beta1",
					APIResources: []metav1.APIResource{{
						Name:         "dwarfplanets",
						SingularName: "dwarfplanet",
						Namespaced:   true,
						Kind:         "DwarfPlanet",
						ShortNames:   []string{"dp"},
					}},
				},
			},
		},
	}

	c := NewMemCacheClient(fake)
	if c.Fresh() {
		t.Errorf("Expected not fresh.")
	}
	g, err := c.ServerGroups()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.groupList, g; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
	if !c.Fresh() {
		t.Errorf("Expected fresh.")
	}
	c.Invalidate()
	if c.Fresh() {
		t.Errorf("Expected not fresh.")
	}

	g, err = c.ServerGroups()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.groupList, g; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
	if !c.Fresh() {
		t.Errorf("Expected fresh.")
	}
	r, err := c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.resourceMap["astronomy/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}

	fake.lock.Lock()
	fake.resourceMap = map[string]*resourceMapEntry{
		"astronomy/v8beta1": {
			list: &metav1.APIResourceList{
				GroupVersion: "astronomy/v8beta1",
				APIResources: []metav1.APIResource{{
					Name:         "stars",
					SingularName: "star",
					Namespaced:   true,
					Kind:         "Star",
					ShortNames:   []string{"s"},
				}},
			},
		},
	}
	fake.lock.Unlock()

	c.Invalidate()
	r, err = c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.resourceMap["astronomy/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
}

func TestServerGroupsFails(t *testing.T) {
	fake := &fakeDiscovery{
		groupList: &metav1.APIGroupList{
			Groups: []metav1.APIGroup{{
				Name: "astronomy",
				Versions: []metav1.GroupVersionForDiscovery{{
					GroupVersion: "astronomy/v8beta1",
					Version:      "v8beta1",
				}},
			}},
		},
		groupListErr: errors.New("some error"),
		resourceMap: map[string]*resourceMapEntry{
			"astronomy/v8beta1": {
				list: &metav1.APIResourceList{
					GroupVersion: "astronomy/v8beta1",
					APIResources: []metav1.APIResource{{
						Name:         "dwarfplanets",
						SingularName: "dwarfplanet",
						Namespaced:   true,
						Kind:         "DwarfPlanet",
						ShortNames:   []string{"dp"},
					}},
				},
			},
		},
	}

	c := NewMemCacheClient(fake)
	if c.Fresh() {
		t.Errorf("Expected not fresh.")
	}
	_, err := c.ServerGroups()
	if err == nil {
		t.Errorf("Expected error")
	}
	if c.Fresh() {
		t.Errorf("Expected not fresh.")
	}
	fake.lock.Lock()
	fake.groupListErr = nil
	fake.lock.Unlock()
	r, err := c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.resourceMap["astronomy/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
	if !c.Fresh() {
		t.Errorf("Expected not fresh.")
	}
}

func TestPartialPermanentFailure(t *testing.T) {
	fake := &fakeDiscovery{
		groupList: &metav1.APIGroupList{
			Groups: []metav1.APIGroup{
				{
					Name: "astronomy",
					Versions: []metav1.GroupVersionForDiscovery{{
						GroupVersion: "astronomy/v8beta1",
						Version:      "v8beta1",
					}},
				},
				{
					Name: "astronomy2",
					Versions: []metav1.GroupVersionForDiscovery{{
						GroupVersion: "astronomy2/v8beta1",
						Version:      "v8beta1",
					}},
				},
			},
		},
		resourceMap: map[string]*resourceMapEntry{
			"astronomy/v8beta1": {
				err: errors.New("some permanent error"),
			},
			"astronomy2/v8beta1": {
				list: &metav1.APIResourceList{
					GroupVersion: "astronomy2/v8beta1",
					APIResources: []metav1.APIResource{{
						Name:         "dwarfplanets",
						SingularName: "dwarfplanet",
						Namespaced:   true,
						Kind:         "DwarfPlanet",
						ShortNames:   []string{"dp"},
					}},
				},
			},
		},
	}

	c := NewMemCacheClient(fake)
	if c.Fresh() {
		t.Errorf("Expected not fresh.")
	}
	r, err := c.ServerResourcesForGroupVersion("astronomy2/v8beta1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.resourceMap["astronomy2/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
	_, err = c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err == nil {
		t.Errorf("Expected error, got nil")
	}

	fake.lock.Lock()
	fake.resourceMap["astronomy/v8beta1"] = &resourceMapEntry{
		list: &metav1.APIResourceList{
			GroupVersion: "astronomy/v8beta1",
			APIResources: []metav1.APIResource{{
				Name:         "dwarfplanets",
				SingularName: "dwarfplanet",
				Namespaced:   true,
				Kind:         "DwarfPlanet",
				ShortNames:   []string{"dp"},
			}},
		},
		err: nil,
	}
	fake.lock.Unlock()
	// We don't retry permanent errors, so it should fail.
	_, err = c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err == nil {
		t.Errorf("Expected error, got nil")
	}
	c.Invalidate()

	// After Invalidate, we should retry.
	r, err = c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.resourceMap["astronomy/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
}

func TestPartialRetryableFailure(t *testing.T) {
	fake := &fakeDiscovery{
		groupList: &metav1.APIGroupList{
			Groups: []metav1.APIGroup{
				{
					Name: "astronomy",
					Versions: []metav1.GroupVersionForDiscovery{{
						GroupVersion: "astronomy/v8beta1",
						Version:      "v8beta1",
					}},
				},
				{
					Name: "astronomy2",
					Versions: []metav1.GroupVersionForDiscovery{{
						GroupVersion: "astronomy2/v8beta1",
						Version:      "v8beta1",
					}},
				},
			},
		},
		resourceMap: map[string]*resourceMapEntry{
			"astronomy/v8beta1": {
				err: &errorsutil.StatusError{
					ErrStatus: metav1.Status{
						Message: "Some retryable error",
						Code:    int32(http.StatusServiceUnavailable),
						Reason:  metav1.StatusReasonServiceUnavailable,
					},
				},
			},
			"astronomy2/v8beta1": {
				list: &metav1.APIResourceList{
					GroupVersion: "astronomy2/v8beta1",
					APIResources: []metav1.APIResource{{
						Name:         "dwarfplanets",
						SingularName: "dwarfplanet",
						Namespaced:   true,
						Kind:         "DwarfPlanet",
						ShortNames:   []string{"dp"},
					}},
				},
			},
		},
	}

	c := NewMemCacheClient(fake)
	if c.Fresh() {
		t.Errorf("Expected not fresh.")
	}
	r, err := c.ServerResourcesForGroupVersion("astronomy2/v8beta1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if e, a := fake.resourceMap["astronomy2/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
	_, err = c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err == nil {
		t.Errorf("Expected error, got nil")
	}

	fake.lock.Lock()
	fake.resourceMap["astronomy/v8beta1"] = &resourceMapEntry{
		list: &metav1.APIResourceList{
			GroupVersion: "astronomy/v8beta1",
			APIResources: []metav1.APIResource{{
				Name:         "dwarfplanets",
				SingularName: "dwarfplanet",
				Namespaced:   true,
				Kind:         "DwarfPlanet",
				ShortNames:   []string{"dp"},
			}},
		},
		err: nil,
	}
	fake.lock.Unlock()
	// We should retry retryable error even without Invalidate() being called,
	// so no error is expected.
	r, err = c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if e, a := fake.resourceMap["astronomy/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}

	// Check that the last result was cached and we don't retry further.
	fake.lock.Lock()
	fake.resourceMap["astronomy/v8beta1"].err = errors.New("some permanent error")
	fake.lock.Unlock()
	r, err = c.ServerResourcesForGroupVersion("astronomy/v8beta1")
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if e, a := fake.resourceMap["astronomy/v8beta1"].list, r; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}
}

// Tests that schema instances returned by openapi cached and returned after
// successive calls
func TestOpenAPIMemCache(t *testing.T) {
	fakeServer, err := testutil.NewFakeOpenAPIV3Server("../../testdata")
	require.NoError(t, err)
	defer fakeServer.HttpServer.Close()

	require.NotEmpty(t, fakeServer.ServedDocuments)

	client := NewMemCacheClient(
		discovery.NewDiscoveryClientForConfigOrDie(
			&rest.Config{Host: fakeServer.HttpServer.URL},
		),
	)
	openapiClient := client.OpenAPIV3()

	paths, err := openapiClient.Paths()
	require.NoError(t, err)

	contentTypes := []string{
		runtime.ContentTypeJSON, openapi.ContentTypeOpenAPIV3PB,
	}

	for _, contentType := range contentTypes {
		t.Run(contentType, func(t *testing.T) {
			for k, v := range paths {
				original, err := v.Schema(contentType)
				if !assert.NoError(t, err) {
					continue
				}

				// This is the original OpenAPI client. It is not affected
				// by client.Invalidate() below.
				pathsAgain, err := openapiClient.Paths()
				if !assert.NoError(t, err) {
					continue
				}

				schemaAgain, err := pathsAgain[k].Schema(contentType)
				if !assert.NoError(t, err) {
					continue
				}

				// The map itself might be different, but the content should not be.
				assert.Equal(t, reflect.ValueOf(paths[k]).Pointer(), reflect.ValueOf(pathsAgain[k]).Pointer())
				assert.Equal(t, reflect.ValueOf(original).Pointer(), reflect.ValueOf(schemaAgain).Pointer())

				// Invalidate and try again. This time pointers should not be equal
				client.Invalidate()

				pathsAgain, err = client.OpenAPIV3().Paths()
				if !assert.NoError(t, err) {
					continue
				}

				schemaAgain, err = pathsAgain[k].Schema(contentType)
				if !assert.NoError(t, err) {
					continue
				}

				assert.NotEqual(t, reflect.ValueOf(paths[k]).Pointer(), reflect.ValueOf(pathsAgain[k]).Pointer())
				assert.NotEqual(t, reflect.ValueOf(original).Pointer(), reflect.ValueOf(schemaAgain).Pointer())
				assert.Equal(t, original, schemaAgain)
			}
		})
	}
}

// Tests that schema instances returned by openapi are cached and returned after
// successive calls.
func TestOpenAPIMemCacheWithContext(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	fakeServer, err := testutil.NewFakeOpenAPIV3Server("../../testdata")
	require.NoError(t, err)
	defer fakeServer.HttpServer.Close()

	require.NotEmpty(t, fakeServer.ServedDocuments)

	client := NewMemCacheClientWithContext(
		discovery.NewDiscoveryClientForConfigOrDie(
			&rest.Config{Host: fakeServer.HttpServer.URL},
		),
	)
	openapiClient := client.OpenAPIV3WithContext(ctx)

	paths, err := openapiClient.PathsWithContext(ctx)
	require.NoError(t, err)

	contentTypes := []string{
		runtime.ContentTypeJSON, openapi.ContentTypeOpenAPIV3PB,
	}

	for _, contentType := range contentTypes {
		t.Run(contentType, func(t *testing.T) {
			for k, v := range paths {
				original, err := v.SchemaWithContext(ctx, contentType)
				if !assert.NoError(t, err) {
					continue
				}

				// This is the original OpenAPI client. It is not affected
				// by client.Invalidate() below.
				pathsAgain, err := openapiClient.PathsWithContext(ctx)
				if !assert.NoError(t, err) {
					continue
				}

				schemaAgain, err := pathsAgain[k].SchemaWithContext(ctx, contentType)
				if !assert.NoError(t, err) {
					continue
				}

				// When using the *WithContext API, the map itself is cached.
				assert.Equal(t, reflect.ValueOf(paths).Pointer(), reflect.ValueOf(pathsAgain).Pointer())
				assert.Equal(t, reflect.ValueOf(original).Pointer(), reflect.ValueOf(schemaAgain).Pointer())

				// Invalidate and try again. This time pointers should not be equal
				client.InvalidateWithContext(ctx)

				pathsAgain, err = client.OpenAPIV3WithContext(ctx).PathsWithContext(ctx)
				if !assert.NoError(t, err) {
					continue
				}

				schemaAgain, err = pathsAgain[k].SchemaWithContext(ctx, contentType)
				if !assert.NoError(t, err) {
					continue
				}

				assert.NotEqual(t, reflect.ValueOf(paths).Pointer(), reflect.ValueOf(pathsAgain).Pointer())
				assert.NotEqual(t, reflect.ValueOf(original).Pointer(), reflect.ValueOf(schemaAgain).Pointer())
				assert.Equal(t, original, schemaAgain)
			}
		})
	}
}

// Tests function "GroupsAndMaybeResources" when the "unaggregated" discovery is returned.
func TestMemCacheGroupsAndMaybeResources(t *testing.T) {
	tests := []struct {
		name                  string
		corev1                *metav1.APIVersions
		apis                  *metav1.APIGroupList
		expectedGroupNames    []string
		expectedGroupVersions []string
	}{
		{
			name: "Legacy discovery format: 1 version at /api, 1 group at /apis",
			corev1: &metav1.APIVersions{
				Versions: []string{
					"v1",
				},
			},
			apis: &metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "extensions",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "extensions/v1beta1"},
						},
					},
				},
			},
			expectedGroupNames:    []string{"", "extensions"},
			expectedGroupVersions: []string{"v1", "extensions/v1beta1"},
		},
		{
			name: "Legacy discovery format: 1 version at /api, 2 groups/1 version at /apis",
			corev1: &metav1.APIVersions{
				Versions: []string{
					"v1",
				},
			},
			apis: &metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "apps",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "apps/v1"},
						},
					},
					{
						Name: "extensions",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "extensions/v1beta1"},
						},
					},
				},
			},
			expectedGroupNames:    []string{"", "apps", "extensions"},
			expectedGroupVersions: []string{"v1", "apps/v1", "extensions/v1beta1"},
		},
		{
			name: "Legacy discovery format: 1 version at /api, 2 groups/2 versions at /apis",
			corev1: &metav1.APIVersions{
				Versions: []string{
					"v1",
				},
			},
			apis: &metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "batch",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "batch/v1"},
						},
					},
					{
						Name: "batch",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "batch/v1beta1"},
						},
					},
					{
						Name: "extensions",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "extensions/v1beta1"},
						},
					},
					{
						Name: "extensions",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "extensions/v1alpha1"},
						},
					},
				},
			},
			expectedGroupNames: []string{
				"",
				"batch",
				"extensions",
			},
			expectedGroupVersions: []string{
				"v1",
				"batch/v1",
				"batch/v1beta1",
				"extensions/v1beta1",
				"extensions/v1alpha1",
			},
		},
	}

	for _, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var body interface{}
			switch req.URL.Path {
			case "/api":
				body = test.corev1
			case "/apis":
				body = test.apis
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			output, err := json.Marshal(body)
			require.NoError(t, err)
			// Content-type is "unaggregated" discovery format -- no resources returned.
			w.Header().Set("Content-Type", discovery.AcceptV1)
			w.WriteHeader(http.StatusOK)
			w.Write(output)
		}))
		defer server.Close()
		client := discovery.NewDiscoveryClientForConfigOrDie(&rest.Config{Host: server.URL})
		memClient := memCacheClient{
			delegate:               client,
			groupToServerResources: map[string]*cacheEntry{},
		}
		assert.False(t, memClient.Fresh())
		apiGroupList, resourcesMap, failedGVs, err := memClient.GroupsAndMaybeResources()
		require.NoError(t, err)
		// "Unaggregated" discovery always returns nil for resources.
		assert.Nil(t, resourcesMap)
		assert.Emptyf(t, failedGVs, "expected empty failed GroupVersions, got (%d)", len(failedGVs))
		assert.False(t, memClient.receivedAggregatedDiscovery)
		assert.True(t, memClient.Fresh())
		// Test the expected groups are returned for the aggregated format.
		expectedGroupNames := sets.NewString(test.expectedGroupNames...)
		actualGroupNames := sets.NewString(groupNamesFromList(apiGroupList)...)
		assert.True(t, expectedGroupNames.Equal(actualGroupNames),
			"%s: Expected groups (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
		// Test the expected group versions for the aggregated discovery is correct.
		expectedGroupVersions := sets.NewString(test.expectedGroupVersions...)
		actualGroupVersions := sets.NewString(groupVersionsFromGroups(apiGroupList)...)
		assert.True(t, expectedGroupVersions.Equal(actualGroupVersions),
			"%s: Expected group/versions (%s), got (%s)", test.name, expectedGroupVersions.List(), actualGroupVersions.List())
		// Invalidate the cache and retrieve the server groups and resources again.
		memClient.Invalidate()
		assert.False(t, memClient.Fresh())
		apiGroupList, resourcesMap, _, err = memClient.GroupsAndMaybeResources()
		require.NoError(t, err)
		assert.Nil(t, resourcesMap)
		assert.False(t, memClient.receivedAggregatedDiscovery)
		// Test the expected groups are returned for the aggregated format.
		actualGroupNames = sets.NewString(groupNamesFromList(apiGroupList)...)
		assert.True(t, expectedGroupNames.Equal(actualGroupNames),
			"%s: Expected after invalidation groups (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
	}
}

// Tests function "GroupsAndMaybeResources" when the "aggregated" discovery is returned.
func TestAggregatedMemCacheGroupsAndMaybeResources(t *testing.T) {
	tests := []struct {
		name                  string
		corev1                *apidiscovery.APIGroupDiscoveryList
		apis                  *apidiscovery.APIGroupDiscoveryList
		expectedGroupNames    []string
		expectedGroupVersions []string
		expectedGVKs          []string
		expectedFailedGVs     []string
	}{
		{
			name: "Aggregated discovery: 1 group/1 resources at /api, 1 group/1 resources at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroupNames:    []string{"", "apps"},
			expectedGroupVersions: []string{"v1", "apps/v1"},
			expectedGVKs: []string{
				"/v1/Pod",
				"apps/v1/Deployment",
			},
			expectedFailedGVs: []string{},
		},
		{
			name: "Aggregated discovery: 1 group/1 resources at /api, 1 group/2 versions/1 resources/1 stale GV at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
							{
								// Stale Version is not included in discovery.
								Version: "v2",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v2",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
						},
					},
				},
			},
			expectedGroupNames:    []string{"", "apps"},
			expectedGroupVersions: []string{"v1", "apps/v1"},
			expectedGVKs: []string{
				"/v1/Pod",
				"apps/v1/Deployment",
			},
			expectedFailedGVs: []string{"apps/v2"},
		},
		{
			name: "Aggregated discovery: 1 group/2 resources at /api, 1 group/2 resources at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "services",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Service",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroupNames:    []string{"", "apps"},
			expectedGroupVersions: []string{"v1", "apps/v1"},
			expectedGVKs: []string{
				"/v1/Pod",
				"/v1/Service",
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
			},
			expectedFailedGVs: []string{},
		},
		{
			name: "Aggregated discovery: 1 group/2 resources at /api, 2 group/2 resources/1 stale GV at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "services",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Service",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
							{
								// Stale version is not included in discovery.
								Version: "v1beta1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1beta1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1beta1",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "batch",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "jobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1",
											Kind:    "Job",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "cronjobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1",
											Kind:    "CronJob",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroupNames:    []string{"", "apps", "batch"},
			expectedGroupVersions: []string{"v1", "apps/v1", "batch/v1"},
			expectedGVKs: []string{
				"/v1/Pod",
				"/v1/Service",
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
				"batch/v1/Job",
				"batch/v1/CronJob",
			},
			expectedFailedGVs: []string{"apps/v1beta1"},
		},
		{
			name:   "Aggregated discovery: /api returns nothing, 2 groups/2 resources/2 stale GV at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							// Statel "v1" Version is not included in discovery.
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
							{
								Version: "v1beta1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1beta1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1beta1",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "batch",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "jobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1",
											Kind:    "Job",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "cronjobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1",
											Kind:    "CronJob",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
							{
								// Stale Version is not included in discovery.
								Version: "v1beta1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "jobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1beta1",
											Kind:    "Job",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
						},
					},
				},
			},
			expectedGroupNames:    []string{"apps", "batch"},
			expectedGroupVersions: []string{"apps/v1beta1", "batch/v1"},
			expectedGVKs: []string{
				"apps/v1beta1/Deployment",
				"apps/v1beta1/StatefulSet",
				"batch/v1/Job",
				"batch/v1/CronJob",
			},
			expectedFailedGVs: []string{"apps/v1", "batch/v1beta1"},
		},
	}

	for _, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var agg *apidiscovery.APIGroupDiscoveryList
			switch req.URL.Path {
			case "/api":
				agg = test.corev1
			case "/apis":
				agg = test.apis
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			output, err := json.Marshal(agg)
			require.NoError(t, err)
			// Content-type is "aggregated" discovery format.
			w.Header().Set("Content-Type", discovery.AcceptV2)
			w.WriteHeader(http.StatusOK)
			w.Write(output)
		}))
		defer server.Close()
		client := discovery.NewDiscoveryClientForConfigOrDie(&rest.Config{Host: server.URL})
		memClient := memCacheClient{
			delegate:               client,
			groupToServerResources: map[string]*cacheEntry{},
		}
		assert.False(t, memClient.Fresh())
		apiGroupList, resourcesMap, failedGVs, err := memClient.GroupsAndMaybeResources()
		require.NoError(t, err)
		assert.True(t, memClient.receivedAggregatedDiscovery)
		assert.True(t, memClient.Fresh())
		// Test the expected groups are returned for the aggregated format.
		expectedGroupNames := sets.NewString(test.expectedGroupNames...)
		actualGroupNames := sets.NewString(groupNamesFromList(apiGroupList)...)
		assert.True(t, expectedGroupNames.Equal(actualGroupNames),
			"%s: Expected groups (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
		// Test the expected group versions for the aggregated discovery is correct.
		expectedGroupVersions := sets.NewString(test.expectedGroupVersions...)
		actualGroupVersions := sets.NewString(groupVersionsFromGroups(apiGroupList)...)
		assert.True(t, expectedGroupVersions.Equal(actualGroupVersions),
			"%s: Expected group/versions (%s), got (%s)", test.name, expectedGroupVersions.List(), actualGroupVersions.List())
		// Test the resources are correct.
		expectedGVKs := sets.NewString(test.expectedGVKs...)
		resources := []*metav1.APIResourceList{}
		for _, resourceList := range resourcesMap {
			resources = append(resources, resourceList)
		}
		actualGVKs := sets.NewString(groupVersionKinds(resources)...)
		assert.True(t, expectedGVKs.Equal(actualGVKs),
			"%s: Expected GVKs (%s), got (%s)", test.name, expectedGVKs.List(), actualGVKs.List())
		// Test the returned failed GroupVersions are correct.
		expectedFailedGVs := sets.NewString(test.expectedFailedGVs...)
		actualFailedGVs := sets.NewString(failedGroupVersions(failedGVs)...)
		assert.True(t, expectedFailedGVs.Equal(actualFailedGVs),
			"%s: Expected Failed GroupVersions (%s), got (%s)", test.name, expectedFailedGVs.List(), actualFailedGVs.List())
		// Invalidate the cache and retrieve the server groups again.
		memClient.Invalidate()
		assert.False(t, memClient.Fresh())
		apiGroupList, _, _, err = memClient.GroupsAndMaybeResources()

		require.NoError(t, err)
		// Test the expected groups are returned for the aggregated format.
		actualGroupNames = sets.NewString(groupNamesFromList(apiGroupList)...)
		assert.True(t, expectedGroupNames.Equal(actualGroupNames),
			"%s: Expected after invalidation groups (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
	}
}

// Tests function "ServerGroups" when the "aggregated" discovery is returned.
func TestMemCacheAggregatedServerGroups(t *testing.T) {
	tests := []struct {
		name                      string
		corev1                    *apidiscovery.APIGroupDiscoveryList
		apis                      *apidiscovery.APIGroupDiscoveryList
		expectedGroupNames        []string
		expectedGroupVersions     []string
		expectedPreferredVersions []string
	}{
		{
			name: "Aggregated discovery: 1 group/1 version at /api, 1 group/1 version at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroupNames:        []string{"", "apps"},
			expectedGroupVersions:     []string{"v1", "apps/v1"},
			expectedPreferredVersions: []string{"v1", "apps/v1"},
		},
		{
			name: "Aggregated discovery: 1 group/1 version at /api, 1 group/2 versions at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							// v2 is preferred since it is first
							{
								Version: "v2",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v2",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroupNames:        []string{"", "apps"},
			expectedGroupVersions:     []string{"v1", "apps/v1", "apps/v2"},
			expectedPreferredVersions: []string{"v1", "apps/v2"},
		},
		{
			name:   "Aggregated discovery: /api returns nothing, 2 groups at /apis",
			corev1: &apidiscovery.APIGroupDiscoveryList{},
			apis: &apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "batch",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							// v1 is preferred since it is first
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "jobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1",
											Kind:    "Job",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "cronjobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1",
											Kind:    "CronJob",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
							{
								Version: "v1beta1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "jobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1beta1",
											Kind:    "Job",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "cronjobs",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "batch",
											Version: "v1beta1",
											Kind:    "CronJob",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroupNames:        []string{"apps", "batch"},
			expectedGroupVersions:     []string{"apps/v1", "batch/v1", "batch/v1beta1"},
			expectedPreferredVersions: []string{"apps/v1", "batch/v1"},
		},
	}

	for _, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var agg *apidiscovery.APIGroupDiscoveryList
			switch req.URL.Path {
			case "/api":
				agg = test.corev1
			case "/apis":
				agg = test.apis
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			output, err := json.Marshal(agg)
			require.NoError(t, err)
			// Content-type is "aggregated" discovery format.
			w.Header().Set("Content-Type", discovery.AcceptV2Beta1)
			w.WriteHeader(http.StatusOK)
			w.Write(output)
		}))
		defer server.Close()
		client := discovery.NewDiscoveryClientForConfigOrDie(&rest.Config{Host: server.URL})
		memCacheClient := NewMemCacheClient(client)
		assert.False(t, memCacheClient.Fresh())
		apiGroupList, err := memCacheClient.ServerGroups()
		require.NoError(t, err)
		assert.True(t, memCacheClient.Fresh())
		// Test the expected groups are returned for the aggregated format.
		expectedGroupNames := sets.NewString(test.expectedGroupNames...)
		actualGroupNames := sets.NewString(groupNamesFromList(apiGroupList)...)
		assert.True(t, expectedGroupNames.Equal(actualGroupNames),
			"%s: Expected groups (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
		// Test the expected group versions for the aggregated discovery is correct.
		expectedGroupVersions := sets.NewString(test.expectedGroupVersions...)
		actualGroupVersions := sets.NewString(groupVersionsFromGroups(apiGroupList)...)
		assert.True(t, expectedGroupVersions.Equal(actualGroupVersions),
			"%s: Expected group/versions (%s), got (%s)", test.name, expectedGroupVersions.List(), actualGroupVersions.List())
		// Test the groups preferred version is correct.
		expectedPreferredVersions := sets.NewString(test.expectedPreferredVersions...)
		actualPreferredVersions := sets.NewString(preferredVersionsFromList(apiGroupList)...)
		assert.True(t, expectedPreferredVersions.Equal(actualPreferredVersions),
			"%s: Expected preferred group/version (%s), got (%s)", test.name, expectedPreferredVersions.List(), actualPreferredVersions.List())
		// Invalidate the cache and retrieve the server groups again.
		memCacheClient.Invalidate()
		assert.False(t, memCacheClient.Fresh())
		apiGroupList, err = memCacheClient.ServerGroups()
		require.NoError(t, err)
		// Test the expected groups are returned for the aggregated format.
		actualGroupNames = sets.NewString(groupNamesFromList(apiGroupList)...)
		assert.True(t, expectedGroupNames.Equal(actualGroupNames),
			"%s: Expected after invalidation groups (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
	}
}

func groupNamesFromList(groups *metav1.APIGroupList) []string {
	result := []string{}
	for _, group := range groups.Groups {
		result = append(result, group.Name)
	}
	return result
}

func preferredVersionsFromList(groups *metav1.APIGroupList) []string {
	result := []string{}
	for _, group := range groups.Groups {
		preferredGV := group.PreferredVersion.GroupVersion
		result = append(result, preferredGV)
	}
	return result
}

func groupVersionsFromGroups(groups *metav1.APIGroupList) []string {
	result := []string{}
	for _, group := range groups.Groups {
		for _, version := range group.Versions {
			result = append(result, version.GroupVersion)
		}
	}
	return result
}

func groupVersionKinds(resources []*metav1.APIResourceList) []string {
	result := []string{}
	for _, resourceList := range resources {
		for _, resource := range resourceList.APIResources {
			gvk := fmt.Sprintf("%s/%s/%s", resource.Group, resource.Version, resource.Kind)
			result = append(result, gvk)
		}
	}
	return result
}

func failedGroupVersions(gvs map[schema.GroupVersion]error) []string {
	result := []string{}
	for gv := range gvs {
		result = append(result, gv.String())
	}
	return result
}
