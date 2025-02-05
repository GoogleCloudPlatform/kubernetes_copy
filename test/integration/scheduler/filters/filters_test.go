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

package filters

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/kubernetes/pkg/features"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/pointer"
)

var (
	createAndWaitForNodesInCache = testutils.CreateAndWaitForNodesInCache
	createNamespacesWithLabels   = testutils.CreateNamespacesWithLabels
	createNode                   = testutils.CreateNode
	updateNode                   = testutils.UpdateNode
	createPausePod               = testutils.CreatePausePod
	deletePod                    = testutils.DeletePod
	getPod                       = testutils.GetPod
	runPausePod                  = testutils.RunPausePod
	initPausePod                 = testutils.InitPausePod
	initTest                     = testutils.InitTestSchedulerWithNS
	podScheduledIn               = testutils.PodScheduledIn
	podScheduled                 = testutils.PodScheduled
	podUnschedulable             = testutils.PodUnschedulable
	waitForPodUnschedulable      = testutils.WaitForPodUnschedulable
)

// This file tests the scheduler predicates functionality.

const pollInterval = 100 * time.Millisecond

var (
	ignorePolicy = v1.NodeInclusionPolicyIgnore
	honorPolicy  = v1.NodeInclusionPolicyHonor
	taints       = []v1.Taint{{Key: v1.TaintNodeUnschedulable, Value: "", Effect: v1.TaintEffectNoSchedule}}
)

