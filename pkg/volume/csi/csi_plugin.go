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

package csi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"

	authenticationv1 "k8s.io/api/authentication/v1"
	api "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	csitranslationplugins "k8s.io/csi-translation-lib/plugins"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi/nodeinfomanager"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

const (
	// CSIPluginName is the name of the in-tree CSI Plugin
	CSIPluginName = "kubernetes.io/csi"

	csiTimeout      = 2 * time.Minute
	volNameSep      = "^"
	volDataFileName = "vol_data.json"
	fsTypeBlockName = "block"

	// CsiResyncPeriod is default resync period duration
	// TODO: increase to something useful
	CsiResyncPeriod = time.Minute
)

type csiPlugin struct {
	host                      volume.VolumeHost
	csiDriverLister           storagelisters.CSIDriverLister
	serviceAccountTokenGetter func(namespace, name string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error)
	volumeAttachmentLister    storagelisters.VolumeAttachmentLister
}

// ProbeVolumePlugins returns implemented plugins
func ProbeVolumePlugins() []volume.VolumePlugin {
	p := &csiPlugin{
		host: nil,
	}
	return []volume.VolumePlugin{p}
}

// volume.VolumePlugin methods
var _ volume.VolumePlugin = &csiPlugin{}

// RegistrationHandler is the handler which is fed to the pluginwatcher API.
type RegistrationHandler struct {
}

// TODO (verult) consider using a struct instead of global variables
// csiDrivers map keep track of all registered CSI drivers on the node and their
// corresponding sockets
var csiDrivers = &DriversStore{}

var nim nodeinfomanager.Interface

// PluginHandler is the plugin registration handler interface passed to the
// pluginwatcher module in kubelet
var PluginHandler = &RegistrationHandler{}

// ValidatePlugin is called by kubelet's plugin watcher upon detection
// of a new registration socket opened by CSI Driver registrar side car.
func (h *RegistrationHandler) ValidatePlugin(pluginName string, endpoint string, versions []string) error {
	klog.Info(log("Trying to validate a new CSI Driver with name: %s endpoint: %s versions: %s",
		pluginName, endpoint, strings.Join(versions, ",")))

	_, err := h.validateVersions("ValidatePlugin", pluginName, endpoint, versions)
	if err != nil {
		return fmt.Errorf("validation failed for CSI Driver %s at endpoint %s: %v", pluginName, endpoint, err)
	}

	return err
}

