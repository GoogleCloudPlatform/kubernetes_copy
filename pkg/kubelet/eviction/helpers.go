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

package eviction

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	v1resource "k8s.io/kubernetes/pkg/api/v1/resource"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	volumeutils "k8s.io/kubernetes/pkg/volume/util"
)

const (
	unsupportedEvictionSignal = "unsupported eviction signal %v"
	// Reason is the reason reported back in status.
	Reason = "Evicted"
	// nodeLowMessageFmt is the message for evictions due to resource pressure.
	nodeLowMessageFmt = "The node was low on resource: %v. "
	// nodeConditionMessageFmt is the message for evictions due to resource pressure.
	nodeConditionMessageFmt = "The node had condition: %v. "
	// containerMessageFmt provides additional information for containers exceeding requests
	containerMessageFmt = "Container %s was using %s, request is %s, has larger consumption of %v. "
	// containerEphemeralStorageMessageFmt provides additional information for containers which have exceeded their ES limit
	containerEphemeralStorageMessageFmt = "Container %s exceeded its local ephemeral storage limit %q. "
	// podEphemeralStorageMessageFmt provides additional information for pods which have exceeded their ES limit
	podEphemeralStorageMessageFmt = "Pod ephemeral local storage usage exceeds the total limit of containers %s. "
	// emptyDirMessageFmt provides additional information for empty-dir volumes which have exceeded their size limit
	emptyDirMessageFmt = "Usage of EmptyDir volume %q exceeds the limit %q. "
	// inodes, number. internal to this module, used to account for local disk inode consumption.
	resourceInodes v1.ResourceName = "inodes"
	// resourcePids, number. internal to this module, used to account for local pid consumption.
	resourcePids v1.ResourceName = "pids"
	// OffendingContainersKey is the key in eviction event annotations for the list of container names which exceeded their requests
	OffendingContainersKey = "offending_containers"
	// OffendingContainersUsageKey is the key in eviction event annotations for the list of usage of containers which exceeded their requests
	OffendingContainersUsageKey = "offending_containers_usage"
	// StarvedResourceKey is the key for the starved resource in eviction event annotations
	StarvedResourceKey = "starved_resource"
	// thresholdMetMessageFmt is the message for evictions due to resource pressure.
	thresholdMetMessageFmt = "Threshold quantity: %v, available: %v. "
)

var (
	// signalToNodeCondition maps a signal to the node condition to report if threshold is met.
	signalToNodeCondition map[evictionapi.Signal]v1.NodeConditionType
	// signalToResource maps a Signal to its associated Resource.
	signalToResource map[evictionapi.Signal]v1.ResourceName
)

func init() {
	// map eviction signals to node conditions
	signalToNodeCondition = map[evictionapi.Signal]v1.NodeConditionType{}
	signalToNodeCondition[evictionapi.SignalMemoryAvailable] = v1.NodeMemoryPressure
	signalToNodeCondition[evictionapi.SignalAllocatableMemoryAvailable] = v1.NodeMemoryPressure
	signalToNodeCondition[evictionapi.SignalImageFsAvailable] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalContainerFsAvailable] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalNodeFsAvailable] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalImageFsInodesFree] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalNodeFsInodesFree] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalContainerFsInodesFree] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalPIDAvailable] = v1.NodePIDPressure

	// map signals to resources (and vice-versa)
	signalToResource = map[evictionapi.Signal]v1.ResourceName{}
	signalToResource[evictionapi.SignalMemoryAvailable] = v1.ResourceMemory
	signalToResource[evictionapi.SignalAllocatableMemoryAvailable] = v1.ResourceMemory
	signalToResource[evictionapi.SignalImageFsAvailable] = v1.ResourceEphemeralStorage
	signalToResource[evictionapi.SignalImageFsInodesFree] = resourceInodes
	signalToResource[evictionapi.SignalContainerFsAvailable] = v1.ResourceEphemeralStorage
	signalToResource[evictionapi.SignalContainerFsInodesFree] = resourceInodes
	signalToResource[evictionapi.SignalNodeFsAvailable] = v1.ResourceEphemeralStorage
	signalToResource[evictionapi.SignalNodeFsInodesFree] = resourceInodes
	signalToResource[evictionapi.SignalPIDAvailable] = resourcePids
}

// validSignal returns true if the signal is supported.
func validSignal(signal evictionapi.Signal) bool {
	_, found := signalToResource[signal]
	return found
}

// getReclaimableThreshold finds the threshold and resource to reclaim
func getReclaimableThreshold(thresholds []evictionapi.Threshold) (evictionapi.Threshold, v1.ResourceName, bool) {
	for _, thresholdToReclaim := range thresholds {
		if resourceToReclaim, ok := signalToResource[thresholdToReclaim.Signal]; ok {
			return thresholdToReclaim, resourceToReclaim, true
		}
		klog.V(3).InfoS("Eviction manager: threshold was crossed, but reclaim is not implemented for this threshold.", "threshold", thresholdToReclaim.Signal)
	}
	return evictionapi.Threshold{}, "", false
}

