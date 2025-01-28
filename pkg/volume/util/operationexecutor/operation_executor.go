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

// Package operationexecutor implements interfaces that enable execution of
// attach, detach, mount, and unmount operations with a
// nestedpendingoperations so that more than one operation is never triggered
// on the same volume for the same pod.
package operationexecutor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/nestedpendingoperations"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

// OperationExecutor defines a set of operations for attaching, detaching,
// mounting, or unmounting a volume that are executed with a NewNestedPendingOperations which
// prevents more than one operation from being triggered on the same volume.
//
// These operations should be idempotent (for example, AttachVolume should
// still succeed if the volume is already attached to the node, etc.). However,
// they depend on the volume plugins to implement this behavior.
//
// Once an operation completes successfully, the actualStateOfWorld is updated
// to indicate the volume is attached/detached/mounted/unmounted.
//
// If the OperationExecutor fails to start the operation because, for example,
// an operation with the same UniqueVolumeName is already pending, a non-nil
// error is returned.
//
// Once the operation is started, since it is executed asynchronously,
// errors are simply logged and the goroutine is terminated without updating
// actualStateOfWorld (callers are responsible for retrying as needed).
//
// Some of these operations may result in calls to the API server; callers are
// responsible for rate limiting on errors.
type OperationExecutor interface {
	// AttachVolume attaches the volume to the node specified in volumeToAttach.
	// It then updates the actual state of the world to reflect that.
	AttachVolume(logger klog.Logger, volumeToAttach VolumeToAttach, actualStateOfWorld ActualStateOfWorldAttacherUpdater) error

	// VerifyVolumesAreAttachedPerNode verifies the given list of volumes to see whether they are still attached to the node.
	// If any volume is not attached right now, it will update the actual state of the world to reflect that.
	// Note that this operation could be operated concurrently with other attach/detach operations.
	// In theory (but very unlikely in practise), race condition among these operations might mark volume as detached
	// even if it is attached. But reconciler can correct this in a short period of time.
	VerifyVolumesAreAttachedPerNode(AttachedVolumes []AttachedVolume, nodeName types.NodeName, actualStateOfWorld ActualStateOfWorldAttacherUpdater) error

	// VerifyVolumesAreAttached verifies volumes being used in entire cluster and if they are still attached to the node
	// If any volume is not attached right now, it will update actual state of world to reflect that.
	VerifyVolumesAreAttached(volumesToVerify map[types.NodeName][]AttachedVolume, actualStateOfWorld ActualStateOfWorldAttacherUpdater)

	// DetachVolume detaches the volume from the node specified in
	// volumeToDetach, and updates the actual state of the world to reflect
	// that. If verifySafeToDetach is set, a call is made to the fetch the node
	// object and it is used to verify that the volume does not exist in Node's
	// Status.VolumesInUse list (operation fails with error if it is).
	DetachVolume(logger klog.Logger, volumeToDetach AttachedVolume, verifySafeToDetach bool, actualStateOfWorld ActualStateOfWorldAttacherUpdater) error

	// If a volume has 'Filesystem' volumeMode, MountVolume mounts the
	// volume to the pod specified in volumeToMount.
	// Specifically it will:
	// * Wait for the device to finish attaching (for attachable volumes only).
	// * Mount device to global mount path (for attachable volumes only).
	// * Update actual state of world to reflect volume is globally mounted (for
	//   attachable volumes only).
	// * Mount the volume to the pod specific path.
	// * Update actual state of world to reflect volume is mounted to the pod
	//   path.
	// The parameter "isRemount" is informational and used to adjust logging
	// verbosity. An initial mount is more log-worthy than a remount, for
	// example.
	//
	// For 'Block' volumeMode, this method creates a symbolic link to
	// the volume from both the pod specified in volumeToMount and global map path.
	// Specifically it will:
	// * Wait for the device to finish attaching (for attachable volumes only).
	// * Update actual state of world to reflect volume is globally mounted/mapped.
	// * Map volume to global map path using symbolic link.
	// * Map the volume to the pod device map path using symbolic link.
	// * Update actual state of world to reflect volume is mounted/mapped to the pod path.
	MountVolume(waitForAttachTimeout time.Duration, volumeToMount VolumeToMount, actualStateOfWorld ActualStateOfWorldMounterUpdater, isRemount bool) error

	// If a volume has 'Filesystem' volumeMode, UnmountVolume unmounts the
	// volume from the pod specified in volumeToUnmount and updates the actual
	// state of the world to reflect that.
	//
	// For 'Block' volumeMode, this method unmaps symbolic link to the volume
	// from both the pod device map path in volumeToUnmount and global map path.
	// And then, updates the actual state of the world to reflect that.
	UnmountVolume(volumeToUnmount MountedVolume, actualStateOfWorld ActualStateOfWorldMounterUpdater, podsDir string) error

	// If a volume has 'Filesystem' volumeMode, UnmountDevice unmounts the
	// volumes global mount path from the device (for attachable volumes only,
	// freeing it for detach. It then updates the actual state of the world to
	// reflect that.
	//
	// For 'Block' volumeMode, this method checks number of symbolic links under
	// global map path. If number of reference is zero, remove global map path
	// directory and free a volume for detach.
	// It then updates the actual state of the world to reflect that.
	UnmountDevice(deviceToDetach AttachedVolume, actualStateOfWorld ActualStateOfWorldMounterUpdater, hostutil hostutil.HostUtils) error

	// VerifyControllerAttachedVolume checks if the specified volume is present
	// in the specified nodes AttachedVolumes Status field. It uses kubeClient
	// to fetch the node object.
	// If the volume is found, the actual state of the world is updated to mark
	// the volume as attached.
	// If the volume does not implement the attacher interface, it is assumed to
	// be attached and the actual state of the world is updated accordingly.
	// If the volume is not found or there is an error (fetching the node
	// object, for example) then an error is returned which triggers exponential
	// back off on retries.
	VerifyControllerAttachedVolume(logger klog.Logger, volumeToMount VolumeToMount, nodeName types.NodeName, actualStateOfWorld ActualStateOfWorldAttacherUpdater) error

	// IsOperationPending returns true if an operation for the given volumeName
	// and one of podName or nodeName is pending, otherwise it returns false
	IsOperationPending(volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName, nodeName types.NodeName) bool
	// IsOperationSafeToRetry returns false if an operation for the given volumeName
	// and one of podName or nodeName is pending or in exponential backoff, otherwise it returns true
	IsOperationSafeToRetry(volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName, nodeName types.NodeName, operationName string) bool
	// ExpandInUseVolume will resize volume's file system to expected size without unmounting the volume.
	ExpandInUseVolume(volumeToMount VolumeToMount, actualStateOfWorld ActualStateOfWorldMounterUpdater, currentSize resource.Quantity) error
	// ReconstructVolumeOperation construct a new volumeSpec and returns it created by plugin
	ReconstructVolumeOperation(volumeMode v1.PersistentVolumeMode, plugin volume.VolumePlugin, mapperPlugin volume.BlockVolumePlugin, uid types.UID, podName volumetypes.UniquePodName, volumeSpecName string, volumePath string, pluginName string) (volume.ReconstructedVolume, error)
}

