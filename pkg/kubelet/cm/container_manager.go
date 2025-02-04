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

//go:generate mockery
package cm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	// TODO: Migrate kubelet to either use its own internal objects or client library.
	v1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/healthz"
	internalapi "k8s.io/cri-api/pkg/apis"
	"k8s.io/klog/v2"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/apis/podresources"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/resourceupdates"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/status"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/utils/cpuset"
)

const (
	// Warning message for the users still using cgroup v1
	CgroupV1MaintenanceModeWarning = "cgroup v1 support is in maintenance mode, please migrate to cgroup v2"

	// Warning message for the users using cgroup v2 on kernel doesn't support root `cpu.stat`.
	// `cpu.stat` was added to root cgroup in kernel 5.8.
	// (ref: https://github.com/torvalds/linux/commit/936f2a70f2077f64fab1dcb3eca71879e82ecd3f)
	CgroupV2KernelWarning = "cgroup v2 is being used on a kernel, which doesn't support root `cpu.stat`." +
		"Kubelet will continue, but may experience instability or wrong behavior"
)

type ActivePodsFunc func() []*v1.Pod

type GetNodeFunc func() (*v1.Node, error)

// Manages the containers running on a machine.
type ContainerManager interface {
	// Runs the container manager's housekeeping.
	// - Ensures that the Docker daemon is in a container.
	// - Creates the system container where all non-containerized processes run.
	Start(context.Context, *v1.Node, ActivePodsFunc, GetNodeFunc, config.SourcesReady, status.PodStatusProvider, internalapi.RuntimeService, bool) error

	// SystemCgroupsLimit returns resources allocated to system cgroups in the machine.
	// These cgroups include the system and Kubernetes services.
	SystemCgroupsLimit() v1.ResourceList

	// GetNodeConfig returns a NodeConfig that is being used by the container manager.
	GetNodeConfig() NodeConfig

	// Status returns internal Status.
	Status() Status

	// NewPodContainerManager is a factory method which returns a podContainerManager object
	// Returns a noop implementation if qos cgroup hierarchy is not enabled
	NewPodContainerManager() PodContainerManager

	// GetMountedSubsystems returns the mounted cgroup subsystems on the node
	GetMountedSubsystems() *CgroupSubsystems

	// GetQOSContainersInfo returns the names of top level QoS containers
	GetQOSContainersInfo() QOSContainersInfo

	// GetNodeAllocatableReservation returns the amount of compute resources that have to be reserved from scheduling.
	GetNodeAllocatableReservation() v1.ResourceList

	// GetCapacity returns the amount of compute resources tracked by container manager available on the node.
	GetCapacity(localStorageCapacityIsolation bool) v1.ResourceList

	// GetDevicePluginResourceCapacity returns the node capacity (amount of total device plugin resources),
	// node allocatable (amount of total healthy resources reported by device plugin),
	// and inactive device plugin resources previously registered on the node.
	GetDevicePluginResourceCapacity() (v1.ResourceList, v1.ResourceList, []string)

	// UpdateQOSCgroups performs housekeeping updates to ensure that the top
	// level QoS containers have their desired state in a thread-safe way
	UpdateQOSCgroups() error

	// GetResources returns RunContainerOptions with devices, mounts, and env fields populated for
	// extended resources required by container.
	GetResources(ctx context.Context, pod *v1.Pod, container *v1.Container) (*kubecontainer.RunContainerOptions, error)

	// UpdatePluginResources calls Allocate of device plugin handler for potential
	// requests for device plugin resources, and returns an error if fails.
	// Otherwise, it updates allocatableResource in nodeInfo if necessary,
	// to make sure it is at least equal to the pod's requested capacity for
	// any registered device plugin resource
	UpdatePluginResources(*schedulerframework.NodeInfo, *lifecycle.PodAdmitAttributes) error

	InternalContainerLifecycle() InternalContainerLifecycle

	// GetPodCgroupRoot returns the cgroup which contains all pods.
	GetPodCgroupRoot() string

	// GetPluginRegistrationHandlers returns a set of plugin registration handlers
	// The pluginwatcher's Handlers allow to have a single module for handling
	// registration.
	GetPluginRegistrationHandlers() map[string]cache.PluginHandler

	// GetHealthCheckers returns a set of health checkers for all plugins.
	// These checkers are integrated into the systemd watchdog to monitor the service's health.
	GetHealthCheckers() []healthz.HealthChecker

	// ShouldResetExtendedResourceCapacity returns whether or not the extended resources should be zeroed,
	// due to node recreation.
	ShouldResetExtendedResourceCapacity() bool

	// GetAllocateResourcesPodAdmitHandler returns an instance of a PodAdmitHandler responsible for allocating pod resources.
	GetAllocateResourcesPodAdmitHandler() lifecycle.PodAdmitHandler

	// GetNodeAllocatableAbsolute returns the absolute value of Node Allocatable which is primarily useful for enforcement.
	GetNodeAllocatableAbsolute() v1.ResourceList

	// PrepareDynamicResource prepares dynamic pod resources
	PrepareDynamicResources(context.Context, *v1.Pod) error

	// UnprepareDynamicResources unprepares dynamic pod resources
	UnprepareDynamicResources(context.Context, *v1.Pod) error

	// PodMightNeedToUnprepareResources returns true if the pod with the given UID
	// might need to unprepare resources.
	PodMightNeedToUnprepareResources(UID types.UID) bool

	// UpdateAllocatedResourcesStatus updates the status of allocated resources for the pod.
	UpdateAllocatedResourcesStatus(pod *v1.Pod, status *v1.PodStatus)

	// Updates returns a channel that receives an Update when the device changed its status.
	Updates() <-chan resourceupdates.Update

	// PodHasExclusiveCPUs returns true if the provided pod has containers with exclusive CPUs,
	// This means that at least one sidecar container or one app container has exclusive CPUs allocated.
	PodHasExclusiveCPUs(pod *v1.Pod) bool

	// ContainerHasExclusiveCPUs returns true if the provided container in the pod has exclusive cpu
	ContainerHasExclusiveCPUs(pod *v1.Pod, container *v1.Container) bool

	// Implements the PodResources Provider API
	podresources.CPUsProvider
	podresources.DevicesProvider
	podresources.MemoryProvider
	podresources.DynamicResourcesProvider
}

