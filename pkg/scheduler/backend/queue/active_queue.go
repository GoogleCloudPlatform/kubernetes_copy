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

package queue

import (
	"container/list"
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/backend/heap"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
)

// activeQueuer is a wrapper for activeQ related operations.
// Its methods, except "unlocked" ones, take the lock inside.
// Note: be careful when using unlocked() methods.
// getLock() methods should be used only for unlocked() methods
// and it is forbidden to call any other activeQueuer's method under this lock.
type activeQueuer interface {
	underLock(func(unlockedActiveQ unlockedActiveQueuer))
	underRLock(func(unlockedActiveQ unlockedActiveQueueReader))

	update(newPod *v1.Pod, oldPodInfo *framework.QueuedPodInfo) *framework.QueuedPodInfo
	delete(pInfo *framework.QueuedPodInfo) error
	pop(logger klog.Logger) (*framework.QueuedPodInfo, error)
	list() []*v1.Pod
	len() int
	has(pInfo *framework.QueuedPodInfo) bool

	listInFlightEvents() []interface{}
	listInFlightPods() []*v1.Pod
	clusterEventsForPod(logger klog.Logger, pInfo *framework.QueuedPodInfo) ([]*clusterEvent, error)
	addEventsIfPodInFlight(oldPod, newPod *v1.Pod, events []framework.ClusterEvent) bool
	addEventIfAnyInFlight(oldObj, newObj interface{}, event framework.ClusterEvent) bool

	schedulingCycle() int64
	done(pod types.UID)
	close()
	broadcast()
}

// unlockedActiveQueuer defines activeQ methods that are not protected by the lock itself.
// underLock() method should be used to protect these methods.
type unlockedActiveQueuer interface {
	unlockedActiveQueueReader
	AddOrUpdate(pInfo *framework.QueuedPodInfo)
}

// unlockedActiveQueueReader defines activeQ read-only methods that are not protected by the lock itself.
// underLock() or underRLock() method should be used to protect these methods.
type unlockedActiveQueueReader interface {
	Get(pInfo *framework.QueuedPodInfo) (*framework.QueuedPodInfo, bool)
	Has(pInfo *framework.QueuedPodInfo) bool
}

// activeQueue implements activeQueuer. All of the fields have to be protected using the lock.
type activeQueue struct {
	// lock synchronizes all operations related to activeQ.
	// It protects activeQ, inFlightPods, inFlightEvents, schedulingCycle and closed fields.
	// Caution: DO NOT take "SchedulingQueue.lock" after taking "lock".
	// You should always take "SchedulingQueue.lock" first, otherwise the queue could end up in deadlock.
	// "lock" should not be taken after taking "nLock".
	// Correct locking order is: SchedulingQueue.lock > lock > nominator.nLock.
	lock sync.RWMutex

	// activeQ is heap structure that scheduler actively looks at to find pods to
	// schedule. Head of heap is the highest priority pod.
	queue *heap.Heap[*framework.QueuedPodInfo]

	// cond is a condition that is notified when the pod is added to activeQ.
	// It is used with lock.
	cond sync.Cond

	// inFlightPods holds the UID of all pods which have been popped out for which Done
	// hasn't been called yet - in other words, all pods that are currently being
	// processed (being scheduled, in permit, or in the binding cycle).
	//
	// The values in the map are the entry of each pod in the inFlightEvents list.
	// The value of that entry is the *v1.Pod at the time that scheduling of that
	// pod started, which can be useful for logging or debugging.
	inFlightPods map[types.UID]*list.Element

	// inFlightEvents holds the events received by the scheduling queue
	// (entry value is clusterEvent) together with in-flight pods (entry
	// value is *v1.Pod). Entries get added at the end while the mutex is
	// locked, so they get serialized.
	//
	// The pod entries are added in Pop and used to track which events
	// occurred after the pod scheduling attempt for that pod started.
	// They get removed when the scheduling attempt is done, at which
	// point all events that occurred in the meantime are processed.
	//
	// After removal of a pod, events at the start of the list are no
	// longer needed because all of the other in-flight pods started
	// later. Those events can be removed.
	inFlightEvents *list.List

	// schedCycle represents sequence number of scheduling cycle and is incremented
	// when a pod is popped.
	schedCycle int64

	// closed indicates that the queue is closed.
	// It is mainly used to let Pop() exit its control loop while waiting for an item.
	closed bool

	// isSchedulingQueueHintEnabled indicates whether the feature gate for the scheduling queue is enabled.
	isSchedulingQueueHintEnabled bool

	metricsRecorder metrics.MetricAsyncRecorder
}

func newActiveQueue(queue *heap.Heap[*framework.QueuedPodInfo], isSchedulingQueueHintEnabled bool, metricRecorder metrics.MetricAsyncRecorder) *activeQueue {
	aq := &activeQueue{
		queue:                        queue,
		inFlightPods:                 make(map[types.UID]*list.Element),
		inFlightEvents:               list.New(),
		isSchedulingQueueHintEnabled: isSchedulingQueueHintEnabled,
		metricsRecorder:              metricRecorder,
	}
	aq.cond.L = &aq.lock

	return aq
}

