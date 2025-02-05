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

package tainttoleration

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
)

func nodeWithTaints(nodeName string, taints []v1.Taint) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Spec: v1.NodeSpec{
			Taints: taints,
		},
	}
}

func podWithTolerations(podName string, tolerations []v1.Toleration) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Tolerations: tolerations,
		},
	}
}

func TestTaintTolerationScore(t *testing.T) {
	tests := []struct {
		name         string
		pod          *v1.Pod
		nodes        []*v1.Node
		expectedList framework.NodeScoreList
	}{
		// basic test case
		{
			name: "node with taints tolerated by the pod, gets a higher score than those node with intolerable taints",
			pod: podWithTolerations("pod1", []v1.Toleration{{
				Key:      "foo",
				Operator: v1.TolerationOpEqual,
				Value:    "bar",
				Effect:   v1.TaintEffectPreferNoSchedule,
			}}),
			nodes: []*v1.Node{
				nodeWithTaints("nodeA", []v1.Taint{{
					Key:    "foo",
					Value:  "bar",
					Effect: v1.TaintEffectPreferNoSchedule,
				}}),
				nodeWithTaints("nodeB", []v1.Taint{{
					Key:    "foo",
					Value:  "blah",
					Effect: v1.TaintEffectPreferNoSchedule,
				}}),
			},
			expectedList: []framework.NodeScore{
				{Name: "nodeA", Score: framework.MaxNodeScore},
				{Name: "nodeB", Score: 0},
			},
		},
		// the count of taints that are tolerated by pod, does not matter.
		{
			name: "the nodes that all of their taints are tolerated by the pod, get the same score, no matter how many tolerable taints a node has",
			pod: podWithTolerations("pod1", []v1.Toleration{
				{
					Key:      "cpu-type",
					Operator: v1.TolerationOpEqual,
					Value:    "arm64",
					Effect:   v1.TaintEffectPreferNoSchedule,
				}, {
					Key:      "disk-type",
					Operator: v1.TolerationOpEqual,
					Value:    "ssd",
					Effect:   v1.TaintEffectPreferNoSchedule,
				},
			}),
			nodes: []*v1.Node{
				nodeWithTaints("nodeA", []v1.Taint{}),
				nodeWithTaints("nodeB", []v1.Taint{
					{
						Key:    "cpu-type",
						Value:  "arm64",
						Effect: v1.TaintEffectPreferNoSchedule,
					},
				}),
				nodeWithTaints("nodeC", []v1.Taint{
					{
						Key:    "cpu-type",
						Value:  "arm64",
						Effect: v1.TaintEffectPreferNoSchedule,
					}, {
						Key:    "disk-type",
						Value:  "ssd",
						Effect: v1.TaintEffectPreferNoSchedule,
					},
				}),
			},
			expectedList: []framework.NodeScore{
				{Name: "nodeA", Score: framework.MaxNodeScore},
				{Name: "nodeB", Score: framework.MaxNodeScore},
				{Name: "nodeC", Score: framework.MaxNodeScore},
			},
		},
		// the count of taints on a node that are not tolerated by pod, matters.
		{
			name: "the more intolerable taints a node has, the lower score it gets.",
			pod: podWithTolerations("pod1", []v1.Toleration{{
				Key:      "foo",
				Operator: v1.TolerationOpEqual,
				Value:    "bar",
				Effect:   v1.TaintEffectPreferNoSchedule,
			}}),
			nodes: []*v1.Node{
				nodeWithTaints("nodeA", []v1.Taint{}),
				nodeWithTaints("nodeB", []v1.Taint{
					{
						Key:    "cpu-type",
						Value:  "arm64",
						Effect: v1.TaintEffectPreferNoSchedule,
					},
				}),
				nodeWithTaints("nodeC", []v1.Taint{
					{
						Key:    "cpu-type",
						Value:  "arm64",
						Effect: v1.TaintEffectPreferNoSchedule,
					}, {
						Key:    "disk-type",
						Value:  "ssd",
						Effect: v1.TaintEffectPreferNoSchedule,
					},
				}),
			},
			expectedList: []framework.NodeScore{
				{Name: "nodeA", Score: framework.MaxNodeScore},
				{Name: "nodeB", Score: 50},
				{Name: "nodeC", Score: 0},
			},
		},
		// taints-tolerations priority only takes care about the taints and tolerations that have effect PreferNoSchedule
		{
			name: "only taints and tolerations that have effect PreferNoSchedule are checked by taints-tolerations priority function",
			pod: podWithTolerations("pod1", []v1.Toleration{
				{
					Key:      "cpu-type",
					Operator: v1.TolerationOpEqual,
					Value:    "arm64",
					Effect:   v1.TaintEffectNoSchedule,
				}, {
					Key:      "disk-type",
					Operator: v1.TolerationOpEqual,
					Value:    "ssd",
					Effect:   v1.TaintEffectNoSchedule,
				},
			}),
			nodes: []*v1.Node{
				nodeWithTaints("nodeA", []v1.Taint{}),
				nodeWithTaints("nodeB", []v1.Taint{
					{
						Key:    "cpu-type",
						Value:  "arm64",
						Effect: v1.TaintEffectNoSchedule,
					},
				}),
				nodeWithTaints("nodeC", []v1.Taint{
					{
						Key:    "cpu-type",
						Value:  "arm64",
						Effect: v1.TaintEffectPreferNoSchedule,
					}, {
						Key:    "disk-type",
						Value:  "ssd",
						Effect: v1.TaintEffectPreferNoSchedule,
					},
				}),
			},
			expectedList: []framework.NodeScore{
				{Name: "nodeA", Score: framework.MaxNodeScore},
				{Name: "nodeB", Score: framework.MaxNodeScore},
				{Name: "nodeC", Score: 0},
			},
		},
		{
			name: "Default behaviour No taints and tolerations, lands on node with no taints",
			//pod without tolerations
			pod: podWithTolerations("pod1", []v1.Toleration{}),
			nodes: []*v1.Node{
				//Node without taints
				nodeWithTaints("nodeA", []v1.Taint{}),
				nodeWithTaints("nodeB", []v1.Taint{
					{
						Key:    "cpu-type",
						Value:  "arm64",
						Effect: v1.TaintEffectPreferNoSchedule,
					},
				}),
			},
			expectedList: []framework.NodeScore{
				{Name: "nodeA", Score: framework.MaxNodeScore},
				{Name: "nodeB", Score: 0},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			state := framework.NewCycleState()
			snapshot := cache.NewSnapshot(nil, test.nodes)
			fh, _ := runtime.NewFramework(ctx, nil, nil, runtime.WithSnapshotSharedLister(snapshot))

			p, err := New(ctx, nil, fh, feature.Features{})
			if err != nil {
				t.Fatalf("creating plugin: %v", err)
			}
			status := p.(framework.PreScorePlugin).PreScore(ctx, state, test.pod, tf.BuildNodeInfos(test.nodes))
			if !status.IsSuccess() {
				t.Errorf("unexpected error: %v", status)
			}
			var gotList framework.NodeScoreList
			for _, n := range test.nodes {
				nodeName := n.ObjectMeta.Name
				score, status := p.(framework.ScorePlugin).Score(ctx, state, test.pod, nodeName)
				if !status.IsSuccess() {
					t.Errorf("unexpected error: %v", status)
				}
				gotList = append(gotList, framework.NodeScore{Name: nodeName, Score: score})
			}

			status = p.(framework.ScorePlugin).ScoreExtensions().NormalizeScore(ctx, state, test.pod, gotList)
			if !status.IsSuccess() {
				t.Errorf("unexpected error: %v", status)
			}

			if diff := cmp.Diff(test.expectedList, gotList); diff != "" {
				t.Errorf("Unexpected NodeScoreList (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestTaintTolerationFilter(t *testing.T) {
	tests := []struct {
		name       string
		pod        *v1.Pod
		node       *v1.Node
		wantStatus *framework.Status
	}{
		{
			name: "A pod having no tolerations can't be scheduled onto a node with nonempty taints",
			pod:  podWithTolerations("pod1", []v1.Toleration{}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable,
				"node(s) had untolerated taint {dedicated: user1}"),
		},
		{
			name: "A pod which can be scheduled on a dedicated node assigned to user1 with effect NoSchedule",
			pod:  podWithTolerations("pod1", []v1.Toleration{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
		},
		{
			name: "A pod which can't be scheduled on a dedicated node assigned to user2 with effect NoSchedule",
			pod:  podWithTolerations("pod1", []v1.Toleration{{Key: "dedicated", Operator: "Equal", Value: "user2", Effect: "NoSchedule"}}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable,
				"node(s) had untolerated taint {dedicated: user1}"),
		},
		{
			name: "A pod can be scheduled onto the node, with a toleration uses operator Exists that tolerates the taints on the node",
			pod:  podWithTolerations("pod1", []v1.Toleration{{Key: "foo", Operator: "Exists", Effect: "NoSchedule"}}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "foo", Value: "bar", Effect: "NoSchedule"}}),
		},
		{
			name: "A pod has multiple tolerations, node has multiple taints, all the taints are tolerated, pod can be scheduled onto the node",
			pod: podWithTolerations("pod1", []v1.Toleration{
				{Key: "dedicated", Operator: "Equal", Value: "user2", Effect: "NoSchedule"},
				{Key: "foo", Operator: "Exists", Effect: "NoSchedule"},
			}),
			node: nodeWithTaints("nodeA", []v1.Taint{
				{Key: "dedicated", Value: "user2", Effect: "NoSchedule"},
				{Key: "foo", Value: "bar", Effect: "NoSchedule"},
			}),
		},
		{
			name: "A pod has a toleration that keys and values match the taint on the node, but (non-empty) effect doesn't match, " +
				"can't be scheduled onto the node",
			pod:  podWithTolerations("pod1", []v1.Toleration{{Key: "foo", Operator: "Equal", Value: "bar", Effect: "PreferNoSchedule"}}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "foo", Value: "bar", Effect: "NoSchedule"}}),
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable,
				"node(s) had untolerated taint {foo: bar}"),
		},
		{
			name: "The pod has a toleration that keys and values match the taint on the node, the effect of toleration is empty, " +
				"and the effect of taint is NoSchedule. Pod can be scheduled onto the node",
			pod:  podWithTolerations("pod1", []v1.Toleration{{Key: "foo", Operator: "Equal", Value: "bar"}}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "foo", Value: "bar", Effect: "NoSchedule"}}),
		},
		{
			name: "The pod has a toleration that key and value don't match the taint on the node, " +
				"but the effect of taint on node is PreferNoSchedule. Pod can be scheduled onto the node",
			pod:  podWithTolerations("pod1", []v1.Toleration{{Key: "dedicated", Operator: "Equal", Value: "user2", Effect: "NoSchedule"}}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "PreferNoSchedule"}}),
		},
		{
			name: "The pod has no toleration, " +
				"but the effect of taint on node is PreferNoSchedule. Pod can be scheduled onto the node",
			pod:  podWithTolerations("pod1", []v1.Toleration{}),
			node: nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "PreferNoSchedule"}}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(test.node)
			p, err := New(ctx, nil, nil, feature.Features{})
			if err != nil {
				t.Fatalf("creating plugin: %v", err)
			}
			gotStatus := p.(framework.FilterPlugin).Filter(ctx, nil, test.pod, nodeInfo)
			if diff := cmp.Diff(test.wantStatus, gotStatus); diff != "" {
				t.Errorf("Unexpected status (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestIsSchedulableAfterNodeChange(t *testing.T) {
	tests := []struct {
		name         string
		pod          *v1.Pod
		oldObj       interface{}
		newObj       interface{}
		expectedHint framework.QueueingHint
		wantErr      bool
	}{
		{
			name:         "backoff-wrong-new-object",
			newObj:       "not-a-node",
			expectedHint: framework.Queue,
			wantErr:      true,
		},
		{
			name:         "backoff-wrong-old-object",
			newObj:       nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
			oldObj:       "not-a-node",
			expectedHint: framework.Queue,
			wantErr:      true,
		},
		{
			name:         "skip-queue-on-untoleratedtaint-node-added",
			pod:          podWithTolerations("pod1", []v1.Toleration{{Key: "dedicated", Operator: "Equal", Value: "user2", Effect: "NoSchedule"}}),
			newObj:       nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
			expectedHint: framework.QueueSkip,
		},
		{
			name:         "queue-on-toleratedtaint-node-added",
			pod:          podWithTolerations("pod1", []v1.Toleration{{Key: "dedicated", Operator: "Equal", Value: "user2", Effect: "NoSchedule"}}),
			newObj:       nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user2", Effect: "NoSchedule"}}),
			expectedHint: framework.Queue,
		},
		{
			name:         "skip-unrelated-change",
			pod:          podWithTolerations("pod1", []v1.Toleration{{Key: "dedicated", Operator: "Equal", Value: "user2", Effect: "NoSchedule"}}),
			newObj:       nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}, {Key: "dedicated", Value: "user3", Effect: "NoSchedule"}}),
			oldObj:       nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
			expectedHint: framework.QueueSkip,
		},
		{
			name:         "queue-on-taint-change",
			pod:          podWithTolerations("pod1", []v1.Toleration{{Key: "dedicated", Operator: "Equal", Value: "user2", Effect: "NoSchedule"}}),
			newObj:       nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user2", Effect: "NoSchedule"}}),
			oldObj:       nodeWithTaints("nodeA", []v1.Taint{{Key: "dedicated", Value: "user1", Effect: "NoSchedule"}}),
			expectedHint: framework.Queue,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			pl := &TaintToleration{}
			got, err := pl.isSchedulableAfterNodeChange(logger, test.pod, test.oldObj, test.newObj)
			if (err != nil) != test.wantErr {
				t.Errorf("isSchedulableAfterNodeChange() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.expectedHint {
				t.Errorf("isSchedulableAfterNodeChange() = %v, want %v", got, test.expectedHint)
			}
		})
	}
}

