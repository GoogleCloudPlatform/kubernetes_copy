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

package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/conversion"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	informers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	listers "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/controller/establish"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	serializerjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/storage/cacher"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	"k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func TestConvertFieldLabel(t *testing.T) {
	tests := []struct {
		name          string
		clusterScoped bool
		label         string
		expectError   bool
	}{
		{
			name:          "cluster scoped - name is ok",
			clusterScoped: true,
			label:         "metadata.name",
		},
		{
			name:          "cluster scoped - namespace is not ok",
			clusterScoped: true,
			label:         "metadata.namespace",
			expectError:   true,
		},
		{
			name:          "cluster scoped - other field is not ok",
			clusterScoped: true,
			label:         "some.other.field",
			expectError:   true,
		},
		{
			name:  "namespace scoped - name is ok",
			label: "metadata.name",
		},
		{
			name:  "namespace scoped - namespace is ok",
			label: "metadata.namespace",
		},
		{
			name:        "namespace scoped - other field is not ok",
			label:       "some.other.field",
			expectError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			crd := apiextensionsv1.CustomResourceDefinition{
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Conversion: &apiextensionsv1.CustomResourceConversion{
						Strategy: "None",
					},
				},
			}

			if test.clusterScoped {
				crd.Spec.Scope = apiextensionsv1.ClusterScoped
			} else {
				crd.Spec.Scope = apiextensionsv1.NamespaceScoped
			}
			f, err := conversion.NewCRConverterFactory(nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, c, err := f.NewConverter(&crd)
			if err != nil {
				t.Fatalf("Failed to create CR converter. error: %v", err)
			}

			label, value, err := c.ConvertFieldLabel(schema.GroupVersionKind{}, test.label, "value")
			if e, a := test.expectError, err != nil; e != a {
				t.Fatalf("err: expected %t, got %t", e, a)
			}
			if test.expectError {
				if e, a := "field label not supported: "+test.label, err.Error(); e != a {
					t.Errorf("err: expected %s, got %s", e, a)
				}
				return
			}

			if e, a := test.label, label; e != a {
				t.Errorf("label: expected %s, got %s", e, a)
			}
			if e, a := "value", value; e != a {
				t.Errorf("value: expected %s, got %s", e, a)
			}
		})
	}
}

