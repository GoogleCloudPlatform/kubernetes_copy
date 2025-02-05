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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/testing/defaults"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	utiltesting "k8s.io/kubernetes/test/utils/ktesting"
)

func TestSchedulerCreation(t *testing.T) {
	invalidRegistry := map[string]frameworkruntime.PluginFactory{
		defaultbinder.Name: defaultbinder.New,
	}
	validRegistry := map[string]frameworkruntime.PluginFactory{
		"Foo": defaultbinder.New,
	}
	cases := []struct {
		name          string
		opts          []Option
		wantErr       string
		wantProfiles  []string
		wantExtenders []string
	}{
		{
			name: "valid out-of-tree registry",
			opts: []Option{
				WithFrameworkOutOfTreeRegistry(validRegistry),
				WithProfiles(
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "default-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
				)},
			wantProfiles: []string{"default-scheduler"},
		},
		{
			name: "repeated plugin name in out-of-tree plugin",
			opts: []Option{
				WithFrameworkOutOfTreeRegistry(invalidRegistry),
				WithProfiles(
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "default-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
				)},
			wantProfiles: []string{"default-scheduler"},
			wantErr:      "a plugin named DefaultBinder already exists",
		},
		{
			name: "multiple profiles",
			opts: []Option{
				WithProfiles(
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "foo",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "bar",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
				)},
			wantProfiles: []string{"bar", "foo"},
		},
		{
			name: "Repeated profiles",
			opts: []Option{
				WithProfiles(
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "foo",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "bar",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "foo",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
				)},
			wantErr: "duplicate profile with scheduler name \"foo\"",
		},
		{
			name: "With extenders",
			opts: []Option{
				WithProfiles(
					schedulerapi.KubeSchedulerProfile{
						SchedulerName: "default-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
							Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
						},
					},
				),
				WithExtenders(
					schedulerapi.Extender{
						URLPrefix: "http://extender.kube-system/",
					},
				),
			},
			wantProfiles:  []string{"default-scheduler"},
			wantExtenders: []string{"http://extender.kube-system/"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)

			eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			s, err := New(
				ctx,
				client,
				informerFactory,
				nil,
				profile.NewRecorderFactory(eventBroadcaster),
				tc.opts...,
			)

			// Errors
			if len(tc.wantErr) != 0 {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("got error %q, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Failed to create scheduler: %v", err)
			}

			// Profiles
			profiles := make([]string, 0, len(s.Profiles))
			for name := range s.Profiles {
				profiles = append(profiles, name)
			}
			sort.Strings(profiles)
			if diff := cmp.Diff(tc.wantProfiles, profiles); diff != "" {
				t.Errorf("unexpected profiles (-want, +got):\n%s", diff)
			}

			// Extenders
			if len(tc.wantExtenders) != 0 {
				// Scheduler.Extenders
				extenders := make([]string, 0, len(s.Extenders))
				for _, e := range s.Extenders {
					extenders = append(extenders, e.Name())
				}
				if diff := cmp.Diff(tc.wantExtenders, extenders); diff != "" {
					t.Errorf("unexpected extenders (-want, +got):\n%s", diff)
				}

				// framework.Handle.Extenders()
				for _, p := range s.Profiles {
					extenders := make([]string, 0, len(p.Extenders()))
					for _, e := range p.Extenders() {
						extenders = append(extenders, e.Name())
					}
					if diff := cmp.Diff(tc.wantExtenders, extenders); diff != "" {
						t.Errorf("unexpected extenders (-want, +got):\n%s", diff)
					}
				}
			}
		})
	}
}

