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
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"k8s.io/klog/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubetypes "k8s.io/kubelet/pkg/types"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
)

// cadvisorStatsProvider implements the containerStatsProvider interface by
// getting the container stats from cAdvisor. This is needed by
// integrations which do not provide stats from CRI. See
// `pkg/kubelet/cadvisor/util.go#UsingLegacyCadvisorStats` for the logic for
// determining which integrations do not provide stats from CRI.
type cadvisorStatsProvider struct {
	// cadvisor is used to get the stats of the cgroup for the containers that
	// are managed by pods.
	cadvisor cadvisor.Interface
	// resourceAnalyzer is used to get the volume stats of the pods.
	resourceAnalyzer stats.ResourceAnalyzer
	// imageService is used to get the stats of the image filesystem.
	imageService kubecontainer.ImageService
	// statusProvider is used to get pod metadata
	statusProvider status.PodStatusProvider
	// hostStatsProvider is used to get pod host stat usage.
	hostStatsProvider HostStatsProvider
}

// newCadvisorStatsProvider returns a containerStatsProvider that provides
// container stats from cAdvisor.
func newCadvisorStatsProvider(
	cadvisor cadvisor.Interface,
	resourceAnalyzer stats.ResourceAnalyzer,
	imageService kubecontainer.ImageService,
	statusProvider status.PodStatusProvider,
	hostStatsProvider HostStatsProvider,
) containerStatsProvider {
	return &cadvisorStatsProvider{
		cadvisor:          cadvisor,
		resourceAnalyzer:  resourceAnalyzer,
		imageService:      imageService,
		statusProvider:    statusProvider,
		hostStatsProvider: hostStatsProvider,
	}
}

// ListPodStats returns the stats of all the pod-managed containers.
func (p *cadvisorStatsProvider) ListPodStats(ctx context.Context) ([]statsapi.PodStats, error) {
	// Gets node root filesystem information and image filesystem stats, which
	// will be used to populate the available and capacity bytes/inodes in
	// container stats.
	rootFsInfo, err := p.cadvisor.RootFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs info: %v", err)
	}
	imageFsInfo, err := p.cadvisor.ImagesFsInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get imageFs info: %v", err)
	}
	infos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info from cadvisor: %v", err)
	}

	filteredInfos, terminatedInfos, allInfos := filterTerminatedContainerInfoAndAssembleByPodCgroupKey(infos)
	// Map each container to a pod and update the PodStats with container data.
	podToStats := p.containerInfoToPodStats(filteredInfos, rootFsInfo, imageFsInfo)
	podTerminatedContainersToStats := p.containerInfoToPodStats(terminatedInfos, rootFsInfo, imageFsInfo)

	// Add each PodStats to the result.
	result := make([]statsapi.PodStats, 0, len(podToStats))
	for podRef, podStats := range podToStats {
		var podTerminatedContainerStats []statsapi.ContainerStats
		if terminatedContainers, found := podTerminatedContainersToStats[podRef]; found {
			podTerminatedContainerStats = terminatedContainers.Containers
		}
		makePodStorageStats(podStats, podTerminatedContainerStats, &rootFsInfo, p.resourceAnalyzer, p.hostStatsProvider, false)

		podUID := types.UID(podStats.PodRef.UID)
		// Lookup the pod-level cgroup's CPU and memory stats
		podInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
		if podInfo != nil {
			cpu, memory := cadvisorInfoToCPUandMemoryStats(podInfo)
			podStats.CPU = cpu
			podStats.Memory = memory
			podStats.Swap = cadvisorInfoToSwapStats(podInfo)
			// ProcessStats were accumulated as the containers were iterated.
		}

		status, found := p.statusProvider.GetPodStatus(podUID)
		if found && status.StartTime != nil && !status.StartTime.IsZero() {
			podStats.StartTime = *status.StartTime
			// only append stats if we were able to get the start time of the pod
			result = append(result, *podStats)
		}
	}

	return result, nil
}

// ListPodStatsAndUpdateCPUNanoCoreUsage updates the cpu nano core usage for
// the containers and returns the stats for all the pod-managed containers.
// For cadvisor, cpu nano core usages are pre-computed and cached, so this
// function simply calls ListPodStats.
func (p *cadvisorStatsProvider) ListPodStatsAndUpdateCPUNanoCoreUsage(ctx context.Context) ([]statsapi.PodStats, error) {
	return p.ListPodStats(ctx)
}

