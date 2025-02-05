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

package pod

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/component-base/featuregate"
	"k8s.io/utils/ptr"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/pointer"
)

func TestVisitContainers(t *testing.T) {
	setAllFeatureEnabledContainersDuringTest := ContainerType(0)
	testCases := []struct {
		desc           string
		spec           *api.PodSpec
		wantContainers []string
		mask           ContainerType
	}{
		{
			desc:           "empty podspec",
			spec:           &api.PodSpec{},
			wantContainers: []string{},
			mask:           AllContainers,
		},
		{
			desc: "regular containers",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"c1", "c2"},
			mask:           Containers,
		},
		{
			desc: "init containers",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"i1", "i2"},
			mask:           InitContainers,
		},
		{
			desc: "ephemeral containers",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"e1", "e2"},
			mask:           EphemeralContainers,
		},
		{
			desc: "all container types",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"i1", "i2", "c1", "c2", "e1", "e2"},
			mask:           AllContainers,
		},
		{
			desc: "all feature enabled container types with ephemeral containers enabled",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2", SecurityContext: &api.SecurityContext{}},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2", SecurityContext: &api.SecurityContext{}},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"i1", "i2", "c1", "c2", "e1", "e2"},
			mask:           setAllFeatureEnabledContainersDuringTest,
		},
		{
			desc: "dropping fields",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2", SecurityContext: &api.SecurityContext{}},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2", SecurityContext: &api.SecurityContext{}},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2", SecurityContext: &api.SecurityContext{}}},
				},
			},
			wantContainers: []string{"i1", "i2", "c1", "c2", "e1", "e2"},
			mask:           AllContainers,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.mask == setAllFeatureEnabledContainersDuringTest {
				tc.mask = AllFeatureEnabledContainers()
			}

			gotContainers := []string{}
			VisitContainers(tc.spec, tc.mask, func(c *api.Container, containerType ContainerType) bool {
				gotContainers = append(gotContainers, c.Name)
				if c.SecurityContext != nil {
					c.SecurityContext = nil
				}
				return true
			})
			if !cmp.Equal(gotContainers, tc.wantContainers) {
				t.Errorf("VisitContainers() = %+v, want %+v", gotContainers, tc.wantContainers)
			}
			for _, c := range tc.spec.Containers {
				if c.SecurityContext != nil {
					t.Errorf("VisitContainers() did not drop SecurityContext for container %q", c.Name)
				}
			}
			for _, c := range tc.spec.InitContainers {
				if c.SecurityContext != nil {
					t.Errorf("VisitContainers() did not drop SecurityContext for init container %q", c.Name)
				}
			}
			for _, c := range tc.spec.EphemeralContainers {
				if c.SecurityContext != nil {
					t.Errorf("VisitContainers() did not drop SecurityContext for ephemeral container %q", c.Name)
				}
			}
		})
	}
}

