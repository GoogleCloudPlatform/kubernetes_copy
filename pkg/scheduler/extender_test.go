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

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2/ktesting"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
)

var scheduleResultCmpOpts = []cmp.Option{
	cmpopts.EquateComparable(),
	cmp.AllowUnexported(ScheduleResult{}),
}

func TestSchedulerWithExtenders(t *testing.T) {
	tests := []struct {
		name            string
		registerPlugins []tf.RegisterPluginFunc
		extenders       []tf.FakeExtender
		nodes           []string
		expectedResult  ScheduleResult
		expectsErr      bool
	}{
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
				},
				{
					ExtenderName: "FakeExtender2",
					Predicates:   []tf.FitPredicate{tf.ErrorPredicateExtender},
				},
			},
			nodes:      []string{"node1", "node2"},
			expectsErr: true,
			name:       "test 1",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
				},
				{
					ExtenderName: "FakeExtender2",
					Predicates:   []tf.FitPredicate{tf.FalsePredicateExtender},
				},
			},
			nodes:      []string{"node1", "node2"},
			expectsErr: true,
			name:       "test 2",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterScorePlugin("EqualPrioritizerPlugin", tf.NewEqualPrioritizerPlugin(), 1),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
				},
				{
					ExtenderName: "FakeExtender2",
					Predicates:   []tf.FitPredicate{tf.Node1PredicateExtender},
				},
			},
			nodes: []string{"node1", "node2"},
			expectedResult: ScheduleResult{
				SuggestedHost:  "node1",
				EvaluatedNodes: 2,
				FeasibleNodes:  1,
			},
			name: "test 3",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.Node2PredicateExtender},
				},
				{
					ExtenderName: "FakeExtender2",
					Predicates:   []tf.FitPredicate{tf.Node1PredicateExtender},
				},
			},
			nodes:      []string{"node1", "node2"},
			expectsErr: true,
			name:       "test 4",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
					Prioritizers: []tf.PriorityConfig{{Function: tf.ErrorPrioritizerExtender, Weight: 10}},
					Weight:       1,
				},
			},
			nodes: []string{"node1"},
			expectedResult: ScheduleResult{
				SuggestedHost:  "node1",
				EvaluatedNodes: 1,
				FeasibleNodes:  1,
			},
			name: "test 5",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
					Prioritizers: []tf.PriorityConfig{{Function: tf.Node1PrioritizerExtender, Weight: 10}},
					Weight:       1,
				},
				{
					ExtenderName: "FakeExtender2",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
					Prioritizers: []tf.PriorityConfig{{Function: tf.Node2PrioritizerExtender, Weight: 10}},
					Weight:       5,
				},
			},
			nodes: []string{"node1", "node2"},
			expectedResult: ScheduleResult{
				SuggestedHost:  "node2",
				EvaluatedNodes: 2,
				FeasibleNodes:  2,
			},
			name: "test 6",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterScorePlugin("Node2Prioritizer", tf.NewNode2PrioritizerPlugin(), 20),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
					Prioritizers: []tf.PriorityConfig{{Function: tf.Node1PrioritizerExtender, Weight: 10}},
					Weight:       1,
				},
			},
			nodes: []string{"node1", "node2"},
			expectedResult: ScheduleResult{
				SuggestedHost:  "node2",
				EvaluatedNodes: 2,
				FeasibleNodes:  2,
			}, // node2 has higher score
			name: "test 7",
		},
		{
			// Scheduler is expected to not send pod to extender in
			// Filter/Prioritize phases if the extender is not interested in
			// the pod.
			//
			// If scheduler sends the pod by mistake, the test would fail
			// because of the errors from errorPredicateExtender and/or
			// errorPrioritizerExtender.
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterScorePlugin("Node2Prioritizer", tf.NewNode2PrioritizerPlugin(), 1),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.ErrorPredicateExtender},
					Prioritizers: []tf.PriorityConfig{{Function: tf.ErrorPrioritizerExtender, Weight: 10}},
					UnInterested: true,
				},
			},
			nodes:      []string{"node1", "node2"},
			expectsErr: false,
			expectedResult: ScheduleResult{
				SuggestedHost:  "node2",
				EvaluatedNodes: 2,
				FeasibleNodes:  2,
			}, // node2 has higher score
			name: "test 8",
		},
		{
			// Scheduling is expected to not fail in
			// Filter/Prioritize phases if the extender is not available and ignorable.
			//
			// If scheduler did not ignore the extender, the test would fail
			// because of the errors from errorPredicateExtender.
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterScorePlugin("EqualPrioritizerPlugin", tf.NewEqualPrioritizerPlugin(), 1),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.ErrorPredicateExtender},
					Ignorable:    true,
				},
				{
					ExtenderName: "FakeExtender2",
					Predicates:   []tf.FitPredicate{tf.Node1PredicateExtender},
				},
			},
			nodes:      []string{"node1", "node2"},
			expectsErr: false,
			expectedResult: ScheduleResult{
				SuggestedHost:  "node1",
				EvaluatedNodes: 2,
				FeasibleNodes:  1,
			},
			name: "test 9",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Predicates:   []tf.FitPredicate{tf.TruePredicateExtender},
				},
				{
					ExtenderName: "FakeExtender2",
					Predicates:   []tf.FitPredicate{tf.Node1PredicateExtender},
				},
			},
			nodes: []string{"node1", "node2"},
			expectedResult: ScheduleResult{
				SuggestedHost:  "node1",
				EvaluatedNodes: 2,
				FeasibleNodes:  1,
			},
			name: "test 10 - no scoring, extender filters configured, multiple feasible nodes are evaluated",
		},
		{
			registerPlugins: []tf.RegisterPluginFunc{
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			},
			extenders: []tf.FakeExtender{
				{
					ExtenderName: "FakeExtender1",
					Binder:       func() error { return nil },
				},
			},
			nodes: []string{"node1", "node2"},
			expectedResult: ScheduleResult{
				SuggestedHost:  "node1",
				EvaluatedNodes: 1,
				FeasibleNodes:  1,
			},
			name: "test 11 - no scoring, no prefilters or  extender filters configured, a single feasible node is evaluated",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := clientsetfake.NewClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)

			var extenders []framework.Extender
			for ii := range test.extenders {
				extenders = append(extenders, &test.extenders[ii])
			}
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			cache := internalcache.New(ctx, time.Duration(0))
			for _, name := range test.nodes {
				cache.AddNode(logger, createNode(name))
			}
			fwk, err := tf.NewFramework(
				ctx,
				test.registerPlugins, "",
				runtime.WithClientSet(client),
				runtime.WithInformerFactory(informerFactory),
				runtime.WithPodNominator(internalqueue.NewSchedulingQueue(nil, informerFactory)),
				runtime.WithLogger(logger),
			)
			if err != nil {
				t.Fatal(err)
			}

			sched := &Scheduler{
				Cache:                    cache,
				nodeInfoSnapshot:         emptySnapshot,
				percentageOfNodesToScore: schedulerapi.DefaultPercentageOfNodesToScore,
				Extenders:                extenders,
				logger:                   logger,
			}
			sched.applyDefaultHandlers()

			podIgnored := &v1.Pod{}
			result, err := sched.SchedulePod(ctx, fwk, framework.NewCycleState(), podIgnored)
			if test.expectsErr {
				if err == nil {
					t.Errorf("Unexpected non-error, result %+v", result)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
					return
				}

				if diff := cmp.Diff(test.expectedResult, result, scheduleResultCmpOpts...); diff != "" {
					t.Errorf("Unexpected result: (-want, +got):\n%s", diff)
				}
			}
		})
	}
}

