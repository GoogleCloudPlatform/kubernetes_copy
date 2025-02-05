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

package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/kubernetes/pkg/apis/autoscaling"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/utils/ptr"
)

// Test for #101370
func TestConvert_autoscaling_HorizontalPodAutoscalerSpec_To_v1_HorizontalPodAutoscalerSpec(t *testing.T) {

	type args struct {
		in        *autoscaling.HorizontalPodAutoscalerSpec
		out       *autoscalingv1.HorizontalPodAutoscalerSpec
		expectOut *autoscalingv1.HorizontalPodAutoscalerSpec
		s         conversion.Scope
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			"TestConversionWithCPUAverageValueAndUtilizationBoth1",
			args{
				in: &autoscaling.HorizontalPodAutoscalerSpec{
					MinReplicas: ptr.To(int32(1)),
					MaxReplicas: 3,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
								},
							},
						},
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(70)),
								},
							},
						},
					},
				},
				out: &autoscalingv1.HorizontalPodAutoscalerSpec{},
				expectOut: &autoscalingv1.HorizontalPodAutoscalerSpec{
					MinReplicas:                    ptr.To(int32(1)),
					MaxReplicas:                    3,
					TargetCPUUtilizationPercentage: ptr.To(int32(70)),
				},
				s: nil,
			},
			false,
		},
		{
			"TestConversionWithCPUAverageValueAndUtilizationBoth2",
			args{
				in: &autoscaling.HorizontalPodAutoscalerSpec{
					MinReplicas: ptr.To(int32(1)),
					MaxReplicas: 3,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:         autoscaling.AverageValueMetricType,
									AverageValue: resource.NewMilliQuantity(300, resource.DecimalSI),
								},
							},
						},
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(70)),
								},
							},
						},
					},
				},
				out: &autoscalingv1.HorizontalPodAutoscalerSpec{
					MinReplicas:                    ptr.To(int32(2)),
					MaxReplicas:                    4,
					TargetCPUUtilizationPercentage: ptr.To(int32(60)),
				},
				expectOut: &autoscalingv1.HorizontalPodAutoscalerSpec{
					MinReplicas:                    ptr.To(int32(1)),
					MaxReplicas:                    3,
					TargetCPUUtilizationPercentage: ptr.To(int32(70)),
				},
				s: nil,
			},
			false,
		},
		{
			"TestConversionWithoutMetrics",
			args{
				in: &autoscaling.HorizontalPodAutoscalerSpec{
					MinReplicas: ptr.To(int32(1)),
					MaxReplicas: 3,
					Metrics:     []autoscaling.MetricSpec{},
				},
				out: &autoscalingv1.HorizontalPodAutoscalerSpec{
					MinReplicas:                    ptr.To(int32(1)),
					MaxReplicas:                    4,
					TargetCPUUtilizationPercentage: ptr.To(int32(60)),
				},
				expectOut: &autoscalingv1.HorizontalPodAutoscalerSpec{
					MinReplicas:                    ptr.To(int32(1)),
					MaxReplicas:                    3,
					TargetCPUUtilizationPercentage: ptr.To(int32(60)),
				},
				s: nil,
			},
			false,
		},
		{
			"TestConversionWithCPUUtilizationOnly",
			args{
				in: &autoscaling.HorizontalPodAutoscalerSpec{
					MinReplicas: ptr.To(int32(1)),
					MaxReplicas: 3,
					Metrics: []autoscaling.MetricSpec{
						{
							Type: autoscaling.ResourceMetricSourceType,
							Resource: &autoscaling.ResourceMetricSource{
								Name: api.ResourceCPU,
								Target: autoscaling.MetricTarget{
									Type:               autoscaling.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(60)),
								},
							},
						},
					},
				},
				out: &autoscalingv1.HorizontalPodAutoscalerSpec{},
				expectOut: &autoscalingv1.HorizontalPodAutoscalerSpec{
					MinReplicas:                    ptr.To(int32(1)),
					MaxReplicas:                    3,
					TargetCPUUtilizationPercentage: ptr.To(int32(60)),
				},
				s: nil,
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Convert_autoscaling_HorizontalPodAutoscalerSpec_To_v1_HorizontalPodAutoscalerSpec(tt.args.in, tt.args.out, tt.args.s); (err != nil) != tt.wantErr {
				t.Errorf("Convert_autoscaling_HorizontalPodAutoscalerSpec_To_v1_HorizontalPodAutoscalerSpec() error = %v, wantErr %v", err, tt.wantErr)
			}

			assert.Equal(t, tt.args.expectOut, tt.args.out)
		})
	}
}
