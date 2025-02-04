/*
Copyright 2015 The Kubernetes Authors.

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

package testing

import (
	"context"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

type FakeMirrorClient struct {
	mirrorPodLock sync.RWMutex
	// Note that a real mirror manager does not store the mirror pods in
	// itself. This fake manager does this to track calls.
	mirrorPods   sets.Set[string]
	createCounts map[string]int
	deleteCounts map[string]int
}

func NewFakeMirrorClient() *FakeMirrorClient {
	m := FakeMirrorClient{}
	m.mirrorPods = sets.New[string]()
	m.createCounts = make(map[string]int)
	m.deleteCounts = make(map[string]int)
	return &m
}

func (fmc *FakeMirrorClient) CreateMirrorPod(_ context.Context, pod *v1.Pod) error {
	fmc.mirrorPodLock.Lock()
	defer fmc.mirrorPodLock.Unlock()
	podFullName := kubecontainer.GetPodFullName(pod)
	fmc.mirrorPods.Insert(podFullName)
	fmc.createCounts[podFullName]++
	return nil
}

// TODO (Robert Krawitz): Implement UID checking
func (fmc *FakeMirrorClient) DeleteMirrorPod(_ context.Context, podFullName string, _ *types.UID) (bool, error) {
	fmc.mirrorPodLock.Lock()
	defer fmc.mirrorPodLock.Unlock()
	fmc.mirrorPods.Delete(podFullName)
	fmc.deleteCounts[podFullName]++
	return true, nil
}

func (fmc *FakeMirrorClient) HasPod(podFullName string) bool {
	fmc.mirrorPodLock.RLock()
	defer fmc.mirrorPodLock.RUnlock()
	return fmc.mirrorPods.Has(podFullName)
}

func (fmc *FakeMirrorClient) NumOfPods() int {
	fmc.mirrorPodLock.RLock()
	defer fmc.mirrorPodLock.RUnlock()
	return fmc.mirrorPods.Len()
}

func (fmc *FakeMirrorClient) GetPods() []string {
	fmc.mirrorPodLock.RLock()
	defer fmc.mirrorPodLock.RUnlock()
	return sets.List(fmc.mirrorPods)
}

func (fmc *FakeMirrorClient) GetCounts(podFullName string) (int, int) {
	fmc.mirrorPodLock.RLock()
	defer fmc.mirrorPodLock.RUnlock()
	return fmc.createCounts[podFullName], fmc.deleteCounts[podFullName]
}
