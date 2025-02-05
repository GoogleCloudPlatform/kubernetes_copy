/*
Copyright 2023 The Kubernetes Authors.

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
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"

	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

// terminationOrdering is used to enforce a termination ordering for sidecar containers.  It sets up
// dependencies between sidecars and allows the pod termination process to wait until the grace period
// expires, or all dependent containers have finished terminating.
type terminationOrdering struct {
	// terminated is a map from container name to a channel, that if closed
	// indicates that the container with that name was terminated
	terminated map[string]chan struct{}
	// prereqs is a map from container name to a list of channel that the container
	// must wait on to ensure termination ordering
	prereqs map[string][]chan struct{}

	lock sync.Mutex
}

// newTerminationOrdering constructs a terminationOrdering based on the pod spec and the currently running containers.
func newTerminationOrdering(pod *v1.Pod, runningContainerNames []string) *terminationOrdering {
	to := &terminationOrdering{
		prereqs:    map[string][]chan struct{}{},
		terminated: map[string]chan struct{}{},
	}

	runningContainers := map[string]struct{}{}
	for _, name := range runningContainerNames {
		runningContainers[name] = struct{}{}
	}

	var mainContainerChannels []chan struct{}
	// sidecar containers need to wait on main containers, so we create a channel per main container
	// for them to wait on
	for _, c := range pod.Spec.Containers {
		channel := make(chan struct{})
		to.terminated[c.Name] = channel
		mainContainerChannels = append(mainContainerChannels, channel)

		// if it's not a running container, pre-close the channel so nothing
		// waits on it
		if _, isRunning := runningContainers[c.Name]; !isRunning {
			close(channel)
		}
	}

	var previousSidecarName string
	for i := range pod.Spec.InitContainers {
		// get the init containers in reverse order
		ic := pod.Spec.InitContainers[len(pod.Spec.InitContainers)-i-1]

		channel := make(chan struct{})
		to.terminated[ic.Name] = channel

		// if it's not a running container, pre-close the channel so nothing
		// waits on it
		if _, isRunning := runningContainers[ic.Name]; !isRunning {
			close(channel)
		}

		if podutil.IsRestartableInitContainer(&ic) {
			// sidecars need to wait for all main containers to exit
			to.prereqs[ic.Name] = append(to.prereqs[ic.Name], mainContainerChannels...)

			// if there is a later sidecar, this container needs to wait for it to finish
			if previousSidecarName != "" {
				to.prereqs[ic.Name] = append(to.prereqs[ic.Name], to.terminated[previousSidecarName])
			}
			previousSidecarName = ic.Name
		}
	}
	return to
}

// waitForTurn waits until it is time for the container with the specified name to begin terminating, up until
// the specified grace period.  If gracePeriod = 0, there is no wait.
func (o *terminationOrdering) waitForTurn(name string, gracePeriod int64) float64 {
	// if there is no grace period, we don't wait
	if gracePeriod <= 0 {
		return 0
	}

	start := time.Now()
	remainingGrace := time.NewTimer(time.Duration(gracePeriod) * time.Second)

	for _, c := range o.prereqs[name] {
		select {
		case <-c:
		case <-remainingGrace.C:
			// grace period expired, so immediately exit
			return time.Since(start).Seconds()
		}
	}

	return time.Since(start).Seconds()
}

func (o *terminationOrdering) waitForTurnCh(name string, gracePeriod int64) <-chan struct{} {
	ch := make(chan struct{})
	remainingGrace := time.NewTimer(time.Duration(gracePeriod) * time.Second)

	go func() {
		defer close(ch)
		for _, c := range o.prereqs[name] {
			select {
			case <-c:
			case <-remainingGrace.C:
				// grace period expired, so immediately exit
				ch <- struct{}{}
				return
			}
		}
		ch <- struct{}{}
	}()

	return ch
}

// containerTerminated should be called once the container with the specified name has exited.
func (o *terminationOrdering) containerTerminated(name string) {
	o.lock.Lock()
	defer o.lock.Unlock()
	if ch, ok := o.terminated[name]; ok {
		close(ch)
		delete(o.terminated, name)
	}
}
