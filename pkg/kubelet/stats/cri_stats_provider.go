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
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	cadvisormemory "github.com/google/cadvisor/cache/memory"
	cadvisorfs "github.com/google/cadvisor/fs"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	crierror "k8s.io/cri-api/pkg/errors"
	"k8s.io/klog/v2"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubetypes "k8s.io/kubelet/pkg/types"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/utils/clock"
)

var (
	// defaultCachePeriod is the default cache period for each cpuUsage.
	defaultCachePeriod = 10 * time.Minute
)

// cpuUsageRecord holds the cpu usage stats and the calculated usageNanoCores.
type cpuUsageRecord struct {
	stats          *runtimeapi.CpuUsage
	usageNanoCores *uint64
}

// criStatsProvider implements the containerStatsProvider interface by getting
// the container stats from CRI.
type criStatsProvider struct {
	// cadvisor is used to get the node root filesystem's stats (such as the
	// capacity/available bytes/inodes) that will be populated in per container
	// filesystem stats.
	cadvisor cadvisor.Interface
	// resourceAnalyzer is used to get the volume stats of the pods.
	resourceAnalyzer stats.ResourceAnalyzer
	// runtimeService is used to get the status and stats of the pods and its
	// managed containers.
	runtimeService internalapi.RuntimeService
	// imageService is used to get the stats of the image filesystem.
	imageService internalapi.ImageManagerService
	// hostStatsProvider is used to get the status of the host filesystem consumed by pods.
	hostStatsProvider HostStatsProvider
	// windowsNetworkStatsProvider is used by kubelet to gather networking stats on Windows
	windowsNetworkStatsProvider interface{} //nolint:unused // U1000 We can't import hcsshim due to Build constraints in hcsshim
	// clock is used report current time
	clock clock.Clock

	// cpuUsageCache caches the cpu usage for containers.
	cpuUsageCache               map[string]*cpuUsageRecord
	mutex                       sync.RWMutex
	podAndContainerStatsFromCRI bool
}

// newCRIStatsProvider returns a containerStatsProvider implementation that
// provides container stats using CRI.
func newCRIStatsProvider(
	cadvisor cadvisor.Interface,
	resourceAnalyzer stats.ResourceAnalyzer,
	runtimeService internalapi.RuntimeService,
	imageService internalapi.ImageManagerService,
	hostStatsProvider HostStatsProvider,
	podAndContainerStatsFromCRI bool,
) containerStatsProvider {
	return &criStatsProvider{
		cadvisor:                    cadvisor,
		resourceAnalyzer:            resourceAnalyzer,
		runtimeService:              runtimeService,
		imageService:                imageService,
		hostStatsProvider:           hostStatsProvider,
		cpuUsageCache:               make(map[string]*cpuUsageRecord),
		podAndContainerStatsFromCRI: podAndContainerStatsFromCRI,
		clock:                       clock.RealClock{},
	}
}

// ListPodStats returns the stats of all the pod-managed containers.
func (p *criStatsProvider) ListPodStats(ctx context.Context) ([]statsapi.PodStats, error) {
	// Don't update CPU nano core usage.
	return p.listPodStats(ctx, false)
}

// ListPodStatsAndUpdateCPUNanoCoreUsage updates the cpu nano core usage for
// the containers and returns the stats for all the pod-managed containers.
// This is a workaround because CRI runtimes do not supply nano core usages,
// so this function calculate the difference between the current and the last
// (cached) cpu stats to calculate this metrics. The implementation assumes a
// single caller to periodically invoke this function to update the metrics. If
// there exist multiple callers, the period used to compute the cpu usage may
// vary and the usage could be incoherent (e.g., spiky). If no caller calls
// this function, the cpu usage will stay nil. Right now, eviction manager is
// the only caller, and it calls this function every 10s.
func (p *criStatsProvider) ListPodStatsAndUpdateCPUNanoCoreUsage(ctx context.Context) ([]statsapi.PodStats, error) {
	// Update CPU nano core usage.
	return p.listPodStats(ctx, true)
}

func (p *criStatsProvider) listPodStats(ctx context.Context, updateCPUNanoCoreUsage bool) ([]statsapi.PodStats, error) {
	// Gets node root filesystem information, which will be used to populate
	// the available and capacity bytes/inodes in container stats.
	rootFsInfo, err := p.cadvisor.RootFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs info: %v", err)
	}

	containerMap, podSandboxMap, err := p.getPodAndContainerMaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod or container map: %v", err)
	}

	if p.podAndContainerStatsFromCRI {
		result, err := p.listPodStatsStrictlyFromCRI(ctx, updateCPUNanoCoreUsage, containerMap, podSandboxMap, &rootFsInfo)
		if err == nil {
			// Call succeeded
			return result, nil
		}
		s, ok := status.FromError(err)
		// Legitimate failure, rather than the CRI implementation does not support ListPodSandboxStats.
		if !ok || s.Code() != codes.Unimplemented {
			return nil, err
		}
		// CRI implementation doesn't support ListPodSandboxStats, warn and fallback.
		klog.V(5).ErrorS(err,
			"CRI implementation must be updated to support ListPodSandboxStats if PodAndContainerStatsFromCRI feature gate is enabled. Falling back to populating with cAdvisor; this call will fail in the future.",
		)
	}
	return p.listPodStatsPartiallyFromCRI(ctx, updateCPUNanoCoreUsage, containerMap, podSandboxMap, &rootFsInfo)
}

