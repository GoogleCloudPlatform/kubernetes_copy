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

package kuberuntime

import (
	"context"
	"fmt"
	"net/url"
	"runtime"
	"sort"

	v1 "k8s.io/api/core/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/types"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	runtimeutil "k8s.io/kubernetes/pkg/kubelet/kuberuntime/util"
	"k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	netutils "k8s.io/utils/net"
)

// createPodSandbox creates a pod sandbox and returns (podSandBoxID, message, error).
func (m *kubeGenericRuntimeManager) createPodSandbox(ctx context.Context, pod *v1.Pod, attempt uint32) (string, string, error) {
	podSandboxConfig, err := m.generatePodSandboxConfig(pod, attempt)
	if err != nil {
		message := fmt.Sprintf("Failed to generate sandbox config for pod %q: %v", format.Pod(pod), err)
		klog.ErrorS(err, "Failed to generate sandbox config for pod", "pod", klog.KObj(pod))
		return "", message, err
	}

	// Create pod logs directory
	err = m.osInterface.MkdirAll(podSandboxConfig.LogDirectory, 0755)
	if err != nil {
		message := fmt.Sprintf("Failed to create log directory for pod %q: %v", format.Pod(pod), err)
		klog.ErrorS(err, "Failed to create log directory for pod", "pod", klog.KObj(pod))
		return "", message, err
	}

	runtimeHandler := ""
	if m.runtimeClassManager != nil {
		runtimeHandler, err = m.runtimeClassManager.LookupRuntimeHandler(pod.Spec.RuntimeClassName)
		if err != nil {
			message := fmt.Sprintf("Failed to create sandbox for pod %q: %v", format.Pod(pod), err)
			return "", message, err
		}
		if runtimeHandler != "" {
			klog.V(2).InfoS("Running pod with runtime handler", "pod", klog.KObj(pod), "runtimeHandler", runtimeHandler)
		}
	}

	podSandBoxID, err := m.runtimeService.RunPodSandbox(ctx, podSandboxConfig, runtimeHandler)
	if err != nil {
		message := fmt.Sprintf("Failed to create sandbox for pod %q: %v", format.Pod(pod), err)
		klog.ErrorS(err, "Failed to create sandbox for pod", "pod", klog.KObj(pod))
		return "", message, err
	}

	return podSandBoxID, "", nil
}

// generatePodSandboxConfig generates pod sandbox config from v1.Pod.
func (m *kubeGenericRuntimeManager) generatePodSandboxConfig(pod *v1.Pod, attempt uint32) (*runtimeapi.PodSandboxConfig, error) {
	// TODO: deprecating podsandbox resource requirements in favor of the pod level cgroup
	// Refer https://github.com/kubernetes/kubernetes/issues/29871
	podUID := string(pod.UID)
	podSandboxConfig := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Uid:       podUID,
			Attempt:   attempt,
		},
		Labels:      newPodLabels(pod),
		Annotations: newPodAnnotations(pod),
	}

	dnsConfig, err := m.runtimeHelper.GetPodDNS(pod)
	if err != nil {
		return nil, err
	}
	podSandboxConfig.DnsConfig = dnsConfig

	if !kubecontainer.IsHostNetworkPod(pod) {
		// TODO: Add domain support in new runtime interface
		podHostname, podDomain, err := m.runtimeHelper.GeneratePodHostNameAndDomain(pod)
		if err != nil {
			return nil, err
		}
		podHostname, err = util.GetNodenameForKernel(podHostname, podDomain, pod.Spec.SetHostnameAsFQDN)
		if err != nil {
			return nil, err
		}
		podSandboxConfig.Hostname = podHostname
	}

	logDir := BuildPodLogsDirectory(m.podLogsDirectory, pod.Namespace, pod.Name, pod.UID)
	podSandboxConfig.LogDirectory = logDir

	portMappings := []*runtimeapi.PortMapping{}
	for _, c := range pod.Spec.Containers {
		containerPortMappings := kubecontainer.MakePortMappings(&c)

		for idx := range containerPortMappings {
			port := containerPortMappings[idx]
			hostPort := int32(port.HostPort)
			containerPort := int32(port.ContainerPort)
			protocol := toRuntimeProtocol(port.Protocol)
			portMappings = append(portMappings, &runtimeapi.PortMapping{
				HostIp:        port.HostIP,
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      protocol,
			})
		}

	}
	if len(portMappings) > 0 {
		podSandboxConfig.PortMappings = portMappings
	}

	lc, err := m.generatePodSandboxLinuxConfig(pod)
	if err != nil {
		return nil, err
	}
	podSandboxConfig.Linux = lc

	if runtime.GOOS == "windows" {
		wc, err := m.generatePodSandboxWindowsConfig(pod)
		if err != nil {
			return nil, err
		}
		podSandboxConfig.Windows = wc
	}

	// Update config to include overhead, sandbox level resources
	if err := m.applySandboxResources(pod, podSandboxConfig); err != nil {
		return nil, err
	}
	return podSandboxConfig, nil
}

