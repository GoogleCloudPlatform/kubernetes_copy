/*
Copyright 2015 The Kubernetes Authors.

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

package job

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	batch "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

func TestMatchPodFailurePolicy(t *testing.T) {
	validPodObjectMeta := metav1.ObjectMeta{
		Namespace: "default",
		Name:      "mypod",
	}
	ignore := batch.PodFailurePolicyActionIgnore
	failJob := batch.PodFailurePolicyActionFailJob
	failIndex := batch.PodFailurePolicyActionFailIndex
	count := batch.PodFailurePolicyActionCount

	testCases := map[string]struct {
		enableJobBackoffLimitPerIndex bool
		podFailurePolicy              *batch.PodFailurePolicy
		failedPod                     *v1.Pod
		wantJobFailureMessage         *string
		wantCountFailed               bool
		wantAction                    *batch.PodFailurePolicyAction
	}{
		"unknown action for rule matching by exit codes - skip rule with unknown action": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: "UnknownAction",
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2},
						},
					},
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{2, 3},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "main-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: ptr.To("Container main-container for pod default/mypod failed with exit code 2 matching FailJob rule at index 1"),
			wantCountFailed:       true,
			wantAction:            &failJob,
		},
		"unknown action for rule matching by pod conditions - skip rule with unknown action": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: "UnknownAction",
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       false,
			wantAction:            &ignore,
		},
		"unknown operator - rule with unknown action is skipped for onExitCodes": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: "UnknownOperator",
							Values:   []int32{1, 2},
						},
					},
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{2, 3},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "main-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: ptr.To("Container main-container for pod default/mypod failed with exit code 2 matching FailJob rule at index 1"),
			wantCountFailed:       true,
			wantAction:            &failJob,
		},
		"no policy rules": {
			podFailurePolicy: nil,
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
		},
		"ignore rule matched for exit codes": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2, 3},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       false,
			wantAction:            &ignore,
		},
		"ignore rule matched on negative exit codes - needed for Windows support": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{-1073741676, -1073741510},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: -1073741510,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       false,
			wantAction:            &ignore,
		},
		"FailJob rule matched for exit codes": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2, 3},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "main-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: ptr.To("Container main-container for pod default/mypod failed with exit code 2 matching FailJob rule at index 0"),
			wantCountFailed:       true,
			wantAction:            &failJob,
		},
		"successful containers are skipped by the rules": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpNotIn,
							Values:   []int32{111},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					InitContainerStatuses: []v1.ContainerStatus{
						{
							Name: "init-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 0,
								},
							},
						},
					},
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "main-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 111,
								},
							},
						},
						{
							Name: "support-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 0,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
		},
		"FailIndex rule matched for exit codes; JobBackoffLimitPerIndex enabled": {
			enableJobBackoffLimitPerIndex: true,
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailIndex,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2, 3},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantCountFailed: true,
			wantAction:      &failIndex,
		},
		"FailIndex rule matched for exit codes; JobBackoffLimitPerIndex disabled": {
			enableJobBackoffLimitPerIndex: false,
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailIndex,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2, 3},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantCountFailed: true,
			wantAction:      nil,
		},
		"pod failure policy with NotIn operator and value 0": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpNotIn,
							Values:   []int32{0},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "main-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 1,
								},
							},
						},
						{
							Name: "support-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 0,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: ptr.To("Container main-container for pod default/mypod failed with exit code 1 matching FailJob rule at index 0"),
			wantCountFailed:       true,
			wantAction:            &failJob,
		},
		"second jobfail rule matched for exit codes": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionCount,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2, 3},
						},
					},
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{4, 5, 6},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "main-container",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 6,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: ptr.To("Container main-container for pod default/mypod failed with exit code 6 matching FailJob rule at index 1"),
			wantCountFailed:       true,
			wantAction:            &failJob,
		},
		"count rule matched for exit codes": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionCount,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2, 3},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "foo",
						},
						{
							Name: "bar",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
			wantAction:            &count,
		},
		"FailIndex rule matched for pod conditions; JobBackoffLimitPerIndex enabled": {
			enableJobBackoffLimitPerIndex: true,
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailIndex,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			wantCountFailed: true,
			wantAction:      &failIndex,
		},
		"FailIndex rule matched for pod conditions; JobBackoffLimitPerIndex disabled": {
			enableJobBackoffLimitPerIndex: false,
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailIndex,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			wantCountFailed: true,
			wantAction:      nil,
		},
		"ignore rule matched for pod conditions": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       false,
			wantAction:            &ignore,
		},
		"ignore rule matches by the status=False": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       false,
			wantAction:            &ignore,
		},
		"ignore rule matches by the status=Unknown": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionUnknown,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionUnknown,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       false,
			wantAction:            &ignore,
		},
		"ignore rule does not match when status for pattern is False, but actual True": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
		},
		"ignore rule does not match when status for pattern is True, but actual False": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
		},
		"default - do not match condition with status=False": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
		},
		"job fail rule matched for pod conditions": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			wantJobFailureMessage: ptr.To("Pod default/mypod has condition DisruptionTarget matching FailJob rule at index 0"),
			wantCountFailed:       true,
			wantAction:            &failJob,
		},
		"count rule matched for pod conditions": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionCount,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.DisruptionTarget,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
			wantAction:            &count,
		},
		"no rule matched": {
			podFailurePolicy: &batch.PodFailurePolicy{
				Rules: []batch.PodFailurePolicyRule{
					{
						Action: batch.PodFailurePolicyActionCount,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpNotIn,
							Values:   []int32{8},
						},
					},
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{1, 2, 3},
						},
					},
					{
						Action: batch.PodFailurePolicyActionFailJob,
						OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
							Operator: batch.PodFailurePolicyOnExitCodesOpNotIn,
							Values:   []int32{5, 6, 7},
						},
					},
					{
						Action: batch.PodFailurePolicyActionCount,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.PodConditionType("ResourceLimitExceeded"),
								Status: v1.ConditionTrue,
							},
						},
					},
					{
						Action: batch.PodFailurePolicyActionIgnore,
						OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			failedPod: &v1.Pod{
				ObjectMeta: validPodObjectMeta,
				Status: v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 32,
								},
							},
						},
					},
				},
			},
			wantJobFailureMessage: nil,
			wantCountFailed:       true,
			wantAction:            &count,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, tc.enableJobBackoffLimitPerIndex)
			jobFailMessage, countFailed, action := matchPodFailurePolicy(tc.podFailurePolicy, tc.failedPod)
			if diff := cmp.Diff(tc.wantJobFailureMessage, jobFailMessage); diff != "" {
				t.Errorf("Unexpected job failure message: %s", diff)
			}
			if tc.wantCountFailed != countFailed {
				t.Errorf("Unexpected count failed. want: %v. got: %v", tc.wantCountFailed, countFailed)
			}
			if diff := cmp.Diff(tc.wantAction, action); diff != "" {
				t.Errorf("Unexpected failure policy action: %s", diff)
			}
		})
	}
}