func (p *criStatsProvider) listPodStatsPartiallyFromCRI(ctx context.Context, updateCPUNanoCoreUsage bool, containerMap map[string]*runtimeapi.Container, podSandboxMap map[string]*runtimeapi.PodSandbox, rootFsInfo *cadvisorapiv2.FsInfo) ([]statsapi.PodStats, error) {
	// fsIDtoInfo is a map from filesystem id to its stats. This will be used
	// as a cache to avoid querying cAdvisor for the filesystem stats with the
	// same filesystem id many times.
	fsIDtoInfo := make(map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo)

	// sandboxIDToPodStats is a temporary map from sandbox ID to its pod stats.
	sandboxIDToPodStats := make(map[string]*statsapi.PodStats)

	resp, err := p.runtimeService.ListContainerStats(ctx, &runtimeapi.ContainerStatsFilter{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all container stats: %v", err)
	}
	allInfos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch cadvisor stats: %v", err)
	}
	caInfos, allInfos := getCRICadvisorStats(allInfos)

	// get network stats for containers.
	// This is only used on Windows. For other platforms, (nil, nil) should be returned.
	containerNetworkStats, err := p.listContainerNetworkStats()
	if err != nil {
		return nil, fmt.Errorf("failed to list container network stats: %v", err)
	}

	for _, stats := range resp {
		containerID := stats.Attributes.Id
		container, found := containerMap[containerID]
		if !found {
			continue
		}

		podSandboxID := container.PodSandboxId
		podSandbox, found := podSandboxMap[podSandboxID]
		if !found {
			continue
		}

		// Creates the stats of the pod (if not created yet) which the
		// container belongs to.
		ps, found := sandboxIDToPodStats[podSandboxID]
		if !found {
			ps = buildPodStats(podSandbox)
			sandboxIDToPodStats[podSandboxID] = ps
		}

		// Fill available stats for full set of required pod stats
		cs, err := p.makeContainerStats(ctx, stats, container, rootFsInfo, fsIDtoInfo, podSandbox.GetMetadata(), updateCPUNanoCoreUsage)
		if err != nil {
			return nil, fmt.Errorf("make container stats: %w", err)
		}
		p.addPodNetworkStats(ps, podSandboxID, caInfos, cs, containerNetworkStats[podSandboxID])
		p.addPodCPUMemoryStats(ps, types.UID(podSandbox.Metadata.Uid), allInfos, cs)
		p.addSwapStats(ps, types.UID(podSandbox.Metadata.Uid), allInfos, cs)

		// If cadvisor stats is available for the container, use it to populate
		// container stats
		caStats, caFound := caInfos[containerID]
		if !caFound {
			klog.V(5).InfoS("Unable to find cadvisor stats for container", "containerID", containerID)
		} else {
			p.addCadvisorContainerStats(cs, &caStats)
			p.addProcessStats(ps, &caStats)
		}

		ps.Containers = append(ps.Containers, *cs)
	}
	// cleanup outdated caches.
	p.cleanupOutdatedCaches()

	result := make([]statsapi.PodStats, 0, len(sandboxIDToPodStats))
	for _, s := range sandboxIDToPodStats {
		makePodStorageStats(s, rootFsInfo, p.resourceAnalyzer, p.hostStatsProvider, true)
		result = append(result, *s)
	}
	return result, nil
}

func (p *criStatsProvider) listPodStatsStrictlyFromCRI(ctx context.Context, updateCPUNanoCoreUsage bool, containerMap map[string]*runtimeapi.Container, podSandboxMap map[string]*runtimeapi.PodSandbox, rootFsInfo *cadvisorapiv2.FsInfo) ([]statsapi.PodStats, error) {
	criSandboxStats, err := p.runtimeService.ListPodSandboxStats(ctx, &runtimeapi.PodSandboxStatsFilter{})
	if err != nil {
		return nil, err
	}

	fsIDtoInfo := make(map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo)
	summarySandboxStats := make([]statsapi.PodStats, 0, len(podSandboxMap))
	for _, criSandboxStat := range criSandboxStats {
		if criSandboxStat == nil || criSandboxStat.Attributes == nil {
			klog.V(5).InfoS("Unable to find CRI stats for sandbox")
			continue
		}
		podSandbox, found := podSandboxMap[criSandboxStat.Attributes.Id]
		if !found {
			continue
		}
		ps := buildPodStats(podSandbox)
		if err := p.addCRIPodContainerStats(ctx, criSandboxStat, ps, fsIDtoInfo, containerMap, podSandbox, rootFsInfo, updateCPUNanoCoreUsage); err != nil {
			return nil, fmt.Errorf("add CRI pod container stats: %w", err)
		}
		addCRIPodNetworkStats(ps, criSandboxStat)
		addCRIPodCPUStats(ps, criSandboxStat)
		addCRIPodMemoryStats(ps, criSandboxStat)
		addCRIPodProcessStats(ps, criSandboxStat)
		makePodStorageStats(ps, rootFsInfo, p.resourceAnalyzer, p.hostStatsProvider, true)
		summarySandboxStats = append(summarySandboxStats, *ps)
	}
	return summarySandboxStats, nil
}

