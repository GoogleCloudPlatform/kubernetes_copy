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
	"k8s.io/apiserver/pkg/registry/rest"

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
	ac.SetEventLevel(auditapis.LevelRequestResponse)

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
