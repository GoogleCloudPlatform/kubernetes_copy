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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/metrics"
	volumepkg "k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
	utilpath "k8s.io/utils/path"
	utilstrings "k8s.io/utils/strings"
)

// these interfaces are necessary to keep the structures private
// and at the same time log them correctly in structured logs.
var _ logr.Marshaler = podVolume{}
var _ logr.Marshaler = reconstructedVolume{}
var _ logr.Marshaler = globalVolumeInfo{}

type podVolume struct {
	podName        volumetypes.UniquePodName
	volumeSpecName string
	volumePath     string
	pluginName     string
	volumeMode     v1.PersistentVolumeMode
}

func (p podVolume) MarshalLog() interface{} {
	return struct {
		PodName        string `json:"podName"`
		VolumeSpecName string `json:"volumeSpecName"`
		VolumePath     string `json:"volumePath"`
		PluginName     string `json:"pluginName"`
		VolumeMode     string `json:"volumeMode"`
	}{
		PodName:        string(p.podName),
		VolumeSpecName: p.volumeSpecName,
		VolumePath:     p.volumePath,
		PluginName:     p.pluginName,
		VolumeMode:     string(p.volumeMode),
	}
}

type reconstructedVolume struct {
	volumeName          v1.UniqueVolumeName
	podName             volumetypes.UniquePodName
	volumeSpec          *volumepkg.Spec
	pod                 *v1.Pod
	volumeGIDValue      string
	devicePath          string
	mounter             volumepkg.Mounter
	deviceMounter       volumepkg.DeviceMounter
	blockVolumeMapper   volumepkg.BlockVolumeMapper
	seLinuxMountContext string
}

func (rv reconstructedVolume) MarshalLog() interface{} {
	return struct {
		VolumeName          string `json:"volumeName"`
		PodName             string `json:"podName"`
		VolumeSpecName      string `json:"volumeSpecName"`
		PodUID              string `json:"podUID"`
		VolumeGIDValue      string `json:"volumeGIDValue"`
		DevicePath          string `json:"devicePath"`
		SeLinuxMountContext string `json:"seLinuxMountContext"`
	}{
		VolumeName:          string(rv.volumeName),
		PodName:             string(rv.podName),
		VolumeSpecName:      rv.volumeSpec.Name(),
		PodUID:              string(rv.pod.UID),
		VolumeGIDValue:      rv.volumeGIDValue,
		DevicePath:          rv.devicePath,
		SeLinuxMountContext: rv.seLinuxMountContext,
	}
}

// globalVolumeInfo stores reconstructed volume information
// for each pod that was using that volume.
type globalVolumeInfo struct {
	volumeName        v1.UniqueVolumeName
	volumeSpec        *volumepkg.Spec
	devicePath        string
	mounter           volumepkg.Mounter
	deviceMounter     volumepkg.DeviceMounter
	blockVolumeMapper volumepkg.BlockVolumeMapper
	podVolumes        map[volumetypes.UniquePodName]*reconstructedVolume
}

func (gvi globalVolumeInfo) MarshalLog() interface{} {
	podVolumes := make(map[volumetypes.UniquePodName]v1.UniqueVolumeName)
	for podName, volume := range gvi.podVolumes {
		podVolumes[podName] = volume.volumeName
	}

	return struct {
		VolumeName     string                                            `json:"volumeName"`
		VolumeSpecName string                                            `json:"volumeSpecName"`
		DevicePath     string                                            `json:"devicePath"`
		PodVolumes     map[volumetypes.UniquePodName]v1.UniqueVolumeName `json:"podVolumes"`
	}{
		VolumeName:     string(gvi.volumeName),
		VolumeSpecName: gvi.volumeSpec.Name(),
		DevicePath:     gvi.devicePath,
		PodVolumes:     podVolumes,
	}
}

func (rc *reconciler) updateLastSyncTime() {
	rc.timeOfLastSyncLock.Lock()
	defer rc.timeOfLastSyncLock.Unlock()
	rc.timeOfLastSync = time.Now()
}

func (rc *reconciler) StatesHasBeenSynced() bool {
	rc.timeOfLastSyncLock.Lock()
	defer rc.timeOfLastSyncLock.Unlock()
	return !rc.timeOfLastSync.IsZero()
}

func (gvi *globalVolumeInfo) addPodVolume(rcv *reconstructedVolume) {
	if gvi.podVolumes == nil {
		gvi.podVolumes = map[volumetypes.UniquePodName]*reconstructedVolume{}
	}
	gvi.podVolumes[rcv.podName] = rcv
}

