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

package podtopologyspread

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	plugintesting "k8s.io/kubernetes/pkg/scheduler/framework/plugins/testing"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"k8s.io/utils/ptr"
)

var podTopologySpreadFunc = frameworkruntime.FactoryAdapter(feature.Features{}, New)

// TestPreScoreSkip tests the cases that TopologySpread#PreScore returns the Skip status.
func TestPreScoreSkip(t *testing.T) {
	tests := []struct {
		name   string
		pod    *v1.Pod
		nodes  []*v1.Node
		objs   []runtime.Object
		config config.PodTopologySpreadArgs
	}{
		{
			name: "the pod doesn't have soft topology spread Constraints",
			pod:  st.MakePod().Name("p").Namespace("default").Obj(),
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
			},
		},
		{
			name: "default constraints and a replicaset that doesn't match",
			pod:  st.MakePod().Name("p").Namespace("default").Label("foo", "bar").Label("baz", "sup").OwnerReference("rs2", appsv1.SchemeGroupVersion.WithKind("ReplicaSet")).Obj(),
			config: config.PodTopologySpreadArgs{
				DefaultConstraints: []v1.TopologySpreadConstraint{
					{
						MaxSkew:           2,
						TopologyKey:       "planet",
						WhenUnsatisfiable: v1.ScheduleAnyway,
					},
				},
				DefaultingType: config.ListDefaulting,
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("planet", "mars").Obj(),
			},
			objs: []runtime.Object{
				&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rs1"}, Spec: appsv1.ReplicaSetSpec{Selector: st.MakeLabelSelector().Exists("tar").Obj()}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(tt.objs...), 0)
			f, err := frameworkruntime.NewFramework(ctx, nil, nil,
				frameworkruntime.WithSnapshotSharedLister(cache.NewSnapshot(nil, tt.nodes)),
				frameworkruntime.WithInformerFactory(informerFactory))
			if err != nil {
				t.Fatalf("Failed creating framework runtime: %v", err)
			}
			pl, err := New(ctx, &tt.config, f, feature.Features{})
			if err != nil {
				t.Fatalf("Failed creating plugin: %v", err)
			}
			informerFactory.Start(ctx.Done())
			informerFactory.WaitForCacheSync(ctx.Done())
			p := pl.(*PodTopologySpread)
			cs := framework.NewCycleState()
			if s := p.PreScore(ctx, cs, tt.pod, tf.BuildNodeInfos(tt.nodes)); !s.IsSkip() {
				t.Fatalf("Expected skip but got %v", s.AsError())
			}
		})
	}
}

