/*
Copyright 2014 The Kubernetes Authors.

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

package hostpath

import (
	"fmt"
	"os"
	"regexp"

	"k8s.io/klog/v2"

	"github.com/opencontainers/selinux/go-selinux"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/recyclerclient"
	"k8s.io/kubernetes/pkg/volume/validation"
	"k8s.io/mount-utils"
)

// ProbeVolumePlugins is the primary entrypoint for volume plugins.
// The volumeConfig arg provides the ability to configure volume behavior.  It is implemented as a pointer to allow nils.
// The hostPathPlugin is used to store the volumeConfig and give it, when needed, to the func that Recycles.
// Tests that exercise recycling should not use this func but instead use ProbeRecyclablePlugins() to override default behavior.
func ProbeVolumePlugins(volumeConfig volume.VolumeConfig) []volume.VolumePlugin {
	return []volume.VolumePlugin{
		&hostPathPlugin{
			host:   nil,
			config: volumeConfig,
		},
	}
}

func FakeProbeVolumePlugins(volumeConfig volume.VolumeConfig) []volume.VolumePlugin {
	return []volume.VolumePlugin{
		&hostPathPlugin{
			host:          nil,
			config:        volumeConfig,
			noTypeChecker: true,
		},
	}
}

type hostPathPlugin struct {
	host          volume.VolumeHost
	config        volume.VolumeConfig
	noTypeChecker bool
}

var _ volume.VolumePlugin = &hostPathPlugin{}
var _ volume.PersistentVolumePlugin = &hostPathPlugin{}
var _ volume.RecyclableVolumePlugin = &hostPathPlugin{}
var _ volume.DeletableVolumePlugin = &hostPathPlugin{}
var _ volume.ProvisionableVolumePlugin = &hostPathPlugin{}

const (
	hostPathPluginName = "kubernetes.io/host-path"
)

func (plugin *hostPathPlugin) Init(host volume.VolumeHost) error {
	plugin.host = host
	return nil
}

func (plugin *hostPathPlugin) GetPluginName() string {
	return hostPathPluginName
}

func (plugin *hostPathPlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	return volumeSource.Path, nil
}

func (plugin *hostPathPlugin) CanSupport(spec *volume.Spec) bool {
	return (spec.PersistentVolume != nil && spec.PersistentVolume.Spec.HostPath != nil) ||
		(spec.Volume != nil && spec.Volume.HostPath != nil)
}

func (plugin *hostPathPlugin) RequiresRemount(spec *volume.Spec) bool {
	return false
}

func (plugin *hostPathPlugin) SupportsMountOption() bool {
	return false
}

func (plugin *hostPathPlugin) SupportsSELinuxContextMount(spec *volume.Spec) (bool, error) {
	return false, nil
}

func (plugin *hostPathPlugin) GetAccessModes() []v1.PersistentVolumeAccessMode {
	return []v1.PersistentVolumeAccessMode{
		v1.ReadWriteOnce,
	}
}

func (plugin *hostPathPlugin) NewMounter(spec *volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	hostPathVolumeSource, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return nil, err
	}

	path := hostPathVolumeSource.Path
	pathType := new(v1.HostPathType)
	if hostPathVolumeSource.Type == nil {
		*pathType = v1.HostPathUnset
	} else {
		pathType = hostPathVolumeSource.Type
	}
	kvh, ok := plugin.host.(volume.KubeletVolumeHost)
	if !ok {
		return nil, fmt.Errorf("plugin volume host does not implement KubeletVolumeHost interface")
	}
	return &hostPathMounter{
		hostPath:      &hostPath{path: path, pathType: pathType},
		readOnly:      readOnly,
		mounter:       plugin.host.GetMounter(plugin.GetPluginName()),
		hu:            kvh.GetHostUtil(),
		noTypeChecker: plugin.noTypeChecker,
	}, nil
}

func (plugin *hostPathPlugin) NewUnmounter(volName string, podUID types.UID) (volume.Unmounter, error) {
	return &hostPathUnmounter{&hostPath{
		path: "",
	}}, nil
}

// Recycle recycles/scrubs clean a HostPath volume.
// Recycle blocks until the pod has completed or any error occurs.
// HostPath recycling only works in single node clusters and is meant for testing purposes only.
func (plugin *hostPathPlugin) Recycle(pvName string, spec *volume.Spec, eventRecorder recyclerclient.RecycleEventRecorder) error {
	if spec.PersistentVolume == nil || spec.PersistentVolume.Spec.HostPath == nil {
		return fmt.Errorf("spec.PersistentVolume.Spec.HostPath is nil")
	}

	pod := plugin.config.RecyclerPodTemplate
	timeout := util.CalculateTimeoutForVolume(plugin.config.RecyclerMinimumTimeout, plugin.config.RecyclerTimeoutIncrement, spec.PersistentVolume)
	// overrides
	pod.Spec.ActiveDeadlineSeconds = &timeout
	pod.Spec.Volumes[0].VolumeSource = v1.VolumeSource{
		HostPath: &v1.HostPathVolumeSource{
			Path: spec.PersistentVolume.Spec.HostPath.Path,
		},
	}
	return recyclerclient.RecycleVolumeByWatchingPodUntilCompletion(pvName, pod, plugin.host.GetKubeClient(), eventRecorder)
}

func (plugin *hostPathPlugin) NewDeleter(logger klog.Logger, spec *volume.Spec) (volume.Deleter, error) {
	return newDeleter(spec, plugin.host)
}

func (plugin *hostPathPlugin) NewProvisioner(logger klog.Logger, options volume.VolumeOptions) (volume.Provisioner, error) {
	if !plugin.config.ProvisioningEnabled {
		return nil, fmt.Errorf("provisioning in volume plugin %q is disabled", plugin.GetPluginName())
	}
	return newProvisioner(options, plugin.host, plugin)
}

func (plugin *hostPathPlugin) ConstructVolumeSpec(volumeName, mountPath string) (volume.ReconstructedVolume, error) {
	hostPathVolume := &v1.Volume{
		Name: volumeName,
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: volumeName,
			},
		},
	}
	return volume.ReconstructedVolume{
		Spec: volume.NewSpecFromVolume(hostPathVolume),
	}, nil
}

func newDeleter(spec *volume.Spec, host volume.VolumeHost) (volume.Deleter, error) {
	if spec.PersistentVolume != nil && spec.PersistentVolume.Spec.HostPath == nil {
		return nil, fmt.Errorf("spec.PersistentVolumeSource.HostPath is nil")
	}
	path := spec.PersistentVolume.Spec.HostPath.Path
	return &hostPathDeleter{name: spec.Name(), path: path, host: host}, nil
}

func newProvisioner(options volume.VolumeOptions, host volume.VolumeHost, plugin *hostPathPlugin) (volume.Provisioner, error) {
	return &hostPathProvisioner{options: options, host: host, plugin: plugin, basePath: "hostpath_pv"}, nil
}

// HostPath volumes represent a bare host file or directory mount.
// The direct at the specified path will be directly exposed to the container.
type hostPath struct {
	path     string
	pathType *v1.HostPathType
	volume.MetricsNil
}

func (hp *hostPath) GetPath() string {
	return hp.path
}

type hostPathMounter struct {
	*hostPath
	readOnly      bool
	mounter       mount.Interface
	hu            hostutil.HostUtils
	noTypeChecker bool
}

var _ volume.Mounter = &hostPathMounter{}

func (b *hostPathMounter) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:       b.readOnly,
		Managed:        false,
		SELinuxRelabel: false,
	}
}

// SetUp does nothing.
func (b *hostPathMounter) SetUp(mounterArgs volume.MounterArgs) error {
	err := validation.ValidatePathNoBacksteps(b.GetPath())
	if err != nil {
		return fmt.Errorf("invalid HostPath `%s`: %v", b.GetPath(), err)
	}

	if *b.pathType == v1.HostPathUnset {
		return nil
	}
	if b.noTypeChecker {
		return nil
	} else {
		return checkType(b.GetPath(), b.pathType, b.hu)
	}
}

// SetUpAt does not make sense for host paths - probably programmer error.
func (b *hostPathMounter) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	return fmt.Errorf("SetUpAt() does not make sense for host paths")
}

func (b *hostPathMounter) GetPath() string {
	return b.path
}

type hostPathUnmounter struct {
	*hostPath
}

var _ volume.Unmounter = &hostPathUnmounter{}

// TearDown does nothing.
func (c *hostPathUnmounter) TearDown() error {
	return nil
}

// TearDownAt does not make sense for host paths - probably programmer error.
func (c *hostPathUnmounter) TearDownAt(dir string) error {
	return fmt.Errorf("TearDownAt() does not make sense for host paths")
}

// hostPathProvisioner implements a Provisioner for the HostPath plugin
// This implementation is meant for testing only and only works in a single node cluster.
type hostPathProvisioner struct {
	host     volume.VolumeHost
	options  volume.VolumeOptions
	plugin   *hostPathPlugin
	basePath string
}

// Create for hostPath simply creates a local /tmp/%/%s directory as a new PersistentVolume, default /tmp/hostpath_pv/%s.
// This Provisioner is meant for development and testing only and WILL NOT WORK in a multi-node cluster.
func (r *hostPathProvisioner) Provision(selectedNode *v1.Node, allowedTopologies []v1.TopologySelectorTerm) (*v1.PersistentVolume, error) {
	if util.CheckPersistentVolumeClaimModeBlock(r.options.PVC) {
		return nil, fmt.Errorf("%s does not support block volume provisioning", r.plugin.GetPluginName())
	}

	fullpath := fmt.Sprintf("/tmp/%s/%s", r.basePath, uuid.NewUUID())

	capacity := r.options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.options.PVName,
			Annotations: map[string]string{
				util.VolumeDynamicallyCreatedByKey: "hostpath-dynamic-provisioner",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: r.options.PersistentVolumeReclaimPolicy,
			AccessModes:                   r.options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): capacity,
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: fullpath,
				},
			},
		},
	}
	if len(r.options.PVC.Spec.AccessModes) == 0 {
		pv.Spec.AccessModes = r.plugin.GetAccessModes()
	}

	if err := os.MkdirAll(pv.Spec.HostPath.Path, 0750); err != nil {
		return nil, err
	}
	if selinux.GetEnabled() {
		err := selinux.SetFileLabel(pv.Spec.HostPath.Path, config.KubeletContainersSharedSELinuxLabel)
		if err != nil {
			return nil, fmt.Errorf("failed to set selinux label for %q: %v", pv.Spec.HostPath.Path, err)
		}
	}

	return pv, nil
}

// hostPathDeleter deletes a hostPath PV from the cluster.
// This deleter only works on a single host cluster and is for testing purposes only.
type hostPathDeleter struct {
	name string
	path string
	host volume.VolumeHost
	volume.MetricsNil
}

func (r *hostPathDeleter) GetPath() string {
	return r.path
}

// Delete for hostPath removes the local directory so long as it is beneath /tmp/*.
// THIS IS FOR TESTING AND LOCAL DEVELOPMENT ONLY!  This message should scare you away from using
// this deleter for anything other than development and testing.
func (r *hostPathDeleter) Delete() error {
	regexp := regexp.MustCompile("/tmp/.+")
	if !regexp.MatchString(r.GetPath()) {
		return fmt.Errorf("host_path deleter only supports /tmp/.+ but received provided %s", r.GetPath())
	}
	return os.RemoveAll(r.GetPath())
}

func getVolumeSource(spec *volume.Spec) (*v1.HostPathVolumeSource, bool, error) {
	if spec.Volume != nil && spec.Volume.HostPath != nil {
		return spec.Volume.HostPath, spec.ReadOnly, nil
	} else if spec.PersistentVolume != nil &&
		spec.PersistentVolume.Spec.HostPath != nil {
		return spec.PersistentVolume.Spec.HostPath, spec.ReadOnly, nil
	}

	return nil, false, fmt.Errorf("spec does not reference an HostPath volume type")
}

type hostPathTypeChecker interface {
	Exists() bool
	IsFile() bool
	MakeFile() error
	IsDir() bool
	MakeDir() error
	IsBlock() bool
	IsChar() bool
	IsSocket() bool
	GetPath() string
	GetType() (string, error)
}

type fileTypeChecker struct {
	path string
	hu   hostutil.HostUtils
}

func (ftc *fileTypeChecker) Exists() bool {
	exists, err := ftc.hu.PathExists(ftc.path)
	return exists && err == nil
}

func (ftc *fileTypeChecker) GetType() (string, error) {
	pathType, err := ftc.hu.GetFileType(ftc.path)
	return string(pathType), err
}

func (ftc *fileTypeChecker) IsFile() bool {
	if !ftc.Exists() {
		return false
	}
	pathType, err := ftc.GetType()
	if err != nil {
		return false
	}
	return pathType == string(v1.HostPathFile)
}

func (ftc *fileTypeChecker) MakeFile() error {
	return makeFile(ftc.path)
}

func (ftc *fileTypeChecker) IsDir() bool {
	if !ftc.Exists() {
		return false
	}
	pathType, err := ftc.GetType()
	if err != nil {
		return false
	}
	return pathType == string(v1.HostPathDirectory)
}

func (ftc *fileTypeChecker) MakeDir() error {
	return makeDir(ftc.path)
}

func (ftc *fileTypeChecker) IsBlock() bool {
	blkDevType, err := ftc.GetType()
	if err != nil {
		return false
	}
	return blkDevType == string(v1.HostPathBlockDev)
}

func (ftc *fileTypeChecker) IsChar() bool {
	charDevType, err := ftc.GetType()
	if err != nil {
		return false
	}
	return charDevType == string(v1.HostPathCharDev)
}

func (ftc *fileTypeChecker) IsSocket() bool {
	socketType, err := ftc.GetType()
	if err != nil {
		return false
	}
	return socketType == string(v1.HostPathSocket)
}

func (ftc *fileTypeChecker) GetPath() string {
	return ftc.path
}

func newFileTypeChecker(path string, hu hostutil.HostUtils) hostPathTypeChecker {
	return &fileTypeChecker{path: path, hu: hu}
}

// checkType checks whether the given path is the exact pathType
func checkType(path string, pathType *v1.HostPathType, hu hostutil.HostUtils) error {
	return checkTypeInternal(newFileTypeChecker(path, hu), pathType)
}

func typeMatchedOrError(ftc hostPathTypeChecker, match func() bool, expected string) error {
	if !match() {
		pathType, err := ftc.GetType()
		if err != nil {
			return fmt.Errorf("hostPath type check failed, %s is not a %s, unable to determine its type: %w", ftc.GetPath(), expected, err)
		}
		return fmt.Errorf("hostPath type check failed: %s is not a %s, it's a %s", ftc.GetPath(), expected, pathType)
	}
	return nil
}

func checkTypeInternal(ftc hostPathTypeChecker, pathType *v1.HostPathType) error {
	switch *pathType {
	case v1.HostPathDirectoryOrCreate:
		if !ftc.Exists() {
			return ftc.MakeDir()
		}
		fallthrough
	case v1.HostPathDirectory:
		return typeMatchedOrError(ftc, ftc.IsDir, "directory")
	case v1.HostPathFileOrCreate:
		if !ftc.Exists() {
			return ftc.MakeFile()
		}
		fallthrough
	case v1.HostPathFile:
		return typeMatchedOrError(ftc, ftc.IsFile, "file")
	case v1.HostPathSocket:
		return typeMatchedOrError(ftc, ftc.IsSocket, "socket file")
	case v1.HostPathCharDev:
		return typeMatchedOrError(ftc, ftc.IsChar, "character device")
	case v1.HostPathBlockDev:
		return typeMatchedOrError(ftc, ftc.IsBlock, "block device")
	default:
		return fmt.Errorf("%s is an invalid volume type", *pathType)
	}
}

// makeDir creates a new directory.
// If pathname already exists as a directory, no error is returned.
// If pathname already exists as a file, an error is returned.
func makeDir(pathname string) error {
	err := os.MkdirAll(pathname, os.FileMode(0755))
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// makeFile creates an empty file.
// If pathname already exists, whether a file or directory, no error is returned.
func makeFile(pathname string) error {
	f, err := os.OpenFile(pathname, os.O_CREATE, os.FileMode(0644))
	if f != nil {
		f.Close()
	}
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return nil
}
