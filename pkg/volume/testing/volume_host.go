/*
Copyright 2020 The Kubernetes Authors.

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

package testing

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	csilibplugins "k8s.io/csi-translation-lib/plugins"
	. "k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
)

type FakeVolumeHost interface {
	VolumeHost

	GetPluginMgr() *VolumePluginMgr
}

// fakeVolumeHost is useful for testing volume plugins.
// TODO: Extract fields specific to fakeKubeletVolumeHost and fakeAttachDetachVolumeHost.
type fakeVolumeHost struct {
	rootDir                string
	kubeClient             clientset.Interface
	pluginMgr              *VolumePluginMgr
	mounter                mount.Interface
	hostUtil               hostutil.HostUtils
	exec                   *testingexec.FakeExec
	nodeLabels             map[string]string
	nodeName               string
	subpather              subpath.Interface
	node                   *v1.Node
	csiDriverLister        storagelistersv1.CSIDriverLister
	volumeAttachmentLister storagelistersv1.VolumeAttachmentLister
	informerFactory        informers.SharedInformerFactory
	kubeletErr             error
	mux                    sync.Mutex
}

var _ VolumeHost = &fakeVolumeHost{}
var _ FakeVolumeHost = &fakeVolumeHost{}

func NewFakeVolumeHost(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin) FakeVolumeHost {
	return newFakeVolumeHost(t, rootDir, kubeClient, plugins, nil, "", nil, nil)
}

func NewFakeVolumeHostWithCloudProvider(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin) FakeVolumeHost {
	return newFakeVolumeHost(t, rootDir, kubeClient, plugins, nil, "", nil, nil)
}

func NewFakeVolumeHostWithCSINodeName(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin, nodeName string, driverLister storagelistersv1.CSIDriverLister, volumeAttachLister storagelistersv1.VolumeAttachmentLister) FakeVolumeHost {
	return newFakeVolumeHost(t, rootDir, kubeClient, plugins, nil, nodeName, driverLister, volumeAttachLister)
}

func newFakeVolumeHost(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin, pathToTypeMap map[string]hostutil.FileType, nodeName string, driverLister storagelistersv1.CSIDriverLister, volumeAttachLister storagelistersv1.VolumeAttachmentLister) FakeVolumeHost {
	host := &fakeVolumeHost{rootDir: rootDir, kubeClient: kubeClient, nodeName: nodeName, csiDriverLister: driverLister, volumeAttachmentLister: volumeAttachLister}
	host.mounter = mount.NewFakeMounter(nil)
	host.hostUtil = hostutil.NewFakeHostUtil(pathToTypeMap)
	host.exec = &testingexec.FakeExec{DisableScripts: true}
	host.pluginMgr = &VolumePluginMgr{}
	if err := host.pluginMgr.InitPlugins(plugins, nil /* prober */, host); err != nil {
		t.Fatalf("Failed to init plugins while creating fake volume host: %v", err)
	}
	host.subpather = &subpath.FakeSubpath{}
	host.informerFactory = informers.NewSharedInformerFactory(kubeClient, time.Minute)
	// Wait until the InitPlugins setup is finished before returning from this setup func
	if err := host.WaitForKubeletErrNil(); err != nil {
		t.Fatalf("Failed to wait for kubelet err to be nil while creating fake volume host: %v", err)
	}
	return host
}

func (f *fakeVolumeHost) GetPluginDir(podUID string) string {
	return filepath.Join(f.rootDir, "plugins", podUID)
}

func (f *fakeVolumeHost) GetVolumeDevicePluginDir(pluginName string) string {
	return filepath.Join(f.rootDir, "plugins", pluginName, "volumeDevices")
}

func (f *fakeVolumeHost) GetPodsDir() string {
	return filepath.Join(f.rootDir, "pods")
}

func (f *fakeVolumeHost) GetPodVolumeDir(podUID types.UID, pluginName, volumeName string) string {
	return filepath.Join(f.rootDir, "pods", string(podUID), "volumes", pluginName, volumeName)
}

func (f *fakeVolumeHost) GetPodVolumeDeviceDir(podUID types.UID, pluginName string) string {
	return filepath.Join(f.rootDir, "pods", string(podUID), "volumeDevices", pluginName)
}

func (f *fakeVolumeHost) GetPodPluginDir(podUID types.UID, pluginName string) string {
	return filepath.Join(f.rootDir, "pods", string(podUID), "plugins", pluginName)
}