// TestInterPodAffinity verifies that scheduler's inter pod affinity and
// anti-affinity predicate functions works correctly.
func TestInterPodAffinity(t *testing.T) {
	podLabel := map[string]string{"service": "securityscan"}
	podLabel2 := map[string]string{"security": "S1"}

	defaultNS := "ns1"

	tests := []struct {
		name                           string
		pod                            *v1.Pod
		pods                           []*v1.Pod
		fits                           bool
		enableMatchLabelKeysInAffinity bool
		errorType                      string
	}{
		{
			name: "validates that a pod with an invalid podAffinity is rejected because of the LabelSelectorRequirement is invalid",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "security",
												Operator: metav1.LabelSelectorOpDoesNotExist,
											},
										},
									},
									TopologyKey: "region",
								},
							},
						},
					},
				},
			},
			fits:      false,
			errorType: "invalidPod",
		},
		{
			name: "validates that Inter-pod-Affinity is respected if not matching",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "security",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"securityscan"},
											},
										},
									},
									TopologyKey: "region",
								},
							},
						},
					},
				},
			},
			fits: false,
		},
		{
			name: "validates that InterPodAffinity is respected if matching. requiredDuringSchedulingIgnoredDuringExecution in PodAffinity using In operator that matches the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
									TopologyKey: "region",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename2",
					Labels: podLabel,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					NodeName:   "testnode-0",
				},
			},
			},
			fits: true,
		},
		{
			name: "validates that InterPodAffinity is respected if matching. requiredDuringSchedulingIgnoredDuringExecution in PodAffinity using not in operator in labelSelector that matches the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"securityscan3", "value3"},
											},
										},
									},
									TopologyKey: "region",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "testnode-0"},
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename2",
					Labels: podLabel}}},
			fits: true,
		},
		{
			name: "validates that inter-pod-affinity is respected when pods have different Namespaces",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
									TopologyKey: "region",
									Namespaces:  []string{"diff-namespace"},
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "testnode-0"},
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename2",
					Labels: podLabel, Namespace: "ns2"}}},
			fits: false,
		},
		{
			name: "Doesn't satisfy the PodAffinity because of unmatching labelSelector with the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"antivirusscan", "value2"},
											},
										},
									},
									TopologyKey: "region",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "testnode-0"}, ObjectMeta: metav1.ObjectMeta{
				Name:   "fakename2",
				Labels: podLabel}}},
			fits: false,
		},
		{
			name: "validates that InterPodAffinity is respected if matching with multiple affinities in multiple RequiredDuringSchedulingIgnoredDuringExecution ",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "testnode-0"}, ObjectMeta: metav1.ObjectMeta{
				Name:   "fakename2",
				Labels: podLabel}}},
			fits: true,
		},
		{
			name: "The labelSelector requirements(items of matchExpressions) are ANDed, the pod cannot schedule onto the node because one of the matchExpression items doesn't match.",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabel2,
					Name:   "fakename",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "testnode-0"}, ObjectMeta: metav1.ObjectMeta{
				Name:   "fakename2",
				Labels: podLabel}}},
			fits: false,
		},
		{
			name: "validates that InterPod Affinity and AntiAffinity is respected if matching",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
									TopologyKey: "region",
								},
							},
						},
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"antivirusscan", "value2"},
											},
										},
									},
									TopologyKey: "node",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "testnode-0"}, ObjectMeta: metav1.ObjectMeta{
				Name:   "fakename2",
				Labels: podLabel}}},
			fits: true,
		},
		{
			name: "satisfies the PodAffinity and PodAntiAffinity and PodAntiAffinity symmetry with the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
									TopologyKey: "region",
								},
							},
						},
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"antivirusscan", "value2"},
											},
										},
									},
									TopologyKey: "node",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{
				{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
						Affinity: &v1.Affinity{
							PodAntiAffinity: &v1.PodAntiAffinity{
								RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
									{
										LabelSelector: &metav1.LabelSelector{
											MatchExpressions: []metav1.LabelSelectorRequirement{
												{
													Key:      "service",
													Operator: metav1.LabelSelectorOpIn,
													Values:   []string{"antivirusscan", "value2"},
												},
											},
										},
										TopologyKey: "node",
									},
								},
							},
						},
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:   "fakename2",
						Labels: podLabel},
				},
			},
			fits: true,
		},
		{
			name: "satisfies the PodAffinity but doesn't satisfies the PodAntiAffinity with the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel2,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
									TopologyKey: "region",
								},
							},
						},
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
									TopologyKey: "zone",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "testnode-0"}, ObjectMeta: metav1.ObjectMeta{
				Name:   "fakename2",
				Labels: podLabel}}},
			fits: false,
		},
		{
			name: "satisfies the PodAffinity and PodAntiAffinity but doesn't satisfies PodAntiAffinity symmetry with the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
									TopologyKey: "region",
								},
							},
						},
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"antivirusscan", "value2"},
											},
										},
									},
									TopologyKey: "node",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{
				{
					Spec: v1.PodSpec{
						NodeName:   "testnode-0",
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						Affinity: &v1.Affinity{
							PodAntiAffinity: &v1.PodAntiAffinity{
								RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
									{
										LabelSelector: &metav1.LabelSelector{
											MatchExpressions: []metav1.LabelSelectorRequirement{
												{
													Key:      "service",
													Operator: metav1.LabelSelectorOpIn,
													Values:   []string{"securityscan", "value3"},
												},
											},
										},
										TopologyKey: "zone",
									},
								},
							},
						},
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:   "fakename2",
						Labels: podLabel},
				},
			},
			fits: false,
		},
		{
			name: "pod matches its own Label in PodAffinity and that matches the existing pod Labels",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"securityscan", "value2"},
											},
										},
									},
									TopologyKey: "region",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				NodeName:   "machine2"}, ObjectMeta: metav1.ObjectMeta{
				Name:   "fakename2",
				Labels: podLabel}}},
			fits: false,
		},
		{
			name: "Verify that PodAntiAffinity of an existing pod is respected when PodAntiAffinity symmetry is not satisfied with the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fakename",
					Labels: podLabel,
				},
				Spec: v1.PodSpec{Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}}},
			},
			pods: []*v1.Pod{
				{
					Spec: v1.PodSpec{NodeName: "testnode-0",
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						Affinity: &v1.Affinity{
							PodAntiAffinity: &v1.PodAntiAffinity{
								RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
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
										TopologyKey: "zone",
									},
								},
							},
						},
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:   "fakename2",
						Labels: podLabel},
				},
			},
			fits: false,
		},
		{
			name: "Verify that PodAntiAffinity from existing pod is respected when pod statisfies PodAntiAffinity symmetry with the existing pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "fake-name",
					Labels: podLabel,
				},
				Spec: v1.PodSpec{Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}}},
			},
			pods: []*v1.Pod{
				{
					Spec: v1.PodSpec{NodeName: "testnode-0",
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						Affinity: &v1.Affinity{
							PodAntiAffinity: &v1.PodAntiAffinity{
								RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
									{
										LabelSelector: &metav1.LabelSelector{
											MatchExpressions: []metav1.LabelSelectorRequirement{
												{
													Key:      "service",
													Operator: metav1.LabelSelectorOpNotIn,
													Values:   []string{"securityscan", "value2"},
												},
											},
										},
										TopologyKey: "zone",
									},
								},
							},
						},
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:   "fake-name2",
						Labels: podLabel},
				},
			},
			fits: true,
		},
		{
			name: "nodes[0] and nodes[1] have same topologyKey and label value. nodes[0] has an existing pod that matches the inter pod affinity rule. The new pod can not be scheduled onto either of the two nodes.",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "fake-name2"},
				Spec: v1.PodSpec{
					Containers:   []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					NodeSelector: map[string]string{"region": "r1"},
					Affinity: &v1.Affinity{
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "foo",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"abc"},
											},
										},
									},
									TopologyKey: "region",
								},
							},
						},
					},
				},
			},
			pods: []*v1.Pod{
				{Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					NodeName:   "testnode-0"}, ObjectMeta: metav1.ObjectMeta{Name: "fakename", Labels: map[string]string{"foo": "abc"}}},
			},
			fits: false,
		},
		{
			name: "anti affinity: matchLabelKeys is merged into LabelSelector with In operator (feature flag: enabled)",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									TopologyKey: "zone",
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "foo",
												Operator: metav1.LabelSelectorOpExists,
											},
										},
									},
									MatchLabelKeys: []string{"bar"},
								},
							},
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:   "incoming",
					Labels: map[string]string{"foo": "", "bar": "a"},
				},
			},
			pods: []*v1.Pod{
				// It matches the incoming Pod's anti affinity's labelSelector.
				// BUT, the matchLabelKeys make the existing Pod's anti affinity's labelSelector not match with this label.
				{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"foo": "", "bar": "fuga"}},
				},
				// It matches the incoming Pod's anti affinity's labelSelector.
				// BUT, the matchLabelKeys make the existing Pod's anti affinity's labelSelector not match with this label.
				{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"foo": "", "bar": "hoge"}},
				},
			},
			enableMatchLabelKeysInAffinity: true,
			fits:                           true,
		},
		{
			name: "anti affinity: mismatchLabelKeys is merged into LabelSelector with NotIn operator (feature flag: enabled)",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAntiAffinity: &v1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									TopologyKey: "zone",
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "foo",
												Operator: metav1.LabelSelectorOpExists,
											},
										},
									},
									MismatchLabelKeys: []string{"bar"},
								},
							},
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:   "incoming",
					Labels: map[string]string{"foo": "", "bar": "a"},
				},
			},
			pods: []*v1.Pod{
				// It matches the incoming Pod's anti affinity's labelSelector.
				// BUT, the mismatchLabelKeys make the existing Pod's anti affinity's labelSelector not match with this label.
				{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"foo": "", "bar": "a"}},
				},
				// It matches the incoming Pod's anti affinity's labelSelector.
				// BUT, the mismatchLabelKeys make the existing Pod's anti affinity's labelSelector not match with this label.
				{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"foo": "", "bar": "a"}},
				},
			},
			enableMatchLabelKeysInAffinity: true,
			fits:                           true,
		},
		{
			name: "affinity: matchLabelKeys is merged into LabelSelector with In operator (feature flag: enabled)",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "container",
							Image: imageutils.GetPauseImageName(),
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceMemory: resource.MustParse("1G"),
								},
							},
						},
					},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									TopologyKey: "node",
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "foo",
												Operator: metav1.LabelSelectorOpExists,
											},
										},
									},
									MatchLabelKeys: []string{"bar"},
								},
							},
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:   "incoming",
					Labels: map[string]string{"foo": "", "bar": "a"},
				},
			},
			pods: []*v1.Pod{
				{
					// It matches the incoming affinity. But, it uses all resources on nodes[1].
					// So, the incoming Pod can no longer get scheduled on nodes[1].
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "container",
								Image: imageutils.GetPauseImageName(),
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceMemory: resource.MustParse("1G"),
									},
								},
							},
						},
						NodeName: "anothernode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"foo": "", "bar": "a"}},
				},
				{
					// It doesn't match the incoming affinity due to matchLabelKeys.
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"foo": "", "bar": "hoge"}},
				},
				{
					// It doesn't match the incoming affinity due to matchLabelKeys.
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod3", Labels: map[string]string{"foo": "", "bar": "fuga"}},
				},
			},
			enableMatchLabelKeysInAffinity: true,
			fits:                           false,
		},
		{
			name: "affinity: mismatchLabelKeys is merged into LabelSelector with NotIn operator (feature flag: enabled)",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "container",
							Image: imageutils.GetPauseImageName(),
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceMemory: resource.MustParse("1G"),
								},
							},
						},
					},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									TopologyKey: "node",
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "foo",
												Operator: metav1.LabelSelectorOpExists,
											},
										},
									},
									MismatchLabelKeys: []string{"bar"},
								},
							},
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:   "incoming",
					Labels: map[string]string{"foo": "", "bar": "a"},
				},
			},
			pods: []*v1.Pod{
				{
					// It matches the incoming affinity. But, it uses all resources on nodes[1].
					// So, the incoming Pod can no longer get scheduled on nodes[1].
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "container",
								Image: imageutils.GetPauseImageName(),
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceMemory: resource.MustParse("1G"),
									},
								},
							},
						},
						NodeName: "anothernode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"foo": "", "bar": "fuga"}},
				},
				{
					// It doesn't match the incoming affinity due to mismatchLabelKeys.
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"foo": "", "bar": "a"}},
				},
				{
					// It doesn't match the incoming affinity due to mismatchLabelKeys.
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
						NodeName:   "testnode-0",
					},
					ObjectMeta: metav1.ObjectMeta{Name: "pod3", Labels: map[string]string{"foo": "", "bar": "a"}},
				},
			},
			enableMatchLabelKeysInAffinity: true,
			fits:                           false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodAffinity, test.enableMatchLabelKeysInAffinity)
			_, ctx := ktesting.NewTestContext(t)

			testCtx := initTest(t, "")
			cs := testCtx.ClientSet

			if _, err := createNode(cs, st.MakeNode().Name("testnode-0").Label("region", "r1").Label("zone", "z11").Label("node", "n1").Capacity(
				map[v1.ResourceName]string{
					v1.ResourceMemory: "1G",
				},
			).Obj()); err != nil {
				t.Fatalf("failed to create node: %v", err)
			}

			// another test node has the same "region" and "zone" labels as testnode, but has a different "node" label.
			if _, err := createNode(cs, st.MakeNode().Name("anothernode-0").Label("region", "r1").Label("zone", "z11").Label("node", "n2").Capacity(
				map[v1.ResourceName]string{
					v1.ResourceMemory: "1G",
				},
			).Obj()); err != nil {
				t.Fatalf("failed to create node: %v", err)
			}

			if err := createNamespacesWithLabels(cs, []string{"ns1", "ns2"}, map[string]string{"team": "team1"}); err != nil {
				t.Fatal(err)
			}
			if err := createNamespacesWithLabels(cs, []string{"ns3"}, map[string]string{"team": "team2"}); err != nil {
				t.Fatal(err)
			}

			for _, pod := range test.pods {
				if pod.Namespace == "" {
					pod.Namespace = defaultNS
				}
				createdPod, err := cs.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Error while creating pod: %v", err)
				}
				err = wait.PollUntilContextTimeout(ctx, pollInterval, wait.ForeverTestTimeout, false,
					testutils.PodScheduled(cs, createdPod.Namespace, createdPod.Name))
				if err != nil {
					t.Errorf("Error while creating pod: %v", err)
				}
			}
			if test.pod.Namespace == "" {
				test.pod.Namespace = defaultNS
			}

			testPod, err := cs.CoreV1().Pods(test.pod.Namespace).Create(ctx, test.pod, metav1.CreateOptions{})
			if err != nil {
				if !(test.errorType == "invalidPod" && apierrors.IsInvalid(err)) {
					t.Fatalf("Error while creating pod: %v", err)
				}
			}

			if test.fits {
				err = wait.PollUntilContextTimeout(ctx, pollInterval, wait.ForeverTestTimeout, false,
					testutils.PodScheduled(cs, testPod.Namespace, testPod.Name))
			} else {
				err = wait.PollUntilContextTimeout(ctx, pollInterval, wait.ForeverTestTimeout, false,
					podUnschedulable(cs, testPod.Namespace, testPod.Name))
			}
			if err != nil {
				t.Errorf("Error while trying to fit a pod: %v", err)
				return
			}

			err = cs.CoreV1().Pods(test.pod.Namespace).Delete(ctx, test.pod.Name, *metav1.NewDeleteOptions(0))
			if err != nil {
				t.Errorf("Error while deleting pod: %v", err)
			}
			err = wait.PollUntilContextTimeout(ctx, pollInterval, wait.ForeverTestTimeout, true,
				testutils.PodDeleted(ctx, cs, testCtx.NS.Name, test.pod.Name))
			if err != nil {
				t.Errorf("Error while waiting for pod to get deleted: %v", err)
			}
			for _, pod := range test.pods {
				err = cs.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
				if err != nil {
					t.Errorf("Error while deleting pod: %v", err)
				}
				err = wait.PollUntilContextTimeout(ctx, pollInterval, wait.ForeverTestTimeout, true,
					testutils.PodDeleted(ctx, cs, pod.Namespace, pod.Name))
				if err != nil {
					t.Errorf("Error while waiting for pod to get deleted: %v", err)
				}
			}
		})
	}
}

