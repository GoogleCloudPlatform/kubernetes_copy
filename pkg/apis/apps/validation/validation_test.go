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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/api/pod"
	podtest "k8s.io/kubernetes/pkg/api/pod/testing"
	"k8s.io/kubernetes/pkg/apis/apps"
	api "k8s.io/kubernetes/pkg/apis/core"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/utils/ptr"
)

type statefulSetTweak func(ss *apps.StatefulSet)

func mkStatefulSet(template *api.PodTemplate, tweaks ...statefulSetTweak) apps.StatefulSet {
	ss := apps.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
		Spec: apps.StatefulSetSpec{
			PodManagementPolicy: apps.OrderedReadyPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
			Template:            template.Template,
			UpdateStrategy:      apps.StatefulSetUpdateStrategy{Type: apps.RollingUpdateStatefulSetStrategyType},
		},
	}

	for _, tw := range tweaks {
		tw(&ss)
	}

	return ss
}

func tweakName(name string) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.ObjectMeta.Name = name
	}
}

func tweakNamespace(ns string) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.ObjectMeta.Namespace = ns
	}
}

func tweakLabels(key string, value string) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		if ss.ObjectMeta.Labels == nil {
			ss.ObjectMeta.Labels = map[string]string{}
		}
		ss.ObjectMeta.Labels[key] = value
	}
}

func tweakAnnotations(key string, value string) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		if ss.ObjectMeta.Annotations == nil {
			ss.ObjectMeta.Annotations = map[string]string{}
		}
		ss.ObjectMeta.Annotations[key] = value
	}
}

func tweakFinalizers(finalizers ...string) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.ObjectMeta.Finalizers = finalizers
	}
}

func tweakPodManagementPolicy(policy apps.PodManagementPolicyType) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.PodManagementPolicy = policy
	}
}

func tweakReplicas(replicas int32) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.Replicas = replicas
	}
}

func tweakSelectorLabels(labels map[string]string) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		if labels == nil {
			ss.Spec.Selector = nil
		} else {
			ss.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}
	}
}

func tweakTemplateRestartPolicy(rp api.RestartPolicy) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.Template.Spec.RestartPolicy = rp
	}
}

func tweakMinReadySeconds(t int32) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.MinReadySeconds = t
	}
}

func tweakOrdinalsStart(s int32) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.Ordinals = &apps.StatefulSetOrdinals{Start: s}
	}
}

func tweakPVCTemplate(pvc ...api.PersistentVolumeClaim) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.VolumeClaimTemplates = pvc
	}
}

func tweakUpdateStrategyType(t apps.StatefulSetUpdateStrategyType) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.UpdateStrategy.Type = t
	}
}

func tweakRollingUpdatePartition(partition int32) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		if ss.Spec.UpdateStrategy.RollingUpdate == nil {
			ss.Spec.UpdateStrategy.RollingUpdate = &apps.RollingUpdateStatefulSetStrategy{}
		}
		ss.Spec.UpdateStrategy.RollingUpdate.Partition = partition
	}
}

func tweakMaxUnavailable(mu intstr.IntOrString) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		if ss.Spec.UpdateStrategy.RollingUpdate == nil {
			ss.Spec.UpdateStrategy.RollingUpdate = &apps.RollingUpdateStatefulSetStrategy{}
		}
		ss.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable = ptr.To(mu)
	}
}

func tweakPVCPolicy(policy *apps.StatefulSetPersistentVolumeClaimRetentionPolicy) statefulSetTweak {
	return func(ss *apps.StatefulSet) {
		ss.Spec.PersistentVolumeClaimRetentionPolicy = policy
	}
}

type pvcPolicyTweak func(policy *apps.StatefulSetPersistentVolumeClaimRetentionPolicy)

func mkPVCPolicy(tweaks ...pvcPolicyTweak) *apps.StatefulSetPersistentVolumeClaimRetentionPolicy {
	policy := &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{}

	for _, tw := range tweaks {
		tw(policy)
	}

	return policy
}

func tweakPVCDeletedPolicy(t apps.PersistentVolumeClaimRetentionPolicyType) pvcPolicyTweak {
	return func(policy *apps.StatefulSetPersistentVolumeClaimRetentionPolicy) {
		policy.WhenDeleted = t
	}
}

func tweakPVCScalePolicy(t apps.PersistentVolumeClaimRetentionPolicyType) pvcPolicyTweak {
	return func(policy *apps.StatefulSetPersistentVolumeClaimRetentionPolicy) {
		policy.WhenScaled = t
	}
}

func TestValidateStatefulSet(t *testing.T) {
	validLabels := map[string]string{"a": "b"}
	validPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
				Containers:    []api.Container{{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent"}},
			},
		},
	}

	validHostNetPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					HostNetwork: true,
				},
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
				Containers: []api.Container{{
					Name:            "abc",
					Image:           "image",
					ImagePullPolicy: "IfNotPresent",
					Ports: []api.ContainerPort{{
						ContainerPort: 12345,
						Protocol:      api.ProtocolTCP,
					}},
				}},
			},
		},
	}

	invalidLabels := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	invalidPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
			},
			ObjectMeta: metav1.ObjectMeta{
				Labels: invalidLabels,
			},
		},
	}

	invalidTime := int64(60)
	invalidPodTemplate2 := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: api.PodSpec{
				RestartPolicy:         api.RestartPolicyAlways,
				DNSPolicy:             api.DNSClusterFirst,
				ActiveDeadlineSeconds: &invalidTime,
			},
		},
	}

	type testCase struct {
		name string
		set  apps.StatefulSet
		errs field.ErrorList
	}

	successCases := []testCase{{
		name: "alpha name",
		set:  mkStatefulSet(&validPodTemplate, tweakName("abc")),
	}, {
		name: "alphanumeric name",
		set:  mkStatefulSet(&validPodTemplate, tweakName("abc-123")),
	}, {
		name: "hostNetwork true",
		set:  mkStatefulSet(&validHostNetPodTemplate),
	}, {
		name: "parallel pod management",
		set:  mkStatefulSet(&validPodTemplate, tweakPodManagementPolicy(apps.ParallelPodManagement)),
	}, {
		name: "ordered ready pod management",
		set:  mkStatefulSet(&validPodTemplate, tweakPodManagementPolicy(apps.OrderedReadyPodManagement)),
	}, {
		name: "update strategy",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType(apps.RollingUpdateStatefulSetStrategyType),
			tweakRollingUpdatePartition(2),
		),
	}, {
		name: "PVC policy",
		set: mkStatefulSet(&validPodTemplate,
			tweakPVCPolicy(mkPVCPolicy(
				tweakPVCDeletedPolicy(apps.DeletePersistentVolumeClaimRetentionPolicyType),
				tweakPVCScalePolicy(apps.RetainPersistentVolumeClaimRetentionPolicyType),
			)),
		),
	}, {
		name: "maxUnavailable with parallel pod management",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType(apps.RollingUpdateStatefulSetStrategyType),
			tweakRollingUpdatePartition(2),
			tweakMaxUnavailable(intstr.FromInt32(2)),
		),
	}, {
		name: "ordinals.start positive value",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakOrdinalsStart(2),
		),
	},
	}

	errorCases := []testCase{{
		name: "zero-length name",
		set:  mkStatefulSet(&validPodTemplate, tweakName("")),
		errs: field.ErrorList{
			field.Required(field.NewPath("metadata", "name"), ""),
		},
	}, {
		name: "name-with-dots",
		set:  mkStatefulSet(&validPodTemplate, tweakName("abc.123")),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("metadata", "name"), "abc.123", ""),
		},
	}, {
		name: "long name",
		set:  mkStatefulSet(&validPodTemplate, tweakName(strings.Repeat("a", 64))),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("metadata", "name"), strings.Repeat("a", 64), ""),
		},
	}, {
		name: "missing-namespace",
		set:  mkStatefulSet(&validPodTemplate, tweakNamespace("")),
		errs: field.ErrorList{
			field.Required(field.NewPath("metadata", "namespace"), ""),
		},
	}, {
		name: "empty selector",
		set:  mkStatefulSet(&validPodTemplate, tweakSelectorLabels(nil)),
		errs: field.ErrorList{
			field.Required(field.NewPath("spec", "selector"), ""),
			field.Invalid(field.NewPath("spec", "template", "metadata", "labels"), nil, ""), // selector is empty, labels are not, so select doesn't match labels
		},
	}, {
		name: "selector_doesnt_match",
		set:  mkStatefulSet(&validPodTemplate, tweakSelectorLabels(map[string]string{"foo": "bar"})),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "template", "metadata", "labels"), nil, ""),
		},
	}, {
		name: "negative_replicas",
		set:  mkStatefulSet(&validPodTemplate, tweakReplicas(-1)),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "replicas"), nil, ""),
		},
	}, {
		name: "invalid_label",
		set: mkStatefulSet(&validPodTemplate,
			tweakLabels("NoUppercaseOrSpecialCharsLike=Equals", "bar"),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("metadata", "labels"), nil, ""),
		},
	}, {
		name: "invalid_label 2",
		set: mkStatefulSet(&invalidPodTemplate,
			tweakLabels("NoUppercaseOrSpecialCharsLike=Equals", "bar"),
			tweakSelectorLabels(invalidLabels),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("metadata", "labels"), nil, ""),
			field.Invalid(field.NewPath("spec", "selector"), nil, ""),
			field.Invalid(field.NewPath("spec", "selector", "matchLabels"), nil, ""),
		},
	}, {
		name: "invalid_annotation",
		set: mkStatefulSet(&validPodTemplate,
			tweakAnnotations("NoUppercaseOrSpecialCharsLike=Equals", "bar"),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("metadata", "annotations"), nil, ""),
		},
	}, {
		name: "invalid restart policy 1",
		set:  mkStatefulSet(&validPodTemplate, tweakTemplateRestartPolicy(api.RestartPolicyOnFailure)),
		errs: field.ErrorList{
			field.NotSupported[string](field.NewPath("spec", "template", "spec", "restartPolicy"), nil, nil),
		},
	}, {
		name: "invalid restart policy 2",
		set:  mkStatefulSet(&validPodTemplate, tweakTemplateRestartPolicy(api.RestartPolicyNever)),
		errs: field.ErrorList{
			field.NotSupported[string](field.NewPath("spec", "template", "spec", "restartPolicy"), nil, nil),
		},
	}, {
		name: "empty restart policy",
		set:  mkStatefulSet(&validPodTemplate, tweakTemplateRestartPolicy("")),
		errs: field.ErrorList{
			field.NotSupported[string](field.NewPath("spec", "template", "spec", "restartPolicy"), nil, nil),
		},
	}, {
		name: "invalid update strategy",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType("foo"),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "updateStrategy"), nil, ""),
		},
	}, {
		name: "empty update strategy",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType(""),
		),
		errs: field.ErrorList{
			field.Required(field.NewPath("spec", "updateStrategy"), ""),
		},
	}, {
		name: "invalid rolling update",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType(apps.OnDeleteStatefulSetStrategyType),
			tweakRollingUpdatePartition(1),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "updateStrategy", "rollingUpdate"), nil, ""),
		},
	}, {
		name: "negative parition",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakRollingUpdatePartition(-1),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "updateStrategy", "rollingUpdate", "partition"), nil, ""),
		},
	}, {
		name: "empty pod management policy",
		set: mkStatefulSet(&validPodTemplate,
			tweakPodManagementPolicy(""),
			tweakReplicas(3),
		),
		errs: field.ErrorList{
			field.Required(field.NewPath("spec", "podManagementPolicy"), ""),
		},
	}, {
		name: "invalid pod management policy",
		set:  mkStatefulSet(&validPodTemplate, tweakPodManagementPolicy("foo")),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "podManagementPolicy"), nil, ""),
		},
	}, {
		name: "set active deadline seconds",
		set:  mkStatefulSet(&invalidPodTemplate2, tweakReplicas(3)),
		errs: field.ErrorList{
			field.Forbidden(field.NewPath("spec", "template", "spec", "activeDeadlineSeconds"), ""),
		},
	}, {
		name: "empty PersistentVolumeClaimRetentionPolicy",
		set: mkStatefulSet(&validPodTemplate,
			tweakPVCPolicy(mkPVCPolicy()),
		),
		errs: field.ErrorList{
			field.NotSupported[string](field.NewPath("spec", "persistentVolumeClaimRetentionPolicy", "whenDeleted"), nil, nil),
			field.NotSupported[string](field.NewPath("spec", "persistentVolumeClaimRetentionPolicy", "whenScaled"), nil, nil),
		},
	}, {
		name: "invalid PersistentVolumeClaimRetentionPolicy",
		set: mkStatefulSet(&validPodTemplate,
			tweakPVCPolicy(mkPVCPolicy(
				tweakPVCDeletedPolicy("invalid-retention-policy"),
				tweakPVCScalePolicy("invalid-retention-policy"),
			)),
		),
		errs: field.ErrorList{
			field.NotSupported[string](field.NewPath("spec", "persistentVolumeClaimRetentionPolicy", "whenDeleted"), nil, nil),
			field.NotSupported[string](field.NewPath("spec", "persistentVolumeClaimRetentionPolicy", "whenScaled"), nil, nil),
		},
	}, {
		name: "zero maxUnavailable",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType(apps.RollingUpdateStatefulSetStrategyType),
			tweakMaxUnavailable(intstr.FromInt32(0)),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "updateStrategy", "rollingUpdate", "maxUnavailable"), nil, ""),
		},
	}, {
		name: "zero percent maxUnavailable",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType(apps.RollingUpdateStatefulSetStrategyType),
			tweakMaxUnavailable(intstr.FromString("0%")),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "updateStrategy", "rollingUpdate", "maxUnavailable"), nil, ""),
		},
	}, {
		name: "greater than 100 percent maxUnavailable",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakUpdateStrategyType(apps.RollingUpdateStatefulSetStrategyType),
			tweakMaxUnavailable(intstr.FromString("101%")),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "updateStrategy", "rollingUpdate", "maxUnavailable"), nil, ""),
		},
	}, {
		name: "invalid ordinals.start",
		set: mkStatefulSet(&validPodTemplate,
			tweakReplicas(3),
			tweakOrdinalsStart(-2),
		),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "ordinals.start"), nil, ""),
		},
	},
	}

	cmpOpts := []cmp.Option{cmpopts.IgnoreFields(field.Error{}, "BadValue", "Detail"), cmpopts.SortSlices(func(a, b *field.Error) bool { return a.Error() < b.Error() })}
	for _, testCase := range append(successCases, errorCases...) {
		name := testCase.name
		var testTitle string
		if len(testCase.errs) == 0 {
			testTitle = fmt.Sprintf("success case %s", name)
		} else {
			testTitle = fmt.Sprintf("error case %s", name)
		}

		t.Run(testTitle, func(t *testing.T) {
			errs := ValidateStatefulSet(&testCase.set, pod.GetValidationOptionsFromPodTemplate(&testCase.set.Spec.Template, nil))
			wantErrs := testCase.errs
			if diff := cmp.Diff(wantErrs, errs, cmpOpts...); diff != "" {
				t.Errorf("Unexpected validation errors (-want,+got):\n%s", diff)
			}
		})
	}
}

