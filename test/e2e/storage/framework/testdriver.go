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

package framework

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/framework"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
)

// TestDriver represents an interface for a driver to be tested in TestSuite.
// Except for GetDriverInfo, all methods will be called at test runtime and thus
// can use e2eskipper.Skipf, framework.Fatal, Gomega assertions, etc.
type TestDriver interface {
	// GetDriverInfo returns DriverInfo for the TestDriver. This must be static
	// information.
	GetDriverInfo() *DriverInfo

	// SkipUnsupportedTest skips test if Testpattern is not
	// suitable to test with the TestDriver. It gets called after
	// parsing parameters of the test suite and before the
	// framework is initialized. Cheap tests that just check
	// parameters like the cloud provider can and should be
	// done in SkipUnsupportedTest to avoid setting up more
	// expensive resources like framework.Framework. Tests that
	// depend on a connection to the cluster can be done in
	// PrepareTest once the framework is ready.
	SkipUnsupportedTest(TestPattern)

	// PrepareTest is called at test execution time each time a new test case is about to start.
	// It sets up all necessary resources and returns the per-test configuration.
	// Cleanup is handled via ginkgo.DeferCleanup inside PrepareTest.
	PrepareTest(ctx context.Context, f *framework.Framework) *PerTestConfig
}

// TestVolume is the result of PreprovisionedVolumeTestDriver.CreateVolume.
// The only common functionality is to delete it. Individual driver interfaces
// have additional methods that work with volumes created by them.
type TestVolume interface {
	DeleteVolume(ctx context.Context)
}

// PreprovisionedVolumeTestDriver represents an interface for a TestDriver that has pre-provisioned volume
type PreprovisionedVolumeTestDriver interface {
	TestDriver
	// CreateVolume creates a pre-provisioned volume of the desired volume type.
	CreateVolume(ctx context.Context, config *PerTestConfig, volumeType TestVolType) TestVolume
}

// InlineVolumeTestDriver represents an interface for a TestDriver that supports InlineVolume
type InlineVolumeTestDriver interface {
	PreprovisionedVolumeTestDriver

	// GetVolumeSource returns a volumeSource for inline volume.
	// It will set readOnly and fsType to the volumeSource, if TestDriver supports both of them.
	// It will return nil, if the TestDriver doesn't support either of the parameters.
	GetVolumeSource(readOnly bool, fsType string, testVolume TestVolume) *v1.VolumeSource
}

// PreprovisionedPVTestDriver represents an interface for a TestDriver that supports PreprovisionedPV
type PreprovisionedPVTestDriver interface {
	PreprovisionedVolumeTestDriver
	// GetPersistentVolumeSource returns a PersistentVolumeSource with volume node affinity for pre-provisioned Persistent Volume.
	// It will set readOnly and fsType to the PersistentVolumeSource, if TestDriver supports both of them.
	// It will return nil, if the TestDriver doesn't support either of the parameters.
	GetPersistentVolumeSource(readOnly bool, fsType string, testVolume TestVolume) (*v1.PersistentVolumeSource, *v1.VolumeNodeAffinity)
}

// DynamicPVTestDriver represents an interface for a TestDriver that supports DynamicPV
type DynamicPVTestDriver interface {
	TestDriver
	// GetDynamicProvisionStorageClass returns a StorageClass dynamic provision Persistent Volume.
	// The StorageClass must have
	// a unique name because GetDynamicProvisionStorageClass might
	// be called more than once per test.
	// It will set fsType to the StorageClass, if TestDriver supports it.
	// It will return nil, if the TestDriver doesn't support it.
	GetDynamicProvisionStorageClass(ctx context.Context, config *PerTestConfig, fsType string) *storagev1.StorageClass
}

// EphemeralTestDriver represents an interface for a TestDriver that supports ephemeral inline volumes.
type EphemeralTestDriver interface {
	TestDriver

	// GetVolume returns the volume attributes for a certain
	// inline ephemeral volume, enumerated starting with #0. Some
	// tests might require more than one volume. They can all be
	// the same or different, depending what the driver supports
	// and/or wants to test.
	//
	// For each volume, the test driver can return volume attributes,
	// whether the resulting volume is shared between different pods (i.e.
	// changes made in one pod are visible in another), and whether the
	// volume can be mounted read/write or only read-only.
	GetVolume(config *PerTestConfig, volumeNumber int) (attributes map[string]string, shared bool, readOnly bool)

	// GetCSIDriverName returns the name that was used when registering with
	// kubelet. Depending on how the driver was deployed, this can be different
	// from DriverInfo.Name. Starting with Kubernetes 1.16, there must also
	// be a CSIDriver object under the same name with a "mode" field that enables
	// usage of the driver for ephemeral inline volumes.
	GetCSIDriverName(config *PerTestConfig) string
}

