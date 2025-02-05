/*
Copyright 2014 The Kubernetes Authors.

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

package limitranger

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	admissiontesting "k8s.io/apiserver/pkg/admission/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"

	api "k8s.io/kubernetes/pkg/apis/core"
	v1 "k8s.io/kubernetes/pkg/apis/core/v1"
)

func getComputeResourceList(cpu, memory string) api.ResourceList {
	res := api.ResourceList{}
	if cpu != "" {
		res[api.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		res[api.ResourceMemory] = resource.MustParse(memory)
	}
	return res
}

func getStorageResourceList(storage string) api.ResourceList {
	res := api.ResourceList{}
	if storage != "" {
		res[api.ResourceStorage] = resource.MustParse(storage)
	}
	return res
}

func getResourceRequirements(requests, limits api.ResourceList) api.ResourceRequirements {
	res := api.ResourceRequirements{}
	res.Requests = requests
	res.Limits = limits
	return res
}

func getVolumeResourceRequirements(requests, limits api.ResourceList) api.VolumeResourceRequirements {
	res := api.VolumeResourceRequirements{}
	res.Requests = requests
	res.Limits = limits
	return res
}

// createLimitRange creates a limit range with the specified data
func createLimitRange(limitType api.LimitType, min, max, defaultLimit, defaultRequest, maxLimitRequestRatio api.ResourceList) corev1.LimitRange {
	internalLimitRage := api.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "abc",
			Namespace: "test",
		},
		Spec: api.LimitRangeSpec{
			Limits: []api.LimitRangeItem{
				{
					Type:                 limitType,
					Min:                  min,
					Max:                  max,
					Default:              defaultLimit,
					DefaultRequest:       defaultRequest,
					MaxLimitRequestRatio: maxLimitRequestRatio,
				},
			},
		},
	}
	externalLimitRange := corev1.LimitRange{}
	v1.Convert_core_LimitRange_To_v1_LimitRange(&internalLimitRage, &externalLimitRange, nil)
	return externalLimitRange
}

func validLimitRange() corev1.LimitRange {
	internalLimitRange := api.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "abc",
			Namespace: "test",
		},
		Spec: api.LimitRangeSpec{
			Limits: []api.LimitRangeItem{
				{
					Type: api.LimitTypePod,
					Max:  getComputeResourceList("200m", "4Gi"),
					Min:  getComputeResourceList("50m", "2Mi"),
				},
				{
					Type:           api.LimitTypeContainer,
					Max:            getComputeResourceList("100m", "2Gi"),
					Min:            getComputeResourceList("25m", "1Mi"),
					Default:        getComputeResourceList("75m", "10Mi"),
					DefaultRequest: getComputeResourceList("50m", "5Mi"),
				},
			},
		},
	}
	externalLimitRange := corev1.LimitRange{}
	v1.Convert_core_LimitRange_To_v1_LimitRange(&internalLimitRange, &externalLimitRange, nil)
	return externalLimitRange
}

func validLimitRangeNoDefaults() corev1.LimitRange {
	internalLimitRange := api.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "abc",
			Namespace: "test",
		},
		Spec: api.LimitRangeSpec{
			Limits: []api.LimitRangeItem{
				{
					Type: api.LimitTypePod,
					Max:  getComputeResourceList("200m", "4Gi"),
					Min:  getComputeResourceList("50m", "2Mi"),
				},
				{
					Type: api.LimitTypeContainer,
					Max:  getComputeResourceList("100m", "2Gi"),
					Min:  getComputeResourceList("25m", "1Mi"),
				},
			},
		},
	}
	externalLimitRange := corev1.LimitRange{}
	v1.Convert_core_LimitRange_To_v1_LimitRange(&internalLimitRange, &externalLimitRange, nil)
	return externalLimitRange
}

func validPodWithPodLevelResources(name string, numContainers int, containerResources api.ResourceRequirements, podResources api.ResourceRequirements) api.Pod {
	pod := validPod(name, numContainers, containerResources)
	pod.Spec.Resources = &podResources
	return pod
}

func validPod(name string, numContainers int, resources api.ResourceRequirements) api.Pod {
	pod := api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Spec:       api.PodSpec{},
	}
	pod.Spec.Containers = make([]api.Container, 0, numContainers)
	for i := 0; i < numContainers; i++ {
		pod.Spec.Containers = append(pod.Spec.Containers, api.Container{
			Image:     "foo:V" + strconv.Itoa(i),
			Resources: resources,
			Name:      "foo-" + strconv.Itoa(i),
		})
	}
	return pod
}

func validPodInit(pod api.Pod, resources ...api.ResourceRequirements) api.Pod {
	for i := 0; i < len(resources); i++ {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, api.Container{
			Image:     "foo:V" + strconv.Itoa(i),
			Resources: resources[i],
			Name:      "foo-" + strconv.Itoa(i),
		})
	}
	return pod
}

func TestDefaultContainerResourceRequirements(t *testing.T) {
	limitRange := validLimitRange()
	expected := api.ResourceRequirements{
		Requests: getComputeResourceList("50m", "5Mi"),
		Limits:   getComputeResourceList("75m", "10Mi"),
	}

	actual := defaultContainerResourceRequirements(&limitRange)
	if !apiequality.Semantic.DeepEqual(expected, actual) {
		t.Errorf("actual.Limits != expected.Limits; %v != %v", actual.Limits, expected.Limits)
		t.Errorf("actual.Requests != expected.Requests; %v != %v", actual.Requests, expected.Requests)
		t.Errorf("expected != actual; %v != %v", expected, actual)
	}
}

func verifyAnnotation(t *testing.T, pod *api.Pod, expected string) {
	a, ok := pod.ObjectMeta.Annotations[limitRangerAnnotation]
	if !ok {
		t.Errorf("No annotation but expected %v", expected)
	}
	if a != expected {
		t.Errorf("Wrong annotation set by Limit Ranger: got %v, expected %v", a, expected)
	}
}

func expectNoAnnotation(t *testing.T, pod *api.Pod) {
	if a, ok := pod.ObjectMeta.Annotations[limitRangerAnnotation]; ok {
		t.Errorf("Expected no annotation but got %v", a)
	}
}

func TestMergePodResourceRequirements(t *testing.T) {
	limitRange := validLimitRange()

	// pod with no resources enumerated should get each resource from default request
	expected := getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", ""))
	pod := validPod("empty-resources", 1, expected)
	defaultRequirements := defaultContainerResourceRequirements(&limitRange)
	mergePodResourceRequirements(&pod, &defaultRequirements)
	for i := range pod.Spec.Containers {
		actual := pod.Spec.Containers[i].Resources
		if !apiequality.Semantic.DeepEqual(expected, actual) {
			t.Errorf("pod %v, expected != actual; %v != %v", pod.Name, expected, actual)
		}
	}
	verifyAnnotation(t, &pod, "LimitRanger plugin set: cpu, memory request for container foo-0; cpu, memory limit for container foo-0")

	// pod with some resources enumerated should only merge empty
	input := getResourceRequirements(getComputeResourceList("", "512Mi"), getComputeResourceList("", ""))
	pod = validPodInit(validPod("limit-memory", 1, input), input)
	expected = api.ResourceRequirements{
		Requests: api.ResourceList{
			api.ResourceCPU:    defaultRequirements.Requests[api.ResourceCPU],
			api.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: defaultRequirements.Limits,
	}
	mergePodResourceRequirements(&pod, &defaultRequirements)
	for i := range pod.Spec.Containers {
		actual := pod.Spec.Containers[i].Resources
		if !apiequality.Semantic.DeepEqual(expected, actual) {
			t.Errorf("pod %v, expected != actual; %v != %v", pod.Name, expected, actual)
		}
	}
	for i := range pod.Spec.InitContainers {
		actual := pod.Spec.InitContainers[i].Resources
		if !apiequality.Semantic.DeepEqual(expected, actual) {
			t.Errorf("pod %v, expected != actual; %v != %v", pod.Name, expected, actual)
		}
	}
	verifyAnnotation(t, &pod, "LimitRanger plugin set: cpu request for container foo-0; cpu, memory limit for container foo-0")

	// pod with all resources enumerated should not merge anything
	input = getResourceRequirements(getComputeResourceList("100m", "512Mi"), getComputeResourceList("200m", "1G"))
	initInputs := []api.ResourceRequirements{getResourceRequirements(getComputeResourceList("200m", "1G"), getComputeResourceList("400m", "2G"))}
	pod = validPodInit(validPod("limit-memory", 1, input), initInputs...)
	expected = input
	mergePodResourceRequirements(&pod, &defaultRequirements)
	for i := range pod.Spec.Containers {
		actual := pod.Spec.Containers[i].Resources
		if !apiequality.Semantic.DeepEqual(expected, actual) {
			t.Errorf("pod %v, expected != actual; %v != %v", pod.Name, expected, actual)
		}
	}
	for i := range pod.Spec.InitContainers {
		actual := pod.Spec.InitContainers[i].Resources
		if !apiequality.Semantic.DeepEqual(initInputs[i], actual) {
			t.Errorf("pod %v, expected != actual; %v != %v", pod.Name, initInputs[i], actual)
		}
	}
	expectNoAnnotation(t, &pod)
}

func TestPodLimitFunc(t *testing.T) {
	type testCase struct {
		pod                      api.Pod
		limitRange               corev1.LimitRange
		podLevelResourcesEnabled bool
	}

	successCases := []testCase{
		{
			pod:        validPod("ctr-min-cpu-request", 1, getResourceRequirements(getComputeResourceList("100m", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("50m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-cpu-request-limit", 1, getResourceRequirements(getComputeResourceList("100m", ""), getComputeResourceList("200m", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("50m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-memory-request", 1, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("", "50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-memory-request-limit", 1, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", "100Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("", "50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-cpu-request-limit", 1, getResourceRequirements(getComputeResourceList("500m", ""), getComputeResourceList("1", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-cpu-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("1", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-mem-request-limit", 1, getResourceRequirements(getComputeResourceList("", "250Mi"), getComputeResourceList("", "500Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-cpu-ratio", 1, getResourceRequirements(getComputeResourceList("500m", ""), getComputeResourceList("750m", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, getComputeResourceList("1.5", "")),
		},
		{
			pod:        validPod("ctr-max-mem-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", "500Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-cpu-request", 2, getResourceRequirements(getComputeResourceList("75m", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("100m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-cpu-request-limit", 2, getResourceRequirements(getComputeResourceList("75m", ""), getComputeResourceList("200m", ""))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("100m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-memory-request", 2, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("", "100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-memory-request-limit", 2, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", "100Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("", "100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-min-memory-request", 2, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", ""))),
				getResourceRequirements(getComputeResourceList("", "100Mi"), getComputeResourceList("", "")),
			),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("", "100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-min-memory-request-limit", 2, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", "100Mi"))),
				getResourceRequirements(getComputeResourceList("", "80Mi"), getComputeResourceList("", "100Mi")),
			),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("", "100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-cpu-request-limit", 2, getResourceRequirements(getComputeResourceList("500m", ""), getComputeResourceList("1", ""))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-cpu-limit", 2, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("1", ""))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-max-cpu-request-limit", 2, getResourceRequirements(getComputeResourceList("500m", ""), getComputeResourceList("1", ""))),
				getResourceRequirements(getComputeResourceList("1", ""), getComputeResourceList("2", "")),
				getResourceRequirements(getComputeResourceList("1", ""), getComputeResourceList("1", "")),
			),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-max-cpu-limit", 2, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("1", ""))),
				getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("2", "")),
				getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("2", "")),
			),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-mem-request-limit", 2, getResourceRequirements(getComputeResourceList("", "250Mi"), getComputeResourceList("", "500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-mem-limit", 2, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", "500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-mem-ratio", 3, getResourceRequirements(getComputeResourceList("", "300Mi"), getComputeResourceList("", "450Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("", "2Gi"), api.ResourceList{}, api.ResourceList{}, getComputeResourceList("", "1.5")),
		},
		{
			pod:        validPod("ctr-1-min-local-ephemeral-storage-request", 1, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-min-local-ephemeral-storage-request-limit", 1, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList("100Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-max-local-ephemeral-storage-request-limit", 1, getResourceRequirements(getLocalStorageResourceList("250Mi"), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-max-local-ephemeral-storage-limit", 1, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-min-local-ephemeral-storage-request", 2, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-min-local-ephemeral-storage-request-limit", 2, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList("100Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-max-local-ephemeral-storage-request-limit", 2, getResourceRequirements(getLocalStorageResourceList("250Mi"), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("600Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-max-local-ephemeral-storage-limit", 2, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("600Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-local-ephemeral-storage-request", 2, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePod, getLocalStorageResourceList("100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-local-ephemeral-storage-request-limit", 2, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList("100Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, getLocalStorageResourceList("100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-min-local-ephemeral-storage-request", 2, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList(""))),
				getResourceRequirements(getLocalStorageResourceList("100Mi"), getLocalStorageResourceList("")),
			),
			limitRange: createLimitRange(api.LimitTypePod, getLocalStorageResourceList("100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-min-local-ephemeral-storage-request-limit", 2, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList("100Mi"))),
				getResourceRequirements(getLocalStorageResourceList("80Mi"), getLocalStorageResourceList("100Mi")),
			),
			limitRange: createLimitRange(api.LimitTypePod, getLocalStorageResourceList("100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-local-ephemeral-storage-request-limit", 2, getResourceRequirements(getLocalStorageResourceList("250Mi"), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-local-ephemeral-storage-limit", 2, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-local-ephemeral-storage-ratio", 3, getResourceRequirements(getLocalStorageResourceList("300Mi"), getLocalStorageResourceList("450Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getLocalStorageResourceList("2Gi"), api.ResourceList{}, api.ResourceList{}, getLocalStorageResourceList("1.5")),
		},
		{
			pod: validPodWithPodLevelResources("pod-level-resources-with-min-max", 3, getResourceRequirements(getComputeResourceList("100m", "60Mi"), getComputeResourceList("200m", "100Mi")),
				getResourceRequirements(getComputeResourceList("200m", "180Mi"), getComputeResourceList("400m", "200Mi")),
			),
			limitRange:               createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("400m", "200Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
			podLevelResourcesEnabled: true,
		},
		{
			pod: validPodWithPodLevelResources("pod-level-requests-with-min", 3, getResourceRequirements(getComputeResourceList("50m", "60Mi"), getComputeResourceList("", "")),
				getResourceRequirements(getComputeResourceList("160m", "200Mi"), getComputeResourceList("", "")),
			),
			limitRange:               createLimitRange(api.LimitTypePod, getComputeResourceList("160m", "200Mi"), getComputeResourceList("", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
			podLevelResourcesEnabled: true,
		},
		{
			pod: validPodWithPodLevelResources("pod-level-limits-with-max", 3, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("50m", "60Mi")),
				getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("160m", "200Mi")),
			),
			limitRange:               createLimitRange(api.LimitTypePod, getComputeResourceList("", ""), getComputeResourceList("160m", "200Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
			podLevelResourcesEnabled: true,
		},
	}
	for i := range successCases {
		test := successCases[i]
		t.Run(test.pod.Name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLevelResources, test.podLevelResourcesEnabled)
			err := PodMutateLimitFunc(&test.limitRange, &test.pod)
			if err != nil {
				t.Errorf("Unexpected error for pod: %s, %v", test.pod.Name, err)
			}

			err = PodValidateLimitFunc(&test.limitRange, &test.pod)
			if err != nil {
				t.Errorf("Unexpected error for pod: %s, %v", test.pod.Name, err)
			}
		})
	}

	errorCases := []testCase{
		{
			pod:        validPod("ctr-min-cpu-request", 1, getResourceRequirements(getComputeResourceList("40m", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("50m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-cpu-request-limit", 1, getResourceRequirements(getComputeResourceList("40m", ""), getComputeResourceList("200m", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("50m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-cpu-no-request-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("50m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-memory-request", 1, getResourceRequirements(getComputeResourceList("", "40Mi"), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("", "50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-memory-request-limit", 1, getResourceRequirements(getComputeResourceList("", "40Mi"), getComputeResourceList("", "100Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("", "50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-min-memory-no-request-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getComputeResourceList("", "50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-cpu-request-limit", 1, getResourceRequirements(getComputeResourceList("500m", ""), getComputeResourceList("2500m", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-cpu-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("2500m", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-cpu-no-request-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-cpu-ratio", 1, getResourceRequirements(getComputeResourceList("1250m", ""), getComputeResourceList("2500m", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, getComputeResourceList("1", "")),
		},
		{
			pod:        validPod("ctr-max-mem-request-limit", 1, getResourceRequirements(getComputeResourceList("", "250Mi"), getComputeResourceList("", "2Gi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-mem-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", "2Gi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-max-mem-no-request-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-cpu-request", 1, getResourceRequirements(getComputeResourceList("75m", ""), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("100m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-cpu-request-limit", 1, getResourceRequirements(getComputeResourceList("75m", ""), getComputeResourceList("200m", ""))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("100m", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-memory-request", 1, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", ""))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("", "100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-memory-request-limit", 1, getResourceRequirements(getComputeResourceList("", "60Mi"), getComputeResourceList("", "100Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, getComputeResourceList("", "100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-cpu-request-limit", 3, getResourceRequirements(getComputeResourceList("500m", ""), getComputeResourceList("1", ""))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-cpu-limit", 3, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("1", ""))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-mem-request-limit", 3, getResourceRequirements(getComputeResourceList("", "250Mi"), getComputeResourceList("", "500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-mem-limit", 3, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", "500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-max-mem-limit", 1, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", "500Mi"))),
				getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("", "1.5Gi")),
			),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("", "1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-mem-ratio", 3, getResourceRequirements(getComputeResourceList("", "250Mi"), getComputeResourceList("", "500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("", "2Gi"), api.ResourceList{}, api.ResourceList{}, getComputeResourceList("", "1.5")),
		},
		{
			pod:        validPod("ctr-1-min-local-ephemeral-storage-request", 1, getResourceRequirements(getLocalStorageResourceList("40Mi"), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-min-local-ephemeral-storage-request-limit", 1, getResourceRequirements(getLocalStorageResourceList("40Mi"), getLocalStorageResourceList("100Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-min-local-ephemeral-storage-no-request-limit", 1, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-max-local-ephemeral-storage-request-limit", 1, getResourceRequirements(getLocalStorageResourceList("250Mi"), getLocalStorageResourceList("2Gi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-max-local-ephemeral-storage-limit", 1, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("2Gi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-1-max-local-ephemeral-storage-no-request-limit", 1, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-min-local-ephemeral-storage-request", 2, getResourceRequirements(getLocalStorageResourceList("40Mi"), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-min-local-ephemeral-storage-request-limit", 2, getResourceRequirements(getLocalStorageResourceList("40Mi"), getLocalStorageResourceList("100Mi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-min-local-ephemeral-storage-no-request-limit", 2, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, getLocalStorageResourceList("50Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-max-local-ephemeral-storage-request-limit", 2, getResourceRequirements(getLocalStorageResourceList("250Mi"), getLocalStorageResourceList("2Gi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-max-local-ephemeral-storage-limit", 2, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("2Gi"))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("ctr-2-max-local-ephemeral-storage-no-request-limit", 2, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypeContainer, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-local-ephemeral-storage-request", 1, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePod, getLocalStorageResourceList("100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-min-local-ephemeral-storage-request-limit", 1, getResourceRequirements(getLocalStorageResourceList("60Mi"), getLocalStorageResourceList("100Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, getLocalStorageResourceList("100Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-local-ephemeral-storage-request-limit", 3, getResourceRequirements(getLocalStorageResourceList("250Mi"), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-local-ephemeral-storage-limit", 3, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod: validPodInit(
				validPod("pod-init-max-local-ephemeral-storage-limit", 1, getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("500Mi"))),
				getResourceRequirements(getLocalStorageResourceList(""), getLocalStorageResourceList("1.5Gi")),
			),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getLocalStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pod:        validPod("pod-max-local-ephemeral-storage-ratio", 3, getResourceRequirements(getLocalStorageResourceList("250Mi"), getLocalStorageResourceList("500Mi"))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getLocalStorageResourceList("2Gi"), api.ResourceList{}, api.ResourceList{}, getLocalStorageResourceList("1.5")),
		},
		{
			pod: withRestartableInitContainer(getComputeResourceList("1500m", ""), api.ResourceList{},
				validPod("ctr-max-cpu-limit-restartable-init-container", 1, getResourceRequirements(getComputeResourceList("1000m", ""), getComputeResourceList("1500m", "")))),
			limitRange: createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("2", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		}, {
			pod: validPodWithPodLevelResources("pod-level-resources-exceeding-max", 3, getResourceRequirements(getComputeResourceList("100m", "60Mi"), getComputeResourceList("200m", "100Mi")),
				getResourceRequirements(getComputeResourceList("200m", "180Mi"), getComputeResourceList("500m", "280Mi")),
			),
			limitRange:               createLimitRange(api.LimitTypePod, api.ResourceList{}, getComputeResourceList("400m", "200Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
			podLevelResourcesEnabled: true,
		},
		{
			pod: validPodWithPodLevelResources("pod-level-requests-less-than-min", 3, getResourceRequirements(getComputeResourceList("50m", "60Mi"), getComputeResourceList("", "")),
				getResourceRequirements(getComputeResourceList("100m", "200Mi"), getComputeResourceList("", "")),
			),
			limitRange:               createLimitRange(api.LimitTypePod, getComputeResourceList("160m", "200Mi"), getComputeResourceList("", ""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
			podLevelResourcesEnabled: true,
		},
		{
			pod: validPodWithPodLevelResources("pod-level-limits-exceeding-max", 3, getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("50m", "60Mi")),
				getResourceRequirements(getComputeResourceList("", ""), getComputeResourceList("160m", "300Mi")),
			),
			limitRange:               createLimitRange(api.LimitTypePod, getComputeResourceList("", ""), getComputeResourceList("160m", "200Mi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
			podLevelResourcesEnabled: true,
		},
	}
	for i := range errorCases {
		test := errorCases[i]
		t.Run(test.pod.Name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLevelResources, test.podLevelResourcesEnabled)
			err := PodMutateLimitFunc(&test.limitRange, &test.pod)
			if err != nil {
				t.Errorf("Unexpected error for pod: %s, %v", test.pod.Name, err)
			}
			err = PodValidateLimitFunc(&test.limitRange, &test.pod)
			if err == nil {
				t.Errorf("Expected error for pod: %s", test.pod.Name)
			}
		})
	}
}

func withRestartableInitContainer(requests, limits api.ResourceList, pod api.Pod) api.Pod {
	policyAlways := api.ContainerRestartPolicyAlways
	pod.Spec.InitContainers = append(pod.Spec.InitContainers,
		api.Container{
			RestartPolicy: &policyAlways,
			Image:         "foo:V" + strconv.Itoa(len(pod.Spec.InitContainers)),
			Resources:     getResourceRequirements(requests, limits),
			Name:          "foo-" + strconv.Itoa(len(pod.Spec.InitContainers)),
		})
	return pod
}

func getLocalStorageResourceList(ephemeralStorage string) api.ResourceList {
	res := api.ResourceList{}
	if ephemeralStorage != "" {
		res[api.ResourceEphemeralStorage] = resource.MustParse(ephemeralStorage)
	}
	return res
}

func TestPodLimitFuncApplyDefault(t *testing.T) {
	limitRange := validLimitRange()
	testPod := validPodInit(validPod("foo", 1, getResourceRequirements(api.ResourceList{}, api.ResourceList{})), getResourceRequirements(api.ResourceList{}, api.ResourceList{}))
	err := PodMutateLimitFunc(&limitRange, &testPod)
	if err != nil {
		t.Errorf("Unexpected error for valid pod: %s, %v", testPod.Name, err)
	}

	for i := range testPod.Spec.Containers {
		container := testPod.Spec.Containers[i]
		limitMemory := container.Resources.Limits.Memory().String()
		limitCPU := container.Resources.Limits.CPU().String()
		requestMemory := container.Resources.Requests.Memory().String()
		requestCPU := container.Resources.Requests.CPU().String()

		if limitMemory != "10Mi" {
			t.Errorf("Unexpected limit memory value %s", limitMemory)
		}
		if limitCPU != "75m" {
			t.Errorf("Unexpected limit cpu value %s", limitCPU)
		}
		if requestMemory != "5Mi" {
			t.Errorf("Unexpected request memory value %s", requestMemory)
		}
		if requestCPU != "50m" {
			t.Errorf("Unexpected request cpu value %s", requestCPU)
		}
	}

	for i := range testPod.Spec.InitContainers {
		container := testPod.Spec.InitContainers[i]
		limitMemory := container.Resources.Limits.Memory().String()
		limitCPU := container.Resources.Limits.CPU().String()
		requestMemory := container.Resources.Requests.Memory().String()
		requestCPU := container.Resources.Requests.CPU().String()

		if limitMemory != "10Mi" {
			t.Errorf("Unexpected limit memory value %s", limitMemory)
		}
		if limitCPU != "75m" {
			t.Errorf("Unexpected limit cpu value %s", limitCPU)
		}
		if requestMemory != "5Mi" {
			t.Errorf("Unexpected request memory value %s", requestMemory)
		}
		if requestCPU != "50m" {
			t.Errorf("Unexpected request cpu value %s", requestCPU)
		}
	}
}

func TestLimitRangerIgnoresSubresource(t *testing.T) {
	limitRange := validLimitRangeNoDefaults()
	mockClient := newMockClientForTest([]corev1.LimitRange{limitRange})
	handler, informerFactory, err := newHandlerForTest(mockClient)
	if err != nil {
		t.Errorf("unexpected error initializing handler: %v", err)
	}
	informerFactory.Start(wait.NeverStop)

	testPod := validPod("testPod", 1, api.ResourceRequirements{})
	err = admissiontesting.WithReinvocationTesting(t, handler).Admit(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
	if err == nil {
		t.Errorf("Expected an error since the pod did not specify resource limits in its create call")
	}
	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "", admission.Update, &metav1.UpdateOptions{}, false, nil), nil)
	if err != nil {
		t.Errorf("Expected not to call limitranger actions on pod updates")
	}

	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "status", admission.Update, &metav1.UpdateOptions{}, false, nil), nil)
	if err != nil {
		t.Errorf("Should have ignored calls to any subresource of pod %v", err)
	}
}

func TestLimitRangerAllowPodResize(t *testing.T) {
	limitRange := validLimitRangeNoDefaults()
	mockClient := newMockClientForTest([]corev1.LimitRange{limitRange})
	handler, informerFactory, err := newHandlerForTest(mockClient)
	if err != nil {
		t.Errorf("unexpected error initializing handler: %v", err)
	}
	informerFactory.Start(wait.NeverStop)

	testPod := validPod("testPod", 1, api.ResourceRequirements{})
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)
	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "resize", admission.Update, &metav1.UpdateOptions{}, false, nil), nil)
	if err == nil {
		t.Errorf("expect error, but got nil")
	}
}

func TestLimitRangerAdmitPod(t *testing.T) {
	limitRange := validLimitRangeNoDefaults()
	mockClient := newMockClientForTest([]corev1.LimitRange{limitRange})
	handler, informerFactory, err := newHandlerForTest(mockClient)
	if err != nil {
		t.Errorf("unexpected error initializing handler: %v", err)
	}
	informerFactory.Start(wait.NeverStop)

	testPod := validPod("testPod", 1, api.ResourceRequirements{})
	err = admissiontesting.WithReinvocationTesting(t, handler).Admit(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
	if err == nil {
		t.Errorf("Expected an error since the pod did not specify resource limits in its create call")
	}
	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "", admission.Update, &metav1.UpdateOptions{}, false, nil), nil)
	if err != nil {
		t.Errorf("Expected not to call limitranger actions on pod updates")
	}

	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&testPod, nil, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "testPod", api.Resource("pods").WithVersion("version"), "status", admission.Update, &metav1.UpdateOptions{}, false, nil), nil)
	if err != nil {
		t.Errorf("Should have ignored calls to any subresource of pod %v", err)
	}

	// a pod that is undergoing termination should never be blocked
	terminatingPod := validPod("terminatingPod", 1, api.ResourceRequirements{})
	now := metav1.Now()
	terminatingPod.DeletionTimestamp = &now
	err = handler.Validate(context.TODO(), admission.NewAttributesRecord(&terminatingPod, &terminatingPod, api.Kind("Pod").WithVersion("version"), limitRange.Namespace, "terminatingPod", api.Resource("pods").WithVersion("version"), "", admission.Update, &metav1.UpdateOptions{}, false, nil), nil)
	if err != nil {
		t.Errorf("LimitRange should ignore a pod marked for termination")
	}
}

// newMockClientForTest creates a mock client that returns a client configured for the specified list of limit ranges
func newMockClientForTest(limitRanges []corev1.LimitRange) *fake.Clientset {
	mockClient := &fake.Clientset{}
	mockClient.AddReactor("list", "limitranges", func(action core.Action) (bool, runtime.Object, error) {
		limitRangeList := &corev1.LimitRangeList{
			ListMeta: metav1.ListMeta{
				ResourceVersion: fmt.Sprintf("%d", len(limitRanges)),
			},
		}
		for index, value := range limitRanges {
			value.ResourceVersion = fmt.Sprintf("%d", index)
			limitRangeList.Items = append(limitRangeList.Items, value)
		}
		return true, limitRangeList, nil
	})
	return mockClient
}

// newHandlerForTest returns a handler configured for testing.
func newHandlerForTest(c clientset.Interface) (*LimitRanger, informers.SharedInformerFactory, error) {
	f := informers.NewSharedInformerFactory(c, 5*time.Minute)
	handler, err := NewLimitRanger(&DefaultLimitRangerActions{})
	if err != nil {
		return nil, f, err
	}
	pluginInitializer := genericadmissioninitializer.New(c, nil, f, nil, nil, nil, nil)
	pluginInitializer.Initialize(handler)
	err = admission.ValidateInitialization(handler)
	return handler, f, err
}

func validPersistentVolumeClaim(name string, resources api.VolumeResourceRequirements) api.PersistentVolumeClaim {
	pvc := api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Spec: api.PersistentVolumeClaimSpec{
			Resources: resources,
		},
	}
	return pvc
}

func TestPersistentVolumeClaimLimitFunc(t *testing.T) {
	type testCase struct {
		pvc        api.PersistentVolumeClaim
		limitRange corev1.LimitRange
	}

	successCases := []testCase{
		{
			pvc:        validPersistentVolumeClaim("pvc-is-min-storage-request", getVolumeResourceRequirements(getStorageResourceList("1Gi"), getStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePersistentVolumeClaim, getStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pvc:        validPersistentVolumeClaim("pvc-is-max-storage-request", getVolumeResourceRequirements(getStorageResourceList("1Gi"), getStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePersistentVolumeClaim, api.ResourceList{}, getStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pvc:        validPersistentVolumeClaim("pvc-no-minmax-storage-request", getVolumeResourceRequirements(getStorageResourceList("100Gi"), getStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePersistentVolumeClaim, getStorageResourceList(""), getStorageResourceList(""), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pvc:        validPersistentVolumeClaim("pvc-within-minmax-storage-request", getVolumeResourceRequirements(getStorageResourceList("5Gi"), getStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePersistentVolumeClaim, getStorageResourceList("1Gi"), getStorageResourceList("10Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
	}
	for i := range successCases {
		test := successCases[i]
		err := PersistentVolumeClaimValidateLimitFunc(&test.limitRange, &test.pvc)
		if err != nil {
			t.Errorf("Unexpected error for pvc: %s, %v", test.pvc.Name, err)
		}
	}

	errorCases := []testCase{
		{
			pvc:        validPersistentVolumeClaim("pvc-below-min-storage-request", getVolumeResourceRequirements(getStorageResourceList("500Mi"), getStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePersistentVolumeClaim, getStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
		{
			pvc:        validPersistentVolumeClaim("pvc-exceeds-max-storage-request", getVolumeResourceRequirements(getStorageResourceList("100Gi"), getStorageResourceList(""))),
			limitRange: createLimitRange(api.LimitTypePersistentVolumeClaim, getStorageResourceList("1Gi"), getStorageResourceList("1Gi"), api.ResourceList{}, api.ResourceList{}, api.ResourceList{}),
		},
	}
	for i := range errorCases {
		test := errorCases[i]
		err := PersistentVolumeClaimValidateLimitFunc(&test.limitRange, &test.pvc)
		if err == nil {
			t.Errorf("Expected error for pvc: %s", test.pvc.Name)
		}
	}
}

// TestLimitRanger_GetLimitRangesFixed22422 Fixed Admission controllers can cause unnecessary significant load on apiserver #22422
func TestLimitRanger_GetLimitRangesFixed22422(t *testing.T) {
	limitRange := validLimitRangeNoDefaults()
	limitRanges := []corev1.LimitRange{limitRange}

	mockClient := &fake.Clientset{}

	var (
		testCount  int64
		test1Count int64
	)
	mockClient.AddReactor("list", "limitranges", func(action core.Action) (bool, runtime.Object, error) {
		switch action.GetNamespace() {
		case "test":
			atomic.AddInt64(&testCount, 1)
		case "test1":
			atomic.AddInt64(&test1Count, 1)
		default:
			t.Error("unexpected namespace")
		}

		limitRangeList := &corev1.LimitRangeList{
			ListMeta: metav1.ListMeta{
				ResourceVersion: fmt.Sprintf("%d", len(limitRanges)),
			},
		}
		for index, value := range limitRanges {
			value.ResourceVersion = fmt.Sprintf("%d", index)
			value.Namespace = action.GetNamespace()
			limitRangeList.Items = append(limitRangeList.Items, value)
		}
		// make the handler slow so concurrent calls exercise the singleflight
		time.Sleep(time.Second)
		return true, limitRangeList, nil
	})

	handler, _, err := newHandlerForTest(mockClient)
	if err != nil {
		t.Errorf("unexpected error initializing handler: %v", err)
	}

	attributes := admission.NewAttributesRecord(nil, nil, api.Kind("kind").WithVersion("version"), "test", "name", api.Resource("resource").WithVersion("version"), "subresource", admission.Create, &metav1.CreateOptions{}, false, nil)

	attributesTest1 := admission.NewAttributesRecord(nil, nil, api.Kind("kind").WithVersion("version"), "test1", "name", api.Resource("resource").WithVersion("version"), "subresource", admission.Create, &metav1.CreateOptions{}, false, nil)

	wg := sync.WaitGroup{}
	for i := 0; i < 10; i++ {
		wg.Add(2)
		// simulating concurrent calls after a cache failure
		go func() {
			defer wg.Done()
			ret, err := handler.GetLimitRanges(attributes)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			for _, c := range ret {
				if c.Namespace != attributes.GetNamespace() {
					t.Errorf("Expected %s namespace, got %s", attributes.GetNamespace(), c.Namespace)
				}
			}
		}()

		// simulation of different namespaces is not a call
		go func() {
			defer wg.Done()
			ret, err := handler.GetLimitRanges(attributesTest1)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			for _, c := range ret {
				if c.Namespace != attributesTest1.GetNamespace() {
					t.Errorf("Expected %s namespace, got %s", attributesTest1.GetNamespace(), c.Namespace)
				}
			}
		}()
	}

	// and here we wait for all the goroutines
	wg.Wait()
	// since all the calls with the same namespace will be holded, they must be caught on the singleflight group,
	// There are two different sets of namespace calls
	// hence only 2
	if testCount != 1 {
		t.Errorf("Expected 1 limit range call, got %d", testCount)
	}
	if test1Count != 1 {
		t.Errorf("Expected 1 limit range call, got %d", test1Count)
	}

	// invalidate the cache
	handler.liveLookupCache.Remove(attributes.GetNamespace())
	_, err = handler.GetLimitRanges(attributes)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if testCount != 2 {
		t.Errorf("Expected 2 limit range call, got %d", testCount)
	}
	if test1Count != 1 {
		t.Errorf("Expected 1 limit range call, got %d", test1Count)
	}
}