// ParseThresholdConfig parses the flags for thresholds.
func ParseThresholdConfig(allocatableConfig []string, evictionHard, evictionSoft, evictionSoftGracePeriod, evictionMinimumReclaim map[string]string) ([]evictionapi.Threshold, error) {
	results := []evictionapi.Threshold{}
	hardThresholds, err := parseThresholdStatements(evictionHard)
	if err != nil {
		return nil, err
	}
	results = append(results, hardThresholds...)
	softThresholds, err := parseThresholdStatements(evictionSoft)
	if err != nil {
		return nil, err
	}
	gracePeriods, err := parseGracePeriods(evictionSoftGracePeriod)
	if err != nil {
		return nil, err
	}
	minReclaims, err := parseMinimumReclaims(evictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	for i := range softThresholds {
		signal := softThresholds[i].Signal
		period, found := gracePeriods[signal]
		if !found {
			return nil, fmt.Errorf("grace period must be specified for the soft eviction threshold %v", signal)
		}
		softThresholds[i].GracePeriod = period
	}
	results = append(results, softThresholds...)
	for i := range results {
		if minReclaim, ok := minReclaims[results[i].Signal]; ok {
			results[i].MinReclaim = &minReclaim
		}
	}
	for _, key := range allocatableConfig {
		if key == kubetypes.NodeAllocatableEnforcementKey {
			results = addAllocatableThresholds(results)
			break
		}
	}
	return results, nil
}

func addAllocatableThresholds(thresholds []evictionapi.Threshold) []evictionapi.Threshold {
	additionalThresholds := []evictionapi.Threshold{}
	for _, threshold := range thresholds {
		if threshold.Signal == evictionapi.SignalMemoryAvailable && isHardEvictionThreshold(threshold) {
			// Copy the SignalMemoryAvailable to SignalAllocatableMemoryAvailable
			additionalThresholds = append(additionalThresholds, evictionapi.Threshold{
				Signal:     evictionapi.SignalAllocatableMemoryAvailable,
				Operator:   threshold.Operator,
				Value:      threshold.Value,
				MinReclaim: threshold.MinReclaim,
			})
		}
	}
	return append(append([]evictionapi.Threshold{}, thresholds...), additionalThresholds...)
}

// UpdateContainerFsThresholds will add containerfs eviction hard/soft
// settings based on container runtime settings.
// Thresholds are parsed from evictionHard and evictionSoft limits so we will override.
// If there is a single filesystem, then containerfs settings are same as nodefs.
// If there is a separate image filesystem for both containers and images then containerfs settings are same as imagefs.
func UpdateContainerFsThresholds(thresholds []evictionapi.Threshold, imageFs, separateContainerImageFs bool) ([]evictionapi.Threshold, error) {
	hardNodeFsDisk := evictionapi.Threshold{}
	softNodeFsDisk := evictionapi.Threshold{}
	hardNodeINodeDisk := evictionapi.Threshold{}
	softNodeINodeDisk := evictionapi.Threshold{}
	hardImageFsDisk := evictionapi.Threshold{}
	softImageFsDisk := evictionapi.Threshold{}
	hardImageINodeDisk := evictionapi.Threshold{}
	softImageINodeDisk := evictionapi.Threshold{}

	hardContainerFsDisk := -1
	softContainerFsDisk := -1
	hardContainerFsINodes := -1
	softContainerFsINodes := -1
	// Find the imagefs and nodefs thresholds
	var err error = nil
	for idx, threshold := range thresholds {
		if threshold.Signal == evictionapi.SignalImageFsAvailable && isHardEvictionThreshold(threshold) {
			hardImageFsDisk = threshold
		}
		if threshold.Signal == evictionapi.SignalImageFsAvailable && !isHardEvictionThreshold(threshold) {
			softImageFsDisk = threshold
		}
		if threshold.Signal == evictionapi.SignalImageFsInodesFree && isHardEvictionThreshold(threshold) {
			hardImageINodeDisk = threshold
		}
		if threshold.Signal == evictionapi.SignalImageFsInodesFree && !isHardEvictionThreshold(threshold) {
			softImageINodeDisk = threshold
		}
		if threshold.Signal == evictionapi.SignalNodeFsAvailable && isHardEvictionThreshold(threshold) {
			hardNodeFsDisk = threshold
		}
		if threshold.Signal == evictionapi.SignalNodeFsAvailable && !isHardEvictionThreshold(threshold) {
			softNodeFsDisk = threshold
		}
		if threshold.Signal == evictionapi.SignalNodeFsInodesFree && isHardEvictionThreshold(threshold) {
			hardNodeINodeDisk = threshold
		}
		if threshold.Signal == evictionapi.SignalNodeFsInodesFree && !isHardEvictionThreshold(threshold) {
			softNodeINodeDisk = threshold
		}
		// We are logging a warning and we will override the settings.
		// In this case this is safe because we do not support a separate container filesystem.
		// So we want either limits to be same as nodefs or imagefs.
		if threshold.Signal == evictionapi.SignalContainerFsAvailable && isHardEvictionThreshold(threshold) {
			err = errors.Join(fmt.Errorf("found containerfs.available for hard eviction. ignoring"))
			hardContainerFsDisk = idx
		}
		if threshold.Signal == evictionapi.SignalContainerFsAvailable && !isHardEvictionThreshold(threshold) {
			err = errors.Join(fmt.Errorf("found containerfs.available for soft eviction. ignoring"))
			softContainerFsDisk = idx
		}
		if threshold.Signal == evictionapi.SignalContainerFsInodesFree && isHardEvictionThreshold(threshold) {
			err = errors.Join(fmt.Errorf("found containerfs.inodesFree for hard eviction. ignoring"))
			hardContainerFsINodes = idx
		}
		if threshold.Signal == evictionapi.SignalContainerFsInodesFree && !isHardEvictionThreshold(threshold) {
			err = errors.Join(fmt.Errorf("found containerfs.inodesFree for soft eviction. ignoring"))
			softContainerFsINodes = idx
		}
	}
	// Either split disk case (containerfs=nodefs) or single filesystem
	if (imageFs && separateContainerImageFs) || (!imageFs && !separateContainerImageFs) {
		if hardContainerFsDisk != -1 {
			thresholds[hardContainerFsDisk] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsAvailable, Operator: hardNodeFsDisk.Operator, Value: hardNodeFsDisk.Value, MinReclaim: hardNodeFsDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:     evictionapi.SignalContainerFsAvailable,
				Operator:   hardNodeFsDisk.Operator,
				Value:      hardNodeFsDisk.Value,
				MinReclaim: hardNodeFsDisk.MinReclaim,
			})
		}
		if softContainerFsDisk != -1 {
			thresholds[softContainerFsDisk] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsAvailable, GracePeriod: softNodeFsDisk.GracePeriod, Operator: softNodeFsDisk.Operator, Value: softNodeFsDisk.Value, MinReclaim: softNodeFsDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:      evictionapi.SignalContainerFsAvailable,
				Operator:    softNodeFsDisk.Operator,
				Value:       softNodeFsDisk.Value,
				MinReclaim:  softNodeFsDisk.MinReclaim,
				GracePeriod: softNodeFsDisk.GracePeriod,
			})
		}
		if hardContainerFsINodes != -1 {
			thresholds[hardContainerFsINodes] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsInodesFree, Operator: hardNodeINodeDisk.Operator, Value: hardNodeINodeDisk.Value, MinReclaim: hardNodeINodeDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:     evictionapi.SignalContainerFsInodesFree,
				Operator:   hardNodeINodeDisk.Operator,
				Value:      hardNodeINodeDisk.Value,
				MinReclaim: hardNodeINodeDisk.MinReclaim,
			})
		}
		if softContainerFsINodes != -1 {
			thresholds[softContainerFsINodes] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsInodesFree, GracePeriod: softNodeINodeDisk.GracePeriod, Operator: softNodeINodeDisk.Operator, Value: softNodeINodeDisk.Value, MinReclaim: softNodeINodeDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:      evictionapi.SignalContainerFsInodesFree,
				Operator:    softNodeINodeDisk.Operator,
				Value:       softNodeINodeDisk.Value,
				MinReclaim:  softNodeINodeDisk.MinReclaim,
				GracePeriod: softNodeINodeDisk.GracePeriod,
			})
		}
	}
	// Separate image filesystem case
	if imageFs && !separateContainerImageFs {
		if hardContainerFsDisk != -1 {
			thresholds[hardContainerFsDisk] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsAvailable, Operator: hardImageFsDisk.Operator, Value: hardImageFsDisk.Value, MinReclaim: hardImageFsDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:     evictionapi.SignalContainerFsAvailable,
				Operator:   hardImageFsDisk.Operator,
				Value:      hardImageFsDisk.Value,
				MinReclaim: hardImageFsDisk.MinReclaim,
			})
		}
		if softContainerFsDisk != -1 {
			thresholds[softContainerFsDisk] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsAvailable, GracePeriod: softImageFsDisk.GracePeriod, Operator: softImageFsDisk.Operator, Value: softImageFsDisk.Value, MinReclaim: softImageFsDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:      evictionapi.SignalContainerFsAvailable,
				Operator:    softImageFsDisk.Operator,
				Value:       softImageFsDisk.Value,
				MinReclaim:  softImageFsDisk.MinReclaim,
				GracePeriod: softImageFsDisk.GracePeriod,
			})
		}
		if hardContainerFsINodes != -1 {
			thresholds[hardContainerFsINodes] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsInodesFree, GracePeriod: hardImageINodeDisk.GracePeriod, Operator: hardImageINodeDisk.Operator, Value: hardImageINodeDisk.Value, MinReclaim: hardImageINodeDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:     evictionapi.SignalContainerFsInodesFree,
				Operator:   hardImageINodeDisk.Operator,
				Value:      hardImageINodeDisk.Value,
				MinReclaim: hardImageINodeDisk.MinReclaim,
			})
		}
		if softContainerFsINodes != -1 {
			thresholds[softContainerFsINodes] = evictionapi.Threshold{
				Signal: evictionapi.SignalContainerFsInodesFree, GracePeriod: softImageINodeDisk.GracePeriod, Operator: softImageINodeDisk.Operator, Value: softImageINodeDisk.Value, MinReclaim: softImageINodeDisk.MinReclaim,
			}
		} else {
			thresholds = append(thresholds, evictionapi.Threshold{
				Signal:      evictionapi.SignalContainerFsInodesFree,
				Operator:    softImageINodeDisk.Operator,
				Value:       softImageINodeDisk.Value,
				MinReclaim:  softImageINodeDisk.MinReclaim,
				GracePeriod: softImageINodeDisk.GracePeriod,
			})
		}
	}
	return thresholds, err
}

