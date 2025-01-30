/*
Copyright 2018 The Kubernetes Authors.

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

package nodestatus

import (
	"context"
	"fmt"
	"math"
	"net"
	goruntime "runtime"
	"strings"
	"time"

	cadvisorapiv1 "github.com/google/cadvisor/info/v1"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cloudprovider "k8s.io/cloud-provider"
	cloudproviderapi "k8s.io/cloud-provider/api"
	cloudprovidernodeutil "k8s.io/cloud-provider/node/helpers"
	"k8s.io/component-base/version"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/volume"
	netutils "k8s.io/utils/net"

	"k8s.io/klog/v2"
)

const (
	// MaxNamesPerImageInNodeStatus is max number of names
	// per image stored in the node status.
	MaxNamesPerImageInNodeStatus = 5
)

// Setter modifies the node in-place, and returns an error if the modification failed.
// Setters may partially mutate the node before returning an error.
type Setter func(ctx context.Context, node *v1.Node) error

// NodeAddress returns a Setter that updates address-related information on the node.
func NodeAddress(nodeIPs []net.IP, // typically Kubelet.nodeIPs
	validateNodeIPFunc func(net.IP) error, // typically Kubelet.nodeIPValidator
	hostname string, // typically Kubelet.hostname
	hostnameOverridden bool, // was the hostname force set?
	externalCloudProvider bool, // typically Kubelet.externalCloudProvider
	cloud cloudprovider.Interface, // typically Kubelet.cloud
	nodeAddressesFunc func() ([]v1.NodeAddress, error), // typically Kubelet.cloudResourceSyncManager.NodeAddresses
) Setter {
	var nodeIP, secondaryNodeIP net.IP
	if len(nodeIPs) > 0 {
		nodeIP = nodeIPs[0]
	}
	preferIPv4 := nodeIP == nil || nodeIP.To4() != nil
	isPreferredIPFamily := func(ip net.IP) bool { return (ip.To4() != nil) == preferIPv4 }
	nodeIPSpecified := nodeIP != nil && !nodeIP.IsUnspecified()

	if len(nodeIPs) > 1 {
		secondaryNodeIP = nodeIPs[1]
	}
	secondaryNodeIPSpecified := secondaryNodeIP != nil && !secondaryNodeIP.IsUnspecified()

	return func(ctx context.Context, node *v1.Node) error {
		if nodeIPSpecified {
			if err := validateNodeIPFunc(nodeIP); err != nil {
				return fmt.Errorf("failed to validate nodeIP: %v", err)
			}
			klog.V(4).InfoS("Using node IP", "IP", nodeIP.String())
		}
		if secondaryNodeIPSpecified {
			if err := validateNodeIPFunc(secondaryNodeIP); err != nil {
				return fmt.Errorf("failed to validate secondaryNodeIP: %v", err)
			}
			klog.V(4).InfoS("Using secondary node IP", "IP", secondaryNodeIP.String())
		}

		if (externalCloudProvider || cloud != nil) && nodeIPSpecified {
			// Annotate the Node object with nodeIP for external cloud provider.
			//
			// We do this even when external CCM is not configured to cover a situation
			// during migration from legacy to external CCM: when CCM is running the
			// node controller in the cluster but kubelet is still running the in-tree
			// provider. Adding this annotation in all cases ensures that while
			// Addresses flap between the competing controllers, they at least flap
			// consistently.
			//
			// We do not add the annotation in the case where there is no cloud
			// controller at all, as we don't expect to migrate these clusters to use an
			// external CCM.
			if node.ObjectMeta.Annotations == nil {
				node.ObjectMeta.Annotations = make(map[string]string)
			}
			annotation := nodeIP.String()
			if secondaryNodeIPSpecified {
				annotation += "," + secondaryNodeIP.String()
			}
			node.ObjectMeta.Annotations[cloudproviderapi.AnnotationAlphaProvidedIPAddr] = annotation
		} else if node.ObjectMeta.Annotations != nil {
			// Clean up stale annotations if no longer using a cloud provider or
			// no longer overriding node IP.
			delete(node.ObjectMeta.Annotations, cloudproviderapi.AnnotationAlphaProvidedIPAddr)
		}

		if externalCloudProvider {
			// If --cloud-provider=external and node address is already set,
			// then we return early because provider set addresses should take precedence.
			// Otherwise, we try to use the node IP defined via flags and let the cloud provider override it later
			// This should alleviate a lot of the bootstrapping issues with out-of-tree providers
			if len(node.Status.Addresses) > 0 {
				return nil
			}
			// If nodeIPs are not specified wait for the external cloud-provider to set the node addresses.
			// Otherwise uses them on the assumption that the installer/administrator has the previous knowledge
			// required to ensure the external cloud provider will use the same addresses to avoid the issues explained
			// in https://github.com/kubernetes/kubernetes/issues/120720.
			// We are already hinting the external cloud provider via the annotation AnnotationAlphaProvidedIPAddr.
			if !nodeIPSpecified {
				node.Status.Addresses = []v1.NodeAddress{
					{Type: v1.NodeHostName, Address: hostname},
				}
				return nil
			}
		}
		if cloud != nil {
			cloudNodeAddresses, err := nodeAddressesFunc()
			if err != nil {
				return err
			}

			nodeAddresses, err := cloudprovidernodeutil.GetNodeAddressesFromNodeIPLegacy(nodeIP, cloudNodeAddresses)
			if err != nil {
				return err
			}

			switch {
			case len(cloudNodeAddresses) == 0:
				// the cloud provider didn't specify any addresses
				nodeAddresses = append(nodeAddresses, v1.NodeAddress{Type: v1.NodeHostName, Address: hostname})

			case !hasAddressType(cloudNodeAddresses, v1.NodeHostName) && hasAddressValue(cloudNodeAddresses, hostname):
				// the cloud provider didn't specify an address of type Hostname,
				// but the auto-detected hostname matched an address reported by the cloud provider,
				// so we can add it and count on the value being verifiable via cloud provider metadata
				nodeAddresses = append(nodeAddresses, v1.NodeAddress{Type: v1.NodeHostName, Address: hostname})

			case hostnameOverridden:
				// the hostname was force-set via flag/config.
				// this means the hostname might not be able to be validated via cloud provider metadata,
				// but was a choice by the kubelet deployer we should honor
				var existingHostnameAddress *v1.NodeAddress
				for i := range nodeAddresses {
					if nodeAddresses[i].Type == v1.NodeHostName {
						existingHostnameAddress = &nodeAddresses[i]
						break
					}
				}

				if existingHostnameAddress == nil {
					// no existing Hostname address found, add it
					klog.InfoS("Adding overridden hostname to cloudprovider-reported addresses", "hostname", hostname)
					nodeAddresses = append(nodeAddresses, v1.NodeAddress{Type: v1.NodeHostName, Address: hostname})
				} else if existingHostnameAddress.Address != hostname {
					// override the Hostname address reported by the cloud provider
					klog.InfoS("Replacing cloudprovider-reported hostname with overridden hostname", "cloudProviderHostname", existingHostnameAddress.Address, "overriddenHostname", hostname)
					existingHostnameAddress.Address = hostname
				}
			}
			node.Status.Addresses = nodeAddresses
		} else if nodeIPSpecified && secondaryNodeIPSpecified {
			node.Status.Addresses = []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: nodeIP.String()},
				{Type: v1.NodeInternalIP, Address: secondaryNodeIP.String()},
				{Type: v1.NodeHostName, Address: hostname},
			}
		} else {
			var ipAddr net.IP
			var err error

			// 1) Use nodeIP if set (and not "0.0.0.0"/"::")
			// 2) If the user has specified an IP to HostnameOverride, use it
			// 3) Lookup the IP from node name by DNS
			// 4) Try to get the IP from the network interface used as default gateway
			//
			// For steps 3 and 4, IPv4 addresses are preferred to IPv6 addresses
			// unless nodeIP is "::", in which case it is reversed.
			if nodeIPSpecified {
				ipAddr = nodeIP
			} else if addr := netutils.ParseIPSloppy(hostname); addr != nil {
				ipAddr = addr
			} else {
				var addrs []net.IP
				addrs, _ = net.LookupIP(node.Name)
				for _, addr := range addrs {
					if err = validateNodeIPFunc(addr); err == nil {
						if isPreferredIPFamily(addr) {
							ipAddr = addr
							break
						} else if ipAddr == nil {
							ipAddr = addr
						}
					}
				}

				if ipAddr == nil {
					ipAddr, err = utilnet.ResolveBindAddress(nodeIP)
				}
			}

			if ipAddr == nil {
				// We tried everything we could, but the IP address wasn't fetchable; error out
				return fmt.Errorf("can't get ip address of node %s. error: %v", node.Name, err)
			}
			node.Status.Addresses = []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: ipAddr.String()},
				{Type: v1.NodeHostName, Address: hostname},
			}
		}
		return nil
	}
}

func hasAddressType(addresses []v1.NodeAddress, addressType v1.NodeAddressType) bool {
	for _, address := range addresses {
		if address.Type == addressType {
			return true
		}
	}
	return false
}
func hasAddressValue(addresses []v1.NodeAddress, addressValue string) bool {
	for _, address := range addresses {
		if address.Address == addressValue {
			return true
		}
	}
	return false
}

// MachineInfo returns a Setter that updates machine-related information on the node.
func MachineInfo(nodeName string,
	maxPods int,
	podsPerCore int,
	machineInfoFunc func() (*cadvisorapiv1.MachineInfo, error), // typically Kubelet.GetCachedMachineInfo
	capacityFunc func(localStorageCapacityIsolation bool) v1.ResourceList, // typically Kubelet.containerManager.GetCapacity
	devicePluginResourceCapacityFunc func() (v1.ResourceList, v1.ResourceList, []string), // typically Kubelet.containerManager.GetDevicePluginResourceCapacity
	nodeAllocatableReservationFunc func() v1.ResourceList, // typically Kubelet.containerManager.GetNodeAllocatableReservation
	recordEventFunc func(eventType, event, message string), // typically Kubelet.recordEvent
	localStorageCapacityIsolation bool,
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		// Note: avoid blindly overwriting the capacity in case opaque
		//       resources are being advertised.
		if node.Status.Capacity == nil {
			node.Status.Capacity = v1.ResourceList{}
		}

		var devicePluginAllocatable v1.ResourceList
		var devicePluginCapacity v1.ResourceList
		var removedDevicePlugins []string

		// TODO: Post NotReady if we cannot get MachineInfo from cAdvisor. This needs to start
		// cAdvisor locally, e.g. for test-cmd.sh, and in integration test.
		info, err := machineInfoFunc()
		if err != nil {
			// TODO(roberthbailey): This is required for test-cmd.sh to pass.
			// See if the test should be updated instead.
			node.Status.Capacity[v1.ResourceCPU] = *resource.NewMilliQuantity(0, resource.DecimalSI)
			node.Status.Capacity[v1.ResourceMemory] = resource.MustParse("0Gi")
			node.Status.Capacity[v1.ResourcePods] = *resource.NewQuantity(int64(maxPods), resource.DecimalSI)
			klog.ErrorS(err, "Error getting machine info")
		} else {
			node.Status.NodeInfo.MachineID = info.MachineID
			node.Status.NodeInfo.SystemUUID = info.SystemUUID

			for rName, rCap := range cadvisor.CapacityFromMachineInfo(info) {
				node.Status.Capacity[rName] = rCap
			}

			if podsPerCore > 0 {
				node.Status.Capacity[v1.ResourcePods] = *resource.NewQuantity(
					int64(math.Min(float64(info.NumCores*podsPerCore), float64(maxPods))), resource.DecimalSI)
			} else {
				node.Status.Capacity[v1.ResourcePods] = *resource.NewQuantity(
					int64(maxPods), resource.DecimalSI)
			}

			if node.Status.NodeInfo.BootID != "" &&
				node.Status.NodeInfo.BootID != info.BootID {
				// TODO: This requires a transaction, either both node status is updated
				// and event is recorded or neither should happen, see issue #6055.
				recordEventFunc(v1.EventTypeWarning, events.NodeRebooted,
					fmt.Sprintf("Node %s has been rebooted, boot id: %s", nodeName, info.BootID))
			}
			node.Status.NodeInfo.BootID = info.BootID

			// TODO: all the node resources should use ContainerManager.GetCapacity instead of deriving the
			// capacity for every node status request
			initialCapacity := capacityFunc(localStorageCapacityIsolation)
			if initialCapacity != nil {
				if v, exists := initialCapacity[v1.ResourceEphemeralStorage]; exists {
					node.Status.Capacity[v1.ResourceEphemeralStorage] = v
				}
			}
			//}

			devicePluginCapacity, devicePluginAllocatable, removedDevicePlugins = devicePluginResourceCapacityFunc()
			for k, v := range devicePluginCapacity {
				if old, ok := node.Status.Capacity[k]; !ok || old.Value() != v.Value() {
					klog.V(2).InfoS("Updated capacity for device plugin", "plugin", k, "capacity", v.Value())
				}
				node.Status.Capacity[k] = v
			}

			for _, removedResource := range removedDevicePlugins {
				klog.V(2).InfoS("Set capacity for removed resource to 0 on device removal", "device", removedResource)
				// Set the capacity of the removed resource to 0 instead of
				// removing the resource from the node status. This is to indicate
				// that the resource is managed by device plugin and had been
				// registered before.
				//
				// This is required to differentiate the device plugin managed
				// resources and the cluster-level resources, which are absent in
				// node status.
				node.Status.Capacity[v1.ResourceName(removedResource)] = *resource.NewQuantity(int64(0), resource.DecimalSI)
			}
		}

		// Set Allocatable.
		if node.Status.Allocatable == nil {
			node.Status.Allocatable = make(v1.ResourceList)
		}
		// Remove extended resources from allocatable that are no longer
		// present in capacity.
		for k := range node.Status.Allocatable {
			_, found := node.Status.Capacity[k]
			if !found && v1helper.IsExtendedResourceName(k) {
				delete(node.Status.Allocatable, k)
			}
		}
		allocatableReservation := nodeAllocatableReservationFunc()
		for k, v := range node.Status.Capacity {
			value := v.DeepCopy()
			if res, exists := allocatableReservation[k]; exists {
				value.Sub(res)
			}
			if value.Sign() < 0 {
				// Negative Allocatable resources don't make sense.
				value.Set(0)
			}
			node.Status.Allocatable[k] = value
		}

		for k, v := range devicePluginAllocatable {
			if old, ok := node.Status.Allocatable[k]; !ok || old.Value() != v.Value() {
				klog.V(2).InfoS("Updated allocatable", "device", k, "allocatable", v.Value())
			}
			node.Status.Allocatable[k] = v
		}
		// for every huge page reservation, we need to remove it from allocatable memory
		for k, v := range node.Status.Capacity {
			if v1helper.IsHugePageResourceName(k) {
				allocatableMemory := node.Status.Allocatable[v1.ResourceMemory]
				value := v.DeepCopy()
				allocatableMemory.Sub(value)
				if allocatableMemory.Sign() < 0 {
					// Negative Allocatable resources don't make sense.
					allocatableMemory.Set(0)
				}
				node.Status.Allocatable[v1.ResourceMemory] = allocatableMemory
			}
		}
		return nil
	}
}

// VersionInfo returns a Setter that updates version-related information on the node.
func VersionInfo(versionInfoFunc func() (*cadvisorapiv1.VersionInfo, error), // typically Kubelet.cadvisor.VersionInfo
	runtimeTypeFunc func() string, // typically Kubelet.containerRuntime.Type
	runtimeVersionFunc func(ctx context.Context) (kubecontainer.Version, error), // typically Kubelet.containerRuntime.Version
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		verinfo, err := versionInfoFunc()
		if err != nil {
			return fmt.Errorf("error getting version info: %v", err)
		}

		node.Status.NodeInfo.KernelVersion = verinfo.KernelVersion
		node.Status.NodeInfo.OSImage = verinfo.ContainerOsVersion

		runtimeVersion := "Unknown"
		if runtimeVer, err := runtimeVersionFunc(ctx); err == nil {
			runtimeVersion = runtimeVer.String()
		}
		node.Status.NodeInfo.ContainerRuntimeVersion = fmt.Sprintf("%s://%s", runtimeTypeFunc(), runtimeVersion)

		node.Status.NodeInfo.KubeletVersion = version.Get().String()

		if utilfeature.DefaultFeatureGate.Enabled(features.DisableNodeKubeProxyVersion) {
			// This field is deprecated and should be cleared if it was previously set.
			node.Status.NodeInfo.KubeProxyVersion = ""
		} else {
			node.Status.NodeInfo.KubeProxyVersion = version.Get().String()
		}

		return nil
	}
}

// DaemonEndpoints returns a Setter that updates the daemon endpoints on the node.
func DaemonEndpoints(daemonEndpoints *v1.NodeDaemonEndpoints) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		node.Status.DaemonEndpoints = *daemonEndpoints
		return nil
	}
}

// Images returns a Setter that updates the images on the node.
// imageListFunc is expected to return a list of images sorted in descending order by image size.
// nodeStatusMaxImages is ignored if set to -1.
func Images(nodeStatusMaxImages int32,
	imageListFunc func() ([]kubecontainer.Image, error), // typically Kubelet.imageManager.GetImageList
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		// Update image list of this node
		var imagesOnNode []v1.ContainerImage
		containerImages, err := imageListFunc()
		if err != nil {
			node.Status.Images = imagesOnNode
			return fmt.Errorf("error getting image list: %v", err)
		}
		// we expect imageListFunc to return a sorted list, so we just need to truncate
		if int(nodeStatusMaxImages) > -1 &&
			int(nodeStatusMaxImages) < len(containerImages) {
			containerImages = containerImages[0:nodeStatusMaxImages]
		}

		for _, image := range containerImages {
			// make a copy to avoid modifying slice members of the image items in the list
			names := append([]string{}, image.RepoDigests...)
			names = append(names, image.RepoTags...)
			// Report up to MaxNamesPerImageInNodeStatus names per image.
			if len(names) > MaxNamesPerImageInNodeStatus {
				names = names[0:MaxNamesPerImageInNodeStatus]
			}
			imagesOnNode = append(imagesOnNode, v1.ContainerImage{
				Names:     names,
				SizeBytes: image.Size,
			})
		}

		node.Status.Images = imagesOnNode
		return nil
	}
}

// GoRuntime returns a Setter that sets GOOS and GOARCH on the node.
func GoRuntime() Setter {
	return func(ctx context.Context, node *v1.Node) error {
		node.Status.NodeInfo.OperatingSystem = goruntime.GOOS
		node.Status.NodeInfo.Architecture = goruntime.GOARCH
		return nil
	}
}

// RuntimeHandlers returns a Setter that sets RuntimeHandlers on the node.
func RuntimeHandlers(fn func() []kubecontainer.RuntimeHandler) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		if !utilfeature.DefaultFeatureGate.Enabled(features.RecursiveReadOnlyMounts) {
			return nil
		}
		handlers := fn()
		node.Status.RuntimeHandlers = make([]v1.NodeRuntimeHandler, len(handlers))
		for i, h := range handlers {
			node.Status.RuntimeHandlers[i] = v1.NodeRuntimeHandler{
				Name: h.Name,
				Features: &v1.NodeRuntimeHandlerFeatures{
					RecursiveReadOnlyMounts: &h.SupportsRecursiveReadOnlyMounts,
				},
			}
		}
		return nil
	}
}

// ReadyCondition returns a Setter that updates the v1.NodeReady condition on the node.
func ReadyCondition(
	nowFunc func() time.Time, // typically Kubelet.clock.Now
	runtimeErrorsFunc func() error, // typically Kubelet.runtimeState.runtimeErrors
	networkErrorsFunc func() error, // typically Kubelet.runtimeState.networkErrors
	storageErrorsFunc func() error, // typically Kubelet.runtimeState.storageErrors
	cmStatusFunc func() cm.Status, // typically Kubelet.containerManager.Status
	nodeShutdownManagerErrorsFunc func() error, // typically kubelet.shutdownManager.errors.
	recordEventFunc func(eventType, event string), // typically Kubelet.recordNodeStatusEvent
	localStorageCapacityIsolation bool,
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		// NOTE(aaronlevy): NodeReady condition needs to be the last in the list of node conditions.
		// This is due to an issue with version skewed kubelet and master components.
		// ref: https://github.com/kubernetes/kubernetes/issues/16961
		currentTime := metav1.NewTime(nowFunc())
		newNodeReadyCondition := v1.NodeCondition{
			Type:              v1.NodeReady,
			Status:            v1.ConditionTrue,
			Reason:            "KubeletReady",
			Message:           "kubelet is posting ready status",
			LastHeartbeatTime: currentTime,
		}
		errs := []error{runtimeErrorsFunc(), networkErrorsFunc(), storageErrorsFunc(), nodeShutdownManagerErrorsFunc()}
		requiredCapacities := []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory, v1.ResourcePods}
		if localStorageCapacityIsolation {
			requiredCapacities = append(requiredCapacities, v1.ResourceEphemeralStorage)
		}
		missingCapacities := []string{}
		for _, resource := range requiredCapacities {
			if _, found := node.Status.Capacity[resource]; !found {
				missingCapacities = append(missingCapacities, string(resource))
			}
		}
		if len(missingCapacities) > 0 {
			errs = append(errs, fmt.Errorf("missing node capacity for resources: %s", strings.Join(missingCapacities, ", ")))
		}
		if aggregatedErr := errors.NewAggregate(errs); aggregatedErr != nil {
			newNodeReadyCondition = v1.NodeCondition{
				Type:              v1.NodeReady,
				Status:            v1.ConditionFalse,
				Reason:            "KubeletNotReady",
				Message:           aggregatedErr.Error(),
				LastHeartbeatTime: currentTime,
			}
		}

		// Record any soft requirements that were not met in the container manager.
		status := cmStatusFunc()
		if status.SoftRequirements != nil {
			newNodeReadyCondition.Message = fmt.Sprintf("%s. WARNING: %s", newNodeReadyCondition.Message, status.SoftRequirements.Error())
		}

		readyConditionUpdated := false
		needToRecordEvent := false
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == v1.NodeReady {
				if node.Status.Conditions[i].Status == newNodeReadyCondition.Status {
					newNodeReadyCondition.LastTransitionTime = node.Status.Conditions[i].LastTransitionTime
				} else {
					newNodeReadyCondition.LastTransitionTime = currentTime
					needToRecordEvent = true
				}
				node.Status.Conditions[i] = newNodeReadyCondition
				readyConditionUpdated = true
				break
			}
		}
		if !readyConditionUpdated {
			newNodeReadyCondition.LastTransitionTime = currentTime
			node.Status.Conditions = append(node.Status.Conditions, newNodeReadyCondition)
		}
		if needToRecordEvent {
			if newNodeReadyCondition.Status == v1.ConditionTrue {
				recordEventFunc(v1.EventTypeNormal, events.NodeReady)
			} else {
				recordEventFunc(v1.EventTypeNormal, events.NodeNotReady)
				klog.InfoS("Node became not ready", "node", klog.KObj(node), "condition", newNodeReadyCondition)
			}
		}
		return nil
	}
}

// MemoryPressureCondition returns a Setter that updates the v1.NodeMemoryPressure condition on the node.
func MemoryPressureCondition(nowFunc func() time.Time, // typically Kubelet.clock.Now
	pressureFunc func() bool, // typically Kubelet.evictionManager.IsUnderMemoryPressure
	recordEventFunc func(eventType, event string), // typically Kubelet.recordNodeStatusEvent
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		currentTime := metav1.NewTime(nowFunc())
		var condition *v1.NodeCondition

		// Check if NodeMemoryPressure condition already exists and if it does, just pick it up for update.
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == v1.NodeMemoryPressure {
				condition = &node.Status.Conditions[i]
			}
		}

		newCondition := false
		// If the NodeMemoryPressure condition doesn't exist, create one
		if condition == nil {
			condition = &v1.NodeCondition{
				Type:   v1.NodeMemoryPressure,
				Status: v1.ConditionUnknown,
			}
			// cannot be appended to node.Status.Conditions here because it gets
			// copied to the slice. So if we append to the slice here none of the
			// updates we make below are reflected in the slice.
			newCondition = true
		}

		// Update the heartbeat time
		condition.LastHeartbeatTime = currentTime

		// Note: The conditions below take care of the case when a new NodeMemoryPressure condition is
		// created and as well as the case when the condition already exists. When a new condition
		// is created its status is set to v1.ConditionUnknown which matches either
		// condition.Status != v1.ConditionTrue or
		// condition.Status != v1.ConditionFalse in the conditions below depending on whether
		// the kubelet is under memory pressure or not.
		if pressureFunc() {
			if condition.Status != v1.ConditionTrue {
				condition.Status = v1.ConditionTrue
				condition.Reason = "KubeletHasInsufficientMemory"
				condition.Message = "kubelet has insufficient memory available"
				condition.LastTransitionTime = currentTime
				recordEventFunc(v1.EventTypeNormal, "NodeHasInsufficientMemory")
			}
		} else if condition.Status != v1.ConditionFalse {
			condition.Status = v1.ConditionFalse
			condition.Reason = "KubeletHasSufficientMemory"
			condition.Message = "kubelet has sufficient memory available"
			condition.LastTransitionTime = currentTime
			recordEventFunc(v1.EventTypeNormal, "NodeHasSufficientMemory")
		}

		if newCondition {
			node.Status.Conditions = append(node.Status.Conditions, *condition)
		}
		return nil
	}
}

// PIDPressureCondition returns a Setter that updates the v1.NodePIDPressure condition on the node.
func PIDPressureCondition(nowFunc func() time.Time, // typically Kubelet.clock.Now
	pressureFunc func() bool, // typically Kubelet.evictionManager.IsUnderPIDPressure
	recordEventFunc func(eventType, event string), // typically Kubelet.recordNodeStatusEvent
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		currentTime := metav1.NewTime(nowFunc())
		var condition *v1.NodeCondition

		// Check if NodePIDPressure condition already exists and if it does, just pick it up for update.
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == v1.NodePIDPressure {
				condition = &node.Status.Conditions[i]
			}
		}

		newCondition := false
		// If the NodePIDPressure condition doesn't exist, create one
		if condition == nil {
			condition = &v1.NodeCondition{
				Type:   v1.NodePIDPressure,
				Status: v1.ConditionUnknown,
			}
			// cannot be appended to node.Status.Conditions here because it gets
			// copied to the slice. So if we append to the slice here none of the
			// updates we make below are reflected in the slice.
			newCondition = true
		}

		// Update the heartbeat time
		condition.LastHeartbeatTime = currentTime

		// Note: The conditions below take care of the case when a new NodePIDPressure condition is
		// created and as well as the case when the condition already exists. When a new condition
		// is created its status is set to v1.ConditionUnknown which matches either
		// condition.Status != v1.ConditionTrue or
		// condition.Status != v1.ConditionFalse in the conditions below depending on whether
		// the kubelet is under PID pressure or not.
		if pressureFunc() {
			if condition.Status != v1.ConditionTrue {
				condition.Status = v1.ConditionTrue
				condition.Reason = "KubeletHasInsufficientPID"
				condition.Message = "kubelet has insufficient PID available"
				condition.LastTransitionTime = currentTime
				recordEventFunc(v1.EventTypeNormal, "NodeHasInsufficientPID")
			}
		} else if condition.Status != v1.ConditionFalse {
			condition.Status = v1.ConditionFalse
			condition.Reason = "KubeletHasSufficientPID"
			condition.Message = "kubelet has sufficient PID available"
			condition.LastTransitionTime = currentTime
			recordEventFunc(v1.EventTypeNormal, "NodeHasSufficientPID")
		}

		if newCondition {
			node.Status.Conditions = append(node.Status.Conditions, *condition)
		}
		return nil
	}
}

// DiskPressureCondition returns a Setter that updates the v1.NodeDiskPressure condition on the node.
func DiskPressureCondition(nowFunc func() time.Time, // typically Kubelet.clock.Now
	pressureFunc func() bool, // typically Kubelet.evictionManager.IsUnderDiskPressure
	recordEventFunc func(eventType, event string), // typically Kubelet.recordNodeStatusEvent
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		currentTime := metav1.NewTime(nowFunc())
		var condition *v1.NodeCondition

		// Check if NodeDiskPressure condition already exists and if it does, just pick it up for update.
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == v1.NodeDiskPressure {
				condition = &node.Status.Conditions[i]
			}
		}

		newCondition := false
		// If the NodeDiskPressure condition doesn't exist, create one
		if condition == nil {
			condition = &v1.NodeCondition{
				Type:   v1.NodeDiskPressure,
				Status: v1.ConditionUnknown,
			}
			// cannot be appended to node.Status.Conditions here because it gets
			// copied to the slice. So if we append to the slice here none of the
			// updates we make below are reflected in the slice.
			newCondition = true
		}

		// Update the heartbeat time
		condition.LastHeartbeatTime = currentTime

		// Note: The conditions below take care of the case when a new NodeDiskPressure condition is
		// created and as well as the case when the condition already exists. When a new condition
		// is created its status is set to v1.ConditionUnknown which matches either
		// condition.Status != v1.ConditionTrue or
		// condition.Status != v1.ConditionFalse in the conditions below depending on whether
		// the kubelet is under disk pressure or not.
		if pressureFunc() {
			if condition.Status != v1.ConditionTrue {
				condition.Status = v1.ConditionTrue
				condition.Reason = "KubeletHasDiskPressure"
				condition.Message = "kubelet has disk pressure"
				condition.LastTransitionTime = currentTime
				recordEventFunc(v1.EventTypeNormal, "NodeHasDiskPressure")
			}
		} else if condition.Status != v1.ConditionFalse {
			condition.Status = v1.ConditionFalse
			condition.Reason = "KubeletHasNoDiskPressure"
			condition.Message = "kubelet has no disk pressure"
			condition.LastTransitionTime = currentTime
			recordEventFunc(v1.EventTypeNormal, "NodeHasNoDiskPressure")
		}

		if newCondition {
			node.Status.Conditions = append(node.Status.Conditions, *condition)
		}
		return nil
	}
}

// VolumesInUse returns a Setter that updates the volumes in use on the node.
func VolumesInUse(syncedFunc func() bool, // typically Kubelet.volumeManager.ReconcilerStatesHasBeenSynced
	volumesInUseFunc func() []v1.UniqueVolumeName, // typically Kubelet.volumeManager.GetVolumesInUse
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		// Make sure to only update node status after reconciler starts syncing up states
		if syncedFunc() {
			node.Status.VolumesInUse = volumesInUseFunc()
		}
		return nil
	}
}

// VolumeLimits returns a Setter that updates the volume limits on the node.
func VolumeLimits(volumePluginListFunc func() []volume.VolumePluginWithAttachLimits, // typically Kubelet.volumePluginMgr.ListVolumePluginWithLimits
) Setter {
	return func(ctx context.Context, node *v1.Node) error {
		if node.Status.Capacity == nil {
			node.Status.Capacity = v1.ResourceList{}
		}
		if node.Status.Allocatable == nil {
			node.Status.Allocatable = v1.ResourceList{}
		}

		pluginWithLimits := volumePluginListFunc()
		for _, volumePlugin := range pluginWithLimits {
			attachLimits, err := volumePlugin.GetVolumeLimits()
			if err != nil {
				klog.V(4).InfoS("Skipping volume limits for volume plugin", "plugin", volumePlugin.GetPluginName())
				continue
			}
			for limitKey, value := range attachLimits {
				node.Status.Capacity[v1.ResourceName(limitKey)] = *resource.NewQuantity(value, resource.DecimalSI)
				node.Status.Allocatable[v1.ResourceName(limitKey)] = *resource.NewQuantity(value, resource.DecimalSI)
			}
		}
		return nil
	}
}