// NewOperationExecutor returns a new instance of OperationExecutor.
func NewOperationExecutor(
	operationGenerator OperationGenerator) OperationExecutor {

	return &operationExecutor{
		pendingOperations: nestedpendingoperations.NewNestedPendingOperations(
			true /* exponentialBackOffOnError */),
		operationGenerator: operationGenerator,
	}
}

// MarkVolumeOpts is a struct to pass arguments to MountVolume functions
type MarkVolumeOpts struct {
	PodName             volumetypes.UniquePodName
	PodUID              types.UID
	VolumeName          v1.UniqueVolumeName
	Mounter             volume.Mounter
	BlockVolumeMapper   volume.BlockVolumeMapper
	OuterVolumeSpecName string
	VolumeGIDVolume     string
	VolumeSpec          *volume.Spec
	VolumeMountState    VolumeMountState
	SELinuxMountContext string
}

// ActualStateOfWorldMounterUpdater defines a set of operations updating the actual
// state of the world cache after successful mount/unmount.
type ActualStateOfWorldMounterUpdater interface {
	// Marks the specified volume as mounted to the specified pod
	MarkVolumeAsMounted(markVolumeOpts MarkVolumeOpts) error

	// Marks the specified volume as unmounted from the specified pod
	MarkVolumeAsUnmounted(podName volumetypes.UniquePodName, volumeName v1.UniqueVolumeName) error

	// MarkVolumeMountAsUncertain marks state of volume mount for the pod uncertain
	MarkVolumeMountAsUncertain(markVolumeOpts MarkVolumeOpts) error

	// Marks the specified volume as having been globally mounted.
	MarkDeviceAsMounted(volumeName v1.UniqueVolumeName, devicePath, deviceMountPath, seLinuxMountContext string) error

	// MarkDeviceAsUncertain marks device state in global mount path as uncertain
	MarkDeviceAsUncertain(volumeName v1.UniqueVolumeName, devicePath, deviceMountPath, seLinuxMountContext string) error

	// Marks the specified volume as having its global mount unmounted.
	MarkDeviceAsUnmounted(volumeName v1.UniqueVolumeName) error

	// Marks the specified volume's file system resize request is finished.
	MarkVolumeAsResized(volumeName v1.UniqueVolumeName, claimSize resource.Quantity) bool

	// GetDeviceMountState returns mount state of the device in global path
	GetDeviceMountState(volumeName v1.UniqueVolumeName) DeviceMountState

	// GetVolumeMountState returns mount state of the volume for the Pod
	GetVolumeMountState(volumName v1.UniqueVolumeName, podName volumetypes.UniquePodName) VolumeMountState

	// IsVolumeMountedElsewhere returns whether the supplied volume is mounted in a Pod other than the supplied one
	IsVolumeMountedElsewhere(volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName) bool

	// MarkForInUseExpansionError marks the volume to have in-use error during expansion.
	// volume expansion must not be retried for this volume
	MarkForInUseExpansionError(volumeName v1.UniqueVolumeName)

	// CheckAndMarkVolumeAsUncertainViaReconstruction only adds volume to actual state of the world
	// if volume was not already there. This avoid overwriting in any previously stored
	// state. It returns error if there was an error adding the volume to ASOW.
	// It returns true, if this operation resulted in volume being added to ASOW
	// otherwise it returns false.
	CheckAndMarkVolumeAsUncertainViaReconstruction(opts MarkVolumeOpts) (bool, error)

	// CheckAndMarkDeviceUncertainViaReconstruction only adds device to actual state of the world
	// if device was not already there. This avoids overwriting in any previously stored
	// state. We only supply deviceMountPath because devicePath is already determined from
	// VerifyControllerAttachedVolume function.
	CheckAndMarkDeviceUncertainViaReconstruction(volumeName v1.UniqueVolumeName, deviceMountPath string) bool

	// IsVolumeReconstructed returns true if volume currently added to actual state of the world
	// was found during reconstruction.
	IsVolumeReconstructed(volumeName v1.UniqueVolumeName, podName volumetypes.UniquePodName) bool

	// IsVolumeDeviceReconstructed returns true if volume device identified by volumeName has been
	// found during reconstruction.
	IsVolumeDeviceReconstructed(volumeName v1.UniqueVolumeName) bool

	// MarkVolumeExpansionFailedWithFinalError marks volume as failed with a final error, so as
	// this state doesn't have to be recorded in the API server
	MarkVolumeExpansionFailedWithFinalError(volumeName v1.UniqueVolumeName)

	// RemoveVolumeFromFailedWithFinalErrors removes volume from list that indicates that volume
	// has failed expansion with a final error
	RemoveVolumeFromFailedWithFinalErrors(volumeName v1.UniqueVolumeName)

	// CheckVolumeInFailedExpansionWithFinalErrors verifies if volume expansion has failed with a final
	// error
	CheckVolumeInFailedExpansionWithFinalErrors(volumeName v1.UniqueVolumeName) bool
}

