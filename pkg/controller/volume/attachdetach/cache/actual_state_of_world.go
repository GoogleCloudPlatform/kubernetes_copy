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
Package cache implements data structures used by the attach/detach controller
to keep track of volumes, the nodes they are attached to, and the pods that
reference them.
*/
package cache

import (
	"errors"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
)

// ActualStateOfWorld defines a set of thread-safe operations supported on
// the attach/detach controller's actual state of the world cache.
// This cache contains volumes->nodes i.e. a set of all volumes and the nodes
// the attach/detach controller believes are successfully attached.
// Note: This is distinct from the ActualStateOfWorld implemented by the kubelet
// volume manager. They both keep track of different objects. This contains
// attach/detach controller specific state.
type ActualStateOfWorld interface {
	// ActualStateOfWorld must implement the methods required to allow
	// operationexecutor to interact with it.
	operationexecutor.ActualStateOfWorldAttacherUpdater

	// AddVolumeNode adds the given volume and node to the underlying store.
	// If attached is set to true, it indicates the specified volume is already
	// attached to the specified node. If attached set to false, it means that
	// the volume is not confirmed to be attached to the node yet.
	// A unique volume name is generated from the volumeSpec and returned on
	// success.
	// If volumeSpec is not an attachable volume plugin, an error is returned.
	// If no volume with the name volumeName exists in the store, the volume is
	// added.
	// If no node with the name nodeName exists in list of attached nodes for
	// the specified volume, the node is added.
	AddVolumeNode(logger klog.Logger, uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string, attached bool) (v1.UniqueVolumeName, error)

	// SetVolumesMountedByNode sets all the volumes mounted by the given node.
	// These volumes should include attached volumes, not-yet-attached volumes,
	// and may also include non-attachable volumes.
	// When present in the volumeNames parameter, the volume
	// is mounted by the given node, indicating it may not be safe to detach.
	// Otherwise, the volume is not mounted by the given node.
	SetVolumesMountedByNode(logger klog.Logger, volumeNames []v1.UniqueVolumeName, nodeName types.NodeName)

	// SetNodeStatusUpdateNeeded sets statusUpdateNeeded for the specified
	// node to true indicating the AttachedVolume field in the Node's Status
	// object needs to be updated by the node updater again.
	// If the specified node does not exist in the nodesToUpdateStatusFor list,
	// log the error and return
	SetNodeStatusUpdateNeeded(logger klog.Logger, nodeName types.NodeName)

	// ResetDetachRequestTime resets the detachRequestTime to 0 which indicates there is no detach
	// request any more for the volume
	ResetDetachRequestTime(logger klog.Logger, volumeName v1.UniqueVolumeName, nodeName types.NodeName)

	// SetDetachRequestTime sets the detachRequestedTime to current time if this is no
	// previous request (the previous detachRequestedTime is zero) and return the time elapsed
	// since last request
	SetDetachRequestTime(logger klog.Logger, volumeName v1.UniqueVolumeName, nodeName types.NodeName) (time.Duration, error)

	// DeleteVolumeNode removes the given volume and node from the underlying
	// store indicating the specified volume is no longer attached to the
	// specified node.
	// If the volume/node combo does not exist, this is a no-op.
	// If after deleting the node, the specified volume contains no other child
	// nodes, the volume is also deleted.
	DeleteVolumeNode(volumeName v1.UniqueVolumeName, nodeName types.NodeName)

	// GetAttachState returns the attach state for the given volume-node
	// combination.
	// Returns AttachStateAttached if the specified volume/node combo exists in
	// the underlying store indicating the specified volume is attached to the
	// specified node, AttachStateDetached if the combo does not exist, or
	// AttachStateUncertain if the attached state is marked as uncertain.
	GetAttachState(volumeName v1.UniqueVolumeName, nodeName types.NodeName) AttachState

	// GetAttachedVolumes generates and returns a list of volumes/node pairs
	// reflecting which volumes might attached to which nodes based on the
	// current actual state of the world. This list includes all the volumes which return successful
	// attach and also the volumes which return errors during attach.
	GetAttachedVolumes() []AttachedVolume

	// GetAttachedVolumesForNode generates and returns a list of volumes that added to
	// the specified node reflecting which volumes are/or might be attached to that node
	// based on the current actual state of the world. This function is currently used by
	// attach_detach_controller to process VolumeInUse
	GetAttachedVolumesForNode(nodeName types.NodeName) []AttachedVolume

	// GetAttachedVolumesPerNode generates and returns a map of nodes and volumes that added to
	// the specified node reflecting which volumes are attached to that node
	// based on the current actual state of the world. This function is currently used by
	// reconciler to verify whether the volume is still attached to the node.
	GetAttachedVolumesPerNode() map[types.NodeName][]operationexecutor.AttachedVolume

	// GetPossiblyAttachedNodesForVolume returns the nodes on which the volume is possibly attached.
	// This function is used by reconciler for multi-attach check.
	GetPossiblyAttachedNodesForVolume(volumeName v1.UniqueVolumeName) []types.NodeName

	// GetVolumesToReportAttached returns a map containing the set of nodes for
	// which the VolumesAttached Status field in the Node API object should be
	// updated. The key in this map is the name of the node to update and the
	// value is list of volumes that should be reported as attached (note that
	// this may differ from the actual list of attached volumes for the node
	// since volumes should be removed from this list as soon a detach operation
	// is considered, before the detach operation is triggered).
	GetVolumesToReportAttached(logger klog.Logger) map[types.NodeName][]v1.AttachedVolume

	// GetVolumesToReportAttachedForNode returns the list of volumes that should be reported as
	// attached for the given node. It reports a boolean indicating if there is an update for that
	// node and the corresponding attachedVolumes list.
	GetVolumesToReportAttachedForNode(logger klog.Logger, name types.NodeName) (bool, []v1.AttachedVolume)

	// GetNodesToUpdateStatusFor returns the map of nodeNames to nodeToUpdateStatusFor
	GetNodesToUpdateStatusFor() map[types.NodeName]nodeToUpdateStatusFor
}