// TestInterPodAffinityWithNamespaceSelector verifies that inter pod affinity with NamespaceSelector works as expected.
func TestInterPodAffinityWithNamespaceSelector(t *testing.T) {
	podLabel := map[string]string{"service": "securityscan"}
	tests := []struct {
		name        string
		pod         *v1.Pod
		existingPod *v1.Pod
		fits        bool
		errorType   string
	}{
		{
			name: "MatchingNamespaces",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-ns-selector",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"securityscan"},
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
							},
						},
					},
				},
			},
			existingPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fakename2",
					Labels:    podLabel,
					Namespace: "ns2",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				},
			},
			fits: true,
		},
		{
			name: "MismatchingNamespaces",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-ns-selector",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
					Affinity: &v1.Affinity{
						PodAffinity: &v1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "service",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"securityscan"},
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
							},
						},
					},
				},
			},
			existingPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fakename2",
					Labels:    podLabel,
					Namespace: "ns3",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container", Image: imageutils.GetPauseImageName()}},
				},
			},
			fits: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testCtx := initTest(t, "")

			// Add a few nodes with labels
			nodes, err := createAndWaitForNodesInCache(testCtx, "testnode", st.MakeNode().Label("region", "r1").Label("zone", "z11"), 2)
			if err != nil {
				t.Fatal(err)
			}
			test.existingPod.Spec.NodeName = nodes[0].Name

			cs := testCtx.ClientSet

			if err := createNamespacesWithLabels(cs, []string{"ns1", "ns2"}, map[string]string{"team": "team1"}); err != nil {
				t.Fatal(err)
			}
			if err := createNamespacesWithLabels(cs, []string{"ns3"}, map[string]string{"team": "team2"}); err != nil {
				t.Fatal(err)
			}
			defaultNS := "ns1"

			createdPod, err := cs.CoreV1().Pods(test.existingPod.Namespace).Create(testCtx.Ctx, test.existingPod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Error while creating pod: %v", err)
			}
			err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
				testutils.PodScheduled(cs, createdPod.Namespace, createdPod.Name))
			if err != nil {
				t.Errorf("Error while creating pod: %v", err)
			}

			if test.pod.Namespace == "" {
				test.pod.Namespace = defaultNS
			}

			testPod, err := cs.CoreV1().Pods(test.pod.Namespace).Create(testCtx.Ctx, test.pod, metav1.CreateOptions{})
			if err != nil {
				if !(test.errorType == "invalidPod" && apierrors.IsInvalid(err)) {
					t.Fatalf("Error while creating pod: %v", err)
				}
			}

			if test.fits {
				err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
					testutils.PodScheduled(cs, testPod.Namespace, testPod.Name))
			} else {
				err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
					podUnschedulable(cs, testPod.Namespace, testPod.Name))
			}
			if err != nil {
				t.Errorf("Error while trying to fit a pod: %v", err)
			}
			err = cs.CoreV1().Pods(test.pod.Namespace).Delete(testCtx.Ctx, test.pod.Name, *metav1.NewDeleteOptions(0))
			if err != nil {
				t.Errorf("Error while deleting pod: %v", err)
			}
			err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, true,
				testutils.PodDeleted(testCtx.Ctx, cs, testCtx.NS.Name, test.pod.Name))
			if err != nil {
				t.Errorf("Error while waiting for pod to get deleted: %v", err)
			}
			err = cs.CoreV1().Pods(test.existingPod.Namespace).Delete(testCtx.Ctx, test.existingPod.Name, *metav1.NewDeleteOptions(0))
			if err != nil {
				t.Errorf("Error while deleting pod: %v", err)
			}
			err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, true,
				testutils.PodDeleted(testCtx.Ctx, cs, test.existingPod.Namespace, test.existingPod.Name))
			if err != nil {
				t.Errorf("Error while waiting for pod to get deleted: %v", err)
			}
		})
	}
}

