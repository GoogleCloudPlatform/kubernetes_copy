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
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/storage"
	"k8s.io/kubernetes/pkg/features"
	utilpointer "k8s.io/utils/pointer"
)

var (
	deleteReclaimPolicy = api.PersistentVolumeReclaimDelete
	immediateMode1      = storage.VolumeBindingImmediate
	immediateMode2      = storage.VolumeBindingImmediate
	waitingMode         = storage.VolumeBindingWaitForFirstConsumer
	invalidMode         = storage.VolumeBindingMode("foo")
	inlineSpec          = api.PersistentVolumeSpec{
		AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
		PersistentVolumeSource: api.PersistentVolumeSource{
			CSI: &api.CSIPersistentVolumeSource{
				Driver:       "com.test.foo",
				VolumeHandle: "foobar",
			},
		},
	}
	longerIDValidateOption = CSINodeValidationOptions{
		AllowLongNodeID: true,
	}
	shorterIDValidationOption = CSINodeValidationOptions{
		AllowLongNodeID: false,
	}
)

func TestValidateStorageClass(t *testing.T) {
	deleteReclaimPolicy := api.PersistentVolumeReclaimPolicy("Delete")
	retainReclaimPolicy := api.PersistentVolumeReclaimPolicy("Retain")
	recycleReclaimPolicy := api.PersistentVolumeReclaimPolicy("Recycle")
	successCases := []storage.StorageClass{{
		// empty parameters
		ObjectMeta:        metav1.ObjectMeta{Name: "foo"},
		Provisioner:       "kubernetes.io/foo-provisioner",
		Parameters:        map[string]string{},
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &immediateMode1,
	}, {
		// nil parameters
		ObjectMeta:        metav1.ObjectMeta{Name: "foo"},
		Provisioner:       "kubernetes.io/foo-provisioner",
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &immediateMode1,
	}, {
		// some parameters
		ObjectMeta:  metav1.ObjectMeta{Name: "foo"},
		Provisioner: "kubernetes.io/foo-provisioner",
		Parameters: map[string]string{
			"kubernetes.io/foo-parameter": "free/form/string",
			"foo-parameter":               "free-form-string",
			"foo-parameter2":              "{\"embedded\": \"json\", \"with\": {\"structures\":\"inside\"}}",
		},
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &immediateMode1,
	}, {
		// retain reclaimPolicy
		ObjectMeta:        metav1.ObjectMeta{Name: "foo"},
		Provisioner:       "kubernetes.io/foo-provisioner",
		ReclaimPolicy:     &retainReclaimPolicy,
		VolumeBindingMode: &immediateMode1,
	}}

	// Success cases are expected to pass validation.
	for k, v := range successCases {
		if errs := ValidateStorageClass(&v); len(errs) != 0 {
			t.Errorf("Expected success for %d, got %v", k, errs)
		}
	}

	// generate a map longer than maxProvisionerParameterSize
	longParameters := make(map[string]string)
	totalSize := 0
	for totalSize < maxProvisionerParameterSize {
		k := fmt.Sprintf("param/%d", totalSize)
		v := fmt.Sprintf("value-%d", totalSize)
		longParameters[k] = v
		totalSize = totalSize + len(k) + len(v)
	}

	errorCases := map[string]storage.StorageClass{
		"namespace is present": {
			ObjectMeta:    metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
			Provisioner:   "kubernetes.io/foo-provisioner",
			ReclaimPolicy: &deleteReclaimPolicy,
		},
		"invalid provisioner": {
			ObjectMeta:    metav1.ObjectMeta{Name: "foo"},
			Provisioner:   "kubernetes.io/invalid/provisioner",
			ReclaimPolicy: &deleteReclaimPolicy,
		},
		"invalid empty parameter name": {
			ObjectMeta:  metav1.ObjectMeta{Name: "foo"},
			Provisioner: "kubernetes.io/foo",
			Parameters: map[string]string{
				"": "value",
			},
			ReclaimPolicy: &deleteReclaimPolicy,
		},
		"provisioner: Required value": {
			ObjectMeta:    metav1.ObjectMeta{Name: "foo"},
			Provisioner:   "",
			ReclaimPolicy: &deleteReclaimPolicy,
		},
		"too long parameters": {
			ObjectMeta:    metav1.ObjectMeta{Name: "foo"},
			Provisioner:   "kubernetes.io/foo",
			Parameters:    longParameters,
			ReclaimPolicy: &deleteReclaimPolicy,
		},
		"invalid reclaimpolicy": {
			ObjectMeta:    metav1.ObjectMeta{Name: "foo"},
			Provisioner:   "kubernetes.io/foo",
			ReclaimPolicy: &recycleReclaimPolicy,
		},
	}

	// Error cases are not expected to pass validation.
	for testName, storageClass := range errorCases {
		if errs := ValidateStorageClass(&storageClass); len(errs) == 0 {
			t.Errorf("Expected failure for test: %s", testName)
		}
	}
}

func TestVolumeAttachmentValidation(t *testing.T) {
	volumeName := "pv-name"
	empty := ""
	migrationEnabledSuccessCases := []storage.VolumeAttachment{{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
			NodeName: "mynode",
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "foo-with-inlinespec"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				InlineVolumeSpec: &inlineSpec,
			},
			NodeName: "mynode",
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "foo-with-status"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
			NodeName: "mynode",
		},
		Status: storage.VolumeAttachmentStatus{
			Attached: true,
			AttachmentMetadata: map[string]string{
				"foo": "bar",
			},
			AttachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
			DetachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "foo-with-inlinespec-and-status"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				InlineVolumeSpec: &inlineSpec,
			},
			NodeName: "mynode",
		},
		Status: storage.VolumeAttachmentStatus{
			Attached: true,
			AttachmentMetadata: map[string]string{
				"foo": "bar",
			},
			AttachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
			DetachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
		},
	}}

	for _, volumeAttachment := range migrationEnabledSuccessCases {
		if errs := ValidateVolumeAttachment(&volumeAttachment); len(errs) != 0 {
			t.Errorf("expected success: %v %v", volumeAttachment, errs)
		}
	}
	migrationEnabledErrorCases := []storage.VolumeAttachment{{
		// Empty attacher name
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "",
			NodeName: "mynode",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
		},
	}, {
		// Empty node name
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
		},
	}, {
		// No volume name
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "node",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: nil,
			},
		},
	}, {
		// Empty volume name
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "node",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &empty,
			},
		},
	}, {
		// Too long error message
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "node",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
		},
		Status: storage.VolumeAttachmentStatus{
			Attached: true,
			AttachmentMetadata: map[string]string{
				"foo": "bar",
			},
			AttachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
			DetachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: strings.Repeat("a", maxVolumeErrorMessageSize+1),
			},
		},
	}, {
		// Too long metadata
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "node",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
		},
		Status: storage.VolumeAttachmentStatus{
			Attached: true,
			AttachmentMetadata: map[string]string{
				"foo": strings.Repeat("a", maxAttachedVolumeMetadataSize),
			},
			AttachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
			DetachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
		},
	}, {
		// VolumeAttachmentSource with no PersistentVolumeName nor InlineSpec
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "node",
			Source:   storage.VolumeAttachmentSource{},
		},
	}, {
		// VolumeAttachmentSource with PersistentVolumeName and InlineSpec
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "node",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
				InlineVolumeSpec:     &inlineSpec,
			},
		},
	}, {
		// VolumeAttachmentSource with InlineSpec without CSI PV Source
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "node",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
				InlineVolumeSpec: &api.PersistentVolumeSpec{
					Capacity: api.ResourceList{
						api.ResourceName(api.ResourceStorage): resource.MustParse("10G"),
					},
					AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
					PersistentVolumeSource: api.PersistentVolumeSource{
						FlexVolume: &api.FlexPersistentVolumeSource{
							Driver: "kubernetes.io/blue",
							FSType: "ext4",
						},
					},
					StorageClassName: "test-storage-class",
				},
			},
		},
	}}

	for _, volumeAttachment := range migrationEnabledErrorCases {
		if errs := ValidateVolumeAttachment(&volumeAttachment); len(errs) == 0 {
			t.Errorf("expected failure for test: %v", volumeAttachment)
		}
	}
}

