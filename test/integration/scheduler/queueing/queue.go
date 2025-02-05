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

package queueing

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/component-helpers/storage/volume"
	configv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler"
	configtesting "k8s.io/kubernetes/pkg/scheduler/apis/config/testing"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	"k8s.io/utils/ptr"
)

type CoreResourceEnqueueTestCase struct {
	Name string
	// InitialNodes is the list of Nodes to be created at first.
	InitialNodes []*v1.Node
	// InitialPods is the list of Pods to be created at first if it's not empty.
	// Note that the scheduler won't schedule those Pods,
	// meaning, those Pods should be already scheduled basically; they should have .spec.nodename.
	InitialPods []*v1.Pod
	// InitialPVCs are the list of PersistentVolumeClaims to be created at first.
	// Note that PVs are automatically created following PVCs.
	// Also, the namespace of pvcs is automatically filled in.
	InitialPVCs []*v1.PersistentVolumeClaim
	// InitialPVs are the list of PersistentVolume to be created at first.
	InitialPVs []*v1.PersistentVolume
	// InitialStorageClasses are the list of StorageClass to be created at first.
	InitialStorageClasses []*storagev1.StorageClass
	// InitialCSINodes are the list of CSINode to be created at first.
	InitialCSINodes []*storagev1.CSINode
	// InitialCSIDrivers are the list of CSIDriver to be created at first.
	InitialCSIDrivers []*storagev1.CSIDriver
	// InitialStorageCapacities are the list of CSIStorageCapacity to be created at first.
	InitialStorageCapacities []*storagev1.CSIStorageCapacity
	// Pods are the list of Pods to be created.
	// All of them are expected to be unschedulable at first.
	Pods []*v1.Pod
	// TriggerFn is the function that triggers the event to move Pods.
	// It returns the map keyed with ClusterEvents to be triggered by this function,
	// and valued with the number of triggering of the event.
	TriggerFn func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error)
	// WantRequeuedPods is the map of Pods that are expected to be requeued after triggerFn.
	WantRequeuedPods sets.Set[string]
	// EnableSchedulingQueueHint indicates which feature gate value(s) the test case should run with.
	// By default, it's {true, false}
	EnableSchedulingQueueHint sets.Set[bool]
	// EnablePlugins is a list of plugins to enable.
	// PrioritySort and DefaultPreemption are enabled by default because they are required by the framework.
	// If empty, all plugins are enabled.
	EnablePlugins []string
}

var (
	// These resources are unexported from the framework intentionally
	// because they're only used internally for the metric labels/logging.
	// We need to declare them here to use them in the test
	// because this test is using the metric labels.
	assignedPod      framework.EventResource = "AssignedPod"
	unschedulablePod framework.EventResource = "UnschedulablePod"
)

