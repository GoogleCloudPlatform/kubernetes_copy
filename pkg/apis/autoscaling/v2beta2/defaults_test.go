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

package v2beta2_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/api/legacyscheme"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/autoscaling"
	_ "k8s.io/kubernetes/pkg/apis/autoscaling/install"
	. "k8s.io/kubernetes/pkg/apis/autoscaling/v2beta2"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/utils/ptr"
)

func TestGenerateScaleDownRules(t *testing.T) {
	type TestCase struct {
		rateDownPods                 int32
		rateDownPodsPeriodSeconds    int32
		rateDownPercent              int32
		rateDownPercentPeriodSeconds int32
		stabilizationSeconds         *int32
		selectPolicy                 *autoscalingv2.ScalingPolicySelect

		expectedPolicies      []autoscalingv2.HPAScalingPolicy
		expectedStabilization *int32
		expectedSelectPolicy  string
		annotation            string
	}
	maxPolicy := autoscalingv2.MaxPolicySelect
	minPolicy := autoscalingv2.MinPolicySelect
	tests := []TestCase{
		{
			annotation: "Default values",
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 15},
			},
			expectedStabilization: nil,
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
		{
			annotation:                   "All parameters are specified",
			rateDownPods:                 1,
			rateDownPodsPeriodSeconds:    2,
			rateDownPercent:              3,
			rateDownPercentPeriodSeconds: 4,
			stabilizationSeconds:         ptr.To(int32(25)),
			selectPolicy:                 &maxPolicy,
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PodsScalingPolicy, Value: 1, PeriodSeconds: 2},
				{Type: autoscalingv2.PercentScalingPolicy, Value: 3, PeriodSeconds: 4},
			},
			expectedStabilization: ptr.To(int32(25)),
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
		{
			annotation:                   "Percent policy is specified",
			rateDownPercent:              1,
			rateDownPercentPeriodSeconds: 2,
			selectPolicy:                 &minPolicy,
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PercentScalingPolicy, Value: 1, PeriodSeconds: 2},
			},
			expectedStabilization: nil,
			expectedSelectPolicy:  string(autoscalingv2.MinPolicySelect),
		},
		{
			annotation:                "Pods policy is specified",
			rateDownPods:              3,
			rateDownPodsPeriodSeconds: 4,
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PodsScalingPolicy, Value: 3, PeriodSeconds: 4},
			},
			expectedStabilization: nil,
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
	}
	for _, tc := range tests {
		t.Run(tc.annotation, func(t *testing.T) {
			scaleDownRules := &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: tc.stabilizationSeconds,
				SelectPolicy:               tc.selectPolicy,
			}
			if tc.rateDownPods != 0 || tc.rateDownPodsPeriodSeconds != 0 {
				scaleDownRules.Policies = append(scaleDownRules.Policies, autoscalingv2.HPAScalingPolicy{
					Type: autoscalingv2.PodsScalingPolicy, Value: tc.rateDownPods, PeriodSeconds: tc.rateDownPodsPeriodSeconds,
				})
			}
			if tc.rateDownPercent != 0 || tc.rateDownPercentPeriodSeconds != 0 {
				scaleDownRules.Policies = append(scaleDownRules.Policies, autoscalingv2.HPAScalingPolicy{
					Type: autoscalingv2.PercentScalingPolicy, Value: tc.rateDownPercent, PeriodSeconds: tc.rateDownPercentPeriodSeconds,
				})
			}
			down := GenerateHPAScaleDownRules(scaleDownRules)
			assert.EqualValues(t, tc.expectedPolicies, down.Policies)
			if tc.expectedStabilization != nil {
				assert.Equal(t, *tc.expectedStabilization, *down.StabilizationWindowSeconds)
			} else {
				assert.Equal(t, tc.expectedStabilization, down.StabilizationWindowSeconds)
			}
			assert.Equal(t, autoscalingv2.ScalingPolicySelect(tc.expectedSelectPolicy), *down.SelectPolicy)
		})
	}
}