func TestPreScoreStateEmptyNodes(t *testing.T) {
	tests := []struct {
		name                      string
		pod                       *v1.Pod
		nodes                     []*v1.Node
		objs                      []runtime.Object
		config                    config.PodTopologySpreadArgs
		want                      *preScoreState
		enableNodeInclusionPolicy bool
	}{
		{
			name: "normal case",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, fooSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
					{
						MaxSkew:            1,
						TopologyKey:        v1.LabelHostname,
						Selector:           mustConvertLabelSelectorAsSelector(t, fooSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{
					{
						"zone1": ptr.To[int64](0),
						"zone2": ptr.To[int64](0),
					},
					{},
				},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2), topologyNormalizingWeight(3)},
			},
		},
		{
			name: "null selector",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, nil, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           labels.Nothing(),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"zone1": ptr.To[int64](0),
					"zone2": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2)},
			},
		},
		{
			name: "node-x doesn't have label zone",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label(v1.LabelHostname, "node-x").Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, fooSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
					{
						MaxSkew:            1,
						TopologyKey:        v1.LabelHostname,
						Selector:           mustConvertLabelSelectorAsSelector(t, barSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New("node-x"),
				TopologyValueToPodCounts: []map[string]*int64{
					{
						"zone1": ptr.To[int64](0),
					},
					{},
				},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(1), topologyNormalizingWeight(2)},
			},
		},
		{
			name: "system default constraints and a replicaset",
			pod:  st.MakePod().Name("p").Namespace("default").Label("foo", "tar").Label("baz", "sup").OwnerReference("rs1", appsv1.SchemeGroupVersion.WithKind("ReplicaSet")).Obj(),
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.SystemDefaulting,
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Label(v1.LabelTopologyZone, "mars").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Label(v1.LabelTopologyZone, "mars").Obj(),
				// Nodes with no zone are not excluded. They are considered a separate zone.
				st.MakeNode().Name("node-c").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label(v1.LabelHostname, "node-d").Obj(),
			},
			objs: []runtime.Object{
				&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rs1"}, Spec: appsv1.ReplicaSetSpec{Selector: fooSelector}},
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            3,
						TopologyKey:        v1.LabelHostname,
						Selector:           mustConvertLabelSelectorAsSelector(t, fooSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
					{
						MaxSkew:            5,
						TopologyKey:        v1.LabelTopologyZone,
						Selector:           mustConvertLabelSelectorAsSelector(t, fooSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{
					{},
					{
						"mars": ptr.To[int64](0),
						"":     ptr.To[int64](0),
					},
				},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(4), topologyNormalizingWeight(2)},
			},
		},
		{
			name: "default constraints and a replicaset",
			pod:  st.MakePod().Name("p").Namespace("default").Label("foo", "tar").Label("baz", "sup").OwnerReference("rs1", appsv1.SchemeGroupVersion.WithKind("ReplicaSet")).Obj(),
			config: config.PodTopologySpreadArgs{
				DefaultConstraints: []v1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       v1.LabelHostname,
						WhenUnsatisfiable: v1.ScheduleAnyway,
					},
					{MaxSkew: 2,
						TopologyKey:       "rack",
						WhenUnsatisfiable: v1.DoNotSchedule,
					},
					{MaxSkew: 2, TopologyKey: "planet", WhenUnsatisfiable: v1.ScheduleAnyway},
				},
				DefaultingType: config.ListDefaulting,
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("rack", "rack1").Label(v1.LabelHostname, "node-a").Label("planet", "mars").Obj(),
			},
			objs: []runtime.Object{
				&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rs1"}, Spec: appsv1.ReplicaSetSpec{Selector: fooSelector}},
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        v1.LabelHostname,
						Selector:           mustConvertLabelSelectorAsSelector(t, fooSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
					{
						MaxSkew:            2,
						TopologyKey:        "planet",
						Selector:           mustConvertLabelSelectorAsSelector(t, fooSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{
					{},
					{
						"mars": ptr.To[int64](0),
					},
				},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(1), topologyNormalizingWeight(1)},
			},
		},
		{
			name: "default constraints and a replicaset, but pod has constraints",
			pod: st.MakePod().Name("p").Namespace("default").Label("foo", "bar").Label("baz", "sup").
				OwnerReference("rs1", appsv1.SchemeGroupVersion.WithKind("ReplicaSet")).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, barSelector, nil, nil, nil, nil).
				SpreadConstraint(2, "planet", v1.ScheduleAnyway, st.MakeLabelSelector().Label("baz", "sup").Obj(), nil, nil, nil, nil).
				Obj(),
			config: config.PodTopologySpreadArgs{
				DefaultConstraints: []v1.TopologySpreadConstraint{
					{
						MaxSkew:           2,
						TopologyKey:       "galaxy",
						WhenUnsatisfiable: v1.ScheduleAnyway,
					},
				},
				DefaultingType: config.ListDefaulting,
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("planet", "mars").Label("galaxy", "andromeda").Obj(),
			},
			objs: []runtime.Object{
				&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rs1"}, Spec: appsv1.ReplicaSetSpec{Selector: fooSelector}},
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            2,
						TopologyKey:        "planet",
						Selector:           mustConvertLabelSelectorAsSelector(t, st.MakeLabelSelector().Label("baz", "sup").Obj()),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"mars": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(1)},
			},
		},
		{
			name: "NodeAffinityPolicy honored with labelSelectors",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, barSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"zone1": ptr.To[int64](0),
					"zone2": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2)},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeAffinityPolicy ignored with labelSelectors",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, barSelector, nil, &ignorePolicy, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, barSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyIgnore,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"zone1": ptr.To[int64](0),
					"zone2": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2)},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeAffinityPolicy honored with nodeAffinity",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeAffinityIn("foo", []string{""}, st.NodeSelectorTypeMatchExpressions).
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, barSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"zone1": ptr.To[int64](0),
					"zone2": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2)},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeAffinityPolicy ignored with nodeAffinity",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeAffinityIn("foo", []string{""}, st.NodeSelectorTypeMatchExpressions).
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, barSelector, nil, &ignorePolicy, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, barSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyIgnore,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"zone1": ptr.To[int64](0),
					"zone2": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2)},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeTaintsPolicy honored",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, barSelector, nil, nil, &honorPolicy, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Taints(taints).Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, barSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyHonor,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"zone1": ptr.To[int64](0),
					"zone2": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2)},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeTaintsPolicy ignored",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("foo", "").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Taints(taints).Obj(),
			},
			config: config.PodTopologySpreadArgs{
				DefaultingType: config.ListDefaulting,
			},
			want: &preScoreState{
				Constraints: []topologySpreadConstraint{
					{
						MaxSkew:            1,
						TopologyKey:        "zone",
						Selector:           mustConvertLabelSelectorAsSelector(t, barSelector),
						MinDomains:         1,
						NodeAffinityPolicy: v1.NodeInclusionPolicyHonor,
						NodeTaintsPolicy:   v1.NodeInclusionPolicyIgnore,
					},
				},
				IgnoredNodes: sets.New[string](),
				TopologyValueToPodCounts: []map[string]*int64{{
					"zone1": ptr.To[int64](0),
					"zone2": ptr.To[int64](0),
				}},
				TopologyNormalizingWeight: []float64{topologyNormalizingWeight(2)},
			},
			enableNodeInclusionPolicy: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(tt.objs...), 0)
			f, err := frameworkruntime.NewFramework(ctx, nil, nil,
				frameworkruntime.WithSnapshotSharedLister(cache.NewSnapshot(nil, tt.nodes)),
				frameworkruntime.WithInformerFactory(informerFactory))
			if err != nil {
				t.Fatalf("Failed creating framework runtime: %v", err)
			}
			pl, err := New(ctx, &tt.config, f, feature.Features{EnableNodeInclusionPolicyInPodTopologySpread: tt.enableNodeInclusionPolicy})
			if err != nil {
				t.Fatalf("Failed creating plugin: %v", err)
			}
			informerFactory.Start(ctx.Done())
			informerFactory.WaitForCacheSync(ctx.Done())
			p := pl.(*PodTopologySpread)
			cs := framework.NewCycleState()
			if s := p.PreScore(ctx, cs, tt.pod, tf.BuildNodeInfos(tt.nodes)); !s.IsSuccess() {
				t.Fatal(s.AsError())
			}

			got, err := getPreScoreState(cs)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got, preFilterStateCmpOpts...); diff != "" {
				t.Errorf("PodTopologySpread#PreScore() returned (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestPodTopologySpreadScore(t *testing.T) {
	tests := []struct {
		name                      string
		pod                       *v1.Pod
		existingPods              []*v1.Pod
		nodes                     []*v1.Node
		failedNodes               []*v1.Node // nodes + failedNodes = all nodes
		objs                      []runtime.Object
		want                      framework.NodeScoreList
		enableNodeInclusionPolicy bool
		enableMatchLabelKeys      bool
	}{
		// Explanation on the Legend:
		// a) X/Y means there are X matching pods on node1 and Y on node2, both nodes are candidates
		//   (i.e. they have passed all predicates)
		// b) X/~Y~ means there are X matching pods on node1 and Y on node2, but node Y is NOT a candidate
		// c) X/?Y? means there are X matching pods on node1 and Y on node2, both nodes are candidates
		//    but node2 doesn't have all required topologyKeys present.
		{
			name: "one constraint on node, no existing pods",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
				{Name: "node-b", Score: 100},
			},
		},
		{
			// if there is only one candidate node, it should be scored to 100
			name: "one constraint on node, only one node is candidate",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
			},
			failedNodes: []*v1.Node{
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
			},
		},
		{
			name: "one constraint on node, all nodes have the same number of matching pods",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
				{Name: "node-b", Score: 100},
			},
		},
		{
			// matching pods spread as 2/1/0/3.
			name: "one constraint on node, all 4 nodes are candidates",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-d1").Node("node-d").Label("foo", "").Obj(),
				st.MakePod().Name("p-d2").Node("node-d").Label("foo", "").Obj(),
				st.MakePod().Name("p-d3").Node("node-d").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label(v1.LabelHostname, "node-d").Obj(),
			},
			failedNodes: []*v1.Node{},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 20},
				{Name: "node-b", Score: 60},
				{Name: "node-c", Score: 100},
				{Name: "node-d", Score: 0},
			},
		},
		{
			name: "one constraint on node, null selector",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, nil, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-d1").Node("node-d").Label("foo", "").Obj(),
				st.MakePod().Name("p-d2").Node("node-d").Label("foo", "").Obj(),
				st.MakePod().Name("p-d3").Node("node-d").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label(v1.LabelHostname, "node-d").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
				{Name: "node-b", Score: 100},
				{Name: "node-c", Score: 100},
				{Name: "node-d", Score: 100},
			},
		},
		{
			name: "one constraint on node, all 4 nodes are candidates, maxSkew=2",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(2, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			// matching pods spread as 2/1/0/3.
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-d1").Node("node-d").Label("foo", "").Obj(),
				st.MakePod().Name("p-d2").Node("node-d").Label("foo", "").Obj(),
				st.MakePod().Name("p-d3").Node("node-d").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label(v1.LabelHostname, "node-d").Obj(),
			},
			failedNodes: []*v1.Node{},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 33}, // +13, compared to maxSkew=1
				{Name: "node-b", Score: 66}, // +6, compared to maxSkew=1
				{Name: "node-c", Score: 100},
				{Name: "node-d", Score: 16}, // +16, compared to maxSkew=1
			},
		},
		{
			name: "one constraint on node, all 4 nodes are candidates, maxSkew=3",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(3, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				// matching pods spread as 4/3/2/1.
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a3").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a4").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b2").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b3").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
				st.MakePod().Name("p-c2").Node("node-c").Label("foo", "").Obj(),
				st.MakePod().Name("p-d1").Node("node-d").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label(v1.LabelHostname, "node-d").Obj(),
			},
			failedNodes: []*v1.Node{},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 44}, // +16 compared to maxSkew=1
				{Name: "node-b", Score: 66}, // +9 compared to maxSkew=1
				{Name: "node-c", Score: 77}, // +6 compared to maxSkew=1
				{Name: "node-d", Score: 100},
			},
		},
		{
			name: "system defaulting, nodes don't have zone, pods match service",
			pod:  st.MakePod().Name("p").Label("foo", "").Obj(),
			existingPods: []*v1.Pod{
				// matching pods spread as 4/3/2/1.
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a3").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a4").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b2").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b3").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
				st.MakePod().Name("p-c2").Node("node-c").Label("foo", "").Obj(),
				st.MakePod().Name("p-d1").Node("node-d").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label(v1.LabelHostname, "node-d").Obj(),
			},
			failedNodes: []*v1.Node{},
			objs: []runtime.Object{
				&v1.Service{Spec: v1.ServiceSpec{Selector: map[string]string{"foo": ""}}},
			},
			want: []framework.NodeScore{
				// Same scores as if we were using one spreading constraint.
				{Name: "node-a", Score: 44},
				{Name: "node-b", Score: 66},
				{Name: "node-c", Score: 77},
				{Name: "node-d", Score: 100},
			},
		},
		{
			// matching pods spread as 4/2/1/~3~ (node4 is not a candidate)
			name: "one constraint on node, 3 out of 4 nodes are candidates",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a3").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a4").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b2").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-x1").Node("node-x").Label("foo", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y3").Node("node-y").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label(v1.LabelHostname, "node-x").Obj(),
			},
			failedNodes: []*v1.Node{
				st.MakeNode().Name("node-y").Label(v1.LabelHostname, "node-y").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 33},
				{Name: "node-b", Score: 83},
				{Name: "node-x", Score: 100},
			},
		},
		{
			// matching pods spread as 4/?2?/1/~3~, total = 4+?+1 = 5 (as node2 is problematic)
			name: "one constraint on node, 3 out of 4 nodes are candidates, one node doesn't match topology key",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a3").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a4").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b2").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-x1").Node("node-x").Label("foo", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y3").Node("node-y").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("n", "node-b").Obj(), // label `n` doesn't match topologyKey
				st.MakeNode().Name("node-x").Label(v1.LabelHostname, "node-x").Obj(),
			},
			failedNodes: []*v1.Node{
				st.MakeNode().Name("node-y").Label(v1.LabelHostname, "node-y").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 16},
				{Name: "node-b", Score: 0},
				{Name: "node-x", Score: 100},
			},
		},
		{
			// matching pods spread as 4/2/1/~3~
			name: "one constraint on zone, 3 out of 4 nodes are candidates",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a3").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a4").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b2").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-x1").Node("node-x").Label("foo", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y3").Node("node-y").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			failedNodes: []*v1.Node{
				st.MakeNode().Name("node-y").Label("zone", "zone2").Label(v1.LabelHostname, "node-y").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 75},
				{Name: "node-b", Score: 75},
				{Name: "node-x", Score: 100},
			},
		},
		{
			// matching pods spread as 2/~1~/2/~4~.
			name: "two Constraints on zone and node, 2 out of 4 nodes are candidates",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-x1").Node("node-x").Label("foo", "").Obj(),
				st.MakePod().Name("p-x2").Node("node-x").Label("foo", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y3").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y4").Node("node-y").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			failedNodes: []*v1.Node{
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-y").Label("zone", "zone2").Label(v1.LabelHostname, "node-y").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
				{Name: "node-x", Score: 63},
			},
		},
		{
			name: "two Constraints on zone, 2 out of 4 nodes are candidates",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Label("bar", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Label("bar", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-y").Label("foo", "").Label("bar", "").Obj(),
				st.MakePod().Name("p-y3").Node("node-y").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Obj(),
			},
			failedNodes: []*v1.Node{
				st.MakeNode().Name("node-b").Label("zone", "zone1").Obj(),
				st.MakeNode().Name("node-y").Label("zone", "zone2").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 85},
				{Name: "node-x", Score: 100},
			},
		},
		{
			// If Constraints hold different labelSelectors, it's a little complex.
			// +----------------------+------------------------+
			// |         zone1        |          zone2         |
			// +----------------------+------------------------+
			// | node-a |    node-b   | node-x |     node-y    |
			// +--------+-------------+--------+---------------+
			// | P{foo} | P{foo, bar} |        | P{foo} P{bar} |
			// +--------+-------------+--------+---------------+
			// For the first constraint (zone): the matching pods spread as 2/2/1/1
			// For the second constraint (node): the matching pods spread as 0/1/0/1
			name: "two Constraints on zone and node, with different labelSelectors",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Label("bar", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-y").Label("bar", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
				st.MakeNode().Name("node-y").Label("zone", "zone2").Label(v1.LabelHostname, "node-y").Obj(),
			},
			failedNodes: []*v1.Node{},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 60},
				{Name: "node-b", Score: 20},
				{Name: "node-x", Score: 100},
				{Name: "node-y", Score: 60},
			},
		},
		{
			// For the first constraint (zone): the matching pods spread as 0/0/2/2
			// For the second constraint (node): the matching pods spread as 0/1/0/1
			name: "two Constraints on zone and node, with different labelSelectors, some nodes have 0 pods",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-b1").Node("node-b").Label("bar", "").Obj(),
				st.MakePod().Name("p-x1").Node("node-x").Label("foo", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Label("bar", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
				st.MakeNode().Name("node-y").Label("zone", "zone2").Label(v1.LabelHostname, "node-y").Obj(),
			},
			failedNodes: []*v1.Node{},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
				{Name: "node-b", Score: 60},
				{Name: "node-x", Score: 40},
				{Name: "node-y", Score: 0},
			},
		},
		{
			// For the first constraint (zone): the matching pods spread as 2/2/1/~1~
			// For the second constraint (node): the matching pods spread as 0/1/0/~1~
			name: "two Constraints on zone and node, with different labelSelectors, 3 out of 4 nodes are candidates",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Label("bar", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-y").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-y").Label("bar", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label(v1.LabelHostname, "node-x").Obj(),
			},
			failedNodes: []*v1.Node{
				st.MakeNode().Name("node-y").Label("zone", "zone2").Label(v1.LabelHostname, "node-y").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 50},
				{Name: "node-b", Score: 25},
				{Name: "node-x", Score: 100},
			},
		},
		{
			name: "existing pods in a different namespace do not count",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Namespace("ns1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b2").Node("node-b").Label("foo", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
				{Name: "node-b", Score: 33},
			},
		},
		{
			name: "terminating Pods should be excluded",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label(v1.LabelHostname, "node-b").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a").Node("node-a").Label("foo", "").Terminating().Obj(),
				st.MakePod().Name("p-b").Node("node-b").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 100},
				{Name: "node-b", Score: 0},
			},
		},
		{
			// This test is artificial. In the real world, API Server would fail a pod's creation
			// when non-default minDomains is specified along with SchedulingAnyway.
			name: "minDomains has no effect when ScheduleAnyway",
			pod: st.MakePod().Name("p").Label("foo", "").SpreadConstraint(
				2,
				"node",
				v1.ScheduleAnyway,
				fooSelector,
				ptr.To[int32](10), // larger than the number of domains(3)
				nil,
				nil,
				nil,
			).Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-b2").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 75},
				{Name: "node-b", Score: 75},
				{Name: "node-c", Score: 100},
			},
		},
		{
			name: "NodeAffinityPolicy honoed with labelSelectors",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "node", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Label("foo", "").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Label("foo", "").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 0},
				{Name: "node-b", Score: 33},
				{Name: "node-c", Score: 100},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeAffinityPolicy ignored with labelSelectors",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "node", v1.ScheduleAnyway, fooSelector, nil, &ignorePolicy, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Label("foo", "").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Label("foo", "").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 66},
				{Name: "node-b", Score: 100},
				{Name: "node-c", Score: 100},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeAffinityPolicy honoed with nodeAffinity",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeAffinityIn("foo", []string{""}, st.NodeSelectorTypeMatchExpressions).
				SpreadConstraint(1, "node", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Label("foo", "").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Label("foo", "").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 0},
				{Name: "node-b", Score: 33},
				{Name: "node-c", Score: 100},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeAffinityPolicy ignored with nodeAffinity",
			pod: st.MakePod().Name("p").Label("foo", "").
				NodeAffinityIn("foo", []string{""}, st.NodeSelectorTypeMatchExpressions).
				SpreadConstraint(1, "node", v1.ScheduleAnyway, fooSelector, nil, &ignorePolicy, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Label("foo", "").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Label("foo", "").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 66},
				{Name: "node-b", Score: 100},
				{Name: "node-c", Score: 100},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeTaintsPolicy honored",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "node", v1.ScheduleAnyway, fooSelector, nil, nil, &honorPolicy, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Taints(taints).Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 0},
				{Name: "node-b", Score: 33},
				{Name: "node-c", Score: 100},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeTaintsPolicy ignored",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, "node", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Taints(taints).Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-a2").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 66},
				{Name: "node-b", Score: 100},
				{Name: "node-c", Score: 100},
			},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "matchLabelKeys ignored when feature gate disabled",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").Label("baz", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, []string{"baz"}).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, barSelector, nil, nil, nil, []string{"baz"}).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Label("bar", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-c").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-c").Label("bar", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label("zone", "zone2").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label("zone", "zone2").Label(v1.LabelHostname, "node-d").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 60},
				{Name: "node-b", Score: 20},
				{Name: "node-c", Score: 60},
				{Name: "node-d", Score: 100},
			},
			enableMatchLabelKeys: false,
		},
		{
			name: "matchLabelKeys ANDed with LabelSelector when LabelSelector is empty",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, st.MakeLabelSelector().Obj(), nil, nil, nil, []string{"foo"}).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, st.MakeLabelSelector().Obj(), nil, nil, nil, []string{"bar"}).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Label("bar", "").Obj(),
				st.MakePod().Name("p-y1").Node("node-c").Label("foo", "").Obj(),
				st.MakePod().Name("p-y2").Node("node-c").Label("bar", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label("zone", "zone2").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label("zone", "zone2").Label(v1.LabelHostname, "node-d").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 60},
				{Name: "node-b", Score: 20},
				{Name: "node-c", Score: 60},
				{Name: "node-d", Score: 100},
			},
			enableMatchLabelKeys: true,
		},
		{
			name: "matchLabelKeys ANDed with LabelSelector when LabelSelector isn't empty",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").Label("baz", "").
				SpreadConstraint(1, "zone", v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, barSelector, nil, nil, nil, []string{"baz"}).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p-a1").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("p-b1").Node("node-b").Label("foo", "").Label("bar", "").Label("baz", "").Obj(),
				st.MakePod().Name("p-c1").Node("node-c").Label("foo", "").Obj(),
				st.MakePod().Name("p-c2").Node("node-c").Label("bar", "").Obj(),
				st.MakePod().Name("p-d3").Node("node-c").Label("bar", "").Label("baz", "").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label(v1.LabelHostname, "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label(v1.LabelHostname, "node-b").Obj(),
				st.MakeNode().Name("node-c").Label("zone", "zone2").Label(v1.LabelHostname, "node-c").Obj(),
				st.MakeNode().Name("node-d").Label("zone", "zone2").Label(v1.LabelHostname, "node-d").Obj(),
			},
			want: []framework.NodeScore{
				{Name: "node-a", Score: 60},
				{Name: "node-b", Score: 20},
				{Name: "node-c", Score: 60},
				{Name: "node-d", Score: 100},
			},
			enableMatchLabelKeys: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			t.Cleanup(cancel)
			allNodes := append([]*v1.Node{}, tt.nodes...)
			allNodes = append(allNodes, tt.failedNodes...)
			state := framework.NewCycleState()
			pl := plugintesting.SetupPluginWithInformers(ctx, t, podTopologySpreadFunc, &config.PodTopologySpreadArgs{DefaultingType: config.SystemDefaulting}, cache.NewSnapshot(tt.existingPods, allNodes), tt.objs)
			p := pl.(*PodTopologySpread)
			p.enableNodeInclusionPolicyInPodTopologySpread = tt.enableNodeInclusionPolicy
			p.enableMatchLabelKeysInPodTopologySpread = tt.enableMatchLabelKeys

			status := p.PreScore(ctx, state, tt.pod, tf.BuildNodeInfos(tt.nodes))
			if !status.IsSuccess() {
				t.Errorf("unexpected error: %v", status)
			}

			var gotList framework.NodeScoreList
			for _, n := range tt.nodes {
				nodeName := n.Name
				score, status := p.Score(ctx, state, tt.pod, nodeName)
				if !status.IsSuccess() {
					t.Errorf("unexpected error: %v", status)
				}
				gotList = append(gotList, framework.NodeScore{Name: nodeName, Score: score})
			}

			status = p.NormalizeScore(ctx, state, tt.pod, gotList)
			if !status.IsSuccess() {
				t.Errorf("unexpected error: %v", status)
			}
			if diff := cmp.Diff(tt.want, gotList, preFilterStateCmpOpts...); diff != "" {
				t.Errorf("unexpected scores (-want,+got):\n%s", diff)
			}
		})
	}
}

