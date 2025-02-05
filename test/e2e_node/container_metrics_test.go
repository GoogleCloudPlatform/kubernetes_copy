/*
Copyright 2024 The Kubernetes Authors.

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

package e2enode

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	"github.com/onsi/gomega/types"

	"k8s.io/kubernetes/test/e2e/framework"
	e2emetrics "k8s.io/kubernetes/test/e2e/framework/metrics"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("ContainerMetrics", "[LinuxOnly]", framework.WithNodeConformance(), func() {
	f := framework.NewDefaultFramework("container-metrics")
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged
	ginkgo.Context("when querying /metrics/cadvisor", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			createMetricsPods(ctx, f)
		})
		ginkgo.AfterEach(func(ctx context.Context) {
			removeMetricsPods(ctx, f)
		})
		ginkgo.It("should report container metrics", func(ctx context.Context) {
			keys := gstruct.Keys{}
			ctrMatches := map[string]types.GomegaMatcher{
				"container_blkio_device_usage_total":            boundedSample(0, 10000000),
				"container_cpu_load_average_10s":                boundedSample(0, 100),
				"container_cpu_system_seconds_total":            boundedSample(0, 100),
				"container_cpu_usage_seconds_total":             boundedSample(0, 100),
				"container_cpu_user_seconds_total":              boundedSample(0, 100),
				"container_file_descriptors":                    boundedSample(0, 100),
				"container_fs_reads_bytes_total":                boundedSample(0, 10000000),
				"container_fs_reads_total":                      boundedSample(0, 100),
				"container_fs_usage_bytes":                      boundedSample(0, 1000000),
				"container_fs_writes_bytes_total":               boundedSample(0, 1000000),
				"container_fs_writes_total":                     boundedSample(0, 100),
				"container_last_seen":                           boundedSample(time.Now().Add(-maxStatsAge).Unix(), time.Now().Add(2*time.Minute).Unix()),
				"container_memory_cache":                        boundedSample(1*e2evolume.Kb, 10*e2evolume.Mb),
				"container_memory_failcnt":                      preciseSample(0),
				"container_memory_failures_total":               boundedSample(0, 1000000),
				"container_memory_mapped_file":                  boundedSample(0, 10000000),
				"container_memory_max_usage_bytes":              boundedSample(0, 80*e2evolume.Mb),
				"container_memory_rss":                          boundedSample(10*e2evolume.Kb, 80*e2evolume.Mb),
				"container_memory_swap":                         preciseSample(0),
				"container_memory_usage_bytes":                  boundedSample(10*e2evolume.Kb, 80*e2evolume.Mb),
				"container_memory_working_set_bytes":            boundedSample(10*e2evolume.Kb, 80*e2evolume.Mb),
				"container_oom_events_total":                    preciseSample(0),
				"container_processes":                           boundedSample(0, 10),
				"container_sockets":                             boundedSample(0, 10),
				"container_spec_cpu_period":                     preciseSample(100000),
				"container_spec_cpu_shares":                     preciseSample(2),
				"container_spec_memory_limit_bytes":             preciseSample(79998976),
				"container_spec_memory_reservation_limit_bytes": preciseSample(0),
				"container_spec_memory_swap_limit_bytes":        preciseSample(0),
				"container_start_time_seconds":                  boundedSample(time.Now().Add(-maxStatsAge).Unix(), time.Now().Add(2*time.Minute).Unix()),
				"container_tasks_state":                         preciseSample(0),
				"container_threads":                             boundedSample(0, 10),
				"container_threads_max":                         boundedSample(0, 100000),
				"container_ulimits_soft":                        boundedSample(0, 10000000),
			}
			appendMatchesForContainer(f.Namespace.Name, pod0, pod1, "busybox-container", ctrMatches, keys, gstruct.AllowDuplicates|gstruct.IgnoreExtras)

			ctrOptionalMatches := map[string]types.GomegaMatcher{
				"container_fs_io_current":                     boundedSample(0, 100),
				"container_fs_io_time_seconds_total":          boundedSample(0, 100),
				"container_fs_io_time_weighted_seconds_total": boundedSample(0, 100),
				"container_fs_inodes_free":                    boundedSample(0, 10*e2evolume.Kb),
				"container_fs_inodes_total":                   boundedSample(0, 100),
				"container_fs_limit_bytes":                    boundedSample(100*e2evolume.Mb, 10*e2evolume.Tb),
				"container_fs_usage_bytes":                    boundedSample(0, 1000000),
				"container_fs_read_seconds_total":             preciseSample(0),
				"container_fs_reads_merged_total":             preciseSample(0),
				"container_fs_sector_reads_total":             preciseSample(0),
				"container_fs_sector_writes_total":            preciseSample(0),
				"container_fs_write_seconds_total":            preciseSample(0),
				"container_fs_writes_merged_total":            preciseSample(0),
			}
			// Missing from containerd, so set gstruct.IgnoreMissing
			// See https://github.com/google/cadvisor/issues/2785
			appendMatchesForContainer(f.Namespace.Name, pod0, pod1, "busybox-container", ctrOptionalMatches, keys, gstruct.AllowDuplicates|gstruct.IgnoreMissing|gstruct.IgnoreExtras)

			podMatches := map[string]types.GomegaMatcher{
				"container_network_receive_bytes_total":            boundedSample(10, 10*e2evolume.Mb),
				"container_network_receive_errors_total":           boundedSample(0, 1000),
				"container_network_receive_packets_dropped_total":  boundedSample(0, 1000),
				"container_network_receive_packets_total":          boundedSample(0, 1000),
				"container_network_transmit_bytes_total":           boundedSample(10, 10*e2evolume.Mb),
				"container_network_transmit_errors_total":          boundedSample(0, 1000),
				"container_network_transmit_packets_dropped_total": boundedSample(0, 1000),
				"container_network_transmit_packets_total":         boundedSample(0, 1000),
			}
			// TODO: determine why these are missing from containerd but not CRI-O
			appendMatchesForContainer(f.Namespace.Name, pod0, pod1, "POD", podMatches, keys, gstruct.AllowDuplicates|gstruct.IgnoreMissing|gstruct.IgnoreExtras)

			matchResourceMetrics := gstruct.MatchKeys(gstruct.IgnoreExtras, keys)
			ginkgo.By("Giving pods a minute to start up and produce metrics")
			gomega.Eventually(ctx, getContainerMetrics, 1*time.Minute, 15*time.Second).Should(matchResourceMetrics)
			ginkgo.By("Ensuring the metrics match the expectations a few more times")
			gomega.Consistently(ctx, getContainerMetrics, 1*time.Minute, 15*time.Second).Should(matchResourceMetrics)
		})
	})
})

func getContainerMetrics(ctx context.Context) (e2emetrics.KubeletMetrics, error) {
	ginkgo.By("getting container metrics from cadvisor")
	return e2emetrics.GrabKubeletMetricsWithoutProxy(ctx, framework.TestContext.NodeName+":10255", "/metrics/cadvisor")
}

func preciseSample(value interface{}) types.GomegaMatcher {
	return gstruct.PointTo(gstruct.MatchAllFields(gstruct.Fields{
		"Metric":    gstruct.Ignore(),
		"Value":     gomega.BeEquivalentTo(value),
		"Timestamp": gstruct.Ignore(),
		"Histogram": gstruct.Ignore(),
	}))
}

func appendMatchesForContainer(ns, pod1, pod2, ctr string, matches map[string]types.GomegaMatcher, keys gstruct.Keys, options gstruct.Options) {
	for k, v := range matches {
		keys[k] = gstruct.MatchElements(containerID, options, gstruct.Elements{
			fmt.Sprintf("%s::%s::%s", ns, pod1, ctr): v,
			fmt.Sprintf("%s::%s::%s", ns, pod2, ctr): v,
		})
	}
}
