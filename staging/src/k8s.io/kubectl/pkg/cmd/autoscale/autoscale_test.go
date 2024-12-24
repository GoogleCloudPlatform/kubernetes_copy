/*
Copyright 2024 The Kubernetes Authors.

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

package autoscale

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
)

type validateTestCase struct {
	name          string
	options       *AutoscaleOptions
	expectedError error
}

func TestAutoscaleValidate(t *testing.T) {
	tests := []validateTestCase{
		{
			name: "valid options",
			options: &AutoscaleOptions{
				Max: 10,
				Min: 1,
			},
			expectedError: nil,
		},
		{
			name: "max less than 1",
			options: &AutoscaleOptions{
				Max: 0,
				Min: 1,
			},
			expectedError: fmt.Errorf("--max=MAXPODS is required and must be at least 1, max: 0"),
		},
		{
			name: "min greater than max",
			options: &AutoscaleOptions{
				Max: 1,
				Min: 2,
			},
			expectedError: fmt.Errorf("--max=MAXPODS must be larger or equal to --min=MINPODS, max: 1, min: 2"),
		},
		{
			name: "zero min replicas",
			options: &AutoscaleOptions{
				Max: 5,
				Min: 0,
			},
			expectedError: nil,
		},
		{
			name: "negative min replicas",
			options: &AutoscaleOptions{
				Max: 5,
				Min: -2,
			},
			expectedError: nil,
		},
		{
			name: "CPUPercent appears with CPU",
			options: &AutoscaleOptions{
				Max:        5,
				Min:        0,
				CPU:        "800",
				CPUPercent: 20,
			},
			expectedError: fmt.Errorf("--cpu-percent and --cpu are mutually exclusive, CPUPercent: %v, CPU: %v", 20, "800"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errorGot := tc.options.Validate()
			assert.Equal(t, tc.expectedError, errorGot)
		})
	}
}

type createHorizontalPodAutoscalerTestCase struct {
	name          string
	options       *AutoscaleOptions
	refName       string
	mapping       *meta.RESTMapping
	expectedHPAV2 *autoscalingv2.HorizontalPodAutoscaler
	expectedHPAV1 *autoscalingv1.HorizontalPodAutoscaler
}

func TestCreateHorizontalPodAutoscalerV2(t *testing.T) {
	cpu500m := apiresource.MustParse("500m")
	mem512Mi := apiresource.MustParse("512Mi")
	cpu2000m := apiresource.MustParse("2000m")
	mem3Gi := apiresource.MustParse("3Gi")
	tests := []createHorizontalPodAutoscalerTestCase{
		{
			name: "create with all options",
			options: &AutoscaleOptions{
				Name:       "custom-name",
				Max:        10,
				Min:        2,
				CPUPercent: 80,
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(80)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create without min replicas",
			options: &AutoscaleOptions{
				Name:       "custom-name-2",
				Max:        10,
				Min:        -1,
				CPUPercent: 80,
			},
			refName: "deployment-2",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name-2",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-2",
					},
					MinReplicas: nil,
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(80)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create without max replicas",
			options: &AutoscaleOptions{
				Name:       "custom-name-3",
				Max:        -1,
				Min:        2,
				CPUPercent: 80,
			},
			refName: "deployment-3",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name-3",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-3",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(-1),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(80)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create without cpu utilization",
			options: &AutoscaleOptions{
				Name:       "custom-name-4",
				Max:        10,
				Min:        2,
				CPUPercent: -1,
			},
			refName: "deployment-4",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name-4",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-4",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
				},
			},
		},
		{
			name: "create with replicaset reference",
			options: &AutoscaleOptions{
				Name:       "replicaset-hpa",
				Max:        5,
				Min:        1,
				CPUPercent: 70,
			},
			refName: "frontend",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "ReplicaSet",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "replicaset-hpa",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       "frontend",
					},
					MinReplicas: ptr.To(int32(1)),
					MaxReplicas: int32(5),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(70)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with statefulset reference",
			options: &AutoscaleOptions{
				Name:       "statefulset-hpa",
				Max:        8,
				Min:        2,
				CPUPercent: 60,
			},
			refName: "web",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "StatefulSet",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "statefulset-hpa",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "StatefulSet",
						Name:       "web",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(8),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(60)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with memory(use %) options",
			options: &AutoscaleOptions{
				Name:   "custom-name",
				Max:    10,
				Min:    2,
				Memory: "50%",
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceMemory,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(50)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with both cpu(use %) and memory(use %) options",
			options: &AutoscaleOptions{
				Name:   "custom-name",
				Max:    10,
				Min:    2,
				CPU:    "70%",
				Memory: "50%",
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(70)),
								},
							},
						},
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceMemory,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(50)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with both cpu(use m unit) and memory(use %) options",
			options: &AutoscaleOptions{
				Name:   "custom-name",
				Max:    10,
				Min:    2,
				CPU:    "500m",
				Memory: "50%",
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:  autoscalingv2.ValueMetricType,
									Value: &cpu500m,
								},
							},
						},
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceMemory,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(50)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with both cpu(no use unit) and memory(use %) options",
			options: &AutoscaleOptions{
				Name:   "custom-name",
				Max:    10,
				Min:    2,
				CPU:    "500",
				Memory: "50%",
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:  autoscalingv2.ValueMetricType,
									Value: &cpu500m,
								},
							},
						},
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceMemory,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: ptr.To(int32(50)),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with memory(no use unit) options",
			options: &AutoscaleOptions{
				Name:   "custom-name",
				Max:    10,
				Min:    2,
				Memory: "512",
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceMemory,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:  autoscalingv2.ValueMetricType,
									Value: &mem512Mi,
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with cpu(no use unit) and memory(no use unit) options",
			options: &AutoscaleOptions{
				Name:   "custom-name",
				Max:    10,
				Min:    2,
				CPU:    "500",
				Memory: "512",
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:  autoscalingv2.ValueMetricType,
									Value: &cpu500m,
								},
							},
						},
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceMemory,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:  autoscalingv2.ValueMetricType,
									Value: &mem512Mi,
								},
							},
						},
					},
				},
			},
		},
		{
			name: "create with both cpu(use m unit) and memory(use Gi unit) options",
			options: &AutoscaleOptions{
				Name:   "custom-name",
				Max:    10,
				Min:    2,
				CPU:    "2000m",
				Memory: "3Gi",
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV2: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
					Metrics: []autoscalingv2.MetricSpec{
						// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricSpec
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:  autoscalingv2.ValueMetricType,
									Value: &cpu2000m,
								},
							},
						},
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#ResourceMetricSource
								Name: corev1.ResourceMemory,
								Target: autoscalingv2.MetricTarget{
									// Reference: https://pkg.go.dev/k8s.io/api/autoscaling/v2#MetricTarget
									Type:  autoscalingv2.ValueMetricType,
									Value: &mem3Gi,
								},
							},
						},
					},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hpa, _ := tc.options.createHorizontalPodAutoscalerV2(tc.refName, tc.mapping)
			assert.Equal(t, tc.expectedHPAV2, hpa)
		})
	}
}

func TestCreateHorizontalPodAutoscalerV1(t *testing.T) {
	tests := []createHorizontalPodAutoscalerTestCase{
		{
			name: "create with all options",
			options: &AutoscaleOptions{
				Name:       "custom-name",
				Max:        10,
				Min:        2,
				CPUPercent: 80,
			},
			refName: "deployment-1",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV1: &autoscalingv1.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name",
				},
				Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-1",
					},
					MinReplicas:                    ptr.To(int32(2)),
					MaxReplicas:                    int32(10),
					TargetCPUUtilizationPercentage: ptr.To(int32(80)),
				},
			},
		},
		{
			name: "create without min replicas",
			options: &AutoscaleOptions{
				Name:       "custom-name-2",
				Max:        10,
				Min:        -1,
				CPUPercent: 80,
			},
			refName: "deployment-2",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV1: &autoscalingv1.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name-2",
				},
				Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-2",
					},
					MinReplicas:                    nil,
					MaxReplicas:                    int32(10),
					TargetCPUUtilizationPercentage: ptr.To(int32(80)),
				},
			},
		},
		{
			name: "create without max replicas",
			options: &AutoscaleOptions{
				Name:       "custom-name-3",
				Max:        -1,
				Min:        2,
				CPUPercent: 80,
			},
			refName: "deployment-3",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV1: &autoscalingv1.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name-3",
				},
				Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-3",
					},
					MinReplicas:                    ptr.To(int32(2)),
					MaxReplicas:                    int32(-1),
					TargetCPUUtilizationPercentage: ptr.To(int32(80)),
				},
			},
		},
		{
			name: "create without cpu utilization",
			options: &AutoscaleOptions{
				Name:       "custom-name-4",
				Max:        10,
				Min:        2,
				CPUPercent: -1,
			},
			refName: "deployment-4",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expectedHPAV1: &autoscalingv1.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-name-4",
				},
				Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "deployment-4",
					},
					MinReplicas: ptr.To(int32(2)),
					MaxReplicas: int32(10),
				},
			},
		},
		{
			name: "create with replicaset reference",
			options: &AutoscaleOptions{
				Name:       "replicaset-hpa",
				Max:        5,
				Min:        1,
				CPUPercent: 70,
			},
			refName: "frontend",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "ReplicaSet",
				},
			},
			expectedHPAV1: &autoscalingv1.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "replicaset-hpa",
				},
				Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       "frontend",
					},
					MinReplicas:                    ptr.To(int32(1)),
					MaxReplicas:                    int32(5),
					TargetCPUUtilizationPercentage: ptr.To(int32(70)),
				},
			},
		},
		{
			name: "create with statefulset reference",
			options: &AutoscaleOptions{
				Name:       "statefulset-hpa",
				Max:        8,
				Min:        2,
				CPUPercent: 60,
			},
			refName: "web",
			mapping: &meta.RESTMapping{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "StatefulSet",
				},
			},
			expectedHPAV1: &autoscalingv1.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name: "statefulset-hpa",
				},
				Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "StatefulSet",
						Name:       "web",
					},
					MinReplicas:                    ptr.To(int32(2)),
					MaxReplicas:                    int32(8),
					TargetCPUUtilizationPercentage: ptr.To(int32(60)),
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hpa := tc.options.createHorizontalPodAutoscalerV1(tc.refName, tc.mapping)
			assert.Equal(t, tc.expectedHPAV1, hpa)
		})
	}
}