// TestPodTopologySpreadFilter verifies that EvenPodsSpread predicate functions well.
func TestPodTopologySpreadFilter(t *testing.T) {
	pause := imageutils.GetPauseImageName()
	//  default nodes with labels "zone: zone-{0,1}" and "node: <node name>".
	defaultNodes := []*v1.Node{
		st.MakeNode().Name("node-0").Label("node", "node-0").Label("zone", "zone-0").Obj(),
		st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-0").Obj(),
		st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Obj(),
		st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-1").Obj(),
	}

	tests := []struct {
		name                      string
		incomingPod               *v1.Pod
		existingPods              []*v1.Pod
		fits                      bool
		nodes                     []*v1.Node
		candidateNodes            []string // nodes expected to schedule onto
		enableNodeInclusionPolicy bool
		enableMatchLabelKeys      bool
	}{
		// note: naming starts at index 0
		{
			name: "place pod on a 1/1/0/1 cluster with MaxSkew=1, node-2 is the only fit",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(1, "node", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p0").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p1").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-2"},
		},
		{
			name: "place pod on a 2/0/0/1 cluster with MaxSkew=2, node-{1,2,3} are good fits",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(2, "node", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p0a").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p0b").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-1", "node-2", "node-3"},
		},
		{
			name: "pod is required to be placed on zone0, so only node-1 fits",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeAffinityIn("zone", []string{"zone-0"}, st.NodeSelectorTypeMatchExpressions).
				SpreadConstraint(1, "node", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p0").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-1"},
		},
		{
			name: "two constraints: pod can only be placed to zone-1/node-2",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(1, "zone", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				SpreadConstraint(1, "node", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p0").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p1").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3b").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-2"},
		},
		{
			name: "pod cannot be placed onto any node",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeAffinityNotIn("node", []string{"node-0"}). // mock a 3-node cluster
				SpreadConstraint(1, "zone", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				SpreadConstraint(1, "node", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p1b").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2b").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:  false,
			nodes: defaultNodes,
		},
		{
			name: "high priority pod can preempt others",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).Priority(100).
				NodeAffinityNotIn("node", []string{"node-0"}). // mock a 3-node cluster
				SpreadConstraint(1, "zone", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				SpreadConstraint(1, "node", hardSpread, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().ZeroTerminationGracePeriod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p1b").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p2b").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p3").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-1", "node-2", "node-3"},
		},
		{
			name: "pods spread across nodes as 2/2/1, maxSkew is 2, and the number of domains < minDomains, then the third node fits",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeAffinityNotIn("node", []string{"node-0"}). // mock a 3-node cluster
				SpreadConstraint(
					2,
					"node",
					hardSpread,
					st.MakeLabelSelector().Exists("foo").Obj(),
					pointer.Int32(4), // larger than the number of domains (= 3)
					nil,
					nil,
					nil,
				).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().ZeroTerminationGracePeriod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p1b").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p2b").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p3").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-3"},
		},
		{
			name: "pods spread across nodes as 2/2/1, maxSkew is 2, and the number of domains > minDomains, then the all nodes fit",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeAffinityNotIn("node", []string{"node-0"}). // mock a 3-node cluster
				SpreadConstraint(
					2,
					"node",
					hardSpread,
					st.MakeLabelSelector().Exists("foo").Obj(),
					pointer.Int32(2), // smaller than the number of domains (= 3)
					nil,
					nil,
					nil,
				).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().ZeroTerminationGracePeriod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p1b").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p2b").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().ZeroTerminationGracePeriod().Name("p3").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-1", "node-2", "node-3"},
		},
		{
			name: "pods spread across zone as 2/1, maxSkew is 2 and the number of domains < minDomains, then the third and fourth nodes fit",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(
					2,
					"zone",
					v1.DoNotSchedule,
					st.MakeLabelSelector().Exists("foo").Obj(),
					pointer.Int32(3), // larger than the number of domains(2)
					nil,
					nil,
					nil,
				).Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-2").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-2", "node-3"},
		},
		{
			name: "pods spread across zone as 2/1, maxSkew is 2 and the number of domains < minDomains, then the all nodes fit",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(
					2,
					"zone",
					v1.DoNotSchedule,
					st.MakeLabelSelector().Exists("foo").Obj(),
					pointer.Int32(1), // smaller than the number of domains(2)
					nil,
					nil,
					nil,
				).Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
			},
			fits:           true,
			nodes:          defaultNodes,
			candidateNodes: []string{"node-0", "node-1", "node-2", "node-3"},
		},
		{
			name: "NodeAffinityPolicy honored with labelSelectors, pods spread across zone as 2/1",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p4a").Node("node-4").Label("foo", "").Container(pause).Obj(),
			},
			fits: true,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-2").Obj(),
				st.MakeNode().Name("node-4").Label("node", "node-4").Label("zone", "zone-2").Label("foo", "").Obj(),
			},
			candidateNodes:            []string{"node-4"}, // node-3 is filtered out by NodeAffinity plugin
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeAffinityPolicy ignored with nodeAffinity, pods spread across zone as 1/~2~",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeAffinityIn("foo", []string{""}, st.NodeSelectorTypeMatchExpressions).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, &ignorePolicy, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p4a").Node("node-4").Label("foo", "").Container(pause).Obj(),
			},
			fits: true,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-2").Obj(),
				st.MakeNode().Name("node-4").Label("node", "node-4").Label("zone", "zone-2").Label("foo", "").Obj(),
			},
			candidateNodes:            []string{"node-1", "node-2"},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeTaintsPolicy honored, pods spread across zone as 2/1",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, &honorPolicy, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p4a").Node("node-4").Label("foo", "").Container(pause).Obj(),
			},
			fits: true,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-2").Taints(taints).Obj(),
				st.MakeNode().Name("node-4").Label("node", "node-4").Label("zone", "zone-2").Label("foo", "").Obj(),
			},
			candidateNodes:            []string{"node-4"}, // node-3 is filtered out by TaintToleration plugin
			enableNodeInclusionPolicy: true,
		},
		{
			name: "NodeTaintsPolicy ignored, pods spread across zone as 2/2",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p4a").Node("node-4").Label("foo", "").Container(pause).Obj(),
			},
			fits: true,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-2").Taints(taints).Obj(),
				st.MakeNode().Name("node-4").Label("node", "node-4").Label("zone", "zone-2").Label("foo", "").Obj(),
			},
			candidateNodes:            []string{"node-1", "node-2", "node-4"}, // node-3 is filtered out by TaintToleration plugin
			enableNodeInclusionPolicy: true,
		},
		{
			// 1. to fulfil "zone" constraint, pods spread across zones as 2/1
			// 2. to fulfil "node" constraint, pods spread across zones as 1/1/~0~/1
			// intersection of (1) and (2) returns node-4 as node-3 is filtered out by NodeAffinity plugin.
			name: "two node inclusion Constraints, zone: honor/ignore, node: honor/ignore",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p4a").Node("node-4").Label("foo", "").Container(pause).Obj(),
			},
			fits: true,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Label("foo", "").Taints(taints).Obj(),
				st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-2").Obj(),
				st.MakeNode().Name("node-4").Label("node", "node-4").Label("zone", "zone-2").Label("foo", "").Obj(),
			},
			candidateNodes:            []string{"node-4"},
			enableNodeInclusionPolicy: true,
		},
		{
			// 1. to fulfil "zone" constraint, pods spread across zones as 2/1
			// 2. to fulfil "node" constraint, pods spread across zones as 1/1/~0~/1
			// intersection of (1) and (2) returns node-4 as node-3 is filtered out by NodeAffinity plugin
			name: "feature gate disabled, two node inclusion Constraints, zone: honor/ignore, node: honor/ignore",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p4a").Node("node-4").Label("foo", "").Container(pause).Obj(),
			},
			fits: true,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Label("foo", "").Taints(taints).Obj(),
				st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-2").Obj(),
				st.MakeNode().Name("node-4").Label("node", "node-4").Label("zone", "zone-2").Label("foo", "").Obj(),
			},
			candidateNodes:            []string{"node-4"},
			enableNodeInclusionPolicy: false,
		},
		{
			// 1. to fulfil "zone" constraint, pods spread across zones as 2/2
			// 2. to fulfil "node" constraint, pods spread across zones as 1/~0~/~0~/1
			// intersection of (1) and (2) returns node-1 and node-4 as node-2, node-3 are filtered out by plugins
			name: "two node inclusion Constraints, zone: ignore/ignore, node: honor/honor",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				NodeSelector(map[string]string{"foo": ""}).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, &ignorePolicy, nil, nil).
				SpreadConstraint(1, "node", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, &honorPolicy, nil).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-2").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-3").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p4a").Node("node-4").Label("foo", "").Container(pause).Obj(),
			},
			fits: true,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label("node", "node-1").Label("zone", "zone-1").Label("foo", "").Obj(),
				st.MakeNode().Name("node-2").Label("node", "node-2").Label("zone", "zone-1").Label("foo", "").Taints(taints).Obj(),
				st.MakeNode().Name("node-3").Label("node", "node-3").Label("zone", "zone-2").Obj(),
				st.MakeNode().Name("node-4").Label("node", "node-4").Label("zone", "zone-2").Label("foo", "").Obj(),
			},
			candidateNodes:            []string{"node-1", "node-4"},
			enableNodeInclusionPolicy: true,
		},
		{
			name: "matchLabelKeys ignored when feature gate disabled, pods spread across zone as 2/1",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").Container(pause).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, []string{"bar"}).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-2").Label("foo", "").Label("bar", "").Container(pause).Obj(),
			},
			fits:                 true,
			nodes:                defaultNodes,
			candidateNodes:       []string{"node-2", "node-3"},
			enableMatchLabelKeys: false,
		},
		{
			name: "matchLabelKeys ANDed with LabelSelector when LabelSelector isn't empty, pods spread across zone as 0/1",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Label("bar", "").Container(pause).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj(), nil, nil, nil, []string{"bar"}).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-2").Label("foo", "").Label("bar", "").Container(pause).Obj(),
			},
			fits:                 true,
			nodes:                defaultNodes,
			candidateNodes:       []string{"node-0", "node-1"},
			enableMatchLabelKeys: true,
		},
		{
			name: "matchLabelKeys ANDed with LabelSelector when LabelSelector is empty, pods spread across zone as 2/1",
			incomingPod: st.MakePod().Name("p").Label("foo", "").Container(pause).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Obj(), nil, nil, nil, []string{"foo"}).
				Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1a").Node("node-0").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p2a").Node("node-1").Label("foo", "").Container(pause).Obj(),
				st.MakePod().Name("p3a").Node("node-2").Label("foo", "").Container(pause).Obj(),
			},
			fits:                 true,
			nodes:                defaultNodes,
			candidateNodes:       []string{"node-2", "node-3"},
			enableMatchLabelKeys: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, tt.enableNodeInclusionPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodTopologySpread, tt.enableMatchLabelKeys)

			testCtx := initTest(t, "pts-predicate")
			cs := testCtx.ClientSet
			ns := testCtx.NS.Name

			for i := range tt.nodes {
				if _, err := createNode(cs, tt.nodes[i]); err != nil {
					t.Fatalf("Cannot create node: %v", err)
				}
			}

			// set namespace to pods
			for i := range tt.existingPods {
				tt.existingPods[i].SetNamespace(ns)
			}
			tt.incomingPod.SetNamespace(ns)

			allPods := append(tt.existingPods, tt.incomingPod)
			defer testutils.CleanupPods(testCtx.Ctx, cs, t, allPods)

			for _, pod := range tt.existingPods {
				createdPod, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Error while creating pod during test: %v", err)
				}
				err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
					testutils.PodScheduled(cs, createdPod.Namespace, createdPod.Name))
				if err != nil {
					t.Errorf("Error while waiting for pod during test: %v", err)
				}
			}
			testPod, err := cs.CoreV1().Pods(tt.incomingPod.Namespace).Create(testCtx.Ctx, tt.incomingPod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Error while creating pod during test: %v", err)
			}

			if tt.fits {
				err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
					podScheduledIn(cs, testPod.Namespace, testPod.Name, tt.candidateNodes))
			} else {
				err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
					podUnschedulable(cs, testPod.Namespace, testPod.Name))
			}
			if err != nil {
				t.Errorf("Test Failed: %v", err)
			}
		})
	}
}

