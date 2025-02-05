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

/*
Package cache implements data structures used by the kubelet volume manager to
keep track of attached volumes and the pods that mounted them.
*/
package cache

import (
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

// ActualStateOfWorld defines a set of thread-safe operations for the kubelet
// volume manager's actual state of the world cache.
// This cache contains volumes->pods i.e. a set of all volumes attached to this
// node and the pods that the manager believes have successfully mounted the
// volume.
// Note: This is distinct from the ActualStateOfWorld implemented by the
// attach/detach controller. They both keep track of different objects. This
// contains kubelet volume manager specific state.
type ActualStateOfWorld interface {
	// ActualStateOfWorld must implement the methods required to allow
	// operationexecutor to interact with it.
	operationexecutor.ActualStateOfWorldMounterUpdater

	// ActualStateOfWorld must implement the methods required to allow
	// operationexecutor to interact with it.
	operationexecutor.ActualStateOfWorldAttacherUpdater

	// AddPodToVolume adds the given pod to the given volume in the cache
	// indicating the specified volume has been successfully mounted to the
	// specified pod.
	// If a pod with the same unique name already exists under the specified
	// volume, reset the pod's remountRequired value.
	// If a volume with the name volumeName does not exist in the list of
	// attached volumes, an error is returned.
	AddPodToVolume(operationexecutor.MarkVolumeOpts) error

	// MarkRemountRequired marks each volume that is successfully attached and
	// mounted for the specified pod as requiring remount (if the plugin for the
	// volume indicates it requires remounting on pod updates). Atomically
	// updating volumes depend on this to update the contents of the volume on
	// pod update.
	MarkRemountRequired(podName volumetypes.UniquePodName)

	// SetDeviceMountState sets device mount state for the given volume. When deviceMountState is set to DeviceGloballyMounted
	// then device is mounted at a global mount point. When it is set to DeviceMountUncertain then also it means volume
	// MAY be globally mounted at a global mount point. In both cases - the volume must be unmounted from
	// global mount point prior to detach.
	// If a volume with the name volumeName does not exist in the list of
	// attached volumes, an error is returned.
	SetDeviceMountState(volumeName v1.UniqueVolumeName, deviceMountState operationexecutor.DeviceMountState, devicePath, deviceMountPath, seLinuxMountContext string) error

	// DeletePodFromVolume removes the given pod from the given volume in the
	// cache indicating the volume has been successfully unmounted from the pod.
	// If a pod with the same unique name does not exist under the specified
	// volume, this is a no-op.
	// If a volume with the name volumeName does not exist in the list of
	// attached volumes, an error is returned.
	DeletePodFromVolume(podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName) error

	// DeleteVolume removes the given volume from the list of attached volumes
	// in the cache indicating the volume has been successfully detached from
	// this node.
	// If a volume with the name volumeName does not exist in the list of
	// attached volumes, this is a no-op.
	// If a volume with the name volumeName exists and its list of mountedPods
	// is not empty, an error is returned.
	DeleteVolume(volumeName v1.UniqueVolumeName) error

	// PodExistsInVolume returns true if the given pod exists in the list of
	// mountedPods for the given volume in the cache, indicating that the volume
	// is attached to this node and the pod has successfully mounted it.
	// If a pod with the same unique name does not exist under the specified
	// volume, false is returned.
	// If a volume with the name volumeName does not exist in the list of
	// attached volumes, a volumeNotAttachedError is returned indicating the
	// given volume is not yet attached.
	// If the given volumeName/podName combo exists but the value of
	// remountRequired is true, a remountRequiredError is returned indicating
	// the given volume has been successfully mounted to this pod but should be
	// remounted to reflect changes in the referencing pod. Atomically updating
	// volumes, depend on this to update the contents of the volume.
	// All volume mounting calls should be idempotent so a second mount call for
	// volumes that do not need to update contents should not fail.
	PodExistsInVolume(podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName, desiredVolumeSize resource.Quantity, seLinuxLabel string) (bool, string, error)

	// PodRemovedFromVolume returns true if the given pod does not exist in the list of
	// mountedPods for the given volume in the cache, indicating that the pod has
	// fully unmounted it or it was never mounted the volume.
	// If the volume is fully mounted or is in uncertain mount state for the pod, it is
	// considered that the pod still exists in volume manager's actual state of the world
	// and false is returned.
	PodRemovedFromVolume(podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName) bool

	// PodHasMountedVolumes returns true if any volume is mounted on the given pod
	PodHasMountedVolumes(podName volumetypes.UniquePodName) bool

	// VolumeExistsWithSpecName returns true if the given volume specified with the
	// volume spec name (a.k.a., InnerVolumeSpecName) exists in the list of
	// volumes that should be attached to this node.
	// If a pod with the same name does not exist under the specified
	// volume, false is returned.
	VolumeExistsWithSpecName(podName volumetypes.UniquePodName, volumeSpecName string) bool

	// VolumeExists returns true if the given volume exists in the list of
	// attached volumes in the cache, indicating the volume is attached to this
	// node.
	VolumeExists(volumeName v1.UniqueVolumeName) bool

	// GetMountedVolumes generates and returns a list of volumes and the pods
	// they are successfully attached and mounted for based on the current
	// actual state of the world.
	GetMountedVolumes() []MountedVolume

	// GetAllMountedVolumes returns list of all possibly mounted volumes including
	// those that are in VolumeMounted state and VolumeMountUncertain state.
	GetAllMountedVolumes() []MountedVolume

	// GetMountedVolumesForPod generates and returns a list of volumes that are
	// successfully attached and mounted for the specified pod based on the
	// current actual state of the world.
	GetMountedVolumesForPod(podName volumetypes.UniquePodName) []MountedVolume

	// GetMountedVolumeForPod returns the volume and true if
	// the given name is mounted on the given pod.
	GetMountedVolumeForPod(podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName) (MountedVolume, bool)

	// GetPossiblyMountedVolumesForPod generates and returns a list of volumes for
	// the specified pod that either are attached and mounted or are "uncertain",
	// i.e. a volume plugin may be mounting the volume right now.
	GetPossiblyMountedVolumesForPod(podName volumetypes.UniquePodName) []MountedVolume

	// GetGloballyMountedVolumes generates and returns a list of all attached
	// volumes that are globally mounted. This list can be used to determine
	// which volumes should be reported as "in use" in the node's VolumesInUse
	// status field. Globally mounted here refers to the shared plugin mount
	// point for the attachable volume from which the pod specific mount points
	// are created (via bind mount).
	GetGloballyMountedVolumes() []AttachedVolume

	// GetUnmountedVolumes generates and returns a list of attached volumes that
	// have no mountedPods. This list can be used to determine which volumes are
	// no longer referenced and may be globally unmounted and detached.
	GetUnmountedVolumes() []AttachedVolume

	// GetAttachedVolumes returns a list of volumes that is known to be attached
	// to the node. This list can be used to determine volumes that are either in-use
	// or have a mount/unmount operation pending.
	GetAttachedVolumes() []AttachedVolume

	// GetAttachedVolume returns the volume that is known to be attached to the node
	// with the given volume name. If the volume is not found, the second return value
	// is false.
	GetAttachedVolume(volumeName v1.UniqueVolumeName) (AttachedVolume, bool)

	// Add the specified volume to ASW as uncertainly attached.
	AddAttachUncertainReconstructedVolume(volumeName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string) error

	// UpdateReconstructedDevicePath updates devicePath of a reconstructed volume
	// from Node.Status.VolumesAttached. The ASW is updated only when the volume is still
	// uncertain. If the volume got mounted in the meantime, its devicePath must have
	// been fixed by such an update.
	UpdateReconstructedDevicePath(volumeName v1.UniqueVolumeName, devicePath string)

	// UpdateReconstructedVolumeAttachability updates volume attachability from the API server.
	UpdateReconstructedVolumeAttachability(volumeName v1.UniqueVolumeName, volumeAttachable bool)
}

// MountedVolume represents a volume that has successfully been mounted to a pod.
type MountedVolume struct {
	operationexecutor.MountedVolume
}

// AttachedVolume represents a volume that is attached to a node.
type AttachedVolume struct {
	operationexecutor.AttachedVolume

	// DeviceMountState indicates if device has been globally mounted or is not.
	DeviceMountState operationexecutor.DeviceMountState

	// SELinuxMountContext is the context with that the volume is globally mounted
	// (via -o context=XYZ mount option). If empty, the volume is not mounted with
	// "-o context=".
	SELinuxMountContext string
}

// DeviceMayBeMounted returns true if device is mounted in global path or is in
// uncertain state.
func (av AttachedVolume) DeviceMayBeMounted() bool {
	return av.DeviceMountState == operationexecutor.DeviceGloballyMounted ||
		av.DeviceMountState == operationexecutor.DeviceMountUncertain
}

// NewActualStateOfWorld returns a new instance of ActualStateOfWorld.
func NewActualStateOfWorld(
	nodeName types.NodeName,
	volumePluginMgr *volume.VolumePluginMgr) ActualStateOfWorld {
	return &actualStateOfWorld{
		nodeName:                        nodeName,
		attachedVolumes:                 make(map[v1.UniqueVolumeName]attachedVolume),
		foundDuringReconstruction:       make(map[v1.UniqueVolumeName]map[volumetypes.UniquePodName]types.UID),
		volumePluginMgr:                 volumePluginMgr,
		volumesWithFinalExpansionErrors: sets.New[v1.UniqueVolumeName](),
	}
}

// IsVolumeNotAttachedError returns true if the specified error is a
// volumeNotAttachedError.
func IsVolumeNotAttachedError(err error) bool {
	_, ok := err.(volumeNotAttachedError)
	return ok
}

// IsRemountRequiredError returns true if the specified error is a
// remountRequiredError.
func IsRemountRequiredError(err error) bool {
	_, ok := err.(remountRequiredError)
	return ok
}

type actualStateOfWorld struct {
	// nodeName is the name of this node. This value is passed to Attach/Detach
	nodeName types.NodeName

	// attachedVolumes is a map containing the set of volumes the kubelet volume
	// manager believes to be successfully attached to this node. Volume types
	// that do not implement an attacher interface are assumed to be in this
	// state by default.
	// The key in this map is the name of the volume and the value is an object
	// containing more information about the attached volume.
	attachedVolumes map[v1.UniqueVolumeName]attachedVolume
	// foundDuringReconstruction is a map of volumes which were discovered
	// from kubelet root directory when kubelet was restarted.
	foundDuringReconstruction map[v1.UniqueVolumeName]map[volumetypes.UniquePodName]types.UID

	volumesWithFinalExpansionErrors sets.Set[v1.UniqueVolumeName]

	// volumePluginMgr is the volume plugin manager used to create volume
	// plugin objects.
	volumePluginMgr *volume.VolumePluginMgr
	sync.RWMutex
}

type volumeAttachability string

const (
	volumeAttachabilityTrue      volumeAttachability = "True"
	volumeAttachabilityFalse     volumeAttachability = "False"
	volumeAttachabilityUncertain volumeAttachability = "Uncertain"
)

// attachedVolume represents a volume the kubelet volume manager believes to be
// successfully attached to a node it is managing. Volume types that do not
// implement an attacher are assumed to be in this state.
type attachedVolume struct {
	// volumeName contains the unique identifier for this volume.
	volumeName v1.UniqueVolumeName

	// mountedPods is a map containing the set of pods that this volume has been
	// successfully mounted to. The key in this map is the name of the pod and
	// the value is a mountedPod object containing more information about the
	// pod.
	mountedPods map[volumetypes.UniquePodName]mountedPod

	// spec is the volume spec containing the specification for this volume.
	// Used to generate the volume plugin object, and passed to plugin methods.
	// In particular, the Unmount method uses spec.Name() as the volumeSpecName
	// in the mount path:
	// /var/lib/kubelet/pods/{podUID}/volumes/{escapeQualifiedPluginName}/{volumeSpecName}/
	spec *volume.Spec

	// pluginName is the Unescaped Qualified name of the volume plugin used to
	// attach and mount this volume. It is stored separately in case the full
	// volume spec (everything except the name) can not be reconstructed for a
	// volume that should be unmounted (which would be the case for a mount path
	// read from disk without a full volume spec).
	pluginName string

	// pluginIsAttachable indicates the volume plugin used to attach and mount
	// this volume implements the volume.Attacher interface
	pluginIsAttachable volumeAttachability

	// deviceMountState stores information that tells us if device is mounted
	// globally or not
	deviceMountState operationexecutor.DeviceMountState

	// devicePath contains the path on the node where the volume is attached for
	// attachable volumes
	devicePath string

	// deviceMountPath contains the path on the node where the device should
	// be mounted after it is attached.
	deviceMountPath string

	// volumeInUseErrorForExpansion indicates volume driver has previously returned volume-in-use error
	// for this volume and volume expansion on this node should not be retried
	volumeInUseErrorForExpansion bool

	// persistentVolumeSize records size of the volume when pod was started or
	// size after successful completion of volume expansion operation.
	persistentVolumeSize resource.Quantity

	// seLinuxMountContext is the context with that the volume is mounted to global directory
	// (via -o context=XYZ mount option). If nil, the volume is not mounted. If "", the volume is
	// mounted without "-o context=".
	seLinuxMountContext *string
}

// The mountedPod object represents a pod for which the kubelet volume manager
// believes the underlying volume has been successfully been mounted.
type mountedPod struct {
	// the name of the pod
	podName volumetypes.UniquePodName

	// the UID of the pod
	podUID types.UID

	// mounter used to mount
	mounter volume.Mounter

	// mapper used to block volumes support
	blockVolumeMapper volume.BlockVolumeMapper

	// spec is the volume spec containing the specification for this volume.
	// Used to generate the volume plugin object, and passed to plugin methods.
	// In particular, the Unmount method uses spec.Name() as the volumeSpecName
	// in the mount path:
	// /var/lib/kubelet/pods/{podUID}/volumes/{escapeQualifiedPluginName}/{volumeSpecName}/
	volumeSpec *volume.Spec

	// remountRequired indicates the underlying volume has been successfully
	// mounted to this pod but it should be remounted to reflect changes in the
	// referencing pod.
	// Atomically updating volumes depend on this to update the contents of the
	// volume. All volume mounting calls should be idempotent so a second mount
	// call for volumes that do not need to update contents should not fail.
	remountRequired bool

	// volumeGIDValue contains the value of the GID annotation, if present.
	volumeGIDValue string

	// volumeMountStateForPod stores state of volume mount for the pod. if it is:
	//   - VolumeMounted: means volume for pod has been successfully mounted
	//   - VolumeMountUncertain: means volume for pod may not be mounted, but it must be unmounted
	volumeMountStateForPod operationexecutor.VolumeMountState

	// seLinuxMountContext is the context with that the volume is mounted to Pod directory
	// (via -o context=XYZ mount option). If nil, the volume is not mounted. If "", the volume is
	// mounted without "-o context=".
	seLinuxMountContext string
}

func (asw *actualStateOfWorld) MarkVolumeAsAttached(
	logger klog.Logger,
	volumeName v1.UniqueVolumeName, volumeSpec *volume.Spec, _ types.NodeName, devicePath string) error {

	pluginIsAttachable := volumeAttachabilityFalse
	if attachablePlugin, err := asw.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec); err == nil && attachablePlugin != nil {
		pluginIsAttachable = volumeAttachabilityTrue
	}

	return asw.addVolume(volumeName, volumeSpec, devicePath, pluginIsAttachable)
}

