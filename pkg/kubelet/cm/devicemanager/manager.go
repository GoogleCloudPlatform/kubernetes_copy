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
	"context"
	goerrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/checkpoint"
	plugin "k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/plugin/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/cm/resourceupdates"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

const nodeWithoutTopology = -1

// ActivePodsFunc is a function that returns a list of pods to reconcile.
type ActivePodsFunc func() []*v1.Pod

// ManagerImpl is the structure in charge of managing Device Plugins.
type ManagerImpl struct {
	checkpointdir string

	endpoints map[string]endpointInfo // Key is ResourceName
	mutex     sync.Mutex

	server plugin.Server

	// activePods is a method for listing active pods on the node
	// so the amount of pluginResources requested by existing pods
	// could be counted when updating allocated devices
	activePods ActivePodsFunc

	// sourcesReady provides the readiness of kubelet configuration sources such as apiserver update readiness.
	// We use it to determine when we can purge inactive pods from checkpointed state.
	sourcesReady config.SourcesReady

	// allDevices holds all the devices currently registered to the device manager
	allDevices ResourceDeviceInstances

	// healthyDevices contains all the registered healthy resourceNames and their exported device IDs.
	healthyDevices map[string]sets.Set[string]

	// unhealthyDevices contains all the unhealthy devices and their exported device IDs.
	unhealthyDevices map[string]sets.Set[string]

	// allocatedDevices contains allocated deviceIds, keyed by resourceName.
	allocatedDevices map[string]sets.Set[string]

	// podDevices contains pod to allocated device mapping.
	podDevices        *podDevices
	checkpointManager checkpointmanager.CheckpointManager

	// List of NUMA Nodes available on the underlying machine
	numaNodes []int

	// Store of Topology Affinities that the Device Manager can query.
	topologyAffinityStore topologymanager.Store

	// devicesToReuse contains devices that can be reused as they have been allocated to
	// init containers.
	devicesToReuse PodReusableDevices

	// containerMap provides a mapping from (pod, container) -> containerID
	// for all containers in a pod. Used to detect pods running across a restart
	containerMap containermap.ContainerMap

	// containerRunningSet identifies which container among those present in `containerMap`
	// was reported running by the container runtime when `containerMap` was computed.
	// Used to detect pods running across a restart
	containerRunningSet sets.Set[string]

	// update channel for device health updates
	update chan resourceupdates.Update
}

type endpointInfo struct {
	e    endpoint
	opts *pluginapi.DevicePluginOptions
}

type sourcesReadyStub struct{}

// PodReusableDevices is a map by pod name of devices to reuse.
type PodReusableDevices map[string]map[string]sets.Set[string]

func (s *sourcesReadyStub) AddSource(source string) {}
func (s *sourcesReadyStub) AllReady() bool          { return true }

// NewManagerImpl creates a new manager.
func NewManagerImpl(topology []cadvisorapi.Node, topologyAffinityStore topologymanager.Store) (*ManagerImpl, error) {
	socketPath := pluginapi.KubeletSocket
	if runtime.GOOS == "windows" {
		socketPath = os.Getenv("SYSTEMDRIVE") + pluginapi.KubeletSocketWindows
	}
	return newManagerImpl(socketPath, topology, topologyAffinityStore)
}

func newManagerImpl(socketPath string, topology []cadvisorapi.Node, topologyAffinityStore topologymanager.Store) (*ManagerImpl, error) {
	klog.V(2).InfoS("Creating Device Plugin manager", "path", socketPath)

	var numaNodes []int
	for _, node := range topology {
		numaNodes = append(numaNodes, node.Id)
	}

	manager := &ManagerImpl{
		endpoints: make(map[string]endpointInfo),

		allDevices:            NewResourceDeviceInstances(),
		healthyDevices:        make(map[string]sets.Set[string]),
		unhealthyDevices:      make(map[string]sets.Set[string]),
		allocatedDevices:      make(map[string]sets.Set[string]),
		podDevices:            newPodDevices(),
		numaNodes:             numaNodes,
		topologyAffinityStore: topologyAffinityStore,
		devicesToReuse:        make(PodReusableDevices),
		update:                make(chan resourceupdates.Update, 100),
	}

	server, err := plugin.NewServer(socketPath, manager, manager)
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin server: %v", err)
	}

	manager.server = server
	manager.checkpointdir, _ = filepath.Split(server.SocketPath())

	// The following structures are populated with real implementations in manager.Start()
	// Before that, initializes them to perform no-op operations.
	manager.activePods = func() []*v1.Pod { return []*v1.Pod{} }
	manager.sourcesReady = &sourcesReadyStub{}
	checkpointManager, err := checkpointmanager.NewCheckpointManager(manager.checkpointdir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize checkpoint manager: %v", err)
	}
	manager.checkpointManager = checkpointManager

	return manager, nil
}

func (m *ManagerImpl) Updates() <-chan resourceupdates.Update {
	return m.update
}

// CleanupPluginDirectory is to remove all existing unix sockets
// from /var/lib/kubelet/device-plugins on Device Plugin Manager start
func (m *ManagerImpl) CleanupPluginDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		filePath := filepath.Join(dir, name)
		if filePath == m.checkpointFile() {
			continue
		}
		// TODO: Until the bug - https://github.com/golang/go/issues/33357 is fixed, os.stat wouldn't return the
		// right mode(socket) on windows. Hence deleting the file, without checking whether
		// its a socket, on windows.
		stat, err := os.Lstat(filePath)
		if err != nil {
			klog.ErrorS(err, "Failed to stat file", "path", filePath)
			continue
		}
		if stat.IsDir() {
			continue
		}
		err = os.RemoveAll(filePath)
		if err != nil {
			errs = append(errs, err)
			klog.ErrorS(err, "Failed to remove file", "path", filePath)
			continue
		}
	}
	return errorsutil.NewAggregate(errs)
}