func TestVolumeAttachmentUpdateValidation(t *testing.T) {
	volumeName := "foo"
	newVolumeName := "bar"

	old := storage.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source:   storage.VolumeAttachmentSource{},
			NodeName: "mynode",
		},
	}

	successCases := []storage.VolumeAttachment{{
		// no change
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source:   storage.VolumeAttachmentSource{},
			NodeName: "mynode",
		},
	}, {
		// modify status
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source:   storage.VolumeAttachmentSource{},
			NodeName: "mynode",
		},
		Status: storage.VolumeAttachmentStatus{
			Attached: true,
			AttachmentMetadata: map[string]string{
				"foo": "bar",
			},
			AttachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
			DetachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
		},
	}}

	for _, volumeAttachment := range successCases {
		volumeAttachment.Spec.Source = storage.VolumeAttachmentSource{}
		old.Spec.Source = storage.VolumeAttachmentSource{}
		// test scenarios with PersistentVolumeName set
		volumeAttachment.Spec.Source.PersistentVolumeName = &volumeName
		old.Spec.Source.PersistentVolumeName = &volumeName
		if errs := ValidateVolumeAttachmentUpdate(&volumeAttachment, &old); len(errs) != 0 {
			t.Errorf("expected success: %+v", errs)
		}

		volumeAttachment.Spec.Source = storage.VolumeAttachmentSource{}
		old.Spec.Source = storage.VolumeAttachmentSource{}
		// test scenarios with InlineVolumeSpec set
		volumeAttachment.Spec.Source.InlineVolumeSpec = &inlineSpec
		old.Spec.Source.InlineVolumeSpec = &inlineSpec
		if errs := ValidateVolumeAttachmentUpdate(&volumeAttachment, &old); len(errs) != 0 {
			t.Errorf("expected success: %+v", errs)
		}
	}

	// reset old's source with volumeName in case it was left with something else by earlier tests
	old.Spec.Source = storage.VolumeAttachmentSource{}
	old.Spec.Source.PersistentVolumeName = &volumeName

	errorCases := []storage.VolumeAttachment{{
		// change attacher
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "another-attacher",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
			NodeName: "mynode",
		},
	}, {
		// change source volume name
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &newVolumeName,
			},
			NodeName: "mynode",
		},
	}, {
		// change node
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
			NodeName: "anothernode",
		},
	}, {
		// change source
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				InlineVolumeSpec: &inlineSpec,
			},
			NodeName: "mynode",
		},
	}, {
		// add invalid status
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
			NodeName: "mynode",
		},
		Status: storage.VolumeAttachmentStatus{
			Attached: true,
			AttachmentMetadata: map[string]string{
				"foo": "bar",
			},
			AttachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: strings.Repeat("a", maxAttachedVolumeMetadataSize),
			},
			DetachError: &storage.VolumeError{
				Time:    metav1.Time{},
				Message: "hello world",
			},
		},
	}}

	for _, volumeAttachment := range errorCases {
		if errs := ValidateVolumeAttachmentUpdate(&volumeAttachment, &old); len(errs) == 0 {
			t.Errorf("Expected failure for test: %+v", volumeAttachment)
		}
	}
}

func TestVolumeAttachmentValidationV1(t *testing.T) {
	volumeName := "pv-name"
	invalidVolumeName := "-invalid-@#$%^&*()-"
	successCases := []storage.VolumeAttachment{{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
			NodeName: "mynode",
		},
	}}

	for _, volumeAttachment := range successCases {
		if errs := ValidateVolumeAttachmentV1(&volumeAttachment); len(errs) != 0 {
			t.Errorf("expected success: %+v", errs)
		}
	}

	errorCases := []storage.VolumeAttachment{{
		// Invalid attacher name
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "invalid-@#$%^&*()",
			NodeName: "mynode",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &volumeName,
			},
		},
	}, {
		// Invalid PV name
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: "myattacher",
			NodeName: "mynode",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &invalidVolumeName,
			},
		},
	}}

	for _, volumeAttachment := range errorCases {
		if errs := ValidateVolumeAttachmentV1(&volumeAttachment); len(errs) == 0 {
			t.Errorf("Expected failure for test: %+v", volumeAttachment)
		}
	}
}

func makeClass(mode *storage.VolumeBindingMode, topologies []api.TopologySelectorTerm) *storage.StorageClass {
	return &storage.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "foo", ResourceVersion: "foo"},
		Provisioner:       "kubernetes.io/foo-provisioner",
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: mode,
		AllowedTopologies: topologies,
	}
}

type bindingTest struct {
	class         *storage.StorageClass
	shouldSucceed bool
}

func TestValidateVolumeBindingMode(t *testing.T) {
	cases := map[string]bindingTest{
		"no mode": {
			class:         makeClass(nil, nil),
			shouldSucceed: false,
		},
		"immediate mode": {
			class:         makeClass(&immediateMode1, nil),
			shouldSucceed: true,
		},
		"waiting mode": {
			class:         makeClass(&waitingMode, nil),
			shouldSucceed: true,
		},
		"invalid mode": {
			class:         makeClass(&invalidMode, nil),
			shouldSucceed: false,
		},
	}

	for testName, testCase := range cases {
		errs := ValidateStorageClass(testCase.class)
		if testCase.shouldSucceed && len(errs) != 0 {
			t.Errorf("Expected success for test %q, got %v", testName, errs)
		}
		if !testCase.shouldSucceed && len(errs) == 0 {
			t.Errorf("Expected failure for test %q, got success", testName)
		}
	}
}

type updateTest struct {
	oldClass      *storage.StorageClass
	newClass      *storage.StorageClass
	shouldSucceed bool
}

