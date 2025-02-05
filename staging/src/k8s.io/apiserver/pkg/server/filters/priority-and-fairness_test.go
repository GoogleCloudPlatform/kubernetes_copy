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

package filters

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	flowcontrol "k8s.io/api/flowcontrol/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/flowcontrol/bootstrap"
	"k8s.io/apiserver/pkg/authentication/user"
	apifilters "k8s.io/apiserver/pkg/endpoints/filters"
	epmetrics "k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server/mux"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	fq "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing"
	fcmetrics "k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	fcrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	"github.com/google/go-cmp/cmp"
)

func TestMain(m *testing.M) {
	klog.InitFlags(nil)
	os.Exit(m.Run())
}

type mockDecision int

const (
	decisionNoQueuingExecute mockDecision = iota
	decisionQueuingExecute
	decisionCancelWait
	decisionReject
	decisionSkipFilter
)

var defaultRequestWorkEstimator = func(req *http.Request, fsName, plName string) fcrequest.WorkEstimate {
	return fcrequest.WorkEstimate{InitialSeats: 1}
}

type fakeApfFilter struct {
	mockDecision mockDecision
	postEnqueue  func()
	postDequeue  func()

	utilflowcontrol.WatchTracker
	utilflowcontrol.MaxSeatsTracker
}

func (t fakeApfFilter) Handle(ctx context.Context,
	requestDigest utilflowcontrol.RequestDigest,
	noteFn func(fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, flowDistinguisher string),
	workEstimator func() fcrequest.WorkEstimate,
	queueNoteFn fq.QueueNoteFn,
	execFn func(),
) {
	if t.mockDecision == decisionSkipFilter {
		panic("Handle should not be invoked")
	}
	noteFn(bootstrap.SuggestedFlowSchemaGlobalDefault, bootstrap.SuggestedPriorityLevelConfigurationGlobalDefault, requestDigest.User.GetName())
	switch t.mockDecision {
	case decisionNoQueuingExecute:
		execFn()
	case decisionQueuingExecute:
		queueNoteFn(true)
		t.postEnqueue()
		queueNoteFn(false)
		t.postDequeue()
		execFn()
	case decisionCancelWait:
		queueNoteFn(true)
		t.postEnqueue()
		queueNoteFn(false)
		t.postDequeue()
	case decisionReject:
		return
	}
}

func (t fakeApfFilter) Run(stopCh <-chan struct{}) error {
	return nil
}

func (t fakeApfFilter) Install(c *mux.PathRecorderMux) {
}

func newApfServerWithSingleRequest(t *testing.T, decision mockDecision) *httptest.Server {
	onExecuteFunc := func() {
		if decision == decisionCancelWait {
			t.Errorf("execute should not be invoked")
		}
		// atomicReadOnlyExecuting can be either 0 or 1 as we test one request at a time.
		if decision != decisionSkipFilter && atomicReadOnlyExecuting != 1 {
			t.Errorf("Wanted %d requests executing, got %d", 1, atomicReadOnlyExecuting)
		}
	}
	postExecuteFunc := func() {}
	// atomicReadOnlyWaiting can be either 0 or 1 as we test one request at a time.
	postEnqueueFunc := func() {
		if atomicReadOnlyWaiting != 1 {
			t.Errorf("Wanted %d requests in queue, got %d", 1, atomicReadOnlyWaiting)
		}
	}
	postDequeueFunc := func() {
		if atomicReadOnlyWaiting != 0 {
			t.Errorf("Wanted %d requests in queue, got %d", 0, atomicReadOnlyWaiting)
		}
	}
	return newApfServerWithHooks(t, decision, onExecuteFunc, postExecuteFunc, postEnqueueFunc, postDequeueFunc)
}

func newApfServerWithHooks(t *testing.T, decision mockDecision, onExecute, postExecute, postEnqueue, postDequeue func()) *httptest.Server {
	fakeFilter := fakeApfFilter{
		mockDecision:    decision,
		postEnqueue:     postEnqueue,
		postDequeue:     postDequeue,
		WatchTracker:    utilflowcontrol.NewWatchTracker(),
		MaxSeatsTracker: utilflowcontrol.NewMaxSeatsTracker(),
	}
	return newApfServerWithFilter(t, fakeFilter, time.Minute/4, onExecute, postExecute)
}

func newApfServerWithFilter(t *testing.T, flowControlFilter utilflowcontrol.Interface, defaultWaitLimit time.Duration, onExecute, postExecute func()) *httptest.Server {
	epmetrics.Register()
	fcmetrics.Register()
	apfServer := httptest.NewServer(newApfHandlerWithFilter(t, flowControlFilter, defaultWaitLimit, onExecute, postExecute))
	return apfServer
}

func newApfHandlerWithFilter(t *testing.T, flowControlFilter utilflowcontrol.Interface, defaultWaitLimit time.Duration, onExecute, postExecute func()) http.Handler {
	requestInfoFactory := &apirequest.RequestInfoFactory{APIPrefixes: sets.NewString("apis", "api"), GrouplessAPIPrefixes: sets.NewString("api")}
	longRunningRequestCheck := BasicLongRunningRequestCheck(sets.NewString("watch"), sets.NewString("proxy"))

	apfHandler := WithPriorityAndFairness(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onExecute()
	}), longRunningRequestCheck, flowControlFilter, defaultRequestWorkEstimator, defaultWaitLimit)

	handler := apifilters.WithRequestInfo(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(apirequest.WithUser(r.Context(), &user.DefaultInfo{
			Groups: []string{user.AllUnauthenticated},
		}))
		func() {
			// the APF handler completes its run, either normally or
			// with a panic, in either case, all APF book keeping must
			// be completed by now. Also, whether the request is
			// executed or rejected, we expect the counter to be zero.
			// TODO: all test(s) using this filter must run
			// serially to each other
			defer func() {
				if atomicReadOnlyExecuting != 0 {
					t.Errorf("Wanted %d requests executing, got %d", 0, atomicReadOnlyExecuting)
				}
			}()
			apfHandler.ServeHTTP(w, r)
			postExecute()
		}()
	}), requestInfoFactory)

	return handler
}

func TestApfSkipLongRunningRequest(t *testing.T) {
	server := newApfServerWithSingleRequest(t, decisionSkipFilter)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartPriorityAndFairnessWatermarkMaintenance(ctx.Done())

	// send a watch request to test skipping long running request
	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/foos/foo/proxy", server.URL), http.StatusOK); err != nil {
		// request should not be rejected
		t.Error(err)
	}
}

func TestApfRejectRequest(t *testing.T) {
	server := newApfServerWithSingleRequest(t, decisionReject)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartPriorityAndFairnessWatermarkMaintenance(ctx.Done())

	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default", server.URL), http.StatusTooManyRequests); err != nil {
		t.Error(err)
	}

	checkForExpectedMetrics(t, []string{
		"apiserver_request_terminations_total",
		"apiserver_request_total",
	})
}