// RegisterPlugin is called when a plugin can be registered
func (h *RegistrationHandler) RegisterPlugin(pluginName string, endpoint string, versions []string, pluginClientTimeout *time.Duration) error {
	klog.Info(log("Register new plugin with name: %s at endpoint: %s", pluginName, endpoint))

	highestSupportedVersion, err := h.validateVersions("RegisterPlugin", pluginName, endpoint, versions)
	if err != nil {
		return err
	}

	// Storing endpoint of newly registered CSI driver into the map, where CSI driver name will be the key
	// all other CSI components will be able to get the actual socket of CSI drivers by its name.
	csiDrivers.Set(pluginName, Driver{
		endpoint:                endpoint,
		highestSupportedVersion: highestSupportedVersion,
	})

	// Get node info from the driver.
	csi, err := newCsiDriverClient(csiDriverName(pluginName))
	if err != nil {
		return err
	}

	var timeout time.Duration
	if pluginClientTimeout == nil {
		timeout = csiTimeout
	} else {
		timeout = *pluginClientTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	driverNodeID, maxVolumePerNode, accessibleTopology, err := csi.NodeGetInfo(ctx)
	if err != nil {
		if unregErr := unregisterDriver(pluginName); unregErr != nil {
			klog.Error(log("registrationHandler.RegisterPlugin failed to unregister plugin due to previous error: %v", unregErr))
		}
		return err
	}

	err = nim.InstallCSIDriver(pluginName, driverNodeID, maxVolumePerNode, accessibleTopology)
	if err != nil {
		if unregErr := unregisterDriver(pluginName); unregErr != nil {
			klog.Error(log("registrationHandler.RegisterPlugin failed to unregister plugin due to previous error: %v", unregErr))
		}
		return err
	}

	return nil
}

func (h *RegistrationHandler) validateVersions(callerName, pluginName string, endpoint string, versions []string) (*utilversion.Version, error) {
	if len(versions) == 0 {
		return nil, errors.New(log("%s for CSI driver %q failed. Plugin returned an empty list for supported versions", callerName, pluginName))
	}

	// Validate version
	// CSI currently only has version 0.x and 1.x (see https://github.com/container-storage-interface/spec/releases).
	// Therefore any driver claiming version 2.x+ is ignored as an unsupported versions.
	// Future 1.x versions of CSI are supposed to be backwards compatible so this version of Kubernetes will work with any 1.x driver
	// (or 0.x), but it may not work with 2.x drivers (because 2.x does not have to be backwards compatible with 1.x).
	// CSI v0.x is no longer supported as of Kubernetes v1.17 in accordance with deprecation policy set out in Kubernetes v1.13.
	newDriverHighestVersion, err := utilversion.HighestSupportedVersion(versions)
	if err != nil {
		return nil, errors.New(log("%s for CSI driver %q failed. None of the versions specified %q are supported. err=%v", callerName, pluginName, versions, err))
	}

	existingDriver, driverExists := csiDrivers.Get(pluginName)
	if driverExists {
		if !existingDriver.highestSupportedVersion.LessThan(newDriverHighestVersion) {
			return nil, errors.New(log("%s for CSI driver %q failed. Another driver with the same name is already registered with a higher supported version: %q", callerName, pluginName, existingDriver.highestSupportedVersion))
		}
	}

	return newDriverHighestVersion, nil
}

// DeRegisterPlugin is called when a plugin removed its socket, signaling
// it is no longer available
func (h *RegistrationHandler) DeRegisterPlugin(pluginName, endpoint string) {
	klog.Info(log("registrationHandler.DeRegisterPlugin request for plugin %s, endpoint %s", pluginName, endpoint))
	if err := unregisterDriver(pluginName); err != nil {
		klog.Error(log("registrationHandler.DeRegisterPlugin failed: %v", err))
	}
}

func (p *csiPlugin) Init(host volume.VolumeHost) error {
	p.host = host

	csiClient := host.GetKubeClient()
	if csiClient == nil {
		klog.Warning(log("kubeclient not set, assuming standalone kubelet"))
	} else {
		// set CSIDriverLister and volumeAttachmentLister
		seLinuxHost, ok := host.(volume.CSIDriverVolumeHost)
		if ok {
			p.csiDriverLister = seLinuxHost.CSIDriverLister()
			if p.csiDriverLister == nil {
				klog.Error(log("CSIDriverLister not found on CSIDriverVolumeHost"))
			}
		}
		adcHost, ok := host.(volume.AttachDetachVolumeHost)
		if ok {
			p.volumeAttachmentLister = adcHost.VolumeAttachmentLister()
			if p.volumeAttachmentLister == nil {
				klog.Error(log("VolumeAttachmentLister not found on AttachDetachVolumeHost"))
			}
		}
		kletHost, ok := host.(volume.KubeletVolumeHost)
		if ok {
			p.csiDriverLister = kletHost.CSIDriverLister()
			if p.csiDriverLister == nil {
				klog.Error(log("CSIDriverLister not found on KubeletVolumeHost"))
			}
			p.serviceAccountTokenGetter = host.GetServiceAccountTokenFunc()
			if p.serviceAccountTokenGetter == nil {
				klog.Error(log("ServiceAccountTokenGetter not found on KubeletVolumeHost"))
			}
			// We don't run the volumeAttachmentLister in the kubelet context
			p.volumeAttachmentLister = nil
		}
	}

	var migratedPlugins = map[string](func() bool){
		csitranslationplugins.GCEPDInTreePluginName: func() bool {
			return true
		},
		csitranslationplugins.AWSEBSInTreePluginName: func() bool {
			return true
		},
		csitranslationplugins.CinderInTreePluginName: func() bool {
			return true
		},
		csitranslationplugins.AzureDiskInTreePluginName: func() bool {
			return true
		},
		csitranslationplugins.AzureFileInTreePluginName: func() bool {
			return true
		},
		csitranslationplugins.VSphereInTreePluginName: func() bool {
			return true
		},
		csitranslationplugins.PortworxVolumePluginName: func() bool {
			return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationPortworx)
		},
	}

	// Initializing the label management channels
	nim = nodeinfomanager.NewNodeInfoManager(host.GetNodeName(), host, migratedPlugins)

	// This function prevents Kubelet from posting Ready status until CSINode
	// is both installed and initialized
	if err := initializeCSINode(host); err != nil {
		return errors.New(log("failed to initialize CSINode: %v", err))
	}

	return nil
}

