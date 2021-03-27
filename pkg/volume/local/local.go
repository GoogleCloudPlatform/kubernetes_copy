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

package local

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolsevents "k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/validation"
	"k8s.io/mount-utils"
	"k8s.io/utils/keymutex"
	utilstrings "k8s.io/utils/strings"
)

const (
	defaultFSType = "ext4"
)

// ProbeVolumePlugins is the primary entrypoint for volume plugins.
func ProbeVolumePlugins() []volume.VolumePlugin {
	return []volume.VolumePlugin{&localVolumePlugin{}}
}

type localVolumePlugin struct {
	host        volume.VolumeHost
	volumeLocks keymutex.KeyMutex
	recorder    toolsevents.EventRecorder
}

var _ volume.VolumePlugin = &localVolumePlugin{}
var _ volume.PersistentVolumePlugin = &localVolumePlugin{}
var _ volume.BlockVolumePlugin = &localVolumePlugin{}
var _ volume.NodeExpandableVolumePlugin = &localVolumePlugin{}

const (
	localVolumePluginName = "kubernetes.io/local-volume"
)

func (plugin *localVolumePlugin) Init(host volume.VolumeHost) error {
	plugin.host = host
	plugin.volumeLocks = keymutex.NewHashed(0)
	plugin.recorder = host.GetEventRecorder()
	return nil
}

func (plugin *localVolumePlugin) GetPluginName() string {
	return localVolumePluginName
}

func (plugin *localVolumePlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	// This volume is only supported as a PersistentVolumeSource, so the PV name is unique
	return spec.Name(), nil
}

func (plugin *localVolumePlugin) CanSupport(spec *volume.Spec) bool {
	// This volume is only supported as a PersistentVolumeSource
	return (spec.PersistentVolume != nil && spec.PersistentVolume.Spec.Local != nil)
}

func (plugin *localVolumePlugin) RequiresRemount(spec *volume.Spec) bool {
	return false
}

func (plugin *localVolumePlugin) SupportsMountOption() bool {
	return true
}

func (plugin *localVolumePlugin) SupportsSELinuxContextMount(spec *volume.Spec) (bool, error) {
	return false, nil
}

func (plugin *localVolumePlugin) GetAccessModes() []v1.PersistentVolumeAccessMode {
	// The current meaning of AccessMode is how many nodes can attach to it, not how many pods can mount it
	return []v1.PersistentVolumeAccessMode{
		v1.ReadWriteOnce,
	}
}

func getVolumeSource(spec *volume.Spec) (*v1.LocalVolumeSource, bool, error) {
	if spec.PersistentVolume != nil && spec.PersistentVolume.Spec.Local != nil {
		return spec.PersistentVolume.Spec.Local, spec.ReadOnly, nil
	}

	return nil, false, fmt.Errorf("Spec does not reference a Local volume type")
}

func (plugin *localVolumePlugin) NewMounter(spec *volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	_, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return nil, err
	}

	globalLocalPath, err := plugin.getGlobalLocalPath(spec)
	if err != nil {
		return nil, err
	}

	kvh, ok := plugin.host.(volume.KubeletVolumeHost)
	if !ok {
		return nil, fmt.Errorf("plugin volume host does not implement KubeletVolumeHost interface")
	}

	return &localVolumeMounter{
		localVolume: &localVolume{
			pod:             pod,
			podUID:          pod.UID,
			volName:         spec.Name(),
			mounter:         plugin.host.GetMounter(plugin.GetPluginName()),
			hostUtil:        kvh.GetHostUtil(),
			plugin:          plugin,
			globalPath:      globalLocalPath,
			MetricsProvider: volume.NewMetricsStatFS(plugin.host.GetPodVolumeDir(pod.UID, utilstrings.EscapeQualifiedName(localVolumePluginName), spec.Name())),
		},
		mountOptions: util.MountOptionFromSpec(spec),
		readOnly:     readOnly,
	}, nil

}

func (plugin *localVolumePlugin) NewUnmounter(volName string, podUID types.UID) (volume.Unmounter, error) {
	return &localVolumeUnmounter{
		localVolume: &localVolume{
			podUID:  podUID,
			volName: volName,
			mounter: plugin.host.GetMounter(plugin.GetPluginName()),
			plugin:  plugin,
		},
	}, nil
}

