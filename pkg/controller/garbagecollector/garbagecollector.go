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

package garbagecollector

import (
	"context"
	goerrors "errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	clientset "k8s.io/client-go/kubernetes" // import known versions
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/controller-manager/controller"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/klog/v2"
	c "k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/garbagecollector/metrics"
)

// ResourceResyncTime defines the resync period of the garbage collector's informers.
const ResourceResyncTime time.Duration = 0

// GarbageCollector runs reflectors to watch for changes of managed API
// objects, funnels the results to a single-threaded dependencyGraphBuilder,
// which builds a graph caching the dependencies among objects. Triggered by the
// graph changes, the dependencyGraphBuilder enqueues objects that can
// potentially be garbage-collected to the `attemptToDelete` queue, and enqueues
// objects whose dependents need to be orphaned to the `attemptToOrphan` queue.
// The GarbageCollector has workers who consume these two queues, send requests
// to the API server to delete/update the objects accordingly.
// Note that having the dependencyGraphBuilder notify the garbage collector
// ensures that the garbage collector operates with a graph that is at least as
// up to date as the notification is sent.
type GarbageCollector struct {
	restMapper     meta.ResettableRESTMapper
	metadataClient metadata.Interface
	// garbage collector attempts to delete the items in attemptToDelete queue when the time is ripe.
	attemptToDelete workqueue.TypedRateLimitingInterface[*node]
	// garbage collector attempts to orphan the dependents of the items in the attemptToOrphan queue, then deletes the items.
	attemptToOrphan        workqueue.TypedRateLimitingInterface[*node]
	dependencyGraphBuilder *GraphBuilder
	// GC caches the owners that do not exist according to the API server.
	absentOwnerCache *ReferenceCache

	kubeClient       clientset.Interface
	eventBroadcaster record.EventBroadcaster
}

var _ controller.Interface = (*GarbageCollector)(nil)
var _ controller.Debuggable = (*GarbageCollector)(nil)

// NewGarbageCollector creates a new GarbageCollector.
func NewGarbageCollector(
	ctx context.Context,
	kubeClient clientset.Interface,
	metadataClient metadata.Interface,
	mapper meta.ResettableRESTMapper,
	ignoredResources map[schema.GroupResource]struct{},
	sharedInformers informerfactory.InformerFactory,
	informersStarted <-chan struct{},
) (*GarbageCollector, error) {
	graphBuilder := NewDependencyGraphBuilder(ctx, metadataClient, mapper, ignoredResources, sharedInformers, informersStarted)
	return NewComposedGarbageCollector(ctx, kubeClient, metadataClient, mapper, graphBuilder)
}

func NewComposedGarbageCollector(
	ctx context.Context,
	kubeClient clientset.Interface,
	metadataClient metadata.Interface,
	mapper meta.ResettableRESTMapper,
	graphBuilder *GraphBuilder,
) (*GarbageCollector, error) {
	attemptToDelete, attemptToOrphan, absentOwnerCache := graphBuilder.GetGraphResources()

	gc := &GarbageCollector{
		metadataClient:         metadataClient,
		restMapper:             mapper,
		attemptToDelete:        attemptToDelete,
		attemptToOrphan:        attemptToOrphan,
		absentOwnerCache:       absentOwnerCache,
		kubeClient:             kubeClient,
		eventBroadcaster:       graphBuilder.eventBroadcaster,
		dependencyGraphBuilder: graphBuilder,
	}

	metrics.Register()

	return gc, nil
}

// resyncMonitors starts or stops resource monitors as needed to ensure that all
// (and only) those resources present in the map are monitored.
func (gc *GarbageCollector) resyncMonitors(logger klog.Logger, deletableResources map[schema.GroupVersionResource]struct{}) error {
	if err := gc.dependencyGraphBuilder.syncMonitors(logger, deletableResources); err != nil {
		return err
	}
	gc.dependencyGraphBuilder.startMonitors(logger)
	return nil
}

