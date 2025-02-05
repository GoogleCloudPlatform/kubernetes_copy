//go:build linux
// +build linux

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

package nftables

//
// NOTE: this needs to be tested in e2e since it uses nftables for everything.
//

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"golang.org/x/time/rate"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/conntrack"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	"k8s.io/kubernetes/pkg/proxy/metaproxier"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/async"
	utilkernel "k8s.io/kubernetes/pkg/util/kernel"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"
)

const (
	// Our nftables table. All of our chains/sets/maps are created inside this table,
	// so they don't need any "kube-" or "kube-proxy-" prefix of their own.
	kubeProxyTable = "kube-proxy"

	// base chains
	filterPreroutingChain     = "filter-prerouting"
	filterInputChain          = "filter-input"
	filterForwardChain        = "filter-forward"
	filterOutputChain         = "filter-output"
	filterOutputPostDNATChain = "filter-output-post-dnat"
	natPreroutingChain        = "nat-prerouting"
	natOutputChain            = "nat-output"
	natPostroutingChain       = "nat-postrouting"

	// service dispatch
	servicesChain       = "services"
	serviceIPsMap       = "service-ips"
	serviceNodePortsMap = "service-nodeports"

	// set of IPs that accept NodePort traffic
	nodePortIPsSet = "nodeport-ips"

	// set of active ClusterIPs.
	clusterIPsSet = "cluster-ips"

	// handling for services with no endpoints
	serviceEndpointsCheckChain  = "service-endpoints-check"
	nodePortEndpointsCheckChain = "nodeport-endpoints-check"
	noEndpointServicesMap       = "no-endpoint-services"
	noEndpointNodePortsMap      = "no-endpoint-nodeports"
	rejectChain                 = "reject-chain"

	// handling traffic to unallocated ClusterIPs and undefined ports of ClusterIPs
	clusterIPsCheckChain = "cluster-ips-check"

	// LoadBalancerSourceRanges handling
	firewallIPsMap     = "firewall-ips"
	firewallCheckChain = "firewall-check"

	// masquerading
	markMasqChain     = "mark-for-masquerade"
	masqueradingChain = "masquerading"
)

// NewDualStackProxier creates a MetaProxier instance, with IPv4 and IPv6 proxies.
func NewDualStackProxier(
	ctx context.Context,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	masqueradeAll bool,
	masqueradeBit int,
	localDetectors map[v1.IPFamily]proxyutil.LocalTrafficDetector,
	hostname string,
	nodeIPs map[v1.IPFamily]net.IP,
	recorder events.EventRecorder,
	healthzServer *healthcheck.ProxyHealthServer,
	nodePortAddresses []string,
	initOnly bool,
) (proxy.Provider, error) {
	// Create an ipv4 instance of the single-stack proxier
	ipv4Proxier, err := NewProxier(ctx, v1.IPv4Protocol,
		syncPeriod, minSyncPeriod, masqueradeAll, masqueradeBit,
		localDetectors[v1.IPv4Protocol], hostname, nodeIPs[v1.IPv4Protocol],
		recorder, healthzServer, nodePortAddresses, initOnly)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv4 proxier: %v", err)
	}

	ipv6Proxier, err := NewProxier(ctx, v1.IPv6Protocol,
		syncPeriod, minSyncPeriod, masqueradeAll, masqueradeBit,
		localDetectors[v1.IPv6Protocol], hostname, nodeIPs[v1.IPv6Protocol],
		recorder, healthzServer, nodePortAddresses, initOnly)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv6 proxier: %v", err)
	}
	if initOnly {
		return nil, nil
	}
	return metaproxier.NewMetaProxier(ipv4Proxier, ipv6Proxier), nil
}

// Proxier is an nftables based proxy
type Proxier struct {
	// ipFamily defines the IP family which this proxier is tracking.
	ipFamily v1.IPFamily

	// endpointsChanges and serviceChanges contains all changes to endpoints and
	// services that happened since nftables was synced. For a single object,
	// changes are accumulated, i.e. previous is state from before all of them,
	// current is state after applying all of those.
	endpointsChanges *proxy.EndpointsChangeTracker
	serviceChanges   *proxy.ServiceChangeTracker

	mu           sync.Mutex // protects the following fields
	svcPortMap   proxy.ServicePortMap
	endpointsMap proxy.EndpointsMap
	nodeLabels   map[string]string
	// endpointSlicesSynced, and servicesSynced are set to true
	// when corresponding objects are synced after startup. This is used to avoid
	// updating nftables with some partial data after kube-proxy restart.
	endpointSlicesSynced bool
	servicesSynced       bool
	lastFullSync         time.Time
	needFullSync         bool
	initialized          int32
	syncRunner           *async.BoundedFrequencyRunner // governs calls to syncProxyRules
	syncPeriod           time.Duration
	flushed              bool

	// These are effectively const and do not need the mutex to be held.
	nftables       knftables.Interface
	masqueradeAll  bool
	masqueradeMark string
	conntrack      conntrack.Interface
	localDetector  proxyutil.LocalTrafficDetector
	hostname       string
	nodeIP         net.IP
	recorder       events.EventRecorder

	serviceHealthServer healthcheck.ServiceHealthServer
	healthzServer       *healthcheck.ProxyHealthServer

	// nodePortAddresses selects the interfaces where nodePort works.
	nodePortAddresses *proxyutil.NodePortAddresses
	// networkInterfacer defines an interface for several net library functions.
	// Inject for test purpose.
	networkInterfacer proxyutil.NetworkInterfacer

	// staleChains contains information about chains to be deleted later
	staleChains map[string]time.Time

	// serviceCIDRs is a comma separated list of ServiceCIDRs belonging to the IPFamily
	// which proxier is operating on, can be directly consumed by knftables.
	serviceCIDRs string

	logger         klog.Logger
	logRateLimiter *rate.Limiter

	clusterIPs          *nftElementStorage
	serviceIPs          *nftElementStorage
	firewallIPs         *nftElementStorage
	noEndpointServices  *nftElementStorage
	noEndpointNodePorts *nftElementStorage
	serviceNodePorts    *nftElementStorage
}

// Proxier implements proxy.Provider
var _ proxy.Provider = &Proxier{}

// NewProxier returns a new nftables Proxier. Once a proxier is created, it will keep
// nftables up to date in the background and will not terminate if a particular nftables
// call fails.
func NewProxier(ctx context.Context,
	ipFamily v1.IPFamily,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	masqueradeAll bool,
	masqueradeBit int,
	localDetector proxyutil.LocalTrafficDetector,
	hostname string,
	nodeIP net.IP,
	recorder events.EventRecorder,
	healthzServer *healthcheck.ProxyHealthServer,
	nodePortAddressStrings []string,
	initOnly bool,
) (*Proxier, error) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "ipFamily", ipFamily)

	nft, err := getNFTablesInterface(ipFamily)
	if err != nil {
		return nil, err
	}

	if initOnly {
		logger.Info("System initialized and --init-only specified")
		return nil, nil
	}

	// Generate the masquerade mark to use for SNAT rules.
	masqueradeValue := 1 << uint(masqueradeBit)
	masqueradeMark := fmt.Sprintf("%#08x", masqueradeValue)
	logger.V(2).Info("Using nftables mark for masquerade", "mark", masqueradeMark)

	nodePortAddresses := proxyutil.NewNodePortAddresses(ipFamily, nodePortAddressStrings)

	serviceHealthServer := healthcheck.NewServiceHealthServer(hostname, recorder, nodePortAddresses, healthzServer)

	proxier := &Proxier{
		ipFamily:            ipFamily,
		svcPortMap:          make(proxy.ServicePortMap),
		serviceChanges:      proxy.NewServiceChangeTracker(ipFamily, newServiceInfo, nil),
		endpointsMap:        make(proxy.EndpointsMap),
		endpointsChanges:    proxy.NewEndpointsChangeTracker(ipFamily, hostname, newEndpointInfo, nil),
		needFullSync:        true,
		syncPeriod:          syncPeriod,
		nftables:            nft,
		masqueradeAll:       masqueradeAll,
		masqueradeMark:      masqueradeMark,
		conntrack:           conntrack.New(),
		localDetector:       localDetector,
		hostname:            hostname,
		nodeIP:              nodeIP,
		recorder:            recorder,
		serviceHealthServer: serviceHealthServer,
		healthzServer:       healthzServer,
		nodePortAddresses:   nodePortAddresses,
		networkInterfacer:   proxyutil.RealNetwork{},
		staleChains:         make(map[string]time.Time),
		logger:              logger,
		logRateLimiter:      rate.NewLimiter(rate.Every(24*time.Hour), 1),
		clusterIPs:          newNFTElementStorage("set", clusterIPsSet),
		serviceIPs:          newNFTElementStorage("map", serviceIPsMap),
		firewallIPs:         newNFTElementStorage("map", firewallIPsMap),
		noEndpointServices:  newNFTElementStorage("map", noEndpointServicesMap),
		noEndpointNodePorts: newNFTElementStorage("map", noEndpointNodePortsMap),
		serviceNodePorts:    newNFTElementStorage("map", serviceNodePortsMap),
	}

	burstSyncs := 2
	logger.V(2).Info("NFTables sync params", "minSyncPeriod", minSyncPeriod, "syncPeriod", syncPeriod, "burstSyncs", burstSyncs)
	// We need to pass *some* maxInterval to NewBoundedFrequencyRunner. time.Hour is arbitrary.
	proxier.syncRunner = async.NewBoundedFrequencyRunner("sync-runner", proxier.syncProxyRules, minSyncPeriod, proxyutil.FullSyncPeriod, burstSyncs)

	return proxier, nil
}

