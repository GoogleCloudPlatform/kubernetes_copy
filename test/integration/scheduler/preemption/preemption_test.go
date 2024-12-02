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

// This file tests preemption functionality of the scheduler.

package preemption

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
	configv1 "k8s.io/kube-scheduler/config/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	configtesting "k8s.io/kubernetes/pkg/scheduler/apis/config/testing"
	"k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	plfeature "k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/volumerestrictions"
	"k8s.io/kubernetes/pkg/scheduler/framework/preemption"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/plugin/pkg/admission/priority"
	testutils "k8s.io/kubernetes/test/integration/util"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
)

// imported from testutils
var (
	initPausePod                    = testutils.InitPausePod
	createNode                      = testutils.CreateNode
	createPausePod                  = testutils.CreatePausePod
	runPausePod                     = testutils.RunPausePod
	deletePod                       = testutils.DeletePod
	initTest                        = testutils.InitTestSchedulerWithNS
	initTestDisablePreemption       = testutils.InitTestDisablePreemption
	initDisruptionController        = testutils.InitDisruptionController
	waitCachedPodsStable            = testutils.WaitCachedPodsStable
	podIsGettingEvicted             = testutils.PodIsGettingEvicted
	podUnschedulable                = testutils.PodUnschedulable
	waitForPDBsStable               = testutils.WaitForPDBsStable
	waitForPodToScheduleWithTimeout = testutils.WaitForPodToScheduleWithTimeout
	waitForPodUnschedulable         = testutils.WaitForPodUnschedulable
)

const filterPluginName = "filter-plugin"

var lowPriority, mediumPriority, highPriority = int32(100), int32(200), int32(300)

const tokenFilterName = "token-filter"

// tokenFilter is a fake plugin that implements PreFilter and Filter.
// `Token` simulates the allowed pods number a cluster can host.
// If `EnablePreFilter` is set to false or `Token` is positive, PreFilter passes; otherwise returns Unschedulable
// For each Filter() call, `Token` is decreased by one. When `Token` is positive, Filter passes; otherwise return
// Unschedulable or UnschedulableAndUnresolvable (when `Unresolvable` is set to true)
// AddPod()/RemovePod() adds/removes one token to the cluster to simulate the dryrun preemption
type tokenFilter struct {
	Tokens          int
	Unresolvable    bool
	EnablePreFilter bool
}

// Name returns name of the plugin.
func (fp *tokenFilter) Name() string {
	return tokenFilterName
}

func (fp *tokenFilter) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod,
	nodeInfo *framework.NodeInfo) *framework.Status {
	if fp.Tokens > 0 {
		fp.Tokens--
		return nil
	}
	status := framework.Unschedulable
	if fp.Unresolvable {
		status = framework.UnschedulableAndUnresolvable
	}
	return framework.NewStatus(status, fmt.Sprintf("can't fit %v", pod.Name))
}

func (fp *tokenFilter) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	if !fp.EnablePreFilter || fp.Tokens > 0 {
		return nil, nil
	}
	return nil, framework.NewStatus(framework.Unschedulable)
}

func (fp *tokenFilter) AddPod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod,
	podInfoToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	fp.Tokens--
	return nil
}

func (fp *tokenFilter) RemovePod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod,
	podInfoToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	fp.Tokens++
	return nil
}

func (fp *tokenFilter) PreFilterExtensions() framework.PreFilterExtensions {
	return fp
}

var _ framework.FilterPlugin = &tokenFilter{}

// TestPreemption tests a few preemption scenarios.
func TestPreemption(t *testing.T) {
	// Initialize scheduler with a filter plugin.
	var filter tokenFilter
	registry := make(frameworkruntime.Registry)
	err := registry.Register(filterPluginName, func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
		return &filter, nil
	})
	if err != nil {
		t.Fatalf("Error registering a filter: %v", err)
	}
	cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
		Profiles: []configv1.KubeSchedulerProfile{{
			SchedulerName: pointer.String(v1.DefaultSchedulerName),
			Plugins: &configv1.Plugins{
				Filter: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: filterPluginName},
					},
				},
				PreFilter: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: filterPluginName},
					},
				},
			},
		}},
	})

	testCtx := testutils.InitTestSchedulerWithOptions(t,
		testutils.InitTestAPIServer(t, "preemption", nil),
		0,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(registry))
	testutils.SyncSchedulerInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)

	cs := testCtx.ClientSet

	defaultPodRes := &v1.ResourceRequirements{Requests: v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI)},
	}

	maxTokens := 1000
	tests := []struct {
		name                string
		existingPods        []*v1.Pod
		pod                 *v1.Pod
		initTokens          int
		enablePreFilter     bool
		unresolvable        bool
		preemptedPodIndexes map[int]struct{}
	}{
		{
			name:       "basic pod preemption",
			initTokens: maxTokens,
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(400, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
					},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(300, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{0: {}},
		},
		{
			name:       "basic pod preemption with filter",
			initTokens: 1,
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
					},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{0: {}},
		},
		// This is identical with previous subtest except for setting enablePreFilter to true.
		// With this fake plugin returning Unschedulable in PreFilter, it's able to exercise the path
		// that in-tree plugins return Skip in PreFilter and their AddPod/RemovePod functions are also
		// skipped properly upon preemption.
		{
			name:            "basic pod preemption with preFilter",
			initTokens:      1,
			enablePreFilter: true,
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
					},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{0: {}},
		},
		{
			// same as the previous test, but the filter is unresolvable.
			name:         "basic pod preemption with unresolvable filter",
			initTokens:   1,
			unresolvable: true,
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
					},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{},
		},
		{
			name:       "preemption is performed to satisfy anti-affinity",
			initTokens: maxTokens,
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name: "pod-0", Namespace: testCtx.NS.Name,
					Priority:  &mediumPriority,
					Labels:    map[string]string{"pod": "p0"},
					Resources: defaultPodRes,
				}),
				initPausePod(&testutils.PausePodConfig{
					Name: "pod-1", Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Labels:    map[string]string{"pod": "p1"},
					Resources: defaultPodRes,
					Affinity: &v1.Affinity{
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "pod",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"preemptor"},
											},
										},
									},
									TopologyKey: "node",
								},
							},
						},
					},
				}),
			},
			// A higher priority pod with anti-affinity.
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Labels:    map[string]string{"pod": "preemptor"},
				Resources: defaultPodRes,
				Affinity: &v1.Affinity{
					PodAntiAffinity: &v1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "pod",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"p0"},
										},
									},
								},
								TopologyKey: "node",
							},
						},
					},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{0: {}, 1: {}},
		},
		{
			// This is similar to the previous case only pod-1 is high priority.
			name:       "preemption is not performed when anti-affinity is not satisfied",
			initTokens: maxTokens,
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name: "pod-0", Namespace: testCtx.NS.Name,
					Priority:  &mediumPriority,
					Labels:    map[string]string{"pod": "p0"},
					Resources: defaultPodRes,
				}),
				initPausePod(&testutils.PausePodConfig{
					Name: "pod-1", Namespace: testCtx.NS.Name,
					Priority:  &highPriority,
					Labels:    map[string]string{"pod": "p1"},
					Resources: defaultPodRes,
					Affinity: &v1.Affinity{
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "pod",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"preemptor"},
											},
										},
									},
									TopologyKey: "node",
								},
							},
						},
					},
				}),
			},
			// A higher priority pod with anti-affinity.
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Labels:    map[string]string{"pod": "preemptor"},
				Resources: defaultPodRes,
				Affinity: &v1.Affinity{
					PodAntiAffinity: &v1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "pod",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"p0"},
										},
									},
								},
								TopologyKey: "node",
							},
						},
					},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{},
		},
	}

	// Create a node with some resources and a label.
	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	nodeObject := st.MakeNode().Name("node1").Capacity(nodeRes).Label("node", "node1").Obj()
	if _, err := createNode(testCtx.ClientSet, nodeObject); err != nil {
		t.Fatalf("Error creating node: %v", err)
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerAsyncPreemption, asyncPreemptionEnabled)

				filter.Tokens = test.initTokens
				filter.EnablePreFilter = test.enablePreFilter
				filter.Unresolvable = test.unresolvable
				pods := make([]*v1.Pod, len(test.existingPods))
				// Create and run existingPods.
				for i, p := range test.existingPods {
					pods[i], err = runPausePod(cs, p)
					if err != nil {
						t.Fatalf("Error running pause pod: %v", err)
					}
				}
				// Create the "pod".
				preemptor, err := createPausePod(cs, test.pod)
				if err != nil {
					t.Errorf("Error while creating high priority pod: %v", err)
				}
				// Wait for preemption of pods and make sure the other ones are not preempted.
				for i, p := range pods {
					if _, found := test.preemptedPodIndexes[i]; found {
						if err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Second, wait.ForeverTestTimeout, false,
							podIsGettingEvicted(cs, p.Namespace, p.Name)); err != nil {
							t.Errorf("Pod %v/%v is not getting evicted.", p.Namespace, p.Name)
						}
						pod, err := cs.CoreV1().Pods(p.Namespace).Get(testCtx.Ctx, p.Name, metav1.GetOptions{})
						if err != nil {
							t.Errorf("Error %v when getting the updated status for pod %v/%v ", err, p.Namespace, p.Name)
						}
						_, cond := podutil.GetPodCondition(&pod.Status, v1.DisruptionTarget)
						if cond == nil {
							t.Errorf("Pod %q does not have the expected condition: %q", klog.KObj(pod), v1.DisruptionTarget)
						}
					} else if p.DeletionTimestamp != nil {
						t.Errorf("Didn't expect pod %v to get preempted.", p.Name)
					}
				}
				// Also check that the preemptor pod gets the NominatedNodeName field set.
				if len(test.preemptedPodIndexes) > 0 {
					if err := testutils.WaitForNominatedNodeName(cs, preemptor); err != nil {
						t.Errorf("NominatedNodeName field was not set for pod %v: %v", preemptor.Name, err)
					}
				}

				// Cleanup
				pods = append(pods, preemptor)
				testutils.CleanupPods(testCtx.Ctx, cs, t, pods)
			})
		}
	}
}

