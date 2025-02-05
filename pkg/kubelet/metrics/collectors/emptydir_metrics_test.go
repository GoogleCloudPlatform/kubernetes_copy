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

package collectors

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics/testutil"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	statstest "k8s.io/kubernetes/pkg/kubelet/server/stats/testing"
)

func TestEmptyDirCollector(t *testing.T) {

	testNamespace := "test-namespace"
	existingPodNameWithStats := "foo"
	podNameWithoutStats := "bar"

	podStats := []statsapi.PodStats{
		{
			PodRef: statsapi.PodReference{
				Name:      existingPodNameWithStats,
				Namespace: testNamespace,
				UID:       "UID_foo",
			},
			StartTime: metav1.Now(),
			VolumeStats: []statsapi.VolumeStats{
				{
					Name: "foo-emptydir-1",
					FsStats: statsapi.FsStats{
						UsedBytes: newUint64Pointer(2101248),
					},
				},
				{
					Name: "foo-emptydir-2",
					FsStats: statsapi.FsStats{
						UsedBytes: newUint64Pointer(6488064),
					},
				},
				{
					Name: "foo-memory-emptydir",
					FsStats: statsapi.FsStats{
						UsedBytes: newUint64Pointer(25362432),
					},
				},
				{
					Name: "foo-configmap",
					FsStats: statsapi.FsStats{
						UsedBytes: newUint64Pointer(4096),
					},
				},
			},
		},
	}

	existingPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingPodNameWithStats,
			Namespace: testNamespace,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "foo-emptydir-1",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(3000100, resource.BinarySI)},
					},
				},
				{
					Name: "foo-emptydir-2",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "foo-memory-emptydir",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{Medium: v1.StorageMediumMemory},
					},
				},
				{
					Name: "foo-configmap",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
			},
		},
	}

	podWithoutStats := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podNameWithoutStats,
			Namespace: testNamespace,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "bar-emptydir",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}

	mockStatsProvider := statstest.NewMockProvider(t)

	mockStatsProvider.EXPECT().ListPodStats(context.Background()).Return(podStats, nil).Maybe()
	mockStatsProvider.EXPECT().
		GetPodByName(testNamespace, existingPodNameWithStats).
		Return(existingPod, true).
		Maybe()
	mockStatsProvider.EXPECT().
		GetPodByName(testNamespace, podNameWithoutStats).
		Return(podWithoutStats, true).
		Maybe()

	err := testutil.CustomCollectAndCompare(
		&emptyDirMetricsCollector{statsProvider: mockStatsProvider},
		strings.NewReader(`
		# HELP kubelet_pod_emptydir_volume_size_limit_bytes [ALPHA] Size limit of the emptyDir volume in bytes, if set. Only volumes on the default medium are considered.
		# TYPE kubelet_pod_emptydir_volume_size_limit_bytes gauge
		kubelet_pod_emptydir_volume_size_limit_bytes{namespace="test-namespace",pod="foo",volume_name="foo-emptydir-1"} 3.0001e+06
		# HELP kubelet_pod_emptydir_volume_used_bytes [ALPHA] Bytes used by the emptyDir volume. Only volumes on the default medium are considered.
		# TYPE kubelet_pod_emptydir_volume_used_bytes gauge
		kubelet_pod_emptydir_volume_used_bytes{namespace="test-namespace",pod="foo",volume_name="foo-emptydir-1"} 2.101248e+06
		kubelet_pod_emptydir_volume_used_bytes{namespace="test-namespace",pod="foo",volume_name="foo-emptydir-2"} 6.488064e+06
		`),
		"kubelet_pod_emptydir_volume_size_limit_bytes",
		"kubelet_pod_emptydir_volume_used_bytes",
	)
	if err != nil {
		t.Fatal(err)
	}

}