func (asw *actualStateOfWorld) AddAttachUncertainReconstructedVolume(
	volumeName v1.UniqueVolumeName, volumeSpec *volume.Spec, _ types.NodeName, devicePath string) error {

	return asw.addVolume(volumeName, volumeSpec, devicePath, volumeAttachabilityUncertain)
}

func (asw *actualStateOfWorld) MarkVolumeAsUncertain(
	logger klog.Logger, volumeName v1.UniqueVolumeName, volumeSpec *volume.Spec, _ types.NodeName) error {
	return nil
}

func (asw *actualStateOfWorld) MarkVolumeAsDetached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.DeleteVolume(volumeName)
}

func (asw *actualStateOfWorld) MarkVolumeExpansionFailedWithFinalError(volumeName v1.UniqueVolumeName) {
	asw.Lock()
	defer asw.Unlock()

	asw.volumesWithFinalExpansionErrors.Insert(volumeName)
}

func (asw *actualStateOfWorld) RemoveVolumeFromFailedWithFinalErrors(volumeName v1.UniqueVolumeName) {
	asw.Lock()
	defer asw.Unlock()

	asw.volumesWithFinalExpansionErrors.Delete(volumeName)
}

func (asw *actualStateOfWorld) CheckVolumeInFailedExpansionWithFinalErrors(volumeName v1.UniqueVolumeName) bool {
	asw.RLock()
	defer asw.RUnlock()

	return asw.volumesWithFinalExpansionErrors.Has(volumeName)
}