func TestAsyncPreemption(t *testing.T) {
	type createPod struct {
		pod *v1.Pod
		// count is the number of times the pod should be created by this action.
		// i.e., if you use it, you have to use GenerateName.
		// By default, it's 1.
		count *int
	}

	type schedulePod struct {
		podName       string
		expectSuccess bool
	}

	type scenario struct {
		// name is this step's name, just for the debugging purpose.
		name string

		// Only one of the following actions should be set.

		// createPod creates a Pod.
		createPod *createPod
		// schedulePod schedules one Pod that is at the top of the activeQ.
		// You should give a Pod name that is supposed to be scheduled.
		schedulePod *schedulePod
		// completePreemption completes the preemption that is currently on-going.
		// You should give a Pod name.
		completePreemption string
		// podGatedInQueue checks if the given Pod is in the scheduling queue and gated by the preemption.
		// You should give a Pod name.
		podGatedInQueue string
		// podRunningPreemption checks if the given Pod is running preemption.
		// You should give a Pod index representing the order of Pod creation.
		// e.g., if you want to check the Pod created first in the test case, you should give 0.
		podRunningPreemption *int
	}

	tests := []struct {
		name string
		// scenarios after the first attempt of scheduling the pod.
		scenarios []scenario
	}{
		{
			// Very basic test case: if it fails, the basic scenario is broken somewhere.
			name: "basic: async preemption happens expectedly",
			scenarios: []scenario{
				{
					name: "create scheduled Pod",
					createPod: &createPod{
						pod:   st.MakePod().GenerateName("victim-").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Node("node").Container("image").ZeroTerminationGracePeriod().Priority(1).Obj(),
						count: ptr.To(2),
					},
				},
				{
					name: "create a preemptor Pod",
					createPod: &createPod{
						pod: st.MakePod().Name("preemptor").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Priority(100).Obj(),
					},
				},
				{
					name: "schedule the preemptor Pod",
					schedulePod: &schedulePod{
						podName: "preemptor",
					},
				},
				{
					name:            "check the pod is in the queue and gated",
					podGatedInQueue: "preemptor",
				},
				{
					name:                 "check the preemptor Pod making the preemption API calls",
					podRunningPreemption: ptr.To(2),
				},
				{
					name:               "complete the preemption API calls",
					completePreemption: "preemptor",
				},
				{
					name: "schedule the preemptor Pod after the preemption",
					schedulePod: &schedulePod{
						podName:       "preemptor",
						expectSuccess: true,
					},
				},
			},
		},
		{
			name: "Lower priority Pod doesn't take over the place for higher priority Pod that is running the preemption",
			scenarios: []scenario{
				{
					name: "create scheduled Pod",
					createPod: &createPod{
						pod:   st.MakePod().GenerateName("victim-").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Node("node").Container("image").ZeroTerminationGracePeriod().Priority(1).Obj(),
						count: ptr.To(2),
					},
				},
				{
					name: "create a preemptor Pod",
					createPod: &createPod{
						pod: st.MakePod().Name("preemptor-high-priority").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Priority(100).Obj(),
					},
				},
				{
					name: "schedule the preemptor Pod",
					schedulePod: &schedulePod{
						podName: "preemptor-high-priority",
					},
				},
				{
					name:            "check the pod is in the queue and gated",
					podGatedInQueue: "preemptor-high-priority",
				},
				{
					name:                 "check the preemptor Pod making the preemption API calls",
					podRunningPreemption: ptr.To(2),
				},
				{
					// This Pod is lower priority than the preemptor Pod.
					// Given the preemptor Pod is nominated to the node, this Pod should be unschedulable.
					name: "create a second Pod that is lower priority than the first preemptor Pod",
					createPod: &createPod{
						pod: st.MakePod().Name("pod-mid-priority").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Priority(50).Obj(),
					},
				},
				{
					name: "schedule the mid-priority Pod",
					schedulePod: &schedulePod{
						podName: "pod-mid-priority",
					},
				},
				{
					name:               "complete the preemption API calls",
					completePreemption: "preemptor-high-priority",
				},
				{
					// the preemptor pod should be popped from the queue before the mid-priority pod.
					name: "schedule the preemptor Pod again",
					schedulePod: &schedulePod{
						podName:       "preemptor-high-priority",
						expectSuccess: true,
					},
				},
				{
					name: "schedule the mid-priority Pod again",
					schedulePod: &schedulePod{
						podName: "pod-mid-priority",
					},
				},
			},
		},
		{
			name: "Higher priority Pod takes over the place for lower priority Pod that is running the preemption",
			scenarios: []scenario{
				{
					name: "create scheduled Pod",
					createPod: &createPod{
						pod:   st.MakePod().GenerateName("victim-").Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Node("node").Container("image").ZeroTerminationGracePeriod().Priority(1).Obj(),
						count: ptr.To(4),
					},
				},
				{
					name: "create a preemptor Pod",
					createPod: &createPod{
						pod: st.MakePod().Name("preemptor-high-priority").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Priority(100).Obj(),
					},
				},
				{
					name: "schedule the preemptor Pod",
					schedulePod: &schedulePod{
						podName: "preemptor-high-priority",
					},
				},
				{
					name:            "check the pod is in the queue and gated",
					podGatedInQueue: "preemptor-high-priority",
				},
				{
					name:                 "check the preemptor Pod making the preemption API calls",
					podRunningPreemption: ptr.To(4),
				},
				{
					// This Pod is higher priority than the preemptor Pod.
					// Even though the preemptor Pod is nominated to the node, this Pod can take over the place.
					name: "create a second Pod that is higher priority than the first preemptor Pod",
					createPod: &createPod{
						pod: st.MakePod().Name("preemptor-super-high-priority").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Container("image").Priority(200).Obj(),
					},
				},
				{
					name: "schedule the super-high-priority Pod",
					schedulePod: &schedulePod{
						podName: "preemptor-super-high-priority",
					},
				},
				{
					name:                 "check the super-high-priority Pod making the preemption API calls",
					podRunningPreemption: ptr.To(5),
				},
				{
					// the super-high-priority preemptor should enter the preemption
					// and select the place where the preemptor-high-priority selected.
					// So, basically both goroutines are preempting the same Pods.
					name:            "check the super-high-priority pod is in the queue and gated",
					podGatedInQueue: "preemptor-super-high-priority",
				},
				{
					name:               "complete the preemption API calls of super-high-priority",
					completePreemption: "preemptor-super-high-priority",
				},
				{
					name:               "complete the preemption API calls of high-priority",
					completePreemption: "preemptor-high-priority",
				},
				{
					name: "schedule the super-high-priority Pod",
					schedulePod: &schedulePod{
						podName:       "preemptor-super-high-priority",
						expectSuccess: true,
					},
				},
				{
					name: "schedule the high-priority Pod",
					schedulePod: &schedulePod{
						podName: "preemptor-high-priority",
					},
				},
			},
		},
		{
			name: "Lower priority Pod can select the same place where the higher priority Pod is preempting if the node is big enough",
			scenarios: []scenario{
				{
					name: "create scheduled Pod",
					createPod: &createPod{
						pod:   st.MakePod().GenerateName("victim-").Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Node("node").Container("image").ZeroTerminationGracePeriod().Priority(1).Obj(),
						count: ptr.To(4),
					},
				},
				{
					// It will preempt two victims.
					name: "create a preemptor Pod",
					createPod: &createPod{
						pod: st.MakePod().Name("preemptor-high-priority").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Container("image").Priority(100).Obj(),
					},
				},
				{
					name: "schedule the preemptor Pod",
					schedulePod: &schedulePod{
						podName: "preemptor-high-priority",
					},
				},
				{
					name:            "check the pod is in the queue and gated",
					podGatedInQueue: "preemptor-high-priority",
				},
				{
					name:                 "check the preemptor Pod making the preemption API calls",
					podRunningPreemption: ptr.To(4),
				},
				{
					// This Pod is lower priority than the preemptor Pod.
					// Given the preemptor Pod is nominated to the node, this Pod should be unschedulable.
					// This Pod will trigger the preemption to target the two victims that the first Pod doesn't target.
					name: "create a second Pod that is lower priority than the first preemptor Pod",
					createPod: &createPod{
						pod: st.MakePod().Name("preemptor-mid-priority").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Container("image").Priority(50).Obj(),
					},
				},
				{
					name: "schedule the mid-priority Pod",
					schedulePod: &schedulePod{
						podName: "preemptor-mid-priority",
					},
				},
				{
					name:            "check the mid-priority pod is in the queue and gated",
					podGatedInQueue: "preemptor-mid-priority",
				},
				{
					name:                 "check the mid-priority Pod making the preemption API calls",
					podRunningPreemption: ptr.To(5),
				},
				{
					name:               "complete the preemption API calls",
					completePreemption: "preemptor-mid-priority",
				},
				{
					name:               "complete the preemption API calls",
					completePreemption: "preemptor-high-priority",
				},
				{
					// the preemptor pod should be popped from the queue before the mid-priority pod.
					name: "schedule the preemptor Pod again",
					schedulePod: &schedulePod{
						podName:       "preemptor-high-priority",
						expectSuccess: true,
					},
				},
				{
					name: "schedule the mid-priority Pod again",
					schedulePod: &schedulePod{
						podName:       "preemptor-mid-priority",
						expectSuccess: true,
					},
				},
			},
		},
	}

	// All test cases have the same node.
	node := st.MakeNode().Name("node").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// We need to use a custom preemption plugin to test async preemption behavior
			delayedPreemptionPluginName := "delay-preemption"
			// keyed by the pod name
			preemptionDoneChannels := make(map[string]chan struct{})
			defer func() {
				for _, ch := range preemptionDoneChannels {
					close(ch)
				}
			}()
			registry := make(frameworkruntime.Registry)
			var preemptionPlugin *defaultpreemption.DefaultPreemption
			err := registry.Register(delayedPreemptionPluginName, func(c context.Context, r runtime.Object, fh framework.Handle) (framework.Plugin, error) {
				p, err := frameworkruntime.FactoryAdapter(plfeature.Features{EnableAsyncPreemption: true}, defaultpreemption.New)(c, &config.DefaultPreemptionArgs{
					// Set default values to pass the validation at the initialization, not related to the test.
					MinCandidateNodesPercentage: 10,
					MinCandidateNodesAbsolute:   100,
				}, fh)
				if err != nil {
					return nil, fmt.Errorf("error creating default preemption plugin: %w", err)
				}

				var ok bool
				preemptionPlugin, ok = p.(*defaultpreemption.DefaultPreemption)
				if !ok {
					return nil, fmt.Errorf("unexpected plugin type %T", p)
				}

				preemptPodFn := preemptionPlugin.Evaluator.PreemptPod
				preemptionPlugin.Evaluator.PreemptPod = func(ctx context.Context, c preemption.Candidate, preemptor, victim *v1.Pod, pluginName string) error {
					// block the preemption goroutine to complete until the test case allows it to proceed.
					if ch, ok := preemptionDoneChannels[preemptor.Name]; ok {
						<-ch
					}
					return preemptPodFn(ctx, c, preemptor, victim, pluginName)
				}

				return preemptionPlugin, nil
			})
			if err != nil {
				t.Fatalf("Error registering a filter: %v", err)
			}
			cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
				Profiles: []configv1.KubeSchedulerProfile{{
					SchedulerName: pointer.String(v1.DefaultSchedulerName),
					Plugins: &configv1.Plugins{
						MultiPoint: configv1.PluginSet{
							Enabled: []configv1.Plugin{
								{Name: delayedPreemptionPluginName},
							},
							Disabled: []configv1.Plugin{
								{Name: names.DefaultPreemption},
							},
						},
					},
				}},
			})

			// It initializes the scheduler, but doesn't start.
			// We manually trigger the scheduling cycle.
			testCtx := testutils.InitTestSchedulerWithOptions(t,
				testutils.InitTestAPIServer(t, "preemption", nil),
				0,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(registry),
				// disable backoff
				scheduler.WithPodMaxBackoffSeconds(0),
				scheduler.WithPodInitialBackoffSeconds(0),
			)
			testutils.SyncSchedulerInformerFactory(testCtx)
			cs := testCtx.ClientSet

			if preemptionPlugin == nil {
				t.Fatalf("the preemption plugin should be initialized")
			}

			logger, _ := ktesting.NewTestContext(t)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerAsyncPreemption, true)

			createdPods := []*v1.Pod{}
			defer testutils.CleanupPods(testCtx.Ctx, cs, t, createdPods)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if _, err := cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Failed to create an initial Node %q: %v", node.Name, err)
			}
			defer func() {
				if err := cs.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatalf("Failed to delete the Node %q: %v", node.Name, err)
				}
			}()

			for _, scenario := range test.scenarios {
				t.Logf("Running scenario: %s", scenario.name)
				switch {
				case scenario.createPod != nil:
					if scenario.createPod.count == nil {
						scenario.createPod.count = ptr.To(1)
					}

					for i := 0; i < *scenario.createPod.count; i++ {
						pod, err := cs.CoreV1().Pods(testCtx.NS.Name).Create(ctx, scenario.createPod.pod, metav1.CreateOptions{})
						if err != nil {
							t.Fatalf("Failed to create a Pod %q: %v", pod.Name, err)
						}
						createdPods = append(createdPods, pod)
					}
				case scenario.schedulePod != nil:
					lastFailure := ""
					if err := wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
						if len(testCtx.Scheduler.SchedulingQueue.PodsInActiveQ()) == 0 {
							lastFailure = fmt.Sprintf("Expected the pod %s to be scheduled, but no pod arrives at the activeQ", scenario.schedulePod.podName)
							return false, nil
						}

						if testCtx.Scheduler.SchedulingQueue.PodsInActiveQ()[0].Name != scenario.schedulePod.podName {
							// need to wait more because maybe the queue will get another Pod that higher priority than the current top pod.
							lastFailure = fmt.Sprintf("The pod %s is expected to be scheduled, but the top Pod is %s", scenario.schedulePod.podName, testCtx.Scheduler.SchedulingQueue.PodsInActiveQ()[0].Name)
							return false, nil
						}

						return true, nil
					}); err != nil {
						t.Fatal(lastFailure)
					}

					preemptionDoneChannels[scenario.schedulePod.podName] = make(chan struct{})
					testCtx.Scheduler.ScheduleOne(testCtx.Ctx)
					if scenario.schedulePod.expectSuccess {
						if err := wait.PollUntilContextTimeout(testCtx.Ctx, 200*time.Millisecond, wait.ForeverTestTimeout, false, testutils.PodScheduled(cs, testCtx.NS.Name, scenario.schedulePod.podName)); err != nil {
							t.Fatalf("Expected the pod %s to be scheduled", scenario.schedulePod.podName)
						}
					} else {
						if !podInUnschedulablePodPool(t, testCtx.Scheduler.SchedulingQueue, scenario.schedulePod.podName) {
							t.Fatalf("Expected the pod %s to be in the queue after the scheduling attempt", scenario.schedulePod.podName)
						}
					}
				case scenario.completePreemption != "":
					if _, ok := preemptionDoneChannels[scenario.completePreemption]; !ok {
						t.Fatalf("The preemptor Pod %q is not running preemption", scenario.completePreemption)
					}

					close(preemptionDoneChannels[scenario.completePreemption])
					delete(preemptionDoneChannels, scenario.completePreemption)
				case scenario.podGatedInQueue != "":
					// make sure the Pod is in the queue in the first place.
					if !podInUnschedulablePodPool(t, testCtx.Scheduler.SchedulingQueue, scenario.podGatedInQueue) {
						t.Fatalf("Expected the pod %s to be in the queue", scenario.podGatedInQueue)
					}

					// Make sure this Pod is gated by the preemption at PreEnqueue extension point
					// by activating the Pod and see if it's still in the unsched pod pool.
					testCtx.Scheduler.SchedulingQueue.Activate(logger, map[string]*v1.Pod{scenario.podGatedInQueue: st.MakePod().Namespace(testCtx.NS.Name).Name(scenario.podGatedInQueue).Obj()})
					if !podInUnschedulablePodPool(t, testCtx.Scheduler.SchedulingQueue, scenario.podGatedInQueue) {
						t.Fatalf("Expected the pod %s to be in the queue even after the activation", scenario.podGatedInQueue)
					}
				case scenario.podRunningPreemption != nil:
					if err := wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
						return preemptionPlugin.Evaluator.IsPodRunningPreemption(createdPods[*scenario.podRunningPreemption].GetUID()), nil
					}); err != nil {
						t.Fatalf("Expected the pod %s to be running preemption", createdPods[*scenario.podRunningPreemption].Name)
					}
				}
			}
		})
	}
}

