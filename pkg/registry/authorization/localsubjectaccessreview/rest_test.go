/*
Copyright 2021 The Kubernetes Authors.

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

package localsubjectaccessreview

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	authorizationapi "k8s.io/kubernetes/pkg/apis/authorization"
)

type fakeAuthorizer struct {
	attrs authorizer.Attributes

	decision authorizer.Decision
	reason   string
	err      error
}

func (f *fakeAuthorizer) Authorize(ctx context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
	f.attrs = attrs
	return f.decision, f.reason, f.err
}

func TestCreate(t *testing.T) {
	testcases := map[string]struct {
		spec              authorizationapi.SubjectAccessReviewSpec
		decision          authorizer.Decision
		reason            string
		err               error
		user              user.Info
		namespaceContext  string
		namespaceMetadata string

		expectedErr    string
		expectedAttrs  authorizer.Attributes
		expectedStatus authorizationapi.SubjectAccessReviewStatus
	}{
		"empty": {
			expectedErr: "nonResourceAttributes or resourceAttributes",
		},

		"resource rejected": {
			spec: authorizationapi.SubjectAccessReviewSpec{
				User: "bob",
				ResourceAttributes: &authorizationapi.ResourceAttributes{
					Namespace:   "myns",
					Verb:        "create",
					Group:       "extensions",
					Version:     "v1beta1",
					Resource:    "deployments",
					Subresource: "scale",
					Name:        "mydeployment",
				},
			},
			decision:          authorizer.DecisionNoOpinion,
			reason:            "myreason",
			err:               errors.New("myerror"),
			user:              &user.DefaultInfo{Name: "bob"},
			namespaceContext:  "myns",
			namespaceMetadata: "myns",
			expectedAttrs: authorizer.AttributesRecord{
				User:            &user.DefaultInfo{Name: "bob"},
				Namespace:       "myns",
				Verb:            "create",
				APIGroup:        "extensions",
				APIVersion:      "v1beta1",
				Resource:        "deployments",
				Subresource:     "scale",
				Name:            "mydeployment",
				ResourceRequest: true,
			},
			expectedStatus: authorizationapi.SubjectAccessReviewStatus{
				Allowed:         false,
				Denied:          false,
				Reason:          "myreason",
				EvaluationError: "myerror",
			},
		},

		"resource allowed": {
			spec: authorizationapi.SubjectAccessReviewSpec{
				User: "bob",
				ResourceAttributes: &authorizationapi.ResourceAttributes{
					Namespace:   "myns",
					Verb:        "create",
					Group:       "extensions",
					Version:     "v1beta1",
					Resource:    "deployments",
					Subresource: "scale",
					Name:        "mydeployment",
				},
			},
			decision:          authorizer.DecisionAllow,
			reason:            "allowed",
			err:               nil,
			user:              &user.DefaultInfo{Name: "bob"},
			namespaceContext:  "myns",
			namespaceMetadata: "myns",
			expectedAttrs: authorizer.AttributesRecord{
				User:            &user.DefaultInfo{Name: "bob"},
				Namespace:       "myns",
				Verb:            "create",
				APIGroup:        "extensions",
				APIVersion:      "v1beta1",
				Resource:        "deployments",
				Subresource:     "scale",
				Name:            "mydeployment",
				ResourceRequest: true,
			},
			expectedStatus: authorizationapi.SubjectAccessReviewStatus{
				Allowed:         true,
				Denied:          false,
				Reason:          "allowed",
				EvaluationError: "",
			},
		},

		"resource denied": {
			spec: authorizationapi.SubjectAccessReviewSpec{
				User: "bob",
				ResourceAttributes: &authorizationapi.ResourceAttributes{
					Namespace: "myns",
				},
			},
			user:              &user.DefaultInfo{Name: "bob"},
			namespaceContext:  "myns",
			namespaceMetadata: "myns",
			decision:          authorizer.DecisionDeny,
			expectedAttrs: authorizer.AttributesRecord{
				User:            &user.DefaultInfo{Name: "bob"},
				Namespace:       "myns",
				ResourceRequest: true,
				APIVersion:      "*",
			},
			expectedStatus: authorizationapi.SubjectAccessReviewStatus{
				Allowed: false,
				Denied:  true,
			},
		},

		"invalid": {
			spec: authorizationapi.SubjectAccessReviewSpec{
				User: "bob",
				ResourceAttributes: &authorizationapi.ResourceAttributes{
					Namespace: "myns",
				},
			},
			expectedErr: "must match metadata.namespace",
		},

		"no namespace": {
			spec: authorizationapi.SubjectAccessReviewSpec{
				User: "bob",
				ResourceAttributes: &authorizationapi.ResourceAttributes{
					Namespace: "myns",
				},
			},
			namespaceContext:  "",
			namespaceMetadata: "myns",
			expectedErr:       "namespace is required on this type",
		},
	}

	for k, tc := range testcases {
		auth := &fakeAuthorizer{
			decision: tc.decision,
			reason:   tc.reason,
			err:      tc.err,
		}
		storage := NewREST(auth)

		result, err := storage.Create(genericapirequest.WithNamespace(genericapirequest.NewContext(), tc.namespaceContext), &authorizationapi.LocalSubjectAccessReview{ObjectMeta: metav1.ObjectMeta{Namespace: tc.namespaceMetadata}, Spec: tc.spec}, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
		if err != nil {
			if tc.expectedErr != "" {
				if !strings.Contains(err.Error(), tc.expectedErr) {
					t.Errorf("%s: expected %s to contain %q", k, err, tc.expectedErr)
				}
			} else {
				t.Errorf("%s: %v", k, err)
			}
			continue
		}
		if !reflect.DeepEqual(auth.attrs, tc.expectedAttrs) {
			t.Errorf("%s: expected\n%#v\ngot\n%#v", k, tc.expectedAttrs, auth.attrs)
		}
		status := result.(*authorizationapi.LocalSubjectAccessReview).Status
		if !reflect.DeepEqual(status, tc.expectedStatus) {
			t.Errorf("%s: expected\n%#v\ngot\n%#v", k, tc.expectedStatus, status)
		}
	}
}