// We define all the test cases here in the one place,
// and those will be run either in ./former or ./queueinghint tests, depending on EnableSchedulingQueueHint.
// We needed to do this because running all these test cases resulted in the timeout.
var CoreResourceEnqueueTestCases = []*CoreResourceEnqueueTestCase{
	{
		Name:         "Pod without a required toleration to a node isn't requeued to activeQ",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Taints([]v1.Taint{{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule}}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1 doesn't have the required toleration and will be rejected by the TaintToleration plugin.
			//   (TaintToleration plugin is evaluated before NodeResourcesFit plugin.)
			// - Pod2 has the required toleration, but requests a large amount of CPU - will be rejected by the NodeResourcesFit plugin.
			st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Obj(),
			st.MakePod().Name("pod2").Toleration(v1.TaintNodeNotReady).Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeChange event by increasing CPU capacity.
			// It makes Pod2 schedulable.
			// Pod1 is not requeued because the Node is still unready and it doesn't have the required toleration.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().UpdateStatus(testCtx.Ctx, st.MakeNode().Name("fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Taints([]v1.Taint{{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule}}).Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeAllocatable}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod2"),
	},
	{
		Name:         "Pod rejected by the PodAffinity plugin is requeued when a new Node is created and turned to ready",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Label("anti", "anti").Name("pod1").PodAntiAffinityExists("anti", "node", st.PodAntiAffinityWithRequiredReq).Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod2 will be rejected by the PodAffinity plugin.
			st.MakePod().Label("anti", "anti").Name("pod2").PodAntiAffinityExists("anti", "node", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeCreated event.
			// Note that this Node has a un-ready taint and pod2 should be requeued ideally because unschedulable plugins registered for pod2 is PodAffinity.
			// However, due to preCheck, it's not requeueing pod2 to activeQ.
			// It'll be fixed by the removal of preCheck in the future.
			// https://github.com/kubernetes/kubernetes/issues/110175
			node := st.MakeNode().Name("fake-node2").Label("node", "fake-node2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Taints([]v1.Taint{{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule}}).Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create a new node: %w", err)
			}

			// As a mitigation of an issue described above, all plugins subscribing Node/Add event register UpdateNodeTaint too.
			// So, this removal of taint moves pod2 to activeQ.
			node.Spec.Taints = nil
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, node, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to remove taints off the node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{
				{Resource: framework.Node, ActionType: framework.Add}:             1,
				{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod2"),
	},
	{
		Name:         "Pod rejected by the NodeAffinity plugin is requeued when a Node's label is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Label("group", "a").Obj()},
		Pods: []*v1.Pod{
			// - Pod1 will be rejected by the NodeAffinity plugin.
			st.MakePod().Name("pod1").NodeAffinityIn("group", []string{"b"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
			// - Pod2 will be rejected by the NodeAffinity plugin.
			st.MakePod().Name("pod2").NodeAffinityIn("group", []string{"c"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event to change label.
			// It causes pod1 to be requeued.
			// It causes pod2 not to be requeued.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, st.MakeNode().Name("fake-node1").Label("group", "b").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeLabel}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name: "Pod rejected by the NodeAffinity plugin is not requeued when an updated Node haven't changed the 'match' verdict",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("node1").Label("group", "a").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj(),
			st.MakeNode().Name("node2").Label("group", "b").Obj()},
		Pods: []*v1.Pod{
			// - The initial pod would be accepted by the NodeAffinity plugin for node1, but will be blocked by the NodeResources plugin.
			// - The pod will be blocked by the NodeAffinity plugin for node2, therefore we know NodeAffinity will be queried for qhint for both testing nodes.
			st.MakePod().Name("pod1").NodeAffinityIn("group", []string{"a"}, st.NodeSelectorTypeMatchExpressions).Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event to add new label.
			// It won't cause pod to be requeued, because there was a match already before the update, meaning this plugin wasn't blocking the scheduling.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, st.MakeNode().Name("node1").Label("group", "a").Label("node", "fake-node").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeLabel}: 1}, nil
		},
		WantRequeuedPods:          sets.Set[string]{},
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the NodeAffinity plugin is requeued when a Node is added",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Label("group", "a").Obj()},
		Pods: []*v1.Pod{
			// - Pod1 will be rejected by the NodeAffinity plugin.
			st.MakePod().Name("pod1").NodeAffinityIn("group", []string{"b"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
			// - Pod2 will be rejected by the NodeAffinity plugin.
			st.MakePod().Name("pod2").NodeAffinityIn("group", []string{"c"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeAdd event with the awaited label.
			// It causes pod1 to be requeued.
			// It causes pod2 not to be requeued.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, st.MakeNode().Name("fake-node2").Label("group", "b").Obj(), metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name:         "Pod updated with toleration requeued to activeQ",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Taints([]v1.Taint{{Key: "taint-key", Effect: v1.TaintEffectNoSchedule}}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1 doesn't have the required toleration and will be rejected by the TaintToleration plugin.
			st.MakePod().Name("pod1").Container("image").Obj(),
			st.MakePod().Name("pod2").Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a PodUpdate event by adding a toleration to Pod1.
			// It makes Pod1 schedulable. Pod2 is not requeued because of not having toleration.
			if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Update(testCtx.Ctx, st.MakePod().Name("pod1").Container("image").Toleration("taint-key").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the pod: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: unschedulablePod, ActionType: framework.UpdatePodToleration}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name:         "Pod rejected by the TaintToleration plugin is requeued when the Node's taint is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Taints([]v1.Taint{{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule}}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1, pod2 and pod3 don't have the required toleration and will be rejected by the TaintToleration plugin.
			st.MakePod().Name("pod1").Toleration("taint-key").Container("image").Obj(),
			st.MakePod().Name("pod2").Toleration("taint-key2").Container("image").Obj(),
			st.MakePod().Name("pod3").Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event that changes the existing taint to a taint that matches the toleration that pod1 has.
			// It makes Pod1 schedulable. Pod2 and pod3 are not requeued because of not having the corresponding toleration.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, st.MakeNode().Name("fake-node").Taints([]v1.Taint{{Key: "taint-key", Effect: v1.TaintEffectNoSchedule}}).Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the Node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the TaintToleration plugin is requeued when a Node that has the correspoding taint is added",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Taints([]v1.Taint{{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule}}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1 and Pod2 don't have the required toleration and will be rejected by the TaintToleration plugin.
			st.MakePod().Name("pod1").Toleration("taint-key").Container("image").Obj(),
			st.MakePod().Name("pod2").Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeCreated event with the taint.
			// It makes Pod1 schedulable. Pod2 is not requeued because of not having toleration.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, st.MakeNode().Name("fake-node2").Taints([]v1.Taint{{Key: "taint-key", Effect: v1.TaintEffectNoSchedule}}).Obj(), metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create the Node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name:         "Pod rejected by the NodeResourcesFit plugin is requeued when the Pod is updated to scale down",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1 requests a large amount of CPU and will be rejected by the NodeResourcesFit plugin.
			st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a PodUpdate event by reducing cpu requested by pod1.
			// It makes Pod1 schedulable.
			if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).UpdateResize(testCtx.Ctx, "pod1", st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Container("image").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to resize the pod: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: unschedulablePod, ActionType: framework.UpdatePodScaleDown}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name:         "Pod rejected by the NodeResourcesFit plugin is requeued when a Pod is deleted",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod2 request will be rejected because there are not enough free resources on the Node
			st.MakePod().Name("pod2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an assigned Pod1 delete event.
			// It makes Pod2 schedulable.
			if err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Delete(testCtx.Ctx, "pod1", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
				return nil, fmt.Errorf("failed to delete pod1: %w", err)
			}
			return map[framework.ClusterEvent]uint64{framework.EventAssignedPodDelete: 1}, nil
		},
		WantRequeuedPods: sets.New("pod2"),
	},
	{
		Name:         "Pod rejected by the NodeResourcesFit plugin is requeued when a Node is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1 requests a large amount of CPU and will be rejected by the NodeResourcesFit plugin.
			st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Obj(),
			// - Pod2 requests a large amount of CPU and will be rejected by the NodeResourcesFit plugin.
			st.MakePod().Name("pod2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "5"}).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeCreated event.
			// It makes Pod1 schedulable. Pod2 is not requeued because of having too high requests.
			node := st.MakeNode().Name("fake-node2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create a new node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name:         "Pod rejected by the NodeResourcesFit plugin is requeued when a Node is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1 requests a large amount of CPU and will be rejected by the NodeResourcesFit plugin.
			st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Obj(),
			// - Pod2 requests a large amount of CPU and will be rejected by the NodeResourcesFit plugin.
			st.MakePod().Name("pod2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "5"}).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event that increases the CPU capacity of fake-node.
			// It makes Pod1 schedulable. Pod2 is not requeued because of having too high requests.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().UpdateStatus(testCtx.Ctx, st.MakeNode().Name("fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update fake-node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeAllocatable}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name: "Pod rejected by the NodeResourcesFit plugin isn't requeued when a Node is updated without increase in the requested resources",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(),
			st.MakeNode().Name("fake-node2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Label("group", "b").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod1 requests available amount of CPU (in fake-node1), but will be rejected by NodeAffinity plugin. Note that the NodeResourceFit plugin will register for QHints because it rejected fake-node2.
			st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).NodeAffinityIn("group", []string{"b"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event that increases unrealted (not requested) memory capacity of fake-node1, which should not requeue Pod1.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().UpdateStatus(testCtx.Ctx, st.MakeNode().Name("fake-node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourceMemory: "4000"}).Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update fake-node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeAllocatable}: 1}, nil
		},
		WantRequeuedPods:          sets.Set[string]{},
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pod rejected by the NodeResourcesFit plugin is requeued when a Node is updated with increase in the requested resources",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(),
			st.MakeNode().Name("fake-node2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Label("group", "b").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod1 requests available amount of CPU (in fake-node1), but will be rejected by NodeAffinity plugin. Note that the NodeResourceFit plugin will register for QHints because it rejected fake-node2.
			st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).NodeAffinityIn("group", []string{"b"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event that increases the requested CPU capacity of fake-node1, which should requeue Pod1.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().UpdateStatus(testCtx.Ctx, st.MakeNode().Name("fake-node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "5"}).Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update fake-node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeAllocatable}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pod rejected by the NodeResourcesFit plugin is requeued when a Node is updated with increase in the allowed pods number",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "2"}).Obj(),
			st.MakeNode().Name("fake-node2").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "1"}).Label("group", "b").Obj(),
		},
		InitialPods: []*v1.Pod{
			// - Pod1 will be scheduled on fake-node2 because of the affinity label.
			st.MakePod().Name("pod1").NodeAffinityIn("group", []string{"b"}, st.NodeSelectorTypeMatchExpressions).Container("image").Node("fake-node2").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod2 is unschedulable because Pod1 saturated ResourcePods limit in fake-node2. Note that the NodeResourceFit plugin will register for QHints because it rejected fake-node2.
			st.MakePod().Name("pod2").NodeAffinityIn("group", []string{"b"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event that increases the allowed Pods number of fake-node1, which should requeue Pod2.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().UpdateStatus(testCtx.Ctx, st.MakeNode().Name("fake-node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "3"}).Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update fake-node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeAllocatable}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Updating pod label doesn't retry scheduling if the Pod was rejected by TaintToleration",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Taints([]v1.Taint{{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule}}).Obj()},
		Pods: []*v1.Pod{
			// - Pod1 doesn't have the required toleration and will be rejected by the TaintToleration plugin.
			st.MakePod().Name("pod1").Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Update(testCtx.Ctx, st.MakePod().Name("pod1").Label("key", "val").Container("image").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the pod: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: unschedulablePod, ActionType: framework.UpdatePodLabel}: 1}, nil
		},
		WantRequeuedPods: sets.Set[string]{},
		// This behaviour is only true when enabling QHint
		// because QHint of TaintToleration would decide to ignore a Pod update.
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		// The test case makes sure that PreFilter plugins returning PreFilterResult are also inserted into pInfo.UnschedulablePlugins
		// meaning, they're taken into consideration during requeuing flow of the queue.
		// https://github.com/kubernetes/kubernetes/issues/122018
		Name: "Pod rejected by the PreFilter of NodeAffinity plugin and Filter of NodeResourcesFit is requeued based on both plugins",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node").Label("node", "fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj(),
			st.MakeNode().Name("fake-node2").Label("node", "fake-node2").Label("zone", "zone1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod1 will be rejected by the NodeAffinity plugin and NodeResourcesFit plugin.
			st.MakePod().Label("unscheduled", "plugins").Name("pod1").NodeAffinityIn("metadata.name", []string{"fake-node"}, st.NodeSelectorTypeMatchFields).Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Because of preCheck, we cannot determine which unschedulable plugins are registered for pod1.
			// So, not the ideal way as the integration test though,
			// here we check the unschedulable plugins by directly using the SchedulingQueue function for now.
			// We can change here to assess unschedPlugin by triggering cluster events like other test cases
			// after QHint is graduated and preCheck is removed.
			pInfo, ok := testCtx.Scheduler.SchedulingQueue.GetPod("pod1", testCtx.NS.Name)
			if !ok || pInfo == nil {
				return nil, fmt.Errorf("pod1 is not found in the scheduling queue")
			}

			if pInfo.Pod.Name != "pod1" {
				return nil, fmt.Errorf("unexpected pod info: %#v", pInfo)
			}

			if pInfo.UnschedulablePlugins.Difference(sets.New(names.NodeAffinity, names.NodeResourcesFit)).Len() != 0 {
				return nil, fmt.Errorf("unexpected unschedulable plugin(s) is registered in pod1: %v", pInfo.UnschedulablePlugins.UnsortedList())
			}

			return nil, nil
		},
	},
	{
		Name:         "Pod rejected by the PodAffinity plugin is requeued when deleting the existed pod's label that matches the podAntiAffinity",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("anti1", "anti1").Label("anti2", "anti2").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod2 and pod3 will be rejected by the PodAffinity plugin.
			st.MakePod().Name("pod2").Label("anti1", "anti1").PodAntiAffinityExists("anti1", "node", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod3").Label("anti2", "anti2").PodAntiAffinityExists("anti2", "node", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
		},

		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Delete the pod's label 'anti1' which will make it match pod2's antiAffinity.
			if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Update(testCtx.Ctx, st.MakePod().Name("pod1").Label("anti2", "anti2").Container("image").Node("fake-node").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update pod1: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: assignedPod, ActionType: framework.UpdatePodLabel}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the PodAffinity plugin is requeued when updating the existed pod's label to make it match the pod's podAffinity",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod2 and pod3 will be rejected by the PodAffinity plugin.
			st.MakePod().Name("pod2").PodAffinityExists("aaa", "node", st.PodAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod3").PodAffinityExists("bbb", "node", st.PodAffinityWithRequiredReq).Container("image").Obj(),
		},

		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Add label to the pod which will make it match pod2's podAffinity.
			if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Update(testCtx.Ctx, st.MakePod().Name("pod1").Label("aaa", "bbb").Container("image").Node("fake-node").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update pod1: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: assignedPod, ActionType: framework.UpdatePodLabel}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	//{
	//	Name:         "Pod rejected by the PodAffinity plugin is requeued when creating pod which matches podAffinity",
	//	InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
	//	InitialPods:  []*v1.Pod{},
	//	Pods: []*v1.Pod{
	//		st.MakePod().Name("pod1").PodAffinityIn("service", "region", []string{"securityscan", "value2"}, st.PodAffinityWithRequiredReq).Container("image").Obj(),
	//	},
	//	TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
	//		// Create a pod to make it match the pod1's podAffinity
	//		pod := st.MakePod().Name("pod2").Label("service", "securityscan").Container("image").Obj()
	//		if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Create(testCtx.Ctx, pod, metav1.CreateOptions{}); err != nil {
	//			return nil, fmt.Errorf("failed to create pod2: %w", err)
	//		}
	//		return map[framework.ClusterEvent]uint64{framework.EventAssignedPodAdd: 1}, nil
	//	},
	//	WantRequeuedPods:          sets.New("pod1"),
	//	EnableSchedulingQueueHint: sets.New(true),
	//},
	{
		Name:         "Pod rejected by the PodAffinity plugin is requeued when deleting pod which matches podAntiAffinity",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("service", "securityscan").Container("image").Node("fake-node").Obj(),
			st.MakePod().Name("pod2").Label("region", "region-1").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod3").PodAntiAffinityIn("service", "node", []string{"securityscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod4").PodAntiAffinityIn("region", "node", []string{"region-1", "value2"}, st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Delete pod to make it match the pod3's podAntiAffinity
			if err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Delete(testCtx.Ctx, "pod1", metav1.DeleteOptions{}); err != nil {
				return nil, fmt.Errorf("failed to delete pod1: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Pod, ActionType: framework.Delete}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod3"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the interPodAffinity plugin is requeued when creating the node with the topologyKey label of the pod affinity",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Label("aaa", "fake-node").Obj()},
		Pods: []*v1.Pod{
			// - pod1 and pod2 will be rejected by the PodAffinity plugin.
			st.MakePod().Name("pod1").PodAffinityExists("bbb", "zone", st.PodAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod2").PodAffinityExists("ccc", "region", st.PodAffinityWithRequiredReq).Container("image").Obj(),
		},

		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Create the node which will make it match pod1's podAffinity.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, st.MakeNode().Name("fake-node2").Label("zone", "zone1").Obj(), metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create fake-node2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pod rejected by the interPodAffinity plugin is requeued when updating the node with the topologyKey label of the pod affinity",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node").Label("region", "region-1").Obj(),
			st.MakeNode().Name("fake-node2").Label("region", "region-2").Label("group", "b").Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("affinity1", "affinity1").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - pod2, pod3, pod4 will be rejected by the PodAffinity plugin.
			st.MakePod().Name("pod2").Label("affinity1", "affinity1").PodAffinityExists("affinity1", "node", st.PodAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod3").Label("affinity1", "affinity1").PodAffinityExists("affinity1", "other", st.PodAffinityWithRequiredReq).Container("image").Obj(),
			// This Pod doesn't have any label so that this PodAffinity doesn't match this Pod itself.
			st.MakePod().Name("pod4").PodAffinityExists("affinity1", "region", st.PodAffinityWithRequiredReq).NodeAffinityIn("group", []string{"b"}, st.NodeSelectorTypeMatchExpressions).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Add "node" label to the node which may make pod2 schedulable.
			// Update "region" label to the node which may make pod4 schedulable.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, st.MakeNode().Name("fake-node").Label("node", "fake-node").Label("region", "region-2").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update fake-node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeLabel}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2", "pod4"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the interPodAffinity plugin is requeued when updating the node with the topologyKey label of the pod anti affinity",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label("service", "service-a").Label("other", "fake-node").Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("anti1", "anti1").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - pod2, pod3, pod4 will be rejected by the PodAffinity plugin.
			st.MakePod().Name("pod2").Label("anti1", "anti1").PodAntiAffinityExists("anti1", "node", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod3").Label("anti1", "anti1").PodAntiAffinityExists("anti1", "service", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("anti1", "anti1").PodAntiAffinityExists("anti1", "other", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Remove "node" label from the node which will make it not match pod2's podAntiAffinity.
			// Change "service" label from service-a to service-b which may pod3 schedulable.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, st.MakeNode().Name("fake-node").Label("service", "service-b").Label("region", "fake-node").Label("other", "fake-node").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update fake-node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeLabel}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2", "pod3"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the interPodAffinity plugin is requeued when creating the node with the topologyKey label of the pod anti affinity",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Label("zone", "fake-node").Label("node", "fake-node").Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("anti1", "anti1").Container("image").Node("fake-node1").Obj(),
		},
		Pods: []*v1.Pod{
			// - pod2, pod3 will be rejected by the PodAntiAffinity plugin.
			st.MakePod().Name("pod2").PodAntiAffinityExists("anti1", "zone", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
			st.MakePod().Name("pod3").PodAntiAffinityExists("anti1", "node", st.PodAntiAffinityWithRequiredReq).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Create the node which will make pod2, pod3 be schedulable.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, st.MakeNode().Name("fake-node2").Label("zone", "fake-node").Obj(), metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create fake-node2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2", "pod3"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with hostport by the NodePorts plugin is requeued when pod with common hostport is deleted",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").ContainerPort([]v1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}).Node("fake-node").Obj(),
			st.MakePod().Name("pod2").Container("image").ContainerPort([]v1.ContainerPort{{ContainerPort: 8080, HostPort: 8081}}).Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod3").Container("image").ContainerPort([]v1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}).Obj(),
			st.MakePod().Name("pod4").Container("image").ContainerPort([]v1.ContainerPort{{ContainerPort: 8080, HostPort: 8081}}).Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an assigned Pod delete event.
			// Because Pod1 and Pod3 have common port, deleting Pod1 makes Pod3 schedulable.
			// By setting GracePeriodSeconds to 0, allowing Pod3 to be requeued immediately after Pod1 is deleted.
			if err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Delete(testCtx.Ctx, "pod1", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
				return nil, fmt.Errorf("failed to delete Pod: %w", err)
			}
			return map[framework.ClusterEvent]uint64{framework.EventAssignedPodDelete: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod3"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with hostport by the NodePorts plugin is requeued when new node is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").ContainerPort([]v1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}).Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").ContainerPort([]v1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}).Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeCreated event.
			// It makes Pod2 schedulable.
			node := st.MakeNode().Name("fake-node2").Label("node", "fake-node2").Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create a new node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the NodeUnschedulable plugin is requeued when the node is turned to ready",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Unschedulable(true).Obj()},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeUpdate event to change the Node to ready.
			// It makes Pod1 schedulable.
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, st.MakeNode().Name("fake-node").Unschedulable(false).Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name:         "Pod rejected by the NodeUnschedulable plugin is requeued when a new node is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Unschedulable(true).Obj()},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeCreated event.
			// It makes Pod1 schedulable.
			node := st.MakeNode().Name("fake-node2").Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create a new node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod1"),
	},
	{
		Name:         "Pod rejected by the NodeUnschedulable plugin isn't requeued when another unschedulable node is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node1").Unschedulable(true).Obj()},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger a NodeCreated event, but with unschedulable node, which wouldn't make Pod1 schedulable.
			node := st.MakeNode().Name("fake-node2").Unschedulable(true).Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create a new node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods: sets.Set[string]{},
		// This test case is valid only when QHint is enabled
		// because QHint filters out a node creation made in triggerFn.
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pods with PodTopologySpread should be requeued when a Pod with matching label is scheduled",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").Container("image").Node("fake-node").Obj(),
			st.MakePod().Name("pod2").Label("key2", "val").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod3 and Pod4 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("key2", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key2").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an assigned Pod add event.
			// It should requeue pod3 only because this pod only has key1 label that pod3's topologyspread checks, and doesn't have key2 label that pod4's one does.
			pod := st.MakePod().Name("pod5").Label("key1", "val").Node("fake-node").Container("image").Obj()
			if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Create(testCtx.Ctx, pod, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create Pod %q: %w", pod.Name, err)
			}

			return map[framework.ClusterEvent]uint64{framework.EventAssignedPodAdd: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod3"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pods with PodTopologySpread should be requeued when a scheduled Pod label is updated to match the selector",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").Container("image").Node("fake-node").Obj(),
			st.MakePod().Name("pod2").Label("key2", "val").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod3 and Pod4 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("key2", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key2").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an assigned Pod update event.
			// It should requeue pod3 only because this updated pod had key1 label,
			// and it's related only to the label selector that pod3's topologyspread has.
			if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Update(testCtx.Ctx, st.MakePod().Name("pod1").Label("key3", "val").Container("image").Node("fake-node").Obj(), metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update the pod: %w", err)
			}
			return map[framework.ClusterEvent]uint64{framework.EventAssignedPodUpdate: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod3"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pods with PodTopologySpread should be requeued when a scheduled Pod with matching label is deleted",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").Container("image").Node("fake-node").Obj(),
			st.MakePod().Name("pod2").Label("key2", "val").Container("image").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod3 and Pod4 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(2)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("key2", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key2").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an assigned Pod delete event.
			// It should requeue pod3 only because this pod only has key1 label that pod3's topologyspread checks, and doesn't have key2 label that pod4's one does.
			if err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Delete(testCtx.Ctx, "pod1", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
				return nil, fmt.Errorf("failed to delete Pod: %w", err)
			}
			return map[framework.ClusterEvent]uint64{framework.EventAssignedPodDelete: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod3"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pods with PodTopologySpread should be requeued when a Node with topology label is created",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Label("node", "fake-node").Obj(),
			st.MakeNode().Name("fake-node2").Label("zone", "fake-zone").Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").Container("image").Node("fake-node1").Obj(),
			st.MakePod().Name("pod2").Label("key1", "val").Container("image").Node("fake-node2").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod3 and Pod4 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(2)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(2)), nil, nil, nil).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an Node add event.
			// It should requeue pod3 only because this node only has node label, and doesn't have zone label that pod4's topologyspread requires.
			node := st.MakeNode().Name("fake-node3").Label("node", "fake-node").Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create a new node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod3"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pods with PodTopologySpread should be requeued when a Node is updated to have the topology label",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Label("node", "fake-node").Label("region", "fake-node").Label("service", "service-a").Obj(),
			st.MakeNode().Name("fake-node2").Label("node", "fake-node").Label("region", "fake-node").Label("service", "service-a").Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node1").Obj(),
			st.MakePod().Name("pod2").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node2").Obj(),
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "region", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node2").Obj(),
			st.MakePod().Name("pod4").Label("key1", "val").SpreadConstraint(1, "service", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node2").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod5, Pod6, Pod7, Pod8, Pod9 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod5").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod6").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod7").Label("key1", "val").SpreadConstraint(1, "region", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod8").Label("key1", "val").SpreadConstraint(1, "other", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod9").Label("key1", "val").SpreadConstraint(1, "service", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an Node update event.
			// It should requeue pod5 because it deletes the "node" label from fake-node2.
			// It should requeue pod6 because the update adds the "zone" label to fake-node2.
			// It should not requeue pod7 because the update does not change the value of "region" label.
			// It should not requeue pod8 because the update does not add the "other" label.
			// It should requeue pod9 because the update changes the value of "service" label.
			node := st.MakeNode().Name("fake-node2").Label("zone", "fake-node").Label("region", "fake-node").Label("service", "service-b").Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, node, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeLabel}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod5", "pod6", "pod9"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pods with PodTopologySpread should be requeued when a Node with a topology label is deleted (QHint: enabled)",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Label("node", "fake-node").Obj(),
			st.MakeNode().Name("fake-node2").Label("zone", "fake-node").Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node1").Obj(),
			st.MakePod().Name("pod2").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node2").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod3 and Pod4 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an NodeTaint delete event.
			// It should requeue pod4 only because this node only has zone label, and doesn't have node label that pod3 requires.
			if err := testCtx.ClientSet.CoreV1().Nodes().Delete(testCtx.Ctx, "fake-node2", metav1.DeleteOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Delete}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod4"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pods with PodTopologySpread should be requeued when a Node with a topology label is deleted (QHint: disabled)",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Label("node", "fake-node").Obj(),
			st.MakeNode().Name("fake-node2").Label("zone", "fake-node").Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node1").Obj(),
			st.MakePod().Name("pod2").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node2").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod3 and Pod4 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an NodeTaint delete event.
			// It should requeue both pod3 and pod4 only because PodTopologySpread subscribes to Node/delete events.
			if err := testCtx.ClientSet.CoreV1().Nodes().Delete(testCtx.Ctx, "fake-node2", metav1.DeleteOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Delete}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod3", "pod4"),
		EnableSchedulingQueueHint: sets.New(false),
	},
	{
		Name: "Pods with PodTopologySpread should be requeued when a NodeTaint of a Node with a topology label has been updated",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node1").Label("node", "fake-node").Obj(),
			st.MakeNode().Name("fake-node2").Label("zone", "fake-node").Obj(),
			st.MakeNode().Name("fake-node3").Label("zone", "fake-node").Taints([]v1.Taint{{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule}}).Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node1").Obj(),
			st.MakePod().Name("pod2").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), nil, nil, nil, nil).Container("image").Node("fake-node2").Obj(),
		},
		Pods: []*v1.Pod{
			// - Pod3 and Pod4 will be rejected by the PodTopologySpread plugin.
			st.MakePod().Name("pod3").Label("key1", "val").SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Obj(),
			st.MakePod().Name("pod4").Label("key1", "val").SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("key1").Obj(), ptr.To(int32(3)), nil, nil, nil).Container("image").Toleration("aaa").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// Trigger an NodeTaint update event.
			// It should requeue pod4 only because this node only has zone label, and doesn't have node label that pod3 requires.
			node := st.MakeNode().Name("fake-node3").Label("zone", "fake-node").Taints([]v1.Taint{{Key: "aaa", Value: "bbb", Effect: v1.TaintEffectNoSchedule}}).Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, node, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}: 1}, nil
		},
		WantRequeuedPods: sets.New("pod4"),
	},
	{
		Name:         "Pod rejected with node by the VolumeZone plugin is requeued when the PV is added",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west1-a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				Labels(map[string]string{v1.LabelTopologyZone: "us-west1-a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pv2 := st.MakePersistentVolume().Name("pv2").Label(v1.LabelTopologyZone, "us-west1-a").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumes().Create(testCtx.Ctx, pv2, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create pv2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolume, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeZone plugin is requeued when the PV is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west1-a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				Labels(map[string]string{v1.LabelTopologyZone: "us-west1-a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
			st.MakePersistentVolume().
				Name("pv2").
				Labels(map[string]string{v1.LabelTopologyZone: "us-east1"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pv2 := st.MakePersistentVolume().Name("pv2").Label(v1.LabelTopologyZone, "us-west1-a").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumes().Update(testCtx.Ctx, pv2, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update pv2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolume, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeZone plugin is requeued when the PVC bound to the pod is added",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west1-a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				Labels(map[string]string{v1.LabelTopologyZone: "us-west1-a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
			st.MakePersistentVolume().
				Name("pv2").
				Labels(map[string]string{v1.LabelTopologyZone: "us-east1"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
			st.MakePod().Name("pod3").Container("image").PVC("pvc3").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pvc2 := st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumeClaims(testCtx.NS.Name).Create(testCtx.Ctx, pvc2, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to add pvc2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeZone plugin is requeued when the PVC bound to the pod is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west1-a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				Labels(map[string]string{v1.LabelTopologyZone: "us-west1-a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
			st.MakePersistentVolume().
				Name("pv2").
				Labels(map[string]string{v1.LabelTopologyZone: "us-east1"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
			st.MakePod().Name("pod3").Container("image").PVC("pvc3").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pvc2 := st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumeClaims(testCtx.NS.Name).Update(testCtx.Ctx, pvc2, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update pvc2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolumeClaim, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeZone plugin is requeued when the Storage class is added",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west1-a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				Labels(map[string]string{v1.LabelTopologyZone: "us-west1-a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			sc1 := st.MakeStorageClass().
				Name("sc1").
				VolumeBindingMode(storagev1.VolumeBindingWaitForFirstConsumer).
				Provisioner("p").
				Obj()
			if _, err := testCtx.ClientSet.StorageV1().StorageClasses().Create(testCtx.Ctx, sc1, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create sc1: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.StorageClass, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeZone plugin is not requeued when the PV is updated but the topology is same",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west1-a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				Labels(map[string]string{v1.LabelTopologyZone: "us-west1-a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
			st.MakePersistentVolume().
				Name("pv2").
				Labels(map[string]string{v1.LabelTopologyZone: "us-east1"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pv2 := st.MakePersistentVolume().Name("pv2").
				Labels(map[string]string{v1.LabelTopologyZone: "us-east1", "unrelated": "unrelated"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumes().Update(testCtx.Ctx, pv2, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update pv2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolume, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.Set[string]{},
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the VolumeRestriction plugin is requeued when the PVC bound to the pod is added",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pvc2 := st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumeClaims(testCtx.NS.Name).Create(testCtx.Ctx, pvc2, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to add pvc1: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected by the VolumeRestriction plugin is requeued when the pod is deleted",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc1").Obj(),
			st.MakePod().Name("pod3").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			if err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Delete(testCtx.Ctx, "pod1", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
				return nil, fmt.Errorf("failed to delete pod1: %w", err)
			}
			return map[framework.ClusterEvent]uint64{framework.EventAssignedPodDelete: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name: "Pod rejected with node by the VolumeBinding plugin is requeued when the Node is created",
		InitialNodes: []*v1.Node{
			st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-east-1b").Obj(),
		},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				NodeAffinityIn(v1.LabelTopologyZone, []string{"us-east-1a"}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			node := st.MakeNode().Name("fake-node2").Label(v1.LabelTopologyZone, "us-east-1a").Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name: "Pod rejected with node by the VolumeBinding plugin is requeued when the Node is updated",
		InitialNodes: []*v1.Node{
			st.MakeNode().
				Name("fake-node").
				Label("node", "fake-node").
				Label("aaa", "bbb").
				Obj(),
		},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				NodeAffinityIn("aaa", []string{"ccc"}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			node := st.MakeNode().Name("fake-node").Label("node", "fake-node").Label("aaa", "ccc").Obj()
			if _, err := testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, node, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update node: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.Node, ActionType: framework.UpdateNodeLabel}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the PV is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label("aaa", "bbb").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				NodeAffinityIn("aaa", []string{"ccc"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				VolumeName("pv1").
				Annotation(volume.AnnBindCompleted, "true").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			if err := testCtx.ClientSet.CoreV1().PersistentVolumes().Delete(testCtx.Ctx, "pv1", metav1.DeleteOptions{}); err != nil {
				return nil, fmt.Errorf("failed to delete pv1: %w", err)
			}
			pv1 := st.MakePersistentVolume().
				Name("pv1").
				NodeAffinityIn("aaa", []string{"bbb"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumes().Create(testCtx.Ctx, pv1, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create pv: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolume, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the PV is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-east-1a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				NodeAffinityIn(v1.LabelFailureDomainBetaZone, []string{"us-east-1a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				VolumeName("pv1").
				Annotation(volume.AnnBindCompleted, "true").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pv1 := st.MakePersistentVolume().
				Name("pv1").
				NodeAffinityIn(v1.LabelTopologyZone, []string{"us-east-1a"}).
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumes().Update(testCtx.Ctx, pv1, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update pv: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolume, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the PVC is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			if err := testCtx.ClientSet.CoreV1().PersistentVolumeClaims(testCtx.NS.Name).Delete(testCtx.Ctx, "pvc1", metav1.DeleteOptions{}); err != nil {
				return nil, fmt.Errorf("failed to delete pvc1: %w", err)
			}
			pvc1 := st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumeClaims(testCtx.NS.Name).Create(testCtx.Ctx, pvc1, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create pvc: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the PVC is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pvc1 := st.MakePersistentVolumeClaim().
				Name("pvc1").
				VolumeName("pv1").
				Annotation(volume.AnnBindCompleted, "true").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj()

			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumeClaims(testCtx.NS.Name).Update(testCtx.Ctx, pvc1, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update pvc: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolumeClaim, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the StorageClass is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				StorageClassName("sc1").
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				StorageClassName(ptr.To("sc1")).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			sc1 := st.MakeStorageClass().
				Name("sc1").
				VolumeBindingMode(storagev1.VolumeBindingWaitForFirstConsumer).
				Provisioner("p").
				Obj()
			if _, err := testCtx.ClientSet.StorageV1().StorageClasses().Create(testCtx.Ctx, sc1, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create storageclass: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.StorageClass, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the StorageClass's AllowedTopologies is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west-1a").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				StorageClassName("sc1").
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				StorageClassName(ptr.To("sc1")).
				Obj(),
		},
		InitialStorageClasses: []*storagev1.StorageClass{
			st.MakeStorageClass().
				Name("sc1").
				VolumeBindingMode(storagev1.VolumeBindingWaitForFirstConsumer).
				Provisioner("p").
				AllowedTopologies([]v1.TopologySelectorTerm{
					{
						MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
							{Key: v1.LabelTopologyZone, Values: []string{"us-west-1c"}},
						},
					},
				}).Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			sc1 := st.MakeStorageClass().
				Name("sc1").
				VolumeBindingMode(storagev1.VolumeBindingWaitForFirstConsumer).
				Provisioner("p").
				AllowedTopologies([]v1.TopologySelectorTerm{
					{
						MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
							{Key: v1.LabelTopologyZone, Values: []string{"us-west-1a"}},
						},
					},
				}).
				Obj()
			if _, err := testCtx.ClientSet.StorageV1().StorageClasses().Update(testCtx.Ctx, sc1, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update storageclass: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.StorageClass, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is not requeued when the StorageClass is updated but the AllowedTopologies is same",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Label(v1.LabelTopologyZone, "us-west-1c").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				StorageClassName("sc1").
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				StorageClassName(ptr.To("sc1")).
				Obj(),
		},
		InitialStorageClasses: []*storagev1.StorageClass{
			st.MakeStorageClass().
				Name("sc1").
				Label("key", "value").
				VolumeBindingMode(storagev1.VolumeBindingWaitForFirstConsumer).
				Provisioner("p").
				AllowedTopologies([]v1.TopologySelectorTerm{
					{
						MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
							{Key: v1.LabelTopologyZone, Values: []string{"us-west-1a"}},
						},
					},
				}).Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			sc1 := st.MakeStorageClass().
				Name("sc1").
				Label("key", "updated").
				VolumeBindingMode(storagev1.VolumeBindingWaitForFirstConsumer).
				Provisioner("p").
				AllowedTopologies([]v1.TopologySelectorTerm{
					{
						MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
							{Key: v1.LabelTopologyZone, Values: []string{"us-west-1a"}},
						},
					},
				}).
				Obj()
			if _, err := testCtx.ClientSet.StorageV1().StorageClasses().Update(testCtx.Ctx, sc1, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update storageclass: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.StorageClass, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.Set[string]{},
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the CSINode is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().
				Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			csinode1 := st.MakeCSINode().Name("fake-node").Obj()

			if _, err := testCtx.ClientSet.StorageV1().CSINodes().Create(testCtx.Ctx, csinode1, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create CSINode: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSINode, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the CSINode's MigratedPluginsAnnotation is updated",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				StorageClassName("sc1").
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				StorageClassName(ptr.To("sc1")).
				Obj(),
		},
		InitialCSINodes: []*storagev1.CSINode{
			st.MakeCSINode().Name("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// When updating CSINodes by using MakeCSINode, an error occurs because the resourceVersion is not specified. Therefore, the actual CSINode is retrieved.
			csinode, err := testCtx.ClientSet.StorageV1().CSINodes().Get(testCtx.Ctx, "fake-node", metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get CSINode: %w", err)
			}

			metav1.SetMetaDataAnnotation(&csinode.ObjectMeta, v1.MigratedPluginsAnnotationKey, "value")
			if _, err := testCtx.ClientSet.StorageV1().CSINodes().Update(testCtx.Ctx, csinode, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update CSINode: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSINode, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the CSIDriver's StorageCapacity gets disabled",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialCSIDrivers: []*storagev1.CSIDriver{
			st.MakeCSIDriver().Name("csidriver").StorageCapacity(ptr.To(true)).Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Volume(
				v1.Volume{
					Name: "volume",
					VolumeSource: v1.VolumeSource{
						CSI: &v1.CSIVolumeSource{
							Driver: "csidriver",
						},
					},
				},
			).Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// When updating CSIDrivers by using MakeCSIDriver, an error occurs because the resourceVersion is not specified. Therefore, the actual CSIDrivers is retrieved.
			csidriver, err := testCtx.ClientSet.StorageV1().CSIDrivers().Get(testCtx.Ctx, "csidriver", metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get CSIDriver: %w", err)
			}
			csidriver.Spec.StorageCapacity = ptr.To(false)

			if _, err := testCtx.ClientSet.StorageV1().CSIDrivers().Update(testCtx.Ctx, csidriver, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update CSIDriver: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSIDriver, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is not requeued when the CSIDriver is updated but the storage capacity is originally enabled",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialCSIDrivers: []*storagev1.CSIDriver{
			st.MakeCSIDriver().Name("csidriver").StorageCapacity(ptr.To(false)).Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Volume(
				v1.Volume{
					Name: "volume",
					VolumeSource: v1.VolumeSource{
						CSI: &v1.CSIVolumeSource{
							Driver: "csidriver",
						},
					},
				},
			).Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// When updating CSIDrivers by using MakeCSIDriver, an error occurs because the resourceVersion is not specified. Therefore, the actual CSIDrivers is retrieved.
			csidriver, err := testCtx.ClientSet.StorageV1().CSIDrivers().Get(testCtx.Ctx, "csidriver", metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get CSIDriver: %w", err)
			}
			csidriver.Spec.StorageCapacity = ptr.To(true)

			if _, err := testCtx.ClientSet.StorageV1().CSIDrivers().Update(testCtx.Ctx, csidriver, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update CSIDriver: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSIDriver, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.Set[string]{},
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the CSIStorageCapacity is created",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				StorageClassName("sc1").
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				StorageClassName(ptr.To("sc1")).
				Obj(),
		},
		InitialStorageClasses: []*storagev1.StorageClass{
			st.MakeStorageClass().
				Name("sc1").
				VolumeBindingMode(storagev1.VolumeBindingImmediate).
				Provisioner("p").
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			csc := st.MakeCSIStorageCapacity().Name("csc").StorageClassName("sc1").Capacity(resource.NewQuantity(10, resource.BinarySI)).Obj()
			if _, err := testCtx.ClientSet.StorageV1().CSIStorageCapacities(testCtx.NS.Name).Create(testCtx.Ctx, csc, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create CSIStorageCapacity: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSIStorageCapacity, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is requeued when the CSIStorageCapacity is increased",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				StorageClassName("sc1").
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				StorageClassName(ptr.To("sc1")).
				Obj(),
		},
		InitialStorageCapacities: []*storagev1.CSIStorageCapacity{
			st.MakeCSIStorageCapacity().Name("csc").StorageClassName("sc1").Capacity(resource.NewQuantity(10, resource.BinarySI)).Obj(),
		},
		InitialStorageClasses: []*storagev1.StorageClass{
			st.MakeStorageClass().
				Name("sc1").
				VolumeBindingMode(storagev1.VolumeBindingImmediate).
				Provisioner("p").
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// When updating CSIStorageCapacities by using MakeCSIStorageCapacity, an error occurs because the resourceVersion is not specified. Therefore, the actual CSIStorageCapacities is retrieved.
			csc, err := testCtx.ClientSet.StorageV1().CSIStorageCapacities(testCtx.NS.Name).Get(testCtx.Ctx, "csc", metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get CSIStorageCapacity: %w", err)
			}
			csc.Capacity = resource.NewQuantity(20, resource.BinarySI)

			if _, err := testCtx.ClientSet.StorageV1().CSIStorageCapacities(testCtx.NS.Name).Update(testCtx.Ctx, csc, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update CSIStorageCapacity: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSIStorageCapacity, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true, false),
	},
	{
		Name:         "Pod rejected with node by the VolumeBinding plugin is not requeued when the CSIStorageCapacity is updated but the volumelimit is not increased",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: ptr.To(v1.HostPathDirectoryOrCreate)}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialStorageCapacities: []*storagev1.CSIStorageCapacity{
			st.MakeCSIStorageCapacity().Name("csc").StorageClassName("sc1").Capacity(resource.NewQuantity(10, resource.BinarySI)).Obj(),
		},
		InitialStorageClasses: []*storagev1.StorageClass{
			st.MakeStorageClass().
				Name("sc1").
				VolumeBindingMode(storagev1.VolumeBindingImmediate).
				Provisioner("p").
				Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			// When updating CSIStorageCapacities by using MakeCSIStorageCapacity, an error occurs because the resourceVersion is not specified. Therefore, the actual CSIStorageCapacities is retrieved.
			csc, err := testCtx.ClientSet.StorageV1().CSIStorageCapacities(testCtx.NS.Name).Get(testCtx.Ctx, "csc", metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get CSIStorageCapacity: %w", err)
			}
			csc.Capacity = resource.NewQuantity(5, resource.BinarySI)

			if _, err := testCtx.ClientSet.StorageV1().CSIStorageCapacities(testCtx.NS.Name).Update(testCtx.Ctx, csc, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to update CSIStorageCapacity: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSIStorageCapacity, ActionType: framework.Update}: 1}, nil
		},
		WantRequeuedPods:          sets.Set[string]{},
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected the CSI plugin is requeued when the CSINode is added",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				PersistentVolumeSource(v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{Driver: "csidriver", VolumeHandle: "volumehandle"}}).
				Obj()},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialCSIDrivers: []*storagev1.CSIDriver{
			st.MakeCSIDriver().Name("csidriver").StorageCapacity(ptr.To(true)).Obj(),
		},
		InitialCSINodes: []*storagev1.CSINode{
			st.MakeCSINode().Name("fake-node").Driver(storagev1.CSINodeDriver{Name: "csidriver", NodeID: "fake-node", Allocatable: &storagev1.VolumeNodeResources{
				Count: ptr.To(int32(0)),
			}}).Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			csinode1 := st.MakeCSINode().Name("csinode").Obj()
			if _, err := testCtx.ClientSet.StorageV1().CSINodes().Create(testCtx.Ctx, csinode1, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to create CSINode: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.CSINode, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod1"),
		EnableSchedulingQueueHint: sets.New(true),
	},
	{
		Name:         "Pod rejected with PVC by the CSI plugin is requeued when the pod having related PVC is deleted",
		InitialNodes: []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				PersistentVolumeSource(v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{Driver: "csidriver", VolumeHandle: "volumehandle1"}}).
				Obj(),
			st.MakePersistentVolume().Name("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				PersistentVolumeSource(v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{Driver: "csidriver", VolumeHandle: "volumehandle2"}}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
			st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialCSIDrivers: []*storagev1.CSIDriver{
			st.MakeCSIDriver().Name("csidriver").StorageCapacity(ptr.To(true)).Obj(),
		},
		InitialCSINodes: []*storagev1.CSINode{
			st.MakeCSINode().Name("fake-node").Driver(storagev1.CSINodeDriver{Name: "csidriver", NodeID: "fake-node", Allocatable: &storagev1.VolumeNodeResources{
				Count: ptr.To(int32(1)),
			}}).Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			if err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Delete(testCtx.Ctx, "pod1", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
				return nil, fmt.Errorf("failed to delete Pod: %w", err)
			}
			return map[framework.ClusterEvent]uint64{framework.EventAssignedPodDelete: 1}, nil
		},
		WantRequeuedPods: sets.New("pod2"),
	},
	{
		Name:          "Pod rejected with PVC by the CSI plugin is requeued when the related PVC is added",
		InitialNodes:  []*v1.Node{st.MakeNode().Name("fake-node").Label("node", "fake-node").Obj()},
		EnablePlugins: []string{"VolumeBinding"},
		InitialPVs: []*v1.PersistentVolume{
			st.MakePersistentVolume().Name("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				PersistentVolumeSource(v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{Driver: "csidriver", VolumeHandle: "volumehandle1"}}).
				Obj(),
			st.MakePersistentVolume().Name("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}).
				Capacity(v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}).
				PersistentVolumeSource(v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{Driver: "csidriver", VolumeHandle: "volumehandle2"}}).
				Obj(),
		},
		InitialPVCs: []*v1.PersistentVolumeClaim{
			st.MakePersistentVolumeClaim().
				Name("pvc1").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv1").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj(),
		},
		InitialCSIDrivers: []*storagev1.CSIDriver{
			st.MakeCSIDriver().Name("csidriver").StorageCapacity(ptr.To(true)).Obj(),
		},
		InitialCSINodes: []*storagev1.CSINode{
			st.MakeCSINode().Name("fake-node").Driver(storagev1.CSINodeDriver{Name: "csidriver", NodeID: "fake-node", Allocatable: &storagev1.VolumeNodeResources{
				Count: ptr.To(int32(1)),
			}}).Obj(),
		},
		InitialPods: []*v1.Pod{
			st.MakePod().Name("pod1").Container("image").PVC("pvc1").Node("fake-node").Obj(),
		},
		Pods: []*v1.Pod{
			st.MakePod().Name("pod2").Container("image").PVC("pvc2").Obj(),
		},
		TriggerFn: func(testCtx *testutils.TestContext) (map[framework.ClusterEvent]uint64, error) {
			pvc := st.MakePersistentVolumeClaim().
				Name("pvc2").
				Annotation(volume.AnnBindCompleted, "true").
				VolumeName("pv2").
				AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
				Resources(v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}).
				Obj()
			if _, err := testCtx.ClientSet.CoreV1().PersistentVolumeClaims(testCtx.NS.Name).Create(testCtx.Ctx, pvc, metav1.CreateOptions{}); err != nil {
				return nil, fmt.Errorf("failed to add pvc2: %w", err)
			}
			return map[framework.ClusterEvent]uint64{{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add}: 1}, nil
		},
		WantRequeuedPods:          sets.New("pod2"),
		EnableSchedulingQueueHint: sets.New(true),
	},
}

// TestCoreResourceEnqueue verify Pods failed by in-tree default plugins can be
// moved properly upon their registered events.
func RunTestCoreResourceEnqueue(t *testing.T, tt *CoreResourceEnqueueTestCase) {
	t.Helper()
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)

	opts := []scheduler.Option{scheduler.WithPodInitialBackoffSeconds(0), scheduler.WithPodMaxBackoffSeconds(0)}
	if tt.EnablePlugins != nil {
		enablePlugins := []configv1.Plugin{
			// These are required plugins to start the scheduler.
			{Name: "PrioritySort"},
			{Name: "DefaultBinder"},
		}
		for _, pluginName := range tt.EnablePlugins {
			enablePlugins = append(enablePlugins, configv1.Plugin{Name: pluginName})
		}
		profile := configv1.KubeSchedulerProfile{
			SchedulerName: ptr.To("default-scheduler"),
			Plugins: &configv1.Plugins{
				MultiPoint: configv1.PluginSet{
					Enabled: enablePlugins,
					Disabled: []configv1.Plugin{
						{Name: "*"},
					},
				},
			},
		}
		cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
			Profiles: []configv1.KubeSchedulerProfile{profile},
		})
		opts = append(opts, scheduler.WithProfiles(cfg.Profiles...))
	}

	// Use zero backoff seconds to bypass backoffQ.
	// It's intended to not start the scheduler's queue, and hence to
	// not start any flushing logic. We will pop and schedule the Pods manually later.
	testCtx := testutils.InitTestSchedulerWithOptions(
		t,
		testutils.InitTestAPIServer(t, "core-res-enqueue", nil),
		0,
		opts...,
	)
	testutils.SyncSchedulerInformerFactory(testCtx)

	defer testCtx.Scheduler.SchedulingQueue.Close()

	cs, ns, ctx := testCtx.ClientSet, testCtx.NS.Name, testCtx.Ctx
	// Create one Node with a taint.
	for _, node := range tt.InitialNodes {
		if _, err := cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create an initial Node %q: %v", node.Name, err)
		}
	}

	for _, csinode := range tt.InitialCSINodes {
		if _, err := testCtx.ClientSet.StorageV1().CSINodes().Create(ctx, csinode, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create a CSINode %q: %v", csinode.Name, err)
		}
	}

	for _, csc := range tt.InitialStorageCapacities {
		if _, err := cs.StorageV1().CSIStorageCapacities(ns).Create(ctx, csc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create a CSIStorageCapacity %q: %v", csc.Name, err)
		}
	}

	for _, csidriver := range tt.InitialCSIDrivers {
		if _, err := cs.StorageV1().CSIDrivers().Create(ctx, csidriver, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create a CSIDriver %q: %v", csidriver.Name, err)
		}
	}

	for _, sc := range tt.InitialStorageClasses {
		if _, err := cs.StorageV1().StorageClasses().Create(testCtx.Ctx, sc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create a StorageClass %q: %v", sc.Name, err)
		}
	}

	for _, pv := range tt.InitialPVs {
		if _, err := testutils.CreatePV(cs, pv); err != nil {
			t.Fatalf("Failed to create a PV %q: %v", pv.Name, err)
		}
	}

	for _, pvc := range tt.InitialPVCs {
		pvc.Namespace = ns
		if _, err := testutils.CreatePVC(cs, pvc); err != nil {
			t.Fatalf("Failed to create a PVC %q: %v", pvc.Name, err)
		}
	}

	for _, pod := range tt.InitialPods {
		if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create an initial Pod %q: %v", pod.Name, err)
		}
	}

	for _, pod := range tt.Pods {
		if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
		}
	}

	// Wait for the tt.Pods to be present in the scheduling active queue.
	if err := wait.PollUntilContextTimeout(ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
		return len(pendingPods) == len(tt.Pods) && len(testCtx.Scheduler.SchedulingQueue.PodsInActiveQ()) == len(tt.Pods), nil
	}); err != nil {
		t.Fatalf("Failed to wait for all pods to be present in the scheduling queue: %v", err)
	}

	t.Log("Confirmed Pods in the scheduling queue, starting to schedule them")

	// Pop all pods out. They should become unschedulable.
	for i := 0; i < len(tt.Pods); i++ {
		testCtx.Scheduler.ScheduleOne(testCtx.Ctx)
	}
	// Wait for the tt.Pods to be still present in the scheduling (unschedulable) queue.
	if err := wait.PollUntilContextTimeout(ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		activePodsCount := len(testCtx.Scheduler.SchedulingQueue.PodsInActiveQ())
		if activePodsCount > 0 {
			return false, fmt.Errorf("active queue was expected to be empty, but found %v Pods", activePodsCount)
		}

		pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
		return len(pendingPods) == len(tt.Pods), nil
	}); err != nil {
		t.Fatalf("Failed to wait for all pods to remain in the scheduling queue after scheduling attempts: %v", err)
	}

	t.Log("finished initial schedulings for all Pods, will trigger triggerFn")

	legacyregistry.Reset() // reset the metric before triggering
	wantTriggeredEvents, err := tt.TriggerFn(testCtx)
	if err != nil {
		t.Fatalf("Failed to trigger the event: %v", err)
	}

	t.Log("finished triggering triggerFn, waiting for the scheduler to handle events")

	if err := wait.PollUntilContextTimeout(ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		for e, count := range wantTriggeredEvents {
			vec, err := testutil.GetHistogramVecFromGatherer(legacyregistry.DefaultGatherer, "scheduler_event_handling_duration_seconds", map[string]string{
				"event": string(e.Label()),
			})
			if err != nil {
				return false, err
			}

			if vec.GetAggregatedSampleCount() != count {
				t.Logf("Expected %d sample for event %s, got %d", count, e.Label(), vec.GetAggregatedSampleCount())
				return false, nil
			}
		}

		return true, nil
	}); err != nil {
		t.Fatalf("Failed to wait for the scheduler to handle the event: %v", err)
	}

	t.Log("all events are handled by the scheduler, will check if tt.requeuedPods are requeued")

	// Wait for the tt.Pods to be still present in the scheduling queue.
	var requeuedPods sets.Set[string]
	if err := wait.PollUntilContextTimeout(ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		requeuedPods = sets.Set[string]{} // reset
		for _, requeuedPod := range testCtx.Scheduler.SchedulingQueue.PodsInActiveQ() {
			requeuedPods.Insert(requeuedPod.Name)
		}

		return requeuedPods.Equal(tt.WantRequeuedPods), nil
	}); err != nil {
		t.Fatalf("Expect Pods %v to be requeued, but %v are requeued actually", tt.WantRequeuedPods, requeuedPods)
	}
}