func TestValidateUpdateVolumeBindingMode(t *testing.T) {
	noBinding := makeClass(nil, nil)
	immediateBinding1 := makeClass(&immediateMode1, nil)
	immediateBinding2 := makeClass(&immediateMode2, nil)
	waitBinding := makeClass(&waitingMode, nil)

	cases := map[string]updateTest{
		"old and new no mode": {
			oldClass:      noBinding,
			newClass:      noBinding,
			shouldSucceed: true,
		},
		"old and new same mode ptr": {
			oldClass:      immediateBinding1,
			newClass:      immediateBinding1,
			shouldSucceed: true,
		},
		"old and new same mode value": {
			oldClass:      immediateBinding1,
			newClass:      immediateBinding2,
			shouldSucceed: true,
		},
		"old no mode, new mode": {
			oldClass:      noBinding,
			newClass:      waitBinding,
			shouldSucceed: false,
		},
		"old mode, new no mode": {
			oldClass:      waitBinding,
			newClass:      noBinding,
			shouldSucceed: false,
		},
		"old and new different modes": {
			oldClass:      waitBinding,
			newClass:      immediateBinding1,
			shouldSucceed: false,
		},
	}

	for testName, testCase := range cases {
		errs := ValidateStorageClassUpdate(testCase.newClass, testCase.oldClass)
		if testCase.shouldSucceed && len(errs) != 0 {
			t.Errorf("Expected success for %v, got %v", testName, errs)
		}
		if !testCase.shouldSucceed && len(errs) == 0 {
			t.Errorf("Expected failure for %v, got success", testName)
		}
	}
}

func TestValidateAllowedTopologies(t *testing.T) {

	validTopology := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}},
	}, {
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone2"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node2"},
		}},
	}}

	topologyInvalidKey := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "/invalidkey",
			Values: []string{"zone1"},
		}},
	}}

	topologyLackOfValues := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{},
		}},
	}}

	topologyDupValues := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1", "node1"},
		}},
	}}

	topologyMultiValues := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1", "node2"},
		}},
	}}

	topologyEmptyMatchLabelExpressions := []api.TopologySelectorTerm{{
		MatchLabelExpressions: nil,
	}}

	topologyDupKeys := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node2"},
		}},
	}}

	topologyMultiTerm := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}},
	}, {
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node2"},
		}},
	}}

	topologyDupTermsIdentical := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}},
	}, {
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}},
	}}

	topologyExprsOneSameOneDiff := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}},
	}, {
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node2"},
		}},
	}}

	topologyValuesOneSameOneDiff := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1", "node2"},
		}},
	}, {
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1", "node3"},
		}},
	}}

	topologyDupTermsDiffExprOrder := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}, {
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1"},
		}},
	}, {
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1"},
		}, {
			Key:    "kubernetes.io/hostname",
			Values: []string{"node1"},
		}},
	}}

	topologyDupTermsDiffValueOrder := []api.TopologySelectorTerm{{
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone1", "zone2"},
		}},
	}, {
		MatchLabelExpressions: []api.TopologySelectorLabelRequirement{{
			Key:    "failure-domain.beta.kubernetes.io/zone",
			Values: []string{"zone2", "zone1"},
		}},
	}}

	cases := map[string]bindingTest{
		"no topology": {
			class:         makeClass(&waitingMode, nil),
			shouldSucceed: true,
		},
		"valid topology": {
			class:         makeClass(&waitingMode, validTopology),
			shouldSucceed: true,
		},
		"topology invalid key": {
			class:         makeClass(&waitingMode, topologyInvalidKey),
			shouldSucceed: false,
		},
		"topology lack of values": {
			class:         makeClass(&waitingMode, topologyLackOfValues),
			shouldSucceed: false,
		},
		"duplicate TopologySelectorRequirement values": {
			class:         makeClass(&waitingMode, topologyDupValues),
			shouldSucceed: false,
		},
		"multiple TopologySelectorRequirement values": {
			class:         makeClass(&waitingMode, topologyMultiValues),
			shouldSucceed: true,
		},
		"empty MatchLabelExpressions": {
			class:         makeClass(&waitingMode, topologyEmptyMatchLabelExpressions),
			shouldSucceed: true,
		},
		"duplicate MatchLabelExpression keys": {
			class:         makeClass(&waitingMode, topologyDupKeys),
			shouldSucceed: false,
		},
		"duplicate MatchLabelExpression keys but across separate terms": {
			class:         makeClass(&waitingMode, topologyMultiTerm),
			shouldSucceed: true,
		},
		"duplicate AllowedTopologies terms - identical": {
			class:         makeClass(&waitingMode, topologyDupTermsIdentical),
			shouldSucceed: false,
		},
		"two AllowedTopologies terms, with a pair of the same MatchLabelExpressions and a pair of different ones": {
			class:         makeClass(&waitingMode, topologyExprsOneSameOneDiff),
			shouldSucceed: true,
		},
		"two AllowedTopologies terms, with a pair of the same Values and a pair of different ones": {
			class:         makeClass(&waitingMode, topologyValuesOneSameOneDiff),
			shouldSucceed: true,
		},
		"duplicate AllowedTopologies terms - different MatchLabelExpressions order": {
			class:         makeClass(&waitingMode, topologyDupTermsDiffExprOrder),
			shouldSucceed: false,
		},
		"duplicate AllowedTopologies terms - different TopologySelectorRequirement values order": {
			class:         makeClass(&waitingMode, topologyDupTermsDiffValueOrder),
			shouldSucceed: false,
		},
	}

	for testName, testCase := range cases {
		errs := ValidateStorageClass(testCase.class)
		if testCase.shouldSucceed && len(errs) != 0 {
			t.Errorf("Expected success for test %q, got %v", testName, errs)
		}
		if !testCase.shouldSucceed && len(errs) == 0 {
			t.Errorf("Expected failure for test %q, got success", testName)
		}
	}
}

