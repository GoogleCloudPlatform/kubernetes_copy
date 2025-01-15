/*
Copyright 2014 The Kubernetes Authors.

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

package emptydir

import (
	"fmt"
	"k8s.io/kubernetes/pkg/kubelet/util/swap"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilstrings "k8s.io/utils/strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	usernamespacefeature "k8s.io/kubernetes/pkg/kubelet/userns"
	filesystem "k8s.io/kubernetes/pkg/util/filesystem"
	"k8s.io/kubernetes/pkg/volume"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/fsquota"
)

// TODO: in the near future, this will be changed to be more restrictive
// and the group will be set to allow containers to use emptyDir volumes
// from the group attribute.
//
// https://issue.k8s.io/2630
const perm os.FileMode = 0777

// ProbeVolumePlugins is the primary entrypoint for volume plugins.
func ProbeVolumePlugins() []volume.VolumePlugin {
	return []volume.VolumePlugin{
		&emptyDirPlugin{nil},
	}
}

type emptyDirPlugin struct {
	host volume.VolumeHost
}

var _ volume.VolumePlugin = &emptyDirPlugin{}

const (
	emptyDirPluginName           = "kubernetes.io/empty-dir"
	hugePagesPageSizeMountOption = "pagesize"
)

func getPath(uid types.UID, volName string, host volume.VolumeHost) string {
	return host.GetPodVolumeDir(uid, utilstrings.EscapeQualifiedName(emptyDirPluginName), volName)
}

func (plugin *emptyDirPlugin) Init(host volume.VolumeHost) error {
	plugin.host = host

	return nil
}

func (plugin *emptyDirPlugin) GetPluginName() string {
	return emptyDirPluginName
}

func (plugin *emptyDirPlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	volumeSource, _ := getVolumeSource(spec)
	if volumeSource == nil {
		return "", fmt.Errorf("Spec does not reference an EmptyDir volume type")
	}

	// Return user defined volume name, since this is an ephemeral volume type
	return spec.Name(), nil
}

func (plugin *emptyDirPlugin) CanSupport(spec *volume.Spec) bool {
	return spec.Volume != nil && spec.Volume.EmptyDir != nil
}

func (plugin *emptyDirPlugin) RequiresRemount(spec *volume.Spec) bool {
	return false
}

func (plugin *emptyDirPlugin) SupportsMountOption() bool {
	return false
}

func (plugin *emptyDirPlugin) SupportsSELinuxContextMount(spec *volume.Spec) (bool, error) {
	return false, nil
}

func (plugin *emptyDirPlugin) NewMounter(spec *volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	return plugin.newMounterInternal(spec, pod, plugin.host.GetMounter(plugin.GetPluginName()), &realMountDetector{plugin.host.GetMounter(plugin.GetPluginName())})
}

func calculateEmptyDirMemorySize(nodeAllocatableMemory *resource.Quantity, spec *volume.Spec, pod *v1.Pod) *resource.Quantity {
	// if feature is disabled, continue the default behavior of linux host default
	sizeLimit := &resource.Quantity{}
	if !utilfeature.DefaultFeatureGate.Enabled(features.SizeMemoryBackedVolumes) {
		return sizeLimit
	}

	// size limit defaults to node allocatable (pods can't consume more memory than all pods)
	sizeLimit = nodeAllocatableMemory
	zero := resource.MustParse("0")

	// determine pod resource allocation
	// we use the same function for pod cgroup assignment to maintain consistent behavior
	// NOTE: this could be nil on systems that do not support pod memory containment (i.e. windows)
	podResourceConfig := cm.ResourceConfigForPod(pod, false, uint64(100000), false)
	if podResourceConfig != nil && podResourceConfig.Memory != nil {
		podMemoryLimit := resource.NewQuantity(*(podResourceConfig.Memory), resource.BinarySI)
		// ensure 0 < value < size
		if podMemoryLimit.Cmp(zero) > 0 && podMemoryLimit.Cmp(*sizeLimit) < 1 {
			sizeLimit = podMemoryLimit
		}
	}

	// volume local size is  used if and only if less than what pod could consume
	if spec.Volume.EmptyDir.SizeLimit != nil {
		volumeSizeLimit := spec.Volume.EmptyDir.SizeLimit
		// ensure 0 < value < size
		if volumeSizeLimit.Cmp(zero) > 0 && volumeSizeLimit.Cmp(*sizeLimit) < 1 {
			sizeLimit = volumeSizeLimit
		}
	}
	return sizeLimit
}

func (plugin *emptyDirPlugin) newMounterInternal(spec *volume.Spec, pod *v1.Pod, mounter mount.Interface, mountDetector mountDetector) (volume.Mounter, error) {
	medium := v1.StorageMediumDefault
	sizeLimit := &resource.Quantity{}
	if spec.Volume.EmptyDir != nil { // Support a non-specified source as EmptyDir.
		medium = spec.Volume.EmptyDir.Medium
		if medium == v1.StorageMediumMemory {
			nodeAllocatable, err := plugin.host.GetNodeAllocatable()
			if err != nil {
				return nil, err
			}
			sizeLimit = calculateEmptyDirMemorySize(nodeAllocatable.Memory(), spec, pod)
		}
	}
	return &emptyDir{
		pod:             pod,
		volName:         spec.Name(),
		medium:          medium,
		sizeLimit:       sizeLimit,
		mounter:         mounter,
		mountDetector:   mountDetector,
		plugin:          plugin,
		MetricsProvider: volume.NewMetricsDu(getPath(pod.UID, spec.Name(), plugin.host)),
	}, nil
}

func (plugin *emptyDirPlugin) NewUnmounter(volName string, podUID types.UID) (volume.Unmounter, error) {
	// Inject real implementations here, test through the internal function.
	return plugin.newUnmounterInternal(volName, podUID, plugin.host.GetMounter(plugin.GetPluginName()), &realMountDetector{plugin.host.GetMounter(plugin.GetPluginName())})
}

func (plugin *emptyDirPlugin) newUnmounterInternal(volName string, podUID types.UID, mounter mount.Interface, mountDetector mountDetector) (volume.Unmounter, error) {
	ed := &emptyDir{
		pod:             &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID}},
		volName:         volName,
		medium:          v1.StorageMediumDefault, // might be changed later
		mounter:         mounter,
		mountDetector:   mountDetector,
		plugin:          plugin,
		MetricsProvider: volume.NewMetricsDu(getPath(podUID, volName, plugin.host)),
	}
	return ed, nil
}

func (plugin *emptyDirPlugin) ConstructVolumeSpec(volName, mountPath string) (volume.ReconstructedVolume, error) {
	emptyDirVolume := &v1.Volume{
		Name: volName,
		VolumeSource: v1.VolumeSource{
			EmptyDir: &v1.EmptyDirVolumeSource{},
		},
	}
	return volume.ReconstructedVolume{
		Spec: volume.NewSpecFromVolume(emptyDirVolume),
	}, nil
}

// mountDetector abstracts how to find what kind of mount a path is backed by.
type mountDetector interface {
	// GetMountMedium determines what type of medium a given path is backed
	// by and whether that path is a mount point.  For example, if this
	// returns (v1.StorageMediumMemory, false, nil), the caller knows that the path is
	// on a memory FS (tmpfs on Linux) but is not the root mountpoint of
	// that tmpfs.
	GetMountMedium(path string, requestedMedium v1.StorageMedium) (v1.StorageMedium, bool, *resource.Quantity, error)
}

// EmptyDir volumes are temporary directories exposed to the pod.
// These do not persist beyond the lifetime of a pod.
type emptyDir struct {
	pod           *v1.Pod
	volName       string
	sizeLimit     *resource.Quantity
	medium        v1.StorageMedium
	mounter       mount.Interface
	mountDetector mountDetector
	plugin        *emptyDirPlugin
	volume.MetricsProvider
}

func (ed *emptyDir) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:       false,
		Managed:        true,
		SELinuxRelabel: true,
	}
}

// SetUp creates new directory.
func (ed *emptyDir) SetUp(mounterArgs volume.MounterArgs) error {
	return ed.SetUpAt(ed.GetPath(), mounterArgs)
}

// SetUpAt creates new directory.
func (ed *emptyDir) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	notMnt, err := ed.mounter.IsLikelyNotMountPoint(dir)
	// Getting an os.IsNotExist err from is a contingency; the directory
	// may not exist yet, in which case, setup should run.
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// If the plugin readiness file is present for this volume, and the
	// storage medium is the default, then the volume is ready.  If the
	// medium is memory, and a mountpoint is present, then the volume is
	// ready.
	readyDir := ed.getMetaDir()
	if volumeutil.IsReady(readyDir) {
		if ed.medium == v1.StorageMediumMemory && !notMnt {
			return nil
		} else if ed.medium == v1.StorageMediumDefault {
			// Further check dir exists
			if _, err := os.Stat(dir); err == nil {
				klog.V(6).InfoS("Dir exists, so check and assign quota if the underlying medium supports quotas", "dir", dir)
				err = ed.assignQuota(dir, mounterArgs.DesiredSize)
				return err
			}
			// This situation should not happen unless user manually delete volume dir.
			// In this case, delete ready file and print a warning for it.
			klog.Warningf("volume ready file dir %s exist, but volume dir %s does not. Remove ready dir", readyDir, dir)
			if err := os.RemoveAll(readyDir); err != nil && !os.IsNotExist(err) {
				klog.Warningf("failed to remove ready dir [%s]: %v", readyDir, err)
			}
		}
	}

	switch {
	case ed.medium == v1.StorageMediumDefault:
		err = ed.setupDir(dir)
	case ed.medium == v1.StorageMediumMemory:
		err = ed.setupTmpfs(dir)
	case v1helper.IsHugePageMedium(ed.medium):
		err = ed.setupHugepages(dir)
	default:
		err = fmt.Errorf("unknown storage medium %q", ed.medium)
	}

	volume.SetVolumeOwnership(ed, dir, mounterArgs.FsGroup, nil /*fsGroupChangePolicy*/, volumeutil.FSGroupCompleteHook(ed.plugin, nil))

	// If setting up the quota fails, just log a message but don't actually error out.
	// We'll use the old du mechanism in this case, at least until we support
	// enforcement.
	if err == nil {
		volumeutil.SetReady(ed.getMetaDir())
		err = ed.assignQuota(dir, mounterArgs.DesiredSize)
	}
	return err
}

