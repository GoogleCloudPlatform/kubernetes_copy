//go:build linux
// +build linux

/*
Copyright 2023 The Kubernetes Authors.

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

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

func (p *criStatsProvider) addCRIPodContainerStats(ctx context.Context, criSandboxStat *runtimeapi.PodSandboxStats,
	ps *statsapi.PodStats, fsIDtoInfo map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo,
	containerMap map[string]*runtimeapi.Container,
	podSandbox *runtimeapi.PodSandbox,
	rootFsInfo *cadvisorapiv2.FsInfo, updateCPUNanoCoreUsage bool) error {
	for _, criContainerStat := range criSandboxStat.Linux.Containers {
		container, found := containerMap[criContainerStat.Attributes.Id]
		if !found {
			continue
		}
		// Fill available stats for full set of required pod stats
		cs, err := p.makeContainerStats(ctx, criContainerStat, container, rootFsInfo, fsIDtoInfo, podSandbox.GetMetadata(),
			updateCPUNanoCoreUsage)
		if err != nil {
			return fmt.Errorf("make container stats: %w", err)
		}
		ps.Containers = append(ps.Containers, *cs)
	}
	return nil
}

func addCRIPodNetworkStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Linux == nil || criPodStat.Linux.Network == nil {
		return
	}
	criNetwork := criPodStat.Linux.Network
	iStats := statsapi.NetworkStats{
		Time:           metav1.NewTime(time.Unix(0, criNetwork.Timestamp)),
		InterfaceStats: criInterfaceToSummary(criNetwork.DefaultInterface),
		Interfaces:     make([]statsapi.InterfaceStats, 0, len(criNetwork.Interfaces)),
	}
	for _, iface := range criNetwork.Interfaces {
		iStats.Interfaces = append(iStats.Interfaces, criInterfaceToSummary(iface))
	}
	ps.Network = &iStats
}

func addCRIPodMemoryStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Linux == nil || criPodStat.Linux.Memory == nil {
		return
	}
	criMemory := criPodStat.Linux.Memory
	ps.Memory = &statsapi.MemoryStats{
		Time:            metav1.NewTime(time.Unix(0, criMemory.Timestamp)),
		AvailableBytes:  valueOfUInt64Value(criMemory.AvailableBytes),
		UsageBytes:      valueOfUInt64Value(criMemory.UsageBytes),
		WorkingSetBytes: valueOfUInt64Value(criMemory.WorkingSetBytes),
		RSSBytes:        valueOfUInt64Value(criMemory.RssBytes),
		PageFaults:      valueOfUInt64Value(criMemory.PageFaults),
		MajorPageFaults: valueOfUInt64Value(criMemory.MajorPageFaults),
	}
}

func addCRIPodCPUStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Linux == nil || criPodStat.Linux.Cpu == nil {
		return
	}
	criCPU := criPodStat.Linux.Cpu
	ps.CPU = &statsapi.CPUStats{
		Time:                 metav1.NewTime(time.Unix(0, criCPU.Timestamp)),
		UsageNanoCores:       valueOfUInt64Value(criCPU.UsageNanoCores),
		UsageCoreNanoSeconds: valueOfUInt64Value(criCPU.UsageCoreNanoSeconds),
	}
}

func addCRIPodProcessStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
	if criPodStat == nil || criPodStat.Linux == nil || criPodStat.Linux.Process == nil {
		return
	}
	ps.ProcessStats = &statsapi.ProcessStats{
		ProcessCount: valueOfUInt64Value(criPodStat.Linux.Process.ProcessCount),
	}
}

// listContainerNetworkStats returns the network stats of all the running containers.
// It should return (nil, nil) for platforms other than Windows.
func (p *criStatsProvider) listContainerNetworkStats() (map[string]*statsapi.NetworkStats, error) {
	return nil, nil
}
