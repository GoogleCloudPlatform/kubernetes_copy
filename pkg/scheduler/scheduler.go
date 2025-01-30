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

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	configv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/scheme"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	cachedebugger "k8s.io/kubernetes/pkg/scheduler/backend/cache/debugger"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	frameworkplugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/dynamicresources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
	"k8s.io/utils/clock"
)

const (
	// Duration the scheduler will wait before expiring an assumed pod.
	// See issue #106361 for more details about this parameter and its value.
	durationToExpireAssumedPod time.Duration = 0
)

// ErrNoNodesAvailable is used to describe the error that no nodes available to schedule pods.
var ErrNoNodesAvailable = fmt.Errorf("no nodes available to schedule pods")

// Scheduler watches for new unscheduled pods. It attempts to find
// nodes that they fit on and writes bindings back to the api server.
type Scheduler struct {
	// It is expected that changes made via Cache will be observed
	// by NodeLister and Algorithm.
	Cache internalcache.Cache

	Extenders []framework.Extender

	// NextPod should be a function that blocks until the next pod
	// is available. We don't use a channel for this, because scheduling
	// a pod may take some amount of time and we don't want pods to get
	// stale while they sit in a channel.
	NextPod func(logger klog.Logger) (*framework.QueuedPodInfo, error)

	// FailureHandler is called upon a scheduling failure.
	FailureHandler FailureHandlerFn

	// SchedulePod tries to schedule the given pod to one of the nodes in the node list.
	// Return a struct of ScheduleResult with the name of suggested host on success,
	// otherwise will return a FitError with reasons.
	SchedulePod func(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (ScheduleResult, error)

	// Close this to shut down the scheduler.
	StopEverything <-chan struct{}

	// SchedulingQueue holds pods to be scheduled
	SchedulingQueue internalqueue.SchedulingQueue

	// Profiles are the scheduling profiles.
	Profiles profile.Map

	client clientset.Interface

	nodeInfoSnapshot *internalcache.Snapshot

	percentageOfNodesToScore int32

	nextStartNodeIndex int

	// logger *must* be initialized when creating a Scheduler,
	// otherwise logging functions will access a nil sink and
	// panic.
	logger klog.Logger

	// registeredHandlers contains the registrations of all handlers. It's used to check if all handlers have finished syncing before the scheduling cycles start.
	registeredHandlers []cache.ResourceEventHandlerRegistration
}

func (sched *Scheduler) applyDefaultHandlers() {
	sched.SchedulePod = sched.schedulePod
	sched.FailureHandler = sched.handleSchedulingFailure
}

type schedulerOptions struct {
	clock                  clock.Clock
	componentConfigVersion string
	kubeConfig             *restclient.Config
	// Overridden by profile level percentageOfNodesToScore if set in v1.
	percentageOfNodesToScore          int32
	podInitialBackoffSeconds          int64
	podMaxBackoffSeconds              int64
	podMaxInUnschedulablePodsDuration time.Duration
	// Contains out-of-tree plugins to be merged with the in-tree registry.
	frameworkOutOfTreeRegistry frameworkruntime.Registry
	profiles                   []schedulerapi.KubeSchedulerProfile
	extenders                  []schedulerapi.Extender
	frameworkCapturer          FrameworkCapturer
	parallelism                int32
	applyDefaultProfile        bool
}

// Option configures a Scheduler
type Option func(*schedulerOptions)

// ScheduleResult represents the result of scheduling a pod.
type ScheduleResult struct {
	// Name of the selected node.
	SuggestedHost string
	// The number of nodes the scheduler evaluated the pod against in the filtering
	// phase and beyond.
	EvaluatedNodes int
	// The number of nodes out of the evaluated ones that fit the pod.
	FeasibleNodes int
	// The nominating info for scheduling cycle.
	nominatingInfo *framework.NominatingInfo
}

// WithComponentConfigVersion sets the component config version to the
// KubeSchedulerConfiguration version used. The string should be the full
// scheme group/version of the external type we converted from (for example
// "kubescheduler.config.k8s.io/v1")
func WithComponentConfigVersion(apiVersion string) Option {
	return func(o *schedulerOptions) {
		o.componentConfigVersion = apiVersion
	}
}

// WithKubeConfig sets the kube config for Scheduler.
func WithKubeConfig(cfg *restclient.Config) Option {
	return func(o *schedulerOptions) {
		o.kubeConfig = cfg
	}
}

// WithProfiles sets profiles for Scheduler. By default, there is one profile
// with the name "default-scheduler".
func WithProfiles(p ...schedulerapi.KubeSchedulerProfile) Option {
	return func(o *schedulerOptions) {
		o.profiles = p
		o.applyDefaultProfile = false
	}
}

// WithParallelism sets the parallelism for all scheduler algorithms. Default is 16.
func WithParallelism(threads int32) Option {
	return func(o *schedulerOptions) {
		o.parallelism = threads
	}
}

// WithPercentageOfNodesToScore sets percentageOfNodesToScore for Scheduler.
// The default value of 0 will use an adaptive percentage: 50 - (num of nodes)/125.
func WithPercentageOfNodesToScore(percentageOfNodesToScore *int32) Option {
	return func(o *schedulerOptions) {
		if percentageOfNodesToScore != nil {
			o.percentageOfNodesToScore = *percentageOfNodesToScore
		}
	}
}

// WithFrameworkOutOfTreeRegistry sets the registry for out-of-tree plugins. Those plugins
// will be appended to the default registry.
func WithFrameworkOutOfTreeRegistry(registry frameworkruntime.Registry) Option {
	return func(o *schedulerOptions) {
		o.frameworkOutOfTreeRegistry = registry
	}
}

// WithPodInitialBackoffSeconds sets podInitialBackoffSeconds for Scheduler, the default value is 1
func WithPodInitialBackoffSeconds(podInitialBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podInitialBackoffSeconds = podInitialBackoffSeconds
	}
}