func TestFailureHandler(t *testing.T) {
	metrics.Register()
	testPod := st.MakePod().Name("test-pod").Namespace(v1.NamespaceDefault).Obj()
	testPodUpdated := testPod.DeepCopy()
	testPodUpdated.Labels = map[string]string{"foo": ""}

	tests := []struct {
		name                       string
		podUpdatedDuringScheduling bool // pod is updated during a scheduling cycle
		podDeletedDuringScheduling bool // pod is deleted during a scheduling cycle
		expect                     *v1.Pod
	}{
		{
			name:                       "pod is updated during a scheduling cycle",
			podUpdatedDuringScheduling: true,
			expect:                     testPodUpdated,
		},
		{
			name:   "pod is not updated during a scheduling cycle",
			expect: testPod,
		},
		{
			name:                       "pod is deleted during a scheduling cycle",
			podDeletedDuringScheduling: true,
			expect:                     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			client := fake.NewClientset(&v1.PodList{Items: []v1.Pod{*testPod}})
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			podInformer := informerFactory.Core().V1().Pods()
			// Need to add/update/delete testPod to the store.
			podInformer.Informer().GetStore().Add(testPod)

			recorder := metrics.NewMetricsAsyncRecorder(3, 20*time.Microsecond, ctx.Done())
			queue := internalqueue.NewPriorityQueue(nil, informerFactory, internalqueue.WithClock(testingclock.NewFakeClock(time.Now())), internalqueue.WithMetricsRecorder(*recorder), internalqueue.WithPodMaxBackoffDuration(0), internalqueue.WithPodInitialBackoffDuration(0))
			schedulerCache := internalcache.New(ctx, 30*time.Second)

			queue.Add(logger, testPod)

			if _, err := queue.Pop(logger); err != nil {
				t.Fatalf("Pop failed: %v", err)
			}

			if tt.podUpdatedDuringScheduling {
				podInformer.Informer().GetStore().Update(testPodUpdated)
				queue.Update(logger, testPod, testPodUpdated)
			}
			if tt.podDeletedDuringScheduling {
				podInformer.Informer().GetStore().Delete(testPod)
				queue.Delete(testPod)
			}

			s, fwk, err := initScheduler(ctx, schedulerCache, queue, client, informerFactory)
			if err != nil {
				t.Fatal(err)
			}

			testPodInfo := &framework.QueuedPodInfo{PodInfo: mustNewPodInfo(t, testPod)}
			s.FailureHandler(ctx, fwk, testPodInfo, framework.NewStatus(framework.Unschedulable), nil, time.Now())

			var got *v1.Pod
			if tt.podUpdatedDuringScheduling {
				head, e := queue.Pop(logger)
				if e != nil {
					t.Fatalf("Cannot pop pod from the activeQ: %v", e)
				}
				got = head.Pod
			} else {
				got = getPodFromPriorityQueue(queue, testPod)
			}

			if diff := cmp.Diff(tt.expect, got); diff != "" {
				t.Errorf("Unexpected pod (-want, +got): %s", diff)
			}
		})
	}
}

func TestFailureHandler_PodAlreadyBound(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	nodeFoo := v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}
	testPod := st.MakePod().Name("test-pod").Namespace(v1.NamespaceDefault).Node("foo").Obj()

	client := fake.NewClientset(&v1.PodList{Items: []v1.Pod{*testPod}}, &v1.NodeList{Items: []v1.Node{nodeFoo}})
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	podInformer := informerFactory.Core().V1().Pods()
	// Need to add testPod to the store.
	podInformer.Informer().GetStore().Add(testPod)

	queue := internalqueue.NewPriorityQueue(nil, informerFactory, internalqueue.WithClock(testingclock.NewFakeClock(time.Now())))
	schedulerCache := internalcache.New(ctx, 30*time.Second)

	// Add node to schedulerCache no matter it's deleted in API server or not.
	schedulerCache.AddNode(logger, &nodeFoo)

	s, fwk, err := initScheduler(ctx, schedulerCache, queue, client, informerFactory)
	if err != nil {
		t.Fatal(err)
	}

	testPodInfo := &framework.QueuedPodInfo{PodInfo: mustNewPodInfo(t, testPod)}
	s.FailureHandler(ctx, fwk, testPodInfo, framework.NewStatus(framework.Unschedulable).WithError(fmt.Errorf("binding rejected: timeout")), nil, time.Now())

	pod := getPodFromPriorityQueue(queue, testPod)
	if pod != nil {
		t.Fatalf("Unexpected pod: %v should not be in PriorityQueue when the NodeName of pod is not empty", pod.Name)
	}
}

// TestWithPercentageOfNodesToScore tests scheduler's PercentageOfNodesToScore is set correctly.
func TestWithPercentageOfNodesToScore(t *testing.T) {
	tests := []struct {
		name                           string
		percentageOfNodesToScoreConfig *int32
		wantedPercentageOfNodesToScore int32
	}{
		{
			name:                           "percentageOfNodesScore is nil",
			percentageOfNodesToScoreConfig: nil,
			wantedPercentageOfNodesToScore: schedulerapi.DefaultPercentageOfNodesToScore,
		},
		{
			name:                           "percentageOfNodesScore is not nil",
			percentageOfNodesToScoreConfig: ptr.To[int32](10),
			wantedPercentageOfNodesToScore: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			sched, err := New(
				ctx,
				client,
				informerFactory,
				nil,
				profile.NewRecorderFactory(eventBroadcaster),
				WithPercentageOfNodesToScore(tt.percentageOfNodesToScoreConfig),
			)
			if err != nil {
				t.Fatalf("Failed to create scheduler: %v", err)
			}
			if sched.percentageOfNodesToScore != tt.wantedPercentageOfNodesToScore {
				t.Errorf("scheduler.percercentageOfNodesToScore = %v, want %v", sched.percentageOfNodesToScore, tt.wantedPercentageOfNodesToScore)
			}
		})
	}
}

// getPodFromPriorityQueue is the function used in the TestDefaultErrorFunc test to get
// the specific pod from the given priority queue. It returns the found pod in the priority queue.
func getPodFromPriorityQueue(queue *internalqueue.PriorityQueue, pod *v1.Pod) *v1.Pod {
	podList, _ := queue.PendingPods()
	if len(podList) == 0 {
		return nil
	}

	queryPodKey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		return nil
	}

	for _, foundPod := range podList {
		foundPodKey, err := cache.MetaNamespaceKeyFunc(foundPod)
		if err != nil {
			return nil
		}

		if foundPodKey == queryPodKey {
			return foundPod
		}
	}

	return nil
}

