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

package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// resourceVersionGetter is an interface used to get resource version from events.
// We can't reuse an interface from meta otherwise it would be a cyclic dependency and we need just this one method
type resourceVersionGetter interface {
	GetResourceVersion() string
}

// RetryWatcher will make sure that in case the underlying watcher is closed (e.g. due to API timeout or etcd timeout)
// it will get restarted from the last point without the consumer even knowing about it.
// RetryWatcher does that by inspecting events and keeping track of resourceVersion.
// Especially useful when using watch.UntilWithoutRetry where premature termination is causing issues and flakes.
// Please note that this is not resilient to etcd cache not having the resource version anymore - you would need to
// use Informers for that.
type RetryWatcher struct {
	cancel              func(error)
	lastResourceVersion string
	watcherClient       cache.WatcherWithContext
	resultChan          chan watch.Event
	doneChan            chan struct{}
	minRestartDelay     time.Duration
}

// NewRetryWatcher creates a new RetryWatcher.
// It will make sure that watches gets restarted in case of recoverable errors.
// The initialResourceVersion will be given to watch method when first called.
//
// Deprecated: use NewRetryWatcherWithContext instead.
func NewRetryWatcher(initialResourceVersion string, watcherClient cache.Watcher) (*RetryWatcher, error) {
	return NewRetryWatcherWithContext(context.Background(), initialResourceVersion, cache.ToWatcherWithContext(watcherClient))
}

// NewRetryWatcherWithContext creates a new RetryWatcher.
// It will make sure that watches gets restarted in case of recoverable errors.
// The initialResourceVersion will be given to watch method when first called.
func NewRetryWatcherWithContext(ctx context.Context, initialResourceVersion string, watcherClient cache.WatcherWithContext) (*RetryWatcher, error) {
	return newRetryWatcher(ctx, initialResourceVersion, watcherClient, 1*time.Second)
}

func newRetryWatcher(ctx context.Context, initialResourceVersion string, watcherClient cache.WatcherWithContext, minRestartDelay time.Duration) (*RetryWatcher, error) {
	switch initialResourceVersion {
	case "", "0":
		// TODO: revisit this if we ever get WATCH v2 where it means start "now"
		//       without doing the synthetic list of objects at the beginning (see #74022)
		return nil, fmt.Errorf("initial RV %q is not supported due to issues with underlying WATCH", initialResourceVersion)
	default:
		break
	}

	ctx, cancel := context.WithCancelCause(ctx)

	rw := &RetryWatcher{
		cancel:              cancel,
		lastResourceVersion: initialResourceVersion,
		watcherClient:       watcherClient,
		doneChan:            make(chan struct{}),
		resultChan:          make(chan watch.Event, 0),
		minRestartDelay:     minRestartDelay,
	}

	go rw.receive(ctx)
	return rw, nil
}

