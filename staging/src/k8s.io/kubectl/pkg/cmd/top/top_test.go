/*
Copyright 2016 The Kubernetes Authors.

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

package top

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	metricsv1beta1api "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

func TestTopSubcommandsExist(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)

	f := cmdtesting.NewTestFactory()
	defer f.Cleanup()

	cmd := NewCmdTop(f, genericiooptions.NewTestIOStreamsDiscard())
	if !cmd.HasSubCommands() {
		t.Error("top command should have subcommands")
	}
}

func marshallBody(metrics interface{}) (io.ReadCloser, error) {
	result, err := json.Marshal(metrics)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(result)), nil
}

func testNodeV1beta1MetricsData() (*metricsv1beta1api.NodeMetricsList, *v1.NodeList) {
	metrics := &metricsv1beta1api.NodeMetricsList{
		ListMeta: metav1.ListMeta{
			ResourceVersion: "1",
		},
		Items: []metricsv1beta1api.NodeMetrics{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", ResourceVersion: "10", Labels: map[string]string{"key": "value"}},
				Window:     metav1.Duration{Duration: time.Minute},
				Usage: v1.ResourceList{
					v1.ResourceCPU:     *resource.NewMilliQuantity(1, resource.DecimalSI),
					"swap":             *resource.NewQuantity(1*(1024*1024), resource.DecimalSI),
					v1.ResourceMemory:  *resource.NewQuantity(2*(1024*1024), resource.DecimalSI),
					v1.ResourceStorage: *resource.NewQuantity(3*(1024*1024), resource.DecimalSI),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node2", ResourceVersion: "11"},
				Window:     metav1.Duration{Duration: time.Minute},
				Usage: v1.ResourceList{
					v1.ResourceCPU:     *resource.NewMilliQuantity(5, resource.DecimalSI),
					"swap":             *resource.NewQuantity(2*(1024*1024), resource.DecimalSI),
					v1.ResourceMemory:  *resource.NewQuantity(6*(1024*1024), resource.DecimalSI),
					v1.ResourceStorage: *resource.NewQuantity(7*(1024*1024), resource.DecimalSI),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node3", ResourceVersion: "11"},
				Window:     metav1.Duration{Duration: time.Minute},
				Usage: v1.ResourceList{
					v1.ResourceCPU:     *resource.NewMilliQuantity(3, resource.DecimalSI),
					"swap":             *resource.NewQuantity(0, resource.DecimalSI),
					v1.ResourceMemory:  *resource.NewQuantity(4*(1024*1024), resource.DecimalSI),
					v1.ResourceStorage: *resource.NewQuantity(5*(1024*1024), resource.DecimalSI),
				},
			},
		},
	}
	nodes := &v1.NodeList{
		ListMeta: metav1.ListMeta{
			ResourceVersion: "15",
		},
		Items: []v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", ResourceVersion: "10"},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:     *resource.NewMilliQuantity(10, resource.DecimalSI),
						v1.ResourceMemory:  *resource.NewQuantity(20*(1024*1024), resource.DecimalSI),
						v1.ResourceStorage: *resource.NewQuantity(30*(1024*1024), resource.DecimalSI),
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node2", ResourceVersion: "11"},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:     *resource.NewMilliQuantity(50, resource.DecimalSI),
						v1.ResourceMemory:  *resource.NewQuantity(60*(1024*1024), resource.DecimalSI),
						v1.ResourceStorage: *resource.NewQuantity(70*(1024*1024), resource.DecimalSI),
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node3", ResourceVersion: "11"},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:     *resource.NewMilliQuantity(30, resource.DecimalSI),
						v1.ResourceMemory:  *resource.NewQuantity(40*(1024*1024), resource.DecimalSI),
						v1.ResourceStorage: *resource.NewQuantity(50*(1024*1024), resource.DecimalSI),
					},
				},
			},
		},
	}
	return metrics, nodes
}
