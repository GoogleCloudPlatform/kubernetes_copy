/*
Copyright 2019 The Kubernetes Authors.

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

package filters

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	flowcontrol "k8s.io/api/flowcontrol/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	epmetrics "k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server/httplog"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	fcmetrics "k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	flowcontrolrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/klog/v2"
	utilsclock "k8s.io/utils/clock"
)

// PriorityAndFairnessClassification identifies the results of
// classification for API Priority and Fairness
type PriorityAndFairnessClassification struct {
	FlowSchemaName    string
	FlowSchemaUID     apitypes.UID
	PriorityLevelName string
	PriorityLevelUID  apitypes.UID
}

// waitingMark tracks requests waiting rather than being executed
var waitingMark = &requestWatermark{
	phase: epmetrics.WaitingPhase,
}

var atomicMutatingExecuting, atomicReadOnlyExecuting int32
var atomicMutatingWaiting, atomicReadOnlyWaiting int32

// newInitializationSignal is defined for testing purposes.
var newInitializationSignal = utilflowcontrol.NewInitializationSignal

func truncateLogField(s string) string {
	const maxFieldLogLength = 64

	if len(s) > maxFieldLogLength {
		s = s[0:maxFieldLogLength]
	}
	return s
}

var initAPFOnce sync.Once

type priorityAndFairnessHandler struct {
	handler                 http.Handler
	longRunningRequestCheck apirequest.LongRunningRequestCheck
	fcIfc                   utilflowcontrol.Interface
	workEstimator           flowcontrolrequest.WorkEstimatorFunc

	// droppedRequests tracks the history of dropped requests for
	// the purpose of computing RetryAfter header to avoid system
	// overload.
	droppedRequests utilflowcontrol.DroppedRequestsTracker

	// newReqWaitCtxFn creates a derived context with a deadline
	// of how long a given request can wait in its queue.
	newReqWaitCtxFn func(context.Context) (context.Context, context.CancelFunc)
}

func (h *priorityAndFairnessHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestInfo, ok := apirequest.RequestInfoFrom(ctx)
	if !ok {
		handleError(w, r, fmt.Errorf("no RequestInfo found in context"))
		return
	}
	user, ok := apirequest.UserFrom(ctx)
	if !ok {
		handleError(w, r, fmt.Errorf("no User found in context"))
		return
	}

	isWatchRequest := watchVerbs.Has(requestInfo.Verb)

	// Skip tracking long running non-watch requests.
	if h.longRunningRequestCheck != nil && h.longRunningRequestCheck(r, requestInfo) && !isWatchRequest {
		klog.V(6).Infof("Serving RequestInfo=%#+v, user.Info=%#+v as longrunning\n", requestInfo, user)
		h.handler.ServeHTTP(w, r)
		return
	}

	var classification *PriorityAndFairnessClassification
	noteFn := func(fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, flowDistinguisher string) {
		classification = &PriorityAndFairnessClassification{
			FlowSchemaName:    fs.Name,
			FlowSchemaUID:     fs.UID,
			PriorityLevelName: pl.Name,
			PriorityLevelUID:  pl.UID,
		}

		httplog.AddKeyValue(ctx, "apf_pl", truncateLogField(pl.Name))
		httplog.AddKeyValue(ctx, "apf_fs", truncateLogField(fs.Name))
	}
	// estimateWork is called, if at all, after noteFn
	estimateWork := func() flowcontrolrequest.WorkEstimate {
		if classification == nil {
			// workEstimator is being invoked before classification of
			// the request has completed, we should never be here though.
			klog.ErrorS(fmt.Errorf("workEstimator is being invoked before classification of the request has completed"),
				"Using empty FlowSchema and PriorityLevelConfiguration name", "verb", r.Method, "URI", r.RequestURI)
			return h.workEstimator(r, "", "")
		}

		workEstimate := h.workEstimator(r, classification.FlowSchemaName, classification.PriorityLevelName)

		fcmetrics.ObserveWorkEstimatedSeats(classification.PriorityLevelName, classification.FlowSchemaName, workEstimate.MaxSeats())
		httplog.AddKeyValue(ctx, "apf_iseats", workEstimate.InitialSeats)
		httplog.AddKeyValue(ctx, "apf_fseats", workEstimate.FinalSeats)
		httplog.AddKeyValue(ctx, "apf_additionalLatency", workEstimate.AdditionalLatency)

		return workEstimate
	}

	var served bool
	isMutatingRequest := !nonMutatingRequestVerbs.Has(requestInfo.Verb)
	noteExecutingDelta := func(delta int32) {
		if isMutatingRequest {
			watermark.recordMutating(int(atomic.AddInt32(&atomicMutatingExecuting, delta)))
		} else {
			watermark.recordReadOnly(int(atomic.AddInt32(&atomicReadOnlyExecuting, delta)))
		}
	}
	noteWaitingDelta := func(delta int32) {
		if isMutatingRequest {
			waitingMark.recordMutating(int(atomic.AddInt32(&atomicMutatingWaiting, delta)))
		} else {
			waitingMark.recordReadOnly(int(atomic.AddInt32(&atomicReadOnlyWaiting, delta)))
		}
	}
	queueNote := func(inQueue bool) {
		if inQueue {
			noteWaitingDelta(1)
		} else {
			noteWaitingDelta(-1)
		}
	}

	digest := utilflowcontrol.RequestDigest{
		RequestInfo: requestInfo,
		User:        user,
	}

	if isWatchRequest {
		// This channel blocks calling handler.ServeHTTP() until closed, and is closed inside execute().
		// If APF rejects the request, it is never closed.
		shouldStartWatchCh := make(chan struct{})

		watchInitializationSignal := newInitializationSignal()
		// This wraps the request passed to handler.ServeHTTP(),
		// setting a context that plumbs watchInitializationSignal to storage
		var watchReq *http.Request
		// This is set inside execute(), prior to closing shouldStartWatchCh.
		// If the request is rejected by APF it is left nil.
		var forgetWatch utilflowcontrol.ForgetWatchFunc

		defer func() {
			// Protect from the situation when request will not reach storage layer
			// and the initialization signal will not be send.
			if watchInitializationSignal != nil {
				watchInitializationSignal.Signal()
			}
			// Forget the watcher if it was registered.
			//
			// This is race-free because by this point, one of the following occurred:
			// case <-shouldStartWatchCh: execute() completed the assignment to forgetWatch
			// case <-resultCh: Handle() completed, and Handle() does not return
			//   while execute() is running
			if forgetWatch != nil {
				forgetWatch()
			}
		}()

		execute := func() {
			startedAt := time.Now()
			defer func() {
				httplog.AddKeyValue(ctx, "apf_init_latency", time.Since(startedAt))
			}()
			noteExecutingDelta(1)
			defer noteExecutingDelta(-1)
			served = true
			setResponseHeaders(classification, w)

			forgetWatch = h.fcIfc.RegisterWatch(r)

			// Notify the main thread that we're ready to start the watch.
			close(shouldStartWatchCh)

			// Wait until the request is finished from the APF point of view
			// (which is when its initialization is done).
			watchInitializationSignal.Wait()
		}

		// Ensure that an item can be put to resultCh asynchronously.
		resultCh := make(chan interface{}, 1)

		// Call Handle in a separate goroutine.
		// The reason for it is that from APF point of view, the request processing
		// finishes as soon as watch is initialized (which is generally orders of
		// magnitude faster than the watch request itself). This means that Handle()
		// call finishes much faster and for performance reasons we want to reduce
		// the number of running goroutines - so we run the shorter thing in a
		// dedicated goroutine and the actual watch handler in the main one.
		go func() {
			defer func() {
				err := recover()
				// do not wrap the sentinel ErrAbortHandler panic value
				if err != nil && err != http.ErrAbortHandler {
					// Same as stdlib http server code. Manually allocate stack
					// trace buffer size to prevent excessively large logs
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					err = fmt.Sprintf("%v\n%s", err, buf)
				}

				// Ensure that the result is put into resultCh independently of the panic.
				resultCh <- err
			}()

			// We create handleCtx with an adjusted deadline, for two reasons.
			// One is to limit the time the request waits before its execution starts.
			// The other reason for it is that Handle() underneath may start additional goroutine
			// that is blocked on context cancellation. However, from APF point of view,
			// we don't want to wait until the whole watch request is processed (which is
			// when it context is actually cancelled) - we want to unblock the goroutine as
			// soon as the request is processed from the APF point of view.
			//
			// Note that we explicitly do NOT call the actual handler using that context
			// to avoid cancelling request too early.
			handleCtx, handleCtxCancel := h.newReqWaitCtxFn(ctx)
			defer handleCtxCancel()

			// Note that Handle will return irrespective of whether the request
			// executes or is rejected. In the latter case, the function will return
			// without calling the passed `execute` function.
			h.fcIfc.Handle(handleCtx, digest, noteFn, estimateWork, queueNote, execute)
		}()

		select {
		case <-shouldStartWatchCh:
			func() {
				// TODO: if both goroutines panic, propagate the stack traces from both
				// goroutines so they are logged properly:
				defer func() {
					// Protect from the situation when request will not reach storage layer
					// and the initialization signal will not be send.
					// It has to happen before waiting on the resultCh below.
					watchInitializationSignal.Signal()
					// TODO: Consider finishing the request as soon as Handle call panics.
					if err := <-resultCh; err != nil {
						panic(err)
					}
				}()
				watchCtx := utilflowcontrol.WithInitializationSignal(ctx, watchInitializationSignal)
				watchReq = r.WithContext(watchCtx)
				h.handler.ServeHTTP(w, watchReq)
			}()
		case err := <-resultCh:
			if err != nil {
				panic(err)
			}
		}
	} else {
		execute := func() {
			noteExecutingDelta(1)
			defer noteExecutingDelta(-1)
			served = true
			setResponseHeaders(classification, w)

			h.handler.ServeHTTP(w, r)
		}

		func() {
			handleCtx, cancelFn := h.newReqWaitCtxFn(ctx)
			defer cancelFn()
			h.fcIfc.Handle(handleCtx, digest, noteFn, estimateWork, queueNote, execute)
		}()
	}

	if !served {
		setResponseHeaders(classification, w)

		epmetrics.RecordDroppedRequest(r, requestInfo, epmetrics.APIServerComponent, isMutatingRequest)
		epmetrics.RecordRequestTermination(r, requestInfo, epmetrics.APIServerComponent, http.StatusTooManyRequests)
		h.droppedRequests.RecordDroppedRequest(classification.PriorityLevelName)

		// TODO(wojtek-t): Idea from deads2k: we can consider some jittering and in case of non-int
		//  number, just return the truncated result and sleep the remainder server-side.
		tooManyRequests(r, w, strconv.Itoa(int(h.droppedRequests.GetRetryAfter(classification.PriorityLevelName))))
	}
}

// WithPriorityAndFairness limits the number of in-flight
// requests in a fine-grained way.
func WithPriorityAndFairness(
	handler http.Handler,
	longRunningRequestCheck apirequest.LongRunningRequestCheck,
	fcIfc utilflowcontrol.Interface,
	workEstimator flowcontrolrequest.WorkEstimatorFunc,
	defaultRequestWaitLimit time.Duration,
) http.Handler {
	if fcIfc == nil {
		klog.Warningf("priority and fairness support not found, skipping")
		return handler
	}
	initAPFOnce.Do(func() {
		initMaxInFlight(0, 0)
		// Fetching these gauges is delayed until after their underlying metric has been registered
		// so that this latches onto the efficient implementation.
		waitingMark.readOnlyObserver = fcmetrics.GetWaitingReadonlyConcurrency()
		waitingMark.mutatingObserver = fcmetrics.GetWaitingMutatingConcurrency()
	})

	clock := &utilsclock.RealClock{}
	newReqWaitCtxFn := func(ctx context.Context) (context.Context, context.CancelFunc) {
		return getRequestWaitContext(ctx, defaultRequestWaitLimit, clock)
	}

	priorityAndFairnessHandler := &priorityAndFairnessHandler{
		handler:                 handler,
		longRunningRequestCheck: longRunningRequestCheck,
		fcIfc:                   fcIfc,
		workEstimator:           workEstimator,
		droppedRequests:         utilflowcontrol.NewDroppedRequestsTracker(),
		newReqWaitCtxFn:         newReqWaitCtxFn,
	}
	return http.HandlerFunc(priorityAndFairnessHandler.Handle)
}

// StartPriorityAndFairnessWatermarkMaintenance starts the goroutines to observe and maintain watermarks for
// priority-and-fairness requests.
func StartPriorityAndFairnessWatermarkMaintenance(stopCh <-chan struct{}) {
	startWatermarkMaintenance(watermark, stopCh)
	startWatermarkMaintenance(waitingMark, stopCh)
}

func setResponseHeaders(classification *PriorityAndFairnessClassification, w http.ResponseWriter) {
	if classification == nil {
		return
	}

	// We intentionally set the UID of the flow-schema and priority-level instead of name. This is so that
	// the names that cluster-admins choose for categorization and priority levels are not exposed, also
	// the names might make it obvious to the users that they are rejected due to classification with low priority.
	w.Header().Set(flowcontrol.ResponseHeaderMatchedPriorityLevelConfigurationUID, string(classification.PriorityLevelUID))
	w.Header().Set(flowcontrol.ResponseHeaderMatchedFlowSchemaUID, string(classification.FlowSchemaUID))
}

func tooManyRequests(req *http.Request, w http.ResponseWriter, retryAfter string) {
	// Return a 429 status indicating "Too Many Requests"
	w.Header().Set("Retry-After", retryAfter)
	http.Error(w, "Too many requests, please try again later.", http.StatusTooManyRequests)
}

// getRequestWaitContext returns a new context with a deadline of how
// long the request is allowed to wait before it is removed from its
// queue and rejected.
// The context.CancelFunc returned must never be nil and the caller is
// responsible for calling the CancelFunc function for cleanup.
//   - ctx: the context associated with the request (it may or may
//     not have a deadline).
//   - defaultRequestWaitLimit: the default wait duration that is used
//     if the request context does not have any deadline.
//     (a) initialization of a watch or
//     (b) a request whose context has no deadline
//
// clock comes in handy for testing the function
func getRequestWaitContext(ctx context.Context, defaultRequestWaitLimit time.Duration, clock utilsclock.PassiveClock) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		return ctx, func() {}
	}

	reqArrivedAt := clock.Now()
	if reqReceivedTimestamp, ok := apirequest.ReceivedTimestampFrom(ctx); ok {
		reqArrivedAt = reqReceivedTimestamp
	}

	// a) we will allow the request to wait in the queue for one
	// fourth of the time of its allotted deadline.
	// b) if the request context does not have any deadline
	// then we default to 'defaultRequestWaitLimit'
	// in any case, the wait limit for any request must not
	// exceed the hard limit of 1m
	//
	// request has deadline:
	//   wait-limit = min(remaining deadline / 4, 1m)
	// request has no deadline:
	//   wait-limit = min(defaultRequestWaitLimit, 1m)
	thisReqWaitLimit := defaultRequestWaitLimit
	if deadline, ok := ctx.Deadline(); ok {
		thisReqWaitLimit = deadline.Sub(reqArrivedAt) / 4
	}
	if thisReqWaitLimit > time.Minute {
		thisReqWaitLimit = time.Minute
	}

	return context.WithDeadline(ctx, reqArrivedAt.Add(thisReqWaitLimit))
}
