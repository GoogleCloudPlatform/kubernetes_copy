// Copyright 2018 The Prometheus Authors
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

// Package testutil provides helpers to test code using the prometheus package
// of client_golang.
//
// While writing unit tests to verify correct instrumentation of your code, it's
// a common mistake to mostly test the instrumentation library instead of your
// own code. Rather than verifying that a prometheus.Counter's value has changed
// as expected or that it shows up in the exposition after registration, it is
// in general more robust and more faithful to the concept of unit tests to use
// mock implementations of the prometheus.Counter and prometheus.Registerer
// interfaces that simply assert that the Add or Register methods have been
// called with the expected arguments. However, this might be overkill in simple
// scenarios. The ToFloat64 function is provided for simple inspection of a
// single-value metric, but it has to be used with caution.
//
// End-to-end tests to verify all or larger parts of the metrics exposition can
// be implemented with the CollectAndCompare or GatherAndCompare functions. The
// most appropriate use is not so much testing instrumentation of your code, but
// testing custom prometheus.Collector implementations and in particular whole
// exporters, i.e. programs that retrieve telemetry data from a 3rd party source
// and convert it into Prometheus metrics.
//
// In a similar pattern, CollectAndLint and GatherAndLint can be used to detect
// metrics that have issues with their name, type, or metadata without being
// necessarily invalid, e.g. a counter with a name missing the “_total” suffix.
package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kylelemons/godebug/diff"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"google.golang.org/protobuf/proto"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/internal"
)

// ToFloat64 collects all Metrics from the provided Collector. It expects that
// this results in exactly one Metric being collected, which must be a Gauge,
// Counter, or Untyped. In all other cases, ToFloat64 panics. ToFloat64 returns
// the value of the collected Metric.
//
// The Collector provided is typically a simple instance of Gauge or Counter, or
// – less commonly – a GaugeVec or CounterVec with exactly one element. But any
// Collector fulfilling the prerequisites described above will do.
//
// Use this function with caution. It is computationally very expensive and thus
// not suited at all to read values from Metrics in regular code. This is really
// only for testing purposes, and even for testing, other approaches are often
// more appropriate (see this package's documentation).
//
// A clear anti-pattern would be to use a metric type from the prometheus
// package to track values that are also needed for something else than the
// exposition of Prometheus metrics. For example, you would like to track the
// number of items in a queue because your code should reject queuing further
// items if a certain limit is reached. It is tempting to track the number of
// items in a prometheus.Gauge, as it is then easily available as a metric for
// exposition, too. However, then you would need to call ToFloat64 in your
// regular code, potentially quite often. The recommended way is to track the
// number of items conventionally (in the way you would have done it without
// considering Prometheus metrics) and then expose the number with a
// prometheus.GaugeFunc.
func ToFloat64(c prometheus.Collector) float64 {
	var (
		m      prometheus.Metric
		mCount int
		mChan  = make(chan prometheus.Metric)
		done   = make(chan struct{})
	)

	go func() {
		for m = range mChan {
			mCount++
		}
		close(done)
	}()

	c.Collect(mChan)
	close(mChan)
	<-done

	if mCount != 1 {
		panic(fmt.Errorf("collected %d metrics instead of exactly 1", mCount))
	}

	pb := &dto.Metric{}
	if err := m.Write(pb); err != nil {
		panic(fmt.Errorf("error happened while collecting metrics: %w", err))
	}
	if pb.Gauge != nil {
		return pb.Gauge.GetValue()
	}
	if pb.Counter != nil {
		return pb.Counter.GetValue()
	}
	if pb.Untyped != nil {
		return pb.Untyped.GetValue()
	}
	panic(fmt.Errorf("collected a non-gauge/counter/untyped metric: %s", pb))
}

// CollectAndCount registers the provided Collector with a newly created
// pedantic Registry. It then calls GatherAndCount with that Registry and with
// the provided metricNames. In the unlikely case that the registration or the
// gathering fails, this function panics. (This is inconsistent with the other
// CollectAnd… functions in this package and has historical reasons. Changing
// the function signature would be a breaking change and will therefore only
// happen with the next major version bump.)
func CollectAndCount(c prometheus.Collector, metricNames ...string) int {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		panic(fmt.Errorf("registering collector failed: %w", err))
	}
	result, err := GatherAndCount(reg, metricNames...)
	if err != nil {
		panic(err)
	}
	return result
}

// GatherAndCount gathers all metrics from the provided Gatherer and counts
// them. It returns the number of metric children in all gathered metric
// families together. If any metricNames are provided, only metrics with those
// names are counted.
func GatherAndCount(g prometheus.Gatherer, metricNames ...string) (int, error) {
	got, err := g.Gather()
	if err != nil {
		return 0, fmt.Errorf("gathering metrics failed: %w", err)
	}
	if metricNames != nil {
		got = filterMetrics(got, metricNames)
	}

	result := 0
	for _, mf := range got {
		result += len(mf.GetMetric())
	}
	return result, nil
}

// ScrapeAndCompare calls a remote exporter's endpoint which is expected to return some metrics in
// plain text format. Then it compares it with the results that the `expected` would return.
// If the `metricNames` is not empty it would filter the comparison only to the given metric names.
//
// NOTE: Be mindful of accidental discrepancies between expected and metricNames; metricNames filter
// both expected and scraped metrics. See https://github.com/prometheus/client_golang/issues/1351.
func ScrapeAndCompare(url string, expected io.Reader, metricNames ...string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("scraping metrics failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the scraping target returned a status code other than 200: %d",
			resp.StatusCode)
	}

	scraped, err := convertReaderToMetricFamily(resp.Body)
	if err != nil {
		return err
	}

	wanted, err := convertReaderToMetricFamily(expected)
	if err != nil {
		return err
	}

	return compareMetricFamilies(scraped, wanted, metricNames...)
}

