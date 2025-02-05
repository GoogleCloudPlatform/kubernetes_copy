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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	componentbaseconfigv1alpha1 "k8s.io/component-base/config/v1alpha1"
	logsapi "k8s.io/component-base/logs/api/v1"
)

// KubeProxyIPSetConfiguration contains ipset-related configuration
type KubeProxyIPSetConfiguration struct {
	// hashSize is the size of hash table for ipset
	// +optional
	HashSize *int32 `json:"hashSize,omitempty"`
	// maxElements is the maximal number of elements that can be stored in an ipset
	// +optional
	MaxElements *int32 `json:"maxElements,omitempty"`
}

// KubeProxyIPTablesConfiguration contains iptables-related configuration
// details for the Kubernetes proxy server.
type KubeProxyIPTablesConfiguration struct {
	// masqueradeBit is the bit of the iptables fwmark space to use for SNAT if using
	// the iptables or ipvs proxy mode. Values must be within the range [0, 31].
	MasqueradeBit *int32 `json:"masqueradeBit"`
	// masqueradeAll tells kube-proxy to SNAT all traffic sent to Service cluster IPs,
	// when using the iptables or ipvs proxy mode. This may be required with some CNI
	// plugins.
	MasqueradeAll bool `json:"masqueradeAll"`
	// localhostNodePorts, if false, tells kube-proxy to disable the legacy behavior
	// of allowing NodePort services to be accessed via localhost. (Applies only to
	// iptables mode and IPv4; localhost NodePorts are never allowed with other proxy
	// modes or with IPv6.)
	LocalhostNodePorts *bool `json:"localhostNodePorts"`
	// syncPeriod is an interval (e.g. '5s', '1m', '2h22m') indicating how frequently
	// various re-synchronizing and cleanup operations are performed. Must be greater
	// than 0.
	SyncPeriod metav1.Duration `json:"syncPeriod"`
	// minSyncPeriod is the minimum period between iptables rule resyncs (e.g. '5s',
	// '1m', '2h22m'). A value of 0 means every Service or EndpointSlice change will
	// result in an immediate iptables resync.
	MinSyncPeriod metav1.Duration `json:"minSyncPeriod"`
}

// KubeProxyIPVSConfiguration contains ipvs-related configuration
// details for the Kubernetes proxy server.
type KubeProxyIPVSConfiguration struct {
	// syncPeriod is an interval (e.g. '5s', '1m', '2h22m') indicating how frequently
	// various re-synchronizing and cleanup operations are performed. Must be greater
	// than 0.
	SyncPeriod metav1.Duration `json:"syncPeriod"`
	// minSyncPeriod is the minimum period between IPVS rule resyncs (e.g. '5s', '1m',
	// '2h22m'). A value of 0 means every Service or EndpointSlice change will result
	// in an immediate IPVS resync.
	MinSyncPeriod metav1.Duration `json:"minSyncPeriod"`
	// scheduler is the IPVS scheduler to use
	Scheduler string `json:"scheduler"`
	// excludeCIDRs is a list of CIDRs which the ipvs proxier should not touch
	// when cleaning up ipvs services.
	ExcludeCIDRs []string `json:"excludeCIDRs"`
	// strictARP configures arp_ignore and arp_announce to avoid answering ARP queries
	// from kube-ipvs0 interface
	StrictARP bool `json:"strictARP"`
	// tcpTimeout is the timeout value used for idle IPVS TCP sessions.
	// The default value is 0, which preserves the current timeout value on the system.
	TCPTimeout metav1.Duration `json:"tcpTimeout"`
	// tcpFinTimeout is the timeout value used for IPVS TCP sessions after receiving a FIN.
	// The default value is 0, which preserves the current timeout value on the system.
	TCPFinTimeout metav1.Duration `json:"tcpFinTimeout"`
	// udpTimeout is the timeout value used for IPVS UDP packets.
	// The default value is 0, which preserves the current timeout value on the system.
	UDPTimeout metav1.Duration `json:"udpTimeout"`
	// ipSet contains ipset settings for IPVS proxier
	IPSet KubeProxyIPSetConfiguration `json:"ipSet,omitempty"`
}

// KubeProxyNFTablesConfiguration contains nftables-related configuration
// details for the Kubernetes proxy server.
type KubeProxyNFTablesConfiguration struct {
	// masqueradeBit is the bit of the iptables fwmark space to use for SNAT if using
	// the nftables proxy mode. Values must be within the range [0, 31].
	MasqueradeBit *int32 `json:"masqueradeBit"`
	// masqueradeAll tells kube-proxy to SNAT all traffic sent to Service cluster IPs,
	// when using the nftables mode. This may be required with some CNI plugins.
	MasqueradeAll bool `json:"masqueradeAll"`
	// syncPeriod is an interval (e.g. '5s', '1m', '2h22m') indicating how frequently
	// various re-synchronizing and cleanup operations are performed. Must be greater
	// than 0.
	SyncPeriod metav1.Duration `json:"syncPeriod"`
	// minSyncPeriod is the minimum period between iptables rule resyncs (e.g. '5s',
	// '1m', '2h22m'). A value of 0 means every Service or EndpointSlice change will
	// result in an immediate iptables resync.
	MinSyncPeriod metav1.Duration `json:"minSyncPeriod"`
}