// Create a knftables.Interface and check if we can use the nftables proxy mode on this host.
func getNFTablesInterface(ipFamily v1.IPFamily) (knftables.Interface, error) {
	var nftablesFamily knftables.Family
	if ipFamily == v1.IPv4Protocol {
		nftablesFamily = knftables.IPv4Family
	} else {
		nftablesFamily = knftables.IPv6Family
	}

	// We require (or rather, knftables.New does) that the nft binary be version 1.0.1
	// or later, because versions before that would always attempt to parse the entire
	// nft ruleset at startup, even if you were only operating on a single table.
	// That's bad, because in some cases, new versions of nft have added new rule
	// types in ways that triggered bugs in older versions of nft, causing them to
	// crash. Thus, if kube-proxy used nft < 1.0.1, it could potentially get locked
	// out of its rules because of something some other component had done in a
	// completely different table.
	nft, err := knftables.New(nftablesFamily, kubeProxyTable)
	if err != nil {
		return nil, err
	}

	// Likewise, we want to ensure that the host filesystem has nft >= 1.0.1, so that
	// it's not possible that *our* rules break *the system's* nft. (In particular, we
	// know that if kube-proxy uses nft >= 1.0.3 and the system has nft <= 0.9.8, that
	// the system nft will become completely unusable.) Unfortunately, we can't easily
	// figure out the version of nft installed on the host filesystem, so instead, we
	// check the kernel version, under the assumption that the distro will have an nft
	// binary that supports the same features as its kernel does, and so kernel 5.13
	// or later implies nft 1.0.1 or later. https://issues.k8s.io/122743
	//
	// However, we allow the user to bypass this check by setting
	// `KUBE_PROXY_NFTABLES_SKIP_KERNEL_VERSION_CHECK` to anything non-empty.
	if os.Getenv("KUBE_PROXY_NFTABLES_SKIP_KERNEL_VERSION_CHECK") != "" {
		kernelVersion, err := utilkernel.GetVersion()
		if err != nil {
			return nil, fmt.Errorf("could not check kernel version: %w", err)
		}
		if kernelVersion.LessThan(version.MustParseGeneric(utilkernel.NFTablesKubeProxyKernelVersion)) {
			return nil, fmt.Errorf("kube-proxy in nftables mode requires kernel %s or later", utilkernel.NFTablesKubeProxyKernelVersion)
		}
	}

	return nft, nil
}

// internal struct for string service information
type servicePortInfo struct {
	*proxy.BaseServicePortInfo
	// The following fields are computed and stored for performance reasons.
	nameString             string
	clusterPolicyChainName string
	localPolicyChainName   string
	externalChainName      string
	firewallChainName      string
}

// returns a new proxy.ServicePort which abstracts a serviceInfo
func newServiceInfo(port *v1.ServicePort, service *v1.Service, bsvcPortInfo *proxy.BaseServicePortInfo) proxy.ServicePort {
	svcPort := &servicePortInfo{BaseServicePortInfo: bsvcPortInfo}

	// Store the following for performance reasons.
	svcName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	svcPortName := proxy.ServicePortName{NamespacedName: svcName, Port: port.Name}
	svcPort.nameString = svcPortName.String()

	chainNameBase := servicePortChainNameBase(&svcPortName, strings.ToLower(string(svcPort.Protocol())))
	svcPort.clusterPolicyChainName = servicePortPolicyClusterChainNamePrefix + chainNameBase
	svcPort.localPolicyChainName = servicePortPolicyLocalChainNamePrefix + chainNameBase
	svcPort.externalChainName = serviceExternalChainNamePrefix + chainNameBase
	svcPort.firewallChainName = servicePortFirewallChainNamePrefix + chainNameBase

	return svcPort
}

// internal struct for endpoints information
type endpointInfo struct {
	*proxy.BaseEndpointInfo

	chainName       string
	affinitySetName string
}

// returns a new proxy.Endpoint which abstracts a endpointInfo
func newEndpointInfo(baseInfo *proxy.BaseEndpointInfo, svcPortName *proxy.ServicePortName) proxy.Endpoint {
	chainNameBase := servicePortEndpointChainNameBase(svcPortName, strings.ToLower(string(svcPortName.Protocol)), baseInfo.String())
	return &endpointInfo{
		BaseEndpointInfo: baseInfo,
		chainName:        servicePortEndpointChainNamePrefix + chainNameBase,
		affinitySetName:  servicePortEndpointAffinityNamePrefix + chainNameBase,
	}
}

// nftablesBaseChains lists our "base chains"; those that are directly connected to the
// netfilter hooks (e.g., "postrouting", "input", etc.), as opposed to "regular" chains,
// which are only run when a rule jumps to them. See
// https://wiki.nftables.org/wiki-nftables/index.php/Configuring_chains.
//
// These are set up from setupNFTables() and then not directly referenced by
// syncProxyRules().
//
// All of our base chains have names that are just "${type}-${hook}". e.g., "nat-prerouting".
type nftablesBaseChain struct {
	name      string
	chainType knftables.BaseChainType
	hook      knftables.BaseChainHook
	priority  knftables.BaseChainPriority
}

var nftablesBaseChains = []nftablesBaseChain{
	// We want our filtering rules to operate on pre-DNAT dest IPs, so our filter
	// chains have to run before DNAT.
	{filterPreroutingChain, knftables.FilterType, knftables.PreroutingHook, knftables.DNATPriority + "-10"},
	{filterInputChain, knftables.FilterType, knftables.InputHook, knftables.DNATPriority + "-10"},
	{filterForwardChain, knftables.FilterType, knftables.ForwardHook, knftables.DNATPriority + "-10"},
	{filterOutputChain, knftables.FilterType, knftables.OutputHook, knftables.DNATPriority + "-10"},
	{filterOutputPostDNATChain, knftables.FilterType, knftables.OutputHook, knftables.DNATPriority + "+10"},
	{natPreroutingChain, knftables.NATType, knftables.PreroutingHook, knftables.DNATPriority},
	{natOutputChain, knftables.NATType, knftables.OutputHook, knftables.DNATPriority},
	{natPostroutingChain, knftables.NATType, knftables.PostroutingHook, knftables.SNATPriority},
}

// nftablesJumpChains lists our top-level "regular chains" that are jumped to directly
// from one of the base chains. These are set up from setupNFTables(), and some of them
// are also referenced in syncProxyRules().
type nftablesJumpChain struct {
	dstChain  string
	srcChain  string
	extraArgs string
}

var nftablesJumpChains = []nftablesJumpChain{
	// We can't jump to endpointsCheckChain from filter-prerouting like
	// firewallCheckChain because reject action is only valid in chains using the
	// input, forward or output hooks with kernels before 5.9.
	{nodePortEndpointsCheckChain, filterInputChain, "ct state new"},
	{serviceEndpointsCheckChain, filterInputChain, "ct state new"},
	{serviceEndpointsCheckChain, filterForwardChain, "ct state new"},
	{serviceEndpointsCheckChain, filterOutputChain, "ct state new"},

	{firewallCheckChain, filterPreroutingChain, "ct state new"},
	{firewallCheckChain, filterOutputChain, "ct state new"},

	{servicesChain, natOutputChain, ""},
	{servicesChain, natPreroutingChain, ""},
	{masqueradingChain, natPostroutingChain, ""},

	{clusterIPsCheckChain, filterForwardChain, "ct state new"},
	{clusterIPsCheckChain, filterOutputPostDNATChain, "ct state new"},
}

