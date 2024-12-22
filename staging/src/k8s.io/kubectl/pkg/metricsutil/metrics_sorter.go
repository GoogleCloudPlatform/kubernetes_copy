/*
Copyright 2021 The Kubernetes Authors.

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

package metricsutil

import (
	"k8s.io/api/core/v1"
	metricsapi "k8s.io/metrics/pkg/apis/metrics"
)

type NodeMetricsSorter struct {
	metrics []metricsapi.NodeMetrics
	sortBy  string
}

func (n *NodeMetricsSorter) Len() int {
	return len(n.metrics)
}

func (n *NodeMetricsSorter) Swap(i, j int) {
	n.metrics[i], n.metrics[j] = n.metrics[j], n.metrics[i]
}

func (n *NodeMetricsSorter) Less(i, j int) bool {
	switch n.sortBy {
	case "cpu":
		return n.metrics[i].Usage.Cpu().MilliValue() > n.metrics[j].Usage.Cpu().MilliValue()
	case "memory":
		return n.metrics[i].Usage.Memory().Value() > n.metrics[j].Usage.Memory().Value()
	default:
		return n.metrics[i].Name < n.metrics[j].Name
	}
}

func NewNodeMetricsSorter(metrics []metricsapi.NodeMetrics, sortBy string) *NodeMetricsSorter {
	return &NodeMetricsSorter{
		metrics: metrics,
		sortBy:  sortBy,
	}
}

type PodMetricsSorter struct {
	metrics       []metricsapi.PodMetrics
	sortBy        string
	withNamespace bool
	podMetrics    []v1.ResourceList
}

func (p *PodMetricsSorter) Len() int {
	return len(p.metrics)
}

func (p *PodMetricsSorter) Swap(i, j int) {
	p.metrics[i], p.metrics[j] = p.metrics[j], p.metrics[i]
	p.podMetrics[i], p.podMetrics[j] = p.podMetrics[j], p.podMetrics[i]
}

func (p *PodMetricsSorter) Less(i, j int) bool {
	switch p.sortBy {
	case "cpu":
		return p.podMetrics[i].Cpu().MilliValue() > p.podMetrics[j].Cpu().MilliValue()
	case "memory":
		return p.podMetrics[i].Memory().Value() > p.podMetrics[j].Memory().Value()
	default:
		if p.withNamespace && p.metrics[i].Namespace != p.metrics[j].Namespace {
			return p.metrics[i].Namespace < p.metrics[j].Namespace
		}
		return p.metrics[i].Name < p.metrics[j].Name
	}
}

func NewPodMetricsSorter(metrics []metricsapi.PodMetrics, withNamespace bool, sortBy string, measuredResources []v1.ResourceName) *PodMetricsSorter {
	var podMetrics = make([]v1.ResourceList, len(metrics))
	if len(sortBy) > 0 {
		for i, v := range metrics {
			podMetrics[i] = getPodMetrics(&v, measuredResources)
		}
	}

	return &PodMetricsSorter{
		metrics:       metrics,
		sortBy:        sortBy,
		withNamespace: withNamespace,
		podMetrics:    podMetrics,
	}
}

type ContainerMetricsSorter struct {
	metrics []metricsapi.ContainerMetrics
	sortBy  string
}

func (s *ContainerMetricsSorter) Len() int {
	return len(s.metrics)
}

func (s *ContainerMetricsSorter) Swap(i, j int) {
	s.metrics[i], s.metrics[j] = s.metrics[j], s.metrics[i]
}

func (s *ContainerMetricsSorter) Less(i, j int) bool {
	switch s.sortBy {
	case "cpu":
		return s.metrics[i].Usage.Cpu().MilliValue() > s.metrics[j].Usage.Cpu().MilliValue()
	case "memory":
		return s.metrics[i].Usage.Memory().Value() > s.metrics[j].Usage.Memory().Value()
	default:
		return s.metrics[i].Name < s.metrics[j].Name
	}
}

func NewContainerMetricsSorter(metrics []metricsapi.ContainerMetrics, sortBy string) *ContainerMetricsSorter {
	return &ContainerMetricsSorter{
		metrics: metrics,
		sortBy:  sortBy,
	}
}
