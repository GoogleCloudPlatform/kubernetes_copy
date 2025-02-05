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
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	eventv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/kubernetes/pkg/controller/apis/config/scheme"
	"k8s.io/kubernetes/pkg/controller/garbagecollector/metaonly"
)

type eventType int

func (e eventType) String() string {
	switch e {
	case addEvent:
		return "add"
	case updateEvent:
		return "update"
	case deleteEvent:
		return "delete"
	default:
		return fmt.Sprintf("unknown(%d)", int(e))
	}
}

const (
	addEvent eventType = iota
	updateEvent
	deleteEvent
)

type event struct {
	// virtual indicates this event did not come from an informer, but was constructed artificially
	virtual   bool
	eventType eventType
	obj       interface{}
	// the update event comes with an old object, but it's not used by the garbage collector.
	oldObj interface{}
	gvk    schema.GroupVersionKind
}

// GraphBuilder processes events supplied by the informers, updates uidToNode,
// a graph that caches the dependencies as we know, and enqueues
// items to the attemptToDelete and attemptToOrphan.
type GraphBuilder struct {
	restMapper meta.RESTMapper

	// each monitor list/watches a resource, the results are funneled to the
	// dependencyGraphBuilder
	monitors    monitors
	monitorLock sync.RWMutex
	// informersStarted is closed after all of the controllers have been initialized and are running.
	// After that it is safe to start them here, before that it is not.
	informersStarted <-chan struct{}

	// stopCh drives shutdown. When a receive from it unblocks, monitors will shut down.
	// This channel is also protected by monitorLock.
	stopCh <-chan struct{}

	// running tracks whether Run() has been called.
	// it is protected by monitorLock.
	running bool

	eventRecorder    record.EventRecorder
	eventBroadcaster record.EventBroadcaster

	metadataClient metadata.Interface
	// monitors are the producer of the graphChanges queue, graphBuilder alters
	// the in-memory graph according to the changes.
	graphChanges workqueue.TypedRateLimitingInterface[*event]
	// uidToNode doesn't require a lock to protect, because only the
	// single-threaded GraphBuilder.processGraphChanges() reads/writes it.
	uidToNode *concurrentUIDToNode
	// GraphBuilder is the producer of attemptToDelete and attemptToOrphan, GC is the consumer.
	attemptToDelete workqueue.TypedRateLimitingInterface[*node]
	attemptToOrphan workqueue.TypedRateLimitingInterface[*node]
	// GraphBuilder and GC share the absentOwnerCache. Objects that are known to
	// be non-existent are added to the cached.
	absentOwnerCache *ReferenceCache
	sharedInformers  informerfactory.InformerFactory
	ignoredResources map[schema.GroupResource]struct{}
}

// monitor runs a Controller with a local stop channel.
type monitor struct {
	controller cache.Controller
	store      cache.Store

	// stopCh stops Controller. If stopCh is nil, the monitor is considered to be
	// not yet started.
	stopCh chan struct{}
}

// Run is intended to be called in a goroutine. Multiple calls of this is an
// error.
func (m *monitor) Run() {
	m.controller.Run(m.stopCh)
}

type monitors map[schema.GroupVersionResource]*monitor

func NewDependencyGraphBuilder(
	ctx context.Context,
	metadataClient metadata.Interface,
	mapper meta.ResettableRESTMapper,
	ignoredResources map[schema.GroupResource]struct{},
	sharedInformers informerfactory.InformerFactory,
	informersStarted <-chan struct{},
) *GraphBuilder {
	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))

	attemptToDelete := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[*node](),
		workqueue.TypedRateLimitingQueueConfig[*node]{
			Name: "garbage_collector_attempt_to_delete",
		},
	)
	attemptToOrphan := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[*node](),
		workqueue.TypedRateLimitingQueueConfig[*node]{
			Name: "garbage_collector_attempt_to_orphan",
		},
	)
	absentOwnerCache := NewReferenceCache(500)
	graphBuilder := &GraphBuilder{
		eventRecorder:    eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "garbage-collector-controller"}),
		eventBroadcaster: eventBroadcaster,
		metadataClient:   metadataClient,
		informersStarted: informersStarted,
		restMapper:       mapper,
		graphChanges: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[*event](),
			workqueue.TypedRateLimitingQueueConfig[*event]{
				Name: "garbage_collector_graph_changes",
			},
		),
		uidToNode: &concurrentUIDToNode{
			uidToNode: make(map[types.UID]*node),
		},
		attemptToDelete:  attemptToDelete,
		attemptToOrphan:  attemptToOrphan,
		absentOwnerCache: absentOwnerCache,
		sharedInformers:  sharedInformers,
		ignoredResources: ignoredResources,
	}

	return graphBuilder
}

