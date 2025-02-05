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

package windows

import (
	"context"
	"encoding/json"
	"time"

	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletconfigscheme "k8s.io/kubernetes/pkg/kubelet/apis/config/scheme"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = sigDescribe(feature.Windows, "Memory Limits", framework.WithSerial(), framework.WithSlow(), skipUnlessWindows(func() {

	f := framework.NewDefaultFramework("memory-limit-test-windows")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.BeforeEach(func() {
		// NOTE(vyta): these tests are Windows specific
		e2eskipper.SkipUnlessNodeOSDistroIs("windows")
	})

	ginkgo.Context("Allocatable node memory", func() {
		ginkgo.It("should be equal to a calculated allocatable memory value", func(ctx context.Context) {
			checkNodeAllocatableTest(ctx, f)
		})
	})

	ginkgo.Context("attempt to deploy past allocatable memory limits", func() {
		ginkgo.It("should fail deployments of pods once there isn't enough memory", func(ctx context.Context) {
			overrideAllocatableMemoryTest(ctx, f, framework.TestContext.CloudConfig.NumNodes)
		})
	})
}))

type nodeMemory struct {
	// capacity
	capacity resource.Quantity
	// allocatable memory
	allocatable resource.Quantity
	// memory reserved for OS level processes
	systemReserve resource.Quantity
	// memory reserved for kubelet (not implemented)
	kubeReserve resource.Quantity
	// grace period memory limit (not implemented)
	softEviction resource.Quantity
	// no grace period memory limit
	hardEviction resource.Quantity
}

// runDensityBatchTest runs the density batch pod creation test
// checks that a calculated value for NodeAllocatable is equal to the reported value
func checkNodeAllocatableTest(ctx context.Context, f *framework.Framework) {

	nodeMem := getFirstNodeMemory(ctx, f)
	framework.Logf("nodeMem says: %+v", nodeMem)

	// calculate the allocatable mem based on capacity - reserved amounts
	calculatedNodeAlloc := nodeMem.capacity.DeepCopy()
	calculatedNodeAlloc.Sub(nodeMem.systemReserve)
	calculatedNodeAlloc.Sub(nodeMem.kubeReserve)
	calculatedNodeAlloc.Sub(nodeMem.softEviction)
	calculatedNodeAlloc.Sub(nodeMem.hardEviction)

	// sanity check against stated allocatable
	gomega.Expect(calculatedNodeAlloc.Cmp(nodeMem.allocatable)).To(gomega.Equal(0), "calculated allocatable memory %+v and stated allocatable memory %+v are same", calculatedNodeAlloc, nodeMem.allocatable)
}

// Deploys `allocatablePods + 1` pods, each with a memory limit of `1/allocatablePods` of the total allocatable
// memory, then confirms that the last pod failed because of failedScheduling
func overrideAllocatableMemoryTest(ctx context.Context, f *framework.Framework, allocatablePods int) {
	selector := labels.Set{"kubernetes.io/os": "windows"}.AsSelector()
	nodeList, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	framework.ExpectNoError(err)

	framework.Logf("Scheduling 1 pod per node to consume all allocatable memory")
	for _, node := range nodeList.Items {
		status := node.Status
		podMemLimt := resource.NewQuantity(status.Allocatable.Memory().Value()-(1024*1024*100), resource.BinarySI)
		podName := "mem-test-" + string(uuid.NewUUID())
		framework.Logf("Scheduling pod %s on node %s (allocatable memory=%v) with memory limit %v", podName, node.Name, status.Allocatable.Memory(), podMemLimt)
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: podName,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  podName,
						Image: imageutils.GetPauseImageName(),
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceMemory: *podMemLimt,
							},
						},
					},
				},
				NodeSelector: map[string]string{
					"kubernetes.io/os": "windows",
				},
				NodeName: node.Name,
			},
		}
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
	}
	framework.Logf("Schedule additional pod which should not get scheduled")
	podName := "mem-failure-pod"
	failurePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  podName,
					Image: imageutils.GetPauseImageName(),
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(1024*1024*1024, resource.BinarySI),
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"kubernetes.io/os": "windows",
			},
		},
	}
	framework.Logf("Ensuring that pod %s fails to schedule", podName)
	failurePod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, failurePod, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	gomega.Eventually(ctx, func() bool {
		eventList, err := f.ClientSet.CoreV1().Events(f.Namespace.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			framework.Logf("Error getting events: %v", err)
			return false
		}
		for _, e := range eventList.Items {
			// Look for an event that shows FailedScheduling
			if e.Type == "Warning" && e.Reason == "FailedScheduling" && e.InvolvedObject.Name == failurePod.ObjectMeta.Name {
				framework.Logf("Found %+v event with message %+v", e.Reason, e.Message)
				return true
			}
		}
		return false
	}, 3*time.Minute, 10*time.Second).Should(gomega.BeTrueBecause("Expected %s pod to be failed scheduling", podName))
}

