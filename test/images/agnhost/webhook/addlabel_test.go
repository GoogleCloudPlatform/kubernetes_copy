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

package webhook

import (
	"encoding/json"
	"reflect"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddLabel(t *testing.T) {
	testCases := []struct {
		name           string
		initialLabels  map[string]string
		expectedLabels map[string]string
	}{
		{
			name:           "add first label",
			initialLabels:  nil,
			expectedLabels: map[string]string{"added-label": "yes"},
		},
		{
			name:           "add second label",
			initialLabels:  map[string]string{"other-label": "yes"},
			expectedLabels: map[string]string{"other-label": "yes", "added-label": "yes"},
		},
		{
			name:           "idempotent update label",
			initialLabels:  map[string]string{"added-label": "yes"},
			expectedLabels: map[string]string{"added-label": "yes"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			request := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: tc.initialLabels}}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			review := v1.AdmissionReview{Request: &v1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}}}
			response := addLabel(review)
			if response.Patch != nil {
				patchObj, err := jsonpatch.DecodePatch(response.Patch)
				if err != nil {
					t.Fatal(err)
				}
				raw, err = patchObj.Apply(raw)
				if err != nil {
					t.Fatal(err)
				}
			}

			objType := reflect.TypeOf(request)
			objTest := reflect.New(objType).Interface()
			err = json.Unmarshal(raw, objTest)
			if err != nil {
				t.Fatal(err)
			}
			actual := objTest.(*corev1.ConfigMap)
			if !reflect.DeepEqual(actual.Labels, tc.expectedLabels) {
				t.Errorf("\nexpected %#v, got %#v, patch: %v", actual.Labels, tc.expectedLabels, string(response.Patch))
			}
		})
	}
}
