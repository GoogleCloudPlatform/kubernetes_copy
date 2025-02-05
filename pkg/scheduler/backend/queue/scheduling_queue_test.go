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

package queue

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	plfeature "k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/schedulinggates"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/pkg/scheduler/util"
	testingclock "k8s.io/utils/clock/testing"
)

const queueMetricMetadata = `
		# HELP scheduler_queue_incoming_pods_total [STABLE] Number of pods added to scheduling queues by event and queue type.
		# TYPE scheduler_queue_incoming_pods_total counter
	`

var (
	// nodeAdd is the event when a new node is added to the cluster.
	nodeAdd = framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Add}
	// pvAdd is the event when a persistent volume is added in the cluster.
	pvAdd = framework.ClusterEvent{Resource: framework.PersistentVolume, ActionType: framework.Add}
	// pvUpdate is the event when a persistent volume is updated in the cluster.
	pvUpdate = framework.ClusterEvent{Resource: framework.PersistentVolume, ActionType: framework.Update}
	// pvcAdd is the event when a persistent volume claim is added in the cluster.
	pvcAdd = framework.ClusterEvent{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add}
	// csiNodeUpdate is the event when a CSI node is updated in the cluster.
	csiNodeUpdate = framework.ClusterEvent{Resource: framework.CSINode, ActionType: framework.Update}

	lowPriority, midPriority, highPriority = int32(0), int32(100), int32(1000)
	mediumPriority                         = (lowPriority + highPriority) / 2

	highPriorityPodInfo = mustNewPodInfo(
		st.MakePod().Name("hpp").Namespace("ns1").UID("hppns1").Priority(highPriority).Obj(),
	)
	highPriNominatedPodInfo = mustNewPodInfo(
		st.MakePod().Name("hpp").Namespace("ns1").UID("hppns1").Priority(highPriority).NominatedNodeName("node1").Obj(),
	)
	medPriorityPodInfo = mustNewPodInfo(
		st.MakePod().Name("mpp").Namespace("ns2").UID("mppns2").Annotation("annot2", "val2").Priority(mediumPriority).NominatedNodeName("node1").Obj(),
	)
	unschedulablePodInfo = mustNewPodInfo(
		st.MakePod().Name("up").Namespace("ns1").UID("upns1").Annotation("annot2", "val2").Priority(lowPriority).NominatedNodeName("node1").Condition(v1.PodScheduled, v1.ConditionFalse, v1.PodReasonUnschedulable).Obj(),
	)
	nonExistentPodInfo = mustNewPodInfo(
		st.MakePod().Name("ne").Namespace("ns1").UID("nens1").Obj(),
	)
	scheduledPodInfo = mustNewPodInfo(
		st.MakePod().Name("sp").Namespace("ns1").UID("spns1").Node("foo").Obj(),
	)

	nominatorCmpOpts = []cmp.Option{
		cmp.AllowUnexported(nominator{}, podRef{}),
		cmpopts.IgnoreFields(nominator{}, "podLister", "nLock"),
	}

	queueHintReturnQueue = func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
		return framework.Queue, nil
	}
	queueHintReturnSkip = func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
		return framework.QueueSkip, nil
	}
)

func init() {
	metrics.Register()
}

func setQueuedPodInfoGated(queuedPodInfo *framework.QueuedPodInfo) *framework.QueuedPodInfo {
	queuedPodInfo.Gated = true
	return queuedPodInfo
}

func getUnschedulablePod(p *PriorityQueue, pod *v1.Pod) *v1.Pod {
	pInfo := p.unschedulablePods.get(pod)
	if pInfo != nil {
		return pInfo.Pod
	}
	return nil
}

// makeEmptyQueueingHintMapPerProfile initializes an empty QueueingHintMapPerProfile for "" profile name.
func makeEmptyQueueingHintMapPerProfile() QueueingHintMapPerProfile {
	m := make(QueueingHintMapPerProfile)
	m[""] = make(QueueingHintMap)
	return m
}

func TestPriorityQueue_Add(t *testing.T) {
	objs := []runtime.Object{medPriorityPodInfo.Pod, unschedulablePodInfo.Pod, highPriorityPodInfo.Pod}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)
	q.Add(logger, medPriorityPodInfo.Pod)
	q.Add(logger, unschedulablePodInfo.Pod)
	q.Add(logger, highPriorityPodInfo.Pod)
	expectedNominatedPods := &nominator{
		nominatedPodToNode: map[types.UID]string{
			medPriorityPodInfo.Pod.UID:   "node1",
			unschedulablePodInfo.Pod.UID: "node1",
		},
		nominatedPods: map[string][]podRef{
			"node1": {podToRef(medPriorityPodInfo.Pod), podToRef(unschedulablePodInfo.Pod)},
		},
	}
	if diff := cmp.Diff(q.nominator, expectedNominatedPods, nominatorCmpOpts...); diff != "" {
		t.Errorf("Unexpected diff after adding pods (-want, +got):\n%s", diff)
	}
	if p, err := q.Pop(logger); err != nil || p.Pod != highPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	if p, err := q.Pop(logger); err != nil || p.Pod != medPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	if p, err := q.Pop(logger); err != nil || p.Pod != unschedulablePodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePodInfo.Pod.Name, p.Pod.Name)
	}
	if len(q.nominator.nominatedPods["node1"]) != 2 {
		t.Errorf("Expected medPriorityPodInfo and unschedulablePodInfo to be still present in nominatedPods: %v", q.nominator.nominatedPods["node1"])
	}
}

func newDefaultQueueSort() framework.LessFunc {
	sort := &queuesort.PrioritySort{}
	return sort.Less
}

func TestPriorityQueue_AddWithReversePriorityLessFunc(t *testing.T) {
	objs := []runtime.Object{medPriorityPodInfo.Pod, highPriorityPodInfo.Pod}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)
	q.Add(logger, medPriorityPodInfo.Pod)
	q.Add(logger, highPriorityPodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != highPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	if p, err := q.Pop(logger); err != nil || p.Pod != medPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
}

func Test_InFlightPods(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	pod1 := st.MakePod().Name("targetpod").UID("pod1").Obj()
	pod2 := st.MakePod().Name("targetpod2").UID("pod2").Obj()
	pod3 := st.MakePod().Name("targetpod3").UID("pod3").Obj()
	var poppedPod, poppedPod2 *framework.QueuedPodInfo

	type action struct {
		// ONLY ONE of the following should be set.
		eventHappens *framework.ClusterEvent
		podPopped    *v1.Pod
		// podCreated is the Pod that is created and inserted into the activeQ.
		podCreated *v1.Pod
		// podEnqueued is the Pod that is enqueued back to activeQ.
		podEnqueued *framework.QueuedPodInfo
		callback    func(t *testing.T, q *PriorityQueue)
	}

	tests := []struct {
		name            string
		queueingHintMap QueueingHintMapPerProfile
		// initialPods is the initial Pods in the activeQ.
		initialPods                  []*v1.Pod
		actions                      []action
		wantInFlightPods             []*v1.Pod
		wantInFlightEvents           []interface{}
		wantActiveQPodNames          []string
		wantBackoffQPodNames         []string
		wantUnschedPodPoolPodNames   []string
		isSchedulingQueueHintEnabled bool
	}{
		{
			name:        "when SchedulingQueueHint is disabled, inFlightPods and inFlightEvents should be empty",
			initialPods: []*v1.Pod{pod1},
			actions: []action{
				// This Pod shouldn't be added to inFlightPods because SchedulingQueueHint is disabled.
				{podPopped: pod1},
				// This event shouldn't be added to inFlightEvents because SchedulingQueueHint is disabled.
				{eventHappens: &pvAdd},
			},
			wantInFlightPods:   nil,
			wantInFlightEvents: nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					pvAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "Pod and interested events are registered in inFlightPods/inFlightEvents",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				// This won't be added to inFlightEvents because no inFlightPods at this point.
				{eventHappens: &pvcAdd},
				{podPopped: pod1},
				// This gets added for the pod.
				{eventHappens: &pvAdd},
				// This doesn't get added because no plugin is interested in PvUpdate.
				{eventHappens: &pvUpdate},
			},
			wantInFlightPods:   []*v1.Pod{pod1},
			wantInFlightEvents: []interface{}{pod1, pvAdd},
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					pvAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "Pod, registered in inFlightPods, is enqueued back to backoffQ",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1, pod2},
			actions: []action{
				// This won't be added to inFlightEvents because no inFlightPods at this point.
				{eventHappens: &pvcAdd},
				{podPopped: pod1},
				{eventHappens: &pvAdd},
				{podPopped: pod2},
				{eventHappens: &nodeAdd},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod1)},
			},
			wantBackoffQPodNames: []string{"targetpod"},
			wantInFlightPods:     []*v1.Pod{pod2}, // only pod2 is registered because pod is already enqueued back.
			wantInFlightEvents:   []interface{}{pod2, nodeAdd},
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					pvAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					nodeAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					pvcAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "All Pods registered in inFlightPods are enqueued back to activeQ",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1, pod2},
			actions: []action{
				// This won't be added to inFlightEvents because no inFlightPods at this point.
				{eventHappens: &pvcAdd},
				{podPopped: pod1},
				{eventHappens: &pvAdd},
				{podPopped: pod2},
				{eventHappens: &nodeAdd},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod1)},
				{eventHappens: &csiNodeUpdate},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod2)},
			},
			wantBackoffQPodNames: []string{"targetpod", "targetpod2"},
			wantInFlightPods:     nil, // empty
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					pvAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					nodeAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					pvcAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					csiNodeUpdate: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "One intermediate Pod registered in inFlightPods is enqueued back to activeQ",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1, pod2, pod3},
			actions: []action{
				// This won't be added to inFlightEvents because no inFlightPods at this point.
				{eventHappens: &pvcAdd},
				{podPopped: pod1},
				{eventHappens: &pvAdd},
				{podPopped: pod2},
				{eventHappens: &nodeAdd},
				// This Pod won't be requeued again.
				{podPopped: pod3},
				{eventHappens: &framework.EventAssignedPodAdd},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod2)},
			},
			wantBackoffQPodNames: []string{"targetpod2"},
			wantInFlightPods:     []*v1.Pod{pod1, pod3},
			wantInFlightEvents:   []interface{}{pod1, pvAdd, nodeAdd, pod3, framework.EventAssignedPodAdd},
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					pvAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					nodeAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					framework.EventAssignedPodAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:        "pod is enqueued to queue without QueueingHint when SchedulingQueueHint is disabled",
			initialPods: []*v1.Pod{pod1},
			actions: []action{
				{podPopped: pod1},
				{eventHappens: &framework.EventAssignedPodAdd},
				{podEnqueued: newQueuedPodInfoForLookup(pod1, "fooPlugin1")},
			},
			wantBackoffQPodNames: []string{"targetpod"},
			wantInFlightPods:     nil,
			wantInFlightEvents:   nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					// This hint fn tells that this event doesn't make a Pod schedulable.
					// However, this QueueingHintFn will be ignored actually because SchedulingQueueHint is disabled.
					framework.EventAssignedPodAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnSkip,
						},
					},
				},
			},
		},
		{
			name:                         "events before popping Pod are ignored when Pod is enqueued back to queue",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				{eventHappens: &framework.EventUnschedulableTimeout},
				{podPopped: pod1},
				{eventHappens: &framework.EventAssignedPodAdd},
				// This Pod won't be requeued to activeQ/backoffQ because fooPlugin1 returns QueueSkip.
				{podEnqueued: newQueuedPodInfoForLookup(pod1, "fooPlugin1")},
			},
			wantUnschedPodPoolPodNames: []string{"targetpod"},
			wantInFlightPods:           nil,
			wantInFlightEvents:         nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					// fooPlugin1 has a queueing hint function for framework.AssignedPodAdd,
					// but hint fn tells that this event doesn't make a Pod scheudlable.
					framework.EventAssignedPodAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnSkip,
						},
					},
				},
			},
		},
		{
			name:                         "pod is enqueued to backoff if no failed plugin",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				{podPopped: pod1},
				{eventHappens: &framework.EventAssignedPodAdd},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod1)},
			},
			wantBackoffQPodNames: []string{"targetpod"},
			wantInFlightPods:     nil,
			wantInFlightEvents:   nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					// It will be ignored because no failed plugin.
					framework.EventAssignedPodAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "pod is enqueued to unschedulable pod pool if no events that can make the pod schedulable",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				{podPopped: pod1},
				{eventHappens: &nodeAdd},
				{podEnqueued: newQueuedPodInfoForLookup(pod1, "fooPlugin1")},
			},
			wantUnschedPodPoolPodNames: []string{"targetpod"},
			wantInFlightPods:           nil,
			wantInFlightEvents:         nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					// fooPlugin1 has no queueing hint function for NodeAdd.
					framework.EventAssignedPodAdd: {
						{
							// It will be ignored because the event is not NodeAdd.
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "pod is enqueued to unschedulable pod pool because the failed plugin has a hint fn but it returns Skip",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				{podPopped: pod1},
				{eventHappens: &framework.EventAssignedPodAdd},
				{podEnqueued: newQueuedPodInfoForLookup(pod1, "fooPlugin1")},
			},
			wantUnschedPodPoolPodNames: []string{"targetpod"},
			wantInFlightPods:           nil,
			wantInFlightEvents:         nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					// fooPlugin1 has a queueing hint function for framework.AssignedPodAdd,
					// but hint fn tells that this event doesn't make a Pod scheudlable.
					framework.EventAssignedPodAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnSkip,
						},
					},
				},
			},
		},
		{
			name:                         "pod is enqueued to activeQ because the Pending plugins has a hint fn and it returns Queue",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				{podPopped: pod1},
				{eventHappens: &framework.EventAssignedPodAdd},
				{podEnqueued: &framework.QueuedPodInfo{
					PodInfo:              mustNewPodInfo(pod1),
					UnschedulablePlugins: sets.New("fooPlugin2", "fooPlugin3"),
					PendingPlugins:       sets.New("fooPlugin1"),
				}},
			},
			wantActiveQPodNames: []string{"targetpod"},
			wantInFlightPods:    nil,
			wantInFlightEvents:  nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					framework.EventAssignedPodAdd: {
						{
							PluginName:     "fooPlugin3",
							QueueingHintFn: queueHintReturnSkip,
						},
						{
							PluginName:     "fooPlugin2",
							QueueingHintFn: queueHintReturnQueue,
						},
						{
							// The hint fn tells that this event makes a Pod scheudlable immediately.
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "pod is enqueued to backoffQ because the failed plugin has a hint fn and it returns Queue",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				{podPopped: pod1},
				{eventHappens: &framework.EventAssignedPodAdd},
				{podEnqueued: newQueuedPodInfoForLookup(pod1, "fooPlugin1", "fooPlugin2")},
			},
			wantBackoffQPodNames: []string{"targetpod"},
			wantInFlightPods:     nil,
			wantInFlightEvents:   nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					framework.EventAssignedPodAdd: {
						{
							// it will be ignored because the hint fn returns Skip that is weaker than queueHintReturnQueue from fooPlugin1.
							PluginName:     "fooPlugin2",
							QueueingHintFn: queueHintReturnSkip,
						},
						{
							// The hint fn tells that this event makes a Pod schedulable.
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "pod is enqueued to activeQ because the pending plugin has a hint fn and it returns Queue for a concurrent event that was received while some other pod was in flight",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1, pod2},
			actions: []action{
				{callback: func(t *testing.T, q *PriorityQueue) { poppedPod = popPod(t, logger, q, pod1) }},
				{eventHappens: &nodeAdd},
				{callback: func(t *testing.T, q *PriorityQueue) { poppedPod2 = popPod(t, logger, q, pod2) }},
				{eventHappens: &framework.EventAssignedPodAdd},
				{callback: func(t *testing.T, q *PriorityQueue) {
					logger, _ := ktesting.NewTestContext(t)
					// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
					// which means the pod encountered an unexpected error (e.g., a network error).
					err := q.AddUnschedulableIfNotPresent(logger, poppedPod, q.SchedulingCycle())
					if err != nil {
						t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
					}
				}},
				{callback: func(t *testing.T, q *PriorityQueue) {
					logger, _ := ktesting.NewTestContext(t)
					poppedPod2.UnschedulablePlugins = sets.New("fooPlugin2", "fooPlugin3")
					poppedPod2.PendingPlugins = sets.New("fooPlugin1")
					err := q.AddUnschedulableIfNotPresent(logger, poppedPod2, q.SchedulingCycle())
					if err != nil {
						t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
					}
				}},
			},
			wantActiveQPodNames:  []string{pod2.Name},
			wantBackoffQPodNames: []string{pod1.Name},
			wantInFlightPods:     nil,
			wantInFlightEvents:   nil,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					framework.EventAssignedPodAdd: {
						{
							// it will be ignored because the hint fn returns QueueSkip that is weaker than queueHintReturnQueueImmediately from fooPlugin1.
							PluginName:     "fooPlugin3",
							QueueingHintFn: queueHintReturnSkip,
						},
						{
							// it will be ignored because the fooPlugin2 is registered in UnschedulablePlugins and it's interpret as Queue that is weaker than QueueImmediately from fooPlugin1.
							PluginName:     "fooPlugin2",
							QueueingHintFn: queueHintReturnQueue,
						},
						{
							// The hint fn tells that this event makes a Pod scheudlable.
							// Given fooPlugin1 is registered as Pendings, we interpret Queue as queueImmediately.
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name:                         "popped pod must have empty UnschedulablePlugins and PendingPlugins",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1},
			actions: []action{
				{callback: func(t *testing.T, q *PriorityQueue) { poppedPod = popPod(t, logger, q, pod1) }},
				{callback: func(t *testing.T, q *PriorityQueue) {
					logger, _ := ktesting.NewTestContext(t)
					// Unschedulable due to PendingPlugins.
					poppedPod.PendingPlugins = sets.New("fooPlugin1")
					poppedPod.UnschedulablePlugins = sets.New("fooPlugin2")
					if err := q.AddUnschedulableIfNotPresent(logger, poppedPod, q.SchedulingCycle()); err != nil {
						t.Errorf("Unexpected error from AddUnschedulableIfNotPresent: %v", err)
					}
				}},
				{eventHappens: &pvAdd}, // Active again.
				{callback: func(t *testing.T, q *PriorityQueue) {
					poppedPod = popPod(t, logger, q, pod1)
					if len(poppedPod.UnschedulablePlugins) > 0 {
						t.Errorf("QueuedPodInfo from Pop should have empty UnschedulablePlugins, got instead: %+v", poppedPod)
					}
				}},
				{callback: func(t *testing.T, q *PriorityQueue) {
					logger, _ := ktesting.NewTestContext(t)
					// Failed (i.e. no UnschedulablePlugins). Should go to backoff.
					if err := q.AddUnschedulableIfNotPresent(logger, poppedPod, q.SchedulingCycle()); err != nil {
						t.Errorf("Unexpected error from AddUnschedulableIfNotPresent: %v", err)
					}
				}},
			},
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					pvAdd: {
						{
							// The hint fn tells that this event makes a Pod scheudlable immediately.
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			// This scenario shouldn't happen unless we make the similar bug like https://github.com/kubernetes/kubernetes/issues/118226.
			// But, given the bug could make a serious memory leak and likely would be hard to detect,
			// we should have a safe guard from the same bug so that, at least, we can prevent the memory leak.
			name:                         "Pop is made twice for the same Pod, but the cleanup still happen correctly",
			isSchedulingQueueHintEnabled: true,
			initialPods:                  []*v1.Pod{pod1, pod2},
			actions: []action{
				// This won't be added to inFlightEvents because no inFlightPods at this point.
				{eventHappens: &pvcAdd},
				{podPopped: pod1},
				{eventHappens: &pvAdd},
				{podPopped: pod2},
				// Simulate a bug, putting pod into activeQ, while pod is being scheduled.
				{callback: func(t *testing.T, q *PriorityQueue) {
					q.activeQ.underLock(func(unlocked unlockedActiveQueuer) {
						unlocked.AddOrUpdate(newQueuedPodInfoForLookup(pod1))
					})
				}},
				// At this point, in the activeQ, we have pod1 and pod3 in this order.
				{podCreated: pod3},
				// pod3 is poped, not pod1.
				// In detail, this Pop() first tries to pop pod1, but it's already being scheduled and hence discarded.
				// Then, it pops the next pod, pod3.
				{podPopped: pod3},
				{callback: func(t *testing.T, q *PriorityQueue) {
					// Make sure that pod1 is discarded and hence no pod in activeQ.
					if len(q.activeQ.list()) != 0 {
						t.Fatalf("activeQ should be empty, but got: %v", q.activeQ.list())
					}
				}},
				{eventHappens: &nodeAdd},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod1)},
				{eventHappens: &csiNodeUpdate},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod2)},
				// This pod will be requeued to backoffQ immediately because no plugin is registered as unschedulable plugin,
				// which means the pod encountered an unexpected error (e.g., a network error).
				{podEnqueued: newQueuedPodInfoForLookup(pod3)},
			},
			wantBackoffQPodNames: []string{"targetpod", "targetpod2", "targetpod3"},
			wantInFlightPods:     nil, // should be empty
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					pvAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					nodeAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					pvcAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					csiNodeUpdate: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, test.isSchedulingQueueHintEnabled)
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			obj := make([]runtime.Object, 0, len(test.initialPods))
			for _, p := range test.initialPods {
				obj = append(obj, p)
			}
			fakeClock := testingclock.NewFakeClock(time.Now())
			q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), obj, WithQueueingHintMapPerProfile(test.queueingHintMap), WithClock(fakeClock))
			sortOpt := cmpopts.SortSlices(func(a, b string) bool { return a < b })

			// When a Pod is added to the queue, the QueuedPodInfo will have a new timestamp.
			// On Windows, time.Now() is not as precise, 2 consecutive calls may return the same timestamp.
			// Thus, all the QueuedPodInfos can have the same timestamps, which can be an issue
			// when we're expecting them to be popped in a certain order (the Less function
			// sorts them by Timestamps if they have the same Pod Priority).
			// Using a fake clock for the queue and incrementing it after each added Pod will
			// solve this issue on Windows unit test runs.
			// For more details on the Windows clock resolution issue, see: https://github.com/golang/go/issues/8687
			for _, p := range test.initialPods {
				q.Add(logger, p)
				fakeClock.Step(time.Second)
			}

			for _, action := range test.actions {
				switch {
				case action.podCreated != nil:
					q.Add(logger, action.podCreated)
				case action.podPopped != nil:
					popPod(t, logger, q, action.podPopped)
				case action.eventHappens != nil:
					q.MoveAllToActiveOrBackoffQueue(logger, *action.eventHappens, nil, nil, nil)
				case action.podEnqueued != nil:
					err := q.AddUnschedulableIfNotPresent(logger, action.podEnqueued, q.SchedulingCycle())
					if err != nil {
						t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
					}
				case action.callback != nil:
					action.callback(t, q)
				}
			}

			actualInFlightPods := make(map[types.UID]*v1.Pod)
			for _, pod := range q.activeQ.listInFlightPods() {
				actualInFlightPods[pod.UID] = pod
			}
			wantInFlightPods := make(map[types.UID]*v1.Pod)
			for _, pod := range test.wantInFlightPods {
				wantInFlightPods[pod.UID] = pod
			}
			if diff := cmp.Diff(wantInFlightPods, actualInFlightPods); diff != "" {
				t.Errorf("Unexpected diff in inFlightPods (-want, +got):\n%s", diff)
			}

			var wantInFlightEvents []interface{}
			for _, value := range test.wantInFlightEvents {
				if event, ok := value.(framework.ClusterEvent); ok {
					value = &clusterEvent{event: event}
				}
				wantInFlightEvents = append(wantInFlightEvents, value)
			}
			if diff := cmp.Diff(wantInFlightEvents, q.activeQ.listInFlightEvents(), cmp.AllowUnexported(clusterEvent{}), cmpopts.EquateComparable(framework.ClusterEvent{})); diff != "" {
				t.Errorf("Unexpected diff in inFlightEvents (-want, +got):\n%s", diff)
			}

			if test.wantActiveQPodNames != nil {
				pods := q.activeQ.list()
				var podNames []string
				for _, pod := range pods {
					podNames = append(podNames, pod.Name)
				}
				if diff := cmp.Diff(test.wantActiveQPodNames, podNames, sortOpt); diff != "" {
					t.Fatalf("Unexpected diff of activeQ pod names (-want, +got):\n%s", diff)
				}

				wantPodNames := sets.New(test.wantActiveQPodNames...)
				for _, pod := range pods {
					if !wantPodNames.Has(pod.Name) {
						t.Fatalf("Pod %v was not expected to be in the activeQ.", pod.Name)
					}
				}
			}

			if test.wantBackoffQPodNames != nil {
				podInfos := q.podBackoffQ.List()
				var podNames []string
				for _, pInfo := range podInfos {
					podNames = append(podNames, pInfo.Pod.Name)
				}
				if diff := cmp.Diff(test.wantBackoffQPodNames, podNames, sortOpt); diff != "" {
					t.Fatalf("Unexpected diff of backoffQ pod names (-want, +got):\n%s", diff)
				}

				wantPodNames := sets.New(test.wantBackoffQPodNames...)
				for _, podInfo := range podInfos {
					podGotFromBackoffQ := podInfo.Pod
					if !wantPodNames.Has(podGotFromBackoffQ.Name) {
						t.Fatalf("Pod %v was not expected to be in the backoffQ.", podGotFromBackoffQ.Name)
					}
				}
			}

			for _, podName := range test.wantUnschedPodPoolPodNames {
				p := getUnschedulablePod(q, &st.MakePod().Name(podName).Pod)
				if p == nil {
					t.Fatalf("Pod %v was not found in the unschedulablePods.", podName)
				}
			}
		})
	}
}