func (gb *GraphBuilder) controllerFor(logger klog.Logger, resource schema.GroupVersionResource, kind schema.GroupVersionKind) (cache.Controller, cache.Store, error) {
	handlers := cache.ResourceEventHandlerFuncs{
		// add the event to the dependencyGraphBuilder's graphChanges.
		AddFunc: func(obj interface{}) {
			event := &event{
				eventType: addEvent,
				obj:       obj,
				gvk:       kind,
			}
			gb.graphChanges.Add(event)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			// TODO: check if there are differences in the ownerRefs,
			// finalizers, and DeletionTimestamp; if not, ignore the update.
			event := &event{
				eventType: updateEvent,
				obj:       newObj,
				oldObj:    oldObj,
				gvk:       kind,
			}
			gb.graphChanges.Add(event)
		},
		DeleteFunc: func(obj interface{}) {
			// delta fifo may wrap the object in a cache.DeletedFinalStateUnknown, unwrap it
			if deletedFinalStateUnknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = deletedFinalStateUnknown.Obj
			}
			event := &event{
				eventType: deleteEvent,
				obj:       obj,
				gvk:       kind,
			}
			gb.graphChanges.Add(event)
		},
	}

	shared, err := gb.sharedInformers.ForResource(resource)
	if err != nil {
		logger.V(4).Error(err, "unable to use a shared informer", "resource", resource, "kind", kind)
		return nil, nil, err
	}
	logger.V(4).Info("using a shared informer", "resource", resource, "kind", kind)
	// need to clone because it's from a shared cache
	shared.Informer().AddEventHandlerWithResyncPeriod(handlers, ResourceResyncTime)
	return shared.Informer().GetController(), shared.Informer().GetStore(), nil
}

// syncMonitors rebuilds the monitor set according to the supplied resources,
// creating or deleting monitors as necessary. It will return any error
// encountered, but will make an attempt to create a monitor for each resource
// instead of immediately exiting on an error. It may be called before or after
// Run. Monitors are NOT started as part of the sync. To ensure all existing
// monitors are started, call startMonitors.
func (gb *GraphBuilder) syncMonitors(logger klog.Logger, resources map[schema.GroupVersionResource]struct{}) error {
	gb.monitorLock.Lock()
	defer gb.monitorLock.Unlock()

	toRemove := gb.monitors
	if toRemove == nil {
		toRemove = monitors{}
	}
	current := monitors{}
	errs := []error{}
	kept := 0
	added := 0
	for resource := range resources {
		if _, ok := gb.ignoredResources[resource.GroupResource()]; ok {
			continue
		}
		if m, ok := toRemove[resource]; ok {
			current[resource] = m
			delete(toRemove, resource)
			kept++
			continue
		}
		kind, err := gb.restMapper.KindFor(resource)
		if err != nil {
			errs = append(errs, fmt.Errorf("couldn't look up resource %q: %v", resource, err))
			continue
		}
		c, s, err := gb.controllerFor(logger, resource, kind)
		if err != nil {
			errs = append(errs, fmt.Errorf("couldn't start monitor for resource %q: %v", resource, err))
			continue
		}
		current[resource] = &monitor{store: s, controller: c}
		added++
	}
	gb.monitors = current

	for _, monitor := range toRemove {
		if monitor.stopCh != nil {
			close(monitor.stopCh)
		}
	}

	logger.V(4).Info("synced monitors", "added", added, "kept", kept, "removed", len(toRemove))
	// NewAggregate returns nil if errs is 0-length
	return utilerrors.NewAggregate(errs)
}

// startMonitors ensures the current set of monitors are running. Any newly
// started monitors will also cause shared informers to be started.
//
// If called before Run, startMonitors does nothing (as there is no stop channel
// to support monitor/informer execution).
func (gb *GraphBuilder) startMonitors(logger klog.Logger) {
	gb.monitorLock.Lock()
	defer gb.monitorLock.Unlock()

	if !gb.running {
		return
	}

	// we're waiting until after the informer start that happens once all the controllers are initialized.  This ensures
	// that they don't get unexpected events on their work queues.
	<-gb.informersStarted

	monitors := gb.monitors
	started := 0
	for _, monitor := range monitors {
		if monitor.stopCh == nil {
			monitor.stopCh = make(chan struct{})
			gb.sharedInformers.Start(gb.stopCh)
			go monitor.Run()
			started++
		}
	}
	logger.V(4).Info("started new monitors", "new", started, "current", len(monitors))
}

// IsResourceSynced returns true if a monitor exists for the given resource and has synced
func (gb *GraphBuilder) IsResourceSynced(resource schema.GroupVersionResource) bool {
	gb.monitorLock.Lock()
	defer gb.monitorLock.Unlock()
	monitor, ok := gb.monitors[resource]
	return ok && monitor.controller.HasSynced()
}

