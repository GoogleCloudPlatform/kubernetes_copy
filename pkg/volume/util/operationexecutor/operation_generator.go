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

package operationexecutor

import (
	"context"
	goerrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	volerr "k8s.io/cloud-provider/volume/errors"
	storagehelpers "k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/features"
	kevents "k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
)

const (
	unknownVolumePlugin                  string = "UnknownVolumePlugin"
	unknownAttachableVolumePlugin        string = "UnknownAttachableVolumePlugin"
	DetachOperationName                  string = "volume_detach"
	VerifyControllerAttachedVolumeOpName string = "verify_controller_attached_volume"
)

var _ OperationGenerator = &operationGenerator{}

type operationGenerator struct {
	// Used to fetch objects from the API server like Node in the
	// VerifyControllerAttachedVolume operation.
	kubeClient clientset.Interface

	// volumePluginMgr is the volume plugin manager used to create volume
	// plugin objects.
	volumePluginMgr *volume.VolumePluginMgr

	// recorder is used to record events in the API server
	recorder record.EventRecorder

	// blkUtil provides volume path related operations for block volume
	blkUtil volumepathhandler.BlockVolumePathHandler
}

type inTreeResizeResponse struct {
	pvc *v1.PersistentVolumeClaim
	pv  *v1.PersistentVolume

	err error
	// indicates that resize operation was called on underlying volume driver
	// mainly useful for testing.
	resizeCalled bool
}

// NewOperationGenerator is returns instance of operationGenerator
func NewOperationGenerator(kubeClient clientset.Interface,
	volumePluginMgr *volume.VolumePluginMgr,
	recorder record.EventRecorder,
	blkUtil volumepathhandler.BlockVolumePathHandler) OperationGenerator {

	return &operationGenerator{
		kubeClient:      kubeClient,
		volumePluginMgr: volumePluginMgr,
		recorder:        recorder,
		blkUtil:         blkUtil,
	}
}

// OperationGenerator interface that extracts out the functions from operation_executor to make it dependency injectable
type OperationGenerator interface {
	// Generates the MountVolume function needed to perform the mount of a volume plugin
	GenerateMountVolumeFunc(waitForAttachTimeout time.Duration, volumeToMount VolumeToMount, actualStateOfWorldMounterUpdater ActualStateOfWorldMounterUpdater, isRemount bool) volumetypes.GeneratedOperations

	// Generates the UnmountVolume function needed to perform the unmount of a volume plugin
	GenerateUnmountVolumeFunc(volumeToUnmount MountedVolume, actualStateOfWorld ActualStateOfWorldMounterUpdater, podsDir string) (volumetypes.GeneratedOperations, error)

	// Generates the AttachVolume function needed to perform attach of a volume plugin
	GenerateAttachVolumeFunc(logger klog.Logger, volumeToAttach VolumeToAttach, actualStateOfWorld ActualStateOfWorldAttacherUpdater) volumetypes.GeneratedOperations

	// Generates the DetachVolume function needed to perform the detach of a volume plugin
	GenerateDetachVolumeFunc(logger klog.Logger, volumeToDetach AttachedVolume, verifySafeToDetach bool, actualStateOfWorld ActualStateOfWorldAttacherUpdater) (volumetypes.GeneratedOperations, error)

	// Generates the VolumesAreAttached function needed to verify if volume plugins are attached
	GenerateVolumesAreAttachedFunc(attachedVolumes []AttachedVolume, nodeName types.NodeName, actualStateOfWorld ActualStateOfWorldAttacherUpdater) (volumetypes.GeneratedOperations, error)

	// Generates the UnMountDevice function needed to perform the unmount of a device
	GenerateUnmountDeviceFunc(deviceToDetach AttachedVolume, actualStateOfWorld ActualStateOfWorldMounterUpdater, mounter hostutil.HostUtils) (volumetypes.GeneratedOperations, error)

	// Generates the function needed to check if the attach_detach controller has attached the volume plugin
	GenerateVerifyControllerAttachedVolumeFunc(logger klog.Logger, volumeToMount VolumeToMount, nodeName types.NodeName, actualStateOfWorld ActualStateOfWorldAttacherUpdater) (volumetypes.GeneratedOperations, error)

	// Generates the MapVolume function needed to perform the map of a volume plugin
	GenerateMapVolumeFunc(waitForAttachTimeout time.Duration, volumeToMount VolumeToMount, actualStateOfWorldMounterUpdater ActualStateOfWorldMounterUpdater) (volumetypes.GeneratedOperations, error)

	// Generates the UnmapVolume function needed to perform the unmap of a volume plugin
	GenerateUnmapVolumeFunc(volumeToUnmount MountedVolume, actualStateOfWorld ActualStateOfWorldMounterUpdater) (volumetypes.GeneratedOperations, error)

	// Generates the UnmapDevice function needed to perform the unmap of a device
	GenerateUnmapDeviceFunc(deviceToDetach AttachedVolume, actualStateOfWorld ActualStateOfWorldMounterUpdater, mounter hostutil.HostUtils) (volumetypes.GeneratedOperations, error)

	// GetVolumePluginMgr returns volume plugin manager
	GetVolumePluginMgr() *volume.VolumePluginMgr

	GenerateExpandVolumeFunc(*v1.PersistentVolumeClaim, *v1.PersistentVolume) (volumetypes.GeneratedOperations, error)

	GenerateExpandAndRecoverVolumeFunc(*v1.PersistentVolumeClaim, *v1.PersistentVolume, string) (volumetypes.GeneratedOperations, error)

	// Generates the volume file system resize function, which can resize volume's file system to expected size without unmounting the volume.
	// Along with volumeToMount and actualStateOfWorld, the function expects current size of volume on the node as an argument. The current
	// size here always refers to capacity last recorded in actualStateOfWorld from pvc.Status.Capacity
	GenerateExpandInUseVolumeFunc(volumeToMount VolumeToMount, actualStateOfWorld ActualStateOfWorldMounterUpdater, currentSize resource.Quantity) (volumetypes.GeneratedOperations, error)
}

type inTreeResizeOpts struct {
	resizerName  string
	pvc          *v1.PersistentVolumeClaim
	pv           *v1.PersistentVolume
	volumeSpec   *volume.Spec
	volumePlugin volume.ExpandableVolumePlugin
}

type nodeResizeOperationOpts struct {
	vmt                VolumeToMount
	pvc                *v1.PersistentVolumeClaim
	pv                 *v1.PersistentVolume
	pluginResizeOpts   volume.NodeResizeOptions
	volumePlugin       volume.NodeExpandableVolumePlugin
	actualStateOfWorld ActualStateOfWorldMounterUpdater
}

func (og *operationGenerator) GenerateVolumesAreAttachedFunc(
	attachedVolumes []AttachedVolume,
	nodeName types.NodeName,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) (volumetypes.GeneratedOperations, error) {
	// volumesPerPlugin maps from a volume plugin to a list of volume specs which belong
	// to this type of plugin
	volumesPerPlugin := make(map[string][]*volume.Spec)
	// volumeSpecMap maps from a volume spec to its unique volumeName which will be used
	// when calling MarkVolumeAsDetached
	volumeSpecMap := make(map[*volume.Spec]v1.UniqueVolumeName)

	// Iterate each volume spec and put them into a map index by the pluginName
	for _, volumeAttached := range attachedVolumes {
		if volumeAttached.VolumeSpec == nil {
			klog.Errorf("VerifyVolumesAreAttached.GenerateVolumesAreAttachedFunc: nil spec for volume %s", volumeAttached.VolumeName)
			continue
		}
		volumePlugin, err :=
			og.volumePluginMgr.FindPluginBySpec(volumeAttached.VolumeSpec)
		if err != nil || volumePlugin == nil {
			klog.Error(volumeAttached.GenerateErrorDetailed("VolumesAreAttached.FindPluginBySpec failed", err).Error())
			continue
		}
		volumeSpecList, pluginExists := volumesPerPlugin[volumePlugin.GetPluginName()]
		if !pluginExists {
			volumeSpecList = []*volume.Spec{}
		}
		volumeSpecList = append(volumeSpecList, volumeAttached.VolumeSpec)
		volumesPerPlugin[volumePlugin.GetPluginName()] = volumeSpecList
		// Migration: VolumeSpecMap contains original VolumeName for use in ActualStateOfWorld
		volumeSpecMap[volumeAttached.VolumeSpec] = volumeAttached.VolumeName
	}

	volumesAreAttachedFunc := func() volumetypes.OperationContext {

		// For each volume plugin, pass the list of volume specs to VolumesAreAttached to check
		// whether the volumes are still attached.
		for pluginName, volumesSpecs := range volumesPerPlugin {
			attachableVolumePlugin, err :=
				og.volumePluginMgr.FindAttachablePluginByName(pluginName)
			if err != nil || attachableVolumePlugin == nil {
				klog.Errorf(
					"VolumeAreAttached.FindAttachablePluginBySpec failed for plugin %q with: %v",
					pluginName,
					err)
				continue
			}

			volumeAttacher, newAttacherErr := attachableVolumePlugin.NewAttacher()
			if newAttacherErr != nil {
				klog.Errorf(
					"VolumesAreAttached.NewAttacher failed for getting plugin %q with: %v",
					pluginName,
					newAttacherErr)
				continue
			}

			attached, areAttachedErr := volumeAttacher.VolumesAreAttached(volumesSpecs, nodeName)
			if areAttachedErr != nil {
				klog.Errorf(
					"VolumesAreAttached failed for checking on node %q with: %v",
					nodeName,
					areAttachedErr)
				continue
			}

			for spec, check := range attached {
				if !check {
					actualStateOfWorld.MarkVolumeAsDetached(volumeSpecMap[spec], nodeName)
					klog.V(1).Infof("VerifyVolumesAreAttached determined volume %q (spec.Name: %q) is no longer attached to node %q, therefore it was marked as detached.",
						volumeSpecMap[spec], spec.Name(), nodeName)
				}
			}
		}

		// It is hard to differentiate migrated status for all volumes for verify_volumes_are_attached_per_node
		return volumetypes.NewOperationContext(nil, nil, false)
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "verify_volumes_are_attached_per_node",
		OperationFunc:     volumesAreAttachedFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume("<n/a>", nil), "verify_volumes_are_attached_per_node"),
		EventRecorderFunc: nil, // nil because we do not want to generate event on error
	}, nil
}