func initScheduler(ctx context.Context, cache internalcache.Cache, queue internalqueue.SchedulingQueue,
	client kubernetes.Interface, informerFactory informers.SharedInformerFactory) (*Scheduler, framework.Framework, error) {
	logger := klog.FromContext(ctx)
	registerPluginFuncs := []tf.RegisterPluginFunc{
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})
	waitingPods := frameworkruntime.NewWaitingPodsMap()
	fwk, err := tf.NewFramework(ctx,
		registerPluginFuncs,
		testSchedulerName,
		frameworkruntime.WithClientSet(client),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithEventRecorder(eventBroadcaster.NewRecorder(scheme.Scheme, testSchedulerName)),
		frameworkruntime.WithWaitingPods(waitingPods),
	)
	if err != nil {
		return nil, nil, err
	}

	s := &Scheduler{
		Cache:           cache,
		client:          client,
		StopEverything:  ctx.Done(),
		SchedulingQueue: queue,
		Profiles:        profile.Map{testSchedulerName: fwk},
		logger:          logger,
	}
	s.applyDefaultHandlers()

	return s, fwk, nil
}

func TestInitPluginsWithIndexers(t *testing.T) {
	tests := []struct {
		name string
		// the plugin registration ordering must not matter, being map traversal random
		entrypoints map[string]frameworkruntime.PluginFactory
		wantErr     string
	}{
		{
			name: "register indexer, no conflicts",
			entrypoints: map[string]frameworkruntime.PluginFactory{
				"AddIndexer": func(ctx context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
					podInformer := handle.SharedInformerFactory().Core().V1().Pods()
					err := podInformer.Informer().AddIndexers(cache.Indexers{
						"nodeName": indexByPodSpecNodeName,
					})
					return &TestPlugin{name: "AddIndexer"}, err
				},
			},
		},
		{
			name: "register the same indexer name multiple times, conflict",
			// order of registration doesn't matter
			entrypoints: map[string]frameworkruntime.PluginFactory{
				"AddIndexer1": func(ctx context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
					podInformer := handle.SharedInformerFactory().Core().V1().Pods()
					err := podInformer.Informer().AddIndexers(cache.Indexers{
						"nodeName": indexByPodSpecNodeName,
					})
					return &TestPlugin{name: "AddIndexer1"}, err
				},
				"AddIndexer2": func(ctx context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
					podInformer := handle.SharedInformerFactory().Core().V1().Pods()
					err := podInformer.Informer().AddIndexers(cache.Indexers{
						"nodeName": indexByPodAnnotationNodeName,
					})
					return &TestPlugin{name: "AddIndexer1"}, err
				},
			},
			wantErr: "indexer conflict",
		},
		{
			name: "register the same indexer body with different names, no conflicts",
			// order of registration doesn't matter
			entrypoints: map[string]frameworkruntime.PluginFactory{
				"AddIndexer1": func(ctx context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
					podInformer := handle.SharedInformerFactory().Core().V1().Pods()
					err := podInformer.Informer().AddIndexers(cache.Indexers{
						"nodeName1": indexByPodSpecNodeName,
					})
					return &TestPlugin{name: "AddIndexer1"}, err
				},
				"AddIndexer2": func(ctx context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
					podInformer := handle.SharedInformerFactory().Core().V1().Pods()
					err := podInformer.Informer().AddIndexers(cache.Indexers{
						"nodeName2": indexByPodAnnotationNodeName,
					})
					return &TestPlugin{name: "AddIndexer2"}, err
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeInformerFactory := NewInformerFactory(&fake.Clientset{}, 0*time.Second)

			var registerPluginFuncs []tf.RegisterPluginFunc
			for name, entrypoint := range tt.entrypoints {
				registerPluginFuncs = append(registerPluginFuncs,
					// anything supported by TestPlugin is fine
					tf.RegisterFilterPlugin(name, entrypoint),
				)
			}
			// we always need this
			registerPluginFuncs = append(registerPluginFuncs,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			_, err := tf.NewFramework(ctx, registerPluginFuncs, "test", frameworkruntime.WithInformerFactory(fakeInformerFactory))

			if len(tt.wantErr) > 0 {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("got error %q, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Failed to create scheduler: %v", err)
			}
		})
	}
}

func indexByPodSpecNodeName(obj interface{}) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}
	if len(pod.Spec.NodeName) == 0 {
		return []string{}, nil
	}
	return []string{pod.Spec.NodeName}, nil
}

func indexByPodAnnotationNodeName(obj interface{}) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}
	if len(pod.Annotations) == 0 {
		return []string{}, nil
	}
	nodeName, ok := pod.Annotations["node-name"]
	if !ok {
		return []string{}, nil
	}
	return []string{nodeName}, nil
}