func (asw *actualStateOfWorld) IsVolumeReconstructed(volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName) bool {
	volumeState := asw.GetVolumeMountState(volumeName, podName)

	// only uncertain volumes are reconstructed
	if volumeState != operationexecutor.VolumeMountUncertain {
		return false
	}

	asw.RLock()
	defer asw.RUnlock()
	podMap, ok := asw.foundDuringReconstruction[volumeName]
	if !ok {
		return false
	}
	_, foundPod := podMap[podName]
	return foundPod
}

func (asw *actualStateOfWorld) IsVolumeDeviceReconstructed(volumeName v1.UniqueVolumeName) bool {
	asw.RLock()
	defer asw.RUnlock()
	_, ok := asw.foundDuringReconstruction[volumeName]
	return ok
}

func (asw *actualStateOfWorld) CheckAndMarkVolumeAsUncertainViaReconstruction(opts operationexecutor.MarkVolumeOpts) (bool, error) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[opts.VolumeName]
	if !volumeExists {
		return false, nil
	}

	podObj, podExists := volumeObj.mountedPods[opts.PodName]
	if podExists {
		// if volume mount was uncertain we should keep trying to unmount the volume
		if podObj.volumeMountStateForPod == operationexecutor.VolumeMountUncertain {
			return false, nil
		}
		if podObj.volumeMountStateForPod == operationexecutor.VolumeMounted {
			return false, nil
		}
	}

	podName := opts.PodName
	podUID := opts.PodUID
	volumeName := opts.VolumeName
	mounter := opts.Mounter
	blockVolumeMapper := opts.BlockVolumeMapper
	volumeGIDValue := opts.VolumeGIDVolume
	volumeSpec := opts.VolumeSpec

	podObj = mountedPod{
		podName:                podName,
		podUID:                 podUID,
		mounter:                mounter,
		blockVolumeMapper:      blockVolumeMapper,
		volumeGIDValue:         volumeGIDValue,
		volumeSpec:             volumeSpec,
		remountRequired:        false,
		volumeMountStateForPod: operationexecutor.VolumeMountUncertain,
	}

	if mounter != nil {
		// The mounter stored in the object may have old information,
		// use the newest one.
		podObj.mounter = mounter
	}

	asw.attachedVolumes[volumeName].mountedPods[podName] = podObj

	podMap, ok := asw.foundDuringReconstruction[opts.VolumeName]
	if !ok {
		podMap = map[volumetypes.UniquePodName]types.UID{}
	}
	podMap[opts.PodName] = opts.PodUID
	asw.foundDuringReconstruction[opts.VolumeName] = podMap
	return true, nil
}