// SnapshottableTestDriver represents an interface for a TestDriver that supports DynamicSnapshot
type SnapshottableTestDriver interface {
	TestDriver
	// GetSnapshotClass returns a SnapshotClass to create snapshot.
	// It will return nil, if the TestDriver doesn't support it.
	GetSnapshotClass(ctx context.Context, config *PerTestConfig, parameters map[string]string) *unstructured.Unstructured
}

type VoulmeGroupSnapshottableTestDriver interface {
	TestDriver
	// GetVolumeGroupSnapshotClass returns a VolumeGroupSnapshotClass to create group snapshot.
	GetVolumeGroupSnapshotClass(ctx context.Context, config *PerTestConfig, parameters map[string]string) *unstructured.Unstructured
}

// VolumeAttributesClassTestDriver represents an interface for a TestDriver that supports
// creating and modifying volumes via VolumeAttributesClass objects
type VolumeAttributesClassTestDriver interface {
	TestDriver
	// GetVolumeAttributesClass returns a VolumeAttributesClass to create/modify PVCs
	// It will return nil if the TestDriver does not support VACs
	GetVolumeAttributesClass(ctx context.Context, config *PerTestConfig) *storagev1beta1.VolumeAttributesClass
}

// CustomTimeoutsTestDriver represents an interface fo a TestDriver that supports custom timeouts.
type CustomTimeoutsTestDriver interface {
	TestDriver
	GetTimeouts() *framework.TimeoutContext
}

// GetDriverTimeouts returns the timeout of the driver operation
func GetDriverTimeouts(driver TestDriver) *framework.TimeoutContext {
	if d, ok := driver.(CustomTimeoutsTestDriver); ok {
		return d.GetTimeouts()
	}
	return framework.NewTimeoutContext()
}

// Capability represents a feature that a volume plugin supports
type Capability string

// Constants related to capabilities and behavior of the driver.
const (
	CapPersistence         Capability = "persistence"        // data is persisted across pod restarts
	CapBlock               Capability = "block"              // raw block mode
	CapFsGroup             Capability = "fsGroup"            // volume ownership via fsGroup
	CapVolumeMountGroup    Capability = "volumeMountGroup"   // Driver has the VolumeMountGroup CSI node capability. Because this is a FSGroup feature, the fsGroup capability must also be set to true.
	CapExec                Capability = "exec"               // exec a file in the volume
	CapSnapshotDataSource  Capability = "snapshotDataSource" // support populate data from snapshot
	CapVolumeGroupSnapshot Capability = "groupSnapshot"      // support group snapshot
	CapPVCDataSource       Capability = "pvcDataSource"      // support populate data from pvc

	// multiple pods on a node can use the same volume concurrently;
	// for CSI, see:
	// - https://github.com/container-storage-interface/spec/pull/150
	// - https://github.com/container-storage-interface/spec/issues/178
	// - NodeStageVolume in the spec
	CapMultiPODs Capability = "multipods"

	CapRWX                 Capability = "RWX"                 // support ReadWriteMany access modes
	CapControllerExpansion Capability = "controllerExpansion" // support volume expansion for controller
	CapNodeExpansion       Capability = "nodeExpansion"       // support volume expansion for node

	// offlineExpansion and onlineExpansion both default to true when
	// controllerExpansion is true. The only reason to set offlineExpansion
	// to false is when a CSI driver can only expand a volume while it's
	// attached to a pod. Conversely, onlineExpansion can be set to false
	// if the driver can only expand a volume while it is detached.
	CapOfflineExpansion Capability = "offlineExpansion" // supports offline volume expansion (default: true)
	CapOnlineExpansion  Capability = "onlineExpansion"  // supports online volume expansion (default: true)

	CapVolumeLimits     Capability = "volumeLimits"     // support volume limits (can be *very* slow)
	CapSingleNodeVolume Capability = "singleNodeVolume" // support volume that can run on single node (like hostpath)
	CapTopology         Capability = "topology"         // support topology

	// The driver publishes storage capacity information: when the storage class
	// for dynamic provisioning exists, the driver is expected to provide
	// capacity information for it.
	CapCapacity Capability = "capacity"

	// Anti-capability for drivers that do not support filesystem resizing of PVCs
	// that are cloned or restored from a snapshot.
	CapFSResizeFromSourceNotSupported Capability = "FSResizeFromSourceNotSupported"

	// To support ReadWriteOncePod, the following CSI sidecars must be
	// updated to these versions or greater:
	// - csi-provisioner:v3.0.0+
	// - csi-attacher:v3.3.0+
	// - csi-resizer:v1.3.0+
	CapReadWriteOncePod Capability = "readWriteOncePod"

	// The driver can handle two PersistentVolumes with the same VolumeHandle (= volume_id in CSI spec).
	// This capability is highly recommended for volumes that support ReadWriteMany access mode,
	// because creating multiple PVs for the same VolumeHandle is frequently used to share a single
	// volume among multiple namespaces.
	// Note that this capability needs to be disabled only for CSI drivers that break CSI boundary and
	// inspect Kubernetes PersistentVolume objects. A CSI driver that implements only CSI and does not
	// talk to Kubernetes API server in any way should keep this capability enabled, because
	// they will see the same NodeStage / NodePublish requests as if only one PV existed.
	CapMultiplePVsSameID Capability = "multiplePVsSameID"

	// The driver supports ReadOnlyMany (ROX) access mode
	CapReadOnlyMany Capability = "capReadOnlyMany"
)

