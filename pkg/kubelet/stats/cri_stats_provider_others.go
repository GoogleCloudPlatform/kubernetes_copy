//go:build !linux && !windows
// +build !linux,!windows

/*
Copyright 2019 The Kubernetes Authors.

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
	"context"

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// listContainerNetworkStats returns the network stats of all the running containers.
// It should return (nil, nil) for platforms other than Windows.
func (p *criStatsProvider) listContainerNetworkStats() (map[string]*statsapi.NetworkStats, error) {
	return nil, nil
}

func (p *criStatsProvider) addCRIPodContainerStats(ctx context.Context, criSandboxStat *runtimeapi.PodSandboxStats,
	ps *statsapi.PodStats, fsIDtoInfo map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo,
	containerMap map[string]*runtimeapi.Container,
	podSandbox *runtimeapi.PodSandbox,
	rootFsInfo *cadvisorapiv2.FsInfo, updateCPUNanoCoreUsage bool) error {
	return nil
}

func addCRIPodNetworkStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
}

func addCRIPodMemoryStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
}

func addCRIPodCPUStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
}

func addCRIPodProcessStats(ps *statsapi.PodStats, criPodStat *runtimeapi.PodSandboxStats) {
}