func TestApfExemptRequest(t *testing.T) {
	server := newApfServerWithSingleRequest(t, decisionNoQueuingExecute)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartPriorityAndFairnessWatermarkMaintenance(ctx.Done())

	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default", server.URL), http.StatusOK); err != nil {
		t.Error(err)
	}

	checkForExpectedMetrics(t, []string{
		"apiserver_current_inflight_requests",
		"apiserver_flowcontrol_read_vs_write_current_requests",
	})
}

func TestApfExecuteRequest(t *testing.T) {
	server := newApfServerWithSingleRequest(t, decisionQueuingExecute)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartPriorityAndFairnessWatermarkMaintenance(ctx.Done())

	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default", server.URL), http.StatusOK); err != nil {
		t.Error(err)
	}

	checkForExpectedMetrics(t, []string{
		"apiserver_current_inflight_requests",
		"apiserver_current_inqueue_requests",
		"apiserver_flowcontrol_read_vs_write_current_requests",
	})
}

func TestApfExecuteMultipleRequests(t *testing.T) {
	concurrentRequests := 5
	preStartExecute, postStartExecute := &sync.WaitGroup{}, &sync.WaitGroup{}
	preEnqueue, postEnqueue := &sync.WaitGroup{}, &sync.WaitGroup{}
	preDequeue, postDequeue := &sync.WaitGroup{}, &sync.WaitGroup{}
	finishExecute := &sync.WaitGroup{}
	for _, wg := range []*sync.WaitGroup{preStartExecute, postStartExecute, preEnqueue, postEnqueue, preDequeue, postDequeue, finishExecute} {
		wg.Add(concurrentRequests)
	}

	onExecuteFunc := func() {
		preStartExecute.Done()
		preStartExecute.Wait()
		if int(atomicReadOnlyExecuting) != concurrentRequests {
			t.Errorf("Wanted %d requests executing, got %d", concurrentRequests, atomicReadOnlyExecuting)
		}
		postStartExecute.Done()
		postStartExecute.Wait()
	}

	postEnqueueFunc := func() {
		preEnqueue.Done()
		preEnqueue.Wait()
		if int(atomicReadOnlyWaiting) != concurrentRequests {
			t.Errorf("Wanted %d requests in queue, got %d", 1, atomicReadOnlyWaiting)

		}
		postEnqueue.Done()
		postEnqueue.Wait()
	}

	postDequeueFunc := func() {
		preDequeue.Done()
		preDequeue.Wait()
		if atomicReadOnlyWaiting != 0 {
			t.Errorf("Wanted %d requests in queue, got %d", 0, atomicReadOnlyWaiting)
		}
		postDequeue.Done()
		postDequeue.Wait()
	}

	postExecuteFunc := func() {
		finishExecute.Done()
		finishExecute.Wait()
	}

	server := newApfServerWithHooks(t, decisionQueuingExecute, onExecuteFunc, postExecuteFunc, postEnqueueFunc, postDequeueFunc)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartPriorityAndFairnessWatermarkMaintenance(ctx.Done())

	var wg sync.WaitGroup
	wg.Add(concurrentRequests)
	for i := 0; i < concurrentRequests; i++ {
		go func() {
			defer wg.Done()
			if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default", server.URL), http.StatusOK); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	checkForExpectedMetrics(t, []string{
		"apiserver_current_inflight_requests",
		"apiserver_current_inqueue_requests",
		"apiserver_flowcontrol_read_vs_write_current_requests",
	})
}

func TestApfCancelWaitRequest(t *testing.T) {
	server := newApfServerWithSingleRequest(t, decisionCancelWait)
	defer server.Close()

	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default", server.URL), http.StatusTooManyRequests); err != nil {
		t.Error(err)
	}

	checkForExpectedMetrics(t, []string{
		"apiserver_current_inflight_requests",
		"apiserver_request_terminations_total",
		"apiserver_request_total",
	})
}

type fakeWatchApfFilter struct {
	lock     sync.Mutex
	inflight int
	capacity int

	postExecutePanic bool
	preExecutePanic  bool

	utilflowcontrol.WatchTracker
	utilflowcontrol.MaxSeatsTracker
}

func newFakeWatchApfFilter(capacity int) *fakeWatchApfFilter {
	return &fakeWatchApfFilter{
		capacity:        capacity,
		WatchTracker:    utilflowcontrol.NewWatchTracker(),
		MaxSeatsTracker: utilflowcontrol.NewMaxSeatsTracker(),
	}
}

func (f *fakeWatchApfFilter) Handle(ctx context.Context,
	requestDigest utilflowcontrol.RequestDigest,
	noteFn func(fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, flowDistinguisher string),
	_ func() fcrequest.WorkEstimate,
	_ fq.QueueNoteFn,
	execFn func(),
) {
	noteFn(bootstrap.SuggestedFlowSchemaGlobalDefault, bootstrap.SuggestedPriorityLevelConfigurationGlobalDefault, requestDigest.User.GetName())
	canExecute := false
	func() {
		f.lock.Lock()
		defer f.lock.Unlock()
		if f.inflight < f.capacity {
			f.inflight++
			canExecute = true
		}
	}()
	if !canExecute {
		return
	}

	if f.preExecutePanic {
		panic("pre-exec-panic")
	}
	execFn()
	if f.postExecutePanic {
		panic("post-exec-panic")
	}

	f.lock.Lock()
	defer f.lock.Unlock()
	f.inflight--
}

func (f *fakeWatchApfFilter) Run(stopCh <-chan struct{}) error {
	return nil
}

func (t *fakeWatchApfFilter) Install(c *mux.PathRecorderMux) {
}

func (f *fakeWatchApfFilter) wait() error {
	return wait.Poll(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		f.lock.Lock()
		defer f.lock.Unlock()
		return f.inflight == 0, nil
	})
}