// generatePodSandboxLinuxConfig generates LinuxPodSandboxConfig from v1.Pod.
// We've to call PodSandboxLinuxConfig always irrespective of the underlying OS as securityContext is not part of
// podSandboxConfig. It is currently part of LinuxPodSandboxConfig. In future, if we have securityContext pulled out
// in podSandboxConfig we should be able to use it.
func (m *kubeGenericRuntimeManager) generatePodSandboxLinuxConfig(pod *v1.Pod) (*runtimeapi.LinuxPodSandboxConfig, error) {
	cgroupParent := m.runtimeHelper.GetPodCgroupParent(pod)
	lc := &runtimeapi.LinuxPodSandboxConfig{
		CgroupParent: cgroupParent,
		SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{
			Privileged: kubecontainer.HasPrivilegedContainer(pod),

			// Forcing sandbox to run as `runtime/default` allow users to
			// use least privileged seccomp profiles at pod level. Issue #84623
			Seccomp: &runtimeapi.SecurityProfile{
				ProfileType: runtimeapi.SecurityProfile_RuntimeDefault,
			},
		},
	}

	sysctls := make(map[string]string)
	if pod.Spec.SecurityContext != nil {
		for _, c := range pod.Spec.SecurityContext.Sysctls {
			sysctls[c.Name] = c.Value
		}
	}

	lc.Sysctls = sysctls

	if pod.Spec.SecurityContext != nil {
		sc := pod.Spec.SecurityContext
		if sc.RunAsUser != nil && runtime.GOOS != "windows" {
			lc.SecurityContext.RunAsUser = &runtimeapi.Int64Value{Value: int64(*sc.RunAsUser)}
		}
		if sc.RunAsGroup != nil && runtime.GOOS != "windows" {
			lc.SecurityContext.RunAsGroup = &runtimeapi.Int64Value{Value: int64(*sc.RunAsGroup)}
		}
		namespaceOptions, err := runtimeutil.NamespacesForPod(pod, m.runtimeHelper, m.runtimeClassManager)
		if err != nil {
			return nil, err
		}
		lc.SecurityContext.NamespaceOptions = namespaceOptions

		if sc.FSGroup != nil && runtime.GOOS != "windows" {
			lc.SecurityContext.SupplementalGroups = append(lc.SecurityContext.SupplementalGroups, int64(*sc.FSGroup))
		}
		if groups := m.runtimeHelper.GetExtraSupplementalGroupsForPod(pod); len(groups) > 0 {
			lc.SecurityContext.SupplementalGroups = append(lc.SecurityContext.SupplementalGroups, groups...)
		}
		if sc.SupplementalGroups != nil {
			for _, sg := range sc.SupplementalGroups {
				lc.SecurityContext.SupplementalGroups = append(lc.SecurityContext.SupplementalGroups, int64(sg))
			}
		}
		if sc.SupplementalGroupsPolicy != nil {
			policyValue, ok := runtimeapi.SupplementalGroupsPolicy_value[string(*sc.SupplementalGroupsPolicy)]
			if !ok {
				return nil, fmt.Errorf("unsupported supplementalGroupsPolicy: %s", string(*sc.SupplementalGroupsPolicy))
			}
			lc.SecurityContext.SupplementalGroupsPolicy = runtimeapi.SupplementalGroupsPolicy(policyValue)
		}

		if sc.SELinuxOptions != nil && runtime.GOOS != "windows" {
			lc.SecurityContext.SelinuxOptions = &runtimeapi.SELinuxOption{
				User:  sc.SELinuxOptions.User,
				Role:  sc.SELinuxOptions.Role,
				Type:  sc.SELinuxOptions.Type,
				Level: sc.SELinuxOptions.Level,
			}
		}
	}

	return lc, nil
}

