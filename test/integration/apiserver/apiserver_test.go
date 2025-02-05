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
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	jsonserializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/metadata"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/pager"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	utilversion "k8s.io/component-base/version"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/controlplane"
	"k8s.io/kubernetes/pkg/kubeapiserver"
	"k8s.io/kubernetes/test/integration"
	"k8s.io/kubernetes/test/integration/etcd"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

func setup(t *testing.T, groupVersions ...schema.GroupVersion) (context.Context, clientset.Interface, *restclient.Config, framework.TearDownFunc) {
	return setupWithResources(t, groupVersions, nil)
}

func setupWithResources(t *testing.T, groupVersions []schema.GroupVersion, resources []schema.GroupVersionResource) (context.Context, clientset.Interface /* TODO (pohly): return ktesting.TContext */, *restclient.Config, framework.TearDownFunc) {
	tCtx := ktesting.Init(t)

	client, config, teardown := framework.StartTestServer(tCtx, t, framework.TestServerSetup{
		ModifyServerConfig: func(config *controlplane.Config) {
			if len(groupVersions) > 0 || len(resources) > 0 {
				resourceConfig := controlplane.DefaultAPIResourceConfigSource()
				resourceConfig.EnableVersions(groupVersions...)
				resourceConfig.EnableResources(resources...)
				config.ControlPlane.Extra.APIResourceConfigSource = resourceConfig
			}
		},
	})

	newTeardown := func() {
		tCtx.Cancel("tearing down apiserver")
		teardown()
	}

	return tCtx, client, config, newTeardown
}

func verifyStatusCode(t *testing.T, transport http.RoundTripper, verb, URL, body string, expectedStatusCode int) {
	// We don't use the typed Go client to send this request to be able to verify the response status code.
	bodyBytes := bytes.NewReader([]byte(body))
	req, err := http.NewRequest(verb, URL, bodyBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v in sending req with verb: %s, URL: %s and body: %s", err, verb, URL, body)
	}
	klog.Infof("Sending request: %v", req)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v in req: %v", err, req)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != expectedStatusCode {
		t.Errorf("Expected status %v, but got %v", expectedStatusCode, resp.StatusCode)
		t.Errorf("Body: %v", string(b))
	}
}

func newRS(namespace string) *apps.ReplicaSet {
	return &apps.ReplicaSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ReplicaSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    namespace,
			GenerateName: "apiserver-test",
		},
		Spec: apps.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"name": "test"},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "fake-name",
							Image: "fakeimage",
						},
					},
				},
			},
		},
	}
}

var cascDel = `
{
  "kind": "DeleteOptions",
  "apiVersion": "v1",
  "orphanDependents": false
}
`