func TestApfExecuteWatchRequestsWithInitializationSignal(t *testing.T) {
	signalsLock := sync.Mutex{}
	signals := []utilflowcontrol.InitializationSignal{}
	sendSignals := func() {
		signalsLock.Lock()
		defer signalsLock.Unlock()
		for i := range signals {
			signals[i].Signal()
		}
		signals = signals[:0]
	}

	newInitializationSignal = func() utilflowcontrol.InitializationSignal {
		signalsLock.Lock()
		defer signalsLock.Unlock()
		signal := utilflowcontrol.NewInitializationSignal()
		signals = append(signals, signal)
		return signal
	}
	defer func() {
		newInitializationSignal = utilflowcontrol.NewInitializationSignal
	}()

	// We test if initialization after receiving initialization signal the
	// new requests will be allowed to run by:
	// - sending N requests that will occupy the whole capacity
	// - sending initialization signals for them
	// - ensuring that number of inflight requests will get to zero
	concurrentRequests := 5
	firstRunning := sync.WaitGroup{}
	firstRunning.Add(concurrentRequests)
	allRunning := sync.WaitGroup{}
	allRunning.Add(2 * concurrentRequests)

	fakeFilter := newFakeWatchApfFilter(concurrentRequests)

	onExecuteFunc := func() {
		firstRunning.Done()

		fakeFilter.wait()

		allRunning.Done()
		allRunning.Wait()
	}

	postExecuteFunc := func() {}

	server := newApfServerWithFilter(t, fakeFilter, time.Minute/4, onExecuteFunc, postExecuteFunc)
	defer server.Close()

	var wg sync.WaitGroup
	wg.Add(2 * concurrentRequests)
	for i := 0; i < concurrentRequests; i++ {
		go func() {
			defer wg.Done()
			if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default/pods?watch=true", server.URL), http.StatusOK); err != nil {
				t.Error(err)
			}
		}()
	}

	firstRunning.Wait()
	sendSignals()
	fakeFilter.wait()
	firstRunning.Add(concurrentRequests)

	for i := 0; i < concurrentRequests; i++ {
		go func() {
			defer wg.Done()
			if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default/pods?watch=true", server.URL), http.StatusOK); err != nil {
				t.Error(err)
			}
		}()
	}
	firstRunning.Wait()
	sendSignals()
	wg.Wait()
}

func TestApfRejectWatchRequestsWithInitializationSignal(t *testing.T) {
	fakeFilter := newFakeWatchApfFilter(0)

	onExecuteFunc := func() {
		t.Errorf("Request unexepectedly executing")
	}
	postExecuteFunc := func() {}

	server := newApfServerWithFilter(t, fakeFilter, time.Minute/4, onExecuteFunc, postExecuteFunc)
	defer server.Close()

	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default/pods?watch=true", server.URL), http.StatusTooManyRequests); err != nil {
		t.Error(err)
	}
}

func TestApfWatchPanic(t *testing.T) {
	epmetrics.Register()
	fcmetrics.Register()

	fakeFilter := newFakeWatchApfFilter(1)

	onExecuteFunc := func() {
		panic("test panic")
	}
	postExecuteFunc := func() {}

	apfHandler := newApfHandlerWithFilter(t, fakeFilter, time.Minute/4, onExecuteFunc, postExecuteFunc)
	handler := func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err == nil {
				t.Errorf("expected panic, got %v", err)
			}
		}()
		apfHandler.ServeHTTP(w, r)
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default/pods?watch=true", server.URL), http.StatusOK); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestApfWatchHandlePanic(t *testing.T) {
	epmetrics.Register()
	fcmetrics.Register()
	preExecutePanicingFilter := newFakeWatchApfFilter(1)
	preExecutePanicingFilter.preExecutePanic = true

	postExecutePanicingFilter := newFakeWatchApfFilter(1)
	postExecutePanicingFilter.postExecutePanic = true

	testCases := []struct {
		name   string
		filter *fakeWatchApfFilter
	}{
		{
			name:   "pre-execute panic",
			filter: preExecutePanicingFilter,
		},
		{
			name:   "post-execute panic",
			filter: postExecutePanicingFilter,
		},
	}

	onExecuteFunc := func() {
		time.Sleep(5 * time.Second)
	}
	postExecuteFunc := func() {}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			apfHandler := newApfHandlerWithFilter(t, test.filter, time.Minute/4, onExecuteFunc, postExecuteFunc)
			handler := func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					if err := recover(); err == nil {
						t.Errorf("expected panic, got %v", err)
					}
				}()
				apfHandler.ServeHTTP(w, r)
			}
			server := httptest.NewServer(http.HandlerFunc(handler))
			defer server.Close()

			if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default/pods?watch=true", server.URL), http.StatusOK); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestContextClosesOnRequestProcessed ensures that the request context is cancelled
// automatically even if the server doesn't cancel is explicitly.
// This is required to ensure we won't be leaking goroutines that wait for context
// cancelling (e.g. in queueset::StartRequest method).
// Even though in production we are not using httptest.Server, this logic is shared
// across these two.
func TestContextClosesOnRequestProcessed(t *testing.T) {
	epmetrics.Register()
	fcmetrics.Register()
	wg := sync.WaitGroup{}
	wg.Add(1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// asynchronously wait for context being closed
		go func() {
			<-ctx.Done()
			wg.Done()
		}()
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	if err := expectHTTPGet(fmt.Sprintf("%s/api/v1/namespaces/default/pods?watch=true", server.URL), http.StatusOK); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	wg.Wait()
}

type fakeFilterRequestDigest struct {
	*fakeApfFilter
	requestDigestGot *utilflowcontrol.RequestDigest
	workEstimateGot  fcrequest.WorkEstimate
}

func (f *fakeFilterRequestDigest) Handle(ctx context.Context,
	requestDigest utilflowcontrol.RequestDigest,
	noteFn func(fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, flowDistinguisher string),
	workEstimator func() fcrequest.WorkEstimate,
	_ fq.QueueNoteFn, _ func(),
) {
	f.requestDigestGot = &requestDigest
	noteFn(bootstrap.MandatoryFlowSchemaCatchAll, bootstrap.MandatoryPriorityLevelConfigurationCatchAll, "")
	f.workEstimateGot = workEstimator()
}

func TestApfWithRequestDigest(t *testing.T) {
	epmetrics.Register()
	fcmetrics.Register()
	longRunningFunc := func(_ *http.Request, _ *apirequest.RequestInfo) bool { return false }
	fakeFilter := &fakeFilterRequestDigest{}

	reqDigestExpected := &utilflowcontrol.RequestDigest{
		RequestInfo: &apirequest.RequestInfo{Verb: "get"},
		User:        &user.DefaultInfo{Name: "foo"},
	}
	workExpected := fcrequest.WorkEstimate{
		InitialSeats:      5,
		FinalSeats:        7,
		AdditionalLatency: 3 * time.Second,
	}

	handler := WithPriorityAndFairness(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {}),
		longRunningFunc,
		fakeFilter,
		func(_ *http.Request, _, _ string) fcrequest.WorkEstimate { return workExpected },
		time.Minute/4,
	)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "/bar", nil)
	if err != nil {
		t.Fatalf("Failed to create new http request - %v", err)
	}
	req = req.WithContext(apirequest.WithRequestInfo(req.Context(), reqDigestExpected.RequestInfo))
	req = req.WithContext(apirequest.WithUser(req.Context(), reqDigestExpected.User))

	handler.ServeHTTP(w, req)

	if !reflect.DeepEqual(reqDigestExpected, fakeFilter.requestDigestGot) {
		t.Errorf("Expected RequestDigest to match, diff: %s", cmp.Diff(reqDigestExpected, fakeFilter.requestDigestGot))
	}
	if !reflect.DeepEqual(workExpected, fakeFilter.workEstimateGot) {
		t.Errorf("Expected WorkEstimate to match, diff: %s", cmp.Diff(workExpected, fakeFilter.workEstimateGot))
	}
}

