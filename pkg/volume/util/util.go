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

package util

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/securitycontext"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	"k8s.io/mount-utils"
	"k8s.io/utils/io"
	utilstrings "k8s.io/utils/strings"
)

const (
	readyFileName = "ready"

	// ControllerManagedAttachAnnotation is the key of the annotation on Node
	// objects that indicates attach/detach operations for the node should be
	// managed by the attach/detach controller
	ControllerManagedAttachAnnotation string = "volumes.kubernetes.io/controller-managed-attach-detach"

	// MountsInGlobalPDPath is name of the directory appended to a volume plugin
	// name to create the place for volume mounts in the global PD path.
	MountsInGlobalPDPath = "mounts"

	// VolumeGidAnnotationKey is the of the annotation on the PersistentVolume
	// object that specifies a supplemental GID.
	VolumeGidAnnotationKey = "pv.beta.kubernetes.io/gid"

	// VolumeDynamicallyCreatedByKey is the key of the annotation on PersistentVolume
	// object created dynamically
	VolumeDynamicallyCreatedByKey = "kubernetes.io/createdby"

	// kubernetesPluginPathPrefix is the prefix of kubernetes plugin mount paths.
	kubernetesPluginPathPrefix = "/plugins/kubernetes.io/"
)

// IsReady checks for the existence of a regular file
// called 'ready' in the given directory and returns
// true if that file exists.
func IsReady(dir string) bool {
	readyFile := filepath.Join(dir, readyFileName)
	s, err := os.Stat(readyFile)
	if err != nil {
		return false
	}

	if !s.Mode().IsRegular() {
		klog.Errorf("ready-file is not a file: %s", readyFile)
		return false
	}

	return true
}

// SetReady creates a file called 'ready' in the given
// directory.  It logs an error if the file cannot be
// created.
func SetReady(dir string) {
	if err := os.MkdirAll(dir, 0750); err != nil && !os.IsExist(err) {
		klog.Errorf("Can't mkdir %s: %v", dir, err)
		return
	}

	readyFile := filepath.Join(dir, readyFileName)
	file, err := os.Create(readyFile)
	if err != nil {
		klog.Errorf("Can't touch %s: %v", readyFile, err)
		return
	}
	file.Close()
}

// GetSecretForPV locates secret by name and namespace, verifies the secret type, and returns secret map
func GetSecretForPV(secretNamespace, secretName, volumePluginName string, kubeClient clientset.Interface) (map[string]string, error) {
	secret := make(map[string]string)
	if kubeClient == nil {
		return secret, fmt.Errorf("cannot get kube client")
	}
	secrets, err := kubeClient.CoreV1().Secrets(secretNamespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return secret, err
	}
	if secrets.Type != v1.SecretType(volumePluginName) {
		return secret, fmt.Errorf("cannot get secret of type %s", volumePluginName)
	}
	for name, data := range secrets.Data {
		secret[name] = string(data)
	}
	return secret, nil
}

// LoadPodFromFile will read, decode, and return a Pod from a file.
func LoadPodFromFile(filePath string) (*v1.Pod, error) {
	if filePath == "" {
		return nil, fmt.Errorf("file path not specified")
	}
	podDef, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file path %s: %+v", filePath, err)
	}
	if len(podDef) == 0 {
		return nil, fmt.Errorf("file was empty: %s", filePath)
	}
	pod := &v1.Pod{}

	codec := legacyscheme.Codecs.UniversalDecoder()
	if err := apiruntime.DecodeInto(codec, podDef, pod); err != nil {
		return nil, fmt.Errorf("failed decoding file: %v", err)
	}
	return pod, nil
}

// CalculateTimeoutForVolume calculates time for a Recycler pod to complete a
// recycle operation. The calculation and return value is either the
// minimumTimeout or the timeoutIncrement per Gi of storage size, whichever is
// greater.
func CalculateTimeoutForVolume(minimumTimeout, timeoutIncrement int, pv *v1.PersistentVolume) int64 {
	giQty := resource.MustParse("1Gi")
	pvQty := pv.Spec.Capacity[v1.ResourceStorage]
	giSize := giQty.Value()
	pvSize := pvQty.Value()
	timeout := (pvSize / giSize) * int64(timeoutIncrement)
	if timeout < int64(minimumTimeout) {
		return int64(minimumTimeout)
	}
	return timeout
}

