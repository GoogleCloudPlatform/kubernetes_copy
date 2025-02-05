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
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	kubeauthenticator "k8s.io/kubernetes/pkg/kubeapiserver/authenticator"
	"k8s.io/kubernetes/pkg/serviceaccount"

	"k8s.io/utils/pointer"
)

func TestAuthenticationValidate(t *testing.T) {
	testCases := []struct {
		name                              string
		testAnonymous                     *AnonymousAuthenticationOptions
		testOIDC                          *OIDCAuthenticationOptions
		testSA                            *ServiceAccountAuthenticationOptions
		testWebHook                       *WebHookAuthenticationOptions
		testAuthenticationConfigFile      string
		expectErr                         string
		enabledFeatures, disabledFeatures []featuregate.Feature
	}{
		{
			name: "test when OIDC and ServiceAccounts are nil",
		},
		{
			name: "test when OIDC and ServiceAccounts are valid",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				Issuers:  []string{"http://foo.bar.com"},
				KeyFiles: []string{"testkeyfile1", "testkeyfile2"},
			},
		},
		{
			name: "test when OIDC is invalid",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				Issuers:  []string{"http://foo.bar.com"},
				KeyFiles: []string{"testkeyfile1", "testkeyfile2"},
			},
			expectErr: "oidc-issuer-url and oidc-client-id must be specified together when any oidc-* flags are set",
		},
		{
			name: "test when ServiceAccounts doesn't have key file",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				Issuers: []string{"http://foo.bar.com"},
			},
			expectErr: "either `--service-account-key-file` or `--service-account-signing-endpoint` must be set",
		},
		{
			name: "test when ServiceAccounts doesn't have issuer",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				Issuers: []string{},
			},
			expectErr: "service-account-issuer is a required flag",
		},
		{
			name: "test when ServiceAccounts has empty string as issuer",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				Issuers: []string{""},
			},
			expectErr: "service-account-issuer should not be an empty string",
		},
		{
			name: "test when ServiceAccounts has duplicate issuers",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				Issuers: []string{"http://foo.bar.com", "http://foo.bar.com"},
			},
			expectErr: "service-account-issuer \"http://foo.bar.com\" is already specified",
		},
		{
			name: "test when ServiceAccount has bad issuer",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				Issuers: []string{"http://[::1]:namedport"},
			},
			expectErr: "service-account-issuer \"http://[::1]:namedport\" contained a ':' but was not a valid URL",
		},
		{
			name: "test when ServiceAccounts has invalid JWKSURI",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				KeyFiles: []string{"cert", "key"},
				Issuers:  []string{"http://foo.bar.com"},
				JWKSURI:  "https://host:port",
			},
			expectErr: "service-account-jwks-uri must be a valid URL: parse \"https://host:port\": invalid port \":port\" after host",
		},
		{
			name: "test when ServiceAccounts has invalid JWKSURI (not https scheme)",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				KeyFiles: []string{"cert", "key"},
				Issuers:  []string{"http://foo.bar.com"},
				JWKSURI:  "http://baz.com",
			},
			expectErr: "service-account-jwks-uri requires https scheme, parsed as: http://baz.com",
		},
		{
			name: "test when WebHook has invalid retry attempts",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			testSA: &ServiceAccountAuthenticationOptions{
				KeyFiles: []string{"cert", "key"},
				Issuers:  []string{"http://foo.bar.com"},
				JWKSURI:  "https://baz.com",
			},
			testWebHook: &WebHookAuthenticationOptions{
				ConfigFile: "configfile",
				Version:    "v1",
				CacheTTL:   60 * time.Second,
				RetryBackoff: &wait.Backoff{
					Duration: 500 * time.Millisecond,
					Factor:   1.5,
					Jitter:   0.2,
					Steps:    0,
				},
			},
			expectErr: "number of webhook retry attempts must be greater than 0, but is: 0",
		},
		{
			name:                         "test when authentication config file is set (feature gate enabled by default)",
			testAuthenticationConfigFile: "configfile",
			expectErr:                    "",
		},
		{
			name:                         "test when authentication config file and oidc-* flags are set",
			testAuthenticationConfigFile: "configfile",
			testOIDC: &OIDCAuthenticationOptions{
				UsernameClaim:      "sub",
				SigningAlgs:        []string{"RS256"},
				IssuerURL:          "https://testIssuerURL",
				ClientID:           "testClientID",
				areFlagsConfigured: func() bool { return true },
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name:                         "test when authentication config file and anonymous-auth flags are set AnonymousAuthConfigurableEndpoints disabled",
			disabledFeatures:             []featuregate.Feature{features.AnonymousAuthConfigurableEndpoints},
			testAuthenticationConfigFile: "configfile",
			testAnonymous: &AnonymousAuthenticationOptions{
				Allow:       true,
				areFlagsSet: func() bool { return true },
			},
		},
	}

	for _, testcase := range testCases {
		t.Run(testcase.name, func(t *testing.T) {
			options := NewBuiltInAuthenticationOptions()
			options.Anonymous = testcase.testAnonymous
			options.OIDC = testcase.testOIDC
			options.ServiceAccounts = testcase.testSA
			options.WebHook = testcase.testWebHook
			options.AuthenticationConfigFile = testcase.testAuthenticationConfigFile
			for _, f := range testcase.enabledFeatures {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, f, true)
			}
			for _, f := range testcase.disabledFeatures {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, f, false)
			}
			errs := options.Validate()
			if len(errs) > 0 && (!strings.Contains(utilerrors.NewAggregate(errs).Error(), testcase.expectErr) || testcase.expectErr == "") {
				t.Errorf("Got err: %v, Expected err: %s", errs, testcase.expectErr)
			}
			if len(errs) == 0 && len(testcase.expectErr) != 0 {
				t.Errorf("Got err nil, Expected err: %s", testcase.expectErr)
			}
		})
	}
}