func TestPriorityAndFairnessWithPanicRecoveryAndTimeoutFilter(t *testing.T) {
	epmetrics.Register()
	fcmetrics.Register()
	timeFmt := "15:04:05.999"

	t.Run("priority level concurrency is set to 1, request handler panics, next request should not be rejected", func(t *testing.T) {
		const (
			userName                                              = "alice"
			fsName                                                = "test-fs"
			plName                                                = "test-pl"
			serverConcurrency, plConcurrencyShares, plConcurrency = 1, 1, 1
		)

		apfConfiguration := newConfiguration(fsName, plName, userName, plConcurrencyShares, 0)
		stopCh := make(chan struct{})
		controller, controllerCompletedCh := startAPFController(t, stopCh, apfConfiguration, serverConcurrency, plName, plConcurrency)

		headerMatcher := headerMatcher{}
		// we will raise a panic for the first request.
		firstRequestPathPanic, secondRequestPathShouldWork := "/request/panic-as-designed", "/request/should-succeed-as-expected"
		firstHandlerDoneCh, secondHandlerDoneCh := make(chan struct{}), make(chan struct{})
		requestHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerMatcher.inspect(t, w, fsName, plName)
			switch {
			case r.URL.Path == firstRequestPathPanic:
				close(firstHandlerDoneCh)
				panic(fmt.Errorf("request handler panic'd as designed - %#v", r.RequestURI))
			case r.URL.Path == secondRequestPathShouldWork:
				close(secondHandlerDoneCh)

			}
		})

		// NOTE: the server will enforce a 1m timeout on every incoming
		//  request, and the client enforces a timeout of 2m.
		handler := newHandlerChain(t, requestHandler, controller, userName, time.Minute)
		server, requestGetter := newHTTP2ServerWithClient(handler, 2*time.Minute)
		defer server.Close()

		// we send two requests synchronously, one at a time
		//  - first request is expected to panic as designed
		//  - second request is expected to succeed
		_, err := requestGetter(firstRequestPathPanic)

		// did the server handler panic, as expected?
		select {
		case <-firstHandlerDoneCh:
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected the server handler to panic for request: %q", firstRequestPathPanic)
		}
		if isClientTimeout(err) {
			t.Fatalf("the client has unexpectedly timed out - request: %q error: %s", firstRequestPathPanic, err.Error())
		}
		expectResetStreamError(t, err)

		// the second request should be served successfully.
		response, err := requestGetter(secondRequestPathShouldWork)
		if err != nil {
			t.Fatalf("Expected request: %q to get a response, but got error: %#v", secondRequestPathShouldWork, err)
		}
		if response.StatusCode != http.StatusOK {
			t.Errorf("Expected HTTP status code: %d for request: %q, but got: %#v", http.StatusOK, secondRequestPathShouldWork, response)
		}
		select {
		case <-secondHandlerDoneCh:
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected the server handler to have completed: %q", secondRequestPathShouldWork)
		}

		close(stopCh)
		t.Log("Waiting for the controller to shutdown")

		controllerErr := <-controllerCompletedCh
		if controllerErr != nil {
			t.Errorf("Expected no error from the controller, but got: %#v", controllerErr)
		}
	})

	t.Run("priority level concurrency is set to 1, request times out and inner handler hasn't written to the response yet", func(t *testing.T) {
		t.Parallel()
		const (
			userName                                              = "alice"
			fsName                                                = "test-fs"
			plName                                                = "test-pl"
			serverConcurrency, plConcurrencyShares, plConcurrency = 1, 1, 1
		)

		apfConfiguration := newConfiguration(fsName, plName, userName, plConcurrencyShares, 0)
		stopCh := make(chan struct{})
		controller, controllerCompletedCh := startAPFController(t, stopCh, apfConfiguration, serverConcurrency, plName, plConcurrency)

		headerMatcher := headerMatcher{}
		rquestTimesOutPath := "/request/time-out-as-designed"
		reqHandlerCompletedCh, callerRoundTripDoneCh := make(chan struct{}), make(chan struct{})
		requestHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerMatcher.inspect(t, w, fsName, plName)

			if r.URL.Path == rquestTimesOutPath {
				defer close(reqHandlerCompletedCh)

				// this will force the request to time out.
				<-callerRoundTripDoneCh
			}
		})

		// NOTE: the server will enforce a 5s timeout on every
		//  incoming request, and the client enforces a timeout of 1m.
		handler := newHandlerChain(t, requestHandler, controller, userName, 5*time.Second)
		server, requestGetter := newHTTP2ServerWithClient(handler, time.Minute)
		defer server.Close()

		// send a request synchronously with a client timeout of 1m,  this minimizes the
		// chance of a flake in ci, the client waits long enough for the server to send a
		// timeout response to the client.
		var (
			response *http.Response
			err      error
		)
		func() {
			defer close(callerRoundTripDoneCh)

			t.Logf("Waiting for the request: %q to time out", rquestTimesOutPath)
			response, err = requestGetter(rquestTimesOutPath)
			if isClientTimeout(err) {
				t.Fatalf("the client has unexpectedly timed out - request: %q error: %s", rquestTimesOutPath, err.Error())
			}
		}()

		t.Logf("Waiting for the inner handler of the request: %q to complete", rquestTimesOutPath)
		select {
		case <-reqHandlerCompletedCh:
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected the server handler to have completed: %q", rquestTimesOutPath)
		}

		if err != nil {
			t.Fatalf("Expected request: %q to get a response, but got error: %#v", rquestTimesOutPath, err)
		}
		if response.StatusCode != http.StatusGatewayTimeout {
			t.Errorf("Expected HTTP status code: %d for request: %q, but got: %#v", http.StatusGatewayTimeout, rquestTimesOutPath, response)
		}

		close(stopCh)
		t.Log("Waiting for the controller to shutdown")

		controllerErr := <-controllerCompletedCh
		if controllerErr != nil {
			t.Errorf("Expected no error from the controller, but got: %#v", controllerErr)
		}
	})

	t.Run("priority level concurrency is set to 1, inner handler panics after the request times out", func(t *testing.T) {
		t.Parallel()
		const (
			userName                                              = "alice"
			fsName                                                = "test-fs"
			plName                                                = "test-pl"
			serverConcurrency, plConcurrencyShares, plConcurrency = 1, 1, 1
		)

		apfConfiguration := newConfiguration(fsName, plName, userName, plConcurrencyShares, 0)
		stopCh := make(chan struct{})
		controller, controllerCompletedCh := startAPFController(t, stopCh, apfConfiguration, serverConcurrency, plName, plConcurrency)

		headerMatcher := headerMatcher{}
		reqHandlerErrCh, callerRoundTripDoneCh := make(chan error, 1), make(chan struct{})
		rquestTimesOutPath := "/request/time-out-as-designed"
		requestHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerMatcher.inspect(t, w, fsName, plName)

			if r.URL.Path == rquestTimesOutPath {
				<-callerRoundTripDoneCh

				// we expect the timeout handler to have timed out this request by now and any attempt
				// to write to the response should return a http.ErrHandlerTimeout error.
				_, innerHandlerWriteErr := w.Write([]byte("foo"))
				reqHandlerErrCh <- innerHandlerWriteErr

				panic(http.ErrAbortHandler)
			}
		})

		// NOTE: the server will enforce a 5s timeout on every
		//  incoming request, and the client enforces a timeout of 1m.
		handler := newHandlerChain(t, requestHandler, controller, userName, 5*time.Second)
		server, requestGetter := newHTTP2ServerWithClient(handler, time.Minute)
		defer server.Close()

		// send a request synchronously with a client timeout of 1m, this minimizes the
		// chance of a flake in ci, the client waits long enough for the server to send a
		// timeout response to the client.
		var (
			response *http.Response
			err      error
		)
		func() {
			defer close(callerRoundTripDoneCh)
			t.Logf("Waiting for the request: %q to time out", rquestTimesOutPath)
			response, err = requestGetter(rquestTimesOutPath)
			if isClientTimeout(err) {
				t.Fatalf("the client has unexpectedly timed out - request: %q error: %s", rquestTimesOutPath, err.Error())
			}
		}()

		t.Logf("Waiting for the inner handler of the request: %q to complete", rquestTimesOutPath)
		select {
		case innerHandlerWriteErr := <-reqHandlerErrCh:
			if innerHandlerWriteErr != http.ErrHandlerTimeout {
				t.Fatalf("Expected error: %#v, but got: %#v", http.ErrHandlerTimeout, err)
			}
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected the server handler to have completed: %q", rquestTimesOutPath)
		}

		if err != nil {
			t.Fatalf("Expected request: %q to get a response, but got error: %#v", rquestTimesOutPath, err)
		}
		if response.StatusCode != http.StatusGatewayTimeout {
			t.Errorf("Expected HTTP status code: %d for request: %q, but got: %#v", http.StatusGatewayTimeout, rquestTimesOutPath, response)
		}

		close(stopCh)
		t.Log("Waiting for the controller to shutdown")

		controllerErr := <-controllerCompletedCh
		if controllerErr != nil {
			t.Errorf("Expected no error from the controller, but got: %#v", controllerErr)
		}
	})

	t.Run("priority level concurrency is set to 1, inner handler writes to the response before request times out", func(t *testing.T) {
		t.Parallel()
		const (
			userName                                              = "alice"
			fsName                                                = "test-fs"
			plName                                                = "test-pl"
			serverConcurrency, plConcurrencyShares, plConcurrency = 1, 1, 1
		)

		apfConfiguration := newConfiguration(fsName, plName, userName, plConcurrencyShares, 0)
		stopCh := make(chan struct{})
		controller, controllerCompletedCh := startAPFController(t, stopCh, apfConfiguration, serverConcurrency, plName, plConcurrency)

		headerMatcher := headerMatcher{}
		rquestTimesOutPath := "/request/time-out-as-designed"
		reqHandlerErrCh, callerRoundTripDoneCh := make(chan error, 1), make(chan struct{})
		requestHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerMatcher.inspect(t, w, fsName, plName)

			if r.URL.Path == rquestTimesOutPath {

				// inner handler writes header and then let the request time out.
				w.WriteHeader(http.StatusBadRequest)
				<-callerRoundTripDoneCh

				// we expect the timeout handler to have timed out this request by now and any attempt
				// to write to the response should return a http.ErrHandlerTimeout error.
				_, innerHandlerWriteErr := w.Write([]byte("foo"))
				reqHandlerErrCh <- innerHandlerWriteErr
			}
		})

		// NOTE: the server will enforce a 5s timeout on every
		//  incoming request, and the client enforces a timeout of 1m.
		handler := newHandlerChain(t, requestHandler, controller, userName, 5*time.Second)
		server, requestGetter := newHTTP2ServerWithClient(handler, time.Minute)
		defer server.Close()

		// send a request synchronously with a client timeout of 1m, this minimizes the
		// chance of a flake in ci, the client waits long enough for the server to send a
		// timeout response to the client.
		var err error
		func() {
			defer close(callerRoundTripDoneCh)
			t.Logf("Waiting for the request: %q to time out", rquestTimesOutPath)
			_, err = requestGetter(rquestTimesOutPath)
			if isClientTimeout(err) {
				t.Fatalf("the client has unexpectedly timed out - request: %q error: %s", rquestTimesOutPath, err.Error())
			}
		}()

		t.Logf("Waiting for the inner handler of the request: %q to complete", rquestTimesOutPath)
		select {
		case innerHandlerWriteErr := <-reqHandlerErrCh:
			if innerHandlerWriteErr != http.ErrHandlerTimeout {
				t.Fatalf("Expected error: %#v, but got: %#v", http.ErrHandlerTimeout, err)
			}
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected the server handler to have completed: %q", rquestTimesOutPath)
		}

		expectResetStreamError(t, err)

		close(stopCh)
		t.Log("Waiting for the controller to shutdown")

		controllerErr := <-controllerCompletedCh
		if controllerErr != nil {
			t.Errorf("Expected no error from the controller, but got: %#v", controllerErr)
		}
	})

	t.Run("priority level concurrency is set to 1, queue length is 1, first request should time out and second (enqueued) request should time out as well", func(t *testing.T) {
		t.Parallel()

		type result struct {
			err      error
			response *http.Response
		}

		const (
			userName                                                           = "alice"
			fsName                                                             = "test-fs"
			plName                                                             = "test-pl"
			serverConcurrency, plConcurrencyShares, plConcurrency, queueLength = 1, 1, 1, 1
		)

		apfConfiguration := newConfiguration(fsName, plName, userName, plConcurrencyShares, queueLength)
		stopCh := make(chan struct{})
		controller, controllerCompletedCh := startAPFController(t, stopCh, apfConfiguration, serverConcurrency, plName, plConcurrency)

		headerMatcher := headerMatcher{}
		firstRequestTimesOutPath, secondRequestEnqueuedPath := "/request/first/time-out-as-designed", "/request/second/enqueued-as-designed"
		firstReqHandlerErrCh, firstReqInProgressCh := make(chan error, 1), make(chan struct{})
		firstReqRoundTripDoneCh, secondReqRoundTripDoneCh := make(chan struct{}), make(chan struct{})
		requestHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerMatcher.inspect(t, w, fsName, plName)
			switch {
			case r.URL.Path == firstRequestTimesOutPath:
				close(firstReqInProgressCh)
				<-firstReqRoundTripDoneCh

				// make sure we wait until the caller of the second request returns, this is to
				// ensure that second request never has a chance to be executed (to avoid flakes)
				<-secondReqRoundTripDoneCh

				// we expect the timeout handler to have timed out this request by now and any attempt
				// to write to the response should return a http.ErrHandlerTimeout error.
				_, firstRequestInnerHandlerWriteErr := w.Write([]byte("foo"))
				firstReqHandlerErrCh <- firstRequestInnerHandlerWriteErr

			case r.URL.Path == secondRequestEnqueuedPath:
				// we expect the concurrency to be set to 1 and so this request should never be executed.
				t.Errorf("Expected second request to be enqueued: %q", secondRequestEnqueuedPath)
			}
		})

		// NOTE: the server will enforce a 5s timeout on every
		//  incoming request, and the client enforces a timeout of 1m.
		handler := newHandlerChain(t, requestHandler, controller, userName, 5*time.Second)
		server, requestGetter := newHTTP2ServerWithClient(handler, time.Minute)
		defer server.Close()

		// This test involves two requests sent to the same priority level, which has 1 queue and
		// a concurrency limit of 1.  The handler chain include the timeout filter.
		// Each request is sent from a separate goroutine, with a client-side timeout of 1m, on
		// the other hand, the server enforces a timeout of 5s (via the timeout filter).
		// The first request should get dispatched immediately; execution (a) starts with closing
		// the channel that triggers the second client goroutine to send its request and then (b)
		// waits for both client goroutines to have gotten a response (expected to be timeouts).
		// The second request sits in the queue until the timeout filter does its thing, which
		// it does concurrently to both requests.  For the first request this should make the client
		// get a timeout response without directly affecting execution.  For the second request, the
		// fact that the timeout filter closes the request's Context.Done() causes the request to be
		// promptly ejected from its queue.  The goroutine doing the APF handling writes an HTTP
		// response message with status 429.
		// The timeout handler invokes its inner handler in one goroutine while reacting to the
		// passage of time in its original goroutine.  That reaction to a time out consists of either
		// (a) writing an HTTP response message with status 504 to indicate the timeout or (b) doing an
		// HTTP/2 stream close; the latter is done if either the connection has been "hijacked" or some
		// other goroutine (e.g., the one running the inner handler) has started to write a response.
		// In the scenario tested here, there is thus a race between two goroutines to respond to
		// the second request and any of their responses is allowed by the test.
		firstReqResultCh, secondReqResultCh := make(chan result, 1), make(chan result, 1)
		go func() {
			defer close(firstReqRoundTripDoneCh)
			t.Logf("At %s, Sending request: %q", time.Now().Format(timeFmt), firstRequestTimesOutPath)
			resp, err := requestGetter(firstRequestTimesOutPath)
			t.Logf("At %s, RoundTrip of request: %q has completed", time.Now().Format(timeFmt), firstRequestTimesOutPath)
			firstReqResultCh <- result{err: err, response: resp}
		}()
		go func() {
			// we must wait for the "first" request to start executing before
			// we can initiate the "second".
			defer close(secondReqRoundTripDoneCh)

			<-firstReqInProgressCh
			t.Logf("At %s, Sending request: %q", time.Now().Format(timeFmt), secondRequestEnqueuedPath)
			resp, err := requestGetter(secondRequestEnqueuedPath)
			t.Logf("At %s, RoundTrip of request: %q has completed", time.Now().Format(timeFmt), secondRequestEnqueuedPath)
			secondReqResultCh <- result{err: err, response: resp}
		}()

		firstReqResult := <-firstReqResultCh
		if isClientTimeout(firstReqResult.err) {
			t.Fatalf("the client has unexpectedly timed out - request: %q error: %s", firstRequestTimesOutPath, fmtError(firstReqResult.err))
		}
		t.Logf("Waiting for the inner handler of the request: %q to complete", firstRequestTimesOutPath)
		select {
		case firstRequestInnerHandlerWriteErr := <-firstReqHandlerErrCh:
			if firstRequestInnerHandlerWriteErr != http.ErrHandlerTimeout {
				t.Fatalf("Expected error: %#v, but got: %s", http.ErrHandlerTimeout, fmtError(firstRequestInnerHandlerWriteErr))
			}
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected the server handler to have completed: %q", firstRequestTimesOutPath)
		}

		// first request is expected to time out.
		if isStreamReset(firstReqResult.err) || firstReqResult.response.StatusCode != http.StatusGatewayTimeout {
			// got what was expected
		} else if firstReqResult.err != nil {
			t.Fatalf("Expected request: %q to get a response or stream reset, but got error: %s", firstRequestTimesOutPath, fmtError(firstReqResult.err))
		} else if firstReqResult.response.StatusCode != http.StatusGatewayTimeout {
			t.Errorf("Expected HTTP status code: %d for request: %q, but got: %#+v", http.StatusGatewayTimeout, firstRequestTimesOutPath, firstReqResult.response)
		}

		// second request is expected to either be rejected (ideal behavior) or time out (current approximation of the ideal behavior)
		secondReqResult := <-secondReqResultCh
		if isClientTimeout(secondReqResult.err) {
			t.Fatalf("the client has unexpectedly timed out - request: %q error: %s", secondRequestEnqueuedPath, fmtError(secondReqResult.err))
		}
		if isStreamReset(secondReqResult.err) || secondReqResult.response.StatusCode == http.StatusTooManyRequests || secondReqResult.response.StatusCode == http.StatusGatewayTimeout {
			// got what was expected
		} else if secondReqResult.err != nil {
			t.Fatalf("Expected request: %q to get a response or stream reset, but got error: %s", secondRequestEnqueuedPath, fmtError(secondReqResult.err))
		} else if !(secondReqResult.response.StatusCode == http.StatusTooManyRequests || secondReqResult.response.StatusCode == http.StatusGatewayTimeout) {
			t.Errorf("Expected HTTP status code: %d or %d for request: %q, but got: %#+v", http.StatusTooManyRequests, http.StatusGatewayTimeout, secondRequestEnqueuedPath, secondReqResult.response)
		}

		close(stopCh)
		t.Log("Waiting for the controller to shutdown")

		controllerErr := <-controllerCompletedCh
		if controllerErr != nil {
			t.Errorf("Expected no error from the controller, but got: %s", fmtError(controllerErr))
		}
	})
}

