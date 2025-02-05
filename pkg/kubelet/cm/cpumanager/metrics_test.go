/*
Copyright 2025 The Kubernetes Authors.

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

package cpumanager

import (
	"testing"

	"github.com/blang/semver/v4"

	metrics2 "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/utils/cpuset"
)

func TestMetricsAllocate(t *testing.T) {
	// NOTE: metrics.ContainerAlignedComputeResources is a global metric that could be accessed by multiple tests.
	// Currently, no other tests increment this metric. To prevent potential race conditions:
	// - Avoid running tests that interact with this metric in parallel.
	// - Consider implementing a test-specific mutex to synchronize access to this global object when needed.

	// restore the metric after this UT.
	bakContainerAlignedComputeResources := metrics.ContainerAlignedComputeResources
	defer func() {
		metrics.ContainerAlignedComputeResources = bakContainerAlignedComputeResources
	}()

	metrics.ContainerAlignedComputeResources = metrics2.NewCounterVec(
		&metrics2.CounterOpts{
			Subsystem:      metrics.KubeletSubsystem,
			Name:           metrics.ContainerAlignedComputeResourcesNameKey,
			Help:           "Cumulative number of aligned compute resources allocated to containers by alignment type.",
			StabilityLevel: metrics2.ALPHA,
		},
		[]string{metrics.ContainerAlignedComputeResourcesScopeLabelKey, metrics.ContainerAlignedComputeResourcesBoundaryLabelKey},
	)
	metrics.ContainerAlignedComputeResources.Create(&semver.SpecVersion)

	testCases := []struct {
		staticPolicyTest
		expCounterMetricValue float64
	}{
		{
			staticPolicyTest: staticPolicyTest{
				description: "SocketNoSMT",
				topo:        topoUncoreDualSocketNoSMT,
				pod: makeMultiContainerPod(
					[]struct{ request, limit string }{
						{"4000m", "4000m"}},
					[]struct{ request, limit string }{
						{"2000m", "2000m"}}),
				containerName:   "initContainer-0",
				stAssignments:   state.ContainerCPUAssignments{},
				stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			},
			expCounterMetricValue: 2,
		},
		{
			staticPolicyTest: staticPolicyTest{
				description: "SingleSocketHT",
				topo:        topoSingleSocketHT,
				pod: makeMultiContainerPod(
					[]struct{ request, limit string }{
						{"4000m", "4000m"}},
					[]struct{ request, limit string }{
						{"2000m", "2000m"}}),
				containerName:   "initContainer-0",
				stAssignments:   state.ContainerCPUAssignments{},
				stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			},
			expCounterMetricValue: 0,
		},
		{
			staticPolicyTest: staticPolicyTest{
				description: "SocketNoSMT",
				topo:        topoUncoreDualSocketNoSMT,
				pod: makeMultiContainerPod(
					[]struct{ request, limit string }{},
					[]struct{ request, limit string }{
						{"1000m", "1000m"}}),
				containerName:   "initContainer-0",
				stAssignments:   state.ContainerCPUAssignments{},
				stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			},
			expCounterMetricValue: 1,
		},
		{
			staticPolicyTest: staticPolicyTest{
				description: "SingleSocketHT",
				topo:        topoSingleSocketHT,
				pod: makeMultiContainerPod(
					[]struct{ request, limit string }{},
					[]struct{ request, limit string }{
						{"1000m", "1000m"}}),
				containerName:   "initContainer-0",
				stAssignments:   state.ContainerCPUAssignments{},
				stDefaultCPUSet: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			},
			expCounterMetricValue: 0,
		},
	}

	for _, testCase := range testCases {
		policy, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, cpuset.New(), topologymanager.NewFakeManager(), nil)

		st := &mockState{
			assignments:   testCase.stAssignments,
			defaultCPUSet: testCase.stDefaultCPUSet,
		}

		pod := testCase.pod // shortcut

		metrics.ContainerAlignedComputeResources.ClearState()
		metrics.ContainerAlignedComputeResources.Create(&semver.SpecVersion)

		// allocate
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			if err := policy.Allocate(st, pod, &container); err != nil {
				t.Errorf("StaticPolicy Allocate() error (%v).", err)
			}
		}

		mValue, err := testutil.GetCounterMetricValue(metrics.ContainerAlignedComputeResources.WithLabelValues(metrics.AlignScopeContainer, metrics.AlignedPhysicalCPU))
		if err != nil {
			t.Errorf("StaticPolicy GetCounterMetricValue() error (%v)", err)
		}

		if mValue != testCase.expCounterMetricValue {
			t.Errorf("expected counter metric value  %v but got %v", testCase.expCounterMetricValue, mValue)
		}
	}
}