func (plugin *localVolumePlugin) NewBlockVolumeMapper(spec *volume.Spec, pod *v1.Pod) (volume.BlockVolumeMapper, error) {
	volumeSource, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return nil, err
	}

	mapper := &localVolumeMapper{
		localVolume: &localVolume{
			podUID:     pod.UID,
			volName:    spec.Name(),
			globalPath: volumeSource.Path,
			plugin:     plugin,
		},
		readOnly: readOnly,
	}

	blockPath, err := mapper.GetGlobalMapPath(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to get device path: %v", err)
	}
	mapper.MetricsProvider = volume.NewMetricsBlock(filepath.Join(blockPath, string(pod.UID)))

	return mapper, nil
}

func (plugin *localVolumePlugin) NewBlockVolumeUnmapper(volName string,
	podUID types.UID) (volume.BlockVolumeUnmapper, error) {
	return &localVolumeUnmapper{
		localVolume: &localVolume{
			podUID:  podUID,
			volName: volName,
			plugin:  plugin,
		},
	}, nil
}

// TODO: check if no path and no topology constraints are ok
func (plugin *localVolumePlugin) ConstructVolumeSpec(volumeName, mountPath string) (volume.ReconstructedVolume, error) {
	fs := v1.PersistentVolumeFilesystem
	// The main purpose of reconstructed volume is to clean unused mount points
	// and directories.
	// For filesystem volume with directory source, no global mount path is
	// needed to clean. Empty path is ok.
	// For filesystem volume with block source, we should resolve to its device
	// path if global mount path exists.
	var path string
	mounter := plugin.host.GetMounter(plugin.GetPluginName())
	refs, err := mounter.GetMountRefs(mountPath)
	if err != nil {
		return volume.ReconstructedVolume{}, err
	}
	baseMountPath := plugin.generateBlockDeviceBaseGlobalPath()
	for _, ref := range refs {
		if mount.PathWithinBase(ref, baseMountPath) {
			// If the global mount for block device exists, the source is block
			// device.
			// The resolved device path may not be the exact same as path in
			// local PV object if symbolic link is used. However, it's the true
			// source and can be used in reconstructed volume.
			path, _, err = mount.GetDeviceNameFromMount(mounter, ref)
			if err != nil {
				return volume.ReconstructedVolume{}, err
			}
			klog.V(4).Infof("local: reconstructing volume %q (pod volume mount: %q) with device %q", volumeName, mountPath, path)
			break
		}
	}
	localVolume := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				Local: &v1.LocalVolumeSource{
					Path: path,
				},
			},
			VolumeMode: &fs,
		},
	}
	return volume.ReconstructedVolume{
		Spec: volume.NewSpecFromPersistentVolume(localVolume, false),
	}, nil
}

func (plugin *localVolumePlugin) ConstructBlockVolumeSpec(podUID types.UID, volumeName,
	mapPath string) (*volume.Spec, error) {
	block := v1.PersistentVolumeBlock

	localVolume := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				Local: &v1.LocalVolumeSource{
					// Not needed because we don't need to detach local device from the host.
					Path: "",
				},
			},
			VolumeMode: &block,
		},
	}

	return volume.NewSpecFromPersistentVolume(localVolume, false), nil
}

func (plugin *localVolumePlugin) generateBlockDeviceBaseGlobalPath() string {
	return filepath.Join(plugin.host.GetPluginDir(localVolumePluginName), util.MountsInGlobalPDPath)
}