func fmtError(err error) string {
	return fmt.Sprintf("%#+v=%q", err, err.Error())
}

func startAPFController(t *testing.T, stopCh <-chan struct{}, apfConfiguration []runtime.Object, serverConcurrency int,
	plName string, plConcurrency int) (utilflowcontrol.Interface, <-chan error) {
	clientset := newClientset(t, apfConfiguration...)
	// this test does not rely on resync, so resync period is set to zero
	factory := informers.NewSharedInformerFactory(clientset, 0)
	controller := utilflowcontrol.New(factory, clientset.FlowcontrolV1(), serverConcurrency)

	factory.Start(stopCh)

	// wait for the informer cache to sync.
	timeout, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()
	cacheSyncDone := factory.WaitForCacheSync(timeout.Done())
	if names := unsyncedInformers(cacheSyncDone); len(names) > 0 {
		t.Fatalf("WaitForCacheSync did not successfully complete, resources=%#v", names)
	}

	controllerCompletedCh := make(chan error)
	var controllerErr error
	go func() {
		controllerErr = controller.Run(stopCh)
		controllerCompletedCh <- controllerErr
	}()

	// make sure that apf controller syncs the priority level configuration object we are using in this test.
	// read the metrics and ensure the concurrency limit for our priority level is set to the expected value.
	pollErr := wait.PollImmediate(100*time.Millisecond, 5*time.Second, func() (done bool, err error) {
		if err := gaugeValueMatch("apiserver_flowcontrol_nominal_limit_seats", map[string]string{"priority_level": plName}, plConcurrency); err != nil {
			t.Logf("polling retry - error: %s", err)
			return false, nil
		}
		return true, nil
	})
	if pollErr != nil {
		t.Fatalf("expected the apf controller to sync the priotity level configuration object: %s", plName)
	}

	return controller, controllerCompletedCh
}