// parseThresholdStatements parses the input statements into a list of Threshold objects.
func parseThresholdStatements(statements map[string]string) ([]evictionapi.Threshold, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	results := []evictionapi.Threshold{}
	for signal, val := range statements {
		result, err := parseThresholdStatement(evictionapi.Signal(signal), val)
		if err != nil {
			return nil, err
		}
		if result != nil {
			results = append(results, *result)
		}
	}
	return results, nil
}

// parseThresholdStatement parses a threshold statement and returns a threshold,
// or nil if the threshold should be ignored.
func parseThresholdStatement(signal evictionapi.Signal, val string) (*evictionapi.Threshold, error) {
	if !validSignal(signal) {
		return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
	}
	operator := evictionapi.OpForSignal[signal]
	if strings.HasSuffix(val, "%") {
		// ignore 0% and 100%
		if val == "0%" || val == "100%" {
			return nil, nil
		}
		percentage, err := parsePercentage(val)
		if err != nil {
			return nil, err
		}
		if percentage < 0 {
			return nil, fmt.Errorf("eviction percentage threshold %v must be >= 0%%: %s", signal, val)
		}
		// percentage is a float and should not be greater than 1 (100%)
		if percentage > 1 {
			return nil, fmt.Errorf("eviction percentage threshold %v must be <= 100%%: %s", signal, val)
		}
		return &evictionapi.Threshold{
			Signal:   signal,
			Operator: operator,
			Value: evictionapi.ThresholdValue{
				Percentage: percentage,
			},
		}, nil
	}
	quantity, err := resource.ParseQuantity(val)
	if err != nil {
		return nil, err
	}
	if quantity.Sign() < 0 || quantity.IsZero() {
		return nil, fmt.Errorf("eviction threshold %v must be positive: %s", signal, &quantity)
	}
	return &evictionapi.Threshold{
		Signal:   signal,
		Operator: operator,
		Value: evictionapi.ThresholdValue{
			Quantity: &quantity,
		},
	}, nil
}

// parsePercentage parses a string representing a percentage value
func parsePercentage(input string) (float32, error) {
	value, err := strconv.ParseFloat(strings.TrimRight(input, "%"), 32)
	if err != nil {
		return 0, err
	}
	return float32(value) / 100, nil
}

// parseGracePeriods parses the grace period statements
func parseGracePeriods(statements map[string]string) (map[evictionapi.Signal]time.Duration, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	results := map[evictionapi.Signal]time.Duration{}
	for signal, val := range statements {
		signal := evictionapi.Signal(signal)
		if !validSignal(signal) {
			return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
		}
		gracePeriod, err := time.ParseDuration(val)
		if err != nil {
			return nil, err
		}
		if gracePeriod < 0 {
			return nil, fmt.Errorf("invalid eviction grace period specified: %v, must be a positive value", val)
		}
		results[signal] = gracePeriod
	}
	return results, nil
}

// parseMinimumReclaims parses the minimum reclaim statements
func parseMinimumReclaims(statements map[string]string) (map[evictionapi.Signal]evictionapi.ThresholdValue, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	results := map[evictionapi.Signal]evictionapi.ThresholdValue{}
	for signal, val := range statements {
		signal := evictionapi.Signal(signal)
		if !validSignal(signal) {
			return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
		}
		if strings.HasSuffix(val, "%") {
			percentage, err := parsePercentage(val)
			if err != nil {
				return nil, err
			}
			if percentage <= 0 {
				return nil, fmt.Errorf("eviction percentage minimum reclaim %v must be positive: %s", signal, val)
			}
			results[signal] = evictionapi.ThresholdValue{
				Percentage: percentage,
			}
			continue
		}
		quantity, err := resource.ParseQuantity(val)
		if err != nil {
			return nil, err
		}
		if quantity.Sign() < 0 {
			return nil, fmt.Errorf("negative eviction minimum reclaim specified for %v", signal)
		}
		results[signal] = evictionapi.ThresholdValue{
			Quantity: &quantity,
		}
	}
	return results, nil
}

// diskUsage converts used bytes into a resource quantity.
func diskUsage(fsStats *statsapi.FsStats) *resource.Quantity {
	if fsStats == nil || fsStats.UsedBytes == nil {
		return &resource.Quantity{Format: resource.BinarySI}
	}
	usage := int64(*fsStats.UsedBytes)
	return resource.NewQuantity(usage, resource.BinarySI)
}

// inodeUsage converts inodes consumed into a resource quantity.
func inodeUsage(fsStats *statsapi.FsStats) *resource.Quantity {
	if fsStats == nil || fsStats.InodesUsed == nil {
		return &resource.Quantity{Format: resource.DecimalSI}
	}
	usage := int64(*fsStats.InodesUsed)
	return resource.NewQuantity(usage, resource.DecimalSI)
}