func TestCSINodeValidation(t *testing.T) {
	driverName := "driver-name"
	driverName2 := "1io.kubernetes-storage-2-csi-driver3"
	longName := "my-a-b-c-d-c-f-g-h-i-j-k-l-m-n-o-p-q-r-s-t-u-v-w-x-y-z-ABCDEFGHIJKLMNOPQRSTUVWXYZ-driver" // 88 chars
	nodeID := "nodeA"
	longID := longName + longName + "abcdefghijklmnopqrstuvwxyz" // 202 chars
	successCases := []storage.CSINode{{
		// driver name: dot only
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// driver name: dash only
		ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io-kubernetes-storage-csi-driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// driver name: numbers
		ObjectMeta: metav1.ObjectMeta{Name: "foo3"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "1io-kubernetes-storage-2-csi-driver3",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// driver name: dot, dash
		ObjectMeta: metav1.ObjectMeta{Name: "foo4"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage-csi-driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// driver name: dot, dash, and numbers
		ObjectMeta: metav1.ObjectMeta{Name: "foo5"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         driverName2,
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// Driver name length 1
		ObjectMeta: metav1.ObjectMeta{Name: "foo2"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "a",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// multiple drivers with different node IDs, topology keys
		ObjectMeta: metav1.ObjectMeta{Name: "foo6"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "driver1",
				NodeID:       "node1",
				TopologyKeys: []string{"key1", "key2"},
			}, {
				Name:         "driverB",
				NodeID:       "nodeA",
				TopologyKeys: []string{"keyA", "keyB"},
			}},
		},
	}, {
		// multiple drivers with same node IDs, topology keys
		ObjectMeta: metav1.ObjectMeta{Name: "foo7"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "driver1",
				NodeID:       "node1",
				TopologyKeys: []string{"key1"},
			}, {
				Name:         "driver2",
				NodeID:       "node1",
				TopologyKeys: []string{"key1"},
			}},
		},
	}, {
		// Volume limits being zero
		ObjectMeta: metav1.ObjectMeta{Name: "foo11"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(0)},
			}},
		},
	}, {
		// Volume limits with positive number
		ObjectMeta: metav1.ObjectMeta{Name: "foo11"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(1)},
			}},
		},
	}, {
		// topology key names with -, _, and dot .
		ObjectMeta: metav1.ObjectMeta{Name: "foo8"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "driver1",
				NodeID:       "node1",
				TopologyKeys: []string{"zone_1", "zone.2"},
			}, {
				Name:         "driver2",
				NodeID:       "node1",
				TopologyKeys: []string{"zone-3", "zone.4"},
			}},
		},
	}, {
		// topology prefix with - and dot.
		ObjectMeta: metav1.ObjectMeta{Name: "foo9"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "driver1",
				NodeID:       "node1",
				TopologyKeys: []string{"company-com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// No topology keys
		ObjectMeta: metav1.ObjectMeta{Name: "foo10"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:   driverName,
				NodeID: nodeID,
			}},
		},
	}}

	for _, csiNode := range successCases {
		if errs := ValidateCSINode(&csiNode, shorterIDValidationOption); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	nodeIDCase := storage.CSINode{
		// node ID length > 128 but < 192
		ObjectMeta: metav1.ObjectMeta{Name: "foo7"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         driverName,
				NodeID:       longID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}

	if errs := ValidateCSINode(&nodeIDCase, longerIDValidateOption); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	errorCases := []storage.CSINode{{
		// Empty driver name
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// Invalid start char in driver name
		ObjectMeta: metav1.ObjectMeta{Name: "foo3"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "_io.kubernetes.storage.csi.driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// Invalid end char in driver name
		ObjectMeta: metav1.ObjectMeta{Name: "foo4"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver/",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// Invalid separators in driver name
		ObjectMeta: metav1.ObjectMeta{Name: "foo5"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io/kubernetes/storage/csi~driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// driver name: underscore only
		ObjectMeta: metav1.ObjectMeta{Name: "foo6"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io_kubernetes_storage_csi_driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// Driver name length > 63
		ObjectMeta: metav1.ObjectMeta{Name: "foo7"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         longName,
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// No driver name
		ObjectMeta: metav1.ObjectMeta{Name: "foo8"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// Empty individual topology key
		ObjectMeta: metav1.ObjectMeta{Name: "foo9"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         driverName,
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", ""},
			}},
		},
	}, {
		// duplicate drivers in driver specs
		ObjectMeta: metav1.ObjectMeta{Name: "foo10"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "driver1",
				NodeID:       "node1",
				TopologyKeys: []string{"key1", "key2"},
			}, {
				Name:         "driver1",
				NodeID:       "nodeX",
				TopologyKeys: []string{"keyA", "keyB"},
			}},
		},
	}, {
		// single driver with duplicate topology keys in driver specs
		ObjectMeta: metav1.ObjectMeta{Name: "foo11"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "driver1",
				NodeID:       "node1",
				TopologyKeys: []string{"key1", "key1"},
			}},
		},
	}, {
		// multiple drivers with one set of duplicate topology keys in driver specs
		ObjectMeta: metav1.ObjectMeta{Name: "foo12"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "driver1",
				NodeID:       "node1",
				TopologyKeys: []string{"key1"},
			}, {
				Name:         "driver2",
				NodeID:       "nodeX",
				TopologyKeys: []string{"keyA", "keyA"},
			}},
		},
	}, {
		// Empty NodeID
		ObjectMeta: metav1.ObjectMeta{Name: "foo13"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         driverName,
				NodeID:       "",
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// Volume limits with negative number
		ObjectMeta: metav1.ObjectMeta{Name: "foo11"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(-1)},
			}},
		},
	}, {
		// topology prefix should be lower case
		ObjectMeta: metav1.ObjectMeta{Name: "foo14"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         driverName,
				NodeID:       "node1",
				TopologyKeys: []string{"Company.Com/zone1", "company.com/zone2"},
			}},
		},
	},
		nodeIDCase,
	}

	for _, csiNode := range errorCases {
		if errs := ValidateCSINode(&csiNode, shorterIDValidationOption); len(errs) == 0 {
			t.Errorf("Expected failure for test: %v", csiNode)
		}
	}
}

func TestCSINodeUpdateValidation(t *testing.T) {
	//driverName := "driver-name"
	//driverName2 := "1io.kubernetes-storage-2-csi-driver3"
	//longName := "my-a-b-c-d-c-f-g-h-i-j-k-l-m-n-o-p-q-r-s-t-u-v-w-x-y-z-ABCDEFGHIJKLMNOPQRSTUVWXYZ-driver"
	nodeID := "nodeA"

	old := storage.CSINode{
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(20)},
			}},
		},
	}

	successCases := []storage.CSINode{{
		// no change
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(20)},
			}},
		},
	}, {
		// remove a driver
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}, {
		// add a driver
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(20)},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-3",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(30)},
			}},
		},
	}, {
		// remove a driver and add a driver
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.new-driver",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(30)},
			}},
		},
	}}

	for _, csiNode := range successCases {
		if errs := ValidateCSINodeUpdate(&csiNode, &old, shorterIDValidationOption); len(errs) != 0 {
			t.Errorf("expected success: %+v", errs)
		}
	}

	errorCases := []storage.CSINode{{
		// invalid change node id
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       "nodeB",
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(20)},
			}},
		},
	}, {
		// invalid change topology keys
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(20)},
			}},
		},
	}, {
		// invalid change trying to set a previously unset allocatable
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(10)},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(20)},
			}},
		},
	}, {
		// invalid change trying to update allocatable with a different volume limit
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: utilpointer.Int32(21)},
			}},
		},
	}, {
		// invalid change trying to update allocatable with an empty volume limit
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
				Allocatable:  &storage.VolumeNodeResources{Count: nil},
			}},
		},
	}, {
		// invalid change trying to remove allocatable
		ObjectMeta: metav1.ObjectMeta{Name: "foo1"},
		Spec: storage.CSINodeSpec{
			Drivers: []storage.CSINodeDriver{{
				Name:         "io.kubernetes.storage.csi.driver-1",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}, {
				Name:         "io.kubernetes.storage.csi.driver-2",
				NodeID:       nodeID,
				TopologyKeys: []string{"company.com/zone1", "company.com/zone2"},
			}},
		},
	}}

	for _, csiNode := range errorCases {
		if errs := ValidateCSINodeUpdate(&csiNode, &old, shorterIDValidationOption); len(errs) == 0 {
			t.Errorf("Expected failure for test: %+v", csiNode)
		}
	}
}