func (rc *reconciler) cleanupMounts(volume podVolume) {
	klog.V(2).InfoS("Reconciler sync states: could not find volume information in desired state, clean up the mount points", "podName", volume.podName, "volumeSpecName", volume.volumeSpecName)
	mountedVolume := operationexecutor.MountedVolume{
		PodName: volume.podName,
		// VolumeName should be generated by `GetUniqueVolumeNameFromSpec` or `GetUniqueVolumeNameFromSpecWithPod`.
		// However, since we don't have the volume information in asw when cleanup mounts, it doesn't matter what we put here.
		VolumeName:          v1.UniqueVolumeName(volume.volumeSpecName),
		InnerVolumeSpecName: volume.volumeSpecName,
		PluginName:          volume.pluginName,
		PodUID:              types.UID(volume.podName),
	}
	metrics.ForceCleanedFailedVolumeOperationsTotal.Inc()
	// TODO: Currently cleanupMounts only includes UnmountVolume operation. In the next PR, we will add
	// to unmount both volume and device in the same routine.
	err := rc.operationExecutor.UnmountVolume(mountedVolume, rc.actualStateOfWorld, rc.kubeletPodsDir)
	if err != nil {
		metrics.ForceCleanedFailedVolumeOperationsErrorsTotal.Inc()
		klog.ErrorS(err, mountedVolume.GenerateErrorDetailed("volumeHandler.UnmountVolumeHandler for UnmountVolume failed", err).Error())
		return
	}
}

// getDeviceMountPath returns device mount path for block volume which
// implements BlockVolumeMapper or filesystem volume which implements
// DeviceMounter
func getDeviceMountPath(gvi *globalVolumeInfo) (string, error) {
	if gvi.blockVolumeMapper != nil {
		// for block gvi, we return its global map path
		return gvi.blockVolumeMapper.GetGlobalMapPath(gvi.volumeSpec)
	} else if gvi.deviceMounter != nil {
		// for filesystem gvi, we return its device mount path if the plugin implements DeviceMounter
		return gvi.deviceMounter.GetDeviceMountPath(gvi.volumeSpec)
	} else {
		return "", fmt.Errorf("blockVolumeMapper or deviceMounter required")
	}
}

// getVolumesFromPodDir scans through the volumes directories under the given pod directory.
// It returns a list of pod volume information including pod's uid, volume's plugin name, mount path,
// and volume spec name.
func getVolumesFromPodDir(podDir string) ([]podVolume, error) {
	podsDirInfo, err := os.ReadDir(podDir)
	if err != nil {
		return nil, err
	}
	volumes := []podVolume{}
	for i := range podsDirInfo {
		if !podsDirInfo[i].IsDir() {
			continue
		}
		podName := podsDirInfo[i].Name()
		podDir := filepath.Join(podDir, podName)

		// Find filesystem volume information
		// ex. filesystem volume: /pods/{podUid}/volumes/{escapeQualifiedPluginName}/{volumeName}
		volumesDirs := map[v1.PersistentVolumeMode]string{
			v1.PersistentVolumeFilesystem: filepath.Join(podDir, config.DefaultKubeletVolumesDirName),
		}
		// Find block volume information
		// ex. block volume: /pods/{podUid}/volumeDevices/{escapeQualifiedPluginName}/{volumeName}
		volumesDirs[v1.PersistentVolumeBlock] = filepath.Join(podDir, config.DefaultKubeletVolumeDevicesDirName)

		for volumeMode, volumesDir := range volumesDirs {
			var volumesDirInfo []fs.DirEntry
			if volumesDirInfo, err = os.ReadDir(volumesDir); err != nil {
				// Just skip the loop because given volumesDir doesn't exist depending on volumeMode
				continue
			}
			for _, volumeDir := range volumesDirInfo {
				pluginName := volumeDir.Name()
				volumePluginPath := filepath.Join(volumesDir, pluginName)
				volumePluginDirs, err := utilpath.ReadDirNoStat(volumePluginPath)
				if err != nil {
					klog.ErrorS(err, "Could not read volume plugin directory", "volumePluginPath", volumePluginPath)
					continue
				}
				unescapePluginName := utilstrings.UnescapeQualifiedName(pluginName)
				for _, volumeName := range volumePluginDirs {
					volumePath := filepath.Join(volumePluginPath, volumeName)
					klog.V(5).InfoS("Volume path from volume plugin directory", "podName", podName, "volumePath", volumePath)
					volumes = append(volumes, podVolume{
						podName:        volumetypes.UniquePodName(podName),
						volumeSpecName: volumeName,
						volumePath:     volumePath,
						pluginName:     unescapePluginName,
						volumeMode:     volumeMode,
					})
				}
			}
		}
	}
	for _, volume := range volumes {
		klog.V(4).InfoS("Get volume from pod directory", "path", podDir, "volume", volume)
	}
	return volumes, nil
}