// generateStatefulSetSpec generates a valid StatefulSet spec
func generateStatefulSetSpec(minSeconds int32) *apps.StatefulSetSpec {
	labels := map[string]string{"a": "b"}
	podTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
			},
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
				Containers:    []api.Container{{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent"}},
			},
		},
	}
	ss := &apps.StatefulSetSpec{
		PodManagementPolicy: "OrderedReady",
		Selector:            &metav1.LabelSelector{MatchLabels: labels},
		Template:            podTemplate.Template,
		Replicas:            3,
		UpdateStrategy:      apps.StatefulSetUpdateStrategy{Type: apps.RollingUpdateStatefulSetStrategyType},
		MinReadySeconds:     minSeconds,
	}
	return ss
}

// TestValidateStatefulSetMinReadySeconds tests the StatefulSet Spec's minReadySeconds field
func TestValidateStatefulSetMinReadySeconds(t *testing.T) {
	testCases := map[string]struct {
		ss        *apps.StatefulSetSpec
		expectErr bool
	}{
		"valid : minReadySeconds enabled, zero": {
			ss:        generateStatefulSetSpec(0),
			expectErr: false,
		},
		"invalid : minReadySeconds enabled, negative": {
			ss:        generateStatefulSetSpec(-1),
			expectErr: true,
		},
		"valid : minReadySeconds enabled, very large value": {
			ss:        generateStatefulSetSpec(2147483647),
			expectErr: false,
		},
		"invalid : minReadySeconds enabled, large negative": {
			ss:        generateStatefulSetSpec(-2147483648),
			expectErr: true,
		},
	}
	for tcName, tc := range testCases {
		t.Run(tcName, func(t *testing.T) {
			errs := ValidateStatefulSetSpec(tc.ss, field.NewPath("spec", "minReadySeconds"),
				corevalidation.PodValidationOptions{})
			if tc.expectErr && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !tc.expectErr && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateStatefulSetStatus(t *testing.T) {
	observedGenerationMinusOne := int64(-1)
	collisionCountMinusOne := int32(-1)
	tests := []struct {
		name               string
		replicas           int32
		readyReplicas      int32
		currentReplicas    int32
		updatedReplicas    int32
		availableReplicas  int32
		observedGeneration *int64
		collisionCount     *int32
		expectedErr        bool
	}{{
		name:            "valid status",
		replicas:        3,
		readyReplicas:   3,
		currentReplicas: 2,
		updatedReplicas: 1,
		expectedErr:     false,
	}, {
		name:            "invalid replicas",
		replicas:        -1,
		readyReplicas:   3,
		currentReplicas: 2,
		updatedReplicas: 1,
		expectedErr:     true,
	}, {
		name:            "invalid readyReplicas",
		replicas:        3,
		readyReplicas:   -1,
		currentReplicas: 2,
		updatedReplicas: 1,
		expectedErr:     true,
	}, {
		name:            "invalid currentReplicas",
		replicas:        3,
		readyReplicas:   3,
		currentReplicas: -1,
		updatedReplicas: 1,
		expectedErr:     true,
	}, {
		name:            "invalid updatedReplicas",
		replicas:        3,
		readyReplicas:   3,
		currentReplicas: 2,
		updatedReplicas: -1,
		expectedErr:     true,
	}, {
		name:               "invalid observedGeneration",
		replicas:           3,
		readyReplicas:      3,
		currentReplicas:    2,
		updatedReplicas:    1,
		observedGeneration: &observedGenerationMinusOne,
		expectedErr:        true,
	}, {
		name:            "invalid collisionCount",
		replicas:        3,
		readyReplicas:   3,
		currentReplicas: 2,
		updatedReplicas: 1,
		collisionCount:  &collisionCountMinusOne,
		expectedErr:     true,
	}, {
		name:            "readyReplicas greater than replicas",
		replicas:        3,
		readyReplicas:   4,
		currentReplicas: 2,
		updatedReplicas: 1,
		expectedErr:     true,
	}, {
		name:            "currentReplicas greater than replicas",
		replicas:        3,
		readyReplicas:   3,
		currentReplicas: 4,
		updatedReplicas: 1,
		expectedErr:     true,
	}, {
		name:            "updatedReplicas greater than replicas",
		replicas:        3,
		readyReplicas:   3,
		currentReplicas: 2,
		updatedReplicas: 4,
		expectedErr:     true,
	}, {
		name:              "invalid: number of available replicas",
		replicas:          3,
		readyReplicas:     3,
		currentReplicas:   2,
		availableReplicas: int32(-1),
		expectedErr:       true,
	}, {
		name:              "invalid: available replicas greater than replicas",
		replicas:          3,
		readyReplicas:     3,
		currentReplicas:   2,
		availableReplicas: int32(4),
		expectedErr:       true,
	}, {
		name:              "invalid: available replicas greater than ready replicas",
		replicas:          3,
		readyReplicas:     2,
		currentReplicas:   2,
		availableReplicas: int32(3),
		expectedErr:       true,
	},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := apps.StatefulSetStatus{
				Replicas:           test.replicas,
				ReadyReplicas:      test.readyReplicas,
				CurrentReplicas:    test.currentReplicas,
				UpdatedReplicas:    test.updatedReplicas,
				ObservedGeneration: test.observedGeneration,
				CollisionCount:     test.collisionCount,
				AvailableReplicas:  test.availableReplicas,
			}

			errs := ValidateStatefulSetStatus(&status, field.NewPath("status"))
			if hasErr := len(errs) > 0; hasErr != test.expectedErr {
				t.Errorf("%s: expected error: %t, got error: %t\nerrors: %s", test.name, test.expectedErr, hasErr, errs.ToAggregate().Error())
			}
		})
	}
}

func TestValidateStatefulSetUpdate(t *testing.T) {
	validLabels := map[string]string{"a": "b"}
	validLabels2 := map[string]string{"c": "d"}
	validPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
				Containers:    []api.Container{{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent"}},
			},
		},
	}
	validPodTemplate2 := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels2,
			},
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
				Containers:    []api.Container{{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent"}},
			},
		},
	}

	storageClass := "storage-class1"
	storageClass2 := "storage-class2"

	validPVCTemplate := api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-abc",
		},
		Spec: api.PersistentVolumeClaimSpec{
			StorageClassName: &storageClass,
			Resources: api.VolumeResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	validPVCTemplateChangedSize := *validPVCTemplate.DeepCopy()
	validPVCTemplateChangedSize.Spec.Resources.Requests[api.ResourceStorage] = resource.MustParse("2Gi")

	validPVCTemplateChangedClass := *validPVCTemplate.DeepCopy()
	validPVCTemplateChangedClass.Spec.StorageClassName = &storageClass2

	validPVCTemplate2 := api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-abc2",
		},
		Spec: api.PersistentVolumeClaimSpec{
			StorageClassName: &storageClass2,
			Resources: api.VolumeResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}

	addContainersValidTemplate := validPodTemplate.DeepCopy()
	addContainersValidTemplate.Template.Spec.Containers = append(addContainersValidTemplate.Template.Spec.Containers,
		api.Container{Name: "def", Image: "image2", ImagePullPolicy: "IfNotPresent"})
	if len(addContainersValidTemplate.Template.Spec.Containers) != len(validPodTemplate.Template.Spec.Containers)+1 {
		t.Errorf("failure during test setup: template %v should have more containers than template %v", addContainersValidTemplate, validPodTemplate)
	}

	type testCase struct {
		name   string
		old    apps.StatefulSet
		update apps.StatefulSet
		errs   field.ErrorList
	}

	successCases := []testCase{{
		name:   "update replica count",
		old:    mkStatefulSet(&validPodTemplate),
		update: mkStatefulSet(&validPodTemplate, tweakReplicas(3)),
	}, {
		name:   "update containers 1",
		old:    mkStatefulSet(&validPodTemplate),
		update: mkStatefulSet(addContainersValidTemplate),
	}, {
		name:   "update containers 2",
		old:    mkStatefulSet(addContainersValidTemplate),
		update: mkStatefulSet(&validPodTemplate),
	}, {
		name: "update containers and pvc retention policy 1",
		old:  mkStatefulSet(addContainersValidTemplate),
		update: mkStatefulSet(&validPodTemplate,
			tweakPVCPolicy(mkPVCPolicy(
				tweakPVCDeletedPolicy(apps.RetainPersistentVolumeClaimRetentionPolicyType),
				tweakPVCScalePolicy(apps.RetainPersistentVolumeClaimRetentionPolicyType),
			)),
		),
	}, {
		name: "update containers and pvc retention policy 2",
		old: mkStatefulSet(&validPodTemplate,
			tweakPVCPolicy(mkPVCPolicy(
				tweakPVCScalePolicy(apps.RetainPersistentVolumeClaimRetentionPolicyType),
			)),
		),
		update: mkStatefulSet(&validPodTemplate),
	}, {
		name: "update update strategy",
		old:  mkStatefulSet(&validPodTemplate),
		update: mkStatefulSet(&validPodTemplate,
			tweakUpdateStrategyType(apps.OnDeleteStatefulSetStrategyType),
		),
	}, {
		name:   "update min ready seconds 1",
		old:    mkStatefulSet(&validPodTemplate),
		update: mkStatefulSet(&validPodTemplate, tweakMinReadySeconds(10)),
	}, {
		name:   "update min ready seconds 2",
		old:    mkStatefulSet(&validPodTemplate, tweakMinReadySeconds(5)),
		update: mkStatefulSet(&validPodTemplate, tweakMinReadySeconds(10)),
	}, {
		name:   "update existing instance with now-invalid name",
		old:    mkStatefulSet(&validPodTemplate, tweakFinalizers("final")),
		update: mkStatefulSet(&validPodTemplate, tweakFinalizers()),
	}, {
		name:   "update existing instance with .spec.ordinals.start",
		old:    mkStatefulSet(&validPodTemplate),
		update: mkStatefulSet(&validPodTemplate, tweakOrdinalsStart(3)),
	},
	}

	errorCases := []testCase{{
		name:   "update name",
		old:    mkStatefulSet(&validPodTemplate, tweakName("abc")),
		update: mkStatefulSet(&validPodTemplate, tweakName("abc2")),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("metadata", "name"), nil, ""),
		},
	}, {
		name:   "update namespace",
		old:    mkStatefulSet(&validPodTemplate, tweakNamespace(metav1.NamespaceDefault)),
		update: mkStatefulSet(&validPodTemplate, tweakNamespace(metav1.NamespaceDefault+"1")),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("metadata", "namespace"), nil, ""),
		},
	}, {
		name: "update selector",
		old:  mkStatefulSet(&validPodTemplate, tweakSelectorLabels(validLabels)),
		update: mkStatefulSet(&validPodTemplate2,
			tweakSelectorLabels(validLabels2),
		),
		errs: field.ErrorList{
			field.Forbidden(field.NewPath("spec"), ""),
		},
	}, {
		name:   "update pod management policy 1",
		old:    mkStatefulSet(&validPodTemplate, tweakPodManagementPolicy("")),
		update: mkStatefulSet(&validPodTemplate, tweakPodManagementPolicy(apps.OrderedReadyPodManagement)),
		errs: field.ErrorList{
			field.Forbidden(field.NewPath("spec"), ""),
		},
	}, {
		name:   "update pod management policy 2",
		old:    mkStatefulSet(&validPodTemplate, tweakPodManagementPolicy(apps.ParallelPodManagement)),
		update: mkStatefulSet(&validPodTemplate, tweakPodManagementPolicy(apps.OrderedReadyPodManagement)),
		errs: field.ErrorList{
			field.Forbidden(field.NewPath("spec"), ""),
		},
	}, {
		name:   "update to negative replicas",
		old:    mkStatefulSet(&validPodTemplate),
		update: mkStatefulSet(&validPodTemplate, tweakReplicas(-1)),
		errs: field.ErrorList{
			field.Invalid(field.NewPath("spec", "replicas"), nil, ""),
		},
	}, {
		name:   "update pvc template size",
		old:    mkStatefulSet(&validPodTemplate, tweakPVCTemplate(validPVCTemplate)),
		update: mkStatefulSet(&validPodTemplate, tweakPVCTemplate(validPVCTemplateChangedSize)),
		errs: field.ErrorList{
			field.Forbidden(field.NewPath("spec"), ""),
		},
	}, {
		name:   "update pvc template storage class",
		old:    mkStatefulSet(&validPodTemplate, tweakPVCTemplate(validPVCTemplate)),
		update: mkStatefulSet(&validPodTemplate, tweakPVCTemplate(validPVCTemplateChangedClass)),
		errs: field.ErrorList{
			field.Forbidden(field.NewPath("spec"), ""),
		},
	}, {
		name:   "add new pvc template",
		old:    mkStatefulSet(&validPodTemplate, tweakPVCTemplate(validPVCTemplate)),
		update: mkStatefulSet(&validPodTemplate, tweakPVCTemplate(validPVCTemplate, validPVCTemplate2)),
		errs: field.ErrorList{
			field.Forbidden(field.NewPath("spec"), ""),
		},
	},
	}

	cmpOpts := []cmp.Option{
		cmpopts.IgnoreFields(field.Error{}, "BadValue", "Detail"),
		cmpopts.SortSlices(func(a, b *field.Error) bool { return a.Error() < b.Error() }),
		cmpopts.EquateEmpty(),
	}
	for _, testCase := range append(successCases, errorCases...) {
		name := testCase.name
		var testTitle string
		if len(testCase.errs) == 0 {
			testTitle = fmt.Sprintf("success case %s", name)
		} else {
			testTitle = fmt.Sprintf("error case %s", name)
		}

		t.Run(testTitle, func(t *testing.T) {
			testCase.old.ObjectMeta.ResourceVersion = "1"
			testCase.update.ObjectMeta.ResourceVersion = "1"

			errs := ValidateStatefulSetUpdate(&testCase.update, &testCase.old, pod.GetValidationOptionsFromPodTemplate(&testCase.update.Spec.Template, &testCase.old.Spec.Template))
			wantErrs := testCase.errs
			if diff := cmp.Diff(wantErrs, errs, cmpOpts...); diff != "" {
				t.Errorf("Unexpected validation errors (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestValidateControllerRevision(t *testing.T) {
	newControllerRevision := func(name, namespace string, data runtime.RawExtension, revision int64) apps.ControllerRevision {
		return apps.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Data:     data,
			Revision: revision,
		}
	}

	podTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
			},
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"a": "b"},
			},
		},
	}

	ss := mkStatefulSet(&podTemplate)
	ssJSON, _ := json.Marshal(ss)
	raw := runtime.RawExtension{Raw: ssJSON}

	var (
		valid       = newControllerRevision("validname", "validns", raw, 0)
		badRevision = newControllerRevision("validname", "validns", raw, -1)
		emptyName   = newControllerRevision("", "validns", raw, 0)
		invalidName = newControllerRevision("NoUppercaseOrSpecialCharsLike=Equals", "validns", raw, 0)
		emptyNs     = newControllerRevision("validname", "", raw, 100)
		invalidNs   = newControllerRevision("validname", "NoUppercaseOrSpecialCharsLike=Equals", raw, 100)
		nilData     = newControllerRevision("validname", "NoUppercaseOrSpecialCharsLike=Equals", runtime.RawExtension{}, 100)
	)

	tests := map[string]struct {
		history apps.ControllerRevision
		isValid bool
	}{
		"valid":             {valid, true},
		"negative revision": {badRevision, false},
		"empty name":        {emptyName, false},
		"invalid name":      {invalidName, false},
		"empty namespace":   {emptyNs, false},
		"invalid namespace": {invalidNs, false},
		"nil data":          {nilData, false},
		"json error":        {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`{`)}, 0), false},
		"json bool":         {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`true`)}, 0), false},
		"json int":          {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`0`)}, 0), false},
		"json float":        {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`0.5`)}, 0), false},
		"json null":         {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`null`)}, 0), false},
		"json array":        {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`[]`)}, 0), false},
		"json string":       {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`"test"`)}, 0), false},
		"json object":       {newControllerRevision("validname", "validns", runtime.RawExtension{Raw: []byte(`{}`)}, 0), true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			errs := ValidateControllerRevisionCreate(&tc.history)
			if tc.isValid && len(errs) > 0 {
				t.Errorf("%v: unexpected error: %v", name, errs)
			}
			if !tc.isValid && len(errs) == 0 {
				t.Errorf("%v: unexpected non-error", name)
			}
		})
	}
}