// ActualStateOfWorldAttacherUpdater defines a set of operations updating the
// actual state of the world cache after successful attach/detach/mount/unmount.
type ActualStateOfWorldAttacherUpdater interface {
	// Marks the specified volume as attached to the specified node.  If the
	// volume name is supplied, that volume name will be used.  If not, the
	// volume name is computed using the result from querying the plugin.
	//
	// TODO: in the future, we should be able to remove the volumeName
	// argument to this method -- since it is used only for attachable
	// volumes.  See issue 29695.
	MarkVolumeAsAttached(logger klog.Logger, volumeName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string) error

	// Marks the specified volume as *possibly* attached to the specified node.
	// If an attach operation fails, the attach/detach controller does not know for certain if the volume is attached or not.
	// If the volume name is supplied, that volume name will be used.  If not, the
	// volume name is computed using the result from querying the plugin.
	MarkVolumeAsUncertain(logger klog.Logger, volumeName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName) error

	// Marks the specified volume as detached from the specified node
	MarkVolumeAsDetached(volumeName v1.UniqueVolumeName, nodeName types.NodeName)

	// Marks desire to detach the specified volume (remove the volume from the node's
	// volumesToReportAsAttached list)
	RemoveVolumeFromReportAsAttached(volumeName v1.UniqueVolumeName, nodeName types.NodeName) error

	// Unmarks the desire to detach for the specified volume (add the volume back to
	// the node's volumesToReportAsAttached list)
	AddVolumeToReportAsAttached(logger klog.Logger, volumeName v1.UniqueVolumeName, nodeName types.NodeName)

	// InitializeClaimSize sets pvc claim size by reading pvc.Status.Capacity
	InitializeClaimSize(logger klog.Logger, volumeName v1.UniqueVolumeName, claimSize resource.Quantity)

	GetClaimSize(volumeName v1.UniqueVolumeName) resource.Quantity
}

// VolumeLogger defines a set of operations for generating volume-related logging and error msgs
type VolumeLogger interface {
	// Creates a detailed msg that can be used in logs
	// The msg format follows the pattern "<prefixMsg> <volume details> <suffixMsg>",
	// where each implementation provides the volume details
	GenerateMsgDetailed(prefixMsg, suffixMsg string) (detailedMsg string)

	// Creates a detailed error that can be used in logs.
	// The msg format follows the pattern "<prefixMsg> <volume details>: <err> ",
	GenerateErrorDetailed(prefixMsg string, err error) (detailedErr error)

	// Creates a simple msg that is user friendly and a detailed msg that can be used in logs
	// The msg format follows the pattern "<prefixMsg> <volume details> <suffixMsg>",
	// where each implementation provides the volume details
	GenerateMsg(prefixMsg, suffixMsg string) (simpleMsg, detailedMsg string)

	// Creates a simple error that is user friendly and a detailed error that can be used in logs.
	// The msg format follows the pattern "<prefixMsg> <volume details>: <err> ",
	GenerateError(prefixMsg string, err error) (simpleErr, detailedErr error)
}

// Generates an error string with the format ": <err>" if err exists
func errSuffix(err error) string {
	errStr := ""
	if err != nil {
		errStr = fmt.Sprintf(": %v", err)
	}
	return errStr
}

// Generate a detailed error msg for logs
func generateVolumeMsgDetailed(prefixMsg, suffixMsg, volumeName, details string) (detailedMsg string) {
	return fmt.Sprintf("%v for volume %q %v %v", prefixMsg, volumeName, details, suffixMsg)
}

// Generate a simplified error msg for events and a detailed error msg for logs
func generateVolumeMsg(prefixMsg, suffixMsg, volumeName, details string) (simpleMsg, detailedMsg string) {
	simpleMsg = fmt.Sprintf("%v for volume %q %v", prefixMsg, volumeName, suffixMsg)
	return simpleMsg, generateVolumeMsgDetailed(prefixMsg, suffixMsg, volumeName, details)
}

// VolumeToAttach represents a volume that should be attached to a node.
type VolumeToAttach struct {
	// MultiAttachErrorReported indicates whether the multi-attach error has been reported for the given volume.
	// It is used to prevent reporting the error from being reported more than once for a given volume.
	MultiAttachErrorReported bool

	// VolumeName is the unique identifier for the volume that should be
	// attached.
	VolumeName v1.UniqueVolumeName

	// VolumeSpec is a volume spec containing the specification for the volume
	// that should be attached.
	VolumeSpec *volume.Spec

	// NodeName is the identifier for the node that the volume should be
	// attached to.
	NodeName types.NodeName

	// scheduledPods is a map containing the set of pods that reference this
	// volume and are scheduled to the underlying node. The key in the map is
	// the name of the pod and the value is a pod object containing more
	// information about the pod.
	ScheduledPods []*v1.Pod
}