// returns a started http2 server, with a client function to send request to the server.
func newHTTP2ServerWithClient(handler http.Handler, clientTimeout time.Duration) (*httptest.Server, func(path string) (*http.Response, error)) {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	cli := server.Client()
	cli.Timeout = clientTimeout
	return server, func(path string) (*http.Response, error) {
		return cli.Get(server.URL + path)
	}
}

type headerMatcher struct{}

// verifies that the expected flow schema and priority level UIDs are attached to the header.
func (m *headerMatcher) inspect(t *testing.T, w http.ResponseWriter, expectedFS, expectedPL string) {
	t.Helper()
	err := func() error {
		if w == nil {
			return fmt.Errorf("expected a non nil HTTP response")
		}

		key := flowcontrol.ResponseHeaderMatchedFlowSchemaUID
		if value := w.Header().Get(key); expectedFS != value {
			return fmt.Errorf("expected HTTP header %s to have value %q, but got: %q", key, expectedFS, value)
		}

		key = flowcontrol.ResponseHeaderMatchedPriorityLevelConfigurationUID
		if value := w.Header().Get(key); expectedPL != value {
			return fmt.Errorf("expected HTTP header %s to have value %q, but got %q", key, expectedPL, value)
		}
		return nil
	}()
	if err == nil {
		return
	}
	t.Errorf("Expected APF headers to match, but got: %v", err)
}