// Run starts garbage collector workers.
func (gc *GarbageCollector) Run(ctx context.Context, workers int, initialSyncTimeout time.Duration) {
	defer utilruntime.HandleCrash()
	defer gc.attemptToDelete.ShutDown()
	defer gc.attemptToOrphan.ShutDown()
	defer gc.dependencyGraphBuilder.graphChanges.ShutDown()

	// Start events processing pipeline.
	gc.eventBroadcaster.StartStructuredLogging(3)
	gc.eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: gc.kubeClient.CoreV1().Events("")})
	defer gc.eventBroadcaster.Shutdown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting controller", "controller", "garbagecollector")
	defer logger.Info("Shutting down controller", "controller", "garbagecollector")

	go gc.dependencyGraphBuilder.Run(ctx)

	if !cache.WaitForNamedCacheSync("garbage collector", waitForStopOrTimeout(ctx.Done(), initialSyncTimeout), func() bool {
		return gc.dependencyGraphBuilder.IsSynced(logger)
	}) {
		logger.Info("Garbage collector: not all resource monitors could be synced, proceeding anyways")
	} else {
		logger.Info("Garbage collector: all resource monitors have synced")
	}

	logger.Info("Proceeding to collect garbage")

	// gc workers
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, gc.runAttemptToDeleteWorker, 1*time.Second)
		go wait.Until(func() { gc.runAttemptToOrphanWorker(logger) }, 1*time.Second, ctx.Done())
	}

	<-ctx.Done()
}

// Sync periodically resyncs the garbage collector when new resources are
// observed from discovery. When new resources are detected, it will reset
// gc.restMapper, and resync the monitors.
//
// Note that discoveryClient should NOT be shared with gc.restMapper, otherwise
// the mapper's underlying discovery client will be unnecessarily reset during
// the course of detecting new resources.
func (gc *GarbageCollector) Sync(ctx context.Context, discoveryClient discovery.ServerResourcesInterface, period time.Duration) {
	oldResources := make(map[schema.GroupVersionResource]struct{})
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		logger := klog.FromContext(ctx)

		// Get the current resource list from discovery.
		newResources, err := GetDeletableResources(logger, discoveryClient)

		if len(newResources) == 0 {
			logger.V(2).Info("no resources reported by discovery, skipping garbage collector sync")
			metrics.GarbageCollectorResourcesSyncError.Inc()
			return
		}
		if groupLookupFailures, isLookupFailure := discovery.GroupDiscoveryFailedErrorGroups(err); isLookupFailure {
			// In partial discovery cases, preserve existing synced informers for resources in the failed groups, so resyncMonitors will only add informers for newly seen resources
			for k, v := range oldResources {
				if _, failed := groupLookupFailures[k.GroupVersion()]; failed && gc.dependencyGraphBuilder.IsResourceSynced(k) {
					newResources[k] = v
				}
			}
		}

		// Decide whether discovery has reported a change.
		if reflect.DeepEqual(oldResources, newResources) {
			logger.V(5).Info("no resource updates from discovery, skipping garbage collector sync")
			return
		}

		logger.V(2).Info(
			"syncing garbage collector with updated resources from discovery",
			"diff", printDiff(oldResources, newResources),
		)

		// Resetting the REST mapper will also invalidate the underlying discovery
		// client. This is a leaky abstraction and assumes behavior about the REST
		// mapper, but we'll deal with it for now.
		gc.restMapper.Reset()
		logger.V(4).Info("reset restmapper")

		// Perform the monitor resync and wait for controllers to report cache sync.
		//
		// NOTE: It's possible that newResources will diverge from the resources
		// discovered by restMapper during the call to Reset, since they are
		// distinct discovery clients invalidated at different times. For example,
		// newResources may contain resources not returned in the restMapper's
		// discovery call if the resources appeared in-between the calls. In that
		// case, the restMapper will fail to map some of newResources until the next
		// attempt.
		if err := gc.resyncMonitors(logger, newResources); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to sync resource monitors: %w", err))
			metrics.GarbageCollectorResourcesSyncError.Inc()
			return
		}
		logger.V(4).Info("resynced monitors")

		// gc worker no longer waits for cache to be synced, but we will keep the periodical check to provide logs & metrics
		cacheSynced := cache.WaitForNamedCacheSync("garbage collector", waitForStopOrTimeout(ctx.Done(), period), func() bool {
			return gc.dependencyGraphBuilder.IsSynced(logger)
		})
		if cacheSynced {
			logger.V(2).Info("synced garbage collector")
		} else {
			utilruntime.HandleError(fmt.Errorf("timed out waiting for dependency graph builder sync during GC sync"))
			metrics.GarbageCollectorResourcesSyncError.Inc()
		}

		// Finally, keep track of our new resource monitor state.
		// Monitors where the cache sync times out are still tracked here as
		// subsequent runs should stop them if their resources were removed.
		oldResources = newResources
	}, period)
}

