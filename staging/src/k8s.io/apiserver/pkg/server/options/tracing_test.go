/*
Copyright 2021 The Kubernetes Authors.

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
	"os"
	"reflect"
	"strings"
	"testing"

	tracingapi "k8s.io/component-base/tracing/api/v1"
)

var (
	localhost    = "localhost:4317"
	ipAddress    = "127.0.0.1:4317"
	samplingRate = int32(12345)
)

func strptr(s string) *string {
	return &s
}

func TestValidateTracingOptions(t *testing.T) {
	testcases := []struct {
		name        string
		expectError bool
		contents    *TracingOptions
	}{
		{
			name:        "nil-valid",
			expectError: false,
		},
		{
			name:        "empty-valid",
			expectError: false,
			contents:    &TracingOptions{},
		},
		{
			name:        "path-valid",
			expectError: false,
			contents:    &TracingOptions{ConfigFile: "/"},
		},
		{
			name:        "path-invalid",
			expectError: true,
			contents:    &TracingOptions{ConfigFile: "/path/doesnt/exist"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			errs := tc.contents.Validate()
			if !tc.expectError && len(errs) != 0 {
				t.Errorf("Calling Validate expected no error, got %v", errs)
			} else if tc.expectError && len(errs) == 0 {
				t.Errorf("Calling Validate expected error, got no error")
			}
		})
	}
}

func TestReadTracingConfiguration(t *testing.T) {
	testcases := []struct {
		name           string
		contents       string
		createFile     bool
		expectedResult *tracingapi.TracingConfiguration
		expectedError  *string
	}{
		{
			name:           "empty",
			createFile:     true,
			contents:       ``,
			expectedResult: &tracingapi.TracingConfiguration{},
			expectedError:  nil,
		},
		{
			name:           "absent",
			createFile:     false,
			contents:       ``,
			expectedResult: nil,
			expectedError:  strptr("unable to read tracing configuration from \"test-tracing-config-absent\": open test-tracing-config-absent: no such file or directory"),
		},
		{
			name:       "duplicate field error; strict validation",
			createFile: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: TracingConfiguration
endpoint: localhost:4317
endpoint: localhost:4318
samplingRatePerMillion: 12345
`,
			expectedResult: nil,
			expectedError:  strptr("unable to decode tracing configuration data: strict decoding error"),
		},
		{
			name:       "unknown field error; strict validation",
			createFile: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: TracingConfiguration
foo: bar
endpoint: localhost:4318
samplingRatePerMillion: 12345
`,
			expectedResult: nil,
			expectedError:  strptr("unable to decode tracing configuration data: strict decoding error"),
		},
		{
			name:       "v1alpha1",
			createFile: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: TracingConfiguration
endpoint: localhost:4317
samplingRatePerMillion: 12345
`,
			expectedResult: &tracingapi.TracingConfiguration{
				Endpoint:               &localhost,
				SamplingRatePerMillion: &samplingRate,
			},
			expectedError: nil,
		},
		{
			name:       "ip address",
			createFile: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: TracingConfiguration
endpoint: 127.0.0.1:4317
`,
			expectedResult: &tracingapi.TracingConfiguration{
				Endpoint: &ipAddress,
			},
			expectedError: nil,
		},
		{
			name:       "wrong_type",
			createFile: true,
			contents: `
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: agent
spec:
  selector:
    matchLabels:
      k8s-app: agent
  template:
    metadata:
      labels:
        k8s-app: agent
    spec:
      containers:
        - image: registry.k8s.io/busybox
          name: agent
`,
			expectedResult: nil,
			expectedError:  strptr("unable to decode tracing configuration data: no kind \"DaemonSet\" is registered for version \"apps/v1\" in scheme"),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			proxyConfig := fmt.Sprintf("test-tracing-config-%s", tc.name)
			if tc.createFile {
				f, err := os.CreateTemp("", proxyConfig)
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(f.Name())
				if err := os.WriteFile(f.Name(), []byte(tc.contents), os.FileMode(0755)); err != nil {
					t.Fatal(err)
				}
				proxyConfig = f.Name()
			}
			config, err := ReadTracingConfiguration(proxyConfig)
			if err == nil && tc.expectedError != nil {
				t.Errorf("calling ReadTracingConfiguration expected error: %s, did not get it", *tc.expectedError)
			}
			if err != nil && tc.expectedError == nil {
				t.Errorf("unexpected error calling ReadTracingConfiguration got: %#v", err)
			}
			if err != nil && tc.expectedError != nil && !strings.HasPrefix(err.Error(), *tc.expectedError) {
				t.Errorf("calling ReadTracingConfiguration expected error: %s, got %#v", *tc.expectedError, err)
			}
			if !reflect.DeepEqual(config, tc.expectedResult) {
				t.Errorf("problem with configuration returned from ReadTracingConfiguration expected: %#v, got: %#v", tc.expectedResult, config)
			}
		})
	}
}
