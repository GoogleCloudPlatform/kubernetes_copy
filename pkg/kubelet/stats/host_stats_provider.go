/*
Copyright 2018 The Kubernetes Authors.

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

package stats

import (
	"fmt"
	"os"
	"path/filepath"

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/volume"
)

// PodEtcHostsPathFunc is a function to fetch a etc hosts path by pod uid and whether etc host path is supported by the runtime
type PodEtcHostsPathFunc func(podUID types.UID) string

// metricsProviderByPath maps a path to its metrics provider
type metricsProviderByPath map[string]volume.MetricsProvider

// HostStatsProvider defines an interface for providing host stats associated with pod.
type HostStatsProvider interface {
	// getPodLogStats gets stats associated with pod log usage
	getPodLogStats(podNamespace, podName string, podUID types.UID, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error)
	// getPodContainerLogStats gets stats associated with container log usage
	getPodContainerLogStats(podNamespace, podName string, podUID types.UID, containerName string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error)
	// getPodEtcHostsStats gets stats associated with pod etc-hosts usage
	getPodEtcHostsStats(podUID types.UID, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error)
	// getTerminationMessagePathStats gets stats associated with container termination log usage
	getTerminationMessagePathStats(path string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error)
}

type hostStatsProvider struct {
	// osInterface is the interface for syscalls.
	osInterface kubecontainer.OSInterface
	// podEtcHostsPathFunc fetches a pod etc hosts path by uid.
	podEtcHostsPathFunc PodEtcHostsPathFunc
	// podLogsDirectory is the root directory path for pod logs.
	podLogsDirectory string
}

// NewHostStatsProvider returns a new HostStatsProvider type struct.
func NewHostStatsProvider(osInterface kubecontainer.OSInterface, podEtcHostsPathFunc PodEtcHostsPathFunc, podLogsDirectory string) HostStatsProvider {
	return hostStatsProvider{
		osInterface:         osInterface,
		podEtcHostsPathFunc: podEtcHostsPathFunc,
		podLogsDirectory:    podLogsDirectory,
	}
}

func (h hostStatsProvider) getPodLogStats(podNamespace, podName string, podUID types.UID, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	metricsByPath, err := h.podLogMetrics(podNamespace, podName, podUID)
	if err != nil {
		return nil, err
	}
	return metricsByPathToFsStats(metricsByPath, rootFsInfo)
}

// getPodContainerLogStats gets stats for container
func (h hostStatsProvider) getPodContainerLogStats(podNamespace, podName string, podUID types.UID, containerName string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	metricsByPath, err := h.podContainerLogMetrics(podNamespace, podName, podUID, containerName)
	if err != nil {
		return nil, err
	}
	return metricsByPathToFsStats(metricsByPath, rootFsInfo)
}

// getTerminationMessagePathStats gets stats for container termination message usage
func (h hostStatsProvider) getTerminationMessagePathStats(path string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	metrics := volume.NewMetricsDu(path)
	hostMetrics, err := metrics.GetMetrics()
	if err != nil {
		return nil, fmt.Errorf("failed to get stats %w", err)
	}
	result := rootFsInfoToFsStats(rootFsInfo)
	usedBytes := uint64(hostMetrics.Used.Value())
	inodesUsed := uint64(hostMetrics.InodesUsed.Value())
	result.UsedBytes = addUsage(result.UsedBytes, &usedBytes)
	result.InodesUsed = addUsage(result.InodesUsed, &inodesUsed)
	result.Time = maxUpdateTime(&result.Time, &hostMetrics.Time)
	return result, nil
}

// getPodEtcHostsStats gets status for pod etc hosts usage
func (h hostStatsProvider) getPodEtcHostsStats(podUID types.UID, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	// Runtimes may not support etc hosts file (Windows with docker)
	podEtcHostsPath := h.podEtcHostsPathFunc(podUID)
	// Some pods have an explicit /etc/hosts mount and the Kubelet will not create an etc-hosts file for them
	if _, err := os.Stat(podEtcHostsPath); os.IsNotExist(err) {
		return nil, nil
	}

	metrics := volume.NewMetricsDu(podEtcHostsPath)
	hostMetrics, err := metrics.GetMetrics()
	if err != nil {
		return nil, fmt.Errorf("failed to get stats %v", err)
	}
	result := rootFsInfoToFsStats(rootFsInfo)
	usedBytes := uint64(hostMetrics.Used.Value())
	inodesUsed := uint64(hostMetrics.InodesUsed.Value())
	result.UsedBytes = addUsage(result.UsedBytes, &usedBytes)
	result.InodesUsed = addUsage(result.InodesUsed, &inodesUsed)
	result.Time = maxUpdateTime(&result.Time, &hostMetrics.Time)
	return result, nil
}

func (h hostStatsProvider) podLogMetrics(podNamespace, podName string, podUID types.UID) (metricsProviderByPath, error) {
	podLogsDirectoryPath := kuberuntime.BuildPodLogsDirectory(h.podLogsDirectory, podNamespace, podName, podUID)
	return h.fileMetricsByDir(podLogsDirectoryPath)
}

func (h hostStatsProvider) podContainerLogMetrics(podNamespace, podName string, podUID types.UID, containerName string) (metricsProviderByPath, error) {
	podContainerLogsDirectoryPath := kuberuntime.BuildContainerLogsDirectory(h.podLogsDirectory, podNamespace, podName, podUID, containerName)
	return h.fileMetricsByDir(podContainerLogsDirectoryPath)
}

// fileMetricsByDir returns metrics by path for each file under specified directory
func (h hostStatsProvider) fileMetricsByDir(dirname string) (metricsProviderByPath, error) {
	files, err := h.osInterface.ReadDir(dirname)
	if err != nil {
		return nil, err
	}
	results := metricsProviderByPath{}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		// Only include *files* under pod log directory.
		fpath := filepath.Join(dirname, f.Name())
		results[fpath] = volume.NewMetricsDu(fpath)
	}
	return results, nil
}

// metricsByPathToFsStats converts a metrics provider by path to fs stats
func metricsByPathToFsStats(metricsByPath metricsProviderByPath, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	result := rootFsInfoToFsStats(rootFsInfo)
	for fpath, metrics := range metricsByPath {
		hostMetrics, err := metrics.GetMetrics()
		if err != nil {
			return nil, fmt.Errorf("failed to get fsstats for %q: %v", fpath, err)
		}
		usedBytes := uint64(hostMetrics.Used.Value())
		inodesUsed := uint64(hostMetrics.InodesUsed.Value())
		result.UsedBytes = addUsage(result.UsedBytes, &usedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, &inodesUsed)
		result.Time = maxUpdateTime(&result.Time, &hostMetrics.Time)
	}
	return result, nil
}

// rootFsInfoToFsStats is a utility to convert rootFsInfo into statsapi.FsStats
func rootFsInfoToFsStats(rootFsInfo *cadvisorapiv2.FsInfo) *statsapi.FsStats {
	return &statsapi.FsStats{
		Time:           metav1.NewTime(rootFsInfo.Timestamp),
		AvailableBytes: &rootFsInfo.Available,
		CapacityBytes:  &rootFsInfo.Capacity,
		InodesFree:     rootFsInfo.InodesFree,
		Inodes:         rootFsInfo.Inodes,
	}
}
