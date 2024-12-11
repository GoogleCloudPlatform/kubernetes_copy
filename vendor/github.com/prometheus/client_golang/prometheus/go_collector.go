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

package prometheus

import (
	"runtime"
	"runtime/debug"
	"time"
)

// goRuntimeMemStats provides the metrics initially provided by runtime.ReadMemStats.
// From Go 1.17 those similar (and better) statistics are provided by runtime/metrics, so
// while eval closure works on runtime.MemStats, the struct from Go 1.17+ is
// populated using runtime/metrics. Those are the defaults we can't alter.
func goRuntimeMemStats() memStatsMetrics {
	return memStatsMetrics{
		{
			desc: NewDesc(
				memstatNamespace("alloc_bytes"),
				"Number of bytes allocated in heap and currently in use. Equals to /memory/classes/heap/objects:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.Alloc) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("alloc_bytes_total"),
				"Total number of bytes allocated in heap until now, even if released already. Equals to /gc/heap/allocs:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.TotalAlloc) },
			valType: CounterValue,
		}, {
			desc: NewDesc(
				memstatNamespace("sys_bytes"),
				"Number of bytes obtained from system. Equals to /memory/classes/total:byte.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.Sys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("mallocs_total"),
				// TODO(bwplotka): We could add go_memstats_heap_objects, probably useful for discovery. Let's gather more feedback, kind of a waste of bytes for everybody for compatibility reasons to keep both, and we can't really rename/remove useful metric.
				"Total number of heap objects allocated, both live and gc-ed. Semantically a counter version for go_memstats_heap_objects gauge. Equals to /gc/heap/allocs:objects + /gc/heap/tiny/allocs:objects.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.Mallocs) },
			valType: CounterValue,
		}, {
			desc: NewDesc(
				memstatNamespace("frees_total"),
				"Total number of heap objects frees. Equals to /gc/heap/frees:objects + /gc/heap/tiny/allocs:objects.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.Frees) },
			valType: CounterValue,
		}, {
			desc: NewDesc(
				memstatNamespace("heap_alloc_bytes"),
				"Number of heap bytes allocated and currently in use, same as go_memstats_alloc_bytes. Equals to /memory/classes/heap/objects:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.HeapAlloc) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("heap_sys_bytes"),
				"Number of heap bytes obtained from system. Equals to /memory/classes/heap/objects:bytes + /memory/classes/heap/unused:bytes + /memory/classes/heap/released:bytes + /memory/classes/heap/free:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.HeapSys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("heap_idle_bytes"),
				"Number of heap bytes waiting to be used. Equals to /memory/classes/heap/released:bytes + /memory/classes/heap/free:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.HeapIdle) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("heap_inuse_bytes"),
				"Number of heap bytes that are in use. Equals to /memory/classes/heap/objects:bytes + /memory/classes/heap/unused:bytes",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.HeapInuse) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("heap_released_bytes"),
				"Number of heap bytes released to OS. Equals to /memory/classes/heap/released:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.HeapReleased) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("heap_objects"),
				"Number of currently allocated objects. Equals to /gc/heap/objects:objects.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.HeapObjects) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("stack_inuse_bytes"),
				"Number of bytes obtained from system for stack allocator in non-CGO environments. Equals to /memory/classes/heap/stacks:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.StackInuse) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("stack_sys_bytes"),
				"Number of bytes obtained from system for stack allocator. Equals to /memory/classes/heap/stacks:bytes + /memory/classes/os-stacks:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.StackSys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("mspan_inuse_bytes"),
				"Number of bytes in use by mspan structures. Equals to /memory/classes/metadata/mspan/inuse:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.MSpanInuse) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("mspan_sys_bytes"),
				"Number of bytes used for mspan structures obtained from system. Equals to /memory/classes/metadata/mspan/inuse:bytes + /memory/classes/metadata/mspan/free:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.MSpanSys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("mcache_inuse_bytes"),
				"Number of bytes in use by mcache structures. Equals to /memory/classes/metadata/mcache/inuse:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.MCacheInuse) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("mcache_sys_bytes"),
				"Number of bytes used for mcache structures obtained from system. Equals to /memory/classes/metadata/mcache/inuse:bytes + /memory/classes/metadata/mcache/free:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.MCacheSys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("buck_hash_sys_bytes"),
				"Number of bytes used by the profiling bucket hash table. Equals to /memory/classes/profiling/buckets:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.BuckHashSys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("gc_sys_bytes"),
				"Number of bytes used for garbage collection system metadata. Equals to /memory/classes/metadata/other:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.GCSys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("other_sys_bytes"),
				"Number of bytes used for other system allocations. Equals to /memory/classes/other:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.OtherSys) },
			valType: GaugeValue,
		}, {
			desc: NewDesc(
				memstatNamespace("next_gc_bytes"),
				"Number of heap bytes when next garbage collection will take place. Equals to /gc/heap/goal:bytes.",
				nil, nil,
			),
			eval:    func(ms *runtime.MemStats) float64 { return float64(ms.NextGC) },
			valType: GaugeValue,
		},
	}
}

type baseGoCollector struct {
	goroutinesDesc *Desc
	threadsDesc    *Desc
	gcDesc         *Desc
	gcLastTimeDesc *Desc
	goInfoDesc     *Desc
}

func newBaseGoCollector() baseGoCollector {
	return baseGoCollector{
		goroutinesDesc: NewDesc(
			"go_goroutines",
			"Number of goroutines that currently exist.",
			nil, nil),
		threadsDesc: NewDesc(
			"go_threads",
			"Number of OS threads created.",
			nil, nil),
		gcDesc: NewDesc(
			"go_gc_duration_seconds",
			"A summary of the wall-time pause (stop-the-world) duration in garbage collection cycles.",
			nil, nil),
		gcLastTimeDesc: NewDesc(
			"go_memstats_last_gc_time_seconds",
			"Number of seconds since 1970 of last garbage collection.",
			nil, nil),
		goInfoDesc: NewDesc(
			"go_info",
			"Information about the Go environment.",
			nil, Labels{"version": runtime.Version()}),
	}
}

// Describe returns all descriptions of the collector.
func (c *baseGoCollector) Describe(ch chan<- *Desc) {
	ch <- c.goroutinesDesc
	ch <- c.threadsDesc
	ch <- c.gcDesc
	ch <- c.gcLastTimeDesc
	ch <- c.goInfoDesc
}

// Collect returns the current state of all metrics of the collector.
func (c *baseGoCollector) Collect(ch chan<- Metric) {
	ch <- MustNewConstMetric(c.goroutinesDesc, GaugeValue, float64(runtime.NumGoroutine()))

	n := getRuntimeNumThreads()
	ch <- MustNewConstMetric(c.threadsDesc, GaugeValue, n)

	var stats debug.GCStats
	stats.PauseQuantiles = make([]time.Duration, 5)
	debug.ReadGCStats(&stats)

	quantiles := make(map[float64]float64)
	for idx, pq := range stats.PauseQuantiles[1:] {
		quantiles[float64(idx+1)/float64(len(stats.PauseQuantiles)-1)] = pq.Seconds()
	}
	quantiles[0.0] = stats.PauseQuantiles[0].Seconds()
	ch <- MustNewConstSummary(c.gcDesc, uint64(stats.NumGC), stats.PauseTotal.Seconds(), quantiles)
	ch <- MustNewConstMetric(c.gcLastTimeDesc, GaugeValue, float64(stats.LastGC.UnixNano())/1e9)
	ch <- MustNewConstMetric(c.goInfoDesc, GaugeValue, 1)
}

func memstatNamespace(s string) string {
	return "go_memstats_" + s
}

// memStatsMetrics provide description, evaluator, runtime/metrics name, and
// value type for memstat metrics.
type memStatsMetrics []struct {
	desc    *Desc
	eval    func(*runtime.MemStats) float64
	valType ValueType
}
