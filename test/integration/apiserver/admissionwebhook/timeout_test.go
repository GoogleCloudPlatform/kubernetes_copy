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

package admissionwebhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

const (
	testTimeoutClientUsername = "webhook-timeout-integration-client"
)

// TestWebhookTimeoutWithWatchCache ensures that the admission webhook timeout policy is applied correctly with the watch cache enabled.
func TestWebhookTimeoutWithWatchCache(t *testing.T) {
	testWebhookTimeout(t, true)
}

// TestWebhookTimeoutWithoutWatchCache ensures that the admission webhook timeout policy is applied correctly without the watch cache enabled.
func TestWebhookTimeoutWithoutWatchCache(t *testing.T) {
	testWebhookTimeout(t, false)
}

type invocation struct {
	path           string
	timeoutSeconds int
}

// testWebhookTimeout ensures that the admission webhook timeout policy is applied correctly.
func testWebhookTimeout(t *testing.T, watchCache bool) {
	type testWebhook struct {
		path           string
		timeoutSeconds int32
		policy         admissionregistrationv1.FailurePolicyType
		objectSelector *metav1.LabelSelector
	}

	testCases := []struct {
		name               string
		timeoutSeconds     int32
		mutatingWebhooks   []testWebhook
		validatingWebhooks []testWebhook
		expectInvocations  []invocation
		expectError        bool
		errorContainsAnyOf []string
	}{
		{
			name:           "minimum of request timeout or webhook timeout propagated",
			timeoutSeconds: 10,
			mutatingWebhooks: []testWebhook{
				{path: "/mutating/1/0s", policy: admissionregistrationv1.Fail, timeoutSeconds: 20},
				{path: "/mutating/2/0s", policy: admissionregistrationv1.Fail, timeoutSeconds: 5},
			},
			validatingWebhooks: []testWebhook{
				{path: "/validating/3/0s", policy: admissionregistrationv1.Fail, timeoutSeconds: 20},
				{path: "/validating/4/0s", policy: admissionregistrationv1.Fail, timeoutSeconds: 5},
			},
			expectInvocations: []invocation{
				{path: "/mutating/1/0s", timeoutSeconds: 10},   // from request
				{path: "/mutating/2/0s", timeoutSeconds: 5},    // from webhook config
				{path: "/validating/3/0s", timeoutSeconds: 10}, // from request
				{path: "/validating/4/0s", timeoutSeconds: 5},  // from webhook config
			},
		},
		{
			name:           "webhooks consume client timeout available, not webhook timeout",
			timeoutSeconds: 10,
			mutatingWebhooks: []testWebhook{
				{path: "/mutating/1/1s", policy: admissionregistrationv1.Fail, timeoutSeconds: 20},
				{path: "/mutating/2/1s", policy: admissionregistrationv1.Fail, timeoutSeconds: 5},
				{path: "/mutating/3/1s", policy: admissionregistrationv1.Fail, timeoutSeconds: 20},
			},
			validatingWebhooks: []testWebhook{
				{path: "/validating/4/1s", policy: admissionregistrationv1.Fail, timeoutSeconds: 5},
				{path: "/validating/5/1s", policy: admissionregistrationv1.Fail, timeoutSeconds: 10},
				{path: "/validating/6/1s", policy: admissionregistrationv1.Fail, timeoutSeconds: 20},
			},
			expectInvocations: []invocation{
				{path: "/mutating/1/1s", timeoutSeconds: 10},  // from request
				{path: "/mutating/2/1s", timeoutSeconds: 5},   // from webhook config (less than request - 1s consumed)
				{path: "/mutating/3/1s", timeoutSeconds: 8},   // from request - 2s consumed
				{path: "/validating/4/1s", timeoutSeconds: 5}, // from webhook config (less than request - 3s consumed by mutating)
				{path: "/validating/5/1s", timeoutSeconds: 7}, // from request - 3s consumed by mutating
				{path: "/validating/6/1s", timeoutSeconds: 7}, // from request - 3s consumed by mutating
			},
		},
		{
			name:           "timed out client requests skip later mutating webhooks (regardless of failure policy) and fail",
			timeoutSeconds: 3,
			mutatingWebhooks: []testWebhook{
				{path: "/mutating/1/5s", policy: admissionregistrationv1.Ignore, timeoutSeconds: 4},
				{path: "/mutating/2/1s", policy: admissionregistrationv1.Ignore, timeoutSeconds: 5},
				{path: "/mutating/3/1s", policy: admissionregistrationv1.Ignore, timeoutSeconds: 5},
			},
			expectInvocations: []invocation{
				{path: "/mutating/1/5s", timeoutSeconds: 3}, // from request
			},
			expectError: true,
			errorContainsAnyOf: []string{
				// refer to https://github.com/kubernetes/kubernetes/issues/98606#issuecomment-774832633
				// for the reason for triggering this scenario
				"stream error",
				"the server was unable to return a response in the time allotted",
				"request did not complete within requested timeout",
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

	recorder := &timeoutRecorder{invocations: []invocation{}, markers: sets.NewString()}
	webhookServer := httptest.NewUnstartedServer(newTimeoutWebhookHandler(recorder))
	webhookServer.TLS = &tls.Config{

		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
	}
	webhookServer.StartTLS()
	defer webhookServer.Close()

	s := kubeapiservertesting.StartTestServerOrDie(t, kubeapiservertesting.NewDefaultTestServerOptions(), []string{
		"--disable-admission-plugins=ServiceAccount",
		fmt.Sprintf("--watch-cache=%v", watchCache),
	}, framework.SharedEtcd())
	defer s.TearDownFn()

	// Configure a client with a distinct user name so that it is easy to distinguish requests
	// made by the client from requests made by controllers. We use this to filter out requests
	// before recording them to ensure we don't accidentally mistake requests from controllers
	// as requests made by the client.
	clientConfig := rest.CopyConfig(s.ClientConfig)
	clientConfig.Timeout = 0 // no timeout, we want to set this manually
	clientConfig.Impersonate.UserName = testTimeoutClientUsername
	clientConfig.Impersonate.Groups = []string{"system:masters", "system:authenticated"}
	client, err := clientset.NewForConfig(clientConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = client.CoreV1().Pods("default").Create(context.TODO(), timeoutMarkerFixture, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for i, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			recorder.Reset()
			ns := fmt.Sprintf("reinvoke-%d", i)
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			mutatingWebhooks := []admissionregistrationv1.MutatingWebhook{}
			for j, webhook := range tt.mutatingWebhooks {
				name := fmt.Sprintf("admission.integration.test.%d.%s", j, strings.Replace(strings.TrimPrefix(webhook.path, "/"), "/", "-", -1))
				endpoint := webhookServer.URL + webhook.path
				mutatingWebhooks = append(mutatingWebhooks, admissionregistrationv1.MutatingWebhook{
					Name: name,
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						URL:      &endpoint,
						CABundle: localhostCert,
					},
					Rules: []admissionregistrationv1.RuleWithOperations{{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
					}},
					ObjectSelector:          webhook.objectSelector,
					FailurePolicy:           &tt.mutatingWebhooks[j].policy,
					TimeoutSeconds:          &tt.mutatingWebhooks[j].timeoutSeconds,
					AdmissionReviewVersions: []string{"v1beta1"},
					SideEffects:             &noSideEffects,
				})
			}
			mutatingCfg, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("admission.integration.test-%d", i)},
				Webhooks:   mutatingWebhooks,
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.TODO(), mutatingCfg.GetName(), metav1.DeleteOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}()

			validatingWebhooks := []admissionregistrationv1.ValidatingWebhook{}
			for j, webhook := range tt.validatingWebhooks {
				name := fmt.Sprintf("admission.integration.test.%d.%s", j, strings.Replace(strings.TrimPrefix(webhook.path, "/"), "/", "-", -1))
				endpoint := webhookServer.URL + webhook.path
				validatingWebhooks = append(validatingWebhooks, admissionregistrationv1.ValidatingWebhook{
					Name: name,
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						URL:      &endpoint,
						CABundle: localhostCert,
					},
					Rules: []admissionregistrationv1.RuleWithOperations{{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
					}},
					ObjectSelector:          webhook.objectSelector,
					FailurePolicy:           &tt.validatingWebhooks[j].policy,
					TimeoutSeconds:          &tt.validatingWebhooks[j].timeoutSeconds,
					AdmissionReviewVersions: []string{"v1beta1"},
					SideEffects:             &noSideEffects,
				})
			}
			validatingCfg, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("admission.integration.test-%d", i)},
				Webhooks:   validatingWebhooks,
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), validatingCfg.GetName(), metav1.DeleteOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}()

			// wait until new webhook is called the first time
			if err := wait.PollImmediate(time.Millisecond*5, wait.ForeverTestTimeout, func() (bool, error) {
				_, err = client.CoreV1().Pods("default").Patch(context.TODO(), timeoutMarkerFixture.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
				received := recorder.MarkerReceived()
				if len(tt.mutatingWebhooks) > 0 && !received.Has("mutating") {
					t.Logf("Waiting for mutating webhooks to become effective, getting marker object: %v", err)
					return false, nil
				}
				if len(tt.validatingWebhooks) > 0 && !received.Has("validating") {
					t.Logf("Waiting for validating webhooks to become effective, getting marker object: %v", err)
					return false, nil
				}
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}

			pod := &corev1.Pod{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      "labeled",
					Labels:    map[string]string{"x": "true"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "fake-name",
						Image: "fakeimage",
					}},
				},
			}

			body, err := json.Marshal(pod)
			if err != nil {
				t.Fatal(err)
			}

			// set the timeout parameter manually so we don't actually cut off the request client-side, and wait for the server response
			err = client.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Body(body).Param("timeout", fmt.Sprintf("%ds", tt.timeoutSeconds)).Do(context.TODO()).Error()
			// _, err = testClient.CoreV1().Pods(ns).Create(pod)

			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error but got none")
				}

				expected := false
				if len(tt.errorContainsAnyOf) != 0 {
					for _, errStr := range tt.errorContainsAnyOf {
						if strings.Contains(err.Error(), errStr) {
							expected = true
							break
						}
					}
				}
				if !expected {
					t.Errorf("expected the error to be any of %q, but got: %v", tt.errorContainsAnyOf, err)
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if tt.expectInvocations != nil {
				for i, invocation := range tt.expectInvocations {
					if len(recorder.invocations) <= i {
						t.Errorf("expected invocation of %s, got none", invocation.path)
						continue
					}

					if recorder.invocations[i].path != invocation.path {
						t.Errorf("expected invocation of %s, got %s", invocation.path, recorder.invocations[i].path)
						continue
					}
					if recorder.invocations[i].timeoutSeconds != invocation.timeoutSeconds {
						t.Errorf("expected invocation of %s with timeout %d, got %d", invocation.path, invocation.timeoutSeconds, recorder.invocations[i].timeoutSeconds)
						continue
					}
				}

				if len(recorder.invocations) > len(tt.expectInvocations) {
					for _, invocation := range recorder.invocations[len(tt.expectInvocations):] {
						t.Errorf("unexpected invocation of %s", invocation.path)
					}
				}
			}
		})
	}
}