func TestCSIDriverValidation(t *testing.T) {
	// assume this feature is on for this test, detailed enabled/disabled tests in TestCSIDriverValidationSELinuxMountEnabledDisabled
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SELinuxMountReadWriteOncePod, true)

	driverName := "test-driver"
	longName := "my-a-b-c-d-c-f-g-h-i-j-k-l-m-n-o-p-q-r-s-t-u-v-w-x-y-z-ABCDEFGHIJKLMNOPQRSTUVWXYZ-driver"
	invalidName := "-invalid-@#$%^&*()-"
	attachRequired := true
	attachNotRequired := false
	podInfoOnMount := true
	notPodInfoOnMount := false
	notRequiresRepublish := false
	storageCapacity := true
	notStorageCapacity := false
	seLinuxMount := true
	notSELinuxMount := false
	supportedFSGroupPolicy := storage.FileFSGroupPolicy
	invalidFSGroupPolicy := storage.FSGroupPolicy("invalid-mode")
	successCases := []storage.CSIDriver{{
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachRequired,
			PodInfoOnMount:    &podInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		// driver name: dot only
		ObjectMeta: metav1.ObjectMeta{Name: "io.kubernetes.storage.csi.driver"},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachRequired,
			PodInfoOnMount:    &podInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &notStorageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		// driver name: dash only
		ObjectMeta: metav1.ObjectMeta{Name: "io-kubernetes-storage-csi-driver"},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		// driver name: numbers
		ObjectMeta: metav1.ObjectMeta{Name: "1csi2driver3"},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachRequired,
			PodInfoOnMount:    &podInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		// driver name: dot and dash
		ObjectMeta: metav1.ObjectMeta{Name: "io.kubernetes.storage.csi-driver"},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachRequired,
			PodInfoOnMount:    &podInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachRequired,
			PodInfoOnMount:    &podInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			VolumeLifecycleModes: []storage.VolumeLifecycleMode{
				storage.VolumeLifecyclePersistent,
			},
			SELinuxMount: &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			VolumeLifecycleModes: []storage.VolumeLifecycleMode{
				storage.VolumeLifecycleEphemeral,
			},
			SELinuxMount: &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			VolumeLifecycleModes: []storage.VolumeLifecycleMode{
				storage.VolumeLifecycleEphemeral,
				storage.VolumeLifecyclePersistent,
			},
			SELinuxMount: &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			VolumeLifecycleModes: []storage.VolumeLifecycleMode{
				storage.VolumeLifecycleEphemeral,
				storage.VolumeLifecyclePersistent,
				storage.VolumeLifecycleEphemeral,
			},
			SELinuxMount: &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			FSGroupPolicy:     &supportedFSGroupPolicy,
			SELinuxMount:      &seLinuxMount,
		},
	}, {
		// SELinuxMount: false
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			StorageCapacity:   &storageCapacity,
			SELinuxMount:      &notSELinuxMount,
		},
	}}

	for _, csiDriver := range successCases {
		if errs := ValidateCSIDriver(&csiDriver); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	errorCases := []storage.CSIDriver{{
		ObjectMeta: metav1.ObjectMeta{Name: invalidName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  &attachRequired,
			PodInfoOnMount:  &podInfoOnMount,
			StorageCapacity: &storageCapacity,
			SELinuxMount:    &seLinuxMount,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: longName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  &attachNotRequired,
			PodInfoOnMount:  &notPodInfoOnMount,
			StorageCapacity: &storageCapacity,
			SELinuxMount:    &seLinuxMount,
		},
	}, {
		// AttachRequired not set
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  nil,
			PodInfoOnMount:  &podInfoOnMount,
			StorageCapacity: &storageCapacity,
			SELinuxMount:    &seLinuxMount,
		},
	}, {
		// PodInfoOnMount not set
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  &attachNotRequired,
			PodInfoOnMount:  nil,
			StorageCapacity: &storageCapacity,
			SELinuxMount:    &seLinuxMount,
		},
	}, {
		// StorageCapacity not set
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  &attachNotRequired,
			PodInfoOnMount:  &podInfoOnMount,
			StorageCapacity: nil,
			SELinuxMount:    &seLinuxMount,
		},
	}, {
		// invalid mode
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  &attachNotRequired,
			PodInfoOnMount:  &notPodInfoOnMount,
			StorageCapacity: &storageCapacity,
			VolumeLifecycleModes: []storage.VolumeLifecycleMode{
				"no-such-mode",
			},
			SELinuxMount: &seLinuxMount,
		},
	}, {
		// invalid fsGroupPolicy
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  &attachNotRequired,
			PodInfoOnMount:  &notPodInfoOnMount,
			FSGroupPolicy:   &invalidFSGroupPolicy,
			StorageCapacity: &storageCapacity,
			SELinuxMount:    &seLinuxMount,
		},
	}, {
		// no SELinuxMount
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired:  &attachNotRequired,
			PodInfoOnMount:  &notPodInfoOnMount,
			StorageCapacity: &storageCapacity,
		},
	}}

	for _, csiDriver := range errorCases {
		if errs := ValidateCSIDriver(&csiDriver); len(errs) == 0 {
			t.Errorf("Expected failure for test: %v", csiDriver)
		}
	}
}

