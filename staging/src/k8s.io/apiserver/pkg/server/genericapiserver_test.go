/*
Copyright 2015 The Kubernetes Authors.

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

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/rest"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	"k8s.io/apiserver/pkg/warning"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/klog/v2/ktesting"
	kubeopenapi "k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"
	netutils "k8s.io/utils/net"
)

const (
	extensionsGroupName = "extensions"
)

var (
	v1GroupVersion = schema.GroupVersion{Group: "", Version: "v1"}

	scheme         = runtime.NewScheme()
	codecs         = serializer.NewCodecFactory(scheme)
	parameterCodec = runtime.NewParameterCodec(scheme)
)

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	scheme.AddUnversionedTypes(v1GroupVersion,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))
}

func buildTestOpenAPIDefinition() kubeopenapi.OpenAPIDefinition {
	return kubeopenapi.OpenAPIDefinition{
		Schema: spec.Schema{
			SchemaProps: spec.SchemaProps{
				Description: "Description",
				Properties:  map[string]spec.Schema{},
			},
			VendorExtensible: spec.VendorExtensible{
				Extensions: spec.Extensions{
					"x-kubernetes-group-version-kind": []interface{}{
						map[string]interface{}{
							"group":   "",
							"version": "v1",
							"kind":    "Getter",
						},
						map[string]interface{}{
							"group":   "batch",
							"version": "v1",
							"kind":    "Getter",
						},
						map[string]interface{}{
							"group":   "extensions",
							"version": "v1",
							"kind":    "Getter",
						},
					},
				},
			},
		},
	}
}

func testGetOpenAPIDefinitions(_ kubeopenapi.ReferenceCallback) map[string]kubeopenapi.OpenAPIDefinition {
	return map[string]kubeopenapi.OpenAPIDefinition{
		"k8s.io/apimachinery/pkg/apis/meta/v1.Status":          {},
		"k8s.io/apimachinery/pkg/apis/meta/v1.APIVersions":     {},
		"k8s.io/apimachinery/pkg/apis/meta/v1.APIGroupList":    {},
		"k8s.io/apimachinery/pkg/apis/meta/v1.APIGroup":        buildTestOpenAPIDefinition(),
		"k8s.io/apimachinery/pkg/apis/meta/v1.APIResourceList": {},
	}
}

// setUp is a convience function for setting up for (most) tests.
func setUp(t *testing.T) (Config, *assert.Assertions) {
	config := NewConfig(codecs)
	config.ExternalAddress = "192.168.10.4:443"
	config.PublicAddress = netutils.ParseIPSloppy("192.168.10.4")
	config.LegacyAPIGroupPrefixes = sets.NewString("/api")
	config.LoopbackClientConfig = &restclient.Config{}

	clientset := fake.NewSimpleClientset()
	if clientset == nil {
		t.Fatal("unable to create fake client set")
	}
	config.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("", "", "")
	config.OpenAPIConfig = DefaultOpenAPIConfig(testGetOpenAPIDefinitions, openapinamer.NewDefinitionNamer(runtime.NewScheme()))
	config.OpenAPIConfig.Info.Version = "unversioned"
	config.OpenAPIV3Config = DefaultOpenAPIV3Config(testGetOpenAPIDefinitions, openapinamer.NewDefinitionNamer(runtime.NewScheme()))
	config.OpenAPIV3Config.Info.Version = "unversioned"
	sharedInformers := informers.NewSharedInformerFactory(clientset, config.LoopbackClientConfig.Timeout)
	config.Complete(sharedInformers)

	return *config, assert.New(t)
}

func newMaster(t *testing.T) (*GenericAPIServer, Config, *assert.Assertions) {
	config, assert := setUp(t)

	s, err := config.Complete(nil).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatalf("Error in bringing up the server: %v", err)
	}
	return s, config, assert
}

// TestNew verifies that the New function returns a GenericAPIServer
// using the configuration properly.
func TestNew(t *testing.T) {
	s, config, assert := newMaster(t)

	// Verify many of the variables match their config counterparts
	assert.Equal(s.legacyAPIGroupPrefixes, config.LegacyAPIGroupPrefixes)
	assert.Equal(s.admissionControl, config.AdmissionControl)
}

// Verifies that AddGroupVersions works as expected.
func TestInstallAPIGroups(t *testing.T) {
	config, assert := setUp(t)

	config.LegacyAPIGroupPrefixes = sets.NewString("/apiPrefix")
	config.DiscoveryAddresses = discovery.DefaultAddresses{DefaultAddress: "ExternalAddress"}

	s, err := config.Complete(nil).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatalf("Error in bringing up the server: %v", err)
	}

	testAPI := func(gv schema.GroupVersion) APIGroupInfo {
		getter, noVerbs := testGetterStorage{}, testNoVerbsStorage{}

		scheme := runtime.NewScheme()
		scheme.AddKnownTypeWithName(gv.WithKind("Getter"), getter.New())
		scheme.AddKnownTypeWithName(gv.WithKind("NoVerb"), noVerbs.New())
		scheme.AddKnownTypes(v1GroupVersion, &metav1.Status{})
		metav1.AddToGroupVersion(scheme, v1GroupVersion)

		return APIGroupInfo{
			PrioritizedVersions: []schema.GroupVersion{gv},
			VersionedResourcesStorageMap: map[string]map[string]rest.Storage{
				gv.Version: {
					"getter":  &testGetterStorage{Version: gv.Version},
					"noverbs": &testNoVerbsStorage{Version: gv.Version},
				},
			},
			OptionsExternalVersion: &schema.GroupVersion{Version: "v1"},
			ParameterCodec:         parameterCodec,
			NegotiatedSerializer:   codecs,
			Scheme:                 scheme,
		}
	}

	apis := []APIGroupInfo{
		testAPI(schema.GroupVersion{Group: "", Version: "v1"}),
		testAPI(schema.GroupVersion{Group: extensionsGroupName, Version: "v1"}),
		testAPI(schema.GroupVersion{Group: "batch", Version: "v1"}),
	}

	err = s.InstallLegacyAPIGroup("/apiPrefix", &apis[0])
	assert.NoError(err)
	groupPaths := []string{
		config.LegacyAPIGroupPrefixes.List()[0], // /apiPrefix
	}
	for _, api := range apis[1:] {
		err = s.InstallAPIGroup(&api)
		assert.NoError(err)
		groupPaths = append(groupPaths, APIGroupPrefix+"/"+api.PrioritizedVersions[0].Group) // /apis/<group>
	}

	server := httptest.NewServer(s.Handler)
	defer server.Close()

	for i := range apis {
		// should serve APIGroup at group path
		info := &apis[i]
		path := groupPaths[i]
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Errorf("[%d] unexpected error getting path %q path: %v", i, path, err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("[%d] unexpected error reading body at path %q: %v", i, path, err)
			continue
		}

		t.Logf("[%d] json at %s: %s", i, path, string(body))

		if i == 0 {
			// legacy API returns APIVersions
			group := metav1.APIVersions{}
			err = json.Unmarshal(body, &group)
			if err != nil {
				t.Errorf("[%d] unexpected error parsing json body at path %q: %v", i, path, err)
				continue
			}
		} else {
			// API groups return APIGroup
			group := metav1.APIGroup{}
			err = json.Unmarshal(body, &group)
			if err != nil {
				t.Errorf("[%d] unexpected error parsing json body at path %q: %v", i, path, err)
				continue
			}

			if got, expected := group.Name, info.PrioritizedVersions[0].Group; got != expected {
				t.Errorf("[%d] unexpected group name at path %q: got=%q expected=%q", i, path, got, expected)
				continue
			}

			if got, expected := group.PreferredVersion.Version, info.PrioritizedVersions[0].Version; got != expected {
				t.Errorf("[%d] unexpected group version at path %q: got=%q expected=%q", i, path, got, expected)
				continue
			}
		}

		// should serve APIResourceList at group path + /<group-version>
		path = path + "/" + info.PrioritizedVersions[0].Version
		resp, err = http.Get(server.URL + path)
		if err != nil {
			t.Errorf("[%d] unexpected error getting path %q path: %v", i, path, err)
			continue
		}

		body, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("[%d] unexpected error reading body at path %q: %v", i, path, err)
			continue
		}

		t.Logf("[%d] json at %s: %s", i, path, string(body))

		resources := metav1.APIResourceList{}
		err = json.Unmarshal(body, &resources)
		if err != nil {
			t.Errorf("[%d] unexpected error parsing json body at path %q: %v", i, path, err)
			continue
		}

		if got, expected := resources.GroupVersion, info.PrioritizedVersions[0].String(); got != expected {
			t.Errorf("[%d] unexpected groupVersion at path %q: got=%q expected=%q", i, path, got, expected)
			continue
		}

		// the verbs should match the features of resources
		for _, r := range resources.APIResources {
			switch r.Name {
			case "getter":
				if got, expected := sets.NewString([]string(r.Verbs)...), sets.NewString("get"); !got.Equal(expected) {
					t.Errorf("[%d] unexpected verbs for resource %s/%s: got=%v expected=%v", i, resources.GroupVersion, r.Name, got, expected)
				}
			case "noverbs":
				if r.Verbs == nil {
					t.Errorf("[%d] unexpected nil verbs slice. Expected: []string{}", i)
				}
				if got, expected := sets.NewString([]string(r.Verbs)...), sets.NewString(); !got.Equal(expected) {
					t.Errorf("[%d] unexpected verbs for resource %s/%s: got=%v expected=%v", i, resources.GroupVersion, r.Name, got, expected)
				}
			}
		}
	}
}

func TestPrepareRun(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	s, config, assert := newMaster(t)

	assert.NotNil(config.OpenAPIConfig)

	server := httptest.NewServer(s.Handler.Director)
	defer server.Close()

	s.PrepareRun()
	s.RunPostStartHooks(ctx)

	// openapi is installed in PrepareRun
	resp, err := http.Get(server.URL + "/openapi/v2")
	assert.NoError(err)
	assert.Equal(http.StatusOK, resp.StatusCode)

	// wait for health (max-in-flight-filter is initialized asynchronously, can take a few milliseconds to initialize)
	assert.NoError(wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		// healthz checks are installed in PrepareRun
		resp, err = http.Get(server.URL + "/healthz")
		assert.NoError(err)
		data, _ := io.ReadAll(resp.Body)
		if http.StatusOK != resp.StatusCode {
			t.Logf("got %d", resp.StatusCode)
			t.Log(string(data))
			return false, nil
		}
		return true, nil
	}))
	resp, err = http.Get(server.URL + "/healthz/ping")
	assert.NoError(err)
	assert.Equal(http.StatusOK, resp.StatusCode)
}

func TestUpdateOpenAPISpec(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	s, _, assert := newMaster(t)
	s.PrepareRun()
	s.RunPostStartHooks(ctx)

	server := httptest.NewServer(s.Handler.Director)
	defer server.Close()

	// verify the static spec in record is what we currently serve
	oldSpec, err := json.Marshal(s.StaticOpenAPISpec)
	assert.NoError(err)

	resp, err := http.Get(server.URL + "/openapi/v2")
	assert.NoError(err)
	assert.Equal(http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	assert.NoError(err)
	assert.Equal(oldSpec, body)
	resp.Body.Close()

	// verify we are able to update the served spec using the exposed service
	newSpec := []byte(`{"swagger":"2.0","info":{"title":"Test Updated Generic API Server Swagger","version":"v0.1.0"},"paths":null}`)
	swagger := new(spec.Swagger)
	err = json.Unmarshal(newSpec, swagger)
	assert.NoError(err)

	err = s.OpenAPIVersionedService.UpdateSpec(swagger)
	assert.NoError(err)

	resp, err = http.Get(server.URL + "/openapi/v2")
	assert.NoError(err)
	defer resp.Body.Close()
	assert.Equal(http.StatusOK, resp.StatusCode)

	body, err = io.ReadAll(resp.Body)
	assert.NoError(err)
	assert.Equal(newSpec, body)
}

// TestCustomHandlerChain verifies the handler chain with custom handler chain builder functions.
func TestCustomHandlerChain(t *testing.T) {
	config, _ := setUp(t)

	var protected, called bool

	config.BuildHandlerChainFunc = func(apiHandler http.Handler, c *Config) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			protected = true
			apiHandler.ServeHTTP(w, req)
		})
	}
	handler := http.HandlerFunc(func(r http.ResponseWriter, req *http.Request) {
		called = true
	})

	s, err := config.Complete(nil).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatalf("Error in bringing up the server: %v", err)
	}

	s.Handler.NonGoRestfulMux.Handle("/nonswagger", handler)
	s.Handler.NonGoRestfulMux.Handle("/secret", handler)

	type Test struct {
		handler   http.Handler
		path      string
		protected bool
	}
	for i, test := range []Test{
		{s.Handler, "/nonswagger", true},
		{s.Handler, "/secret", true},
	} {
		protected, called = false, false

		var w io.Reader
		req, err := http.NewRequest("GET", test.path, w)
		if err != nil {
			t.Errorf("%d: Unexpected http error: %v", i, err)
			continue
		}

		test.handler.ServeHTTP(httptest.NewRecorder(), req)

		if !called {
			t.Errorf("%d: Expected handler to be called.", i)
		}
		if test.protected != protected {
			t.Errorf("%d: Expected protected=%v, got protected=%v.", i, test.protected, protected)
		}
	}
}

// TestNotRestRoutesHaveAuth checks that special non-routes are behind authz/authn.
func TestNotRestRoutesHaveAuth(t *testing.T) {
	config, _ := setUp(t)

	authz := mockAuthorizer{}

	config.LegacyAPIGroupPrefixes = sets.NewString("/apiPrefix")
	config.Authorization.Authorizer = &authz

	config.EnableIndex = true
	config.EnableProfiling = true

	kubeVersion := fakeVersion()
	binaryVersion := utilversion.MustParse(kubeVersion.String())
	effectiveVersion := basecompatibility.NewEffectiveVersion(binaryVersion, false, binaryVersion, binaryVersion.SubtractMinor(1))
	config.EffectiveVersion = effectiveVersion

	s, err := config.Complete(nil).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatalf("Error in bringing up the server: %v", err)
	}

	for _, test := range []struct {
		route string
	}{
		{"/"},
		{"/debug/pprof/"},
		{"/debug/flags/"},
		{"/version"},
	} {
		resp := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", test.route, nil)
		s.Handler.ServeHTTP(resp, req)
		if resp.Code != 200 {
			t.Errorf("route %q expected to work: code %d", test.route, resp.Code)
			continue
		}

		if authz.lastURI != test.route {
			t.Errorf("route %q expected to go through authorization, last route did: %q", test.route, authz.lastURI)
		}
	}
}

func TestMuxAndDiscoveryCompleteSignals(t *testing.T) {
	// setup
	cfg, assert := setUp(t)

	// scenario 1: single server with some signals
	root, err := cfg.Complete(nil).New("rootServer", NewEmptyDelegate())
	assert.NoError(err)
	if len(root.MuxAndDiscoveryCompleteSignals()) != 0 {
		assert.Error(fmt.Errorf("unexpected signals %v registered in the root server", root.MuxAndDiscoveryCompleteSignals()))
	}
	root.RegisterMuxAndDiscoveryCompleteSignal("rootTestSignal", make(chan struct{}))
	if len(root.MuxAndDiscoveryCompleteSignals()) != 1 {
		assert.Error(fmt.Errorf("unexpected signals %v registered in the root server", root.MuxAndDiscoveryCompleteSignals()))
	}

	// scenario 2: multiple servers with some signals
	delegate, err := cfg.Complete(nil).New("delegateServer", NewEmptyDelegate())
	assert.NoError(err)
	delegate.RegisterMuxAndDiscoveryCompleteSignal("delegateTestSignal", make(chan struct{}))
	if len(delegate.MuxAndDiscoveryCompleteSignals()) != 1 {
		assert.Error(fmt.Errorf("unexpected signals %v registered in the delegate server", delegate.MuxAndDiscoveryCompleteSignals()))
	}
	newRoot, err := cfg.Complete(nil).New("newRootServer", delegate)
	assert.NoError(err)
	if len(newRoot.MuxAndDiscoveryCompleteSignals()) != 1 {
		assert.Error(fmt.Errorf("unexpected signals %v registered in the newRoot server", newRoot.MuxAndDiscoveryCompleteSignals()))
	}
	newRoot.RegisterMuxAndDiscoveryCompleteSignal("newRootTestSignal", make(chan struct{}))
	if len(newRoot.MuxAndDiscoveryCompleteSignals()) != 2 {
		assert.Error(fmt.Errorf("unexpected signals %v registered in the newRoot server", newRoot.MuxAndDiscoveryCompleteSignals()))
	}
}

type mockAuthorizer struct {
	lastURI string
}

func (authz *mockAuthorizer) Authorize(ctx context.Context, a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
	authz.lastURI = a.GetPath()
	return authorizer.DecisionAllow, "", nil
}

type testGetterStorage struct {
	Version string
}

func (p *testGetterStorage) NamespaceScoped() bool {
	return true
}

func (p *testGetterStorage) New() runtime.Object {
	return &metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Getter",
			APIVersion: p.Version,
		},
	}
}

func (p *testGetterStorage) Destroy() {
}

func (p *testGetterStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return nil, nil
}

func (p *testGetterStorage) GetSingularName() string {
	return "getter"
}

type testNoVerbsStorage struct {
	Version string
}

func (p *testNoVerbsStorage) NamespaceScoped() bool {
	return true
}

func (p *testNoVerbsStorage) New() runtime.Object {
	return &metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			Kind:       "NoVerbs",
			APIVersion: p.Version,
		},
	}
}

func (p *testNoVerbsStorage) Destroy() {
}

func (p *testNoVerbsStorage) GetSingularName() string {
	return "noverb"
}

func fakeVersion() version.Info {
	return version.Info{
		Major:        "42",
		Minor:        "42",
		GitVersion:   "42.42",
		GitCommit:    "34973274ccef6ab4dfaaf86599792fa9c3fe4689",
		GitTreeState: "Dirty",
		BuildDate:    time.Now().String(),
		GoVersion:    goruntime.Version(),
		Compiler:     goruntime.Compiler,
		Platform:     fmt.Sprintf("%s/%s", goruntime.GOOS, goruntime.GOARCH),
	}
}

// TestGracefulShutdown verifies server shutdown after request handler finish.
func TestGracefulShutdown(t *testing.T) {
	config, _ := setUp(t)

	var graceShutdown bool
	wg := sync.WaitGroup{}
	wg.Add(1)

	config.BuildHandlerChainFunc = func(apiHandler http.Handler, c *Config) http.Handler {
		handler := genericfilters.WithWaitGroup(apiHandler, c.LongRunningFunc, c.NonLongRunningRequestWaitGroup)
		handler = genericapifilters.WithRequestInfo(handler, c.RequestInfoResolver)
		return handler
	}

	s, err := config.Complete(nil).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatalf("Error in bringing up the server: %v", err)
	}

	twoSecondHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wg.Done()
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		graceShutdown = true
	})
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	s.Handler.NonGoRestfulMux.Handle("/test", twoSecondHandler)
	s.Handler.NonGoRestfulMux.Handle("/200", okHandler)

	insecureServer := &http.Server{
		Addr:    "0.0.0.0:0",
		Handler: s.Handler,
	}
	stopCh := make(chan struct{})

	ln, err := net.Listen("tcp", insecureServer.Addr)
	if err != nil {
		t.Errorf("failed to listen on %v: %v", insecureServer.Addr, err)
	}

	// get port
	serverPort := ln.Addr().(*net.TCPAddr).Port
	stoppedCh, _, err := RunServer(insecureServer, ln, 10*time.Second, stopCh)
	if err != nil {
		t.Fatalf("RunServer err: %v", err)
	}

	graceCh := make(chan struct{})
	// mock a client request
	go func() {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(serverPort) + "/test")
		if err != nil {
			t.Errorf("Unexpected http error: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Unexpected http status code: %v", resp.StatusCode)
		}
		close(graceCh)
	}()

	// close stopCh after request sent to server to guarantee request handler is running.
	wg.Wait()
	close(stopCh)

	time.Sleep(500 * time.Millisecond)
	if _, err := http.Get("http://127.0.0.1:" + strconv.Itoa(serverPort) + "/200"); err == nil {
		t.Errorf("Unexpected http success after stopCh was closed")
	}

	// wait for wait group handler finish
	s.NonLongRunningRequestWaitGroup.Wait()
	<-stoppedCh

	// check server all handlers finished.
	if !graceShutdown {
		t.Errorf("server shutdown not gracefully.")
	}
	// check client to make sure receive response.
	select {
	case <-graceCh:
		t.Logf("server shutdown gracefully.")
	case <-time.After(30 * time.Second):
		t.Errorf("Timed out waiting for response.")
	}
}

func TestWarningWithRequestTimeout(t *testing.T) {
	type result struct {
		err        interface{}
		stackTrace string
	}
	clientDoneCh, resultCh := make(chan struct{}), make(chan result, 1)
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// this will catch recoverable panic like 'Header called after Handler finished'.
		// go runtime crashes the program if it detects a program-ending
		// panic like 'concurrent map iteration and map write', so this
		// panic can not be caught.
		defer func() {
			result := result{}
			result.err = recover()
			if result.err != nil {
				// Same as stdlib http server code. Manually allocate stack
				// trace buffer size to prevent excessively large logs
				const size = 64 << 10
				buf := make([]byte, size)
				buf = buf[:goruntime.Stack(buf, false)]
				result.stackTrace = string(buf)
			}
			resultCh <- result
		}()

		// add warnings while we're waiting for the request to timeout to catch read/write races
	loop:
		for {
			select {
			case <-r.Context().Done():
				break loop
			default:
				warning.AddWarning(r.Context(), "a", "1")
			}
		}
		// the request has just timed out, write to catch read/write races
		warning.AddWarning(r.Context(), "agent", "text")

		// give time for the timeout response to be written, then try to
		// write a response header to catch the "Header after Handler finished" panic
		<-clientDoneCh

		warning.AddWarning(r.Context(), "agent", "text")
	})
	handler := newGenericAPIServerHandlerChain(t, "/test", testHandler)

	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	request, err := http.NewRequest("GET", server.URL+"/test?timeout=100ms", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	client := server.Client()
	response, err := client.Do(request)
	close(clientDoneCh)
	if err != nil {
		t.Errorf("expected server to return an HTTP response: %v", err)
	}
	if want := http.StatusGatewayTimeout; response == nil || response.StatusCode != want {
		t.Errorf("expected server to return %d, but got: %v", want, response)
	}

	var resultGot result
	select {
	case resultGot = <-resultCh:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("the handler never returned a result")
	}
	if resultGot.err != nil {
		t.Errorf("Expected no panic, but got: %v", resultGot.err)
		t.Errorf("Stack Trace: %s", resultGot.stackTrace)
	}
}

// builds a handler chain with the given user handler as used by GenericAPIServer.
func newGenericAPIServerHandlerChain(t *testing.T, path string, handler http.Handler) http.Handler {
	config, _ := setUp(t)
	s, err := config.Complete(nil).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatalf("Error in bringing up the server: %v", err)
	}

	s.Handler.NonGoRestfulMux.Handle(path, handler)
	return s.Handler
}
