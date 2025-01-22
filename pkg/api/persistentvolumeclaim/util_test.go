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

package persistentvolumeclaim

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
)

func TestDropDisabledSnapshotDataSource(t *testing.T) {
	pvcWithoutDataSource := func() *core.PersistentVolumeClaim {
		return &core.PersistentVolumeClaim{
			Spec: core.PersistentVolumeClaimSpec{
				DataSource: nil,
			},
		}
	}
	apiGroup := "snapshot.storage.k8s.io"
	pvcWithDataSource := func() *core.PersistentVolumeClaim {
		return &core.PersistentVolumeClaim{
			Spec: core.PersistentVolumeClaimSpec{
				DataSource: &core.TypedLocalObjectReference{
					APIGroup: &apiGroup,
					Kind:     "VolumeSnapshot",
					Name:     "test_snapshot",
				},
			},
		}
	}

	pvcInfo := []struct {
		description string
		pvc         func() *core.PersistentVolumeClaim
	}{
		{
			description: "pvc without DataSource",
			pvc:         pvcWithoutDataSource,
		},
		{
			description: "pvc with DataSource",
			pvc:         pvcWithDataSource,
		},
		{
			description: "is nil",
			pvc:         func() *core.PersistentVolumeClaim { return nil },
		},
	}

	for _, oldpvcInfo := range pvcInfo {
		for _, newpvcInfo := range pvcInfo {
			oldpvc := oldpvcInfo.pvc()
			newpvc := newpvcInfo.pvc()
			if newpvc == nil {
				continue
			}

			t.Run(fmt.Sprintf("old pvc %v, new pvc %v", oldpvcInfo.description, newpvcInfo.description), func(t *testing.T) {
				EnforceDataSourceBackwardsCompatibility(&newpvc.Spec, nil)

				// old pvc should never be changed
				if !reflect.DeepEqual(oldpvc, oldpvcInfo.pvc()) {
					t.Errorf("old pvc changed: %v", cmp.Diff(oldpvc, oldpvcInfo.pvc()))
				}

				// new pvc should not be changed
				if !reflect.DeepEqual(newpvc, newpvcInfo.pvc()) {
					t.Errorf("new pvc changed: %v", cmp.Diff(newpvc, newpvcInfo.pvc()))
				}
			})
		}
	}
}

// TestPVCDataSourceSpecFilter checks to ensure the DropDisabledFields function behaves correctly for PVCDataSource featuregate
func TestPVCDataSourceSpecFilter(t *testing.T) {
	apiGroup := ""
	validSpec := core.PersistentVolumeClaimSpec{
		DataSource: &core.TypedLocalObjectReference{
			APIGroup: &apiGroup,
			Kind:     "PersistentVolumeClaim",
			Name:     "test_clone",
		},
	}
	validSpecNilAPIGroup := core.PersistentVolumeClaimSpec{
		DataSource: &core.TypedLocalObjectReference{
			Kind: "PersistentVolumeClaim",
			Name: "test_clone",
		},
	}

	invalidAPIGroup := "invalid.pvc.api.group"
	invalidSpec := core.PersistentVolumeClaimSpec{
		DataSource: &core.TypedLocalObjectReference{
			APIGroup: &invalidAPIGroup,
			Kind:     "PersistentVolumeClaim",
			Name:     "test_clone_invalid",
		},
	}

	var tests = map[string]struct {
		spec core.PersistentVolumeClaimSpec
		want *core.TypedLocalObjectReference
	}{
		"enabled with empty ds": {
			spec: core.PersistentVolumeClaimSpec{},
			want: nil,
		},
		"enabled with invalid spec": {
			spec: invalidSpec,
			want: nil,
		},
		"enabled with valid spec": {
			spec: validSpec,
			want: validSpec.DataSource,
		},
		"enabled with valid spec but nil APIGroup": {
			spec: validSpecNilAPIGroup,
			want: validSpecNilAPIGroup.DataSource,
		},
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			EnforceDataSourceBackwardsCompatibility(&test.spec, nil)
			if test.spec.DataSource != test.want {
				t.Errorf("expected drop datasource condition was not met, test: %s, spec: %v, expected: %v", testName, test.spec, test.want)
			}

		})
	}
}