func (asw *actualStateOfWorld) CheckAndMarkDeviceUncertainViaReconstruction(volumeName v1.UniqueVolumeName, deviceMountPath string) bool {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	// CheckAndMarkDeviceUncertainViaReconstruction requires volume to be marked as attached, so if
	// volume does not exist in ASOW or is in any state other than DeviceNotMounted we should return
	if !volumeExists || volumeObj.deviceMountState != operationexecutor.DeviceNotMounted {
		return false
	}

	volumeObj.deviceMountState = operationexecutor.DeviceMountUncertain
	// we are only changing deviceMountPath because devicePath at at this stage is
	// determined from node object.
	volumeObj.deviceMountPath = deviceMountPath
	asw.attachedVolumes[volumeName] = volumeObj
	return true

}

func (asw *actualStateOfWorld) MarkVolumeAsMounted(markVolumeOpts operationexecutor.MarkVolumeOpts) error {
	return asw.AddPodToVolume(markVolumeOpts)
}

func (asw *actualStateOfWorld) AddVolumeToReportAsAttached(logger klog.Logger, volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	// no operation for kubelet side
}

func (asw *actualStateOfWorld) RemoveVolumeFromReportAsAttached(volumeName v1.UniqueVolumeName, nodeName types.NodeName) error {
	// no operation for kubelet side
	return nil
}

func (asw *actualStateOfWorld) MarkVolumeAsUnmounted(
	podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName) error {
	return asw.DeletePodFromVolume(podName, volumeName)
}

func (asw *actualStateOfWorld) MarkDeviceAsMounted(
	volumeName v1.UniqueVolumeName, devicePath, deviceMountPath, seLinuxMountContext string) error {
	return asw.SetDeviceMountState(volumeName, operationexecutor.DeviceGloballyMounted, devicePath, deviceMountPath, seLinuxMountContext)
}

func (asw *actualStateOfWorld) MarkDeviceAsUncertain(
	volumeName v1.UniqueVolumeName, devicePath, deviceMountPath, seLinuxMountContext string) error {
	return asw.SetDeviceMountState(volumeName, operationexecutor.DeviceMountUncertain, devicePath, deviceMountPath, seLinuxMountContext)
}

func (asw *actualStateOfWorld) MarkVolumeMountAsUncertain(markVolumeOpts operationexecutor.MarkVolumeOpts) error {
	markVolumeOpts.VolumeMountState = operationexecutor.VolumeMountUncertain
	return asw.AddPodToVolume(markVolumeOpts)
}

func (asw *actualStateOfWorld) MarkDeviceAsUnmounted(
	volumeName v1.UniqueVolumeName) error {
	return asw.SetDeviceMountState(volumeName, operationexecutor.DeviceNotMounted, "", "", "")
}

func (asw *actualStateOfWorld) UpdateReconstructedDevicePath(volumeName v1.UniqueVolumeName, devicePath string) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return
	}
	if volumeObj.deviceMountState != operationexecutor.DeviceMountUncertain {
		// Reconciler must have updated volume state, i.e. when a pod uses the volume and
		// succeeded mounting the volume. Such update has fixed the device path.
		return
	}

	volumeObj.devicePath = devicePath
	asw.attachedVolumes[volumeName] = volumeObj
}

func (asw *actualStateOfWorld) UpdateReconstructedVolumeAttachability(volumeName v1.UniqueVolumeName, attachable bool) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return
	}
	if volumeObj.pluginIsAttachable != volumeAttachabilityUncertain {
		// Reconciler must have updated volume state, i.e. when a pod uses the volume and
		// succeeded mounting the volume. Such update has fixed the device path.
		return
	}

	if attachable {
		volumeObj.pluginIsAttachable = volumeAttachabilityTrue
	} else {
		volumeObj.pluginIsAttachable = volumeAttachabilityFalse
	}
	asw.attachedVolumes[volumeName] = volumeObj
}

