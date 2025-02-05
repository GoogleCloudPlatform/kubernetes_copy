// Copyright 2015 The Prometheus Authors
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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type processCollector struct {
	collectFn         func(chan<- Metric)
	describeFn        func(chan<- *Desc)
	pidFn             func() (int, error)
	reportErrors      bool
	cpuTotal          *Desc
	openFDs, maxFDs   *Desc
	vsize, maxVsize   *Desc
	rss               *Desc
	startTime         *Desc
	inBytes, outBytes *Desc
}

// ProcessCollectorOpts defines the behavior of a process metrics collector
// created with NewProcessCollector.
type ProcessCollectorOpts struct {
	// PidFn returns the PID of the process the collector collects metrics
	// for. It is called upon each collection. By default, the PID of the
	// current process is used, as determined on construction time by
	// calling os.Getpid().
	PidFn func() (int, error)
	// If non-empty, each of the collected metrics is prefixed by the
	// provided string and an underscore ("_").
	Namespace string
	// If true, any error encountered during collection is reported as an
	// invalid metric (see NewInvalidMetric). Otherwise, errors are ignored
	// and the collected metrics will be incomplete. (Possibly, no metrics
	// will be collected at all.) While that's usually not desired, it is
	// appropriate for the common "mix-in" of process metrics, where process
	// metrics are nice to have, but failing to collect them should not
	// disrupt the collection of the remaining metrics.
	ReportErrors bool
}

// NewProcessCollector is the obsolete version of collectors.NewProcessCollector.
// See there for documentation.
//
// Deprecated: Use collectors.NewProcessCollector instead.
func NewProcessCollector(opts ProcessCollectorOpts) Collector {
	ns := ""
	if len(opts.Namespace) > 0 {
		ns = opts.Namespace + "_"
	}

	c := &processCollector{
		reportErrors: opts.ReportErrors,
		cpuTotal: NewDesc(
			ns+"process_cpu_seconds_total",
			"Total user and system CPU time spent in seconds.",
			nil, nil,
		),
		openFDs: NewDesc(
			ns+"process_open_fds",
			"Number of open file descriptors.",
			nil, nil,
		),
		maxFDs: NewDesc(
			ns+"process_max_fds",
			"Maximum number of open file descriptors.",
			nil, nil,
		),
		vsize: NewDesc(
			ns+"process_virtual_memory_bytes",
			"Virtual memory size in bytes.",
			nil, nil,
		),
		maxVsize: NewDesc(
			ns+"process_virtual_memory_max_bytes",
			"Maximum amount of virtual memory available in bytes.",
			nil, nil,
		),
		rss: NewDesc(
			ns+"process_resident_memory_bytes",
			"Resident memory size in bytes.",
			nil, nil,
		),
		startTime: NewDesc(
			ns+"process_start_time_seconds",
			"Start time of the process since unix epoch in seconds.",
			nil, nil,
		),
		inBytes: NewDesc(
			ns+"process_network_receive_bytes_total",
			"Number of bytes received by the process over the network.",
			nil, nil,
		),
		outBytes: NewDesc(
			ns+"process_network_transmit_bytes_total",
			"Number of bytes sent by the process over the network.",
			nil, nil,
		),
	}

	if opts.PidFn == nil {
		c.pidFn = getPIDFn()
	} else {
		c.pidFn = opts.PidFn
	}

	// Set up process metric collection if supported by the runtime.
	if canCollectProcess() {
		c.collectFn = c.processCollect
		c.describeFn = c.describe
	} else {
		c.collectFn = c.errorCollectFn
		c.describeFn = c.errorDescribeFn
	}

	return c
}

func (c *processCollector) errorCollectFn(ch chan<- Metric) {
	c.reportError(ch, nil, errors.New("process metrics not supported on this platform"))
}

func (c *processCollector) errorDescribeFn(ch chan<- *Desc) {
	if c.reportErrors {
		ch <- NewInvalidDesc(errors.New("process metrics not supported on this platform"))
	}
}

// Collect returns the current state of all metrics of the collector.
func (c *processCollector) Collect(ch chan<- Metric) {
	c.collectFn(ch)
}

// Describe returns all descriptions of the collector.
func (c *processCollector) Describe(ch chan<- *Desc) {
	c.describeFn(ch)
}

func (c *processCollector) reportError(ch chan<- Metric, desc *Desc, err error) {
	if !c.reportErrors {
		return
	}
	if desc == nil {
		desc = NewInvalidDesc(err)
	}
	ch <- NewInvalidMetric(desc, err)
}

// NewPidFileFn returns a function that retrieves a pid from the specified file.
// It is meant to be used for the PidFn field in ProcessCollectorOpts.
func NewPidFileFn(pidFilePath string) func() (int, error) {
	return func() (int, error) {
		content, err := os.ReadFile(pidFilePath)
		if err != nil {
			return 0, fmt.Errorf("can't read pid file %q: %w", pidFilePath, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
		if err != nil {
			return 0, fmt.Errorf("can't parse pid file %q: %w", pidFilePath, err)
		}

		return pid, nil
	}
}
