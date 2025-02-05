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

package metrics

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	epmetrics "k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	compbasemetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	basemetricstestutil "k8s.io/component-base/metrics/testutil"
)

const (
	namespace = "apiserver"
	subsystem = "flowcontrol"
)

const (
	requestKind         = "request_kind"
	priorityLevel       = "priority_level"
	flowSchema          = "flow_schema"
	phase               = "phase"
	LabelNamePhase      = "phase"
	LabelValueWaiting   = "waiting"
	LabelValueExecuting = "executing"
)

var (
	queueLengthBuckets            = []float64{0, 10, 25, 50, 100, 250, 500, 1000}
	requestDurationSecondsBuckets = []float64{0, 0.005, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 15, 30}
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

type resettable interface {
	Reset()
}

// Reset all resettable metrics to zero
func Reset() {
	for _, metric := range metrics {
		if rm, ok := metric.(resettable); ok {
			rm.Reset()
		}
	}
}

// GatherAndCompare the given metrics with the given Prometheus syntax expected value
func GatherAndCompare(expected string, metricNames ...string) error {
	return basemetricstestutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...)
}

// Registerables is a slice of Registerable
type Registerables []compbasemetrics.Registerable

// Append adds more
func (rs Registerables) Append(more ...compbasemetrics.Registerable) Registerables {
	return append(rs, more...)
}

