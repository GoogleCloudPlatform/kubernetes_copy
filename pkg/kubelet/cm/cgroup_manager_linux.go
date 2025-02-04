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
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fscommon"
	libcontainercgroupmanager "github.com/opencontainers/runc/libcontainer/cgroups/manager"
	cgroupsystemd "github.com/opencontainers/runc/libcontainer/cgroups/systemd"
	libcontainerconfigs "github.com/opencontainers/runc/libcontainer/configs"
	"k8s.io/klog/v2"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

const (
	// systemdSuffix is the cgroup name suffix for systemd
	systemdSuffix string = ".slice"
	// Cgroup2MemoryMin is memory.min for cgroup v2
	Cgroup2MemoryMin string = "memory.min"
	// Cgroup2MemoryHigh is memory.high for cgroup v2
	Cgroup2MemoryHigh      string = "memory.high"
	Cgroup2MaxCpuLimit     string = "max"
	Cgroup2MaxSwapFilename string = "memory.swap.max"
)

var RootCgroupName = CgroupName([]string{})

// NewCgroupName composes a new cgroup name.
// Use RootCgroupName as base to start at the root.
// This function does some basic check for invalid characters at the name.
func NewCgroupName(base CgroupName, components ...string) CgroupName {
	for _, component := range components {
		// Forbid using "_" in internal names. When remapping internal
		// names to systemd cgroup driver, we want to remap "-" => "_",
		// so we forbid "_" so that we can always reverse the mapping.
		if strings.Contains(component, "/") || strings.Contains(component, "_") {
			panic(fmt.Errorf("invalid character in component [%q] of CgroupName", component))
		}
	}
	return CgroupName(append(append([]string{}, base...), components...))
}

func escapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "-", "_", -1)
}

func unescapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "_", "-", -1)
}

// cgroupName.ToSystemd converts the internal cgroup name to a systemd name.
// For example, the name {"kubepods", "burstable", "pod1234-abcd-5678-efgh"} becomes
// "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice"
// This function always expands the systemd name into the cgroupfs form. If only
// the last part is needed, use path.Base(...) on it to discard the rest.
func (cgroupName CgroupName) ToSystemd() string {
	if len(cgroupName) == 0 || (len(cgroupName) == 1 && cgroupName[0] == "") {
		return "/"
	}
	newparts := []string{}
	for _, part := range cgroupName {
		part = escapeSystemdCgroupName(part)
		newparts = append(newparts, part)
	}

	result, err := cgroupsystemd.ExpandSlice(strings.Join(newparts, "-") + systemdSuffix)
	if err != nil {
		// Should never happen...
		panic(fmt.Errorf("error converting cgroup name [%v] to systemd format: %v", cgroupName, err))
	}
	return result
}

func ParseSystemdToCgroupName(name string) CgroupName {
	driverName := path.Base(name)
	driverName = strings.TrimSuffix(driverName, systemdSuffix)
	parts := strings.Split(driverName, "-")
	result := []string{}
	for _, part := range parts {
		result = append(result, unescapeSystemdCgroupName(part))
	}
	return CgroupName(result)
}

func (cgroupName CgroupName) ToCgroupfs() string {
	return "/" + path.Join(cgroupName...)
}

func ParseCgroupfsToCgroupName(name string) CgroupName {
	components := strings.Split(strings.TrimPrefix(name, "/"), "/")
	if len(components) == 1 && components[0] == "" {
		components = []string{}
	}
	return CgroupName(components)
}

func IsSystemdStyleName(name string) bool {
	return strings.HasSuffix(name, systemdSuffix)
}

// CgroupSubsystems holds information about the mounted cgroup subsystems
type CgroupSubsystems struct {
	// Cgroup subsystem mounts.
	// e.g.: "/sys/fs/cgroup/cpu" -> ["cpu", "cpuacct"]
	Mounts []libcontainercgroups.Mount

	// Cgroup subsystem to their mount location.
	// e.g.: "cpu" -> "/sys/fs/cgroup/cpu"
	MountPoints map[string]string
}

