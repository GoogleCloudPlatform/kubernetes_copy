/*
Copyright 2015 The Kubernetes Authors.

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

package alwayspullimages

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
	admissiontesting "k8s.io/apiserver/pkg/admission/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
)

// TestAdmission verifies all create requests for pods result in every container's image pull policy
// set to Always
func TestAdmission(t *testing.T) {
	namespace := "test"
	handler := admissiontesting.WithReinvocationTesting(t, &AlwaysPullImages{})
	pod := api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "123", Namespace: namespace},
		Spec: api.PodSpec{
			InitContainers: []api.Container{
				{Name: "init1", Image: "image"},
				{Name: "init2", Image: "image", ImagePullPolicy: api.PullNever},
				{Name: "init3", Image: "image", ImagePullPolicy: api.PullIfNotPresent},
				{Name: "init4", Image: "image", ImagePullPolicy: api.PullAlways},
			},
			Containers: []api.Container{
				{Name: "ctr1", Image: "image"},
				{Name: "ctr2", Image: "image", ImagePullPolicy: api.PullNever},
				{Name: "ctr3", Image: "image", ImagePullPolicy: api.PullIfNotPresent},
				{Name: "ctr4", Image: "image", ImagePullPolicy: api.PullAlways},
			},
			Volumes: []api.Volume{
				{Name: "volume1", VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{PullPolicy: api.PullNever}}},
				{Name: "volume2", VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{PullPolicy: api.PullIfNotPresent}}},
				{Name: "volume3", VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{PullPolicy: api.PullAlways}}},
			},
		},
	}
	err := handler.Admit(context.TODO(), admission.NewAttributesRecord(&pod, nil, api.Kind("Pod").WithVersion("version"), pod.Namespace, pod.Name, api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
	if err != nil {
		t.Errorf("Unexpected error returned from admission handler")
	}
	for _, c := range pod.Spec.InitContainers {
		if c.ImagePullPolicy != api.PullAlways {
			t.Errorf("Container %v: expected pull always, got %v", c, c.ImagePullPolicy)
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.ImagePullPolicy != api.PullAlways {
			t.Errorf("Container %v: expected pull always, got %v", c, c.ImagePullPolicy)
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.Image.PullPolicy != api.PullAlways {
			t.Errorf("Image volume %v: expected pull always, got %v", v, v.Image.PullPolicy)
		}
	}
}

func TestEphemeralContainersAdmission(t *testing.T) {
	namespace := "test"
	handler := admissiontesting.WithReinvocationTesting(t, &AlwaysPullImages{})
	pod := api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "123", Namespace: namespace},
		Spec: api.PodSpec{
			EphemeralContainers: []api.EphemeralContainer{
				{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "ephemeral1", Image: "image"}},
				{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "ephemeral2", Image: "image", ImagePullPolicy: api.PullNever}},
				{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "ephemeral3", Image: "image", ImagePullPolicy: api.PullIfNotPresent}},
				{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "ephemeral4", Image: "image", ImagePullPolicy: api.PullAlways}},
			},
		},
	}
	err := handler.Admit(context.TODO(), admission.NewAttributesRecord(&pod, nil, api.Kind("Pod").WithVersion("version"), pod.Namespace, pod.Name, api.Resource("pods").WithVersion("version"), "ephemeralcontainers", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
	if err != nil {
		t.Errorf("Unexpected error returned from admission handler")
	}
	for _, c := range pod.Spec.EphemeralContainers {
		if c.EphemeralContainerCommon.ImagePullPolicy != api.PullAlways {
			t.Errorf("Container %v: expected pull always, got %v", c, c.ImagePullPolicy)
		}
	}
}

func TestValidate(t *testing.T) {
	namespace := "test"
	handler := &AlwaysPullImages{}
	pod := api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "123", Namespace: namespace},
		Spec: api.PodSpec{
			InitContainers: []api.Container{
				{Name: "init1", Image: "image"},
				{Name: "init2", Image: "image", ImagePullPolicy: api.PullNever},
				{Name: "init3", Image: "image", ImagePullPolicy: api.PullIfNotPresent},
				{Name: "init4", Image: "image", ImagePullPolicy: api.PullAlways},
			},
			Containers: []api.Container{
				{Name: "ctr1", Image: "image"},
				{Name: "ctr2", Image: "image", ImagePullPolicy: api.PullNever},
				{Name: "ctr3", Image: "image", ImagePullPolicy: api.PullIfNotPresent},
				{Name: "ctr4", Image: "image", ImagePullPolicy: api.PullAlways},
			},
			Volumes: []api.Volume{
				{Name: "volume1", VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{PullPolicy: ""}}},
				{Name: "volume2", VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{PullPolicy: api.PullNever}}},
				{Name: "volume3", VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{PullPolicy: api.PullIfNotPresent}}},
				{Name: "volume4", VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{PullPolicy: api.PullAlways}}},
			},
		},
	}
	expectedError := `[` +
		`pods "123" is forbidden: spec.initContainers[0].imagePullPolicy: Unsupported value: "": supported values: "Always", ` +
		`pods "123" is forbidden: spec.initContainers[1].imagePullPolicy: Unsupported value: "Never": supported values: "Always", ` +
		`pods "123" is forbidden: spec.initContainers[2].imagePullPolicy: Unsupported value: "IfNotPresent": supported values: "Always", ` +
		`pods "123" is forbidden: spec.containers[0].imagePullPolicy: Unsupported value: "": supported values: "Always", ` +
		`pods "123" is forbidden: spec.containers[1].imagePullPolicy: Unsupported value: "Never": supported values: "Always", ` +
		`pods "123" is forbidden: spec.containers[2].imagePullPolicy: Unsupported value: "IfNotPresent": supported values: "Always", ` +
		`pods "123" is forbidden: spec.volumes[0].image.pullPolicy: Unsupported value: "": supported values: "Always", ` +
		`pods "123" is forbidden: spec.volumes[1].image.pullPolicy: Unsupported value: "Never": supported values: "Always", ` +
		`pods "123" is forbidden: spec.volumes[2].image.pullPolicy: Unsupported value: "IfNotPresent": supported values: "Always"` +
		`]`
	err := handler.Validate(context.TODO(), admission.NewAttributesRecord(&pod, nil, api.Kind("Pod").WithVersion("version"), pod.Namespace, pod.Name, api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
	if err == nil {
		t.Fatal("missing expected error")
	}
	if err.Error() != expectedError {
		t.Fatal(err)
	}
}

// TestOtherResources ensures that this admission controller is a no-op for other resources,
// subresources other than ephemeralcontainers, and non-pods.
func TestOtherResources(t *testing.T) {
	namespace := "testnamespace"
	name := "testname"
	pod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "ctr2", Image: "image", ImagePullPolicy: api.PullNever},
			},
		},
	}
	tests := []struct {
		name        string
		kind        string
		resource    string
		subresource string
		object      runtime.Object
		expectError bool
	}{
		{
			name:     "non-pod resource",
			kind:     "Foo",
			resource: "foos",
			object:   pod,
		},
		{
			name:        "pod subresource",
			kind:        "Pod",
			resource:    "pods",
			subresource: "exec",
			object:      pod,
		},
		{
			name:        "non-pod object",
			kind:        "Pod",
			resource:    "pods",
			object:      &api.Service{},
			expectError: true,
		},
	}

	for _, tc := range tests {
		handler := admissiontesting.WithReinvocationTesting(t, &AlwaysPullImages{})

		err := handler.Admit(context.TODO(), admission.NewAttributesRecord(tc.object, nil, api.Kind(tc.kind).WithVersion("version"), namespace, name, api.Resource(tc.resource).WithVersion("version"), tc.subresource, admission.Create, &metav1.CreateOptions{}, false, nil), nil)

		if tc.expectError {
			if err == nil {
				t.Errorf("%s: unexpected nil error", tc.name)
			}
			continue
		}

		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}

		if e, a := api.PullNever, pod.Spec.Containers[0].ImagePullPolicy; e != a {
			t.Errorf("%s: image pull policy was changed to %s", tc.name, a)
		}
	}

}

// TestUpdatePod ensures that this admission controller is a no-op for update pod if no
// images were changed in the new pod spec.
func TestUpdatePod(t *testing.T) {
	namespace := "testnamespace"
	name := "testname"
	oldPod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "ctr2", Image: "image", ImagePullPolicy: api.PullIfNotPresent},
			},
		},
	}
	// only add new annotation
	pod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				"test": "test",
			},
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "ctr2", Image: "image", ImagePullPolicy: api.PullIfNotPresent},
			},
		},
	}
	// add new label and change image
	podWithNewImage := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				"test": "test",
			},
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "ctr2", Image: "image2", ImagePullPolicy: api.PullIfNotPresent},
			},
		},
	}
	tests := []struct {
		name         string
		kind         string
		resource     string
		subresource  string
		object       runtime.Object
		oldObject    runtime.Object
		expectError  bool
		expectIgnore bool
	}{
		{
			name:         "update IfNotPresent pod annotations",
			kind:         "Pod",
			resource:     "pods",
			subresource:  "finalizers",
			object:       pod,
			oldObject:    oldPod,
			expectIgnore: true,
		},
		{
			name:        "update IfNotPresent pod image",
			kind:        "Pod",
			resource:    "pods",
			subresource: "finalizers",
			object:      podWithNewImage,
			oldObject:   oldPod,
		},
	}

	for _, tc := range tests {
		handler := admissiontesting.WithReinvocationTesting(t, &AlwaysPullImages{})

		err := handler.Admit(context.TODO(), admission.NewAttributesRecord(tc.object, tc.oldObject, api.Kind(tc.kind).WithVersion("version"), namespace, name, api.Resource(tc.resource).WithVersion("version"), tc.subresource, admission.Create, &metav1.UpdateOptions{}, false, nil), nil)

		if tc.expectError {
			if err == nil {
				t.Errorf("%s: unexpected nil error", tc.name)
			}
			continue
		}
		if tc.expectIgnore {
			if e, a := api.PullIfNotPresent, pod.Spec.Containers[0].ImagePullPolicy; e != a {
				t.Errorf("%s: image pull policy was changed to %s", tc.name, a)
			}
			continue
		}

		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}

	}

}
