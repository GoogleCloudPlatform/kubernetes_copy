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

package checkpointmanager

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/checksum"
	utilstore "k8s.io/kubernetes/pkg/kubelet/checkpointmanager/testing"
	v1 "k8s.io/kubernetes/pkg/kubelet/checkpointmanager/testing/example_checkpoint_formats/v1"
)

var testStore *utilstore.MemStore

type FakeCheckpoint interface {
	Checkpoint
	GetData() ([]*PortMapping, bool)
}

// Data contains all types of data that can be stored in the checkpoint.
type Data struct {
	PortMappings []*PortMapping `json:"port_mappings,omitempty"`
	HostNetwork  bool           `json:"host_network,omitempty"`
}

type CheckpointDataV2 struct {
	PortMappings []*PortMapping `json:"port_mappings,omitempty"`
	HostNetwork  bool           `json:"host_network,omitempty"`
	V2Field      string         `json:"v2field"`
}

type protocol string

// portMapping is the port mapping configurations of a sandbox.
type PortMapping struct {
	// protocol of the port mapping.
	Protocol *protocol
	// Port number within the container.
	ContainerPort *int32
	// Port number on the host.
	HostPort *int32
	// Host ip to expose.
	HostIP string
}

// CheckpointData is a sample example structure to be used in test cases for checkpointing
type CheckpointData struct {
	Version  string
	Name     string
	Data     *Data
	Checksum checksum.Checksum
}

func newFakeCheckpointV1(name string, portMappings []*PortMapping, hostNetwork bool) FakeCheckpoint {
	return &CheckpointData{
		Version: "v1",
		Name:    name,
		Data: &Data{
			PortMappings: portMappings,
			HostNetwork:  hostNetwork,
		},
	}
}

func (cp *CheckpointData) MarshalCheckpoint() ([]byte, error) {
	cp.Checksum = checksum.New(*cp.Data)
	return json.Marshal(*cp)
}

func (cp *CheckpointData) UnmarshalCheckpoint(blob []byte) error {
	return json.Unmarshal(blob, cp)
}

func (cp *CheckpointData) VerifyChecksum() error {
	return cp.Checksum.Verify(*cp.Data)
}

func (cp *CheckpointData) GetData() ([]*PortMapping, bool) {
	return cp.Data.PortMappings, cp.Data.HostNetwork
}

type checkpointDataV2 struct {
	Version  string
	Name     string
	Data     *CheckpointDataV2
	Checksum checksum.Checksum
}

func newFakeCheckpointV2(name string, portMappings []*PortMapping, hostNetwork bool) FakeCheckpoint {
	return &checkpointDataV2{
		Version: "v2",
		Name:    name,
		Data: &CheckpointDataV2{
			PortMappings: portMappings,
			HostNetwork:  hostNetwork,
		},
	}
}

func newFakeCheckpointRemoteV1(name string, portMappings []*v1.PortMapping, hostNetwork bool) Checkpoint {
	return &v1.CheckpointData{
		Version: "v1",
		Name:    name,
		Data: &v1.Data{
			PortMappings: portMappings,
			HostNetwork:  hostNetwork,
		},
	}
}

func (cp *checkpointDataV2) MarshalCheckpoint() ([]byte, error) {
	cp.Checksum = checksum.New(*cp.Data)
	return json.Marshal(*cp)
}

func (cp *checkpointDataV2) UnmarshalCheckpoint(blob []byte) error {
	return json.Unmarshal(blob, cp)
}

func (cp *checkpointDataV2) VerifyChecksum() error {
	return cp.Checksum.Verify(*cp.Data)
}

func (cp *checkpointDataV2) GetData() ([]*PortMapping, bool) {
	return cp.Data.PortMappings, cp.Data.HostNetwork
}

func newTestCheckpointManager() CheckpointManager {
	return &impl{store: testStore}
}

func TestCheckpointManager(t *testing.T) {
	var err error
	testStore = utilstore.NewMemStore()
	manager := newTestCheckpointManager()
	port80 := int32(80)
	port443 := int32(443)
	proto := protocol("tcp")
	ip1234 := "1.2.3.4"

	portMappings := []*PortMapping{
		{
			&proto,
			&port80,
			&port80,
			ip1234,
		},
		{
			&proto,
			&port443,
			&port443,
			ip1234,
		},
	}
	checkpoint1 := newFakeCheckpointV1("check1", portMappings, true)

	checkpoints := []struct {
		checkpointKey     string
		checkpoint        FakeCheckpoint
		expectHostNetwork bool
	}{
		{
			"key1",
			checkpoint1,
			true,
		},
		{
			"key2",
			newFakeCheckpointV1("check2", nil, false),
			false,
		},
	}

	for _, tc := range checkpoints {
		// Test CreateCheckpoints
		err = manager.CreateCheckpoint(tc.checkpointKey, tc.checkpoint)
		assert.NoError(t, err)

		// Test GetCheckpoints
		checkpointOut := newFakeCheckpointV1("", nil, false)
		err := manager.GetCheckpoint(tc.checkpointKey, checkpointOut)
		assert.NoError(t, err)
		actualPortMappings, actualHostNetwork := checkpointOut.GetData()
		expPortMappings, expHostNetwork := tc.checkpoint.GetData()
		assert.Equal(t, expPortMappings, actualPortMappings)
		assert.Equal(t, expHostNetwork, actualHostNetwork)
	}
	// Test it fails if tried to read V1 structure into V2, a different structure from the structure which is checkpointed
	checkpointV2 := newFakeCheckpointV2("", nil, false)
	err = manager.GetCheckpoint("key1", checkpointV2)
	assert.EqualError(t, err, "checkpoint is corrupted")

	// Test it fails if tried to read V1 structure into the same structure but defined in another package
	checkpointRemoteV1 := newFakeCheckpointRemoteV1("", nil, false)
	err = manager.GetCheckpoint("key1", checkpointRemoteV1)
	assert.EqualError(t, err, "checkpoint is corrupted")

	// Test it works if tried to read V1 structure using into a new V1 structure
	checkpointV1 := newFakeCheckpointV1("", nil, false)
	err = manager.GetCheckpoint("key1", checkpointV1)
	assert.NoError(t, err)

	// Test corrupt checksum case
	checkpointOut := newFakeCheckpointV1("", nil, false)
	blob, err := checkpointOut.MarshalCheckpoint()
	assert.NoError(t, err)
	testStore.Write("key1", blob)
	err = manager.GetCheckpoint("key1", checkpoint1)
	assert.EqualError(t, err, "checkpoint is corrupted")

	// Test ListCheckpoints
	keys, err := manager.ListCheckpoints()
	assert.NoError(t, err)
	sort.Strings(keys)
	assert.Equal(t, []string{"key1", "key2"}, keys)

	// Test RemoveCheckpoints
	err = manager.RemoveCheckpoint("key1")
	assert.NoError(t, err)
	// Test Remove Nonexistent Checkpoints
	err = manager.RemoveCheckpoint("key1")
	assert.NoError(t, err)

	// Test ListCheckpoints
	keys, err = manager.ListCheckpoints()
	assert.NoError(t, err)
	assert.Equal(t, []string{"key2"}, keys)

	// Test Get Nonexistent Checkpoint
	checkpointNE := newFakeCheckpointV1("NE", nil, false)
	err = manager.GetCheckpoint("key1", checkpointNE)
	assert.Error(t, err)
}