func initializeCSINode(host volume.VolumeHost) error {
	kvh, ok := host.(volume.KubeletVolumeHost)
	if !ok {
		klog.V(4).Info("Cast from VolumeHost to KubeletVolumeHost failed. Skipping CSINode initialization, not running on kubelet")
		return nil
	}
	kubeClient := host.GetKubeClient()
	if kubeClient == nil {
		// Kubelet running in standalone mode. Skip CSINode initialization
		klog.Warning("Skipping CSINode initialization, kubelet running in standalone mode")
		return nil
	}

	kvh.SetKubeletError(errors.New("CSINode is not yet initialized"))

	go func() {
		defer utilruntime.HandleCrash()

		// First wait indefinitely to talk to Kube APIServer
		nodeName := host.GetNodeName()
		err := waitForAPIServerForever(kubeClient, nodeName)
		if err != nil {
			klog.Fatalf("Failed to initialize CSINode while waiting for API server to report ok: %v", err)
		}

		// Backoff parameters tuned to retry over 140 seconds. Will fail and restart the Kubelet
		// after max retry steps.
		initBackoff := wait.Backoff{
			Steps:    6,
			Duration: 15 * time.Millisecond,
			Factor:   6.0,
			Jitter:   0.1,
		}
		err = wait.ExponentialBackoff(initBackoff, func() (bool, error) {
			klog.V(4).Infof("Initializing migrated drivers on CSINode")
			err := nim.InitializeCSINodeWithAnnotation()
			if err != nil {
				kvh.SetKubeletError(fmt.Errorf("failed to initialize CSINode: %v", err))
				klog.Errorf("Failed to initialize CSINode: %v", err)
				return false, nil
			}

			// Successfully initialized drivers, allow Kubelet to post Ready
			kvh.SetKubeletError(nil)
			return true, nil
		})
		if err != nil {
			// 2 releases after CSIMigration and all CSIMigrationX (where X is a volume plugin)
			// are permanently enabled the apiserver/controllers can assume that the kubelet is
			// using CSI for all Migrated volume plugins. Then all the CSINode initialization
			// code can be dropped from Kubelet.
			// Kill the Kubelet process and allow it to restart to retry initialization
			klog.Fatalf("Failed to initialize CSINode after retrying: %v", err)
		}
	}()
	return nil
}

func (p *csiPlugin) GetPluginName() string {
	return CSIPluginName
}

// GetvolumeName returns a concatenated string of CSIVolumeSource.Driver<volNameSe>CSIVolumeSource.VolumeHandle
// That string value is used in Detach() to extract driver name and volumeName.
func (p *csiPlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	csi, err := getPVSourceFromSpec(spec)
	if err != nil {
		return "", errors.New(log("plugin.GetVolumeName failed to extract volume source from spec: %v", err))
	}

	// return driverName<separator>volumeHandle
	return fmt.Sprintf("%s%s%s", csi.Driver, volNameSep, csi.VolumeHandle), nil
}

func (p *csiPlugin) CanSupport(spec *volume.Spec) bool {
	// TODO (vladimirvivien) CanSupport should also take into account
	// the availability/registration of specified Driver in the volume source
	if spec == nil {
		return false
	}
	return (spec.PersistentVolume != nil && spec.PersistentVolume.Spec.CSI != nil) ||
		(spec.Volume != nil && spec.Volume.CSI != nil)
}

