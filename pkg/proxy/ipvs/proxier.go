//go:build linux
// +build linux

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

package ipvs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
	utilexec "k8s.io/utils/exec"
	netutils "k8s.io/utils/net"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/conntrack"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	utilipset "k8s.io/kubernetes/pkg/proxy/ipvs/ipset"
	utilipvs "k8s.io/kubernetes/pkg/proxy/ipvs/util"
	"k8s.io/kubernetes/pkg/proxy/metaproxier"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/async"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilkernel "k8s.io/kubernetes/pkg/util/kernel"
)

const (
	// kubeServicesChain is the services portal chain
	kubeServicesChain utiliptables.Chain = "KUBE-SERVICES"

	// kubeProxyFirewallChain is the kube-proxy firewall chain.
	kubeProxyFirewallChain utiliptables.Chain = "KUBE-PROXY-FIREWALL"

	// kubeSourceRangesFirewallChain is the firewall subchain for LoadBalancerSourceRanges.
	kubeSourceRangesFirewallChain utiliptables.Chain = "KUBE-SOURCE-RANGES-FIREWALL"

	// kubePostroutingChain is the kubernetes postrouting chain
	kubePostroutingChain utiliptables.Chain = "KUBE-POSTROUTING"

	// kubeMarkMasqChain is the mark-for-masquerade chain
	kubeMarkMasqChain utiliptables.Chain = "KUBE-MARK-MASQ"

	// kubeNodePortChain is the kubernetes node port chain
	kubeNodePortChain utiliptables.Chain = "KUBE-NODE-PORT"

	// kubeForwardChain is the kubernetes forward chain
	kubeForwardChain utiliptables.Chain = "KUBE-FORWARD"

	// kubeLoadBalancerChain is the kubernetes chain for loadbalancer type service
	kubeLoadBalancerChain utiliptables.Chain = "KUBE-LOAD-BALANCER"

	// kubeIPVSFilterChain filters external access to main netns
	// https://github.com/kubernetes/kubernetes/issues/72236
	kubeIPVSFilterChain utiliptables.Chain = "KUBE-IPVS-FILTER"

	// kubeIPVSOutFilterChain filters access to load balancer services from node.
	// https://github.com/kubernetes/kubernetes/issues/119656
	kubeIPVSOutFilterChain utiliptables.Chain = "KUBE-IPVS-OUT-FILTER"

	// defaultScheduler is the default ipvs scheduler algorithm - round robin.
	defaultScheduler = "rr"

	// defaultDummyDevice is the default dummy interface which ipvs service address will bind to it.
	defaultDummyDevice = "kube-ipvs0"
)

// In IPVS proxy mode, the following flags need to be set
const (
	sysctlVSConnTrack             = "net/ipv4/vs/conntrack"
	sysctlConnReuse               = "net/ipv4/vs/conn_reuse_mode"
	sysctlExpireNoDestConn        = "net/ipv4/vs/expire_nodest_conn"
	sysctlExpireQuiescentTemplate = "net/ipv4/vs/expire_quiescent_template"
	sysctlForward                 = "net/ipv4/ip_forward"
	sysctlArpIgnore               = "net/ipv4/conf/all/arp_ignore"
	sysctlArpAnnounce             = "net/ipv4/conf/all/arp_announce"
)

// NewDualStackProxier returns a new Proxier for dual-stack operation
func NewDualStackProxier(
	ctx context.Context,
	ipt [2]utiliptables.Interface,
	ipvs utilipvs.Interface,
	ipset utilipset.Interface,
	sysctl utilsysctl.Interface,
	exec utilexec.Interface,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	excludeCIDRs []string,
	strictARP bool,
	tcpTimeout time.Duration,
	tcpFinTimeout time.Duration,
	udpTimeout time.Duration,
	masqueradeAll bool,
	masqueradeBit int,
	localDetectors map[v1.IPFamily]proxyutil.LocalTrafficDetector,
	hostname string,
	nodeIPs map[v1.IPFamily]net.IP,
	recorder events.EventRecorder,
	healthzServer *healthcheck.ProxyHealthServer,
	scheduler string,
	nodePortAddresses []string,
	initOnly bool,
) (proxy.Provider, error) {
	// Create an ipv4 instance of the single-stack proxier
	ipv4Proxier, err := NewProxier(ctx, v1.IPv4Protocol, ipt[0], ipvs, ipset, sysctl,
		exec, syncPeriod, minSyncPeriod, filterCIDRs(false, excludeCIDRs), strictARP,
		tcpTimeout, tcpFinTimeout, udpTimeout, masqueradeAll, masqueradeBit,
		localDetectors[v1.IPv4Protocol], hostname, nodeIPs[v1.IPv4Protocol], recorder,
		healthzServer, scheduler, nodePortAddresses, initOnly)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv4 proxier: %v", err)
	}

	ipv6Proxier, err := NewProxier(ctx, v1.IPv6Protocol, ipt[1], ipvs, ipset, sysctl,
		exec, syncPeriod, minSyncPeriod, filterCIDRs(true, excludeCIDRs), strictARP,
		tcpTimeout, tcpFinTimeout, udpTimeout, masqueradeAll, masqueradeBit,
		localDetectors[v1.IPv6Protocol], hostname, nodeIPs[v1.IPv6Protocol], recorder,
		healthzServer, scheduler, nodePortAddresses, initOnly)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv6 proxier: %v", err)
	}
	if initOnly {
		return nil, nil
	}

	// Return a meta-proxier that dispatch calls between the two
	// single-stack proxier instances
	return metaproxier.NewMetaProxier(ipv4Proxier, ipv6Proxier), nil
}

// Proxier is an ipvs based proxy for connections between a localhost:lport
// and services that provide the actual backends.
type Proxier struct {
	// the ipfamily on which this proxy is operating on.
	ipFamily v1.IPFamily
	// endpointsChanges and serviceChanges contains all changes to endpoints and
	// services that happened since last syncProxyRules call. For a single object,
	// changes are accumulated, i.e. previous is state from before all of them,
	// current is state after applying all of those.
	endpointsChanges *proxy.EndpointsChangeTracker
	serviceChanges   *proxy.ServiceChangeTracker

	mu           sync.Mutex // protects the following fields
	svcPortMap   proxy.ServicePortMap
	endpointsMap proxy.EndpointsMap
	nodeLabels   map[string]string
	// initialSync is a bool indicating if the proxier is syncing for the first time.
	// It is set to true when a new proxier is initialized and then set to false on all
	// future syncs.
	// This lets us run specific logic that's required only during proxy startup.
	// For eg: it enables us to update weights of existing destinations only on startup
	// saving us the cost of querying and updating real servers during every sync.
	initialSync bool
	// endpointSlicesSynced, and servicesSynced are set to true when
	// corresponding objects are synced after startup. This is used to avoid updating
	// ipvs rules with some partial data after kube-proxy restart.
	endpointSlicesSynced bool
	servicesSynced       bool
	initialized          int32
	syncRunner           *async.BoundedFrequencyRunner // governs calls to syncProxyRules

	// These are effectively const and do not need the mutex to be held.
	syncPeriod    time.Duration
	minSyncPeriod time.Duration
	// Values are CIDR's to exclude when cleaning up IPVS rules.
	excludeCIDRs []*net.IPNet
	// Set to true to set sysctls arp_ignore and arp_announce
	strictARP      bool
	iptables       utiliptables.Interface
	ipvs           utilipvs.Interface
	ipset          utilipset.Interface
	conntrack      conntrack.Interface
	masqueradeAll  bool
	masqueradeMark string
	localDetector  proxyutil.LocalTrafficDetector
	hostname       string
	nodeIP         net.IP
	recorder       events.EventRecorder

	serviceHealthServer healthcheck.ServiceHealthServer
	healthzServer       *healthcheck.ProxyHealthServer

	ipvsScheduler string
	// The following buffers are used to reuse memory and avoid allocations
	// that are significantly impacting performance.
	iptablesData     *bytes.Buffer
	filterChainsData *bytes.Buffer
	natChains        proxyutil.LineBuffer
	filterChains     proxyutil.LineBuffer
	natRules         proxyutil.LineBuffer
	filterRules      proxyutil.LineBuffer
	// Added as a member to the struct to allow injection for testing.
	netlinkHandle NetLinkHandle
	// ipsetList is the list of ipsets that ipvs proxier used.
	ipsetList map[string]*IPSet
	// nodePortAddresses selects the interfaces where nodePort works.
	nodePortAddresses *proxyutil.NodePortAddresses
	// networkInterfacer defines an interface for several net library functions.
	// Inject for test purpose.
	networkInterfacer     proxyutil.NetworkInterfacer
	gracefuldeleteManager *GracefulTerminationManager
	// serviceNoLocalEndpointsInternal represents the set of services that couldn't be applied
	// due to the absence of local endpoints when the internal traffic policy is "Local".
	// It is used to publish the sync_proxy_rules_no_endpoints_total
	// metric with the traffic_policy label set to "internal".
	// A Set is used here since we end up calculating endpoint topology multiple times for the same Service
	// if it has multiple ports but each Service should only be counted once.
	serviceNoLocalEndpointsInternal sets.Set[string]
	// serviceNoLocalEndpointsExternal represents the set of services that couldn't be applied
	// due to the absence of any endpoints when the external traffic policy is "Local".
	// It is used to publish the sync_proxy_rules_no_endpoints_total
	// metric with the traffic_policy label set to "external".
	// A Set is used here since we end up calculating endpoint topology multiple times for the same Service
	// if it has multiple ports but each Service should only be counted once.
	serviceNoLocalEndpointsExternal sets.Set[string]
	// lbNoNodeAccessIPPortProtocolEntries represents the set of loadBalancers IP + Port + Protocol that should not be accessible from K8s nodes
	// We cannot directly restrict LB access from node using LoadBalancerSourceRanges, we need to install
	// additional iptables rules.
	// (ref: https://github.com/kubernetes/kubernetes/issues/119656)
	lbNoNodeAccessIPPortProtocolEntries []*utilipset.Entry

	logger klog.Logger
}

// Proxier implements proxy.Provider
var _ proxy.Provider = &Proxier{}

