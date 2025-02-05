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

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/waitgroup"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/audit/policy"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	basecompatibility "k8s.io/component-base/compatibility"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/tracing"
	"k8s.io/klog/v2/ktesting"
	netutils "k8s.io/utils/net"
)

func TestAuthorizeClientBearerTokenNoops(t *testing.T) {
	// All of these should do nothing (not panic, no side-effects)
	cfgGens := []func() *rest.Config{
		func() *rest.Config { return nil },
		func() *rest.Config { return &rest.Config{} },
		func() *rest.Config { return &rest.Config{BearerToken: "mu"} },
	}
	authcGens := []func() *AuthenticationInfo{
		func() *AuthenticationInfo { return nil },
		func() *AuthenticationInfo { return &AuthenticationInfo{} },
	}
	authzGens := []func() *AuthorizationInfo{
		func() *AuthorizationInfo { return nil },
		func() *AuthorizationInfo { return &AuthorizationInfo{} },
	}
	for _, cfgGen := range cfgGens {
		for _, authcGen := range authcGens {
			for _, authzGen := range authzGens {
				pConfig := cfgGen()
				pAuthc := authcGen()
				pAuthz := authzGen()
				AuthorizeClientBearerToken(pConfig, pAuthc, pAuthz)
				if before, after := authcGen(), pAuthc; !reflect.DeepEqual(before, after) {
					t.Errorf("AuthorizeClientBearerToken(%v, %#+v, %v) changed %#+v", pConfig, pAuthc, pAuthz, *before)
				}
				if before, after := authzGen(), pAuthz; !reflect.DeepEqual(before, after) {
					t.Errorf("AuthorizeClientBearerToken(%v, %v, %#+v) changed %#+v", pConfig, pAuthc, pAuthz, *before)
				}
			}
		}
	}
}