func BenchmarkTestPodTopologySpreadScore(b *testing.B) {
	tests := []struct {
		name             string
		pod              *v1.Pod
		existingPodsNum  int
		allNodesNum      int
		filteredNodesNum int
	}{
		{
			name: "1000nodes/single-constraint-zone",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelTopologyZone, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPodsNum:  10000,
			allNodesNum:      1000,
			filteredNodesNum: 500,
		},
		{
			name: "1000nodes/single-constraint-node",
			pod: st.MakePod().Name("p").Label("foo", "").
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				Obj(),
			existingPodsNum:  10000,
			allNodesNum:      1000,
			filteredNodesNum: 500,
		},
		{
			name: "1000nodes/two-Constraints-zone-node",
			pod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").
				SpreadConstraint(1, v1.LabelTopologyZone, v1.ScheduleAnyway, fooSelector, nil, nil, nil, nil).
				SpreadConstraint(1, v1.LabelHostname, v1.ScheduleAnyway, barSelector, nil, nil, nil, nil).
				Obj(),
			existingPodsNum:  10000,
			allNodesNum:      1000,
			filteredNodesNum: 500,
		},
	}
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			_, ctx := ktesting.NewTestContext(b)
			existingPods, allNodes, filteredNodes := st.MakeNodesAndPodsForEvenPodsSpread(tt.pod.Labels, tt.existingPodsNum, tt.allNodesNum, tt.filteredNodesNum)
			state := framework.NewCycleState()
			pl := plugintesting.SetupPlugin(ctx, b, podTopologySpreadFunc, &config.PodTopologySpreadArgs{DefaultingType: config.ListDefaulting}, cache.NewSnapshot(existingPods, allNodes))
			p := pl.(*PodTopologySpread)

			status := p.PreScore(ctx, state, tt.pod, tf.BuildNodeInfos(filteredNodes))
			if !status.IsSuccess() {
				b.Fatalf("unexpected error: %v", status)
			}
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var gotList framework.NodeScoreList
				for _, n := range filteredNodes {
					nodeName := n.Name
					score, status := p.Score(ctx, state, tt.pod, nodeName)
					if !status.IsSuccess() {
						b.Fatalf("unexpected error: %v", status)
					}
					gotList = append(gotList, framework.NodeScore{Name: nodeName, Score: score})
				}

				status = p.NormalizeScore(ctx, state, tt.pod, gotList)
				if !status.IsSuccess() {
					b.Fatal(status)
				}
			}
		})
	}
}
