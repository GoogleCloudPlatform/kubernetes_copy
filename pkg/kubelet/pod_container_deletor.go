/*
Copyright 2016 The Kubernetes Authors.

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

package kubelet

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

const (
	// The limit on the number of buffered container deletion requests
	// This number is a bit arbitrary and may be adjusted in the future.
	containerDeletorBufferLimit = 50
)

type containerStatusbyCreatedList []*kubecontainer.Status

type podContainerDeletor struct {
	worker chan<- kubecontainer.ContainerID
}

func (a containerStatusbyCreatedList) Len() int      { return len(a) }
func (a containerStatusbyCreatedList) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a containerStatusbyCreatedList) Less(i, j int) bool {
	return a[i].CreatedAt.After(a[j].CreatedAt)
}

func newPodContainerDeletor(runtime kubecontainer.Runtime) *podContainerDeletor {
	buffer := make(chan kubecontainer.ContainerID, containerDeletorBufferLimit)
	go wait.Until(func() {
		for {
			id := <-buffer
			if err := runtime.DeleteContainer(context.Background(), id); err != nil {
				klog.InfoS("DeleteContainer returned error", "containerID", id, "err", err)
			}
		}
	}, 0, wait.NeverStop)

	return &podContainerDeletor{
		worker: buffer,
	}
}

// getContainersToDeleteInPod returns the exited containers in a pod whose name matches the name inferred from filterContainerId (if not empty), ordered by the creation time from the latest to the earliest.
// If filterContainerID is empty, all dead containers in the pod are returned.
func getContainersToDeleteInPod(filterContainerID string, podStatus *kubecontainer.PodStatus, containersToKeep int) containerStatusbyCreatedList {
	matchedContainer := func(filterContainerId string, podStatus *kubecontainer.PodStatus) *kubecontainer.Status {
		if filterContainerId == "" {
			return nil
		}
		for _, containerStatus := range podStatus.ContainerStatuses {
			if containerStatus.ID.ID == filterContainerId {
				return containerStatus
			}
		}
		return nil
	}(filterContainerID, podStatus)

	if filterContainerID != "" && matchedContainer == nil {
		klog.InfoS("Container not found in pod's containers", "containerID", filterContainerID)
		return containerStatusbyCreatedList{}
	}

	// Find the exited containers whose name matches the name of the container with id being filterContainerId
	var candidates containerStatusbyCreatedList
	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.State != kubecontainer.ContainerStateExited {
			continue
		}
		if matchedContainer == nil || matchedContainer.Name == containerStatus.Name {
			candidates = append(candidates, containerStatus)
		}
	}

	if len(candidates) <= containersToKeep {
		return containerStatusbyCreatedList{}
	}
	sort.Sort(candidates)
	return candidates[containersToKeep:]
}

// deleteContainersInPod issues container deletion requests for containers selected by getContainersToDeleteInPod.
func (p *podContainerDeletor) deleteContainersInPod(filterContainerID string, podStatus *kubecontainer.PodStatus, removeAll bool) {
	containersToKeep := 1
	if removeAll {
		containersToKeep = 0
		filterContainerID = ""
	}

	for _, candidate := range getContainersToDeleteInPod(filterContainerID, podStatus, containersToKeep) {
		select {
		case p.worker <- candidate.ID:
		default:
			klog.InfoS("Failed to issue the request to remove container", "containerID", candidate.ID)
		}
	}
}
