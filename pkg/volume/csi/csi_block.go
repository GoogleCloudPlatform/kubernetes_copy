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

/*
This file defines block volume related methods for CSI driver.
CSI driver is responsible for staging/publishing volumes to their staging/publish paths.
Mapping and unmapping of a device in a publish path to its global map path and its
pod device map path are done by operation_executor through MapBlockVolume/UnmapBlockVolume
(MapBlockVolume and UnmapBlockVolume take care for lock, symlink, and bind mount).

Summary of block volume related CSI driver's methods are as follows:
 - GetGlobalMapPath returns a global map path,
 - GetPodDeviceMapPath returns a pod device map path and filename,
 - SetUpDevice calls CSI's NodeStageVolume and stage a volume to its staging path,
 - MapPodDevice calls CSI's NodePublishVolume and publish a volume to its publish path,
 - UnmapPodDevice calls CSI's NodeUnpublishVolume and unpublish a volume from its publish path,
 - TearDownDevice calls CSI's NodeUnstageVolume and unstage a volume from its staging path.

These methods are called by below sequences:
 - operation_executor.MountVolume
   - csi.GetGlobalMapPath
   - csi.SetupDevice
     - NodeStageVolume
   - ASW.MarkDeviceAsMounted
   - csi.GetPodDeviceMapPath
   - csi.MapPodDevice
     - NodePublishVolume
   - util.MapBlockVolume
   - ASW.MarkVolumeAsMounted

 - operation_executor.UnmountVolume
   - csi.GetPodDeviceMapPath
   - util.UnmapBlockVolume
   - csi.UnmapPodDevice
     - NodeUnpublishVolume
   - ASW.MarkVolumeAsUnmounted

 - operation_executor.UnmountDevice
   - csi.TearDownDevice
     - NodeUnstageVolume
   - ASW.MarkDeviceAsUnmounted

After successful MountVolume for block volume, directory structure will be like below:
  /dev/loopX ... Descriptor lock(Loopback device to mapFile under global map path)
  /var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/{specName}/dev/ ... Global map path
  /var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/{specName}/dev/{podUID} ... MapFile(Bind mount to publish Path)
  /var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/staging/{specName} ... Staging path
  /var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/{specName}/{podUID} ... Publish path
  /var/lib/kubelet/pods/{podUID}/volumeDevices/kubernetes.io~csi/ ... Pod device map path
  /var/lib/kubelet/pods/{podUID}/volumeDevices/kubernetes.io~csi/{specName} ... MapFile(Symlink to publish path)
*/

package csi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/util/removeall"
	"k8s.io/kubernetes/pkg/volume"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
	utilstrings "k8s.io/utils/strings"
)

type csiBlockMapper struct {
	csiClientGetter
	k8s        kubernetes.Interface
	plugin     *csiPlugin
	driverName csiDriverName
	specName   string
	volumeID   string
	readOnly   bool
	spec       *volume.Spec
	pod        *v1.Pod
	podUID     types.UID
	volume.MetricsProvider
}

var _ volume.BlockVolumeMapper = &csiBlockMapper{}
var _ volume.CustomBlockVolumeMapper = &csiBlockMapper{}

// GetGlobalMapPath returns a global map path (on the node) to a device file which will be symlinked to
// Example: plugins/kubernetes.io/csi/volumeDevices/{specName}/dev
func (m *csiBlockMapper) GetGlobalMapPath(spec *volume.Spec) (string, error) {
	dir := getVolumeDevicePluginDir(m.specName, m.plugin.host)
	klog.V(4).Info(log("blockMapper.GetGlobalMapPath = %s", dir))
	return dir, nil
}

// GetStagingPath returns a staging path for a directory (on the node) that should be used on NodeStageVolume/NodeUnstageVolume
// Example: plugins/kubernetes.io/csi/volumeDevices/staging/{specName}
func (m *csiBlockMapper) GetStagingPath() string {
	return filepath.Join(m.plugin.host.GetVolumeDevicePluginDir(CSIPluginName), "staging", m.specName)
}

// SupportsMetrics returns true for csiBlockMapper as it initializes the
// MetricsProvider.
func (m *csiBlockMapper) SupportsMetrics() bool {
	return true
}

// getPublishDir returns path to a directory, where the volume is published to each pod.
// Example: plugins/kubernetes.io/csi/volumeDevices/publish/{specName}
func (m *csiBlockMapper) getPublishDir() string {
	return filepath.Join(m.plugin.host.GetVolumeDevicePluginDir(CSIPluginName), "publish", m.specName)
}

