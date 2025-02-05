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

// NOTE: DO NOT use those helper functions through client-go, the
// package path will be changed in the future.
package qos

import (
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
)

var supportedQoSComputeResources = sets.NewString(string(core.ResourceCPU), string(core.ResourceMemory))

func isSupportedQoSComputeResource(name core.ResourceName) bool {
	return supportedQoSComputeResources.Has(string(name))
}

// GetPodQOS returns the QoS class of a pod persisted in the PodStatus.QOSClass field.
// If PodStatus.QOSClass is empty, it returns value of ComputePodQOS() which evaluates pod's QoS class.
func GetPodQOS(pod *core.Pod) core.PodQOSClass {
	if pod.Status.QOSClass != "" {
		return pod.Status.QOSClass
	}
	return ComputePodQOS(pod)
}

// zeroQuantity represents a resource.Quantity with value "0", used as a baseline
// for resource comparisons.
var zeroQuantity = resource.MustParse("0")

// processResourceList adds non-zero quantities for supported QoS compute resources
// quantities from newList to list.
func processResourceList(list, newList core.ResourceList) {
	for name, quantity := range newList {
		if !isSupportedQoSComputeResource(name) {
			continue
		}
		if quantity.Cmp(zeroQuantity) == 1 {
			delta := quantity.DeepCopy()
			if _, exists := list[name]; !exists {
				list[name] = delta
			} else {
				delta.Add(list[name])
				list[name] = delta
			}
		}
	}
}

// getQOSResources returns a set of resource names from the provided resource list that:
// 1. Are supported QoS compute resources
// 2. Have quantities greater than zero
func getQOSResources(list core.ResourceList) sets.Set[string] {
	qosResources := sets.New[string]()
	for name, quantity := range list {
		if !isSupportedQoSComputeResource(name) {
			continue
		}
		if quantity.Cmp(zeroQuantity) == 1 {
			qosResources.Insert(string(name))
		}
	}
	return qosResources
}

// ComputePodQOS evaluates the list of containers to determine a pod's QoS class. This function is more
// expensive than GetPodQOS which should be used for pods having a non-empty .Status.QOSClass.
// A pod is besteffort if none of its containers have specified any requests or limits.
// A pod is guaranteed only when requests and limits are specified for all the containers and they are equal.
// A pod is burstable if limits and requests do not match across all containers.
// When this function is updated please also update staging/src/k8s.io/kubectl/pkg/util/qos/qos.go
func ComputePodQOS(pod *core.Pod) core.PodQOSClass {
	requests := core.ResourceList{}
	limits := core.ResourceList{}
	isGuaranteed := true
	// When pod-level resources are specified, we use them to determine QoS class.
	if utilfeature.DefaultFeatureGate.Enabled(features.PodLevelResources) &&
		pod.Spec.Resources != nil {
		if len(pod.Spec.Resources.Requests) > 0 {
			// process requests
			processResourceList(requests, pod.Spec.Resources.Requests)
		}

		if len(pod.Spec.Resources.Limits) > 0 {
			// process limits
			processResourceList(limits, pod.Spec.Resources.Limits)
			qosLimitResources := getQOSResources(pod.Spec.Resources.Limits)
			if !qosLimitResources.HasAll(string(core.ResourceMemory), string(core.ResourceCPU)) {
				isGuaranteed = false
			}
		}
	} else {
		for _, container := range getAllContainers(pod) {
			// Use a logical AND operation to accumulate the isGuaranteed status,
			// ensuring that all containers must meet the Guaranteed condition.
			isGuaranteed = processContainerResources(container, &requests, &limits) && isGuaranteed
		}
	}

	if len(requests) == 0 && len(limits) == 0 {
		return core.PodQOSBestEffort
	}
	// Check if requests match limits for all resources.
	if isGuaranteed && areRequestsMatchingLimits(requests, limits) {
		return core.PodQOSGuaranteed
	}
	return core.PodQOSBurstable
}

// processContainerResources processes the resources of a single container and updates the provided requests and limits lists.
func processContainerResources(container core.Container, requests, limits *core.ResourceList) bool {
	isGuaranteed := true
	processResourceList(*requests, container.Resources.Requests)
	qosLimitsFound := getQOSResources(container.Resources.Limits)
	processResourceList(*limits, container.Resources.Limits)
	if !qosLimitsFound.HasAll(string(core.ResourceMemory), string(core.ResourceCPU)) {
		isGuaranteed = false
	}
	return isGuaranteed
}

func getAllContainers(pod *core.Pod) []core.Container {
	allContainers := []core.Container{}
	allContainers = append(allContainers, pod.Spec.Containers...)
	allContainers = append(allContainers, pod.Spec.InitContainers...)
	return allContainers
}

// areRequestsMatchingLimits checks if all resource requests match their respective limits.
func areRequestsMatchingLimits(requests, limits core.ResourceList) bool {
	for name, req := range requests {
		if lim, exists := limits[name]; !exists || lim.Cmp(req) != 0 {
			return false
		}
	}
	return len(requests) == len(limits)
}