// podInUnschedulablePodPool checks if the given Pod is in the unschedulable pod pool.
func podInUnschedulablePodPool(t *testing.T, queue queue.SchedulingQueue, podName string) bool {
	t.Helper()
	// First, look for the pod in the activeQ.
	for _, pod := range queue.PodsInActiveQ() {
		if pod.Name == podName {
			return false
		}
	}

	pendingPods, _ := queue.PendingPods()
	for _, pod := range pendingPods {
		if pod.Name == podName {
			return true
		}
	}
	return false
}

// TestNonPreemption tests NonPreempt option of PriorityClass of scheduler works as expected.
func TestNonPreemption(t *testing.T) {
	var preemptNever = v1.PreemptNever
	// Initialize scheduler.
	testCtx := initTest(t, "non-preemption")
	cs := testCtx.ClientSet
	tests := []struct {
		name             string
		PreemptionPolicy *v1.PreemptionPolicy
	}{
		{
			name:             "pod preemption will happen",
			PreemptionPolicy: nil,
		},
		{
			name:             "pod preemption will not happen",
			PreemptionPolicy: &preemptNever,
		},
	}
	victim := initPausePod(&testutils.PausePodConfig{
		Name:      "victim-pod",
		Namespace: testCtx.NS.Name,
		Priority:  &lowPriority,
		Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
			v1.ResourceCPU:    *resource.NewMilliQuantity(400, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
		},
	})

	preemptor := initPausePod(&testutils.PausePodConfig{
		Name:      "preemptor-pod",
		Namespace: testCtx.NS.Name,
		Priority:  &highPriority,
		Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
			v1.ResourceCPU:    *resource.NewMilliQuantity(300, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
		},
	})

	// Create a node with some resources
	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	_, err := createNode(testCtx.ClientSet, st.MakeNode().Name("node1").Capacity(nodeRes).Obj())
	if err != nil {
		t.Fatalf("Error creating nodes: %v", err)
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				defer testutils.CleanupPods(testCtx.Ctx, cs, t, []*v1.Pod{preemptor, victim})
				preemptor.Spec.PreemptionPolicy = test.PreemptionPolicy
				victimPod, err := createPausePod(cs, victim)
				if err != nil {
					t.Fatalf("Error while creating victim: %v", err)
				}
				if err := waitForPodToScheduleWithTimeout(cs, victimPod, 5*time.Second); err != nil {
					t.Fatalf("victim %v should be become scheduled", victimPod.Name)
				}

				preemptorPod, err := createPausePod(cs, preemptor)
				if err != nil {
					t.Fatalf("Error while creating preemptor: %v", err)
				}

				err = testutils.WaitForNominatedNodeNameWithTimeout(cs, preemptorPod, 5*time.Second)
				// test.PreemptionPolicy == nil means we expect the preemptor to be nominated.
				expect := test.PreemptionPolicy == nil
				// err == nil indicates the preemptor is indeed nominated.
				got := err == nil
				if got != expect {
					t.Errorf("Expect preemptor to be nominated=%v, but got=%v", expect, got)
				}
			})
		}
	}
}