// ListPodCPUAndMemoryStats returns the CPU and Memory stats of all the pod-managed containers.
func (p *criStatsProvider) ListPodCPUAndMemoryStats(ctx context.Context) ([]statsapi.PodStats, error) {
	// sandboxIDToPodStats is a temporary map from sandbox ID to its pod stats.
	sandboxIDToPodStats := make(map[string]*statsapi.PodStats)
	containerMap, podSandboxMap, err := p.getPodAndContainerMaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod or container map: %v", err)
	}

	result := make([]statsapi.PodStats, 0, len(podSandboxMap))
	if p.podAndContainerStatsFromCRI {
		criSandboxStats, err := p.runtimeService.ListPodSandboxStats(ctx, &runtimeapi.PodSandboxStatsFilter{})
		// Call succeeded
		if err == nil {
			for _, criSandboxStat := range criSandboxStats {
				podSandbox, found := podSandboxMap[criSandboxStat.Attributes.Id]
				if !found {
					continue
				}
				ps := buildPodStats(podSandbox)
				addCRIPodCPUStats(ps, criSandboxStat)
				addCRIPodMemoryStats(ps, criSandboxStat)
				result = append(result, *ps)
			}
			return result, err
		}
		// Call failed, why?
		s, ok := status.FromError(err)
		// Legitimate failure, rather than the CRI implementation does not support ListPodSandboxStats.
		if !ok || s.Code() != codes.Unimplemented {
			return nil, err
		}
		// CRI implementation doesn't support ListPodSandboxStats, warn and fallback.
		klog.ErrorS(err,
			"CRI implementation must be updated to support ListPodSandboxStats if PodAndContainerStatsFromCRI feature gate is enabled. Falling back to populating with cAdvisor; this call will fail in the future.",
		)
	}

	resp, err := p.runtimeService.ListContainerStats(ctx, &runtimeapi.ContainerStatsFilter{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all container stats: %v", err)
	}

	allInfos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch cadvisor stats: %v", err)
	}
	caInfos, allInfos := getCRICadvisorStats(allInfos)

	for _, stats := range resp {
		containerID := stats.Attributes.Id
		container, found := containerMap[containerID]
		if !found {
			continue
		}

		podSandboxID := container.PodSandboxId
		podSandbox, found := podSandboxMap[podSandboxID]
		if !found {
			continue
		}

		// Creates the stats of the pod (if not created yet) which the
		// container belongs to.
		ps, found := sandboxIDToPodStats[podSandboxID]
		if !found {
			ps = buildPodStats(podSandbox)
			sandboxIDToPodStats[podSandboxID] = ps
		}

		// Fill available CPU and memory stats for full set of required pod stats
		cs := p.makeContainerCPUAndMemoryStats(stats, container)
		p.addPodCPUMemoryStats(ps, types.UID(podSandbox.Metadata.Uid), allInfos, cs)

		// If cadvisor stats is available for the container, use it to populate
		// container stats
		caStats, caFound := caInfos[containerID]
		if !caFound {
			klog.V(4).InfoS("Unable to find cadvisor stats for container", "containerID", containerID)
		} else {
			p.addCadvisorContainerCPUAndMemoryStats(cs, &caStats)
		}
		ps.Containers = append(ps.Containers, *cs)
	}
	// cleanup outdated caches.
	p.cleanupOutdatedCaches()

	for _, s := range sandboxIDToPodStats {
		result = append(result, *s)
	}
	return result, nil
}

func (p *criStatsProvider) getPodAndContainerMaps(ctx context.Context) (map[string]*runtimeapi.Container, map[string]*runtimeapi.PodSandbox, error) {
	containers, err := p.runtimeService.ListContainers(ctx, &runtimeapi.ContainerFilter{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list all containers: %v", err)
	}

	// Creates pod sandbox map between the pod sandbox ID and the PodSandbox object.
	podSandboxMap := make(map[string]*runtimeapi.PodSandbox)
	podSandboxes, err := p.runtimeService.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list all pod sandboxes: %v", err)
	}
	podSandboxes = removeTerminatedPods(podSandboxes)
	for _, s := range podSandboxes {
		podSandboxMap[s.Id] = s
	}

	containers = removeTerminatedContainers(containers)
	// Creates container map between the container ID and the Container object.
	containerMap := make(map[string]*runtimeapi.Container)
	for _, c := range containers {
		containerMap[c.Id] = c
	}
	return containerMap, podSandboxMap, nil
}

