// Copyright 2014 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/beorn7/perks/quantile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// quantileLabel is used for the label that defines the quantile in a
// summary.
const quantileLabel = "quantile"

// A Summary captures individual observations from an event or sample stream and
// summarizes them in a manner similar to traditional summary statistics: 1. sum
// of observations, 2. observation count, 3. rank estimations.
//
// A typical use-case is the observation of request latencies. By default, a
// Summary provides the median, the 90th and the 99th percentile of the latency
// as rank estimations. However, the default behavior will change in the
// upcoming v1.0.0 of the library. There will be no rank estimations at all by
// default. For a sane transition, it is recommended to set the desired rank
// estimations explicitly.
//
// Note that the rank estimations cannot be aggregated in a meaningful way with
// the Prometheus query language (i.e. you cannot average or add them). If you
// need aggregatable quantiles (e.g. you want the 99th percentile latency of all
// queries served across all instances of a service), consider the Histogram
// metric type. See the Prometheus documentation for more details.
//
// To create Summary instances, use NewSummary.
type Summary interface {
	Metric
	Collector

	// Observe adds a single observation to the summary. Observations are
	// usually positive or zero. Negative observations are accepted but
	// prevent current versions of Prometheus from properly detecting
	// counter resets in the sum of observations. See
	// https://prometheus.io/docs/practices/histograms/#count-and-sum-of-observations
	// for details.
	Observe(float64)
}

var errQuantileLabelNotAllowed = fmt.Errorf(
	"%q is not allowed as label name in summaries", quantileLabel,
)

// Default values for SummaryOpts.
const (
	// DefMaxAge is the default duration for which observations stay
	// relevant.
	DefMaxAge time.Duration = 10 * time.Minute
	// DefAgeBuckets is the default number of buckets used to calculate the
	// age of observations.
	DefAgeBuckets = 5
	// DefBufCap is the standard buffer size for collecting Summary observations.
	DefBufCap = 500
)

// SummaryOpts bundles the options for creating a Summary metric. It is
// mandatory to set Name to a non-empty string. While all other fields are
// optional and can safely be left at their zero value, it is recommended to set
// a help string and to explicitly set the Objectives field to the desired value
// as the default value will change in the upcoming v1.0.0 of the library.
type SummaryOpts struct {
	// Namespace, Subsystem, and Name are components of the fully-qualified
	// name of the Summary (created by joining these components with
	// "_"). Only Name is mandatory, the others merely help structuring the
	// name. Note that the fully-qualified name of the Summary must be a
	// valid Prometheus metric name.
	Namespace string
	Subsystem string
	Name      string

	// Help provides information about this Summary.
	//
	// Metrics with the same fully-qualified name must have the same Help
	// string.
	Help string

	// ConstLabels are used to attach fixed labels to this metric. Metrics
	// with the same fully-qualified name must have the same label names in
	// their ConstLabels.
	//
	// Due to the way a Summary is represented in the Prometheus text format
	// and how it is handled by the Prometheus server internally, “quantile”
	// is an illegal label name. Construction of a Summary or SummaryVec
	// will panic if this label name is used in ConstLabels.
	//
	// ConstLabels are only used rarely. In particular, do not use them to
	// attach the same labels to all your metrics. Those use cases are
	// better covered by target labels set by the scraping Prometheus
	// server, or by one specific metric (e.g. a build_info or a
	// machine_role metric). See also
	// https://prometheus.io/docs/instrumenting/writing_exporters/#target-labels-not-static-scraped-labels
	ConstLabels Labels

	// Objectives defines the quantile rank estimates with their respective
	// absolute error. If Objectives[q] = e, then the value reported for q
	// will be the φ-quantile value for some φ between q-e and q+e.  The
	// default value is an empty map, resulting in a summary without
	// quantiles.
	Objectives map[float64]float64

	// MaxAge defines the duration for which an observation stays relevant
	// for the summary. Only applies to pre-calculated quantiles, does not
	// apply to _sum and _count. Must be positive. The default value is
	// DefMaxAge.
	MaxAge time.Duration

	// AgeBuckets is the number of buckets used to exclude observations that
	// are older than MaxAge from the summary. A higher number has a
	// resource penalty, so only increase it if the higher resolution is
	// really required. For very high observation rates, you might want to
	// reduce the number of age buckets. With only one age bucket, you will
	// effectively see a complete reset of the summary each time MaxAge has
	// passed. The default value is DefAgeBuckets.
	AgeBuckets uint32

	// BufCap defines the default sample stream buffer size.  The default
	// value of DefBufCap should suffice for most uses. If there is a need
	// to increase the value, a multiple of 500 is recommended (because that
	// is the internal buffer size of the underlying package
	// "github.com/bmizerany/perks/quantile").
	BufCap uint32

	// now is for testing purposes, by default it's time.Now.
	now func() time.Time
}