func TestRouting(t *testing.T) {
	hasSynced := false

	crdIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	crdLister := listers.NewCustomResourceDefinitionLister(crdIndexer)

	// note that in production we delegate to the special handler that is attached at the end of the delegation chain that checks if the server has installed all known HTTP paths before replying to the client.
	// it returns 503 if not all registered signals have been ready (closed) otherwise it simply replies with 404.
	// the apiextentionserver is considered to be initialized once hasCRDInformerSyncedSignal is closed.
	//
	// here, in this test the delegate represent the special handler and hasSync represents the signal.
	// primarily we just want to make sure that the delegate has been called.
	// the behaviour of the real delegate is tested elsewhere.
	delegateCalled := false
	delegate := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		delegateCalled = true
		if !hasSynced {
			http.Error(w, "", 503)
			return
		}
		http.Error(w, "", 418)
	})
	customV1 := schema.GroupVersion{Group: "custom", Version: "v1"}
	handler := &crdHandler{
		crdLister: crdLister,
		delegate:  delegate,
		versionDiscoveryHandler: &versionDiscoveryHandler{
			discovery: map[schema.GroupVersion]*discovery.APIVersionHandler{
				customV1: discovery.NewAPIVersionHandler(Codecs, customV1, discovery.APIResourceListerFunc(func() []metav1.APIResource {
					return nil
				})),
			},
			delegate: delegate,
		},
		groupDiscoveryHandler: &groupDiscoveryHandler{
			discovery: map[string]*discovery.APIGroupHandler{
				"custom": discovery.NewAPIGroupHandler(Codecs, metav1.APIGroup{
					Name:             customV1.Group,
					Versions:         []metav1.GroupVersionForDiscovery{{GroupVersion: customV1.String(), Version: customV1.Version}},
					PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: customV1.String(), Version: customV1.Version},
				}),
			},
			delegate: delegate,
		},
	}

	testcases := []struct {
		Name    string
		Method  string
		Path    string
		Headers map[string]string
		Body    io.Reader

		APIGroup          string
		APIVersion        string
		Verb              string
		Resource          string
		IsResourceRequest bool

		HasSynced bool

		ExpectStatus         int
		ExpectResponse       func(*testing.T, *http.Response, []byte)
		ExpectDelegateCalled bool
	}{
		{
			Name:                 "existing group discovery, presync",
			Method:               "GET",
			Path:                 "/apis/custom",
			APIGroup:             "custom",
			APIVersion:           "",
			HasSynced:            false,
			IsResourceRequest:    false,
			ExpectDelegateCalled: false,
			ExpectStatus:         200,
		},
		{
			Name:                 "existing group discovery",
			Method:               "GET",
			Path:                 "/apis/custom",
			APIGroup:             "custom",
			APIVersion:           "",
			HasSynced:            true,
			IsResourceRequest:    false,
			ExpectDelegateCalled: false,
			ExpectStatus:         200,
		},

		{
			Name:                 "nonexisting group discovery, presync",
			Method:               "GET",
			Path:                 "/apis/other",
			APIGroup:             "other",
			APIVersion:           "",
			HasSynced:            false,
			IsResourceRequest:    false,
			ExpectDelegateCalled: true,
			ExpectStatus:         503,
		},
		{
			Name:                 "nonexisting group discovery",
			Method:               "GET",
			Path:                 "/apis/other",
			APIGroup:             "other",
			APIVersion:           "",
			HasSynced:            true,
			IsResourceRequest:    false,
			ExpectDelegateCalled: true,
			ExpectStatus:         418,
		},

		{
			Name:                 "existing group version discovery, presync",
			Method:               "GET",
			Path:                 "/apis/custom/v1",
			APIGroup:             "custom",
			APIVersion:           "v1",
			HasSynced:            false,
			IsResourceRequest:    false,
			ExpectDelegateCalled: false,
			ExpectStatus:         200,
		},
		{
			Name:                 "existing group version discovery",
			Method:               "GET",
			Path:                 "/apis/custom/v1",
			APIGroup:             "custom",
			APIVersion:           "v1",
			HasSynced:            true,
			IsResourceRequest:    false,
			ExpectDelegateCalled: false,
			ExpectStatus:         200,
		},

		{
			Name:                 "nonexisting group version discovery, presync",
			Method:               "GET",
			Path:                 "/apis/other/v1",
			APIGroup:             "other",
			APIVersion:           "v1",
			HasSynced:            false,
			IsResourceRequest:    false,
			ExpectDelegateCalled: true,
			ExpectStatus:         503,
		},
		{
			Name:                 "nonexisting group version discovery",
			Method:               "GET",
			Path:                 "/apis/other/v1",
			APIGroup:             "other",
			APIVersion:           "v1",
			HasSynced:            true,
			IsResourceRequest:    false,
			ExpectDelegateCalled: true,
			ExpectStatus:         418,
		},

		{
			Name:                 "existing group, nonexisting version discovery, presync",
			Method:               "GET",
			Path:                 "/apis/custom/v2",
			APIGroup:             "custom",
			APIVersion:           "v2",
			HasSynced:            false,
			IsResourceRequest:    false,
			ExpectDelegateCalled: true,
			ExpectStatus:         503,
		},
		{
			Name:                 "existing group, nonexisting version discovery",
			Method:               "GET",
			Path:                 "/apis/custom/v2",
			APIGroup:             "custom",
			APIVersion:           "v2",
			HasSynced:            true,
			IsResourceRequest:    false,
			ExpectDelegateCalled: true,
			ExpectStatus:         418,
		},

		{
			Name:                 "nonexisting group, resource request, presync",
			Method:               "GET",
			Path:                 "/apis/custom/v2/foos",
			APIGroup:             "custom",
			APIVersion:           "v2",
			Verb:                 "list",
			Resource:             "foos",
			HasSynced:            false,
			IsResourceRequest:    true,
			ExpectDelegateCalled: true,
			ExpectStatus:         503,
		},
		{
			Name:                 "nonexisting group, resource request",
			Method:               "GET",
			Path:                 "/apis/custom/v2/foos",
			APIGroup:             "custom",
			APIVersion:           "v2",
			Verb:                 "list",
			Resource:             "foos",
			HasSynced:            true,
			IsResourceRequest:    true,
			ExpectDelegateCalled: true,
			ExpectStatus:         418,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.Name, func(t *testing.T) {
			for _, contentType := range []string{"json", "yaml", "proto", "unknown"} {
				t.Run(contentType, func(t *testing.T) {
					delegateCalled = false
					hasSynced = tc.HasSynced

					recorder := httptest.NewRecorder()

					req := httptest.NewRequest(tc.Method, tc.Path, tc.Body)
					for k, v := range tc.Headers {
						req.Header.Set(k, v)
					}

					expectStatus := tc.ExpectStatus
					switch contentType {
					case "json":
						req.Header.Set("Accept", "application/json")
					case "yaml":
						req.Header.Set("Accept", "application/yaml")
					case "proto":
						req.Header.Set("Accept", "application/vnd.kubernetes.protobuf, application/json")
					case "unknown":
						req.Header.Set("Accept", "application/vnd.kubernetes.unknown")
						// rather than success, we'll get a not supported error
						if expectStatus == 200 {
							expectStatus = 406
						}
					default:
						t.Fatalf("unknown content type %v", contentType)
					}

					req = req.WithContext(apirequest.WithRequestInfo(req.Context(), &apirequest.RequestInfo{
						Verb:              tc.Verb,
						Resource:          tc.Resource,
						APIGroup:          tc.APIGroup,
						APIVersion:        tc.APIVersion,
						IsResourceRequest: tc.IsResourceRequest,
						Path:              tc.Path,
					}))

					handler.ServeHTTP(recorder, req)

					if tc.ExpectDelegateCalled != delegateCalled {
						t.Errorf("expected delegated called %v, got %v", tc.ExpectDelegateCalled, delegateCalled)
					}
					result := recorder.Result()
					content, _ := io.ReadAll(result.Body)
					if e, a := expectStatus, result.StatusCode; e != a {
						t.Log(string(content))
						t.Errorf("expected %v, got %v", e, a)
					}
					if tc.ExpectResponse != nil {
						tc.ExpectResponse(t, result, content)
					}

					// Make sure error responses come back with status objects in all encodings, including unknown encodings
					if !delegateCalled && expectStatus >= 300 {
						status := &metav1.Status{}

						switch contentType {
						// unknown accept headers fall back to json errors
						case "json", "unknown":
							if e, a := "application/json", result.Header.Get("Content-Type"); e != a {
								t.Errorf("expected Content-Type %v, got %v", e, a)
							}
							if err := json.Unmarshal(content, status); err != nil {
								t.Fatal(err)
							}
						case "yaml":
							if e, a := "application/yaml", result.Header.Get("Content-Type"); e != a {
								t.Errorf("expected Content-Type %v, got %v", e, a)
							}
							if err := yaml.Unmarshal(content, status); err != nil {
								t.Fatal(err)
							}
						case "proto":
							if e, a := "application/vnd.kubernetes.protobuf", result.Header.Get("Content-Type"); e != a {
								t.Errorf("expected Content-Type %v, got %v", e, a)
							}
							if _, _, err := protobuf.NewSerializer(Scheme, Scheme).Decode(content, nil, status); err != nil {
								t.Fatal(err)
							}
						default:
							t.Fatalf("unknown content type %v", contentType)
						}

						if e, a := metav1.Unversioned.WithKind("Status"), status.GroupVersionKind(); e != a {
							t.Errorf("expected %#v, got %#v", e, a)
						}
						if int(status.Code) != int(expectStatus) {
							t.Errorf("expected %v, got %v", expectStatus, status.Code)
						}
					}
				})
			}
		})
	}
}