func TestCSIDriverValidationUpdate(t *testing.T) {
	// assume this feature is on for this test, detailed enabled/disabled tests in TestCSIDriverValidationSELinuxMountEnabledDisabled
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SELinuxMountReadWriteOncePod, true)

	driverName := "test-driver"
	longName := "my-a-b-c-d-c-f-g-h-i-j-k-l-m-n-o-p-q-r-s-t-u-v-w-x-y-z-ABCDEFGHIJKLMNOPQRSTUVWXYZ-driver"
	invalidName := "-invalid-@#$%^&*()-"
	attachRequired := true
	attachNotRequired := false
	podInfoOnMount := true
	storageCapacity := true
	notPodInfoOnMount := false
	gcp := "gcp"
	requiresRepublish := true
	notRequiresRepublish := false
	notStorageCapacity := false
	seLinuxMount := true
	notSELinuxMount := false
	resourceVersion := "1"
	old := storage.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: driverName, ResourceVersion: resourceVersion},
		Spec: storage.CSIDriverSpec{
			AttachRequired:    &attachNotRequired,
			PodInfoOnMount:    &notPodInfoOnMount,
			RequiresRepublish: &notRequiresRepublish,
			VolumeLifecycleModes: []storage.VolumeLifecycleMode{
				storage.VolumeLifecycleEphemeral,
				storage.VolumeLifecyclePersistent,
			},
			StorageCapacity: &storageCapacity,
			SELinuxMount:    &seLinuxMount,
		},
	}

	successCases := []struct {
		name   string
		modify func(new *storage.CSIDriver)
	}{{
		name:   "no change",
		modify: func(new *storage.CSIDriver) {},
	}, {
		name: "change TokenRequests",
		modify: func(new *storage.CSIDriver) {
			new.Spec.TokenRequests = []storage.TokenRequest{{Audience: gcp}}
		},
	}, {
		name: "change RequiresRepublish",
		modify: func(new *storage.CSIDriver) {
			new.Spec.RequiresRepublish = &requiresRepublish
		},
	}, {
		name: "StorageCapacity changed",
		modify: func(new *storage.CSIDriver) {
			new.Spec.StorageCapacity = &notStorageCapacity
		},
	}, {
		name: "SELinuxMount changed",
		modify: func(new *storage.CSIDriver) {
			new.Spec.SELinuxMount = &notSELinuxMount
		},
	}, {
		name: "change PodInfoOnMount",
		modify: func(new *storage.CSIDriver) {
			new.Spec.PodInfoOnMount = &podInfoOnMount
		},
	}, {
		name: "change FSGroupPolicy",
		modify: func(new *storage.CSIDriver) {
			fileFSGroupPolicy := storage.FileFSGroupPolicy
			new.Spec.FSGroupPolicy = &fileFSGroupPolicy
		},
	}}
	for _, test := range successCases {
		t.Run(test.name, func(t *testing.T) {
			new := old.DeepCopy()
			test.modify(new)
			if errs := ValidateCSIDriverUpdate(new, &old); len(errs) != 0 {
				t.Errorf("Expected success for %+v: %v", new, errs)
			}
		})
	}

	// Each test case changes exactly one field. None of that is valid.
	errorCases := []struct {
		name   string
		modify func(new *storage.CSIDriver)
	}{{
		name: "invalid name",
		modify: func(new *storage.CSIDriver) {
			new.Name = invalidName
		},
	}, {
		name: "long name",
		modify: func(new *storage.CSIDriver) {
			new.Name = longName
		},
	}, {
		name: "AttachRequired not set",
		modify: func(new *storage.CSIDriver) {
			new.Spec.AttachRequired = nil
		},
	}, {
		name: "AttachRequired changed",
		modify: func(new *storage.CSIDriver) {
			new.Spec.AttachRequired = &attachRequired
		},
	}, {
		name: "PodInfoOnMount not set",
		modify: func(new *storage.CSIDriver) {
			new.Spec.PodInfoOnMount = nil
		},
	}, {
		name: "invalid volume lifecycle mode",
		modify: func(new *storage.CSIDriver) {
			new.Spec.VolumeLifecycleModes = []storage.VolumeLifecycleMode{
				"no-such-mode",
			}
		},
	}, {
		name: "volume lifecycle modes not set",
		modify: func(new *storage.CSIDriver) {
			new.Spec.VolumeLifecycleModes = nil
		},
	}, {
		name: "VolumeLifecyclePersistent removed",
		modify: func(new *storage.CSIDriver) {
			new.Spec.VolumeLifecycleModes = []storage.VolumeLifecycleMode{
				storage.VolumeLifecycleEphemeral,
			}
		},
	}, {
		name: "VolumeLifecycleEphemeral removed",
		modify: func(new *storage.CSIDriver) {
			new.Spec.VolumeLifecycleModes = []storage.VolumeLifecycleMode{
				storage.VolumeLifecyclePersistent,
			}
		},
	}, {
		name: "FSGroupPolicy invalidated",
		modify: func(new *storage.CSIDriver) {
			invalidFSGroupPolicy := storage.FSGroupPolicy("invalid")
			new.Spec.FSGroupPolicy = &invalidFSGroupPolicy
		},
	}, {
		name: "TokenRequests invalidated",
		modify: func(new *storage.CSIDriver) {
			new.Spec.TokenRequests = []storage.TokenRequest{{Audience: gcp}, {Audience: gcp}}
		},
	}, {
		name: "invalid nil StorageCapacity",
		modify: func(new *storage.CSIDriver) {
			new.Spec.StorageCapacity = nil
		},
	}, {
		name: "SELinuxMount not set",
		modify: func(new *storage.CSIDriver) {
			new.Spec.SELinuxMount = nil
		},
	}}

	for _, test := range errorCases {
		t.Run(test.name, func(t *testing.T) {
			new := old.DeepCopy()
			test.modify(new)
			if errs := ValidateCSIDriverUpdate(new, &old); len(errs) == 0 {
				t.Errorf("Expected failure for test: %+v", new)
			}
		})
	}
}

func TestCSIDriverStorageCapacityEnablement(t *testing.T) {
	run := func(t *testing.T, withField bool) {
		driverName := "test-driver"
		attachRequired := true
		podInfoOnMount := true
		requiresRepublish := true
		storageCapacity := true
		seLinuxMount := false
		csiDriver := storage.CSIDriver{
			ObjectMeta: metav1.ObjectMeta{Name: driverName},
			Spec: storage.CSIDriverSpec{
				AttachRequired:    &attachRequired,
				PodInfoOnMount:    &podInfoOnMount,
				RequiresRepublish: &requiresRepublish,
				SELinuxMount:      &seLinuxMount,
			},
		}
		if withField {
			csiDriver.Spec.StorageCapacity = &storageCapacity
		}
		errs := ValidateCSIDriver(&csiDriver)
		success := withField
		if success && len(errs) != 0 {
			t.Errorf("expected success, got: %v", errs)
		}
		if !success && len(errs) == 0 {
			t.Errorf("expected error, got success")
		}
	}

	yesNo := []bool{true, false}
	for _, withField := range yesNo {
		t.Run(fmt.Sprintf("with-field=%v", withField), func(t *testing.T) {
			run(t, withField)
		})
	}
}