func TestNodePortsFilter(t *testing.T) {
	pause := imageutils.GetPauseImageName()
	tests := []struct {
		name         string
		existingPods []*v1.Pod
		incomingPod  *v1.Pod
		fit          bool
	}{
		{
			name: "Port Conflict on Node",
			existingPods: []*v1.Pod{
				st.MakePod().Name("test-1").
					Containers([]v1.Container{st.MakeContainer().Name("test-1-container").Image(pause).
						ContainerPort([]v1.ContainerPort{
							{
								HostIP:        "127.0.0.1",
								ContainerPort: 8081,
								HostPort:      8081,
								Protocol:      "TCP",
							},
							{
								HostIP:        "127.0.0.1",
								ContainerPort: 8082,
								HostPort:      8082,
								Protocol:      "TCP",
							},
						}).Obj()}).
					Obj(),
			},
			incomingPod: st.MakePod().Name("test-2").
				Containers([]v1.Container{st.MakeContainer().Name("test-2-container").Image(pause).
					ContainerPort([]v1.ContainerPort{
						{
							HostIP:        "127.0.0.1",
							ContainerPort: 8081,
							HostPort:      8081,
							Protocol:      "TCP",
						},
					}).Obj()}).
				Obj(),
			fit: false,
		},
		{
			name: "No Port Conflict on Node",
			existingPods: []*v1.Pod{
				st.MakePod().Name("test-1").
					Containers([]v1.Container{st.MakeContainer().Name("test-1-container").Image(pause).
						ContainerPort([]v1.ContainerPort{
							{
								HostIP:        "127.0.0.1",
								HostPort:      8081,
								ContainerPort: 8081,
								Protocol:      "TCP",
							},
							{
								HostIP:        "127.0.0.1",
								HostPort:      8082,
								ContainerPort: 8082,
								Protocol:      "TCP",
							},
						}).Obj()}).
					Obj(),
			},
			incomingPod: st.MakePod().Name("test-2").
				Containers([]v1.Container{st.MakeContainer().Name("test-2-container").Image(pause).
					ContainerPort([]v1.ContainerPort{
						{
							HostIP:        "127.0.0.1",
							HostPort:      8080,
							ContainerPort: 8080,
							Protocol:      "TCP",
						},
					}).Obj()}).
				Obj(),
			fit: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCtx := initTest(t, "node-ports-filter")
			cs := testCtx.ClientSet
			ns := testCtx.NS.Name

			if _, err := createNode(cs, st.MakeNode().Name("node-0").Obj()); err != nil {
				t.Fatalf("Cannot create node: %v", err)
			}

			// set namespace to pods
			tt.incomingPod.SetNamespace(ns)
			for i := range tt.existingPods {
				tt.existingPods[i].SetNamespace(ns)
			}
			allPods := append([]*v1.Pod{tt.incomingPod}, tt.existingPods...)
			defer testutils.CleanupPods(testCtx.Ctx, cs, t, allPods)

			for _, pod := range tt.existingPods {
				if _, err := runPausePod(testCtx.ClientSet, pod); err != nil {
					t.Fatalf("failed to create existing pod: %v", err)
				}
			}

			testPod, err := cs.CoreV1().Pods(tt.incomingPod.Namespace).Create(testCtx.Ctx, tt.incomingPod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Error while creating pod during test: %v", err)
			}

			if tt.fit {
				err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
					podScheduled(cs, testPod.Namespace, testPod.Name))
				if err != nil {
					t.Errorf("Test Failed: Expected pod %s/%s to be scheduled but got error: %v", testPod.Namespace, testPod.Name, err)
				}
			} else {
				err = wait.PollUntilContextTimeout(testCtx.Ctx, pollInterval, wait.ForeverTestTimeout, false,
					podUnschedulable(cs, testPod.Namespace, testPod.Name))
				if err != nil {
					t.Errorf("Test Failed: Expected pod %s/%s to be unschedulable but got error: %v", testPod.Namespace, testPod.Name, err)
				}
			}
		})
	}
}

