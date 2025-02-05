//go:build windows
// +build windows

/*
Copyright 2019 The Kubernetes Authors.

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
	"context"
	"fmt"
	"time"

	"github.com/Microsoft/hnslib"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// windowsNetworkStatsProvider creates an interface that allows for testing the logic without needing to create a container
type windowsNetworkStatsProvider interface {
	HNSListEndpointRequest() ([]hnslib.HNSEndpoint, error)
	GetHNSEndpointStats(endpointName string) (*hnslib.HNSEndpointStats, error)
}

// networkStats exposes the required functionality for hnslib in this scenario
type networkStats struct{}

func (s networkStats) HNSListEndpointRequest() ([]hnslib.HNSEndpoint, error) {
	return hnslib.HNSListEndpointRequest()
}

func (s networkStats) GetHNSEndpointStats(endpointName string) (*hnslib.HNSEndpointStats, error) {
	return hnslib.GetHNSEndpointStats(endpointName)
}

// listContainerNetworkStats returns the network stats of all the running containers.
func (p *criStatsProvider) listContainerNetworkStats() (map[string]*statsapi.NetworkStats, error) {
	networkStatsProvider := newNetworkStatsProvider(p)

	endpoints, err := networkStatsProvider.HNSListEndpointRequest()
	if err != nil {
		klog.ErrorS(err, "Failed to fetch current HNS endpoints")
		return nil, err
	}

	networkStats := make(map[string]*statsapi.NetworkStats)
	for _, endpoint := range endpoints {
		endpointStats, err := networkStatsProvider.GetHNSEndpointStats(endpoint.Id)
		if err != nil {
			klog.V(2).InfoS("Failed to fetch statistics for endpoint, continue to get stats for other endpoints", "endpointId", endpoint.Id, "containers", endpoint.SharedContainers)
			continue
		}

		// only add the interface for each container if not already in the list
		for _, cId := range endpoint.SharedContainers {
			networkStat, found := networkStats[cId]
			if found && networkStat.Name != endpoint.Name {
				iStat := hcsStatToInterfaceStat(endpointStats, endpoint.Name)
				networkStat.Interfaces = append(networkStat.Interfaces, iStat)
				continue
			}
			networkStats[cId] = hcsStatsToNetworkStats(p.clock.Now(), endpointStats, endpoint.Name)
		}
	}

	return networkStats, nil
}

func (p *criStatsProvider) addCRIPodContainerStats(ctx context.Context, criSandboxStat *runtimeapi.PodSandboxStats,
	ps *statsapi.PodStats, fsIDtoInfo map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo,
	containerMap map[string]*runtimeapi.Container,
	podSandbox *runtimeapi.PodSandbox,
	rootFsInfo *cadvisorapiv2.FsInfo,
	updateCPUNanoCoreUsage bool) error {
	for _, criContainerStat := range criSandboxStat.GetWindows().GetContainers() {
		container, found := containerMap[criContainerStat.Attributes.Id]
		if !found {
			continue
		}
		// Fill available stats for full set of required pod stats
		cs, err := p.makeWinContainerStats(criContainerStat, container, rootFsInfo, fsIDtoInfo, podSandbox.GetMetadata())
		if err != nil {
			return fmt.Errorf("make container stats: %w", err)

		}
		ps.Containers = append(ps.Containers, *cs)
	}

	return nil
}

func (p *criStatsProvider) makeWinContainerStats(
	stats *runtimeapi.WindowsContainerStats,
	container *runtimeapi.Container,
	rootFsInfo *cadvisorapiv2.FsInfo,
	fsIDtoInfo map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo,
	meta *runtimeapi.PodSandboxMetadata) (*statsapi.ContainerStats, error) {
	result := &statsapi.ContainerStats{
		Name: stats.Attributes.Metadata.Name,
		// The StartTime in the summary API is the container creation time.
		StartTime: metav1.NewTime(time.Unix(0, container.CreatedAt)),
		CPU:       &statsapi.CPUStats{},
		Memory:    &statsapi.MemoryStats{},
		Rootfs:    &statsapi.FsStats{},
		// UserDefinedMetrics is not supported by CRI.
	}
	if stats.Cpu != nil {
		result.CPU.Time = metav1.NewTime(time.Unix(0, stats.Cpu.Timestamp))
		if stats.Cpu.UsageCoreNanoSeconds != nil {
			result.CPU.UsageCoreNanoSeconds = &stats.Cpu.UsageCoreNanoSeconds.Value
		}
		if stats.Cpu.UsageNanoCores != nil {
			result.CPU.UsageNanoCores = &stats.Cpu.UsageNanoCores.Value
		}
	} else {
		result.CPU.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.CPU.UsageCoreNanoSeconds = uint64Ptr(0)
		result.CPU.UsageNanoCores = uint64Ptr(0)
	}
	if stats.Memory != nil {
		result.Memory.Time = metav1.NewTime(time.Unix(0, stats.Memory.Timestamp))
		if stats.Memory.WorkingSetBytes != nil {
			result.Memory.WorkingSetBytes = &stats.Memory.WorkingSetBytes.Value
		}
		if stats.Memory.AvailableBytes != nil {
			result.Memory.AvailableBytes = &stats.Memory.AvailableBytes.Value
		}
		if stats.Memory.PageFaults != nil {
			result.Memory.PageFaults = &stats.Memory.PageFaults.Value
		}
	} else {
		result.Memory.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.Memory.WorkingSetBytes = uint64Ptr(0)
		result.Memory.AvailableBytes = uint64Ptr(0)
		result.Memory.PageFaults = uint64Ptr(0)
	}
	if stats.WritableLayer != nil {
		result.Rootfs.Time = metav1.NewTime(time.Unix(0, stats.WritableLayer.Timestamp))
		if stats.WritableLayer.UsedBytes != nil {
			result.Rootfs.UsedBytes = &stats.WritableLayer.UsedBytes.Value
		}
	}
	var err error
	fsID := stats.GetWritableLayer().GetFsId()
	if fsID != nil {
		imageFsInfo, found := fsIDtoInfo[*fsID]
		if !found {
			imageFsInfo, err = p.getFsInfo(fsID)
			if err != nil {
				return nil, fmt.Errorf("get filesystem info: %w", err)
			}
			fsIDtoInfo[*fsID] = imageFsInfo
		}
		if imageFsInfo != nil {
			// The image filesystem id is unknown to the local node or there's
			// an error on retrieving the stats. In these cases, we omit those stats
			// and return the best-effort partial result. See
			// https://github.com/kubernetes/heapster/issues/1793.
			result.Rootfs.AvailableBytes = &imageFsInfo.Available
			result.Rootfs.CapacityBytes = &imageFsInfo.Capacity
		}
	}
	// NOTE: This doesn't support the old pod log path, `/var/log/pods/UID`. For containers
	// using old log path, empty log stats are returned. This is fine, because we don't
	// officially support in-place upgrade anyway.
	result.Logs, err = p.hostStatsProvider.getPodContainerLogStats(meta.GetNamespace(), meta.GetName(), types.UID(meta.GetUid()), container.GetMetadata().GetName(), rootFsInfo)
	if err != nil {
		klog.ErrorS(err, "Unable to fetch container log stats", "containerName", container.GetMetadata().GetName())
	}
	return result, nil
}

// hcsStatsToNetworkStats converts hnslib.Statistics.Network to statsapi.NetworkStats
func hcsStatsToNetworkStats(timestamp time.Time, hcsStats *hnslib.HNSEndpointStats, endpointName string) *statsapi.NetworkStats {
	result := &statsapi.NetworkStats{
		Time:       metav1.NewTime(timestamp),
		Interfaces: make([]statsapi.InterfaceStats, 0),
	}

	iStat := hcsStatToInterfaceStat(hcsStats, endpointName)

	// TODO: add support of multiple interfaces for getting default interface.
	result.Interfaces = append(result.Interfaces, iStat)
	result.InterfaceStats = iStat

	return result
}

func hcsStatToInterfaceStat(hcsStats *hnslib.HNSEndpointStats, endpointName string) statsapi.InterfaceStats {
	iStat := statsapi.InterfaceStats{
		Name:    endpointName,
		RxBytes: &hcsStats.BytesReceived,
		TxBytes: &hcsStats.BytesSent,
	}
	return iStat
}

func addCRIPodCPUStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Windows == nil || criPodStat.Windows.Cpu == nil {
		return
	}
	criCPU := criPodStat.Windows.Cpu
	ps.CPU = &statsapi.CPUStats{
		Time:                 metav1.NewTime(time.Unix(0, criCPU.Timestamp)),
		UsageNanoCores:       valueOfUInt64Value(criCPU.UsageNanoCores),
		UsageCoreNanoSeconds: valueOfUInt64Value(criCPU.UsageCoreNanoSeconds),
	}
}

func addCRIPodMemoryStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Windows == nil || criPodStat.Windows.Memory == nil {
		return
	}
	criMemory := criPodStat.Windows.Memory
	ps.Memory = &statsapi.MemoryStats{
		Time:            metav1.NewTime(time.Unix(0, criMemory.Timestamp)),
		AvailableBytes:  valueOfUInt64Value(criMemory.AvailableBytes),
		WorkingSetBytes: valueOfUInt64Value(criMemory.WorkingSetBytes),
		PageFaults:      valueOfUInt64Value(criMemory.PageFaults),
	}
}

func addCRIPodProcessStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Windows == nil || criPodStat.Windows.Process == nil {
		return
	}
	ps.ProcessStats = &statsapi.ProcessStats{
		ProcessCount: valueOfUInt64Value(criPodStat.Windows.Process.ProcessCount),
	}
}

func addCRIPodNetworkStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Windows == nil || criPodStat.Windows.Network == nil {
		return
	}
	criNetwork := criPodStat.Windows.Network
	iStats := statsapi.NetworkStats{
		Time:           metav1.NewTime(time.Unix(0, criNetwork.Timestamp)),
		InterfaceStats: criInterfaceToWinSummary(criNetwork.DefaultInterface),
		Interfaces:     make([]statsapi.InterfaceStats, 0, len(criNetwork.Interfaces)),
	}
	for _, iface := range criNetwork.Interfaces {
		iStats.Interfaces = append(iStats.Interfaces, criInterfaceToWinSummary(iface))
	}
	ps.Network = &iStats
}

func criInterfaceToWinSummary(criIface *runtimeapi.WindowsNetworkInterfaceUsage) statsapi.InterfaceStats {
	return statsapi.InterfaceStats{
		Name:    criIface.Name,
		RxBytes: valueOfUInt64Value(criIface.RxBytes),
		TxBytes: valueOfUInt64Value(criIface.TxBytes),
	}
}

// newNetworkStatsProvider uses the real windows hnslib if not provided otherwise if the interface is provided
// by the cristatsprovider in testing scenarios it uses that one
func newNetworkStatsProvider(p *criStatsProvider) windowsNetworkStatsProvider {
	var statsProvider windowsNetworkStatsProvider
	if p.windowsNetworkStatsProvider == nil {
		statsProvider = networkStats{}
	} else {
		statsProvider = p.windowsNetworkStatsProvider.(windowsNetworkStatsProvider)
	}
	return statsProvider
}