func popPod(t *testing.T, logger klog.Logger, q *PriorityQueue, pod *v1.Pod) *framework.QueuedPodInfo {
	p, err := q.Pop(logger)
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if p.Pod.UID != pod.UID {
		t.Errorf("Unexpected popped pod: %v", p)
	}
	return p
}

func TestPop(t *testing.T) {
	pod := st.MakePod().Name("targetpod").UID("pod1").Obj()
	queueingHintMap := QueueingHintMapPerProfile{
		"": {
			pvAdd: {
				{
					// The hint fn tells that this event makes a Pod scheudlable.
					PluginName:     "fooPlugin1",
					QueueingHintFn: queueHintReturnQueue,
				},
			},
		},
	}

	for name, isSchedulingQueueHintEnabled := range map[string]bool{"with-hints": true, "without-hints": false} {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, isSchedulingQueueHintEnabled)
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), []runtime.Object{pod}, WithQueueingHintMapPerProfile(queueingHintMap))
			q.Add(logger, pod)

			// Simulate failed attempt that makes the pod unschedulable.
			poppedPod := popPod(t, logger, q, pod)
			// We put register the plugin to PendingPlugins so that it's interpreted as queueImmediately and skip backoff.
			poppedPod.PendingPlugins = sets.New("fooPlugin1")
			if err := q.AddUnschedulableIfNotPresent(logger, poppedPod, q.SchedulingCycle()); err != nil {
				t.Errorf("Unexpected error from AddUnschedulableIfNotPresent: %v", err)
			}

			// Activate it again.
			q.MoveAllToActiveOrBackoffQueue(logger, pvAdd, nil, nil, nil)

			// Now check result of Pop.
			poppedPod = popPod(t, logger, q, pod)
			if len(poppedPod.PendingPlugins) > 0 {
				t.Errorf("QueuedPodInfo from Pop should have empty PendingPlugins, got instead: %+v", poppedPod)
			}
		})
	}
}

func TestPriorityQueue_AddUnschedulableIfNotPresent(t *testing.T) {
	objs := []runtime.Object{highPriNominatedPodInfo.Pod, unschedulablePodInfo.Pod}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)
	// insert unschedulablePodInfo and pop right after that
	// because the scheduling queue records unschedulablePod as in-flight Pod.
	q.Add(logger, unschedulablePodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != unschedulablePodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePodInfo.Pod.Name, p.Pod.Name)
	}

	q.Add(logger, highPriNominatedPodInfo.Pod)
	err := q.AddUnschedulableIfNotPresent(logger, newQueuedPodInfoForLookup(unschedulablePodInfo.Pod, "plugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	if p, err := q.Pop(logger); err != nil || p.Pod != highPriNominatedPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriNominatedPodInfo.Pod.Name, p.Pod.Name)
	}
	if len(q.nominator.nominatedPods) != 1 {
		t.Errorf("Expected nominatedPods to have one element: %v", q.nominator)
	}
	// unschedulablePodInfo is inserted to unschedulable pod pool because no events happened during scheduling.
	if getUnschedulablePod(q, unschedulablePodInfo.Pod) != unschedulablePodInfo.Pod {
		t.Errorf("Pod %v was not found in the unschedulablePods.", unschedulablePodInfo.Pod.Name)
	}
}

// TestPriorityQueue_AddUnschedulableIfNotPresent_Backoff tests the scenarios when
// AddUnschedulableIfNotPresent is called asynchronously.
// Pods in and before current scheduling cycle will be put back to activeQueue
// if we were trying to schedule them when we received move request.
func TestPriorityQueue_AddUnschedulableIfNotPresent_Backoff(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(testingclock.NewFakeClock(time.Now())))
	totalNum := 10
	expectedPods := make([]v1.Pod, 0, totalNum)
	for i := 0; i < totalNum; i++ {
		priority := int32(i)
		p := st.MakePod().Name(fmt.Sprintf("pod%d", i)).Namespace(fmt.Sprintf("ns%d", i)).UID(fmt.Sprintf("upns%d", i)).Priority(priority).Obj()
		expectedPods = append(expectedPods, *p)
		// priority is to make pods ordered in the PriorityQueue
		q.Add(logger, p)
	}

	// Pop all pods except for the first one
	for i := totalNum - 1; i > 0; i-- {
		p, _ := q.Pop(logger)
		if diff := cmp.Diff(&expectedPods[i], p.Pod); diff != "" {
			t.Errorf("Unexpected pod (-want, +got):\n%s", diff)
		}
	}

	// move all pods to active queue when we were trying to schedule them
	q.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)
	oldCycle := q.SchedulingCycle()

	firstPod, _ := q.Pop(logger)
	if diff := cmp.Diff(&expectedPods[0], firstPod.Pod); diff != "" {
		t.Errorf("Unexpected pod (-want, +got):\n%s", diff)
	}

	// mark pods[1] ~ pods[totalNum-1] as unschedulable and add them back
	for i := 1; i < totalNum; i++ {
		unschedulablePod := expectedPods[i].DeepCopy()
		unschedulablePod.Status = v1.PodStatus{
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodScheduled,
					Status: v1.ConditionFalse,
					Reason: v1.PodReasonUnschedulable,
				},
			},
		}

		err := q.AddUnschedulableIfNotPresent(logger, newQueuedPodInfoForLookup(unschedulablePod, "plugin"), oldCycle)
		if err != nil {
			t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
		}
	}

	// Since there was a move request at the same cycle as "oldCycle", these pods
	// should be in the backoff queue.
	for i := 1; i < totalNum; i++ {
		if !q.podBackoffQ.Has(newQueuedPodInfoForLookup(&expectedPods[i])) {
			t.Errorf("Expected %v to be added to podBackoffQ.", expectedPods[i].Name)
		}
	}
}

func TestPriorityQueue_Pop(t *testing.T) {
	objs := []runtime.Object{medPriorityPodInfo.Pod}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if p, err := q.Pop(logger); err != nil || p.Pod != medPriorityPodInfo.Pod {
			t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod.Name, p.Pod.Name)
		}
		if len(q.nominator.nominatedPods["node1"]) != 1 {
			t.Errorf("Expected medPriorityPodInfo to be present in nominatedPods: %v", q.nominator.nominatedPods["node1"])
		}
	}()
	q.Add(logger, medPriorityPodInfo.Pod)
	wg.Wait()
}

func TestPriorityQueue_Update(t *testing.T) {
	c := testingclock.NewFakeClock(time.Now())

	queuePlugin := "queuePlugin"
	skipPlugin := "skipPlugin"
	queueingHintMap := QueueingHintMapPerProfile{
		"": {
			framework.EventUnscheduledPodUpdate: {
				{
					PluginName:     queuePlugin,
					QueueingHintFn: queueHintReturnQueue,
				},
				{
					PluginName:     skipPlugin,
					QueueingHintFn: queueHintReturnSkip,
				},
			},
		},
	}

	notInAnyQueue := "NotInAnyQueue"
	tests := []struct {
		name  string
		wantQ string
		// wantAddedToNominated is whether a Pod from the test case should be registered as a nominated Pod in the nominator.
		wantAddedToNominated bool
		// prepareFunc is the function called to prepare pods in the queue before the test case calls Update().
		// This function returns three values;
		// - oldPod/newPod: each test will call Update() with these oldPod and newPod.
		prepareFunc func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod)
		// schedulingHintsEnablement shows which value of QHint feature gate we test a test case with.
		schedulingHintsEnablement []bool
	}{
		{
			name:  "Update pod that didn't exist in the queue",
			wantQ: activeQ,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				updatedPod := medPriorityPodInfo.Pod.DeepCopy()
				updatedPod.Annotations["foo"] = "test"
				return medPriorityPodInfo.Pod, updatedPod
			},
			schedulingHintsEnablement: []bool{false, true},
		},
		{
			name:                 "Update highPriorityPodInfo and add a nominatedNodeName to it",
			wantQ:                activeQ,
			wantAddedToNominated: true,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				return highPriorityPodInfo.Pod, highPriNominatedPodInfo.Pod
			},
			schedulingHintsEnablement: []bool{false, true},
		},
		{
			name:  "When updating a pod that is already in activeQ, the pod should remain in activeQ after Update()",
			wantQ: activeQ,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				q.Add(logger, highPriorityPodInfo.Pod)
				return highPriorityPodInfo.Pod, highPriorityPodInfo.Pod
			},
			schedulingHintsEnablement: []bool{false, true},
		},
		{
			name:  "When updating a pod that is in backoff queue and is still backing off, it will be updated in backoff queue",
			wantQ: backoffQ,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				podInfo := q.newQueuedPodInfo(medPriorityPodInfo.Pod)
				q.podBackoffQ.AddOrUpdate(podInfo)
				return podInfo.Pod, podInfo.Pod
			},
			schedulingHintsEnablement: []bool{false, true},
		},
		{
			name:  "when updating a pod which is in unschedulable queue and is backing off, it will be moved to backoff queue",
			wantQ: backoffQ,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				pInfo := q.newQueuedPodInfo(medPriorityPodInfo.Pod, queuePlugin)
				// needs to increment to make the pod backing off
				pInfo.UnschedulableCount++
				q.unschedulablePods.addOrUpdate(pInfo)
				updatedPod := medPriorityPodInfo.Pod.DeepCopy()
				updatedPod.Annotations["foo"] = "test"
				return medPriorityPodInfo.Pod, updatedPod
			},
			schedulingHintsEnablement: []bool{false, true},
		},
		{
			name:  "when updating a pod which is in unschedulable queue and is not backing off, it will be moved to active queue",
			wantQ: activeQ,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				pInfo := q.newQueuedPodInfo(medPriorityPodInfo.Pod, queuePlugin)
				// needs to increment to make the pod backing off
				pInfo.UnschedulableCount++
				q.unschedulablePods.addOrUpdate(pInfo)
				updatedPod := medPriorityPodInfo.Pod.DeepCopy()
				updatedPod.Annotations["foo"] = "test1"
				// Move clock by podInitialBackoffDuration, so that pods in the unschedulablePods would pass the backing off,
				// and the pods will be moved into activeQ.
				c.Step(q.podInitialBackoffDuration)
				return medPriorityPodInfo.Pod, updatedPod
			},
			schedulingHintsEnablement: []bool{false, true},
		},
		{
			name:  "when updating a pod which is in unschedulable pods but the plugin returns skip, it will remain in unschedulablePods",
			wantQ: unschedulablePods,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				q.unschedulablePods.addOrUpdate(q.newQueuedPodInfo(medPriorityPodInfo.Pod, skipPlugin))
				updatedPod := medPriorityPodInfo.Pod.DeepCopy()
				updatedPod.Annotations["foo"] = "test1"
				return medPriorityPodInfo.Pod, updatedPod
			},
			schedulingHintsEnablement: []bool{true},
		},
		{
			name:  "when updating a pod which is in flightPods, the pod will not be added to any queue",
			wantQ: notInAnyQueue,
			prepareFunc: func(t *testing.T, logger klog.Logger, q *PriorityQueue) (oldPod, newPod *v1.Pod) {
				// We need to once add this Pod to activeQ and Pop() it so that this Pod is registered correctly in inFlightPods.
				q.Add(logger, medPriorityPodInfo.Pod)
				if p, err := q.Pop(logger); err != nil || p.Pod != medPriorityPodInfo.Pod {
					t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod.Name, p.Pod.Name)
				}
				updatedPod := medPriorityPodInfo.Pod.DeepCopy()
				updatedPod.Annotations["foo"] = "bar"
				return medPriorityPodInfo.Pod, updatedPod
			},
			schedulingHintsEnablement: []bool{true},
		},
	}

	for _, tt := range tests {
		for _, qHintEnabled := range tt.schedulingHintsEnablement {
			t.Run(fmt.Sprintf("%s, with queuehint(%v)", tt.name, qHintEnabled), func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, qHintEnabled)
				logger, ctx := ktesting.NewTestContext(t)
				objs := []runtime.Object{highPriorityPodInfo.Pod, unschedulablePodInfo.Pod, medPriorityPodInfo.Pod}
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs, WithClock(c), WithQueueingHintMapPerProfile(queueingHintMap))

				oldPod, newPod := tt.prepareFunc(t, logger, q)

				q.Update(logger, oldPod, newPod)

				var pInfo *framework.QueuedPodInfo

				// validate expected queue
				if pInfoFromBackoff, exists := q.podBackoffQ.Get(newQueuedPodInfoForLookup(newPod)); exists {
					if tt.wantQ != backoffQ {
						t.Errorf("expected pod %s not to be queued to backoffQ, but it was", newPod.Name)
					}
					pInfo = pInfoFromBackoff
				}

				q.activeQ.underRLock(func(unlockedActiveQ unlockedActiveQueueReader) {
					if pInfoFromActive, exists := unlockedActiveQ.Get(newQueuedPodInfoForLookup(newPod)); exists {
						if tt.wantQ != activeQ {
							t.Errorf("expected pod %s not to be queued to activeQ, but it was", newPod.Name)
						}
						pInfo = pInfoFromActive
					}
				})

				if pInfoFromUnsched := q.unschedulablePods.get(newPod); pInfoFromUnsched != nil {
					if tt.wantQ != unschedulablePods {
						t.Errorf("expected pod %s to not be queued to unschedulablePods, but it was", newPod.Name)
					}
					pInfo = pInfoFromUnsched
				}

				if tt.wantQ == notInAnyQueue {
					// skip the rest of the test if pod is not expected to be in any of the queues.
					return
				}

				if diff := cmp.Diff(newPod, pInfo.PodInfo.Pod); diff != "" {
					t.Errorf("Unexpected updated pod diff (-want, +got): %s", diff)
				}

				if tt.wantAddedToNominated && len(q.nominator.nominatedPods) != 1 {
					t.Errorf("Expected one item in nominatedPods map: %v", q.nominator)
				}

			})
		}
	}
}