func (plugin *localVolumePlugin) getGlobalLocalPath(spec *volume.Spec) (string, error) {
	if spec.PersistentVolume.Spec.Local == nil || len(spec.PersistentVolume.Spec.Local.Path) == 0 {
		return "", fmt.Errorf("local volume source is nil or local path is not set")
	}

	kvh, ok := plugin.host.(volume.KubeletVolumeHost)
	if !ok {
		return "", fmt.Errorf("plugin volume host does not implement KubeletVolumeHost interface")
	}

	fileType, err := kvh.GetHostUtil().GetFileType(spec.PersistentVolume.Spec.Local.Path)
	if err != nil {
		return "", err
	}
	switch fileType {
	case hostutil.FileTypeDirectory:
		return spec.PersistentVolume.Spec.Local.Path, nil
	case hostutil.FileTypeBlockDev:
		return filepath.Join(plugin.generateBlockDeviceBaseGlobalPath(), spec.Name()), nil
	default:
		return "", fmt.Errorf("only directory and block device are supported")
	}
}

var _ volume.DeviceMountableVolumePlugin = &localVolumePlugin{}

type deviceMounter struct {
	plugin   *localVolumePlugin
	mounter  *mount.SafeFormatAndMount
	hostUtil hostutil.HostUtils
}

var _ volume.DeviceMounter = &deviceMounter{}

func (plugin *localVolumePlugin) CanDeviceMount(spec *volume.Spec) (bool, error) {
	return true, nil
}

func (plugin *localVolumePlugin) NewDeviceMounter() (volume.DeviceMounter, error) {
	kvh, ok := plugin.host.(volume.KubeletVolumeHost)
	if !ok {
		return nil, fmt.Errorf("plugin volume host does not implement KubeletVolumeHost interface")
	}
	return &deviceMounter{
		plugin:   plugin,
		mounter:  util.NewSafeFormatAndMountFromHost(plugin.GetPluginName(), plugin.host),
		hostUtil: kvh.GetHostUtil(),
	}, nil
}

func (dm *deviceMounter) mountLocalBlockDevice(spec *volume.Spec, devicePath string, deviceMountPath string) error {
	klog.V(4).Infof("local: mounting device %s to %s", devicePath, deviceMountPath)
	notMnt, err := dm.mounter.IsLikelyNotMountPoint(deviceMountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(deviceMountPath, 0750); err != nil {
				return err
			}
			notMnt = true
		} else {
			return err
		}
	}
	if !notMnt {
		return nil
	}
	fstype, err := getVolumeSourceFSType(spec)
	if err != nil {
		return err
	}

	ro, err := getVolumeSourceReadOnly(spec)
	if err != nil {
		return err
	}
	options := []string{}
	if ro {
		options = append(options, "ro")
	}
	mountOptions := util.MountOptionFromSpec(spec, options...)
	err = dm.mounter.FormatAndMount(devicePath, deviceMountPath, fstype, mountOptions)
	if err != nil {
		if rmErr := os.Remove(deviceMountPath); rmErr != nil {
			klog.Warningf("local: failed to remove %s: %v", deviceMountPath, rmErr)
		}
		return fmt.Errorf("local: failed to mount device %s at %s (fstype: %s), error %w", devicePath, deviceMountPath, fstype, err)
	}
	klog.V(3).Infof("local: successfully mount device %s at %s (fstype: %s)", devicePath, deviceMountPath, fstype)
	return nil
}

func (dm *deviceMounter) MountDevice(spec *volume.Spec, devicePath string, deviceMountPath string, _ volume.DeviceMounterArgs) error {
	if spec.PersistentVolume.Spec.Local == nil || len(spec.PersistentVolume.Spec.Local.Path) == 0 {
		return fmt.Errorf("local volume source is nil or local path is not set")
	}
	fileType, err := dm.hostUtil.GetFileType(spec.PersistentVolume.Spec.Local.Path)
	if err != nil {
		return err
	}

	switch fileType {
	case hostutil.FileTypeBlockDev:
		// local volume plugin does not implement AttachableVolumePlugin interface, so set devicePath to Path in PV spec directly
		return dm.mountLocalBlockDevice(spec, spec.PersistentVolume.Spec.Local.Path, deviceMountPath)
	case hostutil.FileTypeDirectory:
		// if the given local volume path is of already filesystem directory, return directly
		return nil
	default:
		return fmt.Errorf("only directory and block device are supported")
	}
}

func (plugin *localVolumePlugin) RequiresFSResize() bool {
	return true
}