// memoryUsage converts working set into a resource quantity.
func memoryUsage(memStats *statsapi.MemoryStats) *resource.Quantity {
	if memStats == nil || memStats.WorkingSetBytes == nil {
		return &resource.Quantity{Format: resource.BinarySI}
	}
	usage := int64(*memStats.WorkingSetBytes)
	return resource.NewQuantity(usage, resource.BinarySI)
}

// processUsage converts working set into a process count.
func processUsage(processStats *statsapi.ProcessStats) uint64 {
	if processStats == nil || processStats.ProcessCount == nil {
		return 0
	}
	usage := uint64(*processStats.ProcessCount)
	return usage
}

// localVolumeNames returns the set of volumes for the pod that are local
// TODO: summary API should report what volumes consume local storage rather than hard-code here.
func localVolumeNames(pod *v1.Pod) []string {
	result := []string{}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil ||
			volumeutils.IsLocalEphemeralVolume(volume) {
			result = append(result, volume.Name)
		}
	}
	return result
}

// containerUsage aggregates container disk usage and inode consumption for the specified stats to measure.
func containerUsage(podStats statsapi.PodStats, statsToMeasure []fsStatsType) v1.ResourceList {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.DecimalSI}
	for _, container := range podStats.Containers {
		if hasFsStatsType(statsToMeasure, fsStatsRoot) {
			disk.Add(*diskUsage(container.Rootfs))
			inodes.Add(*inodeUsage(container.Rootfs))
		}
		if hasFsStatsType(statsToMeasure, fsStatsLogs) {
			disk.Add(*diskUsage(container.Logs))
			inodes.Add(*inodeUsage(container.Logs))
		}
	}
	return v1.ResourceList{
		v1.ResourceEphemeralStorage: disk,
		resourceInodes:              inodes,
	}
}

// podLocalVolumeUsage aggregates pod local volumes disk usage and inode consumption for the specified stats to measure.
func podLocalVolumeUsage(volumeNames []string, podStats statsapi.PodStats) v1.ResourceList {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.DecimalSI}
	for _, volumeName := range volumeNames {
		for _, volumeStats := range podStats.VolumeStats {
			if volumeStats.Name == volumeName {
				disk.Add(*diskUsage(&volumeStats.FsStats))
				inodes.Add(*inodeUsage(&volumeStats.FsStats))
				break
			}
		}
	}
	return v1.ResourceList{
		v1.ResourceEphemeralStorage: disk,
		resourceInodes:              inodes,
	}
}

// podDiskUsage aggregates pod disk usage and inode consumption for the specified stats to measure.
func podDiskUsage(podStats statsapi.PodStats, pod *v1.Pod, statsToMeasure []fsStatsType) (v1.ResourceList, error) {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.DecimalSI}

	containerUsageList := containerUsage(podStats, statsToMeasure)
	disk.Add(containerUsageList[v1.ResourceEphemeralStorage])
	inodes.Add(containerUsageList[resourceInodes])

	if hasFsStatsType(statsToMeasure, fsStatsLocalVolumeSource) {
		volumeNames := localVolumeNames(pod)
		podLocalVolumeUsageList := podLocalVolumeUsage(volumeNames, podStats)
		disk.Add(podLocalVolumeUsageList[v1.ResourceEphemeralStorage])
		inodes.Add(podLocalVolumeUsageList[resourceInodes])
	}
	return v1.ResourceList{
		v1.ResourceEphemeralStorage: disk,
		resourceInodes:              inodes,
	}, nil
}

// formatThreshold formats a threshold for logging.
func formatThreshold(threshold evictionapi.Threshold) string {
	return fmt.Sprintf("threshold(signal=%v, operator=%v, value=%v, gracePeriod=%v)", threshold.Signal, threshold.Operator, evictionapi.ThresholdValue(threshold.Value), threshold.GracePeriod)
}

// cachedStatsFunc returns a statsFunc based on the provided pod stats.
func cachedStatsFunc(podStats []statsapi.PodStats) statsFunc {
	uid2PodStats := map[string]statsapi.PodStats{}
	for i := range podStats {
		uid2PodStats[podStats[i].PodRef.UID] = podStats[i]
	}
	return func(pod *v1.Pod) (statsapi.PodStats, bool) {
		stats, found := uid2PodStats[string(pod.UID)]
		return stats, found
	}
}

// Cmp compares p1 and p2 and returns:
//
//	-1 if p1 <  p2
//	 0 if p1 == p2
//	+1 if p1 >  p2
type cmpFunc func(p1, p2 *v1.Pod) int

// multiSorter implements the Sort interface, sorting changes within.
type multiSorter struct {
	pods []*v1.Pod
	cmp  []cmpFunc
}

// Sort sorts the argument slice according to the less functions passed to OrderedBy.
func (ms *multiSorter) Sort(pods []*v1.Pod) {
	ms.pods = pods
	sort.Sort(ms)
}

// OrderedBy returns a Sorter that sorts using the cmp functions, in order.
// Call its Sort method to sort the data.
func orderedBy(cmp ...cmpFunc) *multiSorter {
	return &multiSorter{
		cmp: cmp,
	}
}

// Len is part of sort.Interface.
func (ms *multiSorter) Len() int {
	return len(ms.pods)
}

// Swap is part of sort.Interface.
func (ms *multiSorter) Swap(i, j int) {
	ms.pods[i], ms.pods[j] = ms.pods[j], ms.pods[i]
}

// Less is part of sort.Interface.
func (ms *multiSorter) Less(i, j int) bool {
	p1, p2 := ms.pods[i], ms.pods[j]
	var k int
	for k = 0; k < len(ms.cmp)-1; k++ {
		cmpResult := ms.cmp[k](p1, p2)
		// p1 is less than p2
		if cmpResult < 0 {
			return true
		}
		// p1 is greater than p2
		if cmpResult > 0 {
			return false
		}
		// we don't know yet
	}
	// the last cmp func is the final decider
	return ms.cmp[k](p1, p2) < 0
}

// priority compares pods by Priority, if priority is enabled.
func priority(p1, p2 *v1.Pod) int {
	priority1 := corev1helpers.PodPriority(p1)
	priority2 := corev1helpers.PodPriority(p2)
	if priority1 == priority2 {
		return 0
	}
	if priority1 > priority2 {
		return 1
	}
	return -1
}

// exceedMemoryRequests compares whether or not pods' memory usage exceeds their requests
func exceedMemoryRequests(stats statsFunc) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			return cmpBool(!p1Found, !p2Found)
		}

		p1Memory := memoryUsage(p1Stats.Memory)
		p2Memory := memoryUsage(p2Stats.Memory)
		p1ExceedsRequests := p1Memory.Cmp(v1resource.GetResourceRequestQuantity(p1, v1.ResourceMemory)) == 1
		p2ExceedsRequests := p2Memory.Cmp(v1resource.GetResourceRequestQuantity(p2, v1.ResourceMemory)) == 1
		// prioritize evicting the pod which exceeds its requests
		return cmpBool(p1ExceedsRequests, p2ExceedsRequests)
	}
}

