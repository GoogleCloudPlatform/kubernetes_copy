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

package metricsutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	metricsapi "k8s.io/metrics/pkg/apis/metrics"
)

func TestPrintNodeMetrics(t *testing.T) {

	tests := []struct {
		name           string
		nodeMetric     []metricsapi.NodeMetrics
		nodes          []*v1.Node
		noHeader       bool
		sortBy         string
		expectedErr    error
		expectedOutput string
	}{
		{
			name:  "Single node with default header",
			nodes: []*v1.Node{newNode("node1")},
			nodeMetric: []metricsapi.NodeMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node1", ResourceVersion: "10", Labels: map[string]string{"key": "value"}},
					Window:     metav1.Duration{Duration: time.Minute},
					Usage: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
			expectedOutput: `NAME    CPU(cores)   CPU(%)   MEMORY(bytes)   MEMORY(%)   
node1   1000m        10%      1024Mi          10%         
`,
		},
		{
			name:  "Single node without header",
			nodes: []*v1.Node{newNode("node1")},
			nodeMetric: []metricsapi.NodeMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node1", ResourceVersion: "10", Labels: map[string]string{"key": "value"}},
					Window:     metav1.Duration{Duration: time.Minute},
					Usage: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
			noHeader: true,
			expectedOutput: `node1   1000m   10%   1024Mi   10%   
`,
		},
		{
			name:  "Multiple nodes with one missing metrics",
			nodes: []*v1.Node{newNode("node1"), newNode("node2")},
			nodeMetric: []metricsapi.NodeMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node1", ResourceVersion: "10", Labels: map[string]string{"key": "value"}},
					Window:     metav1.Duration{Duration: time.Minute},
					Usage: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
			expectedOutput: `NAME    CPU(cores)   CPU(%)      MEMORY(bytes)   MEMORY(%)   
node1   1000m        10%         1024Mi          10%         
node2   <unknown>    <unknown>   <unknown>       <unknown>   
`,
		},
		{
			name:  "Multiple nodes with metrics sorted by memory",
			nodes: []*v1.Node{newNode("node1"), newNode("node2")},
			nodeMetric: []metricsapi.NodeMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node1", ResourceVersion: "10", Labels: map[string]string{"key": "value"}},
					Window:     metav1.Duration{Duration: time.Minute},
					Usage: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node2", ResourceVersion: "10", Labels: map[string]string{"key": "value"}},
					Window:     metav1.Duration{Duration: time.Minute},
					Usage: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
				},
			},
			sortBy: "memory",
			expectedOutput: `NAME    CPU(cores)   CPU(%)   MEMORY(bytes)   MEMORY(%)   
node2   2000m        20%      5120Mi          50%         
node1   1000m        10%      1024Mi          10%         
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a new TopCmdPrinter with a test writer.
			_, _, out, _ := genericiooptions.NewTestIOStreams()

			availableResources := make(map[string]v1.ResourceList)
			for _, n := range test.nodes {
				availableResources[n.Name] = n.Status.Capacity
			}
			top := NewTopCmdPrinter(out, false)
			err := top.PrintNodeMetrics(test.nodeMetric, availableResources, test.noHeader, test.sortBy)
			assert.Equal(t, test.expectedErr, err)
			assert.Equal(t, test.expectedOutput, out.String())
		})
	}
}

func TestPrintPodMetrics(t *testing.T) {

	tests := []struct {
		name            string
		podMetric       []metricsapi.PodMetrics
		printContainers bool
		withNamespace   bool
		noHeader        bool
		sortBy          string
		sum             bool
		expectedErr     error
		expectedOutput  string
	}{
		{
			name: "Single Pod with Multiple Containers",
			podMetric: []metricsapi.PodMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "default",
					},
					Timestamp: metav1.Time{Time: time.Now()},
					Window:    metav1.Duration{Duration: time.Minute},
					Containers: []metricsapi.ContainerMetrics{
						{
							Name: "container1",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						{
							Name: "container2",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			expectedOutput: `NAME   CPU(cores)   MEMORY(bytes)   
test   400m         2048Mi          
`,
		},
		{
			name: "Single Pod with Multiple Containers - Print Containers",
			podMetric: []metricsapi.PodMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "default",
					},
					Timestamp: metav1.Time{Time: time.Now()},
					Window:    metav1.Duration{Duration: time.Minute},
					Containers: []metricsapi.ContainerMetrics{
						{
							Name: "container1",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						{
							Name: "container2",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			printContainers: true,
			expectedOutput: `POD    NAME         CPU(cores)   MEMORY(bytes)   
test   container1   200m         1024Mi          
test   container2   200m         1024Mi          
`,
		},
		{
			name: "Single Pod with Multiple Containers - Calculate Resource Usage Without Header",
			podMetric: []metricsapi.PodMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "default",
					},
					Timestamp: metav1.Time{Time: time.Now()},
					Window:    metav1.Duration{Duration: time.Minute},
					Containers: []metricsapi.ContainerMetrics{
						{
							Name: "container1",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						{
							Name: "container2",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			noHeader: true,
			expectedOutput: `test   400m   2048Mi   
`,
		},
		{
			name: "Multiple Pods with Multiple Containers - Sort by Memory Usage",
			podMetric: []metricsapi.PodMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "default",
					},
					Timestamp: metav1.Time{Time: time.Now()},
					Window:    metav1.Duration{Duration: time.Minute},
					Containers: []metricsapi.ContainerMetrics{
						{
							Name: "container1",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						{
							Name: "container2",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1",
						Namespace: "default",
					},
					Timestamp: metav1.Time{Time: time.Now()},
					Window:    metav1.Duration{Duration: time.Minute},
					Containers: []metricsapi.ContainerMetrics{
						{
							Name: "container1",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("3Gi"),
							},
						},
						{
							Name: "container2",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					},
				},
			},
			sortBy: "memory",
			expectedOutput: `NAME     CPU(cores)   MEMORY(bytes)   
test-1   400m         5120Mi          
test     400m         2048Mi          
`,
		},
		{
			name: "Multiple Pods with Multiple Containers - Calculate Total Resource Usage",
			podMetric: []metricsapi.PodMetrics{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "default",
					},
					Timestamp: metav1.Time{Time: time.Now()},
					Window:    metav1.Duration{Duration: time.Minute},
					Containers: []metricsapi.ContainerMetrics{
						{
							Name: "container1",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						{
							Name: "container2",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1",
						Namespace: "default",
					},
					Timestamp: metav1.Time{Time: time.Now()},
					Window:    metav1.Duration{Duration: time.Minute},
					Containers: []metricsapi.ContainerMetrics{
						{
							Name: "container1",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("3Gi"),
							},
						},
						{
							Name: "container2",
							Usage: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("0.2"),
								v1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					},
				},
			},
			sum: true,
			expectedOutput: `NAME     CPU(cores)   MEMORY(bytes)   
test     400m         2048Mi          
test-1   400m         5120Mi          
         ________     ________        
         800m         7168Mi          
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a new TopCmdPrinter with a test writer.
			_, _, out, _ := genericiooptions.NewTestIOStreams()

			top := NewTopCmdPrinter(out, false)
			err := top.PrintPodMetrics(test.podMetric, test.printContainers,
				test.withNamespace, test.noHeader, test.sortBy, test.sum)
			assert.Equal(t, test.expectedErr, err)
			assert.Equal(t, test.expectedOutput, out.String())
		})
	}
}

func newNode(name string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("10"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("10Gi"),
			},
		},
	}
}
