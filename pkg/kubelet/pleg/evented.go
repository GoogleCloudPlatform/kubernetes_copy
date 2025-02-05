/*
Copyright 2022 The Kubernetes Authors.

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

package pleg

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/utils/clock"
)

// The frequency with which global timestamp of the cache is to
// is to be updated periodically. If pod workers get stuck at cache.GetNewerThan
// call, after this period it will be unblocked.
const globalCacheUpdatePeriod = 5 * time.Second

var (
	eventedPLEGUsage   = false
	eventedPLEGUsageMu = sync.RWMutex{}
)

// IsEventedPLEGInUse indicates whether Evented PLEG is in use. Even after enabling
// the Evented PLEG feature gate, there could be several reasons it may not be in use.
// e.g. Streaming data issues from the runtime or the runtime does not implement the
// container events stream.
func IsEventedPLEGInUse() bool {
	eventedPLEGUsageMu.RLock()
	defer eventedPLEGUsageMu.RUnlock()
	return eventedPLEGUsage
}

// setEventedPLEGUsage should only be accessed from
// Start/Stop of Evented PLEG.
func setEventedPLEGUsage(enable bool) {
	eventedPLEGUsageMu.Lock()
	defer eventedPLEGUsageMu.Unlock()
	eventedPLEGUsage = enable
}

type EventedPLEG struct {
	// The container runtime.
	runtime kubecontainer.Runtime
	// The runtime service.
	runtimeService internalapi.RuntimeService
	// The channel from which the subscriber listens events.
	eventChannel chan *PodLifecycleEvent
	// Cache for storing the runtime states required for syncing pods.
	cache kubecontainer.Cache
	// For testability.
	clock clock.Clock
	// GenericPLEG is used to force relist when required.
	genericPleg podLifecycleEventGeneratorHandler
	// The maximum number of retries when getting container events from the runtime.
	eventedPlegMaxStreamRetries int
	// Indicates relisting related parameters
	relistDuration *RelistDuration
	// Stop the Evented PLEG by closing the channel.
	stopCh chan struct{}
	// Stops the periodic update of the cache global timestamp.
	stopCacheUpdateCh chan struct{}
	// Locks the start/stop operation of the Evented PLEG.
	runningMu sync.Mutex
	// logger is used for contextual logging
	logger klog.Logger
}

// NewEventedPLEG instantiates a new EventedPLEG object and return it.
func NewEventedPLEG(logger klog.Logger, runtime kubecontainer.Runtime, runtimeService internalapi.RuntimeService, eventChannel chan *PodLifecycleEvent,
	cache kubecontainer.Cache, genericPleg PodLifecycleEventGenerator, eventedPlegMaxStreamRetries int,
	relistDuration *RelistDuration, clock clock.Clock) (PodLifecycleEventGenerator, error) {
	handler, ok := genericPleg.(podLifecycleEventGeneratorHandler)
	if !ok {
		return nil, fmt.Errorf("%v doesn't implement podLifecycleEventGeneratorHandler interface", genericPleg)
	}
	return &EventedPLEG{
		runtime:                     runtime,
		runtimeService:              runtimeService,
		eventChannel:                eventChannel,
		cache:                       cache,
		genericPleg:                 handler,
		eventedPlegMaxStreamRetries: eventedPlegMaxStreamRetries,
		relistDuration:              relistDuration,
		clock:                       clock,
		logger:                      logger,
	}, nil
}

// Watch returns a channel from which the subscriber can receive PodLifecycleEvent events.
func (e *EventedPLEG) Watch() chan *PodLifecycleEvent {
	return e.eventChannel
}

// Relist relists all containers using GenericPLEG
func (e *EventedPLEG) Relist() {
	e.genericPleg.Relist()
}

// Start starts the Evented PLEG
func (e *EventedPLEG) Start() {
	e.runningMu.Lock()
	defer e.runningMu.Unlock()
	if IsEventedPLEGInUse() {
		return
	}
	setEventedPLEGUsage(true)
	e.stopCh = make(chan struct{})
	e.stopCacheUpdateCh = make(chan struct{})
	go wait.Until(e.watchEventsChannel, 0, e.stopCh)
	go wait.Until(e.updateGlobalCache, globalCacheUpdatePeriod, e.stopCacheUpdateCh)
}

// Stop stops the Evented PLEG
func (e *EventedPLEG) Stop() {
	e.runningMu.Lock()
	defer e.runningMu.Unlock()
	if !IsEventedPLEGInUse() {
		return
	}
	setEventedPLEGUsage(false)
	close(e.stopCh)
	close(e.stopCacheUpdateCh)
}

// In case the Evented PLEG experiences undetectable issues in the underlying
// GRPC connection there is a remote chance the pod might get stuck in a
// given state while it has progressed in its life cycle. This function will be
// called periodically to update the global timestamp of the cache so that those
// pods stuck at GetNewerThan in pod workers will get unstuck.
func (e *EventedPLEG) updateGlobalCache() {
	e.cache.UpdateTime(time.Now())
}

// Update the relisting period and threshold
func (e *EventedPLEG) Update(relistDuration *RelistDuration) {
	e.genericPleg.Update(relistDuration)
}

// Healthy check if PLEG work properly.
func (e *EventedPLEG) Healthy() (bool, error) {
	// GenericPLEG is declared unhealthy when relisting time is more
	// than the relistThreshold. In case EventedPLEG is turned on,
	// relistingPeriod and relistingThreshold are adjusted to higher
	// values. So the health check of Generic PLEG should check
	// the adjusted values of relistingPeriod and relistingThreshold.

	// EventedPLEG is declared unhealthy only if eventChannel is out of capacity.
	if len(e.eventChannel) == cap(e.eventChannel) {
		return false, fmt.Errorf("EventedPLEG: pleg event channel capacity is full with %v events", len(e.eventChannel))
	}

	timestamp := e.clock.Now()
	metrics.PLEGLastSeen.Set(float64(timestamp.Unix()))
	return true, nil
}

func (e *EventedPLEG) watchEventsChannel() {
	containerEventsResponseCh := make(chan *runtimeapi.ContainerEventResponse, cap(e.eventChannel))
	defer close(containerEventsResponseCh)

	// Get the container events from the runtime.
	go func() {
		numAttempts := 0
		for {
			if numAttempts >= e.eventedPlegMaxStreamRetries {
				if IsEventedPLEGInUse() {
					// Fall back to Generic PLEG relisting since Evented PLEG is not working.
					e.logger.V(4).Info("Fall back to Generic PLEG relisting since Evented PLEG is not working")
					e.Stop()
					e.genericPleg.Stop()       // Stop the existing Generic PLEG which runs with longer relisting period when Evented PLEG is in use.
					e.Update(e.relistDuration) // Update the relisting period to the default value for the Generic PLEG.
					e.genericPleg.Start()
					break
				}
			}

			err := e.runtimeService.GetContainerEvents(context.Background(), containerEventsResponseCh, func(runtimeapi.RuntimeService_GetContainerEventsClient) {
				metrics.EventedPLEGConn.Inc()
			})
			if err != nil {
				metrics.EventedPLEGConnErr.Inc()
				numAttempts++
				e.Relist() // Force a relist to get the latest container and pods running metric.
				e.logger.V(4).Info("Evented PLEG: Failed to get container events, retrying: ", "err", err)
			}
		}
	}()

	if IsEventedPLEGInUse() {
		e.processCRIEvents(containerEventsResponseCh)
	}
}

func (e *EventedPLEG) processCRIEvents(containerEventsResponseCh chan *runtimeapi.ContainerEventResponse) {
	for event := range containerEventsResponseCh {
		// Ignore the event if PodSandboxStatus is nil.
		// This might happen under some race condition where the podSandbox has
		// been deleted, and therefore container runtime couldn't find the
		// podSandbox for the container when generating the event.
		// It is safe to ignore because
		// a) a event would have been received for the sandbox deletion,
		// b) in worst case, a relist will eventually sync the pod status.
		// TODO(#114371): Figure out a way to handle this case instead of ignoring.
		if event.PodSandboxStatus == nil || event.PodSandboxStatus.Metadata == nil {
			e.logger.Error(nil, "Evented PLEG: received ContainerEventResponse with nil PodSandboxStatus or PodSandboxStatus.Metadata", "containerEventResponse", event)
			continue
		}

		podID := types.UID(event.PodSandboxStatus.Metadata.Uid)
		shouldSendPLEGEvent := false

		status, err := e.runtime.GeneratePodStatus(event)
		if err != nil {
			// nolint:logcheck // Not using the result of klog.V inside the
			// if branch is okay, we just use it to determine whether the
			// additional "podStatus" key and its value should be added.
			if klog.V(6).Enabled() {
				e.logger.Error(err, "Evented PLEG: error generating pod status from the received event", "podUID", podID, "podStatus", status)
			} else {
				e.logger.Error(err, "Evented PLEG: error generating pod status from the received event", "podUID", podID)
			}
		} else {
			if klogV := e.logger.V(6); klogV.Enabled() {
				e.logger.Info("Evented PLEG: Generated pod status from the received event", "podUID", podID, "podStatus", status)
			} else {
				e.logger.V(4).Info("Evented PLEG: Generated pod status from the received event", "podUID", podID)
			}
			// Preserve the pod IP across cache updates if the new IP is empty.
			// When a pod is torn down, kubelet may race with PLEG and retrieve
			// a pod status after network teardown, but the kubernetes API expects
			// the completed pod's IP to be available after the pod is dead.
			status.IPs = e.getPodIPs(podID, status)
		}

		e.updateRunningPodMetric(status)
		e.updateRunningContainerMetric(status)
		e.updateLatencyMetric(event)

		if event.ContainerEventType == runtimeapi.ContainerEventType_CONTAINER_DELETED_EVENT {
			for _, sandbox := range status.SandboxStatuses {
				if sandbox.Id == event.ContainerId {
					// When the CONTAINER_DELETED_EVENT is received by the kubelet,
					// the runtime has indicated that the container has been removed
					// by the runtime and hence, it must be removed from the cache
					// of kubelet too.
					e.cache.Delete(podID)
				}
			}
			shouldSendPLEGEvent = true
		} else {
			// For sandbox events, we only need to update the pod cache without sending events to the pod worker.
			// The pod worker does not need to handle sandbox events,
			// sending sandbox events would cause unnecessary race conditions.
			if e.cache.Set(podID, status, err, time.Unix(0, event.GetCreatedAt())) && !(event.PodSandboxStatus != nil && event.ContainersStatuses == nil) {
				shouldSendPLEGEvent = true
			}
		}

		if shouldSendPLEGEvent {
			e.processCRIEvent(event)
		}
	}
}

func (e *EventedPLEG) processCRIEvent(event *runtimeapi.ContainerEventResponse) {
	switch event.ContainerEventType {
	case runtimeapi.ContainerEventType_CONTAINER_STOPPED_EVENT:
		e.sendPodLifecycleEvent(&PodLifecycleEvent{ID: types.UID(event.PodSandboxStatus.Metadata.Uid), Type: ContainerDied, Data: event.ContainerId})
		e.logger.V(4).Info("Received Container Stopped Event", "event", event.String())
	case runtimeapi.ContainerEventType_CONTAINER_CREATED_EVENT:
		// We only need to update the pod status on container create.
		// But we don't have to generate any PodLifeCycleEvent. Container creation related
		// PodLifeCycleEvent is ignored by the existing Generic PLEG as well.
		// https://github.com/kubernetes/kubernetes/blob/24753aa8a4df8d10bfd6330e0f29186000c018be/pkg/kubelet/pleg/generic.go#L88 and
		// https://github.com/kubernetes/kubernetes/blob/24753aa8a4df8d10bfd6330e0f29186000c018be/pkg/kubelet/pleg/generic.go#L273
		e.logger.V(4).Info("Received Container Created Event", "event", event.String())
	case runtimeapi.ContainerEventType_CONTAINER_STARTED_EVENT:
		e.sendPodLifecycleEvent(&PodLifecycleEvent{ID: types.UID(event.PodSandboxStatus.Metadata.Uid), Type: ContainerStarted, Data: event.ContainerId})
		e.logger.V(4).Info("Received Container Started Event", "event", event.String())
	case runtimeapi.ContainerEventType_CONTAINER_DELETED_EVENT:
		// In case the pod is deleted it is safe to generate both ContainerDied and ContainerRemoved events, just like in the case of
		// Generic PLEG. https://github.com/kubernetes/kubernetes/blob/24753aa8a4df8d10bfd6330e0f29186000c018be/pkg/kubelet/pleg/generic.go#L169
		e.sendPodLifecycleEvent(&PodLifecycleEvent{ID: types.UID(event.PodSandboxStatus.Metadata.Uid), Type: ContainerDied, Data: event.ContainerId})
		e.sendPodLifecycleEvent(&PodLifecycleEvent{ID: types.UID(event.PodSandboxStatus.Metadata.Uid), Type: ContainerRemoved, Data: event.ContainerId})
		e.logger.V(4).Info("Received Container Deleted Event", "event", event)
	}
}

func (e *EventedPLEG) getPodIPs(pid types.UID, status *kubecontainer.PodStatus) []string {
	if len(status.IPs) != 0 {
		return status.IPs
	}

	oldStatus, err := e.cache.Get(pid)
	if err != nil || len(oldStatus.IPs) == 0 {
		return nil
	}

	for _, sandboxStatus := range status.SandboxStatuses {
		// If at least one sandbox is ready, then use this status update's pod IP
		if sandboxStatus.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			return status.IPs
		}
	}

	// For pods with no ready containers or sandboxes (like exited pods)
	// use the old status' pod IP
	return oldStatus.IPs
}

func (e *EventedPLEG) sendPodLifecycleEvent(event *PodLifecycleEvent) {
	select {
	case e.eventChannel <- event:
	default:
		// record how many events were discarded due to channel out of capacity
		metrics.PLEGDiscardEvents.Inc()
		e.logger.Error(nil, "Evented PLEG: Event channel is full, discarded pod lifecycle event")
	}
}

func getPodSandboxState(podStatus *kubecontainer.PodStatus) kubecontainer.State {
	// increase running pod count when cache doesn't contain podID
	var sandboxId string
	for _, sandbox := range podStatus.SandboxStatuses {
		sandboxId = sandbox.Id
		// pod must contain only one sandbox
		break
	}

	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.ID.ID == sandboxId {
			if containerStatus.State == kubecontainer.ContainerStateRunning {
				return containerStatus.State
			}
		}
	}
	return kubecontainer.ContainerStateExited
}

func (e *EventedPLEG) updateRunningPodMetric(podStatus *kubecontainer.PodStatus) {
	cachedPodStatus, err := e.cache.Get(podStatus.ID)
	if err != nil {
		e.logger.Error(err, "Evented PLEG: Get cache", "podID", podStatus.ID)
	}
	// cache miss condition: The pod status object will have empty state if missed in cache
	if len(cachedPodStatus.SandboxStatuses) < 1 {
		sandboxState := getPodSandboxState(podStatus)
		if sandboxState == kubecontainer.ContainerStateRunning {
			metrics.RunningPodCount.Inc()
		}
	} else {
		oldSandboxState := getPodSandboxState(cachedPodStatus)
		currentSandboxState := getPodSandboxState(podStatus)

		if oldSandboxState == kubecontainer.ContainerStateRunning && currentSandboxState != kubecontainer.ContainerStateRunning {
			metrics.RunningPodCount.Dec()
		} else if oldSandboxState != kubecontainer.ContainerStateRunning && currentSandboxState == kubecontainer.ContainerStateRunning {
			metrics.RunningPodCount.Inc()
		}
	}
}

func getContainerStateCount(podStatus *kubecontainer.PodStatus) map[kubecontainer.State]int {
	containerStateCount := make(map[kubecontainer.State]int)
	for _, container := range podStatus.ContainerStatuses {
		containerStateCount[container.State]++
	}
	return containerStateCount
}

func (e *EventedPLEG) updateRunningContainerMetric(podStatus *kubecontainer.PodStatus) {
	cachedPodStatus, err := e.cache.Get(podStatus.ID)
	if err != nil {
		e.logger.Error(err, "Evented PLEG: Get cache", "podID", podStatus.ID)
	}

	// cache miss condition: The pod status object will have empty state if missed in cache
	if len(cachedPodStatus.SandboxStatuses) < 1 {
		containerStateCount := getContainerStateCount(podStatus)
		for state, count := range containerStateCount {
			// add currently obtained count
			metrics.RunningContainerCount.WithLabelValues(string(state)).Add(float64(count))
		}
	} else {
		oldContainerStateCount := getContainerStateCount(cachedPodStatus)
		currentContainerStateCount := getContainerStateCount(podStatus)

		// old and new set of container states may vary;
		// get a unique set of container states combining both
		containerStates := make(map[kubecontainer.State]bool)
		for state := range oldContainerStateCount {
			containerStates[state] = true
		}
		for state := range currentContainerStateCount {
			containerStates[state] = true
		}

		// update the metric via difference of old and current counts
		for state := range containerStates {
			diff := currentContainerStateCount[state] - oldContainerStateCount[state]
			metrics.RunningContainerCount.WithLabelValues(string(state)).Add(float64(diff))
		}
	}
}

func (e *EventedPLEG) updateLatencyMetric(event *runtimeapi.ContainerEventResponse) {
	duration := time.Duration(time.Now().UnixNano()-event.CreatedAt) * time.Nanosecond
	metrics.EventedPLEGConnLatency.Observe(duration.Seconds())
}

func (e *EventedPLEG) SetPodWatchCondition(podUID types.UID, conditionKey string, condition WatchCondition) {
	e.genericPleg.SetPodWatchCondition(podUID, conditionKey, condition)
}