// ImageFsStats returns the stats of the image filesystem.
func (p *criStatsProvider) ImageFsStats(ctx context.Context) (imageFsRet *statsapi.FsStats, containerFsRet *statsapi.FsStats, errRet error) {
	resp, err := p.imageService.ImageFsInfo(ctx)
	if err != nil {
		return nil, nil, err
	}

	// CRI may return the stats of multiple image filesystems but we only
	// return the first one.
	//
	// TODO(yguo0905): Support returning stats of multiple image filesystems.
	if len(resp.GetImageFilesystems()) == 0 {
		return nil, nil, fmt.Errorf("imageFs information is unavailable")
	}
	fs := resp.GetImageFilesystems()[0]
	imageFsRet = &statsapi.FsStats{
		Time:      metav1.NewTime(time.Unix(0, fs.Timestamp)),
		UsedBytes: &fs.UsedBytes.Value,
	}
	if fs.InodesUsed != nil {
		imageFsRet.InodesUsed = &fs.InodesUsed.Value
	}
	imageFsInfo, err := p.getFsInfo(fs.GetFsId())
	if err != nil {
		return nil, nil, fmt.Errorf("get filesystem info: %w", err)
	}
	if imageFsInfo != nil {
		// The image filesystem id is unknown to the local node or there's
		// an error on retrieving the stats. In these cases, we omit those
		// stats and return the best-effort partial result. See
		// https://github.com/kubernetes/heapster/issues/1793.
		imageFsRet.AvailableBytes = &imageFsInfo.Available
		imageFsRet.CapacityBytes = &imageFsInfo.Capacity
		imageFsRet.InodesFree = imageFsInfo.InodesFree
		imageFsRet.Inodes = imageFsInfo.Inodes
	}
	// TODO: For CRI Stats Provider we don't support separate disks yet.
	return imageFsRet, imageFsRet, nil
}

// ImageFsDevice returns name of the device where the image filesystem locates,
// e.g. /dev/sda1.
func (p *criStatsProvider) ImageFsDevice(ctx context.Context) (string, error) {
	resp, err := p.imageService.ImageFsInfo(ctx)
	if err != nil {
		return "", err
	}
	for _, fs := range resp.GetImageFilesystems() {
		fsInfo, err := p.getFsInfo(fs.GetFsId())
		if err != nil {
			return "", fmt.Errorf("get filesystem info: %w", err)
		}
		if fsInfo != nil {
			return fsInfo.Device, nil
		}
	}
	return "", errors.New("imagefs device is not found")
}

// getFsInfo returns the information of the filesystem with the specified
// fsID. If any error occurs, this function logs the error and returns
// nil.
func (p *criStatsProvider) getFsInfo(fsID *runtimeapi.FilesystemIdentifier) (*cadvisorapiv2.FsInfo, error) {
	if fsID == nil {
		klog.V(2).InfoS("Failed to get filesystem info: fsID is nil")
		return nil, nil
	}
	mountpoint := fsID.GetMountpoint()
	fsInfo, err := p.cadvisor.GetDirFsInfo(mountpoint)
	if err != nil {
		msg := "Failed to get the info of the filesystem with mountpoint"
		if errors.Is(err, cadvisorfs.ErrNoSuchDevice) ||
			errors.Is(err, cadvisorfs.ErrDeviceNotInPartitionsMap) ||
			errors.Is(err, cadvisormemory.ErrDataNotFound) {
			klog.V(2).InfoS(msg, "mountpoint", mountpoint, "err", err)
		} else {
			klog.ErrorS(err, msg, "mountpoint", mountpoint)
			return nil, fmt.Errorf("%s: %w", msg, err)
		}
		return nil, nil
	}
	return &fsInfo, nil
}

// buildPodStats returns a PodStats that identifies the Pod managing cinfo
func buildPodStats(podSandbox *runtimeapi.PodSandbox) *statsapi.PodStats {
	return &statsapi.PodStats{
		PodRef: statsapi.PodReference{
			Name:      podSandbox.Metadata.Name,
			UID:       podSandbox.Metadata.Uid,
			Namespace: podSandbox.Metadata.Namespace,
		},
		// The StartTime in the summary API is the pod creation time.
		StartTime: metav1.NewTime(time.Unix(0, podSandbox.CreatedAt)),
	}
}

func (p *criStatsProvider) addPodNetworkStats(
	ps *statsapi.PodStats,
	podSandboxID string,
	caInfos map[string]cadvisorapiv2.ContainerInfo,
	cs *statsapi.ContainerStats,
	netStats *statsapi.NetworkStats,
) {
	caPodSandbox, found := caInfos[podSandboxID]
	// try get network stats from cadvisor first.
	if found {
		networkStats := cadvisorInfoToNetworkStats(&caPodSandbox)
		if networkStats != nil {
			ps.Network = networkStats
			return
		}
	}

	// Not found from cadvisor, get from netStats.
	if netStats != nil {
		ps.Network = netStats
		return
	}

	// TODO: sum Pod network stats from container stats.
	klog.V(4).InfoS("Unable to find network stats for sandbox", "sandboxID", podSandboxID)
}

