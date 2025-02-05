/*
Copyright 2015 The Kubernetes Authors.

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
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	kubeproxyconfigv1alpha1 "k8s.io/kube-proxy/config/v1alpha1"

	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/kubernetes/pkg/cluster/ports"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

func addDefaultingFuncs(scheme *kruntime.Scheme) error {
	return RegisterDefaults(scheme)
}

func SetDefaults_KubeProxyConfiguration(obj *kubeproxyconfigv1alpha1.KubeProxyConfiguration) {

	if len(obj.BindAddress) == 0 {
		obj.BindAddress = "0.0.0.0"
	}

	defaultHealthzAddress, defaultMetricsAddress := getDefaultAddresses(obj.BindAddress)

	if obj.HealthzBindAddress == "" {
		obj.HealthzBindAddress = fmt.Sprintf("%s:%v", defaultHealthzAddress, ports.ProxyHealthzPort)
	} else {
		obj.HealthzBindAddress = proxyutil.AppendPortIfNeeded(obj.HealthzBindAddress, ports.ProxyHealthzPort)
	}
	if obj.MetricsBindAddress == "" {
		obj.MetricsBindAddress = fmt.Sprintf("%s:%v", defaultMetricsAddress, ports.ProxyStatusPort)
	} else {
		obj.MetricsBindAddress = proxyutil.AppendPortIfNeeded(obj.MetricsBindAddress, ports.ProxyStatusPort)
	}

	if obj.OOMScoreAdj == nil {
		temp := int32(qos.KubeProxyOOMScoreAdj)
		obj.OOMScoreAdj = &temp
	}
	if obj.IPTables.SyncPeriod.Duration == 0 {
		obj.IPTables.SyncPeriod = metav1.Duration{Duration: 30 * time.Second}
	}
	if obj.IPTables.MinSyncPeriod.Duration == 0 {
		obj.IPTables.MinSyncPeriod = metav1.Duration{Duration: 1 * time.Second}
	}
	if obj.IPTables.LocalhostNodePorts == nil {
		obj.IPTables.LocalhostNodePorts = ptr.To(true)
	}
	if obj.IPVS.SyncPeriod.Duration == 0 {
		obj.IPVS.SyncPeriod = metav1.Duration{Duration: 30 * time.Second}
	}
	if obj.NFTables.SyncPeriod.Duration == 0 {
		obj.NFTables.SyncPeriod = metav1.Duration{Duration: 30 * time.Second}
	}
	if obj.NFTables.MinSyncPeriod.Duration == 0 {
		obj.NFTables.MinSyncPeriod = metav1.Duration{Duration: 1 * time.Second}
	}

	if obj.Conntrack.MaxPerCore == nil {
		obj.Conntrack.MaxPerCore = ptr.To[int32](32 * 1024)
	}
	if obj.Conntrack.Min == nil {
		obj.Conntrack.Min = ptr.To[int32](128 * 1024)
	}

	if obj.IPTables.MasqueradeBit == nil {
		temp := int32(14)
		obj.IPTables.MasqueradeBit = &temp
	}
	if obj.NFTables.MasqueradeBit == nil {
		temp := int32(14)
		obj.NFTables.MasqueradeBit = &temp
	}
	if obj.Conntrack.TCPEstablishedTimeout == nil {
		obj.Conntrack.TCPEstablishedTimeout = &metav1.Duration{Duration: 24 * time.Hour} // 1 day (1/5 default)
	}
	if obj.Conntrack.TCPCloseWaitTimeout == nil {
		// See https://github.com/kubernetes/kubernetes/issues/32551.
		//
		// CLOSE_WAIT conntrack state occurs when the Linux kernel
		// sees a FIN from the remote server. Note: this is a half-close
		// condition that persists as long as the local side keeps the
		// socket open. The condition is rare as it is typical in most
		// protocols for both sides to issue a close; this typically
		// occurs when the local socket is lazily garbage collected.
		//
		// If the CLOSE_WAIT conntrack entry expires, then FINs from the
		// local socket will not be properly SNAT'd and will not reach the
		// remote server (if the connection was subject to SNAT). If the
		// remote timeouts for FIN_WAIT* states exceed the CLOSE_WAIT
		// timeout, then there will be an inconsistency in the state of
		// the connection and a new connection reusing the SNAT (src,
		// port) pair may be rejected by the remote side with RST. This
		// can cause new calls to connect(2) to return with ECONNREFUSED.
		//
		// We set CLOSE_WAIT to one hour by default to better match
		// typical server timeouts.
		obj.Conntrack.TCPCloseWaitTimeout = &metav1.Duration{Duration: 1 * time.Hour}
	}
	if obj.ConfigSyncPeriod.Duration == 0 {
		obj.ConfigSyncPeriod.Duration = 15 * time.Minute
	}

	if len(obj.ClientConnection.ContentType) == 0 {
		obj.ClientConnection.ContentType = "application/vnd.kubernetes.protobuf"
	}
	if obj.ClientConnection.QPS == 0.0 {
		obj.ClientConnection.QPS = 5.0
	}
	if obj.ClientConnection.Burst == 0 {
		obj.ClientConnection.Burst = 10
	}
	if obj.FeatureGates == nil {
		obj.FeatureGates = make(map[string]bool)
	}
	if obj.IPVS.IPSet.HashSize == nil {
		obj.IPVS.IPSet.HashSize = ptr.To[int32](1024)
	}
	if obj.IPVS.IPSet.MaxElements == nil {
		obj.IPVS.IPSet.MaxElements = ptr.To[int32](65536)
	}
	// Use the Default LoggingConfiguration option
	logsapi.SetRecommendedLoggingConfiguration(&obj.Logging)
}

// getDefaultAddresses returns default address of healthz and metrics server
// based on the given bind address. IPv6 addresses are enclosed in square
// brackets for appending port.
func getDefaultAddresses(bindAddress string) (defaultHealthzAddress, defaultMetricsAddress string) {
	if netutils.ParseIPSloppy(bindAddress).To4() != nil {
		return "0.0.0.0", "127.0.0.1"
	}
	return "[::]", "[::1]"
}
