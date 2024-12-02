/*
Copyright 2023 The Kubernetes Authors.

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

package eventhandler

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-helpers/scheduling/corev1"
	configv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler"
	configtesting "k8s.io/kubernetes/pkg/scheduler/apis/config/testing"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	schedulerutils "k8s.io/kubernetes/test/integration/scheduler"
	testutils "k8s.io/kubernetes/test/integration/util"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

var lowPriority, mediumPriority, highPriority = int32(100), int32(200), int32(300)

var _ framework.FilterPlugin = &fooPlugin{}

type fooPlugin struct {
}

func (pl *fooPlugin) Name() string {
	return "foo"
}

func (pl *fooPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	taints := nodeInfo.Node().Spec.Taints
	if len(taints) == 0 {
		return nil
	}

	if corev1.TolerationsTolerateTaint(pod.Spec.Tolerations, &nodeInfo.Node().Spec.Taints[0]) {
		return nil
	}
	return framework.NewStatus(framework.Unschedulable)
}

func (pl *fooPlugin) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}},
	}, nil
}

// newPlugin returns a plugin factory with specified Plugin.
func newPlugin(plugin framework.Plugin) frameworkruntime.PluginFactory {
	return func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
		return plugin, nil
	}
}

func TestUpdateNodeEvent(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "test-event", nil)

	taints := []v1.Taint{{Key: v1.TaintNodeUnschedulable, Value: "", Effect: v1.TaintEffectNoSchedule}}
	nodeWrapper := st.MakeNode().Name("node-0").Label("kubernetes.io/hostname", "node-0").Taints(taints).Obj()
	podWrapper := testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testContext.NS.Name})
	fooPlugin := &fooPlugin{}

	registry := frameworkruntime.Registry{
		fooPlugin.Name(): newPlugin(fooPlugin),
	}

	// Setup plugins for testing.
	cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
		Profiles: []configv1.KubeSchedulerProfile{{
			SchedulerName: ptr.To[string](v1.DefaultSchedulerName),
			Plugins: &configv1.Plugins{
				Filter: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: fooPlugin.Name()},
					},
					Disabled: []configv1.Plugin{
						{Name: "*"},
					},
				},
			},
		}},
	})

	testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 0,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(registry),
	)
	defer teardown()

	node, err := testutils.CreateNode(testCtx.ClientSet, nodeWrapper)
	if err != nil {
		t.Fatalf("Creating node error: %v", err)
	}

	pod, err := testutils.CreatePausePod(testCtx.ClientSet, podWrapper)
	if err != nil {
		t.Fatalf("Creating pod error: %v", err)
	}

	if err := testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
		t.Fatalf("Pod %v got scheduled: %v", pod.Name, err)
	}
	node, err = testCtx.ClientSet.CoreV1().Nodes().Get(testCtx.Ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error while getting a node: %v", err)
	}

	// Update node label and node taints
	node.Labels["foo"] = "bar"
	node.Spec.Taints = nil

	_, err = testCtx.ClientSet.CoreV1().Nodes().Update(testCtx.Ctx, node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Error updating the node: %v", err)
	}

	if err := testutils.WaitForPodToSchedule(testCtx.ClientSet, pod); err != nil {
		t.Errorf("Pod %v was not scheduled: %v", pod.Name, err)
	}
}

func TestUpdateNominatedNodeName(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	testBackoff := time.Minute
	testContext := testutils.InitTestAPIServer(t, "test-event", nil)
	capacity := map[v1.ResourceName]string{
		v1.ResourceMemory: "32",
	}
	var cleanupPods []*v1.Pod

	testNode := st.MakeNode().Name("node-0").Label("kubernetes.io/hostname", "node-0").Capacity(capacity).Obj()
	// Note that the low priority pod that cannot fit with the mid priority, but can fit with the high priority one.
	podLow := testutils.InitPausePod(&testutils.PausePodConfig{
		Name:      "test-lp-pod",
		Namespace: testContext.NS.Name,
		Priority:  &lowPriority,
		Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(20, resource.DecimalSI)},
		}})
	cleanupPods = append(cleanupPods, podLow)
	podMidNominated := testutils.InitPausePod(&testutils.PausePodConfig{
		Name:      "test-nominated-pod",
		Namespace: testContext.NS.Name,
		Priority:  &mediumPriority,
		Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(25, resource.DecimalSI)},
		}})
	cleanupPods = append(cleanupPods, podMidNominated)
	podHigh := testutils.InitPausePod(&testutils.PausePodConfig{
		Name:      "test-hp-pod",
		Namespace: testContext.NS.Name,
		Priority:  &highPriority,
		Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(10, resource.DecimalSI)},
		}})
	cleanupPods = append(cleanupPods, podHigh)

	tests := []struct {
		name       string
		updateFunc func(testCtx *testutils.TestContext)
	}{
		{
			name: "Preempt nominated pod",
			updateFunc: func(testCtx *testutils.TestContext) {
				// Create high-priority pod and wait until it's scheduled (unnominate mid-priority pod)
				pod, err := testutils.CreatePausePod(testCtx.ClientSet, podHigh)
				if err != nil {
					t.Fatalf("Creating pod error: %v", err)
				}
				if err = testutils.WaitForPodToSchedule(testCtx.ClientSet, pod); err != nil {
					t.Fatalf("Pod %v was not scheduled: %v", pod.Name, err)
				}
			},
		},
		{
			name: "Remove nominated pod",
			updateFunc: func(testCtx *testutils.TestContext) {
				testutils.DeletePod(testCtx.ClientSet, podMidNominated.Name, podMidNominated.Namespace)
			},
		},
	}

	for _, tt := range tests {
		for _, qHintEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, with queuehint(%v)", tt.name, qHintEnabled), func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, qHintEnabled)

				testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 0,
					scheduler.WithClock(fakeClock),
					// UpdateFunc needs to be called when the nominated pod is still in the backoff queue, thus small, but non 0 value.
					scheduler.WithPodInitialBackoffSeconds(int64(testBackoff.Seconds())),
					scheduler.WithPodMaxBackoffSeconds(int64(testBackoff.Seconds())),
				)
				defer teardown()

				_, err := testutils.CreateNode(testCtx.ClientSet, testNode)
				if err != nil {
					t.Fatalf("Creating node error: %v", err)
				}

				// Create initial low-priority pod and wait until it's scheduled.
				pod, err := testutils.CreatePausePod(testCtx.ClientSet, podLow)
				if err != nil {
					t.Fatalf("Creating pod error: %v", err)
				}
				if err := testutils.WaitForPodToSchedule(testCtx.ClientSet, pod); err != nil {
					t.Fatalf("Pod %v was not scheduled: %v", pod.Name, err)
				}

				// Create mid-priority pod and wait until it becomes nominated (preempt low-priority pod) and remain uschedulable.
				pod, err = testutils.CreatePausePod(testCtx.ClientSet, podMidNominated)
				if err != nil {
					t.Fatalf("Creating pod error: %v", err)
				}
				if err := testutils.WaitForNominatedNodeName(testCtx.ClientSet, pod); err != nil {
					t.Errorf("NominatedNodeName field was not set for pod %v: %v", pod.Name, err)
				}
				if err := testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Pod %v haven't become unschedulabe: %v", pod.Name, err)
				}

				// Remove the initial, which will move the nominated unschedulable back to the backoff queue.
				testutils.DeletePod(testCtx.ClientSet, podLow.Name, podLow.Namespace)

				// Create another low-priority pods which cannot be scheduled due to resource constraints.
				pod, err = testutils.CreatePausePod(testCtx.ClientSet, podLow)
				if err != nil {
					t.Fatalf("Creating pod error: %v", err)
				}
				if err := testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Fatalf("Pod %v was not scheduled: %v", pod.Name, err)
				}

				// Update causing the nominated pod to be removed, which should trigger scheduling of the low priority pod.
				// Note that the update has to happen since the nominated pod is still in the backoffQ to actually test updates of nominated, but not bound yet pods.
				tt.updateFunc(testCtx)

				// Advance time by the maxPodBackoffSeconds to move low priority pod out of the backoff queue.
				fakeClock.Step(testBackoff)

				// Expect the low-priority pod is notified about unnominated mid-pririty pod and gets scheduled, as it should fit this time.
				if err := testutils.WaitForPodToSchedule(testCtx.ClientSet, podLow); err != nil {
					t.Fatalf("Pod %v was not scheduled: %v", podLow.Name, err)
				}
				testutils.CleanupPods(testCtx.Ctx, testCtx.ClientSet, t, cleanupPods)
			})
		}
	}
}