func (plugin *localVolumePlugin) NodeExpand(resizeOptions volume.NodeResizeOptions) (bool, error) {
	fsVolume, err := util.CheckVolumeModeFilesystem(resizeOptions.VolumeSpec)
	if err != nil {
		return false, fmt.Errorf("error checking VolumeMode: %v", err)
	}
	if !fsVolume {
		return true, nil
	}

	localDevicePath := resizeOptions.VolumeSpec.PersistentVolume.Spec.Local.Path

	kvh, ok := plugin.host.(volume.KubeletVolumeHost)
	if !ok {
		return false, fmt.Errorf("plugin volume host does not implement KubeletVolumeHost interface")
	}

	fileType, err := kvh.GetHostUtil().GetFileType(localDevicePath)
	if err != nil {
		return false, err
	}

	switch fileType {
	case hostutil.FileTypeBlockDev:
		_, err = util.GenericResizeFS(plugin.host, plugin.GetPluginName(), localDevicePath, resizeOptions.DeviceMountPath)
		if err != nil {
			return false, err
		}
		return true, nil
	case hostutil.FileTypeDirectory:
		// if the given local volume path is of already filesystem directory, return directly because
		// we do not want to prevent mount operation from succeeding.
		klog.InfoS("Expansion of directory based local volumes is NO-OP", "localVolumePath", localDevicePath)
		return true, nil
	default:
		return false, fmt.Errorf("only directory and block device are supported")
	}
}

func getVolumeSourceFSType(spec *volume.Spec) (string, error) {
	if spec.PersistentVolume != nil &&
		spec.PersistentVolume.Spec.Local != nil {
		if spec.PersistentVolume.Spec.Local.FSType != nil {
			return *spec.PersistentVolume.Spec.Local.FSType, nil
		}
		// if the FSType is not set in local PV spec, setting it to default ("ext4")
		return defaultFSType, nil
	}

	return "", fmt.Errorf("spec does not reference a Local volume type")
}

func getVolumeSourceReadOnly(spec *volume.Spec) (bool, error) {
	if spec.PersistentVolume != nil &&
		spec.PersistentVolume.Spec.Local != nil {
		// local volumes used as a PersistentVolume gets the ReadOnly flag indirectly through
		// the persistent-claim volume used to mount the PV
		return spec.ReadOnly, nil
	}

	return false, fmt.Errorf("spec does not reference a Local volume type")
}

func (dm *deviceMounter) GetDeviceMountPath(spec *volume.Spec) (string, error) {
	return dm.plugin.getGlobalLocalPath(spec)
}

func (plugin *localVolumePlugin) NewDeviceUnmounter() (volume.DeviceUnmounter, error) {
	return &deviceMounter{
		plugin:  plugin,
		mounter: util.NewSafeFormatAndMountFromHost(plugin.GetPluginName(), plugin.host),
	}, nil
}

func (plugin *localVolumePlugin) GetDeviceMountRefs(deviceMountPath string) ([]string, error) {
	mounter := plugin.host.GetMounter(plugin.GetPluginName())
	return mounter.GetMountRefs(deviceMountPath)
}

var _ volume.DeviceUnmounter = &deviceMounter{}

func (dm *deviceMounter) UnmountDevice(deviceMountPath string) error {
	// If the local PV is a block device,
	// The deviceMountPath is generated to the format like :/var/lib/kubelet/plugins/kubernetes.io/local-volume/mounts/localpv.spec.Name;
	// If it is a filesystem directory, then the deviceMountPath is set directly to pvSpec.Local.Path
	// We only need to unmount block device here, so we need to check if the deviceMountPath passed here
	// has base mount path: /var/lib/kubelet/plugins/kubernetes.io/local-volume/mounts
	basemountPath := dm.plugin.generateBlockDeviceBaseGlobalPath()
	if mount.PathWithinBase(deviceMountPath, basemountPath) {
		return mount.CleanupMountPoint(deviceMountPath, dm.mounter, false)
	}

	return nil
}

// Local volumes represent a local directory on a node.
// The directory at the globalPath will be bind-mounted to the pod's directory
type localVolume struct {
	volName string
	pod     *v1.Pod
	podUID  types.UID
	// Global path to the volume
	globalPath string
	// Mounter interface that provides system calls to mount the global path to the pod local path.
	mounter  mount.Interface
	hostUtil hostutil.HostUtils
	plugin   *localVolumePlugin
	volume.MetricsProvider
}