// PluginConnected is to connect a plugin to a new endpoint.
// This is done as part of device plugin registration.
func (m *ManagerImpl) PluginConnected(resourceName string, p plugin.DevicePlugin) error {
	options, err := p.API().GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})
	if err != nil {
		return fmt.Errorf("failed to get device plugin options: %v", err)
	}

	e := newEndpointImpl(p)

	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.endpoints[resourceName] = endpointInfo{e, options}

	klog.V(2).InfoS("Device plugin connected", "resourceName", resourceName)
	return nil
}

// PluginDisconnected is to disconnect a plugin from an endpoint.
// This is done as part of device plugin deregistration.
func (m *ManagerImpl) PluginDisconnected(resourceName string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if ep, exists := m.endpoints[resourceName]; exists {
		m.markResourceUnhealthy(resourceName)
		klog.V(2).InfoS("Endpoint became unhealthy", "resourceName", resourceName, "endpoint", ep)

		ep.e.setStopTime(time.Now())
	}
}

// PluginListAndWatchReceiver receives ListAndWatchResponse from a device plugin
// and ensures that an upto date state (e.g. number of devices and device health)
// is captured. Also, registered device and device to container allocation
// information is checkpointed to the disk.
func (m *ManagerImpl) PluginListAndWatchReceiver(resourceName string, resp *pluginapi.ListAndWatchResponse) {
	var devices []pluginapi.Device
	for _, d := range resp.Devices {
		devices = append(devices, *d)
	}
	m.genericDeviceUpdateCallback(resourceName, devices)
}

func (m *ManagerImpl) genericDeviceUpdateCallback(resourceName string, devices []pluginapi.Device) {
	healthyCount := 0
	m.mutex.Lock()
	m.healthyDevices[resourceName] = sets.New[string]()
	m.unhealthyDevices[resourceName] = sets.New[string]()
	oldDevices := m.allDevices[resourceName]
	podsToUpdate := sets.New[string]()
	m.allDevices[resourceName] = make(map[string]pluginapi.Device)
	for _, dev := range devices {

		if utilfeature.DefaultFeatureGate.Enabled(features.ResourceHealthStatus) {
			// compare with old device's health and send update to the channel if needed
			updatePodUIDFn := func(deviceID string) {
				podUID, _ := m.podDevices.getPodAndContainerForDevice(deviceID)
				if podUID != "" {
					podsToUpdate.Insert(podUID)
				}
			}
			if oldDev, ok := oldDevices[dev.ID]; ok {
				if oldDev.Health != dev.Health {
					updatePodUIDFn(dev.ID)
				}
			} else {
				// if this is a new device, it might have existed before and disappeared for a while
				// but still be assigned to a Pod. In this case, we need to send an update to the channel
				updatePodUIDFn(dev.ID)
			}
		}

		m.allDevices[resourceName][dev.ID] = dev
		if dev.Health == pluginapi.Healthy {
			m.healthyDevices[resourceName].Insert(dev.ID)
			healthyCount++
		} else {
			m.unhealthyDevices[resourceName].Insert(dev.ID)
		}
	}
	m.mutex.Unlock()

	if utilfeature.DefaultFeatureGate.Enabled(features.ResourceHealthStatus) {
		if len(podsToUpdate) > 0 {
			select {
			case m.update <- resourceupdates.Update{PodUIDs: podsToUpdate.UnsortedList()}:
			default:
				klog.ErrorS(goerrors.New("device update channel is full"), "discard pods info", "podsToUpdate", podsToUpdate.UnsortedList())
			}
		}
	}

	if err := m.writeCheckpoint(); err != nil {
		klog.ErrorS(err, "Writing checkpoint encountered")
	}
	klog.V(2).InfoS("Processed device updates for resource", "resourceName", resourceName, "totalCount", len(devices), "healthyCount", healthyCount)
}

// GetWatcherHandler returns the plugin handler
func (m *ManagerImpl) GetWatcherHandler() cache.PluginHandler {
	return m.server
}

// GetHealthChecker returns the plugin handler
func (m *ManagerImpl) GetHealthChecker() healthz.HealthChecker {
	return m.server
}

// checkpointFile returns device plugin checkpoint file path.
func (m *ManagerImpl) checkpointFile() string {
	return filepath.Join(m.checkpointdir, kubeletDeviceManagerCheckpoint)
}

// Start starts the Device Plugin Manager and start initialization of
// podDevices and allocatedDevices information from checkpointed state and
// starts device plugin registration service.
func (m *ManagerImpl) Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, initialContainers containermap.ContainerMap, initialContainerRunningSet sets.Set[string]) error {
	klog.V(2).InfoS("Starting Device Plugin manager")

	m.activePods = activePods
	m.sourcesReady = sourcesReady
	m.containerMap = initialContainers
	m.containerRunningSet = initialContainerRunningSet

	// Loads in allocatedDevices information from disk.
	err := m.readCheckpoint()
	if err != nil {
		klog.InfoS("Continue after failing to read checkpoint file. Device allocation info may NOT be up-to-date", "err", err)
	}

	return m.server.Start()
}

// Stop is the function that can stop the plugin server.
// Can be called concurrently, more than once, and is safe to call
// without a prior Start.
func (m *ManagerImpl) Stop() error {
	return m.server.Stop()
}