// ListPodCPUAndMemoryStats returns the cpu and memory stats of all the pod-managed containers.
func (p *cadvisorStatsProvider) ListPodCPUAndMemoryStats(_ context.Context) ([]statsapi.PodStats, error) {
	infos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info from cadvisor: %v", err)
	}
	filteredInfos, _, allInfos := filterTerminatedContainerInfoAndAssembleByPodCgroupKey(infos)
	// Map each container to a pod and update the PodStats with container data.
	podToStats := map[statsapi.PodReference]*statsapi.PodStats{}
	for key, cinfo := range filteredInfos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		ref := buildPodRef(cinfo.Spec.Labels)

		// Lookup the PodStats for the pod using the PodRef. If none exists,
		// initialize a new entry.
		podStats, found := podToStats[ref]
		if !found {
			podStats = &statsapi.PodStats{PodRef: ref}
			podToStats[ref] = podStats
		}

		// Update the PodStats entry with the stats from the container by
		// adding it to podStats.Containers.
		containerName := kubetypes.GetContainerName(cinfo.Spec.Labels)
		if containerName == kubetypes.PodInfraContainerName {
			// Special case for infrastructure container which is hidden from
			// the user and has network stats.
			podStats.StartTime = metav1.NewTime(cinfo.Spec.CreationTime)
		} else {
			podStats.Containers = append(podStats.Containers, *cadvisorInfoToContainerCPUAndMemoryStats(containerName, &cinfo))
		}
	}

	// Add each PodStats to the result.
	result := make([]statsapi.PodStats, 0, len(podToStats))
	for _, podStats := range podToStats {
		podUID := types.UID(podStats.PodRef.UID)
		// Lookup the pod-level cgroup's CPU and memory stats
		podInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
		if podInfo != nil {
			cpu, memory := cadvisorInfoToCPUandMemoryStats(podInfo)
			podStats.CPU = cpu
			podStats.Memory = memory
			podStats.Swap = cadvisorInfoToSwapStats(podInfo)
		}
		result = append(result, *podStats)
	}

	return result, nil
}