// WithPodMaxBackoffSeconds sets podMaxBackoffSeconds for Scheduler, the default value is 10
func WithPodMaxBackoffSeconds(podMaxBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podMaxBackoffSeconds = podMaxBackoffSeconds
	}
}

// WithPodMaxInUnschedulablePodsDuration sets podMaxInUnschedulablePodsDuration for PriorityQueue.
func WithPodMaxInUnschedulablePodsDuration(duration time.Duration) Option {
	return func(o *schedulerOptions) {
		o.podMaxInUnschedulablePodsDuration = duration
	}
}

// WithExtenders sets extenders for the Scheduler
func WithExtenders(e ...schedulerapi.Extender) Option {
	return func(o *schedulerOptions) {
		o.extenders = e
	}
}

// WithClock sets clock for PriorityQueue, the default clock is clock.RealClock.
func WithClock(clock clock.Clock) Option {
	return func(o *schedulerOptions) {
		o.clock = clock
	}
}

// FrameworkCapturer is used for registering a notify function in building framework.
type FrameworkCapturer func(schedulerapi.KubeSchedulerProfile)

// WithBuildFrameworkCapturer sets a notify function for getting buildFramework details.
func WithBuildFrameworkCapturer(fc FrameworkCapturer) Option {
	return func(o *schedulerOptions) {
		o.frameworkCapturer = fc
	}
}

var defaultSchedulerOptions = schedulerOptions{
	clock:                             clock.RealClock{},
	percentageOfNodesToScore:          schedulerapi.DefaultPercentageOfNodesToScore,
	podInitialBackoffSeconds:          int64(internalqueue.DefaultPodInitialBackoffDuration.Seconds()),
	podMaxBackoffSeconds:              int64(internalqueue.DefaultPodMaxBackoffDuration.Seconds()),
	podMaxInUnschedulablePodsDuration: internalqueue.DefaultPodMaxInUnschedulablePodsDuration,
	parallelism:                       int32(parallelize.DefaultParallelism),
	// Ideally we would statically set the default profile here, but we can't because
	// creating the default profile may require testing feature gates, which may get
	// set dynamically in tests. Therefore, we delay creating it until New is actually
	// invoked.
	applyDefaultProfile: true,
}