// AttachedVolume represents a volume that is attached to a node.
type AttachedVolume struct {
	operationexecutor.AttachedVolume

	// MountedByNode indicates that this volume has been mounted by the node and
	// is unsafe to detach.
	// The value is set and unset by SetVolumesMountedByNode(...).
	MountedByNode bool

	// DetachRequestedTime is used to capture the desire to detach this volume.
	// When the volume is newly created this value is set to time zero.
	// It is set to current time, when SetDetachRequestTime(...) is called, if it
	// was previously set to zero (other wise its value remains the same).
	// It is reset to zero on ResetDetachRequestTime(...) calls.
	DetachRequestedTime time.Time
}

// AttachState represents the attach state of a volume to a node known to the
// Actual State of World.
// This type is used as external representation of attach state (specifically
// as the return type of GetAttachState only); the state is represented
// differently in the internal cache implementation.
type AttachState int

const (
	// AttachStateAttached represents the state in which the volume is attached to
	// the node.
	AttachStateAttached AttachState = iota

	// AttachStateUncertain represents the state in which the Actual State of World
	// does not know whether the volume is attached to the node.
	AttachStateUncertain

	// AttachStateDetached represents the state in which the volume is not
	// attached to the node.
	AttachStateDetached
)

func (s AttachState) String() string {
	return []string{"Attached", "Uncertain", "Detached"}[s]
}

// NewActualStateOfWorld returns a new instance of ActualStateOfWorld.
func NewActualStateOfWorld(volumePluginMgr *volume.VolumePluginMgr) ActualStateOfWorld {
	return &actualStateOfWorld{
		attachedVolumes:        make(map[v1.UniqueVolumeName]attachedVolume),
		nodesToUpdateStatusFor: make(map[types.NodeName]nodeToUpdateStatusFor),
		inUseVolumes:           make(map[types.NodeName]sets.Set[v1.UniqueVolumeName]),
		volumePluginMgr:        volumePluginMgr,
	}
}