// getPublishPath returns a publish path for a file (on the node) that should be used on NodePublishVolume/NodeUnpublishVolume
// Example: plugins/kubernetes.io/csi/volumeDevices/publish/{specName}/{podUID}
func (m *csiBlockMapper) getPublishPath() string {
	return filepath.Join(m.getPublishDir(), string(m.podUID))
}

// GetPodDeviceMapPath returns pod's device file which will be mapped to a volume
// returns: pods/{podUID}/volumeDevices/kubernetes.io~csi, {specName}
func (m *csiBlockMapper) GetPodDeviceMapPath() (string, string) {
	path := m.plugin.host.GetPodVolumeDeviceDir(m.podUID, utilstrings.EscapeQualifiedName(CSIPluginName))
	klog.V(4).Info(log("blockMapper.GetPodDeviceMapPath [path=%s; name=%s]", path, m.specName))
	return path, m.specName
}

// stageVolumeForBlock stages a block volume to stagingPath
func (m *csiBlockMapper) stageVolumeForBlock(
	ctx context.Context,
	csi csiClient,
	accessMode v1.PersistentVolumeAccessMode,
	csiSource *v1.CSIPersistentVolumeSource,
	attachment *storage.VolumeAttachment,
) (string, error) {
	klog.V(4).Info(log("blockMapper.stageVolumeForBlock called"))

	stagingPath := m.GetStagingPath()
	klog.V(4).Info(log("blockMapper.stageVolumeForBlock stagingPath set [%s]", stagingPath))

	// Check whether "STAGE_UNSTAGE_VOLUME" is set
	stageUnstageSet, err := csi.NodeSupportsStageUnstage(ctx)
	if err != nil {
		return "", errors.New(log("blockMapper.stageVolumeForBlock failed to check STAGE_UNSTAGE_VOLUME capability: %v", err))
	}
	if !stageUnstageSet {
		klog.Info(log("blockMapper.stageVolumeForBlock STAGE_UNSTAGE_VOLUME capability not set. Skipping MountDevice..."))
		return "", nil
	}
	publishVolumeInfo := map[string]string{}
	if attachment != nil {
		publishVolumeInfo = attachment.Status.AttachmentMetadata
	}
	nodeStageSecrets := map[string]string{}
	if csiSource.NodeStageSecretRef != nil {
		nodeStageSecrets, err = getCredentialsFromSecret(m.k8s, csiSource.NodeStageSecretRef)
		if err != nil {
			return "", fmt.Errorf("failed to get NodeStageSecretRef %s/%s: %v",
				csiSource.NodeStageSecretRef.Namespace, csiSource.NodeStageSecretRef.Name, err)
		}
	}

	// Creating a stagingPath directory before call to NodeStageVolume
	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return "", errors.New(log("blockMapper.stageVolumeForBlock failed to create dir %s: %v", stagingPath, err))
	}
	klog.V(4).Info(log("blockMapper.stageVolumeForBlock created stagingPath directory successfully [%s]", stagingPath))

	// Request to stage a block volume to stagingPath.
	// Expected implementation for driver is creating driver specific resource on stagingPath and
	// attaching the block volume to the node.
	err = csi.NodeStageVolume(ctx,
		csiSource.VolumeHandle,
		publishVolumeInfo,
		stagingPath,
		fsTypeBlockName,
		accessMode,
		nodeStageSecrets,
		csiSource.VolumeAttributes,
		nil, /* MountOptions */
		nil /* fsGroup */)

	if err != nil {
		return "", err
	}

	klog.V(4).Info(log("blockMapper.stageVolumeForBlock successfully requested NodeStageVolume [%s]", stagingPath))
	return stagingPath, nil
}