// New returns a Scheduler
func New(ctx context.Context,
	client clientset.Interface,
	informerFactory informers.SharedInformerFactory,
	dynInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	recorderFactory profile.RecorderFactory,
	opts ...Option) (*Scheduler, error) {

	logger := klog.FromContext(ctx)
	stopEverything := ctx.Done()

	options := defaultSchedulerOptions
	for _, opt := range opts {
		opt(&options)
	}

	if options.applyDefaultProfile {
		var versionedCfg configv1.KubeSchedulerConfiguration
		scheme.Scheme.Default(&versionedCfg)
		cfg := schedulerapi.KubeSchedulerConfiguration{}
		if err := scheme.Scheme.Convert(&versionedCfg, &cfg, nil); err != nil {
			return nil, err
		}
		options.profiles = cfg.Profiles
	}

	registry := frameworkplugins.NewInTreeRegistry()
	if err := registry.Merge(options.frameworkOutOfTreeRegistry); err != nil {
		return nil, err
	}

	metrics.Register()

	extenders, err := buildExtenders(logger, options.extenders, options.profiles)
	if err != nil {
		return nil, fmt.Errorf("couldn't build extenders: %w", err)
	}

	podLister := informerFactory.Core().V1().Pods().Lister()
	nodeLister := informerFactory.Core().V1().Nodes().Lister()

	snapshot := internalcache.NewEmptySnapshot()
	metricsRecorder := metrics.NewMetricsAsyncRecorder(1000, time.Second, stopEverything)
	// waitingPods holds all the pods that are in the scheduler and waiting in the permit stage
	waitingPods := frameworkruntime.NewWaitingPodsMap()

	var resourceClaimCache *assumecache.AssumeCache
	var draManager framework.SharedDRAManager
	if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
		resourceClaimInformer := informerFactory.Resource().V1beta1().ResourceClaims().Informer()
		resourceClaimCache = assumecache.NewAssumeCache(logger, resourceClaimInformer, "ResourceClaim", "", nil)
		draManager = dynamicresources.NewDRAManager(ctx, resourceClaimCache, informerFactory)
	}

	profiles, err := profile.NewMap(ctx, options.profiles, registry, recorderFactory,
		frameworkruntime.WithComponentConfigVersion(options.componentConfigVersion),
		frameworkruntime.WithClientSet(client),
		frameworkruntime.WithKubeConfig(options.kubeConfig),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSharedDRAManager(draManager),
		frameworkruntime.WithSnapshotSharedLister(snapshot),
		frameworkruntime.WithCaptureProfile(frameworkruntime.CaptureProfile(options.frameworkCapturer)),
		frameworkruntime.WithParallelism(int(options.parallelism)),
		frameworkruntime.WithExtenders(extenders),
		frameworkruntime.WithMetricsRecorder(metricsRecorder),
		frameworkruntime.WithWaitingPods(waitingPods),
	)
	if err != nil {
		return nil, fmt.Errorf("initializing profiles: %v", err)
	}

	if len(profiles) == 0 {
		return nil, errors.New("at least one profile is required")
	}

	preEnqueuePluginMap := make(map[string][]framework.PreEnqueuePlugin)
	queueingHintsPerProfile := make(internalqueue.QueueingHintMapPerProfile)
	var returnErr error
	for profileName, profile := range profiles {
		preEnqueuePluginMap[profileName] = profile.PreEnqueuePlugins()
		queueingHintsPerProfile[profileName], err = buildQueueingHintMap(ctx, profile.EnqueueExtensions())
		if err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}

	if returnErr != nil {
		return nil, returnErr
	}

	podQueue := internalqueue.NewSchedulingQueue(
		profiles[options.profiles[0].SchedulerName].QueueSortFunc(),
		informerFactory,
		internalqueue.WithClock(options.clock),
		internalqueue.WithPodInitialBackoffDuration(time.Duration(options.podInitialBackoffSeconds)*time.Second),
		internalqueue.WithPodMaxBackoffDuration(time.Duration(options.podMaxBackoffSeconds)*time.Second),
		internalqueue.WithPodLister(podLister),
		internalqueue.WithPodMaxInUnschedulablePodsDuration(options.podMaxInUnschedulablePodsDuration),
		internalqueue.WithPreEnqueuePluginMap(preEnqueuePluginMap),
		internalqueue.WithQueueingHintMapPerProfile(queueingHintsPerProfile),
		internalqueue.WithPluginMetricsSamplePercent(pluginMetricsSamplePercent),
		internalqueue.WithMetricsRecorder(*metricsRecorder),
	)

	for _, fwk := range profiles {
		fwk.SetPodNominator(podQueue)
		fwk.SetPodActivator(podQueue)
	}

	schedulerCache := internalcache.New(ctx, durationToExpireAssumedPod)

	// Setup cache debugger.
	debugger := cachedebugger.New(nodeLister, podLister, schedulerCache, podQueue)
	debugger.ListenForSignal(ctx)

	sched := &Scheduler{
		Cache:                    schedulerCache,
		client:                   client,
		nodeInfoSnapshot:         snapshot,
		percentageOfNodesToScore: options.percentageOfNodesToScore,
		Extenders:                extenders,
		StopEverything:           stopEverything,
		SchedulingQueue:          podQueue,
		Profiles:                 profiles,
		logger:                   logger,
	}
	sched.NextPod = podQueue.Pop
	sched.applyDefaultHandlers()

	if err = addAllEventHandlers(sched, informerFactory, dynInformerFactory, resourceClaimCache, unionedGVKs(queueingHintsPerProfile)); err != nil {
		return nil, fmt.Errorf("adding event handlers: %w", err)
	}

	return sched, nil
}