// ImageFsStats returns the stats of the filesystem for storing images.
func (p *cadvisorStatsProvider) ImageFsStats(ctx context.Context) (imageFsRet *statsapi.FsStats, containerFsRet *statsapi.FsStats, errCall error) {
	imageFsInfo, err := p.cadvisor.ImagesFsInfo(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get imageFs info: %v", err)
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.KubeletSeparateDiskGC) {
		imageStats, err := p.imageService.ImageStats(ctx)
		if err != nil || imageStats == nil {
			return nil, nil, fmt.Errorf("failed to get image stats: %v", err)
		}

		var imageFsInodesUsed *uint64
		if imageFsInfo.Inodes != nil && imageFsInfo.InodesFree != nil {
			imageFsIU := *imageFsInfo.Inodes - *imageFsInfo.InodesFree
			imageFsInodesUsed = &imageFsIU
		}

		imageFs := &statsapi.FsStats{
			Time:           metav1.NewTime(imageFsInfo.Timestamp),
			AvailableBytes: &imageFsInfo.Available,
			CapacityBytes:  &imageFsInfo.Capacity,
			UsedBytes:      &imageStats.TotalStorageBytes,
			InodesFree:     imageFsInfo.InodesFree,
			Inodes:         imageFsInfo.Inodes,
			InodesUsed:     imageFsInodesUsed,
		}
		return imageFs, imageFs, nil
	}
	imageStats, err := p.imageService.ImageFsInfo(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get image stats: %w", err)
	}
	if imageStats == nil || len(imageStats.ImageFilesystems) == 0 || len(imageStats.ContainerFilesystems) == 0 {
		return nil, nil, fmt.Errorf("missing image stats: %+v", imageStats)
	}

	splitFileSystem := false
	imageFs := imageStats.ImageFilesystems[0]
	containerFs := imageStats.ContainerFilesystems[0]
	if imageFs.FsId != nil && containerFs.FsId != nil && imageFs.FsId.Mountpoint != containerFs.FsId.Mountpoint {
		splitFileSystem = true
	}
	var imageFsInodesUsed *uint64
	if imageFsInfo.Inodes != nil && imageFsInfo.InodesFree != nil {
		imageFsIU := *imageFsInfo.Inodes - *imageFsInfo.InodesFree
		imageFsInodesUsed = &imageFsIU
	}
	var usedBytes uint64
	if imageFs.GetUsedBytes() != nil {
		usedBytes = imageFs.GetUsedBytes().GetValue()
	}

	fsStats := &statsapi.FsStats{
		Time:           metav1.NewTime(imageFsInfo.Timestamp),
		AvailableBytes: &imageFsInfo.Available,
		CapacityBytes:  &imageFsInfo.Capacity,
		UsedBytes:      &usedBytes,
		InodesFree:     imageFsInfo.InodesFree,
		Inodes:         imageFsInfo.Inodes,
		InodesUsed:     imageFsInodesUsed,
	}
	// We rely on cadvisor to have the crio-containers label for split filesystem case.
	// We return to avoid checking ContainerFsInfo.
	if !splitFileSystem {
		return fsStats, fsStats, nil
	}

	containerFsInfo, err := p.cadvisor.ContainerFsInfo(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get container fs info: %w", err)
	}
	// ImageFs and ContainerFs could be on different paths on the same device.
	if containerFsInfo.Device == imageFsInfo.Device {
		return fsStats, fsStats, nil
	}

	klog.InfoS("Detect Split Filesystem", "ImageFilesystems", imageStats.ImageFilesystems[0], "ContainerFilesystems", imageStats.ContainerFilesystems[0])

	var containerFsInodesUsed *uint64
	if containerFsInfo.Inodes != nil && containerFsInfo.InodesFree != nil {
		containerFsIU := *containerFsInfo.Inodes - *containerFsInfo.InodesFree
		containerFsInodesUsed = &containerFsIU
	}
	var usedContainerBytes uint64
	if containerFs.GetUsedBytes() != nil {
		usedContainerBytes = containerFs.GetUsedBytes().GetValue()
	}

	fsContainerStats := &statsapi.FsStats{
		Time:           metav1.NewTime(containerFsInfo.Timestamp),
		AvailableBytes: &containerFsInfo.Available,
		CapacityBytes:  &containerFsInfo.Capacity,
		UsedBytes:      &usedContainerBytes,
		InodesFree:     containerFsInfo.InodesFree,
		Inodes:         containerFsInfo.Inodes,
		InodesUsed:     containerFsInodesUsed,
	}

	return fsStats, fsContainerStats, nil
}

// ImageFsDevice returns name of the device where the image filesystem locates,
// e.g. /dev/sda1.
func (p *cadvisorStatsProvider) ImageFsDevice(ctx context.Context) (string, error) {
	imageFsInfo, err := p.cadvisor.ImagesFsInfo(ctx)
	if err != nil {
		return "", err
	}
	return imageFsInfo.Device, nil
}

// buildPodRef returns a PodReference that identifies the Pod managing cinfo
func buildPodRef(containerLabels map[string]string) statsapi.PodReference {
	podName := kubetypes.GetPodName(containerLabels)
	podNamespace := kubetypes.GetPodNamespace(containerLabels)
	podUID := kubetypes.GetPodUID(containerLabels)
	return statsapi.PodReference{Name: podName, Namespace: podNamespace, UID: podUID}
}

// isPodManagedContainer returns true if the cinfo container is managed by a Pod
func isPodManagedContainer(cinfo *cadvisorapiv2.ContainerInfo) bool {
	podName := kubetypes.GetPodName(cinfo.Spec.Labels)
	podNamespace := kubetypes.GetPodNamespace(cinfo.Spec.Labels)
	managed := podName != "" && podNamespace != ""
	if !managed && podName != podNamespace {
		klog.InfoS(
			"Expect container to have either both podName and podNamespace labels, or neither",
			"podNameLabel", podName, "podNamespaceLabel", podNamespace)
	}
	return managed
}

// getCadvisorPodInfoFromPodUID returns a pod cgroup information by matching the podUID with its CgroupName identifier base name
func getCadvisorPodInfoFromPodUID(podUID types.UID, infos map[string]cadvisorapiv2.ContainerInfo) *cadvisorapiv2.ContainerInfo {
	if info, found := infos[cm.GetPodCgroupNameSuffix(podUID)]; found {
		return &info
	}
	return nil
}