// NewProxier returns a new Proxier given an iptables and ipvs Interface instance.
// Because of the iptables and ipvs logic, it is assumed that there is only a single Proxier active on a machine.
// An error will be returned if it fails to update or acquire the initial lock.
// Once a proxier is created, it will keep iptables and ipvs rules up to date in the background and
// will not terminate if a particular iptables or ipvs call fails.
func NewProxier(
	ctx context.Context,
	ipFamily v1.IPFamily,
	ipt utiliptables.Interface,
	ipvs utilipvs.Interface,
	ipset utilipset.Interface,
	sysctl utilsysctl.Interface,
	exec utilexec.Interface,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	excludeCIDRs []string,
	strictARP bool,
	tcpTimeout time.Duration,
	tcpFinTimeout time.Duration,
	udpTimeout time.Duration,
	masqueradeAll bool,
	masqueradeBit int,
	localDetector proxyutil.LocalTrafficDetector,
	hostname string,
	nodeIP net.IP,
	recorder events.EventRecorder,
	healthzServer *healthcheck.ProxyHealthServer,
	scheduler string,
	nodePortAddressStrings []string,
	initOnly bool,
) (*Proxier, error) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "ipFamily", ipFamily)
	// Set the conntrack sysctl we need for
	if err := proxyutil.EnsureSysctl(sysctl, sysctlVSConnTrack, 1); err != nil {
		return nil, err
	}

	kernelVersion, err := utilkernel.GetVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get kernel version: %w", err)
	}

	if kernelVersion.LessThan(version.MustParseGeneric(utilkernel.IPVSConnReuseModeMinSupportedKernelVersion)) {
		logger.Error(nil, "Can't set sysctl, kernel version doesn't satisfy minimum version requirements", "sysctl", sysctlConnReuse, "minimumKernelVersion", utilkernel.IPVSConnReuseModeMinSupportedKernelVersion)
	} else if kernelVersion.AtLeast(version.MustParseGeneric(utilkernel.IPVSConnReuseModeFixedKernelVersion)) {
		// https://github.com/kubernetes/kubernetes/issues/93297
		logger.V(2).Info("Left as-is", "sysctl", sysctlConnReuse)
	} else {
		// Set the connection reuse mode
		if err := proxyutil.EnsureSysctl(sysctl, sysctlConnReuse, 0); err != nil {
			return nil, err
		}
	}

	// Set the expire_nodest_conn sysctl we need for
	if err := proxyutil.EnsureSysctl(sysctl, sysctlExpireNoDestConn, 1); err != nil {
		return nil, err
	}

	// Set the expire_quiescent_template sysctl we need for
	if err := proxyutil.EnsureSysctl(sysctl, sysctlExpireQuiescentTemplate, 1); err != nil {
		return nil, err
	}

	// Set the ip_forward sysctl we need for
	if err := proxyutil.EnsureSysctl(sysctl, sysctlForward, 1); err != nil {
		return nil, err
	}

	if strictARP {
		// Set the arp_ignore sysctl we need for
		if err := proxyutil.EnsureSysctl(sysctl, sysctlArpIgnore, 1); err != nil {
			return nil, err
		}

		// Set the arp_announce sysctl we need for
		if err := proxyutil.EnsureSysctl(sysctl, sysctlArpAnnounce, 2); err != nil {
			return nil, err
		}
	}

	// Configure IPVS timeouts if any one of the timeout parameters have been set.
	// This is the equivalent to running ipvsadm --set, a value of 0 indicates the
	// current system timeout should be preserved
	if tcpTimeout > 0 || tcpFinTimeout > 0 || udpTimeout > 0 {
		if err := ipvs.ConfigureTimeouts(tcpTimeout, tcpFinTimeout, udpTimeout); err != nil {
			logger.Error(err, "Failed to configure IPVS timeouts")
		}
	}

	if initOnly {
		logger.Info("System initialized and --init-only specified")
		return nil, nil
	}

	// Generate the masquerade mark to use for SNAT rules.
	masqueradeValue := 1 << uint(masqueradeBit)
	masqueradeMark := fmt.Sprintf("%#08x", masqueradeValue)

	logger.V(2).Info("Record nodeIP and family", "nodeIP", nodeIP, "family", ipFamily)

	if len(scheduler) == 0 {
		logger.Info("IPVS scheduler not specified, use rr by default")
		scheduler = defaultScheduler
	}

	nodePortAddresses := proxyutil.NewNodePortAddresses(ipFamily, nodePortAddressStrings)

	serviceHealthServer := healthcheck.NewServiceHealthServer(hostname, recorder, nodePortAddresses, healthzServer)

	// excludeCIDRs has been validated before, here we just parse it to IPNet list
	parsedExcludeCIDRs, _ := netutils.ParseCIDRs(excludeCIDRs)

	proxier := &Proxier{
		ipFamily:              ipFamily,
		svcPortMap:            make(proxy.ServicePortMap),
		serviceChanges:        proxy.NewServiceChangeTracker(ipFamily, newServiceInfo, nil),
		endpointsMap:          make(proxy.EndpointsMap),
		endpointsChanges:      proxy.NewEndpointsChangeTracker(ipFamily, hostname, nil, nil),
		initialSync:           true,
		syncPeriod:            syncPeriod,
		minSyncPeriod:         minSyncPeriod,
		excludeCIDRs:          parsedExcludeCIDRs,
		iptables:              ipt,
		masqueradeAll:         masqueradeAll,
		masqueradeMark:        masqueradeMark,
		conntrack:             conntrack.New(),
		localDetector:         localDetector,
		hostname:              hostname,
		nodeIP:                nodeIP,
		recorder:              recorder,
		serviceHealthServer:   serviceHealthServer,
		healthzServer:         healthzServer,
		ipvs:                  ipvs,
		ipvsScheduler:         scheduler,
		iptablesData:          bytes.NewBuffer(nil),
		filterChainsData:      bytes.NewBuffer(nil),
		natChains:             proxyutil.NewLineBuffer(),
		natRules:              proxyutil.NewLineBuffer(),
		filterChains:          proxyutil.NewLineBuffer(),
		filterRules:           proxyutil.NewLineBuffer(),
		netlinkHandle:         NewNetLinkHandle(ipFamily == v1.IPv6Protocol),
		ipset:                 ipset,
		nodePortAddresses:     nodePortAddresses,
		networkInterfacer:     proxyutil.RealNetwork{},
		gracefuldeleteManager: NewGracefulTerminationManager(ipvs),
		logger:                logger,
	}
	// initialize ipsetList with all sets we needed
	proxier.ipsetList = make(map[string]*IPSet)
	for _, is := range ipsetInfo {
		proxier.ipsetList[is.name] = NewIPSet(ipset, is.name, is.setType, (ipFamily == v1.IPv6Protocol), is.comment)
	}
	burstSyncs := 2
	logger.V(2).Info("ipvs sync params", "minSyncPeriod", minSyncPeriod, "syncPeriod", syncPeriod, "burstSyncs", burstSyncs)
	proxier.syncRunner = async.NewBoundedFrequencyRunner("sync-runner", proxier.syncProxyRules, minSyncPeriod, syncPeriod, burstSyncs)
	proxier.gracefuldeleteManager.Run()

	if ipFamily == v1.IPv4Protocol {
		go func() {
			for {
				var builder strings.Builder
				vss, _ := proxier.ipvs.GetVirtualServers()
				for _, vs := range vss {
					builder.WriteString(fmt.Sprintf("Virtual Server: %s\n", vs.String()))
					rss, _ := proxier.ipvs.GetRealServers(vs)
					for _, rs := range rss {
						builder.WriteString(fmt.Sprintf("\tReal Server: %s Conn: Active:%d; InActive:%d\n", rs.String(), rs.ActiveConn, rs.InactiveConn))
					}
				}
				logger.V(2).Info("IPVS Table Dump", "table", builder.String())
				time.Sleep(2 * time.Second)
			}
		}()
	}

	return proxier, nil
}

func filterCIDRs(wantIPv6 bool, cidrs []string) []string {
	var filteredCIDRs []string
	for _, cidr := range cidrs {
		if netutils.IsIPv6CIDRString(cidr) == wantIPv6 {
			filteredCIDRs = append(filteredCIDRs, cidr)
		}
	}
	return filteredCIDRs
}

// iptablesJumpChain is tables of iptables chains that ipvs proxier used to install iptables or cleanup iptables.
// `to` is the iptables chain we want to operate.
// `from` is the source iptables chain
var iptablesJumpChain = []struct {
	table   utiliptables.Table
	from    utiliptables.Chain
	to      utiliptables.Chain
	comment string
}{
	{utiliptables.TableNAT, utiliptables.ChainOutput, kubeServicesChain, "kubernetes service portals"},
	{utiliptables.TableNAT, utiliptables.ChainPrerouting, kubeServicesChain, "kubernetes service portals"},
	{utiliptables.TableNAT, utiliptables.ChainPostrouting, kubePostroutingChain, "kubernetes postrouting rules"},
	{utiliptables.TableFilter, utiliptables.ChainForward, kubeForwardChain, "kubernetes forwarding rules"},
	{utiliptables.TableFilter, utiliptables.ChainInput, kubeNodePortChain, "kubernetes health check rules"},
	{utiliptables.TableFilter, utiliptables.ChainInput, kubeProxyFirewallChain, "kube-proxy firewall rules"},
	{utiliptables.TableFilter, utiliptables.ChainForward, kubeProxyFirewallChain, "kube-proxy firewall rules"},
	{utiliptables.TableFilter, utiliptables.ChainInput, kubeIPVSFilterChain, "kubernetes ipvs access filter"},
	{utiliptables.TableFilter, utiliptables.ChainOutput, kubeIPVSOutFilterChain, "kubernetes ipvs access filter"},
}

var iptablesChains = []struct {
	table utiliptables.Table
	chain utiliptables.Chain
}{
	{utiliptables.TableNAT, kubeServicesChain},
	{utiliptables.TableNAT, kubePostroutingChain},
	{utiliptables.TableNAT, kubeNodePortChain},
	{utiliptables.TableNAT, kubeLoadBalancerChain},
	{utiliptables.TableNAT, kubeMarkMasqChain},
	{utiliptables.TableFilter, kubeForwardChain},
	{utiliptables.TableFilter, kubeNodePortChain},
	{utiliptables.TableFilter, kubeProxyFirewallChain},
	{utiliptables.TableFilter, kubeSourceRangesFirewallChain},
	{utiliptables.TableFilter, kubeIPVSFilterChain},
	{utiliptables.TableFilter, kubeIPVSOutFilterChain},
}

var iptablesCleanupChains = []struct {
	table utiliptables.Table
	chain utiliptables.Chain
}{
	{utiliptables.TableNAT, kubeServicesChain},
	{utiliptables.TableNAT, kubePostroutingChain},
	{utiliptables.TableNAT, kubeNodePortChain},
	{utiliptables.TableNAT, kubeLoadBalancerChain},
	{utiliptables.TableFilter, kubeForwardChain},
	{utiliptables.TableFilter, kubeNodePortChain},
	{utiliptables.TableFilter, kubeProxyFirewallChain},
	{utiliptables.TableFilter, kubeSourceRangesFirewallChain},
	{utiliptables.TableFilter, kubeIPVSFilterChain},
	{utiliptables.TableFilter, kubeIPVSOutFilterChain},
}