// GetPath checks if the path from the mounter is empty.
func GetPath(mounter volume.Mounter) (string, error) {
	path := mounter.GetPath()
	if path == "" {
		return "", fmt.Errorf("path is empty %s", reflect.TypeOf(mounter).String())
	}
	return path, nil
}

// UnmountViaEmptyDir delegates the tear down operation for secret, configmap, git_repo and downwardapi
// to empty_dir
func UnmountViaEmptyDir(dir string, host volume.VolumeHost, volName string, volSpec volume.Spec, podUID utypes.UID) error {
	klog.V(3).Infof("Tearing down volume %v for pod %v at %v", volName, podUID, dir)

	// Wrap EmptyDir, let it do the teardown.
	wrapped, err := host.NewWrapperUnmounter(volName, volSpec, podUID)
	if err != nil {
		return err
	}
	return wrapped.TearDownAt(dir)
}

// MountOptionFromSpec extracts and joins mount options from volume spec with supplied options
func MountOptionFromSpec(spec *volume.Spec, options ...string) []string {
	pv := spec.PersistentVolume

	if pv != nil {
		// Use beta annotation first
		if mo, ok := pv.Annotations[v1.MountOptionAnnotation]; ok {
			moList := strings.Split(mo, ",")
			return JoinMountOptions(moList, options)
		}

		if len(pv.Spec.MountOptions) > 0 {
			return JoinMountOptions(pv.Spec.MountOptions, options)
		}
	}

	return options
}

// JoinMountOptions joins mount options eliminating duplicates
func JoinMountOptions(userOptions []string, systemOptions []string) []string {
	allMountOptions := sets.New[string]()

	for _, mountOption := range userOptions {
		if len(mountOption) > 0 {
			allMountOptions.Insert(mountOption)
		}
	}

	for _, mountOption := range systemOptions {
		allMountOptions.Insert(mountOption)
	}
	return sets.List(allMountOptions)
}

// ContainsAccessMode returns whether the requested mode is contained by modes
func ContainsAccessMode(modes []v1.PersistentVolumeAccessMode, mode v1.PersistentVolumeAccessMode) bool {
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}

// ContainsAllAccessModes returns whether all of the requested modes are contained by modes
func ContainsAllAccessModes(indexedModes []v1.PersistentVolumeAccessMode, requestedModes []v1.PersistentVolumeAccessMode) bool {
	for _, mode := range requestedModes {
		if !ContainsAccessMode(indexedModes, mode) {
			return false
		}
	}
	return true
}

// GetWindowsPath get a windows path
func GetWindowsPath(path string) string {
	windowsPath := strings.Replace(path, "/", "\\", -1)
	if strings.HasPrefix(windowsPath, "\\") {
		windowsPath = "c:" + windowsPath
	}
	return windowsPath
}

// GetUniquePodName returns a unique identifier to reference a pod by
func GetUniquePodName(pod *v1.Pod) types.UniquePodName {
	return types.UniquePodName(pod.UID)
}

// GetUniqueVolumeName returns a unique name representing the volume/plugin.
// Caller should ensure that volumeName is a name/ID uniquely identifying the
// actual backing device, directory, path, etc. for a particular volume.
// The returned name can be used to uniquely reference the volume, for example,
// to prevent operations (attach/detach or mount/unmount) from being triggered
// on the same volume.
func GetUniqueVolumeName(pluginName, volumeName string) v1.UniqueVolumeName {
	return v1.UniqueVolumeName(fmt.Sprintf("%s/%s", pluginName, volumeName))
}

// GetUniqueVolumeNameFromSpecWithPod returns a unique volume name with pod
// name included. This is useful to generate different names for different pods
// on same volume.
func GetUniqueVolumeNameFromSpecWithPod(
	podName types.UniquePodName, volumePlugin volume.VolumePlugin, volumeSpec *volume.Spec) v1.UniqueVolumeName {
	return v1.UniqueVolumeName(
		fmt.Sprintf("%s/%v-%s", volumePlugin.GetPluginName(), podName, volumeSpec.Name()))
}

