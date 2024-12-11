// Copyright 2021 The Prometheus Authors
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

//go:build go1.17
// +build go1.17

package collectors

import (
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/internal"
)

var (
	// MetricsAll allows all the metrics to be collected from Go runtime.
	MetricsAll = GoRuntimeMetricsRule{regexp.MustCompile("/.*")}
	// MetricsGC allows only GC metrics to be collected from Go runtime.
	// e.g. go_gc_cycles_automatic_gc_cycles_total
	// NOTE: This does not include new class of "/cpu/classes/gc/..." metrics.
	// Use custom metric rule to access those.
	MetricsGC = GoRuntimeMetricsRule{regexp.MustCompile(`^/gc/.*`)}
	// MetricsMemory allows only memory metrics to be collected from Go runtime.
	// e.g. go_memory_classes_heap_free_bytes
	MetricsMemory = GoRuntimeMetricsRule{regexp.MustCompile(`^/memory/.*`)}
	// MetricsScheduler allows only scheduler metrics to be collected from Go runtime.
	// e.g. go_sched_goroutines_goroutines
	MetricsScheduler = GoRuntimeMetricsRule{regexp.MustCompile(`^/sched/.*`)}
	// MetricsDebug allows only debug metrics to be collected from Go runtime.
	// e.g. go_godebug_non_default_behavior_gocachetest_events_total
	MetricsDebug = GoRuntimeMetricsRule{regexp.MustCompile(`^/godebug/.*`)}
)

// WithGoCollectorMemStatsMetricsDisabled disables metrics that is gathered in runtime.MemStats structure such as:
//
// go_memstats_alloc_bytes
// go_memstats_alloc_bytes_total
// go_memstats_sys_bytes
// go_memstats_mallocs_total
// go_memstats_frees_total
// go_memstats_heap_alloc_bytes
// go_memstats_heap_sys_bytes
// go_memstats_heap_idle_bytes
// go_memstats_heap_inuse_bytes
// go_memstats_heap_released_bytes
// go_memstats_heap_objects
// go_memstats_stack_inuse_bytes
// go_memstats_stack_sys_bytes
// go_memstats_mspan_inuse_bytes
// go_memstats_mspan_sys_bytes
// go_memstats_mcache_inuse_bytes
// go_memstats_mcache_sys_bytes
// go_memstats_buck_hash_sys_bytes
// go_memstats_gc_sys_bytes
// go_memstats_other_sys_bytes
// go_memstats_next_gc_bytes
//
// so the metrics known from pre client_golang v1.12.0,
//
// NOTE(bwplotka): The above represents runtime.MemStats statistics, but they are
// actually implemented using new runtime/metrics package. (except skipped go_memstats_gc_cpu_fraction
// -- see  https://github.com/prometheus/client_golang/issues/842#issuecomment-861812034 for explanation).
//
// Some users might want to disable this on collector level (although you can use scrape relabelling on Prometheus),
// because similar metrics can be now obtained using WithGoCollectorRuntimeMetrics. Note that the semantics of new
// metrics might be different, plus the names can be change over time with different Go version.
//
// NOTE(bwplotka): Changing metric names can be tedious at times as the alerts, recording rules and dashboards have to be adjusted.
// The old metrics are also very useful, with many guides and books written about how to interpret them.
//
// As a result our recommendation would be to stick with MemStats like metrics and enable other runtime/metrics if you are interested
// in advanced insights Go provides. See ExampleGoCollector_WithAdvancedGoMetrics.
func WithGoCollectorMemStatsMetricsDisabled() func(options *internal.GoCollectorOptions) {
	return func(o *internal.GoCollectorOptions) {
		o.DisableMemStatsLikeMetrics = true
	}
}

// GoRuntimeMetricsRule allow enabling and configuring particular group of runtime/metrics.
// TODO(bwplotka): Consider adding ability to adjust buckets.
type GoRuntimeMetricsRule struct {
	// Matcher represents RE2 expression will match the runtime/metrics from https://golang.bg/src/runtime/metrics/description.go
	// Use `regexp.MustCompile` or `regexp.Compile` to create this field.
	Matcher *regexp.Regexp
}

// WithGoCollectorRuntimeMetrics allows enabling and configuring particular group of runtime/metrics.
// See the list of metrics https://golang.bg/src/runtime/metrics/description.go (pick the Go version you use there!).
// You can use this option in repeated manner, which will add new rules. The order of rules is important, the last rule
// that matches particular metrics is applied.
func WithGoCollectorRuntimeMetrics(rules ...GoRuntimeMetricsRule) func(options *internal.GoCollectorOptions) {
	rs := make([]internal.GoCollectorRule, len(rules))
	for i, r := range rules {
		rs[i] = internal.GoCollectorRule{
			Matcher: r.Matcher,
		}
	}

	return func(o *internal.GoCollectorOptions) {
		o.RuntimeMetricRules = append(o.RuntimeMetricRules, rs...)
	}
}

// WithoutGoCollectorRuntimeMetrics allows disabling group of runtime/metrics that you might have added in WithGoCollectorRuntimeMetrics.
// It behaves similarly to WithGoCollectorRuntimeMetrics just with deny-list semantics.
func WithoutGoCollectorRuntimeMetrics(matchers ...*regexp.Regexp) func(options *internal.GoCollectorOptions) {
	rs := make([]internal.GoCollectorRule, len(matchers))
	for i, m := range matchers {
		rs[i] = internal.GoCollectorRule{
			Matcher: m,
			Deny:    true,
		}
	}

	return func(o *internal.GoCollectorOptions) {
		o.RuntimeMetricRules = append(o.RuntimeMetricRules, rs...)
	}
}

// GoCollectionOption represents Go collection option flag.
// Deprecated.
type GoCollectionOption uint32

const (
	// GoRuntimeMemStatsCollection represents the metrics represented by runtime.MemStats structure.
	//
	// Deprecated: Use WithGoCollectorMemStatsMetricsDisabled() function to disable those metrics in the collector.
	GoRuntimeMemStatsCollection GoCollectionOption = 1 << iota
	// GoRuntimeMetricsCollection is the new set of metrics represented by runtime/metrics package.
	//
	// Deprecated: Use WithGoCollectorRuntimeMetrics(GoRuntimeMetricsRule{Matcher: regexp.MustCompile("/.*")})
	// function to enable those metrics in the collector.
	GoRuntimeMetricsCollection
)

// WithGoCollections allows enabling different collections for Go collector on top of base metrics.
//
// Deprecated: Use WithGoCollectorRuntimeMetrics() and WithGoCollectorMemStatsMetricsDisabled() instead to control metrics.
func WithGoCollections(flags GoCollectionOption) func(options *internal.GoCollectorOptions) {
	return func(options *internal.GoCollectorOptions) {
		if flags&GoRuntimeMemStatsCollection == 0 {
			WithGoCollectorMemStatsMetricsDisabled()(options)
		}

		if flags&GoRuntimeMetricsCollection != 0 {
			WithGoCollectorRuntimeMetrics(GoRuntimeMetricsRule{Matcher: regexp.MustCompile("/.*")})(options)
		}
	}
}

// NewGoCollector returns a collector that exports metrics about the current Go
// process using debug.GCStats (base metrics) and runtime/metrics (both in MemStats style and new ones).
func NewGoCollector(opts ...func(o *internal.GoCollectorOptions)) prometheus.Collector {
	//nolint:staticcheck // Ignore SA1019 until v2.
	return prometheus.NewGoCollector(opts...)
}
