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

package request

import (
	"context"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

func sumDuration(d1 time.Duration, d2 time.Duration) time.Duration {
	return d1 + d2
}

func maxDuration(d1 time.Duration, d2 time.Duration) time.Duration {
	if d1 > d2 {
		return d1
	}
	return d2
}

// DurationTracker is a simple interface for tracking functions duration,
// it is safe for concurrent use by multiple goroutines.
type DurationTracker interface {
	// Track measures time spent in the given function f and
	// aggregates measured duration using aggregateFunction.
	// if Track is invoked with f from multiple goroutines concurrently,
	// then f must be safe to be invoked concurrently by multiple goroutines.
	Track(f func())

	// TrackDuration tracks latency from the specified duration
	// and aggregate it using aggregateFunction
	TrackDuration(time.Duration)

	// GetLatency returns the total latency incurred so far
	GetLatency() time.Duration
}

// durationTracker implements DurationTracker by measuring function time
// using given clock and aggregates the duration using given aggregate function
type durationTracker struct {
	clock             clock.Clock
	latency           time.Duration
	mu                sync.Mutex
	aggregateFunction func(time.Duration, time.Duration) time.Duration
}

// Track measures time spent in given function and aggregates measured
// duration using aggregateFunction
func (t *durationTracker) Track(f func()) {
	startedAt := t.clock.Now()
	defer func() {
		duration := t.clock.Since(startedAt)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.latency = t.aggregateFunction(t.latency, duration)
	}()

	f()
}

// TrackDuration tracks latency from the given duration
// using aggregateFunction
func (t *durationTracker) TrackDuration(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latency = t.aggregateFunction(t.latency, d)
}

// GetLatency returns aggregated latency tracked by a tracker
func (t *durationTracker) GetLatency() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.latency
}

func newSumLatencyTracker(c clock.Clock) DurationTracker {
	return &durationTracker{
		clock:             c,
		aggregateFunction: sumDuration,
	}
}

func newMaxLatencyTracker(c clock.Clock) DurationTracker {
	return &durationTracker{
		clock:             c,
		aggregateFunction: maxDuration,
	}
}

// LatencyTrackers stores trackers used to measure latency incurred in
// components within the apiserver.
type LatencyTrackers struct {
	// MutatingWebhookTracker tracks the latency incurred in mutating webhook(s).
	// Since mutating webhooks are done sequentially, latency
	// is aggregated using sum function.
	MutatingWebhookTracker DurationTracker

	// ValidatingWebhookTracker tracks the latency incurred in validating webhook(s).
	// Validate webhooks are done in parallel, so max function is used.
	ValidatingWebhookTracker DurationTracker

	// APFQueueWaitTracker tracks the latency incurred by queue wait times
	// from priority & fairness.
	APFQueueWaitTracker DurationTracker

	// StorageTracker tracks the latency incurred inside the storage layer,
	// it accounts for the time it takes to send data to the underlying
	// storage layer (etcd) and get the complete response back.
	// If a request involves N (N>=1) round trips to the underlying
	// storage layer, the latency will account for the total duration
	// from these N round trips.
	// It does not include the time incurred in admission, or validation.
	StorageTracker DurationTracker

	// TransformTracker tracks the latency incurred in transforming the
	// response object(s) returned from the underlying storage layer.
	// This includes transforming the object to user's desired form
	// (ie. as Table), and also setting appropriate API level fields.
	// This does not include the latency incurred in serialization
	// (json or protobuf) of the response object or writing
	// of it to the http ResponseWriter object.
	TransformTracker DurationTracker

	// SerializationTracker tracks the latency incurred in serialization
	// (json or protobuf) of the response object.
	// NOTE: serialization and writing of the serialized raw bytes to the
	// associated http ResponseWriter object are interleaved, and hence
	// the latency measured here will include the time spent writing the
	// serialized raw bytes to the http ResponseWriter object.
	SerializationTracker DurationTracker

	// ResponseWriteTracker tracks the latency incurred in writing the
	// serialized raw bytes to the http ResponseWriter object (via the
	// Write method) associated with the request.
	// The Write method can be invoked multiple times, so we use a
	// latency tracker that sums up the duration from each call.
	ResponseWriteTracker DurationTracker

	// DecodeTracker is used to track latency incurred inside the function
	// that takes an object returned from the underlying storage layer
	// (etcd) and performs decoding of the response object.
	// When called multiple times, the latency incurred inside to
	// decode func each time will be summed up.
	DecodeTracker DurationTracker
}

type latencyTrackersKeyType int

// latencyTrackersKey is the key that associates a LatencyTrackers
// instance with the request context.
const latencyTrackersKey latencyTrackersKeyType = iota

// WithLatencyTrackers returns a copy of parent context to which an
// instance of LatencyTrackers is added.
func WithLatencyTrackers(parent context.Context) context.Context {
	return WithLatencyTrackersAndCustomClock(parent, clock.RealClock{})
}

// WithLatencyTrackersAndCustomClock returns a copy of parent context to which
// an instance of LatencyTrackers is added. Tracers use given clock.
func WithLatencyTrackersAndCustomClock(parent context.Context, c clock.Clock) context.Context {
	return WithValue(parent, latencyTrackersKey, &LatencyTrackers{
		MutatingWebhookTracker:   newSumLatencyTracker(c),
		ValidatingWebhookTracker: newMaxLatencyTracker(c),
		APFQueueWaitTracker:      newMaxLatencyTracker(c),
		StorageTracker:           newSumLatencyTracker(c),
		TransformTracker:         newSumLatencyTracker(c),
		SerializationTracker:     newSumLatencyTracker(c),
		ResponseWriteTracker:     newSumLatencyTracker(c),
		DecodeTracker:            newSumLatencyTracker(c),
	})
}