// GenerateMsgDetailed returns detailed msgs for volumes to attach
func (volume *VolumeToAttach) GenerateMsgDetailed(prefixMsg, suffixMsg string) (detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) from node %q", volume.VolumeName, volume.NodeName)
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return generateVolumeMsgDetailed(prefixMsg, suffixMsg, volumeSpecName, detailedStr)
}

// GenerateMsg returns simple and detailed msgs for volumes to attach
func (volume *VolumeToAttach) GenerateMsg(prefixMsg, suffixMsg string) (simpleMsg, detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) from node %q", volume.VolumeName, volume.NodeName)
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return generateVolumeMsg(prefixMsg, suffixMsg, volumeSpecName, detailedStr)
}

// GenerateErrorDetailed returns detailed errors for volumes to attach
func (volume *VolumeToAttach) GenerateErrorDetailed(prefixMsg string, err error) (detailedErr error) {
	return errors.New(volume.GenerateMsgDetailed(prefixMsg, errSuffix(err)))
}

// GenerateError returns simple and detailed errors for volumes to attach
func (volume *VolumeToAttach) GenerateError(prefixMsg string, err error) (simpleErr, detailedErr error) {
	simpleMsg, detailedMsg := volume.GenerateMsg(prefixMsg, errSuffix(err))
	return errors.New(simpleMsg), errors.New(detailedMsg)
}

// String combines key fields of the volume for logging in text format.
func (volume *VolumeToAttach) String() string {
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return fmt.Sprintf("%s (UniqueName: %s) from node %s", volumeSpecName, volume.VolumeName, volume.NodeName)
}

// MarshalLog combines key fields of the volume for logging in a structured format.
func (volume *VolumeToAttach) MarshalLog() interface{} {
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return struct {
		VolumeName, UniqueName, NodeName string
	}{
		VolumeName: volumeSpecName,
		UniqueName: string(volume.VolumeName),
		NodeName:   string(volume.NodeName),
	}
}

var _ fmt.Stringer = &VolumeToAttach{}
var _ logr.Marshaler = &VolumeToAttach{}

// VolumeToMount represents a volume that should be attached to this node and
// mounted to the PodName.
type VolumeToMount struct {
	// VolumeName is the unique identifier for the volume that should be
	// mounted.
	VolumeName v1.UniqueVolumeName

	// PodName is the unique identifier for the pod that the volume should be
	// mounted to after it is attached.
	PodName volumetypes.UniquePodName

	// VolumeSpec is a volume spec containing the specification for the volume
	// that should be mounted. Used to create NewMounter. Used to generate
	// InnerVolumeSpecName.
	VolumeSpec *volume.Spec

	// outerVolumeSpecName is the podSpec.Volume[x].Name of the volume. If the
	// volume was referenced through a persistent volume claim, this contains
	// the podSpec.Volume[x].Name of the persistent volume claim.
	OuterVolumeSpecName string

	// Pod to mount the volume to. Used to create NewMounter.
	Pod *v1.Pod

	// PluginIsAttachable indicates that the plugin for this volume implements
	// the volume.Attacher interface
	PluginIsAttachable bool

	// PluginIsDeviceMountable indicates that the plugin for this volume implements
	// the volume.DeviceMounter interface
	PluginIsDeviceMountable bool

	// VolumeGIDValue contains the value of the GID annotation, if present.
	VolumeGIDValue string

	// DevicePath contains the path on the node where the volume is attached.
	// For non-attachable volumes this is empty.
	DevicePath string

	// ReportedInUse indicates that the volume was successfully added to the
	// VolumesInUse field in the node's status.
	ReportedInUse bool

	// DesiredSizeLimit indicates the desired upper bound on the size of the volume
	// (if so implemented)
	DesiredSizeLimit *resource.Quantity

	// time at which volume was requested to be mounted
	MountRequestTime time.Time

	// DesiredPersistentVolumeSize stores desired size of the volume.
	// usually this is the size if pv.Spec.Capacity
	DesiredPersistentVolumeSize resource.Quantity

	// SELinux label that should be used to mount.
	// The label is set when:
	// * SELinuxMountReadWriteOncePod feature gate is enabled and the volume is RWOP and kubelet knows the SELinux label.
	// * Or, SELinuxMount feature gate is enabled and kubelet knows the SELinux label.
	SELinuxLabel string
}

// DeviceMountState represents device mount state in a global path.
type DeviceMountState string

const (
	// DeviceGloballyMounted means device has been globally mounted successfully
	DeviceGloballyMounted DeviceMountState = "DeviceGloballyMounted"

	// DeviceMountUncertain means device may not be mounted but a mount operation may be
	// in-progress which can cause device mount to succeed.
	DeviceMountUncertain DeviceMountState = "DeviceMountUncertain"

	// DeviceNotMounted means device has not been mounted globally.
	DeviceNotMounted DeviceMountState = "DeviceNotMounted"
)

// VolumeMountState represents volume mount state in a path local to the pod.
type VolumeMountState string

const (
	// VolumeMounted means volume has been mounted in pod's local path
	VolumeMounted VolumeMountState = "VolumeMounted"

	// VolumeMountUncertain means volume may or may not be mounted in pods' local path
	VolumeMountUncertain VolumeMountState = "VolumeMountUncertain"

	// VolumeNotMounted means volume has not be mounted in pod's local path
	VolumeNotMounted VolumeMountState = "VolumeNotMounted"
)

type MountPreConditionFailed struct {
	msg string
}

func (err *MountPreConditionFailed) Error() string {
	return err.msg
}

func NewMountPreConditionFailedError(msg string) *MountPreConditionFailed {
	return &MountPreConditionFailed{msg: msg}
}