func createNode(name string) *v1.Node {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestIsInterested(t *testing.T) {
	mem := &HTTPExtender{
		managedResources: sets.New[string](),
	}
	mem.managedResources.Insert("memory")

	for _, tc := range []struct {
		label    string
		extender *HTTPExtender
		pod      *v1.Pod
		want     bool
	}{
		{
			label: "Empty managed resources",
			extender: &HTTPExtender{
				managedResources: sets.New[string](),
			},
			pod:  &v1.Pod{},
			want: true,
		},
		{
			label:    "Managed memory, empty resources",
			extender: mem,
			pod:      st.MakePod().Container("app").Obj(),
			want:     false,
		},
		{
			label:    "Managed memory, container memory with Requests",
			extender: mem,
			pod: st.MakePod().Req(map[v1.ResourceName]string{
				"memory": "0",
			}).Obj(),
			want: true,
		},
		{
			label:    "Managed memory, container memory with Limits",
			extender: mem,
			pod: st.MakePod().Lim(map[v1.ResourceName]string{
				"memory": "0",
			}).Obj(),
			want: true,
		},
		{
			label:    "Managed memory, init container memory",
			extender: mem,
			pod: st.MakePod().Container("app").InitReq(map[v1.ResourceName]string{
				"memory": "0",
			}).Obj(),
			want: true,
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := tc.extender.IsInterested(tc.pod); got != tc.want {
				t.Fatalf("IsInterested(%v) = %v, wanted %v", tc.pod, got, tc.want)
			}
		})
	}
}

