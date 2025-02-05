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

package cm

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/cpuset"
)

// ResourceConfig holds information about all the supported cgroup resource parameters.
type ResourceConfig struct {
	// Memory limit (in bytes).
	Memory *int64
	// CPU set (number of CPUs the cgroup has access to).
	CPUSet cpuset.CPUSet
	// CPU shares (relative weight vs. other containers).
	CPUShares *uint64
	// CPU hardcap limit (in usecs). Allowed cpu time in a given period.
	CPUQuota *int64
	// CPU quota period.
	CPUPeriod *uint64
	// HugePageLimit map from page size (in bytes) to limit (in bytes)
	HugePageLimit map[int64]int64
	// Maximum number of pids
	PidsLimit *int64
	// Unified for cgroup v2
	Unified map[string]string
}

// CgroupName is the abstract name of a cgroup prior to any driver specific conversion.
// It is specified as a list of strings from its individual components, such as:
// {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}
type CgroupName []string

// CgroupConfig holds the cgroup configuration information.
// This is common object which is used to specify
// cgroup information to both systemd and raw cgroup fs
// implementation of the Cgroup Manager interface.
type CgroupConfig struct {
	// Fully qualified name prior to any driver specific conversions.
	Name CgroupName
	// ResourceParameters contains various cgroups settings to apply.
	ResourceParameters *ResourceConfig
}

// CgroupManager allows for cgroup management.
// Supports Cgroup Creation ,Deletion and Updates.
type CgroupManager interface {
	// Create creates and applies the cgroup configurations on the cgroup.
	// It just creates the leaf cgroups.
	// It expects the parent cgroup to already exist.
	Create(*CgroupConfig) error
	// Destroy the cgroup.
	Destroy(*CgroupConfig) error
	// Update cgroup configuration.
	Update(*CgroupConfig) error
	// Validate checks if the cgroup is valid
	Validate(name CgroupName) error
	// Exists checks if the cgroup already exists
	Exists(name CgroupName) bool
	// Name returns the literal cgroupfs name on the host after any driver specific conversions.
	// We would expect systemd implementation to make appropriate name conversion.
	// For example, if we pass {"foo", "bar"}
	// then systemd should convert the name to something like
	// foo.slice/foo-bar.slice
	Name(name CgroupName) string
	// CgroupName converts the literal cgroupfs name on the host to an internal identifier.
	CgroupName(name string) CgroupName
	// Pids scans through all subsystems to find pids associated with specified cgroup.
	Pids(name CgroupName) []int
	// ReduceCPULimits reduces the CPU CFS values to the minimum amount of shares.
	ReduceCPULimits(cgroupName CgroupName) error
	// MemoryUsage returns current memory usage of the specified cgroup, as read from the cgroupfs.
	MemoryUsage(name CgroupName) (int64, error)
	// Get the resource config values applied to the cgroup for specified resource type
	GetCgroupConfig(name CgroupName, resource v1.ResourceName) (*ResourceConfig, error)
	// Set resource config for the specified resource type on the cgroup
	SetCgroupConfig(name CgroupName, resourceConfig *ResourceConfig) error
	// Version of the cgroup implementation on the host
	Version() int
}

// QOSContainersInfo stores the names of containers per qos
type QOSContainersInfo struct {
	Guaranteed CgroupName
	BestEffort CgroupName
	Burstable  CgroupName
}

// PodContainerManager stores and manages pod level containers
// The Pod workers interact with the PodContainerManager to create and destroy
// containers for the pod.
type PodContainerManager interface {
	// GetPodContainerName returns the CgroupName identifier, and its literal cgroupfs form on the host.
	GetPodContainerName(*v1.Pod) (CgroupName, string)

	// EnsureExists takes a pod as argument and makes sure that
	// pod cgroup exists if qos cgroup hierarchy flag is enabled.
	// If the pod cgroup doesn't already exist this method creates it.
	EnsureExists(*v1.Pod) error

	// Exists returns true if the pod cgroup exists.
	Exists(*v1.Pod) bool

	// Destroy takes a pod Cgroup name as argument and destroys the pod's container.
	Destroy(name CgroupName) error

	// ReduceCPULimits reduces the CPU CFS values to the minimum amount of shares.
	ReduceCPULimits(name CgroupName) error

	// GetAllPodsFromCgroups enumerates the set of pod uids to their associated cgroup based on state of cgroupfs system.
	GetAllPodsFromCgroups() (map[types.UID]CgroupName, error)

	// IsPodCgroup returns true if the literal cgroupfs name corresponds to a pod
	IsPodCgroup(cgroupfs string) (bool, types.UID)

	// Get value of memory usage for the pod Cgroup
	GetPodCgroupMemoryUsage(pod *v1.Pod) (uint64, error)

	// Get the resource config values applied to the pod cgroup for specified resource type
	GetPodCgroupConfig(pod *v1.Pod, resource v1.ResourceName) (*ResourceConfig, error)

	// Set resource config values for the specified resource type on the pod cgroup
	SetPodCgroupConfig(pod *v1.Pod, resourceConfig *ResourceConfig) error
}

type ResourceManager interface {
	CanAllocateExclusively(resName v1.ResourceName) bool
}