func (p *criStatsProvider) addPodCPUMemoryStats(
	ps *statsapi.PodStats,
	podUID types.UID,
	allInfos map[string]cadvisorapiv2.ContainerInfo,
	cs *statsapi.ContainerStats,
) {
	// try get cpu and memory stats from cadvisor first.
	podCgroupInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
	if podCgroupInfo != nil {
		cpu, memory := cadvisorInfoToCPUandMemoryStats(podCgroupInfo)
		ps.CPU = cpu
		ps.Memory = memory
		return
	}

	// Sum Pod cpu and memory stats from containers stats.
	if cs.CPU != nil {
		if ps.CPU == nil {
			ps.CPU = &statsapi.CPUStats{}
		}

		ps.CPU.Time = cs.CPU.Time
		usageCoreNanoSeconds := getUint64Value(cs.CPU.UsageCoreNanoSeconds) + getUint64Value(ps.CPU.UsageCoreNanoSeconds)
		usageNanoCores := getUint64Value(cs.CPU.UsageNanoCores) + getUint64Value(ps.CPU.UsageNanoCores)
		ps.CPU.UsageCoreNanoSeconds = &usageCoreNanoSeconds
		ps.CPU.UsageNanoCores = &usageNanoCores
	}

	if cs.Memory != nil {
		if ps.Memory == nil {
			ps.Memory = &statsapi.MemoryStats{}
		}

		ps.Memory.Time = cs.Memory.Time
		availableBytes := getUint64Value(cs.Memory.AvailableBytes) + getUint64Value(ps.Memory.AvailableBytes)
		usageBytes := getUint64Value(cs.Memory.UsageBytes) + getUint64Value(ps.Memory.UsageBytes)
		workingSetBytes := getUint64Value(cs.Memory.WorkingSetBytes) + getUint64Value(ps.Memory.WorkingSetBytes)
		rSSBytes := getUint64Value(cs.Memory.RSSBytes) + getUint64Value(ps.Memory.RSSBytes)
		pageFaults := getUint64Value(cs.Memory.PageFaults) + getUint64Value(ps.Memory.PageFaults)
		majorPageFaults := getUint64Value(cs.Memory.MajorPageFaults) + getUint64Value(ps.Memory.MajorPageFaults)
		ps.Memory.AvailableBytes = &availableBytes
		ps.Memory.UsageBytes = &usageBytes
		ps.Memory.WorkingSetBytes = &workingSetBytes
		ps.Memory.RSSBytes = &rSSBytes
		ps.Memory.PageFaults = &pageFaults
		ps.Memory.MajorPageFaults = &majorPageFaults
	}
}

func (p *criStatsProvider) addSwapStats(
	ps *statsapi.PodStats,
	podUID types.UID,
	allInfos map[string]cadvisorapiv2.ContainerInfo,
	cs *statsapi.ContainerStats,
) {
	// try get cpu and memory stats from cadvisor first.
	podCgroupInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
	if podCgroupInfo != nil {
		ps.Swap = cadvisorInfoToSwapStats(podCgroupInfo)
		return
	}

	// Sum Pod cpu and memory stats from containers stats.
	if cs.Swap != nil {
		if ps.Swap == nil {
			ps.Swap = &statsapi.SwapStats{Time: cs.Swap.Time}
		}
		swapAvailableBytes := getUint64Value(cs.Swap.SwapAvailableBytes) + getUint64Value(ps.Swap.SwapAvailableBytes)
		swapUsageBytes := getUint64Value(cs.Swap.SwapUsageBytes) + getUint64Value(ps.Swap.SwapUsageBytes)
		ps.Swap.SwapAvailableBytes = &swapAvailableBytes
		ps.Swap.SwapUsageBytes = &swapUsageBytes
	}
}

func (p *criStatsProvider) addProcessStats(
	ps *statsapi.PodStats,
	container *cadvisorapiv2.ContainerInfo,
) {
	processStats := cadvisorInfoToProcessStats(container)
	// Sum up all of the process stats for each of the containers to obtain the cumulative pod level process count
	ps.ProcessStats = mergeProcessStats(ps.ProcessStats, processStats)
}