var (
	apiserverRejectedRequestsTotal = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "rejected_requests_total",
			Help:           "Number of requests rejected by API Priority and Fairness subsystem",
			StabilityLevel: compbasemetrics.BETA,
		},
		[]string{priorityLevel, flowSchema, "reason"},
	)
	apiserverDispatchedRequestsTotal = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "dispatched_requests_total",
			Help:           "Number of requests executed by API Priority and Fairness subsystem",
			StabilityLevel: compbasemetrics.BETA,
		},
		[]string{priorityLevel, flowSchema},
	)
	// PriorityLevelExecutionSeatsGaugeVec creates observers of seats occupied throughout execution for priority levels
	PriorityLevelExecutionSeatsGaugeVec = NewTimingRatioHistogramVec(
		&compbasemetrics.TimingHistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "priority_level_seat_utilization",
			Help:      "Observations, at the end of every nanosecond, of utilization of seats for any stage of execution (but only initial stage for WATCHes); denominator is current seat limit or max(1, round(server seat limit/10))",
			// Buckets for both 0.99 and 1.0 mean PromQL's histogram_quantile will reveal saturation
			Buckets:        []float64{0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99, 1},
			ConstLabels:    map[string]string{phase: "executing"},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		priorityLevel,
	)
	// PriorityLevelConcurrencyGaugeVec creates gauges of concurrency broken down by phase, priority level
	PriorityLevelConcurrencyGaugeVec = NewTimingRatioHistogramVec(
		&compbasemetrics.TimingHistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "priority_level_request_utilization",
			Help:      "Observations, at the end of every nanosecond, of number of requests waiting or in any stage of execution (but only initial stage for WATCHes); denominator for waiting is max(1, QueueLengthLimit) X max(1, DesiredNumQueues), for executing is current seat limit if that is not zero otherwise max(1, round(server seat limit/10))",
			// For executing: the denominator will be seats, so this metric will skew low.
			// For waiting: total queue capacity is generally quite generous, so this metric will skew low.
			Buckets:        []float64{0, 0.001, 0.003, 0.01, 0.03, 0.1, 0.25, 0.5, 0.75, 1},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		LabelNamePhase, priorityLevel,
	)
	// readWriteConcurrencyGaugeVec creates ratioed gauges of requests/limit broken down by phase and mutating vs readonly
	readWriteConcurrencyGaugeVec = NewTimingRatioHistogramVec(
		&compbasemetrics.TimingHistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "read_vs_write_current_requests",
			Help:      "Observations, at the end of every nanosecond, of the number of requests (as a fraction of the relevant limit) waiting or in regular stage of execution",
			// This metric will skew low for the same reason as the priority level metrics
			// and also because APF has a combined limit for mutating and readonly.
			Buckets:        []float64{0, 0.001, 0.01, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99, 1},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		LabelNamePhase, requestKind,
	)
	apiserverCurrentR = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "current_r",
			Help:           "R(time of last change)",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverDispatchR = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "dispatch_r",
			Help:           "R(time of last dispatch)",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverLatestS = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "latest_s",
			Help:           "S(most recently dispatched request)",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverNextSBounds = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "next_s_bounds",
			Help:           "min and max, over queues, of S(oldest waiting request in queue)",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, "bound"},
	)
	apiserverNextDiscountedSBounds = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "next_discounted_s_bounds",
			Help:           "min and max, over queues, of S(oldest waiting request in queue) - estimated work in progress",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, "bound"},
	)
	apiserverCurrentInqueueRequests = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "current_inqueue_requests",
			Help:           "Number of requests currently pending in queues of the API Priority and Fairness subsystem",
			StabilityLevel: compbasemetrics.BETA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverCurrentInqueueSeats = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "current_inqueue_seats",
			Help:           "Number of seats currently pending in queues of the API Priority and Fairness subsystem",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverRequestQueueLength = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "request_queue_length_after_enqueue",
			Help:           "Length of queue in the API Priority and Fairness subsystem, as seen by each request after it is enqueued",
			Buckets:        queueLengthBuckets,
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverRequestConcurrencyLimit = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "request_concurrency_limit",
			Help:      "Nominal number of execution seats configured for each priority level",
			// Remove this metric once all suppported releases have the equal nominal_limit_seats metric
			DeprecatedVersion: "1.30.0",
			StabilityLevel:    compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverCurrentExecutingRequests = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "current_executing_requests",
			Help:           "Number of requests in initial (for a WATCH) or any (for a non-WATCH) execution stage in the API Priority and Fairness subsystem",
			StabilityLevel: compbasemetrics.BETA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverCurrentExecutingSeats = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "current_executing_seats",
			Help:           "Concurrency (number of seats) occupied by the currently executing (initial stage for a WATCH, any stage otherwise) requests in the API Priority and Fairness subsystem",
			StabilityLevel: compbasemetrics.BETA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverRequestConcurrencyInUse = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "request_concurrency_in_use",
			Help:      "Concurrency (number of seats) occupied by the currently executing (initial stage for a WATCH, any stage otherwise) requests in the API Priority and Fairness subsystem",
			// Remove this metric once all suppported releases have the equal current_executing_seats metric
			DeprecatedVersion: "1.31.0",
			StabilityLevel:    compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverRequestWaitingSeconds = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "request_wait_duration_seconds",
			Help:           "Length of time a request spent waiting in its queue",
			Buckets:        requestDurationSecondsBuckets,
			StabilityLevel: compbasemetrics.BETA,
		},
		[]string{priorityLevel, flowSchema, "execute"},
	)
	apiserverRequestExecutionSeconds = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "request_execution_seconds",
			Help:           "Duration of initial stage (for a WATCH) or any (for a non-WATCH) stage of request execution in the API Priority and Fairness subsystem",
			Buckets:        requestDurationSecondsBuckets,
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, flowSchema, "type"},
	)
	watchCountSamples = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "watch_count_samples",
			Help:           "count of watchers for mutating requests in API Priority and Fairness",
			Buckets:        []float64{0, 1, 10, 100, 1000, 10000},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverEpochAdvances = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "epoch_advance_total",
			Help:           "Number of times the queueset's progress meter jumped backward",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, "success"},
	)
	apiserverWorkEstimatedSeats = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "work_estimated_seats",
			Help:      "Number of estimated seats (maximum of initial and final seats) associated with requests in API Priority and Fairness",
			// the upper bound comes from the maximum number of seats a request
			// can occupy which is currently set at 10.
			Buckets:        []float64{1, 2, 4, 10},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverDispatchWithNoAccommodation = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "request_dispatch_no_accommodation_total",
			Help:           "Number of times a dispatch attempt resulted in a non accommodation due to lack of available seats",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel, flowSchema},
	)
	apiserverNominalConcurrencyLimits = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "nominal_limit_seats",
			Help:           "Nominal number of execution seats configured for each priority level",
			StabilityLevel: compbasemetrics.BETA,
		},
		[]string{priorityLevel},
	)
	apiserverMinimumConcurrencyLimits = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "lower_limit_seats",
			Help:           "Configured lower bound on number of execution seats available to each priority level",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverMaximumConcurrencyLimits = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "upper_limit_seats",
			Help:           "Configured upper bound on number of execution seats available to each priority level",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	ApiserverSeatDemands = NewTimingRatioHistogramVec(
		&compbasemetrics.TimingHistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "demand_seats",
			Help:      "Observations, at the end of every nanosecond, of (the number of seats each priority level could use) / (current seat limit for that level if that is not zero otherwise max(1, round(server seat limit/10.0)))",
			// Rationale for the bucket boundaries:
			// For 0--1, evenly spaced and not too many;
			// For 1--2, roughly powers of sqrt(sqrt(2));
			// For 2--6, roughly powers of sqrt(2);
			// We need coverage over 1, but do not want too many buckets.
			Buckets:        []float64{0.2, 0.4, 0.6, 0.8, 1, 1.2, 1.4, 1.7, 2, 2.8, 4, 6},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		priorityLevel,
	)
	apiserverSeatDemandHighWatermarks = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "demand_seats_high_watermark",
			Help:           "High watermark, over last adjustment period, of demand_seats",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverSeatDemandAverages = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "demand_seats_average",
			Help:           "Time-weighted average, over last adjustment period, of demand_seats",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverSeatDemandStandardDeviations = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "demand_seats_stdev",
			Help:           "Time-weighted standard deviation, over last adjustment period, of demand_seats",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverSeatDemandSmootheds = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "demand_seats_smoothed",
			Help:           "Smoothed seat demands",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverSeatDemandTargets = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "target_seats",
			Help:           "Seat allocation targets",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)
	apiserverFairFracs = compbasemetrics.NewGauge(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "seat_fair_frac",
			Help:           "Fair fraction of server's concurrency to allocate to each priority level that can use it",
			StabilityLevel: compbasemetrics.ALPHA,
		})
	apiserverCurrentConcurrencyLimits = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "current_limit_seats",
			Help:           "current derived number of execution seats available to each priority level",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{priorityLevel},
	)

	metrics = Registerables{
		apiserverRejectedRequestsTotal,
		apiserverDispatchedRequestsTotal,
		apiserverCurrentR,
		apiserverDispatchR,
		apiserverLatestS,
		apiserverNextSBounds,
		apiserverNextDiscountedSBounds,
		apiserverCurrentInqueueRequests,
		apiserverCurrentInqueueSeats,
		apiserverRequestQueueLength,
		apiserverRequestConcurrencyLimit,
		apiserverRequestConcurrencyInUse,
		apiserverCurrentExecutingSeats,
		apiserverCurrentExecutingRequests,
		apiserverRequestWaitingSeconds,
		apiserverRequestExecutionSeconds,
		watchCountSamples,
		apiserverEpochAdvances,
		apiserverWorkEstimatedSeats,
		apiserverDispatchWithNoAccommodation,
		apiserverNominalConcurrencyLimits,
		apiserverMinimumConcurrencyLimits,
		apiserverMaximumConcurrencyLimits,
		apiserverSeatDemandHighWatermarks,
		apiserverSeatDemandAverages,
		apiserverSeatDemandStandardDeviations,
		apiserverSeatDemandSmootheds,
		apiserverSeatDemandTargets,
		apiserverFairFracs,
		apiserverCurrentConcurrencyLimits,
	}.
		Append(PriorityLevelExecutionSeatsGaugeVec.metrics()...).
		Append(PriorityLevelConcurrencyGaugeVec.metrics()...).
		Append(readWriteConcurrencyGaugeVec.metrics()...).
		Append(ApiserverSeatDemands.metrics()...)
)