// printDiff returns a human-readable summary of what resources were added and removed
func printDiff(oldResources, newResources map[schema.GroupVersionResource]struct{}) string {
	removed := sets.NewString()
	for oldResource := range oldResources {
		if _, ok := newResources[oldResource]; !ok {
			removed.Insert(fmt.Sprintf("%+v", oldResource))
		}
	}
	added := sets.NewString()
	for newResource := range newResources {
		if _, ok := oldResources[newResource]; !ok {
			added.Insert(fmt.Sprintf("%+v", newResource))
		}
	}
	return fmt.Sprintf("added: %v, removed: %v", added.List(), removed.List())
}

// waitForStopOrTimeout returns a stop channel that closes when the provided stop channel closes or when the specified timeout is reached
func waitForStopOrTimeout(stopCh <-chan struct{}, timeout time.Duration) <-chan struct{} {
	stopChWithTimeout := make(chan struct{})
	go func() {
		select {
		case <-stopCh:
		case <-time.After(timeout):
		}
		close(stopChWithTimeout)
	}()
	return stopChWithTimeout
}

// IsSynced returns true if dependencyGraphBuilder is synced.
func (gc *GarbageCollector) IsSynced(logger klog.Logger) bool {
	return gc.dependencyGraphBuilder.IsSynced(logger)
}

func (gc *GarbageCollector) runAttemptToDeleteWorker(ctx context.Context) {
	for gc.processAttemptToDeleteWorker(ctx) {
	}
}

var enqueuedVirtualDeleteEventErr = goerrors.New("enqueued virtual delete event")

var namespacedOwnerOfClusterScopedObjectErr = goerrors.New("cluster-scoped objects cannot refer to namespaced owners")

func (gc *GarbageCollector) processAttemptToDeleteWorker(ctx context.Context) bool {
	item, quit := gc.attemptToDelete.Get()
	if quit {
		return false
	}
	defer gc.attemptToDelete.Done(item)

	action := gc.attemptToDeleteWorker(ctx, item)
	switch action {
	case forgetItem:
		gc.attemptToDelete.Forget(item)
	case requeueItem:
		gc.attemptToDelete.AddRateLimited(item)
	}

	return true
}

type workQueueItemAction int

const (
	requeueItem = iota
	forgetItem
)