// KubeProxyConntrackConfiguration contains conntrack settings for
// the Kubernetes proxy server.
type KubeProxyConntrackConfiguration struct {
	// maxPerCore is the maximum number of NAT connections to track
	// per CPU core (0 to leave the limit as-is and ignore min).
	MaxPerCore *int32 `json:"maxPerCore"`
	// min is the minimum value of connect-tracking records to allocate,
	// regardless of maxPerCore (set maxPerCore=0 to leave the limit as-is).
	Min *int32 `json:"min"`
	// tcpEstablishedTimeout is how long an idle TCP connection will be kept open
	// (e.g. '2s').  Must be greater than 0 to set.
	TCPEstablishedTimeout *metav1.Duration `json:"tcpEstablishedTimeout"`
	// tcpCloseWaitTimeout is how long an idle conntrack entry
	// in CLOSE_WAIT state will remain in the conntrack
	// table. (e.g. '60s'). Must be greater than 0 to set.
	TCPCloseWaitTimeout *metav1.Duration `json:"tcpCloseWaitTimeout"`
	// tcpBeLiberal, if true, kube-proxy will configure conntrack
	// to run in liberal mode for TCP connections and packets with
	// out-of-window sequence numbers won't be marked INVALID.
	TCPBeLiberal bool `json:"tcpBeLiberal"`
	// udpTimeout is how long an idle UDP conntrack entry in
	// UNREPLIED state will remain in the conntrack table
	// (e.g. '30s'). Must be greater than 0 to set.
	UDPTimeout metav1.Duration `json:"udpTimeout"`
	// udpStreamTimeout is how long an idle UDP conntrack entry in
	// ASSURED state will remain in the conntrack table
	// (e.g. '300s'). Must be greater than 0 to set.
	UDPStreamTimeout metav1.Duration `json:"udpStreamTimeout"`
}

// KubeProxyWinkernelConfiguration contains Windows/HNS settings for
// the Kubernetes proxy server.
type KubeProxyWinkernelConfiguration struct {
	// networkName is the name of the network kube-proxy will use
	// to create endpoints and policies
	NetworkName string `json:"networkName"`
	// sourceVip is the IP address of the source VIP endpoint used for
	// NAT when loadbalancing
	SourceVip string `json:"sourceVip"`
	// enableDSR tells kube-proxy whether HNS policies should be created
	// with DSR
	EnableDSR bool `json:"enableDSR"`
	// rootHnsEndpointName is the name of hnsendpoint that is attached to
	// l2bridge for root network namespace
	RootHnsEndpointName string `json:"rootHnsEndpointName"`
	// forwardHealthCheckVip forwards service VIP for health check port on
	// Windows
	ForwardHealthCheckVip bool `json:"forwardHealthCheckVip"`
}

