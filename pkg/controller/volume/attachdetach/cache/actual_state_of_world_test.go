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

package cache

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/ktesting"
	controllervolumetesting "k8s.io/kubernetes/pkg/controller/volume/attachdetach/testing"
	volumetesting "k8s.io/kubernetes/pkg/volume/testing"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

// Calls AddVolumeNode() once with attached set to true.
// Verifies a single volume/node entry exists.
func Test_AddVolumeNode_Positive_NewVolumeNewNode(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)

	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"

	// Act
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, err := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)

	// Assert
	if err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", err)
	}

	volumeNodeComboState := asw.GetAttachState(generatedVolumeName, nodeName)
	if volumeNodeComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName, nodeName, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Calls AddVolumeNode() once with attached set to false.
// Verifies a single volume/node entry exists.
// Then calls AddVolumeNode() with attached set to true
// Verifies volume is attached to the node according to asw.
func Test_AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)

	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"

	// Act
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, err := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, false)

	// Assert
	if err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", err)
	}

	volumeNodeComboState := asw.GetAttachState(generatedVolumeName, nodeName)
	if volumeNodeComboState != AttachStateUncertain {
		t.Fatalf("%q/%q volume/node combo is marked %q, expected 'Uncertain'.", generatedVolumeName, nodeName, volumeNodeComboState)
	}

	allVolumes := asw.GetAttachedVolumes()
	if len(allVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(allVolumes))
	}
	verifyAttachedVolume(t, allVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)

	reportAsAttachedVolumesMap := asw.GetVolumesToReportAttached(logger)
	_, exists := reportAsAttachedVolumesMap[nodeName]
	if exists {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Actual: <node %q exist> Expect: <node does not exist in the reportedAsAttached map", nodeName)
	}

	volumesForNode := asw.GetAttachedVolumesForNode(nodeName)
	if len(volumesForNode) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(volumesForNode))
	}
	verifyAttachedVolume(t, volumesForNode, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)

	attachedVolumesMap := asw.GetAttachedVolumesPerNode()
	_, exists = attachedVolumesMap[nodeName]
	if exists {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Actual: <node %q exist> Expect: <node does not exist in the reportedAsAttached map", nodeName)
	}

	nodes := asw.GetNodesForAttachedVolume(volumeName)
	if len(nodes) > 0 {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Expect no nodes returned.")
	}

	// Add the volume to the node second time with attached set to true
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)

	// Assert
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	if generatedVolumeName != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName,
			generatedVolumeName2)
	}

	volumeNodeComboState = asw.GetAttachState(generatedVolumeName, nodeName)
	if volumeNodeComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName, nodeName, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)

	nodes = asw.GetNodesForAttachedVolume(volumeName)
	if len(nodes) != 1 {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Expect one node returned.")
	}
	if nodes[0] != nodeName {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Expect node %v, Actual node %v", nodeName, nodes[0])
	}

	attachedVolumesMap = asw.GetAttachedVolumesPerNode()
	_, exists = attachedVolumesMap[nodeName]
	if !exists {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Actual: <node %q does not exist> Expect: <node does exist in the reportedAsAttached map", nodeName)
	}

}

// Calls AddVolumeNode() once with attached set to false.
// Verifies a single volume/node entry exists.
// Then calls AddVolumeNode() to attach the volume to a different node with attached set to true
// Verifies volume is attached to the node according to asw.
func Test_AddVolumeNode_Positive_NewVolumeTwoNodesWithFalseAttached(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)

	node1Name := types.NodeName("node1-name")
	node2Name := types.NodeName("node2-name")
	devicePath := "fake/device/path"

	// Act
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, err := asw.AddVolumeNode(logger, volumeName, volumeSpec, node1Name, devicePath, false)

	// Assert
	if err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", err)
	}

	volumeNodeComboState := asw.GetAttachState(generatedVolumeName, node1Name)
	if volumeNodeComboState != AttachStateUncertain {
		t.Fatalf("%q/%q volume/node combo is marked %q, expected 'Uncertain'.", generatedVolumeName, node1Name, volumeNodeComboState)
	}

	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, node2Name, devicePath, true)

	// Assert
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	if generatedVolumeName != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName,
			generatedVolumeName2)
	}

	volumeNodeComboState = asw.GetAttachState(generatedVolumeName, node2Name)
	if volumeNodeComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName, node2Name, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 2 {
		t.Fatalf("len(attachedVolumes) Expected: <2> Actual: <%v>", len(attachedVolumes))
	}
	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), node1Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), node2Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)

	volumesForNode := asw.GetAttachedVolumesForNode(node2Name)
	if len(volumesForNode) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <2> Actual: <%v>", len(volumesForNode))
	}
	verifyAttachedVolume(t, volumesForNode, generatedVolumeName, string(volumeName), node2Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)

	attachedVolumesMap := asw.GetAttachedVolumesPerNode()
	attachedVolumesPerNode, exists := attachedVolumesMap[node2Name]
	if !exists || len(attachedVolumesPerNode) != 1 {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeTwoNodesWithFalseAttached failed. Actual: <node %q does not exist> Expect: <node does exist in the reportedAsAttached map", node2Name)
	}

	nodes := asw.GetNodesForAttachedVolume(volumeName)
	if len(nodes) != 1 {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Expect one node returned.")
	}

	reportAsAttachedVolumesMap := asw.GetVolumesToReportAttached(logger)
	reportedVolumes, exists := reportAsAttachedVolumesMap[node2Name]
	if !exists || len(reportedVolumes) != 1 {
		t.Fatalf("AddVolumeNode_Positive_NewVolumeNewNodeWithFalseAttached failed. Actual: <node %q exist> Expect: <node does not exist in the reportedAsAttached map", node2Name)
	}
}

