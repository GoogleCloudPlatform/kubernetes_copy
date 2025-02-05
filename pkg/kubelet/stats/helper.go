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

package stats

import (
	"fmt"
	"time"

	cadvisorapiv1 "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
)

// defaultNetworkInterfaceName is used for collectng network stats.
// This logic relies on knowledge of the container runtime implementation and
// is not reliable.
const defaultNetworkInterfaceName = "eth0"

func cadvisorInfoToCPUandMemoryStats(info *cadvisorapiv2.ContainerInfo) (*statsapi.CPUStats, *statsapi.MemoryStats) {
	cstat, found := latestContainerStats(info)
	if !found {
		return nil, nil
	}
	var cpuStats *statsapi.CPUStats
	var memoryStats *statsapi.MemoryStats
	cpuStats = &statsapi.CPUStats{
		Time:                 metav1.NewTime(cstat.Timestamp),
		UsageNanoCores:       uint64Ptr(0),
		UsageCoreNanoSeconds: uint64Ptr(0),
	}
	if info.Spec.HasCpu {
		if cstat.CpuInst != nil {
			cpuStats.UsageNanoCores = &cstat.CpuInst.Usage.Total
		}
		if cstat.Cpu != nil {
			cpuStats.UsageCoreNanoSeconds = &cstat.Cpu.Usage.Total
		}
	}
	if info.Spec.HasMemory && cstat.Memory != nil {
		pageFaults := cstat.Memory.ContainerData.Pgfault
		majorPageFaults := cstat.Memory.ContainerData.Pgmajfault
		memoryStats = &statsapi.MemoryStats{
			Time:            metav1.NewTime(cstat.Timestamp),
			UsageBytes:      &cstat.Memory.Usage,
			WorkingSetBytes: &cstat.Memory.WorkingSet,
			RSSBytes:        &cstat.Memory.RSS,
			PageFaults:      &pageFaults,
			MajorPageFaults: &majorPageFaults,
		}
		// availableBytes = memory limit (if known) - workingset
		if !isMemoryUnlimited(info.Spec.Memory.Limit) {
			availableBytes := info.Spec.Memory.Limit - cstat.Memory.WorkingSet
			memoryStats.AvailableBytes = &availableBytes
		}
	} else {
		memoryStats = &statsapi.MemoryStats{
			Time:            metav1.NewTime(cstat.Timestamp),
			WorkingSetBytes: uint64Ptr(0),
		}
	}
	return cpuStats, memoryStats
}

// cadvisorInfoToContainerStats returns the statsapi.ContainerStats converted
// from the container and filesystem info.
func cadvisorInfoToContainerStats(name string, info *cadvisorapiv2.ContainerInfo, rootFs, imageFs *cadvisorapiv2.FsInfo) *statsapi.ContainerStats {
	result := &statsapi.ContainerStats{
		StartTime: metav1.NewTime(info.Spec.CreationTime),
		Name:      name,
	}
	cstat, found := latestContainerStats(info)
	if !found {
		return result
	}

	cpu, memory := cadvisorInfoToCPUandMemoryStats(info)
	result.CPU = cpu
	result.Memory = memory
	result.Swap = cadvisorInfoToSwapStats(info)

	// NOTE: if they can be found, log stats will be overwritten
	// by the caller, as it knows more information about the pod,
	// which is needed to determine log size.
	if rootFs != nil {
		// The container logs live on the node rootfs device
		result.Logs = buildLogsStats(cstat, rootFs)
	}

	if imageFs != nil {
		// The container rootFs lives on the imageFs devices (which may not be the node root fs)
		result.Rootfs = buildRootfsStats(cstat, imageFs)
	}

	cfs := cstat.Filesystem
	if cfs != nil {
		if cfs.BaseUsageBytes != nil {
			if result.Rootfs != nil {
				rootfsUsage := *cfs.BaseUsageBytes
				result.Rootfs.UsedBytes = &rootfsUsage
			}
			if cfs.TotalUsageBytes != nil && result.Logs != nil {
				logsUsage := *cfs.TotalUsageBytes - *cfs.BaseUsageBytes
				result.Logs.UsedBytes = &logsUsage
			}
		}
		if cfs.InodeUsage != nil && result.Rootfs != nil {
			rootInodes := *cfs.InodeUsage
			result.Rootfs.InodesUsed = &rootInodes
		}
	}

	for _, acc := range cstat.Accelerators {
		result.Accelerators = append(result.Accelerators, statsapi.AcceleratorStats{
			Make:        acc.Make,
			Model:       acc.Model,
			ID:          acc.ID,
			MemoryTotal: acc.MemoryTotal,
			MemoryUsed:  acc.MemoryUsed,
			DutyCycle:   acc.DutyCycle,
		})
	}

	result.UserDefinedMetrics = cadvisorInfoToUserDefinedMetrics(info)

	return result
}