// when a request panics, http2 resets the stream with an INTERNAL_ERROR message
func expectResetStreamError(t *testing.T, err error) {
	if err == nil {
		t.Fatalf("expected the server to send an error, but got nil")
	}
	if isStreamReset(err) {
		return
	}
	t.Fatalf("expected a stream reset error, but got %#+v=%s", err, err.Error())
}

func newClientset(t *testing.T, objects ...runtime.Object) clientset.Interface {
	clientset := fake.NewSimpleClientset(objects...)
	if clientset == nil {
		t.Fatal("unable to create fake client set")
	}
	return clientset
}

// builds a chain of handlers that include the panic recovery and timeout filter, so we can simulate the behavior of
// a real apiserver.
// the specified user is added as the authenticated user to the request context.
func newHandlerChain(t *testing.T, handler http.Handler, filter utilflowcontrol.Interface, userName string, requestTimeout time.Duration) http.Handler {
	requestInfoFactory := &apirequest.RequestInfoFactory{APIPrefixes: sets.NewString("apis", "api"), GrouplessAPIPrefixes: sets.NewString("api")}
	longRunningRequestCheck := BasicLongRunningRequestCheck(sets.NewString("watch"), sets.NewString("proxy"))

	apfHandler := WithPriorityAndFairness(handler, longRunningRequestCheck, filter, defaultRequestWorkEstimator, time.Minute/4)

	// add the handler in the chain that adds the specified user to the request context
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(apirequest.WithUser(r.Context(), &user.DefaultInfo{
			Name:   userName,
			Groups: []string{user.AllAuthenticated},
		}))

		apfHandler.ServeHTTP(w, r)
	})

	handler = WithTimeoutForNonLongRunningRequests(handler, longRunningRequestCheck)
	// we don't have any request with invalid timeout, so leaving audit policy and sink nil.
	handler = apifilters.WithRequestDeadline(handler, nil, nil, longRunningRequestCheck, nil, requestTimeout)
	handler = apifilters.WithRequestInfo(handler, requestInfoFactory)
	handler = WithPanicRecovery(handler, requestInfoFactory)
	handler = apifilters.WithAuditInit(handler)
	return handler
}

func unsyncedInformers(status map[reflect.Type]bool) []string {
	names := make([]string, 0)

	for objType, synced := range status {
		if !synced {
			names = append(names, objType.Name())
		}
	}

	return names
}

func newConfiguration(fsName, plName, user string, concurrency int32, queueLength int32) []runtime.Object {
	fs := &flowcontrol.FlowSchema{
		ObjectMeta: metav1.ObjectMeta{
			Name: fsName,
			UID:  types.UID(fsName),
		},
		Spec: flowcontrol.FlowSchemaSpec{
			MatchingPrecedence: 1,
			PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
				Name: plName,
			},
			DistinguisherMethod: &flowcontrol.FlowDistinguisherMethod{
				Type: flowcontrol.FlowDistinguisherMethodByUserType,
			},
			Rules: []flowcontrol.PolicyRulesWithSubjects{
				{
					Subjects: []flowcontrol.Subject{
						{
							Kind: flowcontrol.SubjectKindUser,
							User: &flowcontrol.UserSubject{
								Name: user,
							},
						},
					},
					NonResourceRules: []flowcontrol.NonResourcePolicyRule{
						{
							Verbs:           []string{flowcontrol.VerbAll},
							NonResourceURLs: []string{flowcontrol.NonResourceAll},
						},
					},
				},
			},
		},
	}

	var (
		responseType flowcontrol.LimitResponseType = flowcontrol.LimitResponseTypeReject
		qcfg         *flowcontrol.QueuingConfiguration
	)
	if queueLength > 0 {
		responseType = flowcontrol.LimitResponseTypeQueue
		qcfg = &flowcontrol.QueuingConfiguration{
			Queues:           1,
			QueueLengthLimit: queueLength,
			HandSize:         1,
		}
	}
	pl := &flowcontrol.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: plName,
			UID:  types.UID(plName),
		},
		Spec: flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(concurrency),
				LimitResponse: flowcontrol.LimitResponse{
					Type:    responseType,
					Queuing: qcfg,
				},
			},
		},
	}

	return []runtime.Object{fs, pl}
}

