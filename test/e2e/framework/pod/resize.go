/*
Copyright 2024 The Kubernetes Authors.

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

package pod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	MinContainerRuntimeVersion string = "1.6.9"
)

type ResizableContainerInfo struct {
	Name         string
	Resources    *ContainerResources
	CPUPolicy    *v1.ResourceResizeRestartPolicy
	MemPolicy    *v1.ResourceResizeRestartPolicy
	RestartCount int32
}

type containerPatch struct {
	Name      string `json:"name"`
	Resources struct {
		Requests struct {
			CPU     string `json:"cpu,omitempty"`
			Memory  string `json:"memory,omitempty"`
			EphStor string `json:"ephemeral-storage,omitempty"`
		} `json:"requests"`
		Limits struct {
			CPU     string `json:"cpu,omitempty"`
			Memory  string `json:"memory,omitempty"`
			EphStor string `json:"ephemeral-storage,omitempty"`
		} `json:"limits"`
	} `json:"resources"`
}

type patchSpec struct {
	Spec struct {
		Containers []containerPatch `json:"containers"`
	} `json:"spec"`
}

func getTestResizePolicy(tcInfo ResizableContainerInfo) (resizePol []v1.ContainerResizePolicy) {
	if tcInfo.CPUPolicy != nil {
		cpuPol := v1.ContainerResizePolicy{ResourceName: v1.ResourceCPU, RestartPolicy: *tcInfo.CPUPolicy}
		resizePol = append(resizePol, cpuPol)
	}
	if tcInfo.MemPolicy != nil {
		memPol := v1.ContainerResizePolicy{ResourceName: v1.ResourceMemory, RestartPolicy: *tcInfo.MemPolicy}
		resizePol = append(resizePol, memPol)
	}
	return resizePol
}

func makeResizableContainer(tcInfo ResizableContainerInfo) v1.Container {
	cmd := "grep Cpus_allowed_list /proc/self/status | cut -f2 && sleep 1d"
	resizePol := getTestResizePolicy(tcInfo)
	tc := MakeContainerWithResources(tcInfo.Name, tcInfo.Resources)
	tc.Command = []string{"/bin/sh"}
	tc.Args = []string{"-c", cmd}
	tc.ResizePolicy = resizePol

	return tc
}

func MakePodWithResizableContainers(ns, name, timeStamp string, tcInfo []ResizableContainerInfo) *v1.Pod {
	var testContainers []v1.Container

	for _, ci := range tcInfo {
		tc := makeResizableContainer(ci)
		testContainers = append(testContainers, tc)
	}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"time": timeStamp,
			},
		},
		Spec: v1.PodSpec{
			OS:            &v1.PodOS{Name: v1.Linux},
			Containers:    testContainers,
			RestartPolicy: v1.RestartPolicyOnFailure,
		},
	}
	return pod
}

func VerifyPodResizePolicy(gotPod *v1.Pod, wantCtrs []ResizableContainerInfo) {
	ginkgo.GinkgoHelper()
	gomega.Expect(gotPod.Spec.Containers).To(gomega.HaveLen(len(wantCtrs)), "number of containers in pod spec should match")
	for i, wantCtr := range wantCtrs {
		gotCtr := &gotPod.Spec.Containers[i]
		ctr := makeResizableContainer(wantCtr)
		gomega.Expect(gotCtr.Name).To(gomega.Equal(ctr.Name))
		gomega.Expect(gotCtr.ResizePolicy).To(gomega.Equal(ctr.ResizePolicy))
	}
}

func VerifyPodResources(gotPod *v1.Pod, wantCtrs []ResizableContainerInfo) {
	ginkgo.GinkgoHelper()
	gomega.Expect(gotPod.Spec.Containers).To(gomega.HaveLen(len(wantCtrs)), "number of containers in pod spec should match")
	for i, wantCtr := range wantCtrs {
		gotCtr := &gotPod.Spec.Containers[i]
		ctr := makeResizableContainer(wantCtr)
		gomega.Expect(gotCtr.Name).To(gomega.Equal(ctr.Name))
		gomega.Expect(gotCtr.Resources).To(gomega.Equal(ctr.Resources))
	}
}

func VerifyPodStatusResources(gotPod *v1.Pod, wantCtrs []ResizableContainerInfo) error {
	ginkgo.GinkgoHelper()

	var errs []error

	if len(gotPod.Status.ContainerStatuses) != len(wantCtrs) {
		return fmt.Errorf("expectation length mismatch: got %d statuses, want %d",
			len(gotPod.Status.ContainerStatuses), len(wantCtrs))
	}
	for i, wantCtr := range wantCtrs {
		gotCtrStatus := &gotPod.Status.ContainerStatuses[i]
		ctr := makeResizableContainer(wantCtr)
		if gotCtrStatus.Name != ctr.Name {
			errs = append(errs, fmt.Errorf("container status %d name %q != expected name %q", i, gotCtrStatus.Name, ctr.Name))
			continue
		}
		if err := framework.Gomega().Expect(*gotCtrStatus.Resources).To(gomega.Equal(ctr.Resources)); err != nil {
			errs = append(errs, fmt.Errorf("container[%s] status resources mismatch: %w", ctr.Name, err))
		}
	}

	return utilerrors.NewAggregate(errs)
}

func verifyContainerRestarts(pod *v1.Pod, expectedContainers []ResizableContainerInfo) error {
	ginkgo.GinkgoHelper()

	expectContainerRestarts := map[string]int32{}
	for _, ci := range expectedContainers {
		expectContainerRestarts[ci.Name] = ci.RestartCount
	}

	errs := []error{}
	for _, cs := range pod.Status.ContainerStatuses {
		expectedRestarts := expectContainerRestarts[cs.Name]
		if cs.RestartCount != expectedRestarts {
			errs = append(errs, fmt.Errorf("unexpected number of restarts for container %s: got %d, want %d", cs.Name, cs.RestartCount, expectedRestarts))
		}
	}
	return utilerrors.NewAggregate(errs)
}

func WaitForPodResizeActuation(ctx context.Context, f *framework.Framework, podClient *PodClient, pod *v1.Pod) *v1.Pod {
	ginkgo.GinkgoHelper()
	// Wait for resize to complete.
	framework.ExpectNoError(WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "resize status cleared", f.Timeouts.PodStart,
		func(pod *v1.Pod) (bool, error) {
			if pod.Status.Resize == v1.PodResizeStatusInfeasible {
				// This is a terminal resize state
				return false, fmt.Errorf("resize is infeasible")
			}
			return pod.Status.Resize == "", nil
		}), "pod should finish resizing")

	resizedPod, err := framework.GetObject(podClient.Get, pod.Name, metav1.GetOptions{})(ctx)
	framework.ExpectNoError(err, "failed to get resized pod")
	return resizedPod
}

func ExpectPodResized(ctx context.Context, f *framework.Framework, resizedPod *v1.Pod, expectedContainers []ResizableContainerInfo) {
	ginkgo.GinkgoHelper()

	// Put each error on a new line for readability.
	formatErrors := func(err error) error {
		var agg utilerrors.Aggregate
		if !errors.As(err, &agg) {
			return err
		}

		errStrings := make([]string, len(agg.Errors()))
		for i, err := range agg.Errors() {
			errStrings[i] = err.Error()
		}
		return fmt.Errorf("[\n%s\n]", strings.Join(errStrings, ",\n"))
	}
	// Verify Pod Containers Cgroup Values
	var errs []error
	if cgroupErrs := VerifyPodContainersCgroupValues(ctx, f, resizedPod, expectedContainers); cgroupErrs != nil {
		errs = append(errs, fmt.Errorf("container cgroup values don't match expected: %w", formatErrors(cgroupErrs)))
	}
	if resourceErrs := VerifyPodStatusResources(resizedPod, expectedContainers); resourceErrs != nil {
		errs = append(errs, fmt.Errorf("container status resources don't match expected: %w", formatErrors(resourceErrs)))
	}
	if restartErrs := verifyContainerRestarts(resizedPod, expectedContainers); restartErrs != nil {
		errs = append(errs, fmt.Errorf("container restart counts don't match expected: %w", formatErrors(restartErrs)))
	}

	if len(errs) > 0 {
		resizedPod.ManagedFields = nil // Suppress managed fields in error output.
		framework.ExpectNoError(formatErrors(utilerrors.NewAggregate(errs)),
			"Verifying pod resources resize state. Pod: %s", framework.PrettyPrintJSON(resizedPod))
	}
}

// ResizeContainerPatch generates a patch string to resize the pod container.
func ResizeContainerPatch(containers []ResizableContainerInfo) (string, error) {
	var patch patchSpec

	for _, container := range containers {
		var cPatch containerPatch
		cPatch.Name = container.Name
		cPatch.Resources.Requests.CPU = container.Resources.CPUReq
		cPatch.Resources.Requests.Memory = container.Resources.MemReq
		cPatch.Resources.Limits.CPU = container.Resources.CPULim
		cPatch.Resources.Limits.Memory = container.Resources.MemLim

		patch.Spec.Containers = append(patch.Spec.Containers, cPatch)
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return "", err
	}

	return string(patchBytes), nil
}
