/*
Copyright 2022 The Kubernetes Authors.

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

package cel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	authzmodes "k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
	"k8s.io/kubernetes/test/integration/authutil"
	"k8s.io/kubernetes/test/integration/etcd"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// Short term fix to refresh the policy source cache faster for tests
// until we reduce the polling interval by default.
const policyRefreshInterval = 10 * time.Millisecond

// Test_ValidateNamespace_NoParams_Success tests a ValidatingAdmissionPolicy that validates creation of a Namespace
// with no params via happy path.
func Test_ValidateNamespace_NoParams_Success(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name          string
		policy        *admissionregistrationv1.ValidatingAdmissionPolicy
		policyBinding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	}{
		{
			name: "namespace name contains suffix enforced by validating admission policy, using object metadata fields",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
		},
		{
			name: "namespace name contains suffix enforced by validating admission policy, using request field",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "request.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
		},
		{
			name: "namespace name does NOT contains suffix enforced by validating admission policy, using request field",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "request.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
		},
		{
			name: "runtime error when validating namespace, but failurePolicy=Ignore",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.nonExistentProperty == 'someval'",
				},
			}, withFailurePolicy(admissionregistrationv1.Ignore, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
		},
		{
			name: "with check against unguarded params using has() and default check",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "(has(params.metadata) && has(params.metadata.name) && object.metadata.name.startsWith(params.metadata.name)) || object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
		},
		{
			name: "with check against null params and default check",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "(params != null && object.metadata.name.startsWith(params.metadata.name)) || object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
		},
		{
			name: "with check against namespaceObject",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "namespaceObject == null", // because namespace itself is cluster-scoped.
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
		},
	}
	for i, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := createAndWaitReady(t, client, testcase.policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			nsName := fmt.Sprintf("test-%d-k8s", i)
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
				},
			}
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})

			checkExpectedError(t, err, "")
			checkFailureReason(t, err, "")
			if err := cleanupPolicy(t, client, policy, testcase.policyBinding); err != nil {
				t.Errorf("error while cleaning up policy and its bindings: %v", err)
			}
		})
	}
}

// Test_ValidateNamespace_NoParams_Failures tests a ValidatingAdmissionPolicy that fails creation of a Namespace with no params.
func Test_ValidateNamespace_NoParams_Failures(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenReason := metav1.StatusReasonForbidden

	testcases := []struct {
		name          string
		policy        *admissionregistrationv1.ValidatingAdmissionPolicy
		policyBinding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
		namespace     *v1.Namespace
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "namespace name does NOT contain suffix enforced by validating admission policyusing, object metadata fields",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-foobar",
				},
			},
			err:           "namespaces \"test-foobar\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: object.metadata.name.endsWith('k8s')",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "namespace name does NOT contain suffix enforced by validating admission policy using object metadata fields, AND validating expression returns StatusReasonForbidden",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
					Reason:     &forbiddenReason,
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "forbidden-test-foobar",
				},
			},
			err:           "namespaces \"forbidden-test-foobar\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: object.metadata.name.endsWith('k8s')",
			failureReason: metav1.StatusReasonForbidden,
		},
		{
			name: "runtime error when validating namespace, but failurePolicy=Fail",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.nonExistentProperty == 'someval'",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: expression 'object.nonExistentProperty == 'someval'' resulted in error: no such key: nonExistentProperty",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "runtime error due to unguarded params",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.metadata.name.startsWith(params.metadata.name)",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: expression 'object.metadata.name.startsWith(params.metadata.name)' resulted in error: no such key: metadata",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "with check against unguarded params using has()",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "has(params.metadata) && has(params.metadata.name) && object.metadata.name.endsWith(params.metadata.name)",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: has(params.metadata) && has(params.metadata.name) && object.metadata.name.endsWith(params.metadata.name)",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "with check against null params",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "(params != null && object.metadata.name.endsWith(params.metadata.name))",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: (params != null && object.metadata.name.endsWith(params.metadata.name))",
			failureReason: metav1.StatusReasonInvalid,
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := createAndWaitReady(t, client, testcase.policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			_, err = client.CoreV1().Namespaces().Create(context.TODO(), testcase.namespace, metav1.CreateOptions{})

			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
			if err := cleanupPolicy(t, client, policy, testcase.policyBinding); err != nil {
				t.Errorf("error while cleaning up policy and its bindings: %v", err)
			}
		})
	}
}

func Test_ValidateAnnotationsAndWarnings(t *testing.T) {
	testcases := []struct {
		name             string
		policy           *admissionregistrationv1.ValidatingAdmissionPolicy
		policyBinding    *admissionregistrationv1.ValidatingAdmissionPolicyBinding
		object           *v1.ConfigMap
		err              string
		failureReason    metav1.StatusReason
		auditAnnotations map[string]string
		warnings         sets.Set[string]
	}{
		{
			name: "with audit annotations",
			policy: withAuditAnnotations([]admissionregistrationv1.AuditAnnotation{
				{
					Key:             "example-key",
					ValueExpression: "'object name: ' + object.metadata.name",
				},
				{
					Key:             "exclude-key",
					ValueExpression: "null",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withConfigMapMatch(makePolicy("validate-audit-annotations"))))),
			policyBinding: makeBinding("validate-audit-annotations-binding", "validate-audit-annotations", ""),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test1-k8s",
				},
			},
			err: "",
			auditAnnotations: map[string]string{
				"validate-audit-annotations/example-key": `object name: test1-k8s`,
			},
		},
		{
			name: "with audit annotations with invalid expression",
			policy: withAuditAnnotations([]admissionregistrationv1.AuditAnnotation{
				{
					Key:             "example-key",
					ValueExpression: "string(params.metadata.name)", // runtime error, params is null
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withConfigMapMatch(makePolicy("validate-audit-annotations-invalid"))))),
			policyBinding: makeBinding("validate-audit-annotations-invalid-binding", "validate-audit-annotations-invalid", ""),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test2-k8s",
				},
			},
			err:           "configmaps \"test2-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-audit-annotations-invalid' with binding 'validate-audit-annotations-invalid-binding' denied request: expression 'string(params.metadata.name)' resulted in error: no such key: metadata",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "with audit annotations with invalid expression and ignore failure policy",
			policy: withAuditAnnotations([]admissionregistrationv1.AuditAnnotation{
				{
					Key:             "example-key",
					ValueExpression: "string(params.metadata.name)", // runtime error, params is null
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Ignore, withConfigMapMatch(makePolicy("validate-audit-annotations-invalid-ignore"))))),
			policyBinding: makeBinding("validate-audit-annotations-invalid-ignore-binding", "validate-audit-annotations-invalid-ignore", ""),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test3-k8s",
				},
			},
			err: "",
		},
		{
			name: "with warn validationActions",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withConfigMapMatch(makePolicy("validate-actions-warn"))))),
			policyBinding: withValidationActions([]admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn}, makeBinding("validate-actions-warn-binding", "validate-actions-warn", "")),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test4-nope",
				},
			},
			warnings: sets.New("Validation failed for ValidatingAdmissionPolicy 'validate-actions-warn' with binding 'validate-actions-warn-binding': failed expression: object.metadata.name.endsWith('k8s')"),
		},
		{
			name: "with audit validationActions",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1.Fail, withConfigMapMatch(makePolicy("validate-actions-audit"))))),
			policyBinding: withValidationActions([]admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny, admissionregistrationv1.Audit}, makeBinding("validate-actions-audit-binding", "validate-actions-audit", "")),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test5-nope",
				},
			},
			err:           "configmaps \"test5-nope\" is forbidden: ValidatingAdmissionPolicy 'validate-actions-audit' with binding 'validate-actions-audit-binding' denied request: failed expression: object.metadata.name.endsWith('k8s')",
			failureReason: metav1.StatusReasonInvalid,
			auditAnnotations: map[string]string{
				"validation.policy.admission.k8s.io/validation_failure": `[{"message":"failed expression: object.metadata.name.endsWith('k8s')","policy":"validate-actions-audit","binding":"validate-actions-audit-binding","expressionIndex":1,"validationActions":["Deny","Audit"]}]`,
			},
		},
	}

	// prepare audit policy file
	policyFile, err := os.CreateTemp("", "audit-policy.yaml")
	if err != nil {
		t.Fatalf("Failed to create audit policy file: %v", err)
	}
	defer os.Remove(policyFile.Name())
	if _, err := policyFile.Write([]byte(auditPolicy)); err != nil {
		t.Fatalf("Failed to write audit policy file: %v", err)
	}
	if err := policyFile.Close(); err != nil {
		t.Fatalf("Failed to close audit policy file: %v", err)
	}

	// prepare audit log file
	logFile, err := os.CreateTemp("", "audit.log")
	if err != nil {
		t.Fatalf("Failed to create audit log file: %v", err)
	}
	defer os.Remove(logFile.Name())

	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
		"--audit-policy-file", policyFile.Name(),
		"--audit-log-version", "audit.k8s.io/v1",
		"--audit-log-mode", "blocking",
		"--audit-log-path", logFile.Name(),
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	warnHandler := newWarningHandler()
	config.WarningHandler = warnHandler
	config.Impersonate.UserName = testReinvocationClientUsername
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	for i, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			testCaseID := strconv.Itoa(i)
			ns := "auditannotations-" + testCaseID
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}

			if err := createAndWaitReadyNamespacedWithWarnHandler(t, client, withMatchNamespace(testcase.policyBinding, ns), nil, ns, warnHandler); err != nil {
				t.Fatal(err)
			}
			warnHandler.reset()
			testcase.object.Namespace = ns
			_, err = client.CoreV1().ConfigMaps(ns).Create(context.TODO(), testcase.object, metav1.CreateOptions{})

			code := int32(201)
			if testcase.err != "" {
				code = 422
			}

			auditAnnotationFilter := func(key, val string) bool {
				_, ok := testcase.auditAnnotations[key]
				return ok
			}

			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
			checkExpectedWarnings(t, warnHandler, testcase.warnings)
			checkAuditEvents(t, logFile, expectedAuditEvents(testcase.auditAnnotations, ns, code), auditAnnotationFilter)
		})
	}
}

// Test_ValidateNamespace_WithConfigMapParams tests a ValidatingAdmissionPolicy that validates creation of a Namespace,
// using ConfigMap as a param reference.
func Test_ValidateNamespace_WithConfigMapParams(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", "validate-namespace-suffix-param")
	configMap := makeConfigParams("validate-namespace-suffix-param", map[string]string{
		"namespaceSuffix": "k8s",
	})
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), configMap, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	valPolicy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.endsWith(params.data.namespaceSuffix)",
		},
	}, withFailurePolicy(admissionregistrationv1.Fail, withParams(configParamKind(), withNamespaceMatch(makePolicy("validate-namespace-suffix")))))
	policy := withWaitReadyConstraintAndExpression(valPolicy)
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name          string
		namespace     *v1.Namespace
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "namespace name contains suffix enforced by validating admission policy",
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
		{
			name: "namespace name does NOT contain suffix enforced by validating admission policy",
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-foo",
				},
			},
			err:           "namespaces \"test-foo\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: object.metadata.name.endsWith(params.data.namespaceSuffix)",
			failureReason: metav1.StatusReasonInvalid,
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), testcase.namespace, metav1.CreateOptions{})
			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
		})
	}
}

func TestMultiplePolicyBindings(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	paramKind := &admissionregistrationv1.ParamKind{
		APIVersion: "v1",
		Kind:       "ConfigMap",
	}
	policy := withPolicyExistsLabels([]string{"paramIdent"}, withParams(paramKind, withPolicyMatch("secrets", withFailurePolicy(admissionregistrationv1.Fail, makePolicy("test-policy")))))
	policy.Spec.Validations = []admissionregistrationv1.Validation{
		{
			Expression: "params.data.autofail != 'true' && (params.data.conditional == 'false' || object.metadata.name.startsWith(params.data.check))",
		},
	}
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	autoFailParams := makeConfigParams("autofail-params", map[string]string{
		"autofail": "true",
	})
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), autoFailParams, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	autofailBinding := withBindingExistsLabels([]string{"autofail-binding-label"}, policy, makeBinding("autofail-binding", "test-policy", "autofail-params"))
	if err := createAndWaitReady(t, client, autofailBinding, map[string]string{"paramIdent": "true", "autofail-binding-label": "true"}); err != nil {
		t.Fatal(err)
	}

	autoPassParams := makeConfigParams("autopass-params", map[string]string{
		"autofail":    "false",
		"conditional": "false",
	})
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), autoPassParams, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	autopassBinding := withBindingExistsLabels([]string{"autopass-binding-label"}, policy, makeBinding("autopass-binding", "test-policy", "autopass-params"))
	if err := createAndWaitReady(t, client, autopassBinding, map[string]string{"paramIdent": "true", "autopass-binding-label": "true"}); err != nil {
		t.Fatal(err)
	}

	condpassParams := makeConfigParams("condpass-params", map[string]string{
		"autofail":    "false",
		"conditional": "true",
		"check":       "prefix-",
	})
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), condpassParams, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	condpassBinding := withBindingExistsLabels([]string{"condpass-binding-label"}, policy, makeBinding("condpass-binding", "test-policy", "condpass-params"))
	if err := createAndWaitReady(t, client, condpassBinding, map[string]string{"paramIdent": "true", "condpass-binding-label": "true"}); err != nil {
		t.Fatal(err)
	}

	autofailingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "autofailing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"autofail-binding-label": "true",
			},
		},
	}
	_, err = client.CoreV1().Secrets("default").Create(context.TODO(), autofailingSecret, metav1.CreateOptions{})
	if err == nil {
		t.Fatal("expected secret creation to fail due to autofail-binding")
	}
	checkForFailedRule(t, err)
	checkFailureReason(t, err, metav1.StatusReasonInvalid)

	autopassingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "autopassing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"autopass-binding-label": "true",
			},
		},
	}
	if _, err := client.CoreV1().Secrets("default").Create(context.TODO(), autopassingSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("expected secret creation to succeed, got: %s", err)
	}

	condpassingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prefix-condpassing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"condpass-binding-label": "true",
			},
		},
	}
	if _, err := client.CoreV1().Secrets("default").Create(context.TODO(), condpassingSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("expected secret creation to succeed, got: %s", err)
	}

	condfailingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "condfailing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"condpass-binding-label": "true",
			},
		},
	}
	_, err = client.CoreV1().Secrets("default").Create(context.TODO(), condfailingSecret, metav1.CreateOptions{})
	if err == nil {
		t.Fatal("expected secret creation to fail due to autofail-binding")
	}
	checkForFailedRule(t, err)
	checkFailureReason(t, err, metav1.StatusReasonInvalid)
}

// Test_PolicyExemption tests that ValidatingAdmissionPolicy and ValidatingAdmissionPolicyBinding resources
// are exempt from policy rules.
func Test_PolicyExemption(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := makePolicy("test-policy")
	policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"*",
						},
						APIVersions: []string{
							"*",
						},
						Resources: []string{
							"*",
						},
					},
				},
			},
		},
	}

	policy.Spec.Validations = []admissionregistrationv1.Validation{{
		Expression: "false",
		Message:    "marker denied; policy is ready",
	}}

	policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("test-policy-binding", "test-policy", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	// validate that operations to ValidatingAdmissionPolicy are exempt from an existing policy that catches all resources
	policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(context.TODO(), policy.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ignoreFailurePolicy := admissionregistrationv1.Ignore
	policy.Spec.FailurePolicy = &ignoreFailurePolicy
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(context.TODO(), policy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}

	policyBinding, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(context.TODO(), policyBinding.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that operations to ValidatingAdmissionPolicyBindings are exempt from an existing policy that catches all resources
	policyBindingCopy := policyBinding.DeepCopy()
	policyBindingCopy.Spec.PolicyName = "different-binding"
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(context.TODO(), policyBindingCopy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}
}

// Test_ValidatingAdmissionPolicy_UpdateParamKind validates the behavior of ValidatingAdmissionPolicy when
// only the ParamKind is updated. This test creates a policy where namespaces must have a prefix that matches
// the ParamKind set in the policy. Switching the ParamKind should result in only namespaces with prefixes matching
// the new ParamKind to be allowed. For example, when Paramkind is v1/ConfigMap, only namespaces prefixed with "configmap"
// is allowed and when ParamKind is updated to v1/Secret, only namespaces prefixed with "secret" is allowed, etc.
func Test_ValidatingAdmissionPolicy_UpdateParamKind(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	allowedPrefixesParamsConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "allowed-prefixes",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), allowedPrefixesParamsConfigMap, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	allowedPrefixesParamSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "allowed-prefixes",
		},
	}
	if _, err := client.CoreV1().Secrets("default").Create(context.TODO(), allowedPrefixesParamSecret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	paramKind := &admissionregistrationv1.ParamKind{
		APIVersion: "v1",
		Kind:       "ConfigMap",
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.kind.lowerAscii())",
			Message:    "wrong paramKind",
		},
	}, withParams(paramKind, withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	allowedPrefixesBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "allowed-prefixes")
	if err := createAndWaitReady(t, client, allowedPrefixesBinding, nil); err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "configmap-" are allowed
	// and namespaces starting with "secret-" are disallowed
	allowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "configmap-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	disallowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "secret-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
	if err == nil {
		t.Error("unexpected nil error")
	}
	if !strings.Contains(err.Error(), "wrong paramKind") {
		t.Errorf("unexpected error message: %v", err)
	}
	checkFailureReason(t, err, metav1.StatusReasonInvalid)

	// update the policy ParamKind to reference a Secret
	paramKind = &admissionregistrationv1.ParamKind{
		APIVersion: "v1",
		Kind:       "Secret",
	}
	policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(context.TODO(), policy.Name, metav1.GetOptions{})
	if err != nil {
		t.Error(err)
	}
	policy.Spec.ParamKind = paramKind
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(context.TODO(), policy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}

	// validate that namespaces starting with "secret-" are allowed
	// and namespaces starting with "configmap-" are disallowed
	// wait loop is required here since ConfigMaps were previousy allowed and we need to wait for the new policy
	// to be enforced
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace = &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "configmap-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong paramKind") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "secret-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

// Test_ValidatingAdmissionPolicy_UpdateParamRef validates the behavior of ValidatingAdmissionPolicy when
// only the ParamRef in the binding is updated. This test creates a policy where namespaces must have a prefix that matches
// the ParamRef set in the policy binding. The paramRef in the binding is then updated to a different object.
func Test_ValidatingAdmissionPolicy_UpdateParamRef(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	allowedPrefixesParamsConfigMap1 := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), allowedPrefixesParamsConfigMap1, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	allowedPrefixesParamsConfigMap2 := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), allowedPrefixesParamsConfigMap2, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.metadata.name)",
			Message:    "wrong paramRef",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test-1" are allowed
	// and namespaces starting with "test-2-" are disallowed
	allowedPrefixesBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "test-1")
	if err := createAndWaitReady(t, client, allowedPrefixesBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-2-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong paramRef") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-1-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	// Update the paramRef in the policy binding to use the test-2 ConfigMap
	policyBinding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(context.TODO(), allowedPrefixesBinding.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	denyAction := admissionregistrationv1.DenyAction
	policyBindingCopy := policyBinding.DeepCopy()
	policyBindingCopy.Spec.ParamRef = &admissionregistrationv1.ParamRef{
		Name:                    "test-2",
		Namespace:               "default",
		ParameterNotFoundAction: &denyAction,
	}
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(context.TODO(), policyBindingCopy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}

	// validate that namespaces starting with "test-2" are allowed
	// and namespaces starting with "test-1" are disallowed
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-1-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong paramRef") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-2-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

// Test_ValidatingAdmissionPolicy_UpdateParamResource validates behavior of a policy after updates to the param resource.
func Test_ValidatingAdmissionPolicy_UpdateParamResource(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	paramConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed-prefix",
			Namespace: "default",
		},
		Data: map[string]string{
			"prefix": "test-1",
		},
	}
	paramConfigMap, err = client.CoreV1().ConfigMaps(paramConfigMap.Namespace).Create(context.TODO(), paramConfigMap, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.data['prefix'])",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test-1" are allowed
	// and namespaces starting with "test-2-" are disallowed
	allowedPrefixesBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "allowed-prefix")
	if err := createAndWaitReady(t, client, allowedPrefixesBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-2-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-1-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	// Update the param resource to use "test-2" as the new allwoed prefix
	paramConfigMapCopy := paramConfigMap.DeepCopy()
	paramConfigMapCopy.Data = map[string]string{
		"prefix": "test-2",
	}
	_, err = client.CoreV1().ConfigMaps(paramConfigMapCopy.Namespace).Update(context.TODO(), paramConfigMapCopy, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test-2" are allowed
	// and namespaces starting with "test-1" are disallowed
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-1-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-2-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchByObjectSelector(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"foo": "bar",
		},
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "matched by object selector!",
		},
	}, withConfigMapMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("match-by-object-selector"))))
	policy = withObjectSelector(labelSelector, policy)
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-object-selector-binding", "match-by-object-selector", "")
	if err := createAndWaitReady(t, client, policyBinding, map[string]string{"foo": "bar"}); err != nil {
		t.Fatal(err)
	}

	matchedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "denied",
			Namespace: "default",
			Labels: map[string]string{
				"foo": "bar",
			},
		},
	}

	_, err = client.CoreV1().ConfigMaps(matchedConfigMap.Namespace).Create(context.TODO(), matchedConfigMap, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by object selector!") {
		t.Errorf("unexpected error: %v", err)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchByNamespaceSelector(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	// only configmaps in default will be allowed.
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "kubernetes.io/metadata.name",
				Operator: "NotIn",
				Values:   []string{"default"},
			},
		},
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "matched by namespace selector!",
		},
	}, withConfigMapMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("match-by-namespace-selector"))))
	policy = withNamespaceSelector(labelSelector, policy)
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-namespace-selector-binding", "match-by-namespace-selector", "")
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), policyBinding, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "not-default",
		},
	}
	if _, err := client.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		matchedConfigMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "denied-",
				Namespace:    "not-default",
			},
		}

		_, err := client.CoreV1().ConfigMaps(matchedConfigMap.Namespace).Create(context.TODO(), matchedConfigMap, metav1.CreateOptions{})
		// policy not enforced yet, try again
		if err == nil {
			return false, nil
		}

		if !strings.Contains(err.Error(), "matched by namespace selector!") {
			return false, err
		}

		return true, nil

	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchByResourceNames(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "matched by resource names!",
		},
	}, withConfigMapMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("match-by-resource-names"))))
	policy.Spec.MatchConstraints.ResourceRules[0].ResourceNames = []string{"matched-by-resource-name"}
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-resource-names-binding", "match-by-resource-names", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	matchedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matched-by-resource-name",
			Namespace: "default",
		},
	}

	_, err = client.CoreV1().ConfigMaps(matchedConfigMap.Namespace).Create(context.TODO(), matchedConfigMap, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by resource names!") {
		t.Errorf("unexpected error: %v", err)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-matched-by-resource-name",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchWithExcludeResources(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "not matched by exclude resources!",
		},
	}, withPolicyMatch("*", withFailurePolicy(admissionregistrationv1.Fail, makePolicy("match-by-resource-names"))))

	policy = withExcludePolicyMatch("configmaps", policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-resource-names-binding", "match-by-resource-names", "")
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), policyBinding, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		secret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-matched-by-exclude-resources",
				Namespace:    "default",
			},
		}

		_, err := client.CoreV1().Secrets(secret.Namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
		// policy not enforced yet, try again
		if err == nil {
			return false, nil
		}

		if !strings.Contains(err.Error(), "not matched by exclude resources!") {
			return false, err
		}

		return true, nil

	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matched-by-exclude-resources",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchWithMatchPolicyEquivalent(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(server.ClientConfig), false, versionedCustomResourceDefinition())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "matched by equivalent match policy!",
		},
	}, withFailurePolicy(admissionregistrationv1.Fail, makePolicy("match-by-match-policy-equivalent")))
	policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"awesome.bears.com",
						},
						APIVersions: []string{
							"v1",
						},
						Resources: []string{
							"pandas",
						},
					},
				},
			},
		},
	}
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-match-policy-equivalent-binding", "match-by-match-policy-equivalent", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	v1Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v1",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v1-bears",
			},
		},
	}

	v2Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v2",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v2-bears",
			},
		},
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	_, err = dynamicClient.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v1", Resource: "pandas"}).Create(context.TODO(), v1Resource, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by equivalent match policy!") {
		t.Errorf("v1 panadas did not match against policy, err: %v", err)
	}

	_, err = dynamicClient.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v2", Resource: "pandas"}).Create(context.TODO(), v2Resource, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by equivalent match policy!") {
		t.Errorf("v2 panadas did not match against policy, err: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchWithMatchPolicyExact(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(server.ClientConfig), false, versionedCustomResourceDefinition())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "matched by exact match policy!",
		},
	}, withFailurePolicy(admissionregistrationv1.Fail, makePolicy("match-by-match-policy-exact")))
	matchPolicyExact := admissionregistrationv1.Exact
	policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{
		MatchPolicy: &matchPolicyExact,
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"awesome.bears.com",
						},
						APIVersions: []string{
							"v1",
						},
						Resources: []string{
							"pandas",
						},
					},
				},
			},
		},
	}
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-match-policy-exact-binding", "match-by-match-policy-exact", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	v1Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v1",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v1-bears",
			},
		},
	}

	v2Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v2",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v2-bears",
			},
		},
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	_, err = dynamicClient.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v1", Resource: "pandas"}).Create(context.TODO(), v1Resource, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by exact match policy!") {
		t.Errorf("v1 panadas did not match against policy, err: %v", err)
	}

	// v2 panadas is allowed since policy specificed match policy Exact and only matched against v1
	_, err = dynamicClient.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v2", Resource: "pandas"}).Create(context.TODO(), v2Resource, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchExcludedResource(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "try to deny SelfSubjectReview",
		},
	}, withFailurePolicy(admissionregistrationv1.Fail, makePolicy("match-excluded-resources")))
	policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{
		MatchPolicy: ptr.To(admissionregistrationv1.Exact),
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"authentication.k8s.io",
						},
						APIVersions: []string{
							"v1",
						},
						Resources: []string{
							"selfsubjectreviews",
						},
					},
				},
			},
		},
	}
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.Background(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatalf("fail to create policy: %v", err)
	}

	policyBinding := makeBinding("match-by-match-policy-exact-binding", "match-excluded-resources", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatalf("fail to create and wait for binding: %v", err)
	}
	r, err := client.AuthenticationV1().SelfSubjectReviews().Create(context.Background(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("unexpected denied SelfSubjectReview: %v", err)
	}
	// confidence check the returned user info.
	if len(r.Status.UserInfo.UID) == 0 {
		t.Errorf("unexpected invalid user info: %v", r.Status.UserInfo)
	}
}

// Test_ValidatingAdmissionPolicy_PolicyDeletedThenRecreated validates that deleting a ValidatingAdmissionPolicy
// removes the policy from the apiserver admission chain and recreating it re-enables it.
func Test_ValidatingAdmissionPolicy_PolicyDeletedThenRecreated(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.startsWith('test')",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test" are allowed
	policyBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// delete the binding object and validate that policy is not enforced
	if err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(context.TODO(), "allowed-prefixes", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		allowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}
		_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return true, nil
		}

		// old policy is still enforced, try again
		if strings.Contains(err.Error(), "wrong prefix") {
			return false, nil
		}

		return false, err
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}
}

// Test_ValidatingAdmissionPolicy_BindingDeletedThenRecreated validates that deleting a ValidatingAdmissionPolicyBinding
// removes the policy from the apiserver admission chain and recreating it re-enables it.
func Test_ValidatingAdmissionPolicy_BindingDeletedThenRecreated(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.startsWith('test')",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test" are allowed
	policyBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// delete the binding object and validate that policy is not enforced
	if err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(context.TODO(), "allowed-prefixes-binding", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		allowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}
		_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return true, nil
		}

		// old policy is still enforced, try again
		if strings.Contains(err.Error(), "wrong prefix") {
			return false, nil
		}

		return false, err
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// recreate the policy binding and test that policy is enforced again
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), policyBinding, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}
}

// Test_ValidatingAdmissionPolicy_ParamResourceDeletedThenRecreated validates that deleting a param resource referenced
// by a binding renders the policy as invalid. Recreating the param resource re-enables the policy.
func Test_ValidatingAdmissionPolicy_ParamResourceDeletedThenRecreated(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	param := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), param, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.metadata.name)",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test" are allowed
	policyBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "test")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// delete param object and validate that policy is invalid
	if err := client.CoreV1().ConfigMaps("default").Delete(context.TODO(), "test", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		allowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
		// old policy is still enforced, try again
		if strings.Contains(err.Error(), "wrong prefix") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "failed to configure binding: no params found for policy binding with `Deny` parameterNotFoundAction") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// recreate the param resource and validate namespace is disallowed again
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), param, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		// cache not synced with new object yet, try again
		if strings.Contains(err.Error(), "failed to configure binding: no params found for policy binding with `Deny` parameterNotFoundAction") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}
}

// Test_CostLimitForValidation tests the cost limit set for a ValidatingAdmissionPolicy
// with StrictCostEnforcementForVAP feature enabled.
func Test_CostLimitForValidation(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.StrictCostEnforcementForVAP, true)
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name          string
		policy        *admissionregistrationv1.ValidatingAdmissionPolicy
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "With StrictCostEnforcementForVAP: Single expression exceeds per call cost limit for native library",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(x, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(y, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z2, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z3, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z4, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z5, int('1'.find('[0-9]*')) < 100)))))))",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			err:           "operation cancelled: actual cost limit exceeded",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "With StrictCostEnforcementForVAP: Expression exceeds per call cost limit for extended library",
			policy: withValidations([]admissionregistrationv1.Validation{
				{

					Expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed()",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			err:           "operation cancelled: actual cost limit exceeded",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "With StrictCostEnforcementForVAP: Expression exceeds per call cost limit for extended library in variables",
			policy: withVariables([]admissionregistrationv1.Variable{
				{
					Name:       "authzCheck",
					Expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed()",
				},
			}, withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "variables.authzCheck",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			err:           "operation cancelled: actual cost limit exceeded",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "With StrictCostEnforcementForVAP: Expression exceeds per call cost limit for extended library in matchConditions",
			policy: withMatchConditions([]admissionregistrationv1.MatchCondition{
				{
					Name:       "test",
					Expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed()",
				},
			}, withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "true",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			err:           "operation cancelled: actual cost limit exceeded",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name:          "With StrictCostEnforcementForVAP: Expression exceeds per policy cost limit for extended library",
			policy:        withValidations(generateValidationsWithAuthzCheck(29, "authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed()"), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			err:           "validation failed due to running out of cost budget, no further validation rules will be run",
			failureReason: metav1.StatusReasonInvalid,
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			policyBinding := makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", "")
			if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			}
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
			if err := cleanupPolicy(t, client, policy, policyBinding); err != nil {
				t.Fatalf("error while cleaning up policy and its bindings: %v", err)
			}
		})
	}
}

// Test_CostLimitForValidationWithFeatureDisabled tests the cost limit set for a ValidatingAdmissionPolicy
// with StrictCostEnforcementForVAP feature disabled.
func Test_CostLimitForValidationWithFeatureDisabled(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--emulated-version", "1.31",
		"--feature-gates", "StrictCostEnforcementForVAP=false",
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name          string
		policy        *admissionregistrationv1.ValidatingAdmissionPolicy
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "Without StrictCostEnforcementForVAP: Single expression exceeds per call cost limit for native library",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(x, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(y, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z2, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z3, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z4, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].all(z5, int('1'.find('[0-9]*')) < 100)))))))",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			err:           "operation cancelled: actual cost limit exceeded",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "Without StrictCostEnforcementForVAP: Expression does not exceed per call cost limit for extended library",
			policy: withValidations([]admissionregistrationv1.Validation{
				{

					Expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed()",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
		},
		{
			name: "Without StrictCostEnforcementForVAP: Expression does not exceed per call cost limit for extended library in variables",
			policy: withVariables([]admissionregistrationv1.Variable{
				{
					Name:       "authzCheck",
					Expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed() && authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed()",
				},
			}, withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "variables.authzCheck",
				},
			}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
		},
		{
			name:   "Without StrictCostEnforcementForVAP: Expression does not exceed per policy cost limit for extended library",
			policy: withValidations(generateValidationsWithAuthzCheck(29, "authorizer.group('apps').resource('deployments').subresource('status').namespace('test').name('backend').check('create').allowed()"), withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
		},
	}
	for i, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			policyBinding := makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", "")
			if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			nsName := fmt.Sprintf("test-%d-k8s", i)
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
				},
			}

			_, err = client.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
			if err := cleanupPolicy(t, client, policy, policyBinding); err != nil {
				t.Fatalf("error while cleaning up policy and its bindings: %v", err)
			}
		})
	}
}

// generate n validation rules with provided expression
func generateValidationsWithAuthzCheck(num int, exp string) []admissionregistrationv1.Validation {
	var validations = make([]admissionregistrationv1.Validation, num)
	for i := 0; i < num; i++ {
		validations[i].Expression = exp
	}
	return validations
}

// TestCRDParams tests that a CustomResource can be used as a param resource for a ValidatingAdmissionPolicy.
func TestCRDParams(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	crd := versionedCustomResourceDefinition()
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(server.ClientConfig), false, crd)
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  crd.Spec.Versions[0].Name,
		Resource: crd.Spec.Names.Plural,
	}
	crClient := dynamicClient.Resource(gvr)

	resource := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "awesome.bears.com/v1",
		"kind":       "Panda",
		"metadata": map[string]interface{}{
			"name": "config-obj",
		},
		"spec": map[string]interface{}{
			"nameCheck": "crd-test-k8s",
		},
	}}
	_, err = crClient.Create(context.TODO(), resource, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating %s: %s", gvr, err)
	}

	testcases := []struct {
		name          string
		policy        *admissionregistrationv1.ValidatingAdmissionPolicy
		namespace     *v1.Namespace
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "a rule that uses data from a CRD param resource does NOT pass",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "params.spec.nameCheck == object.metadata.name",
				},
			}, withNamespaceMatch(withParams(withCRDParamKind("Panda", "awesome.bears.com", "v1"), withFailurePolicy(admissionregistrationv1.Fail, makePolicy("test-policy"))))),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "incorrect-name",
				},
			},
			err:           `namespaces "incorrect-name" is forbidden: ValidatingAdmissionPolicy 'test-policy' with binding 'crd-policy-binding' denied request: failed expression: params.spec.nameCheck == object.metadata.name`,
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "a rule that uses data from a CRD param resource that does pass",
			policy: withValidations([]admissionregistrationv1.Validation{
				{
					Expression: "params.spec.nameCheck == object.metadata.name",
				},
			}, withNamespaceMatch(withParams(withCRDParamKind("Panda", "awesome.bears.com", "v1"), withFailurePolicy(admissionregistrationv1.Fail, makePolicy("test-policy"))))),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "crd-test-k8s",
				},
			},
			err: ``,
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			// remove default namespace since the CRD is cluster-scoped
			policyBinding := makeBinding("crd-policy-binding", "test-policy", "config-obj")
			policyBinding.Spec.ParamRef.Namespace = ""
			if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			_, err = client.CoreV1().Namespaces().Create(context.TODO(), testcase.namespace, metav1.CreateOptions{})

			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
			if err := cleanupPolicy(t, client, policy, policyBinding); err != nil {
				t.Fatalf("error while cleaning up policy and its bindings: %v", err)
			}

		})
	}
}

func TestBindingRemoval(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "false",
			Message:    "policy still in effect",
		},
	}, withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("test-policy"))))
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	binding := makeBinding("test-binding", "test-policy", "test-params")
	if err := createAndWaitReady(t, client, binding, nil); err != nil {
		t.Fatal(err)
	}
	// check that the policy is active
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		namespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "check-namespace",
			},
		}
		_, err = client.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "policy still in effect") {
				return true, nil
			} else {
				// unexpected error while attempting namespace creation
				return true, err
			}
		}
		return false, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}
	if err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(context.TODO(), "test-binding", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	// wait for binding to be deleted
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {

		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(context.TODO(), "test-binding", metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			} else {
				return true, err
			}
		}

		return false, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}

	// policy should be considered in an invalid state and namespace creation should be allowed
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		namespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-namespace",
			},
		}
		_, err = client.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{})
		if err != nil {
			t.Logf("namespace creation failed: %s", err)
			return false, nil
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("expected namespace creation to succeed but timed out waiting: %v", waitErr)
	}
}

// Test_ValidateSecondaryAuthorization tests a ValidatingAdmissionPolicy that performs secondary authorization checks
// for both users and service accounts.
func Test_ValidateSecondaryAuthorization(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	server, err := apiservertesting.StartTestServer(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
		"--authorization-mode=RBAC",
		"--anonymous-auth",
	}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	// For test set up such as creating policies, bindings and RBAC rules.
	adminClient := clientset.NewForConfigOrDie(server.ClientConfig)
	testcases := []struct {
		name             string
		rbac             *rbacv1.PolicyRule
		expression       string
		allowed          bool
		extraAccountFn   func(t *testing.T, adminClient *clientset.Clientset, clientConfig *rest.Config, rules []rbacv1.PolicyRule) *clientset.Clientset
		extraAccountRbac *rbacv1.PolicyRule
	}{
		{
			name: "principal is allowed to create a specific deployment",
			rbac: &rbacv1.PolicyRule{
				Verbs:         []string{"create"},
				APIGroups:     []string{"apps"},
				Resources:     []string{"deployments/status"},
				ResourceNames: []string{"charmander"},
			},
			expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('default').namespace('default').name('charmander').check('create').allowed()",
			allowed:    true,
		},
		{
			name:       "principal is not allowed to create a specific deployment",
			expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('default').name('charmander').check('create').allowed()",
			allowed:    false,
		},
		{
			name: "principal is authorized for custom verb on current resource",
			rbac: &rbacv1.PolicyRule{
				Verbs:     []string{"anthropomorphize"},
				APIGroups: []string{""},
				Resources: []string{"namespaces"},
			},
			expression: "authorizer.requestResource.check('anthropomorphize').allowed()",
			allowed:    true,
		},
		{
			name:       "principal is not authorized for custom verb on current resource",
			expression: "authorizer.requestResource.check('anthropomorphize').allowed()",
			allowed:    false,
		},
		{
			name:           "serviceaccount is authorized for custom verb on current resource",
			extraAccountFn: authutil.ServiceAccountClient("default", "extra-acct"),
			extraAccountRbac: &rbacv1.PolicyRule{
				Verbs:     []string{"anthropomorphize"},
				APIGroups: []string{""},
				Resources: []string{"pods"},
			},
			expression: "authorizer.serviceAccount('default', 'extra-acct').group('').resource('pods').check('anthropomorphize').allowed()",
			allowed:    true,
		},
	}

	for i, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			clients := map[string]func(t *testing.T, adminClient *clientset.Clientset, clientConfig *rest.Config, rules []rbacv1.PolicyRule) *clientset.Clientset{
				"user":           secondaryAuthorizationUserClient,
				"serviceaccount": secondaryAuthorizationServiceAccountClient,
			}

			for clientName, clientFn := range clients {
				t.Run(clientName, func(t *testing.T) {
					// Principal is always allowed to create and update namespaces so that the admission requests to test
					// authorization expressions can be sent by the principal.
					rules := []rbacv1.PolicyRule{{
						Verbs:     []string{"create", "update"},
						APIGroups: []string{""},
						Resources: []string{"namespaces"},
					}}
					if testcase.rbac != nil {
						rules = append(rules, *testcase.rbac)
					}

					client := clientFn(t, adminClient, server.ClientConfig, rules)

					if testcase.extraAccountFn != nil {
						var extraRules []rbacv1.PolicyRule
						if testcase.extraAccountRbac != nil {
							extraRules = append(rules, *testcase.extraAccountRbac)
						}
						testcase.extraAccountFn(t, adminClient, server.ClientConfig, extraRules)
					}

					policyName := fmt.Sprintf("%s-%s-%d", "validate-authz", clientName, i)
					policy := withWaitReadyConstraintAndExpression(withValidations([]admissionregistrationv1.Validation{
						{
							Expression: testcase.expression,
						},
					}, withFailurePolicy(admissionregistrationv1.Fail, withNamespaceMatch(makePolicy(policyName)))))
					if _, err := adminClient.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
					policyBindingName := fmt.Sprintf("%s-%s", policyName, "binding")
					policyBinding := makeBinding(policyBindingName, policyName, "")
					if err := createAndWaitReady(t, adminClient, policyBinding, nil); err != nil {
						t.Fatal(err)
					}

					ns := &v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: fmt.Sprintf("%s-%s-%d", "test-authz", clientName, i),
						},
					}
					_, err = client.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})

					var expected metav1.StatusReason = ""
					if !testcase.allowed {
						expected = metav1.StatusReasonInvalid
					}
					checkFailureReason(t, err, expected)
					if err := cleanupPolicy(t, adminClient, policy, policyBinding); err != nil {
						t.Fatalf("error while cleaning up policy and its bindings: %v", err)
					}
				})
			}
		})
	}
}

func TestCRDsOnStartup(t *testing.T) {

	testContext, testCancel := context.WithCancel(context.Background())
	defer testCancel()

	// Start server and create CRD, and validatingadmission policy and binding
	etcdConfig := framework.SharedEtcd()
	server := apiservertesting.StartTestServerOrDie(t, nil, []string{
		"--enable-admission-plugins", "ValidatingAdmissionPolicy",
		"--authorization-mode=RBAC",
		"--anonymous-auth",
	}, etcdConfig)
	client := clientset.NewForConfigOrDie(server.ClientConfig)
	dynamicClient := dynamic.NewForConfigOrDie(server.ClientConfig)
	apiextclient := apiextensionsclientset.NewForConfigOrDie(server.ClientConfig)
	myCRD := &apiextensionsv1.CustomResourceDefinition{
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

	// Create a bunch of fake CRDs to make the initial startup sync take a long time
	for i := 0; i < 100; i++ {
		crd := myCRD.DeepCopy()
		crd.Name = fmt.Sprintf("foos%d.cr.bar.com", i)
		crd.Spec.Names.Plural = fmt.Sprintf("foos%d", i)
		crd.Spec.Names.Kind = fmt.Sprintf("Foo%d", i)

		if _, err := apiextclient.ApiextensionsV1().CustomResourceDefinitions().Create(context.Background(), crd, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	etcd.CreateTestCRDs(t, apiextclient, false, myCRD)
	crdGVK := schema.GroupVersionKind{
		Group:   "cr.bar.com",
		Version: "v1",
		Kind:    "Foo",
	}
	crdGVR := crdGVK.GroupVersion().WithResource("foos")

	param := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":      "test",
				"namespace": "default",
			},
			"foo": "bar",
		},
	}
	param.GetObjectKind().SetGroupVersionKind(crdGVK)

	if _, err := dynamicClient.Resource(crdGVR).Namespace("default").Create(context.TODO(), param, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.metadata.name)",
			Message:    "wrong prefix",
		},
	}, withParams(withCRDParamKind(crdGVK.Kind, crdGVK.Group, crdGVK.Version), withNamespaceMatch(withFailurePolicy(admissionregistrationv1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test" are allowed
	policyBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "test")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	doCheck := func(client clientset.Interface) {
		if waitErr := wait.PollUntilContextTimeout(testContext, time.Millisecond*100, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
			disallowedNamespace := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "not-test-",
				},
			}

			_, err = client.CoreV1().Namespaces().Create(testContext, disallowedNamespace, metav1.CreateOptions{})
			if err == nil {
				return false, nil
			}

			if strings.Contains(err.Error(), "not yet synced to use for admission") {
				return false, nil
			}

			if strings.Contains(err.Error(), "failed to find resource referenced by paramKind") {
				return false, nil
			}

			if !strings.Contains(err.Error(), "wrong prefix") {
				return false, err
			}

			return true, nil
		}); waitErr != nil {
			t.Errorf("timed out waiting: %v", err)
		}
	}

	// Show that the policy & binding are correctly working before restarting
	// to use the paramKind and deliver an error
	doCheck(client)
	server.TearDownFn()

	// Start the server.
	server = apiservertesting.StartTestServerOrDie(
		t,
		&apiservertesting.TestServerInstanceOptions{},
		[]string{
			"--enable-admission-plugins", "ValidatingAdmissionPolicy",
			"--authorization-mode=RBAC",
			"--anonymous-auth",
		},
		etcdConfig)
	defer server.TearDownFn()

	// Now that the server is restarted, show again that the policy & binding are correctly working
	client = clientset.NewForConfigOrDie(server.ClientConfig)

	doCheck(client)

}

func TestAuthorizationDecisionCaching(t *testing.T) {
	resetPolicyRefreshInterval := generic.SetPolicyRefreshIntervalForTests(policyRefreshInterval)
	defer resetPolicyRefreshInterval()
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	var nChecks int
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review authorizationv1.SubjectAccessReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}

		review.Status.Allowed = true
		if review.Spec.ResourceAttributes.Verb == "test" {
			nChecks++
			review.Status.Reason = fmt.Sprintf("%d", nChecks)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(review); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer webhook.Close()
	kcfd, err := os.CreateTemp("", "kubeconfig-")
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if err := kcfd.Close(); err != nil {
				t.Fatal(err)
			}
		}()
		tmpl, err := template.New("kubeconfig").Parse(`
apiVersion: v1
kind: Config
clusters:
  - name: test-authz-service
    cluster:
      server: {{ .Server }}
users:
  - name: test-api-server
current-context: webhook
contexts:
- context:
    cluster: test-authz-service
    user: test-api-server
  name: webhook
`)
		if err != nil {
			t.Fatal(err)
		}
		err = tmpl.Execute(kcfd, struct {
			Server string
		}{
			Server: webhook.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
	}()
	_, config, teardown := framework.StartTestServer(ctx, t, framework.TestServerSetup{
		ModifyServerRunOptions: func(options *options.ServerRunOptions) {
			options.Admission.GenericAdmission.EnablePlugins = append(options.Admission.GenericAdmission.EnablePlugins, "ValidatingAdmissionPolicy")
			if err = options.APIEnablement.RuntimeConfig.Set("api/all=true"); err != nil {
				t.Fatal(err)
			}

			options.Authorization.Modes = []string{authzmodes.ModeWebhook}
			options.Authorization.WebhookConfigFile = kcfd.Name()
			options.Authorization.WebhookVersion = "v1"
			// Bypass webhook cache to observe the policy plugin's cache behavior.
			options.Authorization.WebhookCacheAuthorizedTTL = 0
			options.Authorization.WebhookCacheUnauthorizedTTL = 0
		},
	})
	defer teardown()

	config = rest.CopyConfig(config)
	config.Impersonate = rest.ImpersonationConfig{
		UserName: "alice",
		UID:      "1234",
	}
	client, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	for i, tc := range []struct {
		name        string
		validations []admissionregistrationv1.Validation
	}{
		{
			name: "hit",
			validations: []admissionregistrationv1.Validation{
				{
					Expression: "authorizer.requestResource.check('test').reason() == authorizer.requestResource.check('test').reason()",
				},
			},
		},
		{
			name: "miss",
			validations: []admissionregistrationv1.Validation{
				{
					Expression: "authorizer.requestResource.subresource('a').check('test').reason() == '1'",
				},
				{
					Expression: "authorizer.requestResource.subresource('b').check('test').reason() == '2'",
				},
				{
					Expression: "authorizer.requestResource.subresource('c').check('test').reason() == '3'",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-authorization-decision-caching-policy",
				},
				Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
					MatchConstraints: &admissionregistrationv1.MatchResources{
						ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
							{
								ResourceNames: []string{"test-authorization-decision-caching-namespace"},
								RuleWithOperations: admissionregistrationv1.RuleWithOperations{
									Operations: []admissionregistrationv1.OperationType{
										admissionregistrationv1.Create,
									},
									Rule: admissionregistrationv1.Rule{
										APIGroups:   []string{""},
										APIVersions: []string{"v1"},
										Resources:   []string{"namespaces"},
									},
								},
							},
						},
					},
					Validations: tc.validations,
				},
			}

			policy, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, withWaitReadyConstraintAndExpression(policy), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			policyBinding := makeBinding(policy.Name+"-binding", policy.Name, "")
			if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			if _, err := client.CoreV1().Namespaces().Create(
				ctx,
				&v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("test-authorization-decision-caching-namespace-%d", i),
					},
				},
				metav1.CreateOptions{},
			); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					t.Fatal(err)
				}
			}

			if err := cleanupPolicy(t, client, policy, policyBinding); err != nil {
				t.Errorf("error while cleaning up policy and its bindings: %v", err)
			}
		})
	}
}

func secondaryAuthorizationUserClient(t *testing.T, adminClient *clientset.Clientset, clientConfig *rest.Config, rules []rbacv1.PolicyRule) *clientset.Clientset {
	clientConfig = rest.CopyConfig(clientConfig)
	clientConfig.Impersonate = rest.ImpersonationConfig{
		UserName: "alice",
		UID:      "1234",
	}
	client := clientset.NewForConfigOrDie(clientConfig)

	for _, rule := range rules {
		authutil.GrantUserAuthorization(t, context.TODO(), adminClient, "alice", rule)
	}
	return client
}

func secondaryAuthorizationServiceAccountClient(t *testing.T, adminClient *clientset.Clientset, clientConfig *rest.Config, rules []rbacv1.PolicyRule) *clientset.Clientset {
	return authutil.ServiceAccountClient("default", "test-service-acct")(t, adminClient, clientConfig, rules)
}

func withWaitReadyConstraintAndExpression(policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy = policy.DeepCopy()
	policy.Spec.MatchConstraints.ResourceRules = append(policy.Spec.MatchConstraints.ResourceRules, admissionregistrationv1.NamedRuleWithOperations{
		ResourceNames: []string{"test-marker"},
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{
				"UPDATE",
			},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{
					"",
				},
				APIVersions: []string{
					"v1",
				},
				Resources: []string{
					"endpoints",
				},
			},
		},
	})
	policy.Spec.Validations = append([]admissionregistrationv1.Validation{{
		Expression: "object.metadata.name != 'test-marker'",
		Message:    "marker denied; policy is ready",
	}}, policy.Spec.Validations...)
	return policy
}

func createAndWaitReady(t *testing.T, client clientset.Interface, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, matchLabels map[string]string) error {
	return createAndWaitReadyNamespaced(t, client, binding, matchLabels, "default")
}

func createAndWaitReadyNamespaced(t *testing.T, client clientset.Interface, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, matchLabels map[string]string, ns string) error {
	return createAndWaitReadyNamespacedWithWarnHandler(t, client, binding, matchLabels, ns, newWarningHandler())
}

func createAndWaitReadyNamespacedWithWarnHandler(t *testing.T, client clientset.Interface, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, matchLabels map[string]string, ns string, handler *warningHandler) error {
	marker := &v1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "test-marker", Namespace: ns, Labels: matchLabels}}
	defer func() {
		err := client.CoreV1().Endpoints(ns).Delete(context.TODO(), marker.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Logf("error deleting marker: %v", err)
		}
	}()
	marker, err := client.CoreV1().Endpoints(ns).Create(context.TODO(), marker, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), binding, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	if waitErr := wait.PollImmediate(time.Millisecond*5, wait.ForeverTestTimeout, func() (bool, error) {
		handler.reset()
		_, err := client.CoreV1().Endpoints(ns).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
		if handler.hasObservedMarker() {
			return true, nil
		}
		if err != nil && strings.Contains(err.Error(), "marker denied; policy is ready") {
			return true, nil
		} else if err != nil && strings.Contains(err.Error(), "not yet synced to use for admission") {
			t.Logf("waiting for policy to be ready. Marker: %v. Admission not synced yet: %v", marker, err)
			return false, nil
		} else if err != nil && strings.Contains(err.Error(), "validation failed due to running out of cost budget, no further validation rules will be run") {
			t.Logf("policy is ready and exceeds the budget: %v", err)
			return true, nil
		} else if err != nil && strings.Contains(err.Error(), "operation cancelled: actual cost limit exceeded") {
			t.Logf("policy is ready and exceeds the budget: %v", err)
			return true, nil
		} else {
			t.Logf("waiting for policy to be ready. Marker: %v, Last marker patch response: %v", marker, err)
			return false, err
		}
	}); waitErr != nil {
		return waitErr
	}
	t.Logf("Marker ready: %v", marker)
	handler.reset()
	return nil
}

func cleanupPolicy(t *testing.T, client clientset.Interface, policy *admissionregistrationv1.ValidatingAdmissionPolicy, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(context.TODO(), binding.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollUntilContextTimeout(context.TODO(), time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(context.TODO(), binding.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); waitErr != nil {
		t.Fatalf("timed out waiting for policy binding to be cleaned up: %v", err)
	}

	if err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(context.TODO(), policy.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollUntilContextTimeout(context.TODO(), time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(context.TODO(), policy.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	}); waitErr != nil {
		t.Fatalf("timed out waiting for policy to be cleaned up: %v", err)
	}

	return nil
}

func withMatchNamespace(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, ns string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	binding.Spec.MatchResources = &admissionregistrationv1.MatchResources{
		NamespaceSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{ns},
				},
			},
		},
	}
	return binding
}

func makePolicy(name string) *admissionregistrationv1.ValidatingAdmissionPolicy {
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func withParams(params *admissionregistrationv1.ParamKind, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.ParamKind = params
	return policy
}

func configParamKind() *admissionregistrationv1.ParamKind {
	return &admissionregistrationv1.ParamKind{
		APIVersion: "v1",
		Kind:       "ConfigMap",
	}
}

func withFailurePolicy(failure admissionregistrationv1.FailurePolicyType, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.FailurePolicy = &failure
	return policy
}

func withNamespaceMatch(policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	return withPolicyMatch("namespaces", policy)
}

func withConfigMapMatch(policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	return withPolicyMatch("configmaps", policy)
}

func withObjectSelector(labelSelector *metav1.LabelSelector, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints.ObjectSelector = labelSelector
	return policy
}

func withNamespaceSelector(labelSelector *metav1.LabelSelector, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints.NamespaceSelector = labelSelector
	return policy
}

func withPolicyMatch(resource string, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"",
						},
						APIVersions: []string{
							"*",
						},
						Resources: []string{
							resource,
						},
					},
				},
			},
		},
	}
	return policy
}

func withExcludePolicyMatch(resource string, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints.ExcludeResourceRules = []admissionregistrationv1.NamedRuleWithOperations{
		{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{
					"*",
				},
				Rule: admissionregistrationv1.Rule{
					APIGroups: []string{
						"",
					},
					APIVersions: []string{
						"*",
					},
					Resources: []string{
						resource,
					},
				},
			},
		},
	}
	return policy
}

func withPolicyExistsLabels(labels []string, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	if policy.Spec.MatchConstraints == nil {
		policy.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{}
	}
	matchExprs := buildExistsSelector(labels)
	policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{
		MatchExpressions: matchExprs,
	}
	return policy
}

func withValidations(validations []admissionregistrationv1.Validation, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.Validations = validations
	return policy
}

func withVariables(variables []admissionregistrationv1.Variable, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.Variables = variables
	return policy
}

func withMatchConditions(matchConditions []admissionregistrationv1.MatchCondition, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConditions = matchConditions
	return policy
}

func withAuditAnnotations(auditAnnotations []admissionregistrationv1.AuditAnnotation, policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	policy.Spec.AuditAnnotations = auditAnnotations
	return policy
}

func makeBinding(name, policyName, paramName string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	var paramRef *admissionregistrationv1.ParamRef
	if paramName != "" {
		denyAction := admissionregistrationv1.DenyAction
		paramRef = &admissionregistrationv1.ParamRef{
			Name:                    paramName,
			Namespace:               "default",
			ParameterNotFoundAction: &denyAction,
		}
	}
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        policyName,
			ParamRef:          paramRef,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func withValidationActions(validationActions []admissionregistrationv1.ValidationAction, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	binding.Spec.ValidationActions = validationActions
	return binding
}

func withBindingExistsLabels(labels []string, policy *admissionregistrationv1.ValidatingAdmissionPolicy, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	if policy != nil {
		// shallow copy
		constraintsCopy := *policy.Spec.MatchConstraints
		binding.Spec.MatchResources = &constraintsCopy
	}
	matchExprs := buildExistsSelector(labels)
	binding.Spec.MatchResources.ObjectSelector = &metav1.LabelSelector{
		MatchExpressions: matchExprs,
	}
	return binding
}

func buildExistsSelector(labels []string) []metav1.LabelSelectorRequirement {
	matchExprs := make([]metav1.LabelSelectorRequirement, len(labels))
	for i := 0; i < len(labels); i++ {
		matchExprs[i].Key = labels[i]
		matchExprs[i].Operator = metav1.LabelSelectorOpExists
	}
	return matchExprs
}

func makeConfigParams(name string, data map[string]string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Data:       data,
	}
}

func checkForFailedRule(t *testing.T, err error) {
	if !strings.Contains(err.Error(), "failed expression") {
		t.Fatalf("unexpected error (expected to find \"failed expression\"): %s", err)
	}
	if strings.Contains(err.Error(), "evaluation error") {
		t.Fatalf("CEL rule evaluation failed: %s", err)
	}
}

func checkFailureReason(t *testing.T, err error, expectedReason metav1.StatusReason) {
	if err == nil && expectedReason == "" {
		// no reason was given, no error was passed - early exit
		return
	}
	switch e := err.(type) {
	case apierrors.APIStatus:
		reason := e.Status().Reason
		if reason != expectedReason {
			t.Logf("actual error reason: %v", reason)
			t.Logf("expected failure reason: %v", expectedReason)
			t.Error("Unexpected error reason")
		}
	default:
		t.Errorf("Unexpected error: %v", err)
	}
}

func checkExpectedWarnings(t *testing.T, recordedWarnings *warningHandler, expectedWarnings sets.Set[string]) {
	if !recordedWarnings.equals(expectedWarnings) {
		t.Errorf("Expected warnings '%v' but got '%v", expectedWarnings, recordedWarnings)
	}
}

func checkAuditEvents(t *testing.T, logFile *os.File, auditEvents []utils.AuditEvent, filter utils.AuditAnnotationsFilter) {
	stream, err := os.OpenFile(logFile.Name(), os.O_RDWR, 0600)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	defer stream.Close()

	if auditEvents != nil {
		missing, err := utils.CheckAuditLinesFiltered(stream, auditEvents, auditv1.SchemeGroupVersion, filter)
		if err != nil {
			t.Errorf("unexpected error checking audit lines: %v", err)
		}
		if len(missing.MissingEvents) > 0 {
			t.Errorf("failed to get expected events -- missing: %s", missing)
		}
	}
	if err := stream.Truncate(0); err != nil {
		t.Errorf("unexpected error truncate file: %v", err)
	}
	if _, err := stream.Seek(0, 0); err != nil {
		t.Errorf("unexpected error reset offset: %v", err)
	}
}

func withCRDParamKind(kind, crdGroup, crdVersion string) *admissionregistrationv1.ParamKind {
	return &admissionregistrationv1.ParamKind{
		APIVersion: crdGroup + "/" + crdVersion,
		Kind:       kind,
	}
}

func checkExpectedError(t *testing.T, err error, expectedErr string) {
	if err == nil && expectedErr == "" {
		return
	}
	if err == nil && expectedErr != "" {
		t.Logf("actual error: %v", err)
		t.Logf("expected error: %v", expectedErr)
		t.Fatal("got nil error but expected an error")
	}

	if err != nil && expectedErr == "" {
		t.Logf("actual error: %v", err)
		t.Logf("expected error: %v", expectedErr)
		t.Fatal("got error but expected none")
	}

	if !strings.Contains(err.Error(), expectedErr) {
		t.Logf("actual validation error: %v", err)
		t.Logf("expected validation error: %v", expectedErr)
		t.Error("unexpected validation error")
	}
}

// Copied from etcd.GetCustomResourceDefinitionData
func versionedCustomResourceDefinition() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pandas.awesome.bears.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "awesome.bears.com",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema:  fixtures.AllowAllSchema(),
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
						Scale: &apiextensionsv1.CustomResourceSubresourceScale{
							SpecReplicasPath:   ".spec.replicas",
							StatusReplicasPath: ".status.replicas",
							LabelSelectorPath:  func() *string { path := ".status.selector"; return &path }(),
						},
					},
				},
				{
					Name:    "v2",
					Served:  true,
					Storage: false,
					Schema:  fixtures.AllowAllSchema(),
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
						Scale: &apiextensionsv1.CustomResourceSubresourceScale{
							SpecReplicasPath:   ".spec.replicas",
							StatusReplicasPath: ".status.replicas",
							LabelSelectorPath:  func() *string { path := ".status.selector"; return &path }(),
						},
					},
				},
			},
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "pandas",
				Kind:   "Panda",
			},
		},
	}
}

type warningHandler struct {
	lock           sync.Mutex
	warnings       sets.Set[string]
	observedMarker bool
}

func newWarningHandler() *warningHandler {
	return &warningHandler{warnings: sets.New[string]()}
}

func (w *warningHandler) reset() {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.warnings = sets.New[string]()
	w.observedMarker = false
}

func (w *warningHandler) equals(s sets.Set[string]) bool {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.warnings.Equal(s)
}

func (w *warningHandler) hasObservedMarker() bool {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.observedMarker
}

func (w *warningHandler) HandleWarningHeader(code int, _ string, message string) {
	if strings.HasSuffix(message, "marker denied; policy is ready") {
		func() {
			w.lock.Lock()
			defer w.lock.Unlock()
			w.observedMarker = true
		}()
	}
	if code != 299 || len(message) == 0 {
		return
	}
	w.lock.Lock()
	defer w.lock.Unlock()
	w.warnings.Insert(message)
}

func expectedAuditEvents(auditAnnotations map[string]string, ns string, code int32) []utils.AuditEvent {
	return []utils.AuditEvent{
		{
			Level:                  auditinternal.LevelRequest,
			Stage:                  auditinternal.StageResponseComplete,
			RequestURI:             fmt.Sprintf("/api/v1/namespaces/%s/configmaps", ns),
			Verb:                   "create",
			Code:                   code,
			User:                   "system:apiserver",
			ImpersonatedUser:       testReinvocationClientUsername,
			ImpersonatedGroups:     "system:authenticated",
			Resource:               "configmaps",
			Namespace:              ns,
			AuthorizeDecision:      "allow",
			RequestObject:          true,
			ResponseObject:         false,
			CustomAuditAnnotations: auditAnnotations,
		},
	}
}

const (
	testReinvocationClientUsername = "webhook-reinvocation-integration-client"
	auditPolicy                    = `
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Request
    resources:
      - group: "" # core
        resources: ["configmaps"]
`
)