func (l *localVolume) GetPath() string {
	return l.plugin.host.GetPodVolumeDir(l.podUID, utilstrings.EscapeQualifiedName(localVolumePluginName), l.volName)
}

type localVolumeMounter struct {
	*localVolume
	readOnly     bool
	mountOptions []string
}

var _ volume.Mounter = &localVolumeMounter{}

func (m *localVolumeMounter) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:       m.readOnly,
		Managed:        !m.readOnly,
		SELinuxRelabel: true,
	}
}

// SetUp bind mounts the directory to the volume path
func (m *localVolumeMounter) SetUp(mounterArgs volume.MounterArgs) error {
	return m.SetUpAt(m.GetPath(), mounterArgs)
}

// SetUpAt bind mounts the directory to the volume path and sets up volume ownership
func (m *localVolumeMounter) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	m.plugin.volumeLocks.LockKey(m.globalPath)
	defer m.plugin.volumeLocks.UnlockKey(m.globalPath)

	if m.globalPath == "" {
		return fmt.Errorf("LocalVolume volume %q path is empty", m.volName)
	}

	err := validation.ValidatePathNoBacksteps(m.globalPath)
	if err != nil {
		return fmt.Errorf("invalid path: %s %v", m.globalPath, err)
	}

	notMnt, err := mount.IsNotMountPoint(m.mounter, dir)
	klog.V(4).Infof("LocalVolume mount setup: PodDir(%s) VolDir(%s) Mounted(%t) Error(%v), ReadOnly(%t)", dir, m.globalPath, !notMnt, err, m.readOnly)
	if err != nil && !os.IsNotExist(err) {
		klog.Errorf("cannot validate mount point: %s %v", dir, err)
		return err
	}

	if !notMnt {
		return nil
	}
	refs, err := m.mounter.GetMountRefs(m.globalPath)
	if mounterArgs.FsGroup != nil {
		if err != nil {
			klog.Errorf("cannot collect mounting information: %s %v", m.globalPath, err)
			return err
		}

		// Only count mounts from other pods
		refs = m.filterPodMounts(refs)
		if len(refs) > 0 {
			fsGroupNew := int64(*mounterArgs.FsGroup)
			_, fsGroupOld, err := m.hostUtil.GetOwner(m.globalPath)
			if err != nil {
				return fmt.Errorf("failed to check fsGroup for %s (%v)", m.globalPath, err)
			}
			if fsGroupNew != fsGroupOld {
				m.plugin.recorder.Eventf(m.pod, nil, v1.EventTypeWarning, events.WarnAlreadyMountedVolume, "MountingVolume", "The requested fsGroup is %d, but the volume %s has GID %d. The volume may not be shareable.", fsGroupNew, m.volName, fsGroupOld)
			}
		}

	}

	if runtime.GOOS != "windows" {
		// skip below MkdirAll for windows since the "bind mount" logic is implemented differently in mount_wiondows.go
		if err := os.MkdirAll(dir, 0750); err != nil {
			klog.Errorf("mkdir failed on disk %s (%v)", dir, err)
			return err
		}
	}
	// Perform a bind mount to the full path to allow duplicate mounts of the same volume.
	options := []string{"bind"}
	if m.readOnly {
		options = append(options, "ro")
	}
	mountOptions := util.JoinMountOptions(options, m.mountOptions)

	klog.V(4).Infof("attempting to mount %s", dir)
	globalPath := util.MakeAbsolutePath(runtime.GOOS, m.globalPath)
	err = m.mounter.MountSensitiveWithoutSystemd(globalPath, dir, "", mountOptions, nil)
	if err != nil {
		klog.Errorf("Mount of volume %s failed: %v", dir, err)
		notMnt, mntErr := mount.IsNotMountPoint(m.mounter, dir)
		if mntErr != nil {
			klog.Errorf("IsNotMountPoint check failed: %v", mntErr)
			return err
		}
		if !notMnt {
			if mntErr = m.mounter.Unmount(dir); mntErr != nil {
				klog.Errorf("Failed to unmount: %v", mntErr)
				return err
			}
			notMnt, mntErr = mount.IsNotMountPoint(m.mounter, dir)
			if mntErr != nil {
				klog.Errorf("IsNotMountPoint check failed: %v", mntErr)
				return err
			}
			if !notMnt {
				// This is very odd, we don't expect it.  We'll try again next sync loop.
				klog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", dir)
				return err
			}
		}
		if rmErr := os.Remove(dir); rmErr != nil {
			klog.Warningf("failed to remove %s: %v", dir, rmErr)
		}
		return err
	}
	if !m.readOnly {
		// Volume owner will be written only once on the first volume mount
		if len(refs) == 0 {
			return volume.SetVolumeOwnership(m, dir, mounterArgs.FsGroup, mounterArgs.FSGroupChangePolicy, util.FSGroupCompleteHook(m.plugin, nil))
		}
	}
	return nil
}

