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

package webhook

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPatches(t *testing.T) {
	sidecarImage = "test-image"
	testCases := []struct {
		patch    string
		initial  interface{}
		expected interface{}
		toTest   interface{}
	}{
		{
			patch: configMapPatch1,
			initial: corev1.ConfigMap{
				Data: map[string]string{
					"mutation-start": "yes",
				},
			},
			expected: &corev1.ConfigMap{
				Data: map[string]string{
					"mutation-start":   "yes",
					"mutation-stage-1": "yes",
				},
			},
		},
		{
			patch: configMapPatch2,
			initial: corev1.ConfigMap{
				Data: map[string]string{
					"mutation-start": "yes",
				},
			},
			expected: &corev1.ConfigMap{
				Data: map[string]string{
					"mutation-start":   "yes",
					"mutation-stage-2": "yes",
				},
			},
		},

		{
			patch: podsInitContainerPatch,
			initial: corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{},
				},
			},
			expected: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Image:     "webhook-added-image",
							Name:      "webhook-added-init-container",
							Resources: corev1.ResourceRequirements{},
						},
					},
				},
			},
		},
		{
			patch: fmt.Sprintf(podsSidecarPatch, sidecarImage),
			initial: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Image:     "image1",
							Name:      "container1",
							Resources: corev1.ResourceRequirements{},
						},
					},
				},
			},
			expected: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Image:     "image1",
							Name:      "container1",
							Resources: corev1.ResourceRequirements{},
						},
						{
							Image:     sidecarImage,
							Name:      "webhook-added-sidecar",
							Resources: corev1.ResourceRequirements{},
						},
					},
				},
			},
		},
	}
	for _, testcase := range testCases {
		objJS, err := json.Marshal(testcase.initial)
		if err != nil {
			t.Fatal(err)
		}
		patchObj, err := jsonpatch.DecodePatch([]byte(testcase.patch))
		if err != nil {
			t.Fatal(err)
		}

		patchedJS, err := patchObj.Apply(objJS)
		if err != nil {
			t.Fatal(err)
		}
		objType := reflect.TypeOf(testcase.initial)
		objTest := reflect.New(objType).Interface()
		err = json.Unmarshal(patchedJS, objTest)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(objTest, testcase.expected) {
			t.Errorf("\nexpected %#v\n, got %#v", testcase.expected, objTest)
		}
	}

}

func TestJSONPatchForUnstructured(t *testing.T) {
	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "Something",
			"apiVersion": "somegroup/v1",
			"data": map[string]interface{}{
				"mutation-start": "yes",
			},
		},
	}
	crJS, err := json.Marshal(cr)
	if err != nil {
		t.Fatal(err)
	}

	patchObj, err := jsonpatch.DecodePatch([]byte(configMapPatch1))
	if err != nil {
		t.Fatal(err)
	}
	patchedJS, err := patchObj.Apply(crJS)
	if err != nil {
		t.Fatal(err)
	}
	patchedObj := unstructured.Unstructured{}
	err = json.Unmarshal(patchedJS, &patchedObj)
	if err != nil {
		t.Fatal(err)
	}
	expectedData := map[string]interface{}{
		"mutation-start":   "yes",
		"mutation-stage-1": "yes",
	}

	if !reflect.DeepEqual(patchedObj.Object["data"], expectedData) {
		t.Errorf("\nexpected %#v\n, got %#v", expectedData, patchedObj.Object["data"])
	}
}
