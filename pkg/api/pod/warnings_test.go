/*
Copyright 2021 The Kubernetes Authors.

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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	api "k8s.io/kubernetes/pkg/apis/core"
	utilpointer "k8s.io/utils/pointer"
)

func BenchmarkNoWarnings(b *testing.B) {
	ctx := context.TODO()
	resources := api.ResourceList{
		api.ResourceCPU:              resource.MustParse("100m"),
		api.ResourceMemory:           resource.MustParse("4M"),
		api.ResourceEphemeralStorage: resource.MustParse("4G"),
	}
	env := []api.EnvVar{
		{Name: "a"},
		{Name: "b"},
	}
	pod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{`foo`: `bar`},
		},
		Spec: api.PodSpec{
			NodeSelector: map[string]string{"foo": "bar", "baz": "quux"},
			Affinity: &api.Affinity{
				NodeAffinity: &api.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &api.NodeSelector{
						NodeSelectorTerms: []api.NodeSelectorTerm{
							{MatchExpressions: []api.NodeSelectorRequirement{{Key: `foo`}}},
						},
					},
					PreferredDuringSchedulingIgnoredDuringExecution: []api.PreferredSchedulingTerm{
						{Preference: api.NodeSelectorTerm{MatchExpressions: []api.NodeSelectorRequirement{{Key: `foo`}}}},
					},
				},
			},
			TopologySpreadConstraints: []api.TopologySpreadConstraint{
				{TopologyKey: `foo`},
			},
			HostAliases: []api.HostAlias{
				{IP: "1.1.1.1"},
				{IP: "2.2.2.2"},
			},
			ImagePullSecrets: []api.LocalObjectReference{
				{Name: "secret1"},
				{Name: "secret2"},
			},
			InitContainers: []api.Container{
				{Name: "init1", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
				{Name: "init2", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
			},
			Containers: []api.Container{
				{Name: "container1", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
				{Name: "container2", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
			},
			Overhead: resources,
			Volumes: []api.Volume{
				{Name: "a"},
				{Name: "b"},
			},
		},
	}
	oldPod := &api.Pod{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := GetWarningsForPod(ctx, pod, oldPod)
		if len(w) > 0 {
			b.Fatalf("expected 0 warnings, got %q", w)
		}
	}
}

func BenchmarkWarnings(b *testing.B) {
	ctx := context.TODO()
	resources := api.ResourceList{
		api.ResourceCPU:              resource.MustParse("100m"),
		api.ResourceMemory:           resource.MustParse("4m"),
		api.ResourceEphemeralStorage: resource.MustParse("4m"),
	}
	env := []api.EnvVar{
		{Name: "a"},
		{Name: "a"},
	}
	pod := &api.Pod{
		Spec: api.PodSpec{
			HostAliases: []api.HostAlias{
				{IP: "1.1.1.1"},
				{IP: "1.1.1.1"},
			},
			ImagePullSecrets: []api.LocalObjectReference{
				{Name: "secret1"},
				{Name: "secret1"},
				{Name: ""},
			},
			InitContainers: []api.Container{
				{Name: "init1", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
				{Name: "init2", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
			},
			Containers: []api.Container{
				{Name: "container1", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
				{Name: "container2", Env: env, Resources: api.ResourceRequirements{Requests: resources, Limits: resources}},
			},
			Overhead: resources,
			Volumes: []api.Volume{
				{Name: "a"},
				{Name: "a"},
			},
		},
	}
	oldPod := &api.Pod{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		GetWarningsForPod(ctx, pod, oldPod)
	}
}

func TestWarnings(t *testing.T) {
	containerRestartPolicyAlways := api.ContainerRestartPolicyAlways
	resources := api.ResourceList{
		api.ResourceCPU:              resource.MustParse("100m"),
		api.ResourceMemory:           resource.MustParse("4m"),
		api.ResourceEphemeralStorage: resource.MustParse("4m"),
	}
	testName := "Test"
	testcases := []struct {
		name        string
		template    *api.PodTemplateSpec
		oldTemplate *api.PodTemplateSpec
		expected    []string
	}{
		{
			name:     "null",
			template: nil,
			expected: nil,
		},
		{
			name: "photon",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "p", VolumeSource: api.VolumeSource{PhotonPersistentDisk: &api.PhotonPersistentDiskVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].photonPersistentDisk: deprecated in v1.11, non-functional in v1.16+`},
		},
		{
			name: "gitRepo",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{GitRepo: &api.GitRepoVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].gitRepo: deprecated in v1.11`},
		},
		{
			name: "scaleIO",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{ScaleIO: &api.ScaleIOVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].scaleIO: deprecated in v1.16, non-functional in v1.22+`},
		},
		{
			name: "flocker",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{Flocker: &api.FlockerVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].flocker: deprecated in v1.22, non-functional in v1.25+`},
		},
		{
			name: "storageOS",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{StorageOS: &api.StorageOSVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].storageOS: deprecated in v1.22, non-functional in v1.25+`},
		},
		{
			name: "quobyte",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{Quobyte: &api.QuobyteVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].quobyte: deprecated in v1.22, non-functional in v1.25+`},
		},
		{
			name: "glusterfs",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{Glusterfs: &api.GlusterfsVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].glusterfs: deprecated in v1.25, non-functional in v1.26+`},
		}, {
			name: "CephFS",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{CephFS: &api.CephFSVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].cephfs: deprecated in v1.28, non-functional in v1.31+`},
		},

		{
			name: "rbd",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{
					{Name: "s", VolumeSource: api.VolumeSource{RBD: &api.RBDVolumeSource{}}},
				}},
			},
			expected: []string{`spec.volumes[0].rbd: deprecated in v1.28, non-functional in v1.31+`},
		},
		{
			name: "overlapping paths in a configmap volume",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "Test",
					VolumeSource: api.VolumeSource{
						ConfigMap: &api.ConfigMapVolumeSource{
							LocalObjectReference: api.LocalObjectReference{Name: "foo"},
							Items: []api.KeyToPath{
								{Key: "foo", Path: "test"},
								{Key: "bar", Path: "test"},
							},
						},
					},
				}},
			}},
			expected: []string{
				`volume "Test" (ConfigMap "foo"): overlapping paths: "test" with "test"`,
			},
		},
		{
			name: "overlapping paths in a configmap volume - try to mount dir path into a file",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "Test",
					VolumeSource: api.VolumeSource{
						ConfigMap: &api.ConfigMapVolumeSource{
							LocalObjectReference: api.LocalObjectReference{Name: "foo"},
							Items: []api.KeyToPath{
								{Key: "foo", Path: "test"},
								{Key: "bar", Path: "test/app"},
							},
						},
					},
				}},
			}},
			expected: []string{
				`volume "Test" (ConfigMap "foo"): overlapping paths: "test/app" with "test"`,
			},
		},
		{
			name: "overlapping paths in a configmap volume - try to mount file into a dir path",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "Test",
					VolumeSource: api.VolumeSource{
						ConfigMap: &api.ConfigMapVolumeSource{
							LocalObjectReference: api.LocalObjectReference{Name: "foo"},
							Items: []api.KeyToPath{
								{Key: "bar", Path: "test/app"},
								{Key: "foo", Path: "test"},
							},
						},
					},
				}},
			}},
			expected: []string{
				`volume "Test" (ConfigMap "foo"): overlapping paths: "test" with "test/app"`,
			},
		},
		{
			name: "overlapping paths in a secret volume",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "Test",
					VolumeSource: api.VolumeSource{
						Secret: &api.SecretVolumeSource{
							SecretName: "foo",
							Items: []api.KeyToPath{
								{Key: "foo", Path: "test"},
								{Key: "bar", Path: "test"},
							},
						},
					},
				}},
			}},
			expected: []string{
				`volume "Test" (Secret "foo"): overlapping paths: "test" with "test"`,
			},
		},
		{
			name: "overlapping paths in a downward api volume",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "Test",
					VolumeSource: api.VolumeSource{
						DownwardAPI: &api.DownwardAPIVolumeSource{
							Items: []api.DownwardAPIVolumeFile{
								{FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}, Path: "test"},
								{FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.labels"}, Path: "test"},
							},
						},
					},
				}},
			}},
			expected: []string{
				`volume "Test" (DownwardAPI): overlapping paths: "test" with "test"`,
			},
		},
		{
			name: "overlapping paths in projected volume - service account and config",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								ConfigMap: &api.ConfigMapProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "Test"},
									Items: []api.KeyToPath{
										{Key: "foo", Path: "test"},
									},
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test" (ConfigMap "Test")`,
			},
		},
		{
			name: "overlapping paths in projected volume volume: service account dir and config file",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								ConfigMap: &api.ConfigMapProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "Test"},
									Items: []api.KeyToPath{
										{Key: "foo", Path: "test"},
									},
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test/file",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test/file" (ServiceAccountToken) with "test" (ConfigMap "Test")`,
			},
		},
		{
			name: "overlapping paths in projected volume - service account file and config dir",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								ConfigMap: &api.ConfigMapProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "Test"},
									Items: []api.KeyToPath{
										{Key: "foo", Path: "test/file"},
									},
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test/file" (ConfigMap "Test")`,
			},
		},
		{
			name: "overlapping paths in projected volume - service account and secret",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								Secret: &api.SecretProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "Test"},
									Items: []api.KeyToPath{
										{Key: "foo", Path: "test"},
									},
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test" (Secret "Test")`,
			},
		},
		{
			name: "overlapping paths in projected volume - service account and downward api",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								DownwardAPI: &api.DownwardAPIProjection{
									Items: []api.DownwardAPIVolumeFile{{
										FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
										Path:     "test",
									}},
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test" (DownwardAPI)`,
			},
		},
		{
			name: "overlapping paths in projected volume - service account and cluster trust bundle",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								ClusterTrustBundle: &api.ClusterTrustBundleProjection{
									Name: &testName, Path: "test",
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test" (ClusterTrustBundle "Test")`,
			},
		},
		{
			name: "overlapping paths in projected volume - service account and cluster trust bundle with signer name",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								ClusterTrustBundle: &api.ClusterTrustBundleProjection{
									SignerName: &testName, Path: "test",
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test" (ClusterTrustBundle "Test")`,
			},
		},
		{
			name: "overlapping paths in projected volume - secret and config map",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								Secret: &api.SecretProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "TestSecret"},
									Items: []api.KeyToPath{
										{Key: "mykey", Path: "test"},
									},
								},
							}, {
								ConfigMap: &api.ConfigMapProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "TestConfigMap"},
									Items: []api.KeyToPath{
										{Key: "mykey", Path: "test/test1"},
									},
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test/test1" (ConfigMap "TestConfigMap") with "test" (Secret "TestSecret")`,
			},
		},
		{
			name: "overlapping paths in projected volume - config map and downward api",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								Secret: &api.SecretProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "TestSecret"},
									Items: []api.KeyToPath{
										{Key: "mykey", Path: "test"},
									},
								},
							}, {
								DownwardAPI: &api.DownwardAPIProjection{
									Items: []api.DownwardAPIVolumeFile{{
										FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
										Path:     "test/test2",
									}},
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test/test2" (DownwardAPI) with "test" (Secret "TestSecret")`,
			},
		},
		{
			name: "overlapping paths in projected volume - downward api and cluster thrust bundle api",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								DownwardAPI: &api.DownwardAPIProjection{
									Items: []api.DownwardAPIVolumeFile{{
										FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
										Path:     "test/test2",
									}},
								},
							}, {
								ClusterTrustBundle: &api.ClusterTrustBundleProjection{
									Name: &testName, Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (ClusterTrustBundle "Test") with "test/test2" (DownwardAPI)`,
			},
		},
		{
			name: "overlapping paths in projected volume - multiple sources",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								ClusterTrustBundle: &api.ClusterTrustBundleProjection{
									SignerName: &testName, Path: "test/test"},
							}, {
								DownwardAPI: &api.DownwardAPIProjection{
									Items: []api.DownwardAPIVolumeFile{{
										FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
										Path:     "test",
									}},
								},
							}, {
								Secret: &api.SecretProjection{
									LocalObjectReference: api.LocalObjectReference{Name: "Test"},
									Items: []api.KeyToPath{
										{Key: "foo", Path: "test"},
									},
								},
							}, {
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test",
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (DownwardAPI) with "test/test" (ClusterTrustBundle "Test")`,
				`volume "foo" (Projected): overlapping paths: "test" (Secret "Test") with "test/test" (ClusterTrustBundle "Test")`,
				`volume "foo" (Projected): overlapping paths: "test" (Secret "Test") with "test" (DownwardAPI)`,
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test/test" (ClusterTrustBundle "Test")`,
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test" (DownwardAPI)`,
				`volume "foo" (Projected): overlapping paths: "test" (ServiceAccountToken) with "test" (Secret "Test")`,
			},
		},
		{
			name: "overlapping paths in projected volume - ServiceAccount vs. DownwardAPI",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{{
								ServiceAccountToken: &api.ServiceAccountTokenProjection{
									Path: "test/test2",
								},
							}, {
								DownwardAPI: &api.DownwardAPIProjection{
									Items: []api.DownwardAPIVolumeFile{
										{FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}, Path: "test"},
										{FieldRef: &api.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}, Path: "test"},
									},
								},
							}},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected): overlapping paths: "test" (DownwardAPI) with "test/test2" (ServiceAccountToken)`,
				`volume "foo" (Projected): overlapping paths: "test" (DownwardAPI) with "test/test2" (ServiceAccountToken)`,
				`volume "foo" (Projected): overlapping paths: "test" (DownwardAPI) with "test" (DownwardAPI)`,
			},
		},
		{
			name: "empty sources in projected volume",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Volumes: []api.Volume{{
					Name: "foo",
					VolumeSource: api.VolumeSource{
						Projected: &api.ProjectedVolumeSource{
							Sources: []api.VolumeProjection{
								{}, // one item, no fields set
							},
						},
					},
				}},
			}},
			expected: []string{
				`volume "foo" (Projected) has no sources provided`,
			},
		},
		{
			name: "duplicate hostAlias",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				HostAliases: []api.HostAlias{
					{IP: "1.1.1.1"},
					{IP: "1.1.1.1"},
					{IP: "1.1.1.1"},
				}},
			},
			expected: []string{
				`spec.hostAliases[1].ip: duplicate ip "1.1.1.1"`,
				`spec.hostAliases[2].ip: duplicate ip "1.1.1.1"`,
			},
		},
		{
			name: "duplicate imagePullSecret",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				ImagePullSecrets: []api.LocalObjectReference{
					{Name: "a"},
					{Name: "a"},
					{Name: "a"},
				}},
			},
			expected: []string{
				`spec.imagePullSecrets[1].name: duplicate name "a"`,
				`spec.imagePullSecrets[2].name: duplicate name "a"`,
			},
		},
		{
			name: "empty imagePullSecret",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				ImagePullSecrets: []api.LocalObjectReference{
					{Name: ""},
				}},
			},
			expected: []string{
				`spec.imagePullSecrets[0].name: invalid empty name ""`,
			},
		},
		{
			name: "duplicate env",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				InitContainers: []api.Container{{Env: []api.EnvVar{
					{Name: "a", Value: "a"},
					{Name: "a", Value: "a"},
					{Name: "a", Value: "other"},
					{Name: "a", Value: ""},
					{Name: "a", Value: "$(a)"},
					{Name: "a", ValueFrom: &api.EnvVarSource{}},
					{Name: "a", Value: "$(a) $(a)"}, // no warning
				}}},
				Containers: []api.Container{{Env: []api.EnvVar{
					{Name: "b", Value: "b"},
					{Name: "b", Value: "b"},
					{Name: "b", Value: "other"},
					{Name: "b", Value: ""},
					{Name: "b", Value: "$(b)"},
					{Name: "b", ValueFrom: &api.EnvVarSource{}},
					{Name: "b", Value: "$(b) $(b)"}, // no warning
				}}},
			}},
			expected: []string{
				`spec.initContainers[0].env[1]: hides previous definition of "a", which may be dropped when using apply`,
				`spec.initContainers[0].env[2]: hides previous definition of "a", which may be dropped when using apply`,
				`spec.initContainers[0].env[3]: hides previous definition of "a", which may be dropped when using apply`,
				`spec.initContainers[0].env[4]: hides previous definition of "a", which may be dropped when using apply`,
				`spec.initContainers[0].env[5]: hides previous definition of "a", which may be dropped when using apply`,
				`spec.containers[0].env[1]: hides previous definition of "b", which may be dropped when using apply`,
				`spec.containers[0].env[2]: hides previous definition of "b", which may be dropped when using apply`,
				`spec.containers[0].env[3]: hides previous definition of "b", which may be dropped when using apply`,
				`spec.containers[0].env[4]: hides previous definition of "b", which may be dropped when using apply`,
				`spec.containers[0].env[5]: hides previous definition of "b", which may be dropped when using apply`,
			},
		},
		{
			name: "fractional resources",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				InitContainers: []api.Container{{
					Resources: api.ResourceRequirements{Requests: resources, Limits: resources},
				}},
				Containers: []api.Container{{
					Resources: api.ResourceRequirements{Requests: resources, Limits: resources},
				}},
				Overhead: resources,
			}},
			expected: []string{
				`spec.initContainers[0].resources.requests[ephemeral-storage]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.initContainers[0].resources.requests[memory]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.initContainers[0].resources.limits[ephemeral-storage]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.initContainers[0].resources.limits[memory]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.containers[0].resources.requests[ephemeral-storage]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.containers[0].resources.requests[memory]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.containers[0].resources.limits[ephemeral-storage]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.containers[0].resources.limits[memory]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.overhead[ephemeral-storage]: fractional byte value "4m" is invalid, must be an integer`,
				`spec.overhead[memory]: fractional byte value "4m" is invalid, must be an integer`,
			},
		},
		{
			name: "node labels in nodeSelector",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				NodeSelector: map[string]string{
					`beta.kubernetes.io/arch`:                  `true`,
					`beta.kubernetes.io/os`:                    `true`,
					`failure-domain.beta.kubernetes.io/region`: `true`,
					`failure-domain.beta.kubernetes.io/zone`:   `true`,
					`beta.kubernetes.io/instance-type`:         `true`,
				},
			}},
			expected: []string{
				`spec.nodeSelector[beta.kubernetes.io/arch]: deprecated since v1.14; use "kubernetes.io/arch" instead`,
				`spec.nodeSelector[beta.kubernetes.io/instance-type]: deprecated since v1.17; use "node.kubernetes.io/instance-type" instead`,
				`spec.nodeSelector[beta.kubernetes.io/os]: deprecated since v1.14; use "kubernetes.io/os" instead`,
				`spec.nodeSelector[failure-domain.beta.kubernetes.io/region]: deprecated since v1.17; use "topology.kubernetes.io/region" instead`,
				`spec.nodeSelector[failure-domain.beta.kubernetes.io/zone]: deprecated since v1.17; use "topology.kubernetes.io/zone" instead`,
			},
		},
		{
			name: "node labels in affinity requiredDuringSchedulingIgnoredDuringExecution",
			template: &api.PodTemplateSpec{
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						NodeAffinity: &api.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &api.NodeSelector{
								NodeSelectorTerms: []api.NodeSelectorTerm{
									{
										MatchExpressions: []api.NodeSelectorRequirement{
											{Key: `foo`},
											{Key: `beta.kubernetes.io/arch`},
											{Key: `beta.kubernetes.io/os`},
											{Key: `failure-domain.beta.kubernetes.io/region`},
											{Key: `failure-domain.beta.kubernetes.io/zone`},
											{Key: `beta.kubernetes.io/instance-type`},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []string{
				`spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[1].key: beta.kubernetes.io/arch is deprecated since v1.14; use "kubernetes.io/arch" instead`,
				`spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[2].key: beta.kubernetes.io/os is deprecated since v1.14; use "kubernetes.io/os" instead`,
				`spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[3].key: failure-domain.beta.kubernetes.io/region is deprecated since v1.17; use "topology.kubernetes.io/region" instead`,
				`spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[4].key: failure-domain.beta.kubernetes.io/zone is deprecated since v1.17; use "topology.kubernetes.io/zone" instead`,
				`spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[5].key: beta.kubernetes.io/instance-type is deprecated since v1.17; use "node.kubernetes.io/instance-type" instead`,
			},
		},
		{
			name: "node labels in affinity preferredDuringSchedulingIgnoredDuringExecution",
			template: &api.PodTemplateSpec{
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						NodeAffinity: &api.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []api.PreferredSchedulingTerm{
								{
									Preference: api.NodeSelectorTerm{
										MatchExpressions: []api.NodeSelectorRequirement{
											{Key: `foo`},
											{Key: `beta.kubernetes.io/arch`},
											{Key: `beta.kubernetes.io/os`},
											{Key: `failure-domain.beta.kubernetes.io/region`},
											{Key: `failure-domain.beta.kubernetes.io/zone`},
											{Key: `beta.kubernetes.io/instance-type`},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []string{
				`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[1].key: beta.kubernetes.io/arch is deprecated since v1.14; use "kubernetes.io/arch" instead`,
				`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[2].key: beta.kubernetes.io/os is deprecated since v1.14; use "kubernetes.io/os" instead`,
				`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[3].key: failure-domain.beta.kubernetes.io/region is deprecated since v1.17; use "topology.kubernetes.io/region" instead`,
				`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[4].key: failure-domain.beta.kubernetes.io/zone is deprecated since v1.17; use "topology.kubernetes.io/zone" instead`,
				`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[5].key: beta.kubernetes.io/instance-type is deprecated since v1.17; use "node.kubernetes.io/instance-type" instead`,
			},
		},
		{
			name: "node labels in topologySpreadConstraints",
			template: &api.PodTemplateSpec{
				Spec: api.PodSpec{
					TopologySpreadConstraints: []api.TopologySpreadConstraint{
						{
							TopologyKey:   `foo`,
							LabelSelector: &metav1.LabelSelector{},
						},
						{
							TopologyKey:   `beta.kubernetes.io/arch`,
							LabelSelector: &metav1.LabelSelector{},
						},
						{
							TopologyKey:   `beta.kubernetes.io/os`,
							LabelSelector: &metav1.LabelSelector{},
						},
						{
							TopologyKey:   `failure-domain.beta.kubernetes.io/region`,
							LabelSelector: &metav1.LabelSelector{},
						},
						{
							TopologyKey:   `failure-domain.beta.kubernetes.io/zone`,
							LabelSelector: &metav1.LabelSelector{},
						},
						{
							TopologyKey:   `beta.kubernetes.io/instance-type`,
							LabelSelector: &metav1.LabelSelector{},
						},
					},
				},
			},
			expected: []string{
				`spec.topologySpreadConstraints[1].topologyKey: beta.kubernetes.io/arch is deprecated since v1.14; use "kubernetes.io/arch" instead`,
				`spec.topologySpreadConstraints[2].topologyKey: beta.kubernetes.io/os is deprecated since v1.14; use "kubernetes.io/os" instead`,
				`spec.topologySpreadConstraints[3].topologyKey: failure-domain.beta.kubernetes.io/region is deprecated since v1.17; use "topology.kubernetes.io/region" instead`,
				`spec.topologySpreadConstraints[4].topologyKey: failure-domain.beta.kubernetes.io/zone is deprecated since v1.17; use "topology.kubernetes.io/zone" instead`,
				`spec.topologySpreadConstraints[5].topologyKey: beta.kubernetes.io/instance-type is deprecated since v1.17; use "node.kubernetes.io/instance-type" instead`,
			},
		},
		{
			name: "annotations",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					`foo`: `bar`,
					`scheduler.alpha.kubernetes.io/critical-pod`:         `true`,
					`seccomp.security.alpha.kubernetes.io/pod`:           `default`,
					`container.seccomp.security.alpha.kubernetes.io/foo`: `default`,
					`security.alpha.kubernetes.io/sysctls`:               `a,b,c`,
					`security.alpha.kubernetes.io/unsafe-sysctls`:        `d,e,f`,
				}},
				Spec: api.PodSpec{Containers: []api.Container{{Name: "foo"}}},
			},
			expected: []string{
				`metadata.annotations[scheduler.alpha.kubernetes.io/critical-pod]: non-functional in v1.16+; use the "priorityClassName" field instead`,
				`metadata.annotations[seccomp.security.alpha.kubernetes.io/pod]: non-functional in v1.27+; use the "seccompProfile" field instead`,
				`metadata.annotations[container.seccomp.security.alpha.kubernetes.io/foo]: non-functional in v1.27+; use the "seccompProfile" field instead`,
				`metadata.annotations[security.alpha.kubernetes.io/sysctls]: non-functional in v1.11+; use the "sysctls" field instead`,
				`metadata.annotations[security.alpha.kubernetes.io/unsafe-sysctls]: non-functional in v1.11+; use the "sysctls" field instead`,
			},
		},
		{
			name: "seccomp fields",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					`seccomp.security.alpha.kubernetes.io/pod`:           `default`,
					`container.seccomp.security.alpha.kubernetes.io/foo`: `default`,
				}},
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						SeccompProfile: &api.SeccompProfile{Type: api.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []api.Container{{
						Name: "foo",
						SecurityContext: &api.SecurityContext{
							SeccompProfile: &api.SeccompProfile{Type: api.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
			expected: []string{},
		},
		{
			name: "pod with ephemeral volume source 200Mi",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: api.PodSpec{Volumes: []api.Volume{
					{Name: "ephemeral-volume", VolumeSource: api.VolumeSource{Ephemeral: &api.EphemeralVolumeSource{
						VolumeClaimTemplate: &api.PersistentVolumeClaimTemplate{
							Spec: api.PersistentVolumeClaimSpec{Resources: api.VolumeResourceRequirements{
								Requests: api.ResourceList{api.ResourceStorage: resource.MustParse("200Mi")}}},
						},
					}}}}},
			},
			expected: []string{},
		},
		{
			name: "pod with ephemeral volume source 200m",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: api.PodSpec{Volumes: []api.Volume{
					{Name: "ephemeral-volume", VolumeSource: api.VolumeSource{Ephemeral: &api.EphemeralVolumeSource{
						VolumeClaimTemplate: &api.PersistentVolumeClaimTemplate{
							Spec: api.PersistentVolumeClaimSpec{Resources: api.VolumeResourceRequirements{
								Requests: api.ResourceList{api.ResourceStorage: resource.MustParse("200m")}}},
						},
					}}}}},
			},
			expected: []string{
				`spec.volumes[0].ephemeral.volumeClaimTemplate.spec.resources.requests[storage]: fractional byte value "200m" is invalid, must be an integer`,
			},
		},
		{
			name: "terminationGracePeriodSeconds is negative",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: api.PodSpec{
					TerminationGracePeriodSeconds: utilpointer.Int64Ptr(-1),
				},
			},
			expected: []string{
				`spec.terminationGracePeriodSeconds: must be >= 0; negative values are invalid and will be treated as 1`,
			},
		},
		{
			name: "null LabelSelector in topologySpreadConstraints",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: api.PodSpec{
					TopologySpreadConstraints: []api.TopologySpreadConstraint{
						{
							LabelSelector: &metav1.LabelSelector{},
						},
						{
							LabelSelector: nil,
						},
					},
				},
			},
			expected: []string{
				`spec.topologySpreadConstraints[1].labelSelector: a null labelSelector results in matching no pod`,
			},
		},
		{
			name: "null LabelSelector in PodAffinity",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{},
								},
								{
									LabelSelector: nil,
								},
							},
							PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{},
									},
								},
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: nil,
									},
								},
							},
						},
						PodAntiAffinity: &api.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{},
								},
								{
									LabelSelector: nil,
								},
							},
							PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{},
									},
								},
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: nil,
									},
								},
							},
						},
					},
				},
			},
			expected: []string{
				`spec.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[1].labelSelector: a null labelSelector results in matching no pod`,
				`spec.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[1].podAffinityTerm.labelSelector: a null labelSelector results in matching no pod`,
				`spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[1].labelSelector: a null labelSelector results in matching no pod`,
				`spec.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[1].podAffinityTerm.labelSelector: a null labelSelector results in matching no pod`,
			},
		},
		{
			name: "container no ports",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name:  "foo",
					Ports: []api.ContainerPort{},
				}},
			}},
			expected: []string{},
		},
		{
			name: "one container, one port",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "one container, two ports, same protocol, different ports",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
						{ContainerPort: 81, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "one container, two ports, different protocols, same port",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
						{ContainerPort: 80, Protocol: api.ProtocolTCP},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "one container, two ports, same protocol, same port, different hostport",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 80},
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 81},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "one container, two ports, same protocol, port and hostPort, different hostIP",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 80, HostIP: "10.0.0.1"},
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 80, HostIP: "10.0.0.2"},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "two containers, one port each, same protocol, different ports",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 81, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "two containers, one port each, different protocols, same port",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolTCP},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "two containers, one port each, same protocol, same port, different hostport",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 80},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 81},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "two containers, one port each, same protocol, port and hostPort, different hostIP",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 80, HostIP: "10.0.0.1"},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolTCP, HostPort: 80, HostIP: "10.0.0.2"},
					},
				}},
			}},
			expected: []string{},
		},
		{
			name: "duplicate container ports with same port and protocol",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{
				`spec.containers[0].ports[1]: duplicate port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "duplicate container ports with same port, hostPort and protocol",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{
				`spec.containers[0].ports[1]: duplicate port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "duplicate container ports with same port, host port, host IP and protocol",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}},
			}},
			expected: []string{
				`spec.containers[0].ports[1]: duplicate port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "one container port hostIP set without host port set",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}},
			}},
			expected: []string{
				`spec.containers[0].ports[0]: hostIP set without hostPort: {Name: HostPort:0 ContainerPort:80 Protocol:UDP HostIP:10.0.0.1}`,
			},
		},
		{
			name: "duplicate container ports with one host port set and one without",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
						{ContainerPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}},
			}},
			expected: []string{
				`spec.containers[0].ports[1]: overlapping port definition with spec.containers[0].ports[0]`,
				`spec.containers[0].ports[1]: hostIP set without hostPort: {Name: HostPort:0 ContainerPort:80 Protocol:UDP HostIP:10.0.0.1}`,
			},
		},
		{
			name: "duplicate container ports without one host IP set and two with",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.2"},
					},
				}},
			}},
			expected: []string{
				`spec.containers[0].ports[1]: dangerously ambiguous port definition with spec.containers[0].ports[0]`,
				`spec.containers[0].ports[2]: dangerously ambiguous port definition with spec.containers[0].ports[1]`,
			},
		},
		{
			name: "duplicate container ports with one host IP set and one without",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{
				`spec.containers[0].ports[1]: dangerously ambiguous port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "duplicate containers with same port and protocol",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{
				`spec.containers[1].ports[0]: duplicate port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "duplicate containers with same port, hostPort and protocol",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{
				`spec.containers[1].ports[0]: duplicate port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "duplicate containers with same port, host port, host IP and protocol",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}},
			}},
			expected: []string{
				`spec.containers[1].ports[0]: duplicate port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "duplicate containers with one host port set and one without",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}},
			}},
			expected: []string{
				`spec.containers[1].ports[0]: overlapping port definition with spec.containers[0].ports[0]`,
				`spec.containers[1].ports[0]: hostIP set without hostPort: {Name: HostPort:0 ContainerPort:80 Protocol:UDP HostIP:10.0.0.1}`,
			},
		},
		{
			name: "duplicate container ports without one host IP set and one with",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}},
			}},
			expected: []string{
				`spec.containers[1].ports[0]: dangerously ambiguous port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "duplicate container ports with one host IP set and one without",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "foo",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP, HostIP: "10.0.0.1"},
					},
				}, {
					Name: "bar",
					Ports: []api.ContainerPort{
						{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
					},
				}},
			}},
			expected: []string{
				`spec.containers[1].ports[0]: dangerously ambiguous port definition with spec.containers[0].ports[0]`,
			},
		},
		{
			name: "create duplicate container ports in two containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
							{ContainerPort: 180, HostPort: 80, Protocol: api.ProtocolUDP},
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
						},
					},
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolUDP},
						},
					}},
			}},
			expected: []string{
				`spec.containers[0].ports[2]: duplicate port definition with spec.containers[0].ports[0]`,
				`spec.containers[1].ports[0]: duplicate port definition with spec.containers[0].ports[0]`,
				`spec.containers[1].ports[0]: duplicate port definition with spec.containers[0].ports[2]`,
				`spec.containers[1].ports[1]: duplicate port definition with spec.containers[0].ports[0]`,
				`spec.containers[1].ports[1]: duplicate port definition with spec.containers[0].ports[2]`,
				`spec.containers[1].ports[1]: duplicate port definition with spec.containers[1].ports[0]`,
			},
		},
		{
			name: "update duplicate container ports in two containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolTCP},
							{ContainerPort: 180, HostPort: 80, Protocol: api.ProtocolTCP},
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolTCP},
						},
					},
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolTCP},
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolTCP},
						},
					}},
			}},
			oldTemplate: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, HostPort: 180, Protocol: api.ProtocolTCP},
							{ContainerPort: 180, HostPort: 80, Protocol: api.ProtocolTCP},
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolTCP},
						},
					},
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, HostPort: 180, Protocol: api.ProtocolTCP},
							{ContainerPort: 80, HostPort: 80, Protocol: api.ProtocolTCP},
						},
					}},
			}},
			expected: []string{
				`spec.containers[0].ports[2]: duplicate port definition with spec.containers[0].ports[0]`,
				`spec.containers[1].ports[0]: duplicate port definition with spec.containers[0].ports[0]`,
				`spec.containers[1].ports[0]: duplicate port definition with spec.containers[0].ports[2]`,
				`spec.containers[1].ports[1]: duplicate port definition with spec.containers[0].ports[0]`,
				`spec.containers[1].ports[1]: duplicate port definition with spec.containers[0].ports[2]`,
				`spec.containers[1].ports[1]: duplicate port definition with spec.containers[1].ports[0]`,
			},
		},
		{
			name: "create duplicate container ports name in two containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolTCP, Name: "test"},
						},
					}},
			}},
			expected: []string{
				`spec.containers[1].ports[0]: duplicate port name "test" with spec.containers[0].ports[0], services and probes that select ports by name will use spec.containers[0].ports[0]`,
			},
		},
		{
			name: "update duplicate container ports name in two containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolUDP, Name: "test1"},
							{ContainerPort: 8092, Protocol: api.ProtocolUDP, Name: "test"},
						},
					}},
			}},
			oldTemplate: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name: "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolUDP, Name: "test1"},
						},
					}},
			}},
			expected: []string{
				`spec.containers[1].ports[1]: duplicate port name "test" with spec.containers[0].ports[0], services and probes that select ports by name will use spec.containers[0].ports[0]`,
			},
		},
		{
			name: "create duplicate container ports name in two sidecar containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						RestartPolicy: &containerRestartPolicyAlways,
						Name:          "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
					{
						Name:          "foo",
						RestartPolicy: &containerRestartPolicyAlways,
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolTCP, Name: "test"},
						},
					}},
			}},
			expected: []string{
				`spec.initContainers[1].ports[0]: duplicate port name "test" with spec.initContainers[0].ports[0], services and probes that select ports by name will use spec.initContainers[0].ports[0]`,
			},
		},
		{
			name: "update duplicate container ports name in two sidecar containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "foo1",
						RestartPolicy: &containerRestartPolicyAlways,
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
					{
						Name:          "foo",
						RestartPolicy: &containerRestartPolicyAlways,
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolUDP, Name: "test1"},
							{ContainerPort: 8091, Protocol: api.ProtocolUDP, Name: "test"},
						},
					}},
			}},
			oldTemplate: &api.PodTemplateSpec{Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:          "foo1",
						RestartPolicy: &containerRestartPolicyAlways,
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
					{
						Name:          "foo",
						RestartPolicy: &containerRestartPolicyAlways,
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolUDP, Name: "test1"},
						},
					}},
			}},
			expected: []string{
				`spec.initContainers[1].ports[1]: duplicate port name "test" with spec.initContainers[0].ports[0], services and probes that select ports by name will use spec.initContainers[0].ports[0]`,
			},
		},
		{
			name: "create duplicate container ports name in containers and sidecar containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						RestartPolicy: &containerRestartPolicyAlways,
						Name:          "foo1",
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
				},
				Containers: []api.Container{
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolTCP, Name: "test"},
						},
					}},
			}},
			expected: []string{
				`spec.containers[0].ports[0]: duplicate port name "test" with spec.initContainers[0].ports[0], services and probes that select ports by name will use spec.initContainers[0].ports[0]`,
			},
		},
		{
			name: "update duplicate container ports name in containers and sidecar containers",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "foo1",
						RestartPolicy: &containerRestartPolicyAlways,
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
				},
				Containers: []api.Container{
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolUDP, Name: "test1"},
							{ContainerPort: 8092, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
				},
			}},
			oldTemplate: &api.PodTemplateSpec{Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "foo1",
						RestartPolicy: &containerRestartPolicyAlways,
						Ports: []api.ContainerPort{
							{ContainerPort: 80, Protocol: api.ProtocolUDP, Name: "test"},
						},
					},
				},
				Containers: []api.Container{
					{
						Name: "foo",
						Ports: []api.ContainerPort{
							{ContainerPort: 8090, Protocol: api.ProtocolUDP, Name: "test1"},
						},
					}},
			}},
			expected: []string{
				`spec.containers[0].ports[1]: duplicate port name "test" with spec.initContainers[0].ports[0], services and probes that select ports by name will use spec.initContainers[0].ports[0]`,
			},
		},
		{
			name: "creating pod with invalid value in nodeaffinity",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Affinity: &api.Affinity{NodeAffinity: &api.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []api.PreferredSchedulingTerm{{
						Weight: 10,
						Preference: api.NodeSelectorTerm{
							MatchExpressions: []api.NodeSelectorRequirement{{
								Key:      "foo",
								Operator: api.NodeSelectorOpIn,
								Values:   []string{"-1"},
							}},
						},
					}},
				}},
			}},
			expected: []string{
				`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].values[0]: -1 is invalid, a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')`,
			},
		},
		{
			name: "updating pod with invalid value in nodeaffinity",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				Affinity: &api.Affinity{NodeAffinity: &api.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []api.PreferredSchedulingTerm{{
						Weight: 10,
						Preference: api.NodeSelectorTerm{
							MatchExpressions: []api.NodeSelectorRequirement{{
								Key:      "foo",
								Operator: api.NodeSelectorOpIn,
								Values:   []string{"-1"},
							}},
						},
					}},
				}},
				SchedulingGates: []api.PodSchedulingGate{{Name: "foo"}},
			}},
			oldTemplate: &api.PodTemplateSpec{Spec: api.PodSpec{
				Affinity: &api.Affinity{NodeAffinity: &api.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []api.PreferredSchedulingTerm{{
						Weight: 10,
						Preference: api.NodeSelectorTerm{
							MatchExpressions: []api.NodeSelectorRequirement{{
								Key:      "foo",
								Operator: api.NodeSelectorOpIn,
								Values:   []string{"bar"},
							}},
						},
					}},
				}},
				SchedulingGates: []api.PodSchedulingGate{{Name: "foo"}},
			}},
			expected: []string{
				`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].values[0]: -1 is invalid, a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')`,
			},
		},
		{
			name: "dubious IP address formats",
			template: &api.PodTemplateSpec{Spec: api.PodSpec{
				DNSConfig: &api.PodDNSConfig{
					Nameservers: []string{"1.2.3.4", "05.06.07.08"},
				},
				HostAliases: []api.HostAlias{
					{IP: "::ffff:1.2.3.4"},
				},
			}},
			expected: []string{
				`spec.dnsConfig.nameservers[1]: non-standard IP address "05.06.07.08" will be considered invalid in a future Kubernetes release: use "5.6.7.8"`,
				`spec.hostAliases[0].ip: non-standard IP address "::ffff:1.2.3.4" will be considered invalid in a future Kubernetes release: use "1.2.3.4"`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run("podspec_"+tc.name, func(t *testing.T) {
			var oldTemplate *api.PodTemplateSpec
			if tc.oldTemplate != nil {
				oldTemplate = tc.oldTemplate
			}
			actual := GetWarningsForPodTemplate(context.TODO(), nil, tc.template, oldTemplate)
			if len(actual) != len(tc.expected) {
				t.Errorf("expected %d errors, got %d:\n%v", len(tc.expected), len(actual), strings.Join(actual, "\n"))
			}
			actualSet := sets.New(actual...)
			expectedSet := sets.New(tc.expected...)
			for _, missing := range sets.List(expectedSet.Difference(actualSet)) {
				t.Errorf("missing: %s", missing)
			}
			for _, extra := range sets.List(actualSet.Difference(expectedSet)) {
				t.Errorf("extra:   %s", extra)
			}
		})

		t.Run("pod_"+tc.name, func(t *testing.T) {
			var pod *api.Pod
			if tc.template != nil {
				pod = &api.Pod{
					ObjectMeta: tc.template.ObjectMeta,
					Spec:       tc.template.Spec,
				}
			}
			actual := GetWarningsForPod(context.TODO(), pod, &api.Pod{})
			if len(actual) != len(tc.expected) {
				t.Errorf("expected %d errors, got %d:\n%v", len(tc.expected), len(actual), strings.Join(actual, "\n"))
			}
			actualSet := sets.New(actual...)
			expectedSet := sets.New(tc.expected...)
			for _, missing := range sets.List(expectedSet.Difference(actualSet)) {
				t.Errorf("missing: %s", missing)
			}
			for _, extra := range sets.List(actualSet.Difference(expectedSet)) {
				t.Errorf("extra:   %s", extra)
			}
		})
	}
}

func TestTemplateOnlyWarnings(t *testing.T) {
	testcases := []struct {
		name        string
		template    *api.PodTemplateSpec
		oldTemplate *api.PodTemplateSpec
		expected    []string
	}{
		{
			name: "annotations",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					`container.apparmor.security.beta.kubernetes.io/foo`: `unconfined`,
				}},
				Spec: api.PodSpec{Containers: []api.Container{{Name: "foo"}}},
			},
			expected: []string{
				`template.metadata.annotations[container.apparmor.security.beta.kubernetes.io/foo]: deprecated since v1.30; use the "appArmorProfile" field instead`,
			},
		},
		{
			name: "AppArmor pod field",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					`container.apparmor.security.beta.kubernetes.io/foo`: `unconfined`,
				}},
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						AppArmorProfile: &api.AppArmorProfile{Type: api.AppArmorProfileTypeUnconfined},
					},
					Containers: []api.Container{{
						Name: "foo",
					}},
				},
			},
			expected: []string{},
		},
		{
			name: "AppArmor container field",
			template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					`container.apparmor.security.beta.kubernetes.io/foo`: `unconfined`,
				}},
				Spec: api.PodSpec{
					Containers: []api.Container{{
						Name: "foo",
						SecurityContext: &api.SecurityContext{
							AppArmorProfile: &api.AppArmorProfile{Type: api.AppArmorProfileTypeUnconfined},
						},
					}},
				},
			},
			expected: []string{},
		},
	}

	for _, tc := range testcases {
		t.Run("podspec_"+tc.name, func(t *testing.T) {
			var oldTemplate *api.PodTemplateSpec
			if tc.oldTemplate != nil {
				oldTemplate = tc.oldTemplate
			}
			actual := sets.New[string](GetWarningsForPodTemplate(context.TODO(), field.NewPath("template"), tc.template, oldTemplate)...)
			expected := sets.New[string](tc.expected...)
			for _, missing := range sets.List[string](expected.Difference(actual)) {
				t.Errorf("missing: %s", missing)
			}
			for _, extra := range sets.List[string](actual.Difference(expected)) {
				t.Errorf("extra: %s", extra)
			}
		})

		t.Run("pod_"+tc.name, func(t *testing.T) {
			var pod *api.Pod
			if tc.template != nil {
				pod = &api.Pod{
					ObjectMeta: tc.template.ObjectMeta,
					Spec:       tc.template.Spec,
				}
			}
			actual := GetWarningsForPod(context.TODO(), pod, &api.Pod{})
			if len(actual) > 0 {
				t.Errorf("unexpected template-only warnings on pod: %v", actual)
			}
		})
	}
}

func TestCheckForOverLap(t *testing.T) {
	testCase := map[string]struct {
		checkPaths []pathAndSource
		path       pathAndSource
		found      bool
		expected   []pathAndSource
	}{
		"exact match": {
			checkPaths: []pathAndSource{{"path/path1", "src1"}},
			path:       pathAndSource{"path/path1", "src2"},
			found:      true,
			expected:   []pathAndSource{{"path/path1", "src1"}},
		},
		"no match": {
			checkPaths: []pathAndSource{{"path/path1", "src1"}},
			path:       pathAndSource{"path2/path1", "src2"},
			found:      false,
		},
		"empty checkPaths": {
			checkPaths: []pathAndSource{},
			path:       pathAndSource{"path2/path1", "src2"},
			found:      false,
		},
		"empty string in checkPaths": {
			checkPaths: []pathAndSource{{"", "src1"}},
			path:       pathAndSource{"path2/path1", "src2"},
			found:      false,
		},
		"empty path": {
			checkPaths: []pathAndSource{{"test", "src1"}},
			path:       pathAndSource{"", ""},
			found:      false,
		},
		"empty strings in checkPaths and path": {
			checkPaths: []pathAndSource{{"", "src1"}},
			path:       pathAndSource{"", ""},
			expected:   []pathAndSource{{"", ""}},
			found:      false,
		},
		"between file and dir": {
			checkPaths: []pathAndSource{{"path/path1", "src1"}},
			path:       pathAndSource{"path", "src2"},
			found:      true,
			expected:   []pathAndSource{{"path/path1", "src1"}},
		},
		"between dir and file": {
			checkPaths: []pathAndSource{{"path", "src1"}},
			path:       pathAndSource{"path/path1", "src2"},
			found:      true,
			expected:   []pathAndSource{{"path", "src1"}},
		},
		"multiple paths without overlap": {
			checkPaths: []pathAndSource{{"path1/path", "src1"}, {"path2/path", "src2"}, {"path3/path", "src3"}},
			path:       pathAndSource{"path4/path", "src4"},
			found:      false,
		},
		"multiple paths with 1 overlap": {
			checkPaths: []pathAndSource{{"path1/path", "src1"}, {"path2/path", "src2"}, {"path3/path", "src3"}},
			path:       pathAndSource{"path3/path", "src4"},
			found:      true,
			expected:   []pathAndSource{{"path3/path", "src3"}},
		},
		"multiple paths with multiple overlap": {
			checkPaths: []pathAndSource{{"path/path1", "src1"}, {"path/path2", "src2"}, {"path/path3", "src3"}},
			path:       pathAndSource{"path", "src4"},
			found:      true,
			expected:   []pathAndSource{{"path/path1", "src1"}, {"path/path2", "src2"}, {"path/path3", "src3"}},
		},
		"partial overlap": {
			checkPaths: []pathAndSource{{"path1/path", "src1"}, {"path2/path", "src2"}, {"path3/path", "src3"}},
			path:       pathAndSource{"path101/path3", "src4"},
			found:      false,
		},
		"partial overlap in path": {
			checkPaths: []pathAndSource{{"dir/path1", "src1"}, {"dir/path2", "src2"}, {"dir/path3", "src3"}},
			path:       pathAndSource{"dir/path345", "src4"},
			found:      false,
		},
		"trailing slash in path": {
			checkPaths: []pathAndSource{{"path1/path3", "src1"}},
			path:       pathAndSource{"path1/path3/", "src2"},
			found:      true,
			expected:   []pathAndSource{{"path1/path3", "src1"}},
		},
		"trailing slash in checkPaths": {
			checkPaths: []pathAndSource{{"path1/path3/", "src1"}},
			path:       pathAndSource{"path1/path3", "src2"},
			found:      true,
			expected:   []pathAndSource{{"path1/path3/", "src1"}},
		},
	}

	for name, tc := range testCase {
		t.Run(name, func(t *testing.T) {
			result := checkForOverlap(tc.checkPaths, tc.path)
			found := len(result) > 0
			if found && !tc.found {
				t.Errorf("unexpected match for %q: %q", tc.path, result)
			}
			if !found && tc.found {
				t.Errorf("expected match for %q: %q", tc.path, tc.expected)
			}
			if tc.found && !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}