func getNodeMemory(ctx context.Context, f *framework.Framework, node v1.Node) nodeMemory {
	framework.Logf("Getting memory details for node %s", node.ObjectMeta.Name)
	request := f.ClientSet.CoreV1().RESTClient().Get().Resource("nodes").Name(node.ObjectMeta.Name).SubResource("proxy").Suffix("configz")
	rawbytes, err := request.DoRaw(ctx)
	framework.ExpectNoError(err)
	kubeletConfig, err := decodeConfigz(rawbytes)
	framework.ExpectNoError(err)

	systemReserve, err := resource.ParseQuantity(kubeletConfig.SystemReserved["memory"])
	if err != nil {
		systemReserve = *resource.NewQuantity(0, resource.BinarySI)
	}
	kubeReserve, err := resource.ParseQuantity(kubeletConfig.KubeReserved["memory"])
	if err != nil {
		kubeReserve = *resource.NewQuantity(0, resource.BinarySI)
	}
	hardEviction, err := resource.ParseQuantity(kubeletConfig.EvictionHard["memory.available"])
	if err != nil {
		hardEviction = *resource.NewQuantity(0, resource.BinarySI)
	}
	softEviction, err := resource.ParseQuantity(kubeletConfig.EvictionSoft["memory.available"])
	if err != nil {
		softEviction = *resource.NewQuantity(0, resource.BinarySI)
	}

	nodeMem := nodeMemory{
		capacity:      node.Status.Capacity[v1.ResourceMemory],
		allocatable:   node.Status.Allocatable[v1.ResourceMemory],
		systemReserve: systemReserve,
		hardEviction:  hardEviction,
		kubeReserve:   kubeReserve,
		softEviction:  softEviction,
	}

	return nodeMem
}

// getNodeMemory populates a nodeMemory struct with information from the first Windows node
// that is found in the cluster.
func getFirstNodeMemory(ctx context.Context, f *framework.Framework) nodeMemory {
	selector := labels.Set{"kubernetes.io/os": "windows"}.AsSelector()
	nodeList, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	framework.ExpectNoError(err)

	// Assuming that agent nodes have the same config
	// Make sure there is >0 agent nodes, then use the first one for info
	gomega.Expect(nodeList.Items).ToNot(gomega.BeEmpty())

	ginkgo.By("Getting memory details from first Windows")
	return getNodeMemory(ctx, f, nodeList.Items[0])
}

// modified from https://github.com/kubernetes/kubernetes/blob/master/test/e2e/framework/kubelet/config.go#L110
// the proxy version was causing and non proxy used a value that isn't set by e2e
func decodeConfigz(contentsBytes []byte) (*kubeletconfig.KubeletConfiguration, error) {
	// This hack because /configz reports the following structure:
	// {"kubeletconfig": {the JSON representation of kubeletconfigv1beta1.KubeletConfiguration}}
	type configzWrapper struct {
		ComponentConfig kubeletconfigv1beta1.KubeletConfiguration `json:"kubeletconfig"`
	}

	configz := configzWrapper{}
	kubeCfg := kubeletconfig.KubeletConfiguration{}

	err := json.Unmarshal(contentsBytes, &configz)
	if err != nil {
		return nil, err
	}

	scheme, _, err := kubeletconfigscheme.NewSchemeAndCodecs()
	if err != nil {
		return nil, err
	}
	err = scheme.Convert(&configz.ComponentConfig, &kubeCfg, nil)
	if err != nil {
		return nil, err
	}

	return &kubeCfg, nil
}
