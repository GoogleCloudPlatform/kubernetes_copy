/*
Copyright 2018 The Kubernetes Authors.

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

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/mutating"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	clientset "k8s.io/client-go/kubernetes"
	utiltesting "k8s.io/client-go/util/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

const (
	testWebhookConfigurationName = "auditmutation.integration.test"
	testWebhookName              = "auditmutation.integration.test"
)

var (
	auditPolicyPattern = `
apiVersion: {version}
kind: Policy
rules:
  - level: RequestResponse
    namespaces: ["no-webhook-namespace"]
    resources:
      - group: "" # core
        resources: ["configmaps"]
  - level: Metadata
    namespaces: ["webhook-audit-metadata"]
    resources:
      - group: "" # core
        resources: ["configmaps"]
  - level: Request
    namespaces: ["webhook-audit-request"]
    resources:
      - group: "" # core
        resources: ["configmaps"]
  - level: RequestResponse
    namespaces: ["webhook-audit-response"]
    resources:
      - group: "" # core
        resources: ["configmaps"]
  - level: Request
    namespaces: ["create-audit-request"]
    resources:
      - group: "" # core
        resources: ["serviceaccounts/token"]
  - level: RequestResponse
    namespaces: ["create-audit-response"]
    resources:
      - group: "" # core
        resources: ["serviceaccounts/token"]
  - level: Request
    namespaces: ["update-audit-request"]
    resources:
      - group: "apps"
        resources: ["deployments/scale"]
  - level: RequestResponse
    namespaces: ["update-audit-response"]
    resources:
      - group: "apps"
        resources: ["deployments/scale"]

`
	nonAdmissionWebhookNamespace       = "no-webhook-namespace"
	watchTestTimeout             int64 = 1
	watchOptions                       = metav1.ListOptions{TimeoutSeconds: &watchTestTimeout}
	patch, _                           = json.Marshal(jsonpatch.Patch{})
	auditTestUser                      = "system:apiserver"
	versions                           = map[string]schema.GroupVersion{
		"audit.k8s.io/v1": auditv1.SchemeGroupVersion,
	}

	expectedEvents = []utils.AuditEvent{
		{
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseComplete,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps", nonAdmissionWebhookNamespace),
			Verb:              "create",
			Code:              201,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     true,
			ResponseObject:    true,
			AuthorizeDecision: "allow",
		}, {
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseComplete,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps/audit-configmap", nonAdmissionWebhookNamespace),
			Verb:              "get",
			Code:              200,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     false,
			ResponseObject:    true,
			AuthorizeDecision: "allow",
		}, {
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseComplete,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps", nonAdmissionWebhookNamespace),
			Verb:              "list",
			Code:              200,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     false,
			ResponseObject:    true,
			AuthorizeDecision: "allow",
		}, {
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseStarted,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps?timeout=%ds&timeoutSeconds=%d&watch=true", nonAdmissionWebhookNamespace, watchTestTimeout, watchTestTimeout),
			Verb:              "watch",
			Code:              200,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     false,
			ResponseObject:    false,
			AuthorizeDecision: "allow",
		}, {
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseComplete,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps?timeout=%ds&timeoutSeconds=%d&watch=true", nonAdmissionWebhookNamespace, watchTestTimeout, watchTestTimeout),
			Verb:              "watch",
			Code:              200,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     false,
			ResponseObject:    false,
			AuthorizeDecision: "allow",
		}, {
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseComplete,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps/audit-configmap", nonAdmissionWebhookNamespace),
			Verb:              "update",
			Code:              200,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     true,
			ResponseObject:    true,
			AuthorizeDecision: "allow",
		}, {
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseComplete,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps/audit-configmap", nonAdmissionWebhookNamespace),
			Verb:              "patch",
			Code:              200,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     true,
			ResponseObject:    true,
			AuthorizeDecision: "allow",
		}, {
			Level:             auditinternal.LevelRequestResponse,
			Stage:             auditinternal.StageResponseComplete,
			RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps/audit-configmap", nonAdmissionWebhookNamespace),
			Verb:              "delete",
			Code:              200,
			User:              auditTestUser,
			Resource:          "configmaps",
			Namespace:         nonAdmissionWebhookNamespace,
			RequestObject:     true,
			ResponseObject:    true,
			AuthorizeDecision: "allow",
		},
	}
)

// TestAudit ensures that both v1beta1 and v1 version audit api could work.
func TestAudit(t *testing.T) {
	for version := range versions {
		runTestWithVersion(t, version)
	}
}

func runTestWithVersion(t *testing.T, version string) {
	webhookMux := http.NewServeMux()
	webhookMux.Handle("/mutation", utils.AdmissionWebhookHandler(t, admitFunc))
	url, closeFunc, err := utils.NewAdmissionWebhookServer(webhookMux)
	defer closeFunc()
	if err != nil {
		t.Fatalf("%v", err)
	}

	// prepare audit policy file
	auditPolicy := strings.Replace(auditPolicyPattern, "{version}", version, 1)
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
	defer utiltesting.CloseAndRemove(t, logFile)

	// start api server
	result := kubeapiservertesting.StartTestServerOrDie(t, nil,
		[]string{
			"--audit-policy-file", policyFile.Name(),
			"--audit-log-version", version,
			"--audit-log-mode", "blocking",
			"--audit-log-path", logFile.Name()},
		framework.SharedEtcd())
	defer result.TearDownFn()

	kubeclient, err := clientset.NewForConfig(result.ClientConfig)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if err := createMutationWebhook(kubeclient, url+"/mutation"); err != nil {
		t.Fatal(err)
	}

	tcs := []struct {
		auditLevel            auditinternal.Level
		enableMutatingWebhook bool
		namespace             string
	}{
		{
			auditLevel:            auditinternal.LevelRequestResponse,
			enableMutatingWebhook: false,
			namespace:             nonAdmissionWebhookNamespace,
		},
		{
			auditLevel:            auditinternal.LevelMetadata,
			enableMutatingWebhook: true,
			namespace:             "webhook-audit-metadata",
		},
		{
			auditLevel:            auditinternal.LevelRequest,
			enableMutatingWebhook: true,
			namespace:             "webhook-audit-request",
		},
		{
			auditLevel:            auditinternal.LevelRequestResponse,
			enableMutatingWebhook: true,
			namespace:             "webhook-audit-response",
		},
	}

	crossGroupTestCases := []struct {
		auditLevel auditinternal.Level
		expEvents  []utils.AuditEvent
		namespace  string
	}{
		{
			auditLevel: auditinternal.LevelRequest,
			namespace:  "create-audit-request",
			expEvents: []utils.AuditEvent{
				{
					Level:             auditinternal.LevelRequest,
					Stage:             auditinternal.StageResponseComplete,
					RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/serviceaccounts/%s/token", "create-audit-request", "audit-serviceaccount"),
					Verb:              "create",
					Code:              201,
					User:              auditTestUser,
					Resource:          "serviceaccounts",
					Namespace:         "create-audit-request",
					RequestObject:     true,
					ResponseObject:    false,
					AuthorizeDecision: "allow",
				},
			},
		},
		{
			auditLevel: auditinternal.LevelRequestResponse,
			namespace:  "create-audit-response",
			expEvents: []utils.AuditEvent{
				{
					Level:             auditinternal.LevelRequestResponse,
					Stage:             auditinternal.StageResponseComplete,
					RequestURI:        fmt.Sprintf("/api/v1/namespaces/%s/serviceaccounts/%s/token", "create-audit-response", "audit-serviceaccount"),
					Verb:              "create",
					Code:              201,
					User:              auditTestUser,
					Resource:          "serviceaccounts",
					Namespace:         "create-audit-response",
					RequestObject:     true,
					ResponseObject:    true,
					AuthorizeDecision: "allow",
				},
			},
		},
		{
			auditLevel: auditinternal.LevelRequest,
			namespace:  "update-audit-request",
			expEvents: []utils.AuditEvent{
				{
					Level:             auditinternal.LevelRequest,
					Stage:             auditinternal.StageResponseComplete,
					RequestURI:        fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s/scale", "update-audit-request", "audit-deployment"),
					Verb:              "update",
					Code:              200,
					User:              auditTestUser,
					Resource:          "deployments",
					Namespace:         "update-audit-request",
					RequestObject:     true,
					ResponseObject:    false,
					AuthorizeDecision: "allow",
				},
			},
		},
		{
			auditLevel: auditinternal.LevelRequestResponse,
			namespace:  "update-audit-response",
			expEvents: []utils.AuditEvent{
				{
					Level:             auditinternal.LevelRequestResponse,
					Stage:             auditinternal.StageResponseComplete,
					RequestURI:        fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s/scale", "update-audit-response", "audit-deployment"),
					Verb:              "update",
					Code:              200,
					User:              auditTestUser,
					Resource:          "deployments",
					Namespace:         "update-audit-response",
					RequestObject:     true,
					ResponseObject:    true,
					AuthorizeDecision: "allow",
				},
			},
		},
	}

	for _, tc := range tcs {
		t.Run(fmt.Sprintf("%s.%s.%t", version, tc.auditLevel, tc.enableMutatingWebhook), func(t *testing.T) {
			testAudit(t, version, tc.auditLevel, tc.enableMutatingWebhook, tc.namespace, kubeclient, logFile)
		})
	}

	// cross-group subResources
	for _, tc := range crossGroupTestCases {
		t.Run(fmt.Sprintf("cross-group-%s.%s.%s", version, tc.auditLevel, tc.namespace), func(t *testing.T) {
			testAuditCrossGroupSubResource(t, version, tc.expEvents, tc.namespace, kubeclient, logFile)
		})
	}
}

func testAudit(t *testing.T, version string, level auditinternal.Level, enableMutatingWebhook bool, namespace string, kubeclient clientset.Interface, logFile *os.File) {
	var lastMissingReport string
	createNamespace(t, kubeclient, namespace)

	if err := wait.Poll(500*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		// perform configmap operations
		configMapOperations(t, kubeclient, namespace)

		// check for corresponding audit logs
		stream, err := os.Open(logFile.Name())
		if err != nil {
			return false, fmt.Errorf("unexpected error: %v", err)
		}
		defer stream.Close()
		missingReport, err := utils.CheckAuditLines(stream, getExpectedEvents(level, enableMutatingWebhook, namespace), versions[version])
		if err != nil {
			return false, fmt.Errorf("unexpected error: %v", err)
		}
		if len(missingReport.MissingEvents) > 0 {
			lastMissingReport = missingReport.String()
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatalf("failed to get expected events -- missingReport: %s, error: %v", lastMissingReport, err)
	}
}

func testAuditCrossGroupSubResource(t *testing.T, version string, expEvents []utils.AuditEvent, namespace string, kubeclient clientset.Interface, logFile *os.File) {
	var (
		lastMissingReport string
		sa                *apiv1.ServiceAccount
		deploy            *appsv1.Deployment
	)

	createNamespace(t, kubeclient, namespace)
	switch expEvents[0].Resource {
	case "serviceaccounts":
		sa = createServiceAccount(t, kubeclient, namespace)
	case "deployments":
		deploy = createDeployment(t, kubeclient, namespace)
	default:
		t.Fatalf("%v resource has no cross-group sub-resources", expEvents[0].Resource)
	}

	if err := wait.Poll(500*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		// perform cross-group subresources operations
		if sa != nil {
			tokenRequestOperations(t, kubeclient, sa.Namespace, sa.Name)
		}
		if deploy != nil {
			scaleOperations(t, kubeclient, deploy.Namespace, deploy.Name)
		}

		// check for corresponding audit logs
		stream, err := os.Open(logFile.Name())
		if err != nil {
			return false, fmt.Errorf("unexpected error: %v", err)
		}
		defer stream.Close()
		missingReport, err := utils.CheckAuditLines(stream, expEvents, versions[version])
		if err != nil {
			return false, fmt.Errorf("unexpected error: %v", err)
		}
		if len(missingReport.MissingEvents) > 0 {
			lastMissingReport = missingReport.String()
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatalf("failed to get expected events -- missingReport: %s, error: %v", lastMissingReport, err)
	}
}

func getExpectedEvents(level auditinternal.Level, enableMutatingWebhook bool, namespace string) []utils.AuditEvent {
	if !enableMutatingWebhook {
		return expectedEvents
	}

	var webhookMutationAnnotations, webhookPatchAnnotations map[string]string
	var requestObject, responseObject bool
	if level.GreaterOrEqual(auditinternal.LevelMetadata) {
		// expect mutation existence annotation
		webhookMutationAnnotations = map[string]string{}
		webhookMutationAnnotations[mutating.MutationAuditAnnotationPrefix+"round_0_index_0"] = fmt.Sprintf(`{"configuration":"%s","webhook":"%s","mutated":%t}`, testWebhookConfigurationName, testWebhookName, true)
	}
	if level.GreaterOrEqual(auditinternal.LevelRequest) {
		// expect actual patch annotation
		webhookPatchAnnotations = map[string]string{}
		webhookPatchAnnotations[mutating.PatchAuditAnnotationPrefix+"round_0_index_0"] = strings.Replace(fmt.Sprintf(`{"configuration": "%s", "webhook": "%s", "patch": %s, "patchType": "JSONPatch"}`, testWebhookConfigurationName, testWebhookName, `[{"op":"add","path":"/data","value":{"test":"dummy"}}]`), " ", "", -1)
		// expect request object in audit log
		requestObject = true
	}
	if level.GreaterOrEqual(auditinternal.LevelRequestResponse) {
		// expect response obect in audit log
		responseObject = true
	}
	return []utils.AuditEvent{
		{
			// expect CREATE audit event with webhook in effect
			Level:                               level,
			Stage:                               auditinternal.StageResponseComplete,
			RequestURI:                          fmt.Sprintf("/api/v1/namespaces/%s/configmaps", namespace),
			Verb:                                "create",
			Code:                                201,
			User:                                auditTestUser,
			Resource:                            "configmaps",
			Namespace:                           namespace,
			AuthorizeDecision:                   "allow",
			RequestObject:                       requestObject,
			ResponseObject:                      responseObject,
			AdmissionWebhookMutationAnnotations: webhookMutationAnnotations,
			AdmissionWebhookPatchAnnotations:    webhookPatchAnnotations,
		}, {
			// expect UPDATE audit event with webhook in effect
			Level:                               level,
			Stage:                               auditinternal.StageResponseComplete,
			RequestURI:                          fmt.Sprintf("/api/v1/namespaces/%s/configmaps/audit-configmap", namespace),
			Verb:                                "update",
			Code:                                200,
			User:                                auditTestUser,
			Resource:                            "configmaps",
			Namespace:                           namespace,
			AuthorizeDecision:                   "allow",
			RequestObject:                       requestObject,
			ResponseObject:                      responseObject,
			AdmissionWebhookMutationAnnotations: webhookMutationAnnotations,
			AdmissionWebhookPatchAnnotations:    webhookPatchAnnotations,
		},
	}
}

// configMapOperations is a set of known operations performed on the configmap type
// which correspond to the expected events.
// This is shared by the dynamic test
func configMapOperations(t *testing.T, kubeclient clientset.Interface, namespace string) {
	// create, get, watch, update, patch, list and delete configmap.
	configMap := &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "audit-configmap",
			Namespace: namespace,
		},
		Data: map[string]string{
			"map-key": "map-value",
		},
	}
	// add admission label to config maps that are to be sent to webhook
	if namespace != nonAdmissionWebhookNamespace {
		configMap.Labels = map[string]string{
			"admission": "true",
		}
	}

	_, err := kubeclient.CoreV1().ConfigMaps(namespace).Create(context.TODO(), configMap, metav1.CreateOptions{})
	expectNoError(t, err, "failed to create audit-configmap")

	_, err = kubeclient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), configMap.Name, metav1.GetOptions{})
	expectNoError(t, err, "failed to get audit-configmap")

	configMapChan, err := kubeclient.CoreV1().ConfigMaps(namespace).Watch(context.TODO(), watchOptions)
	expectNoError(t, err, "failed to create watch for config maps")
	for range configMapChan.ResultChan() {
		// Block until watchOptions.TimeoutSeconds expires.
		// If the test finishes before watchOptions.TimeoutSeconds expires, the watch audit
		// event at stage ResponseComplete will not be generated.
	}

	_, err = kubeclient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), configMap, metav1.UpdateOptions{})
	expectNoError(t, err, "failed to update audit-configmap")

	_, err = kubeclient.CoreV1().ConfigMaps(namespace).Patch(context.TODO(), configMap.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
	expectNoError(t, err, "failed to patch configmap")

	_, err = kubeclient.CoreV1().ConfigMaps(namespace).List(context.TODO(), metav1.ListOptions{})
	expectNoError(t, err, "failed to list config maps")

	err = kubeclient.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), configMap.Name, metav1.DeleteOptions{})
	expectNoError(t, err, "failed to delete audit-configmap")
}

func tokenRequestOperations(t *testing.T, kubeClient clientset.Interface, namespace, name string) {
	var (
		treq = &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				Audiences: []string{"api"},
			},
		}
	)
	// create tokenRequest
	_, err := kubeClient.CoreV1().ServiceAccounts(namespace).CreateToken(context.TODO(), name, treq, metav1.CreateOptions{})
	expectNoError(t, err, "failed to create audit-tokenRequest")
}

func scaleOperations(t *testing.T, kubeClient clientset.Interface, namespace, name string) {
	var (
		scale = &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-deployment",
				Namespace: namespace,
			},
			Spec: autoscalingv1.ScaleSpec{
				Replicas: 2,
			},
		}
	)

	// update scale
	_, err := kubeClient.AppsV1().Deployments(namespace).UpdateScale(context.TODO(), name, scale, metav1.UpdateOptions{})
	expectNoError(t, err, fmt.Sprintf("failed to update scale %v", scale))
}

func expectNoError(t *testing.T, err error, msg string) {
	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

func admitFunc(review *v1beta1.AdmissionReview) error {
	gvk := schema.GroupVersionKind{Group: "admission.k8s.io", Version: "v1beta1", Kind: "AdmissionReview"}
	if review.GetObjectKind().GroupVersionKind() != gvk {
		return fmt.Errorf("invalid admission review kind: %#v", review.GetObjectKind().GroupVersionKind())
	}
	if len(review.Request.Object.Raw) > 0 {
		u := &unstructured.Unstructured{Object: map[string]interface{}{}}
		if err := json.Unmarshal(review.Request.Object.Raw, u); err != nil {
			return fmt.Errorf("failed to deserialize object: %s with error: %v", string(review.Request.Object.Raw), err)
		}
		review.Request.Object.Object = u
	}
	if len(review.Request.OldObject.Raw) > 0 {
		u := &unstructured.Unstructured{Object: map[string]interface{}{}}
		if err := json.Unmarshal(review.Request.OldObject.Raw, u); err != nil {
			return fmt.Errorf("failed to deserialize object: %s with error: %v", string(review.Request.OldObject.Raw), err)
		}
		review.Request.OldObject.Object = u
	}

	review.Response = &v1beta1.AdmissionResponse{
		Allowed: true,
		UID:     review.Request.UID,
		Result:  &metav1.Status{Message: "admitted"},
	}
	review.Response.Patch = []byte(`[{"op":"add","path":"/data","value":{"test":"dummy"}}]`)
	jsonPatch := v1beta1.PatchTypeJSONPatch
	review.Response.PatchType = &jsonPatch
	return nil
}

func createMutationWebhook(client clientset.Interface, endpoint string) error {
	fail := admissionregistrationv1.Fail
	noSideEffects := admissionregistrationv1.SideEffectClassNone
	// Attaching Mutation webhook to API server
	_, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: testWebhookConfigurationName},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name: testWebhookName,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				URL:      &endpoint,
				CABundle: utils.LocalhostCert,
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
				Rule:       admissionregistrationv1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*/*"}},
			}},
			ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{"admission": "true"}},
			FailurePolicy:           &fail,
			AdmissionReviewVersions: []string{"v1beta1"},
			SideEffects:             &noSideEffects,
		}},
	}, metav1.CreateOptions{})
	return err
}