// IsSynced returns true if any monitors exist AND all those monitors'
// controllers HasSynced functions return true. This means IsSynced could return
// true at one time, and then later return false if all monitors were
// reconstructed.
func (gb *GraphBuilder) IsSynced(logger klog.Logger) bool {
	gb.monitorLock.Lock()
	defer gb.monitorLock.Unlock()

	if len(gb.monitors) == 0 {
		logger.V(4).Info("garbage controller monitor not synced: no monitors")
		return false
	}

	for resource, monitor := range gb.monitors {
		if !monitor.controller.HasSynced() {
			logger.V(4).Info("garbage controller monitor not yet synced", "resource", resource)
			return false
		}
	}
	return true
}

// Run sets the stop channel and starts monitor execution until stopCh is
// closed. Any running monitors will be stopped before Run returns.
func (gb *GraphBuilder) Run(ctx context.Context) {
	logger := klog.FromContext(ctx)
	logger.Info("Running", "component", "GraphBuilder")
	defer logger.Info("Stopping", "component", "GraphBuilder")

	// Set up the stop channel.
	gb.monitorLock.Lock()
	gb.stopCh = ctx.Done()
	gb.running = true
	gb.monitorLock.Unlock()

	// Start monitors and begin change processing until the stop channel is
	// closed.
	gb.startMonitors(logger)
	wait.Until(func() { gb.runProcessGraphChanges(logger) }, 1*time.Second, ctx.Done())

	// Stop any running monitors.
	gb.monitorLock.Lock()
	defer gb.monitorLock.Unlock()
	monitors := gb.monitors
	stopped := 0
	for _, monitor := range monitors {
		if monitor.stopCh != nil {
			stopped++
			close(monitor.stopCh)
		}
	}

	// reset monitors so that the graph builder can be safely re-run/synced.
	gb.monitors = nil
	logger.Info("stopped monitors", "stopped", stopped, "total", len(monitors))
}

var ignoredResources = map[schema.GroupResource]struct{}{
	{Group: "", Resource: "events"}:                {},
	{Group: eventv1.GroupName, Resource: "events"}: {},
}

// DefaultIgnoredResources returns the default set of resources that the garbage collector controller
// should ignore. This is exposed so downstream integrators can have access to the defaults, and add
// to them as necessary when constructing the controller.
func DefaultIgnoredResources() map[schema.GroupResource]struct{} {
	return ignoredResources
}

// enqueueVirtualDeleteEvent is used to add a virtual delete event to be processed for virtual nodes
// once it is determined they do not have backing objects in storage
func (gb *GraphBuilder) enqueueVirtualDeleteEvent(ref objectReference) {
	gv, _ := schema.ParseGroupVersion(ref.APIVersion)
	gb.graphChanges.Add(&event{
		virtual:   true,
		eventType: deleteEvent,
		gvk:       gv.WithKind(ref.Kind),
		obj: &metaonly.MetadataOnlyObject{
			TypeMeta:   metav1.TypeMeta{APIVersion: ref.APIVersion, Kind: ref.Kind},
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, UID: ref.UID, Name: ref.Name},
		},
	})
}