// ensureChain adds commands to tx to ensure that chain exists and doesn't contain
// anything from before this transaction (using createdChains to ensure that we don't
// Flush a chain more than once and lose *new* rules as well.)
// If skipCreation is true, chain will not be added to the transaction, but will be added to the createdChains
// for proper cleanup in the end of the sync iteration.
func ensureChain(chain string, tx *knftables.Transaction, createdChains sets.Set[string], skipCreation bool) {
	if createdChains.Has(chain) {
		return
	}
	createdChains.Insert(chain)
	if skipCreation {
		return
	}
	tx.Add(&knftables.Chain{
		Name: chain,
	})
	tx.Flush(&knftables.Chain{
		Name: chain,
	})
}

func (proxier *Proxier) setupNFTables(tx *knftables.Transaction) {
	ipX := "ip"
	ipvX_addr := "ipv4_addr" //nolint:stylecheck // var name intentionally resembles value
	noLocalhost := "ip daddr != 127.0.0.0/8"
	if proxier.ipFamily == v1.IPv6Protocol {
		ipX = "ip6"
		ipvX_addr = "ipv6_addr"
		noLocalhost = "ip6 daddr != ::1"
	}

	tx.Add(&knftables.Table{
		Comment: ptr.To("rules for kube-proxy"),
	})

	// Do an extra "add+delete" once to ensure all previous base chains in the table
	// will be recreated. Otherwise, altering properties (e.g. priority) of these
	// chains would fail the transaction.
	if !proxier.flushed {
		for _, bc := range nftablesBaseChains {
			chain := &knftables.Chain{
				Name: bc.name,
			}
			tx.Add(chain)
			tx.Delete(chain)
		}
		proxier.flushed = true
	}

	// Create and flush base chains
	for _, bc := range nftablesBaseChains {
		chain := &knftables.Chain{
			Name:     bc.name,
			Type:     ptr.To(bc.chainType),
			Hook:     ptr.To(bc.hook),
			Priority: ptr.To(bc.priority),
		}
		tx.Add(chain)
		tx.Flush(chain)
	}

	// Create and flush ordinary chains and add rules jumping to them
	createdChains := sets.New[string]()
	for _, c := range nftablesJumpChains {
		ensureChain(c.dstChain, tx, createdChains, false)
		tx.Add(&knftables.Rule{
			Chain: c.srcChain,
			Rule: knftables.Concat(
				c.extraArgs,
				"jump", c.dstChain,
			),
		})
	}

	// Ensure all of our other "top-level" chains exist
	for _, chain := range []string{servicesChain, clusterIPsCheckChain, masqueradingChain, markMasqChain} {
		ensureChain(chain, tx, createdChains, false)
	}

	// Add the rules in the mark-for-masquerade and masquerading chains
	tx.Add(&knftables.Rule{
		Chain: markMasqChain,
		Rule: knftables.Concat(
			"mark", "set", "mark", "or", proxier.masqueradeMark,
		),
	})

	tx.Add(&knftables.Rule{
		Chain: masqueradingChain,
		Rule: knftables.Concat(
			"mark", "and", proxier.masqueradeMark, "==", "0",
			"return",
		),
	})
	tx.Add(&knftables.Rule{
		Chain: masqueradingChain,
		Rule: knftables.Concat(
			"mark", "set", "mark", "xor", proxier.masqueradeMark,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: masqueradingChain,
		Rule:  "masquerade fully-random",
	})

	// add cluster-ips set.
	tx.Add(&knftables.Set{
		Name:    clusterIPsSet,
		Type:    ipvX_addr,
		Comment: ptr.To("Active ClusterIPs"),
	})

	// add named counter.
	tx.Add(&knftables.Counter{
		Name:    metrics.TrafficToInvalidServicePortsCounter,
		Comment: ptr.To("track packets sent to invalid ports of cluster ips"),
	})

	tx.Reset(&knftables.Counter{
		Name: metrics.TrafficToInvalidServicePortsCounter,
	})

	// reject traffic to invalid ports of ClusterIPs.
	tx.Add(&knftables.Rule{
		Chain: clusterIPsCheckChain,
		Rule: knftables.Concat(
			ipX, "daddr", "@", clusterIPsSet, "counter", "name", metrics.TrafficToInvalidServicePortsCounter, "reject",
		),
		Comment: ptr.To("Reject traffic to invalid ports of ClusterIPs"),
	})

	// drop traffic to unallocated ClusterIPs.
	if len(proxier.serviceCIDRs) > 0 {
		tx.Add(&knftables.Rule{
			Chain: clusterIPsCheckChain,
			Rule: knftables.Concat(
				ipX, "daddr", "{", proxier.serviceCIDRs, "}",
				"drop",
			),
			Comment: ptr.To("Drop traffic to unallocated ClusterIPs"),
		})
	}

	// Fill in nodeport-ips set if needed (or delete it if not). (We do "add+delete"
	// rather than just "delete" when we want to ensure the set doesn't exist, because
	// doing just "delete" would return an error if the set didn't exist.)
	tx.Add(&knftables.Set{
		Name:    nodePortIPsSet,
		Type:    ipvX_addr,
		Comment: ptr.To("IPs that accept NodePort traffic"),
	})
	if proxier.nodePortAddresses.MatchAll() {
		tx.Delete(&knftables.Set{
			Name: nodePortIPsSet,
		})
	} else {
		tx.Flush(&knftables.Set{
			Name: nodePortIPsSet,
		})
		nodeIPs, err := proxier.nodePortAddresses.GetNodeIPs(proxier.networkInterfacer)
		if err != nil {
			proxier.logger.Error(err, "Failed to get node ip address matching nodeport cidrs, services with nodeport may not work as intended", "CIDRs", proxier.nodePortAddresses)
		}
		for _, ip := range nodeIPs {
			if ip.IsLoopback() {
				proxier.logger.Error(nil, "--nodeport-addresses includes localhost but localhost NodePorts are not supported", "address", ip.String())
				continue
			}
			tx.Add(&knftables.Element{
				Set: nodePortIPsSet,
				Key: []string{
					ip.String(),
				},
			})
		}
	}

	// Set up "no endpoints" drop/reject handling
	tx.Add(&knftables.Map{
		Name:    noEndpointServicesMap,
		Type:    ipvX_addr + " . inet_proto . inet_service : verdict",
		Comment: ptr.To("vmap to drop or reject packets to services with no endpoints"),
	})
	tx.Add(&knftables.Map{
		Name:    noEndpointNodePortsMap,
		Type:    "inet_proto . inet_service : verdict",
		Comment: ptr.To("vmap to drop or reject packets to service nodeports with no endpoints"),
	})

	tx.Add(&knftables.Chain{
		Name:    rejectChain,
		Comment: ptr.To("helper for @no-endpoint-services / @no-endpoint-nodeports"),
	})
	tx.Flush(&knftables.Chain{
		Name: rejectChain,
	})
	tx.Add(&knftables.Rule{
		Chain: rejectChain,
		Rule:  "reject",
	})

	tx.Add(&knftables.Rule{
		Chain: serviceEndpointsCheckChain,
		Rule: knftables.Concat(
			ipX, "daddr", ".", "meta l4proto", ".", "th dport",
			"vmap", "@", noEndpointServicesMap,
		),
	})

	if proxier.nodePortAddresses.MatchAll() {
		tx.Add(&knftables.Rule{
			Chain: nodePortEndpointsCheckChain,
			Rule: knftables.Concat(
				noLocalhost,
				"meta l4proto . th dport",
				"vmap", "@", noEndpointNodePortsMap,
			),
		})
	} else {
		tx.Add(&knftables.Rule{
			Chain: nodePortEndpointsCheckChain,
			Rule: knftables.Concat(
				ipX, "daddr", "@", nodePortIPsSet,
				"meta l4proto . th dport",
				"vmap", "@", noEndpointNodePortsMap,
			),
		})
	}

	// Set up LoadBalancerSourceRanges firewalling
	tx.Add(&knftables.Map{
		Name:    firewallIPsMap,
		Type:    ipvX_addr + " . inet_proto . inet_service : verdict",
		Comment: ptr.To("destinations that are subject to LoadBalancerSourceRanges"),
	})

	ensureChain(firewallCheckChain, tx, createdChains, false)
	tx.Add(&knftables.Rule{
		Chain: firewallCheckChain,
		Rule: knftables.Concat(
			ipX, "daddr", ".", "meta l4proto", ".", "th dport",
			"vmap", "@", firewallIPsMap,
		),
	})

	// Set up service dispatch
	tx.Add(&knftables.Map{
		Name:    serviceIPsMap,
		Type:    ipvX_addr + " . inet_proto . inet_service : verdict",
		Comment: ptr.To("ClusterIP, ExternalIP and LoadBalancer IP traffic"),
	})
	tx.Add(&knftables.Map{
		Name:    serviceNodePortsMap,
		Type:    "inet_proto . inet_service : verdict",
		Comment: ptr.To("NodePort traffic"),
	})
	tx.Add(&knftables.Rule{
		Chain: servicesChain,
		Rule: knftables.Concat(
			ipX, "daddr", ".", "meta l4proto", ".", "th dport",
			"vmap", "@", serviceIPsMap,
		),
	})
	if proxier.nodePortAddresses.MatchAll() {
		tx.Add(&knftables.Rule{
			Chain: servicesChain,
			Rule: knftables.Concat(
				"fib daddr type local",
				noLocalhost,
				"meta l4proto . th dport",
				"vmap", "@", serviceNodePortsMap,
			),
		})
	} else {
		tx.Add(&knftables.Rule{
			Chain: servicesChain,
			Rule: knftables.Concat(
				ipX, "daddr @nodeport-ips",
				"meta l4proto . th dport",
				"vmap", "@", serviceNodePortsMap,
			),
		})
	}

	// flush containers
	proxier.clusterIPs.reset(tx)
	proxier.serviceIPs.reset(tx)
	proxier.firewallIPs.reset(tx)
	proxier.noEndpointServices.reset(tx)
	proxier.noEndpointNodePorts.reset(tx)
	proxier.serviceNodePorts.reset(tx)
}

// CleanupLeftovers removes all nftables rules and chains created by the Proxier
// It returns true if an error was encountered. Errors are logged.
func CleanupLeftovers(ctx context.Context) bool {
	logger := klog.FromContext(ctx)
	var encounteredError bool

	for _, family := range []knftables.Family{knftables.IPv4Family, knftables.IPv6Family} {
		nft, err := knftables.New(family, kubeProxyTable)
		if err == nil {
			tx := nft.NewTransaction()
			tx.Delete(&knftables.Table{})
			err = nft.Run(ctx, tx)
		}
		if err != nil && !knftables.IsNotFound(err) {
			logger.Error(err, "Error cleaning up nftables rules")
			encounteredError = true
		}
	}

	return encounteredError
}

// Sync is called to synchronize the proxier state to nftables as soon as possible.
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

// OnServiceAdd is called whenever creation of new service object
// is observed.
func (proxier *Proxier) OnServiceAdd(service *v1.Service) {
	proxier.OnServiceUpdate(nil, service)
}

// OnServiceUpdate is called whenever modification of an existing
// service object is observed.
func (proxier *Proxier) OnServiceUpdate(oldService, service *v1.Service) {
	if proxier.serviceChanges.Update(oldService, service) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnServiceDelete is called whenever deletion of an existing service
// object is observed.
func (proxier *Proxier) OnServiceDelete(service *v1.Service) {
	proxier.OnServiceUpdate(service, nil)

}

// OnServiceSynced is called once all the initial event handlers were
// called and the state is fully propagated to local cache.
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
		proxier.logger.Error(nil, "Received a watch event for a node that doesn't match the current node",
			"eventNode", node.Name, "currentNode", proxier.hostname)
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
	proxier.needFullSync = true
	proxier.mu.Unlock()
	proxier.logger.V(4).Info("Updated proxier node labels", "labels", node.Labels)

	proxier.Sync()
}

// OnNodeUpdate is called whenever modification of an existing
// node object is observed.
func (proxier *Proxier) OnNodeUpdate(oldNode, node *v1.Node) {
	if node.Name != proxier.hostname {
		proxier.logger.Error(nil, "Received a watch event for a node that doesn't match the current node",
			"eventNode", node.Name, "currentNode", proxier.hostname)
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
	proxier.needFullSync = true
	proxier.mu.Unlock()
	proxier.logger.V(4).Info("Updated proxier node labels", "labels", node.Labels)

	proxier.Sync()
}

// OnNodeDelete is called whenever deletion of an existing node
// object is observed.
func (proxier *Proxier) OnNodeDelete(node *v1.Node) {
	if node.Name != proxier.hostname {
		proxier.logger.Error(nil, "Received a watch event for a node that doesn't match the current node",
			"eventNode", node.Name, "currentNode", proxier.hostname)
		return
	}

	proxier.mu.Lock()
	proxier.nodeLabels = nil
	proxier.needFullSync = true
	proxier.mu.Unlock()

	proxier.Sync()
}

// OnNodeSynced is called once all the initial event handlers were
// called and the state is fully propagated to local cache.
func (proxier *Proxier) OnNodeSynced() {
}

// OnServiceCIDRsChanged is called whenever a change is observed
// in any of the ServiceCIDRs, and provides complete list of service cidrs.
func (proxier *Proxier) OnServiceCIDRsChanged(cidrs []string) {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	cidrsForProxier := make([]string, 0)
	for _, cidr := range cidrs {
		isIPv4CIDR := netutils.IsIPv4CIDRString(cidr)
		if proxier.ipFamily == v1.IPv4Protocol && isIPv4CIDR {
			cidrsForProxier = append(cidrsForProxier, cidr)
		}

		if proxier.ipFamily == v1.IPv6Protocol && !isIPv4CIDR {
			cidrsForProxier = append(cidrsForProxier, cidr)
		}
	}
	proxier.serviceCIDRs = strings.Join(cidrsForProxier, ",")
}

const (
	// Maximum length for one of our chain name prefixes, including the trailing
	// hyphen.
	chainNamePrefixLengthMax = 16

	// Maximum length of the string returned from servicePortChainNameBase or
	// servicePortEndpointChainNameBase.
	chainNameBaseLengthMax = knftables.NameLengthMax - chainNamePrefixLengthMax
)

const (
	servicePortPolicyClusterChainNamePrefix = "service-"
	servicePortPolicyLocalChainNamePrefix   = "local-"
	serviceExternalChainNamePrefix          = "external-"
	servicePortEndpointChainNamePrefix      = "endpoint-"
	servicePortEndpointAffinityNamePrefix   = "affinity-"
	servicePortFirewallChainNamePrefix      = "firewall-"
)

// hashAndTruncate prefixes name with a hash of itself and then truncates to
// chainNameBaseLengthMax. The hash ensures that (a) the name is still unique if we have
// to truncate the end, and (b) it's visually distinguishable from other chains that would
// otherwise have nearly identical names (e.g., different endpoint chains for a given
// service that differ in only a single digit).
func hashAndTruncate(name string) string {
	hash := sha256.Sum256([]byte(name))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	name = encoded[:8] + "-" + name
	if len(name) > chainNameBaseLengthMax {
		name = name[:chainNameBaseLengthMax-3] + "..."
	}
	return name
}

// servicePortChainNameBase returns the base name for a chain for the given ServicePort.
// This is something like "HASH-namespace/serviceName/protocol/portName", e.g,
// "ULMVA6XW-ns1/svc1/tcp/p80".
func servicePortChainNameBase(servicePortName *proxy.ServicePortName, protocol string) string {
	// nftables chains can contain the characters [A-Za-z0-9_./-] (but must start with
	// a letter, underscore, or dot).
	//
	// Namespace, Service, and Port names can contain [a-z0-9-] (with some additional
	// restrictions that aren't relevant here).
	//
	// Protocol is /(tcp|udp|sctp)/.
	//
	// Thus, we can safely use all Namespace names, Service names, protocol values,
	// and Port names directly in nftables chain names (though note that this assumes
	// that the chain name won't *start* with any of those strings, since that might
	// be illegal). We use "/" to separate the parts of the name, which is one of the
	// two characters allowed in a chain name that isn't allowed in our input strings.

	name := fmt.Sprintf("%s/%s/%s/%s",
		servicePortName.NamespacedName.Namespace,
		servicePortName.NamespacedName.Name,
		protocol,
		servicePortName.Port,
	)

	// The namespace, service, and port name can each be up to 63 characters, protocol
	// can be up to 4, plus 8 for the hash and 4 additional punctuation characters.
	// That's a total of 205, which is less than chainNameBaseLengthMax (240). So this
	// will never actually return a truncated name.
	return hashAndTruncate(name)
}

// servicePortEndpointChainNameBase returns the suffix for chain names for the given
// endpoint. This is something like
// "HASH-namespace/serviceName/protocol/portName__endpointIP/endpointport", e.g.,
// "5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80".
func servicePortEndpointChainNameBase(servicePortName *proxy.ServicePortName, protocol, endpoint string) string {
	// As above in servicePortChainNameBase: Namespace, Service, Port, Protocol, and
	// EndpointPort are all safe to copy into the chain name directly. But if
	// EndpointIP is IPv6 then it will contain colons, which aren't allowed in a chain
	// name. IPv6 IPs are also quite long, but we can't safely truncate them (e.g. to
	// only the final segment) because (especially for manually-created external
	// endpoints), we can't know for sure that any part of them is redundant.

	endpointIP, endpointPort, _ := net.SplitHostPort(endpoint)
	if strings.Contains(endpointIP, ":") {
		endpointIP = strings.ReplaceAll(endpointIP, ":", ".")
	}

	// As above, we use "/" to separate parts of the name, and "__" to separate the
	// "service" part from the "endpoint" part.
	name := fmt.Sprintf("%s/%s/%s/%s__%s/%s",
		servicePortName.NamespacedName.Namespace,
		servicePortName.NamespacedName.Name,
		protocol,
		servicePortName.Port,
		endpointIP,
		endpointPort,
	)

	// The part of name before the "__" can be up to 205 characters (as with
	// servicePortChainNameBase above). An IPv6 address can be up to 39 characters, and
	// a port can be up to 5 digits, plus 3 punctuation characters gives a max total
	// length of 252, well over chainNameBaseLengthMax (240), so truncation is
	// theoretically possible (though incredibly unlikely).
	return hashAndTruncate(name)
}

func isServiceChainName(chainString string) bool {
	// The chains returned from servicePortChainNameBase and
	// servicePortEndpointChainNameBase will always have at least one "/" in them.
	// Since none of our "stock" chain names use slashes, we can distinguish them this
	// way.
	return strings.Contains(chainString, "/")
}

func isAffinitySetName(set string) bool {
	return strings.HasPrefix(set, servicePortEndpointAffinityNamePrefix)
}

// nftElementStorage is an internal representation of nftables map or set.
type nftElementStorage struct {
	elements      map[string]string
	leftoverKeys  sets.Set[string]
	containerType string
	containerName string
}

// joinNFTSlice converts nft element key or value (type []string) to string to store in the nftElementStorage.
// The separator is the same as the one used by nft commands, so we know that the parsing is going to be unambiguous.
func joinNFTSlice(k []string) string {
	return strings.Join(k, " . ")
}

// splitNFTSlice converts nftElementStorage key or value string representation back to slice.
func splitNFTSlice(k string) []string {
	return strings.Split(k, " . ")
}

// newNFTElementStorage creates an empty nftElementStorage.
// nftElementStorage.reset() must be called before the first usage.
func newNFTElementStorage(containerType, containerName string) *nftElementStorage {
	c := &nftElementStorage{
		elements:      make(map[string]string),
		leftoverKeys:  sets.New[string](),
		containerType: containerType,
		containerName: containerName,
	}
	return c
}

// reset clears the internal state and flushes the nftables map/set.
func (s *nftElementStorage) reset(tx *knftables.Transaction) {
	clear(s.elements)
	if s.containerType == "set" {
		tx.Flush(&knftables.Set{
			Name: s.containerName,
		})
	} else {
		tx.Flush(&knftables.Map{
			Name: s.containerName,
		})
	}
	s.resetLeftoverKeys()
}

// resetLeftoverKeys is only called internally by nftElementStorage methods.
func (s *nftElementStorage) resetLeftoverKeys() {
	clear(s.leftoverKeys)
	for key := range s.elements {
		s.leftoverKeys.Insert(key)
	}
}

// ensureElem adds elem to the transaction if elem is not present in the container, and updates internal
// leftoverKeys set to track unused elements.
func (s *nftElementStorage) ensureElem(tx *knftables.Transaction, elem *knftables.Element) {
	newKey := joinNFTSlice(elem.Key)
	newValue := joinNFTSlice(elem.Value)
	existingValue, exists := s.elements[newKey]
	if exists {
		if existingValue != newValue {
			// value is different, delete and re-add
			tx.Delete(elem)
			tx.Add(elem)
			s.elements[newKey] = newValue
		}
		delete(s.leftoverKeys, newKey)
	} else {
		tx.Add(elem)
		s.elements[newKey] = newValue
	}
}

func (s *nftElementStorage) cleanupLeftoverKeys(tx *knftables.Transaction) {
	for key := range s.leftoverKeys {
		e := &knftables.Element{
			Key: splitNFTSlice(key),
		}
		if s.containerType == "set" {
			e.Set = s.containerName
		} else {
			e.Map = s.containerName
		}
		tx.Delete(e)
		delete(s.elements, key)
	}
	s.resetLeftoverKeys()
}

// logFailure logs the transaction and the full table with a rate limit.
func (proxier *Proxier) logFailure(tx *knftables.Transaction) {
	if klogV4 := klog.V(4); klogV4.Enabled() && proxier.logRateLimiter.Allow() {
		klogV4.InfoS("Failed transaction", "transaction", tx.String())
		// knftables doesn't supporting listing the full table yet, this is a workaround.
		cmd := exec.Command("nft", "list", "table", kubeProxyTable)
		out, err := cmd.Output()
		if err != nil {
			klogV4.InfoS("Listing full table failed", "error", err)
		} else {
			klogV4.InfoS("Listing full table", "result", string(out))
		}
	}
}

// This is where all of the nftables calls happen.
// This assumes proxier.mu is NOT held
func (proxier *Proxier) syncProxyRules() {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	// don't sync rules till we've received services and endpoints
	if !proxier.isInitialized() {
		proxier.logger.V(2).Info("Not syncing nftables until Services and Endpoints have been received from master")
		return
	}

	// Keep track of how long syncs take.
	start := time.Now()

	//
	// Below this point we will not return until we try to write the nftables rules.
	//

	doFullSync := proxier.needFullSync || (time.Since(proxier.lastFullSync) > proxyutil.FullSyncPeriod)

	defer func() {
		metrics.SyncProxyRulesLatency.WithLabelValues(string(proxier.ipFamily)).Observe(metrics.SinceInSeconds(start))
		if !doFullSync {
			metrics.SyncPartialProxyRulesLatency.WithLabelValues(string(proxier.ipFamily)).Observe(metrics.SinceInSeconds(start))
		} else {
			metrics.SyncFullProxyRulesLatency.WithLabelValues(string(proxier.ipFamily)).Observe(metrics.SinceInSeconds(start))
		}
		proxier.logger.V(2).Info("SyncProxyRules complete", "elapsed", time.Since(start))
	}()

	serviceUpdateResult := proxier.svcPortMap.Update(proxier.serviceChanges)
	endpointUpdateResult := proxier.endpointsMap.Update(proxier.endpointsChanges)

	proxier.logger.V(2).Info("Syncing nftables rules", "fullSync", doFullSync)

	success := false
	defer func() {
		if !success {
			proxier.logger.Info("Sync failed", "retryingTime", proxier.syncPeriod)
			proxier.syncRunner.RetryAfter(proxier.syncPeriod)
			// proxier.serviceChanges and proxier.endpointChanges have already
			// been flushed, so we've lost the state needed to be able to do
			// a partial sync.
			proxier.needFullSync = true
		} else if doFullSync {
			proxier.lastFullSync = time.Now()
		}
	}()

	// If there are sufficiently-stale chains left over from previous transactions,
	// try to delete them now.
	if len(proxier.staleChains) > 0 {
		oneSecondAgo := start.Add(-time.Second)
		tx := proxier.nftables.NewTransaction()
		deleted := 0
		for chain, modtime := range proxier.staleChains {
			if modtime.Before(oneSecondAgo) {
				tx.Delete(&knftables.Chain{
					Name: chain,
				})
				delete(proxier.staleChains, chain)
				deleted++
			}
		}
		if deleted > 0 {
			proxier.logger.Info("Deleting stale nftables chains", "numChains", deleted)
			err := proxier.nftables.Run(context.TODO(), tx)
			if err != nil {
				// We already deleted the entries from staleChains, but if
				// the chains still exist, they'll just get added back
				// (with a later timestamp) at the end of the sync.
				proxier.logger.Error(err, "Unable to delete stale chains; will retry later")
				metrics.NFTablesCleanupFailuresTotal.WithLabelValues(string(proxier.ipFamily)).Inc()
				doFullSync = true

				// Log failed transaction and list full kube-proxy table.
				proxier.logFailure(tx)
			}
		}
	}

	// Now start the actual syncing transaction
	tx := proxier.nftables.NewTransaction()
	if doFullSync {
		proxier.setupNFTables(tx)
	}

	// We need to use, eg, "ip daddr" for IPv4 but "ip6 daddr" for IPv6
	ipX := "ip"
	ipvX_addr := "ipv4_addr" //nolint:stylecheck // var name intentionally resembles value
	if proxier.ipFamily == v1.IPv6Protocol {
		ipX = "ip6"
		ipvX_addr = "ipv6_addr"
	}

	// Accumulate service/endpoint chains and affinity sets to keep.
	activeChains := sets.New[string]()
	activeAffinitySets := sets.New[string]()

	// Compute total number of endpoint chains across all services
	// to get a sense of how big the cluster is.
	totalEndpoints := 0
	for svcName := range proxier.svcPortMap {
		totalEndpoints += len(proxier.endpointsMap[svcName])
	}

	// These two variables are used to publish the sync_proxy_rules_no_endpoints_total
	// metric.
	serviceNoLocalEndpointsTotalInternal := 0
	serviceNoLocalEndpointsTotalExternal := 0

	// Build rules for each service-port.
	for svcName, svc := range proxier.svcPortMap {
		svcInfo, ok := svc.(*servicePortInfo)
		if !ok {
			proxier.logger.Error(nil, "Failed to cast serviceInfo", "serviceName", svcName)
			continue
		}

		protocol := strings.ToLower(string(svcInfo.Protocol()))
		svcPortNameString := svcInfo.nameString

		// Figure out the endpoints for Cluster and Local traffic policy.
		// allLocallyReachableEndpoints is the set of all endpoints that can be routed to
		// from this node, given the service's traffic policies. hasEndpoints is true
		// if the service has any usable endpoints on any node, not just this one.
		allEndpoints := proxier.endpointsMap[svcName]
		clusterEndpoints, localEndpoints, allLocallyReachableEndpoints, hasEndpoints := proxy.CategorizeEndpoints(allEndpoints, svcInfo, proxier.nodeLabels)

		// skipServiceUpdate is used for all service-related chains and their elements.
		// If no changes were done to the service or its endpoints, these objects may be skipped.
		skipServiceUpdate := !doFullSync &&
			!serviceUpdateResult.UpdatedServices.Has(svcName.NamespacedName) &&
			!endpointUpdateResult.UpdatedServices.Has(svcName.NamespacedName)

		// Note the endpoint chains that will be used
		for _, ep := range allLocallyReachableEndpoints {
			if epInfo, ok := ep.(*endpointInfo); ok {
				ensureChain(epInfo.chainName, tx, activeChains, skipServiceUpdate)
				// Note the affinity sets that will be used
				if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
					activeAffinitySets.Insert(epInfo.affinitySetName)
				}
			}
		}

		// clusterPolicyChain contains the endpoints used with "Cluster" traffic policy
		clusterPolicyChain := svcInfo.clusterPolicyChainName
		usesClusterPolicyChain := len(clusterEndpoints) > 0 && svcInfo.UsesClusterEndpoints()
		if usesClusterPolicyChain {
			ensureChain(clusterPolicyChain, tx, activeChains, skipServiceUpdate)
		}

		// localPolicyChain contains the endpoints used with "Local" traffic policy
		localPolicyChain := svcInfo.localPolicyChainName
		usesLocalPolicyChain := len(localEndpoints) > 0 && svcInfo.UsesLocalEndpoints()
		if usesLocalPolicyChain {
			ensureChain(localPolicyChain, tx, activeChains, skipServiceUpdate)
		}

		// internalPolicyChain is the chain containing the endpoints for
		// "internal" (ClusterIP) traffic. internalTrafficChain is the chain that
		// internal traffic is routed to (which is always the same as
		// internalPolicyChain). hasInternalEndpoints is true if we should
		// generate rules pointing to internalTrafficChain, or false if there are
		// no available internal endpoints.
		internalPolicyChain := clusterPolicyChain
		hasInternalEndpoints := hasEndpoints
		if svcInfo.InternalPolicyLocal() {
			internalPolicyChain = localPolicyChain
			if len(localEndpoints) == 0 {
				hasInternalEndpoints = false
			}
		}
		internalTrafficChain := internalPolicyChain

		// Similarly, externalPolicyChain is the chain containing the endpoints
		// for "external" (NodePort, LoadBalancer, and ExternalIP) traffic.
		// externalTrafficChain is the chain that external traffic is routed to
		// (which is always the service's "EXT" chain). hasExternalEndpoints is
		// true if there are endpoints that will be reached by external traffic.
		// (But we may still have to generate externalTrafficChain even if there
		// are no external endpoints, to ensure that the short-circuit rules for
		// local traffic are set up.)
		externalPolicyChain := clusterPolicyChain
		hasExternalEndpoints := hasEndpoints
		if svcInfo.ExternalPolicyLocal() {
			externalPolicyChain = localPolicyChain
			if len(localEndpoints) == 0 {
				hasExternalEndpoints = false
			}
		}
		externalTrafficChain := svcInfo.externalChainName // eventually jumps to externalPolicyChain

		// usesExternalTrafficChain is based on hasEndpoints, not hasExternalEndpoints,
		// because we need the local-traffic-short-circuiting rules even when there
		// are no externally-usable endpoints.
		usesExternalTrafficChain := hasEndpoints && svcInfo.ExternallyAccessible()
		if usesExternalTrafficChain {
			ensureChain(externalTrafficChain, tx, activeChains, skipServiceUpdate)
		}

		var internalTrafficFilterVerdict, externalTrafficFilterVerdict string
		if !hasEndpoints {
			// The service has no endpoints at all; hasInternalEndpoints and
			// hasExternalEndpoints will also be false, and we will not
			// generate any chains in the "nat" table for the service; only
			// rules in the "filter" table rejecting incoming packets for
			// the service's IPs.
			internalTrafficFilterVerdict = fmt.Sprintf("goto %s", rejectChain)
			externalTrafficFilterVerdict = fmt.Sprintf("goto %s", rejectChain)
		} else {
			if !hasInternalEndpoints {
				// The internalTrafficPolicy is "Local" but there are no local
				// endpoints. Traffic to the clusterIP will be dropped, but
				// external traffic may still be accepted.
				internalTrafficFilterVerdict = "drop"
				serviceNoLocalEndpointsTotalInternal++
			}
			if !hasExternalEndpoints {
				// The externalTrafficPolicy is "Local" but there are no
				// local endpoints. Traffic to "external" IPs from outside
				// the cluster will be dropped, but traffic from inside
				// the cluster may still be accepted.
				externalTrafficFilterVerdict = "drop"
				serviceNoLocalEndpointsTotalExternal++
			}
		}

		// Capture the clusterIP.
		proxier.clusterIPs.ensureElem(tx, &knftables.Element{
			Set: clusterIPsSet,
			Key: []string{svcInfo.ClusterIP().String()},
		})
		if hasInternalEndpoints {
			proxier.serviceIPs.ensureElem(tx, &knftables.Element{
				Map: serviceIPsMap,
				Key: []string{
					svcInfo.ClusterIP().String(),
					protocol,
					strconv.Itoa(svcInfo.Port()),
				},
				Value: []string{
					fmt.Sprintf("goto %s", internalTrafficChain),
				},
			})
		} else {
			// No endpoints.
			proxier.noEndpointServices.ensureElem(tx, &knftables.Element{
				Map: noEndpointServicesMap,
				Key: []string{
					svcInfo.ClusterIP().String(),
					protocol,
					strconv.Itoa(svcInfo.Port()),
				},
				Value: []string{
					internalTrafficFilterVerdict,
				},
				Comment: &svcPortNameString,
			})
		}

		// Capture externalIPs.
		for _, externalIP := range svcInfo.ExternalIPs() {
			if hasEndpoints {
				// Send traffic bound for external IPs to the "external
				// destinations" chain.
				proxier.serviceIPs.ensureElem(tx, &knftables.Element{
					Map: serviceIPsMap,
					Key: []string{
						externalIP.String(),
						protocol,
						strconv.Itoa(svcInfo.Port()),
					},
					Value: []string{
						fmt.Sprintf("goto %s", externalTrafficChain),
					},
				})
			}
			if !hasExternalEndpoints {
				// Either no endpoints at all (REJECT) or no endpoints for
				// external traffic (DROP anything that didn't get
				// short-circuited by the EXT chain.)
				proxier.noEndpointServices.ensureElem(tx, &knftables.Element{
					Map: noEndpointServicesMap,
					Key: []string{
						externalIP.String(),
						protocol,
						strconv.Itoa(svcInfo.Port()),
					},
					Value: []string{
						externalTrafficFilterVerdict,
					},
					Comment: &svcPortNameString,
				})
			}
		}

		usesFWChain := len(svcInfo.LoadBalancerVIPs()) > 0 && len(svcInfo.LoadBalancerSourceRanges()) > 0
		fwChain := svcInfo.firewallChainName
		if usesFWChain {
			ensureChain(fwChain, tx, activeChains, skipServiceUpdate)
		}

		// Capture load-balancer ingress.
		for _, lbip := range svcInfo.LoadBalancerVIPs() {
			if hasEndpoints {
				proxier.serviceIPs.ensureElem(tx, &knftables.Element{
					Map: serviceIPsMap,
					Key: []string{
						lbip.String(),
						protocol,
						strconv.Itoa(svcInfo.Port()),
					},
					Value: []string{
						fmt.Sprintf("goto %s", externalTrafficChain),
					},
				})
			}

			if usesFWChain {
				proxier.firewallIPs.ensureElem(tx, &knftables.Element{
					Map: firewallIPsMap,
					Key: []string{
						lbip.String(),
						protocol,
						strconv.Itoa(svcInfo.Port()),
					},
					Value: []string{
						fmt.Sprintf("goto %s", fwChain),
					},
				})
			}
		}
		if !hasExternalEndpoints {
			// Either no endpoints at all (REJECT) or no endpoints for
			// external traffic (DROP anything that didn't get short-circuited
			// by the EXT chain.)
			for _, lbip := range svcInfo.LoadBalancerVIPs() {
				proxier.noEndpointServices.ensureElem(tx, &knftables.Element{
					Map: noEndpointServicesMap,
					Key: []string{
						lbip.String(),
						protocol,
						strconv.Itoa(svcInfo.Port()),
					},
					Value: []string{
						externalTrafficFilterVerdict,
					},
					Comment: &svcPortNameString,
				})
			}
		}

		// Capture nodeports.
		if svcInfo.NodePort() != 0 {
			if hasEndpoints {
				// Jump to the external destination chain.  For better or for
				// worse, nodeports are not subject to loadBalancerSourceRanges,
				// and we can't change that.
				proxier.serviceNodePorts.ensureElem(tx, &knftables.Element{
					Map: serviceNodePortsMap,
					Key: []string{
						protocol,
						strconv.Itoa(svcInfo.NodePort()),
					},
					Value: []string{
						fmt.Sprintf("goto %s", externalTrafficChain),
					},
				})
			}
			if !hasExternalEndpoints {
				// Either no endpoints at all (REJECT) or no endpoints for
				// external traffic (DROP anything that didn't get
				// short-circuited by the EXT chain.)
				proxier.noEndpointNodePorts.ensureElem(tx, &knftables.Element{
					Map: noEndpointNodePortsMap,
					Key: []string{
						protocol,
						strconv.Itoa(svcInfo.NodePort()),
					},
					Value: []string{
						externalTrafficFilterVerdict,
					},
					Comment: &svcPortNameString,
				})
			}
		}

		// All the following operations are service-chain related and may be skipped if no svc or endpoint
		// changes are required.
		if skipServiceUpdate {
			continue
		}

		// Set up internal traffic handling.
		if hasInternalEndpoints {
			if proxier.masqueradeAll {
				tx.Add(&knftables.Rule{
					Chain: internalTrafficChain,
					Rule: knftables.Concat(
						ipX, "daddr", svcInfo.ClusterIP(),
						protocol, "dport", svcInfo.Port(),
						"jump", markMasqChain,
					),
				})
			} else if proxier.localDetector.IsImplemented() {
				// This masquerades off-cluster traffic to a service VIP. The
				// idea is that you can establish a static route for your
				// Service range, routing to any node, and that node will
				// bridge into the Service for you. Since that might bounce
				// off-node, we masquerade here.
				tx.Add(&knftables.Rule{
					Chain: internalTrafficChain,
					Rule: knftables.Concat(
						ipX, "daddr", svcInfo.ClusterIP(),
						protocol, "dport", svcInfo.Port(),
						proxier.localDetector.IfNotLocalNFT(),
						"jump", markMasqChain,
					),
				})
			}
		}

		// Set up external traffic handling (if any "external" destinations are
		// enabled). All captured traffic for all external destinations should
		// jump to externalTrafficChain, which will handle some special cases and
		// then jump to externalPolicyChain.
		if usesExternalTrafficChain {
			if !svcInfo.ExternalPolicyLocal() {
				// If we are using non-local endpoints we need to masquerade,
				// in case we cross nodes.
				tx.Add(&knftables.Rule{
					Chain: externalTrafficChain,
					Rule: knftables.Concat(
						"jump", markMasqChain,
					),
				})
			} else {
				// If we are only using same-node endpoints, we can retain the
				// source IP in most cases.

				if proxier.localDetector.IsImplemented() {
					// Treat all locally-originated pod -> external destination
					// traffic as a special-case.  It is subject to neither
					// form of traffic policy, which simulates going up-and-out
					// to an external load-balancer and coming back in.
					tx.Add(&knftables.Rule{
						Chain: externalTrafficChain,
						Rule: knftables.Concat(
							proxier.localDetector.IfLocalNFT(),
							"goto", clusterPolicyChain,
						),
						Comment: ptr.To("short-circuit pod traffic"),
					})
				}

				// Locally originated traffic (not a pod, but the host node)
				// still needs masquerade because the LBIP itself is a local
				// address, so that will be the chosen source IP.
				tx.Add(&knftables.Rule{
					Chain: externalTrafficChain,
					Rule: knftables.Concat(
						"fib", "saddr", "type", "local",
						"jump", markMasqChain,
					),
					Comment: ptr.To("masquerade local traffic"),
				})

				// Redirect all src-type=LOCAL -> external destination to the
				// policy=cluster chain. This allows traffic originating
				// from the host to be redirected to the service correctly.
				tx.Add(&knftables.Rule{
					Chain: externalTrafficChain,
					Rule: knftables.Concat(
						"fib", "saddr", "type", "local",
						"goto", clusterPolicyChain,
					),
					Comment: ptr.To("short-circuit local traffic"),
				})
			}

			// Anything else falls thru to the appropriate policy chain.
			if hasExternalEndpoints {
				tx.Add(&knftables.Rule{
					Chain: externalTrafficChain,
					Rule: knftables.Concat(
						"goto", externalPolicyChain,
					),
				})
			}
		}

		if usesFWChain {
			var sources []string
			allowFromNode := false
			for _, cidr := range svcInfo.LoadBalancerSourceRanges() {
				if len(sources) > 0 {
					sources = append(sources, ",")
				}
				sources = append(sources, cidr.String())
				if cidr.Contains(proxier.nodeIP) {
					allowFromNode = true
				}
			}
			// For VIP-like LBs, the VIP is often added as a local
			// address (via an IP route rule).  In that case, a request
			// from a node to the VIP will not hit the loadbalancer but
			// will loop back with the source IP set to the VIP.  We
			// need the following rules to allow requests from this node.
			if allowFromNode {
				for _, lbip := range svcInfo.LoadBalancerVIPs() {
					sources = append(sources, ",", lbip.String())
				}
			}
			tx.Add(&knftables.Rule{
				Chain: fwChain,
				Rule: knftables.Concat(
					ipX, "saddr", "!=", "{", sources, "}",
					"drop",
				),
			})
		}

		if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
			// Generate the per-endpoint affinity sets
			for _, ep := range allLocallyReachableEndpoints {
				epInfo, ok := ep.(*endpointInfo)
				if !ok {
					proxier.logger.Error(nil, "Failed to cast endpointsInfo", "endpointsInfo", ep)
					continue
				}

				// Create a set to store current affinity mappings. As
				// with the iptables backend, endpoint affinity is
				// recorded for connections from a particular source IP
				// (without regard to source port) to a particular
				// ServicePort (without regard to which service IP was
				// used to reach the service). This may be changed in the
				// future.
				tx.Add(&knftables.Set{
					Name: epInfo.affinitySetName,
					Type: ipvX_addr,
					Flags: []knftables.SetFlag{
						// The nft docs say "dynamic" is only
						// needed for sets containing stateful
						// objects (eg counters), but (at least on
						// RHEL8) if we create the set without
						// "dynamic", it later gets mutated to
						// have it, and then the next attempt to
						// tx.Add() it here fails because it looks
						// like we're trying to change the flags.
						knftables.DynamicFlag,
						knftables.TimeoutFlag,
					},
					Timeout: ptr.To(time.Duration(svcInfo.StickyMaxAgeSeconds()) * time.Second),
				})
			}
		}

		// If Cluster policy is in use, create the chain and create rules jumping
		// from clusterPolicyChain to the clusterEndpoints
		if usesClusterPolicyChain {
			proxier.writeServiceToEndpointRules(tx, svcInfo, clusterPolicyChain, clusterEndpoints)
		}

		// If Local policy is in use, create rules jumping from localPolicyChain
		// to the localEndpoints
		if usesLocalPolicyChain {
			proxier.writeServiceToEndpointRules(tx, svcInfo, localPolicyChain, localEndpoints)
		}

		// Generate the per-endpoint chains
		for _, ep := range allLocallyReachableEndpoints {
			epInfo, ok := ep.(*endpointInfo)
			if !ok {
				proxier.logger.Error(nil, "Failed to cast endpointInfo", "endpointInfo", ep)
				continue
			}

			endpointChain := epInfo.chainName

			// Handle traffic that loops back to the originator with SNAT.
			tx.Add(&knftables.Rule{
				Chain: endpointChain,
				Rule: knftables.Concat(
					ipX, "saddr", epInfo.IP(),
					"jump", markMasqChain,
				),
			})

			// Handle session affinity
			if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
				tx.Add(&knftables.Rule{
					Chain: endpointChain,
					Rule: knftables.Concat(
						"update", "@", epInfo.affinitySetName,
						"{", ipX, "saddr", "}",
					),
				})
			}

			// DNAT to final destination.
			tx.Add(&knftables.Rule{
				Chain: endpointChain,
				Rule: knftables.Concat(
					"meta l4proto", protocol,
					"dnat to", epInfo.String(),
				),
			})
		}
	}

	// Figure out which chains are now stale. Unfortunately, we can't delete them
	// right away, because with kernels before 6.2, if there is a map element pointing
	// to a chain, and you delete that map element, the kernel doesn't notice until a
	// short amount of time later that the chain is now unreferenced. So we flush them
	// now, and record the time that they become stale in staleChains so they can be
	// deleted later.
	existingChains, err := proxier.nftables.List(context.TODO(), "chains")
	if err == nil {
		for _, chain := range existingChains {
			if isServiceChainName(chain) {
				if !activeChains.Has(chain) {
					tx.Flush(&knftables.Chain{
						Name: chain,
					})
					proxier.staleChains[chain] = start
				} else {
					delete(proxier.staleChains, chain)
				}
			}
		}
	} else if !knftables.IsNotFound(err) {
		proxier.logger.Error(err, "Failed to list nftables chains: stale chains will not be deleted")
	}

	// OTOH, we can immediately delete any stale affinity sets
	existingSets, err := proxier.nftables.List(context.TODO(), "sets")
	if err == nil {
		for _, set := range existingSets {
			if isAffinitySetName(set) && !activeAffinitySets.Has(set) {
				tx.Delete(&knftables.Set{
					Name: set,
				})
			}
		}
	} else if !knftables.IsNotFound(err) {
		proxier.logger.Error(err, "Failed to list nftables sets: stale affinity sets will not be deleted")
	}

	proxier.clusterIPs.cleanupLeftoverKeys(tx)
	proxier.serviceIPs.cleanupLeftoverKeys(tx)
	proxier.firewallIPs.cleanupLeftoverKeys(tx)
	proxier.noEndpointServices.cleanupLeftoverKeys(tx)
	proxier.noEndpointNodePorts.cleanupLeftoverKeys(tx)
	proxier.serviceNodePorts.cleanupLeftoverKeys(tx)

	// Sync rules.
	proxier.logger.V(2).Info("Reloading service nftables data",
		"numServices", len(proxier.svcPortMap),
		"numEndpoints", totalEndpoints,
	)

	if klogV9 := klog.V(9); klogV9.Enabled() {
		klogV9.InfoS("Running nftables transaction", "transaction", tx.String())
	}

	err = proxier.nftables.Run(context.TODO(), tx)
	if err != nil {
		proxier.logger.Error(err, "nftables sync failed")
		metrics.NFTablesSyncFailuresTotal.WithLabelValues(string(proxier.ipFamily)).Inc()

		// staleChains is now incorrect since we didn't actually flush the
		// chains in it. We can recompute it next time.
		clear(proxier.staleChains)
		// Log failed transaction and list full kube-proxy table.
		proxier.logFailure(tx)
		return
	}
	success = true
	proxier.needFullSync = false

	for name, lastChangeTriggerTimes := range endpointUpdateResult.LastChangeTriggerTimes {
		for _, lastChangeTriggerTime := range lastChangeTriggerTimes {
			latency := metrics.SinceInSeconds(lastChangeTriggerTime)
			metrics.NetworkProgrammingLatency.WithLabelValues(string(proxier.ipFamily)).Observe(latency)
			proxier.logger.V(4).Info("Network programming", "endpoint", klog.KRef(name.Namespace, name.Name), "elapsed", latency)
		}
	}

	metrics.SyncProxyRulesNoLocalEndpointsTotal.WithLabelValues("internal", string(proxier.ipFamily)).Set(float64(serviceNoLocalEndpointsTotalInternal))
	metrics.SyncProxyRulesNoLocalEndpointsTotal.WithLabelValues("external", string(proxier.ipFamily)).Set(float64(serviceNoLocalEndpointsTotalExternal))
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

	if endpointUpdateResult.ConntrackCleanupRequired {
		// Finish housekeeping, clear stale conntrack entries for UDP Services
		conntrack.CleanStaleEntries(proxier.conntrack, proxier.ipFamily, proxier.svcPortMap, proxier.endpointsMap)
	}
}