func TestToAuthenticationConfig(t *testing.T) {
	testOptions := &BuiltInAuthenticationOptions{
		Anonymous: &AnonymousAuthenticationOptions{
			Allow: false,
		},
		ClientCert: &apiserveroptions.ClientCertAuthenticationOptions{
			ClientCA: "testdata/root.pem",
		},
		WebHook: &WebHookAuthenticationOptions{
			CacheTTL:   180000000000,
			ConfigFile: "/token-webhook-config",
		},
		BootstrapToken: &BootstrapTokenAuthenticationOptions{
			Enable: false,
		},
		OIDC: &OIDCAuthenticationOptions{
			CAFile:        "testdata/root.pem",
			UsernameClaim: "sub",
			SigningAlgs:   []string{"RS256"},
			IssuerURL:     "https://testIssuerURL",
			ClientID:      "testClientID",
		},
		RequestHeader: &apiserveroptions.RequestHeaderAuthenticationOptions{
			UsernameHeaders:     []string{"x-remote-user"},
			UIDHeaders:          []string{"x-remote-uid"},
			GroupHeaders:        []string{"x-remote-group"},
			ExtraHeaderPrefixes: []string{"x-remote-extra-"},
			ClientCAFile:        "testdata/root.pem",
			AllowedNames:        []string{"kube-aggregator"},
		},
		ServiceAccounts: &ServiceAccountAuthenticationOptions{
			Lookup:  true,
			Issuers: []string{"http://foo.bar.com"},
		},
		TokenFile: &TokenFileAuthenticationOptions{
			TokenFile: "/testTokenFile",
		},
		TokenSuccessCacheTTL: 10 * time.Second,
		TokenFailureCacheTTL: 0,
	}

	expectConfig := kubeauthenticator.Config{
		APIAudiences:            authenticator.Audiences{"http://foo.bar.com"},
		Anonymous:               apiserver.AnonymousAuthConfig{Enabled: false},
		BootstrapToken:          false,
		ClientCAContentProvider: nil, // this is nil because you can't compare functions
		TokenAuthFile:           "/testTokenFile",
		AuthenticationConfig: &apiserver.AuthenticationConfiguration{
			JWT: []apiserver.JWTAuthenticator{
				{
					Issuer: apiserver.Issuer{
						URL:       "https://testIssuerURL",
						Audiences: []string{"testClientID"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "sub",
							Prefix: pointer.String("https://testIssuerURL#"),
						},
					},
				},
			},
		},
		OIDCSigningAlgs:             []string{"RS256"},
		ServiceAccountLookup:        true,
		ServiceAccountIssuers:       []string{"http://foo.bar.com"},
		WebhookTokenAuthnConfigFile: "/token-webhook-config",
		WebhookTokenAuthnCacheTTL:   180000000000,

		TokenSuccessCacheTTL: 10 * time.Second,
		TokenFailureCacheTTL: 0,

		RequestHeaderConfig: &authenticatorfactory.RequestHeaderConfig{
			UsernameHeaders:     headerrequest.StaticStringSlice{"x-remote-user"},
			UIDHeaders:          headerrequest.StaticStringSlice{"x-remote-uid"},
			GroupHeaders:        headerrequest.StaticStringSlice{"x-remote-group"},
			ExtraHeaderPrefixes: headerrequest.StaticStringSlice{"x-remote-extra-"},
			CAContentProvider:   nil, // this is nil because you can't compare functions
			AllowedClientNames:  headerrequest.StaticStringSlice{"kube-aggregator"},
		},
	}

	fileBytes, err := os.ReadFile("testdata/root.pem")
	if err != nil {
		t.Fatal(err)
	}
	expectConfig.AuthenticationConfig.JWT[0].Issuer.CertificateAuthority = string(fileBytes)

	resultConfig, err := testOptions.ToAuthenticationConfig()
	if err != nil {
		t.Fatal(err)
	}

	// nil these out because you cannot compare pointers.  Ensure they are non-nil first
	if resultConfig.ClientCAContentProvider == nil {
		t.Error("missing client verify")
	}
	if resultConfig.RequestHeaderConfig.CAContentProvider == nil {
		t.Error("missing requestheader verify")
	}
	resultConfig.ClientCAContentProvider = nil
	resultConfig.RequestHeaderConfig.CAContentProvider = nil

	if !reflect.DeepEqual(resultConfig, expectConfig) {
		t.Error(cmp.Diff(resultConfig, expectConfig))
	}
}

