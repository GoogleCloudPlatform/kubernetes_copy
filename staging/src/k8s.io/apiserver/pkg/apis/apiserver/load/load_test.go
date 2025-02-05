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

package load

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	api "k8s.io/apiserver/pkg/apis/apiserver"
)

var defaultConfig = &api.AuthorizationConfiguration{}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	file, err := os.CreateTemp("", "config")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(file.Name()); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(file.Name(), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func TestLoadFromFile(t *testing.T) {
	// no file
	{
		_, err := LoadFromFile("")
		if err == nil {
			t.Fatalf("expected err: %v", err)
		}
	}

	// empty file
	{
		config, err := LoadFromFile(writeTempFile(t, ``))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(config, defaultConfig) {
			t.Fatalf("unexpected config:\n%s", cmp.Diff(defaultConfig, config))
		}
	}

	// valid file
	{
		input := `{
			"apiVersion":"apiserver.config.k8s.io/v1alpha1",
			"kind":"AuthorizationConfiguration",
			"authorizers":[{"type":"Webhook"}]}`
		expect := &api.AuthorizationConfiguration{
			Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
		}

		config, err := LoadFromFile(writeTempFile(t, input))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(config, expect) {
			t.Fatalf("unexpected config:\n%s", cmp.Diff(expect, config))
		}
	}

	// missing file
	{
		_, err := LoadFromFile(`bogus-missing-file`)
		if err == nil {
			t.Fatalf("expected err, got none")
		}
		if !strings.Contains(err.Error(), "bogus-missing-file") {
			t.Fatalf("expected missing file error, got %v", err)
		}
	}

	// invalid content file
	{
		input := `{
			"apiVersion":"apiserver.config.k8s.io/v99",
			"kind":"AuthorizationConfiguration",
			"authorizers":{"type":"Webhook"}}`

		_, err := LoadFromFile(writeTempFile(t, input))
		if err == nil {
			t.Fatalf("expected err, got none")
		}
		if !strings.Contains(err.Error(), "apiserver.config.k8s.io/v99") {
			t.Fatalf("expected apiVersion error, got %v", err)
		}
	}
}

func TestLoadFromReader(t *testing.T) {
	// no reader
	{
		config, err := LoadFromReader(nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(config, defaultConfig) {
			t.Fatalf("unexpected config:\n%s", cmp.Diff(defaultConfig, config))
		}
	}

	// empty reader
	{
		config, err := LoadFromReader(&bytes.Buffer{})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(config, defaultConfig) {
			t.Fatalf("unexpected config:\n%s", cmp.Diff(defaultConfig, config))
		}
	}

	// valid reader
	{
		input := `{
			"apiVersion":"apiserver.config.k8s.io/v1alpha1",
			"kind":"AuthorizationConfiguration",
			"authorizers":[{"type":"Webhook"}]}`
		expect := &api.AuthorizationConfiguration{
			Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
		}

		config, err := LoadFromReader(bytes.NewBufferString(input))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(config, expect) {
			t.Fatalf("unexpected config:\n%s", cmp.Diff(expect, config))
		}
	}

	// invalid reader
	{
		input := `{
			"apiVersion":"apiserver.config.k8s.io/v99",
			"kind":"AuthorizationConfiguration",
			"authorizers":[{"type":"Webhook"}]}`

		_, err := LoadFromReader(bytes.NewBufferString(input))
		if err == nil {
			t.Fatalf("expected err, got none")
		}
		if !strings.Contains(err.Error(), "apiserver.config.k8s.io/v99") {
			t.Fatalf("expected apiVersion error, got %v", err)
		}
	}
}

func TestLoadFromData(t *testing.T) {
	testcases := []struct {
		name         string
		data         []byte
		expectErr    string
		expectConfig *api.AuthorizationConfiguration
	}{
		{
			name:         "nil",
			data:         nil,
			expectConfig: defaultConfig,
		},
		{
			name:         "nil",
			data:         []byte{},
			expectConfig: defaultConfig,
		},
		{
			name: "v1alpha1 - json",
			data: []byte(`{
"apiVersion":"apiserver.config.k8s.io/v1alpha1",
"kind":"AuthorizationConfiguration",
"authorizers":[{"type":"Webhook"}]}`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
			},
		},
		{
			name: "v1alpha1 - defaults",
			data: []byte(`{
"apiVersion":"apiserver.config.k8s.io/v1alpha1",
"kind":"AuthorizationConfiguration",
"authorizers":[{"type":"Webhook","name":"default","webhook":{}}]}`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{
					Type: "Webhook",
					Name: "default",
					Webhook: &api.WebhookConfiguration{
						AuthorizedTTL:             metav1.Duration{Duration: 5 * time.Minute},
						CacheAuthorizedRequests:   true,
						UnauthorizedTTL:           metav1.Duration{Duration: 30 * time.Second},
						CacheUnauthorizedRequests: true,
					},
				}},
			},
		},
		{
			name: "v1alpha1 - yaml",
			data: []byte(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthorizationConfiguration
authorizers:
- type: Webhook
`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
			},
		},
		{
			name: "v1beta1 - json",
			data: []byte(`{
"apiVersion":"apiserver.config.k8s.io/v1beta1",
"kind":"AuthorizationConfiguration",
"authorizers":[{"type":"Webhook"}]}`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
			},
		},
		{
			name: "v1beta1 - defaults",
			data: []byte(`{
"apiVersion":"apiserver.config.k8s.io/v1beta1",
"kind":"AuthorizationConfiguration",
"authorizers":[{"type":"Webhook","name":"default","webhook":{}}]}`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{
					Type: "Webhook",
					Name: "default",
					Webhook: &api.WebhookConfiguration{
						AuthorizedTTL:             metav1.Duration{Duration: 5 * time.Minute},
						CacheAuthorizedRequests:   true,
						UnauthorizedTTL:           metav1.Duration{Duration: 30 * time.Second},
						CacheUnauthorizedRequests: true,
					},
				}},
			},
		},
		{
			name: "v1beta1 - yaml",
			data: []byte(`
apiVersion: apiserver.config.k8s.io/v1beta1
kind: AuthorizationConfiguration
authorizers:
- type: Webhook
`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
			},
		},
		{
			name: "v1 - json",
			data: []byte(`{
"apiVersion":"apiserver.config.k8s.io/v1",
"kind":"AuthorizationConfiguration",
"authorizers":[{"type":"Webhook"}]}`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
			},
		},
		{
			name: "v1 - defaults",
			data: []byte(`{
"apiVersion":"apiserver.config.k8s.io/v1",
"kind":"AuthorizationConfiguration",
"authorizers":[{"type":"Webhook","name":"default","webhook":{}}]}`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{
					Type: "Webhook",
					Name: "default",
					Webhook: &api.WebhookConfiguration{
						AuthorizedTTL:             metav1.Duration{Duration: 5 * time.Minute},
						CacheAuthorizedRequests:   true,
						UnauthorizedTTL:           metav1.Duration{Duration: 30 * time.Second},
						CacheUnauthorizedRequests: true,
					},
				}},
			},
		},
		{
			name: "v1 - yaml",
			data: []byte(`
apiVersion: apiserver.config.k8s.io/v1
kind: AuthorizationConfiguration
authorizers:
- type: Webhook
`),
			expectConfig: &api.AuthorizationConfiguration{
				Authorizers: []api.AuthorizerConfiguration{{Type: "Webhook"}},
			},
		},
		{
			name:      "missing apiVersion",
			data:      []byte(`{"kind":"AuthorizationConfiguration"}`),
			expectErr: `'apiVersion' is missing`,
		},
		{
			name:      "missing kind",
			data:      []byte(`{"apiVersion":"apiserver.config.k8s.io/v1alpha1"}`),
			expectErr: `'Kind' is missing`,
		},
		{
			name:      "unknown group",
			data:      []byte(`{"apiVersion":"apps/v1alpha1","kind":"AuthorizationConfiguration"}`),
			expectErr: `apps/v1alpha1`,
		},
		{
			name:      "unknown version",
			data:      []byte(`{"apiVersion":"apiserver.config.k8s.io/v99","kind":"AuthorizationConfiguration"}`),
			expectErr: `apiserver.config.k8s.io/v99`,
		},
		{
			name:      "unknown kind",
			data:      []byte(`{"apiVersion":"apiserver.config.k8s.io/v1alpha1","kind":"SomeConfiguration"}`),
			expectErr: `SomeConfiguration`,
		},
		{
			name: "unknown field",
			data: []byte(`{
"apiVersion":"apiserver.config.k8s.io/v1alpha1",
"kind":"AuthorizationConfiguration",
"authorzers":[{"type":"Webhook"}]}`),
			expectErr: `unknown field "authorzers"`,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			config, err := LoadFromData(tc.data)
			if err != nil {
				if len(tc.expectErr) == 0 {
					t.Fatalf("unexpected error: %v", err)
				}
				if !strings.Contains(err.Error(), tc.expectErr) {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if len(tc.expectErr) > 0 {
				t.Fatalf("expected err, got none")
			}

			if !reflect.DeepEqual(config, tc.expectConfig) {
				t.Fatalf("unexpected config:\n%s", cmp.Diff(tc.expectConfig, config))
			}
		})
	}
}
