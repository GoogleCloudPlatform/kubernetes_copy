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

package mutating

import (
	"encoding/json"
	"reflect"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/assert"
)

func TestMutationAnnotationValue(t *testing.T) {
	tcs := []struct {
		config   string
		webhook  string
		mutated  bool
		expected string
	}{
		{
			config:   "test-config",
			webhook:  "test-webhook",
			mutated:  true,
			expected: `{"configuration":"test-config","webhook":"test-webhook","mutated":true}`,
		},
		{
			config:   "test-config",
			webhook:  "test-webhook",
			mutated:  false,
			expected: `{"configuration":"test-config","webhook":"test-webhook","mutated":false}`,
		},
	}

	for _, tc := range tcs {
		actual, err := mutationAnnotationValue(tc.config, tc.webhook, tc.mutated)
		assert.NoError(t, err, "unexpected error")
		if actual != tc.expected {
			t.Errorf("composed mutation annotation value doesn't match, want: %s, got: %s", tc.expected, actual)
		}
	}
}

func TestJSONPatchAnnotationValue(t *testing.T) {
	tcs := []struct {
		name     string
		config   string
		webhook  string
		patch    []byte
		expected string
	}{
		{
			name:     "valid patch annotation",
			config:   "test-config",
			webhook:  "test-webhook",
			patch:    []byte(`[{"op": "add", "path": "/metadata/labels/a", "value": "true"}]`),
			expected: `{"configuration":"test-config","webhook":"test-webhook","patch":[{"op":"add","path":"/metadata/labels/a","value":"true"}],"patchType":"JSONPatch"}`,
		},
		{
			name:     "empty configuration",
			config:   "",
			webhook:  "test-webhook",
			patch:    []byte(`[{"op": "add", "path": "/metadata/labels/a", "value": "true"}]`),
			expected: `{"configuration":"","webhook":"test-webhook","patch":[{"op":"add","path":"/metadata/labels/a","value":"true"}],"patchType":"JSONPatch"}`,
		},
		{
			name:     "empty webhook",
			config:   "test-config",
			webhook:  "",
			patch:    []byte(`[{"op": "add", "path": "/metadata/labels/a", "value": "true"}]`),
			expected: `{"configuration":"test-config","webhook":"","patch":[{"op":"add","path":"/metadata/labels/a","value":"true"}],"patchType":"JSONPatch"}`,
		},
		{
			name:     "valid JSON patch empty operation",
			config:   "test-config",
			webhook:  "test-webhook",
			patch:    []byte("[{}]"),
			expected: `{"configuration":"test-config","webhook":"test-webhook","patch":[{}],"patchType":"JSONPatch"}`,
		},
		{
			name:     "empty slice patch",
			config:   "test-config",
			webhook:  "test-webhook",
			patch:    []byte("[]"),
			expected: `{"configuration":"test-config","webhook":"test-webhook","patch":[],"patchType":"JSONPatch"}`,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			jsonPatch, err := jsonpatch.DecodePatch(tc.patch)
			assert.NoError(t, err, "unexpected error decode patch")
			actual, err := jsonPatchAnnotationValue(tc.config, tc.webhook, jsonPatch)
			assert.NoError(t, err, "unexpected error getting json patch annotation")
			if actual != tc.expected {
				t.Errorf("composed patch annotation value doesn't match, want: %s, got: %s", tc.expected, actual)
			}

			var p map[string]interface{}
			if err := json.Unmarshal([]byte(actual), &p); err != nil {
				t.Errorf("unexpected error unmarshaling patch annotation: %v", err)
			}
			if p["configuration"] != tc.config {
				t.Errorf("unmarshaled configuration doesn't match, want: %s, got: %v", tc.config, p["configuration"])
			}
			if p["webhook"] != tc.webhook {
				t.Errorf("unmarshaled webhook doesn't match, want: %s, got: %v", tc.webhook, p["webhook"])
			}
			var expectedPatch interface{}
			err = json.Unmarshal(tc.patch, &expectedPatch)
			if err != nil {
				t.Errorf("unexpected error unmarshaling patch: %v, %v", tc.patch, err)
			}
			if !reflect.DeepEqual(expectedPatch, p["patch"]) {
				t.Errorf("unmarshaled patch doesn't match, want: %v, got: %v", expectedPatch, p["patch"])
			}
		})
	}
}