// TestDisablePreemption tests disable pod preemption of scheduler works as expected.
func TestDisablePreemption(t *testing.T) {
	// Initialize scheduler, and disable preemption.
	testCtx := initTestDisablePreemption(t, "disable-preemption")
	cs := testCtx.ClientSet

	tests := []struct {
		name         string
		existingPods []*v1.Pod
		pod          *v1.Pod
	}{
		{
			name: "pod preemption will not happen",
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(400, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
					},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(300, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
		},
	}

	// Create a node with some resources
	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	_, err := createNode(testCtx.ClientSet, st.MakeNode().Name("node1").Capacity(nodeRes).Obj())
	if err != nil {
		t.Fatalf("Error creating nodes: %v", err)
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				pods := make([]*v1.Pod, len(test.existingPods))
				// Create and run existingPods.
				for i, p := range test.existingPods {
					pods[i], err = runPausePod(cs, p)
					if err != nil {
						t.Fatalf("Test [%v]: Error running pause pod: %v", test.name, err)
					}
				}
				// Create the "pod".
				preemptor, err := createPausePod(cs, test.pod)
				if err != nil {
					t.Errorf("Error while creating high priority pod: %v", err)
				}
				// Ensure preemptor should keep unschedulable.
				if err := waitForPodUnschedulable(testCtx.Ctx, cs, preemptor); err != nil {
					t.Errorf("Preemptor %v should not become scheduled", preemptor.Name)
				}

				// Ensure preemptor should not be nominated.
				if err := testutils.WaitForNominatedNodeNameWithTimeout(cs, preemptor, 5*time.Second); err == nil {
					t.Errorf("Preemptor %v should not be nominated", preemptor.Name)
				}

				// Cleanup
				pods = append(pods, preemptor)
				testutils.CleanupPods(testCtx.Ctx, cs, t, pods)
			})
		}
	}
}

// This test verifies that system critical priorities are created automatically and resolved properly.
func TestPodPriorityResolution(t *testing.T) {
	admission := priority.NewPlugin()
	testCtx := testutils.InitTestScheduler(t, testutils.InitTestAPIServer(t, "preemption", admission))
	cs := testCtx.ClientSet

	// Build clientset and informers for controllers.
	externalClientConfig := restclient.CopyConfig(testCtx.KubeConfig)
	externalClientConfig.QPS = -1
	externalClientset := clientset.NewForConfigOrDie(externalClientConfig)
	externalInformers := informers.NewSharedInformerFactory(externalClientset, time.Second)
	admission.SetExternalKubeClientSet(externalClientset)
	admission.SetExternalKubeInformerFactory(externalInformers)

	// Waiting for all controllers to sync
	testutils.SyncSchedulerInformerFactory(testCtx)
	externalInformers.Start(testCtx.Ctx.Done())
	externalInformers.WaitForCacheSync(testCtx.Ctx.Done())

	// Run all controllers
	go testCtx.Scheduler.Run(testCtx.Ctx)

	tests := []struct {
		Name             string
		PriorityClass    string
		Pod              *v1.Pod
		ExpectedPriority int32
		ExpectedError    error
	}{
		{
			Name:             "SystemNodeCritical priority class",
			PriorityClass:    scheduling.SystemNodeCritical,
			ExpectedPriority: scheduling.SystemCriticalPriority + 1000,
			Pod: initPausePod(&testutils.PausePodConfig{
				Name:              fmt.Sprintf("pod1-%v", scheduling.SystemNodeCritical),
				Namespace:         metav1.NamespaceSystem,
				PriorityClassName: scheduling.SystemNodeCritical,
			}),
		},
		{
			Name:             "SystemClusterCritical priority class",
			PriorityClass:    scheduling.SystemClusterCritical,
			ExpectedPriority: scheduling.SystemCriticalPriority,
			Pod: initPausePod(&testutils.PausePodConfig{
				Name:              fmt.Sprintf("pod2-%v", scheduling.SystemClusterCritical),
				Namespace:         metav1.NamespaceSystem,
				PriorityClassName: scheduling.SystemClusterCritical,
			}),
		},
		{
			Name:             "Invalid priority class should result in error",
			PriorityClass:    "foo",
			ExpectedPriority: scheduling.SystemCriticalPriority,
			Pod: initPausePod(&testutils.PausePodConfig{
				Name:              fmt.Sprintf("pod3-%v", scheduling.SystemClusterCritical),
				Namespace:         metav1.NamespaceSystem,
				PriorityClassName: "foo",
			}),
			ExpectedError: fmt.Errorf("failed to create pause pod: pods \"pod3-system-cluster-critical\" is forbidden: no PriorityClass with name foo was found"),
		},
	}

	// Create a node with some resources
	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	_, err := createNode(testCtx.ClientSet, st.MakeNode().Name("node1").Capacity(nodeRes).Obj())
	if err != nil {
		t.Fatalf("Error creating nodes: %v", err)
	}

	pods := make([]*v1.Pod, 0, len(tests))
	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.Name, asyncPreemptionEnabled), func(t *testing.T) {
				pod, err := runPausePod(cs, test.Pod)
				if err != nil {
					if test.ExpectedError == nil {
						t.Fatalf("Test [PodPriority/%v]: Error running pause pod: %v", test.PriorityClass, err)
					}
					if err.Error() != test.ExpectedError.Error() {
						t.Fatalf("Test [PodPriority/%v]: Expected error %v but got error %v", test.PriorityClass, test.ExpectedError, err)
					}
					return
				}
				pods = append(pods, pod)
				if pod.Spec.Priority != nil {
					if *pod.Spec.Priority != test.ExpectedPriority {
						t.Errorf("Expected pod %v to have priority %v but was %v", pod.Name, test.ExpectedPriority, pod.Spec.Priority)
					}
				} else {
					t.Errorf("Expected pod %v to have priority %v but was nil", pod.Name, test.PriorityClass)
				}
				testutils.CleanupPods(testCtx.Ctx, cs, t, pods)
			})
		}
	}
	testutils.CleanupNodes(cs, t)
}

