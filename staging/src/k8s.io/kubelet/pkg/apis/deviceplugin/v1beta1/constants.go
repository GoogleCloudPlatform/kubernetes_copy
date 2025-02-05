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

package v1beta1

const (
	// Healthy means that the device is healthy
	Healthy = "Healthy"
	// Unhealthy means that the device is unhealthy
	Unhealthy = "Unhealthy"

	// Version means current version of the API supported by kubelet
	Version = "v1beta1"
	// DevicePluginPath is the folder the Device Plugin is expecting sockets to be on
	// Only privileged pods have access to this path
	// Note: Placeholder until we find a "standard path"
	DevicePluginPath = "/var/lib/kubelet/device-plugins/"
	// KubeletSocket is the path of the Kubelet registry socket
	KubeletSocket = DevicePluginPath + "kubelet.sock"

	// DevicePluginPathWindows Avoid failed to run Kubelet: bad socketPath,
	// must be an absolute path: /var/lib/kubelet/device-plugins/kubelet.sock
	// https://github.com/kubernetes/kubernetes/issues/93262
	// https://github.com/kubernetes/kubernetes/pull/93285#discussion_r458140701
	DevicePluginPathWindows = "\\var\\lib\\kubelet\\device-plugins\\"
	// KubeletSocketWindows is the path of the Kubelet registry socket on windows
	KubeletSocketWindows = DevicePluginPathWindows + "kubelet.sock"

	// KubeletGetPreferredAllocationRPCTimeoutInSecs is the timeout duration in secs for GetPreferredAllocation RPC
	// Timeout duration in secs for GetPreferredAllocation RPC
	KubeletGetPreferredAllocationRPCTimeoutInSecs = 30

	// KubeletAllocateRPCTimeoutInSecs is the timeout duration in secs for Allocate RPC
	// Timeout duration in secs for Allocate RPC
	KubeletAllocateRPCTimeoutInSecs = 30

	// KubeletPreStartContainerRPCTimeoutInSecs is the timeout duration in secs for PreStartContainer RPC
	// Timeout duration in secs for PreStartContainer RPC
	KubeletPreStartContainerRPCTimeoutInSecs = 30
)

// SupportedVersions provides a list of supported version
var SupportedVersions = [...]string{"v1beta1"}
