/*
Copyright 2018 The Kubernetes Authors.

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

package v2beta2

import (
	autoscalingv2beta2 "k8s.io/api/autoscaling/v2beta2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/apis/autoscaling"
	"k8s.io/utils/ptr"
)

var (
	// These constants repeats previous HPA behavior
	scaleUpLimitPercent         int32 = 100
	scaleUpLimitMinimumPods     int32 = 4
	scaleUpPeriod               int32 = 15
	scaleUpStabilizationSeconds int32
	maxPolicy                   = autoscalingv2beta2.MaxPolicySelect
	defaultHPAScaleUpRules      = autoscalingv2beta2.HPAScalingRules{
		StabilizationWindowSeconds: &scaleUpStabilizationSeconds,
		SelectPolicy:               &maxPolicy,
		Policies: []autoscalingv2beta2.HPAScalingPolicy{
			{
				Type:          autoscalingv2beta2.PodsScalingPolicy,
				Value:         scaleUpLimitMinimumPods,
				PeriodSeconds: scaleUpPeriod,
			},
			{
				Type:          autoscalingv2beta2.PercentScalingPolicy,
				Value:         scaleUpLimitPercent,
				PeriodSeconds: scaleUpPeriod,
			},
		},
	}
	scaleDownPeriod int32 = 15
	// Currently we can set the downscaleStabilizationWindow from the command line
	// So we can not rewrite the command line option from here
	scaleDownStabilizationSeconds *int32 = nil
	scaleDownLimitPercent         int32  = 100
	defaultHPAScaleDownRules             = autoscalingv2beta2.HPAScalingRules{
		StabilizationWindowSeconds: scaleDownStabilizationSeconds,
		SelectPolicy:               &maxPolicy,
		Policies: []autoscalingv2beta2.HPAScalingPolicy{
			{
				Type:          autoscalingv2beta2.PercentScalingPolicy,
				Value:         scaleDownLimitPercent,
				PeriodSeconds: scaleDownPeriod,
			},
		},
	}
)

func addDefaultingFuncs(scheme *runtime.Scheme) error {
	return RegisterDefaults(scheme)
}

func SetDefaults_HorizontalPodAutoscaler(obj *autoscalingv2beta2.HorizontalPodAutoscaler) {
	if obj.Spec.MinReplicas == nil {
		obj.Spec.MinReplicas = ptr.To(int32(1))
	}

	if len(obj.Spec.Metrics) == 0 {
		utilizationDefaultVal := int32(autoscaling.DefaultCPUUtilization)
		obj.Spec.Metrics = []autoscalingv2beta2.MetricSpec{
			{
				Type: autoscalingv2beta2.ResourceMetricSourceType,
				Resource: &autoscalingv2beta2.ResourceMetricSource{
					Name: v1.ResourceCPU,
					Target: autoscalingv2beta2.MetricTarget{
						Type:               autoscalingv2beta2.UtilizationMetricType,
						AverageUtilization: &utilizationDefaultVal,
					},
				},
			},
		}
	}
	SetDefaults_HorizontalPodAutoscalerBehavior(obj)
}

// SetDefaults_HorizontalPodAutoscalerBehavior fills the behavior if it is not null
func SetDefaults_HorizontalPodAutoscalerBehavior(obj *autoscalingv2beta2.HorizontalPodAutoscaler) {
	// if behavior is specified, we should fill all the 'nil' values with the default ones
	if obj.Spec.Behavior != nil {
		obj.Spec.Behavior.ScaleUp = GenerateHPAScaleUpRules(obj.Spec.Behavior.ScaleUp)
		obj.Spec.Behavior.ScaleDown = GenerateHPAScaleDownRules(obj.Spec.Behavior.ScaleDown)
	}
}

// GenerateHPAScaleUpRules returns a fully-initialized HPAScalingRules value
// We guarantee that no pointer in the structure will have the 'nil' value
func GenerateHPAScaleUpRules(scalingRules *autoscalingv2beta2.HPAScalingRules) *autoscalingv2beta2.HPAScalingRules {
	defaultScalingRules := defaultHPAScaleUpRules.DeepCopy()
	return copyHPAScalingRules(scalingRules, defaultScalingRules)
}

// GenerateHPAScaleDownRules returns a fully-initialized HPAScalingRules value
// We guarantee that no pointer in the structure will have the 'nil' value
// EXCEPT StabilizationWindowSeconds, for reasoning check the comment for defaultHPAScaleDownRules
func GenerateHPAScaleDownRules(scalingRules *autoscalingv2beta2.HPAScalingRules) *autoscalingv2beta2.HPAScalingRules {
	defaultScalingRules := defaultHPAScaleDownRules.DeepCopy()
	return copyHPAScalingRules(scalingRules, defaultScalingRules)
}

// copyHPAScalingRules copies all non-`nil` fields in HPA constraint structure
func copyHPAScalingRules(from, to *autoscalingv2beta2.HPAScalingRules) *autoscalingv2beta2.HPAScalingRules {
	if from == nil {
		return to
	}
	if from.SelectPolicy != nil {
		to.SelectPolicy = from.SelectPolicy
	}
	if from.StabilizationWindowSeconds != nil {
		to.StabilizationWindowSeconds = from.StabilizationWindowSeconds
	}
	if from.Policies != nil {
		to.Policies = from.Policies
	}
	return to
}
