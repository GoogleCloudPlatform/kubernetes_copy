/*
Copyright 2017 The Kubernetes Authors.

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

// This file contains structures that implement scheduling queue types.
// Scheduling queues hold pods waiting to be scheduled. This file implements a
// priority queue which has two sub queues and a additional data structure,
// namely: activeQ, backoffQ and unschedulablePods.
// - activeQ holds pods that are being considered for scheduling.
// - backoffQ holds pods that moved from unschedulablePods and will move to
//   activeQ when their backoff periods complete.
// - unschedulablePods holds pods that were already attempted for scheduling and
//   are currently determined to be unschedulable.

package queue

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/backend/heap"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/interpodaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util"
	"k8s.io/utils/clock"
)

const (
	// DefaultPodMaxInUnschedulablePodsDuration is the default value for the maximum
	// time a pod can stay in unschedulablePods. If a pod stays in unschedulablePods
	// for longer than this value, the pod will be moved from unschedulablePods to
	// backoffQ or activeQ. If this value is empty, the default value (5min)
	// will be used.
	DefaultPodMaxInUnschedulablePodsDuration time.Duration = 5 * time.Minute
	// Scheduling queue names
	activeQ           = "Active"
	backoffQ          = "Backoff"
	unschedulablePods = "Unschedulable"

	preEnqueue = "PreEnqueue"
)

const (
	// DefaultPodInitialBackoffDuration is the default value for the initial backoff duration
	// for unschedulable pods. To change the default podInitialBackoffDurationSeconds used by the
	// scheduler, update the ComponentConfig value in defaults.go
	DefaultPodInitialBackoffDuration time.Duration = 1 * time.Second
	// DefaultPodMaxBackoffDuration is the default value for the max backoff duration
	// for unschedulable pods. To change the default podMaxBackoffDurationSeconds used by the
	// scheduler, update the ComponentConfig value in defaults.go
	DefaultPodMaxBackoffDuration time.Duration = 10 * time.Second
)

// PreEnqueueCheck is a function type. It's used to build functions that
// run against a Pod and the caller can choose to enqueue or skip the Pod
// by the checking result.
type PreEnqueueCheck func(pod *v1.Pod) bool

// SchedulingQueue is an interface for a queue to store pods waiting to be scheduled.
// The interface follows a pattern similar to cache.FIFO and cache.Heap and
// makes it easy to use those data structures as a SchedulingQueue.
type SchedulingQueue interface {
	framework.PodNominator
	Add(logger klog.Logger, pod *v1.Pod)
	// Activate moves the given pods to activeQ.
	// If a pod isn't found in unschedulablePods or backoffQ and it's in-flight,
	// the wildcard event is registered so that the pod will be requeued when it comes back.
	// But, if a pod isn't found in unschedulablePods or backoffQ and it's not in-flight (i.e., completely unknown pod),
	// Activate would ignore the pod.
	Activate(logger klog.Logger, pods map[string]*v1.Pod)
	// AddUnschedulableIfNotPresent adds an unschedulable pod back to scheduling queue.
	// The podSchedulingCycle represents the current scheduling cycle number which can be
	// returned by calling SchedulingCycle().
	AddUnschedulableIfNotPresent(logger klog.Logger, pod *framework.QueuedPodInfo, podSchedulingCycle int64) error
	// SchedulingCycle returns the current number of scheduling cycle which is
	// cached by scheduling queue. Normally, incrementing this number whenever
	// a pod is popped (e.g. called Pop()) is enough.
	SchedulingCycle() int64
	// Pop removes the head of the queue and returns it. It blocks if the
	// queue is empty and waits until a new item is added to the queue.
	Pop(logger klog.Logger) (*framework.QueuedPodInfo, error)
	// Done must be called for pod returned by Pop. This allows the queue to
	// keep track of which pods are currently being processed.
	Done(types.UID)
	Update(logger klog.Logger, oldPod, newPod *v1.Pod)
	Delete(pod *v1.Pod)
	// Important Note: preCheck shouldn't include anything that depends on the in-tree plugins' logic.
	// (e.g., filter Pods based on added/updated Node's capacity, etc.)
	// We know currently some do, but we'll eventually remove them in favor of the scheduling queue hint.
	MoveAllToActiveOrBackoffQueue(logger klog.Logger, event framework.ClusterEvent, oldObj, newObj interface{}, preCheck PreEnqueueCheck)
	AssignedPodAdded(logger klog.Logger, pod *v1.Pod)
	AssignedPodUpdated(logger klog.Logger, oldPod, newPod *v1.Pod, event framework.ClusterEvent)

	// Close closes the SchedulingQueue so that the goroutine which is
	// waiting to pop items can exit gracefully.
	Close()
	// Run starts the goroutines managing the queue.
	Run(logger klog.Logger)

	// The following functions are supposed to be used only for testing or debugging.
	GetPod(name, namespace string) (*framework.QueuedPodInfo, bool)
	PendingPods() ([]*v1.Pod, string)
	InFlightPods() []*v1.Pod
	PodsInActiveQ() []*v1.Pod
}

// NewSchedulingQueue initializes a priority queue as a new scheduling queue.
func NewSchedulingQueue(
	lessFn framework.LessFunc,
	informerFactory informers.SharedInformerFactory,
	opts ...Option) SchedulingQueue {
	return NewPriorityQueue(lessFn, informerFactory, opts...)
}

// PriorityQueue implements a scheduling queue.
// The head of PriorityQueue is the highest priority pending pod. This structure
// has two sub queues and a additional data structure, namely: activeQ,
// backoffQ and unschedulablePods.
//   - activeQ holds pods that are being considered for scheduling.
//   - backoffQ holds pods that moved from unschedulablePods and will move to
//     activeQ when their backoff periods complete.
//   - unschedulablePods holds pods that were already attempted for scheduling and
//     are currently determined to be unschedulable.
type PriorityQueue struct {
	*nominator

	stop  chan struct{}
	clock clock.Clock

	// lock takes precedence and should be taken first,
	// before any other locks in the queue (activeQueue.lock or nominator.nLock).
	// Correct locking order is: lock > activeQueue.lock > nominator.nLock.
	lock sync.RWMutex

	// pod initial backoff duration.
	podInitialBackoffDuration time.Duration
	// pod maximum backoff duration.
	podMaxBackoffDuration time.Duration
	// the maximum time a pod can stay in the unschedulablePods.
	podMaxInUnschedulablePodsDuration time.Duration

	activeQ activeQueuer
	// podBackoffQ is a heap ordered by backoff expiry. Pods which have completed backoff
	// are popped from this heap before the scheduler looks at activeQ
	podBackoffQ *heap.Heap[*framework.QueuedPodInfo]
	// unschedulablePods holds pods that have been tried and determined unschedulable.
	unschedulablePods *UnschedulablePods
	// moveRequestCycle caches the sequence number of scheduling cycle when we
	// received a move request. Unschedulable pods in and before this scheduling
	// cycle will be put back to activeQueue if we were trying to schedule them
	// when we received move request.
	// TODO: this will be removed after SchedulingQueueHint goes to stable and the feature gate is removed.
	moveRequestCycle int64

	// preEnqueuePluginMap is keyed with profile name, valued with registered preEnqueue plugins.
	preEnqueuePluginMap map[string][]framework.PreEnqueuePlugin
	// queueingHintMap is keyed with profile name, valued with registered queueing hint functions.
	queueingHintMap QueueingHintMapPerProfile

	nsLister listersv1.NamespaceLister

	metricsRecorder metrics.MetricAsyncRecorder
	// pluginMetricsSamplePercent is the percentage of plugin metrics to be sampled.
	pluginMetricsSamplePercent int

	// isSchedulingQueueHintEnabled indicates whether the feature gate for the scheduling queue is enabled.
	isSchedulingQueueHintEnabled bool
}

// QueueingHintFunction is the wrapper of QueueingHintFn that has PluginName.
type QueueingHintFunction struct {
	PluginName     string
	QueueingHintFn framework.QueueingHintFn
}

// clusterEvent has the event and involved objects.
type clusterEvent struct {
	event framework.ClusterEvent
	// oldObj is the object that involved this event.
	oldObj interface{}
	// newObj is the object that involved this event.
	newObj interface{}
}

type priorityQueueOptions struct {
	clock                             clock.Clock
	podInitialBackoffDuration         time.Duration
	podMaxBackoffDuration             time.Duration
	podMaxInUnschedulablePodsDuration time.Duration
	podLister                         listersv1.PodLister
	metricsRecorder                   metrics.MetricAsyncRecorder
	pluginMetricsSamplePercent        int
	preEnqueuePluginMap               map[string][]framework.PreEnqueuePlugin
	queueingHintMap                   QueueingHintMapPerProfile
}

// Option configures a PriorityQueue
type Option func(*priorityQueueOptions)

// WithClock sets clock for PriorityQueue, the default clock is clock.RealClock.
func WithClock(clock clock.Clock) Option {
	return func(o *priorityQueueOptions) {
		o.clock = clock
	}
}

// WithPodInitialBackoffDuration sets pod initial backoff duration for PriorityQueue.
func WithPodInitialBackoffDuration(duration time.Duration) Option {
	return func(o *priorityQueueOptions) {
		o.podInitialBackoffDuration = duration
	}
}

// WithPodMaxBackoffDuration sets pod max backoff duration for PriorityQueue.
func WithPodMaxBackoffDuration(duration time.Duration) Option {
	return func(o *priorityQueueOptions) {
		o.podMaxBackoffDuration = duration
	}
}

// WithPodLister sets pod lister for PriorityQueue.
func WithPodLister(pl listersv1.PodLister) Option {
	return func(o *priorityQueueOptions) {
		o.podLister = pl
	}
}

// WithPodMaxInUnschedulablePodsDuration sets podMaxInUnschedulablePodsDuration for PriorityQueue.
func WithPodMaxInUnschedulablePodsDuration(duration time.Duration) Option {
	return func(o *priorityQueueOptions) {
		o.podMaxInUnschedulablePodsDuration = duration
	}
}

// QueueingHintMapPerProfile is keyed with profile name, valued with queueing hint map registered for the profile.
type QueueingHintMapPerProfile map[string]QueueingHintMap

// QueueingHintMap is keyed with ClusterEvent, valued with queueing hint functions registered for the event.
type QueueingHintMap map[framework.ClusterEvent][]*QueueingHintFunction

// WithQueueingHintMapPerProfile sets queueingHintMap for PriorityQueue.
func WithQueueingHintMapPerProfile(m QueueingHintMapPerProfile) Option {
	return func(o *priorityQueueOptions) {
		o.queueingHintMap = m
	}
}

// WithPreEnqueuePluginMap sets preEnqueuePluginMap for PriorityQueue.
func WithPreEnqueuePluginMap(m map[string][]framework.PreEnqueuePlugin) Option {
	return func(o *priorityQueueOptions) {
		o.preEnqueuePluginMap = m
	}
}

// WithMetricsRecorder sets metrics recorder.
func WithMetricsRecorder(recorder metrics.MetricAsyncRecorder) Option {
	return func(o *priorityQueueOptions) {
		o.metricsRecorder = recorder
	}
}

// WithPluginMetricsSamplePercent sets the percentage of plugin metrics to be sampled.
func WithPluginMetricsSamplePercent(percent int) Option {
	return func(o *priorityQueueOptions) {
		o.pluginMetricsSamplePercent = percent
	}
}

var defaultPriorityQueueOptions = priorityQueueOptions{
	clock:                             clock.RealClock{},
	podInitialBackoffDuration:         DefaultPodInitialBackoffDuration,
	podMaxBackoffDuration:             DefaultPodMaxBackoffDuration,
	podMaxInUnschedulablePodsDuration: DefaultPodMaxInUnschedulablePodsDuration,
}

// Making sure that PriorityQueue implements SchedulingQueue.
var _ SchedulingQueue = &PriorityQueue{}

// newQueuedPodInfoForLookup builds a QueuedPodInfo object for a lookup in the queue.
func newQueuedPodInfoForLookup(pod *v1.Pod, plugins ...string) *framework.QueuedPodInfo {
	// Since this is only used for a lookup in the queue, we only need to set the Pod,
	// and so we avoid creating a full PodInfo, which is expensive to instantiate frequently.
	return &framework.QueuedPodInfo{
		PodInfo:              &framework.PodInfo{Pod: pod},
		UnschedulablePlugins: sets.New(plugins...),
	}
}

// NewPriorityQueue creates a PriorityQueue object.
func NewPriorityQueue(
	lessFn framework.LessFunc,
	informerFactory informers.SharedInformerFactory,
	opts ...Option,
) *PriorityQueue {
	options := defaultPriorityQueueOptions
	if options.podLister == nil {
		options.podLister = informerFactory.Core().V1().Pods().Lister()
	}
	for _, opt := range opts {
		opt(&options)
	}

	isSchedulingQueueHintEnabled := utilfeature.DefaultFeatureGate.Enabled(features.SchedulerQueueingHints)

	pq := &PriorityQueue{
		clock:                             options.clock,
		stop:                              make(chan struct{}),
		podInitialBackoffDuration:         options.podInitialBackoffDuration,
		podMaxBackoffDuration:             options.podMaxBackoffDuration,
		podMaxInUnschedulablePodsDuration: options.podMaxInUnschedulablePodsDuration,
		activeQ:                           newActiveQueue(heap.NewWithRecorder(podInfoKeyFunc, heap.LessFunc[*framework.QueuedPodInfo](lessFn), metrics.NewActivePodsRecorder()), isSchedulingQueueHintEnabled, options.metricsRecorder),
		unschedulablePods:                 newUnschedulablePods(metrics.NewUnschedulablePodsRecorder(), metrics.NewGatedPodsRecorder()),
		preEnqueuePluginMap:               options.preEnqueuePluginMap,
		queueingHintMap:                   options.queueingHintMap,
		metricsRecorder:                   options.metricsRecorder,
		pluginMetricsSamplePercent:        options.pluginMetricsSamplePercent,
		moveRequestCycle:                  -1,
		isSchedulingQueueHintEnabled:      isSchedulingQueueHintEnabled,
	}
	pq.podBackoffQ = heap.NewWithRecorder(podInfoKeyFunc, pq.podsCompareBackoffCompleted, metrics.NewBackoffPodsRecorder())
	pq.nsLister = informerFactory.Core().V1().Namespaces().Lister()
	pq.nominator = newPodNominator(options.podLister)

	return pq
}

// Run starts the goroutine to pump from podBackoffQ to activeQ
func (p *PriorityQueue) Run(logger klog.Logger) {
	go wait.Until(func() {
		p.flushBackoffQCompleted(logger)
	}, 1.0*time.Second, p.stop)
	go wait.Until(func() {
		p.flushUnschedulablePodsLeftover(logger)
	}, 30*time.Second, p.stop)
}

// queueingStrategy indicates how the scheduling queue should enqueue the Pod from unschedulable pod pool.
type queueingStrategy int

const (
	// queueSkip indicates that the scheduling queue should skip requeuing the Pod to activeQ/backoffQ.
	queueSkip queueingStrategy = iota
	// queueAfterBackoff indicates that the scheduling queue should requeue the Pod after backoff is completed.
	queueAfterBackoff
	// queueImmediately indicates that the scheduling queue should skip backoff and requeue the Pod immediately to activeQ.
	queueImmediately
)

// isEventOfInterest returns true if the event is of interest by some plugins.
func (p *PriorityQueue) isEventOfInterest(logger klog.Logger, event framework.ClusterEvent) bool {
	if event.IsWildCard() {
		// Wildcard event moves Pods that failed with any plugins.
		return true
	}

	for _, hintMap := range p.queueingHintMap {
		for eventToMatch := range hintMap {
			if eventToMatch.Match(event) {
				// This event is interested by some plugins.
				return true
			}
		}
	}

	logger.V(6).Info("receive an event that isn't interested by any enabled plugins", "event", event)

	return false
}

// isPodWorthRequeuing calls QueueingHintFn of only plugins registered in pInfo.unschedulablePlugins and pInfo.PendingPlugins.
//
// If any of pInfo.PendingPlugins return Queue,
// the scheduling queue is supposed to enqueue this Pod to activeQ, skipping backoffQ.
// If any of pInfo.unschedulablePlugins return Queue,
// the scheduling queue is supposed to enqueue this Pod to activeQ/backoffQ depending on the remaining backoff time of the Pod.
// If all QueueingHintFns returns Skip, the scheduling queue enqueues the Pod back to unschedulable Pod pool
// because no plugin changes the scheduling result via the event.
func (p *PriorityQueue) isPodWorthRequeuing(logger klog.Logger, pInfo *framework.QueuedPodInfo, event framework.ClusterEvent, oldObj, newObj interface{}) queueingStrategy {
	rejectorPlugins := pInfo.UnschedulablePlugins.Union(pInfo.PendingPlugins)
	if rejectorPlugins.Len() == 0 {
		logger.V(6).Info("Worth requeuing because no failed plugins", "pod", klog.KObj(pInfo.Pod))
		return queueAfterBackoff
	}

	if event.IsWildCard() {
		// If the wildcard event has a Pod in newObj,
		// that indicates that the event wants to be effective for the Pod only.
		// Specifically, EventForceActivate could have a target Pod in newObj.
		if newObj != nil {
			if pod, ok := newObj.(*v1.Pod); !ok || pod.UID != pInfo.Pod.UID {
				// This wildcard event is not for this Pod.
				if ok {
					logger.V(6).Info("Not worth requeuing because the event is wildcard, but for another pod", "pod", klog.KObj(pInfo.Pod), "event", event.Label(), "newObj", klog.KObj(pod))
				}
				return queueSkip
			}
		}

		// If the wildcard event is special one as someone wants to force all Pods to move to activeQ/backoffQ.
		// We return queueAfterBackoff in this case, while resetting all blocked plugins.
		logger.V(6).Info("Worth requeuing because the event is wildcard", "pod", klog.KObj(pInfo.Pod), "event", event.Label())
		return queueAfterBackoff
	}

	hintMap, ok := p.queueingHintMap[pInfo.Pod.Spec.SchedulerName]
	if !ok {
		// shouldn't reach here unless bug.
		logger.Error(nil, "No QueueingHintMap is registered for this profile", "profile", pInfo.Pod.Spec.SchedulerName, "pod", klog.KObj(pInfo.Pod))
		return queueAfterBackoff
	}

	pod := pInfo.Pod
	queueStrategy := queueSkip
	for eventToMatch, hintfns := range hintMap {
		if !eventToMatch.Match(event) {
			continue
		}

		for _, hintfn := range hintfns {
			if !rejectorPlugins.Has(hintfn.PluginName) {
				// skip if it's not hintfn from rejectorPlugins.
				continue
			}

			start := time.Now()
			hint, err := hintfn.QueueingHintFn(logger, pod, oldObj, newObj)
			if err != nil {
				// If the QueueingHintFn returned an error, we should treat the event as Queue so that we can prevent
				// the Pod from being stuck in the unschedulable pod pool.
				oldObjMeta, newObjMeta, asErr := util.As[klog.KMetadata](oldObj, newObj)
				if asErr != nil {
					logger.Error(err, "QueueingHintFn returns error", "event", event, "plugin", hintfn.PluginName, "pod", klog.KObj(pod))
				} else {
					logger.Error(err, "QueueingHintFn returns error", "event", event, "plugin", hintfn.PluginName, "pod", klog.KObj(pod), "oldObj", klog.KObj(oldObjMeta), "newObj", klog.KObj(newObjMeta))
				}
				hint = framework.Queue
			}
			p.metricsRecorder.ObserveQueueingHintDurationAsync(hintfn.PluginName, event.Label(), queueingHintToLabel(hint, err), metrics.SinceInSeconds(start))

			if hint == framework.QueueSkip {
				continue
			}

			if pInfo.PendingPlugins.Has(hintfn.PluginName) {
				// interprets Queue from the Pending plugin as queueImmediately.
				// We can return immediately because queueImmediately is the highest priority.
				return queueImmediately
			}

			// interprets Queue from the unschedulable plugin as queueAfterBackoff.

			if pInfo.PendingPlugins.Len() == 0 {
				// We can return immediately because no Pending plugins, which only can make queueImmediately, registered in this Pod,
				// and queueAfterBackoff is the second highest priority.
				return queueAfterBackoff
			}

			// We can't return immediately because there are some Pending plugins registered in this Pod.
			// We need to check if those plugins return Queue or not and if they do, we return queueImmediately.
			queueStrategy = queueAfterBackoff
		}
	}

	return queueStrategy
}

// queueingHintToLabel converts a hint and an error from QHint to a label string.
func queueingHintToLabel(hint framework.QueueingHint, err error) string {
	if err != nil {
		return metrics.QueueingHintResultError
	}

	switch hint {
	case framework.Queue:
		return metrics.QueueingHintResultQueue
	case framework.QueueSkip:
		return metrics.QueueingHintResultQueueSkip
	}

	// Shouldn't reach here.
	return ""
}

// runPreEnqueuePlugins iterates PreEnqueue function in each registered PreEnqueuePlugin.
// It returns true if all PreEnqueue function run successfully; otherwise returns false
// upon the first failure.
// Note: we need to associate the failed plugin to `pInfo`, so that the pod can be moved back
// to activeQ by related cluster event.
func (p *PriorityQueue) runPreEnqueuePlugins(ctx context.Context, pInfo *framework.QueuedPodInfo) bool {
	logger := klog.FromContext(ctx)
	var s *framework.Status
	pod := pInfo.Pod
	startTime := p.clock.Now()
	defer func() {
		metrics.FrameworkExtensionPointDuration.WithLabelValues(preEnqueue, s.Code().String(), pod.Spec.SchedulerName).Observe(metrics.SinceInSeconds(startTime))
	}()

	shouldRecordMetric := rand.Intn(100) < p.pluginMetricsSamplePercent
	for _, pl := range p.preEnqueuePluginMap[pod.Spec.SchedulerName] {
		s = p.runPreEnqueuePlugin(ctx, pl, pod, shouldRecordMetric)
		if s.IsSuccess() {
			continue
		}
		pInfo.UnschedulablePlugins.Insert(pl.Name())
		metrics.UnschedulableReason(pl.Name(), pod.Spec.SchedulerName).Inc()
		if s.Code() == framework.Error {
			logger.Error(s.AsError(), "Unexpected error running PreEnqueue plugin", "pod", klog.KObj(pod), "plugin", pl.Name())
		} else {
			logger.V(4).Info("Status after running PreEnqueue plugin", "pod", klog.KObj(pod), "plugin", pl.Name(), "status", s)
		}
		return false
	}
	return true
}

func (p *PriorityQueue) runPreEnqueuePlugin(ctx context.Context, pl framework.PreEnqueuePlugin, pod *v1.Pod, shouldRecordMetric bool) *framework.Status {
	if !shouldRecordMetric {
		return pl.PreEnqueue(ctx, pod)
	}
	startTime := p.clock.Now()
	s := pl.PreEnqueue(ctx, pod)
	p.metricsRecorder.ObservePluginDurationAsync(preEnqueue, pl.Name(), s.Code().String(), p.clock.Since(startTime).Seconds())
	return s
}

// moveToActiveQ tries to add pod to active queue and remove it from unschedulable and backoff queues.
// It returns 2 parameters:
// 1. a boolean flag to indicate whether the pod is added successfully.
// 2. an error for the caller to act on.
func (p *PriorityQueue) moveToActiveQ(logger klog.Logger, pInfo *framework.QueuedPodInfo, event string) bool {
	gatedBefore := pInfo.Gated
	pInfo.Gated = !p.runPreEnqueuePlugins(context.Background(), pInfo)

	added := false
	p.activeQ.underLock(func(unlockedActiveQ unlockedActiveQueuer) {
		if pInfo.Gated {
			// Add the Pod to unschedulablePods if it's not passing PreEnqueuePlugins.
			if unlockedActiveQ.Has(pInfo) {
				return
			}
			if p.podBackoffQ.Has(pInfo) {
				return
			}
			p.unschedulablePods.addOrUpdate(pInfo)
			return
		}
		if pInfo.InitialAttemptTimestamp == nil {
			now := p.clock.Now()
			pInfo.InitialAttemptTimestamp = &now
		}

		unlockedActiveQ.AddOrUpdate(pInfo)
		added = true

		p.unschedulablePods.delete(pInfo.Pod, gatedBefore)
		_ = p.podBackoffQ.Delete(pInfo) // Don't need to react when pInfo is not found.
		logger.V(5).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pInfo.Pod), "event", event, "queue", activeQ)
		metrics.SchedulerQueueIncomingPods.WithLabelValues("active", event).Inc()
		if event == framework.EventUnscheduledPodAdd.Label() || event == framework.EventUnscheduledPodUpdate.Label() {
			p.AddNominatedPod(logger, pInfo.PodInfo, nil)
		}
	})
	return added
}

// Add adds a pod to the active queue. It should be called only when a new pod
// is added so there is no chance the pod is already in active/unschedulable/backoff queues
func (p *PriorityQueue) Add(logger klog.Logger, pod *v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()

	pInfo := p.newQueuedPodInfo(pod)
	if added := p.moveToActiveQ(logger, pInfo, framework.EventUnscheduledPodAdd.Label()); added {
		p.activeQ.broadcast()
	}
}

// Activate moves the given pods to activeQ.
// If a pod isn't found in unschedulablePods or backoffQ and it's in-flight,
// the wildcard event is registered so that the pod will be requeued when it comes back.
// But, if a pod isn't found in unschedulablePods or backoffQ and it's not in-flight (i.e., completely unknown pod),
// Activate would ignore the pod.
func (p *PriorityQueue) Activate(logger klog.Logger, pods map[string]*v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()

	activated := false
	for _, pod := range pods {
		if p.activate(logger, pod) {
			activated = true
			continue
		}

		// If this pod is in-flight, register the activation event (for when QHint is enabled) or update moveRequestCycle (for when QHints is disabled)
		// so that the pod will be requeued when it comes back.
		// Specifically in the in-tree plugins, this is for the scenario with the preemption plugin
		// where the async preemption API calls are all done or fail at some point before the Pod comes back to the queue.
		p.activeQ.addEventsIfPodInFlight(nil, pod, []framework.ClusterEvent{framework.EventForceActivate})
		p.moveRequestCycle = p.activeQ.schedulingCycle()
	}

	if activated {
		p.activeQ.broadcast()
	}
}

func (p *PriorityQueue) activate(logger klog.Logger, pod *v1.Pod) bool {
	var pInfo *framework.QueuedPodInfo
	// Verify if the pod is present in unschedulablePods or backoffQ.
	if pInfo = p.unschedulablePods.get(pod); pInfo == nil {
		// If the pod doesn't belong to unschedulablePods or backoffQ, don't activate it.
		// The pod can be already in activeQ.
		var exists bool
		pInfo, exists = p.podBackoffQ.Get(newQueuedPodInfoForLookup(pod))
		if !exists {
			return false
		}
	}

	if pInfo == nil {
		// Redundant safe check. We shouldn't reach here.
		logger.Error(nil, "Internal error: cannot obtain pInfo")
		return false
	}

	return p.moveToActiveQ(logger, pInfo, framework.ForceActivate)
}

// isPodBackingoff returns true if a pod is still waiting for its backoff timer.
// If this returns true, the pod should not be re-tried.
func (p *PriorityQueue) isPodBackingoff(podInfo *framework.QueuedPodInfo) bool {
	boTime := p.getBackoffTime(podInfo)
	return boTime.After(p.clock.Now())
}

// SchedulingCycle returns current scheduling cycle.
func (p *PriorityQueue) SchedulingCycle() int64 {
	return p.activeQ.schedulingCycle()
}

// determineSchedulingHintForInFlightPod looks at the unschedulable plugins of the given Pod
// and determines the scheduling hint for this Pod while checking the events that happened during in-flight.
func (p *PriorityQueue) determineSchedulingHintForInFlightPod(logger klog.Logger, pInfo *framework.QueuedPodInfo) queueingStrategy {
	if len(pInfo.UnschedulablePlugins) == 0 && len(pInfo.PendingPlugins) == 0 {
		// No failed plugins are associated with this Pod.
		// Meaning something unusual (a temporal failure on kube-apiserver, etc) happened and this Pod gets moved back to the queue.
		// In this case, we should retry scheduling it because this Pod may not be retried until the next flush.
		return queueAfterBackoff
	}

	events, err := p.activeQ.clusterEventsForPod(logger, pInfo)
	if err != nil {
		logger.Error(err, "Error getting cluster events for pod", "pod", klog.KObj(pInfo.Pod))
		return queueAfterBackoff
	}

	// check if there is an event that makes this Pod schedulable based on pInfo.UnschedulablePlugins.
	queueingStrategy := queueSkip
	for _, e := range events {
		logger.V(5).Info("Checking event for in-flight pod", "pod", klog.KObj(pInfo.Pod), "event", e.event.Label())

		switch p.isPodWorthRequeuing(logger, pInfo, e.event, e.oldObj, e.newObj) {
		case queueSkip:
			continue
		case queueImmediately:
			// queueImmediately is the highest priority.
			// No need to go through the rest of the events.
			return queueImmediately
		case queueAfterBackoff:
			// replace schedulingHint with queueAfterBackoff
			queueingStrategy = queueAfterBackoff
			if pInfo.PendingPlugins.Len() == 0 {
				// We can return immediately because no Pending plugins, which only can make queueImmediately, registered in this Pod,
				// and queueAfterBackoff is the second highest priority.
				return queueAfterBackoff
			}
		}
	}
	return queueingStrategy
}

// addUnschedulableWithoutQueueingHint inserts a pod that cannot be scheduled into
// the queue, unless it is already in the queue. Normally, PriorityQueue puts
// unschedulable pods in `unschedulablePods`. But if there has been a recent move
// request, then the pod is put in `podBackoffQ`.
// TODO: This function is called only when p.isSchedulingQueueHintEnabled is false,
// and this will be removed after SchedulingQueueHint goes to stable and the feature gate is removed.
func (p *PriorityQueue) addUnschedulableWithoutQueueingHint(logger klog.Logger, pInfo *framework.QueuedPodInfo, podSchedulingCycle int64) error {
	pod := pInfo.Pod
	// Refresh the timestamp since the pod is re-added.
	pInfo.Timestamp = p.clock.Now()

	// When the queueing hint is enabled, they are used differently.
	// But, we use all of them as UnschedulablePlugins when the queueing hint isn't enabled so that we don't break the old behaviour.
	rejectorPlugins := pInfo.UnschedulablePlugins.Union(pInfo.PendingPlugins)

	// If a move request has been received, move it to the BackoffQ, otherwise move
	// it to unschedulablePods.
	for plugin := range rejectorPlugins {
		metrics.UnschedulableReason(plugin, pInfo.Pod.Spec.SchedulerName).Inc()
	}
	if p.moveRequestCycle >= podSchedulingCycle || len(rejectorPlugins) == 0 {
		// Two cases to move a Pod to ctive/backoff queue:
		// - The Pod is rejected by some plugins, but a move request is received after this Pod's scheduling cycle is started.
		//   In this case, the received event may be make Pod schedulable and we should retry scheduling it.
		// - No unschedulable plugins are associated with this Pod,
		//   meaning something unusual (a temporal failure on kube-apiserver, etc) happened and this Pod gets moved back to the queue.
		//   In this case, we should retry scheduling it because this Pod may not be retried until the next flush.
		p.podBackoffQ.AddOrUpdate(pInfo)
		logger.V(5).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pod), "event", framework.ScheduleAttemptFailure, "queue", backoffQ)
		metrics.SchedulerQueueIncomingPods.WithLabelValues("backoff", framework.ScheduleAttemptFailure).Inc()
	} else {
		p.unschedulablePods.addOrUpdate(pInfo)
		logger.V(5).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pod), "event", framework.ScheduleAttemptFailure, "queue", unschedulablePods)
		metrics.SchedulerQueueIncomingPods.WithLabelValues("unschedulable", framework.ScheduleAttemptFailure).Inc()
	}

	return nil
}

// AddUnschedulableIfNotPresent inserts a pod that cannot be scheduled into
// the queue, unless it is already in the queue. Normally, PriorityQueue puts
// unschedulable pods in `unschedulablePods`. But if there has been a recent move
// request, then the pod is put in `podBackoffQ`.
func (p *PriorityQueue) AddUnschedulableIfNotPresent(logger klog.Logger, pInfo *framework.QueuedPodInfo, podSchedulingCycle int64) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// In any case, this Pod will be moved back to the queue and we should call Done.
	defer p.Done(pInfo.Pod.UID)

	pod := pInfo.Pod
	if p.unschedulablePods.get(pod) != nil {
		return fmt.Errorf("Pod %v is already present in unschedulable queue", klog.KObj(pod))
	}

	if p.activeQ.has(pInfo) {
		return fmt.Errorf("Pod %v is already present in the active queue", klog.KObj(pod))
	}
	if p.podBackoffQ.Has(pInfo) {
		return fmt.Errorf("Pod %v is already present in the backoff queue", klog.KObj(pod))
	}

	if len(pInfo.UnschedulablePlugins) == 0 && len(pInfo.PendingPlugins) == 0 {
		// This Pod came back because of some unexpected errors (e.g., a network issue).
		pInfo.ConsecutiveErrorsCount++
	} else {
		// This Pod is rejected by some plugins, not coming back due to unexpected errors (e.g., a network issue)
		pInfo.UnschedulableCount++
		// We should reset the error count because the error is gone.
		pInfo.ConsecutiveErrorsCount = 0
	}

	if !p.isSchedulingQueueHintEnabled {
		// fall back to the old behavior which doesn't depend on the queueing hint.
		return p.addUnschedulableWithoutQueueingHint(logger, pInfo, podSchedulingCycle)
	}

	// Refresh the timestamp since the pod is re-added.
	pInfo.Timestamp = p.clock.Now()

	// If a move request has been received, move it to the BackoffQ, otherwise move
	// it to unschedulablePods.
	rejectorPlugins := pInfo.UnschedulablePlugins.Union(pInfo.PendingPlugins)
	for plugin := range rejectorPlugins {
		metrics.UnschedulableReason(plugin, pInfo.Pod.Spec.SchedulerName).Inc()
	}

	// We check whether this Pod may change its scheduling result by any of events that happened during scheduling.
	schedulingHint := p.determineSchedulingHintForInFlightPod(logger, pInfo)

	// In this case, we try to requeue this Pod to activeQ/backoffQ.
	queue := p.requeuePodWithQueueingStrategy(logger, pInfo, schedulingHint, framework.ScheduleAttemptFailure)
	logger.V(3).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pod), "event", framework.ScheduleAttemptFailure, "queue", queue, "schedulingCycle", podSchedulingCycle, "hint", schedulingHint, "unschedulable plugins", rejectorPlugins)
	if queue == activeQ {
		// When the Pod is moved to activeQ, need to let p.cond know so that the Pod will be pop()ed out.
		p.activeQ.broadcast()
	}

	return nil
}

// flushBackoffQCompleted Moves all pods from backoffQ which have completed backoff in to activeQ
func (p *PriorityQueue) flushBackoffQCompleted(logger klog.Logger) {
	p.lock.Lock()
	defer p.lock.Unlock()
	activated := false
	for {
		pInfo, ok := p.podBackoffQ.Peek()
		if !ok || pInfo == nil {
			break
		}
		pod := pInfo.Pod
		if p.isPodBackingoff(pInfo) {
			break
		}
		_, err := p.podBackoffQ.Pop()
		if err != nil {
			logger.Error(err, "Unable to pop pod from backoff queue despite backoff completion", "pod", klog.KObj(pod))
			break
		}
		if added := p.moveToActiveQ(logger, pInfo, framework.BackoffComplete); added {
			activated = true
		}
	}

	if activated {
		p.activeQ.broadcast()
	}
}

// flushUnschedulablePodsLeftover moves pods which stay in unschedulablePods
// longer than podMaxInUnschedulablePodsDuration to backoffQ or activeQ.
func (p *PriorityQueue) flushUnschedulablePodsLeftover(logger klog.Logger) {
	p.lock.Lock()
	defer p.lock.Unlock()

	var podsToMove []*framework.QueuedPodInfo
	currentTime := p.clock.Now()
	for _, pInfo := range p.unschedulablePods.podInfoMap {
		lastScheduleTime := pInfo.Timestamp
		if currentTime.Sub(lastScheduleTime) > p.podMaxInUnschedulablePodsDuration {
			podsToMove = append(podsToMove, pInfo)
		}
	}

	if len(podsToMove) > 0 {
		p.movePodsToActiveOrBackoffQueue(logger, podsToMove, framework.EventUnschedulableTimeout, nil, nil)
	}
}

// Pop removes the head of the active queue and returns it. It blocks if the
// activeQ is empty and waits until a new item is added to the queue. It
// increments scheduling cycle when a pod is popped.
// Note: This method should NOT be locked by the p.lock at any moment,
// as it would lead to scheduling throughput degradation.
func (p *PriorityQueue) Pop(logger klog.Logger) (*framework.QueuedPodInfo, error) {
	return p.activeQ.pop(logger)
}

// Done must be called for pod returned by Pop. This allows the queue to
// keep track of which pods are currently being processed.
func (p *PriorityQueue) Done(pod types.UID) {
	if !p.isSchedulingQueueHintEnabled {
		// do nothing if schedulingQueueHint is disabled.
		// In that case, we don't have inFlightPods and inFlightEvents.
		return
	}
	p.activeQ.done(pod)
}

func (p *PriorityQueue) InFlightPods() []*v1.Pod {
	if !p.isSchedulingQueueHintEnabled {
		// do nothing if schedulingQueueHint is disabled.
		// In that case, we don't have inFlightPods and inFlightEvents.
		return nil
	}
	return p.activeQ.listInFlightPods()
}

// isPodUpdated checks if the pod is updated in a way that it may have become
// schedulable. It drops status of the pod and compares it with old version,
// except for pod.status.resourceClaimStatuses: changing that may have an
// effect on scheduling.
func isPodUpdated(oldPod, newPod *v1.Pod) bool {
	strip := func(pod *v1.Pod) *v1.Pod {
		p := pod.DeepCopy()
		p.ResourceVersion = ""
		p.Generation = 0
		p.Status = v1.PodStatus{
			ResourceClaimStatuses: pod.Status.ResourceClaimStatuses,
		}
		p.ManagedFields = nil
		p.Finalizers = nil
		return p
	}
	return !reflect.DeepEqual(strip(oldPod), strip(newPod))
}

// Update updates a pod in the active or backoff queue if present. Otherwise, it removes
// the item from the unschedulable queue if pod is updated in a way that it may
// become schedulable and adds the updated one to the active queue.
// If pod is not present in any of the queues, it is added to the active queue.
func (p *PriorityQueue) Update(logger klog.Logger, oldPod, newPod *v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()

	var events []framework.ClusterEvent
	if p.isSchedulingQueueHintEnabled {
		events = framework.PodSchedulingPropertiesChange(newPod, oldPod)
		// The inflight pod will be requeued using the latest version from the informer cache, which matches what the event delivers.
		// Record this Pod update because
		// this update may make the Pod schedulable in case it gets rejected and comes back to the queue.
		// We can clean it up once we change updatePodInSchedulingQueue to call MoveAllToActiveOrBackoffQueue.
		// See https://github.com/kubernetes/kubernetes/pull/125578#discussion_r1648338033 for more context.
		if exists := p.activeQ.addEventsIfPodInFlight(oldPod, newPod, events); exists {
			logger.V(6).Info("The pod doesn't be queued for now because it's being scheduled and will be queued back if necessary", "pod", klog.KObj(newPod))
			return
		}
	}

	if oldPod != nil {
		oldPodInfo := newQueuedPodInfoForLookup(oldPod)
		// If the pod is already in the active queue, just update it there.
		if pInfo := p.activeQ.update(newPod, oldPodInfo); pInfo != nil {
			p.UpdateNominatedPod(logger, oldPod, pInfo.PodInfo)
			return
		}

		// If the pod is in the backoff queue, update it there.
		if pInfo, exists := p.podBackoffQ.Get(oldPodInfo); exists {
			_ = pInfo.Update(newPod)
			p.UpdateNominatedPod(logger, oldPod, pInfo.PodInfo)
			p.podBackoffQ.AddOrUpdate(pInfo)
			return
		}
	}

	// If the pod is in the unschedulable queue, updating it may make it schedulable.
	if pInfo := p.unschedulablePods.get(newPod); pInfo != nil {
		_ = pInfo.Update(newPod)
		p.UpdateNominatedPod(logger, oldPod, pInfo.PodInfo)
		gated := pInfo.Gated
		if p.isSchedulingQueueHintEnabled {
			// When unscheduled Pods are updated, we check with QueueingHint
			// whether the update may make the pods schedulable.
			// Plugins have to implement a QueueingHint for Pod/Update event
			// if the rejection from them could be resolved by updating unscheduled Pods itself.
			for _, evt := range events {
				hint := p.isPodWorthRequeuing(logger, pInfo, evt, oldPod, newPod)
				queue := p.requeuePodWithQueueingStrategy(logger, pInfo, hint, evt.Label())
				if queue != unschedulablePods {
					logger.V(5).Info("Pod moved to an internal scheduling queue because the Pod is updated", "pod", klog.KObj(newPod), "event", evt.Label(), "queue", queue)
					p.unschedulablePods.delete(pInfo.Pod, gated)
				}
				if queue == activeQ {
					p.activeQ.broadcast()
					break
				}
			}
			return
		}
		if isPodUpdated(oldPod, newPod) {
			if p.isPodBackingoff(pInfo) {
				p.podBackoffQ.AddOrUpdate(pInfo)
				p.unschedulablePods.delete(pInfo.Pod, gated)
				logger.V(5).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pInfo.Pod), "event", framework.EventUnscheduledPodUpdate.Label(), "queue", backoffQ)
				return
			}

			if added := p.moveToActiveQ(logger, pInfo, framework.BackoffComplete); added {
				p.activeQ.broadcast()
			}
			return
		}

		// Pod update didn't make it schedulable, keep it in the unschedulable queue.
		p.unschedulablePods.addOrUpdate(pInfo)
		return
	}
	// If pod is not in any of the queues, we put it in the active queue.
	pInfo := p.newQueuedPodInfo(newPod)
	if added := p.moveToActiveQ(logger, pInfo, framework.EventUnscheduledPodUpdate.Label()); added {
		p.activeQ.broadcast()
	}
}

// Delete deletes the item from either of the two queues. It assumes the pod is
// only in one queue.
func (p *PriorityQueue) Delete(pod *v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.DeleteNominatedPodIfExists(pod)
	pInfo := newQueuedPodInfoForLookup(pod)
	if err := p.activeQ.delete(pInfo); err != nil {
		// The item was probably not found in the activeQ.
		p.podBackoffQ.Delete(pInfo)
		if pInfo = p.unschedulablePods.get(pod); pInfo != nil {
			p.unschedulablePods.delete(pod, pInfo.Gated)
		}
	}
}

// AssignedPodAdded is called when a bound pod is added. Creation of this pod
// may make pending pods with matching affinity terms schedulable.
func (p *PriorityQueue) AssignedPodAdded(logger klog.Logger, pod *v1.Pod) {
	p.lock.Lock()

	// Pre-filter Pods to move by getUnschedulablePodsWithCrossTopologyTerm
	// because Pod related events shouldn't make Pods that rejected by single-node scheduling requirement schedulable.
	p.movePodsToActiveOrBackoffQueue(logger, p.getUnschedulablePodsWithCrossTopologyTerm(logger, pod), framework.EventAssignedPodAdd, nil, pod)
	p.lock.Unlock()
}

// AssignedPodUpdated is called when a bound pod is updated. Change of labels
// may make pending pods with matching affinity terms schedulable.
func (p *PriorityQueue) AssignedPodUpdated(logger klog.Logger, oldPod, newPod *v1.Pod, event framework.ClusterEvent) {
	p.lock.Lock()
	if (framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodScaleDown}.Match(event)) {
		// In this case, we don't want to pre-filter Pods by getUnschedulablePodsWithCrossTopologyTerm
		// because Pod related events may make Pods that were rejected by NodeResourceFit schedulable.
		p.moveAllToActiveOrBackoffQueue(logger, event, oldPod, newPod, nil)
	} else {
		// Pre-filter Pods to move by getUnschedulablePodsWithCrossTopologyTerm
		// because Pod related events only make Pods rejected by cross topology term schedulable.
		p.movePodsToActiveOrBackoffQueue(logger, p.getUnschedulablePodsWithCrossTopologyTerm(logger, newPod), event, oldPod, newPod)
	}
	p.lock.Unlock()
}

// NOTE: this function assumes a lock has been acquired in the caller.
// moveAllToActiveOrBackoffQueue moves all pods from unschedulablePods to activeQ or backoffQ.
// This function adds all pods and then signals the condition variable to ensure that
// if Pop() is waiting for an item, it receives the signal after all the pods are in the
// queue and the head is the highest priority pod.
func (p *PriorityQueue) moveAllToActiveOrBackoffQueue(logger klog.Logger, event framework.ClusterEvent, oldObj, newObj interface{}, preCheck PreEnqueueCheck) {
	if !p.isEventOfInterest(logger, event) {
		// No plugin is interested in this event.
		// Return early before iterating all pods in unschedulablePods for preCheck.
		return
	}

	unschedulablePods := make([]*framework.QueuedPodInfo, 0, len(p.unschedulablePods.podInfoMap))
	for _, pInfo := range p.unschedulablePods.podInfoMap {
		if preCheck == nil || preCheck(pInfo.Pod) {
			unschedulablePods = append(unschedulablePods, pInfo)
		}
	}
	p.movePodsToActiveOrBackoffQueue(logger, unschedulablePods, event, oldObj, newObj)
}

// MoveAllToActiveOrBackoffQueue moves all pods from unschedulablePods to activeQ or backoffQ.
// This function adds all pods and then signals the condition variable to ensure that
// if Pop() is waiting for an item, it receives the signal after all the pods are in the
// queue and the head is the highest priority pod.
func (p *PriorityQueue) MoveAllToActiveOrBackoffQueue(logger klog.Logger, event framework.ClusterEvent, oldObj, newObj interface{}, preCheck PreEnqueueCheck) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.moveAllToActiveOrBackoffQueue(logger, event, oldObj, newObj, preCheck)
}

// requeuePodWithQueueingStrategy tries to requeue Pod to activeQ, backoffQ or unschedulable pod pool based on schedulingHint.
// It returns the queue name Pod goes.
//
// NOTE: this function assumes lock has been acquired in caller
func (p *PriorityQueue) requeuePodWithQueueingStrategy(logger klog.Logger, pInfo *framework.QueuedPodInfo, strategy queueingStrategy, event string) string {
	if strategy == queueSkip {
		p.unschedulablePods.addOrUpdate(pInfo)
		metrics.SchedulerQueueIncomingPods.WithLabelValues("unschedulable", event).Inc()
		return unschedulablePods
	}

	if strategy == queueAfterBackoff && p.isPodBackingoff(pInfo) {
		p.podBackoffQ.AddOrUpdate(pInfo)
		metrics.SchedulerQueueIncomingPods.WithLabelValues("backoff", event).Inc()
		return backoffQ
	}

	// Reach here if schedulingHint is QueueImmediately, or schedulingHint is Queue but the pod is not backing off.
	if added := p.moveToActiveQ(logger, pInfo, event); added {
		return activeQ
	}
	if pInfo.Gated {
		// In case the pod is gated, the Pod is pushed back to unschedulable Pods pool in moveToActiveQ.
		return unschedulablePods
	}

	p.unschedulablePods.addOrUpdate(pInfo)
	metrics.SchedulerQueueIncomingPods.WithLabelValues("unschedulable", framework.ScheduleAttemptFailure).Inc()
	return unschedulablePods
}

// NOTE: this function assumes lock has been acquired in caller
func (p *PriorityQueue) movePodsToActiveOrBackoffQueue(logger klog.Logger, podInfoList []*framework.QueuedPodInfo, event framework.ClusterEvent, oldObj, newObj interface{}) {
	if !p.isEventOfInterest(logger, event) {
		// No plugin is interested in this event.
		return
	}

	activated := false
	for _, pInfo := range podInfoList {
		// When handling events takes time, a scheduling throughput gets impacted negatively
		// because of a shared lock within PriorityQueue, which Pop() also requires.
		//
		// Scheduling-gated Pods never get schedulable with any events,
		// except the Pods themselves got updated, which isn't handled by movePodsToActiveOrBackoffQueue.
		// So, we can skip them early here so that they don't go through isPodWorthRequeuing,
		// which isn't fast enough to keep a sufficient scheduling throughput
		// when the number of scheduling-gated Pods in unschedulablePods is large.
		// https://github.com/kubernetes/kubernetes/issues/124384
		// This is a hotfix for this issue, which might be changed
		// once we have a better general solution for the shared lock issue.
		//
		// Note that we cannot skip all pInfo.Gated Pods here
		// because PreEnqueue plugins apart from the scheduling gate plugin may change the gating status
		// with these events.
		if pInfo.Gated && pInfo.UnschedulablePlugins.Has(names.SchedulingGates) {
			continue
		}

		schedulingHint := p.isPodWorthRequeuing(logger, pInfo, event, oldObj, newObj)
		if schedulingHint == queueSkip {
			// QueueingHintFn determined that this Pod isn't worth putting to activeQ or backoffQ by this event.
			logger.V(5).Info("Event is not making pod schedulable", "pod", klog.KObj(pInfo.Pod), "event", event.Label())
			continue
		}

		p.unschedulablePods.delete(pInfo.Pod, pInfo.Gated)
		queue := p.requeuePodWithQueueingStrategy(logger, pInfo, schedulingHint, event.Label())
		logger.V(4).Info("Pod moved to an internal scheduling queue", "pod", klog.KObj(pInfo.Pod), "event", event.Label(), "queue", queue, "hint", schedulingHint)
		if queue == activeQ {
			activated = true
		}
	}

	p.moveRequestCycle = p.activeQ.schedulingCycle()

	if p.isSchedulingQueueHintEnabled {
		// AddUnschedulableIfNotPresent might get called for in-flight Pods later, and in
		// AddUnschedulableIfNotPresent we need to know whether events were
		// observed while scheduling them.
		if added := p.activeQ.addEventIfAnyInFlight(oldObj, newObj, event); added {
			logger.V(5).Info("Event received while pods are in flight", "event", event.Label())
		}
	}

	if activated {
		p.activeQ.broadcast()
	}
}

// getUnschedulablePodsWithCrossTopologyTerm returns unschedulable pods which either of following conditions is met:
// - have any affinity term that matches "pod".
// - rejected by PodTopologySpread plugin.
// NOTE: this function assumes lock has been acquired in caller.
func (p *PriorityQueue) getUnschedulablePodsWithCrossTopologyTerm(logger klog.Logger, pod *v1.Pod) []*framework.QueuedPodInfo {
	nsLabels := interpodaffinity.GetNamespaceLabelsSnapshot(logger, pod.Namespace, p.nsLister)

	var podsToMove []*framework.QueuedPodInfo
	for _, pInfo := range p.unschedulablePods.podInfoMap {
		if pInfo.UnschedulablePlugins.Has(podtopologyspread.Name) && pod.Namespace == pInfo.Pod.Namespace {
			// This Pod may be schedulable now by this Pod event.
			podsToMove = append(podsToMove, pInfo)
			continue
		}

		for _, term := range pInfo.RequiredAffinityTerms {
			if term.Matches(pod, nsLabels) {
				podsToMove = append(podsToMove, pInfo)
				break
			}
		}
	}

	return podsToMove
}

// PodsInActiveQ returns all the Pods in the activeQ.
func (p *PriorityQueue) PodsInActiveQ() []*v1.Pod {
	return p.activeQ.list()
}

var pendingPodsSummary = "activeQ:%v; backoffQ:%v; unschedulablePods:%v"

// GetPod searches for a pod in the activeQ, backoffQ, and unschedulablePods.
func (p *PriorityQueue) GetPod(name, namespace string) (pInfo *framework.QueuedPodInfo, ok bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	pInfoLookup := &framework.QueuedPodInfo{
		PodInfo: &framework.PodInfo{
			Pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
			},
		},
	}
	if pInfo, ok = p.podBackoffQ.Get(pInfoLookup); ok {
		return pInfo, true
	}
	if pInfo = p.unschedulablePods.get(pInfoLookup.Pod); pInfo != nil {
		return pInfo, true
	}

	p.activeQ.underRLock(func(unlockedActiveQ unlockedActiveQueueReader) {
		pInfo, ok = unlockedActiveQ.Get(pInfoLookup)
	})
	return
}

// PendingPods returns all the pending pods in the queue; accompanied by a debugging string
// recording showing the number of pods in each queue respectively.
// This function is used for debugging purposes in the scheduler cache dumper and comparer.
func (p *PriorityQueue) PendingPods() ([]*v1.Pod, string) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	result := p.activeQ.list()
	activeQLen := len(result)
	for _, pInfo := range p.podBackoffQ.List() {
		result = append(result, pInfo.Pod)
	}
	for _, pInfo := range p.unschedulablePods.podInfoMap {
		result = append(result, pInfo.Pod)
	}
	return result, fmt.Sprintf(pendingPodsSummary, activeQLen, p.podBackoffQ.Len(), len(p.unschedulablePods.podInfoMap))
}

// Note: this function assumes the caller locks both p.lock.RLock and p.activeQ.getLock().RLock.
func (p *PriorityQueue) nominatedPodToInfo(np podRef, unlockedActiveQ unlockedActiveQueueReader) *framework.PodInfo {
	pod := np.toPod()
	pInfoLookup := newQueuedPodInfoForLookup(pod)

	queuedPodInfo, exists := unlockedActiveQ.Get(pInfoLookup)
	if exists {
		return queuedPodInfo.PodInfo
	}

	queuedPodInfo = p.unschedulablePods.get(pod)
	if queuedPodInfo != nil {
		return queuedPodInfo.PodInfo
	}

	queuedPodInfo, exists = p.podBackoffQ.Get(pInfoLookup)
	if exists {
		return queuedPodInfo.PodInfo
	}

	return &framework.PodInfo{Pod: pod}
}

// Close closes the priority queue.
func (p *PriorityQueue) Close() {
	p.lock.Lock()
	defer p.lock.Unlock()
	close(p.stop)
	p.activeQ.close()
	p.activeQ.broadcast()
}

// NominatedPodsForNode returns a copy of pods that are nominated to run on the given node,
// but they are waiting for other pods to be removed from the node.
// CAUTION: Make sure you don't call this function while taking any queue's lock in any scenario.
func (p *PriorityQueue) NominatedPodsForNode(nodeName string) []*framework.PodInfo {
	p.lock.RLock()
	defer p.lock.RUnlock()
	nominatedPods := p.nominator.nominatedPodsForNode(nodeName)

	pods := make([]*framework.PodInfo, len(nominatedPods))
	p.activeQ.underRLock(func(unlockedActiveQ unlockedActiveQueueReader) {
		for i, np := range nominatedPods {
			pods[i] = p.nominatedPodToInfo(np, unlockedActiveQ).DeepCopy()
		}
	})
	return pods
}

func (p *PriorityQueue) podsCompareBackoffCompleted(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
	bo1 := p.getBackoffTime(pInfo1)
	bo2 := p.getBackoffTime(pInfo2)
	return bo1.Before(bo2)
}

// newQueuedPodInfo builds a QueuedPodInfo object.
func (p *PriorityQueue) newQueuedPodInfo(pod *v1.Pod, plugins ...string) *framework.QueuedPodInfo {
	now := p.clock.Now()
	// ignore this err since apiserver doesn't properly validate affinity terms
	// and we can't fix the validation for backwards compatibility.
	podInfo, _ := framework.NewPodInfo(pod)
	return &framework.QueuedPodInfo{
		PodInfo:                 podInfo,
		Timestamp:               now,
		InitialAttemptTimestamp: nil,
		UnschedulablePlugins:    sets.New(plugins...),
	}
}

// getBackoffTime returns the time that podInfo completes backoff
func (p *PriorityQueue) getBackoffTime(podInfo *framework.QueuedPodInfo) time.Time {
	duration := p.calculateBackoffDuration(podInfo)
	backoffTime := podInfo.Timestamp.Add(duration)
	return backoffTime
}

// calculateBackoffDuration is a helper function for calculating the backoffDuration
// based on the number of attempts the pod has made.
func (p *PriorityQueue) calculateBackoffDuration(podInfo *framework.QueuedPodInfo) time.Duration {
	if podInfo.UnschedulableCount == 0 && podInfo.ConsecutiveErrorsCount == 0 {
		// When the Pod hasn't experienced any scheduling attempts,
		// they don't have to get a backoff.
		return 0
	}

	duration := p.podInitialBackoffDuration
	count := podInfo.UnschedulableCount
	if podInfo.ConsecutiveErrorsCount > 0 {
		// This Pod has experienced an error status at the last scheduling cycle,
		// and we should consider the error count for the backoff duration.
		count = podInfo.ConsecutiveErrorsCount
	}

	for i := 1; i < count; i++ {
		// Use subtraction instead of addition or multiplication to avoid overflow.
		if duration > p.podMaxBackoffDuration-duration {
			return p.podMaxBackoffDuration
		}
		duration += duration
	}
	return duration
}

// UnschedulablePods holds pods that cannot be scheduled. This data structure
// is used to implement unschedulablePods.
type UnschedulablePods struct {
	// podInfoMap is a map key by a pod's full-name and the value is a pointer to the QueuedPodInfo.
	podInfoMap map[string]*framework.QueuedPodInfo
	keyFunc    func(*v1.Pod) string
	// unschedulableRecorder/gatedRecorder updates the counter when elements of an unschedulablePodsMap
	// get added or removed, and it does nothing if it's nil.
	unschedulableRecorder, gatedRecorder metrics.MetricRecorder
}

// addOrUpdate adds a pod to the unschedulable podInfoMap.
func (u *UnschedulablePods) addOrUpdate(pInfo *framework.QueuedPodInfo) {
	podID := u.keyFunc(pInfo.Pod)
	if _, exists := u.podInfoMap[podID]; !exists {
		if pInfo.Gated && u.gatedRecorder != nil {
			u.gatedRecorder.Inc()
		} else if !pInfo.Gated && u.unschedulableRecorder != nil {
			u.unschedulableRecorder.Inc()
		}
	}
	u.podInfoMap[podID] = pInfo
}

// delete deletes a pod from the unschedulable podInfoMap.
// The `gated` parameter is used to figure out which metric should be decreased.
func (u *UnschedulablePods) delete(pod *v1.Pod, gated bool) {
	podID := u.keyFunc(pod)
	if _, exists := u.podInfoMap[podID]; exists {
		if gated && u.gatedRecorder != nil {
			u.gatedRecorder.Dec()
		} else if !gated && u.unschedulableRecorder != nil {
			u.unschedulableRecorder.Dec()
		}
	}
	delete(u.podInfoMap, podID)
}

// get returns the QueuedPodInfo if a pod with the same key as the key of the given "pod"
// is found in the map. It returns nil otherwise.
func (u *UnschedulablePods) get(pod *v1.Pod) *framework.QueuedPodInfo {
	podKey := u.keyFunc(pod)
	if pInfo, exists := u.podInfoMap[podKey]; exists {
		return pInfo
	}
	return nil
}

// clear removes all the entries from the unschedulable podInfoMap.
func (u *UnschedulablePods) clear() {
	u.podInfoMap = make(map[string]*framework.QueuedPodInfo)
	if u.unschedulableRecorder != nil {
		u.unschedulableRecorder.Clear()
	}
	if u.gatedRecorder != nil {
		u.gatedRecorder.Clear()
	}
}

// newUnschedulablePods initializes a new object of UnschedulablePods.
func newUnschedulablePods(unschedulableRecorder, gatedRecorder metrics.MetricRecorder) *UnschedulablePods {
	return &UnschedulablePods{
		podInfoMap:            make(map[string]*framework.QueuedPodInfo),
		keyFunc:               util.GetPodFullName,
		unschedulableRecorder: unschedulableRecorder,
		gatedRecorder:         gatedRecorder,
	}
}

func podInfoKeyFunc(pInfo *framework.QueuedPodInfo) string {
	return cache.NewObjectName(pInfo.Pod.Namespace, pInfo.Pod.Name).String()
}