// cgroupCommon implements common tasks
// that are valid for both cgroup v1 and v2.
// This prevents duplicating the code between
// v1 and v2 specific implementations.
type cgroupCommon struct {
	// subsystems holds information about all the
	// mounted cgroup subsystems on the node
	subsystems *CgroupSubsystems

	// useSystemd tells if systemd cgroup manager should be used.
	useSystemd bool
}

// Make sure that cgroupV1impl and cgroupV2impl implement the CgroupManager interface
var _ CgroupManager = &cgroupV1impl{}
var _ CgroupManager = &cgroupV2impl{}

// NewCgroupManager is a factory method that returns a CgroupManager
func NewCgroupManager(cs *CgroupSubsystems, cgroupDriver string) CgroupManager {
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		return NewCgroupV2Manager(cs, cgroupDriver)
	}
	return NewCgroupV1Manager(cs, cgroupDriver)
}

func newCgroupCommon(cs *CgroupSubsystems, cgroupDriver string) cgroupCommon {
	return cgroupCommon{
		subsystems: cs,
		useSystemd: cgroupDriver == "systemd",
	}
}

// Name converts the cgroup to the driver specific value in cgroupfs form.
// This always returns a valid cgroupfs path even when systemd driver is in use!
func (m *cgroupCommon) Name(name CgroupName) string {
	if m.useSystemd {
		return name.ToSystemd()
	}
	return name.ToCgroupfs()
}

// CgroupName converts the literal cgroupfs name on the host to an internal identifier.
func (m *cgroupCommon) CgroupName(name string) CgroupName {
	if m.useSystemd {
		return ParseSystemdToCgroupName(name)
	}
	return ParseCgroupfsToCgroupName(name)
}

// buildCgroupPaths builds a path to each cgroup subsystem for the specified name.
func (m *cgroupCommon) buildCgroupPaths(name CgroupName) map[string]string {
	cgroupFsAdaptedName := m.Name(name)
	cgroupPaths := make(map[string]string, len(m.subsystems.MountPoints))
	for key, val := range m.subsystems.MountPoints {
		cgroupPaths[key] = path.Join(val, cgroupFsAdaptedName)
	}
	return cgroupPaths
}

// libctCgroupConfig converts CgroupConfig to libcontainer's Cgroup config.
func (m *cgroupCommon) libctCgroupConfig(in *CgroupConfig, needResources bool) *libcontainerconfigs.Cgroup {
	config := &libcontainerconfigs.Cgroup{
		Systemd: m.useSystemd,
	}
	if needResources {
		config.Resources = m.toResources(in.ResourceParameters)
	} else {
		config.Resources = &libcontainerconfigs.Resources{}
	}

	if !config.Systemd {
		// For fs cgroup manager, we can either set Path or Name and Parent.
		// Setting Path is easier.
		config.Path = in.Name.ToCgroupfs()

		return config
	}

	// For systemd, we have to set Name and Parent, as they are needed to talk to systemd.
	// Setting Path is optional as it can be deduced from Name and Parent.

	// TODO(filbranden): This logic belongs in libcontainer/cgroup/systemd instead.
	// It should take a libcontainerconfigs.Cgroup.Path field (rather than Name and Parent)
	// and split it appropriately, using essentially the logic below.
	// This was done for cgroupfs in opencontainers/runc#497 but a counterpart
	// for systemd was never introduced.
	dir, base := path.Split(in.Name.ToSystemd())
	if dir == "/" {
		dir = "-.slice"
	} else {
		dir = path.Base(dir)
	}
	config.Parent = dir
	config.Name = base

	return config
}

// Destroy destroys the specified cgroup
func (m *cgroupCommon) Destroy(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("destroy").Observe(metrics.SinceInSeconds(start))
	}()

	libcontainerCgroupConfig := m.libctCgroupConfig(cgroupConfig, false)
	manager, err := libcontainercgroupmanager.New(libcontainerCgroupConfig)
	if err != nil {
		return err
	}

	// Delete cgroups using libcontainers Managers Destroy() method
	if err = manager.Destroy(); err != nil {
		return fmt.Errorf("unable to destroy cgroup paths for cgroup %v : %v", cgroupConfig.Name, err)
	}

	return nil
}