// defaultQueueingHintFn is the default queueing hint function.
// It always returns Queue as the queueing hint.
var defaultQueueingHintFn = func(_ klog.Logger, _ *v1.Pod, _, _ interface{}) (framework.QueueingHint, error) {
	return framework.Queue, nil
}

func buildQueueingHintMap(ctx context.Context, es []framework.EnqueueExtensions) (internalqueue.QueueingHintMap, error) {
	queueingHintMap := make(internalqueue.QueueingHintMap)
	var returnErr error
	for _, e := range es {
		events, err := e.EventsToRegister(ctx)
		if err != nil {
			returnErr = errors.Join(returnErr, err)
		}

		// This will happen when plugin registers with empty events, it's usually the case a pod
		// will become reschedulable only for self-update, e.g. schedulingGates plugin, the pod
		// will enter into the activeQ via priorityQueue.Update().
		if len(events) == 0 {
			continue
		}

		// Note: Rarely, a plugin implements EnqueueExtensions but returns nil.
		// We treat it as: the plugin is not interested in any event, and hence pod failed by that plugin
		// cannot be moved by any regular cluster event.
		// So, we can just ignore such EventsToRegister here.

		registerNodeAdded := false
		registerNodeTaintUpdated := false
		for _, event := range events {
			fn := event.QueueingHintFn
			if fn == nil || !utilfeature.DefaultFeatureGate.Enabled(features.SchedulerQueueingHints) {
				fn = defaultQueueingHintFn
			}

			if event.Event.Resource == framework.Node {
				if event.Event.ActionType&framework.Add != 0 {
					registerNodeAdded = true
				}
				if event.Event.ActionType&framework.UpdateNodeTaint != 0 {
					registerNodeTaintUpdated = true
				}
			}

			queueingHintMap[event.Event] = append(queueingHintMap[event.Event], &internalqueue.QueueingHintFunction{
				PluginName:     e.Name(),
				QueueingHintFn: fn,
			})
		}
		if registerNodeAdded && !registerNodeTaintUpdated {
			// Temporally fix for the issue https://github.com/kubernetes/kubernetes/issues/109437
			// NodeAdded QueueingHint isn't always called because of preCheck.
			// It's definitely not something expected for plugin developers,
			// and registering UpdateNodeTaint event is the only mitigation for now.
			//
			// So, here registers UpdateNodeTaint event for plugins that has NodeAdded event, but don't have UpdateNodeTaint event.
			// It has a bad impact for the requeuing efficiency though, a lot better than some Pods being stuch in the
			// unschedulable pod pool.
			// This behavior will be removed when we remove the preCheck feature.
			// See: https://github.com/kubernetes/kubernetes/issues/110175
			queueingHintMap[framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}] =
				append(queueingHintMap[framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}],
					&internalqueue.QueueingHintFunction{
						PluginName:     e.Name(),
						QueueingHintFn: defaultQueueingHintFn,
					},
				)
		}
	}
	if returnErr != nil {
		return nil, returnErr
	}
	return queueingHintMap, nil
}

// Run begins watching and scheduling. It starts scheduling and blocked until the context is done.
func (sched *Scheduler) Run(ctx context.Context) {
	logger := klog.FromContext(ctx)
	sched.SchedulingQueue.Run(logger)

	// We need to start scheduleOne loop in a dedicated goroutine,
	// because scheduleOne function hangs on getting the next item
	// from the SchedulingQueue.
	// If there are no new pods to schedule, it will be hanging there
	// and if done in this goroutine it will be blocking closing
	// SchedulingQueue, in effect causing a deadlock on shutdown.
	go wait.UntilWithContext(ctx, sched.ScheduleOne, 0)

	<-ctx.Done()
	sched.SchedulingQueue.Close()

	// If the plugins satisfy the io.Closer interface, they are closed.
	err := sched.Profiles.Close()
	if err != nil {
		logger.Error(err, "Failed to close plugins")
	}
}