func mkPriorityPodWithGrace(tc *testutils.TestContext, name string, priority int32, grace int64) *v1.Pod {
	defaultPodRes := &v1.ResourceRequirements{Requests: v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI)},
	}
	pod := initPausePod(&testutils.PausePodConfig{
		Name:      name,
		Namespace: tc.NS.Name,
		Priority:  &priority,
		Labels:    map[string]string{"pod": name},
		Resources: defaultPodRes,
	})
	pod.Spec.TerminationGracePeriodSeconds = &grace
	return pod
}

// This test ensures that while the preempting pod is waiting for the victims to
// terminate, other pending lower priority pods are not scheduled in the room created
// after preemption and while the higher priority pods is not scheduled yet.
func TestPreemptionStarvation(t *testing.T) {
	// Initialize scheduler.
	testCtx := initTest(t, "preemption")
	cs := testCtx.ClientSet

	tests := []struct {
		name               string
		numExistingPod     int
		numExpectedPending int
		preemptor          *v1.Pod
	}{
		{
			// This test ensures that while the preempting pod is waiting for the victims
			// terminate, other lower priority pods are not scheduled in the room created
			// after preemption and while the higher priority pods is not scheduled yet.
			name:               "starvation test: higher priority pod is scheduled before the lower priority ones",
			numExistingPod:     10,
			numExpectedPending: 5,
			preemptor: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(300, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
		},
	}

	// Create a node with some resources
	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	_, err := createNode(testCtx.ClientSet, st.MakeNode().Name("node1").Capacity(nodeRes).Obj())
	if err != nil {
		t.Fatalf("Error creating nodes: %v", err)
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				pendingPods := make([]*v1.Pod, test.numExpectedPending)
				numRunningPods := test.numExistingPod - test.numExpectedPending
				runningPods := make([]*v1.Pod, numRunningPods)
				// Create and run existingPods.
				for i := 0; i < numRunningPods; i++ {
					runningPods[i], err = createPausePod(cs, mkPriorityPodWithGrace(testCtx, fmt.Sprintf("rpod-%v", i), mediumPriority, 0))
					if err != nil {
						t.Fatalf("Error creating pause pod: %v", err)
					}
				}
				// make sure that runningPods are all scheduled.
				for _, p := range runningPods {
					if err := testutils.WaitForPodToSchedule(cs, p); err != nil {
						t.Fatalf("Pod %v/%v didn't get scheduled: %v", p.Namespace, p.Name, err)
					}
				}
				// Create pending pods.
				for i := 0; i < test.numExpectedPending; i++ {
					pendingPods[i], err = createPausePod(cs, mkPriorityPodWithGrace(testCtx, fmt.Sprintf("ppod-%v", i), mediumPriority, 0))
					if err != nil {
						t.Fatalf("Error creating pending pod: %v", err)
					}
				}
				// Make sure that all pending pods are being marked unschedulable.
				for _, p := range pendingPods {
					if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, wait.ForeverTestTimeout, false,
						podUnschedulable(cs, p.Namespace, p.Name)); err != nil {
						t.Errorf("Pod %v/%v didn't get marked unschedulable: %v", p.Namespace, p.Name, err)
					}
				}
				// Create the preemptor.
				preemptor, err := createPausePod(cs, test.preemptor)
				if err != nil {
					t.Errorf("Error while creating the preempting pod: %v", err)
				}
				// Check if .status.nominatedNodeName of the preemptor pod gets set.
				if err := testutils.WaitForNominatedNodeName(cs, preemptor); err != nil {
					t.Errorf(".status.nominatedNodeName was not set for pod %v/%v: %v", preemptor.Namespace, preemptor.Name, err)
				}
				// Make sure that preemptor is scheduled after preemptions.
				if err := testutils.WaitForPodToScheduleWithTimeout(cs, preemptor, 60*time.Second); err != nil {
					t.Errorf("Preemptor pod %v didn't get scheduled: %v", preemptor.Name, err)
				}
				// Cleanup
				klog.Info("Cleaning up all pods...")
				allPods := pendingPods
				allPods = append(allPods, runningPods...)
				allPods = append(allPods, preemptor)
				testutils.CleanupPods(testCtx.Ctx, cs, t, allPods)
			})
		}
	}
}

// TestPreemptionRaces tests that other scheduling events and operations do not
// race with the preemption process.
func TestPreemptionRaces(t *testing.T) {
	// Initialize scheduler.
	testCtx := initTest(t, "preemption-race")
	cs := testCtx.ClientSet

	tests := []struct {
		name              string
		numInitialPods    int // Pods created and executed before running preemptor
		numAdditionalPods int // Pods created after creating the preemptor
		numRepetitions    int // Repeat the tests to check races
		preemptor         *v1.Pod
	}{
		{
			// This test ensures that while the preempting pod is waiting for the victims
			// terminate, other lower priority pods are not scheduled in the room created
			// after preemption and while the higher priority pods is not scheduled yet.
			name:              "ensures that other pods are not scheduled while preemptor is being marked as nominated (issue #72124)",
			numInitialPods:    2,
			numAdditionalPods: 20,
			numRepetitions:    5,
			preemptor: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(4900, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(4900, resource.DecimalSI)},
				},
			}),
		},
	}

	// Create a node with some resources
	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "100",
		v1.ResourceCPU:    "5000m",
		v1.ResourceMemory: "5000",
	}
	_, err := createNode(testCtx.ClientSet, st.MakeNode().Name("node1").Capacity(nodeRes).Obj())
	if err != nil {
		t.Fatalf("Error creating nodes: %v", err)
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				if test.numRepetitions <= 0 {
					test.numRepetitions = 1
				}
				for n := 0; n < test.numRepetitions; n++ {
					initialPods := make([]*v1.Pod, test.numInitialPods)
					additionalPods := make([]*v1.Pod, test.numAdditionalPods)
					// Create and run existingPods.
					for i := 0; i < test.numInitialPods; i++ {
						initialPods[i], err = createPausePod(cs, mkPriorityPodWithGrace(testCtx, fmt.Sprintf("rpod-%v", i), mediumPriority, 0))
						if err != nil {
							t.Fatalf("Error creating pause pod: %v", err)
						}
					}
					// make sure that initial Pods are all scheduled.
					for _, p := range initialPods {
						if err := testutils.WaitForPodToSchedule(cs, p); err != nil {
							t.Fatalf("Pod %v/%v didn't get scheduled: %v", p.Namespace, p.Name, err)
						}
					}
					// Create the preemptor.
					klog.Info("Creating the preemptor pod...")
					preemptor, err := createPausePod(cs, test.preemptor)
					if err != nil {
						t.Errorf("Error while creating the preempting pod: %v", err)
					}

					klog.Info("Creating additional pods...")
					for i := 0; i < test.numAdditionalPods; i++ {
						additionalPods[i], err = createPausePod(cs, mkPriorityPodWithGrace(testCtx, fmt.Sprintf("ppod-%v", i), mediumPriority, 0))
						if err != nil {
							t.Fatalf("Error creating pending pod: %v", err)
						}
					}
					// Check that the preemptor pod gets nominated node name.
					if err := testutils.WaitForNominatedNodeName(cs, preemptor); err != nil {
						t.Errorf(".status.nominatedNodeName was not set for pod %v/%v: %v", preemptor.Namespace, preemptor.Name, err)
					}
					// Make sure that preemptor is scheduled after preemptions.
					if err := testutils.WaitForPodToScheduleWithTimeout(cs, preemptor, 60*time.Second); err != nil {
						t.Errorf("Preemptor pod %v didn't get scheduled: %v", preemptor.Name, err)
					}

					klog.Info("Check unschedulable pods still exists and were never scheduled...")
					for _, p := range additionalPods {
						pod, err := cs.CoreV1().Pods(p.Namespace).Get(testCtx.Ctx, p.Name, metav1.GetOptions{})
						if err != nil {
							t.Errorf("Error in getting Pod %v/%v info: %v", p.Namespace, p.Name, err)
						}
						if len(pod.Spec.NodeName) > 0 {
							t.Errorf("Pod %v/%v is already scheduled", p.Namespace, p.Name)
						}
						_, cond := podutil.GetPodCondition(&pod.Status, v1.PodScheduled)
						if cond != nil && cond.Status != v1.ConditionFalse {
							t.Errorf("Pod %v/%v is no longer unschedulable: %v", p.Namespace, p.Name, err)
						}
					}
					// Cleanup
					klog.Info("Cleaning up all pods...")
					allPods := additionalPods
					allPods = append(allPods, initialPods...)
					allPods = append(allPods, preemptor)
					testutils.CleanupPods(testCtx.Ctx, cs, t, allPods)
				}
			})
		}
	}
}

const (
	alwaysFailPlugin = "alwaysFailPlugin"
	doNotFailMe      = "do-not-fail-me"
)

// A fake plugin implements PreBind extension point.
// It always fails with an Unschedulable status, unless the pod contains a `doNotFailMe` string.
type alwaysFail struct{}

func (af *alwaysFail) Name() string {
	return alwaysFailPlugin
}

func (af *alwaysFail) PreBind(_ context.Context, _ *framework.CycleState, p *v1.Pod, _ string) *framework.Status {
	if strings.Contains(p.Name, doNotFailMe) {
		return nil
	}
	return framework.NewStatus(framework.Unschedulable)
}

