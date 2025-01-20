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

package e2enode

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientset "k8s.io/client-go/kubernetes"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/cpuset"
)

const (
	fakeExtendedResource = "dummy.com/dummy"
)

func patchNode(ctx context.Context, client clientset.Interface, old *v1.Node, new *v1.Node) error {
	oldData, err := json.Marshal(old)
	if err != nil {
		return err
	}

	newData, err := json.Marshal(new)
	if err != nil {
		return err
	}
	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &v1.Node{})
	if err != nil {
		return fmt.Errorf("failed to create merge patch for node %q: %w", old.Name, err)
	}
	_, err = client.CoreV1().Nodes().Patch(ctx, old.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	return err
}

func addExtendedResource(clientSet clientset.Interface, nodeName, extendedResourceName string, extendedResourceQuantity resource.Quantity) {
	extendedResource := v1.ResourceName(extendedResourceName)

	ginkgo.By("Adding a custom resource")
	OriginalNode, err := clientSet.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	framework.ExpectNoError(err)

	node := OriginalNode.DeepCopy()
	node.Status.Capacity[extendedResource] = extendedResourceQuantity
	node.Status.Allocatable[extendedResource] = extendedResourceQuantity
	err = patchNode(context.Background(), clientSet, OriginalNode.DeepCopy(), node)
	framework.ExpectNoError(err)

	gomega.Eventually(func() error {
		node, err = clientSet.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		fakeResourceCapacity, exists := node.Status.Capacity[extendedResource]
		if !exists {
			return fmt.Errorf("node %s has no %s resource capacity", node.Name, extendedResourceName)
		}
		if expectedResource := resource.MustParse("123"); fakeResourceCapacity.Cmp(expectedResource) != 0 {
			return fmt.Errorf("node %s has resource capacity %s, expected: %s", node.Name, fakeResourceCapacity.String(), expectedResource.String())
		}

		return nil
	}).WithTimeout(30 * time.Second).WithPolling(time.Second).ShouldNot(gomega.HaveOccurred())
}

func removeExtendedResource(clientSet clientset.Interface, nodeName, extendedResourceName string) {
	extendedResource := v1.ResourceName(extendedResourceName)

	ginkgo.By("Removing a custom resource")
	originalNode, err := clientSet.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	framework.ExpectNoError(err)

	node := originalNode.DeepCopy()
	delete(node.Status.Capacity, extendedResource)
	delete(node.Status.Allocatable, extendedResource)
	err = patchNode(context.Background(), clientSet, originalNode.DeepCopy(), node)
	framework.ExpectNoError(err)

	gomega.Eventually(func() error {
		node, err = clientSet.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		framework.ExpectNoError(err)

		if _, exists := node.Status.Capacity[extendedResource]; exists {
			return fmt.Errorf("node %s has resource capacity %s which is expected to be removed", node.Name, extendedResourceName)
		}

		return nil
	}).WithTimeout(30 * time.Second).WithPolling(time.Second).ShouldNot(gomega.HaveOccurred())
}

func cpuManagerPolicyKubeletConfig(ctx context.Context, f *framework.Framework, oldCfg *kubeletconfig.KubeletConfiguration, cpuManagerPolicyName string, cpuManagerPolicyOptions map[string]string, isInPlacePodVerticalScalingAllocatedStatusEnabled bool, isInPlacePodVerticalScalingExclusiveCPUsEnabled bool) {
	if cpuManagerPolicyName != "" {
		if cpuManagerPolicyOptions != nil {
			func() {
				var cpuAlloc int64
				for policyOption, policyOptionValue := range cpuManagerPolicyOptions {
					if policyOption == cpumanager.FullPCPUsOnlyOption && policyOptionValue == "true" {
						_, cpuAlloc, _ = getLocalNodeCPUDetails(ctx, f)
						smtLevel := getSMTLevel()

						// strict SMT alignment is trivially verified and granted on non-SMT systems
						if smtLevel < 2 {
							e2eskipper.Skipf("Skipping Pod Resize along side CPU Manager %s tests since SMT disabled", policyOption)
						}

						// our tests want to allocate a full core, so we need at last 2*2=4 virtual cpus
						if cpuAlloc < int64(smtLevel*2) {
							e2eskipper.Skipf("Skipping Pod resize along side CPU Manager %s tests since the CPU capacity < 4", policyOption)
						}

						framework.Logf("SMT level %d", smtLevel)
						return
					}
				}
			}()

			// TODO: we assume the first available CPUID is 0, which is pretty fair, but we should probably
			// check what we do have in the node.
			newCfg := configureCPUManagerInKubelet(oldCfg,
				&cpuManagerKubeletArguments{
					policyName:              cpuManagerPolicyName,
					reservedSystemCPUs:      cpuset.New(0),
					enableCPUManagerOptions: true,
					options:                 cpuManagerPolicyOptions,
				},
				isInPlacePodVerticalScalingAllocatedStatusEnabled,
				isInPlacePodVerticalScalingExclusiveCPUsEnabled,
			)
			updateKubeletConfig(ctx, f, newCfg, true)
		} else {
			var cpuCap int64
			cpuCap, _, _ = getLocalNodeCPUDetails(ctx, f)
			// Skip CPU Manager tests altogether if the CPU capacity < 2.
			if cpuCap < 2 {
				e2eskipper.Skipf("Skipping Pod Resize alongside CPU Manager tests since the CPU capacity < 2")
			}
			// Enable CPU Manager in the kubelet.
			newCfg := configureCPUManagerInKubelet(oldCfg, &cpuManagerKubeletArguments{
				policyName:         cpuManagerPolicyName,
				reservedSystemCPUs: cpuset.CPUSet{},
			}, isInPlacePodVerticalScalingAllocatedStatusEnabled, isInPlacePodVerticalScalingExclusiveCPUsEnabled)
			updateKubeletConfig(ctx, f, newCfg, true)
		}
	}
}