func (asw *actualStateOfWorld) GetDeviceMountState(volumeName v1.UniqueVolumeName) operationexecutor.DeviceMountState {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return operationexecutor.DeviceNotMounted
	}

	return volumeObj.deviceMountState
}

func (asw *actualStateOfWorld) MarkForInUseExpansionError(volumeName v1.UniqueVolumeName) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, ok := asw.attachedVolumes[volumeName]
	if ok {
		volumeObj.volumeInUseErrorForExpansion = true
		asw.attachedVolumes[volumeName] = volumeObj
	}
}

func (asw *actualStateOfWorld) GetVolumeMountState(volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName) operationexecutor.VolumeMountState {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return operationexecutor.VolumeNotMounted
	}

	podObj, podExists := volumeObj.mountedPods[podName]
	if !podExists {
		return operationexecutor.VolumeNotMounted
	}
	return podObj.volumeMountStateForPod
}

func (asw *actualStateOfWorld) IsVolumeMountedElsewhere(volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName) bool {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return false
	}

	for _, podObj := range volumeObj.mountedPods {
		if podName != podObj.podName {
			// Treat uncertain mount state as mounted until certain.
			if podObj.volumeMountStateForPod != operationexecutor.VolumeNotMounted {
				return true
			}
		}
	}
	return false
}

// addVolume adds the given volume to the cache indicating the specified
// volume is attached to this node. If no volume name is supplied, a unique
// volume name is generated from the volumeSpec and returned on success. If a
// volume with the same generated name already exists, this is a noop. If no
// volume plugin can support the given volumeSpec or more than one plugin can
// support it, an error is returned.
func (asw *actualStateOfWorld) addVolume(
	volumeName v1.UniqueVolumeName, volumeSpec *volume.Spec, devicePath string, attachability volumeAttachability) error {
	asw.Lock()
	defer asw.Unlock()

	volumePlugin, err := asw.volumePluginMgr.FindPluginBySpec(volumeSpec)
	if err != nil || volumePlugin == nil {
		return fmt.Errorf(
			"failed to get Plugin from volumeSpec for volume %q err=%v",
			volumeSpec.Name(),
			err)
	}

	if len(volumeName) == 0 {
		volumeName, err = util.GetUniqueVolumeNameFromSpec(volumePlugin, volumeSpec)
		if err != nil {
			return fmt.Errorf(
				"failed to GetUniqueVolumeNameFromSpec for volumeSpec %q using volume plugin %q err=%v",
				volumeSpec.Name(),
				volumePlugin.GetPluginName(),
				err)
		}
	}

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		volumeObj = attachedVolume{
			volumeName:         volumeName,
			spec:               volumeSpec,
			mountedPods:        make(map[volumetypes.UniquePodName]mountedPod),
			pluginName:         volumePlugin.GetPluginName(),
			pluginIsAttachable: attachability,
			deviceMountState:   operationexecutor.DeviceNotMounted,
			devicePath:         devicePath,
		}
	} else {
		// If volume object already exists, update the fields such as device path
		volumeObj.devicePath = devicePath
		klog.V(2).InfoS("Volume is already added to attachedVolume list, update device path", "volumeName", volumeName, "path", devicePath)
	}
	asw.attachedVolumes[volumeName] = volumeObj

	return nil
}

func (asw *actualStateOfWorld) AddPodToVolume(markVolumeOpts operationexecutor.MarkVolumeOpts) error {
	podName := markVolumeOpts.PodName
	podUID := markVolumeOpts.PodUID
	volumeName := markVolumeOpts.VolumeName
	mounter := markVolumeOpts.Mounter
	blockVolumeMapper := markVolumeOpts.BlockVolumeMapper
	volumeGIDValue := markVolumeOpts.VolumeGIDVolume
	volumeSpec := markVolumeOpts.VolumeSpec
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return fmt.Errorf(
			"no volume with the name %q exists in the list of attached volumes",
			volumeName)
	}

	podObj, podExists := volumeObj.mountedPods[podName]

	updateUncertainVolume := false
	if podExists {
		// Update uncertain volumes - the new markVolumeOpts may have updated information.
		// Especially reconstructed volumes (marked as uncertain during reconstruction) need
		// an update.
		updateUncertainVolume = podObj.volumeMountStateForPod == operationexecutor.VolumeMountUncertain
	}
	if !podExists || updateUncertainVolume {
		// Add new mountedPod or update existing one.
		podObj = mountedPod{
			podName:                podName,
			podUID:                 podUID,
			mounter:                mounter,
			blockVolumeMapper:      blockVolumeMapper,
			volumeGIDValue:         volumeGIDValue,
			volumeSpec:             volumeSpec,
			volumeMountStateForPod: markVolumeOpts.VolumeMountState,
			seLinuxMountContext:    markVolumeOpts.SELinuxMountContext,
		}
	}

	// If pod exists, reset remountRequired value
	podObj.remountRequired = false
	podObj.volumeMountStateForPod = markVolumeOpts.VolumeMountState

	// if volume is mounted successfully, then it should be removed from foundDuringReconstruction map
	if markVolumeOpts.VolumeMountState == operationexecutor.VolumeMounted {
		delete(asw.foundDuringReconstruction[volumeName], podName)
	}
	if mounter != nil {
		// The mounter stored in the object may have old information,
		// use the newest one.
		podObj.mounter = mounter
	}
	asw.attachedVolumes[volumeName].mountedPods[podName] = podObj
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod) {
		// Store the mount context also in the AttachedVolume to have a global volume context
		// for a quick comparison in PodExistsInVolume.
		if volumeObj.seLinuxMountContext == nil {
			volumeObj.seLinuxMountContext = &markVolumeOpts.SELinuxMountContext
			asw.attachedVolumes[volumeName] = volumeObj
		}
	}

	return nil
}