// SummaryVecOpts bundles the options to create a SummaryVec metric.
// It is mandatory to set SummaryOpts, see there for mandatory fields. VariableLabels
// is optional and can safely be left to its default value.
type SummaryVecOpts struct {
	SummaryOpts

	// VariableLabels are used to partition the metric vector by the given set
	// of labels. Each label value will be constrained with the optional Constraint
	// function, if provided.
	VariableLabels ConstrainableLabels
}

// Problem with the sliding-window decay algorithm... The Merge method of
// perk/quantile is actually not working as advertised - and it might be
// unfixable, as the underlying algorithm is apparently not capable of merging
// summaries in the first place. To avoid using Merge, we are currently adding
// observations to _each_ age bucket, i.e. the effort to add a sample is
// essentially multiplied by the number of age buckets. When rotating age
// buckets, we empty the previous head stream. On scrape time, we simply take
// the quantiles from the head stream (no merging required). Result: More effort
// on observation time, less effort on scrape time, which is exactly the
// opposite of what we try to accomplish, but at least the results are correct.
//
// The quite elegant previous contraption to merge the age buckets efficiently
// on scrape time (see code up commit 6b9530d72ea715f0ba612c0120e6e09fbf1d49d0)
// can't be used anymore.

// NewSummary creates a new Summary based on the provided SummaryOpts.
func NewSummary(opts SummaryOpts) Summary {
	return newSummary(
		NewDesc(
			BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
			opts.Help,
			nil,
			opts.ConstLabels,
		),
		opts,
	)
}

func newSummary(desc *Desc, opts SummaryOpts, labelValues ...string) Summary {
	if len(desc.variableLabels.names) != len(labelValues) {
		panic(makeInconsistentCardinalityError(desc.fqName, desc.variableLabels.names, labelValues))
	}

	for _, n := range desc.variableLabels.names {
		if n == quantileLabel {
			panic(errQuantileLabelNotAllowed)
		}
	}
	for _, lp := range desc.constLabelPairs {
		if lp.GetName() == quantileLabel {
			panic(errQuantileLabelNotAllowed)
		}
	}

	if opts.Objectives == nil {
		opts.Objectives = map[float64]float64{}
	}

	if opts.MaxAge < 0 {
		panic(fmt.Errorf("illegal max age MaxAge=%v", opts.MaxAge))
	}
	if opts.MaxAge == 0 {
		opts.MaxAge = DefMaxAge
	}

	if opts.AgeBuckets == 0 {
		opts.AgeBuckets = DefAgeBuckets
	}

	if opts.BufCap == 0 {
		opts.BufCap = DefBufCap
	}

	if opts.now == nil {
		opts.now = time.Now
	}
	if len(opts.Objectives) == 0 {
		// Use the lock-free implementation of a Summary without objectives.
		s := &noObjectivesSummary{
			desc:       desc,
			labelPairs: MakeLabelPairs(desc, labelValues),
			counts:     [2]*summaryCounts{{}, {}},
		}
		s.init(s) // Init self-collection.
		s.createdTs = timestamppb.New(opts.now())
		return s
	}

	s := &summary{
		desc: desc,
		now:  opts.now,

		objectives:       opts.Objectives,
		sortedObjectives: make([]float64, 0, len(opts.Objectives)),

		labelPairs: MakeLabelPairs(desc, labelValues),

		hotBuf:         make([]float64, 0, opts.BufCap),
		coldBuf:        make([]float64, 0, opts.BufCap),
		streamDuration: opts.MaxAge / time.Duration(opts.AgeBuckets),
	}
	s.headStreamExpTime = opts.now().Add(s.streamDuration)
	s.hotBufExpTime = s.headStreamExpTime

	for i := uint32(0); i < opts.AgeBuckets; i++ {
		s.streams = append(s.streams, s.newStream())
	}
	s.headStream = s.streams[0]

	for qu := range s.objectives {
		s.sortedObjectives = append(s.sortedObjectives, qu)
	}
	sort.Float64s(s.sortedObjectives)

	s.init(s) // Init self-collection.
	s.createdTs = timestamppb.New(opts.now())
	return s
}

