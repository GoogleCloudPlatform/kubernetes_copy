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

package util

import (
	v1 "k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// PodSandboxChanged checks whether the spec of the pod is changed and returns
// (changed, new attempt, original sandboxID if exist).
func PodSandboxChanged(pod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, uint32, string) {
	if len(podStatus.SandboxStatuses) == 0 {
		klog.V(2).InfoS("No sandbox for pod can be found. Need to start a new one", "pod", klog.KObj(pod))
		return true, 0, ""
	}

	readySandboxCount := 0
	maxAttempt := uint32(0)
	for _, s := range podStatus.SandboxStatuses {
		if s.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			readySandboxCount++
		}
		if s.Metadata.Attempt > maxAttempt {
			maxAttempt = s.Metadata.Attempt
		}
	}

	// Needs to create a new sandbox when readySandboxCount > 1 or the ready sandbox is not the latest one.
	sandboxStatus := podStatus.SandboxStatuses[0]
	if readySandboxCount > 1 {
		klog.V(2).InfoS("Multiple sandboxes are ready for Pod. Need to reconcile them", "pod", klog.KObj(pod))
		return true, maxAttempt + 1, sandboxStatus.Id
	}
	if sandboxStatus.State != runtimeapi.PodSandboxState_SANDBOX_READY {
		klog.V(2).InfoS("No ready sandbox for pod can be found. Need to start a new one", "pod", klog.KObj(pod))
		return true, maxAttempt + 1, sandboxStatus.Id
	}

	// Needs to create a new sandbox when network namespace changed.
	if sandboxStatus.GetLinux().GetNamespaces().GetOptions().GetNetwork() != NetworkNamespaceForPod(pod) {
		klog.V(2).InfoS("Sandbox for pod has changed. Need to start a new one", "pod", klog.KObj(pod))
		return true, maxAttempt + 1, ""
	}

	// Needs to create a new sandbox when the sandbox does not have an IP address.
	if !kubecontainer.IsHostNetworkPod(pod) && sandboxStatus.Network != nil && sandboxStatus.Network.Ip == "" {
		klog.V(2).InfoS("Sandbox for pod has no IP address. Need to start a new one", "pod", klog.KObj(pod))
		return true, maxAttempt + 1, sandboxStatus.Id
	}

	return false, maxAttempt, sandboxStatus.Id
}

// IpcNamespaceForPod returns the runtimeapi.NamespaceMode
// for the IPC namespace of a pod
func IpcNamespaceForPod(pod *v1.Pod) runtimeapi.NamespaceMode {
	if pod != nil && pod.Spec.HostIPC {
		return runtimeapi.NamespaceMode_NODE
	}
	return runtimeapi.NamespaceMode_POD
}

// NetworkNamespaceForPod returns the runtimeapi.NamespaceMode
// for the network namespace of a pod
func NetworkNamespaceForPod(pod *v1.Pod) runtimeapi.NamespaceMode {
	if pod != nil && pod.Spec.HostNetwork {
		return runtimeapi.NamespaceMode_NODE
	}
	return runtimeapi.NamespaceMode_POD
}

// PidNamespaceForPod returns the runtimeapi.NamespaceMode
// for the PID namespace of a pod
func PidNamespaceForPod(pod *v1.Pod) runtimeapi.NamespaceMode {
	if pod != nil {
		if pod.Spec.HostPID {
			return runtimeapi.NamespaceMode_NODE
		}
		if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
			return runtimeapi.NamespaceMode_POD
		}
	}
	// Note that PID does not default to the zero value for v1.Pod
	return runtimeapi.NamespaceMode_CONTAINER
}

// LookupRuntimeHandler is implemented by *runtimeclass.Manager.
type RuntimeHandlerResolver interface {
	LookupRuntimeHandler(runtimeClassName *string) (string, error)
}

// namespacesForPod returns the runtimeapi.NamespaceOption for a given pod.
// An empty or nil pod can be used to get the namespace defaults for v1.Pod.
func NamespacesForPod(pod *v1.Pod, runtimeHelper kubecontainer.RuntimeHelper, rcManager RuntimeHandlerResolver) (*runtimeapi.NamespaceOption, error) {
	runtimeHandler := ""
	if pod != nil && rcManager != nil {
		var err error
		runtimeHandler, err = rcManager.LookupRuntimeHandler(pod.Spec.RuntimeClassName)
		if err != nil {
			return nil, err
		}
	}
	userNs, err := runtimeHelper.GetOrCreateUserNamespaceMappings(pod, runtimeHandler)
	if err != nil {
		return nil, err
	}

	return &runtimeapi.NamespaceOption{
		Ipc:           IpcNamespaceForPod(pod),
		Network:       NetworkNamespaceForPod(pod),
		Pid:           PidNamespaceForPod(pod),
		UsernsOptions: userNs,
	}, nil
}