func (asw *actualStateOfWorld) MarkVolumeAsResized(volumeName v1.UniqueVolumeName, claimSize resource.Quantity) bool {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, ok := asw.attachedVolumes[volumeName]
	if ok {
		volumeObj.persistentVolumeSize = claimSize
		asw.attachedVolumes[volumeName] = volumeObj
		return true
	}
	return false
}

func (asw *actualStateOfWorld) MarkRemountRequired(
	podName volumetypes.UniquePodName) {
	asw.Lock()
	defer asw.Unlock()
	for volumeName, volumeObj := range asw.attachedVolumes {
		if podObj, podExists := volumeObj.mountedPods[podName]; podExists {
			volumePlugin, err :=
				asw.volumePluginMgr.FindPluginBySpec(podObj.volumeSpec)
			if err != nil || volumePlugin == nil {
				// Log and continue processing
				klog.ErrorS(nil, "MarkRemountRequired failed to FindPluginBySpec for volume", "uniquePodName", podObj.podName, "podUID", podObj.podUID, "volumeName", volumeName, "volumeSpecName", podObj.volumeSpec.Name())
				continue
			}

			if volumePlugin.RequiresRemount(podObj.volumeSpec) {
				podObj.remountRequired = true
				asw.attachedVolumes[volumeName].mountedPods[podName] = podObj
			}
		}
	}
}

func (asw *actualStateOfWorld) SetDeviceMountState(
	volumeName v1.UniqueVolumeName, deviceMountState operationexecutor.DeviceMountState, devicePath, deviceMountPath, seLinuxMountContext string) error {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return fmt.Errorf(
			"no volume with the name %q exists in the list of attached volumes",
			volumeName)
	}

	volumeObj.deviceMountState = deviceMountState
	volumeObj.deviceMountPath = deviceMountPath
	if devicePath != "" {
		volumeObj.devicePath = devicePath
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod) {
		if seLinuxMountContext != "" {
			volumeObj.seLinuxMountContext = &seLinuxMountContext
		}
	}

	asw.attachedVolumes[volumeName] = volumeObj
	return nil
}

func (asw *actualStateOfWorld) InitializeClaimSize(logger klog.Logger, volumeName v1.UniqueVolumeName, claimSize resource.Quantity) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, ok := asw.attachedVolumes[volumeName]
	// only set volume claim size if claimStatusSize is zero
	// this can happen when volume was rebuilt after kubelet startup
	if ok && volumeObj.persistentVolumeSize.IsZero() {
		volumeObj.persistentVolumeSize = claimSize
		asw.attachedVolumes[volumeName] = volumeObj
	}
}

func (asw *actualStateOfWorld) GetClaimSize(volumeName v1.UniqueVolumeName) resource.Quantity {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, ok := asw.attachedVolumes[volumeName]
	if ok {
		return volumeObj.persistentVolumeSize.DeepCopy()
	}
	return resource.Quantity{}
}

func (asw *actualStateOfWorld) DeletePodFromVolume(
	podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName) error {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return fmt.Errorf(
			"no volume with the name %q exists in the list of attached volumes",
			volumeName)
	}

	_, podExists := volumeObj.mountedPods[podName]
	if podExists {
		delete(asw.attachedVolumes[volumeName].mountedPods, podName)
	}

	// if there were reconstructed volumes, we should remove them
	_, podExists = asw.foundDuringReconstruction[volumeName]
	if podExists {
		delete(asw.foundDuringReconstruction[volumeName], podName)
	}

	return nil
}

func (asw *actualStateOfWorld) DeleteVolume(volumeName v1.UniqueVolumeName) error {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return nil
	}

	if len(volumeObj.mountedPods) != 0 {
		return fmt.Errorf(
			"failed to DeleteVolume %q, it still has %v mountedPods",
			volumeName,
			len(volumeObj.mountedPods))
	}

	delete(asw.attachedVolumes, volumeName)
	delete(asw.foundDuringReconstruction, volumeName)
	return nil
}

func (asw *actualStateOfWorld) PodExistsInVolume(podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName, desiredVolumeSize resource.Quantity, seLinuxLabel string) (bool, string, error) {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return false, "", newVolumeNotAttachedError(volumeName)
	}

	// The volume exists, check its SELinux context mount option
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod) {
		if volumeObj.seLinuxMountContext != nil && *volumeObj.seLinuxMountContext != seLinuxLabel {
			fullErr := newSELinuxMountMismatchError(volumeName)
			return false, volumeObj.devicePath, fullErr
		}
	}

	podObj, podExists := volumeObj.mountedPods[podName]
	if podExists {
		// if volume mount was uncertain we should keep trying to mount the volume
		if podObj.volumeMountStateForPod == operationexecutor.VolumeMountUncertain {
			return false, volumeObj.devicePath, nil
		}
		if podObj.remountRequired {
			return true, volumeObj.devicePath, newRemountRequiredError(volumeObj.volumeName, podObj.podName)
		}
		if currentSize, expandVolume := asw.volumeNeedsExpansion(volumeObj, desiredVolumeSize); expandVolume {
			return true, volumeObj.devicePath, newFsResizeRequiredError(volumeObj.volumeName, podObj.podName, currentSize)
		}
	}

	return podExists, volumeObj.devicePath, nil
}

func (asw *actualStateOfWorld) PodHasMountedVolumes(podName volumetypes.UniquePodName) bool {
	asw.RLock()
	defer asw.RUnlock()
	for _, volumeObj := range asw.attachedVolumes {
		if podObj, hasPod := volumeObj.mountedPods[podName]; hasPod {
			if podObj.volumeMountStateForPod == operationexecutor.VolumeMounted {
				return true
			}
		}
	}

	return false
}

