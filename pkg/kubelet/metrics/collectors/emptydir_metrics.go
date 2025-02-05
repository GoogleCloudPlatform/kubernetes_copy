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

package collectors

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"

	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
)

var (
	emptyDirUsedBytesDesc = metrics.NewDesc(
		metrics.BuildFQName(
			"",
			kubeletmetrics.KubeletSubsystem,
			kubeletmetrics.EmptyDirUsedBytesKey,
		),
		"Bytes used by the emptyDir volume. Only volumes on the default medium are considered.",
		[]string{
			"volume_name",
			"namespace",
			"pod",
		},
		nil,
		metrics.ALPHA,
		"",
	)
	emptyDirSizeLimitBytesDesc = metrics.NewDesc(
		metrics.BuildFQName(
			"",
			kubeletmetrics.KubeletSubsystem,
			kubeletmetrics.EmptyDirSizeLimitBytesKey,
		),
		"Size limit of the emptyDir volume in bytes, if set. Only volumes on the default medium are considered.",
		[]string{
			"volume_name",
			"namespace",
			"pod",
		},
		nil,
		metrics.ALPHA,
		"",
	)
)

type emptyDirMetricsCollector struct {
	metrics.BaseStableCollector

	statsProvider serverstats.Provider
}

// Check if emptyDirMetricsCollector implements necessary interface
var _ metrics.StableCollector = &emptyDirMetricsCollector{}

// NewEmptyDirMetricsCollector implements the metrics.StableCollector interface and
// exposes metrics about pod's emptyDir.
func NewEmptyDirMetricsCollector(statsProvider serverstats.Provider) metrics.StableCollector {
	return &emptyDirMetricsCollector{statsProvider: statsProvider}
}

// DescribeWithStability implements the metrics.StableCollector interface.
func (c *emptyDirMetricsCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- emptyDirUsedBytesDesc
	ch <- emptyDirSizeLimitBytesDesc
}

// CollectWithStability implements the metrics.StableCollector interface.
func (c *emptyDirMetricsCollector) CollectWithStability(ch chan<- metrics.Metric) {
	podStats, err := c.statsProvider.ListPodStats(context.Background())
	if err != nil {
		klog.ErrorS(err, "Failed to get pod stats")
		return
	}

	for _, podStat := range podStats {
		podName := podStat.PodRef.Name
		podNamespace := podStat.PodRef.Namespace

		if podStat.VolumeStats == nil {
			klog.V(5).InfoS("Pod has no volume stats", "pod", podName, "namespace", podNamespace)
			continue
		}

		pod, found := c.statsProvider.GetPodByName(podNamespace, podName)
		if !found {
			klog.V(5).InfoS("Couldn't get pod", "pod", podName, "namespace", podNamespace)
			continue
		}

		podVolumes := make(map[string]v1.Volume, len(pod.Spec.Volumes))
		for _, volume := range pod.Spec.Volumes {
			podVolumes[volume.Name] = volume
		}

		for _, volumeStat := range podStat.VolumeStats {
			if volume, found := podVolumes[volumeStat.Name]; found {
				// Only consider volumes on the default medium.
				if volume.EmptyDir != nil && volume.EmptyDir.Medium == v1.StorageMediumDefault {
					if volumeStat.UsedBytes != nil {
						ch <- metrics.NewLazyConstMetric(
							emptyDirUsedBytesDesc,
							metrics.GaugeValue,
							float64(*volumeStat.UsedBytes),
							volumeStat.Name,
							podNamespace,
							podName,
						)
					}
					if volume.EmptyDir.SizeLimit != nil {
						ch <- metrics.NewLazyConstMetric(
							emptyDirSizeLimitBytesDesc,
							metrics.GaugeValue,
							volume.EmptyDir.SizeLimit.AsApproximateFloat64(),
							volumeStat.Name,
							podNamespace,
							podName,
						)
					}
				}
			}

		}
	}
}
