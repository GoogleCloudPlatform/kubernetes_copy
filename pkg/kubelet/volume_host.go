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

package kubelet

import (
	"fmt"
	"net"
	"runtime"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/kubelet/clustertrustbundle"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	"k8s.io/kubernetes/pkg/kubelet/token"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
)

// NewInitializedVolumePluginMgr returns a new instance of
// volume.VolumePluginMgr initialized with kubelets implementation of the
// volume.VolumeHost interface.
//
// kubelet - used by VolumeHost methods to expose kubelet specific parameters
// plugins - used to initialize volumePluginMgr
func NewInitializedVolumePluginMgr(
	kubelet *Kubelet,
	secretManager secret.Manager,
	configMapManager configmap.Manager,
	tokenManager *token.Manager,
	clusterTrustBundleManager clustertrustbundle.Manager,
	plugins []volume.VolumePlugin,
	prober volume.DynamicPluginProber) (*volume.VolumePluginMgr, error) {

	// Initialize csiDriverLister before calling InitPlugins
	var informerFactory informers.SharedInformerFactory
	var csiDriverLister storagelisters.CSIDriverLister
	var csiDriversSynced cache.InformerSynced
	const resyncPeriod = 0
	// Don't initialize if kubeClient is nil
	if kubelet.kubeClient != nil {
		informerFactory = informers.NewSharedInformerFactory(kubelet.kubeClient, resyncPeriod)
		csiDriverInformer := informerFactory.Storage().V1().CSIDrivers()
		csiDriverLister = csiDriverInformer.Lister()
		csiDriversSynced = csiDriverInformer.Informer().HasSynced

	} else {
		klog.InfoS("KubeClient is nil. Skip initialization of CSIDriverLister")
	}

	kvh := &kubeletVolumeHost{
		kubelet:                   kubelet,
		volumePluginMgr:           volume.VolumePluginMgr{},
		secretManager:             secretManager,
		configMapManager:          configMapManager,
		tokenManager:              tokenManager,
		clusterTrustBundleManager: clusterTrustBundleManager,
		informerFactory:           informerFactory,
		csiDriverLister:           csiDriverLister,
		csiDriversSynced:          csiDriversSynced,
		exec:                      utilexec.New(),
	}

	if err := kvh.volumePluginMgr.InitPlugins(plugins, prober, kvh); err != nil {
		return nil, fmt.Errorf(
			"could not initialize volume plugins for KubeletVolumePluginMgr: %v",
			err)
	}

	return &kvh.volumePluginMgr, nil
}

// Compile-time check to ensure kubeletVolumeHost implements the VolumeHost interface
var _ volume.VolumeHost = &kubeletVolumeHost{}
var _ volume.KubeletVolumeHost = &kubeletVolumeHost{}

func (kvh *kubeletVolumeHost) GetPluginDir(pluginName string) string {
	return kvh.kubelet.getPluginDir(pluginName)
}

type kubeletVolumeHost struct {
	kubelet                   *Kubelet
	volumePluginMgr           volume.VolumePluginMgr
	secretManager             secret.Manager
	tokenManager              *token.Manager
	configMapManager          configmap.Manager
	clusterTrustBundleManager clustertrustbundle.Manager
	informerFactory           informers.SharedInformerFactory
	csiDriverLister           storagelisters.CSIDriverLister
	csiDriversSynced          cache.InformerSynced
	exec                      utilexec.Interface
}

func (kvh *kubeletVolumeHost) SetKubeletError(err error) {
	kvh.kubelet.runtimeState.setStorageState(err)
}

func (kvh *kubeletVolumeHost) GetVolumeDevicePluginDir(pluginName string) string {
	return kvh.kubelet.getVolumeDevicePluginDir(pluginName)
}

func (kvh *kubeletVolumeHost) GetPodsDir() string {
	return kvh.kubelet.getPodsDir()
}

func (kvh *kubeletVolumeHost) GetPodVolumeDir(podUID types.UID, pluginName string, volumeName string) string {
	dir := kvh.kubelet.getPodVolumeDir(podUID, pluginName, volumeName)
	if runtime.GOOS == "windows" {
		dir = util.GetWindowsPath(dir)
	}
	return dir
}

func (kvh *kubeletVolumeHost) GetPodVolumeDeviceDir(podUID types.UID, pluginName string) string {
	return kvh.kubelet.getPodVolumeDeviceDir(podUID, pluginName)
}

func (kvh *kubeletVolumeHost) GetPodPluginDir(podUID types.UID, pluginName string) string {
	return kvh.kubelet.getPodPluginDir(podUID, pluginName)
}

func (kvh *kubeletVolumeHost) GetKubeClient() clientset.Interface {
	return kvh.kubelet.kubeClient
}

func (kvh *kubeletVolumeHost) GetSubpather() subpath.Interface {
	return kvh.kubelet.subpather
}

func (kvh *kubeletVolumeHost) GetHostUtil() hostutil.HostUtils {
	return kvh.kubelet.hostutil
}

func (kvh *kubeletVolumeHost) GetInformerFactory() informers.SharedInformerFactory {
	return kvh.informerFactory
}

func (kvh *kubeletVolumeHost) CSIDriverLister() storagelisters.CSIDriverLister {
	return kvh.csiDriverLister
}

func (kvh *kubeletVolumeHost) CSIDriversSynced() cache.InformerSynced {
	return kvh.csiDriversSynced
}