// addDependentToOwners adds n to owners' dependents list. If the owner does not
// exist in the gb.uidToNode yet, a "virtual" node will be created to represent
// the owner. The "virtual" node will be enqueued to the attemptToDelete, so that
// attemptToDeleteItem() will verify if the owner exists according to the API server.
func (gb *GraphBuilder) addDependentToOwners(logger klog.Logger, n *node, owners []metav1.OwnerReference) {
	// track if some of the referenced owners already exist in the graph and have been observed,
	// and the dependent's ownerRef does not match their observed coordinates
	hasPotentiallyInvalidOwnerReference := false

	for _, owner := range owners {
		ownerNode, ok := gb.uidToNode.Read(owner.UID)
		if !ok {
			// Create a "virtual" node in the graph for the owner if it doesn't
			// exist in the graph yet.
			ownerNode = &node{
				identity: objectReference{
					OwnerReference: ownerReferenceCoordinates(owner),
					Namespace:      n.identity.Namespace,
				},
				dependents: make(map[*node]struct{}),
				virtual:    true,
			}
			logger.V(5).Info("add virtual item", "identity", ownerNode.identity)
			gb.uidToNode.Write(ownerNode)
		}
		ownerNode.addDependent(n)
		if !ok {
			// Enqueue the virtual node into attemptToDelete.
			// The garbage processor will enqueue a virtual delete
			// event to delete it from the graph if API server confirms this
			// owner doesn't exist.
			gb.attemptToDelete.Add(ownerNode)
		} else if !hasPotentiallyInvalidOwnerReference {
			ownerIsNamespaced := len(ownerNode.identity.Namespace) > 0
			if ownerIsNamespaced && ownerNode.identity.Namespace != n.identity.Namespace {
				if ownerNode.isObserved() {
					// The owner node has been observed via an informer
					// the dependent's namespace doesn't match the observed owner's namespace, this is definitely wrong.
					// cluster-scoped owners can be referenced as an owner from any namespace or cluster-scoped object.
					logger.V(2).Info("item references an owner but does not match namespaces", "item", n.identity, "owner", ownerNode.identity)
					gb.reportInvalidNamespaceOwnerRef(n, owner.UID)
				}
				hasPotentiallyInvalidOwnerReference = true
			} else if !ownerReferenceMatchesCoordinates(owner, ownerNode.identity.OwnerReference) {
				if ownerNode.isObserved() {
					// The owner node has been observed via an informer
					// n's owner reference doesn't match the observed identity, this might be wrong.
					logger.V(2).Info("item references an owner with coordinates that do not match the observed identity", "item", n.identity, "owner", ownerNode.identity)
				}
				hasPotentiallyInvalidOwnerReference = true
			} else if !ownerIsNamespaced && ownerNode.identity.Namespace != n.identity.Namespace && !ownerNode.isObserved() {
				// the ownerNode is cluster-scoped and virtual, and does not match the child node's namespace.
				// the owner could be a missing instance of a namespaced type incorrectly referenced by a cluster-scoped child (issue #98040).
				// enqueue this child to attemptToDelete to verify parent references.
				hasPotentiallyInvalidOwnerReference = true
			}
		}
	}

	if hasPotentiallyInvalidOwnerReference {
		// Enqueue the potentially invalid dependent node into attemptToDelete.
		// The garbage processor will verify whether the owner references are dangling
		// and delete the dependent if all owner references are confirmed absent.
		gb.attemptToDelete.Add(n)
	}
}

func (gb *GraphBuilder) reportInvalidNamespaceOwnerRef(n *node, invalidOwnerUID types.UID) {
	var invalidOwnerRef metav1.OwnerReference
	var found = false
	for _, ownerRef := range n.owners {
		if ownerRef.UID == invalidOwnerUID {
			invalidOwnerRef = ownerRef
			found = true
			break
		}
	}
	if !found {
		return
	}
	ref := &v1.ObjectReference{
		Kind:       n.identity.Kind,
		APIVersion: n.identity.APIVersion,
		Namespace:  n.identity.Namespace,
		Name:       n.identity.Name,
		UID:        n.identity.UID,
	}
	invalidIdentity := objectReference{
		OwnerReference: metav1.OwnerReference{
			Kind:       invalidOwnerRef.Kind,
			APIVersion: invalidOwnerRef.APIVersion,
			Name:       invalidOwnerRef.Name,
			UID:        invalidOwnerRef.UID,
		},
		Namespace: n.identity.Namespace,
	}
	gb.eventRecorder.Eventf(ref, v1.EventTypeWarning, "OwnerRefInvalidNamespace", "ownerRef %s does not exist in namespace %q", invalidIdentity, n.identity.Namespace)
}

// insertNode insert the node to gb.uidToNode; then it finds all owners as listed
// in n.owners, and adds the node to their dependents list.
func (gb *GraphBuilder) insertNode(logger klog.Logger, n *node) {
	gb.uidToNode.Write(n)
	gb.addDependentToOwners(logger, n, n.owners)
}

// removeDependentFromOwners remove n from owners' dependents list.
func (gb *GraphBuilder) removeDependentFromOwners(n *node, owners []metav1.OwnerReference) {
	for _, owner := range owners {
		ownerNode, ok := gb.uidToNode.Read(owner.UID)
		if !ok {
			continue
		}
		ownerNode.deleteDependent(n)
	}
}

// removeNode removes the node from gb.uidToNode, then finds all
// owners as listed in n.owners, and removes n from their dependents list.
func (gb *GraphBuilder) removeNode(n *node) {
	gb.uidToNode.Delete(n.identity.UID)
	gb.removeDependentFromOwners(n, n.owners)
}

type ownerRefPair struct {
	oldRef metav1.OwnerReference
	newRef metav1.OwnerReference
}