func (f *fakeVolumeHost) GetKubeClient() clientset.Interface {
	return f.kubeClient
}

func (f *fakeVolumeHost) GetMounter(pluginName string) mount.Interface {
	return f.mounter
}

func (f *fakeVolumeHost) GetSubpather() subpath.Interface {
	return f.subpather
}

func (f *fakeVolumeHost) GetPluginMgr() *VolumePluginMgr {
	return f.pluginMgr
}

func (f *fakeVolumeHost) GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error) {
	return map[v1.UniqueVolumeName]string{}, nil
}

func (f *fakeVolumeHost) NewWrapperMounter(volName string, spec Spec, pod *v1.Pod) (Mounter, error) {
	// The name of wrapper volume is set to "wrapped_{wrapped_volume_name}"
	wrapperVolumeName := "wrapped_" + volName
	if spec.Volume != nil {
		spec.Volume.Name = wrapperVolumeName
	}
	plug, err := f.pluginMgr.FindPluginBySpec(&spec)
	if err != nil {
		return nil, err
	}
	return plug.NewMounter(&spec, pod)
}

func (f *fakeVolumeHost) NewWrapperUnmounter(volName string, spec Spec, podUID types.UID) (Unmounter, error) {
	// The name of wrapper volume is set to "wrapped_{wrapped_volume_name}"
	wrapperVolumeName := "wrapped_" + volName
	if spec.Volume != nil {
		spec.Volume.Name = wrapperVolumeName
	}
	plug, err := f.pluginMgr.FindPluginBySpec(&spec)
	if err != nil {
		return nil, err
	}
	return plug.NewUnmounter(spec.Name(), podUID)
}

// Returns the hostname of the host kubelet is running on
func (f *fakeVolumeHost) GetHostName() string {
	return "fakeHostName"
}

// Returns host IP or nil in the case of error.
func (f *fakeVolumeHost) GetHostIP() (net.IP, error) {
	return nil, fmt.Errorf("GetHostIP() not implemented")
}

func (f *fakeVolumeHost) GetNodeAllocatable() (v1.ResourceList, error) {
	return v1.ResourceList{}, nil
}

func (f *fakeVolumeHost) GetSecretFunc() func(namespace, name string) (*v1.Secret, error) {
	return func(namespace, name string) (*v1.Secret, error) {
		return f.kubeClient.CoreV1().Secrets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	}
}

func (f *fakeVolumeHost) GetExec(pluginName string) exec.Interface {
	return f.exec
}

func (f *fakeVolumeHost) GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error) {
	return func(namespace, name string) (*v1.ConfigMap, error) {
		return f.kubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	}
}

func (f *fakeVolumeHost) GetServiceAccountTokenFunc() func(string, string, *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	return func(namespace, name string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		return f.kubeClient.CoreV1().ServiceAccounts(namespace).CreateToken(context.TODO(), name, tr, metav1.CreateOptions{})
	}
}

func (f *fakeVolumeHost) DeleteServiceAccountTokenFunc() func(types.UID) {
	return func(types.UID) {}
}

func (f *fakeVolumeHost) GetNodeLabels() (map[string]string, error) {
	if f.nodeLabels == nil {
		f.nodeLabels = map[string]string{"test-label": "test-value"}
	}
	return f.nodeLabels, nil
}

func (f *fakeVolumeHost) GetNodeName() types.NodeName {
	return types.NodeName(f.nodeName)
}

func (f *fakeVolumeHost) GetEventRecorder() record.EventRecorder {
	return nil
}

func (f *fakeVolumeHost) ScriptCommands(scripts []CommandScript) {
	ScriptCommands(f.exec, scripts)
}

func (f *fakeVolumeHost) WaitForKubeletErrNil() error {
	return wait.PollImmediate(10*time.Millisecond, 10*time.Second, func() (bool, error) {
		f.mux.Lock()
		defer f.mux.Unlock()
		return f.kubeletErr == nil, nil
	})
}

type fakeAttachDetachVolumeHost struct {
	fakeVolumeHost
}

var _ AttachDetachVolumeHost = &fakeAttachDetachVolumeHost{}
var _ FakeVolumeHost = &fakeAttachDetachVolumeHost{}