func (p *criStatsProvider) makeContainerStats(
	ctx context.Context,
	stats *runtimeapi.ContainerStats,
	container *runtimeapi.Container,
	rootFsInfo *cadvisorapiv2.FsInfo,
	fsIDtoInfo map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo,
	meta *runtimeapi.PodSandboxMetadata,
	updateCPUNanoCoreUsage bool,
) (*statsapi.ContainerStats, error) {
	result := &statsapi.ContainerStats{
		Name: stats.Attributes.Metadata.Name,
		// The StartTime in the summary API is the container creation time.
		StartTime: metav1.NewTime(time.Unix(0, container.CreatedAt)),
		CPU:       &statsapi.CPUStats{},
		Memory:    &statsapi.MemoryStats{},
		Rootfs:    &statsapi.FsStats{},
		Swap:      &statsapi.SwapStats{},
		// UserDefinedMetrics is not supported by CRI.
	}
	if stats.Cpu != nil {
		result.CPU.Time = metav1.NewTime(time.Unix(0, stats.Cpu.Timestamp))
		if stats.Cpu.UsageCoreNanoSeconds != nil {
			result.CPU.UsageCoreNanoSeconds = &stats.Cpu.UsageCoreNanoSeconds.Value
		}
		var usageNanoCores *uint64
		if updateCPUNanoCoreUsage {
			usageNanoCores = p.getAndUpdateContainerUsageNanoCores(stats)
		} else {
			usageNanoCores = p.getContainerUsageNanoCores(stats)
		}
		if usageNanoCores != nil {
			result.CPU.UsageNanoCores = usageNanoCores
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
	} else {
		result.Memory.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.Memory.WorkingSetBytes = uint64Ptr(0)
	}
	if stats.Swap != nil {
		result.Swap.Time = metav1.NewTime(time.Unix(0, stats.Swap.Timestamp))
		if stats.Swap.SwapUsageBytes != nil {
			result.Swap.SwapUsageBytes = &stats.Swap.SwapUsageBytes.Value
		}
		if stats.Swap.SwapAvailableBytes != nil {
			result.Swap.SwapAvailableBytes = &stats.Swap.SwapAvailableBytes.Value
		}
	} else {
		result.Swap.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.Swap.SwapUsageBytes = uint64Ptr(0)
		result.Swap.SwapAvailableBytes = uint64Ptr(0)
	}
	if stats.WritableLayer != nil {
		result.Rootfs.Time = metav1.NewTime(time.Unix(0, stats.WritableLayer.Timestamp))
		if stats.WritableLayer.UsedBytes != nil {
			result.Rootfs.UsedBytes = &stats.WritableLayer.UsedBytes.Value
		}
		if stats.WritableLayer.InodesUsed != nil {
			result.Rootfs.InodesUsed = &stats.WritableLayer.InodesUsed.Value
		}
	}
	fsID := stats.GetWritableLayer().GetFsId()
	var err error
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
			result.Rootfs.InodesFree = imageFsInfo.InodesFree
			result.Rootfs.Inodes = imageFsInfo.Inodes
		}
	}

	// Iterate over the mount points in the container state to get the path of the termination message on the host.
	var terminationMessagePath string
	resp, err := p.runtimeService.ContainerStatus(ctx, container.Id, false)
	if !crierror.IsNotFound(err) {
		for _, mount := range resp.Status.Mounts {
			// The termination message does not have a fixed name, it is randomly generated,
			// so we can only get its path on the host through annotations.
			if mount.ContainerPath == resp.Status.Annotations["io.kubernetes.container.terminationMessagePath"] {
				terminationMessagePath = mount.HostPath
			}
		}
	}

	terminationMessageStats, err := p.hostStatsProvider.getTerminationMessagePathStats(terminationMessagePath, rootFsInfo)
	if err != nil {
		klog.ErrorS(err, "Unable to fetch container termination message stats", "containerName", container.GetMetadata().GetName())
	}

	// NOTE: This doesn't support the old pod log path, `/var/log/pods/UID`. For containers
	// using old log path, empty log stats are returned. This is fine, because we don't
	// officially support in-place upgrade anyway.
	logsStats, err := p.hostStatsProvider.getPodContainerLogStats(meta.GetNamespace(), meta.GetName(), types.UID(meta.GetUid()), container.GetMetadata().GetName(), rootFsInfo)
	if err != nil {
		klog.ErrorS(err, "Unable to fetch container log stats", "containerName", container.GetMetadata().GetName())
	}

	result.Logs = mergeLogStats(rootFsInfo, logsStats, terminationMessageStats)

	return result, nil
}

func mergeLogStats(rootFsInfo *cadvisorapiv2.FsInfo, podLogStats *statsapi.FsStats, terminationMessageStats *statsapi.FsStats) *statsapi.FsStats {
	result := &statsapi.FsStats{
		Time:           metav1.NewTime(rootFsInfo.Timestamp),
		AvailableBytes: &rootFsInfo.Available,
		CapacityBytes:  &rootFsInfo.Capacity,
		InodesFree:     rootFsInfo.InodesFree,
		Inodes:         rootFsInfo.Inodes,
	}

	if podLogStats != nil {
		result.UsedBytes = addUsage(result.UsedBytes, podLogStats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, podLogStats.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &podLogStats.Time)
	}
	if terminationMessageStats != nil {
		result.UsedBytes = addUsage(result.UsedBytes, terminationMessageStats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, terminationMessageStats.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &terminationMessageStats.Time)
	}
	return result
}