func newAlwaysFail(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &alwaysFail{}, nil
}

// TestNominatedNodeCleanUp verifies if a pod's nominatedNodeName is set and unset
// properly in different scenarios.
func TestNominatedNodeCleanUp(t *testing.T) {
	tests := []struct {
		name         string
		nodeCapacity map[v1.ResourceName]string
		// A slice of pods to be created in batch.
		podsToCreate [][]*v1.Pod
		// Each postCheck function is run after each batch of pods' creation.
		postChecks []func(cs clientset.Interface, pod *v1.Pod) error
		// Delete the fake node or not. Optional.
		deleteNode bool
		// Pods to be deleted. Optional.
		podNamesToDelete []string

		// Register dummy plugin to simulate particular scheduling failures. Optional.
		customPlugins     *configv1.Plugins
		outOfTreeRegistry frameworkruntime.Registry
	}{
		{
			name:         "mid-priority pod preempts low-priority pod, followed by a high-priority pod with another preemption",
			nodeCapacity: map[v1.ResourceName]string{v1.ResourceCPU: "5"},
			podsToCreate: [][]*v1.Pod{
				{
					st.MakePod().Name("low-1").Priority(lowPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
					st.MakePod().Name("low-2").Priority(lowPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
					st.MakePod().Name("low-3").Priority(lowPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
					st.MakePod().Name("low-4").Priority(lowPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
				},
				{
					st.MakePod().Name("medium").Priority(mediumPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(),
				},
				{
					st.MakePod().Name("high").Priority(highPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "3"}).Obj(),
				},
			},
			postChecks: []func(cs clientset.Interface, pod *v1.Pod) error{
				testutils.WaitForPodToSchedule,
				testutils.WaitForNominatedNodeName,
				testutils.WaitForNominatedNodeName,
			},
		},
		{
			name:         "mid-priority pod preempts low-priority pod, followed by a high-priority pod without additional preemption",
			nodeCapacity: map[v1.ResourceName]string{v1.ResourceCPU: "2"},
			podsToCreate: [][]*v1.Pod{
				{
					st.MakePod().Name("low").Priority(lowPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
				},
				{
					st.MakePod().Name("medium").Priority(mediumPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj(),
				},
				{
					st.MakePod().Name("high").Priority(highPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
				},
			},
			postChecks: []func(cs clientset.Interface, pod *v1.Pod) error{
				testutils.WaitForPodToSchedule,
				testutils.WaitForNominatedNodeName,
				testutils.WaitForPodToSchedule,
			},
			podNamesToDelete: []string{"low"},
		},
		{
			name:         "mid-priority pod preempts low-priority pod, followed by a node deletion",
			nodeCapacity: map[v1.ResourceName]string{v1.ResourceCPU: "1"},
			podsToCreate: [][]*v1.Pod{
				{
					st.MakePod().Name("low").Priority(lowPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
				},
				{
					st.MakePod().Name("medium").Priority(mediumPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
				},
			},
			postChecks: []func(cs clientset.Interface, pod *v1.Pod) error{
				testutils.WaitForPodToSchedule,
				testutils.WaitForNominatedNodeName,
			},
			// Delete the node to simulate an ErrNoNodesAvailable error.
			deleteNode:       true,
			podNamesToDelete: []string{"low"},
		},
		{
			name:         "mid-priority pod preempts low-priority pod, but failed the scheduling unexpectedly",
			nodeCapacity: map[v1.ResourceName]string{v1.ResourceCPU: "1"},
			podsToCreate: [][]*v1.Pod{
				{
					st.MakePod().Name(fmt.Sprintf("low-%v", doNotFailMe)).Priority(lowPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
				},
				{
					st.MakePod().Name("medium").Priority(mediumPriority).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
				},
			},
			postChecks: []func(cs clientset.Interface, pod *v1.Pod) error{
				testutils.WaitForPodToSchedule,
				testutils.WaitForNominatedNodeName,
			},
			podNamesToDelete: []string{fmt.Sprintf("low-%v", doNotFailMe)},
			customPlugins: &configv1.Plugins{
				PreBind: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: alwaysFailPlugin},
					},
				},
			},
			outOfTreeRegistry: frameworkruntime.Registry{alwaysFailPlugin: newAlwaysFail},
		},
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", tt.name, asyncPreemptionEnabled), func(t *testing.T) {
				cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
					Profiles: []configv1.KubeSchedulerProfile{{
						SchedulerName: pointer.String(v1.DefaultSchedulerName),
						Plugins:       tt.customPlugins,
					}},
				})
				testCtx := initTest(
					t,
					"preemption",
					scheduler.WithProfiles(cfg.Profiles...),
					scheduler.WithFrameworkOutOfTreeRegistry(tt.outOfTreeRegistry),
				)

				cs, ns := testCtx.ClientSet, testCtx.NS.Name
				// Create a node with the specified capacity.
				nodeName := "fake-node"
				if _, err := createNode(cs, st.MakeNode().Name(nodeName).Capacity(tt.nodeCapacity).Obj()); err != nil {
					t.Fatalf("Error creating node %v: %v", nodeName, err)
				}

				// Create pods and run post check if necessary.
				for i, pods := range tt.podsToCreate {
					for _, p := range pods {
						p.Namespace = ns
						if _, err := createPausePod(cs, p); err != nil {
							t.Fatalf("Error creating pod %v: %v", p.Name, err)
						}
					}
					// If necessary, run the post check function.
					if len(tt.postChecks) > i && tt.postChecks[i] != nil {
						for _, p := range pods {
							if err := tt.postChecks[i](cs, p); err != nil {
								t.Fatalf("Pod %v didn't pass the postChecks[%v]: %v", p.Name, i, err)
							}
						}
					}
				}

				// Delete the node if necessary.
				if tt.deleteNode {
					if err := cs.CoreV1().Nodes().Delete(testCtx.Ctx, nodeName, *metav1.NewDeleteOptions(0)); err != nil {
						t.Fatalf("Node %v cannot be deleted: %v", nodeName, err)
					}
				}

				// Force deleting the terminating pods if necessary.
				// This is required if we demand to delete terminating Pods physically.
				for _, podName := range tt.podNamesToDelete {
					if err := deletePod(cs, podName, ns); err != nil {
						t.Fatalf("Pod %v cannot be deleted: %v", podName, err)
					}
				}

				// Verify if .status.nominatedNodeName is cleared.
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
					pod, err := cs.CoreV1().Pods(ns).Get(ctx, "medium", metav1.GetOptions{})
					if err != nil {
						t.Errorf("Error getting the medium pod: %v", err)
					}
					if len(pod.Status.NominatedNodeName) == 0 {
						return true, nil
					}
					return false, err
				}); err != nil {
					t.Errorf(".status.nominatedNodeName of the medium pod was not cleared: %v", err)
				}
			})
		}
	}
}

