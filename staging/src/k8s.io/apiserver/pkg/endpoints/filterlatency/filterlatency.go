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
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server/httplog"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

type requestFilterRecordKeyType int

// requestFilterRecordKey is the context key for a request filter record struct.
const requestFilterRecordKey requestFilterRecordKeyType = iota

const minFilterLatencyToLog = 100 * time.Millisecond

type requestFilterRecord struct {
	name             string
	startedTimestamp time.Time
}

// withRequestFilterRecord attaches the given request filter record to the parent context.
func withRequestFilterRecord(parent context.Context, fr *requestFilterRecord) context.Context {
	return apirequest.WithValue(parent, requestFilterRecordKey, fr)
}

// requestFilterRecordFrom returns the request filter record from the given context.
func requestFilterRecordFrom(ctx context.Context) *requestFilterRecord {
	fr, _ := ctx.Value(requestFilterRecordKey).(*requestFilterRecord)
	return fr
}

// TrackStarted measures the timestamp the given handler has started execution
// by attaching a handler to the chain.
func TrackStarted(handler http.Handler, tp trace.TracerProvider, name string) http.Handler {
	return trackStarted(handler, tp, name, clock.RealClock{})
}

// TrackCompleted measures the timestamp the given handler has completed execution and then
// it updates the corresponding metric with the filter latency duration.
func TrackCompleted(handler http.Handler) http.Handler {
	return trackCompleted(handler, clock.RealClock{}, func(ctx context.Context, fr *requestFilterRecord, completedAt time.Time) {
		latency := completedAt.Sub(fr.startedTimestamp)
		metrics.RecordFilterLatency(ctx, fr.name, latency)
		if klog.V(3).Enabled() && latency > minFilterLatencyToLog {
			httplog.AddKeyValue(ctx, fmt.Sprintf("fl_%s", fr.name), latency.String())
		}
	})
}

func trackStarted(handler http.Handler, tp trace.TracerProvider, name string, clock clock.PassiveClock) http.Handler {
	// This is a noop if the tracing is disabled, since tp will be a NoopTracerProvider
	tracer := tp.Tracer("k8s.op/apiserver/pkg/endpoints/filterlatency")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), name)
		defer span.End()
		if fr := requestFilterRecordFrom(ctx); fr != nil {
			fr.name = name
			fr.startedTimestamp = clock.Now()
			r = r.WithContext(ctx)
		} else {
			fr := &requestFilterRecord{
				name:             name,
				startedTimestamp: clock.Now(),
			}
			r = r.WithContext(withRequestFilterRecord(ctx, fr))
		}

		handler.ServeHTTP(w, r)
	})
}

func trackCompleted(handler http.Handler, clock clock.PassiveClock, action func(context.Context, *requestFilterRecord, time.Time)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The previous filter has just completed.
		completedAt := clock.Now()

		defer handler.ServeHTTP(w, r)

		ctx := r.Context()
		if fr := requestFilterRecordFrom(ctx); fr != nil {
			action(ctx, fr, completedAt)
		}
	})
}
