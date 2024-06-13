//go:build linux
// +build linux

/*
Copyright 2021 The Kubernetes Authors.

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
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (i *internalContainerLifecycleImpl) PreCreateContainer(pod *v1.Pod, container *v1.Container, containerConfig *runtimeapi.ContainerConfig) error {
	if i.cpuManager != nil {
		allocatedCPUs := i.cpuManager.GetCPUAffinity(string(pod.UID), container.Name)
		if !allocatedCPUs.IsEmpty() {
			containerConfig.Linux.Resources.CpusetCpus = allocatedCPUs.String()
		}

		// Remove CPU quota only for containers that requested exclusive CPUs
		// The container should have exclusive CPUs only when:
		// 1. The pod has guaranteed QoS
		// 2. The container requested whole CPUs
		exclusiveCPUs := i.cpuManager.GetExclusiveCPUs(string(pod.UID), container.Name)
		if !exclusiveCPUs.IsEmpty() {
			containerConfig.Linux.Resources.CpuQuota = -1
		}
	}

	if i.memoryManager != nil {
		numaNodes := i.memoryManager.GetMemoryNUMANodes(pod, container)
		if numaNodes.Len() > 0 {
			var affinity []string
			for _, numaNode := range sets.List(numaNodes) {
				affinity = append(affinity, strconv.Itoa(numaNode))
			}
			containerConfig.Linux.Resources.CpusetMems = strings.Join(affinity, ",")
		}
	}

	return nil
}