func (og *operationGenerator) GenerateAttachVolumeFunc(
	logger klog.Logger,
	volumeToAttach VolumeToAttach,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) volumetypes.GeneratedOperations {

	attachVolumeFunc := func() volumetypes.OperationContext {
		attachableVolumePlugin, err :=
			og.volumePluginMgr.FindAttachablePluginBySpec(volumeToAttach.VolumeSpec)

		migrated := getMigratedStatusBySpec(volumeToAttach.VolumeSpec)

		if err != nil || attachableVolumePlugin == nil {
			eventErr, detailedErr := volumeToAttach.GenerateError("AttachVolume.FindAttachablePluginBySpec failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		volumeAttacher, newAttacherErr := attachableVolumePlugin.NewAttacher()
		if newAttacherErr != nil {
			eventErr, detailedErr := volumeToAttach.GenerateError("AttachVolume.NewAttacher failed", newAttacherErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Execute attach
		devicePath, attachErr := volumeAttacher.Attach(
			volumeToAttach.VolumeSpec, volumeToAttach.NodeName)

		if attachErr != nil {
			uncertainNode := volumeToAttach.NodeName
			if derr, ok := attachErr.(*volerr.DanglingAttachError); ok {
				uncertainNode = derr.CurrentNode
			}
			addErr := actualStateOfWorld.MarkVolumeAsUncertain(
				logger,
				volumeToAttach.VolumeName,
				volumeToAttach.VolumeSpec,
				uncertainNode)
			if addErr != nil {
				klog.Errorf("AttachVolume.MarkVolumeAsUncertain fail to add the volume %q to actual state with %s", volumeToAttach.VolumeName, addErr)
			}

			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToAttach.GenerateError("AttachVolume.Attach failed", attachErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Update actual state of world
		addVolumeNodeErr := actualStateOfWorld.MarkVolumeAsAttached(
			logger, v1.UniqueVolumeName(""), volumeToAttach.VolumeSpec, volumeToAttach.NodeName, devicePath)
		if addVolumeNodeErr != nil {
			markErr := actualStateOfWorld.MarkVolumeAsUncertain(
				logger,
				volumeToAttach.VolumeName,
				volumeToAttach.VolumeSpec,
				volumeToAttach.NodeName)
			if markErr != nil {
				klog.Errorf("AttachVolume.MarkVolumeAsUncertain failed to add the volume %q to actual state after attach error: %s", volumeToAttach.VolumeName, markErr)
			}
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToAttach.GenerateError("AttachVolume.MarkVolumeAsAttached failed", addVolumeNodeErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Successful attach event is useful for user debugging
		simpleMsg, _ := volumeToAttach.GenerateMsg("AttachVolume.Attach succeeded", "")
		for _, pod := range volumeToAttach.ScheduledPods {
			og.recorder.Eventf(pod, v1.EventTypeNormal, kevents.SuccessfulAttachVolume, simpleMsg)
		}
		klog.Info(volumeToAttach.GenerateMsgDetailed("AttachVolume.Attach succeeded", ""))

		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	eventRecorderFunc := func(err *error) {
		if *err != nil {
			for _, pod := range volumeToAttach.ScheduledPods {
				og.recorder.Eventf(pod, v1.EventTypeWarning, kevents.FailedAttachVolume, (*err).Error())
			}
		}
	}

	attachableVolumePluginName := unknownAttachableVolumePlugin

	// Get attacher plugin
	attachableVolumePlugin, err :=
		og.volumePluginMgr.FindAttachablePluginBySpec(volumeToAttach.VolumeSpec)
	// It's ok to ignore the error, returning error is not expected from this function.
	// If an error case occurred during the function generation, this error case(skipped one) will also trigger an error
	// while the generated function is executed. And those errors will be handled during the execution of the generated
	// function with a back off policy.
	if err == nil && attachableVolumePlugin != nil {
		attachableVolumePluginName = attachableVolumePlugin.GetPluginName()
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "volume_attach",
		OperationFunc:     attachVolumeFunc,
		EventRecorderFunc: eventRecorderFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(attachableVolumePluginName, volumeToAttach.VolumeSpec), "volume_attach"),
	}
}

func (og *operationGenerator) GetVolumePluginMgr() *volume.VolumePluginMgr {
	return og.volumePluginMgr
}

func (og *operationGenerator) GenerateDetachVolumeFunc(
	logger klog.Logger,
	volumeToDetach AttachedVolume,
	verifySafeToDetach bool,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) (volumetypes.GeneratedOperations, error) {
	var volumeName string
	var attachableVolumePlugin volume.AttachableVolumePlugin
	var pluginName string
	var err error

	if volumeToDetach.VolumeSpec != nil {
		attachableVolumePlugin, err = findDetachablePluginBySpec(volumeToDetach.VolumeSpec, og.volumePluginMgr)
		if err != nil || attachableVolumePlugin == nil {
			return volumetypes.GeneratedOperations{}, volumeToDetach.GenerateErrorDetailed("DetachVolume.findDetachablePluginBySpec failed", err)
		}

		volumeName, err =
			attachableVolumePlugin.GetVolumeName(volumeToDetach.VolumeSpec)
		if err != nil {
			return volumetypes.GeneratedOperations{}, volumeToDetach.GenerateErrorDetailed("DetachVolume.GetVolumeName failed", err)
		}
	} else {
		// Get attacher plugin and the volumeName by splitting the volume unique name in case
		// there's no VolumeSpec: this happens only on attach/detach controller crash recovery
		// when a pod has been deleted during the controller downtime
		pluginName, volumeName, err = util.SplitUniqueName(volumeToDetach.VolumeName)
		if err != nil {
			return volumetypes.GeneratedOperations{}, volumeToDetach.GenerateErrorDetailed("DetachVolume.SplitUniqueName failed", err)
		}

		attachableVolumePlugin, err = og.volumePluginMgr.FindAttachablePluginByName(pluginName)
		if err != nil || attachableVolumePlugin == nil {
			return volumetypes.GeneratedOperations{}, volumeToDetach.GenerateErrorDetailed("DetachVolume.FindAttachablePluginByName failed", err)
		}

	}

	if pluginName == "" {
		pluginName = attachableVolumePlugin.GetPluginName()
	}

	volumeDetacher, err := attachableVolumePlugin.NewDetacher()
	if err != nil {
		return volumetypes.GeneratedOperations{}, volumeToDetach.GenerateErrorDetailed("DetachVolume.NewDetacher failed", err)
	}

	detachVolumeFunc := func() volumetypes.OperationContext {
		var err error
		if verifySafeToDetach {
			err = og.verifyVolumeIsSafeToDetach(volumeToDetach)
		}
		if err == nil {
			err = volumeDetacher.Detach(volumeName, volumeToDetach.NodeName)
		}

		migrated := getMigratedStatusBySpec(volumeToDetach.VolumeSpec)

		if err != nil {
			// On failure, mark the volume as uncertain. Attach() must succeed before adding the volume back
			// to node status as attached.
			uncertainError := actualStateOfWorld.MarkVolumeAsUncertain(
				logger, volumeToDetach.VolumeName, volumeToDetach.VolumeSpec, volumeToDetach.NodeName)
			if uncertainError != nil {
				klog.Errorf("DetachVolume.MarkVolumeAsUncertain failed to add the volume %q to actual state after detach error: %s", volumeToDetach.VolumeName, uncertainError)
			}
			eventErr, detailedErr := volumeToDetach.GenerateError("DetachVolume.Detach failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		klog.Info(volumeToDetach.GenerateMsgDetailed("DetachVolume.Detach succeeded", ""))

		// Update actual state of world
		actualStateOfWorld.MarkVolumeAsDetached(
			volumeToDetach.VolumeName, volumeToDetach.NodeName)

		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	return volumetypes.GeneratedOperations{
		OperationName:     DetachOperationName,
		OperationFunc:     detachVolumeFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(pluginName, volumeToDetach.VolumeSpec), DetachOperationName),
		EventRecorderFunc: nil, // nil because we do not want to generate event on error
	}, nil
}

func (og *operationGenerator) GenerateMountVolumeFunc(
	waitForAttachTimeout time.Duration,
	volumeToMount VolumeToMount,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	isRemount bool) volumetypes.GeneratedOperations {

	volumePluginName := unknownVolumePlugin
	volumePlugin, err :=
		og.volumePluginMgr.FindPluginBySpec(volumeToMount.VolumeSpec)
	if err == nil && volumePlugin != nil {
		volumePluginName = volumePlugin.GetPluginName()
	}

	mountVolumeFunc := func() volumetypes.OperationContext {
		// Get mounter plugin
		volumePlugin, err := og.volumePluginMgr.FindPluginBySpec(volumeToMount.VolumeSpec)

		migrated := getMigratedStatusBySpec(volumeToMount.VolumeSpec)

		if err != nil || volumePlugin == nil {
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.FindPluginBySpec failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		affinityErr := checkNodeAffinity(og, volumeToMount)
		if affinityErr != nil {
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.NodeAffinity check failed", affinityErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		volumeMounter, newMounterErr := volumePlugin.NewMounter(
			volumeToMount.VolumeSpec,
			volumeToMount.Pod)
		if newMounterErr != nil {
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.NewMounter initialization failed", newMounterErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		mountCheckError := checkMountOptionSupport(og, volumeToMount, volumePlugin)
		if mountCheckError != nil {
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.MountOptionSupport check failed", mountCheckError)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Enforce ReadWriteOncePod access mode if it is the only one present. This is also enforced during scheduling.
		if actualStateOfWorld.IsVolumeMountedElsewhere(volumeToMount.VolumeName, volumeToMount.PodName) &&
			// Because we do not know what access mode the pod intends to use if there are multiple.
			len(volumeToMount.VolumeSpec.PersistentVolume.Spec.AccessModes) == 1 &&
			v1helper.ContainsAccessMode(volumeToMount.VolumeSpec.PersistentVolume.Spec.AccessModes, v1.ReadWriteOncePod) {

			err = goerrors.New("volume uses the ReadWriteOncePod access mode and is already in use by another pod")
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.SetUp failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Get attacher, if possible
		attachableVolumePlugin, _ :=
			og.volumePluginMgr.FindAttachablePluginBySpec(volumeToMount.VolumeSpec)
		var volumeAttacher volume.Attacher
		if attachableVolumePlugin != nil {
			volumeAttacher, _ = attachableVolumePlugin.NewAttacher()
		}

		// get deviceMounter, if possible
		deviceMountableVolumePlugin, _ := og.volumePluginMgr.FindDeviceMountablePluginBySpec(volumeToMount.VolumeSpec)
		var volumeDeviceMounter volume.DeviceMounter
		if deviceMountableVolumePlugin != nil {
			volumeDeviceMounter, _ = deviceMountableVolumePlugin.NewDeviceMounter()
		}

		var fsGroup *int64
		var fsGroupChangePolicy *v1.PodFSGroupChangePolicy
		if podSc := volumeToMount.Pod.Spec.SecurityContext; podSc != nil {
			if podSc.FSGroup != nil {
				fsGroup = podSc.FSGroup
			}
			if podSc.FSGroupChangePolicy != nil {
				fsGroupChangePolicy = podSc.FSGroupChangePolicy
			}
		}

		devicePath := volumeToMount.DevicePath
		if volumeAttacher != nil {
			// Wait for attachable volumes to finish attaching
			klog.InfoS(volumeToMount.GenerateMsgDetailed("MountVolume.WaitForAttach entering", fmt.Sprintf("DevicePath %q", volumeToMount.DevicePath)), "pod", klog.KObj(volumeToMount.Pod))

			devicePath, err = volumeAttacher.WaitForAttach(
				volumeToMount.VolumeSpec, devicePath, volumeToMount.Pod, waitForAttachTimeout)
			if err != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.WaitForAttach failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			klog.InfoS(volumeToMount.GenerateMsgDetailed("MountVolume.WaitForAttach succeeded", fmt.Sprintf("DevicePath %q", devicePath)), "pod", klog.KObj(volumeToMount.Pod))
		}

		var resizeError error
		resizeOptions := volume.NodeResizeOptions{
			DevicePath: devicePath,
		}

		if volumeDeviceMounter != nil && actualStateOfWorld.GetDeviceMountState(volumeToMount.VolumeName) != DeviceGloballyMounted {
			deviceMountPath, err :=
				volumeDeviceMounter.GetDeviceMountPath(volumeToMount.VolumeSpec)
			if err != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.GetDeviceMountPath failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			// Mount device to global mount path
			err = volumeDeviceMounter.MountDevice(
				volumeToMount.VolumeSpec,
				devicePath,
				deviceMountPath,
				volume.DeviceMounterArgs{FsGroup: fsGroup, SELinuxLabel: volumeToMount.SELinuxLabel},
			)
			if err != nil {
				og.checkForFailedMount(volumeToMount, err)
				og.markDeviceErrorState(volumeToMount, devicePath, deviceMountPath, err, actualStateOfWorld)
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.MountDevice failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			klog.InfoS(volumeToMount.GenerateMsgDetailed("MountVolume.MountDevice succeeded", fmt.Sprintf("device mount path %q", deviceMountPath)), "pod", klog.KObj(volumeToMount.Pod))

			// Update actual state of world to reflect volume is globally mounted
			markDeviceMountedErr := actualStateOfWorld.MarkDeviceAsMounted(
				volumeToMount.VolumeName, devicePath, deviceMountPath, volumeToMount.SELinuxLabel)
			if markDeviceMountedErr != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.MarkDeviceAsMounted failed", markDeviceMountedErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
			// set staging path for volume expansion
			resizeOptions.DeviceStagePath = deviceMountPath
		}

		if volumeDeviceMounter != nil && resizeOptions.DeviceStagePath == "" {
			deviceStagePath, err := volumeDeviceMounter.GetDeviceMountPath(volumeToMount.VolumeSpec)
			if err != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.GetDeviceMountPath failed for expansion", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
			resizeOptions.DeviceStagePath = deviceStagePath
		}

		// Execute mount
		mountErr := volumeMounter.SetUp(volume.MounterArgs{
			FsUser:              util.FsUserFrom(volumeToMount.Pod),
			FsGroup:             fsGroup,
			DesiredSize:         volumeToMount.DesiredSizeLimit,
			FSGroupChangePolicy: fsGroupChangePolicy,
			SELinuxLabel:        volumeToMount.SELinuxLabel,
		})
		// Update actual state of world
		markOpts := MarkVolumeOpts{
			PodName:             volumeToMount.PodName,
			PodUID:              volumeToMount.Pod.UID,
			VolumeName:          volumeToMount.VolumeName,
			Mounter:             volumeMounter,
			OuterVolumeSpecName: volumeToMount.OuterVolumeSpecName,
			VolumeGIDVolume:     volumeToMount.VolumeGIDValue,
			VolumeSpec:          volumeToMount.VolumeSpec,
			VolumeMountState:    VolumeMounted,
			SELinuxMountContext: volumeToMount.SELinuxLabel,
		}
		if mountErr != nil {
			og.checkForFailedMount(volumeToMount, mountErr)
			og.markVolumeErrorState(volumeToMount, markOpts, mountErr, actualStateOfWorld)
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.SetUp failed", mountErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		detailedMsg := volumeToMount.GenerateMsgDetailed("MountVolume.SetUp succeeded", "")
		verbosity := klog.Level(1)
		if isRemount {
			verbosity = klog.Level(4)
		}
		klog.V(verbosity).InfoS(detailedMsg, "pod", klog.KObj(volumeToMount.Pod))
		resizeOptions.DeviceMountPath = volumeMounter.GetPath()

		_, resizeError = og.expandVolumeDuringMount(volumeToMount, actualStateOfWorld, resizeOptions)
		if resizeError != nil {
			klog.Errorf("MountVolume.NodeExpandVolume failed with %v", resizeError)
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.Setup failed while expanding volume", resizeError)
			// At this point, MountVolume.Setup already succeeded, we should add volume into actual state
			// so that reconciler can clean up volume when needed. However, volume resize failed,
			// we should not mark the volume as mounted to avoid pod starts using it.
			// Considering the above situations, we mark volume as uncertain here so that reconciler will trigger
			// volume tear down when pod is deleted, and also makes sure pod will not start using it.
			if err := actualStateOfWorld.MarkVolumeMountAsUncertain(markOpts); err != nil {
				klog.Error(volumeToMount.GenerateErrorDetailed("MountVolume.MarkVolumeMountAsUncertain failed", err).Error())
			}
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// record total time it takes to mount a volume. This is end to end time that includes waiting for volume to attach, node to be update
		// plugin call to succeed
		mountRequestTime := volumeToMount.MountRequestTime
		totalTimeTaken := time.Since(mountRequestTime).Seconds()
		util.RecordOperationLatencyMetric(util.GetFullQualifiedPluginNameForVolume(volumePluginName, volumeToMount.VolumeSpec), "overall_volume_mount", totalTimeTaken)

		markVolMountedErr := actualStateOfWorld.MarkVolumeAsMounted(markOpts)
		if markVolMountedErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToMount.GenerateError("MountVolume.MarkVolumeAsMounted failed", markVolMountedErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}
		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	eventRecorderFunc := func(err *error) {
		if *err != nil {
			og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeWarning, kevents.FailedMountVolume, (*err).Error())
		}
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "volume_mount",
		OperationFunc:     mountVolumeFunc,
		EventRecorderFunc: eventRecorderFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(volumePluginName, volumeToMount.VolumeSpec), "volume_mount"),
	}
}

func (og *operationGenerator) checkForFailedMount(volumeToMount VolumeToMount, mountError error) {
	pv := volumeToMount.VolumeSpec.PersistentVolume
	if pv == nil {
		return
	}

	if volumetypes.IsFilesystemMismatchError(mountError) {
		simpleMsg, _ := volumeToMount.GenerateMsg("MountVolume failed", mountError.Error())
		og.recorder.Eventf(pv, v1.EventTypeWarning, kevents.FailedMountOnFilesystemMismatch, simpleMsg)
	}
}

func (og *operationGenerator) markDeviceErrorState(volumeToMount VolumeToMount, devicePath, deviceMountPath string, mountError error, actualStateOfWorld ActualStateOfWorldMounterUpdater) {
	if volumetypes.IsOperationFinishedError(mountError) &&
		actualStateOfWorld.GetDeviceMountState(volumeToMount.VolumeName) == DeviceMountUncertain {

		if actualStateOfWorld.IsVolumeDeviceReconstructed(volumeToMount.VolumeName) {
			klog.V(2).InfoS("MountVolume.markDeviceErrorState leaving volume uncertain", "volumeName", volumeToMount.VolumeName)
			return
		}

		// Only devices which were uncertain can be marked as unmounted
		markDeviceUnmountError := actualStateOfWorld.MarkDeviceAsUnmounted(volumeToMount.VolumeName)
		if markDeviceUnmountError != nil {
			klog.Error(volumeToMount.GenerateErrorDetailed("MountDevice.MarkDeviceAsUnmounted failed", markDeviceUnmountError).Error())
		}
		return
	}

	if volumetypes.IsUncertainProgressError(mountError) &&
		actualStateOfWorld.GetDeviceMountState(volumeToMount.VolumeName) == DeviceNotMounted {
		// only devices which are not mounted can be marked as uncertain. We do not want to mark a device
		// which was previously marked as mounted here as uncertain.
		markDeviceUncertainError := actualStateOfWorld.MarkDeviceAsUncertain(volumeToMount.VolumeName, devicePath, deviceMountPath, volumeToMount.SELinuxLabel)
		if markDeviceUncertainError != nil {
			klog.Error(volumeToMount.GenerateErrorDetailed("MountDevice.MarkDeviceAsUncertain failed", markDeviceUncertainError).Error())
		}
	}

}

func (og *operationGenerator) markVolumeErrorState(volumeToMount VolumeToMount, markOpts MarkVolumeOpts, mountError error, actualStateOfWorld ActualStateOfWorldMounterUpdater) {
	if volumetypes.IsOperationFinishedError(mountError) &&
		actualStateOfWorld.GetVolumeMountState(volumeToMount.VolumeName, markOpts.PodName) == VolumeMountUncertain {
		// if volume was previously reconstructed we are not going to change its state as unmounted even
		// if mount operation fails.
		if actualStateOfWorld.IsVolumeReconstructed(volumeToMount.VolumeName, volumeToMount.PodName) {
			klog.V(3).InfoS("MountVolume.markVolumeErrorState leaving volume uncertain", "volumeName", volumeToMount.VolumeName)
			return
		}

		t := actualStateOfWorld.MarkVolumeAsUnmounted(volumeToMount.PodName, volumeToMount.VolumeName)
		if t != nil {
			klog.Error(volumeToMount.GenerateErrorDetailed("MountVolume.MarkVolumeAsUnmounted failed", t).Error())
		}
		return

	}

	if volumetypes.IsUncertainProgressError(mountError) &&
		actualStateOfWorld.GetVolumeMountState(volumeToMount.VolumeName, markOpts.PodName) == VolumeNotMounted {
		t := actualStateOfWorld.MarkVolumeMountAsUncertain(markOpts)
		if t != nil {
			klog.Error(volumeToMount.GenerateErrorDetailed("MountVolume.MarkVolumeMountAsUncertain failed", t).Error())
		}
	}
}

func (og *operationGenerator) GenerateUnmountVolumeFunc(
	volumeToUnmount MountedVolume,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	podsDir string) (volumetypes.GeneratedOperations, error) {
	// Get mountable plugin
	volumePlugin, err := og.volumePluginMgr.FindPluginByName(volumeToUnmount.PluginName)
	if err != nil || volumePlugin == nil {
		return volumetypes.GeneratedOperations{}, volumeToUnmount.GenerateErrorDetailed("UnmountVolume.FindPluginByName failed", err)
	}
	volumeUnmounter, newUnmounterErr := volumePlugin.NewUnmounter(
		volumeToUnmount.InnerVolumeSpecName, volumeToUnmount.PodUID)
	if newUnmounterErr != nil {
		return volumetypes.GeneratedOperations{}, volumeToUnmount.GenerateErrorDetailed("UnmountVolume.NewUnmounter failed", newUnmounterErr)
	}

	unmountVolumeFunc := func() volumetypes.OperationContext {
		subpather := og.volumePluginMgr.Host.GetSubpather()

		migrated := getMigratedStatusBySpec(volumeToUnmount.VolumeSpec)

		// Remove all bind-mounts for subPaths
		podDir := filepath.Join(podsDir, string(volumeToUnmount.PodUID))
		if err := subpather.CleanSubPaths(podDir, volumeToUnmount.InnerVolumeSpecName); err != nil {
			eventErr, detailedErr := volumeToUnmount.GenerateError("error cleaning subPath mounts", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Execute unmount
		unmountErr := volumeUnmounter.TearDown()
		if unmountErr != nil {
			// Mark the volume as uncertain, so SetUp is called for new pods. Teardown may be already in progress.
			opts := MarkVolumeOpts{
				PodName:             volumeToUnmount.PodName,
				PodUID:              volumeToUnmount.PodUID,
				VolumeName:          volumeToUnmount.VolumeName,
				OuterVolumeSpecName: volumeToUnmount.OuterVolumeSpecName,
				VolumeGIDVolume:     volumeToUnmount.VolumeGIDValue,
				VolumeSpec:          volumeToUnmount.VolumeSpec,
				VolumeMountState:    VolumeMountUncertain,
			}
			markMountUncertainErr := actualStateOfWorld.MarkVolumeMountAsUncertain(opts)
			if markMountUncertainErr != nil {
				// There is nothing else we can do. Hope that UnmountVolume will be re-tried shortly.
				klog.Error(volumeToUnmount.GenerateErrorDetailed("UnmountVolume.MarkVolumeMountAsUncertain failed", markMountUncertainErr).Error())
			}

			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToUnmount.GenerateError("UnmountVolume.TearDown failed", unmountErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		klog.Infof(
			"UnmountVolume.TearDown succeeded for volume %q (OuterVolumeSpecName: %q) pod %q (UID: %q). InnerVolumeSpecName %q. PluginName %q, VolumeGIDValue %q",
			volumeToUnmount.VolumeName,
			volumeToUnmount.OuterVolumeSpecName,
			volumeToUnmount.PodName,
			volumeToUnmount.PodUID,
			volumeToUnmount.InnerVolumeSpecName,
			volumeToUnmount.PluginName,
			volumeToUnmount.VolumeGIDValue)

		// Update actual state of world
		markVolMountedErr := actualStateOfWorld.MarkVolumeAsUnmounted(
			volumeToUnmount.PodName, volumeToUnmount.VolumeName)
		if markVolMountedErr != nil {
			// On failure, just log and exit
			klog.Error(volumeToUnmount.GenerateErrorDetailed("UnmountVolume.MarkVolumeAsUnmounted failed", markVolMountedErr).Error())
		}

		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "volume_unmount",
		OperationFunc:     unmountVolumeFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), volumeToUnmount.VolumeSpec), "volume_unmount"),
		EventRecorderFunc: nil, // nil because we do not want to generate event on error
	}, nil
}

func (og *operationGenerator) GenerateUnmountDeviceFunc(
	deviceToDetach AttachedVolume,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	hostutil hostutil.HostUtils) (volumetypes.GeneratedOperations, error) {
	// Get DeviceMounter plugin
	deviceMountableVolumePlugin, err :=
		og.volumePluginMgr.FindDeviceMountablePluginByName(deviceToDetach.PluginName)
	if err != nil || deviceMountableVolumePlugin == nil {
		return volumetypes.GeneratedOperations{}, deviceToDetach.GenerateErrorDetailed("UnmountDevice.FindDeviceMountablePluginByName failed", err)
	}

	volumeDeviceUnmounter, err := deviceMountableVolumePlugin.NewDeviceUnmounter()
	if err != nil {
		return volumetypes.GeneratedOperations{}, deviceToDetach.GenerateErrorDetailed("UnmountDevice.NewDeviceUnmounter failed", err)
	}

	volumeDeviceMounter, err := deviceMountableVolumePlugin.NewDeviceMounter()
	if err != nil {
		return volumetypes.GeneratedOperations{}, deviceToDetach.GenerateErrorDetailed("UnmountDevice.NewDeviceMounter failed", err)
	}

	unmountDeviceFunc := func() volumetypes.OperationContext {

		migrated := getMigratedStatusBySpec(deviceToDetach.VolumeSpec)

		//deviceMountPath := deviceToDetach.DeviceMountPath
		deviceMountPath, err :=
			volumeDeviceMounter.GetDeviceMountPath(deviceToDetach.VolumeSpec)
		if err != nil {
			// On failure other than "does not exist", return error. Caller will log and retry.
			if !strings.Contains(err.Error(), "does not exist") {
				eventErr, detailedErr := deviceToDetach.GenerateError("GetDeviceMountPath failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
			// If the mount path could not be found, don't fail the unmount, but instead log a warning and proceed,
			// using the value from deviceToDetach.DeviceMountPath, so that the device can be marked as unmounted
			deviceMountPath = deviceToDetach.DeviceMountPath
			klog.Warning(deviceToDetach.GenerateMsgDetailed(fmt.Sprintf(
				"GetDeviceMountPath failed, but unmount operation will proceed using deviceMountPath=%s: %v", deviceMountPath, err), ""))
		}
		refs, err := deviceMountableVolumePlugin.GetDeviceMountRefs(deviceMountPath)

		if err != nil || util.HasMountRefs(deviceMountPath, refs) {
			if err == nil {
				err = fmt.Errorf("the device mount path %q is still mounted by other references %v", deviceMountPath, refs)
			}
			eventErr, detailedErr := deviceToDetach.GenerateError("GetDeviceMountRefs check failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}
		// Execute unmount
		unmountDeviceErr := volumeDeviceUnmounter.UnmountDevice(deviceMountPath)
		if unmountDeviceErr != nil {
			// Mark the device as uncertain, so MountDevice is called for new pods. UnmountDevice may be already in progress.
			markDeviceUncertainErr := actualStateOfWorld.MarkDeviceAsUncertain(deviceToDetach.VolumeName, deviceToDetach.DevicePath, deviceMountPath, deviceToDetach.SELinuxMountContext)
			if markDeviceUncertainErr != nil {
				// There is nothing else we can do. Hope that UnmountDevice will be re-tried shortly.
				klog.Error(deviceToDetach.GenerateErrorDetailed("UnmountDevice.MarkDeviceAsUncertain failed", markDeviceUncertainErr).Error())
			}

			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := deviceToDetach.GenerateError("UnmountDevice failed", unmountDeviceErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}
		// Before logging that UnmountDevice succeeded and moving on,
		// use hostutil.PathIsDevice to check if the path is a device,
		// if so use hostutil.DeviceOpened to check if the device is in use anywhere
		// else on the system. Retry if it returns true.
		deviceOpened, deviceOpenedErr := isDeviceOpened(deviceToDetach, hostutil)
		if deviceOpenedErr != nil {
			return volumetypes.NewOperationContext(nil, deviceOpenedErr, migrated)
		}
		// The device is still in use elsewhere. Caller will log and retry.
		if deviceOpened {
			// Mark the device as uncertain, so MountDevice is called for new pods.
			markDeviceUncertainErr := actualStateOfWorld.MarkDeviceAsUncertain(deviceToDetach.VolumeName, deviceToDetach.DevicePath, deviceMountPath, deviceToDetach.SELinuxMountContext)
			if markDeviceUncertainErr != nil {
				// There is nothing else we can do. Hope that UnmountDevice will be re-tried shortly.
				klog.Error(deviceToDetach.GenerateErrorDetailed("UnmountDevice.MarkDeviceAsUncertain failed", markDeviceUncertainErr).Error())
			}
			eventErr, detailedErr := deviceToDetach.GenerateError(
				"UnmountDevice failed",
				goerrors.New("the device is in use when it was no longer expected to be in use"))
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		klog.Info(deviceToDetach.GenerateMsgDetailed("UnmountDevice succeeded", ""))

		// Update actual state of world
		markDeviceUnmountedErr := actualStateOfWorld.MarkDeviceAsUnmounted(
			deviceToDetach.VolumeName)
		if markDeviceUnmountedErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := deviceToDetach.GenerateError("MarkDeviceAsUnmounted failed", markDeviceUnmountedErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "unmount_device",
		OperationFunc:     unmountDeviceFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(deviceMountableVolumePlugin.GetPluginName(), deviceToDetach.VolumeSpec), "unmount_device"),
		EventRecorderFunc: nil, // nil because we do not want to generate event on error
	}, nil
}

// GenerateMapVolumeFunc marks volume as mounted based on following steps.
// If plugin is attachable, call WaitForAttach() and then mark the device
// as mounted. On next step, SetUpDevice is called without dependent of
// plugin type, but this method mainly is targeted for none attachable plugin.
// After setup is done, create symbolic links on both global map path and pod
// device map path. Once symbolic links are created, take fd lock by
// loopback for the device to avoid silent volume replacement. This lock
// will be released once no one uses the device.
// If all steps are completed, the volume is marked as mounted.
func (og *operationGenerator) GenerateMapVolumeFunc(
	waitForAttachTimeout time.Duration,
	volumeToMount VolumeToMount,
	actualStateOfWorld ActualStateOfWorldMounterUpdater) (volumetypes.GeneratedOperations, error) {

	// Get block volume mapper plugin
	blockVolumePlugin, err :=
		og.volumePluginMgr.FindMapperPluginBySpec(volumeToMount.VolumeSpec)
	if err != nil {
		return volumetypes.GeneratedOperations{}, volumeToMount.GenerateErrorDetailed("MapVolume.FindMapperPluginBySpec failed", err)
	}

	if blockVolumePlugin == nil {
		return volumetypes.GeneratedOperations{}, volumeToMount.GenerateErrorDetailed("MapVolume.FindMapperPluginBySpec failed to find BlockVolumeMapper plugin. Volume plugin is nil.", nil)
	}

	affinityErr := checkNodeAffinity(og, volumeToMount)
	if affinityErr != nil {
		eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.NodeAffinity check failed", affinityErr)
		og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeWarning, kevents.FailedMountVolume, eventErr.Error())
		return volumetypes.GeneratedOperations{}, detailedErr
	}
	blockVolumeMapper, newMapperErr := blockVolumePlugin.NewBlockVolumeMapper(
		volumeToMount.VolumeSpec,
		volumeToMount.Pod)
	if newMapperErr != nil {
		eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.NewBlockVolumeMapper initialization failed", newMapperErr)
		og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeWarning, kevents.FailedMapVolume, eventErr.Error())
		return volumetypes.GeneratedOperations{}, detailedErr
	}

	// Get attacher, if possible
	attachableVolumePlugin, _ :=
		og.volumePluginMgr.FindAttachablePluginBySpec(volumeToMount.VolumeSpec)
	var volumeAttacher volume.Attacher
	if attachableVolumePlugin != nil {
		volumeAttacher, _ = attachableVolumePlugin.NewAttacher()
	}

	mapVolumeFunc := func() (operationContext volumetypes.OperationContext) {
		var devicePath string
		var stagingPath string

		migrated := getMigratedStatusBySpec(volumeToMount.VolumeSpec)

		// Enforce ReadWriteOncePod access mode. This is also enforced during scheduling.
		if actualStateOfWorld.IsVolumeMountedElsewhere(volumeToMount.VolumeName, volumeToMount.PodName) &&
			// Because we do not know what access mode the pod intends to use if there are multiple.
			len(volumeToMount.VolumeSpec.PersistentVolume.Spec.AccessModes) == 1 &&
			v1helper.ContainsAccessMode(volumeToMount.VolumeSpec.PersistentVolume.Spec.AccessModes, v1.ReadWriteOncePod) {

			err = goerrors.New("volume uses the ReadWriteOncePod access mode and is already in use by another pod")
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.SetUpDevice failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Set up global map path under the given plugin directory using symbolic link
		globalMapPath, err :=
			blockVolumeMapper.GetGlobalMapPath(volumeToMount.VolumeSpec)
		if err != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.GetGlobalMapPath failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}
		if volumeAttacher != nil {
			// Wait for attachable volumes to finish attaching
			klog.InfoS(volumeToMount.GenerateMsgDetailed("MapVolume.WaitForAttach entering", fmt.Sprintf("DevicePath %q", volumeToMount.DevicePath)), "pod", klog.KObj(volumeToMount.Pod))

			devicePath, err = volumeAttacher.WaitForAttach(
				volumeToMount.VolumeSpec, volumeToMount.DevicePath, volumeToMount.Pod, waitForAttachTimeout)
			if err != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.WaitForAttach failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			klog.InfoS(volumeToMount.GenerateMsgDetailed("MapVolume.WaitForAttach succeeded", fmt.Sprintf("DevicePath %q", devicePath)), "pod", klog.KObj(volumeToMount.Pod))

		}
		// Call SetUpDevice if blockVolumeMapper implements CustomBlockVolumeMapper
		if customBlockVolumeMapper, ok := blockVolumeMapper.(volume.CustomBlockVolumeMapper); ok && actualStateOfWorld.GetDeviceMountState(volumeToMount.VolumeName) != DeviceGloballyMounted {
			var mapErr error
			stagingPath, mapErr = customBlockVolumeMapper.SetUpDevice()
			if mapErr != nil {
				og.markDeviceErrorState(volumeToMount, devicePath, globalMapPath, mapErr, actualStateOfWorld)
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.SetUpDevice failed", mapErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
		}

		// Update actual state of world to reflect volume is globally mounted
		markedDevicePath := devicePath
		markDeviceMappedErr := actualStateOfWorld.MarkDeviceAsMounted(
			volumeToMount.VolumeName, markedDevicePath, globalMapPath, "")
		if markDeviceMappedErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.MarkDeviceAsMounted failed", markDeviceMappedErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		markVolumeOpts := MarkVolumeOpts{
			PodName:             volumeToMount.PodName,
			PodUID:              volumeToMount.Pod.UID,
			VolumeName:          volumeToMount.VolumeName,
			BlockVolumeMapper:   blockVolumeMapper,
			OuterVolumeSpecName: volumeToMount.OuterVolumeSpecName,
			VolumeGIDVolume:     volumeToMount.VolumeGIDValue,
			VolumeSpec:          volumeToMount.VolumeSpec,
			VolumeMountState:    VolumeMounted,
		}

		// Call MapPodDevice if blockVolumeMapper implements CustomBlockVolumeMapper
		if customBlockVolumeMapper, ok := blockVolumeMapper.(volume.CustomBlockVolumeMapper); ok {
			// Execute driver specific map
			pluginDevicePath, mapErr := customBlockVolumeMapper.MapPodDevice()
			if mapErr != nil {
				// On failure, return error. Caller will log and retry.
				og.markVolumeErrorState(volumeToMount, markVolumeOpts, mapErr, actualStateOfWorld)
				eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.MapPodDevice failed", mapErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			// From now on, the volume is mapped. Mark it as uncertain on error,
			// so it is is unmapped when corresponding pod is deleted.
			defer func() {
				if operationContext.EventErr != nil {
					errText := operationContext.EventErr.Error()
					og.markVolumeErrorState(volumeToMount, markVolumeOpts, volumetypes.NewUncertainProgressError(errText), actualStateOfWorld)
				}
			}()

			// if pluginDevicePath is provided, assume attacher may not provide device
			// or attachment flow uses SetupDevice to get device path
			if len(pluginDevicePath) != 0 {
				devicePath = pluginDevicePath
			}
			if len(devicePath) == 0 {
				eventErr, detailedErr := volumeToMount.GenerateError("MapVolume failed", goerrors.New("device path of the volume is empty"))
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
		}

		// When kubelet is containerized, devicePath may be a symlink at a place unavailable to
		// kubelet, so evaluate it on the host and expect that it links to a device in /dev,
		// which will be available to containerized kubelet. If still it does not exist,
		// AttachFileDevice will fail. If kubelet is not containerized, eval it anyway.
		kvh, ok := og.GetVolumePluginMgr().Host.(volume.KubeletVolumeHost)
		if !ok {
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume type assertion error", fmt.Errorf("volume host does not implement KubeletVolumeHost interface"))
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}
		hu := kvh.GetHostUtil()
		devicePath, err = hu.EvalHostSymlinks(devicePath)
		if err != nil {
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.EvalHostSymlinks failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Update actual state of world with the devicePath again, if devicePath has changed from markedDevicePath
		// TODO: This can be improved after #82492 is merged and ASW has state.
		if markedDevicePath != devicePath {
			markDeviceMappedErr := actualStateOfWorld.MarkDeviceAsMounted(
				volumeToMount.VolumeName, devicePath, globalMapPath, "")
			if markDeviceMappedErr != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.MarkDeviceAsMounted failed", markDeviceMappedErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
		}

		// Execute common map
		volumeMapPath, volName := blockVolumeMapper.GetPodDeviceMapPath()
		mapErr := util.MapBlockVolume(og.blkUtil, devicePath, globalMapPath, volumeMapPath, volName, volumeToMount.Pod.UID)
		if mapErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.MapBlockVolume failed", mapErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Device mapping for global map path succeeded
		simpleMsg, detailedMsg := volumeToMount.GenerateMsg("MapVolume.MapPodDevice succeeded", fmt.Sprintf("globalMapPath %q", globalMapPath))
		verbosity := klog.Level(4)
		og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeNormal, kevents.SuccessfulMountVolume, simpleMsg)
		klog.V(verbosity).InfoS(detailedMsg, "pod", klog.KObj(volumeToMount.Pod))

		// Device mapping for pod device map path succeeded
		simpleMsg, detailedMsg = volumeToMount.GenerateMsg("MapVolume.MapPodDevice succeeded", fmt.Sprintf("volumeMapPath %q", volumeMapPath))
		verbosity = klog.Level(1)
		og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeNormal, kevents.SuccessfulMountVolume, simpleMsg)
		klog.V(verbosity).InfoS(detailedMsg, "pod", klog.KObj(volumeToMount.Pod))

		resizeOptions := volume.NodeResizeOptions{
			DevicePath:      devicePath,
			DeviceStagePath: stagingPath,
		}
		_, resizeError := og.expandVolumeDuringMount(volumeToMount, actualStateOfWorld, resizeOptions)
		if resizeError != nil {
			klog.Errorf("MapVolume.NodeExpandVolume failed with %v", resizeError)
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.MarkVolumeAsMounted failed while expanding volume", resizeError)
			// At this point, MountVolume.Setup already succeeded, we should add volume into actual state
			// so that reconciler can clean up volume when needed. However, if nodeExpandVolume failed,
			// we should not mark the volume as mounted to avoid pod starts using it.
			// Considering the above situations, we mark volume as uncertain here so that reconciler will trigger
			// volume tear down when pod is deleted, and also makes sure pod will not start using it.
			if err := actualStateOfWorld.MarkVolumeMountAsUncertain(markVolumeOpts); err != nil {
				klog.Error(volumeToMount.GenerateErrorDetailed("MountVolume.MarkVolumeMountAsUncertain failed", err).Error())
			}
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		markVolMountedErr := actualStateOfWorld.MarkVolumeAsMounted(markVolumeOpts)
		if markVolMountedErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToMount.GenerateError("MapVolume.MarkVolumeAsMounted failed", markVolMountedErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	eventRecorderFunc := func(err *error) {
		if *err != nil {
			og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeWarning, kevents.FailedMapVolume, (*err).Error())
		}
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "map_volume",
		OperationFunc:     mapVolumeFunc,
		EventRecorderFunc: eventRecorderFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(blockVolumePlugin.GetPluginName(), volumeToMount.VolumeSpec), "map_volume"),
	}, nil
}

// GenerateUnmapVolumeFunc marks volume as unmonuted based on following steps.
// Remove symbolic links from pod device map path dir and  global map path dir.
// Once those cleanups are done, remove pod device map path dir.
// If all steps are completed, the volume is marked as unmounted.
func (og *operationGenerator) GenerateUnmapVolumeFunc(
	volumeToUnmount MountedVolume,
	actualStateOfWorld ActualStateOfWorldMounterUpdater) (volumetypes.GeneratedOperations, error) {

	// Get block volume unmapper plugin
	blockVolumePlugin, err :=
		og.volumePluginMgr.FindMapperPluginByName(volumeToUnmount.PluginName)
	if err != nil {
		return volumetypes.GeneratedOperations{}, volumeToUnmount.GenerateErrorDetailed("UnmapVolume.FindMapperPluginByName failed", err)
	}
	if blockVolumePlugin == nil {
		return volumetypes.GeneratedOperations{}, volumeToUnmount.GenerateErrorDetailed("UnmapVolume.FindMapperPluginByName failed to find BlockVolumeMapper plugin. Volume plugin is nil.", nil)
	}
	blockVolumeUnmapper, newUnmapperErr := blockVolumePlugin.NewBlockVolumeUnmapper(
		volumeToUnmount.InnerVolumeSpecName, volumeToUnmount.PodUID)
	if newUnmapperErr != nil {
		return volumetypes.GeneratedOperations{}, volumeToUnmount.GenerateErrorDetailed("UnmapVolume.NewUnmapper failed", newUnmapperErr)
	}

	unmapVolumeFunc := func() volumetypes.OperationContext {

		migrated := getMigratedStatusBySpec(volumeToUnmount.VolumeSpec)

		// pods/{podUid}/volumeDevices/{escapeQualifiedPluginName}/{volumeName}
		podDeviceUnmapPath, volName := blockVolumeUnmapper.GetPodDeviceMapPath()
		// plugins/kubernetes.io/{PluginName}/volumeDevices/{volumePluginDependentPath}/{podUID}
		globalUnmapPath, err := blockVolumeUnmapper.GetGlobalMapPath(volumeToUnmount.VolumeSpec)
		if err != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToUnmount.GenerateError("UnmapVolume.GetGlobalMapPath failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Mark the device as uncertain to make sure kubelet calls UnmapDevice again in all the "return err"
		// cases below. The volume is marked as fully un-mapped at the end of this function, when everything
		// succeeds.
		markVolumeOpts := MarkVolumeOpts{
			PodName:             volumeToUnmount.PodName,
			PodUID:              volumeToUnmount.PodUID,
			VolumeName:          volumeToUnmount.VolumeName,
			OuterVolumeSpecName: volumeToUnmount.OuterVolumeSpecName,
			VolumeGIDVolume:     volumeToUnmount.VolumeGIDValue,
			VolumeSpec:          volumeToUnmount.VolumeSpec,
			VolumeMountState:    VolumeMountUncertain,
		}
		markVolumeUncertainErr := actualStateOfWorld.MarkVolumeMountAsUncertain(markVolumeOpts)
		if markVolumeUncertainErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToUnmount.GenerateError("UnmapVolume.MarkDeviceAsUncertain failed", markVolumeUncertainErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Execute common unmap
		unmapErr := util.UnmapBlockVolume(og.blkUtil, globalUnmapPath, podDeviceUnmapPath, volName, volumeToUnmount.PodUID)
		if unmapErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToUnmount.GenerateError("UnmapVolume.UnmapBlockVolume failed", unmapErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Call UnmapPodDevice if blockVolumeUnmapper implements CustomBlockVolumeUnmapper
		if customBlockVolumeUnmapper, ok := blockVolumeUnmapper.(volume.CustomBlockVolumeUnmapper); ok {
			// Execute plugin specific unmap
			unmapErr = customBlockVolumeUnmapper.UnmapPodDevice()
			if unmapErr != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToUnmount.GenerateError("UnmapVolume.UnmapPodDevice failed", unmapErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
		}

		klog.Infof(
			"UnmapVolume succeeded for volume %q (OuterVolumeSpecName: %q) pod %q (UID: %q). InnerVolumeSpecName %q. PluginName %q, VolumeGIDValue %q",
			volumeToUnmount.VolumeName,
			volumeToUnmount.OuterVolumeSpecName,
			volumeToUnmount.PodName,
			volumeToUnmount.PodUID,
			volumeToUnmount.InnerVolumeSpecName,
			volumeToUnmount.PluginName,
			volumeToUnmount.VolumeGIDValue)

		// Update actual state of world
		markVolUnmountedErr := actualStateOfWorld.MarkVolumeAsUnmounted(
			volumeToUnmount.PodName, volumeToUnmount.VolumeName)
		if markVolUnmountedErr != nil {
			// On failure, just log and exit
			klog.Error(volumeToUnmount.GenerateErrorDetailed("UnmapVolume.MarkVolumeAsUnmounted failed", markVolUnmountedErr).Error())
		}

		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "unmap_volume",
		OperationFunc:     unmapVolumeFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(blockVolumePlugin.GetPluginName(), volumeToUnmount.VolumeSpec), "unmap_volume"),
		EventRecorderFunc: nil, // nil because we do not want to generate event on error
	}, nil
}

// GenerateUnmapDeviceFunc marks device as unmounted based on following steps.
// Check under globalMapPath dir if there isn't pod's symbolic links in it.
// If symbolic link isn't there, the device isn't referenced from Pods.
// Call plugin TearDownDevice to clean-up device connection, stored data under
// globalMapPath, these operations depend on plugin implementation.
// Once TearDownDevice is completed, remove globalMapPath dir.
// After globalMapPath is removed, fd lock by loopback for the device can
// be released safely because no one can consume the device at this point.
// At last, device open status will be checked just in case.
// If all steps are completed, the device is marked as unmounted.
func (og *operationGenerator) GenerateUnmapDeviceFunc(
	deviceToDetach AttachedVolume,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	hostutil hostutil.HostUtils) (volumetypes.GeneratedOperations, error) {

	blockVolumePlugin, err :=
		og.volumePluginMgr.FindMapperPluginByName(deviceToDetach.PluginName)
	if err != nil {
		return volumetypes.GeneratedOperations{}, deviceToDetach.GenerateErrorDetailed("UnmapDevice.FindMapperPluginByName failed", err)
	}

	if blockVolumePlugin == nil {
		return volumetypes.GeneratedOperations{}, deviceToDetach.GenerateErrorDetailed("UnmapDevice.FindMapperPluginByName failed to find BlockVolumeMapper plugin. Volume plugin is nil.", nil)
	}

	blockVolumeUnmapper, newUnmapperErr := blockVolumePlugin.NewBlockVolumeUnmapper(
		deviceToDetach.VolumeSpec.Name(),
		"" /* podUID */)
	if newUnmapperErr != nil {
		return volumetypes.GeneratedOperations{}, deviceToDetach.GenerateErrorDetailed("UnmapDevice.NewUnmapper failed", newUnmapperErr)
	}

	unmapDeviceFunc := func() volumetypes.OperationContext {
		migrated := getMigratedStatusBySpec(deviceToDetach.VolumeSpec)
		// Search under globalMapPath dir if all symbolic links from pods have been removed already.
		// If symbolic links are there, pods may still refer the volume.
		globalMapPath := deviceToDetach.DeviceMountPath
		refs, err := og.blkUtil.GetDeviceBindMountRefs(deviceToDetach.DevicePath, globalMapPath)
		if err != nil {
			if os.IsNotExist(err) {
				// Looks like SetupDevice did not complete. Fall through to TearDownDevice and mark the device as unmounted.
				refs = nil
			} else {
				eventErr, detailedErr := deviceToDetach.GenerateError("UnmapDevice.GetDeviceBindMountRefs check failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
		}
		if len(refs) > 0 {
			err = fmt.Errorf("the device %q is still referenced from other Pods %v", globalMapPath, refs)
			eventErr, detailedErr := deviceToDetach.GenerateError("UnmapDevice failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Mark the device as uncertain to make sure kubelet calls UnmapDevice again in all the "return err"
		// cases below. The volume is marked as fully un-mapped at the end of this function, when everything
		// succeeds.
		markDeviceUncertainErr := actualStateOfWorld.MarkDeviceAsUncertain(
			deviceToDetach.VolumeName, deviceToDetach.DevicePath, globalMapPath, "" /* seLinuxMountContext */)
		if markDeviceUncertainErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := deviceToDetach.GenerateError("UnmapDevice.MarkDeviceAsUncertain failed", markDeviceUncertainErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Call TearDownDevice if blockVolumeUnmapper implements CustomBlockVolumeUnmapper
		if customBlockVolumeUnmapper, ok := blockVolumeUnmapper.(volume.CustomBlockVolumeUnmapper); ok {
			// Execute tear down device
			unmapErr := customBlockVolumeUnmapper.TearDownDevice(globalMapPath, deviceToDetach.DevicePath)
			if unmapErr != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := deviceToDetach.GenerateError("UnmapDevice.TearDownDevice failed", unmapErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
		}

		// Plugin finished TearDownDevice(). Now globalMapPath dir and plugin's stored data
		// on the dir are unnecessary, clean up it.
		removeMapPathErr := og.blkUtil.RemoveMapPath(globalMapPath)
		if removeMapPathErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := deviceToDetach.GenerateError("UnmapDevice.RemoveMapPath failed", removeMapPathErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Before logging that UnmapDevice succeeded and moving on,
		// use hostutil.PathIsDevice to check if the path is a device,
		// if so use hostutil.DeviceOpened to check if the device is in use anywhere
		// else on the system. Retry if it returns true.
		deviceOpened, deviceOpenedErr := isDeviceOpened(deviceToDetach, hostutil)
		if deviceOpenedErr != nil {
			return volumetypes.NewOperationContext(nil, deviceOpenedErr, migrated)
		}
		// The device is still in use elsewhere. Caller will log and retry.
		if deviceOpened {
			eventErr, detailedErr := deviceToDetach.GenerateError(
				"UnmapDevice failed",
				fmt.Errorf("the device is in use when it was no longer expected to be in use"))
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		klog.Info(deviceToDetach.GenerateMsgDetailed("UnmapDevice succeeded", ""))

		// Update actual state of world
		markDeviceUnmountedErr := actualStateOfWorld.MarkDeviceAsUnmounted(
			deviceToDetach.VolumeName)
		if markDeviceUnmountedErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := deviceToDetach.GenerateError("MarkDeviceAsUnmounted failed", markDeviceUnmountedErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "unmap_device",
		OperationFunc:     unmapDeviceFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(blockVolumePlugin.GetPluginName(), deviceToDetach.VolumeSpec), "unmap_device"),
		EventRecorderFunc: nil, // nil because we do not want to generate event on error
	}, nil
}

func (og *operationGenerator) GenerateVerifyControllerAttachedVolumeFunc(
	logger klog.Logger,
	volumeToMount VolumeToMount,
	nodeName types.NodeName,
	actualStateOfWorld ActualStateOfWorldAttacherUpdater) (volumetypes.GeneratedOperations, error) {
	volumePlugin, err :=
		og.volumePluginMgr.FindPluginBySpec(volumeToMount.VolumeSpec)
	if err != nil || volumePlugin == nil {
		return volumetypes.GeneratedOperations{}, volumeToMount.GenerateErrorDetailed("VerifyControllerAttachedVolume.FindPluginBySpec failed", err)
	}

	// For attachable volume types, lets check if volume is attached by reading from node lister.
	// This would avoid exponential back-off and creation of goroutine unnecessarily. We still
	// verify status of attached volume by directly reading from API server later on.This is necessarily
	// to ensure any race conditions because of cached state in the informer.
	if volumeToMount.PluginIsAttachable {
		cachedAttachedVolumes, _ := og.volumePluginMgr.Host.GetAttachedVolumesFromNodeStatus()
		if cachedAttachedVolumes != nil {
			_, volumeFound := cachedAttachedVolumes[volumeToMount.VolumeName]
			if !volumeFound {
				return volumetypes.GeneratedOperations{}, NewMountPreConditionFailedError(fmt.Sprintf("volume %s is not yet in node's status", volumeToMount.VolumeName))
			}
		}
	}

	verifyControllerAttachedVolumeFunc := func() volumetypes.OperationContext {
		migrated := getMigratedStatusBySpec(volumeToMount.VolumeSpec)
		claimSize := actualStateOfWorld.GetClaimSize(volumeToMount.VolumeName)

		// only fetch claimSize if it was not set previously
		if volumeToMount.VolumeSpec.PersistentVolume != nil && claimSize.IsZero() && !volumeToMount.VolumeSpec.InlineVolumeSpecForCSIMigration {
			pv := volumeToMount.VolumeSpec.PersistentVolume
			pvc, err := og.kubeClient.CoreV1().PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).Get(context.TODO(), pv.Spec.ClaimRef.Name, metav1.GetOptions{})
			if err != nil {
				eventErr, detailedErr := volumeToMount.GenerateError("VerifyControllerAttachedVolume fetching pvc failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
			pvcStatusSize := pvc.Status.Capacity.Storage()
			if pvcStatusSize != nil {
				claimSize = pvcStatusSize.DeepCopy()
			}
		}

		if !volumeToMount.PluginIsAttachable {
			// If the volume does not implement the attacher interface, it is
			// assumed to be attached and the actual state of the world is
			// updated accordingly.

			addVolumeNodeErr := actualStateOfWorld.MarkVolumeAsAttached(
				logger, volumeToMount.VolumeName, volumeToMount.VolumeSpec, nodeName, "" /* devicePath */)
			if addVolumeNodeErr != nil {
				// On failure, return error. Caller will log and retry.
				eventErr, detailedErr := volumeToMount.GenerateError("VerifyControllerAttachedVolume.MarkVolumeAsAttachedByUniqueVolumeName failed", addVolumeNodeErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}
			actualStateOfWorld.InitializeClaimSize(logger, volumeToMount.VolumeName, claimSize)
			return volumetypes.NewOperationContext(nil, nil, migrated)
		}

		if !volumeToMount.ReportedInUse {
			// If the given volume has not yet been added to the list of
			// VolumesInUse in the node's volume status, do not proceed, return
			// error. Caller will log and retry. The node status is updated
			// periodically by kubelet, so it may take as much as 10 seconds
			// before this clears.
			// Issue #28141 to enable on demand status updates.
			eventErr, detailedErr := volumeToMount.GenerateError("Volume has not been added to the list of VolumesInUse in the node's volume status", nil)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		// Fetch current node object
		node, fetchErr := og.kubeClient.CoreV1().Nodes().Get(context.TODO(), string(nodeName), metav1.GetOptions{})
		if fetchErr != nil {
			// On failure, return error. Caller will log and retry.
			eventErr, detailedErr := volumeToMount.GenerateError("VerifyControllerAttachedVolume failed fetching node from API server", fetchErr)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		for _, attachedVolume := range node.Status.VolumesAttached {
			if attachedVolume.Name == volumeToMount.VolumeName {
				addVolumeNodeErr := actualStateOfWorld.MarkVolumeAsAttached(
					logger, v1.UniqueVolumeName(""), volumeToMount.VolumeSpec, nodeName, attachedVolume.DevicePath)
				klog.InfoS(volumeToMount.GenerateMsgDetailed("Controller attach succeeded", fmt.Sprintf("device path: %q", attachedVolume.DevicePath)), "pod", klog.KObj(volumeToMount.Pod))
				if addVolumeNodeErr != nil {
					// On failure, return error. Caller will log and retry.
					eventErr, detailedErr := volumeToMount.GenerateError("VerifyControllerAttachedVolume.MarkVolumeAsAttached failed", addVolumeNodeErr)
					return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
				}
				actualStateOfWorld.InitializeClaimSize(logger, volumeToMount.VolumeName, claimSize)
				return volumetypes.NewOperationContext(nil, nil, migrated)
			}
		}

		// Volume not attached, return error. Caller will log and retry.
		eventErr, detailedErr := volumeToMount.GenerateError("Volume not attached according to node status", nil)
		return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
	}

	return volumetypes.GeneratedOperations{
		OperationName:     VerifyControllerAttachedVolumeOpName,
		OperationFunc:     verifyControllerAttachedVolumeFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), volumeToMount.VolumeSpec), "verify_controller_attached_volume"),
		EventRecorderFunc: nil, // nil because we do not want to generate event on error
	}, nil

}

func (og *operationGenerator) verifyVolumeIsSafeToDetach(
	volumeToDetach AttachedVolume) error {
	// Fetch current node object
	node, fetchErr := og.kubeClient.CoreV1().Nodes().Get(context.TODO(), string(volumeToDetach.NodeName), metav1.GetOptions{})
	if fetchErr != nil {
		if errors.IsNotFound(fetchErr) {
			klog.Warning(volumeToDetach.GenerateMsgDetailed("Node not found on API server. DetachVolume will skip safe to detach check", ""))
			return nil
		}

		// On failure, return error. Caller will log and retry.
		return volumeToDetach.GenerateErrorDetailed("DetachVolume failed fetching node from API server", fetchErr)
	}

	for _, inUseVolume := range node.Status.VolumesInUse {
		if inUseVolume == volumeToDetach.VolumeName {
			return volumeToDetach.GenerateErrorDetailed(
				"DetachVolume failed",
				fmt.Errorf("volume is still in use by node, according to Node status"))
		}
	}

	// Volume is not marked as in use by node
	klog.Info(volumeToDetach.GenerateMsgDetailed("Verified volume is safe to detach", ""))
	return nil
}

func (og *operationGenerator) GenerateExpandVolumeFunc(
	pvc *v1.PersistentVolumeClaim,
	pv *v1.PersistentVolume) (volumetypes.GeneratedOperations, error) {

	volumeSpec := volume.NewSpecFromPersistentVolume(pv, false)

	volumePlugin, err := og.volumePluginMgr.FindExpandablePluginBySpec(volumeSpec)
	if err != nil {
		return volumetypes.GeneratedOperations{}, fmt.Errorf("error finding plugin for expanding volume: %q with error %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
	}

	if volumePlugin == nil {
		return volumetypes.GeneratedOperations{}, fmt.Errorf("can not find plugin for expanding volume: %q", util.GetPersistentVolumeClaimQualifiedName(pvc))
	}

	expandVolumeFunc := func() volumetypes.OperationContext {
		migrated := false

		newSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
		statusSize := pvc.Status.Capacity[v1.ResourceStorage]
		pvSize := pv.Spec.Capacity[v1.ResourceStorage]
		if pvSize.Cmp(newSize) < 0 {
			updatedSize, expandErr := volumePlugin.ExpandVolumeDevice(
				volumeSpec,
				newSize,
				statusSize)
			if expandErr != nil {
				detailedErr := fmt.Errorf("error expanding volume %q of plugin %q: %v", util.GetPersistentVolumeClaimQualifiedName(pvc), volumePlugin.GetPluginName(), expandErr)
				return volumetypes.NewOperationContext(detailedErr, detailedErr, migrated)
			}

			klog.Infof("ExpandVolume succeeded for volume %s", util.GetPersistentVolumeClaimQualifiedName(pvc))

			newSize = updatedSize
			// k8s doesn't have transactions, we can't guarantee that after updating PV - updating PVC will be
			// successful, that is why all PVCs for which pvc.Spec.Size > pvc.Status.Size must be reprocessed
			// until they reflect user requested size in pvc.Status.Size
			_, updateErr := util.UpdatePVSize(pv, newSize, og.kubeClient)
			if updateErr != nil {
				detailedErr := fmt.Errorf("error updating PV spec capacity for volume %q with : %v", util.GetPersistentVolumeClaimQualifiedName(pvc), updateErr)
				return volumetypes.NewOperationContext(detailedErr, detailedErr, migrated)
			}

			klog.Infof("ExpandVolume.UpdatePV succeeded for volume %s", util.GetPersistentVolumeClaimQualifiedName(pvc))
		}

		fsVolume, _ := util.CheckVolumeModeFilesystem(volumeSpec)
		// No Cloudprovider resize needed, lets mark resizing as done
		// Rest of the volume expand controller code will assume PVC as *not* resized until pvc.Status.Size
		// reflects user requested size.
		if !volumePlugin.RequiresFSResize() || !fsVolume {
			klog.V(4).Infof("Controller resizing done for PVC %s", util.GetPersistentVolumeClaimQualifiedName(pvc))
			_, err := util.MarkResizeFinished(pvc, newSize, og.kubeClient)
			if err != nil {
				detailedErr := fmt.Errorf("error marking pvc %s as resized : %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
				return volumetypes.NewOperationContext(detailedErr, detailedErr, migrated)
			}
			successMsg := fmt.Sprintf("ExpandVolume succeeded for volume %s", util.GetPersistentVolumeClaimQualifiedName(pvc))
			og.recorder.Eventf(pvc, v1.EventTypeNormal, kevents.VolumeResizeSuccess, successMsg)
		} else {
			_, err := util.MarkForFSResize(pvc, og.kubeClient)
			if err != nil {
				detailedErr := fmt.Errorf("error updating pvc %s condition for fs resize : %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
				klog.Warning(detailedErr)
				return volumetypes.NewOperationContext(nil, nil, migrated)
			}
			oldCapacity := pvc.Status.Capacity[v1.ResourceStorage]
			err = util.AddAnnPreResizeCapacity(pv, oldCapacity, og.kubeClient)
			if err != nil {
				detailedErr := fmt.Errorf("error updating pv %s annotation (%s) with pre-resize capacity %s: %v", pv.ObjectMeta.Name, util.AnnPreResizeCapacity, oldCapacity.String(), err)
				klog.Warning(detailedErr)
				return volumetypes.NewOperationContext(nil, nil, migrated)
			}

		}
		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	eventRecorderFunc := func(err *error) {
		if *err != nil {
			og.recorder.Eventf(pvc, v1.EventTypeWarning, kevents.VolumeResizeFailed, (*err).Error())
		}
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "expand_volume",
		OperationFunc:     expandVolumeFunc,
		EventRecorderFunc: eventRecorderFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), volumeSpec), "expand_volume"),
	}, nil
}

func (og *operationGenerator) GenerateExpandAndRecoverVolumeFunc(
	pvc *v1.PersistentVolumeClaim,
	pv *v1.PersistentVolume, resizerName string) (volumetypes.GeneratedOperations, error) {

	volumeSpec := volume.NewSpecFromPersistentVolume(pv, false)

	volumePlugin, err := og.volumePluginMgr.FindExpandablePluginBySpec(volumeSpec)
	if err != nil {
		return volumetypes.GeneratedOperations{}, fmt.Errorf("error finding plugin for expanding volume: %q with error %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
	}

	if volumePlugin == nil {
		return volumetypes.GeneratedOperations{}, fmt.Errorf("can not find plugin for expanding volume: %q", util.GetPersistentVolumeClaimQualifiedName(pvc))
	}

	expandVolumeFunc := func() volumetypes.OperationContext {
		resizeOpts := inTreeResizeOpts{
			pvc:          pvc,
			pv:           pv,
			resizerName:  resizerName,
			volumePlugin: volumePlugin,
			volumeSpec:   volumeSpec,
		}
		migrated := false
		resp := og.expandAndRecoverFunction(resizeOpts)
		if resp.err != nil {
			return volumetypes.NewOperationContext(resp.err, resp.err, migrated)
		}
		return volumetypes.NewOperationContext(nil, nil, migrated)
	}

	eventRecorderFunc := func(err *error) {
		if *err != nil {
			og.recorder.Eventf(pvc, v1.EventTypeWarning, kevents.VolumeResizeFailed, (*err).Error())
		}
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "expand_volume",
		OperationFunc:     expandVolumeFunc,
		EventRecorderFunc: eventRecorderFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), volumeSpec), "expand_volume"),
	}, nil
}

// Deprecated: This function should not called by any controller code in future and should be removed
// from kubernetes code
func (og *operationGenerator) expandAndRecoverFunction(resizeOpts inTreeResizeOpts) inTreeResizeResponse {
	pvc := resizeOpts.pvc
	pv := resizeOpts.pv
	resizerName := resizeOpts.resizerName
	volumePlugin := resizeOpts.volumePlugin
	volumeSpec := resizeOpts.volumeSpec

	pvcSpecSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
	pvcStatusSize := pvc.Status.Capacity[v1.ResourceStorage]
	pvSize := pv.Spec.Capacity[v1.ResourceStorage]

	resizeResponse := inTreeResizeResponse{
		pvc:          pvc,
		pv:           pv,
		resizeCalled: false,
	}

	// by default we are expanding to fulfill size requested in pvc.Spec.Resources
	newSize := pvcSpecSize

	var resizeStatus v1.ClaimResourceStatus
	if status, ok := pvc.Status.AllocatedResourceStatuses[v1.ResourceStorage]; ok {
		resizeStatus = status
	}

	var allocatedSize *resource.Quantity
	t, ok := pvc.Status.AllocatedResources[v1.ResourceStorage]
	if ok {
		allocatedSize = &t
	}
	var err error

	if pvSize.Cmp(pvcSpecSize) < 0 {
		// pv is not of requested size yet and hence will require expanding

		switch resizeStatus {
		case v1.PersistentVolumeClaimControllerResizeInProgress,
			v1.PersistentVolumeClaimNodeResizePending,
			v1.PersistentVolumeClaimNodeResizeInProgress,
			v1.PersistentVolumeClaimNodeResizeInfeasible:
			if allocatedSize != nil {
				newSize = *allocatedSize
			}
		default:
			newSize = pvcSpecSize
		}
	} else {
		// PV has already been expanded and hence we can be here for following reasons:
		//   1. If expansion is pending on the node and this was just a spurious update event
		//      we don't need to do anything and let kubelet handle it.
		//   2. It could be that - although we successfully expanded the volume, we failed to
		//      record our work in API objects, in which case - we should resume resizing operation
		//      and let API objects be updated.
		//   3. Controller successfully expanded the volume, but expansion is failing on the node
		//      and before kubelet can retry failed node expansion - controller must verify if it is
		//      safe to do so.
		//   4. While expansion was still pending on the node, user reduced the pvc size.
		switch resizeStatus {
		case v1.PersistentVolumeClaimNodeResizeInProgress,
			v1.PersistentVolumeClaimNodeResizePending:
			// we don't need to do any work. We could be here because of a spurious update event.
			// This is case #1
			return resizeResponse
		case v1.PersistentVolumeClaimNodeResizeInfeasible:
			// This is case#3
			pvc, err = og.markForPendingNodeExpansion(pvc, pv)
			resizeResponse.pvc = pvc
			resizeResponse.err = err
			return resizeResponse
		case v1.PersistentVolumeClaimControllerResizeInProgress,
			v1.PersistentVolumeClaimControllerResizeInfeasible:
			// This is case#2 or it could also be case#4 when user manually shrunk the PVC
			// after expanding it.
			if allocatedSize != nil {
				newSize = *allocatedSize
			}
		default:
			// It is impossible for ResizeStatus to be "" and allocatedSize to be not nil but somehow
			// if we do end up in this state, it is safest to resume expansion to last recorded size in
			// allocatedSize variable.
			if resizeStatus == "" && allocatedSize != nil {
				newSize = *allocatedSize
			} else {
				newSize = pvcSpecSize
			}
		}
	}

	pvc, err = util.MarkControllerReisizeInProgress(pvc, resizerName, newSize, og.kubeClient)
	if err != nil {
		msg := fmt.Errorf("error updating pvc %s with resize in progress: %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
		resizeResponse.err = msg
		resizeResponse.pvc = pvc
		return resizeResponse
	}

	updatedSize, err := volumePlugin.ExpandVolumeDevice(volumeSpec, newSize, pvcStatusSize)
	resizeResponse.resizeCalled = true

	if err != nil {
		msg := fmt.Errorf("error expanding pvc %s: %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
		resizeResponse.err = msg
		resizeResponse.pvc = pvc
		return resizeResponse
	}

	// update PV size
	var updateErr error
	pv, updateErr = util.UpdatePVSize(pv, updatedSize, og.kubeClient)
	// if updating PV failed, we are going to leave the PVC in ControllerExpansionInProgress state, so as expansion can be retried to previously set allocatedSize value.
	if updateErr != nil {
		msg := fmt.Errorf("error updating pv for pvc %s: %v", util.GetPersistentVolumeClaimQualifiedName(pvc), updateErr)
		resizeResponse.err = msg
		return resizeResponse
	}
	resizeResponse.pv = pv

	fsVolume, _ := util.CheckVolumeModeFilesystem(volumeSpec)

	if !volumePlugin.RequiresFSResize() || !fsVolume {
		pvc, err = util.MarkResizeFinished(pvc, updatedSize, og.kubeClient)
		if err != nil {
			msg := fmt.Errorf("error marking pvc %s as resized: %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
			resizeResponse.err = msg
			return resizeResponse
		}
		resizeResponse.pvc = pvc
		successMsg := fmt.Sprintf("ExpandVolume succeeded for volume %s", util.GetPersistentVolumeClaimQualifiedName(pvc))
		og.recorder.Eventf(pvc, v1.EventTypeNormal, kevents.VolumeResizeSuccess, successMsg)
	} else {
		pvc, err = og.markForPendingNodeExpansion(pvc, pv)
		resizeResponse.pvc = pvc
		if err != nil {
			msg := fmt.Errorf("error marking pvc %s for node expansion: %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
			resizeResponse.err = msg
			return resizeResponse
		}
	}
	return resizeResponse
}

func (og *operationGenerator) markForPendingNodeExpansion(pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume) (*v1.PersistentVolumeClaim, error) {
	var err error
	pvc, err = util.MarkForFSResize(pvc, og.kubeClient)
	if err != nil {
		msg := fmt.Errorf("error marking pvc %s for node expansion: %v", util.GetPersistentVolumeClaimQualifiedName(pvc), err)
		return pvc, msg
	}
	// store old PVC capacity in pv, so as if PVC gets deleted while node expansion was pending
	// we can restore size of pvc from PV annotation and still perform expansion on the node
	oldCapacity := pvc.Status.Capacity[v1.ResourceStorage]
	err = util.AddAnnPreResizeCapacity(pv, oldCapacity, og.kubeClient)
	if err != nil {
		detailedErr := fmt.Errorf("error updating pv %s annotation (%s) with pre-resize capacity %s: %v", pv.ObjectMeta.Name, util.AnnPreResizeCapacity, oldCapacity.String(), err)
		klog.Warning(detailedErr)
		return pvc, detailedErr
	}
	return pvc, nil
}

func (og *operationGenerator) GenerateExpandInUseVolumeFunc(
	volumeToMount VolumeToMount,
	actualStateOfWorld ActualStateOfWorldMounterUpdater, currentSize resource.Quantity) (volumetypes.GeneratedOperations, error) {

	volumePlugin, err :=
		og.volumePluginMgr.FindPluginBySpec(volumeToMount.VolumeSpec)
	if err != nil || volumePlugin == nil {
		return volumetypes.GeneratedOperations{}, volumeToMount.GenerateErrorDetailed("NodeExpandVolume.FindPluginBySpec failed", err)
	}

	fsResizeFunc := func() volumetypes.OperationContext {
		var resizeDone bool
		var eventErr, detailedErr error
		migrated := false

		if currentSize.IsZero() || volumeToMount.DesiredPersistentVolumeSize.IsZero() {
			err := fmt.Errorf("current or new size of the volume is not set")
			eventErr, detailedErr = volumeToMount.GenerateError("NodeExpandvolume.expansion failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		resizeOptions := volume.NodeResizeOptions{
			VolumeSpec: volumeToMount.VolumeSpec,
			DevicePath: volumeToMount.DevicePath,
			OldSize:    currentSize,
			NewSize:    volumeToMount.DesiredPersistentVolumeSize,
		}
		fsVolume, err := util.CheckVolumeModeFilesystem(volumeToMount.VolumeSpec)
		if err != nil {
			eventErr, detailedErr = volumeToMount.GenerateError("NodeExpandvolume.CheckVolumeModeFilesystem failed", err)
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}

		if fsVolume {
			volumeMounter, newMounterErr := volumePlugin.NewMounter(
				volumeToMount.VolumeSpec,
				volumeToMount.Pod)
			if newMounterErr != nil {
				eventErr, detailedErr = volumeToMount.GenerateError("NodeExpandVolume.NewMounter initialization failed", newMounterErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			resizeOptions.DeviceMountPath = volumeMounter.GetPath()

			deviceMountableVolumePlugin, _ := og.volumePluginMgr.FindDeviceMountablePluginBySpec(volumeToMount.VolumeSpec)
			var volumeDeviceMounter volume.DeviceMounter
			if deviceMountableVolumePlugin != nil {
				volumeDeviceMounter, _ = deviceMountableVolumePlugin.NewDeviceMounter()
			}

			if volumeDeviceMounter != nil {
				deviceStagePath, err := volumeDeviceMounter.GetDeviceMountPath(volumeToMount.VolumeSpec)
				if err != nil {
					eventErr, detailedErr = volumeToMount.GenerateError("NodeExpandVolume.GetDeviceMountPath failed", err)
					return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
				}
				resizeOptions.DeviceStagePath = deviceStagePath
			}
		} else {
			// Get block volume mapper plugin
			blockVolumePlugin, err :=
				og.volumePluginMgr.FindMapperPluginBySpec(volumeToMount.VolumeSpec)
			if err != nil {
				eventErr, detailedErr = volumeToMount.GenerateError("MapVolume.FindMapperPluginBySpec failed", err)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			if blockVolumePlugin == nil {
				eventErr, detailedErr = volumeToMount.GenerateError("MapVolume.FindMapperPluginBySpec failed to find BlockVolumeMapper plugin. Volume plugin is nil.", nil)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			blockVolumeMapper, newMapperErr := blockVolumePlugin.NewBlockVolumeMapper(
				volumeToMount.VolumeSpec,
				volumeToMount.Pod)
			if newMapperErr != nil {
				eventErr, detailedErr = volumeToMount.GenerateError("MapVolume.NewBlockVolumeMapper initialization failed", newMapperErr)
				return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
			}

			// if plugin supports custom mappers lets add DeviceStagePath
			if customBlockVolumeMapper, ok := blockVolumeMapper.(volume.CustomBlockVolumeMapper); ok {
				resizeOptions.DeviceStagePath = customBlockVolumeMapper.GetStagingPath()
			}
		}

		// if we are doing online expansion then volume is already published
		resizeDone, eventErr, detailedErr = og.doOnlineExpansion(volumeToMount, actualStateOfWorld, resizeOptions)
		if eventErr != nil || detailedErr != nil {
			return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
		}
		if resizeDone {
			return volumetypes.NewOperationContext(nil, nil, migrated)
		}
		// This is a placeholder error - we should NEVER reach here.
		err = fmt.Errorf("volume resizing failed for unknown reason")
		eventErr, detailedErr = volumeToMount.GenerateError("NodeExpandVolume.NodeExpandVolume failed to resize volume", err)
		return volumetypes.NewOperationContext(eventErr, detailedErr, migrated)
	}

	eventRecorderFunc := func(err *error) {
		if *err != nil {
			og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeWarning, kevents.VolumeResizeFailed, (*err).Error())
		}
	}

	return volumetypes.GeneratedOperations{
		OperationName:     "volume_fs_resize",
		OperationFunc:     fsResizeFunc,
		EventRecorderFunc: eventRecorderFunc,
		CompleteFunc:      util.OperationCompleteHook(util.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), volumeToMount.VolumeSpec), "volume_fs_resize"),
	}, nil
}

func (og *operationGenerator) doOnlineExpansion(volumeToMount VolumeToMount,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	resizeOptions volume.NodeResizeOptions) (bool, error, error) {

	resizeDone, newSize, err := og.nodeExpandVolume(volumeToMount, actualStateOfWorld, resizeOptions)
	if err != nil {
		e1, e2 := volumeToMount.GenerateError("NodeExpandVolume.NodeExpandVolume failed", err)
		klog.Error(e2.Error())
		return false, e1, e2
	}
	if resizeDone {
		markingDone := actualStateOfWorld.MarkVolumeAsResized(volumeToMount.VolumeName, newSize)
		if !markingDone {
			// On failure, return error. Caller will log and retry.
			genericFailureError := fmt.Errorf("unable to mark volume as resized")
			e1, e2 := volumeToMount.GenerateError("NodeExpandVolume.MarkVolumeAsResized failed", genericFailureError)
			return false, e1, e2
		}
		return true, nil, nil
	}
	return false, nil, nil
}

func (og *operationGenerator) expandVolumeDuringMount(volumeToMount VolumeToMount, actualStateOfWorld ActualStateOfWorldMounterUpdater, rsOpts volume.NodeResizeOptions) (bool, error) {
	supportsExpansion, expandablePlugin := og.checkIfSupportsNodeExpansion(volumeToMount)
	if supportsExpansion {
		pv := volumeToMount.VolumeSpec.PersistentVolume
		pvc, err := og.kubeClient.CoreV1().PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).Get(context.TODO(), pv.Spec.ClaimRef.Name, metav1.GetOptions{})
		if err != nil {
			// Return error rather than leave the file system un-resized, caller will log and retry
			return false, fmt.Errorf("mountVolume.NodeExpandVolume get PVC failed : %v", err)
		}

		pvcStatusCap := pvc.Status.Capacity[v1.ResourceStorage]
		pvSpecCap := pv.Spec.Capacity[v1.ResourceStorage]
		if pvcStatusCap.Cmp(pvSpecCap) < 0 {
			if volumeToMount.VolumeSpec.ReadOnly {
				simpleMsg, detailedMsg := volumeToMount.GenerateMsg("MountVolume.NodeExpandVolume failed", "requested read-only file system")
				klog.Warning(detailedMsg)
				og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeWarning, kevents.FileSystemResizeFailed, simpleMsg)
				og.recorder.Eventf(pvc, v1.EventTypeWarning, kevents.FileSystemResizeFailed, simpleMsg)
				return true, nil
			}

			rsOpts.NewSize = pvSpecCap
			rsOpts.OldSize = pvcStatusCap
			rsOpts.VolumeSpec = volumeToMount.VolumeSpec
			resizeOp := nodeResizeOperationOpts{
				vmt:                volumeToMount,
				pvc:                pvc,
				pv:                 pv,
				pluginResizeOpts:   rsOpts,
				volumePlugin:       expandablePlugin,
				actualStateOfWorld: actualStateOfWorld,
			}
			if og.checkForRecoveryFromExpansion(pvc, volumeToMount) {
				// if recovery feature is enabled, we can use allocated size from PVC status as new size
				rsOpts.NewSize = pvc.Status.AllocatedResources[v1.ResourceStorage]
				resizeOp.pluginResizeOpts = rsOpts
				nodeExpander := newNodeExpander(resizeOp, og.kubeClient, og.recorder)
				resizeFinished, _, err := nodeExpander.expandOnPlugin()
				return resizeFinished, err
			} else {
				return og.legacyCallNodeExpandOnPlugin(resizeOp)
			}
		}
	}
	return true, nil
}

func (og *operationGenerator) checkIfSupportsNodeExpansion(volumeToMount VolumeToMount) (bool, volume.NodeExpandableVolumePlugin) {
	if volumeToMount.VolumeSpec != nil &&
		volumeToMount.VolumeSpec.InlineVolumeSpecForCSIMigration {
		klog.V(4).Infof("This volume %s is a migrated inline volume and is not resizable", volumeToMount.VolumeName)
		return false, nil
	}

	// Get expander, if possible
	expandableVolumePlugin, _ :=
		og.volumePluginMgr.FindNodeExpandablePluginBySpec(volumeToMount.VolumeSpec)
	if expandableVolumePlugin != nil &&
		expandableVolumePlugin.RequiresFSResize() &&
		volumeToMount.VolumeSpec.PersistentVolume != nil {
		return true, expandableVolumePlugin
	}
	return false, nil
}

func (og *operationGenerator) nodeExpandVolume(
	volumeToMount VolumeToMount,
	actualStateOfWorld ActualStateOfWorldMounterUpdater,
	rsOpts volume.NodeResizeOptions) (bool, resource.Quantity, error) {

	supportsExpansion, expandableVolumePlugin := og.checkIfSupportsNodeExpansion(volumeToMount)

	if supportsExpansion {
		// lets use sizes handed over to us by caller for comparison
		if rsOpts.NewSize.Cmp(rsOpts.OldSize) > 0 {
			pv := volumeToMount.VolumeSpec.PersistentVolume
			pvc, err := og.kubeClient.CoreV1().PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).Get(context.TODO(), pv.Spec.ClaimRef.Name, metav1.GetOptions{})
			currentSize := pvc.Status.Capacity.Storage()
			if err != nil {
				// Return error rather than leave the file system un-resized, caller will log and retry
				return false, *currentSize, fmt.Errorf("mountVolume.NodeExpandVolume get PVC failed : %w", err)
			}

			if volumeToMount.VolumeSpec.ReadOnly {
				simpleMsg, detailedMsg := volumeToMount.GenerateMsg("MountVolume.NodeExpandVolume failed", "requested read-only file system")
				klog.Warning(detailedMsg)
				og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeWarning, kevents.FileSystemResizeFailed, simpleMsg)
				og.recorder.Eventf(pvc, v1.EventTypeWarning, kevents.FileSystemResizeFailed, simpleMsg)
				return true, *currentSize, nil
			}
			resizeOp := nodeResizeOperationOpts{
				vmt:                volumeToMount,
				pvc:                pvc,
				pv:                 pv,
				pluginResizeOpts:   rsOpts,
				volumePlugin:       expandableVolumePlugin,
				actualStateOfWorld: actualStateOfWorld,
			}

			if og.checkForRecoveryFromExpansion(pvc, volumeToMount) {
				// if recovery feature is enabled, we can use allocated size from PVC status as new size
				newSize := pvc.Status.AllocatedResources[v1.ResourceStorage]
				rsOpts.NewSize = newSize
				resizeOp.pluginResizeOpts.NewSize = newSize
				nodeExpander := newNodeExpander(resizeOp, og.kubeClient, og.recorder)
				resizeFinished, newSize, err := nodeExpander.expandOnPlugin()
				return resizeFinished, newSize, err
			} else {
				resizeFinished, err := og.legacyCallNodeExpandOnPlugin(resizeOp)
				return resizeFinished, rsOpts.NewSize, err
			}
		}
	}
	return true, rsOpts.OldSize, nil
}

func (og *operationGenerator) checkForRecoveryFromExpansion(pvc *v1.PersistentVolumeClaim, volumeToMount VolumeToMount) bool {
	resizeStatus := pvc.Status.AllocatedResourceStatuses[v1.ResourceStorage]
	allocatedResource := pvc.Status.AllocatedResources
	featureGateStatus := utilfeature.DefaultFeatureGate.Enabled(features.RecoverVolumeExpansionFailure)

	if !featureGateStatus {
		return false
	}

	// Even though RecoverVolumeExpansionFailure feature gate is enabled, it appears that we are running with older version
	// of resize controller, which will not populate allocatedResource and resizeStatus. This can happen because of version skew
	// and hence we are going to keep expanding using older logic.
	if resizeStatus == "" && allocatedResource == nil {
		_, detailedMsg := volumeToMount.GenerateMsg("MountVolume.NodeExpandVolume running with", "older external resize controller")
		klog.Warning(detailedMsg)
		return false
	}
	return true
}

// legacyCallNodeExpandOnPlugin is old version of calling node expansion on plugin, which does not support
// recovery from volume expansion failure
// TODO: Removing this code when RecoverVolumeExpansionFailure feature goes GA.
func (og *operationGenerator) legacyCallNodeExpandOnPlugin(resizeOp nodeResizeOperationOpts) (bool, error) {
	pvc := resizeOp.pvc
	volumeToMount := resizeOp.vmt
	rsOpts := resizeOp.pluginResizeOpts
	actualStateOfWorld := resizeOp.actualStateOfWorld
	expandableVolumePlugin := resizeOp.volumePlugin

	pvcStatusCap := pvc.Status.Capacity[v1.ResourceStorage]

	nodeName := volumeToMount.Pod.Spec.NodeName

	var err error

	// File system resize was requested, proceed
	klog.V(4).InfoS(volumeToMount.GenerateMsgDetailed("MountVolume.NodeExpandVolume entering", fmt.Sprintf("DevicePath %q", volumeToMount.DevicePath)), "pod", klog.KObj(volumeToMount.Pod))

	rsOpts.VolumeSpec = volumeToMount.VolumeSpec

	_, resizeErr := expandableVolumePlugin.NodeExpand(rsOpts)
	if resizeErr != nil {
		// This is a workaround for now, until RecoverFromVolumeExpansionFailure feature goes GA.
		// If RecoverFromVolumeExpansionFailure feature is enabled, we will not ever hit this state, because
		// we will wait for VolumeExpansionPendingOnNode before trying to expand volume in kubelet.
		if volumetypes.IsOperationNotSupportedError(resizeErr) {
			klog.V(4).InfoS(volumeToMount.GenerateMsgDetailed("MountVolume.NodeExpandVolume failed", "NodeExpandVolume not supported"), "pod", klog.KObj(volumeToMount.Pod))
			return true, nil
		}

		// if driver returned FailedPrecondition error that means
		// volume expansion should not be retried on this node but
		// expansion operation should not block mounting
		if volumetypes.IsFailedPreconditionError(resizeErr) {
			actualStateOfWorld.MarkForInUseExpansionError(volumeToMount.VolumeName)
			klog.Error(volumeToMount.GenerateErrorDetailed("MountVolume.NodeExapndVolume failed", resizeErr).Error())
			return true, nil
		}
		return false, resizeErr
	}

	simpleMsg, detailedMsg := volumeToMount.GenerateMsg("MountVolume.NodeExpandVolume succeeded", nodeName)
	og.recorder.Eventf(volumeToMount.Pod, v1.EventTypeNormal, kevents.FileSystemResizeSuccess, simpleMsg)
	og.recorder.Eventf(pvc, v1.EventTypeNormal, kevents.FileSystemResizeSuccess, simpleMsg)
	klog.InfoS(detailedMsg, "pod", klog.KObj(volumeToMount.Pod))

	// if PVC already has new size, there is no need to update it.
	if pvcStatusCap.Cmp(rsOpts.NewSize) >= 0 {
		return true, nil
	}

	// File system resize succeeded, now update the PVC's Capacity to match the PV's
	_, err = util.MarkFSResizeFinished(pvc, rsOpts.NewSize, og.kubeClient)
	if err != nil {
		// On retry, NodeExpandVolume will be called again but do nothing
		return false, fmt.Errorf("mountVolume.NodeExpandVolume update PVC status failed : %v", err)
	}
	return true, nil
}

func checkMountOptionSupport(og *operationGenerator, volumeToMount VolumeToMount, plugin volume.VolumePlugin) error {
	mountOptions := util.MountOptionFromSpec(volumeToMount.VolumeSpec)

	if len(mountOptions) > 0 && !plugin.SupportsMountOption() {
		return fmt.Errorf("mount options are not supported for this volume type")
	}
	return nil
}

// checkNodeAffinity looks at the PV node affinity, and checks if the node has the same corresponding labels
// This ensures that we don't mount a volume that doesn't belong to this node
func checkNodeAffinity(og *operationGenerator, volumeToMount VolumeToMount) error {
	pv := volumeToMount.VolumeSpec.PersistentVolume
	if pv != nil {
		nodeLabels, err := og.volumePluginMgr.Host.GetNodeLabels()
		if err != nil {
			return err
		}
		err = storagehelpers.CheckNodeAffinity(pv, nodeLabels)
		if err != nil {
			return err
		}
	}
	return nil
}

// isDeviceOpened checks the device status if the device is in use anywhere else on the system
func isDeviceOpened(deviceToDetach AttachedVolume, hostUtil hostutil.HostUtils) (bool, error) {
	isDevicePath, devicePathErr := hostUtil.PathIsDevice(deviceToDetach.DevicePath)
	var deviceOpened bool
	var deviceOpenedErr error
	if !isDevicePath && devicePathErr == nil ||
		(devicePathErr != nil && strings.Contains(devicePathErr.Error(), "does not exist")) {
		// not a device path or path doesn't exist
		//TODO: refer to #36092
		klog.V(3).Infof("The path isn't device path or doesn't exist. Skip checking device path: %s", deviceToDetach.DevicePath)
		deviceOpened = false
	} else if devicePathErr != nil {
		return false, deviceToDetach.GenerateErrorDetailed("PathIsDevice failed", devicePathErr)
	} else {
		deviceOpened, deviceOpenedErr = hostUtil.DeviceOpened(deviceToDetach.DevicePath)
		if deviceOpenedErr != nil {
			return false, deviceToDetach.GenerateErrorDetailed("DeviceOpened failed", deviceOpenedErr)
		}
	}
	return deviceOpened, nil
}

// findDetachablePluginBySpec is a variant of VolumePluginMgr.FindAttachablePluginByName() function.
// The difference is that it bypass the CanAttach() check for CSI plugin, i.e. it assumes all CSI plugin supports detach.
// The intention here is that a CSI plugin volume can end up in an Uncertain state,  so that a detach
// operation will help it to detach no matter it actually has the ability to attach/detach.
func findDetachablePluginBySpec(spec *volume.Spec, pm *volume.VolumePluginMgr) (volume.AttachableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if attachableVolumePlugin, ok := volumePlugin.(volume.AttachableVolumePlugin); ok {
		if attachableVolumePlugin.GetPluginName() == "kubernetes.io/csi" {
			return attachableVolumePlugin, nil
		}
		if canAttach, err := attachableVolumePlugin.CanAttach(spec); err != nil {
			return nil, err
		} else if canAttach {
			return attachableVolumePlugin, nil
		}
	}
	return nil, nil
}

func getMigratedStatusBySpec(spec *volume.Spec) bool {
	migrated := false
	if spec != nil {
		migrated = spec.Migrated
	}
	return migrated
}