type indexOnce struct {
	labelValues []string
	once        sync.Once
	gauge       RatioedGauge
}

func (io *indexOnce) getGauge() RatioedGauge {
	io.once.Do(func() {
		io.gauge = readWriteConcurrencyGaugeVec.NewForLabelValuesSafe(0, 1, io.labelValues)
	})
	return io.gauge
}

var waitingReadonly = indexOnce{labelValues: []string{LabelValueWaiting, epmetrics.ReadOnlyKind}}
var executingReadonly = indexOnce{labelValues: []string{LabelValueExecuting, epmetrics.ReadOnlyKind}}
var waitingMutating = indexOnce{labelValues: []string{LabelValueWaiting, epmetrics.MutatingKind}}
var executingMutating = indexOnce{labelValues: []string{LabelValueExecuting, epmetrics.MutatingKind}}

// GetWaitingReadonlyConcurrency returns the gauge of number of readonly requests waiting / limit on those.
var GetWaitingReadonlyConcurrency = waitingReadonly.getGauge

// GetExecutingReadonlyConcurrency returns the gauge of number of executing readonly requests / limit on those.
var GetExecutingReadonlyConcurrency = executingReadonly.getGauge

// GetWaitingMutatingConcurrency returns the gauge of number of mutating requests waiting / limit on those.
var GetWaitingMutatingConcurrency = waitingMutating.getGauge

