/*
Copyright 2014 The Kubernetes Authors.

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
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

// fakePodWorkers runs sync pod function in serial, so we can have
// deterministic behaviour in testing.
type fakePodWorkers struct {
	lock      sync.Mutex
	syncPodFn syncPodFnType
	cache     kubecontainer.Cache
	t         TestingInterface

	triggeredDeletion []types.UID
	triggeredTerminal []types.UID

	statusLock            sync.Mutex
	running               map[types.UID]bool
	terminating           map[types.UID]bool
	terminated            map[types.UID]bool
	terminationRequested  map[types.UID]bool
	finished              map[types.UID]bool
	removeRuntime         map[types.UID]bool
	removeContent         map[types.UID]bool
	terminatingStaticPods map[string]bool
}

func (f *fakePodWorkers) UpdatePod(options UpdatePodOptions) {
	f.lock.Lock()
	defer f.lock.Unlock()
	var uid types.UID
	switch {
	case options.Pod != nil:
		uid = options.Pod.UID
	case options.RunningPod != nil:
		uid = options.RunningPod.ID
	default:
		return
	}
	status, err := f.cache.Get(uid)
	if err != nil {
		f.t.Errorf("Unexpected error: %v", err)
	}
	switch options.UpdateType {
	case kubetypes.SyncPodKill:
		f.triggeredDeletion = append(f.triggeredDeletion, uid)
	default:
		isTerminal, err := f.syncPodFn(context.Background(), options.UpdateType, options.Pod, options.MirrorPod, status)
		if err != nil {
			f.t.Errorf("Unexpected error: %v", err)
		}
		if isTerminal {
			f.triggeredTerminal = append(f.triggeredTerminal, uid)
		}
	}
}

func (f *fakePodWorkers) SyncKnownPods(desiredPods []*v1.Pod) map[types.UID]PodWorkerSync {
	return map[types.UID]PodWorkerSync{}
}

func (f *fakePodWorkers) IsPodKnownTerminated(uid types.UID) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.terminated[uid]
}
func (f *fakePodWorkers) CouldHaveRunningContainers(uid types.UID) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.running[uid]
}
func (f *fakePodWorkers) ShouldPodBeFinished(uid types.UID) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.finished[uid]
}
func (f *fakePodWorkers) IsPodTerminationRequested(uid types.UID) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.terminationRequested[uid]
}
func (f *fakePodWorkers) ShouldPodContainersBeTerminating(uid types.UID) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.terminating[uid]
}
func (f *fakePodWorkers) ShouldPodRuntimeBeRemoved(uid types.UID) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.removeRuntime[uid]
}
func (f *fakePodWorkers) setPodRuntimeBeRemoved(uid types.UID) {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	f.removeRuntime = map[types.UID]bool{uid: true}
}
func (f *fakePodWorkers) ShouldPodContentBeRemoved(uid types.UID) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.removeContent[uid]
}
func (f *fakePodWorkers) IsPodForMirrorPodTerminatingByFullName(podFullname string) bool {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	return f.terminatingStaticPods[podFullname]
}

type TestingInterface interface {
	Errorf(format string, args ...interface{})
}

func newPodWithPhase(uid, name string, phase v1.PodPhase) *v1.Pod {
	pod := newNamedPod(uid, "ns", name, false)
	pod.Status.Phase = phase
	return pod
}

func newStaticPod(uid, name string) *v1.Pod {
	thirty := int64(30)
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:  types.UID(uid),
			Name: name,
			Annotations: map[string]string{
				kubetypes.ConfigSourceAnnotationKey: kubetypes.FileSource,
			},
		},
		Spec: v1.PodSpec{
			TerminationGracePeriodSeconds: &thirty,
		},
	}
}

func newNamedPod(uid, namespace, name string, isStatic bool) *v1.Pod {
	thirty := int64(30)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1.PodSpec{
			TerminationGracePeriodSeconds: &thirty,
		},
	}
	if isStatic {
		pod.Annotations = map[string]string{
			kubetypes.ConfigSourceAnnotationKey: kubetypes.FileSource,
		}
	}
	return pod
}

// syncPodRecord is a record of a sync pod call
type syncPodRecord struct {
	name        string
	updateType  kubetypes.SyncPodType
	runningPod  *kubecontainer.Pod
	terminated  bool
	gracePeriod *int64
}

type FakeQueueItem struct {
	UID   types.UID
	Delay time.Duration
}

type fakeQueue struct {
	lock         sync.Mutex
	queue        []FakeQueueItem
	currentStart int
}

func (q *fakeQueue) Empty() bool {
	q.lock.Lock()
	defer q.lock.Unlock()
	return (len(q.queue) - q.currentStart) == 0
}

func (q *fakeQueue) Items() []FakeQueueItem {
	q.lock.Lock()
	defer q.lock.Unlock()
	return append(make([]FakeQueueItem, 0, len(q.queue)), q.queue...)
}

func (q *fakeQueue) Set() sets.Set[string] {
	q.lock.Lock()
	defer q.lock.Unlock()
	work := sets.New[string]()
	for _, item := range q.queue[q.currentStart:] {
		work.Insert(string(item.UID))
	}
	return work
}

func (q *fakeQueue) Enqueue(uid types.UID, delay time.Duration) {
	q.lock.Lock()
	defer q.lock.Unlock()
	q.queue = append(q.queue, FakeQueueItem{UID: uid, Delay: delay})
}

func (q *fakeQueue) GetWork() []types.UID {
	q.lock.Lock()
	defer q.lock.Unlock()
	work := make([]types.UID, 0, len(q.queue)-q.currentStart)
	for _, item := range q.queue[q.currentStart:] {
		work = append(work, item.UID)
	}
	q.currentStart = len(q.queue)
	return work
}

type timeIncrementingWorkers struct {
	lock    sync.Mutex
	w       *podWorkers
	runtime *containertest.FakeRuntime
	holds   map[types.UID]chan struct{}
}

// UpdatePod increments the clock after UpdatePod is called, but before the workers
// are invoked, and then drains all workers before returning. The provided functions
// are invoked while holding the lock to prevent workers from receiving updates.
func (w *timeIncrementingWorkers) UpdatePod(options UpdatePodOptions, afterFns ...func()) {
	func() {
		w.lock.Lock()
		defer w.lock.Unlock()
		w.w.UpdatePod(options)
		w.w.clock.(*clocktesting.FakePassiveClock).SetTime(w.w.clock.Now().Add(time.Second))
		for _, fn := range afterFns {
			fn()
		}
	}()
	w.drainUnpausedWorkers()
}

// SyncKnownPods increments the clock after SyncKnownPods is called, but before the workers
// are invoked, and then drains all workers before returning.
func (w *timeIncrementingWorkers) SyncKnownPods(desiredPods []*v1.Pod) (knownPods map[types.UID]PodWorkerSync) {
	func() {
		w.lock.Lock()
		defer w.lock.Unlock()
		knownPods = w.w.SyncKnownPods(desiredPods)
		w.w.clock.(*clocktesting.FakePassiveClock).SetTime(w.w.clock.Now().Add(time.Second))
	}()
	w.drainUnpausedWorkers()
	return
}

func (w *timeIncrementingWorkers) PauseWorkers(uids ...types.UID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	if w.holds == nil {
		w.holds = make(map[types.UID]chan struct{})
	}
	for _, uid := range uids {
		if _, ok := w.holds[uid]; !ok {
			w.holds[uid] = make(chan struct{})
		}
	}
}

func (w *timeIncrementingWorkers) ReleaseWorkers(uids ...types.UID) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.ReleaseWorkersUnderLock(uids...)
}

func (w *timeIncrementingWorkers) ReleaseWorkersUnderLock(uids ...types.UID) {
	for _, uid := range uids {
		if ch, ok := w.holds[uid]; ok {
			close(ch)
			delete(w.holds, uid)
		}
	}
}

func (w *timeIncrementingWorkers) waitForPod(uid types.UID) {
	w.lock.Lock()
	ch, ok := w.holds[uid]
	w.lock.Unlock()
	if !ok {
		return
	}
	<-ch
}

func (w *timeIncrementingWorkers) drainUnpausedWorkers() {
	pausedWorkers := make(map[types.UID]struct{})
	for {
		for uid := range pausedWorkers {
			delete(pausedWorkers, uid)
		}
		stillWorking := false

		// ignore held workers
		w.lock.Lock()
		for uid := range w.holds {
			pausedWorkers[uid] = struct{}{}
		}
		w.lock.Unlock()

		// check for at least one still working non-paused worker
		w.w.podLock.Lock()
		for uid, worker := range w.w.podSyncStatuses {
			if _, ok := pausedWorkers[uid]; ok {
				continue
			}
			if worker.working {
				stillWorking = true
				break
			}
		}
		w.w.podLock.Unlock()

		if !stillWorking {
			break
		}
		time.Sleep(time.Millisecond)
	}
}

func (w *timeIncrementingWorkers) tick() {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.w.clock.(*clocktesting.FakePassiveClock).SetTime(w.w.clock.Now().Add(time.Second))
}

// createTimeIncrementingPodWorkers will guarantee that each call to UpdatePod and each worker goroutine invocation advances the clock by one second,
// although multiple workers will advance the clock in an unpredictable order. Use to observe
// successive internal updates to each update pod state when only a single pod is being updated.
func createTimeIncrementingPodWorkers() (*timeIncrementingWorkers, map[types.UID][]syncPodRecord) {
	nested, runtime, processed := createPodWorkers()
	w := &timeIncrementingWorkers{
		w:       nested,
		runtime: runtime,
	}
	nested.workerChannelFn = func(uid types.UID, in chan struct{}) <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			defer close(ch)
			// TODO: this is an eager loop, we might want to lazily read from in only once
			// ch is empty
			for range in {
				w.waitForPod(uid)
				w.tick()
				ch <- struct{}{}
			}
		}()
		return ch
	}
	return w, processed
}

func createPodWorkers() (*podWorkers, *containertest.FakeRuntime, map[types.UID][]syncPodRecord) {
	lock := sync.Mutex{}
	processed := make(map[types.UID][]syncPodRecord)
	fakeRecorder := &record.FakeRecorder{}
	fakeRuntime := &containertest.FakeRuntime{}
	fakeCache := containertest.NewFakeCache(fakeRuntime)
	fakeQueue := &fakeQueue{}
	clock := clocktesting.NewFakePassiveClock(time.Unix(1, 0))
	w := newPodWorkers(
		&podSyncerFuncs{
			syncPod: func(ctx context.Context, updateType kubetypes.SyncPodType, pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error) {
				func() {
					lock.Lock()
					defer lock.Unlock()
					pod := pod
					processed[pod.UID] = append(processed[pod.UID], syncPodRecord{
						name:       pod.Name,
						updateType: updateType,
					})
				}()
				return false, nil
			},
			syncTerminatingPod: func(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error {
				func() {
					lock.Lock()
					defer lock.Unlock()
					processed[pod.UID] = append(processed[pod.UID], syncPodRecord{
						name:        pod.Name,
						updateType:  kubetypes.SyncPodKill,
						gracePeriod: gracePeriod,
					})
				}()
				return nil
			},
			syncTerminatingRuntimePod: func(ctx context.Context, runningPod *kubecontainer.Pod) error {
				func() {
					lock.Lock()
					defer lock.Unlock()
					processed[runningPod.ID] = append(processed[runningPod.ID], syncPodRecord{
						name:       runningPod.Name,
						updateType: kubetypes.SyncPodKill,
						runningPod: runningPod,
					})
				}()
				return nil
			},
			syncTerminatedPod: func(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error {
				func() {
					lock.Lock()
					defer lock.Unlock()
					processed[pod.UID] = append(processed[pod.UID], syncPodRecord{
						name:       pod.Name,
						terminated: true,
					})
				}()
				return nil
			},
		},
		fakeRecorder,
		fakeQueue,
		time.Second,
		time.Millisecond,
		fakeCache,
	)
	workers := w.(*podWorkers)
	workers.clock = clock
	return workers, fakeRuntime, processed
}

func drainWorkers(podWorkers *podWorkers, numPods int) {
	for {
		stillWorking := false
		podWorkers.podLock.Lock()
		for i := 0; i < numPods; i++ {
			if s, ok := podWorkers.podSyncStatuses[types.UID(strconv.Itoa(i))]; ok && s.working {
				stillWorking = true
				break
			}
		}
		podWorkers.podLock.Unlock()
		if !stillWorking {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func drainWorkersExcept(podWorkers *podWorkers, uids ...types.UID) {
	set := sets.New[string]()
	for _, uid := range uids {
		set.Insert(string(uid))
	}
	for {
		stillWorking := false
		podWorkers.podLock.Lock()
		for k, v := range podWorkers.podSyncStatuses {
			if set.Has(string(k)) {
				continue
			}
			if v.working {
				stillWorking = true
				break
			}
		}
		podWorkers.podLock.Unlock()
		if !stillWorking {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func drainAllWorkers(podWorkers *podWorkers) {
	for {
		stillWorking := false
		podWorkers.podLock.Lock()
		for _, worker := range podWorkers.podSyncStatuses {
			if worker.working {
				stillWorking = true
				break
			}
		}
		podWorkers.podLock.Unlock()
		if !stillWorking {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestUpdatePodParallel(t *testing.T) {
	podWorkers, _, processed := createPodWorkers()

	numPods := 20
	for i := 0; i < numPods; i++ {
		for j := i; j < numPods; j++ {
			podWorkers.UpdatePod(UpdatePodOptions{
				Pod:        newNamedPod(strconv.Itoa(j), "ns", strconv.Itoa(i), false),
				UpdateType: kubetypes.SyncPodCreate,
			})
		}
	}
	drainWorkers(podWorkers, numPods)

	if len(processed) != numPods {
		t.Fatalf("Not all pods processed: %v", len(processed))
	}
	for i := 0; i < numPods; i++ {
		uid := types.UID(strconv.Itoa(i))
		events := processed[uid]
		if len(events) < 1 || len(events) > i+1 {
			t.Errorf("Pod %v processed %v times", i, len(events))
			continue
		}

		// PodWorker guarantees the last event will be processed
		last := len(events) - 1
		if events[last].name != strconv.Itoa(i) {
			t.Errorf("Pod %v: incorrect order %v, %#v", i, last, events)
		}
	}
}

func TestUpdatePod(t *testing.T) {
	one := int64(1)
	hasContext := func(status *podSyncStatus) *podSyncStatus {
		status.ctx, status.cancelFn = context.Background(), func() {}
		return status
	}
	withLabel := func(pod *v1.Pod, label, value string) *v1.Pod {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[label] = value
		return pod
	}
	withDeletionTimestamp := func(pod *v1.Pod, ts time.Time, gracePeriod *int64) *v1.Pod {
		pod.DeletionTimestamp = &metav1.Time{Time: ts}
		pod.DeletionGracePeriodSeconds = gracePeriod
		return pod
	}
	intp := func(i int64) *int64 {
		return &i
	}
	expectPodSyncStatus := func(t *testing.T, expected, status *podSyncStatus) {
		t.Helper()
		// handle special non-comparable fields
		if status != nil {
			if e, a := expected.ctx != nil, status.ctx != nil; e != a {
				t.Errorf("expected context %t, has context %t", e, a)
			} else {
				expected.ctx, status.ctx = nil, nil
			}
			if e, a := expected.cancelFn != nil, status.cancelFn != nil; e != a {
				t.Errorf("expected cancelFn %t, has cancelFn %t", e, a)
			} else {
				expected.cancelFn, status.cancelFn = nil, nil
			}
		}
		if e, a := expected, status; !reflect.DeepEqual(e, a) {
			t.Fatalf("unexpected status: %s", cmp.Diff(e, a, cmp.AllowUnexported(podSyncStatus{})))
		}
	}
	for _, tc := range []struct {
		name          string
		update        UpdatePodOptions
		runtimeStatus *kubecontainer.PodStatus
		prepare       func(t *testing.T, w *timeIncrementingWorkers) (afterUpdateFn func())

		expect                *podSyncStatus
		expectBeforeWorker    *podSyncStatus
		expectKnownTerminated bool
	}{
		{
			name: "a new pod is recorded and started",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodCreate,
				Pod:        newNamedPod("1", "ns", "running-pod", false),
			},
			expect: hasContext(&podSyncStatus{
				fullname:  "running-pod_ns",
				syncedAt:  time.Unix(1, 0),
				startedAt: time.Unix(3, 0),
				activeUpdate: &UpdatePodOptions{
					Pod: newNamedPod("1", "ns", "running-pod", false),
				},
			}),
		},
		{
			name: "a new pod is recorded and started unless it is a duplicate of an existing terminating pod UID",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodCreate,
				Pod:        withLabel(newNamedPod("1", "ns", "running-pod", false), "updated", "value"),
			},
			prepare: func(t *testing.T, w *timeIncrementingWorkers) func() {
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					Pod:        newNamedPod("1", "ns", "running-pod", false),
				})
				w.PauseWorkers("1")
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodKill,
					Pod:        newNamedPod("1", "ns", "running-pod", false),
				})
				return func() { w.ReleaseWorkersUnderLock("1") }
			},
			expect: hasContext(&podSyncStatus{
				fullname:           "running-pod_ns",
				syncedAt:           time.Unix(1, 0),
				startedAt:          time.Unix(3, 0),
				terminatingAt:      time.Unix(3, 0),
				terminatedAt:       time.Unix(6, 0),
				gracePeriod:        30,
				startedTerminating: true,
				restartRequested:   true, // because we received a create during termination
				finished:           true,
				activeUpdate: &UpdatePodOptions{
					Pod:            newNamedPod("1", "ns", "running-pod", false),
					KillPodOptions: &KillPodOptions{PodTerminationGracePeriodSecondsOverride: intp(30)},
				},
			}),
			expectKnownTerminated: true,
		},
		{
			name: "a new pod is recorded and started and running pod is ignored",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodCreate,
				Pod:        newNamedPod("1", "ns", "running-pod", false),
				RunningPod: &kubecontainer.Pod{ID: "1", Name: "orphaned-pod", Namespace: "ns"},
			},
			expect: hasContext(&podSyncStatus{
				fullname:  "running-pod_ns",
				syncedAt:  time.Unix(1, 0),
				startedAt: time.Unix(3, 0),
				activeUpdate: &UpdatePodOptions{
					Pod: newNamedPod("1", "ns", "running-pod", false),
				},
			}),
		},
		{
			name: "a running pod is terminated when an update contains a deletionTimestamp",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodUpdate,
				Pod:        withDeletionTimestamp(newNamedPod("1", "ns", "running-pod", false), time.Unix(1, 0), intp(15)),
			},
			prepare: func(t *testing.T, w *timeIncrementingWorkers) func() {
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					Pod:        newNamedPod("1", "ns", "running-pod", false),
				})
				return nil
			},
			expect: hasContext(&podSyncStatus{
				fullname:           "running-pod_ns",
				syncedAt:           time.Unix(1, 0),
				startedAt:          time.Unix(3, 0),
				terminatingAt:      time.Unix(3, 0),
				terminatedAt:       time.Unix(5, 0),
				gracePeriod:        15,
				startedTerminating: true,
				finished:           true,
				deleted:            true,
				activeUpdate: &UpdatePodOptions{
					Pod:            withDeletionTimestamp(newNamedPod("1", "ns", "running-pod", false), time.Unix(1, 0), intp(15)),
					KillPodOptions: &KillPodOptions{PodTerminationGracePeriodSecondsOverride: intp(15)},
				},
			}),
			expectKnownTerminated: true,
		},
		{
			name: "a running pod is terminated when an eviction is requested",
			update: UpdatePodOptions{
				UpdateType:     kubetypes.SyncPodKill,
				Pod:            newNamedPod("1", "ns", "running-pod", false),
				KillPodOptions: &KillPodOptions{Evict: true},
			},
			prepare: func(t *testing.T, w *timeIncrementingWorkers) func() {
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					Pod:        newNamedPod("1", "ns", "running-pod", false),
				})
				return nil
			},
			expect: hasContext(&podSyncStatus{
				fullname:           "running-pod_ns",
				syncedAt:           time.Unix(1, 0),
				startedAt:          time.Unix(3, 0),
				terminatingAt:      time.Unix(3, 0),
				terminatedAt:       time.Unix(5, 0),
				gracePeriod:        30,
				startedTerminating: true,
				finished:           true,
				evicted:            true,
				activeUpdate: &UpdatePodOptions{
					Pod: newNamedPod("1", "ns", "running-pod", false),
					KillPodOptions: &KillPodOptions{
						PodTerminationGracePeriodSecondsOverride: intp(30),
						Evict:                                    true,
					},
				},
			}),
			expectKnownTerminated: true,
		},
		{
			name: "a pod that is terminal and has never started must be terminated if the runtime does not have a cached terminal state",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodCreate,
				Pod:        newPodWithPhase("1", "done-pod", v1.PodSucceeded),
			},
			expect: hasContext(&podSyncStatus{
				fullname:      "done-pod_ns",
				syncedAt:      time.Unix(1, 0),
				terminatingAt: time.Unix(1, 0),
				startedAt:     time.Unix(3, 0),
				terminatedAt:  time.Unix(3, 0),
				activeUpdate: &UpdatePodOptions{
					Pod:            newPodWithPhase("1", "done-pod", v1.PodSucceeded),
					KillPodOptions: &KillPodOptions{PodTerminationGracePeriodSecondsOverride: intp(30)},
				},
				gracePeriod:        30,
				startedTerminating: true,
				finished:           true,
			}),
			expectKnownTerminated: true,
		},
		{
			name: "a pod that is terminal and has never started advances to finished if the runtime has a cached terminal state",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodCreate,
				Pod:        newPodWithPhase("1", "done-pod", v1.PodSucceeded),
			},
			runtimeStatus: &kubecontainer.PodStatus{ /* we know about this pod */ },
			expectBeforeWorker: &podSyncStatus{
				fullname:      "done-pod_ns",
				syncedAt:      time.Unix(1, 0),
				terminatingAt: time.Unix(1, 0),
				terminatedAt:  time.Unix(1, 0),
				pendingUpdate: &UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					Pod:        newPodWithPhase("1", "done-pod", v1.PodSucceeded),
				},
				finished:           false, // Should be marked as not finished initially (to ensure `SyncTerminatedPod` will run) and status will progress to terminated.
				startedTerminating: true,
				working:            true,
			},
			expect: hasContext(&podSyncStatus{
				fullname:           "done-pod_ns",
				syncedAt:           time.Unix(1, 0),
				terminatingAt:      time.Unix(1, 0),
				terminatedAt:       time.Unix(1, 0),
				startedAt:          time.Unix(3, 0),
				startedTerminating: true,
				finished:           true,
				activeUpdate: &UpdatePodOptions{
					UpdateType: kubetypes.SyncPodSync,
					Pod:        newPodWithPhase("1", "done-pod", v1.PodSucceeded),
				},

				// if we have never seen the pod before, a restart makes no sense
				restartRequested: false,
			}),
			expectKnownTerminated: true,
		},
		{
			name: "an orphaned running pod we have not seen is marked terminating and advances to finished and then is removed",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodKill,
				RunningPod: &kubecontainer.Pod{ID: "1", Name: "orphaned-pod", Namespace: "ns"},
			},
			expectBeforeWorker: &podSyncStatus{
				fullname:      "orphaned-pod_ns",
				syncedAt:      time.Unix(1, 0),
				terminatingAt: time.Unix(1, 0),
				pendingUpdate: &UpdatePodOptions{
					UpdateType:     kubetypes.SyncPodKill,
					RunningPod:     &kubecontainer.Pod{ID: "1", Name: "orphaned-pod", Namespace: "ns"},
					KillPodOptions: &KillPodOptions{PodTerminationGracePeriodSecondsOverride: &one},
				},
				gracePeriod:     1,
				deleted:         true,
				observedRuntime: true,
				working:         true,
			},
			// Once a running pod is fully terminated, we stop tracking it in history, and so it
			// is deliberately expected not to be known outside the pod worker since the source of
			// the pod is also not in the desired pod set.
			expectKnownTerminated: false,
		},
		{
			name: "an orphaned running pod with a non-kill update type does nothing",
			update: UpdatePodOptions{
				UpdateType: kubetypes.SyncPodCreate,
				RunningPod: &kubecontainer.Pod{ID: "1", Name: "orphaned-pod", Namespace: "ns"},
			},
			expect: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uid types.UID
			switch {
			case tc.update.Pod != nil:
				uid = tc.update.Pod.UID
			case tc.update.RunningPod != nil:
				uid = tc.update.RunningPod.ID
			default:
				t.Fatalf("unable to find uid for update")
			}

			var fns []func()

			podWorkers, _ := createTimeIncrementingPodWorkers()

			if tc.expectBeforeWorker != nil {
				fns = append(fns, func() {
					expectPodSyncStatus(t, tc.expectBeforeWorker, podWorkers.w.podSyncStatuses[uid])
				})
			}

			if tc.prepare != nil {
				if fn := tc.prepare(t, podWorkers); fn != nil {
					fns = append(fns, fn)
				}
			}

			// set up an initial pod status for the UpdatePod invocation which is
			// reset before workers call the podCache
			if tc.runtimeStatus != nil {
				podWorkers.runtime.PodStatus = *tc.runtimeStatus
				podWorkers.runtime.Err = nil
			} else {
				podWorkers.runtime.PodStatus = kubecontainer.PodStatus{}
				podWorkers.runtime.Err = status.Error(codes.NotFound, "No such pod")
			}
			fns = append(fns, func() {
				podWorkers.runtime.PodStatus = kubecontainer.PodStatus{}
				podWorkers.runtime.Err = nil
			})

			podWorkers.UpdatePod(tc.update, fns...)

			if podWorkers.w.IsPodKnownTerminated(uid) != tc.expectKnownTerminated {
				t.Errorf("podWorker.IsPodKnownTerminated expected to be %t", tc.expectKnownTerminated)
			}

			expectPodSyncStatus(t, tc.expect, podWorkers.w.podSyncStatuses[uid])

			// TODO: validate processed records for the pod based on the test case, which reduces
			// the amount of testing we need to do in kubelet_pods_test.go
		})
	}
}