func TestBuiltInAuthenticationOptionsAddFlags(t *testing.T) {
	var args = []string{
		"--api-audiences=foo",
		"--anonymous-auth=true",
		"--enable-bootstrap-token-auth=true",
		"--oidc-issuer-url=https://baz.com",
		"--oidc-client-id=client-id",
		"--oidc-ca-file=cert",
		"--oidc-username-prefix=-",
		"--client-ca-file=client-cacert",
		"--requestheader-client-ca-file=testdata/root.pem",
		"--requestheader-username-headers=x-remote-user-custom",
		"--requestheader-uid-headers=x-remote-uid-custom",
		"--requestheader-group-headers=x-remote-group-custom",
		"--requestheader-allowed-names=kube-aggregator",
		"--service-account-key-file=cert",
		"--service-account-key-file=key",
		"--service-account-issuer=http://foo.bar.com",
		"--service-account-jwks-uri=https://qux.com",
		"--token-auth-file=tokenfile",
		"--authentication-token-webhook-config-file=webhook_config.yaml",
		"--authentication-token-webhook-cache-ttl=180s",
	}

	expected := &BuiltInAuthenticationOptions{
		APIAudiences: []string{"foo"},
		Anonymous: &AnonymousAuthenticationOptions{
			Allow: true,
		},
		BootstrapToken: &BootstrapTokenAuthenticationOptions{
			Enable: true,
		},
		ClientCert: &apiserveroptions.ClientCertAuthenticationOptions{
			ClientCA: "client-cacert",
		},
		OIDC: &OIDCAuthenticationOptions{
			CAFile:         "cert",
			ClientID:       "client-id",
			IssuerURL:      "https://baz.com",
			UsernameClaim:  "sub",
			UsernamePrefix: "-",
			SigningAlgs:    []string{"RS256"},
		},
		RequestHeader: &apiserveroptions.RequestHeaderAuthenticationOptions{
			ClientCAFile:    "testdata/root.pem",
			UsernameHeaders: []string{"x-remote-user-custom"},
			UIDHeaders:      []string{"x-remote-uid-custom"},
			GroupHeaders:    []string{"x-remote-group-custom"},
			AllowedNames:    []string{"kube-aggregator"},
		},
		ServiceAccounts: &ServiceAccountAuthenticationOptions{
			KeyFiles:              []string{"cert", "key"},
			Lookup:                true,
			Issuers:               []string{"http://foo.bar.com"},
			JWKSURI:               "https://qux.com",
			ExtendExpiration:      true,
			MaxExtendedExpiration: serviceaccount.ExpirationExtensionSeconds * time.Second,
		},
		TokenFile: &TokenFileAuthenticationOptions{
			TokenFile: "tokenfile",
		},
		WebHook: &WebHookAuthenticationOptions{
			ConfigFile: "webhook_config.yaml",
			Version:    "v1beta1",
			CacheTTL:   180 * time.Second,
			RetryBackoff: &wait.Backoff{
				Duration: 500 * time.Millisecond,
				Factor:   1.5,
				Jitter:   0.2,
				Steps:    5,
			},
		},
		TokenSuccessCacheTTL: 10 * time.Second,
		TokenFailureCacheTTL: 0 * time.Second,
	}

	opts := NewBuiltInAuthenticationOptions().WithAll()
	pf := pflag.NewFlagSet("test-builtin-authentication-opts", pflag.ContinueOnError)
	opts.AddFlags(pf)

	if err := pf.Parse(args); err != nil {
		t.Fatal(err)
	}

	if !opts.OIDC.areFlagsConfigured() {
		t.Fatal("OIDC flags should be configured")
	}
	// nil these out because you cannot compare functions
	opts.OIDC.areFlagsConfigured = nil

	if !opts.Anonymous.areFlagsSet() {
		t.Fatalf("Anonymous flags should be configured")
	}

	// nil these out because you cannot compare functions
	opts.Anonymous.areFlagsSet = nil

	if !reflect.DeepEqual(opts, expected) {
		t.Error(cmp.Diff(opts, expected, cmp.AllowUnexported(OIDCAuthenticationOptions{}, AnonymousAuthenticationOptions{})))
	}
}

func TestWithTokenGetterFunction(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	versionedInformer := informers.NewSharedInformerFactory(fakeClientset, 0)
	{
		var called bool
		f := func(factory informers.SharedInformerFactory) serviceaccount.ServiceAccountTokenGetter {
			called = true
			return nil
		}
		opts := NewBuiltInAuthenticationOptions().WithServiceAccounts()
		opts.ServiceAccounts.OptionalTokenGetter = f
		err := opts.ApplyTo(context.Background(), &genericapiserver.AuthenticationInfo{}, nil, nil, &openapicommon.Config{}, nil, fakeClientset, versionedInformer, "")
		if err != nil {
			t.Fatal(err)
		}

		if !called {
			t.Fatal("expected token getter function to be called")
		}
	}
	{
		opts := NewBuiltInAuthenticationOptions().WithServiceAccounts()
		err := opts.ApplyTo(context.Background(), &genericapiserver.AuthenticationInfo{}, nil, nil, &openapicommon.Config{}, nil, fakeClientset, versionedInformer, "")
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestToAuthenticationConfig_Anonymous(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)
	testCases := []struct {
		name                     string
		args                     []string
		expectConfig             kubeauthenticator.Config
		enableAnonymousEndpoints bool
		expectErr                string
	}{
		{
			name: "flag-none",
			args: []string{},
			expectConfig: kubeauthenticator.Config{
				Anonymous:            apiserver.AnonymousAuthConfig{Enabled: true},
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{},
				TokenSuccessCacheTTL: 10 * time.Second,
			},
		},
		{
			name: "flag-anonymous-enabled",
			args: []string{"--anonymous-auth=True"},
			expectConfig: kubeauthenticator.Config{
				Anonymous:            apiserver.AnonymousAuthConfig{Enabled: true},
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{},
				TokenSuccessCacheTTL: 10 * time.Second,
			},
		},
		{
			name: "flag-anonymous-disabled",
			args: []string{"--anonymous-auth=False"},
			expectConfig: kubeauthenticator.Config{
				Anonymous:            apiserver.AnonymousAuthConfig{Enabled: false},
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{},
				TokenSuccessCacheTTL: 10 * time.Second,
			},
		},
		{
			name: "file-anonymous-disabled-AnonymousAuthConfigurableEndpoints-disabled",
			args: []string{
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: false
`),
			},
			expectErr: "anonymous is not supported when AnonymousAuthConfigurableEndpoints feature gate is disabled",
		},
		{
			name:                     "file-anonymous-disabled-AnonymousAuthConfigurableEndpoints-enabled",
			enableAnonymousEndpoints: true,
			args: []string{
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: false
`),
			},
			expectConfig: kubeauthenticator.Config{
				Anonymous:            apiserver.AnonymousAuthConfig{Enabled: false},
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					Anonymous: &apiserver.AnonymousAuthConfig{Enabled: false},
				},
				AuthenticationConfigData: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: false
`,
				OIDCSigningAlgs: []string{"ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "RS256", "RS384", "RS512"},
			},
		},
		{
			name:                     "file-anonymous-enabled-AnonymousAuthConfigurableEndpoints-enabled",
			enableAnonymousEndpoints: true,
			args: []string{
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: true
`),
			},
			expectConfig: kubeauthenticator.Config{
				Anonymous:            apiserver.AnonymousAuthConfig{Enabled: true},
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					Anonymous: &apiserver.AnonymousAuthConfig{Enabled: true},
				},
				AuthenticationConfigData: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: true
`,
				OIDCSigningAlgs: []string{"ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "RS256", "RS384", "RS512"},
			},
		},
		{
			name:                     "file-anonymous-disabled-with-conditions-AnonymousAuthConfigurableEndpoints-enabled",
			enableAnonymousEndpoints: true,
			args: []string{
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: false
  conditions:
  - path: "/livez"
`),
			},
			expectErr: "enabled should be set to true when conditions are defined",
		},
		{
			name:                     "file-anonymous-missing-with-conditions-AnonymousAuthConfigurableEndpoints-enabled",
			enableAnonymousEndpoints: true,
			args: []string{
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  conditions:
  - path: "/livez"
`),
			},
			expectErr: "enabled should be set to true when conditions are defined",
		},
		{
			name:                     "file-anonymous-enabled-with-conditions-AnonymousAuthConfigurableEndpoints-enabled",
			enableAnonymousEndpoints: true,
			args: []string{
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: true
  conditions:
  - path: "/livez"
`),
			},
			expectConfig: kubeauthenticator.Config{
				Anonymous: apiserver.AnonymousAuthConfig{
					Enabled: true,
					Conditions: []apiserver.AnonymousAuthCondition{
						{
							Path: "/livez",
						},
					},
				},
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					Anonymous: &apiserver.AnonymousAuthConfig{
						Enabled: true,
						Conditions: []apiserver.AnonymousAuthCondition{
							{
								Path: "/livez",
							},
						},
					},
				},
				AuthenticationConfigData: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: true
  conditions:
  - path: "/livez"