// TestPriorityQueue_UpdateWhenInflight ensures to requeue a Pod back to activeQ/backoffQ
// if it actually got an update that may make it schedulable while being scheduled.
// See https://github.com/kubernetes/kubernetes/pull/125578#discussion_r1648338033 for more context.
func TestPriorityQueue_UpdateWhenInflight(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, true)
	m := makeEmptyQueueingHintMapPerProfile()
	// fakePlugin could change its scheduling result by any updates in Pods.
	m[""][framework.EventUnscheduledPodUpdate] = []*QueueingHintFunction{
		{
			PluginName:     "fakePlugin",
			QueueingHintFn: queueHintReturnQueue,
		},
	}
	c := testingclock.NewFakeClock(time.Now())
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithQueueingHintMapPerProfile(m), WithClock(c))

	// test-pod is created and popped out from the queue
	testPod := st.MakePod().Name("test-pod").Namespace("test-ns").UID("test-uid").Obj()
	q.Add(logger, testPod)
	if p, err := q.Pop(logger); err != nil || p.Pod != testPod {
		t.Errorf("Expected: %v after Pop, but got: %v", testPod.Name, p.Pod.Name)
	}

	// testPod is updated while being scheduled.
	updatedPod := testPod.DeepCopy()
	updatedPod.Spec.Tolerations = []v1.Toleration{
		{
			Key:    "foo",
			Effect: v1.TaintEffectNoSchedule,
		},
	}

	q.Update(logger, testPod, updatedPod)
	// test-pod got rejected by fakePlugin,
	// but the update event that it just got may change this scheduling result,
	// and hence we should put this pod to activeQ/backoffQ.
	err := q.AddUnschedulableIfNotPresent(logger, newQueuedPodInfoForLookup(updatedPod, "fakePlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}

	pInfo, exists := q.podBackoffQ.Get(newQueuedPodInfoForLookup(updatedPod))
	if !exists {
		t.Fatalf("expected pod %s to be queued to backoffQ, but it wasn't.", updatedPod.Name)
	}
	if diff := cmp.Diff(updatedPod, pInfo.PodInfo.Pod); diff != "" {
		t.Errorf("Unexpected updated pod diff (-want, +got): %s", diff)
	}
}

func TestPriorityQueue_Delete(t *testing.T) {
	objs := []runtime.Object{highPriorityPodInfo.Pod, unschedulablePodInfo.Pod}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)
	q.Update(logger, highPriorityPodInfo.Pod, highPriNominatedPodInfo.Pod)
	q.Add(logger, unschedulablePodInfo.Pod)
	q.Delete(highPriNominatedPodInfo.Pod)
	if !q.activeQ.has(newQueuedPodInfoForLookup(unschedulablePodInfo.Pod)) {
		t.Errorf("Expected %v to be in activeQ.", unschedulablePodInfo.Pod.Name)
	}
	if q.activeQ.has(newQueuedPodInfoForLookup(highPriNominatedPodInfo.Pod)) {
		t.Errorf("Didn't expect %v to be in activeQ.", highPriorityPodInfo.Pod.Name)
	}
	if len(q.nominator.nominatedPods) != 1 {
		t.Errorf("Expected nominatedPods to have only 'unschedulablePodInfo': %v", q.nominator.nominatedPods)
	}
	q.Delete(unschedulablePodInfo.Pod)
	if len(q.nominator.nominatedPods) != 0 {
		t.Errorf("Expected nominatedPods to be empty: %v", q.nominator)
	}
}

func TestPriorityQueue_Activate(t *testing.T) {
	metrics.Register()
	tests := []struct {
		name                        string
		qPodInfoInUnschedulablePods []*framework.QueuedPodInfo
		qPodInfoInPodBackoffQ       []*framework.QueuedPodInfo
		qPodInActiveQ               []*v1.Pod
		qPodInfoToActivate          *framework.QueuedPodInfo
		qPodInInFlightPod           *v1.Pod
		expectedInFlightEvent       *clusterEvent
		want                        []*framework.QueuedPodInfo
		qHintEnabled                bool
	}{
		{
			name:               "pod already in activeQ",
			qPodInActiveQ:      []*v1.Pod{highPriNominatedPodInfo.Pod},
			qPodInfoToActivate: &framework.QueuedPodInfo{PodInfo: highPriNominatedPodInfo},
			want:               []*framework.QueuedPodInfo{{PodInfo: highPriNominatedPodInfo}}, // 1 already active
		},
		{
			name:               "pod not in unschedulablePods/podBackoffQ",
			qPodInfoToActivate: &framework.QueuedPodInfo{PodInfo: highPriNominatedPodInfo},
			want:               []*framework.QueuedPodInfo{},
		},
		{
			name:                  "[QHint] pod not in unschedulablePods/podBackoffQ but in-flight",
			qPodInfoToActivate:    &framework.QueuedPodInfo{PodInfo: highPriNominatedPodInfo},
			qPodInInFlightPod:     highPriNominatedPodInfo.Pod,
			expectedInFlightEvent: &clusterEvent{oldObj: (*v1.Pod)(nil), newObj: highPriNominatedPodInfo.Pod, event: framework.EventForceActivate},
			want:                  []*framework.QueuedPodInfo{},
			qHintEnabled:          true,
		},
		{
			name:               "[QHint] pod not in unschedulablePods/podBackoffQ and not in-flight",
			qPodInfoToActivate: &framework.QueuedPodInfo{PodInfo: highPriNominatedPodInfo},
			qPodInInFlightPod:  medPriorityPodInfo.Pod, // different pod is in-flight
			want:               []*framework.QueuedPodInfo{},
			qHintEnabled:       true,
		},
		{
			name:                        "pod in unschedulablePods",
			qPodInfoInUnschedulablePods: []*framework.QueuedPodInfo{{PodInfo: highPriNominatedPodInfo}},
			qPodInfoToActivate:          &framework.QueuedPodInfo{PodInfo: highPriNominatedPodInfo},
			want:                        []*framework.QueuedPodInfo{{PodInfo: highPriNominatedPodInfo}},
		},
		{
			name:                  "pod in backoffQ",
			qPodInfoInPodBackoffQ: []*framework.QueuedPodInfo{{PodInfo: highPriNominatedPodInfo}},
			qPodInfoToActivate:    &framework.QueuedPodInfo{PodInfo: highPriNominatedPodInfo},
			want:                  []*framework.QueuedPodInfo{{PodInfo: highPriNominatedPodInfo}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, tt.qHintEnabled)
			var objs []runtime.Object
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)

			if tt.qPodInInFlightPod != nil {
				// Put -> Pop the Pod to make it registered in inFlightPods.
				q.activeQ.underLock(func(unlockedActiveQ unlockedActiveQueuer) {
					unlockedActiveQ.AddOrUpdate(newQueuedPodInfoForLookup(tt.qPodInInFlightPod))
				})
				p, err := q.activeQ.pop(logger)
				if err != nil {
					t.Fatalf("Pop failed: %v", err)
				}
				if p.Pod.Name != tt.qPodInInFlightPod.Name {
					t.Errorf("Unexpected popped pod: %v", p.Pod.Name)
				}
				if len(q.activeQ.listInFlightEvents()) != 1 {
					t.Fatal("Expected the pod to be recorded in in-flight events, but it doesn't")
				}
			}

			// Prepare activeQ/unschedulablePods/podBackoffQ according to the table
			for _, qPod := range tt.qPodInActiveQ {
				q.Add(logger, qPod)
			}

			for _, qPodInfo := range tt.qPodInfoInUnschedulablePods {
				q.unschedulablePods.addOrUpdate(qPodInfo)
			}

			for _, qPodInfo := range tt.qPodInfoInPodBackoffQ {
				q.podBackoffQ.AddOrUpdate(qPodInfo)
			}

			// Activate specific pod according to the table
			q.Activate(logger, map[string]*v1.Pod{"test_pod": tt.qPodInfoToActivate.PodInfo.Pod})

			// Check the result after activation by the length of activeQ
			if wantLen := len(tt.want); q.activeQ.len() != wantLen {
				t.Fatalf("length compare: want %v, got %v", wantLen, q.activeQ.len())
			}

			if tt.expectedInFlightEvent != nil {
				if len(q.activeQ.listInFlightEvents()) != 2 {
					t.Fatalf("Expected two in-flight event to be recorded, but got %v events", len(q.activeQ.listInFlightEvents()))
				}
				found := false
				for _, e := range q.activeQ.listInFlightEvents() {
					event, ok := e.(*clusterEvent)
					if !ok {
						continue
					}

					if d := cmp.Diff(tt.expectedInFlightEvent, event, cmpopts.EquateComparable(clusterEvent{})); d != "" {
						t.Fatalf("Unexpected in-flight event (-want, +got):\n%s", d)
					}
					found = true
				}

				if !found {
					t.Fatalf("Expected in-flight event to be recorded, but it wasn't.")
				}
			}

			// Check if the specific pod exists in activeQ
			for _, want := range tt.want {
				if !q.activeQ.has(newQueuedPodInfoForLookup(want.PodInfo.Pod)) {
					t.Errorf("podInfo not exist in activeQ: want %v", want.PodInfo.Pod.Name)
				}
			}
		})
	}
}

type preEnqueuePlugin struct {
	allowlists []string
}

func (pl *preEnqueuePlugin) Name() string {
	return "preEnqueuePlugin"
}

func (pl *preEnqueuePlugin) PreEnqueue(ctx context.Context, p *v1.Pod) *framework.Status {
	for _, allowed := range pl.allowlists {
		for label := range p.Labels {
			if label == allowed {
				return nil
			}
		}
	}
	return framework.NewStatus(framework.UnschedulableAndUnresolvable, "pod name not in allowlists")
}

func TestPriorityQueue_addToActiveQ(t *testing.T) {
	tests := []struct {
		name                  string
		plugins               []framework.PreEnqueuePlugin
		pod                   *v1.Pod
		wantUnschedulablePods int
		wantSuccess           bool
	}{
		{
			name:                  "no plugins registered",
			pod:                   st.MakePod().Name("p").Label("p", "").Obj(),
			wantUnschedulablePods: 0,
			wantSuccess:           true,
		},
		{
			name:                  "preEnqueue plugin registered, pod name not in allowlists",
			plugins:               []framework.PreEnqueuePlugin{&preEnqueuePlugin{}, &preEnqueuePlugin{}},
			pod:                   st.MakePod().Name("p").Label("p", "").Obj(),
			wantUnschedulablePods: 1,
			wantSuccess:           false,
		},
		{
			name: "preEnqueue plugin registered, pod failed one preEnqueue plugin",
			plugins: []framework.PreEnqueuePlugin{
				&preEnqueuePlugin{allowlists: []string{"foo", "bar"}},
				&preEnqueuePlugin{allowlists: []string{"foo"}},
			},
			pod:                   st.MakePod().Name("bar").Label("bar", "").Obj(),
			wantUnschedulablePods: 1,
			wantSuccess:           false,
		},
		{
			name: "preEnqueue plugin registered, pod passed all preEnqueue plugins",
			plugins: []framework.PreEnqueuePlugin{
				&preEnqueuePlugin{allowlists: []string{"foo", "bar"}},
				&preEnqueuePlugin{allowlists: []string{"bar"}},
			},
			pod:                   st.MakePod().Name("bar").Label("bar", "").Obj(),
			wantUnschedulablePods: 0,
			wantSuccess:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			m := map[string][]framework.PreEnqueuePlugin{"": tt.plugins}
			q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), []runtime.Object{tt.pod}, WithPreEnqueuePluginMap(m),
				WithPodInitialBackoffDuration(time.Second*30), WithPodMaxBackoffDuration(time.Second*60))
			got := q.moveToActiveQ(logger, q.newQueuedPodInfo(tt.pod), framework.EventUnscheduledPodAdd.Label())
			if got != tt.wantSuccess {
				t.Errorf("Unexpected result: want %v, but got %v", tt.wantSuccess, got)
			}
			if tt.wantUnschedulablePods != len(q.unschedulablePods.podInfoMap) {
				t.Errorf("Unexpected unschedulablePods: want %v, but got %v", tt.wantUnschedulablePods, len(q.unschedulablePods.podInfoMap))
			}

			// Simulate an update event.
			clone := tt.pod.DeepCopy()
			metav1.SetMetaDataAnnotation(&clone.ObjectMeta, "foo", "")
			q.Update(logger, tt.pod, clone)
			// Ensure the pod is still located in unschedulablePods.
			if tt.wantUnschedulablePods != len(q.unschedulablePods.podInfoMap) {
				t.Errorf("Unexpected unschedulablePods: want %v, but got %v", tt.wantUnschedulablePods, len(q.unschedulablePods.podInfoMap))
			}
		})
	}
}

func BenchmarkMoveAllToActiveOrBackoffQueue(b *testing.B) {
	tests := []struct {
		name      string
		moveEvent framework.ClusterEvent
	}{
		{
			name:      "baseline",
			moveEvent: framework.EventUnschedulableTimeout,
		},
		{
			name:      "worst",
			moveEvent: nodeAdd,
		},
		{
			name: "random",
			// leave "moveEvent" unspecified
		},
	}

	podTemplates := []*v1.Pod{
		highPriorityPodInfo.Pod, highPriNominatedPodInfo.Pod,
		medPriorityPodInfo.Pod, unschedulablePodInfo.Pod,
	}

	events := []framework.ClusterEvent{
		{Resource: framework.Node, ActionType: framework.Add},
		{Resource: framework.Node, ActionType: framework.UpdateNodeTaint},
		{Resource: framework.Node, ActionType: framework.UpdateNodeAllocatable},
		{Resource: framework.Node, ActionType: framework.UpdateNodeCondition},
		{Resource: framework.Node, ActionType: framework.UpdateNodeLabel},
		{Resource: framework.Node, ActionType: framework.UpdateNodeAnnotation},
		{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add},
		{Resource: framework.PersistentVolumeClaim, ActionType: framework.Update},
		{Resource: framework.PersistentVolume, ActionType: framework.Add},
		{Resource: framework.PersistentVolume, ActionType: framework.Update},
		{Resource: framework.StorageClass, ActionType: framework.Add},
		{Resource: framework.StorageClass, ActionType: framework.Update},
		{Resource: framework.CSINode, ActionType: framework.Add},
		{Resource: framework.CSINode, ActionType: framework.Update},
		{Resource: framework.CSIDriver, ActionType: framework.Add},
		{Resource: framework.CSIDriver, ActionType: framework.Update},
		{Resource: framework.CSIStorageCapacity, ActionType: framework.Add},
		{Resource: framework.CSIStorageCapacity, ActionType: framework.Update},
	}

	pluginNum := 20
	var plugins []string
	// Mimic that we have 20 plugins loaded in runtime.
	for i := 0; i < pluginNum; i++ {
		plugins = append(plugins, fmt.Sprintf("fake-plugin-%v", i))
	}

	for _, tt := range tests {
		for _, podsInUnschedulablePods := range []int{1000, 5000} {
			b.Run(fmt.Sprintf("%v-%v", tt.name, podsInUnschedulablePods), func(b *testing.B) {
				logger, ctx := ktesting.NewTestContext(b)
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					c := testingclock.NewFakeClock(time.Now())

					m := makeEmptyQueueingHintMapPerProfile()
					// - All plugins registered for events[0], which is NodeAdd.
					// - 1/2 of plugins registered for events[1]
					// - 1/3 of plugins registered for events[2]
					// - ...
					for j := 0; j < len(events); j++ {
						for k := 0; k < len(plugins); k++ {
							if (k+1)%(j+1) == 0 {
								m[""][events[j]] = append(m[""][events[j]], &QueueingHintFunction{
									PluginName:     plugins[k],
									QueueingHintFn: queueHintReturnQueue,
								})
							}
						}
					}

					ctx, cancel := context.WithCancel(ctx)
					defer cancel()
					q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c), WithQueueingHintMapPerProfile(m))

					// Init pods in unschedulablePods.
					for j := 0; j < podsInUnschedulablePods; j++ {
						p := podTemplates[j%len(podTemplates)].DeepCopy()
						p.Name, p.UID = fmt.Sprintf("%v-%v", p.Name, j), types.UID(fmt.Sprintf("%v-%v", p.UID, j))
						var podInfo *framework.QueuedPodInfo
						// The ultimate goal of composing each PodInfo is to cover the path that intersects
						// (unschedulable) plugin names with the plugins that register the moveEvent,
						// here the rational is:
						// - in baseline case, don't inject unschedulable plugin names, so podMatchesEvent()
						//   never gets executed.
						// - in worst case, make both ends (of the intersection) a big number,i.e.,
						//   M intersected with N instead of M with 1 (or 1 with N)
						// - in random case, each pod failed by a random plugin, and also the moveEvent
						//   is randomized.
						if tt.name == "baseline" {
							podInfo = q.newQueuedPodInfo(p)
						} else if tt.name == "worst" {
							// Each pod failed by all plugins.
							podInfo = q.newQueuedPodInfo(p, plugins...)
						} else {
							// Random case.
							podInfo = q.newQueuedPodInfo(p, plugins[j%len(plugins)])
						}
						err := q.AddUnschedulableIfNotPresent(logger, podInfo, q.SchedulingCycle())
						if err != nil {
							b.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
						}
					}

					b.StartTimer()
					if tt.moveEvent.Resource != "" {
						q.MoveAllToActiveOrBackoffQueue(logger, tt.moveEvent, nil, nil, nil)
					} else {
						// Random case.
						q.MoveAllToActiveOrBackoffQueue(logger, events[i%len(events)], nil, nil, nil)
					}
				}
			})
		}
	}
}

