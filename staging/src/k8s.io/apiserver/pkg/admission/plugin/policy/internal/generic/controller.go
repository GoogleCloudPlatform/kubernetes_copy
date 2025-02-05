/*
Copyright 2022 The Kubernetes Authors.

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

package generic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/cache/synctrack"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

var _ Controller[runtime.Object] = &controller[runtime.Object]{}

type controller[T runtime.Object] struct {
	informer Informer[T]
	queue    workqueue.TypedRateLimitingInterface[string]

	// Returns an error if there was a transient error during reconciliation
	// and the object should be tried again later.
	reconciler func(namespace, name string, newObj T) error

	options ControllerOptions

	// must hold a func() bool or nil
	notificationsDelivered atomic.Value

	hasProcessed synctrack.AsyncTracker[string]
}

type ControllerOptions struct {
	Name    string
	Workers uint
}

func (c *controller[T]) Informer() Informer[T] {
	return c.informer
}

func NewController[T runtime.Object](
	informer Informer[T],
	reconciler func(namepace, name string, newObj T) error,
	options ControllerOptions,
) Controller[T] {
	if options.Workers == 0 {
		options.Workers = 2
	}

	if len(options.Name) == 0 {
		options.Name = fmt.Sprintf("%T-controller", *new(T))
	}

	c := &controller[T]{
		options:    options,
		informer:   informer,
		reconciler: reconciler,
		queue:      nil,
	}
	c.hasProcessed.UpstreamHasSynced = func() bool {
		f := c.notificationsDelivered.Load()
		if f == nil {
			return false
		}
		return f.(func() bool)()
	}
	return c
}

// Runs the controller and returns an error explaining why running was stopped.
// Reconciliation ends as soon as the context completes. If there are events
// waiting to be processed at that itme, they will be dropped.
func (c *controller[T]) Run(ctx context.Context) error {
	klog.Infof("starting %s", c.options.Name)
	defer klog.Infof("stopping %s", c.options.Name)

	c.queue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: c.options.Name},
	)

	// Forcefully shutdown workqueue. Drop any enqueued items.
	// Important to do this in a `defer` at the start of `Run`.
	// Otherwise, if there are any early returns without calling this, we
	// would never shut down the workqueue
	defer c.queue.ShutDown()

	enqueue := func(obj interface{}, isInInitialList bool) {
		var key string
		var err error
		if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
			utilruntime.HandleError(err)
			return
		}
		if isInInitialList {
			c.hasProcessed.Start(key)
		}

		c.queue.Add(key)
	}

	registration, err := c.informer.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: enqueue,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldMeta, err1 := meta.Accessor(oldObj)
			newMeta, err2 := meta.Accessor(newObj)

			if err1 != nil || err2 != nil {
				if err1 != nil {
					utilruntime.HandleError(err1)
				}

				if err2 != nil {
					utilruntime.HandleError(err2)
				}
				return
			} else if oldMeta.GetResourceVersion() == newMeta.GetResourceVersion() {
				if len(oldMeta.GetResourceVersion()) == 0 {
					klog.Warningf("%v throwing out update with empty RV. this is likely to happen if a test did not supply a resource version on an updated object", c.options.Name)
				}
				return
			}

			enqueue(newObj, false)
		},
		DeleteFunc: func(obj interface{}) {
			// Enqueue
			enqueue(obj, false)
		},
	})

	// Error might be raised if informer was started and stopped already
	if err != nil {
		return err
	}

	c.notificationsDelivered.Store(registration.HasSynced)

	// Make sure event handler is removed from informer in case return early from
	// an error
	defer func() {
		c.notificationsDelivered.Store(func() bool { return false })
		// Remove event handler and Handle Error here. Error should only be raised
		// for improper usage of event handler API.
		if err := c.informer.RemoveEventHandler(registration); err != nil {
			utilruntime.HandleError(err)
		}
	}()

	// Wait for initial cache list to complete before beginning to reconcile
	// objects.
	if !cache.WaitForNamedCacheSync(c.options.Name, ctx.Done(), c.informer.HasSynced) {
		// ctx cancelled during cache sync. return early
		err := ctx.Err()
		if err == nil {
			// if context wasn't cancelled then the sync failed for another reason
			err = errors.New("cache sync failed")
		}
		return err
	}

	waitGroup := sync.WaitGroup{}

	for i := uint(0); i < c.options.Workers; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			wait.Until(c.runWorker, time.Second, ctx.Done())
		}()
	}

	klog.Infof("Started %v workers for %v", c.options.Workers, c.options.Name)

	// Wait for context cancel.
	<-ctx.Done()

	// Forcefully shutdown workqueue. Drop any enqueued items.
	c.queue.ShutDown()

	// Workqueue shutdown signals for workers to stop. Wait for all workers to
	// clean up
	waitGroup.Wait()

	// Only way for workers to ever stop is for caller to cancel the context
	return ctx.Err()
}

func (c *controller[T]) HasSynced() bool {
	return c.hasProcessed.HasSynced()
}

func (c *controller[T]) runWorker() {
	for {
		key, shutdown := c.queue.Get()
		if shutdown {
			return
		}

		// We wrap this block in a func so we can defer c.workqueue.Done.
		err := func(obj string) error {
			// We call Done here so the workqueue knows we have finished
			// processing this item. We also must remember to call Forget if we
			// do not want this work item being re-queued. For example, we do
			// not call Forget if a transient error occurs, instead the item is
			// put back on the workqueue and attempted again after a back-off
			// period.
			defer c.queue.Done(obj)
			defer c.hasProcessed.Finished(key)

			if err := c.reconcile(key); err != nil {
				// Put the item back on the workqueue to handle any transient errors.
				c.queue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
			}
			// Finally, if no error occurs we Forget this item so it is allowed
			// to be re-enqueued without a long rate limit
			c.queue.Forget(obj)
			klog.V(4).Infof("syncAdmissionPolicy(%q)", key)
			return nil
		}(key)

		if err != nil {
			utilruntime.HandleError(err)
		}
	}
}

func (c *controller[T]) reconcile(key string) error {
	var newObj T
	var err error
	var namespace string
	var name string
	var lister NamespacedLister[T]

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	if len(namespace) > 0 {
		lister = c.informer.Namespaced(namespace)
	} else {
		lister = c.informer
	}

	newObj, err = lister.Get(name)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return err
		}

		// Deleted object. Inform reconciler with empty
	}

	return c.reconciler(namespace, name, newObj)
}