// LatencyTrackersFrom returns the associated LatencyTrackers instance
// from the specified context.
func LatencyTrackersFrom(ctx context.Context) (*LatencyTrackers, bool) {
	wd, ok := ctx.Value(latencyTrackersKey).(*LatencyTrackers)
	return wd, ok && wd != nil
}

// TrackTransformResponseObjectLatency is used to track latency incurred
// inside the function that takes an object returned from the underlying
// storage layer (etcd) and performs any necessary transformations
// of the response object. This does not include the latency incurred in
// serialization (json or protobuf) of the response object or writing of
// it to the http ResponseWriter object.
// When called multiple times, the latency incurred inside the
// transform func each time will be summed up.
func TrackTransformResponseObjectLatency(ctx context.Context, transform func()) {
	if tracker, ok := LatencyTrackersFrom(ctx); ok {
		tracker.TransformTracker.Track(transform)
		return
	}

	transform()
}

// TrackStorageLatency is used to track latency incurred
// inside the underlying storage layer.
// When called multiple times, the latency provided will be summed up.
func TrackStorageLatency(ctx context.Context, d time.Duration) {
	if tracker, ok := LatencyTrackersFrom(ctx); ok {
		tracker.StorageTracker.TrackDuration(d)
	}
}

// TrackSerializeResponseObjectLatency is used to track latency incurred in
// serialization (json or protobuf) of the response object.
// When called multiple times, the latency provided will be summed up.
func TrackSerializeResponseObjectLatency(ctx context.Context, f func()) {
	if tracker, ok := LatencyTrackersFrom(ctx); ok {
		tracker.SerializationTracker.Track(f)
		return
	}

	f()
}

// TrackResponseWriteLatency is used to track latency incurred in writing
// the serialized raw bytes to the http ResponseWriter object (via the
// Write method) associated with the request.
// When called multiple times, the latency provided will be summed up.
func TrackResponseWriteLatency(ctx context.Context, d time.Duration) {
	if tracker, ok := LatencyTrackersFrom(ctx); ok {
		tracker.ResponseWriteTracker.TrackDuration(d)
	}
}

// TrackAPFQueueWaitLatency is used to track latency incurred
// by priority and fairness queues.
func TrackAPFQueueWaitLatency(ctx context.Context, d time.Duration) {
	if tracker, ok := LatencyTrackersFrom(ctx); ok {
		tracker.APFQueueWaitTracker.TrackDuration(d)
	}
}

// TrackDecodeLatency is used to track latency incurred inside the function
// that takes an object returned from the underlying storage layer
// (etcd) and performs decoding of the response object.
// When called multiple times, the latency incurred inside to
// decode func each time will be summed up.
func TrackDecodeLatency(ctx context.Context, d time.Duration) {
	if tracker, ok := LatencyTrackersFrom(ctx); ok {
		tracker.DecodeTracker.TrackDuration(d)
	}
}

// AuditAnnotationsFromLatencyTrackers will inspect each latency tracker
// associated with the request context and return a set of audit
// annotations that can be added to the API audit entry.
func AuditAnnotationsFromLatencyTrackers(ctx context.Context) map[string]string {
	const (
		transformLatencyKey         = "apiserver.latency.k8s.io/transform-response-object"
		storageLatencyKey           = "apiserver.latency.k8s.io/etcd"
		serializationLatencyKey     = "apiserver.latency.k8s.io/serialize-response-object"
		responseWriteLatencyKey     = "apiserver.latency.k8s.io/response-write"
		mutatingWebhookLatencyKey   = "apiserver.latency.k8s.io/mutating-webhook"
		validatingWebhookLatencyKey = "apiserver.latency.k8s.io/validating-webhook"
		decodeLatencyKey            = "apiserver.latency.k8s.io/decode-response-object"
		apfQueueWaitLatencyKey      = "apiserver.latency.k8s.io/apf-queue-wait"
	)

	tracker, ok := LatencyTrackersFrom(ctx)
	if !ok {
		return nil
	}

	annotations := map[string]string{}
	if latency := tracker.TransformTracker.GetLatency(); latency != 0 {
		annotations[transformLatencyKey] = latency.String()
	}
	if latency := tracker.StorageTracker.GetLatency(); latency != 0 {
		annotations[storageLatencyKey] = latency.String()
	}
	if latency := tracker.SerializationTracker.GetLatency(); latency != 0 {
		annotations[serializationLatencyKey] = latency.String()
	}
	if latency := tracker.ResponseWriteTracker.GetLatency(); latency != 0 {
		annotations[responseWriteLatencyKey] = latency.String()
	}
	if latency := tracker.MutatingWebhookTracker.GetLatency(); latency != 0 {
		annotations[mutatingWebhookLatencyKey] = latency.String()
	}
	if latency := tracker.ValidatingWebhookTracker.GetLatency(); latency != 0 {
		annotations[validatingWebhookLatencyKey] = latency.String()
	}
	if latency := tracker.DecodeTracker.GetLatency(); latency != 0 {
		annotations[decodeLatencyKey] = latency.String()
	}
	if latency := tracker.APFQueueWaitTracker.GetLatency(); latency != 0 {
		annotations[apfQueueWaitLatencyKey] = latency.String()
	}
	return annotations
}