var (
	coreGroup    = ""
	snapGroup    = "snapshot.storage.k8s.io"
	genericGroup = "generic.storage.k8s.io"
	pvcKind      = "PersistentVolumeClaim"
	snapKind     = "VolumeSnapshot"
	genericKind  = "Generic"
	podKind      = "Pod"
)

func makeDataSource(apiGroup, kind, name string) *core.TypedLocalObjectReference {
	return &core.TypedLocalObjectReference{
		APIGroup: &apiGroup,
		Kind:     kind,
		Name:     name,
	}
}

func makeDataSourceRef(apiGroup, kind, name string, namespace *string) *core.TypedObjectReference {
	return &core.TypedObjectReference{
		APIGroup:  &apiGroup,
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
	}
}

// TestDataSourceFilter checks to ensure the AnyVolumeDataSource feature gate and CrossNamespaceVolumeDataSource works
func TestDataSourceFilter(t *testing.T) {
	ns := "ns1"
	volumeDataSource := makeDataSource(coreGroup, pvcKind, "my-vol")
	volumeDataSourceRef := makeDataSourceRef(coreGroup, pvcKind, "my-vol", nil)
	xnsVolumeDataSourceRef := makeDataSourceRef(coreGroup, pvcKind, "my-vol", &ns)

	var tests = map[string]struct {
		spec       core.PersistentVolumeClaimSpec
		oldSpec    core.PersistentVolumeClaimSpec
		anyEnabled bool
		xnsEnabled bool
		want       *core.TypedLocalObjectReference
		wantRef    *core.TypedObjectReference
	}{
		"any disabled with empty ds": {
			spec: core.PersistentVolumeClaimSpec{},
		},
		"any disabled with volume ds": {
			spec: core.PersistentVolumeClaimSpec{DataSource: volumeDataSource},
			want: volumeDataSource,
		},
		"any disabled with volume ds ref": {
			spec: core.PersistentVolumeClaimSpec{DataSourceRef: volumeDataSourceRef},
		},
		"any disabled with both data sources": {
			spec: core.PersistentVolumeClaimSpec{DataSource: volumeDataSource, DataSourceRef: volumeDataSourceRef},
			want: volumeDataSource,
		},
		"any enabled with empty ds": {
			spec:       core.PersistentVolumeClaimSpec{},
			anyEnabled: true,
		},
		"any enabled with volume ds": {
			spec:       core.PersistentVolumeClaimSpec{DataSource: volumeDataSource},
			anyEnabled: true,
			want:       volumeDataSource,
		},
		"any enabled with volume ds ref": {
			spec:       core.PersistentVolumeClaimSpec{DataSourceRef: volumeDataSourceRef},
			anyEnabled: true,
			wantRef:    volumeDataSourceRef,
		},
		"any enabled with both data sources": {
			spec:       core.PersistentVolumeClaimSpec{DataSource: volumeDataSource, DataSourceRef: volumeDataSourceRef},
			anyEnabled: true,
			want:       volumeDataSource,
			wantRef:    volumeDataSourceRef,
		},
		"both any and xns enabled with xns volume ds": {
			spec:       core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			anyEnabled: true,
			xnsEnabled: true,
			wantRef:    xnsVolumeDataSourceRef,
		},
		"both any and xns enabled with xns volume ds when xns volume exists in oldSpec": {
			spec:       core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			oldSpec:    core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			anyEnabled: true,
			xnsEnabled: true,
			wantRef:    xnsVolumeDataSourceRef,
		},
		"only xns enabled with xns volume ds": {
			spec:       core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			xnsEnabled: true,
		},
		"only any enabled with xns volume ds": {
			spec:       core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			anyEnabled: true,
		},
		"only any enabled with xns volume ds when xns volume exists in oldSpec": {
			spec:       core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			oldSpec:    core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			anyEnabled: true,
			wantRef:    xnsVolumeDataSourceRef, // existing field isn't dropped.
		},
		"only any enabled with xns volume ds when volume exists in oldSpec": {
			spec:       core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			oldSpec:    core.PersistentVolumeClaimSpec{DataSourceRef: volumeDataSourceRef},
			anyEnabled: true,
			wantRef:    xnsVolumeDataSourceRef, // existing field isn't dropped.8
		},
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			// TODO: this will be removed in 1.36
			featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.32"))
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AnyVolumeDataSource, test.anyEnabled)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CrossNamespaceVolumeDataSource, test.xnsEnabled)
			DropDisabledFields(&test.spec, &test.oldSpec)
			if test.spec.DataSource != test.want {
				t.Errorf("expected condition was not met, test: %s, anyEnabled: %v, xnsEnabled: %v, spec: %+v, expected DataSource: %+v",
					testName, test.anyEnabled, test.xnsEnabled, test.spec, test.want)
			}
			if test.spec.DataSourceRef != test.wantRef {
				t.Errorf("expected condition was not met, test: %s, anyEnabled: %v, xnsEnabled: %v, spec: %+v, expected DataSourceRef: %+v",
					testName, test.anyEnabled, test.xnsEnabled, test.spec, test.wantRef)
			}
		})
	}
}