type cpuManagerPolicyConfig struct {
	name    string
	title   string
	options map[string]string
}

func doPodResizeTests(policy cpuManagerPolicyConfig, isInPlacePodVerticalScalingAllocatedStatusEnabled bool, isInPlacePodVerticalScalingExclusiveCPUsEnabled bool) {
	f := framework.NewDefaultFramework("pod-resize-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var podClient *e2epod.PodClient
	var oldCfg *kubeletconfig.KubeletConfiguration
	ginkgo.BeforeEach(func(ctx context.Context) {
		var err error
		node := getLocalNode(ctx, f)
		if framework.NodeOSDistroIs("windows") || e2enode.IsARM64(node) {
			e2eskipper.Skipf("runtime does not support InPlacePodVerticalScaling -- skipping")
		}
		podClient = e2epod.NewPodClient(f)
		if oldCfg == nil {
			oldCfg, err = getCurrentKubeletConfig(ctx)
			framework.ExpectNoError(err)
		}
	})

	type testCase struct {
		name                string
		containers          []e2epod.ResizableContainerInfo
		patchString         string
		expected            []e2epod.ResizableContainerInfo
		addExtendedResource bool
	}

	noRestart := v1.NotRequired
	doRestart := v1.RestartContainer
	tests := []testCase{
		{
			name: "Guaranteed QoS pod, one container - increase CPU & memory",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"200m","memory":"400Mi"},"limits":{"cpu":"200m","memory":"400Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "200m", MemReq: "400Mi", MemLim: "400Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - decrease CPU & memory",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "300m", MemReq: "500Mi", MemLim: "500Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"100m","memory":"250Mi"},"limits":{"cpu":"100m","memory":"250Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "250Mi", MemLim: "250Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU & decrease memory",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "200Mi", MemLim: "200Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"200m","memory":"100Mi"},"limits":{"cpu":"200m","memory":"100Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "200m", MemReq: "100Mi", MemLim: "100Mi"},
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - decrease CPU & increase memory",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "200Mi", MemLim: "200Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"50m","memory":"300Mi"},"limits":{"cpu":"50m","memory":"300Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "50m", MemReq: "300Mi", MemLim: "300Mi"},
				},
			},
		},
		{
			name: "Guaranteed QoS pod, three containers (c1, c2, c3) - increase: CPU (c1,c3), memory (c2) ; decrease: CPU (c2), memory (c1,c3)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "200m", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "300m", MemReq: "300Mi", MemLim: "300Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"140m","memory":"50Mi"},"limits":{"cpu":"140m","memory":"50Mi"}}},
						{"name":"c2", "resources":{"requests":{"cpu":"150m","memory":"240Mi"},"limits":{"cpu":"150m","memory":"240Mi"}}},
						{"name":"c3", "resources":{"requests":{"cpu":"340m","memory":"250Mi"},"limits":{"cpu":"340m","memory":"250Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "140m", CPULim: "140m", MemReq: "50Mi", MemLim: "50Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "150m", CPULim: "150m", MemReq: "240Mi", MemLim: "240Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "340m", CPULim: "340m", MemReq: "250Mi", MemLim: "250Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease memory requests only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"200Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "200Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease memory limits only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"limits":{"memory":"400Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "400Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase memory requests only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"300Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "300Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase memory limits only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"limits":{"memory":"600Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "600Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease CPU requests only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"100m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease CPU limits only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"limits":{"cpu":"300m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "300m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase CPU requests only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"150m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "150m", CPULim: "200m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase CPU limits only",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"limits":{"cpu":"500m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "500m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease CPU requests and limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"100m"},"limits":{"cpu":"200m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase CPU requests and limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"200m"},"limits":{"cpu":"400m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease CPU requests and increase CPU limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"100m"},"limits":{"cpu":"500m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "500m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase CPU requests and decrease CPU limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "400m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"200m"},"limits":{"cpu":"300m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "300m", MemReq: "250Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease memory requests and limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"100Mi"},"limits":{"memory":"300Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "300Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase memory requests and limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"300Mi"},"limits":{"memory":"500Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "300Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease memory requests and increase memory limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"100Mi"},"limits":{"memory":"500Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase memory requests and decrease memory limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"300Mi"},"limits":{"memory":"300Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "300Mi", MemLim: "300Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease CPU requests and increase memory limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"100m"},"limits":{"memory":"500Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "400m", MemReq: "200Mi", MemLim: "500Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase CPU requests and decrease memory limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "400m", MemReq: "200Mi", MemLim: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"200m"},"limits":{"memory":"400Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - decrease memory requests and increase CPU limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"100Mi"},"limits":{"cpu":"300m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "300m", MemReq: "100Mi", MemLim: "400Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests + limits - increase memory requests and decrease CPU limits",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "400m", MemReq: "200Mi", MemLim: "400Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"300Mi"},"limits":{"cpu":"300m"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "300m", MemReq: "300Mi", MemLim: "400Mi"},
				},
			},
		},
		{
			name: "Burstable QoS pod, one container with cpu & memory requests - decrease memory request",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", MemReq: "500Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"400Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", MemReq: "400Mi"},
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU (NotRequired) & memory (RestartContainer)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &doRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"200m","memory":"400Mi"},"limits":{"cpu":"200m","memory":"400Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:         "c1",
					Resources:    &e2epod.ContainerResources{CPUReq: "200m", CPULim: "200m", MemReq: "400Mi", MemLim: "400Mi"},
					CPUPolicy:    &noRestart,
					MemPolicy:    &doRestart,
					RestartCount: 1,
				},
			},
		},
		{
			name: "Burstable QoS pod, one container - decrease CPU (RestartContainer) & memory (NotRequired)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "200Mi", MemLim: "400Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"50m","memory":"100Mi"},"limits":{"cpu":"100m","memory":"200Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:         "c1",
					Resources:    &e2epod.ContainerResources{CPUReq: "50m", CPULim: "100m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy:    &doRestart,
					MemPolicy:    &noRestart,
					RestartCount: 1,
				},
			},
		},
		{
			name: "Burstable QoS pod, three containers - increase c1 resources, no change for c2, decrease c3 resources (no net change for pod)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "300m", MemReq: "200Mi", MemLim: "300Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "400m", MemReq: "300Mi", MemLim: "400Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"150m","memory":"150Mi"},"limits":{"cpu":"250m","memory":"250Mi"}}},
						{"name":"c3", "resources":{"requests":{"cpu":"250m","memory":"250Mi"},"limits":{"cpu":"350m","memory":"350Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "150m", CPULim: "250m", MemReq: "150Mi", MemLim: "250Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "300m", MemReq: "200Mi", MemLim: "300Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "250m", CPULim: "350m", MemReq: "250Mi", MemLim: "350Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
		},
		{
			name: "Burstable QoS pod, three containers - decrease c1 resources, increase c2 resources, no change for c3 (net increase for pod)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "300m", MemReq: "200Mi", MemLim: "300Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "400m", MemReq: "300Mi", MemLim: "400Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"cpu":"50m","memory":"50Mi"},"limits":{"cpu":"150m","memory":"150Mi"}}},
						{"name":"c2", "resources":{"requests":{"cpu":"350m","memory":"350Mi"},"limits":{"cpu":"450m","memory":"450Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "150m", MemReq: "50Mi", MemLim: "150Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:         "c2",
					Resources:    &e2epod.ContainerResources{CPUReq: "350m", CPULim: "450m", MemReq: "350Mi", MemLim: "450Mi"},
					CPUPolicy:    &noRestart,
					MemPolicy:    &doRestart,
					RestartCount: 1,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "400m", MemReq: "300Mi", MemLim: "400Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
		},
		{
			name: "Burstable QoS pod, three containers - no change for c1, increase c2 resources, decrease c3 (net decrease for pod)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "300m", MemReq: "200Mi", MemLim: "300Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "400m", MemReq: "300Mi", MemLim: "400Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &doRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c2", "resources":{"requests":{"cpu":"250m","memory":"250Mi"},"limits":{"cpu":"350m","memory":"350Mi"}}},
						{"name":"c3", "resources":{"requests":{"cpu":"100m","memory":"100Mi"},"limits":{"cpu":"200m","memory":"200Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:         "c2",
					Resources:    &e2epod.ContainerResources{CPUReq: "250m", CPULim: "350m", MemReq: "250Mi", MemLim: "350Mi"},
					CPUPolicy:    &noRestart,
					MemPolicy:    &noRestart,
					RestartCount: 1,
				},
				{
					Name:         "c3",
					Resources:    &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy:    &doRestart,
					MemPolicy:    &doRestart,
					RestartCount: 1,
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU & memory with an extended resource",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name: "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "200Mi", MemLim: "200Mi",
						ExtendedResourceReq: "1", ExtendedResourceLim: "1"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
					{"name":"c1", "resources":{"requests":{"cpu":"200m","memory":"400Mi"},"limits":{"cpu":"200m","memory":"400Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name: "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "200m", MemReq: "400Mi", MemLim: "400Mi",
						ExtendedResourceReq: "1", ExtendedResourceLim: "1"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			addExtendedResource: true,
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU & memory, with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "2",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"4","memory":"400Mi"},"limits":{"cpu":"4","memory":"400Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "400Mi", MemLim: "400Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "4",
				},
			},
		},
		{
			name: "Burstable QoS pod, three containers - no change for c1, decrease c2 resources, decrease c3 (net decrease for pod)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "200Mi", MemLim: "300Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "400m", MemReq: "300Mi", MemLim: "400Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &doRestart,
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c2", "resources":{"requests":{"cpu":"1","memory":"150Mi"},"limits":{"cpu":"1","memory":"250Mi"}}},
						{"name":"c3", "resources":{"requests":{"cpu":"100m","memory":"100Mi"},"limits":{"cpu":"200m","memory":"200Mi"}}}
					]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:         "c2",
					Resources:    &e2epod.ContainerResources{CPUReq: "1", CPULim: "1", MemReq: "150Mi", MemLim: "250Mi"},
					CPUPolicy:    &doRestart,
					MemPolicy:    &noRestart,
					RestartCount: 1,
				},
				{
					Name:         "c3",
					Resources:    &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy:    &noRestart,
					MemPolicy:    &doRestart,
					RestartCount: 1,
				},
			},
		},
		{
			name: "Burstable QoS pod, three containers - no change for c1, increase c2 resources, decrease c3 (net increase for pod)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "200Mi", MemLim: "300Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "400m", MemReq: "300Mi", MemLim: "400Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &doRestart,
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c2", "resources":{"requests":{"cpu":"4","memory":"250Mi"},"limits":{"cpu":"4","memory":"350Mi"}}},
							{"name":"c3", "resources":{"requests":{"cpu":"100m","memory":"100Mi"},"limits":{"cpu":"200m","memory":"200Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy: &doRestart,
					MemPolicy: &doRestart,
				},
				{
					Name:         "c2",
					Resources:    &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "250Mi", MemLim: "350Mi"},
					CPUPolicy:    &doRestart,
					MemPolicy:    &noRestart,
					RestartCount: 1,
				},
				{
					Name:         "c3",
					Resources:    &e2epod.ContainerResources{CPUReq: "100m", CPULim: "200m", MemReq: "100Mi", MemLim: "200Mi"},
					CPUPolicy:    &noRestart,
					MemPolicy:    &doRestart,
					RestartCount: 1,
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - decrease CPU & increase memory",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "200Mi", MemLim: "200Mi"},
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"50m","memory":"300Mi"},"limits":{"cpu":"50m","memory":"300Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "50m", CPULim: "50m", MemReq: "300Mi", MemLim: "300Mi"},
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - decrease CPU & memory, with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "500Mi", MemLim: "500Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "4",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"2","memory":"250Mi"},"limits":{"cpu":"2","memory":"250Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "250Mi", MemLim: "250Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "2",
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - decrease CPU & memory, with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "500Mi", MemLim: "500Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "4",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"2","memory":"250Mi"},"limits":{"cpu":"2","memory":"250Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "250Mi", MemLim: "250Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "2",
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU & decrease memory, with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "200Mi", MemLim: "200Mi"},
					CPUsAllowedListValue: "2",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"4","memory":"100Mi"},"limits":{"cpu":"4","memory":"100Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "100Mi", MemLim: "100Mi"},
					CPUsAllowedListValue: "4",
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU & decrease memory, with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "200Mi", MemLim: "200Mi"},
					CPUsAllowedListValue: "2",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"4","memory":"100Mi"},"limits":{"cpu":"4","memory":"100Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "100Mi", MemLim: "100Mi"},
					CPUsAllowedListValue: "4",
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU & memory, with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "2",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"4","memory":"400Mi"},"limits":{"cpu":"4","memory":"400Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "400Mi", MemLim: "400Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "4",
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU (NotRequired) & memory (RestartContainer), with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &doRestart,
					CPUsAllowedListValue: "2",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"4","memory":"400Mi"},"limits":{"cpu":"4","memory":"400Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "400Mi", MemLim: "400Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &doRestart,
					CPUsAllowedListValue: "4",
					RestartCount:         1,
				},
			},
		},
		{
			name: "Guaranteed QoS pod, one container - increase CPU (NotRequired) & memory (RestartContainer), with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &doRestart,
					CPUsAllowedListValue: "2",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"4","memory":"400Mi"},"limits":{"cpu":"4","memory":"400Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "400Mi", MemLim: "400Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &doRestart,
					CPUsAllowedListValue: "4",
					RestartCount:         1,
				},
			},
		},
		{
			name: "Guaranteed QoS pod, three containers (c1, c2, c3) - increase CPU (c1,c3) and memory (c2) ; decrease CPU (c2) and memory (c1,c3)",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "100m", CPULim: "100m", MemReq: "100Mi", MemLim: "100Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "200m", CPULim: "200m", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "300m", CPULim: "300m", MemReq: "300Mi", MemLim: "300Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"140m","memory":"50Mi"},"limits":{"cpu":"140m","memory":"50Mi"}}},
							{"name":"c2", "resources":{"requests":{"cpu":"150m","memory":"240Mi"},"limits":{"cpu":"150m","memory":"240Mi"}}},
							{"name":"c3", "resources":{"requests":{"cpu":"340m","memory":"250Mi"},"limits":{"cpu":"340m","memory":"250Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &e2epod.ContainerResources{CPUReq: "140m", CPULim: "140m", MemReq: "50Mi", MemLim: "50Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c2",
					Resources: &e2epod.ContainerResources{CPUReq: "150m", CPULim: "150m", MemReq: "240Mi", MemLim: "240Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
				{
					Name:      "c3",
					Resources: &e2epod.ContainerResources{CPUReq: "340m", CPULim: "340m", MemReq: "250Mi", MemLim: "250Mi"},
					CPUPolicy: &noRestart,
					MemPolicy: &noRestart,
				},
			},
		},
		{
			name: "Guaranteed QoS pod, three containers (c1, c2, c3) - increase CPU (c1,c3) and memory (c2) ; decrease CPU (c2) and memory (c1,c3), with integer CPU requests",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "100Mi", MemLim: "100Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "2",
				},
				{
					Name:                 "c2",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "200Mi", MemLim: "200Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "4",
				},
				{
					Name:                 "c3",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "300Mi", MemLim: "300Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "2",
				},
			},
			patchString: `{"spec":{"containers":[
							{"name":"c1", "resources":{"requests":{"cpu":"4","memory":"50Mi"},"limits":{"cpu":"4","memory":"50Mi"}}},
							{"name":"c2", "resources":{"requests":{"cpu":"2","memory":"240Mi"},"limits":{"cpu":"2","memory":"240Mi"}}},
							{"name":"c3", "resources":{"requests":{"cpu":"4","memory":"250Mi"},"limits":{"cpu":"4","memory":"250Mi"}}}
						]}}`,
			expected: []e2epod.ResizableContainerInfo{
				{
					Name:                 "c1",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "50Mi", MemLim: "50Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "4",
				},
				{
					Name:                 "c2",
					Resources:            &e2epod.ContainerResources{CPUReq: "2", CPULim: "2", MemReq: "240Mi", MemLim: "240Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "2",
				},
				{
					Name:                 "c3",
					Resources:            &e2epod.ContainerResources{CPUReq: "4", CPULim: "4", MemReq: "250Mi", MemLim: "250Mi"},
					CPUPolicy:            &noRestart,
					MemPolicy:            &noRestart,
					CPUsAllowedListValue: "4",
				},
			},
		},
	}

	timeouts := framework.NewTimeoutContext()

	for idx := range tests {
		tc := tests[idx]
		ginkgo.It(tc.name+policy.title+" (InPlacePodVerticalScalingAllocatedStatus="+strconv.FormatBool(isInPlacePodVerticalScalingAllocatedStatusEnabled)+", InPlacePodVerticalScalingExclusiveCPUs="+strconv.FormatBool(isInPlacePodVerticalScalingExclusiveCPUsEnabled)+")", func(ctx context.Context) {
			cpuManagerPolicyKubeletConfig(ctx, f, oldCfg, policy.name, policy.options, isInPlacePodVerticalScalingAllocatedStatusEnabled, isInPlacePodVerticalScalingExclusiveCPUsEnabled)

			var testPod, patchedPod *v1.Pod
			var pErr error

			tStamp := strconv.Itoa(time.Now().Nanosecond())
			testPod = e2epod.MakePodWithResizableContainers(f.Namespace.Name, "testpod", tStamp, tc.containers)
			testPod.GenerateName = "resize-test-"
			testPod = e2epod.MustMixinRestrictedPodSecurity(testPod)

			if tc.addExtendedResource {
				nodes, err := e2enode.GetReadySchedulableNodes(context.Background(), f.ClientSet)
				framework.ExpectNoError(err)

				for _, node := range nodes.Items {
					addExtendedResource(f.ClientSet, node.Name, fakeExtendedResource, resource.MustParse("123"))
				}
				defer func() {
					for _, node := range nodes.Items {
						removeExtendedResource(f.ClientSet, node.Name, fakeExtendedResource)
					}
				}()
			}

			ginkgo.By("creating pod")
			newPod := podClient.CreateSync(ctx, testPod)

			ginkgo.By("verifying initial pod resources, allocations are as expected")
			e2epod.VerifyPodResources(newPod, tc.containers)
			ginkgo.By("verifying initial pod resize policy is as expected")
			e2epod.VerifyPodResizePolicy(newPod, tc.containers)

			ginkgo.By("verifying initial pod status resources are as expected")
			framework.ExpectNoError(e2epod.VerifyPodStatusResources(newPod, tc.containers))
			ginkgo.By("verifying initial cgroup config are as expected")
			framework.ExpectNoError(e2epod.VerifyPodContainersCgroupValues(ctx, f, newPod, tc.containers))
			// TODO make this dynamic depending on Policy Name, Resources input and topology of target
			// machine.
			// For the moment skip below if CPU Manager Policy is set to none
			if policy.name == string(cpumanager.PolicyStatic) {
				ginkgo.By("verifying initial pod Cpus allowed list value")
				gomega.Eventually(ctx, e2epod.VerifyPodContainersCPUsAllowedListValue, timeouts.PodStartShort, timeouts.Poll).
					WithArguments(f, newPod, tc.containers).
					Should(gomega.BeNil(), "failed to verify initial Pod CPUsAllowedListValue")
			}

			patchAndVerify := func(patchString string, expectedContainers []e2epod.ResizableContainerInfo, initialContainers []e2epod.ResizableContainerInfo, opStr string, isRollback bool) {
				ginkgo.By(fmt.Sprintf("patching pod for %s", opStr))
				patchedPod, pErr = f.ClientSet.CoreV1().Pods(newPod.Namespace).Patch(ctx, newPod.Name,
					types.StrategicMergePatchType, []byte(patchString), metav1.PatchOptions{}, "resize")
				framework.ExpectNoError(pErr, fmt.Sprintf("failed to patch pod for %s", opStr))

				ginkgo.By(fmt.Sprintf("verifying pod patched for %s", opStr))
				e2epod.VerifyPodResources(patchedPod, expectedContainers)

				ginkgo.By(fmt.Sprintf("waiting for %s to be actuated", opStr))
				resizedPod := e2epod.WaitForPodResizeActuation(ctx, f, podClient, newPod)
				e2epod.ExpectPodResized(ctx, f, resizedPod, expectedContainers)

				// Check cgroup values only for containerd versions before 1.6.9
				ginkgo.By(fmt.Sprintf("verifying pod container's cgroup values after %s", opStr))
				framework.ExpectNoError(e2epod.VerifyPodContainersCgroupValues(ctx, f, resizedPod, expectedContainers))

				ginkgo.By(fmt.Sprintf("verifying pod resources after %s", opStr))
				e2epod.VerifyPodResources(resizedPod, expectedContainers)

				// TODO make this dynamic depending on Policy Name, Resources input and topology of target
				// machine.
				// For the moment skip below if CPU Manager Policy is set to none
				if policy.name == string(cpumanager.PolicyStatic) {
					ginkgo.By("verifying pod Cpus allowed list value after resize")
					if isInPlacePodVerticalScalingExclusiveCPUsEnabled {
						gomega.Eventually(ctx, e2epod.VerifyPodContainersCPUsAllowedListValue, timeouts.PodStartShort, timeouts.Poll).
							WithArguments(f, resizedPod, tc.expected).
							Should(gomega.BeNil(), "failed to verify Pod CPUsAllowedListValue for resizedPod with InPlacePodVerticalScalingExclusiveCPUs enabled")
					} else {
						gomega.Eventually(ctx, e2epod.VerifyPodContainersCPUsAllowedListValue, timeouts.PodStartShort, timeouts.Poll).
							WithArguments(f, resizedPod, tc.containers).
							Should(gomega.BeNil(), "failed to verify Pod CPUsAllowedListValue for resizedPod with InPlacePodVerticalScalingExclusiveCPUs disabled (default)")
					}
				}
			}

			patchAndVerify(tc.patchString, tc.expected, tc.containers, "resize", false)

			rbPatchStr, err := e2epod.ResizeContainerPatch(tc.containers)
			framework.ExpectNoError(err)
			// Resize has been actuated, test rollback
			patchAndVerify(rbPatchStr, tc.containers, tc.expected, "rollback", true)

			ginkgo.By("deleting pod")
			deletePodSyncByName(ctx, f, newPod.Name)
			// we need to wait for all containers to really be gone so cpumanager reconcile loop will not rewrite the cpu_manager_state.
			// this is in turn needed because we will have an unavoidable (in the current framework) race with the
			// reconcile loop which will make our attempt to delete the state file and to restore the old config go haywire
			waitForAllContainerRemoval(ctx, newPod.Name, newPod.Namespace)
		})
	}

	ginkgo.AfterEach(func(ctx context.Context) {
		if oldCfg != nil {
			updateKubeletConfig(ctx, f, oldCfg, true)
		}
	})

}

