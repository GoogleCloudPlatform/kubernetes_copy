/*
Copyright 2016 The Kubernetes Authors.

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

package imagepolicy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	utiltesting "k8s.io/client-go/util/testing"

	"k8s.io/api/imagepolicy/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	v1 "k8s.io/client-go/tools/clientcmd/api/v1"
	api "k8s.io/kubernetes/pkg/apis/core"

	"fmt"
	"os"
	"path/filepath"
	"text/template"

	_ "k8s.io/kubernetes/pkg/apis/imagepolicy/install"
)

const defaultConfigTmplJSON = `
{
"imagePolicy": {
	"kubeConfigFile": "{{ .KubeConfig }}",
	"allowTTL": {{ .AllowTTL }},
	"denyTTL": {{ .DenyTTL }},
	"retryBackoff": {{ .RetryBackoff }},
	"defaultAllow": {{ .DefaultAllow }}
}
}
`

const defaultConfigTmplYAML = `
imagePolicy:
  kubeConfigFile: "{{ .KubeConfig }}"
  allowTTL: {{ .AllowTTL }}
  denyTTL: {{ .DenyTTL }}
  retryBackoff: {{ .RetryBackoff }}
  defaultAllow: {{ .DefaultAllow }}
`

func TestNewFromConfig(t *testing.T) {
	dir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	data := struct {
		CA   string
		Cert string
		Key  string
	}{
		CA:   filepath.Join(dir, "ca.pem"),
		Cert: filepath.Join(dir, "clientcert.pem"),
		Key:  filepath.Join(dir, "clientkey.pem"),
	}

	files := []struct {
		name string
		data []byte
	}{
		{data.CA, caCert},
		{data.Cert, clientCert},
		{data.Key, clientKey},
	}
	for _, file := range files {
		if err := os.WriteFile(file.name, file.data, 0400); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		msg            string
		kubeConfigTmpl string
		wantErr        bool
	}{
		{
			msg: "a single cluster and single user",
			kubeConfigTmpl: `
clusters:
- cluster:
    certificate-authority: {{ .CA }}
    server: https://admission.example.com
  name: foobar
users:
- name: a cluster
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
`,
			wantErr: true,
		},
		{
			msg: "multiple clusters with no context",
			kubeConfigTmpl: `
clusters:
- cluster:
    certificate-authority: {{ .CA }}
    server: https://admission.example.com
  name: foobar
- cluster:
    certificate-authority: a bad certificate path
    server: https://admission.example.com
  name: barfoo
users:
- name: a name
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
`,
			wantErr: true,
		},
		{
			msg: "multiple clusters with a context",
			kubeConfigTmpl: `
clusters:
- cluster:
    certificate-authority: a bad certificate path
    server: https://admission.example.com
  name: foobar
- cluster:
    certificate-authority: {{ .CA }}
    server: https://admission.example.com
  name: barfoo
users:
- name: a name
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
contexts:
- name: default
  context:
    cluster: barfoo
    user: a name
current-context: default
`,
			wantErr: false,
		},
		{
			msg: "cluster with bad certificate path specified",
			kubeConfigTmpl: `
clusters:
- cluster:
    certificate-authority: a bad certificate path
    server: https://admission.example.com
  name: foobar
- cluster:
    certificate-authority: {{ .CA }}
    server: https://admission.example.com
  name: barfoo
users:
- name: a name
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
contexts:
- name: default
  context:
    cluster: foobar
    user: a name
current-context: default
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		t.Run(tt.msg, func(t *testing.T) {
			err := func() error {
				tempfile, err := os.CreateTemp("", "")
				if err != nil {
					return err
				}
				p := tempfile.Name()
				defer utiltesting.CloseAndRemove(t, tempfile)

				tmpl, err := template.New("test").Parse(tt.kubeConfigTmpl)
				if err != nil {
					return fmt.Errorf("failed to parse test template: %v", err)
				}
				if err := tmpl.Execute(tempfile, data); err != nil {
					return fmt.Errorf("failed to execute test template: %v", err)
				}

				tempconfigfile, err := os.CreateTemp("", "")
				if err != nil {
					return err
				}
				pc := tempconfigfile.Name()
				defer utiltesting.CloseAndRemove(t, tempconfigfile)

				configTmpl, err := template.New("testconfig").Parse(defaultConfigTmplJSON)
				if err != nil {
					return fmt.Errorf("failed to parse test template: %v", err)
				}
				dataConfig := struct {
					KubeConfig   string
					AllowTTL     int
					DenyTTL      int
					RetryBackoff int
					DefaultAllow bool
				}{
					KubeConfig:   p,
					AllowTTL:     500,
					DenyTTL:      500,
					RetryBackoff: 500,
					DefaultAllow: true,
				}
				if err := configTmpl.Execute(tempconfigfile, dataConfig); err != nil {
					return fmt.Errorf("failed to execute test template: %v", err)
				}

				// Create a new admission controller
				configFile, err := os.Open(pc)
				if err != nil {
					return fmt.Errorf("failed to read test config: %v", err)
				}
				defer configFile.Close()

				_, err = NewImagePolicyWebhook(configFile)
				return err
			}()
			if err != nil && !tt.wantErr {
				t.Errorf("failed to load plugin from config %q: %v", tt.msg, err)
			}
			if err == nil && tt.wantErr {
				t.Errorf("wanted an error when loading config, did not get one: %q", tt.msg)
			}
		})
	}
}

// Service mocks a remote service.
type Service interface {
	Review(*v1alpha1.ImageReview)
	HTTPStatusCode() int
}

// NewTestServer wraps a Service as an httptest.Server.
func NewTestServer(s Service, cert, key, caCert []byte) (*httptest.Server, error) {
	var tlsConfig *tls.Config
	if cert != nil {
		cert, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	if caCert != nil {
		rootCAs := x509.NewCertPool()
		rootCAs.AppendCertsFromPEM(caCert)
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		tlsConfig.ClientCAs = rootCAs
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	serveHTTP := func(w http.ResponseWriter, r *http.Request) {
		var review v1alpha1.ImageReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			http.Error(w, fmt.Sprintf("failed to decode body: %v", err), http.StatusBadRequest)
			return
		}
		if s.HTTPStatusCode() < 200 || s.HTTPStatusCode() >= 300 {
			http.Error(w, "HTTP Error", s.HTTPStatusCode())
			return
		}
		s.Review(&review)
		type status struct {
			Allowed          bool              `json:"allowed"`
			Reason           string            `json:"reason"`
			AuditAnnotations map[string]string `json:"auditAnnotations"`
		}
		resp := struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Status     status `json:"status"`
		}{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "ImageReview",
			Status: status{
				review.Status.Allowed,
				review.Status.Reason,
				review.Status.AuditAnnotations,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(serveHTTP))
	server.TLS = tlsConfig
	server.StartTLS()
	return server, nil
}

// A service that can be set to allow all or deny all authorization requests.
type mockService struct {
	allow          bool
	statusCode     int
	outAnnotations map[string]string
}

func (m *mockService) Review(r *v1alpha1.ImageReview) {
	r.Status.Allowed = m.allow

	// hardcoded overrides
	if r.Spec.Containers[0].Image == "good" {
		r.Status.Allowed = true
	}

	for _, c := range r.Spec.Containers {
		if c.Image == "bad" {
			r.Status.Allowed = false
		}
	}

	if !r.Status.Allowed {
		r.Status.Reason = "not allowed"
	}

	r.Status.AuditAnnotations = m.outAnnotations
}
func (m *mockService) Allow()              { m.allow = true }
func (m *mockService) Deny()               { m.allow = false }
func (m *mockService) HTTPStatusCode() int { return m.statusCode }

// newImagePolicyWebhook creates a temporary kubeconfig file from the provided arguments and attempts to load
// a new newImagePolicyWebhook from it.
func newImagePolicyWebhook(callbackURL string, clientCert, clientKey, ca []byte, cacheTime time.Duration, defaultAllow bool) (*Plugin, error) {
	tempfile, err := os.CreateTemp("", "")
	if err != nil {
		return nil, err
	}
	p := tempfile.Name()
	defer os.Remove(p)
	config := v1.Config{
		Clusters: []v1.NamedCluster{
			{
				Cluster: v1.Cluster{Server: callbackURL, CertificateAuthorityData: ca},
			},
		},
		AuthInfos: []v1.NamedAuthInfo{
			{
				AuthInfo: v1.AuthInfo{ClientCertificateData: clientCert, ClientKeyData: clientKey},
			},
		},
	}
	if err := json.NewEncoder(tempfile).Encode(config); err != nil {
		return nil, err
	}

	tempconfigfile, err := os.CreateTemp("", "")
	if err != nil {
		return nil, err
	}
	pc := tempconfigfile.Name()
	defer os.Remove(pc)

	configTmpl, err := template.New("testconfig").Parse(defaultConfigTmplYAML)
	if err != nil {
		return nil, fmt.Errorf("failed to parse test template: %v", err)
	}
	dataConfig := struct {
		KubeConfig   string
		AllowTTL     int64
		DenyTTL      int64
		RetryBackoff int64
		DefaultAllow bool
	}{
		KubeConfig:   p,
		AllowTTL:     cacheTime.Nanoseconds(),
		DenyTTL:      cacheTime.Nanoseconds(),
		RetryBackoff: 0,
		DefaultAllow: defaultAllow,
	}
	if err := configTmpl.Execute(tempconfigfile, dataConfig); err != nil {
		return nil, fmt.Errorf("failed to execute test template: %v", err)
	}

	// Create a new admission controller
	configFile, err := os.Open(pc)
	if err != nil {
		return nil, fmt.Errorf("failed to read test config: %v", err)
	}
	defer configFile.Close()
	wh, err := NewImagePolicyWebhook(configFile)
	if err != nil {
		return nil, err
	}
	return wh, err
}

func TestTLSConfig(t *testing.T) {
	tests := []struct {
		test                            string
		clientCert, clientKey, clientCA []byte
		serverCert, serverKey, serverCA []byte
		wantAllowed, wantErr            bool
	}{
		{
			test:       "TLS setup between client and server",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: caCert,
			wantAllowed: true,
		},
		{
			test:       "Server does not require client auth",
			clientCA:   caCert,
			serverCert: serverCert, serverKey: serverKey,
			wantAllowed: true,
		},
		{
			test:       "Server does not require client auth, client provides it",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey,
			wantAllowed: true,
		},
		{
			test:       "Client does not trust server",
			clientCert: clientCert, clientKey: clientKey,
			serverCert: serverCert, serverKey: serverKey,
			wantErr: true,
		},
		{
			test:       "Server does not trust client",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: badCACert,
			wantErr: true,
		},
		{
			// Plugin does not support insecure configurations.
			test:    "Server is using insecure connection",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		t.Run(tt.test, func(t *testing.T) {
			service := new(mockService)
			service.statusCode = 200

			server, err := NewTestServer(service, tt.serverCert, tt.serverKey, tt.serverCA)
			if err != nil {
				t.Errorf("%s: failed to create server: %v", tt.test, err)
				return
			}
			defer server.Close()

			wh, err := newImagePolicyWebhook(server.URL, tt.clientCert, tt.clientKey, tt.clientCA, -1, false)
			if err != nil {
				t.Errorf("%s: failed to create client: %v", tt.test, err)
				return
			}
			pod := goodPod(strconv.Itoa(rand.Intn(1000)))
			attr := admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), "namespace", "", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, &user.DefaultInfo{})

			// Allow all and see if we get an error.
			service.Allow()

			err = wh.Validate(context.TODO(), attr, nil)
			if tt.wantAllowed {
				if err != nil {
					t.Errorf("expected successful admission")
				}
			} else {
				if err == nil {
					t.Errorf("expected failed admission")
				}
			}
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error making admission request: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%s: failed to admit with AllowAll policy: %v", tt.test, err)
				return
			}

			service.Deny()
			if err := wh.Validate(context.TODO(), attr, nil); err == nil {
				t.Errorf("%s: incorrectly admitted with DenyAll policy", tt.test)
			}
		})
	}
}

type webhookCacheTestCase struct {
	statusCode         int
	expectedErr        bool
	expectedAuthorized bool
	expectedCached     bool
}

func testWebhookCacheCases(t *testing.T, serv *mockService, wh *Plugin, attr admission.Attributes, tests []webhookCacheTestCase) {
	for _, test := range tests {
		serv.statusCode = test.statusCode
		err := wh.Validate(context.TODO(), attr, nil)
		authorized := err == nil

		if test.expectedErr && err == nil {
			t.Errorf("Expected error")
		} else if !test.expectedErr && err != nil {
			t.Fatal(err)
		}
		if test.expectedAuthorized && !authorized {
			if test.expectedCached {
				t.Errorf("Webhook should have successful response cached, but authorizer reported unauthorized.")
			} else {
				t.Errorf("Webhook returned HTTP %d, but authorizer reported unauthorized.", test.statusCode)
			}
		} else if !test.expectedAuthorized && authorized {
			t.Errorf("Webhook returned HTTP %d, but authorizer reported success.", test.statusCode)
		}
	}
}

// TestWebhookCache verifies that error responses from the server are not
// cached, but successful responses are.
func TestWebhookCache(t *testing.T) {
	serv := new(mockService)
	s, err := NewTestServer(serv, serverCert, serverKey, caCert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Create an admission controller that caches successful responses.
	wh, err := newImagePolicyWebhook(s.URL, clientCert, clientKey, caCert, 200, false)
	if err != nil {
		t.Fatal(err)
	}

	tests := []webhookCacheTestCase{
		{statusCode: 500, expectedErr: true, expectedAuthorized: false, expectedCached: false},
		{statusCode: 404, expectedErr: true, expectedAuthorized: false, expectedCached: false},
		{statusCode: 403, expectedErr: true, expectedAuthorized: false, expectedCached: false},
		{statusCode: 401, expectedErr: true, expectedAuthorized: false, expectedCached: false},
		{statusCode: 200, expectedErr: false, expectedAuthorized: true, expectedCached: false},
		{statusCode: 500, expectedErr: false, expectedAuthorized: true, expectedCached: true},
	}

	attr := admission.NewAttributesRecord(goodPod("test"), nil, api.Kind("Pod").WithVersion("version"), "namespace", "", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, &user.DefaultInfo{})

	serv.allow = true

	testWebhookCacheCases(t, serv, wh, attr, tests)

	// For a different request, webhook should be called again.
	tests = []webhookCacheTestCase{
		{statusCode: 500, expectedErr: true, expectedAuthorized: false, expectedCached: false},
		{statusCode: 200, expectedErr: false, expectedAuthorized: true, expectedCached: false},
		{statusCode: 500, expectedErr: false, expectedAuthorized: true, expectedCached: true},
	}
	attr = admission.NewAttributesRecord(goodPod("test2"), nil, api.Kind("Pod").WithVersion("version"), "namespace", "", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, &user.DefaultInfo{})

	testWebhookCacheCases(t, serv, wh, attr, tests)
}

func TestContainerCombinations(t *testing.T) {
	tests := []struct {
		test                 string
		pod                  *api.Pod
		wantAllowed, wantErr bool
		subresource          string
		operation            admission.Operation
	}{
		{
			test:        "Single container allowed",
			pod:         goodPod("good"),
			wantAllowed: true,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test:        "Single container denied",
			pod:         goodPod("bad"),
			wantAllowed: false,
			wantErr:     true,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test: "One good container, one bad",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "bad",
							SecurityContext: &api.SecurityContext{},
						},
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
				},
			},
			wantAllowed: false,
			wantErr:     true,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test: "Multiple good containers",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
				},
			},
			wantAllowed: true,
			wantErr:     false,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test: "Multiple bad containers",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "bad",
							SecurityContext: &api.SecurityContext{},
						},
						{
							Image:           "bad",
							SecurityContext: &api.SecurityContext{},
						},
					},
				},
			},
			wantAllowed: false,
			wantErr:     true,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test: "Good container, bad init container",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
					InitContainers: []api.Container{
						{
							Image:           "bad",
							SecurityContext: &api.SecurityContext{},
						},
					},
				},
			},
			wantAllowed: false,
			wantErr:     true,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test: "Bad container, good init container",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "bad",
							SecurityContext: &api.SecurityContext{},
						},
					},
					InitContainers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
				},
			},
			wantAllowed: false,
			wantErr:     true,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test: "Good container, good init container",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
					InitContainers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
				},
			},
			wantAllowed: true,
			wantErr:     false,
			subresource: "",
			operation:   admission.Create,
		},
		{
			test: "Good container, good init container, bad ephemeral container when updating ephemeralcontainers subresource",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
					InitContainers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Image:           "bad",
								SecurityContext: &api.SecurityContext{},
							},
						},
					},
				},
			},
			wantAllowed: false,
			wantErr:     true,
			subresource: "ephemeralcontainers",
			operation:   admission.Update,
		},
		{
			test: "Good container, good init container, bad ephemeral container when updating subresource=='' which sets initContainer and container only",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
					InitContainers: []api.Container{
						{
							Image:           "good",
							SecurityContext: &api.SecurityContext{},
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Image:           "bad",
								SecurityContext: &api.SecurityContext{},
							},
						},
					},
				},
			},
			wantAllowed: true,
			wantErr:     false,
			subresource: "",
			operation:   admission.Update,
		},

		{
			test: "Bad container, good ephemeral container when updating subresource=='ephemeralcontainers' which sets ephemeralcontainers only",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "bad",
							SecurityContext: &api.SecurityContext{},
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Image:           "good",
								SecurityContext: &api.SecurityContext{},
							},
						},
					},
				},
			},
			wantAllowed: true,
			wantErr:     false,
			subresource: "ephemeralcontainers",
			operation:   admission.Update,
		},
		{
			test: "Good ephemeral container",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Image:           "good",
								SecurityContext: &api.SecurityContext{},
							},
						},
					},
				},
			},
			wantAllowed: true,
			wantErr:     false,
			subresource: "ephemeralcontainers",
			operation:   admission.Update,
		},
	}
	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		t.Run(tt.test, func(t *testing.T) {
			service := new(mockService)
			service.statusCode = 200

			server, err := NewTestServer(service, serverCert, serverKey, caCert)
			if err != nil {
				t.Errorf("%s: failed to create server: %v", tt.test, err)
				return
			}
			defer server.Close()

			wh, err := newImagePolicyWebhook(server.URL, clientCert, clientKey, caCert, 0, false)
			if err != nil {
				t.Errorf("%s: failed to create client: %v", tt.test, err)
				return
			}

			attr := admission.NewAttributesRecord(tt.pod, nil, api.Kind("Pod").WithVersion("version"), "namespace", "", api.Resource("pods").WithVersion("version"), tt.subresource, tt.operation, &metav1.CreateOptions{}, false, &user.DefaultInfo{})

			err = wh.Validate(context.TODO(), attr, nil)
			if tt.wantAllowed {
				if err != nil {
					t.Errorf("expected successful admission: %s", tt.test)
				}
			} else {
				if err == nil {
					t.Errorf("expected failed admission: %s", tt.test)
				}
			}
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error making admission request: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%s: failed to admit: %v", tt.test, err)
				return
			}
		})
	}
}

// fakeAttributes decorate admission.Attributes. It's used to trace the added annotations.
type fakeAttributes struct {
	admission.Attributes
	annotations map[string]string
}

func (f fakeAttributes) AddAnnotation(k, v string) error {
	f.annotations[k] = v
	return f.Attributes.AddAnnotation(k, v)
}

func TestDefaultAllow(t *testing.T) {
	tests := []struct {
		test                               string
		pod                                *api.Pod
		defaultAllow                       bool
		wantAllowed, wantErr, wantFailOpen bool
	}{
		{
			test:         "DefaultAllow = true, backend unreachable, bad image",
			pod:          goodPod("bad"),
			defaultAllow: true,
			wantAllowed:  true,
			wantFailOpen: true,
		},
		{
			test:         "DefaultAllow = true, backend unreachable, good image",
			pod:          goodPod("good"),
			defaultAllow: true,
			wantAllowed:  true,
			wantFailOpen: true,
		},
		{
			test:         "DefaultAllow = false, backend unreachable, good image",
			pod:          goodPod("good"),
			defaultAllow: false,
			wantAllowed:  false,
			wantErr:      true,
			wantFailOpen: false,
		},
		{
			test:         "DefaultAllow = false, backend unreachable, bad image",
			pod:          goodPod("bad"),
			defaultAllow: false,
			wantAllowed:  false,
			wantErr:      true,
			wantFailOpen: false,
		},
	}
	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		t.Run(tt.test, func(t *testing.T) {
			service := new(mockService)
			service.statusCode = 500

			server, err := NewTestServer(service, serverCert, serverKey, caCert)
			if err != nil {
				t.Errorf("%s: failed to create server: %v", tt.test, err)
				return
			}
			defer server.Close()

			wh, err := newImagePolicyWebhook(server.URL, clientCert, clientKey, caCert, 0, tt.defaultAllow)
			if err != nil {
				t.Errorf("%s: failed to create client: %v", tt.test, err)
				return
			}

			attr := admission.NewAttributesRecord(tt.pod, nil, api.Kind("Pod").WithVersion("version"), "namespace", "", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, &user.DefaultInfo{})
			annotations := make(map[string]string)
			attr = &fakeAttributes{attr, annotations}

			err = wh.Validate(context.TODO(), attr, nil)
			if tt.wantAllowed {
				if err != nil {
					t.Errorf("expected successful admission")
				}
			} else {
				if err == nil {
					t.Errorf("expected failed admission")
				}
			}
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error making admission request: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%s: failed to admit: %v", tt.test, err)
				return
			}
			podAnnotations := tt.pod.GetAnnotations()
			if tt.wantFailOpen {
				if podAnnotations == nil || podAnnotations[api.ImagePolicyFailedOpenKey] != "true" {
					t.Errorf("missing expected fail open pod annotation")
				}
				if annotations[AuditKeyPrefix+ImagePolicyFailedOpenKeySuffix] != "true" {
					t.Errorf("missing expected fail open attributes annotation")
				}
			} else {
				if podAnnotations != nil && podAnnotations[api.ImagePolicyFailedOpenKey] == "true" {
					t.Errorf("found unexpected fail open pod annotation")
				}
				if annotations[AuditKeyPrefix+ImagePolicyFailedOpenKeySuffix] == "true" {
					t.Errorf("found unexpected fail open attributes annotation")
				}
			}
		})
	}
}

// A service that can record annotations sent to it
type annotationService struct {
	annotations map[string]string
}

func (a *annotationService) Review(r *v1alpha1.ImageReview) {
	a.annotations = make(map[string]string)
	for k, v := range r.Spec.Annotations {
		a.annotations[k] = v
	}
	r.Status.Allowed = true
}
func (a *annotationService) HTTPStatusCode() int            { return 200 }
func (a *annotationService) Annotations() map[string]string { return a.annotations }

func TestAnnotationFiltering(t *testing.T) {
	tests := []struct {
		test           string
		annotations    map[string]string
		outAnnotations map[string]string
	}{
		{
			test: "all annotations filtered out",
			annotations: map[string]string{
				"test":    "test",
				"another": "annotation",
				"":        "",
			},
			outAnnotations: map[string]string{},
		},
		{
			test: "image-policy annotations allowed",
			annotations: map[string]string{
				"my.image-policy.k8s.io/test":     "test",
				"other.image-policy.k8s.io/test2": "annotation",
				"test":                            "test",
				"another":                         "another",
				"":                                "",
			},
			outAnnotations: map[string]string{
				"my.image-policy.k8s.io/test":     "test",
				"other.image-policy.k8s.io/test2": "annotation",
			},
		},
	}
	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		t.Run(tt.test, func(t *testing.T) {
			service := new(annotationService)

			server, err := NewTestServer(service, serverCert, serverKey, caCert)
			if err != nil {
				t.Errorf("%s: failed to create server: %v", tt.test, err)
				return
			}
			defer server.Close()

			wh, err := newImagePolicyWebhook(server.URL, clientCert, clientKey, caCert, 0, true)
			if err != nil {
				t.Errorf("%s: failed to create client: %v", tt.test, err)
				return
			}

			pod := goodPod("test")
			pod.Annotations = tt.annotations

			attr := admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), "namespace", "", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, &user.DefaultInfo{})

			err = wh.Validate(context.TODO(), attr, nil)
			if err != nil {
				t.Errorf("expected successful admission")
			}

			if !reflect.DeepEqual(tt.outAnnotations, service.Annotations()) {
				t.Errorf("expected annotations sent to webhook: %v to match expected: %v", service.Annotations(), tt.outAnnotations)
			}

		})
	}
}

func TestReturnedAnnotationAdd(t *testing.T) {
	tests := []struct {
		test                string
		pod                 *api.Pod
		verifierAnnotations map[string]string
		expectedAnnotations map[string]string
		wantErr             bool
	}{
		{
			test: "Add valid response annotations",
			pod:  goodPod("good"),
			verifierAnnotations: map[string]string{
				"foo-test": "true",
				"bar-test": "false",
			},
			expectedAnnotations: map[string]string{
				"imagepolicywebhook.image-policy.k8s.io/foo-test": "true",
				"imagepolicywebhook.image-policy.k8s.io/bar-test": "false",
			},
		},
		{
			test:                "No returned annotations are ignored",
			pod:                 goodPod("good"),
			verifierAnnotations: map[string]string{},
			expectedAnnotations: map[string]string{},
		},
		{
			test:                "Handles nil annotations",
			pod:                 goodPod("good"),
			verifierAnnotations: nil,
			expectedAnnotations: map[string]string{},
		},
		{
			test: "Adds annotations for bad request",
			pod: &api.Pod{
				Spec: api.PodSpec{
					ServiceAccountName: "default",
					SecurityContext:    &api.PodSecurityContext{},
					Containers: []api.Container{
						{
							Image:           "bad",
							SecurityContext: &api.SecurityContext{},
						},
					},
				},
			},
			verifierAnnotations: map[string]string{
				"foo-test": "false",
			},
			expectedAnnotations: map[string]string{
				"imagepolicywebhook.image-policy.k8s.io/foo-test": "false",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		t.Run(tt.test, func(t *testing.T) {
			service := new(mockService)
			service.statusCode = 200
			service.outAnnotations = tt.verifierAnnotations

			server, err := NewTestServer(service, serverCert, serverKey, caCert)
			if err != nil {
				t.Errorf("%s: failed to create server: %v", tt.test, err)
				return
			}
			defer server.Close()

			wh, err := newImagePolicyWebhook(server.URL, clientCert, clientKey, caCert, 0, true)
			if err != nil {
				t.Errorf("%s: failed to create client: %v", tt.test, err)
				return
			}

			pod := tt.pod

			attr := admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), "namespace", "", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, &user.DefaultInfo{})
			annotations := make(map[string]string)
			attr = &fakeAttributes{attr, annotations}

			err = wh.Validate(context.TODO(), attr, nil)
			if tt.wantErr {
				if err == nil {
					t.Errorf("%s: expected error making admission request: %v", tt.test, err)
				}
			} else if err != nil {
				t.Errorf("%s: failed to admit: %v", tt.test, err)
			}

			if !reflect.DeepEqual(annotations, tt.expectedAnnotations) {
				t.Errorf("got audit annotations: %v; want: %v", annotations, tt.expectedAnnotations)
			}
		})
	}
}

func goodPod(containerID string) *api.Pod {
	return &api.Pod{
		Spec: api.PodSpec{
			ServiceAccountName: "default",
			SecurityContext:    &api.PodSecurityContext{},
			Containers: []api.Container{
				{
					Image:           containerID,
					SecurityContext: &api.SecurityContext{},
				},
			},
		},
	}
}