func (proxier *Proxier) writeServiceToEndpointRules(tx *knftables.Transaction, svcInfo *servicePortInfo, svcChain string, endpoints []proxy.Endpoint) {
	// First write session affinity rules, if applicable.
	if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
		ipX := "ip"
		if proxier.ipFamily == v1.IPv6Protocol {
			ipX = "ip6"
		}

		for _, ep := range endpoints {
			epInfo, ok := ep.(*endpointInfo)
			if !ok {
				continue
			}

			tx.Add(&knftables.Rule{
				Chain: svcChain,
				Rule: knftables.Concat(
					ipX, "saddr", "@", epInfo.affinitySetName,
					"goto", epInfo.chainName,
				),
			})
		}
	}

	// Now write loadbalancing rule
	var elements []string
	for i, ep := range endpoints {
		epInfo, ok := ep.(*endpointInfo)
		if !ok {
			continue
		}

		elements = append(elements,
			strconv.Itoa(i), ":", "goto", epInfo.chainName,
		)
		if i != len(endpoints)-1 {
			elements = append(elements, ",")
		}
	}
	tx.Add(&knftables.Rule{
		Chain: svcChain,
		Rule: knftables.Concat(
			"numgen random mod", len(endpoints), "vmap",
			"{", elements, "}",
		),
	})
}
