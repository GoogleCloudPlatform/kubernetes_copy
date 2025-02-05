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
	"strings"
	"testing"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/integration/scheduler/queueing"
)

// TestCoreResourceEnqueue verify Pods failed by in-tree default plugins can be
// moved properly upon their registered events.
// Here, we run only the test cases where the EnableSchedulingQueueHint is enabled.
func TestCoreResourceEnqueue(t *testing.T) {
	for _, tt := range queueing.CoreResourceEnqueueTestCases {
		if tt.EnableSchedulingQueueHint != nil && !tt.EnableSchedulingQueueHint.Has(true) {
			continue
		}
		// Note: if EnableSchedulingQueueHint is nil, we assume the test should be run both with/without the feature gate.
		t.Run(strings.Join(append(tt.EnablePlugins, tt.Name), "/"), func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, true)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DynamicResourceAllocation, true)
			queueing.RunTestCoreResourceEnqueue(t, tt)
		})
	}
}
