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
	"testing"

	"github.com/blang/semver/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	apimachineryversion "k8s.io/apimachinery/pkg/version"
)

func TestHistogram(t *testing.T) {
	v115 := semver.MustParse("1.15.0")
	var tests = []struct {
		desc string
		*HistogramOpts
		registryVersion     *semver.Version
		expectedMetricCount int
		expectedHelp        string
	}{
		{
			desc: "Test non deprecated",
			HistogramOpts: &HistogramOpts{
				Namespace: "namespace",
				Name:      "metric_test_name",
				Subsystem: "subsystem",
				Help:      "histogram help message",
				Buckets:   prometheus.DefBuckets,
			},
			registryVersion:     &v115,
			expectedMetricCount: 1,
			expectedHelp:        "[ALPHA] histogram help message",
		},
		{
			desc: "Test deprecated",
			HistogramOpts: &HistogramOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "histogram help message",
				DeprecatedVersion: "1.15.0",
				Buckets:           prometheus.DefBuckets,
			},
			registryVersion:     &v115,
			expectedMetricCount: 1,
			expectedHelp:        "[ALPHA] (Deprecated since 1.15.0) histogram help message",
		},
		{
			desc: "Test hidden",
			HistogramOpts: &HistogramOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "histogram help message",
				DeprecatedVersion: "1.14.0",
				Buckets:           prometheus.DefBuckets,
			},
			registryVersion:     &v115,
			expectedMetricCount: 0,
			expectedHelp:        "histogram help message",
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			registry := newKubeRegistry(apimachineryversion.Info{
				Major:      "1",
				Minor:      "15",
				GitVersion: "v1.15.0-alpha-1.12345",
			})
			c := NewHistogram(test.HistogramOpts)
			registry.MustRegister(c)
			cm := c.ObserverMetric.(prometheus.Metric)

			metricChan := make(chan prometheus.Metric, 2)
			c.Collect(metricChan)
			close(metricChan)
			m1 := <-metricChan
			if m1 != cm {
				t.Error("Unexpected metric", m1, cm)
			}
			m2, ok := <-metricChan
			if ok {
				t.Error("Unexpected second metric", m2)
			}

			ms, err := registry.Gather()
			assert.Lenf(t, ms, test.expectedMetricCount, "Got %v metrics, Want: %v metrics", len(ms), test.expectedMetricCount)
			require.NoError(t, err, "Gather failed %v", err)

			for _, metric := range ms {
				assert.Equalf(t, test.expectedHelp, metric.GetHelp(), "Got %s as help message, want %s", metric.GetHelp(), test.expectedHelp)
			}

			// let's increment the counter and verify that the metric still works
			c.Observe(1)
			c.Observe(2)
			c.Observe(3)
			c.Observe(1.5)
			expected := 4
			ms, err = registry.Gather()
			require.NoError(t, err, "Gather failed %v", err)

			for _, mf := range ms {
				for _, m := range mf.GetMetric() {
					assert.Equalf(t, expected, int(m.GetHistogram().GetSampleCount()), "Got %v, want %v as the sample count", m.GetHistogram().GetSampleCount(), expected)
				}
			}
		})
	}
}

