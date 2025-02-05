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

package volumemanager

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/volume/util/types"
)

// FakeVolumeManager is a test implementation that just tracks calls
type FakeVolumeManager struct {
	volumes       map[v1.UniqueVolumeName]bool
	reportedInUse map[v1.UniqueVolumeName]bool
	unmountDelay  time.Duration
	unmountError  error
}

var _ VolumeManager = &FakeVolumeManager{}

// NewFakeVolumeManager creates a new VolumeManager test instance
func NewFakeVolumeManager(initialVolumes []v1.UniqueVolumeName, unmountDelay time.Duration, unmountError error) *FakeVolumeManager {
	volumes := map[v1.UniqueVolumeName]bool{}
	for _, v := range initialVolumes {
		volumes[v] = true
	}
	return &FakeVolumeManager{
		volumes:       volumes,
		reportedInUse: map[v1.UniqueVolumeName]bool{},
		unmountDelay:  unmountDelay,
		unmountError:  unmountError,
	}
}

// Run is not implemented
func (f *FakeVolumeManager) Run(ctx context.Context, sourcesReady config.SourcesReady) {
}

// WaitForAttachAndMount is not implemented
func (f *FakeVolumeManager) WaitForAttachAndMount(ctx context.Context, pod *v1.Pod) error {
	return nil
}

// WaitForUnmount is not implemented
func (f *FakeVolumeManager) WaitForUnmount(ctx context.Context, pod *v1.Pod) error {
	return nil
}

func (f *FakeVolumeManager) WaitForAllPodsUnmount(ctx context.Context, pods []*v1.Pod) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(f.unmountDelay):
		return f.unmountError
	}
}

// GetMountedVolumesForPod is not implemented
func (f *FakeVolumeManager) GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap {
	return nil
}

// HasPossiblyMountedVolumesForPod is not implemented
func (f *FakeVolumeManager) HasPossiblyMountedVolumesForPod(podName types.UniquePodName) bool {
	return false
}

// GetExtraSupplementalGroupsForPod is not implemented
func (f *FakeVolumeManager) GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64 {
	return nil
}

// GetVolumesInUse returns a list of the initial volumes
func (f *FakeVolumeManager) GetVolumesInUse() []v1.UniqueVolumeName {
	inuse := []v1.UniqueVolumeName{}
	for v := range f.volumes {
		inuse = append(inuse, v)
	}
	return inuse
}

// ReconcilerStatesHasBeenSynced is not implemented
func (f *FakeVolumeManager) ReconcilerStatesHasBeenSynced() bool {
	return true
}

// VolumeIsAttached is not implemented
func (f *FakeVolumeManager) VolumeIsAttached(volumeName v1.UniqueVolumeName) bool {
	return false
}

// MarkVolumesAsReportedInUse adds the given volumes to the reportedInUse map
func (f *FakeVolumeManager) MarkVolumesAsReportedInUse(volumesReportedAsInUse []v1.UniqueVolumeName) {
	for _, reportedVolume := range volumesReportedAsInUse {
		if _, ok := f.volumes[reportedVolume]; ok {
			f.reportedInUse[reportedVolume] = true
		}
	}
}

// GetVolumesReportedInUse is a test function only that returns a list of volumes
// from the reportedInUse map
func (f *FakeVolumeManager) GetVolumesReportedInUse() []v1.UniqueVolumeName {
	inuse := []v1.UniqueVolumeName{}
	for reportedVolume := range f.reportedInUse {
		inuse = append(inuse, reportedVolume)
	}
	return inuse
}