type summary struct {
	selfCollector

	bufMtx sync.Mutex // Protects hotBuf and hotBufExpTime.
	mtx    sync.Mutex // Protects every other moving part.
	// Lock bufMtx before mtx if both are needed.

	desc *Desc

	now func() time.Time

	objectives       map[float64]float64
	sortedObjectives []float64

	labelPairs []*dto.LabelPair

	sum float64
	cnt uint64

	hotBuf, coldBuf []float64

	streams                          []*quantile.Stream
	streamDuration                   time.Duration
	headStream                       *quantile.Stream
	headStreamIdx                    int
	headStreamExpTime, hotBufExpTime time.Time

	createdTs *timestamppb.Timestamp
}

func (s *summary) Desc() *Desc {
	return s.desc
}

func (s *summary) Observe(v float64) {
	s.bufMtx.Lock()
	defer s.bufMtx.Unlock()

	now := s.now()
	if now.After(s.hotBufExpTime) {
		s.asyncFlush(now)
	}
	s.hotBuf = append(s.hotBuf, v)
	if len(s.hotBuf) == cap(s.hotBuf) {
		s.asyncFlush(now)
	}
}

func (s *summary) Write(out *dto.Metric) error {
	sum := &dto.Summary{
		CreatedTimestamp: s.createdTs,
	}
	qs := make([]*dto.Quantile, 0, len(s.objectives))

	s.bufMtx.Lock()
	s.mtx.Lock()
	// Swap bufs even if hotBuf is empty to set new hotBufExpTime.
	s.swapBufs(s.now())
	s.bufMtx.Unlock()

	s.flushColdBuf()
	sum.SampleCount = proto.Uint64(s.cnt)
	sum.SampleSum = proto.Float64(s.sum)

	for _, rank := range s.sortedObjectives {
		var q float64
		if s.headStream.Count() == 0 {
			q = math.NaN()
		} else {
			q = s.headStream.Query(rank)
		}
		qs = append(qs, &dto.Quantile{
			Quantile: proto.Float64(rank),
			Value:    proto.Float64(q),
		})
	}

	s.mtx.Unlock()

	if len(qs) > 0 {
		sort.Sort(quantSort(qs))
	}
	sum.Quantile = qs

	out.Summary = sum
	out.Label = s.labelPairs
	return nil
}

func (s *summary) newStream() *quantile.Stream {
	return quantile.NewTargeted(s.objectives)
}

// asyncFlush needs bufMtx locked.
func (s *summary) asyncFlush(now time.Time) {
	s.mtx.Lock()
	s.swapBufs(now)

	// Unblock the original goroutine that was responsible for the mutation
	// that triggered the compaction.  But hold onto the global non-buffer
	// state mutex until the operation finishes.
	go func() {
		s.flushColdBuf()
		s.mtx.Unlock()
	}()
}