func Test4xxStatusCodeInvalidPatch(t *testing.T) {
	ctx, client, _, tearDownFn := setup(t)
	defer tearDownFn()

	obj := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"}
                },
		"spec": {
			"selector": {
				"matchLabels": {
					 "app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	resp, err := client.CoreV1().RESTClient().Post().
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Body(obj).Do(ctx).Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v: %v", err, resp)
	}
	result := client.CoreV1().RESTClient().Patch(types.MergePatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Body([]byte(`{"metadata":{"annotations":{"foo":["bar"]}}}`)).Do(ctx)
	var statusCode int
	result.StatusCode(&statusCode)
	if statusCode != 422 {
		t.Fatalf("Expected status code to be 422, got %v (%#v)", statusCode, result)
	}
	result = client.CoreV1().RESTClient().Patch(types.StrategicMergePatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Body([]byte(`{"metadata":{"annotations":{"foo":["bar"]}}}`)).Do(ctx)
	result.StatusCode(&statusCode)
	if statusCode != 422 {
		t.Fatalf("Expected status code to be 422, got %v (%#v)", statusCode, result)
	}
}

func TestCacheControl(t *testing.T) {
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()

	rt, err := restclient.TransportFor(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{
		// untyped
		"/",
		// health
		"/healthz",
		// openapi
		"/openapi/v2",
		// discovery
		"/api",
		"/api/v1",
		"/apis",
		"/apis/apps",
		"/apis/apps/v1",
		// apis
		"/api/v1/namespaces",
		"/apis/apps/v1/deployments",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest("GET", server.ClientConfig.Host+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			cc := resp.Header.Get("Cache-Control")
			if !strings.Contains(cc, "private") {
				t.Errorf("expected private cache-control, got %q", cc)
			}
		})
	}
}

// Tests that the apiserver returns HSTS headers as expected.
func TestHSTS(t *testing.T) {
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{"--strict-transport-security-directives=max-age=31536000,includeSubDomains"}, framework.SharedEtcd())
	defer server.TearDownFn()

	rt, err := restclient.TransportFor(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{
		// untyped
		"/",
		// health
		"/healthz",
		// openapi
		"/openapi/v2",
		// discovery
		"/api",
		"/api/v1",
		"/apis",
		"/apis/apps",
		"/apis/apps/v1",
		// apis
		"/api/v1/namespaces",
		"/apis/apps/v1/deployments",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest("GET", server.ClientConfig.Host+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			cc := resp.Header.Get("Strict-Transport-Security")
			if !strings.Contains(cc, "max-age=31536000; includeSubDomains") {
				t.Errorf("expected max-age=31536000; includeSubDomains, got %q", cc)
			}
		})
	}
}

// Tests that the apiserver returns 202 status code as expected.
func Test202StatusCode(t *testing.T) {
	ctx, clientSet, kubeConfig, tearDownFn := setup(t)
	defer tearDownFn()

	transport, err := restclient.TransportFor(kubeConfig)
	if err != nil {
		t.Fatal(err)
	}

	ns := framework.CreateNamespaceOrDie(clientSet, "status-code", t)
	defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

	rsClient := clientSet.AppsV1().ReplicaSets(ns.Name)

	// 1. Create the resource without any finalizer and then delete it without setting DeleteOptions.
	// Verify that server returns 200 in this case.
	rs, err := rsClient.Create(ctx, newRS(ns.Name), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create rs: %v", err)
	}
	verifyStatusCode(t, transport, "DELETE", kubeConfig.Host+path.Join("/apis/apps/v1/namespaces", ns.Name, "replicasets", rs.Name), "", 200)

	// 2. Create the resource with a finalizer so that the resource is not immediately deleted and then delete it without setting DeleteOptions.
	// Verify that the apiserver still returns 200 since DeleteOptions.OrphanDependents is not set.
	rs = newRS(ns.Name)
	rs.ObjectMeta.Finalizers = []string{"kube.io/dummy-finalizer"}
	rs, err = rsClient.Create(ctx, rs, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create rs: %v", err)
	}
	verifyStatusCode(t, transport, "DELETE", kubeConfig.Host+path.Join("/apis/apps/v1/namespaces", ns.Name, "replicasets", rs.Name), "", 200)

	// 3. Create the resource and then delete it with DeleteOptions.OrphanDependents=false.
	// Verify that the server still returns 200 since the resource is immediately deleted.
	rs = newRS(ns.Name)
	rs, err = rsClient.Create(ctx, rs, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create rs: %v", err)
	}
	verifyStatusCode(t, transport, "DELETE", kubeConfig.Host+path.Join("/apis/apps/v1/namespaces", ns.Name, "replicasets", rs.Name), cascDel, 200)

	// 4. Create the resource with a finalizer so that the resource is not immediately deleted and then delete it with DeleteOptions.OrphanDependents=false.
	// Verify that the server returns 202 in this case.
	rs = newRS(ns.Name)
	rs.ObjectMeta.Finalizers = []string{"kube.io/dummy-finalizer"}
	rs, err = rsClient.Create(ctx, rs, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create rs: %v", err)
	}
	verifyStatusCode(t, transport, "DELETE", kubeConfig.Host+path.Join("/apis/apps/v1/namespaces", ns.Name, "replicasets", rs.Name), cascDel, 202)
}

var (
	invalidContinueToken        = "invalidContinueToken"
	invalidResourceVersion      = "invalid"
	invalidResourceVersionMatch = metav1.ResourceVersionMatch("InvalidMatch")
)

// TestListOptions ensures that list works as expected for valid and invalid combinations of limit, continue,
// resourceVersion and resourceVersionMatch.
func TestListOptions(t *testing.T) {

	for _, watchCacheEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("watchCacheEnabled=%t", watchCacheEnabled), func(t *testing.T) {
			tCtx := ktesting.Init(t)

			var storageTransport *storagebackend.TransportConfig
			clientSet, _, tearDownFn := framework.StartTestServer(tCtx, t, framework.TestServerSetup{
				ModifyServerRunOptions: func(opts *options.ServerRunOptions) {
					opts.Etcd.EnableWatchCache = watchCacheEnabled
					storageTransport = &opts.Etcd.StorageConfig.Transport
				},
			})
			defer tearDownFn()

			ns := framework.CreateNamespaceOrDie(clientSet, "list-options", t)
			defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

			rsClient := clientSet.AppsV1().ReplicaSets(ns.Name)

			var compactedRv, oldestUncompactedRv string
			for i := 0; i < 15; i++ {
				rs := newRS(ns.Name)
				rs.Name = fmt.Sprintf("test-%d", i)
				created, err := rsClient.Create(tCtx, rs, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					compactedRv = created.ResourceVersion // We compact this first resource version below
				}
				// delete the first 5, and then compact them
				if i < 5 {
					var zero int64
					if err := rsClient.Delete(tCtx, rs.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
						t.Fatal(err)
					}
					oldestUncompactedRv = created.ResourceVersion
				}
			}

			// compact some of the revision history in etcd so we can test "too old" resource versions
			rawClient, kvClient, err := integration.GetEtcdClients(*storageTransport)
			if err != nil {
				t.Fatal(err)
			}
			// kvClient is a wrapper around rawClient and to avoid leaking goroutines we need to
			// close the client (which we can do by closing rawClient).
			defer rawClient.Close()

			revision, err := strconv.Atoi(oldestUncompactedRv)
			if err != nil {
				t.Fatal(err)
			}
			_, err = kvClient.Compact(tCtx, int64(revision))
			if err != nil {
				t.Fatal(err)
			}

			listObj, err := rsClient.List(tCtx, metav1.ListOptions{
				Limit: 6,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			validContinueToken := listObj.Continue

			// test all combinations of these, for both watch cache enabled and disabled:
			limits := []int64{0, 6}
			continueTokens := []string{"", validContinueToken, invalidContinueToken}
			rvs := []string{"", "0", compactedRv, invalidResourceVersion}
			rvMatches := []metav1.ResourceVersionMatch{
				"",
				metav1.ResourceVersionMatchNotOlderThan,
				metav1.ResourceVersionMatchExact,
				invalidResourceVersionMatch,
			}

			for _, limit := range limits {
				for _, continueToken := range continueTokens {
					for _, rv := range rvs {
						for _, rvMatch := range rvMatches {
							rvName := ""
							switch rv {
							case "":
								rvName = "empty"
							case "0":
								rvName = "0"
							case compactedRv:
								rvName = "compacted"
							case invalidResourceVersion:
								rvName = "invalid"
							default:
								rvName = "unknown"
							}

							continueName := ""
							switch continueToken {
							case "":
								continueName = "empty"
							case validContinueToken:
								continueName = "valid"
							case invalidContinueToken:
								continueName = "invalid"
							default:
								continueName = "unknown"
							}

							name := fmt.Sprintf("limit=%d continue=%s rv=%s rvMatch=%s", limit, continueName, rvName, rvMatch)
							t.Run(name, func(t *testing.T) {
								opts := metav1.ListOptions{
									ResourceVersion:      rv,
									ResourceVersionMatch: rvMatch,
									Continue:             continueToken,
									Limit:                limit,
								}
								testListOptionsCase(t, rsClient, watchCacheEnabled, opts, compactedRv)
							})
						}
					}
				}
			}
		})
	}
}

func testListOptionsCase(t *testing.T, rsClient appsv1.ReplicaSetInterface, watchCacheEnabled bool, opts metav1.ListOptions, compactedRv string) {
	listObj, err := rsClient.List(context.Background(), opts)

	// check for expected validation errors
	if opts.ResourceVersion == "" && opts.ResourceVersionMatch != "" {
		if err == nil || !strings.Contains(err.Error(), "resourceVersionMatch is forbidden unless resourceVersion is provided") {
			t.Fatalf("expected forbidden error, but got: %v", err)
		}
		return
	}
	if opts.Continue != "" && opts.ResourceVersionMatch != "" {
		if err == nil || !strings.Contains(err.Error(), "resourceVersionMatch is forbidden when continue is provided") {
			t.Fatalf("expected forbidden error, but got: %v", err)
		}
		return
	}
	if opts.ResourceVersionMatch == invalidResourceVersionMatch {
		if err == nil || !strings.Contains(err.Error(), "supported values") {
			t.Fatalf("expected not supported error, but got: %v", err)
		}
		return
	}
	if opts.ResourceVersionMatch == metav1.ResourceVersionMatchExact && opts.ResourceVersion == "0" {
		if err == nil || !strings.Contains(err.Error(), "resourceVersionMatch \"exact\" is forbidden for resourceVersion \"0\"") {
			t.Fatalf("expected forbidden error, but got: %v", err)
		}
		return
	}
	if opts.Continue == invalidContinueToken {
		if err == nil || !strings.Contains(err.Error(), "continue key is not valid") {
			t.Fatalf("expected continue key not valid error, but got: %v", err)
		}
		return
	}
	// Should not be allowed for any resource version, but tightening the validation would be a breaking change
	if opts.Continue != "" && !(opts.ResourceVersion == "" || opts.ResourceVersion == "0") {
		if err == nil || !strings.Contains(err.Error(), "specifying resource version is not allowed when using continue") {
			t.Fatalf("expected not allowed error, but got: %v", err)
		}
		return
	}
	if opts.ResourceVersion == invalidResourceVersion {
		if err == nil || !strings.Contains(err.Error(), "Invalid value") {
			t.Fatalf("expecting invalid value error, but got: %v", err)
		}
		return
	}

	// Check for too old errors
	isExact := opts.ResourceVersionMatch == metav1.ResourceVersionMatchExact
	// Legacy corner cases that can be avoided by using an explicit resourceVersionMatch value
	// see https://kubernetes.io/docs/reference/using-api/api-concepts/#the-resourceversion-parameter
	isLegacyExact := opts.Limit > 0 && opts.ResourceVersionMatch == ""

	if opts.ResourceVersion == compactedRv && (isExact || isLegacyExact) {
		if err == nil || !strings.Contains(err.Error(), "The resourceVersion for the provided list is too old") {
			t.Fatalf("expected too old error, but got: %v", err)
		}
		return
	}

	// test successful responses
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	items, err := meta.ExtractList(listObj)
	if err != nil {
		t.Fatalf("Failed to extract list from %v", listObj)
	}
	count := int64(len(items))

	// Cacher.GetList defines this for logic to decide if the watch cache is skipped. We need to know it to know if
	// the limit is respected when testing here.
	hasContinuation := len(opts.Continue) > 0
	hasLimit := opts.Limit > 0 && opts.ResourceVersion != "0"
	skipWatchCache := opts.ResourceVersion == "" || hasContinuation || hasLimit || isExact
	usingWatchCache := watchCacheEnabled && !skipWatchCache

	if usingWatchCache { // watch cache does not respect limit and is not used for continue
		if count != 10 {
			t.Errorf("Expected list size to be 10 but got %d", count) // limit is ignored if watch cache is hit
		}
		return
	}

	if opts.Continue != "" {
		if count != 4 {
			t.Errorf("Expected list size of 4 but got %d", count)
		}
		return
	}
	if opts.Limit > 0 {
		if count != opts.Limit {
			t.Errorf("Expected list size to be limited to %d but got %d", opts.Limit, count)
		}
		return
	}
	if count != 10 {
		t.Errorf("Expected list size to be 10 but got %d", count)
	}
}

func TestListResourceVersion0(t *testing.T) {
	var testcases = []struct {
		name              string
		watchCacheEnabled bool
	}{
		{
			name:              "watchCacheOn",
			watchCacheEnabled: true,
		},
		{
			name:              "watchCacheOff",
			watchCacheEnabled: false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tCtx := ktesting.Init(t)

			clientSet, _, tearDownFn := framework.StartTestServer(tCtx, t, framework.TestServerSetup{
				ModifyServerRunOptions: func(opts *options.ServerRunOptions) {
					opts.Etcd.EnableWatchCache = tc.watchCacheEnabled
				},
			})
			defer tearDownFn()

			ns := framework.CreateNamespaceOrDie(clientSet, "list-paging", t)
			defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

			rsClient := clientSet.AppsV1().ReplicaSets(ns.Name)

			for i := 0; i < 10; i++ {
				rs := newRS(ns.Name)
				rs.Name = fmt.Sprintf("test-%d", i)
				if _, err := rsClient.Create(tCtx, rs, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}

			if tc.watchCacheEnabled {
				// poll until the watch cache has the full list in memory
				err := wait.PollImmediate(time.Second, wait.ForeverTestTimeout, func() (bool, error) {
					list, err := clientSet.AppsV1().ReplicaSets(ns.Name).List(tCtx, metav1.ListOptions{ResourceVersion: "0"})
					if err != nil {
						return false, err
					}
					return len(list.Items) == 10, nil
				})
				if err != nil {
					t.Fatalf("error waiting for watch cache to observe the full list: %v", err)
				}
			}

			pagerFn := func(opts metav1.ListOptions) (runtime.Object, error) {
				return rsClient.List(tCtx, opts)
			}

			p := pager.New(pager.SimplePageFunc(pagerFn))
			p.PageSize = 3
			listObj, _, err := p.List(tCtx, metav1.ListOptions{ResourceVersion: "0"})
			if err != nil {
				t.Fatalf("Unexpected list error: %v", err)
			}
			items, err := meta.ExtractList(listObj)
			if err != nil {
				t.Fatalf("Failed to extract list from %v", listObj)
			}
			if len(items) != 10 {
				t.Errorf("Expected list size of 10 but got %d", len(items))
			}
		})
	}
}

func TestAPIListChunking(t *testing.T) {
	ctx, clientSet, _, tearDownFn := setup(t)
	defer tearDownFn()

	ns := framework.CreateNamespaceOrDie(clientSet, "list-paging", t)
	defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

	rsClient := clientSet.AppsV1().ReplicaSets(ns.Name)

	for i := 0; i < 4; i++ {
		rs := newRS(ns.Name)
		rs.Name = fmt.Sprintf("test-%d", i)
		if _, err := rsClient.Create(ctx, rs, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	calls := 0
	firstRV := ""
	p := &pager.ListPager{
		PageSize: 1,
		PageFn: pager.SimplePageFunc(func(opts metav1.ListOptions) (runtime.Object, error) {
			calls++
			list, err := rsClient.List(ctx, opts)
			if err != nil {
				return nil, err
			}
			if calls == 1 {
				firstRV = list.ResourceVersion
			}
			if calls == 2 {
				rs := newRS(ns.Name)
				rs.Name = "test-5"
				if _, err := rsClient.Create(ctx, rs, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			return list, err
		}),
	}
	listObj, _, err := p.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("unexpected list invocations: %d", calls)
	}
	list := listObj.(metav1.ListInterface)
	if len(list.GetContinue()) != 0 {
		t.Errorf("unexpected continue: %s", list.GetContinue())
	}
	if list.GetResourceVersion() != firstRV {
		t.Errorf("unexpected resource version: %s instead of %s", list.GetResourceVersion(), firstRV)
	}
	var names []string
	if err := meta.EachListItem(listObj, func(obj runtime.Object) error {
		rs := obj.(*apps.ReplicaSet)
		names = append(names, rs.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"test-0", "test-1", "test-2", "test-3"}) {
		t.Errorf("unexpected items: %#v", list)
	}
}

func TestAPIListChunkingWithLabelSelector(t *testing.T) {
	ctx, clientSet, _, tearDownFn := setup(t)
	defer tearDownFn()

	ns := framework.CreateNamespaceOrDie(clientSet, "list-paging-with-label-selector", t)
	defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

	rsClient := clientSet.AppsV1().ReplicaSets(ns.Name)

	for i := 0; i < 10; i++ {
		rs := newRS(ns.Name)
		rs.Name = fmt.Sprintf("test-%d", i)
		odd := i%2 != 0
		rs.Labels = map[string]string{"odd-index": strconv.FormatBool(odd)}
		if _, err := rsClient.Create(ctx, rs, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	calls := 0
	firstRV := ""
	p := &pager.ListPager{
		PageSize: 1,
		PageFn: pager.SimplePageFunc(func(opts metav1.ListOptions) (runtime.Object, error) {
			calls++
			list, err := rsClient.List(ctx, opts)
			if err != nil {
				return nil, err
			}
			if calls == 1 {
				firstRV = list.ResourceVersion
			}
			return list, err
		}),
	}
	listObj, _, err := p.List(ctx, metav1.ListOptions{LabelSelector: "odd-index=true", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("unexpected list invocations: %d", calls)
	}
	list := listObj.(metav1.ListInterface)
	if len(list.GetContinue()) != 0 {
		t.Errorf("unexpected continue: %s", list.GetContinue())
	}
	if list.GetResourceVersion() != firstRV {
		t.Errorf("unexpected resource version: %s instead of %s", list.GetResourceVersion(), firstRV)
	}
	var names []string
	if err := meta.EachListItem(listObj, func(obj runtime.Object) error {
		rs := obj.(*apps.ReplicaSet)
		names = append(names, rs.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"test-1", "test-3", "test-5", "test-7", "test-9"}) {
		t.Errorf("unexpected items: %#v", list)
	}
}

func makeSecret(name string) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Data: map[string][]byte{
			"key": []byte("value"),
		},
	}
}

func TestNameInFieldSelector(t *testing.T) {
	ctx, clientSet, _, tearDownFn := setup(t)
	defer tearDownFn()

	numNamespaces := 3
	for i := 0; i < 3; i++ {
		ns := framework.CreateNamespaceOrDie(clientSet, fmt.Sprintf("ns%d", i), t)
		defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

		_, err := clientSet.CoreV1().Secrets(ns.Name).Create(ctx, makeSecret("foo"), metav1.CreateOptions{})
		if err != nil {
			t.Errorf("Couldn't create secret: %v", err)
		}
		_, err = clientSet.CoreV1().Secrets(ns.Name).Create(ctx, makeSecret("bar"), metav1.CreateOptions{})
		if err != nil {
			t.Errorf("Couldn't create secret: %v", err)
		}
	}

	testcases := []struct {
		namespace       string
		selector        string
		expectedSecrets int
	}{
		{
			namespace:       "",
			selector:        "metadata.name=foo",
			expectedSecrets: numNamespaces,
		},
		{
			namespace:       "",
			selector:        "metadata.name=foo,metadata.name=bar",
			expectedSecrets: 0,
		},
		{
			namespace:       "",
			selector:        "metadata.name=foo,metadata.namespace=ns1",
			expectedSecrets: 1,
		},
		{
			namespace:       "ns1",
			selector:        "metadata.name=foo,metadata.namespace=ns1",
			expectedSecrets: 1,
		},
		{
			namespace:       "ns1",
			selector:        "metadata.name=foo,metadata.namespace=ns2",
			expectedSecrets: 0,
		},
		{
			namespace:       "ns1",
			selector:        "metadata.name=foo,metadata.namespace=",
			expectedSecrets: 0,
		},
	}

	for _, tc := range testcases {
		opts := metav1.ListOptions{
			FieldSelector: tc.selector,
		}
		secrets, err := clientSet.CoreV1().Secrets(tc.namespace).List(ctx, opts)
		if err != nil {
			t.Errorf("%s: Unexpected error: %v", tc.selector, err)
		}
		if len(secrets.Items) != tc.expectedSecrets {
			t.Errorf("%s: Unexpected number of secrets: %d, expected: %d", tc.selector, len(secrets.Items), tc.expectedSecrets)
		}
	}
}

type callWrapper struct {
	nested http.RoundTripper
	req    *http.Request
	resp   *http.Response
	err    error
}

func (w *callWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	w.req = req
	resp, err := w.nested.RoundTrip(req)
	w.resp = resp
	w.err = err
	return resp, err
}

func TestMetadataClient(t *testing.T) {
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	ctx, clientset, kubeConfig, tearDownFn := setup(t)
	defer tearDownFn()

	apiExtensionClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	fooCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foos.cr.bar.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "cr.bar.com",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "foos",
				Kind:   "Foo",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema:  fixtures.AllowAllSchema(),
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
				},
			},
		},
	}
	fooCRD, err = fixtures.CreateNewV1CustomResourceDefinition(fooCRD, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	crdGVR := schema.GroupVersionResource{Group: fooCRD.Spec.Group, Version: fooCRD.Spec.Versions[0].Name, Resource: "foos"}

	testcases := []struct {
		name string
		want func(*testing.T)
	}{
		{
			name: "list, get, patch, and delete via metadata client",
			want: func(t *testing.T) {
				ns := "metadata-builtin"
				namespace := framework.CreateNamespaceOrDie(clientset, ns, t)
				defer framework.DeleteNamespaceOrDie(clientset, namespace, t)

				svc, err := clientset.CoreV1().Services(ns).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-1", Annotations: map[string]string{"foo": "bar"}}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create service: %v", err)
				}

				cfg := metadata.ConfigFor(kubeConfig)
				wrapper := &callWrapper{}
				cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
					wrapper.nested = rt
					return wrapper
				})

				client := metadata.NewForConfigOrDie(cfg).Resource(v1.SchemeGroupVersion.WithResource("services"))
				items, err := client.Namespace(ns).List(ctx, metav1.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if items.ResourceVersion == "" {
					t.Fatalf("unexpected items: %#v", items)
				}
				if len(items.Items) != 1 {
					t.Fatalf("unexpected list: %#v", items)
				}
				if item := items.Items[0]; item.Name != "test-1" || item.UID != svc.UID || item.Annotations["foo"] != "bar" {
					t.Fatalf("unexpected object: %#v", item)
				}

				if wrapper.resp == nil || wrapper.resp.Header.Get("Content-Type") != "application/vnd.kubernetes.protobuf" {
					t.Fatalf("unexpected response: %#v", wrapper.resp)
				}
				wrapper.resp = nil

				item, err := client.Namespace(ns).Get(ctx, "test-1", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if item.ResourceVersion == "" || item.UID != svc.UID || item.Annotations["foo"] != "bar" {
					t.Fatalf("unexpected object: %#v", item)
				}
				if wrapper.resp == nil || wrapper.resp.Header.Get("Content-Type") != "application/vnd.kubernetes.protobuf" {
					t.Fatalf("unexpected response: %#v", wrapper.resp)
				}

				item, err = client.Namespace(ns).Patch(ctx, "test-1", types.MergePatchType, []byte(`{"metadata":{"annotations":{"foo":"baz"}}}`), metav1.PatchOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if item.Annotations["foo"] != "baz" {
					t.Fatalf("unexpected object: %#v", item)
				}

				if err := client.Namespace(ns).Delete(ctx, "test-1", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &item.UID}}); err != nil {
					t.Fatal(err)
				}

				if _, err := client.Namespace(ns).Get(ctx, "test-1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "list, get, patch, and delete via metadata client on a CRD",
			want: func(t *testing.T) {
				ns := "metadata-crd"
				crclient := dynamicClient.Resource(crdGVR).Namespace(ns)
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "cr.bar.com/v1",
						"kind":       "Foo",
						"spec":       map[string]interface{}{"field": 1},
						"metadata": map[string]interface{}{
							"name": "test-1",
							"annotations": map[string]interface{}{
								"foo": "bar",
							},
						},
					},
				}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}

				cfg := metadata.ConfigFor(config)
				wrapper := &callWrapper{}
				cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
					wrapper.nested = rt
					return wrapper
				})

				client := metadata.NewForConfigOrDie(cfg).Resource(crdGVR)
				items, err := client.Namespace(ns).List(ctx, metav1.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if items.ResourceVersion == "" {
					t.Fatalf("unexpected items: %#v", items)
				}
				if len(items.Items) != 1 {
					t.Fatalf("unexpected list: %#v", items)
				}
				if item := items.Items[0]; item.Name != "test-1" || item.UID != cr.GetUID() || item.Annotations["foo"] != "bar" {
					t.Fatalf("unexpected object: %#v", item)
				}

				if wrapper.resp == nil || wrapper.resp.Header.Get("Content-Type") != "application/vnd.kubernetes.protobuf" {
					t.Fatalf("unexpected response: %#v", wrapper.resp)
				}
				wrapper.resp = nil

				item, err := client.Namespace(ns).Get(ctx, "test-1", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if item.ResourceVersion == "" || item.UID != cr.GetUID() || item.Annotations["foo"] != "bar" {
					t.Fatalf("unexpected object: %#v", item)
				}
				if wrapper.resp == nil || wrapper.resp.Header.Get("Content-Type") != "application/vnd.kubernetes.protobuf" {
					t.Fatalf("unexpected response: %#v", wrapper.resp)
				}

				item, err = client.Namespace(ns).Patch(ctx, "test-1", types.MergePatchType, []byte(`{"metadata":{"annotations":{"foo":"baz"}}}`), metav1.PatchOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if item.Annotations["foo"] != "baz" {
					t.Fatalf("unexpected object: %#v", item)
				}

				if err := client.Namespace(ns).Delete(ctx, "test-1", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &item.UID}}); err != nil {
					t.Fatal(err)
				}
				if _, err := client.Namespace(ns).Get(ctx, "test-1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "watch via metadata client",
			want: func(t *testing.T) {
				ns := "metadata-watch"
				namespace := framework.CreateNamespaceOrDie(clientset, ns, t)
				defer framework.DeleteNamespaceOrDie(clientset, namespace, t)

				svc, err := clientset.CoreV1().Services(ns).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-2", Annotations: map[string]string{"foo": "bar"}}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create service: %v", err)
				}
				if _, err := clientset.CoreV1().Services(ns).Patch(ctx, "test-2", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}

				cfg := metadata.ConfigFor(kubeConfig)
				wrapper := &callWrapper{}
				cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
					wrapper.nested = rt
					return wrapper
				})

				client := metadata.NewForConfigOrDie(cfg).Resource(v1.SchemeGroupVersion.WithResource("services"))
				w, err := client.Namespace(ns).Watch(ctx, metav1.ListOptions{ResourceVersion: svc.ResourceVersion, Watch: true})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Stop()
				var r watch.Event
				select {
				case evt, ok := <-w.ResultChan():
					if !ok {
						t.Fatal("watch closed")
					}
					r = evt
				case <-time.After(5 * time.Second):
					t.Fatal("no watch event in 5 seconds, bug")
				}
				if r.Type != watch.Modified {
					t.Fatalf("unexpected watch: %#v", r)
				}
				item, ok := r.Object.(*metav1.PartialObjectMetadata)
				if !ok {
					t.Fatalf("unexpected object: %T", item)
				}
				if item.ResourceVersion == "" || item.Name != "test-2" || item.UID != svc.UID || item.Annotations["test"] != "1" {
					t.Fatalf("unexpected object: %#v", item)
				}

				if wrapper.resp == nil || wrapper.resp.Header.Get("Content-Type") != "application/vnd.kubernetes.protobuf;stream=watch" {
					t.Fatalf("unexpected response: %#v", wrapper.resp)
				}
			},
		},

		{
			name: "watch via metadata client on a CRD",
			want: func(t *testing.T) {
				ns := "metadata-watch-crd"
				crclient := dynamicClient.Resource(crdGVR).Namespace(ns)
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "cr.bar.com/v1",
						"kind":       "Foo",
						"spec":       map[string]interface{}{"field": 1},
						"metadata": map[string]interface{}{
							"name": "test-2",
							"annotations": map[string]interface{}{
								"foo": "bar",
							},
						},
					},
				}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}

				cfg := metadata.ConfigFor(config)
				client := metadata.NewForConfigOrDie(cfg).Resource(crdGVR)

				patched, err := client.Namespace(ns).Patch(ctx, "test-2", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if patched.GetResourceVersion() == cr.GetResourceVersion() {
					t.Fatalf("Patch did not modify object: %#v", patched)
				}

				wrapper := &callWrapper{}
				cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
					wrapper.nested = rt
					return wrapper
				})
				client = metadata.NewForConfigOrDie(cfg).Resource(crdGVR)

				w, err := client.Namespace(ns).Watch(ctx, metav1.ListOptions{ResourceVersion: cr.GetResourceVersion(), Watch: true})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Stop()
				var r watch.Event
				select {
				case evt, ok := <-w.ResultChan():
					if !ok {
						t.Fatal("watch closed")
					}
					r = evt
				case <-time.After(5 * time.Second):
					t.Fatal("no watch event in 5 seconds, bug")
				}
				if r.Type != watch.Modified {
					t.Fatalf("unexpected watch: %#v", r)
				}
				item, ok := r.Object.(*metav1.PartialObjectMetadata)
				if !ok {
					t.Fatalf("unexpected object: %T", item)
				}
				if item.ResourceVersion == "" || item.Name != "test-2" || item.UID != cr.GetUID() || item.Annotations["test"] != "1" {
					t.Fatalf("unexpected object: %#v", item)
				}

				if wrapper.resp == nil || wrapper.resp.Header.Get("Content-Type") != "application/vnd.kubernetes.protobuf;stream=watch" {
					t.Fatalf("unexpected response: %#v", wrapper.resp)
				}
			},
		},
	}

	for i := range testcases {
		tc := testcases[i]
		t.Run(tc.name, func(t *testing.T) {
			tc.want(t)
		})
	}
}