func TestValidateControllerRevisionUpdate(t *testing.T) {
	newControllerRevision := func(version, name, namespace string, data runtime.RawExtension, revision int64) apps.ControllerRevision {
		return apps.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				ResourceVersion: version,
			},
			Data:     data,
			Revision: revision,
		}
	}

	podTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSClusterFirst,
			},
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"a": "b"},
			},
		},
	}

	ss := mkStatefulSet(&podTemplate, tweakName("abc"))
	modifiedss := mkStatefulSet(&podTemplate, tweakName("cdf"))

	ssJSON, _ := json.Marshal(ss)
	modifiedSSJSON, _ := json.Marshal(modifiedss)
	raw := runtime.RawExtension{Raw: ssJSON}
	modifiedRaw := runtime.RawExtension{Raw: modifiedSSJSON}

	var (
		valid           = newControllerRevision("1", "validname", "validns", raw, 0)
		noVersion       = newControllerRevision("", "validname", "validns", raw, 0)
		changedData     = newControllerRevision("1", "validname", "validns", modifiedRaw, 0)
		changedRevision = newControllerRevision("1", "validname", "validns", raw, 1)
	)

	cases := []struct {
		name       string
		newHistory apps.ControllerRevision
		oldHistory apps.ControllerRevision
		isValid    bool
	}{{
		name:       "valid",
		newHistory: valid,
		oldHistory: valid,
		isValid:    true,
	}, {
		name:       "invalid",
		newHistory: noVersion,
		oldHistory: valid,
		isValid:    false,
	}, {
		name:       "changed data",
		newHistory: changedData,
		oldHistory: valid,
		isValid:    false,
	}, {
		name:       "changed revision",
		newHistory: changedRevision,
		oldHistory: valid,
		isValid:    true,
	},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateControllerRevisionUpdate(&tc.newHistory, &tc.oldHistory)
			if tc.isValid && len(errs) > 0 {
				t.Errorf("%v: unexpected error: %v", tc.name, errs)
			}
			if !tc.isValid && len(errs) == 0 {
				t.Errorf("%v: unexpected non-error", tc.name)
			}
		})
	}
}