// GetUniqueVolumeNameFromSpec uses the given VolumePlugin to generate a unique
// name representing the volume defined in the specified volume spec.
// This returned name can be used to uniquely reference the actual backing
// device, directory, path, etc. referenced by the given volumeSpec.
// If the given plugin does not support the volume spec, this returns an error.
func GetUniqueVolumeNameFromSpec(
	volumePlugin volume.VolumePlugin,
	volumeSpec *volume.Spec) (v1.UniqueVolumeName, error) {
	if volumePlugin == nil {
		return "", fmt.Errorf(
			"volumePlugin should not be nil. volumeSpec.Name=%q",
			volumeSpec.Name())
	}

	volumeName, err := volumePlugin.GetVolumeName(volumeSpec)
	if err != nil || volumeName == "" {
		return "", fmt.Errorf(
			"failed to GetVolumeName from volumePlugin for volumeSpec %q err=%v",
			volumeSpec.Name(),
			err)
	}

	return GetUniqueVolumeName(
			volumePlugin.GetPluginName(),
			volumeName),
		nil
}

// IsPodTerminated checks if pod is terminated
func IsPodTerminated(pod *v1.Pod, podStatus v1.PodStatus) bool {
	// TODO: the guarantees provided by kubelet status are not sufficient to guarantee it's safe to ignore a deleted pod,
	// even if everything is notRunning (kubelet does not guarantee that when pod status is waiting that it isn't trying
	// to start a container).
	return podStatus.Phase == v1.PodFailed || podStatus.Phase == v1.PodSucceeded || (pod.DeletionTimestamp != nil && notRunning(podStatus.InitContainerStatuses) && notRunning(podStatus.ContainerStatuses) && notRunning(podStatus.EphemeralContainerStatuses))
}

// notRunning returns true if every status is terminated or waiting, or the status list
// is empty.
func notRunning(statuses []v1.ContainerStatus) bool {
	for _, status := range statuses {
		if status.State.Terminated == nil && status.State.Waiting == nil {
			return false
		}
	}
	return true
}

// SplitUniqueName splits the unique name to plugin name and volume name strings. It expects the uniqueName to follow
// the format plugin_name/volume_name and the plugin name must be namespaced as described by the plugin interface,
// i.e. namespace/plugin containing exactly one '/'. This means the unique name will always be in the form of
// plugin_namespace/plugin/volume_name, see k8s.io/kubernetes/pkg/volume/plugins.go VolumePlugin interface
// description and pkg/volume/util/volumehelper/volumehelper.go GetUniqueVolumeNameFromSpec that constructs
// the unique volume names.
func SplitUniqueName(uniqueName v1.UniqueVolumeName) (string, string, error) {
	components := strings.SplitN(string(uniqueName), "/", 3)
	if len(components) != 3 {
		return "", "", fmt.Errorf("cannot split volume unique name %s to plugin/volume components", uniqueName)
	}
	pluginName := fmt.Sprintf("%s/%s", components[0], components[1])
	return pluginName, components[2], nil
}

// NewSafeFormatAndMountFromHost creates a new SafeFormatAndMount with Mounter
// and Exec taken from given VolumeHost.
func NewSafeFormatAndMountFromHost(pluginName string, host volume.VolumeHost) *mount.SafeFormatAndMount {
	mounter := host.GetMounter(pluginName)
	exec := host.GetExec(pluginName)
	return &mount.SafeFormatAndMount{Interface: mounter, Exec: exec}
}

// GetVolumeMode retrieves VolumeMode from pv.
// If the volume doesn't have PersistentVolume, it's an inline volume,
// should return volumeMode as filesystem to keep existing behavior.
func GetVolumeMode(volumeSpec *volume.Spec) (v1.PersistentVolumeMode, error) {
	if volumeSpec == nil || volumeSpec.PersistentVolume == nil {
		return v1.PersistentVolumeFilesystem, nil
	}
	if volumeSpec.PersistentVolume.Spec.VolumeMode != nil {
		return *volumeSpec.PersistentVolume.Spec.VolumeMode, nil
	}
	return "", fmt.Errorf("cannot get volumeMode for volume: %v", volumeSpec.Name())
}

// GetPersistentVolumeClaimQualifiedName returns a qualified name for pvc.
func GetPersistentVolumeClaimQualifiedName(claim *v1.PersistentVolumeClaim) string {
	return utilstrings.JoinQualifiedName(claim.GetNamespace(), claim.GetName())
}

// CheckVolumeModeFilesystem checks VolumeMode.
// If the mode is Filesystem, return true otherwise return false.
func CheckVolumeModeFilesystem(volumeSpec *volume.Spec) (bool, error) {
	volumeMode, err := GetVolumeMode(volumeSpec)
	if err != nil {
		return true, err
	}
	if volumeMode == v1.PersistentVolumeBlock {
		return false, nil
	}
	return true, nil
}

