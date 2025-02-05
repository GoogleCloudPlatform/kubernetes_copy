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

package resourcequota

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	quota "k8s.io/apiserver/pkg/quota/v1"
	"k8s.io/apiserver/pkg/quota/v1/generic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/kubernetes/pkg/controller"
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
	eventType eventType
	obj       interface{}
	oldObj    interface{}
	gvr       schema.GroupVersionResource
}

// QuotaMonitor contains all necessary information to track quotas and trigger replenishments
type QuotaMonitor struct {
	// each monitor list/watches a resource and determines if we should replenish quota
	monitors    monitors
	monitorLock sync.RWMutex
	// informersStarted is closed after all the controllers have been initialized and are running.
	// After that it is safe to start them here, before that it is not.
	informersStarted <-chan struct{}

	// stopCh drives shutdown. When a reception from it unblocks, monitors will shut down.
	// This channel is also protected by monitorLock.
	stopCh <-chan struct{}

	// running tracks whether Run() has been called.
	// it is protected by monitorLock.
	running bool

	// monitors are the producer of the resourceChanges queue
	resourceChanges workqueue.TypedRateLimitingInterface[*event]

	// interfaces with informers
	informerFactory informerfactory.InformerFactory

	// list of resources to ignore
	ignoredResources map[schema.GroupResource]struct{}

	// The period that should be used to re-sync the monitored resource
	resyncPeriod controller.ResyncPeriodFunc

	// callback to alert that a change may require quota recalculation
	replenishmentFunc ReplenishmentFunc

	// maintains list of evaluators
	registry quota.Registry

	updateFilter UpdateFilter
}

// NewMonitor creates a new instance of a QuotaMonitor
func NewMonitor(informersStarted <-chan struct{}, informerFactory informerfactory.InformerFactory, ignoredResources map[schema.GroupResource]struct{}, resyncPeriod controller.ResyncPeriodFunc, replenishmentFunc ReplenishmentFunc, registry quota.Registry, updateFilter UpdateFilter) *QuotaMonitor {
	return &QuotaMonitor{
		informersStarted: informersStarted,
		informerFactory:  informerFactory,
		ignoredResources: ignoredResources,
		resourceChanges: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[*event](),
			workqueue.TypedRateLimitingQueueConfig[*event]{Name: "resource_quota_controller_resource_changes"},
		),
		resyncPeriod:      resyncPeriod,
		replenishmentFunc: replenishmentFunc,
		registry:          registry,
		updateFilter:      updateFilter,
	}
}

// monitor runs a Controller with a local stop channel.
type monitor struct {
	controller cache.Controller

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

// UpdateFilter is a function that returns true if the update event should be added to the resourceChanges queue.
type UpdateFilter func(resource schema.GroupVersionResource, oldObj, newObj interface{}) bool

func (qm *QuotaMonitor) controllerFor(ctx context.Context, resource schema.GroupVersionResource) (cache.Controller, error) {
	logger := klog.FromContext(ctx)

	handlers := cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			if qm.updateFilter != nil && qm.updateFilter(resource, oldObj, newObj) {
				event := &event{
					eventType: updateEvent,
					obj:       newObj,
					oldObj:    oldObj,
					gvr:       resource,
				}
				qm.resourceChanges.Add(event)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// delta fifo may wrap the object in a cache.DeletedFinalStateUnknown, unwrap it
			if deletedFinalStateUnknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = deletedFinalStateUnknown.Obj
			}
			event := &event{
				eventType: deleteEvent,
				obj:       obj,
				gvr:       resource,
			}
			qm.resourceChanges.Add(event)
		},
	}
	shared, err := qm.informerFactory.ForResource(resource)
	if err == nil {
		logger.V(4).Info("QuotaMonitor using a shared informer", "resource", resource.String())
		shared.Informer().AddEventHandlerWithResyncPeriod(handlers, qm.resyncPeriod())
		return shared.Informer().GetController(), nil
	}
	logger.V(4).Error(err, "QuotaMonitor unable to use a shared informer", "resource", resource.String())

	// TODO: if we can share storage with garbage collector, it may make sense to support other resources
	// until that time, aggregated api servers will have to run their own controller to reconcile their own quota.
	return nil, fmt.Errorf("unable to monitor quota for resource %q", resource.String())
}