func TestPodSecrets(t *testing.T) {
	// Stub containing all possible secret references in a pod.
	// The names of the referenced secrets match struct paths detected by reflection.
	pod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					SecretRef: &api.SecretEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.Containers[*].EnvFrom[*].SecretRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						SecretKeyRef: &api.SecretKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.Containers[*].Env[*].ValueFrom.SecretKeyRef"}}}}}}},
			ImagePullSecrets: []api.LocalObjectReference{{
				Name: "Spec.ImagePullSecrets"}},
			InitContainers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					SecretRef: &api.SecretEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.InitContainers[*].EnvFrom[*].SecretRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						SecretKeyRef: &api.SecretKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.InitContainers[*].Env[*].ValueFrom.SecretKeyRef"}}}}}}},
			Volumes: []api.Volume{{
				VolumeSource: api.VolumeSource{
					AzureFile: &api.AzureFileVolumeSource{
						SecretName: "Spec.Volumes[*].VolumeSource.AzureFile.SecretName"}}}, {
				VolumeSource: api.VolumeSource{
					CephFS: &api.CephFSVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.CephFS.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					Cinder: &api.CinderVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.Cinder.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					FlexVolume: &api.FlexVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.FlexVolume.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					Projected: &api.ProjectedVolumeSource{
						Sources: []api.VolumeProjection{{
							Secret: &api.SecretProjection{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.Volumes[*].VolumeSource.Projected.Sources[*].Secret"}}}}}}}, {
				VolumeSource: api.VolumeSource{
					RBD: &api.RBDVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.RBD.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					Secret: &api.SecretVolumeSource{
						SecretName: "Spec.Volumes[*].VolumeSource.Secret.SecretName"}}}, {
				VolumeSource: api.VolumeSource{
					Secret: &api.SecretVolumeSource{
						SecretName: "Spec.Volumes[*].VolumeSource.Secret"}}}, {
				VolumeSource: api.VolumeSource{
					ScaleIO: &api.ScaleIOVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.ScaleIO.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					ISCSI: &api.ISCSIVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.ISCSI.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					StorageOS: &api.StorageOSVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.StorageOS.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					CSI: &api.CSIVolumeSource{
						NodePublishSecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.CSI.NodePublishSecretRef"}}}}},
			EphemeralContainers: []api.EphemeralContainer{{
				EphemeralContainerCommon: api.EphemeralContainerCommon{
					EnvFrom: []api.EnvFromSource{{
						SecretRef: &api.SecretEnvSource{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].SecretRef"}}}},
					Env: []api.EnvVar{{
						ValueFrom: &api.EnvVarSource{
							SecretKeyRef: &api.SecretKeySelector{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.SecretKeyRef"}}}}}}}},
		},
	}
	extractedNames := sets.New[string]()
	VisitPodSecretNames(pod, func(name string) bool {
		extractedNames.Insert(name)
		return true
	}, AllContainers)

	// excludedSecretPaths holds struct paths to fields with "secret" in the name that are not actually references to secret API objects
	excludedSecretPaths := sets.New[string](
		"Spec.Volumes[*].VolumeSource.CephFS.SecretFile",
	)
	// expectedSecretPaths holds struct paths to fields with "secret" in the name that are references to secret API objects.
	// every path here should be represented as an example in the Pod stub above, with the secret name set to the path.
	expectedSecretPaths := sets.New[string](
		"Spec.Containers[*].EnvFrom[*].SecretRef",
		"Spec.Containers[*].Env[*].ValueFrom.SecretKeyRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].SecretRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.SecretKeyRef",
		"Spec.ImagePullSecrets",
		"Spec.InitContainers[*].EnvFrom[*].SecretRef",
		"Spec.InitContainers[*].Env[*].ValueFrom.SecretKeyRef",
		"Spec.Volumes[*].VolumeSource.AzureFile.SecretName",
		"Spec.Volumes[*].VolumeSource.CephFS.SecretRef",
		"Spec.Volumes[*].VolumeSource.Cinder.SecretRef",
		"Spec.Volumes[*].VolumeSource.FlexVolume.SecretRef",
		"Spec.Volumes[*].VolumeSource.Projected.Sources[*].Secret",
		"Spec.Volumes[*].VolumeSource.RBD.SecretRef",
		"Spec.Volumes[*].VolumeSource.Secret",
		"Spec.Volumes[*].VolumeSource.Secret.SecretName",
		"Spec.Volumes[*].VolumeSource.ScaleIO.SecretRef",
		"Spec.Volumes[*].VolumeSource.ISCSI.SecretRef",
		"Spec.Volumes[*].VolumeSource.StorageOS.SecretRef",
		"Spec.Volumes[*].VolumeSource.CSI.NodePublishSecretRef",
	)
	secretPaths := collectResourcePaths(t, "secret", nil, "", reflect.TypeOf(&api.Pod{}))
	secretPaths = secretPaths.Difference(excludedSecretPaths)
	if missingPaths := expectedSecretPaths.Difference(secretPaths); len(missingPaths) > 0 {
		t.Logf("Missing expected secret paths:\n%s", strings.Join(sets.List[string](missingPaths), "\n"))
		t.Error("Missing expected secret paths. Verify VisitPodSecretNames() is correctly finding the missing paths, then correct expectedSecretPaths")
	}
	if extraPaths := secretPaths.Difference(expectedSecretPaths); len(extraPaths) > 0 {
		t.Logf("Extra secret paths:\n%s", strings.Join(sets.List[string](extraPaths), "\n"))
		t.Error("Extra fields with 'secret' in the name found. Verify VisitPodSecretNames() is including these fields if appropriate, then correct expectedSecretPaths")
	}

	if missingNames := expectedSecretPaths.Difference(extractedNames); len(missingNames) > 0 {
		t.Logf("Missing expected secret names:\n%s", strings.Join(sets.List[string](missingNames), "\n"))
		t.Error("Missing expected secret names. Verify the pod stub above includes these references, then verify VisitPodSecretNames() is correctly finding the missing names")
	}
	if extraNames := extractedNames.Difference(expectedSecretPaths); len(extraNames) > 0 {
		t.Logf("Extra secret names:\n%s", strings.Join(sets.List[string](extraNames), "\n"))
		t.Error("Extra secret names extracted. Verify VisitPodSecretNames() is correctly extracting secret names")
	}

	// emptyPod is a stub containing empty object names
	emptyPod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					SecretRef: &api.SecretEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: ""}}}}}},
		},
	}
	VisitPodSecretNames(emptyPod, func(name string) bool {
		t.Fatalf("expected no empty names collected, got %q", name)
		return false
	}, AllContainers)
}

// collectResourcePaths traverses the object, computing all the struct paths that lead to fields with resourcename in the name.
func collectResourcePaths(t *testing.T, resourcename string, path *field.Path, name string, tp reflect.Type) sets.Set[string] {
	resourcename = strings.ToLower(resourcename)
	resourcePaths := sets.New[string]()

	if tp.Kind() == reflect.Pointer {
		resourcePaths.Insert(sets.List[string](collectResourcePaths(t, resourcename, path, name, tp.Elem()))...)
		return resourcePaths
	}

	if strings.Contains(strings.ToLower(name), resourcename) {
		resourcePaths.Insert(path.String())
	}

	switch tp.Kind() {
	case reflect.Pointer:
		resourcePaths.Insert(sets.List[string](collectResourcePaths(t, resourcename, path, name, tp.Elem()))...)
	case reflect.Struct:
		// ObjectMeta is generic and therefore should never have a field with a specific resource's name;
		// it contains cycles so it's easiest to just skip it.
		if name == "ObjectMeta" {
			break
		}
		for i := 0; i < tp.NumField(); i++ {
			field := tp.Field(i)
			resourcePaths.Insert(sets.List[string](collectResourcePaths(t, resourcename, path.Child(field.Name), field.Name, field.Type))...)
		}
	case reflect.Interface:
		t.Errorf("cannot find %s fields in interface{} field %s", resourcename, path.String())
	case reflect.Map:
		resourcePaths.Insert(sets.List[string](collectResourcePaths(t, resourcename, path.Key("*"), "", tp.Elem()))...)
	case reflect.Slice:
		resourcePaths.Insert(sets.List[string](collectResourcePaths(t, resourcename, path.Key("*"), "", tp.Elem()))...)
	default:
		// all primitive types
	}

	return resourcePaths
}

func TestPodConfigmaps(t *testing.T) {
	// Stub containing all possible ConfigMap references in a pod.
	// The names of the referenced ConfigMaps match struct paths detected by reflection.
	pod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					ConfigMapRef: &api.ConfigMapEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.Containers[*].EnvFrom[*].ConfigMapRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						ConfigMapKeyRef: &api.ConfigMapKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.Containers[*].Env[*].ValueFrom.ConfigMapKeyRef"}}}}}}},
			EphemeralContainers: []api.EphemeralContainer{{
				EphemeralContainerCommon: api.EphemeralContainerCommon{
					EnvFrom: []api.EnvFromSource{{
						ConfigMapRef: &api.ConfigMapEnvSource{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].ConfigMapRef"}}}},
					Env: []api.EnvVar{{
						ValueFrom: &api.EnvVarSource{
							ConfigMapKeyRef: &api.ConfigMapKeySelector{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.ConfigMapKeyRef"}}}}}}}},
			InitContainers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					ConfigMapRef: &api.ConfigMapEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.InitContainers[*].EnvFrom[*].ConfigMapRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						ConfigMapKeyRef: &api.ConfigMapKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.InitContainers[*].Env[*].ValueFrom.ConfigMapKeyRef"}}}}}}},
			Volumes: []api.Volume{{
				VolumeSource: api.VolumeSource{
					Projected: &api.ProjectedVolumeSource{
						Sources: []api.VolumeProjection{{
							ConfigMap: &api.ConfigMapProjection{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.Volumes[*].VolumeSource.Projected.Sources[*].ConfigMap"}}}}}}}, {
				VolumeSource: api.VolumeSource{
					ConfigMap: &api.ConfigMapVolumeSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.ConfigMap"}}}}},
		},
	}
	extractedNames := sets.New[string]()
	VisitPodConfigmapNames(pod, func(name string) bool {
		extractedNames.Insert(name)
		return true
	}, AllContainers)

	// expectedPaths holds struct paths to fields with "ConfigMap" in the name that are references to ConfigMap API objects.
	// every path here should be represented as an example in the Pod stub above, with the ConfigMap name set to the path.
	expectedPaths := sets.New[string](
		"Spec.Containers[*].EnvFrom[*].ConfigMapRef",
		"Spec.Containers[*].Env[*].ValueFrom.ConfigMapKeyRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].ConfigMapRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.ConfigMapKeyRef",
		"Spec.InitContainers[*].EnvFrom[*].ConfigMapRef",
		"Spec.InitContainers[*].Env[*].ValueFrom.ConfigMapKeyRef",
		"Spec.Volumes[*].VolumeSource.Projected.Sources[*].ConfigMap",
		"Spec.Volumes[*].VolumeSource.ConfigMap",
	)
	collectPaths := collectResourcePaths(t, "ConfigMap", nil, "", reflect.TypeOf(&api.Pod{}))
	if missingPaths := expectedPaths.Difference(collectPaths); len(missingPaths) > 0 {
		t.Logf("Missing expected paths:\n%s", strings.Join(sets.List[string](missingPaths), "\n"))
		t.Error("Missing expected paths. Verify VisitPodConfigmapNames() is correctly finding the missing paths, then correct expectedPaths")
	}
	if extraPaths := collectPaths.Difference(expectedPaths); len(extraPaths) > 0 {
		t.Logf("Extra paths:\n%s", strings.Join(sets.List[string](extraPaths), "\n"))
		t.Error("Extra fields with resource in the name found. Verify VisitPodConfigmapNames() is including these fields if appropriate, then correct expectedPaths")
	}

	if missingNames := expectedPaths.Difference(extractedNames); len(missingNames) > 0 {
		t.Logf("Missing expected names:\n%s", strings.Join(sets.List[string](missingNames), "\n"))
		t.Error("Missing expected names. Verify the pod stub above includes these references, then verify VisitPodConfigmapNames() is correctly finding the missing names")
	}
	if extraNames := extractedNames.Difference(expectedPaths); len(extraNames) > 0 {
		t.Logf("Extra names:\n%s", strings.Join(sets.List[string](extraNames), "\n"))
		t.Error("Extra names extracted. Verify VisitPodConfigmapNames() is correctly extracting resource names")
	}

	// emptyPod is a stub containing empty object names
	emptyPod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					ConfigMapRef: &api.ConfigMapEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: ""}}}}}},
		},
	}
	VisitPodConfigmapNames(emptyPod, func(name string) bool {
		t.Fatalf("expected no empty names collected, got %q", name)
		return false
	}, AllContainers)
}

func TestDropFSGroupFields(t *testing.T) {
	nofsGroupPod := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "container1",
						Image: "testimage",
					},
				},
			},
		}
	}

	var podFSGroup int64 = 100
	changePolicy := api.FSGroupChangeAlways

	fsGroupPod := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "container1",
						Image: "testimage",
					},
				},
				SecurityContext: &api.PodSecurityContext{
					FSGroup:             &podFSGroup,
					FSGroupChangePolicy: &changePolicy,
				},
			},
		}
	}
	podInfos := []struct {
		description                  string
		newPodHasFSGroupChangePolicy bool
		pod                          func() *api.Pod
		expectPolicyInPod            bool
	}{
		{
			description:                  "oldPod.FSGroupChangePolicy=nil, feature=true, newPod.FSGroupChangePolicy=true",
			pod:                          nofsGroupPod,
			newPodHasFSGroupChangePolicy: true,
			expectPolicyInPod:            true,
		},
		{
			description:                  "oldPod=nil, feature=true, newPod.FSGroupChangePolicy=true",
			pod:                          func() *api.Pod { return nil },
			newPodHasFSGroupChangePolicy: true,
			expectPolicyInPod:            true,
		},
		{
			description:                  "oldPod.FSGroupChangePolicy=true, feature=true, newPod.FSGroupChangePolicy=false",
			pod:                          fsGroupPod,
			newPodHasFSGroupChangePolicy: false,
			expectPolicyInPod:            false,
		},
	}
	for _, podInfo := range podInfos {
		t.Run(podInfo.description, func(t *testing.T) {
			oldPod := podInfo.pod()
			newPod := oldPod.DeepCopy()
			if oldPod == nil && podInfo.newPodHasFSGroupChangePolicy {
				newPod = fsGroupPod()
			}

			if oldPod != nil {
				if podInfo.newPodHasFSGroupChangePolicy {
					newPod.Spec.SecurityContext = &api.PodSecurityContext{
						FSGroup:             &podFSGroup,
						FSGroupChangePolicy: &changePolicy,
					}
				} else {
					newPod.Spec.SecurityContext = &api.PodSecurityContext{}
				}
			}
			DropDisabledPodFields(newPod, oldPod)

			if podInfo.expectPolicyInPod {
				secContext := newPod.Spec.SecurityContext
				if secContext == nil || secContext.FSGroupChangePolicy == nil {
					t.Errorf("for %s, expected fsGroupChangepolicy found none", podInfo.description)
				}
			} else {
				secContext := newPod.Spec.SecurityContext
				if secContext != nil && secContext.FSGroupChangePolicy != nil {
					t.Errorf("for %s, unexpected fsGroupChangepolicy set", podInfo.description)
				}
			}
		})
	}

}

func TestDropProcMount(t *testing.T) {
	procMount := api.UnmaskedProcMount
	defaultProcMount := api.DefaultProcMount
	podWithProcMount := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				RestartPolicy:  api.RestartPolicyNever,
				Containers:     []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &procMount}}},
				InitContainers: []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &procMount}}},
			},
		}
	}
	podWithDefaultProcMount := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				RestartPolicy:  api.RestartPolicyNever,
				Containers:     []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &defaultProcMount}}},
				InitContainers: []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &defaultProcMount}}},
			},
		}
	}
	podWithoutProcMount := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				RestartPolicy:  api.RestartPolicyNever,
				Containers:     []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: nil}}},
				InitContainers: []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: nil}}},
			},
		}
	}

	podInfo := []struct {
		description  string
		hasProcMount bool
		pod          func() *api.Pod
	}{
		{
			description:  "has ProcMount",
			hasProcMount: true,
			pod:          podWithProcMount,
		},
		{
			description:  "has default ProcMount",
			hasProcMount: false,
			pod:          podWithDefaultProcMount,
		},
		{
			description:  "does not have ProcMount",
			hasProcMount: false,
			pod:          podWithoutProcMount,
		},
		{
			description:  "is nil",
			hasProcMount: false,
			pod:          func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasProcMount, oldPod := oldPodInfo.hasProcMount, oldPodInfo.pod()
				newPodHasProcMount, newPod := newPodInfo.hasProcMount, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ProcMountType, enabled)

					var oldPodSpec *api.PodSpec
					if oldPod != nil {
						oldPodSpec = &oldPod.Spec
					}
					dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasProcMount:
						// new pod should not be changed if the feature is enabled, or if the old pod had ProcMount
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasProcMount:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have ProcMount
						if procMountInUse(&newPod.Spec) {
							t.Errorf("new pod had ProcMount: %#v", &newPod.Spec)
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}
}

