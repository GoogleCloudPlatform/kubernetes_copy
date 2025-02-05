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

package priority

import (
	"context"
	"testing"

	"k8s.io/klog/v2"

	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	admissiontesting "k8s.io/apiserver/pkg/admission/testing"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/informers"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	v1 "k8s.io/kubernetes/pkg/apis/scheduling/v1"
	"k8s.io/kubernetes/pkg/controller"
)

func addPriorityClasses(ctrl *Plugin, priorityClasses []*scheduling.PriorityClass) error {
	informerFactory := informers.NewSharedInformerFactory(nil, controller.NoResyncPeriodFunc())
	ctrl.SetExternalKubeInformerFactory(informerFactory)
	// First add the existing classes to the cache.
	for _, c := range priorityClasses {
		s := &schedulingv1.PriorityClass{}
		if err := v1.Convert_scheduling_PriorityClass_To_v1_PriorityClass(c, s, nil); err != nil {
			return err
		}
		informerFactory.Scheduling().V1().PriorityClasses().Informer().GetStore().Add(s)
	}
	return nil
}

var (
	preemptNever         = api.PreemptNever
	preemptLowerPriority = api.PreemptLowerPriority
)

var defaultClass1 = &scheduling.PriorityClass{
	TypeMeta: metav1.TypeMeta{
		Kind: "PriorityClass",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "default1",
	},
	Value:         1000,
	GlobalDefault: true,
}

var defaultClass2 = &scheduling.PriorityClass{
	TypeMeta: metav1.TypeMeta{
		Kind: "PriorityClass",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "default2",
	},
	Value:         2000,
	GlobalDefault: true,
}

var nondefaultClass1 = &scheduling.PriorityClass{
	TypeMeta: metav1.TypeMeta{
		Kind: "PriorityClass",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "nondefault1",
	},
	Value:       2000,
	Description: "Just a test priority class",
}

var systemClusterCritical = &scheduling.PriorityClass{
	TypeMeta: metav1.TypeMeta{
		Kind: "PriorityClass",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: scheduling.SystemClusterCritical,
	},
	Value:         scheduling.SystemCriticalPriority,
	GlobalDefault: true,
}

var neverPreemptionPolicyClass = &scheduling.PriorityClass{
	TypeMeta: metav1.TypeMeta{
		Kind: "PriorityClass",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "nopreemptionpolicy",
	},
	Value:            2000,
	Description:      "Just a test priority class",
	GlobalDefault:    true,
	PreemptionPolicy: &preemptNever,
}

var preemptionPolicyClass = &scheduling.PriorityClass{
	TypeMeta: metav1.TypeMeta{
		Kind: "PriorityClass",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "nopreemptionpolicy",
	},
	Value:            2000,
	Description:      "Just a test priority class",
	GlobalDefault:    true,
	PreemptionPolicy: &preemptLowerPriority,
}