func TestHistogramVec(t *testing.T) {
	v115 := semver.MustParse("1.15.0")
	var tests = []struct {
		desc string
		*HistogramOpts
		labels              []string
		registryVersion     *semver.Version
		expectedMetricCount int
		expectedHelp        string
	}{
		{
			desc: "Test non deprecated",
			HistogramOpts: &HistogramOpts{
				Namespace: "namespace",
				Name:      "metric_test_name",
				Subsystem: "subsystem",
				Help:      "histogram help message",
				Buckets:   prometheus.DefBuckets,
			},
			labels:              []string{"label_a", "label_b"},
			registryVersion:     &v115,
			expectedMetricCount: 1,
			expectedHelp:        "[ALPHA] histogram help message",
		},
		{
			desc: "Test deprecated",
			HistogramOpts: &HistogramOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "histogram help message",
				DeprecatedVersion: "1.15.0",
				Buckets:           prometheus.DefBuckets,
			},
			labels:              []string{"label_a", "label_b"},
			registryVersion:     &v115,
			expectedMetricCount: 1,
			expectedHelp:        "[ALPHA] (Deprecated since 1.15.0) histogram help message",
		},
		{
			desc: "Test hidden",
			HistogramOpts: &HistogramOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "histogram help message",
				DeprecatedVersion: "1.14.0",
				Buckets:           prometheus.DefBuckets,
			},
			labels:              []string{"label_a", "label_b"},
			registryVersion:     &v115,
			expectedMetricCount: 0,
			expectedHelp:        "histogram help message",
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			registry := newKubeRegistry(apimachineryversion.Info{
				Major:      "1",
				Minor:      "15",
				GitVersion: "v1.15.0-alpha-1.12345",
			})
			c := NewHistogramVec(test.HistogramOpts, test.labels)
			registry.MustRegister(c)
			ov12 := c.WithLabelValues("1", "2")
			cm1 := ov12.(prometheus.Metric)
			ov12.Observe(1.0)

			if test.expectedMetricCount > 0 {
				metricChan := make(chan prometheus.Metric, 2)
				c.Collect(metricChan)
				close(metricChan)
				m1 := <-metricChan
				if m1 != cm1 {
					t.Error("Unexpected metric", m1, cm1)
				}
				m2, ok := <-metricChan
				if ok {
					t.Error("Unexpected second metric", m2)
				}
			}

			ms, err := registry.Gather()
			assert.Lenf(t, ms, test.expectedMetricCount, "Got %v metrics, Want: %v metrics", len(ms), test.expectedMetricCount)
			require.NoError(t, err, "Gather failed %v", err)
			for _, metric := range ms {
				if metric.GetHelp() != test.expectedHelp {
					assert.Equalf(t, test.expectedHelp, metric.GetHelp(), "Got %s as help message, want %s", metric.GetHelp(), test.expectedHelp)
				}
			}

			// let's increment the counter and verify that the metric still works
			c.WithLabelValues("1", "3").Observe(1.0)
			c.WithLabelValues("2", "3").Observe(1.0)
			ms, err = registry.Gather()
			require.NoError(t, err, "Gather failed %v", err)

			for _, mf := range ms {
				assert.Lenf(t, mf.GetMetric(), 3, "Got %v metrics, wanted 3 as the count", len(mf.GetMetric()))
				for _, m := range mf.GetMetric() {
					assert.Equalf(t, uint64(1), m.GetHistogram().GetSampleCount(), "Got %v metrics, expected histogram sample count to equal 1", m.GetHistogram().GetSampleCount())
				}
			}
		})
	}
}

func TestHistogramWithLabelValueAllowList(t *testing.T) {
	labelAllowValues := map[string]string{
		"namespace_subsystem_metric_allowlist_test,label_a": "allowed",
	}
	labels := []string{"label_a", "label_b"}
	opts := &HistogramOpts{
		Namespace: "namespace",
		Name:      "metric_allowlist_test",
		Subsystem: "subsystem",
	}
	var tests = []struct {
		desc               string
		labelValues        [][]string
		expectMetricValues map[string]uint64
	}{
		{
			desc:        "Test no unexpected input",
			labelValues: [][]string{{"allowed", "b1"}, {"allowed", "b2"}},
			expectMetricValues: map[string]uint64{
				"allowed b1": 1.0,
				"allowed b2": 1.0,
			},
		},
		{
			desc:        "Test unexpected input",
			labelValues: [][]string{{"allowed", "b1"}, {"not_allowed", "b1"}},
			expectMetricValues: map[string]uint64{
				"allowed b1":    1.0,
				"unexpected b1": 1.0,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			labelValueAllowLists = map[string]*MetricLabelAllowList{}
			registry := newKubeRegistry(apimachineryversion.Info{
				Major:      "1",
				Minor:      "15",
				GitVersion: "v1.15.0-alpha-1.12345",
			})
			c := NewHistogramVec(opts, labels)
			registry.MustRegister(c)
			SetLabelAllowListFromCLI(labelAllowValues)

			for _, lv := range test.labelValues {
				c.WithLabelValues(lv...).Observe(1.0)
			}
			mfs, err := registry.Gather()
			require.NoError(t, err, "Gather failed %v", err)

			for _, mf := range mfs {
				if *mf.Name != BuildFQName(opts.Namespace, opts.Subsystem, opts.Name) {
					continue
				}
				mfMetric := mf.GetMetric()

				for _, m := range mfMetric {
					var aValue, bValue string
					for _, l := range m.Label {
						if *l.Name == "label_a" {
							aValue = *l.Value
						}
						if *l.Name == "label_b" {
							bValue = *l.Value
						}
					}
					labelValuePair := aValue + " " + bValue
					expectedValue, ok := test.expectMetricValues[labelValuePair]
					assert.True(t, ok, "Got unexpected label values, lable_a is %v, label_b is %v", aValue, bValue)
					actualValue := m.GetHistogram().GetSampleCount()
					assert.Equalf(t, expectedValue, actualValue, "Got %v, wanted %v as the count while setting label_a to %v and label b to %v", actualValue, expectedValue, aValue, bValue)
				}
			}
		})
	}
}

