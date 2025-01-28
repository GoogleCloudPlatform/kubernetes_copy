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

package stats

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	csipbv1 "github.com/container-storage-interface/spec/lib/go/csi"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/events"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubestats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/features"
	statstest "k8s.io/kubernetes/pkg/kubelet/server/stats/testing"
	"k8s.io/kubernetes/pkg/volume"
)

const (
	namespace0  = "test0"
	pName0      = "pod0"
	capacity    = int64(10000000)
	available   = int64(5000000)
	inodesTotal = int64(2000)
	inodesFree  = int64(1000)

	vol0          = "vol0"
	vol1          = "vol1"
	vol2          = "vol2"
	vol3          = "vol3"
	pvcClaimName0 = "pvc-fake0"
	pvcClaimName1 = "pvc-fake1"
)

var (
	ErrorWatchTimeout = errors.New("watch event timeout")
	// Create pod spec to test against
	podVolumes = []k8sv1.Volume{
		{
			Name: vol0,
			VolumeSource: k8sv1.VolumeSource{
				GCEPersistentDisk: &k8sv1.GCEPersistentDiskVolumeSource{
					PDName: "fake-device1",
				},
			},
		},
		{
			Name: vol1,
			VolumeSource: k8sv1.VolumeSource{
				PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcClaimName0,
				},
			},
		},
		{
			Name: vol2,
			VolumeSource: k8sv1.VolumeSource{
				PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcClaimName1,
				},
			},
		},
		{
			Name: vol3,
			VolumeSource: k8sv1.VolumeSource{
				Ephemeral: &k8sv1.EphemeralVolumeSource{},
			},
		},
	}

	fakePod = &k8sv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pName0,
			Namespace: namespace0,
			UID:       "UID" + pName0,
		},
		Spec: k8sv1.PodSpec{
			Volumes: podVolumes,
		},
	}

	volumeCondition = &csipbv1.VolumeCondition{}
)

func TestPVCRef(t *testing.T) {
	// Setup mock stats provider
	mockStats := statstest.NewMockProvider(t)
	volumes := map[string]volume.Volume{vol0: &fakeVolume{}, vol1: &fakeVolume{}, vol3: &fakeVolume{}}
	mockStats.EXPECT().ListVolumesForPod(fakePod.UID).Return(volumes, true)
	blockVolumes := map[string]volume.BlockVolume{vol2: &fakeBlockVolume{}}
	mockStats.EXPECT().ListBlockVolumesForPod(fakePod.UID).Return(blockVolumes, true)

	eventStore := make(chan string, 1)
	fakeEventRecorder := events.FakeRecorder{
		Events: eventStore,
	}

	// Calculate stats for pod
	statsCalculator := newVolumeStatCalculator(mockStats, time.Minute, fakePod, &fakeEventRecorder)
	statsCalculator.calcAndStoreStats()
	vs, _ := statsCalculator.GetLatest()

	assert.Len(t, append(vs.EphemeralVolumes, vs.PersistentVolumes...), 4)
	// Verify 'vol0' doesn't have a PVC reference
	assert.Contains(t, append(vs.EphemeralVolumes, vs.PersistentVolumes...), kubestats.VolumeStats{
		Name:              vol0,
		FsStats:           expectedFSStats(),
		VolumeHealthStats: expectedVolumeHealthStats(),
	})
	// Verify 'vol1' has a PVC reference
	assert.Contains(t, append(vs.EphemeralVolumes, vs.PersistentVolumes...), kubestats.VolumeStats{
		Name: vol1,
		PVCRef: &kubestats.PVCReference{
			Name:      pvcClaimName0,
			Namespace: namespace0,
		},
		FsStats:           expectedFSStats(),
		VolumeHealthStats: expectedVolumeHealthStats(),
	})
	// // Verify 'vol2' has a PVC reference
	assert.Contains(t, append(vs.EphemeralVolumes, vs.PersistentVolumes...), kubestats.VolumeStats{
		Name: vol2,
		PVCRef: &kubestats.PVCReference{
			Name:      pvcClaimName1,
			Namespace: namespace0,
		},
		FsStats:           expectedBlockStats(),
		VolumeHealthStats: expectedVolumeHealthStats(),
	})
	// Verify 'vol3' has a PVC reference
	assert.Contains(t, append(vs.EphemeralVolumes, vs.PersistentVolumes...), kubestats.VolumeStats{
		Name: vol3,
		PVCRef: &kubestats.PVCReference{
			Name:      pName0 + "-" + vol3,
			Namespace: namespace0,
		},
		FsStats:           expectedFSStats(),
		VolumeHealthStats: expectedVolumeHealthStats(),
	})
}

func TestNormalVolumeEvent(t *testing.T) {
	mockStats := statstest.NewMockProvider(t)

	volumes := map[string]volume.Volume{vol0: &fakeVolume{}, vol1: &fakeVolume{}}
	mockStats.EXPECT().ListVolumesForPod(fakePod.UID).Return(volumes, true)
	blockVolumes := map[string]volume.BlockVolume{vol2: &fakeBlockVolume{}}
	mockStats.EXPECT().ListBlockVolumesForPod(fakePod.UID).Return(blockVolumes, true)

	eventStore := make(chan string, 2)
	fakeEventRecorder := events.FakeRecorder{
		Events: eventStore,
	}

	// Calculate stats for pod
	statsCalculator := newVolumeStatCalculator(mockStats, time.Minute, fakePod, &fakeEventRecorder)
	statsCalculator.calcAndStoreStats()

	event, err := WatchEvent(eventStore)
	assert.Error(t, err)
	assert.Equal(t, "", event)
}

