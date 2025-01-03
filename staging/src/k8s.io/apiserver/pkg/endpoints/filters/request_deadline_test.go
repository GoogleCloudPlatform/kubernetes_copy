/*
Copyright 2020 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/audit/policy"
	"k8s.io/apiserver/pkg/endpoints/request"
	testingclock "k8s.io/utils/clock/testing"
)

func TestParseTimeout(t *testing.T) {
	tests := []struct {
		name            string
		url             string
		expected        bool
		timeoutExpected time.Duration
		message         string
	}{
		{
			name: "the user does not specify a timeout",
			url:  "/api/v1/namespaces?timeout=",
		},
		{
			name:            "the user specifies a valid timeout",
			url:             "/api/v1/namespaces?timeout=10s",
			expected:        true,
			timeoutExpected: 10 * time.Second,
		},
		{
			name:     "the user specifies a timeout of 0s",
			url:      "/api/v1/namespaces?timeout=0s",
			expected: true,
		},
		{
			name:    "the user specifies an invalid timeout",
			url:     "/api/v1/namespaces?timeout=foo",
			message: invalidTimeoutInURL,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, test.url, nil)
			if err != nil {
				t.Fatalf("failed to create new http request - %v", err)
			}

			timeoutGot, ok, err := parseTimeout(request)

			if test.expected != ok {
				t.Errorf("expected: %t, but got: %t", test.expected, ok)
			}
			if test.timeoutExpected != timeoutGot {
				t.Errorf("expected timeout: %s, but got: %s", test.timeoutExpected, timeoutGot)
			}

			errMessageGot := message(err)
			if !strings.Contains(errMessageGot, test.message) {
				t.Errorf("expected error message to contain: %s, but got: %s", test.message, errMessageGot)
			}
		})
	}
}

func TestWithRequestDeadline(t *testing.T) {
	const requestTimeoutMaximum = 60 * time.Second
	tests := []struct {
		name                     string
		requestURL               string
		longRunning              bool
		hasDeadlineExpected      bool
		deadlineExpected         time.Duration
		handlerCallCountExpected int
		statusCodeExpected       int
	}{
		{
			name:                     "the user specifies a valid request timeout",
			requestURL:               "/api/v1/namespaces?timeout=15s",
			longRunning:              false,
			handlerCallCountExpected: 1,
			hasDeadlineExpected:      true,
			deadlineExpected:         14 * time.Second, // to account for the delay in verification
			statusCodeExpected:       http.StatusOK,
		},
		{
			name:                     "the user specifies a valid request timeout",
			requestURL:               "/api/v1/namespaces?timeout=15s",
			longRunning:              false,
			handlerCallCountExpected: 1,
			hasDeadlineExpected:      true,
			deadlineExpected:         14 * time.Second, // to account for the delay in verification
			statusCodeExpected:       http.StatusOK,
		},
		{
			name:                     "the specified timeout is 0s, default deadline is expected to be set",
			requestURL:               "/api/v1/namespaces?timeout=0s",
			longRunning:              false,
			handlerCallCountExpected: 1,
			hasDeadlineExpected:      true,
			deadlineExpected:         requestTimeoutMaximum - time.Second, // to account for the delay in verification
			statusCodeExpected:       http.StatusOK,
		},
		{
			name:                     "the user does not specify any request timeout, default deadline is expected to be set",
			requestURL:               "/api/v1/namespaces?timeout=",
			longRunning:              false,
			handlerCallCountExpected: 1,
			hasDeadlineExpected:      true,
			deadlineExpected:         requestTimeoutMaximum - time.Second, // to account for the delay in verification
			statusCodeExpected:       http.StatusOK,
		},
		{
			name:                     "the request is long running, no deadline is expected to be set",
			requestURL:               "/api/v1/namespaces?timeout=10s",
			longRunning:              true,
			hasDeadlineExpected:      false,
			handlerCallCountExpected: 1,
			statusCodeExpected:       http.StatusOK,
		},
		{
			name:               "the timeout specified is malformed, the request is aborted with HTTP 400",
			requestURL:         "/api/v1/namespaces?timeout=foo",
			longRunning:        false,
			statusCodeExpected: http.StatusBadRequest,
		},
		{
			name:                     "the timeout specified exceeds the maximum deadline allowed, the default deadline is used",
			requestURL:               fmt.Sprintf("/api/v1/namespaces?timeout=%s", requestTimeoutMaximum+time.Second),
			longRunning:              false,
			statusCodeExpected:       http.StatusOK,
			handlerCallCountExpected: 1,
			hasDeadlineExpected:      true,
			deadlineExpected:         requestTimeoutMaximum - time.Second, // to account for the delay in verification
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				callCount      int
				hasDeadlineGot bool
				deadlineGot    time.Duration
			)
			handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				callCount++
				deadlineGot, hasDeadlineGot = deadline(req)
			})

			fakeSink := &fakeAuditSink{}
			fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
			withDeadline := WithRequestDeadline(handler, fakeSink, fakeRuleEvaluator,
				func(_ *http.Request, _ *request.RequestInfo) bool { return test.longRunning },
				newSerializer(), requestTimeoutMaximum)
			withDeadline = WithRequestInfo(withDeadline, &fakeRequestResolver{})

			testRequest := newRequest(t, test.requestURL)

			// make sure a default request does not have any deadline set
			remaning, ok := deadline(testRequest)
			if ok {
				t.Fatalf("test setup failed, expected the new HTTP request context to have no deadline but got: %s", remaning)
			}

			w := httptest.NewRecorder()
			withDeadline.ServeHTTP(w, testRequest)

			if test.handlerCallCountExpected != callCount {
				t.Errorf("expected the request handler to be invoked %d times, but was actually invoked %d times", test.handlerCallCountExpected, callCount)
			}

			if test.hasDeadlineExpected != hasDeadlineGot {
				t.Errorf("expected the request context to have deadline set: %t but got: %t", test.hasDeadlineExpected, hasDeadlineGot)
			}

			deadlineGot = deadlineGot.Truncate(time.Second)
			if test.deadlineExpected != deadlineGot {
				t.Errorf("expected a request context with a deadline of %s but got: %s", test.deadlineExpected, deadlineGot)
			}

			statusCodeGot := w.Result().StatusCode
			if test.statusCodeExpected != statusCodeGot {
				t.Errorf("expected status code %d but got: %d", test.statusCodeExpected, statusCodeGot)
			}
		})
	}
}

func TestWithRequestDeadlineWithClock(t *testing.T) {
	var (
		hasDeadlineGot bool
		deadlineGot    time.Duration
	)
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		deadlineGot, hasDeadlineGot = deadline(req)
	})

	// if the deadline filter uses the clock instead of using the request started timestamp from the context
	// then we will see a request deadline of about a minute.
	receivedTimestampExpected := time.Now().Add(time.Minute)
	fakeClock := testingclock.NewFakeClock(receivedTimestampExpected)

	fakeSink := &fakeAuditSink{}
	fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
	withDeadline := withRequestDeadline(handler, fakeSink, fakeRuleEvaluator,
		func(_ *http.Request, _ *request.RequestInfo) bool { return false }, newSerializer(), time.Minute, fakeClock)
	withDeadline = WithRequestInfo(withDeadline, &fakeRequestResolver{})

	testRequest := newRequest(t, "/api/v1/namespaces?timeout=1s")
	// the request has arrived just now.
	testRequest = testRequest.WithContext(request.WithReceivedTimestamp(testRequest.Context(), time.Now()))

	w := httptest.NewRecorder()
	withDeadline.ServeHTTP(w, testRequest)

	if !hasDeadlineGot {
		t.Error("expected the request context to have deadline set")
	}

	// we expect a deadline <= 1s since the filter should use the request started timestamp from the context.
	if deadlineGot > time.Second {
		t.Errorf("expected a request context with a deadline <= %s, but got: %s", time.Second, deadlineGot)
	}
}

func TestWithRequestDeadlineWithInvalidTimeoutIsAudited(t *testing.T) {
	var handlerInvoked bool
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		handlerInvoked = true
	})

	fakeSink := &fakeAuditSink{}
	fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
	withDeadline := WithRequestDeadline(handler, fakeSink, fakeRuleEvaluator,
		func(_ *http.Request, _ *request.RequestInfo) bool { return false }, newSerializer(), time.Minute)
	withDeadline = WithRequestInfo(withDeadline, &fakeRequestResolver{})

	testRequest := newRequest(t, "/api/v1/namespaces?timeout=foo")
	w := httptest.NewRecorder()
	withDeadline.ServeHTTP(w, testRequest)

	if handlerInvoked {
		t.Error("expected the request to fail and the handler to be skipped")
	}

	statusCodeGot := w.Result().StatusCode
	if statusCodeGot != http.StatusBadRequest {
		t.Errorf("expected status code %d, but got: %d", http.StatusBadRequest, statusCodeGot)
	}
	// verify that the audit event from the request context is written to the audit sink.
	if len(fakeSink.events) != 1 {
		t.Fatalf("expected audit sink to have 1 event, but got: %d", len(fakeSink.events))
	}
}

func TestWithRequestDeadlineWithPanic(t *testing.T) {
	var (
		panicErrGot interface{}
		ctxGot      context.Context
	)

	panicErrExpected := errors.New("apiserver panic'd")
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		ctxGot = req.Context()
		panic(panicErrExpected)
	})

	fakeSink := &fakeAuditSink{}
	fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
	withDeadline := WithRequestDeadline(handler, fakeSink, fakeRuleEvaluator,
		func(_ *http.Request, _ *request.RequestInfo) bool { return false }, newSerializer(), 1*time.Minute)
	withDeadline = WithRequestInfo(withDeadline, &fakeRequestResolver{})
	withPanicRecovery := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			panicErrGot = recover()
		}()
		withDeadline.ServeHTTP(w, req)
	})

	testRequest := newRequest(t, "/api/v1/namespaces?timeout=1s")
	w := httptest.NewRecorder()
	withPanicRecovery.ServeHTTP(w, testRequest)

	if panicErrExpected != panicErrGot {
		t.Errorf("expected panic error: %#v, but got: %#v", panicErrExpected, panicErrGot)
	}
	if ctxGot.Err() != context.Canceled {
		t.Error("expected the request context to be canceled on handler panic")
	}
}

func TestWithRequestDeadlineWithRequestTimesOut(t *testing.T) {
	timeout := 100 * time.Millisecond
	var errGot error
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		select {
		case <-time.After(timeout + time.Second):
			errGot = fmt.Errorf("expected the request context to have timed out in %s", timeout)
		case <-ctx.Done():
			errGot = ctx.Err()
		}
	})

	fakeSink := &fakeAuditSink{}
	fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)
	withDeadline := WithRequestDeadline(handler, fakeSink, fakeRuleEvaluator,
		func(_ *http.Request, _ *request.RequestInfo) bool { return false }, newSerializer(), 1*time.Minute)
	withDeadline = WithRequestInfo(withDeadline, &fakeRequestResolver{})

	testRequest := newRequest(t, fmt.Sprintf("/api/v1/namespaces?timeout=%s", timeout))
	w := httptest.NewRecorder()
	withDeadline.ServeHTTP(w, testRequest)

	if errGot != context.DeadlineExceeded {
		t.Errorf("expected error: %#v, but got: %#v", context.DeadlineExceeded, errGot)
	}
}

func TestWithFailedRequestAudit(t *testing.T) {
	tests := []struct {
		name                          string
		statusErr                     *apierrors.StatusError
		errorHandlerCallCountExpected int
		statusCodeExpected            int
		auditExpected                 bool
	}{
		{
			name:                          "bad request, the error handler is invoked and the request is audited",
			statusErr:                     apierrors.NewBadRequest("error serving request"),
			errorHandlerCallCountExpected: 1,
			statusCodeExpected:            http.StatusBadRequest,
			auditExpected:                 true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				errorHandlerCallCountGot int
				rwGot                    http.ResponseWriter
				requestGot               *http.Request
			)

			errorHandler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				http.Error(rw, "error serving request", http.StatusBadRequest)

				errorHandlerCallCountGot++
				requestGot = req
				rwGot = rw
			})

			fakeSink := &fakeAuditSink{}
			fakeRuleEvaluator := policy.NewFakePolicyRuleEvaluator(auditinternal.LevelRequestResponse, nil)

			withAudit := withFailedRequestAudit(errorHandler, test.statusErr, fakeSink, fakeRuleEvaluator)

			w := httptest.NewRecorder()
			testRequest := newRequest(t, "/apis/v1/namespaces/default/pods")
			info := request.RequestInfo{}
			testRequest = testRequest.WithContext(request.WithRequestInfo(testRequest.Context(), &info))

			withAudit.ServeHTTP(w, testRequest)

			if test.errorHandlerCallCountExpected != errorHandlerCallCountGot {
				t.Errorf("expected the testRequest handler to be invoked %d times, but was actually invoked %d times", test.errorHandlerCallCountExpected, errorHandlerCallCountGot)
			}

			statusCodeGot := w.Result().StatusCode
			if test.statusCodeExpected != statusCodeGot {
				t.Errorf("expected status code %d, but got: %d", test.statusCodeExpected, statusCodeGot)
			}

			if test.auditExpected {
				// verify that the right http.ResponseWriter is passed to the error handler
				_, ok := rwGot.(*auditResponseWriter)
				if !ok {
					t.Errorf("expected an http.ResponseWriter of type: %T but got: %T", &auditResponseWriter{}, rwGot)
				}

				auditContext := audit.AuditContextFrom(requestGot.Context())
				if auditContext == nil {
					t.Fatal("expected an audit event object but got nil")
				}
				if auditContext.GetEventStage() != auditinternal.StageResponseStarted {
					t.Errorf("expected audit event Stage: %s, but got: %s", auditinternal.StageResponseStarted, auditContext.GetEventStage())
				}
				if auditContext.GetEventResponseStatus() == nil {
					t.Fatal("expected a ResponseStatus field of the audit event object, but got nil")
				}
				if test.statusCodeExpected != int(auditContext.GetEventResponseStatus().Code) {
					t.Errorf("expected audit event ResponseStatus.Code: %d, but got: %d", test.statusCodeExpected, auditContext.GetEventResponseStatus().Code)
				}
				if test.statusErr.Error() != auditContext.GetEventResponseStatus().Message {
					t.Errorf("expected audit event ResponseStatus.Message: %s, but got: %s", test.statusErr, auditContext.GetEventResponseStatus().Message)
				}

				// verify that the audit event from the request context is written to the audit sink.
				if len(fakeSink.events) != 1 {
					t.Fatalf("expected audit sink to have 1 event, but got: %d", len(fakeSink.events))
				}
				auditEventFromSink := fakeSink.events[0]
				auditContext.Invoke(func(eventFromAuditContext *auditinternal.Event) bool {
					if !reflect.DeepEqual(eventFromAuditContext, auditEventFromSink) {
						t.Errorf("expected the audit event from the request context to be written to the audit sink, but got diffs: %s", cmp.Diff(auditContext, auditEventFromSink))
					}
					return true
				})
			}
		})
	}
}

func newRequest(t *testing.T, requestURL string) *http.Request {
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatalf("failed to create new http request - %v", err)
	}
	ctx := audit.WithAuditContext(req.Context())
	return req.WithContext(ctx)
}

func message(err error) string {
	if err != nil {
		return err.Error()
	}

	return ""
}

func newSerializer() runtime.NegotiatedSerializer {
	scheme := runtime.NewScheme()
	return serializer.NewCodecFactory(scheme).WithoutConversion()
}

type fakeRequestResolver struct{}

func (r fakeRequestResolver) NewRequestInfo(req *http.Request) (*request.RequestInfo, error) {
	return &request.RequestInfo{}, nil
}

func deadline(r *http.Request) (time.Duration, bool) {
	if deadline, ok := r.Context().Deadline(); ok {
		remaining := time.Until(deadline)
		return remaining, ok
	}

	return 0, false
}
