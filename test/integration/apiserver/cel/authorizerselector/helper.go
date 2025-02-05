/*
Copyright 2024 The Kubernetes Authors.

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

package authorizerselector

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/kubernetes"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/utils/ptr"
)

func RunAuthzSelectorsLibraryTests(t *testing.T, featureEnabled bool) {
	if _, initialized := environment.AuthzSelectorsLibraryEnabled(); initialized {
		// This ensures CEL environments don't get initialized during init(),
		// before they can be informed by configured feature gates.
		// If this check fails, uncomment the debug.PrintStack() when the authz selectors
		// library is first initialized to find the culprit, and modify it to be lazily initialized on first use.
		t.Fatalf("authz selector library was initialized before feature gates were finalized (possibly from an init() or package variable)")
	}

	// Start the server with the desired feature enablement
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		fmt.Sprintf("--feature-gates=AuthorizeNodeWithSelectors=%v,AuthorizeWithSelectors=%v", featureEnabled, featureEnabled),
		"--runtime-config=resource.k8s.io/v1alpha3=true",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	// Ensure the authz selectors library was initialized and saw the right feature enablement
	if gotEnabled, initialized := environment.AuthzSelectorsLibraryEnabled(); !initialized {
		t.Fatalf("authz selector library was not initialized during API server construction")
	} else if gotEnabled != featureEnabled {
		t.Fatalf("authz selector library enabled=%v, expected %v", gotEnabled, featureEnabled)
	}

	// Attempt to create API objects using the fieldSelector and labelSelector authorizer functions,
	// and ensure they are only allowed when the feature is enabled.

	c, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("Failed to create clientset: %v", err)
	}
	crdClient, err := extclientset.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("Failed to create clientset: %v", err)
	}

	boolFieldSelectorExpression := `type(authorizer.group('').resource('').fieldSelector('')) == string`
	stringFieldSelectorExpression := boolFieldSelectorExpression + ` ? 'yes' : 'no'`
	fieldSelectorErrorSubstring := `undeclared reference to 'fieldSelector'`

	testcases := []struct {
		name                     string
		createObject             func() error
		expectErrorsWhenEnabled  []*regexp.Regexp
		expectErrorsWhenDisabled []*regexp.Regexp
	}{
		{
			name: "ValidatingAdmissionPolicy",
			createObject: func() error {
				obj := &admissionregistrationv1.ValidatingAdmissionPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: "test-with-variables"},
					Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
						MatchConstraints: &admissionregistrationv1.MatchResources{
							ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
								RuleWithOperations: admissionregistrationv1.RuleWithOperations{
									Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
									Rule:       admissionregistrationv1.Rule{APIGroups: []string{"example.com"}, APIVersions: []string{"*"}, Resources: []string{"*"}}}}}},
						Validations: []admissionregistrationv1.Validation{{
							Expression:        boolFieldSelectorExpression,
							MessageExpression: stringFieldSelectorExpression}},
						AuditAnnotations: []admissionregistrationv1.AuditAnnotation{{Key: "test", ValueExpression: stringFieldSelectorExpression}},
						MatchConditions:  []admissionregistrationv1.MatchCondition{{Name: "test", Expression: boolFieldSelectorExpression}},
						Variables:        []admissionregistrationv1.Variable{{Name: "test", Expression: boolFieldSelectorExpression}}}}
				_, err := c.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), obj, metav1.CreateOptions{})
				return err
			},
			expectErrorsWhenEnabled: []*regexp.Regexp{
				// authorizer is not available to messageExpression
				regexp.MustCompile(`spec\.validations\[0\]\.messageExpression:.*undeclared reference to 'authorizer'`),
			},
			expectErrorsWhenDisabled: []*regexp.Regexp{
				regexp.MustCompile(`spec\.validations\[0\]\.expression:.*` + fieldSelectorErrorSubstring),
				// authorizer is not available to messageExpression
				regexp.MustCompile(`spec\.validations\[0\]\.messageExpression:.*undeclared reference to 'authorizer'`),
				regexp.MustCompile(`spec\.auditAnnotations\[0\]\.valueExpression:.*` + fieldSelectorErrorSubstring),
				regexp.MustCompile(`spec\.matchConditions\[0\]\.expression:.*` + fieldSelectorErrorSubstring),
				regexp.MustCompile(`spec\.variables\[0\]\.expression:.*` + fieldSelectorErrorSubstring),
			},
		},
		{
			name: "ValidatingWebhookConfiguration",
			createObject: func() error {
				obj := &admissionregistrationv1.ValidatingWebhookConfiguration{
					ObjectMeta: metav1.ObjectMeta{Name: "test"},
					Webhooks: []admissionregistrationv1.ValidatingWebhook{{
						Name:                    "test.example.com",
						ClientConfig:            admissionregistrationv1.WebhookClientConfig{URL: ptr.To("https://127.0.0.1")},
						AdmissionReviewVersions: []string{"v1"},
						SideEffects:             ptr.To(admissionregistrationv1.SideEffectClassNone),
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{"example.com"}, APIVersions: []string{"*"}, Resources: []string{"*"}}}},
						MatchConditions: []admissionregistrationv1.MatchCondition{{Name: "test", Expression: boolFieldSelectorExpression}}}}}
				_, err := c.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), obj, metav1.CreateOptions{})
				return err
			},
			expectErrorsWhenDisabled: []*regexp.Regexp{
				regexp.MustCompile(`webhooks\[0\]\.matchConditions\[0\]\.expression:.*` + fieldSelectorErrorSubstring),
			},
		},
		{
			name: "MutatingWebhookConfiguration",
			createObject: func() error {
				obj := &admissionregistrationv1.MutatingWebhookConfiguration{
					ObjectMeta: metav1.ObjectMeta{Name: "test"},
					Webhooks: []admissionregistrationv1.MutatingWebhook{{
						Name:                    "test.example.com",
						ClientConfig:            admissionregistrationv1.WebhookClientConfig{URL: ptr.To("https://127.0.0.1")},
						AdmissionReviewVersions: []string{"v1"},
						SideEffects:             ptr.To(admissionregistrationv1.SideEffectClassNone),
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{"example.com"}, APIVersions: []string{"*"}, Resources: []string{"*"}}}},
						MatchConditions: []admissionregistrationv1.MatchCondition{{Name: "test", Expression: boolFieldSelectorExpression}}}}}
				_, err := c.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), obj, metav1.CreateOptions{})
				return err
			},
			expectErrorsWhenDisabled: []*regexp.Regexp{
				regexp.MustCompile(`webhooks\[0\]\.matchConditions\[0\]\.expression:.*` + fieldSelectorErrorSubstring),
			},
		},
		{
			name: "ResourceClaim",
			createObject: func() error {
				obj := &resourceapi.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test"},
					Spec: resourceapi.ResourceClaimSpec{
						Devices: resourceapi.DeviceClaim{
							Requests: []resourceapi.DeviceRequest{{
								Name:            "req-0",
								DeviceClassName: "example-class",
								Selectors: []resourceapi.DeviceSelector{{
									CEL: &resourceapi.CELDeviceSelector{
										Expression: boolFieldSelectorExpression,
									},
								}},
							}},
						},
					},
				}
				_, err := c.ResourceV1beta1().ResourceClaims("default").Create(context.TODO(), obj, metav1.CreateOptions{})
				return err
			},
			// authorizer is not available to resource APIs
			expectErrorsWhenEnabled:  []*regexp.Regexp{regexp.MustCompile(`spec\.devices\.requests\[0\]\.selectors\[0\].cel\.expression:.*undeclared reference to 'authorizer'`)},
			expectErrorsWhenDisabled: []*regexp.Regexp{regexp.MustCompile(`spec\.devices\.requests\[0\]\.selectors\[0\].cel\.expression:.*undeclared reference to 'authorizer'`)},
		},
		{
			name: "CustomResourceDefinition - rule",
			createObject: func() error {
				obj := &apiextensionsv1.CustomResourceDefinition{
					ObjectMeta: metav1.ObjectMeta{Name: "crontabs.apis.example.com"},
					Spec: apiextensionsv1.CustomResourceDefinitionSpec{
						Group: "apis.example.com",
						Scope: apiextensionsv1.NamespaceScoped,
						Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "crontabs", Singular: "crontab", Kind: "CronTab", ListKind: "CronTabList"},
						Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
							Name:    "v1beta1",
							Served:  true,
							Storage: true,
							Schema: &apiextensionsv1.CustomResourceValidation{
								OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"spec": {
											Type:         "object",
											XValidations: apiextensionsv1.ValidationRules{{Rule: boolFieldSelectorExpression}}}}}}}}}}
				_, err := crdClient.ApiextensionsV1().CustomResourceDefinitions().Create(context.TODO(), obj, metav1.CreateOptions{})
				return err
			},
			// authorizer is not available to CRD validation
			expectErrorsWhenEnabled:  []*regexp.Regexp{regexp.MustCompile(`x-kubernetes-validations\[0\]\.rule:.*undeclared reference to 'authorizer'`)},
			expectErrorsWhenDisabled: []*regexp.Regexp{regexp.MustCompile(`x-kubernetes-validations\[0\]\.rule:.*undeclared reference to 'authorizer'`)},
		},
		{
			name: "CustomResourceDefinition - messageExpression",
			createObject: func() error {
				obj := &apiextensionsv1.CustomResourceDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name: "crontabs.apis.example.com"},
					Spec: apiextensionsv1.CustomResourceDefinitionSpec{
						Group: "apis.example.com",
						Scope: apiextensionsv1.NamespaceScoped,
						Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "crontabs", Singular: "crontab", Kind: "CronTab", ListKind: "CronTabList"},
						Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
							Name:    "v1beta1",
							Served:  true,
							Storage: true,
							Schema: &apiextensionsv1.CustomResourceValidation{
								OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"spec": {
											Type:         "object",
											XValidations: apiextensionsv1.ValidationRules{{Rule: `self == oldSelf`, MessageExpression: stringFieldSelectorExpression}}}}}}}}}}
				_, err := crdClient.ApiextensionsV1().CustomResourceDefinitions().Create(context.TODO(), obj, metav1.CreateOptions{})
				return err
			},
			// authorizer is not available to CRD validation
			expectErrorsWhenEnabled:  []*regexp.Regexp{regexp.MustCompile(`x-kubernetes-validations\[0\]\.messageExpression:.*undeclared reference to 'authorizer'`)},
			expectErrorsWhenDisabled: []*regexp.Regexp{regexp.MustCompile(`x-kubernetes-validations\[0\]\.messageExpression:.*undeclared reference to 'authorizer'`)},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.createObject()

			var expectedErrors []*regexp.Regexp
			if featureEnabled {
				expectedErrors = tc.expectErrorsWhenEnabled
			} else {
				expectedErrors = tc.expectErrorsWhenDisabled
			}

			switch {
			case len(expectedErrors) == 0 && err == nil:
				// success
			case len(expectedErrors) == 0 && err != nil:
				t.Fatalf("expected success, got error:\n%s", strings.Join(sets.List(getCauses(t, err)), "\n\n"))
			case len(expectedErrors) > 0 && err == nil:
				t.Fatalf("expected error, got success")
			case len(expectedErrors) > 0 && err != nil:
				// make sure errors match expectations
				actualCauses := getCauses(t, err)
				for _, expectCause := range expectedErrors {
					found := false
					for _, cause := range actualCauses.UnsortedList() {
						if expectCause.MatchString(cause) {
							actualCauses.Delete(cause)
							found = true
							break
						}
					}
					if !found {
						t.Errorf("missing error matching %s", expectCause)
					}
				}
				if len(actualCauses) > 0 {
					t.Errorf("unexpected errors:\n%s", strings.Join(sets.List(actualCauses), "\n\n"))
				}
			}
		})
	}
}

func getCauses(t *testing.T, err error) sets.Set[string] {
	t.Helper()
	status, ok := err.(apierrors.APIStatus)
	if !ok {
		t.Fatalf("expected API status error, got %#v", err)
	}
	if len(status.Status().Details.Causes) == 0 {
		t.Fatalf("expected API status error with causes, got %#v", err)
	}
	causes := sets.New[string]()
	for _, cause := range status.Status().Details.Causes {
		causes.Insert(cause.Field + ": " + cause.Message)
	}
	return causes
}
