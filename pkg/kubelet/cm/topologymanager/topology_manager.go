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

package topologymanager

import (
	"fmt"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

const (
	// defaultMaxAllowableNUMANodes specifies the maximum number of NUMA Nodes that
	// the TopologyManager supports on the underlying machine.
	//
	// At present, having more than this number of NUMA Nodes will result in a
	// state explosion when trying to enumerate possible NUMAAffinity masks and
	// generate hints for them. As such, if more NUMA Nodes than this are
	// present on a machine and the TopologyManager is enabled, an error will
	// be returned and the TopologyManager will not be loaded.
	defaultMaxAllowableNUMANodes = 8
	// ErrorTopologyAffinity represents the type for a TopologyAffinityError
	ErrorTopologyAffinity = "TopologyAffinityError"
)

// TopologyAffinityError represents an resource alignment error
type TopologyAffinityError struct{}

func (e TopologyAffinityError) Error() string {
	return "Resources cannot be allocated with Topology locality"
}

func (e TopologyAffinityError) Type() string {
	return ErrorTopologyAffinity
}

// Manager interface provides methods for Kubelet to manage pod topology hints
type Manager interface {
	// PodAdmitHandler is implemented by Manager
	lifecycle.PodAdmitHandler
	// AddHintProvider adds a hint provider to manager to indicate the hint provider
	// wants to be consulted with when making topology hints
	AddHintProvider(HintProvider)
	// AddContainer adds pod to Manager for tracking
	AddContainer(pod *v1.Pod, container *v1.Container, containerID string)
	// RemoveContainer removes pod from Manager tracking
	RemoveContainer(containerID string) error
	// Store is the interface for storing pod topology hints
	Store
}

type manager struct {
	//Topology Manager Scope
	scope Scope
}

// HintProvider is an interface for components that want to collaborate to
// achieve globally optimal concrete resource alignment with respect to
// NUMA locality.
type HintProvider interface {
	// GetTopologyHints returns a map of resource names to a list of possible
	// concrete resource allocations in terms of NUMA locality hints. Each hint
	// is optionally marked "preferred" and indicates the set of NUMA nodes
	// involved in the hypothetical allocation. The topology manager calls
	// this function for each hint provider, and merges the hints to produce
	// a consensus "best" hint. The hint providers may subsequently query the
	// topology manager to influence actual resource assignment.
	GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]TopologyHint
	// GetPodTopologyHints returns a map of resource names to a list of possible
	// concrete resource allocations per Pod in terms of NUMA locality hints.
	GetPodTopologyHints(pod *v1.Pod) map[string][]TopologyHint
	// Allocate triggers resource allocation to occur on the HintProvider after
	// all hints have been gathered and the aggregated Hint is available via a
	// call to Store.GetAffinity().
	Allocate(pod *v1.Pod, container *v1.Container) error
}

// Store interface is to allow Hint Providers to retrieve pod affinity
type Store interface {
	GetAffinity(podUID string, containerName string) TopologyHint
	GetPolicy() Policy
}

// TopologyHint is a struct containing the NUMANodeAffinity for a Container
type TopologyHint struct {
	NUMANodeAffinity bitmask.BitMask
	// Preferred is set to true when the NUMANodeAffinity encodes a preferred
	// allocation for the Container. It is set to false otherwise.
	Preferred bool
}

// IsEqual checks if TopologyHint are equal
func (th *TopologyHint) IsEqual(topologyHint TopologyHint) bool {
	if th.Preferred == topologyHint.Preferred {
		if th.NUMANodeAffinity == nil || topologyHint.NUMANodeAffinity == nil {
			return th.NUMANodeAffinity == topologyHint.NUMANodeAffinity
		}
		return th.NUMANodeAffinity.IsEqual(topologyHint.NUMANodeAffinity)
	}
	return false
}

// LessThan checks if TopologyHint `a` is less than TopologyHint `b`
// this means that either `a` is a preferred hint and `b` is not
// or `a` NUMANodeAffinity attribute is narrower than `b` NUMANodeAffinity attribute.
func (th *TopologyHint) LessThan(other TopologyHint) bool {
	if th.Preferred != other.Preferred {
		return th.Preferred
	}
	return th.NUMANodeAffinity.IsNarrowerThan(other.NUMANodeAffinity)
}

var _ Manager = &manager{}

// NewManager creates a new TopologyManager based on provided policy and scope
func NewManager(topology []cadvisorapi.Node, topologyPolicyName string, topologyScopeName string, topologyPolicyOptions map[string]string) (Manager, error) {
	// When policy is none, the scope is not relevant, so we can short circuit here.
	if topologyPolicyName == PolicyNone {
		klog.InfoS("Creating topology manager with none policy")
		return &manager{scope: NewNoneScope()}, nil
	}

	opts, err := NewPolicyOptions(topologyPolicyOptions)
	if err != nil {
		return nil, err
	}

	klog.InfoS("Creating topology manager with policy per scope", "topologyPolicyName", topologyPolicyName, "topologyScopeName", topologyScopeName, "topologyPolicyOptions", opts)

	numaInfo, err := NewNUMAInfo(topology, opts)
	if err != nil {
		return nil, fmt.Errorf("cannot discover NUMA topology: %w", err)
	}

	if len(numaInfo.Nodes) > opts.MaxAllowableNUMANodes {
		return nil, fmt.Errorf("unsupported on machines with more than %v NUMA Nodes", opts.MaxAllowableNUMANodes)
	}

	var policy Policy
	switch topologyPolicyName {

	case PolicyBestEffort:
		policy = NewBestEffortPolicy(numaInfo, opts)

	case PolicyRestricted:
		policy = NewRestrictedPolicy(numaInfo, opts)

	case PolicySingleNumaNode:
		policy = NewSingleNumaNodePolicy(numaInfo, opts)

	default:
		return nil, fmt.Errorf("unknown policy: \"%s\"", topologyPolicyName)
	}

	var scope Scope
	switch topologyScopeName {

	case containerTopologyScope:
		scope = NewContainerScope(policy)

	case podTopologyScope:
		scope = NewPodScope(policy)

	default:
		return nil, fmt.Errorf("unknown scope: \"%s\"", topologyScopeName)
	}

	manager := &manager{
		scope: scope,
	}

	return manager, nil
}

func (m *manager) GetAffinity(podUID string, containerName string) TopologyHint {
	return m.scope.GetAffinity(podUID, containerName)
}

func (m *manager) GetPolicy() Policy {
	return m.scope.GetPolicy()
}

func (m *manager) AddHintProvider(h HintProvider) {
	m.scope.AddHintProvider(h)
}

func (m *manager) AddContainer(pod *v1.Pod, container *v1.Container, containerID string) {
	m.scope.AddContainer(pod, container, containerID)
}

func (m *manager) RemoveContainer(containerID string) error {
	return m.scope.RemoveContainer(containerID)
}

func (m *manager) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	metrics.TopologyManagerAdmissionRequestsTotal.Inc()

	startTime := time.Now()
	podAdmitResult := m.scope.Admit(attrs.Pod)
	metrics.TopologyManagerAdmissionDuration.Observe(float64(time.Since(startTime).Milliseconds()))

	return podAdmitResult
}