func (rw *RetryWatcher) send(ctx context.Context, event watch.Event) bool {
	// Writing to an unbuffered channel is blocking operation
	// and we need to check if stop wasn't requested while doing so.
	select {
	case rw.resultChan <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// doReceive returns true when it is done, false otherwise.
// If it is not done the second return value holds the time to wait before calling it again.
func (rw *RetryWatcher) doReceive(ctx context.Context) (bool, time.Duration) {
	watcher, err := rw.watcherClient.WatchWithContext(ctx, metav1.ListOptions{
		ResourceVersion:     rw.lastResourceVersion,
		AllowWatchBookmarks: true,
	})
	// We are very unlikely to hit EOF here since we are just establishing the call,
	// but it may happen that the apiserver is just shutting down (e.g. being restarted)
	// This is consistent with how it is handled for informers
	switch err {
	case nil:
		break

	case io.EOF:
		// watch closed normally
		return false, 0

	case io.ErrUnexpectedEOF:
		klog.FromContext(ctx).V(1).Info("Watch closed with unexpected EOF", "err", err)
		return false, 0

	default:
		msg := "Watch failed"
		if net.IsProbableEOF(err) || net.IsTimeout(err) {
			klog.FromContext(ctx).V(5).Info(msg, "err", err)
			// Retry
			return false, 0
		}

		// Check if the watch failed due to the client not having permission to watch the resource or the credentials
		// being invalid (e.g. expired token).
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			// Add more detail since the forbidden message returned by the Kubernetes API is just "unknown".
			klog.FromContext(ctx).Error(err, msg+": ensure the client has valid credentials and watch permissions on the resource")

			if apiStatus, ok := err.(apierrors.APIStatus); ok {
				statusErr := apiStatus.Status()

				sent := rw.send(ctx, watch.Event{
					Type:   watch.Error,
					Object: &statusErr,
				})
				if !sent {
					// This likely means the RetryWatcher is stopping but return false so the caller to doReceive can
					// verify this and potentially retry.
					klog.FromContext(ctx).Error(nil, "Failed to send the Unauthorized or Forbidden watch event")

					return false, 0
				}
			} else {
				// This should never happen since apierrors only handles apierrors.APIStatus. Still, this is an
				// unrecoverable error, so still allow it to return true below.
				klog.FromContext(ctx).Error(err, msg+": encountered an unexpected Unauthorized or Forbidden error type")
			}

			return true, 0
		}

		klog.FromContext(ctx).Error(err, msg)
		// Retry
		return false, 0
	}

	if watcher == nil {
		klog.FromContext(ctx).Error(nil, "Watch returned nil watcher")
		// Retry
		return false, 0
	}

	ch := watcher.ResultChan()
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.FromContext(ctx).V(4).Info("Stopping RetryWatcher")
			return true, 0
		case event, ok := <-ch:
			if !ok {
				klog.FromContext(ctx).V(4).Info("Failed to get event - re-creating the watcher", "resourceVersion", rw.lastResourceVersion)
				return false, 0
			}

			// We need to inspect the event and get ResourceVersion out of it
			switch event.Type {
			case watch.Added, watch.Modified, watch.Deleted, watch.Bookmark:
				metaObject, ok := event.Object.(resourceVersionGetter)
				if !ok {
					_ = rw.send(ctx, watch.Event{
						Type:   watch.Error,
						Object: &apierrors.NewInternalError(errors.New("retryWatcher: doesn't support resourceVersion")).ErrStatus,
					})
					// We have to abort here because this might cause lastResourceVersion inconsistency by skipping a potential RV with valid data!
					return true, 0
				}

				resourceVersion := metaObject.GetResourceVersion()
				if resourceVersion == "" {
					_ = rw.send(ctx, watch.Event{
						Type:   watch.Error,
						Object: &apierrors.NewInternalError(fmt.Errorf("retryWatcher: object %#v doesn't support resourceVersion", event.Object)).ErrStatus,
					})
					// We have to abort here because this might cause lastResourceVersion inconsistency by skipping a potential RV with valid data!
					return true, 0
				}

				// All is fine; send the non-bookmark events and update resource version.
				if event.Type != watch.Bookmark {
					ok = rw.send(ctx, event)
					if !ok {
						return true, 0
					}
				}
				rw.lastResourceVersion = resourceVersion

				continue

			case watch.Error:
				// This round trip allows us to handle unstructured status
				errObject := apierrors.FromObject(event.Object)
				statusErr, ok := errObject.(*apierrors.StatusError)
				if !ok {
					klog.FromContext(ctx).Error(nil, "Received an error which is not *metav1.Status", "errorObject", dump.Pretty(event.Object))
					// Retry unknown errors
					return false, 0
				}

				status := statusErr.ErrStatus

				statusDelay := time.Duration(0)
				if status.Details != nil {
					statusDelay = time.Duration(status.Details.RetryAfterSeconds) * time.Second
				}

				switch status.Code {
				case http.StatusGone:
					// Never retry RV too old errors
					_ = rw.send(ctx, event)
					return true, 0

				case http.StatusGatewayTimeout, http.StatusInternalServerError:
					// Retry
					return false, statusDelay

				default:
					// We retry by default. RetryWatcher is meant to proceed unless it is certain
					// that it can't. If we are not certain, we proceed with retry and leave it
					// up to the user to timeout if needed.

					// Log here so we have a record of hitting the unexpected error
					// and we can whitelist some error codes if we missed any that are expected.
					klog.FromContext(ctx).V(5).Info("Retrying after unexpected error", "errorObject", dump.Pretty(event.Object))

					// Retry
					return false, statusDelay
				}

			default:
				klog.FromContext(ctx).Error(nil, "Failed to recognize event", "type", event.Type)
				_ = rw.send(ctx, watch.Event{
					Type:   watch.Error,
					Object: &apierrors.NewInternalError(fmt.Errorf("retryWatcher failed to recognize Event type %q", event.Type)).ErrStatus,
				})
				// We are unable to restart the watch and have to stop the loop or this might cause lastResourceVersion inconsistency by skipping a potential RV with valid data!
				return true, 0
			}
		}
	}
}

// receive reads the result from a watcher, restarting it if necessary.
func (rw *RetryWatcher) receive(ctx context.Context) {
	defer close(rw.doneChan)
	defer close(rw.resultChan)

	logger := klog.FromContext(ctx)
	logger.V(4).Info("Starting RetryWatcher")
	defer logger.V(4).Info("Stopping RetryWatcher")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// We use non sliding until so we don't introduce delays on happy path when WATCH call
	// timeouts or gets closed and we need to reestablish it while also avoiding hot loops.
	wait.NonSlidingUntilWithContext(ctx, func(ctx context.Context) {
		done, retryAfter := rw.doReceive(ctx)
		if done {
			cancel()
			return
		}

		timer := time.NewTimer(retryAfter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		logger.V(4).Info("Restarting RetryWatcher", "resourceVersion", rw.lastResourceVersion)
	}, rw.minRestartDelay)
}

// ResultChan implements Interface.
func (rw *RetryWatcher) ResultChan() <-chan watch.Event {
	return rw.resultChan
}

// Stop implements Interface.
func (rw *RetryWatcher) Stop() {
	rw.cancel(errors.New("asked to stop"))
}

// Done allows the caller to be notified when Retry watcher stops.
func (rw *RetryWatcher) Done() <-chan struct{} {
	return rw.doneChan
}