func TestValidateDaemonSetStatusUpdate(t *testing.T) {
	type dsUpdateTest struct {
		old    apps.DaemonSet
		update apps.DaemonSet
	}

	successCases := []dsUpdateTest{{
		old: apps.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Status: apps.DaemonSetStatus{
				CurrentNumberScheduled: 1,
				NumberMisscheduled:     2,
				DesiredNumberScheduled: 3,
				NumberReady:            1,
				UpdatedNumberScheduled: 1,
				NumberAvailable:        1,
				NumberUnavailable:      2,
			},
		},
		update: apps.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Status: apps.DaemonSetStatus{
				CurrentNumberScheduled: 1,
				NumberMisscheduled:     1,
				DesiredNumberScheduled: 3,
				NumberReady:            1,
				UpdatedNumberScheduled: 1,
				NumberAvailable:        1,
				NumberUnavailable:      2,
			},
		},
	},
	}

	for _, successCase := range successCases {
		successCase.old.ObjectMeta.ResourceVersion = "1"
		successCase.update.ObjectMeta.ResourceVersion = "1"
		if errs := ValidateDaemonSetStatusUpdate(&successCase.update, &successCase.old); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	errorCases := map[string]dsUpdateTest{
		"negative values": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: -1,
					NumberMisscheduled:     -1,
					DesiredNumberScheduled: -3,
					NumberReady:            -1,
					ObservedGeneration:     -3,
					UpdatedNumberScheduled: -1,
					NumberAvailable:        -1,
					NumberUnavailable:      -2,
				},
			},
		},
		"negative CurrentNumberScheduled": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: -1,
					NumberMisscheduled:     1,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
		},
		"negative NumberMisscheduled": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     -1,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
		},
		"negative DesiredNumberScheduled": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     1,
					DesiredNumberScheduled: -3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
		},
		"negative NumberReady": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     1,
					DesiredNumberScheduled: 3,
					NumberReady:            -1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
		},
		"negative ObservedGeneration": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     1,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     -3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
		},
		"negative UpdatedNumberScheduled": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     1,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: -1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
		},
		"negative NumberAvailable": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     1,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        -1,
					NumberUnavailable:      2,
				},
			},
		},
		"negative NumberUnavailable": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     2,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      2,
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "abc",
					Namespace:       metav1.NamespaceDefault,
					ResourceVersion: "10",
				},
				Status: apps.DaemonSetStatus{
					CurrentNumberScheduled: 1,
					NumberMisscheduled:     1,
					DesiredNumberScheduled: 3,
					NumberReady:            1,
					ObservedGeneration:     3,
					UpdatedNumberScheduled: 1,
					NumberAvailable:        1,
					NumberUnavailable:      -2,
				},
			},
		},
	}

	for testName, errorCase := range errorCases {
		if errs := ValidateDaemonSetStatusUpdate(&errorCase.update, &errorCase.old); len(errs) == 0 {
			t.Errorf("expected failure: %s", testName)
		}
	}
}

func TestValidateDaemonSetUpdate(t *testing.T) {
	validSelector := map[string]string{"a": "b"}
	validSelector2 := map[string]string{"c": "d"}
	invalidSelector := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}

	validPodSpecAbc := podtest.MakePodSpec(
		podtest.SetContainers(podtest.MakeContainer("abc")))
	validPodSpecDef := podtest.MakePodSpec(
		podtest.SetContainers(podtest.MakeContainer("def")))
	validPodSpecNodeSelector := podtest.MakePodSpec(
		podtest.SetNodeSelector(validSelector),
		podtest.SetNodeName("xyz"))
	validPodSpecVolume := podtest.MakePodSpec(
		podtest.SetVolumes(api.Volume{Name: "gcepd", VolumeSource: api.VolumeSource{GCEPersistentDisk: &api.GCEPersistentDiskVolumeSource{PDName: "my-PD", FSType: "ext4", Partition: 1, ReadOnly: false}}}))

	validPodTemplateAbc := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: validPodSpecAbc,
		},
	}
	validPodTemplateAbcSemanticallyEqual := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: validPodSpecAbc,
		},
	}
	validPodTemplateAbcSemanticallyEqual.Template.Spec.ImagePullSecrets = []api.LocalObjectReference{}
	validPodTemplateNodeSelector := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: validPodSpecNodeSelector,
		},
	}
	validPodTemplateAbc2 := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector2,
			},
			Spec: validPodSpecAbc,
		},
	}
	validPodTemplateDef := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector2,
			},
			Spec: validPodSpecDef,
		},
	}
	invalidPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			// no containers specified
			Spec: podtest.MakePodSpec(podtest.SetContainers()),
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
		},
	}
	readWriteVolumePodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: validPodSpecVolume,
		},
	}
	type dsUpdateTest struct {
		old            apps.DaemonSet
		update         apps.DaemonSet
		expectedErrNum int
	}
	successCases := map[string]dsUpdateTest{
		"no change": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
		},
		"change template and selector": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 2,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector2},
					TemplateGeneration: 3,
					Template:           validPodTemplateAbc2.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
		},
		"change template": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 3,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 4,
					Template:           validPodTemplateNodeSelector.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
		},
		"change container image name": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector2},
					TemplateGeneration: 2,
					Template:           validPodTemplateDef.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
		},
		"change update strategy": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 4,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 4,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.RollingUpdateDaemonSetStrategyType,
						RollingUpdate: &apps.RollingUpdateDaemonSet{
							MaxUnavailable: intstr.FromInt32(1),
						},
					},
				},
			},
		},
		"unchanged templateGeneration upon semantically equal template update": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 4,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 4,
					Template:           validPodTemplateAbcSemanticallyEqual.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.RollingUpdateDaemonSetStrategyType,
						RollingUpdate: &apps.RollingUpdateDaemonSet{
							MaxUnavailable: intstr.FromInt32(1),
						},
					},
				},
			},
		},
		"Read-write volume verification": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 2,
					Template:           readWriteVolumePodTemplate.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
		},
	}
	for testName, successCase := range successCases {
		t.Run(testName, func(t *testing.T) {
			// ResourceVersion is required for updates.
			successCase.old.ObjectMeta.ResourceVersion = "1"
			successCase.update.ObjectMeta.ResourceVersion = "2"
			// Check test setup
			if successCase.expectedErrNum > 0 {
				t.Errorf("%q has incorrect test setup with expectedErrNum %d, expected no error", testName, successCase.expectedErrNum)
			}
			if len(successCase.old.ObjectMeta.ResourceVersion) == 0 || len(successCase.update.ObjectMeta.ResourceVersion) == 0 {
				t.Errorf("%q has incorrect test setup with no resource version set", testName)
			}
			if errs := ValidateDaemonSetUpdate(&successCase.update, &successCase.old, corevalidation.PodValidationOptions{}); len(errs) != 0 {
				t.Errorf("%q expected no error, but got: %v", testName, errs)
			}
		})
	}
	errorCases := map[string]dsUpdateTest{
		"change daemon name": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			expectedErrNum: 1,
		},
		"invalid selector": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: invalidSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			expectedErrNum: 1,
		},
		"invalid pod": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 2,
					Template:           invalidPodTemplate.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			expectedErrNum: 1,
		},
		"invalid update strategy": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: 1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: "Random",
					},
				},
			},
			expectedErrNum: 1,
		},
		"negative templateGeneration": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: -1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					TemplateGeneration: -1,
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			expectedErrNum: 1,
		},
		"decreased templateGeneration": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					TemplateGeneration: 2,
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					TemplateGeneration: 1,
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			expectedErrNum: 1,
		},
		"unchanged templateGeneration upon template update": {
			old: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					TemplateGeneration: 2,
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector},
					Template:           validPodTemplateAbc.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			update: apps.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DaemonSetSpec{
					TemplateGeneration: 2,
					Selector:           &metav1.LabelSelector{MatchLabels: validSelector2},
					Template:           validPodTemplateAbc2.Template,
					UpdateStrategy: apps.DaemonSetUpdateStrategy{
						Type: apps.OnDeleteDaemonSetStrategyType,
					},
				},
			},
			expectedErrNum: 1,
		},
	}
	for testName, errorCase := range errorCases {
		t.Run(testName, func(t *testing.T) {
			// ResourceVersion is required for updates.
			errorCase.old.ObjectMeta.ResourceVersion = "1"
			errorCase.update.ObjectMeta.ResourceVersion = "2"
			// Check test setup
			if errorCase.expectedErrNum <= 0 {
				t.Errorf("%q has incorrect test setup with expectedErrNum %d, expected at least one error", testName, errorCase.expectedErrNum)
			}
			if len(errorCase.old.ObjectMeta.ResourceVersion) == 0 || len(errorCase.update.ObjectMeta.ResourceVersion) == 0 {
				t.Errorf("%q has incorrect test setup with no resource version set", testName)
			}
			// Run the tests
			if errs := ValidateDaemonSetUpdate(&errorCase.update, &errorCase.old, corevalidation.PodValidationOptions{}); len(errs) != errorCase.expectedErrNum {
				t.Errorf("%q expected %d errors, but got %d error: %v", testName, errorCase.expectedErrNum, len(errs), errs)
			} else {
				t.Logf("(PASS) %q got errors %v", testName, errs)
			}
		})
	}
}