func TestPriorityQueue_MoveAllToActiveOrBackoffQueueWithQueueingHint(t *testing.T) {
	now := time.Now()
	p := st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()
	tests := []struct {
		name    string
		podInfo *framework.QueuedPodInfo
		hint    framework.QueueingHintFn
		// duration is the duration that the Pod has been in the unschedulable queue.
		duration time.Duration
		// expectedQ is the queue name (activeQ, backoffQ, or unschedulablePods) that this Pod should be quened to.
		expectedQ string
	}{
		{
			name: "Queue queues pod to activeQ",
			// This pod has PendingPlugins and hence will be pushed directly to activeQ
			podInfo:   &framework.QueuedPodInfo{PodInfo: mustNewPodInfo(p), PendingPlugins: sets.New("foo")},
			hint:      queueHintReturnQueue,
			expectedQ: activeQ,
		},
		{
			name: "Queue queues pod to backoffQ if Pod is backing off",
			// needs UnschedulableCount to make it backing off.
			podInfo:   &framework.QueuedPodInfo{PodInfo: mustNewPodInfo(p), UnschedulableCount: 1, UnschedulablePlugins: sets.New("foo")},
			hint:      queueHintReturnQueue,
			expectedQ: backoffQ,
		},
		{
			name:      "Queue queues pod to activeQ if Pod is not backing off",
			podInfo:   &framework.QueuedPodInfo{PodInfo: mustNewPodInfo(p), UnschedulablePlugins: sets.New("foo")},
			hint:      queueHintReturnQueue,
			duration:  DefaultPodInitialBackoffDuration, // backoff is finished
			expectedQ: activeQ,
		},
		{
			name:      "Skip queues pod to unschedulablePods",
			podInfo:   &framework.QueuedPodInfo{PodInfo: mustNewPodInfo(p), UnschedulablePlugins: sets.New("foo")},
			hint:      queueHintReturnSkip,
			expectedQ: unschedulablePods,
		},
		{
			name:    "QueueHintFunction is not called when Pod is gated by SchedulingGates plugin",
			podInfo: setQueuedPodInfoGated(&framework.QueuedPodInfo{PodInfo: mustNewPodInfo(p), UnschedulablePlugins: sets.New(names.SchedulingGates, "foo")}),
			hint: func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
				return framework.Queue, fmt.Errorf("QueueingHintFn should not be called as pod is gated")
			},
			expectedQ: unschedulablePods,
		},
		{
			name:      "QueueHintFunction is called when Pod is gated by a plugin other than SchedulingGates",
			podInfo:   setQueuedPodInfoGated(&framework.QueuedPodInfo{PodInfo: mustNewPodInfo(p), UnschedulablePlugins: sets.New("foo")}),
			hint:      queueHintReturnQueue,
			expectedQ: backoffQ,
		},
		{
			name: "Pod that experienced a scheduling failure before should be queued to backoffQ after un-gated",
			// needs UnschedulableCount to make it backing off.
			podInfo:   setQueuedPodInfoGated(&framework.QueuedPodInfo{PodInfo: mustNewPodInfo(p), UnschedulableCount: 1, UnschedulablePlugins: sets.New("foo")}),
			hint:      queueHintReturnQueue,
			expectedQ: backoffQ,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			m := makeEmptyQueueingHintMapPerProfile()
			m[""][nodeAdd] = []*QueueingHintFunction{
				{
					PluginName:     "foo",
					QueueingHintFn: test.hint,
				},
			}
			cl := testingclock.NewFakeClock(now)
			q := NewTestQueue(ctx, newDefaultQueueSort(), WithQueueingHintMapPerProfile(m), WithClock(cl))
			q.Add(logger, test.podInfo.Pod)
			if p, err := q.Pop(logger); err != nil || p.Pod != test.podInfo.Pod {
				t.Errorf("Expected: %v after Pop, but got: %v", test.podInfo.Pod.Name, p.Pod.Name)
			}
			// add to unsched pod pool
			err := q.AddUnschedulableIfNotPresent(logger, test.podInfo, q.SchedulingCycle())
			if err != nil {
				t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
			}
			cl.Step(test.duration)

			q.MoveAllToActiveOrBackoffQueue(logger, nodeAdd, nil, nil, nil)

			if q.podBackoffQ.Len() == 0 && test.expectedQ == backoffQ {
				t.Fatalf("expected pod to be queued to backoffQ, but it was not")
			}

			if q.activeQ.len() == 0 && test.expectedQ == activeQ {
				t.Fatalf("expected pod to be queued to activeQ, but it was not")
			}

			if q.unschedulablePods.get(test.podInfo.Pod) == nil && test.expectedQ == unschedulablePods {
				t.Fatalf("expected pod to be queued to unschedulablePods, but it was not")
			}
		})
	}
}

func TestPriorityQueue_MoveAllToActiveOrBackoffQueue(t *testing.T) {
	c := testingclock.NewFakeClock(time.Now())
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := makeEmptyQueueingHintMapPerProfile()
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, true)

	m[""][nodeAdd] = []*QueueingHintFunction{
		{
			PluginName:     "fooPlugin",
			QueueingHintFn: queueHintReturnQueue,
		},
	}
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c), WithQueueingHintMapPerProfile(m))
	// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent()s below.
	q.Add(logger, unschedulablePodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != unschedulablePodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePodInfo.Pod.Name, p.Pod.Name)
	}
	expectInFlightPods(t, q, unschedulablePodInfo.Pod.UID)
	q.Add(logger, highPriorityPodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != highPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	expectInFlightPods(t, q, unschedulablePodInfo.Pod.UID, highPriorityPodInfo.Pod.UID)
	err := q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(unschedulablePodInfo.Pod, "fooPlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	err = q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(highPriorityPodInfo.Pod, "fooPlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	expectInFlightPods(t, q)
	// Construct a Pod, but don't associate its scheduler failure to any plugin
	hpp1 := clonePod(highPriorityPodInfo.Pod, "hpp1")
	q.Add(logger, hpp1)
	if p, err := q.Pop(logger); err != nil || p.Pod != hpp1 {
		t.Errorf("Expected: %v after Pop, but got: %v", hpp1, p.Pod.Name)
	}
	expectInFlightPods(t, q, hpp1.UID)
	// This Pod will go to backoffQ because no failure plugin is associated with it.
	hpp1PodInfo := q.newQueuedPodInfo(hpp1)
	hpp1PodInfo.UnschedulableCount++
	err = q.AddUnschedulableIfNotPresent(logger, hpp1PodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	expectInFlightPods(t, q)
	// Construct another Pod, and associate its scheduler failure to plugin "barPlugin".
	hpp2 := clonePod(highPriorityPodInfo.Pod, "hpp2")
	q.Add(logger, hpp2)
	if p, err := q.Pop(logger); err != nil || p.Pod != hpp2 {
		t.Errorf("Expected: %v after Pop, but got: %v", hpp2, p.Pod.Name)
	}
	expectInFlightPods(t, q, hpp2.UID)
	// This Pod will go to the unschedulable Pod pool.
	err = q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(hpp2, "barPlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	expectInFlightPods(t, q)
	// This NodeAdd event moves unschedulablePodInfo and highPriorityPodInfo to the backoffQ,
	// because of the queueing hint function registered for NodeAdd/fooPlugin.
	q.MoveAllToActiveOrBackoffQueue(logger, nodeAdd, nil, nil, nil)
	q.Add(logger, medPriorityPodInfo.Pod)
	if q.activeQ.len() != 1 {
		t.Errorf("Expected 1 item to be in activeQ, but got: %v", q.activeQ.len())
	}
	// Pop out the medPriorityPodInfo in activeQ.
	if p, err := q.Pop(logger); err != nil || p.Pod != medPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod, p.Pod.Name)
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID)
	// hpp2 won't be moved.
	if q.podBackoffQ.Len() != 3 {
		t.Fatalf("Expected 3 items to be in podBackoffQ, but got: %v", q.podBackoffQ.Len())
	}

	// pop out the pods in the backoffQ.
	// This doesn't make them in-flight pods.
	for q.podBackoffQ.Len() != 0 {
		q.podBackoffQ.Pop()
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID)

	q.Add(logger, unschedulablePodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != unschedulablePodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePodInfo.Pod.Name, p.Pod.Name)
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID, unschedulablePodInfo.Pod.UID)
	q.Add(logger, highPriorityPodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != highPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID, unschedulablePodInfo.Pod.UID, highPriorityPodInfo.Pod.UID)
	q.Add(logger, hpp1)
	if p, err := q.Pop(logger); err != nil || p.Pod != hpp1 {
		t.Errorf("Expected: %v after Pop, but got: %v", hpp1, p.Pod.Name)
	}
	unschedulableQueuedPodInfo := q.newQueuedPodInfo(unschedulablePodInfo.Pod, "fooPlugin")
	highPriorityQueuedPodInfo := q.newQueuedPodInfo(highPriorityPodInfo.Pod, "fooPlugin")
	hpp1QueuedPodInfo := q.newQueuedPodInfo(hpp1)
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID, unschedulablePodInfo.Pod.UID, highPriorityPodInfo.Pod.UID, hpp1.UID)
	err = q.AddUnschedulableIfNotPresent(logger, unschedulableQueuedPodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID, highPriorityPodInfo.Pod.UID, hpp1.UID)
	err = q.AddUnschedulableIfNotPresent(logger, highPriorityQueuedPodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID, hpp1.UID)
	err = q.AddUnschedulableIfNotPresent(logger, hpp1QueuedPodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID)
	q.Add(logger, medPriorityPodInfo.Pod)
	// hpp1 will go to backoffQ because no failure plugin is associated with it.
	// All plugins other than hpp1 are enqueued to the unschedulable Pod pool.
	for _, pod := range []*v1.Pod{unschedulablePodInfo.Pod, highPriorityPodInfo.Pod, hpp2} {
		if q.unschedulablePods.get(pod) == nil {
			t.Errorf("Expected %v in the unschedulablePods", pod.Name)
		}
	}
	if !q.podBackoffQ.Has(hpp1QueuedPodInfo) {
		t.Errorf("Expected %v in the podBackoffQ", hpp1.Name)
	}
	// all the remaining Pods should be in activeQ.
	if q.activeQ.len() != 1 {
		t.Errorf("Expected %v in the activeQ", medPriorityPodInfo.Pod.Name)
	}

	// Move clock by podInitialBackoffDuration, so that pods in the unschedulablePods would pass the backing off,
	// and the pods will be moved into activeQ.
	c.Step(q.podInitialBackoffDuration)
	q.flushBackoffQCompleted(logger) // flush the completed backoffQ to move hpp1 to activeQ.
	q.MoveAllToActiveOrBackoffQueue(logger, nodeAdd, nil, nil, nil)
	if q.activeQ.len() != 4 {
		t.Errorf("Expected 4 items to be in activeQ, but got: %v", q.activeQ.len())
	}
	if q.podBackoffQ.Len() != 0 {
		t.Errorf("Expected 0 item to be in podBackoffQ, but got: %v", q.podBackoffQ.Len())
	}
	expectInFlightPods(t, q, medPriorityPodInfo.Pod.UID)
	if len(q.unschedulablePods.podInfoMap) != 1 {
		// hpp2 won't be moved regardless of its backoff timer.
		t.Errorf("Expected 1 item to be in unschedulablePods, but got: %v", len(q.unschedulablePods.podInfoMap))
	}
}

func TestPriorityQueue_MoveAllToActiveOrBackoffQueueWithoutQueueingHint(t *testing.T) {
	c := testingclock.NewFakeClock(time.Now())
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := makeEmptyQueueingHintMapPerProfile()
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, false)
	m[""][nodeAdd] = []*QueueingHintFunction{
		{
			PluginName:     "fooPlugin",
			QueueingHintFn: queueHintReturnQueue,
		},
	}
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c), WithQueueingHintMapPerProfile(m))
	// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent()s below.
	q.Add(logger, medPriorityPodInfo.Pod)

	err := q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(unschedulablePodInfo.Pod, "fooPlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	err = q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(highPriorityPodInfo.Pod, "fooPlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	// Construct a Pod, but don't associate its scheduler failure to any plugin
	hpp1 := clonePod(highPriorityPodInfo.Pod, "hpp1")
	// This Pod will go to backoffQ because no failure plugin is associated with it.
	hpp1PodInfo := q.newQueuedPodInfo(hpp1)
	hpp1PodInfo.UnschedulableCount++
	err = q.AddUnschedulableIfNotPresent(logger, hpp1PodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	// Construct another Pod, and associate its scheduler failure to plugin "barPlugin".
	hpp2 := clonePod(highPriorityPodInfo.Pod, "hpp2")
	// This Pod will go to the unschedulable Pod pool.
	err = q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(hpp2, "barPlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	// This NodeAdd event moves unschedulablePodInfo and highPriorityPodInfo to the backoffQ,
	// because of the queueing hint function registered for NodeAdd/fooPlugin.
	q.MoveAllToActiveOrBackoffQueue(logger, nodeAdd, nil, nil, nil)
	if q.activeQ.len() != 1 {
		t.Errorf("Expected 1 item to be in activeQ, but got: %v", q.activeQ.len())
	}
	// Pop out the medPriorityPodInfo in activeQ.
	if p, err := q.Pop(logger); err != nil || p.Pod != medPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod, p.Pod.Name)
	}
	// hpp2 won't be moved.
	if q.podBackoffQ.Len() != 3 {
		t.Fatalf("Expected 3 items to be in podBackoffQ, but got: %v", q.podBackoffQ.Len())
	}

	// pop out the pods in the backoffQ.
	// This doesn't make them in-flight pods.
	for q.podBackoffQ.Len() != 0 {
		_, err = q.podBackoffQ.Pop()
		if err != nil {
			t.Errorf("pop failed: %v", err)
		}
	}

	unschedulableQueuedPodInfo := q.newQueuedPodInfo(unschedulablePodInfo.Pod, "fooPlugin")
	highPriorityQueuedPodInfo := q.newQueuedPodInfo(highPriorityPodInfo.Pod, "fooPlugin")
	hpp1QueuedPodInfo := q.newQueuedPodInfo(hpp1)
	err = q.AddUnschedulableIfNotPresent(logger, unschedulableQueuedPodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	err = q.AddUnschedulableIfNotPresent(logger, highPriorityQueuedPodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	err = q.AddUnschedulableIfNotPresent(logger, hpp1QueuedPodInfo, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	q.Add(logger, medPriorityPodInfo.Pod)
	// hpp1 will go to backoffQ because no failure plugin is associated with it.
	// All plugins other than hpp1 are enqueued to the unschedulable Pod pool.
	for _, pod := range []*v1.Pod{unschedulablePodInfo.Pod, highPriorityPodInfo.Pod, hpp2} {
		if q.unschedulablePods.get(pod) == nil {
			t.Errorf("Expected %v in the unschedulablePods", pod.Name)
		}
	}

	if !q.podBackoffQ.Has(hpp1QueuedPodInfo) {
		t.Errorf("Expected %v in the podBackoffQ", hpp1.Name)
	}

	// the remaining Pods should be in activeQ.
	if q.activeQ.len() != 1 {
		t.Errorf("Expected %v in the activeQ", medPriorityPodInfo.Pod.Name)
	}

	// Move clock by podInitialBackoffDuration, so that pods in the unschedulablePods would pass the backing off,
	// and the pods will be moved into activeQ.
	c.Step(q.podInitialBackoffDuration)
	q.flushBackoffQCompleted(logger) // flush the completed backoffQ to move hpp1 to activeQ.
	q.MoveAllToActiveOrBackoffQueue(logger, nodeAdd, nil, nil, nil)
	if q.activeQ.len() != 4 {
		t.Errorf("Expected 4 items to be in activeQ, but got: %v", q.activeQ.len())
	}
	if q.podBackoffQ.Len() != 0 {
		t.Errorf("Expected 0 item to be in podBackoffQ, but got: %v", q.podBackoffQ.Len())
	}
	if len(q.unschedulablePods.podInfoMap) != 1 {
		// hpp2 won't be moved regardless of its backoff timer.
		t.Errorf("Expected 1 item to be in unschedulablePods, but got: %v", len(q.unschedulablePods.podInfoMap))
	}
}

func clonePod(pod *v1.Pod, newName string) *v1.Pod {
	pod = pod.DeepCopy()
	pod.Name = newName
	pod.UID = types.UID(pod.Name + pod.Namespace)
	return pod
}

func expectInFlightPods(t *testing.T, q *PriorityQueue, uids ...types.UID) {
	t.Helper()
	var actualUIDs []types.UID
	for _, pod := range q.activeQ.listInFlightPods() {
		actualUIDs = append(actualUIDs, pod.UID)
	}
	sortUIDs := cmpopts.SortSlices(func(a, b types.UID) bool { return a < b })
	if diff := cmp.Diff(uids, actualUIDs, sortUIDs); diff != "" {
		t.Fatalf("Unexpected content of inFlightPods (-want, +have):\n%s", diff)
	}
	actualUIDs = nil
	events := q.activeQ.listInFlightEvents()
	for _, e := range events {
		if pod, ok := e.(*v1.Pod); ok {
			actualUIDs = append(actualUIDs, pod.UID)
		}
	}
	if diff := cmp.Diff(uids, actualUIDs, sortUIDs); diff != "" {
		t.Fatalf("Unexpected pods in inFlightEvents (-want, +have):\n%s", diff)
	}
}

// TestPriorityQueue_AssignedPodAdded tests AssignedPodAdded. It checks that
// when a pod with pod affinity is in unschedulablePods and another pod with a
// matching label is added, the unschedulable pod is moved to activeQ.
func TestPriorityQueue_AssignedPodAdded_(t *testing.T) {
	tests := []struct {
		name               string
		unschedPod         *v1.Pod
		unschedPlugin      string
		updatedAssignedPod *v1.Pod
		wantToRequeue      bool
	}{
		{
			name:               "Pod rejected by pod affinity is requeued with matching Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").PodAffinityExists("service", "region", st.PodAffinityWithRequiredReq).Obj(),
			unschedPlugin:      names.InterPodAffinity,
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("ns1").Label("service", "securityscan").Node("node1").Obj(),
			wantToRequeue:      true,
		},
		{
			name:               "Pod rejected by pod affinity isn't requeued with unrelated Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").PodAffinityExists("service", "region", st.PodAffinityWithRequiredReq).Obj(),
			unschedPlugin:      names.InterPodAffinity,
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("unrelated").Label("unrelated", "unrelated").Node("node1").Obj(),
			wantToRequeue:      false,
		},
		{
			name:               "Pod rejected by pod topology spread is requeued with Pod's update in the same namespace",
			unschedPod:         st.MakePod().Name("tsp").Namespace("ns1").UID("tsp").SpreadConstraint(1, "node", v1.DoNotSchedule, nil, nil, nil, nil, nil).Obj(),
			unschedPlugin:      names.PodTopologySpread,
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("ns1").Label("service", "securityscan").Node("node1").Obj(),
			wantToRequeue:      true,
		},
		{
			name:               "Pod rejected by pod topology spread isn't requeued with unrelated Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").PodAffinityExists("service", "region", st.PodAffinityWithRequiredReq).Obj(),
			unschedPlugin:      names.PodTopologySpread,
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("unrelated").Label("unrelated", "unrelated").Node("node1").Obj(),
			wantToRequeue:      false,
		},
		{
			name:               "Pod rejected by other plugins isn't requeued with any Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").Obj(),
			unschedPlugin:      "fakePlugin",
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("unrelated").Label("unrelated", "unrelated").Node("node1").Obj(),
			wantToRequeue:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			c := testingclock.NewFakeClock(time.Now())
			m := makeEmptyQueueingHintMapPerProfile()
			m[""][framework.EventAssignedPodAdd] = []*QueueingHintFunction{
				{
					PluginName:     "fakePlugin",
					QueueingHintFn: queueHintReturnQueue,
				},
				{
					PluginName:     names.InterPodAffinity,
					QueueingHintFn: queueHintReturnQueue,
				},
				{
					PluginName:     names.PodTopologySpread,
					QueueingHintFn: queueHintReturnQueue,
				},
			}
			q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c), WithQueueingHintMapPerProfile(m))

			// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent()s below.
			q.Add(logger, tt.unschedPod)
			if p, err := q.Pop(logger); err != nil || p.Pod != tt.unschedPod {
				t.Errorf("Expected: %v after Pop, but got: %v", tt.unschedPod.Name, p.Pod.Name)
			}

			err := q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(tt.unschedPod, tt.unschedPlugin), q.SchedulingCycle())
			if err != nil {
				t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
			}

			// Move clock to make the unschedulable pods complete backoff.
			c.Step(DefaultPodInitialBackoffDuration + time.Second)

			q.AssignedPodAdded(logger, tt.updatedAssignedPod)

			if q.activeQ.has(newQueuedPodInfoForLookup(tt.unschedPod)) != tt.wantToRequeue {
				t.Fatalf("unexpected Pod move: Pod should be requeued: %v. Pod is actually requeued: %v", tt.wantToRequeue, !tt.wantToRequeue)
			}
		})
	}
}