// publishVolumeForBlock publishes a block volume to publishPath
func (m *csiBlockMapper) publishVolumeForBlock(
	ctx context.Context,
	csi csiClient,
	accessMode v1.PersistentVolumeAccessMode,
	csiSource *v1.CSIPersistentVolumeSource,
	attachment *storage.VolumeAttachment,
) (string, error) {
	klog.V(4).Info(log("blockMapper.publishVolumeForBlock called"))

	publishVolumeInfo := map[string]string{}
	if attachment != nil {
		publishVolumeInfo = attachment.Status.AttachmentMetadata
	}

	// Inject pod information into volume_attributes
	volAttribs := csiSource.VolumeAttributes
	podInfoEnabled, err := m.plugin.podInfoEnabled(string(m.driverName))
	if err != nil {
		return "", errors.New(log("blockMapper.publishVolumeForBlock failed to assemble volume attributes: %v", err))
	}
	volumeLifecycleMode, err := m.plugin.getVolumeLifecycleMode(m.spec)
	if err != nil {
		return "", errors.New(log("blockMapper.publishVolumeForBlock failed to get VolumeLifecycleMode: %v", err))
	}
	if podInfoEnabled {
		volAttribs = mergeMap(volAttribs, getPodInfoAttrs(m.pod, volumeLifecycleMode))
	}

	nodePublishSecrets := map[string]string{}
	if csiSource.NodePublishSecretRef != nil {
		nodePublishSecrets, err = getCredentialsFromSecret(m.k8s, csiSource.NodePublishSecretRef)
		if err != nil {
			return "", errors.New(log("blockMapper.publishVolumeForBlock failed to get NodePublishSecretRef %s/%s: %v",
				csiSource.NodePublishSecretRef.Namespace, csiSource.NodePublishSecretRef.Name, err))
		}
	}

	publishPath := m.getPublishPath()
	// Setup a parent directory for publishPath before call to NodePublishVolume
	publishDir := filepath.Dir(publishPath)
	if err := os.MkdirAll(publishDir, 0750); err != nil {
		return "", errors.New(log("blockMapper.publishVolumeForBlock failed to create dir %s:  %v", publishDir, err))
	}
	klog.V(4).Info(log("blockMapper.publishVolumeForBlock created directory for publishPath successfully [%s]", publishDir))

	// Request to publish a block volume to publishPath.
	// Expectation for driver is to place a block volume on the publishPath, by bind-mounting the device file on the publishPath or
	// creating device file on the publishPath.
	// Parent directory for publishPath is created by k8s, but driver is responsible for creating publishPath itself.
	// If driver doesn't implement NodeStageVolume, attaching the block volume to the node may be done, here.
	err = csi.NodePublishVolume(
		ctx,
		m.volumeID,
		m.readOnly,
		m.GetStagingPath(),
		publishPath,
		accessMode,
		publishVolumeInfo,
		volAttribs,
		nodePublishSecrets,
		fsTypeBlockName,
		[]string{}, /* mountOptions */
		nil,        /* fsGroup */
	)

	if err != nil {
		return "", err
	}

	return publishPath, nil
}