func TestDropAppArmor(t *testing.T) {
	tests := []struct {
		description    string
		hasAnnotations bool
		hasFields      bool
		pod            api.Pod
	}{{
		description:    "with AppArmor Annotations",
		hasAnnotations: true,
		pod: api.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"a": "1", v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "foo": "default"}},
			Spec:       api.PodSpec{},
		},
	}, {
		description:    "with AppArmor Annotations & fields",
		hasAnnotations: true,
		hasFields:      true,
		pod: api.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"a": "1", v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "foo": "default"}},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
			},
		},
	}, {
		description: "with pod AppArmor profile",
		hasFields:   true,
		pod: api.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"a": "1"}},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
			},
		},
	}, {
		description: "with container AppArmor profile",
		hasFields:   true,
		pod: api.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"a": "1"}},
			Spec: api.PodSpec{
				Containers: []api.Container{{
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type: api.AppArmorProfileTypeRuntimeDefault,
						},
					},
				}},
			},
		},
	}, {
		description: "without AppArmor",
		pod: api.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"a": "1"}},
			Spec:       api.PodSpec{},
		},
	}}

	for _, test := range tests {

		t.Run(fmt.Sprintf("%v", test.description), func(t *testing.T) {
			newPod := test.pod.DeepCopy()

			if hasAnnotations := appArmorAnnotationsInUse(newPod.Annotations); hasAnnotations != test.hasAnnotations {
				t.Errorf("appArmorAnnotationsInUse does not match expectation: %t != %t", hasAnnotations, test.hasAnnotations)
			}
			if hasFields := appArmorFieldsInUse(&newPod.Spec); hasFields != test.hasFields {
				t.Errorf("appArmorFieldsInUse does not match expectation: %t != %t", hasFields, test.hasFields)
			}

			DropDisabledPodFields(newPod, newPod)
			require.Equal(t, &test.pod, newPod, "unchanged pod should never be mutated")

			DropDisabledPodFields(newPod, nil)
			assert.Equal(t, &test.pod, newPod, "pod should not be mutated when both feature gates are enabled")

			expectAnnotations := test.hasAnnotations
			assert.Equal(t, expectAnnotations, appArmorAnnotationsInUse(newPod.Annotations), "AppArmor annotations expectation")
			if expectAnnotations == test.hasAnnotations {
				assert.Equal(t, test.pod.Annotations, newPod.Annotations, "annotations should not be mutated")
			}

			expectFields := test.hasFields
			assert.Equal(t, expectFields, appArmorFieldsInUse(&newPod.Spec), "AppArmor fields expectation")
			if expectFields == test.hasFields {
				assert.Equal(t, &test.pod.Spec, &newPod.Spec, "PodSpec should not be mutated")
			}
		})

	}
}

func TestDropDynamicResourceAllocation(t *testing.T) {
	resourceClaimName := "external-claim"

	podWithClaims := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Resources: api.ResourceRequirements{
						Claims: []api.ResourceClaim{{Name: "my-claim"}},
					},
				},
			},
			InitContainers: []api.Container{
				{
					Resources: api.ResourceRequirements{
						Claims: []api.ResourceClaim{{Name: "my-claim"}},
					},
				},
			},
			EphemeralContainers: []api.EphemeralContainer{
				{
					EphemeralContainerCommon: api.EphemeralContainerCommon{
						Resources: api.ResourceRequirements{
							Claims: []api.ResourceClaim{{Name: "my-claim"}},
						},
					},
				},
			},
			ResourceClaims: []api.PodResourceClaim{
				{
					Name:              "my-claim",
					ResourceClaimName: &resourceClaimName,
				},
			},
		},
		Status: api.PodStatus{
			ResourceClaimStatuses: []api.PodResourceClaimStatus{
				{Name: "my-claim", ResourceClaimName: pointer.String("pod-my-claim")},
			},
		},
	}
	podWithoutClaims := &api.Pod{
		Spec: api.PodSpec{
			Containers:          []api.Container{{}},
			InitContainers:      []api.Container{{}},
			EphemeralContainers: []api.EphemeralContainer{{}},
		},
	}

	var noPod *api.Pod

	testcases := []struct {
		description string
		enabled     bool
		oldPod      *api.Pod
		newPod      *api.Pod
		wantPod     *api.Pod
	}{
		{
			description: "old with claims / new with claims / disabled",
			oldPod:      podWithClaims,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},
		{
			description: "old without claims / new with claims / disabled",
			oldPod:      podWithoutClaims,
			newPod:      podWithClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "no old pod/ new with claims / disabled",
			oldPod:      noPod,
			newPod:      podWithClaims,
			wantPod:     podWithoutClaims,
		},

		{
			description: "old with claims / new without claims / disabled",
			oldPod:      podWithClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "old without claims / new without claims / disabled",
			oldPod:      podWithoutClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "no old pod/ new without claims / disabled",
			oldPod:      noPod,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},

		{
			description: "old with claims / new with claims / enabled",
			enabled:     true,
			oldPod:      podWithClaims,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},
		{
			description: "old without claims / new with claims / enabled",
			enabled:     true,
			oldPod:      podWithoutClaims,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},
		{
			description: "no old pod/ new with claims / enabled",
			enabled:     true,
			oldPod:      noPod,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},

		{
			description: "old with claims / new without claims / enabled",
			enabled:     true,
			oldPod:      podWithClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "old without claims / new without claims / enabled",
			enabled:     true,
			oldPod:      podWithoutClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "no old pod/ new without claims / enabled",
			enabled:     true,
			oldPod:      noPod,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DynamicResourceAllocation, tc.enabled)

			oldPod := tc.oldPod.DeepCopy()
			newPod := tc.newPod.DeepCopy()
			wantPod := tc.wantPod
			DropDisabledPodFields(newPod, oldPod)

			// old pod should never be changed
			if diff := cmp.Diff(oldPod, tc.oldPod); diff != "" {
				t.Errorf("old pod changed: %s", diff)
			}

			if diff := cmp.Diff(wantPod, newPod); diff != "" {
				t.Errorf("new pod changed (- want, + got): %s", diff)
			}
		})
	}
}

func TestValidatePodDeletionCostOption(t *testing.T) {
	testCases := []struct {
		name                            string
		oldPodMeta                      *metav1.ObjectMeta
		featureEnabled                  bool
		wantAllowInvalidPodDeletionCost bool
	}{
		{
			name:                            "CreateFeatureEnabled",
			featureEnabled:                  true,
			wantAllowInvalidPodDeletionCost: false,
		},
		{
			name:                            "CreateFeatureDisabled",
			featureEnabled:                  false,
			wantAllowInvalidPodDeletionCost: true,
		},
		{
			name:                            "UpdateFeatureDisabled",
			oldPodMeta:                      &metav1.ObjectMeta{Annotations: map[string]string{api.PodDeletionCost: "100"}},
			featureEnabled:                  false,
			wantAllowInvalidPodDeletionCost: true,
		},
		{
			name:                            "UpdateFeatureEnabledValidOldValue",
			oldPodMeta:                      &metav1.ObjectMeta{Annotations: map[string]string{api.PodDeletionCost: "100"}},
			featureEnabled:                  true,
			wantAllowInvalidPodDeletionCost: false,
		},
		{
			name:                            "UpdateFeatureEnabledValidOldValue",
			oldPodMeta:                      &metav1.ObjectMeta{Annotations: map[string]string{api.PodDeletionCost: "invalid-value"}},
			featureEnabled:                  true,
			wantAllowInvalidPodDeletionCost: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodDeletionCost, tc.featureEnabled)
			// The new pod doesn't impact the outcome.
			gotOptions := GetValidationOptionsFromPodSpecAndMeta(nil, nil, nil, tc.oldPodMeta)
			if tc.wantAllowInvalidPodDeletionCost != gotOptions.AllowInvalidPodDeletionCost {
				t.Errorf("unexpected diff, want: %v, got: %v", tc.wantAllowInvalidPodDeletionCost, gotOptions.AllowInvalidPodDeletionCost)
			}
		})
	}
}

func TestDropDisabledPodStatusFields(t *testing.T) {
	podWithHostIPs := func() *api.PodStatus {
		return &api.PodStatus{
			HostIPs: makeHostIPs("10.0.0.1", "fd00:10::1"),
		}
	}

	podWithoutHostIPs := func() *api.PodStatus {
		return &api.PodStatus{
			HostIPs: nil,
		}
	}

	tests := []struct {
		name          string
		podStatus     *api.PodStatus
		oldPodStatus  *api.PodStatus
		wantPodStatus *api.PodStatus
	}{
		{
			name:         "old=without, new=without",
			oldPodStatus: podWithoutHostIPs(),
			podStatus:    podWithoutHostIPs(),

			wantPodStatus: podWithoutHostIPs(),
		},
		{
			name:         "old=without, new=with",
			oldPodStatus: podWithoutHostIPs(),
			podStatus:    podWithHostIPs(),

			wantPodStatus: podWithHostIPs(),
		},
		{
			name:         "old=with, new=without",
			oldPodStatus: podWithHostIPs(),
			podStatus:    podWithoutHostIPs(),

			wantPodStatus: podWithoutHostIPs(),
		},
		{
			name:         "old=with, new=with",
			oldPodStatus: podWithHostIPs(),
			podStatus:    podWithHostIPs(),

			wantPodStatus: podWithHostIPs(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dropDisabledPodStatusFields(tt.podStatus, tt.oldPodStatus, &api.PodSpec{}, &api.PodSpec{})

			if !reflect.DeepEqual(tt.podStatus, tt.wantPodStatus) {
				t.Errorf("dropDisabledStatusFields() = %v, want %v", tt.podStatus, tt.wantPodStatus)
			}
		})
	}
}