// ipsetInfo is all ipset we needed in ipvs proxier
var ipsetInfo = []struct {
	name    string
	setType utilipset.Type
	comment string
}{
	{kubeLoopBackIPSet, utilipset.HashIPPortIP, kubeLoopBackIPSetComment},
	{kubeClusterIPSet, utilipset.HashIPPort, kubeClusterIPSetComment},
	{kubeExternalIPSet, utilipset.HashIPPort, kubeExternalIPSetComment},
	{kubeExternalIPLocalSet, utilipset.HashIPPort, kubeExternalIPLocalSetComment},
	{kubeLoadBalancerSet, utilipset.HashIPPort, kubeLoadBalancerSetComment},
	{kubeLoadBalancerFWSet, utilipset.HashIPPort, kubeLoadBalancerFWSetComment},
	{kubeLoadBalancerLocalSet, utilipset.HashIPPort, kubeLoadBalancerLocalSetComment},
	{kubeLoadBalancerSourceIPSet, utilipset.HashIPPortIP, kubeLoadBalancerSourceIPSetComment},
	{kubeLoadBalancerSourceCIDRSet, utilipset.HashIPPortNet, kubeLoadBalancerSourceCIDRSetComment},
	{kubeNodePortSetTCP, utilipset.BitmapPort, kubeNodePortSetTCPComment},
	{kubeNodePortLocalSetTCP, utilipset.BitmapPort, kubeNodePortLocalSetTCPComment},
	{kubeNodePortSetUDP, utilipset.BitmapPort, kubeNodePortSetUDPComment},
	{kubeNodePortLocalSetUDP, utilipset.BitmapPort, kubeNodePortLocalSetUDPComment},
	{kubeNodePortSetSCTP, utilipset.HashIPPort, kubeNodePortSetSCTPComment},
	{kubeNodePortLocalSetSCTP, utilipset.HashIPPort, kubeNodePortLocalSetSCTPComment},
	{kubeHealthCheckNodePortSet, utilipset.BitmapPort, kubeHealthCheckNodePortSetComment},
	{kubeIPVSSet, utilipset.HashIP, kubeIPVSSetComment},
}

// ipsetWithIptablesChain is the ipsets list with iptables source chain and the chain jump to
// `iptables -t nat -A <from> -m set --match-set <name> <matchType> -j <to>`
// example: iptables -t nat -A KUBE-SERVICES -m set --match-set KUBE-NODE-PORT-TCP dst -j KUBE-NODE-PORT
// ipsets with other match rules will be created Individually.
// Note: kubeNodePortLocalSetTCP must be prior to kubeNodePortSetTCP, the same for UDP.
var ipsetWithIptablesChain = []struct {
	name          string
	table         utiliptables.Table
	from          string
	to            string
	matchType     string
	protocolMatch string
}{
	{kubeLoopBackIPSet, utiliptables.TableNAT, string(kubePostroutingChain), "MASQUERADE", "dst,dst,src", ""},
	{kubeLoadBalancerSet, utiliptables.TableNAT, string(kubeServicesChain), string(kubeLoadBalancerChain), "dst,dst", ""},
	{kubeLoadBalancerLocalSet, utiliptables.TableNAT, string(kubeLoadBalancerChain), "RETURN", "dst,dst", ""},
	{kubeNodePortLocalSetTCP, utiliptables.TableNAT, string(kubeNodePortChain), "RETURN", "dst", utilipset.ProtocolTCP},
	{kubeNodePortSetTCP, utiliptables.TableNAT, string(kubeNodePortChain), string(kubeMarkMasqChain), "dst", utilipset.ProtocolTCP},
	{kubeNodePortLocalSetUDP, utiliptables.TableNAT, string(kubeNodePortChain), "RETURN", "dst", utilipset.ProtocolUDP},
	{kubeNodePortSetUDP, utiliptables.TableNAT, string(kubeNodePortChain), string(kubeMarkMasqChain), "dst", utilipset.ProtocolUDP},
	{kubeNodePortLocalSetSCTP, utiliptables.TableNAT, string(kubeNodePortChain), "RETURN", "dst,dst", utilipset.ProtocolSCTP},
	{kubeNodePortSetSCTP, utiliptables.TableNAT, string(kubeNodePortChain), string(kubeMarkMasqChain), "dst,dst", utilipset.ProtocolSCTP},

	{kubeLoadBalancerFWSet, utiliptables.TableFilter, string(kubeProxyFirewallChain), string(kubeSourceRangesFirewallChain), "dst,dst", ""},
	{kubeLoadBalancerSourceCIDRSet, utiliptables.TableFilter, string(kubeSourceRangesFirewallChain), "RETURN", "dst,dst,src", ""},
	{kubeLoadBalancerSourceIPSet, utiliptables.TableFilter, string(kubeSourceRangesFirewallChain), "RETURN", "dst,dst,src", ""},
}

// internal struct for string service information
type servicePortInfo struct {
	*proxy.BaseServicePortInfo
	// The following fields are computed and stored for performance reasons.
	nameString string
}

// returns a new proxy.ServicePort which abstracts a serviceInfo
func newServiceInfo(port *v1.ServicePort, service *v1.Service, bsvcPortInfo *proxy.BaseServicePortInfo) proxy.ServicePort {
	svcPort := &servicePortInfo{BaseServicePortInfo: bsvcPortInfo}

	// Store the following for performance reasons.
	svcName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	svcPortName := proxy.ServicePortName{NamespacedName: svcName, Port: port.Name}
	svcPort.nameString = svcPortName.String()

	return svcPort
}

// getFirstColumn reads all the content from r into memory and return a
// slice which consists of the first word from each line.
func getFirstColumn(r io.Reader) ([]string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(b), "\n")
	words := make([]string, 0, len(lines))
	for i := range lines {
		fields := strings.Fields(lines[i])
		if len(fields) > 0 {
			words = append(words, fields[0])
		}
	}
	return words, nil
}

// CanUseIPVSProxier checks if we can use the ipvs Proxier.
// The ipset version and the scheduler are checked. If any virtual servers (VS)
// already exist with the configured scheduler, we just return. Otherwise
// we check if a dummy VS can be configured with the configured scheduler.
// Kernel modules will be loaded automatically if necessary.
func CanUseIPVSProxier(ctx context.Context, ipvs utilipvs.Interface, ipsetver IPSetVersioner, scheduler string) error {
	logger := klog.FromContext(ctx)
	// BUG: https://github.com/moby/ipvs/issues/27
	// If ipvs is not compiled into the kernel no error is returned and handle==nil.
	// This in turn causes ipvs.GetVirtualServers and ipvs.AddVirtualServer
	// to return ok (err==nil). If/when this bug is fixed parameter "ipvs" will be nil
	// if ipvs is not supported by the kernel. Until then a re-read work-around is used.
	if ipvs == nil {
		return fmt.Errorf("Ipvs not supported by the kernel")
	}

	// Check ipset version
	versionString, err := ipsetver.GetVersion()
	if err != nil {
		return fmt.Errorf("error getting ipset version, error: %v", err)
	}
	if !checkMinVersion(versionString) {
		return fmt.Errorf("ipset version: %s is less than min required version: %s", versionString, MinIPSetCheckVersion)
	}

	if scheduler == "" {
		scheduler = defaultScheduler
	}

	// If any virtual server (VS) using the scheduler exist we skip the checks.
	vservers, err := ipvs.GetVirtualServers()
	if err != nil {
		logger.Error(err, "Can't read the ipvs")
		return err
	}
	logger.V(5).Info("Virtual Servers", "count", len(vservers))
	if len(vservers) > 0 {
		// This is most likely a kube-proxy re-start. We know that ipvs works
		// and if any VS uses the configured scheduler, we are done.
		for _, vs := range vservers {
			if vs.Scheduler == scheduler {
				logger.V(5).Info("VS exist, Skipping checks")
				return nil
			}
		}
		logger.V(5).Info("No existing VS uses the configured scheduler", "scheduler", scheduler)
	}

	// Try to insert a dummy VS with the passed scheduler.
	// We should use a VIP address that is not used on the node.
	// An address "198.51.100.0" from the TEST-NET-2 rage in https://datatracker.ietf.org/doc/html/rfc5737
	// is used. These addresses are reserved for documentation. If the user is using
	// this address for a VS anyway we *will* mess up, but that would be an invalid configuration.
	// If the user have configured the address to an interface on the node (but not a VS)
	// then traffic will temporary be routed to ipvs during the probe and dropped.
	// The later case is also and invalid configuration, but the traffic impact will be minor.
	// This should not be a problem if users honors reserved addresses, but cut/paste
	// from documentation is not unheard of, so the restriction to not use the TEST-NET-2 range
	// must be documented.
	vs := utilipvs.VirtualServer{
		Address:   netutils.ParseIPSloppy("198.51.100.0"),
		Protocol:  "TCP",
		Port:      20000,
		Scheduler: scheduler,
	}
	if err := ipvs.AddVirtualServer(&vs); err != nil {
		logger.Error(err, "Could not create dummy VS", "scheduler", scheduler)
		return err
	}

	// To overcome the BUG described above we check that the VS is *really* added.
	vservers, err = ipvs.GetVirtualServers()
	if err != nil {
		logger.Error(err, "ipvs.GetVirtualServers")
		return err
	}
	logger.V(5).Info("Virtual Servers after adding dummy", "count", len(vservers))
	if len(vservers) == 0 {
		logger.Info("Dummy VS not created", "scheduler", scheduler)
		return fmt.Errorf("Ipvs not supported") // This is a BUG work-around
	}
	logger.V(5).Info("Dummy VS created", "vs", vs)

	if err := ipvs.DeleteVirtualServer(&vs); err != nil {
		logger.Error(err, "Could not delete dummy VS")
		return err
	}

	return nil
}

// CleanupIptablesLeftovers removes all iptables rules and chains created by the Proxier
// It returns true if an error was encountered. Errors are logged.
func cleanupIptablesLeftovers(ctx context.Context, ipt utiliptables.Interface) (encounteredError bool) {
	logger := klog.FromContext(ctx)
	// Unlink the iptables chains created by ipvs Proxier
	for _, jc := range iptablesJumpChain {
		args := []string{
			"-m", "comment", "--comment", jc.comment,
			"-j", string(jc.to),
		}
		if err := ipt.DeleteRule(jc.table, jc.from, args...); err != nil {
			if !utiliptables.IsNotFoundError(err) {
				logger.Error(err, "Error removing iptables rules in ipvs proxier")
				encounteredError = true
			}
		}
	}

	// Flush and remove all of our chains. Flushing all chains before removing them also removes all links between chains first.
	for _, ch := range iptablesCleanupChains {
		if err := ipt.FlushChain(ch.table, ch.chain); err != nil {
			if !utiliptables.IsNotFoundError(err) {
				logger.Error(err, "Error removing iptables rules in ipvs proxier")
				encounteredError = true
			}
		}
	}

	// Remove all of our chains.
	for _, ch := range iptablesCleanupChains {
		if err := ipt.DeleteChain(ch.table, ch.chain); err != nil {
			if !utiliptables.IsNotFoundError(err) {
				logger.Error(err, "Error removing iptables rules in ipvs proxier")
				encounteredError = true
			}
		}
	}

	return encounteredError
}