// memory compares pods by largest consumer of memory relative to request.
func memory(stats statsFunc) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			return cmpBool(!p1Found, !p2Found)
		}

		// adjust p1, p2 usage relative to the request (if any)
		p1Memory := memoryUsage(p1Stats.Memory)
		p1Request := v1resource.GetResourceRequestQuantity(p1, v1.ResourceMemory)
		p1Memory.Sub(p1Request)

		p2Memory := memoryUsage(p2Stats.Memory)
		p2Request := v1resource.GetResourceRequestQuantity(p2, v1.ResourceMemory)
		p2Memory.Sub(p2Request)

		// prioritize evicting the pod which has the larger consumption of memory
		return p2Memory.Cmp(*p1Memory)
	}
}

// process compares pods by largest consumer of process number relative to request.
func process(stats statsFunc) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			return cmpBool(!p1Found, !p2Found)
		}

		p1Process := processUsage(p1Stats.ProcessStats)
		p2Process := processUsage(p2Stats.ProcessStats)
		return int(p2Process - p1Process)
	}
}

// exceedDiskRequests compares whether or not pods' disk usage exceeds their requests
func exceedDiskRequests(stats statsFunc, fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			return cmpBool(!p1Found, !p2Found)
		}

		p1Usage, p1Err := podDiskUsage(p1Stats, p1, fsStatsToMeasure)
		p2Usage, p2Err := podDiskUsage(p2Stats, p2, fsStatsToMeasure)
		if p1Err != nil || p2Err != nil {
			// prioritize evicting the pod which had an error getting stats
			return cmpBool(p1Err != nil, p2Err != nil)
		}

		p1Disk := p1Usage[diskResource]
		p2Disk := p2Usage[diskResource]
		p1ExceedsRequests := p1Disk.Cmp(v1resource.GetResourceRequestQuantity(p1, diskResource)) == 1
		p2ExceedsRequests := p2Disk.Cmp(v1resource.GetResourceRequestQuantity(p2, diskResource)) == 1
		// prioritize evicting the pod which exceeds its requests
		return cmpBool(p1ExceedsRequests, p2ExceedsRequests)
	}
}

// disk compares pods by largest consumer of disk relative to request for the specified disk resource.
func disk(stats statsFunc, fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			return cmpBool(!p1Found, !p2Found)
		}
		p1Usage, p1Err := podDiskUsage(p1Stats, p1, fsStatsToMeasure)
		p2Usage, p2Err := podDiskUsage(p2Stats, p2, fsStatsToMeasure)
		if p1Err != nil || p2Err != nil {
			// prioritize evicting the pod which had an error getting stats
			return cmpBool(p1Err != nil, p2Err != nil)
		}

		// adjust p1, p2 usage relative to the request (if any)
		p1Disk := p1Usage[diskResource]
		p2Disk := p2Usage[diskResource]
		p1Request := v1resource.GetResourceRequestQuantity(p1, v1.ResourceEphemeralStorage)
		p1Disk.Sub(p1Request)
		p2Request := v1resource.GetResourceRequestQuantity(p2, v1.ResourceEphemeralStorage)
		p2Disk.Sub(p2Request)
		// prioritize evicting the pod which has the larger consumption of disk
		return p2Disk.Cmp(p1Disk)
	}
}

// cmpBool compares booleans, placing true before false
func cmpBool(a, b bool) int {
	if a == b {
		return 0
	}
	if !b {
		return -1
	}
	return 1
}

// rankMemoryPressure orders the input pods for eviction in response to memory pressure.
// It ranks by whether or not the pod's usage exceeds its requests, then by priority, and
// finally by memory usage above requests.
func rankMemoryPressure(pods []*v1.Pod, stats statsFunc) {
	orderedBy(exceedMemoryRequests(stats), priority, memory(stats)).Sort(pods)
}

// rankPIDPressure orders the input pods by priority in response to PID pressure.
func rankPIDPressure(pods []*v1.Pod, stats statsFunc) {
	orderedBy(priority, process(stats)).Sort(pods)
}

// rankDiskPressureFunc returns a rankFunc that measures the specified fs stats.
func rankDiskPressureFunc(fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) rankFunc {
	return func(pods []*v1.Pod, stats statsFunc) {
		orderedBy(exceedDiskRequests(stats, fsStatsToMeasure, diskResource), priority, disk(stats, fsStatsToMeasure, diskResource)).Sort(pods)
	}
}

// byEvictionPriority implements sort.Interface for []v1.ResourceName.
type byEvictionPriority []evictionapi.Threshold

func (a byEvictionPriority) Len() int      { return len(a) }
func (a byEvictionPriority) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// Less ranks memory before all other resources, and ranks thresholds with no resource to reclaim last
func (a byEvictionPriority) Less(i, j int) bool {
	_, jSignalHasResource := signalToResource[a[j].Signal]
	return a[i].Signal == evictionapi.SignalMemoryAvailable || a[i].Signal == evictionapi.SignalAllocatableMemoryAvailable || !jSignalHasResource
}

