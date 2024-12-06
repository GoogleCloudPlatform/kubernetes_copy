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

package disk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apidiscovery "k8s.io/api/apidiscovery/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/openapi"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/rest/fake"
	testutil "k8s.io/client-go/util/testing"
)

func TestCachedDiscoveryClient_Fresh(t *testing.T) {
	assert := assert.New(t)

	d, err := os.MkdirTemp("", "")
	assert.NoError(err)
	defer os.RemoveAll(d)

	c := fakeDiscoveryClient{}
	cdc := newCachedDiscoveryClient(discovery.ToDiscoveryInterfaceWithContext(&c), d, 60*time.Second)
	assert.True(cdc.Fresh(), "should be fresh after creation")

	cdc.ServerGroups()
	assert.True(cdc.Fresh(), "should be fresh after groups call without cache")
	assert.Equal(1, c.groupCalls)

	cdc.ServerGroups()
	assert.True(cdc.Fresh(), "should be fresh after another groups call")
	assert.Equal(1, c.groupCalls)

	cdc.ServerGroupsAndResources()
	assert.True(cdc.Fresh(), "should be fresh after resources call")
	assert.Equal(1, c.resourceCalls)

	cdc.ServerGroupsAndResources()
	assert.True(cdc.Fresh(), "should be fresh after another resources call")
	assert.Equal(1, c.resourceCalls)

	cdc = newCachedDiscoveryClient(discovery.ToDiscoveryInterfaceWithContext(&c), d, 60*time.Second)
	cdc.ServerGroups()
	assert.False(cdc.Fresh(), "should NOT be fresh after recreation with existing groups cache")
	assert.Equal(1, c.groupCalls)

	cdc.ServerGroupsAndResources()
	assert.False(cdc.Fresh(), "should NOT be fresh after recreation with existing resources cache")
	assert.Equal(1, c.resourceCalls)

	cdc.Invalidate()
	assert.True(cdc.Fresh(), "should be fresh after cache invalidation")

	cdc.ServerGroupsAndResources()
	assert.True(cdc.Fresh(), "should ignore existing resources cache after invalidation")
	assert.Equal(2, c.resourceCalls)
}

func TestNewCachedDiscoveryClient_TTL(t *testing.T) {
	assert := assert.New(t)

	d, err := os.MkdirTemp("", "")
	assert.NoError(err)
	defer os.RemoveAll(d)

	c := fakeDiscoveryClient{}
	cdc := newCachedDiscoveryClient(discovery.ToDiscoveryInterfaceWithContext(&c), d, 1*time.Nanosecond)
	cdc.ServerGroups()
	assert.Equal(1, c.groupCalls)

	time.Sleep(1 * time.Second)

	cdc.ServerGroups()
	assert.Equal(2, c.groupCalls)
}

func TestNewCachedDiscoveryClient_PathPerm(t *testing.T) {
	assert := assert.New(t)

	d, err := os.MkdirTemp("", "")
	assert.NoError(err)
	os.RemoveAll(d)
	defer os.RemoveAll(d)

	c := fakeDiscoveryClient{}
	cdc := newCachedDiscoveryClient(discovery.ToDiscoveryInterfaceWithContext(&c), d, 1*time.Nanosecond)
	cdc.ServerGroups()

	err = filepath.Walk(d, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			assert.Equal(os.FileMode(0750), info.Mode().Perm())
		} else {
			assert.Equal(os.FileMode(0660), info.Mode().Perm())
		}
		return nil
	})
	assert.NoError(err)
}