// TestDataSourceRef checks to ensure the DataSourceRef field handles backwards
// compatibility with the DataSource field
func TestDataSourceRef(t *testing.T) {
	ns := "ns1"
	volumeDataSource := makeDataSource(coreGroup, pvcKind, "my-vol")
	volumeDataSourceRef := makeDataSourceRef(coreGroup, pvcKind, "my-vol", nil)
	xnsVolumeDataSourceRef := makeDataSourceRef(coreGroup, pvcKind, "my-vol", &ns)
	snapshotDataSource := makeDataSource(snapGroup, snapKind, "my-snap")
	snapshotDataSourceRef := makeDataSourceRef(snapGroup, snapKind, "my-snap", nil)
	xnsSnapshotDataSourceRef := makeDataSourceRef(snapGroup, snapKind, "my-snap", &ns)
	genericDataSource := makeDataSource(genericGroup, genericKind, "my-foo")
	genericDataSourceRef := makeDataSourceRef(genericGroup, genericKind, "my-foo", nil)
	xnsGenericDataSourceRef := makeDataSourceRef(genericGroup, genericKind, "my-foo", &ns)
	coreDataSource := makeDataSource(coreGroup, podKind, "my-pod")
	coreDataSourceRef := makeDataSourceRef(coreGroup, podKind, "my-pod", nil)
	xnsCoreDataSourceRef := makeDataSourceRef(coreGroup, podKind, "my-pod", &ns)

	var tests = map[string]struct {
		spec    core.PersistentVolumeClaimSpec
		want    *core.TypedLocalObjectReference
		wantRef *core.TypedObjectReference
	}{
		"empty ds": {
			spec: core.PersistentVolumeClaimSpec{},
		},
		"volume ds": {
			spec:    core.PersistentVolumeClaimSpec{DataSource: volumeDataSource},
			want:    volumeDataSource,
			wantRef: volumeDataSourceRef,
		},
		"snapshot ds": {
			spec:    core.PersistentVolumeClaimSpec{DataSource: snapshotDataSource},
			want:    snapshotDataSource,
			wantRef: snapshotDataSourceRef,
		},
		"generic ds": {
			spec:    core.PersistentVolumeClaimSpec{DataSource: genericDataSource},
			want:    genericDataSource,
			wantRef: genericDataSourceRef,
		},
		"core ds": {
			spec:    core.PersistentVolumeClaimSpec{DataSource: coreDataSource},
			want:    coreDataSource,
			wantRef: coreDataSourceRef,
		},
		"volume ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: volumeDataSourceRef},
			want:    volumeDataSource,
			wantRef: volumeDataSourceRef,
		},
		"snapshot ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: snapshotDataSourceRef},
			want:    snapshotDataSource,
			wantRef: snapshotDataSourceRef,
		},
		"generic ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: genericDataSourceRef},
			want:    genericDataSource,
			wantRef: genericDataSourceRef,
		},
		"core ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: coreDataSourceRef},
			want:    coreDataSource,
			wantRef: coreDataSourceRef,
		},
		"xns volume ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: xnsVolumeDataSourceRef},
			wantRef: xnsVolumeDataSourceRef,
		},
		"xns snapshot ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: xnsSnapshotDataSourceRef},
			wantRef: xnsSnapshotDataSourceRef,
		},
		"xns generic ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: xnsGenericDataSourceRef},
			wantRef: xnsGenericDataSourceRef,
		},
		"xns core ds ref": {
			spec:    core.PersistentVolumeClaimSpec{DataSourceRef: xnsCoreDataSourceRef},
			wantRef: xnsCoreDataSourceRef,
		},
	}

	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AnyVolumeDataSource, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CrossNamespaceVolumeDataSource, true)

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			NormalizeDataSources(&test.spec)
			if !reflect.DeepEqual(test.spec.DataSource, test.want) {
				t.Errorf("expected condition was not met, test: %s, spec.datasource: %+v, want: %+v",
					testName, test.spec.DataSource, test.want)
			}
			if !reflect.DeepEqual(test.spec.DataSourceRef, test.wantRef) {
				t.Errorf("expected condition was not met, test: %s, spec.datasourceRef: %+v, wantRef: %+v",
					testName, test.spec.DataSourceRef, test.wantRef)
			}
		})
	}
}