type actualStateOfWorld struct {
	// attachedVolumes is a map containing the set of volumes the attach/detach
	// controller believes to be successfully attached to the nodes it is
	// managing. The key in this map is the name of the volume and the value is
	// an object containing more information about the attached volume.
	attachedVolumes map[v1.UniqueVolumeName]attachedVolume

	// nodesToUpdateStatusFor is a map containing the set of nodes for which to
	// update the VolumesAttached Status field. The key in this map is the name
	// of the node and the value is an object containing more information about
	// the node (including the list of volumes to report attached).
	nodesToUpdateStatusFor map[types.NodeName]nodeToUpdateStatusFor

	// inUseVolumes is a map containing the set of volumes that are reported as
	// in use by the kubelet.
	inUseVolumes map[types.NodeName]sets.Set[v1.UniqueVolumeName]

	// volumePluginMgr is the volume plugin manager used to create volume
	// plugin objects.
	volumePluginMgr *volume.VolumePluginMgr

	sync.RWMutex
}

// The volume object represents a volume the attach/detach controller
// believes to be successfully attached to a node it is managing.
type attachedVolume struct {
	// volumeName contains the unique identifier for this volume.
	volumeName v1.UniqueVolumeName

	// spec is the volume spec containing the specification for this volume.
	// Used to generate the volume plugin object, and passed to attach/detach
	// methods.
	spec *volume.Spec

	// nodesAttachedTo is a map containing the set of nodes this volume has
	// been attached to. The key in this map is the name of the
	// node and the value is a node object containing more information about
	// the node.
	nodesAttachedTo map[types.NodeName]nodeAttachedTo

	// devicePath contains the path on the node where the volume is attached
	devicePath string
}

// The nodeAttachedTo object represents a node that has volumes attached to it
// or trying to attach to it.
type nodeAttachedTo struct {
	// nodeName contains the name of this node.
	nodeName types.NodeName

	// attachConfirmed indicates that the storage system verified the volume has been attached to this node.
	// This value is set to false when an attach  operation fails and the volume may be attached or not.
	attachedConfirmed bool

	// detachRequestedTime used to capture the desire to detach this volume
	detachRequestedTime time.Time
}

// nodeToUpdateStatusFor is an object that reflects a node that has one or more
// volume attached. It keeps track of the volumes that should be reported as
// attached in the Node's Status API object.
type nodeToUpdateStatusFor struct {
	// nodeName contains the name of this node.
	nodeName types.NodeName

	// statusUpdateNeeded indicates that the value of the VolumesAttached field
	// in the Node's Status API object should be updated. This should be set to
	// true whenever a volume is added or deleted from
	// volumesToReportAsAttached. It should be reset whenever the status is
	// updated.
	statusUpdateNeeded bool

	// volumesToReportAsAttached is the list of volumes that should be reported
	// as attached in the Node's status (note that this may differ from the
	// actual list of attached volumes since volumes should be removed from this
	// list as soon a detach operation is considered, before the detach
	// operation is triggered).
	volumesToReportAsAttached map[v1.UniqueVolumeName]v1.UniqueVolumeName
}

func (asw *actualStateOfWorld) MarkVolumeAsUncertain(
	logger klog.Logger,
	uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName) error {

	_, err := asw.AddVolumeNode(logger, uniqueName, volumeSpec, nodeName, "", false /* isAttached */)
	return err
}

func (asw *actualStateOfWorld) MarkVolumeAsAttached(
	logger klog.Logger,
	uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string) error {
	_, err := asw.AddVolumeNode(logger, uniqueName, volumeSpec, nodeName, devicePath, true)
	return err
}

func (asw *actualStateOfWorld) MarkVolumeAsDetached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.DeleteVolumeNode(volumeName, nodeName)
}

func (asw *actualStateOfWorld) RemoveVolumeFromReportAsAttached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) error {
	asw.Lock()
	defer asw.Unlock()
	return asw.removeVolumeFromReportAsAttached(volumeName, nodeName)
}