func (p *csiPlugin) RequiresRemount(spec *volume.Spec) bool {
	if p.csiDriverLister == nil {
		return false
	}
	driverName, err := GetCSIDriverName(spec)
	if err != nil {
		klog.V(5).Info(log("Failed to mark %q as republish required, err: %v", spec.Name(), err))
		return false
	}
	csiDriver, err := p.getCSIDriver(driverName)
	if err != nil {
		klog.V(5).Info(log("Failed to mark %q as republish required, err: %v", spec.Name(), err))
		return false
	}
	return *csiDriver.Spec.RequiresRepublish
}

func (p *csiPlugin) NewMounter(
	spec *volume.Spec,
	pod *api.Pod) (volume.Mounter, error) {

	volSrc, pvSrc, err := getSourceFromSpec(spec)
	if err != nil {
		return nil, err
	}

	var (
		driverName   string
		volumeHandle string
		readOnly     bool
	)

	switch {
	case volSrc != nil:
		volumeHandle = makeVolumeHandle(string(pod.UID), spec.Name())
		driverName = volSrc.Driver
		if volSrc.ReadOnly != nil {
			readOnly = *volSrc.ReadOnly
		}
	case pvSrc != nil:
		driverName = pvSrc.Driver
		volumeHandle = pvSrc.VolumeHandle
		readOnly = spec.ReadOnly
	default:
		return nil, errors.New(log("volume source not found in volume.Spec"))
	}

	volumeLifecycleMode, err := p.getVolumeLifecycleMode(spec)
	if err != nil {
		return nil, err
	}

	k8s := p.host.GetKubeClient()
	if k8s == nil {
		return nil, errors.New(log("failed to get a kubernetes client"))
	}

	kvh, ok := p.host.(volume.KubeletVolumeHost)
	if !ok {
		return nil, errors.New(log("cast from VolumeHost to KubeletVolumeHost failed"))
	}

	mounter := &csiMountMgr{
		plugin:              p,
		k8s:                 k8s,
		spec:                spec,
		pod:                 pod,
		podUID:              pod.UID,
		driverName:          csiDriverName(driverName),
		volumeLifecycleMode: volumeLifecycleMode,
		volumeID:            volumeHandle,
		specVolumeID:        spec.Name(),
		readOnly:            readOnly,
		kubeVolHost:         kvh,
	}
	mounter.csiClientGetter.driverName = csiDriverName(driverName)

	dir := mounter.GetPath()
	mounter.MetricsProvider = NewMetricsCsi(volumeHandle, dir, csiDriverName(driverName))
	klog.V(4).Info(log("mounter created successfully"))
	return mounter, nil
}

func (p *csiPlugin) NewUnmounter(specName string, podUID types.UID) (volume.Unmounter, error) {
	klog.V(4).Info(log("setting up unmounter for [name=%v, podUID=%v]", specName, podUID))

	kvh, ok := p.host.(volume.KubeletVolumeHost)
	if !ok {
		return nil, errors.New(log("cast from VolumeHost to KubeletVolumeHost failed"))
	}

	unmounter := &csiMountMgr{
		plugin:       p,
		podUID:       podUID,
		specVolumeID: specName,
		kubeVolHost:  kvh,
	}

	// load volume info from file
	dir := unmounter.GetPath()
	dataDir := filepath.Dir(dir) // dropoff /mount at end
	data, err := loadVolumeData(dataDir, volDataFileName)
	if err != nil {
		return nil, errors.New(log("unmounter failed to load volume data file [%s]: %v", dir, err))
	}
	unmounter.driverName = csiDriverName(data[volDataKey.driverName])
	unmounter.volumeID = data[volDataKey.volHandle]
	unmounter.csiClientGetter.driverName = unmounter.driverName

	return unmounter, nil
}

