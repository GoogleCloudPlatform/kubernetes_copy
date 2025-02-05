/*
Copyright 2015 The Kubernetes Authors.

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

package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilsets "k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	compbasemetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// resettableCollector is the interface implemented by prometheus.MetricVec
// that can be used by Prometheus to collect metrics and reset their values.
type resettableCollector interface {
	compbasemetrics.Registerable
	Reset()
}

const (
	APIServerComponent string = "apiserver"
	OtherRequestMethod string = "other"
)

/*
 * By default, all the following metrics are defined as falling under
 * ALPHA stability level https://github.com/kubernetes/enhancements/blob/master/keps/sig-instrumentation/1209-metrics-stability/kubernetes-control-plane-metrics-stability.md#stability-classes)
 *
 * Promoting the stability level of the metric is a responsibility of the component owner, since it
 * involves explicitly acknowledging support for the metric across multiple releases, in accordance with
 * the metric stability policy.
 */
var (
	deprecatedRequestGauge = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Subsystem:      APIServerComponent,
			Name:           "requested_deprecated_apis",
			Help:           "Gauge of deprecated APIs that have been requested, broken out by API group, version, resource, subresource, and removed_release.",
			StabilityLevel: compbasemetrics.STABLE,
		},
		[]string{"group", "version", "resource", "subresource", "removed_release"},
	)

	// TODO(a-robinson): Add unit tests for the handling of these metrics once
	// the upstream library supports it.
	requestCounter = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Subsystem:      APIServerComponent,
			Name:           "request_total",
			Help:           "Counter of apiserver requests broken out for each verb, dry run value, group, version, resource, scope, component, and HTTP response code.",
			StabilityLevel: compbasemetrics.STABLE,
		},
		[]string{"verb", "dry_run", "group", "version", "resource", "subresource", "scope", "component", "code"},
	)
	longRunningRequestsGauge = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Subsystem:      APIServerComponent,
			Name:           "longrunning_requests",
			Help:           "Gauge of all active long-running apiserver requests broken out by verb, group, version, resource, scope and component. Not all requests are tracked this way.",
			StabilityLevel: compbasemetrics.STABLE,
		},
		[]string{"verb", "group", "version", "resource", "subresource", "scope", "component"},
	)
	requestLatencies = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: APIServerComponent,
			Name:      "request_duration_seconds",
			Help:      "Response latency distribution in seconds for each verb, dry run value, group, version, resource, subresource, scope and component.",
			// This metric is used for verifying api call latencies SLO,
			// as well as tracking regressions in this aspects.
			// Thus we customize buckets significantly, to empower both usecases.
			Buckets: []float64{0.005, 0.025, 0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
				4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
			StabilityLevel: compbasemetrics.STABLE,
		},
		[]string{"verb", "dry_run", "group", "version", "resource", "subresource", "scope", "component"},
	)
	requestSloLatencies = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: APIServerComponent,
			Name:      "request_slo_duration_seconds",
			Help:      "Response latency distribution (not counting webhook duration and priority & fairness queue wait times) in seconds for each verb, group, version, resource, subresource, scope and component.",
			// This metric is supplementary to the requestLatencies metric.
			// It measures request duration excluding webhooks as they are mostly
			// dependant on user configuration.
			Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
				4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
			StabilityLevel:    compbasemetrics.ALPHA,
			DeprecatedVersion: "1.27.0",
		},
		[]string{"verb", "group", "version", "resource", "subresource", "scope", "component"},
	)
	requestSliLatencies = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: APIServerComponent,
			Name:      "request_sli_duration_seconds",
			Help:      "Response latency distribution (not counting webhook duration and priority & fairness queue wait times) in seconds for each verb, group, version, resource, subresource, scope and component.",
			// This metric is supplementary to the requestLatencies metric.
			// It measures request duration excluding webhooks as they are mostly
			// dependant on user configuration.
			Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
				4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"verb", "group", "version", "resource", "subresource", "scope", "component"},
	)
	// Deprecated: fieldValidationRequestLatenciesDeprecated is deprecated,
	// and will be removed at v1.34. Please use fieldValidationRequestLatencies instead.
	fieldValidationRequestLatenciesDeprecated = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Name: "field_validation_request_duration_seconds",
			Help: "Response latency distribution in seconds for each field validation value",
			// This metric is supplementary to the requestLatencies metric.
			// It measures request durations for the various field validation
			// values.
			Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
				4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
			StabilityLevel:    compbasemetrics.ALPHA,
			DeprecatedVersion: "1.33.0",
		},
		[]string{"field_validation"},
	)
	fieldValidationRequestLatencies = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: APIServerComponent,
			Name:      "field_validation_request_duration_seconds",
			Help:      "Response latency distribution in seconds for each field validation value",
			// This metric is supplementary to the requestLatencies metric.
			// It measures request durations for the various field validation
			// values.
			Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
				4, 5, 6, 8, 10, 15, 20, 30, 45, 60},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"field_validation"},
	)
	responseSizes = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: APIServerComponent,
			Name:      "response_sizes",
			Help:      "Response size distribution in bytes for each group, version, verb, resource, subresource, scope and component.",
			// Use buckets ranging from 1000 bytes (1KB) to 10^9 bytes (1GB).
			Buckets:        compbasemetrics.ExponentialBuckets(1000, 10.0, 7),
			StabilityLevel: compbasemetrics.STABLE,
		},
		[]string{"verb", "group", "version", "resource", "subresource", "scope", "component"},
	)
	// TLSHandshakeErrors is a number of requests dropped with 'TLS handshake error from' error
	TLSHandshakeErrors = compbasemetrics.NewCounter(
		&compbasemetrics.CounterOpts{
			Subsystem:      APIServerComponent,
			Name:           "tls_handshake_errors_total",
			Help:           "Number of requests dropped with 'TLS handshake error from' error",
			StabilityLevel: compbasemetrics.ALPHA,
		},
	)
	WatchEvents = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Subsystem:      APIServerComponent,
			Name:           "watch_events_total",
			Help:           "Number of events sent in watch clients",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"group", "version", "kind"},
	)
	WatchEventsSizes = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem:      APIServerComponent,
			Name:           "watch_events_sizes",
			Help:           "Watch event size distribution in bytes",
			Buckets:        compbasemetrics.ExponentialBuckets(1024, 2.0, 8), // 1K, 2K, 4K, 8K, ..., 128K.
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"group", "version", "kind"},
	)
	// Because of volatility of the base metric this is pre-aggregated one. Instead of reporting current usage all the time
	// it reports maximal usage during the last second.
	currentInflightRequests = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Subsystem:      APIServerComponent,
			Name:           "current_inflight_requests",
			Help:           "Maximal number of currently used inflight request limit of this apiserver per request kind in last second.",
			StabilityLevel: compbasemetrics.STABLE,
		},
		[]string{"request_kind"},
	)
	currentInqueueRequests = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Subsystem:      APIServerComponent,
			Name:           "current_inqueue_requests",
			Help:           "Maximal number of queued requests in this apiserver per request kind in last second.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"request_kind"},
	)

	requestTerminationsTotal = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Subsystem:      APIServerComponent,
			Name:           "request_terminations_total",
			Help:           "Number of requests which apiserver terminated in self-defense.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"verb", "group", "version", "resource", "subresource", "scope", "component", "code"},
	)

	apiSelfRequestCounter = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Subsystem:      APIServerComponent,
			Name:           "selfrequest_total",
			Help:           "Counter of apiserver self-requests broken out for each verb, API resource and subresource.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"verb", "resource", "subresource"},
	)

	requestFilterDuration = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem:      APIServerComponent,
			Name:           "request_filter_duration_seconds",
			Help:           "Request filter latency distribution in seconds, for each filter type",
			Buckets:        []float64{0.0001, 0.0003, 0.001, 0.003, 0.01, 0.03, 0.1, 0.3, 1.0, 5.0, 10.0, 15.0, 30.0},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"filter"},
	)

	// requestAbortsTotal is a number of aborted requests with http.ErrAbortHandler
	requestAbortsTotal = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Subsystem:      APIServerComponent,
			Name:           "request_aborts_total",
			Help:           "Number of requests which apiserver aborted possibly due to a timeout, for each group, version, verb, resource, subresource and scope",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"verb", "group", "version", "resource", "subresource", "scope"},
	)

	// requestPostTimeoutTotal tracks the activity of the executing request handler after the associated request
	// has been timed out by the apiserver.
	// source: the name of the handler that is recording this metric. Currently, we have two:
	//  - timeout-handler: the "executing" handler returns after the timeout filter times out the request.
	//  - rest-handler: the "executing" handler returns after the rest layer times out the request.
	// status: whether the handler panicked or threw an error, possible values:
	//  - 'panic': the handler panicked
	//  - 'error': the handler return an error
	//  - 'ok': the handler returned a result (no error and no panic)
	//  - 'pending': the handler is still running in the background and it did not return
	//    within the wait threshold.
	requestPostTimeoutTotal = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Subsystem:      APIServerComponent,
			Name:           "request_post_timeout_total",
			Help:           "Tracks the activity of the request handlers after the associated requests have been timed out by the apiserver",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"source", "status"},
	)

	requestTimestampComparisonDuration = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem:      APIServerComponent,
			Name:           "request_timestamp_comparison_time",
			Help:           "Time taken for comparison of old vs new objects in UPDATE or PATCH requests",
			Buckets:        []float64{0.0001, 0.0003, 0.001, 0.003, 0.01, 0.03, 0.1, 0.3, 1.0, 5.0},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		// Path the code takes to reach a conclusion:
		// i.e. unequalObjectsFast, unequalObjectsSlow, equalObjectsSlow
		[]string{"code_path"},
	)

	watchListLatencies = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem:      APIServerComponent,
			Name:           "watch_list_duration_seconds",
			Help:           "Response latency distribution in seconds for watch list requests broken by group, version, resource and scope.",
			Buckets:        []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 2, 4, 6, 8, 10, 15, 20, 30, 45, 60},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"group", "version", "resource", "scope"},
	)

	metrics = []resettableCollector{
		deprecatedRequestGauge,
		requestCounter,
		longRunningRequestsGauge,
		requestLatencies,
		requestSloLatencies,
		requestSliLatencies,
		fieldValidationRequestLatencies,
		fieldValidationRequestLatenciesDeprecated,
		responseSizes,
		TLSHandshakeErrors,
		WatchEvents,
		WatchEventsSizes,
		currentInflightRequests,
		currentInqueueRequests,
		requestTerminationsTotal,
		apiSelfRequestCounter,
		requestFilterDuration,
		requestAbortsTotal,
		requestPostTimeoutTotal,
		requestTimestampComparisonDuration,
		watchListLatencies,
	}

	// these are the valid request methods which we report in our metrics. Any other request methods
	// will be aggregated under 'unknown'
	validRequestMethods = utilsets.NewString(
		"APPLY",
		"CONNECT",
		"CREATE",
		"DELETE",
		"DELETECOLLECTION",
		"GET",
		"LIST",
		"PATCH",
		"POST",
		"PROXY",
		"PUT",
		"UPDATE",
		"WATCH",
		"WATCHLIST")

	// These are the valid connect requests which we report in our metrics.
	validConnectRequests = utilsets.NewString(
		"log",
		"exec",
		"portforward",
		"attach",
		"proxy")
)

