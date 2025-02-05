/*
Copyright 2021 The Kubernetes Authors.

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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	resourceapi "k8s.io/kubernetes/pkg/api/v1/resource"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func doPodResizeAdmissionPluginsTests() {
	testcases := []struct {
		name                  string
		enableAdmissionPlugin func(ctx context.Context, f *framework.Framework)
		wantMemoryError       string
		wantCPUError          string
	}{
		{
			name: "pod-resize-resource-quota-test",
			enableAdmissionPlugin: func(ctx context.Context, f *framework.Framework) {
				resourceQuota := v1.ResourceQuota{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "resize-resource-quota",
						Namespace: f.Namespace.Name,
					},
					Spec: v1.ResourceQuotaSpec{
						Hard: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("800m"),
							v1.ResourceMemory: resource.MustParse("800Mi"),
						},
					},
				}

				ginkgo.By("Creating a ResourceQuota")
				_, rqErr := f.ClientSet.CoreV1().ResourceQuotas(f.Namespace.Name).Create(ctx, &resourceQuota, metav1.CreateOptions{})
				framework.ExpectNoError(rqErr, "failed to create resource quota")
			},
			wantMemoryError: "exceeded quota: resize-resource-quota, requested: memory=350Mi, used: memory=700Mi, limited: memory=800Mi",
			wantCPUError:    "exceeded quota: resize-resource-quota, requested: cpu=200m, used: cpu=700m, limited: cpu=800m",
		},
		{
			name: "pod-resize-limit-ranger-test",
			enableAdmissionPlugin: func(ctx context.Context, f *framework.Framework) {
				lr := v1.LimitRange{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "resize-limit-ranger",
						Namespace: f.Namespace.Name,
					},
					Spec: v1.LimitRangeSpec{
						Limits: []v1.LimitRangeItem{
							{
								Type: v1.LimitTypeContainer,
								Max: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("500m"),
									v1.ResourceMemory: resource.MustParse("500Mi"),
								},
								Min: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("50m"),
									v1.ResourceMemory: resource.MustParse("50Mi"),
								},
								Default: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("100m"),
									v1.ResourceMemory: resource.MustParse("100Mi"),
								},
								DefaultRequest: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("50m"),
									v1.ResourceMemory: resource.MustParse("50Mi"),
								},
							},
						},
					},
				}

				ginkgo.By("Creating a LimitRanger")
				_, lrErr := f.ClientSet.CoreV1().LimitRanges(f.Namespace.Name).Create(ctx, &lr, metav1.CreateOptions{})
				framework.ExpectNoError(lrErr, "failed to create limit ranger")
			},
			wantMemoryError: "forbidden: maximum memory usage per Container is 500Mi, but limit is 750Mi",
			wantCPUError:    "forbidden: maximum cpu usage per Container is 500m, but limit is 600m",
		},
	}

	for _, tc := range testcases {
		f := framework.NewDefaultFramework(tc.name)

		ginkgo.It(tc.name, func(ctx context.Context) {
			containers := []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "300m", MemReq: "300Mi", MemLim: "300Mi"},
				},
			}
			patchString := `{"spec":{"containers":[
				{"name":"c1", "resources":{"requests":{"cpu":"400m","memory":"400Mi"},"limits":{"cpu":"400m","memory":"400Mi"}}}
			]}}`
			expected := []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "400m", CPULim: "400m", MemReq: "400Mi", MemLim: "400Mi"},
				},
			}
			patchStringExceedCPU := `{"spec":{"containers":[
				{"name":"c1", "resources":{"requests":{"cpu":"600m","memory":"200Mi"},"limits":{"cpu":"600m","memory":"200Mi"}}}
			]}}`
			patchStringExceedMemory := `{"spec":{"containers":[
				{"name":"c1", "resources":{"requests":{"cpu":"250m","memory":"750Mi"},"limits":{"cpu":"250m","memory":"750Mi"}}}
			]}}`

			tc.enableAdmissionPlugin(ctx, f)

			tStamp := strconv.Itoa(time.Now().Nanosecond())
			testPod1 := e2epod.MakePodWithResizableContainers(f.Namespace.Name, "testpod1", tStamp, containers)
			testPod1 = e2epod.MustMixinRestrictedPodSecurity(testPod1)
			testPod2 := e2epod.MakePodWithResizableContainers(f.Namespace.Name, "testpod2", tStamp, containers)
			testPod2 = e2epod.MustMixinRestrictedPodSecurity(testPod2)

			ginkgo.By("creating pods")
			podClient := e2epod.NewPodClient(f)
			newPods := podClient.CreateBatch(ctx, []*v1.Pod{testPod1, testPod2})

			ginkgo.By("verifying initial pod resources, and policy are as expected")
			e2epod.VerifyPodResources(newPods[0], containers)

			ginkgo.By("patching pod for resize within resource quota")
			patchedPod, pErr := f.ClientSet.CoreV1().Pods(newPods[0].Namespace).Patch(ctx, newPods[0].Name,
				types.StrategicMergePatchType, []byte(patchString), metav1.PatchOptions{}, "resize")
			framework.ExpectNoError(pErr, "failed to patch pod for resize")

			ginkgo.By("verifying pod patched for resize within resource quota")
			e2epod.VerifyPodResources(patchedPod, expected)

			ginkgo.By("waiting for resize to be actuated")
			resizedPod := e2epod.WaitForPodResizeActuation(ctx, f, podClient, newPods[0])
			e2epod.ExpectPodResized(ctx, f, resizedPod, expected)

			ginkgo.By("verifying pod resources after resize")
			e2epod.VerifyPodResources(resizedPod, expected)

			ginkgo.By("patching pod for resize with memory exceeding resource quota")
			_, pErrExceedMemory := f.ClientSet.CoreV1().Pods(resizedPod.Namespace).Patch(ctx,
				resizedPod.Name, types.StrategicMergePatchType, []byte(patchStringExceedMemory), metav1.PatchOptions{}, "resize")
			gomega.Expect(pErrExceedMemory).To(gomega.HaveOccurred())
			gomega.Expect(pErrExceedMemory).To(gomega.MatchError(gomega.ContainSubstring(tc.wantMemoryError)))

			ginkgo.By("verifying pod patched for resize exceeding memory resource quota remains unchanged")
			patchedPodExceedMemory, pErrEx2 := podClient.Get(ctx, resizedPod.Name, metav1.GetOptions{})
			framework.ExpectNoError(pErrEx2, "failed to get pod post exceed memory resize")
			e2epod.VerifyPodResources(patchedPodExceedMemory, expected)
			framework.ExpectNoError(e2epod.VerifyPodStatusResources(patchedPodExceedMemory, expected))

			ginkgo.By(fmt.Sprintf("patching pod %s for resize with CPU exceeding resource quota", resizedPod.Name))
			_, pErrExceedCPU := f.ClientSet.CoreV1().Pods(resizedPod.Namespace).Patch(ctx,
				resizedPod.Name, types.StrategicMergePatchType, []byte(patchStringExceedCPU), metav1.PatchOptions{}, "resize")
			gomega.Expect(pErrExceedCPU).To(gomega.HaveOccurred())
			gomega.Expect(pErrExceedCPU).To(gomega.MatchError(gomega.ContainSubstring(tc.wantCPUError)))

			ginkgo.By("verifying pod patched for resize exceeding CPU resource quota remains unchanged")
			patchedPodExceedCPU, pErrEx1 := podClient.Get(ctx, resizedPod.Name, metav1.GetOptions{})
			framework.ExpectNoError(pErrEx1, "failed to get pod post exceed CPU resize")
			e2epod.VerifyPodResources(patchedPodExceedCPU, expected)
			framework.ExpectNoError(e2epod.VerifyPodStatusResources(patchedPodExceedMemory, expected))
		})
	}

}

func doPodResizeSchedulerTests(f *framework.Framework) {
	ginkgo.It("pod-resize-scheduler-tests", func(ctx context.Context) {
		podClient := e2epod.NewPodClient(f)
		nodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		framework.ExpectNoError(err, "failed to get running nodes")
		gomega.Expect(nodes.Items).ShouldNot(gomega.BeEmpty())
		framework.Logf("Found %d schedulable nodes", len(nodes.Items))

		//
		// Calculate available CPU. nodeAvailableCPU = nodeAllocatableCPU - sum(podAllocatedCPU)
		//
		getNodeAllocatableAndAvailableMilliCPUValues := func(n *v1.Node) (int64, int64) {
			nodeAllocatableMilliCPU := n.Status.Allocatable.Cpu().MilliValue()
			gomega.Expect(n.Status.Allocatable).ShouldNot(gomega.BeNil(), "allocatable")
			podAllocatedMilliCPU := int64(0)

			// Exclude pods that are in the Succeeded or Failed states
			selector := fmt.Sprintf("spec.nodeName=%s,status.phase!=%v,status.phase!=%v", n.Name, v1.PodSucceeded, v1.PodFailed)
			listOptions := metav1.ListOptions{FieldSelector: selector}
			podList, err := f.ClientSet.CoreV1().Pods(metav1.NamespaceAll).List(ctx, listOptions)

			framework.ExpectNoError(err, "failed to get running pods")
			framework.Logf("Found %d pods on node '%s'", len(podList.Items), n.Name)
			for _, pod := range podList.Items {
				podRequestMilliCPU := resourceapi.GetResourceRequest(&pod, v1.ResourceCPU)
				podAllocatedMilliCPU += podRequestMilliCPU
			}
			nodeAvailableMilliCPU := nodeAllocatableMilliCPU - podAllocatedMilliCPU
			return nodeAllocatableMilliCPU, nodeAvailableMilliCPU
		}

		ginkgo.By("Find node CPU resources available for allocation!")
		node := nodes.Items[0]
		nodeAllocatableMilliCPU, nodeAvailableMilliCPU := getNodeAllocatableAndAvailableMilliCPUValues(&node)
		framework.Logf("Node '%s': NodeAllocatable MilliCPUs = %dm. MilliCPUs currently available to allocate = %dm.",
			node.Name, nodeAllocatableMilliCPU, nodeAvailableMilliCPU)

		//
		// Scheduler focussed pod resize E2E test case #1:
		//     1. Create pod1 and pod2 on node such that pod1 has enough CPU to be scheduled, but pod2 does not.
		//     2. Resize pod2 down so that it fits on the node and can be scheduled.
		//     3. Verify that pod2 gets scheduled and comes up and running.
		//
		testPod1CPUQuantity := resource.NewMilliQuantity(nodeAvailableMilliCPU/2, resource.DecimalSI)
		testPod2CPUQuantity := resource.NewMilliQuantity(nodeAvailableMilliCPU, resource.DecimalSI)
		testPod2CPUQuantityResized := resource.NewMilliQuantity(testPod1CPUQuantity.MilliValue()/2, resource.DecimalSI)
		framework.Logf("TEST1: testPod1 initial CPU request is '%dm'", testPod1CPUQuantity.MilliValue())
		framework.Logf("TEST1: testPod2 initial CPU request is '%dm'", testPod2CPUQuantity.MilliValue())
		framework.Logf("TEST1: testPod2 resized CPU request is '%dm'", testPod2CPUQuantityResized.MilliValue())

		c1 := []e2epod.ResizableContainerInfo{
			{
				Name:      "c1",
				Resources: &e2epod.ContainerResources{CPUReq: testPod1CPUQuantity.String(), CPULim: testPod1CPUQuantity.String()},
			},
		}
		c2 := []e2epod.ResizableContainerInfo{
			{
				Name:      "c2",
				Resources: &e2epod.ContainerResources{CPUReq: testPod2CPUQuantity.String(), CPULim: testPod2CPUQuantity.String()},
			},
		}
		patchTestpod2ToFitNode := fmt.Sprintf(`{
				"spec": {
					"containers": [
						{
							"name":      "c2",
							"resources": {"requests": {"cpu": "%dm"}, "limits": {"cpu": "%dm"}}
						}
					]
				}
			}`, testPod2CPUQuantityResized.MilliValue(), testPod2CPUQuantityResized.MilliValue())

		tStamp := strconv.Itoa(time.Now().Nanosecond())
		testPod1 := e2epod.MakePodWithResizableContainers(f.Namespace.Name, "testpod1", tStamp, c1)
		testPod1 = e2epod.MustMixinRestrictedPodSecurity(testPod1)
		testPod2 := e2epod.MakePodWithResizableContainers(f.Namespace.Name, "testpod2", tStamp, c2)
		testPod2 = e2epod.MustMixinRestrictedPodSecurity(testPod2)
		e2epod.SetNodeAffinity(&testPod1.Spec, node.Name)
		e2epod.SetNodeAffinity(&testPod2.Spec, node.Name)

		ginkgo.By(fmt.Sprintf("TEST1: Create pod '%s' that fits the node '%s'", testPod1.Name, node.Name))
		testPod1 = podClient.CreateSync(ctx, testPod1)
		gomega.Expect(testPod1.Status.Phase).To(gomega.Equal(v1.PodRunning))

		ginkgo.By(fmt.Sprintf("TEST1: Create pod '%s' that won't fit node '%s' with pod '%s' on it", testPod2.Name, node.Name, testPod1.Name))
		testPod2 = podClient.Create(ctx, testPod2)
		err = e2epod.WaitForPodNameUnschedulableInNamespace(ctx, f.ClientSet, testPod2.Name, testPod2.Namespace)
		framework.ExpectNoError(err)
		gomega.Expect(testPod2.Status.Phase).To(gomega.Equal(v1.PodPending))

		ginkgo.By(fmt.Sprintf("TEST1: Resize pod '%s' to fit in node '%s'", testPod2.Name, node.Name))
		testPod2, pErr := f.ClientSet.CoreV1().Pods(testPod2.Namespace).Patch(ctx,
			testPod2.Name, types.StrategicMergePatchType, []byte(patchTestpod2ToFitNode), metav1.PatchOptions{}, "resize")
		framework.ExpectNoError(pErr, "failed to patch pod for resize")

		ginkgo.By(fmt.Sprintf("TEST1: Verify that pod '%s' is running after resize", testPod2.Name))
		framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, testPod2))

		// Scheduler focussed pod resize E2E test case #2
		//     1. With pod1 + pod2 running on node above, create pod3 that requests more CPU than available, verify pending.
		//     2. Resize pod1 down so that pod3 gets room to be scheduled.
		//     3. Verify that pod3 is scheduled and running.
		//
		nodeAllocatableMilliCPU2, nodeAvailableMilliCPU2 := getNodeAllocatableAndAvailableMilliCPUValues(&node)
		framework.Logf("TEST2: Node '%s': NodeAllocatable MilliCPUs = %dm. MilliCPUs currently available to allocate = %dm.",
			node.Name, nodeAllocatableMilliCPU2, nodeAvailableMilliCPU2)
		testPod3CPUQuantity := resource.NewMilliQuantity(nodeAvailableMilliCPU2+testPod1CPUQuantity.MilliValue()/4, resource.DecimalSI)
		testPod1CPUQuantityResized := resource.NewMilliQuantity(testPod1CPUQuantity.MilliValue()/3, resource.DecimalSI)
		framework.Logf("TEST2: testPod1 MilliCPUs after resize '%dm'", testPod1CPUQuantityResized.MilliValue())

		c3 := []e2epod.ResizableContainerInfo{
			{
				Name:      "c3",
				Resources: &e2epod.ContainerResources{CPUReq: testPod3CPUQuantity.String(), CPULim: testPod3CPUQuantity.String()},
			},
		}
		patchTestpod1ToMakeSpaceForPod3 := fmt.Sprintf(`{
				"spec": {
					"containers": [
						{
							"name":      "c1",
							"resources": {"requests": {"cpu": "%dm"},"limits": {"cpu": "%dm"}}
						}
					]
				}
			}`, testPod1CPUQuantityResized.MilliValue(), testPod1CPUQuantityResized.MilliValue())

		tStamp = strconv.Itoa(time.Now().Nanosecond())
		testPod3 := e2epod.MakePodWithResizableContainers(f.Namespace.Name, "testpod3", tStamp, c3)
		testPod3 = e2epod.MustMixinRestrictedPodSecurity(testPod3)
		e2epod.SetNodeAffinity(&testPod3.Spec, node.Name)

		ginkgo.By(fmt.Sprintf("TEST2: Create testPod3 '%s' that cannot fit node '%s' due to insufficient CPU.", testPod3.Name, node.Name))
		testPod3 = podClient.Create(ctx, testPod3)
		p3Err := e2epod.WaitForPodNameUnschedulableInNamespace(ctx, f.ClientSet, testPod3.Name, testPod3.Namespace)
		framework.ExpectNoError(p3Err, "failed to create pod3 or pod3 did not become pending!")
		gomega.Expect(testPod3.Status.Phase).To(gomega.Equal(v1.PodPending))

		ginkgo.By(fmt.Sprintf("TEST2: Resize pod '%s' to make enough space for pod '%s'", testPod1.Name, testPod3.Name))
		testPod1, p1Err := f.ClientSet.CoreV1().Pods(testPod1.Namespace).Patch(ctx,
			testPod1.Name, types.StrategicMergePatchType, []byte(patchTestpod1ToMakeSpaceForPod3), metav1.PatchOptions{}, "resize")
		framework.ExpectNoError(p1Err, "failed to patch pod for resize")

		ginkgo.By(fmt.Sprintf("TEST2: Verify pod '%s' is running after successfully resizing pod '%s'", testPod3.Name, testPod1.Name))
		framework.Logf("TEST2: Pod '%s' CPU requests '%dm'", testPod1.Name, testPod1.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
		framework.Logf("TEST2: Pod '%s' CPU requests '%dm'", testPod2.Name, testPod2.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
		framework.Logf("TEST2: Pod '%s' CPU requests '%dm'", testPod3.Name, testPod3.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
		framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, testPod3))

		ginkgo.By("deleting pods")
		delErr1 := e2epod.DeletePodWithWait(ctx, f.ClientSet, testPod1)
		framework.ExpectNoError(delErr1, "failed to delete pod %s", testPod1.Name)
		delErr2 := e2epod.DeletePodWithWait(ctx, f.ClientSet, testPod2)
		framework.ExpectNoError(delErr2, "failed to delete pod %s", testPod2.Name)
		delErr3 := e2epod.DeletePodWithWait(ctx, f.ClientSet, testPod3)
		framework.ExpectNoError(delErr3, "failed to delete pod %s", testPod3.Name)
	})
}

var _ = SIGDescribe(framework.WithSerial(), "Pod InPlace Resize Container (scheduler-focused)", feature.InPlacePodVerticalScaling, func() {
	f := framework.NewDefaultFramework("pod-resize-scheduler-tests")
	ginkgo.BeforeEach(func(ctx context.Context) {
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)
		if framework.NodeOSDistroIs("windows") || e2enode.IsARM64(node) {
			e2eskipper.Skipf("runtime does not support InPlacePodVerticalScaling -- skipping")
		}
	})
	doPodResizeSchedulerTests(f)
})

var _ = SIGDescribe("Pod InPlace Resize Container", feature.InPlacePodVerticalScaling, func() {
	f := framework.NewDefaultFramework("pod-resize-tests")

	ginkgo.BeforeEach(func(ctx context.Context) {
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)
		if framework.NodeOSDistroIs("windows") || e2enode.IsARM64(node) {
			e2eskipper.Skipf("runtime does not support InPlacePodVerticalScaling -- skipping")
		}
	})
	doPodResizeAdmissionPluginsTests()
})