// assignQuota checks if the underlying medium supports quotas and if so, sets
func (ed *emptyDir) assignQuota(dir string, mounterSize *resource.Quantity) error {
	if mounterSize != nil {
		// Deliberately shadow the outer use of err as noted
		// above.
		// hostUsers field is interpreted as true by default, so a pod is by default
		// not confined by a user namespace.
		userNamespacesEnabled := false
		if usernamespacefeature.EnabledUserNamespacesSupport() {
			userNamespacesEnabled = ed.pod.Spec.HostUsers != nil && !*ed.pod.Spec.HostUsers
		}
		hasQuotas, err := fsquota.SupportsQuotas(ed.mounter, dir, userNamespacesEnabled)
		klog.V(3).Infof("assignQuota called, hasQuotas = %t userNamespacesEnabled = %t", hasQuotas, userNamespacesEnabled)
		if err != nil {
			klog.V(3).Infof("Unable to check for quota support on %s: %s", dir, err.Error())
		} else if hasQuotas {
			klog.V(3).Infof("emptydir trying to assign quota %v on %s", mounterSize, dir)
			if err := fsquota.AssignQuota(ed.mounter, dir, ed.pod.UID, mounterSize, userNamespacesEnabled); err != nil {
				klog.V(3).Infof("Set quota on %s failed %s", dir, err.Error())
				return err
			}
			return nil
		}
	}
	return nil
}