func TestValidateDaemonSet(t *testing.T) {
	validSelector := map[string]string{"a": "b"}
	validPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	validHostNetPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetSecurityContext(&api.PodSecurityContext{
					HostNetwork: true,
				}),
				podtest.SetContainers(podtest.MakeContainer("abc",
					podtest.SetContainerPorts(api.ContainerPort{
						ContainerPort: 12345,
						Protocol:      api.ProtocolTCP,
					}))),
			),
		},
	}
	invalidSelector := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	invalidPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			Spec: podtest.MakePodSpec(podtest.SetLabels(invalidSelector)),
		},
	}
	successCases := []apps.DaemonSet{{
		ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
		Spec: apps.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: validSelector},
			Template: validPodTemplate.Template,
			UpdateStrategy: apps.DaemonSetUpdateStrategy{
				Type: apps.OnDeleteDaemonSetStrategyType,
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "abc-123", Namespace: metav1.NamespaceDefault},
		Spec: apps.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: validSelector},
			Template: validPodTemplate.Template,
			UpdateStrategy: apps.DaemonSetUpdateStrategy{
				Type: apps.OnDeleteDaemonSetStrategyType,
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "hostnet", Namespace: metav1.NamespaceDefault},
		Spec: apps.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: validSelector},
			Template: validHostNetPodTemplate.Template,
			UpdateStrategy: apps.DaemonSetUpdateStrategy{
				Type: apps.OnDeleteDaemonSetStrategyType,
			},
		},
	},
	}
	for _, successCase := range successCases {
		if errs := ValidateDaemonSet(&successCase, corevalidation.PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := map[string]apps.DaemonSet{
		"zero-length ID": {
			ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
				Template: validPodTemplate.Template,
			},
		},
		"missing-namespace": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc-123"},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
				Template: validPodTemplate.Template,
			},
		},
		"nil selector": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.DaemonSetSpec{
				Template: validPodTemplate.Template,
			},
		},
		"empty selector": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{},
				Template: validPodTemplate.Template,
			},
		},
		"selector_doesnt_match": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
				Template: validPodTemplate.Template,
			},
		},
		"invalid template": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
			},
		},
		"invalid_label": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Labels: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
				Template: validPodTemplate.Template,
			},
		},
		"invalid_label 2": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Labels: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: apps.DaemonSetSpec{
				Template: invalidPodTemplate.Template,
			},
		},
		"invalid_annotation": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Annotations: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
				Template: validPodTemplate.Template,
			},
		},
		"invalid restart policy 1": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
				Template: api.PodTemplateSpec{
					Spec: podtest.MakePodSpec(podtest.SetRestartPolicy(api.RestartPolicyOnFailure), podtest.SetLabels(validSelector)),
				},
			},
		},
		"invalid restart policy 2": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
				Template: api.PodTemplateSpec{
					Spec: podtest.MakePodSpec(podtest.SetRestartPolicy(api.RestartPolicyNever), podtest.SetLabels(validSelector)),
				},
			},
		},
		"template may not contain ephemeral containers": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validSelector},
				Template: api.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: validSelector,
					},
					Spec: podtest.MakePodSpec(
						podtest.SetEphemeralContainers(api.EphemeralContainer{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "debug", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}})),
				},
				UpdateStrategy: apps.DaemonSetUpdateStrategy{
					Type: apps.OnDeleteDaemonSetStrategyType,
				},
			},
		},
	}
	for k, v := range errorCases {
		errs := ValidateDaemonSet(&v, corevalidation.PodValidationOptions{})
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
		for i := range errs {
			field := errs[i].Field
			if !strings.HasPrefix(field, "spec.template.") &&
				!strings.HasPrefix(field, "spec.updateStrategy") &&
				field != "metadata.name" &&
				field != "metadata.namespace" &&
				field != "spec.selector" &&
				field != "spec.template" &&
				field != "GCEPersistentDisk.ReadOnly" &&
				field != "spec.template.labels" &&
				field != "metadata.annotations" &&
				field != "metadata.labels" {
				t.Errorf("%s: missing prefix for: %v", k, errs[i])
			}
		}
	}
}

func validDeployment(tweaks ...func(d *apps.Deployment)) *apps.Deployment {
	d := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "abc",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: apps.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": "abc",
				},
			},
			Strategy: apps.DeploymentStrategy{
				Type: apps.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &apps.RollingUpdateDeployment{
					MaxSurge:       intstr.FromInt32(1),
					MaxUnavailable: intstr.FromInt32(1),
				},
			},
			Template: api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: metav1.NamespaceDefault,
					Labels: map[string]string{
						"name": "abc",
					},
				},
				Spec: podtest.MakePodSpec(),
			},
			RollbackTo: &apps.RollbackConfig{
				Revision: 1,
			},
		},
	}

	for _, tweak := range tweaks {
		tweak(d)
	}

	return d
}

func TestValidateDeployment(t *testing.T) {
	successCases := []*apps.Deployment{
		validDeployment(),
		validDeployment(func(d *apps.Deployment) {
			d.Spec.Template.Spec.SecurityContext = &api.PodSecurityContext{
				HostNetwork: true,
			}
			d.Spec.Template.Spec.Containers[0].Ports = []api.ContainerPort{{
				ContainerPort: 12345,
				Protocol:      api.ProtocolTCP,
			}}
		}),
	}
	for _, successCase := range successCases {
		if errs := ValidateDeployment(successCase, corevalidation.PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := map[string]*apps.Deployment{}
	errorCases["metadata.name: Required value"] = &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
		},
	}
	// selector should match the labels in pod template.
	invalidSelectorDeployment := validDeployment()
	invalidSelectorDeployment.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"name": "def",
		},
	}
	errorCases["`selector` does not match template `labels`"] = invalidSelectorDeployment

	// RestartPolicy should be always.
	invalidRestartPolicyDeployment := validDeployment()
	invalidRestartPolicyDeployment.Spec.Template.Spec.RestartPolicy = api.RestartPolicyNever
	errorCases["Unsupported value: \"Never\""] = invalidRestartPolicyDeployment

	// must have valid strategy type
	invalidStrategyDeployment := validDeployment()
	invalidStrategyDeployment.Spec.Strategy.Type = apps.DeploymentStrategyType("randomType")
	errorCases[`supported values: "Recreate", "RollingUpdate"`] = invalidStrategyDeployment

	// rollingUpdate should be nil for recreate.
	invalidRecreateDeployment := validDeployment()
	invalidRecreateDeployment.Spec.Strategy = apps.DeploymentStrategy{
		Type:          apps.RecreateDeploymentStrategyType,
		RollingUpdate: &apps.RollingUpdateDeployment{},
	}
	errorCases["may not be specified when strategy `type` is 'Recreate'"] = invalidRecreateDeployment

	// MaxSurge should be in the form of 20%.
	invalidMaxSurgeDeployment := validDeployment()
	invalidMaxSurgeDeployment.Spec.Strategy = apps.DeploymentStrategy{
		Type: apps.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &apps.RollingUpdateDeployment{
			MaxSurge: intstr.FromString("20Percent"),
		},
	}
	errorCases["a valid percent string must be"] = invalidMaxSurgeDeployment

	// MaxSurge and MaxUnavailable cannot both be zero.
	invalidRollingUpdateDeployment := validDeployment()
	invalidRollingUpdateDeployment.Spec.Strategy = apps.DeploymentStrategy{
		Type: apps.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &apps.RollingUpdateDeployment{
			MaxSurge:       intstr.FromString("0%"),
			MaxUnavailable: intstr.FromInt32(0),
		},
	}
	errorCases["may not be 0 when `maxSurge` is 0"] = invalidRollingUpdateDeployment

	// MaxUnavailable should not be more than 100%.
	invalidMaxUnavailableDeployment := validDeployment()
	invalidMaxUnavailableDeployment.Spec.Strategy = apps.DeploymentStrategy{
		Type: apps.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &apps.RollingUpdateDeployment{
			MaxUnavailable: intstr.FromString("110%"),
		},
	}
	errorCases["must not be greater than 100%"] = invalidMaxUnavailableDeployment

	// Rollback.Revision must be non-negative
	invalidRollbackRevisionDeployment := validDeployment()
	invalidRollbackRevisionDeployment.Spec.RollbackTo.Revision = -3
	errorCases["must be greater than or equal to 0"] = invalidRollbackRevisionDeployment

	// ProgressDeadlineSeconds should be greater than MinReadySeconds
	invalidProgressDeadlineDeployment := validDeployment()
	seconds := int32(600)
	invalidProgressDeadlineDeployment.Spec.ProgressDeadlineSeconds = &seconds
	invalidProgressDeadlineDeployment.Spec.MinReadySeconds = seconds
	errorCases["must be greater than minReadySeconds"] = invalidProgressDeadlineDeployment

	// Must not have ephemeral containers
	invalidEphemeralContainersDeployment := validDeployment()
	invalidEphemeralContainersDeployment.Spec.Template.Spec.EphemeralContainers = []api.EphemeralContainer{{
		EphemeralContainerCommon: api.EphemeralContainerCommon{
			Name:                     "ec",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File"},
	}}
	errorCases["ephemeral containers not allowed"] = invalidEphemeralContainersDeployment

	for k, v := range errorCases {
		errs := ValidateDeployment(v, corevalidation.PodValidationOptions{})
		if len(errs) == 0 {
			t.Errorf("[%s] expected failure", k)
		} else if !strings.Contains(errs[0].Error(), k) {
			t.Errorf("unexpected error: %q, expected: %q", errs[0].Error(), k)
		}
	}
}