func (p *criStatsProvider) makeContainerCPUAndMemoryStats(
	stats *runtimeapi.ContainerStats,
	container *runtimeapi.Container,
) *statsapi.ContainerStats {
	result := &statsapi.ContainerStats{
		Name: stats.Attributes.Metadata.Name,
		// The StartTime in the summary API is the container creation time.
		StartTime: metav1.NewTime(time.Unix(0, container.CreatedAt)),
		CPU:       &statsapi.CPUStats{},
		Memory:    &statsapi.MemoryStats{},
		// UserDefinedMetrics is not supported by CRI.
	}
	if stats.Cpu != nil {
		result.CPU.Time = metav1.NewTime(time.Unix(0, stats.Cpu.Timestamp))
		if stats.Cpu.UsageCoreNanoSeconds != nil {
			result.CPU.UsageCoreNanoSeconds = &stats.Cpu.UsageCoreNanoSeconds.Value
		}

		usageNanoCores := p.getContainerUsageNanoCores(stats)
		if usageNanoCores != nil {
			result.CPU.UsageNanoCores = usageNanoCores
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
	} else {
		result.Memory.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.Memory.WorkingSetBytes = uint64Ptr(0)
	}

	return result
}

// getContainerUsageNanoCores first attempts to get the usage nano cores from the stats reported
// by the CRI. If it is unable to, it gets the information from the cache instead.
func (p *criStatsProvider) getContainerUsageNanoCores(stats *runtimeapi.ContainerStats) *uint64 {
	if stats == nil || stats.Attributes == nil {
		return nil
	}

	// Bypass the cache if the CRI implementation specified the UsageNanoCores.
	if stats.Cpu != nil && stats.Cpu.UsageNanoCores != nil {
		return &stats.Cpu.UsageNanoCores.Value
	}

	p.mutex.RLock()
	defer p.mutex.RUnlock()

	cached, ok := p.cpuUsageCache[stats.Attributes.Id]
	if !ok || cached.usageNanoCores == nil {
		return nil
	}
	// return a copy of the usage
	latestUsage := *cached.usageNanoCores
	return &latestUsage
}

// getAndUpdateContainerUsageNanoCores first attempts to get the usage nano cores from the stats reported
// by the CRI. If it is unable to, it computes usageNanoCores based on the given and the cached usageCoreNanoSeconds,
// updates the cache with the computed usageNanoCores, and returns the usageNanoCores.
func (p *criStatsProvider) getAndUpdateContainerUsageNanoCores(stats *runtimeapi.ContainerStats) *uint64 {
	if stats == nil || stats.Attributes == nil || stats.Cpu == nil {
		return nil
	}
	// Bypass the cache if the CRI implementation specified the UsageNanoCores.
	if stats.Cpu.UsageNanoCores != nil {
		return &stats.Cpu.UsageNanoCores.Value
	}
	// If there is no UsageNanoCores, nor UsageCoreNanoSeconds, there is no information to use
	if stats.Cpu.UsageCoreNanoSeconds == nil {
		return nil
	}
	id := stats.Attributes.Id
	usage, err := func() (*uint64, error) {
		p.mutex.Lock()
		defer p.mutex.Unlock()

		cached, ok := p.cpuUsageCache[id]
		if !ok || cached.stats.UsageCoreNanoSeconds == nil || stats.Cpu.UsageCoreNanoSeconds.Value < cached.stats.UsageCoreNanoSeconds.Value {
			// Cannot compute the usage now, but update the cached stats anyway
			p.cpuUsageCache[id] = &cpuUsageRecord{stats: stats.Cpu, usageNanoCores: nil}
			return nil, nil
		}

		newStats := stats.Cpu
		cachedStats := cached.stats
		nanoSeconds := newStats.Timestamp - cachedStats.Timestamp
		if nanoSeconds <= 0 {
			return nil, fmt.Errorf("zero or negative interval (%v - %v)", newStats.Timestamp, cachedStats.Timestamp)
		}
		usageNanoCores := uint64(float64(newStats.UsageCoreNanoSeconds.Value-cachedStats.UsageCoreNanoSeconds.Value) /
			float64(nanoSeconds) * float64(time.Second/time.Nanosecond))

		// Update cache with new value.
		usageToUpdate := usageNanoCores
		p.cpuUsageCache[id] = &cpuUsageRecord{stats: newStats, usageNanoCores: &usageToUpdate}

		return &usageNanoCores, nil
	}()

	if err != nil {
		// This should not happen. Log now to raise visibility
		klog.ErrorS(err, "Failed updating cpu usage nano core")
	}
	return usage
}

func (p *criStatsProvider) cleanupOutdatedCaches() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for k, v := range p.cpuUsageCache {
		if v == nil {
			delete(p.cpuUsageCache, k)
			continue
		}

		if time.Since(time.Unix(0, v.stats.Timestamp)) > defaultCachePeriod {
			delete(p.cpuUsageCache, k)
		}
	}
}

