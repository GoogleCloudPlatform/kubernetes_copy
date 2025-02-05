/*
Copyright 2016 The Kubernetes Authors.

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

package validation

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/apis/autoscaling"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

func TestValidateScale(t *testing.T) {
	successCases := []autoscaling.Scale{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "frontend",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.ScaleSpec{
			Replicas: 1,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "frontend",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.ScaleSpec{
			Replicas: 10,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "frontend",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.ScaleSpec{
			Replicas: 0,
		},
	}}

	for _, successCase := range successCases {
		if errs := ValidateScale(&successCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := []struct {
		scale autoscaling.Scale
		msg   string
	}{{
		scale: autoscaling.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "frontend",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: autoscaling.ScaleSpec{
				Replicas: -1,
			},
		},
		msg: "must be greater than or equal to 0",
	}}

	for _, c := range errorCases {
		if errs := ValidateScale(&c.scale); len(errs) == 0 {
			t.Errorf("expected failure for %s", c.msg)
		} else if !strings.Contains(errs[0].Error(), c.msg) {
			t.Errorf("unexpected error: %v, expected: %s", errs[0], c.msg)
		}
	}
}

func TestValidateBehavior(t *testing.T) {
	maxPolicy := autoscaling.MaxPolicySelect
	minPolicy := autoscaling.MinPolicySelect
	disabledPolicy := autoscaling.DisabledPolicySelect
	incorrectPolicy := autoscaling.ScalingPolicySelect("incorrect")
	simplePoliciesList := []autoscaling.HPAScalingPolicy{{
		Type:          autoscaling.PercentScalingPolicy,
		Value:         10,
		PeriodSeconds: 1,
	}, {
		Type:          autoscaling.PodsScalingPolicy,
		Value:         1,
		PeriodSeconds: 1800,
	}}
	successCases := []autoscaling.HorizontalPodAutoscalerBehavior{{
		ScaleUp:   nil,
		ScaleDown: nil,
	}, {
		ScaleUp: &autoscaling.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(int32(3600)),
			SelectPolicy:               &minPolicy,
			Policies:                   simplePoliciesList,
		},
		ScaleDown: &autoscaling.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(int32(0)),
			SelectPolicy:               &disabledPolicy,
			Policies:                   simplePoliciesList,
		},
	}, {
		ScaleUp: &autoscaling.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(int32(120)),
			SelectPolicy:               &maxPolicy,
			Policies: []autoscaling.HPAScalingPolicy{{
				Type:          autoscaling.PodsScalingPolicy,
				Value:         1,
				PeriodSeconds: 2,
			}, {
				Type:          autoscaling.PercentScalingPolicy,
				Value:         3,
				PeriodSeconds: 4,
			}, {
				Type:          autoscaling.PodsScalingPolicy,
				Value:         5,
				PeriodSeconds: 6,
			}, {
				Type:          autoscaling.PercentScalingPolicy,
				Value:         7,
				PeriodSeconds: 8,
			}},
		},
		ScaleDown: &autoscaling.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(int32(120)),
			SelectPolicy:               &maxPolicy,
			Policies: []autoscaling.HPAScalingPolicy{{
				Type:          autoscaling.PodsScalingPolicy,
				Value:         1,
				PeriodSeconds: 2,
			}, {
				Type:          autoscaling.PercentScalingPolicy,
				Value:         3,
				PeriodSeconds: 4,
			}, {
				Type:          autoscaling.PodsScalingPolicy,
				Value:         5,
				PeriodSeconds: 6,
			}, {
				Type:          autoscaling.PercentScalingPolicy,
				Value:         7,
				PeriodSeconds: 8,
			}},
		},
	}}
	for _, behavior := range successCases {
		hpa := prepareHPAWithBehavior(behavior)
		if errs := ValidateHorizontalPodAutoscaler(&hpa); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	errorCases := []struct {
		behavior autoscaling.HorizontalPodAutoscalerBehavior
		msg      string
	}{{
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				SelectPolicy: &minPolicy,
			},
		},
		msg: "spec.behavior.scaleUp.policies: Required value: must specify at least one Policy",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(3601)),
				SelectPolicy:               &minPolicy,
				Policies:                   simplePoliciesList,
			},
		},
		msg: "spec.behavior.scaleUp.stabilizationWindowSeconds: Invalid value: 3601: must be less than or equal to 3600",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					Value:         7,
					PeriodSeconds: 1801,
				}},
			},
		},
		msg: "spec.behavior.scaleUp.policies[0].periodSeconds: Invalid value: 1801: must be less than or equal to 1800",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				SelectPolicy: &incorrectPolicy,
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					Value:         7,
					PeriodSeconds: 8,
				}},
			},
		},
		msg: `spec.behavior.scaleUp.selectPolicy: Unsupported value: "incorrect": supported values: "Disabled", "Max", "Min"`,
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.HPAScalingPolicyType("hm"),
					Value:         7,
					PeriodSeconds: 8,
				}},
			},
		},
		msg: `spec.behavior.scaleUp.policies[0].type: Unsupported value: "hm": supported values: "Percent", "Pods"`,
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:  autoscaling.PodsScalingPolicy,
					Value: 8,
				}},
			},
		},
		msg: "spec.behavior.scaleUp.policies[0].periodSeconds: Invalid value: 0: must be greater than zero",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					PeriodSeconds: 8,
				}},
			},
		},
		msg: "spec.behavior.scaleUp.policies[0].value: Invalid value: 0: must be greater than zero",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					PeriodSeconds: -1,
					Value:         1,
				}},
			},
		},
		msg: "spec.behavior.scaleUp.policies[0].periodSeconds: Invalid value: -1: must be greater than zero",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					PeriodSeconds: 1,
					Value:         -1,
				}},
			},
		},
		msg: "spec.behavior.scaleUp.policies[0].value: Invalid value: -1: must be greater than zero",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				SelectPolicy: &minPolicy,
			},
		},
		msg: "spec.behavior.scaleDown.policies: Required value: must specify at least one Policy",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(3601)),
				SelectPolicy:               &minPolicy,
				Policies:                   simplePoliciesList,
			},
		},
		msg: "spec.behavior.scaleDown.stabilizationWindowSeconds: Invalid value: 3601: must be less than or equal to 3600",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PercentScalingPolicy,
					Value:         7,
					PeriodSeconds: 1801,
				}},
			},
		},
		msg: "spec.behavior.scaleDown.policies[0].periodSeconds: Invalid value: 1801: must be less than or equal to 1800",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				SelectPolicy: &incorrectPolicy,
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					Value:         7,
					PeriodSeconds: 8,
				}},
			},
		},
		msg: `spec.behavior.scaleDown.selectPolicy: Unsupported value: "incorrect": supported values: "Disabled", "Max", "Min"`,
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.HPAScalingPolicyType("hm"),
					Value:         7,
					PeriodSeconds: 8,
				}},
			},
		},
		msg: `spec.behavior.scaleDown.policies[0].type: Unsupported value: "hm": supported values: "Percent", "Pods"`,
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:  autoscaling.PodsScalingPolicy,
					Value: 8,
				}},
			},
		},
		msg: "spec.behavior.scaleDown.policies[0].periodSeconds: Invalid value: 0: must be greater than zero",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					PeriodSeconds: 8,
				}},
			},
		},
		msg: "spec.behavior.scaleDown.policies[0].value: Invalid value: 0: must be greater than zero",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					PeriodSeconds: -1,
					Value:         1,
				}},
			},
		},
		msg: "spec.behavior.scaleDown.policies[0].periodSeconds: Invalid value: -1: must be greater than zero",
	}, {
		behavior: autoscaling.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscaling.HPAScalingRules{
				Policies: []autoscaling.HPAScalingPolicy{{
					Type:          autoscaling.PodsScalingPolicy,
					PeriodSeconds: 1,
					Value:         -1,
				}},
			},
		},
		msg: "spec.behavior.scaleDown.policies[0].value: Invalid value: -1: must be greater than zero",
	}}
	for _, c := range errorCases {
		hpa := prepareHPAWithBehavior(c.behavior)
		if errs := ValidateHorizontalPodAutoscaler(&hpa); len(errs) == 0 {
			t.Errorf("expected failure for %s", c.msg)
		} else if !strings.Contains(errs[0].Error(), c.msg) {
			t.Errorf("unexpected error: %v, expected: %s", errs[0], c.msg)
		}
	}
}

func prepareHPAWithBehavior(b autoscaling.HorizontalPodAutoscalerBehavior) autoscaling.HorizontalPodAutoscaler {
	return autoscaling.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ResourceMetricSourceType,
				Resource: &autoscaling.ResourceMetricSource{
					Name: api.ResourceCPU,
					Target: autoscaling.MetricTarget{
						Type:               autoscaling.UtilizationMetricType,
						AverageUtilization: ptr.To(int32(70)),
					},
				},
			}},
			Behavior: &b,
		},
	}
}

func TestValidateHorizontalPodAutoscaler(t *testing.T) {
	metricLabelSelector, err := metav1.ParseToLabelSelector("label=value")
	if err != nil {
		t.Errorf("unable to parse label selector: %v", err)
	}

	successCases := []autoscaling.HorizontalPodAutoscaler{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ResourceMetricSourceType,
				Resource: &autoscaling.ResourceMetricSource{
					Name: api.ResourceCPU,
					Target: autoscaling.MetricTarget{
						Type:               autoscaling.UtilizationMetricType,
						AverageUtilization: ptr.To(int32(70)),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ResourceMetricSourceType,
				Resource: &autoscaling.ResourceMetricSource{
					Name: api.ResourceCPU,
					Target: autoscaling.MetricTarget{
						Type:         autoscaling.AverageValueMetricType,
						AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.PodsMetricSourceType,
				Pods: &autoscaling.PodsMetricSource{
					Metric: autoscaling.MetricIdentifier{
						Name: "somemetric",
					},
					Target: autoscaling.MetricTarget{
						Type:         autoscaling.AverageValueMetricType,
						AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ContainerResourceMetricSourceType,
				ContainerResource: &autoscaling.ContainerResourceMetricSource{
					Name:      api.ResourceCPU,
					Container: "test-container",
					Target: autoscaling.MetricTarget{
						Type:               autoscaling.UtilizationMetricType,
						AverageUtilization: ptr.To(int32(70)),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ContainerResourceMetricSourceType,
				ContainerResource: &autoscaling.ContainerResourceMetricSource{
					Name:      api.ResourceCPU,
					Container: "test-container",
					Target: autoscaling.MetricTarget{
						Type:         autoscaling.AverageValueMetricType,
						AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ObjectMetricSourceType,
				Object: &autoscaling.ObjectMetricSource{
					DescribedObject: autoscaling.CrossVersionObjectReference{
						Kind: "ReplicationController",
						Name: "myrc",
					},
					Metric: autoscaling.MetricIdentifier{
						Name: "somemetric",
					},
					Target: autoscaling.MetricTarget{
						Type:  autoscaling.ValueMetricType,
						Value: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ExternalMetricSourceType,
				External: &autoscaling.ExternalMetricSource{
					Metric: autoscaling.MetricIdentifier{
						Name:     "somemetric",
						Selector: metricLabelSelector,
					},
					Target: autoscaling.MetricTarget{
						Type:  autoscaling.ValueMetricType,
						Value: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myautoscaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ExternalMetricSourceType,
				External: &autoscaling.ExternalMetricSource{
					Metric: autoscaling.MetricIdentifier{
						Name:     "somemetric",
						Selector: metricLabelSelector,
					},
					Target: autoscaling.MetricTarget{
						Type:         autoscaling.AverageValueMetricType,
						AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}}
	for _, successCase := range successCases {
		if errs := ValidateHorizontalPodAutoscaler(&successCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := []struct {
		horizontalPodAutoscaler autoscaling.HorizontalPodAutoscaler
		msg                     string
	}{{
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.kind: Required",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.kind: Required",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Kind: "..", Name: "myrc"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.kind: Invalid",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Kind: "..", Name: "myrc"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.kind: Invalid",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.name: Required",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.name: Required",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Kind: "ReplicationController", Name: ".."},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.name: Invalid",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Kind: "ReplicationController", Name: ".."},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "scaleTargetRef.name: Invalid",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "myautoscaler",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{},
				MinReplicas:    ptr.To(int32(-1)),
				MaxReplicas:    5,
			},
		},
		msg: "must be greater than or equal to 1",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "myautoscaler",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{},
				MinReplicas:    ptr.To(int32(7)),
				MaxReplicas:    5,
			},
		},
		msg: "must be greater than or equal to `minReplicas`",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "myautoscaler",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
							AverageValue:       resource.NewMilliQuantity(300, resource.DecimalSI),
						},
					},
				}},
			},
		},
		msg: "may not set both a target raw value and a target utilization",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "myautoscaler",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
							AverageValue:       resource.NewMilliQuantity(300, resource.DecimalSI),
						},
					},
				}},
			},
		},
		msg: "may not set both a target raw value and a target utilization",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "must specify a resource name",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "must specify a resource name",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      "InvalidResource",
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(70)),
						},
					},
				}},
			},
		},
		msg: "Invalid value: InvalidResource: must be a standard resource type or fully qualified",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(-10)),
						},
					},
				}},
			},
		},
		msg: "must be greater than 0",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(-10)),
						},
					},
				}},
			},
		},
		msg: "must be greater than 0",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(-10)),
						},
					},
				}},
			},
		},
		msg: "must specify a container",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "---***",
						Target: autoscaling.MetricTarget{
							Type:               autoscaling.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(-10)),
						},
					},
				}},
			},
		},
		msg: "Invalid value: \"---***\"",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ResourceMetricSourceType,
					Resource: &autoscaling.ResourceMetricSource{
						Name: api.ResourceCPU,
						Target: autoscaling.MetricTarget{
							Type: autoscaling.ValueMetricType,
						},
					},
				}},
			},
		},
		msg: "must set either a target raw value or a target utilization",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ContainerResourceMetricSourceType,
					ContainerResource: &autoscaling.ContainerResourceMetricSource{
						Name:      api.ResourceCPU,
						Container: "test-application",
						Target: autoscaling.MetricTarget{
							Type: autoscaling.ValueMetricType,
						},
					},
				}},
			},
		},
		msg: "must set either a target raw value or a target utilization",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.PodsMetricSourceType,
					Pods: &autoscaling.PodsMetricSource{
						Metric: autoscaling.MetricIdentifier{},
						Target: autoscaling.MetricTarget{
							Type:         autoscaling.ValueMetricType,
							AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
						},
					},
				}},
			},
		},
		msg: "must specify a metric name",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.PodsMetricSourceType,
					Pods: &autoscaling.PodsMetricSource{
						Metric: autoscaling.MetricIdentifier{
							Name: "somemetric",
						},
						Target: autoscaling.MetricTarget{
							Type: autoscaling.ValueMetricType,
						},
					},
				}},
			},
		},
		msg: "must specify a positive target averageValue",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ObjectMetricSourceType,
					Object: &autoscaling.ObjectMetricSource{
						DescribedObject: autoscaling.CrossVersionObjectReference{
							Kind: "ReplicationController",
							Name: "myrc",
						},
						Metric: autoscaling.MetricIdentifier{
							Name: "somemetric",
						},
						Target: autoscaling.MetricTarget{
							Type: autoscaling.ValueMetricType,
						},
					},
				}},
			},
		},
		msg: "must set either a target value or averageValue",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ObjectMetricSourceType,
					Object: &autoscaling.ObjectMetricSource{
						DescribedObject: autoscaling.CrossVersionObjectReference{
							Name: "myrc",
						},
						Metric: autoscaling.MetricIdentifier{
							Name: "somemetric",
						},
						Target: autoscaling.MetricTarget{
							Type:  autoscaling.ValueMetricType,
							Value: resource.NewMilliQuantity(100, resource.DecimalSI),
						},
					},
				}},
			},
		},
		msg: "object.describedObject.kind: Required",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ObjectMetricSourceType,
					Object: &autoscaling.ObjectMetricSource{
						DescribedObject: autoscaling.CrossVersionObjectReference{
							Kind: "ReplicationController",
							Name: "myrc",
						},
						Target: autoscaling.MetricTarget{
							Type:  autoscaling.ValueMetricType,
							Value: resource.NewMilliQuantity(100, resource.DecimalSI),
						},
					},
				}},
			},
		},
		msg: "must specify a metric name",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ExternalMetricSourceType,
					External: &autoscaling.ExternalMetricSource{
						Metric: autoscaling.MetricIdentifier{
							Selector: metricLabelSelector,
						},
						Target: autoscaling.MetricTarget{
							Type:  autoscaling.ValueMetricType,
							Value: resource.NewMilliQuantity(300, resource.DecimalSI),
						},
					},
				}},
			},
		},
		msg: "must specify a metric name",
	}, {
		horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
			Spec: autoscaling.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
				Metrics: []autoscaling.MetricSpec{{
					Type: autoscaling.ExternalMetricSourceType,
					External: &autoscaling.ExternalMetricSource{
						Metric: autoscaling.MetricIdentifier{
							Name:     "foo/../",
							Selector: metricLabelSelector,
						},
						Target: autoscaling.MetricTarget{
							Type:  autoscaling.ValueMetricType,
							Value: resource.NewMilliQuantity(300, resource.DecimalSI),
						},
					},
				}},
			},
		},
		msg: "'/'",
	},

		{
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ExternalMetricSourceType,
						External: &autoscaling.ExternalMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name:     "somemetric",
								Selector: metricLabelSelector,
							},
							Target: autoscaling.MetricTarget{
								Type: autoscaling.ValueMetricType,
							},
						},
					}},
				},
			},
			msg: "must set either a target value for metric or a per-pod target",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ExternalMetricSourceType,
						External: &autoscaling.ExternalMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name:     "somemetric",
								Selector: metricLabelSelector,
							},
							Target: autoscaling.MetricTarget{
								Type:  autoscaling.ValueMetricType,
								Value: resource.NewMilliQuantity(-300, resource.DecimalSI),
							},
						},
					}},
				},
			},
			msg: "must be positive",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ExternalMetricSourceType,
						External: &autoscaling.ExternalMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name:     "somemetric",
								Selector: metricLabelSelector,
							},
							Target: autoscaling.MetricTarget{
								Type:         autoscaling.ValueMetricType,
								AverageValue: resource.NewMilliQuantity(-300, resource.DecimalSI),
							},
						},
					}},
				},
			},
			msg: "must be positive",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ExternalMetricSourceType,
						External: &autoscaling.ExternalMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name:     "somemetric",
								Selector: metricLabelSelector,
							},
							Target: autoscaling.MetricTarget{
								Type:         autoscaling.ValueMetricType,
								Value:        resource.NewMilliQuantity(300, resource.DecimalSI),
								AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
							},
						},
					}},
				},
			},
			msg: "may not set both a target value for metric and a per-pod target",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ExternalMetricSourceType,
						External: &autoscaling.ExternalMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name:     "somemetric",
								Selector: metricLabelSelector,
							},
							Target: autoscaling.MetricTarget{
								Type:  "boogity",
								Value: resource.NewMilliQuantity(300, resource.DecimalSI),
							},
						},
					}},
				},
			},
			msg: "must be either Utilization, Value, or AverageValue",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ExternalMetricSourceType,
						External: &autoscaling.ExternalMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name:     "somemetric",
								Selector: metricLabelSelector,
							},
							Target: autoscaling.MetricTarget{
								Value: resource.NewMilliQuantity(300, resource.DecimalSI),
							},
						},
					}},
				},
			},
			msg: "must specify a metric target type",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ExternalMetricSourceType,
						External: &autoscaling.ExternalMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name:     "somemetric",
								Selector: metricLabelSelector,
							},
						},
					}},
				},
			},
			msg: "must specify a metric target",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{
						{},
					},
				},
			},
			msg: "must specify a metric source type",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.MetricSourceType("InvalidType"),
					}},
				},
			},
			msg: "type: Unsupported value",
		}, {
			horizontalPodAutoscaler: autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					Metrics: []autoscaling.MetricSpec{{
						Type: autoscaling.ResourceMetricSourceType,
						Resource: &autoscaling.ResourceMetricSource{
							Name: api.ResourceCPU,
							Target: autoscaling.MetricTarget{
								Type:         autoscaling.AverageValueMetricType,
								AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
							},
						},
						Pods: &autoscaling.PodsMetricSource{
							Metric: autoscaling.MetricIdentifier{
								Name: "somemetric",
							},
							Target: autoscaling.MetricTarget{
								Type:         autoscaling.AverageValueMetricType,
								AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
							},
						},
					}},
				},
			},
			msg: "must populate the given metric source only",
		},
	}

	for _, c := range errorCases {
		errs := ValidateHorizontalPodAutoscaler(&c.horizontalPodAutoscaler)
		if len(errs) == 0 {
			t.Errorf("expected failure for %q", c.msg)
		} else if !strings.Contains(errs[0].Error(), c.msg) {
			t.Errorf("unexpected error: %q, expected: %q", errs[0], c.msg)
		}
	}

	sourceTypes := map[autoscaling.MetricSourceType]autoscaling.MetricSpec{
		autoscaling.ResourceMetricSourceType: {
			Resource: &autoscaling.ResourceMetricSource{
				Name: api.ResourceCPU,
				Target: autoscaling.MetricTarget{
					Type:         autoscaling.AverageValueMetricType,
					AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
				},
			},
		},
		autoscaling.ContainerResourceMetricSourceType: {
			ContainerResource: &autoscaling.ContainerResourceMetricSource{
				Name:      api.ResourceCPU,
				Container: "test-application",
				Target: autoscaling.MetricTarget{
					Type:         autoscaling.AverageValueMetricType,
					AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
				},
			},
		},
		autoscaling.PodsMetricSourceType: {
			Pods: &autoscaling.PodsMetricSource{
				Metric: autoscaling.MetricIdentifier{
					Name: "somemetric",
				},
				Target: autoscaling.MetricTarget{
					Type:         autoscaling.AverageValueMetricType,
					AverageValue: resource.NewMilliQuantity(100, resource.DecimalSI),
				},
			},
		},
		autoscaling.ObjectMetricSourceType: {
			Object: &autoscaling.ObjectMetricSource{
				DescribedObject: autoscaling.CrossVersionObjectReference{
					Kind: "ReplicationController",
					Name: "myrc",
				},
				Metric: autoscaling.MetricIdentifier{
					Name: "somemetric",
				},
				Target: autoscaling.MetricTarget{
					Type:  autoscaling.ValueMetricType,
					Value: resource.NewMilliQuantity(100, resource.DecimalSI),
				},
			},
		},
	}

	for correctType, spec := range sourceTypes {
		for incorrectType := range sourceTypes {
			if correctType == incorrectType {
				continue
			}

			spec.Type = incorrectType

			errs := ValidateHorizontalPodAutoscaler(&autoscaling.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "myautoscaler", Namespace: metav1.NamespaceDefault},
				Spec: autoscaling.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscaling.CrossVersionObjectReference{Name: "myrc", Kind: "ReplicationController"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5, Metrics: []autoscaling.MetricSpec{spec},
				},
			})

			expectedMsg := "must populate information for the given metric source"

			if len(errs) == 0 {
				t.Errorf("expected failure with type of %v and spec for %v", incorrectType, correctType)
			} else if !strings.Contains(errs[0].Error(), expectedMsg) {
				t.Errorf("unexpected error: %q, expected %q", errs[0], expectedMsg)
			}
		}
	}
}

func prepareMinReplicasCases(t *testing.T, minReplicas int32) []autoscaling.HorizontalPodAutoscaler {
	metricLabelSelector, err := metav1.ParseToLabelSelector("label=value")
	if err != nil {
		t.Errorf("unable to parse label selector: %v", err)
	}
	minReplicasCases := []autoscaling.HorizontalPodAutoscaler{{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "myautoscaler",
			Namespace:       metav1.NamespaceDefault,
			ResourceVersion: "theversion",
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(minReplicas)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ObjectMetricSourceType,
				Object: &autoscaling.ObjectMetricSource{
					DescribedObject: autoscaling.CrossVersionObjectReference{
						Kind: "ReplicationController",
						Name: "myrc",
					},
					Metric: autoscaling.MetricIdentifier{
						Name: "somemetric",
					},
					Target: autoscaling.MetricTarget{
						Type:  autoscaling.ValueMetricType,
						Value: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:            "myautoscaler",
			Namespace:       metav1.NamespaceDefault,
			ResourceVersion: "theversion",
		},
		Spec: autoscaling.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling.CrossVersionObjectReference{
				Kind: "ReplicationController",
				Name: "myrc",
			},
			MinReplicas: ptr.To(int32(minReplicas)),
			MaxReplicas: 5,
			Metrics: []autoscaling.MetricSpec{{
				Type: autoscaling.ExternalMetricSourceType,
				External: &autoscaling.ExternalMetricSource{
					Metric: autoscaling.MetricIdentifier{
						Name:     "somemetric",
						Selector: metricLabelSelector,
					},
					Target: autoscaling.MetricTarget{
						Type:         autoscaling.AverageValueMetricType,
						AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
					},
				},
			}},
		},
	}}
	return minReplicasCases
}

func TestValidateHorizontalPodAutoscalerScaleToZeroEnabled(t *testing.T) {
	// Enable HPAScaleToZero feature gate.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HPAScaleToZero, true)

	zeroMinReplicasCases := prepareMinReplicasCases(t, 0)
	for _, successCase := range zeroMinReplicasCases {
		if errs := ValidateHorizontalPodAutoscaler(&successCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
}

func TestValidateHorizontalPodAutoscalerScaleToZeroDisabled(t *testing.T) {
	// Disable HPAScaleToZero feature gate.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HPAScaleToZero, false)

	zeroMinReplicasCases := prepareMinReplicasCases(t, 0)
	errorMsg := "must be greater than or equal to 1"

	for _, errorCase := range zeroMinReplicasCases {
		errs := ValidateHorizontalPodAutoscaler(&errorCase)
		if len(errs) == 0 {
			t.Errorf("expected failure for %q", errorMsg)
		} else if !strings.Contains(errs[0].Error(), errorMsg) {
			t.Errorf("unexpected error: %q, expected: %q", errs[0], errorMsg)
		}
	}

	nonZeroMinReplicasCases := prepareMinReplicasCases(t, 1)

	for _, successCase := range nonZeroMinReplicasCases {
		successCase.Spec.MinReplicas = ptr.To(int32(1))
		if errs := ValidateHorizontalPodAutoscaler(&successCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
}

func TestValidateHorizontalPodAutoscalerUpdateScaleToZeroEnabled(t *testing.T) {
	// Enable HPAScaleToZero feature gate.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HPAScaleToZero, true)

	zeroMinReplicasCases := prepareMinReplicasCases(t, 0)
	nonZeroMinReplicasCases := prepareMinReplicasCases(t, 1)

	for i, zeroCase := range zeroMinReplicasCases {
		nonZeroCase := nonZeroMinReplicasCases[i]

		if errs := ValidateHorizontalPodAutoscalerUpdate(&nonZeroCase, &zeroCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}

		if errs := ValidateHorizontalPodAutoscalerUpdate(&zeroCase, &nonZeroCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
}

func TestValidateHorizontalPodAutoscalerScaleToZeroUpdateDisabled(t *testing.T) {
	// Disable HPAScaleToZero feature gate.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HPAScaleToZero, false)

	zeroMinReplicasCases := prepareMinReplicasCases(t, 0)
	nonZeroMinReplicasCases := prepareMinReplicasCases(t, 1)
	errorMsg := "must be greater than or equal to 1"

	for i, zeroCase := range zeroMinReplicasCases {
		nonZeroCase := nonZeroMinReplicasCases[i]
		errs := ValidateHorizontalPodAutoscalerUpdate(&zeroCase, &nonZeroCase)

		if len(errs) == 0 {
			t.Errorf("expected failure for %q", errorMsg)
		} else if !strings.Contains(errs[0].Error(), errorMsg) {
			t.Errorf("unexpected error: %q, expected: %q", errs[0], errorMsg)
		}

		if errs := ValidateHorizontalPodAutoscalerUpdate(&zeroCase, &zeroCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}

		if errs := ValidateHorizontalPodAutoscalerUpdate(&nonZeroCase, &zeroCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
}
