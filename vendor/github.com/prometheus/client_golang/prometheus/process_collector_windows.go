// Copyright 2019 The Prometheus Authors
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
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func canCollectProcess() bool {
	return true
}

var (
	modpsapi    = syscall.NewLazyDLL("psapi.dll")
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetProcessMemoryInfo  = modpsapi.NewProc("GetProcessMemoryInfo")
	procGetProcessHandleCount = modkernel32.NewProc("GetProcessHandleCount")
)

type processMemoryCounters struct {
	// System interface description
	// https://docs.microsoft.com/en-us/windows/desktop/api/psapi/ns-psapi-process_memory_counters_ex

	// Refer to the Golang internal implementation
	// https://golang.org/src/internal/syscall/windows/psapi_windows.go
	_                          uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

func getProcessMemoryInfo(handle windows.Handle) (processMemoryCounters, error) {
	mem := processMemoryCounters{}
	r1, _, err := procGetProcessMemoryInfo.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&mem)),
		uintptr(unsafe.Sizeof(mem)),
	)
	if r1 != 1 {
		return mem, err
	} else {
		return mem, nil
	}
}

func getProcessHandleCount(handle windows.Handle) (uint32, error) {
	var count uint32
	r1, _, err := procGetProcessHandleCount.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&count)),
	)
	if r1 != 1 {
		return 0, err
	} else {
		return count, nil
	}
}

func (c *processCollector) processCollect(ch chan<- Metric) {
	h := windows.CurrentProcess()

	var startTime, exitTime, kernelTime, userTime windows.Filetime
	err := windows.GetProcessTimes(h, &startTime, &exitTime, &kernelTime, &userTime)
	if err != nil {
		c.reportError(ch, nil, err)
		return
	}
	ch <- MustNewConstMetric(c.startTime, GaugeValue, float64(startTime.Nanoseconds()/1e9))
	ch <- MustNewConstMetric(c.cpuTotal, CounterValue, fileTimeToSeconds(kernelTime)+fileTimeToSeconds(userTime))

	mem, err := getProcessMemoryInfo(h)
	if err != nil {
		c.reportError(ch, nil, err)
		return
	}
	ch <- MustNewConstMetric(c.vsize, GaugeValue, float64(mem.PrivateUsage))
	ch <- MustNewConstMetric(c.rss, GaugeValue, float64(mem.WorkingSetSize))

	handles, err := getProcessHandleCount(h)
	if err != nil {
		c.reportError(ch, nil, err)
		return
	}
	ch <- MustNewConstMetric(c.openFDs, GaugeValue, float64(handles))
	ch <- MustNewConstMetric(c.maxFDs, GaugeValue, float64(16*1024*1024)) // Windows has a hard-coded max limit, not per-process.
}

// describe returns all descriptions of the collector for windows.
// Ensure that this list of descriptors is kept in sync with the metrics collected
// in the processCollect method. Any changes to the metrics in processCollect
// (such as adding or removing metrics) should be reflected in this list of descriptors.
func (c *processCollector) describe(ch chan<- *Desc) {
	ch <- c.cpuTotal
	ch <- c.openFDs
	ch <- c.maxFDs
	ch <- c.vsize
	ch <- c.rss
	ch <- c.startTime
}

func fileTimeToSeconds(ft windows.Filetime) float64 {
	return float64(uint64(ft.HighDateTime)<<32+uint64(ft.LowDateTime)) / 1e7
}