func doPodResizeErrorTests(policy cpuManagerPolicyConfig, isInPlacePodVerticalScalingAllocatedStatusEnabled bool, isInPlacePodVerticalScalingExclusiveCPUsEnabled bool) {
	f := framework.NewDefaultFramework("pod-resize-errors")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var podClient *e2epod.PodClient
	var oldCfg *kubeletconfig.KubeletConfiguration
	ginkgo.BeforeEach(func(ctx context.Context) {
		var err error
		node := getLocalNode(ctx, f)
		if framework.NodeOSDistroIs("windows") || e2enode.IsARM64(node) {
			e2eskipper.Skipf("runtime does not support InPlacePodVerticalScaling -- skipping")
		}
		podClient = e2epod.NewPodClient(f)
		if oldCfg == nil {
			oldCfg, err = getCurrentKubeletConfig(ctx)
			framework.ExpectNoError(err)
		}
	})

	type testCase struct {
		name        string
		containers  []e2epod.ResizableContainerInfo
		patchString string
		patchError  string
		expected    []e2epod.ResizableContainerInfo
	}

	tests := []testCase{
		{
			name: "BestEffort QoS pod, one container - try requesting memory, expect error",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name: "c1",
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"400Mi"}}}
					]}}`,
			patchError: "Pod QoS is immutable",
			expected: []e2epod.ResizableContainerInfo{
				{
					Name: "c1",
				},
			},
		},
		{
			name: "BestEffort QoS pod, three containers - try requesting memory for c1, expect error",
			containers: []e2epod.ResizableContainerInfo{
				{
					Name: "c1",
				},
				{
					Name: "c2",
				},
				{
					Name: "c3",
				},
			},
			patchString: `{"spec":{"containers":[
						{"name":"c1", "resources":{"requests":{"memory":"400Mi"}}}
					]}}`,
			patchError: "Pod QoS is immutable",
			expected: []e2epod.ResizableContainerInfo{
				{
					Name: "c1",
				},
				{
					Name: "c2",
				},
				{
					Name: "c3",
				},
			},
		},
	}

	timeouts := framework.NewTimeoutContext()

	for idx := range tests {
		tc := tests[idx]
		ginkgo.It(tc.name+policy.title+" (InPlacePodVerticalScalingAllocatedStatus="+strconv.FormatBool(isInPlacePodVerticalScalingAllocatedStatusEnabled)+", InPlacePodVerticalScalingExclusiveCPUs="+strconv.FormatBool(isInPlacePodVerticalScalingExclusiveCPUsEnabled)+")", func(ctx context.Context) {
			var testPod, patchedPod *v1.Pod
			var pErr error

			tStamp := strconv.Itoa(time.Now().Nanosecond())
			testPod = e2epod.MakePodWithResizableContainers(f.Namespace.Name, "testpod", tStamp, tc.containers)
			testPod = e2epod.MustMixinRestrictedPodSecurity(testPod)

			ginkgo.By("creating pod")
			newPod := podClient.CreateSync(ctx, testPod)

			perr := e2epod.WaitForPodCondition(ctx, f.ClientSet, newPod.Namespace, newPod.Name, "Ready", timeouts.PodStartSlow, testutils.PodRunningReady)
			framework.ExpectNoError(perr, "pod %s/%s did not go running", newPod.Namespace, newPod.Name)
			framework.Logf("pod %s/%s running", newPod.Namespace, newPod.Name)

			ginkgo.By("verifying initial pod resources, allocations, and policy are as expected")
			e2epod.VerifyPodResources(newPod, tc.containers)
			e2epod.VerifyPodResizePolicy(newPod, tc.containers)

			ginkgo.By("verifying initial pod status resources and cgroup config are as expected")
			framework.ExpectNoError(e2epod.VerifyPodStatusResources(newPod, tc.containers))

			ginkgo.By("patching pod for resize")
			patchedPod, pErr = f.ClientSet.CoreV1().Pods(newPod.Namespace).Patch(ctx, newPod.Name,
				types.StrategicMergePatchType, []byte(tc.patchString), metav1.PatchOptions{})
			if tc.patchError == "" {
				framework.ExpectNoError(pErr, "failed to patch pod for resize")
			} else {
				gomega.Expect(pErr).To(gomega.HaveOccurred(), tc.patchError)
				patchedPod = newPod
			}

			ginkgo.By("verifying pod resources after patch")
			e2epod.VerifyPodResources(patchedPod, tc.expected)

			deletePodSyncByName(ctx, f, newPod.Name)
			// we need to wait for all containers to really be gone so cpumanager reconcile loop will not rewrite the cpu_manager_state.
			// this is in turn needed because we will have an unavoidable (in the current framework) race with the
			// reconcile loop which will make our attempt to delete the state file and to restore the old config go haywire
			waitForAllContainerRemoval(ctx, newPod.Name, newPod.Namespace)

		})
	}

	ginkgo.AfterEach(func(ctx context.Context) {
		if oldCfg != nil {
			updateKubeletConfig(ctx, f, oldCfg, true)
		}
	})

}

// NOTE: Pod resize scheduler resource quota tests are out of scope in e2e_node tests,
//       because in e2e_node tests
//          a) scheduler and controller manager is not running by the Node e2e
//          b) api-server in services doesn't start with --enable-admission-plugins=ResourceQuota
//             and is not possible to start it from TEST_ARGS
//       Above tests are performed by doSheduletTests() and doPodResizeResourceQuotaTests()
//       in test/e2e/node/pod_resize.go

var _ = SIGDescribe("Pod InPlace Resize Container", framework.WithSerial(), func() {

	policiesGeneralAvailability := []cpuManagerPolicyConfig{
		{
			name:  string(cpumanager.PolicyNone),
			title: "",
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with no options",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "false",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "false",
				cpumanager.AlignBySocketOption:             "false",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
	}

	policiesBeta := []cpuManagerPolicyConfig{
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with FullPCPUsOnlyOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "true",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "false",
				cpumanager.AlignBySocketOption:             "false",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
	}

	/*policiesAlpha := []cpuManagerPolicyConfig{
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with DistributeCPUsAcrossNUMAOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "false",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "true",
				cpumanager.AlignBySocketOption:             "false",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with FullPCPUsOnlyOption, DistributeCPUsAcrossNUMAOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "true",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "true",
				cpumanager.AlignBySocketOption:             "false",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with AlignBySocketOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "false",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "false",
				cpumanager.AlignBySocketOption:             "true",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with FullPCPUsOnlyOption, AlignBySocketOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "true",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "false",
				cpumanager.AlignBySocketOption:             "true",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with DistributeCPUsAcrossNUMAOption, AlignBySocketOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "false",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "true",
				cpumanager.AlignBySocketOption:             "true",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with FullPCPUsOnlyOption, DistributeCPUsAcrossNUMAOption, AlignBySocketOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "true",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "true",
				cpumanager.AlignBySocketOption:             "true",
				cpumanager.DistributeCPUsAcrossCoresOption: "false",
			},
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with DistributeCPUsAcrossCoresOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "false",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "false",
				cpumanager.AlignBySocketOption:             "false",
				cpumanager.DistributeCPUsAcrossCoresOption: "true",
			},
		},
		{
			name:  string(cpumanager.PolicyStatic),
			title: ", alongside CPU Manager Static Policy with DistributeCPUsAcrossCoresOption, AlignBySocketOption",
			options: map[string]string{
				cpumanager.FullPCPUsOnlyOption:             "false",
				cpumanager.DistributeCPUsAcrossNUMAOption:  "false",
				cpumanager.AlignBySocketOption:             "true",
				cpumanager.DistributeCPUsAcrossCoresOption: "true",
			},
		},
	}*/

	for idp := range policiesGeneralAvailability {
		doPodResizeTests(policiesGeneralAvailability[idp], false, false)
		doPodResizeTests(policiesGeneralAvailability[idp], true, false)
		doPodResizeTests(policiesGeneralAvailability[idp], false, true)
		doPodResizeTests(policiesGeneralAvailability[idp], true, true)
		doPodResizeErrorTests(policiesGeneralAvailability[idp], false, false)
		doPodResizeErrorTests(policiesGeneralAvailability[idp], true, false)
		doPodResizeErrorTests(policiesGeneralAvailability[idp], false, true)
		doPodResizeErrorTests(policiesGeneralAvailability[idp], true, true)
	}

	for idp := range policiesBeta {
		doPodResizeTests(policiesBeta[idp], false, false)
		doPodResizeTests(policiesBeta[idp], true, false)
		doPodResizeTests(policiesBeta[idp], false, true)
		doPodResizeTests(policiesBeta[idp], true, true)
		doPodResizeErrorTests(policiesBeta[idp], false, false)
		doPodResizeErrorTests(policiesBeta[idp], true, false)
		doPodResizeErrorTests(policiesBeta[idp], false, true)
		doPodResizeErrorTests(policiesBeta[idp], true, true)
	}

	/*for idp := range policiesAlpha {
		doPodResizeTests(policiesAlpha[idp], true, false)
		doPodResizeTests(policiesAlpha[idp], true, true)
		doPodResizeErrorTests(policiesAlpha[idp], true, false)
		doPodResizeErrorTests(policiesAlpha[idp], true, true)
	}*/

})
