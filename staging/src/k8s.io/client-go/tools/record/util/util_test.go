/*
Copyright 2025 The Kubernetes Authors.

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

package util

import (
	"strings"
	"testing"

	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
)

func TestGenerateEventName(t *testing.T) {
	timestamp := int64(105999103295324396)
	testCases := []struct {
		name     string
		refName  string
		expected string
	}{
		{
			name:     "valid name",
			refName:  "test-pod",
			expected: "test-pod.178959f726d80ec",
		},
		{
			name:     "invalid name - too long",
			refName:  strings.Repeat("x", 300),
			expected: strings.Repeat("x", 237) + ".178959f726d80ec",
		},
		{
			name:     "invalid name - upper case",
			refName:  "test.POD",
			expected: "test.pod.178959f726d80ec",
		},
		{
			name:     "invalid name - special chars",
			refName:  "test.pod/invalid!chars?",
			expected: "test.pod-invalid-chars.178959f726d80ec",
		},
		{
			// it yields the same name but the unix timestamp should differentiate them
			// and is unlikely an object allow non alphanumeric starting characters
			name:     "invalid name - special chars and non alphanumeric starting character",
			refName:  "--test.pod/invalid!chars?",
			expected: "test.pod-invalid-chars.178959f726d80ec",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := GenerateEventName(tc.refName, timestamp)

			if errs := apimachineryvalidation.NameIsDNSSubdomain(actual, false); len(errs) > 0 {
				t.Errorf("generateEventName(%s) = %s; not a valid name: %v", tc.refName, actual, errs)

			}

			if actual != tc.expected {
				t.Errorf("generateEventName(%s) returned %s expected %s", tc.refName, actual, tc.expected)
			}

		})

	}
}