// Tests that schema instances returned by openapi cached and returned after
// successive calls
func TestOpenAPIDiskCache(t *testing.T) {
	// Create discovery cache dir (unused)
	discoCache, err := os.MkdirTemp("", "test-cached-discovery-client-disco-*")
	require.NoError(t, err)
	os.RemoveAll(discoCache)
	defer os.RemoveAll(discoCache)

	// Create http cache dir
	httpCache, err := os.MkdirTemp("", "test-cached-discovery-client-http-*")
	require.NoError(t, err)
	os.RemoveAll(httpCache)
	defer os.RemoveAll(httpCache)

	// Start test OpenAPI server
	fakeServer, err := testutil.NewFakeOpenAPIV3Server("../../testdata")
	require.NoError(t, err)
	defer fakeServer.HttpServer.Close()

	require.NotEmpty(t, fakeServer.ServedDocuments)

	client, err := NewCachedDiscoveryClientForConfig(
		&restclient.Config{Host: fakeServer.HttpServer.URL},
		discoCache,
		httpCache,
		1*time.Nanosecond,
	)
	require.NoError(t, err)

	openapiClient := client.OpenAPIV3()

	// Ensure initial Paths call hits server
	_, err = openapiClient.Paths()
	require.NoError(t, err)
	assert.Equal(t, 1, fakeServer.RequestCounters["/openapi/v3"])

	// Ensure Paths call does hits server again
	// This is expected since openapiClient is the same instance, so Paths()
	// should be cached in memory.
	paths, err := openapiClient.Paths()
	require.NoError(t, err)
	assert.Equal(t, 1, fakeServer.RequestCounters["/openapi/v3"])
	require.NotEmpty(t, paths)

	contentTypes := []string{
		runtime.ContentTypeJSON, openapi.ContentTypeOpenAPIV3PB,
	}

	for _, contentType := range contentTypes {
		t.Run(contentType, func(t *testing.T) {
			// Reset all counters (cant just reset to nil since reference is shared)
			for k := range fakeServer.RequestCounters {
				delete(fakeServer.RequestCounters, k)
			}

			i := 0
			for k, v := range paths {
				i++

				_, err = v.Schema(contentType)
				assert.NoError(t, err)

				path := "/openapi/v3/" + strings.TrimPrefix(k, "/")
				assert.Equal(t, 1, fakeServer.RequestCounters[path])

				// Ensure schema call is served from memory
				_, err = v.Schema(contentType)
				assert.NoError(t, err)
				assert.Equal(t, 1, fakeServer.RequestCounters[path])

				client.Invalidate()

				// Refetch the schema from a new openapi client to try to force a new
				// http request
				newPaths, err := client.OpenAPIV3().Paths()
				if !assert.NoError(t, err) {
					continue
				}

				// Ensure schema call is still served from disk
				_, err = newPaths[k].Schema(contentType)
				assert.NoError(t, err)
				assert.Equal(t, 1, fakeServer.RequestCounters[path])
			}
		})
	}

}

// Tests function "ServerGroups" when the "unaggregated" discovery is returned.
func TestCachedDiscoveryClientUnaggregatedServerGroups(t *testing.T) {
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
		// Create discovery cache dir
		discoCache, err := os.MkdirTemp("", "test-cached-discovery-client-disco-*")
		require.NoError(t, err)
		os.RemoveAll(discoCache)
		defer os.RemoveAll(discoCache)
		// Create http cache dir (unused)
		httpCache, err := os.MkdirTemp("", "test-cached-discovery-client-http-*")
		require.NoError(t, err)
		os.RemoveAll(httpCache)
		defer os.RemoveAll(httpCache)
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
		client, err := NewCachedDiscoveryClientForConfig(
			&restclient.Config{Host: server.URL},
			discoCache,
			httpCache,
			1*time.Nanosecond,
		)
		require.NoError(t, err)
		apiGroupList, err := client.ServerGroups()
		require.NoError(t, err)
		// Discovery groups cached in servergroups.json file.
		numFound, err := numFilesFound(discoCache, "servergroups.json")
		assert.NoError(t, err)
		assert.Equal(t, 1, numFound,
			"%s: expected 1 discovery cache file servergroups.json found, got %d", test.name, numFound)
		// Test expected groups returned by server groups.
		expectedGroupNames := sets.NewString(test.expectedGroupNames...)
		actualGroupNames := sets.NewString(groupNamesFromList(apiGroupList)...)
		assert.True(t, expectedGroupNames.Equal(actualGroupNames),
			"%s: Expected groups (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
		// Test the expected group versions for the aggregated discovery is correct.
		expectedGroupVersions := sets.NewString(test.expectedGroupVersions...)
		actualGroupVersions := sets.NewString(groupVersionsFromGroups(apiGroupList)...)
		assert.True(t, expectedGroupVersions.Equal(actualGroupVersions),
			"%s: Expected group/versions (%s), got (%s)", test.name, expectedGroupVersions.List(), actualGroupVersions.List())
	}
}

