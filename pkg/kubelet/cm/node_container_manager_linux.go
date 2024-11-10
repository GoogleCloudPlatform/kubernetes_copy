//go:build linux
// +build linux

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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/stats/pidlimit"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

const (
	defaultNodeAllocatableCgroupName = "kubepods"
)

// createNodeAllocatableCgroups creates Node Allocatable Cgroup when CgroupsPerQOS flag is specified as true
func (cm *containerManagerImpl) createNodeAllocatableCgroups() error {
	nodeAllocatable := cm.internalCapacity
	// Use Node Allocatable limits instead of capacity if the user requested enforcing node allocatable.
	nc := cm.NodeConfig.NodeAllocatableConfig
	if cm.CgroupsPerQOS && nc.EnforceNodeAllocatable.Has(kubetypes.NodeAllocatableEnforcementKey) {
		nodeAllocatable = cm.getNodeAllocatableInternalAbsolute()
	}

	cgroupConfig := &CgroupConfig{
		Name: cm.cgroupRoot,
		// The default limits for cpu shares can be very low which can lead to CPU starvation for pods.
		ResourceParameters: cm.getCgroupConfig(nodeAllocatable, false),
	}
	if cm.cgroupManager.Exists(cgroupConfig.Name) {
		return nil
	}
	if err := cm.cgroupManager.Create(cgroupConfig); err != nil {
		klog.ErrorS(err, "Failed to create cgroup", "cgroupName", cm.cgroupRoot)
		return err
	}
	return nil
}