// Calls AddVolumeNode() twice. Second time use a different node name.
// Verifies two volume/node entries exist with the same volumeSpec.
func Test_AddVolumeNode_Positive_ExistingVolumeNewNode(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	node1Name := types.NodeName("node1-name")
	node2Name := types.NodeName("node2-name")
	devicePath := "fake/device/path"

	// Act
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName1, add1Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, node1Name, devicePath, true)
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, node2Name, devicePath, true)

	// Assert
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	if generatedVolumeName1 != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName1,
			generatedVolumeName2)
	}

	volumeNode1ComboState := asw.GetAttachState(generatedVolumeName1, node1Name)
	if volumeNode1ComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName1, node1Name, volumeNode1ComboState)
	}

	volumeNode2ComboState := asw.GetAttachState(generatedVolumeName1, node2Name)
	if volumeNode2ComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName1, node2Name, volumeNode2ComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 2 {
		t.Fatalf("len(attachedVolumes) Expected: <2> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volumeName), node1Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volumeName), node2Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Calls AddVolumeNode() twice. Uses the same volume and node both times.
// Verifies a single volume/node entry exists.
func Test_AddVolumeNode_Positive_ExistingVolumeExistingNode(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"

	// Act
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName1, add1Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)

	// Assert
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	if generatedVolumeName1 != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName1,
			generatedVolumeName2)
	}

	volumeNodeComboState := asw.GetAttachState(generatedVolumeName1, nodeName)
	if volumeNodeComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName1, nodeName, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls DeleteVolumeNode() to delete volume/node.