func (p *csiPlugin) ConstructVolumeSpec(volumeName, mountPath string) (volume.ReconstructedVolume, error) {
	klog.V(4).Info(log("plugin.ConstructVolumeSpec [pv.Name=%v, path=%v]", volumeName, mountPath))

	volData, err := loadVolumeData(mountPath, volDataFileName)
	if err != nil {
		return volume.ReconstructedVolume{}, errors.New(log("plugin.ConstructVolumeSpec failed loading volume data using [%s]: %v", mountPath, err))
	}
	klog.V(4).Info(log("plugin.ConstructVolumeSpec extracted [%#v]", volData))

	var ret volume.ReconstructedVolume
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod) {
		ret.SELinuxMountContext = volData[volDataKey.seLinuxMountContext]
	}

	// If mode is VolumeLifecycleEphemeral, use constructVolSourceSpec
	// to construct volume source spec. If mode is VolumeLifecyclePersistent,
	// use constructPVSourceSpec to construct volume construct pv source spec.
	if storage.VolumeLifecycleMode(volData[volDataKey.volumeLifecycleMode]) == storage.VolumeLifecycleEphemeral {
		ret.Spec = p.constructVolSourceSpec(volData[volDataKey.specVolID], volData[volDataKey.driverName])
		return ret, nil
	}

	ret.Spec = p.constructPVSourceSpec(volData[volDataKey.specVolID], volData[volDataKey.driverName], volData[volDataKey.volHandle])
	return ret, nil
}

// constructVolSourceSpec constructs volume.Spec with CSIVolumeSource
func (p *csiPlugin) constructVolSourceSpec(volSpecName, driverName string) *volume.Spec {
	vol := &api.Volume{
		Name: volSpecName,
		VolumeSource: api.VolumeSource{
			CSI: &api.CSIVolumeSource{
				Driver: driverName,
			},
		},
	}
	return volume.NewSpecFromVolume(vol)
}

// constructPVSourceSpec constructs volume.Spec with CSIPersistentVolumeSource
func (p *csiPlugin) constructPVSourceSpec(volSpecName, driverName, volumeHandle string) *volume.Spec {
	fsMode := api.PersistentVolumeFilesystem
	pv := &api.PersistentVolume{
		ObjectMeta: meta.ObjectMeta{
			Name: volSpecName,
		},
		Spec: api.PersistentVolumeSpec{
			PersistentVolumeSource: api.PersistentVolumeSource{
				CSI: &api.CSIPersistentVolumeSource{
					Driver:       driverName,
					VolumeHandle: volumeHandle,
				},
			},
			VolumeMode: &fsMode,
		},
	}
	return volume.NewSpecFromPersistentVolume(pv, false)
}

func (p *csiPlugin) SupportsMountOption() bool {
	// TODO (vladimirvivien) use CSI VolumeCapability.MountVolume.mount_flags
	// to probe for the result for this method
	// (bswartz) Until the CSI spec supports probing, our only option is to
	// make plugins register their support for mount options or lack thereof
	// directly with kubernetes.
	return true
}

func (p *csiPlugin) SupportsSELinuxContextMount(spec *volume.Spec) (bool, error) {
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod) {
		driver, err := GetCSIDriverName(spec)
		if err != nil {
			return false, err
		}
		csiDriver, err := p.getCSIDriver(driver)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if csiDriver.Spec.SELinuxMount != nil {
			return *csiDriver.Spec.SELinuxMount, nil
		}
		return false, nil
	}
	return false, nil
}

// volume.AttachableVolumePlugin methods
var _ volume.AttachableVolumePlugin = &csiPlugin{}

var _ volume.DeviceMountableVolumePlugin = &csiPlugin{}

func (p *csiPlugin) NewAttacher() (volume.Attacher, error) {
	return p.newAttacherDetacher()
}

func (p *csiPlugin) NewDeviceMounter() (volume.DeviceMounter, error) {
	return p.NewAttacher()
}

func (p *csiPlugin) NewDetacher() (volume.Detacher, error) {
	return p.newAttacherDetacher()
}

