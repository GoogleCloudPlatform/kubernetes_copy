/*
Copyright 2014 The Kubernetes Authors.

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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type resourceType string

const (
	resourceTypeRequest resourceType = "request"
	resourceTypeLimit   resourceType = "limit"
)

func getResourceQuantity(pod *v1.Pod, resourceName v1.ResourceName, resourceType resourceType) resource.Quantity {
	requestQuantity := resource.Quantity{}

	switch resourceName {
	case v1.ResourceCPU:
		requestQuantity = resource.Quantity{Format: resource.DecimalSI}
	case v1.ResourceMemory, v1.ResourceStorage, v1.ResourceEphemeralStorage:
		requestQuantity = resource.Quantity{Format: resource.BinarySI}
	default:
		requestQuantity = resource.Quantity{Format: resource.DecimalSI}
	}

	getResource := func(resources v1.ResourceRequirements) v1.ResourceList {
		switch resourceType {
		case resourceTypeRequest:
			return resources.Requests
		case resourceTypeLimit:
			return resources.Limits
		default:
			panic(fmt.Sprintf("unknown resource type: %v", resourceType))
		}
	}

	for _, container := range pod.Spec.Containers {
		if rQuantity, ok := getResource(container.Resources)[resourceName]; ok {
			requestQuantity.Add(rQuantity)
		}
	}

	for _, container := range pod.Spec.InitContainers {
		if rQuantity, ok := getResource(container.Resources)[resourceName]; ok {
			if requestQuantity.Cmp(rQuantity) < 0 {
				requestQuantity = rQuantity.DeepCopy()
			}
		}
	}

	// Add overhead for running a pod
	// to the total requests if the resource total is non-zero
	if pod.Spec.Overhead != nil {
		if podOverhead, ok := pod.Spec.Overhead[resourceName]; ok && !requestQuantity.IsZero() {
			requestQuantity.Add(podOverhead)
		}
	}

	return requestQuantity
}

// GetResourceRequestQuantity finds and returns the request quantity for a specific resource.
func GetResourceRequestQuantity(pod *v1.Pod, resourceName v1.ResourceName) resource.Quantity {
	return getResourceQuantity(pod, resourceName, resourceTypeRequest)
}

func GetResourceLimitQuantity(pod *v1.Pod, resourceName v1.ResourceName) resource.Quantity {
	return getResourceQuantity(pod, resourceName, resourceTypeLimit)
}

// GetResourceRequest finds and returns the request value for a specific resource.
func GetResourceRequest(pod *v1.Pod, resource v1.ResourceName) int64 {
	if resource == v1.ResourcePods {
		return 1
	}

	requestQuantity := GetResourceRequestQuantity(pod, resource)

	if resource == v1.ResourceCPU {
		return requestQuantity.MilliValue()
	}

	return requestQuantity.Value()
}

// ExtractResourceValueByContainerName extracts the value of a resource
// by providing container name
func ExtractResourceValueByContainerName(fs *v1.ResourceFieldSelector, pod *v1.Pod, containerName string) (string, error) {
	container, err := findContainerInPod(pod, containerName)
	if err != nil {
		return "", err
	}
	return ExtractContainerResourceValue(fs, container)
}

// ExtractResourceValueByContainerNameAndNodeAllocatable extracts the value of a resource
// by providing container name and node allocatable
func ExtractResourceValueByContainerNameAndNodeAllocatable(fs *v1.ResourceFieldSelector, pod *v1.Pod, containerName string, nodeAllocatable v1.ResourceList) (string, error) {
	realContainer, err := findContainerInPod(pod, containerName)
	if err != nil {
		return "", err
	}

	container := realContainer.DeepCopy()

	MergeContainerResourceLimits(container, nodeAllocatable)

	return ExtractContainerResourceValue(fs, container)
}

// ExtractContainerResourceValue extracts the value of a resource
// in an already known container
func ExtractContainerResourceValue(fs *v1.ResourceFieldSelector, container *v1.Container) (string, error) {
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
		resourceName := v1.ResourceName(strings.TrimPrefix(fs.Resource, "requests."))
		if IsHugePageResourceName(resourceName) {
			return convertResourceHugePagesToString(container.Resources.Requests.Name(resourceName, resource.BinarySI), divisor)
		}
	}
	if strings.HasPrefix(fs.Resource, "limits.") {
		resourceName := v1.ResourceName(strings.TrimPrefix(fs.Resource, "limits."))
		if IsHugePageResourceName(resourceName) {
			return convertResourceHugePagesToString(container.Resources.Limits.Name(resourceName, resource.BinarySI), divisor)
		}
	}
	return "", fmt.Errorf("unsupported container resource : %v", fs.Resource)
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

// findContainerInPod finds a container by its name in the provided pod
func findContainerInPod(pod *v1.Pod, containerName string) (*v1.Container, error) {
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			return &container, nil
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if container.Name == containerName {
			return &container, nil
		}
	}
	return nil, fmt.Errorf("container %s not found", containerName)
}

// MergeContainerResourceLimits checks if a limit is applied for
// the container, and if not, it sets the limit to the passed resource list.
func MergeContainerResourceLimits(container *v1.Container,
	allocatable v1.ResourceList) {
	if container.Resources.Limits == nil {
		container.Resources.Limits = make(v1.ResourceList)
	}
	// NOTE: we exclude hugepages-* resources because hugepages are never overcommitted.
	// This means that the container always has a limit specified.
	for _, resource := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory, v1.ResourceEphemeralStorage} {
		if quantity, exists := container.Resources.Limits[resource]; !exists || quantity.IsZero() {
			if cap, exists := allocatable[resource]; exists {
				container.Resources.Limits[resource] = cap.DeepCopy()
			}
		}
	}
}

// IsHugePageResourceName returns true if the resource name has the huge page
// resource prefix.
func IsHugePageResourceName(name v1.ResourceName) bool {
	return strings.HasPrefix(string(name), v1.ResourceHugePagesPrefix)
}
