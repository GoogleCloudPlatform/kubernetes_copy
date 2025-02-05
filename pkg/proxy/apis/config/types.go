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

package config

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	componentbaseconfig "k8s.io/component-base/config"
	logsapi "k8s.io/component-base/logs/api/v1"
)

// KubeProxyLinuxConfiguration contains Linux platform related configuration details for the
// Kubernetes proxy server that aren't specific to a particular backend.
type KubeProxyLinuxConfiguration struct {
	// conntrack contains conntrack-related configuration options.
	Conntrack KubeProxyConntrackConfiguration
	// masqueradeAll tells kube-proxy to SNAT all traffic sent to Service cluster IPs. This may
	// be required with some CNI plugins.
	MasqueradeAll bool
	// oomScoreAdj is the oom-score-adj value for kube-proxy process. Values must be within
	// the range [-1000, 1000]
	OOMScoreAdj *int32
}

// KubeProxyWindowsConfiguration contains Windows platform related configuration details for the
// Kubernetes proxy server that aren't specific to a particular backend
type KubeProxyWindowsConfiguration struct {
	// runAsService, if true, enables Windows service control manager API integration.
	RunAsService bool
}

// KubeProxyIPTablesConfiguration contains iptables-related configuration
// details for the Kubernetes proxy server.
type KubeProxyIPTablesConfiguration struct {
	// masqueradeBit is the bit of the iptables fwmark space to use for SNAT if using
	// the iptables or ipvs proxy mode. Values must be within the range [0, 31].
	MasqueradeBit *int32
	// localhostNodePorts, if false, tells kube-proxy to disable the legacy behavior
	// of allowing NodePort services to be accessed via localhost. (Applies only to
	// iptables mode and IPv4; localhost NodePorts are never allowed with other proxy
	// modes or with IPv6.)
	LocalhostNodePorts *bool
}

// KubeProxyIPVSConfiguration contains ipvs-related configuration
// details for the Kubernetes proxy server.
type KubeProxyIPVSConfiguration struct {
	// scheduler is the IPVS scheduler to use
	Scheduler string
	// excludeCIDRs is a list of CIDRs which the ipvs proxier should not touch
	// when cleaning up ipvs services.
	ExcludeCIDRs []string
	// strictARP configures arp_ignore and arp_announce to avoid answering ARP queries
	// from kube-ipvs0 interface
	StrictARP bool
	// tcpTimeout is the timeout value used for idle IPVS TCP sessions.
	// The default value is 0, which preserves the current timeout value on the system.
	TCPTimeout metav1.Duration
	// tcpFinTimeout is the timeout value used for IPVS TCP sessions after receiving a FIN.
	// The default value is 0, which preserves the current timeout value on the system.
	TCPFinTimeout metav1.Duration
	// udpTimeout is the timeout value used for IPVS UDP packets.
	// The default value is 0, which preserves the current timeout value on the system.
	UDPTimeout metav1.Duration
}

// KubeProxyNFTablesConfiguration contains nftables-related configuration
// details for the Kubernetes proxy server.
type KubeProxyNFTablesConfiguration struct {
	// masqueradeBit is the bit of the iptables fwmark space to use for SNAT if using
	// the nftables proxy mode. Values must be within the range [0, 31].
	MasqueradeBit *int32
}

// KubeProxyConntrackConfiguration contains conntrack settings for
// the Kubernetes proxy server.
type KubeProxyConntrackConfiguration struct {
	// maxPerCore is the maximum number of NAT connections to track
	// per CPU core (0 to leave the limit as-is and ignore min).
	MaxPerCore *int32
	// min is the minimum value of connect-tracking records to allocate,
	// regardless of maxPerCore (set maxPerCore=0 to leave the limit as-is).
	Min *int32
	// tcpEstablishedTimeout is how long an idle TCP connection will be kept open
	// (e.g. '2s').  Must be greater than 0 to set.
	TCPEstablishedTimeout *metav1.Duration
	// tcpCloseWaitTimeout is how long an idle conntrack entry
	// in CLOSE_WAIT state will remain in the conntrack
	// table. (e.g. '60s'). Must be greater than 0 to set.
	TCPCloseWaitTimeout *metav1.Duration
	// tcpBeLiberal, if true, kube-proxy will configure conntrack
	// to run in liberal mode for TCP connections and packets with
	// out-of-window sequence numbers won't be marked INVALID.
	TCPBeLiberal bool
	// udpTimeout is how long an idle UDP conntrack entry in
	// UNREPLIED state will remain in the conntrack table
	// (e.g. '30s'). Must be greater than 0 to set.
	UDPTimeout metav1.Duration
	// udpStreamTimeout is how long an idle UDP conntrack entry in
	// ASSURED state will remain in the conntrack table
	// (e.g. '300s'). Must be greater than 0 to set.
	UDPStreamTimeout metav1.Duration
}

