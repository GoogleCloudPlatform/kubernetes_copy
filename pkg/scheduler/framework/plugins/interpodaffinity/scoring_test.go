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
	"strings"
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
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
)

var nsLabelT1 = map[string]string{"team": "team1"}
var nsLabelT2 = map[string]string{"team": "team2"}
var namespaces = []runtime.Object{
	&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "subteam1.team1", Labels: nsLabelT1}},
	&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "subteam2.team1", Labels: nsLabelT1}},
	&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "subteam1.team2", Labels: nsLabelT2}},
	&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "subteam2.team2", Labels: nsLabelT2}},
}

func TestPreferredAffinity(t *testing.T) {
	labelRgChina := map[string]string{
		"region": "China",
	}
	labelRgIndia := map[string]string{
		"region": "India",
	}
	labelAzAz1 := map[string]string{
		"az": "az1",
	}
	labelAzAz2 := map[string]string{
		"az": "az2",
	}
	labelRgChinaAzAz1 := map[string]string{
		"region": "China",
		"az":     "az1",
	}
	podLabelSecurityS1 := map[string]string{
		"security": "S1",
	}
	podLabelSecurityS2 := map[string]string{
		"security": "S2",
	}
	// considered only preferredDuringSchedulingIgnoredDuringExecution in pod affinity
	stayWithS1InRegion := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 5,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S1"},
								},
							},
						},
						TopologyKey: "region",
					},
				},
			},
		},
	}
	stayWithS2InRegion := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 6,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S2"},
								},
							},
						},
						TopologyKey: "region",
					},
				},
			},
		},
	}
	affinity3 := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 8,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpNotIn,
									Values:   []string{"S1"},
								}, {
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S2"},
								},
							},
						},
						TopologyKey: "region",
					},
				}, {
					Weight: 2,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpExists,
								}, {
									Key:      "wrongkey",
									Operator: metav1.LabelSelectorOpDoesNotExist,
								},
							},
						},
						TopologyKey: "region",
					},
				},
			},
		},
	}
	hardAffinity := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "security",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"S1", "value2"},
							},
						},
					},
					TopologyKey: "region",
				}, {
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "security",
								Operator: metav1.LabelSelectorOpExists,
							}, {
								Key:      "wrongkey",
								Operator: metav1.LabelSelectorOpDoesNotExist,
							},
						},
					},
					TopologyKey: "region",
				},
			},
		},
	}
	awayFromS1InAz := &v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 5,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S1"},
								},
							},
						},
						TopologyKey: "az",
					},
				},
			},
		},
	}
	// to stay away from security S2 in any az.
	awayFromS2InAz := &v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 5,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S2"},
								},
							},
						},
						TopologyKey: "az",
					},
				},
			},
		},
	}
	// to stay with security S1 in same region, stay away from security S2 in any az.
	stayWithS1InRegionAwayFromS2InAz := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 8,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S1"},
								},
							},
						},
						TopologyKey: "region",
					},
				},
			},
		},
		PodAntiAffinity: &v1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 5,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S2"},
								},
							},
						},
						TopologyKey: "az",
					},
				},
			},
		},
	}

	affinityNamespaceSelector := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 5,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S1"},
								},
							},
						},
						TopologyKey: "region",
						Namespaces:  []string{"subteam2.team2"},
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "team",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"team1"},
								},
							},
						},
					},
				},
			},
		},
	}
	antiAffinityNamespaceSelector := &v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 5,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"S1"},
								},
							},
						},
						TopologyKey: "region",
						Namespaces:  []string{"subteam2.team2"},
						NamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "team",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"team1"},
								},
							},
						},
					},
				},
			},
		},
	}
	invalidAffinityLabels := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 8,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"{{.bad-value.}}"},
								},
							},
						},
						TopologyKey: "region",
					},
				},
			},
		},
	}
	invalidAntiAffinityLabels := &v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 5,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "security",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"{{.bad-value.}}"},
								},
							},
						},
						TopologyKey: "az",
					},
				},
			},
		},
	}

	tests := []struct {
		pod                                *v1.Pod
		pods                               []*v1.Pod
		nodes                              []*v1.Node
		expectedList                       framework.NodeScoreList
		name                               string
		ignorePreferredTermsOfExistingPods bool
		wantStatus                         *framework.Status
	}{
		{
			name: "all nodes are same priority as Affinity is nil",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			wantStatus: framework.NewStatus(framework.Skip),
		},
		// the node(node1) that have the label {"region": "China"} (match the topology key) and that have existing pods that match the labelSelector get high score
		// the node(node3) that don't have the label {"region": "whatever the value is"} (mismatch the topology key) but that have existing pods that match the labelSelector get low score
		// the node(node2) that have the label {"region": "China"} (match the topology key) but that have existing pods that mismatch the labelSelector get low score
		{
			name: "Affinity: pod that matches topology key & pods in nodes will get high score comparing to others" +
				"which doesn't match either pods in nodes or in topology key",
			pod: &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: stayWithS1InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node3"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}, {Name: "node3", Score: 0}},
		},
		// the node1(node1) that have the label {"region": "China"} (match the topology key) and that have existing pods that match the labelSelector get high score
		// the node2(node2) that have the label {"region": "China"}, match the topology key and have the same label value with node1, get the same high score with node1
		// the node3(node3) that have the label {"region": "India"}, match the topology key but have a different label value, don't have existing pods that match the labelSelector,
		// get a low score.
		{
			name: "All the nodes that have the same topology key & label value with one of them has an existing pod that match the affinity rules, have the same score",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: stayWithS1InRegion}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgChinaAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: 0}},
		},
		// there are 2 regions, say regionChina(node1,node3,node4) and regionIndia(node2,node5), both regions have nodes that match the preference.
		// But there are more nodes(actually more existing pods) in regionChina that match the preference than regionIndia.
		// Then, nodes in regionChina get higher score than nodes in regionIndia, and all the nodes in regionChina should get a same score(high score),
		// while all the nodes in regionIndia should get another same score(low score).
		{
			name: "Affinity: nodes in one region has more matching pods comparing to other region, so the region which has more matches will get high score",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: stayWithS2InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node3"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node4"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node5"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node4", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node5", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}, {Name: "node3", Score: framework.MaxNodeScore}, {Name: "node4", Score: framework.MaxNodeScore}, {Name: "node5", Score: 0}},
		},
		// Test with the different operators and values for pod affinity scheduling preference, including some match failures.
		{
			name: "Affinity: different Label operators and values for pod affinity scheduling preference, including some match failures ",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: affinity3}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node3"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 20}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: 0}},
		},
		// Test the symmetry cases for affinity, the difference between affinity and symmetry is not the pod wants to run together with some existing pods,
		// but the existing pods have the inter pod affinity preference while the pod to schedule satisfy the preference.
		{
			name: "Affinity symmetry: considered only the preferredDuringSchedulingIgnoredDuringExecution in pod affinity symmetry",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: stayWithS1InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: stayWithS2InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: 0}},
		},
		{
			name: "Affinity symmetry with namespace selector",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: affinityNamespaceSelector}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: stayWithS2InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}, {Name: "node3", Score: 0}},
		},
		{
			name: "AntiAffinity symmetry with namespace selector",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: antiAffinityNamespaceSelector}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: stayWithS2InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: framework.MaxNodeScore}},
		},
		{
			name: "Affinity symmetry: considered RequiredDuringSchedulingIgnoredDuringExecution in pod affinity symmetry",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: hardAffinity}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: hardAffinity}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: 0}},
		},

		// The pod to schedule prefer to stay away from some existing pods at node level using the pod anti affinity.
		// the nodes that have the label {"node": "bar"} (match the topology key) and that have existing pods that match the labelSelector get low score
		// the nodes that don't have the label {"node": "whatever the value is"} (mismatch the topology key) but that have existing pods that match the labelSelector get high score
		// the nodes that have the label {"node": "bar"} (match the topology key) but that have existing pods that mismatch the labelSelector get high score
		// there are 2 nodes, say node1 and node2, both nodes have pods that match the labelSelector and have topology-key in node.Labels.
		// But there are more pods on node1 that match the preference than node2. Then, node1 get a lower score than node2.
		{
			name: "Anti Affinity: pod that does not match existing pods in node will get high score ",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: awayFromS1InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgChina}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}},
		},
		{
			name: "Anti Affinity: pod that does not match topology key & match the pods in nodes will get higher score comparing to others ",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: awayFromS1InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgChina}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}},
		},
		{
			name: "Anti Affinity: one node has more matching pods comparing to other node, so the node which has more unmatches will get high score",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: awayFromS1InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}},
		},
		// Test the symmetry cases for anti affinity
		{
			name: "Anti Affinity symmetry: the existing pods in node which has anti affinity match will get high score",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: awayFromS2InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: awayFromS1InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelAzAz2}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}},
		},
		// Test both  affinity and anti-affinity
		{
			name: "Affinity and Anti Affinity: considered only preferredDuringSchedulingIgnoredDuringExecution in both pod affinity & anti affinity",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: stayWithS1InRegionAwayFromS2InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelAzAz1}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}},
		},
		// Combined cases considering both affinity and anti-affinity, the pod to schedule and existing pods have the same labels (they are in the same RC/service),
		// the pod prefer to run together with its brother pods in the same region, but wants to stay away from them at node level,
		// so that all the pods of a RC/service can stay in a same region but trying to separate with each other
		// node-1,node-3,node-4 are in ChinaRegion others node-2,node-5 are in IndiaRegion
		{
			name: "Affinity and Anti Affinity: considering both affinity and anti-affinity, the pod to schedule and existing pods have the same labels",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: stayWithS1InRegionAwayFromS2InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node3"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node3"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node4"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node5"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChinaAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node4", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node5", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}, {Name: "node3", Score: framework.MaxNodeScore}, {Name: "node4", Score: framework.MaxNodeScore}, {Name: "node5", Score: 0}},
		},
		// Consider Affinity, Anti Affinity and symmetry together.
		// for Affinity, the weights are:                8,  0,  0,  0
		// for Anti Affinity, the weights are:           0, -5,  0,  0
		// for Affinity symmetry, the weights are:       0,  0,  8,  0
		// for Anti Affinity symmetry, the weights are:  0,  0,  0, -5
		{
			name: "Affinity and Anti Affinity and symmetry: considered only preferredDuringSchedulingIgnoredDuringExecution in both pod affinity & anti affinity & symmetry",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: stayWithS1InRegionAwayFromS2InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
				{Spec: v1.PodSpec{NodeName: "node3", Affinity: stayWithS1InRegionAwayFromS2InAz}},
				{Spec: v1.PodSpec{NodeName: "node4", Affinity: awayFromS1InAz}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelAzAz1}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node4", Labels: labelAzAz2}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}, {Name: "node3", Score: framework.MaxNodeScore}, {Name: "node4", Score: 0}},
		},
		// Cover https://github.com/kubernetes/kubernetes/issues/82796 which panics upon:
		// 1. Some nodes in a topology don't have pods with affinity, but other nodes in the same topology have.
		// 2. The incoming pod doesn't have affinity.
		{
			name: "Avoid panic when partial nodes in a topology don't have pods with affinity",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: stayWithS1InRegionAwayFromS2InAz}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgChina}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: 0}},
		},
		{
			name:       "invalid Affinity fails PreScore",
			pod:        &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: invalidAffinityLabels}},
			wantStatus: framework.NewStatus(framework.Error, `Invalid value: "{{.bad-value.}}"`),
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgChina}},
			},
		},
		{
			name:       "invalid AntiAffinity fails PreScore",
			pod:        &v1.Pod{Spec: v1.PodSpec{NodeName: "", Affinity: invalidAntiAffinityLabels}},
			wantStatus: framework.NewStatus(framework.Error, `Invalid value: "{{.bad-value.}}"`),
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgChina}},
			},
		},
		{
			name: "Affinity with pods matching NamespaceSelector",
			pod:  &v1.Pod{Spec: v1.PodSpec{Affinity: affinityNamespaceSelector}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team2", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team1", Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}},
		},
		{
			name: "Affinity with pods matching both NamespaceSelector and Namespaces fields",
			pod:  &v1.Pod{Spec: v1.PodSpec{Affinity: affinityNamespaceSelector}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team2", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team1", Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 0}},
		},
		{
			name: "Affinity with pods matching NamespaceSelector",
			pod:  &v1.Pod{Spec: v1.PodSpec{Affinity: antiAffinityNamespaceSelector}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team2", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team1", Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}},
		},
		{
			name: "Affinity with pods matching both NamespaceSelector and Namespaces fields",
			pod:  &v1.Pod{Spec: v1.PodSpec{Affinity: antiAffinityNamespaceSelector}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node1"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team2", Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2"}, ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team1", Labels: podLabelSecurityS1}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
			},
			expectedList: []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}},
		},
		{
			name: "Ignore preferred terms of existing pods",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: stayWithS1InRegionAwayFromS2InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: stayWithS2InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
			},
			expectedList:                       []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: 0}},
			wantStatus:                         framework.NewStatus(framework.Skip),
			ignorePreferredTermsOfExistingPods: true,
		},
		{
			name: "Do not ignore preferred terms of existing pods",
			pod:  &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: stayWithS1InRegionAwayFromS2InAz}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS1}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: stayWithS2InRegion}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
			},
			expectedList:                       []framework.NodeScore{{Name: "node1", Score: 0}, {Name: "node2", Score: framework.MaxNodeScore}},
			ignorePreferredTermsOfExistingPods: false,
		},
		{
			name:       "No nodes to score",
			pod:        &v1.Pod{Spec: v1.PodSpec{NodeName: ""}, ObjectMeta: metav1.ObjectMeta{Labels: podLabelSecurityS2}},
			pods:       []*v1.Pod{},
			nodes:      []*v1.Node{},
			wantStatus: framework.NewStatus(framework.Skip),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			state := framework.NewCycleState()
			p := plugintesting.SetupPluginWithInformers(ctx, t, schedruntime.FactoryAdapter(feature.Features{}, New), &config.InterPodAffinityArgs{HardPodAffinityWeight: 1, IgnorePreferredTermsOfExistingPods: test.ignorePreferredTermsOfExistingPods}, cache.NewSnapshot(test.pods, test.nodes), namespaces)
			status := p.(framework.PreScorePlugin).PreScore(ctx, state, test.pod, tf.BuildNodeInfos(test.nodes))

			if !status.IsSuccess() {
				if status.Code() != test.wantStatus.Code() {
					t.Errorf("InterPodAffinity#PreScore() returned unexpected status.Code got: %v, want: %v", status.Code(), test.wantStatus.Code())
				}

				if !strings.Contains(status.Message(), test.wantStatus.Message()) {
					t.Errorf("InterPodAffinity#PreScore() returned unexpected status.Message got: %v, want: %v", status.Message(), test.wantStatus.Message())
				}
				return
			}

			var gotList framework.NodeScoreList
			for _, n := range test.nodes {
				nodeName := n.ObjectMeta.Name
				score, status := p.(framework.ScorePlugin).Score(ctx, state, test.pod, nodeName)
				if !status.IsSuccess() {
					t.Errorf("unexpected error from Score: %v", status)
				}
				gotList = append(gotList, framework.NodeScore{Name: nodeName, Score: score})
			}

			status = p.(framework.ScorePlugin).ScoreExtensions().NormalizeScore(ctx, state, test.pod, gotList)
			if !status.IsSuccess() {
				t.Errorf("unexpected error from NormalizeScore: %v", status)
			}

			if diff := cmp.Diff(test.expectedList, gotList); diff != "" {
				t.Errorf("node score list doesn't match (-want,+got): \n %s", diff)
			}
		})
	}
}

