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

package nodeunschedulable

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func TestNodeUnschedulable(t *testing.T) {
	testCases := []struct {
		name       string
		pod        *v1.Pod
		node       *v1.Node
		wantStatus *framework.Status
	}{
		{
			name: "Does not schedule pod to unschedulable node (node.Spec.Unschedulable==true)",
			pod:  &v1.Pod{},
			node: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: true,
				},
			},
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, ErrReasonUnschedulable),
		},
		{
			name: "Schedule pod to normal node",
			pod:  &v1.Pod{},
			node: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: false,
				},
			},
		},
		{
			name: "Schedule pod with toleration to unschedulable node (node.Spec.Unschedulable==true)",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Tolerations: []v1.Toleration{
						{
							Key:    v1.TaintNodeUnschedulable,
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
			node: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: true,
				},
			},
		},
	}

	for _, test := range testCases {
		nodeInfo := framework.NewNodeInfo()
		nodeInfo.SetNode(test.node)
		_, ctx := ktesting.NewTestContext(t)
		p, err := New(ctx, nil, nil, feature.Features{})
		if err != nil {
			t.Fatalf("creating plugin: %v", err)
		}
		gotStatus := p.(framework.FilterPlugin).Filter(ctx, nil, test.pod, nodeInfo)
		if diff := cmp.Diff(test.wantStatus, gotStatus); diff != "" {
			t.Errorf("status does not match (-want,+got):\n%s", diff)
		}
	}
}

func TestIsSchedulableAfterNodeChange(t *testing.T) {
	testCases := []struct {
		name           string
		pod            *v1.Pod
		oldObj, newObj interface{}
		expectedHint   framework.QueueingHint
		expectedErr    bool
	}{
		{
			name:         "error-wrong-new-object",
			pod:          &v1.Pod{},
			newObj:       "not-a-node",
			expectedHint: framework.Queue,
			expectedErr:  true,
		},
		{
			name: "error-wrong-old-object",
			pod:  &v1.Pod{},
			newObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: true,
				},
			},
			oldObj:       "not-a-node",
			expectedHint: framework.Queue,
			expectedErr:  true,
		},
		{
			name: "skip-queue-on-unschedulable-node-added",
			pod:  &v1.Pod{},
			newObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: true,
				},
			},
			expectedHint: framework.QueueSkip,
		},
		{
			name: "queue-on-schedulable-node-added",
			pod:  &v1.Pod{},
			newObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: false,
				},
			},
			expectedHint: framework.Queue,
		},
		{
			name: "skip-unrelated-change-unschedulable-true",
			pod:  &v1.Pod{},
			newObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: true,
					Taints: []v1.Taint{
						{
							Key:    v1.TaintNodeNotReady,
							Effect: v1.TaintEffectNoExecute,
						},
					},
				},
			},
			oldObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: true,
				},
			},
			expectedHint: framework.QueueSkip,
		},
		{
			name: "skip-unrelated-change-unschedulable-false",
			pod:  &v1.Pod{},
			newObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: false,
					Taints: []v1.Taint{
						{
							Key:    v1.TaintNodeNotReady,
							Effect: v1.TaintEffectNoExecute,
						},
					},
				},
			},
			oldObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: false,
				},
			},
			expectedHint: framework.QueueSkip,
		},
		{
			name: "queue-on-unschedulable-field-change",
			pod:  &v1.Pod{},
			newObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: false,
				},
			},
			oldObj: &v1.Node{
				Spec: v1.NodeSpec{
					Unschedulable: true,
				},
			},
			expectedHint: framework.Queue,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			pl := &NodeUnschedulable{}
			got, err := pl.isSchedulableAfterNodeChange(logger, testCase.pod, testCase.oldObj, testCase.newObj)
			if err != nil && !testCase.expectedErr {
				t.Errorf("unexpected error: %v", err)
			}
			if got != testCase.expectedHint {
				t.Errorf("isSchedulableAfterNodeChange() = %v, want %v", got, testCase.expectedHint)
			}
		})
	}
}

func TestIsSchedulableAfterPodTolerationChange(t *testing.T) {
	testCases := []struct {
		name           string
		pod            *v1.Pod
		oldObj, newObj interface{}
		expectedHint   framework.QueueingHint
		expectedErr    bool
	}{
		{
			name:         "error-wrong-new-object",
			pod:          &v1.Pod{},
			newObj:       "not-a-pod",
			expectedHint: framework.Queue,
			expectedErr:  true,
		},
		{
			name:         "error-wrong-old-object",
			pod:          &v1.Pod{},
			newObj:       &v1.Pod{},
			oldObj:       "not-a-pod",
			expectedHint: framework.Queue,
			expectedErr:  true,
		},
		{
			name:         "skip-different-pod-update",
			pod:          st.MakePod().UID("uid").Obj(),
			newObj:       st.MakePod().UID("different-uid").Toleration(v1.TaintNodeUnschedulable).Obj(),
			oldObj:       st.MakePod().UID("different-uid").Obj(),
			expectedHint: framework.QueueSkip,
		},
		{
			name:         "queue-when-the-unsched-pod-gets-toleration",
			pod:          st.MakePod().UID("uid").Obj(),
			newObj:       st.MakePod().UID("uid").Toleration(v1.TaintNodeUnschedulable).Obj(),
			oldObj:       st.MakePod().UID("uid").Obj(),
			expectedHint: framework.Queue,
		},
		{
			name:         "skip-when-the-unsched-pod-gets-unrelated-toleration",
			pod:          st.MakePod().UID("uid").Obj(),
			newObj:       st.MakePod().UID("uid").Toleration("unrelated-key").Obj(),
			oldObj:       st.MakePod().UID("uid").Obj(),
			expectedHint: framework.QueueSkip,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			pl := &NodeUnschedulable{}
			got, err := pl.isSchedulableAfterPodTolerationChange(logger, testCase.pod, testCase.oldObj, testCase.newObj)
			if err != nil && !testCase.expectedErr {
				t.Errorf("unexpected error: %v", err)
			}
			if got != testCase.expectedHint {
				t.Errorf("isSchedulableAfterNodeChange() = %v, want %v", got, testCase.expectedHint)
			}
		})
	}
}