func TestUpdatePodForRuntimePod(t *testing.T) {
	podWorkers, _, processed := createPodWorkers()

	// ignores running pod of wrong sync type
	podWorkers.UpdatePod(UpdatePodOptions{
		UpdateType: kubetypes.SyncPodCreate,
		RunningPod: &kubecontainer.Pod{ID: "1", Name: "1", Namespace: "test"},
	})
	drainAllWorkers(podWorkers)
	if len(processed) != 0 {
		t.Fatalf("Not all pods processed: %v", len(processed))
	}

	// creates synthetic pod
	podWorkers.UpdatePod(UpdatePodOptions{
		UpdateType: kubetypes.SyncPodKill,
		RunningPod: &kubecontainer.Pod{ID: "1", Name: "1", Namespace: "test"},
	})
	drainAllWorkers(podWorkers)
	if len(processed) != 1 {
		t.Fatalf("Not all pods processed: %v", processed)
	}
	updates := processed["1"]
	if len(updates) != 1 {
		t.Fatalf("unexpected updates: %v", updates)
	}
	if updates[0].runningPod == nil || updates[0].updateType != kubetypes.SyncPodKill || updates[0].name != "1" {
		t.Fatalf("unexpected update: %v", updates)
	}
}

func TestUpdatePodForTerminatedRuntimePod(t *testing.T) {
	podWorkers, _, processed := createPodWorkers()

	now := time.Now()
	podWorkers.podSyncStatuses[types.UID("1")] = &podSyncStatus{
		startedTerminating: true,
		terminatedAt:       now.Add(-time.Second),
		terminatingAt:      now.Add(-2 * time.Second),
		gracePeriod:        1,
	}

	// creates synthetic pod
	podWorkers.UpdatePod(UpdatePodOptions{
		UpdateType: kubetypes.SyncPodKill,
		RunningPod: &kubecontainer.Pod{ID: "1", Name: "1", Namespace: "test"},
	})
	drainAllWorkers(podWorkers)
	if len(processed) != 0 {
		t.Fatalf("Not all pods processed: %v", processed)
	}
	updates := processed["1"]
	if len(updates) != 0 {
		t.Fatalf("unexpected updates: %v", updates)
	}
}