const (
	filterWithoutEnqueueExtensions = "filterWithoutEnqueueExtensions"
	fakeNode                       = "fakeNode"
	fakePod                        = "fakePod"
	emptyEventsToRegister          = "emptyEventsToRegister"
	errorEventsToRegister          = "errorEventsToRegister"
	queueSort                      = "no-op-queue-sort-plugin"
	fakeBind                       = "bind-plugin"
	emptyEventExtensions           = "emptyEventExtensions"
	fakePermit                     = "fakePermit"
)

func Test_buildQueueingHintMap(t *testing.T) {
	tests := []struct {
		name                string
		plugins             []framework.Plugin
		want                map[framework.ClusterEvent][]*internalqueue.QueueingHintFunction
		featuregateDisabled bool
		wantErr             error
	}{
		{
			name:    "filter without EnqueueExtensions plugin",
			plugins: []framework.Plugin{&filterWithoutEnqueueExtensionsPlugin{}},
			want: map[framework.ClusterEvent][]*internalqueue.QueueingHintFunction{
				{Resource: framework.Pod, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.Node, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.CSINode, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.CSIDriver, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.CSIStorageCapacity, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.PersistentVolume, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.StorageClass, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.PersistentVolumeClaim, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.ResourceClaim, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
				{Resource: framework.DeviceClass, ActionType: framework.All}: {
					{PluginName: filterWithoutEnqueueExtensions, QueueingHintFn: defaultQueueingHintFn},
				},
			},
		},
		{
			name:    "node and pod plugin",
			plugins: []framework.Plugin{&fakeNodePlugin{}, &fakePodPlugin{}},
			want: map[framework.ClusterEvent][]*internalqueue.QueueingHintFunction{
				{Resource: framework.Pod, ActionType: framework.Add}: {
					{PluginName: fakePod, QueueingHintFn: fakePodPluginQueueingFn},
				},
				{Resource: framework.Node, ActionType: framework.Add}: {
					{PluginName: fakeNode, QueueingHintFn: fakeNodePluginQueueingFn},
				},
				{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}: {
					{PluginName: fakeNode, QueueingHintFn: defaultQueueingHintFn}, // When Node/Add is registered, Node/UpdateNodeTaint is automatically registered.
				},
			},
		},
		{
			name:                "node and pod plugin (featuregate is disabled)",
			plugins:             []framework.Plugin{&fakeNodePlugin{}, &fakePodPlugin{}},
			featuregateDisabled: true,
			want: map[framework.ClusterEvent][]*internalqueue.QueueingHintFunction{
				{Resource: framework.Pod, ActionType: framework.Add}: {
					{PluginName: fakePod, QueueingHintFn: defaultQueueingHintFn}, // default queueing hint due to disabled feature gate.
				},
				{Resource: framework.Node, ActionType: framework.Add}: {
					{PluginName: fakeNode, QueueingHintFn: defaultQueueingHintFn}, // default queueing hint due to disabled feature gate.
				},
				{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}: {
					{PluginName: fakeNode, QueueingHintFn: defaultQueueingHintFn}, // When Node/Add is registered, Node/UpdateNodeTaint is automatically registered.
				},
			},
		},
		{
			name:    "register plugin with empty event",
			plugins: []framework.Plugin{&emptyEventPlugin{}},
			want:    map[framework.ClusterEvent][]*internalqueue.QueueingHintFunction{},
		},
		{
			name:    "register plugins including emptyEventPlugin",
			plugins: []framework.Plugin{&emptyEventPlugin{}, &fakeNodePlugin{}},
			want: map[framework.ClusterEvent][]*internalqueue.QueueingHintFunction{
				{Resource: framework.Pod, ActionType: framework.Add}: {
					{PluginName: fakePod, QueueingHintFn: fakePodPluginQueueingFn},
				},
				{Resource: framework.Node, ActionType: framework.Add}: {
					{PluginName: fakeNode, QueueingHintFn: fakeNodePluginQueueingFn},
				},
				{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}: {
					{PluginName: fakeNode, QueueingHintFn: defaultQueueingHintFn}, // When Node/Add is registered, Node/UpdateNodeTaint is automatically registered.
				},
			},
		},
		{
			name:    "one EventsToRegister returns an error",
			plugins: []framework.Plugin{&errorEventsToRegisterPlugin{}},
			want:    map[framework.ClusterEvent][]*internalqueue.QueueingHintFunction{},
			wantErr: errors.New("mock error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, !tt.featuregateDisabled)
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			registry := frameworkruntime.Registry{}
			cfgPls := &schedulerapi.Plugins{}
			plugins := append(tt.plugins, &fakebindPlugin{}, &fakeQueueSortPlugin{})
			for _, pl := range plugins {
				tmpPl := pl
				if err := registry.Register(pl.Name(), func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
					return tmpPl, nil
				}); err != nil {
					t.Fatalf("fail to register filter plugin (%s)", pl.Name())
				}
				cfgPls.MultiPoint.Enabled = append(cfgPls.MultiPoint.Enabled, schedulerapi.Plugin{Name: pl.Name()})
			}

			profile := schedulerapi.KubeSchedulerProfile{Plugins: cfgPls}
			fwk, err := newFramework(ctx, registry, profile)
			if err != nil {
				t.Fatal(err)
			}

			exts := fwk.EnqueueExtensions()
			// need to sort to make the test result stable.
			sort.Slice(exts, func(i, j int) bool {
				return exts[i].Name() < exts[j].Name()
			})

			got, err := buildQueueingHintMap(ctx, exts)
			if err != nil {
				if tt.wantErr != nil && tt.wantErr.Error() != err.Error() {
					t.Fatalf("unexpected error from buildQueueingHintMap: expected: %v, actual: %v", tt.wantErr, err)
				}

				if tt.wantErr == nil {
					t.Fatalf("unexpected error from buildQueueingHintMap: %v", err)
				}
			}

			for e, fns := range got {
				wantfns, ok := tt.want[e]
				if !ok {
					t.Errorf("got unexpected event %v", e)
					continue
				}
				if len(fns) != len(wantfns) {
					t.Errorf("got %v queueing hint functions, want %v", len(fns), len(wantfns))
					continue
				}
				for i, fn := range fns {
					if fn.PluginName != wantfns[i].PluginName {
						t.Errorf("got plugin name %v, want %v", fn.PluginName, wantfns[i].PluginName)
						continue
					}
					got, gotErr := fn.QueueingHintFn(logger, nil, nil, nil)
					want, wantErr := wantfns[i].QueueingHintFn(logger, nil, nil, nil)
					if got != want || gotErr != wantErr {
						t.Errorf("got queueing hint function (%v) returning (%v, %v), expect it to return (%v, %v)", fn.PluginName, got, gotErr, want, wantErr)
						continue
					}
				}
			}
		})
	}
}