func (p *csiPlugin) CanAttach(spec *volume.Spec) (bool, error) {
	volumeLifecycleMode, err := p.getVolumeLifecycleMode(spec)
	if err != nil {
		return false, err
	}

	if volumeLifecycleMode == storage.VolumeLifecycleEphemeral {
		klog.V(5).Info(log("plugin.CanAttach = false, ephemeral mode detected for spec %v", spec.Name()))
		return false, nil
	}

	pvSrc, err := getCSISourceFromSpec(spec)
	if err != nil {
		return false, err
	}

	driverName := pvSrc.Driver

	skipAttach, err := p.skipAttach(driverName)
	if err != nil {
		return false, err
	}

	return !skipAttach, nil
}

// CanDeviceMount returns true if the spec supports device mount
func (p *csiPlugin) CanDeviceMount(spec *volume.Spec) (bool, error) {
	volumeLifecycleMode, err := p.getVolumeLifecycleMode(spec)
	if err != nil {
		return false, err
	}

	if volumeLifecycleMode == storage.VolumeLifecycleEphemeral {
		klog.V(5).Info(log("plugin.CanDeviceMount skipped ephemeral mode detected for spec %v", spec.Name()))
		return false, nil
	}

	// Persistent volumes support device mount.
	return true, nil
}

func (p *csiPlugin) NewDeviceUnmounter() (volume.DeviceUnmounter, error) {
	return p.NewDetacher()
}

func (p *csiPlugin) GetDeviceMountRefs(deviceMountPath string) ([]string, error) {
	m := p.host.GetMounter(p.GetPluginName())
	return m.GetMountRefs(deviceMountPath)
}

// BlockVolumePlugin methods
var _ volume.BlockVolumePlugin = &csiPlugin{}

func (p *csiPlugin) NewBlockVolumeMapper(spec *volume.Spec, podRef *api.Pod) (volume.BlockVolumeMapper, error) {
	pvSource, err := getCSISourceFromSpec(spec)
	if err != nil {
		return nil, err
	}
	readOnly, err := getReadOnlyFromSpec(spec)
	if err != nil {
		return nil, err
	}

	klog.V(4).Info(log("setting up block mapper for [volume=%v,driver=%v]", pvSource.VolumeHandle, pvSource.Driver))

	k8s := p.host.GetKubeClient()
	if k8s == nil {
		return nil, errors.New(log("failed to get a kubernetes client"))
	}

	mapper := &csiBlockMapper{
		k8s:        k8s,
		plugin:     p,
		volumeID:   pvSource.VolumeHandle,
		driverName: csiDriverName(pvSource.Driver),
		readOnly:   readOnly,
		spec:       spec,
		specName:   spec.Name(),
		pod:        podRef,
		podUID:     podRef.UID,
	}
	mapper.csiClientGetter.driverName = csiDriverName(pvSource.Driver)

	// Save volume info in pod dir
	dataDir := getVolumeDeviceDataDir(spec.Name(), p.host)

	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, errors.New(log("failed to create data dir %s:  %v", dataDir, err))
	}
	klog.V(4).Info(log("created path successfully [%s]", dataDir))

	blockPath, err := mapper.GetGlobalMapPath(spec)
	if err != nil {
		return nil, errors.New(log("failed to get device path: %v", err))
	}

	mapper.MetricsProvider = NewMetricsCsi(pvSource.VolumeHandle, blockPath+"/"+string(podRef.UID), csiDriverName(pvSource.Driver))

	// persist volume info data for teardown
	node := string(p.host.GetNodeName())
	attachID := getAttachmentName(pvSource.VolumeHandle, pvSource.Driver, node)
	volData := map[string]string{
		volDataKey.specVolID:    spec.Name(),
		volDataKey.volHandle:    pvSource.VolumeHandle,
		volDataKey.driverName:   pvSource.Driver,
		volDataKey.nodeName:     node,
		volDataKey.attachmentID: attachID,
	}

	err = saveVolumeData(dataDir, volDataFileName, volData)
	defer func() {
		// Only if there was an error and volume operation was considered
		// finished, we should remove the directory.
		if err != nil && volumetypes.IsOperationFinishedError(err) {
			// attempt to cleanup volume mount dir.
			if err = removeMountDir(p, dataDir); err != nil {
				klog.Error(log("attacher.MountDevice failed to remove mount dir after error [%s]: %v", dataDir, err))
			}
		}
	}()
	if err != nil {
		errorMsg := log("csi.NewBlockVolumeMapper failed to save volume info data: %v", err)
		klog.Error(errorMsg)
		return nil, errors.New(errorMsg)
	}

	return mapper, nil
}