const (
	// ReadOnlyKind is a string identifying read only request kind
	ReadOnlyKind = "readOnly"
	// MutatingKind is a string identifying mutating request kind
	MutatingKind = "mutating"

	// WaitingPhase is the phase value for a request waiting in a queue
	WaitingPhase = "waiting"
	// ExecutingPhase is the phase value for an executing request
	ExecutingPhase = "executing"
)

const (
	// deprecatedAnnotationKey is a key for an audit annotation set to
	// "true" on requests made to deprecated API versions
	deprecatedAnnotationKey = "k8s.io/deprecated"
	// removedReleaseAnnotationKey is a key for an audit annotation set to
	// the target removal release, in "<major>.<minor>" format,
	// on requests made to deprecated API versions with a target removal release
	removedReleaseAnnotationKey = "k8s.io/removed-release"
)

const (
	// The source that is recording the apiserver_request_post_timeout_total metric.
	// The "executing" request handler returns after the timeout filter times out the request.
	PostTimeoutSourceTimeoutHandler = "timeout-handler"

	// The source that is recording the apiserver_request_post_timeout_total metric.
	// The "executing" request handler returns after the rest layer times out the request.
	PostTimeoutSourceRestHandler = "rest-handler"
)

const (
	// The executing request handler panicked after the request had
	// been timed out by the apiserver.
	PostTimeoutHandlerPanic = "panic"

	// The executing request handler has returned an error to the post-timeout
	// receiver after the request had been timed out by the apiserver.
	PostTimeoutHandlerError = "error"

	// The executing request handler has returned a result to the post-timeout
	// receiver after the request had been timed out by the apiserver.
	PostTimeoutHandlerOK = "ok"

	// The executing request handler has not panicked or returned any error/result to
	// the post-timeout receiver yet after the request had been timed out by the apiserver.
	// The post-timeout receiver gives up after waiting for certain threshold and if the
	// executing request handler has not returned yet we use the following label.
	PostTimeoutHandlerPending = "pending"
)

