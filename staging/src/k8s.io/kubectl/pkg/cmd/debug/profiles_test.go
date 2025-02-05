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

package debug

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

var testNode = &corev1.Node{
	ObjectMeta: metav1.ObjectMeta{
		Name: "node-XXX",
	},
}

func TestLegacyProfile(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "dbgimage",
				},
			},
		}},
	}

	tests := map[string]struct {
		pod           *corev1.Pod
		containerName string
		target        runtime.Object
		expectPod     *corev1.Pod
		expectErr     bool
	}{
		"bad inputs results in error": {
			pod:           nil,
			containerName: "dbg",
			target:        runtime.Object(nil),
			expectErr:     true,
		},
		"debug by ephemeral container": {
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "dbg", Image: "dbgimage"},
					},
				}},
			},
		},
		"debug by pod copy": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN"},
								},
							},
						},
					},
				},
			},
		},
		"debug by node": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN"},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: "/host",
									Name:      "host-root",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "host-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/"},
							},
						},
					},
				},
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			applier := &legacyProfile{KeepFlags{InitContainers: true}}
			err := applier.Apply(test.pod, test.containerName, test.target)
			if (err != nil) != test.expectErr {
				t.Fatalf("expect error: %v, got error: %v", test.expectErr, (err != nil))
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.expectPod, test.pod); diff != "" {
				t.Error("unexpected diff in generated object: (-want +got):\n", diff)
			}
		})
	}
}

func TestGeneralProfile(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "dbgimage",
				},
			},
		}},
	}

	tests := map[string]struct {
		pod           *corev1.Pod
		containerName string
		target        runtime.Object
		expectPod     *corev1.Pod
		expectErr     bool
	}{
		"bad inputs results in error": {
			pod:           nil,
			containerName: "dbg",
			target:        runtime.Object(nil),
			expectErr:     true,
		},
		"debug by ephemeral container": {
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
				}},
			},
		},
		"debug by pod copy": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN", "SYS_PTRACE"},
								},
							},
						},
					},
					ShareProcessNamespace: ptr.To(true),
				},
			},
		},
		"debug by node": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: "/host",
									Name:      "host-root",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "host-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/"},
							},
						},
					},
				},
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			applier := &generalProfile{KeepFlags{InitContainers: true}}
			err := applier.Apply(test.pod, test.containerName, test.target)
			if (err != nil) != test.expectErr {
				t.Fatalf("expect error: %v, got error: %v", test.expectErr, (err != nil))
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.expectPod, test.pod); diff != "" {
				t.Error("unexpected diff in generated object: (-want +got):\n", diff)
			}
		})
	}
}

func TestBaselineProfile(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "dbgimage",
					SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{
							Add: []corev1.Capability{"SYS_PTRACE"},
						},
					},
				},
			},
		}},
	}

	tests := map[string]struct {
		pod           *corev1.Pod
		containerName string
		target        runtime.Object
		expectPod     *corev1.Pod
		expectErr     bool
	}{
		"bad inputs results in error": {
			pod:           nil,
			containerName: "dbg",
			target:        runtime.Object(nil),
			expectErr:     true,
		},
		"debug by ephemeral container": {
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
						},
					},
				}},
			},
		},
		"debug by pod copy": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					ShareProcessNamespace: ptr.To(true),
					InitContainers:        []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
						},
					},
				},
			},
		},
		"debug by node": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
						},
					},
				},
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			applier := &baselineProfile{KeepFlags{InitContainers: true}}
			err := applier.Apply(test.pod, test.containerName, test.target)
			if (err != nil) != test.expectErr {
				t.Fatalf("expect error: %v, got error: %v", test.expectErr, (err != nil))
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.expectPod, test.pod); diff != "" {
				t.Error("unexpected diff in generated object: (-want +got):\n", diff)
			}
		})
	}
}