func TestHandlerConversionWithWatchCache(t *testing.T) {
	testHandlerConversion(t, true)
}

func TestHandlerConversionWithoutWatchCache(t *testing.T) {
	testHandlerConversion(t, false)
}

func testHandlerConversion(t *testing.T, enableWatchCache bool) {
	cl := fake.NewSimpleClientset()
	informers := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0)
	crdInformer := informers.Apiextensions().V1().CustomResourceDefinitions()

	server, storageConfig := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	defer server.Terminate(t)

	crd := multiVersionFixture.DeepCopy()
	// Create a context with metav1.NamespaceNone as the namespace since multiVersionFixture
	// is a cluster scoped CRD.
	ctx := apirequest.WithNamespace(apirequest.NewContext(), metav1.NamespaceNone)
	if _, err := cl.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := crdInformer.Informer().GetStore().Add(crd); err != nil {
		t.Fatal(err)
	}

	if enableWatchCache {
		storageConfig.EventsHistoryWindow = cacher.DefaultEventFreshDuration
	}

	etcdOptions := options.NewEtcdOptions(storageConfig)
	etcdOptions.StorageConfig.Codec = unstructured.UnstructuredJSONScheme
	restOptionsGetter := generic.RESTOptions{
		StorageConfig:           etcdOptions.StorageConfig.ForResource(schema.GroupResource{Group: crd.Spec.Group, Resource: crd.Spec.Names.Plural}),
		Decorator:               generic.UndecoratedStorage,
		EnableGarbageCollection: true,
		DeleteCollectionWorkers: 1,
		ResourcePrefix:          crd.Spec.Group + "/" + crd.Spec.Names.Plural,
		CountMetricPollPeriod:   time.Minute,
	}
	if enableWatchCache {
		restOptionsGetter.Decorator = genericregistry.StorageWithCacher()
	}

	handler, err := NewCustomResourceDefinitionHandler(
		&versionDiscoveryHandler{}, &groupDiscoveryHandler{},
		crdInformer,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		restOptionsGetter,
		dummyAdmissionImpl{},
		&establish.EstablishingController{},
		dummyServiceResolverImpl{},
		func(r webhook.AuthenticationInfoResolver) webhook.AuthenticationInfoResolver { return r },
		1,
		dummyAuthorizerImpl{},
		time.Minute, time.Minute, nil, 3*1024*1024)
	if err != nil {
		t.Fatal(err)
	}

	crdInfo, err := handler.getOrCreateServingInfoFor(crd.UID, crd.Name)
	if err != nil {
		t.Fatal(err)
	}

	updateValidateFunc := func(ctx context.Context, obj, old runtime.Object) error { return nil }
	validateFunc := func(ctx context.Context, obj runtime.Object) error { return nil }
	startResourceVersion := ""

	if enableWatchCache {
		// Let watch cache establish initial list
		time.Sleep(time.Second)
	}

	// Create and delete a marker object to get a starting resource version
	{
		u := &unstructured.Unstructured{Object: map[string]interface{}{}}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: "stable.example.com", Version: "v1beta1", Kind: "MultiVersion"})
		u.SetName("marker")
		if item, err := crdInfo.storages["v1beta1"].CustomResource.Create(ctx, u, validateFunc, &metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		} else {
			startResourceVersion = item.(*unstructured.Unstructured).GetResourceVersion()
		}
		if _, _, err := crdInfo.storages["v1beta1"].CustomResource.Delete(ctx, u.GetName(), validateFunc, &metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// Create and get every version, expect returned result to match creation GVK
	for _, version := range crd.Spec.Versions {
		expectGVK := schema.GroupVersionKind{Group: "stable.example.com", Version: version.Name, Kind: "MultiVersion"}
		u := &unstructured.Unstructured{Object: map[string]interface{}{}}
		u.SetGroupVersionKind(expectGVK)
		u.SetName("my-" + version.Name)
		unstructured.SetNestedField(u.Object, int64(1), "spec", "num")

		// Create
		if item, err := crdInfo.storages[version.Name].CustomResource.Create(ctx, u, validateFunc, &metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		} else if item.GetObjectKind().GroupVersionKind() != expectGVK {
			t.Errorf("expected create result to be %#v, got %#v", expectGVK, item.GetObjectKind().GroupVersionKind())
		} else {
			u = item.(*unstructured.Unstructured)
		}

		// Update
		u.SetAnnotations(map[string]string{"updated": "true"})
		if item, _, err := crdInfo.storages[version.Name].CustomResource.Update(ctx, u.GetName(), rest.DefaultUpdatedObjectInfo(u), validateFunc, updateValidateFunc, false, &metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		} else if item.GetObjectKind().GroupVersionKind() != expectGVK {
			t.Errorf("expected update result to be %#v, got %#v", expectGVK, item.GetObjectKind().GroupVersionKind())
		}

		// Get
		if item, err := crdInfo.storages[version.Name].CustomResource.Get(ctx, u.GetName(), &metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		} else if item.GetObjectKind().GroupVersionKind() != expectGVK {
			t.Errorf("expected get result to be %#v, got %#v", expectGVK, item.GetObjectKind().GroupVersionKind())
		}

		if enableWatchCache {
			// Allow time to propagate the create into the cache
			time.Sleep(time.Second)
			// Get cached
			if item, err := crdInfo.storages[version.Name].CustomResource.Get(ctx, u.GetName(), &metav1.GetOptions{ResourceVersion: "0"}); err != nil {
				t.Fatal(err)
			} else if item.GetObjectKind().GroupVersionKind() != expectGVK {
				t.Errorf("expected cached get result to be %#v, got %#v", expectGVK, item.GetObjectKind().GroupVersionKind())
			}
		}
	}

	// List every version, expect all returned items to match request GVK
	for _, version := range crd.Spec.Versions {
		expectGVK := schema.GroupVersionKind{Group: "stable.example.com", Version: version.Name, Kind: "MultiVersion"}

		if list, err := crdInfo.storages[version.Name].CustomResource.List(ctx, &metainternalversion.ListOptions{}); err != nil {
			t.Fatal(err)
		} else {
			for _, item := range list.(*unstructured.UnstructuredList).Items {
				if item.GroupVersionKind() != expectGVK {
					t.Errorf("expected list item to be %#v, got %#v", expectGVK, item.GroupVersionKind())
				}
			}
		}

		if enableWatchCache {
			// List from watch cache
			if list, err := crdInfo.storages[version.Name].CustomResource.List(ctx, &metainternalversion.ListOptions{ResourceVersion: "0"}); err != nil {
				t.Fatal(err)
			} else {
				for _, item := range list.(*unstructured.UnstructuredList).Items {
					if item.GroupVersionKind() != expectGVK {
						t.Errorf("expected cached list item to be %#v, got %#v", expectGVK, item.GroupVersionKind())
					}
				}
			}
		}

		watch, err := crdInfo.storages[version.Name].CustomResource.Watch(ctx, &metainternalversion.ListOptions{ResourceVersion: startResourceVersion})
		if err != nil {
			t.Fatal(err)
		}
		// 5 events: delete marker, create v1alpha1, create v1beta1, update v1alpha1, update v1beta1
		for i := 0; i < 5; i++ {
			select {
			case event, ok := <-watch.ResultChan():
				if !ok {
					t.Fatalf("watch closed")
				}
				item, isUnstructured := event.Object.(*unstructured.Unstructured)
				if !isUnstructured {
					t.Fatalf("unexpected object type %T: %#v", item, event)
				}
				if item.GroupVersionKind() != expectGVK {
					t.Errorf("expected watch object to be %#v, got %#v", expectGVK, item.GroupVersionKind())
				}
			case <-time.After(time.Second):
				t.Errorf("timed out waiting for watch event")
			}
		}
		// Expect no more watch events
		select {
		case event := <-watch.ResultChan():
			t.Errorf("unexpected event: %#v", event)
		case <-time.After(time.Second):
		}
	}
}

