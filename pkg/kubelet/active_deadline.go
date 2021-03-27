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
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/utils/clock"
)

const (
	reason  = "DeadlineExceeded"
	message = "Pod was active on the node longer than the specified deadline"
)

// activeDeadlineHandler knows how to enforce active deadlines on pods.
type activeDeadlineHandler struct {
	// the clock to use for deadline enforcement
	clock clock.Clock
	// the provider of pod status
	podStatusProvider status.PodStatusProvider
	// the recorder to dispatch events when we identify a pod has exceeded active deadline
	recorder events.EventRecorder
}

// newActiveDeadlineHandler returns an active deadline handler that can enforce pod active deadline
func newActiveDeadlineHandler(
	podStatusProvider status.PodStatusProvider,
	recorder events.EventRecorder,
	clock clock.Clock,
) (*activeDeadlineHandler, error) {

	// check for all required fields
	if clock == nil || podStatusProvider == nil || recorder == nil {
		return nil, fmt.Errorf("required arguments must not be nil: %v, %v, %v", clock, podStatusProvider, recorder)
	}
	return &activeDeadlineHandler{
		clock:             clock,
		podStatusProvider: podStatusProvider,
		recorder:          recorder,
	}, nil
}

// ShouldSync returns true if the pod is past its active deadline.
func (m *activeDeadlineHandler) ShouldSync(pod *v1.Pod) bool {
	return m.pastActiveDeadline(pod)
}

// ShouldEvict returns true if the pod is past its active deadline.
// It dispatches an event that the pod should be evicted if it is past its deadline.
func (m *activeDeadlineHandler) ShouldEvict(pod *v1.Pod) lifecycle.ShouldEvictResponse {
	if !m.pastActiveDeadline(pod) {
		return lifecycle.ShouldEvictResponse{Evict: false}
	}
	m.recorder.Eventf(pod, nil, v1.EventTypeNormal, reason, "EvictingPod", message)
	return lifecycle.ShouldEvictResponse{Evict: true, Reason: reason, Message: message}
}

// pastActiveDeadline returns true if the pod has been active for more than its ActiveDeadlineSeconds
func (m *activeDeadlineHandler) pastActiveDeadline(pod *v1.Pod) bool {
	// no active deadline was specified
	if pod.Spec.ActiveDeadlineSeconds == nil {
		return false
	}
	// get the latest status to determine if it was started
	podStatus, ok := m.podStatusProvider.GetPodStatus(pod.UID)
	if !ok {
		podStatus = pod.Status
	}
	// we have no start time so just return
	if podStatus.StartTime.IsZero() {
		return false
	}
	// determine if the deadline was exceeded
	start := podStatus.StartTime.Time
	duration := m.clock.Since(start)
	allowedDuration := time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second
	return duration >= allowedDuration
}