// CollectAndCompare collects the metrics identified by `metricNames` and compares them in the Prometheus text
// exposition format to the data read from expected.
//
// NOTE: Be mindful of accidental discrepancies between expected and metricNames; metricNames filter
// both expected and collected metrics. See https://github.com/prometheus/client_golang/issues/1351.
func CollectAndCompare(c prometheus.Collector, expected io.Reader, metricNames ...string) error {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		return fmt.Errorf("registering collector failed: %w", err)
	}
	return GatherAndCompare(reg, expected, metricNames...)
}

// GatherAndCompare gathers all metrics from the provided Gatherer and compares
// it to an expected output read from the provided Reader in the Prometheus text
// exposition format. If any metricNames are provided, only metrics with those
// names are compared.
//
// NOTE: Be mindful of accidental discrepancies between expected and metricNames; metricNames filter
// both expected and gathered metrics. See https://github.com/prometheus/client_golang/issues/1351.
func GatherAndCompare(g prometheus.Gatherer, expected io.Reader, metricNames ...string) error {
	return TransactionalGatherAndCompare(prometheus.ToTransactionalGatherer(g), expected, metricNames...)
}

// TransactionalGatherAndCompare gathers all metrics from the provided Gatherer and compares
// it to an expected output read from the provided Reader in the Prometheus text
// exposition format. If any metricNames are provided, only metrics with those
// names are compared.
//
// NOTE: Be mindful of accidental discrepancies between expected and metricNames; metricNames filter
// both expected and gathered metrics. See https://github.com/prometheus/client_golang/issues/1351.
func TransactionalGatherAndCompare(g prometheus.TransactionalGatherer, expected io.Reader, metricNames ...string) error {
	got, done, err := g.Gather()
	defer done()
	if err != nil {
		return fmt.Errorf("gathering metrics failed: %w", err)
	}

	wanted, err := convertReaderToMetricFamily(expected)
	if err != nil {
		return err
	}

	return compareMetricFamilies(got, wanted, metricNames...)
}

// CollectAndFormat collects the metrics identified by `metricNames` and returns them in the given format.
func CollectAndFormat(c prometheus.Collector, format expfmt.FormatType, metricNames ...string) ([]byte, error) {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		return nil, fmt.Errorf("registering collector failed: %w", err)
	}

	gotFiltered, err := reg.Gather()
	if err != nil {
		return nil, fmt.Errorf("gathering metrics failed: %w", err)
	}

	gotFiltered = filterMetrics(gotFiltered, metricNames)

	var gotFormatted bytes.Buffer
	enc := expfmt.NewEncoder(&gotFormatted, expfmt.NewFormat(format))
	for _, mf := range gotFiltered {
		if err := enc.Encode(mf); err != nil {
			return nil, fmt.Errorf("encoding gathered metrics failed: %w", err)
		}
	}

	return gotFormatted.Bytes(), nil
}

// convertReaderToMetricFamily would read from a io.Reader object and convert it to a slice of
// dto.MetricFamily.
func convertReaderToMetricFamily(reader io.Reader) ([]*dto.MetricFamily, error) {
	var tp expfmt.TextParser
	notNormalized, err := tp.TextToMetricFamilies(reader)
	if err != nil {
		return nil, fmt.Errorf("converting reader to metric families failed: %w", err)
	}

	// The text protocol handles empty help fields inconsistently. When
	// encoding, any non-nil value, include the empty string, produces a
	// "# HELP" line. But when decoding, the help field is only set to a
	// non-nil value if the "# HELP" line contains a non-empty value.
	//
	// Because metrics in a registry always have non-nil help fields, populate
	// any nil help fields in the parsed metrics with the empty string so that
	// when we compare text encodings, the results are consistent.
	for _, metric := range notNormalized {
		if metric.Help == nil {
			metric.Help = proto.String("")
		}
	}

	return internal.NormalizeMetricFamilies(notNormalized), nil
}

// compareMetricFamilies would compare 2 slices of metric families, and optionally filters both of
// them to the `metricNames` provided.
func compareMetricFamilies(got, expected []*dto.MetricFamily, metricNames ...string) error {
	if metricNames != nil {
		got = filterMetrics(got, metricNames)
		expected = filterMetrics(expected, metricNames)
	}

	return compare(got, expected)
}

// compare encodes both provided slices of metric families into the text format,
// compares their string message, and returns an error if they do not match.
// The error contains the encoded text of both the desired and the actual
// result.
func compare(got, want []*dto.MetricFamily) error {
	var gotBuf, wantBuf bytes.Buffer
	enc := expfmt.NewEncoder(&gotBuf, expfmt.NewFormat(expfmt.TypeTextPlain).WithEscapingScheme(model.NoEscaping))
	for _, mf := range got {
		if err := enc.Encode(mf); err != nil {
			return fmt.Errorf("encoding gathered metrics failed: %w", err)
		}
	}
	enc = expfmt.NewEncoder(&wantBuf, expfmt.NewFormat(expfmt.TypeTextPlain).WithEscapingScheme(model.NoEscaping))
	for _, mf := range want {
		if err := enc.Encode(mf); err != nil {
			return fmt.Errorf("encoding expected metrics failed: %w", err)
		}
	}
	if diffErr := diff.Diff(gotBuf.String(), wantBuf.String()); diffErr != "" {
		return errors.New(diffErr)
	}
	return nil
}

func filterMetrics(metrics []*dto.MetricFamily, names []string) []*dto.MetricFamily {
	var filtered []*dto.MetricFamily
	for _, m := range metrics {
		for _, name := range names {
			if m.GetName() == name {
				filtered = append(filtered, m)
				break
			}
		}
	}
	return filtered
}