// removeTerminatedPods returns pods with terminated ones removed.
// It only removes a terminated pod when there is a running instance
// of the pod with the same name and namespace.
// This is needed because:
// 1) PodSandbox may be recreated;
// 2) Pod may be recreated with the same name and namespace.
func removeTerminatedPods(pods []*runtimeapi.PodSandbox) []*runtimeapi.PodSandbox {
	podMap := make(map[statsapi.PodReference][]*runtimeapi.PodSandbox)
	// Sort order by create time
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreatedAt < pods[j].CreatedAt
	})
	for _, pod := range pods {
		refID := statsapi.PodReference{
			Name:      pod.GetMetadata().GetName(),
			Namespace: pod.GetMetadata().GetNamespace(),
			// UID is intentionally left empty.
		}
		podMap[refID] = append(podMap[refID], pod)
	}

	result := make([]*runtimeapi.PodSandbox, 0)
	for _, refs := range podMap {
		if len(refs) == 1 {
			result = append(result, refs[0])
			continue
		}
		found := false
		for i := 0; i < len(refs); i++ {
			if refs[i].State == runtimeapi.PodSandboxState_SANDBOX_READY {
				found = true
				result = append(result, refs[i])
			}
		}
		if !found {
			result = append(result, refs[len(refs)-1])
		}
	}
	return result
}

// removeTerminatedContainers removes all terminated containers since they should
// not be used for usage calculations.
func removeTerminatedContainers(containers []*runtimeapi.Container) []*runtimeapi.Container {
	containerMap := make(map[containerID][]*runtimeapi.Container)
	// Sort order by create time
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].CreatedAt < containers[j].CreatedAt
	})
	for _, container := range containers {
		refID := containerID{
			podRef:        buildPodRef(container.Labels),
			containerName: kubetypes.GetContainerName(container.Labels),
		}
		containerMap[refID] = append(containerMap[refID], container)
	}

	result := make([]*runtimeapi.Container, 0)
	for _, refs := range containerMap {
		for i := 0; i < len(refs); i++ {
			if refs[i].State == runtimeapi.ContainerState_CONTAINER_RUNNING {
				result = append(result, refs[i])
			}
		}
	}
	return result
}

func (p *criStatsProvider) addCadvisorContainerStats(
	cs *statsapi.ContainerStats,
	caPodStats *cadvisorapiv2.ContainerInfo,
) {
	if caPodStats.Spec.HasCustomMetrics {
		cs.UserDefinedMetrics = cadvisorInfoToUserDefinedMetrics(caPodStats)
	}

	cpu, memory := cadvisorInfoToCPUandMemoryStats(caPodStats)
	if cpu != nil {
		cs.CPU = cpu
	}
	if memory != nil {
		cs.Memory = memory
	}

	swap := cadvisorInfoToSwapStats(caPodStats)
	if swap != nil {
		cs.Swap = swap
	}
}

func (p *criStatsProvider) addCadvisorContainerCPUAndMemoryStats(
	cs *statsapi.ContainerStats,
	caPodStats *cadvisorapiv2.ContainerInfo,
) {
	if caPodStats.Spec.HasCustomMetrics {
		cs.UserDefinedMetrics = cadvisorInfoToUserDefinedMetrics(caPodStats)
	}

	cpu, memory := cadvisorInfoToCPUandMemoryStats(caPodStats)
	if cpu != nil {
		cs.CPU = cpu
	}
	if memory != nil {
		cs.Memory = memory
	}
}

func getCRICadvisorStats(infos map[string]cadvisorapiv2.ContainerInfo) (map[string]cadvisorapiv2.ContainerInfo, map[string]cadvisorapiv2.ContainerInfo) {
	stats := make(map[string]cadvisorapiv2.ContainerInfo)
	filteredInfos, cinfosByPodCgroupKey := filterTerminatedContainerInfoAndAssembleByPodCgroupKey(infos)
	for key, info := range filteredInfos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		if !isPodManagedContainer(&info) {
			continue
		}
		stats[extractIDFromCgroupPath(key)] = info
	}
	return stats, cinfosByPodCgroupKey
}

func extractIDFromCgroupPath(cgroupPath string) string {
	// case0 == cgroupfs: "/kubepods/burstable/pod2fc932ce-fdcc-454b-97bd-aadfdeb4c340/9be25294016e2dc0340dd605ce1f57b492039b267a6a618a7ad2a7a58a740f32"
	id := filepath.Base(cgroupPath)

	// case1 == systemd: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod2fc932ce_fdcc_454b_97bd_aadfdeb4c340.slice/cri-containerd-aaefb9d8feed2d453b543f6d928cede7a4dbefa6a0ae7c9b990dd234c56e93b9.scope"
	// trim anything before the final '-' and suffix .scope
	systemdSuffix := ".scope"
	if strings.HasSuffix(id, systemdSuffix) {
		id = strings.TrimSuffix(id, systemdSuffix)
		components := strings.Split(id, "-")
		if len(components) > 1 {
			id = components[len(components)-1]
		}
	}
	return id
}

func criInterfaceToSummary(criIface *runtimeapi.NetworkInterfaceUsage) statsapi.InterfaceStats {
	return statsapi.InterfaceStats{
		Name:     criIface.Name,
		RxBytes:  valueOfUInt64Value(criIface.RxBytes),
		RxErrors: valueOfUInt64Value(criIface.RxErrors),
		TxBytes:  valueOfUInt64Value(criIface.TxBytes),
		TxErrors: valueOfUInt64Value(criIface.TxErrors),
	}
}

func valueOfUInt64Value(value *runtimeapi.UInt64Value) *uint64 {
	if value == nil {
		return nil
	}
	return &value.Value
}