func NewFakeAttachDetachVolumeHostWithCSINodeName(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin, nodeName string, driverLister storagelistersv1.CSIDriverLister, volumeAttachLister storagelistersv1.VolumeAttachmentLister) FakeVolumeHost {
	return newFakeAttachDetachVolumeHost(t, rootDir, kubeClient, plugins, nil, nodeName, driverLister, volumeAttachLister)
}

func newFakeAttachDetachVolumeHost(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin, pathToTypeMap map[string]hostutil.FileType, nodeName string, driverLister storagelistersv1.CSIDriverLister, volumeAttachLister storagelistersv1.VolumeAttachmentLister) FakeVolumeHost {
	host := &fakeAttachDetachVolumeHost{}
	host.rootDir = rootDir
	host.kubeClient = kubeClient
	host.nodeName = nodeName
	host.csiDriverLister = driverLister
	host.volumeAttachmentLister = volumeAttachLister
	host.mounter = mount.NewFakeMounter(nil)
	host.hostUtil = hostutil.NewFakeHostUtil(pathToTypeMap)
	host.exec = &testingexec.FakeExec{DisableScripts: true}
	host.pluginMgr = &VolumePluginMgr{}
	if err := host.pluginMgr.InitPlugins(plugins, nil /* prober */, host); err != nil {
		t.Fatalf("Failed to init plugins while creating fake volume host: %v", err)
	}
	host.subpather = &subpath.FakeSubpath{}
	host.informerFactory = informers.NewSharedInformerFactory(kubeClient, time.Minute)
	// Wait until the InitPlugins setup is finished before returning from this setup func
	if err := host.WaitForKubeletErrNil(); err != nil {
		t.Fatalf("Failed to wait for kubelet err to be nil while creating fake volume host: %v", err)
	}
	return host
}

func (f *fakeAttachDetachVolumeHost) CSINodeLister() storagelistersv1.CSINodeLister {
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{Name: f.nodeName},
		Spec: storagev1.CSINodeSpec{
			Drivers: []storagev1.CSINodeDriver{},
		},
	}
	enableMigrationOnNode(csiNode, csilibplugins.GCEPDInTreePluginName)
	return getFakeCSINodeLister(csiNode)
}

func enableMigrationOnNode(csiNode *storagev1.CSINode, pluginName string) {
	nodeInfoAnnotations := csiNode.GetAnnotations()
	if nodeInfoAnnotations == nil {
		nodeInfoAnnotations = map[string]string{}
	}

	newAnnotationSet := sets.New[string]()
	newAnnotationSet.Insert(pluginName)
	nas := strings.Join(sets.List(newAnnotationSet), ",")
	nodeInfoAnnotations[v1.MigratedPluginsAnnotationKey] = nas

	csiNode.Annotations = nodeInfoAnnotations
}

func (f *fakeAttachDetachVolumeHost) CSIDriverLister() storagelistersv1.CSIDriverLister {
	return f.csiDriverLister
}

func (f *fakeAttachDetachVolumeHost) VolumeAttachmentLister() storagelistersv1.VolumeAttachmentLister {
	return f.volumeAttachmentLister
}

func (f *fakeAttachDetachVolumeHost) IsAttachDetachController() bool {
	return true
}

type fakeKubeletVolumeHost struct {
	fakeVolumeHost
}

var _ KubeletVolumeHost = &fakeKubeletVolumeHost{}
var _ FakeVolumeHost = &fakeKubeletVolumeHost{}

func NewFakeKubeletVolumeHost(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin) *fakeKubeletVolumeHost {
	return newFakeKubeletVolumeHost(t, rootDir, kubeClient, plugins, nil, "", nil, nil)
}

func NewFakeKubeletVolumeHostWithCSINodeName(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin, nodeName string, driverLister storagelistersv1.CSIDriverLister, volumeAttachLister storagelistersv1.VolumeAttachmentLister) *fakeKubeletVolumeHost {
	return newFakeKubeletVolumeHost(t, rootDir, kubeClient, plugins, nil, nodeName, driverLister, volumeAttachLister)
}

func NewFakeKubeletVolumeHostWithMounterFSType(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin, pathToTypeMap map[string]hostutil.FileType) *fakeKubeletVolumeHost {
	return newFakeKubeletVolumeHost(t, rootDir, kubeClient, plugins, pathToTypeMap, "", nil, nil)
}