func TestValidateDeploymentPodReplacementPolicy(t *testing.T) {
	testCases := []struct {
		name             string
		deployment       *apps.Deployment
		expectedErrorMsg string
	}{
		{
			name:       "valid nil PodReplacementPolicy",
			deployment: validDeployment(),
		},
		{
			name: "valid PodReplacementPolicy",
			deployment: func() *apps.Deployment {
				d := validDeployment()
				d.Spec.PodReplacementPolicy = ptr.To(apps.TerminationComplete)
				return d
			}(),
		},
		{
			name: "empty PodReplacementPolicy",
			deployment: func() *apps.Deployment {
				d := validDeployment()
				d.Spec.PodReplacementPolicy = new(apps.DeploymentPodReplacementPolicy)
				return d
			}(),
			expectedErrorMsg: "Unsupported value: \"\"",
		},
		{
			name: "invalid PodReplacementPolicy",
			deployment: func() *apps.Deployment {
				d := validDeployment()
				d.Spec.PodReplacementPolicy = ptr.To(apps.DeploymentPodReplacementPolicy("Invalid"))
				return d
			}(),
			expectedErrorMsg: "Unsupported value: \"Invalid\"",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateDeployment(tc.deployment, corevalidation.PodValidationOptions{})
			if len(tc.expectedErrorMsg) > 0 {
				if len(errs) == 0 {
					t.Errorf("unexpected success")
				}
				if !strings.Contains(errs[0].Error(), tc.expectedErrorMsg) {
					t.Errorf("unexpected error: %q, expected: %q", errs[0].Error(), tc.expectedErrorMsg)
				}
			} else if len(errs) != 0 {
				t.Errorf("unexpected error: %v", errs)
			}
		})
	}
}

func TestValidateDeploymentStatus(t *testing.T) {
	collisionCount := int32(-3)
	tests := []struct {
		name string

		replicas            int32
		updatedReplicas     int32
		readyReplicas       int32
		availableReplicas   int32
		terminatingReplicas *int32
		observedGeneration  int64
		collisionCount      *int32

		expectedErr bool
	}{{
		name:                "valid status",
		replicas:            3,
		updatedReplicas:     3,
		readyReplicas:       2,
		availableReplicas:   1,
		terminatingReplicas: nil,
		observedGeneration:  2,
		expectedErr:         false,
	}, {
		name:                "valid status with terminating replicas",
		replicas:            3,
		updatedReplicas:     3,
		readyReplicas:       2,
		availableReplicas:   1,
		terminatingReplicas: ptr.To[int32](5),
		observedGeneration:  2,
		expectedErr:         false,
	}, {
		name:                "invalid replicas",
		replicas:            -1,
		updatedReplicas:     2,
		readyReplicas:       2,
		availableReplicas:   1,
		terminatingReplicas: nil,
		observedGeneration:  2,
		expectedErr:         true,
	}, {
		name:                "invalid updatedReplicas",
		replicas:            2,
		updatedReplicas:     -1,
		readyReplicas:       2,
		availableReplicas:   1,
		terminatingReplicas: nil,
		observedGeneration:  2,
		expectedErr:         true,
	}, {
		name:                "invalid readyReplicas",
		replicas:            3,
		readyReplicas:       -1,
		availableReplicas:   1,
		terminatingReplicas: nil,
		observedGeneration:  2,
		expectedErr:         true,
	}, {
		name:                "invalid availableReplicas",
		replicas:            3,
		readyReplicas:       3,
		availableReplicas:   -1,
		terminatingReplicas: nil,
		observedGeneration:  2,
		expectedErr:         true,
	}, {
		name:                "invalid terminatingReplicas",
		replicas:            3,
		updatedReplicas:     3,
		readyReplicas:       2,
		availableReplicas:   1,
		terminatingReplicas: ptr.To[int32](-1),
		observedGeneration:  2,
		expectedErr:         true,
	}, {
		name:                "invalid observedGeneration",
		replicas:            3,
		readyReplicas:       3,
		availableReplicas:   3,
		terminatingReplicas: nil,
		observedGeneration:  -1,
		expectedErr:         true,
	}, {
		name:               "updatedReplicas greater than replicas",
		replicas:           3,
		updatedReplicas:    4,
		readyReplicas:      3,
		availableReplicas:  3,
		observedGeneration: 1,
		expectedErr:        true,
	}, {
		name:               "readyReplicas greater than replicas",
		replicas:           3,
		readyReplicas:      4,
		availableReplicas:  3,
		observedGeneration: 1,
		expectedErr:        true,
	}, {
		name:               "availableReplicas greater than replicas",
		replicas:           3,
		readyReplicas:      3,
		availableReplicas:  4,
		observedGeneration: 1,
		expectedErr:        true,
	}, {
		name:               "availableReplicas greater than readyReplicas",
		replicas:           3,
		readyReplicas:      2,
		availableReplicas:  3,
		observedGeneration: 1,
		expectedErr:        true,
	}, {
		name:               "invalid collisionCount",
		replicas:           3,
		observedGeneration: 1,
		collisionCount:     &collisionCount,
		expectedErr:        true,
	},
	}

	for _, test := range tests {
		status := apps.DeploymentStatus{
			Replicas:            test.replicas,
			UpdatedReplicas:     test.updatedReplicas,
			ReadyReplicas:       test.readyReplicas,
			AvailableReplicas:   test.availableReplicas,
			TerminatingReplicas: test.terminatingReplicas,
			ObservedGeneration:  test.observedGeneration,
			CollisionCount:      test.collisionCount,
		}

		errs := ValidateDeploymentStatus(&status, field.NewPath("status"))
		if hasErr := len(errs) > 0; hasErr != test.expectedErr {
			errString := dump.Pretty(errs)
			t.Errorf("%s: expected error: %t, got error: %t\nerrors: %s", test.name, test.expectedErr, hasErr, errString)
		}
	}
}

func TestValidateDeploymentStatusUpdate(t *testing.T) {
	collisionCount := int32(1)
	otherCollisionCount := int32(2)
	tests := []struct {
		name string

		from, to apps.DeploymentStatus

		expectedErr bool
	}{{
		name: "increase: valid update",
		from: apps.DeploymentStatus{
			CollisionCount: nil,
		},
		to: apps.DeploymentStatus{
			CollisionCount: &collisionCount,
		},
		expectedErr: false,
	}, {
		name: "stable: valid update",
		from: apps.DeploymentStatus{
			CollisionCount: &collisionCount,
		},
		to: apps.DeploymentStatus{
			CollisionCount: &collisionCount,
		},
		expectedErr: false,
	}, {
		name: "unset: invalid update",
		from: apps.DeploymentStatus{
			CollisionCount: &collisionCount,
		},
		to: apps.DeploymentStatus{
			CollisionCount: nil,
		},
		expectedErr: true,
	}, {
		name: "decrease: invalid update",
		from: apps.DeploymentStatus{
			CollisionCount: &otherCollisionCount,
		},
		to: apps.DeploymentStatus{
			CollisionCount: &collisionCount,
		},
		expectedErr: true,
	},
	}

	for _, test := range tests {
		meta := metav1.ObjectMeta{Name: "foo", Namespace: metav1.NamespaceDefault, ResourceVersion: "1"}
		from := &apps.Deployment{
			ObjectMeta: meta,
			Status:     test.from,
		}
		to := &apps.Deployment{
			ObjectMeta: meta,
			Status:     test.to,
		}

		errs := ValidateDeploymentStatusUpdate(to, from)
		if hasErr := len(errs) > 0; hasErr != test.expectedErr {
			errString := dump.Pretty(errs)
			t.Errorf("%s: expected error: %t, got error: %t\nerrors: %s", test.name, test.expectedErr, hasErr, errString)
		}
	}
}

func validDeploymentRollback() *apps.DeploymentRollback {
	return &apps.DeploymentRollback{
		Name: "abc",
		UpdatedAnnotations: map[string]string{
			"created-by": "abc",
		},
		RollbackTo: apps.RollbackConfig{
			Revision: 1,
		},
	}
}