func TestAbnormalVolumeEvent(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CSIVolumeHealth, true)

	// Setup mock stats provider
	mockStats := statstest.NewMockProvider(t)
	volumes := map[string]volume.Volume{vol0: &fakeVolume{}}
	mockStats.EXPECT().ListVolumesForPod(fakePod.UID).Return(volumes, true)
	blockVolumes := map[string]volume.BlockVolume{vol1: &fakeBlockVolume{}}
	mockStats.EXPECT().ListBlockVolumesForPod(fakePod.UID).Return(blockVolumes, true)

	eventStore := make(chan string, 2)
	fakeEventRecorder := events.FakeRecorder{
		Events: eventStore,
	}

	// Calculate stats for pod
	if volumeCondition != nil {
		volumeCondition.Message = "The target path of the volume doesn't exist"
		volumeCondition.Abnormal = true
	}
	statsCalculator := newVolumeStatCalculator(mockStats, time.Minute, fakePod, &fakeEventRecorder)
	statsCalculator.calcAndStoreStats()

	event, err := WatchEvent(eventStore)
	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Warning VolumeConditionAbnormal Volume %s: The target path of the volume doesn't exist", "vol0"), event)
}

func WatchEvent(eventChan <-chan string) (string, error) {
	select {
	case event := <-eventChan:
		return event, nil
	case <-time.After(5 * time.Second):
		return "", ErrorWatchTimeout
	}
}

// Fake volume/metrics provider
var _ volume.Volume = &fakeVolume{}

type fakeVolume struct{}

func (v *fakeVolume) GetPath() string { return "" }

func (v *fakeVolume) GetMetrics() (*volume.Metrics, error) {
	return expectedMetrics(), nil
}

func expectedMetrics() *volume.Metrics {
	vMetrics := &volume.Metrics{
		Available:  resource.NewQuantity(available, resource.BinarySI),
		Capacity:   resource.NewQuantity(capacity, resource.BinarySI),
		Used:       resource.NewQuantity(available-capacity, resource.BinarySI),
		Inodes:     resource.NewQuantity(inodesTotal, resource.BinarySI),
		InodesFree: resource.NewQuantity(inodesFree, resource.BinarySI),
		InodesUsed: resource.NewQuantity(inodesTotal-inodesFree, resource.BinarySI),
	}

	if volumeCondition != nil {
		vMetrics.Message = &volumeCondition.Message
		vMetrics.Abnormal = &volumeCondition.Abnormal
	}

	return vMetrics
}

func expectedFSStats() kubestats.FsStats {
	metric := expectedMetrics()
	available := uint64(metric.Available.Value())
	capacity := uint64(metric.Capacity.Value())
	used := uint64(metric.Used.Value())
	inodes := uint64(metric.Inodes.Value())
	inodesFree := uint64(metric.InodesFree.Value())
	inodesUsed := uint64(metric.InodesUsed.Value())
	return kubestats.FsStats{
		AvailableBytes: &available,
		CapacityBytes:  &capacity,
		UsedBytes:      &used,
		Inodes:         &inodes,
		InodesFree:     &inodesFree,
		InodesUsed:     &inodesUsed,
	}
}

func expectedVolumeHealthStats() *kubestats.VolumeHealthStats {
	metric := expectedMetrics()
	hs := &kubestats.VolumeHealthStats{}

	if metric != nil && metric.Abnormal != nil {
		hs.Abnormal = *metric.Abnormal
	}

	return hs
}

// Fake block-volume/metrics provider, block-devices have no inodes
var _ volume.BlockVolume = &fakeBlockVolume{}

type fakeBlockVolume struct{}

func (v *fakeBlockVolume) GetGlobalMapPath(*volume.Spec) (string, error) { return "", nil }

func (v *fakeBlockVolume) GetPodDeviceMapPath() (string, string) { return "", "" }

func (v *fakeBlockVolume) SupportsMetrics() bool { return true }

func (v *fakeBlockVolume) GetMetrics() (*volume.Metrics, error) {
	return expectedBlockMetrics(), nil
}

func expectedBlockMetrics() *volume.Metrics {
	vMetrics := &volume.Metrics{
		Available: resource.NewQuantity(available, resource.BinarySI),
		Capacity:  resource.NewQuantity(capacity, resource.BinarySI),
		Used:      resource.NewQuantity(available-capacity, resource.BinarySI),
	}

	if volumeCondition != nil {
		vMetrics.Abnormal = &volumeCondition.Abnormal
	}

	return vMetrics
}

func expectedBlockStats() kubestats.FsStats {
	metric := expectedBlockMetrics()
	available := uint64(metric.Available.Value())
	capacity := uint64(metric.Capacity.Value())
	used := uint64(metric.Used.Value())
	null := uint64(0)
	return kubestats.FsStats{
		AvailableBytes: &available,
		CapacityBytes:  &capacity,
		UsedBytes:      &used,
		Inodes:         &null,
		InodesFree:     &null,
		InodesUsed:     &null,
	}
}
