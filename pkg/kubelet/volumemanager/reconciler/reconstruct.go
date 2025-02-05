/*
Copyright 2022 The Kubernetes Authors.

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

package reconciler

import (
	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
)

// readyToUnmount returns true when reconciler can start unmounting volumes.
func (rc *reconciler) readyToUnmount() bool {
	// During kubelet startup, all volumes present on disk are added as uncertain to ASW.
	// Allow unmount only when DSW is fully populated to prevent unmounting volumes that
	// did not reach DSW yet.
	if !rc.populatorHasAddedPods() {
		return false
	}

	// Allow unmount only when ASW device paths were corrected from node.status to prevent
	// calling unmount with a wrong devicePath.
	if len(rc.volumesNeedUpdateFromNodeStatus) != 0 {
		return false
	}
	return true
}

// reconstructVolumes tries to reconstruct the actual state of world by scanning all pods' volume
// directories from the disk. For the volumes that cannot support or fail reconstruction, it will
// put the volumes to volumesFailedReconstruction to be cleaned up later when DesiredStateOfWorld
// is populated.
func (rc *reconciler) reconstructVolumes() {
	// Get volumes information by reading the pod's directory
	podVolumes, err := getVolumesFromPodDir(rc.kubeletPodsDir)
	if err != nil {
		klog.ErrorS(err, "Cannot get volumes from disk, skip sync states for volume reconstruction")
		return
	}
	reconstructedVolumes := make(map[v1.UniqueVolumeName]*globalVolumeInfo)
	reconstructedVolumeNames := []v1.UniqueVolumeName{}
	for _, volume := range podVolumes {
		if rc.actualStateOfWorld.VolumeExistsWithSpecName(volume.podName, volume.volumeSpecName) {
			klog.V(4).InfoS("Volume exists in actual state, skip cleaning up mounts", "podName", volume.podName, "volumeSpecName", volume.volumeSpecName)
			// There is nothing to reconstruct
			continue
		}
		reconstructedVolume, err := rc.reconstructVolume(volume)
		if err != nil {
			klog.InfoS("Could not construct volume information", "podName", volume.podName, "volumeSpecName", volume.volumeSpecName, "err", err)
			// We can't reconstruct the volume. Remember to check DSW after it's fully populated and force unmount the volume when it's orphaned.
			rc.volumesFailedReconstruction = append(rc.volumesFailedReconstruction, volume)
			continue
		}
		klog.V(4).InfoS("Adding reconstructed volume to actual state and node status", "podName", volume.podName, "volumeSpecName", volume.volumeSpecName)
		gvl := &globalVolumeInfo{
			volumeName:        reconstructedVolume.volumeName,
			volumeSpec:        reconstructedVolume.volumeSpec,
			devicePath:        reconstructedVolume.devicePath,
			deviceMounter:     reconstructedVolume.deviceMounter,
			blockVolumeMapper: reconstructedVolume.blockVolumeMapper,
			mounter:           reconstructedVolume.mounter,
		}
		if cachedInfo, ok := reconstructedVolumes[reconstructedVolume.volumeName]; ok {
			gvl = cachedInfo
		}
		gvl.addPodVolume(reconstructedVolume)

		reconstructedVolumeNames = append(reconstructedVolumeNames, reconstructedVolume.volumeName)
		reconstructedVolumes[reconstructedVolume.volumeName] = gvl
	}

	if len(reconstructedVolumes) > 0 {
		// Add the volumes to ASW
		rc.updateStates(reconstructedVolumes)

		// Remember to update devicePath from node.status.volumesAttached
		rc.volumesNeedUpdateFromNodeStatus = reconstructedVolumeNames
	}
	klog.V(2).InfoS("Volume reconstruction finished")
}

func (rc *reconciler) updateStates(reconstructedVolumes map[v1.UniqueVolumeName]*globalVolumeInfo) {
	for _, gvl := range reconstructedVolumes {
		err := rc.actualStateOfWorld.AddAttachUncertainReconstructedVolume(
			//TODO: the devicePath might not be correct for some volume plugins: see issue #54108
			gvl.volumeName, gvl.volumeSpec, rc.nodeName, gvl.devicePath)
		if err != nil {
			klog.ErrorS(err, "Could not add volume information to actual state of world", "volumeName", gvl.volumeName)
			continue
		}
		var seLinuxMountContext string
		for _, volume := range gvl.podVolumes {
			markVolumeOpts := operationexecutor.MarkVolumeOpts{
				PodName:             volume.podName,
				PodUID:              types.UID(volume.podName),
				VolumeName:          volume.volumeName,
				Mounter:             volume.mounter,
				BlockVolumeMapper:   volume.blockVolumeMapper,
				VolumeGIDVolume:     volume.volumeGIDValue,
				VolumeSpec:          volume.volumeSpec,
				VolumeMountState:    operationexecutor.VolumeMountUncertain,
				SELinuxMountContext: volume.seLinuxMountContext,
			}

			_, err = rc.actualStateOfWorld.CheckAndMarkVolumeAsUncertainViaReconstruction(markVolumeOpts)
			if err != nil {
				klog.ErrorS(err, "Could not add pod to volume information to actual state of world", "pod", klog.KObj(volume.pod))
				continue
			}
			seLinuxMountContext = volume.seLinuxMountContext
			klog.V(2).InfoS("Volume is marked as uncertain and added into the actual state", "pod", klog.KObj(volume.pod), "podName", volume.podName, "volumeName", volume.volumeName, "seLinuxMountContext", volume.seLinuxMountContext)
		}
		// If the volume has device to mount, we mark its device as uncertain.
		if gvl.deviceMounter != nil || gvl.blockVolumeMapper != nil {
			deviceMountPath, err := getDeviceMountPath(gvl)
			if err != nil {
				klog.ErrorS(err, "Could not find device mount path for volume", "volumeName", gvl.volumeName)
				continue
			}
			err = rc.actualStateOfWorld.MarkDeviceAsUncertain(gvl.volumeName, gvl.devicePath, deviceMountPath, seLinuxMountContext)
			if err != nil {
				klog.ErrorS(err, "Could not mark device is uncertain to actual state of world", "volumeName", gvl.volumeName, "deviceMountPath", deviceMountPath)
				continue
			}
			klog.V(2).InfoS("Volume is marked device as uncertain and added into the actual state", "volumeName", gvl.volumeName, "deviceMountPath", deviceMountPath)
		}
	}
}

// cleanOrphanVolumes tries to clean up all volumes that failed reconstruction.
func (rc *reconciler) cleanOrphanVolumes() {
	if len(rc.volumesFailedReconstruction) == 0 {
		return
	}

	for _, volume := range rc.volumesFailedReconstruction {
		if rc.desiredStateOfWorld.VolumeExistsWithSpecName(volume.podName, volume.volumeSpecName) {
			// Some pod needs the volume, don't clean it up and hope that
			// reconcile() calls SetUp and reconstructs the volume in ASW.
			klog.V(4).InfoS("Volume exists in desired state, skip cleaning up mounts", "podName", volume.podName, "volumeSpecName", volume.volumeSpecName)
			continue
		}
		klog.InfoS("Cleaning up mounts for volume that could not be reconstructed", "podName", volume.podName, "volumeSpecName", volume.volumeSpecName)
		rc.cleanupMounts(volume)
	}

	klog.V(2).InfoS("Orphan volume cleanup finished")
	// Clean the cache, cleanup is one shot operation.
	rc.volumesFailedReconstruction = make([]podVolume, 0)
}

// updateReconstructedFromNodeStatus tries to file devicePaths of reconstructed volumes from
// node.Status.VolumesAttached. This can be done only after connection to the API
// server is established, i.e. it can't be part of reconstructVolumes().
func (rc *reconciler) updateReconstructedFromNodeStatus() {
	klog.V(4).InfoS("Updating reconstructed devicePaths")

	if rc.kubeClient == nil {
		// Skip reconstructing devicePath from node objects if kubelet is in standalone mode.
		// Such kubelet is not expected to mount any attachable volume or Secrets / ConfigMap.
		klog.V(2).InfoS("Skipped reconstruction of DevicePaths from node.status in standalone mode")
		rc.volumesNeedUpdateFromNodeStatus = nil
		return
	}

	node, fetchErr := rc.kubeClient.CoreV1().Nodes().Get(context.TODO(), string(rc.nodeName), metav1.GetOptions{})
	if fetchErr != nil {
		// This may repeat few times per second until kubelet is able to read its own status for the first time.
		klog.V(4).ErrorS(fetchErr, "Failed to get Node status to reconstruct device paths")
		return
	}

	for _, volumeID := range rc.volumesNeedUpdateFromNodeStatus {
		attachable := false
		for _, attachedVolume := range node.Status.VolumesAttached {
			if volumeID != attachedVolume.Name {
				continue
			}
			rc.actualStateOfWorld.UpdateReconstructedDevicePath(volumeID, attachedVolume.DevicePath)
			attachable = true
			klog.V(4).InfoS("Updated devicePath from node status for volume", "volumeName", attachedVolume.Name, "path", attachedVolume.DevicePath)
		}
		rc.actualStateOfWorld.UpdateReconstructedVolumeAttachability(volumeID, attachable)
	}

	klog.V(2).InfoS("DevicePaths of reconstructed volumes updated")
	rc.volumesNeedUpdateFromNodeStatus = nil
}