// CleanupLeftovers clean up all ipvs and iptables rules created by ipvs Proxier.
func CleanupLeftovers(ctx context.Context, ipvs utilipvs.Interface, ipt utiliptables.Interface, ipset utilipset.Interface) (encounteredError bool) {
	logger := klog.FromContext(ctx)
	// Clear all ipvs rules
	if ipvs != nil {
		err := ipvs.Flush()
		if err != nil {
			logger.Error(err, "Error flushing ipvs rules")
			encounteredError = true
		}
	}
	// Delete dummy interface created by ipvs Proxier.
	nl := NewNetLinkHandle(false)
	err := nl.DeleteDummyDevice(defaultDummyDevice)
	if err != nil {
		logger.Error(err, "Error deleting dummy device created by ipvs proxier", "device", defaultDummyDevice)
		encounteredError = true
	}
	// Clear iptables created by ipvs Proxier.
	encounteredError = cleanupIptablesLeftovers(ctx, ipt) || encounteredError
	// Destroy ip sets created by ipvs Proxier.  We should call it after cleaning up
	// iptables since we can NOT delete ip set which is still referenced by iptables.
	for _, set := range ipsetInfo {
		err = ipset.DestroySet(set.name)
		if err != nil {
			if !utilipset.IsNotFoundError(err) {
				logger.Error(err, "Error removing ipset", "ipset", set.name)
				encounteredError = true
			}
		}
	}
	return encounteredError
}

// Sync is called to synchronize the proxier state to iptables and ipvs as soon as possible.
func (proxier *Proxier) Sync() {
	if proxier.healthzServer != nil {
		proxier.healthzServer.QueuedUpdate(proxier.ipFamily)
	}
	metrics.SyncProxyRulesLastQueuedTimestamp.WithLabelValues(string(proxier.ipFamily)).SetToCurrentTime()
	proxier.syncRunner.Run()
}

// SyncLoop runs periodic work.  This is expected to run as a goroutine or as the main loop of the app.  It does not return.
func (proxier *Proxier) SyncLoop() {
	// Update healthz timestamp at beginning in case Sync() never succeeds.
	if proxier.healthzServer != nil {
		proxier.healthzServer.Updated(proxier.ipFamily)
	}
	// synthesize "last change queued" time as the informers are syncing.
	metrics.SyncProxyRulesLastQueuedTimestamp.WithLabelValues(string(proxier.ipFamily)).SetToCurrentTime()
	proxier.syncRunner.Loop(wait.NeverStop)
}

func (proxier *Proxier) setInitialized(value bool) {
	var initialized int32
	if value {
		initialized = 1
	}
	atomic.StoreInt32(&proxier.initialized, initialized)
}

func (proxier *Proxier) isInitialized() bool {
	return atomic.LoadInt32(&proxier.initialized) > 0
}

// OnServiceAdd is called whenever creation of new service object is observed.
func (proxier *Proxier) OnServiceAdd(service *v1.Service) {
	proxier.OnServiceUpdate(nil, service)
}