// WaitForCacheSync is a helper function that waits for cache sync for CSIDriverLister
func (kvh *kubeletVolumeHost) WaitForCacheSync() error {
	if kvh.csiDriversSynced == nil {
		klog.ErrorS(nil, "CsiDriversSynced not found on KubeletVolumeHost")
		return fmt.Errorf("csiDriversSynced not found on KubeletVolumeHost")
	}

	synced := []cache.InformerSynced{kvh.csiDriversSynced}
	if !cache.WaitForCacheSync(wait.NeverStop, synced...) {
		klog.InfoS("Failed to wait for cache sync for CSIDriverLister")
		return fmt.Errorf("failed to wait for cache sync for CSIDriverLister")
	}

	return nil
}

func (kvh *kubeletVolumeHost) NewWrapperMounter(
	volName string,
	spec volume.Spec,
	pod *v1.Pod) (volume.Mounter, error) {
	// The name of wrapper volume is set to "wrapped_{wrapped_volume_name}"
	wrapperVolumeName := "wrapped_" + volName
	if spec.Volume != nil {
		spec.Volume.Name = wrapperVolumeName
	}

	return kvh.kubelet.newVolumeMounterFromPlugins(&spec, pod)
}

func (kvh *kubeletVolumeHost) NewWrapperUnmounter(volName string, spec volume.Spec, podUID types.UID) (volume.Unmounter, error) {
	// The name of wrapper volume is set to "wrapped_{wrapped_volume_name}"
	wrapperVolumeName := "wrapped_" + volName
	if spec.Volume != nil {
		spec.Volume.Name = wrapperVolumeName
	}

	plugin, err := kvh.kubelet.volumePluginMgr.FindPluginBySpec(&spec)
	if err != nil {
		return nil, err
	}

	return plugin.NewUnmounter(spec.Name(), podUID)
}

func (kvh *kubeletVolumeHost) GetMounter(pluginName string) mount.Interface {
	return kvh.kubelet.mounter
}

func (kvh *kubeletVolumeHost) GetHostName() string {
	return kvh.kubelet.hostname
}

func (kvh *kubeletVolumeHost) GetHostIP() (net.IP, error) {
	hostIPs, err := kvh.kubelet.GetHostIPs()
	if err != nil {
		return nil, err
	}
	return hostIPs[0], err
}

func (kvh *kubeletVolumeHost) GetNodeAllocatable() (v1.ResourceList, error) {
	node, err := kvh.kubelet.getNodeAnyWay()
	if err != nil {
		return nil, fmt.Errorf("error retrieving node: %v", err)
	}
	return node.Status.Allocatable, nil
}

func (kvh *kubeletVolumeHost) GetSecretFunc() func(namespace, name string) (*v1.Secret, error) {
	if kvh.secretManager != nil {
		return kvh.secretManager.GetSecret
	}
	return func(namespace, name string) (*v1.Secret, error) {
		return nil, fmt.Errorf("not supported due to running kubelet in standalone mode")
	}
}

func (kvh *kubeletVolumeHost) GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error) {
	if kvh.configMapManager != nil {
		return kvh.configMapManager.GetConfigMap
	}
	return func(namespace, name string) (*v1.ConfigMap, error) {
		return nil, fmt.Errorf("not supported due to running kubelet in standalone mode")
	}
}

func (kvh *kubeletVolumeHost) GetServiceAccountTokenFunc() func(namespace, name string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	return kvh.tokenManager.GetServiceAccountToken
}

func (kvh *kubeletVolumeHost) DeleteServiceAccountTokenFunc() func(podUID types.UID) {
	return kvh.tokenManager.DeleteServiceAccountToken
}

func (kvh *kubeletVolumeHost) GetTrustAnchorsByName(name string, allowMissing bool) ([]byte, error) {
	return kvh.clusterTrustBundleManager.GetTrustAnchorsByName(name, allowMissing)
}

func (kvh *kubeletVolumeHost) GetTrustAnchorsBySigner(signerName string, labelSelector *metav1.LabelSelector, allowMissing bool) ([]byte, error) {
	return kvh.clusterTrustBundleManager.GetTrustAnchorsBySigner(signerName, labelSelector, allowMissing)
}

func (kvh *kubeletVolumeHost) GetNodeLabels() (map[string]string, error) {
	node, err := kvh.kubelet.GetNode()
	if err != nil {
		return nil, fmt.Errorf("error retrieving node: %v", err)
	}
	return node.Labels, nil
}

func (kvh *kubeletVolumeHost) GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error) {
	node, err := kvh.kubelet.GetNode()
	if err != nil {
		return nil, fmt.Errorf("error retrieving node: %v", err)
	}
	attachedVolumes := node.Status.VolumesAttached
	result := map[v1.UniqueVolumeName]string{}
	for i := range attachedVolumes {
		attachedVolume := attachedVolumes[i]
		result[attachedVolume.Name] = attachedVolume.DevicePath
	}
	return result, nil
}

func (kvh *kubeletVolumeHost) GetNodeName() types.NodeName {
	return kvh.kubelet.nodeName
}

func (kvh *kubeletVolumeHost) GetEventRecorder() events.EventRecorder {
	return kvh.kubelet.recorder
}

func (kvh *kubeletVolumeHost) GetExec(pluginName string) utilexec.Interface {
	return kvh.exec
}