func (m *cgroupCommon) SetCgroupConfig(name CgroupName, resourceConfig *ResourceConfig) error {
	containerConfig := &CgroupConfig{
		Name:               name,
		ResourceParameters: resourceConfig,
	}

	return m.Update(containerConfig)
}

// getCPUWeight converts from the range [2, 262144] to [1, 10000]
func getCPUWeight(cpuShares *uint64) uint64 {
	if cpuShares == nil {
		return 0
	}
	if *cpuShares >= 262144 {
		return 10000
	}
	return 1 + ((*cpuShares-2)*9999)/262142
}

var (
	availableRootControllersOnce sync.Once
	availableRootControllers     sets.Set[string]
)

func (m *cgroupCommon) toResources(resourceConfig *ResourceConfig) *libcontainerconfigs.Resources {
	resources := &libcontainerconfigs.Resources{
		SkipDevices:     true,
		SkipFreezeOnSet: true,
	}
	if resourceConfig == nil {
		return resources
	}
	if resourceConfig.Memory != nil {
		resources.Memory = *resourceConfig.Memory
	}
	if resourceConfig.CPUShares != nil {
		if libcontainercgroups.IsCgroup2UnifiedMode() {
			resources.CpuWeight = getCPUWeight(resourceConfig.CPUShares)
		} else {
			resources.CpuShares = *resourceConfig.CPUShares
		}
	}
	if resourceConfig.CPUQuota != nil {
		resources.CpuQuota = *resourceConfig.CPUQuota
	}
	if resourceConfig.CPUPeriod != nil {
		resources.CpuPeriod = *resourceConfig.CPUPeriod
	}
	if resourceConfig.PidsLimit != nil {
		resources.PidsLimit = *resourceConfig.PidsLimit
	}
	if !resourceConfig.CPUSet.IsEmpty() {
		resources.CpusetCpus = resourceConfig.CPUSet.String()
	}

	m.maybeSetHugetlb(resourceConfig, resources)

	// Ideally unified is used for all the resources when running on cgroup v2.
	// It doesn't make difference for the memory.max limit, but for e.g. the cpu controller
	// you can specify the correct setting without relying on the conversions performed by the OCI runtime.
	if resourceConfig.Unified != nil && libcontainercgroups.IsCgroup2UnifiedMode() {
		resources.Unified = make(map[string]string)
		for k, v := range resourceConfig.Unified {
			resources.Unified[k] = v
		}
	}
	return resources
}

func (m *cgroupCommon) maybeSetHugetlb(resourceConfig *ResourceConfig, resources *libcontainerconfigs.Resources) {
	// Check if hugetlb is supported.
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		if !getSupportedUnifiedControllers().Has("hugetlb") {
			klog.V(6).InfoS("Optional subsystem not supported: hugetlb")
			return
		}
	} else if _, ok := m.subsystems.MountPoints["hugetlb"]; !ok {
		klog.V(6).InfoS("Optional subsystem not supported: hugetlb")
		return
	}

	// For each page size enumerated, set that value.
	pageSizes := sets.New[string]()
	for pageSize, limit := range resourceConfig.HugePageLimit {
		sizeString, err := v1helper.HugePageUnitSizeFromByteSize(pageSize)
		if err != nil {
			klog.InfoS("Invalid pageSize", "err", err)
			continue
		}
		resources.HugetlbLimit = append(resources.HugetlbLimit, &libcontainerconfigs.HugepageLimit{
			Pagesize: sizeString,
			Limit:    uint64(limit),
		})
		pageSizes.Insert(sizeString)
	}
	// for each page size omitted, limit to 0
	for _, pageSize := range libcontainercgroups.HugePageSizes() {
		if pageSizes.Has(pageSize) {
			continue
		}
		resources.HugetlbLimit = append(resources.HugetlbLimit, &libcontainerconfigs.HugepageLimit{
			Pagesize: pageSize,
			Limit:    uint64(0),
		})
	}
}

// Update updates the cgroup with the specified Cgroup Configuration
func (m *cgroupCommon) Update(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("update").Observe(metrics.SinceInSeconds(start))
	}()

	libcontainerCgroupConfig := m.libctCgroupConfig(cgroupConfig, true)
	manager, err := libcontainercgroupmanager.New(libcontainerCgroupConfig)
	if err != nil {
		return fmt.Errorf("failed to create cgroup manager: %v", err)
	}
	return manager.Set(libcontainerCgroupConfig.Resources)
}

