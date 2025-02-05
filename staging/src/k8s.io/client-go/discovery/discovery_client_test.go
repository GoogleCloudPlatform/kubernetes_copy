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

package discovery

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	openapi_v3 "github.com/google/gnostic-models/openapiv3"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	golangproto "google.golang.org/protobuf/proto"
	apidiscovery "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/openapi"
	restclient "k8s.io/client-go/rest"
	testutil "k8s.io/client-go/util/testing"
	"k8s.io/kube-openapi/pkg/spec3"
)

func TestGetServerVersion(t *testing.T) {
	expect := version.Info{
		Major:     "foo",
		Minor:     "bar",
		GitCommit: "baz",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		output, err := json.Marshal(expect)
		if err != nil {
			t.Errorf("unexpected encoding error: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(output)
		require.NoError(t, err)
	}))
	defer server.Close()
	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})

	got, err := client.ServerVersion()
	if err != nil {
		t.Fatalf("unexpected encoding error: %v", err)
	}
	if e, a := expect, *got; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestGetServerGroupsWithV1Server(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var obj interface{}
		switch req.URL.Path {
		case "/api":
			obj = &metav1.APIVersions{
				Versions: []string{
					"v1",
				},
			}
		case "/apis":
			obj = &metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "extensions",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "extensions/v1beta1"},
						},
					},
				},
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		output, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("unexpected encoding error: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(output)
		require.NoError(t, err)
	}))
	defer server.Close()
	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	apiGroupList, err := client.ServerGroups()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	groupVersions := metav1.ExtractGroupVersions(apiGroupList)
	if !reflect.DeepEqual(groupVersions, []string{"v1", "extensions/v1beta1"}) {
		t.Errorf("expected: %q, got: %q", []string{"v1", "extensions/v1beta1"}, groupVersions)
	}
}

func TestDiscoveryToleratesMissingCoreGroup(t *testing.T) {
	// Discovery tolerates 404 from /api. Aggregated api servers can do this.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var obj interface{}
		switch req.URL.Path {
		case "/api":
			w.WriteHeader(http.StatusNotFound)
		case "/apis":
			obj = &metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "extensions",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "extensions/v1beta1"},
						},
					},
				},
			}
		}
		output, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("unexpected encoding error: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(output)
		require.NoError(t, err)
	}))
	defer server.Close()
	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	// ServerGroups should not return an error even if server returns 404 at /api.
	apiGroupList, err := client.ServerGroups()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	groupVersions := metav1.ExtractGroupVersions(apiGroupList)
	if !reflect.DeepEqual(groupVersions, []string{"extensions/v1beta1"}) {
		t.Errorf("expected: %q, got: %q", []string{"extensions/v1beta1"}, groupVersions)
	}
}

func TestDiscoveryFailsWhenNonCoreGroupsMissing(t *testing.T) {
	// Discovery fails when /apis returns 404.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var obj interface{}
		switch req.URL.Path {
		case "/api":
			obj = &metav1.APIVersions{
				Versions: []string{
					"v1",
				},
			}
		case "/apis":
			w.WriteHeader(http.StatusNotFound)
		}
		output, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("unexpected encoding error: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(output)
		require.NoError(t, err)
	}))
	defer server.Close()
	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	_, err := client.ServerGroups()
	if err == nil {
		t.Fatal("expected error, received none")
	}
}

func TestGetServerGroupsWithBrokenServer(t *testing.T) {
	// 404 Not Found errors because discovery at /apis returns an error.
	// 403 Forbidden errors because discovery at both /api and /apis returns error.
	for _, statusCode := range []int{http.StatusNotFound, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(statusCode)
		}))
		defer server.Close()
		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		_, err := client.ServerGroups()
		if err == nil {
			t.Fatal("expected error, received none")
		}
	}
}

func TestTimeoutIsSet(t *testing.T) {
	cfg := &restclient.Config{}
	setDiscoveryDefaults(cfg)
	assert.Equal(t, defaultTimeout, cfg.Timeout)
}

