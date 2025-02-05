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

package options

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	kubeapiserveradmission "k8s.io/apiserver/pkg/admission"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/featuregate"
	basemetrics "k8s.io/component-base/metrics"
	"k8s.io/kubernetes/pkg/features"

	peerreconcilers "k8s.io/apiserver/pkg/reconcilers"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
)

func TestValidateAPIPriorityAndFairness(t *testing.T) {
	const conflict = "conflicts with --enable-priority-and-fairness=true"
	tests := []struct {
		runtimeConfig    string
		errShouldContain string
	}{
		{
			runtimeConfig:    "api/all=false",
			errShouldContain: conflict,
		},
		{
			runtimeConfig:    "api/beta=false",
			errShouldContain: "",
		},
		{
			runtimeConfig:    "api/ga=false",
			errShouldContain: conflict,
		},
		{
			runtimeConfig:    "api/ga=true",
			errShouldContain: "",
		},
		{
			runtimeConfig:    "flowcontrol.apiserver.k8s.io/v1beta1=false",
			errShouldContain: "",
		},
		{
			runtimeConfig:    "flowcontrol.apiserver.k8s.io/v1beta2=false",
			errShouldContain: "",
		},
		{
			runtimeConfig:    "flowcontrol.apiserver.k8s.io/v1beta3=false",
			errShouldContain: "",
		},
		{
			runtimeConfig:    "flowcontrol.apiserver.k8s.io/v1beta3=true",
			errShouldContain: "",
		},
		{
			runtimeConfig:    "flowcontrol.apiserver.k8s.io/v1=true",
			errShouldContain: "",
		},
		{
			runtimeConfig:    "flowcontrol.apiserver.k8s.io/v1=false",
			errShouldContain: conflict,
		},
		{
			runtimeConfig:    "flowcontrol.apiserver.k8s.io/v1beta3=true,flowcontrol.apiserver.k8s.io/v1=false",
			errShouldContain: conflict,
		},
	}

	for _, test := range tests {
		t.Run(test.runtimeConfig, func(t *testing.T) {
			options := &Options{
				Features: &genericoptions.FeatureOptions{
					EnablePriorityAndFairness: true,
				},
				APIEnablement: genericoptions.NewAPIEnablementOptions(),
			}
			options.APIEnablement.RuntimeConfig.Set(test.runtimeConfig)

			var errMessageGot string
			if errs := validateAPIPriorityAndFairness(options); len(errs) > 0 {
				errMessageGot = errs[0].Error()
			}

			switch {
			case len(test.errShouldContain) == 0:
				if len(errMessageGot) > 0 {
					t.Errorf("Expected no error, but got: %q", errMessageGot)
				}
			default:
				if !strings.Contains(errMessageGot, test.errShouldContain) {
					t.Errorf("Expected error message to contain: %q, but got: %q", test.errShouldContain, errMessageGot)
				}
			}
		})
	}
}

func TestValidateUnknownVersionInteroperabilityProxy(t *testing.T) {
	tests := []struct {
		name                 string
		featureEnabled       bool
		errShouldContain     string
		peerCAFile           string
		peerAdvertiseAddress peerreconcilers.PeerAdvertiseAddress
	}{
		{
			name:             "feature disabled but peerCAFile set",
			featureEnabled:   false,
			peerCAFile:       "foo",
			errShouldContain: "--peer-ca-file requires UnknownVersionInteroperabilityProxy feature to be turned on",
		},
		{
			name:                 "feature disabled but peerAdvertiseIP set",
			featureEnabled:       false,
			peerAdvertiseAddress: peerreconcilers.PeerAdvertiseAddress{PeerAdvertiseIP: "1.2.3.4"},
			errShouldContain:     "--peer-advertise-ip requires UnknownVersionInteroperabilityProxy feature to be turned on",
		},
		{
			name:                 "feature disabled but peerAdvertisePort set",
			featureEnabled:       false,
			peerAdvertiseAddress: peerreconcilers.PeerAdvertiseAddress{PeerAdvertisePort: "1"},
			errShouldContain:     "--peer-advertise-port requires UnknownVersionInteroperabilityProxy feature to be turned on",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := &Options{
				PeerCAFile:           test.peerCAFile,
				PeerAdvertiseAddress: test.peerAdvertiseAddress,
			}
			if test.featureEnabled {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.UnknownVersionInteroperabilityProxy, true)
			}
			var errMessageGot string
			if errs := validateUnknownVersionInteroperabilityProxyFlags(options); len(errs) > 0 {
				errMessageGot = errs[0].Error()
			}
			if !strings.Contains(errMessageGot, test.errShouldContain) {
				t.Errorf("Expected error message to contain: %q, but got: %q", test.errShouldContain, errMessageGot)
			}

		})
	}
}

