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
	"fmt"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	kubecm "k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/test/e2e/framework"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

const (
	cgroupFsPath          string = "/sys/fs/cgroup"
	cgroupCPUSharesFile   string = "cpu/cpu.shares"
	cgroupCPUQuotaFile    string = "cpu/cpu.cfs_quota_us"
	cgroupMemLimitFile    string = "memory/memory.limit_in_bytes"
	cgroupv2CPUWeightFile string = "cpu.weight"
	cgroupv2CPULimitFile  string = "cpu.max"
	cgroupv2MemLimitFile  string = "memory.max"
	cgroupVolumeName      string = "sysfscgroup"
	cgroupMountPath       string = "/sysfscgroup"
)

var (
	podOnCgroupv2Node *bool
)

type ContainerResources struct {
	CPUReq              string
	CPULim              string
	MemReq              string
	MemLim              string
	EphStorReq          string
	EphStorLim          string
	ExtendedResourceReq string
	ExtendedResourceLim string
}

func (cr *ContainerResources) ResourceRequirements() *v1.ResourceRequirements {
	if cr == nil {
		return nil
	}

	var lim, req v1.ResourceList
	if cr.CPULim != "" || cr.MemLim != "" || cr.EphStorLim != "" {
		lim = make(v1.ResourceList)
	}
	if cr.CPUReq != "" || cr.MemReq != "" || cr.EphStorReq != "" {
		req = make(v1.ResourceList)
	}
	if cr.CPULim != "" {
		lim[v1.ResourceCPU] = resource.MustParse(cr.CPULim)
	}
	if cr.MemLim != "" {
		lim[v1.ResourceMemory] = resource.MustParse(cr.MemLim)
	}
	if cr.EphStorLim != "" {
		lim[v1.ResourceEphemeralStorage] = resource.MustParse(cr.EphStorLim)
	}
	if cr.CPUReq != "" {
		req[v1.ResourceCPU] = resource.MustParse(cr.CPUReq)
	}
	if cr.MemReq != "" {
		req[v1.ResourceMemory] = resource.MustParse(cr.MemReq)
	}
	if cr.EphStorReq != "" {
		req[v1.ResourceEphemeralStorage] = resource.MustParse(cr.EphStorReq)
	}
	return &v1.ResourceRequirements{Limits: lim, Requests: req}
}

func MakeContainerWithResources(name string, r *ContainerResources) v1.Container {
	var resources v1.ResourceRequirements
	if r != nil {
		resources = *r.ResourceRequirements()
	}
	return v1.Container{
		Name:      name,
		Resources: resources,
		Image:     imageutils.GetE2EImage(imageutils.BusyBox),
	}
}

func ConfigureHostPathForPodCgroup(pod *v1.Pod) {
	if pod.Spec.Volumes == nil {
		pod.Spec.Volumes = []v1.Volume{}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
		Name: cgroupVolumeName,
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{Path: cgroupFsPath},
		},
	})
	firstContainer := &pod.Spec.Containers[0]
	if firstContainer.VolumeMounts == nil {
		firstContainer.VolumeMounts = []v1.VolumeMount{}
	}
	firstContainer.VolumeMounts = append(firstContainer.VolumeMounts, v1.VolumeMount{
		Name:      cgroupVolumeName,
		MountPath: cgroupMountPath,
	})
}

func getPodCgroupPath(f *framework.Framework, pod *v1.Pod) (string, error) {
	// search path for both systemd driver and cgroupfs driver
	cmd := fmt.Sprintf("find %s -name '*%s*' -o -name '*%s*'", cgroupMountPath, strings.ReplaceAll(string(pod.UID), "-", "_"), string(pod.UID))
	framework.Logf("Namespace %s Pod %s - looking for Pod cgroup directory path: %q", f.Namespace, pod.Name, cmd)
	podCgPath, stderr, err := ExecCommandInContainerWithFullOutput(f, pod.Name, pod.Spec.Containers[0].Name, []string{"/bin/sh", "-c", cmd}...)
	if err != nil || len(stderr) > 0 {
		return "", fmt.Errorf("encountered error while running command: %q, \nerr: %w \nstdErr: %q", cmd, err, stderr)
	} else if podCgPath == "" {
		return "", fmt.Errorf("pod cgroup dirctory not found by command: %q", cmd)
	}
	return podCgPath, nil
}

func getCgroupMemLimitPath(cgPath string, podOnCgroupv2 bool) string {
	if podOnCgroupv2 {
		return fmt.Sprintf("%s/%s", cgPath, cgroupv2MemLimitFile)
	} else {
		return fmt.Sprintf("%s/%s", cgPath, cgroupMemLimitFile)
	}
}