func TestValidateDeploymentUpdate(t *testing.T) {
	validLabels := map[string]string{"a": "b"}
	validPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	readWriteVolumePodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetVolumes(api.Volume{Name: "gcepd", VolumeSource: api.VolumeSource{GCEPersistentDisk: &api.GCEPersistentDiskVolumeSource{PDName: "my-PD", FSType: "ext4", Partition: 1, ReadOnly: false}}}),
			),
		},
	}
	invalidLabels := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	invalidPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			// no containers specified
			Spec: podtest.MakePodSpec(podtest.SetContainers()),
			ObjectMeta: metav1.ObjectMeta{
				Labels: invalidLabels,
			},
		},
	}
	type depUpdateTest struct {
		old            apps.Deployment
		update         apps.Deployment
		expectedErrNum int
	}
	successCases := map[string]depUpdateTest{
		"positive replicas": {
			old: apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
					Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
				},
			},
			update: apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DeploymentSpec{
					Replicas: 1,
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: readWriteVolumePodTemplate.Template,
					Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
				},
			},
		},
		"Read-write volume verification": {
			old: apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
					Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
				},
			},
			update: apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.DeploymentSpec{
					Replicas: 2,
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: readWriteVolumePodTemplate.Template,
					Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
				},
			},
		},
	}
	for testName, successCase := range successCases {
		t.Run(testName, func(t *testing.T) {
			// ResourceVersion is required for updates.
			successCase.old.ObjectMeta.ResourceVersion = "1"
			successCase.update.ObjectMeta.ResourceVersion = "2"
			// Check test setup
			if successCase.expectedErrNum > 0 {
				t.Errorf("%q has incorrect test setup with expectedErrNum %d, expected no error", testName, successCase.expectedErrNum)
			}
			if len(successCase.old.ObjectMeta.ResourceVersion) == 0 || len(successCase.update.ObjectMeta.ResourceVersion) == 0 {
				t.Errorf("%q has incorrect test setup with no resource version set", testName)
			}
			// Run the tests
			if errs := ValidateDeploymentUpdate(&successCase.update, &successCase.old, corevalidation.PodValidationOptions{}); len(errs) != 0 {
				t.Errorf("%q expected no error, but got: %v", testName, errs)
			}
		})
		errorCases := map[string]depUpdateTest{
			"invalid selector": {
				old: apps.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
					Spec: apps.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: validLabels},
						Template: validPodTemplate.Template,
						Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
					},
				},
				update: apps.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
					Spec: apps.DeploymentSpec{
						Replicas: 2,
						Selector: &metav1.LabelSelector{MatchLabels: invalidLabels},
						Template: validPodTemplate.Template,
						Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
					},
				},
				expectedErrNum: 3,
			},
			"invalid pod": {
				old: apps.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
					Spec: apps.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: validLabels},
						Template: validPodTemplate.Template,
						Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
					},
				},
				update: apps.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
					Spec: apps.DeploymentSpec{
						Replicas: 2,
						Selector: &metav1.LabelSelector{MatchLabels: validLabels},
						Template: invalidPodTemplate.Template,
						Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
					},
				},
				expectedErrNum: 4,
			},
			"negative replicas": {
				old: apps.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
					Spec: apps.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: validLabels},
						Template: validPodTemplate.Template,
						Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
					},
				},
				update: apps.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
					Spec: apps.DeploymentSpec{
						Replicas: -1,
						Selector: &metav1.LabelSelector{MatchLabels: validLabels},
						Template: readWriteVolumePodTemplate.Template,
						Strategy: apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType},
					},
				},
				expectedErrNum: 1,
			},
		}
		for testName, errorCase := range errorCases {
			t.Run(testName, func(t *testing.T) {
				// ResourceVersion is required for updates.
				errorCase.old.ObjectMeta.ResourceVersion = "1"
				errorCase.update.ObjectMeta.ResourceVersion = "2"
				// Check test setup
				if errorCase.expectedErrNum <= 0 {
					t.Errorf("%q has incorrect test setup with expectedErrNum %d, expected at least one error", testName, errorCase.expectedErrNum)
				}
				if len(errorCase.old.ObjectMeta.ResourceVersion) == 0 || len(errorCase.update.ObjectMeta.ResourceVersion) == 0 {
					t.Errorf("%q has incorrect test setup with no resource version set", testName)
				}
				// Run the tests
				if errs := ValidateDeploymentUpdate(&errorCase.update, &errorCase.old, corevalidation.PodValidationOptions{}); len(errs) != errorCase.expectedErrNum {
					t.Errorf("%q expected %d errors, but got %d error: %v", testName, errorCase.expectedErrNum, len(errs), errs)
				} else {
					t.Logf("(PASS) %q got errors %v", testName, errs)
				}
			})
		}
	}
}

func TestValidateDeploymentRollback(t *testing.T) {
	noAnnotation := validDeploymentRollback()
	noAnnotation.UpdatedAnnotations = nil
	successCases := []*apps.DeploymentRollback{
		validDeploymentRollback(),
		noAnnotation,
	}
	for _, successCase := range successCases {
		if errs := ValidateDeploymentRollback(successCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := map[string]*apps.DeploymentRollback{}
	invalidNoName := validDeploymentRollback()
	invalidNoName.Name = ""
	errorCases["name: Required value"] = invalidNoName

	for k, v := range errorCases {
		errs := ValidateDeploymentRollback(v)
		if len(errs) == 0 {
			t.Errorf("[%s] expected failure", k)
		} else if !strings.Contains(errs[0].Error(), k) {
			t.Errorf("unexpected error: %q, expected: %q", errs[0].Error(), k)
		}
	}
}

func TestValidateReplicaSetStatus(t *testing.T) {
	tests := []struct {
		name string

		replicas             int32
		fullyLabeledReplicas int32
		readyReplicas        int32
		availableReplicas    int32
		terminatingReplicas  *int32
		observedGeneration   int64

		expectedErr bool
	}{{
		name:                 "valid status",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    1,
		terminatingReplicas:  nil,
		observedGeneration:   2,
		expectedErr:          false,
	}, {
		name:                 "valid status with terminating replicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    1,
		terminatingReplicas:  ptr.To[int32](5),
		observedGeneration:   2,
		expectedErr:          false,
	}, {
		name:                 "invalid replicas",
		replicas:             -1,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    1,
		terminatingReplicas:  nil,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid fullyLabeledReplicas",
		replicas:             3,
		fullyLabeledReplicas: -1,
		readyReplicas:        2,
		availableReplicas:    1,
		terminatingReplicas:  nil,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid readyReplicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        -1,
		availableReplicas:    1,
		terminatingReplicas:  nil,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid availableReplicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        3,
		availableReplicas:    -1,
		terminatingReplicas:  nil,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid terminatingReplicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    1,
		terminatingReplicas:  ptr.To[int32](-1),
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid observedGeneration",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        3,
		availableReplicas:    3,
		terminatingReplicas:  nil,
		observedGeneration:   -1,
		expectedErr:          true,
	}, {
		name:                 "fullyLabeledReplicas greater than replicas",
		replicas:             3,
		fullyLabeledReplicas: 4,
		readyReplicas:        3,
		availableReplicas:    3,
		terminatingReplicas:  nil,
		observedGeneration:   1,
		expectedErr:          true,
	}, {
		name:                 "readyReplicas greater than replicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        4,
		availableReplicas:    3,
		terminatingReplicas:  nil,
		observedGeneration:   1,
		expectedErr:          true,
	}, {
		name:                 "availableReplicas greater than replicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        3,
		availableReplicas:    4,
		terminatingReplicas:  nil,
		observedGeneration:   1,
		expectedErr:          true,
	}, {
		name:                 "availableReplicas greater than readyReplicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    3,
		terminatingReplicas:  nil,
		observedGeneration:   1,
		expectedErr:          true,
	},
	}

	for _, test := range tests {
		status := apps.ReplicaSetStatus{
			Replicas:             test.replicas,
			FullyLabeledReplicas: test.fullyLabeledReplicas,
			ReadyReplicas:        test.readyReplicas,
			AvailableReplicas:    test.availableReplicas,
			TerminatingReplicas:  test.terminatingReplicas,
			ObservedGeneration:   test.observedGeneration,
		}

		if hasErr := len(ValidateReplicaSetStatus(status, field.NewPath("status"))) > 0; hasErr != test.expectedErr {
			t.Errorf("%s: expected error: %t, got error: %t", test.name, test.expectedErr, hasErr)
		}
	}
}

func TestValidateReplicaSetStatusUpdate(t *testing.T) {
	validLabels := map[string]string{"a": "b"}
	validPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	type rcUpdateTest struct {
		old    apps.ReplicaSet
		update apps.ReplicaSet
	}
	successCases := []rcUpdateTest{{
		old: apps.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: validPodTemplate.Template,
			},
			Status: apps.ReplicaSetStatus{
				Replicas: 2,
			},
		},
		update: apps.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.ReplicaSetSpec{
				Replicas: 3,
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: validPodTemplate.Template,
			},
			Status: apps.ReplicaSetStatus{
				Replicas: 4,
			},
		},
	},
	}
	for _, successCase := range successCases {
		successCase.old.ObjectMeta.ResourceVersion = "1"
		successCase.update.ObjectMeta.ResourceVersion = "1"
		if errs := ValidateReplicaSetStatusUpdate(&successCase.update, &successCase.old); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	errorCases := map[string]rcUpdateTest{
		"negative replicas": {
			old: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
				Status: apps.ReplicaSetStatus{
					Replicas: 3,
				},
			},
			update: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Replicas: 2,
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
				Status: apps.ReplicaSetStatus{
					Replicas: -3,
				},
			},
		},
	}
	for testName, errorCase := range errorCases {
		if errs := ValidateReplicaSetStatusUpdate(&errorCase.update, &errorCase.old); len(errs) == 0 {
			t.Errorf("expected failure: %s", testName)
		}
	}

}