func TestPriorityQueue_AssignedPodUpdated(t *testing.T) {
	tests := []struct {
		name               string
		unschedPod         *v1.Pod
		unschedPlugin      string
		updatedAssignedPod *v1.Pod
		event              framework.ClusterEvent
		wantToRequeue      bool
	}{
		{
			name:               "Pod rejected by pod affinity is requeued with matching Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").PodAffinityExists("service", "region", st.PodAffinityWithRequiredReq).Obj(),
			unschedPlugin:      names.InterPodAffinity,
			event:              framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodLabel},
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("ns1").Label("service", "securityscan").Node("node1").Obj(),
			wantToRequeue:      true,
		},
		{
			name:               "Pod rejected by pod affinity isn't requeued with unrelated Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").PodAffinityExists("service", "region", st.PodAffinityWithRequiredReq).Obj(),
			unschedPlugin:      names.InterPodAffinity,
			event:              framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodLabel},
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("unrelated").Label("unrelated", "unrelated").Node("node1").Obj(),
			wantToRequeue:      false,
		},
		{
			name:               "Pod rejected by pod topology spread is requeued with Pod's update in the same namespace",
			unschedPod:         st.MakePod().Name("tsp").Namespace("ns1").UID("tsp").SpreadConstraint(1, "node", v1.DoNotSchedule, nil, nil, nil, nil, nil).Obj(),
			unschedPlugin:      names.PodTopologySpread,
			event:              framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodLabel},
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("ns1").Label("service", "securityscan").Node("node1").Obj(),
			wantToRequeue:      true,
		},
		{
			name:               "Pod rejected by pod topology spread isn't requeued with unrelated Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").PodAffinityExists("service", "region", st.PodAffinityWithRequiredReq).Obj(),
			unschedPlugin:      names.PodTopologySpread,
			event:              framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodLabel},
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("unrelated").Label("unrelated", "unrelated").Node("node1").Obj(),
			wantToRequeue:      false,
		},
		{
			name:               "Pod rejected by resource fit is requeued with assigned Pod's scale down",
			unschedPod:         st.MakePod().Name("rp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
			unschedPlugin:      names.NodeResourcesFit,
			event:              framework.ClusterEvent{Resource: framework.EventResource("AssignedPod"), ActionType: framework.UpdatePodScaleDown},
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("ns2").Node("node1").Obj(),
			wantToRequeue:      true,
		},
		{
			name:               "Pod rejected by other plugins isn't requeued with any Pod's update",
			unschedPod:         st.MakePod().Name("afp").Namespace("ns1").UID("afp").Annotation("annot2", "val2").Obj(),
			unschedPlugin:      "fakePlugin",
			event:              framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodLabel},
			updatedAssignedPod: st.MakePod().Name("lbp").Namespace("unrelated").Label("unrelated", "unrelated").Node("node1").Obj(),
			wantToRequeue:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			c := testingclock.NewFakeClock(time.Now())
			m := makeEmptyQueueingHintMapPerProfile()
			m[""] = map[framework.ClusterEvent][]*QueueingHintFunction{
				{Resource: framework.Pod, ActionType: framework.UpdatePodLabel}: {
					{
						PluginName:     "fakePlugin",
						QueueingHintFn: queueHintReturnQueue,
					},
					{
						PluginName:     names.InterPodAffinity,
						QueueingHintFn: queueHintReturnQueue,
					},
					{
						PluginName:     names.PodTopologySpread,
						QueueingHintFn: queueHintReturnQueue,
					},
				},
				{Resource: framework.Pod, ActionType: framework.UpdatePodScaleDown}: {
					{
						PluginName:     names.NodeResourcesFit,
						QueueingHintFn: queueHintReturnQueue,
					},
				},
			}
			q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c), WithQueueingHintMapPerProfile(m))

			// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent()s below.
			q.Add(logger, tt.unschedPod)
			if p, err := q.Pop(logger); err != nil || p.Pod != tt.unschedPod {
				t.Errorf("Expected: %v after Pop, but got: %v", tt.unschedPod.Name, p.Pod.Name)
			}

			err := q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(tt.unschedPod, tt.unschedPlugin), q.SchedulingCycle())
			if err != nil {
				t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
			}

			// Move clock to make the unschedulable pods complete backoff.
			c.Step(DefaultPodInitialBackoffDuration + time.Second)

			q.AssignedPodUpdated(logger, nil, tt.updatedAssignedPod, tt.event)

			if q.activeQ.has(newQueuedPodInfoForLookup(tt.unschedPod)) != tt.wantToRequeue {
				t.Fatalf("unexpected Pod move: Pod should be requeued: %v. Pod is actually requeued: %v", tt.wantToRequeue, !tt.wantToRequeue)
			}
		})
	}
}

func TestPriorityQueue_NominatedPodsForNode(t *testing.T) {
	objs := []runtime.Object{medPriorityPodInfo.Pod, unschedulablePodInfo.Pod, highPriorityPodInfo.Pod}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)
	q.Add(logger, medPriorityPodInfo.Pod)
	q.Add(logger, unschedulablePodInfo.Pod)
	q.Add(logger, highPriorityPodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != highPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	expectedList := []*framework.PodInfo{medPriorityPodInfo, unschedulablePodInfo}
	podInfos := q.NominatedPodsForNode("node1")
	if diff := cmp.Diff(expectedList, podInfos, cmpopts.IgnoreUnexported(framework.PodInfo{})); diff != "" {
		t.Errorf("Unexpected list of nominated Pods for node: (-want, +got):\n%s", diff)
	}
	podInfos[0].Pod.Name = "not mpp"
	if diff := cmp.Diff(podInfos, q.NominatedPodsForNode("node1"), cmpopts.IgnoreUnexported(framework.PodInfo{})); diff == "" {
		t.Error("Expected list of nominated Pods for node2 is different from podInfos")
	}
	if len(q.NominatedPodsForNode("node2")) != 0 {
		t.Error("Expected list of nominated Pods for node2 to be empty.")
	}
}

func TestPriorityQueue_NominatedPodDeleted(t *testing.T) {
	tests := []struct {
		name      string
		podInfo   *framework.PodInfo
		deletePod bool
		wantLen   int
	}{
		{
			name:    "alive pod gets added into PodNominator",
			podInfo: medPriorityPodInfo,
			wantLen: 1,
		},
		{
			name:      "deleted pod shouldn't be added into PodNominator",
			podInfo:   highPriNominatedPodInfo,
			deletePod: true,
			wantLen:   0,
		},
		{
			name:    "pod without .status.nominatedPodName specified shouldn't be added into PodNominator",
			podInfo: highPriorityPodInfo,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			cs := fake.NewClientset(tt.podInfo.Pod)
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podLister := informerFactory.Core().V1().Pods().Lister()

			// Build a PriorityQueue.
			q := NewPriorityQueue(newDefaultQueueSort(), informerFactory, WithPodLister(podLister))
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			informerFactory.Start(ctx.Done())
			informerFactory.WaitForCacheSync(ctx.Done())

			if tt.deletePod {
				// Simulate that the test pod gets deleted physically.
				informerFactory.Core().V1().Pods().Informer().GetStore().Delete(tt.podInfo.Pod)
			}

			q.AddNominatedPod(logger, tt.podInfo, nil)

			if got := len(q.NominatedPodsForNode(tt.podInfo.Pod.Status.NominatedNodeName)); got != tt.wantLen {
				t.Errorf("Expected %v nominated pods for node, but got %v", tt.wantLen, got)
			}
		})
	}
}

func TestPriorityQueue_PendingPods(t *testing.T) {
	makeSet := func(pods []*v1.Pod) map[*v1.Pod]struct{} {
		pendingSet := map[*v1.Pod]struct{}{}
		for _, p := range pods {
			pendingSet[p] = struct{}{}
		}
		return pendingSet
	}

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort())
	// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent()s below.
	q.Add(logger, unschedulablePodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != unschedulablePodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePodInfo.Pod.Name, p.Pod.Name)
	}
	q.Add(logger, highPriorityPodInfo.Pod)
	if p, err := q.Pop(logger); err != nil || p.Pod != highPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	q.Add(logger, medPriorityPodInfo.Pod)
	err := q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(unschedulablePodInfo.Pod, "plugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	err = q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(highPriorityPodInfo.Pod, "plugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}

	expectedSet := makeSet([]*v1.Pod{medPriorityPodInfo.Pod, unschedulablePodInfo.Pod, highPriorityPodInfo.Pod})
	gotPods, gotSummary := q.PendingPods()
	if diff := cmp.Diff(expectedSet, makeSet(gotPods)); diff != "" {
		t.Errorf("Unexpected list of pending Pods (-want, +got):\n%s", diff)
	}
	if wantSummary := fmt.Sprintf(pendingPodsSummary, 1, 0, 2); wantSummary != gotSummary {
		t.Errorf("Unexpected pending pods summary: want %v, but got %v.", wantSummary, gotSummary)
	}
	// Move all to active queue. We should still see the same set of pods.
	q.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)
	gotPods, gotSummary = q.PendingPods()
	if diff := cmp.Diff(expectedSet, makeSet(gotPods)); diff != "" {
		t.Errorf("Unexpected list of pending Pods (-want, +got):\n%s", diff)
	}
	if wantSummary := fmt.Sprintf(pendingPodsSummary, 1, 2, 0); wantSummary != gotSummary {
		t.Errorf("Unexpected pending pods summary: want %v, but got %v.", wantSummary, gotSummary)
	}
}

func TestPriorityQueue_UpdateNominatedPodForNode(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	objs := []runtime.Object{medPriorityPodInfo.Pod, unschedulablePodInfo.Pod, highPriorityPodInfo.Pod, scheduledPodInfo.Pod}
	q := NewTestQueueWithObjects(ctx, newDefaultQueueSort(), objs)
	q.Add(logger, medPriorityPodInfo.Pod)
	// Update unschedulablePodInfo on a different node than specified in the pod.
	q.AddNominatedPod(logger, mustNewTestPodInfo(t, unschedulablePodInfo.Pod),
		&framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: "node5"})

	// Update nominated node name of a pod on a node that is not specified in the pod object.
	q.AddNominatedPod(logger, mustNewTestPodInfo(t, highPriorityPodInfo.Pod),
		&framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: "node2"})
	expectedNominatedPods := &nominator{
		nominatedPodToNode: map[types.UID]string{
			medPriorityPodInfo.Pod.UID:   "node1",
			highPriorityPodInfo.Pod.UID:  "node2",
			unschedulablePodInfo.Pod.UID: "node5",
		},
		nominatedPods: map[string][]podRef{
			"node1": {podToRef(medPriorityPodInfo.Pod)},
			"node2": {podToRef(highPriorityPodInfo.Pod)},
			"node5": {podToRef(unschedulablePodInfo.Pod)},
		},
	}
	if diff := cmp.Diff(q.nominator, expectedNominatedPods, nominatorCmpOpts...); diff != "" {
		t.Errorf("Unexpected diff after adding pods (-want, +got):\n%s", diff)
	}
	if p, err := q.Pop(logger); err != nil || p.Pod != medPriorityPodInfo.Pod {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	// List of nominated pods shouldn't change after popping them from the queue.
	if diff := cmp.Diff(q.nominator, expectedNominatedPods, nominatorCmpOpts...); diff != "" {
		t.Errorf("Unexpected diff after popping pods (-want, +got):\n%s", diff)
	}
	// Update one of the nominated pods that doesn't have nominatedNodeName in the
	// pod object. It should be updated correctly.
	q.AddNominatedPod(logger, highPriorityPodInfo, &framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: "node4"})
	expectedNominatedPods = &nominator{
		nominatedPodToNode: map[types.UID]string{
			medPriorityPodInfo.Pod.UID:   "node1",
			highPriorityPodInfo.Pod.UID:  "node4",
			unschedulablePodInfo.Pod.UID: "node5",
		},
		nominatedPods: map[string][]podRef{
			"node1": {podToRef(medPriorityPodInfo.Pod)},
			"node4": {podToRef(highPriorityPodInfo.Pod)},
			"node5": {podToRef(unschedulablePodInfo.Pod)},
		},
	}
	if diff := cmp.Diff(q.nominator, expectedNominatedPods, nominatorCmpOpts...); diff != "" {
		t.Errorf("Unexpected diff after updating pods (-want, +got):\n%s", diff)
	}

	// Attempt to nominate a pod that was deleted from the informer cache.
	// Nothing should change.
	q.AddNominatedPod(logger, nonExistentPodInfo, &framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: "node1"})
	if diff := cmp.Diff(q.nominator, expectedNominatedPods, nominatorCmpOpts...); diff != "" {
		t.Errorf("Unexpected diff after nominating a deleted pod (-want, +got):\n%s", diff)
	}
	// Attempt to nominate a pod that was already scheduled in the informer cache.
	// Nothing should change.
	scheduledPodCopy := scheduledPodInfo.Pod.DeepCopy()
	scheduledPodInfo.Pod.Spec.NodeName = ""
	q.AddNominatedPod(logger, mustNewTestPodInfo(t, scheduledPodCopy), &framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: "node1"})
	if diff := cmp.Diff(q.nominator, expectedNominatedPods, nominatorCmpOpts...); diff != "" {
		t.Errorf("Unexpected diff after nominating a scheduled pod (-want, +got):\n%s", diff)
	}

	// Delete a nominated pod that doesn't have nominatedNodeName in the pod
	// object. It should be deleted.
	q.DeleteNominatedPodIfExists(highPriorityPodInfo.Pod)
	expectedNominatedPods = &nominator{
		nominatedPodToNode: map[types.UID]string{
			medPriorityPodInfo.Pod.UID:   "node1",
			unschedulablePodInfo.Pod.UID: "node5",
		},
		nominatedPods: map[string][]podRef{
			"node1": {podToRef(medPriorityPodInfo.Pod)},
			"node5": {podToRef(unschedulablePodInfo.Pod)},
		},
	}
	if diff := cmp.Diff(q.nominator, expectedNominatedPods, nominatorCmpOpts...); diff != "" {
		t.Errorf("Unexpected diff after deleting pods (-want, +got):\n%s", diff)
	}
}

func TestPriorityQueue_NewWithOptions(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx,
		newDefaultQueueSort(),
		WithPodInitialBackoffDuration(2*time.Second),
		WithPodMaxBackoffDuration(20*time.Second),
	)

	if q.podInitialBackoffDuration != 2*time.Second {
		t.Errorf("Unexpected pod backoff initial duration. Expected: %v, got: %v", 2*time.Second, q.podInitialBackoffDuration)
	}

	if q.podMaxBackoffDuration != 20*time.Second {
		t.Errorf("Unexpected pod backoff max duration. Expected: %v, got: %v", 2*time.Second, q.podMaxBackoffDuration)
	}
}

func TestUnschedulablePodsMap(t *testing.T) {
	var pods = []*v1.Pod{
		st.MakePod().Name("p0").Namespace("ns1").Annotation("annot1", "val1").NominatedNodeName("node1").Obj(),
		st.MakePod().Name("p1").Namespace("ns1").Annotation("annot", "val").Obj(),
		st.MakePod().Name("p2").Namespace("ns2").Annotation("annot2", "val2").Annotation("annot3", "val3").NominatedNodeName("node3").Obj(),
		st.MakePod().Name("p3").Namespace("ns4").Annotation("annot2", "val2").Annotation("annot3", "val3").NominatedNodeName("node1").Obj(),
	}
	var updatedPods = make([]*v1.Pod, len(pods))
	updatedPods[0] = pods[0].DeepCopy()
	updatedPods[1] = pods[1].DeepCopy()
	updatedPods[3] = pods[3].DeepCopy()

	tests := []struct {
		name                   string
		podsToAdd              []*v1.Pod
		expectedMapAfterAdd    map[string]*framework.QueuedPodInfo
		podsToUpdate           []*v1.Pod
		expectedMapAfterUpdate map[string]*framework.QueuedPodInfo
		podsToDelete           []*v1.Pod
		expectedMapAfterDelete map[string]*framework.QueuedPodInfo
	}{
		{
			name:      "create, update, delete subset of pods",
			podsToAdd: []*v1.Pod{pods[0], pods[1], pods[2], pods[3]},
			expectedMapAfterAdd: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[0]): {PodInfo: mustNewTestPodInfo(t, pods[0]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[1]): {PodInfo: mustNewTestPodInfo(t, pods[1]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[2]): {PodInfo: mustNewTestPodInfo(t, pods[2]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[3]): {PodInfo: mustNewTestPodInfo(t, pods[3]), UnschedulablePlugins: sets.New[string]()},
			},
			podsToUpdate: []*v1.Pod{updatedPods[0]},
			expectedMapAfterUpdate: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[0]): {PodInfo: mustNewTestPodInfo(t, updatedPods[0]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[1]): {PodInfo: mustNewTestPodInfo(t, pods[1]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[2]): {PodInfo: mustNewTestPodInfo(t, pods[2]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[3]): {PodInfo: mustNewTestPodInfo(t, pods[3]), UnschedulablePlugins: sets.New[string]()},
			},
			podsToDelete: []*v1.Pod{pods[0], pods[1]},
			expectedMapAfterDelete: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[2]): {PodInfo: mustNewTestPodInfo(t, pods[2]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[3]): {PodInfo: mustNewTestPodInfo(t, pods[3]), UnschedulablePlugins: sets.New[string]()},
			},
		},
		{
			name:      "create, update, delete all",
			podsToAdd: []*v1.Pod{pods[0], pods[3]},
			expectedMapAfterAdd: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[0]): {PodInfo: mustNewTestPodInfo(t, pods[0]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[3]): {PodInfo: mustNewTestPodInfo(t, pods[3]), UnschedulablePlugins: sets.New[string]()},
			},
			podsToUpdate: []*v1.Pod{updatedPods[3]},
			expectedMapAfterUpdate: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[0]): {PodInfo: mustNewTestPodInfo(t, pods[0]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[3]): {PodInfo: mustNewTestPodInfo(t, updatedPods[3]), UnschedulablePlugins: sets.New[string]()},
			},
			podsToDelete:           []*v1.Pod{pods[0], pods[3]},
			expectedMapAfterDelete: map[string]*framework.QueuedPodInfo{},
		},
		{
			name:      "delete non-existing and existing pods",
			podsToAdd: []*v1.Pod{pods[1], pods[2]},
			expectedMapAfterAdd: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[1]): {PodInfo: mustNewTestPodInfo(t, pods[1]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[2]): {PodInfo: mustNewTestPodInfo(t, pods[2]), UnschedulablePlugins: sets.New[string]()},
			},
			podsToUpdate: []*v1.Pod{updatedPods[1]},
			expectedMapAfterUpdate: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[1]): {PodInfo: mustNewTestPodInfo(t, updatedPods[1]), UnschedulablePlugins: sets.New[string]()},
				util.GetPodFullName(pods[2]): {PodInfo: mustNewTestPodInfo(t, pods[2]), UnschedulablePlugins: sets.New[string]()},
			},
			podsToDelete: []*v1.Pod{pods[2], pods[3]},
			expectedMapAfterDelete: map[string]*framework.QueuedPodInfo{
				util.GetPodFullName(pods[1]): {PodInfo: mustNewTestPodInfo(t, updatedPods[1]), UnschedulablePlugins: sets.New[string]()},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upm := newUnschedulablePods(nil, nil)
			for _, p := range test.podsToAdd {
				upm.addOrUpdate(newQueuedPodInfoForLookup(p))
			}
			if diff := cmp.Diff(test.expectedMapAfterAdd, upm.podInfoMap, cmpopts.IgnoreUnexported(framework.PodInfo{})); diff != "" {
				t.Errorf("Unexpected map after adding pods(-want, +got):\n%s", diff)
			}

			if len(test.podsToUpdate) > 0 {
				for _, p := range test.podsToUpdate {
					upm.addOrUpdate(newQueuedPodInfoForLookup(p))
				}
				if diff := cmp.Diff(test.expectedMapAfterUpdate, upm.podInfoMap, cmpopts.IgnoreUnexported(framework.PodInfo{})); diff != "" {
					t.Errorf("Unexpected map after updating pods (-want, +got):\n%s", diff)
				}
			}
			for _, p := range test.podsToDelete {
				upm.delete(p, false)
			}
			if diff := cmp.Diff(test.expectedMapAfterDelete, upm.podInfoMap, cmpopts.IgnoreUnexported(framework.PodInfo{})); diff != "" {
				t.Errorf("Unexpected map after deleting pods (-want, +got):\n%s", diff)
			}
			upm.clear()
			if len(upm.podInfoMap) != 0 {
				t.Errorf("Expected the map to be empty, but has %v elements.", len(upm.podInfoMap))
			}
		})
	}
}