func TestAuthorizeClientBearerTokenRequiredGroups(t *testing.T) {
	fakeAuthenticator := authenticator.RequestFunc(func(req *http.Request) (*authenticator.Response, bool, error) {
		return &authenticator.Response{User: &user.DefaultInfo{}}, false, nil
	})
	fakeAuthorizer := authorizer.AuthorizerFunc(func(ctx context.Context, a authorizer.Attributes) (authorizer.Decision, string, error) {
		return authorizer.DecisionAllow, "", nil
	})
	target := &rest.Config{BearerToken: "secretToken"}
	authN := &AuthenticationInfo{Authenticator: fakeAuthenticator}
	authC := &AuthorizationInfo{Authorizer: fakeAuthorizer}

	AuthorizeClientBearerToken(target, authN, authC)

	fakeRequest, err := http.NewRequest("", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fakeRequest.Header.Set("Authorization", "bearer secretToken")
	rsp, _, err := authN.Authenticator.AuthenticateRequest(fakeRequest)
	if err != nil {
		t.Fatal(err)
	}
	expectedGroups := []string{user.AllAuthenticated, user.SystemPrivilegedGroup}
	if !reflect.DeepEqual(expectedGroups, rsp.User.GetGroups()) {
		t.Fatalf("unexpected groups = %v returned, expected = %v", rsp.User.GetGroups(), expectedGroups)
	}
}

func TestNewWithDelegate(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(errors.New("test is done"))
	delegateConfig := NewConfig(codecs)
	delegateConfig.ExternalAddress = "192.168.10.4:443"
	delegateConfig.PublicAddress = netutils.ParseIPSloppy("192.168.10.4")
	delegateConfig.LegacyAPIGroupPrefixes = sets.NewString("/api")
	delegateConfig.LoopbackClientConfig = &rest.Config{}
	delegateConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("", "", "")
	clientset := fake.NewSimpleClientset()
	if clientset == nil {
		t.Fatal("unable to create fake client set")
	}

	delegateConfig.HealthzChecks = append(delegateConfig.HealthzChecks, healthz.NamedCheck("delegate-health", func(r *http.Request) error {
		return fmt.Errorf("delegate failed healthcheck")
	}))

	sharedInformers := informers.NewSharedInformerFactory(clientset, delegateConfig.LoopbackClientConfig.Timeout)
	delegateServer, err := delegateConfig.Complete(sharedInformers).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatal(err)
	}
	delegateServer.Handler.NonGoRestfulMux.HandleFunc("/foo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	delegatePostStartHookChan := make(chan struct{})
	delegateServer.AddPostStartHookOrDie("delegate-post-start-hook", func(context PostStartHookContext) error {
		defer close(delegatePostStartHookChan)
		return nil
	})

	// this wires up swagger
	delegateServer.PrepareRun()

	wrappingConfig := NewConfig(codecs)
	wrappingConfig.ExternalAddress = "192.168.10.4:443"
	wrappingConfig.PublicAddress = netutils.ParseIPSloppy("192.168.10.4")
	wrappingConfig.LegacyAPIGroupPrefixes = sets.NewString("/api")
	wrappingConfig.LoopbackClientConfig = &rest.Config{}
	wrappingConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("", "", "")

	wrappingConfig.HealthzChecks = append(wrappingConfig.HealthzChecks, healthz.NamedCheck("wrapping-health", func(r *http.Request) error {
		return fmt.Errorf("wrapping failed healthcheck")
	}))

	sharedInformers = informers.NewSharedInformerFactory(clientset, wrappingConfig.LoopbackClientConfig.Timeout)
	wrappingServer, err := wrappingConfig.Complete(sharedInformers).New("test", delegateServer)
	if err != nil {
		t.Fatal(err)
	}
	wrappingServer.Handler.NonGoRestfulMux.HandleFunc("/bar", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	wrappingPostStartHookChan := make(chan struct{})
	wrappingServer.AddPostStartHookOrDie("wrapping-post-start-hook", func(context PostStartHookContext) error {
		defer close(wrappingPostStartHookChan)
		return nil
	})

	wrappingServer.PrepareRun()
	wrappingServer.RunPostStartHooks(ctx)

	server := httptest.NewServer(wrappingServer.Handler)
	defer server.Close()

	// Wait for the hooks to finish before checking the response
	<-delegatePostStartHookChan
	<-wrappingPostStartHookChan
	expectedPaths := []string{
		"/apis",
		"/bar",
		"/foo",
		"/healthz",
		"/healthz/delegate-health",
		"/healthz/log",
		"/healthz/ping",
		"/healthz/poststarthook/delegate-post-start-hook",
		"/healthz/poststarthook/generic-apiserver-start-informers",
		"/healthz/poststarthook/max-in-flight-filter",
		"/healthz/poststarthook/storage-object-count-tracker-hook",
		"/healthz/poststarthook/wrapping-post-start-hook",
		"/healthz/wrapping-health",
		"/livez",
		"/livez/delegate-health",
		"/livez/log",
		"/livez/ping",
		"/livez/poststarthook/delegate-post-start-hook",
		"/livez/poststarthook/generic-apiserver-start-informers",
		"/livez/poststarthook/max-in-flight-filter",
		"/livez/poststarthook/storage-object-count-tracker-hook",
		"/livez/poststarthook/wrapping-post-start-hook",
		"/metrics",
		"/metrics/slis",
		"/readyz",
		"/readyz/delegate-health",
		"/readyz/informer-sync",
		"/readyz/log",
		"/readyz/ping",
		"/readyz/poststarthook/delegate-post-start-hook",
		"/readyz/poststarthook/generic-apiserver-start-informers",
		"/readyz/poststarthook/max-in-flight-filter",
		"/readyz/poststarthook/storage-object-count-tracker-hook",
		"/readyz/poststarthook/wrapping-post-start-hook",
		"/readyz/shutdown",
	}
	checkExpectedPathsAtRoot(server.URL, expectedPaths, t)

	// wait for health (max-in-flight-filter is initialized asynchronously, can take a few milliseconds to initialize)
	if err := wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		// healthz checks are installed in PrepareRun
		resp, err := http.Get(server.URL + "/healthz?exclude=wrapping-health&exclude=delegate-health")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		if http.StatusOK != resp.StatusCode {
			t.Logf("got %d", resp.StatusCode)
			t.Log(string(data))
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	checkPath(server.URL+"/healthz", http.StatusInternalServerError, `[+]ping ok
[+]log ok
[-]wrapping-health failed: reason withheld
[-]delegate-health failed: reason withheld
[+]poststarthook/generic-apiserver-start-informers ok
[+]poststarthook/max-in-flight-filter ok
[+]poststarthook/storage-object-count-tracker-hook ok
[+]poststarthook/delegate-post-start-hook ok
[+]poststarthook/wrapping-post-start-hook ok
healthz check failed
`, t)

	checkPath(server.URL+"/healthz/delegate-health", http.StatusInternalServerError, `internal server error: delegate failed healthcheck
`, t)
	checkPath(server.URL+"/healthz/wrapping-health", http.StatusInternalServerError, `internal server error: wrapping failed healthcheck
`, t)
	checkPath(server.URL+"/healthz/poststarthook/delegate-post-start-hook", http.StatusOK, `ok`, t)
	checkPath(server.URL+"/healthz/poststarthook/wrapping-post-start-hook", http.StatusOK, `ok`, t)
	checkPath(server.URL+"/foo", http.StatusForbidden, ``, t)
	checkPath(server.URL+"/bar", http.StatusUnauthorized, ``, t)
}

func checkPath(url string, expectedStatusCode int, expectedBody string, t *testing.T) {
	t.Run(url, func(t *testing.T) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		dump, _ := httputil.DumpResponse(resp, true)
		t.Log(string(dump))

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		if e, a := expectedBody, string(body); e != a {
			t.Errorf("%q expected %v, got %v", url, e, a)
		}
		if e, a := expectedStatusCode, resp.StatusCode; e != a {
			t.Errorf("%q expected %v, got %v", url, e, a)
		}
	})
}

func checkExpectedPathsAtRoot(url string, expectedPaths []string, t *testing.T) {
	t.Run(url, func(t *testing.T) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		dump, _ := httputil.DumpResponse(resp, true)
		t.Log(string(dump))

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]interface{}
		json.Unmarshal(body, &result)
		paths, ok := result["paths"].([]interface{})
		if !ok {
			t.Errorf("paths not found")
		}
		pathset := sets.NewString()
		for _, p := range paths {
			pathset.Insert(p.(string))
		}
		expectedset := sets.NewString(expectedPaths...)
		for p := range pathset.Difference(expectedset) {
			t.Errorf("Got %v path, which we did not expect", p)
		}
		for p := range expectedset.Difference(pathset) {
			t.Errorf(" Expected %v path which we did not get", p)
		}
	})
}