func TestUpdatePodDoesNotForgetSyncPodKill(t *testing.T) {
	podWorkers, _, processed := createPodWorkers()
	numPods := 20
	for i := 0; i < numPods; i++ {
		pod := newNamedPod(strconv.Itoa(i), "ns", strconv.Itoa(i), false)
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodCreate,
		})
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
		})
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodUpdate,
		})
	}
	drainWorkers(podWorkers, numPods)
	if len(processed) != numPods {
		t.Errorf("Not all pods processed: %v", len(processed))
		return
	}
	for i := 0; i < numPods; i++ {
		uid := types.UID(strconv.Itoa(i))
		// each pod should be processed two or three times (kill,terminate or create,kill,terminate) because
		// we buffer pending updates and the pod worker may compress the create and kill
		syncPodRecords := processed[uid]
		var match bool
		grace := int64(30)
		for _, possible := range [][]syncPodRecord{
			{{name: string(uid), updateType: kubetypes.SyncPodKill, gracePeriod: &grace}, {name: string(uid), terminated: true}},
			{{name: string(uid), updateType: kubetypes.SyncPodCreate}, {name: string(uid), updateType: kubetypes.SyncPodKill, gracePeriod: &grace}, {name: string(uid), terminated: true}},
		} {
			if reflect.DeepEqual(possible, syncPodRecords) {
				match = true
				break
			}
		}
		if !match {
			t.Fatalf("unexpected history for pod %v: %#v", i, syncPodRecords)
		}
	}
}