func TestAPICRDProtobuf(t *testing.T) {
	testNamespace := "test-api-crd-protobuf"
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	ctx, _, kubeConfig, tearDownFn := setup(t)
	defer tearDownFn()

	apiExtensionClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	fooCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foos.cr.bar.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "cr.bar.com",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "foos",
				Kind:   "Foo",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:         "v1",
					Served:       true,
					Storage:      true,
					Schema:       fixtures.AllowAllSchema(),
					Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				},
			},
		},
	}
	fooCRD, err = fixtures.CreateNewV1CustomResourceDefinition(fooCRD, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	crdGVR := schema.GroupVersionResource{Group: fooCRD.Spec.Group, Version: fooCRD.Spec.Versions[0].Name, Resource: "foos"}
	crclient := dynamicClient.Resource(crdGVR).Namespace(testNamespace)

	testcases := []struct {
		name        string
		accept      string
		subresource string
		object      func(*testing.T) (metav1.Object, string, string)
		wantErr     func(*testing.T, error)
		wantBody    func(*testing.T, io.Reader)
	}{
		{
			name:   "server returns 406 when asking for protobuf for CRDs, which dynamic client does not support",
			accept: "application/vnd.kubernetes.protobuf",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-1"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-1", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
				status := err.(apierrors.APIStatus).Status()
				data, _ := json.MarshalIndent(status, "", "  ")
				// because the dynamic client only has a json serializer, the client processing of the error cannot
				// turn the response into something meaningful, so we verify that fallback handling works correctly
				if !apierrors.IsUnexpectedServerError(err) {
					t.Fatal(string(data))
				}
				if status.Message != "the server was unable to respond with a content type that the client supports (get foos.cr.bar.com test-1)" {
					t.Fatal(string(data))
				}
			},
		},
		{
			name:   "server returns JSON when asking for protobuf and json for CRDs",
			accept: "application/vnd.kubernetes.protobuf,application/json",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "spec": map[string]interface{}{"field": 1}, "metadata": map[string]interface{}{"name": "test-2"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-2", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				obj := &unstructured.Unstructured{}
				if err := json.NewDecoder(w).Decode(obj); err != nil {
					t.Fatal(err)
				}
				v, ok, err := unstructured.NestedInt64(obj.UnstructuredContent(), "spec", "field")
				if !ok || err != nil {
					data, _ := json.MarshalIndent(obj.UnstructuredContent(), "", "  ")
					t.Fatalf("err=%v ok=%t json=%s", err, ok, string(data))
				}
				if v != 1 {
					t.Fatalf("unexpected body: %#v", obj.UnstructuredContent())
				}
			},
		},
		{
			name:        "server returns 406 when asking for protobuf for CRDs status, which dynamic client does not support",
			accept:      "application/vnd.kubernetes.protobuf",
			subresource: "status",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-3"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-3", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"3"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
				status := err.(apierrors.APIStatus).Status()
				data, _ := json.MarshalIndent(status, "", "  ")
				// because the dynamic client only has a json serializer, the client processing of the error cannot
				// turn the response into something meaningful, so we verify that fallback handling works correctly
				if !apierrors.IsUnexpectedServerError(err) {
					t.Fatal(string(data))
				}
				if status.Message != "the server was unable to respond with a content type that the client supports (get foos.cr.bar.com test-3)" {
					t.Fatal(string(data))
				}
			},
		},
		{
			name:        "server returns JSON when asking for protobuf and json for CRDs status",
			accept:      "application/vnd.kubernetes.protobuf,application/json",
			subresource: "status",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "spec": map[string]interface{}{"field": 1}, "metadata": map[string]interface{}{"name": "test-4"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-4", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"4"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				obj := &unstructured.Unstructured{}
				if err := json.NewDecoder(w).Decode(obj); err != nil {
					t.Fatal(err)
				}
				v, ok, err := unstructured.NestedInt64(obj.UnstructuredContent(), "spec", "field")
				if !ok || err != nil {
					data, _ := json.MarshalIndent(obj.UnstructuredContent(), "", "  ")
					t.Fatalf("err=%v ok=%t json=%s", err, ok, string(data))
				}
				if v != 1 {
					t.Fatalf("unexpected body: %#v", obj.UnstructuredContent())
				}
			},
		},
	}

	for i := range testcases {
		tc := testcases[i]
		t.Run(tc.name, func(t *testing.T) {
			obj, group, resource := tc.object(t)

			cfg := dynamic.ConfigFor(config)
			if len(group) == 0 {
				cfg = dynamic.ConfigFor(kubeConfig)
				cfg.APIPath = "/api"
			} else {
				cfg.APIPath = "/apis"
			}
			cfg.GroupVersion = &schema.GroupVersion{Group: group, Version: "v1"}
			client, err := restclient.RESTClientFor(cfg)
			if err != nil {
				t.Fatal(err)
			}

			w, err := client.Get().
				Resource(resource).NamespaceIfScoped(obj.GetNamespace(), len(obj.GetNamespace()) > 0).Name(obj.GetName()).SubResource(tc.subresource).
				SetHeader("Accept", tc.accept).
				Stream(ctx)
			if (tc.wantErr != nil) != (err != nil) {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil {
				tc.wantErr(t, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			tc.wantBody(t, w)
		})
	}
}

func TestGetSubresourcesAsTables(t *testing.T) {
	testNamespace := "test-transform"
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	ctx, clientset, kubeConfig, tearDownFn := setup(t)
	defer tearDownFn()

	ns := framework.CreateNamespaceOrDie(clientset, testNamespace, t)
	defer framework.DeleteNamespaceOrDie(clientset, ns, t)

	apiExtensionClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	fooWithSubresourceCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foosubs.cr.bar.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "cr.bar.com",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "foosubs",
				Kind:   "FooSub",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"replicas": {
											Type: "integer",
										},
									},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"replicas": {
											Type: "integer",
										},
									},
								},
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
						Scale: &apiextensionsv1.CustomResourceSubresourceScale{
							SpecReplicasPath:   ".spec.replicas",
							StatusReplicasPath: ".status.replicas",
						},
					},
				},
			},
		},
	}

	fooWithSubresourceCRD, err = fixtures.CreateNewV1CustomResourceDefinition(fooWithSubresourceCRD, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	subresourcesCrdGVR := schema.GroupVersionResource{Group: fooWithSubresourceCRD.Spec.Group, Version: fooWithSubresourceCRD.Spec.Versions[0].Name, Resource: "foosubs"}
	subresourcesCrclient := dynamicClient.Resource(subresourcesCrdGVR).Namespace(testNamespace)

	testcases := []struct {
		name        string
		accept      string
		object      func(*testing.T) (metav1.Object, string, string)
		subresource string
	}{
		{
			name:   "v1 verify status subresource returns a table for CRDs",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := subresourcesCrclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "FooSub", "metadata": map[string]interface{}{"name": "test-1"}, "spec": map[string]interface{}{"replicas": 2}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				return cr, subresourcesCrdGVR.Group, "foosubs"
			},
			subresource: "status",
		},
		{
			name:   "v1 verify scale subresource returns a table for CRDs",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := subresourcesCrclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "FooSub", "metadata": map[string]interface{}{"name": "test-2"}, "spec": map[string]interface{}{"replicas": 2}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				return cr, subresourcesCrdGVR.Group, "foosubs"
			},
			subresource: "scale",
		},
		{
			name:   "verify status subresource returns a table for replicationcontrollers",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				rc := &v1.ReplicationController{
					ObjectMeta: metav1.ObjectMeta{
						Name: "replicationcontroller-1",
					},
					Spec: v1.ReplicationControllerSpec{
						Replicas: int32Ptr(2),
						Selector: map[string]string{
							"label": "test-label",
						},
						Template: &v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"label": "test-label",
								},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{Name: "test-name", Image: "nonexistent-image"},
								},
							},
						},
					},
				}
				rc, err := clientset.CoreV1().ReplicationControllers(testNamespace).Create(ctx, rc, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create replicationcontroller: %v", err)
				}
				return rc, "", "replicationcontrollers"
			},
			subresource: "status",
		},
		{
			name:   "verify scale subresource returns a table for replicationcontrollers",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				rc := &v1.ReplicationController{
					ObjectMeta: metav1.ObjectMeta{
						Name: "replicationcontroller-2",
					},
					Spec: v1.ReplicationControllerSpec{
						Replicas: int32Ptr(2),
						Selector: map[string]string{
							"label": "test-label",
						},
						Template: &v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"label": "test-label",
								},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{Name: "test-name", Image: "nonexistent-image"},
								},
							},
						},
					},
				}
				rc, err := clientset.CoreV1().ReplicationControllers(testNamespace).Create(ctx, rc, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create replicationcontroller: %v", err)
				}
				return rc, "", "replicationcontrollers"
			},
			subresource: "scale",
		},
	}

	for i := range testcases {
		tc := testcases[i]
		t.Run(tc.name, func(t *testing.T) {
			obj, group, resource := tc.object(t)

			cfg := dynamic.ConfigFor(config)
			if len(group) == 0 {
				cfg = dynamic.ConfigFor(kubeConfig)
				cfg.APIPath = "/api"
			} else {
				cfg.APIPath = "/apis"
			}
			cfg.GroupVersion = &schema.GroupVersion{Group: group, Version: "v1"}

			client, err := restclient.RESTClientFor(cfg)
			if err != nil {
				t.Fatal(err)
			}

			res := client.Get().
				Resource(resource).NamespaceIfScoped(obj.GetNamespace(), len(obj.GetNamespace()) > 0).
				SetHeader("Accept", tc.accept).
				Name(obj.GetName()).
				SubResource(tc.subresource).
				Do(ctx)

			resObj, err := res.Get()
			if err != nil {
				t.Fatalf("failed to retrieve object from response: %v", err)
			}
			actualKind := resObj.GetObjectKind().GroupVersionKind().Kind
			if actualKind != "Table" {
				t.Fatalf("Expected Kind 'Table', got '%v'", actualKind)
			}
		})
	}
}