var registerMetrics sync.Once

// Register all metrics.
func Register() {
	registerMetrics.Do(func() {
		for _, metric := range metrics {
			legacyregistry.MustRegister(metric)
		}
	})
}

// Reset all metrics.
func Reset() {
	for _, metric := range metrics {
		metric.Reset()
	}
}

// ResetLabelAllowLists resets the label allow lists for all metrics.
// NOTE: This is only used for testing.
func ResetLabelAllowLists() {
	for _, metric := range metrics {
		if counterVec, ok := metric.(*compbasemetrics.CounterVec); ok {
			counterVec.ResetLabelAllowLists()
			continue
		}
		if gaugeVec, ok := metric.(*compbasemetrics.GaugeVec); ok {
			gaugeVec.ResetLabelAllowLists()
			continue
		}
		if histogramVec, ok := metric.(*compbasemetrics.HistogramVec); ok {
			histogramVec.ResetLabelAllowLists()
			continue
		}
		if summaryVec, ok := metric.(*compbasemetrics.SummaryVec); ok {
			summaryVec.ResetLabelAllowLists()
			continue
		}
		if timingHistogramVec, ok := metric.(*compbasemetrics.TimingHistogramVec); ok {
			timingHistogramVec.ResetLabelAllowLists()
			continue
		}
	}
}