// TODO: profile this function to see if a naive N^2 algorithm performs better
// when the number of references is small.
func referencesDiffs(old []metav1.OwnerReference, new []metav1.OwnerReference) (added []metav1.OwnerReference, removed []metav1.OwnerReference, changed []ownerRefPair) {
	oldUIDToRef := make(map[string]metav1.OwnerReference)
	for _, value := range old {
		oldUIDToRef[string(value.UID)] = value
	}
	oldUIDSet := sets.StringKeySet(oldUIDToRef)
	for _, value := range new {
		newUID := string(value.UID)
		if oldUIDSet.Has(newUID) {
			if !reflect.DeepEqual(oldUIDToRef[newUID], value) {
				changed = append(changed, ownerRefPair{oldRef: oldUIDToRef[newUID], newRef: value})
			}
			oldUIDSet.Delete(newUID)
		} else {
			added = append(added, value)
		}
	}
	for oldUID := range oldUIDSet {
		removed = append(removed, oldUIDToRef[oldUID])
	}

	return added, removed, changed
}

func deletionStartsWithFinalizer(oldObj interface{}, newAccessor metav1.Object, matchingFinalizer string) bool {
	// if the new object isn't being deleted, or doesn't have the finalizer we're interested in, return false
	if !beingDeleted(newAccessor) || !hasFinalizer(newAccessor, matchingFinalizer) {
		return false
	}

	// if the old object is nil, or wasn't being deleted, or didn't have the finalizer, return true
	if oldObj == nil {
		return true
	}
	oldAccessor, err := meta.Accessor(oldObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("cannot access oldObj: %v", err))
		return false
	}
	return !beingDeleted(oldAccessor) || !hasFinalizer(oldAccessor, matchingFinalizer)
}

func beingDeleted(accessor metav1.Object) bool {
	return accessor.GetDeletionTimestamp() != nil
}

func hasDeleteDependentsFinalizer(accessor metav1.Object) bool {
	return hasFinalizer(accessor, metav1.FinalizerDeleteDependents)
}

func hasOrphanFinalizer(accessor metav1.Object) bool {
	return hasFinalizer(accessor, metav1.FinalizerOrphanDependents)
}

func hasFinalizer(accessor metav1.Object, matchingFinalizer string) bool {
	finalizers := accessor.GetFinalizers()
	for _, finalizer := range finalizers {
		if finalizer == matchingFinalizer {
			return true
		}
	}
	return false
}

// this function takes newAccessor directly because the caller already
// instantiates an accessor for the newObj.
func startsWaitingForDependentsDeleted(oldObj interface{}, newAccessor metav1.Object) bool {
	return deletionStartsWithFinalizer(oldObj, newAccessor, metav1.FinalizerDeleteDependents)
}

// this function takes newAccessor directly because the caller already
// instantiates an accessor for the newObj.
func startsWaitingForDependentsOrphaned(oldObj interface{}, newAccessor metav1.Object) bool {
	return deletionStartsWithFinalizer(oldObj, newAccessor, metav1.FinalizerOrphanDependents)
}

// if an blocking ownerReference points to an object gets removed, or gets set to
// "BlockOwnerDeletion=false", add the object to the attemptToDelete queue.
func (gb *GraphBuilder) addUnblockedOwnersToDeleteQueue(logger klog.Logger, removed []metav1.OwnerReference, changed []ownerRefPair) {
	for _, ref := range removed {
		if ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
			node, found := gb.uidToNode.Read(ref.UID)
			if !found {
				logger.V(5).Info("cannot find uid in uidToNode", "uid", ref.UID)
				continue
			}
			gb.attemptToDelete.Add(node)
		}
	}
	for _, c := range changed {
		wasBlocked := c.oldRef.BlockOwnerDeletion != nil && *c.oldRef.BlockOwnerDeletion
		isUnblocked := c.newRef.BlockOwnerDeletion == nil || (c.newRef.BlockOwnerDeletion != nil && !*c.newRef.BlockOwnerDeletion)
		if wasBlocked && isUnblocked {
			node, found := gb.uidToNode.Read(c.newRef.UID)
			if !found {
				logger.V(5).Info("cannot find uid in uidToNode", "uid", c.newRef.UID)
				continue
			}
			gb.attemptToDelete.Add(node)
		}
	}
}

func (gb *GraphBuilder) processTransitions(logger klog.Logger, oldObj interface{}, newAccessor metav1.Object, n *node) {
	if startsWaitingForDependentsOrphaned(oldObj, newAccessor) {
		logger.V(5).Info("add item to attemptToOrphan", "item", n.identity)
		gb.attemptToOrphan.Add(n)
		return
	}
	if startsWaitingForDependentsDeleted(oldObj, newAccessor) {
		logger.V(2).Info("add item to attemptToDelete, because it's waiting for its dependents to be deleted", "item", n.identity)
		// if the n is added as a "virtual" node, its deletingDependents field is not properly set, so always set it here.
		n.markDeletingDependents()
		for dep := range n.dependents {
			gb.attemptToDelete.Add(dep)
		}
		gb.attemptToDelete.Add(n)
	}
}

func (gb *GraphBuilder) runProcessGraphChanges(logger klog.Logger) {
	for gb.processGraphChanges(logger) {
	}
}

