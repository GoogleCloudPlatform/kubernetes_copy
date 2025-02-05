/*
Copyright 2019 The Kubernetes Authors.

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

package handlers

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/admission"
	auditapis "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/rest"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
)

type mockCodecs struct {
	serializer.CodecFactory
	err error
}

type mockCodec struct {
	runtime.Codec
	codecs *mockCodecs
}

func (p mockCodec) Encode(obj runtime.Object, w io.Writer) error {
	err := p.Codec.Encode(obj, w)
	p.codecs.err = err
	return err
}

func (s *mockCodecs) EncoderForVersion(encoder runtime.Encoder, gv runtime.GroupVersioner) runtime.Encoder {
	out := s.CodecFactory.CodecForVersions(encoder, nil, gv, nil)
	return &mockCodec{
		Codec:  out,
		codecs: s,
	}
}

func TestDeleteResourceAuditLogRequestObject(t *testing.T) {

	ctx := audit.WithAuditContext(context.TODO())
	ac := audit.AuditContextFrom(ctx)
	ac.Event.Level = auditapis.LevelRequestResponse

	policy := metav1.DeletePropagationBackground
	deleteOption := &metav1.DeleteOptions{
		GracePeriodSeconds: pointer.Int64Ptr(30),
		PropagationPolicy:  &policy,
	}

	fakeCorev1GroupVersion := schema.GroupVersion{
		Group:   "",
		Version: "v1",
	}
	testScheme := runtime.NewScheme()
	metav1.AddToGroupVersion(testScheme, fakeCorev1GroupVersion)
	testCodec := serializer.NewCodecFactory(testScheme)

	tests := []struct {
		name       string
		object     runtime.Object
		gv         schema.GroupVersion
		serializer serializer.CodecFactory
		ok         bool
	}{
		{
			name: "meta built-in Codec encode v1.DeleteOptions",
			object: &metav1.DeleteOptions{
				GracePeriodSeconds: pointer.Int64Ptr(30),
				PropagationPolicy:  &policy,
			},
			gv:         metav1.SchemeGroupVersion,
			serializer: metainternalversionscheme.Codecs,
			ok:         true,
		},
		{
			name: "fake corev1 registered codec encode v1 DeleteOptions",
			object: &metav1.DeleteOptions{
				GracePeriodSeconds: pointer.Int64Ptr(30),
				PropagationPolicy:  &policy,
			},
			gv:         metav1.SchemeGroupVersion,
			serializer: testCodec,
			ok:         false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			codecs := &mockCodecs{}
			codecs.CodecFactory = test.serializer

			audit.LogRequestObject(ctx, deleteOption, test.gv, schema.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
			}, "", codecs)

			err := codecs.err
			if err != nil {
				if test.ok {
					t.Errorf("expect nil but got %#v", err)
				}
				t.Logf("encode object: %#v", err)
			} else {
				if !test.ok {
					t.Errorf("expect err but got nil")
				}
			}
		})
	}
}

func TestDeleteCollection(t *testing.T) {
	req := &http.Request{
		Header: http.Header{},
	}
	req.Header.Set("Content-Type", "application/json")

	fakeCorev1GroupVersion := schema.GroupVersion{
		Group:   "",
		Version: "v1",
	}
	fakeCorev1Scheme := runtime.NewScheme()
	fakeCorev1Scheme.AddKnownTypes(fakeCorev1GroupVersion, &metav1.DeleteOptions{})
	fakeCorev1Codec := serializer.NewCodecFactory(fakeCorev1Scheme)

	tests := []struct {
		name         string
		codecFactory serializer.CodecFactory
		data         []byte
		expectErr    string
	}{
		//  for issue: https://github.com/kubernetes/kubernetes/issues/111985
		{
			name:         "decode '{}' to metav1.DeleteOptions with fakeCorev1Codecs",
			codecFactory: fakeCorev1Codec,
			data:         []byte("{}"),
			expectErr:    "no kind \"DeleteOptions\" is registered",
		},
		{
			name:         "decode '{}' to metav1.DeleteOptions with metainternalversionscheme.Codecs",
			codecFactory: metainternalversionscheme.Codecs,
			data:         []byte("{}"),
			expectErr:    "",
		},
		{
			name:         "decode versioned (corev1) DeleteOptions with metainternalversionscheme.Codecs",
			codecFactory: metainternalversionscheme.Codecs,
			data:         []byte(`{"apiVersion":"v1","kind":"DeleteOptions","gracePeriodSeconds":123}`),
			expectErr:    "",
		},
		{
			name:         "decode versioned (foo) DeleteOptions with metainternalversionscheme.Codecs",
			codecFactory: metainternalversionscheme.Codecs,
			data:         []byte(`{"apiVersion":"foo/v1","kind":"DeleteOptions","gracePeriodSeconds":123}`),
			expectErr:    "",
		},
	}

	defaultGVK := metav1.SchemeGroupVersion.WithKind("DeleteOptions")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, err := negotiation.NegotiateInputSerializer(req, false, test.codecFactory)
			if err != nil {
				t.Fatal(err)
			}

			options := &metav1.DeleteOptions{}
			_, _, err = metainternalversionscheme.Codecs.DecoderToVersion(s.Serializer, defaultGVK.GroupVersion()).Decode(test.data, &defaultGVK, options)
			if test.expectErr != "" {
				if err == nil {
					t.Fatalf("expect %s but got nil", test.expectErr)
				}
				if !strings.Contains(err.Error(), test.expectErr) {
					t.Fatalf("expect %s but got %s", test.expectErr, err.Error())
				}
			}
		})
	}
}

func TestDeleteResourceWithUnsafeDeleter(t *testing.T) {
	tests := []struct {
		name           string
		featureEnabled bool
		unsafeDeleter  *bool // how we setup the registry store
		ignoreReadErr  bool  // what the user passes in the delete options for ignoreStoreReadErrorWithClusterBreakingPotential
		authorizer     authorizer.Authorizer
		// want
		regularDeleterInvoked int    // how many times the regular deleter should be invoked
		unsafeDeleterInvoked  int    // how many times the unsafe deleter should be invoked
		statusCode            int    // what the delete handler should return
		errshouldContain      string // the expected error in the response body
		unsafeAnnotation      bool   // the expected audit annotation
	}{
		{
			name:                  "feature disabled, registry does not provide an unsafe deleter, ignore not set, should invoke normal deleter",
			featureEnabled:        false,
			unsafeDeleter:         nil, // registry does not provide an unsafe deleter
			ignoreReadErr:         false,
			regularDeleterInvoked: 1,
			statusCode:            http.StatusOK,
		},
		{
			name:                  "feature disabled, registry provides an unsafe deleter, ignore not set, should invoke normal deleter",
			featureEnabled:        false,
			unsafeDeleter:         ptr.To[bool](true),
			ignoreReadErr:         false,
			regularDeleterInvoked: 1,
			statusCode:            http.StatusOK,
		},
		{
			name:                  "feature disabled, registry provides an unsafe deleter, ignore set, should invoke normal deleter",
			featureEnabled:        false,
			unsafeDeleter:         ptr.To[bool](true),
			ignoreReadErr:         true,
			regularDeleterInvoked: 1,
			statusCode:            http.StatusOK,
		},
		{
			name:                  "feature enabled, registry does not provide an unsafe deleter, ignore not set, should invoke normal deleter",
			featureEnabled:        true,
			unsafeDeleter:         nil,
			ignoreReadErr:         false,
			regularDeleterInvoked: 1,
			statusCode:            http.StatusOK,
		},
		{
			name:                  "feature enabled, registry does not provide an unsafe deleter, ignore set, error expected",
			featureEnabled:        true,
			unsafeDeleter:         nil,
			ignoreReadErr:         true,
			statusCode:            http.StatusInternalServerError,
			errshouldContain:      "no unsafe deleter provided, can not honor ignoreStoreReadErrorWithClusterBreakingPotential",
			regularDeleterInvoked: 0,
			unsafeAnnotation:      true,
		},
		{
			name:                  "feature enabled, registry provides a nil unsafe deleter, ignore set, error expected",
			featureEnabled:        true,
			unsafeDeleter:         ptr.To[bool](false),
			ignoreReadErr:         true,
			statusCode:            http.StatusInternalServerError,
			errshouldContain:      "no unsafe deleter provided, can not honor ignoreStoreReadErrorWithClusterBreakingPotential",
			regularDeleterInvoked: 0,
			unsafeAnnotation:      true,
		},
		{
			name:                  "feature enabled, registry provides an unsafe deleter, ignore set, no authorizer, error expected",
			featureEnabled:        true,
			unsafeDeleter:         ptr.To[bool](true),
			ignoreReadErr:         true,
			statusCode:            http.StatusInternalServerError,
			errshouldContain:      "Internal error occurred: no authorizer provided, unable to authorize unsafe delete",
			regularDeleterInvoked: 0,
			unsafeAnnotation:      true,
		},
		{
			name:                  "feature enabled, registry provides an unsafe deleter, ignore set, not authorized, error expected",
			featureEnabled:        true,
			unsafeDeleter:         ptr.To[bool](true),
			ignoreReadErr:         true,
			authorizer:            &fakeAuthorizer{decision: authorizer.DecisionDeny, reason: "not permitted"},
			statusCode:            http.StatusForbidden,
			errshouldContain:      "forbidden: not permitted to do",
			regularDeleterInvoked: 0,
			unsafeAnnotation:      true,
		},
		{
			name:                  "feature enabled, registry provides an unsafe deleter, ignore set, authorized, shold invoke unsafe deleter",
			featureEnabled:        true,
			unsafeDeleter:         ptr.To[bool](true),
			ignoreReadErr:         true,
			authorizer:            &fakeAuthorizer{decision: authorizer.DecisionAllow, reason: "permitted"},
			statusCode:            http.StatusOK,
			regularDeleterInvoked: 0,
			unsafeDeleterInvoked:  1,
			unsafeAnnotation:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AllowUnsafeMalformedObjectDeletion, test.featureEnabled)

			scheme := runtime.NewScheme()
			metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
			if err := metainternalversion.AddToScheme(scheme); err != nil {
				t.Fatalf("metainternalversion.AddToScheme failed - err: %v", err)
			}
			codecs := serializer.NewCodecFactory(scheme)
			info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), runtime.ContentTypeJSON)
			if !ok {
				t.Fatalf("failed to setup serializer")
			}
			scope := &RequestScope{
				Namer:            &mockNamer{},
				Serializer:       runtime.NewSimpleNegotiatedSerializer(info),
				Authorizer:       test.authorizer,
				MetaGroupVersion: metav1.SchemeGroupVersion,
			}

			// keep track of what the normal and unsafe deleter observes.
			// the normal deleter should never see the ignore option
			// enabled, so initializing it to true, so we can verify
			// that it is reset to nil
			normalDeleterTracker := &deleteTracker{ignore: ptr.To[bool](true)}
			// on the other hand, the unsafe deleter should always
			// see the ignore options enabled.
			unsafeDeleterTracker := &deleteTracker{}

			var deleter rest.GracefulDeleter = &fakeDeleter{got: normalDeleterTracker}
			switch {
			case test.unsafeDeleter == nil:
			case *test.unsafeDeleter:
				deleter = &fakeCorruptObjDeleter{GracefulDeleter: deleter, unsafeDeleter: &fakeDeleter{unsafeDeleterTracker}}
			default:
				// it returns nil when asked for an unsafe deleter
				deleter = &fakeCorruptObjDeleter{GracefulDeleter: deleter}
			}

			handler := DeleteResource(deleter, true, scope, &fakeAdmitter{})
			req := newRequest(t, test.ignoreReadErr)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			// verify the http response expected
			resp := recorder.Result()
			if want, got := test.statusCode, resp.StatusCode; want != got {
				t.Errorf("expected status code: %d, but got: %d", want, got)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("unexpected error reading response body: %v", err)
			}
			switch {
			case len(test.errshouldContain) > 0:
				if want, got := test.errshouldContain, string(body); !strings.Contains(got, want) {
					t.Errorf("expected response body to contain %q, but got: %s", want, got)
				}
			default:
				// we expect Success
				if want, got := `"status":"Success"`, string(body); !strings.Contains(got, want) {
					t.Errorf("expected response body to contain %q, but got: %s", want, got)
				}
			}

			// verify normal deleter expectations
			if want, got := test.regularDeleterInvoked, normalDeleterTracker.invoked; want != got {
				t.Errorf("expected the normal deleter to be ivoked %d times, but got: %d", want, got)
			}
			// if invoked, the normal deleter should never see the ignore option enabled
			if got := normalDeleterTracker.ignore; test.regularDeleterInvoked == 1 && got != nil {
				t.Errorf("expected IgnoreStoreReadErrorWithClusterBreakingPotential to be nil for the normal deleter, but got: %t", *got)
			}
			// we expect certain annotation to be added
			const keyWant = "apiserver.k8s.io/unsafe-delete-ignore-read-error"
			ac := audit.AuditContextFrom(req.Context())
			switch test.unsafeAnnotation {
			case true:
				if value, ok := ac.Event.Annotations[keyWant]; !ok || len(value) > 0 {
					t.Errorf("expected annotation %q to exist with an empty value, but got exists: %t, value: %q", keyWant, ok, value)
				}
			default:
				if value, ok := ac.Event.Annotations[keyWant]; ok || len(value) > 0 {
					t.Errorf("did not expect annotation %q to exist, but got exists: %t, value: %q", keyWant, ok, value)
				}
			}

			// verify unsafe deleter expectations
			if want, got := test.unsafeDeleterInvoked, unsafeDeleterTracker.invoked; want != got {
				t.Errorf("expected the unsafe deleter to be nvoked %d times, but got: %d", want, got)
			}
			// if invoked, the unsafe deleter should always see the option enabled
			if got := ptr.Deref[bool](unsafeDeleterTracker.ignore, false); test.unsafeDeleterInvoked == 1 && !got {
				t.Errorf("expected IgnoreStoreReadErrorWithClusterBreakingPotential to be %t for the unsafe deleter, but got: %t", false, got)
			}
		})
	}
}

func TestDeleteCollectionWithUnsafeDeleter(t *testing.T) {
	tests := []struct {
		name           string
		featureEnabled bool
		ignoreReadErr  bool // what the user passes in the delete options for ignoreStoreReadErrorWithClusterBreakingPotential
		// want
		deleterInvoked   int    // how many times the regular deleter was invoked
		statusCode       int    // what the delete collection handler returned
		errshouldContain string // the expected error in the response body
	}{
		{
			name:             "feature disabled, ignore not set, should invoke the deleter",
			featureEnabled:   false,
			ignoreReadErr:    false,
			deleterInvoked:   1,
			statusCode:       http.StatusOK,
			errshouldContain: "",
		},
		{
			name:             "feature disabled, ignore set, should invoke the deleter",
			featureEnabled:   false,
			ignoreReadErr:    true,
			deleterInvoked:   1,
			statusCode:       http.StatusOK,
			errshouldContain: "",
		},
		{
			name:             "feature enabled, ignore not set, should invoke deleter",
			featureEnabled:   true,
			ignoreReadErr:    false,
			deleterInvoked:   1,
			statusCode:       http.StatusOK,
			errshouldContain: "",
		},
		{
			name:             "feature enabled, ignore set, invalid error expected",
			featureEnabled:   true,
			ignoreReadErr:    true,
			statusCode:       http.StatusUnprocessableEntity,
			errshouldContain: `is invalid: ignoreStoreReadErrorWithClusterBreakingPotential: Invalid value: true: is not allowed with DELETECOLLECTION, try again after removing the option`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AllowUnsafeMalformedObjectDeletion, test.featureEnabled)

			scheme := runtime.NewScheme()
			metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
			if err := metainternalversion.AddToScheme(scheme); err != nil {
				t.Fatalf("metainternalversion.AddToScheme failed - err: %v", err)
			}
			codecs := serializer.NewCodecFactory(scheme)
			info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), runtime.ContentTypeJSON)
			if !ok {
				t.Fatalf("failed to setup serializer")
			}
			scope := &RequestScope{
				Namer:            &mockNamer{},
				Serializer:       runtime.NewSimpleNegotiatedSerializer(info),
				ParameterCodec:   runtime.NewParameterCodec(scheme),
				MetaGroupVersion: metav1.SchemeGroupVersion,
			}

			// the deleter should never see the ignore option enabled, so
			// initializing it to true, so we can verify that it is reset to nil
			tracker := &deleteTracker{ignore: ptr.To[bool](true)}
			deleter := func(ctx context.Context, _ rest.ValidateObjectFunc, options *metav1.DeleteOptions, _ *metainternalversion.ListOptions) (runtime.Object, error) {
				tracker.invoked++
				tracker.ignore = options.IgnoreStoreReadErrorWithClusterBreakingPotential
				return nil, nil
			}

			handler := DeleteCollection(fakeCollectionDeleterFunc(deleter), true, scope, &fakeAdmitter{})
			req := newRequest(t, test.ignoreReadErr)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			// verify the http response
			resp := recorder.Result()
			if want, got := test.statusCode, resp.StatusCode; want != got {
				t.Errorf("expected status code: %d, but got: %d", want, got)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("unexpected error reading response body: %v", err)
			}
			switch {
			case len(test.errshouldContain) > 0:
				if want, got := test.errshouldContain, string(body); !strings.Contains(got, want) {
					t.Errorf("expected response body to contain %q, but got: %s", want, got)
				}
			default:
				// we expect Success
				if want, got := `"status":"Success"`, string(body); !strings.Contains(got, want) {
					t.Errorf("expected response body to contain %q, but got: %s", want, got)
				}
			}
			if want, got := test.deleterInvoked, tracker.invoked; want != got {
				t.Errorf("expected the deleter to be ivoked %d times, but got: %d", want, got)
			}
			// if invoked, the deleter should never see the ignore option enabled
			if got := tracker.ignore; test.deleterInvoked == 1 && got != nil {
				t.Errorf("expected IgnoreStoreReadErrorWithClusterBreakingPotential to be nil for the deleter, but got: %t", *got)
			}
			// we never expect the annotation to be added
			const keyNeverWant = "apiserver.k8s.io/unsafe-delete-ignore-read-error"
			ac := audit.AuditContextFrom(req.Context())
			if value, ok := ac.Event.Annotations[keyNeverWant]; ok || len(value) > 0 {
				t.Errorf("did not expect annotation %q to exist, but got exists: %t, value: %q", keyNeverWant, ok, value)
			}
		})
	}
}

func newRequest(t *testing.T, ignoreReadErr bool) *http.Request {
	req, err := http.NewRequest(http.MethodGet, "/test", ioutil.NopCloser(strings.NewReader("")))
	if err != nil {
		t.Fatalf("unexpected error creating a new http.Request: %v", err)
	}

	reqInfo := &request.RequestInfo{IsResourceRequest: true}
	req = req.WithContext(request.WithRequestInfo(req.Context(), reqInfo))

	ctx := audit.WithAuditContext(req.Context())
	ac := audit.AuditContextFrom(ctx)
	ac.RequestAuditConfig.Level = auditapis.LevelMetadata
	req = req.WithContext(ctx)

	if ignoreReadErr {
		q := req.URL.Query()
		q.Add("ignoreStoreReadErrorWithClusterBreakingPotential", "true")
		req.URL.RawQuery = q.Encode()
	}

	req.Header.Set("Content-Type", "application/json")
	return req
}

// keeps track of what the deleter observes
type deleteTracker struct {
	invoked int   // how many times Delete has been invoked
	ignore  *bool // the ignore option passed to Delete
}

type fakeDeleter struct {
	got *deleteTracker
}

func (f *fakeDeleter) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	f.got.invoked++
	if options.IgnoreStoreReadErrorWithClusterBreakingPotential == nil {
		f.got.ignore = nil
	} else {
		f.got.ignore = ptr.To[bool](*options.IgnoreStoreReadErrorWithClusterBreakingPotential)
	}

	return nil, true, nil
}

type fakeCorruptObjDeleter struct {
	rest.GracefulDeleter // normal deleter
	unsafeDeleter        rest.GracefulDeleter
}

func (p *fakeCorruptObjDeleter) GetCorruptObjDeleter() rest.GracefulDeleter {
	return p.unsafeDeleter
}

type fakeAdmitter struct{}

func (f *fakeAdmitter) Handles(_ admission.Operation) bool { return false }

func TestDeleteCollectionWithNoContextDeadlineEnforced(t *testing.T) {
	var invokedGot, hasDeadlineGot int32
	fakeDeleterFn := func(ctx context.Context, _ rest.ValidateObjectFunc, _ *metav1.DeleteOptions, _ *metainternalversion.ListOptions) (runtime.Object, error) {
		// we expect CollectionDeleter to be executed once
		atomic.AddInt32(&invokedGot, 1)

		// we don't expect any context deadline to be set
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			atomic.AddInt32(&hasDeadlineGot, 1)
		}
		return nil, nil
	}

	// do the minimum setup to ensure that it gets as far as CollectionDeleter
	scope := &RequestScope{
		Namer: &mockNamer{},
		Serializer: &fakeSerializer{
			serializer: runtime.NewCodec(runtime.NoopEncoder{}, runtime.NoopDecoder{}),
		},
	}
	handler := DeleteCollection(fakeCollectionDeleterFunc(fakeDeleterFn), false, scope, nil)

	request, err := http.NewRequest("GET", "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// the request context should not have any deadline by default
	if _, hasDeadline := request.Context().Deadline(); hasDeadline {
		t.Fatalf("expected request context to not have any deadline")
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if atomic.LoadInt32(&invokedGot) != 1 {
		t.Errorf("expected collection deleter to be invoked")
	}
	if atomic.LoadInt32(&hasDeadlineGot) > 0 {
		t.Errorf("expected context to not have any deadline")
	}
}

type fakeCollectionDeleterFunc func(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error)

func (f fakeCollectionDeleterFunc) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	return f(ctx, deleteValidation, options, listOptions)
}

type fakeSerializer struct {
	serializer runtime.Serializer
}

func (n *fakeSerializer) SupportedMediaTypes() []runtime.SerializerInfo {
	return []runtime.SerializerInfo{
		{
			MediaType:        "application/json",
			MediaTypeType:    "application",
			MediaTypeSubType: "json",
		},
	}
}
func (n *fakeSerializer) EncoderForVersion(serializer runtime.Encoder, gv runtime.GroupVersioner) runtime.Encoder {
	return n.serializer
}
func (n *fakeSerializer) DecoderToVersion(serializer runtime.Decoder, gv runtime.GroupVersioner) runtime.Decoder {
	return n.serializer
}

func TestAuthorizeUnsafeDelete(t *testing.T) {
	const verbWant = "unsafe-delete-ignore-read-errors"
	tests := []struct {
		name    string
		reqInfo *request.RequestInfo
		attr    admission.Attributes
		authz   authorizer.Authorizer
		err     func(admission.Attributes) error
	}{
		{
			name:  "operation is not delete, admit",
			attr:  newAttributes(attributes{operation: admission.Update}),
			authz: nil, // Authorize should not be invoked
		},
		{
			name: "feature enabled, delete, operation option is nil, admit",
			attr: newAttributes(attributes{
				operation:        admission.Delete,
				operationOptions: nil,
			}),
			authz: nil, // Authorize should not be invoked
		},
		{
			name: "delete, operation option is not a match, forbid",
			attr: newAttributes(attributes{
				operation:        admission.Delete,
				operationOptions: &metav1.PatchOptions{},
			}),
			authz: nil, // Authorize should not be invoked
			err: func(admission.Attributes) error {
				return errors.NewInternalError(fmt.Errorf("expected an option of type: %T, but got: %T", &metav1.DeleteOptions{}, &metav1.PatchOptions{}))
			},
		},
		{
			name: "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is nil, admit",
			attr: newAttributes(attributes{
				operation: admission.Delete,
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: nil,
				},
			}),
			authz: nil, // Authorize should not be invoked
		},
		{
			name: "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is false, admit",
			attr: newAttributes(attributes{
				operation: admission.Delete,
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](false),
				},
			}),
			authz: nil, // Authorize should not be invoked
		},
		{
			name:    "feature enabled, delete, IgnoreStoreReadErrorWithClusterBreakingPotential is true, no RequestInfo in request context, forbid",
			reqInfo: nil,
			attr: newAttributes(attributes{
				operation: admission.Delete,
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](true),
				},
			}),
			authz: nil,
			err: func(attr admission.Attributes) error {
				return admission.NewForbidden(attr, fmt.Errorf("no RequestInfo found in the context"))
			},
		},
		{
			name:    "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is true, subresource request, forbid",
			reqInfo: &request.RequestInfo{IsResourceRequest: true},
			attr: newAttributes(attributes{
				operation:   admission.Delete,
				subresource: "foo",
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](true),
				},
			}),
			authz: nil,
			err: func(attr admission.Attributes) error {
				return admission.NewForbidden(attr, fmt.Errorf("ignoreStoreReadErrorWithClusterBreakingPotential delete option is not allowed on a subresource or non-resource request"))
			},
		},
		{
			name:    "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is true, subresource request, forbid",
			reqInfo: &request.RequestInfo{IsResourceRequest: false},
			attr: newAttributes(attributes{
				operation:   admission.Delete,
				subresource: "",
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](true),
				},
			}),
			authz: nil,
			err: func(attr admission.Attributes) error {
				return admission.NewForbidden(attr, fmt.Errorf("ignoreStoreReadErrorWithClusterBreakingPotential delete option is not allowed on a subresource or non-resource request"))
			},
		},
		{
			name:    "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is true, authorizer returns error, forbid",
			reqInfo: &request.RequestInfo{IsResourceRequest: true},
			attr: newAttributes(attributes{
				subresource: "",
				operation:   admission.Delete,
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](true),
				},
			}),
			authz: &fakeAuthorizer{err: fmt.Errorf("unexpected error")},
			err: func(attr admission.Attributes) error {
				return admission.NewForbidden(attr, fmt.Errorf("error while checking permission for %q, %w", verbWant, fmt.Errorf("unexpected error")))
			},
		},
		{
			name:    "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is true, user does not have permission, forbid",
			reqInfo: &request.RequestInfo{IsResourceRequest: true},
			attr: newAttributes(attributes{
				operation:   admission.Delete,
				subresource: "",
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](true),
				},
			}),
			authz: &fakeAuthorizer{
				decision: authorizer.DecisionDeny,
				reason:   "does not have permission",
			},
			err: func(attr admission.Attributes) error {
				return admission.NewForbidden(attr, fmt.Errorf("not permitted to do %q, reason: %s", verbWant, "does not have permission"))
			},
		},
		{
			name:    "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is true, authorizer gives no opinion, forbid",
			reqInfo: &request.RequestInfo{IsResourceRequest: true},
			attr: newAttributes(attributes{
				operation:   admission.Delete,
				subresource: "",
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](true),
				},
			}),
			authz: &fakeAuthorizer{
				decision: authorizer.DecisionNoOpinion,
				reason:   "no opinion",
			},
			err: func(attr admission.Attributes) error {
				return admission.NewForbidden(attr, fmt.Errorf("not permitted to do %q, reason: %s", verbWant, "no opinion"))
			},
		},
		{
			name:    "delete, IgnoreStoreReadErrorWithClusterBreakingPotential is true, user has permission, admit",
			reqInfo: &request.RequestInfo{IsResourceRequest: true},
			attr: newAttributes(attributes{
				operation:   admission.Delete,
				subresource: "",
				operationOptions: &metav1.DeleteOptions{
					IgnoreStoreReadErrorWithClusterBreakingPotential: ptr.To[bool](true),
				},
				userInfo: &user.DefaultInfo{Name: "foo"},
			}),
			authz: &fakeAuthorizer{
				decision: authorizer.DecisionAllow,
				reason:   "permitted",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var want error
			if test.err != nil {
				want = test.err(test.attr)
			}

			ctx := context.Background()
			if test.reqInfo != nil {
				ctx = request.WithRequestInfo(ctx, test.reqInfo)
			}

			// wrap the attributes so we can access the annotations set during admission
			attrs := &fakeAttributes{Attributes: test.attr}
			got := authorizeUnsafeDelete(ctx, attrs, test.authz)
			switch {
			case want != nil:
				if got == nil || want.Error() != got.Error() {
					t.Errorf("expected error: %v, but got: %v", want, got)
				}
			default:
				if got != nil {
					t.Errorf("expected no error, but got: %v", got)
				}
			}
		})
	}
}

// attributes of interest for this test
type attributes struct {
	operation        admission.Operation
	operationOptions runtime.Object
	userInfo         user.Info
	subresource      string
}

func newAttributes(attr attributes) admission.Attributes {
	return admission.NewAttributesRecord(
		nil,                           // this plugin should never inspect the object
		nil,                           // old object, this plugin should never inspect it
		schema.GroupVersionKind{},     // this plugin should never inspect kind
		"",                            // namespace, leave it empty, this plugin only passes it along to the authorizer
		"",                            // name, leave it empty, this plugin only passes it along to the authorizer
		schema.GroupVersionResource{}, // resource, leave it empty, this plugin only passes it along to the authorizer
		attr.subresource,
		attr.operation,
		attr.operationOptions,
		false, // dryRun, this plugin should never inspect this attribute
		attr.userInfo)
}

type fakeAttributes struct {
	admission.Attributes
	annotations map[string]string
}

func (f *fakeAttributes) AddAnnotation(key, value string) error {
	if err := f.Attributes.AddAnnotation(key, value); err != nil {
		return err
	}

	if len(f.annotations) == 0 {
		f.annotations = map[string]string{}
	}
	f.annotations[key] = value
	return nil
}

type fakeAuthorizer struct {
	decision authorizer.Decision
	reason   string
	err      error
}

func (authorizer fakeAuthorizer) Authorize(ctx context.Context, a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
	return authorizer.decision, authorizer.reason, authorizer.err
}
