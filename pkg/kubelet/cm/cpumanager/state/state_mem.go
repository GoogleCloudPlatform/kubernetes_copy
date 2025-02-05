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

package state

import (
	"sync"

	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"
)

type stateMemory struct {
	sync.RWMutex
	assignments   ContainerCPUAssignments
	defaultCPUSet cpuset.CPUSet
}

var _ State = &stateMemory{}

// NewMemoryState creates new State for keeping track of cpu/pod assignment
func NewMemoryState() State {
	klog.InfoS("Initialized new in-memory state store")
	return &stateMemory{
		assignments:   ContainerCPUAssignments{},
		defaultCPUSet: cpuset.New(),
	}
}

func (s *stateMemory) GetCPUSet(podUID string, containerName string) (cpuset.CPUSet, bool) {
	s.RLock()
	defer s.RUnlock()

	res, ok := s.assignments[podUID][containerName]
	return res.Clone(), ok
}

func (s *stateMemory) GetDefaultCPUSet() cpuset.CPUSet {
	s.RLock()
	defer s.RUnlock()

	return s.defaultCPUSet.Clone()
}

func (s *stateMemory) GetCPUSetOrDefault(podUID string, containerName string) cpuset.CPUSet {
	if res, ok := s.GetCPUSet(podUID, containerName); ok {
		return res
	}
	return s.GetDefaultCPUSet()
}

func (s *stateMemory) GetCPUAssignments() ContainerCPUAssignments {
	s.RLock()
	defer s.RUnlock()
	return s.assignments.Clone()
}

func (s *stateMemory) SetCPUSet(podUID string, containerName string, cset cpuset.CPUSet) {
	s.Lock()
	defer s.Unlock()

	if _, ok := s.assignments[podUID]; !ok {
		s.assignments[podUID] = make(map[string]cpuset.CPUSet)
	}

	s.assignments[podUID][containerName] = cset
	klog.InfoS("Updated desired CPUSet", "podUID", podUID, "containerName", containerName, "cpuSet", cset)
}

func (s *stateMemory) SetDefaultCPUSet(cset cpuset.CPUSet) {
	s.Lock()
	defer s.Unlock()

	s.defaultCPUSet = cset
	klog.InfoS("Updated default CPUSet", "cpuSet", cset)
}

func (s *stateMemory) SetCPUAssignments(a ContainerCPUAssignments) {
	s.Lock()
	defer s.Unlock()

	s.assignments = a.Clone()
	klog.InfoS("Updated CPUSet assignments", "assignments", a.String())
}

func (s *stateMemory) Delete(podUID string, containerName string) {
	s.Lock()
	defer s.Unlock()

	delete(s.assignments[podUID], containerName)
	if len(s.assignments[podUID]) == 0 {
		delete(s.assignments, podUID)
	}
	klog.V(2).InfoS("Deleted CPUSet assignment", "podUID", podUID, "containerName", containerName)
}

func (s *stateMemory) ClearState() {
	s.Lock()
	defer s.Unlock()

	s.defaultCPUSet = cpuset.CPUSet{}
	s.assignments = make(ContainerCPUAssignments)
	klog.V(2).InfoS("Cleared state")
}
