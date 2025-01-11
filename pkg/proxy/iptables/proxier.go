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

package iptables

//
// NOTE: this needs to be tested in e2e since it uses iptables for everything.
//

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/conntrack"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	"k8s.io/kubernetes/pkg/proxy/metaproxier"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/proxy/util/nfacct"
	"k8s.io/kubernetes/pkg/util/async"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilexec "k8s.io/utils/exec"
)

const (
	// the services chain
	kubeServicesChain utiliptables.Chain = "KUBE-SERVICES"

	// the external services chain
	kubeExternalServicesChain utiliptables.Chain = "KUBE-EXTERNAL-SERVICES"

	// the nodeports chain
	kubeNodePortsChain utiliptables.Chain = "KUBE-NODEPORTS"

	// the kubernetes postrouting chain
	kubePostroutingChain utiliptables.Chain = "KUBE-POSTROUTING"

	// kubeMarkMasqChain is the mark-for-masquerade chain
	kubeMarkMasqChain utiliptables.Chain = "KUBE-MARK-MASQ"

	// the kubernetes forward chain
	kubeForwardChain utiliptables.Chain = "KUBE-FORWARD"

	// kubeProxyFirewallChain is the kube-proxy firewall chain
	kubeProxyFirewallChain utiliptables.Chain = "KUBE-PROXY-FIREWALL"

	// kube proxy canary chain is used for monitoring rule reload
	kubeProxyCanaryChain utiliptables.Chain = "KUBE-PROXY-CANARY"

	// kubeletFirewallChain is a duplicate of kubelet's firewall containing
	// the anti-martian-packet rule. It should not be used for any other
	// rules.
	kubeletFirewallChain utiliptables.Chain = "KUBE-FIREWALL"

	// largeClusterEndpointsThreshold is the number of endpoints at which
	// we switch into "large cluster mode" and optimize for iptables
	// performance over iptables debuggability
	largeClusterEndpointsThreshold = 1000
)

const sysctlRouteLocalnet = "net/ipv4/conf/all/route_localnet"
const sysctlNFConntrackTCPBeLiberal = "net/netfilter/nf_conntrack_tcp_be_liberal"