// gathers and checks the metrics.
func checkForExpectedMetrics(t *testing.T, expectedMetrics []string) {
	metricsFamily, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics %v", err)
	}

	metrics := map[string]interface{}{}
	for _, mf := range metricsFamily {
		metrics[*mf.Name] = mf
	}

	for _, metricName := range expectedMetrics {
		if _, ok := metrics[metricName]; !ok {
			if !ok {
				t.Errorf("Scraped metrics did not include expected metric %s", metricName)
			}
		}
	}
}

// gaugeValueMatch ensures that the value of gauge metrics matching the labelFilter is as expected.
func gaugeValueMatch(name string, labelFilter map[string]string, wantValue int) error {
	metrics, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		return fmt.Errorf("failed to gather metrics: %s", err)
	}

	sum := 0
	familyMatch, labelMatch := false, false
	for _, mf := range metrics {
		if mf.GetName() != name {
			continue
		}

		familyMatch = true
		for _, metric := range mf.GetMetric() {
			if !testutil.LabelsMatch(metric, labelFilter) {
				continue
			}

			labelMatch = true
			sum += int(metric.GetGauge().GetValue())
		}
	}
	if !familyMatch {
		return fmt.Errorf("expected to find the metric family: %s in the gathered result", name)
	}
	if !labelMatch {
		return fmt.Errorf("expected to find metrics with matching labels: %#+v", labelFilter)
	}
	if wantValue != sum {
		return fmt.Errorf("expected the sum to be: %d, but got: %d for gauge metric: %s with labels %#+v", wantValue, sum, name, labelFilter)
	}

	return nil
}

func isClientTimeout(err error) bool {
	if urlErr, ok := err.(*url.Error); ok {
		return urlErr.Timeout()
	}
	return false
}

func isStreamReset(err error) bool {
	if err == nil {
		return false
	}
	if urlErr, ok := err.(*url.Error); ok {
		// Sadly, the client does not receive a more specific indication
		// of stream reset.
		return strings.Contains(urlErr.Err.Error(), "INTERNAL_ERROR")
	}
	return false
}

func TestGetRequestWaitContext(t *testing.T) {
	tests := []struct {
		name                    string
		defaultRequestWaitLimit time.Duration
		parent                  func(t time.Time) (context.Context, context.CancelFunc)
		newReqWaitCtxExpected   bool
		reqWaitLimitExpected    time.Duration
	}{
		{
			name: "context deadline has exceeded",
			parent: func(time.Time) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
		},
		{
			name: "context has a deadline, 'received at' is not set, wait limit should be one fourth of the remaining deadline from now",
			parent: func(now time.Time) (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), now.Add(60*time.Second))
			},
			newReqWaitCtxExpected: true,
			reqWaitLimitExpected:  15 * time.Second,
		},
		{
			name: "context has a deadline, 'received at' is set, wait limit should be one fourth of the deadline starting from the 'received at' time",
			parent: func(now time.Time) (context.Context, context.CancelFunc) {
				ctx := apirequest.WithReceivedTimestamp(context.Background(), now.Add(-10*time.Second))
				return context.WithDeadline(ctx, now.Add(50*time.Second))
			},
			newReqWaitCtxExpected: true,
			reqWaitLimitExpected:  5 * time.Second, // from now
		},
		{
			name:                    "context does not have any deadline, 'received at' is not set, default wait limit should be in effect from now",
			defaultRequestWaitLimit: 15 * time.Second,
			parent: func(time.Time) (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			newReqWaitCtxExpected: true,
			reqWaitLimitExpected:  15 * time.Second,
		},
		{
			name:                    "context does not have any deadline, 'received at' is set, default wait limit should be in effect starting from the 'received at' time",
			defaultRequestWaitLimit: 15 * time.Second,
			parent: func(now time.Time) (context.Context, context.CancelFunc) {
				ctx := apirequest.WithReceivedTimestamp(context.Background(), now.Add(-10*time.Second))
				return context.WithCancel(ctx)
			},
			newReqWaitCtxExpected: true,
			reqWaitLimitExpected:  5 * time.Second, // from now
		},
		{
			name: "context has a deadline, wait limit should not exceed the hard limit of 1m",
			parent: func(now time.Time) (context.Context, context.CancelFunc) {
				// let 1/4th of the remaining deadline exceed the hard limit
				return context.WithDeadline(context.Background(), now.Add(8*time.Minute))
			},
			newReqWaitCtxExpected: true,
			reqWaitLimitExpected:  time.Minute,
		},
		{
			name:                    "context has no deadline, wait limit should not exceed the hard limit of 1m",
			defaultRequestWaitLimit: 2 * time.Minute, // it exceeds the hard limit
			parent: func(now time.Time) (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			newReqWaitCtxExpected: true,
			reqWaitLimitExpected:  time.Minute,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			parent, cancel := test.parent(now)
			defer cancel()

			clock := clocktesting.NewFakePassiveClock(now)
			newReqWaitCtxGot, cancelGot := getRequestWaitContext(parent, test.defaultRequestWaitLimit, clock)
			if cancelGot == nil {
				t.Errorf("Expected a non nil context.CancelFunc")
				return
			}
			defer cancelGot()

			switch {
			case test.newReqWaitCtxExpected:
				deadlineGot, ok := newReqWaitCtxGot.Deadline()
				if !ok {
					t.Errorf("Expected the new wait limit context to have a deadline")
				}
				if waitLimitGot := deadlineGot.Sub(now); test.reqWaitLimitExpected != waitLimitGot {
					t.Errorf("Expected request wait limit %s, but got: %s", test.reqWaitLimitExpected, waitLimitGot)
				}
			default:
				if parent != newReqWaitCtxGot {
					t.Errorf("Expected the parent context to be returned: want: %#v, got %#v", parent, newReqWaitCtxGot)
				}
			}
		})
	}
}