func identityFromEvent(event *event, accessor metav1.Object) objectReference {
	return objectReference{
		OwnerReference: metav1.OwnerReference{
			APIVersion: event.gvk.GroupVersion().String(),
			Kind:       event.gvk.Kind,
			UID:        accessor.GetUID(),
			Name:       accessor.GetName(),
		},
		Namespace: accessor.GetNamespace(),
	}
}

// Dequeueing an event from graphChanges, updating graph, populating dirty_queue.
func (gb *GraphBuilder) processGraphChanges(logger klog.Logger) bool {
	item, quit := gb.graphChanges.Get()
	if quit {
		return false
	}
	defer gb.graphChanges.Done(item)
	event := item
	obj := item.obj
	accessor, err := meta.Accessor(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("cannot access obj: %v", err))
		return true
	}

	logger.V(5).Info("GraphBuilder process object",
		"apiVersion", event.gvk.GroupVersion().String(),
		"kind", event.gvk.Kind,
		"object", klog.KObj(accessor),
		"uid", string(accessor.GetUID()),
		"eventType", event.eventType,
		"virtual", event.virtual,
	)

	// Check if the node already exists
	existingNode, found := gb.uidToNode.Read(accessor.GetUID())
	if found && !event.virtual && !existingNode.isObserved() {
		// this marks the node as having been observed via an informer event
		// 1. this depends on graphChanges only containing add/update events from the actual informer
		// 2. this allows things tracking virtual nodes' existence to stop polling and rely on informer events
		observedIdentity := identityFromEvent(event, accessor)
		if observedIdentity != existingNode.identity {
			// find dependents that don't match the identity we observed
			_, potentiallyInvalidDependents := partitionDependents(existingNode.getDependents(), observedIdentity)
			// add those potentially invalid dependents to the attemptToDelete queue.
			// if their owners are still solid the attemptToDelete will be a no-op.
			// this covers the bad child -> good parent observation sequence.
			// the good parent -> bad child observation sequence is handled in addDependentToOwners
			for _, dep := range potentiallyInvalidDependents {
				if len(observedIdentity.Namespace) > 0 && dep.identity.Namespace != observedIdentity.Namespace {
					// Namespace mismatch, this is definitely wrong
					logger.V(2).Info("item references an owner but does not match namespaces",
						"item", dep.identity,
						"owner", observedIdentity,
					)
					gb.reportInvalidNamespaceOwnerRef(dep, observedIdentity.UID)
				}
				gb.attemptToDelete.Add(dep)
			}

			// make a copy (so we don't modify the existing node in place), store the observed identity, and replace the virtual node
			logger.V(2).Info("replacing virtual item with observed item",
				"virtual", existingNode.identity,
				"observed", observedIdentity,
			)
			existingNode = existingNode.clone()
			existingNode.identity = observedIdentity
			gb.uidToNode.Write(existingNode)
		}
		existingNode.markObserved()
	}
	switch {
	case (event.eventType == addEvent || event.eventType == updateEvent) && !found:
		newNode := &node{
			identity:           identityFromEvent(event, accessor),
			dependents:         make(map[*node]struct{}),
			owners:             accessor.GetOwnerReferences(),
			deletingDependents: beingDeleted(accessor) && hasDeleteDependentsFinalizer(accessor),
			beingDeleted:       beingDeleted(accessor),
		}
		gb.insertNode(logger, newNode)
		// the underlying delta_fifo may combine a creation and a deletion into
		// one event, so we need to further process the event.
		gb.processTransitions(logger, event.oldObj, accessor, newNode)
	case (event.eventType == addEvent || event.eventType == updateEvent) && found:
		// handle changes in ownerReferences
		added, removed, changed := referencesDiffs(existingNode.owners, accessor.GetOwnerReferences())
		if len(added) != 0 || len(removed) != 0 || len(changed) != 0 {
			// check if the changed dependency graph unblock owners that are
			// waiting for the deletion of their dependents.
			gb.addUnblockedOwnersToDeleteQueue(logger, removed, changed)
			// update the node itself
			existingNode.owners = accessor.GetOwnerReferences()
			// Add the node to its new owners' dependent lists.
			gb.addDependentToOwners(logger, existingNode, added)
			// remove the node from the dependent list of node that are no longer in
			// the node's owners list.
			gb.removeDependentFromOwners(existingNode, removed)
		}

		if beingDeleted(accessor) {
			existingNode.markBeingDeleted()
		}
		gb.processTransitions(logger, event.oldObj, accessor, existingNode)
	case event.eventType == deleteEvent:
		if !found {
			logger.V(5).Info("item doesn't exist in the graph, this shouldn't happen",
				"item", accessor.GetUID(),
			)
			return true
		}

		removeExistingNode := true

		if event.virtual {
			// this is a virtual delete event, not one observed from an informer
			deletedIdentity := identityFromEvent(event, accessor)
			if existingNode.virtual {

				// our existing node is also virtual, we're not sure of its coordinates.
				// see if any dependents reference this owner with coordinates other than the one we got a virtual delete event for.
				if matchingDependents, nonmatchingDependents := partitionDependents(existingNode.getDependents(), deletedIdentity); len(nonmatchingDependents) > 0 {

					// some of our dependents disagree on our coordinates, so do not remove the existing virtual node from the graph
					removeExistingNode = false

					if len(matchingDependents) > 0 {
						// mark the observed deleted identity as absent
						gb.absentOwnerCache.Add(deletedIdentity)
						// attempt to delete dependents that do match the verified deleted identity
						for _, dep := range matchingDependents {
							gb.attemptToDelete.Add(dep)
						}
					}

					// if the delete event verified existingNode.identity doesn't exist...
					if existingNode.identity == deletedIdentity {
						// find an alternative identity our nonmatching dependents refer to us by
						replacementIdentity := getAlternateOwnerIdentity(nonmatchingDependents, deletedIdentity)
						if replacementIdentity != nil {
							// replace the existing virtual node with a new one with one of our other potential identities
							replacementNode := existingNode.clone()
							replacementNode.identity = *replacementIdentity
							gb.uidToNode.Write(replacementNode)
							// and add the new virtual node back to the attemptToDelete queue
							gb.attemptToDelete.AddRateLimited(replacementNode)
						}
					}
				}

			} else if existingNode.identity != deletedIdentity {
				// do not remove the existing real node from the graph based on a virtual delete event
				removeExistingNode = false

				// our existing node which was observed via informer disagrees with the virtual delete event's coordinates
				matchingDependents, _ := partitionDependents(existingNode.getDependents(), deletedIdentity)

				if len(matchingDependents) > 0 {
					// mark the observed deleted identity as absent
					gb.absentOwnerCache.Add(deletedIdentity)
					// attempt to delete dependents that do match the verified deleted identity
					for _, dep := range matchingDependents {
						gb.attemptToDelete.Add(dep)
					}
				}
			}
		}

		if removeExistingNode {
			// removeNode updates the graph
			gb.removeNode(existingNode)
			existingNode.dependentsLock.RLock()
			defer existingNode.dependentsLock.RUnlock()
			if len(existingNode.dependents) > 0 {
				gb.absentOwnerCache.Add(identityFromEvent(event, accessor))
			}
			for dep := range existingNode.dependents {
				gb.attemptToDelete.Add(dep)
			}
			for _, owner := range existingNode.owners {
				ownerNode, found := gb.uidToNode.Read(owner.UID)
				if !found || !ownerNode.isDeletingDependents() {
					continue
				}
				// this is to let attemptToDeleteItem check if all the owner's
				// dependents are deleted, if so, the owner will be deleted.
				gb.attemptToDelete.Add(ownerNode)
			}
		}
	}
	return true
}