// makeSignalObservations derives observations using the specified summary provider.
func makeSignalObservations(summary *statsapi.Summary) (signalObservations, statsFunc) {
	// build the function to work against for pod stats
	statsFunc := cachedStatsFunc(summary.Pods)
	// build an evaluation context for current eviction signals
	result := signalObservations{}

	memoryAvailableSignal := makeMemoryAvailableSignalObservation(summary)
	if memoryAvailableSignal != nil {
		result[evictionapi.SignalMemoryAvailable] = *memoryAvailableSignal
	}

	if allocatableContainer, err := getSysContainer(summary.Node.SystemContainers, statsapi.SystemContainerPods); err != nil {
		klog.ErrorS(err, "Eviction manager: failed to construct signal", "signal", evictionapi.SignalAllocatableMemoryAvailable)
	} else {
		if memory := allocatableContainer.Memory; memory != nil && memory.AvailableBytes != nil && memory.WorkingSetBytes != nil {
			result[evictionapi.SignalAllocatableMemoryAvailable] = signalObservation{
				available: resource.NewQuantity(int64(*memory.AvailableBytes), resource.BinarySI),
				capacity:  resource.NewQuantity(int64(*memory.AvailableBytes+*memory.WorkingSetBytes), resource.BinarySI),
				time:      memory.Time,
			}
		}
	}
	if nodeFs := summary.Node.Fs; nodeFs != nil {
		if nodeFs.AvailableBytes != nil && nodeFs.CapacityBytes != nil {
			result[evictionapi.SignalNodeFsAvailable] = signalObservation{
				available: resource.NewQuantity(int64(*nodeFs.AvailableBytes), resource.BinarySI),
				capacity:  resource.NewQuantity(int64(*nodeFs.CapacityBytes), resource.BinarySI),
				time:      nodeFs.Time,
			}
		}
		if nodeFs.InodesFree != nil && nodeFs.Inodes != nil {
			result[evictionapi.SignalNodeFsInodesFree] = signalObservation{
				available: resource.NewQuantity(int64(*nodeFs.InodesFree), resource.DecimalSI),
				capacity:  resource.NewQuantity(int64(*nodeFs.Inodes), resource.DecimalSI),
				time:      nodeFs.Time,
			}
		}
	}
	if summary.Node.Runtime != nil {
		if imageFs := summary.Node.Runtime.ImageFs; imageFs != nil {
			if imageFs.AvailableBytes != nil && imageFs.CapacityBytes != nil {
				result[evictionapi.SignalImageFsAvailable] = signalObservation{
					available: resource.NewQuantity(int64(*imageFs.AvailableBytes), resource.BinarySI),
					capacity:  resource.NewQuantity(int64(*imageFs.CapacityBytes), resource.BinarySI),
					time:      imageFs.Time,
				}
			}
			if imageFs.InodesFree != nil && imageFs.Inodes != nil {
				result[evictionapi.SignalImageFsInodesFree] = signalObservation{
					available: resource.NewQuantity(int64(*imageFs.InodesFree), resource.DecimalSI),
					capacity:  resource.NewQuantity(int64(*imageFs.Inodes), resource.DecimalSI),
					time:      imageFs.Time,
				}
			}
		}
		if containerFs := summary.Node.Runtime.ContainerFs; containerFs != nil {
			if containerFs.AvailableBytes != nil && containerFs.CapacityBytes != nil {
				result[evictionapi.SignalContainerFsAvailable] = signalObservation{
					available: resource.NewQuantity(int64(*containerFs.AvailableBytes), resource.BinarySI),
					capacity:  resource.NewQuantity(int64(*containerFs.CapacityBytes), resource.BinarySI),
					time:      containerFs.Time,
				}
			}
			if containerFs.InodesFree != nil && containerFs.Inodes != nil {
				result[evictionapi.SignalContainerFsInodesFree] = signalObservation{
					available: resource.NewQuantity(int64(*containerFs.InodesFree), resource.DecimalSI),
					capacity:  resource.NewQuantity(int64(*containerFs.Inodes), resource.DecimalSI),
					time:      containerFs.Time,
				}
			}
		}
	}
	if rlimit := summary.Node.Rlimit; rlimit != nil {
		if rlimit.NumOfRunningProcesses != nil && rlimit.MaxPID != nil {
			available := int64(*rlimit.MaxPID) - int64(*rlimit.NumOfRunningProcesses)
			result[evictionapi.SignalPIDAvailable] = signalObservation{
				available: resource.NewQuantity(available, resource.DecimalSI),
				capacity:  resource.NewQuantity(int64(*rlimit.MaxPID), resource.DecimalSI),
				time:      rlimit.Time,
			}
		}
	}
	return result, statsFunc
}

func getSysContainer(sysContainers []statsapi.ContainerStats, name string) (*statsapi.ContainerStats, error) {
	for _, cont := range sysContainers {
		if cont.Name == name {
			return &cont, nil
		}
	}
	return nil, fmt.Errorf("system container %q not found in metrics", name)
}

// thresholdsMet returns the set of thresholds that were met independent of grace period
func thresholdsMet(thresholds []evictionapi.Threshold, observations signalObservations, enforceMinReclaim bool) []evictionapi.Threshold {
	results := []evictionapi.Threshold{}
	for i := range thresholds {
		threshold := thresholds[i]
		observed, found := observations[threshold.Signal]
		if !found {
			klog.InfoS("Eviction manager: no observation found for eviction signal", "signal", threshold.Signal)
			continue
		}
		// determine if we have met the specified threshold
		thresholdMet := false
		quantity := evictionapi.GetThresholdQuantity(threshold.Value, observed.capacity)
		// if enforceMinReclaim is specified, we compare relative to value - minreclaim
		if enforceMinReclaim && threshold.MinReclaim != nil {
			quantity.Add(*evictionapi.GetThresholdQuantity(*threshold.MinReclaim, observed.capacity))
		}
		thresholdResult := quantity.Cmp(*observed.available)
		switch threshold.Operator {
		case evictionapi.OpLessThan:
			thresholdMet = thresholdResult > 0
		}
		if thresholdMet {
			results = append(results, threshold)
		}
	}
	return results
}

func debugLogObservations(logPrefix string, observations signalObservations) {
	klogV := klog.V(3)
	if !klogV.Enabled() {
		return
	}
	for k, v := range observations {
		if !v.time.IsZero() {
			klogV.InfoS("Eviction manager:", "log", logPrefix, "signal", k, "resourceName", signalToResource[k], "available", v.available, "capacity", v.capacity, "time", v.time)
		} else {
			klogV.InfoS("Eviction manager:", "log", logPrefix, "signal", k, "resourceName", signalToResource[k], "available", v.available, "capacity", v.capacity)
		}
	}
}

func debugLogThresholdsWithObservation(logPrefix string, thresholds []evictionapi.Threshold, observations signalObservations) {
	klogV := klog.V(3)
	if !klogV.Enabled() {
		return
	}
	for i := range thresholds {
		threshold := thresholds[i]
		observed, found := observations[threshold.Signal]
		if found {
			quantity := evictionapi.GetThresholdQuantity(threshold.Value, observed.capacity)
			klogV.InfoS("Eviction manager: threshold observed resource", "log", logPrefix, "signal", threshold.Signal, "resourceName", signalToResource[threshold.Signal], "quantity", quantity, "available", observed.available)
		} else {
			klogV.InfoS("Eviction manager: threshold had no observation", "log", logPrefix, "signal", threshold.Signal)
		}
	}
}

func thresholdsUpdatedStats(thresholds []evictionapi.Threshold, observations, lastObservations signalObservations) []evictionapi.Threshold {
	results := []evictionapi.Threshold{}
	for i := range thresholds {
		threshold := thresholds[i]
		observed, found := observations[threshold.Signal]
		if !found {
			klog.InfoS("Eviction manager: no observation found for eviction signal", "signal", threshold.Signal)
			continue
		}
		last, found := lastObservations[threshold.Signal]
		if !found || observed.time.IsZero() || observed.time.After(last.time.Time) {
			results = append(results, threshold)
		}
	}
	return results
}

// thresholdsFirstObservedAt merges the input set of thresholds with the previous observation to determine when active set of thresholds were initially met.
func thresholdsFirstObservedAt(thresholds []evictionapi.Threshold, lastObservedAt thresholdsObservedAt, now time.Time) thresholdsObservedAt {
	results := thresholdsObservedAt{}
	for i := range thresholds {
		observedAt, found := lastObservedAt[thresholds[i]]
		if !found {
			observedAt = now
		}
		results[thresholds[i]] = observedAt
	}
	return results
}