func TestValidateCSIStorageCapacity(t *testing.T) {
	storageClassName := "test-sc"
	invalidName := "-invalid-@#$%^&*()-"

	goodCapacity := storage.CSIStorageCapacity{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "csc-329803da-fdd2-42e4-af6f-7b07e7ccc305",
			Namespace: metav1.NamespaceDefault,
		},
		StorageClassName: storageClassName,
	}
	goodTopology := metav1.LabelSelector{
		MatchLabels: map[string]string{"foo": "bar"},
	}

	scenarios := map[string]struct {
		isExpectedFailure bool
		capacity          *storage.CSIStorageCapacity
	}{
		"good-capacity": {
			capacity: &goodCapacity,
		},
		"missing-storage-class-name": {
			isExpectedFailure: true,
			capacity: func() *storage.CSIStorageCapacity {
				capacity := goodCapacity
				capacity.StorageClassName = ""
				return &capacity
			}(),
		},
		"bad-storage-class-name": {
			isExpectedFailure: true,
			capacity: func() *storage.CSIStorageCapacity {
				capacity := goodCapacity
				capacity.StorageClassName = invalidName
				return &capacity
			}(),
		},
		"good-capacity-value": {
			capacity: func() *storage.CSIStorageCapacity {
				capacity := goodCapacity
				capacity.Capacity = resource.NewQuantity(1, resource.BinarySI)
				return &capacity
			}(),
		},
		"bad-capacity-value": {
			isExpectedFailure: true,
			capacity: func() *storage.CSIStorageCapacity {
				capacity := goodCapacity
				capacity.Capacity = resource.NewQuantity(-11, resource.BinarySI)
				return &capacity
			}(),
		},
		"good-topology": {
			capacity: func() *storage.CSIStorageCapacity {
				capacity := goodCapacity
				capacity.NodeTopology = &goodTopology
				return &capacity
			}(),
		},
		"empty-topology": {
			capacity: func() *storage.CSIStorageCapacity {
				capacity := goodCapacity
				capacity.NodeTopology = &metav1.LabelSelector{}
				return &capacity
			}(),
		},
		"bad-topology-fields": {
			isExpectedFailure: true,
			capacity: func() *storage.CSIStorageCapacity {
				capacity := goodCapacity
				capacity.NodeTopology = &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "foo",
						Operator: metav1.LabelSelectorOperator("no-such-operator"),
						Values: []string{
							"bar",
						},
					}},
				}
				return &capacity
			}(),
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			errs := ValidateCSIStorageCapacity(scenario.capacity, CSIStorageCapacityValidateOptions{false})
			if len(errs) == 0 && scenario.isExpectedFailure {
				t.Errorf("Unexpected success")
			}
			if len(errs) > 0 && !scenario.isExpectedFailure {
				t.Errorf("Unexpected failure: %+v", errs)
			}
		})
	}

}

func TestCSIServiceAccountToken(t *testing.T) {
	driverName := "test-driver"
	gcp := "gcp"
	aws := "aws"
	notRequiresRepublish := false
	tests := []struct {
		desc      string
		csiDriver *storage.CSIDriver
		wantErr   bool
	}{{
		desc: "invalid - TokenRequests has tokens with the same audience",
		csiDriver: &storage.CSIDriver{
			ObjectMeta: metav1.ObjectMeta{Name: driverName},
			Spec: storage.CSIDriverSpec{
				TokenRequests:     []storage.TokenRequest{{Audience: gcp}, {Audience: gcp}},
				RequiresRepublish: &notRequiresRepublish,
			},
		},
		wantErr: true,
	}, {
		desc: "invalid - TokenRequests has tokens with ExpirationSeconds less than 10min",
		csiDriver: &storage.CSIDriver{
			ObjectMeta: metav1.ObjectMeta{Name: driverName},
			Spec: storage.CSIDriverSpec{
				TokenRequests:     []storage.TokenRequest{{Audience: gcp, ExpirationSeconds: utilpointer.Int64(10)}},
				RequiresRepublish: &notRequiresRepublish,
			},
		},
		wantErr: true,
	}, {
		desc: "invalid - TokenRequests has tokens with ExpirationSeconds longer than 1<<32 min",
		csiDriver: &storage.CSIDriver{
			ObjectMeta: metav1.ObjectMeta{Name: driverName},
			Spec: storage.CSIDriverSpec{
				TokenRequests:     []storage.TokenRequest{{Audience: gcp, ExpirationSeconds: utilpointer.Int64(1<<32 + 1)}},
				RequiresRepublish: &notRequiresRepublish,
			},
		},
		wantErr: true,
	}, {
		desc: "valid - TokenRequests has at most one token with empty string audience",
		csiDriver: &storage.CSIDriver{
			ObjectMeta: metav1.ObjectMeta{Name: driverName},
			Spec: storage.CSIDriverSpec{
				TokenRequests:     []storage.TokenRequest{{Audience: ""}},
				RequiresRepublish: &notRequiresRepublish,
			},
		},
	}, {
		desc: "valid - TokenRequests has tokens with different audience",
		csiDriver: &storage.CSIDriver{
			ObjectMeta: metav1.ObjectMeta{Name: driverName},
			Spec: storage.CSIDriverSpec{
				TokenRequests:     []storage.TokenRequest{{}, {Audience: gcp}, {Audience: aws}},
				RequiresRepublish: &notRequiresRepublish,
			},
		},
	}}

	for _, test := range tests {
		test.csiDriver.Spec.AttachRequired = new(bool)
		test.csiDriver.Spec.PodInfoOnMount = new(bool)
		test.csiDriver.Spec.StorageCapacity = new(bool)
		test.csiDriver.Spec.SELinuxMount = new(bool)
		if errs := ValidateCSIDriver(test.csiDriver); test.wantErr != (len(errs) != 0) {
			t.Errorf("ValidateCSIDriver = %v, want err: %v", errs, test.wantErr)
		}
	}
}