func (gc *GarbageCollector) attemptToDeleteWorker(ctx context.Context, item interface{}) workQueueItemAction {
	n, ok := item.(*node)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expect *node, got %#v", item))
		return forgetItem
	}

	logger := klog.FromContext(ctx)

	if !n.isObserved() {
		nodeFromGraph, existsInGraph := gc.dependencyGraphBuilder.uidToNode.Read(n.identity.UID)
		if !existsInGraph {
			// this can happen if attemptToDelete loops on a requeued virtual node because attemptToDeleteItem returned an error,
			// and in the meantime a deletion of the real object associated with that uid was observed
			logger.V(5).Info("item no longer in the graph, skipping attemptToDeleteItem", "item", n.identity)
			return forgetItem
		}
		if nodeFromGraph.isObserved() {
			// this can happen if attemptToDelete loops on a requeued virtual node because attemptToDeleteItem returned an error,
			// and in the meantime the real object associated with that uid was observed
			logger.V(5).Info("item no longer virtual in the graph, skipping attemptToDeleteItem on virtual node", "item", n.identity)
			return forgetItem
		}
	}

	err := gc.attemptToDeleteItem(ctx, n)
	if err == enqueuedVirtualDeleteEventErr {
		// a virtual event was produced and will be handled by processGraphChanges, no need to requeue this node
		return forgetItem
	} else if err == namespacedOwnerOfClusterScopedObjectErr {
		// a cluster-scoped object referring to a namespaced owner is an error that will not resolve on retry, no need to requeue this node
		return forgetItem
	} else if err != nil {
		if _, ok := err.(*restMappingError); ok {
			// There are at least two ways this can happen:
			// 1. The reference is to an object of a custom type that has not yet been
			//    recognized by gc.restMapper (this is a transient error).
			// 2. The reference is to an invalid group/version. We don't currently
			//    have a way to distinguish this from a valid type we will recognize
			//    after the next discovery sync.
			// For now, record the error and retry.
			logger.V(5).Error(err, "error syncing item", "item", n.identity)
		} else {
			utilruntime.HandleError(fmt.Errorf("error syncing item %s: %v", n, err))
		}
		// retry if garbage collection of an object failed.
		return requeueItem
	} else if !n.isObserved() {
		// requeue if item hasn't been observed via an informer event yet.
		// otherwise a virtual node for an item added AND removed during watch reestablishment can get stuck in the graph and never removed.
		// see https://issue.k8s.io/56121
		logger.V(5).Info("item hasn't been observed via informer yet", "item", n.identity)
		return requeueItem
	}

	return forgetItem
}

// isDangling check if a reference is pointing to an object that doesn't exist.
// If isDangling looks up the referenced object at the API server, it also
// returns its latest state.
func (gc *GarbageCollector) isDangling(ctx context.Context, reference metav1.OwnerReference, item *node) (
	dangling bool, owner *metav1.PartialObjectMetadata, err error) {

	logger := klog.FromContext(ctx)
	// check for recorded absent cluster-scoped parent
	absentOwnerCacheKey := objectReference{OwnerReference: ownerReferenceCoordinates(reference)}
	if gc.absentOwnerCache.Has(absentOwnerCacheKey) {
		logger.V(5).Info("according to the absentOwnerCache, item's owner does not exist",
			"item", item.identity,
			"owner", reference,
		)
		return true, nil, nil
	}

	// check for recorded absent namespaced parent
	absentOwnerCacheKey.Namespace = item.identity.Namespace
	if gc.absentOwnerCache.Has(absentOwnerCacheKey) {
		logger.V(5).Info("according to the absentOwnerCache, item's owner does not exist in namespace",
			"item", item.identity,
			"owner", reference,
		)
		return true, nil, nil
	}

	// TODO: we need to verify the reference resource is supported by the
	// system. If it's not a valid resource, the garbage collector should i)
	// ignore the reference when decide if the object should be deleted, and
	// ii) should update the object to remove such references. This is to
	// prevent objects having references to an old resource from being
	// deleted during a cluster upgrade.
	resource, namespaced, err := gc.apiResource(reference.APIVersion, reference.Kind)
	if err != nil {
		return false, nil, err
	}
	if !namespaced {
		absentOwnerCacheKey.Namespace = ""
	}

	if len(item.identity.Namespace) == 0 && namespaced {
		// item is a cluster-scoped object referring to a namespace-scoped owner, which is not valid.
		// return a marker error, rather than retrying on the lookup failure forever.
		logger.V(2).Info("item is cluster-scoped, but refers to a namespaced owner",
			"item", item.identity,
			"owner", reference,
		)
		return false, nil, namespacedOwnerOfClusterScopedObjectErr
	}

	// TODO: It's only necessary to talk to the API server if the owner node
	// is a "virtual" node. The local graph could lag behind the real
	// status, but in practice, the difference is small.
	owner, err = gc.metadataClient.Resource(resource).Namespace(resourceDefaultNamespace(namespaced, item.identity.Namespace)).Get(ctx, reference.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		gc.absentOwnerCache.Add(absentOwnerCacheKey)
		logger.V(5).Info("item's owner is not found",
			"item", item.identity,
			"owner", reference,
		)
		return true, nil, nil
	case err != nil:
		return false, nil, err
	}

	if owner.GetUID() != reference.UID {
		logger.V(5).Info("item's owner is not found, UID mismatch",
			"item", item.identity,
			"owner", reference,
		)
		gc.absentOwnerCache.Add(absentOwnerCacheKey)
		return true, nil, nil
	}
	return false, owner, nil
}