func makeHostIPs(ips ...string) []api.HostIP {
	ret := []api.HostIP{}
	for _, ip := range ips {
		ret = append(ret, api.HostIP{IP: ip})
	}
	return ret
}

func TestDropNodeInclusionPolicyFields(t *testing.T) {
	ignore := api.NodeInclusionPolicyIgnore
	honor := api.NodeInclusionPolicyHonor

	tests := []struct {
		name        string
		enabled     bool
		podSpec     *api.PodSpec
		oldPodSpec  *api.PodSpec
		wantPodSpec *api.PodSpec
	}{
		{
			name:    "feature disabled, both pods don't use the fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only old pod use NodeAffinityPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &honor},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only old pod use NodeTaintsPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &ignore},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only current pod use NodeAffinityPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &honor},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{{
					NodeAffinityPolicy: nil,
				}},
			},
		},
		{
			name:    "feature disabled, only current pod use NodeTaintsPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &ignore},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: nil},
				},
			},
		},
		{
			name:    "feature disabled, both pods use NodeAffinityPolicy fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &honor},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &ignore},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &ignore},
				},
			},
		},
		{
			name:    "feature disabled, both pods use NodeTaintsPolicy fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &ignore},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &honor},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &honor},
				},
			},
		},
		{
			name:    "feature enabled, both pods use the fields",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &ignore,
						NodeTaintsPolicy:   &honor,
					},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &honor,
						NodeTaintsPolicy:   &ignore,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &honor,
						NodeTaintsPolicy:   &ignore,
					},
				},
			},
		},
		{
			name:    "feature enabled, only old pod use NodeAffinityPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &honor,
					},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature enabled, only old pod use NodeTaintsPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeTaintsPolicy: &ignore,
					},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature enabled, only current pod use NodeAffinityPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &ignore,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &ignore,
					},
				},
			},
		},
		{
			name:    "feature enabled, only current pod use NodeTaintsPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeTaintsPolicy: &honor,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeTaintsPolicy: &honor,
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, test.enabled)

			dropDisabledFields(test.podSpec, nil, test.oldPodSpec, nil)
			if diff := cmp.Diff(test.wantPodSpec, test.podSpec); diff != "" {
				t.Errorf("unexpected pod spec (-want, +got):\n%s", diff)
			}
		})
	}
}

func Test_dropDisabledMatchLabelKeysFieldInPodAffinity(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		podSpec     *api.PodSpec
		oldPodSpec  *api.PodSpec
		wantPodSpec *api.PodSpec
	}{
		{
			name:    "[PodAffinity/required] feature disabled, both pods don't use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/required] feature disabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/required] feature disabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{{}},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/required] feature disabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/required] feature enabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/required] feature enabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/required] feature enabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/preferred] feature disabled, both pods don't use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/preferred] feature disabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/preferred] feature disabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{{}},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/preferred] feature disabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/preferred] feature enabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/preferred] feature enabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
		},
		{
			name:    "[PodAffinity/preferred] feature enabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAffinity: &api.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/required] feature disabled, both pods don't use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/required] feature disabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/required] feature disabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{{}},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/required] feature disabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/required] feature enabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/required] feature enabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/required] feature enabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
							{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
						},
					},
				},
			},
		},

		{
			name:    "[PodAntiAffinity/preferred] feature disabled, both pods don't use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/preferred] feature disabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/preferred] feature disabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{{}},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/preferred] feature disabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/preferred] feature enabled, only old pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/preferred] feature enabled, only current pod uses MatchLabelKeys/MismatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
		},
		{
			name:    "[PodAntiAffinity/preferred] feature enabled, both pods use MatchLabelKeys/MismatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			podSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					PodAntiAffinity: &api.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
							{
								PodAffinityTerm: api.PodAffinityTerm{MatchLabelKeys: []string{"foo"}, MismatchLabelKeys: []string{"foo"}},
							},
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodAffinity, test.enabled)

			dropDisabledFields(test.podSpec, nil, test.oldPodSpec, nil)
			if diff := cmp.Diff(test.wantPodSpec, test.podSpec); diff != "" {
				t.Errorf("unexpected pod spec (-want, +got):\n%s", diff)
			}
		})
	}
}

func Test_dropDisabledMatchLabelKeysFieldInTopologySpread(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		podSpec     *api.PodSpec
		oldPodSpec  *api.PodSpec
		wantPodSpec *api.PodSpec
	}{
		{
			name:    "feature disabled, both pods don't use MatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only old pod uses MatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only current pod uses MatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{{}},
			},
		},
		{
			name:    "feature disabled, both pods use MatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
		},
		{
			name:    "feature enabled, only old pod uses MatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature enabled, only current pod uses MatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
		},
		{
			name:    "feature enabled, both pods use MatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodTopologySpread, test.enabled)

			dropDisabledFields(test.podSpec, nil, test.oldPodSpec, nil)
			if diff := cmp.Diff(test.wantPodSpec, test.podSpec); diff != "" {
				t.Errorf("unexpected pod spec (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestDropHostUsers(t *testing.T) {
	falseVar := false
	trueVar := true

	podWithoutHostUsers := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{}},
		}
	}
	podWithHostUsersFalse := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					HostUsers: &falseVar,
				},
			},
		}
	}
	podWithHostUsersTrue := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					HostUsers: &trueVar,
				},
			},
		}
	}

	podInfo := []struct {
		description  string
		hasHostUsers bool
		pod          func() *api.Pod
	}{
		{
			description:  "with hostUsers=true",
			hasHostUsers: true,
			pod:          podWithHostUsersTrue,
		},
		{
			description:  "with hostUsers=false",
			hasHostUsers: true,
			pod:          podWithHostUsersFalse,
		},
		{
			description: "with hostUsers=nil",
			pod:         func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasHostUsers, oldPod := oldPodInfo.hasHostUsers, oldPodInfo.pod()
				newPodHasHostUsers, newPod := newPodInfo.hasHostUsers, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.UserNamespacesSupport, enabled)

					DropDisabledPodFields(newPod, oldPod)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasHostUsers:
						// new pod should not be changed if the feature is enabled, or if the old pod had hostUsers
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasHostUsers:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have hostUsers
						if exp := podWithoutHostUsers(); !reflect.DeepEqual(newPod, exp) {
							t.Errorf("new pod had hostUsers: %v", cmp.Diff(newPod, exp))
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}

}

func TestValidateTopologySpreadConstraintLabelSelectorOption(t *testing.T) {
	testCases := []struct {
		name       string
		oldPodSpec *api.PodSpec
		wantOption bool
	}{
		{
			name:       "Create",
			wantOption: false,
		},
		{
			name: "UpdateInvalidLabelSelector",
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "foo"},
						},
					},
				},
			},
			wantOption: true,
		},
		{
			name: "UpdateValidLabelSelector",
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"foo": "foo"},
						},
					},
				},
			},
			wantOption: false,
		},
		{
			name: "UpdateEmptyLabelSelector",
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						LabelSelector: nil,
					},
				},
			},
			wantOption: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Pod meta doesn't impact the outcome.
			gotOptions := GetValidationOptionsFromPodSpecAndMeta(&api.PodSpec{}, tc.oldPodSpec, nil, nil)
			if tc.wantOption != gotOptions.AllowInvalidTopologySpreadConstraintLabelSelector {
				t.Errorf("Got AllowInvalidLabelValueInSelector=%t, want %t", gotOptions.AllowInvalidTopologySpreadConstraintLabelSelector, tc.wantOption)
			}
		})
	}
}

func TestValidateAllowNonLocalProjectedTokenPathOption(t *testing.T) {
	testCases := []struct {
		name       string
		oldPodSpec *api.PodSpec
		wantOption bool
	}{
		{
			name:       "Create",
			wantOption: false,
		},
		{
			name: "UpdateInvalidProjectedTokenPath",
			oldPodSpec: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ServiceAccountToken: &api.ServiceAccountTokenProjection{
											Path: "../foo",
										},
									},
								},
							},
						},
					},
				},
			},
			wantOption: true,
		},
		{
			name: "UpdateValidProjectedTokenPath",
			oldPodSpec: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ServiceAccountToken: &api.ServiceAccountTokenProjection{
											Path: "foo",
										},
									},
								},
							},
						},
					},
				},
			},
			wantOption: false,
		},
		{
			name: "UpdateEmptyProjectedTokenPath",
			oldPodSpec: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: nil,
							HostPath:  &api.HostPathVolumeSource{Path: "foo"},
						},
					},
				},
			},
			wantOption: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Pod meta doesn't impact the outcome.
			gotOptions := GetValidationOptionsFromPodSpecAndMeta(&api.PodSpec{}, tc.oldPodSpec, nil, nil)
			if tc.wantOption != gotOptions.AllowNonLocalProjectedTokenPath {
				t.Errorf("Got AllowNonLocalProjectedTokenPath=%t, want %t", gotOptions.AllowNonLocalProjectedTokenPath, tc.wantOption)
			}
		})
	}
}