// KubeProxyWinkernelConfiguration contains Windows/HNS settings for
// the Kubernetes proxy server.
type KubeProxyWinkernelConfiguration struct {
	// networkName is the name of the network kube-proxy will use
	// to create endpoints and policies
	NetworkName string
	// sourceVip is the IP address of the source VIP endpoint used for
	// NAT when loadbalancing
	SourceVip string
	// enableDSR tells kube-proxy whether HNS policies should be created
	// with DSR
	EnableDSR bool
	// rootHnsEndpointName is the name of hnsendpoint that is attached to
	// l2bridge for root network namespace
	RootHnsEndpointName string
	// forwardHealthCheckVip forwards service VIP for health check port on
	// Windows
	ForwardHealthCheckVip bool
}

// DetectLocalConfiguration contains optional settings related to DetectLocalMode option
type DetectLocalConfiguration struct {
	// bridgeInterface is a bridge interface name. When DetectLocalMode is set to
	// LocalModeBridgeInterface, kube-proxy will consider traffic to be local if
	// it originates from this bridge.
	BridgeInterface string
	// clusterCIDRs is the dual-stack list of CIDR ranges of the pods in the cluster. When
	// DetectLocalMode is set to LocalModeClusterCIDR, kube-proxy will consider
	// traffic to be local if its source IP is in the range of any given CIDR.
	ClusterCIDRs []string
	// interfaceNamePrefix is an interface name prefix. When DetectLocalMode is set to
	// LocalModeInterfaceNamePrefix, kube-proxy will consider traffic to be local if
	// it originates from any interface whose name begins with this prefix.
	InterfaceNamePrefix string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// KubeProxyConfiguration contains everything necessary to configure the
// Kubernetes proxy server.
type KubeProxyConfiguration struct {
	metav1.TypeMeta

	// linux contains Linux-related configuration options.
	Linux KubeProxyLinuxConfiguration

	// windows contains Windows-related configuration options.
	Windows KubeProxyWindowsConfiguration

	// featureGates is a map of feature names to bools that enable or disable alpha/experimental features.
	FeatureGates map[string]bool

	// clientConnection specifies the kubeconfig file and client connection settings for the proxy
	// server to use when communicating with the apiserver.
	ClientConnection componentbaseconfig.ClientConnectionConfiguration
	// logging specifies the options of logging.
	// Refer to [Logs Options](https://github.com/kubernetes/component-base/blob/master/logs/options.go)
	// for more information.
	Logging logsapi.LoggingConfiguration

	// hostnameOverride, if non-empty, will be used as the name of the Node that
	// kube-proxy is running on. If unset, the node name is assumed to be the same as
	// the node's hostname.
	HostnameOverride string
	// nodeIPOverride can be used to override kube-proxy's idea of what its node's
	// primary IPs are. nodeIPOverride[0] will be used for defaulting healthzBindAddress
	// and metricsBindAddress values. If nodePortAddresses is set to "primary" node-ports
	// will be exposed on the given ips.
	NodeIPOverride []string
	// ipFamilyPolicy controls the nodeIP detection logic, which further influences if LoadBalancer
	// services should be allowed on node when SourceRanges are set and on which interfaces node
	// ports should be exposed when nodePortAddresses is set to 'primary'.
	IPFamilyPolicy IPFamilyPolicy
	// healthzBindAddress is the IP address and port for the health check server to
	// serve on, defaulting to "0.0.0.0:10256" (if bindAddress is unset or IPv4), or
	// "[::]:10256" (if bindAddress is IPv6).
	HealthzBindAddress string
	// metricsBindAddress is the IP address and port for the metrics server to serve
	// on, defaulting to "127.0.0.1:10249" (if bindAddress is unset or IPv4), or
	// "[::1]:10249" (if bindAddress is IPv6). (Set to "0.0.0.0:10249" / "[::]:10249"
	// to bind on all interfaces.)
	MetricsBindAddress string
	// bindAddressHardFail, if true, tells kube-proxy to treat failure to bind to a
	// port as fatal and exit
	BindAddressHardFail bool
	// enableProfiling enables profiling via web interface on /debug/pprof handler.
	// Profiling handlers will be handled by metrics server.
	EnableProfiling bool
	// showHiddenMetricsForVersion is the version for which you want to show hidden metrics.
	ShowHiddenMetricsForVersion string

	// mode specifies which proxy mode to use.
	Mode ProxyMode
	// iptables contains iptables-related configuration options.
	IPTables KubeProxyIPTablesConfiguration
	// ipvs contains ipvs-related configuration options.
	IPVS KubeProxyIPVSConfiguration
	// winkernel contains winkernel-related configuration options.
	Winkernel KubeProxyWinkernelConfiguration
	// nftables contains nftables-related configuration options.
	NFTables KubeProxyNFTablesConfiguration

	// detectLocalMode determines mode to use for detecting local traffic, defaults to LocalModeClusterCIDR
	DetectLocalMode LocalMode
	// detectLocal contains optional configuration settings related to DetectLocalMode.
	DetectLocal DetectLocalConfiguration

	// nodePortAddresses is a list of CIDR ranges that contain valid node IPs, or
	// alternatively, the single string 'primary'. If set to a list of CIDRs,
	// connections to NodePort services will only be accepted on node IPs in one of
	// the indicated ranges. If set to 'primary', NodePort services will only be
	// accepted on the node's primary IPv4 and/or IPv6 address according to the Node
	// object. If unset, NodePort connections will be accepted on all local IPs.
	NodePortAddresses []string

	// syncPeriod is an interval (e.g. '5s', '1m', '2h22m') indicating how frequently
	// various re-synchronizing and cleanup operations are performed. Must be greater
	// than 0.
	SyncPeriod metav1.Duration
	// minSyncPeriod is the minimum period between proxier rule resyncs (e.g. '5s',
	// '1m', '2h22m'). A value of 0 means every Service or EndpointSlice change will
	// result in an immediate proxier resync.
	MinSyncPeriod metav1.Duration
	// configSyncPeriod is how often configuration from the apiserver is refreshed. Must be greater
	// than 0.
	ConfigSyncPeriod metav1.Duration
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

const (
	ProxyModeIPTables    ProxyMode = "iptables"
	ProxyModeIPVS        ProxyMode = "ipvs"
	ProxyModeNFTables    ProxyMode = "nftables"
	ProxyModeKernelspace ProxyMode = "kernelspace"
)

func (m *ProxyMode) Set(s string) error {
	*m = ProxyMode(s)
	return nil
}

func (m *ProxyMode) String() string {
	if m != nil {
		return string(*m)
	}
	return ""
}

func (m *ProxyMode) Type() string {
	return "ProxyMode"
}

// LocalMode represents modes to detect local traffic from the node
type LocalMode string

// Currently supported modes for LocalMode
const (
	LocalModeClusterCIDR         LocalMode = "ClusterCIDR"
	LocalModeNodeCIDR            LocalMode = "NodeCIDR"
	LocalModeBridgeInterface     LocalMode = "BridgeInterface"
	LocalModeInterfaceNamePrefix LocalMode = "InterfaceNamePrefix"
)

func (m *LocalMode) Set(s string) error {
	*m = LocalMode(s)
	return nil
}

func (m *LocalMode) String() string {
	if m != nil {
		return string(*m)
	}
	return ""
}

func (m *LocalMode) Type() string {
	return "LocalMode"
}

// NodePortAddressesPrimary is a special value for NodePortAddresses indicating that it
// should only use the primary node IPs.
const NodePortAddressesPrimary string = "primary"

// IPFamilyPolicy controls the nodeIP detection logic, which further influences if LoadBalancer
// services should be allowed on node when SourceRanges are set and on which interfaces node
// ports should be exposed when nodePortAddresses is set to 'primary'.
type IPFamilyPolicy string

const (
	// IPFamilyPolicySingleStack indicates that kube-proxy is required to work with NodeIP
	// of single IP family. The NodeIP assigned is based on NodeIPOverride.
	IPFamilyPolicySingleStack IPFamilyPolicy = "SingleStack"

	// IPFamilyPolicyPreferDualStack indicates that kube-proxy is preferred to work with NodeIPs
	// of both the IP families. The NodeIPs will be auto-detected if NodeIPOverride is empty
	// or missing value for a particular IPFamily.
	IPFamilyPolicyPreferDualStack IPFamilyPolicy = "PreferDualStack"

	// IPFamilyPolicyRequireDualStack indicates that kube-proxy is preferred to work with NodeIPs
	// of both the IP families.
	IPFamilyPolicyRequireDualStack IPFamilyPolicy = "RequireDualStack"
)
