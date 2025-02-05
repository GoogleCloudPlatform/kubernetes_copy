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

package interpodaffinity

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	plugintesting "k8s.io/kubernetes/pkg/scheduler/framework/plugins/testing"
	schedruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

var (
	defaultNamespace      = ""
	preFilterStateCmpOpts = []cmp.Option{
		cmp.AllowUnexported(preFilterState{}, framework.PodInfo{}),
	}
)

func createPodWithAffinityTerms(namespace, nodeName string, labels map[string]string, affinity, antiAffinity []v1.PodAffinityTerm) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels:    labels,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			NodeName: nodeName,
			Affinity: &v1.Affinity{
				PodAffinity: &v1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: affinity,
				},
				PodAntiAffinity: &v1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: antiAffinity,
				},
			},
		},
	}
}

func TestRequiredAffinitySingleNode(t *testing.T) {
	podLabel := map[string]string{"service": "securityscan"}
	pod := st.MakePod().Labels(podLabel).Node("node1").Obj()

	labels1 := map[string]string{
		"region": "r1",
		"zone":   "z11",
	}
	podLabel2 := map[string]string{"security": "S1"}
	node1 := v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labels1}}
	metrics.Register()

	tests := []struct {
		pod                 *v1.Pod
		pods                []*v1.Pod
		node                *v1.Node
		name                string
		wantPreFilterStatus *framework.Status
		wantFilterStatus    *framework.Status
	}{
		{
			name:                "A pod that has no required pod affinity scheduling rules can schedule onto a node with no existing pods",
			pod:                 new(v1.Pod),
			node:                &node1,
			wantPreFilterStatus: framework.NewStatus(framework.Skip),
		},
		{
			name: "satisfies with requiredDuringSchedulingIgnoredDuringExecution in PodAffinity using In operator that matches the existing pod",
			pod:  st.MakePod().Namespace(defaultNamespace).Labels(podLabel2).PodAffinityIn("service", "region", []string{"securityscan", "value2"}, st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{pod},
			node: &node1,
		},
		{
			name: "satisfies the pod with requiredDuringSchedulingIgnoredDuringExecution in PodAffinity using not in operator in labelSelector that matches the existing pod",
			pod:  st.MakePod().Namespace(defaultNamespace).Labels(podLabel2).PodAffinityNotIn("service", "region", []string{"securityscan3", "value3"}, st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{pod},
			node: &node1,
		},
		{
			name: "Does not satisfy the PodAffinity with labelSelector because of diff Namespace",
			pod: createPodWithAffinityTerms(defaultNamespace, "", podLabel2,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan", "value2"},
								},
							},
						},
						Namespaces: []string{"DiffNameSpace"},
					},
				}, nil),
			pods: []*v1.Pod{st.MakePod().Namespace("ns").Label("service", "securityscan").Node("node1").Obj()},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				ErrReasonAffinityRulesNotMatch,
			),
		},
		{
			name: "Doesn't satisfy the PodAffinity because of unmatching labelSelector with the existing pod",
			pod:  st.MakePod().Namespace(defaultNamespace).Labels(podLabel).PodAffinityIn("service", "", []string{"antivirusscan", "value2"}, st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{pod},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				ErrReasonAffinityRulesNotMatch,
			),
		},
		{
			name: "satisfies the PodAffinity with different label Operators in multiple RequiredDuringSchedulingIgnoredDuringExecution ",
			pod: createPodWithAffinityTerms(defaultNamespace, "", podLabel2,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpExists,
								}, {
									Key:      "wrongkey",
									Operator: metav1.LabelSelectorOpDoesNotExist,
								},
							},
						},
						TopologyKey: "region",
					}, {
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan"},
								}, {
									Key:      "service",
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"WrongValue"},
								},
							},
						},
						TopologyKey: "region",
					},
				}, nil),
			pods: []*v1.Pod{pod},
			node: &node1,
		},
		{
			name: "The labelSelector requirements(items of matchExpressions) are ANDed, the pod cannot schedule onto the node because one of the matchExpression item don't match.",
			pod: createPodWithAffinityTerms(defaultNamespace, "", podLabel2,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpExists,
								}, {
									Key:      "wrongkey",
									Operator: metav1.LabelSelectorOpDoesNotExist,
								},
							},
						},
						TopologyKey: "region",
					}, {
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan2"},
								}, {
									Key:      "service",
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"WrongValue"},
								},
							},
						},
						TopologyKey: "region",
					},
				}, nil),
			pods: []*v1.Pod{pod},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				ErrReasonAffinityRulesNotMatch,
			),
		},
		{
			name: "satisfies the PodAffinity and PodAntiAffinity with the existing pod",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel2).
				PodAffinityIn("service", "region", []string{"securityscan", "value2"}, st.PodAffinityWithRequiredReq).
				PodAntiAffinityIn("service", "node", []string{"antivirusscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{pod},
			node: &node1,
		},
		{
			name: "satisfies the PodAffinity and PodAntiAffinity and PodAntiAffinity symmetry with the existing pod",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel2).
				PodAffinityIn("service", "region", []string{"securityscan", "value2"}, st.PodAffinityWithRequiredReq).
				PodAntiAffinityIn("service", "node", []string{"antivirusscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(podLabel).
					PodAntiAffinityIn("service", "node", []string{"antivirusscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
		},
		{
			name: "satisfies the PodAffinity but doesn't satisfy the PodAntiAffinity with the existing pod",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel2).
				PodAffinityIn("service", "region", []string{"securityscan", "value2"}, st.PodAffinityWithRequiredReq).
				PodAntiAffinityIn("service", "zone", []string{"securityscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{pod},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "satisfies the PodAffinity and PodAntiAffinity but doesn't satisfy PodAntiAffinity symmetry with the existing pod",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel).
				PodAffinityIn("service", "region", []string{"securityscan", "value2"}, st.PodAffinityWithRequiredReq).
				PodAntiAffinityIn("service", "node", []string{"antivirusscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Labels(podLabel).Node("node1").PodAntiAffinityIn("service", "zone", []string{"securityscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonExistingAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "pod matches its own Label in PodAffinity and that matches the existing pod Labels",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel).
				PodAffinityNotIn("service", "region", []string{"securityscan", "value2"}, st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{st.MakePod().Label("service", "securityscan").Node("node2").Obj()},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				ErrReasonAffinityRulesNotMatch,
			),
		},
		{
			name: "verify that PodAntiAffinity from existing pod is respected when pod has no AntiAffinity constraints. doesn't satisfy PodAntiAffinity symmetry with the existing pod",
			pod:  st.MakePod().Labels(podLabel).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(podLabel).
					PodAntiAffinityIn("service", "zone", []string{"securityscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonExistingAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "verify that PodAntiAffinity from existing pod is respected when pod has no AntiAffinity constraints. satisfy PodAntiAffinity symmetry with the existing pod",
			pod:  st.MakePod().Labels(podLabel).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(podLabel).
					PodAntiAffinityNotIn("service", "zone", []string{"securityscan", "value2"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node:                &node1,
			wantPreFilterStatus: framework.NewStatus(framework.Skip),
		},
		{
			name: "satisfies the PodAntiAffinity with existing pod but doesn't satisfy PodAntiAffinity symmetry with incoming pod",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel).
				PodAntiAffinityExists("service", "region", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("security", "region", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(podLabel2).
					PodAntiAffinityExists("security", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "PodAntiAffinity symmetry check a1: incoming pod and existing pod partially match each other on AffinityTerms",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel).
				PodAntiAffinityExists("service", "zone", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("security", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(podLabel2).
					PodAntiAffinityExists("security", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "PodAntiAffinity symmetry check a2: incoming pod and existing pod partially match each other on AffinityTerms",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel2).
				PodAntiAffinityExists("security", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(podLabel).
					PodAntiAffinityExists("service", "zone", st.PodAntiAffinityWithRequiredReq).
					PodAntiAffinityExists("security", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonExistingAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "PodAntiAffinity symmetry check b1: incoming pod and existing pod partially match each other on AffinityTerms",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(map[string]string{"abc": "", "xyz": ""}).
				PodAntiAffinityExists("abc", "zone", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("def", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(map[string]string{"def": "", "xyz": ""}).
					PodAntiAffinityExists("abc", "zone", st.PodAntiAffinityWithRequiredReq).
					PodAntiAffinityExists("def", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "PodAntiAffinity symmetry check b2: incoming pod and existing pod partially match each other on AffinityTerms",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(map[string]string{"def": "", "xyz": ""}).
				PodAntiAffinityExists("abc", "zone", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("def", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("node1").Labels(map[string]string{"abc": "", "xyz": ""}).
					PodAntiAffinityExists("abc", "zone", st.PodAntiAffinityWithRequiredReq).
					PodAntiAffinityExists("def", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "PodAffinity fails PreFilter with an invalid affinity label syntax",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel).
				PodAffinityIn("service", "region", []string{"{{.bad-value.}}"}, st.PodAffinityWithRequiredReq).
				PodAffinityIn("service", "node", []string{"antivirusscan", "value2"}, st.PodAffinityWithRequiredReq).Obj(),
			node: &node1,
			wantPreFilterStatus: framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				`parsing pod: requiredAffinityTerms: values[0][service]: Invalid value: "{{.bad-value.}}": a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')`,
			),
		},
		{
			name: "PodAntiAffinity fails PreFilter with an invalid antiaffinity label syntax",
			pod: st.MakePod().Namespace(defaultNamespace).Labels(podLabel).
				PodAffinityIn("service", "region", []string{"foo"}, st.PodAffinityWithRequiredReq).
				PodAffinityIn("service", "node", []string{"{{.bad-value.}}"}, st.PodAffinityWithRequiredReq).Obj(),
			node: &node1,
			wantPreFilterStatus: framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				`parsing pod: requiredAffinityTerms: values[0][service]: Invalid value: "{{.bad-value.}}": a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')`,
			),
		},
		{
			name: "affinity with NamespaceSelector",
			pod: createPodWithAffinityTerms(defaultNamespace, "", podLabel2,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan", "value2"},
								},
							},
						},
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "team",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"team1"},
								},
							},
						},
						TopologyKey: "region",
					},
				}, nil),
			pods: []*v1.Pod{{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabel}}},
			node: &node1,
		},
		{
			name: "affinity with non-matching NamespaceSelector",
			pod: createPodWithAffinityTerms(defaultNamespace, "", podLabel2,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan", "value2"},
								},
							},
						},
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "team",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"team1"},
								},
							},
						},
						TopologyKey: "region",
					},
				}, nil),
			pods: []*v1.Pod{{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team2", Labels: podLabel}}},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				ErrReasonAffinityRulesNotMatch,
			),
		},
		{
			name: "anti-affinity with matching NamespaceSelector",
			pod: createPodWithAffinityTerms("subteam1.team1", "", podLabel2, nil,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan", "value2"},
								},
							},
						},
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "team",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"team1"},
								},
							},
						},
						TopologyKey: "zone",
					},
				}),
			pods: []*v1.Pod{{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team1", Labels: podLabel}}},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "anti-affinity with matching all NamespaceSelector",
			pod: createPodWithAffinityTerms("subteam1.team1", "", podLabel2, nil,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan", "value2"},
								},
							},
						},
						NamespaceSelector: &metav1.LabelSelector{},
						TopologyKey:       "zone",
					},
				}),
			pods: []*v1.Pod{{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team1", Labels: podLabel}}},
			node: &node1,
			wantFilterStatus: framework.NewStatus(
				framework.Unschedulable,
				ErrReasonAntiAffinityRulesNotMatch,
			),
		},
		{
			name: "anti-affinity with non-matching NamespaceSelector",
			pod: createPodWithAffinityTerms("subteam1.team1", "", podLabel2, nil,
				[]v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan", "value2"},
								},
							},
						},
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "team",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"team1"},
								},
							},
						},
						TopologyKey: "zone",
					},
				}),
			pods: []*v1.Pod{{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team2", Labels: podLabel}}},
			node: &node1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			snapshot := cache.NewSnapshot(test.pods, []*v1.Node{test.node})
			p := plugintesting.SetupPluginWithInformers(ctx, t, schedruntime.FactoryAdapter(feature.Features{}, New), &config.InterPodAffinityArgs{}, snapshot, namespaces)
			state := framework.NewCycleState()
			_, preFilterStatus := p.(framework.PreFilterPlugin).PreFilter(ctx, state, test.pod)
			if diff := cmp.Diff(test.wantPreFilterStatus, preFilterStatus); diff != "" {
				t.Errorf("PreFilter: status does not match (-want,+got):\n%s", diff)
			}
			if !preFilterStatus.IsSuccess() {
				return
			}

			nodeInfo := mustGetNodeInfo(t, snapshot, test.node.Name)
			gotStatus := p.(framework.FilterPlugin).Filter(ctx, state, test.pod, nodeInfo)
			if diff := cmp.Diff(test.wantFilterStatus, gotStatus); diff != "" {
				t.Errorf("Filter: status does not match (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestRequiredAffinityMultipleNodes(t *testing.T) {
	podLabelA := map[string]string{
		"foo": "bar",
	}
	labelRgChina := map[string]string{
		"region": "China",
	}
	labelRgChinaAzAz1 := map[string]string{
		"region": "China",
		"az":     "az1",
	}
	labelRgIndia := map[string]string{
		"region": "India",
	}

	tests := []struct {
		pod                 *v1.Pod
		pods                []*v1.Pod
		nodes               []*v1.Node
		wantFilterStatuses  []*framework.Status
		wantPreFilterStatus *framework.Status
		name                string
	}{
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAffinityIn("foo", "region", []string{"bar"}, st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Node("node1").Labels(podLabelA).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgChinaAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelRgIndia}},
			},
			wantFilterStatuses: []*framework.Status{
				nil,
				nil,
				framework.NewStatus(
					framework.UnschedulableAndUnresolvable,
					ErrReasonAffinityRulesNotMatch,
				),
			},
			name: "A pod can be scheduled onto all the nodes that have the same topology key & label value with one of them has an existing pod that matches the affinity rules",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).Labels(map[string]string{"foo": "bar", "service": "securityscan"}).
				PodAffinityIn("foo", "zone", []string{"bar"}, st.PodAffinityWithRequiredReq).
				PodAffinityIn("service", "zone", []string{"securityscan"}, st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Node("nodeA").Labels(map[string]string{"foo": "bar"}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"zone": "az1", "hostname": "h1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"zone": "az2", "hostname": "h2"}}},
			},
			wantFilterStatuses: []*framework.Status{nil, nil},
			name: "The affinity rule is to schedule all of the pods of this collection to the same zone. The first pod of the collection " +
				"should not be blocked from being scheduled onto any node, even there's no existing pod that matches the rule anywhere.",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).Labels(map[string]string{"foo": "bar", "service": "securityscan"}).
				PodAffinityIn("foo", "zone", []string{"bar"}, st.PodAffinityWithRequiredReq).
				PodAffinityIn("service", "zone", []string{"securityscan"}, st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Node("nodeA").Labels(map[string]string{"foo": "bar"}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"zoneLabel": "az1", "hostname": "h1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"zoneLabel": "az2", "hostname": "h2"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.UnschedulableAndUnresolvable,
					ErrReasonAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.UnschedulableAndUnresolvable,
					ErrReasonAffinityRulesNotMatch,
				),
			},
			name: "The first pod of the collection can only be scheduled on nodes labelled with the requested topology keys",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAntiAffinityIn("foo", "region", []string{"abc"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Labels(map[string]string{"foo": "abc"}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
			},
			name: "NodeA and nodeB have same topologyKey and label value. NodeA has an existing pod that matches the inter pod affinity rule. The pod can not be scheduled onto nodeA and nodeB.",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAntiAffinityIn("foo", "region", []string{"abc"}, st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityIn("service", "zone", []string{"securityscan"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Labels(map[string]string{"foo": "abc", "service": "securityscan"}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
			},
			name: "This test ensures that anti-affinity matches a pod when any term of the anti-affinity rule matches a pod.",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAntiAffinityIn("foo", "region", []string{"abc"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Labels(map[string]string{"foo": "abc"}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: labelRgChinaAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeC", Labels: labelRgIndia}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				nil,
			},
			name: "NodeA and nodeB have same topologyKey and label value. NodeA has an existing pod that matches the inter pod affinity rule. The pod can not be scheduled onto nodeA and nodeB but can be scheduled onto nodeC",
		},
		{
			pod: st.MakePod().Namespace("NS1").Labels(map[string]string{"foo": "123"}).PodAntiAffinityIn("foo", "region", []string{"bar"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Namespace("NS1").Labels(map[string]string{"foo": "bar"}).Obj(),
				st.MakePod().Node("nodeC").Namespace("NS2").PodAntiAffinityIn("foo", "region", []string{"123"}, st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: labelRgChinaAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeC", Labels: labelRgIndia}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				nil,
			},
			name: "NodeA and nodeB have same topologyKey and label value. NodeA has an existing pod that matches the inter pod affinity rule. The pod can not be scheduled onto nodeA, nodeB, but can be scheduled onto nodeC (NodeC has an existing pod that match the inter pod affinity rule but in different namespace)",
		},
		{
			pod: st.MakePod().Label("foo", "").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Namespace(defaultNamespace).PodAntiAffinityExists("foo", "invalid-node-label", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeB"}}},
			},
			wantPreFilterStatus: framework.NewStatus(framework.Skip),
			wantFilterStatuses:  []*framework.Status{nil, nil},
			name:                "Test existing pod's anti-affinity: if an existing pod has a term with invalid topologyKey, labelSelector of the term is firstly checked, and then topologyKey of the term is also checked",
		},
		{
			pod: st.MakePod().Node("nodeA").Namespace(defaultNamespace).PodAntiAffinityExists("foo", "invalid-node-label", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Labels(map[string]string{"foo": ""}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{nil, nil},
			name:               "Test incoming pod's anti-affinity: even if labelSelector matches, we still check if topologyKey matches",
		},
		{
			pod: st.MakePod().Label("foo", "").Label("bar", "").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Namespace(defaultNamespace).PodAntiAffinityExists("foo", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
				st.MakePod().Node("nodeA").Namespace(defaultNamespace).PodAntiAffinityExists("bar", "region", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonExistingAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonExistingAntiAffinityRulesNotMatch,
				),
			},
			name: "Test existing pod's anti-affinity: incoming pod wouldn't considered as a fit as it violates each existingPod's terms on all nodes",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAntiAffinityExists("foo", "zone", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("bar", "region", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Labels(map[string]string{"foo": ""}).Obj(),
				st.MakePod().Node("nodeB").Labels(map[string]string{"bar": ""}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
			},
			name: "Test incoming pod's anti-affinity: incoming pod wouldn't considered as a fit as it at least violates one anti-affinity rule of existingPod",
		},
		{
			pod: st.MakePod().Label("foo", "").Label("bar", "").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Namespace(defaultNamespace).PodAntiAffinityExists("foo", "invalid-node-label", st.PodAntiAffinityWithRequiredReq).
					PodAntiAffinityExists("bar", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonExistingAntiAffinityRulesNotMatch,
				),
				nil,
			},
			name: "Test existing pod's anti-affinity: only when labelSelector and topologyKey both match, it's counted as a single term match - case when one term has invalid topologyKey",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAntiAffinityExists("foo", "invalid-node-label", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("bar", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("podA").Node("nodeA").Labels(map[string]string{"foo": "", "bar": ""}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				nil,
			},
			name: "Test incoming pod's anti-affinity: only when labelSelector and topologyKey both match, it's counted as a single term match - case when one term has invalid topologyKey",
		},
		{
			pod: st.MakePod().Label("foo", "").Label("bar", "").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Namespace(defaultNamespace).Node("nodeA").PodAntiAffinityExists("foo", "region", st.PodAntiAffinityWithRequiredReq).
					PodAntiAffinityExists("bar", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonExistingAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonExistingAntiAffinityRulesNotMatch,
				),
			},
			name: "Test existing pod's anti-affinity: only when labelSelector and topologyKey both match, it's counted as a single term match - case when all terms have valid topologyKey",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAntiAffinityExists("foo", "region", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("bar", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Labels(map[string]string{"foo": "", "bar": ""}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonAntiAffinityRulesNotMatch,
				),
			},
			name: "Test incoming pod's anti-affinity: only when labelSelector and topologyKey both match, it's counted as a single term match - case when all terms have valid topologyKey",
		},
		{
			pod: st.MakePod().Label("foo", "").Label("bar", "").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Namespace(defaultNamespace).PodAntiAffinityExists("foo", "zone", st.PodAntiAffinityWithRequiredReq).
					PodAntiAffinityExists("labelA", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
				st.MakePod().Node("nodeB").Namespace(defaultNamespace).PodAntiAffinityExists("bar", "zone", st.PodAntiAffinityWithRequiredReq).
					PodAntiAffinityExists("labelB", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeC", Labels: map[string]string{"region": "r1", "zone": "z3", "hostname": "nodeC"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonExistingAntiAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.Unschedulable,
					ErrReasonExistingAntiAffinityRulesNotMatch,
				),
				nil,
			},
			name: "Test existing pod's anti-affinity: existingPod on nodeA and nodeB has at least one anti-affinity term matches incoming pod, so incoming pod can only be scheduled to nodeC",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAffinityExists("foo", "region", st.PodAffinityWithRequiredReq).
				PodAffinityExists("bar", "zone", st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pod1").Labels(map[string]string{"foo": "", "bar": ""}).Node("nodeA").Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{nil, nil},
			name:               "Test incoming pod's affinity: firstly check if all affinityTerms match, and then check if all topologyKeys match",
		},
		{
			pod: st.MakePod().Namespace(defaultNamespace).PodAffinityExists("foo", "region", st.PodAffinityWithRequiredReq).
				PodAffinityExists("bar", "zone", st.PodAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Node("nodeA").Name("pod1").Namespace(defaultNamespace).Labels(map[string]string{"foo": ""}).Obj(),
				st.MakePod().Node("nodeB").Name("pod2").Namespace(defaultNamespace).Labels(map[string]string{"bar": ""}).Obj(),
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"region": "r1", "zone": "z1", "hostname": "nodeA"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: map[string]string{"region": "r1", "zone": "z2", "hostname": "nodeB"}}},
			},
			wantFilterStatuses: []*framework.Status{
				framework.NewStatus(
					framework.UnschedulableAndUnresolvable,
					ErrReasonAffinityRulesNotMatch,
				),
				framework.NewStatus(
					framework.UnschedulableAndUnresolvable,
					ErrReasonAffinityRulesNotMatch,
				),
			},
			name: "Test incoming pod's affinity: firstly check if all affinityTerms match, and then check if all topologyKeys match, and the match logic should be satisfied on the same pod",
		},
	}

	for indexTest, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			snapshot := cache.NewSnapshot(test.pods, test.nodes)
			p := plugintesting.SetupPluginWithInformers(ctx, t, schedruntime.FactoryAdapter(feature.Features{}, New), &config.InterPodAffinityArgs{}, snapshot,
				[]runtime.Object{
					&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "NS1"}},
				})
			state := framework.NewCycleState()
			_, preFilterStatus := p.(framework.PreFilterPlugin).PreFilter(ctx, state, test.pod)
			if diff := cmp.Diff(test.wantPreFilterStatus, preFilterStatus); diff != "" {
				t.Errorf("PreFilter: status does not match (-want,+got):\n%s", diff)
			}
			if preFilterStatus.IsSkip() {
				return
			}
			for indexNode, node := range test.nodes {
				nodeInfo := mustGetNodeInfo(t, snapshot, node.Name)
				gotStatus := p.(framework.FilterPlugin).Filter(ctx, state, test.pod, nodeInfo)
				if diff := cmp.Diff(test.wantFilterStatuses[indexNode], gotStatus); diff != "" {
					t.Errorf("index: %d: Filter: status does not match (-want,+got):\n%s", indexTest, diff)
				}
			}
		})
	}
}

func TestPreFilterDisabled(t *testing.T) {
	pod := &v1.Pod{}
	nodeInfo := framework.NewNodeInfo()
	node := v1.Node{}
	nodeInfo.SetNode(&node)
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := plugintesting.SetupPluginWithInformers(ctx, t, schedruntime.FactoryAdapter(feature.Features{}, New), &config.InterPodAffinityArgs{}, cache.NewEmptySnapshot(), nil)
	cycleState := framework.NewCycleState()
	gotStatus := p.(framework.FilterPlugin).Filter(ctx, cycleState, pod, nodeInfo)
	wantStatus := framework.AsStatus(framework.ErrNotFound)
	if diff := cmp.Diff(gotStatus, wantStatus); diff != "" {
		t.Errorf("Status does not match (-want,+got):\n%s", diff)
	}
}

func TestPreFilterStateAddRemovePod(t *testing.T) {
	var label1 = map[string]string{
		"region": "r1",
		"zone":   "z11",
	}
	var label2 = map[string]string{
		"region": "r1",
		"zone":   "z12",
	}
	var label3 = map[string]string{
		"region": "r2",
		"zone":   "z21",
	}
	selector1 := map[string]string{"foo": "bar"}
	antiAffinityFooBar := &v1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "foo",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"bar"},
						},
					},
				},
				TopologyKey: "region",
			},
		},
	}
	antiAffinityComplex := &v1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "foo",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"bar", "buzz"},
						},
					},
				},
				TopologyKey: "region",
			},
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "service",
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{"bar", "security", "test"},
						},
					},
				},
				TopologyKey: "zone",
			},
		},
	}
	affinityComplex := &v1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "foo",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"bar", "buzz"},
						},
					},
				},
				TopologyKey: "region",
			},
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "service",
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{"bar", "security", "test"},
						},
					},
				},
				TopologyKey: "zone",
			},
		},
	}

	tests := []struct {
		name                 string
		pendingPod           *v1.Pod
		addedPod             *v1.Pod
		existingPods         []*v1.Pod
		nodes                []*v1.Node
		expectedAntiAffinity topologyToMatchedTermCount
		expectedAffinity     topologyToMatchedTermCount
	}{
		{
			name: "preFilterState anti-affinity terms are updated correctly after adding and removing a pod",
			pendingPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pending", Labels: selector1},
				Spec: v1.PodSpec{
					Affinity: &v1.Affinity{
						PodAntiAffinity: antiAffinityFooBar,
					},
				},
			},
			existingPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: selector1},
					Spec: v1.PodSpec{NodeName: "nodeA"},
				},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"},
					Spec: v1.PodSpec{
						NodeName: "nodeC",
						Affinity: &v1.Affinity{
							PodAntiAffinity: antiAffinityFooBar,
						},
					},
				},
			},
			addedPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "addedPod", Labels: selector1},
				Spec: v1.PodSpec{
					NodeName: "nodeB",
					Affinity: &v1.Affinity{
						PodAntiAffinity: antiAffinityFooBar,
					},
				},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: label1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: label2}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeC", Labels: label3}},
			},
			expectedAntiAffinity: topologyToMatchedTermCount{
				{key: "region", value: "r1"}: 2,
			},
			expectedAffinity: topologyToMatchedTermCount{},
		},
		{
			name: "preFilterState anti-affinity terms are updated correctly after adding and removing a pod",
			pendingPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pending", Labels: selector1},
				Spec: v1.PodSpec{
					Affinity: &v1.Affinity{
						PodAntiAffinity: antiAffinityComplex,
					},
				},
			},
			existingPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: selector1},
					Spec: v1.PodSpec{NodeName: "nodeA"},
				},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"},
					Spec: v1.PodSpec{
						NodeName: "nodeC",
						Affinity: &v1.Affinity{
							PodAntiAffinity: antiAffinityFooBar,
						},
					},
				},
			},
			addedPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "addedPod", Labels: selector1},
				Spec: v1.PodSpec{
					NodeName: "nodeA",
					Affinity: &v1.Affinity{
						PodAntiAffinity: antiAffinityComplex,
					},
				},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: label1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: label2}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeC", Labels: label3}},
			},
			expectedAntiAffinity: topologyToMatchedTermCount{
				{key: "region", value: "r1"}: 2,
				{key: "zone", value: "z11"}:  2,
				{key: "zone", value: "z21"}:  1,
			},
			expectedAffinity: topologyToMatchedTermCount{},
		},
		{
			name: "preFilterState matching pod affinity and anti-affinity are updated correctly after adding and removing a pod",
			pendingPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pending", Labels: selector1},
				Spec: v1.PodSpec{
					Affinity: &v1.Affinity{
						PodAffinity: affinityComplex,
					},
				},
			},
			existingPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: selector1},
					Spec: v1.PodSpec{NodeName: "nodeA"},
				},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"},
					Spec: v1.PodSpec{
						NodeName: "nodeC",
						Affinity: &v1.Affinity{
							PodAntiAffinity: antiAffinityFooBar,
							PodAffinity:     affinityComplex,
						},
					},
				},
			},
			addedPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "addedPod", Labels: selector1},
				Spec: v1.PodSpec{
					NodeName: "nodeA",
					Affinity: &v1.Affinity{
						PodAntiAffinity: antiAffinityComplex,
					},
				},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: label1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeB", Labels: label2}},
				{ObjectMeta: metav1.ObjectMeta{Name: "nodeC", Labels: label3}},
			},
			expectedAntiAffinity: topologyToMatchedTermCount{},
			expectedAffinity: topologyToMatchedTermCount{
				{key: "region", value: "r1"}: 2,
				{key: "zone", value: "z11"}:  2,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// getMeta creates predicate meta data given the list of pods.
			getState := func(pods []*v1.Pod) (*InterPodAffinity, *framework.CycleState, *preFilterState, *cache.Snapshot) {
				snapshot := cache.NewSnapshot(pods, test.nodes)
				_, ctx := ktesting.NewTestContext(t)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				p := plugintesting.SetupPluginWithInformers(ctx, t, schedruntime.FactoryAdapter(feature.Features{}, New), &config.InterPodAffinityArgs{}, snapshot, nil)
				cycleState := framework.NewCycleState()
				_, preFilterStatus := p.(framework.PreFilterPlugin).PreFilter(ctx, cycleState, test.pendingPod)
				if !preFilterStatus.IsSuccess() {
					t.Errorf("prefilter failed with status: %v", preFilterStatus)
				}

				state, err := getPreFilterState(cycleState)
				if err != nil {
					t.Errorf("failed to get preFilterState from cycleState: %v", err)
				}

				return p.(*InterPodAffinity), cycleState, state, snapshot
			}

			_, ctx := ktesting.NewTestContext(t)
			// allPodsState is the state produced when all pods, including test.addedPod are given to prefilter.
			_, _, allPodsState, _ := getState(append(test.existingPods, test.addedPod))

			// state is produced for test.existingPods (without test.addedPod).
			ipa, cycleState, state, snapshot := getState(test.existingPods)
			// clone the state so that we can compare it later when performing Remove.
			originalState := state.Clone()

			// Add test.addedPod to state1 and verify it is equal to allPodsState.
			nodeInfo := mustGetNodeInfo(t, snapshot, test.addedPod.Spec.NodeName)
			if err := ipa.AddPod(ctx, cycleState, test.pendingPod, mustNewPodInfo(t, test.addedPod), nodeInfo); err != nil {
				t.Errorf("error adding pod to meta: %v", err)
			}

			newState, err := getPreFilterState(cycleState)
			if err != nil {
				t.Errorf("failed to get preFilterState from cycleState: %v", err)
			}

			if diff := cmp.Diff(test.expectedAntiAffinity, newState.antiAffinityCounts); diff != "" {
				t.Errorf("State is not equal (-want,+got):\n%s", diff)
			}

			if diff := cmp.Diff(test.expectedAffinity, newState.affinityCounts); diff != "" {
				t.Errorf("State is not equal (-want,+got):\n%s", diff)
			}

			if diff := cmp.Diff(allPodsState, state, preFilterStateCmpOpts...); diff != "" {
				t.Errorf("State is not equal (-want,+got):\n%s", diff)
			}

			// Remove the added pod pod and make sure it is equal to the original state.
			if err := ipa.RemovePod(ctx, cycleState, test.pendingPod, mustNewPodInfo(t, test.addedPod), nodeInfo); err != nil {
				t.Errorf("error removing pod from meta: %v", err)
			}
			if diff := cmp.Diff(originalState, state, preFilterStateCmpOpts...); diff != "" {
				t.Errorf("State is not equal (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreFilterStateClone(t *testing.T) {
	source := &preFilterState{
		existingAntiAffinityCounts: topologyToMatchedTermCount{
			{key: "name", value: "node1"}: 1,
			{key: "name", value: "node2"}: 1,
		},
		affinityCounts: topologyToMatchedTermCount{
			{key: "name", value: "nodeA"}: 1,
			{key: "name", value: "nodeC"}: 2,
		},
		antiAffinityCounts: topologyToMatchedTermCount{
			{key: "name", value: "nodeN"}: 3,
			{key: "name", value: "nodeM"}: 1,
		},
	}

	clone := source.Clone()
	if clone == source {
		t.Errorf("Clone returned the exact same object!")
	}
	if diff := cmp.Diff(source, clone, preFilterStateCmpOpts...); diff != "" {
		t.Errorf("Copy is not equal to source! (-want,+got):\n%s", diff)
	}
}

// TestGetTPMapMatchingIncomingAffinityAntiAffinity tests against method getTPMapMatchingIncomingAffinityAntiAffinity
// on Anti Affinity cases
func TestGetTPMapMatchingIncomingAffinityAntiAffinity(t *testing.T) {
	newPod := func(labels ...string) *v1.Pod {
		pod := st.MakePod().Name("normal").Node("nodeA")
		for _, l := range labels {
			pod.Label(l, "")
		}
		return pod.Obj()
	}
	normalPodA := newPod("aaa")
	normalPodB := newPod("bbb")
	normalPodAB := newPod("aaa", "bbb")
	nodeA := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "nodeA", Labels: map[string]string{"hostname": "nodeA"}}}

	tests := []struct {
		name                    string
		existingPods            []*v1.Pod
		nodes                   []*v1.Node
		pod                     *v1.Pod
		wantAffinityPodsMap     topologyToMatchedTermCount
		wantAntiAffinityPodsMap topologyToMatchedTermCount
	}{
		{
			name:                    "nil test",
			nodes:                   []*v1.Node{nodeA},
			pod:                     st.MakePod().Name("aaa-normal").Obj(),
			wantAffinityPodsMap:     make(topologyToMatchedTermCount),
			wantAntiAffinityPodsMap: make(topologyToMatchedTermCount),
		},
		{
			name:                    "incoming pod without affinity/anti-affinity causes a no-op",
			existingPods:            []*v1.Pod{normalPodA},
			nodes:                   []*v1.Node{nodeA},
			pod:                     st.MakePod().Name("aaa-normal").Obj(),
			wantAffinityPodsMap:     make(topologyToMatchedTermCount),
			wantAntiAffinityPodsMap: make(topologyToMatchedTermCount),
		},
		{
			name:         "no pod has label that violates incoming pod's affinity and anti-affinity",
			existingPods: []*v1.Pod{normalPodB},
			nodes:        []*v1.Node{nodeA},
			pod: st.MakePod().Name("aaa-anti").PodAffinityExists("aaa", "hostname", st.PodAffinityWithRequiredReq).
				PodAntiAffinityExists("aaa", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
			wantAffinityPodsMap:     make(topologyToMatchedTermCount),
			wantAntiAffinityPodsMap: make(topologyToMatchedTermCount),
		},
		{
			name:         "existing pod matches incoming pod's affinity and anti-affinity - single term case",
			existingPods: []*v1.Pod{normalPodA},
			nodes:        []*v1.Node{nodeA},
			pod: st.MakePod().Name("affi-antiaffi").PodAffinityExists("aaa", "hostname", st.PodAffinityWithRequiredReq).
				PodAntiAffinityExists("aaa", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
			wantAffinityPodsMap: topologyToMatchedTermCount{
				{key: "hostname", value: "nodeA"}: 1,
			},
			wantAntiAffinityPodsMap: topologyToMatchedTermCount{
				{key: "hostname", value: "nodeA"}: 1,
			},
		},
		{
			name:         "existing pod matches incoming pod's affinity and anti-affinity - multiple terms case",
			existingPods: []*v1.Pod{normalPodAB},
			nodes:        []*v1.Node{nodeA},
			pod: st.MakePod().Name("affi-antiaffi").PodAffinityExists("aaa", "hostname", st.PodAffinityWithRequiredReq).
				PodAffinityExists("bbb", "hostname", st.PodAffinityWithRequiredReq).PodAntiAffinityExists("aaa", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
			wantAffinityPodsMap: topologyToMatchedTermCount{
				{key: "hostname", value: "nodeA"}: 2, // 2 one for each term.
			},
			wantAntiAffinityPodsMap: topologyToMatchedTermCount{
				{key: "hostname", value: "nodeA"}: 1,
			},
		},
		{
			name:         "existing pod not match incoming pod's affinity but matches anti-affinity",
			existingPods: []*v1.Pod{normalPodA},
			nodes:        []*v1.Node{nodeA},
			pod: st.MakePod().Name("affi-antiaffi").PodAffinityExists("aaa", "hostname", st.PodAffinityWithRequiredReq).
				PodAffinityExists("bbb", "hostname", st.PodAffinityWithRequiredReq).
				PodAntiAffinityExists("aaa", "hostname", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("bbb", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
			wantAffinityPodsMap: make(topologyToMatchedTermCount),
			wantAntiAffinityPodsMap: topologyToMatchedTermCount{
				{key: "hostname", value: "nodeA"}: 1,
			},
		},
		{
			name:         "incoming pod's anti-affinity has more than one term - existing pod violates partial term - case 1",
			existingPods: []*v1.Pod{normalPodAB},
			nodes:        []*v1.Node{nodeA},
			pod: st.MakePod().Name("anaffi-antiaffiti").PodAffinityExists("aaa", "hostname", st.PodAffinityWithRequiredReq).
				PodAffinityExists("ccc", "hostname", st.PodAffinityWithRequiredReq).
				PodAntiAffinityExists("aaa", "hostname", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("ccc", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
			wantAffinityPodsMap: make(topologyToMatchedTermCount),
			wantAntiAffinityPodsMap: topologyToMatchedTermCount{
				{key: "hostname", value: "nodeA"}: 1,
			},
		},
		{
			name:         "incoming pod's anti-affinity has more than one term - existing pod violates partial term - case 2",
			existingPods: []*v1.Pod{normalPodB},
			nodes:        []*v1.Node{nodeA},
			pod: st.MakePod().Name("affi-antiaffi").PodAffinityExists("aaa", "hostname", st.PodAffinityWithRequiredReq).
				PodAffinityExists("bbb", "hostname", st.PodAffinityWithRequiredReq).
				PodAntiAffinityExists("aaa", "hostname", st.PodAntiAffinityWithRequiredReq).
				PodAntiAffinityExists("bbb", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
			wantAffinityPodsMap: make(topologyToMatchedTermCount),
			wantAntiAffinityPodsMap: topologyToMatchedTermCount{
				{key: "hostname", value: "nodeA"}: 1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := cache.NewSnapshot(tt.existingPods, tt.nodes)
			l, _ := snapshot.NodeInfos().List()
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			p := plugintesting.SetupPluginWithInformers(ctx, t, schedruntime.FactoryAdapter(feature.Features{}, New), &config.InterPodAffinityArgs{}, snapshot, nil)
			gotAffinityPodsMap, gotAntiAffinityPodsMap := p.(*InterPodAffinity).getIncomingAffinityAntiAffinityCounts(ctx, mustNewPodInfo(t, tt.pod), l)
			if diff := cmp.Diff(tt.wantAffinityPodsMap, gotAffinityPodsMap); diff != "" {
				t.Errorf("Unexpected getTPMapMatchingIncomingAffinityAntiAffinity() (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantAntiAffinityPodsMap, gotAntiAffinityPodsMap); diff != "" {
				t.Errorf("Unexpected getTPMapMatchingIncomingAffinityAntiAffinity() (-want,+got):\n%s", diff)
			}
		})
	}
}

func mustGetNodeInfo(t *testing.T, snapshot *cache.Snapshot, name string) *framework.NodeInfo {
	t.Helper()
	nodeInfo, err := snapshot.NodeInfos().Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return nodeInfo
}

func mustNewPodInfo(t *testing.T, pod *v1.Pod) *framework.PodInfo {
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		t.Fatal(err)
	}
	return podInfo
}
