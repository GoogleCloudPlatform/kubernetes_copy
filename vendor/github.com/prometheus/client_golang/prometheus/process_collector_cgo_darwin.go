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

//go:build darwin && cgo

package prometheus

/*
int get_memory_info(unsigned long long *rss, unsigned long long *vs);
*/
import "C"
import "fmt"

func getMemory() (*memoryInfo, error) {
	var rss, vsize C.ulonglong

	if err := C.get_memory_info(&rss, &vsize); err != 0 {
		return nil, fmt.Errorf("task_info() failed with 0x%x", int(err))
	}

	return &memoryInfo{vsize: uint64(vsize), rss: uint64(rss)}, nil
}

// describe returns all descriptions of the collector for Darwin.
// Ensure that this list of descriptors is kept in sync with the metrics collected
// in the processCollect method. Any changes to the metrics in processCollect
// (such as adding or removing metrics) should be reflected in this list of descriptors.
func (c *processCollector) describe(ch chan<- *Desc) {
	ch <- c.cpuTotal
	ch <- c.openFDs
	ch <- c.maxFDs
	ch <- c.maxVsize
	ch <- c.startTime
	ch <- c.rss
	ch <- c.vsize

	/* the process could be collected but not implemented yet
	ch <- c.inBytes
	ch <- c.outBytes
	*/
}