// thresholdsMetGracePeriod returns the set of thresholds that have satisfied associated grace period
func thresholdsMetGracePeriod(observedAt thresholdsObservedAt, now time.Time) []evictionapi.Threshold {
	results := []evictionapi.Threshold{}
	for threshold, at := range observedAt {
		duration := now.Sub(at)
		if duration < threshold.GracePeriod {
			klog.V(2).InfoS("Eviction manager: eviction criteria not yet met", "threshold", formatThreshold(threshold), "duration", duration)
			continue
		}
		results = append(results, threshold)
	}
	return results
}

// nodeConditions returns the set of node conditions associated with a threshold
func nodeConditions(thresholds []evictionapi.Threshold) []v1.NodeConditionType {
	results := []v1.NodeConditionType{}
	for _, threshold := range thresholds {
		if nodeCondition, found := signalToNodeCondition[threshold.Signal]; found {
			if !hasNodeCondition(results, nodeCondition) {
				results = append(results, nodeCondition)
			}
		}
	}
	return results
}

// nodeConditionsLastObservedAt merges the input with the previous observation to determine when a condition was most recently met.
func nodeConditionsLastObservedAt(nodeConditions []v1.NodeConditionType, lastObservedAt nodeConditionsObservedAt, now time.Time) nodeConditionsObservedAt {
	results := nodeConditionsObservedAt{}
	// the input conditions were observed "now"
	for i := range nodeConditions {
		results[nodeConditions[i]] = now
	}
	// the conditions that were not observed now are merged in with their old time
	for key, value := range lastObservedAt {
		_, found := results[key]
		if !found {
			results[key] = value
		}
	}
	return results
}

// nodeConditionsObservedSince returns the set of conditions that have been observed within the specified period
func nodeConditionsObservedSince(observedAt nodeConditionsObservedAt, period time.Duration, now time.Time) []v1.NodeConditionType {
	results := []v1.NodeConditionType{}
	for nodeCondition, at := range observedAt {
		duration := now.Sub(at)
		if duration < period {
			results = append(results, nodeCondition)
		}
	}
	return results
}

// hasFsStatsType returns true if the fsStat is in the input list
func hasFsStatsType(inputs []fsStatsType, item fsStatsType) bool {
	for _, input := range inputs {
		if input == item {
			return true
		}
	}
	return false
}

// hasNodeCondition returns true if the node condition is in the input list
func hasNodeCondition(inputs []v1.NodeConditionType, item v1.NodeConditionType) bool {
	for _, input := range inputs {
		if input == item {
			return true
		}
	}
	return false
}

// mergeThresholds will merge both threshold lists eliminating duplicates.
func mergeThresholds(inputsA []evictionapi.Threshold, inputsB []evictionapi.Threshold) []evictionapi.Threshold {
	results := inputsA
	for _, threshold := range inputsB {
		if !hasThreshold(results, threshold) {
			results = append(results, threshold)
		}
	}
	return results
}

// hasThreshold returns true if the threshold is in the input list
func hasThreshold(inputs []evictionapi.Threshold, item evictionapi.Threshold) bool {
	for _, input := range inputs {
		if input.GracePeriod == item.GracePeriod && input.Operator == item.Operator && input.Signal == item.Signal && compareThresholdValue(input.Value, item.Value) {
			return true
		}
	}
	return false
}

// compareThresholdValue returns true if the two thresholdValue objects are logically the same
func compareThresholdValue(a evictionapi.ThresholdValue, b evictionapi.ThresholdValue) bool {
	if a.Quantity != nil {
		if b.Quantity == nil {
			return false
		}
		return a.Quantity.Cmp(*b.Quantity) == 0
	}
	if b.Quantity != nil {
		return false
	}
	return a.Percentage == b.Percentage
}

// isHardEvictionThreshold returns true if eviction should immediately occur
func isHardEvictionThreshold(threshold evictionapi.Threshold) bool {
	return threshold.GracePeriod == time.Duration(0)
}

func isAllocatableEvictionThreshold(threshold evictionapi.Threshold) bool {
	return threshold.Signal == evictionapi.SignalAllocatableMemoryAvailable
}

