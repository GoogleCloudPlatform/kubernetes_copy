/*
Copyright 2020 The Kubernetes Authors.

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

package pod

import (
	"context"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/kubernetes/fake"
)

func TestGetPodsInNamespace(t *testing.T) {
	tests := []struct {
		name      string
		pods      []runtime.Object
		wantPods  []*v1.Pod
		expectErr bool
	}{
		{
			name: "nil check",
		},
		{
			name: "2 pods",
			pods: []runtime.Object{
				&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
			},
			wantPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fakeclient.NewSimpleClientset(tt.pods...)
			got, err := GetPodsInNamespace(context.Background(), cs, "", map[string]string{})
			if (err != nil) != tt.expectErr {
				t.Errorf("expectErr = %v, but got err = %v", tt.expectErr, err)
			}
			if !reflect.DeepEqual(got, tt.wantPods) {
				t.Errorf("expect %v, got %v", tt.wantPods, got)
			}
		})
	}
}

func TestFilterNonRecreatablePods(t *testing.T) {
	deploymentKind := v1.SchemeGroupVersion.WithKind("Deployment")
	tests := []struct {
		name            string
		pods            []*v1.Pod
		expectedPodsLen int
	}{
		{
			name: "only normal pods",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "normal-pod-1",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: deploymentKind.Version,
								Kind:       deploymentKind.Kind,
							},
						},
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "normal-pod-2",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: deploymentKind.Version,
								Kind:       deploymentKind.Kind,
							},
						},
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
					},
				},
			},
			expectedPodsLen: 2,
		},
		{
			name: "naked pod",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "naked-pod",
						OwnerReferences: nil,
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
					},
				},
			},
			expectedPodsLen: 0,
		},
		{
			name: "mirror pod",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "mirror-pod",
						Annotations: map[string]string{
							v1.MirrorPodAnnotationKey: "true",
						},
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyOnFailure,
					},
				},
			},
			expectedPodsLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filteredPods := FilterNonRecreatablePods(tt.pods)
			if len(filteredPods) != tt.expectedPodsLen {
				t.Errorf("len(filteredPods) = %v, want len = %v", len(filteredPods), tt.expectedPodsLen)
			}
		})
	}
}