func TestGenerateScaleUpRules(t *testing.T) {
	type TestCase struct {
		rateUpPods                 int32
		rateUpPodsPeriodSeconds    int32
		rateUpPercent              int32
		rateUpPercentPeriodSeconds int32
		stabilizationSeconds       *int32
		selectPolicy               *autoscalingv2.ScalingPolicySelect

		expectedPolicies      []autoscalingv2.HPAScalingPolicy
		expectedStabilization *int32
		expectedSelectPolicy  string
		annotation            string
	}
	maxPolicy := autoscalingv2.MaxPolicySelect
	minPolicy := autoscalingv2.MinPolicySelect
	tests := []TestCase{
		{
			annotation: "Default values",
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PodsScalingPolicy, Value: 4, PeriodSeconds: 15},
				{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 15},
			},
			expectedStabilization: ptr.To(int32(0)),
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
		{
			annotation:                 "All parameters are specified",
			rateUpPods:                 1,
			rateUpPodsPeriodSeconds:    2,
			rateUpPercent:              3,
			rateUpPercentPeriodSeconds: 4,
			stabilizationSeconds:       ptr.To(int32(25)),
			selectPolicy:               &maxPolicy,
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PodsScalingPolicy, Value: 1, PeriodSeconds: 2},
				{Type: autoscalingv2.PercentScalingPolicy, Value: 3, PeriodSeconds: 4},
			},
			expectedStabilization: ptr.To(int32(25)),
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
		{
			annotation:              "Pod policy is specified",
			rateUpPods:              1,
			rateUpPodsPeriodSeconds: 2,
			selectPolicy:            &minPolicy,
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PodsScalingPolicy, Value: 1, PeriodSeconds: 2},
			},
			expectedStabilization: ptr.To(int32(0)),
			expectedSelectPolicy:  string(autoscalingv2.MinPolicySelect),
		},
		{
			annotation:                 "Percent policy is specified",
			rateUpPercent:              7,
			rateUpPercentPeriodSeconds: 10,
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PercentScalingPolicy, Value: 7, PeriodSeconds: 10},
			},
			expectedStabilization: ptr.To(int32(0)),
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
		{
			annotation:              "Pod policy and stabilization window are specified",
			rateUpPodsPeriodSeconds: 2,
			stabilizationSeconds:    ptr.To(int32(25)),
			rateUpPods:              4,
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PodsScalingPolicy, Value: 4, PeriodSeconds: 2},
			},
			expectedStabilization: ptr.To(int32(25)),
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
		{
			annotation:                 "Percent policy and stabilization window are specified",
			rateUpPercent:              7,
			rateUpPercentPeriodSeconds: 60,
			stabilizationSeconds:       ptr.To(int32(25)),
			expectedPolicies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PercentScalingPolicy, Value: 7, PeriodSeconds: 60},
			},
			expectedStabilization: ptr.To(int32(25)),
			expectedSelectPolicy:  string(autoscalingv2.MaxPolicySelect),
		},
	}
	for _, tc := range tests {
		t.Run(tc.annotation, func(t *testing.T) {
			scaleUpRules := &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: tc.stabilizationSeconds,
				SelectPolicy:               tc.selectPolicy,
			}
			if tc.rateUpPods != 0 || tc.rateUpPodsPeriodSeconds != 0 {
				scaleUpRules.Policies = append(scaleUpRules.Policies, autoscalingv2.HPAScalingPolicy{
					Type: autoscalingv2.PodsScalingPolicy, Value: tc.rateUpPods, PeriodSeconds: tc.rateUpPodsPeriodSeconds,
				})
			}
			if tc.rateUpPercent != 0 || tc.rateUpPercentPeriodSeconds != 0 {
				scaleUpRules.Policies = append(scaleUpRules.Policies, autoscalingv2.HPAScalingPolicy{
					Type: autoscalingv2.PercentScalingPolicy, Value: tc.rateUpPercent, PeriodSeconds: tc.rateUpPercentPeriodSeconds,
				})
			}
			up := GenerateHPAScaleUpRules(scaleUpRules)
			assert.Equal(t, tc.expectedPolicies, up.Policies)
			if tc.expectedStabilization != nil {
				assert.Equal(t, *tc.expectedStabilization, *up.StabilizationWindowSeconds)
			} else {
				assert.Equal(t, tc.expectedStabilization, up.StabilizationWindowSeconds)
			}

			assert.Equal(t, autoscalingv2.ScalingPolicySelect(tc.expectedSelectPolicy), *up.SelectPolicy)
		})
	}
}

func TestHorizontalPodAutoscalerAnnotations(t *testing.T) {
	tests := []struct {
		hpa  autoscalingv2.HorizontalPodAutoscaler
		test string
	}{
		{
			hpa: autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						autoscaling.HorizontalPodAutoscalerConditionsAnnotation: "",
						autoscaling.MetricSpecsAnnotation:                       "",
						autoscaling.BehaviorSpecsAnnotation:                     "",
						autoscaling.MetricStatusesAnnotation:                    "",
					},
				},
			},
			test: "test empty value for Annotations",
		},
		{
			hpa: autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						autoscaling.HorizontalPodAutoscalerConditionsAnnotation: "abc",
						autoscaling.MetricSpecsAnnotation:                       "abc",
						autoscaling.BehaviorSpecsAnnotation:                     "abc",
						autoscaling.MetricStatusesAnnotation:                    "abc",
					},
				},
			},
			test: "test random value for Annotations",
		},
		{
			hpa: autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						autoscaling.HorizontalPodAutoscalerConditionsAnnotation: "[]",
						autoscaling.MetricSpecsAnnotation:                       "[]",
						autoscaling.BehaviorSpecsAnnotation:                     "[]",
						autoscaling.MetricStatusesAnnotation:                    "[]",
					},
				},
			},
			test: "test empty array value for Annotations",
		},
	}

	for _, test := range tests {
		hpa := &test.hpa
		hpaBeforeMuatate := *hpa.DeepCopy()
		obj := roundTrip(t, runtime.Object(hpa))
		final_obj, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
		if !ok {
			t.Fatalf("unexpected object: %v", obj)
		}
		if !reflect.DeepEqual(*hpa, hpaBeforeMuatate) {
			t.Errorf("diff: %v", cmp.Diff(*hpa, hpaBeforeMuatate))
			t.Errorf("expected: %#v\n actual:   %#v", *hpa, hpaBeforeMuatate)
		}

		if len(final_obj.ObjectMeta.Annotations) != 0 {
			t.Fatalf("unexpected annotations: %v", final_obj.ObjectMeta.Annotations)
		}
	}
}

func roundTrip(t *testing.T, obj runtime.Object) runtime.Object {
	data, err := runtime.Encode(legacyscheme.Codecs.LegacyCodec(SchemeGroupVersion), obj)
	if err != nil {
		t.Errorf("%v\n %#v", err, obj)
		return nil
	}
	obj2, err := runtime.Decode(legacyscheme.Codecs.UniversalDecoder(), data)
	if err != nil {
		t.Errorf("%v\nData: %s\nSource: %#v", err, string(data), obj)
		return nil
	}
	obj3 := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(runtime.Object)
	err = legacyscheme.Scheme.Convert(obj2, obj3, nil)
	if err != nil {
		t.Errorf("%v\nSource: %#v", err, obj2)
		return nil
	}
	return obj3
}