func TestDropInPlacePodVerticalScaling(t *testing.T) {
	podWithInPlaceVerticalScaling := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
							{ResourceName: api.ResourceMemory, RestartPolicy: api.RestartContainer},
						},
					},
				},
			},
			Status: api.PodStatus{
				Resize: api.PodResizeStatusInProgress,
				ContainerStatuses: []api.ContainerStatus{
					{
						Name:               "c1",
						Image:              "image",
						AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
						Resources: &api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
					},
				},
			},
		}
	}
	podWithoutInPlaceVerticalScaling := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			Status: api.PodStatus{
				ContainerStatuses: []api.ContainerStatus{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
		}
	}

	podInfo := []struct {
		description               string
		hasInPlaceVerticalScaling bool
		pod                       func() *api.Pod
	}{
		{
			description:               "has in-place vertical scaling enabled with resources",
			hasInPlaceVerticalScaling: true,
			pod:                       podWithInPlaceVerticalScaling,
		},
		{
			description:               "has in-place vertical scaling disabled",
			hasInPlaceVerticalScaling: false,
			pod:                       podWithoutInPlaceVerticalScaling,
		},
		{
			description:               "is nil",
			hasInPlaceVerticalScaling: false,
			pod:                       func() *api.Pod { return nil },
		},
	}

	for _, ippvsEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("InPlacePodVerticalScaling=%t", ippvsEnabled), func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, ippvsEnabled)

			for _, allocatedStatusEnabled := range []bool{true, false} {
				t.Run(fmt.Sprintf("AllocatedStatus=%t", allocatedStatusEnabled), func(t *testing.T) {
					featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScalingAllocatedStatus, allocatedStatusEnabled)

					for _, oldPodInfo := range podInfo {
						for _, newPodInfo := range podInfo {
							oldPodHasInPlaceVerticalScaling, oldPod := oldPodInfo.hasInPlaceVerticalScaling, oldPodInfo.pod()
							newPodHasInPlaceVerticalScaling, newPod := newPodInfo.hasInPlaceVerticalScaling, newPodInfo.pod()
							if newPod == nil {
								continue
							}

							t.Run(fmt.Sprintf("old pod %v, new pod %v", oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
								var oldPodSpec *api.PodSpec
								var oldPodStatus *api.PodStatus
								if oldPod != nil {
									oldPodSpec = &oldPod.Spec
									oldPodStatus = &oldPod.Status
								}
								dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)
								dropDisabledPodStatusFields(&newPod.Status, oldPodStatus, &newPod.Spec, oldPodSpec)

								// old pod should never be changed
								if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
									t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
								}

								switch {
								case ippvsEnabled || oldPodHasInPlaceVerticalScaling:
									// new pod shouldn't change if feature enabled or if old pod has ResizePolicy set
									expected := newPodInfo.pod()
									if !ippvsEnabled || !allocatedStatusEnabled {
										expected.Status.ContainerStatuses[0].AllocatedResources = nil
									}
									if !reflect.DeepEqual(newPod, expected) {
										t.Errorf("new pod changed: %v", cmp.Diff(newPod, expected))
									}
								case newPodHasInPlaceVerticalScaling:
									// new pod should be changed
									if reflect.DeepEqual(newPod, newPodInfo.pod()) {
										t.Errorf("new pod was not changed")
									}
									// new pod should not have ResizePolicy
									if !reflect.DeepEqual(newPod, podWithoutInPlaceVerticalScaling()) {
										t.Errorf("new pod has ResizePolicy: %v", cmp.Diff(newPod, podWithoutInPlaceVerticalScaling()))
									}
								default:
									// new pod should not need to be changed
									if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
										t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
									}
								}
							})
						}
					}

				})
			}
		})
	}
}

func TestDropPodLevelResources(t *testing.T) {
	containers := []api.Container{
		{
			Name:  "c1",
			Image: "image",
			Resources: api.ResourceRequirements{
				Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
				Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
			},
		},
	}
	podWithPodLevelResources := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Resources: &api.ResourceRequirements{
					Requests: api.ResourceList{
						api.ResourceCPU:    resource.MustParse("100m"),
						api.ResourceMemory: resource.MustParse("50Gi"),
					},
					Limits: api.ResourceList{
						api.ResourceCPU:    resource.MustParse("100m"),
						api.ResourceMemory: resource.MustParse("50Gi"),
					},
				},
				Containers: containers,
			},
		}
	}

	podWithoutPodLevelResources := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: containers,
			},
		}
	}

	podInfo := []struct {
		description          string
		hasPodLevelResources bool
		pod                  func() *api.Pod
	}{
		{
			description:          "has pod-level resources",
			hasPodLevelResources: true,
			pod:                  podWithPodLevelResources,
		},
		{
			description:          "does not have pod-level resources",
			hasPodLevelResources: false,
			pod:                  podWithoutPodLevelResources,
		},
		{
			description:          "is nil",
			hasPodLevelResources: false,
			pod:                  func() *api.Pod { return nil },
		},
		{
			description:          "is empty struct",
			hasPodLevelResources: false,
			// refactor to generalize and use podWithPodLevelResources()
			pod: func() *api.Pod {
				return &api.Pod{
					Spec: api.PodSpec{
						Resources:  &api.ResourceRequirements{},
						Containers: containers,
					},
				}
			},
		},
		{
			description:          "is empty Requests list",
			hasPodLevelResources: false,
			pod: func() *api.Pod {
				return &api.Pod{
					Spec: api.PodSpec{Resources: &api.ResourceRequirements{
						Requests: api.ResourceList{},
					}}}
			},
		},
		{
			description:          "is empty Limits list",
			hasPodLevelResources: false,
			pod: func() *api.Pod {
				return &api.Pod{
					Spec: api.PodSpec{Resources: &api.ResourceRequirements{
						Limits: api.ResourceList{},
					}}}
			},
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasPodLevelResources, oldPod := oldPodInfo.hasPodLevelResources, oldPodInfo.pod()
				newPodHasPodLevelResources, newPod := newPodInfo.hasPodLevelResources, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLevelResources, enabled)

					var oldPodSpec *api.PodSpec
					if oldPod != nil {
						oldPodSpec = &oldPod.Spec
					}

					dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasPodLevelResources:
						// new pod shouldn't change if feature enabled or if old pod has
						// any pod level resources
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasPodLevelResources:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have any pod-level resources
						if !reflect.DeepEqual(newPod, podWithoutPodLevelResources()) {
							t.Errorf("new pod has pod-level resources: %v", cmp.Diff(newPod, podWithoutPodLevelResources()))
						}
					default:
						if newPod.Spec.Resources != nil {
							t.Errorf("expected nil, got: %v", newPod.Spec.Resources)
						}
					}
				})
			}
		}
	}
}

func TestDropSidecarContainers(t *testing.T) {
	containerRestartPolicyAlways := api.ContainerRestartPolicyAlways

	podWithSidecarContainers := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "c1",
						Image:         "image",
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
		}
	}

	podWithoutSidecarContainers := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
		}
	}

	podInfo := []struct {
		description         string
		hasSidecarContainer bool
		pod                 func() *api.Pod
	}{
		{
			description:         "has a sidecar container",
			hasSidecarContainer: true,
			pod:                 podWithSidecarContainers,
		},
		{
			description:         "does not have a sidecar container",
			hasSidecarContainer: false,
			pod:                 podWithoutSidecarContainers,
		},
		{
			description:         "is nil",
			hasSidecarContainer: false,
			pod:                 func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasSidecarContainer, oldPod := oldPodInfo.hasSidecarContainer, oldPodInfo.pod()
				newPodHasSidecarContainer, newPod := newPodInfo.hasSidecarContainer, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					if !enabled {
						// TODO: Remove this in v1.36
						featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.32"))
						featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SidecarContainers, false)
					}

					var oldPodSpec *api.PodSpec
					if oldPod != nil {
						oldPodSpec = &oldPod.Spec
					}
					dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasSidecarContainer:
						// new pod shouldn't change if feature enabled or if old pod has
						// any sidecar container
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasSidecarContainer:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have any sidecar container
						if !reflect.DeepEqual(newPod, podWithoutSidecarContainers()) {
							t.Errorf("new pod has a sidecar container: %v", cmp.Diff(newPod, podWithoutSidecarContainers()))
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}
}

func TestMarkPodProposedForResize(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SidecarContainers, true)
	containerRestartPolicyAlways := api.ContainerRestartPolicyAlways
	testCases := []struct {
		desc                 string
		newPodSpec           api.PodSpec
		oldPodSpec           api.PodSpec
		expectProposedResize bool
	}{
		{
			desc: "nil requests",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "resources unchanged",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "requests resized",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			expectProposedResize: true,
		},
		{
			desc: "limits resized",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("500m")},
						},
					},
				},
			},
			expectProposedResize: true,
		},
		{
			desc: "the number of containers in the pod has increased; no action should be taken.",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "the number of containers in the pod has decreased; no action should be taken.",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "containers reordered",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "resources unchanged with sidecar containers",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "requests resized with sidecar containers",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			expectProposedResize: true,
		},
		{
			desc: "limits resized with sidecar containers",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("500m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			expectProposedResize: true,
		},
		{
			desc: "requests resized should fail with non-sidecar init container",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "limits resized should fail with non-sidecar init containers",
			newPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
					},
				},
			},
			oldPodSpec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("500m")},
						},
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "the number of sidecar containers in the pod has increased; no action should be taken.",
			newPodSpec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
					{
						Name:  "i2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			oldPodSpec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "the number of sidecar containers in the pod has decreased; no action should be taken.",
			newPodSpec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			oldPodSpec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
					{
						Name:  "i2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			expectProposedResize: false,
		},
		{
			desc: "sidecar containers reordered",
			newPodSpec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
					{
						Name:  "i2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			oldPodSpec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "i2",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
					{
						Name:  "i1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
						},
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
			expectProposedResize: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			newPod := &api.Pod{Spec: tc.newPodSpec}
			newPodUnchanged := newPod.DeepCopy()
			oldPod := &api.Pod{Spec: tc.oldPodSpec}
			MarkPodProposedForResize(oldPod, newPod)
			if tc.expectProposedResize {
				assert.Equal(t, api.PodResizeStatusProposed, newPod.Status.Resize)
			} else {
				assert.Equal(t, api.PodResizeStatus(""), newPod.Status.Resize)
			}
			newPod.Status.Resize = newPodUnchanged.Status.Resize // Only field that might have changed.
			assert.Equal(t, newPodUnchanged, newPod, "No fields other than .status.resize should be modified")
		})
	}
}

