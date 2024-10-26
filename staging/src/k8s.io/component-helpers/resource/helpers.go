/*
Copyright 2024 The Kubernetes Authors.

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

package resource

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// ContainerType signifies container type
type ContainerType int

const (
	// Containers is for normal containers
	Containers ContainerType = 1 << iota
	// InitContainers is for init containers
	InitContainers
)

// PodResourcesOptions controls the behavior of PodRequests and PodLimits.
type PodResourcesOptions struct {
	// Reuse, if provided will be reused to accumulate resources and returned by the PodRequests or PodLimits
	// functions. All existing values in Reuse will be lost.
	Reuse v1.ResourceList
	// InPlacePodVerticalScalingEnabled indicates that the in-place pod vertical scaling feature gate is enabled.
	InPlacePodVerticalScalingEnabled bool
	// ExcludeOverhead controls if pod overhead is excluded from the calculation.
	ExcludeOverhead bool
	// ContainerFn is called with the effective resources required for each container within the pod.
	ContainerFn func(res v1.ResourceList, containerType ContainerType)
	// NonMissingContainerRequests if provided will replace any missing container level requests for the specified resources
	// with the given values.  If the requests for those resources are explicitly set, even if zero, they will not be modified.
	NonMissingContainerRequests v1.ResourceList
	// PodLevelResourcesEnabled indicates that the pod-level resources feature gate
	// is enabled.
	PodLevelResourcesEnabled bool
	// UseContainerLevelResources controls whether pod resource calculation should use
	// container-level resources to calculate PodRequests.
	UseContainerLevelResources bool
}

var supportedPodLevelResources = sets.NewString(string(v1.ResourceCPU), string(v1.ResourceMemory))

// IsSupportedPodLevelResources checks if a given resource is supported by pod-level
// resource management through the PodLevelResources feature. Returns true if
// the resource is supported.
func IsSupportedPodLevelResource(name v1.ResourceName) bool {
	return supportedPodLevelResources.Has(string(name))
}

// PodRequests computes the total pod requests per the PodResourcesOptions supplied.
// If PodResourcesOptions is nil, then the requests are returned including pod overhead.
// If the PodLevelResources feature is enabled AND the pod-level resources are set,
// those pod-level values are used in calculating Pod Requests.
// The computation is part of the API and must be reviewed as an API change.
func PodRequests(pod *v1.Pod, opts PodResourcesOptions) v1.ResourceList {
	// attempt to reuse the maps if passed, or allocate otherwise
	reqs := reuseOrClearResourceList(opts.Reuse)

	if !opts.UseContainerLevelResources && (opts.PodLevelResourcesEnabled && pod.Spec.Resources != nil) {
		addResourceList(reqs, pod.Spec.Resources.Requests)
	} else {
		addResourceList(reqs, effectiveContainersRequests(pod, opts))
	}

	// Add overhead for running a pod to the sum of requests if requested:
	if !opts.ExcludeOverhead && pod.Spec.Overhead != nil {
		addResourceList(reqs, pod.Spec.Overhead)
	}

	return reqs
}

// effectiveContainersRequests computes the effective resource requests of all the containers
// in a pod. This computation folows the formula defined in the KEP for sidecar
// containers. See https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/753-sidecar-containers#resources-calculation-for-scheduling-and-pod-admission
// for more details.
func effectiveContainersRequests(pod *v1.Pod, opts PodResourcesOptions) v1.ResourceList {
	reqs := v1.ResourceList{}
	var containerStatuses map[string]*v1.ContainerStatus
	if opts.InPlacePodVerticalScalingEnabled {
		containerStatuses = make(map[string]*v1.ContainerStatus, len(pod.Status.ContainerStatuses))
		for i := range pod.Status.ContainerStatuses {
			containerStatuses[pod.Status.ContainerStatuses[i].Name] = &pod.Status.ContainerStatuses[i]
		}
	}

	for _, container := range pod.Spec.Containers {
		containerReqs := container.Resources.Requests
		if opts.InPlacePodVerticalScalingEnabled {
			cs, found := containerStatuses[container.Name]
			if found {
				if pod.Status.Resize == v1.PodResizeStatusInfeasible {
					containerReqs = cs.AllocatedResources.DeepCopy()
				} else {
					containerReqs = max(container.Resources.Requests, cs.AllocatedResources)
				}
			}
		}

		if len(opts.NonMissingContainerRequests) > 0 {
			containerReqs = applyNonMissing(containerReqs, opts.NonMissingContainerRequests)
		}

		if opts.ContainerFn != nil {
			opts.ContainerFn(containerReqs, Containers)
		}

		addResourceList(reqs, containerReqs)
	}

	restartableInitContainerReqs := v1.ResourceList{}
	initContainerReqs := v1.ResourceList{}
	// init containers define the minimum of any resource
	// Note: In-place resize is not allowed for InitContainers, so no need to check for ResizeStatus value
	//
	// Let's say `InitContainerUse(i)` is the resource requirements when the i-th
	// init container is initializing, then
	// `InitContainerUse(i) = sum(Resources of restartable init containers with index < i) + Resources of i-th init container`.
	//
	// See https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/753-sidecar-containers#exposing-pod-resource-requirements for the detail.
	for _, container := range pod.Spec.InitContainers {
		containerReqs := container.Resources.Requests
		if len(opts.NonMissingContainerRequests) > 0 {
			containerReqs = applyNonMissing(containerReqs, opts.NonMissingContainerRequests)
		}

		if container.RestartPolicy != nil && *container.RestartPolicy == v1.ContainerRestartPolicyAlways {
			// and add them to the resulting cumulative container requests
			addResourceList(reqs, containerReqs)

			// track our cumulative restartable init container resources
			addResourceList(restartableInitContainerReqs, containerReqs)
			containerReqs = restartableInitContainerReqs
		} else {
			tmp := v1.ResourceList{}
			addResourceList(tmp, containerReqs)
			addResourceList(tmp, restartableInitContainerReqs)
			containerReqs = tmp
		}

		if opts.ContainerFn != nil {
			opts.ContainerFn(containerReqs, InitContainers)
		}
		maxResourceList(initContainerReqs, containerReqs)
	}

	maxResourceList(reqs, initContainerReqs)
	return reqs
}

// applyNonMissing will return a copy of the given resource list with any missing values replaced by the nonMissing values
func applyNonMissing(reqs v1.ResourceList, nonMissing v1.ResourceList) v1.ResourceList {
	cp := v1.ResourceList{}
	for k, v := range reqs {
		cp[k] = v.DeepCopy()
	}

	for k, v := range nonMissing {
		if _, found := reqs[k]; !found {
			rk := cp[k]
			rk.Add(v)
			cp[k] = rk
		}
	}
	return cp
}

// PodLimits computes the pod limits per the PodResourcesOptions supplied. If PodResourcesOptions is nil, then
// the limits are returned including pod overhead for any non-zero limits. The computation is part of the API and must be reviewed
// as an API change.
func PodLimits(pod *v1.Pod, opts PodResourcesOptions) v1.ResourceList {
	// attempt to reuse the maps if passed, or allocate otherwise
	limits := reuseOrClearResourceList(opts.Reuse)

	for _, container := range pod.Spec.Containers {
		if opts.ContainerFn != nil {
			opts.ContainerFn(container.Resources.Limits, Containers)
		}
		addResourceList(limits, container.Resources.Limits)
	}

	restartableInitContainerLimits := v1.ResourceList{}
	initContainerLimits := v1.ResourceList{}
	// init containers define the minimum of any resource
	//
	// Let's say `InitContainerUse(i)` is the resource requirements when the i-th
	// init container is initializing, then
	// `InitContainerUse(i) = sum(Resources of restartable init containers with index < i) + Resources of i-th init container`.
	//
	// See https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/753-sidecar-containers#exposing-pod-resource-requirements for the detail.
	for _, container := range pod.Spec.InitContainers {
		containerLimits := container.Resources.Limits
		// Is the init container marked as a restartable init container?
		if container.RestartPolicy != nil && *container.RestartPolicy == v1.ContainerRestartPolicyAlways {
			addResourceList(limits, containerLimits)

			// track our cumulative restartable init container resources
			addResourceList(restartableInitContainerLimits, containerLimits)
			containerLimits = restartableInitContainerLimits
		} else {
			tmp := v1.ResourceList{}
			addResourceList(tmp, containerLimits)
			addResourceList(tmp, restartableInitContainerLimits)
			containerLimits = tmp
		}

		if opts.ContainerFn != nil {
			opts.ContainerFn(containerLimits, InitContainers)
		}
		maxResourceList(initContainerLimits, containerLimits)
	}

	maxResourceList(limits, initContainerLimits)

	// Add overhead to non-zero limits if requested:
	if !opts.ExcludeOverhead && pod.Spec.Overhead != nil {
		for name, quantity := range pod.Spec.Overhead {
			if value, ok := limits[name]; ok && !value.IsZero() {
				value.Add(quantity)
				limits[name] = value
			}
		}
	}

	return limits
}

// addResourceList adds the resources in newList to list.
func addResourceList(list, newList v1.ResourceList) {
	for name, quantity := range newList {
		if value, ok := list[name]; !ok {
			list[name] = quantity.DeepCopy()
		} else {
			value.Add(quantity)
			list[name] = value
		}
	}
}

// maxResourceList sets list to the greater of list/newList for every resource in newList
func maxResourceList(list, newList v1.ResourceList) {
	for name, quantity := range newList {
		if value, ok := list[name]; !ok || quantity.Cmp(value) > 0 {
			list[name] = quantity.DeepCopy()
		}
	}
}

// max returns the result of max(a, b) for each named resource and is only used if we can't
// accumulate into an existing resource list
func max(a v1.ResourceList, b v1.ResourceList) v1.ResourceList {
	result := v1.ResourceList{}
	for key, value := range a {
		if other, found := b[key]; found {
			if value.Cmp(other) <= 0 {
				result[key] = other.DeepCopy()
				continue
			}
		}
		result[key] = value.DeepCopy()
	}
	for key, value := range b {
		if _, found := result[key]; !found {
			result[key] = value.DeepCopy()
		}
	}
	return result
}

// reuseOrClearResourceList is a helper for avoiding excessive allocations of
// resource lists within the inner loop of resource calculations.
func reuseOrClearResourceList(reuse v1.ResourceList) v1.ResourceList {
	if reuse == nil {
		return make(v1.ResourceList, 4)
	}
	for k := range reuse {
		delete(reuse, k)
	}
	return reuse
}
