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

//go:generate mockery
package eviction

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
)

// fsStatsType defines the types of filesystem stats to collect.
type fsStatsType string

const (
	// fsStatsLocalVolumeSource identifies stats for pod local volume sources.
	fsStatsLocalVolumeSource fsStatsType = "localVolumeSource"
	// fsStatsLogs identifies stats for pod logs.
	fsStatsLogs fsStatsType = "logs"
	// fsStatsRoot identifies stats for pod container writable layers.
	fsStatsRoot fsStatsType = "root"
	// fsStatsImages identifies stats for pod container read-only layers
	fsStatsImages fsStatsType = "images"
)

// Config holds information about how eviction is configured.
type Config struct {
	// PressureTransitionPeriod is duration the kubelet has to wait before transitioning out of a pressure condition.
	PressureTransitionPeriod time.Duration
	// Maximum allowed grace period (in seconds) to use when terminating pods in response to a soft eviction threshold being met.
	MaxPodGracePeriodSeconds int64
	// Thresholds define the set of conditions monitored to trigger eviction.
	Thresholds []evictionapi.Threshold
	// KernelMemcgNotification if true will integrate with the kernel memcg notification to determine if memory thresholds are crossed.
	KernelMemcgNotification bool
	// PodCgroupRoot is the cgroup which contains all pods.
	PodCgroupRoot string
}

// Manager evaluates when an eviction threshold for node stability has been met on the node.
type Manager interface {
	// Start starts the control loop to monitor eviction thresholds at specified interval.
	Start(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc, terminatedPodFunc TerminatedPodsFunc, podCleanedUpFunc PodCleanedUpFunc, monitoringInterval time.Duration)

	// IsUnderMemoryPressure returns true if the node is under memory pressure.
	IsUnderMemoryPressure() bool

	// IsUnderDiskPressure returns true if the node is under disk pressure.
	IsUnderDiskPressure() bool

	// IsUnderPIDPressure returns true if the node is under PID pressure.
	IsUnderPIDPressure() bool
}

// DiskInfoProvider is responsible for informing the manager how disk is configured.
type DiskInfoProvider interface {
	// HasDedicatedImageFs returns true if the imagefs is on a separate device from the rootfs.
	HasDedicatedImageFs(ctx context.Context) (bool, error)
	// HasDedicatedContainerFs returns true if the container fs is on a separate device from the rootfs.
	HasDedicatedContainerFs(ctx context.Context) (bool, error)
}

// ImageGC is responsible for performing garbage collection of unused images.
type ImageGC interface {
	// DeleteUnusedImages deletes unused images.
	DeleteUnusedImages(ctx context.Context) error
}

// ContainerGC is responsible for performing garbage collection of unused containers.
type ContainerGC interface {
	// DeleteAllUnusedContainers deletes all unused containers, even those that belong to pods that are terminated, but not deleted.
	DeleteAllUnusedContainers(ctx context.Context) error
}

// KillPodFunc kills a pod.
// The pod status is updated, and then it is killed with the specified grace period.
// This function must block until either the pod is killed or an error is encountered.
// Arguments:
// pod - the pod to kill
// status - the desired status to associate with the pod (i.e. why its killed)
// gracePeriodOverride - the grace period override to use instead of what is on the pod spec
type KillPodFunc func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error

// MirrorPodFunc returns the mirror pod for the given static pod and
// whether it was known to the pod manager.
type MirrorPodFunc func(*v1.Pod) (*v1.Pod, bool)

// ActivePodsFunc returns pods bound to the kubelet that are active (i.e. non-terminal state)
type ActivePodsFunc func() []*v1.Pod

// TerminatedPodsFunc return pods bound to the kubelet that are inactive (i.e. terminal state)
type TerminatedPodsFunc func() []*v1.Pod

// PodCleanedUpFunc returns true if all resources associated with a pod have been reclaimed.
type PodCleanedUpFunc func(*v1.Pod) bool

// statsFunc returns the usage stats if known for an input pod.
type statsFunc func(pod *v1.Pod) (statsapi.PodStats, bool)

// rankFunc sorts the pods in eviction order
type rankFunc func(pods []*v1.Pod, stats statsFunc)

// signalObservation is the observed resource usage
type signalObservation struct {
	// The resource capacity
	capacity *resource.Quantity
	// The available resource
	available *resource.Quantity
	// Time at which the observation was taken
	time metav1.Time
}

// signalObservations maps a signal to an observed quantity
type signalObservations map[evictionapi.Signal]signalObservation

// thresholdsObservedAt maps a threshold to a time that it was observed
type thresholdsObservedAt map[evictionapi.Threshold]time.Time

// nodeConditionsObservedAt maps a node condition to a time that it was observed
type nodeConditionsObservedAt map[v1.NodeConditionType]time.Time

// nodeReclaimFunc is a function that knows how to reclaim a resource from the node without impacting pods.
type nodeReclaimFunc func(ctx context.Context) error

// nodeReclaimFuncs is an ordered list of nodeReclaimFunc
type nodeReclaimFuncs []nodeReclaimFunc

// CgroupNotifier generates events from cgroup events
type CgroupNotifier interface {
	// Start causes the CgroupNotifier to begin notifying on the eventCh
	Start(eventCh chan<- struct{})
	// Stop stops all processes and cleans up file descriptors associated with the CgroupNotifier
	Stop()
}

// NotifierFactory creates CgroupNotifer
type NotifierFactory interface {
	// NewCgroupNotifier creates a CgroupNotifier that creates events when the threshold
	// on the attribute in the cgroup specified by the path is crossed.
	NewCgroupNotifier(path, attribute string, threshold int64) (CgroupNotifier, error)
}

// ThresholdNotifier manages CgroupNotifiers based on memory eviction thresholds, and performs a function
// when memory eviction thresholds are crossed
type ThresholdNotifier interface {
	// Start calls the notifier function when the CgroupNotifier notifies the ThresholdNotifier that an event occurred
	Start()
	// UpdateThreshold updates the memory cgroup threshold based on the metrics provided.
	// Calling UpdateThreshold with recent metrics allows the ThresholdNotifier to trigger at the
	// eviction threshold more accurately
	UpdateThreshold(summary *statsapi.Summary) error
	// Description produces a relevant string describing the Memory Threshold Notifier
	Description() string
}
