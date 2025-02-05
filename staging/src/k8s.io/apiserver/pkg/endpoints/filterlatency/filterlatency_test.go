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

package filterlatency

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	noopoteltrace "go.opentelemetry.io/otel/trace/noop"

	testingclock "k8s.io/utils/clock/testing"
)

func TestTrackStartedWithContextAlreadyHasFilterRecord(t *testing.T) {
	filterName := "my-filter"
	var (
		callCount    int
		filterRecord *requestFilterRecord
	)
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		// we expect the handler to be invoked just once.
		callCount++

		// we expect the filter record to be set in the context
		filterRecord = requestFilterRecordFrom(req.Context())
	})

	requestFilterStarted := time.Now()
	wrapped := trackStarted(handler, noopoteltrace.NewTracerProvider(), filterName, testingclock.NewFakeClock(requestFilterStarted))

	testRequest, err := http.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	if err != nil {
		t.Fatalf("failed to create new http request - %v", err)
	}
	testRequest = testRequest.WithContext(withRequestFilterRecord(testRequest.Context(), &requestFilterRecord{
		name:             "foo",
		startedTimestamp: time.Now(),
	}))

	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, testRequest)

	if callCount != 1 {
		t.Errorf("expected the given handler to be invoked once, but was actually invoked %d times", callCount)
	}
	if filterRecord == nil {
		t.Fatal("expected a filter record in the request context, but got nil")
	}
	if filterName != filterRecord.name {
		t.Errorf("expected filter name=%s but got=%s", filterName, filterRecord.name)
	}
	if requestFilterStarted != filterRecord.startedTimestamp {
		t.Errorf("expected filter started timestamp=%s but got=%s", requestFilterStarted, filterRecord.startedTimestamp)
	}
}

func TestTrackStartedWithContextDoesNotHaveFilterRecord(t *testing.T) {
	filterName := "my-filter"
	var (
		callCount    int
		filterRecord *requestFilterRecord
	)
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		// we expect the handler to be invoked just once.
		callCount++

		// we expect the filter record to be set in the context
		filterRecord = requestFilterRecordFrom(req.Context())
	})

	requestFilterStarted := time.Now()
	wrapped := trackStarted(handler, noopoteltrace.NewTracerProvider(), filterName, testingclock.NewFakeClock(requestFilterStarted))

	testRequest, err := http.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	if err != nil {
		t.Fatalf("failed to create new http request - %v", err)
	}

	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, testRequest)

	if callCount != 1 {
		t.Errorf("expected the given handler to be invoked once, but was actually invoked %d times", callCount)
	}
	if filterRecord == nil {
		t.Fatal("expected a filter record in the request context, but got nil")
	}
	if filterName != filterRecord.name {
		t.Errorf("expected filter name=%s but got=%s", filterName, filterRecord.name)
	}
	if requestFilterStarted != filterRecord.startedTimestamp {
		t.Errorf("expected filter started timestamp=%s but got=%s", requestFilterStarted, filterRecord.startedTimestamp)
	}
}

func TestTrackCompletedContextHasFilterRecord(t *testing.T) {
	var (
		handlerCallCount     int
		actionCallCount      int
		filterRecordGot      *requestFilterRecord
		filterCompletedAtGot time.Time
	)
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		// we expect the handler to be invoked just once.
		handlerCallCount++
	})

	requestFilterEndedAt := time.Now()
	wrapped := trackCompleted(handler, testingclock.NewFakeClock(requestFilterEndedAt), func(_ context.Context, fr *requestFilterRecord, completedAt time.Time) {
		actionCallCount++
		filterRecordGot = fr
		filterCompletedAtGot = completedAt
	})

	testRequest, err := http.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	if err != nil {
		t.Fatalf("failed to create new http request - %v", err)
	}

	testRequest = testRequest.WithContext(withRequestFilterRecord(testRequest.Context(), &requestFilterRecord{}))

	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, testRequest)

	if handlerCallCount != 1 {
		t.Errorf("expected the given handler to be invoked once, but was actually invoked %d times", handlerCallCount)
	}
	if actionCallCount != 1 {
		t.Errorf("expected the action callback to be invoked once, but was actually invoked %d times", actionCallCount)
	}
	if filterRecordGot == nil {
		t.Fatal("expected a filter record in the request context, but got nil")
	}
	if requestFilterEndedAt != filterCompletedAtGot {
		t.Errorf("expected filter ended timestamp=%s but got=%s", requestFilterEndedAt, filterCompletedAtGot)
	}
}

func TestTrackCompletedContextDoesNotHaveFilterRecord(t *testing.T) {
	var actionCallCount, handlerCallCount int
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		handlerCallCount++
	})

	wrapped := trackCompleted(handler, testingclock.NewFakeClock(time.Now()), func(_ context.Context, _ *requestFilterRecord, _ time.Time) {
		actionCallCount++
	})

	testRequest, err := http.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	if err != nil {
		t.Fatalf("failed to create new http request - %v", err)
	}

	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, testRequest)

	if handlerCallCount != 1 {
		t.Errorf("expected the given handler to be invoked once, but was actually invoked %d times", handlerCallCount)
	}
	if actionCallCount != 0 {
		t.Errorf("expected the callback to not be invoked, but was actually invoked %d times", actionCallCount)
	}
}

func TestStartedAndCompletedOpenTelemetryTracing(t *testing.T) {
	filterName := "my-filter"
	// Setup OTel for testing
	fakeRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(fakeRecorder))

	// base handler func
	var callCount int
	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		// we expect the handler to be invoked just once.
		callCount++
	})
	// wrap with start and completed handler
	wrapped := TrackCompleted(handler)
	wrapped = TrackStarted(wrapped, tp, filterName)

	testRequest, err := http.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	if err != nil {
		t.Fatalf("failed to create new http request - %v", err)
	}

	wrapped.ServeHTTP(httptest.NewRecorder(), testRequest)

	if callCount != 1 {
		t.Errorf("expected the given handler to be invoked once, but was actually invoked %d times", callCount)
	}
	output := fakeRecorder.Ended()
	if len(output) != 1 {
		t.Fatalf("got %d; expected len(output) == 1", len(output))
	}
	span := output[0]
	if span.Name() != filterName {
		t.Fatalf("got %s; expected span.Name == my-filter", span.Name())
	}
}