func newUIDSet(uids ...types.UID) sets.Set[string] {
	set := sets.New[string]()
	for _, uid := range uids {
		set.Insert(string(uid))
	}
	return set
}

type terminalPhaseSync struct {
	lock     sync.Mutex
	fn       syncPodFnType
	terminal sets.Set[string]
}

func (s *terminalPhaseSync) SyncPod(ctx context.Context, updateType kubetypes.SyncPodType, pod *v1.Pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error) {
	isTerminal, err := s.fn(ctx, updateType, pod, mirrorPod, podStatus)
	if err != nil {
		return false, err
	}
	if !isTerminal {
		s.lock.Lock()
		defer s.lock.Unlock()
		isTerminal = s.terminal.Has(string(pod.UID))
	}
	return isTerminal, nil
}

func (s *terminalPhaseSync) SetTerminal(uid types.UID) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.terminal.Insert(string(uid))
}

func newTerminalPhaseSync(fn syncPodFnType) *terminalPhaseSync {
	return &terminalPhaseSync{
		fn:       fn,
		terminal: sets.New[string](),
	}
}

func TestTerminalPhaseTransition(t *testing.T) {
	podWorkers, _, _ := createPodWorkers()
	var channels WorkChannel
	podWorkers.workerChannelFn = channels.Intercept
	terminalPhaseSyncer := newTerminalPhaseSync(podWorkers.podSyncer.(*podSyncerFuncs).syncPod)
	podWorkers.podSyncer.(*podSyncerFuncs).syncPod = terminalPhaseSyncer.SyncPod

	// start pod
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("1", "test1", "pod1", false),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// should observe pod running
	pod1 := podWorkers.podSyncStatuses[types.UID("1")]
	if pod1.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod1)
	}

	// send another update to the pod
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("1", "test1", "pod1", false),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// should observe pod still running
	pod1 = podWorkers.podSyncStatuses[types.UID("1")]
	if pod1.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod1)
	}

	// the next sync should result in a transition to terminal
	terminalPhaseSyncer.SetTerminal(types.UID("1"))
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("1", "test1", "pod1", false),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// should observe pod terminating
	pod1 = podWorkers.podSyncStatuses[types.UID("1")]
	if !pod1.IsTerminationRequested() || !pod1.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod1)
	}
}

