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

package daemon

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	unversionedapps "k8s.io/client-go/kubernetes/typed/apps/v1"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/workqueue"
	v1helper "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/daemon/util"
)

const (
	// BurstReplicas is a rate limiter for booting pods on a lot of pods.
	// The value of 250 is chosen b/c values that are too high can cause registry DoS issues.
	BurstReplicas = 250

	// StatusUpdateRetries limits the number of retries if sending a status update to API server fails.
	StatusUpdateRetries = 1

	// BackoffGCInterval is the time that has to pass before next iteration of backoff GC is run
	BackoffGCInterval = 1 * time.Minute
)

// Reasons for DaemonSet events
const (
	// SelectingAllReason is added to an event when a DaemonSet selects all Pods.
	SelectingAllReason = "SelectingAll"
	// FailedPlacementReason is added to an event when a DaemonSet can't schedule a Pod to a specified node.
	FailedPlacementReason = "FailedPlacement"
	// FailedDaemonPodReason is added to an event when the status of a Pod of a DaemonSet is 'Failed'.
	FailedDaemonPodReason = "FailedDaemonPod"
	// SucceededDaemonPodReason is added to an event when the status of a Pod of a DaemonSet is 'Succeeded'.
	SucceededDaemonPodReason = "SucceededDaemonPod"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = apps.SchemeGroupVersion.WithKind("DaemonSet")

// DaemonSetsController is responsible for synchronizing DaemonSet objects stored
// in the system with actual running pods.
type DaemonSetsController struct {
	kubeClient clientset.Interface

	eventBroadcaster record.EventBroadcaster
	eventRecorder    record.EventRecorder

	podControl controller.PodControlInterface
	crControl  controller.ControllerRevisionControlInterface

	// An dsc is temporarily suspended after creating/deleting these many replicas.
	// It resumes normal action after observing the watch events for them.
	burstReplicas int

	// To allow injection of syncDaemonSet for testing.
	syncHandler func(ctx context.Context, dsKey string) error
	// used for unit testing
	enqueueDaemonSet func(ds *apps.DaemonSet)
	// A TTLCache of pod creates/deletes each ds expects to see
	expectations controller.ControllerExpectationsInterface
	// dsLister can list/get daemonsets from the shared informer's store
	dsLister appslisters.DaemonSetLister
	// dsStoreSynced returns true if the daemonset store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	dsStoreSynced cache.InformerSynced
	// historyLister get list/get history from the shared informers's store
	historyLister appslisters.ControllerRevisionLister
	// historyStoreSynced returns true if the history store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	historyStoreSynced cache.InformerSynced
	// podLister get list/get pods from the shared informers's store
	podLister corelisters.PodLister
	// podStoreSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podStoreSynced cache.InformerSynced
	// nodeLister can list/get nodes from the shared informer's store
	nodeLister corelisters.NodeLister
	// nodeStoreSynced returns true if the node store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	nodeStoreSynced cache.InformerSynced

	// DaemonSet keys that need to be synced.
	queue workqueue.TypedRateLimitingInterface[string]

	failedPodsBackoff *flowcontrol.Backoff
}

// NewDaemonSetsController creates a new DaemonSetsController
func NewDaemonSetsController(
	ctx context.Context,
	daemonSetInformer appsinformers.DaemonSetInformer,
	historyInformer appsinformers.ControllerRevisionInformer,
	podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer,
	kubeClient clientset.Interface,
	failedPodsBackoff *flowcontrol.Backoff,
) (*DaemonSetsController, error) {
	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	logger := klog.FromContext(ctx)
	dsc := &DaemonSetsController{
		kubeClient:       kubeClient,
		eventBroadcaster: eventBroadcaster,
		eventRecorder:    eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "daemonset-controller"}),
		podControl: controller.RealPodControl{
			KubeClient: kubeClient,
			Recorder:   eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "daemonset-controller"}),
		},
		crControl: controller.RealControllerRevisionControl{
			KubeClient: kubeClient,
		},
		burstReplicas: BurstReplicas,
		expectations:  controller.NewControllerExpectations(),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "daemonset",
			},
		),
	}

	daemonSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dsc.addDaemonset(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			dsc.updateDaemonset(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			dsc.deleteDaemonset(logger, obj)
		},
	})
	dsc.dsLister = daemonSetInformer.Lister()
	dsc.dsStoreSynced = daemonSetInformer.Informer().HasSynced

	historyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dsc.addHistory(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			dsc.updateHistory(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			dsc.deleteHistory(logger, obj)
		},
	})
	dsc.historyLister = historyInformer.Lister()
	dsc.historyStoreSynced = historyInformer.Informer().HasSynced

	// Watch for creation/deletion of pods. The reason we watch is that we don't want a daemon set to create/delete
	// more pods until all the effects (expectations) of a daemon set's create/delete have been observed.
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dsc.addPod(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			dsc.updatePod(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			dsc.deletePod(logger, obj)
		},
	})
	dsc.podLister = podInformer.Lister()
	dsc.podStoreSynced = podInformer.Informer().HasSynced

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dsc.addNode(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			dsc.updateNode(logger, oldObj, newObj)
		},
	},
	)
	dsc.nodeStoreSynced = nodeInformer.Informer().HasSynced
	dsc.nodeLister = nodeInformer.Lister()

	dsc.syncHandler = dsc.syncDaemonSet
	dsc.enqueueDaemonSet = dsc.enqueue

	dsc.failedPodsBackoff = failedPodsBackoff

	return dsc, nil
}

func (dsc *DaemonSetsController) addDaemonset(logger klog.Logger, obj interface{}) {
	ds := obj.(*apps.DaemonSet)
	logger.V(4).Info("Adding daemon set", "daemonset", klog.KObj(ds))
	dsc.enqueueDaemonSet(ds)
}

func (dsc *DaemonSetsController) updateDaemonset(logger klog.Logger, cur, old interface{}) {
	oldDS := old.(*apps.DaemonSet)
	curDS := cur.(*apps.DaemonSet)

	// TODO: make a KEP and fix informers to always call the delete event handler on re-create
	if curDS.UID != oldDS.UID {
		key, err := controller.KeyFunc(oldDS)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", oldDS, err))
			return
		}
		dsc.deleteDaemonset(logger, cache.DeletedFinalStateUnknown{
			Key: key,
			Obj: oldDS,
		})
	}

	logger.V(4).Info("Updating daemon set", "daemonset", klog.KObj(oldDS))
	dsc.enqueueDaemonSet(curDS)
}