func TestRestrictedProfile(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "dbgimage",
					SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{
							Add: []corev1.Capability{"SYS_PTRACE"},
						},
					},
				},
			},
		}},
	}

	tests := map[string]struct {
		pod           *corev1.Pod
		containerName string
		target        runtime.Object
		expectPod     *corev1.Pod
		expectErr     bool
	}{
		"bad inputs results in error": {
			pod:           nil,
			containerName: "dbg",
			target:        runtime.Object(nil),
			expectErr:     true,
		},
		"debug by ephemeral container": {
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				}},
			},
		},
		"debug by pod copy": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					ShareProcessNamespace: ptr.To(true),
					InitContainers:        []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				},
			},
		},
		"debug by node": {
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				},
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			applier := &restrictedProfile{KeepFlags{InitContainers: true}}
			err := applier.Apply(test.pod, test.containerName, test.target)
			if (err != nil) != test.expectErr {
				t.Fatalf("expect error: %v, got error: %v", test.expectErr, (err != nil))
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.expectPod, test.pod); diff != "" {
				t.Error("unexpected diff in generated object: (-want +got):\n", diff)
			}
		})
	}
}

func TestNetAdminProfile(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "dbgimage",
				},
			},
		}},
	}

	tests := []struct {
		name          string
		pod           *corev1.Pod
		containerName string
		target        runtime.Object
		expectPod     *corev1.Pod
		expectErr     error
	}{
		{
			name:          "nil target",
			pod:           pod,
			containerName: "dbg",
			target:        nil,
			expectErr:     fmt.Errorf("netadmin profile: objects of type <nil> are not supported"),
		},
		{
			name:          "debug by ephemeral container",
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
								},
							},
						},
					},
				}},
			},
		},
		{
			name: "debug by pod copy",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					ShareProcessNamespace: ptr.To(true),
					InitContainers:        []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "debug by pod copy preserve existing capability",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					ShareProcessNamespace: ptr.To(true),
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE", "NET_ADMIN", "NET_RAW"},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "debug by node",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "debug by node preserve existing capability",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE", "NET_ADMIN", "NET_RAW"},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applier := &netadminProfile{KeepFlags{InitContainers: true}}
			err := applier.Apply(test.pod, test.containerName, test.target)
			if (err == nil) != (test.expectErr == nil) || (err != nil && test.expectErr != nil && err.Error() != test.expectErr.Error()) {
				t.Fatalf("expect error: %v, got error: %v", test.expectErr, err)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.expectPod, test.pod); diff != "" {
				t.Error("unexpected diff in generated object: (-want +got):\n", diff)
			}
		})
	}
}

func TestSysAdminProfile(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "dbgimage",
				},
			},
		}},
	}

	tests := []struct {
		name          string
		pod           *corev1.Pod
		containerName string
		target        runtime.Object
		expectPod     *corev1.Pod
		expectErr     error
	}{
		{
			name:          "nil target",
			pod:           pod,
			containerName: "dbg",
			target:        nil,
			expectErr:     fmt.Errorf("sysadmin profile: objects of type <nil> are not supported"),
		},
		{
			name:          "debug by ephemeral container",
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
						},
					},
				}},
			},
		},
		{
			name: "debug by pod copy",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "podcopy",
					Labels: map[string]string{
						"app": "podcopy",
					},
					Annotations: map[string]string{
						"test": "test",
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-container"}},
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
						},
					},
					ShareProcessNamespace: ptr.To(true),
				},
			},
		},
		{
			name: "debug by pod copy preserve existing capability",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:           "app",
							Image:          "appimage",
							LivenessProbe:  &corev1.Probe{},
							ReadinessProbe: &corev1.Probe{},
							StartupProbe:   &corev1.Probe{},
						},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
					ShareProcessNamespace: ptr.To(true),
				},
			},
		},
		{
			name: "debug by node",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Volumes: []corev1.Volume{
						{
							Name:         "host-root",
							VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
							VolumeMounts: []corev1.VolumeMount{{Name: "host-root", MountPath: "/host"}},
						},
					},
				},
			},
		},
		{
			name: "debug by node preserve existing capability",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Volumes: []corev1.Volume{
						{
							Name:         "host-root",
							VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
							VolumeMounts: []corev1.VolumeMount{{Name: "host-root", MountPath: "/host"}},
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applier := &sysadminProfile{KeepFlags{InitContainers: true}}
			err := applier.Apply(test.pod, test.containerName, test.target)
			if (err == nil) != (test.expectErr == nil) || (err != nil && test.expectErr != nil && err.Error() != test.expectErr.Error()) {
				t.Fatalf("expect error: %v, got error: %v", test.expectErr, err)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.expectPod, test.pod); diff != "" {
				t.Error("unexpected diff in generated object: (-want +got):\n", diff)
			}
		})
	}
}

