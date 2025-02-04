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

package cm

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/util/wait"

	units "github.com/docker/go-units"
	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
	utilfeature "k8s.io/apiserver/pkg/util/feature"

	"k8s.io/component-helpers/resource"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubefeatures "k8s.io/kubernetes/pkg/features"
)

const (
	// how often the qos cgroup manager will perform periodic update
	// of the qos level cgroup resource constraints
	periodicQOSCgroupUpdateInterval = 1 * time.Minute
)

type QOSContainerManager interface {
	Start(func() v1.ResourceList, ActivePodsFunc) error
	GetQOSContainersInfo() QOSContainersInfo
	UpdateCgroups() error
}

type qosContainerManagerImpl struct {
	sync.Mutex
	qosContainersInfo  QOSContainersInfo
	subsystems         *CgroupSubsystems
	cgroupManager      CgroupManager
	activePods         ActivePodsFunc
	getNodeAllocatable func() v1.ResourceList
	cgroupRoot         CgroupName
	qosReserved        map[v1.ResourceName]int64
}

func NewQOSContainerManager(subsystems *CgroupSubsystems, cgroupRoot CgroupName, nodeConfig NodeConfig, cgroupManager CgroupManager) (QOSContainerManager, error) {
	if !nodeConfig.CgroupsPerQOS {
		return &qosContainerManagerNoop{
			cgroupRoot: cgroupRoot,
		}, nil
	}

	return &qosContainerManagerImpl{
		subsystems:    subsystems,
		cgroupManager: cgroupManager,
		cgroupRoot:    cgroupRoot,
		qosReserved:   nodeConfig.QOSReserved,
	}, nil
}

func (m *qosContainerManagerImpl) GetQOSContainersInfo() QOSContainersInfo {
	return m.qosContainersInfo
}

func (m *qosContainerManagerImpl) Start(getNodeAllocatable func() v1.ResourceList, activePods ActivePodsFunc) error {
	cm := m.cgroupManager
	rootContainer := m.cgroupRoot

	if err := cm.Validate(rootContainer); err != nil {
		return fmt.Errorf("error validating root container %v : %w", rootContainer, err)
	}

	// Top level for Qos containers are created only for Burstable
	// and Best Effort classes
	qosClasses := map[v1.PodQOSClass]CgroupName{
		v1.PodQOSBurstable:  NewCgroupName(rootContainer, strings.ToLower(string(v1.PodQOSBurstable))),
		v1.PodQOSBestEffort: NewCgroupName(rootContainer, strings.ToLower(string(v1.PodQOSBestEffort))),
	}

	// Create containers for both qos classes
	for qosClass, containerName := range qosClasses {
		resourceParameters := &ResourceConfig{}
		// the BestEffort QoS class has a statically configured minShares value
		if qosClass == v1.PodQOSBestEffort {
			minShares := uint64(MinShares)
			resourceParameters.CPUShares = &minShares
		}

		// containerConfig object stores the cgroup specifications
		containerConfig := &CgroupConfig{
			Name:               containerName,
			ResourceParameters: resourceParameters,
		}

		// for each enumerated huge page size, the qos tiers are unbounded
		m.setHugePagesUnbounded(containerConfig)

		// check if it exists
		if !cm.Exists(containerName) {
			if err := cm.Create(containerConfig); err != nil {
				return fmt.Errorf("failed to create top level %v QOS cgroup : %v", qosClass, err)
			}
		} else {
			// to ensure we actually have the right state, we update the config on startup
			if err := cm.Update(containerConfig); err != nil {
				return fmt.Errorf("failed to update top level %v QOS cgroup : %v", qosClass, err)
			}
		}
	}
	// Store the top level qos container names
	m.qosContainersInfo = QOSContainersInfo{
		Guaranteed: rootContainer,
		Burstable:  qosClasses[v1.PodQOSBurstable],
		BestEffort: qosClasses[v1.PodQOSBestEffort],
	}
	m.getNodeAllocatable = getNodeAllocatable
	m.activePods = activePods

	// update qos cgroup tiers on startup and in periodic intervals
	// to ensure desired state is in sync with actual state.
	go wait.Until(func() {
		err := m.UpdateCgroups()
		if err != nil {
			klog.InfoS("Failed to reserve QoS requests", "err", err)
		}
	}, periodicQOSCgroupUpdateInterval, wait.NeverStop)

	return nil
}

// setHugePagesUnbounded ensures hugetlb is effectively unbounded
func (m *qosContainerManagerImpl) setHugePagesUnbounded(cgroupConfig *CgroupConfig) error {
	hugePageLimit := map[int64]int64{}
	for _, pageSize := range libcontainercgroups.HugePageSizes() {
		pageSizeBytes, err := units.RAMInBytes(pageSize)
		if err != nil {
			return err
		}
		hugePageLimit[pageSizeBytes] = int64(1 << 62)
	}
	cgroupConfig.ResourceParameters.HugePageLimit = hugePageLimit
	return nil
}