func TestDropDisabledVolumeAttributesClass(t *testing.T) {
	vacName := ptr.To("foo")

	var tests = map[string]struct {
		spec       core.PersistentVolumeClaimSpec
		oldSpec    core.PersistentVolumeClaimSpec
		vacEnabled bool
		wantVAC    *string
	}{
		"vac disabled with empty vac": {
			spec: core.PersistentVolumeClaimSpec{},
		},
		"vac disabled with vac": {
			spec: core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
		},
		"vac enabled with empty vac": {
			spec:       core.PersistentVolumeClaimSpec{},
			vacEnabled: true,
		},
		"vac enabled with vac": {
			spec:       core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
			vacEnabled: true,
			wantVAC:    vacName,
		},
		"vac disabled with vac when vac doesn't exists in oldSpec": {
			spec:    core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
			oldSpec: core.PersistentVolumeClaimSpec{},
		},
		"vac disabled with vac when vac exists in oldSpec": {
			spec:       core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
			oldSpec:    core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
			vacEnabled: false,
			wantVAC:    vacName,
		},
		"vac enabled with vac when vac doesn't exists in oldSpec": {
			spec:       core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
			oldSpec:    core.PersistentVolumeClaimSpec{},
			vacEnabled: true,
			wantVAC:    vacName,
		},
		"vac enable with vac when vac exists in oldSpec": {
			spec:       core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
			oldSpec:    core.PersistentVolumeClaimSpec{VolumeAttributesClassName: vacName},
			vacEnabled: true,
			wantVAC:    vacName,
		},
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, test.vacEnabled)
			DropDisabledFields(&test.spec, &test.oldSpec)
			if test.spec.VolumeAttributesClassName != test.wantVAC {
				t.Errorf("expected vac was not met, test: %s, vacEnabled: %v, spec: %+v, expected VAC: %+v",
					testName, test.vacEnabled, test.spec, test.wantVAC)
			}
		})
	}
}