// Verifies no volume/node entries exists.
func Test_DeleteVolumeNode_Positive_VolumeExistsNodeExists(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	asw.DeleteVolumeNode(generatedVolumeName, nodeName)

	// Assert
	volumeNodeComboState := asw.GetAttachState(generatedVolumeName, nodeName)
	if volumeNodeComboState != AttachStateDetached {
		t.Fatalf("%q/%q volume/node combo is marked %q, expected 'Detached'.", generatedVolumeName, nodeName, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 0 {
		t.Fatalf("len(attachedVolumes) Expected: <0> Actual: <%v>", len(attachedVolumes))
	}
}

// Calls DeleteVolumeNode() to delete volume/node on empty data struct
// Verifies no volume/node entries exists.
func Test_DeleteVolumeNode_Positive_VolumeDoesntExistNodeDoesntExist(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	nodeName := types.NodeName("node-name")

	// Act
	asw.DeleteVolumeNode(volumeName, nodeName)

	// Assert
	volumeNodeComboState := asw.GetAttachState(volumeName, nodeName)
	if volumeNodeComboState != AttachStateDetached {
		t.Fatalf("%q/%q volume/node combo is marked %q, expected 'Detached'.", volumeName, nodeName, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 0 {
		t.Fatalf("len(attachedVolumes) Expected: <0> Actual: <%v>", len(attachedVolumes))
	}
}

// Populates data struct with two volume/node entries the second one using a
// different node.
// Calls DeleteVolumeNode() to delete first volume/node.
// Verifies only second volume/node entry exists.
func Test_DeleteVolumeNode_Positive_TwoNodesOneDeleted(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	node1Name := types.NodeName("node1-name")
	node2Name := types.NodeName("node2-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName1, add1Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, node1Name, devicePath, true)
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, volumeName, volumeSpec, node2Name, devicePath, true)
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}
	if generatedVolumeName1 != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName1,
			generatedVolumeName2)
	}

	// Act
	asw.DeleteVolumeNode(generatedVolumeName1, node1Name)

	// Assert
	volumeNodeComboState := asw.GetAttachState(generatedVolumeName1, node1Name)
	if volumeNodeComboState != AttachStateDetached {
		t.Fatalf("%q/%q volume/node combo is marked %q, expected 'Detached'.", generatedVolumeName1, node1Name, volumeNodeComboState)
	}

	volumeNodeComboState = asw.GetAttachState(generatedVolumeName1, node2Name)
	if volumeNodeComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName1, node2Name, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volumeName), node2Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls GetAttachState() to verify entry.
// Verifies the populated volume/node entry exists.
func Test_VolumeNodeExists_Positive_VolumeExistsNodeExists(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	volumeNodeComboState := asw.GetAttachState(generatedVolumeName, nodeName)

	// Assert
	if volumeNodeComboState != AttachStateAttached {
		t.Fatalf("%q/%q volume/node combo is marked %q; expected 'Attached'.", generatedVolumeName, nodeName, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume1/node1 entry.
// Calls GetAttachState() with volume1/node2.
// Verifies requested entry does not exist, but populated entry does.
func Test_VolumeNodeExists_Positive_VolumeExistsNodeDoesntExist(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	node1Name := types.NodeName("node1-name")
	node2Name := types.NodeName("node2-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, node1Name, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	volumeNodeComboState := asw.GetAttachState(generatedVolumeName, node2Name)

	// Assert
	if volumeNodeComboState != AttachStateDetached {
		t.Fatalf("%q/%q volume/node combo is marked %q, expected 'Detached'.", generatedVolumeName, node2Name, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), node1Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Calls GetAttachState() on empty data struct.
// Verifies requested entry does not exist.
func Test_VolumeNodeExists_Positive_VolumeAndNodeDontExist(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	nodeName := types.NodeName("node-name")

	// Act
	volumeNodeComboState := asw.GetAttachState(volumeName, nodeName)

	// Assert
	if volumeNodeComboState != AttachStateDetached {
		t.Fatalf("%q/%q volume/node combo is marked %q, expected 'Detached'.", volumeName, nodeName, volumeNodeComboState)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 0 {
		t.Fatalf("len(attachedVolumes) Expected: <0> Actual: <%v>", len(attachedVolumes))
	}
}

// Calls GetAttachedVolumes() on empty data struct.
// Verifies no volume/node entries are returned.
func Test_GetAttachedVolumes_Positive_NoVolumesOrNodes(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)

	// Act
	attachedVolumes := asw.GetAttachedVolumes()

	// Assert
	if len(attachedVolumes) != 0 {
		t.Fatalf("len(attachedVolumes) Expected: <0> Actual: <%v>", len(attachedVolumes))
	}
}

// Populates data struct with one volume/node entry.
// Calls GetAttachedVolumes() to get list of entries.
// Verifies one volume/node entry is returned.
func Test_GetAttachedVolumes_Positive_OneVolumeOneNode(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	attachedVolumes := asw.GetAttachedVolumes()

	// Assert
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with two volume/node entries (different node and volume).
// Calls GetAttachedVolumes() to get list of entries.
// Verifies both volume/node entries are returned.
func Test_GetAttachedVolumes_Positive_TwoVolumeTwoNodes(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volume1Name := v1.UniqueVolumeName("volume1-name")
	volume1Spec := controllervolumetesting.GetTestVolumeSpec(string(volume1Name), volume1Name)
	node1Name := types.NodeName("node1-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName1, add1Err := asw.AddVolumeNode(logger, volume1Name, volume1Spec, node1Name, devicePath, true)
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	volume2Name := v1.UniqueVolumeName("volume2-name")
	volume2Spec := controllervolumetesting.GetTestVolumeSpec(string(volume2Name), volume2Name)
	node2Name := types.NodeName("node2-name")
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, volume2Name, volume2Spec, node2Name, devicePath, true)
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	// Act
	attachedVolumes := asw.GetAttachedVolumes()

	// Assert
	if len(attachedVolumes) != 2 {
		t.Fatalf("len(attachedVolumes) Expected: <2> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volume1Name), node1Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName2, string(volume2Name), node2Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with two volume/node entries (same volume different node).
// Calls GetAttachedVolumes() to get list of entries.
// Verifies both volume/node entries are returned.
func Test_GetAttachedVolumes_Positive_OneVolumeTwoNodes(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	node1Name := types.NodeName("node1-name")
	devicePath := "fake/device/path"
	plugin, err := volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get volume plugin from spec %v, %v", volumeSpec, err)
	}
	uniqueVolumeName, err := volumeutil.GetUniqueVolumeNameFromSpec(plugin, volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get uniqueVolumeName from spec %v, %v", volumeSpec, err)
	}
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName1, add1Err := asw.AddVolumeNode(logger, uniqueVolumeName, volumeSpec, node1Name, devicePath, true)
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	node2Name := types.NodeName("node2-name")
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, v1.UniqueVolumeName(""), volumeSpec, node2Name, devicePath, true)
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	if generatedVolumeName1 != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName1,
			generatedVolumeName2)
	}

	// Act
	attachedVolumes := asw.GetAttachedVolumes()

	// Assert
	if len(attachedVolumes) != 2 {
		t.Fatalf("len(attachedVolumes) Expected: <2> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volumeName), node1Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volumeName), node2Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Verifies mountedByNode is true and DetachRequestedTime is zero.
func Test_SetVolumesMountedByNode_Positive_Set(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act: do not mark -- test default value

	// Assert
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls SetVolumesMountedByNode twice, first setting mounted to true then false.
// Verifies mountedByNode is false.
func Test_SetVolumesMountedByNode_Positive_UnsetWithInitialSet(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	asw.SetVolumesMountedByNode(logger, []v1.UniqueVolumeName{generatedVolumeName}, nodeName)
	asw.SetVolumesMountedByNode(logger, nil, nodeName)

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, false /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls SetVolumesMountedByNode once, setting mounted to false.
// Verifies mountedByNode is false because value is overwritten
func Test_SetVolumesMountedByNode_Positive_UnsetWithoutInitialSet(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)

	// Act
	asw.SetVolumesMountedByNode(logger, nil, nodeName)

	// Assert
	attachedVolumes = asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, false /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls SetVolumesMountedByNode twice, first setting mounted to true then false.
// Calls AddVolumeNode to readd the same volume/node.
// Verifies mountedByNode is false and detachRequestedTime is zero.
func Test_SetVolumesMountedByNode_Positive_UnsetWithInitialSetAddVolumeNodeNotReset(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	asw.SetVolumesMountedByNode(logger, []v1.UniqueVolumeName{generatedVolumeName}, nodeName)
	asw.SetVolumesMountedByNode(logger, nil, nodeName)
	generatedVolumeName, addErr = asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)

	// Assert
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, false /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls RemoveVolumeFromReportAsAttached() once on volume/node entry.
// Calls SetVolumesMountedByNode() twice, first setting mounted to true then false.
// Verifies mountedByNode is false and detachRequestedTime is NOT zero.
func Test_SetVolumesMountedByNode_Positive_UnsetWithInitialSetVerifyDetachRequestedTimePreserved(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}
	_, err := asw.SetDetachRequestTime(logger, generatedVolumeName, nodeName)
	if err != nil {
		t.Fatalf("SetDetachRequestTime failed. Expected: <no error> Actual: <%v>", err)
	}
	err = asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	if err != nil {
		t.Fatalf("RemoveVolumeFromReportAsAttached failed. Expected: <no error> Actual: <%v>", err)
	}
	expectedDetachRequestedTime := asw.GetAttachedVolumes()[0].DetachRequestedTime

	// Act
	asw.SetVolumesMountedByNode(logger, []v1.UniqueVolumeName{generatedVolumeName}, nodeName)
	asw.SetVolumesMountedByNode(logger, nil, nodeName)

	// Assert
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, false /* expectedMountedByNode */, true /* expectNonZeroDetachRequestedTime */)
	if !expectedDetachRequestedTime.Equal(attachedVolumes[0].DetachRequestedTime) {
		t.Fatalf("DetachRequestedTime changed. Expected: <%v> Actual: <%v>", expectedDetachRequestedTime, attachedVolumes[0].DetachRequestedTime)
	}
}

// Populates data struct with one volume/node entry.
// Verifies mountedByNode is true and detachRequestedTime is zero (default values).
func Test_RemoveVolumeFromReportAsAttached_Positive_Set(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	devicePath := "fake/device/path"
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act: do not mark -- test default value

	// Assert
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls RemoveVolumeFromReportAsAttached() once on volume/node entry.
// Verifies mountedByNode is true and detachRequestedTime is NOT zero.
func Test_RemoveVolumeFromReportAsAttached_Positive_Marked(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	_, err := asw.SetDetachRequestTime(logger, generatedVolumeName, nodeName)
	if err != nil {
		t.Fatalf("SetDetachRequestTime failed. Expected: <no error> Actual: <%v>", err)
	}
	markDesireToDetachErr := asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	if markDesireToDetachErr != nil {
		t.Fatalf("MarkDesireToDetach failed. Expected: <no error> Actual: <%v>", markDesireToDetachErr)
	}

	// Assert
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, true /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls MarkDesireToDetach() once on volume/node entry.
// Calls ResetDetachRequestTime() to reset the detach request time value back to 0.
// Verifies mountedByNode is true and detachRequestedTime is reset to zero.
func Test_MarkDesireToDetach_Positive_MarkedAddVolumeNodeReset(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	_, err := asw.SetDetachRequestTime(logger, generatedVolumeName, nodeName)
	if err != nil {
		t.Fatalf("SetDetachRequestTime failed. Expected: <no error> Actual: <%v>", err)
	}
	markDesireToDetachErr := asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	// Reset detach request time to 0
	asw.ResetDetachRequestTime(logger, generatedVolumeName, nodeName)

	// Assert
	if markDesireToDetachErr != nil {
		t.Fatalf("RemoveVolumeFromReportAsAttached failed. Expected: <no error> Actual: <%v>", markDesireToDetachErr)
	}
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Assert
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls SetVolumesMountedByNode() twice, first setting mounted to true then false.
// Calls RemoveVolumeFromReportAsAttached() once on volume/node entry.
// Verifies mountedByNode is false and detachRequestedTime is NOT zero.
func Test_RemoveVolumeFromReportAsAttached_Positive_UnsetWithInitialSetVolumesMountedByNodePreserved(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}
	asw.SetVolumesMountedByNode(logger, []v1.UniqueVolumeName{generatedVolumeName}, nodeName)
	asw.SetVolumesMountedByNode(logger, nil, nodeName)
	// Act
	_, err := asw.SetDetachRequestTime(logger, generatedVolumeName, nodeName)
	if err != nil {
		t.Fatalf("SetDetachRequestTime failed. Expected: <no error> Actual: <%v>", err)
	}
	removeVolumeDetachErr := asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	if removeVolumeDetachErr != nil {
		t.Fatalf("RemoveVolumeFromReportAsAttached failed. Expected: <no error> Actual: <%v>", removeVolumeDetachErr)
	}

	// Assert
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, false /* expectedMountedByNode */, true /* expectNonZeroDetachRequestedTime */)
}

// Populates data struct with one volume/node entry.
// Calls RemoveVolumeFromReportAsAttached
// Verifies there is no volume as reported as attached
func Test_RemoveVolumeFromReportAsAttached(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	removeVolumeDetachErr := asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	if removeVolumeDetachErr != nil {
		t.Fatalf("RemoveVolumeFromReportAsAttached failed. Expected: <no error> Actual: <%v>", removeVolumeDetachErr)
	}

	reportAsAttachedVolumesMap := asw.GetVolumesToReportAttached(logger)
	volumes, exists := reportAsAttachedVolumesMap[nodeName]
	if !exists {
		t.Fatalf("MarkDesireToDetach_UnmarkDesireToDetach failed. Expected: <node %q exist> Actual: <node does not exist in the reportedAsAttached map", nodeName)
	}
	if len(volumes) > 0 {
		t.Fatalf("len(reportAsAttachedVolumes) Expected: <0> Actual: <%v>", len(volumes))
	}

}

// Populates data struct with one volume/node entry.
// Calls RemoveVolumeFromReportAsAttached
// Calls AddVolumeToReportAsAttached to add volume back as attached
// Verifies there is one volume as reported as attached
func Test_RemoveVolumeFromReportAsAttached_AddVolumeToReportAsAttached_Positive(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	removeVolumeDetachErr := asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	if removeVolumeDetachErr != nil {
		t.Fatalf("RemoveVolumeFromReportAsAttached failed. Expected: <no error> Actual: <%v>", removeVolumeDetachErr)
	}

	reportAsAttachedVolumesMap := asw.GetVolumesToReportAttached(logger)
	volumes, exists := reportAsAttachedVolumesMap[nodeName]
	if !exists {
		t.Fatalf("Test_RemoveVolumeFromReportAsAttached_AddVolumeToReportAsAttached_Positive failed. Expected: <node %q exist> Actual: <node does not exist in the reportedAsAttached map", nodeName)
	}
	if len(volumes) > 0 {
		t.Fatalf("len(reportAsAttachedVolumes) Expected: <0> Actual: <%v>", len(volumes))
	}

	asw.AddVolumeToReportAsAttached(logger, generatedVolumeName, nodeName)
	reportAsAttachedVolumesMap = asw.GetVolumesToReportAttached(logger)
	volumes, exists = reportAsAttachedVolumesMap[nodeName]
	if !exists {
		t.Fatalf("Test_RemoveVolumeFromReportAsAttached_AddVolumeToReportAsAttached_Positive failed. Expected: <node %q exist> Actual: <node does not exist in the reportedAsAttached map", nodeName)
	}
	if len(volumes) != 1 {
		t.Fatalf("len(reportAsAttachedVolumes) Expected: <1> Actual: <%v>", len(volumes))
	}
}

// Populates data struct with one volume/node entry.
// Calls RemoveVolumeFromReportAsAttached
// Calls DeleteVolumeNode
// Calls AddVolumeNode
// Verifies there is no volume as reported as attached
func Test_RemoveVolumeFromReportAsAttached_Delete_AddVolumeNode(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	removeVolumeDetachErr := asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	if removeVolumeDetachErr != nil {
		t.Fatalf("RemoveVolumeFromReportAsAttached failed. Expected: <no error> Actual: <%v>", removeVolumeDetachErr)
	}

	reportAsAttachedVolumesMap := asw.GetVolumesToReportAttached(logger)
	volumes, exists := reportAsAttachedVolumesMap[nodeName]
	if !exists {
		t.Fatalf("Test_RemoveVolumeFromReportAsAttached_Delete_AddVolumeNode failed. Expected: <node %q exists> Actual: <node does not exist in the reportedAsAttached map", nodeName)
	}
	if len(volumes) > 0 {
		t.Fatalf("len(reportAsAttachedVolumes) Expected: <0> Actual: <%v>", len(volumes))
	}

	asw.DeleteVolumeNode(generatedVolumeName, nodeName)

	asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, "" /*device path*/, true)

	reportAsAttachedVolumesMap = asw.GetVolumesToReportAttached(logger)
	volumes, exists = reportAsAttachedVolumesMap[nodeName]
	if !exists {
		t.Fatalf("Test_RemoveVolumeFromReportAsAttached_Delete_AddVolumeNode failed. Expected: <node %q exists> Actual: <node does not exist in the reportedAsAttached map", nodeName)
	}
	if len(volumes) != 1 {
		t.Fatalf("len(reportAsAttachedVolumes) Expected: <1> Actual: <%v>", len(volumes))
	}
}

// Populates data struct with one volume/node entry.
// Calls SetDetachRequestTime twice and sleep maxWaitTime (1 second) in between
// The elapsed time returned from the first SetDetachRequestTime call should be smaller than maxWaitTime
// The elapsed time returned from the second SetDetachRequestTime call should be larger than maxWaitTime
func Test_SetDetachRequestTime_Positive(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	maxWaitTime := 1 * time.Second
	etime, err := asw.SetDetachRequestTime(logger, generatedVolumeName, nodeName)
	if err != nil {
		t.Fatalf("SetDetachRequestTime failed. Expected: <no error> Actual: <%v>", err)
	}
	if etime >= maxWaitTime {
		t.Logf("SetDetachRequestTim Expected: <elapsed time %v is smaller than maxWaitTime %v> Actual <elapsed time is larger than maxWaitTime>", etime, maxWaitTime)
	}
	// Sleep and call SetDetachRequestTime again
	time.Sleep(maxWaitTime)
	etime, err = asw.SetDetachRequestTime(logger, generatedVolumeName, nodeName)
	if err != nil {
		t.Fatalf("SetDetachRequestTime failed. Expected: <no error> Actual: <%v>", err)
	}
	if etime < maxWaitTime {
		t.Fatalf("SetDetachRequestTim Expected: <elapsed time %v is larger than maxWaitTime %v> Actual <elapsed time is smaller>", etime, maxWaitTime)
	}
}

func Test_GetAttachedVolumesForNode_Positive_NoVolumesOrNodes(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	node := types.NodeName("random")

	// Act
	attachedVolumes := asw.GetAttachedVolumesForNode(node)

	// Assert
	if len(attachedVolumes) != 0 {
		t.Fatalf("len(attachedVolumes) Expected: <0> Actual: <%v>", len(attachedVolumes))
	}
}

func Test_GetAttachedVolumesForNode_Positive_OneVolumeOneNode(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, addErr := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)
	if addErr != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", addErr)
	}

	// Act
	attachedVolumes := asw.GetAttachedVolumesForNode(nodeName)

	// Assert
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

func Test_GetAttachedVolumesForNode_Positive_TwoVolumeTwoNodes(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volume1Name := v1.UniqueVolumeName("volume1-name")
	volume1Spec := controllervolumetesting.GetTestVolumeSpec(string(volume1Name), volume1Name)
	node1Name := types.NodeName("node1-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	_, add1Err := asw.AddVolumeNode(logger, volume1Name, volume1Spec, node1Name, devicePath, true)
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	volume2Name := v1.UniqueVolumeName("volume2-name")
	volume2Spec := controllervolumetesting.GetTestVolumeSpec(string(volume2Name), volume2Name)
	node2Name := types.NodeName("node2-name")
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, volume2Name, volume2Spec, node2Name, devicePath, true)
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	// Act
	attachedVolumes := asw.GetAttachedVolumesForNode(node2Name)

	// Assert
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName2, string(volume2Name), node2Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

func Test_GetAttachedVolumesForNode_Positive_OneVolumeTwoNodes(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	node1Name := types.NodeName("node1-name")
	devicePath := "fake/device/path"
	logger, _ := ktesting.NewTestContext(t)
	plugin, err := volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get volume plugin from spec %v, %v", volumeSpec, err)
	}
	uniqueVolumeName, err := volumeutil.GetUniqueVolumeNameFromSpec(plugin, volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get uniqueVolumeName from spec %v, %v", volumeSpec, err)
	}
	generatedVolumeName1, add1Err := asw.AddVolumeNode(logger, uniqueVolumeName, volumeSpec, node1Name, devicePath, true)
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	node2Name := types.NodeName("node2-name")
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, v1.UniqueVolumeName(""), volumeSpec, node2Name, devicePath, true)
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	if generatedVolumeName1 != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName1,
			generatedVolumeName2)
	}

	// Act
	attachedVolumes := asw.GetAttachedVolumesForNode(node1Name)

	// Assert
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName1, string(volumeName), node1Name, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

func Test_OneVolumeTwoNodes_TwoDevicePaths(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	node1Name := types.NodeName("node1-name")
	devicePath1 := "fake/device/path1"
	logger, _ := ktesting.NewTestContext(t)
	plugin, err := volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get volume plugin from spec %v, %v", volumeSpec, err)
	}
	uniqueVolumeName, err := volumeutil.GetUniqueVolumeNameFromSpec(plugin, volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get uniqueVolumeName from spec %v, %v", volumeSpec, err)
	}
	generatedVolumeName1, add1Err := asw.AddVolumeNode(logger, uniqueVolumeName, volumeSpec, node1Name, devicePath1, true)
	if add1Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add1Err)
	}
	node2Name := types.NodeName("node2-name")
	devicePath2 := "fake/device/path2"
	generatedVolumeName2, add2Err := asw.AddVolumeNode(logger, v1.UniqueVolumeName(""), volumeSpec, node2Name, devicePath2, true)
	if add2Err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", add2Err)
	}

	if generatedVolumeName1 != generatedVolumeName2 {
		t.Fatalf(
			"Generated volume names for the same volume should be the same but they are not: %q and %q",
			generatedVolumeName1,
			generatedVolumeName2)
	}

	// Act
	attachedVolumes := asw.GetAttachedVolumesForNode(node2Name)

	// Assert
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	verifyAttachedVolume(t, attachedVolumes, generatedVolumeName2, string(volumeName), node2Name, devicePath2, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Test_SetNodeStatusUpdateNeededError expects the map nodesToUpdateStatusFor
// to be empty if the SetNodeStatusUpdateNeeded is called on a node that
// does not exist in the actual state of the world
func Test_SetNodeStatusUpdateNeededError(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	nodeName := types.NodeName("node-1")

	// Act
	logger, _ := ktesting.NewTestContext(t)
	asw.SetNodeStatusUpdateNeeded(logger, nodeName)

	// Assert
	nodesToUpdateStatusFor := asw.GetNodesToUpdateStatusFor()
	if len(nodesToUpdateStatusFor) != 0 {
		t.Fatalf("nodesToUpdateStatusFor should be empty as nodeName does not exist")
	}
}

// Test_updateNodeStatusUpdateNeeded expects statusUpdateNeeded to be properly updated if
// updateNodeStatusUpdateNeeded is called on a node that exists in the actual state of the world
func Test_updateNodeStatusUpdateNeeded(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := &actualStateOfWorld{
		attachedVolumes:        make(map[v1.UniqueVolumeName]attachedVolume),
		nodesToUpdateStatusFor: make(map[types.NodeName]nodeToUpdateStatusFor),
		volumePluginMgr:        volumePluginMgr,
	}
	nodeName := types.NodeName("node-1")
	nodeToUpdate := nodeToUpdateStatusFor{
		nodeName:                  nodeName,
		statusUpdateNeeded:        true,
		volumesToReportAsAttached: make(map[v1.UniqueVolumeName]v1.UniqueVolumeName),
	}
	asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate

	// Act
	err := asw.updateNodeStatusUpdateNeeded(nodeName, false)

	// Assert
	if err != nil {
		t.Fatalf("updateNodeStatusUpdateNeeded should not return error, but got: %v", err)
	}
	nodesToUpdateStatusFor := asw.GetNodesToUpdateStatusFor()
	if nodesToUpdateStatusFor[nodeName].statusUpdateNeeded {
		t.Fatalf("nodesToUpdateStatusFor should be updated to: false, but got: true")
	}
}

// Test_updateNodeStatusUpdateNeededError expects statusUpdateNeeded to report error if
// updateNodeStatusUpdateNeeded is called on a node that does not exist in the actual state of the world
func Test_updateNodeStatusUpdateNeededError(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := &actualStateOfWorld{
		attachedVolumes:        make(map[v1.UniqueVolumeName]attachedVolume),
		nodesToUpdateStatusFor: make(map[types.NodeName]nodeToUpdateStatusFor),
		volumePluginMgr:        volumePluginMgr,
	}
	nodeName := types.NodeName("node-1")

	// Act
	err := asw.updateNodeStatusUpdateNeeded(nodeName, false)

	// Assert
	if err == nil {
		t.Fatalf("updateNodeStatusUpdateNeeded should return error, but got nothing")
	}
}

// Mark a volume as attached to a node.
// Verify GetAttachState returns AttachedState
// Verify GetAttachedVolumes return this volume
func Test_MarkVolumeAsAttached(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)

	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"

	plugin, err := volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get volume plugin from spec %v, %v", volumeSpec, err)
	}

	// Act
	logger, _ := ktesting.NewTestContext(t)
	err = asw.MarkVolumeAsAttached(logger, volumeName, volumeSpec, nodeName, devicePath)

	// Assert
	if err != nil {
		t.Fatalf("MarkVolumeAsAttached failed. Expected: <no error> Actual: <%v>", err)
	}

	volumeNodeComboState := asw.GetAttachState(volumeName, nodeName)
	if volumeNodeComboState != AttachStateAttached {
		t.Fatalf("asw says the volume: %q is not attached (%v) to node:%q, it should.",
			volumeName, AttachStateAttached, nodeName)
	}
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}
	verifyAttachedVolume(t, attachedVolumes, volumeName, string(volumeName), nodeName, devicePath, true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Mark a volume as attachment as uncertain.
// Verify GetAttachState returns UncertainState
// Verify GetAttachedVolumes return this volume
func Test_MarkVolumeAsUncertain(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)
	nodeName := types.NodeName("node-name")

	plugin, err := volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	if err != nil || plugin == nil {
		t.Fatalf("Failed to get volume plugin from spec %v, %v", volumeSpec, err)
	}

	// Act
	logger, _ := ktesting.NewTestContext(t)
	err = asw.MarkVolumeAsUncertain(logger, volumeName, volumeSpec, nodeName)

	// Assert
	if err != nil {
		t.Fatalf("MarkVolumeAsUncertain failed. Expected: <no error> Actual: <%v>", err)
	}
	volumeNodeComboState := asw.GetAttachState(volumeName, nodeName)
	if volumeNodeComboState != AttachStateUncertain {
		t.Fatalf("asw says the volume: %q is attached (%v) to node:%q, it should not.",
			volumeName, volumeNodeComboState, nodeName)
	}
	attachedVolumes := asw.GetAttachedVolumes()
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}
	verifyAttachedVolume(t, attachedVolumes, volumeName, string(volumeName), nodeName, "", true /* expectedMountedByNode */, false /* expectNonZeroDetachRequestedTime */)
}

// Calls AddVolumeNode() once with attached set to true.
// Verifies GetVolumesToReportAttachedForNode has an update for the node.
// Call GetVolumesToReportAttachedForNode a second time for the node, verify it does not report
// an update is needed any more
// Then calls RemoveVolumeFromReportAsAttached()
// Verifies GetVolumesToReportAttachedForNode reports an update is needed
func Test_GetVolumesToReportAttachedForNode_Positive(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	volumeName := v1.UniqueVolumeName("volume-name")
	volumeSpec := controllervolumetesting.GetTestVolumeSpec(string(volumeName), volumeName)

	nodeName := types.NodeName("node-name")
	devicePath := "fake/device/path"

	// Act
	logger, _ := ktesting.NewTestContext(t)
	generatedVolumeName, err := asw.AddVolumeNode(logger, volumeName, volumeSpec, nodeName, devicePath, true)

	// Assert
	if err != nil {
		t.Fatalf("AddVolumeNode failed. Expected: <no error> Actual: <%v>", err)
	}

	needsUpdate, attachedVolumes := asw.GetVolumesToReportAttachedForNode(logger, nodeName)
	if !needsUpdate {
		t.Fatalf("GetVolumesToReportAttachedForNode_Positive_NewVolumeNewNodeWithTrueAttached failed. Actual: <node %q does not need an update> Expect: <node exists in the reportedAsAttached map and needs an update", nodeName)
	}
	if len(attachedVolumes) != 1 {
		t.Fatalf("len(attachedVolumes) Expected: <1> Actual: <%v>", len(attachedVolumes))
	}

	needsUpdate, _ = asw.GetVolumesToReportAttachedForNode(logger, nodeName)
	if needsUpdate {
		t.Fatalf("GetVolumesToReportAttachedForNode_Positive_NewVolumeNewNodeWithTrueAttached failed. Actual: <node %q needs an update> Expect: <node exists in the reportedAsAttached map and does not need an update", nodeName)
	}

	removeVolumeDetachErr := asw.RemoveVolumeFromReportAsAttached(generatedVolumeName, nodeName)
	if removeVolumeDetachErr != nil {
		t.Fatalf("RemoveVolumeFromReportAsAttached failed. Expected: <no error> Actual: <%v>", removeVolumeDetachErr)
	}

	needsUpdate, attachedVolumes = asw.GetVolumesToReportAttachedForNode(logger, nodeName)
	if !needsUpdate {
		t.Fatalf("GetVolumesToReportAttachedForNode_Positive_NewVolumeNewNodeWithTrueAttached failed. Actual: <node %q does not need an update> Expect: <node exists in the reportedAsAttached map and needs an update", nodeName)
	}
	if len(attachedVolumes) != 0 {
		t.Fatalf("len(attachedVolumes) Expected: <0> Actual: <%v>", len(attachedVolumes))
	}
}

// Verifies GetVolumesToReportAttachedForNode reports no update needed for an unknown node.
func Test_GetVolumesToReportAttachedForNode_UnknownNode(t *testing.T) {
	// Arrange
	volumePluginMgr, _ := volumetesting.GetTestVolumePluginMgr(t)
	asw := NewActualStateOfWorld(volumePluginMgr)
	nodeName := types.NodeName("node-name")
	logger, _ := ktesting.NewTestContext(t)
	needsUpdate, _ := asw.GetVolumesToReportAttachedForNode(logger, nodeName)
	if needsUpdate {
		t.Fatalf("GetVolumesToReportAttachedForNode_UnknownNode failed. Actual: <node %q needs an update> Expect: <node does not exist in the reportedAsAttached map and does not need an update", nodeName)
	}
}

func verifyAttachedVolume(
	t *testing.T,
	attachedVolumes []AttachedVolume,
	expectedVolumeName v1.UniqueVolumeName,
	expectedVolumeSpecName string,
	expectedNodeName types.NodeName,
	expectedDevicePath string,
	expectedMountedByNode,
	expectNonZeroDetachRequestedTime bool) {
	for _, attachedVolume := range attachedVolumes {
		if attachedVolume.VolumeName == expectedVolumeName &&
			attachedVolume.VolumeSpec.Name() == expectedVolumeSpecName &&
			attachedVolume.NodeName == expectedNodeName &&
			attachedVolume.DevicePath == expectedDevicePath &&
			attachedVolume.MountedByNode == expectedMountedByNode &&
			attachedVolume.DetachRequestedTime.IsZero() == !expectNonZeroDetachRequestedTime {
			return
		}
	}

	t.Fatalf(
		"attachedVolumes (%v) should contain the volume/node combo %q/%q with DevicePath=%q MountedByNode=%v and NonZeroDetachRequestedTime=%v. It does not.",
		attachedVolumes,
		expectedVolumeName,
		expectedNodeName,
		expectedDevicePath,
		expectedMountedByNode,
		expectNonZeroDetachRequestedTime)
}