// UpdateInflightRequestMetrics reports concurrency metrics classified by
// mutating vs Readonly.
func UpdateInflightRequestMetrics(phase string, nonmutating, mutating int) {
	for _, kc := range []struct {
		kind  string
		count int
	}{{ReadOnlyKind, nonmutating}, {MutatingKind, mutating}} {
		if phase == ExecutingPhase {
			currentInflightRequests.WithLabelValues(kc.kind).Set(float64(kc.count))
		} else {
			currentInqueueRequests.WithLabelValues(kc.kind).Set(float64(kc.count))
		}
	}
}

func RecordFilterLatency(ctx context.Context, name string, elapsed time.Duration) {
	requestFilterDuration.WithContext(ctx).WithLabelValues(name).Observe(elapsed.Seconds())
}

func RecordTimestampComparisonLatency(codePath string, elapsed time.Duration) {
	requestTimestampComparisonDuration.WithLabelValues(codePath).Observe(elapsed.Seconds())
}

func RecordRequestPostTimeout(source string, status string) {
	requestPostTimeoutTotal.WithLabelValues(source, status).Inc()
}

// RecordRequestAbort records that the request was aborted possibly due to a timeout.
func RecordRequestAbort(req *http.Request, requestInfo *request.RequestInfo) {
	if requestInfo == nil {
		requestInfo = &request.RequestInfo{Verb: req.Method, Path: req.URL.Path}
	}

	scope := CleanScope(requestInfo)
	reportedVerb := cleanVerb(CanonicalVerb(strings.ToUpper(req.Method), scope), "", req, requestInfo)
	resource := requestInfo.Resource
	subresource := requestInfo.Subresource
	group := requestInfo.APIGroup
	version := requestInfo.APIVersion

	requestAbortsTotal.WithContext(req.Context()).WithLabelValues(reportedVerb, group, version, resource, subresource, scope).Inc()
}

// RecordDroppedRequest records that the request was rejected via http.TooManyRequests.
func RecordDroppedRequest(req *http.Request, requestInfo *request.RequestInfo, component string, isMutatingRequest bool) {
	if requestInfo == nil {
		requestInfo = &request.RequestInfo{Verb: req.Method, Path: req.URL.Path}
	}
	scope := CleanScope(requestInfo)
	dryRun := cleanDryRun(req.URL)

	// We don't use verb from <requestInfo>, as this may be propagated from
	// InstrumentRouteFunc which is registered in installer.go with predefined
	// list of verbs (different than those translated to RequestInfo).
	// However, we need to tweak it e.g. to differentiate GET from LIST.
	reportedVerb := cleanVerb(CanonicalVerb(strings.ToUpper(req.Method), scope), "", req, requestInfo)

	if requestInfo.IsResourceRequest {
		requestCounter.WithContext(req.Context()).WithLabelValues(reportedVerb, dryRun, requestInfo.APIGroup, requestInfo.APIVersion, requestInfo.Resource, requestInfo.Subresource, scope, component, codeToString(http.StatusTooManyRequests)).Inc()
	} else {
		requestCounter.WithContext(req.Context()).WithLabelValues(reportedVerb, dryRun, "", "", "", requestInfo.Subresource, scope, component, codeToString(http.StatusTooManyRequests)).Inc()
	}
}

