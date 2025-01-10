/*
Copyright 2018 The Kubernetes Authors.

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

package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"k8s.io/kubernetes/test/e2e/storage/drivers/csi-test/mock/cache"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// Name is the name of the CSI plug-in.
	Name = "io.kubernetes.storage.mock"

	// VendorVersion is the version returned by GetPluginInfo.
	VendorVersion = "0.3.0"

	// TopologyKey simulates a per-node topology.
	TopologyKey = Name + "/node"

	// TopologyValue is the one, fixed node on which the driver runs.
	TopologyValue = "some-mock-node"
)

// Manifest is the SP's manifest.
var Manifest = map[string]string{
	"url": "https://github.com/kubernetes/kubernetes/tree/master/test/e2e/storage/drivers/csi-test/mock",
}

type Config struct {
	DisableAttach               bool
	DriverName                  string
	AttachLimit                 int64
	NodeExpansionRequired       bool
	NodeVolumeConditionRequired bool
	VolumeMountGroupRequired    bool
	DisableControllerExpansion  bool
	DisableOnlineExpansion      bool
	PermissiveTargetPath        bool
	EnableTopology              bool
	IO                          DirIO
}

// DirIO is an abstraction over direct os calls.
type DirIO interface {
	// DirExists returns false if the path doesn't exist, true if it exists and is a directory, an error otherwise.
	DirExists(path string) (bool, error)
	// Mkdir creates the directory, but not its parents, with 0755 permissions.
	Mkdir(path string) error
	// RemoveAll removes the path and everything contained inside it. It's not an error if the path does not exist.
	RemoveAll(path string) error
	// Rename changes the name of a file or directory. The parent directory
	// of newPath must exist.
	Rename(oldPath, newPath string) error
}

type OSDirIO struct{}

func (o OSDirIO) DirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil && !info.IsDir():
		return false, fmt.Errorf("%s: not a directory", path)
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

func (o OSDirIO) Mkdir(path string) error {
	return os.Mkdir(path, os.FileMode(0755))
}

func (o OSDirIO) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (o OSDirIO) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

// Service is the CSI Mock service provider.
type Service interface {
	csi.ControllerServer
	csi.IdentityServer
	csi.NodeServer
}

type service struct {
	sync.Mutex
	nodeID       string
	vols         []csi.Volume
	volsRWL      sync.RWMutex
	volsNID      uint64
	snapshots    cache.SnapshotCache
	snapshotsNID uint64
	config       Config
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer
}

type Volume struct {
	VolumeCSI             csi.Volume
	NodeID                string
	ISStaged              bool
	ISPublished           bool
	ISEphemeral           bool
	ISControllerPublished bool
	StageTargetPath       string
	TargetPath            string
}

var MockVolumes map[string]Volume

// New returns a new Service.
func New(config Config) Service {
	s := &service{
		nodeID: config.DriverName,
		config: config,
	}
	if s.config.IO == nil {
		s.config.IO = OSDirIO{}
	}
	s.snapshots = cache.NewSnapshotCache()
	s.vols = []csi.Volume{
		s.newVolume("Mock Volume 1", gib100),
		s.newVolume("Mock Volume 2", gib100),
		s.newVolume("Mock Volume 3", gib100),
	}
	MockVolumes = map[string]Volume{}

	s.snapshots.Add(s.newSnapshot("Mock Snapshot 1", "1", map[string]string{"Description": "snapshot 1"}))
	s.snapshots.Add(s.newSnapshot("Mock Snapshot 2", "2", map[string]string{"Description": "snapshot 2"}))
	s.snapshots.Add(s.newSnapshot("Mock Snapshot 3", "3", map[string]string{"Description": "snapshot 3"}))

	return s
}

const (
	kib    int64 = 1024
	mib    int64 = kib * 1024
	gib    int64 = mib * 1024
	gib100 int64 = gib * 100
	tib    int64 = gib * 1024
)

func (s *service) newVolume(name string, capcity int64) csi.Volume {
	vol := csi.Volume{
		VolumeId:      fmt.Sprintf("%d", atomic.AddUint64(&s.volsNID, 1)),
		VolumeContext: map[string]string{"name": name},
		CapacityBytes: capcity,
	}
	s.setTopology(&vol)
	return vol
}

func (s *service) newVolumeFromSnapshot(name string, capacity int64, snapshotID int) csi.Volume {
	vol := s.newVolume(name, capacity)
	vol.ContentSource = &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{
				SnapshotId: fmt.Sprintf("%d", snapshotID),
			},
		},
	}
	s.setTopology(&vol)
	return vol
}

func (s *service) newVolumeFromVolume(name string, capacity int64, volumeID int) csi.Volume {
	vol := s.newVolume(name, capacity)
	vol.ContentSource = &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{
				VolumeId: fmt.Sprintf("%d", volumeID),
			},
		},
	}
	s.setTopology(&vol)
	return vol
}

func (s *service) setTopology(vol *csi.Volume) {
	if s.config.EnableTopology {
		vol.AccessibleTopology = []*csi.Topology{
			{
				Segments: map[string]string{
					TopologyKey: TopologyValue,
				},
			},
		}
	}
}

func (s *service) findVol(k, v string) (volIdx int, volInfo csi.Volume) {
	s.volsRWL.RLock()
	defer s.volsRWL.RUnlock()
	return s.findVolNoLock(k, v)
}

func (s *service) findVolNoLock(k, v string) (volIdx int, volInfo csi.Volume) {
	volIdx = -1

	for i, vi := range s.vols {
		switch k {
		case "id":
			if strings.EqualFold(v, vi.GetVolumeId()) {
				return i, vi
			}
		case "name":
			if n, ok := vi.VolumeContext["name"]; ok && strings.EqualFold(v, n) {
				return i, vi
			}
		}
	}

	return
}

func (s *service) findVolByName(
	ctx context.Context, name string) (int, csi.Volume) {

	return s.findVol("name", name)
}

func (s *service) findVolByID(
	ctx context.Context, id string) (int, csi.Volume) {

	return s.findVol("id", id)
}

func (s *service) newSnapshot(name, sourceVolumeId string, parameters map[string]string) cache.Snapshot {

	ptime := timestamppb.Now()
	return cache.Snapshot{
		Name:       name,
		Parameters: parameters,
		SnapshotCSI: csi.Snapshot{
			SnapshotId:     fmt.Sprintf("%d", atomic.AddUint64(&s.snapshotsNID, 1)),
			CreationTime:   ptime,
			SourceVolumeId: sourceVolumeId,
			ReadyToUse:     true,
		},
	}
}

// getAttachCount returns the number of attached volumes on the node.
func (s *service) getAttachCount(devPathKey string) int64 {
	var count int64
	for _, v := range s.vols {
		if device := v.VolumeContext[devPathKey]; device != "" {
			count++
		}
	}
	return count
}

func (s *service) execHook(hookName string) (codes.Code, string) {
	return codes.OK, ""
}