// Aggregated discovery format returned
func TestCachedDiscoveryClientAggregatedServerGroups(t *testing.T) {
	tests := []struct {
		name                      string
		corev1                    *apidiscovery.APIGroupDiscoveryList
		apis                      *apidiscovery.APIGroupDiscoveryList
		expectedGroupNames        []string
		expectedGroupVersions     []string
		expectedPreferredVersions []string
	}{
		{
			name: "Aggregated cached discovery: 1 group/1 version at /api, 1 group/1 version at /apis",
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
		// Create discovery cache dir
		discoCache, err := os.MkdirTemp("", "test-cached-discovery-client-disco-*")
		require.NoError(t, err)
		os.RemoveAll(discoCache)
		defer os.RemoveAll(discoCache)
		// Create http cache dir (unused)
		httpCache, err := os.MkdirTemp("", "test-cached-discovery-client-http-*")
		require.NoError(t, err)
		os.RemoveAll(httpCache)
		defer os.RemoveAll(httpCache)
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
			if err != nil {
				t.Fatalf("unexpected encoding error: %v", err)
				return
			}
			// Content-type is "aggregated" discovery format.
			w.Header().Set("Content-Type", discovery.AcceptV2)
			w.WriteHeader(http.StatusOK)
			w.Write(output)
		}))
		defer server.Close()
		client, err := NewCachedDiscoveryClientForConfig(
			&restclient.Config{Host: server.URL},
			discoCache,
			httpCache,
			1*time.Nanosecond,
		)
		require.NoError(t, err)
		apiGroupList, err := client.ServerGroups()
		require.NoError(t, err)
		// Discovery groups cached in servergroups.json file.
		numFound, err := numFilesFound(discoCache, "servergroups.json")
		assert.NoError(t, err)
		assert.Equal(t, 1, numFound,
			"%s: expected 1 discovery cache file servergroups.json found, got %d", test.name, numFound)
		// Test expected groups returned by server groups.
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
	}
}

func numFilesFound(dir string, filename string) (int, error) {
	numFound := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Name() == filename {
			numFound++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return numFound, nil
}

type fakeDiscoveryClient struct {
	groupCalls    int
	resourceCalls int
	versionCalls  int
	openAPICalls  int

	serverResourcesHandler func() ([]*metav1.APIResourceList, error)
}

var _ discovery.DiscoveryInterface = &fakeDiscoveryClient{}

func (c *fakeDiscoveryClient) RESTClient() restclient.Interface {
	return &fake.RESTClient{}
}

func (c *fakeDiscoveryClient) ServerGroups() (*metav1.APIGroupList, error) {
	c.groupCalls = c.groupCalls + 1
	return c.serverGroups()
}

func (c *fakeDiscoveryClient) serverGroups() (*metav1.APIGroupList, error) {
	return &metav1.APIGroupList{
		Groups: []metav1.APIGroup{
			{
				Name: "a",
				Versions: []metav1.GroupVersionForDiscovery{
					{
						GroupVersion: "a/v1",
						Version:      "v1",
					},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "a/v1",
					Version:      "v1",
				},
			},
		},
	}, nil
}

func (c *fakeDiscoveryClient) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	c.resourceCalls = c.resourceCalls + 1
	if groupVersion == "a/v1" {
		return &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget"}}}, nil
	}

	return nil, errors.NewNotFound(schema.GroupResource{}, "")
}

func (c *fakeDiscoveryClient) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	c.resourceCalls = c.resourceCalls + 1

	gs, _ := c.serverGroups()
	resultGroups := []*metav1.APIGroup{}
	for i := range gs.Groups {
		resultGroups = append(resultGroups, &gs.Groups[i])
	}

	if c.serverResourcesHandler != nil {
		rs, err := c.serverResourcesHandler()
		return resultGroups, rs, err
	}
	return resultGroups, []*metav1.APIResourceList{}, nil
}

func (c *fakeDiscoveryClient) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	c.resourceCalls = c.resourceCalls + 1
	return nil, nil
}

func (c *fakeDiscoveryClient) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	c.resourceCalls = c.resourceCalls + 1
	return nil, nil
}

func (c *fakeDiscoveryClient) ServerVersion() (*version.Info, error) {
	c.versionCalls = c.versionCalls + 1
	return &version.Info{}, nil
}

func (c *fakeDiscoveryClient) OpenAPISchema() (*openapi_v2.Document, error) {
	c.openAPICalls = c.openAPICalls + 1
	return &openapi_v2.Document{}, nil
}

func (d *fakeDiscoveryClient) OpenAPIV3() openapi.Client {
	panic("unimplemented")
}

func (d *fakeDiscoveryClient) WithLegacy() discovery.DiscoveryInterface {
	panic("unimplemented")
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