// RecordRequestTermination records that the request was terminated early as part of a resource
// preservation or apiserver self-defense mechanism (e.g. timeouts, maxinflight throttling,
// proxyHandler errors). RecordRequestTermination should only be called zero or one times
// per request.
func RecordRequestTermination(req *http.Request, requestInfo *request.RequestInfo, component string, code int) {
	if requestInfo == nil {
		requestInfo = &request.RequestInfo{Verb: req.Method, Path: req.URL.Path}
	}
	scope := CleanScope(requestInfo)

	// We don't use verb from <requestInfo>, as this may be propagated from
	// InstrumentRouteFunc which is registered in installer.go with predefined
	// list of verbs (different than those translated to RequestInfo).
	// However, we need to tweak it e.g. to differentiate GET from LIST.
	reportedVerb := cleanVerb(CanonicalVerb(strings.ToUpper(req.Method), scope), "", req, requestInfo)

	if requestInfo.IsResourceRequest {
		requestTerminationsTotal.WithContext(req.Context()).WithLabelValues(reportedVerb, requestInfo.APIGroup, requestInfo.APIVersion, requestInfo.Resource, requestInfo.Subresource, scope, component, codeToString(code)).Inc()
	} else {
		requestTerminationsTotal.WithContext(req.Context()).WithLabelValues(reportedVerb, "", "", "", requestInfo.Path, scope, component, codeToString(code)).Inc()
	}
}

// RecordLongRunning tracks the execution of a long running request against the API server. It provides an accurate count
// of the total number of open long running requests. requestInfo may be nil if the caller is not in the normal request flow.
func RecordLongRunning(req *http.Request, requestInfo *request.RequestInfo, component string, fn func()) {
	if requestInfo == nil {
		requestInfo = &request.RequestInfo{Verb: req.Method, Path: req.URL.Path}
	}
	var g compbasemetrics.GaugeMetric
	scope := CleanScope(requestInfo)

	// We don't use verb from <requestInfo>, as this may be propagated from
	// InstrumentRouteFunc which is registered in installer.go with predefined
	// list of verbs (different than those translated to RequestInfo).
	// However, we need to tweak it e.g. to differentiate GET from LIST.
	reportedVerb := cleanVerb(CanonicalVerb(strings.ToUpper(req.Method), scope), "", req, requestInfo)

	if requestInfo.IsResourceRequest {
		g = longRunningRequestsGauge.WithContext(req.Context()).WithLabelValues(reportedVerb, requestInfo.APIGroup, requestInfo.APIVersion, requestInfo.Resource, requestInfo.Subresource, scope, component)
	} else {
		g = longRunningRequestsGauge.WithContext(req.Context()).WithLabelValues(reportedVerb, "", "", "", requestInfo.Path, scope, component)
	}
	g.Inc()
	defer g.Dec()
	fn()
}

// RecordWatchListLatency simply records response latency for watch list requests.
func RecordWatchListLatency(ctx context.Context, gvr schema.GroupVersionResource, metricsScope string) {
	requestReceivedTimestamp, ok := request.ReceivedTimestampFrom(ctx)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unable to measure watchlist latency because no received ts found in the ctx, gvr: %s", gvr))
		return
	}
	elapsedSeconds := time.Since(requestReceivedTimestamp).Seconds()

	watchListLatencies.WithContext(ctx).WithLabelValues(gvr.Group, gvr.Version, gvr.Resource, metricsScope).Observe(elapsedSeconds)
}