func TestPriorityClassAdmission(t *testing.T) {
	var systemClass = &scheduling.PriorityClass{
		TypeMeta: metav1.TypeMeta{
			Kind: "PriorityClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: scheduling.SystemPriorityClassPrefix + "test",
		},
		Value:       scheduling.HighestUserDefinablePriority + 1,
		Description: "Name has system critical prefix",
	}

	tests := []struct {
		name            string
		existingClasses []*scheduling.PriorityClass
		newClass        *scheduling.PriorityClass
		userInfo        user.Info
		operation       admission.Operation
		expectError     bool
	}{
		{
			"create operator with default class",
			[]*scheduling.PriorityClass{},
			defaultClass1,
			nil,
			admission.Create,
			false,
		},
		{
			"create operator with one existing default class",
			[]*scheduling.PriorityClass{defaultClass1},
			defaultClass2,
			nil,
			admission.Create,
			true,
		},
		{
			"create operator with system name and value allowed by admission controller",
			[]*scheduling.PriorityClass{},
			systemClass,
			&user.DefaultInfo{
				Name: user.APIServerUser,
			},
			admission.Create,
			false,
		},
		{
			"update operator with default class",
			[]*scheduling.PriorityClass{},
			defaultClass1,
			nil,
			admission.Update,
			false,
		},
		{
			"update operator with one existing default class",
			[]*scheduling.PriorityClass{defaultClass1},
			defaultClass2,
			nil,
			admission.Update,
			true,
		},
		{
			"update operator with system name and value allowed by admission controller",
			[]*scheduling.PriorityClass{},
			systemClass,
			&user.DefaultInfo{
				Name: user.APIServerUser,
			},
			admission.Update,
			false,
		},
		{
			"update operator with different default classes",
			[]*scheduling.PriorityClass{defaultClass1},
			defaultClass2,
			nil,
			admission.Update,
			true,
		},
		{
			"delete operation with default class",
			[]*scheduling.PriorityClass{},
			defaultClass1,
			nil,
			admission.Delete,
			false,
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("starting test %q", test.name)

		ctrl := NewPlugin()
		// Add existing priority classes.
		if err := addPriorityClasses(ctrl, test.existingClasses); err != nil {
			t.Errorf("Test %q: unable to add object to informer: %v", test.name, err)
		}
		// Now add the new class.
		attrs := admission.NewAttributesRecord(
			test.newClass,
			nil,
			scheduling.Kind("PriorityClass").WithVersion("version"),
			"",
			"",
			scheduling.Resource("priorityclasses").WithVersion("version"),
			"",
			test.operation,
			&metav1.CreateOptions{},
			false,
			test.userInfo,
		)
		err := ctrl.Validate(context.TODO(), attrs, nil)
		klog.Infof("Got %v", err)
		if err != nil && !test.expectError {
			t.Errorf("Test %q: unexpected error received: %v", test.name, err)
		}
		if err == nil && test.expectError {
			t.Errorf("Test %q: expected error and no error received", test.name)
		}
	}
}

// TestDefaultPriority tests that default priority is resolved correctly.
func TestDefaultPriority(t *testing.T) {
	pcResource := scheduling.Resource("priorityclasses").WithVersion("version")
	pcKind := scheduling.Kind("PriorityClass").WithVersion("version")
	updatedDefaultClass1 := *defaultClass1
	updatedDefaultClass1.GlobalDefault = false

	tests := []struct {
		name                      string
		classesBefore             []*scheduling.PriorityClass
		classesAfter              []*scheduling.PriorityClass
		attributes                admission.Attributes
		expectedDefaultBefore     int32
		expectedDefaultNameBefore string
		expectedDefaultAfter      int32
		expectedDefaultNameAfter  string
	}{
		{
			name:                      "simple resolution with a default class",
			classesBefore:             []*scheduling.PriorityClass{defaultClass1},
			classesAfter:              []*scheduling.PriorityClass{defaultClass1},
			attributes:                nil,
			expectedDefaultBefore:     defaultClass1.Value,
			expectedDefaultNameBefore: defaultClass1.Name,
			expectedDefaultAfter:      defaultClass1.Value,
			expectedDefaultNameAfter:  defaultClass1.Name,
		},
		{
			name:                      "add a default class",
			classesBefore:             []*scheduling.PriorityClass{nondefaultClass1},
			classesAfter:              []*scheduling.PriorityClass{nondefaultClass1, defaultClass1},
			attributes:                admission.NewAttributesRecord(defaultClass1, nil, pcKind, "", defaultClass1.Name, pcResource, "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectedDefaultBefore:     scheduling.DefaultPriorityWhenNoDefaultClassExists,
			expectedDefaultNameBefore: "",
			expectedDefaultAfter:      defaultClass1.Value,
			expectedDefaultNameAfter:  defaultClass1.Name,
		},
		{
			name:                      "multiple default classes resolves to the minimum value among them",
			classesBefore:             []*scheduling.PriorityClass{defaultClass1, defaultClass2},
			classesAfter:              []*scheduling.PriorityClass{defaultClass2},
			attributes:                admission.NewAttributesRecord(nil, nil, pcKind, "", defaultClass1.Name, pcResource, "", admission.Delete, &metav1.DeleteOptions{}, false, nil),
			expectedDefaultBefore:     defaultClass1.Value,
			expectedDefaultNameBefore: defaultClass1.Name,
			expectedDefaultAfter:      defaultClass2.Value,
			expectedDefaultNameAfter:  defaultClass2.Name,
		},
		{
			name:                      "delete default priority class",
			classesBefore:             []*scheduling.PriorityClass{defaultClass1},
			classesAfter:              []*scheduling.PriorityClass{},
			attributes:                admission.NewAttributesRecord(nil, nil, pcKind, "", defaultClass1.Name, pcResource, "", admission.Delete, &metav1.DeleteOptions{}, false, nil),
			expectedDefaultBefore:     defaultClass1.Value,
			expectedDefaultNameBefore: defaultClass1.Name,
			expectedDefaultAfter:      scheduling.DefaultPriorityWhenNoDefaultClassExists,
			expectedDefaultNameAfter:  "",
		},
		{
			name:                      "update default class and remove its global default",
			classesBefore:             []*scheduling.PriorityClass{defaultClass1},
			classesAfter:              []*scheduling.PriorityClass{&updatedDefaultClass1},
			attributes:                admission.NewAttributesRecord(&updatedDefaultClass1, defaultClass1, pcKind, "", defaultClass1.Name, pcResource, "", admission.Update, &metav1.UpdateOptions{}, false, nil),
			expectedDefaultBefore:     defaultClass1.Value,
			expectedDefaultNameBefore: defaultClass1.Name,
			expectedDefaultAfter:      scheduling.DefaultPriorityWhenNoDefaultClassExists,
			expectedDefaultNameAfter:  "",
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("starting test %q", test.name)
		ctrl := NewPlugin()
		if err := addPriorityClasses(ctrl, test.classesBefore); err != nil {
			t.Errorf("Test %q: unable to add object to informer: %v", test.name, err)
		}
		pcName, defaultPriority, _, err := ctrl.getDefaultPriority()
		if err != nil {
			t.Errorf("Test %q: unexpected error while getting default priority: %v", test.name, err)
		}
		if err == nil &&
			(defaultPriority != test.expectedDefaultBefore || pcName != test.expectedDefaultNameBefore) {
			t.Errorf("Test %q: expected default priority %s(%d), but got %s(%d)",
				test.name, test.expectedDefaultNameBefore, test.expectedDefaultBefore, pcName, defaultPriority)
		}
		if test.attributes != nil {
			err := ctrl.Validate(context.TODO(), test.attributes, nil)
			if err != nil {
				t.Errorf("Test %q: unexpected error received: %v", test.name, err)
			}
		}
		if err := addPriorityClasses(ctrl, test.classesAfter); err != nil {
			t.Errorf("Test %q: unable to add object to informer: %v", test.name, err)
		}
		pcName, defaultPriority, _, err = ctrl.getDefaultPriority()
		if err != nil {
			t.Errorf("Test %q: unexpected error while getting default priority: %v", test.name, err)
		}
		if err == nil &&
			(defaultPriority != test.expectedDefaultAfter || pcName != test.expectedDefaultNameAfter) {
			t.Errorf("Test %q: expected default priority %s(%d), but got %s(%d)",
				test.name, test.expectedDefaultNameAfter, test.expectedDefaultAfter, pcName, defaultPriority)
		}
	}
}

var zeroPriority = int32(0)
var intPriority = int32(1000)

func TestPodAdmission(t *testing.T) {
	containerName := "container"

	pods := []*api.Pod{
		// pod[0]: Pod with a proper priority class.
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-priorityclass",
				Namespace: "namespace",
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: "default1",
			},
		},
		// pod[1]: Pod with no priority class
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-wo-priorityclass",
				Namespace: "namespace",
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
			},
		},
		// pod[2]: Pod with non-existing priority class
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-non-existing-priorityclass",
				Namespace: "namespace",
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: "non-existing",
			},
		},
		// pod[3]: Pod with integer value of priority
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-integer-priority",
				Namespace: "namespace",
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: "default1",
				Priority:          &intPriority,
			},
		},
		// pod[4]: Pod with a system priority class name
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-system-priority",
				Namespace: metav1.NamespaceSystem,
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: scheduling.SystemClusterCritical,
			},
		},
		// pod[5]: mirror Pod with a system priority class name
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "mirror-pod-w-system-priority",
				Namespace:   metav1.NamespaceSystem,
				Annotations: map[string]string{api.MirrorPodAnnotationKey: ""},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: "system-cluster-critical",
			},
		},
		// pod[6]: mirror Pod with integer value of priority
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "mirror-pod-w-integer-priority",
				Namespace:   "namespace",
				Annotations: map[string]string{api.MirrorPodAnnotationKey: ""},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: "default1",
				Priority:          &intPriority,
			},
		},
		// pod[7]: Pod with a system priority class name in non-system namespace
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-system-priority-in-nonsystem-namespace",
				Namespace: "non-system-namespace",
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: scheduling.SystemClusterCritical,
			},
		},
		// pod[8]: Pod with a priority value that matches the resolved priority
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-zero-priority-in-nonsystem-namespace",
				Namespace: "non-system-namespace",
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				Priority: &zeroPriority,
			},
		},
		// pod[9]: Pod with a priority value that matches the resolved default priority
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-priority-matching-default-priority",
				Namespace: "non-system-namespace",
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				Priority: &defaultClass2.Value,
			},
		},
		// pod[10]: Pod with a priority value that matches the resolved priority
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-w-priority-matching-resolved-default-priority",
				Namespace: metav1.NamespaceSystem,
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: systemClusterCritical.Name,
				Priority:          &systemClusterCritical.Value,
			},
		},
		// pod[11]: Pod without a preemption policy that matches the resolved preemption policy
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-never-preemption-policy-matching-resolved-preemption-policy",
				Namespace: metav1.NamespaceSystem,
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: neverPreemptionPolicyClass.Name,
				Priority:          &neverPreemptionPolicyClass.Value,
				PreemptionPolicy:  nil,
			},
		},
		// pod[12]: Pod with a preemption policy that matches the resolved preemption policy
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-preemption-policy-matching-resolved-preemption-policy",
				Namespace: metav1.NamespaceSystem,
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: preemptionPolicyClass.Name,
				Priority:          &preemptionPolicyClass.Value,
				PreemptionPolicy:  &preemptLowerPriority,
			},
		},
		// pod[13]: Pod with a preemption policy that doesn't match the resolved preemption policy
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-preemption-policy-not-matching-resolved-preemption-policy",
				Namespace: metav1.NamespaceSystem,
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: containerName,
					},
				},
				PriorityClassName: preemptionPolicyClass.Name,
				Priority:          &preemptionPolicyClass.Value,
				PreemptionPolicy:  &preemptNever,
			},
		},
	}

	tests := []struct {
		name            string
		existingClasses []*scheduling.PriorityClass
		// Admission controller changes pod spec. So, we take an api.Pod instead of
		// *api.Pod to avoid interfering with other tests.
		pod                    api.Pod
		expectedPriority       int32
		expectError            bool
		expectPreemptionPolicy *api.PreemptionPolicy
	}{
		{
			"Pod with priority class",
			[]*scheduling.PriorityClass{defaultClass1, nondefaultClass1},
			*pods[0],
			1000,
			false,
			nil,
		},

		{
			"Pod without priority class",
			[]*scheduling.PriorityClass{defaultClass1},
			*pods[1],
			1000,
			false,
			nil,
		},
		{
			"pod without priority class and no existing priority class",
			[]*scheduling.PriorityClass{},
			*pods[1],
			scheduling.DefaultPriorityWhenNoDefaultClassExists,
			false,
			nil,
		},
		{
			"pod without priority class and no default class",
			[]*scheduling.PriorityClass{nondefaultClass1},
			*pods[1],
			scheduling.DefaultPriorityWhenNoDefaultClassExists,
			false,
			nil,
		},
		{
			"pod with a system priority class",
			[]*scheduling.PriorityClass{systemClusterCritical},
			*pods[4],
			scheduling.SystemCriticalPriority,
			false,
			nil,
		},
		{
			"Pod with non-existing priority class",
			[]*scheduling.PriorityClass{defaultClass1, nondefaultClass1},
			*pods[2],
			0,
			true,
			nil,
		},
		{
			"pod with integer priority",
			[]*scheduling.PriorityClass{},
			*pods[3],
			0,
			true,
			nil,
		},
		{
			"mirror pod with system priority class",
			[]*scheduling.PriorityClass{systemClusterCritical},
			*pods[5],
			scheduling.SystemCriticalPriority,
			false,
			nil,
		},
		{
			"mirror pod with integer priority",
			[]*scheduling.PriorityClass{},
			*pods[6],
			0,
			true,
			nil,
		},
		{
			"pod with system critical priority in non-system namespace",
			[]*scheduling.PriorityClass{systemClusterCritical},
			*pods[7],
			scheduling.SystemCriticalPriority,
			false,
			nil,
		},
		{
			"pod with priority that matches computed priority",
			[]*scheduling.PriorityClass{nondefaultClass1},
			*pods[8],
			0,
			false,
			nil,
		},
		{
			"pod with priority that matches default priority",
			[]*scheduling.PriorityClass{defaultClass2},
			*pods[9],
			defaultClass2.Value,
			false,
			nil,
		},
		{
			"pod with priority that matches resolved priority",
			[]*scheduling.PriorityClass{systemClusterCritical},
			*pods[10],
			systemClusterCritical.Value,
			false,
			nil,
		},
		{
			"pod with nil preemtpion policy",
			[]*scheduling.PriorityClass{preemptionPolicyClass},
			*pods[11],
			preemptionPolicyClass.Value,
			false,
			nil,
		},
		{
			"pod with preemtpion policy that matches resolved preemtpion policy",
			[]*scheduling.PriorityClass{preemptionPolicyClass},
			*pods[12],
			preemptionPolicyClass.Value,
			false,
			&preemptLowerPriority,
		},
		{
			"pod with preemtpion policy that does't matches resolved preemtpion policy",
			[]*scheduling.PriorityClass{preemptionPolicyClass},
			*pods[13],
			preemptionPolicyClass.Value,
			true,
			&preemptLowerPriority,
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("starting test %q", test.name)
		ctrl := NewPlugin()
		// Add existing priority classes.
		if err := addPriorityClasses(ctrl, test.existingClasses); err != nil {
			t.Errorf("Test %q: unable to add object to informer: %v", test.name, err)
		}

		// Create pod.
		attrs := admission.NewAttributesRecord(
			&test.pod,
			nil,
			api.Kind("Pod").WithVersion("version"),
			test.pod.ObjectMeta.Namespace,
			"",
			api.Resource("pods").WithVersion("version"),
			"",
			admission.Create,
			&metav1.CreateOptions{},
			false,
			nil,
		)
		err := admissiontesting.WithReinvocationTesting(t, ctrl).Admit(context.TODO(), attrs, nil)
		klog.Infof("Got %v", err)

		if !test.expectError {
			if err != nil {
				t.Errorf("Test %q: unexpected error received: %v", test.name, err)
			} else if *test.pod.Spec.Priority != test.expectedPriority {
				t.Errorf("Test %q: expected priority is %d, but got %d.", test.name, test.expectedPriority, *test.pod.Spec.Priority)
			} else if test.pod.Spec.PreemptionPolicy != nil && test.expectPreemptionPolicy != nil && *test.pod.Spec.PreemptionPolicy != *test.expectPreemptionPolicy {
				t.Errorf("Test %q: expected preemption policy is %s, but got %s.", test.name, *test.expectPreemptionPolicy, *test.pod.Spec.PreemptionPolicy)
			}
		}
		if err == nil && test.expectError {
			t.Errorf("Test %q: expected error and no error received", test.name)
		}
	}
}