func IsMountFailedPreconditionError(err error) bool {
	var failedPreconditionError *MountPreConditionFailed
	return errors.As(err, &failedPreconditionError)
}

// GenerateMsgDetailed returns detailed msgs for volumes to mount
func (volume *VolumeToMount) GenerateMsgDetailed(prefixMsg, suffixMsg string) (detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) pod %q (UID: %q)", volume.VolumeName, volume.Pod.Name, volume.Pod.UID)
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return generateVolumeMsgDetailed(prefixMsg, suffixMsg, volumeSpecName, detailedStr)
}

// GenerateMsg returns simple and detailed msgs for volumes to mount
func (volume *VolumeToMount) GenerateMsg(prefixMsg, suffixMsg string) (simpleMsg, detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) pod %q (UID: %q)", volume.VolumeName, volume.Pod.Name, volume.Pod.UID)
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return generateVolumeMsg(prefixMsg, suffixMsg, volumeSpecName, detailedStr)
}

// GenerateErrorDetailed returns detailed errors for volumes to mount
func (volume *VolumeToMount) GenerateErrorDetailed(prefixMsg string, err error) (detailedErr error) {
	return errors.New(volume.GenerateMsgDetailed(prefixMsg, errSuffix(err)))
}

// GenerateError returns simple and detailed errors for volumes to mount
func (volume *VolumeToMount) GenerateError(prefixMsg string, err error) (simpleErr, detailedErr error) {
	simpleMsg, detailedMsg := volume.GenerateMsg(prefixMsg, errSuffix(err))
	return errors.New(simpleMsg), errors.New(detailedMsg)
}

// AttachedVolume represents a volume that is attached to a node.
type AttachedVolume struct {
	// VolumeName is the unique identifier for the volume that is attached.
	VolumeName v1.UniqueVolumeName

	// VolumeSpec is the volume spec containing the specification for the
	// volume that is attached.
	VolumeSpec *volume.Spec

	// NodeName is the identifier for the node that the volume is attached to.
	NodeName types.NodeName

	// PluginIsAttachable indicates that the plugin for this volume implements
	// the volume.Attacher interface
	PluginIsAttachable bool

	// DevicePath contains the path on the node where the volume is attached.
	// For non-attachable volumes this is empty.
	DevicePath string

	// DeviceMountPath contains the path on the node where the device should
	// be mounted after it is attached.
	DeviceMountPath string

	// PluginName is the Unescaped Qualified name of the volume plugin used to
	// attach and mount this volume.
	PluginName string

	SELinuxMountContext string
}

// GenerateMsgDetailed returns detailed msgs for attached volumes
func (volume *AttachedVolume) GenerateMsgDetailed(prefixMsg, suffixMsg string) (detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) on node %q", volume.VolumeName, volume.NodeName)
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return generateVolumeMsgDetailed(prefixMsg, suffixMsg, volumeSpecName, detailedStr)
}

// GenerateMsg returns simple and detailed msgs for attached volumes
func (volume *AttachedVolume) GenerateMsg(prefixMsg, suffixMsg string) (simpleMsg, detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) on node %q", volume.VolumeName, volume.NodeName)
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return generateVolumeMsg(prefixMsg, suffixMsg, volumeSpecName, detailedStr)
}

// GenerateErrorDetailed returns detailed errors for attached volumes
func (volume *AttachedVolume) GenerateErrorDetailed(prefixMsg string, err error) (detailedErr error) {
	return errors.New(volume.GenerateMsgDetailed(prefixMsg, errSuffix(err)))
}

// GenerateError returns simple and detailed errors for attached volumes
func (volume *AttachedVolume) GenerateError(prefixMsg string, err error) (simpleErr, detailedErr error) {
	simpleMsg, detailedMsg := volume.GenerateMsg(prefixMsg, errSuffix(err))
	return errors.New(simpleMsg), errors.New(detailedMsg)
}

// String combines key fields of the volume for logging in text format.
func (volume *AttachedVolume) String() string {
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return fmt.Sprintf("%s (UniqueName: %s) from node %s", volumeSpecName, volume.VolumeName, volume.NodeName)
}

// MarshalLog combines key fields of the volume for logging in a structured format.
func (volume *AttachedVolume) MarshalLog() interface{} {
	volumeSpecName := "nil"
	if volume.VolumeSpec != nil {
		volumeSpecName = volume.VolumeSpec.Name()
	}
	return struct {
		VolumeName, UniqueName, NodeName string
	}{
		VolumeName: volumeSpecName,
		UniqueName: string(volume.VolumeName),
		NodeName:   string(volume.NodeName),
	}
}

var _ fmt.Stringer = &AttachedVolume{}
var _ logr.Marshaler = &AttachedVolume{}