type timeoutRecorder struct {
	mu          sync.Mutex
	markers     sets.String
	invocations []invocation
}

// Reset zeros out all counts
func (i *timeoutRecorder) Reset() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.invocations = []invocation{}
	i.markers = sets.NewString()
}

// MarkerReceived records the specified markers were received and returns the set of received markers
func (i *timeoutRecorder) MarkerReceived(markers ...string) sets.String {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.markers.Insert(markers...)
	return i.markers.Union(nil)
}

func (i *timeoutRecorder) RecordInvocation(call invocation) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.invocations = append(i.invocations, call)
	sort.SliceStable(i.invocations, func(a, b int) bool {
		aValidating := strings.Contains(i.invocations[a].path, "validating")
		bValidating := strings.Contains(i.invocations[b].path, "validating")
		switch {
		case aValidating && bValidating:
			// sort validating by path
			return strings.Compare(i.invocations[a].path, i.invocations[b].path) < 0
		case !aValidating && !bValidating:
			// keep mutating in original order
			return a < b
		case aValidating && !bValidating:
			// put validating last
			return false
		default:
			return true
		}
	})
}

func newTimeoutWebhookHandler(recorder *timeoutRecorder) http.Handler {
	allow := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(&v1beta1.AdmissionReview{
			Response: &v1beta1.AdmissionResponse{
				Allowed: true,
			},
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
		}
		review := v1beta1.AdmissionReview{}
		if err := json.Unmarshal(data, &review); err != nil {
			http.Error(w, err.Error(), 400)
		}
		if review.Request.UserInfo.Username != testTimeoutClientUsername {
			// skip requests not originating from this integration test's client
			allow(w)
			return
		}

		if len(review.Request.Object.Raw) == 0 {
			http.Error(w, err.Error(), 400)
		}
		pod := &corev1.Pod{}
		if err := json.Unmarshal(review.Request.Object.Raw, pod); err != nil {
			http.Error(w, err.Error(), 400)
		}

		// When resetting between tests, a marker object is patched until this webhook
		// observes it, at which point it is considered ready.
		if pod.Namespace == timeoutMarkerFixture.Namespace && pod.Name == timeoutMarkerFixture.Name {
			if strings.HasPrefix(r.URL.Path, "/mutating/") {
				recorder.MarkerReceived("mutating")
			}
			if strings.HasPrefix(r.URL.Path, "/validating/") {
				recorder.MarkerReceived("validating")
			}
			allow(w)
			return
		}

		timeout, err := time.ParseDuration(r.URL.Query().Get("timeout"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		invocation := invocation{path: r.URL.Path, timeoutSeconds: int(timeout.Round(time.Second) / time.Second)}
		recorder.RecordInvocation(invocation)

		switch {
		case strings.HasSuffix(r.URL.Path, "/0s"):
			allow(w)
		case strings.HasSuffix(r.URL.Path, "/1s"):
			time.Sleep(time.Second)
			allow(w)
		case strings.HasSuffix(r.URL.Path, "/5s"):
			time.Sleep(5 * time.Second)
			allow(w)
		default:
			http.NotFound(w, r)
		}
	})
}

var timeoutMarkerFixture = &corev1.Pod{
	ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      "marker",
	},
	Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "fake-name",
			Image: "fakeimage",
		}},
	},
}