func Test_isSchedulableAfterPodTolerationChange(t *testing.T) {
	testcases := map[string]struct {
		pod            *v1.Pod
		oldObj, newObj interface{}
		expectedHint   framework.QueueingHint
		expectedErr    bool
	}{
		"backoff-wrong-new-object": {
			pod:          &v1.Pod{},
			newObj:       "not-a-pod",
			expectedHint: framework.Queue,
			expectedErr:  true,
		},
		"backoff-wrong-old-object": {
			pod:          &v1.Pod{},
			oldObj:       "not-a-pod",
			newObj:       &v1.Pod{},
			expectedHint: framework.Queue,
			expectedErr:  true,
		},
		"skip-updates-other-pod": {
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-1",
					Namespace: "ns-1",
					UID:       "uid0",
				}},
			oldObj: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-2",
					Namespace: "ns-1",
					UID:       "uid1",
				}},
			newObj: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-2",
					Namespace: "ns-1",
					UID:       "uid1",
				},
				Spec: v1.PodSpec{
					Tolerations: []v1.Toleration{
						{
							Key:    "foo",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
			expectedHint: framework.QueueSkip,
			expectedErr:  false,
		},
		"queue-on-toleration-added": {
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-1",
					Namespace: "ns-1",
				}},
			oldObj: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-1",
					Namespace: "ns-1",
				}},
			newObj: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-1",
					Namespace: "ns-1",
				},
				Spec: v1.PodSpec{
					Tolerations: []v1.Toleration{
						{
							Key:    "foo",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
			expectedHint: framework.Queue,
			expectedErr:  false,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			p, err := New(ctx, nil, nil, feature.Features{})
			if err != nil {
				t.Fatalf("creating plugin: %v", err)
			}
			actualHint, err := p.(*TaintToleration).isSchedulableAfterPodTolerationChange(logger, tc.pod, tc.oldObj, tc.newObj)
			if tc.expectedErr {
				if err == nil {
					t.Errorf("unexpected success")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error")
				return
			}
			if diff := cmp.Diff(tc.expectedHint, actualHint); diff != "" {
				t.Errorf("Unexpected hint (-want, +got):\n%s", diff)
			}
		})
	}
}