func (asw *actualStateOfWorld) AddVolumeToReportAsAttached(
	logger klog.Logger,
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()
	asw.addVolumeToReportAsAttached(logger, volumeName, nodeName)
}

func (asw *actualStateOfWorld) AddVolumeNode(
	logger klog.Logger,
	uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string, isAttached bool) (v1.UniqueVolumeName, error) {
	volumeName := uniqueName
	if volumeName == "" {
		if volumeSpec == nil {
			return volumeName, fmt.Errorf("volumeSpec cannot be nil if volumeName is empty")
		}
		attachableVolumePlugin, err := asw.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
		if err != nil || attachableVolumePlugin == nil {
			if attachableVolumePlugin == nil {
				err = fmt.Errorf("plugin do not support attachment")
			}
			return "", fmt.Errorf(
				"failed to get AttachablePlugin from volumeSpec for volume %q err=%v",
				volumeSpec.Name(),
				err)
		}

		volumeName, err = util.GetUniqueVolumeNameFromSpec(
			attachableVolumePlugin, volumeSpec)
		if err != nil {
			return "", fmt.Errorf(
				"failed to GetUniqueVolumeNameFromSpec for volumeSpec %q err=%v",
				volumeSpec.Name(),
				err)
		}
	}

	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		volumeObj = attachedVolume{
			volumeName:      volumeName,
			spec:            volumeSpec,
			nodesAttachedTo: make(map[types.NodeName]nodeAttachedTo),
			devicePath:      devicePath,
		}
	} else {
		// If volume object already exists, it indicates that the information would be out of date.
		// Update the fields for volume object except the nodes attached to the volumes.
		volumeObj.devicePath = devicePath
		volumeObj.spec = volumeSpec
		logger.V(2).Info("Volume is already added to attachedVolume list to node, update device path",
			"volumeName", volumeName,
			"node", klog.KRef("", string(nodeName)),
			"devicePath", devicePath)
	}
	node, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if !nodeExists {
		// Create object if it doesn't exist.
		node = nodeAttachedTo{
			nodeName:            nodeName,
			attachedConfirmed:   isAttached,
			detachRequestedTime: time.Time{},
		}
		// Assume mounted, until proven otherwise
		if asw.inUseVolumes[nodeName] == nil {
			asw.inUseVolumes[nodeName] = sets.New(volumeName)
		} else {
			asw.inUseVolumes[nodeName].Insert(volumeName)
		}
	} else {
		node.attachedConfirmed = isAttached
		logger.V(5).Info("Volume is already added to attachedVolume list to the node",
			"volumeName", volumeName,
			"node", klog.KRef("", string(nodeName)),
			"currentAttachState", isAttached)
	}

	volumeObj.nodesAttachedTo[nodeName] = node
	asw.attachedVolumes[volumeName] = volumeObj

	if isAttached {
		asw.addVolumeToReportAsAttached(logger, volumeName, nodeName)
	}
	return volumeName, nil
}

func (asw *actualStateOfWorld) SetVolumesMountedByNode(
	logger klog.Logger, volumeNames []v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()

	asw.inUseVolumes[nodeName] = sets.New(volumeNames...)
	logger.V(5).Info("SetVolumesMountedByNode volume to the node",
		"node", klog.KRef("", string(nodeName)),
		"volumeNames", volumeNames)
}

func (asw *actualStateOfWorld) ResetDetachRequestTime(
	logger klog.Logger,
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, nodeObj, err := asw.getNodeAndVolume(volumeName, nodeName)
	if err != nil {
		logger.Error(err, "Failed to ResetDetachRequestTime with error")
		return
	}
	nodeObj.detachRequestedTime = time.Time{}
	volumeObj.nodesAttachedTo[nodeName] = nodeObj
}