func TestSchedulingQueue_Close(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		pod, err := q.Pop(logger)
		if err != nil {
			t.Errorf("Expected nil err from Pop() if queue is closed, but got %q", err.Error())
		}
		if pod != nil {
			t.Errorf("Expected pod nil from Pop() if queue is closed, but got: %v", pod)
		}
	}()
	q.Close()
	wg.Wait()
}

// TestRecentlyTriedPodsGoBack tests that pods which are recently tried and are
// unschedulable go behind other pods with the same priority. This behavior
// ensures that an unschedulable pod does not block head of the queue when there
// are frequent events that move pods to the active queue.
func TestRecentlyTriedPodsGoBack(t *testing.T) {
	c := testingclock.NewFakeClock(time.Now())
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c))
	// Add a few pods to priority queue.
	for i := 0; i < 5; i++ {
		p := st.MakePod().Name(fmt.Sprintf("test-pod-%v", i)).Namespace("ns1").UID(fmt.Sprintf("tp00%v", i)).Priority(highPriority).Node("node1").NominatedNodeName("node1").Obj()
		q.Add(logger, p)
	}
	c.Step(time.Microsecond)
	// Simulate a pod being popped by the scheduler, determined unschedulable, and
	// then moved back to the active queue.
	p1, err := q.Pop(logger)
	if err != nil {
		t.Errorf("Error while popping the head of the queue: %v", err)
	}
	// Update pod condition to unschedulable.
	podutil.UpdatePodCondition(&p1.PodInfo.Pod.Status, &v1.PodCondition{
		Type:          v1.PodScheduled,
		Status:        v1.ConditionFalse,
		Reason:        v1.PodReasonUnschedulable,
		Message:       "fake scheduling failure",
		LastProbeTime: metav1.Now(),
	})
	p1.UnschedulablePlugins = sets.New("plugin")
	// Put in the unschedulable queue.
	err = q.AddUnschedulableIfNotPresent(logger, p1, q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	c.Step(DefaultPodInitialBackoffDuration)
	// Move all unschedulable pods to the active queue.
	q.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)
	// Simulation is over. Now let's pop all pods. The pod popped first should be
	// the last one we pop here.
	for i := 0; i < 5; i++ {
		p, err := q.Pop(logger)
		if err != nil {
			t.Errorf("Error while popping pods from the queue: %v", err)
		}
		if (i == 4) != (p1 == p) {
			t.Errorf("A pod tried before is not the last pod popped: i: %v, pod name: %v", i, p.PodInfo.Pod.Name)
		}
	}
}

// TestPodFailedSchedulingMultipleTimesDoesNotBlockNewerPod tests
// that a pod determined as unschedulable multiple times doesn't block any newer pod.
// This behavior ensures that an unschedulable pod does not block head of the queue when there
// are frequent events that move pods to the active queue.
func TestPodFailedSchedulingMultipleTimesDoesNotBlockNewerPod(t *testing.T) {
	c := testingclock.NewFakeClock(time.Now())
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c))

	// Add an unschedulable pod to a priority queue.
	// This makes a situation that the pod was tried to schedule
	// and had been determined unschedulable so far
	unschedulablePod := st.MakePod().Name("test-pod-unscheduled").Namespace("ns1").UID("tp001").Priority(highPriority).NominatedNodeName("node1").Obj()

	// Update pod condition to unschedulable.
	podutil.UpdatePodCondition(&unschedulablePod.Status, &v1.PodCondition{
		Type:    v1.PodScheduled,
		Status:  v1.ConditionFalse,
		Reason:  v1.PodReasonUnschedulable,
		Message: "fake scheduling failure",
	})

	// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent() below.
	q.Add(logger, unschedulablePod)
	if p, err := q.Pop(logger); err != nil || p.Pod != unschedulablePod {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod.Name, p.Pod.Name)
	}
	// Put in the unschedulable queue
	err := q.AddUnschedulableIfNotPresent(logger, newQueuedPodInfoForLookup(unschedulablePod, "plugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	// Move clock to make the unschedulable pods complete backoff.
	c.Step(DefaultPodInitialBackoffDuration + time.Second)
	// Move all unschedulable pods to the active queue.
	q.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)

	// Simulate a pod being popped by the scheduler,
	// At this time, unschedulable pod should be popped.
	p1, err := q.Pop(logger)
	if err != nil {
		t.Errorf("Error while popping the head of the queue: %v", err)
	}
	if p1.Pod != unschedulablePod {
		t.Errorf("Expected that test-pod-unscheduled was popped, got %v", p1.Pod.Name)
	}

	// Assume newer pod was added just after unschedulable pod
	// being popped and before being pushed back to the queue.
	newerPod := st.MakePod().Name("test-newer-pod").Namespace("ns1").UID("tp002").CreationTimestamp(metav1.Now()).Priority(highPriority).NominatedNodeName("node1").Obj()
	q.Add(logger, newerPod)

	// And then unschedulablePodInfo was determined as unschedulable AGAIN.
	podutil.UpdatePodCondition(&unschedulablePod.Status, &v1.PodCondition{
		Type:    v1.PodScheduled,
		Status:  v1.ConditionFalse,
		Reason:  v1.PodReasonUnschedulable,
		Message: "fake scheduling failure",
	})

	// And then, put unschedulable pod to the unschedulable queue
	err = q.AddUnschedulableIfNotPresent(logger, newQueuedPodInfoForLookup(unschedulablePod, "plugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	// Move clock to make the unschedulable pods complete backoff.
	c.Step(DefaultPodInitialBackoffDuration + time.Second)
	// Move all unschedulable pods to the active queue.
	q.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)

	// At this time, newerPod should be popped
	// because it is the oldest tried pod.
	p2, err2 := q.Pop(logger)
	if err2 != nil {
		t.Errorf("Error while popping the head of the queue: %v", err2)
	}
	if p2.Pod != newerPod {
		t.Errorf("Expected that test-newer-pod was popped, got %v", p2.Pod.Name)
	}
}

// TestHighPriorityBackoff tests that a high priority pod does not block
// other pods if it is unschedulable
func TestHighPriorityBackoff(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort())

	midPod := st.MakePod().Name("test-midpod").Namespace("ns1").UID("tp-mid").Priority(midPriority).NominatedNodeName("node1").Obj()
	highPod := st.MakePod().Name("test-highpod").Namespace("ns1").UID("tp-high").Priority(highPriority).NominatedNodeName("node1").Obj()
	q.Add(logger, midPod)
	q.Add(logger, highPod)
	// Simulate a pod being popped by the scheduler, determined unschedulable, and
	// then moved back to the active queue.
	p, err := q.Pop(logger)
	if err != nil {
		t.Errorf("Error while popping the head of the queue: %v", err)
	}
	if p.Pod != highPod {
		t.Errorf("Expected to get high priority pod, got: %v", p)
	}
	// Update pod condition to unschedulable.
	podutil.UpdatePodCondition(&p.Pod.Status, &v1.PodCondition{
		Type:    v1.PodScheduled,
		Status:  v1.ConditionFalse,
		Reason:  v1.PodReasonUnschedulable,
		Message: "fake scheduling failure",
	})
	// Put in the unschedulable queue.
	err = q.AddUnschedulableIfNotPresent(logger, newQueuedPodInfoForLookup(p.Pod, "fooPlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	// Move all unschedulable pods to the active/backoff queue.
	q.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)

	p, err = q.Pop(logger)
	if err != nil {
		t.Errorf("Error while popping the head of the queue: %v", err)
	}
	if p.Pod != midPod {
		// high pod should be in backoffQ
		t.Errorf("Expected to get mid priority pod, got: %v", p.Pod.Name)
	}
}

// TestHighPriorityFlushUnschedulablePodsLeftover tests that pods will be moved to
// activeQ after one minutes if it is in unschedulablePods.
func TestHighPriorityFlushUnschedulablePodsLeftover(t *testing.T) {
	c := testingclock.NewFakeClock(time.Now())
	m := makeEmptyQueueingHintMapPerProfile()
	m[""][nodeAdd] = []*QueueingHintFunction{
		{
			PluginName:     "fakePlugin",
			QueueingHintFn: queueHintReturnQueue,
		},
	}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c), WithQueueingHintMapPerProfile(m))
	midPod := st.MakePod().Name("test-midpod").Namespace("ns1").UID("tp-mid").Priority(midPriority).NominatedNodeName("node1").Obj()
	highPod := st.MakePod().Name("test-highpod").Namespace("ns1").UID("tp-high").Priority(highPriority).NominatedNodeName("node1").Obj()

	// Update pod condition to highPod.
	podutil.UpdatePodCondition(&highPod.Status, &v1.PodCondition{
		Type:    v1.PodScheduled,
		Status:  v1.ConditionFalse,
		Reason:  v1.PodReasonUnschedulable,
		Message: "fake scheduling failure",
	})

	// Update pod condition to midPod.
	podutil.UpdatePodCondition(&midPod.Status, &v1.PodCondition{
		Type:    v1.PodScheduled,
		Status:  v1.ConditionFalse,
		Reason:  v1.PodReasonUnschedulable,
		Message: "fake scheduling failure",
	})

	// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent()s below.
	q.Add(logger, highPod)
	if p, err := q.Pop(logger); err != nil || p.Pod != highPod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPod.Name, p.Pod.Name)
	}
	q.Add(logger, midPod)
	if p, err := q.Pop(logger); err != nil || p.Pod != midPod {
		t.Errorf("Expected: %v after Pop, but got: %v", midPod.Name, p.Pod.Name)
	}
	err := q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(highPod, "fakePlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	err = q.AddUnschedulableIfNotPresent(logger, q.newQueuedPodInfo(midPod, "fakePlugin"), q.SchedulingCycle())
	if err != nil {
		t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
	}
	c.Step(DefaultPodMaxInUnschedulablePodsDuration + time.Second)
	q.flushUnschedulablePodsLeftover(logger)

	if p, err := q.Pop(logger); err != nil || p.Pod != highPod {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
	if p, err := q.Pop(logger); err != nil || p.Pod != midPod {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPodInfo.Pod.Name, p.Pod.Name)
	}
}

func TestPriorityQueue_initPodMaxInUnschedulablePodsDuration(t *testing.T) {
	pod1 := st.MakePod().Name("test-pod-1").Namespace("ns1").UID("tp-1").NominatedNodeName("node1").Obj()
	pod2 := st.MakePod().Name("test-pod-2").Namespace("ns2").UID("tp-2").NominatedNodeName("node2").Obj()

	var timestamp = time.Now()
	pInfo1 := &framework.QueuedPodInfo{
		PodInfo:   mustNewTestPodInfo(t, pod1),
		Timestamp: timestamp.Add(-time.Second),
	}
	pInfo2 := &framework.QueuedPodInfo{
		PodInfo:   mustNewTestPodInfo(t, pod2),
		Timestamp: timestamp.Add(-2 * time.Second),
	}

	tests := []struct {
		name                              string
		podMaxInUnschedulablePodsDuration time.Duration
		operations                        []operation
		operands                          []*framework.QueuedPodInfo
		expected                          []*framework.QueuedPodInfo
	}{
		{
			name: "New priority queue by the default value of podMaxInUnschedulablePodsDuration",
			operations: []operation{
				addPodUnschedulablePods,
				addPodUnschedulablePods,
				flushUnscheduledQ,
			},
			operands: []*framework.QueuedPodInfo{pInfo1, pInfo2, nil},
			expected: []*framework.QueuedPodInfo{pInfo2, pInfo1},
		},
		{
			name:                              "New priority queue by user-defined value of podMaxInUnschedulablePodsDuration",
			podMaxInUnschedulablePodsDuration: 30 * time.Second,
			operations: []operation{
				addPodUnschedulablePods,
				addPodUnschedulablePods,
				flushUnscheduledQ,
			},
			operands: []*framework.QueuedPodInfo{pInfo1, pInfo2, nil},
			expected: []*framework.QueuedPodInfo{pInfo2, pInfo1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var queue *PriorityQueue
			if test.podMaxInUnschedulablePodsDuration > 0 {
				queue = NewTestQueue(ctx, newDefaultQueueSort(),
					WithClock(testingclock.NewFakeClock(timestamp)),
					WithPodMaxInUnschedulablePodsDuration(test.podMaxInUnschedulablePodsDuration))
			} else {
				queue = NewTestQueue(ctx, newDefaultQueueSort(),
					WithClock(testingclock.NewFakeClock(timestamp)))
			}

			var podInfoList []*framework.QueuedPodInfo

			for i, op := range test.operations {
				op(t, logger, queue, test.operands[i])
			}

			expectedLen := len(test.expected)
			if queue.activeQ.len() != expectedLen {
				t.Fatalf("Expected %v items to be in activeQ, but got: %v", expectedLen, queue.activeQ.len())
			}

			for i := 0; i < expectedLen; i++ {
				if pInfo, err := queue.activeQ.pop(logger); err != nil {
					t.Errorf("Error while popping the head of the queue: %v", err)
				} else {
					podInfoList = append(podInfoList, pInfo)
					// Cleanup attempts counter incremented in activeQ.pop()
					pInfo.Attempts = 0
				}
			}

			if diff := cmp.Diff(test.expected, podInfoList, cmpopts.IgnoreUnexported(framework.PodInfo{})); diff != "" {
				t.Errorf("Unexpected QueuedPodInfo list (-want, +got):\n%s", diff)
			}
		})
	}
}

type operation func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo)

var (
	add = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		queue.Add(logger, pInfo.Pod)
	}
	popAndRequeueAsUnschedulable = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent() below.
		// UnschedulablePlugins will get cleared by Pop, so make a copy first.
		unschedulablePlugins := pInfo.UnschedulablePlugins.Clone()
		queue.Add(logger, pInfo.Pod)
		p, err := queue.Pop(logger)
		if err != nil {
			t.Fatalf("Unexpected error during Pop: %v", err)
		}
		if p.Pod != pInfo.Pod {
			t.Fatalf("Expected: %v after Pop, but got: %v", pInfo.Pod.Name, p.Pod.Name)
		}
		// Simulate plugins that are waiting for some events.
		p.UnschedulablePlugins = unschedulablePlugins
		if err := queue.AddUnschedulableIfNotPresent(logger, p, 1); err != nil {
			t.Fatalf("Unexpected error during AddUnschedulableIfNotPresent: %v", err)
		}
	}
	popAndRequeueAsBackoff = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent() below.
		queue.Add(logger, pInfo.Pod)
		p, err := queue.Pop(logger)
		if err != nil {
			t.Fatalf("Unexpected error during Pop: %v", err)
		}
		if p.Pod != pInfo.Pod {
			t.Fatalf("Expected: %v after Pop, but got: %v", pInfo.Pod.Name, p.Pod.Name)
		}
		// needs to increment it to make it backoff
		p.UnschedulableCount++
		// When there is no known unschedulable plugin, pods always go to the backoff queue.
		if err := queue.AddUnschedulableIfNotPresent(logger, p, 1); err != nil {
			t.Fatalf("Unexpected error during AddUnschedulableIfNotPresent: %v", err)
		}
	}
	addPodActiveQ = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		queue.Add(logger, pInfo.Pod)
	}
	addPodActiveQDirectly = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		queue.activeQ.underLock(func(unlockedActiveQ unlockedActiveQueuer) {
			unlockedActiveQ.AddOrUpdate(pInfo)
		})
	}
	addPodUnschedulablePods = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		if !pInfo.Gated {
			// Update pod condition to unschedulable.
			podutil.UpdatePodCondition(&pInfo.Pod.Status, &v1.PodCondition{
				Type:    v1.PodScheduled,
				Status:  v1.ConditionFalse,
				Reason:  v1.PodReasonUnschedulable,
				Message: "fake scheduling failure",
			})
			// needs to increment it to make it backoff
			pInfo.UnschedulableCount++
		}
		queue.unschedulablePods.addOrUpdate(pInfo)
	}
	deletePod = func(t *testing.T, _ klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		queue.Delete(pInfo.Pod)
	}
	updatePodQueueable = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		newPod := pInfo.Pod.DeepCopy()
		newPod.Labels = map[string]string{"queueable": ""}
		queue.Update(logger, pInfo.Pod, newPod)
	}
	addPodBackoffQ = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, pInfo *framework.QueuedPodInfo) {
		queue.podBackoffQ.AddOrUpdate(pInfo)
	}
	moveAllToActiveOrBackoffQ = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, _ *framework.QueuedPodInfo) {
		queue.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)
	}
	flushBackoffQ = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, _ *framework.QueuedPodInfo) {
		queue.clock.(*testingclock.FakeClock).Step(2 * time.Second)
		queue.flushBackoffQCompleted(logger)
	}
	moveClockForward = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, _ *framework.QueuedPodInfo) {
		queue.clock.(*testingclock.FakeClock).Step(2 * time.Second)
	}
	flushUnscheduledQ = func(t *testing.T, logger klog.Logger, queue *PriorityQueue, _ *framework.QueuedPodInfo) {
		queue.clock.(*testingclock.FakeClock).Step(queue.podMaxInUnschedulablePodsDuration)
		queue.flushUnschedulablePodsLeftover(logger)
	}
)