// NewInformerFactory creates a SharedInformerFactory and initializes a scheduler specific
// in-place podInformer.
func NewInformerFactory(cs clientset.Interface, resyncPeriod time.Duration) informers.SharedInformerFactory {
	informerFactory := informers.NewSharedInformerFactory(cs, resyncPeriod)
	informerFactory.InformerFor(&v1.Pod{}, newPodInformer)
	return informerFactory
}

func buildExtenders(logger klog.Logger, extenders []schedulerapi.Extender, profiles []schedulerapi.KubeSchedulerProfile) ([]framework.Extender, error) {
	var fExtenders []framework.Extender
	if len(extenders) == 0 {
		return nil, nil
	}

	var ignoredExtendedResources []string
	var ignorableExtenders []framework.Extender
	for i := range extenders {
		logger.V(2).Info("Creating extender", "extender", extenders[i])
		extender, err := NewHTTPExtender(&extenders[i])
		if err != nil {
			return nil, err
		}
		if !extender.IsIgnorable() {
			fExtenders = append(fExtenders, extender)
		} else {
			ignorableExtenders = append(ignorableExtenders, extender)
		}
		for _, r := range extenders[i].ManagedResources {
			if r.IgnoredByScheduler {
				ignoredExtendedResources = append(ignoredExtendedResources, r.Name)
			}
		}
	}
	// place ignorable extenders to the tail of extenders
	fExtenders = append(fExtenders, ignorableExtenders...)

	// If there are any extended resources found from the Extenders, append them to the pluginConfig for each profile.
	// This should only have an effect on ComponentConfig, where it is possible to configure Extenders and
	// plugin args (and in which case the extender ignored resources take precedence).
	if len(ignoredExtendedResources) == 0 {
		return fExtenders, nil
	}

	for i := range profiles {
		prof := &profiles[i]
		var found = false
		for k := range prof.PluginConfig {
			if prof.PluginConfig[k].Name == noderesources.Name {
				// Update the existing args
				pc := &prof.PluginConfig[k]
				args, ok := pc.Args.(*schedulerapi.NodeResourcesFitArgs)
				if !ok {
					return nil, fmt.Errorf("want args to be of type NodeResourcesFitArgs, got %T", pc.Args)
				}
				args.IgnoredResources = ignoredExtendedResources
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("can't find NodeResourcesFitArgs in plugin config")
		}
	}
	return fExtenders, nil
}

type FailureHandlerFn func(ctx context.Context, fwk framework.Framework, podInfo *framework.QueuedPodInfo, status *framework.Status, nominatingInfo *framework.NominatingInfo, start time.Time)

func unionedGVKs(queueingHintsPerProfile internalqueue.QueueingHintMapPerProfile) map[framework.EventResource]framework.ActionType {
	gvkMap := make(map[framework.EventResource]framework.ActionType)
	for _, queueingHints := range queueingHintsPerProfile {
		for evt := range queueingHints {
			if _, ok := gvkMap[evt.Resource]; ok {
				gvkMap[evt.Resource] |= evt.ActionType
			} else {
				gvkMap[evt.Resource] = evt.ActionType
			}
		}
	}
	return gvkMap
}

// newPodInformer creates a shared index informer that returns only non-terminal pods.
// The PodInformer allows indexers to be added, but note that only non-conflict indexers are allowed.
func newPodInformer(cs clientset.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	selector := fmt.Sprintf("status.phase!=%v,status.phase!=%v", v1.PodSucceeded, v1.PodFailed)
	tweakListOptions := func(options *metav1.ListOptions) {
		options.FieldSelector = selector
	}
	informer := coreinformers.NewFilteredPodInformer(cs, metav1.NamespaceAll, resyncPeriod, cache.Indexers{}, tweakListOptions)

	// Dropping `.metadata.managedFields` to improve memory usage.
	// The Extract workflow (i.e. `ExtractPod`) should be unused.
	trim := func(obj interface{}) (interface{}, error) {
		if accessor, err := meta.Accessor(obj); err == nil {
			if accessor.GetManagedFields() != nil {
				accessor.SetManagedFields(nil)
			}
		}
		return obj, nil
	}
	informer.SetTransform(trim)
	return informer
}