// rotateStreams needs mtx AND bufMtx locked.
func (s *summary) maybeRotateStreams() {
	for !s.hotBufExpTime.Equal(s.headStreamExpTime) {
		s.headStream.Reset()
		s.headStreamIdx++
		if s.headStreamIdx >= len(s.streams) {
			s.headStreamIdx = 0
		}
		s.headStream = s.streams[s.headStreamIdx]
		s.headStreamExpTime = s.headStreamExpTime.Add(s.streamDuration)
	}
}

// flushColdBuf needs mtx locked.
func (s *summary) flushColdBuf() {
	for _, v := range s.coldBuf {
		for _, stream := range s.streams {
			stream.Insert(v)
		}
		s.cnt++
		s.sum += v
	}
	s.coldBuf = s.coldBuf[0:0]
	s.maybeRotateStreams()
}

// swapBufs needs mtx AND bufMtx locked, coldBuf must be empty.
func (s *summary) swapBufs(now time.Time) {
	if len(s.coldBuf) != 0 {
		panic("coldBuf is not empty")
	}
	s.hotBuf, s.coldBuf = s.coldBuf, s.hotBuf
	// hotBuf is now empty and gets new expiration set.
	for now.After(s.hotBufExpTime) {
		s.hotBufExpTime = s.hotBufExpTime.Add(s.streamDuration)
	}
}

type summaryCounts struct {
	// sumBits contains the bits of the float64 representing the sum of all
	// observations. sumBits and count have to go first in the struct to
	// guarantee alignment for atomic operations.
	// http://golang.org/pkg/sync/atomic/#pkg-note-BUG
	sumBits uint64
	count   uint64
}

type noObjectivesSummary struct {
	// countAndHotIdx enables lock-free writes with use of atomic updates.
	// The most significant bit is the hot index [0 or 1] of the count field
	// below. Observe calls update the hot one. All remaining bits count the
	// number of Observe calls. Observe starts by incrementing this counter,
	// and finish by incrementing the count field in the respective
	// summaryCounts, as a marker for completion.
	//
	// Calls of the Write method (which are non-mutating reads from the
	// perspective of the summary) swap the hot–cold under the writeMtx
	// lock. A cooldown is awaited (while locked) by comparing the number of
	// observations with the initiation count. Once they match, then the
	// last observation on the now cool one has completed. All cool fields must
	// be merged into the new hot before releasing writeMtx.

	// Fields with atomic access first! See alignment constraint:
	// http://golang.org/pkg/sync/atomic/#pkg-note-BUG
	countAndHotIdx uint64

	selfCollector
	desc     *Desc
	writeMtx sync.Mutex // Only used in the Write method.

	// Two counts, one is "hot" for lock-free observations, the other is
	// "cold" for writing out a dto.Metric. It has to be an array of
	// pointers to guarantee 64bit alignment of the histogramCounts, see
	// http://golang.org/pkg/sync/atomic/#pkg-note-BUG.
	counts [2]*summaryCounts

	labelPairs []*dto.LabelPair

	createdTs *timestamppb.Timestamp
}

func (s *noObjectivesSummary) Desc() *Desc {
	return s.desc
}

func (s *noObjectivesSummary) Observe(v float64) {
	// We increment h.countAndHotIdx so that the counter in the lower
	// 63 bits gets incremented. At the same time, we get the new value
	// back, which we can use to find the currently-hot counts.
	n := atomic.AddUint64(&s.countAndHotIdx, 1)
	hotCounts := s.counts[n>>63]

	atomicUpdateFloat(&hotCounts.sumBits, func(oldVal float64) float64 {
		return oldVal + v
	})
	// Increment count last as we take it as a signal that the observation
	// is complete.
	atomic.AddUint64(&hotCounts.count, 1)
}

