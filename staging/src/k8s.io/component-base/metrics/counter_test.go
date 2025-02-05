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
	"bytes"
	"context"
	"testing"

	"github.com/blang/semver/v4"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	apimachineryversion "k8s.io/apimachinery/pkg/version"
)

func TestCounter(t *testing.T) {
	var tests = []struct {
		desc string
		*CounterOpts
		expectedMetricCount int
		expectedHelp        string
	}{
		{
			desc: "Test non deprecated",
			CounterOpts: &CounterOpts{
				Namespace:      "namespace",
				Name:           "metric_test_name",
				Subsystem:      "subsystem",
				StabilityLevel: ALPHA,
				Help:           "counter help",
			},
			expectedMetricCount: 1,
			expectedHelp:        "[ALPHA] counter help",
		},
		{
			desc: "Test deprecated",
			CounterOpts: &CounterOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "counter help",
				StabilityLevel:    ALPHA,
				DeprecatedVersion: "1.15.0",
			},
			expectedMetricCount: 1,
			expectedHelp:        "[ALPHA] (Deprecated since 1.15.0) counter help",
		},
		{
			desc: "Test hidden",
			CounterOpts: &CounterOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "counter help",
				StabilityLevel:    ALPHA,
				DeprecatedVersion: "1.14.0",
			},
			expectedMetricCount: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			registry := newKubeRegistry(apimachineryversion.Info{
				Major:      "1",
				Minor:      "15",
				GitVersion: "v1.15.0-alpha-1.12345",
			})
			// c is a pointer to a Counter
			c := NewCounter(test.CounterOpts)
			registry.MustRegister(c)
			// mfs is a pointer to a dto.MetricFamily slice
			mfs, err := registry.Gather()
			var buf bytes.Buffer
			enc := expfmt.NewEncoder(&buf, "text/plain; version=0.0.4; charset=utf-8")
			assert.Lenf(t, mfs, test.expectedMetricCount, "Got %v metrics, Want: %v metrics", len(mfs), test.expectedMetricCount)
			require.NoError(t, err, "Gather failed %v", err)
			for _, metric := range mfs {
				err := enc.Encode(metric)
				require.NoError(t, err, "Unexpected err %v in encoding the metric", err)
				assert.Equalf(t, test.expectedHelp, metric.GetHelp(), "Got %s as help message, want %s", metric.GetHelp(), test.expectedHelp)
			}

			// increment the counter N number of times and verify that the metric retains the count correctly
			numberOfTimesToIncrement := 3
			for i := 0; i < numberOfTimesToIncrement; i++ {
				c.Inc()
			}
			mfs, err = registry.Gather()
			require.NoError(t, err, "Gather failed %v", err)

			for _, mf := range mfs {
				mfMetric := mf.GetMetric()
				for _, m := range mfMetric {
					assert.Equalf(t, numberOfTimesToIncrement, int(m.GetCounter().GetValue()), "Got %v, wanted %v as the count", m.GetCounter().GetValue(), numberOfTimesToIncrement)
				}
			}
		})
	}
}

func TestCounterVec(t *testing.T) {
	var tests = []struct {
		desc string
		*CounterOpts
		labels                    []string
		registryVersion           *semver.Version
		expectedMetricFamilyCount int
		expectedHelp              string
	}{
		{
			desc: "Test non deprecated",
			CounterOpts: &CounterOpts{
				Namespace: "namespace",
				Name:      "metric_test_name",
				Subsystem: "subsystem",
				Help:      "counter help",
			},
			labels:                    []string{"label_a", "label_b"},
			expectedMetricFamilyCount: 1,
			expectedHelp:              "[ALPHA] counter help",
		},
		{
			desc: "Test deprecated",
			CounterOpts: &CounterOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "counter help",
				DeprecatedVersion: "1.15.0",
			},
			labels:                    []string{"label_a", "label_b"},
			expectedMetricFamilyCount: 1,
			expectedHelp:              "[ALPHA] (Deprecated since 1.15.0) counter help",
		},
		{
			desc: "Test hidden",
			CounterOpts: &CounterOpts{
				Namespace:         "namespace",
				Name:              "metric_test_name",
				Subsystem:         "subsystem",
				Help:              "counter help",
				DeprecatedVersion: "1.14.0",
			},
			labels:                    []string{"label_a", "label_b"},
			expectedMetricFamilyCount: 0,
			expectedHelp:              "counter help",
		},
		{
			desc: "Test alpha",
			CounterOpts: &CounterOpts{
				StabilityLevel: ALPHA,
				Namespace:      "namespace",
				Name:           "metric_test_name",
				Subsystem:      "subsystem",
				Help:           "counter help",
			},
			labels:                    []string{"label_a", "label_b"},
			expectedMetricFamilyCount: 1,
			expectedHelp:              "[ALPHA] counter help",
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			registry := newKubeRegistry(apimachineryversion.Info{
				Major:      "1",
				Minor:      "15",
				GitVersion: "v1.15.0-alpha-1.12345",
			})
			c := NewCounterVec(test.CounterOpts, test.labels)
			registry.MustRegister(c)
			c.WithLabelValues("1", "2").Inc()
			mfs, err := registry.Gather()
			assert.Lenf(t, mfs, test.expectedMetricFamilyCount, "Got %v metric families, Want: %v metric families", len(mfs), test.expectedMetricFamilyCount)
			require.NoError(t, err, "Gather failed %v", err)

			// this no-opts here when there are no metric families (i.e. when the metric is hidden)
			for _, mf := range mfs {
				assert.Lenf(t, mf.GetMetric(), 1, "Got %v metrics, wanted 1 as the count", len(mf.GetMetric()))
				assert.Equalf(t, test.expectedHelp, mf.GetHelp(), "Got %s as help message, want %s", mf.GetHelp(), test.expectedHelp)
			}

			// let's increment the counter and verify that the metric still works
			c.WithLabelValues("1", "3").Inc()
			c.WithLabelValues("2", "3").Inc()
			mfs, err = registry.Gather()
			require.NoError(t, err, "Gather failed %v", err)

			// this no-opts here when there are no metric families (i.e. when the metric is hidden)
			for _, mf := range mfs {
				assert.Lenf(t, mf.GetMetric(), 3, "Got %v metrics, wanted 3 as the count", len(mf.GetMetric()))
			}
		})
	}
}