func TestValidateReplicaSetUpdate(t *testing.T) {
	validLabels := map[string]string{"a": "b"}
	validPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	readWriteVolumePodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetVolumes(api.Volume{Name: "gcepd", VolumeSource: api.VolumeSource{GCEPersistentDisk: &api.GCEPersistentDiskVolumeSource{PDName: "my-PD", FSType: "ext4", Partition: 1, ReadOnly: false}}}),
			),
		},
	}
	invalidLabels := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	invalidPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			Spec: podtest.MakePodSpec(podtest.SetContainers()),
			ObjectMeta: metav1.ObjectMeta{
				Labels: invalidLabels,
			},
		},
	}
	type rcUpdateTest struct {
		old            apps.ReplicaSet
		update         apps.ReplicaSet
		expectedErrNum int
	}
	successCases := map[string]rcUpdateTest{
		"positive replicas": {
			old: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
			},
			update: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Replicas: 3,
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
			},
		},
		"Read-write volume verification": {
			old: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
			},
			update: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Replicas: 3,
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: readWriteVolumePodTemplate.Template,
				},
			},
		},
	}
	for testName, successCase := range successCases {
		t.Run(testName, func(t *testing.T) {
			// ResourceVersion is required for updates.
			successCase.old.ObjectMeta.ResourceVersion = "1"
			successCase.update.ObjectMeta.ResourceVersion = "2"
			// Check test setup
			if successCase.expectedErrNum > 0 {
				t.Errorf("%q has incorrect test setup with expectedErrNum %d, expected no error", testName, successCase.expectedErrNum)
			}
			if len(successCase.old.ObjectMeta.ResourceVersion) == 0 || len(successCase.update.ObjectMeta.ResourceVersion) == 0 {
				t.Errorf("%q has incorrect test setup with no resource version set", testName)
			}
			// Run the tests
			if errs := ValidateReplicaSetUpdate(&successCase.update, &successCase.old, corevalidation.PodValidationOptions{}); len(errs) != 0 {
				t.Errorf("%q expected no error, but got: %v", testName, errs)
			}
		})
	}
	errorCases := map[string]rcUpdateTest{
		"invalid selector": {
			old: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
			},
			update: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Replicas: 2,
					Selector: &metav1.LabelSelector{MatchLabels: invalidLabels},
					Template: validPodTemplate.Template,
				},
			},
			expectedErrNum: 3,
		},
		"invalid pod": {
			old: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
			},
			update: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Replicas: 2,
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: invalidPodTemplate.Template,
				},
			},
			expectedErrNum: 4,
		},
		"negative replicas": {
			old: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
			},
			update: apps.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: apps.ReplicaSetSpec{
					Replicas: -1,
					Selector: &metav1.LabelSelector{MatchLabels: validLabels},
					Template: validPodTemplate.Template,
				},
			},
			expectedErrNum: 1,
		},
	}
	for testName, errorCase := range errorCases {
		t.Run(testName, func(t *testing.T) {
			// ResourceVersion is required for updates.
			errorCase.old.ObjectMeta.ResourceVersion = "1"
			errorCase.update.ObjectMeta.ResourceVersion = "2"
			// Check test setup
			if errorCase.expectedErrNum <= 0 {
				t.Errorf("%q has incorrect test setup with expectedErrNum %d, expected at least one error", testName, errorCase.expectedErrNum)
			}
			if len(errorCase.old.ObjectMeta.ResourceVersion) == 0 || len(errorCase.update.ObjectMeta.ResourceVersion) == 0 {
				t.Errorf("%q has incorrect test setup with no resource version set", testName)
			}
			// Run the tests
			if errs := ValidateReplicaSetUpdate(&errorCase.update, &errorCase.old, corevalidation.PodValidationOptions{}); len(errs) != errorCase.expectedErrNum {
				t.Errorf("%q expected %d errors, but got %d error: %v", testName, errorCase.expectedErrNum, len(errs), errs)
			} else {
				t.Logf("(PASS) %q got errors %v", testName, errs)
			}
		})
	}
}

func TestValidateReplicaSet(t *testing.T) {
	validLabels := map[string]string{"a": "b"}
	validPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	validHostNetPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetSecurityContext(&api.PodSecurityContext{
					HostNetwork: true,
				}),
				podtest.SetContainers(podtest.MakeContainer("abc", podtest.SetContainerPorts(api.ContainerPort{
					ContainerPort: 12345,
					Protocol:      api.ProtocolTCP,
				}))),
			),
		},
	}
	readWriteVolumePodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validLabels,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetVolumes(api.Volume{Name: "gcepd", VolumeSource: api.VolumeSource{GCEPersistentDisk: &api.GCEPersistentDiskVolumeSource{PDName: "my-PD", FSType: "ext4", Partition: 1, ReadOnly: false}}}),
			),
		},
	}
	invalidLabels := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	invalidPodTemplate := api.PodTemplate{
		Template: api.PodTemplateSpec{
			Spec: podtest.MakePodSpec(),
			ObjectMeta: metav1.ObjectMeta{
				Labels: invalidLabels,
			},
		},
	}
	successCases := []apps.ReplicaSet{{
		ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
		Spec: apps.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: validLabels},
			Template: validPodTemplate.Template,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "abc-123", Namespace: metav1.NamespaceDefault},
		Spec: apps.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: validLabels},
			Template: validPodTemplate.Template,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "hostnet", Namespace: metav1.NamespaceDefault},
		Spec: apps.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: validLabels},
			Template: validHostNetPodTemplate.Template,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "abc-123", Namespace: metav1.NamespaceDefault},
		Spec: apps.ReplicaSetSpec{
			Replicas: 1,
			Selector: &metav1.LabelSelector{MatchLabels: validLabels},
			Template: readWriteVolumePodTemplate.Template,
		},
	},
	}
	for _, successCase := range successCases {
		if errs := ValidateReplicaSet(&successCase, corevalidation.PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := map[string]apps.ReplicaSet{
		"zero-length ID": {
			ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: validPodTemplate.Template,
			},
		},
		"missing-namespace": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc-123"},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: validPodTemplate.Template,
			},
		},
		"empty selector": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.ReplicaSetSpec{
				Template: validPodTemplate.Template,
			},
		},
		"selector_doesnt_match": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
				Template: validPodTemplate.Template,
			},
		},
		"invalid manifest": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
			},
		},
		"read-write persistent disk with > 1 pod": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc"},
			Spec: apps.ReplicaSetSpec{
				Replicas: 2,
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: readWriteVolumePodTemplate.Template,
			},
		},
		"negative_replicas": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: apps.ReplicaSetSpec{
				Replicas: -1,
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
			},
		},
		"invalid_label": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Labels: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: validPodTemplate.Template,
			},
		},
		"invalid_label 2": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Labels: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: apps.ReplicaSetSpec{
				Template: invalidPodTemplate.Template,
			},
		},
		"invalid_annotation": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Annotations: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: validPodTemplate.Template,
			},
		},
		"invalid restart policy 1": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: api.PodTemplateSpec{
					Spec: podtest.MakePodSpec(podtest.SetRestartPolicy(api.RestartPolicyOnFailure)),
					ObjectMeta: metav1.ObjectMeta{
						Labels: validLabels,
					},
				},
			},
		},
		"invalid restart policy 2": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: apps.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: validLabels},
				Template: api.PodTemplateSpec{
					Spec: podtest.MakePodSpec(podtest.SetRestartPolicy(api.RestartPolicyNever)),
					ObjectMeta: metav1.ObjectMeta{
						Labels: validLabels,
					},
				},
			},
		},
	}
	for k, v := range errorCases {
		errs := ValidateReplicaSet(&v, corevalidation.PodValidationOptions{})
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
		for i := range errs {
			field := errs[i].Field
			if !strings.HasPrefix(field, "spec.template.") &&
				field != "metadata.name" &&
				field != "metadata.namespace" &&
				field != "spec.selector" &&
				field != "spec.template" &&
				field != "GCEPersistentDisk.ReadOnly" &&
				field != "spec.replicas" &&
				field != "spec.template.labels" &&
				field != "metadata.annotations" &&
				field != "metadata.labels" &&
				field != "status.replicas" {
				t.Errorf("%s: missing prefix for: %v", k, errs[i])
			}
		}
	}
}

func TestDaemonSetUpdateMaxSurge(t *testing.T) {
	testCases := map[string]struct {
		ds          *apps.RollingUpdateDaemonSet
		expectError bool
	}{
		"invalid: unset": {
			ds:          &apps.RollingUpdateDaemonSet{},
			expectError: true,
		},
		"invalid: zero percent": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("0%"),
			},
			expectError: true,
		},
		"invalid: zero": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromInt32(0),
			},
			expectError: true,
		},
		"valid: one": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromInt32(1),
			},
		},
		"valid: one percent": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("1%"),
			},
		},
		"valid: 100%": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("100%"),
			},
		},
		"invalid: greater than 100%": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("101%"),
			},
			expectError: true,
		},

		"valid: surge and unavailable set": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("1%"),
				MaxSurge:       intstr.FromString("1%"),
			},
			expectError: true,
		},

		"invalid: surge enabled, unavailable zero percent": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("0%"),
			},
			expectError: true,
		},
		"invalid: surge enabled, unavailable zero": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromInt32(0),
			},
			expectError: true,
		},
		"valid: surge enabled, unavailable one": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromInt32(1),
			},
		},
		"valid: surge enabled, unavailable one percent": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("1%"),
			},
		},
		"valid: surge enabled, unavailable 100%": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("100%"),
			},
		},
		"invalid: surge enabled, unavailable greater than 100%": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("101%"),
			},
			expectError: true,
		},

		"invalid: surge enabled, surge zero percent": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxSurge: intstr.FromString("0%"),
			},
			expectError: true,
		},
		"invalid: surge enabled, surge zero": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxSurge: intstr.FromInt32(0),
			},
			expectError: true,
		},
		"valid: surge enabled, surge one": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxSurge: intstr.FromInt32(1),
			},
		},
		"valid: surge enabled, surge one percent": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxSurge: intstr.FromString("1%"),
			},
		},
		"valid: surge enabled, surge 100%": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxSurge: intstr.FromString("100%"),
			},
		},
		"invalid: surge enabled, surge greater than 100%": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxSurge: intstr.FromString("101%"),
			},
			expectError: true,
		},

		"invalid: surge enabled, surge and unavailable set": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("1%"),
				MaxSurge:       intstr.FromString("1%"),
			},
			expectError: true,
		},

		"invalid: surge enabled, surge and unavailable zero percent": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromString("0%"),
				MaxSurge:       intstr.FromString("0%"),
			},
			expectError: true,
		},
		"invalid: surge enabled, surge and unavailable zero": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(0),
			},
			expectError: true,
		},
		"invalid: surge enabled, surge and unavailable mixed zero": {
			ds: &apps.RollingUpdateDaemonSet{
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromString("0%"),
			},
			expectError: true,
		},
	}
	for tcName, tc := range testCases {
		t.Run(tcName, func(t *testing.T) {
			errs := ValidateRollingUpdateDaemonSet(tc.ds, field.NewPath("spec", "updateStrategy", "rollingUpdate"))
			if tc.expectError && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !tc.expectError && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}
