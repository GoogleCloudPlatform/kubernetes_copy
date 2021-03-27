/*
Copyright 2024 The Kubernetes Authors.

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

package selinuxwarning

import (
	"fmt"
	"net"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

var _ volume.VolumeHost = &Controller{}
var _ volume.CSIDriverVolumeHost = &Controller{}

// VolumeHost implementation. It requires a lot of kubelet specific methods that are not used in the controller.
func (c *Controller) GetPluginDir(podUID string) string {
	return ""
}

func (c *Controller) GetVolumeDevicePluginDir(podUID string) string {
	return ""
}

func (c *Controller) GetPodsDir() string {
	return ""
}

func (c *Controller) GetPodVolumeDir(podUID types.UID, pluginName, volumeName string) string {
	return ""
}

func (c *Controller) GetPodPluginDir(podUID types.UID, pluginName string) string {
	return ""
}

func (c *Controller) GetPodVolumeDeviceDir(podUID types.UID, pluginName string) string {
	return ""
}

func (c *Controller) GetKubeClient() clientset.Interface {
	return c.kubeClient
}

func (c *Controller) NewWrapperMounter(volName string, spec volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	return nil, fmt.Errorf("NewWrapperMounter not supported by SELinux controller VolumeHost implementation")
}

func (c *Controller) NewWrapperUnmounter(volName string, spec volume.Spec, podUID types.UID) (volume.Unmounter, error) {
	return nil, fmt.Errorf("NewWrapperUnmounter not supported by SELinux controller VolumeHost implementation")
}

func (c *Controller) GetMounter(pluginName string) mount.Interface {
	return nil
}

func (c *Controller) GetHostName() string {
	return ""
}

func (c *Controller) GetHostIP() (net.IP, error) {
	return nil, fmt.Errorf("GetHostIP() not supported by SELinux controller VolumeHost implementation")
}

func (c *Controller) GetNodeAllocatable() (v1.ResourceList, error) {
	return v1.ResourceList{}, nil
}

func (c *Controller) GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error) {
	return map[v1.UniqueVolumeName]string{}, nil
}

func (c *Controller) GetSecretFunc() func(namespace, name string) (*v1.Secret, error) {
	return func(_, _ string) (*v1.Secret, error) {
		return nil, fmt.Errorf("GetSecret unsupported in SELinux controller")
	}
}

func (c *Controller) GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error) {
	return func(_, _ string) (*v1.ConfigMap, error) {
		return nil, fmt.Errorf("GetConfigMap unsupported in SELinux controller")
	}
}

func (c *Controller) GetServiceAccountTokenFunc() func(_, _ string, _ *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	return func(_, _ string, _ *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		return nil, fmt.Errorf("GetServiceAccountToken unsupported in SELinux controller")
	}
}

func (c *Controller) DeleteServiceAccountTokenFunc() func(types.UID) {
	return func(types.UID) {
		// nolint:logcheck
		klog.ErrorS(nil, "DeleteServiceAccountToken unsupported in SELinux controller")
	}
}

func (c *Controller) GetExec(pluginName string) utilexec.Interface {
	return utilexec.New()
}

func (c *Controller) GetNodeLabels() (map[string]string, error) {
	return nil, fmt.Errorf("GetNodeLabels() unsupported in SELinux controller")
}

func (c *Controller) GetNodeName() types.NodeName {
	return ""
}

func (c *Controller) GetEventRecorder() events.EventRecorder {
	return nil
}

func (c *Controller) GetSubpather() subpath.Interface {
	return nil
}

func (c *Controller) CSIDriverLister() storagelisters.CSIDriverLister {
	return c.csiDriverLister
}