func (asw *actualStateOfWorld) SetDetachRequestTime(
	logger klog.Logger,
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) (time.Duration, error) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, nodeObj, err := asw.getNodeAndVolume(volumeName, nodeName)
	if err != nil {
		return 0, fmt.Errorf("failed to set detach request time with error: %v", err)
	}
	// If there is no previous detach request, set it to the current time
	if nodeObj.detachRequestedTime.IsZero() {
		nodeObj.detachRequestedTime = time.Now()
		volumeObj.nodesAttachedTo[nodeName] = nodeObj
		logger.V(4).Info("Set detach request time to current time for volume on node",
			"node", klog.KRef("", string(nodeName)),
			"volumeName", volumeName)
	}
	return time.Since(nodeObj.detachRequestedTime), nil
}

// Get the volume and node object from actual state of world
// This is an internal function and caller should acquire and release the lock
//
// Note that this returns disconnected objects, so if you change the volume object you must set it back with
// `asw.attachedVolumes[volumeName]=volumeObj`.
//
// If you change the node object you must use `volumeObj.nodesAttachedTo[nodeName] = nodeObj`
// This is correct, because if volumeObj is empty this function returns an error, and nodesAttachedTo
// map is a reference type, and thus mutating the copy changes the original map.
func (asw *actualStateOfWorld) getNodeAndVolume(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) (attachedVolume, nodeAttachedTo, error) {

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if volumeExists {
		nodeObj, nodeExists := volumeObj.nodesAttachedTo[nodeName]
		if nodeExists {
			return volumeObj, nodeObj, nil
		}
	}

	return attachedVolume{}, nodeAttachedTo{}, fmt.Errorf("volume %v is no longer attached to the node %q",
		volumeName,
		nodeName)
}

// Remove the volumeName from the node's volumesToReportAsAttached list
// This is an internal function and caller should acquire and release the lock
func (asw *actualStateOfWorld) removeVolumeFromReportAsAttached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) error {

	nodeToUpdate, nodeToUpdateExists := asw.nodesToUpdateStatusFor[nodeName]
	if nodeToUpdateExists {
		_, nodeToUpdateVolumeExists :=
			nodeToUpdate.volumesToReportAsAttached[volumeName]
		if nodeToUpdateVolumeExists {
			nodeToUpdate.statusUpdateNeeded = true
			delete(nodeToUpdate.volumesToReportAsAttached, volumeName)
			asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate
			return nil
		}
	}
	return fmt.Errorf("volume %q does not exist in volumesToReportAsAttached list or node %q does not exist in nodesToUpdateStatusFor list",
		volumeName,
		nodeName)

}

// Add the volumeName to the node's volumesToReportAsAttached list
// This is an internal function and caller should acquire and release the lock
func (asw *actualStateOfWorld) addVolumeToReportAsAttached(
	logger klog.Logger, volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	// In case the volume/node entry is no longer in attachedVolume list, skip the rest
	if _, _, err := asw.getNodeAndVolume(volumeName, nodeName); err != nil {
		logger.V(4).Info("Volume is no longer attached to node", "node", klog.KRef("", string(nodeName)), "volumeName", volumeName)
		return
	}
	nodeToUpdate, nodeToUpdateExists := asw.nodesToUpdateStatusFor[nodeName]
	if !nodeToUpdateExists {
		// Create object if it doesn't exist
		nodeToUpdate = nodeToUpdateStatusFor{
			nodeName:                  nodeName,
			statusUpdateNeeded:        true,
			volumesToReportAsAttached: make(map[v1.UniqueVolumeName]v1.UniqueVolumeName),
		}
		asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate
		logger.V(4).Info("Add new node to nodesToUpdateStatusFor", "node", klog.KRef("", string(nodeName)))
	}
	_, nodeToUpdateVolumeExists :=
		nodeToUpdate.volumesToReportAsAttached[volumeName]
	if !nodeToUpdateVolumeExists {
		nodeToUpdate.statusUpdateNeeded = true
		nodeToUpdate.volumesToReportAsAttached[volumeName] = volumeName
		asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate
		logger.V(4).Info("Report volume as attached to node", "node", klog.KRef("", string(nodeName)), "volumeName", volumeName)
	}
}

