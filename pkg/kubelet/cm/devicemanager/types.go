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

package devicemanager

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/resourceupdates"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

// Manager manages all the Device Plugins running on a node.
type Manager interface {
	// Start starts device plugin registration service.
	Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, initialContainers containermap.ContainerMap, initialContainerRunningSet sets.Set[string]) error

	// Allocate configures and assigns devices to a container in a pod. From
	// the requested device resources, Allocate will communicate with the
	// owning device plugin to allow setup procedures to take place, and for
	// the device plugin to provide runtime settings to use the device
	// (environment variables, mount points and device files).
	Allocate(pod *v1.Pod, container *v1.Container) error

	// UpdatePluginResources updates node resources based on devices already
	// allocated to pods. The node object is provided for the device manager to
	// update the node capacity to reflect the currently available devices.
	UpdatePluginResources(node *schedulerframework.NodeInfo, attrs *lifecycle.PodAdmitAttributes) error

	// Stop stops the manager.
	Stop() error

	// GetDeviceRunContainerOptions checks whether we have cached containerDevices
	// for the passed-in <pod, container> and returns its DeviceRunContainerOptions
	// for the found one. An empty struct is returned in case no cached state is found.
	GetDeviceRunContainerOptions(pod *v1.Pod, container *v1.Container) (*DeviceRunContainerOptions, error)

	// GetCapacity returns the amount of available device plugin resource capacity, resource allocatable
	// and inactive device plugin resources previously registered on the node.
	GetCapacity() (v1.ResourceList, v1.ResourceList, []string)

	// GetWatcherHandler returns the plugin handler for the device manager.
	GetWatcherHandler() cache.PluginHandler
	GetHealthChecker() healthz.HealthChecker

	// GetDevices returns information about the devices assigned to pods and containers
	GetDevices(podUID, containerName string) ResourceDeviceInstances

	// UpdateAllocatedResourcesStatus updates the status of allocated resources for the pod.
	UpdateAllocatedResourcesStatus(pod *v1.Pod, status *v1.PodStatus)

	// GetAllocatableDevices returns information about all the devices known to the manager
	GetAllocatableDevices() ResourceDeviceInstances

	// ShouldResetExtendedResourceCapacity returns whether the extended resources should be reset or not,
	// depending on the checkpoint file availability. Absence of the checkpoint file strongly indicates
	// the node has been recreated.
	ShouldResetExtendedResourceCapacity() bool

	// TopologyManager HintProvider provider indicates the Device Manager implements the Topology Manager Interface
	// and is consulted to make Topology aware resource alignments
	GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint

	// TopologyManager HintProvider provider indicates the Device Manager implements the Topology Manager Interface
	// and is consulted to make Topology aware resource alignments per Pod
	GetPodTopologyHints(pod *v1.Pod) map[string][]topologymanager.TopologyHint

	// UpdateAllocatedDevices frees any Devices that are bound to terminated pods.
	UpdateAllocatedDevices()

	// Updates returns a channel that receives an Update when the device changed its status.
	Updates() <-chan resourceupdates.Update

	// Returns true if the manager can allocate exclusively the resource(s) to a container
	CanAllocateExclusively(res v1.ResourceName) bool
}

// DeviceRunContainerOptions contains the combined container runtime settings to consume its allocated devices.
type DeviceRunContainerOptions struct {
	// The environment variables list.
	Envs []kubecontainer.EnvVar
	// The mounts for the container.
	Mounts []kubecontainer.Mount
	// The host devices mapped into the container.
	Devices []kubecontainer.DeviceInfo
	// The Annotations for the container
	Annotations []kubecontainer.Annotation
	// CDI Devices for the container
	CDIDevices []kubecontainer.CDIDevice
}

// TODO: evaluate whether we need this error definition.
const (
	errEndpointStopped = "endpoint %v has been stopped"
)

// endpointStopGracePeriod indicates the grace period after an endpoint is stopped
// because its device plugin fails. DeviceManager keeps the stopped endpoint in its
// cache during this grace period to cover the time gap for the capacity change to
// take effect.
const endpointStopGracePeriod = time.Duration(5) * time.Minute

// kubeletDeviceManagerCheckpoint is the file name of device plugin checkpoint
const kubeletDeviceManagerCheckpoint = "kubelet_internal_checkpoint"