// MountedVolume represents a volume that has successfully been mounted to a pod.
type MountedVolume struct {
	// PodName is the unique identifier of the pod mounted to.
	PodName volumetypes.UniquePodName

	// VolumeName is the unique identifier of the volume mounted to the pod.
	VolumeName v1.UniqueVolumeName

	// InnerVolumeSpecName is the volume.Spec.Name() of the volume. If the
	// volume was referenced through a persistent volume claims, this contains
	// the name of the bound persistent volume object.
	// It is the name that plugins use in their pod mount path, i.e.
	// /var/lib/kubelet/pods/{podUID}/volumes/{escapeQualifiedPluginName}/{innerVolumeSpecName}/
	// PVC example,
	//   apiVersion: v1
	//   kind: PersistentVolume
	//   metadata:
	//     name: pv0003				<- InnerVolumeSpecName
	//   spec:
	//     capacity:
	//       storage: 5Gi
	//     accessModes:
	//       - ReadWriteOnce
	//     persistentVolumeReclaimPolicy: Recycle
	//     nfs:
	//       path: /tmp
	//       server: 172.17.0.2
	// Non-PVC example:
	//   apiVersion: v1
	//   kind: Pod
	//   metadata:
	//     name: test-pd
	//   spec:
	//     containers:
	//     - image: registry.k8s.io/test-webserver
	//     	 name: test-container
	//     	 volumeMounts:
	//     	 - mountPath: /test-pd
	//     	   name: test-volume
	//     volumes:
	//     - name: test-volume			<- InnerVolumeSpecName
	//     	 gcePersistentDisk:
	//     	   pdName: my-data-disk
	//     	   fsType: ext4
	InnerVolumeSpecName string

	// outerVolumeSpecName is the podSpec.Volume[x].Name of the volume. If the
	// volume was referenced through a persistent volume claim, this contains
	// the podSpec.Volume[x].Name of the persistent volume claim.
	// PVC example:
	//   kind: Pod
	//   apiVersion: v1
	//   metadata:
	//     name: mypod
	//   spec:
	//     containers:
	//       - name: myfrontend
	//         image: dockerfile/nginx
	//         volumeMounts:
	//         - mountPath: "/var/www/html"
	//           name: mypd
	//     volumes:
	//       - name: mypd				<- OuterVolumeSpecName
	//         persistentVolumeClaim:
	//           claimName: myclaim
	// Non-PVC example:
	//   apiVersion: v1
	//   kind: Pod
	//   metadata:
	//     name: test-pd
	//   spec:
	//     containers:
	//     - image: registry.k8s.io/test-webserver
	//     	 name: test-container
	//     	 volumeMounts:
	//     	 - mountPath: /test-pd
	//     	   name: test-volume
	//     volumes:
	//     - name: test-volume			<- OuterVolumeSpecName
	//     	 gcePersistentDisk:
	//     	   pdName: my-data-disk
	//     	   fsType: ext4
	OuterVolumeSpecName string

	// PluginName is the "Unescaped Qualified" name of the volume plugin used to
	// mount and unmount this volume. It can be used to fetch the volume plugin
	// to unmount with, on demand. It is also the name that plugins use, though
	// escaped, in their pod mount path, i.e.
	// /var/lib/kubelet/pods/{podUID}/volumes/{escapeQualifiedPluginName}/{outerVolumeSpecName}/
	PluginName string

	// PodUID is the UID of the pod mounted to. It is also the string used by
	// plugins in their pod mount path, i.e.
	// /var/lib/kubelet/pods/{podUID}/volumes/{escapeQualifiedPluginName}/{outerVolumeSpecName}/
	PodUID types.UID

	// Mounter is the volume mounter used to mount this volume. It is required
	// by kubelet to create container.VolumeMap.
	// Mounter is only required for file system volumes and not required for block volumes.
	Mounter volume.Mounter

	// BlockVolumeMapper is the volume mapper used to map this volume. It is required
	// by kubelet to create container.VolumeMap.
	// BlockVolumeMapper is only required for block volumes and not required for file system volumes.
	BlockVolumeMapper volume.BlockVolumeMapper

	// VolumeGIDValue contains the value of the GID annotation, if present.
	VolumeGIDValue string

	// VolumeSpec is a volume spec containing the specification for the volume
	// that should be mounted.
	VolumeSpec *volume.Spec

	// DeviceMountPath contains the path on the node where the device should
	// be mounted after it is attached.
	DeviceMountPath string

	// SELinuxMountContext is value of mount option 'mount -o context=XYZ'.
	// If empty, no such mount option was used.
	SELinuxMountContext string
}

// GenerateMsgDetailed returns detailed msgs for mounted volumes
func (volume *MountedVolume) GenerateMsgDetailed(prefixMsg, suffixMsg string) (detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) pod %q (UID: %q)", volume.VolumeName, volume.PodName, volume.PodUID)
	return generateVolumeMsgDetailed(prefixMsg, suffixMsg, volume.OuterVolumeSpecName, detailedStr)
}

// GenerateMsg returns simple and detailed msgs for mounted volumes
func (volume *MountedVolume) GenerateMsg(prefixMsg, suffixMsg string) (simpleMsg, detailedMsg string) {
	detailedStr := fmt.Sprintf("(UniqueName: %q) pod %q (UID: %q)", volume.VolumeName, volume.PodName, volume.PodUID)
	return generateVolumeMsg(prefixMsg, suffixMsg, volume.OuterVolumeSpecName, detailedStr)
}

// GenerateErrorDetailed returns simple and detailed errors for mounted volumes
func (volume *MountedVolume) GenerateErrorDetailed(prefixMsg string, err error) (detailedErr error) {
	return errors.New(volume.GenerateMsgDetailed(prefixMsg, errSuffix(err)))
}

// GenerateError returns simple and detailed errors for mounted volumes
func (volume *MountedVolume) GenerateError(prefixMsg string, err error) (simpleErr, detailedErr error) {
	simpleMsg, detailedMsg := volume.GenerateMsg(prefixMsg, errSuffix(err))
	return errors.New(simpleMsg), errors.New(detailedMsg)
}

type operationExecutor struct {
	// pendingOperations keeps track of pending attach and detach operations so
	// multiple operations are not started on the same volume
	pendingOperations nestedpendingoperations.NestedPendingOperations

	// operationGenerator is an interface that provides implementations for
	// generating volume function
	operationGenerator OperationGenerator
}

func (oe *operationExecutor) IsOperationPending(
	volumeName v1.UniqueVolumeName,
	podName volumetypes.UniquePodName,
	nodeName types.NodeName) bool {
	return oe.pendingOperations.IsOperationPending(volumeName, podName, nodeName)
}