func TestDropDisabledFieldsFromStatus(t *testing.T) {
	tests := []struct {
		name                                string
		enableRecoverVolumeExpansionFailure bool
		enableVolumeAttributesClass         bool
		pvc                                 *core.PersistentVolumeClaim
		oldPVC                              *core.PersistentVolumeClaim
		expected                            *core.PersistentVolumeClaim
	}{
		{
			name:                                "for:newPVC=hasAllocatedResource,oldPVC=doesnot,featuregate=false; should drop field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withAllocatedResource("5G"),
			oldPVC:                              getPVC(),
			expected:                            getPVC(),
		},
		{
			name:                                "for:newPVC=hasAllocatedResource,oldPVC=doesnot,featuregate=RecoverVolumeExpansionFailure=true; should keep field",
			enableRecoverVolumeExpansionFailure: true,
			enableVolumeAttributesClass:         false,
			pvc:                                 withAllocatedResource("5G"),
			oldPVC:                              getPVC(),
			expected:                            withAllocatedResource("5G"),
		},
		{
			name:                                "for:newPVC=hasAllocatedResource,oldPVC=hasAllocatedResource,featuregate=RecoverVolumeExpansionFailure=true; should keep field",
			enableRecoverVolumeExpansionFailure: true,
			enableVolumeAttributesClass:         false,
			pvc:                                 withAllocatedResource("5G"),
			oldPVC:                              withAllocatedResource("5G"),
			expected:                            withAllocatedResource("5G"),
		},
		{
			name:                                "for:newPVC=hasAllocatedResource,oldPVC=hasAllocatedResource,featuregate=false; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withAllocatedResource("10G"),
			oldPVC:                              withAllocatedResource("5G"),
			expected:                            withAllocatedResource("10G"),
		},
		{
			name:                                "for:newPVC=hasAllocatedResource,oldPVC=nil,featuregate=false; should drop field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withAllocatedResource("5G"),
			oldPVC:                              nil,
			expected:                            getPVC(),
		},
		{
			name:                                "for:newPVC=hasResizeStatus,oldPVC=nil, featuregate=false should drop field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
			oldPVC:                              nil,
			expected:                            getPVC(),
		},
		{
			name:                                "for:newPVC=hasResizeStatus,oldPVC=doesnot,featuregate=RecoverVolumeExpansionFailure=true; should keep field",
			enableRecoverVolumeExpansionFailure: true,
			enableVolumeAttributesClass:         false,
			pvc:                                 withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
			oldPVC:                              getPVC(),
			expected:                            withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
		},
		{
			name:                                "for:newPVC=hasResizeStatus,oldPVC=hasResizeStatus,featuregate=RecoverVolumeExpansionFailure=true; should keep field",
			enableRecoverVolumeExpansionFailure: true,
			enableVolumeAttributesClass:         false,
			pvc:                                 withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
			oldPVC:                              withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
			expected:                            withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
		},
		{
			name:                                "for:newPVC=hasResizeStatus,oldPVC=hasResizeStatus,featuregate=false; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
			oldPVC:                              withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
			expected:                            withResizeStatus(core.PersistentVolumeClaimNodeResizeInfeasible),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributeClass,oldPVC=nil, featuregate=false should drop field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withVolumeAttributesClassName("foo"),
			oldPVC:                              nil,
			expected:                            getPVC(),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributeClass,oldPVC=doesnot,featuregate=VolumeAttributesClass=true; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         true,
			pvc:                                 withVolumeAttributesClassName("foo"),
			oldPVC:                              getPVC(),
			expected:                            withVolumeAttributesClassName("foo"),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributeClass,oldPVC=hasVolumeAttributeClass,featuregate=VolumeAttributesClass=true; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         true,
			pvc:                                 withVolumeAttributesClassName("foo"),
			oldPVC:                              withVolumeAttributesClassName("foo"),
			expected:                            withVolumeAttributesClassName("foo"),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributeClass,oldPVC=hasVolumeAttributeClass,featuregate=false; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withVolumeAttributesClassName("foo"),
			oldPVC:                              withVolumeAttributesClassName("foo"),
			expected:                            withVolumeAttributesClassName("foo"),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributesModifyStatus,oldPVC=nil, featuregate=false should drop field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
			oldPVC:                              nil,
			expected:                            getPVC(),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributesModifyStatus,oldPVC=doesnot,featuregate=VolumeAttributesClass=true; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         true,
			pvc:                                 withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
			oldPVC:                              getPVC(),
			expected:                            withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributesModifyStatus,oldPVC=hasVolumeAttributesModifyStatus,featuregate=VolumeAttributesClass=true; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         true,
			pvc:                                 withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
			oldPVC:                              withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
			expected:                            withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
		},
		{
			name:                                "for:newPVC=hasVolumeAttributesModifyStatus,oldPVC=hasVolumeAttributesModifyStatus,featuregate=false; should keep field",
			enableRecoverVolumeExpansionFailure: false,
			enableVolumeAttributesClass:         false,
			pvc:                                 withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
			oldPVC:                              withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
			expected:                            withVolumeAttributesModifyStatus("bar", core.PersistentVolumeClaimModifyVolumePending),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RecoverVolumeExpansionFailure, test.enableRecoverVolumeExpansionFailure)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, test.enableVolumeAttributesClass)

			DropDisabledFieldsFromStatus(test.pvc, test.oldPVC)

			if !reflect.DeepEqual(*test.expected, *test.pvc) {
				t.Errorf("Unexpected change: %+v", cmp.Diff(test.expected, test.pvc))
			}
		})
	}
}

