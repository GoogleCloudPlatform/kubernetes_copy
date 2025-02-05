/*
Copyright 2023 The Kubernetes Authors.

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

package admissionwebhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

type admissionRecorder struct {
	mu       sync.Mutex
	upCh     chan struct{}
	upOnce   sync.Once
	requests []*admissionv1.AdmissionRequest
}

func (r *admissionRecorder) Record(req *admissionv1.AdmissionRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
}

func (r *admissionRecorder) MarkerReceived() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upOnce.Do(func() {
		close(r.upCh)
	})
}

func (r *admissionRecorder) Reset() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = []*admissionv1.AdmissionRequest{}
	r.upCh = make(chan struct{})
	r.upOnce = sync.Once{}
	return r.upCh
}

func newMatchConditionHandler(recorder *admissionRecorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
		}
		review := admissionv1.AdmissionReview{}
		if err := json.Unmarshal(data, &review); err != nil {
			http.Error(w, err.Error(), 400)
		}

		review.Response = &admissionv1.AdmissionResponse{
			Allowed: true,
			UID:     review.Request.UID,
			Result:  &metav1.Status{Message: "admitted"},
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(review); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		switch r.URL.Path {
		case "/marker":
			recorder.MarkerReceived()
			return
		}

		recorder.Record(review.Request)
	})
}

// TestMatchConditions tests ValidatingWebhookConfigurations and MutatingWebhookConfigurations that validates different cases of matchCondition fields
func TestMatchConditions(t *testing.T) {
	fail := admissionregistrationv1.Fail
	ignore := admissionregistrationv1.Ignore

	testcases := []struct {
		name            string
		matchConditions []admissionregistrationv1.MatchCondition
		pods            []*corev1.Pod
		matchedPods     []*corev1.Pod
		expectErrorPod  bool
		failPolicy      *admissionregistrationv1.FailurePolicyType
		errMessage      string
	}{
		{
			name: "pods in namespace kube-system is ignored",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "pods-in-kube-system-exempt.kubernetes.io",
					Expression: "object.metadata.namespace != 'kube-system'",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
				matchConditionsTestPod("test2", "default"),
			},
			matchedPods: []*corev1.Pod{
				matchConditionsTestPod("test2", "default"),
			},
		},
		{
			name: "matchConditions are ANDed together",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "pods-in-kube-system-exempt.kubernetes.io",
					Expression: "object.metadata.namespace != 'kube-system'",
				},
				{
					Name:       "pods-with-name-test1.kubernetes.io",
					Expression: "object.metadata.name == 'test1'",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
				matchConditionsTestPod("test1", "default"),
				matchConditionsTestPod("test2", "default"),
			},
			matchedPods: []*corev1.Pod{
				matchConditionsTestPod("test1", "default"),
			},
		},
		{
			name: "mix of true, error and false should not match and not call webhook",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "test1",
					Expression: "object.nonExistentProperty == 'someval'",
				},
				{
					Name:       "test2",
					Expression: "true",
				},
				{
					Name:       "test3",
					Expression: "false",
				},
				{
					Name:       "test4",
					Expression: "true",
				},
				{
					Name:       "test5",
					Expression: "object.nonExistentProperty == 'someval'",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
				matchConditionsTestPod("test2", "default"),
			},
			matchedPods:    []*corev1.Pod{},
			expectErrorPod: false,
		},
		{
			name: "mix of true and error should reject request without fail policy",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "test1",
					Expression: "object.nonExistentProperty == 'someval'",
				},
				{
					Name:       "test2",
					Expression: "true",
				},
				{
					Name:       "test4",
					Expression: "true",
				},
				{
					Name:       "test5",
					Expression: "object.nonExistentProperty == 'someval'",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
				matchConditionsTestPod("test2", "default"),
			},
			matchedPods:    []*corev1.Pod{},
			expectErrorPod: true,
		},
		{
			name: "mix of true and error should reject request with fail policy fail",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "test1",
					Expression: "object.nonExistentProperty == 'someval'",
				},
				{
					Name:       "test2",
					Expression: "true",
				},
				{
					Name:       "test4",
					Expression: "true",
				},
				{
					Name:       "test5",
					Expression: "object.nonExistentProperty == 'someval'",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
				matchConditionsTestPod("test2", "default"),
			},
			matchedPods:    []*corev1.Pod{},
			failPolicy:     &fail,
			expectErrorPod: true,
		},
		{
			name: "mix of true and error should match request and call webhook with fail policy ignore",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "tes1",
					Expression: "object.nonExistentProperty == 'someval'",
				},
				{
					Name:       "test2",
					Expression: "true",
				},
				{
					Name:       "test4",
					Expression: "true",
				},
				{
					Name:       "test5",
					Expression: "object.nonExistentProperty == 'someval'",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
				matchConditionsTestPod("test2", "default"),
			},
			matchedPods: []*corev1.Pod{},
			failPolicy:  &ignore,
		},
		{
			name: "has access to oldObject",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "old-object-is-null.kubernetes.io",
					Expression: "oldObject == null",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test2", "default"),
			},
			matchedPods: []*corev1.Pod{
				matchConditionsTestPod("test2", "default"),
			},
		},
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(localhostCert) {
		t.Fatal("Failed to append Cert from PEM")
	}
	cert, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		t.Fatalf("Failed to build cert with error: %+v", err)
	}

	recorder := &admissionRecorder{requests: []*admissionv1.AdmissionRequest{}}

	webhookServer := httptest.NewUnstartedServer(newMatchConditionHandler(recorder))
	webhookServer.TLS = &tls.Config{
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
	}
	webhookServer.StartTLS()
	defer webhookServer.Close()

	dryRunCreate := metav1.CreateOptions{
		DryRun: []string{metav1.DryRunAll},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			upCh := recorder.Reset()

			server, err := apiservertesting.StartTestServer(t, nil, []string{
				"--disable-admission-plugins=ServiceAccount",
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

			// Write markers to a separate namespace to avoid cross-talk
			markerNs := "marker"
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: markerNs}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// Create a marker object to use to check for the webhook configurations to be ready.
			marker, err := client.CoreV1().Pods(markerNs).Create(context.TODO(), newMarkerPod(markerNs), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			endpoint := webhookServer.URL
			markerEndpoint := webhookServer.URL + "/marker"
			validatingwebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name: "admission.integration.test",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"pods"},
							},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &endpoint,
							CABundle: localhostCert,
						},
						// ignore pods in the marker namespace
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      corev1.LabelMetadataName,
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"marker"},
								},
							}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
						MatchConditions:         testcase.matchConditions,
					},
					{
						Name: "admission.integration.test.marker",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &markerEndpoint,
							CABundle: localhostCert,
						},
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							corev1.LabelMetadataName: "marker",
						}},
						ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{"marker": "true"}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
					},
				},
			}

			validatingcfg, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), validatingwebhook, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			vhwHasBeenCleanedUp := false
			defer func() {
				if !vhwHasBeenCleanedUp {
					err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), validatingcfg.GetName(), metav1.DeleteOptions{})
					if err != nil {
						t.Fatal(err)
					}
				}
			}()

			// wait until new webhook is called the first time
			if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*5, wait.ForeverTestTimeout, true, func(_ context.Context) (bool, error) {
				_, err = client.CoreV1().Pods(markerNs).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
				select {
				case <-upCh:
					return true, nil
				default:
					t.Logf("Waiting for webhook to become effective, getting marker object: %v", err)
					return false, nil
				}
			}); err != nil {
				t.Fatal(err)
			}

			for _, pod := range testcase.pods {
				_, err := client.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, dryRunCreate)
				if !testcase.expectErrorPod && err != nil {
					t.Fatalf("unexpected error creating test pod: %v", err)
				} else if testcase.expectErrorPod && err == nil {
					t.Fatal("expected error creating pods")
				} else if testcase.expectErrorPod && err != nil && !strings.Contains(err.Error(), testcase.errMessage) {
					t.Fatalf("expected error message includes: %v, but get %v", testcase.errMessage, err)
				}
			}

			if len(recorder.requests) != len(testcase.matchedPods) {
				t.Errorf("unexpected requests %v, expected %v", recorder.requests, testcase.matchedPods)
			}

			for i, request := range recorder.requests {
				if request.Name != testcase.matchedPods[i].Name {
					t.Errorf("unexpected pod name %v, expected %v", request.Name, testcase.matchedPods[i].Name)
				}
				if request.Namespace != testcase.matchedPods[i].Namespace {
					t.Errorf("unexpected pod namespace %v, expected %v", request.Namespace, testcase.matchedPods[i].Namespace)
				}
			}

			// Reset and rerun against mutating webhook configuration
			// TODO: private helper function for validation after creating vwh or mwh
			upCh = recorder.Reset()
			err = client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), validatingcfg.GetName(), metav1.DeleteOptions{})
			if err != nil {
				t.Fatal(err)
			} else {
				vhwHasBeenCleanedUp = true
			}

			mutatingwebhook := &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name: "admission.integration.test",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"pods"},
							},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &endpoint,
							CABundle: localhostCert,
						},
						// ignore pods in the marker namespace
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      corev1.LabelMetadataName,
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"marker"},
								},
							}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
						MatchConditions:         testcase.matchConditions,
					},
					{
						Name: "admission.integration.test.marker",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &markerEndpoint,
							CABundle: localhostCert,
						},
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							corev1.LabelMetadataName: "marker",
						}},
						ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{"marker": "true"}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
					},
				},
			}

			mutatingcfg, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), mutatingwebhook, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.TODO(), mutatingcfg.GetName(), metav1.DeleteOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}()

			// wait until new webhook is called the first time
			if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*5, wait.ForeverTestTimeout, true, func(_ context.Context) (bool, error) {
				_, err = client.CoreV1().Pods(markerNs).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
				select {
				case <-upCh:
					return true, nil
				default:
					t.Logf("Waiting for webhook to become effective, getting marker object: %v", err)
					return false, nil
				}
			}); err != nil {
				t.Fatal(err)
			}

			for _, pod := range testcase.pods {
				_, err = client.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, dryRunCreate)
				if testcase.expectErrorPod == false && err != nil {
					t.Fatalf("unexpected error creating test pod: %v", err)
				} else if testcase.expectErrorPod == true && err == nil {
					t.Fatal("expected error creating pods")
				}
			}

			if len(recorder.requests) != len(testcase.matchedPods) {
				t.Errorf("unexpected requests %v, expected %v", recorder.requests, testcase.matchedPods)
			}

			for i, request := range recorder.requests {
				if request.Name != testcase.matchedPods[i].Name {
					t.Errorf("unexpected pod name %v, expected %v", request.Name, testcase.matchedPods[i].Name)
				}
				if request.Namespace != testcase.matchedPods[i].Namespace {
					t.Errorf("unexpected pod namespace %v, expected %v", request.Namespace, testcase.matchedPods[i].Namespace)
				}
			}
		})
	}
}

func TestMatchConditionsWithoutStrictCostEnforcement(t *testing.T) {
	testcases := []struct {
		name            string
		matchConditions []admissionregistrationv1.MatchCondition
		pods            []*corev1.Pod
		matchedPods     []*corev1.Pod
		expectErrorPod  bool
		failPolicy      *admissionregistrationv1.FailurePolicyType
		errMessage      string
	}{
		{
			name: "without strict cost enforcement: Authz check does not exceed per call limit",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "test1",
					Expression: "authorizer.group('').resource('pods').name('test1').check('create').allowed() && authorizer.group('').resource('pods').name('test1').check('create').allowed() && authorizer.group('').resource('pods').name('test1').check('create').allowed()",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
			},
			matchedPods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
			},
			expectErrorPod: false,
		},
		{
			name:            "without strict cost enforcement: Authz check does not exceed overall cost limit",
			matchConditions: generateMatchConditionsWithAuthzCheck(8, "authorizer.group('').resource('pods').name('test1').check('create').allowed() && authorizer.group('').resource('pods').name('test1').check('create').allowed()"),
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
			},
			matchedPods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
			},
			expectErrorPod: false,
		},
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(localhostCert) {
		t.Fatal("Failed to append Cert from PEM")
	}
	cert, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		t.Fatalf("Failed to build cert with error: %+v", err)
	}

	recorder := &admissionRecorder{requests: []*admissionv1.AdmissionRequest{}}

	webhookServer := httptest.NewUnstartedServer(newMatchConditionHandler(recorder))
	webhookServer.TLS = &tls.Config{
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
	}
	webhookServer.StartTLS()
	defer webhookServer.Close()

	dryRunCreate := metav1.CreateOptions{
		DryRun: []string{metav1.DryRunAll},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			upCh := recorder.Reset()
			server, err := apiservertesting.StartTestServer(t, nil, []string{
				"--emulated-version", "1.31",
				"--feature-gates", "StrictCostEnforcementForWebhooks=false",
				"--disable-admission-plugins=ServiceAccount",
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

			// Write markers to a separate namespace to avoid cross-talk
			markerNs := "marker"
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: markerNs}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// Create a marker object to use to check for the webhook configurations to be ready.
			marker, err := client.CoreV1().Pods(markerNs).Create(context.TODO(), newMarkerPod(markerNs), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			endpoint := webhookServer.URL
			markerEndpoint := webhookServer.URL + "/marker"
			validatingwebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name: "admission.integration.test",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"pods"},
							},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &endpoint,
							CABundle: localhostCert,
						},
						// ignore pods in the marker namespace
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      corev1.LabelMetadataName,
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"marker"},
								},
							}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
						MatchConditions:         testcase.matchConditions,
					},
					{
						Name: "admission.integration.test.marker",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &markerEndpoint,
							CABundle: localhostCert,
						},
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							corev1.LabelMetadataName: "marker",
						}},
						ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{"marker": "true"}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
					},
				},
			}

			validatingcfg, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), validatingwebhook, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			vhwHasBeenCleanedUp := false
			defer func() {
				if !vhwHasBeenCleanedUp {
					err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), validatingcfg.GetName(), metav1.DeleteOptions{})
					if err != nil {
						t.Fatal(err)
					}
				}
			}()

			// wait until new webhook is called the first time
			if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*5, wait.ForeverTestTimeout, true, func(_ context.Context) (bool, error) {
				_, err = client.CoreV1().Pods(markerNs).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
				select {
				case <-upCh:
					return true, nil
				default:
					t.Logf("Waiting for webhook to become effective, getting marker object: %v", err)
					return false, nil
				}
			}); err != nil {
				t.Fatal(err)
			}

			for _, pod := range testcase.pods {
				_, err := client.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, dryRunCreate)
				if !testcase.expectErrorPod && err != nil {
					t.Fatalf("unexpected error creating test pod: %v", err)
				} else if testcase.expectErrorPod && err == nil {
					t.Fatal("expected error creating pods")
				} else if testcase.expectErrorPod && err != nil && !strings.Contains(err.Error(), testcase.errMessage) {
					t.Fatalf("expected error message includes: %v, but get %v", testcase.errMessage, err)
				}
			}

			if len(recorder.requests) != len(testcase.matchedPods) {
				t.Errorf("unexpected requests %v, expected %v", recorder.requests, testcase.matchedPods)
			}

			for i, request := range recorder.requests {
				if request.Name != testcase.matchedPods[i].Name {
					t.Errorf("unexpected pod name %v, expected %v", request.Name, testcase.matchedPods[i].Name)
				}
				if request.Namespace != testcase.matchedPods[i].Namespace {
					t.Errorf("unexpected pod namespace %v, expected %v", request.Namespace, testcase.matchedPods[i].Namespace)
				}
			}

			// Reset and rerun against mutating webhook configuration
			// TODO: private helper function for validation after creating vwh or mwh
			upCh = recorder.Reset()
			err = client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), validatingcfg.GetName(), metav1.DeleteOptions{})
			if err != nil {
				t.Fatal(err)
			} else {
				vhwHasBeenCleanedUp = true
			}

			mutatingwebhook := &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name: "admission.integration.test",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"pods"},
							},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &endpoint,
							CABundle: localhostCert,
						},
						// ignore pods in the marker namespace
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      corev1.LabelMetadataName,
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"marker"},
								},
							}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
						MatchConditions:         testcase.matchConditions,
					},
					{
						Name: "admission.integration.test.marker",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &markerEndpoint,
							CABundle: localhostCert,
						},
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							corev1.LabelMetadataName: "marker",
						}},
						ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{"marker": "true"}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
					},
				},
			}

			mutatingcfg, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), mutatingwebhook, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.TODO(), mutatingcfg.GetName(), metav1.DeleteOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}()

			// wait until new webhook is called the first time
			if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*5, wait.ForeverTestTimeout, true, func(_ context.Context) (bool, error) {
				_, err = client.CoreV1().Pods(markerNs).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
				select {
				case <-upCh:
					return true, nil
				default:
					t.Logf("Waiting for webhook to become effective, getting marker object: %v", err)
					return false, nil
				}
			}); err != nil {
				t.Fatal(err)
			}

			for _, pod := range testcase.pods {
				_, err = client.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, dryRunCreate)
				if testcase.expectErrorPod == false && err != nil {
					t.Fatalf("unexpected error creating test pod: %v", err)
				} else if testcase.expectErrorPod == true && err == nil {
					t.Fatal("expected error creating pods")
				}
			}

			if len(recorder.requests) != len(testcase.matchedPods) {
				t.Errorf("unexpected requests %v, expected %v", recorder.requests, testcase.matchedPods)
			}

			for i, request := range recorder.requests {
				if request.Name != testcase.matchedPods[i].Name {
					t.Errorf("unexpected pod name %v, expected %v", request.Name, testcase.matchedPods[i].Name)
				}
				if request.Namespace != testcase.matchedPods[i].Namespace {
					t.Errorf("unexpected pod namespace %v, expected %v", request.Namespace, testcase.matchedPods[i].Namespace)
				}
			}
		})
	}
}

func TestMatchConditionsWithStrictCostEnforcement(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.StrictCostEnforcementForWebhooks, true)

	testcases := []struct {
		name            string
		matchConditions []admissionregistrationv1.MatchCondition
		pods            []*corev1.Pod
		matchedPods     []*corev1.Pod
		expectErrorPod  bool
		failPolicy      *admissionregistrationv1.FailurePolicyType
		errMessage      string
	}{
		{
			name: "with strict cost enforcement: exceed per call limit should reject request with fail policy fail",
			matchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name:       "test1",
					Expression: "authorizer.group('').resource('pods').namespace('default').check('create').allowed() && authorizer.group('').resource('pods').namespace('default').check('create').allowed() && authorizer.group('').resource('pods').namespace('default').check('create').allowed()",
				},
			},
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "default"),
			},
			matchedPods:    []*corev1.Pod{},
			expectErrorPod: true,
			errMessage:     "operation cancelled: actual cost limit exceeded",
		},
		{
			name:            "with strict cost enforcement: exceed overall cost limit should reject request with fail policy fail",
			matchConditions: generateMatchConditionsWithAuthzCheck(8, "authorizer.group('').resource('pods').name('test1').check('create').allowed() && authorizer.group('').resource('pods').name('test1').check('create').allowed()"),
			pods: []*corev1.Pod{
				matchConditionsTestPod("test1", "kube-system"),
			},
			matchedPods:    []*corev1.Pod{},
			expectErrorPod: true,
			errMessage:     "validation failed due to running out of cost budget, no further validation rules will be run",
		},
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(localhostCert) {
		t.Fatal("Failed to append Cert from PEM")
	}
	cert, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		t.Fatalf("Failed to build cert with error: %+v", err)
	}

	recorder := &admissionRecorder{requests: []*admissionv1.AdmissionRequest{}}

	webhookServer := httptest.NewUnstartedServer(newMatchConditionHandler(recorder))
	webhookServer.TLS = &tls.Config{
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
	}
	webhookServer.StartTLS()
	defer webhookServer.Close()

	dryRunCreate := metav1.CreateOptions{
		DryRun: []string{metav1.DryRunAll},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			upCh := recorder.Reset()
			server, err := apiservertesting.StartTestServer(t, nil, []string{
				"--disable-admission-plugins=ServiceAccount",
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

			// Write markers to a separate namespace to avoid cross-talk
			markerNs := "marker"
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: markerNs}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// Create a marker object to use to check for the webhook configurations to be ready.
			marker, err := client.CoreV1().Pods(markerNs).Create(context.TODO(), newMarkerPod(markerNs), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			endpoint := webhookServer.URL
			markerEndpoint := webhookServer.URL + "/marker"
			validatingwebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name: "admission.integration.test",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"pods"},
							},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &endpoint,
							CABundle: localhostCert,
						},
						// ignore pods in the marker namespace
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      corev1.LabelMetadataName,
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"marker"},
								},
							}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
						MatchConditions:         testcase.matchConditions,
					},
					{
						Name: "admission.integration.test.marker",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &markerEndpoint,
							CABundle: localhostCert,
						},
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							corev1.LabelMetadataName: "marker",
						}},
						ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{"marker": "true"}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
					},
				},
			}

			validatingcfg, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), validatingwebhook, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			vhwHasBeenCleanedUp := false
			defer func() {
				if !vhwHasBeenCleanedUp {
					err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), validatingcfg.GetName(), metav1.DeleteOptions{})
					if err != nil {
						t.Fatal(err)
					}
				}
			}()

			// wait until new webhook is called the first time
			if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*5, wait.ForeverTestTimeout, true, func(_ context.Context) (bool, error) {
				_, err = client.CoreV1().Pods(markerNs).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
				select {
				case <-upCh:
					return true, nil
				default:
					t.Logf("Waiting for webhook to become effective, getting marker object: %v", err)
					return false, nil
				}
			}); err != nil {
				t.Fatal(err)
			}

			for _, pod := range testcase.pods {
				_, err := client.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, dryRunCreate)
				if !testcase.expectErrorPod && err != nil {
					t.Fatalf("unexpected error creating test pod: %v", err)
				} else if testcase.expectErrorPod && err == nil {
					t.Fatal("expected error creating pods")
				} else if testcase.expectErrorPod && err != nil && !strings.Contains(err.Error(), testcase.errMessage) {
					t.Fatalf("expected error message includes: %v, but get %v", testcase.errMessage, err)
				}
			}

			if len(recorder.requests) != len(testcase.matchedPods) {
				t.Errorf("unexpected requests %v, expected %v", recorder.requests, testcase.matchedPods)
			}

			for i, request := range recorder.requests {
				if request.Name != testcase.matchedPods[i].Name {
					t.Errorf("unexpected pod name %v, expected %v", request.Name, testcase.matchedPods[i].Name)
				}
				if request.Namespace != testcase.matchedPods[i].Namespace {
					t.Errorf("unexpected pod namespace %v, expected %v", request.Namespace, testcase.matchedPods[i].Namespace)
				}
			}

			// Reset and rerun against mutating webhook configuration
			// TODO: private helper function for validation after creating vwh or mwh
			upCh = recorder.Reset()
			err = client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), validatingcfg.GetName(), metav1.DeleteOptions{})
			if err != nil {
				t.Fatal(err)
			} else {
				vhwHasBeenCleanedUp = true
			}

			mutatingwebhook := &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name: "admission.integration.test",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"pods"},
							},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &endpoint,
							CABundle: localhostCert,
						},
						// ignore pods in the marker namespace
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      corev1.LabelMetadataName,
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"marker"},
								},
							}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
						MatchConditions:         testcase.matchConditions,
					},
					{
						Name: "admission.integration.test.marker",
						Rules: []admissionregistrationv1.RuleWithOperations{{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
						}},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							URL:      &markerEndpoint,
							CABundle: localhostCert,
						},
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							corev1.LabelMetadataName: "marker",
						}},
						ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{"marker": "true"}},
						FailurePolicy:           testcase.failPolicy,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: []string{"v1"},
					},
				},
			}

			mutatingcfg, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), mutatingwebhook, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.TODO(), mutatingcfg.GetName(), metav1.DeleteOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}()

			// wait until new webhook is called the first time
			if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*5, wait.ForeverTestTimeout, true, func(_ context.Context) (bool, error) {
				_, err = client.CoreV1().Pods(markerNs).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
				select {
				case <-upCh:
					return true, nil
				default:
					t.Logf("Waiting for webhook to become effective, getting marker object: %v", err)
					return false, nil
				}
			}); err != nil {
				t.Fatal(err)
			}

			for _, pod := range testcase.pods {
				_, err = client.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, dryRunCreate)
				if testcase.expectErrorPod == false && err != nil {
					t.Fatalf("unexpected error creating test pod: %v", err)
				} else if testcase.expectErrorPod == true && err == nil {
					t.Fatal("expected error creating pods")
				}
			}

			if len(recorder.requests) != len(testcase.matchedPods) {
				t.Errorf("unexpected requests %v, expected %v", recorder.requests, testcase.matchedPods)
			}

			for i, request := range recorder.requests {
				if request.Name != testcase.matchedPods[i].Name {
					t.Errorf("unexpected pod name %v, expected %v", request.Name, testcase.matchedPods[i].Name)
				}
				if request.Namespace != testcase.matchedPods[i].Namespace {
					t.Errorf("unexpected pod namespace %v, expected %v", request.Namespace, testcase.matchedPods[i].Namespace)
				}
			}
		})
	}
}

func TestMatchConditions_validation(t *testing.T) {

	server := apiservertesting.StartTestServerOrDie(t, nil, []string{
		"--disable-admission-plugins=ServiceAccount",
	}, framework.SharedEtcd())
	defer server.TearDownFn()

	client := clientset.NewForConfigOrDie(server.ClientConfig)

	testcases := []struct {
		name            string
		matchConditions []admissionregistrationv1.MatchCondition
		expectError     bool
	}{{
		name: "valid match condition",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       "true",
			Expression: "true",
		}},
		expectError: false,
	}, {
		name: "multiple valid match conditions",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       "exclude-leases",
			Expression: "!(request.resource.group == 'coordination.k8s.io' && request.resource.resource == 'leases')",
		}, {
			Name:       "exclude-kubelet-requests",
			Expression: "!('system:nodes' in request.userInfo.groups)",
		}, {
			Name:       "breakglass",
			Expression: "!authorizer.group('admissionregistration.k8s.io').resource('validatingwebhookconfigurations').name('my-webhook.example.com').check('breakglass').allowed()",
		}},
		expectError: false,
	}, {
		name: "invalid field should error",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       "old-object-is-null.kubernetes.io",
			Expression: "imnotafield == null",
		}},
		expectError: true,
	}, {
		name: "missing expression should error",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name: "old-object-is-null.kubernetes.io",
		}},
		expectError: true,
	}, {
		name: "missing name should error",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Expression: "oldObject == null",
		}},
		expectError: true,
	}, {
		name: "empty name should error",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       "",
			Expression: "oldObject == null",
		}},
		expectError: true,
	}, {
		name: "empty expression should error",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       "test-empty-expression.kubernetes.io",
			Expression: "",
		}},
		expectError: true,
	}, {
		name: "duplicate name should error",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       "test1",
			Expression: "oldObject == null",
		}, {
			Name:       "test1",
			Expression: "oldObject == null",
		}},
		expectError: true,
	}, {
		name: "name must be qualified name",
		matchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       " test1",
			Expression: "oldObject == null",
		}},
		expectError: true,
	}, {
		name:            "less than 65 match conditions should pass",
		matchConditions: repeatedMatchConditions(64),
		expectError:     false,
	}, {
		name:            "more than 64 match conditions should error",
		matchConditions: repeatedMatchConditions(65),
		expectError:     true,
	},
	}

	dryRunCreate := metav1.CreateOptions{
		DryRun: []string{metav1.DryRunAll},
	}
	endpoint := "https://localhost:1234/server"
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			rules := []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
				},
			}}
			clientConfig := admissionregistrationv1.WebhookClientConfig{
				URL:      &endpoint,
				CABundle: localhostCert,
			}
			versions := []string{"v1"}
			validatingwebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name:                    "admission.integration.test",
						Rules:                   rules,
						ClientConfig:            clientConfig,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: versions,
						MatchConditions:         testcase.matchConditions,
					},
				},
			}

			_, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), validatingwebhook, dryRunCreate)
			if testcase.expectError {
				if err == nil {
					t.Fatalf("Expected error creating ValidatingWebhookConfiguration; got nil")
				} else if !apierrors.IsInvalid(err) {
					t.Errorf("Expected Invalid error creating ValidatingWebhookConfiguration; got: %v", err)
				}
			} else if !testcase.expectError && err != nil {
				t.Fatalf("Unexpected error creating ValidatingWebhookConfiguration: %v", err)
			}

			mutatingwebhook := &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "admission.integration.test",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name:                    "admission.integration.test",
						Rules:                   rules,
						ClientConfig:            clientConfig,
						SideEffects:             &noSideEffects,
						AdmissionReviewVersions: versions,
						MatchConditions:         testcase.matchConditions,
					},
				},
			}

			_, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), mutatingwebhook, dryRunCreate)
			if testcase.expectError {
				if err == nil {
					t.Fatalf("Expected error creating MutatingWebhookConfiguration; got: nil")
				} else if !apierrors.IsInvalid(err) {
					t.Errorf("Expected Invalid error creating MutatingWebhookConfiguration; got: %v", err)
				}
			} else if !testcase.expectError && err != nil {
				t.Fatalf("Unexpected error creating MutatingWebhookConfiguration: %v", err)
			}
		})
	}
}

func matchConditionsTestPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test",
					Image: "test",
				},
			},
		},
	}
}

func newMarkerPod(namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "marker",
			Labels: map[string]string{
				"marker": "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "fake-name",
				Image: "fakeimage",
			}},
		},
	}
}

func repeatedMatchConditions(size int) []admissionregistrationv1.MatchCondition {
	matchConditions := make([]admissionregistrationv1.MatchCondition, 0, size)
	for i := 0; i < size; i++ {
		matchConditions = append(matchConditions, admissionregistrationv1.MatchCondition{
			Name:       "repeated-" + strconv.Itoa(i),
			Expression: "true",
		})
	}
	return matchConditions
}

// generate n matchConditions with provided expression
func generateMatchConditionsWithAuthzCheck(num int, exp string) []admissionregistrationv1.MatchCondition {
	var conditions = make([]admissionregistrationv1.MatchCondition, num)
	for i := 0; i < num; i++ {
		conditions[i].Name = "test" + strconv.Itoa(i)
		conditions[i].Expression = exp
	}
	return conditions
}