func TestHistogramWithExemplar(t *testing.T) {
	// Create context.
	traceID := trace.TraceID([]byte("trace-0000-xxxxx"))
	spanID := trace.SpanID([]byte("span-0000-xxxxx"))
	ctxForSpanCtx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))
	value := float64(10)

	// Create contextual histogram.
	histogram := NewHistogram(&HistogramOpts{
		Name:    "histogram_exemplar_test",
		Help:    "helpless",
		Buckets: []float64{100},
	}).WithContext(ctxForSpanCtx)

	// Register histogram.
	registry := newKubeRegistry(apimachineryversion.Info{
		Major:      "1",
		Minor:      "15",
		GitVersion: "v1.15.0-alpha-1.12345",
	})
	registry.MustRegister(histogram)

	// Call underlying exemplar methods.
	histogram.Observe(value)

	// Gather.
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed %v", err)
	}
	if len(mfs) != 1 {
		t.Fatalf("Got %v metric families, Want: 1 metric family", len(mfs))
	}

	// Verify metric type.
	mf := mfs[0]
	var m *dto.Metric
	switch mf.GetType() {
	case dto.MetricType_HISTOGRAM:
		m = mfs[0].GetMetric()[0]
	default:
		t.Fatalf("Got %v metric type, Want: %v metric type", mf.GetType(), dto.MetricType_HISTOGRAM)
	}

	// Verify value.
	want := value
	got := m.GetHistogram().GetSampleSum()
	if got != want {
		t.Fatalf("Got %f, wanted %f as the count", got, want)
	}

	// Verify exemplars.
	buckets := m.GetHistogram().GetBucket()
	if len(buckets) == 0 {
		t.Fatalf("Got 0 buckets, wanted 1")
	}
	e := buckets[0].GetExemplar()
	if e == nil {
		t.Fatalf("Got nil exemplar, wanted an exemplar")
	}
	eLabels := e.GetLabel()
	if eLabels == nil {
		t.Fatalf("Got nil exemplar label, wanted an exemplar label")
	}
	if len(eLabels) != 2 {
		t.Fatalf("Got %v exemplar labels, wanted 2 exemplar labels", len(eLabels))
	}
	for _, l := range eLabels {
		switch *l.Name {
		case "trace_id":
			if *l.Value != traceID.String() {
				t.Fatalf("Got %s as traceID, wanted %s", *l.Value, traceID.String())
			}
		case "span_id":
			if *l.Value != spanID.String() {
				t.Fatalf("Got %s as spanID, wanted %s", *l.Value, spanID.String())
			}
		default:
			t.Fatalf("Got unexpected label %s", *l.Name)
		}
	}

	// Verify that all contextual histogram calls are exclusive.
	contextualHistogram := NewHistogram(&HistogramOpts{
		Name:    "contextual_histogram",
		Help:    "helpless",
		Buckets: []float64{100},
	})
	traceIDa := trace.TraceID([]byte("trace-0000-aaaaa"))
	spanIDa := trace.SpanID([]byte("span-0000-aaaaa"))
	contextualHistogramA := contextualHistogram.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceIDa,
			SpanID:     spanIDa,
			TraceFlags: trace.FlagsSampled,
		}),
	))
	traceIDb := trace.TraceID([]byte("trace-0000-bbbbb"))
	spanIDb := trace.SpanID([]byte("span-0000-bbbbb"))
	contextualHistogramB := contextualHistogram.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceIDb,
			SpanID:     spanIDb,
			TraceFlags: trace.FlagsSampled,
		}),
	))

	runs := []struct {
		spanID              trace.SpanID
		traceID             trace.TraceID
		contextualHistogram *HistogramWithContext
	}{
		{
			spanID:              spanIDa,
			traceID:             traceIDa,
			contextualHistogram: contextualHistogramA,
		},
		{
			spanID:              spanIDb,
			traceID:             traceIDb,
			contextualHistogram: contextualHistogramB,
		},
	}
	for _, run := range runs {
		registry.MustRegister(run.contextualHistogram)
		run.contextualHistogram.Observe(value)

		mfs, err = registry.Gather()
		if err != nil {
			t.Fatalf("Gather failed %v", err)
		}
		if len(mfs) != 2 {
			t.Fatalf("Got %v metric families, Want: 2 metric families", len(mfs))
		}

		dtoMetric := mfs[0].GetMetric()[0]
		dtoMetricBuckets := dtoMetric.GetHistogram().GetBucket()
		if len(dtoMetricBuckets) == 0 {
			t.Fatalf("Got nil buckets")
		}
		dtoMetricBucketsExemplar := dtoMetricBuckets[0].GetExemplar()
		if dtoMetricBucketsExemplar == nil {
			t.Fatalf("Got nil exemplar")
		}

		dtoMetricLabels := dtoMetricBucketsExemplar.GetLabel()
		if len(dtoMetricLabels) != 2 {
			t.Fatalf("Got %v exemplar labels, wanted 2 exemplar labels", len(dtoMetricLabels))
		}
		for _, l := range dtoMetricLabels {
			switch *l.Name {
			case "trace_id":
				if *l.Value != run.traceID.String() {
					t.Fatalf("Got %s as traceID, wanted %s", *l.Value, run.traceID.String())
				}
			case "span_id":
				if *l.Value != run.spanID.String() {
					t.Fatalf("Got %s as spanID, wanted %s", *l.Value, run.spanID.String())
				}
			default:
				t.Fatalf("Got unexpected label %s", *l.Name)
			}
		}

		registry.Unregister(run.contextualHistogram)
	}
}