func newFakeKubeletVolumeHost(t *testing.T, rootDir string, kubeClient clientset.Interface, plugins []VolumePlugin, pathToTypeMap map[string]hostutil.FileType, nodeName string, driverLister storagelistersv1.CSIDriverLister, volumeAttachLister storagelistersv1.VolumeAttachmentLister) *fakeKubeletVolumeHost {
	host := &fakeKubeletVolumeHost{}
	host.rootDir = rootDir
	host.kubeClient = kubeClient
	host.nodeName = nodeName
	host.csiDriverLister = driverLister
	host.volumeAttachmentLister = volumeAttachLister
	host.mounter = mount.NewFakeMounter(nil)
	host.hostUtil = hostutil.NewFakeHostUtil(pathToTypeMap)
	host.exec = &testingexec.FakeExec{DisableScripts: true}
	host.pluginMgr = &VolumePluginMgr{}
	if err := host.pluginMgr.InitPlugins(plugins, nil /* prober */, host); err != nil {
		t.Fatalf("Failed to init plugins while creating fake volume host: %v", err)
	}
	host.subpather = &subpath.FakeSubpath{}
	host.informerFactory = informers.NewSharedInformerFactory(kubeClient, time.Minute)
	// Wait until the InitPlugins setup is finished before returning from this setup func
	if err := host.WaitForKubeletErrNil(); err != nil {
		t.Fatalf("Failed to wait for kubelet err to be nil while creating fake volume host: %v", err)
	}
	return host
}

func (f *fakeKubeletVolumeHost) WithNode(node *v1.Node) *fakeKubeletVolumeHost {
	f.node = node
	return f
}

type CSINodeLister []storagev1.CSINode

// Get returns a fake CSINode object.
func (n CSINodeLister) Get(name string) (*storagev1.CSINode, error) {
	for _, cn := range n {
		if cn.Name == name {
			return &cn, nil
		}
	}
	return nil, fmt.Errorf("csiNode %q not found", name)
}

// List lists all CSINodes in the indexer.
func (n CSINodeLister) List(selector labels.Selector) (ret []*storagev1.CSINode, err error) {
	return nil, fmt.Errorf("not implemented")
}

func getFakeCSINodeLister(csiNode *storagev1.CSINode) CSINodeLister {
	csiNodeLister := CSINodeLister{}
	if csiNode != nil {
		csiNodeLister = append(csiNodeLister, *csiNode.DeepCopy())
	}
	return csiNodeLister
}

func (f *fakeKubeletVolumeHost) SetKubeletError(err error) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.kubeletErr = err
}

func (f *fakeKubeletVolumeHost) GetInformerFactory() informers.SharedInformerFactory {
	return f.informerFactory
}

func (f *fakeKubeletVolumeHost) GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error) {
	result := map[v1.UniqueVolumeName]string{}
	if f.node != nil {
		for _, av := range f.node.Status.VolumesAttached {
			result[av.Name] = av.DevicePath
		}
	}

	return result, nil
}

func (f *fakeKubeletVolumeHost) CSIDriverLister() storagelistersv1.CSIDriverLister {
	return f.csiDriverLister
}

func (f *fakeKubeletVolumeHost) CSIDriversSynced() cache.InformerSynced {
	// not needed for testing
	return nil
}

func (f *fakeKubeletVolumeHost) WaitForCacheSync() error {
	return nil
}

func (f *fakeKubeletVolumeHost) GetHostUtil() hostutil.HostUtils {
	return f.hostUtil
}

func (f *fakeKubeletVolumeHost) GetTrustAnchorsByName(name string, allowMissing bool) ([]byte, error) {
	ctb, err := f.kubeClient.CertificatesV1beta1().ClusterTrustBundles().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("while getting ClusterTrustBundle %s: %w", name, err)
	}

	return []byte(ctb.Spec.TrustBundle), nil
}

// Note: we do none of the deduplication and sorting that the real deal should do.
func (f *fakeKubeletVolumeHost) GetTrustAnchorsBySigner(signerName string, labelSelector *metav1.LabelSelector, allowMissing bool) ([]byte, error) {
	ctbList, err := f.kubeClient.CertificatesV1beta1().ClusterTrustBundles().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("while listing all ClusterTrustBundles: %w", err)
	}

	fullSet := bytes.Buffer{}
	for i, ctb := range ctbList.Items {
		fullSet.WriteString(ctb.Spec.TrustBundle)
		if i != len(ctbList.Items)-1 {
			fullSet.WriteString("\n")
		}
	}

	return fullSet.Bytes(), nil
}