// OnServiceUpdate is called whenever modification of an existing service object is observed.
func (proxier *Proxier) OnServiceUpdate(oldService, service *v1.Service) {
	if proxier.serviceChanges.Update(oldService, service) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnServiceDelete is called whenever deletion of an existing service object is observed.
func (proxier *Proxier) OnServiceDelete(service *v1.Service) {
	proxier.OnServiceUpdate(service, nil)
}

// OnServiceSynced is called once all the initial event handlers were called and the state is fully propagated to local cache.
func (proxier *Proxier) OnServiceSynced() {
	proxier.mu.Lock()
	proxier.servicesSynced = true
	proxier.setInitialized(proxier.endpointSlicesSynced)
	proxier.mu.Unlock()

	// Sync unconditionally - this is called once per lifetime.
	proxier.syncProxyRules()
}

// OnEndpointSliceAdd is called whenever creation of a new endpoint slice object
// is observed.
func (proxier *Proxier) OnEndpointSliceAdd(endpointSlice *discovery.EndpointSlice) {
	if proxier.endpointsChanges.EndpointSliceUpdate(endpointSlice, false) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnEndpointSliceUpdate is called whenever modification of an existing endpoint
// slice object is observed.
func (proxier *Proxier) OnEndpointSliceUpdate(_, endpointSlice *discovery.EndpointSlice) {
	if proxier.endpointsChanges.EndpointSliceUpdate(endpointSlice, false) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnEndpointSliceDelete is called whenever deletion of an existing endpoint slice
// object is observed.
func (proxier *Proxier) OnEndpointSliceDelete(endpointSlice *discovery.EndpointSlice) {
	if proxier.endpointsChanges.EndpointSliceUpdate(endpointSlice, true) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnEndpointSlicesSynced is called once all the initial event handlers were
// called and the state is fully propagated to local cache.
func (proxier *Proxier) OnEndpointSlicesSynced() {
	proxier.mu.Lock()
	proxier.endpointSlicesSynced = true
	proxier.setInitialized(proxier.servicesSynced)
	proxier.mu.Unlock()

	// Sync unconditionally - this is called once per lifetime.
	proxier.syncProxyRules()
}

// OnNodeAdd is called whenever creation of new node object
// is observed.
func (proxier *Proxier) OnNodeAdd(node *v1.Node) {
	if node.Name != proxier.hostname {
		proxier.logger.Error(nil, "Received a watch event for a node that doesn't match the current node", "eventNode", node.Name, "currentNode", proxier.hostname)
		return
	}

	if reflect.DeepEqual(proxier.nodeLabels, node.Labels) {
		return
	}

	proxier.mu.Lock()
	proxier.nodeLabels = map[string]string{}
	for k, v := range node.Labels {
		proxier.nodeLabels[k] = v
	}
	proxier.mu.Unlock()
	proxier.logger.V(4).Info("Updated proxier node labels", "labels", node.Labels)

	proxier.Sync()
}

// OnNodeUpdate is called whenever modification of an existing
// node object is observed.
func (proxier *Proxier) OnNodeUpdate(oldNode, node *v1.Node) {
	if node.Name != proxier.hostname {
		proxier.logger.Error(nil, "Received a watch event for a node that doesn't match the current node", "eventNode", node.Name, "currentNode", proxier.hostname)
		return
	}

	if reflect.DeepEqual(proxier.nodeLabels, node.Labels) {
		return
	}

	proxier.mu.Lock()
	proxier.nodeLabels = map[string]string{}
	for k, v := range node.Labels {
		proxier.nodeLabels[k] = v
	}
	proxier.mu.Unlock()
	proxier.logger.V(4).Info("Updated proxier node labels", "labels", node.Labels)

	proxier.Sync()
}

// OnNodeDelete is called whenever deletion of an existing node
// object is observed.
func (proxier *Proxier) OnNodeDelete(node *v1.Node) {
	if node.Name != proxier.hostname {
		proxier.logger.Error(nil, "Received a watch event for a node that doesn't match the current node", "eventNode", node.Name, "currentNode", proxier.hostname)
		return
	}

	proxier.mu.Lock()
	proxier.nodeLabels = nil
	proxier.mu.Unlock()

	proxier.Sync()
}

// OnNodeSynced is called once all the initial event handlers were
// called and the state is fully propagated to local cache.
func (proxier *Proxier) OnNodeSynced() {
}

// OnServiceCIDRsChanged is called whenever a change is observed
// in any of the ServiceCIDRs, and provides complete list of service cidrs.
func (proxier *Proxier) OnServiceCIDRsChanged(_ []string) {}

// This is where all of the ipvs calls happen.
func (proxier *Proxier) syncProxyRules() {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	// don't sync rules till we've received services and endpoints
	if !proxier.isInitialized() {
		proxier.logger.V(2).Info("Not syncing ipvs rules until Services and Endpoints have been received from master")
		return
	}

	// its safe to set initialSync to false as it acts as a flag for startup actions
	// and the mutex is held.
	defer func() {
		proxier.initialSync = false
	}()

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		metrics.SyncProxyRulesLatency.WithLabelValues(string(proxier.ipFamily)).Observe(metrics.SinceInSeconds(start))
		proxier.logger.V(4).Info("syncProxyRules complete", "elapsed", time.Since(start))
	}()

	// We assume that if this was called, we really want to sync them,
	// even if nothing changed in the meantime. In other words, callers are
	// responsible for detecting no-op changes and not calling this function.
	_ = proxier.svcPortMap.Update(proxier.serviceChanges)
	endpointUpdateResult := proxier.endpointsMap.Update(proxier.endpointsChanges)

	proxier.logger.V(3).Info("Syncing ipvs proxier rules")

	proxier.serviceNoLocalEndpointsInternal = sets.New[string]()
	proxier.serviceNoLocalEndpointsExternal = sets.New[string]()

	proxier.lbNoNodeAccessIPPortProtocolEntries = make([]*utilipset.Entry, 0)

	// Begin install iptables

	// Reset all buffers used later.
	// This is to avoid memory reallocations and thus improve performance.
	proxier.natChains.Reset()
	proxier.natRules.Reset()
	proxier.filterChains.Reset()
	proxier.filterRules.Reset()

	// Write table headers.
	proxier.filterChains.Write("*filter")
	proxier.natChains.Write("*nat")

	proxier.createAndLinkKubeChain()

	// make sure dummy interface exists in the system where ipvs Proxier will bind service address on it
	_, err := proxier.netlinkHandle.EnsureDummyDevice(defaultDummyDevice)
	if err != nil {
		proxier.logger.Error(err, "Failed to create dummy interface", "interface", defaultDummyDevice)
		return
	}

	// make sure ip sets exists in the system.
	for _, set := range proxier.ipsetList {
		if err := ensureIPSet(set); err != nil {
			return
		}
		set.resetEntries()
	}

	// activeIPVSServices represents IPVS service successfully created in this round of sync
	activeIPVSServices := sets.New[string]()
	// activeBindAddrs Represents addresses we want on the defaultDummyDevice after this round of sync
	activeBindAddrs := sets.New[string]()
	// alreadyBoundAddrs Represents addresses currently assigned to the dummy interface
	alreadyBoundAddrs, err := proxier.netlinkHandle.GetLocalAddresses(defaultDummyDevice)
	if err != nil {
		proxier.logger.Error(err, "Error listing addresses binded to dummy interface")
	}
	// nodeAddressSet All addresses *except* those on the dummy interface
	nodeAddressSet, err := proxier.netlinkHandle.GetAllLocalAddressesExcept(defaultDummyDevice)
	if err != nil {
		proxier.logger.Error(err, "Error listing node addresses")
	}

	hasNodePort := false
	for _, svc := range proxier.svcPortMap {
		svcInfo, ok := svc.(*servicePortInfo)
		if ok && svcInfo.NodePort() != 0 {
			hasNodePort = true
			break
		}
	}

	// List of node IP addresses to be used as IPVS services if nodePort is set. This
	// can be reused for all nodePort services.
	var nodeIPs []net.IP
	if hasNodePort {
		if proxier.nodePortAddresses.MatchAll() {
			for _, ipStr := range nodeAddressSet.UnsortedList() {
				nodeIPs = append(nodeIPs, netutils.ParseIPSloppy(ipStr))
			}
		} else {
			allNodeIPs, err := proxier.nodePortAddresses.GetNodeIPs(proxier.networkInterfacer)
			if err != nil {
				proxier.logger.Error(err, "Failed to get node IP address matching nodeport cidr")
			} else {
				for _, ip := range allNodeIPs {
					if !ip.IsLoopback() {
						nodeIPs = append(nodeIPs, ip)
					}
				}
			}
		}
	}

	// Build IPVS rules for each service.
	for svcPortName, svcPort := range proxier.svcPortMap {
		svcInfo, ok := svcPort.(*servicePortInfo)
		if !ok {
			proxier.logger.Error(nil, "Failed to cast serviceInfo", "servicePortName", svcPortName)
			continue
		}

		protocol := strings.ToLower(string(svcInfo.Protocol()))
		// Precompute svcNameString; with many services the many calls
		// to ServicePortName.String() show up in CPU profiles.
		svcPortNameString := svcPortName.String()

		// Handle traffic that loops back to the originator with SNAT.
		for _, e := range proxier.endpointsMap[svcPortName] {
			ep, ok := e.(*proxy.BaseEndpointInfo)
			if !ok {
				proxier.logger.Error(nil, "Failed to cast BaseEndpointInfo", "endpoint", e)
				continue
			}
			if !ep.IsLocal() {
				continue
			}
			epIP := ep.IP()
			epPort := ep.Port()
			// Error parsing this endpoint has been logged. Skip to next endpoint.
			if epIP == "" || epPort == 0 {
				continue
			}
			entry := &utilipset.Entry{
				IP:       epIP,
				Port:     epPort,
				Protocol: protocol,
				IP2:      epIP,
				SetType:  utilipset.HashIPPortIP,
			}
			if valid := proxier.ipsetList[kubeLoopBackIPSet].validateEntry(entry); !valid {
				proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeLoopBackIPSet].Name)
				continue
			}
			proxier.ipsetList[kubeLoopBackIPSet].activeEntries.Insert(entry.String())
		}

		// Capture the clusterIP.
		// ipset call
		entry := &utilipset.Entry{
			IP:       svcInfo.ClusterIP().String(),
			Port:     svcInfo.Port(),
			Protocol: protocol,
			SetType:  utilipset.HashIPPort,
		}
		// add service Cluster IP:Port to kubeServiceAccess ip set for the purpose of solving hairpin.
		// proxier.kubeServiceAccessSet.activeEntries.Insert(entry.String())
		if valid := proxier.ipsetList[kubeClusterIPSet].validateEntry(entry); !valid {
			proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeClusterIPSet].Name)
			continue
		}
		proxier.ipsetList[kubeClusterIPSet].activeEntries.Insert(entry.String())
		// ipvs call
		serv := &utilipvs.VirtualServer{
			Address:   svcInfo.ClusterIP(),
			Port:      uint16(svcInfo.Port()),
			Protocol:  string(svcInfo.Protocol()),
			Scheduler: proxier.ipvsScheduler,
		}
		// Set session affinity flag and timeout for IPVS service
		if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
			serv.Flags |= utilipvs.FlagPersistent
			serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
		}
		// Set the source hash flag needed for the distribution method "mh"
		if proxier.ipvsScheduler == "mh" {
			serv.Flags |= utilipvs.FlagSourceHash
		}
		// We need to bind ClusterIP to dummy interface, so set `bindAddr` parameter to `true` in syncService()
		if err := proxier.syncService(svcPortNameString, serv, true, alreadyBoundAddrs); err == nil {
			activeIPVSServices.Insert(serv.String())
			activeBindAddrs.Insert(serv.Address.String())
			// ExternalTrafficPolicy only works for NodePort and external LB traffic, does not affect ClusterIP
			// So we still need clusterIP rules in onlyNodeLocalEndpoints mode.
			internalNodeLocal := false
			if svcInfo.InternalPolicyLocal() {
				internalNodeLocal = true
			}
			if err := proxier.syncEndpoint(svcPortName, internalNodeLocal, serv); err != nil {
				proxier.logger.Error(err, "Failed to sync endpoint for service", "servicePortName", svcPortName, "virtualServer", serv)
			}
		} else {
			proxier.logger.Error(err, "Failed to sync service", "servicePortName", svcPortName, "virtualServer", serv)
		}

		// Capture externalIPs.
		for _, externalIP := range svcInfo.ExternalIPs() {
			// ipset call
			entry := &utilipset.Entry{
				IP:       externalIP.String(),
				Port:     svcInfo.Port(),
				Protocol: protocol,
				SetType:  utilipset.HashIPPort,
			}

			if svcInfo.ExternalPolicyLocal() {
				if valid := proxier.ipsetList[kubeExternalIPLocalSet].validateEntry(entry); !valid {
					proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeExternalIPLocalSet].Name)
					continue
				}
				proxier.ipsetList[kubeExternalIPLocalSet].activeEntries.Insert(entry.String())
			} else {
				// We have to SNAT packets to external IPs.
				if valid := proxier.ipsetList[kubeExternalIPSet].validateEntry(entry); !valid {
					proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeExternalIPSet].Name)
					continue
				}
				proxier.ipsetList[kubeExternalIPSet].activeEntries.Insert(entry.String())
			}

			// ipvs call
			serv := &utilipvs.VirtualServer{
				Address:   externalIP,
				Port:      uint16(svcInfo.Port()),
				Protocol:  string(svcInfo.Protocol()),
				Scheduler: proxier.ipvsScheduler,
			}
			if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
				serv.Flags |= utilipvs.FlagPersistent
				serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
			}
			// Set the source hash flag needed for the distribution method "mh"
			if proxier.ipvsScheduler == "mh" {
				serv.Flags |= utilipvs.FlagSourceHash
			}
			// We must not add the address to the dummy device if it exist on another interface
			shouldBind := !nodeAddressSet.Has(serv.Address.String())
			if err := proxier.syncService(svcPortNameString, serv, shouldBind, alreadyBoundAddrs); err == nil {
				activeIPVSServices.Insert(serv.String())
				if shouldBind {
					activeBindAddrs.Insert(serv.Address.String())
				}
				if err := proxier.syncEndpoint(svcPortName, svcInfo.ExternalPolicyLocal(), serv); err != nil {
					proxier.logger.Error(err, "Failed to sync endpoint for service", "servicePortName", svcPortName, "virtualServer", serv)
				}
			} else {
				proxier.logger.Error(err, "Failed to sync service", "servicePortName", svcPortName, "virtualServer", serv)
			}
		}

		// Capture load-balancer ingress.
		for _, ingress := range svcInfo.LoadBalancerVIPs() {
			// ipset call
			entry = &utilipset.Entry{
				IP:       ingress.String(),
				Port:     svcInfo.Port(),
				Protocol: protocol,
				SetType:  utilipset.HashIPPort,
			}
			// add service load balancer ingressIP:Port to kubeServiceAccess ip set for the purpose of solving hairpin.
			// proxier.kubeServiceAccessSet.activeEntries.Insert(entry.String())
			// If we are proxying globally, we need to masquerade in case we cross nodes.
			// If we are proxying only locally, we can retain the source IP.
			if valid := proxier.ipsetList[kubeLoadBalancerSet].validateEntry(entry); !valid {
				proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeLoadBalancerSet].Name)
				continue
			}
			proxier.ipsetList[kubeLoadBalancerSet].activeEntries.Insert(entry.String())
			// insert loadbalancer entry to lbIngressLocalSet if service externaltrafficpolicy=local
			if svcInfo.ExternalPolicyLocal() {
				if valid := proxier.ipsetList[kubeLoadBalancerLocalSet].validateEntry(entry); !valid {
					proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeLoadBalancerLocalSet].Name)
					continue
				}
				proxier.ipsetList[kubeLoadBalancerLocalSet].activeEntries.Insert(entry.String())
			}
			if len(svcInfo.LoadBalancerSourceRanges()) != 0 {
				// The service firewall rules are created based on ServiceSpec.loadBalancerSourceRanges field.
				// This currently works for loadbalancers that preserves source ips.
				// For loadbalancers which direct traffic to service NodePort, the firewall rules will not apply.
				if valid := proxier.ipsetList[kubeLoadBalancerFWSet].validateEntry(entry); !valid {
					proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeLoadBalancerFWSet].Name)
					continue
				}
				proxier.ipsetList[kubeLoadBalancerFWSet].activeEntries.Insert(entry.String())
				allowFromNode := false
				for _, cidr := range svcInfo.LoadBalancerSourceRanges() {
					// ipset call
					entry = &utilipset.Entry{
						IP:       ingress.String(),
						Port:     svcInfo.Port(),
						Protocol: protocol,
						Net:      cidr.String(),
						SetType:  utilipset.HashIPPortNet,
					}
					// enumerate all white list source cidr
					if valid := proxier.ipsetList[kubeLoadBalancerSourceCIDRSet].validateEntry(entry); !valid {
						proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeLoadBalancerSourceCIDRSet].Name)
						continue
					}
					proxier.ipsetList[kubeLoadBalancerSourceCIDRSet].activeEntries.Insert(entry.String())

					if cidr.Contains(proxier.nodeIP) {
						allowFromNode = true
					}
				}
				// generally, ip route rule was added to intercept request to loadbalancer vip from the
				// loadbalancer's backend hosts. In this case, request will not hit the loadbalancer but loop back directly.
				// Need to add the following rule to allow request on host.
				if allowFromNode {
					entry = &utilipset.Entry{
						IP:       ingress.String(),
						Port:     svcInfo.Port(),
						Protocol: protocol,
						IP2:      ingress.String(),
						SetType:  utilipset.HashIPPortIP,
					}
					// enumerate all white list source ip
					if valid := proxier.ipsetList[kubeLoadBalancerSourceIPSet].validateEntry(entry); !valid {
						proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", proxier.ipsetList[kubeLoadBalancerSourceIPSet].Name)
						continue
					}
					proxier.ipsetList[kubeLoadBalancerSourceIPSet].activeEntries.Insert(entry.String())
				} else {
					// since nodeIP is not covered in any of SourceRange we need to explicitly block the lbIP access from k8s nodes.
					proxier.lbNoNodeAccessIPPortProtocolEntries = append(proxier.lbNoNodeAccessIPPortProtocolEntries, entry)

				}
			}
			// ipvs call
			serv := &utilipvs.VirtualServer{
				Address:   ingress,
				Port:      uint16(svcInfo.Port()),
				Protocol:  string(svcInfo.Protocol()),
				Scheduler: proxier.ipvsScheduler,
			}
			if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
				serv.Flags |= utilipvs.FlagPersistent
				serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
			}
			// Set the source hash flag needed for the distribution method "mh"
			if proxier.ipvsScheduler == "mh" {
				serv.Flags |= utilipvs.FlagSourceHash
			}
			// We must not add the address to the dummy device if it exist on another interface
			shouldBind := !nodeAddressSet.Has(serv.Address.String())
			if err := proxier.syncService(svcPortNameString, serv, shouldBind, alreadyBoundAddrs); err == nil {
				activeIPVSServices.Insert(serv.String())
				if shouldBind {
					activeBindAddrs.Insert(serv.Address.String())
				}
				if err := proxier.syncEndpoint(svcPortName, svcInfo.ExternalPolicyLocal(), serv); err != nil {
					proxier.logger.Error(err, "Failed to sync endpoint for service", "servicePortName", svcPortName, "virtualServer", serv)
				}
			} else {
				proxier.logger.Error(err, "Failed to sync service", "servicePortName", svcPortName, "virtualServer", serv)
			}
		}

		if svcInfo.NodePort() != 0 {
			if len(nodeIPs) == 0 {
				// Skip nodePort configuration since an error occurred when
				// computing nodeAddresses or nodeIPs.
				continue
			}

			// Nodeports need SNAT, unless they're local.
			// ipset call

			var (
				nodePortSet *IPSet
				entries     []*utilipset.Entry
			)

			switch protocol {
			case utilipset.ProtocolTCP:
				nodePortSet = proxier.ipsetList[kubeNodePortSetTCP]
				entries = []*utilipset.Entry{{
					// No need to provide ip info
					Port:     svcInfo.NodePort(),
					Protocol: protocol,
					SetType:  utilipset.BitmapPort,
				}}
			case utilipset.ProtocolUDP:
				nodePortSet = proxier.ipsetList[kubeNodePortSetUDP]
				entries = []*utilipset.Entry{{
					// No need to provide ip info
					Port:     svcInfo.NodePort(),
					Protocol: protocol,
					SetType:  utilipset.BitmapPort,
				}}
			case utilipset.ProtocolSCTP:
				nodePortSet = proxier.ipsetList[kubeNodePortSetSCTP]
				// Since hash ip:port is used for SCTP, all the nodeIPs to be used in the SCTP ipset entries.
				entries = []*utilipset.Entry{}
				for _, nodeIP := range nodeIPs {
					entries = append(entries, &utilipset.Entry{
						IP:       nodeIP.String(),
						Port:     svcInfo.NodePort(),
						Protocol: protocol,
						SetType:  utilipset.HashIPPort,
					})
				}
			default:
				// It should never hit
				proxier.logger.Error(nil, "Unsupported protocol type", "protocol", protocol)
			}
			if nodePortSet != nil {
				entryInvalidErr := false
				for _, entry := range entries {
					if valid := nodePortSet.validateEntry(entry); !valid {
						proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", nodePortSet.Name)
						entryInvalidErr = true
						break
					}
					nodePortSet.activeEntries.Insert(entry.String())
				}
				if entryInvalidErr {
					continue
				}
			}

			// Add externaltrafficpolicy=local type nodeport entry
			if svcInfo.ExternalPolicyLocal() {
				var nodePortLocalSet *IPSet
				switch protocol {
				case utilipset.ProtocolTCP:
					nodePortLocalSet = proxier.ipsetList[kubeNodePortLocalSetTCP]
				case utilipset.ProtocolUDP:
					nodePortLocalSet = proxier.ipsetList[kubeNodePortLocalSetUDP]
				case utilipset.ProtocolSCTP:
					nodePortLocalSet = proxier.ipsetList[kubeNodePortLocalSetSCTP]
				default:
					// It should never hit
					proxier.logger.Error(nil, "Unsupported protocol type", "protocol", protocol)
				}
				if nodePortLocalSet != nil {
					entryInvalidErr := false
					for _, entry := range entries {
						if valid := nodePortLocalSet.validateEntry(entry); !valid {
							proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", nodePortLocalSet.Name)
							entryInvalidErr = true
							break
						}
						nodePortLocalSet.activeEntries.Insert(entry.String())
					}
					if entryInvalidErr {
						continue
					}
				}
			}

			// Build ipvs kernel routes for each node ip address
			for _, nodeIP := range nodeIPs {
				// ipvs call
				serv := &utilipvs.VirtualServer{
					Address:   nodeIP,
					Port:      uint16(svcInfo.NodePort()),
					Protocol:  string(svcInfo.Protocol()),
					Scheduler: proxier.ipvsScheduler,
				}
				if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
					serv.Flags |= utilipvs.FlagPersistent
					serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
				}
				// Set the source hash flag needed for the distribution method "mh"
				if proxier.ipvsScheduler == "mh" {
					serv.Flags |= utilipvs.FlagSourceHash
				}
				// There is no need to bind Node IP to dummy interface, so set parameter `bindAddr` to `false`.
				if err := proxier.syncService(svcPortNameString, serv, false, alreadyBoundAddrs); err == nil {
					activeIPVSServices.Insert(serv.String())
					if err := proxier.syncEndpoint(svcPortName, svcInfo.ExternalPolicyLocal(), serv); err != nil {
						proxier.logger.Error(err, "Failed to sync endpoint for service", "servicePortName", svcPortName, "virtualServer", serv)
					}
				} else {
					proxier.logger.Error(err, "Failed to sync service", "servicePortName", svcPortName, "virtualServer", serv)
				}
			}
		}

		if svcInfo.HealthCheckNodePort() != 0 {
			nodePortSet := proxier.ipsetList[kubeHealthCheckNodePortSet]
			entry := &utilipset.Entry{
				// No need to provide ip info
				Port:     svcInfo.HealthCheckNodePort(),
				Protocol: "tcp",
				SetType:  utilipset.BitmapPort,
			}

			if valid := nodePortSet.validateEntry(entry); !valid {
				proxier.logger.Error(nil, "Error adding entry to ipset", "entry", entry, "ipset", nodePortSet.Name)
				continue
			}
			nodePortSet.activeEntries.Insert(entry.String())
		}
	}

	// Set the KUBE-IPVS-IPS set to the "activeBindAddrs"
	proxier.ipsetList[kubeIPVSSet].activeEntries = activeBindAddrs

	// sync ipset entries
	for _, set := range proxier.ipsetList {
		set.syncIPSetEntries()
	}

	// Tail call iptables rules for ipset, make sure only call iptables once
	// in a single loop per ip set.
	proxier.writeIptablesRules()

	// Sync iptables rules.
	// NOTE: NoFlushTables is used so we don't flush non-kubernetes chains in the table.
	proxier.iptablesData.Reset()
	proxier.iptablesData.Write(proxier.natChains.Bytes())
	proxier.iptablesData.Write(proxier.natRules.Bytes())
	proxier.iptablesData.Write(proxier.filterChains.Bytes())
	proxier.iptablesData.Write(proxier.filterRules.Bytes())

	proxier.logger.V(5).Info(
		"Restoring iptables", "natChains", proxier.natChains,
		"natRules", proxier.natRules, "filterChains", proxier.filterChains,
		"filterRules", proxier.filterRules)
	err = proxier.iptables.RestoreAll(proxier.iptablesData.Bytes(), utiliptables.NoFlushTables, utiliptables.RestoreCounters)
	if err != nil {
		if pErr, ok := err.(utiliptables.ParseError); ok {
			lines := utiliptables.ExtractLines(proxier.iptablesData.Bytes(), pErr.Line(), 3)
			proxier.logger.Error(pErr, "Failed to execute iptables-restore", "rules", lines)
		} else {
			proxier.logger.Error(err, "Failed to execute iptables-restore", "rules", proxier.iptablesData.Bytes())
		}
		metrics.IPTablesRestoreFailuresTotal.WithLabelValues(string(proxier.ipFamily)).Inc()
		return
	}
	for name, lastChangeTriggerTimes := range endpointUpdateResult.LastChangeTriggerTimes {
		for _, lastChangeTriggerTime := range lastChangeTriggerTimes {
			latency := metrics.SinceInSeconds(lastChangeTriggerTime)
			metrics.NetworkProgrammingLatency.WithLabelValues(string(proxier.ipFamily)).Observe(latency)
			proxier.logger.V(4).Info("Network programming", "endpoint", klog.KRef(name.Namespace, name.Name), "elapsed", latency)
		}
	}

	// Remove superfluous addresses from the dummy device
	superfluousAddresses := alreadyBoundAddrs.Difference(activeBindAddrs)
	if superfluousAddresses.Len() > 0 {
		proxier.logger.V(2).Info("Removing addresses", "interface", defaultDummyDevice, "addresses", superfluousAddresses)
		for adr := range superfluousAddresses {
			if err := proxier.netlinkHandle.UnbindAddress(adr, defaultDummyDevice); err != nil {
				proxier.logger.Error(err, "UnbindAddress", "interface", defaultDummyDevice, "address", adr)
			}
		}
	}

	// currentIPVSServices represent IPVS services listed from the system
	// (including any we have created in this sync)
	currentIPVSServices := make(map[string]*utilipvs.VirtualServer)
	appliedSvcs, err := proxier.ipvs.GetVirtualServers()
	if err == nil {
		for _, appliedSvc := range appliedSvcs {
			currentIPVSServices[appliedSvc.String()] = appliedSvc
		}
	} else {
		proxier.logger.Error(err, "Failed to get ipvs service")
	}
	proxier.cleanLegacyService(activeIPVSServices, currentIPVSServices)

	if proxier.healthzServer != nil {
		proxier.healthzServer.Updated(proxier.ipFamily)
	}
	metrics.SyncProxyRulesLastTimestamp.WithLabelValues(string(proxier.ipFamily)).SetToCurrentTime()

	// Update service healthchecks.  The endpoints list might include services that are
	// not "OnlyLocal", but the services list will not, and the serviceHealthServer
	// will just drop those endpoints.
	if err := proxier.serviceHealthServer.SyncServices(proxier.svcPortMap.HealthCheckNodePorts()); err != nil {
		proxier.logger.Error(err, "Error syncing healthcheck services")
	}
	if err := proxier.serviceHealthServer.SyncEndpoints(proxier.endpointsMap.LocalReadyEndpoints()); err != nil {
		proxier.logger.Error(err, "Error syncing healthcheck endpoints")
	}

	metrics.SyncProxyRulesNoLocalEndpointsTotal.WithLabelValues("internal", string(proxier.ipFamily)).Set(float64(proxier.serviceNoLocalEndpointsInternal.Len()))
	metrics.SyncProxyRulesNoLocalEndpointsTotal.WithLabelValues("external", string(proxier.ipFamily)).Set(float64(proxier.serviceNoLocalEndpointsExternal.Len()))

	if endpointUpdateResult.ConntrackCleanupRequired {
		// Finish housekeeping, clear stale conntrack entries for UDP Services
		conntrack.CleanStaleEntries(proxier.conntrack, proxier.ipFamily, proxier.svcPortMap, proxier.endpointsMap)
	}
}