func (s *noObjectivesSummary) Write(out *dto.Metric) error {
	// For simplicity, we protect this whole method by a mutex. It is not in
	// the hot path, i.e. Observe is called much more often than Write. The
	// complication of making Write lock-free isn't worth it, if possible at
	// all.
	s.writeMtx.Lock()
	defer s.writeMtx.Unlock()

	// Adding 1<<63 switches the hot index (from 0 to 1 or from 1 to 0)
	// without touching the count bits. See the struct comments for a full
	// description of the algorithm.
	n := atomic.AddUint64(&s.countAndHotIdx, 1<<63)
	// count is contained unchanged in the lower 63 bits.
	count := n & ((1 << 63) - 1)
	// The most significant bit tells us which counts is hot. The complement
	// is thus the cold one.
	hotCounts := s.counts[n>>63]
	coldCounts := s.counts[(^n)>>63]

	// Await cooldown.
	for count != atomic.LoadUint64(&coldCounts.count) {
		runtime.Gosched() // Let observations get work done.
	}

	sum := &dto.Summary{
		SampleCount:      proto.Uint64(count),
		SampleSum:        proto.Float64(math.Float64frombits(atomic.LoadUint64(&coldCounts.sumBits))),
		CreatedTimestamp: s.createdTs,
	}

	out.Summary = sum
	out.Label = s.labelPairs

	// Finally add all the cold counts to the new hot counts and reset the cold counts.
	atomic.AddUint64(&hotCounts.count, count)
	atomic.StoreUint64(&coldCounts.count, 0)

	// Use atomicUpdateFloat to update hotCounts.sumBits atomically.
	atomicUpdateFloat(&hotCounts.sumBits, func(oldVal float64) float64 {
		return oldVal + sum.GetSampleSum()
	})
	atomic.StoreUint64(&coldCounts.sumBits, 0)

	return nil
}

type quantSort []*dto.Quantile

func (s quantSort) Len() int {
	return len(s)
}

func (s quantSort) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s quantSort) Less(i, j int) bool {
	return s[i].GetQuantile() < s[j].GetQuantile()
}

// SummaryVec is a Collector that bundles a set of Summaries that all share the
// same Desc, but have different values for their variable labels. This is used
// if you want to count the same thing partitioned by various dimensions
// (e.g. HTTP request latencies, partitioned by status code and method). Create
// instances with NewSummaryVec.
type SummaryVec struct {
	*MetricVec
}

// NewSummaryVec creates a new SummaryVec based on the provided SummaryOpts and
// partitioned by the given label names.
//
// Due to the way a Summary is represented in the Prometheus text format and how
// it is handled by the Prometheus server internally, “quantile” is an illegal
// label name. NewSummaryVec will panic if this label name is used.
func NewSummaryVec(opts SummaryOpts, labelNames []string) *SummaryVec {
	return V2.NewSummaryVec(SummaryVecOpts{
		SummaryOpts:    opts,
		VariableLabels: UnconstrainedLabels(labelNames),
	})
}

// NewSummaryVec creates a new SummaryVec based on the provided SummaryVecOpts.
func (v2) NewSummaryVec(opts SummaryVecOpts) *SummaryVec {
	for _, ln := range opts.VariableLabels.labelNames() {
		if ln == quantileLabel {
			panic(errQuantileLabelNotAllowed)
		}
	}
	desc := V2.NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		opts.VariableLabels,
		opts.ConstLabels,
	)
	return &SummaryVec{
		MetricVec: NewMetricVec(desc, func(lvs ...string) Metric {
			return newSummary(desc, opts.SummaryOpts, lvs...)
		}),
	}
}