func TestDropClusterTrustBundleProjectedVolumes(t *testing.T) {
	testCases := []struct {
		description                         string
		clusterTrustBundleProjectionEnabled bool
		oldPod                              *api.PodSpec
		newPod                              *api.PodSpec
		wantPod                             *api.PodSpec
	}{
		{
			description: "feature gate disabled, cannot add CTB volume to pod",
			oldPod: &api.PodSpec{
				Volumes: []api.Volume{},
			},
			newPod: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ClusterTrustBundle: &api.ClusterTrustBundleProjection{
											Name: pointer.String("foo"),
										},
									},
								},
							}},
					},
				},
			},
			wantPod: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{},
								},
							}},
					},
				},
			},
		},
		{
			description: "feature gate disabled, can keep CTB volume on pod",
			oldPod: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ClusterTrustBundle: &api.ClusterTrustBundleProjection{
											Name: pointer.String("foo"),
										},
									},
								},
							}},
					},
				},
			},
			newPod: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ClusterTrustBundle: &api.ClusterTrustBundleProjection{
											Name: pointer.String("foo"),
										},
									},
								},
							}},
					},
				},
			},
			wantPod: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ClusterTrustBundle: &api.ClusterTrustBundleProjection{
											Name: pointer.String("foo"),
										},
									},
								},
							}},
					},
				},
			},
		},
		{
			description:                         "feature gate enabled, can add CTB volume to pod",
			clusterTrustBundleProjectionEnabled: true,
			oldPod: &api.PodSpec{
				Volumes: []api.Volume{},
			},
			newPod: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ClusterTrustBundle: &api.ClusterTrustBundleProjection{
											Name: pointer.String("foo"),
										},
									},
								},
							}},
					},
				},
			},
			wantPod: &api.PodSpec{
				Volumes: []api.Volume{
					{
						Name: "foo",
						VolumeSource: api.VolumeSource{
							Projected: &api.ProjectedVolumeSource{
								Sources: []api.VolumeProjection{
									{
										ClusterTrustBundle: &api.ClusterTrustBundleProjection{
											Name: pointer.String("foo"),
										},
									},
								},
							}},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ClusterTrustBundleProjection, tc.clusterTrustBundleProjectionEnabled)

			dropDisabledClusterTrustBundleProjection(tc.newPod, tc.oldPod)
			if diff := cmp.Diff(tc.newPod, tc.wantPod); diff != "" {
				t.Fatalf("Unexpected modification to new pod; diff (-got +want)\n%s", diff)
			}
		})
	}
}

func TestDropPodLifecycleSleepAction(t *testing.T) {
	makeSleepHandler := func() *api.LifecycleHandler {
		return &api.LifecycleHandler{
			Sleep: &api.SleepAction{Seconds: 1},
		}
	}

	makeExecHandler := func() *api.LifecycleHandler {
		return &api.LifecycleHandler{
			Exec: &api.ExecAction{Command: []string{"foo"}},
		}
	}

	makeHTTPGetHandler := func() *api.LifecycleHandler {
		return &api.LifecycleHandler{
			HTTPGet: &api.HTTPGetAction{Host: "foo"},
		}
	}

	makeContainer := func(preStop, postStart *api.LifecycleHandler) api.Container {
		container := api.Container{Name: "foo"}
		if preStop != nil || postStart != nil {
			container.Lifecycle = &api.Lifecycle{
				PostStart: postStart,
				PreStop:   preStop,
			}
		}
		return container
	}

	makeEphemeralContainer := func(preStop, postStart *api.LifecycleHandler) api.EphemeralContainer {
		container := api.EphemeralContainer{
			EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "foo"},
		}
		if preStop != nil || postStart != nil {
			container.Lifecycle = &api.Lifecycle{
				PostStart: postStart,
				PreStop:   preStop,
			}
		}
		return container
	}

	makePod := func(containers []api.Container, initContainers []api.Container, ephemeralContainers []api.EphemeralContainer) *api.PodSpec {
		return &api.PodSpec{
			Containers:          containers,
			InitContainers:      initContainers,
			EphemeralContainers: ephemeralContainers,
		}
	}

	testCases := []struct {
		gateEnabled            bool
		oldLifecycleHandler    *api.LifecycleHandler
		newLifecycleHandler    *api.LifecycleHandler
		expectLifecycleHandler *api.LifecycleHandler
	}{
		// nil -> nil
		{
			gateEnabled:            false,
			oldLifecycleHandler:    nil,
			newLifecycleHandler:    nil,
			expectLifecycleHandler: nil,
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    nil,
			newLifecycleHandler:    nil,
			expectLifecycleHandler: nil,
		},
		// nil -> exec
		{
			gateEnabled:            false,
			oldLifecycleHandler:    nil,
			newLifecycleHandler:    makeExecHandler(),
			expectLifecycleHandler: makeExecHandler(),
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    nil,
			newLifecycleHandler:    makeExecHandler(),
			expectLifecycleHandler: makeExecHandler(),
		},
		// nil -> sleep
		{
			gateEnabled:            false,
			oldLifecycleHandler:    nil,
			newLifecycleHandler:    makeSleepHandler(),
			expectLifecycleHandler: nil,
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    nil,
			newLifecycleHandler:    makeSleepHandler(),
			expectLifecycleHandler: makeSleepHandler(),
		},
		// exec -> exec
		{
			gateEnabled:            false,
			oldLifecycleHandler:    makeExecHandler(),
			newLifecycleHandler:    makeExecHandler(),
			expectLifecycleHandler: makeExecHandler(),
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    makeExecHandler(),
			newLifecycleHandler:    makeExecHandler(),
			expectLifecycleHandler: makeExecHandler(),
		},
		// exec -> http
		{
			gateEnabled:            false,
			oldLifecycleHandler:    makeExecHandler(),
			newLifecycleHandler:    makeHTTPGetHandler(),
			expectLifecycleHandler: makeHTTPGetHandler(),
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    makeExecHandler(),
			newLifecycleHandler:    makeHTTPGetHandler(),
			expectLifecycleHandler: makeHTTPGetHandler(),
		},
		// exec -> sleep
		{
			gateEnabled:            false,
			oldLifecycleHandler:    makeExecHandler(),
			newLifecycleHandler:    makeSleepHandler(),
			expectLifecycleHandler: nil,
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    makeExecHandler(),
			newLifecycleHandler:    makeSleepHandler(),
			expectLifecycleHandler: makeSleepHandler(),
		},
		// sleep -> exec
		{
			gateEnabled:            false,
			oldLifecycleHandler:    makeSleepHandler(),
			newLifecycleHandler:    makeExecHandler(),
			expectLifecycleHandler: makeExecHandler(),
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    makeSleepHandler(),
			newLifecycleHandler:    makeExecHandler(),
			expectLifecycleHandler: makeExecHandler(),
		},
		// sleep -> sleep
		{
			gateEnabled:            false,
			oldLifecycleHandler:    makeSleepHandler(),
			newLifecycleHandler:    makeSleepHandler(),
			expectLifecycleHandler: makeSleepHandler(),
		},
		{
			gateEnabled:            true,
			oldLifecycleHandler:    makeSleepHandler(),
			newLifecycleHandler:    makeSleepHandler(),
			expectLifecycleHandler: makeSleepHandler(),
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("test_%d", i), func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLifecycleSleepAction, tc.gateEnabled)

			// preStop
			// container
			{
				oldPod := makePod([]api.Container{makeContainer(tc.oldLifecycleHandler.DeepCopy(), nil)}, nil, nil)
				newPod := makePod([]api.Container{makeContainer(tc.newLifecycleHandler.DeepCopy(), nil)}, nil, nil)
				expectPod := makePod([]api.Container{makeContainer(tc.expectLifecycleHandler.DeepCopy(), nil)}, nil, nil)
				dropDisabledFields(newPod, nil, oldPod, nil)
				if diff := cmp.Diff(expectPod, newPod); diff != "" {
					t.Fatalf("Unexpected modification to new pod; diff (-got +want)\n%s", diff)
				}
			}
			// InitContainer
			{
				oldPod := makePod(nil, []api.Container{makeContainer(tc.oldLifecycleHandler.DeepCopy(), nil)}, nil)
				newPod := makePod(nil, []api.Container{makeContainer(tc.newLifecycleHandler.DeepCopy(), nil)}, nil)
				expectPod := makePod(nil, []api.Container{makeContainer(tc.expectLifecycleHandler.DeepCopy(), nil)}, nil)
				dropDisabledFields(newPod, nil, oldPod, nil)
				if diff := cmp.Diff(expectPod, newPod); diff != "" {
					t.Fatalf("Unexpected modification to new pod; diff (-got +want)\n%s", diff)
				}
			}
			// EphemeralContainer
			{
				oldPod := makePod(nil, nil, []api.EphemeralContainer{makeEphemeralContainer(tc.oldLifecycleHandler.DeepCopy(), nil)})
				newPod := makePod(nil, nil, []api.EphemeralContainer{makeEphemeralContainer(tc.newLifecycleHandler.DeepCopy(), nil)})
				expectPod := makePod(nil, nil, []api.EphemeralContainer{makeEphemeralContainer(tc.expectLifecycleHandler.DeepCopy(), nil)})
				dropDisabledFields(newPod, nil, oldPod, nil)
				if diff := cmp.Diff(expectPod, newPod); diff != "" {
					t.Fatalf("Unexpected modification to new pod; diff (-got +want)\n%s", diff)
				}
			}
			// postStart
			// container
			{
				oldPod := makePod([]api.Container{makeContainer(nil, tc.oldLifecycleHandler.DeepCopy())}, nil, nil)
				newPod := makePod([]api.Container{makeContainer(nil, tc.newLifecycleHandler.DeepCopy())}, nil, nil)
				expectPod := makePod([]api.Container{makeContainer(nil, tc.expectLifecycleHandler.DeepCopy())}, nil, nil)
				dropDisabledFields(newPod, nil, oldPod, nil)
				if diff := cmp.Diff(expectPod, newPod); diff != "" {
					t.Fatalf("Unexpected modification to new pod; diff (-got +want)\n%s", diff)
				}
			}
			// InitContainer
			{
				oldPod := makePod(nil, []api.Container{makeContainer(nil, tc.oldLifecycleHandler.DeepCopy())}, nil)
				newPod := makePod(nil, []api.Container{makeContainer(nil, tc.newLifecycleHandler.DeepCopy())}, nil)
				expectPod := makePod(nil, []api.Container{makeContainer(nil, tc.expectLifecycleHandler.DeepCopy())}, nil)
				dropDisabledFields(newPod, nil, oldPod, nil)
				if diff := cmp.Diff(expectPod, newPod); diff != "" {
					t.Fatalf("Unexpected modification to new pod; diff (-got +want)\n%s", diff)
				}
			}
			// EphemeralContainer
			{
				oldPod := makePod(nil, nil, []api.EphemeralContainer{makeEphemeralContainer(nil, tc.oldLifecycleHandler.DeepCopy())})
				newPod := makePod(nil, nil, []api.EphemeralContainer{makeEphemeralContainer(nil, tc.newLifecycleHandler.DeepCopy())})
				expectPod := makePod(nil, nil, []api.EphemeralContainer{makeEphemeralContainer(nil, tc.expectLifecycleHandler.DeepCopy())})
				dropDisabledFields(newPod, nil, oldPod, nil)
				if diff := cmp.Diff(expectPod, newPod); diff != "" {
					t.Fatalf("Unexpected modification to new pod; diff (-got +want)\n%s", diff)
				}
			}
		})
	}
}