// generatePodSandboxWindowsConfig generates WindowsPodSandboxConfig from v1.Pod.
// On Windows this will get called in addition to LinuxPodSandboxConfig because not all relevant fields have been added to
// WindowsPodSandboxConfig at this time.
func (m *kubeGenericRuntimeManager) generatePodSandboxWindowsConfig(pod *v1.Pod) (*runtimeapi.WindowsPodSandboxConfig, error) {
	wc := &runtimeapi.WindowsPodSandboxConfig{
		SecurityContext: &runtimeapi.WindowsSandboxSecurityContext{},
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.WindowsHostNetwork) {
		wc.SecurityContext.NamespaceOptions = &runtimeapi.WindowsNamespaceOption{}
		if kubecontainer.IsHostNetworkPod(pod) {
			wc.SecurityContext.NamespaceOptions.Network = runtimeapi.NamespaceMode_NODE
		} else {
			wc.SecurityContext.NamespaceOptions.Network = runtimeapi.NamespaceMode_POD
		}
	}

	// If all of the containers in a pod are HostProcess containers, set the pod's HostProcess field
	// explicitly because the container runtime requires this information at sandbox creation time.
	if kubecontainer.HasWindowsHostProcessContainer(pod) {
		// At present Windows all containers in a Windows pod must be HostProcess containers
		// and HostNetwork is required to be set.
		if !kubecontainer.AllContainersAreWindowsHostProcess(pod) {
			return nil, fmt.Errorf("pod must not contain both HostProcess and non-HostProcess containers")
		}

		if !kubecontainer.IsHostNetworkPod(pod) {
			return nil, fmt.Errorf("hostNetwork is required if Pod contains HostProcess containers")
		}

		wc.SecurityContext.HostProcess = true
	}

	sc := pod.Spec.SecurityContext
	if sc == nil || sc.WindowsOptions == nil {
		return wc, nil
	}

	wo := sc.WindowsOptions
	if wo.GMSACredentialSpec != nil {
		wc.SecurityContext.CredentialSpec = *wo.GMSACredentialSpec
	}

	if wo.RunAsUserName != nil {
		wc.SecurityContext.RunAsUsername = *wo.RunAsUserName
	}

	if kubecontainer.HasWindowsHostProcessContainer(pod) {

		if wo.HostProcess != nil && !*wo.HostProcess {
			return nil, fmt.Errorf("pod must not contain any HostProcess containers if Pod's WindowsOptions.HostProcess is set to false")
		}
	}

	return wc, nil
}

// getKubeletSandboxes lists all (or just the running) sandboxes managed by kubelet.
func (m *kubeGenericRuntimeManager) getKubeletSandboxes(ctx context.Context, all bool) ([]*runtimeapi.PodSandbox, error) {
	var filter *runtimeapi.PodSandboxFilter
	if !all {
		readyState := runtimeapi.PodSandboxState_SANDBOX_READY
		filter = &runtimeapi.PodSandboxFilter{
			State: &runtimeapi.PodSandboxStateValue{
				State: readyState,
			},
		}
	}

	resp, err := m.runtimeService.ListPodSandbox(ctx, filter)
	if err != nil {
		klog.ErrorS(err, "Failed to list pod sandboxes")
		return nil, err
	}

	return resp, nil
}