// GetExecutingMutatingConcurrency returns the gauge of number of executing mutating requests / limit on those.
var GetExecutingMutatingConcurrency = executingMutating.getGauge

// AddRequestsInQueues adds the given delta to the gauge of the # of requests in the queues of the specified flowSchema and priorityLevel
func AddRequestsInQueues(ctx context.Context, priorityLevel, flowSchema string, delta int) {
	apiserverCurrentInqueueRequests.WithLabelValues(priorityLevel, flowSchema).Add(float64(delta))
}

// AddSeatsInQueues adds the given delta to the gauge of the # of seats in the queues of the specified flowSchema and priorityLevel
func AddSeatsInQueues(ctx context.Context, priorityLevel, flowSchema string, delta int) {
	apiserverCurrentInqueueSeats.WithLabelValues(priorityLevel, flowSchema).Add(float64(delta))
}

// AddRequestsExecuting adds the given delta to the gauge of executing requests of the given flowSchema and priorityLevel
func AddRequestsExecuting(ctx context.Context, priorityLevel, flowSchema string, delta int) {
	apiserverCurrentExecutingRequests.WithLabelValues(priorityLevel, flowSchema).Add(float64(delta))
}

// SetCurrentR sets the current-R (virtualTime) gauge for the given priority level
func SetCurrentR(priorityLevel string, r float64) {
	apiserverCurrentR.WithLabelValues(priorityLevel).Set(r)
}

// SetLatestS sets the latest-S (virtual time of dispatched request) gauge for the given priority level
func SetDispatchMetrics(priorityLevel string, r, s, sMin, sMax, discountedSMin, discountedSMax float64) {
	apiserverDispatchR.WithLabelValues(priorityLevel).Set(r)
	apiserverLatestS.WithLabelValues(priorityLevel).Set(s)
	apiserverNextSBounds.WithLabelValues(priorityLevel, "min").Set(sMin)
	apiserverNextSBounds.WithLabelValues(priorityLevel, "max").Set(sMax)
	apiserverNextDiscountedSBounds.WithLabelValues(priorityLevel, "min").Set(discountedSMin)
	apiserverNextDiscountedSBounds.WithLabelValues(priorityLevel, "max").Set(discountedSMax)
}

// AddSeatConcurrencyInUse adds the given delta to the gauge of seats in use by
// the currently executing requests of the given flowSchema and priorityLevel
func AddSeatConcurrencyInUse(priorityLevel, flowSchema string, delta int) {
	apiserverCurrentExecutingSeats.WithLabelValues(priorityLevel, flowSchema).Add(float64(delta))
	apiserverRequestConcurrencyInUse.WithLabelValues(priorityLevel, flowSchema).Add(float64(delta))
}

// AddReject increments the # of rejected requests for flow control
func AddReject(ctx context.Context, priorityLevel, flowSchema, reason string) {
	apiserverRejectedRequestsTotal.WithContext(ctx).WithLabelValues(priorityLevel, flowSchema, reason).Add(1)
}