func TestAutoProfile(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "dbg", Image: "dbgimage",
				},
			},
		}},
	}

	tests := []struct {
		name          string
		pod           *corev1.Pod
		containerName string
		target        runtime.Object
		enforce       string
		expectPod     *corev1.Pod
		expectErr     error
	}{
		{
			name:          "nil target",
			pod:           pod,
			containerName: "dbg",
			target:        nil,
			expectErr:     fmt.Errorf("auto profile: objects of type <nil> are not supported"),
		},
		{
			name:          "debug by ephemeral container",
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
				}},
			},
		},
		{
			name:          "debug by ephemeral container with baseline policy enforcement",
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			enforce:       "baseline",
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
						},
					},
				}},
			},
		},
		{
			name:          "debug by ephemeral container with restricted policy enforcement",
			pod:           pod,
			containerName: "dbg",
			target:        pod,
			enforce:       "restricted",
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name: "dbg", Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				}},
			},
		},
		{
			name: "debug by pod copy",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_PTRACE"},
								},
							},
						},
					},
					ShareProcessNamespace: ptr.To(true),
				},
			},
		},
		{
			name: "debug by pod copy preserve existing capability",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN"},
								},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
					},
				},
			},
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN", "SYS_PTRACE"},
								},
							},
						},
					},
					ShareProcessNamespace: ptr.To(true),
				},
			},
		},
		{
			name: "debug by pod copy with baseline policy enforcement",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
					},
				},
			},
			enforce: "baseline",
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					ShareProcessNamespace: ptr.To(true),
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
		},
		{
			name: "debug by pod copy with restricted policy enforcement",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
					},
				},
			},
			enforce: "restricted",
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "podcopy"},
				Spec: corev1.PodSpec{
					ShareProcessNamespace: ptr.To(true),
					Containers: []corev1.Container{
						{Name: "app", Image: "appimage"},
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				},
			},
		},
		{
			name: "debug by node",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: "/host",
									Name:      "host-root",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "host-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/"},
							},
						},
					},
				},
			},
		},
		{
			name: "debug by node with baseline policy enforcement",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			enforce:       "baseline",
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "dbg", Image: "dbgimage"},
					},
				},
			},
		},
		{
			name: "debug by node with restricted policy enforcement",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				},
			},
			containerName: "dbg",
			target:        testNode,
			enforce:       "restricted",
			expectPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dbg",
							Image: "dbgimage",
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								AllowPrivilegeEscalation: ptr.To(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: "RuntimeDefault"},
							},
						},
					},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applier := &autoProfile{KeepFlags{InitContainers: true}, test.enforce}
			err := applier.Apply(test.pod, test.containerName, test.target)
			if (err == nil) != (test.expectErr == nil) || (err != nil && test.expectErr != nil && err.Error() != test.expectErr.Error()) {
				t.Fatalf("expect error: %v, got error: %v", test.expectErr, err)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.expectPod, test.pod); diff != "" {
				t.Error("unexpected diff in generated object: (-want +got):\n", diff)
			}
		})
	}
}