func TestValidateUnknownVersionInteroperabilityProxyFeature(t *testing.T) {
	const conflict = "UnknownVersionInteroperabilityProxy feature requires StorageVersionAPI feature flag to be enabled"
	tests := []struct {
		name            string
		featuresEnabled []featuregate.Feature
	}{
		{
			name:            "enabled: UnknownVersionInteroperabilityProxy, disabled: StorageVersionAPI",
			featuresEnabled: []featuregate.Feature{features.UnknownVersionInteroperabilityProxy},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, feature := range test.featuresEnabled {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, feature, true)
			}
			var errMessageGot string
			if errs := validateUnknownVersionInteroperabilityProxyFeature(); len(errs) > 0 {
				errMessageGot = errs[0].Error()
			}
			if !strings.Contains(errMessageGot, conflict) {
				t.Errorf("Expected error message to contain: %q, but got: %q", conflict, errMessageGot)
			}
		})
	}
}

func TestValidateOptions(t *testing.T) {
	testCases := []struct {
		name         string
		options      *Options
		expectErrors bool
	}{
		{
			name:         "validate master count equal 0",
			expectErrors: true,
			options: &Options{
				GenericServerRunOptions: &genericoptions.ServerRunOptions{ComponentGlobalsRegistry: basecompatibility.NewComponentGlobalsRegistry()},
				Etcd:                    &genericoptions.EtcdOptions{},
				SecureServing:           &genericoptions.SecureServingOptionsWithLoopback{},
				Audit:                   &genericoptions.AuditOptions{},
				Admission: &kubeoptions.AdmissionOptions{
					GenericAdmission: &genericoptions.AdmissionOptions{
						EnablePlugins: []string{"foo"},
						Plugins:       kubeapiserveradmission.NewPlugins(),
					},
					PluginNames: []string{"foo"},
				},
				Authentication: &kubeoptions.BuiltInAuthenticationOptions{
					APIAudiences: []string{"bar"},
					ServiceAccounts: &kubeoptions.ServiceAccountAuthenticationOptions{
						Issuers: []string{"baz"},
					},
				},
				APIEnablement:                genericoptions.NewAPIEnablementOptions(),
				Metrics:                      &basemetrics.Options{},
				ServiceAccountSigningKeyFile: "",
				Features:                     &genericoptions.FeatureOptions{},
			},
		},
		{
			name:         "validate token request enable not attempted",
			expectErrors: true,
			options: &Options{
				GenericServerRunOptions: &genericoptions.ServerRunOptions{ComponentGlobalsRegistry: basecompatibility.NewComponentGlobalsRegistry()},
				Etcd:                    &genericoptions.EtcdOptions{},
				SecureServing:           &genericoptions.SecureServingOptionsWithLoopback{},
				Audit:                   &genericoptions.AuditOptions{},
				Admission: &kubeoptions.AdmissionOptions{
					GenericAdmission: &genericoptions.AdmissionOptions{
						EnablePlugins: []string{""},
						Plugins:       kubeapiserveradmission.NewPlugins(),
					},
					PluginNames: []string{""},
				},
				Authentication: &kubeoptions.BuiltInAuthenticationOptions{
					ServiceAccounts: &kubeoptions.ServiceAccountAuthenticationOptions{},
				},
				APIEnablement:                genericoptions.NewAPIEnablementOptions(),
				Metrics:                      &basemetrics.Options{},
				ServiceAccountSigningKeyFile: "",
				Features:                     &genericoptions.FeatureOptions{},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := tc.options.Validate()
			if len(errs) > 0 && !tc.expectErrors {
				t.Errorf("expected no errors, errors found %+v", errs)
			}

			if len(errs) == 0 && tc.expectErrors {
				t.Errorf("expected errors, no errors found")
			}
		})
	}
}