func mkMinAvailablePDB(name, namespace string, uid types.UID, minAvailable int, matchLabels map[string]string) *policy.PodDisruptionBudget {
	intMinAvailable := intstr.FromInt32(int32(minAvailable))
	return &policy.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: policy.PodDisruptionBudgetSpec{
			MinAvailable: &intMinAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}

func addPodConditionReady(pod *v1.Pod) {
	pod.Status = v1.PodStatus{
		Phase: v1.PodRunning,
		Conditions: []v1.PodCondition{
			{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			},
		},
	}
}

// TestPDBInPreemption tests PodDisruptionBudget support in preemption.
func TestPDBInPreemption(t *testing.T) {
	// Initialize scheduler.
	testCtx := initTest(t, "preemption-pdb")
	cs := testCtx.ClientSet

	initDisruptionController(t, testCtx)

	defaultPodRes := &v1.ResourceRequirements{Requests: v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI)},
	}
	defaultNodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}

	tests := []struct {
		name                string
		nodeCnt             int
		pdbs                []*policy.PodDisruptionBudget
		pdbPodNum           []int32
		existingPods        []*v1.Pod
		pod                 *v1.Pod
		preemptedPodIndexes map[int]struct{}
	}{
		{
			name:    "A non-PDB violating pod is preempted despite its higher priority",
			nodeCnt: 1,
			pdbs: []*policy.PodDisruptionBudget{
				mkMinAvailablePDB("pdb-1", testCtx.NS.Name, types.UID("pdb-1-uid"), 2, map[string]string{"foo": "bar"}),
			},
			pdbPodNum: []int32{2},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod1",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					Labels:    map[string]string{"foo": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod2",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					Labels:    map[string]string{"foo": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "mid-pod3",
					Namespace: testCtx.NS.Name,
					Priority:  &mediumPriority,
					Resources: defaultPodRes,
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(300, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{2: {}},
		},
		{
			name:    "A node without any PDB violating pods is preferred for preemption",
			nodeCnt: 2,
			pdbs: []*policy.PodDisruptionBudget{
				mkMinAvailablePDB("pdb-1", testCtx.NS.Name, types.UID("pdb-1-uid"), 2, map[string]string{"foo": "bar"}),
			},
			pdbPodNum: []int32{1},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod1",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					NodeName:  "node-1",
					Labels:    map[string]string{"foo": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "mid-pod2",
					Namespace: testCtx.NS.Name,
					Priority:  &mediumPriority,
					NodeName:  "node-2",
					Resources: defaultPodRes,
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{1: {}},
		},
		{
			name:    "A node with fewer PDB violating pods is preferred for preemption",
			nodeCnt: 3,
			pdbs: []*policy.PodDisruptionBudget{
				mkMinAvailablePDB("pdb-1", testCtx.NS.Name, types.UID("pdb-1-uid"), 2, map[string]string{"foo1": "bar"}),
				mkMinAvailablePDB("pdb-2", testCtx.NS.Name, types.UID("pdb-2-uid"), 2, map[string]string{"foo2": "bar"}),
			},
			pdbPodNum: []int32{1, 5},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod1",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					NodeName:  "node-1",
					Labels:    map[string]string{"foo1": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "mid-pod1",
					Namespace: testCtx.NS.Name,
					Priority:  &mediumPriority,
					Resources: defaultPodRes,
					NodeName:  "node-1",
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod2",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					NodeName:  "node-2",
					Labels:    map[string]string{"foo2": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "mid-pod2",
					Namespace: testCtx.NS.Name,
					Priority:  &mediumPriority,
					Resources: defaultPodRes,
					NodeName:  "node-2",
					Labels:    map[string]string{"foo2": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod4",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					NodeName:  "node-3",
					Labels:    map[string]string{"foo2": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod5",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					NodeName:  "node-3",
					Labels:    map[string]string{"foo2": "bar"},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod6",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					NodeName:  "node-3",
					Labels:    map[string]string{"foo2": "bar"},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(400, resource.DecimalSI)},
				},
			}),
			// The third node is chosen because PDB is not violated for node 3 and the victims have lower priority than node-2.
			preemptedPodIndexes: map[int]struct{}{4: {}, 5: {}, 6: {}},
		},
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				for i := 1; i <= test.nodeCnt; i++ {
					nodeName := fmt.Sprintf("node-%v", i)
					_, err := createNode(cs, st.MakeNode().Name(nodeName).Capacity(defaultNodeRes).Obj())
					if err != nil {
						t.Fatalf("Error creating node %v: %v", nodeName, err)
					}
				}

				pods := make([]*v1.Pod, len(test.existingPods))
				var err error
				// Create and run existingPods.
				for i, p := range test.existingPods {
					if pods[i], err = runPausePod(cs, p); err != nil {
						t.Fatalf("Test [%v]: Error running pause pod: %v", test.name, err)
					}
					// Add pod condition ready so that PDB is updated.
					addPodConditionReady(p)
					if _, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).UpdateStatus(testCtx.Ctx, p, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				// Wait for Pods to be stable in scheduler cache.
				if err := waitCachedPodsStable(testCtx, test.existingPods); err != nil {
					t.Fatalf("Not all pods are stable in the cache: %v", err)
				}

				// Create PDBs.
				for _, pdb := range test.pdbs {
					_, err := testCtx.ClientSet.PolicyV1().PodDisruptionBudgets(testCtx.NS.Name).Create(testCtx.Ctx, pdb, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("Failed to create PDB: %v", err)
					}
				}
				// Wait for PDBs to become stable.
				if err := waitForPDBsStable(testCtx, test.pdbs, test.pdbPodNum); err != nil {
					t.Fatalf("Not all pdbs are stable in the cache: %v", err)
				}

				// Create the "pod".
				preemptor, err := createPausePod(cs, test.pod)
				if err != nil {
					t.Errorf("Error while creating high priority pod: %v", err)
				}
				// Wait for preemption of pods and make sure the other ones are not preempted.
				for i, p := range pods {
					if _, found := test.preemptedPodIndexes[i]; found {
						if err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Second, wait.ForeverTestTimeout, false,
							podIsGettingEvicted(cs, p.Namespace, p.Name)); err != nil {
							t.Errorf("Test [%v]: Pod %v/%v is not getting evicted.", test.name, p.Namespace, p.Name)
						}
					} else {
						if p.DeletionTimestamp != nil {
							t.Errorf("Test [%v]: Didn't expect pod %v/%v to get preempted.", test.name, p.Namespace, p.Name)
						}
					}
				}
				// Also check if .status.nominatedNodeName of the preemptor pod gets set.
				if len(test.preemptedPodIndexes) > 0 {
					if err := testutils.WaitForNominatedNodeName(cs, preemptor); err != nil {
						t.Errorf("Test [%v]: .status.nominatedNodeName was not set for pod %v/%v: %v", test.name, preemptor.Namespace, preemptor.Name, err)
					}
				}

				// Cleanup
				pods = append(pods, preemptor)
				testutils.CleanupPods(testCtx.Ctx, cs, t, pods)
				if err := cs.PolicyV1().PodDisruptionBudgets(testCtx.NS.Name).DeleteCollection(testCtx.Ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
					t.Errorf("error while deleting PDBs, error: %v", err)
				}
				if err := cs.CoreV1().Nodes().DeleteCollection(testCtx.Ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
					t.Errorf("error whiling deleting nodes, error: %v", err)
				}
			})
		}
	}
}

func initTestPreferNominatedNode(t *testing.T, nsPrefix string, opts ...scheduler.Option) *testutils.TestContext {
	testCtx := testutils.InitTestSchedulerWithOptions(t, testutils.InitTestAPIServer(t, nsPrefix, nil), 0, opts...)
	testutils.SyncSchedulerInformerFactory(testCtx)
	// wraps the NextPod() method to make it appear the preemption has been done already and the nominated node has been set.
	f := testCtx.Scheduler.NextPod
	testCtx.Scheduler.NextPod = func(logger klog.Logger) (*framework.QueuedPodInfo, error) {
		podInfo, _ := f(klog.FromContext(testCtx.Ctx))
		// Scheduler.Next() may return nil when scheduler is shutting down.
		if podInfo != nil {
			podInfo.Pod.Status.NominatedNodeName = "node-1"
		}
		return podInfo, nil
	}
	go testCtx.Scheduler.Run(testCtx.Ctx)
	return testCtx
}

// TestPreferNominatedNode test that if the nominated node pass all the filters, then preemptor pod will run on the nominated node,
// otherwise, it will be scheduled to another node in the cluster that ables to pass all the filters.
func TestPreferNominatedNode(t *testing.T) {
	defaultNodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	defaultPodRes := &v1.ResourceRequirements{Requests: v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI)},
	}
	tests := []struct {
		name         string
		nodeNames    []string
		existingPods []*v1.Pod
		pod          *v1.Pod
		runningNode  string
	}{
		{
			name:      "nominated node released all resource, preemptor is scheduled to the nominated node",
			nodeNames: []string{"node-1", "node-2"},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod1",
					Priority:  &lowPriority,
					NodeName:  "node-2",
					Resources: defaultPodRes,
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:     "preemptor-pod",
				Priority: &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			runningNode: "node-1",
		},
		{
			name:      "nominated node cannot pass all the filters, preemptor should find a different node",
			nodeNames: []string{"node-1", "node-2"},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "low-pod",
					Priority:  &lowPriority,
					Resources: defaultPodRes,
					NodeName:  "node-1",
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:     "preemptor-pod1",
				Priority: &highPriority,
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI)},
				},
			}),
			runningNode: "node-2",
		},
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				testCtx := initTestPreferNominatedNode(t, "perfer-nominated-node")
				cs := testCtx.ClientSet
				nsName := testCtx.NS.Name
				var err error
				var preemptor *v1.Pod
				for _, nodeName := range test.nodeNames {
					_, err := createNode(cs, st.MakeNode().Name(nodeName).Capacity(defaultNodeRes).Obj())
					if err != nil {
						t.Fatalf("Error creating node %v: %v", nodeName, err)
					}
				}

				pods := make([]*v1.Pod, len(test.existingPods))
				// Create and run existingPods.
				for i, p := range test.existingPods {
					p.Namespace = nsName
					pods[i], err = runPausePod(cs, p)
					if err != nil {
						t.Fatalf("Error running pause pod: %v", err)
					}
				}
				test.pod.Namespace = nsName
				preemptor, err = createPausePod(cs, test.pod)
				if err != nil {
					t.Errorf("Error while creating high priority pod: %v", err)
				}
				err = wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
					preemptor, err = cs.CoreV1().Pods(test.pod.Namespace).Get(ctx, test.pod.Name, metav1.GetOptions{})
					if err != nil {
						t.Errorf("Error getting the preemptor pod info: %v", err)
					}
					if len(preemptor.Spec.NodeName) == 0 {
						return false, err
					}
					return true, nil
				})
				if err != nil {
					t.Errorf("Cannot schedule Pod %v/%v, error: %v", test.pod.Namespace, test.pod.Name, err)
				}
				// Make sure the pod has been scheduled to the right node.
				if preemptor.Spec.NodeName != test.runningNode {
					t.Errorf("Expect pod running on %v, got %v.", test.runningNode, preemptor.Spec.NodeName)
				}
			})
		}
	}
}