func TestStaticPodExclusion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	podWorkers, _, processed := createPodWorkers()
	var channels WorkChannel
	podWorkers.workerChannelFn = channels.Intercept

	testPod := newNamedPod("2-static", "test1", "pod1", true)
	if !kubetypes.IsStaticPod(testPod) {
		t.Fatalf("unable to test static pod")
	}

	// start two pods with the same name, one static, one apiserver
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("1-normal", "test1", "pod1", false),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("2-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// should observe both pods running
	pod1 := podWorkers.podSyncStatuses[types.UID("1-normal")]
	if pod1.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod1)
	}
	pod2 := podWorkers.podSyncStatuses[types.UID("2-static")]
	if pod2.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod2)
	}

	if len(processed) != 2 {
		t.Fatalf("unexpected synced pods: %#v", processed)
	}
	if e, a :=
		[]syncPodRecord{{name: "pod1", updateType: kubetypes.SyncPodUpdate}},
		processed[types.UID("2-static")]; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(e, a))
	}
	if e, a := map[string]types.UID{"pod1_test1": "2-static"}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}

	// attempt to start a second and third static pod, which should not start
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("3-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("4-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// should observe both pods running but last pod shouldn't have synced
	pod1 = podWorkers.podSyncStatuses[types.UID("1-normal")]
	if pod1.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod1)
	}
	pod2 = podWorkers.podSyncStatuses[types.UID("2-static")]
	if pod2.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod2)
	}
	pod3 := podWorkers.podSyncStatuses[types.UID("3-static")]
	if pod3.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod3)
	}
	pod4 := podWorkers.podSyncStatuses[types.UID("4-static")]
	if pod4.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod4)
	}

	if len(processed) != 2 {
		t.Fatalf("unexpected synced pods: %#v", processed)
	}
	if expected, actual :=
		[]syncPodRecord{{name: "pod1", updateType: kubetypes.SyncPodUpdate}},
		processed[types.UID("2-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}
	if expected, actual :=
		[]syncPodRecord(nil),
		processed[types.UID("3-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}
	if expected, actual :=
		[]syncPodRecord(nil),
		processed[types.UID("4-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}
	if e, a := map[string]types.UID{"pod1_test1": "2-static"}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	if e, a := map[string][]types.UID{"pod1_test1": {"3-static", "4-static"}}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}
	// verify all are enqueued
	if e, a := sets.New[string]("1-normal", "2-static", "4-static", "3-static"), podWorkers.workQueue.(*fakeQueue).Set(); !e.Equal(a) {
		t.Fatalf("unexpected queued items: %s", cmp.Diff(e, a))
	}

	// send a basic update for 3-static
	podWorkers.workQueue.GetWork()
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("3-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// 3-static should not be started because 2-static is still running
	if e, a := map[string]types.UID{"pod1_test1": "2-static"}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	if e, a := map[string][]types.UID{"pod1_test1": {"3-static", "4-static"}}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}
	// the queue should include a single item for 3-static (indicating we need to retry later)
	if e, a := sets.New[string]("3-static"), newUIDSet(podWorkers.workQueue.GetWork()...); !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected queued items: %s", cmp.Diff(e, a))
	}

	// mark 3-static as deleted while 2-static is still running
	podWorkers.workQueue.GetWork()
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("3-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodKill,
	})
	drainAllWorkers(podWorkers)

	// should observe 3-static as terminated because it has never started, but other state should be a no-op
	pod3 = podWorkers.podSyncStatuses[types.UID("3-static")]
	if !pod3.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod3)
	}
	// the queue should be empty because the worker is now done
	if e, a := sets.New[string](), newUIDSet(podWorkers.workQueue.GetWork()...); !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected queued items: %s", cmp.Diff(e, a))
	}
	// 2-static is still running
	if e, a := map[string]types.UID{"pod1_test1": "2-static"}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	// 3-static and 4-static are both still queued
	if e, a := map[string][]types.UID{"pod1_test1": {"3-static", "4-static"}}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}

	// terminate 2-static
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("2-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodKill,
	})
	drainAllWorkers(podWorkers)

	// should observe 2-static as terminated, and 2-static should no longer be reported as the started static pod
	pod2 = podWorkers.podSyncStatuses[types.UID("2-static")]
	if !pod2.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod3)
	}
	if e, a := map[string]types.UID{}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	if e, a := map[string][]types.UID{"pod1_test1": {"3-static", "4-static"}}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}

	// simulate a periodic event from the work queue for 4-static
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("4-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// 4-static should be started because 3-static has already terminated
	pod4 = podWorkers.podSyncStatuses[types.UID("4-static")]
	if pod4.IsTerminated() {
		t.Fatalf("unexpected pod state: %#v", pod3)
	}
	if e, a := map[string]types.UID{"pod1_test1": "4-static"}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	if e, a := map[string][]types.UID{}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}

	// initiate a sync with all pods remaining
	state := podWorkers.SyncKnownPods([]*v1.Pod{
		newNamedPod("1-normal", "test1", "pod1", false),
		newNamedPod("2-static", "test1", "pod1", true),
		newNamedPod("3-static", "test1", "pod1", true),
		newNamedPod("4-static", "test1", "pod1", true),
	})
	drainAllWorkers(podWorkers)

	// 2-static and 3-static should both be listed as terminated
	if e, a := map[types.UID]PodWorkerSync{
		"1-normal": {State: SyncPod, HasConfig: true},
		"2-static": {State: TerminatedPod, HasConfig: true, Static: true},
		"3-static": {State: TerminatedPod},
		"4-static": {State: SyncPod, HasConfig: true, Static: true},
	}, state; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected actual state: %s", cmp.Diff(e, a))
	}
	// 3-static is still in the config, it should still be in our status
	if status, ok := podWorkers.podSyncStatuses["3-static"]; !ok || status.terminatedAt.IsZero() || !status.finished || status.working {
		t.Fatalf("unexpected post termination status: %#v", status)
	}

	// initiate a sync with 3-static removed
	state = podWorkers.SyncKnownPods([]*v1.Pod{
		newNamedPod("1-normal", "test1", "pod1", false),
		newNamedPod("2-static", "test1", "pod1", true),
		newNamedPod("4-static", "test1", "pod1", true),
	})
	drainAllWorkers(podWorkers)

	// expect sync to put 3-static into final state and remove the status
	if e, a := map[types.UID]PodWorkerSync{
		"1-normal": {State: SyncPod, HasConfig: true},
		"2-static": {State: TerminatedPod, HasConfig: true, Static: true},
		"4-static": {State: SyncPod, HasConfig: true, Static: true},
	}, state; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected actual state: %s", cmp.Diff(e, a))
	}
	if status, ok := podWorkers.podSyncStatuses["3-static"]; ok {
		t.Fatalf("unexpected post termination status: %#v", status)
	}

	// start a static pod, kill it, then add another one, but ensure the pod worker
	// for pod 5 doesn't see the kill event (so it remains waiting to start)
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("5-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	// Wait for the previous work to be delivered to the worker
	drainAllWorkers(podWorkers)
	channels.Channel("5-static").Hold()
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("5-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodKill,
	})
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("6-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainWorkersExcept(podWorkers, "5-static")

	// pod 5 should have termination requested, but hasn't cleaned up
	pod5 := podWorkers.podSyncStatuses[types.UID("5-static")]
	if !pod5.IsTerminationRequested() || pod5.IsTerminated() {
		t.Fatalf("unexpected status for pod 5: %#v", pod5)
	}
	if e, a := map[string]types.UID{"pod1_test1": "4-static"}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	if e, a := map[string][]types.UID{"pod1_test1": {"5-static", "6-static"}}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}

	// terminate 4-static and wake 6-static
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("4-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodKill,
	})
	drainWorkersExcept(podWorkers, "5-static")
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("6-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainWorkersExcept(podWorkers, "5-static")

	// 5-static should still be waiting, 6-static should have started and synced
	pod5 = podWorkers.podSyncStatuses[types.UID("5-static")]
	if !pod5.IsTerminationRequested() || pod5.IsTerminated() {
		t.Fatalf("unexpected status for pod 5: %#v", pod5)
	}
	if e, a := map[string]types.UID{"pod1_test1": "6-static"}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	// no static pods should be waiting
	if e, a := map[string][]types.UID{}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}
	// prove 6-static synced
	if expected, actual :=
		[]syncPodRecord{{name: "pod1", updateType: kubetypes.SyncPodUpdate}},
		processed[types.UID("6-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}

	// ensure 5-static exits when we deliver the event out of order
	channels.Channel("5-static").Release()
	drainAllWorkers(podWorkers)
	pod5 = podWorkers.podSyncStatuses[types.UID("5-static")]
	if !pod5.IsTerminated() {
		t.Fatalf("unexpected status for pod 5: %#v", pod5)
	}

	// start three more static pods, kill the previous static pod blocking start,
	// and simulate the second pod of three (8) getting to run first
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("7-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("8-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("9-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("6-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodKill,
	})
	drainAllWorkers(podWorkers)
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("6-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodCreate,
	})
	drainAllWorkers(podWorkers)
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("8-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// 6 should have been detected as restartable
	if status := podWorkers.podSyncStatuses["6-static"]; !status.restartRequested {
		t.Fatalf("unexpected restarted static pod: %#v", status)
	}
	// 7 and 8 should both be waiting still with no syncs
	if e, a := map[string]types.UID{}, podWorkers.startedStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected started static pods: %s", cmp.Diff(e, a))
	}
	// only 7-static can start now, but it hasn't received an event
	if e, a := map[string][]types.UID{"pod1_test1": {"7-static", "8-static", "9-static"}}, podWorkers.waitingToStartStaticPodsByFullname; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected waiting static pods: %s", cmp.Diff(e, a))
	}
	// none of the new pods have synced
	if expected, actual :=
		[]syncPodRecord(nil),
		processed[types.UID("7-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}
	if expected, actual :=
		[]syncPodRecord(nil),
		processed[types.UID("8-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}
	if expected, actual :=
		[]syncPodRecord(nil),
		processed[types.UID("9-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}

	// terminate 7-static and wake 8-static
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("7-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodKill,
	})
	drainAllWorkers(podWorkers)
	podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        newNamedPod("8-static", "test1", "pod1", true),
		UpdateType: kubetypes.SyncPodUpdate,
	})
	drainAllWorkers(podWorkers)

	// 8 should have synced
	if expected, actual :=
		[]syncPodRecord{{name: "pod1", updateType: kubetypes.SyncPodUpdate}},
		processed[types.UID("8-static")]; !reflect.DeepEqual(expected, actual) {
		t.Fatalf("unexpected sync pod calls: %s", cmp.Diff(expected, actual))
	}

	// initiate a sync with all but 8-static pods undesired
	state = podWorkers.SyncKnownPods([]*v1.Pod{
		newNamedPod("8-static", "test1", "pod1", true),
	})
	drainAllWorkers(podWorkers)
	if e, a := map[types.UID]PodWorkerSync{
		"1-normal": {State: TerminatingPod, Orphan: true, HasConfig: true},
		"8-static": {State: SyncPod, HasConfig: true, Static: true},
	}, state; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected actual restartable: %s", cmp.Diff(e, a))
	}
}

type WorkChannelItem struct {
	out   chan struct{}
	lock  sync.Mutex
	pause bool
	queue int
}

func (item *WorkChannelItem) Handle() {
	item.lock.Lock()
	defer item.lock.Unlock()
	if item.pause {
		item.queue++
		return
	}
	item.out <- struct{}{}
}

func (item *WorkChannelItem) Hold() {
	item.lock.Lock()
	defer item.lock.Unlock()
	item.pause = true
}

func (item *WorkChannelItem) Close() {
	item.lock.Lock()
	defer item.lock.Unlock()
	if item.out != nil {
		close(item.out)
		item.out = nil
	}
}

// Release blocks until all work is passed on the chain
func (item *WorkChannelItem) Release() {
	item.lock.Lock()
	defer item.lock.Unlock()
	item.pause = false
	for i := 0; i < item.queue; i++ {
		item.out <- struct{}{}
	}
	item.queue = 0
}

// WorkChannel intercepts podWork channels between the pod worker and its child
// goroutines and allows tests to pause or release the flow of podWork to the
// workers.
type WorkChannel struct {
	lock     sync.Mutex
	channels map[types.UID]*WorkChannelItem
}

func (w *WorkChannel) Channel(uid types.UID) *WorkChannelItem {
	w.lock.Lock()
	defer w.lock.Unlock()
	if w.channels == nil {
		w.channels = make(map[types.UID]*WorkChannelItem)
	}
	channel, ok := w.channels[uid]
	if !ok {
		channel = &WorkChannelItem{
			out: make(chan struct{}, 1),
		}
		w.channels[uid] = channel
	}
	return channel
}

func (w *WorkChannel) Intercept(uid types.UID, ch chan struct{}) (outCh <-chan struct{}) {
	channel := w.Channel(uid)
	w.lock.Lock()

	defer w.lock.Unlock()
	go func() {
		defer func() {
			channel.Close()
			w.lock.Lock()
			defer w.lock.Unlock()
			delete(w.channels, uid)
		}()
		for range ch {
			channel.Handle()
		}
	}()
	return channel.out
}

func TestSyncKnownPods(t *testing.T) {
	podWorkers, _, _ := createPodWorkers()

	numPods := 20
	for i := 0; i < numPods; i++ {
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        newNamedPod(strconv.Itoa(i), "ns", "name", false),
			UpdateType: kubetypes.SyncPodUpdate,
		})
	}
	drainWorkers(podWorkers, numPods)

	if len(podWorkers.podUpdates) != numPods {
		t.Errorf("Incorrect number of open channels %v", len(podWorkers.podUpdates))
	}

	desiredPods := map[types.UID]sets.Empty{}
	desiredPods[types.UID("2")] = sets.Empty{}
	desiredPods[types.UID("14")] = sets.Empty{}
	desiredPodList := []*v1.Pod{newNamedPod("2", "ns", "name", false), newNamedPod("14", "ns", "name", false)}

	// kill all but the requested pods
	for i := 0; i < numPods; i++ {
		pod := newNamedPod(strconv.Itoa(i), "ns", "name", false)
		if _, ok := desiredPods[pod.UID]; ok {
			continue
		}
		if (i % 2) == 0 {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
		})
	}
	drainWorkers(podWorkers, numPods)

	if !podWorkers.ShouldPodContainersBeTerminating(types.UID("0")) {
		t.Errorf("Expected pod to be terminating")
	}
	if !podWorkers.ShouldPodContainersBeTerminating(types.UID("1")) {
		t.Errorf("Expected pod to be terminating")
	}
	if podWorkers.ShouldPodContainersBeTerminating(types.UID("2")) {
		t.Errorf("Expected pod to not be terminating")
	}
	if !podWorkers.IsPodTerminationRequested(types.UID("0")) {
		t.Errorf("Expected pod to be terminating")
	}
	if podWorkers.IsPodTerminationRequested(types.UID("2")) {
		t.Errorf("Expected pod to not be terminating")
	}

	if podWorkers.CouldHaveRunningContainers(types.UID("0")) {
		t.Errorf("Expected pod to be terminated (deleted and terminated)")
	}
	if podWorkers.CouldHaveRunningContainers(types.UID("1")) {
		t.Errorf("Expected pod to be terminated")
	}
	if !podWorkers.CouldHaveRunningContainers(types.UID("2")) {
		t.Errorf("Expected pod to not be terminated")
	}

	if !podWorkers.ShouldPodContentBeRemoved(types.UID("0")) {
		t.Errorf("Expected pod to be suitable for removal (deleted and terminated)")
	}
	if podWorkers.ShouldPodContentBeRemoved(types.UID("1")) {
		t.Errorf("Expected pod to not be suitable for removal (terminated but not deleted)")
	}
	if podWorkers.ShouldPodContentBeRemoved(types.UID("2")) {
		t.Errorf("Expected pod to not be suitable for removal (not terminated)")
	}

	if podWorkers.ShouldPodContainersBeTerminating(types.UID("abc")) {
		t.Errorf("Expected pod to not be known to be terminating (does not exist but not yet synced)")
	}
	if !podWorkers.CouldHaveRunningContainers(types.UID("abc")) {
		t.Errorf("Expected pod to potentially have running containers (does not exist but not yet synced)")
	}
	if podWorkers.ShouldPodContentBeRemoved(types.UID("abc")) {
		t.Errorf("Expected pod to not be suitable for removal (does not exist but not yet synced)")
	}

	podWorkers.SyncKnownPods(desiredPodList)
	if len(podWorkers.podUpdates) != 2 {
		t.Errorf("Incorrect number of open channels %v", len(podWorkers.podUpdates))
	}
	if _, exists := podWorkers.podUpdates[types.UID("2")]; !exists {
		t.Errorf("No updates channel for pod 2")
	}
	if _, exists := podWorkers.podUpdates[types.UID("14")]; !exists {
		t.Errorf("No updates channel for pod 14")
	}
	if podWorkers.IsPodTerminationRequested(types.UID("2")) {
		t.Errorf("Expected pod termination request to be cleared after sync")
	}

	if !podWorkers.ShouldPodContainersBeTerminating(types.UID("abc")) {
		t.Errorf("Expected pod to be expected to terminate containers (does not exist and synced at least once)")
	}
	if podWorkers.CouldHaveRunningContainers(types.UID("abc")) {
		t.Errorf("Expected pod to be known not to have running containers (does not exist and synced at least once)")
	}
	if !podWorkers.ShouldPodContentBeRemoved(types.UID("abc")) {
		t.Errorf("Expected pod to be suitable for removal (does not exist and synced at least once)")
	}

	// verify workers that are not terminated stay open even if config no longer
	// sees them
	podWorkers.SyncKnownPods(nil)
	drainAllWorkers(podWorkers)
	if len(podWorkers.podUpdates) != 0 {
		t.Errorf("Incorrect number of open channels %v", len(podWorkers.podUpdates))
	}
	if len(podWorkers.podSyncStatuses) != 2 {
		t.Errorf("Incorrect number of tracked statuses: %#v", podWorkers.podSyncStatuses)
	}

	for uid := range desiredPods {
		pod := newNamedPod(string(uid), "ns", "name", false)
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
		})
	}
	drainWorkers(podWorkers, numPods)

	// verify once those pods terminate (via some other flow) the workers are cleared
	podWorkers.SyncKnownPods(nil)
	if len(podWorkers.podUpdates) != 0 {
		t.Errorf("Incorrect number of open channels %v", len(podWorkers.podUpdates))
	}
	if len(podWorkers.podSyncStatuses) != 0 {
		t.Errorf("Incorrect number of tracked statuses: %#v", podWorkers.podSyncStatuses)
	}
}

