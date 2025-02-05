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

package windows

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	// It can take 10-15 seconds for node memory-pressure taint to show up on the node
	// so we'll wait 45 seconds for the taint to show up so the e2e test case can catch
	// it and the wait for the taint to be removed so other serial/slow tests can run
	// against the same node.
	waitForNodeMemoryPressureTaintDelayDuration = 45 * time.Second
)

var _ = sigDescribe(feature.Windows, "Eviction", framework.WithSerial(), framework.WithSlow(), framework.WithDisruptive(), (func() {
	ginkgo.BeforeEach(func() {
		e2eskipper.SkipUnlessNodeOSDistroIs("windows")
	})

	f := framework.NewDefaultFramework("eviction-test-windows")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	// This test will first find a Windows node memory-pressure hard-eviction enabled.
	// The test will then schedule a pod that requests and consumes 500Mi of memory and then
	// another pod that will consume the rest of the node's memory.
	// The test will then verify that the second pod gets evicted and then the node again becomes
	// ready for schedule after the second pod gets evicted.
	ginkgo.It("should evict a pod when a node experiences memory pressure", func(ctx context.Context) {
		framework.Logf("Looking for a Windows node with memory-pressure eviction enabled")
		selector := labels.Set{"kubernetes.io/os": "windows"}.AsSelector()
		nodeList, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		framework.ExpectNoError(err)

		var node *v1.Node
		var nodeMem nodeMemory
		for _, n := range nodeList.Items {
			nm := getNodeMemory(ctx, f, n)
			if nm.hardEviction.Value() != 0 {
				framework.Logf("Using node %s", n.Name)
				node = &n
				nodeMem = nm
				break
			}
		}

		if node == nil {
			e2eskipper.Skipf("No Windows nodes with hard memory-pressure eviction found")
		}

		// Delete img-puller pods if they exist because eviction manager keeps selecting them for eviction first
		// Note we cannot just delete the namespace because a deferred cleanup task tries to delete the ns if
		// image pre-pulling was enabled.
		nsList, err := f.ClientSet.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		framework.ExpectNoError(err)
		for _, ns := range nsList.Items {
			if strings.Contains(ns.Name, "img-puller") {
				framework.Logf("Deleting pods in namespace %s", ns.Name)
				podList, err := f.ClientSet.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
				framework.ExpectNoError(err)
				for _, pod := range podList.Items {
					framework.Logf("  Deleteing pod %s", pod.Name)
					err = f.ClientSet.CoreV1().Pods(ns.Name).Delete(ctx, pod.Name, metav1.DeleteOptions{})
					framework.ExpectNoError(err)
				}
				break
			}
		}

		ginkgo.By("Scheduling a pod that requests and consumes 500Mi of Memory")

		pod1 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod1",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "pod1",
						Image: imageutils.GetE2EImage(imageutils.ResourceConsumer),
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceMemory: *resource.NewQuantity(500*1024*1024, resource.BinarySI),
							},
						},
						Command: []string{
							"/bin/testlimit.exe",
							"-accepteula",
							"-d",
							"100Mb",
							"-e",
							"5",
							"20000s",
							"-c",
							"5"},
					},
				},
				NodeSelector: map[string]string{
					"kubernetes.io/os": "windows",
				},
				NodeName: node.Name,
			},
		}
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod1, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Scheduling another pod will consume the rest of the node's memory")
		chunks := int((nodeMem.capacity.Value()-nodeMem.hardEviction.Value())/(300*1024*1024) + 3)
		framework.Logf("Pod2 will consume %d chunks of 300Mi", chunks)
		pod2 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod2",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "pod2",
						Image: imageutils.GetE2EImage(imageutils.ResourceConsumer),
						Command: []string{
							"/bin/testlimit.exe",
							"-accepteula",
							"-d",
							"300Mb",
							"-e",
							"1",
							"20000s",
							"-c",
							strconv.Itoa(chunks)},
					},
				},
				NodeSelector: map[string]string{
					"kubernetes.io/os": "windows",
				},
				NodeName: node.Name,
			},
		}
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod2, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for pods to start running")
		err = e2epod.WaitForPodsRunningReady(ctx, f.ClientSet, f.Namespace.Name, 2, 3*time.Minute)
		framework.ExpectNoError(err)

		framework.Logf("Waiting for pod2 to get evicted")
		gomega.Eventually(ctx, func() bool {
			eventList, err := f.ClientSet.CoreV1().Events(f.Namespace.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				framework.Logf("Error getting events: %v", err)
				return false
			}
			for _, e := range eventList.Items {
				// Look for an event that shows FailedScheduling
				if e.Type == "Warning" && e.Reason == "Evicted" && strings.Contains(e.Message, "pod2") {
					framework.Logf("Found %+v event with message %+v", e.Reason, e.Message)
					return true
				}
			}
			return false
		}, 10*time.Minute, 10*time.Second).Should(gomega.BeTrueBecause("Eviction Event was not found"))

		ginkgo.By("Waiting for node.kubernetes.io/memory-pressure taint to be removed")
		// ensure e2e test framework catches the memory-pressure taint
		time.Sleep(waitForNodeMemoryPressureTaintDelayDuration)
		// wait for node.kubernetes.io/memory-pressure=NoSchedule to be removed so other tests can run
		err = e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 10*time.Minute)
		framework.ExpectNoError(err)
	})
}))
