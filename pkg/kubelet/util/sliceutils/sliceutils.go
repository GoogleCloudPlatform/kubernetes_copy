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

package sliceutils

import (
	v1 "k8s.io/api/core/v1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// ByImageSize makes an array of images sortable by their size in descending
// order.
type ByImageSize []kubecontainer.Image

func (a ByImageSize) Less(i, j int) bool {
	if a[i].Size == a[j].Size {
		return a[i].ID > a[j].ID
	}
	return a[i].Size > a[j].Size
}
func (a ByImageSize) Len() int      { return len(a) }
func (a ByImageSize) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// PodsByPriority makes an array of pods sortable by their priority
// in descending order, and then by their creation timestamps in
// ascending order
type PodsByPriority []*v1.Pod

func (s PodsByPriority) Len() int {
	return len(s)
}

func (s PodsByPriority) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s PodsByPriority) Less(i, j int) bool {
	iPrio := getPodPriority(s[i])
	jPrio := getPodPriority(s[j])

	if iPrio > jPrio {
		return true
	}
	if iPrio == jPrio {
		return s[i].CreationTimestamp.Before(&s[j].CreationTimestamp)
	}
	return false
}

func getPodPriority(pod *v1.Pod) int32 {
	if pod == nil || pod.Spec.Priority == nil {
		return 0
	}

	return *pod.Spec.Priority
}