// classify the latestReferences to three categories:
// solid: the owner exists, and is not "waitingForDependentsDeletion"
// dangling: the owner does not exist
// waitingForDependentsDeletion: the owner exists, its deletionTimestamp is non-nil, and it has
// FinalizerDeletingDependents
// This function communicates with the server.
func (gc *GarbageCollector) classifyReferences(ctx context.Context, item *node, latestReferences []metav1.OwnerReference) (
	solid, dangling, waitingForDependentsDeletion []metav1.OwnerReference, err error) {
	for _, reference := range latestReferences {
		isDangling, owner, err := gc.isDangling(ctx, reference, item)
		if err != nil {
			return nil, nil, nil, err
		}
		if isDangling {
			dangling = append(dangling, reference)
			continue
		}

		ownerAccessor, err := meta.Accessor(owner)
		if err != nil {
			return nil, nil, nil, err
		}
		if ownerAccessor.GetDeletionTimestamp() != nil && hasDeleteDependentsFinalizer(ownerAccessor) {
			waitingForDependentsDeletion = append(waitingForDependentsDeletion, reference)
		} else {
			solid = append(solid, reference)
		}
	}
	return solid, dangling, waitingForDependentsDeletion, nil
}

func ownerRefsToUIDs(refs []metav1.OwnerReference) []types.UID {
	var ret []types.UID
	for _, ref := range refs {
		ret = append(ret, ref.UID)
	}
	return ret
}

