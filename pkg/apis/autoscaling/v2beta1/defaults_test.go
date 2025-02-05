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

package v2beta1_test

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	autoscalingv2beta1 "k8s.io/api/autoscaling/v2beta1"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/autoscaling"
	_ "k8s.io/kubernetes/pkg/apis/autoscaling/install"
	. "k8s.io/kubernetes/pkg/apis/autoscaling/v2beta1"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/utils/ptr"
)

func TestSetDefaultHPA(t *testing.T) {
	utilizationDefaultVal := int32(autoscaling.DefaultCPUUtilization)
	defaultReplicas := ptr.To(int32(1))
	defaultTemplate := []autoscalingv2beta1.MetricSpec{
		{
			Type: autoscalingv2beta1.ResourceMetricSourceType,
			Resource: &autoscalingv2beta1.ResourceMetricSource{
				Name:                     v1.ResourceCPU,
				TargetAverageUtilization: &utilizationDefaultVal,
			},
		},
	}

	tests := []struct {
		original *autoscalingv2beta1.HorizontalPodAutoscaler
		expected *autoscalingv2beta1.HorizontalPodAutoscaler
	}{
		{ // MinReplicas default value
			original: &autoscalingv2beta1.HorizontalPodAutoscaler{
				Spec: autoscalingv2beta1.HorizontalPodAutoscalerSpec{
					Metrics: defaultTemplate,
				},
			},
			expected: &autoscalingv2beta1.HorizontalPodAutoscaler{
				Spec: autoscalingv2beta1.HorizontalPodAutoscalerSpec{
					MinReplicas: defaultReplicas,
					Metrics:     defaultTemplate,
				},
			},
		},
		{ // MinReplicas update
			original: &autoscalingv2beta1.HorizontalPodAutoscaler{
				Spec: autoscalingv2beta1.HorizontalPodAutoscalerSpec{
					MinReplicas: ptr.To(int32(3)),
					Metrics:     defaultTemplate,
				},
			},
			expected: &autoscalingv2beta1.HorizontalPodAutoscaler{
				Spec: autoscalingv2beta1.HorizontalPodAutoscalerSpec{
					MinReplicas: ptr.To(int32(3)),
					Metrics:     defaultTemplate,
				},
			},
		},
		{ // Metrics default value
			original: &autoscalingv2beta1.HorizontalPodAutoscaler{
				Spec: autoscalingv2beta1.HorizontalPodAutoscalerSpec{
					MinReplicas: defaultReplicas,
				},
			},
			expected: &autoscalingv2beta1.HorizontalPodAutoscaler{
				Spec: autoscalingv2beta1.HorizontalPodAutoscalerSpec{
					MinReplicas: defaultReplicas,
					Metrics:     defaultTemplate,
				},
			},
		},
	}

	for i, test := range tests {
		original := test.original
		expected := test.expected
		obj2 := roundTrip(t, runtime.Object(original))
		got, ok := obj2.(*autoscalingv2beta1.HorizontalPodAutoscaler)
		if !ok {
			t.Fatalf("(%d) unexpected object: %v", i, obj2)
		}
		if !apiequality.Semantic.DeepEqual(got.Spec, expected.Spec) {
			t.Errorf("(%d) got different than expected\ngot:\n\t%+v\nexpected:\n\t%+v", i, got.Spec, expected.Spec)
		}
	}
}

func TestHorizontalPodAutoscalerAnnotations(t *testing.T) {
	tests := []struct {
		hpa  autoscalingv2beta1.HorizontalPodAutoscaler
		test string
	}{
		{
			hpa: autoscalingv2beta1.HorizontalPodAutoscaler{
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
			hpa: autoscalingv2beta1.HorizontalPodAutoscaler{
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
			hpa: autoscalingv2beta1.HorizontalPodAutoscaler{
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
		final_obj, ok := obj.(*autoscalingv2beta1.HorizontalPodAutoscaler)
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