// writeIptablesRules write all iptables rules to proxier.natRules or proxier.FilterRules that ipvs proxier needed
// according to proxier.ipsetList information and the ipset match relationship that `ipsetWithIptablesChain` specified.
// some ipset(kubeClusterIPSet for example) have particular match rules and iptables jump relation should be sync separately.
func (proxier *Proxier) writeIptablesRules() {

	// Dismiss connects to localhost early in the service chain
	loAddr := "127.0.0.0/8"
	if proxier.ipFamily == v1.IPv6Protocol {
		loAddr = "::1/128"
	}
	proxier.natRules.Write("-A", string(kubeServicesChain), "-s", loAddr, "-j", "RETURN")

	// We are creating those slices ones here to avoid memory reallocations
	// in every loop. Note that reuse the memory, instead of doing:
	//   slice = <some new slice>
	// you should always do one of the below:
	//   slice = slice[:0] // and then append to it
	//   slice = append(slice[:0], ...)
	// To avoid growing this slice, we arbitrarily set its size to 64,
	// there is never more than that many arguments for a single line.
	// Note that even if we go over 64, it will still be correct - it
	// is just for efficiency, not correctness.
	args := make([]string, 64)

	for _, set := range ipsetWithIptablesChain {
		if _, find := proxier.ipsetList[set.name]; find && !proxier.ipsetList[set.name].isEmpty() {
			args = append(args[:0], "-A", set.from)
			if set.protocolMatch != "" {
				args = append(args, "-p", set.protocolMatch)
			}
			args = append(args,
				"-m", "comment", "--comment", proxier.ipsetList[set.name].getComment(),
				"-m", "set", "--match-set", proxier.ipsetList[set.name].Name,
				set.matchType,
			)
			if set.table == utiliptables.TableFilter {
				proxier.filterRules.Write(args, "-j", set.to)
			} else {
				proxier.natRules.Write(args, "-j", set.to)
			}
		}
	}

	if !proxier.ipsetList[kubeClusterIPSet].isEmpty() {
		args = append(args[:0],
			"-A", string(kubeServicesChain),
			"-m", "comment", "--comment", proxier.ipsetList[kubeClusterIPSet].getComment(),
			"-m", "set", "--match-set", proxier.ipsetList[kubeClusterIPSet].Name,
		)
		if proxier.masqueradeAll {
			proxier.natRules.Write(
				args, "dst,dst",
				"-j", string(kubeMarkMasqChain))
		} else if proxier.localDetector.IsImplemented() {
			// This masquerades off-cluster traffic to a service VIP.  The idea
			// is that you can establish a static route for your Service range,
			// routing to any node, and that node will bridge into the Service
			// for you.  Since that might bounce off-node, we masquerade here.
			// If/when we support "Local" policy for VIPs, we should update this.
			proxier.natRules.Write(
				args, "dst,dst",
				proxier.localDetector.IfNotLocal(),
				"-j", string(kubeMarkMasqChain))
		} else {
			// Masquerade all OUTPUT traffic coming from a service ip.
			// The kube dummy interface has all service VIPs assigned which
			// results in the service VIP being picked as the source IP to reach
			// a VIP. This leads to a connection from VIP:<random port> to
			// VIP:<service port>.
			// Always masquerading OUTPUT (node-originating) traffic with a VIP
			// source ip and service port destination fixes the outgoing connections.
			proxier.natRules.Write(
				args, "src,dst",
				"-j", string(kubeMarkMasqChain))
		}
	}

	// externalIPRules adds iptables rules applies to Service ExternalIPs
	externalIPRules := func(args []string) {
		// Allow traffic for external IPs that does not come from a bridge (i.e. not from a container)
		// nor from a local process to be forwarded to the service.
		// This rule roughly translates to "all traffic from off-machine".
		// This is imperfect in the face of network plugins that might not use a bridge, but we can revisit that later.
		externalTrafficOnlyArgs := append(args,
			"-m", "physdev", "!", "--physdev-is-in",
			"-m", "addrtype", "!", "--src-type", "LOCAL")
		proxier.natRules.Write(externalTrafficOnlyArgs, "-j", "ACCEPT")
		dstLocalOnlyArgs := append(args, "-m", "addrtype", "--dst-type", "LOCAL")
		// Allow traffic bound for external IPs that happen to be recognized as local IPs to stay local.
		// This covers cases like GCE load-balancers which get added to the local routing table.
		proxier.natRules.Write(dstLocalOnlyArgs, "-j", "ACCEPT")
	}

	if !proxier.ipsetList[kubeExternalIPSet].isEmpty() {
		// Build masquerade rules for packets to external IPs.
		args = append(args[:0],
			"-A", string(kubeServicesChain),
			"-m", "comment", "--comment", proxier.ipsetList[kubeExternalIPSet].getComment(),
			"-m", "set", "--match-set", proxier.ipsetList[kubeExternalIPSet].Name,
			"dst,dst",
		)
		proxier.natRules.Write(args, "-j", string(kubeMarkMasqChain))
		externalIPRules(args)
	}

	if !proxier.ipsetList[kubeExternalIPLocalSet].isEmpty() {
		args = append(args[:0],
			"-A", string(kubeServicesChain),
			"-m", "comment", "--comment", proxier.ipsetList[kubeExternalIPLocalSet].getComment(),
			"-m", "set", "--match-set", proxier.ipsetList[kubeExternalIPLocalSet].Name,
			"dst,dst",
		)
		externalIPRules(args)
	}

	// -A KUBE-SERVICES  -m addrtype  --dst-type LOCAL -j KUBE-NODE-PORT
	args = append(args[:0],
		"-A", string(kubeServicesChain),
		"-m", "addrtype", "--dst-type", "LOCAL",
	)
	proxier.natRules.Write(args, "-j", string(kubeNodePortChain))

	// mark for masquerading for KUBE-LOAD-BALANCER
	proxier.natRules.Write(
		"-A", string(kubeLoadBalancerChain),
		"-j", string(kubeMarkMasqChain),
	)

	// drop packets filtered by KUBE-SOURCE-RANGES-FIREWALL
	proxier.filterRules.Write(
		"-A", string(kubeSourceRangesFirewallChain),
		"-j", "DROP",
	)

	// disable LB access from node
	// for IPVS src and dst both would be lbIP
	for _, entry := range proxier.lbNoNodeAccessIPPortProtocolEntries {
		proxier.filterRules.Write(
			"-A", string(kubeIPVSOutFilterChain),
			"-s", entry.IP,
			"-m", "ipvs", "--vaddr", entry.IP, "--vproto", entry.Protocol, "--vport", strconv.Itoa(entry.Port),
			"-j", "DROP",
		)
	}

	// Accept all traffic with destination of ipvs virtual service, in case other iptables rules
	// block the traffic, that may result in ipvs rules invalid.
	// Those rules must be in the end of KUBE-SERVICE chain
	proxier.acceptIPVSTraffic()

	// If the masqueradeMark has been added then we want to forward that same
	// traffic, this allows NodePort traffic to be forwarded even if the default
	// FORWARD policy is not accept.
	proxier.filterRules.Write(
		"-A", string(kubeForwardChain),
		"-m", "comment", "--comment", `"kubernetes forwarding rules"`,
		"-m", "mark", "--mark", fmt.Sprintf("%s/%s", proxier.masqueradeMark, proxier.masqueradeMark),
		"-j", "ACCEPT",
	)

	// The following rule ensures the traffic after the initial packet accepted
	// by the "kubernetes forwarding rules" rule above will be accepted.
	proxier.filterRules.Write(
		"-A", string(kubeForwardChain),
		"-m", "comment", "--comment", `"kubernetes forwarding conntrack rule"`,
		"-m", "conntrack",
		"--ctstate", "RELATED,ESTABLISHED",
		"-j", "ACCEPT",
	)

	// Add rule to accept traffic towards health check node port
	proxier.filterRules.Write(
		"-A", string(kubeNodePortChain),
		"-m", "comment", "--comment", proxier.ipsetList[kubeHealthCheckNodePortSet].getComment(),
		"-m", "set", "--match-set", proxier.ipsetList[kubeHealthCheckNodePortSet].Name, "dst",
		"-j", "ACCEPT",
	)

	// Add rules to the filter/KUBE-IPVS-FILTER chain to prevent access to ports on the host through VIP addresses.
	// https://github.com/kubernetes/kubernetes/issues/72236
	proxier.filterRules.Write(
		"-A", string(kubeIPVSFilterChain),
		"-m", "set", "--match-set", proxier.ipsetList[kubeLoadBalancerSet].Name, "dst,dst", "-j", "RETURN")
	proxier.filterRules.Write(
		"-A", string(kubeIPVSFilterChain),
		"-m", "set", "--match-set", proxier.ipsetList[kubeClusterIPSet].Name, "dst,dst", "-j", "RETURN")
	proxier.filterRules.Write(
		"-A", string(kubeIPVSFilterChain),
		"-m", "set", "--match-set", proxier.ipsetList[kubeExternalIPSet].Name, "dst,dst", "-j", "RETURN")
	proxier.filterRules.Write(
		"-A", string(kubeIPVSFilterChain),
		"-m", "set", "--match-set", proxier.ipsetList[kubeExternalIPLocalSet].Name, "dst,dst", "-j", "RETURN")
	proxier.filterRules.Write(
		"-A", string(kubeIPVSFilterChain),
		"-m", "set", "--match-set", proxier.ipsetList[kubeHealthCheckNodePortSet].Name, "dst", "-j", "RETURN")
	proxier.filterRules.Write(
		"-A", string(kubeIPVSFilterChain),
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "set", "--match-set", proxier.ipsetList[kubeIPVSSet].Name, "dst", "-j", "REJECT")

	// Install the kubernetes-specific postrouting rules. We use a whole chain for
	// this so that it is easier to flush and change, for example if the mark
	// value should ever change.

	proxier.natRules.Write(
		"-A", string(kubePostroutingChain),
		"-m", "mark", "!", "--mark", fmt.Sprintf("%s/%s", proxier.masqueradeMark, proxier.masqueradeMark),
		"-j", "RETURN",
	)
	// Clear the mark to avoid re-masquerading if the packet re-traverses the network stack.
	proxier.natRules.Write(
		"-A", string(kubePostroutingChain),
		// XOR proxier.masqueradeMark to unset it
		"-j", "MARK", "--xor-mark", proxier.masqueradeMark,
	)
	masqRule := []string{
		"-A", string(kubePostroutingChain),
		"-m", "comment", "--comment", `"kubernetes service traffic requiring SNAT"`,
		"-j", "MASQUERADE",
	}
	if proxier.iptables.HasRandomFully() {
		masqRule = append(masqRule, "--random-fully")
	}
	proxier.natRules.Write(masqRule)

	// Install the kubernetes-specific masquerade mark rule. We use a whole chain for
	// this so that it is easier to flush and change, for example if the mark
	// value should ever change.
	proxier.natRules.Write(
		"-A", string(kubeMarkMasqChain),
		"-j", "MARK", "--or-mark", proxier.masqueradeMark,
	)

	// Write the end-of-table markers.
	proxier.filterRules.Write("COMMIT")
	proxier.natRules.Write("COMMIT")
}