// attemptToDeleteItem looks up the live API object associated with the node,
// and issues a delete IFF the uid matches, the item is not blocked on deleting dependents,
// and all owner references are dangling.
//
// if the API get request returns a NotFound error, or the retrieved item's uid does not match,
// a virtual delete event for the node is enqueued and enqueuedVirtualDeleteEventErr is returned.
func (gc *GarbageCollector) attemptToDeleteItem(ctx context.Context, item *node) error {
	logger := klog.FromContext(ctx)

	logger.V(2).Info("Processing item",
		"item", item.identity,
		"virtual", !item.isObserved(),
	)

	// "being deleted" is an one-way trip to the final deletion. We'll just wait for the final deletion, and then process the object's dependents.
	if item.isBeingDeleted() && !item.isDeletingDependents() {
		logger.V(5).Info("processing item returned at once, because its DeletionTimestamp is non-nil",
			"item", item.identity,
		)
		return nil
	}
	// TODO: It's only necessary to talk to the API server if this is a
	// "virtual" node. The local graph could lag behind the real status, but in
	// practice, the difference is small.
	latest, err := gc.getObject(item.identity)
	switch {
	case errors.IsNotFound(err):
		// the GraphBuilder can add "virtual" node for an owner that doesn't
		// exist yet, so we need to enqueue a virtual Delete event to remove
		// the virtual node from GraphBuilder.uidToNode.
		logger.V(5).Info("item not found, generating a virtual delete event",
			"item", item.identity,
		)
		gc.dependencyGraphBuilder.enqueueVirtualDeleteEvent(item.identity)
		return enqueuedVirtualDeleteEventErr
	case err != nil:
		return err
	}

	if latest.GetUID() != item.identity.UID {
		logger.V(5).Info("UID doesn't match, item not found, generating a virtual delete event",
			"item", item.identity,
		)
		gc.dependencyGraphBuilder.enqueueVirtualDeleteEvent(item.identity)
		return enqueuedVirtualDeleteEventErr
	}

	// TODO: attemptToOrphanWorker() routine is similar. Consider merging
	// attemptToOrphanWorker() into attemptToDeleteItem() as well.
	if item.isDeletingDependents() {
		return gc.processDeletingDependentsItem(logger, item)
	}

	// compute if we should delete the item
	ownerReferences := latest.GetOwnerReferences()
	if len(ownerReferences) == 0 {
		logger.V(2).Info("item doesn't have an owner, continue on next item",
			"item", item.identity,
		)
		return nil
	}

	solid, dangling, waitingForDependentsDeletion, err := gc.classifyReferences(ctx, item, ownerReferences)
	if err != nil {
		return err
	}
	logger.V(5).Info("classify item's references",
		"item", item.identity,
		"solid", solid,
		"dangling", dangling,
		"waitingForDependentsDeletion", waitingForDependentsDeletion,
	)

	switch {
	case len(solid) != 0:
		logger.V(2).Info("item has at least one existing owner, will not garbage collect",
			"item", item.identity,
			"owner", solid,
		)
		if len(dangling) == 0 && len(waitingForDependentsDeletion) == 0 {
			return nil
		}
		logger.V(2).Info("remove dangling references and waiting references for item",
			"item", item.identity,
			"dangling", dangling,
			"waitingForDependentsDeletion", waitingForDependentsDeletion,
		)
		// waitingForDependentsDeletion needs to be deleted from the
		// ownerReferences, otherwise the referenced objects will be stuck with
		// the FinalizerDeletingDependents and never get deleted.
		ownerUIDs := append(ownerRefsToUIDs(dangling), ownerRefsToUIDs(waitingForDependentsDeletion)...)
		p, err := c.GenerateDeleteOwnerRefStrategicMergeBytes(item.identity.UID, ownerUIDs)
		if err != nil {
			return err
		}
		_, err = gc.patch(item, p, func(n *node) ([]byte, error) {
			return gc.deleteOwnerRefJSONMergePatch(n, ownerUIDs...)
		})
		return err
	case len(waitingForDependentsDeletion) != 0 && item.dependentsLength() != 0:
		deps := item.getDependents()
		for _, dep := range deps {
			if dep.isDeletingDependents() {
				// this circle detection has false positives, we need to
				// apply a more rigorous detection if this turns out to be a
				// problem.
				// there are multiple workers run attemptToDeleteItem in
				// parallel, the circle detection can fail in a race condition.
				logger.V(2).Info("processing item, some of its owners and its dependent have FinalizerDeletingDependents, to prevent potential cycle, its ownerReferences are going to be modified to be non-blocking, then the item is going to be deleted with Foreground",
					"item", item.identity,
					"dependent", dep.identity,
				)
				patch, err := item.unblockOwnerReferencesStrategicMergePatch()
				if err != nil {
					return err
				}
				if _, err := gc.patch(item, patch, gc.unblockOwnerReferencesJSONMergePatch); err != nil {
					return err
				}
				break
			}
		}
		logger.V(2).Info("at least one owner of item has FinalizerDeletingDependents, and the item itself has dependents, so it is going to be deleted in Foreground",
			"item", item.identity,
		)
		// the deletion event will be observed by the graphBuilder, so the item
		// will be processed again in processDeletingDependentsItem. If it
		// doesn't have dependents, the function will remove the
		// FinalizerDeletingDependents from the item, resulting in the final
		// deletion of the item.
		policy := metav1.DeletePropagationForeground
		return gc.deleteObject(item.identity, latest.ResourceVersion, latest.OwnerReferences, &policy)
	default:
		// item doesn't have any solid owner, so it needs to be garbage
		// collected. Also, none of item's owners is waiting for the deletion of
		// the dependents, so set propagationPolicy based on existing finalizers.
		var policy metav1.DeletionPropagation
		switch {
		case hasOrphanFinalizer(latest):
			// if an existing orphan finalizer is already on the object, honor it.
			policy = metav1.DeletePropagationOrphan
		case hasDeleteDependentsFinalizer(latest):
			// if an existing foreground finalizer is already on the object, honor it.
			policy = metav1.DeletePropagationForeground
		default:
			// otherwise, default to background.
			policy = metav1.DeletePropagationBackground
		}
		logger.V(2).Info("Deleting item",
			"item", item.identity,
			"propagationPolicy", policy,
		)
		return gc.deleteObject(item.identity, latest.ResourceVersion, latest.OwnerReferences, &policy)
	}
}