func TestValidateServcieAccountTokenSigningConfig(t *testing.T) {
	tests := []struct {
		name           string
		featureEnabled bool
		options        *Options
		expectedErrors []error
	}{
		{
			name:           "Signing keys file provided while external signer endpoint is provided",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("can't set `--service-account-signing-key-file` and/or `--service-account-key-file` with `--service-account-signing-endpoint` (They are mutually exclusive)"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "@ebc.eng.hij",
				ServiceAccountSigningKeyFile:  "/abc/efg",
			},
		},
		{
			name:           "Verification keys file provided while external signer endpoint is provided",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("can't set `--service-account-signing-key-file` and/or `--service-account-key-file` with `--service-account-signing-endpoint` (They are mutually exclusive)"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "@ebc.eng.hij",
				Authentication: &kubeoptions.BuiltInAuthenticationOptions{
					ServiceAccounts: &kubeoptions.ServiceAccountAuthenticationOptions{
						KeyFiles: []string{
							"abc",
							"efg",
						},
					},
				},
			},
		},
		{
			name:           "Verification key  and signing key file provided while external signer endpoint is provided",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("can't set `--service-account-signing-key-file` and/or `--service-account-key-file` with `--service-account-signing-endpoint` (They are mutually exclusive)"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "@ebc.eng.hij",
				ServiceAccountSigningKeyFile:  "/abc/efg",
				Authentication: &kubeoptions.BuiltInAuthenticationOptions{
					ServiceAccounts: &kubeoptions.ServiceAccountAuthenticationOptions{
						KeyFiles: []string{
							"/abc/efg",
							"/abc/xyz",
						},
					},
				},
			},
		},
		{
			name:           "feature disabled and external signer endpoint is provided",
			featureEnabled: false,
			expectedErrors: []error{
				fmt.Errorf("setting `--service-account-signing-endpoint` requires enabling ExternalServiceAccountTokenSigner feature gate"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "@ebc.eng.hij",
			},
		},
		{
			name:           "invalid external signer endpoint provided - 1",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("invalid value \"abc\" passed for `--service-account-signing-endpoint`, should be a valid location on the filesystem or must be prefixed with @ to name UDS in abstract namespace"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "abc",
			},
		},
		{
			name:           "invalid external signer endpoint provided - 2",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("invalid value \"@abc@\" passed for `--service-account-signing-endpoint`, should be a valid location on the filesystem or must be prefixed with @ to name UDS in abstract namespace"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "@abc@",
			},
		},
		{
			name:           "invalid external signer endpoint provided - 3",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("invalid value \"@abc.abc  .ae\" passed for `--service-account-signing-endpoint`, should be a valid location on the filesystem or must be prefixed with @ to name UDS in abstract namespace"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "@abc.abc  .ae",
			},
		},
		{
			name:           "invalid external signer endpoint provided - 4",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("invalid value \"/@e_adnb/xyz /efg\" passed for `--service-account-signing-endpoint`, should be a valid location on the filesystem or must be prefixed with @ to name UDS in abstract namespace"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "/@e_adnb/xyz /efg",
			},
		},
		{
			name:           "invalid external signer endpoint provided - 5",
			featureEnabled: true,
			expectedErrors: []error{
				fmt.Errorf("invalid value \"/e /xyz /efg\" passed for `--service-account-signing-endpoint`, should be a valid location on the filesystem or must be prefixed with @ to name UDS in abstract namespace"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "/e /xyz /efg",
			},
		},
		{
			name:           "valid external signer endpoint provided - 1",
			featureEnabled: true,
			expectedErrors: []error{},
			options: &Options{
				ServiceAccountSigningEndpoint: "/e/an_b-d/efg",
			},
		},
		{
			name:           "valid external signer endpoint provided - 2",
			featureEnabled: true,
			expectedErrors: []error{},
			options: &Options{
				ServiceAccountSigningEndpoint: "@ebc.sock",
			},
		},
		{
			name:           "valid external signer endpoint provided - 3",
			featureEnabled: true,
			expectedErrors: []error{},
			options: &Options{
				ServiceAccountSigningEndpoint: "@ebc.eng.hij",
			},
		},
		{
			name:           "All errors at once",
			featureEnabled: false,
			expectedErrors: []error{
				fmt.Errorf("can't set `--service-account-signing-key-file` and/or `--service-account-key-file` with `--service-account-signing-endpoint` (They are mutually exclusive)"),
				fmt.Errorf("setting `--service-account-signing-endpoint` requires enabling ExternalServiceAccountTokenSigner feature gate"),
				fmt.Errorf("invalid value \"/e /xyz /efg\" passed for `--service-account-signing-endpoint`, should be a valid location on the filesystem or must be prefixed with @ to name UDS in abstract namespace"),
			},
			options: &Options{
				ServiceAccountSigningEndpoint: "/e /xyz /efg",
				ServiceAccountSigningKeyFile:  "/abc/efg",
				Authentication: &kubeoptions.BuiltInAuthenticationOptions{
					ServiceAccounts: &kubeoptions.ServiceAccountAuthenticationOptions{
						KeyFiles: []string{
							"/abc/xyz",
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			if test.options.Authentication == nil {
				test.options.Authentication = &kubeoptions.BuiltInAuthenticationOptions{
					ServiceAccounts: &kubeoptions.ServiceAccountAuthenticationOptions{
						KeyFiles: []string{},
					},
				}
			}

			if test.featureEnabled {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ExternalServiceAccountTokenSigner, true)
			}
			errs := validateServiceAccountTokenSigningConfig(test.options)
			if !reflect.DeepEqual(errs, test.expectedErrors) {
				t.Errorf("Expected errors message: %v \n but got: %v", test.expectedErrors, errs)
			}
		})
	}
}
