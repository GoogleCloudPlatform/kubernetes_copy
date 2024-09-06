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

package node

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	kubecm "k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

var (
	cmd = []string{"/bin/sh", "-c", "sleep 1d"}
)

var _ = SIGDescribe("Pod Level Resources", framework.WithSerial(), feature.PodLevelResources, func() {
	f := framework.NewDefaultFramework("pod-level-resources-tests")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.BeforeEach(func(ctx context.Context) {
		_, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)

		if framework.NodeOSDistroIs("windows") {
			e2eskipper.Skipf("not supported on windows -- skipping")
		}

		// skip the test on nodes with cgroupv2 not enabled.
		if !isCgroupv2Node(f, ctx) {
			e2eskipper.Skipf("not supported on cgroupv1 -- skipping")
		}
	})
	podLevelResourcesTests(f)
})

// isCgroupv2Node creates a small pod and check if it is running on a node
// with cgroupv2 enabled.
// TODO: refactor to mark this test with cgroupv2 label, and rather check
// the label in the test job, to tun this test on a node with cgroupv2.
func isCgroupv2Node(f *framework.Framework, ctx context.Context) bool {
	podClient := e2epod.NewPodClient(f)
	containerResources := e2epod.ContainerResources{CPULim: "1m", MemReq: "1Mi"}
	cgroupv2Testpod := &v1.Pod{
		ObjectMeta: makeObjectMetadata("cgroupv2-check", f.Namespace.Name),
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:      "cgroupv2-check",
					Image:     imageutils.GetE2EImage(imageutils.BusyBox),
					Command:   cmd,
					Resources: *containerResources.ResourceRequirements(),
				},
			},
		},
	}

	pod := podClient.CreateSync(ctx, cgroupv2Testpod)
	defer func() {
		framework.Logf("Deleting %q pod", cgroupv2Testpod.Name)
		delErr := e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
		framework.ExpectNoError(delErr, "failed to delete pod %s", delErr)
	}()

	return e2epod.IsPodOnCgroupv2Node(f, pod)
}

func makeObjectMetadata(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: "testpod", Namespace: namespace,
		Labels: map[string]string{"time": strconv.Itoa(time.Now().Nanosecond())},
	}
}

type containerInfo struct {
	Name      string
	Resources *e2epod.ContainerResources
}

func makeContainer(info containerInfo) v1.Container {
	cmd := []string{"/bin/sh", "-c", "sleep 1d"}
	var resources v1.ResourceRequirements
	if info.Resources != nil {
		resources = *info.Resources.ResourceRequirements()
	}
	return v1.Container{
		Name:      info.Name,
		Command:   cmd,
		Resources: resources,
		Image:     imageutils.GetE2EImage(imageutils.BusyBox),
		VolumeMounts: []v1.VolumeMount{
			e2epod.CreateVolumeMountForCgroup(),
		},
	}
}

func makePod(metadata *metav1.ObjectMeta, podResources *e2epod.ContainerResources, containers []containerInfo) *v1.Pod {
	var testContainers []v1.Container
	for _, container := range containers {
		testContainers = append(testContainers, makeContainer(container))
	}

	pod := &v1.Pod{
		ObjectMeta: *metadata,

		Spec: v1.PodSpec{
			Containers: testContainers,
			Volumes: []v1.Volume{
				e2epod.CreateHostPathVolumeForCgroup(),
			},
		},
	}

	if podResources != nil {
		res := podResources.ResourceRequirements()
		pod.Spec.Resources = res
	}

	return pod
}

func verifyPodResources(gotPod v1.Pod, inputInfo, expectedInfo *e2epod.ContainerResources) {
	ginkgo.GinkgoHelper()
	var expectedResources *v1.ResourceRequirements
	// expectedResources will be nil if pod-level resources are not set in the test
	// case input.
	if inputInfo != nil {
		expectedResources = expectedInfo.ResourceRequirements()
	}
	gomega.Expect(expectedResources).To(gomega.Equal(gotPod.Spec.Resources))
}

func verifyQoS(gotPod v1.Pod, expectedQoS v1.PodQOSClass) {
	ginkgo.GinkgoHelper()
	gomega.Expect(expectedQoS).To(gomega.Equal(gotPod.Status.QOSClass))
}

