/*
Copyright 2017 The Kubernetes Authors.

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

package scheduling

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	admissionapi "k8s.io/pod-security-admission/api"
)

// Resource is a collection of compute resource.
type Resource struct {
	MilliCPU int64
	Memory   int64
}

var balancePodLabel = map[string]string{"podname": "priority-balanced-memory"}

// track min memory limit based on crio minimum. pods cannot set a limit lower than this
// see: https://github.com/cri-o/cri-o/blob/29805b13e9a43d9d22628553db337ce1c1bec0a8/internal/config/cgmgr/cgmgr.go#L23
// see: https://bugzilla.redhat.com/show_bug.cgi?id=1595256
var crioMinMemLimit = 12 * 1024 * 1024

var podRequestedResource = &v1.ResourceRequirements{
	Limits: v1.ResourceList{
		v1.ResourceMemory: resource.MustParse("100Mi"),
		v1.ResourceCPU:    resource.MustParse("100m"),
	},
	Requests: v1.ResourceList{
		v1.ResourceMemory: resource.MustParse("100Mi"),
		v1.ResourceCPU:    resource.MustParse("100m"),
	},
}

// nodesAreTooUtilized ensures that each node can support 2*crioMinMemLimit
// We check for double because it needs to support at least the cri-o minimum
// plus whatever delta between node usages (which could be up to or at least crioMinMemLimit)
func nodesAreTooUtilized(ctx context.Context, cs clientset.Interface, nodeList *v1.NodeList) bool {
	nodeNameToPodList := podListForEachNode(ctx, cs)
	for _, node := range nodeList.Items {
		_, memFraction, _, memAllocatable := computeCPUMemFraction(node, podRequestedResource, nodeNameToPodList[node.Name])
		if float64(memAllocatable)-(memFraction*float64(memAllocatable)) < float64(2*crioMinMemLimit) {
			return true
		}
	}
	return false
}

// This test suite is used to verifies scheduler priority functions based on the default provider
var _ = SIGDescribe("SchedulerPriorities", framework.WithSerial(), func() {
	var cs clientset.Interface
	var nodeList *v1.NodeList
	var systemPodsNo int
	var ns string
	f := framework.NewDefaultFramework("sched-priority")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.BeforeEach(func(ctx context.Context) {
		cs = f.ClientSet
		ns = f.Namespace.Name
		nodeList = &v1.NodeList{}
		var err error

		e2enode.WaitForTotalHealthy(ctx, cs, time.Minute)
		nodeList, err = e2enode.GetReadySchedulableNodes(ctx, cs)
		if err != nil {
			framework.Logf("Unexpected error occurred: %v", err)
		}
		framework.ExpectNoErrorWithOffset(0, err)

		err = framework.CheckTestingNSDeletedExcept(ctx, cs, ns)
		framework.ExpectNoError(err)
		err = e2epod.WaitForPodsRunningReady(ctx, cs, metav1.NamespaceSystem, systemPodsNo, framework.PodReadyBeforeTimeout)
		framework.ExpectNoError(err)

		// skip if the most utilized node has less than the cri-o minMemLimit available
		// otherwise we will not be able to run the test pod once all nodes are balanced
		if nodesAreTooUtilized(ctx, cs, nodeList) {
			ginkgo.Skip("nodes are too utilized to schedule test pods")
		}
	})

	ginkgo.It("Pod should be scheduled to node that don't match the PodAntiAffinity terms", func(ctx context.Context) {

		e2eskipper.SkipUnlessNodeCountIsAtLeast(2)

		ginkgo.By("Trying to launch a pod with a label to get a node which can launch it.")
		pod := runPausePod(ctx, f, pausePodConfig{
			Name:   "pod-with-label-security-s1",
			Labels: map[string]string{"security": "S1"},
		})
		nodeName := pod.Spec.NodeName

		k := v1.LabelHostname
		ginkgo.By("Verifying the node has a label " + k)
		node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		if _, hasLabel := node.Labels[k]; !hasLabel {
			// If the label is not exists, label all nodes for testing.

			ginkgo.By("Trying to apply a label on the found node.")
			k = "kubernetes.io/e2e-node-topologyKey"
			v := "topologyvalue1"
			e2enode.AddOrUpdateLabelOnNode(cs, nodeName, k, v)
			e2enode.ExpectNodeHasLabel(ctx, cs, nodeName, k, v)
			defer e2enode.RemoveLabelOffNode(cs, nodeName, k)

			ginkgo.By("Trying to apply a label on other nodes.")
			v = "topologyvalue2"
			for _, node := range nodeList.Items {
				if node.Name != nodeName {
					e2enode.AddOrUpdateLabelOnNode(cs, node.Name, k, v)
					e2enode.ExpectNodeHasLabel(ctx, cs, node.Name, k, v)
					defer e2enode.RemoveLabelOffNode(cs, node.Name, k)
				}
			}
		}

		// make the nodes have balanced cpu,mem usage
		err = createBalancedPodForNodes(ctx, f, cs, ns, nodeList.Items, podRequestedResource, 0.6)
		framework.ExpectNoError(err)
		ginkgo.By("Trying to launch the pod with podAntiAffinity.")
		labelPodName := "pod-with-pod-antiaffinity"
		pod = createPausePod(ctx, f, pausePodConfig{
			Resources: podRequestedResource,
			Name:      labelPodName,
			Affinity: &v1.Affinity{
				PodAntiAffinity: &v1.PodAntiAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
						{
							PodAffinityTerm: v1.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "security",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"S1", "value2"},
										},
										{
											Key:      "security",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"S2"},
										}, {
											Key:      "security",
											Operator: metav1.LabelSelectorOpExists,
										},
									},
								},
								TopologyKey: k,
								Namespaces:  []string{ns},
							},
							Weight: 10,
						},
					},
				},
			},
		})
		ginkgo.By("Wait the pod becomes running")
		framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name))
		labelPod, err := cs.CoreV1().Pods(ns).Get(ctx, labelPodName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		ginkgo.By("Verify the pod was scheduled to the expected node.")
		gomega.Expect(labelPod.Spec.NodeName).ToNot(gomega.Equal(nodeName))
	})

	ginkgo.It("Pod should be preferably scheduled to nodes pod can tolerate", func(ctx context.Context) {
		// make the nodes have balanced cpu,mem usage ratio
		err := createBalancedPodForNodes(ctx, f, cs, ns, nodeList.Items, podRequestedResource, 0.5)
		framework.ExpectNoError(err)
		// Apply 10 taints to first node
		nodeName := nodeList.Items[0].Name

		// First, create a set of tolerable taints (+tolerations) for the first node.
		// Generate 10 tolerable taints for the first node (and matching tolerations)
		tolerableTaints := make([]v1.Taint, 0)
		var tolerations []v1.Toleration
		for i := 0; i < 10; i++ {
			testTaint := getRandomTaint()
			tolerableTaints = append(tolerableTaints, testTaint)
			tolerations = append(tolerations, v1.Toleration{Key: testTaint.Key, Value: testTaint.Value, Effect: testTaint.Effect})
		}
		// Generate 10 intolerable taints for each of the remaining nodes
		intolerableTaints := make(map[string][]v1.Taint)
		for i := 1; i < len(nodeList.Items); i++ {
			nodeTaints := make([]v1.Taint, 0)
			for i := 0; i < 10; i++ {
				nodeTaints = append(nodeTaints, getRandomTaint())
			}
			intolerableTaints[nodeList.Items[i].Name] = nodeTaints
		}

		// Apply the tolerable taints generated above to the first node
		ginkgo.By("Trying to apply 10 (tolerable) taints on the first node.")
		// We immediately defer the removal of these taints because addTaintToNode can
		// panic and RemoveTaintsOffNode does not return an error if the taint does not exist.
		ginkgo.DeferCleanup(e2enode.RemoveTaintsOffNode, cs, nodeName, tolerableTaints)
		for _, taint := range tolerableTaints {
			addTaintToNode(ctx, cs, nodeName, taint)
		}
		// Apply the intolerable taints to each of the following nodes
		ginkgo.By("Adding 10 intolerable taints to all other nodes")
		for i := 1; i < len(nodeList.Items); i++ {
			node := nodeList.Items[i]
			ginkgo.DeferCleanup(e2enode.RemoveTaintsOffNode, cs, node.Name, intolerableTaints[node.Name])
			for _, taint := range intolerableTaints[node.Name] {
				addTaintToNode(ctx, cs, node.Name, taint)
			}
		}

		tolerationPodName := "with-tolerations"
		ginkgo.By("Create a pod that tolerates all the taints of the first node.")
		pod := createPausePod(ctx, f, pausePodConfig{
			Name:        tolerationPodName,
			Tolerations: tolerations,
		})
		framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name))

		ginkgo.By("Pod should prefer scheduled to the node that pod can tolerate.")
		tolePod, err := cs.CoreV1().Pods(ns).Get(ctx, tolerationPodName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(tolePod.Spec.NodeName).To(gomega.Equal(nodeName))

		ginkgo.By("Pod should contain the PodScheduled condition and its manager should be kube-scheduler")
		managedFields := tolePod.GetManagedFields()
		PodScheduledConditionFound := false
		for _, entry := range managedFields {
			if strings.Contains(entry.FieldsV1.String(), "PodScheduled") {
				gomega.Expect(entry.Manager).To(gomega.Equal("kube-scheduler"))
				PodScheduledConditionFound = true
			}
		}
		gomega.Expect(PodScheduledConditionFound).To(gomega.BeTrueBecause("PodCondition PodScheduled is not found."))
	})

	ginkgo.Context("PodTopologySpread Scoring", func() {
		var nodeNames []string
		topologyKey := "kubernetes.io/e2e-pts-score"

		ginkgo.BeforeEach(func(ctx context.Context) {
			if len(nodeList.Items) < 2 {
				ginkgo.Skip("At least 2 nodes are required to run the test")
			}
			ginkgo.By("Trying to get 2 available nodes which can run pod")
			nodeNames = Get2NodesThatCanRunPod(ctx, f)
			ginkgo.By(fmt.Sprintf("Apply dedicated topologyKey %v for this test on the 2 nodes.", topologyKey))
			for _, nodeName := range nodeNames {
				e2enode.AddOrUpdateLabelOnNode(cs, nodeName, topologyKey, nodeName)
			}
		})
		ginkgo.AfterEach(func() {
			for _, nodeName := range nodeNames {
				e2enode.RemoveLabelOffNode(cs, nodeName, topologyKey)
			}
		})

		ginkgo.It("validates pod should be preferably scheduled to node which makes the matching pods more evenly distributed", func(ctx context.Context) {
			var nodes []v1.Node
			for _, nodeName := range nodeNames {
				node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
				framework.ExpectNoError(err)
				nodes = append(nodes, *node)
			}

			// Make the nodes have balanced cpu,mem usage.
			err := createBalancedPodForNodes(ctx, f, cs, ns, nodes, podRequestedResource, 0.5)
			framework.ExpectNoError(err)

			replicas := 4
			podLabel := "e2e-pts-score"
			ginkgo.By(fmt.Sprintf("Run a ReplicaSet with %v replicas on node %q", replicas, nodeNames[0]))
			rsConfig := pauseRSConfig{
				Replicas: int32(replicas),
				PodConfig: pausePodConfig{
					Name:         podLabel,
					Namespace:    ns,
					Labels:       map[string]string{podLabel: "foo"},
					NodeSelector: map[string]string{topologyKey: nodeNames[0]},
				},
			}
			runPauseRS(ctx, f, rsConfig)

			// Run a Pod with WhenUnsatisfiable:ScheduleAnyway.
			podCfg := pausePodConfig{
				Name:      "test-pod",
				Namespace: ns,
				// The labels shouldn't match the preceding ReplicaSet, otherwise it will
				// be claimed as orphan of the ReplicaSet.
				Labels: map[string]string{podLabel: "bar"},
				Affinity: &v1.Affinity{
					NodeAffinity: &v1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
							NodeSelectorTerms: []v1.NodeSelectorTerm{
								{
									MatchExpressions: []v1.NodeSelectorRequirement{
										{
											Key:      topologyKey,
											Operator: v1.NodeSelectorOpIn,
											Values:   nodeNames,
										},
									},
								},
							},
						},
					},
				},
				TopologySpreadConstraints: []v1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       topologyKey,
						WhenUnsatisfiable: v1.ScheduleAnyway,
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      podLabel,
									Operator: metav1.LabelSelectorOpExists,
								},
							},
						},
					},
				},
			}
			testPod := runPausePod(ctx, f, podCfg)
			ginkgo.By(fmt.Sprintf("Verifying if the test-pod lands on node %q", nodeNames[1]))
			gomega.Expect(testPod.Spec.NodeName).To(gomega.Equal(nodeNames[1]))
		})
	})
})

// createBalancedPodForNodes creates a pod per node that asks for enough resources to make all nodes have the same mem/cpu usage ratio.
func createBalancedPodForNodes(ctx context.Context, f *framework.Framework, cs clientset.Interface, ns string, nodes []v1.Node, requestedResource *v1.ResourceRequirements, ratio float64) error {
	cleanUp := func(ctx context.Context) {
		// Delete all remaining pods
		err := cs.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set(balancePodLabel)).String(),
		})
		if err != nil {
			framework.Logf("Failed to delete memory balanced pods: %v.", err)
		} else {
			err := wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				podList, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
					LabelSelector: labels.SelectorFromSet(labels.Set(balancePodLabel)).String(),
				})
				if err != nil {
					framework.Logf("Failed to list memory balanced pods: %v.", err)
					return false, nil
				}
				if len(podList.Items) > 0 {
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				framework.Logf("Failed to wait until all memory balanced pods are deleted: %v.", err)
			}
		}
	}
	ginkgo.DeferCleanup(cleanUp)

	// find the max, if the node has the max,use the one, if not,use the ratio parameter
	var maxCPUFraction, maxMemFraction float64 = ratio, ratio
	var cpuFractionMap = make(map[string]float64)
	var memFractionMap = make(map[string]float64)

	// For each node, stores its pods info
	nodeNameToPodList := podListForEachNode(ctx, cs)

	for _, node := range nodes {
		cpuFraction, memFraction, _, _ := computeCPUMemFraction(node, requestedResource, nodeNameToPodList[node.Name])
		cpuFractionMap[node.Name] = cpuFraction
		memFractionMap[node.Name] = memFraction
		if cpuFraction > maxCPUFraction {
			maxCPUFraction = cpuFraction
		}
		if memFraction > maxMemFraction {
			maxMemFraction = memFraction
		}
	}

	errChan := make(chan error, len(nodes))
	var wg sync.WaitGroup

	// we need the max one to keep the same cpu/mem use rate
	ratio = math.Max(maxCPUFraction, maxMemFraction)
	for _, node := range nodes {
		memAllocatable, found := node.Status.Allocatable[v1.ResourceMemory]
		if !found {
			framework.Failf("Node %v: node.Status.Allocatable %v does not contain entry %v", node.Name, node.Status.Allocatable, v1.ResourceMemory)
		}
		memAllocatableVal := memAllocatable.Value()

		cpuAllocatable, found := node.Status.Allocatable[v1.ResourceCPU]
		if !found {
			framework.Failf("Node %v: node.Status.Allocatable %v does not contain entry %v", node.Name, node.Status.Allocatable, v1.ResourceCPU)
		}
		cpuAllocatableMil := cpuAllocatable.MilliValue()

		needCreateResource := v1.ResourceList{}
		cpuFraction := cpuFractionMap[node.Name]
		memFraction := memFractionMap[node.Name]
		needCreateResource[v1.ResourceCPU] = *resource.NewMilliQuantity(int64((ratio-cpuFraction)*float64(cpuAllocatableMil)), resource.DecimalSI)

		// add crioMinMemLimit to ensure that all pods are setting at least that much for a limit, while keeping the same ratios
		needCreateResource[v1.ResourceMemory] = *resource.NewQuantity(int64((ratio-memFraction)*float64(memAllocatableVal)+float64(crioMinMemLimit)), resource.BinarySI)

		podConfig := &pausePodConfig{
			Name:   "",
			Labels: balancePodLabel,
			Resources: &v1.ResourceRequirements{
				Requests: needCreateResource,
			},
			Affinity: &v1.Affinity{
				NodeAffinity: &v1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
						NodeSelectorTerms: []v1.NodeSelectorTerm{
							{
								MatchFields: []v1.NodeSelectorRequirement{
									{Key: "metadata.name", Operator: v1.NodeSelectorOpIn, Values: []string{node.Name}},
								},
							},
						},
					},
				},
			},
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := testutils.StartPods(cs, 1, ns, string(uuid.NewUUID()),
				*initPausePod(f, *podConfig), true, framework.Logf)
			if err != nil {
				errChan <- err
			}
		}()
	}
	wg.Wait()
	close(errChan)
	var errs []error
	for err := range errChan {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.NewAggregate(errs)
	}

	nodeNameToPodList = podListForEachNode(ctx, cs)
	for _, node := range nodes {
		ginkgo.By("Compute Cpu, Mem Fraction after create balanced pods.")
		computeCPUMemFraction(node, requestedResource, nodeNameToPodList[node.Name])
	}

	return nil
}

func podListForEachNode(ctx context.Context, cs clientset.Interface) map[string][]*v1.Pod {
	nodeNameToPodList := make(map[string][]*v1.Pod)
	allPods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		framework.Failf("Expect error of invalid, got : %v", err)
	}
	for i, pod := range allPods.Items {
		nodeName := pod.Spec.NodeName
		nodeNameToPodList[nodeName] = append(nodeNameToPodList[nodeName], &allPods.Items[i])
	}
	return nodeNameToPodList
}

func computeCPUMemFraction(node v1.Node, resource *v1.ResourceRequirements, pods []*v1.Pod) (float64, float64, int64, int64) {
	framework.Logf("ComputeCPUMemFraction for node: %v", node.Name)
	totalRequestedCPUResource := resource.Requests.Cpu().MilliValue()
	totalRequestedMemResource := resource.Requests.Memory().Value()

	for _, pod := range pods {
		framework.Logf("Pod for on the node: %v, Cpu: %v, Mem: %v", pod.Name, getNonZeroRequests(pod).MilliCPU, getNonZeroRequests(pod).Memory)
		// Ignore best effort pods while computing fractions as they won't be taken in account by scheduler.
		if v1qos.GetPodQOS(pod) == v1.PodQOSBestEffort {
			continue
		}
		totalRequestedCPUResource += getNonZeroRequests(pod).MilliCPU
		totalRequestedMemResource += getNonZeroRequests(pod).Memory
	}

	cpuAllocatable, found := node.Status.Allocatable[v1.ResourceCPU]
	if !found {
		framework.Failf("Node %v: node.Status.Allocatable %v does not contain entry %v", node.Name, node.Status.Allocatable, v1.ResourceCPU)
	}
	cpuAllocatableMil := cpuAllocatable.MilliValue()

	floatOne := float64(1)
	cpuFraction := float64(totalRequestedCPUResource) / float64(cpuAllocatableMil)
	if cpuFraction > floatOne {
		cpuFraction = floatOne
	}
	memAllocatable, found := node.Status.Allocatable[v1.ResourceMemory]
	if !found {
		framework.Failf("Node %v: node.Status.Allocatable %v does not contain entry %v", node.Name, node.Status.Allocatable, v1.ResourceMemory)
	}
	memAllocatableVal := memAllocatable.Value()
	memFraction := float64(totalRequestedMemResource) / float64(memAllocatableVal)
	if memFraction > floatOne {
		memFraction = floatOne
	}

	framework.Logf("Node: %v, totalRequestedCPUResource: %v, cpuAllocatableMil: %v, cpuFraction: %v", node.Name, totalRequestedCPUResource, cpuAllocatableMil, cpuFraction)
	framework.Logf("Node: %v, totalRequestedMemResource: %v, memAllocatableVal: %v, memFraction: %v", node.Name, totalRequestedMemResource, memAllocatableVal, memFraction)

	return cpuFraction, memFraction, cpuAllocatableMil, memAllocatableVal
}

func getNonZeroRequests(pod *v1.Pod) Resource {
	result := Resource{}
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		cpu := getNonZeroRequestForResource(v1.ResourceCPU, &container.Resources.Requests)
		memory := getNonZeroRequestForResource(v1.ResourceMemory, &container.Resources.Requests)
		result.MilliCPU += cpu.MilliValue()
		result.Memory += memory.Value()
	}
	return result
}

func getRandomTaint() v1.Taint {
	return v1.Taint{
		Key:    fmt.Sprintf("kubernetes.io/e2e-scheduling-priorities-%s", string(uuid.NewUUID()[:23])),
		Value:  fmt.Sprintf("testing-taint-value-%s", string(uuid.NewUUID())),
		Effect: v1.TaintEffectPreferNoSchedule,
	}
}

func addTaintToNode(ctx context.Context, cs clientset.Interface, nodeName string, testTaint v1.Taint) {
	e2enode.AddOrUpdateTaintOnNode(ctx, cs, nodeName, testTaint)
	e2enode.ExpectNodeHasTaint(ctx, cs, nodeName, &testTaint)
}

// getNonZeroRequestForResource returns the requested values,
// if the resource has undefined request for CPU or memory, it returns a default value.
func getNonZeroRequestForResource(resourceName v1.ResourceName, requests *v1.ResourceList) resource.Quantity {
	if requests == nil {
		return resource.Quantity{}
	}
	switch resourceName {
	case v1.ResourceCPU:
		// Override if un-set, but not if explicitly set to zero
		if _, found := (*requests)[v1.ResourceCPU]; !found {
			return *resource.NewMilliQuantity(schedutil.DefaultMilliCPURequest, resource.DecimalSI)
		}
		return requests.Cpu().DeepCopy()
	case v1.ResourceMemory:
		// Override if un-set, but not if explicitly set to zero
		if _, found := (*requests)[v1.ResourceMemory]; !found {
			return *resource.NewQuantity(schedutil.DefaultMemoryRequest, resource.DecimalSI)
		}
		return requests.Memory().DeepCopy()
	default:
		quantity, found := (*requests)[resourceName]
		if !found {
			return resource.Quantity{}
		}
		return quantity.DeepCopy()
	}
}
