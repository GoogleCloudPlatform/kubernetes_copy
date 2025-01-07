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

package state

import (
	"fmt"
	"path"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
)

var _ State = &stateCheckpoint{}

type stateCheckpoint struct {
	mux               sync.RWMutex
	cache             State
	checkpointManager checkpointmanager.CheckpointManager
	checkpointName    string
}

// NewStateCheckpoint creates new State for keeping track of pod resource allocations with checkpoint backend
func NewStateCheckpoint(stateDir, checkpointName string) (State, error) {
	checkpointManager, err := checkpointmanager.NewCheckpointManager(stateDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize checkpoint manager for pod allocation tracking: %v", err)
	}

	praInfo, err := restoreState(checkpointManager, checkpointName)
	if err != nil {
		//lint:ignore ST1005 user-facing error message
		return nil, fmt.Errorf("could not restore state from checkpoint: %w, please drain this node and delete pod allocation checkpoint file %q before restarting Kubelet",
			err, path.Join(stateDir, checkpointName))
	}

	stateCheckpoint := &stateCheckpoint{
		cache:             NewStateMemory(praInfo.AllocationEntries),
		checkpointManager: checkpointManager,
		checkpointName:    checkpointName,
	}
	return stateCheckpoint, nil
}

// restores state from a checkpoint and creates it if it doesn't exist
func restoreState(checkpointManager checkpointmanager.CheckpointManager, checkpointName string) (*PodResourceAllocationInfo, error) {
	var err error
	checkpoint := &Checkpoint{}

	if err = checkpointManager.GetCheckpoint(checkpointName, checkpoint); err != nil {
		if err == errors.ErrCheckpointNotFound {
			return &PodResourceAllocationInfo{
				AllocationEntries: make(map[string]map[string]v1.ResourceRequirements),
			}, nil
		}
		return nil, err
	}
	klog.V(2).InfoS("State checkpoint: restored pod resource allocation state from checkpoint")
	praInfo, err := checkpoint.GetPodResourceAllocationInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get pod resource allocation info: %w", err)
	}

	return praInfo, nil
}

// saves state to a checkpoint, caller is responsible for locking
func (sc *stateCheckpoint) storeState() error {
	podAllocation := sc.cache.GetPodResourceAllocation()

	checkpoint, err := NewCheckpoint(&PodResourceAllocationInfo{
		AllocationEntries: podAllocation,
	})
	if err != nil {
		return fmt.Errorf("failed to create checkpoint: %w", err)
	}
	err = sc.checkpointManager.CreateCheckpoint(sc.checkpointName, checkpoint)
	if err != nil {
		klog.ErrorS(err, "Failed to save pod allocation checkpoint")
		return err
	}
	return nil
}

// GetContainerResourceAllocation returns current resources allocated to a pod's container
func (sc *stateCheckpoint) GetContainerResourceAllocation(podUID string, containerName string) (v1.ResourceRequirements, bool) {
	sc.mux.RLock()
	defer sc.mux.RUnlock()
	return sc.cache.GetContainerResourceAllocation(podUID, containerName)
}

// GetPodResourceAllocation returns current pod resource allocation
func (sc *stateCheckpoint) GetPodResourceAllocation() PodResourceAllocation {
	sc.mux.RLock()
	defer sc.mux.RUnlock()
	return sc.cache.GetPodResourceAllocation()
}

// GetPodResizeStatus returns the last resize decision for a pod
func (sc *stateCheckpoint) GetPodResizeStatus(podUID string) v1.PodResizeStatus {
	sc.mux.RLock()
	defer sc.mux.RUnlock()
	return sc.cache.GetPodResizeStatus(podUID)
}

// SetContainerResourceAllocation sets resources allocated to a pod's container
func (sc *stateCheckpoint) SetContainerResourceAllocation(podUID string, containerName string, alloc v1.ResourceRequirements) error {
	sc.mux.Lock()
	defer sc.mux.Unlock()
	sc.cache.SetContainerResourceAllocation(podUID, containerName, alloc)
	return sc.storeState()
}

// SetPodResourceAllocation sets pod resource allocation
func (sc *stateCheckpoint) SetPodResourceAllocation(podUID string, alloc map[string]v1.ResourceRequirements) error {
	sc.mux.Lock()
	defer sc.mux.Unlock()
	err := sc.cache.SetPodResourceAllocation(podUID, alloc)
	if err != nil {
		return err
	}
	return sc.storeState()
}

// SetPodResizeStatus sets the last resize decision for a pod
func (sc *stateCheckpoint) SetPodResizeStatus(podUID string, resizeStatus v1.PodResizeStatus) {
	sc.mux.Lock()
	defer sc.mux.Unlock()
	sc.cache.SetPodResizeStatus(podUID, resizeStatus)
}

// Delete deletes allocations for specified pod
func (sc *stateCheckpoint) Delete(podUID string, containerName string) error {
	sc.mux.Lock()
	defer sc.mux.Unlock()
	sc.cache.Delete(podUID, containerName)
	return sc.storeState()
}

type noopStateCheckpoint struct{}

// NewNoopStateCheckpoint creates a dummy state checkpoint manager
func NewNoopStateCheckpoint() State {
	return &noopStateCheckpoint{}
}

func (sc *noopStateCheckpoint) GetContainerResourceAllocation(_ string, _ string) (v1.ResourceRequirements, bool) {
	return v1.ResourceRequirements{}, false
}

func (sc *noopStateCheckpoint) GetPodResourceAllocation() PodResourceAllocation {
	return nil
}

func (sc *noopStateCheckpoint) GetPodResizeStatus(_ string) v1.PodResizeStatus {
	return ""
}

func (sc *noopStateCheckpoint) SetContainerResourceAllocation(_ string, _ string, _ v1.ResourceRequirements) error {
	return nil
}

func (sc *noopStateCheckpoint) SetPodResourceAllocation(_ string, _ map[string]v1.ResourceRequirements) error {
	return nil
}

func (sc *noopStateCheckpoint) SetPodResizeStatus(_ string, _ v1.PodResizeStatus) {}

func (sc *noopStateCheckpoint) Delete(_ string, _ string) error {
	return nil
}