// Test_UnionedGVKs tests UnionedGVKs worked with buildQueueingHintMap.
func Test_UnionedGVKs(t *testing.T) {
	tests := []struct {
		name                            string
		plugins                         schedulerapi.PluginSet
		want                            map[framework.EventResource]framework.ActionType
		enableInPlacePodVerticalScaling bool
		enableSchedulerQueueingHints    bool
	}{
		{
			name: "filter without EnqueueExtensions plugin",
			plugins: schedulerapi.PluginSet{
				Enabled: []schedulerapi.Plugin{
					{Name: filterWithoutEnqueueExtensions},
					{Name: queueSort},
					{Name: fakeBind},
				},
				Disabled: []schedulerapi.Plugin{{Name: "*"}}, // disable default plugins
			},
			want: map[framework.EventResource]framework.ActionType{
				framework.Pod:                   framework.All,
				framework.Node:                  framework.All,
				framework.CSINode:               framework.All,
				framework.CSIDriver:             framework.All,
				framework.CSIStorageCapacity:    framework.All,
				framework.PersistentVolume:      framework.All,
				framework.PersistentVolumeClaim: framework.All,
				framework.StorageClass:          framework.All,
				framework.ResourceClaim:         framework.All,
				framework.DeviceClass:           framework.All,
			},
		},
		{
			name: "node plugin",
			plugins: schedulerapi.PluginSet{
				Enabled: []schedulerapi.Plugin{
					{Name: fakeNode},
					{Name: queueSort},
					{Name: fakeBind},
				},
				Disabled: []schedulerapi.Plugin{{Name: "*"}}, // disable default plugins
			},
			want: map[framework.EventResource]framework.ActionType{
				framework.Node: framework.Add | framework.UpdateNodeTaint, // When Node/Add is registered, Node/UpdateNodeTaint is automatically registered.
			},
		},
		{
			name: "pod plugin",
			plugins: schedulerapi.PluginSet{
				Enabled: []schedulerapi.Plugin{
					{Name: fakePod},
					{Name: queueSort},
					{Name: fakeBind},
				},
				Disabled: []schedulerapi.Plugin{{Name: "*"}}, // disable default plugins
			},
			want: map[framework.EventResource]framework.ActionType{
				framework.Pod: framework.Add,
			},
		},
		{
			name: "node and pod plugin",
			plugins: schedulerapi.PluginSet{
				Enabled: []schedulerapi.Plugin{
					{Name: fakePod},
					{Name: fakeNode},
					{Name: queueSort},
					{Name: fakeBind},
				},
				Disabled: []schedulerapi.Plugin{{Name: "*"}}, // disable default plugins
			},
			want: map[framework.EventResource]framework.ActionType{
				framework.Pod:  framework.Add,
				framework.Node: framework.Add | framework.UpdateNodeTaint, // When Node/Add is registered, Node/UpdateNodeTaint is automatically registered.
			},
		},
		{
			name: "empty EventsToRegister plugin",
			plugins: schedulerapi.PluginSet{
				Enabled: []schedulerapi.Plugin{
					{Name: emptyEventsToRegister},
					{Name: queueSort},
					{Name: fakeBind},
				},
				Disabled: []schedulerapi.Plugin{{Name: "*"}}, // disable default plugins
			},
			want: map[framework.EventResource]framework.ActionType{},
		},
		{
			name:    "plugins with default profile (No feature gate enabled)",
			plugins: schedulerapi.PluginSet{Enabled: defaults.PluginsV1.MultiPoint.Enabled},
			want: map[framework.EventResource]framework.ActionType{
				framework.Pod:                   framework.Add | framework.UpdatePodLabel | framework.Delete,
				framework.Node:                  framework.Add | framework.UpdateNodeAllocatable | framework.UpdateNodeLabel | framework.UpdateNodeTaint | framework.Delete,
				framework.CSINode:               framework.All - framework.Delete,
				framework.CSIDriver:             framework.Update,
				framework.CSIStorageCapacity:    framework.All - framework.Delete,
				framework.PersistentVolume:      framework.All - framework.Delete,
				framework.PersistentVolumeClaim: framework.All - framework.Delete,
				framework.StorageClass:          framework.All - framework.Delete,
				framework.VolumeAttachment:      framework.Delete,
			},
		},
		{
			name:    "plugins with default profile (InPlacePodVerticalScaling: enabled)",
			plugins: schedulerapi.PluginSet{Enabled: defaults.PluginsV1.MultiPoint.Enabled},
			want: map[framework.EventResource]framework.ActionType{
				framework.Pod:                   framework.Add | framework.UpdatePodLabel | framework.UpdatePodScaleDown | framework.Delete,
				framework.Node:                  framework.Add | framework.UpdateNodeAllocatable | framework.UpdateNodeLabel | framework.UpdateNodeTaint | framework.Delete,
				framework.CSINode:               framework.All - framework.Delete,
				framework.CSIDriver:             framework.Update,
				framework.CSIStorageCapacity:    framework.All - framework.Delete,
				framework.PersistentVolume:      framework.All - framework.Delete,
				framework.PersistentVolumeClaim: framework.All - framework.Delete,
				framework.StorageClass:          framework.All - framework.Delete,
				framework.VolumeAttachment:      framework.Delete,
			},
			enableInPlacePodVerticalScaling: true,
		},
		{
			name:    "plugins with default profile (queueingHint/InPlacePodVerticalScaling: enabled)",
			plugins: schedulerapi.PluginSet{Enabled: defaults.PluginsV1.MultiPoint.Enabled},
			want: map[framework.EventResource]framework.ActionType{
				framework.Pod:                   framework.Add | framework.UpdatePodLabel | framework.UpdatePodScaleDown | framework.UpdatePodToleration | framework.UpdatePodSchedulingGatesEliminated | framework.Delete,
				framework.Node:                  framework.Add | framework.UpdateNodeAllocatable | framework.UpdateNodeLabel | framework.UpdateNodeTaint | framework.Delete,
				framework.CSINode:               framework.All - framework.Delete,
				framework.CSIDriver:             framework.Update,
				framework.CSIStorageCapacity:    framework.All - framework.Delete,
				framework.PersistentVolume:      framework.All - framework.Delete,
				framework.PersistentVolumeClaim: framework.All - framework.Delete,
				framework.StorageClass:          framework.All - framework.Delete,
				framework.VolumeAttachment:      framework.Delete,
			},
			enableInPlacePodVerticalScaling: true,
			enableSchedulerQueueingHints:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, tt.enableInPlacePodVerticalScaling)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, tt.enableSchedulerQueueingHints)

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			registry := plugins.NewInTreeRegistry()

			cfgPls := &schedulerapi.Plugins{MultiPoint: tt.plugins}
			plugins := []framework.Plugin{&fakeNodePlugin{}, &fakePodPlugin{}, &filterWithoutEnqueueExtensionsPlugin{}, &emptyEventsToRegisterPlugin{}, &fakeQueueSortPlugin{}, &fakebindPlugin{}}
			for _, pl := range plugins {
				tmpPl := pl
				if err := registry.Register(pl.Name(), func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
					return tmpPl, nil
				}); err != nil {
					t.Fatalf("fail to register filter plugin (%s)", pl.Name())
				}
			}

			profile := schedulerapi.KubeSchedulerProfile{Plugins: cfgPls, PluginConfig: defaults.PluginConfigsV1}
			fwk, err := newFramework(ctx, registry, profile)
			if err != nil {
				t.Fatal(err)
			}
			queueingHintMap, err := buildQueueingHintMap(ctx, fwk.EnqueueExtensions())
			if err != nil {
				t.Fatal(err)
			}
			queueingHintsPerProfile := internalqueue.QueueingHintMapPerProfile{
				"default": queueingHintMap,
			}
			got := unionedGVKs(queueingHintsPerProfile)

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Unexpected eventToPlugin map (-want,+got):%s", diff)
			}
		})
	}
}