// Update the flag statusUpdateNeeded to indicate whether node status is already updated or
// needs to be updated again by the node status updater.
// If the specified node does not exist in the nodesToUpdateStatusFor list, log the error and return
// This is an internal function and caller should acquire and release the lock
func (asw *actualStateOfWorld) updateNodeStatusUpdateNeeded(nodeName types.NodeName, needed bool) error {
	nodeToUpdate, nodeToUpdateExists := asw.nodesToUpdateStatusFor[nodeName]
	if !nodeToUpdateExists {
		// should not happen
		errMsg := fmt.Sprintf("Failed to set statusUpdateNeeded to needed %t, because nodeName=%q does not exist",
			needed, nodeName)
		return errors.New(errMsg)
	}

	nodeToUpdate.statusUpdateNeeded = needed
	asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate

	return nil
}

func (asw *actualStateOfWorld) SetNodeStatusUpdateNeeded(logger klog.Logger, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()
	if err := asw.updateNodeStatusUpdateNeeded(nodeName, true); err != nil {
		logger.Info("Failed to update statusUpdateNeeded field in actual state of world", "err", err)
	}
}

func (asw *actualStateOfWorld) DeleteVolumeNode(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return
	}

	_, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if nodeExists {
		delete(asw.attachedVolumes[volumeName].nodesAttachedTo, nodeName)
	}

	if len(volumeObj.nodesAttachedTo) == 0 {
		delete(asw.attachedVolumes, volumeName)
	}

	// Remove volume from volumes to report as attached
	asw.removeVolumeFromReportAsAttached(volumeName, nodeName)
}

func (asw *actualStateOfWorld) GetAttachState(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) AttachState {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if volumeExists {
		if node, nodeExists := volumeObj.nodesAttachedTo[nodeName]; nodeExists {
			if node.attachedConfirmed {
				return AttachStateAttached
			}
			return AttachStateUncertain
		}
	}

	return AttachStateDetached
}

// SetVolumeClaimSize sets size of the volume. But this function should not be used from attach_detach controller.
func (asw *actualStateOfWorld) InitializeClaimSize(logger klog.Logger, volumeName v1.UniqueVolumeName, claimSize resource.Quantity) {
	logger.V(5).Info("no-op InitializeClaimSize call in attach-detach controller")
}

func (asw *actualStateOfWorld) GetClaimSize(volumeName v1.UniqueVolumeName) resource.Quantity {
	// not needed in attach-detach controller
	return resource.Quantity{}
}

func (asw *actualStateOfWorld) GetAttachedVolumes() []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumes := make([]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		for _, nodeObj := range volumeObj.nodesAttachedTo {
			attachedVolumes = append(
				attachedVolumes,
				asw.getAttachedVolume(&volumeObj, &nodeObj))
		}
	}

	return attachedVolumes
}

func (asw *actualStateOfWorld) GetAttachedVolumesForNode(
	nodeName types.NodeName) []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumes := make(
		[]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		if nodeObj, nodeExists := volumeObj.nodesAttachedTo[nodeName]; nodeExists {
			attachedVolumes = append(
				attachedVolumes,
				asw.getAttachedVolume(&volumeObj, &nodeObj))
		}
	}

	return attachedVolumes
}

func (asw *actualStateOfWorld) GetAttachedVolumesPerNode() map[types.NodeName][]operationexecutor.AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumesPerNode := make(map[types.NodeName][]operationexecutor.AttachedVolume)
	for _, volumeObj := range asw.attachedVolumes {
		for nodeName, nodeObj := range volumeObj.nodesAttachedTo {
			if nodeObj.attachedConfirmed {
				volumes := attachedVolumesPerNode[nodeName]
				volumes = append(volumes, asw.getAttachedVolume(&volumeObj, &nodeObj).AttachedVolume)
				attachedVolumesPerNode[nodeName] = volumes
			}
		}
	}

	return attachedVolumesPerNode
}