// DetectLocalConfiguration contains optional settings related to DetectLocalMode option
type DetectLocalConfiguration struct {
	// bridgeInterface is a bridge interface name. When DetectLocalMode is set to
	// LocalModeBridgeInterface, kube-proxy will consider traffic to be local if
	// it originates from this bridge.
	BridgeInterface string `json:"bridgeInterface"`
	// interfaceNamePrefix is an interface name prefix. When DetectLocalMode is set to
	// LocalModeInterfaceNamePrefix, kube-proxy will consider traffic to be local if
	// it originates from any interface whose name begins with this prefix.
	InterfaceNamePrefix string `json:"interfaceNamePrefix"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// KubeProxyConfiguration contains everything necessary to configure the
// Kubernetes proxy server.
type KubeProxyConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// featureGates is a map of feature names to bools that enable or disable alpha/experimental features.
	FeatureGates map[string]bool `json:"featureGates,omitempty"`

	// clientConnection specifies the kubeconfig file and client connection settings for the proxy
	// server to use when communicating with the apiserver.
	ClientConnection componentbaseconfigv1alpha1.ClientConnectionConfiguration `json:"clientConnection"`
	// logging specifies the options of logging.
	// Refer to [Logs Options](https://github.com/kubernetes/component-base/blob/master/logs/options.go)
	// for more information.
	Logging logsapi.LoggingConfiguration `json:"logging,omitempty"`

	// hostnameOverride, if non-empty, will be used as the name of the Node that
	// kube-proxy is running on. If unset, the node name is assumed to be the same as
	// the node's hostname.
	HostnameOverride string `json:"hostnameOverride"`
	// bindAddress can be used to override kube-proxy's idea of what its node's
	// primary IP is. Note that the name is a historical artifact, and kube-proxy does
	// not actually bind any sockets to this IP.
	BindAddress string `json:"bindAddress"`
	// healthzBindAddress is the IP address and port for the health check server to
	// serve on, defaulting to "0.0.0.0:10256" (if bindAddress is unset or IPv4), or
	// "[::]:10256" (if bindAddress is IPv6).
	HealthzBindAddress string `json:"healthzBindAddress"`
	// metricsBindAddress is the IP address and port for the metrics server to serve
	// on, defaulting to "127.0.0.1:10249" (if bindAddress is unset or IPv4), or
	// "[::1]:10249" (if bindAddress is IPv6). (Set to "0.0.0.0:10249" / "[::]:10249"
	// to bind on all interfaces.)
	MetricsBindAddress string `json:"metricsBindAddress"`
	// bindAddressHardFail, if true, tells kube-proxy to treat failure to bind to a
	// port as fatal and exit
	BindAddressHardFail bool `json:"bindAddressHardFail"`
	// enableProfiling enables profiling via web interface on /debug/pprof handler.
	// Profiling handlers will be handled by metrics server.
	EnableProfiling bool `json:"enableProfiling"`
	// showHiddenMetricsForVersion is the version for which you want to show hidden metrics.
	ShowHiddenMetricsForVersion string `json:"showHiddenMetricsForVersion"`

	// mode specifies which proxy mode to use.
	Mode ProxyMode `json:"mode"`
	// iptables contains iptables-related configuration options.
	IPTables KubeProxyIPTablesConfiguration `json:"iptables"`
	// ipvs contains ipvs-related configuration options.
	IPVS KubeProxyIPVSConfiguration `json:"ipvs"`
	// nftables contains nftables-related configuration options.
	NFTables KubeProxyNFTablesConfiguration `json:"nftables"`
	// winkernel contains winkernel-related configuration options.
	Winkernel KubeProxyWinkernelConfiguration `json:"winkernel"`

	// detectLocalMode determines mode to use for detecting local traffic, defaults to ClusterCIDR
	DetectLocalMode LocalMode `json:"detectLocalMode"`
	// detectLocal contains optional configuration settings related to DetectLocalMode.
	DetectLocal DetectLocalConfiguration `json:"detectLocal"`
	// clusterCIDR is the CIDR range of the pods in the cluster. (For dual-stack
	// clusters, this can be a comma-separated dual-stack pair of CIDR ranges.). When
	// DetectLocalMode is set to ClusterCIDR, kube-proxy will consider
	// traffic to be local if its source IP is in this range. (Otherwise it is not
	// used.)
	ClusterCIDR string `json:"clusterCIDR"`

	// nodePortAddresses is a list of CIDR ranges that contain valid node IPs, or
	// alternatively, the single string 'primary'. If set to a list of CIDRs,
	// connections to NodePort services will only be accepted on node IPs in one of
	// the indicated ranges. If set to 'primary', NodePort services will only be
	// accepted on the node's primary IPv4 and/or IPv6 address according to the Node
	// object. If unset, NodePort connections will be accepted on all local IPs.
	NodePortAddresses []string `json:"nodePortAddresses"`

	// oomScoreAdj is the oom-score-adj value for kube-proxy process. Values must be within
	// the range [-1000, 1000]
	OOMScoreAdj *int32 `json:"oomScoreAdj"`
	// conntrack contains conntrack-related configuration options.
	Conntrack KubeProxyConntrackConfiguration `json:"conntrack"`
	// configSyncPeriod is how often configuration from the apiserver is refreshed. Must be greater
	// than 0.
	ConfigSyncPeriod metav1.Duration `json:"configSyncPeriod"`

	// portRange was previously used to configure the userspace proxy, but is now unused.
	PortRange string `json:"portRange"`

	// windowsRunAsService, if true, enables Windows service control manager API integration.
	WindowsRunAsService bool `json:"windowsRunAsService,omitempty"`
}

// ProxyMode represents modes used by the Kubernetes proxy server.
//
// Three modes of proxy are available on Linux platforms: `iptables`, `ipvs`, and
// `nftables`. One mode of proxy is available on Windows platforms: `kernelspace`.
//
// If the proxy mode is unspecified, a default proxy mode will be used (currently this
// is `iptables` on Linux and `kernelspace` on Windows). If the selected proxy mode cannot be
// used (due to lack of kernel support, missing userspace components, etc) then kube-proxy
// will exit with an error.
type ProxyMode string

// LocalMode represents modes to detect local traffic from the node
type LocalMode string