func Test_removeTerminatedWorker(t *testing.T) {
	podUID := types.UID("pod-uid")

	testCases := []struct {
		desc                               string
		orphan                             bool
		podSyncStatus                      *podSyncStatus
		startedStaticPodsByFullname        map[string]types.UID
		waitingToStartStaticPodsByFullname map[string][]types.UID
		removed                            bool
		expectGracePeriod                  int64
		expectPending                      *UpdatePodOptions
	}{
		{
			desc: "finished worker",
			podSyncStatus: &podSyncStatus{
				finished: true,
			},
			removed: true,
		},
		{
			desc: "waiting to start worker because of another started pod with the same fullname",
			podSyncStatus: &podSyncStatus{
				finished: false,
				fullname: "fake-fullname",
			},
			startedStaticPodsByFullname: map[string]types.UID{
				"fake-fullname": "another-pod-uid",
			},
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"fake-fullname": {podUID},
			},
			removed: false,
		},
		{
			desc: "not yet started worker",
			podSyncStatus: &podSyncStatus{
				finished: false,
				fullname: "fake-fullname",
			},
			startedStaticPodsByFullname: make(map[string]types.UID),
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"fake-fullname": {podUID},
			},
			removed: false,
		},
		{
			desc: "orphaned not started worker",
			podSyncStatus: &podSyncStatus{
				finished: false,
				fullname: "fake-fullname",
			},
			orphan:  true,
			removed: true,
		},
		{
			desc: "orphaned started worker",
			podSyncStatus: &podSyncStatus{
				startedAt: time.Unix(1, 0),
				finished:  false,
				fullname:  "fake-fullname",
			},
			orphan:  true,
			removed: false,
		},
		{
			desc: "orphaned terminating worker with no activeUpdate",
			podSyncStatus: &podSyncStatus{
				startedAt:     time.Unix(1, 0),
				terminatingAt: time.Unix(2, 0),
				finished:      false,
				fullname:      "fake-fullname",
			},
			orphan:  true,
			removed: false,
		},
		{
			desc: "orphaned terminating worker",
			podSyncStatus: &podSyncStatus{
				startedAt:     time.Unix(1, 0),
				terminatingAt: time.Unix(2, 0),
				finished:      false,
				fullname:      "fake-fullname",
				activeUpdate: &UpdatePodOptions{
					Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "1"}},
				},
			},
			orphan:  true,
			removed: false,
			expectPending: &UpdatePodOptions{
				Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "1"}},
			},
		},
		{
			desc: "orphaned terminating worker with pendingUpdate",
			podSyncStatus: &podSyncStatus{
				startedAt:     time.Unix(1, 0),
				terminatingAt: time.Unix(2, 0),
				finished:      false,
				fullname:      "fake-fullname",
				working:       true,
				pendingUpdate: &UpdatePodOptions{
					Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "2"}},
				},
				activeUpdate: &UpdatePodOptions{
					Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "1"}},
				},
			},
			orphan:  true,
			removed: false,
			expectPending: &UpdatePodOptions{
				Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "2"}},
			},
		},
		{
			desc: "orphaned terminated worker with no activeUpdate",
			podSyncStatus: &podSyncStatus{
				startedAt:     time.Unix(1, 0),
				terminatingAt: time.Unix(2, 0),
				terminatedAt:  time.Unix(3, 0),
				finished:      false,
				fullname:      "fake-fullname",
			},
			orphan:  true,
			removed: false,
		},
		{
			desc: "orphaned terminated worker",
			podSyncStatus: &podSyncStatus{
				startedAt:     time.Unix(1, 0),
				terminatingAt: time.Unix(2, 0),
				terminatedAt:  time.Unix(3, 0),
				finished:      false,
				fullname:      "fake-fullname",
				activeUpdate: &UpdatePodOptions{
					Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "1"}},
				},
			},
			orphan:  true,
			removed: false,
			expectPending: &UpdatePodOptions{
				Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "1"}},
			},
		},
		{
			desc: "orphaned terminated worker with pendingUpdate",
			podSyncStatus: &podSyncStatus{
				startedAt:     time.Unix(1, 0),
				terminatingAt: time.Unix(2, 0),
				terminatedAt:  time.Unix(3, 0),
				finished:      false,
				working:       true,
				fullname:      "fake-fullname",
				pendingUpdate: &UpdatePodOptions{
					Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "2"}},
				},
				activeUpdate: &UpdatePodOptions{
					Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "1"}},
				},
			},
			orphan:  true,
			removed: false,
			expectPending: &UpdatePodOptions{
				Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, Name: "2"}},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			podWorkers, _, _ := createPodWorkers()
			podWorkers.podSyncStatuses[podUID] = tc.podSyncStatus
			podWorkers.podUpdates[podUID] = make(chan struct{}, 1)
			if tc.podSyncStatus.working {
				podWorkers.podUpdates[podUID] <- struct{}{}
			}
			podWorkers.startedStaticPodsByFullname = tc.startedStaticPodsByFullname
			podWorkers.waitingToStartStaticPodsByFullname = tc.waitingToStartStaticPodsByFullname

			podWorkers.removeTerminatedWorker(podUID, podWorkers.podSyncStatuses[podUID], tc.orphan)
			status, exists := podWorkers.podSyncStatuses[podUID]
			if tc.removed && exists {
				t.Fatalf("Expected pod worker to be removed")
			}
			if !tc.removed && !exists {
				t.Fatalf("Expected pod worker to not be removed")
			}
			if tc.removed {
				return
			}
			if tc.expectGracePeriod > 0 && status.gracePeriod != tc.expectGracePeriod {
				t.Errorf("Unexpected grace period %d", status.gracePeriod)
			}
			if !reflect.DeepEqual(tc.expectPending, status.pendingUpdate) {
				t.Errorf("Unexpected pending: %s", cmp.Diff(tc.expectPending, status.pendingUpdate))
			}
			if tc.expectPending != nil {
				if !status.working {
					t.Errorf("Should be working")
				}
				if len(podWorkers.podUpdates[podUID]) != 1 {
					t.Errorf("Should have one entry in podUpdates")
				}
			}
		})
	}
}