func newFramework(ctx context.Context, r frameworkruntime.Registry, profile schedulerapi.KubeSchedulerProfile) (framework.Framework, error) {
	return frameworkruntime.NewFramework(ctx, r, &profile,
		frameworkruntime.WithSnapshotSharedLister(internalcache.NewSnapshot(nil, nil)),
		frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(fake.NewClientset(), 0)),
	)
}

func TestFrameworkHandler_IterateOverWaitingPods(t *testing.T) {
	const (
		testSchedulerProfile1 = "test-scheduler-profile-1"
		testSchedulerProfile2 = "test-scheduler-profile-2"
		testSchedulerProfile3 = "test-scheduler-profile-3"
	)

	nodes := []runtime.Object{
		st.MakeNode().Name("node1").UID("node1").Obj(),
		st.MakeNode().Name("node2").UID("node2").Obj(),
		st.MakeNode().Name("node3").UID("node3").Obj(),
	}

	cases := []struct {
		name                        string
		profiles                    []schedulerapi.KubeSchedulerProfile
		waitSchedulingPods          []*v1.Pod
		expectPodNamesInWaitingPods []string
	}{
		{
			name: "pods with same profile are waiting on permit stage",
			profiles: []schedulerapi.KubeSchedulerProfile{
				{
					SchedulerName: testSchedulerProfile1,
					Plugins: &schedulerapi.Plugins{
						QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
						Permit:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: fakePermit}}},
						Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
					},
				},
			},
			waitSchedulingPods: []*v1.Pod{
				st.MakePod().Name("pod1").UID("pod1").SchedulerName(testSchedulerProfile1).Obj(),
				st.MakePod().Name("pod2").UID("pod2").SchedulerName(testSchedulerProfile1).Obj(),
				st.MakePod().Name("pod3").UID("pod3").SchedulerName(testSchedulerProfile1).Obj(),
			},
			expectPodNamesInWaitingPods: []string{"pod1", "pod2", "pod3"},
		},
		{
			name: "pods with different profiles are waiting on permit stage",
			profiles: []schedulerapi.KubeSchedulerProfile{
				{
					SchedulerName: testSchedulerProfile1,
					Plugins: &schedulerapi.Plugins{
						QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
						Permit:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: fakePermit}}},
						Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
					},
				},
				{
					SchedulerName: testSchedulerProfile2,
					Plugins: &schedulerapi.Plugins{
						QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
						Permit:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: fakePermit}}},
						Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
					},
				},
				{
					SchedulerName: testSchedulerProfile3,
					Plugins: &schedulerapi.Plugins{
						QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
						Permit:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: fakePermit}}},
						Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
					},
				},
			},
			waitSchedulingPods: []*v1.Pod{
				st.MakePod().Name("pod1").UID("pod1").SchedulerName(testSchedulerProfile1).Obj(),
				st.MakePod().Name("pod2").UID("pod2").SchedulerName(testSchedulerProfile1).Obj(),
				st.MakePod().Name("pod3").UID("pod3").SchedulerName(testSchedulerProfile2).Obj(),
				st.MakePod().Name("pod4").UID("pod4").SchedulerName(testSchedulerProfile3).Obj(),
			},
			expectPodNamesInWaitingPods: []string{"pod1", "pod2", "pod3", "pod4"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Set up scheduler for the 3 nodes.
			objs := append([]runtime.Object{&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ""}}}, nodes...)
			fakeClient := fake.NewClientset(objs...)
			informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
			eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: fakeClient.EventsV1()})
			defer eventBroadcaster.Shutdown()

			eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, fakePermit)

			outOfTreeRegistry := frameworkruntime.Registry{
				fakePermit: newFakePermitPlugin(eventRecorder),
			}

			tCtx := utiltesting.Init(t)

			scheduler, err := New(
				tCtx,
				fakeClient,
				informerFactory,
				nil,
				profile.NewRecorderFactory(eventBroadcaster),
				WithProfiles(tc.profiles...),
				WithFrameworkOutOfTreeRegistry(outOfTreeRegistry),
			)

			if err != nil {
				t.Fatalf("Failed to create scheduler: %v", err)
			}

			var wg sync.WaitGroup
			waitSchedulingPodNumber := len(tc.waitSchedulingPods)
			wg.Add(waitSchedulingPodNumber)
			stopFn, err := eventBroadcaster.StartEventWatcher(func(obj runtime.Object) {
				e, ok := obj.(*eventsv1.Event)
				if !ok || (e.Reason != podWaitingReason) {
					return
				}
				wg.Done()
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stopFn()

			// Run scheduler.
			informerFactory.Start(tCtx.Done())
			informerFactory.WaitForCacheSync(tCtx.Done())
			go scheduler.Run(tCtx)

			// Send pods to be scheduled.
			for _, p := range tc.waitSchedulingPods {
				_, err = fakeClient.CoreV1().Pods("").Create(tCtx, p, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}

			// Wait all pods in waitSchedulingPods to be scheduled.
			wg.Wait()

			utiltesting.Eventually(tCtx, func(utiltesting.TContext) sets.Set[string] {
				// Ensure that all waitingPods in scheduler can be obtained from any profiles.
				actualPodNamesInWaitingPods := sets.New[string]()
				for _, fwk := range scheduler.Profiles {
					fwk.IterateOverWaitingPods(func(pod framework.WaitingPod) {
						actualPodNamesInWaitingPods.Insert(pod.GetPod().Name)
					})
				}
				return actualPodNamesInWaitingPods
			}).WithTimeout(permitTimeout).Should(gomega.Equal(sets.New(tc.expectPodNamesInWaitingPods...)), "unexpected waitingPods in scheduler profile")
		})
	}
}