// DriverInfo represents static information about a TestDriver.
type DriverInfo struct {
	// Internal name of the driver, this is used as a display name in the test
	// case and test objects
	Name string
	// Fully qualified plugin name as registered in Kubernetes of the in-tree
	// plugin if it exists and is empty if this DriverInfo represents a CSI
	// Driver
	InTreePluginName string
	TestTags         []interface{} // tags for the driver (e.g. framework.WithSlow())

	// Maximum single file size supported by this driver
	MaxFileSize int64
	// The range of disk size supported by this driver
	SupportedSizeRange e2evolume.SizeRange
	// Map of string for supported fs type
	SupportedFsType sets.String
	// Map of string for supported mount option
	SupportedMountOption sets.String
	// [Optional] Map of string for required mount option
	RequiredMountOption sets.String
	// Map that represents plugin capabilities
	Capabilities map[Capability]bool
	// [Optional] List of access modes required for provisioning, defaults to
	// RWO if unset
	RequiredAccessModes []v1.PersistentVolumeAccessMode
	// [Optional] List of topology keys driver supports
	TopologyKeys []string
	// [Optional] Number of allowed topologies the driver requires.
	// Only relevant if TopologyKeys is set. Defaults to 1.
	// Example: multi-zonal disk requires at least 2 allowed topologies.
	NumAllowedTopologies int
	// [Optional] Scale parameters for stress tests.
	// TODO(#96241): Rename this field to reflect the tests that consume it.
	StressTestOptions *StressTestOptions
	// [Optional] Scale parameters for volume snapshot stress tests.
	VolumeSnapshotStressTestOptions *VolumeSnapshotStressTestOptions
	// [Optional] Scale parameters for volume modify stress tests.
	VolumeModifyStressTestOptions *VolumeModifyStressTestOptions
	// [Optional] Parameters for performance tests
	PerformanceTestOptions *PerformanceTestOptions
}

// StressTestOptions contains parameters used for stress tests.
type StressTestOptions struct {
	// Number of pods to create in the test. This may also create
	// up to 1 volume per pod.
	NumPods int
	// Number of times to restart each Pod.
	NumRestarts int
}

// VolumeSnapshotStressTestOptions contains parameters used for volume snapshot stress tests.
type VolumeSnapshotStressTestOptions struct {
	// Number of pods to create in the test. This may also create
	// up to 1 volume per pod.
	NumPods int
	// Number of snapshots to create for each volume.
	NumSnapshots int
}

// VolumeModifyStressTestOptions contains parameters used for volume modify stress tests.
type VolumeModifyStressTestOptions struct {
	// Number of pods to create in the test. This may also create
	// up to 1 volume with volumeAttributesClass per pod.
	NumPods int
}

// Metrics to evaluate performance of an operation
// TODO: Add metrics like median, mode, standard deviation, percentile
type Metrics struct {
	AvgLatency time.Duration
	Throughput float64
}

// PerformanceTestProvisioningOptions contains parameters for
// testing provisioning operation performance.
type PerformanceTestProvisioningOptions struct {
	VolumeSize string
	Count      int
	// Expected metrics from PVC creation till PVC being Bound.
	ExpectedMetrics *Metrics
}

// PerformanceTestOptions contains parameters used for performance tests
type PerformanceTestOptions struct {
	ProvisioningOptions *PerformanceTestProvisioningOptions
}