func TestDropSupplementalGroupsPolicy(t *testing.T) {
	supplementalGroupsPolicyMerge := api.SupplementalGroupsPolicyMerge
	podWithSupplementalGroupsPolicy := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					SupplementalGroupsPolicy: &supplementalGroupsPolicyMerge,
				},
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
			Status: api.PodStatus{
				ContainerStatuses: []api.ContainerStatus{
					{
						Name:  "c1",
						Image: "image",
						User: &api.ContainerUser{
							Linux: &api.LinuxContainerUser{
								UID:                0,
								GID:                0,
								SupplementalGroups: []int64{0, 1000},
							},
						},
					},
				},
			},
		}
	}
	podWithoutSupplementalGroupsPolicy := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{},
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
			Status: api.PodStatus{
				ContainerStatuses: []api.ContainerStatus{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
		}
	}

	podInfo := []struct {
		description                 string
		hasSupplementalGroupsPolicy bool
		pod                         func() *api.Pod
	}{
		{
			description:                 "with SupplementalGroupsPolicy and User",
			hasSupplementalGroupsPolicy: true,
			pod:                         podWithSupplementalGroupsPolicy,
		},
		{
			description:                 "without SupplementalGroupsPolicy and User",
			hasSupplementalGroupsPolicy: false,
			pod:                         podWithoutSupplementalGroupsPolicy,
		},
		{
			description:                 "is nil",
			hasSupplementalGroupsPolicy: false,
			pod:                         func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasSupplementalGroupsPolicy, oldPod := oldPodInfo.hasSupplementalGroupsPolicy, oldPodInfo.pod()
				newPodHasSupplementalGroupsPolicy, newPod := newPodInfo.hasSupplementalGroupsPolicy, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(
					fmt.Sprintf(
						"feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description,
					),
					func(t *testing.T) {
						featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SupplementalGroupsPolicy, enabled)

						var oldPodSpec *api.PodSpec
						var oldPodStatus *api.PodStatus
						if oldPod != nil {
							oldPodSpec = &oldPod.Spec
							oldPodStatus = &oldPod.Status
						}
						dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)
						dropDisabledPodStatusFields(&newPod.Status, oldPodStatus, &newPod.Spec, oldPodSpec)

						// old pod should never be changed
						if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
							t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
						}
						switch {
						case enabled || oldPodHasSupplementalGroupsPolicy:
							// new pod shouldn't change if feature enabled or if old pod has SupplementalGroupsPolicy and User set
							if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
								t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
							}
						case newPodHasSupplementalGroupsPolicy:
							// new pod should be changed
							if reflect.DeepEqual(newPod, newPodInfo.pod()) {
								t.Errorf("new pod was not changed")
							}
							// new pod should not have SupplementalGroupsPolicy and User fields
							if !reflect.DeepEqual(newPod, podWithoutSupplementalGroupsPolicy()) {
								t.Errorf("new pod has SupplementalGroups and User: %v", cmp.Diff(newPod, podWithoutSupplementalGroupsPolicy()))
							}
						default:
							// new pod should not need to be changed
							if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
								t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
							}
						}
					},
				)
			}
		}
	}
}

func TestDropImageVolumes(t *testing.T) {
	const (
		volumeNameImage = "volume"
		volumeNameOther = "volume-other"
	)
	anotherVolume := api.Volume{Name: volumeNameOther, VolumeSource: api.VolumeSource{HostPath: &api.HostPathVolumeSource{}}}
	podWithVolume := &api.Pod{
		Spec: api.PodSpec{
			Volumes: []api.Volume{
				{Name: volumeNameImage, VolumeSource: api.VolumeSource{Image: &api.ImageVolumeSource{}}},
				anotherVolume,
			},
			Containers: []api.Container{{
				VolumeMounts: []api.VolumeMount{{Name: volumeNameImage}, {Name: volumeNameOther}},
			}},
			InitContainers: []api.Container{{
				VolumeMounts: []api.VolumeMount{{Name: volumeNameImage}},
			}},
			EphemeralContainers: []api.EphemeralContainer{
				{EphemeralContainerCommon: api.EphemeralContainerCommon{
					VolumeMounts: []api.VolumeMount{{Name: volumeNameImage}},
				}},
			},
		},
	}

	podWithoutVolume := &api.Pod{
		Spec: api.PodSpec{
			Volumes:             []api.Volume{anotherVolume},
			Containers:          []api.Container{{VolumeMounts: []api.VolumeMount{{Name: volumeNameOther}}}},
			InitContainers:      []api.Container{{}},
			EphemeralContainers: []api.EphemeralContainer{{}},
		},
	}

	noPod := &api.Pod{}

	testcases := []struct {
		description string
		enabled     bool
		oldPod      *api.Pod
		newPod      *api.Pod
		wantPod     *api.Pod
	}{
		{
			description: "old with volume / new with volume / disabled",
			oldPod:      podWithVolume,
			newPod:      podWithVolume,
			wantPod:     podWithVolume,
		},
		{
			description: "old without volume / new with volume / disabled",
			oldPod:      podWithoutVolume,
			newPod:      podWithVolume,
			wantPod:     podWithoutVolume,
		},
		{
			description: "no old pod/ new with volume / disabled",
			oldPod:      noPod,
			newPod:      podWithVolume,
			wantPod:     podWithoutVolume,
		},
		{
			description: "nil old pod/ new with volume / disabled",
			oldPod:      nil,
			newPod:      podWithVolume,
			wantPod:     podWithoutVolume,
		},
		{
			description: "old with volume / new without volume / disabled",
			oldPod:      podWithVolume,
			newPod:      podWithoutVolume,
			wantPod:     podWithoutVolume,
		},
		{
			description: "old without volume / new without volume / disabled",
			oldPod:      podWithoutVolume,
			newPod:      podWithoutVolume,
			wantPod:     podWithoutVolume,
		},
		{
			description: "no old pod/ new without volume / disabled",
			oldPod:      noPod,
			newPod:      podWithoutVolume,
			wantPod:     podWithoutVolume,
		},

		{
			description: "old with volume / new with volume / enabled",
			enabled:     true,
			oldPod:      podWithVolume,
			newPod:      podWithVolume,
			wantPod:     podWithVolume,
		},
		{
			description: "old without volume / new with volume / enabled",
			enabled:     true,
			oldPod:      podWithoutVolume,
			newPod:      podWithVolume,
			wantPod:     podWithVolume,
		},
		{
			description: "no old pod/ new with volume / enabled",
			enabled:     true,
			oldPod:      noPod,
			newPod:      podWithVolume,
			wantPod:     podWithVolume,
		},

		{
			description: "old with volume / new without volume / enabled",
			enabled:     true,
			oldPod:      podWithVolume,
			newPod:      podWithoutVolume,
			wantPod:     podWithoutVolume,
		},
		{
			description: "old without volume / new without volume / enabled",
			enabled:     true,
			oldPod:      podWithoutVolume,
			newPod:      podWithoutVolume,
			wantPod:     podWithoutVolume,
		},
		{
			description: "no old pod/ new without volume / enabled",
			enabled:     true,
			oldPod:      noPod,
			newPod:      podWithoutVolume,
			wantPod:     podWithoutVolume,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ImageVolume, tc.enabled)

			oldPod := tc.oldPod.DeepCopy()
			newPod := tc.newPod.DeepCopy()
			wantPod := tc.wantPod
			DropDisabledPodFields(newPod, oldPod)

			// old pod should never be changed
			if diff := cmp.Diff(oldPod, tc.oldPod); diff != "" {
				t.Errorf("old pod changed: %s", diff)
			}

			if diff := cmp.Diff(wantPod, newPod); diff != "" {
				t.Errorf("new pod changed (- want, + got): %s", diff)
			}
		})
	}
}

