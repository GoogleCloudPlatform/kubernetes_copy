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
	"fmt"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/utils/cpuset"
)

type nonePolicy struct {
	logger logr.Logger
}

var _ Policy = &nonePolicy{}

// PolicyNone name of none policy
const PolicyNone policyName = "none"

// NewNonePolicy returns a cpuset manager policy that does nothing
func NewNonePolicy(ctx context.Context, cpuPolicyOptions map[string]string) (Policy, error) {
	if len(cpuPolicyOptions) > 0 {
		return nil, fmt.Errorf("None policy: received unsupported options=%v", cpuPolicyOptions)
	}
	return &nonePolicy{
		logger: klog.FromContext(ctx),
	}, nil
}

func (p *nonePolicy) Name() string {
	return string(PolicyNone)
}

func (p *nonePolicy) Start(_ context.Context, s state.State) error {
	p.logger.Info("Start")
	return nil
}

func (p *nonePolicy) Allocate(_ context.Context, s state.State, pod *v1.Pod, container *v1.Container) error {
	return nil
}

func (p *nonePolicy) RemoveContainer(_ context.Context, s state.State, podUID string, containerName string) error {
	return nil
}

func (p *nonePolicy) GetTopologyHints(_ context.Context, s state.State, pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	return nil
}

func (p *nonePolicy) GetPodTopologyHints(_ context.Context, s state.State, pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	return nil
}

// Assignable CPUs are the ones that can be exclusively allocated to pods that meet the exclusivity requirement
// (ie guaranteed QoS class and integral CPU request).
// Assignability of CPUs as a concept is only applicable in case of static policy i.e. scenarios where workloads
// CAN get exclusive access to core(s).
// Hence, we return empty set here: no cpus are assignable according to above definition with this policy.
func (p *nonePolicy) GetAllocatableCPUs(_ context.Context, m state.State) cpuset.CPUSet {
	return cpuset.New()
}
