/*
Copyright 2024 The Kubernetes Authors.

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
	"context"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	crierror "k8s.io/cri-api/pkg/errors"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

// containerLifecycle manages the lifecycle of containers. It is responsible
// for starting and terminating containers in a thread-safe manner.
// containerLifecycle needs to serialize the start and termination operations
// for each container as starting a container is a blocking operation, while
// terminating a container is a non-blocking operation.
type containerLifecycle struct {
	// TODO: Remove dependency on kubeGenericRuntimeManager
	m *kubeGenericRuntimeManager

	lock         sync.Mutex
	containerMap map[kubetypes.UID]map[string]*containerWorker
}

func newContainerLifecycle(m *kubeGenericRuntimeManager) *containerLifecycle {
	return &containerLifecycle{
		m:            m,
		containerMap: make(map[kubetypes.UID]map[string]*containerWorker),
	}
}

// startContainer starts a container in a blocking manner with a container
// lock. It returns an error if the container is terminating or it failed to
// start the container.
func (cl *containerLifecycle) startContainer(ctx context.Context, podSandboxID string, podSandboxConfig *runtimeapi.PodSandboxConfig, spec *startSpec, pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, podIP string, podIPs []string, imageVolumes kubecontainer.ImageVolumes) (string, error) {
	c := cl.ensureContainer(pod.UID, spec.container.Name)

	return c.startContainer(ctx, podSandboxID, podSandboxConfig, spec, pod, podStatus, pullSecrets, podIP, podIPs, imageVolumes)
}

// startContainerDuringPodTermination starts a container in a blocking manner
// with a container lock even if the container is termination requested. It
// returns an error if the container has stopped or it failed to start the
// container.
func (cl *containerLifecycle) startContainerDuringPodTermination(ctx context.Context, podSandboxID string, podSandboxConfig *runtimeapi.PodSandboxConfig, spec *startSpec, pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, podIP string, podIPs []string, imageVolumes kubecontainer.ImageVolumes) (string, error) {
	c := cl.ensureContainer(pod.UID, spec.container.Name)

	return c.startContainerDuringPodTermination(ctx, podSandboxID, podSandboxConfig, spec, pod, podStatus, pullSecrets, podIP, podIPs, imageVolumes)
}

// requestTermination requests the termination of a container so that it can
// terminate the container in a non-blocking manner.
func (cl *containerLifecycle) requestTermination(pod *v1.Pod, containerID kubecontainer.ContainerID, containerName, message string, reason kubecontainer.ContainerKillReason, gracePeriodOverride *int64, ordering *terminationOrdering) {
	c := cl.ensureContainer(pod.UID, containerName)

	c.requestTermination(pod, containerID, containerName, message, reason, gracePeriodOverride, ordering)
}

func (cl *containerLifecycle) isPodStopped(podUID kubetypes.UID) bool {
	cl.lock.Lock()
	defer cl.lock.Unlock()

	containers, exists := cl.containerMap[podUID]
	if !exists {
		return true
	}

	for _, worker := range containers {
		if !worker.isTerminated() {
			return false
		}
	}

	return true
}

func (cl *containerLifecycle) removePodIfStopped(podUID kubetypes.UID) bool {
	cl.lock.Lock()
	defer cl.lock.Unlock()

	containers, exists := cl.containerMap[podUID]
	if !exists {
		return true
	}

	for _, worker := range containers {
		if !worker.isTerminated() {
			return false
		}
	}

	delete(cl.containerMap, podUID)
	return true
}

// ensureContainer ensures the container worker for the given podUID and
// containerName.
// Note that this method is for internal use only.
func (cl *containerLifecycle) ensureContainer(podUID kubetypes.UID, containerName string) *containerWorker {
	cl.lock.Lock()
	defer cl.lock.Unlock()

	containers, exists := cl.containerMap[podUID]
	if !exists {
		cl.containerMap[podUID] = make(map[string]*containerWorker)
		containers = cl.containerMap[podUID]
	}

	c, exists := containers[containerName]
	if !exists {
		containers[containerName] = &containerWorker{
			m: cl.m,
		}
		c = containers[containerName]
	}

	return c
}

type containerPhase int

const (
	containerPhaseRunning containerPhase = iota
	// containerPhaseTerminationRequested is a state that indicates the container
	// is requested to terminate.
	containerPhaseTerminationRequested
	// containerPhaseStopping is a state that indicates the container is trying
	// to terminate gracefully.
	// The container worker must not restart the container from this state.
	containerPhaseStopping
	// containerPhaseTerminated is a state that indicates the container has
	// terminated. The container worker should be removed from the container
	// map before restarting the container.
	containerPhaseTerminated
)

// containerWorker serializes the start and termination operations for a
// container.
type containerWorker struct {
	// TODO: Remove dependency on kubeGenericRuntimeManager
	m *kubeGenericRuntimeManager

	// containerCh is a channel to pass the container termination request to the
	// container worker.
	containerCh chan containerTermination

	// lock locks the container during the start operation and protects the
	// fields below.
	lock sync.Mutex

	// phase of the container
	phase containerPhase
	// restartCh is a channel to notify the container worker to restart the
	// terminating container.
	restartCh chan struct{}
	// should restart the container even if it is your turn to exit.
	shouldRestart bool
}

type containerTermination struct {
	containerID kubecontainer.ContainerID
	restartCh   chan struct{}
}

func (c *containerWorker) startContainer(ctx context.Context, podSandboxID string, podSandboxConfig *runtimeapi.PodSandboxConfig, spec *startSpec, pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, podIP string, podIPs []string, imageVolumes kubecontainer.ImageVolumes) (string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.phase >= containerPhaseTerminationRequested && c.phase < containerPhaseTerminated {
		klog.V(3).InfoS("Container is terminating, should not start the container", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", spec.container.Name)
		return "", fmt.Errorf("container %q is terminating", spec.container.Name)
	}

	c.phase = containerPhaseRunning

	_, msg, err := c.m.startContainer(ctx, podSandboxID, podSandboxConfig, spec, pod, podStatus, pullSecrets, podIP, podIPs, imageVolumes)
	return msg, err
}

func (c *containerWorker) startContainerDuringPodTermination(ctx context.Context, podSandboxID string, podSandboxConfig *runtimeapi.PodSandboxConfig, spec *startSpec, pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, podIP string, podIPs []string, imageVolumes kubecontainer.ImageVolumes) (string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.phase >= containerPhaseStopping {
		klog.V(3).InfoS("Container has stopped, should not restart the container", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", spec.container.Name)
		return "", fmt.Errorf("container %q already has stopped", spec.container.Name)
	}

	containerID, msg, err := c.m.startContainer(ctx, podSandboxID, podSandboxConfig, spec, pod, podStatus, pullSecrets, podIP, podIPs, imageVolumes)
	if err != nil {
		return msg, err
	}

	if c.phase == containerPhaseTerminationRequested && c.restartCh != nil {
		close(c.restartCh)
		restartCh := make(chan struct{})
		c.containerCh <- containerTermination{
			containerID: containerID,
			restartCh:   restartCh,
		}
		c.restartCh = restartCh
		c.shouldRestart = true
	}

	return msg, nil
}

func (c *containerWorker) requestTermination(pod *v1.Pod, containerID kubecontainer.ContainerID, containerName, message string, reason kubecontainer.ContainerKillReason, gracePeriodOverride *int64, ordering *terminationOrdering) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.phase >= containerPhaseTerminationRequested {
		klog.V(3).InfoS("Container termination worker already scheduled", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerName)
		return
	}
	c.phase = containerPhaseTerminationRequested

	// Note that exactly one containerTermination can be buffered in the
	// channel to avoid deadlock.
	c.containerCh = make(chan containerTermination, 1)
	go func(ctx context.Context) {
		defer utilruntime.HandleCrash()
		klog.V(3).InfoS("Container termination worker has started", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerName)
		defer klog.V(3).InfoS("Container termination worker has stopped", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerName)
		defer close(c.containerCh)

		c.terminationWorkerLoop(ctx, c.containerCh, pod, containerName, message, reason, gracePeriodOverride, ordering)
	}(context.TODO())

	restartCh := make(chan struct{})
	c.containerCh <- containerTermination{
		containerID: containerID,
		restartCh:   restartCh,
	}
	c.restartCh = restartCh
}

func (c *containerWorker) terminationWorkerLoop(ctx context.Context, ch <-chan containerTermination, pod *v1.Pod, containerName, message string, reason kubecontainer.ContainerKillReason, gracePeriodOverride *int64, ordering *terminationOrdering) {
	defer func() {
		c.lock.Lock()
		defer c.lock.Unlock()
		c.phase = containerPhaseTerminated
	}()

	start := time.Now()

	for container := range c.containerCh {
		// prepare for termination
		containerID := container.containerID
		gracePeriod, err := c.prepareForTermination(ctx, pod, containerID, containerName, message, reason, gracePeriodOverride)
		if err != nil {
			klog.ErrorS(err, "Failed to prepare to kill container", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerName, "containerID", containerID)
			return
		}
		remainingGracePeriod := gracePeriod - int64(time.Since(start).Seconds())

		if ordering != nil {
			select {
			case <-container.restartCh:
				c.lock.Lock()
				c.shouldRestart = false
				c.lock.Unlock()
				klog.V(3).InfoS("Container is restarting during the termination", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerName, "containerID", containerID)
				continue
			case <-ordering.waitForTurnCh(containerName, remainingGracePeriod):
				c.lock.Lock()
				// This is needed because the container may have been restarted
				// before acquiring the lock.
				if c.shouldRestart {
					c.shouldRestart = false
					c.lock.Unlock()
					continue
				}
				c.phase = containerPhaseStopping
				c.lock.Unlock()
			}
		}
		// We cannot restart the container from here.
		err = c.stopContainer(ctx, pod, containerID, containerName, remainingGracePeriod, ordering)
		if err != nil {
			klog.ErrorS(err, "Failed to kill container", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerName, "containerID", containerID)
			return
		}
		return
	}
}

// prepareForTermination prepares for the termination of a container and returns
// the grace period to terminate the container.
func (c *containerWorker) prepareForTermination(ctx context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, containerName string, message string, reason kubecontainer.ContainerKillReason, gracePeriodOverride *int64) (int64, error) {
	var containerSpec *v1.Container
	if pod != nil {
		if containerSpec = kubecontainer.GetContainerSpec(pod, containerName); containerSpec == nil {
			return -1, fmt.Errorf("failed to get containerSpec %q (id=%q) in pod %q when killing container for reason %q",
				containerName, containerID.String(), format.Pod(pod), message)
		}
	} else {
		// Restore necessary information if one of the specs is nil.
		restoredPod, restoredContainer, err := c.m.restoreSpecsFromContainerLabels(ctx, containerID)
		if err != nil {
			return -1, err
		}
		pod, containerSpec = restoredPod, restoredContainer
	}

	// From this point, pod and container must be non-nil.
	gracePeriod := setTerminationGracePeriod(pod, containerSpec, containerName, containerID, reason)

	if len(message) == 0 {
		message = fmt.Sprintf("Stopping container %s", containerSpec.Name)
	}
	c.m.recordContainerEvent(pod, containerSpec, containerID.ID, v1.EventTypeNormal, events.KillingContainer, message)

	if gracePeriodOverride != nil {
		gracePeriod = *gracePeriodOverride
		klog.V(3).InfoS("Terminating container with a grace period override", "pod", klog.KObj(pod), "podUID", pod.UID,
			"containerName", containerName, "containerID", containerID.String(), "gracePeriod", gracePeriod)
	}

	// Run the pre-stop lifecycle hooks if applicable and if there is enough time to run it
	if containerSpec.Lifecycle != nil && containerSpec.Lifecycle.PreStop != nil && gracePeriod > 0 {
		c.m.executePreStopHook(ctx, pod, containerID, containerSpec, gracePeriod)
	}

	return gracePeriod, nil
}

func (c *containerWorker) stopContainer(ctx context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, containerName string, gracePeriod int64, ordering *terminationOrdering) error {
	// always give containers a minimal shutdown window to avoid unnecessary SIGKILLs
	if gracePeriod < minimumGracePeriodInSeconds {
		gracePeriod = minimumGracePeriodInSeconds
	}

	klog.V(2).InfoS("Stopping container with a grace period", "pod", klog.KObj(pod), "podUID", pod.UID,
		"containerName", containerName, "containerID", containerID.String(), "gracePeriod", gracePeriod)

	err := c.m.runtimeService.StopContainer(ctx, containerID.ID, gracePeriod)
	if err != nil && !crierror.IsNotFound(err) {
		klog.ErrorS(err, "Container termination failed with gracePeriod", "pod", klog.KObj(pod), "podUID", pod.UID,
			"containerName", containerName, "containerID", containerID.String(), "gracePeriod", gracePeriod)
		return err
	}
	klog.V(3).InfoS("Container exited normally", "pod", klog.KObj(pod), "podUID", pod.UID,
		"containerName", containerName, "containerID", containerID.String())

	if ordering != nil {
		ordering.containerTerminated(containerName)
	}

	return nil
}

func (c *containerWorker) isTerminated() bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.phase == containerPhaseTerminated
}
