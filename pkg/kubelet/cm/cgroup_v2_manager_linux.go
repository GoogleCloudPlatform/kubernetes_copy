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
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opencontainers/runc/libcontainer/cgroups/fscommon"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	cmutil "k8s.io/kubernetes/pkg/kubelet/cm/util"
)

const cgroupv2MemLimitFile string = "memory.max"

// cgroupV2impl implements the CgroupManager interface
// for cgroup v2.
// It's a stateless object which can be used to
// update, create or delete any number of cgroups
// It relies on runc/libcontainer cgroup managers.
type cgroupV2impl struct {
	cgroupCommon
}

func NewCgroupV2Manager(cs *CgroupSubsystems, cgroupDriver string) CgroupManager {
	return &cgroupV2impl{
		cgroupCommon: newCgroupCommon(cs, cgroupDriver),
	}
}

// Version of the cgroup implementation on the host
func (c *cgroupV2impl) Version() int {
	return 2
}

// Validate checks if all subsystem cgroups are valid
func (c *cgroupV2impl) Validate(name CgroupName) error {
	cgroupPath := c.buildCgroupUnifiedPath(name)
	neededControllers := getSupportedUnifiedControllers()
	enabledControllers, err := readUnifiedControllers(cgroupPath)
	if err != nil {
		return fmt.Errorf("could not read controllers for cgroup %q: %w", name, err)
	}
	difference := neededControllers.Difference(enabledControllers)
	if difference.Len() > 0 {
		return fmt.Errorf("cgroup %q has some missing controllers: %v", name, strings.Join(sets.List(difference), ", "))
	}
	return nil
}

// Exists checks if all subsystem cgroups already exist
func (c *cgroupV2impl) Exists(name CgroupName) bool {
	return c.Validate(name) == nil
}

// MemoryUsage returns the current memory usage of the specified cgroup,
// as read from cgroupfs.
func (c *cgroupV2impl) MemoryUsage(name CgroupName) (int64, error) {
	var path, file string
	path = c.buildCgroupUnifiedPath(name)
	file = "memory.current"
	val, err := fscommon.GetCgroupParamUint(path, file)
	return int64(val), err
}

// Get the resource config values applied to the cgroup for specified resource type
func (c *cgroupV2impl) GetCgroupConfig(name CgroupName, resource v1.ResourceName) (*ResourceConfig, error) {
	cgroupPaths := c.buildCgroupPaths(name)
	cgroupResourcePath, found := cgroupPaths[string(resource)]
	if !found {
		return nil, fmt.Errorf("failed to build %v cgroup fs path for cgroup %v", resource, name)
	}
	switch resource {
	case v1.ResourceCPU:
		return c.getCgroupCPUConfig(cgroupResourcePath)
	case v1.ResourceMemory:
		return c.getCgroupMemoryConfig(cgroupResourcePath)
	}
	return nil, fmt.Errorf("unsupported resource %v for cgroup %v", resource, name)
}

func (c *cgroupV2impl) getCgroupCPUConfig(cgroupPath string) (*ResourceConfig, error) {
	var cpuLimitStr, cpuPeriodStr string
	cpuLimitAndPeriod, err := fscommon.GetCgroupParamString(cgroupPath, "cpu.max")
	if err != nil {
		return nil, fmt.Errorf("failed to read cpu.max file for cgroup %v: %w", cgroupPath, err)
	}
	numItems, errScan := fmt.Sscanf(cpuLimitAndPeriod, "%s %s", &cpuLimitStr, &cpuPeriodStr)
	if errScan != nil || numItems != 2 {
		return nil, fmt.Errorf("failed to correctly parse content of cpu.max file ('%s') for cgroup %v: %w",
			cpuLimitAndPeriod, cgroupPath, errScan)
	}
	cpuLimit := int64(-1)
	if cpuLimitStr != Cgroup2MaxCpuLimit {
		cpuLimit, err = strconv.ParseInt(cpuLimitStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to convert CPU limit as integer for cgroup %v: %w", cgroupPath, err)
		}
	}
	cpuPeriod, errPeriod := strconv.ParseUint(cpuPeriodStr, 10, 64)
	if errPeriod != nil {
		return nil, fmt.Errorf("failed to convert CPU period as integer for cgroup %v: %w", cgroupPath, errPeriod)
	}
	cpuWeight, errWeight := fscommon.GetCgroupParamUint(cgroupPath, "cpu.weight")
	if errWeight != nil {
		return nil, fmt.Errorf("failed to read CPU weight for cgroup %v: %w", cgroupPath, errWeight)
	}
	cpuShares, err := cmutil.CPUWeightToCPUShares(cpuWeight)
	if err != nil {
		return nil, fmt.Errorf("failed to convert cgroup v1 CPU weight %d to v2 CPU shares: %w", cpuWeight, err)
	}
	return &ResourceConfig{CPUShares: &cpuShares, CPUQuota: &cpuLimit, CPUPeriod: &cpuPeriod}, nil
}

func (c *cgroupV2impl) getCgroupMemoryConfig(cgroupPath string) (*ResourceConfig, error) {
	return readCgroupMemoryConfig(cgroupPath, cgroupv2MemLimitFile)
}

// getSupportedUnifiedControllers returns a set of supported controllers when running on cgroup v2
func getSupportedUnifiedControllers() sets.Set[string] {
	// This is the set of controllers used by the Kubelet
	supportedControllers := sets.New("cpu", "cpuset", "memory", "hugetlb", "pids")
	// Memoize the set of controllers that are present in the root cgroup
	availableRootControllersOnce.Do(func() {
		var err error
		availableRootControllers, err = readUnifiedControllers(cmutil.CgroupRoot)
		if err != nil {
			panic(fmt.Errorf("cannot read cgroup controllers at %s", cmutil.CgroupRoot))
		}
	})
	// Return the set of controllers that are supported both by the Kubelet and by the kernel
	return supportedControllers.Intersection(availableRootControllers)
}

// readUnifiedControllers reads the controllers available at the specified cgroup
func readUnifiedControllers(path string) (sets.Set[string], error) {
	controllersFileContent, err := os.ReadFile(filepath.Join(path, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	controllers := strings.Fields(string(controllersFileContent))
	return sets.New(controllers...), nil
}

// buildCgroupUnifiedPath builds a path to the specified name.
func (c *cgroupV2impl) buildCgroupUnifiedPath(name CgroupName) string {
	cgroupFsAdaptedName := c.Name(name)
	return path.Join(cmutil.CgroupRoot, cgroupFsAdaptedName)
}