var (
	hardSpread = v1.DoNotSchedule
)

func TestUnschedulablePodBecomesSchedulable(t *testing.T) {
	tests := []struct {
		name   string
		init   func(kubernetes.Interface, string) error
		pod    *testutils.PausePodConfig
		update func(kubernetes.Interface, string) error
	}{
		{
			name: "node gets added",
			pod: &testutils.PausePodConfig{
				Name: "pod-1",
			},
			update: func(cs kubernetes.Interface, _ string) error {
				_, err := createNode(cs, st.MakeNode().Name("node-added").Obj())
				if err != nil {
					return fmt.Errorf("cannot create node: %v", err)
				}
				return nil
			},
		},
		{
			name: "node gets taint removed",
			init: func(cs kubernetes.Interface, _ string) error {
				node, err := createNode(cs, st.MakeNode().Name("node-tainted").Obj())
				if err != nil {
					return fmt.Errorf("cannot create node: %v", err)
				}
				taint := v1.Taint{Key: "test", Value: "test", Effect: v1.TaintEffectNoSchedule}
				if err := testutils.AddTaintToNode(cs, node.Name, taint); err != nil {
					return fmt.Errorf("cannot add taint to node: %v", err)
				}
				return nil
			},
			pod: &testutils.PausePodConfig{
				Name: "pod-1",
			},
			update: func(cs kubernetes.Interface, _ string) error {
				taint := v1.Taint{Key: "test", Value: "test", Effect: v1.TaintEffectNoSchedule}
				if err := testutils.RemoveTaintOffNode(cs, "node-tainted", taint); err != nil {
					return fmt.Errorf("cannot remove taint off node: %v", err)
				}
				return nil
			},
		},
		{
			name: "other pod gets deleted",
			init: func(cs kubernetes.Interface, ns string) error {
				nodeObject := st.MakeNode().Name("node-scheduler-integration-test").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "1"}).Obj()
				_, err := createNode(cs, nodeObject)
				if err != nil {
					return fmt.Errorf("cannot create node: %v", err)
				}
				_, err = createPausePod(cs, initPausePod(&testutils.PausePodConfig{Name: "pod-to-be-deleted", Namespace: ns}))
				if err != nil {
					return fmt.Errorf("cannot create pod: %v", err)
				}
				return nil
			},
			pod: &testutils.PausePodConfig{
				Name: "pod-1",
			},
			update: func(cs kubernetes.Interface, ns string) error {
				if err := deletePod(cs, "pod-to-be-deleted", ns); err != nil {
					return fmt.Errorf("cannot delete pod: %v", err)
				}
				return nil
			},
		},
		{
			name: "pod with pod-affinity gets added",
			init: func(cs kubernetes.Interface, _ string) error {
				_, err := createNode(cs, st.MakeNode().Name("node-1").Label("region", "test").Obj())
				if err != nil {
					return fmt.Errorf("cannot create node: %v", err)
				}
				return nil
			},
			pod: &testutils.PausePodConfig{
				Name: "pod-1",
				Affinity: &v1.Affinity{
					PodAffinity: &v1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"pod-with-affinity": "true",
									},
								},
								TopologyKey: "region",
							},
						},
					},
				},
			},
			update: func(cs kubernetes.Interface, ns string) error {
				podConfig := &testutils.PausePodConfig{
					Name:      "pod-with-affinity",
					Namespace: ns,
					Labels: map[string]string{
						"pod-with-affinity": "true",
					},
				}
				if _, err := createPausePod(cs, initPausePod(podConfig)); err != nil {
					return fmt.Errorf("cannot create pod: %v", err)
				}
				return nil
			},
		},
		{
			name: "scheduled pod gets updated to match affinity",
			init: func(cs kubernetes.Interface, ns string) error {
				_, err := createNode(cs, st.MakeNode().Name("node-1").Label("region", "test").Obj())
				if err != nil {
					return fmt.Errorf("cannot create node: %v", err)
				}
				if _, err := createPausePod(cs, initPausePod(&testutils.PausePodConfig{Name: "pod-to-be-updated", Namespace: ns})); err != nil {
					return fmt.Errorf("cannot create pod: %v", err)
				}
				return nil
			},
			pod: &testutils.PausePodConfig{
				Name: "pod-1",
				Affinity: &v1.Affinity{
					PodAffinity: &v1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"pod-with-affinity": "true",
									},
								},
								TopologyKey: "region",
							},
						},
					},
				},
			},
			update: func(cs kubernetes.Interface, ns string) error {
				pod, err := getPod(cs, "pod-to-be-updated", ns)
				if err != nil {
					return fmt.Errorf("cannot get pod: %v", err)
				}
				pod.Labels = map[string]string{"pod-with-affinity": "true"}
				if _, err := cs.CoreV1().Pods(pod.Namespace).Update(context.TODO(), pod, metav1.UpdateOptions{}); err != nil {
					return fmt.Errorf("cannot update pod: %v", err)
				}
				return nil
			},
		},
		{
			name: "scheduled pod uses read-write-once-pod pvc",
			init: func(cs kubernetes.Interface, ns string) error {
				_, err := createNode(cs, st.MakeNode().Name("node").Obj())
				if err != nil {
					return fmt.Errorf("cannot create node: %v", err)
				}

				storage := v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}
				volType := v1.HostPathDirectoryOrCreate
				pv, err := testutils.CreatePV(cs, st.MakePersistentVolume().
					Name("pv-with-read-write-once-pod").
					AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
					Capacity(storage.Requests).
					HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/mnt", Type: &volType}).
					Obj())
				if err != nil {
					return fmt.Errorf("cannot create pv: %v", err)
				}
				pvc, err := testutils.CreatePVC(cs, st.MakePersistentVolumeClaim().
					Name("pvc-with-read-write-once-pod").
					Namespace(ns).
					// Annotation and volume name required for PVC to be considered bound.
					Annotation(volume.AnnBindCompleted, "true").
					VolumeName(pv.Name).
					AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
					Resources(storage).
					Obj())
				if err != nil {
					return fmt.Errorf("cannot create pvc: %v", err)
				}

				pod := initPausePod(&testutils.PausePodConfig{
					Name:      "pod-to-be-deleted",
					Namespace: ns,
					Volumes: []v1.Volume{{
						Name: "volume",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc.Name,
							},
						},
					}},
				})
				if _, err := createPausePod(cs, pod); err != nil {
					return fmt.Errorf("cannot create pod: %v", err)
				}
				return nil
			},
			pod: &testutils.PausePodConfig{
				Name: "pod-to-take-over-pvc",
				Volumes: []v1.Volume{{
					Name: "volume",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc-with-read-write-once-pod",
						},
					},
				}},
			},
			update: func(cs kubernetes.Interface, ns string) error {
				return deletePod(cs, "pod-to-be-deleted", ns)
			},
		},
		{
			name: "pod with pvc has node-affinity to non-existent/illegal nodes",
			init: func(cs kubernetes.Interface, ns string) error {
				storage := v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}
				volType := v1.HostPathDirectoryOrCreate
				pv, err := testutils.CreatePV(cs, st.MakePersistentVolume().
					Name("pv-has-non-existent-nodes").
					AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
					Capacity(storage.Requests).
					HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: &volType}).
					NodeAffinityIn("kubernetes.io/hostname", []string{"node-available", "non-existing"}). // one node exist, one doesn't
					Obj())
				if err != nil {
					return fmt.Errorf("cannot create pv: %w", err)
				}
				_, err = testutils.CreatePVC(cs, st.MakePersistentVolumeClaim().
					Name("pvc-has-non-existent-nodes").
					Namespace(ns).
					Annotation(volume.AnnBindCompleted, "true").
					VolumeName(pv.Name).
					AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
					Resources(storage).
					Obj())
				if err != nil {
					return fmt.Errorf("cannot create pvc: %w", err)
				}
				return nil
			},
			pod: &testutils.PausePodConfig{
				Name: "pod-with-pvc-has-non-existent-nodes",
				Volumes: []v1.Volume{{
					Name: "volume",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc-has-non-existent-nodes",
						},
					},
				}},
			},
			update: func(cs kubernetes.Interface, ns string) error {
				_, err := createNode(cs, st.MakeNode().Label("kubernetes.io/hostname", "node-available").Name("node-available").Obj())
				if err != nil {
					return fmt.Errorf("cannot create node: %w", err)
				}
				return nil
			},
		},
		{
			name: "pod with pvc got scheduled after node updated it's label",
			init: func(cs kubernetes.Interface, ns string) error {
				_, err := createNode(cs, st.MakeNode().Label("foo", "foo").Name("node-foo").Obj())
				if err != nil {
					return fmt.Errorf("cannot create node: %w", err)
				}
				storage := v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Mi")}}
				volType := v1.HostPathDirectoryOrCreate
				pv, err := testutils.CreatePV(cs, st.MakePersistentVolume().
					Name("pv-foo").
					AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
					Capacity(storage.Requests).
					HostPathVolumeSource(&v1.HostPathVolumeSource{Path: "/tmp", Type: &volType}).
					NodeAffinityIn("foo", []string{"bar"}).
					Obj())
				if err != nil {
					return fmt.Errorf("cannot create pv: %w", err)
				}
				_, err = testutils.CreatePVC(cs, st.MakePersistentVolumeClaim().
					Name("pvc-foo").
					Namespace(ns).
					Annotation(volume.AnnBindCompleted, "true").
					VolumeName(pv.Name).
					AccessModes([]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}).
					Resources(storage).
					Obj())
				if err != nil {
					return fmt.Errorf("cannot create pvc: %w", err)
				}
				return nil
			},
			pod: &testutils.PausePodConfig{
				Name: "pod-with-pvc-foo",
				Volumes: []v1.Volume{{
					Name: "volume",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc-foo",
						},
					},
				}},
			},
			update: func(cs kubernetes.Interface, ns string) error {
				_, err := updateNode(cs, &v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-foo",
						Labels: map[string]string{
							"foo": "bar",
						},
					},
				})
				if err != nil {
					return fmt.Errorf("cannot update node: %w", err)
				}
				return nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCtx := initTest(t, "scheduler-informer")

			if tt.init != nil {
				if err := tt.init(testCtx.ClientSet, testCtx.NS.Name); err != nil {
					t.Fatal(err)
				}
			}
			tt.pod.Namespace = testCtx.NS.Name
			pod, err := createPausePod(testCtx.ClientSet, initPausePod(tt.pod))
			if err != nil {
				t.Fatal(err)
			}
			if err := waitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
				t.Errorf("Pod %v got scheduled: %v", pod.Name, err)
			}
			if err := tt.update(testCtx.ClientSet, testCtx.NS.Name); err != nil {
				t.Fatal(err)
			}
			if err := testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
				t.Errorf("Pod %v was not scheduled: %v", pod.Name, err)
			}
			// Make sure pending queue is empty.
			pendingPods, s := testCtx.Scheduler.SchedulingQueue.PendingPods()
			if len(pendingPods) != 0 {
				t.Errorf("pending pods queue is not empty, size is: %d, summary is: %s", len(pendingPods), s)
			}
		})
	}
}