// partitionDependents divides the provided dependents into a list which have an ownerReference matching the provided identity,
// and ones which have an ownerReference for the given uid that do not match the provided identity.
// Note that a dependent with multiple ownerReferences for the target uid can end up in both lists.
func partitionDependents(dependents []*node, matchOwnerIdentity objectReference) (matching, nonmatching []*node) {
	ownerIsNamespaced := len(matchOwnerIdentity.Namespace) > 0
	for i := range dependents {
		dep := dependents[i]
		foundMatch := false
		foundMismatch := false
		// if the dep namespace matches or the owner is cluster scoped ...
		if ownerIsNamespaced && matchOwnerIdentity.Namespace != dep.identity.Namespace {
			// all references to the parent do not match, since the dependent namespace does not match the owner
			foundMismatch = true
		} else {
			for _, ownerRef := range dep.owners {
				// ... find the ownerRef with a matching uid ...
				if ownerRef.UID == matchOwnerIdentity.UID {
					// ... and check if it matches all coordinates
					if ownerReferenceMatchesCoordinates(ownerRef, matchOwnerIdentity.OwnerReference) {
						foundMatch = true
					} else {
						foundMismatch = true
					}
				}
			}
		}

		if foundMatch {
			matching = append(matching, dep)
		}
		if foundMismatch {
			nonmatching = append(nonmatching, dep)
		}
	}
	return matching, nonmatching
}