// buildSignalToRankFunc returns ranking functions associated with resources
func buildSignalToRankFunc(withImageFs bool, imageContainerSplitFs bool) map[evictionapi.Signal]rankFunc {
	signalToRankFunc := map[evictionapi.Signal]rankFunc{
		evictionapi.SignalMemoryAvailable:            rankMemoryPressure,
		evictionapi.SignalAllocatableMemoryAvailable: rankMemoryPressure,
		evictionapi.SignalPIDAvailable:               rankPIDPressure,
	}
	// usage of an imagefs is optional
	// We have a dedicated Image filesystem (images and containers are on same disk)
	// then we assume it is just a separate imagefs
	if withImageFs && !imageContainerSplitFs {
		// with an imagefs, nodefs pod rank func for eviction only includes logs and local volumes
		signalToRankFunc[evictionapi.SignalNodeFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource}, v1.ResourceEphemeralStorage)
		signalToRankFunc[evictionapi.SignalNodeFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
		// with an imagefs, imagefs pod rank func for eviction only includes rootfs
		signalToRankFunc[evictionapi.SignalImageFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsImages}, v1.ResourceEphemeralStorage)
		signalToRankFunc[evictionapi.SignalImageFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsImages}, resourceInodes)
		signalToRankFunc[evictionapi.SignalContainerFsAvailable] = signalToRankFunc[evictionapi.SignalImageFsAvailable]
		signalToRankFunc[evictionapi.SignalContainerFsInodesFree] = signalToRankFunc[evictionapi.SignalImageFsInodesFree]

		// If both imagefs and container fs are on separate disks
		// we want to track the writeable layer in containerfs signals.
	} else if withImageFs && imageContainerSplitFs {
		// with an imagefs, nodefs pod rank func for eviction only includes logs and local volumes
		signalToRankFunc[evictionapi.SignalNodeFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource, fsStatsRoot}, v1.ResourceEphemeralStorage)
		signalToRankFunc[evictionapi.SignalNodeFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource, fsStatsRoot}, resourceInodes)
		signalToRankFunc[evictionapi.SignalContainerFsAvailable] = signalToRankFunc[evictionapi.SignalNodeFsAvailable]
		signalToRankFunc[evictionapi.SignalContainerFsInodesFree] = signalToRankFunc[evictionapi.SignalNodeFsInodesFree]
		// with an imagefs, containerfs pod rank func for eviction only includes rootfs

		signalToRankFunc[evictionapi.SignalImageFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsImages}, v1.ResourceEphemeralStorage)
		signalToRankFunc[evictionapi.SignalImageFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsImages}, resourceInodes)
		// If image fs is not on separate disk as root but container fs is
	} else {
		// without an imagefs, nodefs pod rank func for eviction looks at all fs stats.
		// since imagefs and nodefs share a common device, they share common ranking functions.
		signalToRankFunc[evictionapi.SignalNodeFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsImages, fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, v1.ResourceEphemeralStorage)
		signalToRankFunc[evictionapi.SignalNodeFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsImages, fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
		signalToRankFunc[evictionapi.SignalImageFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsImages, fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, v1.ResourceEphemeralStorage)
		signalToRankFunc[evictionapi.SignalImageFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsImages, fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
		signalToRankFunc[evictionapi.SignalContainerFsAvailable] = signalToRankFunc[evictionapi.SignalNodeFsAvailable]
		signalToRankFunc[evictionapi.SignalContainerFsInodesFree] = signalToRankFunc[evictionapi.SignalNodeFsInodesFree]
	}
	return signalToRankFunc
}

// PodIsEvicted returns true if the reported pod status is due to an eviction.
func PodIsEvicted(podStatus v1.PodStatus) bool {
	return podStatus.Phase == v1.PodFailed && podStatus.Reason == Reason
}

// buildSignalToNodeReclaimFuncs returns reclaim functions associated with resources.
func buildSignalToNodeReclaimFuncs(imageGC ImageGC, containerGC ContainerGC, withImageFs bool, splitContainerImageFs bool) map[evictionapi.Signal]nodeReclaimFuncs {
	signalToReclaimFunc := map[evictionapi.Signal]nodeReclaimFuncs{}
	// usage of an imagefs is optional
	if withImageFs && !splitContainerImageFs {
		// with an imagefs, nodefs pressure should just delete logs
		signalToReclaimFunc[evictionapi.SignalNodeFsAvailable] = nodeReclaimFuncs{}
		signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree] = nodeReclaimFuncs{}
		// with an imagefs, imagefs pressure should delete unused images
		signalToReclaimFunc[evictionapi.SignalImageFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalContainerFsAvailable] = signalToReclaimFunc[evictionapi.SignalImageFsAvailable]
		signalToReclaimFunc[evictionapi.SignalContainerFsInodesFree] = signalToReclaimFunc[evictionapi.SignalImageFsInodesFree]
		// usage of imagefs and container fs on separate disks
		// containers gc on containerfs pressure
		// image gc on imagefs pressure
	} else if withImageFs && splitContainerImageFs {
		// with an imagefs, imagefs pressure should delete unused images
		signalToReclaimFunc[evictionapi.SignalImageFsAvailable] = nodeReclaimFuncs{imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsInodesFree] = nodeReclaimFuncs{imageGC.DeleteUnusedImages}
		// with an split fs and imagefs, containerfs pressure should delete unused containers
		signalToReclaimFunc[evictionapi.SignalNodeFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers}
		signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers}
		signalToReclaimFunc[evictionapi.SignalContainerFsAvailable] = signalToReclaimFunc[evictionapi.SignalNodeFsAvailable]
		signalToReclaimFunc[evictionapi.SignalContainerFsInodesFree] = signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree]
	} else {
		// without an imagefs, nodefs pressure should delete logs, and unused images
		// since imagefs, containerfs and nodefs share a common device, they share common reclaim functions
		signalToReclaimFunc[evictionapi.SignalNodeFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalContainerFsAvailable] = signalToReclaimFunc[evictionapi.SignalNodeFsAvailable]
		signalToReclaimFunc[evictionapi.SignalContainerFsInodesFree] = signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree]
	}
	return signalToReclaimFunc
}

// evictionMessage constructs a useful message about why an eviction occurred, and annotations to provide metadata about the eviction
func evictionMessage(resourceToReclaim v1.ResourceName, pod *v1.Pod, stats statsFunc, thresholds []evictionapi.Threshold, observations signalObservations) (message string, annotations map[string]string) {
	annotations = make(map[string]string)
	message = fmt.Sprintf(nodeLowMessageFmt, resourceToReclaim)
	quantity, available := getThresholdMetInfo(resourceToReclaim, thresholds, observations)
	if quantity != nil && available != nil {
		message += fmt.Sprintf(thresholdMetMessageFmt, quantity, available)
	}
	exceededContainers := []string{}
	containerUsage := []string{}
	podStats, ok := stats(pod)
	if !ok {
		return
	}
	// Since the resources field cannot be specified for ephemeral containers,
	// they will always be blamed for resource overuse when an eviction occurs.
	// That’s why only regular, init and restartable init containers are considered
	// for the eviction message.
	containers := pod.Spec.Containers
	if len(pod.Spec.InitContainers) != 0 {
		containers = append(containers, pod.Spec.InitContainers...)
	}
	for _, containerStats := range podStats.Containers {
		for _, container := range containers {
			if container.Name == containerStats.Name {
				requests := container.Resources.Requests[resourceToReclaim]
				var usage *resource.Quantity
				switch resourceToReclaim {
				case v1.ResourceEphemeralStorage:
					if containerStats.Rootfs != nil && containerStats.Rootfs.UsedBytes != nil && containerStats.Logs != nil && containerStats.Logs.UsedBytes != nil {
						usage = resource.NewQuantity(int64(*containerStats.Rootfs.UsedBytes+*containerStats.Logs.UsedBytes), resource.BinarySI)
					}
				case v1.ResourceMemory:
					if containerStats.Memory != nil && containerStats.Memory.WorkingSetBytes != nil {
						usage = resource.NewQuantity(int64(*containerStats.Memory.WorkingSetBytes), resource.BinarySI)
					}
				}
				if usage != nil && usage.Cmp(requests) > 0 {
					message += fmt.Sprintf(containerMessageFmt, container.Name, usage.String(), requests.String(), resourceToReclaim)
					exceededContainers = append(exceededContainers, container.Name)
					containerUsage = append(containerUsage, usage.String())
				}
				// Found the container to compare resource usage with,
				// so it's safe to break out of the containers loop here.
				break
			}
		}
	}
	annotations[OffendingContainersKey] = strings.Join(exceededContainers, ",")
	annotations[OffendingContainersUsageKey] = strings.Join(containerUsage, ",")
	annotations[StarvedResourceKey] = string(resourceToReclaim)
	return
}

// getThresholdMetInfo get the threshold quantity and available for the resource resourceToReclaim
func getThresholdMetInfo(resourceToReclaim v1.ResourceName, thresholds []evictionapi.Threshold, observations signalObservations) (quantity *resource.Quantity, available *resource.Quantity) {
	for i := range thresholds {
		threshold := thresholds[i]
		if signalToResource[threshold.Signal] == resourceToReclaim {
			observed, found := observations[threshold.Signal]
			if found {
				quantity := evictionapi.GetThresholdQuantity(threshold.Value, observed.capacity)
				return quantity, observed.available
			}
		}
	}
	return nil, nil
}