// GetMetricWithLabelValues returns the Summary for the given slice of label
// values (same order as the variable labels in Desc). If that combination of
// label values is accessed for the first time, a new Summary is created.
//
// It is possible to call this method without using the returned Summary to only
// create the new Summary but leave it at its starting value, a Summary without
// any observations.
//
// Keeping the Summary for later use is possible (and should be considered if
// performance is critical), but keep in mind that Reset, DeleteLabelValues and
// Delete can be used to delete the Summary from the SummaryVec. In that case,
// the Summary will still exist, but it will not be exported anymore, even if a
// Summary with the same label values is created later. See also the CounterVec
// example.
//
// An error is returned if the number of label values is not the same as the
// number of variable labels in Desc (minus any curried labels).
//
// Note that for more than one label value, this method is prone to mistakes
// caused by an incorrect order of arguments. Consider GetMetricWith(Labels) as
// an alternative to avoid that type of mistake. For higher label numbers, the
// latter has a much more readable (albeit more verbose) syntax, but it comes
// with a performance overhead (for creating and processing the Labels map).
// See also the GaugeVec example.
func (v *SummaryVec) GetMetricWithLabelValues(lvs ...string) (Observer, error) {
	metric, err := v.MetricVec.GetMetricWithLabelValues(lvs...)
	if metric != nil {
		return metric.(Observer), err
	}
	return nil, err
}

// GetMetricWith returns the Summary for the given Labels map (the label names
// must match those of the variable labels in Desc). If that label map is
// accessed for the first time, a new Summary is created. Implications of
// creating a Summary without using it and keeping the Summary for later use are
// the same as for GetMetricWithLabelValues.
//
// An error is returned if the number and names of the Labels are inconsistent
// with those of the variable labels in Desc (minus any curried labels).
//
// This method is used for the same purpose as
// GetMetricWithLabelValues(...string). See there for pros and cons of the two
// methods.
func (v *SummaryVec) GetMetricWith(labels Labels) (Observer, error) {
	metric, err := v.MetricVec.GetMetricWith(labels)
	if metric != nil {
		return metric.(Observer), err
	}
	return nil, err
}

// WithLabelValues works as GetMetricWithLabelValues, but panics where
// GetMetricWithLabelValues would have returned an error. Not returning an
// error allows shortcuts like
//
//	myVec.WithLabelValues("404", "GET").Observe(42.21)
func (v *SummaryVec) WithLabelValues(lvs ...string) Observer {
	s, err := v.GetMetricWithLabelValues(lvs...)
	if err != nil {
		panic(err)
	}
	return s
}

// With works as GetMetricWith, but panics where GetMetricWithLabels would have
// returned an error. Not returning an error allows shortcuts like
//
//	myVec.With(prometheus.Labels{"code": "404", "method": "GET"}).Observe(42.21)
func (v *SummaryVec) With(labels Labels) Observer {
	s, err := v.GetMetricWith(labels)
	if err != nil {
		panic(err)
	}
	return s
}

// CurryWith returns a vector curried with the provided labels, i.e. the
// returned vector has those labels pre-set for all labeled operations performed
// on it. The cardinality of the curried vector is reduced accordingly. The
// order of the remaining labels stays the same (just with the curried labels
// taken out of the sequence – which is relevant for the
// (GetMetric)WithLabelValues methods). It is possible to curry a curried
// vector, but only with labels not yet used for currying before.
//
// The metrics contained in the SummaryVec are shared between the curried and
// uncurried vectors. They are just accessed differently. Curried and uncurried
// vectors behave identically in terms of collection. Only one must be
// registered with a given registry (usually the uncurried version). The Reset
// method deletes all metrics, even if called on a curried vector.
func (v *SummaryVec) CurryWith(labels Labels) (ObserverVec, error) {
	vec, err := v.MetricVec.CurryWith(labels)
	if vec != nil {
		return &SummaryVec{vec}, err
	}
	return nil, err
}

// MustCurryWith works as CurryWith but panics where CurryWith would have
// returned an error.
func (v *SummaryVec) MustCurryWith(labels Labels) ObserverVec {
	vec, err := v.CurryWith(labels)
	if err != nil {
		panic(err)
	}
	return vec
}

type constSummary struct {
	desc       *Desc
	count      uint64
	sum        float64
	quantiles  map[float64]float64
	labelPairs []*dto.LabelPair
	createdTs  *timestamppb.Timestamp
}