func getCgroupCPULimitPath(cgPath string, podOnCgroupv2 bool) string {
	if podOnCgroupv2 {
		return fmt.Sprintf("%s/%s", cgPath, cgroupv2CPULimitFile)
	} else {
		return fmt.Sprintf("%s/%s", cgPath, cgroupCPUQuotaFile)
	}
}

func getCgroupCPURequestPath(cgPath string, podOnCgroupv2 bool) string {
	if podOnCgroupv2 {
		return fmt.Sprintf("%s/%s", cgPath, cgroupv2CPUWeightFile)
	} else {
		return fmt.Sprintf("%s/%s", cgPath, cgroupCPUSharesFile)
	}
}

func calculateExpectedCPUShares(rr *v1.ResourceRequirements, podOnCgroupv2 bool) int64 {
	cpuRequest := rr.Requests.Cpu()
	cpuLimit := rr.Limits.Cpu()
	var shares int64
	if cpuRequest.IsZero() && !cpuLimit.IsZero() {
		shares = int64(kubecm.MilliCPUToShares(cpuLimit.MilliValue()))
	} else {
		shares = int64(kubecm.MilliCPUToShares(cpuRequest.MilliValue()))
	}
	if podOnCgroupv2 {
		return 1 + ((shares-2)*9999)/262142

	} else {
		return shares
	}
}

func calculateExpectedCPULimitString(rr *v1.ResourceRequirements, podOnCgroupv2 bool) string {
	cpuLimit := rr.Limits.Cpu()
	cpuQuota := int64(-1)
	if !cpuLimit.IsZero() {
		cpuQuota = kubecm.MilliCPUToQuota(cpuLimit.MilliValue(), kubecm.QuotaPeriod)
	}
	expectedCPULimitString := strconv.FormatInt(cpuQuota, 10)
	if podOnCgroupv2 {
		if expectedCPULimitString == "-1" {
			expectedCPULimitString = "max"
		}
		expectedCPULimitString = fmt.Sprintf("%s %d", expectedCPULimitString, kubecm.QuotaPeriod)
	}
	return expectedCPULimitString
}

func calculateExpectedMemLimitString(rr *v1.ResourceRequirements, podOnCgroupv2 bool) string {
	expectedMemLimitInBytes := rr.Limits.Memory().Value()
	expectedMemLimitString := strconv.FormatInt(expectedMemLimitInBytes, 10)
	if podOnCgroupv2 && expectedMemLimitString == "0" {
		expectedMemLimitString = "max"
	}
	return expectedMemLimitString
}

func verifyCPUWeight(f *framework.Framework, pod *v1.Pod, containerName, cgPath string, expectedResources *v1.ResourceRequirements, podOnCgroupv2 bool) error {
	cpuWeightCgPath := getCgroupCPURequestPath(cgPath, podOnCgroupv2)
	expectedCPUShares := calculateExpectedCPUShares(expectedResources, podOnCgroupv2)
	return VerifyCgroupValue(f, pod, containerName, cpuWeightCgPath, strconv.FormatInt(expectedCPUShares, 10))
}

func verifyCPULimit(f *framework.Framework, pod *v1.Pod, containerName string, cgPath string, expectedResources *v1.ResourceRequirements, podOnCgroupv2 bool) error {
	cpuLimCgPath := getCgroupCPULimitPath(cgPath, podOnCgroupv2)
	expectedCPULimit := calculateExpectedCPULimitString(expectedResources, podOnCgroupv2)
	return VerifyCgroupValue(f, pod, containerName, cpuLimCgPath, expectedCPULimit)
}

func verifyMemoryLimit(f *framework.Framework, pod *v1.Pod, containerName, cgPath string, expectedResources *v1.ResourceRequirements, podOnCgroupv2 bool) error {
	memLimCgPath := getCgroupMemLimitPath(cgPath, podOnCgroupv2)
	expectedMemLim := calculateExpectedMemLimitString(expectedResources, podOnCgroupv2)
	if expectedMemLim == "0" {
		return nil
	}
	return VerifyCgroupValue(f, pod, containerName, memLimCgPath, expectedMemLim)
}

func verifyContainerCPUWeight(f *framework.Framework, pod *v1.Pod, containerName string, expectedResources *v1.ResourceRequirements, podOnCgroupv2 bool) error {
	return verifyCPUWeight(f, pod, containerName, cgroupFsPath, expectedResources, podOnCgroupv2)
}

func VerifyContainerCPULimit(f *framework.Framework, pod *v1.Pod, containerName string, expectedResources *v1.ResourceRequirements, podOnCgroupv2 bool) error {
	return verifyCPULimit(f, pod, containerName, cgroupFsPath, expectedResources, podOnCgroupv2)
}

func VerifyContainerMemoryLimit(f *framework.Framework, pod *v1.Pod, containerName string, expectedResources *v1.ResourceRequirements, podOnCgroupv2 bool) error {
	return verifyMemoryLimit(f, pod, containerName, cgroupFsPath, expectedResources, podOnCgroupv2)
}