type simpleFakeKubelet struct {
	pod       *v1.Pod
	mirrorPod *v1.Pod
	podStatus *kubecontainer.PodStatus
	wg        sync.WaitGroup
}

func (kl *simpleFakeKubelet) SyncPod(ctx context.Context, updateType kubetypes.SyncPodType, pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error) {
	kl.pod, kl.mirrorPod, kl.podStatus = pod, mirrorPod, podStatus
	return false, nil
}

func (kl *simpleFakeKubelet) SyncPodWithWaitGroup(ctx context.Context, updateType kubetypes.SyncPodType, pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error) {
	kl.pod, kl.mirrorPod, kl.podStatus = pod, mirrorPod, podStatus
	kl.wg.Done()
	return false, nil
}

func (kl *simpleFakeKubelet) SyncTerminatingPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error {
	return nil
}

func (kl *simpleFakeKubelet) SyncTerminatingRuntimePod(ctx context.Context, runningPod *kubecontainer.Pod) error {
	return nil
}

func (kl *simpleFakeKubelet) SyncTerminatedPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error {
	return nil
}

// TestFakePodWorkers verifies that the fakePodWorkers behaves the same way as the real podWorkers
// for their invocation of the syncPodFn.
func TestFakePodWorkers(t *testing.T) {
	fakeRecorder := &record.FakeRecorder{}
	fakeRuntime := &containertest.FakeRuntime{}
	fakeCache := containertest.NewFakeCache(fakeRuntime)

	kubeletForRealWorkers := &simpleFakeKubelet{}
	kubeletForFakeWorkers := &simpleFakeKubelet{}
	realPodSyncer := newPodSyncerFuncs(kubeletForRealWorkers)
	realPodSyncer.syncPod = kubeletForRealWorkers.SyncPodWithWaitGroup

	realPodWorkers := newPodWorkers(
		realPodSyncer,
		fakeRecorder, queue.NewBasicWorkQueue(&clock.RealClock{}), time.Second, time.Second, fakeCache)
	fakePodWorkers := &fakePodWorkers{
		syncPodFn: kubeletForFakeWorkers.SyncPod,
		cache:     fakeCache,
		t:         t,
	}

	tests := []struct {
		pod       *v1.Pod
		mirrorPod *v1.Pod
	}{
		{
			&v1.Pod{},
			&v1.Pod{},
		},
		{
			podWithUIDNameNs("12345678", "foo", "new"),
			podWithUIDNameNs("12345678", "fooMirror", "new"),
		},
		{
			podWithUIDNameNs("98765", "bar", "new"),
			podWithUIDNameNs("98765", "barMirror", "new"),
		},
	}

	for i, tt := range tests {
		kubeletForRealWorkers.wg.Add(1)
		realPodWorkers.UpdatePod(UpdatePodOptions{
			Pod:        tt.pod,
			MirrorPod:  tt.mirrorPod,
			UpdateType: kubetypes.SyncPodUpdate,
		})
		fakePodWorkers.UpdatePod(UpdatePodOptions{
			Pod:        tt.pod,
			MirrorPod:  tt.mirrorPod,
			UpdateType: kubetypes.SyncPodUpdate,
		})

		kubeletForRealWorkers.wg.Wait()

		if !reflect.DeepEqual(kubeletForRealWorkers.pod, kubeletForFakeWorkers.pod) {
			t.Errorf("%d: Expected: %#v, Actual: %#v", i, kubeletForRealWorkers.pod, kubeletForFakeWorkers.pod)
		}

		if !reflect.DeepEqual(kubeletForRealWorkers.mirrorPod, kubeletForFakeWorkers.mirrorPod) {
			t.Errorf("%d: Expected: %#v, Actual: %#v", i, kubeletForRealWorkers.mirrorPod, kubeletForFakeWorkers.mirrorPod)
		}

		if !reflect.DeepEqual(kubeletForRealWorkers.podStatus, kubeletForFakeWorkers.podStatus) {
			t.Errorf("%d: Expected: %#v, Actual: %#v", i, kubeletForRealWorkers.podStatus, kubeletForFakeWorkers.podStatus)
		}
	}
}

