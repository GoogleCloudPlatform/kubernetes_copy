//go:build !linux && !windows
// +build !linux,!windows

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

package kuberuntime

import (
	v1 "k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// applyPlatformSpecificContainerConfig applies platform specific configurations to runtimeapi.ContainerConfig.
func (m *kubeGenericRuntimeManager) applyPlatformSpecificContainerConfig(config *runtimeapi.ContainerConfig, container *v1.Container, pod *v1.Pod, uid *int64, username string, nsTarget *kubecontainer.ContainerID) error {
	return nil
}

// generateContainerResources generates platform specific container resources config for runtime
func (m *kubeGenericRuntimeManager) generateContainerResources(pod *v1.Pod, container *v1.Container) *runtimeapi.ContainerResources {
	return nil
}

// generateUpdatePodSandboxResourcesRequest generates platform specific podsandox resources config for runtime
func (m *kubeGenericRuntimeManager) generateUpdatePodSandboxResourcesRequest(sandboxID string, pod *v1.Pod) *runtimeapi.UpdatePodSandboxResourcesRequest {
	return nil
}

func toKubeContainerResources(statusResources *runtimeapi.ContainerResources) *kubecontainer.ContainerResources {
	return nil
}

func toKubeContainerUser(statusUser *runtimeapi.ContainerUser) *kubecontainer.ContainerUser {
	return nil
}
