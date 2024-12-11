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

//go:build !windows && !js && !wasip1
// +build !windows,!js,!wasip1

package prometheus

import (
	"github.com/prometheus/procfs"
)

func canCollectProcess() bool {
	_, err := procfs.NewDefaultFS()
	return err == nil
}

func (c *processCollector) processCollect(ch chan<- Metric) {
	pid, err := c.pidFn()
	if err != nil {
		c.reportError(ch, nil, err)
		return
	}

	p, err := procfs.NewProc(pid)
	if err != nil {
		c.reportError(ch, nil, err)
		return
	}

	if stat, err := p.Stat(); err == nil {
		ch <- MustNewConstMetric(c.cpuTotal, CounterValue, stat.CPUTime())
		ch <- MustNewConstMetric(c.vsize, GaugeValue, float64(stat.VirtualMemory()))
		ch <- MustNewConstMetric(c.rss, GaugeValue, float64(stat.ResidentMemory()))
		if startTime, err := stat.StartTime(); err == nil {
			ch <- MustNewConstMetric(c.startTime, GaugeValue, startTime)
		} else {
			c.reportError(ch, c.startTime, err)
		}
	} else {
		c.reportError(ch, nil, err)
	}

	if fds, err := p.FileDescriptorsLen(); err == nil {
		ch <- MustNewConstMetric(c.openFDs, GaugeValue, float64(fds))
	} else {
		c.reportError(ch, c.openFDs, err)
	}

	if limits, err := p.Limits(); err == nil {
		ch <- MustNewConstMetric(c.maxFDs, GaugeValue, float64(limits.OpenFiles))
		ch <- MustNewConstMetric(c.maxVsize, GaugeValue, float64(limits.AddressSpace))
	} else {
		c.reportError(ch, nil, err)
	}

	if netstat, err := p.Netstat(); err == nil {
		var inOctets, outOctets float64
		if netstat.IpExt.InOctets != nil {
			inOctets = *netstat.IpExt.InOctets
		}
		if netstat.IpExt.OutOctets != nil {
			outOctets = *netstat.IpExt.OutOctets
		}
		ch <- MustNewConstMetric(c.inBytes, CounterValue, inOctets)
		ch <- MustNewConstMetric(c.outBytes, CounterValue, outOctets)
	} else {
		c.reportError(ch, nil, err)
	}
}