func TestDecoder(t *testing.T) {
	multiVersionJSON := `
	{
	"apiVersion": "stable.example.com/v1beta1",
	"kind": "MultiVersion",
	"metadata": {
		"name": "my-mv"
	},
	"num": 1,
	"num": 2,
	"unknown": "foo"
	}
	`
	multiVersionYAML := `
apiVersion: stable.example.com/v1beta1
kind: MultiVersion
metadata:
  name: my-mv
num: 1
num: 2
unknown: foo`

	expectedObjUnknownNotPreserved := &unstructured.Unstructured{}
	err := expectedObjUnknownNotPreserved.UnmarshalJSON([]byte(`
	{
	"apiVersion": "stable.example.com/v1beta1",
	"kind": "MultiVersion",
	"metadata": {
		"creationTimestamp": null,
		"generation": 1,
		"name": "my-mv"
	},
	"num": 2
	}
	`))
	if err != nil {
		t.Fatal(err)
	}

	expectedObjUnknownPreserved := &unstructured.Unstructured{}
	err = expectedObjUnknownPreserved.UnmarshalJSON([]byte(`
	{
	"apiVersion": "stable.example.com/v1beta1",
	"kind": "MultiVersion",
	"metadata": {
		"creationTimestamp": null,
		"generation": 1,
		"name": "my-mv"
	},
	"num": 2,
	"unknown": "foo"
	}
	`))
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name                  string
		body                  string
		yaml                  bool
		strictDecoding        bool
		preserveUnknownFields bool
		expectedObj           *unstructured.Unstructured
		expectedErr           error
	}{
		{
			name:           "strict-decoding",
			body:           multiVersionJSON,
			strictDecoding: true,
			expectedObj:    expectedObjUnknownNotPreserved,
			expectedErr:    errors.New(`strict decoding error: duplicate field "num", unknown field "unknown"`),
		},
		{
			name:           "non-strict-decoding",
			body:           multiVersionJSON,
			strictDecoding: false,
			expectedObj:    expectedObjUnknownNotPreserved,
			expectedErr:    nil,
		},
		{
			name:                  "strict-decoding-preserve-unknown",
			body:                  multiVersionJSON,
			strictDecoding:        true,
			preserveUnknownFields: true,
			expectedObj:           expectedObjUnknownPreserved,
			expectedErr:           errors.New(`strict decoding error: duplicate field "num"`),
		},
		{
			name:                  "non-strict-decoding-preserve-unknown",
			body:                  multiVersionJSON,
			strictDecoding:        false,
			preserveUnknownFields: true,
			expectedObj:           expectedObjUnknownPreserved,
			expectedErr:           nil,
		},
		{
			name:           "strict-decoding-yaml",
			body:           multiVersionYAML,
			yaml:           true,
			strictDecoding: true,
			expectedObj:    expectedObjUnknownNotPreserved,
			expectedErr: errors.New(`strict decoding error: yaml: unmarshal errors:
  line 7: key "num" already set in map, unknown field "unknown"`),
		},
		{
			name:           "non-strict-decoding-yaml",
			body:           multiVersionYAML,
			yaml:           true,
			strictDecoding: false,
			expectedObj:    expectedObjUnknownNotPreserved,
			expectedErr:    nil,
		},
		{
			name:                  "strict-decoding-preserve-unknown-yaml",
			body:                  multiVersionYAML,
			yaml:                  true,
			strictDecoding:        true,
			preserveUnknownFields: true,
			expectedObj:           expectedObjUnknownPreserved,
			expectedErr: errors.New(`strict decoding error: yaml: unmarshal errors:
  line 7: key "num" already set in map`),
		},
		{
			name:                  "non-strict-decoding-preserve-unknown-yaml",
			body:                  multiVersionYAML,
			yaml:                  true,
			strictDecoding:        false,
			preserveUnknownFields: true,
			expectedObj:           expectedObjUnknownPreserved,
			expectedErr:           nil,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			v := "v1beta1"
			structuralSchemas := map[string]*structuralschema.Structural{}
			structuralSchema, err := structuralschema.NewStructural(&apiextensions.JSONSchemaProps{
				Type:       "object",
				Properties: map[string]apiextensions.JSONSchemaProps{"num": {Type: "integer", Description: "v1beta1 num field"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			structuralSchemas[v] = structuralSchema
			delegate := serializerjson.NewSerializerWithOptions(serializerjson.DefaultMetaFactory, unstructuredCreator{}, nil, serializerjson.SerializerOptions{Yaml: tc.yaml, Strict: tc.strictDecoding})
			decoder := &schemaCoercingDecoder{
				delegate: delegate,
				validator: unstructuredSchemaCoercer{
					dropInvalidMetadata: true,
					repairGeneration:    true,
					structuralSchemas:   structuralSchemas,
					structuralSchemaGK: schema.GroupKind{
						Group: "stable.example.com",
						Kind:  "MultiVersion",
					},
					returnUnknownFieldPaths: tc.strictDecoding,
					preserveUnknownFields:   tc.preserveUnknownFields,
				},
			}

			obj, _, err := decoder.Decode([]byte(tc.body), nil, nil)
			if obj != nil {
				unstructured, ok := obj.(*unstructured.Unstructured)
				if !ok {
					t.Fatalf("obj is not an unstructured: %v", obj)
				}
				objBytes, err := unstructured.MarshalJSON()
				if err != nil {
					t.Fatalf("err marshaling json: %v", err)
				}
				expectedBytes, err := tc.expectedObj.MarshalJSON()
				if err != nil {
					t.Fatalf("err marshaling json: %v", err)
				}
				if bytes.Compare(objBytes, expectedBytes) != 0 {
					t.Fatalf("expected obj: \n%v\n got obj: \n%v\n", tc.expectedObj, obj)
				}
			}
			if err == nil || tc.expectedErr == nil {
				if err != nil || tc.expectedErr != nil {
					t.Fatalf("expected err: %v, got err: %v", tc.expectedErr, err)
				}
			} else if err.Error() != tc.expectedErr.Error() {
				t.Fatalf("expected err: \n%v\n got err: \n%v\n", tc.expectedErr, err)
			}
		})
	}

}

type dummyAdmissionImpl struct{}

func (dummyAdmissionImpl) Handles(operation admission.Operation) bool { return false }

type dummyAuthorizerImpl struct{}

func (dummyAuthorizerImpl) Authorize(ctx context.Context, a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
	return authorizer.DecisionAllow, "", nil
}

type dummyServiceResolverImpl struct{}

func (dummyServiceResolverImpl) ResolveEndpoint(namespace, name string, port int32) (*url.URL, error) {
	return &url.URL{Scheme: "https", Host: net.JoinHostPort(name+"."+namespace+".svc", strconv.Itoa(int(port)))}, nil
}

var multiVersionFixture = &apiextensionsv1.CustomResourceDefinition{
	ObjectMeta: metav1.ObjectMeta{Name: "multiversion.stable.example.com", UID: types.UID("12345")},
	Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Group: "stable.example.com",
		Names: apiextensionsv1.CustomResourceDefinitionNames{
			Plural: "multiversion", Singular: "multiversion", Kind: "MultiVersion", ShortNames: []string{"mv"}, ListKind: "MultiVersionList", Categories: []string{"all"},
		},
		Conversion:            &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter},
		Scope:                 apiextensionsv1.ClusterScoped,
		PreserveUnknownFields: false,
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			{
				// storage version, same schema as v1alpha1
				Name: "v1beta1", Served: true, Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:       "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{"num": {Type: "integer", Description: "v1beta1 num field"}},
					},
				},
			},
			{
				// same schema as v1beta1
				Name: "v1alpha1", Served: true, Storage: false,
				Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:       "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{"num": {Type: "integer", Description: "v1alpha1 num field"}},
					},
				},
			},
		},
	},
	Status: apiextensionsv1.CustomResourceDefinitionStatus{
		AcceptedNames: apiextensionsv1.CustomResourceDefinitionNames{
			Plural: "multiversion", Singular: "multiversion", Kind: "MultiVersion", ShortNames: []string{"mv"}, ListKind: "MultiVersionList", Categories: []string{"all"},
		},
	},
}