`,
				OIDCSigningAlgs: []string{"ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "RS256", "RS384", "RS512"},
			},
		},
		{
			name:                     "flag-anonymous-enabled-file-anonymous-enabled-AnonymousAuthConfigurableEndpoints-enabled",
			enableAnonymousEndpoints: true,
			args: []string{"--anonymous-auth=True",
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
anonymous:
  enabled: true
`),
			},
			expectErr: "--anonynous-auth flag cannot be set when anonymous field is configured in authentication configuration file",
		},
		{
			name:                     "flag-anonymous-enabled-file-anonymous-notset-AnonymousAuthConfigurableEndpoints-enabled",
			enableAnonymousEndpoints: true,
			args: []string{"--anonymous-auth=True",
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
    audiences: [ "🐼" ]
  claimMappings:
    username:
      claim: sub
      prefix: ""
`),
			},
			expectConfig: kubeauthenticator.Config{
				TokenSuccessCacheTTL: 10 * time.Second,
				Anonymous:            apiserver.AnonymousAuthConfig{Enabled: true},
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					JWT: []apiserver.JWTAuthenticator{
						{
							Issuer: apiserver.Issuer{
								URL:       "https://test-issuer",
								Audiences: []string{"🐼"},
							},
							ClaimMappings: apiserver.ClaimMappings{
								Username: apiserver.PrefixedClaimOrExpression{
									Claim:  "sub",
									Prefix: pointer.String(""),
								},
							},
						},
					},
				},
				AuthenticationConfigData: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
    audiences: [ "🐼" ]
  claimMappings:
    username:
      claim: sub
      prefix: ""
`,
				OIDCSigningAlgs: []string{"ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "RS256", "RS384", "RS512"},
			},
		},
	}

	for _, testcase := range testCases {
		t.Run(testcase.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AnonymousAuthConfigurableEndpoints, testcase.enableAnonymousEndpoints)
			opts := NewBuiltInAuthenticationOptions().WithAnonymous()
			pf := pflag.NewFlagSet("test-builtin-authentication-opts", pflag.ContinueOnError)
			opts.AddFlags(pf)

			if err := pf.Parse(testcase.args); err != nil {
				t.Fatal(err)
			}

			resultConfig, err := opts.ToAuthenticationConfig()

			if testcase.expectErr != "" {
				if err == nil {
					t.Fatalf("Got err: %v; Want err: %v", err, testcase.expectErr)
				}

				if !strings.Contains(err.Error(), testcase.expectErr) {
					t.Fatalf("Got err: %v; Want err: %v", err, testcase.expectErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(resultConfig, testcase.expectConfig) {
				t.Error(cmp.Diff(resultConfig, testcase.expectConfig))
			}
		})
	}
}

