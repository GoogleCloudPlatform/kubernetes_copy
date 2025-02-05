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
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
)

func TestAuthzValidate(t *testing.T) {
	examplePolicyFile := "../../auth/authorizer/abac/example_policy_file.jsonl"

	testCases := []struct {
		name                 string
		modes                []string
		policyFile           string
		webhookConfigFile    string
		webhookRetryBackoff  *wait.Backoff
		expectErr            bool
		expectErrorSubString string
	}{
		{
			name:                 "Unknown modes should return errors",
			modes:                []string{"DoesNotExist"},
			expectErr:            true,
			expectErrorSubString: "is not a valid mode",
		},
		{
			name:                 "At least one authorizationMode is necessary",
			modes:                []string{},
			expectErr:            true,
			expectErrorSubString: "at least one authorization-mode must be passed",
		},
		{
			name:                 "ModeAlwaysAllow specified more than once",
			modes:                []string{modes.ModeAlwaysAllow, modes.ModeAlwaysAllow},
			expectErr:            true,
			expectErrorSubString: "has mode specified more than once",
		},
		{
			name:      "ModeAlwaysAllow and ModeAlwaysDeny should return without authorizationPolicyFile",
			modes:     []string{modes.ModeAlwaysAllow, modes.ModeAlwaysDeny},
			expectErr: false,
		},
		{
			name:                 "ModeABAC requires a policy file",
			modes:                []string{modes.ModeAlwaysAllow, modes.ModeAlwaysDeny, modes.ModeABAC},
			expectErr:            true,
			expectErrorSubString: "authorization-mode ABAC's authorization policy file not passed",
		},
		{
			name:                 "Authorization Policy file cannot be used without ModeABAC",
			modes:                []string{modes.ModeAlwaysAllow, modes.ModeAlwaysDeny},
			policyFile:           examplePolicyFile,
			webhookConfigFile:    "",
			expectErr:            true,
			expectErrorSubString: "cannot specify --authorization-policy-file without mode ABAC",
		},
		{
			name:              "ModeABAC should not error if a valid policy path is provided",
			modes:             []string{modes.ModeAlwaysAllow, modes.ModeAlwaysDeny, modes.ModeABAC},
			policyFile:        examplePolicyFile,
			webhookConfigFile: "",
			expectErr:         false,
		},
		{
			name:                 "ModeWebhook requires a config file",
			modes:                []string{modes.ModeWebhook},
			expectErr:            true,
			expectErrorSubString: "authorization-mode Webhook's authorization config file not passed",
		},
		{
			name:                 "Cannot provide webhook config file without ModeWebhook",
			modes:                []string{modes.ModeAlwaysAllow},
			webhookConfigFile:    "authz_webhook_config.yaml",
			expectErr:            true,
			expectErrorSubString: "cannot specify --authorization-webhook-config-file without mode Webhook",
		},
		{
			name:              "ModeWebhook should not error if a valid config file is provided",
			modes:             []string{modes.ModeWebhook},
			webhookConfigFile: "authz_webhook_config.yaml",
			expectErr:         false,
		},
		{
			name:                 "ModeWebhook should error if an invalid number of webhook retry attempts is provided",
			modes:                []string{modes.ModeWebhook},
			webhookConfigFile:    "authz_webhook_config.yaml",
			webhookRetryBackoff:  &wait.Backoff{Steps: 0},
			expectErr:            true,
			expectErrorSubString: "number of webhook retry attempts must be greater than 0",
		},
	}

	for _, testcase := range testCases {
		t.Run(testcase.name, func(t *testing.T) {
			options := NewBuiltInAuthorizationOptions()
			options.Modes = testcase.modes
			options.WebhookConfigFile = testcase.webhookConfigFile
			options.WebhookRetryBackoff = testcase.webhookRetryBackoff
			options.PolicyFile = testcase.policyFile

			errs := options.Validate()
			if len(errs) > 0 && !testcase.expectErr {
				t.Errorf("got unexpected err %v", errs)
			}
			if testcase.expectErr && len(errs) == 0 {
				t.Errorf("should return an error")
			}
			if len(errs) > 0 && testcase.expectErr {
				if !strings.Contains(utilerrors.NewAggregate(errs).Error(), testcase.expectErrorSubString) {
					t.Errorf("expected to found error: %s, but no error found", testcase.expectErrorSubString)
				}
			}
		})
	}
}

func TestBuiltInAuthorizationOptionsAddFlags(t *testing.T) {
	var args = []string{
		fmt.Sprintf("--authorization-mode=%s,%s,%s,%s", modes.ModeAlwaysAllow, modes.ModeAlwaysDeny, modes.ModeABAC, modes.ModeWebhook),
		"--authorization-policy-file=policy_file.json",
		"--authorization-webhook-config-file=webhook_config_file.yaml",
		"--authorization-webhook-version=v1",
		"--authorization-webhook-cache-authorized-ttl=60s",
		"--authorization-webhook-cache-unauthorized-ttl=30s",
	}

	expected := &BuiltInAuthorizationOptions{
		Modes:                       []string{modes.ModeAlwaysAllow, modes.ModeAlwaysDeny, modes.ModeABAC, modes.ModeWebhook},
		PolicyFile:                  "policy_file.json",
		WebhookConfigFile:           "webhook_config_file.yaml",
		WebhookVersion:              "v1",
		WebhookCacheAuthorizedTTL:   60 * time.Second,
		WebhookCacheUnauthorizedTTL: 30 * time.Second,
		WebhookRetryBackoff: &wait.Backoff{
			Duration: 500 * time.Millisecond,
			Factor:   1.5,
			Jitter:   0.2,
			Steps:    5,
		},
	}

	opts := NewBuiltInAuthorizationOptions()
	pf := pflag.NewFlagSet("test-builtin-authorization-opts", pflag.ContinueOnError)
	opts.AddFlags(pf)

	if err := pf.Parse(args); err != nil {
		t.Fatal(err)
	}

	if !opts.AreLegacyFlagsSet() {
		t.Fatal("legacy flags should have been configured")
	}

	// setting the method to nil since methods can't be compared with reflect.DeepEqual
	opts.AreLegacyFlagsSet = nil

	if !reflect.DeepEqual(opts, expected) {
		t.Error(cmp.Diff(opts, expected))
	}
}