// MonitorRequest handles standard transformations for client and the reported verb and then invokes Monitor to record
// a request. verb must be uppercase to be backwards compatible with existing monitoring tooling.
func MonitorRequest(req *http.Request, verb, group, version, resource, subresource, scope, component string, deprecated bool, removedRelease string, httpCode, respSize int, elapsed time.Duration) {
	requestInfo, ok := request.RequestInfoFrom(req.Context())
	if !ok || requestInfo == nil {
		requestInfo = &request.RequestInfo{Verb: req.Method, Path: req.URL.Path}
	}
	// We don't use verb from <requestInfo>, as this may be propagated from
	// InstrumentRouteFunc which is registered in installer.go with predefined
	// list of verbs (different than those translated to RequestInfo).
	// However, we need to tweak it e.g. to differentiate GET from LIST.
	reportedVerb := cleanVerb(CanonicalVerb(strings.ToUpper(req.Method), scope), verb, req, requestInfo)

	dryRun := cleanDryRun(req.URL)
	elapsedSeconds := elapsed.Seconds()
	requestCounter.WithContext(req.Context()).WithLabelValues(reportedVerb, dryRun, group, version, resource, subresource, scope, component, codeToString(httpCode)).Inc()
	// MonitorRequest happens after authentication, so we can trust the username given by the request
	info, ok := request.UserFrom(req.Context())
	if ok && info.GetName() == user.APIServerUser {
		apiSelfRequestCounter.WithContext(req.Context()).WithLabelValues(reportedVerb, resource, subresource).Inc()
	}
	if deprecated {
		deprecatedRequestGauge.WithContext(req.Context()).WithLabelValues(group, version, resource, subresource, removedRelease).Set(1)
		audit.AddAuditAnnotation(req.Context(), deprecatedAnnotationKey, "true")
		if len(removedRelease) > 0 {
			audit.AddAuditAnnotation(req.Context(), removedReleaseAnnotationKey, removedRelease)
		}
	}
	requestLatencies.WithContext(req.Context()).WithLabelValues(reportedVerb, dryRun, group, version, resource, subresource, scope, component).Observe(elapsedSeconds)
	fieldValidation := cleanFieldValidation(req.URL)
	fieldValidationRequestLatencies.WithContext(req.Context()).WithLabelValues(fieldValidation)
	// we intentionally keep them with the deprecation and will remove at v1.34.
	//nolint:staticcheck
	fieldValidationRequestLatenciesDeprecated.WithContext(req.Context()).WithLabelValues(fieldValidation)

	if wd, ok := request.LatencyTrackersFrom(req.Context()); ok {
		sliLatency := elapsedSeconds - (wd.MutatingWebhookTracker.GetLatency() + wd.ValidatingWebhookTracker.GetLatency() + wd.APFQueueWaitTracker.GetLatency()).Seconds()
		requestSloLatencies.WithContext(req.Context()).WithLabelValues(reportedVerb, group, version, resource, subresource, scope, component).Observe(sliLatency)
		requestSliLatencies.WithContext(req.Context()).WithLabelValues(reportedVerb, group, version, resource, subresource, scope, component).Observe(sliLatency)
	}
	// We are only interested in response sizes of read requests.
	if verb == "GET" || verb == "LIST" {
		responseSizes.WithContext(req.Context()).WithLabelValues(reportedVerb, group, version, resource, subresource, scope, component).Observe(float64(respSize))
	}
}

// InstrumentRouteFunc works like Prometheus' InstrumentHandlerFunc but wraps
// the go-restful RouteFunction instead of a HandlerFunc plus some Kubernetes endpoint specific information.
func InstrumentRouteFunc(verb, group, version, resource, subresource, scope, component string, deprecated bool, removedRelease string, routeFunc restful.RouteFunction) restful.RouteFunction {
	return restful.RouteFunction(func(req *restful.Request, response *restful.Response) {
		requestReceivedTimestamp, ok := request.ReceivedTimestampFrom(req.Request.Context())
		if !ok {
			requestReceivedTimestamp = time.Now()
		}

		delegate := &ResponseWriterDelegator{ResponseWriter: response.ResponseWriter}

		rw := responsewriter.WrapForHTTP1Or2(delegate)
		response.ResponseWriter = rw

		routeFunc(req, response)

		MonitorRequest(req.Request, verb, group, version, resource, subresource, scope, component, deprecated, removedRelease, delegate.Status(), delegate.ContentLength(), time.Since(requestReceivedTimestamp))
	})
}

// InstrumentHandlerFunc works like Prometheus' InstrumentHandlerFunc but adds some Kubernetes endpoint specific information.
func InstrumentHandlerFunc(verb, group, version, resource, subresource, scope, component string, deprecated bool, removedRelease string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		requestReceivedTimestamp, ok := request.ReceivedTimestampFrom(req.Context())
		if !ok {
			requestReceivedTimestamp = time.Now()
		}

		delegate := &ResponseWriterDelegator{ResponseWriter: w}
		w = responsewriter.WrapForHTTP1Or2(delegate)

		handler(w, req)

		MonitorRequest(req, verb, group, version, resource, subresource, scope, component, deprecated, removedRelease, delegate.Status(), delegate.ContentLength(), time.Since(requestReceivedTimestamp))
	}
}

