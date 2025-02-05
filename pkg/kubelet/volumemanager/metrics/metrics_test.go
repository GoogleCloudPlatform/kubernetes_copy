/*
Copyright 2018 The Kubernetes Authors.

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

package metrics

import (
	"testing"

	"k8s.io/klog/v2/ktesting"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/volume"

	volumetesting "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
)

func TestMetricCollection(t *testing.T) {
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(k8stypes.NodeName("node-name"), volumePluginMgr)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}
	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	podName := util.GetUniquePodName(pod)

	// Add one volume to DesiredStateOfWorld
	generatedVolumeName, err := dsw.AddPodToVolume(podName, pod, volumeSpec, volumeSpec.Name(), "", nil /* seLinuxOptions */)
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	mounter, err := fakePlugin.NewMounter(volumeSpec, pod)
	if err != nil {
		t.Fatalf("NewMounter failed. Expected: <no error> Actual: <%v>", err)
	}

	mapper, err := fakePlugin.NewBlockVolumeMapper(volumeSpec, pod)
	if err != nil {
		t.Fatalf("NewBlockVolumeMapper failed. Expected: <no error> Actual: <%v>", err)
	}

	// Add one volume to ActualStateOfWorld
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	err = asw.MarkVolumeAsAttached(logger, "", volumeSpec, "", devicePath)
	if err != nil {
		t.Fatalf("MarkVolumeAsAttached failed. Expected: <no error> Actual: <%v>", err)
	}

	markVolumeOpts := operationexecutor.MarkVolumeOpts{
		PodName:             podName,
		PodUID:              pod.UID,
		VolumeName:          generatedVolumeName,
		Mounter:             mounter,
		BlockVolumeMapper:   mapper,
		OuterVolumeSpecName: volumeSpec.Name(),
		VolumeSpec:          volumeSpec,
		VolumeMountState:    operationexecutor.VolumeMounted,
	}
	err = asw.AddPodToVolume(markVolumeOpts)
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	metricCollector := &totalVolumesCollector{asw: asw, dsw: dsw, pluginMgr: volumePluginMgr}

	// Check if getVolumeCount returns correct data
	count := metricCollector.getVolumeCount()
	if len(count) != 2 {
		t.Errorf("getVolumeCount failed. Expected <2> states, got <%d>", len(count))
	}

	dswCount, ok := count["desired_state_of_world"]
	if !ok {
		t.Errorf("getVolumeCount failed. Expected <desired_state_of_world>, got nothing")
	}

	fakePluginCount := dswCount["fake-plugin"]
	if fakePluginCount != 1 {
		t.Errorf("getVolumeCount failed. Expected <1> fake-plugin volume in DesiredStateOfWorld, got <%d>",
			fakePluginCount)
	}

	aswCount, ok := count["actual_state_of_world"]
	if !ok {
		t.Errorf("getVolumeCount failed. Expected <actual_state_of_world>, got nothing")
	}

	fakePluginCount = aswCount["fake-plugin"]
	if fakePluginCount != 1 {
		t.Errorf("getVolumeCount failed. Expected <1> fake-plugin volume in ActualStateOfWorld, got <%d>",
			fakePluginCount)
	}

	// detach volume from the node and ensure metric collector is correctly updated
	dsw.DeletePodFromVolume(podName, generatedVolumeName)
	asw.MarkVolumeAsDetached(generatedVolumeName, k8stypes.NodeName("node-name"))
	if count := metricCollector.getVolumeCount(); len(count) != 1 {
		t.Errorf("getVolumeCount failed. Expected <1> state, got <%d>", len(count))
	}
}