func Test_defaultDeprecationWarning(t *testing.T) {
	tests := []struct {
		name              string
		deprecatedVersion string
		crd               apiextensionsv1.CustomResourceDefinitionSpec
		want              string
	}{
		{
			name:              "no replacement",
			deprecatedVersion: "v1",
			crd: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "example.com",
				Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Widget"},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{Name: "v1", Served: true, Deprecated: true},
					{Name: "v2", Served: true, Deprecated: true},
					{Name: "v3", Served: false},
				},
			},
			want: "example.com/v1 Widget is deprecated",
		},
		{
			name:              "replacement sorting",
			deprecatedVersion: "v1",
			crd: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "example.com",
				Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Widget"},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{Name: "v1", Served: true},
					{Name: "v1alpha1", Served: true},
					{Name: "v1alpha2", Served: true},
					{Name: "v1beta1", Served: true},
					{Name: "v1beta2", Served: true},
					{Name: "v2", Served: true},
					{Name: "v2alpha1", Served: true},
					{Name: "v2alpha2", Served: true},
					{Name: "v2beta1", Served: true},
					{Name: "v2beta2", Served: true},
					{Name: "v3", Served: false},
					{Name: "v3alpha1", Served: false},
					{Name: "v3alpha2", Served: false},
					{Name: "v3beta1", Served: false},
					{Name: "v3beta2", Served: false},
				},
			},
			want: "example.com/v1 Widget is deprecated; use example.com/v2 Widget",
		},
		{
			name:              "no newer replacement of equal stability",
			deprecatedVersion: "v2",
			crd: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "example.com",
				Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Widget"},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{Name: "v1", Served: true},
					{Name: "v3", Served: false},
					{Name: "v3alpha1", Served: true},
					{Name: "v3beta1", Served: true},
					{Name: "v4", Served: true, Deprecated: true},
				},
			},
			want: "example.com/v2 Widget is deprecated",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultDeprecationWarning(tt.deprecatedVersion, tt.crd); got != tt.want {
				t.Errorf("defaultDeprecationWarning() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildOpenAPIModelsForApply(t *testing.T) {
	// This is a list of validation that we expect to work.
	tests := []apiextensionsv1.CustomResourceValidation{
		{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type:       "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{"num": {Type: "integer", Description: "v1beta1 num field"}},
			},
		},
		{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type:         "",
				XIntOrString: true,
			},
		},
		{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"oneOf": {
						OneOf: []apiextensionsv1.JSONSchemaProps{
							{Type: "boolean"},
							{Type: "string"},
						},
					},
				},
			},
		},
		{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"nullable": {
						Type:     "integer",
						Nullable: true,
					},
				},
			},
		},
	}

	staticSpec, err := getOpenAPISpecFromFile()
	if err != nil {
		t.Fatalf("Failed to load openapi spec: %v", err)
	}

	crd := apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "example.stable.example.com", UID: types.UID("12345")},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "stable.example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "examples", Singular: "example", Kind: "Example", ShortNames: []string{"ex"}, ListKind: "ExampleList", Categories: []string{"all"},
			},
			Conversion:            &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter},
			Scope:                 apiextensionsv1.ClusterScoped,
			PreserveUnknownFields: false,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name: "v1beta1", Served: true, Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				},
			},
		},
	}

	convertedDefs := map[string]*spec.Schema{}
	for k, v := range staticSpec.Definitions {
		vCopy := v
		convertedDefs[k] = &vCopy
	}

	for i, test := range tests {
		crd.Spec.Versions[0].Schema = &test
		models, err := buildOpenAPIModelsForApply(convertedDefs, &crd)
		if err != nil {
			t.Fatalf("failed to convert to apply model: %v", err)
		}
		if models == nil {
			t.Fatalf("%d: failed to convert to apply model: nil", i)
		}
	}
}

func getOpenAPISpecFromFile() (*spec.Swagger, error) {
	path := filepath.Join("testdata", "swagger.json")
	_, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	byteSpec, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	staticSpec := &spec.Swagger{}

	err = yaml.Unmarshal(byteSpec, staticSpec)
	if err != nil {
		return nil, err
	}

	return staticSpec, nil
}