func TestCounterWithLabelValueAllowList(t *testing.T) {
	labelAllowValues := map[string]string{
		"namespace_subsystem_metric_allowlist_test,label_a": "allowed",
	}
	labels := []string{"label_a", "label_b"}
	opts := &CounterOpts{
		Namespace: "namespace",
		Name:      "metric_allowlist_test",
		Subsystem: "subsystem",
	}
	var tests = []struct {
		desc               string
		labelValues        [][]string
		expectMetricValues map[string]int
	}{
		{
			desc:        "Test no unexpected input",
			labelValues: [][]string{{"allowed", "b1"}, {"allowed", "b2"}},
			expectMetricValues: map[string]int{
				"allowed b1": 1,
				"allowed b2": 1,
			},
		},
		{
			desc:        "Test unexpected input",
			labelValues: [][]string{{"allowed", "b1"}, {"not_allowed", "b1"}},
			expectMetricValues: map[string]int{
				"allowed b1":    1,
				"unexpected b1": 1,
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
			c := NewCounterVec(opts, labels)
			registry.MustRegister(c)
			SetLabelAllowListFromCLI(labelAllowValues)
			for _, lv := range test.labelValues {
				c.WithLabelValues(lv...).Inc()
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
					actualValue := int(m.GetCounter().GetValue())
					assert.Equalf(t, expectedValue, actualValue, "Got %v, wanted %v as the count while setting label_a to %v and label b to %v", actualValue, expectedValue, aValue, bValue)
				}
			}
		})
	}
}

func TestCounterWithExemplar(t *testing.T) {
	// Create context.
	fn := func(offset int) []byte {
		arr := make([]byte, 16)
		for i := 0; i < 16; i++ {
			arr[i] = byte(2<<7 - i - offset)
		}
		return arr
	}
	traceID := trace.TraceID(fn(1))
	spanID := trace.SpanID(fn(2))
	ctxForSpanCtx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		SpanID:     spanID,
		TraceID:    traceID,
		TraceFlags: trace.FlagsSampled,
	}))
	toAdd := float64(40)

	// Create contextual counter.
	counter := NewCounter(&CounterOpts{
		Name: "metric_exemplar_test",
		Help: "helpless",
	}).WithContext(ctxForSpanCtx)

	// Register counter.
	registry := newKubeRegistry(apimachineryversion.Info{
		Major:      "1",
		Minor:      "15",
		GitVersion: "v1.15.0-alpha-1.12345",
	})
	registry.MustRegister(counter)

	// Call underlying exemplar methods.
	counter.Add(toAdd)
	counter.Inc()
	counter.Inc()

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
	case dto.MetricType_COUNTER:
		m = mfs[0].GetMetric()[0]
	default:
		t.Fatalf("Got %v metric type, Want: %v metric type", mf.GetType(), dto.MetricType_COUNTER)
	}

	// Verify value.
	want := toAdd + 2
	got := m.GetCounter().GetValue()
	if got != want {
		t.Fatalf("Got %f, wanted %f as the count", got, want)
	}

	// Verify exemplars.
	e := m.GetCounter().GetExemplar()
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

	// Verify that all contextual counter calls are exclusive.
	contextualCounter := NewCounter(&CounterOpts{
		Name: "contextual_counter",
		Help: "helpless",
	})
	spanIDa := trace.SpanID(fn(3))
	traceIDa := trace.TraceID(fn(4))
	contextualCounterA := contextualCounter.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			SpanID:     spanIDa,
			TraceID:    traceIDa,
			TraceFlags: trace.FlagsSampled,
		}),
	))
	spanIDb := trace.SpanID(fn(5))
	traceIDb := trace.TraceID(fn(6))
	contextualCounterB := contextualCounter.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			SpanID:     spanIDb,
			TraceID:    traceIDb,
			TraceFlags: trace.FlagsSampled,
		}),
	))

	runs := []struct {
		spanID            trace.SpanID
		traceID           trace.TraceID
		contextualCounter *CounterWithContext
	}{
		{
			spanID:            spanIDa,
			traceID:           traceIDa,
			contextualCounter: contextualCounterA,
		},
		{
			spanID:            spanIDb,
			traceID:           traceIDb,
			contextualCounter: contextualCounterB,
		},
	}
	for _, run := range runs {
		registry.MustRegister(run.contextualCounter)
		run.contextualCounter.Inc()

		mfs, err = registry.Gather()
		if err != nil {
			t.Fatalf("Gather failed %v", err)
		}
		if len(mfs) != 2 {
			t.Fatalf("Got %v metric families, Want: 2 metric families", len(mfs))
		}

		dtoMetric := mfs[0].GetMetric()[0]
		if dtoMetric.GetCounter().GetExemplar() == nil {
			t.Fatalf("Got nil exemplar, wanted an exemplar")
		}
		dtoMetricLabels := dtoMetric.GetCounter().GetExemplar().GetLabel()
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

		registry.Unregister(run.contextualCounter)
	}
}