func TestGetScaleSubresourceAsTableForAllBuiltins(t *testing.T) {
	// KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE allows for APIs pending removal to not block tests
	t.Setenv("KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE", "true")

	// Enable all features for testing
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, "AllAlpha", true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, "AllBeta", true)

	testNamespace := "test-scale"
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()

	clientset := clientset.NewForConfigOrDie(server.ClientConfig)

	ns := framework.CreateNamespaceOrDie(clientset, testNamespace, t)
	defer framework.DeleteNamespaceOrDie(clientset, ns, t)

	scaleResources := map[string]runtime.Object{
		"deployments.apps/v1": &apps.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dummy",
			},
			Spec: apps.DeploymentSpec{
				Replicas: ptr.To[int32](1),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "dummy",
					},
				},
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "dummy",
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "dummy-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
		"replicasets.apps/v1": &apps.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dummy",
			},
			Spec: apps.ReplicaSetSpec{
				Replicas: ptr.To[int32](1),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "dummy",
					},
				},
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "dummy",
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "dummy-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
		"statefulsets.apps/v1": &apps.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dummy",
			},
			Spec: apps.StatefulSetSpec{
				ServiceName: "dummy",
				Replicas:    ptr.To[int32](1),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "dummy",
					},
				},
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "dummy",
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "dummy-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
		"replicationcontrollers.v1": &v1.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dummy",
			},
			Spec: v1.ReplicationControllerSpec{
				Replicas: ptr.To[int32](1),
				Selector: map[string]string{
					"app": "dummy",
				},
				Template: &v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "dummy",
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "dummy-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
	}

	_, serverResources, err := clientset.Discovery().ServerGroupsAndResources()
	if err != nil {
		t.Fatal(err)
	}

	for _, resourceList := range serverResources {
		for _, resource := range resourceList.APIResources {
			if !strings.Contains(resource.Name, "/scale") {
				continue
			}
			resourceName := strings.Split(resource.Name, "/")[0]
			// we can safely skip error here, since the group version is coming from the server
			groupVersion, _ := schema.ParseGroupVersion(resourceList.GroupVersion)

			t.Run(resourceName, func(t *testing.T) {
				_, ctx := ktesting.NewTestContext(t)
				scaleResourceObjName := fmt.Sprintf("%s.%s", resourceName, resourceList.GroupVersion)
				obj, ok := scaleResources[scaleResourceObjName]
				if !ok {
					t.Fatalf("sample resource for %s not found in scaleResources", scaleResourceObjName)
				}

				cfg := dynamic.ConfigFor(server.ClientConfig)
				if len(groupVersion.Group) == 0 {
					cfg = dynamic.ConfigFor(server.ClientConfig)
					cfg.APIPath = "/api"
				} else {
					cfg.APIPath = "/apis"
				}
				cfg.GroupVersion = &groupVersion
				client, err := restclient.RESTClientFor(cfg)
				if err != nil {
					t.Fatalf("failed to create RESTClient: %v", err)
				}

				err = client.Post().
					NamespaceIfScoped(testNamespace, resource.Namespaced).Resource(resourceName).
					VersionedParams(&metav1.CreateOptions{}, metav1.ParameterCodec).
					Body(obj).Do(ctx).Error()
				if err != nil {
					t.Fatalf("failed to create object: %v", err)
				}

				metaAccessor, err := meta.Accessor(obj)
				if err != nil {
					t.Fatalf("failed to get metadata accessor: %v", err)
				}
				res := client.Get().
					NamespaceIfScoped(testNamespace, resource.Namespaced).Resource(resourceName).
					SetHeader("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1").
					Name(metaAccessor.GetName()).
					SubResource("scale").
					Do(ctx)
				resObj, err := res.Get()
				if err != nil {
					t.Fatalf("failed to retrieve object from response: %v", err)
				}
				actualKind := resObj.GetObjectKind().GroupVersionKind().Kind
				if actualKind != "Table" {
					t.Fatalf("Expected Kind 'Table', got '%v'", actualKind)
				}
			})
		}
	}
}