func (dsc *DaemonSetsController) deleteDaemonset(logger klog.Logger, obj interface{}) {
	ds, ok := obj.(*apps.DaemonSet)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		ds, ok = tombstone.Obj.(*apps.DaemonSet)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a DaemonSet %#v", obj))
			return
		}
	}
	logger.V(4).Info("Deleting daemon set", "daemonset", klog.KObj(ds))

	key, err := controller.KeyFunc(ds)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", ds, err))
		return
	}

	// Delete expectations for the DaemonSet so if we create a new one with the same name it starts clean
	dsc.expectations.DeleteExpectations(logger, key)

	dsc.queue.Add(key)
}

// Run begins watching and syncing daemon sets.
func (dsc *DaemonSetsController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()

	dsc.eventBroadcaster.StartStructuredLogging(3)
	dsc.eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: dsc.kubeClient.CoreV1().Events("")})
	defer dsc.eventBroadcaster.Shutdown()

	defer dsc.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting daemon sets controller")
	defer logger.Info("Shutting down daemon sets controller")

	if !cache.WaitForNamedCacheSync("daemon sets", ctx.Done(), dsc.podStoreSynced, dsc.nodeStoreSynced, dsc.historyStoreSynced, dsc.dsStoreSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, dsc.runWorker, time.Second)
	}

	go wait.Until(dsc.failedPodsBackoff.GC, BackoffGCInterval, ctx.Done())

	<-ctx.Done()
}

func (dsc *DaemonSetsController) runWorker(ctx context.Context) {
	for dsc.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (dsc *DaemonSetsController) processNextWorkItem(ctx context.Context) bool {
	dsKey, quit := dsc.queue.Get()
	if quit {
		return false
	}
	defer dsc.queue.Done(dsKey)

	err := dsc.syncHandler(ctx, dsKey)
	if err == nil {
		dsc.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	dsc.queue.AddRateLimited(dsKey)

	return true
}

func (dsc *DaemonSetsController) enqueue(ds *apps.DaemonSet) {
	key, err := controller.KeyFunc(ds)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", ds, err))
		return
	}

	// TODO: Handle overlapping controllers better. See comment in ReplicationManager.
	dsc.queue.Add(key)
}

func (dsc *DaemonSetsController) enqueueDaemonSetAfter(obj interface{}, after time.Duration) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	// TODO: Handle overlapping controllers better. See comment in ReplicationManager.
	dsc.queue.AddAfter(key, after)
}

// getDaemonSetsForPod returns a list of DaemonSets that potentially match the pod.
func (dsc *DaemonSetsController) getDaemonSetsForPod(pod *v1.Pod) []*apps.DaemonSet {
	sets, err := dsc.dsLister.GetPodDaemonSets(pod)
	if err != nil {
		return nil
	}
	if len(sets) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than one
		// item in this list nevertheless constitutes user error.
		utilruntime.HandleError(fmt.Errorf("user error! more than one daemon is selecting pods with labels: %+v", pod.Labels))
	}
	return sets
}

// getDaemonSetsForHistory returns a list of DaemonSets that potentially
// match a ControllerRevision.
func (dsc *DaemonSetsController) getDaemonSetsForHistory(logger klog.Logger, history *apps.ControllerRevision) []*apps.DaemonSet {
	daemonSets, err := dsc.dsLister.GetHistoryDaemonSets(history)
	if err != nil || len(daemonSets) == 0 {
		return nil
	}
	if len(daemonSets) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than one
		// item in this list nevertheless constitutes user error.
		logger.V(4).Info("Found more than one DaemonSet selecting the ControllerRevision. This is potentially a user error",
			"controllerRevision", klog.KObj(history), "labels", history.Labels)
	}
	return daemonSets
}