// Allocate is the call that you can use to allocate a set of devices
// from the registered device plugins.
func (m *ManagerImpl) Allocate(pod *v1.Pod, container *v1.Container) error {
	if _, ok := m.devicesToReuse[string(pod.UID)]; !ok {
		m.devicesToReuse[string(pod.UID)] = make(map[string]sets.Set[string])
	}
	// If pod entries to m.devicesToReuse other than the current pod exist, delete them.
	for podUID := range m.devicesToReuse {
		if podUID != string(pod.UID) {
			delete(m.devicesToReuse, podUID)
		}
	}
	// Allocate resources for init containers first as we know the caller always loops
	// through init containers before looping through app containers. Should the caller
	// ever change those semantics, this logic will need to be amended.
	for _, initContainer := range pod.Spec.InitContainers {
		if container.Name == initContainer.Name {
			if err := m.allocateContainerResources(pod, container, m.devicesToReuse[string(pod.UID)]); err != nil {
				return err
			}
			if !podutil.IsRestartableInitContainer(&initContainer) {
				m.podDevices.addContainerAllocatedResources(string(pod.UID), container.Name, m.devicesToReuse[string(pod.UID)])
			} else {
				// If the init container is restartable, we need to keep the
				// devices allocated. In other words, we should remove them
				// from the devicesToReuse.
				m.podDevices.removeContainerAllocatedResources(string(pod.UID), container.Name, m.devicesToReuse[string(pod.UID)])
			}
			return nil
		}
	}
	if err := m.allocateContainerResources(pod, container, m.devicesToReuse[string(pod.UID)]); err != nil {
		return err
	}
	m.podDevices.removeContainerAllocatedResources(string(pod.UID), container.Name, m.devicesToReuse[string(pod.UID)])
	return nil
}

// UpdatePluginResources updates node resources based on devices already allocated to pods.
func (m *ManagerImpl) UpdatePluginResources(node *schedulerframework.NodeInfo, attrs *lifecycle.PodAdmitAttributes) error {
	pod := attrs.Pod

	// quick return if no pluginResources requested
	if !m.podDevices.hasPod(string(pod.UID)) {
		return nil
	}

	m.sanitizeNodeAllocatable(node)
	return nil
}

func (m *ManagerImpl) markResourceUnhealthy(resourceName string) {
	klog.V(2).InfoS("Mark all resources Unhealthy for resource", "resourceName", resourceName)
	healthyDevices := sets.New[string]()
	if _, ok := m.healthyDevices[resourceName]; ok {
		healthyDevices = m.healthyDevices[resourceName]
		m.healthyDevices[resourceName] = sets.New[string]()
	}
	if _, ok := m.unhealthyDevices[resourceName]; !ok {
		m.unhealthyDevices[resourceName] = sets.New[string]()
	}
	m.unhealthyDevices[resourceName] = m.unhealthyDevices[resourceName].Union(healthyDevices)
}

// GetCapacity is expected to be called when Kubelet updates its node status.
// The first returned variable contains the registered device plugin resource capacity.
// The second returned variable contains the registered device plugin resource allocatable.
// The third returned variable contains previously registered resources that are no longer active.
// Kubelet uses this information to update resource capacity/allocatable in its node status.
// After the call, device plugin can remove the inactive resources from its internal list as the
// change is already reflected in Kubelet node status.
// Note in the special case after Kubelet restarts, device plugin resource capacities can
// temporarily drop to zero till corresponding device plugins re-register. This is OK because
// cm.UpdatePluginResource() run during predicate Admit guarantees we adjust nodeinfo
// capacity for already allocated pods so that they can continue to run. However, new pods
// requiring device plugin resources will not be scheduled till device plugin re-registers.
func (m *ManagerImpl) GetCapacity() (v1.ResourceList, v1.ResourceList, []string) {
	needsUpdateCheckpoint := false
	var capacity = v1.ResourceList{}
	var allocatable = v1.ResourceList{}
	deletedResources := sets.New[string]()
	m.mutex.Lock()
	for resourceName, devices := range m.healthyDevices {
		eI, ok := m.endpoints[resourceName]
		if (ok && eI.e.stopGracePeriodExpired()) || !ok {
			// The resources contained in endpoints and (un)healthyDevices
			// should always be consistent. Otherwise, we run with the risk
			// of failing to garbage collect non-existing resources or devices.
			if !ok {
				klog.ErrorS(nil, "Unexpected: healthyDevices and endpoints are out of sync")
			}
			delete(m.endpoints, resourceName)
			delete(m.healthyDevices, resourceName)
			deletedResources.Insert(resourceName)
			needsUpdateCheckpoint = true
		} else {
			capacity[v1.ResourceName(resourceName)] = *resource.NewQuantity(int64(devices.Len()), resource.DecimalSI)
			allocatable[v1.ResourceName(resourceName)] = *resource.NewQuantity(int64(devices.Len()), resource.DecimalSI)
		}
	}
	for resourceName, devices := range m.unhealthyDevices {
		eI, ok := m.endpoints[resourceName]
		if (ok && eI.e.stopGracePeriodExpired()) || !ok {
			if !ok {
				klog.ErrorS(nil, "Unexpected: unhealthyDevices and endpoints are out of sync")
			}
			delete(m.endpoints, resourceName)
			delete(m.unhealthyDevices, resourceName)
			deletedResources.Insert(resourceName)
			needsUpdateCheckpoint = true
		} else {
			capacityCount := capacity[v1.ResourceName(resourceName)]
			unhealthyCount := *resource.NewQuantity(int64(devices.Len()), resource.DecimalSI)
			capacityCount.Add(unhealthyCount)
			capacity[v1.ResourceName(resourceName)] = capacityCount
		}
	}
	m.mutex.Unlock()
	if needsUpdateCheckpoint {
		if err := m.writeCheckpoint(); err != nil {
			klog.ErrorS(err, "Error on writing checkpoint")
		}
	}
	return capacity, allocatable, deletedResources.UnsortedList()
}