func TestGetServerResourcesForGroupVersion(t *testing.T) {
	stable := metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "pods", Namespaced: true, Kind: "Pod"},
			{Name: "services", Namespaced: true, Kind: "Service"},
			{Name: "namespaces", Namespaced: false, Kind: "Namespace"},
		},
	}
	beta := metav1.APIResourceList{
		GroupVersion: "extensions/v1beta1",
		APIResources: []metav1.APIResource{
			{Name: "deployments", Namespaced: true, Kind: "Deployment"},
			{Name: "ingresses", Namespaced: true, Kind: "Ingress"},
			{Name: "jobs", Namespaced: true, Kind: "Job"},
		},
	}
	beta2 := metav1.APIResourceList{
		GroupVersion: "extensions/v1beta2",
		APIResources: []metav1.APIResource{
			{Name: "deployments", Namespaced: true, Kind: "Deployment"},
			{Name: "ingresses", Namespaced: true, Kind: "Ingress"},
			{Name: "jobs", Namespaced: true, Kind: "Job"},
		},
	}
	extensionsbeta3 := metav1.APIResourceList{GroupVersion: "extensions/v1beta3", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	extensionsbeta4 := metav1.APIResourceList{GroupVersion: "extensions/v1beta4", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	extensionsbeta5 := metav1.APIResourceList{GroupVersion: "extensions/v1beta5", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	extensionsbeta6 := metav1.APIResourceList{GroupVersion: "extensions/v1beta6", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	extensionsbeta7 := metav1.APIResourceList{GroupVersion: "extensions/v1beta7", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	extensionsbeta8 := metav1.APIResourceList{GroupVersion: "extensions/v1beta8", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	extensionsbeta9 := metav1.APIResourceList{GroupVersion: "extensions/v1beta9", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	extensionsbeta10 := metav1.APIResourceList{GroupVersion: "extensions/v1beta10", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}

	appsbeta1 := metav1.APIResourceList{GroupVersion: "apps/v1beta1", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta2 := metav1.APIResourceList{GroupVersion: "apps/v1beta2", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta3 := metav1.APIResourceList{GroupVersion: "apps/v1beta3", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta4 := metav1.APIResourceList{GroupVersion: "apps/v1beta4", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta5 := metav1.APIResourceList{GroupVersion: "apps/v1beta5", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta6 := metav1.APIResourceList{GroupVersion: "apps/v1beta6", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta7 := metav1.APIResourceList{GroupVersion: "apps/v1beta7", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta8 := metav1.APIResourceList{GroupVersion: "apps/v1beta8", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta9 := metav1.APIResourceList{GroupVersion: "apps/v1beta9", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}
	appsbeta10 := metav1.APIResourceList{GroupVersion: "apps/v1beta10", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}}

	tests := []struct {
		resourcesList *metav1.APIResourceList
		path          string
		request       string
		expectErr     bool
	}{
		{
			resourcesList: &stable,
			path:          "/api/v1",
			request:       "v1",
			expectErr:     false,
		},
		{
			resourcesList: &beta,
			path:          "/apis/extensions/v1beta1",
			request:       "extensions/v1beta1",
			expectErr:     false,
		},
		{
			resourcesList: &stable,
			path:          "/api/v1",
			request:       "foobar",
			expectErr:     true,
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var list interface{}
		switch req.URL.Path {
		case "/api/v1":
			list = &stable
		case "/apis/extensions/v1beta1":
			list = &beta
		case "/apis/extensions/v1beta2":
			list = &beta2
		case "/apis/extensions/v1beta3":
			list = &extensionsbeta3
		case "/apis/extensions/v1beta4":
			list = &extensionsbeta4
		case "/apis/extensions/v1beta5":
			list = &extensionsbeta5
		case "/apis/extensions/v1beta6":
			list = &extensionsbeta6
		case "/apis/extensions/v1beta7":
			list = &extensionsbeta7
		case "/apis/extensions/v1beta8":
			list = &extensionsbeta8
		case "/apis/extensions/v1beta9":
			list = &extensionsbeta9
		case "/apis/extensions/v1beta10":
			list = &extensionsbeta10
		case "/apis/apps/v1beta1":
			list = &appsbeta1
		case "/apis/apps/v1beta2":
			list = &appsbeta2
		case "/apis/apps/v1beta3":
			list = &appsbeta3
		case "/apis/apps/v1beta4":
			list = &appsbeta4
		case "/apis/apps/v1beta5":
			list = &appsbeta5
		case "/apis/apps/v1beta6":
			list = &appsbeta6
		case "/apis/apps/v1beta7":
			list = &appsbeta7
		case "/apis/apps/v1beta8":
			list = &appsbeta8
		case "/apis/apps/v1beta9":
			list = &appsbeta9
		case "/apis/apps/v1beta10":
			list = &appsbeta10
		case "/api":
			list = &metav1.APIVersions{
				Versions: []string{
					"v1",
				},
			}
		case "/apis":
			list = &metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "apps",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "apps/v1beta1", Version: "v1beta1"},
							{GroupVersion: "apps/v1beta2", Version: "v1beta2"},
							{GroupVersion: "apps/v1beta3", Version: "v1beta3"},
							{GroupVersion: "apps/v1beta4", Version: "v1beta4"},
							{GroupVersion: "apps/v1beta5", Version: "v1beta5"},
							{GroupVersion: "apps/v1beta6", Version: "v1beta6"},
							{GroupVersion: "apps/v1beta7", Version: "v1beta7"},
							{GroupVersion: "apps/v1beta8", Version: "v1beta8"},
							{GroupVersion: "apps/v1beta9", Version: "v1beta9"},
							{GroupVersion: "apps/v1beta10", Version: "v1beta10"},
						},
					},
					{
						Name: "extensions",
						Versions: []metav1.GroupVersionForDiscovery{
							{GroupVersion: "extensions/v1beta1", Version: "v1beta1"},
							{GroupVersion: "extensions/v1beta2", Version: "v1beta2"},
							{GroupVersion: "extensions/v1beta3", Version: "v1beta3"},
							{GroupVersion: "extensions/v1beta4", Version: "v1beta4"},
							{GroupVersion: "extensions/v1beta5", Version: "v1beta5"},
							{GroupVersion: "extensions/v1beta6", Version: "v1beta6"},
							{GroupVersion: "extensions/v1beta7", Version: "v1beta7"},
							{GroupVersion: "extensions/v1beta8", Version: "v1beta8"},
							{GroupVersion: "extensions/v1beta9", Version: "v1beta9"},
							{GroupVersion: "extensions/v1beta10", Version: "v1beta10"},
						},
					},
				},
			}
		default:
			t.Logf("unexpected request: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		output, err := json.Marshal(list)
		if err != nil {
			t.Errorf("unexpected encoding error: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(output)
		require.NoError(t, err)
	}))
	defer server.Close()
	for _, test := range tests {
		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		got, err := client.ServerResourcesForGroupVersion(test.request)
		if test.expectErr {
			if err == nil {
				t.Error("unexpected non-error")
			}
			continue
		}
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			continue
		}
		if !reflect.DeepEqual(got, test.resourcesList) {
			t.Errorf("expected:\n%v\ngot:\n%v\n", test.resourcesList, got)
		}
	}

	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	start := time.Now()
	_, serverResources, err := client.ServerGroupsAndResources()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	end := time.Now()
	if d := end.Sub(start); d > time.Second {
		t.Errorf("took too long to perform discovery: %s", d)
	}
	serverGroupVersions := groupVersions(serverResources)
	expectedGroupVersions := []string{
		"v1",
		"apps/v1beta1",
		"apps/v1beta2",
		"apps/v1beta3",
		"apps/v1beta4",
		"apps/v1beta5",
		"apps/v1beta6",
		"apps/v1beta7",
		"apps/v1beta8",
		"apps/v1beta9",
		"apps/v1beta10",
		"extensions/v1beta1",
		"extensions/v1beta2",
		"extensions/v1beta3",
		"extensions/v1beta4",
		"extensions/v1beta5",
		"extensions/v1beta6",
		"extensions/v1beta7",
		"extensions/v1beta8",
		"extensions/v1beta9",
		"extensions/v1beta10",
	}
	if !reflect.DeepEqual(expectedGroupVersions, serverGroupVersions) {
		t.Errorf("unexpected group versions: %v", cmp.Diff(expectedGroupVersions, serverGroupVersions))
	}
}

func returnedOpenAPI() *openapi_v2.Document {
	return &openapi_v2.Document{
		Definitions: &openapi_v2.Definitions{
			AdditionalProperties: []*openapi_v2.NamedSchema{
				{
					Name: "fake.type.1",
					Value: &openapi_v2.Schema{
						Properties: &openapi_v2.Properties{
							AdditionalProperties: []*openapi_v2.NamedSchema{
								{
									Name: "count",
									Value: &openapi_v2.Schema{
										Type: &openapi_v2.TypeItem{
											Value: []string{"integer"},
										},
									},
								},
							},
						},
					},
				},
				{
					Name: "fake.type.2",
					Value: &openapi_v2.Schema{
						Properties: &openapi_v2.Properties{
							AdditionalProperties: []*openapi_v2.NamedSchema{
								{
									Name: "count",
									Value: &openapi_v2.Schema{
										Type: &openapi_v2.TypeItem{
											Value: []string{"array"},
										},
										Items: &openapi_v2.ItemsItem{
											Schema: []*openapi_v2.Schema{
												{
													Type: &openapi_v2.TypeItem{
														Value: []string{"string"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func openapiSchemaFakeServer(t *testing.T) (*httptest.Server, error) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/openapi/v2" {
			errMsg := fmt.Sprintf("Unexpected url %v", req.URL)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(errMsg))
			t.Errorf("testing should fail as %s", errMsg)
			return
		}
		if req.Method != "GET" {
			errMsg := fmt.Sprintf("Unexpected method %v", req.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(errMsg))
			t.Errorf("testing should fail as %s", errMsg)
			return
		}
		decipherableFormat := req.Header.Get("Accept")
		if decipherableFormat != "application/com.github.proto-openapi.spec.v2@v1.0+protobuf" {
			errMsg := fmt.Sprintf("Unexpected accept mime type %v", decipherableFormat)
			w.WriteHeader(http.StatusUnsupportedMediaType)
			w.Write([]byte(errMsg))
			t.Errorf("testing should fail as %s", errMsg)
			return
		}

		output, err := proto.Marshal(returnedOpenAPI())
		if err != nil {
			errMsg := fmt.Sprintf("Unexpected marshal error: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errMsg))
			t.Errorf("testing should fail as %s", errMsg)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(output)
	}))

	return server, nil
}

func openapiV3SchemaFakeServer(t *testing.T) (*httptest.Server, map[string]*spec3.OpenAPI, error) {
	res, err := testutil.NewFakeOpenAPIV3Server("testdata")
	if err != nil {
		return nil, nil, err
	}
	return res.HttpServer, res.ServedDocuments, nil
}

func TestGetOpenAPISchema(t *testing.T) {
	server, err := openapiSchemaFakeServer(t)
	if err != nil {
		t.Errorf("unexpected error starting fake server: %v", err)
	}
	defer server.Close()

	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	got, err := client.OpenAPISchema()
	if err != nil {
		t.Fatalf("unexpected error getting openapi: %v", err)
	}
	if e, a := returnedOpenAPI(), got; !golangproto.Equal(e, a) {
		t.Errorf("expected \n%v, got \n%v", e, a)
	}
}

func TestGetOpenAPISchemaV3(t *testing.T) {
	server, testV3Specs, err := openapiV3SchemaFakeServer(t)
	if err != nil {
		t.Errorf("unexpected error starting fake server: %v", err)
	}
	defer server.Close()

	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	openapiClient := client.OpenAPIV3()
	paths, err := openapiClient.Paths()
	if err != nil {
		t.Fatalf("unexpected error getting openapi: %v", err)
	}

	contentTypes := []string{
		runtime.ContentTypeJSON, openapi.ContentTypeOpenAPIV3PB,
	}

	for _, contentType := range contentTypes {
		t.Run(contentType, func(t *testing.T) {
			for k, v := range paths {
				actual, err := v.Schema(contentType)
				if err != nil {
					t.Fatal(err)
				}

				expected := testV3Specs[k]
				switch contentType {

				case runtime.ContentTypeJSON:
					var actualSpec spec3.OpenAPI

					if err := json.Unmarshal(actual, &actualSpec); err != nil {
						t.Fatal(err)
					}

					// Cannot use DeepEqual directly due to differences in how
					// default key is being handled in gnostic vs kube-openapi
					// Our test server parses the files in directly as gnostic
					// which retains empty maps/lists, etc.
					require.EqualValues(t, expected, &actualSpec)
				case openapi.ContentTypeOpenAPIV3PB:
					// Convert to JSON then to gnostic then to PB for comparison
					expectedJSON, err := json.Marshal(expected)
					if err != nil {
						t.Fatal(err)
					}

					expectedGnostic, err := openapi_v3.ParseDocument(expectedJSON)
					if err != nil {
						t.Fatal(err)
					}

					expectedPB, err := golangproto.Marshal(expectedGnostic)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(expectedPB, actual) {
						t.Fatalf("expected equal values: %v", cmp.Diff(expectedPB, actual))
					}
				default:
					panic(fmt.Errorf("unrecognized content type: %v", contentType))
				}

				// Ensure that fetching schema once again does not return same instance
				actualAgain, err := v.Schema(contentType)
				if err != nil {
					t.Fatal(err)
				}

				if reflect.ValueOf(actual).Pointer() == reflect.ValueOf(actualAgain).Pointer() {
					t.Fatal("expected schema not to be cached")
				}
			}

		})
	}
}

func TestServerPreferredResources(t *testing.T) {
	stable := metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "pods", Namespaced: true, Kind: "Pod"},
			{Name: "services", Namespaced: true, Kind: "Service"},
			{Name: "namespaces", Namespaced: false, Kind: "Namespace"},
		},
	}
	tests := []struct {
		resourcesList []*metav1.APIResourceList
		response      func(w http.ResponseWriter, req *http.Request)
		expectErr     func(err error) bool
	}{
		{
			resourcesList: []*metav1.APIResourceList{&stable},
			expectErr:     IsGroupDiscoveryFailedError,
			response: func(w http.ResponseWriter, req *http.Request) {
				var list interface{}
				switch req.URL.Path {
				case "/apis/extensions/v1beta1":
					w.WriteHeader(http.StatusInternalServerError)
					return
				case "/api/v1":
					list = &stable
				case "/api":
					list = &metav1.APIVersions{
						Versions: []string{
							"v1",
						},
					}
				case "/apis":
					list = &metav1.APIGroupList{
						Groups: []metav1.APIGroup{
							{
								Versions: []metav1.GroupVersionForDiscovery{
									{GroupVersion: "extensions/v1beta1"},
								},
							},
						},
					}
				default:
					t.Logf("unexpected request: %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				output, err := json.Marshal(list)
				if err != nil {
					t.Errorf("unexpected encoding error: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(output)
			},
		},
		{
			resourcesList: nil,
			expectErr:     IsGroupDiscoveryFailedError,
			response: func(w http.ResponseWriter, req *http.Request) {
				var list interface{}
				switch req.URL.Path {
				case "/apis/extensions/v1beta1":
					w.WriteHeader(http.StatusInternalServerError)
					return
				case "/api/v1":
					w.WriteHeader(http.StatusInternalServerError)
				case "/api":
					list = &metav1.APIVersions{
						Versions: []string{
							"v1",
						},
					}
				case "/apis":
					list = &metav1.APIGroupList{
						Groups: []metav1.APIGroup{
							{
								Versions: []metav1.GroupVersionForDiscovery{
									{GroupVersion: "extensions/v1beta1"},
								},
							},
						},
					}
				default:
					t.Logf("unexpected request: %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				output, err := json.Marshal(list)
				if err != nil {
					t.Errorf("unexpected encoding error: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(output)
			},
		},
	}
	for _, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(test.response))
		defer server.Close()

		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		resources, err := client.ServerPreferredResources()
		if test.expectErr != nil {
			if err == nil {
				t.Error("unexpected non-error")
			}

			continue
		}
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			continue
		}
		got, err := GroupVersionResources(resources)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			continue
		}
		expected, _ := GroupVersionResources(test.resourcesList)
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("expected:\n%v\ngot:\n%v\n", test.resourcesList, got)
		}
		server.Close()
	}
}

func TestServerPreferredResourcesRetries(t *testing.T) {
	stable := metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "pods", Namespaced: true, Kind: "Pod"},
		},
	}
	beta := metav1.APIResourceList{
		GroupVersion: "extensions/v1",
		APIResources: []metav1.APIResource{
			{Name: "deployments", Namespaced: true, Kind: "Deployment"},
		},
	}

	response := func(numErrors int) http.HandlerFunc {
		var i = 0
		return func(w http.ResponseWriter, req *http.Request) {
			var list interface{}
			switch req.URL.Path {
			case "/apis/extensions/v1beta1":
				if i < numErrors {
					i++
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				list = &beta
			case "/api/v1":
				list = &stable
			case "/api":
				list = &metav1.APIVersions{
					Versions: []string{
						"v1",
					},
				}
			case "/apis":
				list = &metav1.APIGroupList{
					Groups: []metav1.APIGroup{
						{
							Name: "extensions",
							Versions: []metav1.GroupVersionForDiscovery{
								{GroupVersion: "extensions/v1beta1", Version: "v1beta1"},
							},
							PreferredVersion: metav1.GroupVersionForDiscovery{
								GroupVersion: "extensions/v1beta1",
								Version:      "v1beta1",
							},
						},
					},
				}
			default:
				t.Logf("unexpected request: %s", req.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			output, err := json.Marshal(list)
			if err != nil {
				t.Errorf("unexpected encoding error: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(output)
		}
	}
	tests := []struct {
		responseErrors  int
		expectResources int
		expectedError   func(err error) bool
	}{
		{
			responseErrors:  1,
			expectResources: 2,
			expectedError: func(err error) bool {
				return err == nil
			},
		},
		{
			responseErrors:  2,
			expectResources: 1,
			expectedError:   IsGroupDiscoveryFailedError,
		},
	}

	for i, tc := range tests {
		server := httptest.NewServer(http.HandlerFunc(response(tc.responseErrors)))
		defer server.Close()

		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		resources, err := client.ServerPreferredResources()
		if !tc.expectedError(err) {
			t.Errorf("case %d: unexpected error: %v", i, err)
		}
		got, err := GroupVersionResources(resources)
		if err != nil {
			t.Errorf("case %d: unexpected error: %v", i, err)
		}
		if len(got) != tc.expectResources {
			t.Errorf("case %d: expect %d resources, got %#v", i, tc.expectResources, got)
		}
		server.Close()
	}
}

func TestServerPreferredNamespacedResources(t *testing.T) {
	stable := metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "pods", Namespaced: true, Kind: "Pod"},
			{Name: "services", Namespaced: true, Kind: "Service"},
			{Name: "namespaces", Namespaced: false, Kind: "Namespace"},
		},
	}
	batchv1 := metav1.APIResourceList{
		GroupVersion: "batch/v1",
		APIResources: []metav1.APIResource{
			{Name: "jobs", Namespaced: true, Kind: "Job"},
		},
	}
	batchv2alpha1 := metav1.APIResourceList{
		GroupVersion: "batch/v2alpha1",
		APIResources: []metav1.APIResource{
			{Name: "jobs", Namespaced: true, Kind: "Job"},
			{Name: "cronjobs", Namespaced: true, Kind: "CronJob"},
		},
	}
	batchv3alpha1 := metav1.APIResourceList{
		GroupVersion: "batch/v3alpha1",
		APIResources: []metav1.APIResource{
			{Name: "jobs", Namespaced: true, Kind: "Job"},
			{Name: "cronjobs", Namespaced: true, Kind: "CronJob"},
		},
	}
	tests := []struct {
		response func(w http.ResponseWriter, req *http.Request)
		expected map[schema.GroupVersionResource]struct{}
	}{
		{
			// Combines discovery for /api and /apis.
			response: func(w http.ResponseWriter, req *http.Request) {
				var list interface{}
				switch req.URL.Path {
				case "/api":
					list = &metav1.APIVersions{
						Versions: []string{
							"v1",
						},
					}
				case "/api/v1":
					list = &stable
				case "/apis":
					list = &metav1.APIGroupList{
						Groups: []metav1.APIGroup{
							{
								Name: "batch",
								Versions: []metav1.GroupVersionForDiscovery{
									{GroupVersion: "batch/v1", Version: "v1"},
								},
								PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "batch/v1", Version: "v1"},
							},
						},
					}
				case "/apis/batch/v1":
					list = &batchv1

				default:
					t.Logf("unexpected request: %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				output, err := json.Marshal(list)
				if err != nil {
					t.Errorf("unexpected encoding error: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(output)
			},
			expected: map[schema.GroupVersionResource]struct{}{
				{Group: "", Version: "v1", Resource: "pods"}:      {},
				{Group: "", Version: "v1", Resource: "services"}:  {},
				{Group: "batch", Version: "v1", Resource: "jobs"}: {},
			},
		},
		{
			// Only return /apis (not legacy /api); does not error. 404 for legacy
			// core/v1 at /api is tolerated.
			response: func(w http.ResponseWriter, req *http.Request) {
				var list interface{}
				switch req.URL.Path {
				case "/apis":
					list = &metav1.APIGroupList{
						Groups: []metav1.APIGroup{
							{
								Name: "batch",
								Versions: []metav1.GroupVersionForDiscovery{
									{GroupVersion: "batch/v1", Version: "v1"},
									{GroupVersion: "batch/v2alpha1", Version: "v2alpha1"},
									{GroupVersion: "batch/v3alpha1", Version: "v3alpha1"},
								},
								PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "batch/v1", Version: "v1"},
							},
						},
					}
				case "/apis/batch/v1":
					list = &batchv1
				case "/apis/batch/v2alpha1":
					list = &batchv2alpha1
				case "/apis/batch/v3alpha1":
					list = &batchv3alpha1
				default:
					t.Logf("unexpected request: %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				output, err := json.Marshal(list)
				if err != nil {
					t.Errorf("unexpected encoding error: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(output)
			},
			expected: map[schema.GroupVersionResource]struct{}{
				{Group: "batch", Version: "v1", Resource: "jobs"}:           {},
				{Group: "batch", Version: "v2alpha1", Resource: "cronjobs"}: {},
			},
		},
		{
			response: func(w http.ResponseWriter, req *http.Request) {
				var list interface{}
				switch req.URL.Path {
				case "/apis":
					list = &metav1.APIGroupList{
						Groups: []metav1.APIGroup{
							{
								Name: "batch",
								Versions: []metav1.GroupVersionForDiscovery{
									{GroupVersion: "batch/v1", Version: "v1"},
									{GroupVersion: "batch/v2alpha1", Version: "v2alpha1"},
									{GroupVersion: "batch/v3alpha1", Version: "v3alpha1"},
								},
								PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "batch/v2alpha", Version: "v2alpha1"},
							},
						},
					}
				case "/apis/batch/v1":
					list = &batchv1
				case "/apis/batch/v2alpha1":
					list = &batchv2alpha1
				case "/apis/batch/v3alpha1":
					list = &batchv3alpha1
				default:
					t.Logf("unexpected request: %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				output, err := json.Marshal(list)
				if err != nil {
					t.Errorf("unexpected encoding error: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(output)
			},
			expected: map[schema.GroupVersionResource]struct{}{
				{Group: "batch", Version: "v2alpha1", Resource: "jobs"}:     {},
				{Group: "batch", Version: "v2alpha1", Resource: "cronjobs"}: {},
			},
		},
	}
	for i, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(test.response))
		defer server.Close()

		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		resources, err := client.ServerPreferredNamespacedResources()
		if err != nil {
			t.Errorf("[%d] unexpected error: %v", i, err)
			continue
		}
		got, err := GroupVersionResources(resources)
		if err != nil {
			t.Errorf("[%d] unexpected error: %v", i, err)
			continue
		}

		if !reflect.DeepEqual(got, test.expected) {
			t.Errorf("[%d] expected:\n%v\ngot:\n%v\n", i, test.expected, got)
		}
		server.Close()
	}
}

// Tests of the aggregated discovery format.
func TestAggregatedServerGroups(t *testing.T) {
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
			var output []byte
			var err error
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
			output, err = json.Marshal(agg)
			require.NoError(t, err)
			// Content-Type is "aggregated" discovery format. Add extra parameter
			// to ensure we are resilient to these extra parameters.
			w.Header().Set("Content-Type", AcceptV2+"; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, err = w.Write(output)
			require.NoError(t, err)
		}))
		defer server.Close()
		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		apiGroupList, err := client.ServerGroups()
		require.NoError(t, err)
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
	}
}

func TestAggregatedServerGroupsAndResources(t *testing.T) {
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
		},
		{
			name: "Aggregated discovery: 1 group/1 resources at /api, 1 group/2 versions/1 resources at /apis",
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
						},
					},
				},
			},
			expectedGroupNames:    []string{"", "apps"},
			expectedGroupVersions: []string{"v1", "apps/v1", "apps/v2"},
			expectedGVKs: []string{
				"/v1/Pod",
				"apps/v1/Deployment",
				"apps/v2/Deployment",
			},
		},
		{
			name: "Aggregated discovery: 1 group/1 resources at /api, 1 group/2 versions/1 resources at /apis",
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
							// Stale "v2" version not included.
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
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v2",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
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
			expectedFailedGVs: []string{"apps/v2"},
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
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "batch",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							// Stale Group/Version is not included
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
								Freshness: apidiscovery.DiscoveryFreshnessStale,
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
			expectedGroupNames:    []string{"", "apps", "batch"},
			expectedGroupVersions: []string{"v1", "apps/v1", "batch/v1beta1"},
			expectedGVKs: []string{
				"/v1/Pod",
				"/v1/Service",
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
				"batch/v1beta1/Job",
				"batch/v1beta1/CronJob",
			},
			expectedFailedGVs: []string{"batch/v1"},
		},
		{
			name:   "Aggregated discovery: /api returns nothing, 2 groups/2 resources at /apis",
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
								// Stale "v1beta1" not included.
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
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
						},
					},
				},
			},
			expectedGroupNames:    []string{"apps", "batch"},
			expectedGroupVersions: []string{"apps/v1", "batch/v1"},
			expectedGVKs: []string{
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
				"batch/v1/Job",
				"batch/v1/CronJob",
			},
			expectedFailedGVs: []string{"batch/v1beta1"},
		},
	}

	serverAccepts := []string{AcceptV2}
	for _, test := range tests {
		for _, accept := range serverAccepts {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var output []byte
				var err error
				if accept == AcceptV2 {
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
					output, err = json.Marshal(agg)
					require.NoError(t, err)
				}
				// Content-Type is "aggregated" discovery format. Add extra parameter
				// to ensure we are resilient to these extra parameters.
				w.Header().Set("Content-Type", accept+"; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, err = w.Write(output)
				require.NoError(t, err)

			}))
			defer server.Close()
			client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
			apiGroups, resources, err := client.ServerGroupsAndResources()
			if len(test.expectedFailedGVs) > 0 {
				require.Error(t, err)
				expectedFailedGVs := sets.NewString(test.expectedFailedGVs...)
				actualFailedGVs := sets.NewString(failedGroupVersions(err)...)
				assert.True(t, expectedFailedGVs.Equal(actualFailedGVs),
					"%s: Expected Failed GVs (%s), got (%s)", test.name, expectedFailedGVs, actualFailedGVs)
			} else {
				require.NoError(t, err)
			}
			// Test the expected groups are returned for the aggregated format.
			expectedGroupNames := sets.NewString(test.expectedGroupNames...)
			actualGroupNames := sets.NewString(groupNames(apiGroups)...)
			assert.True(t, expectedGroupNames.Equal(actualGroupNames),
				"%s: Expected GVKs (%s), got (%s)", test.name, expectedGroupNames.List(), actualGroupNames.List())
			// If the core V1 group is returned from /api, it should be the first group.
			if expectedGroupNames.Has("") {
				assert.NotEmpty(t, apiGroups)
				actualFirstGroup := apiGroups[0]
				assert.NotEmpty(t, actualFirstGroup.Versions)
				actualFirstGroupVersion := actualFirstGroup.Versions[0].GroupVersion
				assert.Equal(t, "v1", actualFirstGroupVersion)
			}
			// Test the expected group/versions are returned from the aggregated discovery.
			expectedGroupVersions := sets.NewString(test.expectedGroupVersions...)
			actualGroupVersions := sets.NewString(groupVersions(resources)...)
			assert.True(t, expectedGroupVersions.Equal(actualGroupVersions),
				"%s: Expected GroupVersions(%s), got (%s)", test.name, expectedGroupVersions.List(), actualGroupVersions.List())
			// Test the expected GVKs are returned from the aggregated discovery.
			expectedGVKs := sets.NewString(test.expectedGVKs...)
			actualGVKs := sets.NewString(groupVersionKinds(resources)...)
			assert.True(t, expectedGVKs.Equal(actualGVKs),
				"%s: Expected GVKs (%s), got (%s)", test.name, expectedGVKs.List(), actualGVKs.List())
		}
	}
}

func TestAggregatedServerGroupsAndResourcesWithErrors(t *testing.T) {
	tests := []struct {
		name              string
		corev1            *apidiscovery.APIGroupDiscoveryList
		coreHttpStatus    int
		apis              *apidiscovery.APIGroupDiscoveryList
		apisHttpStatus    int
		expectedGroups    []string
		expectedResources []string
		expectedErr       bool
	}{
		{
			name:           "Aggregated Discovery: 404 for core/v1 is tolerated",
			corev1:         &apidiscovery.APIGroupDiscoveryList{},
			coreHttpStatus: http.StatusNotFound,
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
										Resource: "daemonsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "DaemonSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			apisHttpStatus:    http.StatusOK,
			expectedGroups:    []string{"apps"},
			expectedResources: []string{"apps/v1/Deployment", "apps/v1/DaemonSet"},
			expectedErr:       false,
		},
		{
			name:           "Aggregated Discovery: 403 for core/v1 causes error",
			corev1:         &apidiscovery.APIGroupDiscoveryList{},
			coreHttpStatus: http.StatusForbidden,
			apis:           &apidiscovery.APIGroupDiscoveryList{},
			apisHttpStatus: http.StatusOK,
			expectedErr:    true,
		},
		{
			name:           "Aggregated Discovery: 404 for /apis causes error",
			corev1:         &apidiscovery.APIGroupDiscoveryList{},
			coreHttpStatus: http.StatusOK,
			apis:           &apidiscovery.APIGroupDiscoveryList{},
			apisHttpStatus: http.StatusNotFound,
			expectedErr:    true,
		},
		{
			name:           "Aggregated Discovery: 403 for /apis causes error",
			corev1:         &apidiscovery.APIGroupDiscoveryList{},
			coreHttpStatus: http.StatusOK,
			apis:           &apidiscovery.APIGroupDiscoveryList{},
			apisHttpStatus: http.StatusForbidden,
			expectedErr:    true,
		},
	}

	for _, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var output []byte
			var err error
			var status int
			var agg *apidiscovery.APIGroupDiscoveryList
			switch req.URL.Path {
			case "/api":
				agg = test.corev1
				status = test.coreHttpStatus
			case "/apis":
				agg = test.apis
				status = test.apisHttpStatus
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			output, err = json.Marshal(agg)
			require.NoError(t, err)
			// Content-Type is "aggregated" discovery format. Add extra parameter
			// to ensure we are resilient to these extra parameters.
			w.Header().Set("Content-Type", AcceptV2+"; charset=utf-8")
			w.WriteHeader(status)
			_, err = w.Write(output)
			require.NoError(t, err)
		}))
		defer server.Close()

		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		apiGroups, resources, err := client.ServerGroupsAndResources()
		if test.expectedErr {
			require.Error(t, err)
			require.Nil(t, apiGroups)
			require.Nil(t, resources)
			continue
		}
		require.NoError(t, err)
		// First check the returned groups
		expectedGroups := sets.NewString(test.expectedGroups...)
		actualGroups := sets.NewString(groupNames(apiGroups)...)
		assert.True(t, expectedGroups.Equal(actualGroups),
			"%s: Expected GVKs (%s), got (%s)", test.name, expectedGroups.List(), actualGroups.List())
		// Next check the returned resources
		expectedGVKs := sets.NewString(test.expectedResources...)
		actualGVKs := sets.NewString(groupVersionKinds(resources)...)
		assert.True(t, expectedGVKs.Equal(actualGVKs),
			"%s: Expected GVKs (%s), got (%s)", test.name, expectedGVKs.List(), actualGVKs.List())
	}
}

func TestAggregatedServerPreferredResources(t *testing.T) {
	tests := []struct {
		name              string
		corev1            *apidiscovery.APIGroupDiscoveryList
		apis              *apidiscovery.APIGroupDiscoveryList
		expectedGVKs      []string
		expectedFailedGVs []string
	}{
		{
			name: "Aggregated discovery: basic corev1 and apps/v1 preferred resources returned",
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
			expectedGVKs: []string{
				"/v1/Pod",
				"apps/v1/Deployment",
			},
		},
		{
			name: "Aggregated discovery: only resources from preferred apps/v2 group/version",
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
							// v2 is "preferred version since it is first
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
			// Only v2 resources from apps group, since v2 is preferred version.
			expectedGVKs: []string{
				"/v1/Pod",
				"apps/v2/Deployment",
			},
		},
		{
			name: "Aggregated discovery: stale Group/Version can not produce preferred version",
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
							// v2 is "stale", so it can not be "preferred".
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
								Freshness: apidiscovery.DiscoveryFreshnessStale,
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
			// Only v1 resources from apps group; v2 would be preferred but it is "stale".
			expectedGVKs: []string{
				"/v1/Pod",
				"apps/v1/Deployment",
			},
			expectedFailedGVs: []string{"apps/v2"},
		},
		{
			name: "Aggregated discovery: preferred multiple resources from multiple group/versions",
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
				},
			},
			expectedGVKs: []string{
				"/v1/Pod",
				"/v1/Service",
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
			},
			expectedFailedGVs: []string{"apps/v1beta1"},
		},
		{
			name: "Aggregated discovery: resources from multiple preferred group versions at /apis",
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
								// Not included because "v1" is preferred.
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
						},
					},
				},
			},
			expectedGVKs: []string{
				"/v1/Pod",
				"/v1/Service",
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
				"batch/v1/Job",
				"batch/v1/CronJob",
			},
		},
		{
			name: "Aggregated discovery: resources from only preferred group versions for batch group",
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
								// Not included, since "v1" is preferred.
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
			// Only preferred resources expected--not batch/v1beta1 resources.
			expectedGVKs: []string{
				"/v1/Pod",
				"/v1/Service",
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
				"batch/v1/Job",
				"batch/v1/CronJob",
			},
		},
	}

	for _, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var output []byte
			var err error
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
			output, err = json.Marshal(agg)
			require.NoError(t, err)
			// Content-Type is "aggregated" discovery format. Add extra parameter
			// to ensure we are resilient to these extra parameters.
			w.Header().Set("Content-Type", AcceptV2+"; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, err = w.Write(output)
			require.NoError(t, err)
		}))
		defer server.Close()
		client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
		resources, err := client.ServerPreferredResources()
		if len(test.expectedFailedGVs) > 0 {
			require.Error(t, err)
			expectedFailedGVs := sets.NewString(test.expectedFailedGVs...)
			actualFailedGVs := sets.NewString(failedGroupVersions(err)...)
			assert.True(t, expectedFailedGVs.Equal(actualFailedGVs),
				"%s: Expected Failed GVs (%s), got (%s)", test.name, expectedFailedGVs, actualFailedGVs)
		} else {
			require.NoError(t, err)
		}
		// Test the expected preferred GVKs are returned from the aggregated discovery.
		expectedGVKs := sets.NewString(test.expectedGVKs...)
		actualGVKs := sets.NewString(groupVersionKinds(resources)...)
		assert.True(t, expectedGVKs.Equal(actualGVKs),
			"%s: Expected GVKs (%s), got (%s)", test.name, expectedGVKs.List(), actualGVKs.List())
	}
}