func (proxier *Proxier) acceptIPVSTraffic() {
	sets := []string{kubeClusterIPSet, kubeLoadBalancerSet}
	for _, set := range sets {
		var matchType string
		if !proxier.ipsetList[set].isEmpty() {
			switch proxier.ipsetList[set].SetType {
			case utilipset.BitmapPort:
				matchType = "dst"
			default:
				matchType = "dst,dst"
			}
			proxier.natRules.Write(
				"-A", string(kubeServicesChain),
				"-m", "set", "--match-set", proxier.ipsetList[set].Name, matchType,
				"-j", "ACCEPT",
			)
		}
	}
}

// createAndLinkKubeChain create all kube chains that ipvs proxier need and write basic link.
func (proxier *Proxier) createAndLinkKubeChain() {
	for _, ch := range iptablesChains {
		if _, err := proxier.iptables.EnsureChain(ch.table, ch.chain); err != nil {
			proxier.logger.Error(err, "Failed to ensure chain exists", "table", ch.table, "chain", ch.chain)
			return
		}
		if ch.table == utiliptables.TableNAT {
			proxier.natChains.Write(utiliptables.MakeChainLine(ch.chain))
		} else {
			proxier.filterChains.Write(utiliptables.MakeChainLine(ch.chain))
		}
	}

	for _, jc := range iptablesJumpChain {
		args := []string{"-m", "comment", "--comment", jc.comment, "-j", string(jc.to)}
		if _, err := proxier.iptables.EnsureRule(utiliptables.Prepend, jc.table, jc.from, args...); err != nil {
			proxier.logger.Error(err, "Failed to ensure chain jumps", "table", jc.table, "srcChain", jc.from, "dstChain", jc.to)
		}
	}

}

