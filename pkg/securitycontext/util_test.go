/*
Copyright 2014 The Kubernetes Authors.

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

package securitycontext

import (
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	utilptr "k8s.io/utils/pointer"
)

func TestAddNoNewPrivileges(t *testing.T) {
	pfalse := false
	ptrue := true

	tests := map[string]struct {
		sc     *v1.SecurityContext
		expect bool
	}{
		"allowPrivilegeEscalation nil security context nil": {
			sc:     nil,
			expect: false,
		},
		"allowPrivilegeEscalation nil": {
			sc: &v1.SecurityContext{
				AllowPrivilegeEscalation: nil,
			},
			expect: false,
		},
		"allowPrivilegeEscalation false": {
			sc: &v1.SecurityContext{
				AllowPrivilegeEscalation: &pfalse,
			},
			expect: true,
		},
		"allowPrivilegeEscalation true": {
			sc: &v1.SecurityContext{
				AllowPrivilegeEscalation: &ptrue,
			},
			expect: false,
		},
	}

	for k, v := range tests {
		actual := AddNoNewPrivileges(v.sc)
		if actual != v.expect {
			t.Errorf("%s failed, expected %t but received %t", k, v.expect, actual)
		}
	}
}

func TestConvertToRuntimeMaskedPaths(t *testing.T) {
	dPM := v1.DefaultProcMount
	uPM := v1.UnmaskedProcMount
	tests := map[string]struct {
		pm     *v1.ProcMountType
		expect []string
	}{
		"procMount nil": {
			pm:     nil,
			expect: defaultMaskedPaths,
		},
		"procMount default": {
			pm:     &dPM,
			expect: defaultMaskedPaths,
		},
		"procMount unmasked": {
			pm:     &uPM,
			expect: []string{},
		},
	}

	for k, v := range tests {
		actual := ConvertToRuntimeMaskedPaths(v.pm)
		if !reflect.DeepEqual(actual, v.expect) {
			t.Errorf("%s failed, expected %#v but received %#v", k, v.expect, actual)
		}
	}
}

func TestConvertToRuntimeReadonlyPaths(t *testing.T) {
	dPM := v1.DefaultProcMount
	uPM := v1.UnmaskedProcMount
	tests := map[string]struct {
		pm     *v1.ProcMountType
		expect []string
	}{
		"procMount nil": {
			pm:     nil,
			expect: defaultReadonlyPaths,
		},
		"procMount default": {
			pm:     &dPM,
			expect: defaultReadonlyPaths,
		},
		"procMount unmasked": {
			pm:     &uPM,
			expect: []string{},
		},
	}

	for k, v := range tests {
		actual := ConvertToRuntimeReadonlyPaths(v.pm)
		if !reflect.DeepEqual(actual, v.expect) {
			t.Errorf("%s failed, expected %#v but received %#v", k, v.expect, actual)
		}
	}
}

func TestDetermineEffectiveRunAsUser(t *testing.T) {
	tests := []struct {
		desc          string
		pod           *v1.Pod
		container     *v1.Container
		wantRunAsUser *int64
	}{
		{
			desc: "no securityContext in pod, no securityContext in container",
			pod: &v1.Pod{
				Spec: v1.PodSpec{},
			},
			container:     &v1.Container{},
			wantRunAsUser: nil,
		},
		{
			desc: "no runAsUser in pod, no runAsUser in container",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					SecurityContext: &v1.PodSecurityContext{},
				},
			},
			container: &v1.Container{
				SecurityContext: &v1.SecurityContext{},
			},
			wantRunAsUser: nil,
		},
		{
			desc: "runAsUser in pod, no runAsUser in container",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					SecurityContext: &v1.PodSecurityContext{
						RunAsUser: new(int64),
					},
				},
			},
			container: &v1.Container{
				SecurityContext: &v1.SecurityContext{},
			},
			wantRunAsUser: new(int64),
		},
		{
			desc: "no runAsUser in pod, runAsUser in container",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					SecurityContext: &v1.PodSecurityContext{},
				},
			},
			container: &v1.Container{
				SecurityContext: &v1.SecurityContext{
					RunAsUser: new(int64),
				},
			},
			wantRunAsUser: new(int64),
		},
		{
			desc: "no runAsUser in pod, runAsUser in container",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					SecurityContext: &v1.PodSecurityContext{
						RunAsUser: new(int64),
					},
				},
			},
			container: &v1.Container{
				SecurityContext: &v1.SecurityContext{
					RunAsUser: utilptr.Int64Ptr(1),
				},
			},
			wantRunAsUser: utilptr.Int64Ptr(1),
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			runAsUser, ok := DetermineEffectiveRunAsUser(test.pod, test.container)
			if !ok && test.wantRunAsUser != nil {
				t.Errorf("DetermineEffectiveRunAsUser(%v, %v) = %v, want %d", test.pod, test.container, runAsUser, *test.wantRunAsUser)
			}
			if ok && test.wantRunAsUser == nil {
				t.Errorf("DetermineEffectiveRunAsUser(%v, %v) = %d, want %v", test.pod, test.container, *runAsUser, test.wantRunAsUser)
			}
			if ok && test.wantRunAsUser != nil && *runAsUser != *test.wantRunAsUser {
				t.Errorf("DetermineEffectiveRunAsUser(%v, %v) = %d, want %d", test.pod, test.container, *runAsUser, *test.wantRunAsUser)
			}
		})
	}
}

func Test_securityContextFromPodSecurityContext(t *testing.T) {
	gmsaCredSpecName := "gmsa spec name"
	gmsaCredSpec := "credential spec"
	username := "ContainerAdministrator"
	asHostProcess := true
	var asuid int64
	var asgid int64
	asuid = 1001
	asgid = 1001
	runasnonroot := true

	tests := []struct {
		name string
		pod  *v1.Pod
		want *v1.SecurityContext
	}{
		{
			name: "SecurityContext is nil",
			pod: &v1.Pod{
				Spec: v1.PodSpec{},
			},
			want: nil,
		},
		{
			name: "test get SecurityContext options",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					SecurityContext: &v1.PodSecurityContext{
						SELinuxOptions: &v1.SELinuxOptions{
							User: "foo",
						},
						WindowsOptions: &v1.WindowsSecurityContextOptions{
							GMSACredentialSpecName: &gmsaCredSpecName,
							GMSACredentialSpec:     &gmsaCredSpec,
							RunAsUserName:          &username,
							HostProcess:            &asHostProcess,
						},
						RunAsUser:    &asuid,
						RunAsGroup:   &asgid,
						RunAsNonRoot: &runasnonroot,
					},
				},
			},
			want: &v1.SecurityContext{
				SELinuxOptions: &v1.SELinuxOptions{
					User: "foo",
				},
				WindowsOptions: &v1.WindowsSecurityContextOptions{
					GMSACredentialSpecName: &gmsaCredSpecName,
					GMSACredentialSpec:     &gmsaCredSpec,
					RunAsUserName:          &username,
					HostProcess:            &asHostProcess,
				},
				RunAsUser:    &asuid,
				RunAsGroup:   &asgid,
				RunAsNonRoot: &runasnonroot,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := securityContextFromPodSecurityContext(tt.pod); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("securityContextFromPodSecurityContext() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetermineEffectiveSecurityContext(t *testing.T) {
	// pod var
	podgmsaCredSpecName := "pod gmsa spec name"
	podgmsaCredSpec := "pod credential spec"
	podusername := "pod username"
	podasHostProcess := true
	var podasuid int64
	var podasgid int64
	podasuid = 1001
	podasgid = 1001
	podrunasnonroot := true

	// container var
	containergmsaCredSpecName := "container gmsa spec name"
	containergmsaCredSpec := "container credential spec"
	containerusername := "container username"
	containerasHostProcess := true
	var containerasuid int64
	var containerasgid int64
	containerasuid = 1
	containerasgid = 1
	containerrunasnonroot := false
	privileged := false
	ReadOnlyRootFilesystem := false
	AllowPrivilegeEscalation := true
	ProcMount := v1.DefaultProcMount

	type args struct {
		pod       *v1.Pod
		container *v1.Container
	}
	tests := []struct {
		name string
		args args
		want *v1.SecurityContext
	}{
		{
			name: "pod and container's SecurityContext are empty",
			args: args{
				pod:       &v1.Pod{},
				container: &v1.Container{},
			},
			want: &v1.SecurityContext{},
		},
		{
			name: "pod's SecurityContext is not empty and container's SecurityContext is empty",
			args: args{
				pod: &v1.Pod{
					Spec: v1.PodSpec{
						SecurityContext: &v1.PodSecurityContext{
							SELinuxOptions: &v1.SELinuxOptions{
								User: "foo",
							},
						},
					},
				},
				container: &v1.Container{},
			},
			want: &v1.SecurityContext{
				SELinuxOptions: &v1.SELinuxOptions{
					User: "foo",
				},
			},
		},
		{
			name: "pod's SecurityContext is empty and container's SecurityContext is not empty",
			args: args{
				pod: &v1.Pod{
					Spec: v1.PodSpec{},
				},
				container: &v1.Container{
					SecurityContext: &v1.SecurityContext{
						RunAsUser:  &podasuid,
						RunAsGroup: &podasgid,
					},
				},
			},
			want: &v1.SecurityContext{
				RunAsUser:  &podasuid,
				RunAsGroup: &podasgid,
			},
		},
		{
			name: "merge SecurityContexts for all values",
			args: args{
				pod: &v1.Pod{
					Spec: v1.PodSpec{
						SecurityContext: &v1.PodSecurityContext{
							SELinuxOptions: &v1.SELinuxOptions{
								User: "foo",
							},
							WindowsOptions: &v1.WindowsSecurityContextOptions{
								GMSACredentialSpecName: &podgmsaCredSpecName,
								GMSACredentialSpec:     &podgmsaCredSpec,
								RunAsUserName:          &podusername,
								HostProcess:            &podasHostProcess,
							},
							RunAsUser:    &podasuid,
							RunAsGroup:   &podasgid,
							RunAsNonRoot: &podrunasnonroot,
						},
					},
				},
				container: &v1.Container{
					SecurityContext: &v1.SecurityContext{
						SELinuxOptions: &v1.SELinuxOptions{
							User: "bar",
						},
						WindowsOptions: &v1.WindowsSecurityContextOptions{
							GMSACredentialSpecName: &containergmsaCredSpecName,
							GMSACredentialSpec:     &containergmsaCredSpec,
							RunAsUserName:          &containerusername,
							HostProcess:            &containerasHostProcess,
						},
						RunAsUser:    &containerasuid,
						RunAsGroup:   &containerasgid,
						RunAsNonRoot: &containerrunasnonroot,
						Capabilities: &v1.Capabilities{
							Add: []v1.Capability{"SYS_CHROOT"},
						},
						Privileged:               &privileged,
						ReadOnlyRootFilesystem:   &ReadOnlyRootFilesystem,
						AllowPrivilegeEscalation: &AllowPrivilegeEscalation,
						ProcMount:                &ProcMount,
					},
				},
			},
			want: &v1.SecurityContext{
				SELinuxOptions: &v1.SELinuxOptions{
					User: "bar",
				},
				WindowsOptions: &v1.WindowsSecurityContextOptions{
					GMSACredentialSpecName: &containergmsaCredSpecName,
					GMSACredentialSpec:     &containergmsaCredSpec,
					RunAsUserName:          &containerusername,
					HostProcess:            &containerasHostProcess,
				},
				RunAsUser:    &containerasuid,
				RunAsGroup:   &containerasgid,
				RunAsNonRoot: &containerrunasnonroot,
				Capabilities: &v1.Capabilities{
					Add: []v1.Capability{"SYS_CHROOT"},
				},
				Privileged:               &privileged,
				ReadOnlyRootFilesystem:   &ReadOnlyRootFilesystem,
				AllowPrivilegeEscalation: &AllowPrivilegeEscalation,
				ProcMount:                &ProcMount,
			},
		},
		{
			name: "merge SecurityContexts for part of values",
			args: args{
				pod: &v1.Pod{
					Spec: v1.PodSpec{
						SecurityContext: &v1.PodSecurityContext{
							SELinuxOptions: &v1.SELinuxOptions{
								User: "foo",
							},
							WindowsOptions: &v1.WindowsSecurityContextOptions{
								GMSACredentialSpecName: &podgmsaCredSpecName,
								GMSACredentialSpec:     &podgmsaCredSpec,
								RunAsUserName:          &podusername,
								HostProcess:            &podasHostProcess,
							},
							RunAsUser:    &podasuid,
							RunAsGroup:   &podasgid,
							RunAsNonRoot: &podrunasnonroot,
						},
					},
				},
				container: &v1.Container{
					SecurityContext: &v1.SecurityContext{
						WindowsOptions: &v1.WindowsSecurityContextOptions{
							GMSACredentialSpecName: &containergmsaCredSpecName,
							GMSACredentialSpec:     &containergmsaCredSpec,
							RunAsUserName:          &containerusername,
							HostProcess:            &containerasHostProcess,
						},
						Privileged:               &privileged,
						ReadOnlyRootFilesystem:   &ReadOnlyRootFilesystem,
						AllowPrivilegeEscalation: &AllowPrivilegeEscalation,
						ProcMount:                &ProcMount,
					},
				},
			},
			want: &v1.SecurityContext{
				SELinuxOptions: &v1.SELinuxOptions{
					User: "foo",
				},
				WindowsOptions: &v1.WindowsSecurityContextOptions{
					GMSACredentialSpecName: &containergmsaCredSpecName,
					GMSACredentialSpec:     &containergmsaCredSpec,
					RunAsUserName:          &containerusername,
					HostProcess:            &containerasHostProcess,
				},
				RunAsUser:                &podasuid,
				RunAsGroup:               &podasgid,
				RunAsNonRoot:             &podrunasnonroot,
				Privileged:               &privileged,
				ReadOnlyRootFilesystem:   &ReadOnlyRootFilesystem,
				AllowPrivilegeEscalation: &AllowPrivilegeEscalation,
				ProcMount:                &ProcMount,
			},
		},
		{
			name: "WindowsOptions values",
			args: args{
				pod: &v1.Pod{
					Spec: v1.PodSpec{
						SecurityContext: &v1.PodSecurityContext{
							WindowsOptions: &v1.WindowsSecurityContextOptions{
								GMSACredentialSpecName: &podgmsaCredSpecName,
								GMSACredentialSpec:     &podgmsaCredSpec,
								RunAsUserName:          &podusername,
								HostProcess:            &podasHostProcess,
							},
						},
					},
				},
				container: &v1.Container{
					SecurityContext: &v1.SecurityContext{
						WindowsOptions: &v1.WindowsSecurityContextOptions{
							GMSACredentialSpec: &containergmsaCredSpec,
							RunAsUserName:      &containerusername,
							HostProcess:        &containerasHostProcess,
						},
					},
				},
			},
			want: &v1.SecurityContext{
				WindowsOptions: &v1.WindowsSecurityContextOptions{
					GMSACredentialSpecName: nil,
					GMSACredentialSpec:     &containergmsaCredSpec,
					RunAsUserName:          &containerusername,
					HostProcess:            &containerasHostProcess,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetermineEffectiveSecurityContext(tt.args.pod, tt.args.container); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DetermineEffectiveSecurityContext() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasWindowsHostProcessRequest(t *testing.T) {
	gmsaCredSpecName := "gmsa spec name"
	asHostProcess := true
	type args struct {
		pod       *v1.Pod
		container *v1.Container
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "WindowsOptions is nil",
			args: args{
				pod:       &v1.Pod{},
				container: &v1.Container{},
			},
			want: false,
		},
		{
			name: "WindowsOptions and HostProcess are not nil ",
			args: args{
				pod: &v1.Pod{
					Spec: v1.PodSpec{
						SecurityContext: &v1.PodSecurityContext{
							SELinuxOptions: &v1.SELinuxOptions{
								User: "foo",
							},
							WindowsOptions: &v1.WindowsSecurityContextOptions{
								GMSACredentialSpecName: &gmsaCredSpecName,
								HostProcess:            &asHostProcess,
							},
						},
					},
				},
				container: &v1.Container{},
			},
			want: true,
		},
		{
			name: "HostProcess is nil ",
			args: args{
				pod: &v1.Pod{
					Spec: v1.PodSpec{
						SecurityContext: &v1.PodSecurityContext{
							SELinuxOptions: &v1.SELinuxOptions{
								User: "foo",
							},
							WindowsOptions: &v1.WindowsSecurityContextOptions{
								GMSACredentialSpecName: &gmsaCredSpecName,
							},
						},
					},
				},
				container: &v1.Container{},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasWindowsHostProcessRequest(tt.args.pod, tt.args.container); got != tt.want {
				t.Errorf("HasWindowsHostProcessRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}