// TestPodAffinityMatchLabelKeyEnablement tests the Pod is correctly mutated by MatchLabelKeysInPodAffinity feature,
// even if turing the feature gate enabled or disabled.
func TestPodAffinityMatchLabelKeyEnablement(t *testing.T) {
	// enable the feature gate
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodAffinity, true)
	testCtx := initTest(t, "matchlabelkey")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test",
			Namespace:    testCtx.NS.Name,
			Labels:       map[string]string{"foo": "", "bar": "a"},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "container",
					Image: imageutils.GetPauseImageName(),
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1G"),
						},
					},
				},
			},
			Affinity: &v1.Affinity{
				PodAffinity: &v1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
						{
							TopologyKey: "node",
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "foo",
										Operator: metav1.LabelSelectorOpExists,
									},
								},
							},
							MatchLabelKeys: []string{"bar"},
						},
					},
				},
			},
		},
	}
	expectedLabelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "foo",
				Operator: metav1.LabelSelectorOpExists,
			},
			{
				Key:      "bar",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"a"},
			},
		},
	}

	p1, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error while creating pod during test: %v", err)
	}

	// check the pod has the expected label selector.
	gotpod, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Get(testCtx.Ctx, p1.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error while getting pod during test: %v", err)
	}

	// the label selector should be changed from the original one because the feature gate is enabled.
	if d := cmp.Diff(gotpod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector, expectedLabelSelector); d != "" {
		t.Fatalf("Pod %v has wrong label selector: diff = \n%v", p1.Name, d)
	}

	// disable the feature gate.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodAffinity, false)

	p2, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error while creating pod during test: %v", err)
	}

	// check the pod has the expected label selector.
	gotpod, err = testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Get(testCtx.Ctx, p2.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error while getting pod during test: %v", err)
	}

	// the label selector should be the same as the original one because the feature gate is disabled.
	if d := cmp.Diff(gotpod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector, pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector); d != "" {
		t.Fatalf("Pod %v has wrong label selector: diff = \n%v", p2.Name, d)
	}

	// check the pod, which was created when the feature gate is enabled, still has the expected label selector.
	gotpod, err = testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Get(testCtx.Ctx, p1.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error while getting pod during test: %v", err)
	}

	// the label selector should be changed from the original one because the feature gate is enabled.
	if d := cmp.Diff(gotpod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector, expectedLabelSelector); d != "" {
		t.Fatalf("Pod %v has wrong label selector: diff = \n%v", p1.Name, d)
	}

	// Again, enable the feature gate.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodAffinity, true)

	p3, err := testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error while creating pod during test: %v", err)
	}

	// check the pod has the expected label selector.
	gotpod, err = testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Get(testCtx.Ctx, p3.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error while getting pod during test: %v", err)
	}

	// the label selector should be changed from the original one because the feature gate is enabled.
	if d := cmp.Diff(gotpod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector, expectedLabelSelector); d != "" {
		t.Fatalf("Pod %v has wrong label selector: diff = \n%v", p1.Name, d)
	}

	// check the pod has the expected label selector.
	gotpod, err = testCtx.ClientSet.CoreV1().Pods(testCtx.NS.Name).Get(testCtx.Ctx, p2.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error while getting pod during test: %v", err)
	}

	// the label selector shouldn't get changed because the feature gate was disabled at its creation.
	// Even if the feature gate is enabled now, matchLabelKeys don't get applied to the pod.
	// (it's only handled when the pod is created)
	if d := cmp.Diff(gotpod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector, pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector); d != "" {
		t.Fatalf("Pod %v has wrong label selector: diff = \n%v", p2.Name, d)
	}

}
