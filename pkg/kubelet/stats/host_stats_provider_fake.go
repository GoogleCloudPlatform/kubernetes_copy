/*
Copyright 2017 The Kubernetes Authors.

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
	"path/filepath"

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"k8s.io/apimachinery/pkg/types"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubecontainertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/volume"
)

type fakeHostStatsProvider struct {
	fakeStats   map[string]*volume.Metrics
	osInterface kubecontainer.OSInterface
}

// NewFakeHostStatsProvider provides a way to test with fake host statistics
func NewFakeHostStatsProvider() HostStatsProvider {
	return &fakeHostStatsProvider{
		osInterface: &kubecontainertest.FakeOS{},
	}
}

// NewFakeHostStatsProviderWithData provides a way to test with fake host statistics
func NewFakeHostStatsProviderWithData(fakeStats map[string]*volume.Metrics, osInterface kubecontainer.OSInterface) HostStatsProvider {
	return &fakeHostStatsProvider{
		fakeStats:   fakeStats,
		osInterface: osInterface,
	}
}

func (f *fakeHostStatsProvider) getPodLogStats(podNamespace, podName string, podUID types.UID, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	path := kuberuntime.BuildPodLogsDirectory("/var/log/kube/pods/", podNamespace, podName, podUID)
	files, err := f.osInterface.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var results []volume.MetricsProvider
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		// Only include *files* under pod log directory.
		fpath := filepath.Join(path, file.Name())
		results = append(results, NewFakeMetricsDu(fpath, f.fakeStats[fpath]))
	}
	return fakeMetricsProvidersToStats(results, rootFsInfo)
}

func (f *fakeHostStatsProvider) getPodContainerLogStats(podNamespace, podName string, podUID types.UID, containerName string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	path := kuberuntime.BuildContainerLogsDirectory("/var/log/kube/pods/", podNamespace, podName, podUID, containerName)
	metricsProvider := NewFakeMetricsDu(path, f.fakeStats[path])
	return fakeMetricsProvidersToStats([]volume.MetricsProvider{metricsProvider}, rootFsInfo)
}

func (f *fakeHostStatsProvider) getPodEtcHostsStats(podUID types.UID, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *fakeHostStatsProvider) getTerminationMessagePathStats(path string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	return nil, fmt.Errorf("not implemented")
}

func fakeMetricsProvidersToStats(metricsProviders []volume.MetricsProvider, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	result := rootFsInfoToFsStats(rootFsInfo)
	for i, metricsProvider := range metricsProviders {
		hostMetrics, err := metricsProvider.GetMetrics()
		if err != nil {
			return nil, fmt.Errorf("failed to get stats for item %d: %v", i, err)
		}
		usedBytes := uint64(hostMetrics.Used.Value())
		inodesUsed := uint64(hostMetrics.InodesUsed.Value())
		result.UsedBytes = addUsage(result.UsedBytes, &usedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, &inodesUsed)
		result.Time = maxUpdateTime(&result.Time, &hostMetrics.Time)
	}
	return result, nil
}

type fakeMetricsDu struct {
	fakeStats *volume.Metrics
}

// NewFakeMetricsDu inserts fake statistics when asked for metrics
func NewFakeMetricsDu(path string, stats *volume.Metrics) volume.MetricsProvider {
	return &fakeMetricsDu{fakeStats: stats}
}

func (f *fakeMetricsDu) GetMetrics() (*volume.Metrics, error) {
	if f.fakeStats == nil {
		return nil, fmt.Errorf("no stats provided")
	}
	return f.fakeStats, nil
}