// AddDispatch increments the # of dispatched requests for flow control
func AddDispatch(ctx context.Context, priorityLevel, flowSchema string) {
	apiserverDispatchedRequestsTotal.WithContext(ctx).WithLabelValues(priorityLevel, flowSchema).Add(1)
}

// ObserveQueueLength observes the queue length for flow control
func ObserveQueueLength(ctx context.Context, priorityLevel, flowSchema string, length int) {
	apiserverRequestQueueLength.WithContext(ctx).WithLabelValues(priorityLevel, flowSchema).Observe(float64(length))
}

// ObserveWaitingDuration observes the queue length for flow control
func ObserveWaitingDuration(ctx context.Context, priorityLevel, flowSchema, execute string, waitTime time.Duration) {
	apiserverRequestWaitingSeconds.WithContext(ctx).WithLabelValues(priorityLevel, flowSchema, execute).Observe(waitTime.Seconds())
}

// ObserveExecutionDuration observes the execution duration for flow control
func ObserveExecutionDuration(ctx context.Context, priorityLevel, flowSchema string, executionTime time.Duration) {
	reqType := "regular"
	if requestInfo, ok := apirequest.RequestInfoFrom(ctx); ok && requestInfo.Verb == "watch" {
		reqType = requestInfo.Verb
	}
	apiserverRequestExecutionSeconds.WithContext(ctx).WithLabelValues(priorityLevel, flowSchema, reqType).Observe(executionTime.Seconds())
}

// ObserveWatchCount notes a sampling of a watch count
func ObserveWatchCount(ctx context.Context, priorityLevel, flowSchema string, count int) {
	watchCountSamples.WithLabelValues(priorityLevel, flowSchema).Observe(float64(count))
}

// AddEpochAdvance notes an advance of the progress meter baseline for a given priority level
func AddEpochAdvance(ctx context.Context, priorityLevel string, success bool) {
	apiserverEpochAdvances.WithContext(ctx).WithLabelValues(priorityLevel, strconv.FormatBool(success)).Inc()
}

// ObserveWorkEstimatedSeats notes a sampling of estimated seats associated with a request
func ObserveWorkEstimatedSeats(priorityLevel, flowSchema string, seats int) {
	apiserverWorkEstimatedSeats.WithLabelValues(priorityLevel, flowSchema).Observe(float64(seats))
}

// AddDispatchWithNoAccommodation keeps track of number of times dispatch attempt results
// in a non accommodation due to lack of available seats.
func AddDispatchWithNoAccommodation(priorityLevel, flowSchema string) {
	apiserverDispatchWithNoAccommodation.WithLabelValues(priorityLevel, flowSchema).Inc()
}

func SetPriorityLevelConfiguration(priorityLevel string, nominalCL, minCL, maxCL int) {
	apiserverRequestConcurrencyLimit.WithLabelValues(priorityLevel).Set(float64(nominalCL))
	apiserverNominalConcurrencyLimits.WithLabelValues(priorityLevel).Set(float64(nominalCL))
	apiserverMinimumConcurrencyLimits.WithLabelValues(priorityLevel).Set(float64(minCL))
	apiserverMaximumConcurrencyLimits.WithLabelValues(priorityLevel).Set(float64(maxCL))
}

func NotePriorityLevelConcurrencyAdjustment(priorityLevel string, seatDemandHWM, seatDemandAvg, seatDemandStdev, seatDemandSmoothed, seatDemandTarget float64, currentCL int) {
	apiserverSeatDemandHighWatermarks.WithLabelValues(priorityLevel).Set(seatDemandHWM)
	apiserverSeatDemandAverages.WithLabelValues(priorityLevel).Set(seatDemandAvg)
	apiserverSeatDemandStandardDeviations.WithLabelValues(priorityLevel).Set(seatDemandStdev)
	apiserverSeatDemandSmootheds.WithLabelValues(priorityLevel).Set(seatDemandSmoothed)
	apiserverSeatDemandTargets.WithLabelValues(priorityLevel).Set(seatDemandTarget)
	apiserverCurrentConcurrencyLimits.WithLabelValues(priorityLevel).Set(float64(currentCL))
}

func SetFairFrac(fairFrac float64) {
	apiserverFairFracs.Set(fairFrac)
}