// CheckPersistentVolumeClaimModeBlock checks VolumeMode.
// If the mode is Block, return true otherwise return false.
func CheckPersistentVolumeClaimModeBlock(pvc *v1.PersistentVolumeClaim) bool {
	return pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == v1.PersistentVolumeBlock
}

// IsWindowsUNCPath checks if path is prefixed with \\
// This can be used to skip any processing of paths
// that point to SMB shares, local named pipes and local UNC path
func IsWindowsUNCPath(goos, path string) bool {
	if goos != "windows" {
		return false
	}
	// Check for UNC prefix \\
	if strings.HasPrefix(path, `\\`) {
		return true
	}
	return false
}

// IsWindowsLocalPath checks if path is a local path
// prefixed with "/" or "\" like "/foo/bar" or "\foo\bar"
func IsWindowsLocalPath(goos, path string) bool {
	if goos != "windows" {
		return false
	}
	if IsWindowsUNCPath(goos, path) {
		return false
	}
	if strings.Contains(path, ":") {
		return false
	}
	if !(strings.HasPrefix(path, `/`) || strings.HasPrefix(path, `\`)) {
		return false
	}
	return true
}

// MakeAbsolutePath convert path to absolute path according to GOOS
func MakeAbsolutePath(goos, path string) string {
	if goos != "windows" {
		return filepath.Clean("/" + path)
	}
	// These are all for windows
	// If there is a colon, give up.
	if strings.Contains(path, ":") {
		return path
	}
	// If there is a slash, but no drive, add 'c:'
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		return "c:" + path
	}
	// Otherwise, add 'c:\'
	return "c:\\" + path
}

// MapBlockVolume is a utility function to provide a common way of mapping
// block device path for a specified volume and pod.  This function should be
// called by volume plugins that implements volume.BlockVolumeMapper.Map() method.
func MapBlockVolume(
	blkUtil volumepathhandler.BlockVolumePathHandler,
	devicePath,
	globalMapPath,
	podVolumeMapPath,
	volumeMapName string,
	podUID utypes.UID,
) error {
	// map devicePath to pod volume path
	mapErr := blkUtil.MapDevice(devicePath, podVolumeMapPath, volumeMapName, false /* bindMount */)
	if mapErr != nil {
		return fmt.Errorf("blkUtil.MapDevice failed. devicePath: %s, podVolumeMapPath:%s, volumeMapName: %s, bindMount: %v: %w",
			devicePath, podVolumeMapPath, volumeMapName, false, mapErr)
	}

	// map devicePath to global node path as bind mount
	mapErr = blkUtil.MapDevice(devicePath, globalMapPath, string(podUID), true /* bindMount */)
	if mapErr != nil {
		return fmt.Errorf("blkUtil.MapDevice failed. devicePath: %s, globalMapPath:%s, podUID: %s, bindMount: %v: %w",
			devicePath, globalMapPath, string(podUID), true, mapErr)
	}

	// Take file descriptor lock to keep a block device opened. Otherwise, there is a case
	// that the block device is silently removed and attached another device with the same name.
	// Container runtime can't handle this problem. To avoid unexpected condition fd lock
	// for the block device is required.
	_, mapErr = blkUtil.AttachFileDevice(filepath.Join(globalMapPath, string(podUID)))
	if mapErr != nil {
		return fmt.Errorf("blkUtil.AttachFileDevice failed. globalMapPath:%s, podUID: %s: %w",
			globalMapPath, string(podUID), mapErr)
	}

	return nil
}

// UnmapBlockVolume is a utility function to provide a common way of unmapping
// block device path for a specified volume and pod.  This function should be
// called by volume plugins that implements volume.BlockVolumeMapper.Map() method.
func UnmapBlockVolume(
	blkUtil volumepathhandler.BlockVolumePathHandler,
	globalUnmapPath,
	podDeviceUnmapPath,
	volumeMapName string,
	podUID utypes.UID,
) error {
	// Release file descriptor lock.
	err := blkUtil.DetachFileDevice(filepath.Join(globalUnmapPath, string(podUID)))
	if err != nil {
		return fmt.Errorf("blkUtil.DetachFileDevice failed. globalUnmapPath:%s, podUID: %s: %w",
			globalUnmapPath, string(podUID), err)
	}

	// unmap devicePath from global node path
	unmapDeviceErr := blkUtil.UnmapDevice(globalUnmapPath, string(podUID), true /* bindMount */)
	if unmapDeviceErr != nil {
		return fmt.Errorf("blkUtil.DetachFileDevice failed. globalUnmapPath:%s, podUID: %s, bindMount: %v: %w",
			globalUnmapPath, string(podUID), true, unmapDeviceErr)
	}

	// unmap devicePath from pod volume path
	unmapDeviceErr = blkUtil.UnmapDevice(podDeviceUnmapPath, volumeMapName, false /* bindMount */)
	if unmapDeviceErr != nil {
		return fmt.Errorf("blkUtil.DetachFileDevice failed. podDeviceUnmapPath:%s, volumeMapName: %s, bindMount: %v: %w",
			podDeviceUnmapPath, volumeMapName, false, unmapDeviceErr)
	}

	return nil
}

// IsLocalEphemeralVolume determines whether the argument is a local ephemeral
// volume vs. some other type
// Local means the volume is using storage from the local disk that is managed by kubelet.
// Ephemeral means the lifecycle of the volume is the same as the Pod.
func IsLocalEphemeralVolume(volume v1.Volume) bool {
	return volume.GitRepo != nil ||
		(volume.EmptyDir != nil && volume.EmptyDir.Medium == v1.StorageMediumDefault) ||
		volume.ConfigMap != nil
}

// GetPodVolumeNames returns names of volumes that are used in a pod,
// either as filesystem mount or raw block device.
// To save another sweep through containers, SELinux options are optionally collected too.
func GetPodVolumeNames(pod *v1.Pod, collectSELinuxOptions bool) (mounts sets.Set[string], devices sets.Set[string], seLinuxContainerContexts map[string][]*v1.SELinuxOptions) {
	mounts = sets.New[string]()
	devices = sets.New[string]()
	seLinuxContainerContexts = make(map[string][]*v1.SELinuxOptions)

	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(container *v1.Container, containerType podutil.ContainerType) bool {
		var seLinuxOptions *v1.SELinuxOptions
		if collectSELinuxOptions {
			effectiveContainerSecurity := securitycontext.DetermineEffectiveSecurityContext(pod, container)
			if effectiveContainerSecurity != nil {
				seLinuxOptions = effectiveContainerSecurity.SELinuxOptions
			}
		}

		if container.VolumeMounts != nil {
			for _, mount := range container.VolumeMounts {
				mounts.Insert(mount.Name)
				if seLinuxOptions != nil && collectSELinuxOptions {
					seLinuxContainerContexts[mount.Name] = append(seLinuxContainerContexts[mount.Name], seLinuxOptions.DeepCopy())
				}
			}
		}
		if container.VolumeDevices != nil {
			for _, device := range container.VolumeDevices {
				devices.Insert(device.Name)
			}
		}
		return true
	})
	return
}

// FsUserFrom returns FsUser of pod, which is determined by the runAsUser
// attributes.
func FsUserFrom(pod *v1.Pod) *int64 {
	var fsUser *int64
	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(container *v1.Container, containerType podutil.ContainerType) bool {
		runAsUser, ok := securitycontext.DetermineEffectiveRunAsUser(pod, container)
		// One container doesn't specify user or there are more than one
		// non-root UIDs.
		if !ok || (fsUser != nil && *fsUser != *runAsUser) {
			fsUser = nil
			return false
		}
		if fsUser == nil {
			fsUser = runAsUser
		}
		return true
	})
	return fsUser
}

// HasMountRefs checks if the given mountPath has mountRefs.
// TODO: this is a workaround for the unmount device issue caused by gci mounter.
// In GCI cluster, if gci mounter is used for mounting, the container started by mounter
// script will cause additional mounts created in the container. Since these mounts are
// irrelevant to the original mounts, they should be not considered when checking the
// mount references. The current solution is to filter out those mount paths that contain
// the k8s plugin suffix of original mount path.
func HasMountRefs(mountPath string, mountRefs []string) bool {
	// A mountPath typically is like
	//   /var/lib/kubelet/plugins/kubernetes.io/some-plugin/mounts/volume-XXXX
	// Mount refs can look like
	//   /home/somewhere/var/lib/kubelet/plugins/kubernetes.io/some-plugin/...
	// but if /var/lib/kubelet is mounted to a different device a ref might be like
	//   /mnt/some-other-place/kubelet/plugins/kubernetes.io/some-plugin/...
	// Neither of the above should be counted as a mount ref as those are handled
	// by the kubelet. What we're concerned about is a path like
	//   /data/local/some/manual/mount
	// As unmounting could interrupt usage from that mountpoint.
	//
	// So instead of looking for the entire /var/lib/... path, the plugins/kubernetes.io/
	// suffix is trimmed off and searched for.
	//
	// If there isn't a /plugins/... path, the whole mountPath is used instead.
	pathToFind := mountPath
	if i := strings.Index(mountPath, kubernetesPluginPathPrefix); i > -1 {
		pathToFind = mountPath[i:]
	}
	for _, ref := range mountRefs {
		if !strings.Contains(ref, pathToFind) {
			return true
		}
	}
	return false
}

// IsMultiAttachAllowed checks if attaching this volume to multiple nodes is definitely not allowed/possible.
// In its current form, this function can only reliably say for which volumes it's definitely forbidden. If it returns
// false, it is not guaranteed that multi-attach is actually supported by the volume type and we must rely on the
// attacher to fail fast in such cases.
// Please see https://github.com/kubernetes/kubernetes/issues/40669 and https://github.com/kubernetes/kubernetes/pull/40148#discussion_r98055047
func IsMultiAttachAllowed(volumeSpec *volume.Spec) bool {
	if volumeSpec == nil {
		// we don't know if it's supported or not and let the attacher fail later in cases it's not supported
		return true
	}

	if volumeSpec.Volume != nil {
		// Check for volume types which are known to fail slow or cause trouble when trying to multi-attach
		if volumeSpec.Volume.AzureDisk != nil ||
			volumeSpec.Volume.Cinder != nil {
			return false
		}
	}

	// Only if this volume is a persistent volume, we have reliable information on whether it's allowed or not to
	// multi-attach. We trust in the individual volume implementations to not allow unsupported access modes
	if volumeSpec.PersistentVolume != nil {
		// Check for persistent volume types which do not fail when trying to multi-attach
		if len(volumeSpec.PersistentVolume.Spec.AccessModes) == 0 {
			// No access mode specified so we don't know for sure. Let the attacher fail if needed
			return true
		}

		// check if this volume is allowed to be attached to multiple PODs/nodes, if yes, return false
		for _, accessMode := range volumeSpec.PersistentVolume.Spec.AccessModes {
			if accessMode == v1.ReadWriteMany || accessMode == v1.ReadOnlyMany {
				return true
			}
		}
		return false
	}

	// we don't know if it's supported or not and let the attacher fail later in cases it's not supported
	return true
}

// IsAttachableVolume checks if the given volumeSpec is an attachable volume or not
func IsAttachableVolume(volumeSpec *volume.Spec, volumePluginMgr *volume.VolumePluginMgr) bool {
	attachableVolumePlugin, _ := volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	if attachableVolumePlugin != nil {
		volumeAttacher, err := attachableVolumePlugin.NewAttacher()
		if err == nil && volumeAttacher != nil {
			return true
		}
	}

	return false
}

// IsDeviceMountableVolume checks if the given volumeSpec is an device mountable volume or not
func IsDeviceMountableVolume(volumeSpec *volume.Spec, volumePluginMgr *volume.VolumePluginMgr) bool {
	deviceMountableVolumePlugin, _ := volumePluginMgr.FindDeviceMountablePluginBySpec(volumeSpec)
	if deviceMountableVolumePlugin != nil {
		volumeDeviceMounter, err := deviceMountableVolumePlugin.NewDeviceMounter()
		if err == nil && volumeDeviceMounter != nil {
			return true
		}
	}

	return false
}

// GetReliableMountRefs calls mounter.GetMountRefs and retries on IsInconsistentReadError.
// To be used in volume reconstruction of volume plugins that don't have any protection
// against mounting a single volume on multiple nodes (such as attach/detach).
func GetReliableMountRefs(mounter mount.Interface, mountPath string) ([]string, error) {
	var paths []string
	var lastErr error
	err := wait.PollImmediate(10*time.Millisecond, time.Minute, func() (bool, error) {
		var err error
		paths, err = mounter.GetMountRefs(mountPath)
		if io.IsInconsistentReadError(err) {
			lastErr = err
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	})
	if err == wait.ErrWaitTimeout {
		return nil, lastErr
	}
	return paths, err
}