// NormalizedVerb returns normalized verb
func NormalizedVerb(req *http.Request) string {
	verb := req.Method
	requestInfo, ok := request.RequestInfoFrom(req.Context())
	if ok {
		// If we can find a requestInfo, we can get a scope, and then
		// we can convert GETs to LISTs when needed.
		scope := CleanScope(requestInfo)
		verb = CanonicalVerb(strings.ToUpper(verb), scope)
	}

	// mark APPLY requests, WATCH requests and CONNECT requests correctly.
	return CleanVerb(verb, req, requestInfo)
}

// CleanScope returns the scope of the request.
func CleanScope(requestInfo *request.RequestInfo) string {
	if requestInfo.Name != "" || requestInfo.Verb == "create" {
		return "resource"
	}
	if requestInfo.Namespace != "" {
		return "namespace"
	}
	if requestInfo.IsResourceRequest {
		return "cluster"
	}
	// this is the empty scope
	return ""
}

// CleanListScope computes the request scope for metrics.
//
// Note that normally we would use CleanScope for computation.
// But due to the same reasons mentioned in determineRequestNamespaceAndName we cannot.
func CleanListScope(ctx context.Context, opts *metainternalversion.ListOptions) string {
	namespace, name := determineRequestNamespaceAndName(ctx, opts)
	if len(name) > 0 {
		return "resource"
	}
	if len(namespace) > 0 {
		return "namespace"
	}
	if requestInfo, ok := request.RequestInfoFrom(ctx); ok {
		if requestInfo.IsResourceRequest {
			return "cluster"
		}
	}
	return ""
}

// CanonicalVerb distinguishes LISTs from GETs (and HEADs). It assumes verb is
// UPPERCASE.
func CanonicalVerb(verb string, scope string) string {
	switch verb {
	case "GET", "HEAD":
		if scope != "resource" && scope != "" {
			return "LIST"
		}
		return "GET"
	default:
		return verb
	}
}

// CleanVerb returns a normalized verb, so that it is easy to tell WATCH from
// LIST, APPLY from PATCH and CONNECT from others.
func CleanVerb(verb string, request *http.Request, requestInfo *request.RequestInfo) string {
	reportedVerb := verb
	if suggestedVerb := getVerbIfWatch(request); suggestedVerb == "WATCH" {
		reportedVerb = "WATCH"
	}
	// normalize the legacy WATCHLIST to WATCH to ensure users aren't surprised by metrics
	if verb == "WATCHLIST" {
		reportedVerb = "WATCH"
	}
	if verb == "PATCH" && request.Header.Get("Content-Type") == string(types.ApplyPatchType) {
		reportedVerb = "APPLY"
	}
	if requestInfo != nil && requestInfo.IsResourceRequest && len(requestInfo.Subresource) > 0 && validConnectRequests.Has(requestInfo.Subresource) {
		reportedVerb = "CONNECT"
	}
	return reportedVerb
}

// determineRequestNamespaceAndName computes name and namespace for the given requests
//
// note that the logic of this function was copy&pasted from cacher.go
// after an unsuccessful attempt of moving it to RequestInfo
//
// see: https://github.com/kubernetes/kubernetes/pull/120520
func determineRequestNamespaceAndName(ctx context.Context, opts *metainternalversion.ListOptions) (namespace, name string) {
	if requestNamespace, ok := request.NamespaceFrom(ctx); ok && len(requestNamespace) > 0 {
		namespace = requestNamespace
	} else if opts != nil && opts.FieldSelector != nil {
		if selectorNamespace, ok := opts.FieldSelector.RequiresExactMatch("metadata.namespace"); ok {
			namespace = selectorNamespace
		}
	}
	if requestInfo, ok := request.RequestInfoFrom(ctx); ok && requestInfo != nil && len(requestInfo.Name) > 0 {
		name = requestInfo.Name
	} else if opts != nil && opts.FieldSelector != nil {
		if selectorName, ok := opts.FieldSelector.RequiresExactMatch("metadata.name"); ok {
			name = selectorName
		}
	}
	return
}