// TestPodTimestamp tests the operations related to QueuedPodInfo.
func TestPodTimestamp(t *testing.T) {
	pod1 := st.MakePod().Name("test-pod-1").Namespace("ns1").UID("tp-1").NominatedNodeName("node1").Obj()
	pod2 := st.MakePod().Name("test-pod-2").Namespace("ns2").UID("tp-2").NominatedNodeName("node2").Obj()

	var timestamp = time.Now()
	pInfo1 := &framework.QueuedPodInfo{
		PodInfo:   mustNewTestPodInfo(t, pod1),
		Timestamp: timestamp,
	}
	pInfo2 := &framework.QueuedPodInfo{
		PodInfo:   mustNewTestPodInfo(t, pod2),
		Timestamp: timestamp.Add(time.Second),
	}

	tests := []struct {
		name       string
		operations []operation
		operands   []*framework.QueuedPodInfo
		expected   []*framework.QueuedPodInfo
	}{
		{
			name: "add two pod to activeQ and sort them by the timestamp",
			operations: []operation{
				// Need to add the pods directly to the activeQ to override the timestamps.
				addPodActiveQDirectly,
				addPodActiveQDirectly,
			},
			operands: []*framework.QueuedPodInfo{pInfo2, pInfo1},
			expected: []*framework.QueuedPodInfo{pInfo1, pInfo2},
		},
		{
			name: "add two pod to unschedulablePods then move them to activeQ and sort them by the timestamp",
			operations: []operation{
				addPodUnschedulablePods,
				addPodUnschedulablePods,
				moveClockForward,
				moveAllToActiveOrBackoffQ,
			},
			operands: []*framework.QueuedPodInfo{pInfo2, pInfo1, nil, nil},
			expected: []*framework.QueuedPodInfo{pInfo1, pInfo2},
		},
		{
			name: "add one pod to BackoffQ and move it to activeQ",
			operations: []operation{
				// Need to add the pods directly to activeQ to override the timestamps.
				addPodActiveQDirectly,
				addPodBackoffQ,
				flushBackoffQ,
				moveAllToActiveOrBackoffQ,
			},
			operands: []*framework.QueuedPodInfo{pInfo2, pInfo1, nil, nil},
			expected: []*framework.QueuedPodInfo{pInfo1, pInfo2},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			queue := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(testingclock.NewFakeClock(timestamp)))
			var podInfoList []*framework.QueuedPodInfo

			for i, op := range test.operations {
				op(t, logger, queue, test.operands[i])
			}

			expectedLen := len(test.expected)
			if queue.activeQ.len() != expectedLen {
				t.Fatalf("Expected %v items to be in activeQ, but got: %v", expectedLen, queue.activeQ.len())
			}

			for i := 0; i < expectedLen; i++ {
				if pInfo, err := queue.activeQ.pop(logger); err != nil {
					t.Errorf("Error while popping the head of the queue: %v", err)
				} else {
					podInfoList = append(podInfoList, pInfo)
					// Cleanup attempts counter incremented in activeQ.pop()
					pInfo.Attempts = 0
				}
			}

			if diff := cmp.Diff(test.expected, podInfoList, cmpopts.IgnoreUnexported(framework.PodInfo{})); diff != "" {
				t.Errorf("Unexpected QueuedPodInfo list (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestPendingPodsMetric tests Prometheus metrics related with pending pods
func TestPendingPodsMetric(t *testing.T) {
	timestamp := time.Now()
	total := 60
	queueableNum := 50
	queueable, failme := "queueable", "failme"
	// First 50 Pods are queueable.
	pInfos := makeQueuedPodInfos(queueableNum, "x", queueable, timestamp)
	// The last 10 Pods are not queueable.
	gated := makeQueuedPodInfos(total-queueableNum, "y", failme, timestamp)
	// Manually mark them as gated=true.
	for _, pInfo := range gated {
		setQueuedPodInfoGated(pInfo)
	}
	pInfos = append(pInfos, gated...)
	totalWithDelay := 20
	pInfosWithDelay := makeQueuedPodInfos(totalWithDelay, "z", queueable, timestamp.Add(2*time.Second))

	resetPodInfos := func() {
		// reset PodInfo's Attempts because they influence the backoff time calculation.
		for i := range pInfos {
			pInfos[i].Attempts = 0
			pInfos[i].UnschedulableCount = 0
		}
		for i := range pInfosWithDelay {
			pInfosWithDelay[i].Attempts = 0
			pInfosWithDelay[i].UnschedulableCount = 0
		}
	}

	tests := []struct {
		name                       string
		operations                 []operation
		operands                   [][]*framework.QueuedPodInfo
		metricsName                string
		pluginMetricsSamplePercent int
		wants                      string
	}{
		{
			name: "add pods to activeQ and unschedulablePods",
			operations: []operation{
				addPodActiveQ,
				addPodUnschedulablePods,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:30],
				pInfos[30:],
			},
			metricsName: "scheduler_pending_pods",
			wants: `
# HELP scheduler_pending_pods [STABLE] Number of pending pods, by the queue type. 'active' means number of pods in activeQ; 'backoff' means number of pods in backoffQ; 'unschedulable' means number of pods in unschedulablePods that the scheduler attempted to schedule and failed; 'gated' is the number of unschedulable pods that the scheduler never attempted to schedule because they are gated.
# TYPE scheduler_pending_pods gauge
scheduler_pending_pods{queue="active"} 30
scheduler_pending_pods{queue="backoff"} 0
scheduler_pending_pods{queue="gated"} 10
scheduler_pending_pods{queue="unschedulable"} 20
`,
		},
		{
			name: "add pods to all kinds of queues",
			operations: []operation{
				addPodActiveQ,
				addPodBackoffQ,
				addPodUnschedulablePods,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:15],
				pInfos[15:40],
				pInfos[40:],
			},
			metricsName: "scheduler_pending_pods",
			wants: `
# HELP scheduler_pending_pods [STABLE] Number of pending pods, by the queue type. 'active' means number of pods in activeQ; 'backoff' means number of pods in backoffQ; 'unschedulable' means number of pods in unschedulablePods that the scheduler attempted to schedule and failed; 'gated' is the number of unschedulable pods that the scheduler never attempted to schedule because they are gated.
# TYPE scheduler_pending_pods gauge
scheduler_pending_pods{queue="active"} 15
scheduler_pending_pods{queue="backoff"} 25
scheduler_pending_pods{queue="gated"} 10
scheduler_pending_pods{queue="unschedulable"} 10
`,
		},
		{
			name: "add pods to unschedulablePods and then move all to activeQ",
			operations: []operation{
				addPodUnschedulablePods,
				moveClockForward,
				moveAllToActiveOrBackoffQ,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:total],
				{nil},
				{nil},
			},
			metricsName: "scheduler_pending_pods",
			wants: `
# HELP scheduler_pending_pods [STABLE] Number of pending pods, by the queue type. 'active' means number of pods in activeQ; 'backoff' means number of pods in backoffQ; 'unschedulable' means number of pods in unschedulablePods that the scheduler attempted to schedule and failed; 'gated' is the number of unschedulable pods that the scheduler never attempted to schedule because they are gated.
# TYPE scheduler_pending_pods gauge
scheduler_pending_pods{queue="active"} 50
scheduler_pending_pods{queue="backoff"} 0
scheduler_pending_pods{queue="gated"} 10
scheduler_pending_pods{queue="unschedulable"} 0
`,
		},
		{
			name: "make some pods subject to backoff, add pods to unschedulablePods, and then move all to activeQ",
			operations: []operation{
				addPodUnschedulablePods,
				moveClockForward,
				addPodUnschedulablePods,
				moveAllToActiveOrBackoffQ,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[20:total],
				{nil},
				pInfosWithDelay[:20],
				{nil},
			},
			metricsName: "scheduler_pending_pods",
			wants: `
# HELP scheduler_pending_pods [STABLE] Number of pending pods, by the queue type. 'active' means number of pods in activeQ; 'backoff' means number of pods in backoffQ; 'unschedulable' means number of pods in unschedulablePods that the scheduler attempted to schedule and failed; 'gated' is the number of unschedulable pods that the scheduler never attempted to schedule because they are gated.
# TYPE scheduler_pending_pods gauge
scheduler_pending_pods{queue="active"} 30
scheduler_pending_pods{queue="backoff"} 20
scheduler_pending_pods{queue="gated"} 10
scheduler_pending_pods{queue="unschedulable"} 0
`,
		},
		{
			name: "make some pods subject to backoff, add pods to unschedulablePods/activeQ, move all to activeQ, and finally flush backoffQ",
			operations: []operation{
				addPodUnschedulablePods,
				addPodActiveQ,
				moveAllToActiveOrBackoffQ,
				flushBackoffQ,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:40],
				pInfos[40:50],
				{nil},
				{nil},
			},
			metricsName: "scheduler_pending_pods",
			wants: `
# HELP scheduler_pending_pods [STABLE] Number of pending pods, by the queue type. 'active' means number of pods in activeQ; 'backoff' means number of pods in backoffQ; 'unschedulable' means number of pods in unschedulablePods that the scheduler attempted to schedule and failed; 'gated' is the number of unschedulable pods that the scheduler never attempted to schedule because they are gated.
# TYPE scheduler_pending_pods gauge
scheduler_pending_pods{queue="active"} 50
scheduler_pending_pods{queue="backoff"} 0
scheduler_pending_pods{queue="gated"} 0
scheduler_pending_pods{queue="unschedulable"} 0
`,
		},
		{
			name: "add pods to activeQ/unschedulablePods and then delete some Pods",
			operations: []operation{
				addPodActiveQ,
				addPodUnschedulablePods,
				deletePod,
				deletePod,
				deletePod,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:30],
				pInfos[30:],
				pInfos[:2],
				pInfos[30:33],
				pInfos[50:54],
			},
			metricsName: "scheduler_pending_pods",
			wants: `
# HELP scheduler_pending_pods [STABLE] Number of pending pods, by the queue type. 'active' means number of pods in activeQ; 'backoff' means number of pods in backoffQ; 'unschedulable' means number of pods in unschedulablePods that the scheduler attempted to schedule and failed; 'gated' is the number of unschedulable pods that the scheduler never attempted to schedule because they are gated.
# TYPE scheduler_pending_pods gauge
scheduler_pending_pods{queue="active"} 28
scheduler_pending_pods{queue="backoff"} 0
scheduler_pending_pods{queue="gated"} 6
scheduler_pending_pods{queue="unschedulable"} 17
`,
		},
		{
			name: "add pods to activeQ/unschedulablePods and then update some Pods as queueable",
			operations: []operation{
				addPodActiveQ,
				addPodUnschedulablePods,
				updatePodQueueable,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:30],
				pInfos[30:],
				pInfos[50:55],
			},
			metricsName: "scheduler_pending_pods",
			wants: `
# HELP scheduler_pending_pods [STABLE] Number of pending pods, by the queue type. 'active' means number of pods in activeQ; 'backoff' means number of pods in backoffQ; 'unschedulable' means number of pods in unschedulablePods that the scheduler attempted to schedule and failed; 'gated' is the number of unschedulable pods that the scheduler never attempted to schedule because they are gated.
# TYPE scheduler_pending_pods gauge
scheduler_pending_pods{queue="active"} 35
scheduler_pending_pods{queue="backoff"} 0
scheduler_pending_pods{queue="gated"} 5
scheduler_pending_pods{queue="unschedulable"} 20
`,
		},
		{
			name: "the metrics should not be recorded (pluginMetricsSamplePercent=0)",
			operations: []operation{
				add,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:1],
			},
			metricsName:                "scheduler_plugin_execution_duration_seconds",
			pluginMetricsSamplePercent: 0,
			wants: `
# HELP scheduler_plugin_execution_duration_seconds [ALPHA] Duration for running a plugin at a specific extension point.
# TYPE scheduler_plugin_execution_duration_seconds histogram
`, // the observed value will always be 0, because we don't proceed the fake clock.
		},
		{
			name: "the metrics should be recorded (pluginMetricsSamplePercent=100)",
			operations: []operation{
				add,
			},
			operands: [][]*framework.QueuedPodInfo{
				pInfos[:1],
			},
			metricsName:                "scheduler_plugin_execution_duration_seconds",
			pluginMetricsSamplePercent: 100,
			wants: `
# HELP scheduler_plugin_execution_duration_seconds [ALPHA] Duration for running a plugin at a specific extension point.
# TYPE scheduler_plugin_execution_duration_seconds histogram
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="1e-05"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="1.5000000000000002e-05"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="2.2500000000000005e-05"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="3.375000000000001e-05"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="5.062500000000001e-05"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="7.593750000000002e-05"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.00011390625000000003"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.00017085937500000006"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.0002562890625000001"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.00038443359375000017"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.0005766503906250003"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.0008649755859375004"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.0012974633789062506"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.0019461950683593758"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.0029192926025390638"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.004378938903808595"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.006568408355712893"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.009852612533569338"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.014778918800354007"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="0.02216837820053101"} 1
scheduler_plugin_execution_duration_seconds_bucket{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success",le="+Inf"} 1
scheduler_plugin_execution_duration_seconds_sum{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success"} 0
scheduler_plugin_execution_duration_seconds_count{extension_point="PreEnqueue",plugin="preEnqueuePlugin",status="Success"} 1
`, // the observed value will always be 0, because we don't proceed the fake clock.
		},
	}

	resetMetrics := func() {
		metrics.ActivePods().Set(0)
		metrics.BackoffPods().Set(0)
		metrics.UnschedulablePods().Set(0)
		metrics.GatedPods().Set(0)
		metrics.PluginExecutionDuration.Reset()
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			resetMetrics()
			resetPodInfos()

			m := map[string][]framework.PreEnqueuePlugin{"": {&preEnqueuePlugin{allowlists: []string{queueable}}}}
			recorder := metrics.NewMetricsAsyncRecorder(3, 20*time.Microsecond, ctx.Done())
			queue := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(testingclock.NewFakeClock(timestamp)), WithPreEnqueuePluginMap(m), WithPluginMetricsSamplePercent(test.pluginMetricsSamplePercent), WithMetricsRecorder(*recorder))
			for i, op := range test.operations {
				for _, pInfo := range test.operands[i] {
					op(t, logger, queue, pInfo)
				}
			}

			recorder.FlushMetrics()

			if err := testutil.GatherAndCompare(metrics.GetGather(), strings.NewReader(test.wants), test.metricsName); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestPerPodSchedulingMetrics makes sure pod schedule attempts is updated correctly while
// initialAttemptTimestamp stays the same during multiple add/pop operations.
func TestPerPodSchedulingMetrics(t *testing.T) {
	timestamp := time.Now()

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tests := []struct {
		name                            string
		perPodSchedulingMetricsScenario func(*testingclock.FakeClock, *PriorityQueue, *v1.Pod)
		wantAttempts                    int
		wantInitialAttemptTs            time.Time
	}{
		{
			// The queue operations are Add -> Pop.
			name: "pod is created and scheduled after 1 attempt",
			perPodSchedulingMetricsScenario: func(c *testingclock.FakeClock, queue *PriorityQueue, pod *v1.Pod) {
				queue.Add(logger, pod)
			},
			wantAttempts:         1,
			wantInitialAttemptTs: timestamp,
		},
		{
			// The queue operations are Add -> Pop -> AddUnschedulableIfNotPresent -> flushUnschedulablePodsLeftover -> Pop.
			name: "pod is created and scheduled after 2 attempts",
			perPodSchedulingMetricsScenario: func(c *testingclock.FakeClock, queue *PriorityQueue, pod *v1.Pod) {
				queue.Add(logger, pod)
				pInfo, err := queue.Pop(logger)
				if err != nil {
					t.Fatalf("Failed to pop a pod %v", err)
				}

				pInfo.UnschedulablePlugins = sets.New("plugin")
				queue.AddUnschedulableIfNotPresent(logger, pInfo, 1)
				// Override clock to exceed the DefaultPodMaxInUnschedulablePodsDuration so that unschedulable pods
				// will be moved to activeQ
				c.SetTime(timestamp.Add(DefaultPodMaxInUnschedulablePodsDuration + 1))
				queue.flushUnschedulablePodsLeftover(logger)
			},
			wantAttempts:         2,
			wantInitialAttemptTs: timestamp,
		},
		{
			// The queue operations are Add -> Pop -> AddUnschedulableIfNotPresent -> flushUnschedulablePodsLeftover -> Update -> Pop.
			name: "pod is created and scheduled after 2 attempts but before the second pop, call update",
			perPodSchedulingMetricsScenario: func(c *testingclock.FakeClock, queue *PriorityQueue, pod *v1.Pod) {
				queue.Add(logger, pod)
				pInfo, err := queue.Pop(logger)
				if err != nil {
					t.Fatalf("Failed to pop a pod %v", err)
				}

				pInfo.UnschedulablePlugins = sets.New("plugin")
				queue.AddUnschedulableIfNotPresent(logger, pInfo, 1)
				// Override clock to exceed the DefaultPodMaxInUnschedulablePodsDuration so that unschedulable pods
				// will be moved to activeQ
				updatedTimestamp := timestamp
				c.SetTime(updatedTimestamp.Add(DefaultPodMaxInUnschedulablePodsDuration + 1))
				queue.flushUnschedulablePodsLeftover(logger)
				newPod := pod.DeepCopy()
				newPod.Generation = 1
				queue.Update(logger, pod, newPod)
			},
			wantAttempts:         2,
			wantInitialAttemptTs: timestamp,
		},
		{
			// The queue operations are Add gated pod -> check unschedulablePods -> lift gate & update pod -> Pop.
			name: "A gated pod is created and scheduled after lifting gate",
			perPodSchedulingMetricsScenario: func(c *testingclock.FakeClock, queue *PriorityQueue, pod *v1.Pod) {
				// Create a queue with PreEnqueuePlugin
				queue.preEnqueuePluginMap = map[string][]framework.PreEnqueuePlugin{"": {&preEnqueuePlugin{allowlists: []string{"foo"}}}}
				queue.pluginMetricsSamplePercent = 0
				queue.Add(logger, pod)
				// Check pod is added to the unschedulablePods queue.
				if getUnschedulablePod(queue, pod) != pod {
					t.Errorf("Pod %v was not found in the unschedulablePods.", pod.Name)
				}
				// Override clock to get different InitialAttemptTimestamp
				c.Step(1 * time.Minute)

				// Update pod with the required label to get it out of unschedulablePods queue.
				updateGatedPod := pod.DeepCopy()
				updateGatedPod.Labels = map[string]string{"foo": ""}
				queue.Update(logger, pod, updateGatedPod)
			},
			wantAttempts:         1,
			wantInitialAttemptTs: timestamp.Add(1 * time.Minute),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			c := testingclock.NewFakeClock(timestamp)
			pod := st.MakePod().Name("test-pod").Namespace("test-ns").UID("test-uid").Obj()
			queue := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(c))

			test.perPodSchedulingMetricsScenario(c, queue, pod)
			podInfo, err := queue.Pop(logger)
			if err != nil {
				t.Fatal(err)
			}
			if podInfo.Attempts != test.wantAttempts {
				t.Errorf("Pod schedule attempt unexpected, got %v, want %v", podInfo.Attempts, test.wantAttempts)
			}
			if *podInfo.InitialAttemptTimestamp != test.wantInitialAttemptTs {
				t.Errorf("Pod initial schedule attempt timestamp unexpected, got %v, want %v", *podInfo.InitialAttemptTimestamp, test.wantInitialAttemptTs)
			}
		})
	}
}