func TestCounterVecWithExemplar(t *testing.T) {
	// Create context.
	fn := func(offset int) []byte {
		arr := make([]byte, 16)
		for i := 0; i < 16; i++ {
			arr[i] = byte(2<<7 - i - offset)
		}
		return arr
	}
	traceID := trace.TraceID(fn(1))
	spanID := trace.SpanID(fn(2))
	ctxForSpanCtx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			SpanID:     spanID,
			TraceID:    traceID,
			TraceFlags: trace.FlagsSampled,
		}),
	)
	toAdd := float64(40)

	// Create contextual counter.
	counter := NewCounterVec(&CounterOpts{
		Name: "metricvec_exemplar_test",
		Help: "helpless",
	}, []string{"label_a"}).WithContext(ctxForSpanCtx)

	// Register counter.
	registry := newKubeRegistry(apimachineryversion.Info{
		Major:      "1",
		Minor:      "15",
		GitVersion: "v1.15.0-alpha-1.12345",
	})
	registry.MustRegister(counter)

	// Call underlying exemplar methods.
	counter.WithLabelValues("a").Add(toAdd)
	counter.WithLabelValues("a").Inc()
	counter.WithLabelValues("a").Inc()

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
	case dto.MetricType_COUNTER:
		m = mfs[0].GetMetric()[0]
	default:
		t.Fatalf("Got %v metric type, Want: %v metric type", mf.GetType(), dto.MetricType_COUNTER)
	}

	// Verify value.
	want := toAdd + 2
	got := m.GetCounter().GetValue()
	if got != want {
		t.Fatalf("Got %f, wanted %f as the count", got, want)
	}

	// Verify exemplars.
	e := m.GetCounter().GetExemplar()
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

	// Verify that all contextual counter calls are exclusive.
	contextualCounterVec := NewCounterVec(&CounterOpts{
		Name: "contextual_counter",
		Help: "helpless",
	}, []string{"label_a"})
	spanIDa := trace.SpanID(fn(3))
	traceIDa := trace.TraceID(fn(4))
	contextualCounterVecA := contextualCounterVec.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			SpanID:     spanIDa,
			TraceID:    traceIDa,
			TraceFlags: trace.FlagsSampled,
		}),
	))
	spanIDb := trace.SpanID(fn(5))
	traceIDb := trace.TraceID(fn(6))
	contextualCounterVecB := contextualCounterVec.WithContext(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			SpanID:     spanIDb,
			TraceID:    traceIDb,
			TraceFlags: trace.FlagsSampled,
		}),
	))

	runs := []struct {
		spanID            trace.SpanID
		traceID           trace.TraceID
		contextualCounter *CounterVecWithContext
	}{
		{
			spanID:            spanIDa,
			traceID:           traceIDa,
			contextualCounter: contextualCounterVecA,
		},
		{
			spanID:            spanIDb,
			traceID:           traceIDb,
			contextualCounter: contextualCounterVecB,
		},
	}
	for _, run := range runs {
		registry.MustRegister(run.contextualCounter)
		run.contextualCounter.WithLabelValues("a").Inc()

		mfs, err = registry.Gather()
		if err != nil {
			t.Fatalf("Gather failed %v", err)
		}
		if len(mfs) != 2 {
			t.Fatalf("Got %v metric families, Want: 2 metric families", len(mfs))
		}

		dtoMetric := mfs[0].GetMetric()[0]
		if dtoMetric.GetCounter().GetExemplar() == nil {
			t.Fatalf("Got nil exemplar, wanted an exemplar")
		}
		dtoMetricLabels := dtoMetric.GetCounter().GetExemplar().GetLabel()
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

		registry.Unregister(run.contextualCounter)
	}
}
