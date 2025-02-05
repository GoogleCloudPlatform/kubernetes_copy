/*
Copyright 2021 The Kubernetes Authors.

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

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/net/http2"
)

func TestMain(m *testing.M) {
	klog.InitFlags(nil)
	os.Exit(m.Run())
}

// doer sends a request to the server
type doer func(client *http.Client, gci func(httptrace.GotConnInfo), path string, timeout time.Duration) result

func (d doer) Do(client *http.Client, gci func(httptrace.GotConnInfo), path string, timeout time.Duration) result {
	return d(client, gci, path, timeout)
}

type result struct {
	err      error
	response *http.Response
}

// wrap a lifecycleSignal so the test can inject its own callback
type wrappedLifecycleSignal struct {
	lifecycleSignal
	before func(lifecycleSignal)
	after  func(lifecycleSignal)
}

func (w *wrappedLifecycleSignal) Signal() {
	if w.before != nil {
		w.before(w.lifecycleSignal)
	}
	w.lifecycleSignal.Signal()
	if w.after != nil {
		w.after(w.lifecycleSignal)
	}
}

func wrapLifecycleSignalsWithRecorder(t *testing.T, signals *lifecycleSignals, before func(lifecycleSignal)) {
	// it's important to record the signal being fired on a 'before' callback
	// to avoid flakes, since on the server the signaling of events are
	// an asynchronous process.
	signals.AfterShutdownDelayDuration = wrapLifecycleSignal(t, signals.AfterShutdownDelayDuration, before, nil)
	signals.PreShutdownHooksStopped = wrapLifecycleSignal(t, signals.PreShutdownHooksStopped, before, nil)
	signals.NotAcceptingNewRequest = wrapLifecycleSignal(t, signals.NotAcceptingNewRequest, before, nil)
	signals.HTTPServerStoppedListening = wrapLifecycleSignal(t, signals.HTTPServerStoppedListening, before, nil)
	signals.InFlightRequestsDrained = wrapLifecycleSignal(t, signals.InFlightRequestsDrained, before, nil)
	signals.ShutdownInitiated = wrapLifecycleSignal(t, signals.ShutdownInitiated, before, nil)
}

func wrapLifecycleSignal(t *testing.T, delegated lifecycleSignal, before, after func(_ lifecycleSignal)) lifecycleSignal {
	return &wrappedLifecycleSignal{
		lifecycleSignal: delegated,
		before:          before,
		after:           after,
	}
}

// the server may not wait enough time between firing two events for
// the test to execute its steps, this allows us to intercept the
// signal and execute verification steps inside the goroutine that
// is executing the test.
type signalInterceptingTestStep struct {
	doneCh chan struct{}
}

func (ts signalInterceptingTestStep) done() <-chan struct{} {
	return ts.doneCh
}
func (ts signalInterceptingTestStep) execute(fn func()) {
	defer close(ts.doneCh)
	fn()
}
func newSignalInterceptingTestStep() *signalInterceptingTestStep {
	return &signalInterceptingTestStep{
		doneCh: make(chan struct{}),
	}
}

//	 This test exercises the graceful termination scenario
//	 described in the following diagram
//	   - every vertical line is an independent timeline
//	   - the leftmost vertical line represents the go routine that
//	     is executing GenericAPIServer.Run method
//	   - (signal name) indicates that the given lifecycle signal has been fired
//
//	                                 stopCh
//	                                   |
//	             |--------------------------------------------|
//	             |                                            |
//		    call PreShutdownHooks                        (ShutdownInitiated)
//	             |                                            |
//	  (PreShutdownHooksStopped)                   Sleep(ShutdownDelayDuration)
//	             |                                            |
//	             |                                 (AfterShutdownDelayDuration)
//	             |                                            |
//	             |                                            |
//	             |--------------------------------------------|
//	             |                                            |
//	             |                                 (NotAcceptingNewRequest)
//	             |                                            |
//	             |                       |-------------------------------------------------|
//	             |                       |                                                 |
//	             |             close(stopHttpServerCh)                         NonLongRunningRequestWaitGroup.Wait()
//	             |                       |                                                 |
//	             |            server.Shutdown(timeout=60s)                                 |
//	             |                       |                                         WatchRequestWaitGroup.Wait()
//	             |              stop listener (net/http)                                   |
//	             |                       |                                                 |
//	             |          |-------------------------------------|                        |
//	             |          |                                     |                        |
//	             |          |                      (HTTPServerStoppedListening)            |
//	             |          |                                                              |
//	             |    wait up to 60s                                                       |
//	             |          |                                                  (InFlightRequestsDrained)
//	             |          |
//	             |          |
//	             |	stoppedCh is closed
//	             |
//	             |
//	   <-drainedCh.Signaled()
//	             |
//	  s.AuditBackend.Shutdown()
//	             |
//	     <-listenerStoppedCh
//	             |
//	        <-stoppedCh
//	             |
//	         return nil
func TestGracefulTerminationWithKeepListeningDuringGracefulTerminationDisabled(t *testing.T) {
	fakeAudit := &fakeAudit{}
	s := newGenericAPIServer(t, fakeAudit, false)
	connReusingClient := newClient(false)
	doer := setupDoer(t, s.SecureServingInfo)

	// handler for a non long-running and a watch request that
	// we want to keep in flight through to the end.
	inflightNonLongRunning := setupInFlightNonLongRunningRequestHandler(s)
	inflightWatch := setupInFlightWatchRequestHandler(s)

	// API calls from the pre-shutdown hook(s) must succeed up to
	// the point where the HTTP server is shut down.
	preShutdownHook := setupPreShutdownHookHandler(t, s, doer, newClient(true))

	signals := &s.lifecycleSignals
	recorder := &signalRecorder{}
	wrapLifecycleSignalsWithRecorder(t, signals, recorder.before)

	// before the AfterShutdownDelayDuration signal is fired, we want
	// the test to execute a verification step.
	beforeShutdownDelayDurationStep := newSignalInterceptingTestStep()
	signals.AfterShutdownDelayDuration = wrapLifecycleSignal(t, signals.AfterShutdownDelayDuration, func(_ lifecycleSignal) {
		// wait for the test to execute verification steps before
		// the server signals the next steps
		<-beforeShutdownDelayDurationStep.done()
	}, nil)

	// start the API server
	_, ctx := ktesting.NewTestContext(t)
	stopCtx, stop := context.WithCancelCause(ctx)
	defer stop(errors.New("test has completed"))
	runCompletedCh := make(chan struct{})
	go func() {
		defer close(runCompletedCh)
		if err := s.PrepareRun().RunWithContext(stopCtx); err != nil {
			t.Errorf("unexpected error from RunWithContext: %v", err)
		}
	}()
	waitForAPIServerStarted(t, doer)

	// fire the non long-running and the watch request so it is
	// in-flight on the server now, and we will unblock them
	// after ShutdownDelayDuration elapses.
	inflightNonLongRunning.launch(doer, connReusingClient)
	waitForeverUntil(t, inflightNonLongRunning.startedCh, "in-flight non long-running request did not reach the server")
	inflightWatch.launch(doer, connReusingClient)
	waitForeverUntil(t, inflightWatch.startedCh, "in-flight watch request did not reach the server")

	// /readyz should return OK
	resultGot := doer.Do(newClient(true), func(httptrace.GotConnInfo) {}, "/readyz", time.Second)
	if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}

	// signal termination event: initiate a shutdown
	stop(errors.New("shutting down"))
	waitForeverUntilSignaled(t, signals.ShutdownInitiated)

	// /readyz must return an error, but we need to give it some time
	err := wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
		resultGot := doer.Do(newClient(true), func(httptrace.GotConnInfo) {}, "/readyz", time.Second)
		// wait until we have a non 200 response
		if resultGot.response != nil && resultGot.response.StatusCode == http.StatusOK {
			return false, nil
		}

		if err := assertResponseStatusCode(resultGot, http.StatusInternalServerError); err != nil {
			return true, err
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("Expected /readyz to return 500 status code, but got: %v", err)
	}

	// before ShutdownDelayDuration elapses new request(s) should be served successfully.
	beforeShutdownDelayDurationStep.execute(func() {
		t.Log("Before ShutdownDelayDuration elapses new request(s) should be served")
		resultGot := doer.Do(connReusingClient, shouldReuseConnection(t), "/echo?message=request-on-an-existing-connection-should-succeed", time.Second)
		if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
			t.Errorf("%s", err.Error())
		}
		resultGot = doer.Do(newClient(true), shouldUseNewConnection(t), "/echo?message=request-on-a-new-tcp-connection-should-succeed", time.Second)
		if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
			t.Errorf("%s", err.Error())
		}
	})

	waitForeverUntilSignaled(t, signals.AfterShutdownDelayDuration)

	// preshutdown hook has not completed yet, new incoming request should succeed
	resultGot = doer.Do(newClient(true), shouldUseNewConnection(t), "/echo?message=request-on-a-new-tcp-connection-should-succeed", time.Second)
	if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}

	// let the preshutdown hook issue an API call now, and then
	// let's wait for it to return the result.
	close(preShutdownHook.blockedCh)
	preShutdownHookResult := <-preShutdownHook.resultCh
	waitForeverUntilSignaled(t, signals.PreShutdownHooksStopped)
	if err := assertResponseStatusCode(preShutdownHookResult, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}

	waitForeverUntilSignaled(t, signals.PreShutdownHooksStopped)
	// both AfterShutdownDelayDuration and PreShutdownHooksCompleted
	// have been signaled, we should not be accepting new request
	waitForeverUntilSignaled(t, signals.NotAcceptingNewRequest)
	waitForeverUntilSignaled(t, signals.HTTPServerStoppedListening)

	resultGot = doer.Do(newClient(true), shouldUseNewConnection(t), "/echo?message=request-on-a-new-tcp-connection-should-fail-with-503", time.Second)
	if !utilnet.IsConnectionRefused(resultGot.err) {
		t.Errorf("Expected error %v, but got: %v %v", syscall.ECONNREFUSED, resultGot.err, resultGot.response)
	}

	// even though Server.Serve() has returned, an existing connection on
	// the server may take some time to be in "closing" state, the following
	// poll eliminates any flake due to that delay.
	if err := wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
		result := doer.Do(connReusingClient, shouldReuseConnection(t), "/echo?message=waiting-for-the-existing-connection-to-reject-incoming-request", time.Second)
		if result.response != nil {
			t.Logf("Still waiting for the server to return error - response: %v", result.response)
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Errorf("Expected no error, but got: %v", err)
	}

	// TODO: our original intention was for any incoming request to receive a 503
	// via the WithWaitGroup filter, but, at this point, any incoming requests
	// will get a 'connection refused' error since the net/http server has
	// stopped listening.
	resultGot = doer.Do(connReusingClient, shouldReuseConnection(t), "/echo?message=request-on-an-existing-connection-should-fail-with-error", time.Second)
	if !utilnet.IsConnectionRefused(resultGot.err) {
		t.Errorf("Expected error %v, but got: %v %v", syscall.ECONNREFUSED, resultGot.err, resultGot.response)
	}

	// the server has stopped listening but we still have a non long-running,
	// and a watch request in flight, unblock both of these, and we expect
	// the requests to return appropriate response to the caller.
	inflightNonLongRunningResultGot := inflightNonLongRunning.unblockAndWaitForResult(t)
	if err := assertResponseStatusCode(inflightNonLongRunningResultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}
	if err := assertRequestAudited(inflightNonLongRunningResultGot, fakeAudit); err != nil {
		t.Errorf("%s", err.Error())
	}
	inflightWatchResultGot := inflightWatch.unblockAndWaitForResult(t)
	if err := assertResponseStatusCode(inflightWatchResultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}
	if err := assertRequestAudited(inflightWatchResultGot, fakeAudit); err != nil {
		t.Errorf("%s", err.Error())
	}

	// all requests in flight have drained
	waitForeverUntilSignaled(t, signals.InFlightRequestsDrained)

	t.Log("Waiting for the apiserver Run method to return")
	waitForeverUntil(t, runCompletedCh, "the apiserver Run method did not return")

	if !fakeAudit.shutdownCompleted() {
		t.Errorf("Expected AuditBackend.Shutdown to be completed")
	}

	if err := recorder.verify([]string{
		"ShutdownInitiated",
		"AfterShutdownDelayDuration",
		"PreShutdownHooksStopped",
		"NotAcceptingNewRequest",
		"HTTPServerStoppedListening",
		"InFlightRequestsDrained",
	}); err != nil {
		t.Errorf("%s", err.Error())
	}
}

// This test exercises the graceful termination scenario
// described in the following diagram
//
//   - every vertical line is an independent timeline
//
//   - the leftmost vertical line represents the go routine that
//     is executing GenericAPIServer.Run method
//
//   - (signal) indicates that the given lifecycle signal has been fired
//
//     stopCh
//     |
//     |--------------------------------------------|
//     |                                            |
//     call PreShutdownHooks                       (ShutdownInitiated)
//     |                                            |
//     (PreShutdownHooksCompleted)                  Sleep(ShutdownDelayDuration)
//     |                                            |
//     |                                 (AfterShutdownDelayDuration)
//     |                                            |
//     |                                            |
//     |--------------------------------------------|
//     |                                            |
//     |                               (NotAcceptingNewRequest)
//     |                                            |
//     |                              NonLongRunningRequestWaitGroup.Wait()
//     |                                            |
//     |                                 WatchRequestWaitGroup.Wait()
//     |                                            |
//     |                                (InFlightRequestsDrained)
//     |                                            |
//     |                                            |
//     |------------------------------------------------------------|
//     |                                                            |
//     <-drainedCh.Signaled()                                     close(stopHttpServerCh)
//     |                                                            |
//     s.AuditBackend.Shutdown()                                 server.Shutdown(timeout=2s)
//     |                                                            |
//     |                                                   stop listener (net/http)
//     |                                                            |
//     |                                         |-------------------------------------|
//     |                                         |                                     |
//     |                                   wait up to 2s                 (HTTPServerStoppedListening)
//     <-listenerStoppedCh                                |
//     |                                stoppedCh is closed
//     <-stoppedCh
//     |
//     return nil
func TestGracefulTerminationWithKeepListeningDuringGracefulTerminationEnabled(t *testing.T) {
	fakeAudit := &fakeAudit{}
	s := newGenericAPIServer(t, fakeAudit, true)
	connReusingClient := newClient(false)
	doer := setupDoer(t, s.SecureServingInfo)

	// handler for a non long-running and a watch request that
	// we want to keep in flight through to the end.
	inflightNonLongRunning := setupInFlightNonLongRunningRequestHandler(s)
	inflightWatch := setupInFlightWatchRequestHandler(s)

	// API calls from the pre-shutdown hook(s) must succeed up to
	// the point where the HTTP server is shut down.
	preShutdownHook := setupPreShutdownHookHandler(t, s, doer, newClient(true))

	signals := &s.lifecycleSignals
	recorder := &signalRecorder{}
	wrapLifecycleSignalsWithRecorder(t, signals, recorder.before)

	// before the AfterShutdownDelayDuration signal is fired, we want
	// the test to execute a verification step.
	beforeShutdownDelayDurationStep := newSignalInterceptingTestStep()
	signals.AfterShutdownDelayDuration = wrapLifecycleSignal(t, signals.AfterShutdownDelayDuration, func(_ lifecycleSignal) {
		// Before AfterShutdownDelayDuration event is signaled, the test
		// will send request(s) to assert on expected behavior.
		<-beforeShutdownDelayDurationStep.done()
	}, nil)

	// start the API server
	_, ctx := ktesting.NewTestContext(t)
	stopCtx, stop := context.WithCancelCause(ctx)
	defer stop(errors.New("test has completed"))
	runCompletedCh := make(chan struct{})
	go func() {
		defer close(runCompletedCh)
		if err := s.PrepareRun().RunWithContext(stopCtx); err != nil {
			t.Errorf("unexpected error from RunWithContext: %v", err)
		}
	}()
	waitForAPIServerStarted(t, doer)

	// fire the non long-running and the watch request so it is
	// in-flight on the server now, and we will unblock them
	// after ShutdownDelayDuration elapses.
	inflightNonLongRunning.launch(doer, connReusingClient)
	waitForeverUntil(t, inflightNonLongRunning.startedCh, "in-flight request did not reach the server")
	inflightWatch.launch(doer, connReusingClient)
	waitForeverUntil(t, inflightWatch.startedCh, "in-flight watch request did not reach the server")

	// /readyz should return OK
	resultGot := doer.Do(newClient(true), func(httptrace.GotConnInfo) {}, "/readyz", time.Second)
	if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}

	// signal termination event: initiate a shutdown
	stop(errors.New("shutting down"))
	waitForeverUntilSignaled(t, signals.ShutdownInitiated)

	// /readyz must return an error, but we need to give it some time
	err := wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
		resultGot := doer.Do(newClient(true), func(httptrace.GotConnInfo) {}, "/readyz", time.Second)
		// wait until we have a non 200 response
		if resultGot.response != nil && resultGot.response.StatusCode == http.StatusOK {
			return false, nil
		}

		if err := assertResponseStatusCode(resultGot, http.StatusInternalServerError); err != nil {
			return true, err
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("Expected /readyz to return 500 status code, but got: %v", err)
	}

	// before ShutdownDelayDuration elapses new request(s) should be served successfully.
	beforeShutdownDelayDurationStep.execute(func() {
		t.Log("Before ShutdownDelayDuration elapses new request(s) should be served")
		resultGot := doer.Do(connReusingClient, shouldReuseConnection(t), "/echo?message=request-on-an-existing-connection-should-succeed", time.Second)
		if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
			t.Errorf("%s", err.Error())
		}
		resultGot = doer.Do(newClient(true), shouldUseNewConnection(t), "/echo?message=request-on-a-new-tcp-connection-should-succeed", time.Second)
		if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
			t.Errorf("%s", err.Error())
		}
	})

	waitForeverUntilSignaled(t, signals.AfterShutdownDelayDuration)

	// preshutdown hook has not completed yet, new incoming request should succeed
	resultGot = doer.Do(newClient(true), shouldUseNewConnection(t), "/echo?message=request-on-a-new-tcp-connection-should-succeed", time.Second)
	if err := assertResponseStatusCode(resultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}

	// let the preshutdown hook issue an API call now, and then let's wait
	// for it to return the result, it should succeed.
	close(preShutdownHook.blockedCh)
	preShutdownHookResult := <-preShutdownHook.resultCh
	waitForeverUntilSignaled(t, signals.PreShutdownHooksStopped)
	if err := assertResponseStatusCode(preShutdownHookResult, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}

	waitForeverUntilSignaled(t, signals.NotAcceptingNewRequest)

	// both AfterShutdownDelayDuration and PreShutdownHooksCompleted
	// have been signaled, any incoming request should receive 429
	resultGot = doer.Do(newClient(true), shouldUseNewConnection(t), "/echo?message=request-on-a-new-tcp-connection-should-fail-with-429", time.Second)
	if err := requestMustFailWithRetryHeader(resultGot, http.StatusTooManyRequests); err != nil {
		t.Errorf("%s", err.Error())
	}
	resultGot = doer.Do(connReusingClient, shouldReuseConnection(t), "/echo?message=request-on-an-existing-connection-should-fail-with-429", time.Second)
	if err := requestMustFailWithRetryHeader(resultGot, http.StatusTooManyRequests); err != nil {
		t.Errorf("%s", err.Error())
	}

	// we still have a non long-running, and a watch request in flight,
	// unblock both of these, and we expect the requests
	// to return appropriate response to the caller.
	inflightNonLongRunningResultGot := inflightNonLongRunning.unblockAndWaitForResult(t)
	if err := assertResponseStatusCode(inflightNonLongRunningResultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}
	if err := assertRequestAudited(inflightNonLongRunningResultGot, fakeAudit); err != nil {
		t.Errorf("%s", err.Error())
	}
	inflightWatchResultGot := inflightWatch.unblockAndWaitForResult(t)
	if err := assertResponseStatusCode(inflightWatchResultGot, http.StatusOK); err != nil {
		t.Errorf("%s", err.Error())
	}
	if err := assertRequestAudited(inflightWatchResultGot, fakeAudit); err != nil {
		t.Errorf("%s", err.Error())
	}

	// all requests in flight have drained
	waitForeverUntilSignaled(t, signals.InFlightRequestsDrained)
	waitForeverUntilSignaled(t, signals.HTTPServerStoppedListening)

	t.Log("Waiting for the apiserver Run method to return")
	waitForeverUntil(t, runCompletedCh, "the apiserver Run method did not return")

	if !fakeAudit.shutdownCompleted() {
		t.Errorf("Expected AuditBackend.Shutdown to be completed")
	}

	if err := recorder.verify([]string{
		"ShutdownInitiated",
		"AfterShutdownDelayDuration",
		"PreShutdownHooksStopped",
		"NotAcceptingNewRequest",
		"InFlightRequestsDrained",
		"HTTPServerStoppedListening",
	}); err != nil {
		t.Errorf("%s", err.Error())
	}
}

func TestMuxAndDiscoveryComplete(t *testing.T) {
	// setup
	testSignal1 := make(chan struct{})
	testSignal2 := make(chan struct{})
	s := newGenericAPIServer(t, &fakeAudit{}, true)
	s.muxAndDiscoveryCompleteSignals["TestSignal1"] = testSignal1
	s.muxAndDiscoveryCompleteSignals["TestSignal2"] = testSignal2
	doer := setupDoer(t, s.SecureServingInfo)
	isChanClosed := func(ch <-chan struct{}, delay time.Duration) bool {
		time.Sleep(delay)
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	// start the API server
	_, ctx := ktesting.NewTestContext(t)
	stopCtx, stop := context.WithCancelCause(ctx)
	defer stop(errors.New("test has completed"))
	runCompletedCh := make(chan struct{})
	go func() {
		defer close(runCompletedCh)
		if err := s.PrepareRun().RunWithContext(stopCtx); err != nil {
			t.Errorf("unexpected error from RunWithContext: %v", err)
		}
	}()
	waitForAPIServerStarted(t, doer)

	// act
	if isChanClosed(s.lifecycleSignals.MuxAndDiscoveryComplete.Signaled(), 1*time.Second) {
		t.Fatalf("%s is closed whereas the TestSignal is still open", s.lifecycleSignals.MuxAndDiscoveryComplete.Name())
	}

	close(testSignal1)
	if isChanClosed(s.lifecycleSignals.MuxAndDiscoveryComplete.Signaled(), 1*time.Second) {
		t.Fatalf("%s is closed whereas the TestSignal2 is still open", s.lifecycleSignals.MuxAndDiscoveryComplete.Name())
	}

	close(testSignal2)
	if !isChanClosed(s.lifecycleSignals.MuxAndDiscoveryComplete.Signaled(), 1*time.Second) {
		t.Fatalf("%s wasn't closed", s.lifecycleSignals.MuxAndDiscoveryComplete.Name())
	}
}

func TestPreShutdownHooks(t *testing.T) {
	tests := []struct {
		name   string
		server func() *GenericAPIServer
	}{
		{
			name: "ShutdownSendRetryAfter is disabled",
			server: func() *GenericAPIServer {
				return newGenericAPIServer(t, &fakeAudit{}, false)
			},
		},
		{
			name: "ShutdownSendRetryAfter is enabled",
			server: func() *GenericAPIServer {
				return newGenericAPIServer(t, &fakeAudit{}, true)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			stopCtx, stop := context.WithCancelCause(ctx)
			defer stop(errors.New("test has completed"))
			s := test.server()
			doer := setupDoer(t, s.SecureServingInfo)

			// preshutdown hook should not block when sending to the error channel
			preShutdownHookErrCh := make(chan error, 1)
			err := s.AddPreShutdownHook("test-backend", func() error {
				// this pre-shutdown hook waits for the shutdown duration to elapse,
				// and then send a series of requests to the apiserver, and
				// we expect these series of requests to be completed successfully
				<-s.lifecycleSignals.AfterShutdownDelayDuration.Signaled()

				// we send 5 requests, one every second
				var err error
				client := newClient(true)
				for i := 0; i < 5; i++ {
					r := doer.Do(client, func(httptrace.GotConnInfo) {}, fmt.Sprintf("/echo?message=attempt-%d", i), 1*time.Second)
					err = r.err
					if err == nil && r.response.StatusCode != http.StatusOK {
						err = fmt.Errorf("did not get status code 200 - %#v", r.response)
						break
					}
					time.Sleep(time.Second)
				}
				preShutdownHookErrCh <- err
				return nil
			})
			if err != nil {
				t.Fatalf("Failed to add pre-shutdown hook - %v", err)
			}

			// start the API server
			runCompletedCh := make(chan struct{})
			go func() {
				defer close(runCompletedCh)
				if err := s.PrepareRun().RunWithContext(stopCtx); err != nil {
					t.Errorf("unexpected error from RunWithContext: %v", err)
				}
			}()
			waitForAPIServerStarted(t, doer)

			stop(errors.New("shutting down"))

			waitForeverUntil(t, runCompletedCh, "the apiserver Run method did not return")

			select {
			case err := <-preShutdownHookErrCh:
				if err != nil {
					t.Errorf("PreSHutdown hook can not access the API server - %v", err)
				}
			case <-time.After(wait.ForeverTestTimeout):
				t.Fatalf("pre-shutdown hook did not complete as expected")
			}
		})
	}
}

type signalRecorder struct {
	lock  sync.Mutex
	order []string
}

func (r *signalRecorder) before(s lifecycleSignal) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.order = append(r.order, s.Name())
}

func (r *signalRecorder) verify(got []string) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	want := r.order
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("Expected order of termination event signal to match, diff: %s", cmp.Diff(want, got))
	}
	return nil
}

type inFlightRequest struct {
	blockedCh, startedCh chan struct{}
	resultCh             chan result
	url                  string
}

func setupInFlightNonLongRunningRequestHandler(s *GenericAPIServer) *inFlightRequest {
	inflight := &inFlightRequest{
		blockedCh: make(chan struct{}),
		startedCh: make(chan struct{}),
		resultCh:  make(chan result),
		url:       "/in-flight-non-long-running-request-as-designed",
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(inflight.startedCh)
		// this request handler blocks until we deliberately unblock it.
		<-inflight.blockedCh
		w.WriteHeader(http.StatusOK)
	})
	s.Handler.NonGoRestfulMux.Handle(inflight.url, handler)
	return inflight
}

func setupInFlightWatchRequestHandler(s *GenericAPIServer) *inFlightRequest {
	inflight := &inFlightRequest{
		blockedCh: make(chan struct{}),
		startedCh: make(chan struct{}),
		resultCh:  make(chan result),
		url:       "/apis/watches.group/v1/namespaces/foo/bar?watch=true",
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(inflight.startedCh)
		// this request handler blocks until we deliberately unblock it.
		<-inflight.blockedCh

		// this simulates a watch well enough for our test
		signals := apirequest.ServerShutdownSignalFrom(req.Context())
		if signals == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		<-signals.ShuttingDown()
		w.WriteHeader(http.StatusOK)
	})
	s.Handler.NonGoRestfulMux.Handle("/apis/watches.group/v1/namespaces/foo/bar", handler)
	return inflight
}

func (ifr *inFlightRequest) launch(doer doer, client *http.Client) {
	go func() {
		result := doer.Do(client, func(httptrace.GotConnInfo) {}, ifr.url, 0)
		ifr.resultCh <- result
	}()
}

func (ifr *inFlightRequest) unblockAndWaitForResult(t *testing.T) result {
	close(ifr.blockedCh)

	var resultGot result
	select {
	case resultGot = <-ifr.resultCh:
		return resultGot
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatal("Expected the server to send a response")
	}
	return resultGot
}

type preShutdownHookHandler struct {
	blockedCh chan struct{}
	resultCh  chan result
}

func setupPreShutdownHookHandler(t *testing.T, s *GenericAPIServer, doer doer, client *http.Client) *preShutdownHookHandler {
	hook := &preShutdownHookHandler{
		blockedCh: make(chan struct{}),
		resultCh:  make(chan result),
	}
	if err := s.AddPreShutdownHook("test-preshutdown-hook", func() error {
		// wait until the test commands this pre shutdown
		// hook to invoke an API call.
		<-hook.blockedCh

		resultGot := doer.Do(client, func(httptrace.GotConnInfo) {}, "/echo?message=request-from-pre-shutdown-hook-should-succeed", time.Second)
		hook.resultCh <- resultGot
		return nil
	}); err != nil {
		t.Fatalf("Failed to register preshutdown hook - %v", err)
	}

	return hook
}

type fakeAudit struct {
	shutdownCh chan struct{}
	lock       sync.Mutex
	audits     map[string]struct{}
	completed  bool
}

func (a *fakeAudit) Run(stopCh <-chan struct{}) error {
	a.shutdownCh = make(chan struct{})
	go func() {
		defer close(a.shutdownCh)
		<-stopCh
	}()
	return nil
}

func (a *fakeAudit) Shutdown() {
	<-a.shutdownCh

	a.lock.Lock()
	defer a.lock.Unlock()
	a.completed = true
}

func (a *fakeAudit) String() string {
	return "fake-audit"
}

func (a *fakeAudit) shutdownCompleted() bool {
	a.lock.Lock()
	defer a.lock.Unlock()

	return a.completed
}

func (a *fakeAudit) ProcessEvents(events ...*auditinternal.Event) bool {
	a.lock.Lock()
	defer a.lock.Unlock()
	if len(a.audits) == 0 {
		a.audits = map[string]struct{}{}
	}
	for _, event := range events {
		a.audits[string(event.AuditID)] = struct{}{}
	}

	return true
}

func (a *fakeAudit) requestAudited(auditID string) bool {
	a.lock.Lock()
	defer a.lock.Unlock()
	_, exists := a.audits[auditID]
	return exists
}

func (a *fakeAudit) EvaluatePolicyRule(attrs authorizer.Attributes) audit.RequestAuditConfig {
	return audit.RequestAuditConfig{
		Level: auditinternal.LevelMetadata,
	}
}

func assertRequestAudited(resultGot result, backend *fakeAudit) error {
	resp := resultGot.response
	if resp == nil {
		return fmt.Errorf("Expected a response, but got nil")
	}
	auditIDGot := resp.Header.Get(auditinternal.HeaderAuditID)
	if len(auditIDGot) == 0 {
		return fmt.Errorf("Expected non-empty %q response header, but got: %v", auditinternal.HeaderAuditID, resp)
	}
	if !backend.requestAudited(auditIDGot) {
		return fmt.Errorf("Expected the request to be audited: %q", auditIDGot)
	}
	return nil
}

func waitForeverUntilSignaled(t *testing.T, s lifecycleSignal) {
	waitForeverUntil(t, s.Signaled(), fmt.Sprintf("Expected the server to signal %s event", s.Name()))
}

func waitForeverUntil(t *testing.T, ch <-chan struct{}, msg string) {
	select {
	case <-ch:
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("%s", msg)
	}
}

func shouldReuseConnection(t *testing.T) func(httptrace.GotConnInfo) {
	return func(ci httptrace.GotConnInfo) {
		if !ci.Reused {
			t.Errorf("Expected the request to use an existing TCP connection, but got: %+v", ci)
		}
	}
}

func shouldUseNewConnection(t *testing.T) func(httptrace.GotConnInfo) {
	return func(ci httptrace.GotConnInfo) {
		if ci.Reused {
			t.Errorf("Expected the request to use a new TCP connection, but got: %+v", ci)
		}
	}
}

func assertResponseStatusCode(resultGot result, statusCodeExpected int) error {
	if resultGot.err != nil {
		return fmt.Errorf("Expected no error, but got: %v", resultGot.err)
	}
	if resultGot.response.StatusCode != statusCodeExpected {
		return fmt.Errorf("Expected Status Code: %d, but got: %d", statusCodeExpected, resultGot.response.StatusCode)
	}
	return nil
}

func requestMustFailWithRetryHeader(resultGot result, statusCodedExpected int) error {
	if resultGot.err != nil {
		return fmt.Errorf("Expected no error, but got: %v", resultGot.err)
	}
	if statusCodedExpected != resultGot.response.StatusCode {
		return fmt.Errorf("Expected Status Code: %d, but got: %d", statusCodedExpected, resultGot.response.StatusCode)
	}
	retryAfterGot := resultGot.response.Header.Get("Retry-After")
	if retryAfterGot != "5" {
		return fmt.Errorf("Expected Retry-After Response Header, but got: %v", resultGot.response)
	}
	return nil
}

func waitForAPIServerStarted(t *testing.T, doer doer) {
	client := newClient(true)
	i := 1
	err := wait.PollImmediate(100*time.Millisecond, 5*time.Second, func() (done bool, err error) {
		result := doer.Do(client, func(httptrace.GotConnInfo) {}, fmt.Sprintf("/echo?message=attempt-%d", i), time.Second)
		i++

		if result.err != nil {
			t.Logf("Still waiting for the server to start - err: %v", err)
			return false, nil
		}
		if result.response.StatusCode != http.StatusOK {
			t.Logf("Still waiting for the server to start - expecting: %d, but got: %v", http.StatusOK, result.response)
			return false, nil
		}

		t.Log("The API server has started")
		return true, nil
	})

	if err != nil {
		t.Fatalf("The server has failed to start - err: %v", err)
	}
}

func setupDoer(t *testing.T, info *SecureServingInfo) doer {
	_, port, err := info.HostPort()
	if err != nil {
		t.Fatalf("Expected host, port from SecureServingInfo, but got: %v", err)
	}

	return func(client *http.Client, callback func(httptrace.GotConnInfo), path string, timeout time.Duration) result {
		url := fmt.Sprintf("https://%s:%d%s", "127.0.0.1", port, path)
		t.Logf("Sending request - timeout: %s, url: %s", timeout, url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return result{response: nil, err: err}
		}

		// setup request timeout
		var ctx context.Context
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(req.Context(), timeout)
			defer cancel()

			req = req.WithContext(ctx)
		}

		// setup trace
		trace := &httptrace.ClientTrace{
			GotConn: func(connInfo httptrace.GotConnInfo) {
				callback(connInfo)
			},
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

		response, err := client.Do(req)
		// in this test, we don't depend on the body of the response, so we can
		// close the Body here to ensure the underlying transport can be reused
		if response != nil {
			io.ReadAll(response.Body)
			response.Body.Close()
		}
		return result{
			err:      err,
			response: response,
		}
	}
}

func newClient(useNewConnection bool) *http.Client {
	clientCACertPool := x509.NewCertPool()
	clientCACertPool.AppendCertsFromPEM(backendCrt)
	tlsConfig := &tls.Config{
		RootCAs:    clientCACertPool,
		NextProtos: []string{http2.NextProtoTLS},
	}

	tr := &http.Transport{
		TLSClientConfig:   tlsConfig,
		DisableKeepAlives: useNewConnection,
	}
	if err := http2.ConfigureTransport(tr); err != nil {
		log.Fatalf("Failed to configure HTTP2 transport: %v", err)
	}
	return &http.Client{
		Timeout:   0,
		Transport: tr,
	}
}

func newGenericAPIServer(t *testing.T, fAudit *fakeAudit, keepListening bool) *GenericAPIServer {
	config, _ := setUp(t)
	config.ShutdownDelayDuration = 100 * time.Millisecond
	config.ShutdownSendRetryAfter = keepListening
	// we enable watch draining, any positive value will do that
	config.ShutdownWatchTerminationGracePeriod = 2 * time.Second
	config.AuditPolicyRuleEvaluator = fAudit
	config.AuditBackend = fAudit

	s, err := config.Complete(nil).New("test", NewEmptyDelegate())
	if err != nil {
		t.Fatalf("Error in bringing up the server: %v", err)
	}

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("failed to listen on %v: %v", "0.0.0.0:0", err)
	}
	s.SecureServingInfo = &SecureServingInfo{}
	s.SecureServingInfo.Listener = &wrappedListener{ln, t}

	cert, err := dynamiccertificates.NewStaticCertKeyContent("serving-cert", backendCrt, backendKey)
	if err != nil {
		t.Fatalf("failed to load cert - %v", err)
	}
	s.SecureServingInfo.Cert = cert

	// we use this handler to send a test request to the server.
	s.Handler.NonGoRestfulMux.Handle("/echo", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Logf("[server] received a request, proto: %s, url: %s", req.Proto, req.RequestURI)

		w.Header().Add("echo", req.URL.Query().Get("message"))
		w.WriteHeader(http.StatusOK)
	}))

	return s
}

type wrappedListener struct {
	net.Listener
	t *testing.T
}

func (ln wrappedListener) Accept() (net.Conn, error) {
	c, err := ln.Listener.Accept()

	if tc, ok := c.(*net.TCPConn); ok {
		ln.t.Logf("[server] seen new connection: %#v", tc)
	}
	return c, err
}