func referenceLessThan(a, b objectReference) bool {
	// kind/apiVersion are more significant than namespace,
	// so that we get coherent ordering between kinds
	// regardless of whether they are cluster-scoped or namespaced
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.APIVersion != b.APIVersion {
		return a.APIVersion < b.APIVersion
	}
	// namespace is more significant than name
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	// name is more significant than uid
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	// uid is included for completeness, but is expected to be identical
	// when getting alternate identities for an owner since they are keyed by uid
	if a.UID != b.UID {
		return a.UID < b.UID
	}
	return false
}

// getAlternateOwnerIdentity searches deps for owner references which match
// verifiedAbsentIdentity.UID but differ in apiVersion/kind/name or namespace.
// The first that follows verifiedAbsentIdentity (according to referenceLessThan) is returned.
// If none follow verifiedAbsentIdentity, the first (according to referenceLessThan) is returned.
// If no alternate identities are found, nil is returned.
func getAlternateOwnerIdentity(deps []*node, verifiedAbsentIdentity objectReference) *objectReference {
	absentIdentityIsClusterScoped := len(verifiedAbsentIdentity.Namespace) == 0

	seenAlternates := map[objectReference]bool{verifiedAbsentIdentity: true}

	// keep track of the first alternate reference (according to referenceLessThan)
	var first *objectReference
	// keep track of the first reference following verifiedAbsentIdentity (according to referenceLessThan)
	var firstFollowing *objectReference

	for _, dep := range deps {
		for _, ownerRef := range dep.owners {
			if ownerRef.UID != verifiedAbsentIdentity.UID {
				// skip references that aren't the uid we care about
				continue
			}

			if ownerReferenceMatchesCoordinates(ownerRef, verifiedAbsentIdentity.OwnerReference) {
				if absentIdentityIsClusterScoped || verifiedAbsentIdentity.Namespace == dep.identity.Namespace {
					// skip references that exactly match verifiedAbsentIdentity
					continue
				}
			}

			ref := objectReference{OwnerReference: ownerReferenceCoordinates(ownerRef), Namespace: dep.identity.Namespace}
			if absentIdentityIsClusterScoped && ref.APIVersion == verifiedAbsentIdentity.APIVersion && ref.Kind == verifiedAbsentIdentity.Kind {
				// we know this apiVersion/kind is cluster-scoped because of verifiedAbsentIdentity,
				// so clear the namespace from the alternate identity
				ref.Namespace = ""
			}

			if seenAlternates[ref] {
				// skip references we've already seen
				continue
			}
			seenAlternates[ref] = true

			if first == nil || referenceLessThan(ref, *first) {
				// this alternate comes first lexically
				first = &ref
			}
			if referenceLessThan(verifiedAbsentIdentity, ref) && (firstFollowing == nil || referenceLessThan(ref, *firstFollowing)) {
				// this alternate is the first following verifiedAbsentIdentity lexically
				firstFollowing = &ref
			}
		}
	}

	// return the first alternate identity following the verified absent identity, if there is one
	if firstFollowing != nil {
		return firstFollowing
	}
	// otherwise return the first alternate identity
	return first
}

func (gb *GraphBuilder) GetGraphResources() (
	attemptToDelete workqueue.TypedRateLimitingInterface[*node],
	attemptToOrphan workqueue.TypedRateLimitingInterface[*node],
	absentOwnerCache *ReferenceCache,
) {
	return gb.attemptToDelete, gb.attemptToOrphan, gb.absentOwnerCache
}

type Monitor struct {
	Store      cache.Store
	Controller cache.Controller
}

// GetMonitor returns a monitor for the given resource.
// If the monitor is not synced, it will return an error and the monitor to allow the caller to decide whether to retry.
// If the monitor is not found, it will return only an error.
func (gb *GraphBuilder) GetMonitor(ctx context.Context, resource schema.GroupVersionResource) (*Monitor, error) {
	gb.monitorLock.RLock()
	defer gb.monitorLock.RUnlock()

	var monitor *monitor
	if m, ok := gb.monitors[resource]; ok {
		monitor = m
	} else {
		for monitorGVR, m := range gb.monitors {
			if monitorGVR.Group == resource.Group && monitorGVR.Resource == resource.Resource {
				monitor = m
				break
			}
		}
	}

	if monitor == nil {
		return nil, fmt.Errorf("no monitor found for resource %s", resource.String())
	}

	resourceMonitor := &Monitor{
		Store:      monitor.store,
		Controller: monitor.controller,
	}

	if !cache.WaitForNamedCacheSync(
		gb.Name(),
		ctx.Done(),
		func() bool {
			return monitor.controller.HasSynced()
		},
	) {
		// returning monitor to allow the caller to decide whether to retry as it can be synced later
		return resourceMonitor, fmt.Errorf("dependency graph for resource %s is not synced", resource.String())
	}

	return resourceMonitor, nil
}

func (gb *GraphBuilder) Name() string {
	return "dependencygraphbuilder"
}