func TestToAuthenticationConfig_OIDC(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)

	testCases := []struct {
		name         string
		args         []string
		expectConfig kubeauthenticator.Config
	}{
		{
			name: "username prefix is '-'",
			args: []string{
				"--oidc-issuer-url=https://testIssuerURL",
				"--oidc-client-id=testClientID",
				"--oidc-username-claim=sub",
				"--oidc-username-prefix=-",
				"--oidc-signing-algs=RS256",
				"--oidc-required-claim=foo=bar",
			},
			expectConfig: kubeauthenticator.Config{
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					JWT: []apiserver.JWTAuthenticator{
						{
							Issuer: apiserver.Issuer{
								URL:       "https://testIssuerURL",
								Audiences: []string{"testClientID"},
							},
							ClaimMappings: apiserver.ClaimMappings{
								Username: apiserver.PrefixedClaimOrExpression{
									Claim:  "sub",
									Prefix: pointer.String(""),
								},
							},
							ClaimValidationRules: []apiserver.ClaimValidationRule{
								{
									Claim:         "foo",
									RequiredValue: "bar",
								},
							},
						},
					},
				},
				OIDCSigningAlgs: []string{"RS256"},
			},
		},
		{
			name: "--oidc-username-prefix is empty, --oidc-username-claim is not email",
			args: []string{
				"--oidc-issuer-url=https://testIssuerURL",
				"--oidc-client-id=testClientID",
				"--oidc-username-claim=sub",
				"--oidc-signing-algs=RS256",
				"--oidc-required-claim=foo=bar",
			},
			expectConfig: kubeauthenticator.Config{
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					JWT: []apiserver.JWTAuthenticator{
						{
							Issuer: apiserver.Issuer{
								URL:       "https://testIssuerURL",
								Audiences: []string{"testClientID"},
							},
							ClaimMappings: apiserver.ClaimMappings{
								Username: apiserver.PrefixedClaimOrExpression{
									Claim:  "sub",
									Prefix: pointer.String("https://testIssuerURL#"),
								},
							},
							ClaimValidationRules: []apiserver.ClaimValidationRule{
								{
									Claim:         "foo",
									RequiredValue: "bar",
								},
							},
						},
					},
				},
				OIDCSigningAlgs: []string{"RS256"},
			},
		},
		{
			name: "--oidc-username-prefix is empty, --oidc-username-claim is email",
			args: []string{
				"--oidc-issuer-url=https://testIssuerURL",
				"--oidc-client-id=testClientID",
				"--oidc-username-claim=email",
				"--oidc-signing-algs=RS256",
				"--oidc-required-claim=foo=bar",
			},
			expectConfig: kubeauthenticator.Config{
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					JWT: []apiserver.JWTAuthenticator{
						{
							Issuer: apiserver.Issuer{
								URL:       "https://testIssuerURL",
								Audiences: []string{"testClientID"},
							},
							ClaimMappings: apiserver.ClaimMappings{
								Username: apiserver.PrefixedClaimOrExpression{
									Claim:  "email",
									Prefix: pointer.String(""),
								},
							},
							ClaimValidationRules: []apiserver.ClaimValidationRule{
								{
									Claim:         "foo",
									RequiredValue: "bar",
								},
							},
						},
					},
				},
				OIDCSigningAlgs: []string{"RS256"},
			},
		},
		{
			name: "non empty username prefix",
			args: []string{
				"--oidc-issuer-url=https://testIssuerURL",
				"--oidc-client-id=testClientID",
				"--oidc-username-claim=sub",
				"--oidc-username-prefix=k8s-",
				"--oidc-signing-algs=RS256",
				"--oidc-required-claim=foo=bar",
			},
			expectConfig: kubeauthenticator.Config{
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					JWT: []apiserver.JWTAuthenticator{
						{
							Issuer: apiserver.Issuer{
								URL:       "https://testIssuerURL",
								Audiences: []string{"testClientID"},
							},
							ClaimMappings: apiserver.ClaimMappings{
								Username: apiserver.PrefixedClaimOrExpression{
									Claim:  "sub",
									Prefix: pointer.String("k8s-"),
								},
							},
							ClaimValidationRules: []apiserver.ClaimValidationRule{
								{
									Claim:         "foo",
									RequiredValue: "bar",
								},
							},
						},
					},
				},
				OIDCSigningAlgs: []string{"RS256"},
			},
		},
		{
			name: "groups claim exists",
			args: []string{
				"--oidc-issuer-url=https://testIssuerURL",
				"--oidc-client-id=testClientID",
				"--oidc-username-claim=sub",
				"--oidc-username-prefix=-",
				"--oidc-groups-claim=groups",
				"--oidc-groups-prefix=oidc:",
				"--oidc-signing-algs=RS256",
				"--oidc-required-claim=foo=bar",
			},
			expectConfig: kubeauthenticator.Config{
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					JWT: []apiserver.JWTAuthenticator{
						{
							Issuer: apiserver.Issuer{
								URL:       "https://testIssuerURL",
								Audiences: []string{"testClientID"},
							},
							ClaimMappings: apiserver.ClaimMappings{
								Username: apiserver.PrefixedClaimOrExpression{
									Claim:  "sub",
									Prefix: pointer.String(""),
								},
								Groups: apiserver.PrefixedClaimOrExpression{
									Claim:  "groups",
									Prefix: pointer.String("oidc:"),
								},
							},
							ClaimValidationRules: []apiserver.ClaimValidationRule{
								{
									Claim:         "foo",
									RequiredValue: "bar",
								},
							},
						},
					},
				},
				OIDCSigningAlgs: []string{"RS256"},
			},
		},
		{
			name: "basic authentication configuration",
			args: []string{
				"--authentication-config=" + writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
    audiences: [ "🐼" ]
  claimMappings:
    username:
      claim: sub
      prefix: ""
`),
			},
			expectConfig: kubeauthenticator.Config{
				TokenSuccessCacheTTL: 10 * time.Second,
				AuthenticationConfig: &apiserver.AuthenticationConfiguration{
					JWT: []apiserver.JWTAuthenticator{
						{
							Issuer: apiserver.Issuer{
								URL:       "https://test-issuer",
								Audiences: []string{"🐼"},
							},
							ClaimMappings: apiserver.ClaimMappings{
								Username: apiserver.PrefixedClaimOrExpression{
									Claim:  "sub",
									Prefix: pointer.String(""),
								},
							},
						},
					},
				},
				AuthenticationConfigData: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
    audiences: [ "🐼" ]
  claimMappings:
    username:
      claim: sub
      prefix: ""
`,
				OIDCSigningAlgs: []string{"ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "RS256", "RS384", "RS512"},
			},
		},
	}

	for _, testcase := range testCases {
		t.Run(testcase.name, func(t *testing.T) {
			opts := NewBuiltInAuthenticationOptions().WithOIDC()
			pf := pflag.NewFlagSet("test-builtin-authentication-opts", pflag.ContinueOnError)
			opts.AddFlags(pf)

			if err := pf.Parse(testcase.args); err != nil {
				t.Fatal(err)
			}

			resultConfig, err := opts.ToAuthenticationConfig()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resultConfig, testcase.expectConfig) {
				t.Error(cmp.Diff(resultConfig, testcase.expectConfig))
			}
		})
	}
}