// Checkpoints device to container allocation information to disk.
func (m *ManagerImpl) writeCheckpoint() error {
	m.mutex.Lock()
	registeredDevs := make(map[string][]string)
	for resource, devices := range m.healthyDevices {
		registeredDevs[resource] = devices.UnsortedList()
	}
	data := checkpoint.New(m.podDevices.toCheckpointData(),
		registeredDevs)
	m.mutex.Unlock()
	err := m.checkpointManager.CreateCheckpoint(kubeletDeviceManagerCheckpoint, data)
	if err != nil {
		err2 := fmt.Errorf("failed to write checkpoint file %q: %v", kubeletDeviceManagerCheckpoint, err)
		klog.InfoS("Failed to write checkpoint file", "err", err)
		return err2
	}
	return nil
}

// Reads device to container allocation information from disk, and populates
// m.allocatedDevices accordingly.
func (m *ManagerImpl) readCheckpoint() error {
	cp, err := m.getCheckpoint()
	if err != nil {
		if err == errors.ErrCheckpointNotFound {
			// no point in trying anything else
			klog.InfoS("Failed to read data from checkpoint", "checkpoint", kubeletDeviceManagerCheckpoint, "err", err)
			return nil
		}
		return err
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()
	podDevices, registeredDevs := cp.GetData()
	m.podDevices.fromCheckpointData(podDevices)
	m.allocatedDevices = m.podDevices.devices()
	for resource := range registeredDevs {
		// During start up, creates empty healthyDevices list so that the resource capacity
		// will stay zero till the corresponding device plugin re-registers.
		m.healthyDevices[resource] = sets.New[string]()
		m.unhealthyDevices[resource] = sets.New[string]()
		m.endpoints[resource] = endpointInfo{e: newStoppedEndpointImpl(resource), opts: nil}
	}
	return nil
}

func (m *ManagerImpl) getCheckpoint() (checkpoint.DeviceManagerCheckpoint, error) {
	registeredDevs := make(map[string][]string)
	devEntries := make([]checkpoint.PodDevicesEntry, 0)
	cp := checkpoint.New(devEntries, registeredDevs)
	err := m.checkpointManager.GetCheckpoint(kubeletDeviceManagerCheckpoint, cp)
	return cp, err
}

// UpdateAllocatedDevices frees any Devices that are bound to terminated pods.
func (m *ManagerImpl) UpdateAllocatedDevices() {
	activePods := m.activePods()
	if !m.sourcesReady.AllReady() {
		return
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	podsToBeRemoved := m.podDevices.pods()
	for _, pod := range activePods {
		podsToBeRemoved.Delete(string(pod.UID))
	}
	if len(podsToBeRemoved) <= 0 {
		return
	}
	klog.V(3).InfoS("Pods to be removed", "podUIDs", sets.List(podsToBeRemoved))
	m.podDevices.delete(sets.List(podsToBeRemoved))
	// Regenerated allocatedDevices after we update pod allocation information.
	m.allocatedDevices = m.podDevices.devices()
}

// Returns list of device Ids we need to allocate with Allocate rpc call.
// Returns empty list in case we don't need to issue the Allocate rpc call.
func (m *ManagerImpl) devicesToAllocate(podUID, contName, resource string, required int, reusableDevices sets.Set[string]) (sets.Set[string], error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	needed := required
	// Gets list of devices that have already been allocated.
	// This can happen if a container restarts for example.
	devices := m.podDevices.containerDevices(podUID, contName, resource)
	if devices != nil {
		klog.V(3).InfoS("Found pre-allocated devices for resource on pod", "resourceName", resource, "containerName", contName, "podUID", podUID, "devices", sets.List(devices))
		needed = needed - devices.Len()
		// A pod's resource is not expected to change once admitted by the API server,
		// so just fail loudly here. We can revisit this part if this no longer holds.
		if needed != 0 {
			return nil, fmt.Errorf("pod %q container %q changed request for resource %q from %d to %d", podUID, contName, resource, devices.Len(), required)
		}
	}

	// We have 3 major flows to handle:
	// 1. kubelet running, normal allocation (needed > 0, container being  [re]created). Steady state and most common case by far and large.
	// 2. kubelet restart. In this scenario every other component of the stack (device plugins, app container, runtime) is still running.
	// 3. node reboot. In this scenario device plugins may not be running yet when we try to allocate devices.
	//    note: if we get this far the runtime is surely running. This is usually enforced at OS level by startup system services dependencies.

	// First we take care of the exceptional flow (scenarios 2 and 3). In both flows, kubelet is reinitializing, and while kubelet is initializing, sources are NOT all ready.
	// Is this a simple kubelet restart (scenario 2)? To distinguish, we use the information we got for runtime. If we are asked to allocate devices for containers reported
	// running, then it can only be a kubelet restart. On node reboot the runtime and the containers were also shut down. Then, if the container was running, it can only be
	// because it already has access to all the required devices, so we got nothing to do and we can bail out.
	if !m.sourcesReady.AllReady() && m.isContainerAlreadyRunning(podUID, contName) {
		klog.V(3).InfoS("container detected running, nothing to do", "deviceNumber", needed, "resourceName", resource, "podUID", podUID, "containerName", contName)
		return nil, nil
	}

	// We dealt with scenario 2. If we got this far it's either scenario 3 (node reboot) or scenario 1 (steady state, normal flow).
	klog.V(3).InfoS("Need devices to allocate for pod", "deviceNumber", needed, "resourceName", resource, "podUID", podUID, "containerName", contName)
	healthyDevices, hasRegistered := m.healthyDevices[resource]

	// The following checks are expected to fail only happen on scenario 3 (node reboot).
	// The kubelet is reinitializing and got a container from sources. But there's no ordering, so an app container may attempt allocation _before_ the device plugin was created,
	// has registered and reported back to kubelet the devices.
	// This can only happen on scenario 3 because at steady state (scenario 1) the scheduler prevents pod to be sent towards node which don't report enough devices.
	// Note: we need to check the device health and registration status *before* we check how many devices are needed, doing otherwise caused issue #109595
	// Note: if the scheduler is bypassed, we fall back in scenario 1, so we still need these checks.
	if !hasRegistered {
		return nil, fmt.Errorf("cannot allocate unregistered device %s", resource)
	}

	// Check if registered resource has healthy devices
	if healthyDevices.Len() == 0 {
		return nil, fmt.Errorf("no healthy devices present; cannot allocate unhealthy devices %s", resource)
	}

	// Check if all the previously allocated devices are healthy
	if !healthyDevices.IsSuperset(devices) {
		return nil, fmt.Errorf("previously allocated devices are no longer healthy; cannot allocate unhealthy devices %s", resource)
	}

	// We handled the known error paths in scenario 3 (node reboot), so from now on we can fall back in a common path.
	// We cover container restart on kubelet steady state with the same flow.
	if needed == 0 {
		klog.V(3).InfoS("no devices needed, nothing to do", "deviceNumber", needed, "resourceName", resource, "podUID", podUID, "containerName", contName)
		// No change, no work.
		return nil, nil
	}

	// Declare the list of allocated devices.
	// This will be populated and returned below.
	allocated := sets.New[string]()

	// Create a closure to help with device allocation
	// Returns 'true' once no more devices need to be allocated.
	allocateRemainingFrom := func(devices sets.Set[string]) bool {
		// When we call callGetPreferredAllocationIfAvailable below, we will release
		// the lock and call the device plugin. If someone calls ListResource concurrently,
		// device manager will recalculate the allocatedDevices map. Some entries with
		// empty sets may be removed, so we reinit here.
		if m.allocatedDevices[resource] == nil {
			m.allocatedDevices[resource] = sets.New[string]()
		}
		for device := range devices.Difference(allocated) {
			m.allocatedDevices[resource].Insert(device)
			allocated.Insert(device)
			needed--
			if needed == 0 {
				return true
			}
		}
		return false
	}

	// Allocates from reusableDevices list first.
	if allocateRemainingFrom(reusableDevices) {
		return allocated, nil
	}

	// Gets Devices in use.
	devicesInUse := m.allocatedDevices[resource]
	// Gets Available devices.
	available := m.healthyDevices[resource].Difference(devicesInUse)
	if available.Len() < needed {
		return nil, fmt.Errorf("requested number of devices unavailable for %s. Requested: %d, Available: %d", resource, needed, available.Len())
	}

	// Filters available Devices based on NUMA affinity.
	aligned, unaligned, noAffinity := m.filterByAffinity(podUID, contName, resource, available)

	// If we can allocate all remaining devices from the set of aligned ones, then
	// give the plugin the chance to influence which ones to allocate from that set.
	if needed < aligned.Len() {
		// First allocate from the preferred devices list (if available).
		preferred, err := m.callGetPreferredAllocationIfAvailable(podUID, contName, resource, aligned.Union(allocated), allocated, required)
		if err != nil {
			return nil, err
		}
		if allocateRemainingFrom(preferred.Intersection(aligned)) {
			return allocated, nil
		}
		// Then fallback to allocate from the aligned set if no preferred list
		// is returned (or not enough devices are returned in that list).
		if allocateRemainingFrom(aligned) {
			return allocated, nil
		}

		return nil, fmt.Errorf("unexpectedly allocated less resources than required. Requested: %d, Got: %d", required, required-needed)
	}

	// If we can't allocate all remaining devices from the set of aligned ones,
	// then start by first allocating all the aligned devices (to ensure
	// that the alignment guaranteed by the TopologyManager is honored).
	if allocateRemainingFrom(aligned) {
		return allocated, nil
	}

	// Then give the plugin the chance to influence the decision on any
	// remaining devices to allocate.
	preferred, err := m.callGetPreferredAllocationIfAvailable(podUID, contName, resource, available.Union(allocated), allocated, required)
	if err != nil {
		return nil, err
	}
	if allocateRemainingFrom(preferred.Intersection(available)) {
		return allocated, nil
	}

	// Finally, if the plugin did not return a preferred allocation (or didn't
	// return a large enough one), then fall back to allocating the remaining
	// devices from the 'unaligned' and 'noAffinity' sets.
	if allocateRemainingFrom(unaligned) {
		return allocated, nil
	}
	if allocateRemainingFrom(noAffinity) {
		return allocated, nil
	}

	return nil, fmt.Errorf("unexpectedly allocated less resources than required. Requested: %d, Got: %d", required, required-needed)
}

func (m *ManagerImpl) filterByAffinity(podUID, contName, resource string, available sets.Set[string]) (sets.Set[string], sets.Set[string], sets.Set[string]) {
	// If alignment information is not available, just pass the available list back.
	hint := m.topologyAffinityStore.GetAffinity(podUID, contName)
	if !m.deviceHasTopologyAlignment(resource) || hint.NUMANodeAffinity == nil {
		return sets.New[string](), sets.New[string](), available
	}

	// Build a map of NUMA Nodes to the devices associated with them. A
	// device may be associated to multiple NUMA nodes at the same time. If an
	// available device does not have any NUMA Nodes associated with it, add it
	// to a list of NUMA Nodes for the fake NUMANode -1.
	perNodeDevices := make(map[int]sets.Set[string])
	for d := range available {
		if m.allDevices[resource][d].Topology == nil || len(m.allDevices[resource][d].Topology.Nodes) == 0 {
			if _, ok := perNodeDevices[nodeWithoutTopology]; !ok {
				perNodeDevices[nodeWithoutTopology] = sets.New[string]()
			}
			perNodeDevices[nodeWithoutTopology].Insert(d)
			continue
		}

		for _, node := range m.allDevices[resource][d].Topology.Nodes {
			if _, ok := perNodeDevices[int(node.ID)]; !ok {
				perNodeDevices[int(node.ID)] = sets.New[string]()
			}
			perNodeDevices[int(node.ID)].Insert(d)
		}
	}

	// Get a flat list of all the nodes associated with available devices.
	var nodes []int
	for node := range perNodeDevices {
		nodes = append(nodes, node)
	}

	// Sort the list of nodes by:
	// 1) Nodes contained in the 'hint's affinity set
	// 2) Nodes not contained in the 'hint's affinity set
	// 3) The fake NUMANode of -1 (assuming it is included in the list)
	// Within each of the groups above, sort the nodes by how many devices they contain
	sort.Slice(nodes, func(i, j int) bool {
		// If one or the other of nodes[i] or nodes[j] is in the 'hint's affinity set
		if hint.NUMANodeAffinity.IsSet(nodes[i]) && hint.NUMANodeAffinity.IsSet(nodes[j]) {
			return perNodeDevices[nodes[i]].Len() < perNodeDevices[nodes[j]].Len()
		}
		if hint.NUMANodeAffinity.IsSet(nodes[i]) {
			return true
		}
		if hint.NUMANodeAffinity.IsSet(nodes[j]) {
			return false
		}

		// If one or the other of nodes[i] or nodes[j] is the fake NUMA node -1 (they can't both be)
		if nodes[i] == nodeWithoutTopology {
			return false
		}
		if nodes[j] == nodeWithoutTopology {
			return true
		}

		// Otherwise both nodes[i] and nodes[j] are real NUMA nodes that are not in the 'hint's' affinity list.
		return perNodeDevices[nodes[i]].Len() < perNodeDevices[nodes[j]].Len()
	})

	// Generate three sorted lists of devices. Devices in the first list come
	// from valid NUMA Nodes contained in the affinity mask. Devices in the
	// second list come from valid NUMA Nodes not in the affinity mask. Devices
	// in the third list come from devices with no NUMA Node association (i.e.
	// those mapped to the fake NUMA Node -1). Because we loop through the
	// sorted list of NUMA nodes in order, within each list, devices are sorted
	// by their connection to NUMA Nodes with more devices on them.
	var fromAffinity []string
	var notFromAffinity []string
	var withoutTopology []string
	for d := range available {
		// Since the same device may be associated with multiple NUMA Nodes. We
		// need to be careful not to add each device to multiple lists. The
		// logic below ensures this by breaking after the first NUMA node that
		// has the device is encountered.
		for _, n := range nodes {
			if perNodeDevices[n].Has(d) {
				if n == nodeWithoutTopology {
					withoutTopology = append(withoutTopology, d)
				} else if hint.NUMANodeAffinity.IsSet(n) {
					fromAffinity = append(fromAffinity, d)
				} else {
					notFromAffinity = append(notFromAffinity, d)
				}
				break
			}
		}
	}

	// Return all three lists containing the full set of devices across them.
	return sets.New[string](fromAffinity...), sets.New[string](notFromAffinity...), sets.New[string](withoutTopology...)
}

// allocateContainerResources attempts to allocate all of required device
// plugin resources for the input container, issues an Allocate rpc request
// for each new device resource requirement, processes their AllocateResponses,
// and updates the cached containerDevices on success.
func (m *ManagerImpl) allocateContainerResources(pod *v1.Pod, container *v1.Container, devicesToReuse map[string]sets.Set[string]) error {
	podUID := string(pod.UID)
	contName := container.Name
	allocatedDevicesUpdated := false
	needsUpdateCheckpoint := false
	// Extended resources are not allowed to be overcommitted.
	// Since device plugin advertises extended resources,
	// therefore Requests must be equal to Limits and iterating
	// over the Limits should be sufficient.
	for k, v := range container.Resources.Limits {
		resource := string(k)
		needed := int(v.Value())
		klog.V(3).InfoS("Looking for needed resources", "needed", needed, "resourceName", resource)
		if !m.isDevicePluginResource(resource) {
			continue
		}
		// Updates allocatedDevices to garbage collect any stranded resources
		// before doing the device plugin allocation.
		if !allocatedDevicesUpdated {
			m.UpdateAllocatedDevices()
			allocatedDevicesUpdated = true
		}
		allocDevices, err := m.devicesToAllocate(podUID, contName, resource, needed, devicesToReuse[resource])
		if err != nil {
			return err
		}
		if allocDevices == nil || len(allocDevices) <= 0 {
			continue
		}

		needsUpdateCheckpoint = true

		startRPCTime := time.Now()
		// Manager.Allocate involves RPC calls to device plugin, which
		// could be heavy-weight. Therefore we want to perform this operation outside
		// mutex lock. Note if Allocate call fails, we may leave container resources
		// partially allocated for the failed container. We rely on UpdateAllocatedDevices()
		// to garbage collect these resources later. Another side effect is that if
		// we have X resource A and Y resource B in total, and two containers, container1
		// and container2 both require X resource A and Y resource B. Both allocation
		// requests may fail if we serve them in mixed order.
		// TODO: may revisit this part later if we see inefficient resource allocation
		// in real use as the result of this. Should also consider to parallelize device
		// plugin Allocate grpc calls if it becomes common that a container may require
		// resources from multiple device plugins.
		m.mutex.Lock()
		eI, ok := m.endpoints[resource]
		m.mutex.Unlock()
		if !ok {
			m.mutex.Lock()
			m.allocatedDevices = m.podDevices.devices()
			m.mutex.Unlock()
			return fmt.Errorf("unknown Device Plugin %s", resource)
		}

		devs := allocDevices.UnsortedList()
		// TODO: refactor this part of code to just append a ContainerAllocationRequest
		// in a passed in AllocateRequest pointer, and issues a single Allocate call per pod.
		klog.V(3).InfoS("Making allocation request for device plugin", "devices", devs, "resourceName", resource)
		resp, err := eI.e.allocate(devs)
		metrics.DevicePluginAllocationDuration.WithLabelValues(resource).Observe(metrics.SinceInSeconds(startRPCTime))
		if err != nil {
			// In case of allocation failure, we want to restore m.allocatedDevices
			// to the actual allocated state from m.podDevices.
			m.mutex.Lock()
			m.allocatedDevices = m.podDevices.devices()
			m.mutex.Unlock()
			return err
		}

		if len(resp.ContainerResponses) == 0 {
			return fmt.Errorf("no containers return in allocation response %v", resp)
		}

		allocDevicesWithNUMA := checkpoint.NewDevicesPerNUMA()
		// Update internal cached podDevices state.
		m.mutex.Lock()
		for dev := range allocDevices {
			if m.allDevices[resource][dev].Topology == nil || len(m.allDevices[resource][dev].Topology.Nodes) == 0 {
				allocDevicesWithNUMA[nodeWithoutTopology] = append(allocDevicesWithNUMA[nodeWithoutTopology], dev)
				continue
			}
			for idx := range m.allDevices[resource][dev].Topology.Nodes {
				node := m.allDevices[resource][dev].Topology.Nodes[idx]
				allocDevicesWithNUMA[node.ID] = append(allocDevicesWithNUMA[node.ID], dev)
			}
		}
		m.mutex.Unlock()
		m.podDevices.insert(podUID, contName, resource, allocDevicesWithNUMA, resp.ContainerResponses[0])
	}

	if needsUpdateCheckpoint {
		return m.writeCheckpoint()
	}

	return nil
}

// checkPodActive checks if the given pod is still in activePods list
func (m *ManagerImpl) checkPodActive(pod *v1.Pod) bool {
	activePods := m.activePods()
	for _, activePod := range activePods {
		if activePod.UID == pod.UID {
			return true
		}
	}

	return false
}

// GetDeviceRunContainerOptions checks whether we have cached containerDevices
// for the passed-in <pod, container> and returns its DeviceRunContainerOptions
// for the found one. An empty struct is returned in case no cached state is found.
func (m *ManagerImpl) GetDeviceRunContainerOptions(pod *v1.Pod, container *v1.Container) (*DeviceRunContainerOptions, error) {
	podUID := string(pod.UID)
	contName := container.Name
	needsReAllocate := false
	for k, v := range container.Resources.Limits {
		resource := string(k)
		if !m.isDevicePluginResource(resource) || v.Value() == 0 {
			continue
		}
		err := m.callPreStartContainerIfNeeded(podUID, contName, resource)
		if err != nil {
			return nil, err
		}

		if !m.checkPodActive(pod) {
			klog.ErrorS(nil, "pod deleted from activePods, skip to reAllocate", "podUID", podUID)
			continue
		}

		// This is a device plugin resource yet we don't have cached
		// resource state. This is likely due to a race during node
		// restart. We re-issue allocate request to cover this race.
		if m.podDevices.containerDevices(podUID, contName, resource) == nil {
			needsReAllocate = true
		}
	}
	if needsReAllocate {
		klog.V(2).InfoS("Needs to re-allocate device plugin resources for pod", "pod", klog.KObj(pod), "containerName", container.Name)
		if err := m.Allocate(pod, container); err != nil {
			return nil, err
		}
	}
	return m.podDevices.deviceRunContainerOptions(string(pod.UID), container.Name), nil
}

// callPreStartContainerIfNeeded issues PreStartContainer grpc call for device plugin resource
// with PreStartRequired option set.
func (m *ManagerImpl) callPreStartContainerIfNeeded(podUID, contName, resource string) error {
	m.mutex.Lock()
	eI, ok := m.endpoints[resource]
	if !ok {
		m.mutex.Unlock()
		return fmt.Errorf("endpoint not found in cache for a registered resource: %s", resource)
	}

	if eI.opts == nil || !eI.opts.PreStartRequired {
		m.mutex.Unlock()
		klog.V(4).InfoS("Plugin options indicate to skip PreStartContainer for resource", "resourceName", resource)
		return nil
	}

	devices := m.podDevices.containerDevices(podUID, contName, resource)
	if devices == nil {
		m.mutex.Unlock()
		return fmt.Errorf("no devices found allocated in local cache for pod %s, container %s, resource %s", podUID, contName, resource)
	}

	m.mutex.Unlock()
	devs := devices.UnsortedList()
	klog.V(4).InfoS("Issuing a PreStartContainer call for container", "containerName", contName, "podUID", podUID)
	_, err := eI.e.preStartContainer(devs)
	if err != nil {
		return fmt.Errorf("device plugin PreStartContainer rpc failed with err: %v", err)
	}
	// TODO: Add metrics support for init RPC
	return nil
}

// callGetPreferredAllocationIfAvailable issues GetPreferredAllocation grpc
// call for device plugin resource with GetPreferredAllocationAvailable option set.
func (m *ManagerImpl) callGetPreferredAllocationIfAvailable(podUID, contName, resource string, available, mustInclude sets.Set[string], size int) (sets.Set[string], error) {
	eI, ok := m.endpoints[resource]
	if !ok {
		return nil, fmt.Errorf("endpoint not found in cache for a registered resource: %s", resource)
	}

	if eI.opts == nil || !eI.opts.GetPreferredAllocationAvailable {
		klog.V(4).InfoS("Plugin options indicate to skip GetPreferredAllocation for resource", "resourceName", resource)
		return nil, nil
	}

	m.mutex.Unlock()
	klog.V(4).InfoS("Issuing a GetPreferredAllocation call for container", "containerName", contName, "podUID", podUID)
	resp, err := eI.e.getPreferredAllocation(available.UnsortedList(), mustInclude.UnsortedList(), size)
	m.mutex.Lock()
	if err != nil {
		return nil, fmt.Errorf("device plugin GetPreferredAllocation rpc failed with err: %v", err)
	}
	if resp != nil && len(resp.ContainerResponses) > 0 {
		return sets.New[string](resp.ContainerResponses[0].DeviceIDs...), nil
	}
	return sets.New[string](), nil
}

// sanitizeNodeAllocatable scans through allocatedDevices in the device manager
// and if necessary, updates allocatableResource in nodeInfo to at least equal to
// the allocated capacity. This allows pods that have already been scheduled on
// the node to pass GeneralPredicates admission checking even upon device plugin failure.
func (m *ManagerImpl) sanitizeNodeAllocatable(node *schedulerframework.NodeInfo) {
	var newAllocatableResource *schedulerframework.Resource
	allocatableResource := node.Allocatable
	if allocatableResource.ScalarResources == nil {
		allocatableResource.ScalarResources = make(map[v1.ResourceName]int64)
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()
	for resource, devices := range m.allocatedDevices {
		needed := devices.Len()
		quant, ok := allocatableResource.ScalarResources[v1.ResourceName(resource)]
		if ok && int(quant) >= needed {
			continue
		}
		// Needs to update nodeInfo.AllocatableResource to make sure
		// NodeInfo.allocatableResource at least equal to the capacity already allocated.
		if newAllocatableResource == nil {
			newAllocatableResource = allocatableResource.Clone()
		}
		newAllocatableResource.ScalarResources[v1.ResourceName(resource)] = int64(needed)
	}
	if newAllocatableResource != nil {
		node.Allocatable = newAllocatableResource
	}
}

func (m *ManagerImpl) isDevicePluginResource(resource string) bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	_, registeredResource := m.healthyDevices[resource]
	_, allocatedResource := m.allocatedDevices[resource]
	// Return true if this is either an active device plugin resource or
	// a resource we have previously allocated.
	if registeredResource || allocatedResource {
		return true
	}
	return false
}

// GetAllocatableDevices returns information about all the healthy devices known to the manager
func (m *ManagerImpl) GetAllocatableDevices() ResourceDeviceInstances {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	resp := m.allDevices.Filter(m.healthyDevices)
	klog.V(4).InfoS("GetAllocatableDevices", "known", len(m.allDevices), "allocatable", len(resp))
	return resp
}

// GetDevices returns the devices used by the specified container
func (m *ManagerImpl) GetDevices(podUID, containerName string) ResourceDeviceInstances {
	return m.podDevices.getContainerDevices(podUID, containerName)
}

func (m *ManagerImpl) UpdateAllocatedResourcesStatus(pod *v1.Pod, status *v1.PodStatus) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Today we ignore edge cases that are not likely to happen:
	//  - update statuses for containers that are in spec, but not in status
	//  - update statuses for resources requested in spec, but with no information in podDevices
	for i, containerStatus := range status.ContainerStatuses {
		devices := m.podDevices.getContainerDevices(string(pod.UID), containerStatus.Name)

		for resourceName, deviceInstances := range devices {
			for id, d := range deviceInstances {
				health := pluginapi.Healthy
				// this is unlikely, but check for existence here anyways
				if r, ok := m.allDevices[resourceName]; ok {
					if _, ok := r[id]; ok {
						health = m.allDevices[resourceName][id].Health
					}
				}

				d.Health = health

				deviceInstances[id] = d
			}
		}

		for resourceName, dI := range devices {
			resourceStatus := v1.ResourceStatus{
				Name:      v1.ResourceName(resourceName),
				Resources: []v1.ResourceHealth{},
			}

			for id, d := range dI {
				health := v1.ResourceHealthStatusHealthy
				if d.Health != pluginapi.Healthy {
					health = v1.ResourceHealthStatusUnhealthy
				}
				resourceStatus.Resources = append(resourceStatus.Resources, v1.ResourceHealth{
					ResourceID: v1.ResourceID(id),
					Health:     health,
				})
			}

			if status.ContainerStatuses[i].AllocatedResourcesStatus == nil {
				status.ContainerStatuses[i].AllocatedResourcesStatus = []v1.ResourceStatus{}
			}

			// look up the resource status by name and update it
			found := false
			for j, rs := range status.ContainerStatuses[i].AllocatedResourcesStatus {
				if rs.Name == resourceStatus.Name {
					status.ContainerStatuses[i].AllocatedResourcesStatus[j] = resourceStatus
					found = true
					break
				}
			}

			if !found {
				status.ContainerStatuses[i].AllocatedResourcesStatus = append(status.ContainerStatuses[i].AllocatedResourcesStatus, resourceStatus)
			}
		}
	}
}

// ShouldResetExtendedResourceCapacity returns whether the extended resources should be zeroed or not,
// depending on whether the node has been recreated. Absence of the checkpoint file strongly indicates the node
// has been recreated.
func (m *ManagerImpl) ShouldResetExtendedResourceCapacity() bool {
	checkpoints, err := m.checkpointManager.ListCheckpoints()
	if err != nil {
		return false
	}
	return len(checkpoints) == 0
}

func (m *ManagerImpl) isContainerAlreadyRunning(podUID, cntName string) bool {
	cntID, err := m.containerMap.GetContainerID(podUID, cntName)
	if err != nil {
		klog.V(4).InfoS("container not found in the initial map, assumed NOT running", "podUID", podUID, "containerName", cntName, "err", err)
		return false
	}

	// note that if container runtime is down when kubelet restarts, this set will be empty,
	// so on kubelet restart containers will again fail admission, hitting https://github.com/kubernetes/kubernetes/issues/118559 again.
	// This scenario should however be rare enough.
	if !m.containerRunningSet.Has(cntID) {
		klog.V(4).InfoS("container not present in the initial running set", "podUID", podUID, "containerName", cntName, "containerID", cntID)
		return false
	}

	// Once we make it here we know we have a running container.
	klog.V(4).InfoS("container found in the initial set, assumed running", "podUID", podUID, "containerName", cntName, "containerID", cntID)
	return true
}

func (m *ManagerImpl) CanAllocateExclusively(res v1.ResourceName) bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	_, ok := m.healthyDevices[string(res)]
	return ok
}
