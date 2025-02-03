/*
Copyright 2015 The Kubernetes Authors.

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

package metrics

import (
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"

	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/features"
)

// This const block defines the metric names for the kubelet metrics.
const (
	FirstNetworkPodStartSLIDurationKey = "first_network_pod_start_sli_duration_seconds"
	KubeletSubsystem                   = "kubelet"
	DRASubsystem                       = "dra"
	NodeNameKey                        = "node_name"
	NodeLabelKey                       = "node"
	NodeStartupPreKubeletKey           = "node_startup_pre_kubelet_duration_seconds"
	NodeStartupPreRegistrationKey      = "node_startup_pre_registration_duration_seconds"
	NodeStartupRegistrationKey         = "node_startup_registration_duration_seconds"
	NodeStartupPostRegistrationKey     = "node_startup_post_registration_duration_seconds"
	NodeStartupKey                     = "node_startup_duration_seconds"
	PodWorkerDurationKey               = "pod_worker_duration_seconds"
	PodStartDurationKey                = "pod_start_duration_seconds"
	PodStartSLIDurationKey             = "pod_start_sli_duration_seconds"
	PodStartTotalDurationKey           = "pod_start_total_duration_seconds"
	CgroupManagerOperationsKey         = "cgroup_manager_duration_seconds"
	PodWorkerStartDurationKey          = "pod_worker_start_duration_seconds"
	PodStatusSyncDurationKey           = "pod_status_sync_duration_seconds"
	PLEGRelistDurationKey              = "pleg_relist_duration_seconds"
	PLEGDiscardEventsKey               = "pleg_discard_events"
	PLEGRelistIntervalKey              = "pleg_relist_interval_seconds"
	PLEGLastSeenKey                    = "pleg_last_seen_seconds"
	EventedPLEGConnErrKey              = "evented_pleg_connection_error_count"
	EventedPLEGConnKey                 = "evented_pleg_connection_success_count"
	EventedPLEGConnLatencyKey          = "evented_pleg_connection_latency_seconds"
	EvictionsKey                       = "evictions"
	EvictionStatsAgeKey                = "eviction_stats_age_seconds"
	PreemptionsKey                     = "preemptions"
	VolumeStatsCapacityBytesKey        = "volume_stats_capacity_bytes"
	VolumeStatsAvailableBytesKey       = "volume_stats_available_bytes"
	VolumeStatsUsedBytesKey            = "volume_stats_used_bytes"
	VolumeStatsInodesKey               = "volume_stats_inodes"
	VolumeStatsInodesFreeKey           = "volume_stats_inodes_free"
	VolumeStatsInodesUsedKey           = "volume_stats_inodes_used"
	VolumeStatsHealthStatusAbnormalKey = "volume_stats_health_status_abnormal"
	RunningPodsKey                     = "running_pods"
	RunningContainersKey               = "running_containers"
	DesiredPodCountKey                 = "desired_pods"
	ActivePodCountKey                  = "active_pods"
	MirrorPodCountKey                  = "mirror_pods"
	WorkingPodCountKey                 = "working_pods"
	OrphanedRuntimePodTotalKey         = "orphaned_runtime_pods_total"
	RestartedPodTotalKey               = "restarted_pods_total"
	ImagePullDurationKey               = "image_pull_duration_seconds"
	CgroupVersionKey                   = "cgroup_version"

	// Metrics keys of remote runtime operations
	RuntimeOperationsKey         = "runtime_operations_total"
	RuntimeOperationsDurationKey = "runtime_operations_duration_seconds"
	RuntimeOperationsErrorsKey   = "runtime_operations_errors_total"
	// Metrics keys of device plugin operations
	DevicePluginRegistrationCountKey  = "device_plugin_registration_total"
	DevicePluginAllocationDurationKey = "device_plugin_alloc_duration_seconds"
	// Metrics keys of pod resources operations
	PodResourcesEndpointRequestsTotalKey          = "pod_resources_endpoint_requests_total"
	PodResourcesEndpointRequestsListKey           = "pod_resources_endpoint_requests_list"
	PodResourcesEndpointRequestsGetAllocatableKey = "pod_resources_endpoint_requests_get_allocatable"
	PodResourcesEndpointErrorsListKey             = "pod_resources_endpoint_errors_list"
	PodResourcesEndpointErrorsGetAllocatableKey   = "pod_resources_endpoint_errors_get_allocatable"
	PodResourcesEndpointRequestsGetKey            = "pod_resources_endpoint_requests_get"
	PodResourcesEndpointErrorsGetKey              = "pod_resources_endpoint_errors_get"

	// Metrics keys for RuntimeClass
	RunPodSandboxDurationKey = "run_podsandbox_duration_seconds"
	RunPodSandboxErrorsKey   = "run_podsandbox_errors_total"

	// Metrics to keep track of total number of Pods and Containers started
	StartedPodsTotalKey             = "started_pods_total"
	StartedPodsErrorsTotalKey       = "started_pods_errors_total"
	StartedContainersTotalKey       = "started_containers_total"
	StartedContainersErrorsTotalKey = "started_containers_errors_total"

	// Metrics to track HostProcess container usage by this kubelet
	StartedHostProcessContainersTotalKey       = "started_host_process_containers_total"
	StartedHostProcessContainersErrorsTotalKey = "started_host_process_containers_errors_total"

	// Metrics to track ephemeral container usage by this kubelet
	ManagedEphemeralContainersKey = "managed_ephemeral_containers"

	// Metrics to track the CPU manager behavior
	CPUManagerPinningRequestsTotalKey         = "cpu_manager_pinning_requests_total"
	CPUManagerPinningErrorsTotalKey           = "cpu_manager_pinning_errors_total"
	CPUManagerSharedPoolSizeMilliCoresKey     = "cpu_manager_shared_pool_size_millicores"
	CPUManagerExclusiveCPUsAllocationCountKey = "cpu_manager_exclusive_cpu_allocation_count"

	// Metrics to track the Memory manager behavior
	MemoryManagerPinningRequestsTotalKey = "memory_manager_pinning_requests_total"
	MemoryManagerPinningErrorsTotalKey   = "memory_manager_pinning_errors_total"

	// Metrics to track the Topology manager behavior
	TopologyManagerAdmissionRequestsTotalKey = "topology_manager_admission_requests_total"
	TopologyManagerAdmissionErrorsTotalKey   = "topology_manager_admission_errors_total"
	TopologyManagerAdmissionDurationKey      = "topology_manager_admission_duration_ms"

	// Metrics to track orphan pod cleanup
	orphanPodCleanedVolumesKey       = "orphan_pod_cleaned_volumes"
	orphanPodCleanedVolumesErrorsKey = "orphan_pod_cleaned_volumes_errors"

	// Metric for tracking garbage collected images
	ImageGarbageCollectedTotalKey = "image_garbage_collected_total"

	// Metric for tracking aligment of compute resources
	ContainerAlignedComputeResourcesNameKey          = "container_aligned_compute_resources_count"
	ContainerAlignedComputeResourcesFailureNameKey   = "container_aligned_compute_resources_failure_count"
	ContainerAlignedComputeResourcesScopeLabelKey    = "scope"
	ContainerAlignedComputeResourcesBoundaryLabelKey = "boundary"

	// Metric keys for DRA operations
	DRAOperationsDurationKey     = "operations_duration_seconds"
	DRAGRPCOperationsDurationKey = "grpc_operations_duration_seconds"

	// Values used in metric labels
	Container          = "container"
	InitContainer      = "init_container"
	EphemeralContainer = "ephemeral_container"

	AlignScopePod       = "pod"
	AlignScopeContainer = "container"

	AlignedPhysicalCPU = "physical_cpu"
	AlignedNUMANode    = "numa_node"

	// Metrics to track kubelet admission rejections.
	AdmissionRejectionsTotalKey = "admission_rejections_total"
)

type imageSizeBucket struct {
	lowerBoundInBytes uint64
	label             string
}

var (
	podStartupDurationBuckets = []float64{0.5, 1, 2, 3, 4, 5, 6, 8, 10, 20, 30, 45, 60, 120, 180, 240, 300, 360, 480, 600, 900, 1200, 1800, 2700, 3600}
	imagePullDurationBuckets  = []float64{1, 5, 10, 20, 30, 60, 120, 180, 240, 300, 360, 480, 600, 900, 1200, 1800, 2700, 3600}
	// imageSizeBuckets has the labels to be associated with image_pull_duration_seconds metric. For example, if the size of
	// an image pulled is between 1GB and 5GB, the label will be "1GB-5GB".
	imageSizeBuckets = []imageSizeBucket{
		{0, "0-10MB"},
		{10 * 1024 * 1024, "10MB-100MB"},
		{100 * 1024 * 1024, "100MB-500MB"},
		{500 * 1024 * 1024, "500MB-1GB"},
		{1 * 1024 * 1024 * 1024, "1GB-5GB"},
		{5 * 1024 * 1024 * 1024, "5GB-10GB"},
		{10 * 1024 * 1024 * 1024, "10GB-20GB"},
		{20 * 1024 * 1024 * 1024, "20GB-30GB"},
		{30 * 1024 * 1024 * 1024, "30GB-40GB"},
		{40 * 1024 * 1024 * 1024, "40GB-60GB"},
		{60 * 1024 * 1024 * 1024, "60GB-100GB"},
		{100 * 1024 * 1024 * 1024, "GT100GB"},
	}
	// DRADurationBuckets is the bucket boundaries for DRA operation duration metrics
	// DRAOperationsDuration and DRAGRPCOperationsDuration defined below in this file.
	// The buckets max value 40 is based on the 45sec max gRPC timeout value defined
	// for the DRA gRPC calls in the pkg/kubelet/cm/dra/plugin/registration.go
	DRADurationBuckets = metrics.ExponentialBucketsRange(.1, 40, 15)
)

var (
	// NodeName is a Gauge that tracks the ode's name. The count is always 1.
	NodeName = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           NodeNameKey,
			Help:           "The node's name. The count is always 1.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{NodeLabelKey},
	)
	// ContainersPerPodCount is a Histogram that tracks the number of containers per pod.
	ContainersPerPodCount = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           "containers_per_pod_count",
			Help:           "The number of containers per pod.",
			Buckets:        metrics.ExponentialBuckets(1, 2, 5),
			StabilityLevel: metrics.ALPHA,
		},
	)
	// PodWorkerDuration is a Histogram that tracks the duration (in seconds) in takes to sync a single pod.
	// Broken down by the operation type.
	PodWorkerDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodWorkerDurationKey,
			Help:           "Duration in seconds to sync a single pod. Broken down by operation type: create, update, or sync",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation_type"},
	)
	// PodStartDuration is a Histogram that tracks the duration (in seconds) it takes for a single pod to run since it's
	// first time seen by kubelet.
	PodStartDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodStartDurationKey,
			Help:           "Duration in seconds from kubelet seeing a pod for the first time to the pod starting to run",
			Buckets:        podStartupDurationBuckets,
			StabilityLevel: metrics.ALPHA,
		},
	)
	// PodStartSLIDuration is a Histogram that tracks the duration (in seconds) it takes for a single pod to run,
	// excluding the time for image pulling. This metric should reflect the "Pod startup latency SLI" definition
	// ref: https://github.com/kubernetes/community/blob/master/sig-scalability/slos/pod_startup_latency.md
	//
	// The histogram bucket boundaries for pod startup latency metrics, measured in seconds. These are hand-picked
	// so as to be roughly exponential but still round numbers in everyday units. This is to minimise the number
	// of buckets while allowing accurate measurement of thresholds which might be used in SLOs
	// e.g. x% of pods start up within 30 seconds, or 15 minutes, etc.
	PodStartSLIDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodStartSLIDurationKey,
			Help:           "Duration in seconds to start a pod, excluding time to pull images and run init containers, measured from pod creation timestamp to when all its containers are reported as started and observed via watch",
			Buckets:        podStartupDurationBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{},
	)

	// PodStartTotalDuration is a Histogram that tracks the duration (in seconds) it takes for a single pod to run
	// since creation, including the time for image pulling.
	//
	// The histogram bucket boundaries for pod startup latency metrics, measured in seconds. These are hand-picked
	// so as to be roughly exponential but still round numbers in everyday units. This is to minimise the number
	// of buckets while allowing accurate measurement of thresholds which might be used in SLOs
	// e.g. x% of pods start up within 30 seconds, or 15 minutes, etc.
	PodStartTotalDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodStartTotalDurationKey,
			Help:           "Duration in seconds to start a pod since creation, including time to pull images and run init containers, measured from pod creation timestamp to when all its containers are reported as started and observed via watch",
			Buckets:        podStartupDurationBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{},
	)

	// FirstNetworkPodStartSLIDuration is a gauge that tracks the duration (in seconds) it takes for the first network pod to run,
	// excluding the time for image pulling. This is an internal and temporary metric required because of the existing limitations of the
	// existing networking subsystem and CRI/CNI implementations that will be solved by https://github.com/containernetworking/cni/issues/859
	// The metric represents the latency observed by an user to run workloads in a new node.
	// ref: https://github.com/kubernetes/community/blob/master/sig-scalability/slos/pod_startup_latency.md
	FirstNetworkPodStartSLIDuration = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           FirstNetworkPodStartSLIDurationKey,
			Help:           "Duration in seconds to start the first network pod, excluding time to pull images and run init containers, measured from pod creation timestamp to when all its containers are reported as started and observed via watch",
			StabilityLevel: metrics.INTERNAL,
		},
	)

	// CgroupManagerDuration is a Histogram that tracks the duration (in seconds) it takes for cgroup manager operations to complete.
	// Broken down by method.
	CgroupManagerDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           CgroupManagerOperationsKey,
			Help:           "Duration in seconds for cgroup manager operations. Broken down by method.",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation_type"},
	)
	// PodWorkerStartDuration is a Histogram that tracks the duration (in seconds) it takes from kubelet seeing a pod to starting a worker.
	PodWorkerStartDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodWorkerStartDurationKey,
			Help:           "Duration in seconds from kubelet seeing a pod to starting a worker.",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
	)
	// PodStatusSyncDuration is a Histogram that tracks the duration (in seconds) in takes from the time a pod
	// status is generated to the time it is synced with the apiserver. If multiple status changes are generated
	// on a pod before it is written to the API, the latency is from the first update to the last event.
	PodStatusSyncDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodStatusSyncDurationKey,
			Help:           "Duration in seconds to sync a pod status update. Measures time from detection of a change to pod status until the API is successfully updated for that pod, even if multiple intevening changes to pod status occur.",
			Buckets:        []float64{0.010, 0.050, 0.100, 0.500, 1, 5, 10, 20, 30, 45, 60},
			StabilityLevel: metrics.ALPHA,
		},
	)
	// PLEGRelistDuration is a Histogram that tracks the duration (in seconds) it takes for relisting pods in the Kubelet's
	// Pod Lifecycle Event Generator (PLEG).
	PLEGRelistDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PLEGRelistDurationKey,
			Help:           "Duration in seconds for relisting pods in PLEG.",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
	)
	// PLEGDiscardEvents is a Counter that tracks the number of discarding events in the Kubelet's Pod Lifecycle Event Generator (PLEG).
	PLEGDiscardEvents = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PLEGDiscardEventsKey,
			Help:           "The number of discard events in PLEG.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// PLEGRelistInterval is a Histogram that tracks the intervals (in seconds) between relisting in the Kubelet's
	// Pod Lifecycle Event Generator (PLEG).
	PLEGRelistInterval = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PLEGRelistIntervalKey,
			Help:           "Interval in seconds between relisting in PLEG.",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
	)
	// PLEGLastSeen is a Gauge giving the Unix timestamp when the Kubelet's
	// Pod Lifecycle Event Generator (PLEG) was last seen active.
	PLEGLastSeen = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PLEGLastSeenKey,
			Help:           "Timestamp in seconds when PLEG was last seen active.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// EventedPLEGConnErr is a Counter that tracks the number of errors encountered during
	// the establishment of streaming connection with the CRI runtime.
	EventedPLEGConnErr = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           EventedPLEGConnErrKey,
			Help:           "The number of errors encountered during the establishment of streaming connection with the CRI runtime.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// EventedPLEGConn is a Counter that tracks the number of times a streaming client
	// was obtained to receive CRI Events.
	EventedPLEGConn = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           EventedPLEGConnKey,
			Help:           "The number of times a streaming client was obtained to receive CRI Events.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// EventedPLEGConnLatency is a Histogram that tracks the latency of streaming connection
	// with the CRI runtime, measured in seconds.
	EventedPLEGConnLatency = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           EventedPLEGConnLatencyKey,
			Help:           "The latency of streaming connection with the CRI runtime, measured in seconds.",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
	)

	// RuntimeOperations is a Counter that tracks the cumulative number of remote runtime operations.
	// Broken down by operation type.
	RuntimeOperations = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           RuntimeOperationsKey,
			Help:           "Cumulative number of runtime operations by operation type.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation_type"},
	)
	// RuntimeOperationsDuration is a Histogram that tracks the duration (in seconds) for remote runtime operations to complete.
	// Broken down by operation type.
	RuntimeOperationsDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           RuntimeOperationsDurationKey,
			Help:           "Duration in seconds of runtime operations. Broken down by operation type.",
			Buckets:        metrics.ExponentialBuckets(.005, 2.5, 14),
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation_type"},
	)
	// RuntimeOperationsErrors is a Counter that tracks the cumulative number of remote runtime operations errors.
	// Broken down by operation type.
	RuntimeOperationsErrors = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           RuntimeOperationsErrorsKey,
			Help:           "Cumulative number of runtime operation errors by operation type.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation_type"},
	)
	// Evictions is a Counter that tracks the cumulative number of pod evictions initiated by the kubelet.
	// Broken down by eviction signal.
	Evictions = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           EvictionsKey,
			Help:           "Cumulative number of pod evictions by eviction signal",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"eviction_signal"},
	)
	// EvictionStatsAge is a Histogram that tracks the time (in seconds) between when stats are collected and when a pod is evicted
	// based on those stats. Broken down by eviction signal.
	EvictionStatsAge = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           EvictionStatsAgeKey,
			Help:           "Time between when stats are collected, and when pod is evicted based on those stats by eviction signal",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"eviction_signal"},
	)
	// Preemptions is a Counter that tracks the cumulative number of pod preemptions initiated by the kubelet.
	// Broken down by preemption signal. A preemption is only recorded for one resource, the sum of all signals
	// is the number of preemptions on the given node.
	Preemptions = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PreemptionsKey,
			Help:           "Cumulative number of pod preemptions by preemption resource",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"preemption_signal"},
	)
	// DevicePluginRegistrationCount is a Counter that tracks the cumulative number of device plugin registrations.
	// Broken down by resource name.
	DevicePluginRegistrationCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           DevicePluginRegistrationCountKey,
			Help:           "Cumulative number of device plugin registrations. Broken down by resource name.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource_name"},
	)
	// DevicePluginAllocationDuration is a Histogram that tracks the duration (in seconds) to serve a device plugin allocation request.
	// Broken down by resource name.
	DevicePluginAllocationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           DevicePluginAllocationDurationKey,
			Help:           "Duration in seconds to serve a device plugin Allocation request. Broken down by resource name.",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource_name"},
	)

	// PodResourcesEndpointRequestsTotalCount is a Counter that tracks the cumulative number of requests to the PodResource endpoints.
	// Broken down by server API version.
	PodResourcesEndpointRequestsTotalCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodResourcesEndpointRequestsTotalKey,
			Help:           "Cumulative number of requests to the PodResource endpoint. Broken down by server api version.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"server_api_version"},
	)

	// PodResourcesEndpointRequestsListCount is a Counter that tracks the number of requests to the PodResource List() endpoint.
	// Broken down by server API version.
	PodResourcesEndpointRequestsListCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodResourcesEndpointRequestsListKey,
			Help:           "Number of requests to the PodResource List endpoint. Broken down by server api version.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"server_api_version"},
	)

	// PodResourcesEndpointRequestsGetAllocatableCount is a Counter that tracks the number of requests to the PodResource GetAllocatableResources() endpoint.
	// Broken down by server API version.
	PodResourcesEndpointRequestsGetAllocatableCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodResourcesEndpointRequestsGetAllocatableKey,
			Help:           "Number of requests to the PodResource GetAllocatableResources endpoint. Broken down by server api version.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"server_api_version"},
	)

	// PodResourcesEndpointErrorsListCount is a Counter that tracks the number of errors returned by he PodResource List() endpoint.
	// Broken down by server API version.
	PodResourcesEndpointErrorsListCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodResourcesEndpointErrorsListKey,
			Help:           "Number of requests to the PodResource List endpoint which returned error. Broken down by server api version.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"server_api_version"},
	)

	// PodResourcesEndpointErrorsGetAllocatableCount is a Counter that tracks the number of errors returned by the PodResource GetAllocatableResources() endpoint.
	// Broken down by server API version.
	PodResourcesEndpointErrorsGetAllocatableCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodResourcesEndpointErrorsGetAllocatableKey,
			Help:           "Number of requests to the PodResource GetAllocatableResources endpoint which returned error. Broken down by server api version.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"server_api_version"},
	)

	// PodResourcesEndpointRequestsGetCount is a Counter that tracks the number of requests to the PodResource Get() endpoint.
	// Broken down by server API version.
	PodResourcesEndpointRequestsGetCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodResourcesEndpointRequestsGetKey,
			Help:           "Number of requests to the PodResource Get endpoint. Broken down by server api version.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"server_api_version"},
	)

	// PodResourcesEndpointErrorsGetCount is a Counter that tracks the number of errors returned by he PodResource List() endpoint.
	// Broken down by server API version.
	PodResourcesEndpointErrorsGetCount = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           PodResourcesEndpointErrorsGetKey,
			Help:           "Number of requests to the PodResource Get endpoint which returned error. Broken down by server api version.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"server_api_version"},
	)

	// RunPodSandboxDuration is a Histogram that tracks the duration (in seconds) it takes to run Pod Sandbox operations.
	// Broken down by RuntimeClass.Handler.
	RunPodSandboxDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem: KubeletSubsystem,
			Name:      RunPodSandboxDurationKey,
			Help:      "Duration in seconds of the run_podsandbox operations. Broken down by RuntimeClass.Handler.",
			// Use DefBuckets for now, will customize the buckets if necessary.
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"runtime_handler"},
	)
	// RunPodSandboxErrors is a Counter that tracks the cumulative number of Pod Sandbox operations errors.
	// Broken down by RuntimeClass.Handler.
	RunPodSandboxErrors = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           RunPodSandboxErrorsKey,
			Help:           "Cumulative number of the run_podsandbox operation errors by RuntimeClass.Handler.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"runtime_handler"},
	)

	// RunningPodCount is a gauge that tracks the number of Pods currently with a running sandbox
	// It is used to expose the kubelet internal state: how many pods have running containers in the container runtime, and mainly for debugging purpose.
	RunningPodCount = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           RunningPodsKey,
			Help:           "Number of pods that have a running pod sandbox",
			StabilityLevel: metrics.ALPHA,
		},
	)
	// RunningContainerCount is a gauge that tracks the number of containers currently running
	RunningContainerCount = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           RunningContainersKey,
			Help:           "Number of containers currently running",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"container_state"},
	)
	// DesiredPodCount tracks the count of pods the Kubelet thinks it should be running
	DesiredPodCount = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           DesiredPodCountKey,
			Help:           "The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"static"},
	)
	// ActivePodCount tracks the count of pods the Kubelet considers as active when deciding to admit a new pod
	ActivePodCount = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           ActivePodCountKey,
			Help:           "The number of pods the kubelet considers active and which are being considered when admitting new pods. static is true if the pod is not from the apiserver.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"static"},
	)
	// MirrorPodCount tracks the number of mirror pods the Kubelet should have created for static pods
	MirrorPodCount = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           MirrorPodCountKey,
			Help:           "The number of mirror pods the kubelet will try to create (one per admitted static pod)",
			StabilityLevel: metrics.ALPHA,
		},
	)
	// WorkingPodCount tracks the count of pods in each lifecycle phase, whether they are static pods, and whether they are desired, orphaned, or runtime_only
	WorkingPodCount = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           WorkingPodCountKey,
			Help:           "Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"lifecycle", "config", "static"},
	)
	// OrphanedRuntimePodTotal is incremented every time a pod is detected in the runtime without being known to the pod worker first
	OrphanedRuntimePodTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           OrphanedRuntimePodTotalKey,
			Help:           "Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.",
			StabilityLevel: metrics.ALPHA,
		},
	)
	// RestartedPodTotal is incremented every time a pod with the same UID is deleted and recreated
	RestartedPodTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           RestartedPodTotalKey,
			Help:           "Number of pods that have been restarted because they were deleted and recreated with the same UID while the kubelet was watching them (common for static pods, extremely uncommon for API pods)",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"static"},
	)
	// StartedPodsTotal is a counter that tracks pod sandbox creation operations
	StartedPodsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           StartedPodsTotalKey,
			Help:           "Cumulative number of pods started",
			StabilityLevel: metrics.ALPHA,
		},
	)
	// StartedPodsErrorsTotal is a counter that tracks the number of errors creating pod sandboxes
	StartedPodsErrorsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           StartedPodsErrorsTotalKey,
			Help:           "Cumulative number of errors when starting pods",
			StabilityLevel: metrics.ALPHA,
		},
	)
	// StartedContainersTotal is a counter that tracks the number of container creation operations
	StartedContainersTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           StartedContainersTotalKey,
			Help:           "Cumulative number of containers started",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"container_type"},
	)
	// StartedContainersTotal is a counter that tracks the number of errors creating containers
	StartedContainersErrorsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           StartedContainersErrorsTotalKey,
			Help:           "Cumulative number of errors when starting containers",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"container_type", "code"},
	)
	// StartedHostProcessContainersTotal is a counter that tracks the number of hostprocess container creation operations
	StartedHostProcessContainersTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           StartedHostProcessContainersTotalKey,
			Help:           "Cumulative number of hostprocess containers started. This metric will only be collected on Windows.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"container_type"},
	)
	// StartedHostProcessContainersErrorsTotal is a counter that tracks the number of errors creating hostprocess containers
	StartedHostProcessContainersErrorsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           StartedHostProcessContainersErrorsTotalKey,
			Help:           "Cumulative number of errors when starting hostprocess containers. This metric will only be collected on Windows.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"container_type", "code"},
	)
	// ManagedEphemeralContainers is a gauge that indicates how many ephemeral containers are managed by this kubelet.
	ManagedEphemeralContainers = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           ManagedEphemeralContainersKey,
			Help:           "Current number of ephemeral containers in pods managed by this kubelet.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// GracefulShutdownStartTime is a gauge that records the time at which the kubelet started graceful shutdown.
	GracefulShutdownStartTime = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           "graceful_shutdown_start_time_seconds",
			Help:           "Last graceful shutdown start time since unix epoch in seconds",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// GracefulShutdownEndTime is a gauge that records the time at which the kubelet completed graceful shutdown.
	GracefulShutdownEndTime = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           "graceful_shutdown_end_time_seconds",
			Help:           "Last graceful shutdown end time since unix epoch in seconds",
			StabilityLevel: metrics.ALPHA,
		},
	)

	LifecycleHandlerHTTPFallbacks = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           "lifecycle_handler_http_fallbacks_total",
			Help:           "The number of times lifecycle handlers successfully fell back to http from https.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// CPUManagerPinningRequestsTotal tracks the number of times the pod spec will cause the cpu manager to pin cores
	CPUManagerPinningRequestsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           CPUManagerPinningRequestsTotalKey,
			Help:           "The number of cpu core allocations which required pinning.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// CPUManagerPinningErrorsTotal tracks the number of times the pod spec required the cpu manager to pin cores, but the allocation failed
	CPUManagerPinningErrorsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           CPUManagerPinningErrorsTotalKey,
			Help:           "The number of cpu core allocations which required pinning failed.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// CPUManagerSharedPoolSizeMilliCores reports the current size of the shared CPU pool for non-guaranteed pods
	CPUManagerSharedPoolSizeMilliCores = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           CPUManagerSharedPoolSizeMilliCoresKey,
			Help:           "The size of the shared CPU pool for non-guaranteed QoS pods, in millicores.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// CPUManagerExclusiveCPUsAllocationCount reports the total number of CPUs exclusively allocated to containers running on this node
	CPUManagerExclusiveCPUsAllocationCount = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           CPUManagerExclusiveCPUsAllocationCountKey,
			Help:           "The total number of CPUs exclusively allocated to containers running on this node",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// ContainerAlignedComputeResources reports the count of resources allocation which granted aligned resources, per alignment boundary
	ContainerAlignedComputeResources = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           ContainerAlignedComputeResourcesNameKey,
			Help:           "Cumulative number of aligned compute resources allocated to containers by alignment type.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{ContainerAlignedComputeResourcesScopeLabelKey, ContainerAlignedComputeResourcesBoundaryLabelKey},
	)

	// ContainerAlignedComputeResourcesFailure reports the count of resources allocation attempts which failed to align resources, per alignment boundary
	ContainerAlignedComputeResourcesFailure = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           ContainerAlignedComputeResourcesFailureNameKey,
			Help:           "Cumulative number of failures to allocate aligned compute resources to containers by alignment type.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{ContainerAlignedComputeResourcesScopeLabelKey, ContainerAlignedComputeResourcesBoundaryLabelKey},
	)

	MemoryManagerPinningRequestTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           MemoryManagerPinningRequestsTotalKey,
			Help:           "The number of memory pages allocations which required pinning.",
			StabilityLevel: metrics.ALPHA,
		})

	// MemoryManagerPinningErrorsTotal tracks the number of times the pod spec required the memory manager to pin memory pages, but the allocation failed
	MemoryManagerPinningErrorsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           MemoryManagerPinningErrorsTotalKey,
			Help:           "The number of memory pages allocations which required pinning that failed.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// TopologyManagerAdmissionRequestsTotal tracks the number of times the pod spec will cause the topology manager to admit a pod
	TopologyManagerAdmissionRequestsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           TopologyManagerAdmissionRequestsTotalKey,
			Help:           "The number of admission requests where resources have to be aligned.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// TopologyManagerAdmissionErrorsTotal tracks the number of times the pod spec required the topology manager to admit a pod, but the admission failed
	TopologyManagerAdmissionErrorsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           TopologyManagerAdmissionErrorsTotalKey,
			Help:           "The number of admission request failures where resources could not be aligned.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// TopologyManagerAdmissionDuration is a Histogram that tracks the duration (in seconds) to serve a pod admission request.
	TopologyManagerAdmissionDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           TopologyManagerAdmissionDurationKey,
			Help:           "Duration in milliseconds to serve a pod admission request.",
			Buckets:        metrics.ExponentialBuckets(.05, 2, 15),
			StabilityLevel: metrics.ALPHA,
		},
	)

	// OrphanPodCleanedVolumes is number of orphaned Pods that times that removeOrphanedPodVolumeDirs was called during the last sweep.
	OrphanPodCleanedVolumes = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           orphanPodCleanedVolumesKey,
			Help:           "The total number of orphaned Pods whose volumes were cleaned in the last periodic sweep.",
			StabilityLevel: metrics.ALPHA,
		},
	)
	// OrphanPodCleanedVolumes is number of times that removeOrphanedPodVolumeDirs failed.
	OrphanPodCleanedVolumesErrors = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           orphanPodCleanedVolumesErrorsKey,
			Help:           "The number of orphaned Pods whose volumes failed to be cleaned in the last periodic sweep.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	NodeStartupPreKubeletDuration = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           NodeStartupPreKubeletKey,
			Help:           "Duration in seconds of node startup before kubelet starts.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	NodeStartupPreRegistrationDuration = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           NodeStartupPreRegistrationKey,
			Help:           "Duration in seconds of node startup before registration.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	NodeStartupRegistrationDuration = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           NodeStartupRegistrationKey,
			Help:           "Duration in seconds of node startup during registration.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	NodeStartupPostRegistrationDuration = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           NodeStartupPostRegistrationKey,
			Help:           "Duration in seconds of node startup after registration.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	NodeStartupDuration = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           NodeStartupKey,
			Help:           "Duration in seconds of node startup in total.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	ImageGarbageCollectedTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           ImageGarbageCollectedTotalKey,
			Help:           "Total number of images garbage collected by the kubelet, whether through disk usage or image age.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"reason"},
	)

	// ImagePullDuration is a Histogram that tracks the duration (in seconds) it takes for an image to be pulled,
	// including the time spent in the waiting queue of image puller.
	// The metric is broken down by bucketed image size.
	ImagePullDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      KubeletSubsystem,
			Name:           ImagePullDurationKey,
			Help:           "Duration in seconds to pull an image.",
			Buckets:        imagePullDurationBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"image_size_in_bytes"},
	)

	LifecycleHandlerSleepTerminated = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           "sleep_action_terminated_early_total",
			Help:           "The number of times lifecycle sleep handler got terminated before it finishes",
			StabilityLevel: metrics.ALPHA,
		},
	)

	CgroupVersion = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      KubeletSubsystem,
			Name:           CgroupVersionKey,
			Help:           "cgroup version on the hosts.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// DRAOperationsDuration tracks the duration of the DRA PrepareResources and UnprepareResources requests.
	DRAOperationsDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      DRASubsystem,
			Name:           DRAOperationsDurationKey,
			Help:           "Latency histogram in seconds for the duration of handling all ResourceClaims referenced by a pod when the pod starts or stops. Identified by the name of the operation (PrepareResources or UnprepareResources) and separated by the success of the operation. The number of failed operations is provided through the histogram's overall count.",
			Buckets:        DRADurationBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation_name", "is_error"},
	)

	// DRAGRPCOperationsDuration tracks the duration of the DRA GRPC operations.
	DRAGRPCOperationsDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      DRASubsystem,
			Name:           DRAGRPCOperationsDurationKey,
			Help:           "Duration in seconds of the DRA gRPC operations",
			Buckets:        DRADurationBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"driver_name", "method_name", "grpc_status_code"},
	)

	// AdmissionRejectionsTotal tracks the number of failed admission times, currently, just record it for pod additions
	AdmissionRejectionsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      KubeletSubsystem,
			Name:           AdmissionRejectionsTotalKey,
			Help:           "Cumulative number pod admission rejections by the Kubelet.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"reason"},
	)
)

var registerMetrics sync.Once

// Register registers all metrics.
func Register(collectors ...metrics.StableCollector) {
	// Register the metrics.
	registerMetrics.Do(func() {
		legacyregistry.MustRegister(FirstNetworkPodStartSLIDuration)
		legacyregistry.MustRegister(NodeName)
		legacyregistry.MustRegister(PodWorkerDuration)
		legacyregistry.MustRegister(PodStartDuration)
		legacyregistry.MustRegister(PodStartSLIDuration)
		legacyregistry.MustRegister(PodStartTotalDuration)
		legacyregistry.MustRegister(ImagePullDuration)
		legacyregistry.MustRegister(ImageGarbageCollectedTotal)
		legacyregistry.MustRegister(NodeStartupPreKubeletDuration)
		legacyregistry.MustRegister(NodeStartupPreRegistrationDuration)
		legacyregistry.MustRegister(NodeStartupRegistrationDuration)
		legacyregistry.MustRegister(NodeStartupPostRegistrationDuration)
		legacyregistry.MustRegister(NodeStartupDuration)
		legacyregistry.MustRegister(CgroupManagerDuration)
		legacyregistry.MustRegister(PodWorkerStartDuration)
		legacyregistry.MustRegister(PodStatusSyncDuration)
		legacyregistry.MustRegister(ContainersPerPodCount)
		legacyregistry.MustRegister(PLEGRelistDuration)
		legacyregistry.MustRegister(PLEGDiscardEvents)
		legacyregistry.MustRegister(PLEGRelistInterval)
		legacyregistry.MustRegister(PLEGLastSeen)
		legacyregistry.MustRegister(EventedPLEGConnErr)
		legacyregistry.MustRegister(EventedPLEGConn)
		legacyregistry.MustRegister(EventedPLEGConnLatency)
		legacyregistry.MustRegister(RuntimeOperations)
		legacyregistry.MustRegister(RuntimeOperationsDuration)
		legacyregistry.MustRegister(RuntimeOperationsErrors)
		legacyregistry.MustRegister(Evictions)
		legacyregistry.MustRegister(EvictionStatsAge)
		legacyregistry.MustRegister(Preemptions)
		legacyregistry.MustRegister(DevicePluginRegistrationCount)
		legacyregistry.MustRegister(DevicePluginAllocationDuration)
		legacyregistry.MustRegister(RunningContainerCount)
		legacyregistry.MustRegister(RunningPodCount)
		legacyregistry.MustRegister(DesiredPodCount)
		legacyregistry.MustRegister(ActivePodCount)
		legacyregistry.MustRegister(MirrorPodCount)
		legacyregistry.MustRegister(WorkingPodCount)
		legacyregistry.MustRegister(OrphanedRuntimePodTotal)
		legacyregistry.MustRegister(RestartedPodTotal)
		legacyregistry.MustRegister(ManagedEphemeralContainers)
		legacyregistry.MustRegister(PodResourcesEndpointRequestsTotalCount)
		legacyregistry.MustRegister(PodResourcesEndpointRequestsListCount)
		legacyregistry.MustRegister(PodResourcesEndpointRequestsGetAllocatableCount)
		legacyregistry.MustRegister(PodResourcesEndpointErrorsListCount)
		legacyregistry.MustRegister(PodResourcesEndpointErrorsGetAllocatableCount)
		if utilfeature.DefaultFeatureGate.Enabled(features.KubeletPodResourcesGet) {
			legacyregistry.MustRegister(PodResourcesEndpointRequestsGetCount)
			legacyregistry.MustRegister(PodResourcesEndpointErrorsGetCount)
		}
		legacyregistry.MustRegister(StartedPodsTotal)
		legacyregistry.MustRegister(StartedPodsErrorsTotal)
		legacyregistry.MustRegister(StartedContainersTotal)
		legacyregistry.MustRegister(StartedContainersErrorsTotal)
		legacyregistry.MustRegister(StartedHostProcessContainersTotal)
		legacyregistry.MustRegister(StartedHostProcessContainersErrorsTotal)
		legacyregistry.MustRegister(RunPodSandboxDuration)
		legacyregistry.MustRegister(RunPodSandboxErrors)
		legacyregistry.MustRegister(CPUManagerPinningRequestsTotal)
		legacyregistry.MustRegister(CPUManagerPinningErrorsTotal)
		legacyregistry.MustRegister(CPUManagerSharedPoolSizeMilliCores)
		legacyregistry.MustRegister(CPUManagerExclusiveCPUsAllocationCount)
		legacyregistry.MustRegister(ContainerAlignedComputeResources)
		legacyregistry.MustRegister(ContainerAlignedComputeResourcesFailure)
		legacyregistry.MustRegister(MemoryManagerPinningRequestTotal)
		legacyregistry.MustRegister(MemoryManagerPinningErrorsTotal)
		legacyregistry.MustRegister(TopologyManagerAdmissionRequestsTotal)
		legacyregistry.MustRegister(TopologyManagerAdmissionErrorsTotal)
		legacyregistry.MustRegister(TopologyManagerAdmissionDuration)
		legacyregistry.MustRegister(OrphanPodCleanedVolumes)
		legacyregistry.MustRegister(OrphanPodCleanedVolumesErrors)

		for _, collector := range collectors {
			legacyregistry.CustomMustRegister(collector)
		}

		if utilfeature.DefaultFeatureGate.Enabled(features.GracefulNodeShutdown) &&
			utilfeature.DefaultFeatureGate.Enabled(features.GracefulNodeShutdownBasedOnPodPriority) {
			legacyregistry.MustRegister(GracefulShutdownStartTime)
			legacyregistry.MustRegister(GracefulShutdownEndTime)
		}

		legacyregistry.MustRegister(LifecycleHandlerHTTPFallbacks)
		legacyregistry.MustRegister(LifecycleHandlerSleepTerminated)
		legacyregistry.MustRegister(CgroupVersion)

		if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
			legacyregistry.MustRegister(DRAOperationsDuration)
			legacyregistry.MustRegister(DRAGRPCOperationsDuration)
		}

		legacyregistry.MustRegister(AdmissionRejectionsTotal)
	})
}

// GetGather returns the gatherer. It used by test case outside current package.
func GetGather() metrics.Gatherer {
	return legacyregistry.DefaultGatherer
}

// SinceInSeconds gets the time since the specified start in seconds.
func SinceInSeconds(start time.Time) float64 {
	return time.Since(start).Seconds()
}

// SetNodeName sets the NodeName Gauge to 1.
func SetNodeName(name types.NodeName) {
	NodeName.WithLabelValues(string(name)).Set(1)
}

func GetImageSizeBucket(sizeInBytes uint64) string {
	if sizeInBytes == 0 {
		return "N/A"
	}

	for i := len(imageSizeBuckets) - 1; i >= 0; i-- {
		if sizeInBytes > imageSizeBuckets[i].lowerBoundInBytes {
			return imageSizeBuckets[i].label
		}
	}

	// return empty string when sizeInBytes is 0 (error getting image size)
	return ""
}
