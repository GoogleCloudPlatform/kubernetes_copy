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

package selfsubjectrulesreview

import (
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

type fakeRuleResolver struct {
	resourceRuleInfo    []authorizer.ResourceRuleInfo
	nonResourceRuleInfo []authorizer.NonResourceRuleInfo
	incomplete          bool
	err                 error
}

func (f *fakeRuleResolver) RulesFor(user user.Info, namespace string) ([]authorizer.ResourceRuleInfo, []authorizer.NonResourceRuleInfo, bool, error) {
	return f.resourceRuleInfo, f.nonResourceRuleInfo, f.incomplete, f.err
}

func TestCreate(t *testing.T) {
	testcases := map[string]struct {
		spec                authorizationapi.SelfSubjectRulesReviewSpec
		resourceRuleInfo    []authorizer.ResourceRuleInfo
		nonResourceRuleInfo []authorizer.NonResourceRuleInfo
		incomplete          bool
		err                 error
		user                user.Info

		expectedErr    string
		expectedStatus authorizationapi.SubjectRulesReviewStatus
	}{
		"nonresource": {
			spec: authorizationapi.SelfSubjectRulesReviewSpec{
				Namespace: "mynamespace",
			},
			resourceRuleInfo: []authorizer.ResourceRuleInfo{},
			nonResourceRuleInfo: []authorizer.NonResourceRuleInfo{
				&authorizer.DefaultNonResourceRuleInfo{
					Verbs:           []string{"get"},
					NonResourceURLs: []string{"/mypath"},
				},
			},
			incomplete: true,
			err:        errors.New("myerror"),
			user:       &user.DefaultInfo{Name: "bob"},
			expectedStatus: authorizationapi.SubjectRulesReviewStatus{
				ResourceRules: []authorizationapi.ResourceRule{},
				NonResourceRules: []authorizationapi.NonResourceRule{
					{
						Verbs:           []string{"get"},
						NonResourceURLs: []string{"/mypath"},
					},
				},
				Incomplete:      true,
				EvaluationError: "myerror",
			},
		},
		"resource": {
			spec: authorizationapi.SelfSubjectRulesReviewSpec{
				Namespace: "mynamespace",
			},
			resourceRuleInfo: []authorizer.ResourceRuleInfo{
				&authorizer.DefaultResourceRuleInfo{
					Verbs:     []string{"get"},
					APIGroups: []string{"extensions"},
					Resources: []string{"deployments"},
				},
			},
			nonResourceRuleInfo: []authorizer.NonResourceRuleInfo{},
			incomplete:          true,
			err:                 errors.New("myerror"),
			user:                &user.DefaultInfo{Name: "bob"},
			expectedStatus: authorizationapi.SubjectRulesReviewStatus{
				ResourceRules: []authorizationapi.ResourceRule{
					{
						Verbs:     []string{"get"},
						APIGroups: []string{"extensions"},
						Resources: []string{"deployments"},
					},
				},
				NonResourceRules: []authorizationapi.NonResourceRule{},
				Incomplete:       true,
				EvaluationError:  "myerror",
			},
		},
		"no user": {
			spec: authorizationapi.SelfSubjectRulesReviewSpec{
				Namespace: "mynamespace",
			},
			expectedErr: "no user present on request",
		},
		"no namespace": {
			spec:        authorizationapi.SelfSubjectRulesReviewSpec{},
			user:        &user.DefaultInfo{Name: "bob"},
			expectedErr: "no namespace on request",
		},
	}

	for k, tc := range testcases {
		auth := &fakeRuleResolver{
			resourceRuleInfo:    tc.resourceRuleInfo,
			nonResourceRuleInfo: tc.nonResourceRuleInfo,
			incomplete:          tc.incomplete,
			err:                 tc.err,
		}
		storage := NewREST(auth)

		result, err := storage.Create(genericapirequest.WithUser(genericapirequest.NewContext(), tc.user), &authorizationapi.SelfSubjectRulesReview{Spec: tc.spec}, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
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

		status := result.(*authorizationapi.SelfSubjectRulesReview).Status
		if !reflect.DeepEqual(status, tc.expectedStatus) {
			t.Errorf("%s: expected\n%#v\ngot\n%#v", k, tc.expectedStatus, status)
		}
	}
}