// filterTerminatedContainerInfoAndAssembleByPodCgroupKey returns the specified containerInfo but with
// the stats of the terminated containers removed and all containerInfos assembled by pod cgroup key.
// the first return map is container cgroup name <-> ContainerInfo and
// the second return map is pod cgroup key <-> ContainerInfo.
// A ContainerInfo is considered to be of a terminated container if it has an
// older CreationTime and zero CPU instantaneous and memory RSS usage.
func filterTerminatedContainerInfoAndAssembleByPodCgroupKey(containerInfo map[string]cadvisorapiv2.ContainerInfo) (map[string]cadvisorapiv2.ContainerInfo, map[string]cadvisorapiv2.ContainerInfo, map[string]cadvisorapiv2.ContainerInfo) {
	cinfoMap := make(map[containerID][]containerInfoWithCgroup)
	cinfosByPodCgroupKey := make(map[string]cadvisorapiv2.ContainerInfo)
	for key, cinfo := range containerInfo {
		var podCgroupKey string
		if cm.IsSystemdStyleName(key) {
			// Convert to internal cgroup name and take the last component only.
			internalCgroupName := cm.ParseSystemdToCgroupName(key)
			podCgroupKey = internalCgroupName[len(internalCgroupName)-1]
		} else {
			// Take last component only.
			podCgroupKey = filepath.Base(key)
		}
		cinfosByPodCgroupKey[podCgroupKey] = cinfo
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		cinfoID := containerID{
			podRef:        buildPodRef(cinfo.Spec.Labels),
			containerName: kubetypes.GetContainerName(cinfo.Spec.Labels),
		}
		cinfoMap[cinfoID] = append(cinfoMap[cinfoID], containerInfoWithCgroup{
			cinfo:  cinfo,
			cgroup: key,
		})
	}
	result := make(map[string]cadvisorapiv2.ContainerInfo)
	terminated := make(map[string]cadvisorapiv2.ContainerInfo)
	for _, refs := range cinfoMap {
		if len(refs) == 1 {
			// ContainerInfo with no CPU/memory/network usage for uncleaned cgroups of
			// already terminated containers, which should not be shown in the results.
			if !isContainerTerminated(&refs[0].cinfo) {
				result[refs[0].cgroup] = refs[0].cinfo
			} else {
				terminated[refs[0].cgroup] = refs[0].cinfo
			}
			continue
		}
		sort.Sort(ByCreationTime(refs))
		for i := len(refs) - 1; i >= 0; i-- {
			if hasMemoryAndCPUInstUsage(&refs[i].cinfo) {
				result[refs[i].cgroup] = refs[i].cinfo
				break
			}
		}
	}
	return result, terminated, cinfosByPodCgroupKey
}

func (p *cadvisorStatsProvider) containerInfoToPodStats(containerInfos map[string]cadvisorapiv2.ContainerInfo, rootFsInfo, imageFsInfo cadvisorapiv2.FsInfo) map[statsapi.PodReference]*statsapi.PodStats {
	podToStats := map[statsapi.PodReference]*statsapi.PodStats{}
	for key, cinfo := range containerInfos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		ref := buildPodRef(cinfo.Spec.Labels)

		// Lookup the PodStats for the pod using the PodRef. If none exists,
		// initialize a new entry.
		podStats, found := podToStats[ref]
		if !found {
			podStats = &statsapi.PodStats{PodRef: ref}
			podToStats[ref] = podStats
		}

		// Update the PodStats entry with the stats from the container by
		// adding it to podStats.Containers.
		containerName := kubetypes.GetContainerName(cinfo.Spec.Labels)
		if containerName == kubetypes.PodInfraContainerName {
			// Special case for infrastructure container which is hidden from
			// the user and has network stats.
			podStats.Network = cadvisorInfoToNetworkStats(&cinfo)
		} else {
			containerStat := cadvisorInfoToContainerStats(containerName, &cinfo, &rootFsInfo, &imageFsInfo)
			// NOTE: This doesn't support the old pod log path, `/var/log/pods/UID`. For containers
			// using old log path, they will be populated by cadvisorInfoToContainerStats.
			podUID := types.UID(podStats.PodRef.UID)
			logs, err := p.hostStatsProvider.getPodContainerLogStats(podStats.PodRef.Namespace, podStats.PodRef.Name, podUID, containerName, &rootFsInfo)
			if err != nil {
				klog.ErrorS(err, "Unable to fetch container log stats", "containerName", containerName)
			} else {
				containerStat.Logs = logs
			}
			podStats.Containers = append(podStats.Containers, *containerStat)
		}
		// Either way, collect process stats
		podStats.ProcessStats = mergeProcessStats(podStats.ProcessStats, cadvisorInfoToProcessStats(&cinfo))
	}
	return podToStats
}