var _ framework.QueueSortPlugin = &fakeQueueSortPlugin{}

// fakeQueueSortPlugin is a no-op implementation for QueueSort extension point.
type fakeQueueSortPlugin struct{}

func (pl *fakeQueueSortPlugin) Name() string {
	return queueSort
}

func (pl *fakeQueueSortPlugin) Less(_, _ *framework.QueuedPodInfo) bool {
	return false
}

var _ framework.BindPlugin = &fakebindPlugin{}

// fakebindPlugin is a no-op implementation for Bind extension point.
type fakebindPlugin struct{}

func (t *fakebindPlugin) Name() string {
	return fakeBind
}

func (t *fakebindPlugin) Bind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	return nil
}

// filterWithoutEnqueueExtensionsPlugin implements Filter, but doesn't implement EnqueueExtensions.
type filterWithoutEnqueueExtensionsPlugin struct{}

func (*filterWithoutEnqueueExtensionsPlugin) Name() string { return filterWithoutEnqueueExtensions }

func (*filterWithoutEnqueueExtensionsPlugin) Filter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, _ *framework.NodeInfo) *framework.Status {
	return nil
}

var hintFromFakeNode = framework.QueueingHint(100)

type fakeNodePlugin struct{}

var fakeNodePluginQueueingFn = func(_ klog.Logger, _ *v1.Pod, _, _ interface{}) (framework.QueueingHint, error) {
	return hintFromFakeNode, nil
}