func verifyContainerCgroupValues(f *framework.Framework, pod *v1.Pod, tc *v1.Container, podOnCgroupv2 bool) error {
	if err := VerifyContainerMemoryLimit(f, pod, tc.Name, &tc.Resources, podOnCgroupv2); err != nil {
		return err
	}
	if err := VerifyContainerCPULimit(f, pod, tc.Name, &tc.Resources, podOnCgroupv2); err != nil {
		return err
	}
	if err := verifyContainerCPUWeight(f, pod, tc.Name, &tc.Resources, podOnCgroupv2); err != nil {
		return err
	}
	return nil
}

func verifyPodCPUWeight(f *framework.Framework, pod *v1.Pod, podCgPath string, expectedResources *v1.ResourceRequirements) error {
	return verifyCPUWeight(f, pod, pod.Spec.Containers[0].Name, podCgPath, expectedResources, true)
}

func verifyPodCPULimit(f *framework.Framework, pod *v1.Pod, podCgPath string, expectedResources *v1.ResourceRequirements) error {
	return verifyCPULimit(f, pod, pod.Spec.Containers[0].Name, podCgPath, expectedResources, true)
}

func verifyPodMemoryLimit(f *framework.Framework, pod *v1.Pod, podCgPath string, expectedResources *v1.ResourceRequirements) error {
	return verifyMemoryLimit(f, pod, pod.Spec.Containers[0].Name, podCgPath, expectedResources, true)
}

// VerifyPodCgroups verifies pod cgroup is configured on a node as expected.
func VerifyPodCgroups(ctx context.Context, f *framework.Framework, pod *v1.Pod, info *ContainerResources) error {
	ginkgo.GinkgoHelper()

	// Extract pod cgroup directory path
	podCgPath, err := getPodCgroupPath(f, pod)
	if err != nil {
		return err
	}

	// Verify cgroup values
	expectedResources := info.ResourceRequirements()
	var errs []error
	errs = append(errs, verifyPodCPUWeight(f, pod, podCgPath, expectedResources))
	errs = append(errs, verifyPodCPULimit(f, pod, podCgPath, expectedResources))
	errs = append(errs, verifyPodMemoryLimit(f, pod, podCgPath, expectedResources))

	return utilerrors.NewAggregate(errs)
}

func buildPodResourceInfo(podCPURequestMilliValue, podCPULimitMilliValue, podMemoryLimitInBytes int64) ContainerResources {
	podResourceInfo := ContainerResources{}
	if podCPURequestMilliValue > 0 {
		podResourceInfo.CPUReq = fmt.Sprintf("%dm", podCPURequestMilliValue)
	}
	if podCPULimitMilliValue > 0 {
		podResourceInfo.CPULim = fmt.Sprintf("%dm", podCPULimitMilliValue)
	}
	if podMemoryLimitInBytes > 0 {
		podResourceInfo.MemLim = fmt.Sprintf("%d", podMemoryLimitInBytes)
	}
	return podResourceInfo
}

func VerifyPodContainersCgroupValues(ctx context.Context, f *framework.Framework, pod *v1.Pod, tcInfo []ResizableContainerInfo) error {
	ginkgo.GinkgoHelper()
	if podOnCgroupv2Node == nil {
		value := IsPodOnCgroupv2Node(f, pod)
		podOnCgroupv2Node = &value
	}

	var podCPURequestMilliValue, podCPULimitMilliValue, podMemoryLimitInBytes int64
	var errs []error
	for _, ci := range tcInfo {
		tc := makeResizableContainer(ci)
		errs = append(errs, verifyContainerCgroupValues(f, pod, &tc, *podOnCgroupv2Node))

		// Accumulate container resources for verifying pod
		podCPURequestMilliValue += tc.Resources.Requests.Cpu().MilliValue()
		if podCPULimitMilliValue >= 0 {
			if tc.Resources.Limits.Cpu().IsZero() {
				podCPULimitMilliValue = -1
			} else {
				podCPULimitMilliValue += tc.Resources.Limits.Cpu().MilliValue()
			}
		}
		if podMemoryLimitInBytes >= 0 {
			if tc.Resources.Limits.Memory().IsZero() {
				podMemoryLimitInBytes = -1
			} else {
				podMemoryLimitInBytes += tc.Resources.Limits.Memory().Value()
			}
		}
	}

	if !*podOnCgroupv2Node {
		// cgroup v1 is in maintenance mode. Skip verifying pod cgroup
		return utilerrors.NewAggregate(errs)
	}

	podResourceInfo := buildPodResourceInfo(podCPURequestMilliValue, podCPULimitMilliValue, podMemoryLimitInBytes)
	errs = append(errs, VerifyPodCgroups(ctx, f, pod, &podResourceInfo))

	return utilerrors.NewAggregate(errs)
}