func TestDropSELinuxChangePolicy(t *testing.T) {
	podRecursive := &api.Pod{
		Spec: api.PodSpec{
			SecurityContext: &api.PodSecurityContext{
				SELinuxChangePolicy: ptr.To(api.SELinuxChangePolicyRecursive),
			},
		},
	}
	podMountOption := &api.Pod{
		Spec: api.PodSpec{
			SecurityContext: &api.PodSecurityContext{
				SELinuxChangePolicy: ptr.To(api.SELinuxChangePolicyMountOption),
			},
		},
	}
	podNull := &api.Pod{
		Spec: api.PodSpec{
			SecurityContext: &api.PodSecurityContext{
				SELinuxChangePolicy: nil,
			},
		},
	}

	tests := []struct {
		name    string
		oldPod  *api.Pod
		newPod  *api.Pod
		gates   []featuregate.Feature
		wantPod *api.Pod
	}{
		{
			name:    "no old pod, new pod with Recursive, all features disabled",
			oldPod:  nil,
			newPod:  podRecursive,
			gates:   nil,
			wantPod: podNull,
		},
		{
			name:    "no old pod, new pod with MountOption, all features disabled",
			oldPod:  nil,
			newPod:  podMountOption,
			gates:   nil,
			wantPod: podNull,
		},
		{
			name:    "old pod with Recursive, new pod with Recursive, all features disabled",
			oldPod:  podRecursive,
			newPod:  podRecursive,
			gates:   nil,
			wantPod: podRecursive,
		},
		{
			name:    "old pod with MountOption, new pod with Recursive, all features disabled",
			oldPod:  podMountOption,
			newPod:  podRecursive,
			gates:   nil,
			wantPod: podRecursive,
		},
		{
			name:    "no old pod, new pod with Recursive, SELinuxChangePolicy feature enabled",
			oldPod:  nil,
			newPod:  podRecursive,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy},
			wantPod: podRecursive,
		},
		{
			name:    "no old pod, new pod with MountOption, SELinuxChangePolicy feature enabled",
			oldPod:  nil,
			newPod:  podMountOption,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy},
			wantPod: podMountOption,
		},
		{
			name:    "old pod with Recursive, new pod with Recursive, SELinuxChangePolicy feature enabled",
			oldPod:  podRecursive,
			newPod:  podRecursive,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy},
			wantPod: podRecursive,
		},
		{
			name:    "old pod with MountOption, new pod with Recursive, SELinuxChangePolicy feature enabled",
			oldPod:  podMountOption,
			newPod:  podRecursive,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy},
			wantPod: podRecursive,
		},
		// In theory, SELinuxMount does not have any effect on dropping SELinuxChangePolicy field, but for completeness:
		{
			name:    "no old pod, new pod with Recursive, SELinuxChangePolicy + SELinuxMount features enabled",
			oldPod:  nil,
			newPod:  podRecursive,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy, features.SELinuxMount},
			wantPod: podRecursive,
		},
		{
			name:    "no old pod, new pod with MountOption, SELinuxChangePolicy + SELinuxMount features enabled",
			oldPod:  nil,
			newPod:  podMountOption,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy, features.SELinuxMount},
			wantPod: podMountOption,
		},
		{
			name:    "old pod with Recursive, new pod with Recursive, SELinuxChangePolicy + SELinuxMount features enabled",
			oldPod:  podRecursive,
			newPod:  podRecursive,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy, features.SELinuxMount},
			wantPod: podRecursive,
		},
		{
			name:    "old pod with MountOption, new pod with Recursive, SELinuxChangePolicy + SELinuxMount features enabled",
			oldPod:  podMountOption,
			newPod:  podRecursive,
			gates:   []featuregate.Feature{features.SELinuxChangePolicy, features.SELinuxMount},
			wantPod: podRecursive,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {

			for _, gate := range tc.gates {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, gate, true)
			}

			oldPod := tc.oldPod.DeepCopy()
			newPod := tc.newPod.DeepCopy()
			DropDisabledPodFields(newPod, oldPod)

			// old pod should never be changed
			if diff := cmp.Diff(oldPod, tc.oldPod); diff != "" {
				t.Errorf("old pod changed: %s", diff)
			}

			if diff := cmp.Diff(tc.wantPod, newPod); diff != "" {
				t.Errorf("new pod changed (- want, + got): %s", diff)
			}
		})
	}
}

func TestValidateAllowSidecarResizePolicy(t *testing.T) {
	restartPolicyAlways := api.ContainerRestartPolicyAlways
	testCases := []struct {
		name       string
		oldPodSpec *api.PodSpec
		wantOption bool
	}{
		{
			name:       "old pod spec is nil",
			wantOption: false,
		},
		{
			name: "one sidecar container + one regular init container, no resize policy set on any of them",
			oldPodSpec: &api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "c1-restartable-init",
						Image:         "image",
						RestartPolicy: &restartPolicyAlways,
					},
					{
						Name:  "c1-init",
						Image: "image",
					},
				},
			},
			wantOption: false,
		},
		{
			name: "one sidecar container + one regular init container, resize policy set on regular init container",
			oldPodSpec: &api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "c1-restartable-init",
						Image:         "image",
						RestartPolicy: &restartPolicyAlways,
					},
					{
						Name:  "c1-init",
						Image: "image",
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
				},
			},
			wantOption: false,
		},
		{
			name: "one sidecar container + one regular init container, resize policy set on sidecar container",
			oldPodSpec: &api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "c1-restartable-init",
						Image:         "image",
						RestartPolicy: &restartPolicyAlways,
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
					{
						Name:  "c1-init",
						Image: "image",
					},
				},
			},
			wantOption: true,
		},
		{
			name: "one sidecar container + one regular init container, resize policy set on both of them",
			oldPodSpec: &api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "c1-restartable-init",
						Image:         "image",
						RestartPolicy: &restartPolicyAlways,
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
					{
						Name:  "c1-init",
						Image: "image",
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
				},
			},
			wantOption: true,
		},
		{
			name: "two sidecar containers, resize policy set on one of them",
			oldPodSpec: &api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "c1-restartable-init",
						Image:         "image",
						RestartPolicy: &restartPolicyAlways,
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
					{
						Name:          "c2-restartable-init",
						Image:         "image",
						RestartPolicy: &restartPolicyAlways,
					},
				},
			},
			wantOption: true,
		},
		{
			name: "two regular init containers, resize policy set on both of them",
			oldPodSpec: &api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "c1-init",
						Image: "image",
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
					{
						Name:  "c2-init",
						Image: "image",
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
				},
			},
			wantOption: false,
		},
		{
			name: "two non-init containers, resize policy set on both of them",
			oldPodSpec: &api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
					{
						Name:  "c2",
						Image: "image",
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
						},
					},
				},
			},
			wantOption: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotOptions := GetValidationOptionsFromPodSpecAndMeta(&api.PodSpec{}, tc.oldPodSpec, nil, nil)
			if tc.wantOption != gotOptions.AllowSidecarResizePolicy {
				t.Errorf("Got AllowSidecarResizePolicy=%t, want %t", gotOptions.AllowSidecarResizePolicy, tc.wantOption)
			}
		})
	}
}

func TestValidateInvalidLabelValueInNodeSelectorOption(t *testing.T) {
	testCases := []struct {
		name       string
		oldPodSpec *api.PodSpec
		wantOption bool
	}{
		{
			name:       "Create",
			wantOption: false,
		},
		{
			name: "UpdateInvalidLabelSelector",
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					NodeAffinity: &api.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &api.NodeSelector{
							NodeSelectorTerms: []api.NodeSelectorTerm{{
								MatchExpressions: []api.NodeSelectorRequirement{{
									Key:      "foo",
									Operator: api.NodeSelectorOpIn,
									Values:   []string{"-1"},
								}},
							}},
						},
					},
				},
			},
			wantOption: true,
		},
		{
			name: "UpdateValidLabelSelector",
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					NodeAffinity: &api.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &api.NodeSelector{
							NodeSelectorTerms: []api.NodeSelectorTerm{{
								MatchExpressions: []api.NodeSelectorRequirement{{
									Key:      "foo",
									Operator: api.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
							}},
						},
					},
				},
			},
			wantOption: false,
		},
		{
			name: "UpdateEmptyLabelSelector",
			oldPodSpec: &api.PodSpec{
				Affinity: &api.Affinity{
					NodeAffinity: &api.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &api.NodeSelector{
							NodeSelectorTerms: []api.NodeSelectorTerm{},
						},
					},
				},
			},
			wantOption: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Pod meta doesn't impact the outcome.
			gotOptions := GetValidationOptionsFromPodSpecAndMeta(&api.PodSpec{}, tc.oldPodSpec, nil, nil)
			if tc.wantOption != gotOptions.AllowInvalidLabelValueInRequiredNodeAffinity {
				t.Errorf("Got AllowInvalidLabelValueInRequiredNodeAffinity=%t, want %t", gotOptions.AllowInvalidLabelValueInRequiredNodeAffinity, tc.wantOption)
			}
		})
	}
}