// addHistory enqueues the DaemonSet that manages a ControllerRevision when the ControllerRevision is created
// or when the controller manager is restarted.
func (dsc *DaemonSetsController) addHistory(logger klog.Logger, obj interface{}) {
	history := obj.(*apps.ControllerRevision)
	if history.DeletionTimestamp != nil {
		// On a restart of the controller manager, it's possible for an object to
		// show up in a state that is already pending deletion.
		dsc.deleteHistory(logger, history)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(history); controllerRef != nil {
		ds := dsc.resolveControllerRef(history.Namespace, controllerRef)
		if ds == nil {
			return
		}
		logger.V(4).Info("Observed a ControllerRevision", "controllerRevision", klog.KObj(history))
		return
	}

	// Otherwise, it's an orphan. Get a list of all matching DaemonSets and sync
	// them to see if anyone wants to adopt it.
	daemonSets := dsc.getDaemonSetsForHistory(logger, history)
	if len(daemonSets) == 0 {
		return
	}
	logger.V(4).Info("Orphan ControllerRevision added", "controllerRevision", klog.KObj(history))
	for _, ds := range daemonSets {
		dsc.enqueueDaemonSet(ds)
	}
}

// updateHistory figures out what DaemonSet(s) manage a ControllerRevision when the ControllerRevision
// is updated and wake them up. If anything of the ControllerRevision has changed, we need to  awaken
// both the old and new DaemonSets.
func (dsc *DaemonSetsController) updateHistory(logger klog.Logger, old, cur interface{}) {
	curHistory := cur.(*apps.ControllerRevision)
	oldHistory := old.(*apps.ControllerRevision)
	if curHistory.ResourceVersion == oldHistory.ResourceVersion {
		// Periodic resync will send update events for all known ControllerRevisions.
		return
	}

	curControllerRef := metav1.GetControllerOf(curHistory)
	oldControllerRef := metav1.GetControllerOf(oldHistory)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if ds := dsc.resolveControllerRef(oldHistory.Namespace, oldControllerRef); ds != nil {
			dsc.enqueueDaemonSet(ds)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		ds := dsc.resolveControllerRef(curHistory.Namespace, curControllerRef)
		if ds == nil {
			return
		}
		logger.V(4).Info("Observed an update to a ControllerRevision", "controllerRevision", klog.KObj(curHistory))
		dsc.enqueueDaemonSet(ds)
		return
	}

	// Otherwise, it's an orphan. If anything changed, sync matching controllers
	// to see if anyone wants to adopt it now.
	labelChanged := !reflect.DeepEqual(curHistory.Labels, oldHistory.Labels)
	if labelChanged || controllerRefChanged {
		daemonSets := dsc.getDaemonSetsForHistory(logger, curHistory)
		if len(daemonSets) == 0 {
			return
		}
		logger.V(4).Info("Orphan ControllerRevision updated", "controllerRevision", klog.KObj(curHistory))
		for _, ds := range daemonSets {
			dsc.enqueueDaemonSet(ds)
		}
	}
}

// deleteHistory enqueues the DaemonSet that manages a ControllerRevision when
// the ControllerRevision is deleted. obj could be an *app.ControllerRevision, or
// a DeletionFinalStateUnknown marker item.
func (dsc *DaemonSetsController) deleteHistory(logger klog.Logger, obj interface{}) {
	history, ok := obj.(*apps.ControllerRevision)

	// When a delete is dropped, the relist will notice a ControllerRevision in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the ControllerRevision
	// changed labels the new DaemonSet will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		history, ok = tombstone.Obj.(*apps.ControllerRevision)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a ControllerRevision %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(history)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	ds := dsc.resolveControllerRef(history.Namespace, controllerRef)
	if ds == nil {
		return
	}
	logger.V(4).Info("ControllerRevision deleted", "controllerRevision", klog.KObj(history))
	dsc.enqueueDaemonSet(ds)
}

func (dsc *DaemonSetsController) addPod(logger klog.Logger, obj interface{}) {
	pod := obj.(*v1.Pod)

	if pod.DeletionTimestamp != nil {
		// on a restart of the controller manager, it's possible a new pod shows up in a state that
		// is already pending deletion. Prevent the pod from being a creation observation.
		dsc.deletePod(logger, pod)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(pod); controllerRef != nil {
		ds := dsc.resolveControllerRef(pod.Namespace, controllerRef)
		if ds == nil {
			return
		}
		dsKey, err := controller.KeyFunc(ds)
		if err != nil {
			return
		}
		logger.V(4).Info("Pod added", "pod", klog.KObj(pod))
		dsc.expectations.CreationObserved(logger, dsKey)
		dsc.enqueueDaemonSet(ds)
		return
	}

	// Otherwise, it's an orphan. Get a list of all matching DaemonSets and sync
	// them to see if anyone wants to adopt it.
	// DO NOT observe creation because no controller should be waiting for an
	// orphan.
	dss := dsc.getDaemonSetsForPod(pod)
	if len(dss) == 0 {
		return
	}
	logger.V(4).Info("Orphan Pod added", "pod", klog.KObj(pod))
	for _, ds := range dss {
		dsc.enqueueDaemonSet(ds)
	}
}

// When a pod is updated, figure out what sets manage it and wake them
// up. If the labels of the pod have changed we need to awaken both the old
// and new set. old and cur must be *v1.Pod types.
func (dsc *DaemonSetsController) updatePod(logger klog.Logger, old, cur interface{}) {
	curPod := cur.(*v1.Pod)
	oldPod := old.(*v1.Pod)
	if curPod.ResourceVersion == oldPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// Two different versions of the same pod will always have different RVs.
		return
	}

	if curPod.DeletionTimestamp != nil {
		// when a pod is deleted gracefully its deletion timestamp is first modified to reflect a grace period,
		// and after such time has passed, the kubelet actually deletes it from the store. We receive an update
		// for modification of the deletion timestamp and expect an ds to create more replicas asap, not wait
		// until the kubelet actually deletes the pod.
		dsc.deletePod(logger, curPod)
		return
	}

	curControllerRef := metav1.GetControllerOf(curPod)
	oldControllerRef := metav1.GetControllerOf(oldPod)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if ds := dsc.resolveControllerRef(oldPod.Namespace, oldControllerRef); ds != nil {
			dsc.enqueueDaemonSet(ds)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		ds := dsc.resolveControllerRef(curPod.Namespace, curControllerRef)
		if ds == nil {
			return
		}
		logger.V(4).Info("Pod updated", "pod", klog.KObj(curPod))
		dsc.enqueueDaemonSet(ds)
		changedToReady := !podutil.IsPodReady(oldPod) && podutil.IsPodReady(curPod)
		// See https://github.com/kubernetes/kubernetes/pull/38076 for more details
		if changedToReady && ds.Spec.MinReadySeconds > 0 {
			// Add a second to avoid milliseconds skew in AddAfter.
			// See https://github.com/kubernetes/kubernetes/issues/39785#issuecomment-279959133 for more info.
			dsc.enqueueDaemonSetAfter(ds, (time.Duration(ds.Spec.MinReadySeconds)*time.Second)+time.Second)
		}
		return
	}

	// Otherwise, it's an orphan. If anything changed, sync matching controllers
	// to see if anyone wants to adopt it now.
	dss := dsc.getDaemonSetsForPod(curPod)
	if len(dss) == 0 {
		return
	}
	logger.V(4).Info("Orphan Pod updated", "pod", klog.KObj(curPod))
	labelChanged := !reflect.DeepEqual(curPod.Labels, oldPod.Labels)
	if labelChanged || controllerRefChanged {
		for _, ds := range dss {
			dsc.enqueueDaemonSet(ds)
		}
	}
}

func (dsc *DaemonSetsController) deletePod(logger klog.Logger, obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the pod
	// changed labels the new daemonset will not be woken up till the periodic
	// resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a pod %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	ds := dsc.resolveControllerRef(pod.Namespace, controllerRef)
	if ds == nil {
		return
	}
	dsKey, err := controller.KeyFunc(ds)
	if err != nil {
		return
	}
	logger.V(4).Info("Pod deleted", "pod", klog.KObj(pod))
	dsc.expectations.DeletionObserved(logger, dsKey)
	dsc.enqueueDaemonSet(ds)
}

func (dsc *DaemonSetsController) addNode(logger klog.Logger, obj interface{}) {
	// TODO: it'd be nice to pass a hint with these enqueues, so that each ds would only examine the added node (unless it has other work to do, too).
	dsList, err := dsc.dsLister.List(labels.Everything())
	if err != nil {
		logger.V(4).Info("Error enqueueing daemon sets", "err", err)
		return
	}
	node := obj.(*v1.Node)
	for _, ds := range dsList {
		if shouldRun, _ := NodeShouldRunDaemonPod(node, ds); shouldRun {
			dsc.enqueueDaemonSet(ds)
		}
	}
}

// shouldIgnoreNodeUpdate returns true if Node labels and taints have not changed, otherwise returns false.
// If other calling functions need to use other properties of Node, shouldIgnoreNodeUpdate needs to be updated.
func shouldIgnoreNodeUpdate(oldNode, curNode v1.Node) bool {
	return apiequality.Semantic.DeepEqual(oldNode.Labels, curNode.Labels) &&
		apiequality.Semantic.DeepEqual(oldNode.Spec.Taints, curNode.Spec.Taints)
}

func (dsc *DaemonSetsController) updateNode(logger klog.Logger, old, cur interface{}) {
	oldNode := old.(*v1.Node)
	curNode := cur.(*v1.Node)
	if shouldIgnoreNodeUpdate(*oldNode, *curNode) {
		return
	}

	dsList, err := dsc.dsLister.List(labels.Everything())
	if err != nil {
		logger.V(4).Info("Error listing daemon sets", "err", err)
		return
	}
	// TODO: it'd be nice to pass a hint with these enqueues, so that each ds would only examine the added node (unless it has other work to do, too).
	for _, ds := range dsList {
		// If NodeShouldRunDaemonPod needs to uses other than Labels and Taints (mutable) properties of node, it needs to update shouldIgnoreNodeUpdate.
		oldShouldRun, oldShouldContinueRunning := NodeShouldRunDaemonPod(oldNode, ds)
		currentShouldRun, currentShouldContinueRunning := NodeShouldRunDaemonPod(curNode, ds)
		if (oldShouldRun != currentShouldRun) || (oldShouldContinueRunning != currentShouldContinueRunning) {
			dsc.enqueueDaemonSet(ds)
		}
	}
}

// getDaemonPods returns daemon pods owned by the given ds.
// This also reconciles ControllerRef by adopting/orphaning.
// Note that returned Pods are pointers to objects in the cache.
// If you want to modify one, you need to deep-copy it first.
func (dsc *DaemonSetsController) getDaemonPods(ctx context.Context, ds *apps.DaemonSet) ([]*v1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return nil, err
	}

	// List all pods to include those that don't match the selector anymore but
	// have a ControllerRef pointing to this controller.
	pods, err := dsc.podLister.Pods(ds.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing Pods (see #42639).
	dsNotDeleted := controller.RecheckDeletionTimestamp(func(ctx context.Context) (metav1.Object, error) {
		fresh, err := dsc.kubeClient.AppsV1().DaemonSets(ds.Namespace).Get(ctx, ds.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.UID != ds.UID {
			return nil, fmt.Errorf("original DaemonSet %v/%v is gone: got uid %v, wanted %v", ds.Namespace, ds.Name, fresh.UID, ds.UID)
		}
		return fresh, nil
	})

	// Use ControllerRefManager to adopt/orphan as needed.
	cm := controller.NewPodControllerRefManager(dsc.podControl, ds, selector, controllerKind, dsNotDeleted)
	return cm.ClaimPods(ctx, pods)
}

// getNodesToDaemonPods returns a map from nodes to daemon pods (corresponding to ds) created for the nodes.
// This also reconciles ControllerRef by adopting/orphaning.
// Note that returned Pods are pointers to objects in the cache.
// If you want to modify one, you need to deep-copy it first.
func (dsc *DaemonSetsController) getNodesToDaemonPods(ctx context.Context, ds *apps.DaemonSet, includeDeletedTerminal bool) (map[string][]*v1.Pod, error) {
	claimedPods, err := dsc.getDaemonPods(ctx, ds)
	if err != nil {
		return nil, err
	}
	// Group Pods by Node name.
	nodeToDaemonPods := make(map[string][]*v1.Pod)
	logger := klog.FromContext(ctx)
	for _, pod := range claimedPods {
		if !includeDeletedTerminal && podutil.IsPodTerminal(pod) && pod.DeletionTimestamp != nil {
			// This Pod has a finalizer or is already scheduled for deletion from the
			// store by the kubelet or the Pod GC. The DS controller doesn't have
			// anything else to do with it.
			continue
		}
		nodeName, err := util.GetTargetNodeName(pod)
		if err != nil {
			logger.V(4).Info("Failed to get target node name of Pod in DaemonSet",
				"pod", klog.KObj(pod), "daemonset", klog.KObj(ds))
			continue
		}

		nodeToDaemonPods[nodeName] = append(nodeToDaemonPods[nodeName], pod)
	}

	return nodeToDaemonPods, nil
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (dsc *DaemonSetsController) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *apps.DaemonSet {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	ds, err := dsc.dsLister.DaemonSets(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if ds.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return ds
}

// podsShouldBeOnNode figures out the DaemonSet pods to be created and deleted on the given node:
//   - nodesNeedingDaemonPods: the pods need to start on the node
//   - podsToDelete: the Pods need to be deleted on the node
//   - err: unexpected error
func (dsc *DaemonSetsController) podsShouldBeOnNode(
	logger klog.Logger,
	node *v1.Node,
	nodeToDaemonPods map[string][]*v1.Pod,
	ds *apps.DaemonSet,
	hash string,
) (nodesNeedingDaemonPods, podsToDelete []string) {

	shouldRun, shouldContinueRunning := NodeShouldRunDaemonPod(node, ds)
	daemonPods, exists := nodeToDaemonPods[node.Name]

	switch {
	case shouldRun && !exists:
		// If daemon pod is supposed to be running on node, but isn't, create daemon pod.
		nodesNeedingDaemonPods = append(nodesNeedingDaemonPods, node.Name)
	case shouldContinueRunning:
		// If a daemon pod failed, delete it
		// If there's non-daemon pods left on this node, we will create it in the next sync loop
		var daemonPodsRunning []*v1.Pod
		for _, pod := range daemonPods {
			if pod.DeletionTimestamp != nil {
				continue
			}
			if pod.Status.Phase == v1.PodFailed {
				// This is a critical place where DS is often fighting with kubelet that rejects pods.
				// We need to avoid hot looping and backoff.
				backoffKey := failedPodsBackoffKey(ds, node.Name)

				now := dsc.failedPodsBackoff.Clock.Now()
				inBackoff := dsc.failedPodsBackoff.IsInBackOffSinceUpdate(backoffKey, now)
				if inBackoff {
					delay := dsc.failedPodsBackoff.Get(backoffKey)
					logger.V(4).Info("Deleting failed pod on node has been limited by backoff",
						"pod", klog.KObj(pod), "node", klog.KObj(node), "currentDelay", delay)
					dsc.enqueueDaemonSetAfter(ds, delay)
					continue
				}

				dsc.failedPodsBackoff.Next(backoffKey, now)

				msg := fmt.Sprintf("Found failed daemon pod %s/%s on node %s, will try to kill it", pod.Namespace, pod.Name, node.Name)
				logger.V(2).Info("Found failed daemon pod on node, will try to kill it", "pod", klog.KObj(pod), "node", klog.KObj(node))
				// Emit an event so that it's discoverable to users.
				dsc.eventRecorder.Eventf(ds, v1.EventTypeWarning, FailedDaemonPodReason, msg)
				podsToDelete = append(podsToDelete, pod.Name)
			} else if pod.Status.Phase == v1.PodSucceeded {
				msg := fmt.Sprintf("Found succeeded daemon pod %s/%s on node %s, will try to delete it", pod.Namespace, pod.Name, node.Name)
				logger.V(2).Info("Found succeeded daemon pod on node, will try to delete it", "pod", klog.KObj(pod), "node", klog.KObj(node))
				// Emit an event so that it's discoverable to users.
				dsc.eventRecorder.Eventf(ds, v1.EventTypeNormal, SucceededDaemonPodReason, msg)
				podsToDelete = append(podsToDelete, pod.Name)
			} else {
				daemonPodsRunning = append(daemonPodsRunning, pod)
			}
		}

		// When surge is not enabled, if there is more than 1 running pod on a node delete all but the oldest
		if !util.AllowsSurge(ds) {
			if len(daemonPodsRunning) <= 1 {
				// There are no excess pods to be pruned, and no pods to create
				break
			}

			sort.Sort(podByCreationTimestampAndPhase(daemonPodsRunning))
			for i := 1; i < len(daemonPodsRunning); i++ {
				podsToDelete = append(podsToDelete, daemonPodsRunning[i].Name)
			}
			break
		}

		if len(daemonPodsRunning) <= 1 {
			// // There are no excess pods to be pruned
			if len(daemonPodsRunning) == 0 && shouldRun {
				// We are surging so we need to have at least one non-deleted pod on the node
				nodesNeedingDaemonPods = append(nodesNeedingDaemonPods, node.Name)
			}
			break
		}

		// When surge is enabled, we allow 2 pods if and only if the oldest pod matching the current hash state
		// is not ready AND the oldest pod that doesn't match the current hash state is ready. All other pods are
		// deleted. If neither pod is ready, only the one matching the current hash revision is kept.
		var oldestNewPod, oldestOldPod *v1.Pod
		sort.Sort(podByCreationTimestampAndPhase(daemonPodsRunning))
		for _, pod := range daemonPodsRunning {
			if pod.Labels[apps.ControllerRevisionHashLabelKey] == hash {
				if oldestNewPod == nil {
					oldestNewPod = pod
					continue
				}
			} else {
				if oldestOldPod == nil {
					oldestOldPod = pod
					continue
				}
			}
			podsToDelete = append(podsToDelete, pod.Name)
		}
		if oldestNewPod != nil && oldestOldPod != nil {
			switch {
			case !podutil.IsPodReady(oldestOldPod):
				logger.V(5).Info("Pod from daemonset is no longer ready and will be replaced with newer pod", "oldPod", klog.KObj(oldestOldPod), "daemonset", klog.KObj(ds), "newPod", klog.KObj(oldestNewPod))
				podsToDelete = append(podsToDelete, oldestOldPod.Name)
			case podutil.IsPodAvailable(oldestNewPod, ds.Spec.MinReadySeconds, metav1.Time{Time: dsc.failedPodsBackoff.Clock.Now()}):
				logger.V(5).Info("Pod from daemonset is now ready and will replace older pod", "newPod", klog.KObj(oldestNewPod), "daemonset", klog.KObj(ds), "oldPod", klog.KObj(oldestOldPod))
				podsToDelete = append(podsToDelete, oldestOldPod.Name)
			}
		}

	case !shouldContinueRunning && exists:
		// If daemon pod isn't supposed to run on node, but it is, delete all daemon pods on node.
		for _, pod := range daemonPods {
			if pod.DeletionTimestamp != nil {
				continue
			}
			podsToDelete = append(podsToDelete, pod.Name)
		}
	}

	return nodesNeedingDaemonPods, podsToDelete
}

func (dsc *DaemonSetsController) updateDaemonSet(ctx context.Context, ds *apps.DaemonSet, nodeList []*v1.Node, hash, key string, old []*apps.ControllerRevision) error {
	err := dsc.manage(ctx, ds, nodeList, hash)
	if err != nil {
		return err
	}

	// Process rolling updates if we're ready.
	if dsc.expectations.SatisfiedExpectations(klog.FromContext(ctx), key) {
		switch ds.Spec.UpdateStrategy.Type {
		case apps.OnDeleteDaemonSetStrategyType:
		case apps.RollingUpdateDaemonSetStrategyType:
			err = dsc.rollingUpdate(ctx, ds, nodeList, hash)
		}
		if err != nil {
			return err
		}
	}

	err = dsc.cleanupHistory(ctx, ds, old)
	if err != nil {
		return fmt.Errorf("failed to clean up revisions of DaemonSet: %w", err)
	}

	return nil
}

// manage manages the scheduling and running of Pods of ds on nodes.
// After figuring out which nodes should run a Pod of ds but not yet running one and
// which nodes should not run a Pod of ds but currently running one, it calls function
// syncNodes with a list of pods to remove and a list of nodes to run a Pod of ds.
func (dsc *DaemonSetsController) manage(ctx context.Context, ds *apps.DaemonSet, nodeList []*v1.Node, hash string) error {
	// Find out the pods which are created for the nodes by DaemonSet.
	nodeToDaemonPods, err := dsc.getNodesToDaemonPods(ctx, ds, false)
	if err != nil {
		return fmt.Errorf("couldn't get node to daemon pod mapping for daemon set %q: %v", ds.Name, err)
	}

	// For each node, if the node is running the daemon pod but isn't supposed to, kill the daemon
	// pod. If the node is supposed to run the daemon pod, but isn't, create the daemon pod on the node.
	logger := klog.FromContext(ctx)
	var nodesNeedingDaemonPods, podsToDelete []string
	for _, node := range nodeList {
		nodesNeedingDaemonPodsOnNode, podsToDeleteOnNode := dsc.podsShouldBeOnNode(
			logger, node, nodeToDaemonPods, ds, hash)

		nodesNeedingDaemonPods = append(nodesNeedingDaemonPods, nodesNeedingDaemonPodsOnNode...)
		podsToDelete = append(podsToDelete, podsToDeleteOnNode...)
	}

	// Remove unscheduled pods assigned to not existing nodes when daemonset pods are scheduled by scheduler.
	// If node doesn't exist then pods are never scheduled and can't be deleted by PodGCController.
	podsToDelete = append(podsToDelete, getUnscheduledPodsWithoutNode(nodeList, nodeToDaemonPods)...)

	// Label new pods using the hash label value of the current history when creating them
	if err = dsc.syncNodes(ctx, ds, podsToDelete, nodesNeedingDaemonPods, hash); err != nil {
		return err
	}

	return nil
}

// syncNodes deletes given pods and creates new daemon set pods on the given nodes
// returns slice with errors if any
func (dsc *DaemonSetsController) syncNodes(ctx context.Context, ds *apps.DaemonSet, podsToDelete, nodesNeedingDaemonPods []string, hash string) error {
	// We need to set expectations before creating/deleting pods to avoid race conditions.
	logger := klog.FromContext(ctx)
	dsKey, err := controller.KeyFunc(ds)
	if err != nil {
		return fmt.Errorf("couldn't get key for object %#v: %v", ds, err)
	}

	createDiff := len(nodesNeedingDaemonPods)
	deleteDiff := len(podsToDelete)

	if createDiff > dsc.burstReplicas {
		createDiff = dsc.burstReplicas
	}
	if deleteDiff > dsc.burstReplicas {
		deleteDiff = dsc.burstReplicas
	}

	dsc.expectations.SetExpectations(logger, dsKey, createDiff, deleteDiff)

	// error channel to communicate back failures.  make the buffer big enough to avoid any blocking
	errCh := make(chan error, createDiff+deleteDiff)

	logger.V(4).Info("Nodes needing daemon pods for daemon set, creating", "daemonset", klog.KObj(ds), "needCount", nodesNeedingDaemonPods, "createCount", createDiff)
	createWait := sync.WaitGroup{}
	// If the returned error is not nil we have a parse error.
	// The controller handles this via the hash.
	generation, err := util.GetTemplateGeneration(ds)
	if err != nil {
		generation = nil
	}
	template := util.CreatePodTemplate(ds.Spec.Template, generation, hash)
	// Batch the pod creates. Batch sizes start at SlowStartInitialBatchSize
	// and double with each successful iteration in a kind of "slow start".
	// This handles attempts to start large numbers of pods that would
	// likely all fail with the same error. For example a project with a
	// low quota that attempts to create a large number of pods will be
	// prevented from spamming the API service with the pod create requests
	// after one of its pods fails.  Conveniently, this also prevents the
	// event spam that those failures would generate.
	batchSize := min(createDiff, controller.SlowStartInitialBatchSize)
	for pos := 0; createDiff > pos; batchSize, pos = min(2*batchSize, createDiff-(pos+batchSize)), pos+batchSize {
		errorCount := len(errCh)
		createWait.Add(batchSize)
		for i := pos; i < pos+batchSize; i++ {
			go func(ix int) {
				defer createWait.Done()

				podTemplate := template.DeepCopy()
				// The pod's NodeAffinity will be updated to make sure the Pod is bound
				// to the target node by default scheduler. It is safe to do so because there
				// should be no conflicting node affinity with the target node.
				podTemplate.Spec.Affinity = util.ReplaceDaemonSetPodNodeNameNodeAffinity(
					podTemplate.Spec.Affinity, nodesNeedingDaemonPods[ix])

				err := dsc.podControl.CreatePods(ctx, ds.Namespace, podTemplate,
					ds, metav1.NewControllerRef(ds, controllerKind))

				if err != nil {
					if apierrors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
						// If the namespace is being torn down, we can safely ignore
						// this error since all subsequent creations will fail.
						return
					}
				}
				if err != nil {
					logger.V(2).Info("Failed creation, decrementing expectations for daemon set", "daemonset", klog.KObj(ds))
					dsc.expectations.CreationObserved(logger, dsKey)
					errCh <- err
					utilruntime.HandleError(err)
				}
			}(i)
		}
		createWait.Wait()
		// any skipped pods that we never attempted to start shouldn't be expected.
		skippedPods := createDiff - (batchSize + pos)
		if errorCount < len(errCh) && skippedPods > 0 {
			logger.V(2).Info("Slow-start failure. Skipping creation pods, decrementing expectations for daemon set", "skippedPods", skippedPods, "daemonset", klog.KObj(ds))
			dsc.expectations.LowerExpectations(logger, dsKey, skippedPods, 0)
			// The skipped pods will be retried later. The next controller resync will
			// retry the slow start process.
			break
		}
	}

	logger.V(4).Info("Pods to delete for daemon set, deleting", "daemonset", klog.KObj(ds), "toDeleteCount", podsToDelete, "deleteCount", deleteDiff)
	deleteWait := sync.WaitGroup{}
	deleteWait.Add(deleteDiff)
	for i := 0; i < deleteDiff; i++ {
		go func(ix int) {
			defer deleteWait.Done()
			if err := dsc.podControl.DeletePod(ctx, ds.Namespace, podsToDelete[ix], ds); err != nil {
				dsc.expectations.DeletionObserved(logger, dsKey)
				if !apierrors.IsNotFound(err) {
					logger.V(2).Info("Failed deletion, decremented expectations for daemon set", "daemonset", klog.KObj(ds))
					errCh <- err
					utilruntime.HandleError(err)
				}
			}
		}(i)
	}
	deleteWait.Wait()

	// collect errors if any for proper reporting/retry logic in the controller
	errors := []error{}
	close(errCh)
	for err := range errCh {
		errors = append(errors, err)
	}
	return utilerrors.NewAggregate(errors)
}

func storeDaemonSetStatus(
	ctx context.Context,
	dsClient unversionedapps.DaemonSetInterface,
	ds *apps.DaemonSet, desiredNumberScheduled,
	currentNumberScheduled,
	numberMisscheduled,
	numberReady,
	updatedNumberScheduled,
	numberAvailable,
	numberUnavailable int,
	updateObservedGen bool) error {
	if int(ds.Status.DesiredNumberScheduled) == desiredNumberScheduled &&
		int(ds.Status.CurrentNumberScheduled) == currentNumberScheduled &&
		int(ds.Status.NumberMisscheduled) == numberMisscheduled &&
		int(ds.Status.NumberReady) == numberReady &&
		int(ds.Status.UpdatedNumberScheduled) == updatedNumberScheduled &&
		int(ds.Status.NumberAvailable) == numberAvailable &&
		int(ds.Status.NumberUnavailable) == numberUnavailable &&
		ds.Status.ObservedGeneration >= ds.Generation {
		return nil
	}

	toUpdate := ds.DeepCopy()

	var updateErr, getErr error
	for i := 0; ; i++ {
		if updateObservedGen {
			toUpdate.Status.ObservedGeneration = ds.Generation
		}
		toUpdate.Status.DesiredNumberScheduled = int32(desiredNumberScheduled)
		toUpdate.Status.CurrentNumberScheduled = int32(currentNumberScheduled)
		toUpdate.Status.NumberMisscheduled = int32(numberMisscheduled)
		toUpdate.Status.NumberReady = int32(numberReady)
		toUpdate.Status.UpdatedNumberScheduled = int32(updatedNumberScheduled)
		toUpdate.Status.NumberAvailable = int32(numberAvailable)
		toUpdate.Status.NumberUnavailable = int32(numberUnavailable)

		if _, updateErr = dsClient.UpdateStatus(ctx, toUpdate, metav1.UpdateOptions{}); updateErr == nil {
			return nil
		}

		// Stop retrying if we exceed statusUpdateRetries - the DaemonSet will be requeued with a rate limit.
		if i >= StatusUpdateRetries {
			break
		}
		// Update the set with the latest resource version for the next poll
		if toUpdate, getErr = dsClient.Get(ctx, ds.Name, metav1.GetOptions{}); getErr != nil {
			// If the GET fails we can't trust status.Replicas anymore. This error
			// is bound to be more interesting than the update failure.
			return getErr
		}
	}
	return updateErr
}

func (dsc *DaemonSetsController) updateDaemonSetStatus(ctx context.Context, ds *apps.DaemonSet, nodeList []*v1.Node, hash string, updateObservedGen bool) error {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Updating daemon set status")
	nodeToDaemonPods, err := dsc.getNodesToDaemonPods(ctx, ds, false)
	if err != nil {
		return fmt.Errorf("couldn't get node to daemon pod mapping for daemon set %q: %v", ds.Name, err)
	}

	var desiredNumberScheduled, currentNumberScheduled, numberMisscheduled, numberReady, updatedNumberScheduled, numberAvailable int
	now := dsc.failedPodsBackoff.Clock.Now()
	for _, node := range nodeList {
		shouldRun, _ := NodeShouldRunDaemonPod(node, ds)
		scheduled := len(nodeToDaemonPods[node.Name]) > 0

		if shouldRun {
			desiredNumberScheduled++
			if !scheduled {
				continue
			}

			currentNumberScheduled++
			// Sort the daemon pods by creation time, so that the oldest is first.
			daemonPods, _ := nodeToDaemonPods[node.Name]
			sort.Sort(podByCreationTimestampAndPhase(daemonPods))
			pod := daemonPods[0]
			if podutil.IsPodReady(pod) {
				numberReady++
				if podutil.IsPodAvailable(pod, ds.Spec.MinReadySeconds, metav1.Time{Time: now}) {
					numberAvailable++
				}
			}
			// If the returned error is not nil we have a parse error.
			// The controller handles this via the hash.
			generation, err := util.GetTemplateGeneration(ds)
			if err != nil {
				generation = nil
			}
			if util.IsPodUpdated(pod, hash, generation) {
				updatedNumberScheduled++
			}
		} else {
			if scheduled {
				numberMisscheduled++
			}
		}
	}
	numberUnavailable := desiredNumberScheduled - numberAvailable

	err = storeDaemonSetStatus(ctx, dsc.kubeClient.AppsV1().DaemonSets(ds.Namespace), ds, desiredNumberScheduled, currentNumberScheduled, numberMisscheduled, numberReady, updatedNumberScheduled, numberAvailable, numberUnavailable, updateObservedGen)
	if err != nil {
		return fmt.Errorf("error storing status for daemon set %#v: %w", ds, err)
	}

	// Resync the DaemonSet after MinReadySeconds as a last line of defense to guard against clock-skew.
	if ds.Spec.MinReadySeconds > 0 && numberReady != numberAvailable {
		dsc.enqueueDaemonSetAfter(ds, time.Duration(ds.Spec.MinReadySeconds)*time.Second)
	}
	return nil
}

func (dsc *DaemonSetsController) syncDaemonSet(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := dsc.failedPodsBackoff.Clock.Now()

	defer func() {
		logger.V(4).Info("Finished syncing daemon set", "daemonset", key, "time", dsc.failedPodsBackoff.Clock.Now().Sub(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	ds, err := dsc.dsLister.DaemonSets(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logger.V(3).Info("Daemon set has been deleted", "daemonset", key)
		dsc.expectations.DeleteExpectations(logger, key)
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to retrieve ds %v from store: %v", key, err)
	}

	nodeList, err := dsc.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("couldn't get list of nodes when syncing daemon set %#v: %v", ds, err)
	}

	everything := metav1.LabelSelector{}
	if reflect.DeepEqual(ds.Spec.Selector, &everything) {
		dsc.eventRecorder.Eventf(ds, v1.EventTypeWarning, SelectingAllReason, "This daemon set is selecting all pods. A non-empty selector is required.")
		return nil
	}

	// Don't process a daemon set until all its creations and deletions have been processed.
	// For example if daemon set foo asked for 3 new daemon pods in the previous call to manage,
	// then we do not want to call manage on foo until the daemon pods have been created.
	dsKey, err := controller.KeyFunc(ds)
	if err != nil {
		return fmt.Errorf("couldn't get key for object %#v: %v", ds, err)
	}

	// If the DaemonSet is being deleted (either by foreground deletion or
	// orphan deletion), we cannot be sure if the DaemonSet history objects
	// it owned still exist -- those history objects can either be deleted
	// or orphaned. Garbage collector doesn't guarantee that it will delete
	// DaemonSet pods before deleting DaemonSet history objects, because
	// DaemonSet history doesn't own DaemonSet pods. We cannot reliably
	// calculate the status of a DaemonSet being deleted. Therefore, return
	// here without updating status for the DaemonSet being deleted.
	if ds.DeletionTimestamp != nil {
		return nil
	}

	// Construct histories of the DaemonSet, and get the hash of current history
	cur, old, err := dsc.constructHistory(ctx, ds)
	if err != nil {
		return fmt.Errorf("failed to construct revisions of DaemonSet: %v", err)
	}
	hash := cur.Labels[apps.DefaultDaemonSetUniqueLabelKey]

	if !dsc.expectations.SatisfiedExpectations(logger, dsKey) {
		// Only update status. Don't raise observedGeneration since controller didn't process object of that generation.
		return dsc.updateDaemonSetStatus(ctx, ds, nodeList, hash, false)
	}

	err = dsc.updateDaemonSet(ctx, ds, nodeList, hash, dsKey, old)
	statusErr := dsc.updateDaemonSetStatus(ctx, ds, nodeList, hash, true)
	switch {
	case err != nil && statusErr != nil:
		// If there was an error, and we failed to update status,
		// log it and return the original error.
		logger.Error(statusErr, "Failed to update status", "daemonSet", klog.KObj(ds))
		return err
	case err != nil:
		return err
	case statusErr != nil:
		return statusErr
	}

	return nil
}

// NodeShouldRunDaemonPod checks a set of preconditions against a (node,daemonset) and returns a
// summary. Returned booleans are:
//   - shouldRun:
//     Returns true when a daemonset should run on the node if a daemonset pod is not already
//     running on that node.
//   - shouldContinueRunning:
//     Returns true when a daemonset should continue running on a node if a daemonset pod is already
//     running on that node.
func NodeShouldRunDaemonPod(node *v1.Node, ds *apps.DaemonSet) (bool, bool) {
	pod := NewPod(ds, node.Name)

	// If the daemon set specifies a node name, check that it matches with node.Name.
	if !(ds.Spec.Template.Spec.NodeName == "" || ds.Spec.Template.Spec.NodeName == node.Name) {
		return false, false
	}

	taints := node.Spec.Taints
	fitsNodeName := len(pod.Spec.NodeName) == 0 || pod.Spec.NodeName == node.Name
	if !fitsNodeName {
		return false, false
	}

	fitsNodeName, fitsNodeSelector, fitsNodeAffinity, fitsTaints := predicates(pod, node, taints)

	if !fitsNodeName || !fitsNodeSelector {
		return false, false
	}

	if !fitsTaints {
		// Scheduled daemon pods should continue running if they tolerate NoExecute taint.
		_, hasUntoleratedTaint := v1helper.FindMatchingUntoleratedTaint(taints, pod.Spec.Tolerations, func(t *v1.Taint) bool {
			return t.Effect == v1.TaintEffectNoExecute
		})
		return false, !hasUntoleratedTaint
	}

	if !fitsNodeAffinity {
		// IgnoredDuringExecution means that if the node labels change after Kubernetes schedules the Pod, the Pod continues to run.
		return false, true
	}

	return true, true
}

// predicates checks if a DaemonSet's pod can run on a node.
func predicates(pod *v1.Pod, node *v1.Node, taints []v1.Taint) (fitsNodeName, fitsNodeSelector, fitsNodeAffinity, fitsTaints bool) {
	fitsNodeName = len(pod.Spec.NodeName) == 0 || pod.Spec.NodeName == node.Name

	if len(pod.Spec.NodeSelector) > 0 {
		selector := labels.SelectorFromSet(pod.Spec.NodeSelector)
		fitsNodeSelector = selector.Matches(labels.Set(node.Labels))
	} else {
		fitsNodeSelector = true
	}

	if pod.Spec.Affinity != nil &&
		pod.Spec.Affinity.NodeAffinity != nil &&
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		affinity := nodeaffinity.NewLazyErrorNodeSelector(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
		fitsNodeAffinity, _ = affinity.Match(node)
	} else {
		fitsNodeAffinity = true
	}

	// Ignore parsing errors for backwards compatibility.
	_, hasUntoleratedTaint := v1helper.FindMatchingUntoleratedTaint(taints, pod.Spec.Tolerations, func(t *v1.Taint) bool {
		return t.Effect == v1.TaintEffectNoExecute || t.Effect == v1.TaintEffectNoSchedule
	})
	fitsTaints = !hasUntoleratedTaint
	return
}

// NewPod creates a new pod
func NewPod(ds *apps.DaemonSet, nodeName string) *v1.Pod {
	newPod := &v1.Pod{Spec: ds.Spec.Template.Spec, ObjectMeta: ds.Spec.Template.ObjectMeta}
	newPod.Namespace = ds.Namespace
	newPod.Spec.NodeName = nodeName

	// Added default tolerations for DaemonSet pods.
	util.AddOrUpdateDaemonPodTolerations(&newPod.Spec)

	return newPod
}

type podByCreationTimestampAndPhase []*v1.Pod

func (o podByCreationTimestampAndPhase) Len() int      { return len(o) }
func (o podByCreationTimestampAndPhase) Swap(i, j int) { o[i], o[j] = o[j], o[i] }

func (o podByCreationTimestampAndPhase) Less(i, j int) bool {
	// Scheduled Pod first
	if len(o[i].Spec.NodeName) != 0 && len(o[j].Spec.NodeName) == 0 {
		return true
	}

	if len(o[i].Spec.NodeName) == 0 && len(o[j].Spec.NodeName) != 0 {
		return false
	}

	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

func failedPodsBackoffKey(ds *apps.DaemonSet, nodeName string) string {
	return fmt.Sprintf("%s/%d/%s", ds.UID, ds.Status.ObservedGeneration, nodeName)
}

// getUnscheduledPodsWithoutNode returns list of unscheduled pods assigned to not existing nodes.
// Returned pods can't be deleted by PodGCController so they should be deleted by DaemonSetController.
func getUnscheduledPodsWithoutNode(runningNodesList []*v1.Node, nodeToDaemonPods map[string][]*v1.Pod) []string {
	var results []string
	isNodeRunning := make(map[string]bool, len(runningNodesList))
	for _, node := range runningNodesList {
		isNodeRunning[node.Name] = true
	}

	for n, pods := range nodeToDaemonPods {
		if isNodeRunning[n] {
			continue
		}
		for _, pod := range pods {
			if len(pod.Spec.NodeName) == 0 {
				results = append(results, pod.Name)
			}
		}
	}

	return results
}