// NewDualStackProxier creates a MetaProxier instance, with IPv4 and IPv6 proxies.
func NewDualStackProxier(
	ctx context.Context,
	ipts map[v1.IPFamily]utiliptables.Interface,
	sysctl utilsysctl.Interface,
	exec utilexec.Interface,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	masqueradeAll bool,
	localhostNodePorts bool,
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
	ipv4Proxier, err := NewProxier(ctx, v1.IPv4Protocol, ipts[v1.IPv4Protocol], sysctl,
		exec, syncPeriod, minSyncPeriod, masqueradeAll, localhostNodePorts, masqueradeBit,
		localDetectors[v1.IPv4Protocol], hostname, nodeIPs[v1.IPv4Protocol],
		recorder, healthzServer, nodePortAddresses, initOnly)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv4 proxier: %v", err)
	}

	ipv6Proxier, err := NewProxier(ctx, v1.IPv6Protocol, ipts[v1.IPv6Protocol], sysctl,
		exec, syncPeriod, minSyncPeriod, masqueradeAll, false, masqueradeBit,
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

// Proxier is an iptables based proxy for connections between a localhost:lport
// and services that provide the actual backends.
type Proxier struct {
	// ipFamily defines the IP family which this proxier is tracking.
	ipFamily v1.IPFamily

	// endpointsChanges and serviceChanges contains all changes to endpoints and
	// services that happened since iptables was synced. For a single object,
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
	// updating iptables with some partial data after kube-proxy restart.
	endpointSlicesSynced bool
	servicesSynced       bool
	lastFullSync         time.Time
	needFullSync         bool
	initialized          int32
	syncRunner           *async.BoundedFrequencyRunner // governs calls to syncProxyRules
	syncPeriod           time.Duration
	lastIPTablesCleanup  time.Time

	// These are effectively const and do not need the mutex to be held.
	iptables       utiliptables.Interface
	masqueradeAll  bool
	masqueradeMark string
	conntrack      conntrack.Interface
	nfacct         nfacct.Interface
	localDetector  proxyutil.LocalTrafficDetector
	hostname       string
	nodeIP         net.IP
	recorder       events.EventRecorder

	serviceHealthServer healthcheck.ServiceHealthServer
	healthzServer       *healthcheck.ProxyHealthServer

	// Since converting probabilities (floats) to strings is expensive
	// and we are using only probabilities in the format of 1/n, we are
	// precomputing some number of those and cache for future reuse.
	precomputedProbabilities []string

	// The following buffers are used to reuse memory and avoid allocations
	// that are significantly impacting performance.
	iptablesData             *bytes.Buffer
	existingFilterChainsData *bytes.Buffer
	filterChains             proxyutil.LineBuffer
	filterRules              proxyutil.LineBuffer
	natChains                proxyutil.LineBuffer
	natRules                 proxyutil.LineBuffer

	// largeClusterMode is set at the beginning of syncProxyRules if we are
	// going to end up outputting "lots" of iptables rules and so we need to
	// optimize for performance over debuggability.
	largeClusterMode bool

	// localhostNodePorts indicates whether we allow NodePort services to be accessed
	// via localhost.
	localhostNodePorts bool

	// conntrackTCPLiberal indicates whether the system sets the kernel nf_conntrack_tcp_be_liberal
	conntrackTCPLiberal bool

	// nodePortAddresses selects the interfaces where nodePort works.
	nodePortAddresses *proxyutil.NodePortAddresses
	// networkInterfacer defines an interface for several net library functions.
	// Inject for test purpose.
	networkInterfacer proxyutil.NetworkInterfacer

	logger klog.Logger

	// nfAcctCounters can be used to determine if a counter exist in the nfacct subsystem.
	nfAcctCounters map[string]bool
}

// Proxier implements proxy.Provider
var _ proxy.Provider = &Proxier{}

// NewProxier returns a new Proxier given an iptables Interface instance.
// Because of the iptables logic, it is assumed that there is only a single Proxier active on a machine.
// An error will be returned if iptables fails to update or acquire the initial lock.
// Once a proxier is created, it will keep iptables up to date in the background and
// will not terminate if a particular iptables call fails.
func NewProxier(ctx context.Context,
	ipFamily v1.IPFamily,
	ipt utiliptables.Interface,
	sysctl utilsysctl.Interface,
	exec utilexec.Interface,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	masqueradeAll bool,
	localhostNodePorts bool,
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
	nodePortAddresses := proxyutil.NewNodePortAddresses(ipFamily, nodePortAddressStrings)

	if !nodePortAddresses.ContainsIPv4Loopback() {
		localhostNodePorts = false
	}
	if localhostNodePorts {
		// Set the route_localnet sysctl we need for exposing NodePorts on loopback addresses
		// Refer to https://issues.k8s.io/90259
		logger.Info("Setting route_localnet=1 to allow node-ports on localhost; to change this either disable iptables.localhostNodePorts (--iptables-localhost-nodeports) or set nodePortAddresses (--nodeport-addresses) to filter loopback addresses")
		if err := proxyutil.EnsureSysctl(sysctl, sysctlRouteLocalnet, 1); err != nil {
			return nil, err
		}
	}

	// Be conservative in what you do, be liberal in what you accept from others.
	// If it's non-zero, we mark only out of window RST segments as INVALID.
	// Ref: https://docs.kernel.org/networking/nf_conntrack-sysctl.html
	conntrackTCPLiberal := false
	if val, err := sysctl.GetSysctl(sysctlNFConntrackTCPBeLiberal); err == nil && val != 0 {
		conntrackTCPLiberal = true
		logger.Info("nf_conntrack_tcp_be_liberal set, not installing DROP rules for INVALID packets")
	}

	if initOnly {
		logger.Info("System initialized and --init-only specified")
		return nil, nil
	}

	// Generate the masquerade mark to use for SNAT rules.
	masqueradeValue := 1 << uint(masqueradeBit)
	masqueradeMark := fmt.Sprintf("%#08x", masqueradeValue)
	logger.V(2).Info("Using iptables mark for masquerade", "mark", masqueradeMark)

	serviceHealthServer := healthcheck.NewServiceHealthServer(hostname, recorder, nodePortAddresses, healthzServer)
	nfacctRunner, err := nfacct.New()
	if err != nil {
		logger.Error(err, "Failed to create nfacct runner, nfacct based metrics won't be available")
	}

	proxier := &Proxier{
		ipFamily:                 ipFamily,
		svcPortMap:               make(proxy.ServicePortMap),
		serviceChanges:           proxy.NewServiceChangeTracker(ipFamily, newServiceInfo, nil),
		endpointsMap:             make(proxy.EndpointsMap),
		endpointsChanges:         proxy.NewEndpointsChangeTracker(ipFamily, hostname, newEndpointInfo, nil),
		needFullSync:             true,
		syncPeriod:               syncPeriod,
		iptables:                 ipt,
		masqueradeAll:            masqueradeAll,
		masqueradeMark:           masqueradeMark,
		conntrack:                conntrack.New(),
		nfacct:                   nfacctRunner,
		localDetector:            localDetector,
		hostname:                 hostname,
		nodeIP:                   nodeIP,
		recorder:                 recorder,
		serviceHealthServer:      serviceHealthServer,
		healthzServer:            healthzServer,
		precomputedProbabilities: make([]string, 0, 1001),
		iptablesData:             bytes.NewBuffer(nil),
		existingFilterChainsData: bytes.NewBuffer(nil),
		filterChains:             proxyutil.NewLineBuffer(),
		filterRules:              proxyutil.NewLineBuffer(),
		natChains:                proxyutil.NewLineBuffer(),
		natRules:                 proxyutil.NewLineBuffer(),
		localhostNodePorts:       localhostNodePorts,
		nodePortAddresses:        nodePortAddresses,
		networkInterfacer:        proxyutil.RealNetwork{},
		conntrackTCPLiberal:      conntrackTCPLiberal,
		logger:                   logger,
		nfAcctCounters: map[string]bool{
			metrics.IPTablesCTStateInvalidDroppedNFAcctCounter: false,
			metrics.LocalhostNodePortAcceptedNFAcctCounter:     false,
		},
	}

	burstSyncs := 2
	logger.V(2).Info("Iptables sync params", "minSyncPeriod", minSyncPeriod, "syncPeriod", syncPeriod, "burstSyncs", burstSyncs)
	// We pass syncPeriod to ipt.Monitor, which will call us only if it needs to.
	// We need to pass *some* maxInterval to NewBoundedFrequencyRunner anyway though.
	// time.Hour is arbitrary.
	proxier.syncRunner = async.NewBoundedFrequencyRunner("sync-runner", proxier.syncProxyRules, minSyncPeriod, proxyutil.FullSyncPeriod, burstSyncs)

	go ipt.Monitor(kubeProxyCanaryChain, []utiliptables.Table{utiliptables.TableMangle, utiliptables.TableNAT, utiliptables.TableFilter},
		proxier.forceSyncProxyRules, syncPeriod, wait.NeverStop)

	if ipt.HasRandomFully() {
		logger.V(2).Info("Iptables supports --random-fully")
	} else {
		logger.V(2).Info("Iptables does not support --random-fully")
	}

	return proxier, nil
}

// internal struct for string service information
type servicePortInfo struct {
	*proxy.BaseServicePortInfo
	// The following fields are computed and stored for performance reasons.
	nameString             string
	clusterPolicyChainName utiliptables.Chain
	localPolicyChainName   utiliptables.Chain
	firewallChainName      utiliptables.Chain
	externalChainName      utiliptables.Chain
}

// returns a new proxy.ServicePort which abstracts a serviceInfo
func newServiceInfo(port *v1.ServicePort, service *v1.Service, bsvcPortInfo *proxy.BaseServicePortInfo) proxy.ServicePort {
	svcPort := &servicePortInfo{BaseServicePortInfo: bsvcPortInfo}

	// Store the following for performance reasons.
	svcName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	svcPortName := proxy.ServicePortName{NamespacedName: svcName, Port: port.Name}
	protocol := strings.ToLower(string(svcPort.Protocol()))
	svcPort.nameString = svcPortName.String()
	svcPort.clusterPolicyChainName = servicePortPolicyClusterChain(svcPort.nameString, protocol)
	svcPort.localPolicyChainName = servicePortPolicyLocalChainName(svcPort.nameString, protocol)
	svcPort.firewallChainName = serviceFirewallChainName(svcPort.nameString, protocol)
	svcPort.externalChainName = serviceExternalChainName(svcPort.nameString, protocol)

	return svcPort
}

// internal struct for endpoints information
type endpointInfo struct {
	*proxy.BaseEndpointInfo

	ChainName utiliptables.Chain
}

// returns a new proxy.Endpoint which abstracts a endpointInfo
func newEndpointInfo(baseInfo *proxy.BaseEndpointInfo, svcPortName *proxy.ServicePortName) proxy.Endpoint {
	return &endpointInfo{
		BaseEndpointInfo: baseInfo,
		ChainName:        servicePortEndpointChainName(svcPortName.String(), strings.ToLower(string(svcPortName.Protocol)), baseInfo.String()),
	}
}

type iptablesJumpChain struct {
	table     utiliptables.Table
	dstChain  utiliptables.Chain
	srcChain  utiliptables.Chain
	comment   string
	extraArgs []string
}

var iptablesJumpChains = []iptablesJumpChain{
	{utiliptables.TableFilter, kubeExternalServicesChain, utiliptables.ChainInput, "kubernetes externally-visible service portals", []string{"-m", "conntrack", "--ctstate", "NEW"}},
	{utiliptables.TableFilter, kubeExternalServicesChain, utiliptables.ChainForward, "kubernetes externally-visible service portals", []string{"-m", "conntrack", "--ctstate", "NEW"}},
	{utiliptables.TableFilter, kubeNodePortsChain, utiliptables.ChainInput, "kubernetes health check service ports", nil},
	{utiliptables.TableFilter, kubeServicesChain, utiliptables.ChainForward, "kubernetes service portals", []string{"-m", "conntrack", "--ctstate", "NEW"}},
	{utiliptables.TableFilter, kubeServicesChain, utiliptables.ChainOutput, "kubernetes service portals", []string{"-m", "conntrack", "--ctstate", "NEW"}},
	{utiliptables.TableFilter, kubeForwardChain, utiliptables.ChainForward, "kubernetes forwarding rules", nil},
	{utiliptables.TableFilter, kubeProxyFirewallChain, utiliptables.ChainInput, "kubernetes load balancer firewall", []string{"-m", "conntrack", "--ctstate", "NEW"}},
	{utiliptables.TableFilter, kubeProxyFirewallChain, utiliptables.ChainOutput, "kubernetes load balancer firewall", []string{"-m", "conntrack", "--ctstate", "NEW"}},
	{utiliptables.TableFilter, kubeProxyFirewallChain, utiliptables.ChainForward, "kubernetes load balancer firewall", []string{"-m", "conntrack", "--ctstate", "NEW"}},
	{utiliptables.TableNAT, kubeServicesChain, utiliptables.ChainOutput, "kubernetes service portals", nil},
	{utiliptables.TableNAT, kubeServicesChain, utiliptables.ChainPrerouting, "kubernetes service portals", nil},
	{utiliptables.TableNAT, kubePostroutingChain, utiliptables.ChainPostrouting, "kubernetes postrouting rules", nil},
}

// Duplicates of chains created in pkg/kubelet/kubelet_network_linux.go; we create these
// on startup but do not delete them in CleanupLeftovers.
var iptablesKubeletJumpChains = []iptablesJumpChain{
	{utiliptables.TableFilter, kubeletFirewallChain, utiliptables.ChainInput, "", nil},
	{utiliptables.TableFilter, kubeletFirewallChain, utiliptables.ChainOutput, "", nil},
}

// When chains get removed from iptablesJumpChains, add them here so they get cleaned up
// on upgrade.
var iptablesCleanupOnlyChains = []iptablesJumpChain{}

// CleanupLeftovers removes all iptables rules and chains created by the Proxier
// It returns true if an error was encountered. Errors are logged.
func CleanupLeftovers(ctx context.Context, ipt utiliptables.Interface) (encounteredError bool) {
	logger := klog.FromContext(ctx)
	// Unlink our chains
	for _, jump := range append(iptablesJumpChains, iptablesCleanupOnlyChains...) {
		args := append(jump.extraArgs,
			"-m", "comment", "--comment", jump.comment,
			"-j", string(jump.dstChain),
		)
		if err := ipt.DeleteRule(jump.table, jump.srcChain, args...); err != nil {
			if !utiliptables.IsNotFoundError(err) {
				logger.Error(err, "Error removing pure-iptables proxy rule")
				encounteredError = true
			}
		}
	}

	// Flush and remove all of our "-t nat" chains.
	iptablesData := bytes.NewBuffer(nil)
	if err := ipt.SaveInto(utiliptables.TableNAT, iptablesData); err != nil {
		logger.Error(err, "Failed to execute iptables-save", "table", utiliptables.TableNAT)
		encounteredError = true
	} else {
		existingNATChains := utiliptables.GetChainsFromTable(iptablesData.Bytes())
		natChains := proxyutil.NewLineBuffer()
		natRules := proxyutil.NewLineBuffer()
		natChains.Write("*nat")
		// Start with chains we know we need to remove.
		for _, chain := range []utiliptables.Chain{kubeServicesChain, kubeNodePortsChain, kubePostroutingChain} {
			if existingNATChains.Has(chain) {
				chainString := string(chain)
				natChains.Write(utiliptables.MakeChainLine(chain)) // flush
				natRules.Write("-X", chainString)                  // delete
			}
		}
		// Hunt for service and endpoint chains.
		for chain := range existingNATChains {
			chainString := string(chain)
			if isServiceChainName(chainString) {
				natChains.Write(utiliptables.MakeChainLine(chain)) // flush
				natRules.Write("-X", chainString)                  // delete
			}
		}
		natRules.Write("COMMIT")
		natLines := append(natChains.Bytes(), natRules.Bytes()...)
		// Write it.
		err = ipt.Restore(utiliptables.TableNAT, natLines, utiliptables.NoFlushTables, utiliptables.RestoreCounters)
		if err != nil {
			logger.Error(err, "Failed to execute iptables-restore", "table", utiliptables.TableNAT)
			metrics.IPTablesRestoreFailuresTotal.WithLabelValues(string(ipt.Protocol())).Inc()
			encounteredError = true
		}
	}

	// Flush and remove all of our "-t filter" chains.
	iptablesData.Reset()
	if err := ipt.SaveInto(utiliptables.TableFilter, iptablesData); err != nil {
		logger.Error(err, "Failed to execute iptables-save", "table", utiliptables.TableFilter)
		encounteredError = true
	} else {
		existingFilterChains := utiliptables.GetChainsFromTable(iptablesData.Bytes())
		filterChains := proxyutil.NewLineBuffer()
		filterRules := proxyutil.NewLineBuffer()
		filterChains.Write("*filter")
		for _, chain := range []utiliptables.Chain{kubeServicesChain, kubeExternalServicesChain, kubeForwardChain, kubeNodePortsChain} {
			if existingFilterChains.Has(chain) {
				chainString := string(chain)
				filterChains.Write(utiliptables.MakeChainLine(chain))
				filterRules.Write("-X", chainString)
			}
		}
		filterRules.Write("COMMIT")
		filterLines := append(filterChains.Bytes(), filterRules.Bytes()...)
		// Write it.
		if err := ipt.Restore(utiliptables.TableFilter, filterLines, utiliptables.NoFlushTables, utiliptables.RestoreCounters); err != nil {
			logger.Error(err, "Failed to execute iptables-restore", "table", utiliptables.TableFilter)
			metrics.IPTablesRestoreFailuresTotal.WithLabelValues(string(ipt.Protocol())).Inc()
			encounteredError = true
		}
	}
	return encounteredError
}

func computeProbability(n int) string {
	return fmt.Sprintf("%0.10f", 1.0/float64(n))
}

// This assumes proxier.mu is held
func (proxier *Proxier) precomputeProbabilities(numberOfPrecomputed int) {
	if len(proxier.precomputedProbabilities) == 0 {
		proxier.precomputedProbabilities = append(proxier.precomputedProbabilities, "<bad value>")
	}
	for i := len(proxier.precomputedProbabilities); i <= numberOfPrecomputed; i++ {
		proxier.precomputedProbabilities = append(proxier.precomputedProbabilities, computeProbability(i))
	}
}

// This assumes proxier.mu is held
func (proxier *Proxier) probability(n int) string {
	if n >= len(proxier.precomputedProbabilities) {
		proxier.precomputeProbabilities(n)
	}
	return proxier.precomputedProbabilities[n]
}

// Sync is called to synchronize the proxier state to iptables as soon as possible.
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
func (proxier *Proxier) OnServiceCIDRsChanged(_ []string) {}

// portProtoHash takes the ServicePortName and protocol for a service
// returns the associated 16 character hash. This is computed by hashing (sha256)
// then encoding to base32 and truncating to 16 chars. We do this because IPTables
// Chain Names must be <= 28 chars long, and the longer they are the harder they are to read.
func portProtoHash(servicePortName string, protocol string) string {
	hash := sha256.Sum256([]byte(servicePortName + protocol))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return encoded[:16]
}

const (
	servicePortPolicyClusterChainNamePrefix = "KUBE-SVC-"
	servicePortPolicyLocalChainNamePrefix   = "KUBE-SVL-"
	serviceFirewallChainNamePrefix          = "KUBE-FW-"
	serviceExternalChainNamePrefix          = "KUBE-EXT-"
	servicePortEndpointChainNamePrefix      = "KUBE-SEP-"
)

// servicePortPolicyClusterChain returns the name of the KUBE-SVC-XXXX chain for a service, which is the
// main iptables chain for that service, used for dispatching to endpoints when using `Cluster`
// traffic policy.
func servicePortPolicyClusterChain(servicePortName string, protocol string) utiliptables.Chain {
	return utiliptables.Chain(servicePortPolicyClusterChainNamePrefix + portProtoHash(servicePortName, protocol))
}

// servicePortPolicyLocalChainName returns the name of the KUBE-SVL-XXXX chain for a service, which
// handles dispatching to local endpoints when using `Local` traffic policy. This chain only
// exists if the service has `Local` internal or external traffic policy.
func servicePortPolicyLocalChainName(servicePortName string, protocol string) utiliptables.Chain {
	return utiliptables.Chain(servicePortPolicyLocalChainNamePrefix + portProtoHash(servicePortName, protocol))
}

// serviceFirewallChainName returns the name of the KUBE-FW-XXXX chain for a service, which
// is used to implement the filtering for the LoadBalancerSourceRanges feature.
func serviceFirewallChainName(servicePortName string, protocol string) utiliptables.Chain {
	return utiliptables.Chain(serviceFirewallChainNamePrefix + portProtoHash(servicePortName, protocol))
}

// serviceExternalChainName returns the name of the KUBE-EXT-XXXX chain for a service, which
// implements "short-circuiting" for internally-originated external-destination traffic when using
// `Local` external traffic policy.  It forwards traffic from local sources to the KUBE-SVC-XXXX
// chain and traffic from external sources to the KUBE-SVL-XXXX chain.
func serviceExternalChainName(servicePortName string, protocol string) utiliptables.Chain {
	return utiliptables.Chain(serviceExternalChainNamePrefix + portProtoHash(servicePortName, protocol))
}

// servicePortEndpointChainName returns the name of the KUBE-SEP-XXXX chain for a particular
// service endpoint.
func servicePortEndpointChainName(servicePortName string, protocol string, endpoint string) utiliptables.Chain {
	hash := sha256.Sum256([]byte(servicePortName + protocol + endpoint))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return utiliptables.Chain(servicePortEndpointChainNamePrefix + encoded[:16])
}

func isServiceChainName(chainString string) bool {
	prefixes := []string{
		servicePortPolicyClusterChainNamePrefix,
		servicePortPolicyLocalChainNamePrefix,
		servicePortEndpointChainNamePrefix,
		serviceFirewallChainNamePrefix,
		serviceExternalChainNamePrefix,
	}

	for _, p := range prefixes {
		if strings.HasPrefix(chainString, p) {
			return true
		}
	}
	return false
}

// Assumes proxier.mu is held.
func (proxier *Proxier) appendServiceCommentLocked(args []string, svcName string) []string {
	// Not printing these comments, can reduce size of iptables (in case of large
	// number of endpoints) even by 40%+. So if total number of endpoint chains
	// is large enough, we simply drop those comments.
	if proxier.largeClusterMode {
		return args
	}
	return append(args, "-m", "comment", "--comment", svcName)
}

// Called by the iptables.Monitor, and in response to topology changes; this calls
// syncProxyRules() and tells it to resync all services, regardless of whether the
// Service or Endpoints/EndpointSlice objects themselves have changed
func (proxier *Proxier) forceSyncProxyRules() {
	proxier.mu.Lock()
	proxier.needFullSync = true
	proxier.mu.Unlock()

	proxier.syncProxyRules()
}

// This is where all of the iptables-save/restore calls happen.
// The only other iptables rules are those that are setup in iptablesInit()
// This assumes proxier.mu is NOT held
func (proxier *Proxier) syncProxyRules() {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	// don't sync rules till we've received services and endpoints
	if !proxier.isInitialized() {
		proxier.logger.V(2).Info("Not syncing iptables until Services and Endpoints have been received from master")
		return
	}

	// Keep track of how long syncs take.
	start := time.Now()

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

	proxier.logger.V(2).Info("Syncing iptables rules", "fullSync", doFullSync)

	success := false
	defer func() {
		if !success {
			proxier.logger.Info("Sync failed", "retryingTime", proxier.syncPeriod)
			proxier.syncRunner.RetryAfter(proxier.syncPeriod)
			if !doFullSync {
				metrics.IPTablesPartialRestoreFailuresTotal.WithLabelValues(string(proxier.ipFamily)).Inc()
			}
			// proxier.serviceChanges and proxier.endpointChanges have already
			// been flushed, so we've lost the state needed to be able to do
			// a partial sync.
			proxier.needFullSync = true
		} else if doFullSync {
			proxier.lastFullSync = time.Now()
		}
	}()

	if doFullSync {
		// Ensure that our jump rules (eg from PREROUTING to KUBE-SERVICES) exist.
		// We can't do this as part of the iptables-restore because we don't want
		// to specify/replace *all* of the rules in PREROUTING, etc.
		//
		// We need to create these rules when kube-proxy first starts, and we need
		// to recreate them if the utiliptables Monitor detects that iptables has
		// been flushed. In both of those cases, the code will force a full sync.
		// In all other cases, it ought to be safe to assume that the rules
		// already exist, so we'll skip this step when doing a partial sync, to
		// save us from having to invoke /sbin/iptables 20 times on each sync
		// (which will be very slow on hosts with lots of iptables rules).
		for _, jump := range append(iptablesJumpChains, iptablesKubeletJumpChains...) {
			if _, err := proxier.iptables.EnsureChain(jump.table, jump.dstChain); err != nil {
				proxier.logger.Error(err, "Failed to ensure chain exists", "table", jump.table, "chain", jump.dstChain)
				return
			}
			args := jump.extraArgs
			if jump.comment != "" {
				args = append(args, "-m", "comment", "--comment", jump.comment)
			}
			args = append(args, "-j", string(jump.dstChain))
			if _, err := proxier.iptables.EnsureRule(utiliptables.Prepend, jump.table, jump.srcChain, args...); err != nil {
				proxier.logger.Error(err, "Failed to ensure chain jumps", "table", jump.table, "srcChain", jump.srcChain, "dstChain", jump.dstChain)
				return
			}
		}

		// ensure the nfacct counters
		if proxier.nfacct != nil {
			for name := range proxier.nfAcctCounters {
				if err := proxier.nfacct.Ensure(name); err != nil {
					proxier.nfAcctCounters[name] = false
					proxier.logger.Error(err, "Failed to create nfacct counter; the corresponding metric will not be updated", "counter", name)
				} else {
					proxier.nfAcctCounters[name] = true
				}
			}
		}
	}

	//
	// Below this point we will not return until we try to write the iptables rules.
	//

	// Reset all buffers used later.
	// This is to avoid memory reallocations and thus improve performance.
	proxier.filterChains.Reset()
	proxier.filterRules.Reset()
	proxier.natChains.Reset()
	proxier.natRules.Reset()

	skippedNatChains := proxyutil.NewDiscardLineBuffer()
	skippedNatRules := proxyutil.NewDiscardLineBuffer()

	// Write chain lines for all the "top-level" chains we'll be filling in
	for _, chainName := range []utiliptables.Chain{kubeServicesChain, kubeExternalServicesChain, kubeForwardChain, kubeNodePortsChain, kubeProxyFirewallChain} {
		proxier.filterChains.Write(utiliptables.MakeChainLine(chainName))
	}
	for _, chainName := range []utiliptables.Chain{kubeServicesChain, kubeNodePortsChain, kubePostroutingChain, kubeMarkMasqChain} {
		proxier.natChains.Write(utiliptables.MakeChainLine(chainName))
	}

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

	isIPv6 := proxier.iptables.IsIPv6()
	if !isIPv6 && proxier.localhostNodePorts {
		// Kube-proxy's use of `route_localnet` to enable NodePorts on localhost
		// creates a security hole (https://issue.k8s.io/90259) which this
		// iptables rule mitigates.

		// NOTE: kubelet creates an identical copy of this rule. If you want to
		// change this rule in the future, you MUST do so in a way that will
		// interoperate correctly with skewed versions of the rule created by
		// kubelet. (Actually, kubelet uses "--dst"/"--src" rather than "-d"/"-s"
		// but that's just a command-line thing and results in the same rule being
		// created in the kernel.)
		proxier.filterChains.Write(utiliptables.MakeChainLine(kubeletFirewallChain))
		proxier.filterRules.Write(
			"-A", string(kubeletFirewallChain),
			"-m", "comment", "--comment", `"block incoming localnet connections"`,
			"-d", "127.0.0.0/8",
			"!", "-s", "127.0.0.0/8",
			"-m", "conntrack",
			"!", "--ctstate", "RELATED,ESTABLISHED,DNAT",
			"-j", "DROP",
		)
	}

	// Accumulate NAT chains to keep.
	activeNATChains := sets.New[utiliptables.Chain]()

	// To avoid growing this slice, we arbitrarily set its size to 64,
	// there is never more than that many arguments for a single line.
	// Note that even if we go over 64, it will still be correct - it
	// is just for efficiency, not correctness.
	args := make([]string, 64)

	// Compute total number of endpoint chains across all services
	// to get a sense of how big the cluster is.
	totalEndpoints := 0
	for svcName := range proxier.svcPortMap {
		totalEndpoints += len(proxier.endpointsMap[svcName])
	}
	proxier.largeClusterMode = (totalEndpoints > largeClusterEndpointsThreshold)

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

		// clusterPolicyChain contains the endpoints used with "Cluster" traffic policy
		clusterPolicyChain := svcInfo.clusterPolicyChainName
		usesClusterPolicyChain := len(clusterEndpoints) > 0 && svcInfo.UsesClusterEndpoints()

		// localPolicyChain contains the endpoints used with "Local" traffic policy
		localPolicyChain := svcInfo.localPolicyChainName
		usesLocalPolicyChain := len(localEndpoints) > 0 && svcInfo.UsesLocalEndpoints()

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

		// Traffic to LoadBalancer IPs can go directly to externalTrafficChain
		// unless LoadBalancerSourceRanges is in use in which case we will
		// create a firewall chain.
		loadBalancerTrafficChain := externalTrafficChain
		fwChain := svcInfo.firewallChainName
		usesFWChain := hasEndpoints && len(svcInfo.LoadBalancerVIPs()) > 0 && len(svcInfo.LoadBalancerSourceRanges()) > 0
		if usesFWChain {
			loadBalancerTrafficChain = fwChain
		}

		var internalTrafficFilterTarget, internalTrafficFilterComment string
		var externalTrafficFilterTarget, externalTrafficFilterComment string
		if !hasEndpoints {
			// The service has no endpoints at all; hasInternalEndpoints and
			// hasExternalEndpoints will also be false, and we will not
			// generate any chains in the "nat" table for the service; only
			// rules in the "filter" table rejecting incoming packets for
			// the service's IPs.
			internalTrafficFilterTarget = "REJECT"
			internalTrafficFilterComment = fmt.Sprintf(`"%s has no endpoints"`, svcPortNameString)
			externalTrafficFilterTarget = "REJECT"
			externalTrafficFilterComment = internalTrafficFilterComment
		} else {
			if !hasInternalEndpoints {
				// The internalTrafficPolicy is "Local" but there are no local
				// endpoints. Traffic to the clusterIP will be dropped, but
				// external traffic may still be accepted.
				internalTrafficFilterTarget = "DROP"
				internalTrafficFilterComment = fmt.Sprintf(`"%s has no local endpoints"`, svcPortNameString)
				serviceNoLocalEndpointsTotalInternal++
			}
			if !hasExternalEndpoints {
				// The externalTrafficPolicy is "Local" but there are no
				// local endpoints. Traffic to "external" IPs from outside
				// the cluster will be dropped, but traffic from inside
				// the cluster may still be accepted.
				externalTrafficFilterTarget = "DROP"
				externalTrafficFilterComment = fmt.Sprintf(`"%s has no local endpoints"`, svcPortNameString)
				serviceNoLocalEndpointsTotalExternal++
			}
		}

		filterRules := proxier.filterRules
		natChains := proxier.natChains
		natRules := proxier.natRules

		// Capture the clusterIP.
		if hasInternalEndpoints {
			natRules.Write(
				"-A", string(kubeServicesChain),
				"-m", "comment", "--comment", fmt.Sprintf(`"%s cluster IP"`, svcPortNameString),
				"-m", protocol, "-p", protocol,
				"-d", svcInfo.ClusterIP().String(),
				"--dport", strconv.Itoa(svcInfo.Port()),
				"-j", string(internalTrafficChain))
		} else {
			// No endpoints.
			filterRules.Write(
				"-A", string(kubeServicesChain),
				"-m", "comment", "--comment", internalTrafficFilterComment,
				"-m", protocol, "-p", protocol,
				"-d", svcInfo.ClusterIP().String(),
				"--dport", strconv.Itoa(svcInfo.Port()),
				"-j", internalTrafficFilterTarget,
			)
		}

		// Capture externalIPs.
		for _, externalIP := range svcInfo.ExternalIPs() {
			if hasEndpoints {
				// Send traffic bound for external IPs to the "external
				// destinations" chain.
				natRules.Write(
					"-A", string(kubeServicesChain),
					"-m", "comment", "--comment", fmt.Sprintf(`"%s external IP"`, svcPortNameString),
					"-m", protocol, "-p", protocol,
					"-d", externalIP.String(),
					"--dport", strconv.Itoa(svcInfo.Port()),
					"-j", string(externalTrafficChain))
			}
			if !hasExternalEndpoints {
				// Either no endpoints at all (REJECT) or no endpoints for
				// external traffic (DROP anything that didn't get
				// short-circuited by the EXT chain.)
				filterRules.Write(
					"-A", string(kubeExternalServicesChain),
					"-m", "comment", "--comment", externalTrafficFilterComment,
					"-m", protocol, "-p", protocol,
					"-d", externalIP.String(),
					"--dport", strconv.Itoa(svcInfo.Port()),
					"-j", externalTrafficFilterTarget,
				)
			}
		}

		// Capture load-balancer ingress.
		for _, lbip := range svcInfo.LoadBalancerVIPs() {
			if hasEndpoints {
				natRules.Write(
					"-A", string(kubeServicesChain),
					"-m", "comment", "--comment", fmt.Sprintf(`"%s loadbalancer IP"`, svcPortNameString),
					"-m", protocol, "-p", protocol,
					"-d", lbip.String(),
					"--dport", strconv.Itoa(svcInfo.Port()),
					"-j", string(loadBalancerTrafficChain))

			}
			if usesFWChain {
				filterRules.Write(
					"-A", string(kubeProxyFirewallChain),
					"-m", "comment", "--comment", fmt.Sprintf(`"%s traffic not accepted by %s"`, svcPortNameString, svcInfo.firewallChainName),
					"-m", protocol, "-p", protocol,
					"-d", lbip.String(),
					"--dport", strconv.Itoa(svcInfo.Port()),
					"-j", "DROP")
			}
		}
		if !hasExternalEndpoints {
			// Either no endpoints at all (REJECT) or no endpoints for
			// external traffic (DROP anything that didn't get short-circuited
			// by the EXT chain.)
			for _, lbip := range svcInfo.LoadBalancerVIPs() {
				filterRules.Write(
					"-A", string(kubeExternalServicesChain),
					"-m", "comment", "--comment", externalTrafficFilterComment,
					"-m", protocol, "-p", protocol,
					"-d", lbip.String(),
					"--dport", strconv.Itoa(svcInfo.Port()),
					"-j", externalTrafficFilterTarget,
				)
			}
		}

		// Capture nodeports.
		if svcInfo.NodePort() != 0 {
			if hasEndpoints {
				// Jump to the external destination chain.  For better or for
				// worse, nodeports are not subect to loadBalancerSourceRanges,
				// and we can't change that.
				if proxier.localhostNodePorts && proxier.ipFamily == v1.IPv4Protocol && proxier.nfAcctCounters[metrics.LocalhostNodePortAcceptedNFAcctCounter] {
					natRules.Write(
						"-A", string(kubeNodePortsChain),
						"-m", "comment", "--comment", svcPortNameString,
						"-m", protocol, "-p", protocol,
						"-d", "127.0.0.0/8",
						"--dport", strconv.Itoa(svcInfo.NodePort()),
						"-m", "nfacct", "--nfacct-name", metrics.LocalhostNodePortAcceptedNFAcctCounter,
						"-j", string(externalTrafficChain))
				}
				natRules.Write(
					"-A", string(kubeNodePortsChain),
					"-m", "comment", "--comment", svcPortNameString,
					"-m", protocol, "-p", protocol,
					"--dport", strconv.Itoa(svcInfo.NodePort()),
					"-j", string(externalTrafficChain))
			}
			if !hasExternalEndpoints {
				// Either no endpoints at all (REJECT) or no endpoints for
				// external traffic (DROP anything that didn't get
				// short-circuited by the EXT chain.)
				filterRules.Write(
					"-A", string(kubeExternalServicesChain),
					"-m", "comment", "--comment", externalTrafficFilterComment,
					"-m", "addrtype", "--dst-type", "LOCAL",
					"-m", protocol, "-p", protocol,
					"--dport", strconv.Itoa(svcInfo.NodePort()),
					"-j", externalTrafficFilterTarget,
				)
			}
		}

		// Capture healthCheckNodePorts.
		if svcInfo.HealthCheckNodePort() != 0 {
			// no matter if node has local endpoints, healthCheckNodePorts
			// need to add a rule to accept the incoming connection
			filterRules.Write(
				"-A", string(kubeNodePortsChain),
				"-m", "comment", "--comment", fmt.Sprintf(`"%s health check node port"`, svcPortNameString),
				"-m", "tcp", "-p", "tcp",
				"--dport", strconv.Itoa(svcInfo.HealthCheckNodePort()),
				"-j", "ACCEPT",
			)
		}

		// If the SVC/SVL/EXT/FW/SEP chains have not changed since the last sync
		// then we can omit them from the restore input. However, we have to still
		// figure out how many chains we _would_ have written, to make the metrics
		// come out right, so we just compute them and throw them away.
		if !doFullSync && !serviceUpdateResult.UpdatedServices.Has(svcName.NamespacedName) && !endpointUpdateResult.UpdatedServices.Has(svcName.NamespacedName) {
			natChains = skippedNatChains
			natRules = skippedNatRules
		}

		// Set up internal traffic handling.
		if hasInternalEndpoints {
			args = append(args[:0],
				"-m", "comment", "--comment", fmt.Sprintf(`"%s cluster IP"`, svcPortNameString),
				"-m", protocol, "-p", protocol,
				"-d", svcInfo.ClusterIP().String(),
				"--dport", strconv.Itoa(svcInfo.Port()),
			)
			if proxier.masqueradeAll {
				natRules.Write(
					"-A", string(internalTrafficChain),
					args,
					"-j", string(kubeMarkMasqChain))
			} else if proxier.localDetector.IsImplemented() {
				// This masquerades off-cluster traffic to a service VIP. The
				// idea is that you can establish a static route for your
				// Service range, routing to any node, and that node will
				// bridge into the Service for you. Since that might bounce
				// off-node, we masquerade here.
				natRules.Write(
					"-A", string(internalTrafficChain),
					args,
					proxier.localDetector.IfNotLocal(),
					"-j", string(kubeMarkMasqChain))
			}
		}

		// Set up external traffic handling (if any "external" destinations are
		// enabled). All captured traffic for all external destinations should
		// jump to externalTrafficChain, which will handle some special cases and
		// then jump to externalPolicyChain.
		if usesExternalTrafficChain {
			natChains.Write(utiliptables.MakeChainLine(externalTrafficChain))
			activeNATChains.Insert(externalTrafficChain)

			if !svcInfo.ExternalPolicyLocal() {
				// If we are using non-local endpoints we need to masquerade,
				// in case we cross nodes.
				natRules.Write(
					"-A", string(externalTrafficChain),
					"-m", "comment", "--comment", fmt.Sprintf(`"masquerade traffic for %s external destinations"`, svcPortNameString),
					"-j", string(kubeMarkMasqChain))
			} else {
				// If we are only using same-node endpoints, we can retain the
				// source IP in most cases.

				if proxier.localDetector.IsImplemented() {
					// Treat all locally-originated pod -> external destination
					// traffic as a special-case.  It is subject to neither
					// form of traffic policy, which simulates going up-and-out
					// to an external load-balancer and coming back in.
					natRules.Write(
						"-A", string(externalTrafficChain),
						"-m", "comment", "--comment", fmt.Sprintf(`"pod traffic for %s external destinations"`, svcPortNameString),
						proxier.localDetector.IfLocal(),
						"-j", string(clusterPolicyChain))
				}

				// Locally originated traffic (not a pod, but the host node)
				// still needs masquerade because the LBIP itself is a local
				// address, so that will be the chosen source IP.
				natRules.Write(
					"-A", string(externalTrafficChain),
					"-m", "comment", "--comment", fmt.Sprintf(`"masquerade LOCAL traffic for %s external destinations"`, svcPortNameString),
					"-m", "addrtype", "--src-type", "LOCAL",
					"-j", string(kubeMarkMasqChain))

				// Redirect all src-type=LOCAL -> external destination to the
				// policy=cluster chain. This allows traffic originating
				// from the host to be redirected to the service correctly.
				natRules.Write(
					"-A", string(externalTrafficChain),
					"-m", "comment", "--comment", fmt.Sprintf(`"route LOCAL traffic for %s external destinations"`, svcPortNameString),
					"-m", "addrtype", "--src-type", "LOCAL",
					"-j", string(clusterPolicyChain))
			}

			// Anything else falls thru to the appropriate policy chain.
			if hasExternalEndpoints {
				natRules.Write(
					"-A", string(externalTrafficChain),
					"-j", string(externalPolicyChain))
			}
		}

		// Set up firewall chain, if needed
		if usesFWChain {
			natChains.Write(utiliptables.MakeChainLine(fwChain))
			activeNATChains.Insert(fwChain)

			// The service firewall rules are created based on the
			// loadBalancerSourceRanges field. This only works for VIP-like
			// loadbalancers that preserve source IPs. For loadbalancers which
			// direct traffic to service NodePort, the firewall rules will not
			// apply.
			args = append(args[:0],
				"-A", string(fwChain),
				"-m", "comment", "--comment", fmt.Sprintf(`"%s loadbalancer IP"`, svcPortNameString),
			)

			// firewall filter based on each source range
			allowFromNode := false
			for _, cidr := range svcInfo.LoadBalancerSourceRanges() {
				natRules.Write(args, "-s", cidr.String(), "-j", string(externalTrafficChain))
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
					natRules.Write(
						args,
						"-s", lbip.String(),
						"-j", string(externalTrafficChain))
				}
			}
			// If the packet was able to reach the end of firewall chain,
			// then it did not get DNATed, so it will match the
			// corresponding KUBE-PROXY-FIREWALL rule.
			natRules.Write(
				"-A", string(fwChain),
				"-m", "comment", "--comment", fmt.Sprintf(`"other traffic to %s will be dropped by KUBE-PROXY-FIREWALL"`, svcPortNameString),
			)
		}

		// If Cluster policy is in use, create the chain and create rules jumping
		// from clusterPolicyChain to the clusterEndpoints
		if usesClusterPolicyChain {
			natChains.Write(utiliptables.MakeChainLine(clusterPolicyChain))
			activeNATChains.Insert(clusterPolicyChain)
			proxier.writeServiceToEndpointRules(natRules, svcPortNameString, svcInfo, clusterPolicyChain, clusterEndpoints, args)
		}

		// If Local policy is in use, create the chain and create rules jumping
		// from localPolicyChain to the localEndpoints
		if usesLocalPolicyChain {
			natChains.Write(utiliptables.MakeChainLine(localPolicyChain))
			activeNATChains.Insert(localPolicyChain)
			proxier.writeServiceToEndpointRules(natRules, svcPortNameString, svcInfo, localPolicyChain, localEndpoints, args)
		}

		// Generate the per-endpoint chains.
		for _, ep := range allLocallyReachableEndpoints {
			epInfo, ok := ep.(*endpointInfo)
			if !ok {
				proxier.logger.Error(nil, "Failed to cast endpointInfo", "endpointInfo", ep)
				continue
			}

			endpointChain := epInfo.ChainName

			// Create the endpoint chain
			natChains.Write(utiliptables.MakeChainLine(endpointChain))
			activeNATChains.Insert(endpointChain)

			args = append(args[:0], "-A", string(endpointChain))
			args = proxier.appendServiceCommentLocked(args, svcPortNameString)
			// Handle traffic that loops back to the originator with SNAT.
			natRules.Write(
				args,
				"-s", epInfo.IP(),
				"-j", string(kubeMarkMasqChain))
			// Update client-affinity lists.
			if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
				args = append(args, "-m", "recent", "--name", string(endpointChain), "--set")
			}
			// DNAT to final destination.
			args = append(args, "-m", protocol, "-p", protocol, "-j", "DNAT", "--to-destination", epInfo.String())
			natRules.Write(args)
		}
	}

	// Delete chains no longer in use. Since "iptables-save" can take several seconds
	// to run on hosts with lots of iptables rules, we don't bother to do this on
	// every sync in large clusters. (Stale chains will not be referenced by any
	// active rules, so they're harmless other than taking up memory.)
	deletedChains := 0
	if !proxier.largeClusterMode || time.Since(proxier.lastIPTablesCleanup) > proxier.syncPeriod {
		proxier.iptablesData.Reset()
		if err := proxier.iptables.SaveInto(utiliptables.TableNAT, proxier.iptablesData); err == nil {
			existingNATChains := utiliptables.GetChainsFromTable(proxier.iptablesData.Bytes())
			for chain := range existingNATChains.Difference(activeNATChains) {
				chainString := string(chain)
				if !isServiceChainName(chainString) {
					// Ignore chains that aren't ours.
					continue
				}
				// We must (as per iptables) write a chain-line
				// for it, which has the nice effect of flushing
				// the chain. Then we can remove the chain.
				proxier.natChains.Write(utiliptables.MakeChainLine(chain))
				proxier.natRules.Write("-X", chainString)
				deletedChains++
			}
			proxier.lastIPTablesCleanup = time.Now()
		} else {
			proxier.logger.Error(err, "Failed to execute iptables-save: stale chains will not be deleted")
		}
	}

	// Finally, tail-call to the nodePorts chain.  This needs to be after all
	// other service portal rules.
	if proxier.nodePortAddresses.MatchAll() {
		destinations := []string{"-m", "addrtype", "--dst-type", "LOCAL"}
		// Block localhost nodePorts if they are not supported. (For IPv6 they never
		// work, and for IPv4 they only work if we previously set `route_localnet`.)
		if isIPv6 {
			destinations = append(destinations, "!", "-d", "::1/128")
		} else if !proxier.localhostNodePorts {
			destinations = append(destinations, "!", "-d", "127.0.0.0/8")
		}

		proxier.natRules.Write(
			"-A", string(kubeServicesChain),
			"-m", "comment", "--comment", `"kubernetes service nodeports; NOTE: this must be the last rule in this chain"`,
			destinations,
			"-j", string(kubeNodePortsChain))
	} else {
		nodeIPs, err := proxier.nodePortAddresses.GetNodeIPs(proxier.networkInterfacer)
		if err != nil {
			proxier.logger.Error(err, "Failed to get node ip address matching nodeport cidrs, services with nodeport may not work as intended", "CIDRs", proxier.nodePortAddresses)
		}
		for _, ip := range nodeIPs {
			if ip.IsLoopback() {
				if isIPv6 {
					proxier.logger.Error(nil, "--nodeport-addresses includes localhost but localhost NodePorts are not supported on IPv6", "address", ip.String())
					continue
				} else if !proxier.localhostNodePorts {
					proxier.logger.Error(nil, "--nodeport-addresses includes localhost but --iptables-localhost-nodeports=false was passed", "address", ip.String())
					continue
				}
			}

			// create nodeport rules for each IP one by one
			proxier.natRules.Write(
				"-A", string(kubeServicesChain),
				"-m", "comment", "--comment", `"kubernetes service nodeports; NOTE: this must be the last rule in this chain"`,
				"-d", ip.String(),
				"-j", string(kubeNodePortsChain))
		}
	}

	// Drop the packets in INVALID state, which would potentially cause
	// unexpected connection reset if nf_conntrack_tcp_be_liberal is not set.
	// Ref: https://github.com/kubernetes/kubernetes/issues/74839
	// Ref: https://github.com/kubernetes/kubernetes/issues/117924
	if !proxier.conntrackTCPLiberal {
		rule := []string{
			"-A", string(kubeForwardChain),
			"-m", "conntrack",
			"--ctstate", "INVALID",
		}
		if proxier.nfAcctCounters[metrics.IPTablesCTStateInvalidDroppedNFAcctCounter] {
			rule = append(rule,
				"-m", "nfacct", "--nfacct-name", metrics.IPTablesCTStateInvalidDroppedNFAcctCounter,
			)
		}
		rule = append(rule,
			"-j", "DROP",
		)
		proxier.filterRules.Write(rule)
	}

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

	metrics.IPTablesRulesTotal.WithLabelValues(string(utiliptables.TableFilter), string(proxier.ipFamily)).Set(float64(proxier.filterRules.Lines()))
	metrics.IPTablesRulesLastSync.WithLabelValues(string(utiliptables.TableFilter), string(proxier.ipFamily)).Set(float64(proxier.filterRules.Lines()))
	metrics.IPTablesRulesTotal.WithLabelValues(string(utiliptables.TableNAT), string(proxier.ipFamily)).Set(float64(proxier.natRules.Lines() + skippedNatRules.Lines() - deletedChains))
	metrics.IPTablesRulesLastSync.WithLabelValues(string(utiliptables.TableNAT), string(proxier.ipFamily)).Set(float64(proxier.natRules.Lines() - deletedChains))

	// Sync rules.
	proxier.iptablesData.Reset()
	proxier.iptablesData.WriteString("*filter\n")
	proxier.iptablesData.Write(proxier.filterChains.Bytes())
	proxier.iptablesData.Write(proxier.filterRules.Bytes())
	proxier.iptablesData.WriteString("COMMIT\n")
	proxier.iptablesData.WriteString("*nat\n")
	proxier.iptablesData.Write(proxier.natChains.Bytes())
	proxier.iptablesData.Write(proxier.natRules.Bytes())
	proxier.iptablesData.WriteString("COMMIT\n")

	proxier.logger.V(2).Info("Reloading service iptables data",
		"numServices", len(proxier.svcPortMap),
		"numEndpoints", totalEndpoints,
		"numFilterChains", proxier.filterChains.Lines(),
		"numFilterRules", proxier.filterRules.Lines(),
		"numNATChains", proxier.natChains.Lines(),
		"numNATRules", proxier.natRules.Lines(),
	)
	proxier.logger.V(9).Info("Restoring iptables", "rules", proxier.iptablesData.Bytes())

	// NOTE: NoFlushTables is used so we don't flush non-kubernetes chains in the table
	err := proxier.iptables.RestoreAll(proxier.iptablesData.Bytes(), utiliptables.NoFlushTables, utiliptables.RestoreCounters)
	if err != nil {
		if pErr, ok := err.(utiliptables.ParseError); ok {
			lines := utiliptables.ExtractLines(proxier.iptablesData.Bytes(), pErr.Line(), 3)
			proxier.logger.Error(pErr, "Failed to execute iptables-restore", "rules", lines)
		} else {
			proxier.logger.Error(err, "Failed to execute iptables-restore")
		}
		metrics.IPTablesRestoreFailuresTotal.WithLabelValues(string(proxier.ipFamily)).Inc()
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

func (proxier *Proxier) writeServiceToEndpointRules(natRules proxyutil.LineBuffer, svcPortNameString string, svcInfo proxy.ServicePort, svcChain utiliptables.Chain, endpoints []proxy.Endpoint, args []string) {
	// First write session affinity rules, if applicable.
	if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
		for _, ep := range endpoints {
			epInfo, ok := ep.(*endpointInfo)
			if !ok {
				continue
			}
			comment := fmt.Sprintf(`"%s -> %s"`, svcPortNameString, epInfo.String())

			args = append(args[:0],
				"-A", string(svcChain),
			)
			args = proxier.appendServiceCommentLocked(args, comment)
			args = append(args,
				"-m", "recent", "--name", string(epInfo.ChainName),
				"--rcheck", "--seconds", strconv.Itoa(svcInfo.StickyMaxAgeSeconds()), "--reap",
				"-j", string(epInfo.ChainName),
			)
			natRules.Write(args)
		}
	}

	// Now write loadbalancing rules.
	numEndpoints := len(endpoints)
	for i, ep := range endpoints {
		epInfo, ok := ep.(*endpointInfo)
		if !ok {
			continue
		}
		comment := fmt.Sprintf(`"%s -> %s"`, svcPortNameString, epInfo.String())

		args = append(args[:0], "-A", string(svcChain))
		args = proxier.appendServiceCommentLocked(args, comment)
		if i < (numEndpoints - 1) {
			// Each rule is a probabilistic match.
			args = append(args,
				"-m", "statistic",
				"--mode", "random",
				"--probability", proxier.probability(numEndpoints-i))
		}
		// The final (or only if n == 1) rule is a guaranteed match.
		natRules.Write(args, "-j", string(epInfo.ChainName))
	}
}