// enforceNodeAllocatableCgroups enforce Node Allocatable Cgroup settings.
func (cm *containerManagerImpl) enforceNodeAllocatableCgroups() error {
	nc := cm.NodeConfig.NodeAllocatableConfig

	// We need to update limits on node allocatable cgroup no matter what because
	// default cpu shares on cgroups are low and can cause cpu starvation.
	nodeAllocatable := cm.internalCapacity
	// Use Node Allocatable limits instead of capacity if the user requested enforcing node allocatable.
	if cm.CgroupsPerQOS && nc.EnforceNodeAllocatable.Has(kubetypes.NodeAllocatableEnforcementKey) {
		nodeAllocatable = cm.getNodeAllocatableInternalAbsolute()
	}

	klog.V(4).InfoS("Attempting to enforce Node Allocatable", "config", nc)

	cgroupConfig := &CgroupConfig{
		Name:               cm.cgroupRoot,
		ResourceParameters: cm.getCgroupConfig(nodeAllocatable, false),
	}

	// Using ObjectReference for events as the node maybe not cached; refer to #42701 for detail.
	nodeRef := nodeRefFromNode(cm.nodeInfo.Name)

	// If Node Allocatable is enforced on a node that has not been drained or is updated on an existing node to a lower value,
	// existing memory usage across pods might be higher than current Node Allocatable Memory Limits.
	// Pod Evictions are expected to bring down memory usage to below Node Allocatable limits.
	// Until evictions happen retry cgroup updates.
	// Update limits on non root cgroup-root to be safe since the default limits for CPU can be too low.
	// Check if cgroupRoot is set to a non-empty value (empty would be the root container)
	if len(cm.cgroupRoot) > 0 {
		go func() {
			for {
				err := cm.cgroupManager.Update(cgroupConfig)
				if err == nil {
					cm.recorder.Event(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated Node Allocatable limit across pods")
					return
				}
				message := fmt.Sprintf("Failed to update Node Allocatable Limits %q: %v", cm.cgroupRoot, err)
				cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
				time.Sleep(time.Minute)
			}
		}()
	}
	// Now apply kube reserved and system reserved limits if required.
	if nc.EnforceNodeAllocatable.Has(kubetypes.SystemReservedEnforcementKey) {
		klog.V(2).InfoS("Enforcing system reserved on cgroup", "cgroupName", nc.SystemReservedCgroupName, "limits", nc.SystemReserved)
		if err := cm.enforceExistingCgroup(nc.SystemReservedCgroupName, nc.SystemReserved, false); err != nil {
			message := fmt.Sprintf("Failed to enforce System Reserved Cgroup Limits on %q: %v", nc.SystemReservedCgroupName, err)
			cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
			return errors.New(message)
		}
		cm.recorder.Eventf(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated limits on system reserved cgroup %v", nc.SystemReservedCgroupName)
	}
	if nc.EnforceNodeAllocatable.Has(kubetypes.KubeReservedEnforcementKey) {
		klog.V(2).InfoS("Enforcing kube reserved on cgroup", "cgroupName", nc.KubeReservedCgroupName, "limits", nc.KubeReserved)
		if err := cm.enforceExistingCgroup(nc.KubeReservedCgroupName, nc.KubeReserved, false); err != nil {
			message := fmt.Sprintf("Failed to enforce Kube Reserved Cgroup Limits on %q: %v", nc.KubeReservedCgroupName, err)
			cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
			return errors.New(message)
		}
		cm.recorder.Eventf(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated limits on kube reserved cgroup %v", nc.KubeReservedCgroupName)
	}

	if nc.EnforceNodeAllocatable.Has(kubetypes.SystemReservedCompressibleEnforcementKey) {
		klog.V(2).InfoS("Enforcing system reserved compressible on cgroup", "cgroupName", nc.SystemReservedCgroupName, "limits", nc.SystemReserved)
		if err := cm.enforceExistingCgroup(nc.SystemReservedCgroupName, nc.SystemReserved, true); err != nil {
			message := fmt.Sprintf("Failed to enforce System Reserved Compressible Cgroup Limits on %q: %v", nc.SystemReservedCgroupName, err)
			cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
			return errors.New(message)
		}
		cm.recorder.Eventf(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated limits on system reserved cgroup %v", nc.SystemReservedCgroupName)
	}

	if nc.EnforceNodeAllocatable.Has(kubetypes.KubeReservedCompressibleEnforcementKey) {
		klog.V(2).InfoS("Enforcing kube reserved compressible on cgroup", "cgroupName", nc.KubeReservedCgroupName, "limits", nc.KubeReserved)
		if err := cm.enforceExistingCgroup(nc.KubeReservedCgroupName, nc.KubeReserved, true); err != nil {
			message := fmt.Sprintf("Failed to enforce Kube Reserved Compressible Cgroup Limits on %q: %v", nc.KubeReservedCgroupName, err)
			cm.recorder.Event(nodeRef, v1.EventTypeWarning, events.FailedNodeAllocatableEnforcement, message)
			return errors.New(message)
		}
		cm.recorder.Eventf(nodeRef, v1.EventTypeNormal, events.SuccessfulNodeAllocatableEnforcement, "Updated limits on kube reserved cgroup %v", nc.KubeReservedCgroupName)
	}
	return nil
}

// enforceExistingCgroup updates the limits `rl` on existing cgroup `cName` using `cgroupManager` interface.
func (cm *containerManagerImpl) enforceExistingCgroup(cNameStr string, rl v1.ResourceList, compressibleResources bool) error {
	cName := cm.cgroupManager.CgroupName(cNameStr)
	rp := cm.getCgroupConfig(rl, compressibleResources)
	if rp == nil {
		return fmt.Errorf("%q cgroup is not configured properly", cName)
	}

	// Enforce MemoryQoS for cgroups of kube-reserved/system-reserved. For more information,
	// see https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2570-memory-qos
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.MemoryQoS) {
		if rp.Memory != nil {
			if rp.Unified == nil {
				rp.Unified = make(map[string]string)
			}
			rp.Unified[Cgroup2MemoryMin] = strconv.FormatInt(*rp.Memory, 10)
		}
	}

	cgroupConfig := &CgroupConfig{
		Name:               cName,
		ResourceParameters: rp,
	}
	klog.V(4).InfoS("Enforcing limits on cgroup", "cgroupName", cName, "cpuShares", cgroupConfig.ResourceParameters.CPUShares, "memory", cgroupConfig.ResourceParameters.Memory, "pidsLimit", cgroupConfig.ResourceParameters.PidsLimit)
	if err := cm.cgroupManager.Validate(cgroupConfig.Name); err != nil {
		return err
	}
	if err := cm.cgroupManager.Update(cgroupConfig); err != nil {
		return err
	}
	return nil
}

// getCgroupConfig returns a ResourceConfig object that can be used to create or update cgroups via CgroupManager interface.
func (cm *containerManagerImpl) getCgroupConfig(rl v1.ResourceList, compressibleResourcesOnly bool) *ResourceConfig {
	rc := getCgroupConfigInternal(rl, compressibleResourcesOnly)
	if rc == nil {
		return nil
	}

	// In the case of a None policy, cgroupv2 and systemd cgroup manager, we must make sure systemd is aware of the cpuset cgroup.
	// By default, systemd will not create it, as we've not chosen to delegate it, and we haven't included it in the Apply() request.
	// However, this causes a bug where kubelet restarts unnecessarily (cpuset cgroup is created in the cgroupfs, but systemd
	// doesn't know about it and deletes it, and then kubelet doesn't continue because the cgroup isn't configured as expected).
	// An alternative is to delegate the `cpuset` cgroup to the kubelet, but that would require some plumbing in libcontainer,
	// and this is sufficient.
	// Only do so when the cpumanager is not exclusively allocating CPUs, as it will do its own updating of the cpuset when
	// CPUs are exclusively allocated.
	if !cm.cpuManager.CanAllocateExclusively(v1.ResourceCPU) {
		rc.CPUSet = cm.cpuManager.GetAllCPUs()
	}

	return rc
}

// getCgroupConfigInternal are the pieces of getCgroupConfig that don't require the cm object.
// This is added to unit test without needing to create a full containerManager
func getCgroupConfigInternal(rl v1.ResourceList, compressibleResourcesOnly bool) *ResourceConfig {
	// TODO(vishh): Set CPU Quota if necessary.
	if rl == nil {
		return nil
	}
	var rc ResourceConfig

	setCompressibleResources := func() {
		if q, exists := rl[v1.ResourceCPU]; exists {
			// CPU is defined in milli-cores.
			val := MilliCPUToShares(q.MilliValue())
			rc.CPUShares = &val
		}
	}

	// Only return compressible resources
	if compressibleResourcesOnly {
		setCompressibleResources()
	} else {
		if q, exists := rl[v1.ResourceMemory]; exists {
			// Memory is defined in bytes.
			val := q.Value()
			rc.Memory = &val
		}

		setCompressibleResources()

		if q, exists := rl[pidlimit.PIDs]; exists {
			val := q.Value()
			rc.PidsLimit = &val
		}
		rc.HugePageLimit = HugePageLimits(rl)
	}
	return &rc
}

// GetNodeAllocatableAbsolute returns the absolute value of Node Allocatable which is primarily useful for enforcement.
// Note that not all resources that are available on the node are included in the returned list of resources.
// Returns a ResourceList.
func (cm *containerManagerImpl) GetNodeAllocatableAbsolute() v1.ResourceList {
	return cm.getNodeAllocatableAbsoluteImpl(cm.capacity)
}

func (cm *containerManagerImpl) getNodeAllocatableAbsoluteImpl(capacity v1.ResourceList) v1.ResourceList {
	result := make(v1.ResourceList)
	for k, v := range capacity {
		value := v.DeepCopy()
		if cm.NodeConfig.SystemReserved != nil {
			value.Sub(cm.NodeConfig.SystemReserved[k])
		}
		if cm.NodeConfig.KubeReserved != nil {
			value.Sub(cm.NodeConfig.KubeReserved[k])
		}
		if value.Sign() < 0 {
			// Negative Allocatable resources don't make sense.
			value.Set(0)
		}
		result[k] = value
	}
	return result
}

// getNodeAllocatableInternalAbsolute is similar to getNodeAllocatableAbsolute except that
// it also includes internal resources (currently process IDs).  It is intended for setting
// up top level cgroups only.
func (cm *containerManagerImpl) getNodeAllocatableInternalAbsolute() v1.ResourceList {
	return cm.getNodeAllocatableAbsoluteImpl(cm.internalCapacity)
}

// GetNodeAllocatableReservation returns amount of compute or storage resource that have to be reserved on this node from scheduling.
func (cm *containerManagerImpl) GetNodeAllocatableReservation() v1.ResourceList {
	evictionReservation := hardEvictionReservation(cm.HardEvictionThresholds, cm.capacity)
	result := make(v1.ResourceList)
	for k := range cm.capacity {
		value := resource.NewQuantity(0, resource.DecimalSI)
		if cm.NodeConfig.SystemReserved != nil {
			value.Add(cm.NodeConfig.SystemReserved[k])
		}
		if cm.NodeConfig.KubeReserved != nil {
			value.Add(cm.NodeConfig.KubeReserved[k])
		}
		if evictionReservation != nil {
			value.Add(evictionReservation[k])
		}
		if !value.IsZero() {
			result[k] = *value
		}
	}
	return result
}

// validateNodeAllocatable ensures that the user specified Node Allocatable Configuration doesn't reserve more than the node capacity.
// Returns error if the configuration is invalid, nil otherwise.
func (cm *containerManagerImpl) validateNodeAllocatable() error {
	var errors []string
	nar := cm.GetNodeAllocatableReservation()
	for k, v := range nar {
		value := cm.capacity[k].DeepCopy()
		value.Sub(v)

		if value.Sign() < 0 {
			errors = append(errors, fmt.Sprintf("Resource %q has a reservation of %v but capacity of %v. Expected capacity >= reservation.", k, v, cm.capacity[k]))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("invalid Node Allocatable configuration. %s", strings.Join(errors, " "))
	}
	return nil
}

// Using ObjectReference for events as the node maybe not cached; refer to #42701 for detail.
func nodeRefFromNode(nodeName string) *v1.ObjectReference {
	return &v1.ObjectReference{
		Kind:      "Node",
		Name:      nodeName,
		UID:       types.UID(nodeName),
		Namespace: "",
	}
}