type cpuAllocationReader interface {
	GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet
}

type NodeConfig struct {
	NodeName              types.NodeName
	RuntimeCgroupsName    string
	SystemCgroupsName     string
	KubeletCgroupsName    string
	KubeletOOMScoreAdj    int32
	ContainerRuntime      string
	CgroupsPerQOS         bool
	CgroupRoot            string
	CgroupDriver          string
	KubeletRootDir        string
	ProtectKernelDefaults bool
	NodeAllocatableConfig
	QOSReserved                  map[v1.ResourceName]int64
	CPUManagerPolicy             string
	CPUManagerPolicyOptions      map[string]string
	TopologyManagerScope         string
	CPUManagerReconcilePeriod    time.Duration
	MemoryManagerPolicy          string
	MemoryManagerReservedMemory  []kubeletconfig.MemoryReservation
	PodPidsLimit                 int64
	EnforceCPULimits             bool
	CPUCFSQuotaPeriod            time.Duration
	TopologyManagerPolicy        string
	TopologyManagerPolicyOptions map[string]string
	CgroupVersion                int
}

type NodeAllocatableConfig struct {
	KubeReservedCgroupName   string
	SystemReservedCgroupName string
	ReservedSystemCPUs       cpuset.CPUSet
	EnforceNodeAllocatable   sets.Set[string]
	KubeReserved             v1.ResourceList
	SystemReserved           v1.ResourceList
	HardEvictionThresholds   []evictionapi.Threshold
}

type Status struct {
	// Any soft requirements that were unsatisfied.
	SoftRequirements error
}

func int64Slice(in []int) []int64 {
	out := make([]int64, len(in))
	for i := range in {
		out[i] = int64(in[i])
	}
	return out
}

func podHasExclusiveCPUs(cr cpuAllocationReader, pod *v1.Pod) bool {
	for _, container := range pod.Spec.InitContainers {
		if containerHasExclusiveCPUs(cr, pod, &container) {
			return true
		}
	}
	for _, container := range pod.Spec.Containers {
		if containerHasExclusiveCPUs(cr, pod, &container) {
			return true
		}
	}
	klog.V(4).InfoS("Pod contains no container with pinned cpus", "podName", pod.Name)
	return false
}

func containerHasExclusiveCPUs(cr cpuAllocationReader, pod *v1.Pod, container *v1.Container) bool {
	exclusiveCPUs := cr.GetExclusiveCPUs(string(pod.UID), container.Name)
	if !exclusiveCPUs.IsEmpty() {
		klog.V(4).InfoS("Container has pinned cpus", "podName", pod.Name, "containerName", container.Name)
		return true
	}
	return false
}

// parsePercentage parses the percentage string to numeric value.
func parsePercentage(v string) (int64, error) {
	if !strings.HasSuffix(v, "%") {
		return 0, fmt.Errorf("percentage expected, got '%s'", v)
	}
	percentage, err := strconv.ParseInt(strings.TrimRight(v, "%"), 10, 0)
	if err != nil {
		return 0, fmt.Errorf("invalid number in percentage '%s'", v)
	}
	if percentage < 0 || percentage > 100 {
		return 0, fmt.Errorf("percentage must be between 0 and 100")
	}
	return percentage, nil
}

// ParseQOSReserved parses the --qos-reserved option
func ParseQOSReserved(m map[string]string) (*map[v1.ResourceName]int64, error) {
	reservations := make(map[v1.ResourceName]int64)
	for k, v := range m {
		switch v1.ResourceName(k) {
		// Only memory resources are supported.
		case v1.ResourceMemory:
			q, err := parsePercentage(v)
			if err != nil {
				return nil, fmt.Errorf("failed to parse percentage %q for %q resource: %w", v, k, err)
			}
			reservations[v1.ResourceName(k)] = q
		default:
			return nil, fmt.Errorf("cannot reserve %q resource", k)
		}
	}
	return &reservations, nil
}

func containerDevicesFromResourceDeviceInstances(devs devicemanager.ResourceDeviceInstances) []*podresourcesapi.ContainerDevices {
	var respDevs []*podresourcesapi.ContainerDevices

	for resourceName, resourceDevs := range devs {
		for devID, dev := range resourceDevs {
			topo := dev.GetTopology()
			if topo == nil {
				// Some device plugin do not report the topology information.
				// This is legal, so we report the devices anyway,
				// let the client decide what to do.
				respDevs = append(respDevs, &podresourcesapi.ContainerDevices{
					ResourceName: resourceName,
					DeviceIds:    []string{devID},
				})
				continue
			}

			for _, node := range topo.GetNodes() {
				respDevs = append(respDevs, &podresourcesapi.ContainerDevices{
					ResourceName: resourceName,
					DeviceIds:    []string{devID},
					Topology: &podresourcesapi.TopologyInfo{
						Nodes: []*podresourcesapi.NUMANode{
							{
								ID: node.GetID(),
							},
						},
					},
				})
			}
		}
	}

	return respDevs
}