func (asw *actualStateOfWorld) volumeNeedsExpansion(volumeObj attachedVolume, desiredVolumeSize resource.Quantity) (resource.Quantity, bool) {
	currentSize := volumeObj.persistentVolumeSize.DeepCopy()
	if volumeObj.volumeInUseErrorForExpansion {
		return currentSize, false
	}
	if volumeObj.persistentVolumeSize.IsZero() || desiredVolumeSize.IsZero() {
		return currentSize, false
	}

	klog.V(5).InfoS("NodeExpandVolume checking size", "actualSize", volumeObj.persistentVolumeSize.String(), "desiredSize", desiredVolumeSize.String(), "volume", volumeObj.volumeName)

	if desiredVolumeSize.Cmp(volumeObj.persistentVolumeSize) > 0 {
		volumePlugin, err := asw.volumePluginMgr.FindNodeExpandablePluginBySpec(volumeObj.spec)
		if err != nil || volumePlugin == nil {
			// Log and continue processing
			klog.InfoS("PodExistsInVolume failed to find expandable plugin",
				"volume", volumeObj.volumeName,
				"volumeSpecName", volumeObj.spec.Name())
			return currentSize, false
		}
		if volumePlugin.RequiresFSResize() {
			return currentSize, true
		}
	}
	return currentSize, false
}

func (asw *actualStateOfWorld) PodRemovedFromVolume(
	podName volumetypes.UniquePodName,
	volumeName v1.UniqueVolumeName) bool {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return true
	}

	podObj, podExists := volumeObj.mountedPods[podName]
	if podExists {
		// if volume mount was uncertain we should keep trying to unmount the volume
		if podObj.volumeMountStateForPod == operationexecutor.VolumeMountUncertain {
			return false
		}
		if podObj.volumeMountStateForPod == operationexecutor.VolumeMounted {
			return false
		}
	}
	return true
}

func (asw *actualStateOfWorld) VolumeExistsWithSpecName(podName volumetypes.UniquePodName, volumeSpecName string) bool {
	asw.RLock()
	defer asw.RUnlock()
	for _, volumeObj := range asw.attachedVolumes {
		if podObj, podExists := volumeObj.mountedPods[podName]; podExists {
			if podObj.volumeSpec.Name() == volumeSpecName {
				return true
			}
		}
	}
	return false
}

func (asw *actualStateOfWorld) VolumeExists(
	volumeName v1.UniqueVolumeName) bool {
	asw.RLock()
	defer asw.RUnlock()

	_, volumeExists := asw.attachedVolumes[volumeName]
	return volumeExists
}

func (asw *actualStateOfWorld) GetMountedVolumes() []MountedVolume {
	asw.RLock()
	defer asw.RUnlock()
	mountedVolume := make([]MountedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		for _, podObj := range volumeObj.mountedPods {
			if podObj.volumeMountStateForPod == operationexecutor.VolumeMounted {
				mountedVolume = append(
					mountedVolume,
					getMountedVolume(&podObj, &volumeObj))
			}
		}
	}
	return mountedVolume
}

// GetAllMountedVolumes returns all volumes which could be locally mounted for a pod.
func (asw *actualStateOfWorld) GetAllMountedVolumes() []MountedVolume {
	asw.RLock()
	defer asw.RUnlock()
	mountedVolume := make([]MountedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		for _, podObj := range volumeObj.mountedPods {
			if podObj.volumeMountStateForPod == operationexecutor.VolumeMounted ||
				podObj.volumeMountStateForPod == operationexecutor.VolumeMountUncertain {
				mountedVolume = append(
					mountedVolume,
					getMountedVolume(&podObj, &volumeObj))
			}
		}
	}

	return mountedVolume
}

func (asw *actualStateOfWorld) GetMountedVolumesForPod(
	podName volumetypes.UniquePodName) []MountedVolume {
	asw.RLock()
	defer asw.RUnlock()
	mountedVolume := make([]MountedVolume, 0 /* len */)
	for _, volumeObj := range asw.attachedVolumes {
		for mountedPodName, podObj := range volumeObj.mountedPods {
			if mountedPodName == podName && podObj.volumeMountStateForPod == operationexecutor.VolumeMounted {
				mountedVolume = append(
					mountedVolume,
					getMountedVolume(&podObj, &volumeObj))
			}
		}
	}

	return mountedVolume
}

func (asw *actualStateOfWorld) GetMountedVolumeForPod(
	podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName) (MountedVolume, bool) {
	asw.RLock()
	defer asw.RUnlock()
	volumeObj := asw.attachedVolumes[volumeName]
	if podObj, hasPod := volumeObj.mountedPods[podName]; hasPod {
		if podObj.volumeMountStateForPod == operationexecutor.VolumeMounted {
			return getMountedVolume(&podObj, &volumeObj), true
		}
	}

	return MountedVolume{}, false
}

func (asw *actualStateOfWorld) GetPossiblyMountedVolumesForPod(
	podName volumetypes.UniquePodName) []MountedVolume {
	asw.RLock()
	defer asw.RUnlock()
	mountedVolume := make([]MountedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		for mountedPodName, podObj := range volumeObj.mountedPods {
			if mountedPodName == podName &&
				(podObj.volumeMountStateForPod == operationexecutor.VolumeMounted ||
					podObj.volumeMountStateForPod == operationexecutor.VolumeMountUncertain) {
				mountedVolume = append(
					mountedVolume,
					getMountedVolume(&podObj, &volumeObj))
			}
		}
	}

	return mountedVolume
}

func (asw *actualStateOfWorld) GetGloballyMountedVolumes() []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()
	globallyMountedVolumes := make(
		[]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		if volumeObj.deviceMountState == operationexecutor.DeviceGloballyMounted {
			globallyMountedVolumes = append(
				globallyMountedVolumes,
				asw.newAttachedVolume(&volumeObj))
		}
	}

	return globallyMountedVolumes
}

func (asw *actualStateOfWorld) GetAttachedVolumes() []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()
	allAttachedVolumes := make(
		[]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		allAttachedVolumes = append(
			allAttachedVolumes,
			asw.newAttachedVolume(&volumeObj))
	}

	return allAttachedVolumes
}

