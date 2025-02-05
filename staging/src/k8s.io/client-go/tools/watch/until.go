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

package watch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// PreconditionFunc returns true if the condition has been reached, false if it has not been reached yet,
// or an error if the condition failed or detected an error state.
type PreconditionFunc func(store cache.Store) (bool, error)

// ConditionFunc returns true if the condition has been reached, false if it has not been reached yet,
// or an error if the condition cannot be checked and should terminate. In general, it is better to define
// level driven conditions over edge driven conditions (pod has ready=true, vs pod modified and ready changed
// from false to true).
type ConditionFunc func(event watch.Event) (bool, error)

// ErrWatchClosed is returned when the watch channel is closed before timeout in UntilWithoutRetry.
var ErrWatchClosed = errors.New("watch closed before UntilWithoutRetry timeout")

// UntilWithoutRetry reads items from the watch until each provided condition succeeds, and then returns the last watch
// encountered. The first condition that returns an error terminates the watch (and the event is also returned).
// If no event has been received, the returned event will be nil.
// Conditions are satisfied sequentially so as to provide a useful primitive for higher level composition.
// Waits until context deadline or until context is canceled.
//
// Warning: Unless you have a very specific use case (probably a special Watcher) don't use this function!!!
// Warning: This will fail e.g. on API timeouts and/or 'too old resource version' error.
// Warning: You are most probably looking for a function *Until* or *UntilWithSync* below,
// Warning: solving such issues.
// TODO: Consider making this function private to prevent misuse when the other occurrences in our codebase are gone.
func UntilWithoutRetry(ctx context.Context, watcher watch.Interface, conditions ...ConditionFunc) (*watch.Event, error) {
	ch := watcher.ResultChan()
	defer watcher.Stop()
	var lastEvent *watch.Event
	for _, condition := range conditions {
		// check the next condition against the previous event and short circuit waiting for the next watch
		if lastEvent != nil {
			done, err := condition(*lastEvent)
			if err != nil {
				return lastEvent, err
			}
			if done {
				continue
			}
		}
	ConditionSucceeded:
		for {
			select {
			case event, ok := <-ch:
				if !ok {
					return lastEvent, ErrWatchClosed
				}
				lastEvent = &event

				done, err := condition(event)
				if err != nil {
					return lastEvent, err
				}
				if done {
					break ConditionSucceeded
				}

			case <-ctx.Done():
				return lastEvent, wait.ErrWaitTimeout
			}
		}
	}
	return lastEvent, nil
}

// Until wraps the watcherClient's watch function with RetryWatcher making sure that watcher gets restarted in case of errors.
// The initialResourceVersion will be given to watch method when first called. It shall not be "" or "0"
// given the underlying WATCH call issues (#74022).
// Remaining behaviour is identical to function UntilWithoutRetry. (See above.)
// Until can deal with API timeouts and lost connections.
// It guarantees you to see all events and in the order they happened.
// Due to this guarantee there is no way it can deal with 'Resource version too old error'. It will fail in this case.
// (See `UntilWithSync` if you'd prefer to recover from all the errors including RV too old by re-listing
// those items. In normal code you should care about being level driven so you'd not care about not seeing all the edges.)
//
// The most frequent usage for Until would be a test where you want to verify exact order of events ("edges").
func Until(ctx context.Context, initialResourceVersion string, watcherClient cache.Watcher, conditions ...ConditionFunc) (*watch.Event, error) {
	w, err := NewRetryWatcherWithContext(ctx, initialResourceVersion, cache.ToWatcherWithContext(watcherClient))
	if err != nil {
		return nil, err
	}

	return UntilWithoutRetry(ctx, w, conditions...)
}

// UntilWithSync creates an informer from lw, optionally checks precondition when the store is synced,
// and watches the output until each provided condition succeeds, in a way that is identical
// to function UntilWithoutRetry. (See above.)
// UntilWithSync can deal with all errors like API timeout, lost connections and 'Resource version too old'.
// It is the only function that can recover from 'Resource version too old', Until and UntilWithoutRetry will
// just fail in that case. On the other hand it can't provide you with guarantees as strong as using simple
// Watch method with Until. It can skip some intermediate events in case of watch function failing but it will
// re-list to recover and you always get an event, if there has been a change, after recovery.
// Also with the current implementation based on DeltaFIFO, order of the events you receive is guaranteed only for
// particular object, not between more of them even it's the same resource.
// The most frequent usage would be a command that needs to watch the "state of the world" and should't fail, like:
// waiting for object reaching a state, "small" controllers, ...
func UntilWithSync(ctx context.Context, lw cache.ListerWatcher, objType runtime.Object, precondition PreconditionFunc, conditions ...ConditionFunc) (*watch.Event, error) {
	indexer, informer, watcher, done := NewIndexerInformerWatcherWithLogger(klog.FromContext(ctx), lw, objType)
	// We need to wait for the internal informers to fully stop so it's easier to reason about
	// and it works with non-thread safe clients.
	defer func() { <-done }()
	// Proxy watcher can be stopped multiple times so it's fine to use defer here to cover alternative branches and
	// let UntilWithoutRetry to stop it
	defer watcher.Stop()

	if precondition != nil {
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			return nil, fmt.Errorf("UntilWithSync: unable to sync caches: %w", ctx.Err())
		}

		done, err := precondition(indexer)
		if err != nil {
			return nil, err
		}

		if done {
			return nil, nil
		}
	}

	return UntilWithoutRetry(ctx, watcher, conditions...)
}

// ContextWithOptionalTimeout wraps context.WithTimeout and handles infinite timeouts expressed as 0 duration.
func ContextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout < 0 {
		// This should be handled in validation
		klog.FromContext(parent).Error(nil, "Timeout for context shall not be negative")
		timeout = 0
	}

	if timeout == 0 {
		return context.WithCancel(parent)
	}

	return context.WithTimeout(parent, timeout)
}