// underLock runs the fn function under the lock.Lock.
// fn can run unlockedActiveQueuer methods but should NOT run any other activeQueue method,
// as it would end up in deadlock.
func (aq *activeQueue) underLock(fn func(unlockedActiveQ unlockedActiveQueuer)) {
	aq.lock.Lock()
	defer aq.lock.Unlock()
	fn(aq.queue)
}

// underLock runs the fn function under the lock.RLock.
// fn can run unlockedActiveQueueReader methods but should NOT run any other activeQueue method,
// as it would end up in deadlock.
func (aq *activeQueue) underRLock(fn func(unlockedActiveQ unlockedActiveQueueReader)) {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	fn(aq.queue)
}

// update updates the pod in activeQ if oldPodInfo is already in the queue.
// It returns new pod info if updated, nil otherwise.
func (aq *activeQueue) update(newPod *v1.Pod, oldPodInfo *framework.QueuedPodInfo) *framework.QueuedPodInfo {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	if pInfo, exists := aq.queue.Get(oldPodInfo); exists {
		_ = pInfo.Update(newPod)
		aq.queue.AddOrUpdate(pInfo)
		return pInfo
	}
	return nil
}

// delete deletes the pod info from activeQ.
func (aq *activeQueue) delete(pInfo *framework.QueuedPodInfo) error {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	return aq.queue.Delete(pInfo)
}

// pop removes the head of the queue and returns it.
// It blocks if the queue is empty and waits until a new item is added to the queue.
// It increments scheduling cycle when a pod is popped.
func (aq *activeQueue) pop(logger klog.Logger) (*framework.QueuedPodInfo, error) {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	return aq.unlockedPop(logger)
}

func (aq *activeQueue) unlockedPop(logger klog.Logger) (*framework.QueuedPodInfo, error) {
	for aq.queue.Len() == 0 {
		// When the queue is empty, invocation of Pop() is blocked until new item is enqueued.
		// When Close() is called, the p.closed is set and the condition is broadcast,
		// which causes this loop to continue and return from the Pop().
		if aq.closed {
			logger.V(2).Info("Scheduling queue is closed")
			return nil, nil
		}
		aq.cond.Wait()
	}
	pInfo, err := aq.queue.Pop()
	if err != nil {
		return nil, err
	}
	pInfo.Attempts++
	// In flight, no concurrent events yet.
	if aq.isSchedulingQueueHintEnabled {
		// If the pod is already in the map, we shouldn't overwrite the inFlightPods otherwise it'd lead to a memory leak.
		// https://github.com/kubernetes/kubernetes/pull/127016
		if _, ok := aq.inFlightPods[pInfo.Pod.UID]; ok {
			// Just report it as an error, but no need to stop the scheduler
			// because it likely doesn't cause any visible issues from the scheduling perspective.
			logger.Error(nil, "the same pod is tracked in multiple places in the scheduler, and just discard it", "pod", klog.KObj(pInfo.Pod))
			// Just ignore/discard this duplicated pod and try to pop the next one.
			return aq.unlockedPop(logger)
		}

		aq.metricsRecorder.ObserveInFlightEventsAsync(metrics.PodPoppedInFlightEvent, 1, false)
		aq.inFlightPods[pInfo.Pod.UID] = aq.inFlightEvents.PushBack(pInfo.Pod)
	}
	aq.schedCycle++

	// Update metrics and reset the set of unschedulable plugins for the next attempt.
	for plugin := range pInfo.UnschedulablePlugins.Union(pInfo.PendingPlugins) {
		metrics.UnschedulableReason(plugin, pInfo.Pod.Spec.SchedulerName).Dec()
	}
	pInfo.UnschedulablePlugins.Clear()
	pInfo.PendingPlugins.Clear()
	pInfo.GatingPlugin = ""
	pInfo.GatingPluginEvents = nil

	return pInfo, nil
}

// list returns all pods that are in the queue.
func (aq *activeQueue) list() []*v1.Pod {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	var result []*v1.Pod
	for _, pInfo := range aq.queue.List() {
		result = append(result, pInfo.Pod)
	}
	return result
}

// len returns length of the queue.
func (aq *activeQueue) len() int {
	return aq.queue.Len()
}

// has inform if pInfo exists in the queue.
func (aq *activeQueue) has(pInfo *framework.QueuedPodInfo) bool {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	return aq.queue.Has(pInfo)
}

// listInFlightEvents returns all inFlightEvents.
func (aq *activeQueue) listInFlightEvents() []interface{} {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	var values []interface{}
	for event := aq.inFlightEvents.Front(); event != nil; event = event.Next() {
		values = append(values, event.Value)
	}
	return values
}

// listInFlightPods returns all inFlightPods.
func (aq *activeQueue) listInFlightPods() []*v1.Pod {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	var pods []*v1.Pod
	for _, obj := range aq.inFlightPods {
		pods = append(pods, obj.Value.(*v1.Pod))
	}
	return pods
}