func createNamespace(t *testing.T, kubeclient clientset.Interface, namespace string) {
	ns := &apiv1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	_, err := kubeclient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
	expectNoError(t, err, fmt.Sprintf("failed to create namespace ns %s", namespace))
}

func createServiceAccount(t *testing.T, cs clientset.Interface, namespace string) *apiv1.ServiceAccount {
	sa := &apiv1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "audit-serviceaccount",
			Namespace: namespace,
		},
	}
	_, err := cs.CoreV1().ServiceAccounts(sa.Namespace).Create(context.TODO(), sa, metav1.CreateOptions{})
	expectNoError(t, err, fmt.Sprintf("failed to create serviceaccount %v", sa))
	return sa
}

func createDeployment(t *testing.T, cs clientset.Interface, namespace string) *appsv1.Deployment {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "audit-deployment",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: apiv1.PodTemplateSpec{
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:  "foo",
							Image: "foo/bar",
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "audit-deployment-scale",
					Namespace: namespace,
					Labels:    map[string]string{"app": "test"},
				},
			},
		},
	}
	_, err := cs.AppsV1().Deployments(deploy.Namespace).Create(context.TODO(), deploy, metav1.CreateOptions{})
	expectNoError(t, err, fmt.Sprintf("failed to create deployment %v", deploy))
	return deploy
}