func (s *constSummary) Desc() *Desc {
	return s.desc
}

func (s *constSummary) Write(out *dto.Metric) error {
	sum := &dto.Summary{
		CreatedTimestamp: s.createdTs,
	}
	qs := make([]*dto.Quantile, 0, len(s.quantiles))

	sum.SampleCount = proto.Uint64(s.count)
	sum.SampleSum = proto.Float64(s.sum)

	for rank, q := range s.quantiles {
		qs = append(qs, &dto.Quantile{
			Quantile: proto.Float64(rank),
			Value:    proto.Float64(q),
		})
	}

	if len(qs) > 0 {
		sort.Sort(quantSort(qs))
	}
	sum.Quantile = qs

	out.Summary = sum
	out.Label = s.labelPairs

	return nil
}

// NewConstSummary returns a metric representing a Prometheus summary with fixed
// values for the count, sum, and quantiles. As those parameters cannot be
// changed, the returned value does not implement the Summary interface (but
// only the Metric interface). Users of this package will not have much use for
// it in regular operations. However, when implementing custom Collectors, it is
// useful as a throw-away metric that is generated on the fly to send it to
// Prometheus in the Collect method.
//
// quantiles maps ranks to quantile values. For example, a median latency of
// 0.23s and a 99th percentile latency of 0.56s would be expressed as:
//
//	map[float64]float64{0.5: 0.23, 0.99: 0.56}
//
// NewConstSummary returns an error if the length of labelValues is not
// consistent with the variable labels in Desc or if Desc is invalid.
func NewConstSummary(
	desc *Desc,
	count uint64,
	sum float64,
	quantiles map[float64]float64,
	labelValues ...string,
) (Metric, error) {
	if desc.err != nil {
		return nil, desc.err
	}
	if err := validateLabelValues(labelValues, len(desc.variableLabels.names)); err != nil {
		return nil, err
	}
	return &constSummary{
		desc:       desc,
		count:      count,
		sum:        sum,
		quantiles:  quantiles,
		labelPairs: MakeLabelPairs(desc, labelValues),
	}, nil
}

// MustNewConstSummary is a version of NewConstSummary that panics where
// NewConstMetric would have returned an error.
func MustNewConstSummary(
	desc *Desc,
	count uint64,
	sum float64,
	quantiles map[float64]float64,
	labelValues ...string,
) Metric {
	m, err := NewConstSummary(desc, count, sum, quantiles, labelValues...)
	if err != nil {
		panic(err)
	}
	return m
}

// NewConstSummaryWithCreatedTimestamp does the same thing as NewConstSummary but sets the created timestamp.
func NewConstSummaryWithCreatedTimestamp(
	desc *Desc,
	count uint64,
	sum float64,
	quantiles map[float64]float64,
	ct time.Time,
	labelValues ...string,
) (Metric, error) {
	if desc.err != nil {
		return nil, desc.err
	}
	if err := validateLabelValues(labelValues, len(desc.variableLabels.names)); err != nil {
		return nil, err
	}
	return &constSummary{
		desc:       desc,
		count:      count,
		sum:        sum,
		quantiles:  quantiles,
		labelPairs: MakeLabelPairs(desc, labelValues),
		createdTs:  timestamppb.New(ct),
	}, nil
}

// MustNewConstSummaryWithCreatedTimestamp is a version of NewConstSummaryWithCreatedTimestamp that panics where
// NewConstSummaryWithCreatedTimestamp would have returned an error.
func MustNewConstSummaryWithCreatedTimestamp(
	desc *Desc,
	count uint64,
	sum float64,
	quantiles map[float64]float64,
	ct time.Time,
	labelValues ...string,
) Metric {
	m, err := NewConstSummaryWithCreatedTimestamp(desc, count, sum, quantiles, ct, labelValues...)
	if err != nil {
		panic(err)
	}
	return m
}