func TestIncomingPodsMetrics(t *testing.T) {
	timestamp := time.Now()
	unschedulablePlg := "unschedulable_plugin"
	var pInfos = make([]*framework.QueuedPodInfo, 0, 3)

	for i := 1; i <= 3; i++ {
		p := &framework.QueuedPodInfo{
			PodInfo: mustNewTestPodInfo(t,
				st.MakePod().Name(fmt.Sprintf("test-pod-%d", i)).Namespace(fmt.Sprintf("ns%d", i)).UID(fmt.Sprintf("tp-%d", i)).Obj()),
			Timestamp:            timestamp,
			UnschedulablePlugins: sets.New(unschedulablePlg),
		}
		pInfos = append(pInfos, p)
	}
	tests := []struct {
		name       string
		operations []operation
		want       string
	}{
		{
			name: "add pods to activeQ",
			operations: []operation{
				add,
			},
			want: `
            scheduler_queue_incoming_pods_total{event="UnschedulablePodAdd",queue="active"} 3
`,
		},
		{
			name: "add pods to unschedulablePods",
			operations: []operation{
				popAndRequeueAsUnschedulable,
			},
			want: `scheduler_queue_incoming_pods_total{event="UnschedulablePodAdd",queue="active"} 3
             scheduler_queue_incoming_pods_total{event="ScheduleAttemptFailure",queue="unschedulable"} 3
`,
		},
		{
			name: "add pods to unschedulablePods and then move all to backoffQ",
			operations: []operation{
				popAndRequeueAsUnschedulable,
				moveAllToActiveOrBackoffQ,
			},
			want: `scheduler_queue_incoming_pods_total{event="UnschedulablePodAdd",queue="active"} 3
			scheduler_queue_incoming_pods_total{event="ScheduleAttemptFailure",queue="unschedulable"} 3
            scheduler_queue_incoming_pods_total{event="UnschedulableTimeout",queue="backoff"} 3
`,
		},
		{
			name: "add pods to unschedulablePods and then move all to activeQ",
			operations: []operation{
				popAndRequeueAsUnschedulable,
				moveClockForward,
				moveAllToActiveOrBackoffQ,
			},
			want: `scheduler_queue_incoming_pods_total{event="UnschedulablePodAdd",queue="active"} 3
			scheduler_queue_incoming_pods_total{event="ScheduleAttemptFailure",queue="unschedulable"} 3
            scheduler_queue_incoming_pods_total{event="UnschedulableTimeout",queue="active"} 3
`,
		},
		{
			name: "make some pods subject to backoff and add them to backoffQ, then flush backoffQ",
			operations: []operation{
				popAndRequeueAsBackoff,
				moveClockForward,
				flushBackoffQ,
			},
			want: `scheduler_queue_incoming_pods_total{event="UnschedulablePodAdd",queue="active"} 3
			scheduler_queue_incoming_pods_total{event="BackoffComplete",queue="active"} 3
            scheduler_queue_incoming_pods_total{event="ScheduleAttemptFailure",queue="backoff"} 3
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			metrics.SchedulerQueueIncomingPods.Reset()
			queue := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(testingclock.NewFakeClock(timestamp)))
			for _, op := range test.operations {
				for _, pInfo := range pInfos {
					op(t, logger, queue, pInfo)
				}
			}
			metricName := metrics.SchedulerSubsystem + "_" + metrics.SchedulerQueueIncomingPods.Name
			if err := testutil.CollectAndCompare(metrics.SchedulerQueueIncomingPods, strings.NewReader(queueMetricMetadata+test.want), metricName); err != nil {
				t.Errorf("unexpected collecting result:\n%s", err)
			}

		})
	}
}

func TestBackOffFlow(t *testing.T) {
	cl := testingclock.NewFakeClock(time.Now())
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithClock(cl))
	steps := []struct {
		wantBackoff time.Duration
	}{
		{wantBackoff: time.Second},
		{wantBackoff: 2 * time.Second},
		{wantBackoff: 4 * time.Second},
		{wantBackoff: 8 * time.Second},
		{wantBackoff: 10 * time.Second},
		{wantBackoff: 10 * time.Second},
		{wantBackoff: 10 * time.Second},
	}
	pod := st.MakePod().Name("test-pod").Namespace("test-ns").UID("test-uid").Obj()

	podID := types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}
	q.Add(logger, pod)

	for i, step := range steps {
		t.Run(fmt.Sprintf("step %d", i), func(t *testing.T) {
			timestamp := cl.Now()
			// Simulate schedule attempt.
			podInfo, err := q.Pop(logger)
			if err != nil {
				t.Fatal(err)
			}
			if podInfo.Attempts != i+1 {
				t.Errorf("got attempts %d, want %d", podInfo.Attempts, i+1)
			}
			podInfo.UnschedulablePlugins.Insert("unsched-plugin")
			err = q.AddUnschedulableIfNotPresent(logger, podInfo, int64(i))
			if err != nil {
				t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
			}

			// An event happens.
			q.MoveAllToActiveOrBackoffQueue(logger, framework.EventUnschedulableTimeout, nil, nil, nil)

			if !q.podBackoffQ.Has(podInfo) {
				t.Errorf("pod %v is not in the backoff queue", podID)
			}

			// Check backoff duration.
			deadline := q.getBackoffTime(podInfo)
			backoff := deadline.Sub(timestamp)
			if backoff != step.wantBackoff {
				t.Errorf("got backoff %s, want %s", backoff, step.wantBackoff)
			}

			// Simulate routine that continuously flushes the backoff queue.
			cl.Step(time.Millisecond)
			q.flushBackoffQCompleted(logger)
			// Still in backoff queue after an early flush.
			if !q.podBackoffQ.Has(podInfo) {
				t.Errorf("pod %v is not in the backoff queue", podID)
			}
			// Moved out of the backoff queue after timeout.
			cl.Step(backoff)
			q.flushBackoffQCompleted(logger)
			if q.podBackoffQ.Has(podInfo) {
				t.Errorf("pod %v is still in the backoff queue", podID)
			}
		})
	}
}

func TestMoveAllToActiveOrBackoffQueue_PreEnqueueChecks(t *testing.T) {
	var podInfos []*framework.QueuedPodInfo
	for i := 0; i < 5; i++ {
		pInfo := newQueuedPodInfoForLookup(
			st.MakePod().Name(fmt.Sprintf("p%d", i)).Priority(int32(i)).Obj(),
		)
		podInfos = append(podInfos, pInfo)
	}

	tests := []struct {
		name            string
		preEnqueueCheck PreEnqueueCheck
		podInfos        []*framework.QueuedPodInfo
		event           framework.ClusterEvent
		want            []string
	}{
		{
			name:     "nil PreEnqueueCheck",
			podInfos: podInfos,
			event:    framework.EventUnschedulableTimeout,
			want:     []string{"p0", "p1", "p2", "p3", "p4"},
		},
		{
			name:            "move Pods with priority greater than 2",
			podInfos:        podInfos,
			event:           framework.EventUnschedulableTimeout,
			preEnqueueCheck: func(pod *v1.Pod) bool { return *pod.Spec.Priority >= 2 },
			want:            []string{"p2", "p3", "p4"},
		},
		{
			name:     "move Pods with even priority and greater than 2",
			podInfos: podInfos,
			event:    framework.EventUnschedulableTimeout,
			preEnqueueCheck: func(pod *v1.Pod) bool {
				return *pod.Spec.Priority%2 == 0 && *pod.Spec.Priority >= 2
			},
			want: []string{"p2", "p4"},
		},
		{
			name:     "move Pods with even and negative priority",
			podInfos: podInfos,
			event:    framework.EventUnschedulableTimeout,
			preEnqueueCheck: func(pod *v1.Pod) bool {
				return *pod.Spec.Priority%2 == 0 && *pod.Spec.Priority < 0
			},
		},
		{
			name:     "preCheck isn't called if the event is not interested by any plugins",
			podInfos: podInfos,
			event:    pvAdd, // No plugin is interested in this event.
			preEnqueueCheck: func(pod *v1.Pod) bool {
				panic("preCheck shouldn't be called")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			q := NewTestQueue(ctx, newDefaultQueueSort())
			for i, podInfo := range tt.podInfos {
				// To simulate the pod is failed in scheduling in the real world, Pop() the pod from activeQ before AddUnschedulableIfNotPresent() below.
				q.Add(logger, podInfo.Pod)
				if p, err := q.Pop(logger); err != nil || p.Pod != podInfo.Pod {
					t.Errorf("Expected: %v after Pop, but got: %v", podInfo.Pod.Name, p.Pod.Name)
				}
				podInfo.UnschedulablePlugins = sets.New("plugin")
				err := q.AddUnschedulableIfNotPresent(logger, podInfo, q.activeQ.schedulingCycle())
				if err != nil {
					t.Fatalf("unexpected error from AddUnschedulableIfNotPresent: %v", err)
				}
				// NOTE: On Windows, time.Now() is not as precise, 2 consecutive calls may return the same timestamp,
				// resulting in 0 time delta / latency. This will cause the pods to be backed off in a random
				// order, which would cause this test to fail, since the expectation is for them to be backed off
				// in a certain order.
				// See: https://github.com/golang/go/issues/8687
				podInfo.Timestamp = podInfo.Timestamp.Add(time.Duration((i - len(tt.podInfos))) * time.Millisecond)
			}
			q.MoveAllToActiveOrBackoffQueue(logger, tt.event, nil, nil, tt.preEnqueueCheck)
			var got []string
			for q.podBackoffQ.Len() != 0 {
				queuedPodInfo, err := q.podBackoffQ.Pop()
				if err != nil {
					t.Fatalf("Fail to pop pod from backoffQ: %v", err)
				}
				got = append(got, queuedPodInfo.Pod.Name)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func makeQueuedPodInfos(num int, namePrefix, label string, timestamp time.Time) []*framework.QueuedPodInfo {
	var pInfos = make([]*framework.QueuedPodInfo, 0, num)
	for i := 1; i <= num; i++ {
		p := &framework.QueuedPodInfo{
			PodInfo: mustNewPodInfo(
				st.MakePod().Name(fmt.Sprintf("%v-%d", namePrefix, i)).Namespace(fmt.Sprintf("ns%d", i)).Label(label, "").UID(fmt.Sprintf("tp-%d", i)).Obj()),
			Timestamp:            timestamp,
			UnschedulablePlugins: sets.New[string](),
		}
		pInfos = append(pInfos, p)
	}
	return pInfos
}

func TestPriorityQueue_calculateBackoffDuration(t *testing.T) {
	tests := []struct {
		name                   string
		initialBackoffDuration time.Duration
		maxBackoffDuration     time.Duration
		podInfo                *framework.QueuedPodInfo
		want                   time.Duration
	}{
		{
			name:                   "normal",
			initialBackoffDuration: 1 * time.Nanosecond,
			maxBackoffDuration:     32 * time.Nanosecond,
			podInfo:                &framework.QueuedPodInfo{UnschedulableCount: 16},
			want:                   32 * time.Nanosecond,
		},
		{
			name:                   "overflow_32bit",
			initialBackoffDuration: 1 * time.Nanosecond,
			maxBackoffDuration:     math.MaxInt32 * time.Nanosecond,
			podInfo:                &framework.QueuedPodInfo{UnschedulableCount: 32},
			want:                   math.MaxInt32 * time.Nanosecond,
		},
		{
			name:                   "overflow_64bit",
			initialBackoffDuration: 1 * time.Nanosecond,
			maxBackoffDuration:     math.MaxInt64 * time.Nanosecond,
			podInfo:                &framework.QueuedPodInfo{UnschedulableCount: 64},
			want:                   math.MaxInt64 * time.Nanosecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			q := NewTestQueue(ctx, newDefaultQueueSort(), WithPodInitialBackoffDuration(tt.initialBackoffDuration), WithPodMaxBackoffDuration(tt.maxBackoffDuration))
			if got := q.calculateBackoffDuration(tt.podInfo); got != tt.want {
				t.Errorf("PriorityQueue.calculateBackoffDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

func mustNewTestPodInfo(t *testing.T, pod *v1.Pod) *framework.PodInfo {
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		t.Fatal(err)
	}
	return podInfo
}

func mustNewPodInfo(pod *v1.Pod) *framework.PodInfo {
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		panic(err)
	}
	return podInfo
}

// Test_isPodWorthRequeuing tests isPodWorthRequeuing function.
func Test_isPodWorthRequeuing(t *testing.T) {
	metrics.Register()
	count := 0
	queueHintReturnQueue := func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
		count++
		return framework.Queue, nil
	}
	queueHintReturnSkip := func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
		count++
		return framework.QueueSkip, nil
	}
	queueHintReturnErr := func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
		count++
		return framework.QueueSkip, fmt.Errorf("unexpected error")
	}

	tests := []struct {
		name                   string
		podInfo                *framework.QueuedPodInfo
		event                  framework.ClusterEvent
		oldObj                 interface{}
		newObj                 interface{}
		expected               queueingStrategy
		expectedExecutionCount int // expected total execution count of queueing hint function
		queueingHintMap        QueueingHintMapPerProfile
	}{
		{
			name: "return Queue when no queueing hint function is registered for the event",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  nodeAdd,
			oldObj:                 nil,
			newObj:                 st.MakeNode().Obj(),
			expected:               queueSkip,
			expectedExecutionCount: 0,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					// no queueing hint function for NodeAdd.
					framework.EventAssignedPodAdd: {
						{
							// It will be ignored because the event is not NodeAdd.
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name: "Treat the event as Queue when QueueHintFn returns error",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  nodeAdd,
			oldObj:                 nil,
			newObj:                 st.MakeNode().Obj(),
			expected:               queueAfterBackoff,
			expectedExecutionCount: 1,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					nodeAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnErr,
						},
					},
				},
			},
		},
		{
			name: "return Queue when the event is wildcard",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  framework.EventUnschedulableTimeout,
			oldObj:                 nil,
			newObj:                 nil,
			expected:               queueAfterBackoff,
			expectedExecutionCount: 0,
			queueingHintMap:        QueueingHintMapPerProfile{},
		},
		{
			name: "return Queue when the event is wildcard and the wildcard targets the pod to be requeued right now",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  framework.EventForceActivate,
			oldObj:                 nil,
			newObj:                 st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj(),
			expected:               queueAfterBackoff,
			expectedExecutionCount: 0,
			queueingHintMap:        QueueingHintMapPerProfile{},
		},
		{
			name: "return Skip when the event is wildcard, but the wildcard targets a different pod",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  framework.EventForceActivate,
			oldObj:                 nil,
			newObj:                 st.MakePod().Name("pod-different").Namespace("ns2").UID("2").Obj(),
			expected:               queueSkip,
			expectedExecutionCount: 0,
			queueingHintMap:        QueueingHintMapPerProfile{},
		},
		{
			name: "interprets Queue from the Pending plugin as queueImmediately",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1", "fooPlugin3"),
				PendingPlugins:       sets.New("fooPlugin2"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  nodeAdd,
			oldObj:                 nil,
			newObj:                 st.MakeNode().Node,
			expected:               queueImmediately,
			expectedExecutionCount: 2,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					nodeAdd: {
						{
							PluginName: "fooPlugin1",
							// It returns Queue and it's interpreted as queueAfterBackoff.
							// But, the function continues to run other hints because the Pod has PendingPlugins, which can result in queueImmediately.
							QueueingHintFn: queueHintReturnQueue,
						},
						{
							PluginName: "fooPlugin2",
							// It's interpreted as queueImmediately.
							// The function doesn't run other hints because queueImmediately is the highest priority.
							QueueingHintFn: queueHintReturnQueue,
						},
						{
							PluginName:     "fooPlugin3",
							QueueingHintFn: queueHintReturnQueue,
						},
						{
							PluginName:     "fooPlugin4",
							QueueingHintFn: queueHintReturnErr,
						},
					},
				},
			},
		},
		{
			name: "interprets Queue from the Unschedulable plugin as queueAfterBackoff",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1", "fooPlugin2"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  nodeAdd,
			oldObj:                 nil,
			newObj:                 st.MakeNode().Obj(),
			expected:               queueAfterBackoff,
			expectedExecutionCount: 2,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					nodeAdd: {
						{
							// Skip will be ignored
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnSkip,
						},
						{
							// Skip will be ignored
							PluginName:     "fooPlugin2",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name: "Queueing hint function that isn't from the plugin in UnschedulablePlugins/PendingPlugins is ignored",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1", "fooPlugin2"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  nodeAdd,
			oldObj:                 nil,
			newObj:                 st.MakeNode().Node,
			expected:               queueSkip,
			expectedExecutionCount: 2,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					nodeAdd: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnSkip,
						},
						{
							PluginName:     "fooPlugin2",
							QueueingHintFn: queueHintReturnSkip,
						},
						{
							PluginName:     "fooPlugin3",
							QueueingHintFn: queueHintReturnQueue, // It'll be ignored.
						},
					},
				},
			},
		},
		{
			name: "If event is specific Node update event, queueing hint function for NodeUpdate/UpdateNodeLabel is also executed",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1", "fooPlugin2"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeLabel},
			oldObj:                 nil,
			newObj:                 st.MakeNode().Obj(),
			expected:               queueAfterBackoff,
			expectedExecutionCount: 1,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeLabel}: {
						{
							PluginName: "fooPlugin1",
							// It's only executed and interpreted as queueAfterBackoff.
							// The function doesn't run other hints because this Pod doesn't have PendingPlugins.
							QueueingHintFn: queueHintReturnQueue,
						},
						{
							PluginName:     "fooPlugin2",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Update}: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
					nodeAdd: { // not executed because NodeAdd is unrelated.
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name: "If event with '*' Resource, queueing hint function for specified Resource is also executed",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Add},
			oldObj:                 nil,
			newObj:                 st.MakeNode().Obj(),
			expected:               queueAfterBackoff,
			expectedExecutionCount: 1,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					framework.ClusterEvent{Resource: framework.WildCard, ActionType: framework.Add}: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
		{
			name: "If event is a wildcard one, queueing hint function for all kinds of events is executed",
			podInfo: &framework.QueuedPodInfo{
				UnschedulablePlugins: sets.New("fooPlugin1"),
				PodInfo:              mustNewPodInfo(st.MakePod().Name("pod1").Namespace("ns1").UID("1").Obj()),
			},
			event:                  framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeLabel | framework.UpdateNodeTaint},
			oldObj:                 nil,
			newObj:                 st.MakeNode().Obj(),
			expected:               queueAfterBackoff,
			expectedExecutionCount: 1,
			queueingHintMap: QueueingHintMapPerProfile{
				"": {
					framework.ClusterEvent{Resource: framework.WildCard, ActionType: framework.All}: {
						{
							PluginName:     "fooPlugin1",
							QueueingHintFn: queueHintReturnQueue,
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			count = 0 // reset count every time
			logger, ctx := ktesting.NewTestContext(t)
			q := NewTestQueue(ctx, newDefaultQueueSort(), WithQueueingHintMapPerProfile(test.queueingHintMap))
			actual := q.isPodWorthRequeuing(logger, test.podInfo, test.event, test.oldObj, test.newObj)
			if actual != test.expected {
				t.Errorf("isPodWorthRequeuing() = %v, want %v", actual, test.expected)
			}
			if count != test.expectedExecutionCount {
				t.Errorf("isPodWorthRequeuing() executed queueing hint functions %v times, expected: %v", count, test.expectedExecutionCount)
			}
		})
	}
}

func Test_queuedPodInfo_gatedSetUponCreationAndUnsetUponUpdate(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	plugin, _ := schedulinggates.New(ctx, nil, nil, plfeature.Features{})
	m := map[string][]framework.PreEnqueuePlugin{"": {plugin.(framework.PreEnqueuePlugin)}}
	q := NewTestQueue(ctx, newDefaultQueueSort(), WithPreEnqueuePluginMap(m))

	gatedPod := st.MakePod().SchedulingGates([]string{"hello world"}).Obj()
	q.Add(logger, gatedPod)

	if !q.unschedulablePods.get(gatedPod).Gated {
		t.Error("Expected pod to be gated")
	}

	ungatedPod := gatedPod.DeepCopy()
	ungatedPod.Spec.SchedulingGates = nil
	q.Update(logger, gatedPod, ungatedPod)

	ungatedPodInfo, _ := q.Pop(logger)
	if ungatedPodInfo.Gated {
		t.Error("Expected pod to be ungated")
	}
}

func TestPriorityQueue_GetPod(t *testing.T) {
	activeQPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
		},
	}
	backoffQPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod2",
			Namespace: "default",
		},
	}
	unschedPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod3",
			Namespace: "default",
		},
	}

	_, ctx := ktesting.NewTestContext(t)
	q := NewTestQueue(ctx, newDefaultQueueSort())
	q.activeQ.underLock(func(unlockedActiveQ unlockedActiveQueuer) {
		unlockedActiveQ.AddOrUpdate(newQueuedPodInfoForLookup(activeQPod))
	})
	q.podBackoffQ.AddOrUpdate(newQueuedPodInfoForLookup(backoffQPod))
	q.unschedulablePods.addOrUpdate(newQueuedPodInfoForLookup(unschedPod))

	tests := []struct {
		name        string
		podName     string
		namespace   string
		expectedPod *v1.Pod
		expectedOK  bool
	}{
		{
			name:        "pod is found in activeQ",
			podName:     "pod1",
			namespace:   "default",
			expectedPod: activeQPod,
			expectedOK:  true,
		},
		{
			name:        "pod is found in backoffQ",
			podName:     "pod2",
			namespace:   "default",
			expectedPod: backoffQPod,
			expectedOK:  true,
		},
		{
			name:        "pod is found in unschedulablePods",
			podName:     "pod3",
			namespace:   "default",
			expectedPod: unschedPod,
			expectedOK:  true,
		},
		{
			name:        "pod is not found",
			podName:     "pod4",
			namespace:   "default",
			expectedPod: nil,
			expectedOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pInfo, ok := q.GetPod(tt.podName, tt.namespace)
			if ok != tt.expectedOK {
				t.Errorf("Expected ok=%v, but got ok=%v", tt.expectedOK, ok)
			}

			if tt.expectedPod == nil {
				if pInfo == nil {
					return
				}
				t.Fatalf("Expected pod is empty, but got pod=%v", pInfo.Pod)
			}

			if !cmp.Equal(pInfo.Pod, tt.expectedPod) {
				t.Errorf("Expected pod=%v, but got pod=%v", tt.expectedPod, pInfo.Pod)
			}
		})
	}
}