// TestKillPodNowFunc tests the blocking kill pod function works with pod workers as expected.
func TestKillPodNowFunc(t *testing.T) {
	fakeRecorder := &record.FakeRecorder{}
	podWorkers, _, processed := createPodWorkers()
	killPodFunc := killPodNow(podWorkers, fakeRecorder)
	pod := newNamedPod("test", "ns", "test", false)
	gracePeriodOverride := int64(0)
	err := killPodFunc(pod, false, &gracePeriodOverride, func(status *v1.PodStatus) {
		status.Phase = v1.PodFailed
		status.Reason = "reason"
		status.Message = "message"
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	drainAllWorkers(podWorkers)
	if len(processed) != 1 {
		t.Fatalf("len(processed) expected: %v, actual: %#v", 1, processed)
	}
	syncPodRecords := processed[pod.UID]
	if len(syncPodRecords) != 2 {
		t.Fatalf("Pod processed expected %v times, got %#v", 1, syncPodRecords)
	}
	if syncPodRecords[0].updateType != kubetypes.SyncPodKill {
		t.Errorf("Pod update type was %v, but expected %v", syncPodRecords[0].updateType, kubetypes.SyncPodKill)
	}
	if !syncPodRecords[1].terminated {
		t.Errorf("Pod terminated %v, but expected %v", syncPodRecords[1].terminated, true)
	}
}

func Test_allowPodStart(t *testing.T) {
	testCases := []struct {
		desc                               string
		pod                                *v1.Pod
		podSyncStatuses                    map[types.UID]*podSyncStatus
		startedStaticPodsByFullname        map[string]types.UID
		waitingToStartStaticPodsByFullname map[string][]types.UID

		expectedStartedStaticPodsByFullname        map[string]types.UID
		expectedWaitingToStartStaticPodsByFullname map[string][]types.UID
		allowed                                    bool
		allowedEver                                bool
	}{
		{
			// TODO: Do we want to allow non-static pods with the same full name?
			// Note that it may disable the force deletion of pods.
			desc: "non-static pod",
			pod:  newNamedPod("uid-0", "ns", "test", false),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "test_",
				},
				"uid-1": {
					fullname: "test_",
				},
			},
			allowed:     true,
			allowedEver: true,
		},
		{
			// TODO: Do we want to allow a non-static pod with the same full name
			// as the started static pod?
			desc: "non-static pod when there is a started static pod with the same full name",
			pod:  newNamedPod("uid-0", "ns", "test", false),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "test_",
				},
				"uid-1": {
					fullname: "test_",
				},
			},
			startedStaticPodsByFullname: map[string]types.UID{
				"test_": types.UID("uid-1"),
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{
				"test_": types.UID("uid-1"),
			},
			allowed:     true,
			allowedEver: true,
		},
		{
			// TODO: Do we want to allow a static pod with the same full name as the
			// started non-static pod?
			desc: "static pod when there is a started non-static pod with the same full name",
			pod:  newNamedPod("uid-0", "ns", "test", false),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "test_",
				},
				"uid-1": {
					fullname: "test_",
				},
			},
			allowed:     true,
			allowedEver: true,
		},
		{
			desc: "static pod when there are no started static pods with the same full name",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "foo_",
				},
				"uid-1": {
					fullname: "bar_",
				},
			},
			startedStaticPodsByFullname: map[string]types.UID{
				"bar_": types.UID("uid-1"),
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-0"),
				"bar_": types.UID("uid-1"),
			},
			allowed:     true,
			allowedEver: true,
		},
		{
			desc: "static pod when there is a started static pod with the same full name",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "foo_",
				},
				"uid-1": {
					fullname: "foo_",
				},
			},
			startedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-1"),
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-1"),
			},
			allowed:     false,
			allowedEver: true,
		},
		{
			desc: "static pod if the static pod has already started",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "foo_",
				},
			},
			startedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-0"),
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-0"),
			},
			allowed:     true,
			allowedEver: true,
		},
		{
			desc: "static pod if the static pod is the first pod waiting to start",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "foo_",
				},
			},
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-0"),
				},
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-0"),
			},
			expectedWaitingToStartStaticPodsByFullname: make(map[string][]types.UID),
			allowed:     true,
			allowedEver: true,
		},
		{
			desc: "static pod if the static pod is not the first pod waiting to start",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "foo_",
				},
				"uid-1": {
					fullname: "foo_",
				},
			},
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-1"),
					types.UID("uid-0"),
				},
			},
			expectedStartedStaticPodsByFullname: make(map[string]types.UID),
			expectedWaitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-1"),
					types.UID("uid-0"),
				},
			},
			allowed:     false,
			allowedEver: true,
		},
		{
			desc: "static pod if the static pod is the first valid pod waiting to start / clean up until picking the first valid pod",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "foo_",
				},
				"uid-1": {
					fullname: "foo_",
				},
			},
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-2"),
					types.UID("uid-2"),
					types.UID("uid-3"),
					types.UID("uid-0"),
					types.UID("uid-1"),
				},
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-0"),
			},
			expectedWaitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-1"),
				},
			},
			allowed:     true,
			allowedEver: true,
		},
		{
			desc: "static pod if the static pod is the first pod that is not termination requested and waiting to start",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname: "foo_",
				},
				"uid-1": {
					fullname: "foo_",
				},
				"uid-2": {
					fullname:      "foo_",
					terminatingAt: time.Now(),
				},
				"uid-3": {
					fullname:     "foo_",
					terminatedAt: time.Now(),
				},
			},
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-2"),
					types.UID("uid-3"),
					types.UID("uid-0"),
					types.UID("uid-1"),
				},
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{
				"foo_": types.UID("uid-0"),
			},
			expectedWaitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-1"),
				},
			},
			allowed:     true,
			allowedEver: true,
		},
		{
			desc: "static pod if there is no sync status for the pod should be denied",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-1": {
					fullname: "foo_",
				},
				"uid-2": {
					fullname:      "foo_",
					terminatingAt: time.Now(),
				},
				"uid-3": {
					fullname:     "foo_",
					terminatedAt: time.Now(),
				},
			},
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-1"),
				},
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{},
			expectedWaitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-1"),
				},
			},
			allowed:     false,
			allowedEver: false,
		},
		{
			desc: "static pod if the static pod is terminated should not be allowed",
			pod:  newStaticPod("uid-0", "foo"),
			podSyncStatuses: map[types.UID]*podSyncStatus{
				"uid-0": {
					fullname:      "foo_",
					terminatingAt: time.Now(),
				},
			},
			waitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-2"),
					types.UID("uid-3"),
					types.UID("uid-0"),
					types.UID("uid-1"),
				},
			},
			expectedStartedStaticPodsByFullname: map[string]types.UID{},
			expectedWaitingToStartStaticPodsByFullname: map[string][]types.UID{
				"foo_": {
					types.UID("uid-2"),
					types.UID("uid-3"),
					types.UID("uid-0"),
					types.UID("uid-1"),
				},
			},
			allowed:     false,
			allowedEver: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			podWorkers, _, _ := createPodWorkers()
			if tc.podSyncStatuses != nil {
				podWorkers.podSyncStatuses = tc.podSyncStatuses
			}
			if tc.startedStaticPodsByFullname != nil {
				podWorkers.startedStaticPodsByFullname = tc.startedStaticPodsByFullname
			}
			if tc.waitingToStartStaticPodsByFullname != nil {
				podWorkers.waitingToStartStaticPodsByFullname = tc.waitingToStartStaticPodsByFullname
			}
			allowed, allowedEver := podWorkers.allowPodStart(tc.pod)
			if allowed != tc.allowed {
				if tc.allowed {
					t.Errorf("Pod should be allowed")
				} else {
					t.Errorf("Pod should not be allowed")
				}
			}

			if allowedEver != tc.allowedEver {
				if tc.allowedEver {
					t.Errorf("Pod should be allowed ever")
				} else {
					t.Errorf("Pod should not be allowed ever")
				}
			}

			// if maps are neither nil nor empty
			if len(podWorkers.startedStaticPodsByFullname) != 0 ||
				len(podWorkers.startedStaticPodsByFullname) != len(tc.expectedStartedStaticPodsByFullname) {
				if !reflect.DeepEqual(
					podWorkers.startedStaticPodsByFullname,
					tc.expectedStartedStaticPodsByFullname) {
					t.Errorf("startedStaticPodsByFullname: expected %v, got %v",
						tc.expectedStartedStaticPodsByFullname,
						podWorkers.startedStaticPodsByFullname)
				}
			}

			// if maps are neither nil nor empty
			if len(podWorkers.waitingToStartStaticPodsByFullname) != 0 ||
				len(podWorkers.waitingToStartStaticPodsByFullname) != len(tc.expectedWaitingToStartStaticPodsByFullname) {
				if !reflect.DeepEqual(
					podWorkers.waitingToStartStaticPodsByFullname,
					tc.expectedWaitingToStartStaticPodsByFullname) {
					t.Errorf("waitingToStartStaticPodsByFullname: expected %v, got %v",
						tc.expectedWaitingToStartStaticPodsByFullname,
						podWorkers.waitingToStartStaticPodsByFullname)
				}
			}
		})
	}
}

func Test_calculateEffectiveGracePeriod(t *testing.T) {
	zero := int64(0)
	two := int64(2)
	five := int64(5)
	thirty := int64(30)
	testCases := []struct {
		desc                                 string
		podSpecTerminationGracePeriodSeconds *int64
		podDeletionGracePeriodSeconds        *int64
		gracePeriodOverride                  *int64
		expectedGracePeriod                  int64
	}{
		{
			desc:                                 "use termination grace period from the spec when no overrides",
			podSpecTerminationGracePeriodSeconds: &thirty,
			expectedGracePeriod:                  thirty,
		},
		{
			desc:                                 "use pod DeletionGracePeriodSeconds when set",
			podSpecTerminationGracePeriodSeconds: &thirty,
			podDeletionGracePeriodSeconds:        &five,
			expectedGracePeriod:                  five,
		},
		{
			desc:                                 "use grace period override when set",
			podSpecTerminationGracePeriodSeconds: &thirty,
			podDeletionGracePeriodSeconds:        &five,
			gracePeriodOverride:                  &two,
			expectedGracePeriod:                  two,
		},
		{
			desc:                                 "use 1 when pod DeletionGracePeriodSeconds is zero",
			podSpecTerminationGracePeriodSeconds: &thirty,
			podDeletionGracePeriodSeconds:        &zero,
			expectedGracePeriod:                  1,
		},
		{
			desc:                                 "use 1 when grace period override is zero",
			podSpecTerminationGracePeriodSeconds: &thirty,
			podDeletionGracePeriodSeconds:        &five,
			gracePeriodOverride:                  &zero,
			expectedGracePeriod:                  1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			pod := newNamedPod("1", "ns", "running-pod", false)
			pod.Spec.TerminationGracePeriodSeconds = tc.podSpecTerminationGracePeriodSeconds
			pod.DeletionGracePeriodSeconds = tc.podDeletionGracePeriodSeconds
			gracePeriod, _ := calculateEffectiveGracePeriod(&podSyncStatus{}, pod, &KillPodOptions{
				PodTerminationGracePeriodSecondsOverride: tc.gracePeriodOverride,
			})
			if gracePeriod != tc.expectedGracePeriod {
				t.Errorf("Expected a grace period of %v, but was %v", tc.expectedGracePeriod, gracePeriod)
			}
		})
	}
}