// setupTmpfs creates a tmpfs mount at the specified directory.
func (ed *emptyDir) setupTmpfs(dir string) error {
	if ed.mounter == nil {
		return fmt.Errorf("memory storage requested, but mounter is nil")
	}
	if err := ed.setupDir(dir); err != nil {
		return err
	}
	// Make SetUp idempotent.
	medium, isMnt, _, err := ed.mountDetector.GetMountMedium(dir, ed.medium)
	if err != nil {
		return err
	}
	// If the directory is a mountpoint with medium memory, there is no
	// work to do since we are already in the desired state.
	if isMnt && medium == v1.StorageMediumMemory {
		return nil
	}

	options := ed.generateTmpfsMountOptions(swap.IsTmpfsNoswapOptionSupported(ed.mounter, ed.plugin.host.GetPluginDir(emptyDirPluginName)))

	klog.V(3).Infof("pod %v: mounting tmpfs for volume %v", ed.pod.UID, ed.volName)
	return ed.mounter.MountSensitiveWithoutSystemd("tmpfs", dir, "tmpfs", options, nil)
}

// setupHugepages creates a hugepage mount at the specified directory.
func (ed *emptyDir) setupHugepages(dir string) error {
	if ed.mounter == nil {
		return fmt.Errorf("memory storage requested, but mounter is nil")
	}
	if err := ed.setupDir(dir); err != nil {
		return err
	}
	// Make SetUp idempotent.
	medium, isMnt, mountPageSize, err := ed.mountDetector.GetMountMedium(dir, ed.medium)
	klog.V(3).Infof("pod %v: setupHugepages: medium: %s, isMnt: %v, dir: %s, err: %v", ed.pod.UID, medium, isMnt, dir, err)
	if err != nil {
		return err
	}
	// If the directory is a mountpoint with medium hugepages of the same page size,
	// there is no work to do since we are already in the desired state.
	if isMnt && v1helper.IsHugePageMedium(medium) {
		// Medium is: Hugepages
		if ed.medium == v1.StorageMediumHugePages {
			return nil
		}
		if mountPageSize == nil {
			return fmt.Errorf("pod %v: mounted dir %s pagesize is not determined", ed.pod.UID, dir)
		}
		// Medium is: Hugepages-<size>
		// Mounted page size and medium size must be equal
		mediumSize, err := v1helper.HugePageSizeFromMedium(ed.medium)
		if err != nil {
			return err
		}
		if mountPageSize == nil || mediumSize.Cmp(*mountPageSize) != 0 {
			return fmt.Errorf("pod %v: mounted dir %s pagesize '%s' and requested medium size '%s' differ", ed.pod.UID, dir, mountPageSize.String(), mediumSize.String())
		}
		return nil
	}

	pageSizeMountOption, err := getPageSizeMountOption(ed.medium, ed.pod)
	if err != nil {
		return err
	}

	klog.V(3).Infof("pod %v: mounting hugepages for volume %v", ed.pod.UID, ed.volName)
	return ed.mounter.MountSensitiveWithoutSystemd("nodev", dir, "hugetlbfs", []string{pageSizeMountOption}, nil)
}

