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

package state

import (
	"fmt"
	"strings"

	"k8s.io/utils/cpuset"
)

// ContainerCPUAssignments type used in cpu manager state
type ContainerCPUAssignments map[string]map[string]cpuset.CPUSet

// Clone returns a copy of ContainerCPUAssignments
func (as ContainerCPUAssignments) Clone() ContainerCPUAssignments {
	ret := make(ContainerCPUAssignments, len(as))
	for pod := range as {
		ret[pod] = make(map[string]cpuset.CPUSet, len(as[pod]))
		for container, cset := range as[pod] {
			ret[pod][container] = cset
		}
	}
	return ret
}

// String provides a constructed representation of ContainerCPUAssignments
func (as ContainerCPUAssignments) String() string {
	var sb strings.Builder
	sb.WriteString("{")
	podsCount := len(as)
	for pod, containerMap := range as {
		sb.WriteString(fmt.Sprintf("%q:{", pod))

		containersCount := len(containerMap)
		for container, cpus := range containerMap {
			sb.WriteString(fmt.Sprintf("%q:%q", container, cpus.String()))
			if containersCount > 1 {
				sb.WriteString(",")
			}
			containersCount--
		}
		sb.WriteString("}")
		if podsCount > 1 {
			sb.WriteString(",")
		}
		podsCount--
	}
	sb.WriteString("}")
	return sb.String()
}

// Reader interface used to read current cpu/pod assignment state
type Reader interface {
	GetCPUSet(podUID string, containerName string) (cpuset.CPUSet, bool)
	GetDefaultCPUSet() cpuset.CPUSet
	GetCPUSetOrDefault(podUID string, containerName string) cpuset.CPUSet
	GetCPUAssignments() ContainerCPUAssignments
}

type writer interface {
	SetCPUSet(podUID string, containerName string, cpuset cpuset.CPUSet)
	SetDefaultCPUSet(cpuset cpuset.CPUSet)
	SetCPUAssignments(ContainerCPUAssignments)
	Delete(podUID string, containerName string)
	ClearState()
}

// State interface provides methods for tracking and setting cpu/pod assignment
type State interface {
	Reader
	writer
}
