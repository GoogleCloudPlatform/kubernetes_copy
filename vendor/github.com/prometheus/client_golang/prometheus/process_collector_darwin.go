// Copyright 2024 The Prometheus Authors
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
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// notImplementedErr is returned by stub functions that replace cgo functions, when cgo
// isn't available.
var notImplementedErr = errors.New("not implemented")

type memoryInfo struct {
	vsize uint64 // Virtual memory size in bytes
	rss   uint64 // Resident memory size in bytes
}

func canCollectProcess() bool {
	return true
}

func getSoftLimit(which int) (uint64, error) {
	rlimit := syscall.Rlimit{}

	if err := syscall.Getrlimit(which, &rlimit); err != nil {
		return 0, err
	}

	return rlimit.Cur, nil
}

func getOpenFileCount() (float64, error) {
	// Alternately, the undocumented proc_pidinfo(PROC_PIDLISTFDS) can be used to
	// return a list of open fds, but that requires a way to call C APIs.  The
	// benefits, however, include fewer system calls and not failing when at the
	// open file soft limit.

	if dir, err := os.Open("/dev/fd"); err != nil {
		return 0.0, err
	} else {
		defer dir.Close()

		// Avoid ReadDir(), as it calls stat(2) on each descriptor.  Not only is
		// that info not used, but KQUEUE descriptors fail stat(2), which causes
		// the whole method to fail.
		if names, err := dir.Readdirnames(0); err != nil {
			return 0.0, err
		} else {
			// Subtract 1 to ignore the open /dev/fd descriptor above.
			return float64(len(names) - 1), nil
		}
	}
}

func (c *processCollector) processCollect(ch chan<- Metric) {
	if procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", os.Getpid()); err == nil {
		if len(procs) == 1 {
			startTime := float64(procs[0].Proc.P_starttime.Nano() / 1e9)
			ch <- MustNewConstMetric(c.startTime, GaugeValue, startTime)
		} else {
			err = fmt.Errorf("sysctl() returned %d proc structs (expected 1)", len(procs))
			c.reportError(ch, c.startTime, err)
		}
	} else {
		c.reportError(ch, c.startTime, err)
	}

	// The proc structure returned by kern.proc.pid above has an Rusage member,
	// but it is not filled in, so it needs to be fetched by getrusage(2).  For
	// that call, the UTime, STime, and Maxrss members are filled out, but not
	// Ixrss, Idrss, or Isrss for the memory usage.  Memory stats will require
	// access to the C API to call task_info(TASK_BASIC_INFO).
	rusage := unix.Rusage{}

	if err := unix.Getrusage(syscall.RUSAGE_SELF, &rusage); err == nil {
		cpuTime := time.Duration(rusage.Stime.Nano() + rusage.Utime.Nano()).Seconds()
		ch <- MustNewConstMetric(c.cpuTotal, CounterValue, cpuTime)
	} else {
		c.reportError(ch, c.cpuTotal, err)
	}

	if memInfo, err := getMemory(); err == nil {
		ch <- MustNewConstMetric(c.rss, GaugeValue, float64(memInfo.rss))
		ch <- MustNewConstMetric(c.vsize, GaugeValue, float64(memInfo.vsize))
	} else if !errors.Is(err, notImplementedErr) {
		// Don't report an error when support is not compiled in.
		c.reportError(ch, c.rss, err)
		c.reportError(ch, c.vsize, err)
	}

	if fds, err := getOpenFileCount(); err == nil {
		ch <- MustNewConstMetric(c.openFDs, GaugeValue, fds)
	} else {
		c.reportError(ch, c.openFDs, err)
	}

	if openFiles, err := getSoftLimit(syscall.RLIMIT_NOFILE); err == nil {
		ch <- MustNewConstMetric(c.maxFDs, GaugeValue, float64(openFiles))
	} else {
		c.reportError(ch, c.maxFDs, err)
	}

	if addressSpace, err := getSoftLimit(syscall.RLIMIT_AS); err == nil {
		ch <- MustNewConstMetric(c.maxVsize, GaugeValue, float64(addressSpace))
	} else {
		c.reportError(ch, c.maxVsize, err)
	}

	// TODO: socket(PF_SYSTEM) to fetch "com.apple.network.statistics" might
	//  be able to get the per-process network send/receive counts.
}