// getPageSizeMountOption retrieves pageSize mount option from Pod's resources
// and medium and validates pageSize options in all containers of given Pod.
func getPageSizeMountOption(medium v1.StorageMedium, pod *v1.Pod) (string, error) {
	pageSizeFound := false
	pageSize := resource.Quantity{}

	var mediumPageSize resource.Quantity
	if medium != v1.StorageMediumHugePages {
		// medium is: Hugepages-<size>
		var err error
		mediumPageSize, err = v1helper.HugePageSizeFromMedium(medium)
		if err != nil {
			return "", err
		}
	}

	// In some rare cases init containers can also consume Huge pages
	for _, container := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		// We can take request because limit and requests must match.
		for requestName := range container.Resources.Requests {
			if !v1helper.IsHugePageResourceName(requestName) {
				continue
			}
			currentPageSize, err := v1helper.HugePageSizeFromResourceName(requestName)
			if err != nil {
				return "", err
			}
			if medium == v1.StorageMediumHugePages { // medium is: Hugepages, size is not specified
				// PageSize for all volumes in a POD must be equal if medium is "Hugepages"
				if pageSizeFound && pageSize.Cmp(currentPageSize) != 0 {
					return "", fmt.Errorf("medium: %s can't be used if container requests multiple huge page sizes", medium)
				}

				pageSizeFound = true
				pageSize = currentPageSize
			} else { // medium is: Hugepages-<size>
				if currentPageSize.Cmp(mediumPageSize) == 0 {
					pageSizeFound = true
					pageSize = currentPageSize
				}
			}
		}
	}

	if !pageSizeFound {
		return "", fmt.Errorf("medium %s: hugePages storage requested, but there is no resource request for huge pages", medium)
	}

	return fmt.Sprintf("%s=%s", hugePagesPageSizeMountOption, pageSize.String()), nil

}