func TestValidateOIDCOptions(t *testing.T) {
	testCases := []struct {
		name                                  string
		args                                  []string
		structuredAuthenticationConfigEnabled bool
		expectErr                             string
	}{
		{
			name: "issuer url and client id are not set",
			args: []string{
				"--oidc-username-claim=testClaim",
			},
			expectErr: "oidc-issuer-url and oidc-client-id must be specified together when any oidc-* flags are set",
		},
		{
			name: "issuer url set, client id is not set",
			args: []string{
				"--oidc-issuer-url=https://testIssuerURL",
				"--oidc-username-claim=testClaim",
			},
			expectErr: "oidc-issuer-url and oidc-client-id must be specified together when any oidc-* flags are set",
		},
		{
			name: "issuer url is not set, client id is set",
			args: []string{
				"--oidc-client-id=testClientID",
				"--oidc-username-claim=testClaim",
			},
			expectErr: "oidc-issuer-url and oidc-client-id must be specified together when any oidc-* flags are set",
		},
		{
			name: "issuer url and client id are set",
			args: []string{
				"--oidc-client-id=testClientID",
				"--oidc-issuer-url=https://testIssuerURL",
			},
			expectErr: "",
		},
		{
			name: "authentication-config file, feature gate is not enabled",
			args: []string{
				"--authentication-config=configfile",
			},
			expectErr: "set --feature-gates=StructuredAuthenticationConfiguration=true to use authentication-config file",
		},
		{
			name: "authentication-config file, --oidc-issuer-url is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-issuer-url=https://testIssuerURL",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-client-id is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-client-id=testClientID",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-username-claim is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-username-claim=testClaim",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-username-prefix is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-username-prefix=testPrefix",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-ca-file is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-ca-file=testCAFile",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-groups-claim is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-groups-claim=testClaim",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-groups-prefix is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-groups-prefix=testPrefix",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-required-claim is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-required-claim=foo=bar",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-signature-algs is set",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-signing-algs=RS512",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "authentication-config file, --oidc-username-claim flag not set, defaulting shouldn't error",
			args: []string{
				"--authentication-config=configfile",
			},
			expectErr:                             "",
			structuredAuthenticationConfigEnabled: true,
		},
		{
			name: "authentication-config file, --oidc-username-claim flag explicitly set with default value should error",
			args: []string{
				"--authentication-config=configfile",
				"--oidc-username-claim=sub",
			},
			expectErr: "authentication-config file and oidc-* flags are mutually exclusive",
		},
		{
			name: "valid authentication-config file",
			args: []string{
				"--authentication-config=configfile",
			},
			structuredAuthenticationConfigEnabled: true,
			expectErr:                             "",
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, tt.structuredAuthenticationConfigEnabled)

			opts := NewBuiltInAuthenticationOptions().WithOIDC()
			pf := pflag.NewFlagSet("test-builtin-authentication-opts", pflag.ContinueOnError)
			opts.AddFlags(pf)

			if err := pf.Parse(tt.args); err != nil {
				t.Fatal(err)
			}

			errs := opts.Validate()
			if len(errs) > 0 && (!strings.Contains(utilerrors.NewAggregate(errs).Error(), tt.expectErr) || tt.expectErr == "") {
				t.Errorf("Got err: %v, Expected err: %s", errs, tt.expectErr)
			}
			if len(errs) == 0 && len(tt.expectErr) != 0 {
				t.Errorf("Got err nil, Expected err: %s", tt.expectErr)
			}
			if len(errs) > 0 && len(tt.expectErr) == 0 {
				t.Errorf("Got err: %v, Expected err nil", errs)
			}
		})
	}
}