func TestHistogramVecWithExemplar(t *testing.T) {
	// Create context.
	traceID := trace.TraceID([]byte("trace-0000-xxxxx"))
	spanID := trace.SpanID([]byte("span-0000-xxxxx"))
	ctxForSpanCtx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))
	value := float64(10)

	// Create contextual histogram.
	histogramVec := NewHistogramVec(&HistogramOpts{
		Name:    "histogram_exemplar_test",
		Help:    "helpless",
		Buckets: []float64{100},
	}, []string{"group"})
	h := histogramVec.WithContext(ctxForSpanCtx)

	// Register histogram.
	registry := newKubeRegistry(apimachineryversion.Info{
		Major:      "1",
		Minor:      "15",
		GitVersion: "v1.15.0-alpha-1.12345",
	})
	registry.MustRegister(histogramVec)

	// Call underlying exemplar methods.
	h.WithLabelValues("foo").Observe(value)

	// Gather.
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed %v", err)
	}
	if len(mfs) != 1 {
		t.Fatalf("Got %v metric families, Want: 1 metric family", len(mfs))
	}

	// Verify metric type.
	mf := mfs[0]
	var m *dto.Metric
	switch mf.GetType() {
	case dto.MetricType_HISTOGRAM:
		m = mfs[0].GetMetric()[0]
	default:
		t.Fatalf("Got %v metric type, Want: %v metric type", mf.GetType(), dto.MetricType_HISTOGRAM)
	}

	// Verify value.
	want := value
	got := m.GetHistogram().GetSampleSum()
	if got != want {
		t.Fatalf("Got %f, wanted %f as the count", got, want)
	}

	// Verify exemplars.
	buckets := m.GetHistogram().GetBucket()
	if len(buckets) == 0 {
		t.Fatalf("Got 0 buckets, wanted 1")
	}
	e := buckets[0].GetExemplar()
	if e == nil {
		t.Fatalf("Got nil exemplar, wanted an exemplar")
	}
	eLabels := e.GetLabel()
	if eLabels == nil {
		t.Fatalf("Got nil exemplar label, wanted an exemplar label")
	}
	if len(eLabels) != 2 {
		t.Fatalf("Got %v exemplar labels, wanted 2 exemplar labels", len(eLabels))
	}
	for _, l := range eLabels {
		switch *l.Name {
		case "trace_id":
			if *l.Value != traceID.String() {
				t.Fatalf("Got %s as traceID, wanted %s", *l.Value, traceID.String())
			}
		case "span_id":
			if *l.Value != spanID.String() {
				t.Fatalf("Got %s as spanID, wanted %s", *l.Value, spanID.String())
			}
		default:
			t.Fatalf("Got unexpected label %s", *l.Name)
		}
	}

	// Verify that all contextual histogram calls are exclusive.
	contextualHistogramVec := NewHistogramVec(&HistogramOpts{
		Name:    "contextual_histogram_vec",
		Help:    "helpless",
		Buckets: []float64{100},
	}, []string{"group"})
	traceIDa := trace.TraceID([]byte("trace-0000-aaaaa"))
	spanIDa := trace.SpanID([]byte("span-0000-aaaaa"))
	contextualHistogramVecA := contextualHistogramVec.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceIDa,
			SpanID:     spanIDa,
			TraceFlags: trace.FlagsSampled,
		}),
	))
	traceIDb := trace.TraceID([]byte("trace-0000-bbbbb"))
	spanIDb := trace.SpanID([]byte("span-0000-bbbbb"))
	contextualHistogramVecB := contextualHistogramVec.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceIDb,
			SpanID:     spanIDb,
			TraceFlags: trace.FlagsSampled,
		}),
	))
	runs := []struct {
		spanID                 trace.SpanID
		traceID                trace.TraceID
		contextualHistogramVec *HistogramVecWithContext
	}{
		{
			spanID:                 spanIDa,
			traceID:                traceIDa,
			contextualHistogramVec: contextualHistogramVecA,
		}, {
			spanID:                 spanIDb,
			traceID:                traceIDb,
			contextualHistogramVec: contextualHistogramVecB,
		},
	}
	for _, run := range runs {
		registry.MustRegister(run.contextualHistogramVec)
		run.contextualHistogramVec.WithLabelValues("foo").Observe(value)

		mfs, err = registry.Gather()
		if err != nil {
			t.Fatalf("Gather failed %v", err)
		}
		if len(mfs) != 2 {
			t.Fatalf("Got %v metric families, Want: 2 metric families", len(mfs))
		}

		dtoMetric := mfs[0].GetMetric()[0]
		dtoMetricBuckets := dtoMetric.GetHistogram().GetBucket()
		if len(dtoMetricBuckets) == 0 {
			t.Fatalf("Got nil buckets")
		}
		dtoMetricBucketsExemplar := dtoMetricBuckets[0].GetExemplar()
		if dtoMetricBucketsExemplar == nil {
			t.Fatalf("Got nil exemplar")
		}

		dtoMetricLabels := dtoMetricBucketsExemplar.GetLabel()
		if len(dtoMetricLabels) != 2 {
			t.Fatalf("Got %v exemplar labels, wanted 2 exemplar labels", len(dtoMetricLabels))
		}
		for _, l := range dtoMetricLabels {
			switch *l.Name {
			case "trace_id":
				if *l.Value != run.traceID.String() {
					t.Fatalf("Got %s as traceID, wanted %s", *l.Value, run.traceID.String())
				}
			case "span_id":
				if *l.Value != run.spanID.String() {
					t.Fatalf("Got %s as spanID, wanted %s", *l.Value, run.spanID.String())
				}
			default:
				t.Fatalf("Got unexpected label %s", *l.Name)
			}
		}

		registry.Unregister(run.contextualHistogramVec)
	}
}