func (asw *actualStateOfWorld) GetAttachedVolume(volumeName v1.UniqueVolumeName) (AttachedVolume, bool) {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, ok := asw.attachedVolumes[volumeName]
	if !ok {
		return AttachedVolume{}, false
	}

	return asw.newAttachedVolume(&volumeObj), true
}

func (asw *actualStateOfWorld) GetUnmountedVolumes() []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()
	unmountedVolumes := make([]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		if len(volumeObj.mountedPods) == 0 {
			unmountedVolumes = append(
				unmountedVolumes,
				asw.newAttachedVolume(&volumeObj))
		}
	}

	return unmountedVolumes
}

func (asw *actualStateOfWorld) newAttachedVolume(
	attachedVolume *attachedVolume) AttachedVolume {
	seLinuxMountContext := ""
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod) {
		if attachedVolume.seLinuxMountContext != nil {
			seLinuxMountContext = *attachedVolume.seLinuxMountContext
		}
	}
	return AttachedVolume{
		AttachedVolume: operationexecutor.AttachedVolume{
			VolumeName:          attachedVolume.volumeName,
			VolumeSpec:          attachedVolume.spec,
			NodeName:            asw.nodeName,
			PluginIsAttachable:  attachedVolume.pluginIsAttachable == volumeAttachabilityTrue,
			DevicePath:          attachedVolume.devicePath,
			DeviceMountPath:     attachedVolume.deviceMountPath,
			PluginName:          attachedVolume.pluginName,
			SELinuxMountContext: seLinuxMountContext},
		DeviceMountState:    attachedVolume.deviceMountState,
		SELinuxMountContext: seLinuxMountContext,
	}
}

// Compile-time check to ensure volumeNotAttachedError implements the error interface
var _ error = volumeNotAttachedError{}

// volumeNotAttachedError is an error returned when PodExistsInVolume() fails to
// find specified volume in the list of attached volumes.
type volumeNotAttachedError struct {
	volumeName v1.UniqueVolumeName
}

func (err volumeNotAttachedError) Error() string {
	return fmt.Sprintf(
		"volumeName %q does not exist in the list of attached volumes",
		err.volumeName)
}

func newVolumeNotAttachedError(volumeName v1.UniqueVolumeName) error {
	return volumeNotAttachedError{
		volumeName: volumeName,
	}
}

// Compile-time check to ensure remountRequiredError implements the error interface
var _ error = remountRequiredError{}

// remountRequiredError is an error returned when PodExistsInVolume() found
// volume/pod attached/mounted but remountRequired was true, indicating the
// given volume should be remounted to the pod to reflect changes in the
// referencing pod.
type remountRequiredError struct {
	volumeName v1.UniqueVolumeName
	podName    volumetypes.UniquePodName
}

func (err remountRequiredError) Error() string {
	return fmt.Sprintf(
		"volumeName %q is mounted to %q but should be remounted",
		err.volumeName, err.podName)
}

func newRemountRequiredError(
	volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName) error {
	return remountRequiredError{
		volumeName: volumeName,
		podName:    podName,
	}
}

// fsResizeRequiredError is an error returned when PodExistsInVolume() found
// volume/pod attached/mounted but fsResizeRequired was true, indicating the
// given volume receives an resize request after attached/mounted.
type FsResizeRequiredError struct {
	CurrentSize resource.Quantity
	volumeName  v1.UniqueVolumeName
	podName     volumetypes.UniquePodName
}

func (err FsResizeRequiredError) Error() string {
	return fmt.Sprintf(
		"volumeName %q mounted to %q needs to resize file system",
		err.volumeName, err.podName)
}

func newFsResizeRequiredError(
	volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName, currentSize resource.Quantity) error {
	return FsResizeRequiredError{
		CurrentSize: currentSize,
		volumeName:  volumeName,
		podName:     podName,
	}
}

// IsFSResizeRequiredError returns true if the specified error is a
// fsResizeRequiredError.
func IsFSResizeRequiredError(err error) bool {
	_, ok := err.(FsResizeRequiredError)
	return ok
}

// getMountedVolume constructs and returns a MountedVolume object from the given
// mountedPod and attachedVolume objects.
func getMountedVolume(
	mountedPod *mountedPod, attachedVolume *attachedVolume) MountedVolume {
	seLinuxMountContext := ""
	if attachedVolume.seLinuxMountContext != nil {
		seLinuxMountContext = *attachedVolume.seLinuxMountContext
	}
	return MountedVolume{
		MountedVolume: operationexecutor.MountedVolume{
			PodName:             mountedPod.podName,
			VolumeName:          attachedVolume.volumeName,
			InnerVolumeSpecName: mountedPod.volumeSpec.Name(),
			PluginName:          attachedVolume.pluginName,
			PodUID:              mountedPod.podUID,
			Mounter:             mountedPod.mounter,
			BlockVolumeMapper:   mountedPod.blockVolumeMapper,
			VolumeGIDValue:      mountedPod.volumeGIDValue,
			VolumeSpec:          mountedPod.volumeSpec,
			DeviceMountPath:     attachedVolume.deviceMountPath,
			SELinuxMountContext: seLinuxMountContext}}

}

// seLinuxMountMismatchError is an error returned when PodExistsInVolume() found
// a volume mounted with a different SELinux label than expected.
type seLinuxMountMismatchError struct {
	volumeName v1.UniqueVolumeName
}

func (err seLinuxMountMismatchError) Error() string {
	return fmt.Sprintf(
		"waiting for unmount of volume %q, because it is already mounted to a different pod with a different SELinux label",
		err.volumeName)
}

func newSELinuxMountMismatchError(volumeName v1.UniqueVolumeName) error {
	return seLinuxMountMismatchError{
		volumeName: volumeName,
	}
}

// IsSELinuxMountMismatchError returns true if the specified error is a
// seLinuxMountMismatchError.
func IsSELinuxMountMismatchError(err error) bool {
	_, ok := err.(seLinuxMountMismatchError)
	return ok
}