// ByCreationTime implements sort.Interface for []containerInfoWithCgroup based
// on the cinfo.Spec.CreationTime field.
type ByCreationTime []containerInfoWithCgroup

func (a ByCreationTime) Len() int      { return len(a) }
func (a ByCreationTime) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByCreationTime) Less(i, j int) bool {
	if a[i].cinfo.Spec.CreationTime.Equal(a[j].cinfo.Spec.CreationTime) {
		// There shouldn't be two containers with the same name and/or the same
		// creation time. However, to make the logic here robust, we break the
		// tie by moving the one without CPU instantaneous or memory RSS usage
		// to the beginning.
		return hasMemoryAndCPUInstUsage(&a[j].cinfo)
	}
	return a[i].cinfo.Spec.CreationTime.Before(a[j].cinfo.Spec.CreationTime)
}

// containerID is the identity of a container in a pod.
type containerID struct {
	podRef        statsapi.PodReference
	containerName string
}

// containerInfoWithCgroup contains the ContainerInfo and its cgroup name.
type containerInfoWithCgroup struct {
	cinfo  cadvisorapiv2.ContainerInfo
	cgroup string
}

// hasMemoryAndCPUInstUsage returns true if the specified container info has
// both non-zero CPU instantaneous usage and non-zero memory RSS usage, and
// false otherwise.
func hasMemoryAndCPUInstUsage(info *cadvisorapiv2.ContainerInfo) bool {
	if !info.Spec.HasCpu || !info.Spec.HasMemory {
		return false
	}
	cstat, found := latestContainerStats(info)
	if !found {
		return false
	}
	if cstat.CpuInst == nil {
		return false
	}
	return cstat.CpuInst.Usage.Total != 0 && cstat.Memory.RSS != 0
}

// isContainerTerminated returns true if the specified container meet one of the following conditions
// 1. info.spec both cpu memory and network are false conditions
// 2. info.Stats both network and cpu or memory are nil
// 3. both zero CPU instantaneous usage zero memory RSS usage and zero network usage,
// and false otherwise.
func isContainerTerminated(info *cadvisorapiv2.ContainerInfo) bool {
	if !info.Spec.HasCpu && !info.Spec.HasMemory && !info.Spec.HasNetwork {
		return true
	}
	cstat, found := latestContainerStats(info)
	if !found {
		return true
	}
	if cstat.Network != nil {
		iStats := cadvisorInfoToNetworkStats(info)
		if iStats != nil {
			for _, iStat := range iStats.Interfaces {
				if *iStat.RxErrors != 0 || *iStat.TxErrors != 0 || *iStat.RxBytes != 0 || *iStat.TxBytes != 0 {
					return false
				}
			}
		}
	}
	if cstat.CpuInst == nil || cstat.Memory == nil {
		return true
	}
	return cstat.CpuInst.Usage.Total == 0 && cstat.Memory.RSS == 0
}

func getCadvisorContainerInfo(ca cadvisor.Interface) (map[string]cadvisorapiv2.ContainerInfo, error) {
	infos, err := ca.ContainerInfoV2("/", cadvisorapiv2.RequestOptions{
		IdType:    cadvisorapiv2.TypeName,
		Count:     2, // 2 samples are needed to compute "instantaneous" CPU
		Recursive: true,
	})
	if err != nil {
		if _, ok := infos["/"]; ok {
			// If the failure is partial, log it and return a best-effort
			// response.
			klog.ErrorS(err, "Partial failure issuing cadvisor.ContainerInfoV2")
		} else {
			return nil, fmt.Errorf("failed to get root cgroup stats: %w", err)
		}
	}
	return infos, nil
}