// determinePodSandboxIP determines the IP addresses of the given pod sandbox.
func (m *kubeGenericRuntimeManager) determinePodSandboxIPs(podNamespace, podName string, podSandbox *runtimeapi.PodSandboxStatus) []string {
	podIPs := make([]string, 0)
	if podSandbox.Network == nil {
		klog.InfoS("Pod Sandbox status doesn't have network information, cannot report IPs", "pod", klog.KRef(podNamespace, podName))
		return podIPs
	}

	// ip could be an empty string if runtime is not responsible for the
	// IP (e.g., host networking).

	// pick primary IP
	if len(podSandbox.Network.Ip) != 0 {
		if netutils.ParseIPSloppy(podSandbox.Network.Ip) == nil {
			klog.InfoS("Pod Sandbox reported an unparsable primary IP", "pod", klog.KRef(podNamespace, podName), "IP", podSandbox.Network.Ip)
			return nil
		}
		podIPs = append(podIPs, podSandbox.Network.Ip)
	}

	// pick additional ips, if cri reported them
	for _, podIP := range podSandbox.Network.AdditionalIps {
		if nil == netutils.ParseIPSloppy(podIP.Ip) {
			klog.InfoS("Pod Sandbox reported an unparsable additional IP", "pod", klog.KRef(podNamespace, podName), "IP", podIP.Ip)
			return nil
		}
		podIPs = append(podIPs, podIP.Ip)
	}

	return podIPs
}

// getPodSandboxID gets the sandbox id by podUID and returns ([]sandboxID, error).
// Param state could be nil in order to get all sandboxes belonging to same pod.
func (m *kubeGenericRuntimeManager) getSandboxIDByPodUID(ctx context.Context, podUID kubetypes.UID, state *runtimeapi.PodSandboxState) ([]string, error) {
	filter := &runtimeapi.PodSandboxFilter{
		LabelSelector: map[string]string{types.KubernetesPodUIDLabel: string(podUID)},
	}
	if state != nil {
		filter.State = &runtimeapi.PodSandboxStateValue{
			State: *state,
		}
	}
	sandboxes, err := m.runtimeService.ListPodSandbox(ctx, filter)
	if err != nil {
		klog.ErrorS(err, "Failed to list sandboxes for pod", "podUID", podUID)
		return nil, err
	}

	if len(sandboxes) == 0 {
		return nil, nil
	}

	// Sort with newest first.
	sandboxIDs := make([]string, len(sandboxes))
	sort.Sort(podSandboxByCreated(sandboxes))
	for i, s := range sandboxes {
		sandboxIDs[i] = s.Id
	}

	return sandboxIDs, nil
}

// GetPortForward gets the endpoint the runtime will serve the port-forward request from.
func (m *kubeGenericRuntimeManager) GetPortForward(ctx context.Context, podName, podNamespace string, podUID kubetypes.UID, ports []int32) (*url.URL, error) {
	sandboxIDs, err := m.getSandboxIDByPodUID(ctx, podUID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to find sandboxID for pod %s: %v", format.PodDesc(podName, podNamespace, podUID), err)
	}
	if len(sandboxIDs) == 0 {
		return nil, fmt.Errorf("failed to find sandboxID for pod %s", format.PodDesc(podName, podNamespace, podUID))
	}
	req := &runtimeapi.PortForwardRequest{
		PodSandboxId: sandboxIDs[0],
		Port:         ports,
	}
	resp, err := m.runtimeService.PortForward(ctx, req)
	if err != nil {
		return nil, err
	}
	return url.Parse(resp.Url)
}