// cleanVerb additionally ensures that unknown verbs don't clog up the metrics.
func cleanVerb(verb, suggestedVerb string, request *http.Request, requestInfo *request.RequestInfo) string {
	// CanonicalVerb (being an input for this function) doesn't handle correctly the
	// deprecated path pattern for watch of:
	//   GET /api/{version}/watch/{resource}
	// We correct it manually based on the pass verb from the installer.
	if suggestedVerb == "WATCH" || suggestedVerb == "WATCHLIST" {
		return "WATCH"
	}
	reportedVerb := CleanVerb(verb, request, requestInfo)
	if validRequestMethods.Has(reportedVerb) {
		return reportedVerb
	}
	return OtherRequestMethod
}

// getVerbIfWatch additionally ensures that GET or List would be transformed to WATCH
func getVerbIfWatch(req *http.Request) string {
	if strings.ToUpper(req.Method) == "GET" || strings.ToUpper(req.Method) == "LIST" {
		// see apimachinery/pkg/runtime/conversion.go Convert_Slice_string_To_bool
		if values := req.URL.Query()["watch"]; len(values) > 0 {
			if value := strings.ToLower(values[0]); value != "0" && value != "false" {
				return "WATCH"
			}
		}
	}
	return ""
}

func cleanDryRun(u *url.URL) string {
	// avoid allocating when we don't see dryRun in the query
	if !strings.Contains(u.RawQuery, "dryRun") {
		return ""
	}
	dryRun := u.Query()["dryRun"]
	if errs := validation.ValidateDryRun(nil, dryRun); len(errs) > 0 {
		return "invalid"
	}
	// Since dryRun could be valid with any arbitrarily long length
	// we have to dedup and sort the elements before joining them together
	// TODO: this is a fairly large allocation for what it does, consider
	//   a sort and dedup in a single pass
	return strings.Join(utilsets.NewString(dryRun...).List(), ",")
}

func cleanFieldValidation(u *url.URL) string {
	// avoid allocating when we don't see dryRun in the query
	if !strings.Contains(u.RawQuery, "fieldValidation") {
		return ""
	}
	fieldValidation := u.Query()["fieldValidation"]
	if len(fieldValidation) != 1 {
		return "invalid"
	}
	if errs := validation.ValidateFieldValidation(nil, fieldValidation[0]); len(errs) > 0 {
		return "invalid"
	}
	return fieldValidation[0]
}

var _ http.ResponseWriter = (*ResponseWriterDelegator)(nil)
var _ responsewriter.UserProvidedDecorator = (*ResponseWriterDelegator)(nil)

// ResponseWriterDelegator interface wraps http.ResponseWriter to additionally record content-length, status-code, etc.
type ResponseWriterDelegator struct {
	http.ResponseWriter

	status      int
	written     int64
	wroteHeader bool
}

func (r *ResponseWriterDelegator) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *ResponseWriterDelegator) WriteHeader(code int) {
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *ResponseWriterDelegator) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func (r *ResponseWriterDelegator) Status() int {
	return r.status
}

func (r *ResponseWriterDelegator) ContentLength() int {
	return int(r.written)
}

// Small optimization over Itoa
func codeToString(s int) string {
	switch s {
	case 100:
		return "100"
	case 101:
		return "101"

	case 200:
		return "200"
	case 201:
		return "201"
	case 202:
		return "202"
	case 203:
		return "203"
	case 204:
		return "204"
	case 205:
		return "205"
	case 206:
		return "206"

	case 300:
		return "300"
	case 301:
		return "301"
	case 302:
		return "302"
	case 304:
		return "304"
	case 305:
		return "305"
	case 307:
		return "307"

	case 400:
		return "400"
	case 401:
		return "401"
	case 402:
		return "402"
	case 403:
		return "403"
	case 404:
		return "404"
	case 405:
		return "405"
	case 406:
		return "406"
	case 407:
		return "407"
	case 408:
		return "408"
	case 409:
		return "409"
	case 410:
		return "410"
	case 411:
		return "411"
	case 412:
		return "412"
	case 413:
		return "413"
	case 414:
		return "414"
	case 415:
		return "415"
	case 416:
		return "416"
	case 417:
		return "417"
	case 418:
		return "418"

	case 500:
		return "500"
	case 501:
		return "501"
	case 502:
		return "502"
	case 503:
		return "503"
	case 504:
		return "504"
	case 505:
		return "505"

	case 428:
		return "428"
	case 429:
		return "429"
	case 431:
		return "431"
	case 511:
		return "511"

	default:
		return strconv.Itoa(s)
	}
}