// setupDir creates the directory with the default permissions specified by the perm constant.
func (ed *emptyDir) setupDir(dir string) error {
	// Create the directory if it doesn't already exist.
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}

	// stat the directory to read permission bits
	fileinfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}

	if fileinfo.Mode().Perm() != perm.Perm() {
		// If the permissions on the created directory are wrong, the
		// kubelet is probably running with a umask set.  In order to
		// avoid clearing the umask for the entire process or locking
		// the thread, clearing the umask, creating the dir, restoring
		// the umask, and unlocking the thread, we do a chmod to set
		// the specific bits we need.
		err := filesystem.Chmod(dir, perm)
		if err != nil {
			return err
		}

		fileinfo, err = os.Lstat(dir)
		if err != nil {
			return err
		}

		if fileinfo.Mode().Perm() != perm.Perm() {
			klog.Errorf("Expected directory %q permissions to be: %s; got: %s", dir, perm.Perm(), fileinfo.Mode().Perm())
		}
	}

	return nil
}

func (ed *emptyDir) GetPath() string {
	return getPath(ed.pod.UID, ed.volName, ed.plugin.host)
}

// TearDown simply discards everything in the directory.
func (ed *emptyDir) TearDown() error {
	return ed.TearDownAt(ed.GetPath())
}

// TearDownAt simply discards everything in the directory.
func (ed *emptyDir) TearDownAt(dir string) error {
	// First remove ready dir which created in SetUp func
	readyDir := ed.getMetaDir()
	if removeErr := os.RemoveAll(readyDir); removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("failed to remove ready dir [%s]: %v", readyDir, removeErr)
	}

	if pathExists, pathErr := mount.PathExists(dir); pathErr != nil {
		return fmt.Errorf("error checking if path exists: %w", pathErr)
	} else if !pathExists {
		klog.Warningf("Warning: Unmount skipped because path does not exist: %v", dir)
		return nil
	}

	// Figure out the medium.
	medium, isMnt, _, err := ed.mountDetector.GetMountMedium(dir, ed.medium)
	if err != nil {
		return err
	}
	if isMnt {
		if medium == v1.StorageMediumMemory {
			ed.medium = v1.StorageMediumMemory
			return ed.teardownTmpfsOrHugetlbfs(dir)
		} else if medium == v1.StorageMediumHugePages {
			ed.medium = v1.StorageMediumHugePages
			return ed.teardownTmpfsOrHugetlbfs(dir)
		}
	}
	// assume StorageMediumDefault
	return ed.teardownDefault(dir)
}

func (ed *emptyDir) teardownDefault(dir string) error {
	if utilfeature.DefaultFeatureGate.Enabled(features.LocalStorageCapacityIsolationFSQuotaMonitoring) {
		// Remove any quota
		userNamespacesEnabled := false
		if usernamespacefeature.EnabledUserNamespacesSupport() {
			userNamespacesEnabled = ed.pod.Spec.HostUsers != nil && !*ed.pod.Spec.HostUsers
		}
		err := fsquota.ClearQuota(ed.mounter, dir, userNamespacesEnabled)
		if err != nil {
			klog.Warningf("Warning: Failed to clear quota on %s: %v", dir, err)
		}
	}
	// Renaming the directory is not required anymore because the operation executor
	// now handles duplicate operations on the same volume

	// NOTE(claudiub): Apparently, if we don't have write permissions, we can't delete files
	// with only read permissions set.
	err := filesystem.Chmod(dir, 0777)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (ed *emptyDir) teardownTmpfsOrHugetlbfs(dir string) error {
	if ed.mounter == nil {
		return fmt.Errorf("memory storage requested, but mounter is nil")
	}
	if err := ed.mounter.Unmount(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return nil
}

func (ed *emptyDir) getMetaDir() string {
	return filepath.Join(ed.plugin.host.GetPodPluginDir(ed.pod.UID, utilstrings.EscapeQualifiedName(emptyDirPluginName)), ed.volName)
}

func getVolumeSource(spec *volume.Spec) (*v1.EmptyDirVolumeSource, bool) {
	var readOnly bool
	var volumeSource *v1.EmptyDirVolumeSource

	if spec.Volume != nil && spec.Volume.EmptyDir != nil {
		volumeSource = spec.Volume.EmptyDir
		readOnly = spec.ReadOnly
	}

	return volumeSource, readOnly
}

func (ed *emptyDir) generateTmpfsMountOptions(noswapSupported bool) (options []string) {
	// Linux system default is 50% of capacity.
	if ed.sizeLimit != nil && ed.sizeLimit.Value() > 0 {
		options = append(options, fmt.Sprintf("size=%d", ed.sizeLimit.Value()))
	}

	if noswapSupported {
		options = append(options, swap.TmpfsNoswapOption)
	}

	return options
}