func TestCSIDriverValidationSELinuxMountEnabledDisabled(t *testing.T) {
	tests := []struct {
		name              string
		featureEnabled    bool
		seLinuxMountValue *bool
		expectError       bool
	}{{
		name:              "feature enabled, nil value",
		featureEnabled:    true,
		seLinuxMountValue: nil,
		expectError:       true,
	}, {
		name:              "feature enabled, non-nil value",
		featureEnabled:    true,
		seLinuxMountValue: utilpointer.Bool(true),
		expectError:       false,
	}, {
		name:              "feature disabled, nil value",
		featureEnabled:    false,
		seLinuxMountValue: nil,
		expectError:       false,
	}, {
		name:              "feature disabled, non-nil value",
		featureEnabled:    false,
		seLinuxMountValue: utilpointer.Bool(true),
		expectError:       false,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SELinuxMountReadWriteOncePod, test.featureEnabled)
			csiDriver := &storage.CSIDriver{
				ObjectMeta: metav1.ObjectMeta{Name: "foo"},
				Spec: storage.CSIDriverSpec{
					AttachRequired:    utilpointer.Bool(true),
					PodInfoOnMount:    utilpointer.Bool(true),
					RequiresRepublish: utilpointer.Bool(true),
					StorageCapacity:   utilpointer.Bool(true),
					SELinuxMount:      test.seLinuxMountValue,
				},
			}
			err := ValidateCSIDriver(csiDriver)
			if test.expectError && err == nil {
				t.Error("Expected validation error, got nil")
			}
			if !test.expectError && err != nil {
				t.Errorf("Validation returned error: %s", err)
			}
		})
	}

	updateTests := []struct {
		name           string
		featureEnabled bool
		oldValue       *bool
		newValue       *bool
		expectError    bool
	}{{
		name:           "feature enabled, nil->nil",
		featureEnabled: true,
		oldValue:       nil,
		newValue:       nil,
		expectError:    true, // populated by defaulting and required when feature is enabled
	}, {
		name:           "feature enabled, nil->set",
		featureEnabled: true,
		oldValue:       nil,
		newValue:       utilpointer.Bool(true),
		expectError:    false,
	}, {
		name:           "feature enabled, set->set",
		featureEnabled: true,
		oldValue:       utilpointer.Bool(true),
		newValue:       utilpointer.Bool(true),
		expectError:    false,
	}, {
		name:           "feature enabled, set->nil",
		featureEnabled: true,
		oldValue:       utilpointer.Bool(true),
		newValue:       nil,
		expectError:    true, // populated by defaulting and required when feature is enabled
	}, {
		name:           "feature disabled, nil->nil",
		featureEnabled: false,
		oldValue:       nil,
		newValue:       nil,
		expectError:    false,
	}, {
		name:           "feature disabled, nil->set",
		featureEnabled: false,
		oldValue:       nil,
		newValue:       utilpointer.Bool(true),
		expectError:    false,
	}, {
		name:           "feature disabled, set->set",
		featureEnabled: false,
		oldValue:       utilpointer.Bool(true),
		newValue:       utilpointer.Bool(true),
		expectError:    false,
	}, {
		name:           "feature disabled, set->nil",
		featureEnabled: false,
		oldValue:       utilpointer.Bool(true),
		newValue:       nil,
		expectError:    false,
	}}
	for _, test := range updateTests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SELinuxMountReadWriteOncePod, test.featureEnabled)
			oldCSIDriver := &storage.CSIDriver{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", ResourceVersion: "1"},
				Spec: storage.CSIDriverSpec{
					AttachRequired:    utilpointer.Bool(true),
					PodInfoOnMount:    utilpointer.Bool(true),
					RequiresRepublish: utilpointer.Bool(true),
					StorageCapacity:   utilpointer.Bool(true),
					SELinuxMount:      test.oldValue,
				},
			}
			newCSIDriver := oldCSIDriver.DeepCopy()
			newCSIDriver.Spec.SELinuxMount = test.newValue
			err := ValidateCSIDriverUpdate(newCSIDriver, oldCSIDriver)
			if test.expectError && err == nil {
				t.Error("Expected validation error, got nil")
			}
			if !test.expectError && err != nil {
				t.Errorf("Validation returned error: %s", err)
			}
		})
	}
}

func TestValidateVolumeAttributesClass(t *testing.T) {
	successCases := []storage.VolumeAttributesClass{
		{
			// driverName without a slash
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "foo",
			Parameters: map[string]string{
				"foo-parameter": "free-form-string",
			},
		},
		{
			// some parameters
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "kubernetes.io/foo",
			Parameters: map[string]string{
				"kubernetes.io/foo-parameter": "free/form/string",
				"foo-parameter":               "free-form-string",
				"foo-parameter2":              "{\"embedded\": \"json\", \"with\": {\"structures\":\"inside\"}}",
				"foo-parameter3":              "",
			},
		}}

	// Success cases are expected to pass validation.
	for testName, v := range successCases {
		if errs := ValidateVolumeAttributesClass(&v); len(errs) != 0 {
			t.Errorf("Expected success for %d, got %v", testName, errs)
		}
	}

	// generate a map longer than maxParameterSize
	longParameters := make(map[string]string)
	totalSize := 0
	for totalSize < maxProvisionerParameterSize {
		k := fmt.Sprintf("param/%d", totalSize)
		v := fmt.Sprintf("value-%d", totalSize)
		longParameters[k] = v
		totalSize = totalSize + len(k) + len(v)
	}

	errorCases := map[string]storage.VolumeAttributesClass{
		"namespace is present": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
			DriverName: "kubernetes.io/foo",
			Parameters: map[string]string{
				"foo-parameter": "free-form-string",
			},
		},
		"invalid driverName": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "kubernetes.io/invalid/foo",
			Parameters: map[string]string{
				"foo-parameter": "free-form-string",
			},
		},
		"invalid driverName with invalid chars": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "^/ ",
			Parameters: map[string]string{
				"foo-parameter": "free-form-string",
			},
		},
		"empty parameters": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "kubernetes.io/foo",
			Parameters: map[string]string{},
		},
		"nil parameters": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "kubernetes.io/foo",
		},
		"invalid empty parameter name": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "kubernetes.io/foo",
			Parameters: map[string]string{
				"": "value",
			},
		},
		"driverName: Required value": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "",
			Parameters: map[string]string{
				"foo-parameter": "free-form-string",
			},
		},
		"driverName: whitespace": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: " ",
			Parameters: map[string]string{
				"foo-parameter": "free-form-string",
			},
		},
		"too long parameters": {
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			DriverName: "kubernetes.io/foo",
			Parameters: longParameters,
		},
	}

	// Error cases are not expected to pass validation.
	for testName, v := range errorCases {
		if errs := ValidateVolumeAttributesClass(&v); len(errs) == 0 {
			t.Errorf("Expected failure for test: %s", testName)
		}
	}
}

func TestValidateVolumeAttributesClassUpdate(t *testing.T) {
	cases := map[string]struct {
		oldClass      *storage.VolumeAttributesClass
		newClass      *storage.VolumeAttributesClass
		shouldSucceed bool
	}{
		"invalid driverName update": {
			oldClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/foo",
			},
			newClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/bar",
			},
			shouldSucceed: false,
		},
		"invalid parameter update which changes values": {
			oldClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/foo",
				Parameters: map[string]string{
					"foo": "bar1",
				},
			},
			newClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/foo",
				Parameters: map[string]string{
					"foo": "bar2",
				},
			},
			shouldSucceed: false,
		},
		"invalid parameter update which add new item": {
			oldClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/foo",
				Parameters: map[string]string{},
			},
			newClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/foo",
				Parameters: map[string]string{
					"foo": "bar",
				},
			},
			shouldSucceed: false,
		},
		"invalid parameter update which remove a item": {
			oldClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/foo",
				Parameters: map[string]string{
					"foo": "bar",
				},
			},
			newClass: &storage.VolumeAttributesClass{
				DriverName: "kubernetes.io/foo",
				Parameters: map[string]string{},
			},
			shouldSucceed: false,
		},
	}

	for testName, testCase := range cases {
		errs := ValidateVolumeAttributesClassUpdate(testCase.newClass, testCase.oldClass)
		if testCase.shouldSucceed && len(errs) != 0 {
			t.Errorf("Expected success for %v, got %v", testName, errs)
		}
		if !testCase.shouldSucceed && len(errs) == 0 {
			t.Errorf("Expected failure for %v, got success", testName)
		}
	}
}