func podLevelResourcesTests(f *framework.Framework) {
	type expectedPodConfig struct {
		qos v1.PodQOSClass
		// totalPodResources represents the aggregate resource requests
		// and limits for the pod. If pod-level resource specifications
		// are specified, totalPodResources is equal to pod-level resources.
		// Otherwise, it is calculated by aggregating resource requests and
		// limits from all containers within the pod..
		totalPodResources *e2epod.ContainerResources
	}

	type testCase struct {
		name         string
		podResources *e2epod.ContainerResources
		containers   []containerInfo
		expected     expectedPodConfig
	}

	tests := []testCase{
		{
			name: "Guaranteed QoS pod with container resources",
			containers: []containerInfo{
				{Name: "c1", Resources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "50m", MemReq: "70Mi", MemLim: "70Mi"}},
				{Name: "c2", Resources: &e2epod.ContainerResources{CPUReq: "70m", CPULim: "70m", MemReq: "50Mi", MemLim: "50Mi"}},
			},
			expected: expectedPodConfig{
				qos:               v1.PodQOSGuaranteed,
				totalPodResources: &e2epod.ContainerResources{CPUReq: "120m", CPULim: "120m", MemReq: "120Mi", MemLim: "120Mi"},
			},
		},
		{
			name:         "Guaranteed QoS pod, no container resources",
			podResources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
			containers:   []containerInfo{{Name: "c1"}, {Name: "c2"}},
			expected: expectedPodConfig{
				qos:               v1.PodQOSGuaranteed,
				totalPodResources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
			},
		},
		{
			name:         "Guaranteed QoS pod with container resources",
			podResources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
			containers: []containerInfo{
				{Name: "c1", Resources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"}},
				{Name: "c2", Resources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"}},
			},
			expected: expectedPodConfig{
				qos:               v1.PodQOSGuaranteed,
				totalPodResources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
			},
		},
		{
			name:         "Guaranteed QoS pod, 1 container with resources",
			podResources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
			containers: []containerInfo{
				{Name: "c1", Resources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"}},
				{Name: "c2"},
			},
			expected: expectedPodConfig{
				qos:               v1.PodQOSGuaranteed,
				totalPodResources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
			},
		},
		{
			name:         "Burstable QoS pod, no container resources",
			podResources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"},
			containers: []containerInfo{
				{Name: "c1"},
				{Name: "c2"},
			},
			expected: expectedPodConfig{
				qos:               v1.PodQOSBurstable,
				totalPodResources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"},
			},
		},
		{
			name:         "Burstable QoS pod with container resources",
			podResources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"},
			containers: []containerInfo{
				{Name: "c1", Resources: &e2epod.ContainerResources{CPUReq: "20m", CPULim: "100m", MemReq: "20Mi", MemLim: "100Mi"}},
				{Name: "c2", Resources: &e2epod.ContainerResources{CPUReq: "30m", CPULim: "100m", MemReq: "30Mi", MemLim: "100Mi"}},
			},
			expected: expectedPodConfig{
				qos:               v1.PodQOSBurstable,
				totalPodResources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"},
			},
		},
		{
			name:         "Burstable QoS pod, 1 container with resources",
			podResources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"},
			containers: []containerInfo{
				{Name: "c1", Resources: &e2epod.ContainerResources{CPUReq: "20m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"}},
				{Name: "c2"},
			},
			expected: expectedPodConfig{
				qos:               v1.PodQOSBurstable,
				totalPodResources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "50Mi", MemLim: "100Mi"},
			},
		},
	}

	for _, tc := range tests {
		ginkgo.It(tc.name, func(ctx context.Context) {
			podMetadata := makeObjectMetadata("testpod", f.Namespace.Name)
			testPod := makePod(&podMetadata, tc.podResources, tc.containers)

			ginkgo.By("creating pods")
			podClient := e2epod.NewPodClient(f)
			pod := podClient.CreateSync(ctx, testPod)

			ginkgo.By("verifying pod resources are as expected")
			verifyPodResources(*pod, tc.podResources, tc.expected.totalPodResources)

			ginkgo.By("verifying pod QoS as expected")
			verifyQoS(*pod, tc.expected.qos)

			ginkgo.By("verifying pod cgroup values")
			err := e2epod.VerifyPodCgroups(ctx, f, pod, tc.expected.totalPodResources)
			framework.ExpectNoError(err, "failed to verify pod's cgroup values: %v", err)

			ginkgo.By("verifying containers cgroup limits are same as pod container's cgroup limits")
			err = verifyContainersCgroupLimits(f, pod)
			framework.ExpectNoError(err, "failed to verify containers cgroup values: %v", err)

			ginkgo.By("deleting pods")
			delErr := e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
			framework.ExpectNoError(delErr, "failed to delete pod %s", delErr)
		})
	}
}

func verifyContainersCgroupLimits(f *framework.Framework, pod *v1.Pod) error {
	var errs []error
	for _, container := range pod.Spec.Containers {
		if pod.Spec.Resources != nil && pod.Spec.Resources.Limits.Memory() != nil &&
			container.Resources.Limits.Memory() == nil {
			expectedCgroupMemLimit := strconv.FormatInt(pod.Spec.Resources.Limits.Memory().Value(), 10)
			err := e2epod.VerifyCgroupValue(f, pod, container.Name, e2epod.Cgroupv2MemLimit, expectedCgroupMemLimit)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to verify memory limit cgroup value: %w", err))
			}
		}

		if pod.Spec.Resources != nil && pod.Spec.Resources.Limits.Cpu() != nil &&
			container.Resources.Limits.Cpu() == nil {
			cpuQuota := kubecm.MilliCPUToQuota(pod.Spec.Resources.Limits.Cpu().MilliValue(), kubecm.QuotaPeriod)
			expectedCPULimit := strconv.FormatInt(cpuQuota, 10)
			expectedCPULimit = fmt.Sprintf("%s %s", expectedCPULimit, e2epod.CPUPeriod)
			err := e2epod.VerifyCgroupValue(f, pod, container.Name, e2epod.Cgroupv2CPULimit, expectedCPULimit)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to verify cpu limit cgroup value: %w", err))
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}