func TestLoadAuthenticationConfig(t *testing.T) {
	testCases := []struct {
		name                string
		file                func() string
		expectErr           string
		expectedConfig      *apiserver.AuthenticationConfiguration
		expectedContentData string
	}{
		{
			name:           "empty file",
			file:           func() string { return writeTempFile(t, ``) },
			expectErr:      "empty config data",
			expectedConfig: nil,
		},
		{
			name: "valid file",
			file: func() string {
				return writeTempFile(t,
					`{
						"apiVersion":"apiserver.config.k8s.io/v1alpha1",
						"kind":"AuthenticationConfiguration",
						"jwt":[{"issuer":{"url": "https://test-issuer"}}]}`)
			},
			expectErr: "",
			expectedConfig: &apiserver.AuthenticationConfiguration{
				JWT: []apiserver.JWTAuthenticator{
					{
						Issuer: apiserver.Issuer{URL: "https://test-issuer"},
					},
				},
			},
			expectedContentData: `{
						"apiVersion":"apiserver.config.k8s.io/v1alpha1",
						"kind":"AuthenticationConfiguration",
						"jwt":[{"issuer":{"url": "https://test-issuer"}}]}`,
		},
		{
			name:           "missing file",
			file:           func() string { return "bogus-missing-file" },
			expectErr:      syscall.Errno(syscall.ENOENT).Error(),
			expectedConfig: nil,
		},
		{
			name: "invalid content file",
			file: func() string {
				return writeTempFile(t, `{"apiVersion":"apiserver.config.k8s.io/v99","kind":"AuthenticationConfiguration","authorizers":{"type":"Webhook"}}`)
			},
			expectErr:      `no kind "AuthenticationConfiguration" is registered for version "apiserver.config.k8s.io/v99"`,
			expectedConfig: nil,
		},
		{
			name:      "missing apiVersion",
			file:      func() string { return writeTempFile(t, `{"kind":"AuthenticationConfiguration"}`) },
			expectErr: `'apiVersion' is missing`,
		},
		{
			name:      "missing kind",
			file:      func() string { return writeTempFile(t, `{"apiVersion":"apiserver.config.k8s.io/v1alpha1"}`) },
			expectErr: `'Kind' is missing`,
		},
		{
			name: "unknown group",
			file: func() string {
				return writeTempFile(t, `{"apiVersion":"apps/v1alpha1","kind":"AuthenticationConfiguration"}`)
			},
			expectErr: `apps/v1alpha1`,
		},
		{
			name: "unknown version",
			file: func() string {
				return writeTempFile(t, `{"apiVersion":"apiserver.config.k8s.io/v99","kind":"AuthenticationConfiguration"}`)
			},
			expectErr: `apiserver.config.k8s.io/v99`,
		},
		{
			name: "unknown kind",
			file: func() string {
				return writeTempFile(t, `{"apiVersion":"apiserver.config.k8s.io/v1alpha1","kind":"SomeConfiguration"}`)
			},
			expectErr: `SomeConfiguration`,
		},
		{
			name: "unknown field",
			file: func() string {
				return writeTempFile(t, `{
							"apiVersion":"apiserver.config.k8s.io/v1alpha1",
							"kind":"AuthenticationConfiguration",
							"jwt1":[{"issuer":{"url": "https://test-issuer"}}]}`)
			},
			expectErr: `unknown field "jwt1"`,
		},
		{
			name: "v1alpha1 - json",
			file: func() string {
				return writeTempFile(t, `{
							"apiVersion":"apiserver.config.k8s.io/v1alpha1",
							"kind":"AuthenticationConfiguration",
							"jwt":[{"issuer":{"url": "https://test-issuer"}}]}`)
			},
			expectedConfig: &apiserver.AuthenticationConfiguration{
				JWT: []apiserver.JWTAuthenticator{
					{
						Issuer: apiserver.Issuer{
							URL: "https://test-issuer",
						},
					},
				},
			},
			expectedContentData: `{
							"apiVersion":"apiserver.config.k8s.io/v1alpha1",
							"kind":"AuthenticationConfiguration",
							"jwt":[{"issuer":{"url": "https://test-issuer"}}]}`,
		},
		{
			name: "v1alpha1 - yaml",
			file: func() string {
				return writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
  claimMappings:
    username:
      claim: sub
      prefix: ""
`)
			},
			expectedConfig: &apiserver.AuthenticationConfiguration{
				JWT: []apiserver.JWTAuthenticator{
					{
						Issuer: apiserver.Issuer{
							URL: "https://test-issuer",
						},
						ClaimMappings: apiserver.ClaimMappings{
							Username: apiserver.PrefixedClaimOrExpression{
								Claim:  "sub",
								Prefix: pointer.String(""),
							},
						},
					},
				},
			},
			expectedContentData: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
  claimMappings:
    username:
      claim: sub
      prefix: ""
`,
		},
		{
			name: "v1alpha1 - no jwt",
			file: func() string {
				return writeTempFile(t, `{
							"apiVersion":"apiserver.config.k8s.io/v1alpha1",
							"kind":"AuthenticationConfiguration"}`)
			},
			expectedConfig: &apiserver.AuthenticationConfiguration{},
			expectedContentData: `{
							"apiVersion":"apiserver.config.k8s.io/v1alpha1",
							"kind":"AuthenticationConfiguration"}`,
		},
		{
			name: "v1beta1 - json",
			file: func() string {
				return writeTempFile(t, `{
							"apiVersion":"apiserver.config.k8s.io/v1beta1",
							"kind":"AuthenticationConfiguration",
							"jwt":[{"issuer":{"url": "https://test-issuer"}}]}`)
			},
			expectedConfig: &apiserver.AuthenticationConfiguration{
				JWT: []apiserver.JWTAuthenticator{
					{
						Issuer: apiserver.Issuer{
							URL: "https://test-issuer",
						},
					},
				},
			},
			expectedContentData: `{
							"apiVersion":"apiserver.config.k8s.io/v1beta1",
							"kind":"AuthenticationConfiguration",
							"jwt":[{"issuer":{"url": "https://test-issuer"}}]}`,
		},
		{
			name: "v1beta1 - yaml",
			file: func() string {
				return writeTempFile(t, `
apiVersion: apiserver.config.k8s.io/v1beta1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
  claimMappings:
    username:
      claim: sub
      prefix: ""
`)
			},
			expectedConfig: &apiserver.AuthenticationConfiguration{
				JWT: []apiserver.JWTAuthenticator{
					{
						Issuer: apiserver.Issuer{
							URL: "https://test-issuer",
						},
						ClaimMappings: apiserver.ClaimMappings{
							Username: apiserver.PrefixedClaimOrExpression{
								Claim:  "sub",
								Prefix: pointer.String(""),
							},
						},
					},
				},
			},
			expectedContentData: `
apiVersion: apiserver.config.k8s.io/v1beta1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: https://test-issuer
  claimMappings:
    username:
      claim: sub
      prefix: ""
`,
		},
		{
			name: "v1beta1 - no jwt",
			file: func() string {
				return writeTempFile(t, `{
							"apiVersion":"apiserver.config.k8s.io/v1beta1",
							"kind":"AuthenticationConfiguration"}`)
			},
			expectedConfig: &apiserver.AuthenticationConfiguration{},
			expectedContentData: `{
							"apiVersion":"apiserver.config.k8s.io/v1beta1",
							"kind":"AuthenticationConfiguration"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config, contentData, err := loadAuthenticationConfig(tc.file())
			if !strings.Contains(errString(err), tc.expectErr) {
				t.Fatalf("expected error %q, got %v", tc.expectErr, err)
			}
			if !reflect.DeepEqual(config, tc.expectedConfig) {
				t.Fatalf("unexpected config:\n%s", cmp.Diff(tc.expectedConfig, config))
			}
			if contentData != tc.expectedContentData {
				t.Errorf("unexpected content data: want=%q, got=%q", tc.expectedContentData, contentData)
			}
		})
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	file, err := os.CreateTemp("", "config")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// An open file cannot be removed on Windows. Close it first.
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(file.Name()); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(file.Name(), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestToAuthenticationConfigForServiceAccount(t *testing.T) {

	dummyExternalGetter := &dummyPublicKeyGetter{}
	keyFileName := "public_key.pem"

	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("Error while generating first RSA key")
	}
	pubKey1Bytes, err := x509.MarshalPKIXPublicKey(&key1.PublicKey)
	if err != nil {
		panic("Error while marshaling first public key")
	}

	publicKeyBlock := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubKey1Bytes,
	}

	publicKeyFile, err := os.Create(keyFileName)
	if err != nil {
		fmt.Println("Error creating public key file:", err)
		return
	}
	t.Cleanup(func() {
		// An open file cannot be removed on Windows. Close it first.
		if err := publicKeyFile.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(publicKeyFile.Name()); err != nil {
			t.Fatal(err)
		}
	})

	if err := pem.Encode(publicKeyFile, publicKeyBlock); err != nil {
		fmt.Println("Error encoding public key:", err)
		return
	}

	testCases := []struct {
		desc                   string
		options                *BuiltInAuthenticationOptions
		expectConfig           kubeauthenticator.Config
		expectedErr            error
		expectedExternalGetter bool
		expectedStaticGetter   bool
	}{
		{
			desc: "neither key file nor external getter configured",
			options: &BuiltInAuthenticationOptions{
				ServiceAccounts: &ServiceAccountAuthenticationOptions{
					Lookup:                   true,
					Issuers:                  []string{"http://foo.bar.com"},
					KeyFiles:                 []string{},
					ExternalPublicKeysGetter: nil,
				},
			},
			expectConfig: kubeauthenticator.Config{
				APIAudiences:          authenticator.Audiences{"http://foo.bar.com"},
				ServiceAccountLookup:  true,
				ServiceAccountIssuers: []string{"http://foo.bar.com"},
			},
			expectedErr: nil,
		},
		{
			desc: "both key file and external getter configured",
			options: &BuiltInAuthenticationOptions{
				ServiceAccounts: &ServiceAccountAuthenticationOptions{
					Lookup:                   true,
					Issuers:                  []string{"http://foo.bar.com"},
					KeyFiles:                 []string{keyFileName},
					ExternalPublicKeysGetter: dummyExternalGetter,
				},
			},
			expectConfig: kubeauthenticator.Config{
				APIAudiences:          authenticator.Audiences{"http://foo.bar.com"},
				ServiceAccountLookup:  true,
				ServiceAccountIssuers: []string{"http://foo.bar.com"},
			},
			expectedErr: fmt.Errorf("cannot set mutually exclusive flags `--service-account-key-file` and `--service-account-signing-endpoint` at the same time"),
		},
		{
			desc: "external getter configured",
			options: &BuiltInAuthenticationOptions{
				ServiceAccounts: &ServiceAccountAuthenticationOptions{
					Lookup:                   true,
					Issuers:                  []string{"http://foo.bar.com"},
					KeyFiles:                 []string{},
					ExternalPublicKeysGetter: dummyExternalGetter,
				},
			},
			expectConfig: kubeauthenticator.Config{
				APIAudiences:          authenticator.Audiences{"http://foo.bar.com"},
				ServiceAccountLookup:  true,
				ServiceAccountIssuers: []string{"http://foo.bar.com"},
			},
			expectedErr:            nil,
			expectedExternalGetter: true,
		},
		{
			desc: "external getter configured",
			options: &BuiltInAuthenticationOptions{
				ServiceAccounts: &ServiceAccountAuthenticationOptions{
					Lookup:                   true,
					Issuers:                  []string{"http://foo.bar.com"},
					KeyFiles:                 []string{keyFileName},
					ExternalPublicKeysGetter: nil,
				},
			},
			expectConfig: kubeauthenticator.Config{
				APIAudiences:          authenticator.Audiences{"http://foo.bar.com"},
				ServiceAccountLookup:  true,
				ServiceAccountIssuers: []string{"http://foo.bar.com"},
			},
			expectedErr:          nil,
			expectedStaticGetter: true,
		},
	}

	for _, tc := range testCases {
		resultConfig, err := tc.options.ToAuthenticationConfig()
		if tc.expectedErr != nil {
			if err == nil || tc.expectedErr.Error() != err.Error() {
				t.Fatalf("Expected error: %v and got: %v", tc.expectedErr, err)
			}
			return
		}

		// make out of scope fields nil
		resultConfig.AuthenticationConfig = nil

		if tc.expectedExternalGetter {
			if resultConfig.ServiceAccountPublicKeysGetter == nil {
				t.Fatalf("Expected external getter but none")
			} else if resultConfig.ServiceAccountPublicKeysGetter != dummyExternalGetter {
				t.Fatalf("Expected external getter but found someting else")
			}
			resultConfig.ServiceAccountPublicKeysGetter = nil
		}

		if tc.expectedStaticGetter {
			if resultConfig.ServiceAccountPublicKeysGetter == nil {
				t.Fatalf("Expected static getter but none")
			} else if resultConfig.ServiceAccountPublicKeysGetter == dummyExternalGetter {
				t.Fatalf("Expected static getter but found external getter")
			}
			resultConfig.ServiceAccountPublicKeysGetter = nil
		}

		if !reflect.DeepEqual(resultConfig, tc.expectConfig) {
			t.Error(cmp.Diff(resultConfig, tc.expectConfig))
		}
	}
}

type dummyPublicKeyGetter struct {
}

func (d *dummyPublicKeyGetter) AddListener(listener serviceaccount.Listener) {}

func (d *dummyPublicKeyGetter) GetCacheAgeMaxSeconds() int {
	return 10
}

func (d *dummyPublicKeyGetter) GetPublicKeys(ctx context.Context, keyIDHint string) []serviceaccount.PublicKey {
	return []serviceaccount.PublicKey{}
}