func (p *csiPlugin) NewBlockVolumeUnmapper(volName string, podUID types.UID) (volume.BlockVolumeUnmapper, error) {
	klog.V(4).Info(log("setting up block unmapper for [Spec=%v, podUID=%v]", volName, podUID))
	unmapper := &csiBlockMapper{
		plugin:   p,
		podUID:   podUID,
		specName: volName,
	}

	// load volume info from file
	dataDir := getVolumeDeviceDataDir(unmapper.specName, p.host)
	data, err := loadVolumeData(dataDir, volDataFileName)
	if err != nil {
		return nil, errors.New(log("unmapper failed to load volume data file [%s]: %v", dataDir, err))
	}
	unmapper.driverName = csiDriverName(data[volDataKey.driverName])
	unmapper.volumeID = data[volDataKey.volHandle]
	unmapper.csiClientGetter.driverName = unmapper.driverName

	return unmapper, nil
}

func (p *csiPlugin) ConstructBlockVolumeSpec(podUID types.UID, specVolName, mapPath string) (*volume.Spec, error) {
	klog.V(4).Infof("plugin.ConstructBlockVolumeSpec [podUID=%s, specVolName=%s, path=%s]", string(podUID), specVolName, mapPath)

	dataDir := getVolumeDeviceDataDir(specVolName, p.host)
	volData, err := loadVolumeData(dataDir, volDataFileName)
	if err != nil {
		return nil, errors.New(log("plugin.ConstructBlockVolumeSpec failed loading volume data using [%s]: %v", mapPath, err))
	}

	klog.V(4).Info(log("plugin.ConstructBlockVolumeSpec extracted [%#v]", volData))

	blockMode := api.PersistentVolumeBlock
	pv := &api.PersistentVolume{
		ObjectMeta: meta.ObjectMeta{
			Name: volData[volDataKey.specVolID],
		},
		Spec: api.PersistentVolumeSpec{
			PersistentVolumeSource: api.PersistentVolumeSource{
				CSI: &api.CSIPersistentVolumeSource{
					Driver:       volData[volDataKey.driverName],
					VolumeHandle: volData[volDataKey.volHandle],
				},
			},
			VolumeMode: &blockMode,
		},
	}

	return volume.NewSpecFromPersistentVolume(pv, false), nil
}

// skipAttach looks up CSIDriver object associated with driver name
// to determine if driver requires attachment volume operation
func (p *csiPlugin) skipAttach(driver string) (bool, error) {
	csiDriver, err := p.getCSIDriver(driver)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Don't skip attach if CSIDriver does not exist
			return false, nil
		}
		return false, err
	}
	if csiDriver.Spec.AttachRequired != nil && *csiDriver.Spec.AttachRequired == false {
		return true, nil
	}
	return false, nil
}

func (p *csiPlugin) getCSIDriver(driver string) (*storage.CSIDriver, error) {
	kletHost, ok := p.host.(volume.KubeletVolumeHost)
	if ok {
		if err := kletHost.WaitForCacheSync(); err != nil {
			return nil, err
		}
	}

	if p.csiDriverLister == nil {
		return nil, errors.New("CSIDriver lister does not exist")
	}
	csiDriver, err := p.csiDriverLister.Get(driver)
	return csiDriver, err
}