// SetUpDevice ensures the device is attached returns path where the device is located.
func (m *csiBlockMapper) SetUpDevice() (string, error) {
	klog.V(4).Info(log("blockMapper.SetUpDevice called"))

	// Get csiSource from spec
	if m.spec == nil {
		return "", errors.New(log("blockMapper.SetUpDevice spec is nil"))
	}

	csiSource, err := getCSISourceFromSpec(m.spec)
	if err != nil {
		return "", errors.New(log("blockMapper.SetUpDevice failed to get CSI persistent source: %v", err))
	}

	driverName := csiSource.Driver
	skip, err := m.plugin.skipAttach(driverName)
	if err != nil {
		return "", errors.New(log("blockMapper.SetupDevice failed to check CSIDriver for %s: %v", driverName, err))
	}

	var attachment *storage.VolumeAttachment
	if !skip {
		// Search for attachment by VolumeAttachment.Spec.Source.PersistentVolumeName
		nodeName := string(m.plugin.host.GetNodeName())
		attachID := getAttachmentName(csiSource.VolumeHandle, csiSource.Driver, nodeName)
		attachment, err = m.k8s.StorageV1().VolumeAttachments().Get(context.TODO(), attachID, meta.GetOptions{})
		if err != nil {
			return "", errors.New(log("blockMapper.SetupDevice failed to get volume attachment [id=%v]: %v", attachID, err))
		}
	}

	//TODO (vladimirvivien) implement better AccessModes mapping between k8s and CSI
	accessMode := v1.ReadWriteOnce
	if m.spec.PersistentVolume.Spec.AccessModes != nil {
		accessMode = m.spec.PersistentVolume.Spec.AccessModes[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()

	csiClient, err := m.csiClientGetter.Get()
	if err != nil {
		// Treat the absence of the CSI driver as a transient error
		// See https://github.com/kubernetes/kubernetes/issues/120268
		return "", volumetypes.NewTransientOperationFailure(log("blockMapper.SetUpDevice failed to get CSI client: %v", err))
	}

	// Call NodeStageVolume
	stagingPath, err := m.stageVolumeForBlock(ctx, csiClient, accessMode, csiSource, attachment)
	if err != nil {
		if volumetypes.IsOperationFinishedError(err) {
			cleanupErr := m.cleanupOrphanDeviceFiles()
			if cleanupErr != nil {
				// V(4) for not so serious error
				klog.V(4).Infof("Failed to clean up block volume directory %s", cleanupErr)
			}
		}
		return "", err
	}

	return stagingPath, nil
}

func (m *csiBlockMapper) MapPodDevice() (string, error) {
	klog.V(4).Info(log("blockMapper.MapPodDevice called"))

	// Get csiSource from spec
	if m.spec == nil {
		return "", errors.New(log("blockMapper.MapPodDevice spec is nil"))
	}

	csiSource, err := getCSISourceFromSpec(m.spec)
	if err != nil {
		return "", errors.New(log("blockMapper.MapPodDevice failed to get CSI persistent source: %v", err))
	}

	driverName := csiSource.Driver
	skip, err := m.plugin.skipAttach(driverName)
	if err != nil {
		return "", errors.New(log("blockMapper.MapPodDevice failed to check CSIDriver for %s: %v", driverName, err))
	}

	var attachment *storage.VolumeAttachment
	if !skip {
		// Search for attachment by VolumeAttachment.Spec.Source.PersistentVolumeName
		nodeName := string(m.plugin.host.GetNodeName())
		attachID := getAttachmentName(csiSource.VolumeHandle, csiSource.Driver, nodeName)
		attachment, err = m.k8s.StorageV1().VolumeAttachments().Get(context.TODO(), attachID, meta.GetOptions{})
		if err != nil {
			return "", errors.New(log("blockMapper.MapPodDevice failed to get volume attachment [id=%v]: %v", attachID, err))
		}
	}

	//TODO (vladimirvivien) implement better AccessModes mapping between k8s and CSI
	accessMode := v1.ReadWriteOnce
	if m.spec.PersistentVolume.Spec.AccessModes != nil {
		accessMode = m.spec.PersistentVolume.Spec.AccessModes[0]
	}

	ctx, cancel := createCSIOperationContext(m.spec, csiTimeout)
	defer cancel()

	csiClient, err := m.csiClientGetter.Get()
	if err != nil {
		// Treat the absence of the CSI driver as a transient error
		// See https://github.com/kubernetes/kubernetes/issues/120268
		return "", volumetypes.NewTransientOperationFailure(log("blockMapper.MapPodDevice failed to get CSI client: %v", err))
	}

	// Call NodePublishVolume
	publishPath, err := m.publishVolumeForBlock(ctx, csiClient, accessMode, csiSource, attachment)
	if err != nil {
		return "", err
	}

	return publishPath, nil
}

var _ volume.BlockVolumeUnmapper = &csiBlockMapper{}
var _ volume.CustomBlockVolumeUnmapper = &csiBlockMapper{}

// unpublishVolumeForBlock unpublishes a block volume from publishPath
func (m *csiBlockMapper) unpublishVolumeForBlock(ctx context.Context, csi csiClient, publishPath string) error {
	// Request to unpublish a block volume from publishPath.
	// Expectation for driver is to remove block volume from the publishPath, by unmounting bind-mounted device file
	// or deleting device file.
	// Driver is responsible for deleting publishPath itself.
	// If driver doesn't implement NodeUnstageVolume, detaching the block volume from the node may be done, here.
	if err := csi.NodeUnpublishVolume(ctx, m.volumeID, publishPath); err != nil {
		return errors.New(log("blockMapper.unpublishVolumeForBlock failed: %v", err))
	}
	klog.V(4).Info(log("blockMapper.unpublishVolumeForBlock NodeUnpublished successfully [%s]", publishPath))

	return nil
}

// unstageVolumeForBlock unstages a block volume from stagingPath
func (m *csiBlockMapper) unstageVolumeForBlock(ctx context.Context, csi csiClient, stagingPath string) error {
	// Check whether "STAGE_UNSTAGE_VOLUME" is set
	stageUnstageSet, err := csi.NodeSupportsStageUnstage(ctx)
	if err != nil {
		return errors.New(log("blockMapper.unstageVolumeForBlock failed to check STAGE_UNSTAGE_VOLUME capability: %v", err))
	}
	if !stageUnstageSet {
		klog.Info(log("blockMapper.unstageVolumeForBlock STAGE_UNSTAGE_VOLUME capability not set. Skipping unstageVolumeForBlock ..."))
		return nil
	}

	// Request to unstage a block volume from stagingPath.
	// Expected implementation for driver is removing driver specific resource in stagingPath and
	// detaching the block volume from the node.
	if err := csi.NodeUnstageVolume(ctx, m.volumeID, stagingPath); err != nil {
		return errors.New(log("blockMapper.unstageVolumeForBlock failed: %v", err))
	}
	klog.V(4).Info(log("blockMapper.unstageVolumeForBlock NodeUnstageVolume successfully [%s]", stagingPath))

	// Remove stagingPath directory and its contents
	if err := os.RemoveAll(stagingPath); err != nil {
		return errors.New(log("blockMapper.unstageVolumeForBlock failed to remove staging path after NodeUnstageVolume() error [%s]: %v", stagingPath, err))
	}

	return nil
}

// TearDownDevice removes traces of the SetUpDevice.
func (m *csiBlockMapper) TearDownDevice(globalMapPath, devicePath string) error {
	ctx, cancel := createCSIOperationContext(m.spec, csiTimeout)
	defer cancel()

	csiClient, err := m.csiClientGetter.Get()
	if err != nil {
		// Treat the absence of the CSI driver as a transient error
		// See https://github.com/kubernetes/kubernetes/issues/120268
		return volumetypes.NewTransientOperationFailure(log("blockMapper.TearDownDevice failed to get CSI client: %v", err))
	}

	// Call NodeUnstageVolume
	stagingPath := m.GetStagingPath()
	err = m.unstageVolumeForBlock(ctx, csiClient, stagingPath)
	if err != nil {
		return err
	}
	if err = m.cleanupOrphanDeviceFiles(); err != nil {
		// V(4) for not so serious error
		klog.V(4).Infof("Failed to clean up block volume directory %s", err)
	}

	return nil
}

// Clean up any orphan files / directories when a block volume is being unstaged.
// At this point we can be sure that there is no pod using the volume and all
// files are indeed orphaned.
func (m *csiBlockMapper) cleanupOrphanDeviceFiles() error {
	// Remove artifacts of NodePublish.
	// publishDir: xxx/plugins/kubernetes.io/csi/volumeDevices/publish/<volume name>
	// Each PublishVolume() created a subdirectory there. Since everything should be
	// already unpublished at this point, the directory should be empty by now.
	publishDir := m.getPublishDir()
	if err := os.Remove(publishDir); err != nil && !os.IsNotExist(err) {
		return errors.New(log("failed to remove publish directory [%s]: %v", publishDir, err))
	}

	// Remove artifacts of NodeStage.
	// stagingPath: xxx/plugins/kubernetes.io/csi/volumeDevices/staging/<volume name>
	stagingPath := m.GetStagingPath()
	if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
		return errors.New(log("failed to delete volume staging path [%s]: %v", stagingPath, err))
	}

	// Remove everything under xxx/plugins/kubernetes.io/csi/volumeDevices/<volume name>.
	// At this point it contains only "data/vol_data.json" and empty "dev/".
	volumeDir := getVolumePluginDir(m.specName, m.plugin.host)
	mounter := m.plugin.host.GetMounter(m.plugin.GetPluginName())
	if err := removeall.RemoveAllOneFilesystem(mounter, volumeDir); err != nil {
		return err
	}

	return nil
}

// UnmapPodDevice unmaps the block device path.
func (m *csiBlockMapper) UnmapPodDevice() error {
	publishPath := m.getPublishPath()

	csiClient, err := m.csiClientGetter.Get()
	if err != nil {
		// Treat the absence of the CSI driver as a transient error
		// See https://github.com/kubernetes/kubernetes/issues/120268
		return volumetypes.NewTransientOperationFailure(log("blockMapper.UnmapPodDevice failed to get CSI client: %v", err))
	}

	ctx, cancel := createCSIOperationContext(m.spec, csiTimeout)
	defer cancel()

	// Call NodeUnpublishVolume.
	// Even if publishPath does not exist - previous NodePublish may have timed out
	// and Kubernetes makes sure that the operation is finished.
	return m.unpublishVolumeForBlock(ctx, csiClient, publishPath)
}