func (*fakeNodePlugin) Name() string { return fakeNode }

func (*fakeNodePlugin) Filter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, _ *framework.NodeInfo) *framework.Status {
	return nil
}

func (pl *fakeNodePlugin) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Add}, QueueingHintFn: fakeNodePluginQueueingFn},
	}, nil
}

var hintFromFakePod = framework.QueueingHint(101)

type fakePodPlugin struct{}

var fakePodPluginQueueingFn = func(_ klog.Logger, _ *v1.Pod, _, _ interface{}) (framework.QueueingHint, error) {
	return hintFromFakePod, nil
}

func (*fakePodPlugin) Name() string { return fakePod }

func (*fakePodPlugin) Filter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, _ *framework.NodeInfo) *framework.Status {
	return nil
}

func (pl *fakePodPlugin) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Add}, QueueingHintFn: fakePodPluginQueueingFn},
	}, nil
}

type emptyEventPlugin struct{}

func (*emptyEventPlugin) Name() string { return emptyEventExtensions }

func (*emptyEventPlugin) Filter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, _ *framework.NodeInfo) *framework.Status {
	return nil
}

func (pl *emptyEventPlugin) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return nil, nil
}

// errorEventsToRegisterPlugin is a mock plugin that returns an error for EventsToRegister method
type errorEventsToRegisterPlugin struct{}

func (*errorEventsToRegisterPlugin) Name() string { return errorEventsToRegister }

func (*errorEventsToRegisterPlugin) Filter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, _ *framework.NodeInfo) *framework.Status {
	return nil
}

func (*errorEventsToRegisterPlugin) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return nil, errors.New("mock error")
}

// emptyEventsToRegisterPlugin implement interface framework.EnqueueExtensions, but returns nil from EventsToRegister.
// This can simulate a plugin registered at scheduler setup, but does nothing
// due to some disabled feature gate.
type emptyEventsToRegisterPlugin struct{}

func (*emptyEventsToRegisterPlugin) Name() string { return emptyEventsToRegister }

func (*emptyEventsToRegisterPlugin) Filter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, _ *framework.NodeInfo) *framework.Status {
	return nil
}

func (*emptyEventsToRegisterPlugin) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return nil, nil
}

// fakePermitPlugin only implements PermitPlugin interface.
type fakePermitPlugin struct {
	eventRecorder events.EventRecorder
}

func newFakePermitPlugin(eventRecorder events.EventRecorder) frameworkruntime.PluginFactory {
	return func(ctx context.Context, configuration runtime.Object, f framework.Handle) (framework.Plugin, error) {
		pl := &fakePermitPlugin{
			eventRecorder: eventRecorder,
		}
		return pl, nil
	}
}

func (f fakePermitPlugin) Name() string {
	return fakePermit
}

const (
	podWaitingReason = "podWaiting"
	permitTimeout    = 10 * time.Second
)

func (f fakePermitPlugin) Permit(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (*framework.Status, time.Duration) {
	defer func() {
		// Send event with podWaiting reason to broadcast this pod is already waiting in the permit stage.
		f.eventRecorder.Eventf(p, nil, v1.EventTypeWarning, podWaitingReason, "", "")
	}()

	return framework.NewStatus(framework.Wait), permitTimeout
}

var _ framework.PermitPlugin = &fakePermitPlugin{}