// clusterEventsForPod gets all cluster events that have happened during pod for pInfo is being scheduled.
func (aq *activeQueue) clusterEventsForPod(logger klog.Logger, pInfo *framework.QueuedPodInfo) ([]*clusterEvent, error) {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	logger.V(5).Info("Checking events for in-flight pod", "pod", klog.KObj(pInfo.Pod), "unschedulablePlugins", pInfo.UnschedulablePlugins, "inFlightEventsSize", aq.inFlightEvents.Len(), "inFlightPodsSize", len(aq.inFlightPods))

	// AddUnschedulableIfNotPresent is called with the Pod at the end of scheduling or binding.
	// So, given pInfo should have been Pop()ed before,
	// we can assume pInfo must be recorded in inFlightPods and thus inFlightEvents.
	inFlightPod, ok := aq.inFlightPods[pInfo.Pod.UID]
	if !ok {
		return nil, fmt.Errorf("in flight Pod isn't found in the scheduling queue. If you see this error log, it's likely a bug in the scheduler")
	}

	var events []*clusterEvent
	for event := inFlightPod.Next(); event != nil; event = event.Next() {
		e, ok := event.Value.(*clusterEvent)
		if !ok {
			// Must be another in-flight Pod (*v1.Pod). Can be ignored.
			continue
		}
		events = append(events, e)
	}
	return events, nil
}

// addEventsIfPodInFlight adds clusterEvent to inFlightEvents if the newPod is in inFlightPods.
// It returns true if pushed the event to the inFlightEvents.
func (aq *activeQueue) addEventsIfPodInFlight(oldPod, newPod *v1.Pod, events []framework.ClusterEvent) bool {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	_, ok := aq.inFlightPods[newPod.UID]
	if ok {
		for _, event := range events {
			aq.metricsRecorder.ObserveInFlightEventsAsync(event.Label(), 1, false)
			aq.inFlightEvents.PushBack(&clusterEvent{
				event:  event,
				oldObj: oldPod,
				newObj: newPod,
			})
		}
	}
	return ok
}

// addEventIfAnyInFlight adds clusterEvent to inFlightEvents if any pod is in inFlightPods.
// It returns true if pushed the event to the inFlightEvents.
func (aq *activeQueue) addEventIfAnyInFlight(oldObj, newObj interface{}, event framework.ClusterEvent) bool {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	if len(aq.inFlightPods) != 0 {
		aq.metricsRecorder.ObserveInFlightEventsAsync(event.Label(), 1, false)
		aq.inFlightEvents.PushBack(&clusterEvent{
			event:  event,
			oldObj: oldObj,
			newObj: newObj,
		})
		return true
	}
	return false
}

func (aq *activeQueue) schedulingCycle() int64 {
	aq.lock.RLock()
	defer aq.lock.RUnlock()
	return aq.schedCycle
}

// done must be called for pod returned by Pop. This allows the queue to
// keep track of which pods are currently being processed.
func (aq *activeQueue) done(pod types.UID) {
	aq.lock.Lock()
	defer aq.lock.Unlock()

	inFlightPod, ok := aq.inFlightPods[pod]
	if !ok {
		// This Pod is already done()ed.
		return
	}
	delete(aq.inFlightPods, pod)

	// Remove the pod from the list.
	aq.inFlightEvents.Remove(inFlightPod)

	aggrMetricsCounter := map[string]int{}
	// Remove events which are only referred to by this Pod
	// so that the inFlightEvents list doesn't grow infinitely.
	// If the pod was at the head of the list, then all
	// events between it and the next pod are no longer needed
	// and can be removed.
	for {
		e := aq.inFlightEvents.Front()
		if e == nil {
			// Empty list.
			break
		}
		ev, ok := e.Value.(*clusterEvent)
		if !ok {
			// A pod, must stop pruning.
			break
		}
		aq.inFlightEvents.Remove(e)
		aggrMetricsCounter[ev.event.Label()]--
	}

	for evLabel, count := range aggrMetricsCounter {
		aq.metricsRecorder.ObserveInFlightEventsAsync(evLabel, float64(count), false)
	}

	aq.metricsRecorder.ObserveInFlightEventsAsync(metrics.PodPoppedInFlightEvent, -1,
		// If it's the last Pod in inFlightPods, we should force-flush the metrics.
		// Otherwise, especially in small clusters, which don't get a new Pod frequently,
		// the metrics might not be flushed for a long time.
		len(aq.inFlightPods) == 0)
}

// close closes the activeQueue.
func (aq *activeQueue) close() {
	// We should call done() for all in-flight pods to clean up the inFlightEvents metrics.
	// It's safe even if the binding cycle running asynchronously calls done() afterwards
	// done() will just be a no-op.
	for pod := range aq.inFlightPods {
		aq.done(pod)
	}
	aq.lock.Lock()
	aq.closed = true
	aq.lock.Unlock()
}

// broadcast notifies the pop() operation that new pod(s) was added to the activeQueue.
func (aq *activeQueue) broadcast() {
	aq.cond.Broadcast()
}