// SyncMonitors rebuilds the monitor set according to the supplied resources,
// creating or deleting monitors as necessary. It will return any error
// encountered, but will make an attempt to create a monitor for each resource
// instead of immediately exiting on an error. It may be called before or after
// Run. Monitors are NOT started as part of the sync. To ensure all existing
// monitors are started, call StartMonitors.
func (qm *QuotaMonitor) SyncMonitors(ctx context.Context, resources map[schema.GroupVersionResource]struct{}) error {
	logger := klog.FromContext(ctx)

	qm.monitorLock.Lock()
	defer qm.monitorLock.Unlock()

	toRemove := qm.monitors
	if toRemove == nil {
		toRemove = monitors{}
	}
	current := monitors{}
	var errs []error
	kept := 0
	added := 0
	for resource := range resources {
		if _, ok := qm.ignoredResources[resource.GroupResource()]; ok {
			continue
		}
		if m, ok := toRemove[resource]; ok {
			current[resource] = m
			delete(toRemove, resource)
			kept++
			continue
		}
		c, err := qm.controllerFor(ctx, resource)
		if err != nil {
			errs = append(errs, fmt.Errorf("couldn't start monitor for resource %q: %v", resource, err))
			continue
		}

		// check if we need to create an evaluator for this resource (if none previously registered)
		evaluator := qm.registry.Get(resource.GroupResource())
		if evaluator == nil {
			listerFunc := generic.ListerFuncForResourceFunc(qm.informerFactory.ForResource)
			listResourceFunc := generic.ListResourceUsingListerFunc(listerFunc, resource)
			evaluator = generic.NewObjectCountEvaluator(resource.GroupResource(), listResourceFunc, "")
			qm.registry.Add(evaluator)
			logger.Info("QuotaMonitor created object count evaluator", "resource", resource.GroupResource())
		}

		// track the monitor
		current[resource] = &monitor{controller: c}
		added++
	}
	qm.monitors = current

	for _, monitor := range toRemove {
		if monitor.stopCh != nil {
			close(monitor.stopCh)
		}
	}

	logger.V(4).Info("quota synced monitors", "added", added, "kept", kept, "removed", len(toRemove))
	// NewAggregate returns nil if errs is 0-length
	return utilerrors.NewAggregate(errs)
}

// StartMonitors ensures the current set of monitors are running. Any newly
// started monitors will also cause shared informers to be started.
//
// If called before Run, StartMonitors does nothing (as there is no stop channel
// to support monitor/informer execution).
func (qm *QuotaMonitor) StartMonitors(ctx context.Context) {
	qm.monitorLock.Lock()
	defer qm.monitorLock.Unlock()

	if !qm.running {
		return
	}

	// we're waiting until after the informer start that happens once all the controllers are initialized.  This ensures
	// that they don't get unexpected events on their work queues.
	<-qm.informersStarted

	monitors := qm.monitors
	started := 0
	for _, monitor := range monitors {
		if monitor.stopCh == nil {
			monitor.stopCh = make(chan struct{})
			qm.informerFactory.Start(qm.stopCh)
			go monitor.Run()
			started++
		}
	}
	klog.FromContext(ctx).V(4).Info("QuotaMonitor finished starting monitors", "new", started, "total", len(monitors))
}

// IsSynced returns true if any monitors exist AND all those monitors'
// controllers HasSynced functions return true. This means IsSynced could return
// true at one time, and then later return false if all monitors were
// reconstructed.
func (qm *QuotaMonitor) IsSynced(ctx context.Context) bool {
	logger := klog.FromContext(ctx)

	qm.monitorLock.RLock()
	defer qm.monitorLock.RUnlock()

	if len(qm.monitors) == 0 {
		logger.V(4).Info("quota monitor not synced: no monitors")
		return false
	}

	for resource, monitor := range qm.monitors {
		if !monitor.controller.HasSynced() {
			logger.V(4).Info("quota monitor not synced", "resource", resource)
			return false
		}
	}
	return true
}

// Run sets the stop channel and starts monitor execution until stopCh is
// closed. Any running monitors will be stopped before Run returns.
func (qm *QuotaMonitor) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	logger := klog.FromContext(ctx)

	logger.Info("QuotaMonitor running")
	defer logger.Info("QuotaMonitor stopping")

	// Set up the stop channel.
	qm.monitorLock.Lock()
	qm.stopCh = ctx.Done()
	qm.running = true
	qm.monitorLock.Unlock()

	// Start monitors and begin change processing until the stop channel is
	// closed.
	qm.StartMonitors(ctx)

	// The following workers are hanging forever until the queue is
	// shut down, so we need to shut it down in a separate goroutine.
	go func() {
		defer utilruntime.HandleCrash()
		defer qm.resourceChanges.ShutDown()

		<-ctx.Done()
	}()
	wait.UntilWithContext(ctx, qm.runProcessResourceChanges, 1*time.Second)

	// Stop any running monitors.
	qm.monitorLock.Lock()
	defer qm.monitorLock.Unlock()
	monitors := qm.monitors
	stopped := 0
	for _, monitor := range monitors {
		if monitor.stopCh != nil {
			stopped++
			close(monitor.stopCh)
		}
	}
	logger.Info("QuotaMonitor stopped monitors", "stopped", stopped, "total", len(monitors))
}

func (qm *QuotaMonitor) runProcessResourceChanges(ctx context.Context) {
	for qm.processResourceChanges(ctx) {
	}
}

// Dequeueing an event from resourceChanges to process
func (qm *QuotaMonitor) processResourceChanges(ctx context.Context) bool {
	item, quit := qm.resourceChanges.Get()
	if quit {
		return false
	}
	defer qm.resourceChanges.Done(item)
	event := item
	obj := event.obj
	accessor, err := meta.Accessor(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("cannot access obj: %v", err))
		return true
	}
	klog.FromContext(ctx).V(4).Info("QuotaMonitor process object",
		"resource", event.gvr.String(),
		"namespace", accessor.GetNamespace(),
		"name", accessor.GetName(),
		"uid", string(accessor.GetUID()),
		"eventType", event.eventType,
	)
	qm.replenishmentFunc(ctx, event.gvr.GroupResource(), accessor.GetNamespace())
	return true
}