func TestDiscoveryContentTypeVersion(t *testing.T) {
	v2 := schema.GroupVersionKind{Group: "apidiscovery.k8s.io", Version: "v2", Kind: "APIGroupDiscoveryList"}
	tests := []struct {
		contentType string
		gvk         schema.GroupVersionKind
		match       bool
		expectErr   bool
	}{
		{
			contentType: "application/json; g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList",
			gvk:         v2,
			match:       true,
			expectErr:   false,
		},
		{
			// content-type parameters are not in correct order, but comparison ignores order.
			contentType: "application/json; v=v2;as=APIGroupDiscoveryList;g=apidiscovery.k8s.io",
			gvk:         v2,
			match:       true,
			expectErr:   false,
		},
		{
			// content-type parameters are not in correct order, but comparison ignores order.
			contentType: "application/json; as=APIGroupDiscoveryList;g=apidiscovery.k8s.io;v=v2",
			gvk:         v2,
			match:       true,
			expectErr:   false,
		},
		{
			// Ignores extra parameter "charset=utf-8"
			contentType: "application/json; g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList;charset=utf-8",
			gvk:         v2,
			match:       true,
			expectErr:   false,
		},
		{
			contentType: "application/json",
			gvk:         v2,
			match:       false,
			expectErr:   false,
		},
		{
			contentType: "application/json; charset=UTF-8",
			gvk:         v2,
			match:       false,
			expectErr:   false,
		},
		{
			contentType: "text/json",
			gvk:         v2,
			match:       false,
			expectErr:   false,
		},
		{
			contentType: "text/html",
			gvk:         v2,
			match:       false,
			expectErr:   false,
		},
		{
			contentType: "",
			gvk:         v2,
			match:       false,
			expectErr:   true,
		},
	}

	for _, test := range tests {
		match, err := ContentTypeIsGVK(test.contentType, test.gvk)
		assert.Equal(t, test.expectErr, err != nil)
		assert.Equal(t, test.match, match)
	}
}

func TestUseLegacyDiscovery(t *testing.T) {
	// Default client sends aggregated discovery accept format (first) as well as legacy format.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		acceptHeader := req.Header.Get("Accept")
		assert.Equal(t, acceptDiscoveryFormats, acceptHeader)
	}))
	defer server.Close()
	client := NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	client.ServerGroups()
	// When "UseLegacyDiscovery" field is set, only the legacy discovery format is requested.
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		acceptHeader := req.Header.Get("Accept")
		assert.Equal(t, AcceptV1, acceptHeader)
	}))
	defer server.Close()
	client = NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: server.URL})
	client.UseLegacyDiscovery = true
	client.ServerGroups()
}

func groupNames(groups []*metav1.APIGroup) []string {
	result := []string{}
	for _, group := range groups {
		result = append(result, group.Name)
	}
	return result
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

func groupVersions(resources []*metav1.APIResourceList) []string {
	result := []string{}
	for _, resourceList := range resources {
		result = append(result, resourceList.GroupVersion)
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

func failedGroupVersions(err error) []string {
	result := []string{}
	ferr, ok := err.(*ErrGroupDiscoveryFailed)
	if !ok {
		return result
	}
	for gv := range ferr.Groups {
		result = append(result, gv.String())
	}
	return result
}