func TestPreferredAffinityWithHardPodAffinitySymmetricWeight(t *testing.T) {
	podLabelServiceS1 := map[string]string{
		"service": "S1",
	}
	labelRgChina := map[string]string{
		"region": "China",
	}
	labelRgIndia := map[string]string{
		"region": "India",
	}
	labelAzAz1 := map[string]string{
		"az": "az1",
	}
	hardPodAffinity := &v1.Affinity{
		PodAffinity: &v1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "service",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"S1"},
							},
						},
					},
					Namespaces: []string{"", "subteam2.team2"},
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
			},
		},
	}
	tests := []struct {
		pod                   *v1.Pod
		pods                  []*v1.Pod
		nodes                 []*v1.Node
		hardPodAffinityWeight int32
		expectedList          framework.NodeScoreList
		name                  string
		wantStatus            *framework.Status
	}{
		{
			name: "with default weight",
			pod:  &v1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: podLabelServiceS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: hardPodAffinity}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: hardPodAffinity}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			hardPodAffinityWeight: v1.DefaultHardPodAffinitySymmetricWeight,
			expectedList:          []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: 0}},
		},
		{
			name: "with zero weight",
			pod:  &v1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: podLabelServiceS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: hardPodAffinity}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: hardPodAffinity}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			hardPodAffinityWeight: 0,
			wantStatus:            framework.NewStatus(framework.Skip),
		},
		{
			name: "with no matching namespace",
			pod:  &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team2", Labels: podLabelServiceS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: hardPodAffinity}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: hardPodAffinity}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			hardPodAffinityWeight: v1.DefaultHardPodAffinitySymmetricWeight,
			wantStatus:            framework.NewStatus(framework.Skip),
		},
		{
			name: "with matching NamespaceSelector",
			pod:  &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "subteam1.team1", Labels: podLabelServiceS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: hardPodAffinity}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: hardPodAffinity}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			hardPodAffinityWeight: v1.DefaultHardPodAffinitySymmetricWeight,
			expectedList:          []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: 0}},
		},
		{
			name: "with matching Namespaces",
			pod:  &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "subteam2.team2", Labels: podLabelServiceS1}},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{NodeName: "node1", Affinity: hardPodAffinity}},
				{Spec: v1.PodSpec{NodeName: "node2", Affinity: hardPodAffinity}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: labelRgChina}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node2", Labels: labelRgIndia}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node3", Labels: labelAzAz1}},
			},
			hardPodAffinityWeight: v1.DefaultHardPodAffinitySymmetricWeight,
			expectedList:          []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: framework.MaxNodeScore}, {Name: "node3", Score: 0}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			state := framework.NewCycleState()
			p := plugintesting.SetupPluginWithInformers(ctx, t, schedruntime.FactoryAdapter(feature.Features{}, New), &config.InterPodAffinityArgs{HardPodAffinityWeight: test.hardPodAffinityWeight}, cache.NewSnapshot(test.pods, test.nodes), namespaces)
			status := p.(framework.PreScorePlugin).PreScore(ctx, state, test.pod, tf.BuildNodeInfos(test.nodes))
			if !test.wantStatus.Equal(status) {
				t.Errorf("InterPodAffinity#PreScore() returned unexpected status.Code got: %v, want: %v", status.Code(), test.wantStatus.Code())
			}
			if !status.IsSuccess() {
				return
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
				t.Errorf("unexpected NodeScoreList (-want,+got):\n%s", diff)
			}
		})
	}
}