// Reconstruct volume data structure by reading the pod's volume directories
func (rc *reconciler) reconstructVolume(volume podVolume) (rvolume *reconstructedVolume, rerr error) {
	metrics.ReconstructVolumeOperationsTotal.Inc()
	defer func() {
		if rerr != nil {
			metrics.ReconstructVolumeOperationsErrorsTotal.Inc()
		}
	}()

	// plugin initializations
	plugin, err := rc.volumePluginMgr.FindPluginByName(volume.pluginName)
	if err != nil {
		return nil, err
	}

	// Create pod object
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID(volume.podName),
		},
	}
	mapperPlugin, err := rc.volumePluginMgr.FindMapperPluginByName(volume.pluginName)
	if err != nil {
		return nil, err
	}
	if volume.volumeMode == v1.PersistentVolumeBlock && mapperPlugin == nil {
		return nil, fmt.Errorf("could not find block volume plugin %q (spec.Name: %q) pod %q (UID: %q)", volume.pluginName, volume.volumeSpecName, volume.podName, pod.UID)
	}

	reconstructed, err := rc.operationExecutor.ReconstructVolumeOperation(
		volume.volumeMode,
		plugin,
		mapperPlugin,
		pod.UID,
		volume.podName,
		volume.volumeSpecName,
		volume.volumePath,
		volume.pluginName)
	if err != nil {
		return nil, err
	}
	volumeSpec := reconstructed.Spec
	if volumeSpec == nil {
		return nil, fmt.Errorf("failed to reconstruct volume for plugin %q (spec.Name: %q) pod %q (UID: %q): got nil", volume.pluginName, volume.volumeSpecName, volume.podName, pod.UID)
	}

	// We have to find the plugins by volume spec (NOT by plugin name) here
	// in order to correctly reconstruct ephemeral volume types.
	// Searching by spec checks whether the volume is actually attachable
	// (i.e. has a PV) whereas searching by plugin name can only tell whether
	// the plugin supports attachable volumes.
	deviceMountablePlugin, err := rc.volumePluginMgr.FindDeviceMountablePluginBySpec(volumeSpec)
	if err != nil {
		return nil, err
	}

	// The unique volume name used depends on whether the volume is attachable/device-mountable
	// (needsNameFromSpec = true) or not.
	needsNameFromSpec := deviceMountablePlugin != nil
	if !needsNameFromSpec {
		// Check attach-ability of a volume only as a fallback to avoid calling
		// FindAttachablePluginBySpec for CSI volumes - it needs a connection to the API server,
		// but it may not be available at this stage of kubelet startup.
		// All CSI volumes are device-mountable, so they won't reach this code.
		attachablePlugin, err := rc.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
		if err != nil {
			return nil, err
		}
		needsNameFromSpec = attachablePlugin != nil
	}

	var uniqueVolumeName v1.UniqueVolumeName
	if needsNameFromSpec {
		uniqueVolumeName, err = util.GetUniqueVolumeNameFromSpec(plugin, volumeSpec)
		if err != nil {
			return nil, err
		}
	} else {
		uniqueVolumeName = util.GetUniqueVolumeNameFromSpecWithPod(volume.podName, plugin, volumeSpec)
	}

	var volumeMapper volumepkg.BlockVolumeMapper
	var volumeMounter volumepkg.Mounter
	var deviceMounter volumepkg.DeviceMounter

	if volume.volumeMode == v1.PersistentVolumeBlock {
		var newMapperErr error
		volumeMapper, newMapperErr = mapperPlugin.NewBlockVolumeMapper(
			volumeSpec,
			pod)
		if newMapperErr != nil {
			return nil, fmt.Errorf(
				"reconstructVolume.NewBlockVolumeMapper failed for volume %q (spec.Name: %q) pod %q (UID: %q) with: %v",
				uniqueVolumeName,
				volumeSpec.Name(),
				volume.podName,
				pod.UID,
				newMapperErr)
		}
	} else {
		var err error
		volumeMounter, err = plugin.NewMounter(volumeSpec, pod)
		if err != nil {
			return nil, fmt.Errorf(
				"reconstructVolume.NewMounter failed for volume %q (spec.Name: %q) pod %q (UID: %q) with: %v",
				uniqueVolumeName,
				volumeSpec.Name(),
				volume.podName,
				pod.UID,
				err)
		}
		if deviceMountablePlugin != nil {
			deviceMounter, err = deviceMountablePlugin.NewDeviceMounter()
			if err != nil {
				return nil, fmt.Errorf("reconstructVolume.NewDeviceMounter failed for volume %q (spec.Name: %q) pod %q (UID: %q) with: %v",
					uniqueVolumeName,
					volumeSpec.Name(),
					volume.podName,
					pod.UID,
					err)
			}
		}
	}

	reconstructedVolume := &reconstructedVolume{
		volumeName:     uniqueVolumeName,
		podName:        volume.podName,
		volumeSpec:     volumeSpec,
		pod:            pod,
		deviceMounter:  deviceMounter,
		volumeGIDValue: "",
		// devicePath is updated during updateStates() by checking node status's VolumesAttached data.
		// TODO: get device path directly from the volume mount path.
		devicePath:          "",
		mounter:             volumeMounter,
		blockVolumeMapper:   volumeMapper,
		seLinuxMountContext: reconstructed.SELinuxMountContext,
	}
	return reconstructedVolume, nil
}