// cadvisorInfoToContainerCPUAndMemoryStats returns the statsapi.ContainerStats converted
// from the container and filesystem info.
func cadvisorInfoToContainerCPUAndMemoryStats(name string, info *cadvisorapiv2.ContainerInfo) *statsapi.ContainerStats {
	result := &statsapi.ContainerStats{
		StartTime: metav1.NewTime(info.Spec.CreationTime),
		Name:      name,
	}

	cpu, memory := cadvisorInfoToCPUandMemoryStats(info)
	result.CPU = cpu
	result.Memory = memory
	result.Swap = cadvisorInfoToSwapStats(info)

	return result
}

func cadvisorInfoToProcessStats(info *cadvisorapiv2.ContainerInfo) *statsapi.ProcessStats {
	cstat, found := latestContainerStats(info)
	if !found || cstat.Processes == nil {
		return nil
	}
	num := cstat.Processes.ProcessCount
	return &statsapi.ProcessStats{ProcessCount: uint64Ptr(num)}
}

func mergeProcessStats(first *statsapi.ProcessStats, second *statsapi.ProcessStats) *statsapi.ProcessStats {
	if first == nil && second == nil {
		return nil
	}

	if first == nil {
		return second
	}
	if second == nil {
		return first
	}

	firstProcessCount := uint64(0)
	if first.ProcessCount != nil {
		firstProcessCount = *first.ProcessCount
	}

	secondProcessCount := uint64(0)
	if second.ProcessCount != nil {
		secondProcessCount = *second.ProcessCount
	}

	return &statsapi.ProcessStats{ProcessCount: uint64Ptr(firstProcessCount + secondProcessCount)}
}

// cadvisorInfoToNetworkStats returns the statsapi.NetworkStats converted from
// the container info from cadvisor.
func cadvisorInfoToNetworkStats(info *cadvisorapiv2.ContainerInfo) *statsapi.NetworkStats {
	if !info.Spec.HasNetwork {
		return nil
	}
	cstat, found := latestContainerStats(info)
	if !found {
		return nil
	}

	if cstat.Network == nil {
		return nil
	}

	iStats := statsapi.NetworkStats{
		Time: metav1.NewTime(cstat.Timestamp),
	}

	for i := range cstat.Network.Interfaces {
		inter := cstat.Network.Interfaces[i]
		iStat := statsapi.InterfaceStats{
			Name:     inter.Name,
			RxBytes:  &inter.RxBytes,
			RxErrors: &inter.RxErrors,
			TxBytes:  &inter.TxBytes,
			TxErrors: &inter.TxErrors,
		}

		if inter.Name == defaultNetworkInterfaceName {
			iStats.InterfaceStats = iStat
		}

		iStats.Interfaces = append(iStats.Interfaces, iStat)
	}

	return &iStats
}

// cadvisorInfoToUserDefinedMetrics returns the statsapi.UserDefinedMetric
// converted from the container info from cadvisor.
func cadvisorInfoToUserDefinedMetrics(info *cadvisorapiv2.ContainerInfo) []statsapi.UserDefinedMetric {
	type specVal struct {
		ref     statsapi.UserDefinedMetricDescriptor
		valType cadvisorapiv1.DataType
		time    time.Time
		value   float64
	}
	udmMap := map[string]*specVal{}
	for _, spec := range info.Spec.CustomMetrics {
		udmMap[spec.Name] = &specVal{
			ref: statsapi.UserDefinedMetricDescriptor{
				Name:  spec.Name,
				Type:  statsapi.UserDefinedMetricType(spec.Type),
				Units: spec.Units,
			},
			valType: spec.Format,
		}
	}
	for _, stat := range info.Stats {
		for name, values := range stat.CustomMetrics {
			specVal, ok := udmMap[name]
			if !ok {
				klog.InfoS("Spec for custom metric is missing from cAdvisor output", "metric", name, "spec", info.Spec, "metrics", stat.CustomMetrics)
				continue
			}
			for _, value := range values {
				// Pick the most recent value
				if value.Timestamp.Before(specVal.time) {
					continue
				}
				specVal.time = value.Timestamp
				specVal.value = value.FloatValue
				if specVal.valType == cadvisorapiv1.IntType {
					specVal.value = float64(value.IntValue)
				}
			}
		}
	}
	var udm []statsapi.UserDefinedMetric
	for _, specVal := range udmMap {
		udm = append(udm, statsapi.UserDefinedMetric{
			UserDefinedMetricDescriptor: specVal.ref,
			Time:                        metav1.NewTime(specVal.time),
			Value:                       specVal.value,
		})
	}
	return udm
}