// process item that's waiting for its dependents to be deleted
func (gc *GarbageCollector) processDeletingDependentsItem(logger klog.Logger, item *node) error {
	blockingDependents := item.blockingDependents()
	if len(blockingDependents) == 0 {
		logger.V(2).Info("remove DeleteDependents finalizer for item", "item", item.identity)
		return gc.removeFinalizer(logger, item, metav1.FinalizerDeleteDependents)
	}
	for _, dep := range blockingDependents {
		if !dep.isDeletingDependents() {
			logger.V(2).Info("adding dependent to attemptToDelete, because its owner is deletingDependents",
				"item", item.identity,
				"dependent", dep.identity,
			)
			gc.attemptToDelete.Add(dep)
		}
	}
	return nil
}

// dependents are copies of pointers to the owner's dependents, they don't need to be locked.
func (gc *GarbageCollector) orphanDependents(logger klog.Logger, owner objectReference, dependents []*node) error {
	errCh := make(chan error, len(dependents))
	wg := sync.WaitGroup{}
	wg.Add(len(dependents))
	for i := range dependents {
		go func(dependent *node) {
			defer wg.Done()
			// the dependent.identity.UID is used as precondition
			p, err := c.GenerateDeleteOwnerRefStrategicMergeBytes(dependent.identity.UID, []types.UID{owner.UID})
			if err != nil {
				errCh <- fmt.Errorf("orphaning %s failed, %v", dependent.identity, err)
				return
			}
			_, err = gc.patch(dependent, p, func(n *node) ([]byte, error) {
				return gc.deleteOwnerRefJSONMergePatch(n, owner.UID)
			})
			// note that if the target ownerReference doesn't exist in the
			// dependent, strategic merge patch will NOT return an error.
			if err != nil && !errors.IsNotFound(err) {
				errCh <- fmt.Errorf("orphaning %s failed, %v", dependent.identity, err)
			}
		}(dependents[i])
	}
	wg.Wait()
	close(errCh)

	var errorsSlice []error
	for e := range errCh {
		errorsSlice = append(errorsSlice, e)
	}

	if len(errorsSlice) != 0 {
		return fmt.Errorf("failed to orphan dependents of owner %s, got errors: %s", owner, utilerrors.NewAggregate(errorsSlice).Error())
	}
	logger.V(5).Info("successfully updated all dependents", "owner", owner)
	return nil
}

func (gc *GarbageCollector) runAttemptToOrphanWorker(logger klog.Logger) {
	for gc.processAttemptToOrphanWorker(logger) {
	}
}

// processAttemptToOrphanWorker dequeues a node from the attemptToOrphan, then finds its
// dependents based on the graph maintained by the GC, then removes it from the
// OwnerReferences of its dependents, and finally updates the owner to remove
// the "Orphan" finalizer. The node is added back into the attemptToOrphan if any of
// these steps fail.
func (gc *GarbageCollector) processAttemptToOrphanWorker(logger klog.Logger) bool {
	item, quit := gc.attemptToOrphan.Get()
	if quit {
		return false
	}
	defer gc.attemptToOrphan.Done(item)

	action := gc.attemptToOrphanWorker(logger, item)
	switch action {
	case forgetItem:
		gc.attemptToOrphan.Forget(item)
	case requeueItem:
		gc.attemptToOrphan.AddRateLimited(item)
	}

	return true
}