// Create creates the specified cgroup
func (m *cgroupCommon) Create(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("create").Observe(metrics.SinceInSeconds(start))
	}()

	libcontainerCgroupConfig := m.libctCgroupConfig(cgroupConfig, true)
	manager, err := libcontainercgroupmanager.New(libcontainerCgroupConfig)
	if err != nil {
		return err
	}

	// Apply(-1) is a hack to create the cgroup directories for each resource
	// subsystem. The function [cgroups.Manager.apply()] applies cgroup
	// configuration to the process with the specified pid.
	// It creates cgroup files for each subsystems and writes the pid
	// in the tasks file. We use the function to create all the required
	// cgroup files but not attach any "real" pid to the cgroup.
	if err := manager.Apply(-1); err != nil {
		return err
	}

	// it may confuse why we call set after we do apply, but the issue is that runc
	// follows a similar pattern.  it's needed to ensure cpu quota is set properly.
	if err := manager.Set(libcontainerCgroupConfig.Resources); err != nil {
		utilruntime.HandleError(fmt.Errorf("cgroup manager.Set failed: %w", err))
	}

	return nil
}

// Scans through all subsystems to find pids associated with specified cgroup.
func (m *cgroupCommon) Pids(name CgroupName) []int {
	// we need the driver specific name
	cgroupFsName := m.Name(name)

	// Get a list of processes that we need to kill
	pidsToKill := sets.New[int]()
	var pids []int
	for _, val := range m.subsystems.MountPoints {
		dir := path.Join(val, cgroupFsName)
		_, err := os.Stat(dir)
		if os.IsNotExist(err) {
			// The subsystem pod cgroup is already deleted
			// do nothing, continue
			continue
		}
		// Get a list of pids that are still charged to the pod's cgroup
		pids, err = getCgroupProcs(dir)
		if err != nil {
			continue
		}
		pidsToKill.Insert(pids...)

		// WalkFunc which is called for each file and directory in the pod cgroup dir
		visitor := func(path string, info os.FileInfo, err error) error {
			if err != nil {
				klog.V(4).InfoS("Cgroup manager encountered error scanning cgroup path", "path", path, "err", err)
				return filepath.SkipDir
			}
			if !info.IsDir() {
				return nil
			}
			pids, err = getCgroupProcs(path)
			if err != nil {
				klog.V(4).InfoS("Cgroup manager encountered error getting procs for cgroup path", "path", path, "err", err)
				return filepath.SkipDir
			}
			pidsToKill.Insert(pids...)
			return nil
		}
		// Walk through the pod cgroup directory to check if
		// container cgroups haven't been GCed yet. Get attached processes to
		// all such unwanted containers under the pod cgroup
		if err = filepath.Walk(dir, visitor); err != nil {
			klog.V(4).InfoS("Cgroup manager encountered error scanning pids for directory", "path", dir, "err", err)
		}
	}
	return sets.List(pidsToKill)
}

// ReduceCPULimits reduces the cgroup's cpu shares to the lowest possible value
func (m *cgroupCommon) ReduceCPULimits(cgroupName CgroupName) error {
	// Set lowest possible CpuShares value for the cgroup
	minimumCPUShares := uint64(MinShares)
	resources := &ResourceConfig{
		CPUShares: &minimumCPUShares,
	}
	containerConfig := &CgroupConfig{
		Name:               cgroupName,
		ResourceParameters: resources,
	}
	return m.Update(containerConfig)
}

func readCgroupMemoryConfig(cgroupPath string, memLimitFile string) (*ResourceConfig, error) {
	memLimit, err := fscommon.GetCgroupParamUint(cgroupPath, memLimitFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s for cgroup %v: %v", memLimitFile, cgroupPath, err)
	}
	mLim := int64(memLimit)
	//TODO(vinaykul,InPlacePodVerticalScaling): Add memory request support
	return &ResourceConfig{Memory: &mLim}, nil

}
