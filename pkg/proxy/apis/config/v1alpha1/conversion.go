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

package v1alpha1

import (
	"strings"

	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/kube-proxy/config/v1alpha1"
	"k8s.io/kubernetes/pkg/proxy/apis/config"
)

// Convert_config_KubeProxyConfiguration_To_v1alpha1_KubeProxyConfiguration is defined here, because public conversion is not auto-generated due to existing warnings.
func Convert_config_KubeProxyConfiguration_To_v1alpha1_KubeProxyConfiguration(in *config.KubeProxyConfiguration, out *v1alpha1.KubeProxyConfiguration, scope conversion.Scope) error {
	if err := autoConvert_config_KubeProxyConfiguration_To_v1alpha1_KubeProxyConfiguration(in, out, scope); err != nil {
		return err
	}
	out.WindowsRunAsService = in.Windows.RunAsService
	out.Conntrack = v1alpha1.KubeProxyConntrackConfiguration(in.Linux.Conntrack)
	out.OOMScoreAdj = in.Linux.OOMScoreAdj
	switch in.Mode {
	case config.ProxyModeNFTables:
		out.NFTables.MasqueradeAll = in.Linux.MasqueradeAll
	default:
		out.IPTables.MasqueradeAll = in.Linux.MasqueradeAll
	}

	switch in.Mode {
	case config.ProxyModeIPVS:
		out.IPVS.SyncPeriod = in.SyncPeriod
		out.IPVS.MinSyncPeriod = in.MinSyncPeriod
	case config.ProxyModeNFTables:
		out.NFTables.SyncPeriod = in.SyncPeriod
		out.NFTables.MinSyncPeriod = in.MinSyncPeriod
	default:
		out.IPTables.SyncPeriod = in.SyncPeriod
		out.IPTables.MinSyncPeriod = in.MinSyncPeriod
	}

	if len(in.DetectLocal.ClusterCIDRs) > 0 {
		out.ClusterCIDR = strings.Join(in.DetectLocal.ClusterCIDRs, ",")
	}

	if len(in.NodeIPOverride) > 0 {
		out.BindAddress = in.NodeIPOverride[0]
	}
	return nil
}

// Convert_v1alpha1_KubeProxyConfiguration_To_config_KubeProxyConfiguration is defined here, because public conversion is not auto-generated due to existing warnings.
func Convert_v1alpha1_KubeProxyConfiguration_To_config_KubeProxyConfiguration(in *v1alpha1.KubeProxyConfiguration, out *config.KubeProxyConfiguration, scope conversion.Scope) error {
	if err := autoConvert_v1alpha1_KubeProxyConfiguration_To_config_KubeProxyConfiguration(in, out, scope); err != nil {
		return err
	}
	out.Windows.RunAsService = in.WindowsRunAsService
	out.Linux.Conntrack = config.KubeProxyConntrackConfiguration(in.Conntrack)
	out.Linux.OOMScoreAdj = in.OOMScoreAdj
	switch config.ProxyMode(in.Mode) {
	case config.ProxyModeNFTables:
		out.Linux.MasqueradeAll = in.NFTables.MasqueradeAll
	default:
		out.Linux.MasqueradeAll = in.IPTables.MasqueradeAll
	}

	switch config.ProxyMode(in.Mode) {
	case config.ProxyModeIPVS:
		out.SyncPeriod = in.IPVS.SyncPeriod
		out.MinSyncPeriod = in.IPVS.MinSyncPeriod
	case config.ProxyModeNFTables:
		out.SyncPeriod = in.NFTables.SyncPeriod
		out.MinSyncPeriod = in.NFTables.MinSyncPeriod
	default:
		out.SyncPeriod = in.IPTables.SyncPeriod
		out.MinSyncPeriod = in.IPTables.MinSyncPeriod
	}

	if len(in.ClusterCIDR) > 0 {
		out.DetectLocal.ClusterCIDRs = strings.Split(in.ClusterCIDR, ",")
	}

	if len(in.BindAddress) > 0 {
		out.NodeIPOverride = []string{in.BindAddress}
	}

	// todo: @aroradaman figure out a better way to avoid the following
	out.IPFamilyPolicy = config.IPFamilyPolicyPreferDualStack
	return nil
}

// Convert_v1alpha1_KubeProxyIPTablesConfiguration_To_config_KubeProxyIPTablesConfiguration is defined here, because public conversion is not auto-generated due to existing warnings.
func Convert_v1alpha1_KubeProxyIPTablesConfiguration_To_config_KubeProxyIPTablesConfiguration(in *v1alpha1.KubeProxyIPTablesConfiguration, out *config.KubeProxyIPTablesConfiguration, scope conversion.Scope) error {
	return autoConvert_v1alpha1_KubeProxyIPTablesConfiguration_To_config_KubeProxyIPTablesConfiguration(in, out, scope)
}

// Convert_v1alpha1_KubeProxyIPVSConfiguration_To_config_KubeProxyIPVSConfiguration is defined here, because public conversion is not auto-generated due to existing warnings.
func Convert_v1alpha1_KubeProxyIPVSConfiguration_To_config_KubeProxyIPVSConfiguration(in *v1alpha1.KubeProxyIPVSConfiguration, out *config.KubeProxyIPVSConfiguration, scope conversion.Scope) error {
	return autoConvert_v1alpha1_KubeProxyIPVSConfiguration_To_config_KubeProxyIPVSConfiguration(in, out, scope)
}

// Convert_v1alpha1_KubeProxyNFTablesConfiguration_To_config_KubeProxyNFTablesConfiguration is defined here, because public conversion is not auto-generated due to existing warnings.
func Convert_v1alpha1_KubeProxyNFTablesConfiguration_To_config_KubeProxyNFTablesConfiguration(in *v1alpha1.KubeProxyNFTablesConfiguration, out *config.KubeProxyNFTablesConfiguration, scope conversion.Scope) error {
	return autoConvert_v1alpha1_KubeProxyNFTablesConfiguration_To_config_KubeProxyNFTablesConfiguration(in, out, scope)
}

// Convert_config_DetectLocalConfiguration_To_v1alpha1_DetectLocalConfiguration is defined here, because public conversion is not auto-generated due to existing warnings.
func Convert_config_DetectLocalConfiguration_To_v1alpha1_DetectLocalConfiguration(in *config.DetectLocalConfiguration, out *v1alpha1.DetectLocalConfiguration, s conversion.Scope) error {
	return autoConvert_config_DetectLocalConfiguration_To_v1alpha1_DetectLocalConfiguration(in, out, s)
}
