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

package resource

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	resourcehelper "k8s.io/component-helpers/resource"
)

// PodRequestsAndLimits returns a dictionary of all defined resources summed up for all
// containers of the pod. If pod overhead is non-nil, the pod overhead is added to the
// total container resource requests and to the total container limits which have a
// non-zero quantity.
func PodRequestsAndLimits(pod *corev1.Pod) (reqs, limits corev1.ResourceList) {
	return podRequests(pod), podLimits(pod)
}

func podRequests(pod *corev1.Pod) corev1.ResourceList {
	reqs := resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{
		UseStatusResources: true,
	})
	return reqs
}

func podLimits(pod *corev1.Pod) corev1.ResourceList {
	limits := resourcehelper.PodLimits(pod, resourcehelper.PodResourcesOptions{})
	return limits
}

// ExtractContainerResourceValue extracts the value of a resource
// in an already known container
func ExtractContainerResourceValue(fs *corev1.ResourceFieldSelector, container *corev1.Container) (string, error) {
	divisor := resource.Quantity{}
	if divisor.Cmp(fs.Divisor) == 0 {
		divisor = resource.MustParse("1")
	} else {
		divisor = fs.Divisor
	}

	switch fs.Resource {
	case "limits.cpu":
		return convertResourceCPUToString(container.Resources.Limits.Cpu(), divisor)
	case "limits.memory":
		return convertResourceMemoryToString(container.Resources.Limits.Memory(), divisor)
	case "limits.ephemeral-storage":
		return convertResourceEphemeralStorageToString(container.Resources.Limits.StorageEphemeral(), divisor)
	case "requests.cpu":
		return convertResourceCPUToString(container.Resources.Requests.Cpu(), divisor)
	case "requests.memory":
		return convertResourceMemoryToString(container.Resources.Requests.Memory(), divisor)
	case "requests.ephemeral-storage":
		return convertResourceEphemeralStorageToString(container.Resources.Requests.StorageEphemeral(), divisor)
	}
	// handle extended standard resources with dynamic names
	// example: requests.hugepages-<pageSize> or limits.hugepages-<pageSize>
	if strings.HasPrefix(fs.Resource, "requests.") {
		resourceName := corev1.ResourceName(strings.TrimPrefix(fs.Resource, "requests."))
		if IsHugePageResourceName(resourceName) {
			return convertResourceHugePagesToString(container.Resources.Requests.Name(resourceName, resource.BinarySI), divisor)
		}
	}
	if strings.HasPrefix(fs.Resource, "limits.") {
		resourceName := corev1.ResourceName(strings.TrimPrefix(fs.Resource, "limits."))
		if IsHugePageResourceName(resourceName) {
			return convertResourceHugePagesToString(container.Resources.Limits.Name(resourceName, resource.BinarySI), divisor)
		}
	}
	return "", fmt.Errorf("Unsupported container resource : %v", fs.Resource)
}

// convertResourceCPUToString converts cpu value to the format of divisor and returns
// ceiling of the value.
func convertResourceCPUToString(cpu *resource.Quantity, divisor resource.Quantity) (string, error) {
	c := int64(math.Ceil(float64(cpu.MilliValue()) / float64(divisor.MilliValue())))
	return strconv.FormatInt(c, 10), nil
}

// convertResourceMemoryToString converts memory value to the format of divisor and returns
// ceiling of the value.
func convertResourceMemoryToString(memory *resource.Quantity, divisor resource.Quantity) (string, error) {
	m := int64(math.Ceil(float64(memory.Value()) / float64(divisor.Value())))
	return strconv.FormatInt(m, 10), nil
}

// convertResourceHugePagesToString converts hugepages value to the format of divisor and returns
// ceiling of the value.
func convertResourceHugePagesToString(hugePages *resource.Quantity, divisor resource.Quantity) (string, error) {
	m := int64(math.Ceil(float64(hugePages.Value()) / float64(divisor.Value())))
	return strconv.FormatInt(m, 10), nil
}

// convertResourceEphemeralStorageToString converts ephemeral storage value to the format of divisor and returns
// ceiling of the value.
func convertResourceEphemeralStorageToString(ephemeralStorage *resource.Quantity, divisor resource.Quantity) (string, error) {
	m := int64(math.Ceil(float64(ephemeralStorage.Value()) / float64(divisor.Value())))
	return strconv.FormatInt(m, 10), nil
}

var standardContainerResources = sets.New[string](
	string(corev1.ResourceCPU),
	string(corev1.ResourceMemory),
	string(corev1.ResourceEphemeralStorage),
)

// IsStandardContainerResourceName returns true if the container can make a resource request
// for the specified resource
func IsStandardContainerResourceName(str string) bool {
	return standardContainerResources.Has(str) || IsHugePageResourceName(corev1.ResourceName(str))
}

// IsHugePageResourceName returns true if the resource name has the huge page
// resource prefix.
func IsHugePageResourceName(name corev1.ResourceName) bool {
	return strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix)
}