func getPVC() *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{}
}

func withAllocatedResource(q string) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		Status: core.PersistentVolumeClaimStatus{
			AllocatedResources: core.ResourceList{
				core.ResourceStorage: resource.MustParse(q),
			},
		},
	}
}

func withResizeStatus(status core.ClaimResourceStatus) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		Status: core.PersistentVolumeClaimStatus{
			AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
				core.ResourceStorage: status,
			},
		},
	}
}

func withVolumeAttributesClassName(vacName string) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		Status: core.PersistentVolumeClaimStatus{
			CurrentVolumeAttributesClassName: &vacName,
		},
	}
}

func withVolumeAttributesModifyStatus(target string, status core.PersistentVolumeClaimModifyVolumeStatus) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		Status: core.PersistentVolumeClaimStatus{
			ModifyVolumeStatus: &core.ModifyVolumeStatus{
				TargetVolumeAttributesClassName: target,
				Status:                          status,
			},
		},
	}
}

func TestWarnings(t *testing.T) {
	testcases := []struct {
		name     string
		template *core.PersistentVolumeClaim
		expected []string
	}{
		{
			name:     "null",
			template: nil,
			expected: nil,
		},
		{
			name: "200Mi requests no warning",
			template: &core.PersistentVolumeClaim{
				Spec: core.PersistentVolumeClaimSpec{
					Resources: core.VolumeResourceRequirements{
						Requests: core.ResourceList{
							core.ResourceStorage: resource.MustParse("200Mi"),
						},
						Limits: core.ResourceList{
							core.ResourceStorage: resource.MustParse("200Mi"),
						},
					},
				},
			},
			expected: nil,
		},
		{
			name: "200m warning",
			template: &core.PersistentVolumeClaim{
				Spec: core.PersistentVolumeClaimSpec{
					Resources: core.VolumeResourceRequirements{
						Requests: core.ResourceList{
							core.ResourceStorage: resource.MustParse("200m"),
						},
						Limits: core.ResourceList{
							core.ResourceStorage: resource.MustParse("100m"),
						},
					},
				},
			},
			expected: []string{
				`spec.resources.requests[storage]: fractional byte value "200m" is invalid, must be an integer`,
				`spec.resources.limits[storage]: fractional byte value "100m" is invalid, must be an integer`,
			},
		},
		{
			name: "integer no warning",
			template: &core.PersistentVolumeClaim{
				Spec: core.PersistentVolumeClaimSpec{
					Resources: core.VolumeResourceRequirements{
						Requests: core.ResourceList{
							core.ResourceStorage: resource.MustParse("200"),
						},
					},
				},
			},
			expected: nil,
		},
		{
			name: "storageclass annotations warning",
			template: &core.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
					Annotations: map[string]string{
						core.BetaStorageClassAnnotation: "",
					},
				},
			},
			expected: []string{
				`metadata.annotations[volume.beta.kubernetes.io/storage-class]: deprecated since v1.8; use "storageClassName" attribute instead`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run("pvcspec_"+tc.name, func(t *testing.T) {
			actual := sets.New[string](GetWarningsForPersistentVolumeClaim(tc.template)...)
			expected := sets.New[string](tc.expected...)
			for _, missing := range sets.List[string](expected.Difference(actual)) {
				t.Errorf("missing: %s", missing)
			}
			for _, extra := range sets.List[string](actual.Difference(expected)) {
				t.Errorf("extra: %s", extra)
			}
		})

	}
}