func (oe *operationExecutor) IsOperationSafeToRetry(
	volumeName v1.UniqueVolumeName,
	podName volumetypes.UniquePodName,
	nodeName types.NodeName,
	operationName string) bool {
	return oe.pendingOperations.IsOperationSafeToRetry(volumeName, podName, nodeName, operationName)
}

func (oe *operationExecutor) AttachVolume(
	logger klog.Logger,
	volumeToAttach VolumeToAttach,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) error {
	generatedOperations :=
		oe.operationGenerator.GenerateAttachVolumeFunc(logger, volumeToAttach, actualStateOfWorld)

	if util.IsMultiAttachAllowed(volumeToAttach.VolumeSpec) {
		return oe.pendingOperations.Run(
			volumeToAttach.VolumeName, "" /* podName */, volumeToAttach.NodeName, generatedOperations)
	}

	return oe.pendingOperations.Run(
		volumeToAttach.VolumeName, "" /* podName */, "" /* nodeName */, generatedOperations)
}

func (oe *operationExecutor) DetachVolume(
	logger klog.Logger,
	volumeToDetach AttachedVolume,
	verifySafeToDetach bool,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) error {
	generatedOperations, err :=
		oe.operationGenerator.GenerateDetachVolumeFunc(logger, volumeToDetach, verifySafeToDetach, actualStateOfWorld)
	if err != nil {
		return err
	}

	if util.IsMultiAttachAllowed(volumeToDetach.VolumeSpec) {
		return oe.pendingOperations.Run(
			volumeToDetach.VolumeName, "" /* podName */, volumeToDetach.NodeName, generatedOperations)
	}
	return oe.pendingOperations.Run(
		volumeToDetach.VolumeName, "" /* podName */, "" /* nodeName */, generatedOperations)

}

func (oe *operationExecutor) VerifyVolumesAreAttached(
	attachedVolumes map[types.NodeName][]AttachedVolume,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) {

	for node, nodeAttachedVolumes := range attachedVolumes {
		nodeError := oe.VerifyVolumesAreAttachedPerNode(nodeAttachedVolumes, node, actualStateOfWorld)
		if nodeError != nil {
			klog.Errorf("VerifyVolumesAreAttached failed for volumes %v, node %q with error %v", nodeAttachedVolumes, node, nodeError)
		}
	}
}

func (oe *operationExecutor) VerifyVolumesAreAttachedPerNode(
	attachedVolumes []AttachedVolume,
	nodeName types.NodeName,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) error {
	generatedOperations, err :=
		oe.operationGenerator.GenerateVolumesAreAttachedFunc(attachedVolumes, nodeName, actualStateOfWorld)
	if err != nil {
		return err
	}

	// Give an empty UniqueVolumeName so that this operation could be executed concurrently.
	return oe.pendingOperations.Run("" /* volumeName */, "" /* podName */, "" /* nodeName */, generatedOperations)
}

func (oe *operationExecutor) MountVolume(
	waitForAttachTimeout time.Duration,
	volumeToMount VolumeToMount,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	isRemount bool) error {
	fsVolume, err := util.CheckVolumeModeFilesystem(volumeToMount.VolumeSpec)
	if err != nil {
		return err
	}
	var generatedOperations volumetypes.GeneratedOperations
	if fsVolume {
		// Filesystem volume case
		// Mount/remount a volume when a volume is attached
		generatedOperations = oe.operationGenerator.GenerateMountVolumeFunc(
			waitForAttachTimeout, volumeToMount, actualStateOfWorld, isRemount)

	} else {
		// Block volume case
		// Creates a map to device if a volume is attached
		generatedOperations, err = oe.operationGenerator.GenerateMapVolumeFunc(
			waitForAttachTimeout, volumeToMount, actualStateOfWorld)
	}
	if err != nil {
		return err
	}
	// Avoid executing mount/map from multiple pods referencing the
	// same volume in parallel
	podName := nestedpendingoperations.EmptyUniquePodName

	// TODO: remove this -- not necessary
	if !volumeToMount.PluginIsAttachable && !volumeToMount.PluginIsDeviceMountable {
		// volume plugins which are Non-attachable and Non-deviceMountable can execute mount for multiple pods
		// referencing the same volume in parallel
		podName = util.GetUniquePodName(volumeToMount.Pod)
	}

	// TODO mount_device
	return oe.pendingOperations.Run(
		volumeToMount.VolumeName, podName, "" /* nodeName */, generatedOperations)
}

func (oe *operationExecutor) UnmountVolume(
	volumeToUnmount MountedVolume,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	podsDir string) error {
	fsVolume, err := util.CheckVolumeModeFilesystem(volumeToUnmount.VolumeSpec)
	if err != nil {
		return err
	}
	var generatedOperations volumetypes.GeneratedOperations
	if fsVolume {
		// Filesystem volume case
		// Unmount a volume if a volume is mounted
		generatedOperations, err = oe.operationGenerator.GenerateUnmountVolumeFunc(
			volumeToUnmount, actualStateOfWorld, podsDir)
	} else {
		// Block volume case
		// Unmap a volume if a volume is mapped
		generatedOperations, err = oe.operationGenerator.GenerateUnmapVolumeFunc(
			volumeToUnmount, actualStateOfWorld)
	}
	if err != nil {
		return err
	}
	// All volume plugins can execute unmount/unmap for multiple pods referencing the
	// same volume in parallel
	podName := volumetypes.UniquePodName(volumeToUnmount.PodUID)

	return oe.pendingOperations.Run(
		volumeToUnmount.VolumeName, podName, "" /* nodeName */, generatedOperations)
}

