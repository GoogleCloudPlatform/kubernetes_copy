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

package options

import (
	"net/http"
	"os"
	"reflect"
	"testing"

	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/server"
	openapicommon "k8s.io/kube-openapi/pkg/common"
)

func TestToAuthenticationRequestHeaderConfig(t *testing.T) {
	testCases := []struct {
		name         string
		testOptions  *RequestHeaderAuthenticationOptions
		expectConfig *authenticatorfactory.RequestHeaderConfig
	}{
		{
			name: "test when ClientCAFile is nil",
			testOptions: &RequestHeaderAuthenticationOptions{
				UsernameHeaders:     headerrequest.StaticStringSlice{"x-remote-user"},
				UIDHeaders:          headerrequest.StaticStringSlice{"x-remote-uid"},
				GroupHeaders:        headerrequest.StaticStringSlice{"x-remote-group"},
				ExtraHeaderPrefixes: headerrequest.StaticStringSlice{"x-remote-extra-"},
				AllowedNames:        headerrequest.StaticStringSlice{"kube-aggregator"},
			},
		},
		{
			name: "test when ClientCAFile is not nil",
			testOptions: &RequestHeaderAuthenticationOptions{
				ClientCAFile:        "testdata/root.pem",
				UsernameHeaders:     headerrequest.StaticStringSlice{"x-remote-user"},
				UIDHeaders:          headerrequest.StaticStringSlice{"x-remote-uid"},
				GroupHeaders:        headerrequest.StaticStringSlice{"x-remote-group"},
				ExtraHeaderPrefixes: headerrequest.StaticStringSlice{"x-remote-extra-"},
				AllowedNames:        headerrequest.StaticStringSlice{"kube-aggregator"},
			},
			expectConfig: &authenticatorfactory.RequestHeaderConfig{
				UsernameHeaders:     headerrequest.StaticStringSlice{"x-remote-user"},
				UIDHeaders:          headerrequest.StaticStringSlice{"x-remote-uid"},
				GroupHeaders:        headerrequest.StaticStringSlice{"x-remote-group"},
				ExtraHeaderPrefixes: headerrequest.StaticStringSlice{"x-remote-extra-"},
				CAContentProvider:   nil, // this is nil because you can't compare functions
				AllowedClientNames:  headerrequest.StaticStringSlice{"kube-aggregator"},
			},
		},
	}

	for _, testcase := range testCases {
		t.Run(testcase.name, func(t *testing.T) {
			resultConfig, err := testcase.testOptions.ToAuthenticationRequestHeaderConfig()
			if err != nil {
				t.Fatal(err)
			}
			if resultConfig != nil {
				if resultConfig.CAContentProvider == nil {
					t.Error("missing requestheader verify")
				}
				resultConfig.CAContentProvider = nil
			}

			if !reflect.DeepEqual(resultConfig, testcase.expectConfig) {
				t.Errorf("got RequestHeaderConfig: %#v, expected RequestHeaderConfig: %#v", resultConfig, testcase.expectConfig)
			}
		})
	}
}

func TestApplyToFallback(t *testing.T) {

	f, err := os.CreateTemp("", "authkubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if err := os.WriteFile(f.Name(), []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://localhost:56789
  name: cluster
contexts:
- context:
    cluster: cluster
  name: cluster
current-context: cluster
`), os.FileMode(0755)); err != nil {
		t.Fatal(err)
	}
	remoteKubeconfig := f.Name()

	testcases := []struct {
		name                 string
		options              *DelegatingAuthenticationOptions
		expectError          bool
		expectAuthenticator  bool
		expectTokenAnonymous bool
		expectTokenErrors    bool
	}{
		{
			name:                "empty",
			options:             nil,
			expectError:         false,
			expectAuthenticator: false,
		},
		{
			name:                "default",
			options:             NewDelegatingAuthenticationOptions(),
			expectError:         true, // in-cluster client building fails, no kubeconfig provided
			expectAuthenticator: false,
		},
		{
			name: "optional kubeconfig",
			options: func() *DelegatingAuthenticationOptions {
				opts := NewDelegatingAuthenticationOptions()
				opts.RemoteKubeConfigFileOptional = true
				return opts
			}(),
			expectError:          false, // in-cluster client building fails, no kubeconfig required
			expectAuthenticator:  true,
			expectTokenAnonymous: true, // no token validator available
		},
		{
			name: "valid client, failed cluster info lookup",
			options: func() *DelegatingAuthenticationOptions {
				opts := NewDelegatingAuthenticationOptions()
				opts.RemoteKubeConfigFile = remoteKubeconfig
				return opts
			}(),
			expectError:         true, // client building is valid, remote config lookup fails
			expectAuthenticator: false,
		},
		{
			name: "valid client, skip cluster info lookup",
			options: func() *DelegatingAuthenticationOptions {
				opts := NewDelegatingAuthenticationOptions()
				opts.RemoteKubeConfigFile = remoteKubeconfig
				opts.SkipInClusterLookup = true
				return opts
			}(),
			expectError:         false, // client building is valid, skipped cluster lookup
			expectAuthenticator: true,
			expectTokenErrors:   true, // client fails making tokenreview calls
		},
		{
			name: "valid client, tolerate failed cluster info lookup",
			options: func() *DelegatingAuthenticationOptions {
				opts := NewDelegatingAuthenticationOptions()
				opts.RemoteKubeConfigFile = remoteKubeconfig
				opts.TolerateInClusterLookupFailure = true
				return opts
			}(),
			expectError:         false, // client is valid, skipped cluster lookup
			expectAuthenticator: true,  // anonymous auth
			expectTokenErrors:   true,  // client fails making tokenreview calls
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			c := &server.AuthenticationInfo{}
			servingInfo := &server.SecureServingInfo{}
			openAPIConfig := &openapicommon.Config{}

			err := tc.options.ApplyTo(c, servingInfo, openAPIConfig)
			if (err != nil) != tc.expectError {
				t.Errorf("expected error=%v, got %v", tc.expectError, err)
			}
			if (c.Authenticator != nil) != tc.expectAuthenticator {
				t.Errorf("expected authenticator=%v, got %#v", tc.expectError, c.Authenticator)
			}
			if c.Authenticator != nil {
				{
					result, ok, err := c.Authenticator.AuthenticateRequest(&http.Request{})
					if err != nil || !ok || result == nil || result.User.GetName() != "system:anonymous" {
						t.Errorf("expected anonymous, got %#v, %#v, %#v", result, ok, err)
					}
				}
				{
					result, ok, err := c.Authenticator.AuthenticateRequest(&http.Request{Header: http.Header{"Authorization": []string{"Bearer foo"}}})
					if tc.expectTokenAnonymous {
						if err != nil || !ok || result == nil || result.User.GetName() != "system:anonymous" {
							t.Errorf("expected anonymous, got %#v, %#v, %#v", result, ok, err)
						}
					}
					if tc.expectTokenErrors != (err != nil) {
						t.Errorf("expected error=%v, got %#v, %#v, %#v", tc.expectTokenErrors, result, ok, err)
					}
				}
			}
		})
	}
}