// getVolumeLifecycleMode returns the mode for the specified spec: {persistent|ephemeral}.
// 1) If mode cannot be determined, it will default to "persistent".
// 2) If Mode cannot be resolved to either {persistent | ephemeral}, an error is returned
// See https://github.com/kubernetes/enhancements/blob/master/keps/sig-storage/596-csi-inline-volumes/README.md
func (p *csiPlugin) getVolumeLifecycleMode(spec *volume.Spec) (storage.VolumeLifecycleMode, error) {
	// 1) if volume.Spec.Volume.CSI != nil -> mode is ephemeral
	// 2) if volume.Spec.PersistentVolume.Spec.CSI != nil -> persistent
	volSrc, _, err := getSourceFromSpec(spec)
	if err != nil {
		return "", err
	}

	if volSrc != nil {
		return storage.VolumeLifecycleEphemeral, nil
	}
	return storage.VolumeLifecyclePersistent, nil
}

func (p *csiPlugin) getPublishContext(client clientset.Interface, handle, driver, nodeName string) (map[string]string, error) {
	skip, err := p.skipAttach(driver)
	if err != nil {
		return nil, err
	}
	if skip {
		return nil, nil
	}

	attachID := getAttachmentName(handle, driver, nodeName)

	// search for attachment by VolumeAttachment.Spec.Source.PersistentVolumeName
	attachment, err := client.StorageV1().VolumeAttachments().Get(context.TODO(), attachID, meta.GetOptions{})
	if err != nil {
		return nil, err // This err already has enough context ("VolumeAttachment xyz not found")
	}

	if attachment == nil {
		err = errors.New("no existing VolumeAttachment found")
		return nil, err
	}
	return attachment.Status.AttachmentMetadata, nil
}

func (p *csiPlugin) newAttacherDetacher() (*csiAttacher, error) {
	k8s := p.host.GetKubeClient()
	if k8s == nil {
		return nil, errors.New(log("unable to get kubernetes client from host"))
	}

	return &csiAttacher{
		plugin:       p,
		k8s:          k8s,
		watchTimeout: csiTimeout,
	}, nil
}

// podInfoEnabled  check CSIDriver enabled pod info flag
func (p *csiPlugin) podInfoEnabled(driverName string) (bool, error) {
	csiDriver, err := p.getCSIDriver(driverName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Info(log("CSIDriver %q not found, not adding pod information", driverName))
			return false, nil
		}
		return false, err
	}

	// if PodInfoOnMount is not set or false we do not set pod attributes
	if csiDriver.Spec.PodInfoOnMount == nil || *csiDriver.Spec.PodInfoOnMount == false {
		klog.V(4).Info(log("CSIDriver %q does not require pod information", driverName))
		return false, nil
	}
	return true, nil
}

func unregisterDriver(driverName string) error {
	csiDrivers.Delete(driverName)

	if err := nim.UninstallCSIDriver(driverName); err != nil {
		return errors.New(log("Error uninstalling CSI driver: %v", err))
	}

	return nil
}

// waitForAPIServerForever waits forever to get a CSINode instance as a proxy
// for a healthy APIServer
func waitForAPIServerForever(client clientset.Interface, nodeName types.NodeName) error {
	var lastErr error
	// Served object is discarded so no risk to have stale object with benefit to
	// reduce the load on APIServer and etcd.
	opts := meta.GetOptions{}
	util.FromApiserverCache(&opts)
	err := wait.PollImmediateInfinite(time.Second, func() (bool, error) {
		// Get a CSINode from API server to make sure 1) kubelet can reach API server
		// and 2) it has enough permissions. Kubelet may have restricted permissions
		// when it's bootstrapping TLS.
		// https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/
		_, lastErr = client.StorageV1().CSINodes().Get(context.TODO(), string(nodeName), opts)
		if lastErr == nil || apierrors.IsNotFound(lastErr) {
			// API server contacted
			return true, nil
		}
		klog.V(2).Infof("Failed to contact API server when waiting for CSINode publishing: %s", lastErr)
		return false, nil
	})
	if err != nil {
		// In theory this is unreachable, but just in case:
		return fmt.Errorf("%v: %v", err, lastErr)
	}

	return nil
}