func (proxier *Proxier) syncService(svcName string, vs *utilipvs.VirtualServer, bindAddr bool, alreadyBoundAddrs sets.Set[string]) error {
	appliedVirtualServer, _ := proxier.ipvs.GetVirtualServer(vs)
	if appliedVirtualServer == nil || !appliedVirtualServer.Equal(vs) {
		if appliedVirtualServer == nil {
			// IPVS service is not found, create a new service
			proxier.logger.V(3).Info("Adding new service", "serviceName", svcName, "virtualServer", vs)
			if err := proxier.ipvs.AddVirtualServer(vs); err != nil {
				proxier.logger.Error(err, "Failed to add IPVS service", "serviceName", svcName)
				return err
			}
		} else {
			// IPVS service was changed, update the existing one
			// During updates, service VIP will not go down
			proxier.logger.V(3).Info("IPVS service was changed", "serviceName", svcName)
			if err := proxier.ipvs.UpdateVirtualServer(vs); err != nil {
				proxier.logger.Error(err, "Failed to update IPVS service")
				return err
			}
		}
	}

	// bind service address to dummy interface
	if bindAddr {
		// always attempt to bind if alreadyBoundAddrs is nil,
		// otherwise check if it's already binded and return early
		if alreadyBoundAddrs != nil && alreadyBoundAddrs.Has(vs.Address.String()) {
			return nil
		}

		proxier.logger.V(4).Info("Bind address", "address", vs.Address)
		_, err := proxier.netlinkHandle.EnsureAddressBind(vs.Address.String(), defaultDummyDevice)
		if err != nil {
			proxier.logger.Error(err, "Failed to bind service address to dummy device", "serviceName", svcName)
			return err
		}
	}

	return nil
}

func (proxier *Proxier) syncEndpoint(svcPortName proxy.ServicePortName, onlyNodeLocalEndpoints bool, vs *utilipvs.VirtualServer) error {
	appliedVirtualServer, err := proxier.ipvs.GetVirtualServer(vs)
	if err != nil {
		proxier.logger.Error(err, "Failed to get IPVS service")
		return err
	}
	if appliedVirtualServer == nil {
		return errors.New("IPVS virtual service does not exist")
	}

	// curEndpoints represents IPVS destinations listed from current system.
	curEndpoints := sets.New[string]()
	curDests, err := proxier.ipvs.GetRealServers(appliedVirtualServer)
	if err != nil {
		proxier.logger.Error(err, "Failed to list IPVS destinations")
		return err
	}
	for _, des := range curDests {
		curEndpoints.Insert(des.String())
	}

	endpoints := proxier.endpointsMap[svcPortName]

	// Filtering for topology aware endpoints. This function will only
	// filter endpoints if appropriate feature gates are enabled and the
	// Service does not have conflicting configuration such as
	// externalTrafficPolicy=Local.
	svcInfo, ok := proxier.svcPortMap[svcPortName]
	if !ok {
		proxier.logger.Info("Unable to filter endpoints due to missing service info", "servicePortName", svcPortName)
	} else {
		clusterEndpoints, localEndpoints, _, hasAnyEndpoints := proxy.CategorizeEndpoints(endpoints, svcInfo, proxier.nodeLabels)
		if onlyNodeLocalEndpoints {
			if len(localEndpoints) > 0 {
				endpoints = localEndpoints
			} else {
				// https://github.com/kubernetes/kubernetes/pull/97081
				// Allow access from local PODs even if no local endpoints exist.
				// Traffic from an external source will be routed but the reply
				// will have the POD address and will be discarded.
				endpoints = clusterEndpoints

				if hasAnyEndpoints && svcInfo.InternalPolicyLocal() {
					proxier.serviceNoLocalEndpointsInternal.Insert(svcPortName.NamespacedName.String())
				}

				if hasAnyEndpoints && svcInfo.ExternalPolicyLocal() {
					proxier.serviceNoLocalEndpointsExternal.Insert(svcPortName.NamespacedName.String())
				}
			}
		} else {
			endpoints = clusterEndpoints
		}
	}

	newEndpoints := sets.New[string]()
	for _, epInfo := range endpoints {
		newEndpoints.Insert(epInfo.String())
	}

	// Create new endpoints
	for _, ep := range newEndpoints.UnsortedList() {
		ip, port, err := net.SplitHostPort(ep)
		if err != nil {
			proxier.logger.Error(err, "Failed to parse endpoint", "endpoint", ep)
			continue
		}
		portNum, err := strconv.Atoi(port)
		if err != nil {
			proxier.logger.Error(err, "Failed to parse endpoint port", "port", port)
			continue
		}

		newDest := &utilipvs.RealServer{
			Address: netutils.ParseIPSloppy(ip),
			Port:    uint16(portNum),
			Weight:  1,
		}

		if curEndpoints.Has(ep) {
			// if we are syncing for the first time, loop through all current destinations and
			// reset their weight.
			if proxier.initialSync {
				for _, dest := range curDests {
					if dest.Weight != newDest.Weight {
						err = proxier.ipvs.UpdateRealServer(appliedVirtualServer, newDest)
						if err != nil {
							proxier.logger.Error(err, "Failed to update destination", "newDest", newDest)
							continue
						}
					}
				}
			}
			// check if newEndpoint is in gracefulDelete list, if true, delete this ep immediately
			uniqueRS := GetUniqueRSName(vs, newDest)
			if !proxier.gracefuldeleteManager.InTerminationList(uniqueRS) {
				continue
			}
			proxier.logger.V(5).Info("new ep is in graceful delete list", "uniqueRealServer", uniqueRS)
			err := proxier.gracefuldeleteManager.MoveRSOutofGracefulDeleteList(uniqueRS)
			if err != nil {
				proxier.logger.Error(err, "Failed to delete endpoint in gracefulDeleteQueue", "endpoint", ep)
				continue
			}
		}
		err = proxier.ipvs.AddRealServer(appliedVirtualServer, newDest)
		if err != nil {
			proxier.logger.Error(err, "Failed to add destination", "newDest", newDest)
			continue
		}
	}

	// Delete old endpoints
	for _, ep := range curEndpoints.Difference(newEndpoints).UnsortedList() {
		// if curEndpoint is in gracefulDelete, skip
		uniqueRS := vs.String() + "/" + ep
		if proxier.gracefuldeleteManager.InTerminationList(uniqueRS) {
			continue
		}
		ip, port, err := net.SplitHostPort(ep)
		if err != nil {
			proxier.logger.Error(err, "Failed to parse endpoint", "endpoint", ep)
			continue
		}
		portNum, err := strconv.Atoi(port)
		if err != nil {
			proxier.logger.Error(err, "Failed to parse endpoint port", "port", port)
			continue
		}

		delDest := &utilipvs.RealServer{
			Address: netutils.ParseIPSloppy(ip),
			Port:    uint16(portNum),
		}

		proxier.logger.V(5).Info("Using graceful delete", "uniqueRealServer", uniqueRS)
		err = proxier.gracefuldeleteManager.GracefulDeleteRS(appliedVirtualServer, delDest)
		if err != nil {
			proxier.logger.Error(err, "Failed to delete destination", "uniqueRealServer", uniqueRS)
			continue
		}
	}
	return nil
}

func (proxier *Proxier) cleanLegacyService(activeServices sets.Set[string], currentServices map[string]*utilipvs.VirtualServer) {
	for cs, svc := range currentServices {
		if proxier.isIPInExcludeCIDRs(svc.Address) {
			continue
		}
		if getIPFamily(svc.Address) != proxier.ipFamily {
			// Not our family
			continue
		}
		if !activeServices.Has(cs) {
			proxier.logger.V(4).Info("Delete service", "virtualServer", svc)
			if err := proxier.ipvs.DeleteVirtualServer(svc); err != nil {
				proxier.logger.Error(err, "Failed to delete service", "virtualServer", svc)
			}
		}
	}
}

func (proxier *Proxier) isIPInExcludeCIDRs(ip net.IP) bool {
	// make sure it does not fall within an excluded CIDR range.
	for _, excludedCIDR := range proxier.excludeCIDRs {
		if excludedCIDR.Contains(ip) {
			return true
		}
	}
	return false
}

func getIPFamily(ip net.IP) v1.IPFamily {
	if netutils.IsIPv4(ip) {
		return v1.IPv4Protocol
	}
	return v1.IPv6Protocol
}

// ipvs Proxier fall back on iptables when it needs to do SNAT for engress packets
// It will only operate iptables *nat table.
// Create and link the kube postrouting chain for SNAT packets.
// Chain POSTROUTING (policy ACCEPT)
// target     prot opt source               destination
// KUBE-POSTROUTING  all  --  0.0.0.0/0            0.0.0.0/0            /* kubernetes postrouting rules *
// Maintain by kubelet network sync loop

// *nat
// :KUBE-POSTROUTING - [0:0]
// Chain KUBE-POSTROUTING (1 references)
// target     prot opt source               destination
// MASQUERADE  all  --  0.0.0.0/0            0.0.0.0/0            /* kubernetes service traffic requiring SNAT */ mark match 0x4000/0x4000

// :KUBE-MARK-MASQ - [0:0]
// Chain KUBE-MARK-MASQ (0 references)
// target     prot opt source               destination
// MARK       all  --  0.0.0.0/0            0.0.0.0/0            MARK or 0x4000