func (asw *actualStateOfWorld) GetPossiblyAttachedNodesForVolume(volumeName v1.UniqueVolumeName) []types.NodeName {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists || len(volumeObj.nodesAttachedTo) == 0 {
		return []types.NodeName{}
	}

	nodes := []types.NodeName{}
	for nodeName := range volumeObj.nodesAttachedTo {
		nodes = append(nodes, nodeName)
	}
	return nodes
}

func (asw *actualStateOfWorld) GetVolumesToReportAttached(logger klog.Logger) map[types.NodeName][]v1.AttachedVolume {
	asw.Lock()
	defer asw.Unlock()

	volumesToReportAttached := make(map[types.NodeName][]v1.AttachedVolume)
	for nodeName, nodeToUpdateObj := range asw.nodesToUpdateStatusFor {
		if nodeToUpdateObj.statusUpdateNeeded {
			volumesToReportAttached[nodeToUpdateObj.nodeName] = asw.getAttachedVolumeFromUpdateObject(nodeToUpdateObj.volumesToReportAsAttached)
		}
		// When GetVolumesToReportAttached is called by node status updater, the current status
		// of this node will be updated, so set the flag statusUpdateNeeded to false indicating
		// the current status is already updated.
		if err := asw.updateNodeStatusUpdateNeeded(nodeName, false); err != nil {
			logger.Error(err, "Failed to update statusUpdateNeeded field when getting volumes")
		}
	}

	return volumesToReportAttached
}

func (asw *actualStateOfWorld) GetVolumesToReportAttachedForNode(logger klog.Logger, nodeName types.NodeName) (bool, []v1.AttachedVolume) {
	asw.Lock()
	defer asw.Unlock()

	nodeToUpdateObj, ok := asw.nodesToUpdateStatusFor[nodeName]
	if !ok {
		return false, nil
	}
	if !nodeToUpdateObj.statusUpdateNeeded {
		return false, nil
	}

	volumesToReportAttached := asw.getAttachedVolumeFromUpdateObject(nodeToUpdateObj.volumesToReportAsAttached)
	// When GetVolumesToReportAttached is called by node status updater, the current status
	// of this node will be updated, so set the flag statusUpdateNeeded to false indicating
	// the current status is already updated.
	if err := asw.updateNodeStatusUpdateNeeded(nodeName, false); err != nil {
		logger.Error(err, "Failed to update statusUpdateNeeded field when getting volumes")
	}

	return true, volumesToReportAttached
}

func (asw *actualStateOfWorld) GetNodesToUpdateStatusFor() map[types.NodeName]nodeToUpdateStatusFor {
	return asw.nodesToUpdateStatusFor
}

func (asw *actualStateOfWorld) getAttachedVolumeFromUpdateObject(volumesToReportAsAttached map[v1.UniqueVolumeName]v1.UniqueVolumeName) []v1.AttachedVolume {
	var attachedVolumes = make(
		[]v1.AttachedVolume,
		0,
		len(volumesToReportAsAttached) /* len */)
	for _, volume := range volumesToReportAsAttached {
		attachedVolumes = append(attachedVolumes,
			v1.AttachedVolume{
				Name:       volume,
				DevicePath: asw.attachedVolumes[volume].devicePath,
			})
	}
	return attachedVolumes
}

func (asw *actualStateOfWorld) getAttachedVolume(
	attachedVolume *attachedVolume,
	nodeAttachedTo *nodeAttachedTo) AttachedVolume {
	return AttachedVolume{
		AttachedVolume: operationexecutor.AttachedVolume{
			VolumeName:         attachedVolume.volumeName,
			VolumeSpec:         attachedVolume.spec,
			NodeName:           nodeAttachedTo.nodeName,
			DevicePath:         attachedVolume.devicePath,
			PluginIsAttachable: true,
		},
		MountedByNode:       asw.inUseVolumes[nodeAttachedTo.nodeName].Has(attachedVolume.volumeName),
		DetachRequestedTime: nodeAttachedTo.detachRequestedTime}
}
