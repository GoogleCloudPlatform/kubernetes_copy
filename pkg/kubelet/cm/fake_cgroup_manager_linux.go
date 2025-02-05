/*
Copyright 2024 The Kubernetes Authors.

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
	"sync"

	v1 "k8s.io/api/core/v1"
)

type FakeCgroupManager struct {
	sync.Mutex
	CalledFunctions []string

	// Return values
	create error
	exists bool
}

func (cgm *FakeCgroupManager) Create(_ *CgroupConfig) error {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Create")
	return cgm.create
}

// Destroy the cgroup.
func (cgm *FakeCgroupManager) Destroy(_ *CgroupConfig) error {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Destroy")
	return nil
}

// Update cgroup configuration.
func (cgm *FakeCgroupManager) Update(_ *CgroupConfig) error {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Update")
	return nil
}

// Validate checks if the cgroup is valid
func (cgm *FakeCgroupManager) Validate(name CgroupName) error {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Validate")
	return nil
}

// Exists checks if the cgroup already exists
func (cgm *FakeCgroupManager) Exists(name CgroupName) bool {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Exists")
	return cgm.exists
}

// Name returns the literal cgroupfs name on the host after any driver specific conversions.
// We would expect systemd implementation to make appropriate name conversion.
// For example, if we pass {"foo", "bar"}
// then systemd should convert the name to something like
// foo.slice/foo-bar.slice
func (cgm *FakeCgroupManager) Name(name CgroupName) string {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Name")
	return ""
}

// CgroupName converts the literal cgroupfs name on the host to an internal identifier.
func (cgm *FakeCgroupManager) CgroupName(name string) CgroupName {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "CgroupName")
	return nil
}

// Pids scans through all subsystems to find pids associated with specified cgroup.
func (cgm *FakeCgroupManager) Pids(name CgroupName) []int {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Pids")
	return nil
}

// ReduceCPULimits reduces the CPU CFS values to the minimum amount of shares.
func (cgm *FakeCgroupManager) ReduceCPULimits(cgroupName CgroupName) error {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "ReduceCPULimits")
	return nil
}

// MemoryUsage returns current memory usage of the specified cgroup, as read from the cgroupfs.
func (cgm *FakeCgroupManager) MemoryUsage(name CgroupName) (int64, error) {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "MemoryUsage")
	return 0, nil
}

// Get the resource config values applied to the cgroup for specified resource type
func (cgm *FakeCgroupManager) GetCgroupConfig(name CgroupName, resource v1.ResourceName) (*ResourceConfig, error) {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "GetCgroupConfig")
	return nil, nil
}

// Set resource config for the specified resource type on the cgroup
func (cgm *FakeCgroupManager) SetCgroupConfig(name CgroupName, resourceConfig *ResourceConfig) error {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "SetCgroupConfig")
	return nil
}

// Version of the cgroup implementation on the host
func (cgm *FakeCgroupManager) Version() int {
	cgm.Lock()
	defer cgm.Unlock()
	cgm.CalledFunctions = append(cgm.CalledFunctions, "Version")
	return 0
}