func (oe *operationExecutor) UnmountDevice(
	deviceToDetach AttachedVolume,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	hostutil hostutil.HostUtils) error {
	fsVolume, err := util.CheckVolumeModeFilesystem(deviceToDetach.VolumeSpec)
	if err != nil {
		return err
	}
	var generatedOperations volumetypes.GeneratedOperations
	if fsVolume {
		// Filesystem volume case
		// Unmount and detach a device if a volume isn't referenced
		generatedOperations, err = oe.operationGenerator.GenerateUnmountDeviceFunc(
			deviceToDetach, actualStateOfWorld, hostutil)
	} else {
		// Block volume case
		// Detach a device and remove loopback if a volume isn't referenced
		generatedOperations, err = oe.operationGenerator.GenerateUnmapDeviceFunc(
			deviceToDetach, actualStateOfWorld, hostutil)
	}
	if err != nil {
		return err
	}
	// Avoid executing unmount/unmap device from multiple pods referencing
	// the same volume in parallel
	podName := nestedpendingoperations.EmptyUniquePodName

	return oe.pendingOperations.Run(
		deviceToDetach.VolumeName, podName, "" /* nodeName */, generatedOperations)
}

func (oe *operationExecutor) ExpandInUseVolume(volumeToMount VolumeToMount, actualStateOfWorld ActualStateOfWorldMounterUpdater, currentSize resource.Quantity) error {
	generatedOperations, err := oe.operationGenerator.GenerateExpandInUseVolumeFunc(volumeToMount, actualStateOfWorld, currentSize)
	if err != nil {
		return err
	}
	return oe.pendingOperations.Run(volumeToMount.VolumeName, "", "" /* nodeName */, generatedOperations)
}

func (oe *operationExecutor) VerifyControllerAttachedVolume(
	logger klog.Logger,
	volumeToMount VolumeToMount,
	nodeName types.NodeName,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) error {
	generatedOperations, err :=
		oe.operationGenerator.GenerateVerifyControllerAttachedVolumeFunc(logger, volumeToMount, nodeName, actualStateOfWorld)
	if err != nil {
		return err
	}

	return oe.pendingOperations.Run(
		volumeToMount.VolumeName, "" /* podName */, "" /* nodeName */, generatedOperations)
}

// ReconstructVolumeOperation return a func to create volumeSpec from mount path
func (oe *operationExecutor) ReconstructVolumeOperation(
	volumeMode v1.PersistentVolumeMode,
	plugin volume.VolumePlugin,
	mapperPlugin volume.BlockVolumePlugin,
	uid types.UID,
	podName volumetypes.UniquePodName,
	volumeSpecName string,
	volumePath string,
	pluginName string) (volume.ReconstructedVolume, error) {

	// Filesystem Volume case
	if volumeMode == v1.PersistentVolumeFilesystem {
		// Create volumeSpec from mount path
		klog.V(5).Infof("Starting operationExecutor.ReconstructVolume for file volume on pod %q", podName)
		reconstructed, err := plugin.ConstructVolumeSpec(volumeSpecName, volumePath)
		if err != nil {
			return volume.ReconstructedVolume{}, err
		}
		return reconstructed, nil
	}

	// Block Volume case
	// Create volumeSpec from mount path
	klog.V(5).Infof("Starting operationExecutor.ReconstructVolume for block volume on pod %q", podName)

	// volumePath contains volumeName on the path. In the case of block volume, {volumeName} is symbolic link
	// corresponding to raw block device.
	// ex. volumePath: pods/{podUid}}/{DefaultKubeletVolumeDevicesDirName}/{escapeQualifiedPluginName}/{volumeName}
	volumeSpec, err := mapperPlugin.ConstructBlockVolumeSpec(uid, volumeSpecName, volumePath)
	if err != nil {
		return volume.ReconstructedVolume{}, err
	}
	return volume.ReconstructedVolume{
		Spec: volumeSpec,
	}, nil
}

func GetPVCToAttach(v VolumeToAttach, pvcLister corelisters.PersistentVolumeClaimLister, kubeClient clientset.Interface) runtime.Object {
	var pvc *v1.PersistentVolumeClaim
	if v.VolumeSpec == nil {
		return nil
	}
	if v.VolumeSpec.PersistentVolume == nil {
		return nil
	}
	pv := v.VolumeSpec.PersistentVolume
	if pv.Spec.ClaimRef == nil {
		return nil
	}
	var err error
	if pvcLister != nil {
		pvc, err = pvcLister.PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).Get(pv.Spec.ClaimRef.Name)
	} else if kubeClient != nil {
		pvc, err = kubeClient.CoreV1().PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).Get(context.TODO(), pv.Spec.ClaimRef.Name, metav1.GetOptions{})
	} else {
		return nil
	}
	if err != nil {
		return nil
	}
	return pvc
}
func GetPVCToMount(v VolumeToMount, pvcLister corelisters.PersistentVolumeClaimLister, kubeClient clientset.Interface) runtime.Object {
	var pvc *v1.PersistentVolumeClaim
	if v.VolumeSpec == nil {
		return nil
	}
	if v.VolumeSpec.PersistentVolume == nil {
		return nil
	}
	pv := v.VolumeSpec.PersistentVolume
	if pv.Spec.ClaimRef == nil {
		return nil
	}
	var err error
	if pvcLister != nil {
		pvc, err = pvcLister.PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).Get(pv.Spec.ClaimRef.Name)
	} else if kubeClient != nil {
		pvc, err = kubeClient.CoreV1().PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).Get(context.TODO(), pv.Spec.ClaimRef.Name, metav1.GetOptions{})
	} else {
		return nil
	}
	if err != nil {
		return nil
	}
	return pvc
}