func cadvisorInfoToSwapStats(info *cadvisorapiv2.ContainerInfo) *statsapi.SwapStats {
	cstat, found := latestContainerStats(info)
	if !found {
		return nil
	}

	var swapStats *statsapi.SwapStats

	if info.Spec.HasMemory && cstat.Memory != nil {
		swapStats = &statsapi.SwapStats{
			Time:           metav1.NewTime(cstat.Timestamp),
			SwapUsageBytes: &cstat.Memory.Swap,
		}

		if !isMemoryUnlimited(info.Spec.Memory.SwapLimit) {
			swapAvailableBytes := info.Spec.Memory.SwapLimit - cstat.Memory.Swap
			swapStats.SwapAvailableBytes = &swapAvailableBytes
		}
	}

	return swapStats
}

// latestContainerStats returns the latest container stats from cadvisor, or nil if none exist
func latestContainerStats(info *cadvisorapiv2.ContainerInfo) (*cadvisorapiv2.ContainerStats, bool) {
	stats := info.Stats
	if len(stats) < 1 {
		return nil, false
	}
	latest := stats[len(stats)-1]
	if latest == nil {
		return nil, false
	}
	return latest, true
}

func isMemoryUnlimited(v uint64) bool {
	// Size after which we consider memory to be "unlimited". This is not
	// MaxInt64 due to rounding by the kernel.
	// TODO: cadvisor should export this https://github.com/google/cadvisor/blob/master/metrics/prometheus.go#L596
	const maxMemorySize = uint64(1 << 62)

	return v > maxMemorySize
}

