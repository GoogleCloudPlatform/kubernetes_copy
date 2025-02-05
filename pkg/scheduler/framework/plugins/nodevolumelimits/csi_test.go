/*
Copyright 2019 The Kubernetes Authors.

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

package nodevolumelimits

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	csitrans "k8s.io/csi-translation-lib"
	csilibplugins "k8s.io/csi-translation-lib/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

const (
	ebsCSIDriverName = csilibplugins.AWSEBSDriverName
	gceCSIDriverName = csilibplugins.GCEPDDriverName

	hostpathInTreePluginName = "kubernetes.io/hostpath"
)

var (
	scName = "csi-sc"
)

var statusCmpOpts = []cmp.Option{
	cmp.Comparer(func(s1 *framework.Status, s2 *framework.Status) bool {
		if s1 == nil || s2 == nil {
			return s1.IsSuccess() && s2.IsSuccess()
		}
		if s1.Code() == framework.Error {
			return s1.AsError().Error() == s2.AsError().Error()
		}
		return s1.Code() == s2.Code() && s1.Plugin() == s2.Plugin() && s1.Message() == s2.Message()
	}),
}

func TestCSILimits(t *testing.T) {
	runningPod := st.MakePod().PVC("csi-ebs.csi.aws.com-3").Obj()
	pendingVolumePod := st.MakePod().PVC("csi-4").Obj()

	// Different pod than pendingVolumePod, but using the same unbound PVC
	unboundPVCPod2 := st.MakePod().PVC("csi-4").Obj()

	missingPVPod := st.MakePod().PVC("csi-6").Obj()
	noSCPVCPod := st.MakePod().PVC("csi-5").Obj()

	gceTwoVolPod := st.MakePod().PVC("csi-pd.csi.storage.gke.io-1").PVC("csi-pd.csi.storage.gke.io-2").Obj()

	// In-tree volumes
	inTreeOneVolPod := st.MakePod().PVC("csi-kubernetes.io/aws-ebs-0").Obj()
	inTreeTwoVolPod := st.MakePod().PVC("csi-kubernetes.io/aws-ebs-1").PVC("csi-kubernetes.io/aws-ebs-2").Obj()

	// pods with matching csi driver names
	csiEBSOneVolPod := st.MakePod().PVC("csi-ebs.csi.aws.com-0").Obj()
	csiEBSTwoVolPod := st.MakePod().PVC("csi-ebs.csi.aws.com-1").PVC("csi-ebs.csi.aws.com-2").Obj()

	inTreeNonMigratableOneVolPod := st.MakePod().PVC("csi-kubernetes.io/hostpath-0").Obj()

	ephemeralVolumePod := st.MakePod().Name("abc").Namespace("test").UID("12345").Volume(
		v1.Volume{
			Name: "xyz",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{},
			},
		}).Obj()

	controller := true
	ephemeralClaim := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralVolumePod.Namespace,
			Name:      ephemeralVolumePod.Name + "-" + ephemeralVolumePod.Spec.Volumes[0].Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Pod",
					Name:       ephemeralVolumePod.Name,
					UID:        ephemeralVolumePod.UID,
					Controller: &controller,
				},
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
	}
	conflictingClaim := ephemeralClaim.DeepCopy()
	conflictingClaim.OwnerReferences = nil

	ephemeralTwoVolumePod := st.MakePod().Name("abc").Namespace("test").UID("12345II").Volume(v1.Volume{
		Name: "x",
		VolumeSource: v1.VolumeSource{
			Ephemeral: &v1.EphemeralVolumeSource{},
		},
	}).Volume(v1.Volume{
		Name: "y",
		VolumeSource: v1.VolumeSource{
			Ephemeral: &v1.EphemeralVolumeSource{},
		},
	}).Obj()

	ephemeralClaimX := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralTwoVolumePod.Namespace,
			Name:      ephemeralTwoVolumePod.Name + "-" + ephemeralTwoVolumePod.Spec.Volumes[0].Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Pod",
					Name:       ephemeralTwoVolumePod.Name,
					UID:        ephemeralTwoVolumePod.UID,
					Controller: &controller,
				},
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
	}
	ephemeralClaimY := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralTwoVolumePod.Namespace,
			Name:      ephemeralTwoVolumePod.Name + "-" + ephemeralTwoVolumePod.Spec.Volumes[1].Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Pod",
					Name:       ephemeralTwoVolumePod.Name,
					UID:        ephemeralTwoVolumePod.UID,
					Controller: &controller,
				},
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
	}
	inTreeInlineVolPod := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
							VolumeID: "aws-inline1",
						},
					},
				},
			},
		},
	}
	inTreeInlineVolPodWithSameCSIVolumeID := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
							VolumeID: "csi-ebs.csi.aws.com-1",
						},
					},
				},
			},
		},
	}
	onlyConfigmapAndSecretVolPod := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
			},
		},
	}
	pvcPodWithConfigmapAndSecret := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: "csi-ebs.csi.aws.com-0"},
					},
				},
			},
		},
	}
	ephemeralPodWithConfigmapAndSecret := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralVolumePod.Namespace,
			Name:      ephemeralVolumePod.Name,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
				{
					Name: "xyz",
					VolumeSource: v1.VolumeSource{
						Ephemeral: &v1.EphemeralVolumeSource{},
					},
				},
			},
		},
	}
	inlineMigratablePodWithConfigmapAndSecret := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
							VolumeID: "aws-inline1",
						},
					},
				},
			},
		},
	}
	tests := []struct {
		newPod              *v1.Pod
		existingPods        []*v1.Pod
		extraClaims         []v1.PersistentVolumeClaim
		filterName          string
		maxVols             int32
		vaCount             int
		driverNames         []string
		test                string
		migrationEnabled    bool
		ephemeralEnabled    bool
		limitSource         string
		wantStatus          *framework.Status
		wantPreFilterStatus *framework.Status
	}{
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			vaCount:      2,
			test:         "should count VolumeAttachments towards volume limit when no pods exist",
			limitSource:  "csinode",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			vaCount:      1,
			test:         "should schedule pod when VolumeAttachments count does not exceed limit",
			limitSource:  "csinode",
		},
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      4,
			driverNames:  []string{ebsCSIDriverName},
			test:         "fits when node volume limit >= new pods CSI volume",
			limitSource:  "csinode",
		},
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "doesn't when node volume limit <= pods CSI volume",
			limitSource:  "csinode",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "should when driver does not support volume limits",
			limitSource:  "csinode-with-no-limit",
		},
		// should count pending PVCs
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{pendingVolumePod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "count pending PVCs towards volume limit <= pods CSI volume",
			limitSource:  "csinode",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// two same pending PVCs should be counted as 1
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{pendingVolumePod, unboundPVCPod2, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      4,
			driverNames:  []string{ebsCSIDriverName},
			test:         "count multiple pending pvcs towards volume limit >= pods CSI volume",
			limitSource:  "csinode",
		},
		// should count PVCs with invalid PV name but valid SC
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{missingPVPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "should count PVCs with invalid PV name but valid SC",
			limitSource:  "csinode",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// don't count a volume which has storageclass missing
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, noSCPVCPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "don't count pvcs with missing SC towards volume limit",
			limitSource:  "csinode",
		},
		// don't count multiple volume types
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{gceTwoVolPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName, gceCSIDriverName},
			test:         "count pvcs with the same type towards volume limit",
			limitSource:  "csinode",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:       gceTwoVolPod,
			existingPods: []*v1.Pod{csiEBSTwoVolPod, runningPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName, gceCSIDriverName},
			test:         "don't count pvcs with different type towards volume limit",
			limitSource:  "csinode",
		},
		// Tests for in-tree volume migration
		{
			newPod:           inTreeOneVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree volumes if migration is enabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeInlineVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			test:             "nil csi node",
		},
		{
			newPod:           pendingVolumePod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count unbound in-tree volumes if migration is enabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeOneVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode-with-no-limit",
			test:             "should not limit pod if volume used does not report limits",
		},
		{
			newPod:           inTreeNonMigratableOneVolPod,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{hostpathInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should not count non-migratable in-tree volumes",
		},
		{
			newPod:           inTreeInlineVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree inline volumes if migration is enabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// mixed volumes
		{
			newPod:           inTreeOneVolPod,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree and csi volumes if migration is enabled (when scheduling in-tree volumes)",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeInlineVolPod,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod, inTreeOneVolPod},
			filterName:       "csi",
			maxVols:          3,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree, inline and csi volumes if migration is enabled (when scheduling in-tree volumes)",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeInlineVolPodWithSameCSIVolumeID,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod, inTreeOneVolPod},
			filterName:       "csi",
			maxVols:          3,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should not count in-tree, inline and csi volumes if migration is enabled (when scheduling in-tree volumes)",
		},
		{
			newPod:           csiEBSOneVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree and csi volumes if migration is enabled (when scheduling csi volumes)",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// ephemeral volumes
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			driverNames:      []string{ebsCSIDriverName},
			limitSource:      "csinode-with-no-limit",
			test:             "ephemeral volume missing",
			wantStatus:       framework.NewStatus(framework.UnschedulableAndUnresolvable, `looking up PVC test/abc-xyz: persistentvolumeclaims "abc-xyz" not found`),
		},
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*conflictingClaim},
			driverNames:      []string{ebsCSIDriverName},
			limitSource:      "csinode-with-no-limit",
			test:             "ephemeral volume not owned",
			wantStatus:       framework.AsStatus(errors.New("PVC test/abc-xyz was not created for pod test/abc (pod is not owner)")),
		},
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			limitSource:      "csinode-with-no-limit",
			test:             "ephemeral volume unbound",
		},
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, csiEBSTwoVolPod},
			maxVols:          2,
			limitSource:      "csinode",
			test:             "ephemeral doesn't when node volume limit <= pods CSI volume",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaimX, *ephemeralClaimY},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralTwoVolumePod},
			maxVols:          2,
			limitSource:      "csinode",
			test:             "ephemeral doesn't when node volume limit <= pods ephemeral CSI volume",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: false,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralVolumePod, csiEBSTwoVolPod},
			maxVols:          3,
			limitSource:      "csinode",
			test:             "persistent doesn't when node volume limit <= pods ephemeral CSI volume + persistent volume, ephemeral disabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralVolumePod, csiEBSTwoVolPod},
			maxVols:          3,
			limitSource:      "csinode",
			test:             "persistent doesn't when node volume limit <= pods ephemeral CSI volume + persistent volume",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralVolumePod, csiEBSTwoVolPod},
			maxVols:          5,
			limitSource:      "csinode",
			test:             "persistent okay when node volume limit > pods ephemeral CSI volume + persistent volume",
		},
		{
			newPod:              onlyConfigmapAndSecretVolPod,
			filterName:          "csi",
			maxVols:             2,
			driverNames:         []string{ebsCSIDriverName},
			test:                "skip Filter when the pod only uses secrets and configmaps",
			limitSource:         "csinode",
			wantPreFilterStatus: framework.NewStatus(framework.Skip),
		},
		{
			newPod:      pvcPodWithConfigmapAndSecret,
			filterName:  "csi",
			maxVols:     2,
			driverNames: []string{ebsCSIDriverName},
			test:        "don't skip Filter when the pod has pvcs",
			limitSource: "csinode",
		},
		{
			newPod:           ephemeralPodWithConfigmapAndSecret,
			filterName:       "csi",
			ephemeralEnabled: true,
			driverNames:      []string{ebsCSIDriverName},
			limitSource:      "csinode-with-no-limit",
			test:             "don't skip Filter when the pod has ephemeral volumes",
			wantStatus:       framework.NewStatus(framework.UnschedulableAndUnresolvable, `looking up PVC test/abc-xyz: persistentvolumeclaims "abc-xyz" not found`),
		},
		{
			newPod:           inlineMigratablePodWithConfigmapAndSecret,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "don't skip Filter when the pod has inline migratable volumes",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
	}

	// running attachable predicate tests with feature gate and limit present on nodes
	for _, test := range tests {
		t.Run(test.test, func(t *testing.T) {
			node, csiNode := getNodeWithPodAndVolumeLimits(test.limitSource, test.existingPods, test.maxVols, test.driverNames...)
			if csiNode != nil {
				enableMigrationOnNode(csiNode, csilibplugins.AWSEBSInTreePluginName)
			}
			csiTranslator := csitrans.New()
			p := &CSILimits{
				csiNodeLister:        getFakeCSINodeLister(csiNode),
				pvLister:             getFakeCSIPVLister(test.filterName, test.driverNames...),
				pvcLister:            append(getFakeCSIPVCLister(test.filterName, scName, test.driverNames...), test.extraClaims...),
				scLister:             getFakeCSIStorageClassLister(scName, test.driverNames[0]),
				vaLister:             getFakeVolumeAttachmentLister(test.vaCount, test.driverNames...),
				randomVolumeIDPrefix: rand.String(32),
				translator:           csiTranslator,
			}
			_, ctx := ktesting.NewTestContext(t)
			_, gotPreFilterStatus := p.PreFilter(ctx, nil, test.newPod)
			if diff := cmp.Diff(test.wantPreFilterStatus, gotPreFilterStatus, statusCmpOpts...); diff != "" {
				t.Errorf("PreFilter status does not match (-want, +got):\n%s", diff)
			}
			if gotPreFilterStatus.Code() != framework.Skip {
				gotStatus := p.Filter(ctx, nil, test.newPod, node)
				if diff := cmp.Diff(test.wantStatus, gotStatus, statusCmpOpts...); diff != "" {
					t.Errorf("Filter status does not match (-want, +got):\n%s", diff)
				}
			}
		})
	}
}

func TestCSILimitsQHint(t *testing.T) {
	podEbs := st.MakePod().PVC("csi-ebs.csi.aws.com-2")

	tests := []struct {
		newPod                 *v1.Pod
		deletedPod             *v1.Pod
		deletedPodNotScheduled bool
		test                   string
		wantQHint              framework.QueueingHint
	}{
		{
			newPod:     podEbs.Obj(),
			deletedPod: st.MakePod().PVC("placeholder").Obj(),
			test:       "return a Queue when a deleted pod has a PVC",
			wantQHint:  framework.Queue,
		},
		{
			newPod:     podEbs.Obj(),
			deletedPod: st.MakePod().Volume(v1.Volume{VolumeSource: v1.VolumeSource{AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{}}}).Obj(),
			test:       "return a Queue when a deleted pod has a inline migratable volume",
			wantQHint:  framework.Queue,
		},
		{
			newPod:     podEbs.Obj(),
			deletedPod: st.MakePod().Obj(),
			test:       "return a QueueSkip when a deleted pod doesn't have any volume",
			wantQHint:  framework.QueueSkip,
		},
		{
			newPod:                 podEbs.Obj(),
			deletedPod:             st.MakePod().PVC("csi-ebs.csi.aws.com-0").Obj(),
			deletedPodNotScheduled: true,
			test:                   "return a QueueSkip when a deleted pod is not scheduled.",
			wantQHint:              framework.QueueSkip,
		},
	}

	for _, test := range tests {
		t.Run(test.test, func(t *testing.T) {
			node, csiNode := getNodeWithPodAndVolumeLimits("csiNode", []*v1.Pod{}, 1, "")
			if csiNode != nil {
				enableMigrationOnNode(csiNode, csilibplugins.AWSEBSDriverName)
			}
			if !test.deletedPodNotScheduled {
				test.deletedPod.Spec.NodeName = node.Node().Name
			} else {
				test.deletedPod.Spec.NodeName = ""
			}

			p := &CSILimits{
				randomVolumeIDPrefix: rand.String(32),
				translator:           csitrans.New(),
			}
			logger, _ := ktesting.NewTestContext(t)
			qhint, err := p.isSchedulableAfterPodDeleted(logger, test.newPod, test.deletedPod, nil)
			if err != nil {
				t.Errorf("isSchedulableAfterPodDeleted failed: %v", err)
			}
			if qhint != test.wantQHint {
				t.Errorf("QHint does not match: %v, want: %v", qhint, test.wantQHint)
			}
		})
	}
}

func TestCSILimitsAddedPVCQHint(t *testing.T) {
	tests := []struct {
		test      string
		newPod    *v1.Pod
		addedPvc  *v1.PersistentVolumeClaim
		wantQHint framework.QueueingHint
	}{
		{
			test:      "a pod isn't in the same namespace as an added PVC",
			newPod:    st.MakePod().Namespace("ns1").Obj(),
			addedPvc:  st.MakePersistentVolumeClaim().Namespace("ns2").Obj(),
			wantQHint: framework.QueueSkip,
		},
		{
			test: "a pod is in the same namespace as an added PVC",
			newPod: st.MakePod().Namespace("ns1").Volume(
				v1.Volume{
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc1",
						},
					},
				}).Obj(),
			addedPvc:  st.MakePersistentVolumeClaim().Name("pvc1").Namespace("ns1").Obj(),
			wantQHint: framework.Queue,
		},
		{
			test: "a pod has an ephemeral volume related to an added PVC",
			newPod: st.MakePod().Name("pod1").Namespace("ns1").Volume(
				v1.Volume{
					Name: "ephemeral",
					VolumeSource: v1.VolumeSource{
						Ephemeral: &v1.EphemeralVolumeSource{},
					},
				},
			).Obj(),
			addedPvc:  st.MakePersistentVolumeClaim().Name("pod1-ephemeral").Namespace("ns1").Obj(),
			wantQHint: framework.Queue,
		},
		{
			test: "a pod doesn't have the same PVC as an added PVC",
			newPod: st.MakePod().Namespace("ns1").Volume(
				v1.Volume{
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc1",
						},
					},
				},
			).Obj(),
			addedPvc:  st.MakePersistentVolumeClaim().Name("pvc2").Namespace("ns1").Obj(),
			wantQHint: framework.QueueSkip,
		},
		{
			test:      "a pod doesn't have any PVC attached",
			newPod:    st.MakePod().Namespace("ns1").Obj(),
			addedPvc:  st.MakePersistentVolumeClaim().Name("pvc2").Namespace("ns1").Obj(),
			wantQHint: framework.QueueSkip,
		},
	}

	for _, test := range tests {
		t.Run(test.test, func(t *testing.T) {
			p := &CSILimits{}
			logger, _ := ktesting.NewTestContext(t)

			qhint, err := p.isSchedulableAfterPVCAdded(logger, test.newPod, nil, test.addedPvc)
			if err != nil {
				t.Errorf("isSchedulableAfterPVCAdded failed: %v", err)
			}
			if qhint != test.wantQHint {
				t.Errorf("QHint does not match: %v, want: %v", qhint, test.wantQHint)
			}
		})
	}
}

func getFakeVolumeAttachmentLister(count int, driverNames ...string) tf.VolumeAttachmentLister {
	vaLister := tf.VolumeAttachmentLister{}
	for _, driver := range driverNames {
		for j := 0; j < count; j++ {
			pvName := fmt.Sprintf("csi-%s-%d", driver, j)
			va := storagev1.VolumeAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("va-%s-%d", driver, j),
				},
				Spec: storagev1.VolumeAttachmentSpec{
					NodeName: "node-for-max-pd-test-1",
					Attacher: driver,
					Source: storagev1.VolumeAttachmentSource{
						PersistentVolumeName: &pvName,
					},
				},
			}
			vaLister = append(vaLister, va)
		}
	}
	return vaLister
}
func getFakeCSIPVLister(volumeName string, driverNames ...string) tf.PersistentVolumeLister {
	pvLister := tf.PersistentVolumeLister{}
	for _, driver := range driverNames {
		for j := 0; j < 4; j++ {
			volumeHandle := fmt.Sprintf("%s-%s-%d", volumeName, driver, j)
			pv := v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: volumeHandle},
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						CSI: &v1.CSIPersistentVolumeSource{
							Driver:       driver,
							VolumeHandle: volumeHandle,
						},
					},
				},
			}

			switch driver {
			case csilibplugins.AWSEBSInTreePluginName:
				pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
					AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
						VolumeID: volumeHandle,
					},
				}
			case hostpathInTreePluginName:
				pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
					HostPath: &v1.HostPathVolumeSource{
						Path: "/tmp",
					},
				}
			default:
				pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
					CSI: &v1.CSIPersistentVolumeSource{
						Driver:       driver,
						VolumeHandle: volumeHandle,
					},
				}
			}
			pvLister = append(pvLister, pv)
		}
	}

	return pvLister
}

func getFakeCSIPVCLister(volumeName, scName string, driverNames ...string) tf.PersistentVolumeClaimLister {
	pvcLister := tf.PersistentVolumeClaimLister{}
	for _, driver := range driverNames {
		for j := 0; j < 4; j++ {
			v := fmt.Sprintf("%s-%s-%d", volumeName, driver, j)
			pvc := v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: v},
				Spec:       v1.PersistentVolumeClaimSpec{VolumeName: v},
			}
			pvcLister = append(pvcLister, pvc)
		}
	}

	pvcLister = append(pvcLister, v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName + "-4"},
		Spec:       v1.PersistentVolumeClaimSpec{StorageClassName: &scName},
	})
	pvcLister = append(pvcLister, v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName + "-5"},
		Spec:       v1.PersistentVolumeClaimSpec{},
	})
	// a pvc with missing PV but available storageclass.
	pvcLister = append(pvcLister, v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName + "-6"},
		Spec:       v1.PersistentVolumeClaimSpec{StorageClassName: &scName, VolumeName: "missing-in-action"},
	})
	return pvcLister
}

func enableMigrationOnNode(csiNode *storagev1.CSINode, pluginName string) {
	nodeInfoAnnotations := csiNode.GetAnnotations()
	if nodeInfoAnnotations == nil {
		nodeInfoAnnotations = map[string]string{}
	}

	newAnnotationSet := sets.New[string]()
	newAnnotationSet.Insert(pluginName)
	nas := strings.Join(sets.List(newAnnotationSet), ",")
	nodeInfoAnnotations[v1.MigratedPluginsAnnotationKey] = nas

	csiNode.Annotations = nodeInfoAnnotations
}

func getFakeCSIStorageClassLister(scName, provisionerName string) tf.StorageClassLister {
	return tf.StorageClassLister{
		{
			ObjectMeta:  metav1.ObjectMeta{Name: scName},
			Provisioner: provisionerName,
		},
	}
}

func getFakeCSINodeLister(csiNode *storagev1.CSINode) tf.CSINodeLister {
	csiNodeLister := tf.CSINodeLister{}
	if csiNode != nil {
		csiNodeLister = append(csiNodeLister, *csiNode.DeepCopy())
	}
	return csiNodeLister
}

func getNodeWithPodAndVolumeLimits(limitSource string, pods []*v1.Pod, limit int32, driverNames ...string) (*framework.NodeInfo, *storagev1.CSINode) {
	nodeInfo := framework.NewNodeInfo(pods...)
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-for-max-pd-test-1"},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{},
		},
	}
	var csiNode *storagev1.CSINode

	initCSINode := func() {
		csiNode = &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-for-max-pd-test-1"},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{},
			},
		}
	}

	addDriversCSINode := func(addLimits bool) {
		initCSINode()
		for _, driver := range driverNames {
			driver := storagev1.CSINodeDriver{
				Name:   driver,
				NodeID: "node-for-max-pd-test-1",
			}
			if addLimits {
				driver.Allocatable = &storagev1.VolumeNodeResources{
					Count: ptr.To(limit),
				}
			}
			csiNode.Spec.Drivers = append(csiNode.Spec.Drivers, driver)
		}
	}

	switch limitSource {
	case "csinode":
		addDriversCSINode(true)
	case "csinode-with-no-limit":
		addDriversCSINode(false)
	case "no-csi-driver":
		initCSINode()
	default:
		// Do nothing.
	}

	nodeInfo.SetNode(node)
	return nodeInfo, csiNode
}