func (m *qosContainerManagerImpl) setHugePagesConfig(configs map[v1.PodQOSClass]*CgroupConfig) error {
	for _, v := range configs {
		if err := m.setHugePagesUnbounded(v); err != nil {
			return err
		}
	}
	return nil
}

func (m *qosContainerManagerImpl) setCPUCgroupConfig(configs map[v1.PodQOSClass]*CgroupConfig) error {
	pods := m.activePods()
	burstablePodCPURequest := int64(0)
	reuseReqs := make(v1.ResourceList, 4)
	for i := range pods {
		pod := pods[i]
		qosClass := v1qos.GetPodQOS(pod)
		if qosClass != v1.PodQOSBurstable {
			// we only care about the burstable qos tier
			continue
		}
		req := resource.PodRequests(pod, resource.PodResourcesOptions{
			Reuse: reuseReqs,
			// SkipPodLevelResources is set to false when PodLevelResources feature is enabled.
			SkipPodLevelResources: !utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodLevelResources),
		})
		if request, found := req[v1.ResourceCPU]; found {
			burstablePodCPURequest += request.MilliValue()
		}
	}

	// make sure best effort is always 2 shares
	bestEffortCPUShares := uint64(MinShares)
	configs[v1.PodQOSBestEffort].ResourceParameters.CPUShares = &bestEffortCPUShares

	// set burstable shares based on current observe state
	burstableCPUShares := MilliCPUToShares(burstablePodCPURequest)
	configs[v1.PodQOSBurstable].ResourceParameters.CPUShares = &burstableCPUShares
	return nil
}

// getQoSMemoryRequests sums and returns the memory request of all pods for
// guaranteed and burstable qos classes.
func (m *qosContainerManagerImpl) getQoSMemoryRequests() map[v1.PodQOSClass]int64 {
	qosMemoryRequests := map[v1.PodQOSClass]int64{
		v1.PodQOSGuaranteed: 0,
		v1.PodQOSBurstable:  0,
	}

	// Sum the pod limits for pods in each QOS class
	pods := m.activePods()
	reuseReqs := make(v1.ResourceList, 4)
	for _, pod := range pods {
		podMemoryRequest := int64(0)
		qosClass := v1qos.GetPodQOS(pod)
		if qosClass == v1.PodQOSBestEffort {
			// limits are not set for Best Effort pods
			continue
		}
		req := resource.PodRequests(pod, resource.PodResourcesOptions{Reuse: reuseReqs})
		if request, found := req[v1.ResourceMemory]; found {
			podMemoryRequest += request.Value()
		}
		qosMemoryRequests[qosClass] += podMemoryRequest
	}

	return qosMemoryRequests
}

// setMemoryReserve sums the memory limits of all pods in a QOS class,
// calculates QOS class memory limits, and set those limits in the
// CgroupConfig for each QOS class.
func (m *qosContainerManagerImpl) setMemoryReserve(configs map[v1.PodQOSClass]*CgroupConfig, percentReserve int64) {
	qosMemoryRequests := m.getQoSMemoryRequests()

	resources := m.getNodeAllocatable()
	allocatableResource, ok := resources[v1.ResourceMemory]
	if !ok {
		klog.V(2).InfoS("Allocatable memory value could not be determined, not setting QoS memory limits")
		return
	}
	allocatable := allocatableResource.Value()
	if allocatable == 0 {
		klog.V(2).InfoS("Allocatable memory reported as 0, might be in standalone mode, not setting QoS memory limits")
		return
	}

	for qos, limits := range qosMemoryRequests {
		klog.V(2).InfoS("QoS pod memory limit", "qos", qos, "limits", limits, "percentReserve", percentReserve)
	}

	// Calculate QOS memory limits
	burstableLimit := allocatable - (qosMemoryRequests[v1.PodQOSGuaranteed] * percentReserve / 100)
	bestEffortLimit := burstableLimit - (qosMemoryRequests[v1.PodQOSBurstable] * percentReserve / 100)
	configs[v1.PodQOSBurstable].ResourceParameters.Memory = &burstableLimit
	configs[v1.PodQOSBestEffort].ResourceParameters.Memory = &bestEffortLimit
}

// retrySetMemoryReserve checks for any QoS cgroups over the limit
// that was attempted to be set in the first Update() and adjusts
// their memory limit to the usage to prevent further growth.
func (m *qosContainerManagerImpl) retrySetMemoryReserve(configs map[v1.PodQOSClass]*CgroupConfig, percentReserve int64) {
	// Unreclaimable memory usage may already exceed the desired limit
	// Attempt to set the limit near the current usage to put pressure
	// on the cgroup and prevent further growth.
	for qos, config := range configs {
		usage, err := m.cgroupManager.MemoryUsage(config.Name)
		if err != nil {
			klog.V(2).InfoS("Failed to get resource stats", "err", err)
			return
		}

		// Because there is no good way to determine of the original Update()
		// on the memory resource was successful, we determine failure of the
		// first attempt by checking if the usage is above the limit we attempt
		// to set.  If it is, we assume the first attempt to set the limit failed
		// and try again setting the limit to the usage.  Otherwise we leave
		// the CgroupConfig as is.
		if configs[qos].ResourceParameters.Memory != nil && usage > *configs[qos].ResourceParameters.Memory {
			configs[qos].ResourceParameters.Memory = &usage
		}
	}
}

