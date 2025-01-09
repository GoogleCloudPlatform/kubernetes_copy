/*
Copyright 2015 The Kubernetes Authors.

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

package testing

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/kubelet/container"
)

// FakeManager simulates a prober.Manager for testing.
type FakeManager struct{}

// Unused methods below.

// AddPod simulates adding a Pod.
func (FakeManager) AddPod(_ *v1.Pod, _ container.GetEnvsFunc) {}

// RemovePod simulates removing a Pod.
func (FakeManager) RemovePod(_ *v1.Pod) {}

// Simulated stopping liveness and startup probes.
func (FakeManager) StopLivenessAndStartup(_ *v1.Pod) {}

// CleanupPods simulates cleaning up Pods.
func (FakeManager) CleanupPods(_ map[types.UID]sets.Empty) {}

// Start simulates start syncing the probe status
func (FakeManager) Start() {}

// UpdatePodStatus simulates updating the Pod Status.
func (FakeManager) UpdatePodStatus(_ *v1.Pod, podStatus *v1.PodStatus) {
	for i := range podStatus.ContainerStatuses {
		podStatus.ContainerStatuses[i].Ready = true
	}
}