// TestReadWriteOncePodPreemption tests preemption scenarios for pods with
// ReadWriteOncePod PVCs.
func TestReadWriteOncePodPreemption(t *testing.T) {
	cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
		Profiles: []configv1.KubeSchedulerProfile{{
			SchedulerName: pointer.StringPtr(v1.DefaultSchedulerName),
			Plugins: &configv1.Plugins{
				Filter: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: volumerestrictions.Name},
					},
				},
				PreFilter: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: volumerestrictions.Name},
					},
				},
			},
		}},
	})

	testCtx := testutils.InitTestSchedulerWithOptions(t,
		testutils.InitTestAPIServer(t, "preemption", nil),
		0,
		scheduler.WithProfiles(cfg.Profiles...))
	testutils.SyncSchedulerInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)

	cs := testCtx.ClientSet

	storage := v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}
	volType := v1.HostPathDirectoryOrCreate
	pv1 := st.MakePersistentVolume().
		Name("pv-with-read-write-once-pod-1").
		AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
		Capacity(storage.Requests).
		HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/mnt1", Type: &volType}).
		Obj()
	pvc1 := st.MakePersistentVolumeClaim().
		Name("pvc-with-read-write-once-pod-1").
		Namespace(testCtx.NS.Name).
		// Annotation and volume name required for PVC to be considered bound.
		Annotation(volume.AnnBindCompleted, "true").
		VolumeName(pv1.Name).
		AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
		Resources(storage).
		Obj()
	pv2 := st.MakePersistentVolume().
		Name("pv-with-read-write-once-pod-2").
		AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
		Capacity(storage.Requests).
		HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/mnt2", Type: &volType}).
		Obj()
	pvc2 := st.MakePersistentVolumeClaim().
		Name("pvc-with-read-write-once-pod-2").
		Namespace(testCtx.NS.Name).
		// Annotation and volume name required for PVC to be considered bound.
		Annotation(volume.AnnBindCompleted, "true").
		VolumeName(pv2.Name).
		AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
		Resources(storage).
		Obj()

	tests := []struct {
		name                string
		init                func() error
		existingPods        []*v1.Pod
		pod                 *v1.Pod
		unresolvable        bool
		preemptedPodIndexes map[int]struct{}
		cleanup             func() error
	}{
		{
			name: "preempt single pod",
			init: func() error {
				_, err := testutils.CreatePV(cs, pv1)
				if err != nil {
					return fmt.Errorf("cannot create pv: %v", err)
				}
				_, err = testutils.CreatePVC(cs, pvc1)
				if err != nil {
					return fmt.Errorf("cannot create pvc: %v", err)
				}
				return nil
			},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Volumes: []v1.Volume{{
						Name: "volume",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc1.Name,
							},
						},
					}},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Volumes: []v1.Volume{{
					Name: "volume",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc1.Name,
						},
					},
				}},
			}),
			preemptedPodIndexes: map[int]struct{}{0: {}},
			cleanup: func() error {
				if err := testutils.DeletePVC(cs, pvc1.Name, pvc1.Namespace); err != nil {
					return fmt.Errorf("cannot delete pvc: %v", err)
				}
				if err := testutils.DeletePV(cs, pv1.Name); err != nil {
					return fmt.Errorf("cannot delete pv: %v", err)
				}
				return nil
			},
		},
		{
			name: "preempt two pods",
			init: func() error {
				for _, pv := range []*v1.PersistentVolume{pv1, pv2} {
					_, err := testutils.CreatePV(cs, pv)
					if err != nil {
						return fmt.Errorf("cannot create pv: %v", err)
					}
				}
				for _, pvc := range []*v1.PersistentVolumeClaim{pvc1, pvc2} {
					_, err := testutils.CreatePVC(cs, pvc)
					if err != nil {
						return fmt.Errorf("cannot create pvc: %v", err)
					}
				}
				return nil
			},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod-1",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Volumes: []v1.Volume{{
						Name: "volume",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc1.Name,
							},
						},
					}},
				}),
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod-2",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Volumes: []v1.Volume{{
						Name: "volume",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc2.Name,
							},
						},
					}},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Volumes: []v1.Volume{
					{
						Name: "volume-1",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc1.Name,
							},
						},
					},
					{
						Name: "volume-2",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc2.Name,
							},
						},
					},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{0: {}, 1: {}},
			cleanup: func() error {
				for _, pvc := range []*v1.PersistentVolumeClaim{pvc1, pvc2} {
					if err := testutils.DeletePVC(cs, pvc.Name, pvc.Namespace); err != nil {
						return fmt.Errorf("cannot delete pvc: %v", err)
					}
				}
				for _, pv := range []*v1.PersistentVolume{pv1, pv2} {
					if err := testutils.DeletePV(cs, pv.Name); err != nil {
						return fmt.Errorf("cannot delete pv: %v", err)
					}
				}
				return nil
			},
		},
		{
			name: "preempt single pod with two volumes",
			init: func() error {
				for _, pv := range []*v1.PersistentVolume{pv1, pv2} {
					_, err := testutils.CreatePV(cs, pv)
					if err != nil {
						return fmt.Errorf("cannot create pv: %v", err)
					}
				}
				for _, pvc := range []*v1.PersistentVolumeClaim{pvc1, pvc2} {
					_, err := testutils.CreatePVC(cs, pvc)
					if err != nil {
						return fmt.Errorf("cannot create pvc: %v", err)
					}
				}
				return nil
			},
			existingPods: []*v1.Pod{
				initPausePod(&testutils.PausePodConfig{
					Name:      "victim-pod",
					Namespace: testCtx.NS.Name,
					Priority:  &lowPriority,
					Volumes: []v1.Volume{
						{
							Name: "volume-1",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvc1.Name,
								},
							},
						},
						{
							Name: "volume-2",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvc2.Name,
								},
							},
						},
					},
				}),
			},
			pod: initPausePod(&testutils.PausePodConfig{
				Name:      "preemptor-pod",
				Namespace: testCtx.NS.Name,
				Priority:  &highPriority,
				Volumes: []v1.Volume{
					{
						Name: "volume-1",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc1.Name,
							},
						},
					},
					{
						Name: "volume-2",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc2.Name,
							},
						},
					},
				},
			}),
			preemptedPodIndexes: map[int]struct{}{0: {}},
			cleanup: func() error {
				for _, pvc := range []*v1.PersistentVolumeClaim{pvc1, pvc2} {
					if err := testutils.DeletePVC(cs, pvc.Name, pvc.Namespace); err != nil {
						return fmt.Errorf("cannot delete pvc: %v", err)
					}
				}
				for _, pv := range []*v1.PersistentVolume{pv1, pv2} {
					if err := testutils.DeletePV(cs, pv.Name); err != nil {
						return fmt.Errorf("cannot delete pv: %v", err)
					}
				}
				return nil
			},
		},
	}

	// Create a node with some resources and a label.
	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	nodeObject := st.MakeNode().Name("node1").Capacity(nodeRes).Label("node", "node1").Obj()
	if _, err := createNode(cs, nodeObject); err != nil {
		t.Fatalf("Error creating node: %v", err)
	}

	for _, asyncPreemptionEnabled := range []bool{true, false} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s (Async preemption enabled: %v)", test.name, asyncPreemptionEnabled), func(t *testing.T) {
				if err := test.init(); err != nil {
					t.Fatalf("Error while initializing test: %v", err)
				}

				pods := make([]*v1.Pod, len(test.existingPods))
				t.Cleanup(func() {
					testutils.CleanupPods(testCtx.Ctx, cs, t, pods)
					if err := test.cleanup(); err != nil {
						t.Errorf("Error cleaning up test: %v", err)
					}
				})
				// Create and run existingPods.
				for i, p := range test.existingPods {
					var err error
					pods[i], err = runPausePod(cs, p)
					if err != nil {
						t.Fatalf("Error running pause pod: %v", err)
					}
				}
				// Create the "pod".
				preemptor, err := createPausePod(cs, test.pod)
				if err != nil {
					t.Errorf("Error while creating high priority pod: %v", err)
				}
				pods = append(pods, preemptor)
				// Wait for preemption of pods and make sure the other ones are not preempted.
				for i, p := range pods {
					if _, found := test.preemptedPodIndexes[i]; found {
						if err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Second, wait.ForeverTestTimeout, false,
							podIsGettingEvicted(cs, p.Namespace, p.Name)); err != nil {
							t.Errorf("Pod %v/%v is not getting evicted.", p.Namespace, p.Name)
						}
					} else {
						if p.DeletionTimestamp != nil {
							t.Errorf("Didn't expect pod %v to get preempted.", p.Name)
						}
					}
				}
				// Also check that the preemptor pod gets the NominatedNodeName field set.
				if len(test.preemptedPodIndexes) > 0 {
					if err := testutils.WaitForNominatedNodeName(cs, preemptor); err != nil {
						t.Errorf("NominatedNodeName field was not set for pod %v: %v", preemptor.Name, err)
					}
				}
			})
		}
	}
}