func TestConvertToMetaVictims(t *testing.T) {
	tests := []struct {
		name              string
		nodeNameToVictims map[string]*extenderv1.Victims
		want              map[string]*extenderv1.MetaVictims
	}{
		{
			name: "test NumPDBViolations is transferred from nodeNameToVictims to nodeNameToMetaVictims",
			nodeNameToVictims: map[string]*extenderv1.Victims{
				"node1": {
					Pods: []*v1.Pod{
						st.MakePod().Name("pod1").UID("uid1").Obj(),
						st.MakePod().Name("pod3").UID("uid3").Obj(),
					},
					NumPDBViolations: 1,
				},
				"node2": {
					Pods: []*v1.Pod{
						st.MakePod().Name("pod2").UID("uid2").Obj(),
						st.MakePod().Name("pod4").UID("uid4").Obj(),
					},
					NumPDBViolations: 2,
				},
			},
			want: map[string]*extenderv1.MetaVictims{
				"node1": {
					Pods: []*extenderv1.MetaPod{
						{UID: "uid1"},
						{UID: "uid3"},
					},
					NumPDBViolations: 1,
				},
				"node2": {
					Pods: []*extenderv1.MetaPod{
						{UID: "uid2"},
						{UID: "uid4"},
					},
					NumPDBViolations: 2,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertToMetaVictims(tt.nodeNameToVictims)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Unexpected convertToMetaVictims(): (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestConvertToVictims(t *testing.T) {
	tests := []struct {
		name                  string
		httpExtender          *HTTPExtender
		nodeNameToMetaVictims map[string]*extenderv1.MetaVictims
		nodeNames             []string
		podsInNodeList        []*v1.Pod
		nodeInfos             framework.NodeInfoLister
		want                  map[string]*extenderv1.Victims
		wantErr               bool
	}{
		{
			name:         "test NumPDBViolations is transferred from NodeNameToMetaVictims to newNodeNameToVictims",
			httpExtender: &HTTPExtender{},
			nodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				"node1": {
					Pods: []*extenderv1.MetaPod{
						{UID: "uid1"},
						{UID: "uid3"},
					},
					NumPDBViolations: 1,
				},
				"node2": {
					Pods: []*extenderv1.MetaPod{
						{UID: "uid2"},
						{UID: "uid4"},
					},
					NumPDBViolations: 2,
				},
			},
			nodeNames: []string{"node1", "node2"},
			podsInNodeList: []*v1.Pod{
				st.MakePod().Name("pod1").UID("uid1").Obj(),
				st.MakePod().Name("pod2").UID("uid2").Obj(),
				st.MakePod().Name("pod3").UID("uid3").Obj(),
				st.MakePod().Name("pod4").UID("uid4").Obj(),
			},
			nodeInfos: nil,
			want: map[string]*extenderv1.Victims{
				"node1": {
					Pods: []*v1.Pod{
						st.MakePod().Name("pod1").UID("uid1").Obj(),
						st.MakePod().Name("pod3").UID("uid3").Obj(),
					},
					NumPDBViolations: 1,
				},
				"node2": {
					Pods: []*v1.Pod{
						st.MakePod().Name("pod2").UID("uid2").Obj(),
						st.MakePod().Name("pod4").UID("uid4").Obj(),
					},
					NumPDBViolations: 2,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// nodeInfos instantiations
			nodeInfoList := make([]*framework.NodeInfo, 0, len(tt.nodeNames))
			for i, nm := range tt.nodeNames {
				nodeInfo := framework.NewNodeInfo()
				node := createNode(nm)
				nodeInfo.SetNode(node)
				nodeInfo.AddPod(tt.podsInNodeList[i])
				nodeInfo.AddPod(tt.podsInNodeList[i+2])
				nodeInfoList = append(nodeInfoList, nodeInfo)
			}
			tt.nodeInfos = tf.NodeInfoLister(nodeInfoList)

			got, err := tt.httpExtender.convertToVictims(tt.nodeNameToMetaVictims, tt.nodeInfos)
			if (err != nil) != tt.wantErr {
				t.Errorf("convertToVictims() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Unexpected convertToVictims(): (-want, +got):\n%s", diff)
			}
		})
	}
}