// filterPodMounts only returns mount paths inside the kubelet pod directory
func (m *localVolumeMounter) filterPodMounts(refs []string) []string {
	filtered := []string{}
	for _, r := range refs {
		if strings.HasPrefix(r, m.plugin.host.GetPodsDir()+string(os.PathSeparator)) {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

type localVolumeUnmounter struct {
	*localVolume
}

var _ volume.Unmounter = &localVolumeUnmounter{}

// TearDown unmounts the bind mount
func (u *localVolumeUnmounter) TearDown() error {
	return u.TearDownAt(u.GetPath())
}

// TearDownAt unmounts the bind mount
func (u *localVolumeUnmounter) TearDownAt(dir string) error {
	klog.V(4).Infof("Unmounting volume %q at path %q\n", u.volName, dir)
	return mount.CleanupMountPoint(dir, u.mounter, true) /* extensiveMountPointCheck = true */
}

// localVolumeMapper implements the BlockVolumeMapper interface for local volumes.
type localVolumeMapper struct {
	*localVolume
	readOnly bool
}

var _ volume.BlockVolumeMapper = &localVolumeMapper{}
var _ volume.CustomBlockVolumeMapper = &localVolumeMapper{}

// SetUpDevice prepares the volume to the node by the plugin specific way.
func (m *localVolumeMapper) SetUpDevice() (string, error) {
	return "", nil
}

// MapPodDevice provides physical device path for the local PV.
func (m *localVolumeMapper) MapPodDevice() (string, error) {
	globalPath := util.MakeAbsolutePath(runtime.GOOS, m.globalPath)
	klog.V(4).Infof("MapPodDevice returning path %s", globalPath)
	return globalPath, nil
}

// GetStagingPath returns
func (m *localVolumeMapper) GetStagingPath() string {
	return ""
}

// SupportsMetrics returns true for SupportsMetrics as it initializes the
// MetricsProvider.
func (m *localVolumeMapper) SupportsMetrics() bool {
	return true
}

// localVolumeUnmapper implements the BlockVolumeUnmapper interface for local volumes.
type localVolumeUnmapper struct {
	*localVolume
	volume.MetricsNil
}

var _ volume.BlockVolumeUnmapper = &localVolumeUnmapper{}

// GetGlobalMapPath returns global map path and error.
// path: plugins/kubernetes.io/kubernetes.io/local-volume/volumeDevices/{volumeName}
func (l *localVolume) GetGlobalMapPath(spec *volume.Spec) (string, error) {
	return filepath.Join(l.plugin.host.GetVolumeDevicePluginDir(utilstrings.EscapeQualifiedName(localVolumePluginName)),
		l.volName), nil
}

// GetPodDeviceMapPath returns pod device map path and volume name.
// path: pods/{podUid}/volumeDevices/kubernetes.io~local-volume
// volName: local-pv-ff0d6d4
func (l *localVolume) GetPodDeviceMapPath() (string, string) {
	return l.plugin.host.GetPodVolumeDeviceDir(l.podUID,
		utilstrings.EscapeQualifiedName(localVolumePluginName)), l.volName
}
