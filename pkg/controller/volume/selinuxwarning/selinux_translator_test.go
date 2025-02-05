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

package selinuxwarning

import (
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestSELinuxOptionsToFileLabel(t *testing.T) {
	tests := []struct {
		name     string
		opts     *v1.SELinuxOptions
		expected string
	}{
		{
			name:     "nil options",
			opts:     nil,
			expected: "",
		},
		{
			name: "all fields set",
			opts: &v1.SELinuxOptions{
				User:  "system_u",
				Role:  "system_r",
				Type:  "container_t",
				Level: "c0,c1",
			},
			expected: "system_u:system_r:container_t:c0,c1",
		},
		{
			name: "some fields set",
			opts: &v1.SELinuxOptions{
				Type:  "container_t",
				Level: "c0,c1",
			},
			expected: "container_t:c0,c1",
		},
		{
			name:     "no fields set",
			opts:     &v1.SELinuxOptions{},
			expected: "",
		},
	}

	translator := &controllerSELinuxTranslator{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := translator.SELinuxOptionsToFileLabel(tt.opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
