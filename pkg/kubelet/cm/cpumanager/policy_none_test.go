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

package cpumanager

import (
	"context"
	"testing"

	"k8s.io/klog/v2/ktesting"
	_ "k8s.io/klog/v2/ktesting/init"

	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/utils/cpuset"
)

func TestNonePolicyName(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	policy := &nonePolicy{
		logger: logger,
	}

	policyName := policy.Name()
	if policyName != "none" {
		t.Errorf("NonePolicy Name() error. expected: none, returned: %v", policyName)
	}
}

func TestNonePolicyAllocate(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	policy := &nonePolicy{
		logger: logger,
	}

	st := &mockState{
		assignments:   state.ContainerCPUAssignments{},
		defaultCPUSet: cpuset.New(1, 2, 3, 4, 5, 6, 7),
	}

	testPod := makePod("fakePod", "fakeContainer", "1000m", "1000m")

	container := &testPod.Spec.Containers[0]
	err := policy.Allocate(ctx, st, testPod, container)
	if err != nil {
		t.Errorf("NonePolicy Allocate() error. expected no error but got: %v", err)
	}
}

func TestNonePolicyRemove(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	policy := &nonePolicy{
		logger: logger,
	}

	st := &mockState{
		assignments:   state.ContainerCPUAssignments{},
		defaultCPUSet: cpuset.New(1, 2, 3, 4, 5, 6, 7),
	}

	testPod := makePod("fakePod", "fakeContainer", "1000m", "1000m")

	container := &testPod.Spec.Containers[0]
	err := policy.RemoveContainer(context.Background(), st, string(testPod.UID), container.Name)
	if err != nil {
		t.Errorf("NonePolicy RemoveContainer() error. expected no error but got %v", err)
	}
}

func TestNonePolicyGetAllocatableCPUs(t *testing.T) {
	// any random topology is fine

	var cpuIDs []int
	for cpuID := range topoSingleSocketHT.CPUDetails {
		cpuIDs = append(cpuIDs, cpuID)
	}

	logger, _ := ktesting.NewTestContext(t)
	policy := &nonePolicy{
		logger: logger,
	}

	st := &mockState{
		assignments:   state.ContainerCPUAssignments{},
		defaultCPUSet: cpuset.New(cpuIDs...),
	}

	cpus := policy.GetAllocatableCPUs(context.Background(), st)
	if cpus.Size() != 0 {
		t.Errorf("NonePolicy GetAllocatableCPUs() error. expected empty set, returned: %v", cpus)
	}
}

func TestNonePolicyOptions(t *testing.T) {
	var err error

	_, ctx := ktesting.NewTestContext(t)
	_, err = NewNonePolicy(ctx, nil)
	if err != nil {
		t.Errorf("NewNonePolicy with nil options failure. expected no error but got: %v", err)
	}

	opts := map[string]string{
		FullPCPUsOnlyOption: "true",
	}
	_, err = NewNonePolicy(ctx, opts)
	if err == nil {
		t.Errorf("NewNonePolicy with (any) options failure. expected error but got none")
	}
}