// setMemoryQoS sums the memory requests of all pods in the Burstable class,
// and set the sum memory as the memory.min in the Unified field of CgroupConfig.
func (m *qosContainerManagerImpl) setMemoryQoS(configs map[v1.PodQOSClass]*CgroupConfig) {
	qosMemoryRequests := m.getQoSMemoryRequests()

	// Calculate the memory.min:
	// for burstable(/kubepods/burstable): sum of all burstable pods
	// for guaranteed(/kubepods): sum of all guaranteed and burstable pods
	burstableMin := qosMemoryRequests[v1.PodQOSBurstable]
	guaranteedMin := qosMemoryRequests[v1.PodQOSGuaranteed] + burstableMin

	if burstableMin > 0 {
		if configs[v1.PodQOSBurstable].ResourceParameters.Unified == nil {
			configs[v1.PodQOSBurstable].ResourceParameters.Unified = make(map[string]string)
		}
		configs[v1.PodQOSBurstable].ResourceParameters.Unified[Cgroup2MemoryMin] = strconv.FormatInt(burstableMin, 10)
		klog.V(4).InfoS("MemoryQoS config for qos", "qos", v1.PodQOSBurstable, "memoryMin", burstableMin)
	}

	if guaranteedMin > 0 {
		if configs[v1.PodQOSGuaranteed].ResourceParameters.Unified == nil {
			configs[v1.PodQOSGuaranteed].ResourceParameters.Unified = make(map[string]string)
		}
		configs[v1.PodQOSGuaranteed].ResourceParameters.Unified[Cgroup2MemoryMin] = strconv.FormatInt(guaranteedMin, 10)
		klog.V(4).InfoS("MemoryQoS config for qos", "qos", v1.PodQOSGuaranteed, "memoryMin", guaranteedMin)
	}
}

func (m *qosContainerManagerImpl) UpdateCgroups() error {
	m.Lock()
	defer m.Unlock()

	qosConfigs := map[v1.PodQOSClass]*CgroupConfig{
		v1.PodQOSGuaranteed: {
			Name:               m.qosContainersInfo.Guaranteed,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBurstable: {
			Name:               m.qosContainersInfo.Burstable,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBestEffort: {
			Name:               m.qosContainersInfo.BestEffort,
			ResourceParameters: &ResourceConfig{},
		},
	}

	// update the qos level cgroup settings for cpu shares
	if err := m.setCPUCgroupConfig(qosConfigs); err != nil {
		return err
	}

	// update the qos level cgroup settings for huge pages (ensure they remain unbounded)
	if err := m.setHugePagesConfig(qosConfigs); err != nil {
		return err
	}

	// update the qos level cgrougs v2 settings of memory qos if feature enabled
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.MemoryQoS) &&
		libcontainercgroups.IsCgroup2UnifiedMode() {
		m.setMemoryQoS(qosConfigs)
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.QOSReserved) {
		for resource, percentReserve := range m.qosReserved {
			switch resource {
			case v1.ResourceMemory:
				m.setMemoryReserve(qosConfigs, percentReserve)
			}
		}

		updateSuccess := true
		for _, config := range qosConfigs {
			err := m.cgroupManager.Update(config)
			if err != nil {
				updateSuccess = false
			}
		}
		if updateSuccess {
			klog.V(4).InfoS("Updated QoS cgroup configuration")
			return nil
		}

		// If the resource can adjust the ResourceConfig to increase likelihood of
		// success, call the adjustment function here.  Otherwise, the Update() will
		// be called again with the same values.
		for resource, percentReserve := range m.qosReserved {
			switch resource {
			case v1.ResourceMemory:
				m.retrySetMemoryReserve(qosConfigs, percentReserve)
			}
		}
	}

	for _, config := range qosConfigs {
		err := m.cgroupManager.Update(config)
		if err != nil {
			klog.ErrorS(err, "Failed to update QoS cgroup configuration")
			return err
		}
	}

	klog.V(4).InfoS("Updated QoS cgroup configuration")
	return nil
}

type qosContainerManagerNoop struct {
	cgroupRoot CgroupName
}

var _ QOSContainerManager = &qosContainerManagerNoop{}

func (m *qosContainerManagerNoop) GetQOSContainersInfo() QOSContainersInfo {
	return QOSContainersInfo{}
}

func (m *qosContainerManagerNoop) Start(_ func() v1.ResourceList, _ ActivePodsFunc) error {
	return nil
}

func (m *qosContainerManagerNoop) UpdateCgroups() error {
	return nil
}