func TestAuthenticationAuditAnnotationsDefaultChain(t *testing.T) {
	authn := authenticator.RequestFunc(func(req *http.Request) (*authenticator.Response, bool, error) {
		// confirm that we can set an audit annotation in a handler before WithAudit
		audit.AddAuditAnnotation(req.Context(), "pandas", "are awesome")

		return &authenticator.Response{User: &user.DefaultInfo{}}, true, nil
	})
	backend := &testBackend{}
	c := &Config{
		Authentication:           AuthenticationInfo{Authenticator: authn},
		AuditBackend:             backend,
		AuditPolicyRuleEvaluator: policy.NewFakePolicyRuleEvaluator(auditinternal.LevelMetadata, nil),

		// avoid nil panics
		NonLongRunningRequestWaitGroup: &waitgroup.SafeWaitGroup{},
		RequestInfoResolver:            &request.RequestInfoFactory{},
		RequestTimeout:                 10 * time.Second,
		LongRunningFunc:                func(_ *http.Request, _ *request.RequestInfo) bool { return false },
		lifecycleSignals:               newLifecycleSignals(),
		TracerProvider:                 tracing.NewNoopTracerProvider(),
		FeatureGate:                    utilfeature.DefaultFeatureGate,
	}

	h := DefaultBuildHandlerChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// confirm this is a no-op
		if r.Context() != audit.WithAuditContext(r.Context()) {
			t.Error("unexpected double wrapping of context")
		}

		// confirm that we have an audit event
		ae := audit.AuditEventFrom(r.Context())
		if ae == nil {
			t.Error("unexpected nil audit event")
		}

		// confirm that the indirect way of setting audit annotations later in the chain also works
		audit.AddAuditAnnotation(r.Context(), "dogs", "are okay")

		if _, err := w.Write([]byte("done")); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}), c)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, httptest.NewRequest("GET", "https://ignored.com", nil))

	r := w.Result()
	if ok := r.StatusCode == http.StatusOK && w.Body.String() == "done" && len(r.Header.Get(auditinternal.HeaderAuditID)) > 0; !ok {
		t.Errorf("invalid response: %#v", w)
	}
	if len(backend.events) == 0 {
		t.Error("expected audit events, got none")
	}
	// these should all be the same because the handler chain mutates the event in place
	want := map[string]string{"pandas": "are awesome", "dogs": "are okay"}
	for _, event := range backend.events {
		if event.Stage != auditinternal.StageResponseComplete {
			t.Errorf("expected event stage to be complete, got: %s", event.Stage)
		}

		for wantK, wantV := range want {
			gotV, ok := event.Annotations[wantK]
			if !ok {
				t.Errorf("expected to find annotation key %q in %#v", wantK, event.Annotations)
				continue
			}
			if wantV != gotV {
				t.Errorf("expected the annotation value to match, key: %q, want: %q got: %q", wantK, wantV, gotV)
			}
		}
	}
}

type testBackend struct {
	events []*auditinternal.Event

	audit.Backend // nil panic if anything other than ProcessEvents called
}

func (b *testBackend) ProcessEvents(events ...*auditinternal.Event) bool {
	b.events = append(b.events, events...)
	return true
}

func TestNewErrorForbiddenSerializer(t *testing.T) {
	config := CompletedConfig{
		&completedConfig{
			Config: &Config{
				Serializer: runtime.NewSimpleNegotiatedSerializer(runtime.SerializerInfo{
					MediaType: "application/cbor",
				}),
			},
		},
	}
	_, err := config.New("test", NewEmptyDelegate())
	if err == nil {
		t.Error("successfully created a new server configured with cbor support")
	} else if err.Error() != `refusing to create new apiserver "test" with support for media type "application/cbor" (allowed media types are: application/json, application/yaml, application/vnd.kubernetes.protobuf)` {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewFeatureGatedSerializer(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CBORServingAndStorage, true)

	config := NewConfig(serializer.NewCodecFactory(scheme, serializer.WithSerializer(func(creater runtime.ObjectCreater, typer runtime.ObjectTyper) runtime.SerializerInfo {
		return runtime.SerializerInfo{
			MediaType:        "application/cbor",
			MediaTypeType:    "application",
			MediaTypeSubType: "cbor",
		}
	})))
	config.ExternalAddress = "192.168.10.4:443"
	config.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("", "", "")
	config.LoopbackClientConfig = &rest.Config{}

	if _, err := config.Complete(nil).New("test", NewEmptyDelegate()); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