// getCgroupInfo returns the information of the container with the specified
// containerName from cadvisor.
func getCgroupInfo(cadvisor cadvisor.Interface, containerName string, updateStats bool) (*cadvisorapiv2.ContainerInfo, error) {
	var maxAge *time.Duration
	if updateStats {
		age := 0 * time.Second
		maxAge = &age
	}
	infoMap, err := cadvisor.ContainerInfoV2(containerName, cadvisorapiv2.RequestOptions{
		IdType:    cadvisorapiv2.TypeName,
		Count:     2, // 2 samples are needed to compute "instantaneous" CPU
		Recursive: false,
		MaxAge:    maxAge,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get container info for %q: %w", containerName, err)
	}
	if len(infoMap) != 1 {
		return nil, fmt.Errorf("unexpected number of containers: %v", len(infoMap))
	}
	info := infoMap[containerName]
	return &info, nil
}

// getCgroupStats returns the latest stats of the container having the
// specified containerName from cadvisor.
func getCgroupStats(cadvisor cadvisor.Interface, containerName string, updateStats bool) (*cadvisorapiv2.ContainerStats, error) {
	info, err := getCgroupInfo(cadvisor, containerName, updateStats)
	if err != nil {
		return nil, err
	}
	stats, found := latestContainerStats(info)
	if !found {
		return nil, fmt.Errorf("failed to get latest stats from container info for %q", containerName)
	}
	return stats, nil
}

func buildLogsStats(cstat *cadvisorapiv2.ContainerStats, rootFs *cadvisorapiv2.FsInfo) *statsapi.FsStats {
	fsStats := &statsapi.FsStats{
		Time:           metav1.NewTime(cstat.Timestamp),
		AvailableBytes: &rootFs.Available,
		CapacityBytes:  &rootFs.Capacity,
		InodesFree:     rootFs.InodesFree,
		Inodes:         rootFs.Inodes,
	}

	if rootFs.Inodes != nil && rootFs.InodesFree != nil {
		logsInodesUsed := *rootFs.Inodes - *rootFs.InodesFree
		fsStats.InodesUsed = &logsInodesUsed
	}
	return fsStats
}

func buildRootfsStats(cstat *cadvisorapiv2.ContainerStats, imageFs *cadvisorapiv2.FsInfo) *statsapi.FsStats {
	return &statsapi.FsStats{
		Time:           metav1.NewTime(cstat.Timestamp),
		AvailableBytes: &imageFs.Available,
		CapacityBytes:  &imageFs.Capacity,
		InodesFree:     imageFs.InodesFree,
		Inodes:         imageFs.Inodes,
	}
}

func getUint64Value(value *uint64) uint64 {
	if value == nil {
		return 0
	}

	return *value
}

func uint64Ptr(i uint64) *uint64 {
	return &i
}

func calcEphemeralStorage(containers []statsapi.ContainerStats, volumes []statsapi.VolumeStats, rootFsInfo *cadvisorapiv2.FsInfo,
	podLogStats *statsapi.FsStats, etcHostsStats *statsapi.FsStats, isCRIStatsProvider bool) *statsapi.FsStats {
	result := &statsapi.FsStats{
		Time:           metav1.NewTime(rootFsInfo.Timestamp),
		AvailableBytes: &rootFsInfo.Available,
		CapacityBytes:  &rootFsInfo.Capacity,
		InodesFree:     rootFsInfo.InodesFree,
		Inodes:         rootFsInfo.Inodes,
	}
	for _, container := range containers {
		addContainerUsage(result, &container, isCRIStatsProvider)
	}
	for _, volume := range volumes {
		result.UsedBytes = addUsage(result.UsedBytes, volume.FsStats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, volume.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &volume.FsStats.Time)
	}
	if podLogStats != nil {
		result.UsedBytes = addUsage(result.UsedBytes, podLogStats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, podLogStats.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &podLogStats.Time)
	}
	if etcHostsStats != nil {
		result.UsedBytes = addUsage(result.UsedBytes, etcHostsStats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, etcHostsStats.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &etcHostsStats.Time)
	}
	return result
}

func addContainerUsage(stat *statsapi.FsStats, container *statsapi.ContainerStats, isCRIStatsProvider bool) {
	if rootFs := container.Rootfs; rootFs != nil {
		stat.Time = maxUpdateTime(&stat.Time, &rootFs.Time)
		stat.InodesUsed = addUsage(stat.InodesUsed, rootFs.InodesUsed)
		stat.UsedBytes = addUsage(stat.UsedBytes, rootFs.UsedBytes)
		if logs := container.Logs; logs != nil {
			stat.UsedBytes = addUsage(stat.UsedBytes, logs.UsedBytes)
			// We have accurate container log inode usage for CRI stats provider.
			if isCRIStatsProvider {
				stat.InodesUsed = addUsage(stat.InodesUsed, logs.InodesUsed)
			}
			stat.Time = maxUpdateTime(&stat.Time, &logs.Time)
		}
	}
}

func maxUpdateTime(first, second *metav1.Time) metav1.Time {
	if first.Before(second) {
		return *second
	}
	return *first
}

func addUsage(first, second *uint64) *uint64 {
	if first == nil {
		return second
	} else if second == nil {
		return first
	}
	total := *first + *second
	return &total
}

func makePodStorageStats(s *statsapi.PodStats, terminatedContainerStats []statsapi.ContainerStats, rootFsInfo *cadvisorapiv2.FsInfo, resourceAnalyzer stats.ResourceAnalyzer, hostStatsProvider HostStatsProvider, isCRIStatsProvider bool) {
	podNs := s.PodRef.Namespace
	podName := s.PodRef.Name
	podUID := types.UID(s.PodRef.UID)
	var ephemeralStats []statsapi.VolumeStats
	if vstats, found := resourceAnalyzer.GetPodVolumeStats(podUID); found {
		ephemeralStats = make([]statsapi.VolumeStats, len(vstats.EphemeralVolumes))
		copy(ephemeralStats, vstats.EphemeralVolumes)
		s.VolumeStats = append(append([]statsapi.VolumeStats{}, vstats.EphemeralVolumes...), vstats.PersistentVolumes...)

	}
	logStats, err := hostStatsProvider.getPodLogStats(podNs, podName, podUID, rootFsInfo)
	if err != nil {
		klog.V(6).ErrorS(err, "Unable to fetch pod log stats", "pod", klog.KRef(podNs, podName))
		// If people do in-place upgrade, there might be pods still using
		// the old log path. For those pods, no pod log stats is returned.
		// We should continue generating other stats in that case.
		// calcEphemeralStorage tolerants logStats == nil.
	}
	etcHostsStats, err := hostStatsProvider.getPodEtcHostsStats(podUID, rootFsInfo)
	if err != nil {
		klog.V(6).ErrorS(err, "Unable to fetch pod etc hosts stats", "pod", klog.KRef(podNs, podName))
	}
	if terminatedContainerStats != nil {
		s.EphemeralStorage = calcEphemeralStorage(append(s.Containers, terminatedContainerStats...), ephemeralStats, rootFsInfo, logStats, etcHostsStats, isCRIStatsProvider)
	} else {
		s.EphemeralStorage = calcEphemeralStorage(s.Containers, ephemeralStats, rootFsInfo, logStats, etcHostsStats, isCRIStatsProvider)
	}
}