func (gc *GarbageCollector) attemptToOrphanWorker(logger klog.Logger, item interface{}) workQueueItemAction {
	owner, ok := item.(*node)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expect *node, got %#v", item))
		return forgetItem
	}
	// we don't need to lock each element, because they never get updated
	owner.dependentsLock.RLock()
	dependents := make([]*node, 0, len(owner.dependents))
	for dependent := range owner.dependents {
		dependents = append(dependents, dependent)
	}
	owner.dependentsLock.RUnlock()

	err := gc.orphanDependents(logger, owner.identity, dependents)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("orphanDependents for %s failed with %v", owner.identity, err))
		return requeueItem
	}
	// update the owner, remove "orphaningFinalizer" from its finalizers list
	err = gc.removeFinalizer(logger, owner, metav1.FinalizerOrphanDependents)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("removeOrphanFinalizer for %s failed with %v", owner.identity, err))
		return requeueItem
	}
	return forgetItem
}

// *FOR TEST USE ONLY*
// GraphHasUID returns if the GraphBuilder has a particular UID store in its
// uidToNode graph. It's useful for debugging.
// This method is used by integration tests.
func (gc *GarbageCollector) GraphHasUID(u types.UID) bool {
	_, ok := gc.dependencyGraphBuilder.uidToNode.Read(u)
	return ok
}

// GetDeletableResources returns all resources from discoveryClient that the
// garbage collector should recognize and work with. More specifically, all
// preferred resources which support the 'delete', 'list', and 'watch' verbs.
//
// If an error was encountered fetching resources from the server,
// it is included as well, along with any resources that were successfully resolved.
//
// All discovery errors are considered temporary. Upon encountering any error,
// GetDeletableResources will log and return any discovered resources it was
// able to process (which may be none).
func GetDeletableResources(logger klog.Logger, discoveryClient discovery.ServerResourcesInterface) (map[schema.GroupVersionResource]struct{}, error) {
	preferredResources, lookupErr := discoveryClient.ServerPreferredResources()
	if lookupErr != nil {
		if groupLookupFailures, isLookupFailure := discovery.GroupDiscoveryFailedErrorGroups(lookupErr); isLookupFailure {
			// Serialize groupLookupFailures here as map[schema.GroupVersion]error is not json encodable, otherwise the
			// logger would throw internal error.
			logger.Info("failed to discover some groups", "groups", fmt.Sprintf("%q", groupLookupFailures))
		} else {
			logger.Info("failed to discover preferred resources", "error", lookupErr)
		}
	}
	if preferredResources == nil {
		return map[schema.GroupVersionResource]struct{}{}, lookupErr
	}

	// This is extracted from discovery.GroupVersionResources to allow tolerating
	// failures on a per-resource basis.
	deletableResources := discovery.FilteredBy(discovery.SupportsAllVerbs{Verbs: []string{"delete", "list", "watch"}}, preferredResources)
	deletableGroupVersionResources := map[schema.GroupVersionResource]struct{}{}
	for _, rl := range deletableResources {
		gv, err := schema.ParseGroupVersion(rl.GroupVersion)
		if err != nil {
			logger.Info("ignoring invalid discovered resource", "groupversion", rl.GroupVersion, "error", err)
			continue
		}
		for i := range rl.APIResources {
			deletableGroupVersionResources[schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: rl.APIResources[i].Name}] = struct{}{}
		}
	}

	return deletableGroupVersionResources, lookupErr
}

func (gc *GarbageCollector) Name() string {
	return "garbagecollector"
}

// GetDependencyGraphBuilder return graph builder which is particularly helpful for testing where controllerContext is not available
func (gc *GarbageCollector) GetDependencyGraphBuilder() *GraphBuilder {
	return gc.dependencyGraphBuilder
}
