//go:build linux
// +build linux

/*
Copyright 2015 The Kubernetes Authors.

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

package cm

import (
	"errors"
	pointer "k8s.io/utils/ptr"
	"os"
	"path"
	"testing"
	"time"

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	cadvisortest "k8s.io/kubernetes/pkg/kubelet/cadvisor/testing"

	"k8s.io/mount-utils"
)

func fakeContainerMgrMountInt() mount.Interface {
	return mount.NewFakeMounter(
		[]mount.MountPoint{
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuset"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpu"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuacct"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "memory"},
			},
		})
}

func TestCgroupMountValidationSuccess(t *testing.T) {
	f, err := validateSystemRequirements(fakeContainerMgrMountInt())
	assert.NoError(t, err)
	if cgroups.IsCgroup2UnifiedMode() {
		assert.True(t, f.cpuHardcapping, "cpu hardcapping is expected to be enabled")
	} else {
		assert.False(t, f.cpuHardcapping, "cpu hardcapping is expected to be disabled")
	}
}

func TestCgroupMountValidationMemoryMissing(t *testing.T) {
	if cgroups.IsCgroup2UnifiedMode() {
		t.Skip("skipping cgroup v1 test on a cgroup v2 system")
	}
	mountInt := mount.NewFakeMounter(
		[]mount.MountPoint{
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuset"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpu"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuacct"},
			},
		})
	_, err := validateSystemRequirements(mountInt)
	assert.Error(t, err)
}

func TestCgroupMountValidationMultipleSubsystem(t *testing.T) {
	if cgroups.IsCgroup2UnifiedMode() {
		t.Skip("skipping cgroup v1 test on a cgroup v2 system")
	}
	mountInt := mount.NewFakeMounter(
		[]mount.MountPoint{
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuset", "memory"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpu"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuacct"},
			},
		})
	_, err := validateSystemRequirements(mountInt)
	assert.NoError(t, err)
}

func TestGetCpuWeight(t *testing.T) {
	weight, err := getCPUWeight(nil)
	req := require.New(t)
	req.NoError(err)
	req.Equal(uint64(0), weight)

	weight, err = getCPUWeight(pointer.To(uint64(2)))
	req.NoError(err)
	req.Equal(uint64(1), weight)

	weight, err = getCPUWeight(pointer.To(uint64(262144)))
	req.NoError(err)
	req.Equal(uint64(10000), weight)

	_, err = getCPUWeight(pointer.To(uint64(1000000000)))
	req.Error(err)
}

func TestSoftRequirementsValidationSuccess(t *testing.T) {
	if cgroups.IsCgroup2UnifiedMode() {
		t.Skip("skipping cgroup v1 test on a cgroup v2 system")
	}
	req := require.New(t)
	tempDir, err := os.MkdirTemp("", "")
	req.NoError(err)
	defer os.RemoveAll(tempDir)
	req.NoError(os.WriteFile(path.Join(tempDir, "cpu.cfs_period_us"), []byte("0"), os.ModePerm))
	req.NoError(os.WriteFile(path.Join(tempDir, "cpu.cfs_quota_us"), []byte("0"), os.ModePerm))
	mountInt := mount.NewFakeMounter(
		[]mount.MountPoint{
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuset"},
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpu"},
				Path:   tempDir,
			},
			{
				Device: "cgroup",
				Type:   "cgroup",
				Opts:   []string{"rw", "relatime", "cpuacct", "memory"},
			},
		})
	f, err := validateSystemRequirements(mountInt)
	assert.NoError(t, err)
	assert.True(t, f.cpuHardcapping, "cpu hardcapping is expected to be enabled")
}

func TestGetCapacity(t *testing.T) {
	ephemeralStorageFromCapacity := int64(2000)
	ephemeralStorageFromCadvisor := int64(8000)

	mockCadvisor := cadvisortest.NewMockInterface(t)
	rootfs := cadvisorapiv2.FsInfo{
		Capacity: 8000,
	}
	mockCadvisor.EXPECT().RootFsInfo().Return(rootfs, nil)
	mockCadvisorError := cadvisortest.NewMockInterface(t)
	mockCadvisorError.EXPECT().RootFsInfo().Return(cadvisorapiv2.FsInfo{}, errors.New("Unable to get rootfs data from cAdvisor interface"))
	cases := []struct {
		name                                 string
		cm                                   *containerManagerImpl
		expectedResourceQuantity             *resource.Quantity
		expectedNoEphemeralStorage           bool
		disablelocalStorageCapacityIsolation bool
	}{
		{
			name: "capacity property has ephemeral-storage",
			cm: &containerManagerImpl{
				cadvisorInterface: mockCadvisor,
				capacity: v1.ResourceList{
					v1.ResourceEphemeralStorage: *resource.NewQuantity(ephemeralStorageFromCapacity, resource.BinarySI),
				},
			},
			expectedResourceQuantity:   resource.NewQuantity(ephemeralStorageFromCapacity, resource.BinarySI),
			expectedNoEphemeralStorage: false,
		},
		{
			name: "capacity property does not have ephemeral-storage",
			cm: &containerManagerImpl{
				cadvisorInterface: mockCadvisor,
				capacity:          v1.ResourceList{},
			},
			expectedResourceQuantity:   resource.NewQuantity(ephemeralStorageFromCadvisor, resource.BinarySI),
			expectedNoEphemeralStorage: false,
		},
		{
			name: "capacity property does not have ephemeral-storage, error from rootfs",
			cm: &containerManagerImpl{
				cadvisorInterface: mockCadvisorError,
				capacity:          v1.ResourceList{},
			},
			expectedNoEphemeralStorage: true,
		},
		{
			name: "capacity property does not have ephemeral-storage, cadvisor interface is nil",
			cm: &containerManagerImpl{
				cadvisorInterface: nil,
				capacity:          v1.ResourceList{},
			},
			expectedNoEphemeralStorage: true,
		},
		{
			name: "capacity property has ephemeral-storage, but localStorageCapacityIsolation is disabled",
			cm: &containerManagerImpl{
				cadvisorInterface: mockCadvisor,
				capacity: v1.ResourceList{
					v1.ResourceEphemeralStorage: *resource.NewQuantity(ephemeralStorageFromCapacity, resource.BinarySI),
				},
			},
			expectedResourceQuantity:             resource.NewQuantity(ephemeralStorageFromCapacity, resource.BinarySI),
			expectedNoEphemeralStorage:           true,
			disablelocalStorageCapacityIsolation: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ret := c.cm.GetCapacity(!c.disablelocalStorageCapacityIsolation)
			if v, exists := ret[v1.ResourceEphemeralStorage]; !exists {
				if !c.expectedNoEphemeralStorage {
					t.Errorf("did not get any ephemeral storage data")
				}
			} else {
				if v.Value() != c.expectedResourceQuantity.Value() {
					t.Errorf("got unexpected %s value, expected %d, got %d", v1.ResourceEphemeralStorage, c.expectedResourceQuantity.Value(), v.Value())
				}
			}
		})
	}
}

func TestNewPodContainerManager(t *testing.T) {

	info := QOSContainersInfo{
		Guaranteed: CgroupName{"guaranteed"},
		BestEffort: CgroupName{"besteffort"},
		Burstable:  CgroupName{"burstable"},
	}
	QosEnabled := NodeConfig{
		CgroupsPerQOS: true,
	}
	QosDisabled := NodeConfig{
		CgroupsPerQOS: false,
	}

	cases := []struct {
		name string
		cm   *containerManagerImpl
	}{
		{
			name: "CgroupsPerQOS is disabled, return *podContainerManagerNoop",
			cm: &containerManagerImpl{
				qosContainerManager: &qosContainerManagerImpl{
					qosContainersInfo: info,
					cgroupManager:     NewCgroupManager(&CgroupSubsystems{}, ""),
				},

				NodeConfig: QosDisabled,
			},
		},
		{
			name: "CgroupsPerQOS is enabled, return *podContainerManagerImpl",
			cm: &containerManagerImpl{
				qosContainerManager: &qosContainerManagerImpl{
					qosContainersInfo: info,
					cgroupManager:     NewCgroupManager(&CgroupSubsystems{}, ""),
				},

				NodeConfig: QosEnabled,
			},
		},
		{
			name: "CgroupsPerQOS is enabled, use systemd",
			cm: &containerManagerImpl{
				qosContainerManager: &qosContainerManagerImpl{
					qosContainersInfo: info,
					cgroupManager:     NewCgroupManager(&CgroupSubsystems{}, "systemd"),
				},

				NodeConfig: QosEnabled,
			},
		},
		{
			name: "CgroupsPerQOS is disabled, use systemd",
			cm: &containerManagerImpl{
				qosContainerManager: &qosContainerManagerImpl{
					qosContainersInfo: info,
					cgroupManager:     NewCgroupManager(&CgroupSubsystems{}, "systemd"),
				},

				NodeConfig: QosDisabled,
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pcm := c.cm.NewPodContainerManager()
			if c.cm.NodeConfig.CgroupsPerQOS {
				assert.IsType(t, &podContainerManagerImpl{}, pcm)
				got := pcm.(*podContainerManagerImpl)
				assert.Equal(t, c.cm.subsystems, got.subsystems)
				assert.Equal(t, c.cm.cgroupManager, got.cgroupManager)
				assert.Equal(t, c.cm.PodPidsLimit, got.podPidsLimit)
				assert.Equal(t, c.cm.EnforceCPULimits, got.enforceCPULimits)
				assert.Equal(t, uint64(c.cm.CPUCFSQuotaPeriod/time.Microsecond), got.cpuCFSQuotaPeriod)

			} else {
				assert.IsType(t, &podContainerManagerNoop{}, pcm)
				got := pcm.(*podContainerManagerNoop)
				assert.Equal(t, c.cm.cgroupRoot, got.cgroupRoot)
			}
		})
	}
}
