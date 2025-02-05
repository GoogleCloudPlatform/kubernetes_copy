/*
Copyright 2016 The Kubernetes Authors.

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

package kubelet

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	cadvisorapiv1 "github.com/google/cadvisor/info/v1"
	cadvisorv2 "github.com/google/cadvisor/info/v2"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilpath "k8s.io/utils/path"
	utilstrings "k8s.io/utils/strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	utilnode "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/volume/csi"
)

// getRootDir returns the full path to the directory under which kubelet can
// store data.  These functions are useful to pass interfaces to other modules
// that may need to know where to write data without getting a whole kubelet
// instance.
func (kl *Kubelet) getRootDir() string {
	return kl.rootDirectory
}

// getPodLogsDir returns the full path to the directory that kubelet can use
// to store pod's log files. This defaults to /var/log/pods if not specified
// otherwise in the config file.
func (kl *Kubelet) getPodLogsDir() string {
	return kl.podLogsDirectory
}

// getPodsDir returns the full path to the directory under which pod
// directories are created.
func (kl *Kubelet) getPodsDir() string {
	return filepath.Join(kl.getRootDir(), config.DefaultKubeletPodsDirName)
}

// getPluginsDir returns the full path to the directory under which plugin
// directories are created.  Plugins can use these directories for data that
// they need to persist.  Plugins should create subdirectories under this named
// after their own names.
func (kl *Kubelet) getPluginsDir() string {
	return filepath.Join(kl.getRootDir(), config.DefaultKubeletPluginsDirName)
}

// getPluginsRegistrationDir returns the full path to the directory under which
// plugins socket should be placed to be registered.
// More information is available about plugin registration in the pluginwatcher
// module
func (kl *Kubelet) getPluginsRegistrationDir() string {
	return filepath.Join(kl.getRootDir(), config.DefaultKubeletPluginsRegistrationDirName)
}

// getPluginDir returns a data directory name for a given plugin name.
// Plugins can use these directories to store data that they need to persist.
// For per-pod plugin data, see getPodPluginDir.
func (kl *Kubelet) getPluginDir(pluginName string) string {
	return filepath.Join(kl.getPluginsDir(), pluginName)
}

// getCheckpointsDir returns a data directory name for checkpoints.
// Checkpoints can be stored in this directory for further use.
func (kl *Kubelet) getCheckpointsDir() string {
	return filepath.Join(kl.getRootDir(), config.DefaultKubeletCheckpointsDirName)
}

// getPodStatusDir returns a data directory name for the pod status manager.
// Pod status manager state can be stored in the directory for further use.
func (kl *Kubelet) getPodStatusDir() string {
	return filepath.Join(kl.getRootDir(), config.DefaultKubeletPodStatusDirName)
}

// getVolumeDevicePluginsDir returns the full path to the directory under which plugin
// directories are created.  Plugins can use these directories for data that
// they need to persist.  Plugins should create subdirectories under this named
// after their own names.
func (kl *Kubelet) getVolumeDevicePluginsDir() string {
	return filepath.Join(kl.getRootDir(), config.DefaultKubeletPluginsDirName)
}

// getVolumeDevicePluginDir returns a data directory name for a given plugin name.
// Plugins can use these directories to store data that they need to persist.
// For per-pod plugin data, see getVolumeDevicePluginsDir.
func (kl *Kubelet) getVolumeDevicePluginDir(pluginName string) string {
	return filepath.Join(kl.getVolumeDevicePluginsDir(), pluginName, config.DefaultKubeletVolumeDevicesDirName)
}

// GetPodDir returns the full path to the per-pod data directory for the
// specified pod. This directory may not exist if the pod does not exist.
func (kl *Kubelet) GetPodDir(podUID types.UID) string {
	return kl.getPodDir(podUID)
}

// ListPodsFromDisk gets a list of pods that have data directories.
func (kl *Kubelet) ListPodsFromDisk() ([]types.UID, error) {
	return kl.listPodsFromDisk()
}

// HandlerSupportsUserNamespaces checks whether the specified handler supports
// user namespaces.
func (kl *Kubelet) HandlerSupportsUserNamespaces(rtHandler string) (bool, error) {
	rtHandlers := kl.runtimeState.runtimeHandlers()
	if rtHandlers == nil {
		return false, fmt.Errorf("runtime handlers are not set")
	}
	for _, h := range rtHandlers {
		if h.Name == rtHandler {
			return h.SupportsUserNamespaces, nil
		}
	}
	return false, fmt.Errorf("the handler %q is not known", rtHandler)
}

// GetKubeletMappings gets the additional IDs allocated for the Kubelet.
func (kl *Kubelet) GetKubeletMappings() (uint32, uint32, error) {
	return kl.getKubeletMappings()
}

func (kl *Kubelet) GetMaxPods() int {
	return kl.maxPods
}

// getPodDir returns the full path to the per-pod directory for the pod with
// the given UID.
func (kl *Kubelet) getPodDir(podUID types.UID) string {
	return filepath.Join(kl.getPodsDir(), string(podUID))
}

// getPodVolumesSubpathsDir returns the full path to the per-pod subpaths directory under
// which subpath volumes are created for the specified pod.  This directory may not
// exist if the pod does not exist or subpaths are not specified.
func (kl *Kubelet) getPodVolumeSubpathsDir(podUID types.UID) string {
	return filepath.Join(kl.getPodDir(podUID), config.DefaultKubeletVolumeSubpathsDirName)
}

// getPodVolumesDir returns the full path to the per-pod data directory under
// which volumes are created for the specified pod.  This directory may not
// exist if the pod does not exist.
func (kl *Kubelet) getPodVolumesDir(podUID types.UID) string {
	return filepath.Join(kl.getPodDir(podUID), config.DefaultKubeletVolumesDirName)
}

// getPodVolumeDir returns the full path to the directory which represents the
// named volume under the named plugin for specified pod.  This directory may not
// exist if the pod does not exist.
func (kl *Kubelet) getPodVolumeDir(podUID types.UID, pluginName string, volumeName string) string {
	return filepath.Join(kl.getPodVolumesDir(podUID), pluginName, volumeName)
}

// getPodVolumeDevicesDir returns the full path to the per-pod data directory under
// which volumes are created for the specified pod. This directory may not
// exist if the pod does not exist.
func (kl *Kubelet) getPodVolumeDevicesDir(podUID types.UID) string {
	return filepath.Join(kl.getPodDir(podUID), config.DefaultKubeletVolumeDevicesDirName)
}

// getPodVolumeDeviceDir returns the full path to the directory which represents the
// named plugin for specified pod. This directory may not exist if the pod does not exist.
func (kl *Kubelet) getPodVolumeDeviceDir(podUID types.UID, pluginName string) string {
	return filepath.Join(kl.getPodVolumeDevicesDir(podUID), pluginName)
}

// getPodPluginsDir returns the full path to the per-pod data directory under
// which plugins may store data for the specified pod.  This directory may not
// exist if the pod does not exist.
func (kl *Kubelet) getPodPluginsDir(podUID types.UID) string {
	return filepath.Join(kl.getPodDir(podUID), config.DefaultKubeletPluginsDirName)
}

// getPodPluginDir returns a data directory name for a given plugin name for a
// given pod UID.  Plugins can use these directories to store data that they
// need to persist.  For non-per-pod plugin data, see getPluginDir.
func (kl *Kubelet) getPodPluginDir(podUID types.UID, pluginName string) string {
	return filepath.Join(kl.getPodPluginsDir(podUID), pluginName)
}

// getPodContainerDir returns the full path to the per-pod data directory under
// which container data is held for the specified pod.  This directory may not
// exist if the pod or container does not exist.
func (kl *Kubelet) getPodContainerDir(podUID types.UID, ctrName string) string {
	return filepath.Join(kl.getPodDir(podUID), config.DefaultKubeletContainersDirName, ctrName)
}

// getPodResourcesSocket returns the full path to the directory containing the pod resources socket
func (kl *Kubelet) getPodResourcesDir() string {
	return filepath.Join(kl.getRootDir(), config.DefaultKubeletPodResourcesDirName)
}

// GetPods returns all pods bound to the kubelet and their spec, and the mirror
// pods.
func (kl *Kubelet) GetPods() []*v1.Pod {
	pods := kl.podManager.GetPods()
	for i, p := range pods {
		// Pod cache does not get updated status for static pods.
		// TODO(tallclair): Most callers of GetPods() do not need pod status. We should either parameterize this,
		// or move the status injection to only the callers that do need it (maybe just the /pods http handler?).
		if kubelettypes.IsStaticPod(p) {
			if status, ok := kl.statusManager.GetPodStatus(p.UID); ok {
				// do not mutate the cache
				p = p.DeepCopy()
				p.Status = status
				pods[i] = p
			}
		}
	}
	return pods
}

// GetRunningPods returns all pods running on kubelet from looking at the
// container runtime cache. This function converts kubecontainer.Pod to
// v1.Pod, so only the fields that exist in both kubecontainer.Pod and
// v1.Pod are considered meaningful.
func (kl *Kubelet) GetRunningPods(ctx context.Context) ([]*v1.Pod, error) {
	pods, err := kl.runtimeCache.GetPods(ctx)
	if err != nil {
		return nil, err
	}

	apiPods := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		apiPods = append(apiPods, pod.ToAPIPod())
	}
	return apiPods, nil
}

// GetPodByFullName gets the pod with the given 'full' name, which
// incorporates the namespace as well as whether the pod was found.
func (kl *Kubelet) GetPodByFullName(podFullName string) (*v1.Pod, bool) {
	return kl.podManager.GetPodByFullName(podFullName)
}

// GetPodByName provides the first pod that matches namespace and name, as well
// as whether the pod was found.
func (kl *Kubelet) GetPodByName(namespace, name string) (*v1.Pod, bool) {
	return kl.podManager.GetPodByName(namespace, name)
}

// GetPodByCgroupfs provides the pod that maps to the specified cgroup, as well
// as whether the pod was found.
func (kl *Kubelet) GetPodByCgroupfs(cgroupfs string) (*v1.Pod, bool) {
	pcm := kl.containerManager.NewPodContainerManager()
	if result, podUID := pcm.IsPodCgroup(cgroupfs); result {
		return kl.podManager.GetPodByUID(podUID)
	}
	return nil, false
}

// GetHostname Returns the hostname as the kubelet sees it.
func (kl *Kubelet) GetHostname() string {
	return kl.hostname
}

// getRuntime returns the current Runtime implementation in use by the kubelet.
func (kl *Kubelet) getRuntime() kubecontainer.Runtime {
	return kl.containerRuntime
}

// GetNode returns the node info for the configured node name of this Kubelet.
func (kl *Kubelet) GetNode() (*v1.Node, error) {
	if kl.kubeClient == nil {
		return kl.initialNode(context.TODO())
	}
	return kl.nodeLister.Get(string(kl.nodeName))
}

// getNodeAnyWay() must return a *v1.Node which is required by RunGeneralPredicates().
// The *v1.Node is obtained as follows:
// Return kubelet's nodeInfo for this node, except on error or if in standalone mode,
// in which case return a manufactured nodeInfo representing a node with no pods,
// zero capacity, and the default labels.
func (kl *Kubelet) getNodeAnyWay() (*v1.Node, error) {
	if kl.kubeClient != nil {
		if n, err := kl.nodeLister.Get(string(kl.nodeName)); err == nil {
			return n, nil
		}
	}
	return kl.initialNode(context.TODO())
}

// GetNodeConfig returns the container manager node config.
func (kl *Kubelet) GetNodeConfig() cm.NodeConfig {
	return kl.containerManager.GetNodeConfig()
}

// GetPodCgroupRoot returns the listeral cgroupfs value for the cgroup containing all pods
func (kl *Kubelet) GetPodCgroupRoot() string {
	return kl.containerManager.GetPodCgroupRoot()
}

// GetHostIPs returns host IPs or nil in case of error.
func (kl *Kubelet) GetHostIPs() ([]net.IP, error) {
	node, err := kl.GetNode()
	if err != nil {
		return nil, fmt.Errorf("cannot get node: %v", err)
	}
	return utilnode.GetNodeHostIPs(node)
}

// getHostIPsAnyWay attempts to return the host IPs from kubelet's nodeInfo, or
// the initialNode.
func (kl *Kubelet) getHostIPsAnyWay() ([]net.IP, error) {
	node, err := kl.getNodeAnyWay()
	if err != nil {
		return nil, err
	}
	return utilnode.GetNodeHostIPs(node)
}

// GetExtraSupplementalGroupsForPod returns a list of the extra
// supplemental groups for the Pod. These extra supplemental groups come
// from annotations on persistent volumes that the pod depends on.
func (kl *Kubelet) GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64 {
	return kl.volumeManager.GetExtraSupplementalGroupsForPod(pod)
}

// getPodVolumePathListFromDisk returns a list of the volume paths by reading the
// volume directories for the given pod from the disk.
func (kl *Kubelet) getPodVolumePathListFromDisk(podUID types.UID) ([]string, error) {
	volumes := []string{}
	podVolDir := kl.getPodVolumesDir(podUID)

	if pathExists, pathErr := mount.PathExists(podVolDir); pathErr != nil {
		return volumes, fmt.Errorf("error checking if path %q exists: %v", podVolDir, pathErr)
	} else if !pathExists {
		klog.V(6).InfoS("Path does not exist", "path", podVolDir)
		return volumes, nil
	}

	volumePluginDirs, err := os.ReadDir(podVolDir)
	if err != nil {
		klog.ErrorS(err, "Could not read directory", "path", podVolDir)
		return volumes, err
	}
	for _, volumePluginDir := range volumePluginDirs {
		volumePluginName := volumePluginDir.Name()
		volumePluginPath := filepath.Join(podVolDir, volumePluginName)
		volumeDirs, err := utilpath.ReadDirNoStat(volumePluginPath)
		if err != nil {
			return volumes, fmt.Errorf("could not read directory %s: %v", volumePluginPath, err)
		}
		unescapePluginName := utilstrings.UnescapeQualifiedName(volumePluginName)

		if unescapePluginName != csi.CSIPluginName {
			for _, volumeDir := range volumeDirs {
				volumes = append(volumes, filepath.Join(volumePluginPath, volumeDir))
			}
		} else {
			// For CSI volumes, the mounted volume path has an extra sub path "/mount", so also add it
			// to the list if the mounted path exists.
			for _, volumeDir := range volumeDirs {
				path := filepath.Join(volumePluginPath, volumeDir)
				csimountpath := csi.GetCSIMounterPath(path)
				if pathExists, _ := mount.PathExists(csimountpath); pathExists {
					volumes = append(volumes, csimountpath)
				}
			}
		}
	}
	return volumes, nil
}

func (kl *Kubelet) getMountedVolumePathListFromDisk(podUID types.UID) ([]string, error) {
	mountedVolumes := []string{}
	volumePaths, err := kl.getPodVolumePathListFromDisk(podUID)
	if err != nil {
		return mountedVolumes, err
	}
	// Only use IsLikelyNotMountPoint to check might not cover all cases. For CSI volumes that
	// either: 1) don't mount or 2) bind mount in the rootfs, the mount check will not work as expected.
	// We plan to remove this mountpoint check as a condition before deleting pods since it is
	// not reliable and the condition might be different for different types of volumes. But it requires
	// a reliable way to clean up unused volume dir to avoid problems during pod deletion. See discussion in issue #74650
	for _, volumePath := range volumePaths {
		isNotMount, err := kl.mounter.IsLikelyNotMountPoint(volumePath)
		if err != nil {
			return mountedVolumes, fmt.Errorf("fail to check mount point %q: %v", volumePath, err)
		}
		if !isNotMount {
			mountedVolumes = append(mountedVolumes, volumePath)
		}
	}
	return mountedVolumes, nil
}

// getPodVolumeSubpathListFromDisk returns a list of the volume-subpath paths by reading the
// subpath directories for the given pod from the disk.
func (kl *Kubelet) getPodVolumeSubpathListFromDisk(podUID types.UID) ([]string, error) {
	volumes := []string{}
	podSubpathsDir := kl.getPodVolumeSubpathsDir(podUID)

	if pathExists, pathErr := mount.PathExists(podSubpathsDir); pathErr != nil {
		return nil, fmt.Errorf("error checking if path %q exists: %v", podSubpathsDir, pathErr)
	} else if !pathExists {
		return volumes, nil
	}

	// Explicitly walks /<volume>/<container name>/<subPathIndex>
	volumePluginDirs, err := os.ReadDir(podSubpathsDir)
	if err != nil {
		klog.ErrorS(err, "Could not read directory", "path", podSubpathsDir)
		return volumes, err
	}
	for _, volumePluginDir := range volumePluginDirs {
		volumePluginName := volumePluginDir.Name()
		volumePluginPath := filepath.Join(podSubpathsDir, volumePluginName)
		containerDirs, err := os.ReadDir(volumePluginPath)
		if err != nil {
			return volumes, fmt.Errorf("could not read directory %s: %v", volumePluginPath, err)
		}
		for _, containerDir := range containerDirs {
			containerName := containerDir.Name()
			containerPath := filepath.Join(volumePluginPath, containerName)
			// Switch to ReadDirNoStat at the subPathIndex level to prevent issues with stat'ing
			// mount points that may not be responsive
			subPaths, err := utilpath.ReadDirNoStat(containerPath)
			if err != nil {
				return volumes, fmt.Errorf("could not read directory %s: %v", containerPath, err)
			}
			for _, subPathDir := range subPaths {
				volumes = append(volumes, filepath.Join(containerPath, subPathDir))
			}
		}
	}
	return volumes, nil
}

// GetRequestedContainersInfo returns container info.
func (kl *Kubelet) GetRequestedContainersInfo(containerName string, options cadvisorv2.RequestOptions) (map[string]*cadvisorapiv1.ContainerInfo, error) {
	return kl.cadvisor.GetRequestedContainersInfo(containerName, options)
}

// GetVersionInfo returns information about the version of cAdvisor in use.
func (kl *Kubelet) GetVersionInfo() (*cadvisorapiv1.VersionInfo, error) {
	return kl.cadvisor.VersionInfo()
}

// GetCachedMachineInfo assumes that the machine info can't change without a reboot
func (kl *Kubelet) GetCachedMachineInfo() (*cadvisorapiv1.MachineInfo, error) {
	kl.machineInfoLock.RLock()
	defer kl.machineInfoLock.RUnlock()
	return kl.machineInfo, nil
}

func (kl *Kubelet) setCachedMachineInfo(info *cadvisorapiv1.MachineInfo) {
	kl.machineInfoLock.Lock()
	defer kl.machineInfoLock.Unlock()
	kl.machineInfo = info
}