func TestTransform(t *testing.T) {
	testNamespace := "test-transform"
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	ctx, clientset, kubeConfig, tearDownFn := setup(t)
	defer tearDownFn()

	ns := framework.CreateNamespaceOrDie(clientset, testNamespace, t)
	defer framework.DeleteNamespaceOrDie(clientset, ns, t)

	apiExtensionClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	fooCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foos.cr.bar.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "cr.bar.com",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "foos",
				Kind:   "Foo",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema:  fixtures.AllowAllSchema(),
				},
			},
		},
	}
	fooCRD, err = fixtures.CreateNewV1CustomResourceDefinition(fooCRD, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	crdGVR := schema.GroupVersionResource{Group: fooCRD.Spec.Group, Version: fooCRD.Spec.Versions[0].Name, Resource: "foos"}
	crclient := dynamicClient.Resource(crdGVR).Namespace(testNamespace)

	previousList, err := crclient.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list CRs before test: %v", err)
	}
	previousRV := previousList.GetResourceVersion()

	testcases := []struct {
		name          string
		accept        string
		includeObject metav1.IncludeObjectPolicy
		object        func(*testing.T) (metav1.Object, string, string)
		wantErr       func(*testing.T, error)
		wantBody      func(*testing.T, io.Reader)
	}{
		{
			name:   "v1beta1 verify columns on cluster scoped resources",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "default", Namespace: ""}, "", "namespaces"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableWatchEvents(t, 1, 3, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:   "v1beta1 verify columns on CRDs in json",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-1"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-1", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableWatchEvents(t, 2, 2, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:   "v1beta1 verify columns on CRDs in json;stream=watch",
			accept: "application/json;stream=watch;as=Table;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-2"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-2", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableWatchEvents(t, 2, 2, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:   "v1beta1 verify columns on CRDs in yaml",
			accept: "application/yaml;as=Table;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-3"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-3", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
				// TODO: this should be a more specific error
				if err.Error() != "only the following media types are accepted: application/json;stream=watch, application/vnd.kubernetes.protobuf;stream=watch" {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1beta1 verify columns on services",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				svc, err := clientset.CoreV1().Services(testNamespace).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-1"}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create service: %v", err)
				}
				if _, err := clientset.CoreV1().Services(testNamespace).Patch(ctx, svc.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update service: %v", err)
				}
				return svc, "", "services"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableWatchEvents(t, 2, 7, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:          "v1beta1 verify columns on services with no object",
			accept:        "application/json;as=Table;g=meta.k8s.io;v=v1beta1",
			includeObject: metav1.IncludeNone,
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().Services(testNamespace).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-2"}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().Services(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "services"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableWatchEvents(t, 2, 7, metav1.IncludeNone, json.NewDecoder(w))
			},
		},
		{
			name:          "v1beta1 verify columns on services with full object",
			accept:        "application/json;as=Table;g=meta.k8s.io;v=v1beta1",
			includeObject: metav1.IncludeObject,
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().Services(testNamespace).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-3"}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().Services(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "services"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				objects := expectTableWatchEvents(t, 2, 7, metav1.IncludeObject, json.NewDecoder(w))
				var svc v1.Service
				if err := json.Unmarshal(objects[1], &svc); err != nil {
					t.Fatal(err)
				}
				if svc.Annotations["test"] != "1" || svc.Spec.Ports[0].Port != 1000 {
					t.Fatalf("unexpected object: %#v", svc)
				}
			},
		},
		{
			name:   "v1beta1 verify partial metadata object on config maps",
			accept: "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().ConfigMaps(testNamespace).Create(ctx, &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-1", Annotations: map[string]string{"test": "0"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().ConfigMaps(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "configmaps"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectPartialObjectMetaEvents(t, json.NewDecoder(w), "0", "1")
			},
		},
		{
			name:   "v1beta1 verify partial metadata object on config maps in protobuf",
			accept: "application/vnd.kubernetes.protobuf;as=PartialObjectMetadata;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().ConfigMaps(testNamespace).Create(ctx, &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-2", Annotations: map[string]string{"test": "0"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().ConfigMaps(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "configmaps"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectPartialObjectMetaEventsProtobuf(t, w, "0", "1")
			},
		},
		{
			name:   "v1beta1 verify partial metadata object on CRDs in protobuf",
			accept: "application/vnd.kubernetes.protobuf;as=PartialObjectMetadata;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-4", "annotations": map[string]string{"test": "0"}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-4", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectPartialObjectMetaEventsProtobuf(t, w, "0", "1")
			},
		},
		{
			name:   "v1beta1 verify error on unsupported mimetype protobuf for table conversion",
			accept: "application/vnd.kubernetes.protobuf;as=Table;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
				// TODO: this should be a more specific error
				if err.Error() != "only the following media types are accepted: application/json, application/yaml, application/vnd.kubernetes.protobuf" {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "verify error on invalid mimetype - bad version",
			accept: "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1alpha1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1beta1 verify error on invalid mimetype - bad group",
			accept: "application/json;as=PartialObjectMetadata;g=k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1beta1 verify error on invalid mimetype - bad kind",
			accept: "application/json;as=PartialObject;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1beta1 verify error on invalid mimetype - missing kind",
			accept: "application/json;g=meta.k8s.io;v=v1beta1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:          "v1beta1 verify error on invalid transform parameter",
			accept:        "application/json;as=Table;g=meta.k8s.io;v=v1beta1",
			includeObject: metav1.IncludeObjectPolicy("unrecognized"),
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsBadRequest(err) || !strings.Contains(err.Error(), `Invalid value: "unrecognized": must be 'Metadata', 'Object', 'None', or empty`) {
					t.Fatal(err)
				}
			},
		},

		{
			name:   "v1 verify columns on cluster scoped resources",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "default", Namespace: ""}, "", "namespaces"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableV1WatchEvents(t, 1, 3, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:   "v1 verify columns on CRDs in json",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-5"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-5", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableV1WatchEvents(t, 2, 2, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:   "v1 verify columns on CRDs in json;stream=watch",
			accept: "application/json;stream=watch;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-6"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-6", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableV1WatchEvents(t, 2, 2, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:   "v1 verify columns on CRDs in yaml",
			accept: "application/yaml;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-7"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, "test-7", types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
				// TODO: this should be a more specific error
				if err.Error() != "only the following media types are accepted: application/json;stream=watch, application/vnd.kubernetes.protobuf;stream=watch" {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1 verify columns on services",
			accept: "application/json;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				svc, err := clientset.CoreV1().Services(testNamespace).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-5"}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create service: %v", err)
				}
				if _, err := clientset.CoreV1().Services(testNamespace).Patch(ctx, svc.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update service: %v", err)
				}
				return svc, "", "services"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableV1WatchEvents(t, 2, 7, metav1.IncludeMetadata, json.NewDecoder(w))
			},
		},
		{
			name:          "v1 verify columns on services with no object",
			accept:        "application/json;as=Table;g=meta.k8s.io;v=v1",
			includeObject: metav1.IncludeNone,
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().Services(testNamespace).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-6"}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().Services(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "services"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectTableV1WatchEvents(t, 2, 7, metav1.IncludeNone, json.NewDecoder(w))
			},
		},
		{
			name:          "v1 verify columns on services with full object",
			accept:        "application/json;as=Table;g=meta.k8s.io;v=v1",
			includeObject: metav1.IncludeObject,
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().Services(testNamespace).Create(ctx, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-7"}, Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 1000}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().Services(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "services"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				objects := expectTableV1WatchEvents(t, 2, 7, metav1.IncludeObject, json.NewDecoder(w))
				var svc v1.Service
				if err := json.Unmarshal(objects[1], &svc); err != nil {
					t.Fatal(err)
				}
				if svc.Annotations["test"] != "1" || svc.Spec.Ports[0].Port != 1000 {
					t.Fatalf("unexpected object: %#v", svc)
				}
			},
		},
		{
			name:   "v1 verify partial metadata object on config maps",
			accept: "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().ConfigMaps(testNamespace).Create(ctx, &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-3", Annotations: map[string]string{"test": "0"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().ConfigMaps(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "configmaps"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectPartialObjectMetaV1Events(t, json.NewDecoder(w), "0", "1")
			},
		},
		{
			name:   "v1 verify partial metadata object on config maps in protobuf",
			accept: "application/vnd.kubernetes.protobuf;as=PartialObjectMetadata;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				obj, err := clientset.CoreV1().ConfigMaps(testNamespace).Create(ctx, &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-4", Annotations: map[string]string{"test": "0"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create object: %v", err)
				}
				if _, err := clientset.CoreV1().ConfigMaps(testNamespace).Patch(ctx, obj.Name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to update object: %v", err)
				}
				return obj, "", "configmaps"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectPartialObjectMetaV1EventsProtobuf(t, w, "0", "1")
			},
		},
		{
			name:   "v1 verify partial metadata object on CRDs in protobuf",
			accept: "application/vnd.kubernetes.protobuf;as=PartialObjectMetadata;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				cr, err := crclient.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "cr.bar.com/v1", "kind": "Foo", "metadata": map[string]interface{}{"name": "test-8", "annotations": map[string]string{"test": "0"}}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unable to create cr: %v", err)
				}
				if _, err := crclient.Patch(ctx, cr.GetName(), types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"1"}}}`), metav1.PatchOptions{}); err != nil {
					t.Fatalf("unable to patch cr: %v", err)
				}
				return cr, crdGVR.Group, "foos"
			},
			wantBody: func(t *testing.T, w io.Reader) {
				expectPartialObjectMetaV1EventsProtobuf(t, w, "0", "1")
			},
		},
		{
			name:   "v1 verify error on unsupported mimetype protobuf for table conversion",
			accept: "application/vnd.kubernetes.protobuf;as=Table;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
				// TODO: this should be a more specific error
				if err.Error() != "only the following media types are accepted: application/json, application/yaml, application/vnd.kubernetes.protobuf" {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1 verify error on invalid mimetype - bad group",
			accept: "application/json;as=PartialObjectMetadata;g=k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1 verify error on invalid mimetype - bad kind",
			accept: "application/json;as=PartialObject;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1 verify error on invalid mimetype - only meta kinds accepted",
			accept: "application/json;as=Service;g=;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v1 verify error on invalid mimetype - missing kind",
			accept: "application/json;g=meta.k8s.io;v=v1",
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsNotAcceptable(err) {
					t.Fatal(err)
				}
			},
		},
		{
			name:          "v1 verify error on invalid transform parameter",
			accept:        "application/json;as=Table;g=meta.k8s.io;v=v1",
			includeObject: metav1.IncludeObjectPolicy("unrecognized"),
			object: func(t *testing.T) (metav1.Object, string, string) {
				return &metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, "", "services"
			},
			wantErr: func(t *testing.T, err error) {
				if !apierrors.IsBadRequest(err) || !strings.Contains(err.Error(), `Invalid value: "unrecognized": must be 'Metadata', 'Object', 'None', or empty`) {
					t.Fatal(err)
				}
			},
		},
	}

	for i := range testcases {
		tc := testcases[i]
		t.Run(tc.name, func(t *testing.T) {
			obj, group, resource := tc.object(t)

			cfg := dynamic.ConfigFor(config)
			if len(group) == 0 {
				cfg = dynamic.ConfigFor(kubeConfig)
				cfg.APIPath = "/api"
			} else {
				cfg.APIPath = "/apis"
			}
			cfg.GroupVersion = &schema.GroupVersion{Group: group, Version: "v1"}

			client, err := restclient.RESTClientFor(cfg)
			if err != nil {
				t.Fatal(err)
			}

			var rv string
			if obj.GetResourceVersion() == "" || obj.GetResourceVersion() == "0" {
				// no object was created in the preamble to the test, so get recent data
				rv = "0"
			} else {
				// we created an object, and need to list+watch from some time before the creation to see it
				rv = previousRV
			}

			timeoutCtx, timeoutCancel := context.WithTimeout(ctx, wait.ForeverTestTimeout)
			t.Cleanup(func() {
				timeoutCancel()
			})
			w, err := client.Get().
				Resource(resource).NamespaceIfScoped(obj.GetNamespace(), len(obj.GetNamespace()) > 0).
				SetHeader("Accept", tc.accept).
				VersionedParams(&metav1.ListOptions{
					ResourceVersion: rv,
					Watch:           true,
					FieldSelector:   fields.OneTermEqualSelector("metadata.name", obj.GetName()).String(),
				}, metav1.ParameterCodec).
				Param("includeObject", string(tc.includeObject)).
				Stream(timeoutCtx)
			if (tc.wantErr != nil) != (err != nil) {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil {
				tc.wantErr(t, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			tc.wantBody(t, w)
		})
	}
}

func TestWatchTransformCaching(t *testing.T) {
	ctx, clientSet, _, tearDownFn := setup(t)
	defer tearDownFn()

	ns := framework.CreateNamespaceOrDie(clientSet, "watch-transform", t)
	defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

	list, err := clientSet.CoreV1().ConfigMaps(ns.Name).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list objects: %v", err)
	}

	timeout := 30 * time.Second
	listOptions := &metav1.ListOptions{
		ResourceVersion: list.ResourceVersion,
		Watch:           true,
	}

	wMeta, err := clientSet.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces/watch-transform/configmaps").
		SetHeader("Accept", "application/vnd.kubernetes.protobuf;as=PartialObjectMetadata;g=meta.k8s.io;v=v1beta1").
		VersionedParams(listOptions, metav1.ParameterCodec).
		Timeout(timeout).
		Stream(ctx)
	if err != nil {
		t.Fatalf("Failed to start meta watch: %v", err)
	}
	defer wMeta.Close()

	wTableIncludeMeta, err := clientSet.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces/watch-transform/configmaps").
		SetHeader("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1beta1").
		VersionedParams(listOptions, metav1.ParameterCodec).
		Param("includeObject", string(metav1.IncludeMetadata)).
		Timeout(timeout).
		Stream(ctx)
	if err != nil {
		t.Fatalf("Failed to start table meta watch: %v", err)
	}
	defer wTableIncludeMeta.Close()

	wTableIncludeObject, err := clientSet.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces/watch-transform/configmaps").
		SetHeader("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1beta1").
		VersionedParams(listOptions, metav1.ParameterCodec).
		Param("includeObject", string(metav1.IncludeObject)).
		Timeout(timeout).
		Stream(ctx)
	if err != nil {
		t.Fatalf("Failed to start table object watch: %v", err)
	}
	defer wTableIncludeObject.Close()

	wTableIncludeObjectFiltered, err := clientSet.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces/watch-transform/configmaps").
		SetHeader("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1beta1").
		VersionedParams(listOptions, metav1.ParameterCodec).
		Param("includeObject", string(metav1.IncludeObject)).
		Param("labelSelector", "foo=bar").
		Timeout(timeout).
		Stream(ctx)
	if err != nil {
		t.Fatalf("Failed to start table object watch: %v", err)
	}
	defer wTableIncludeObjectFiltered.Close()

	configMap, err := clientSet.CoreV1().ConfigMaps("watch-transform").Create(ctx, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test1"},
		Data: map[string]string{
			"foo": "bar",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create a configMap: %v", err)
	}

	listOptionsDelayed := &metav1.ListOptions{
		ResourceVersion: configMap.ResourceVersion,
		Watch:           true,
	}
	wTableIncludeObjectDelayed, err := clientSet.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces/watch-transform/configmaps").
		SetHeader("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1beta1").
		VersionedParams(listOptionsDelayed, metav1.ParameterCodec).
		Param("includeObject", string(metav1.IncludeObject)).
		Timeout(timeout).
		Stream(ctx)
	if err != nil {
		t.Fatalf("Failed to start table object watch: %v", err)
	}
	defer wTableIncludeObjectDelayed.Close()

	configMap2, err := clientSet.CoreV1().ConfigMaps("watch-transform").Create(ctx, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test2"},
		Data: map[string]string{
			"foo": "bar",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create a second configMap: %v", err)
	}

	// Now update both configmaps so that filtering watch can observe them.
	// This is needed to validate whether events caching done by apiserver
	// distinguished objects by type.

	configMapUpdated, err := clientSet.CoreV1().ConfigMaps("watch-transform").Update(ctx, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test1", Labels: map[string]string{"foo": "bar"}},
		Data: map[string]string{
			"foo": "baz",
		},
	}, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update a configMap: %v", err)
	}

	configMap2Updated, err := clientSet.CoreV1().ConfigMaps("watch-transform").Update(ctx, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test2", Labels: map[string]string{"foo": "bar"}},
		Data: map[string]string{
			"foo": "baz",
		},
	}, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update a second configMap: %v", err)
	}

	metaChecks := []partialObjectMetadataCheck{
		func(res *metav1beta1.PartialObjectMetadata) {
			if !apiequality.Semantic.DeepEqual(configMap.ObjectMeta, res.ObjectMeta) {
				t.Errorf("expected object: %#v, got: %#v", configMap.ObjectMeta, res.ObjectMeta)
			}
		},
		func(res *metav1beta1.PartialObjectMetadata) {
			if !apiequality.Semantic.DeepEqual(configMap2.ObjectMeta, res.ObjectMeta) {
				t.Errorf("expected object: %#v, got: %#v", configMap2.ObjectMeta, res.ObjectMeta)
			}
		},
		func(res *metav1beta1.PartialObjectMetadata) {
			if !apiequality.Semantic.DeepEqual(configMapUpdated.ObjectMeta, res.ObjectMeta) {
				t.Errorf("expected object: %#v, got: %#v", configMapUpdated.ObjectMeta, res.ObjectMeta)
			}
		},
		func(res *metav1beta1.PartialObjectMetadata) {
			if !apiequality.Semantic.DeepEqual(configMap2Updated.ObjectMeta, res.ObjectMeta) {
				t.Errorf("expected object: %#v, got: %#v", configMap2Updated.ObjectMeta, res.ObjectMeta)
			}
		},
	}
	expectPartialObjectMetaEventsProtobufChecks(t, wMeta, metaChecks)

	tableMetaCheck := func(expected *v1.ConfigMap, got []byte) {
		var obj metav1.PartialObjectMetadata
		if err := json.Unmarshal(got, &obj); err != nil {
			t.Fatal(err)
		}
		if !apiequality.Semantic.DeepEqual(expected.ObjectMeta, obj.ObjectMeta) {
			t.Errorf("expected object: %#v, got: %#v", expected, obj)
		}
	}

	objectMetas := expectTableWatchEvents(t, 4, 3, metav1.IncludeMetadata, json.NewDecoder(wTableIncludeMeta))
	tableMetaCheck(configMap, objectMetas[0])
	tableMetaCheck(configMap2, objectMetas[1])
	tableMetaCheck(configMapUpdated, objectMetas[2])
	tableMetaCheck(configMap2Updated, objectMetas[3])

	tableObjectCheck := func(expectedType watch.EventType, expectedObj *v1.ConfigMap, got streamedEvent) {
		var obj *v1.ConfigMap
		if err := json.Unmarshal(got.rawObject, &obj); err != nil {
			t.Fatal(err)
		}
		if expectedType != watch.EventType(got.eventType) {
			t.Errorf("expected type: %#v, got: %#v", expectedType, got.eventType)
		}
		obj.TypeMeta = metav1.TypeMeta{}
		if !apiequality.Semantic.DeepEqual(expectedObj, obj) {
			t.Errorf("expected object: %#v, got: %#v", expectedObj, obj)
		}
	}

	objects := expectTableWatchEventsWithTypes(t, 4, 3, metav1.IncludeObject, json.NewDecoder(wTableIncludeObject))
	tableObjectCheck(watch.Added, configMap, objects[0])
	tableObjectCheck(watch.Added, configMap2, objects[1])
	tableObjectCheck(watch.Modified, configMapUpdated, objects[2])
	tableObjectCheck(watch.Modified, configMap2Updated, objects[3])

	filteredObjects := expectTableWatchEventsWithTypes(t, 2, 3, metav1.IncludeObject, json.NewDecoder(wTableIncludeObjectFiltered))
	tableObjectCheck(watch.Added, configMapUpdated, filteredObjects[0])
	tableObjectCheck(watch.Added, configMap2Updated, filteredObjects[1])

	delayedObjects := expectTableWatchEventsWithTypes(t, 3, 3, metav1.IncludeObject, json.NewDecoder(wTableIncludeObjectDelayed))
	tableObjectCheck(watch.Added, configMap2, delayedObjects[0])
	tableObjectCheck(watch.Modified, configMapUpdated, delayedObjects[1])
	tableObjectCheck(watch.Modified, configMap2Updated, delayedObjects[2])
}

func expectTableWatchEvents(t *testing.T, count, columns int, policy metav1.IncludeObjectPolicy, d *json.Decoder) [][]byte {
	events := expectTableWatchEventsWithTypes(t, count, columns, policy, d)
	var objects [][]byte
	for _, event := range events {
		objects = append(objects, event.rawObject)
	}
	return objects
}

type streamedEvent struct {
	eventType string
	rawObject []byte
}

func expectTableWatchEventsWithTypes(t *testing.T, count, columns int, policy metav1.IncludeObjectPolicy, d *json.Decoder) []streamedEvent {
	t.Helper()

	var events []streamedEvent

	for i := 0; i < count; i++ {
		var evt metav1.WatchEvent
		if err := d.Decode(&evt); err != nil {
			t.Fatal(err)
		}

		var table metav1beta1.Table
		if err := json.Unmarshal(evt.Object.Raw, &table); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if len(table.ColumnDefinitions) != columns {
				t.Fatalf("Got unexpected columns on first watch event: %d vs %#v", columns, table.ColumnDefinitions)
			}
		} else {
			if len(table.ColumnDefinitions) != 0 {
				t.Fatalf("Expected no columns on second watch event: %#v", table.ColumnDefinitions)
			}
		}
		if len(table.Rows) != 1 {
			t.Fatalf("Invalid rows: %#v", table.Rows)
		}
		row := table.Rows[0]
		if len(row.Cells) != columns {
			t.Fatalf("Invalid row width: %#v", row.Cells)
		}
		switch policy {
		case metav1.IncludeMetadata:
			var meta metav1beta1.PartialObjectMetadata
			if err := json.Unmarshal(row.Object.Raw, &meta); err != nil {
				t.Fatalf("expected partial object: %v", err)
			}
			partialObj := metav1.TypeMeta{Kind: "PartialObjectMetadata", APIVersion: "meta.k8s.io/v1beta1"}
			if meta.TypeMeta != partialObj {
				t.Fatalf("expected partial object: %#v", meta)
			}
			events = append(events, streamedEvent{eventType: evt.Type, rawObject: row.Object.Raw})
		case metav1.IncludeNone:
			if len(row.Object.Raw) != 0 {
				t.Fatalf("Expected no object: %s", string(row.Object.Raw))
			}
		case metav1.IncludeObject:
			if len(row.Object.Raw) == 0 {
				t.Fatalf("Expected object: %s", string(row.Object.Raw))
			}
			events = append(events, streamedEvent{eventType: evt.Type, rawObject: row.Object.Raw})
		}
	}
	return events
}

func expectPartialObjectMetaEvents(t *testing.T, d *json.Decoder, values ...string) {
	t.Helper()

	for i, value := range values {
		var evt metav1.WatchEvent
		if err := d.Decode(&evt); err != nil {
			t.Fatal(err)
		}
		var meta metav1beta1.PartialObjectMetadata
		if err := json.Unmarshal(evt.Object.Raw, &meta); err != nil {
			t.Fatal(err)
		}
		typeMeta := metav1.TypeMeta{Kind: "PartialObjectMetadata", APIVersion: "meta.k8s.io/v1beta1"}
		if meta.TypeMeta != typeMeta {
			t.Fatalf("expected partial object: %#v", meta)
		}
		if meta.Annotations["test"] != value {
			t.Fatalf("expected event %d to have value %q instead of %q", i+1, value, meta.Annotations["test"])
		}
	}
}

type partialObjectMetadataCheck func(*metav1beta1.PartialObjectMetadata)

func expectPartialObjectMetaEventsProtobuf(t *testing.T, r io.Reader, values ...string) {
	checks := []partialObjectMetadataCheck{}
	for i, value := range values {
		i, value := i, value
		checks = append(checks, func(meta *metav1beta1.PartialObjectMetadata) {
			if meta.Annotations["test"] != value {
				t.Fatalf("expected event %d to have value %q instead of %q", i+1, value, meta.Annotations["test"])
			}
		})
	}
	expectPartialObjectMetaEventsProtobufChecks(t, r, checks)
}

func expectPartialObjectMetaEventsProtobufChecks(t *testing.T, r io.Reader, checks []partialObjectMetadataCheck) {
	scheme := runtime.NewScheme()
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	rs := protobuf.NewRawSerializer(scheme, scheme)
	d := streaming.NewDecoder(
		protobuf.LengthDelimitedFramer.NewFrameReader(io.NopCloser(r)),
		rs,
	)
	ds := metainternalversionscheme.Codecs.UniversalDeserializer()

	for _, check := range checks {
		var evt metav1.WatchEvent
		if _, _, err := d.Decode(nil, &evt); err != nil {
			t.Fatal(err)
		}
		obj, gvk, err := ds.Decode(evt.Object.Raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		meta, ok := obj.(*metav1beta1.PartialObjectMetadata)
		if !ok {
			t.Fatalf("unexpected watch object %T", obj)
		}
		expected := &schema.GroupVersionKind{Kind: "PartialObjectMetadata", Version: "v1beta1", Group: "meta.k8s.io"}
		if !reflect.DeepEqual(expected, gvk) {
			t.Fatalf("expected partial object: %#v", meta)
		}
		check(meta)
	}
}

func expectTableV1WatchEvents(t *testing.T, count, columns int, policy metav1.IncludeObjectPolicy, d *json.Decoder) [][]byte {
	t.Helper()

	var objects [][]byte

	for i := 0; i < count; i++ {
		var evt metav1.WatchEvent
		if err := d.Decode(&evt); err != nil {
			t.Fatal(err)
		}
		var table metav1.Table
		if err := json.Unmarshal(evt.Object.Raw, &table); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if len(table.ColumnDefinitions) != columns {
				t.Fatalf("Got unexpected columns on first watch event: %d vs %#v", columns, table.ColumnDefinitions)
			}
		} else {
			if len(table.ColumnDefinitions) != 0 {
				t.Fatalf("Expected no columns on second watch event: %#v", table.ColumnDefinitions)
			}
		}
		if len(table.Rows) != 1 {
			t.Fatalf("Invalid rows: %#v", table.Rows)
		}
		row := table.Rows[0]
		if len(row.Cells) != columns {
			t.Fatalf("Invalid row width: %#v", row.Cells)
		}
		switch policy {
		case metav1.IncludeMetadata:
			var meta metav1.PartialObjectMetadata
			if err := json.Unmarshal(row.Object.Raw, &meta); err != nil {
				t.Fatalf("expected partial object: %v", err)
			}
			partialObj := metav1.TypeMeta{Kind: "PartialObjectMetadata", APIVersion: "meta.k8s.io/v1"}
			if meta.TypeMeta != partialObj {
				t.Fatalf("expected partial object: %#v", meta)
			}
		case metav1.IncludeNone:
			if len(row.Object.Raw) != 0 {
				t.Fatalf("Expected no object: %s", string(row.Object.Raw))
			}
		case metav1.IncludeObject:
			if len(row.Object.Raw) == 0 {
				t.Fatalf("Expected object: %s", string(row.Object.Raw))
			}
			objects = append(objects, row.Object.Raw)
		}
	}
	return objects
}

func expectPartialObjectMetaV1Events(t *testing.T, d *json.Decoder, values ...string) {
	t.Helper()

	for i, value := range values {
		var evt metav1.WatchEvent
		if err := d.Decode(&evt); err != nil {
			t.Fatal(err)
		}
		var meta metav1.PartialObjectMetadata
		if err := json.Unmarshal(evt.Object.Raw, &meta); err != nil {
			t.Fatal(err)
		}
		typeMeta := metav1.TypeMeta{Kind: "PartialObjectMetadata", APIVersion: "meta.k8s.io/v1"}
		if meta.TypeMeta != typeMeta {
			t.Fatalf("expected partial object: %#v", meta)
		}
		if meta.Annotations["test"] != value {
			t.Fatalf("expected event %d to have value %q instead of %q", i+1, value, meta.Annotations["test"])
		}
	}
}

func expectPartialObjectMetaV1EventsProtobuf(t *testing.T, r io.Reader, values ...string) {
	scheme := runtime.NewScheme()
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	rs := protobuf.NewRawSerializer(scheme, scheme)
	d := streaming.NewDecoder(
		protobuf.LengthDelimitedFramer.NewFrameReader(io.NopCloser(r)),
		rs,
	)
	ds := metainternalversionscheme.Codecs.UniversalDeserializer()

	for i, value := range values {
		var evt metav1.WatchEvent
		if _, _, err := d.Decode(nil, &evt); err != nil {
			t.Fatal(err)
		}
		obj, gvk, err := ds.Decode(evt.Object.Raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		meta, ok := obj.(*metav1.PartialObjectMetadata)
		if !ok {
			t.Fatalf("unexpected watch object %T", obj)
		}
		expected := &schema.GroupVersionKind{Kind: "PartialObjectMetadata", Version: "v1", Group: "meta.k8s.io"}
		if !reflect.DeepEqual(expected, gvk) {
			t.Fatalf("expected partial object: %#v", meta)
		}
		if meta.Annotations["test"] != value {
			t.Fatalf("expected event %d to have value %q instead of %q", i+1, value, meta.Annotations["test"])
		}
	}
}

func TestClientsetShareTransport(t *testing.T) {
	var counter int
	var mu sync.Mutex
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()

	dialFn := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		counter++
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	server.ClientConfig.Dial = dialFn
	client := clientset.NewForConfigOrDie(server.ClientConfig)
	// create test namespace
	_, err := client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-creation",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create test ns: %v", err)
	}
	// List different objects
	result := client.CoreV1().RESTClient().Get().AbsPath("/healthz").Do(context.TODO())
	_, err = result.Raw()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Discovery().ServerGroupsAndResources()
	if err != nil {
		t.Fatal(err)
	}
	n, err := client.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Listed %d namespaces on the cluster", len(n.Items))
	p, err := client.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Listed %d pods on the cluster", len(p.Items))
	e, err := client.DiscoveryV1().EndpointSlices("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Listed %d endpoint slices on the cluster", len(e.Items))
	d, err := client.AppsV1().Deployments("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Listed %d deployments on the cluster", len(d.Items))

	if counter != 1 {
		t.Fatalf("expected only one connection, created %d connections", counter)
	}
}

func TestDedupOwnerReferences(t *testing.T) {
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()
	etcd.CreateTestCRDs(t, apiextensionsclient.NewForConfigOrDie(server.ClientConfig), false, etcd.GetCustomResourceDefinitionData()[0])

	b := &bytes.Buffer{}
	warningWriter := restclient.NewWarningWriter(b, restclient.WarningWriterOptions{})
	server.ClientConfig.WarningHandler = warningWriter
	client := clientset.NewForConfigOrDie(server.ClientConfig)
	dynamicClient := dynamic.NewForConfigOrDie(server.ClientConfig)

	ns := "test-dedup-owner-references"
	// create test namespace
	_, err := client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create test ns: %v", err)
	}

	// some fake owner references
	fakeRefA := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "fake-configmap",
		UID:        uuid.NewUUID(),
	}
	fakeRefB := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       "fake-node",
		UID:        uuid.NewUUID(),
	}
	fakeRefC := metav1.OwnerReference{
		APIVersion: "cr.bar.com/v1",
		Kind:       "Foo",
		Name:       "fake-foo",
		UID:        uuid.NewUUID(),
	}

	tcs := []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{
		{
			gvr: schema.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "configmaps",
			},
			kind: "ConfigMap",
		},
		{
			gvr: schema.GroupVersionResource{
				Group:    "cr.bar.com",
				Version:  "v1",
				Resource: "foos",
			},
			kind: "Foo",
		},
	}

	for i, tc := range tcs {
		t.Run(tc.gvr.String(), func(t *testing.T) {
			previousWarningCount := i * 3
			c := &dependentClient{
				t:      t,
				client: dynamicClient.Resource(tc.gvr).Namespace(ns),
				gvr:    tc.gvr,
				kind:   tc.kind,
			}
			klog.Infof("creating dependent with duplicate owner references")
			dependent := c.createDependentWithOwners([]metav1.OwnerReference{fakeRefA, fakeRefA})
			assertManagedFields(t, dependent)
			expectedWarning := fmt.Sprintf(handlers.DuplicateOwnerReferencesWarningFormat, fakeRefA.UID)
			assertOwnerReferences(t, dependent, []metav1.OwnerReference{fakeRefA})
			assertWarningCount(t, warningWriter, previousWarningCount+1)
			assertWarningMessage(t, b, expectedWarning)

			klog.Infof("updating dependent with duplicate owner references")
			dependent = c.updateDependentWithOwners(dependent, []metav1.OwnerReference{fakeRefA, fakeRefA})
			assertManagedFields(t, dependent)
			assertOwnerReferences(t, dependent, []metav1.OwnerReference{fakeRefA})
			assertWarningCount(t, warningWriter, previousWarningCount+2)
			assertWarningMessage(t, b, expectedWarning)

			klog.Infof("patching dependent with duplicate owner reference")
			dependent = c.patchDependentWithOwner(dependent, fakeRefA)
			// TODO: currently a patch request that duplicates owner references can still
			// wipe out managed fields. Note that this happens to built-in resources but
			// not custom resources. In future we should either dedup before writing manage
			// fields, or stop deduping and reject the request.
			// assertManagedFields(t, dependent)
			expectedPatchWarning := fmt.Sprintf(handlers.DuplicateOwnerReferencesAfterMutatingAdmissionWarningFormat, fakeRefA.UID)
			assertOwnerReferences(t, dependent, []metav1.OwnerReference{fakeRefA})
			assertWarningCount(t, warningWriter, previousWarningCount+3)
			assertWarningMessage(t, b, expectedPatchWarning)

			klog.Infof("updating dependent with different owner references")
			dependent = c.updateDependentWithOwners(dependent, []metav1.OwnerReference{fakeRefA, fakeRefB})
			assertOwnerReferences(t, dependent, []metav1.OwnerReference{fakeRefA, fakeRefB})
			assertWarningCount(t, warningWriter, previousWarningCount+3)
			assertWarningMessage(t, b, "")

			klog.Infof("patching dependent with different owner references")
			dependent = c.patchDependentWithOwner(dependent, fakeRefC)
			assertOwnerReferences(t, dependent, []metav1.OwnerReference{fakeRefA, fakeRefB, fakeRefC})
			assertWarningCount(t, warningWriter, previousWarningCount+3)
			assertWarningMessage(t, b, "")

			klog.Infof("deleting dependent")
			c.deleteDependent()
			assertWarningCount(t, warningWriter, previousWarningCount+3)
			assertWarningMessage(t, b, "")
		})
	}
	// cleanup
	if err := client.CoreV1().Namespaces().Delete(context.TODO(), ns, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("failed to delete test ns: %v", err)
	}
}

func TestEmulatedStorageVersion(t *testing.T) {
	validVap := &admissionregistrationv1.ValidatingAdmissionPolicy{
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
					{
						ResourceNames: []string{"foo"},
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{"CREATE"},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{"*"},
								APIVersions: []string{"*"},
								Resources:   []string{"*"},
							},
						},
					},
				},
			},
			Validations: []admissionregistrationv1.Validation{
				{
					Expression: "true",
					Message:    "always valid",
				},
			},
		},
	}

	type testCase struct {
		name                   string
		emulatedVersion        string
		gvr                    schema.GroupVersionResource
		object                 runtime.Object
		expectedStorageVersion schema.GroupVersion
	}
	cases := []testCase{
		{
			name:            "vap after ga release",
			emulatedVersion: "1.31",
			gvr: schema.GroupVersionResource{
				Group:    "admissionregistration.k8s.io",
				Version:  "v1beta1",
				Resource: "validatingadmissionpolicies",
			},
			object: validVap,
			expectedStorageVersion: schema.GroupVersion{
				Group:   "admissionregistration.k8s.io",
				Version: "v1",
			},
		},
	}

	// Group cases by their emulated version
	groupedCases := map[string][]testCase{}
	for _, c := range cases {
		groupedCases[c.emulatedVersion] = append(groupedCases[c.emulatedVersion], c)
	}

	for emulatedVersion, cases := range groupedCases {
		t.Run(emulatedVersion, func(t *testing.T) {
			server := kubeapiservertesting.StartTestServerOrDie(
				t, &kubeapiservertesting.TestServerInstanceOptions{BinaryVersion: emulatedVersion},
				[]string{"--emulated-version=kube=" + emulatedVersion, `--storage-media-type=application/json`}, framework.SharedEtcd())
			defer server.TearDownFn()

			client := clientset.NewForConfigOrDie(server.ClientConfig)
			dynamicClient := dynamic.NewForConfigOrDie(server.ClientConfig)

			// create test namespace
			testNamespace := "test-emulated-storage-version"
			_, err := client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: testNamespace,
				},
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create test ns: %v", err)
			}

			restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(client.Discovery()))

			for i, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					gvk, err := restMapper.KindFor(c.gvr)
					if err != nil {
						t.Fatalf("failed to get GVK: %v", err)
					}
					c.object.GetObjectKind().SetGroupVersionKind(gvk)

					mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
					if err != nil {
						t.Fatalf("failed to get RESTMapping: %v", err)
					} else if mapping.Resource != c.gvr {
						t.Fatalf("expected resource %v, got %v", c.gvr, mapping.Resource)
					}

					asUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(c.object)
					if err != nil {
						t.Fatalf("failed to convert object to unstructured: %v", err)
					}

					uns := &unstructured.Unstructured{
						Object: asUnstructured,
					}
					uns.SetName(fmt.Sprintf("test-object%d", i))

					ns := testNamespace
					if mapping.Scope.Name() == meta.RESTScopeNameRoot {
						ns = ""
					}

					// create object
					created, err := dynamicClient.Resource(c.gvr).Namespace(ns).Create(context.TODO(), uns, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("failed to create object: %v", err)
					}

					// Fetch object from ETCD
					// Use this poor man's way to get the etcd path. This wont
					// work for all resources, but should work for most we want
					// to test against
					resourcePrefix := mapping.Resource.Resource
					if special, ok := kubeapiserver.SpecialDefaultResourcePrefixes[c.gvr.GroupResource()]; ok {
						resourcePrefix = special
					}
					etcdPathComponents := []string{
						"/",
						server.EtcdStoragePrefix,
						resourcePrefix,
					}

					if len(ns) > 0 {
						etcdPathComponents = append(etcdPathComponents, ns)
					}
					etcdPathComponents = append(etcdPathComponents, created.GetName())
					etcdPath := path.Join(etcdPathComponents...)
					fetched, err := server.EtcdClient.Get(context.TODO(), etcdPath)
					if err != nil {
						t.Fatalf("failed to fetch object from etcd: %v", err)
					} else if fetched.More || fetched.Count != 1 || len(fetched.Kvs) != 1 {
						t.Fatalf("unexpected fetched response: %v", fetched)
					}

					storedObject := &metav1.PartialObjectMetadata{}
					err = json.Unmarshal(fetched.Kvs[0].Value, storedObject)

					if err != nil {
						t.Fatalf("failed to decode object: %v", err)
					} else if storedObject.GroupVersionKind().GroupVersion() != c.expectedStorageVersion {
						t.Fatalf("expected storage version %s, got %s", c.expectedStorageVersion.String(), storedObject.GroupVersionKind().GroupVersion().String())
					}
				})
			}
		})
	}
}

// TestAllowedEmulationVersions tests the TestServer can start without problem for all allowed emulation versions.
func TestAllowedEmulationVersions(t *testing.T) {
	tcs := []struct {
		name             string
		emulationVersion string
	}{
		{
			name:             "default",
			emulationVersion: utilversion.DefaultKubeEffectiveVersion().EmulationVersion().String(),
		},
	}

	for _, tc := range tcs {
		t.Run(tc.emulationVersion, func(t *testing.T) {
			server := kubeapiservertesting.StartTestServerOrDie(t, nil,
				[]string{fmt.Sprintf("--emulated-version=kube=%s", tc.emulationVersion)}, framework.SharedEtcd())
			defer server.TearDownFn()

			rt, err := restclient.TransportFor(server.ClientConfig)
			if err != nil {
				t.Fatal(err)
			}

			req, err := http.NewRequest("GET", server.ClientConfig.Host+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			expectedStatusCode := 200
			if resp.StatusCode != expectedStatusCode {
				t.Errorf("expect status code: %d, got : %d\n", expectedStatusCode, resp.StatusCode)
			}
			defer func() {
				_ = resp.Body.Close()
			}()
		})
	}
}

func TestEnableEmulationVersion(t *testing.T) {
	server := kubeapiservertesting.StartTestServerOrDie(t,
		&kubeapiservertesting.TestServerInstanceOptions{BinaryVersion: "1.32"},
		[]string{"--emulated-version=kube=1.31"}, framework.SharedEtcd())
	defer server.TearDownFn()

	rt, err := restclient.TransportFor(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	tcs := []struct {
		path               string
		expectedStatusCode int
	}{
		{
			path:               "/",
			expectedStatusCode: 200,
		},
		{
			path:               "/apis/apps/v1/deployments",
			expectedStatusCode: 200,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1/flowschemas",
			expectedStatusCode: 200,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1beta1/flowschemas", // introduced at 1.20, removed at 1.26
			expectedStatusCode: 404,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1beta2/flowschemas", // introduced at 1.23, removed at 1.29
			expectedStatusCode: 404,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas", // introduced at 1.26, removed at 1.32
			expectedStatusCode: 200,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.path, func(t *testing.T) {
			req, err := http.NewRequest("GET", server.ClientConfig.Host+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.expectedStatusCode {
				t.Errorf("expect status code: %d, got : %d\n", tc.expectedStatusCode, resp.StatusCode)
			}
			defer func() {
				_ = resp.Body.Close()
			}()
		})
	}
}

func TestDisableEmulationVersion(t *testing.T) {
	server := kubeapiservertesting.StartTestServerOrDie(t,
		&kubeapiservertesting.TestServerInstanceOptions{BinaryVersion: "1.32"},
		[]string{}, framework.SharedEtcd())
	defer server.TearDownFn()

	rt, err := restclient.TransportFor(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	tcs := []struct {
		path               string
		expectedStatusCode int
	}{
		{
			path:               "/",
			expectedStatusCode: 200,
		},
		{
			path:               "/apis/apps/v1/deployments",
			expectedStatusCode: 200,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1/flowschemas",
			expectedStatusCode: 200,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1beta1/flowschemas", // introduced at 1.20, removed at 1.26
			expectedStatusCode: 404,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1beta2/flowschemas", // introduced at 1.23, removed at 1.29
			expectedStatusCode: 404,
		},
		{
			path:               "/apis/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas", // introduced at 1.26, removed at 1.32
			expectedStatusCode: 404,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.path, func(t *testing.T) {
			req, err := http.NewRequest("GET", server.ClientConfig.Host+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.expectedStatusCode {
				t.Errorf("expect status code: %d, got : %d\n", tc.expectedStatusCode, resp.StatusCode)
			}
			defer func() {
				_ = resp.Body.Close()
			}()
		})
	}
}

type dependentClient struct {
	t      *testing.T
	client dynamic.ResourceInterface
	gvr    schema.GroupVersionResource
	kind   string
}

func (c *dependentClient) createDependentWithOwners(refs []metav1.OwnerReference) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetName("dependent")
	obj.SetOwnerReferences(refs)
	obj.SetKind(c.kind)
	obj.SetAPIVersion(fmt.Sprintf("%s/%s", c.gvr.Group, c.gvr.Version))
	obj, err := c.client.Create(context.TODO(), obj, metav1.CreateOptions{})
	if err != nil {
		c.t.Fatalf("failed to create dependent with owner references %v: %v", refs, err)
	}
	return obj
}

func (c *dependentClient) updateDependentWithOwners(obj *unstructured.Unstructured, refs []metav1.OwnerReference) *unstructured.Unstructured {
	obj.SetOwnerReferences(refs)
	obj, err := c.client.Update(context.TODO(), obj, metav1.UpdateOptions{})
	if err != nil {
		c.t.Fatalf("failed to update dependent with owner references %v: %v", refs, err)
	}
	return obj
}

func (c *dependentClient) patchDependentWithOwner(obj *unstructured.Unstructured, ref metav1.OwnerReference) *unstructured.Unstructured {
	patch := []byte(fmt.Sprintf(`[{"op":"add","path":"/metadata/ownerReferences/-","value":{"apiVersion":"%v", "kind": "%v", "name": "%v", "uid": "%v"}}]`, ref.APIVersion, ref.Kind, ref.Name, ref.UID))
	obj, err := c.client.Patch(context.TODO(), obj.GetName(), types.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		c.t.Fatalf("failed to append owner reference to dependent with owner reference %v, patch %v: %v",
			ref, patch, err)
	}
	return obj
}

func (c *dependentClient) deleteDependent() {
	if err := c.client.Delete(context.TODO(), "dependent", metav1.DeleteOptions{}); err != nil {
		c.t.Fatalf("failed to delete dependent: %v", err)
	}
}

type warningCounter interface {
	WarningCount() int
}

func assertOwnerReferences(t *testing.T, obj *unstructured.Unstructured, refs []metav1.OwnerReference) {
	if !reflect.DeepEqual(obj.GetOwnerReferences(), refs) {
		t.Errorf("unexpected owner references, expected: %v, got: %v", refs, obj.GetOwnerReferences())
	}
}

func assertWarningCount(t *testing.T, counter warningCounter, expected int) {
	if counter.WarningCount() != expected {
		t.Errorf("unexpected warning count, expected: %v, got: %v", expected, counter.WarningCount())
	}
}

func assertWarningMessage(t *testing.T, b *bytes.Buffer, expected string) {
	defer b.Reset()
	actual := b.String()
	if len(expected) == 0 && len(actual) != 0 {
		t.Errorf("unexpected warning message, expected no warning, got: %v", actual)
	}
	if len(expected) == 0 {
		return
	}
	if !strings.Contains(actual, expected) {
		t.Errorf("unexpected warning message, expected: %v, got: %v", expected, actual)
	}
}

func assertManagedFields(t *testing.T, obj *unstructured.Unstructured) {
	if len(obj.GetManagedFields()) == 0 {
		t.Errorf("unexpected empty managed fields in object: %v", obj)
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

// TestDefaultStorageEncoding verifies that the storage encoding for all built-in resources is
// Protobuf.
func TestDefaultStorageEncoding(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, "AllAlpha", true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, "AllBeta", true)

	protobufRecognizer := protobuf.NewSerializer(runtime.NewScheme(), runtime.NewScheme())
	var recognizersByGroup map[string]recognizer.RecognizingDecoder
	{
		jsonRecognizer := jsonserializer.NewSerializerWithOptions(jsonserializer.DefaultMetaFactory, runtime.NewScheme(), runtime.NewScheme(), jsonserializer.SerializerOptions{})
		recognizersByGroup = map[string]recognizer.RecognizingDecoder{
			// No new exceptions should be added. Once these groups begin using Protobuf
			// for storage, the exception list should be removed.
			"apiextensions.k8s.io":   jsonRecognizer,
			"apiregistration.k8s.io": jsonRecognizer,
		}
	}

	storageConfig := framework.SharedEtcd()
	etcdPrefix := string(uuid.NewUUID())
	storageConfig.Prefix = path.Join(etcdPrefix, "registry")
	server := kubeapiservertesting.StartTestServerOrDie(
		t,
		kubeapiservertesting.NewDefaultTestServerOptions(),
		[]string{
			"--runtime-config=api/all=true",
			"--disable-admission-plugins=ServiceAccount",
		},
		storageConfig,
	)
	t.Cleanup(server.TearDownFn)

	client, err := clientset.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	dynamicClient, err := dynamic.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	etcdClient, kvClient, err := integration.GetEtcdClients(storageConfig.Transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := etcdClient.Close(); err != nil {
			t.Error(err)
		}
	})

	const NamespaceName = "test-protobuf-default-storage-encoding"
	if _, err := client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: NamespaceName}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	storageDataByResource := etcd.GetEtcdStorageDataForNamespace(NamespaceName)

	_, lists, err := client.Discovery().ServerGroupsAndResources()
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range lists {
		for _, resource := range list.APIResources {
			gv, err := schema.ParseGroupVersion(list.GroupVersion)
			if err != nil {
				t.Fatal(err)
			}
			if resource.Group != "" {
				gv.Group = resource.Group
			}
			if resource.Version != "" {
				gv.Version = resource.Version
			}

			if strings.Contains(resource.Name, "/") {
				continue
			}

			if !slices.Contains(resource.Verbs, "create") {
				continue
			}

			gvr := gv.WithResource(resource.Name)

			storageData := storageDataByResource[gvr]
			if storageData.Stub == "" {
				continue
			}

			name := fmt.Sprintf("%s.%s.%s", gvr.Resource, gvr.Version, gvr.Group)
			if gvr.Group == "" {
				name = fmt.Sprintf("%s.%s", gvr.Resource, gvr.Version)
			}
			t.Run(name, func(t *testing.T) {
				var o unstructured.Unstructured
				if err := utiljson.Unmarshal([]byte(storageData.Stub), &o.Object); err != nil {
					t.Fatal(err)
				}

				resourceClient := dynamicClient.Resource(gvr).Namespace(NamespaceName)
				if !resource.Namespaced {
					resourceClient = dynamicClient.Resource(gvr)
				}
				if _, err := resourceClient.Create(context.TODO(), &o, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}

				response, err := kvClient.Get(context.TODO(), path.Join("/", etcdPrefix, storageData.ExpectedEtcdPath))
				if err != nil {
					t.Fatal(err)
				}

				if n := len(response.Kvs); n != 1 {
					t.Fatalf("expected 1 kv, got %d", n)
				}
				recognizer, ok := recognizersByGroup[gvr.Group]
				if !ok {
					recognizer = protobufRecognizer
				}
				recognized, _, err := recognizer.RecognizesData(response.Kvs[0].Value)
				if err != nil {
					t.Fatal(err)
				}
				if !recognized {
					t.Fatal("stored object encoding not recognized as protobuf")
				}
			})
		}
	}
}
