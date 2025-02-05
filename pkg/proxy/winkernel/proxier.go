//go:build windows
// +build windows

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

package winkernel

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Microsoft/hnslib"
	"github.com/Microsoft/hnslib/hcn"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiutil "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/apis/config"
	proxyconfig "k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	"k8s.io/kubernetes/pkg/proxy/metaproxier"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/async"
	netutils "k8s.io/utils/net"
)

// KernelCompatTester tests whether the required kernel capabilities are
// present to run the windows kernel proxier.
type KernelCompatTester interface {
	IsCompatible() error
}

// CanUseWinKernelProxier returns true if we should use the Kernel Proxier
// instead of the "classic" userspace Proxier.  This is determined by checking
// the windows kernel version and for the existence of kernel features.
func CanUseWinKernelProxier(kcompat KernelCompatTester) (bool, error) {
	// Check that the kernel supports what we need.
	if err := kcompat.IsCompatible(); err != nil {
		return false, err
	}
	return true, nil
}

type WindowsKernelCompatTester struct{}

// IsCompatible returns true if winkernel can support this mode of proxy
func (lkct WindowsKernelCompatTester) IsCompatible() error {
	_, err := hnslib.HNSListPolicyListRequest()
	if err != nil {
		return fmt.Errorf("Windows kernel is not compatible for Kernel mode")
	}
	return nil
}

type externalIPInfo struct {
	ip    string
	hnsID string
}

type loadBalancerIngressInfo struct {
	ip               string
	hnsID            string
	healthCheckHnsID string
}

type loadBalancerInfo struct {
	hnsID string
}

type loadBalancerIdentifier struct {
	protocol      uint16
	internalPort  uint16
	externalPort  uint16
	vip           string
	endpointsHash [20]byte
}

type loadBalancerFlags struct {
	isILB           bool
	isDSR           bool
	isVipExternalIP bool
	localRoutedVIP  bool
	useMUX          bool
	preserveDIP     bool
	sessionAffinity bool
	isIPv6          bool
}

// internal struct for string service information
type serviceInfo struct {
	*proxy.BaseServicePortInfo
	targetPort             int
	externalIPs            []*externalIPInfo
	loadBalancerIngressIPs []*loadBalancerIngressInfo
	hnsID                  string
	nodePorthnsID          string
	policyApplied          bool
	remoteEndpoint         *endpointInfo
	hns                    HostNetworkService
	preserveDIP            bool
	localTrafficDSR        bool
	internalTrafficLocal   bool
	winProxyOptimization   bool
}

type hnsNetworkInfo struct {
	name          string
	id            string
	networkType   string
	remoteSubnets []*remoteSubnetInfo
}

type remoteSubnetInfo struct {
	destinationPrefix string
	isolationID       uint16
	providerAddress   string
	drMacAddress      string
}

const (
	SOURCE_VIP_NAME      = "source_vip" // The well-known, documented name for the HNS endpoint used as the Source Vip when using overlay network mode
	NETWORK_TYPE_OVERLAY = "overlay"
	// MAX_COUNT_STALE_LOADBALANCERS is the maximum number of stale loadbalancers which cleanedup in single syncproxyrules.
	// If there are more stale loadbalancers to clean, it will go to next iteration of syncproxyrules.
	MAX_COUNT_STALE_LOADBALANCERS = 20
)

func newHostNetworkService(hcnImpl HcnService) (HostNetworkService, hcn.SupportedFeatures) {
	var h HostNetworkService
	supportedFeatures := hcnImpl.GetSupportedFeatures()
	klog.V(3).InfoS("HNS Supported features", "hnsSupportedFeatures", supportedFeatures)
	if supportedFeatures.Api.V2 {
		h = hns{
			hcn: hcnImpl,
		}
	} else {
		panic("Windows HNS Api V2 required. This version of windows does not support API V2")
	}
	return h, supportedFeatures
}

// logFormattedEndpoints will log all endpoints and its states which are taking part in endpointmap change.
// This mostly for debugging purpose and verbosity is set to 5.
func logFormattedEndpoints(logMsg string, logLevel klog.Level, svcPortName proxy.ServicePortName, eps []proxy.Endpoint) {
	if klog.V(logLevel).Enabled() {
		var epInfo string
		for _, v := range eps {
			epInfo = epInfo + fmt.Sprintf("\n  %s={Ready:%v,Serving:%v,Terminating:%v,IsRemote:%v}", v.String(), v.IsReady(), v.IsServing(), v.IsTerminating(), !v.IsLocal())
		}
		klog.V(logLevel).InfoS(logMsg, "svcPortName", svcPortName, "endpoints", epInfo)
	}
}

// This will cleanup stale load balancers which are pending delete
// in last iteration. This function will act like a self healing of stale
// loadbalancer entries.
func (proxier *Proxier) cleanupStaleLoadbalancers() {
	i := 0
	countStaleLB := len(proxier.mapStaleLoadbalancers)
	if countStaleLB == 0 {
		return
	}
	klog.V(3).InfoS("Cleanup of stale loadbalancers triggered", "LB Count", countStaleLB)
	for lbID := range proxier.mapStaleLoadbalancers {
		i++
		if err := proxier.hns.deleteLoadBalancer(lbID); err == nil {
			delete(proxier.mapStaleLoadbalancers, lbID)
		}
		if i == MAX_COUNT_STALE_LOADBALANCERS {
			// The remaining stale loadbalancers will be cleaned up in next iteration
			break
		}
	}
	countStaleLB = len(proxier.mapStaleLoadbalancers)
	if countStaleLB > 0 {
		klog.V(3).InfoS("Stale loadbalancers still remaining", "LB Count", countStaleLB, "stale_lb_ids", proxier.mapStaleLoadbalancers)
	}
}

func getNetworkName(hnsNetworkName string) (string, error) {
	if len(hnsNetworkName) == 0 {
		klog.V(3).InfoS("Flag --network-name not set, checking environment variable")
		hnsNetworkName = os.Getenv("KUBE_NETWORK")
		if len(hnsNetworkName) == 0 {
			return "", fmt.Errorf("Environment variable KUBE_NETWORK and network-flag not initialized")
		}
	}
	return hnsNetworkName, nil
}

func getNetworkInfo(hns HostNetworkService, hnsNetworkName string) (*hnsNetworkInfo, error) {
	hnsNetworkInfo, err := hns.getNetworkByName(hnsNetworkName)
	for err != nil {
		klog.ErrorS(err, "Unable to find HNS Network specified, please check network name and CNI deployment", "hnsNetworkName", hnsNetworkName)
		time.Sleep(1 * time.Second)
		hnsNetworkInfo, err = hns.getNetworkByName(hnsNetworkName)
	}
	return hnsNetworkInfo, err
}

func isOverlay(hnsNetworkInfo *hnsNetworkInfo) bool {
	return strings.EqualFold(hnsNetworkInfo.networkType, NETWORK_TYPE_OVERLAY)
}

// StackCompatTester tests whether the required kernel and network are dualstack capable
type StackCompatTester interface {
	DualStackCompatible(networkName string) bool
}

type DualStackCompatTester struct{}

func (t DualStackCompatTester) DualStackCompatible(networkName string) bool {
	hcnImpl := newHcnImpl()
	// First tag of hnslib that has a proper check for dual stack support is v0.8.22 due to a bug.
	if err := hcnImpl.Ipv6DualStackSupported(); err != nil {
		// Hcn *can* fail the query to grab the version of hcn itself (which this call will do internally before parsing
		// to see if dual stack is supported), but the only time this can happen, at least that can be discerned, is if the host
		// is pre-1803 and hcn didn't exist. hnslib should truthfully return a known error if this happened that we can
		// check against, and the case where 'err != this known error' would be the 'this feature isn't supported' case, as is being
		// used here. For now, seeming as how nothing before ws2019 (1809) is listed as supported for k8s we can pretty much assume
		// any error here isn't because the query failed, it's just that dualstack simply isn't supported on the host. With all
		// that in mind, just log as info and not error to let the user know we're falling back.
		klog.InfoS("This version of Windows does not support dual-stack, falling back to single-stack", "err", err.Error())
		return false
	}

	// check if network is using overlay
	hns, _ := newHostNetworkService(hcnImpl)
	networkName, err := getNetworkName(networkName)
	if err != nil {
		klog.ErrorS(err, "Unable to determine dual-stack status, falling back to single-stack")
		return false
	}
	networkInfo, err := getNetworkInfo(hns, networkName)
	if err != nil {
		klog.ErrorS(err, "Unable to determine dual-stack status, falling back to single-stack")
		return false
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.WinOverlay) && isOverlay(networkInfo) {
		// Overlay (VXLAN) networks on Windows do not support dual-stack networking today
		klog.InfoS("Winoverlay does not support dual-stack, falling back to single-stack")
		return false
	}

	return true
}

// internal struct for endpoints information
type endpointInfo struct {
	ip              string
	port            uint16
	isLocal         bool
	macAddress      string
	hnsID           string
	refCount        *uint16
	providerAddress string
	hns             HostNetworkService

	// conditions
	ready       bool
	serving     bool
	terminating bool
}

// String is part of proxy.Endpoint interface.
func (info *endpointInfo) String() string {
	return net.JoinHostPort(info.ip, strconv.Itoa(int(info.port)))
}

// IsLocal is part of proxy.Endpoint interface.
func (info *endpointInfo) IsLocal() bool {
	return info.isLocal
}

// IsReady returns true if an endpoint is ready and not terminating.
func (info *endpointInfo) IsReady() bool {
	return info.ready
}

// IsServing returns true if an endpoint is ready, regardless of it's terminating state.
func (info *endpointInfo) IsServing() bool {
	return info.serving
}

// IsTerminating returns true if an endpoint is terminating.
func (info *endpointInfo) IsTerminating() bool {
	return info.terminating
}

// ZoneHints returns the zone hints for the endpoint.
func (info *endpointInfo) ZoneHints() sets.Set[string] {
	return sets.Set[string]{}
}

// IP returns just the IP part of the endpoint, it's a part of proxy.Endpoint interface.
func (info *endpointInfo) IP() string {
	return info.ip
}

// Port returns just the Port part of the endpoint.
func (info *endpointInfo) Port() int {
	return int(info.port)
}

// Uses mac prefix and IPv4 address to return a mac address
// This ensures mac addresses are unique for proper load balancing
// There is a possibility of MAC collisions but this Mac address is used for remote endpoints only
// and not sent on the wire.
func conjureMac(macPrefix string, ip net.IP) string {
	if ip4 := ip.To4(); ip4 != nil {
		a, b, c, d := ip4[0], ip4[1], ip4[2], ip4[3]
		return fmt.Sprintf("%v-%02x-%02x-%02x-%02x", macPrefix, a, b, c, d)
	} else if ip6 := ip.To16(); ip6 != nil {
		a, b, c, d := ip6[15], ip6[14], ip6[13], ip6[12]
		return fmt.Sprintf("%v-%02x-%02x-%02x-%02x", macPrefix, a, b, c, d)
	}
	return "02-11-22-33-44-55"
}

// This will keep the track of all terminated endpoints.
// This is done by adding the endpoints from old endpoint map and removing the endpoints from new endpoint map.
// This way, we have entries which are only present in old endpoint map and not in new endpoint map.
func (proxier *Proxier) updateTerminatedEndpoints(eps []proxy.Endpoint, isOldEndpointsMap bool) {
	for _, ep := range eps {
		if !ep.IsLocal() {
			if isOldEndpointsMap {
				proxier.terminatedEndpoints[ep.IP()] = true
			} else {
				delete(proxier.terminatedEndpoints, ep.IP())
			}
		}
	}
}

func (proxier *Proxier) endpointsMapChange(oldEndpointsMap, newEndpointsMap proxy.EndpointsMap) {
	// This will optimize remote endpoint and loadbalancer deletion based on the annotation
	var svcPortMap = make(map[proxy.ServicePortName]bool)
	clear(proxier.terminatedEndpoints)
	var logLevel klog.Level = 5
	for svcPortName, eps := range oldEndpointsMap {
		logFormattedEndpoints("endpointsMapChange oldEndpointsMap", logLevel, svcPortName, eps)
		svcPortMap[svcPortName] = true
		proxier.updateTerminatedEndpoints(eps, true)
		proxier.onEndpointsMapChange(&svcPortName, false)
	}

	for svcPortName, eps := range newEndpointsMap {
		logFormattedEndpoints("endpointsMapChange newEndpointsMap", logLevel, svcPortName, eps)
		// redundantCleanup true means cleanup is called second time on the same svcPort
		proxier.updateTerminatedEndpoints(eps, false)
		redundantCleanup := svcPortMap[svcPortName]
		proxier.onEndpointsMapChange(&svcPortName, redundantCleanup)
	}
}

func (proxier *Proxier) onEndpointsMapChange(svcPortName *proxy.ServicePortName, redundantCleanup bool) {

	svc, exists := proxier.svcPortMap[*svcPortName]

	if exists {
		svcInfo, ok := svc.(*serviceInfo)

		if !ok {
			klog.ErrorS(nil, "Failed to cast serviceInfo", "servicePortName", svcPortName)
			return
		}

		if svcInfo.winProxyOptimization && redundantCleanup {
			// This is a second cleanup call.
			// Second cleanup on the same svcPort will be ignored if the
			// winProxyOptimization is Enabled
			return
		}

		klog.V(3).InfoS("Endpoints are modified. Service is stale", "servicePortName", svcPortName)
		svcInfo.cleanupAllPolicies(proxier.endpointsMap[*svcPortName], proxier.mapStaleLoadbalancers, true)
	} else {
		// If no service exists, just cleanup the remote endpoints
		klog.V(3).InfoS("Endpoints are orphaned, cleaning up")
		// Cleanup Endpoints references
		epInfos, exists := proxier.endpointsMap[*svcPortName]

		if exists {
			// Cleanup Endpoints references
			for _, ep := range epInfos {
				epInfo, ok := ep.(*endpointInfo)

				if ok {
					epInfo.Cleanup()
				}

			}
		}
	}
}

func (proxier *Proxier) serviceMapChange(previous, current proxy.ServicePortMap) {
	for svcPortName := range current {
		proxier.onServiceMapChange(&svcPortName)
	}

	for svcPortName := range previous {
		if _, ok := current[svcPortName]; ok {
			continue
		}
		proxier.onServiceMapChange(&svcPortName)
	}
}

func (proxier *Proxier) onServiceMapChange(svcPortName *proxy.ServicePortName) {

	svc, exists := proxier.svcPortMap[*svcPortName]

	if exists {
		svcInfo, ok := svc.(*serviceInfo)

		if !ok {
			klog.ErrorS(nil, "Failed to cast serviceInfo", "servicePortName", svcPortName)
			return
		}

		klog.V(3).InfoS("Updating existing service port", "servicePortName", svcPortName, "clusterIP", svcInfo.ClusterIP(), "port", svcInfo.Port(), "protocol", svcInfo.Protocol())
		svcInfo.cleanupAllPolicies(proxier.endpointsMap[*svcPortName], proxier.mapStaleLoadbalancers, false)
	}
}

// returns a new proxy.Endpoint which abstracts a endpointInfo
func (proxier *Proxier) newEndpointInfo(baseInfo *proxy.BaseEndpointInfo, _ *proxy.ServicePortName) proxy.Endpoint {

	info := &endpointInfo{
		ip:         baseInfo.IP(),
		port:       uint16(baseInfo.Port()),
		isLocal:    baseInfo.IsLocal(),
		macAddress: conjureMac("02-11", netutils.ParseIPSloppy(baseInfo.IP())),
		refCount:   new(uint16),
		hnsID:      "",
		hns:        proxier.hns,

		ready:       baseInfo.IsReady(),
		serving:     baseInfo.IsServing(),
		terminating: baseInfo.IsTerminating(),
	}

	return info
}

func newSourceVIP(hns HostNetworkService, network string, ip string, mac string, providerAddress string) (*endpointInfo, error) {
	hnsEndpoint := &endpointInfo{
		ip:              ip,
		isLocal:         true,
		macAddress:      mac,
		providerAddress: providerAddress,

		ready:       true,
		serving:     true,
		terminating: false,
	}
	ep, err := hns.createEndpoint(hnsEndpoint, network)
	return ep, err
}

func (ep *endpointInfo) DecrementRefCount() {
	klog.V(3).InfoS("Decrementing Endpoint RefCount", "endpointInfo", ep)
	if !ep.IsLocal() && ep.refCount != nil && *ep.refCount > 0 {
		*ep.refCount--
	}
}

func (ep *endpointInfo) Cleanup() {
	klog.V(3).InfoS("Endpoint cleanup", "endpointInfo", ep)
	if !ep.IsLocal() && ep.refCount != nil {
		*ep.refCount--

		// Remove the remote hns endpoint, if no service is referring it
		// Never delete a Local Endpoint. Local Endpoints are already created by other entities.
		// Remove only remote endpoints created by this service
		if *ep.refCount <= 0 && !ep.IsLocal() {
			klog.V(4).InfoS("Removing endpoints, since no one is referencing it", "endpoint", ep)
			err := ep.hns.deleteEndpoint(ep.hnsID)
			if err == nil {
				ep.hnsID = ""
			} else {
				klog.ErrorS(err, "Endpoint deletion failed", "ip", ep.IP())
			}
		}

		ep.refCount = nil
	}
}

func (refCountMap endPointsReferenceCountMap) getRefCount(hnsID string) *uint16 {
	refCount, exists := refCountMap[hnsID]
	if !exists {
		refCountMap[hnsID] = new(uint16)
		refCount = refCountMap[hnsID]
	}
	return refCount
}

// returns a new proxy.ServicePort which abstracts a serviceInfo
func (proxier *Proxier) newServiceInfo(port *v1.ServicePort, service *v1.Service, bsvcPortInfo *proxy.BaseServicePortInfo) proxy.ServicePort {
	info := &serviceInfo{BaseServicePortInfo: bsvcPortInfo}
	preserveDIP := service.Annotations["preserve-destination"] == "true"
	// Annotation introduced to enable optimized loadbalancing
	winProxyOptimization := !(strings.ToUpper(service.Annotations["winProxyOptimization"]) == "DISABLED")
	localTrafficDSR := service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyLocal
	var internalTrafficLocal bool
	if service.Spec.InternalTrafficPolicy != nil {
		internalTrafficLocal = *service.Spec.InternalTrafficPolicy == v1.ServiceInternalTrafficPolicyLocal
	}
	hcnImpl := proxier.hcn
	err := hcnImpl.DsrSupported()
	if err != nil {
		preserveDIP = false
		localTrafficDSR = false
	}
	// targetPort is zero if it is specified as a name in port.TargetPort.
	// Its real value would be got later from endpoints.
	targetPort := 0
	if port.TargetPort.Type == intstr.Int {
		targetPort = port.TargetPort.IntValue()
	}

	info.preserveDIP = preserveDIP
	info.targetPort = targetPort
	info.hns = proxier.hns
	info.localTrafficDSR = localTrafficDSR
	info.internalTrafficLocal = internalTrafficLocal
	info.winProxyOptimization = winProxyOptimization
	klog.V(3).InfoS("Flags enabled for service", "service", service.Name, "localTrafficDSR", localTrafficDSR, "internalTrafficLocal", internalTrafficLocal, "preserveDIP", preserveDIP, "winProxyOptimization", winProxyOptimization)

	for _, eip := range service.Spec.ExternalIPs {
		info.externalIPs = append(info.externalIPs, &externalIPInfo{ip: eip})
	}

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if netutils.ParseIPSloppy(ingress.IP) != nil {
			info.loadBalancerIngressIPs = append(info.loadBalancerIngressIPs, &loadBalancerIngressInfo{ip: ingress.IP})
		}
	}
	return info
}

func (network hnsNetworkInfo) findRemoteSubnetProviderAddress(ip string) string {
	var providerAddress string
	for _, rs := range network.remoteSubnets {
		_, ipNet, err := netutils.ParseCIDRSloppy(rs.destinationPrefix)
		if err != nil {
			klog.ErrorS(err, "Failed to parse CIDR")
		}
		if ipNet.Contains(netutils.ParseIPSloppy(ip)) {
			providerAddress = rs.providerAddress
		}
		if ip == rs.providerAddress {
			providerAddress = rs.providerAddress
		}
	}

	return providerAddress
}

type endPointsReferenceCountMap map[string]*uint16

// Proxier is an hns based proxy for connections between a localhost:lport
// and services that provide the actual backends.
type Proxier struct {
	// ipFamily defines the IP family which this proxier is tracking.
	ipFamily v1.IPFamily
	// TODO(imroc): implement node handler for winkernel proxier.
	proxyconfig.NoopNodeHandler

	// endpointsChanges and serviceChanges contains all changes to endpoints and
	// services that happened since policies were synced. For a single object,
	// changes are accumulated, i.e. previous is state from before all of them,
	// current is state after applying all of those.
	endpointsChanges  *proxy.EndpointsChangeTracker
	serviceChanges    *proxy.ServiceChangeTracker
	endPointsRefCount endPointsReferenceCountMap
	mu                sync.Mutex // protects the following fields
	svcPortMap        proxy.ServicePortMap
	endpointsMap      proxy.EndpointsMap
	// endpointSlicesSynced and servicesSynced are set to true when corresponding
	// objects are synced after startup. This is used to avoid updating hns policies
	// with some partial data after kube-proxy restart.
	endpointSlicesSynced bool
	servicesSynced       bool
	initialized          int32
	syncRunner           *async.BoundedFrequencyRunner // governs calls to syncProxyRules
	// These are effectively const and do not need the mutex to be held.
	hostname string
	nodeIP   net.IP
	recorder events.EventRecorder

	serviceHealthServer healthcheck.ServiceHealthServer
	healthzServer       *healthcheck.ProxyHealthServer

	hns               HostNetworkService
	hcn               HcnService
	network           hnsNetworkInfo
	sourceVip         string
	hostMac           string
	isDSR             bool
	supportedFeatures hcn.SupportedFeatures
	healthzPort       int

	forwardHealthCheckVip bool
	rootHnsEndpointName   string
	mapStaleLoadbalancers map[string]bool // This maintains entries of stale load balancers which are pending delete in last iteration
	terminatedEndpoints   map[string]bool // This maintains entries of endpoints which are terminated. Key is ip address:portnumber
}

type localPort struct {
	desc     string
	ip       string
	port     int
	protocol string
}

func (lp *localPort) String() string {
	return fmt.Sprintf("%q (%s:%d/%s)", lp.desc, lp.ip, lp.port, lp.protocol)
}

func Enum(p v1.Protocol) uint16 {
	if p == v1.ProtocolTCP {
		return 6
	}
	if p == v1.ProtocolUDP {
		return 17
	}
	if p == v1.ProtocolSCTP {
		return 132
	}
	return 0
}

type closeable interface {
	Close() error
}

// Proxier implements proxy.Provider
var _ proxy.Provider = &Proxier{}

// NewProxier returns a new Proxier
func NewProxier(
	ipFamily v1.IPFamily,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	hostname string,
	nodeIP net.IP,
	recorder events.EventRecorder,
	healthzServer *healthcheck.ProxyHealthServer,
	healthzBindAddress string,
	config config.KubeProxyWinkernelConfiguration,
) (*Proxier, error) {
	if nodeIP == nil {
		klog.InfoS("Invalid nodeIP, initializing kube-proxy with 127.0.0.1 as nodeIP")
		nodeIP = netutils.ParseIPSloppy("127.0.0.1")
	}

	// windows listens to all node addresses
	nodePortAddresses := proxyutil.NewNodePortAddresses(ipFamily, nil)
	serviceHealthServer := healthcheck.NewServiceHealthServer(hostname, recorder, nodePortAddresses, healthzServer)

	var healthzPort int
	if len(healthzBindAddress) > 0 {
		_, port, _ := net.SplitHostPort(healthzBindAddress)
		healthzPort, _ = strconv.Atoi(port)
	}

	hcnImpl := newHcnImpl()
	hns, supportedFeatures := newHostNetworkService(hcnImpl)
	hnsNetworkName, err := getNetworkName(config.NetworkName)
	if err != nil {
		return nil, err
	}

	klog.V(3).InfoS("Cleaning up old HNS policy lists")
	hcnImpl.DeleteAllHnsLoadBalancerPolicy()

	// Get HNS network information
	hnsNetworkInfo, err := getNetworkInfo(hns, hnsNetworkName)
	if err != nil {
		return nil, err
	}

	// Network could have been detected before Remote Subnet Routes are applied or ManagementIP is updated
	// Sleep and update the network to include new information
	if isOverlay(hnsNetworkInfo) {
		time.Sleep(10 * time.Second)
		hnsNetworkInfo, err = hns.getNetworkByName(hnsNetworkName)
		if err != nil {
			return nil, fmt.Errorf("could not find HNS network %s", hnsNetworkName)
		}
	}

	klog.V(1).InfoS("Hns Network loaded", "hnsNetworkInfo", hnsNetworkInfo)
	isDSR := config.EnableDSR
	if isDSR && !utilfeature.DefaultFeatureGate.Enabled(kubefeatures.WinDSR) {
		return nil, fmt.Errorf("WinDSR feature gate not enabled")
	}

	err = hcnImpl.DsrSupported()
	if isDSR && err != nil {
		return nil, err
	}

	var sourceVip string
	var hostMac string
	if isOverlay(hnsNetworkInfo) {
		if !utilfeature.DefaultFeatureGate.Enabled(kubefeatures.WinOverlay) {
			return nil, fmt.Errorf("WinOverlay feature gate not enabled")
		}
		err = hcn.RemoteSubnetSupported()
		if err != nil {
			return nil, err
		}

		/* Source VIP is required for overlay network mode. KubeProxy will determine the Source Vip in following order:
		1. Use the Source Vip in the KubeProxy winkernel configuration options
		2. If the Source Vip is not provided in the winkernel configuration, check if an HNS endpoint exists with the well-known name
		   It is the responsibility of the CNI to ensure the Source Vip is created using the well known-documented name
		3. If the expected Source Vip endpoint name does not exist on the host, KubeProxy will attempt to create the Source Vip
		*/
		sourceVip = config.SourceVip
		if len(sourceVip) == 0 {
			klog.InfoS("sourceVip not set in the winkernel configuration, will check if an HNS endpoint exists with the well-known name %s", SOURCE_VIP_NAME)
			endpoint, err := hns.getEndpointByName(SOURCE_VIP_NAME)
			if err != nil {
				if hcn.IsNotFoundError(err) {
					klog.Warningf("Unable to find Source Vip endpoint with name '%s', expected endpoint to be created by CNI", SOURCE_VIP_NAME)
				} else {
					klog.Errorf("Querying HNS for Source Vip endpoint with name '%s' failed: %v", SOURCE_VIP_NAME, err)
				}
				return nil, fmt.Errorf("source-vip flag not set")
			} else {
				klog.V(4).Infof("Found HNS endpoint with well-known name %s and IP %s, using as Source Vip", SOURCE_VIP_NAME, endpoint.ip)
				sourceVip = endpoint.ip
			}
		}

		if nodeIP.IsUnspecified() {
			// attempt to get the correct ip address
			klog.V(2).InfoS("Node ip was unspecified, attempting to find node ip")
			nodeIP, err = apiutil.ResolveBindAddress(nodeIP)
			if err != nil {
				klog.InfoS("Failed to find an ip. You may need set the --bind-address flag", "err", err)
			}
		}

		interfaces, _ := net.Interfaces() //TODO create interfaces
		for _, inter := range interfaces {
			addresses, _ := inter.Addrs()
			for _, addr := range addresses {
				addrIP, _, _ := netutils.ParseCIDRSloppy(addr.String())
				if addrIP.String() == nodeIP.String() {
					klog.V(2).InfoS("Record Host MAC address", "addr", inter.HardwareAddr)
					hostMac = inter.HardwareAddr.String()
				}
			}
		}
		if len(hostMac) == 0 {
			return nil, fmt.Errorf("could not find host mac address for %s", nodeIP)
		}
	}

	proxier := &Proxier{
		ipFamily:              ipFamily,
		endPointsRefCount:     make(endPointsReferenceCountMap),
		svcPortMap:            make(proxy.ServicePortMap),
		endpointsMap:          make(proxy.EndpointsMap),
		hostname:              hostname,
		nodeIP:                nodeIP,
		recorder:              recorder,
		serviceHealthServer:   serviceHealthServer,
		healthzServer:         healthzServer,
		hns:                   hns,
		hcn:                   hcnImpl,
		network:               *hnsNetworkInfo,
		sourceVip:             sourceVip,
		hostMac:               hostMac,
		isDSR:                 isDSR,
		supportedFeatures:     supportedFeatures,
		healthzPort:           healthzPort,
		rootHnsEndpointName:   config.RootHnsEndpointName,
		forwardHealthCheckVip: config.ForwardHealthCheckVip,
		mapStaleLoadbalancers: make(map[string]bool),
		terminatedEndpoints:   make(map[string]bool),
	}

	serviceChanges := proxy.NewServiceChangeTracker(ipFamily, proxier.newServiceInfo, proxier.serviceMapChange)
	endPointChangeTracker := proxy.NewEndpointsChangeTracker(ipFamily, hostname, proxier.newEndpointInfo, proxier.endpointsMapChange)
	proxier.endpointsChanges = endPointChangeTracker
	proxier.serviceChanges = serviceChanges

	burstSyncs := 2
	klog.V(3).InfoS("Record sync param", "minSyncPeriod", minSyncPeriod, "syncPeriod", syncPeriod, "burstSyncs", burstSyncs)
	proxier.syncRunner = async.NewBoundedFrequencyRunner("sync-runner", proxier.syncProxyRules, minSyncPeriod, syncPeriod, burstSyncs)
	return proxier, nil
}

func NewDualStackProxier(
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	hostname string,
	nodeIPs map[v1.IPFamily]net.IP,
	recorder events.EventRecorder,
	healthzServer *healthcheck.ProxyHealthServer,
	healthzBindAddress string,
	config config.KubeProxyWinkernelConfiguration,
) (proxy.Provider, error) {

	// Create an ipv4 instance of the single-stack proxier
	ipv4Proxier, err := NewProxier(v1.IPv4Protocol, syncPeriod, minSyncPeriod,
		hostname, nodeIPs[v1.IPv4Protocol], recorder, healthzServer,
		healthzBindAddress, config)

	if err != nil {
		return nil, fmt.Errorf("unable to create ipv4 proxier: %v, hostname: %s, nodeIP:%v", err, hostname, nodeIPs[v1.IPv4Protocol])
	}

	ipv6Proxier, err := NewProxier(v1.IPv6Protocol, syncPeriod, minSyncPeriod,
		hostname, nodeIPs[v1.IPv6Protocol], recorder, healthzServer,
		healthzBindAddress, config)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv6 proxier: %v, hostname: %s, nodeIP:%v", err, hostname, nodeIPs[v1.IPv6Protocol])
	}

	// Return a meta-proxier that dispatch calls between the two
	// single-stack proxier instances
	return metaproxier.NewMetaProxier(ipv4Proxier, ipv6Proxier), nil
}

// CleanupLeftovers removes all hns rules created by the Proxier
// It returns true if an error was encountered. Errors are logged.
func CleanupLeftovers() (encounteredError bool) {
	// Delete all Hns Load Balancer Policies
	newHcnImpl().DeleteAllHnsLoadBalancerPolicy()
	// TODO
	// Delete all Hns Remote endpoints

	return encounteredError
}

func (svcInfo *serviceInfo) cleanupAllPolicies(endpoints []proxy.Endpoint, mapStaleLoadbalancers map[string]bool, isEndpointChange bool) {
	klog.V(3).InfoS("Service cleanup", "serviceInfo", svcInfo)
	// if it's an endpoint change and winProxyOptimization annotation enable, skip lb deletion and remoteEndpoint deletion
	winProxyOptimization := isEndpointChange && svcInfo.winProxyOptimization
	if winProxyOptimization {
		klog.V(3).InfoS("Skipped loadbalancer deletion.", "hnsID", svcInfo.hnsID, "nodePorthnsID", svcInfo.nodePorthnsID, "winProxyOptimization", svcInfo.winProxyOptimization, "isEndpointChange", isEndpointChange)
	} else {
		// Skip the svcInfo.policyApplied check to remove all the policies
		svcInfo.deleteLoadBalancerPolicy(mapStaleLoadbalancers)
	}
	// Cleanup Endpoints references
	for _, ep := range endpoints {
		epInfo, ok := ep.(*endpointInfo)
		if ok {
			if winProxyOptimization {
				epInfo.DecrementRefCount()
			} else {
				epInfo.Cleanup()
			}
		}
	}
	if svcInfo.remoteEndpoint != nil {
		svcInfo.remoteEndpoint.Cleanup()
	}

	svcInfo.policyApplied = false
}

func (svcInfo *serviceInfo) deleteLoadBalancerPolicy(mapStaleLoadbalancer map[string]bool) {
	// Remove the Hns Policy corresponding to this service
	hns := svcInfo.hns
	if err := hns.deleteLoadBalancer(svcInfo.hnsID); err != nil {
		mapStaleLoadbalancer[svcInfo.hnsID] = true
		klog.V(1).ErrorS(err, "Error deleting Hns loadbalancer policy resource.", "hnsID", svcInfo.hnsID, "ClusterIP", svcInfo.ClusterIP())
	} else {
		// On successful delete, remove hnsId
		svcInfo.hnsID = ""
	}

	if err := hns.deleteLoadBalancer(svcInfo.nodePorthnsID); err != nil {
		mapStaleLoadbalancer[svcInfo.nodePorthnsID] = true
		klog.V(1).ErrorS(err, "Error deleting Hns NodePort policy resource.", "hnsID", svcInfo.nodePorthnsID, "NodePort", svcInfo.NodePort())
	} else {
		// On successful delete, remove hnsId
		svcInfo.nodePorthnsID = ""
	}

	for _, externalIP := range svcInfo.externalIPs {
		mapStaleLoadbalancer[externalIP.hnsID] = true
		if err := hns.deleteLoadBalancer(externalIP.hnsID); err != nil {
			klog.V(1).ErrorS(err, "Error deleting Hns ExternalIP policy resource.", "hnsID", externalIP.hnsID, "IP", externalIP.ip)
		} else {
			// On successful delete, remove hnsId
			externalIP.hnsID = ""
		}
	}
	for _, lbIngressIP := range svcInfo.loadBalancerIngressIPs {
		klog.V(3).InfoS("Loadbalancer Hns LoadBalancer delete triggered for loadBalancer Ingress resources in cleanup", "lbIngressIP", lbIngressIP)
		if err := hns.deleteLoadBalancer(lbIngressIP.hnsID); err != nil {
			mapStaleLoadbalancer[lbIngressIP.hnsID] = true
			klog.V(1).ErrorS(err, "Error deleting Hns IngressIP policy resource.", "hnsID", lbIngressIP.hnsID, "IP", lbIngressIP.ip)
		} else {
			// On successful delete, remove hnsId
			lbIngressIP.hnsID = ""
		}

		if lbIngressIP.healthCheckHnsID != "" {
			if err := hns.deleteLoadBalancer(lbIngressIP.healthCheckHnsID); err != nil {
				mapStaleLoadbalancer[lbIngressIP.healthCheckHnsID] = true
				klog.V(1).ErrorS(err, "Error deleting Hns IngressIP HealthCheck policy resource.", "hnsID", lbIngressIP.healthCheckHnsID, "IP", lbIngressIP.ip)
			} else {
				// On successful delete, remove hnsId
				lbIngressIP.healthCheckHnsID = ""
			}
		}
	}
}

// Sync is called to synchronize the proxier state to hns as soon as possible.
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

// OnServiceCIDRsChanged is called whenever a change is observed
// in any of the ServiceCIDRs, and provides complete list of service cidrs.
func (proxier *Proxier) OnServiceCIDRsChanged(_ []string) {}

func (proxier *Proxier) cleanupAllPolicies() {
	for svcName, svc := range proxier.svcPortMap {
		svcInfo, ok := svc.(*serviceInfo)
		if !ok {
			klog.ErrorS(nil, "Failed to cast serviceInfo", "serviceName", svcName)
			continue
		}
		svcInfo.cleanupAllPolicies(proxier.endpointsMap[svcName], proxier.mapStaleLoadbalancers, false)
	}
}

func isNetworkNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(hcn.NetworkNotFoundError); ok {
		return true
	}
	if _, ok := err.(hnslib.NetworkNotFoundError); ok {
		return true
	}
	return false
}

// isAllEndpointsTerminating function will return true if all the endpoints are terminating.
// If atleast one is not terminating, then return false
func (proxier *Proxier) isAllEndpointsTerminating(svcName proxy.ServicePortName, isLocalTrafficDSR bool) bool {
	for _, epInfo := range proxier.endpointsMap[svcName] {
		ep, ok := epInfo.(*endpointInfo)
		if !ok {
			continue
		}
		if isLocalTrafficDSR && !ep.IsLocal() {
			// KEP-1669: Ignore remote endpoints when the ExternalTrafficPolicy is Local (DSR Mode)
			continue
		}
		// If Readiness Probe fails and pod is not under delete, then
		// the state of the endpoint will be - Ready:False, Serving:False, Terminating:False
		if !ep.IsReady() && !ep.IsTerminating() {
			// Ready:false, Terminating:False, ignore
			continue
		}
		if !ep.IsTerminating() {
			return false
		}
	}
	return true
}

// isAllEndpointsNonServing function will return true if all the endpoints are non serving.
// If atleast one is serving, then return false
func (proxier *Proxier) isAllEndpointsNonServing(svcName proxy.ServicePortName, isLocalTrafficDSR bool) bool {
	for _, epInfo := range proxier.endpointsMap[svcName] {
		ep, ok := epInfo.(*endpointInfo)
		if !ok {
			continue
		}
		if isLocalTrafficDSR && !ep.IsLocal() {
			continue
		}
		if ep.IsServing() {
			return false
		}
	}
	return true
}

// updateQueriedEndpoints updates the queriedEndpoints map with newly created endpoint details
func updateQueriedEndpoints(newHnsEndpoint *endpointInfo, queriedEndpoints map[string]*endpointInfo) {
	// store newly created endpoints in queriedEndpoints
	queriedEndpoints[newHnsEndpoint.hnsID] = newHnsEndpoint
	queriedEndpoints[newHnsEndpoint.ip] = newHnsEndpoint
}

func (proxier *Proxier) requiresUpdateLoadbalancer(lbHnsID string, endpointCount int) bool {
	return proxier.supportedFeatures.ModifyLoadbalancer && lbHnsID != "" && endpointCount > 0
}

// handleUpdateLoadbalancerFailure will handle the error returned by updatePolicy. If the error is due to unsupported feature,
// then it will set the supportedFeatures.ModifyLoadbalancer to false. return true means skip the iteration.
func (proxier *Proxier) handleUpdateLoadbalancerFailure(err error, hnsID, svcIP string, endpointCount int) (skipIteration bool) {
	if err != nil {
		if hcn.IsNotImplemented(err) {
			klog.Warning("Update loadbalancer policies is not implemented.", "hnsID", hnsID, "svcIP", svcIP, "endpointCount", endpointCount)
			proxier.supportedFeatures.ModifyLoadbalancer = false
		} else {
			klog.ErrorS(err, "Update loadbalancer policy failed", "hnsID", hnsID, "svcIP", svcIP, "endpointCount", endpointCount)
			skipIteration = true
		}
	}
	return skipIteration
}

// This is where all of the hns save/restore calls happen.
// assumes proxier.mu is held
func (proxier *Proxier) syncProxyRules() {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	// don't sync rules till we've received services and endpoints
	if !proxier.isInitialized() {
		klog.V(2).InfoS("Not syncing hns until Services and Endpoints have been received from master")
		return
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		metrics.SyncProxyRulesLatency.WithLabelValues(string(proxier.ipFamily)).Observe(metrics.SinceInSeconds(start))
		klog.V(4).InfoS("Syncing proxy rules complete", "elapsed", time.Since(start))
	}()

	hnsNetworkName := proxier.network.name
	hns := proxier.hns

	var gatewayHnsendpoint *endpointInfo
	if proxier.forwardHealthCheckVip {
		gatewayHnsendpoint, _ = hns.getEndpointByName(proxier.rootHnsEndpointName)
	}

	prevNetworkID := proxier.network.id
	updatedNetwork, err := hns.getNetworkByName(hnsNetworkName)
	if updatedNetwork == nil || updatedNetwork.id != prevNetworkID || isNetworkNotFoundError(err) {
		klog.InfoS("The HNS network is not present or has changed since the last sync, please check the CNI deployment", "hnsNetworkName", hnsNetworkName)
		proxier.cleanupAllPolicies()
		if updatedNetwork != nil {
			proxier.network = *updatedNetwork
		}
		return
	}

	// We assume that if this was called, we really want to sync them,
	// even if nothing changed in the meantime. In other words, callers are
	// responsible for detecting no-op changes and not calling this function.
	_ = proxier.svcPortMap.Update(proxier.serviceChanges)
	_ = proxier.endpointsMap.Update(proxier.endpointsChanges)

	// Query HNS for endpoints and load balancers
	queriedEndpoints, err := hns.getAllEndpointsByNetwork(hnsNetworkName)
	if err != nil {
		klog.ErrorS(err, "Querying HNS for endpoints failed")
		return
	}
	if queriedEndpoints == nil {
		klog.V(4).InfoS("No existing endpoints found in HNS")
		queriedEndpoints = make(map[string]*(endpointInfo))
	}
	queriedLoadBalancers, err := hns.getAllLoadBalancers()
	if queriedLoadBalancers == nil {
		klog.V(4).InfoS("No existing load balancers found in HNS")
		queriedLoadBalancers = make(map[loadBalancerIdentifier]*(loadBalancerInfo))
	}
	if err != nil {
		klog.ErrorS(err, "Querying HNS for load balancers failed")
		return
	}
	if strings.EqualFold(proxier.network.networkType, NETWORK_TYPE_OVERLAY) {
		if _, ok := queriedEndpoints[proxier.sourceVip]; !ok {
			_, err = newSourceVIP(hns, hnsNetworkName, proxier.sourceVip, proxier.hostMac, proxier.nodeIP.String())
			if err != nil {
				klog.ErrorS(err, "Source Vip endpoint creation failed")
				return
			}
		}
	}

	klog.V(3).InfoS("Syncing Policies")

	// Program HNS by adding corresponding policies for each service.
	for svcName, svc := range proxier.svcPortMap {
		svcInfo, ok := svc.(*serviceInfo)
		if !ok {
			klog.ErrorS(nil, "Failed to cast serviceInfo", "serviceName", svcName)
			continue
		}

		if svcInfo.policyApplied {
			klog.V(4).InfoS("Policy already applied", "serviceInfo", svcInfo)
			continue
		}

		if strings.EqualFold(proxier.network.networkType, NETWORK_TYPE_OVERLAY) {
			serviceVipEndpoint := queriedEndpoints[svcInfo.ClusterIP().String()]
			if serviceVipEndpoint == nil {
				klog.V(4).InfoS("No existing remote endpoint", "IP", svcInfo.ClusterIP())
				hnsEndpoint := &endpointInfo{
					ip:              svcInfo.ClusterIP().String(),
					isLocal:         false,
					macAddress:      proxier.hostMac,
					providerAddress: proxier.nodeIP.String(),
				}

				// TODO: Consider removing sourcevip creation code due to potential unavailability of IP from CNI's IPAM.
				// See https://github.com/kubernetes/kubernetes/issues/100962#issuecomment-821274716
				newHnsEndpoint, err := hns.createEndpoint(hnsEndpoint, hnsNetworkName)
				if err != nil {
					klog.ErrorS(err, "Remote endpoint creation failed for service VIP")
					continue
				}

				newHnsEndpoint.refCount = proxier.endPointsRefCount.getRefCount(newHnsEndpoint.hnsID)
				*newHnsEndpoint.refCount++
				svcInfo.remoteEndpoint = newHnsEndpoint
				updateQueriedEndpoints(newHnsEndpoint, queriedEndpoints)
			}
		}

		var hnsEndpoints []endpointInfo
		var hnsLocalEndpoints []endpointInfo
		klog.V(4).InfoS("Applying Policy", "serviceInfo", svcName)
		// Create Remote endpoints for every endpoint, corresponding to the service
		containsPublicIP := false
		containsNodeIP := false
		var allEndpointsTerminating, allEndpointsNonServing bool
		someEndpointsServing := true

		if len(svcInfo.loadBalancerIngressIPs) > 0 {
			// Check should be done only if comes under the feature gate or enabled
			// The check should be done only if Spec.Type == Loadbalancer.
			allEndpointsTerminating = proxier.isAllEndpointsTerminating(svcName, svcInfo.localTrafficDSR)
			allEndpointsNonServing = proxier.isAllEndpointsNonServing(svcName, svcInfo.localTrafficDSR)
			someEndpointsServing = !allEndpointsNonServing
			klog.V(4).InfoS("Terminating status checked for all endpoints", "svcClusterIP", svcInfo.ClusterIP(), "allEndpointsTerminating", allEndpointsTerminating, "allEndpointsNonServing", allEndpointsNonServing, "localTrafficDSR", svcInfo.localTrafficDSR)
		} else {
			klog.V(4).InfoS("Skipped terminating status check for all endpoints", "svcClusterIP", svcInfo.ClusterIP(), "ingressLBCount", len(svcInfo.loadBalancerIngressIPs))
		}

		for _, epInfo := range proxier.endpointsMap[svcName] {
			ep, ok := epInfo.(*endpointInfo)
			if !ok {
				klog.ErrorS(nil, "Failed to cast endpointInfo", "serviceName", svcName)
				continue
			}

			if svcInfo.internalTrafficLocal && svcInfo.localTrafficDSR && !ep.IsLocal() {
				// No need to use or create remote endpoint when internal and external traffic policy is remote
				klog.V(3).InfoS("Skipping the endpoint. Both internalTraffic and external traffic policies are local", "EpIP", ep.ip, " EpPort", ep.port)
				continue
			}

			if someEndpointsServing {

				if !allEndpointsTerminating && !ep.IsReady() {
					klog.V(3).InfoS("Skipping the endpoint for LB creation. Endpoint is either not ready or all not all endpoints are terminating", "EpIP", ep.ip, " EpPort", ep.port, "allEndpointsTerminating", allEndpointsTerminating, "IsEpReady", ep.IsReady())
					continue
				}
				if !ep.IsServing() {
					klog.V(3).InfoS("Skipping the endpoint for LB creation. Endpoint is not serving", "EpIP", ep.ip, " EpPort", ep.port, "IsEpServing", ep.IsServing())
					continue
				}

			}

			var newHnsEndpoint *endpointInfo
			hnsNetworkName := proxier.network.name
			var err error

			// targetPort is zero if it is specified as a name in port.TargetPort, so the real port should be got from endpoints.
			// Note that hnslib.AddLoadBalancer() doesn't support endpoints with different ports, so only port from first endpoint is used.
			// TODO(feiskyer): add support of different endpoint ports after hnslib.AddLoadBalancer() add that.
			if svcInfo.targetPort == 0 {
				svcInfo.targetPort = int(ep.port)
			}
			// There is a bug in Windows Server 2019 that can cause two endpoints to be created with the same IP address, so we need to check using endpoint ID first.
			// TODO: Remove lookup by endpoint ID, and use the IP address only, so we don't need to maintain multiple keys for lookup.
			if len(ep.hnsID) > 0 {
				newHnsEndpoint = queriedEndpoints[ep.hnsID]
			}

			if newHnsEndpoint == nil {
				// First check if an endpoint resource exists for this IP, on the current host
				// A Local endpoint could exist here already
				// A remote endpoint was already created and proxy was restarted
				newHnsEndpoint = queriedEndpoints[ep.IP()]
			}

			if newHnsEndpoint == nil {
				if ep.IsLocal() {
					klog.ErrorS(err, "Local endpoint not found: on network", "ip", ep.IP(), "hnsNetworkName", hnsNetworkName)
					continue
				}

				if strings.EqualFold(proxier.network.networkType, NETWORK_TYPE_OVERLAY) {
					klog.InfoS("Updating network to check for new remote subnet policies", "networkName", proxier.network.name)
					networkName := proxier.network.name
					updatedNetwork, err := hns.getNetworkByName(networkName)
					if err != nil {
						klog.ErrorS(err, "Unable to find HNS Network specified, please check network name and CNI deployment", "hnsNetworkName", hnsNetworkName)
						proxier.cleanupAllPolicies()
						return
					}
					proxier.network = *updatedNetwork
					providerAddress := proxier.network.findRemoteSubnetProviderAddress(ep.IP())
					if len(providerAddress) == 0 {
						klog.InfoS("Could not find provider address, assuming it is a public IP", "IP", ep.IP())
						providerAddress = proxier.nodeIP.String()
					}

					hnsEndpoint := &endpointInfo{
						ip:              ep.ip,
						isLocal:         false,
						macAddress:      conjureMac("02-11", netutils.ParseIPSloppy(ep.ip)),
						providerAddress: providerAddress,
					}

					newHnsEndpoint, err = hns.createEndpoint(hnsEndpoint, hnsNetworkName)
					if err != nil {
						klog.ErrorS(err, "Remote endpoint creation failed", "endpointInfo", hnsEndpoint)
						continue
					}
					updateQueriedEndpoints(newHnsEndpoint, queriedEndpoints)
				} else {

					hnsEndpoint := &endpointInfo{
						ip:         ep.ip,
						isLocal:    false,
						macAddress: ep.macAddress,
					}

					newHnsEndpoint, err = hns.createEndpoint(hnsEndpoint, hnsNetworkName)
					if err != nil {
						klog.ErrorS(err, "Remote endpoint creation failed")
						continue
					}
					updateQueriedEndpoints(newHnsEndpoint, queriedEndpoints)
				}
			}
			// For Overlay networks 'SourceVIP' on an Load balancer Policy can either be chosen as
			// a) Source VIP configured on kube-proxy (or)
			// b) Node IP of the current node
			//
			// For L2Bridge network the Source VIP is always the NodeIP of the current node and the same
			// would be configured on kube-proxy as SourceVIP
			//
			// The logic for choosing the SourceVIP in Overlay networks is based on the backend endpoints:
			// a) Endpoints are any IP's outside the cluster ==> Choose NodeIP as the SourceVIP
			// b) Endpoints are IP addresses of a remote node => Choose NodeIP as the SourceVIP
			// c) Everything else (Local POD's, Remote POD's, Node IP of current node) ==> Choose the configured SourceVIP
			if strings.EqualFold(proxier.network.networkType, NETWORK_TYPE_OVERLAY) && !ep.IsLocal() {
				providerAddress := proxier.network.findRemoteSubnetProviderAddress(ep.IP())

				isNodeIP := (ep.IP() == providerAddress)
				isPublicIP := (len(providerAddress) == 0)
				klog.InfoS("Endpoint on overlay network", "ip", ep.IP(), "hnsNetworkName", hnsNetworkName, "isNodeIP", isNodeIP, "isPublicIP", isPublicIP)

				containsNodeIP = containsNodeIP || isNodeIP
				containsPublicIP = containsPublicIP || isPublicIP
			}

			// Save the hnsId for reference
			klog.V(1).InfoS("Hns endpoint resource", "endpointInfo", newHnsEndpoint)

			hnsEndpoints = append(hnsEndpoints, *newHnsEndpoint)
			if newHnsEndpoint.IsLocal() {
				hnsLocalEndpoints = append(hnsLocalEndpoints, *newHnsEndpoint)
			} else {
				// We only share the refCounts for remote endpoints
				ep.refCount = proxier.endPointsRefCount.getRefCount(newHnsEndpoint.hnsID)
				*ep.refCount++
			}

			ep.hnsID = newHnsEndpoint.hnsID

			klog.V(3).InfoS("Endpoint resource found", "endpointInfo", ep)
		}

		klog.V(3).InfoS("Associated endpoints for service", "endpointInfo", hnsEndpoints, "serviceName", svcName)

		if len(svcInfo.hnsID) > 0 {
			// This should not happen
			klog.InfoS("Load Balancer already exists.", "hnsID", svcInfo.hnsID)
		}

		// In ETP:Cluster, if all endpoints are under termination,
		// it will have serving and terminating, else only ready and serving
		if len(hnsEndpoints) == 0 {
			if svcInfo.winProxyOptimization {
				// Deleting loadbalancers when there are no endpoints to serve.
				klog.V(3).InfoS("Cleanup existing ", "endpointInfo", hnsEndpoints, "serviceName", svcName)
				svcInfo.deleteLoadBalancerPolicy(proxier.mapStaleLoadbalancers)
			}
			klog.ErrorS(nil, "Endpoint information not available for service, not applying any policy", "serviceName", svcName)
			continue
		}

		klog.V(4).InfoS("Trying to apply Policies for service", "serviceInfo", svcInfo)
		var hnsLoadBalancer *loadBalancerInfo
		var sourceVip = proxier.sourceVip
		if containsPublicIP || containsNodeIP {
			sourceVip = proxier.nodeIP.String()
		}

		sessionAffinityClientIP := svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP
		if sessionAffinityClientIP && !proxier.supportedFeatures.SessionAffinity {
			klog.InfoS("Session Affinity is not supported on this version of Windows")
		}

		endpointsAvailableForLB := !allEndpointsTerminating && !allEndpointsNonServing

		// clusterIPEndpoints is the endpoint list used for creating ClusterIP loadbalancer.
		clusterIPEndpoints := hnsEndpoints
		if svcInfo.internalTrafficLocal {
			// Take local endpoints for clusterip loadbalancer when internal traffic policy is local.
			clusterIPEndpoints = hnsLocalEndpoints
		}

		if proxier.requiresUpdateLoadbalancer(svcInfo.hnsID, len(clusterIPEndpoints)) {
			hnsLoadBalancer, err = hns.updateLoadBalancer(
				svcInfo.hnsID,
				sourceVip,
				svcInfo.ClusterIP().String(),
				clusterIPEndpoints,
				loadBalancerFlags{isDSR: proxier.isDSR, isIPv6: proxier.ipFamily == v1.IPv6Protocol, sessionAffinity: sessionAffinityClientIP},
				Enum(svcInfo.Protocol()),
				uint16(svcInfo.targetPort),
				uint16(svcInfo.Port()),
				queriedLoadBalancers,
			)
			if skipIteration := proxier.handleUpdateLoadbalancerFailure(err, svcInfo.hnsID, svcInfo.ClusterIP().String(), len(clusterIPEndpoints)); skipIteration {
				continue
			}
		}

		if !proxier.requiresUpdateLoadbalancer(svcInfo.hnsID, len(clusterIPEndpoints)) {
			proxier.deleteExistingLoadBalancer(hns, svcInfo.winProxyOptimization, &svcInfo.hnsID, svcInfo.ClusterIP().String(), Enum(svcInfo.Protocol()), uint16(svcInfo.targetPort), uint16(svcInfo.Port()), hnsEndpoints, queriedLoadBalancers)
			if len(clusterIPEndpoints) > 0 {

				// If all endpoints are terminating, then no need to create Cluster IP LoadBalancer
				// Cluster IP LoadBalancer creation
				hnsLoadBalancer, err := hns.getLoadBalancer(
					clusterIPEndpoints,
					loadBalancerFlags{isDSR: proxier.isDSR, isIPv6: proxier.ipFamily == v1.IPv6Protocol, sessionAffinity: sessionAffinityClientIP},
					sourceVip,
					svcInfo.ClusterIP().String(),
					Enum(svcInfo.Protocol()),
					uint16(svcInfo.targetPort),
					uint16(svcInfo.Port()),
					queriedLoadBalancers,
				)
				if err != nil {
					klog.ErrorS(err, "ClusterIP policy creation failed")
					continue
				}

				svcInfo.hnsID = hnsLoadBalancer.hnsID
				klog.V(3).InfoS("Hns LoadBalancer resource created for cluster ip resources", "clusterIP", svcInfo.ClusterIP(), "hnsID", hnsLoadBalancer.hnsID)

			} else {
				klog.V(3).InfoS("Skipped creating Hns LoadBalancer for cluster ip resources. Reason : all endpoints are terminating", "clusterIP", svcInfo.ClusterIP(), "nodeport", svcInfo.NodePort(), "allEndpointsTerminating", allEndpointsTerminating)
			}
		}

		// If nodePort is specified, user should be able to use nodeIP:nodePort to reach the backend endpoints
		if svcInfo.NodePort() > 0 {
			// If the preserve-destination service annotation is present, we will disable routing mesh for NodePort.
			// This means that health services can use Node Port without falsely getting results from a different node.
			nodePortEndpoints := hnsEndpoints
			if svcInfo.preserveDIP || svcInfo.localTrafficDSR {
				nodePortEndpoints = hnsLocalEndpoints
			}

			if proxier.requiresUpdateLoadbalancer(svcInfo.nodePorthnsID, len(nodePortEndpoints)) && endpointsAvailableForLB {
				hnsLoadBalancer, err = hns.updateLoadBalancer(
					svcInfo.nodePorthnsID,
					sourceVip,
					"",
					nodePortEndpoints,
					loadBalancerFlags{isVipExternalIP: true, isDSR: svcInfo.localTrafficDSR, localRoutedVIP: true, sessionAffinity: sessionAffinityClientIP, isIPv6: proxier.ipFamily == v1.IPv6Protocol},
					Enum(svcInfo.Protocol()),
					uint16(svcInfo.targetPort),
					uint16(svcInfo.NodePort()),
					queriedLoadBalancers,
				)
				if skipIteration := proxier.handleUpdateLoadbalancerFailure(err, svcInfo.nodePorthnsID, sourceVip, len(nodePortEndpoints)); skipIteration {
					continue
				}
			}

			if !proxier.requiresUpdateLoadbalancer(svcInfo.nodePorthnsID, len(nodePortEndpoints)) {
				proxier.deleteExistingLoadBalancer(hns, svcInfo.winProxyOptimization, &svcInfo.nodePorthnsID, "", Enum(svcInfo.Protocol()), uint16(svcInfo.targetPort), uint16(svcInfo.NodePort()), nodePortEndpoints, queriedLoadBalancers)

				if len(nodePortEndpoints) > 0 && endpointsAvailableForLB {
					// If all endpoints are in terminating stage, then no need to create Node Port LoadBalancer
					hnsLoadBalancer, err := hns.getLoadBalancer(
						nodePortEndpoints,
						loadBalancerFlags{isVipExternalIP: true, isDSR: svcInfo.localTrafficDSR, localRoutedVIP: true, sessionAffinity: sessionAffinityClientIP, isIPv6: proxier.ipFamily == v1.IPv6Protocol},
						sourceVip,
						"",
						Enum(svcInfo.Protocol()),
						uint16(svcInfo.targetPort),
						uint16(svcInfo.NodePort()),
						queriedLoadBalancers,
					)
					if err != nil {
						klog.ErrorS(err, "Nodeport policy creation failed")
						continue
					}

					svcInfo.nodePorthnsID = hnsLoadBalancer.hnsID
					klog.V(3).InfoS("Hns LoadBalancer resource created for nodePort resources", "clusterIP", svcInfo.ClusterIP(), "nodeport", svcInfo.NodePort(), "hnsID", hnsLoadBalancer.hnsID)
				} else {
					klog.V(3).InfoS("Skipped creating Hns LoadBalancer for nodePort resources", "clusterIP", svcInfo.ClusterIP(), "nodeport", svcInfo.NodePort(), "allEndpointsTerminating", allEndpointsTerminating)
				}
			}
		}

		// Create a Load Balancer Policy for each external IP
		for _, externalIP := range svcInfo.externalIPs {
			// Disable routing mesh if ExternalTrafficPolicy is set to local
			externalIPEndpoints := hnsEndpoints
			if svcInfo.localTrafficDSR {
				externalIPEndpoints = hnsLocalEndpoints
			}

			if proxier.requiresUpdateLoadbalancer(externalIP.hnsID, len(externalIPEndpoints)) && endpointsAvailableForLB {
				hnsLoadBalancer, err = hns.updateLoadBalancer(
					externalIP.hnsID,
					sourceVip,
					externalIP.ip,
					externalIPEndpoints,
					loadBalancerFlags{isVipExternalIP: true, isDSR: svcInfo.localTrafficDSR, sessionAffinity: sessionAffinityClientIP, isIPv6: proxier.ipFamily == v1.IPv6Protocol},
					Enum(svcInfo.Protocol()),
					uint16(svcInfo.targetPort),
					uint16(svcInfo.Port()),
					queriedLoadBalancers,
				)
				if skipIteration := proxier.handleUpdateLoadbalancerFailure(err, externalIP.hnsID, externalIP.ip, len(externalIPEndpoints)); skipIteration {
					continue
				}
			}

			if !proxier.requiresUpdateLoadbalancer(externalIP.hnsID, len(externalIPEndpoints)) {
				proxier.deleteExistingLoadBalancer(hns, svcInfo.winProxyOptimization, &externalIP.hnsID, externalIP.ip, Enum(svcInfo.Protocol()), uint16(svcInfo.targetPort), uint16(svcInfo.Port()), externalIPEndpoints, queriedLoadBalancers)

				if len(externalIPEndpoints) > 0 && endpointsAvailableForLB {
					// If all endpoints are in terminating stage, then no need to External IP LoadBalancer
					// Try loading existing policies, if already available
					hnsLoadBalancer, err = hns.getLoadBalancer(
						externalIPEndpoints,
						loadBalancerFlags{isVipExternalIP: true, isDSR: svcInfo.localTrafficDSR, sessionAffinity: sessionAffinityClientIP, isIPv6: proxier.ipFamily == v1.IPv6Protocol},
						sourceVip,
						externalIP.ip,
						Enum(svcInfo.Protocol()),
						uint16(svcInfo.targetPort),
						uint16(svcInfo.Port()),
						queriedLoadBalancers,
					)
					if err != nil {
						klog.ErrorS(err, "ExternalIP policy creation failed")
						continue
					}
					externalIP.hnsID = hnsLoadBalancer.hnsID
					klog.V(3).InfoS("Hns LoadBalancer resource created for externalIP resources", "externalIP", externalIP, "hnsID", hnsLoadBalancer.hnsID)
				} else {
					klog.V(3).InfoS("Skipped creating Hns LoadBalancer for externalIP resources", "externalIP", externalIP, "allEndpointsTerminating", allEndpointsTerminating)
				}
			}
		}
		// Create a Load Balancer Policy for each loadbalancer ingress
		for _, lbIngressIP := range svcInfo.loadBalancerIngressIPs {
			// Try loading existing policies, if already available
			lbIngressEndpoints := hnsEndpoints
			if svcInfo.preserveDIP || svcInfo.localTrafficDSR {
				lbIngressEndpoints = hnsLocalEndpoints
			}

			if proxier.requiresUpdateLoadbalancer(lbIngressIP.hnsID, len(lbIngressEndpoints)) {
				hnsLoadBalancer, err = hns.updateLoadBalancer(
					lbIngressIP.hnsID,
					sourceVip,
					lbIngressIP.ip,
					lbIngressEndpoints,
					loadBalancerFlags{isVipExternalIP: true, isDSR: svcInfo.preserveDIP || svcInfo.localTrafficDSR, useMUX: svcInfo.preserveDIP, preserveDIP: svcInfo.preserveDIP, sessionAffinity: sessionAffinityClientIP, isIPv6: proxier.ipFamily == v1.IPv6Protocol},
					Enum(svcInfo.Protocol()),
					uint16(svcInfo.targetPort),
					uint16(svcInfo.Port()),
					queriedLoadBalancers,
				)
				if skipIteration := proxier.handleUpdateLoadbalancerFailure(err, lbIngressIP.hnsID, lbIngressIP.ip, len(lbIngressEndpoints)); skipIteration {
					continue
				}
			}

			if !proxier.requiresUpdateLoadbalancer(lbIngressIP.hnsID, len(lbIngressEndpoints)) {
				proxier.deleteExistingLoadBalancer(hns, svcInfo.winProxyOptimization, &lbIngressIP.hnsID, lbIngressIP.ip, Enum(svcInfo.Protocol()), uint16(svcInfo.targetPort), uint16(svcInfo.Port()), lbIngressEndpoints, queriedLoadBalancers)

				if len(lbIngressEndpoints) > 0 {
					hnsLoadBalancer, err := hns.getLoadBalancer(
						lbIngressEndpoints,
						loadBalancerFlags{isVipExternalIP: true, isDSR: svcInfo.preserveDIP || svcInfo.localTrafficDSR, useMUX: svcInfo.preserveDIP, preserveDIP: svcInfo.preserveDIP, sessionAffinity: sessionAffinityClientIP, isIPv6: proxier.ipFamily == v1.IPv6Protocol},
						sourceVip,
						lbIngressIP.ip,
						Enum(svcInfo.Protocol()),
						uint16(svcInfo.targetPort),
						uint16(svcInfo.Port()),
						queriedLoadBalancers,
					)
					if err != nil {
						klog.ErrorS(err, "IngressIP policy creation failed")
						continue
					}
					lbIngressIP.hnsID = hnsLoadBalancer.hnsID
					klog.V(3).InfoS("Hns LoadBalancer resource created for loadBalancer Ingress resources", "lbIngressIP", lbIngressIP)
				} else {
					klog.V(3).InfoS("Skipped creating Hns LoadBalancer for loadBalancer Ingress resources", "lbIngressIP", lbIngressIP)
				}
			}

			if proxier.forwardHealthCheckVip && gatewayHnsendpoint != nil && endpointsAvailableForLB {
				// Avoid creating health check loadbalancer if all the endpoints are terminating
				nodeport := proxier.healthzPort
				if svcInfo.HealthCheckNodePort() != 0 {
					nodeport = svcInfo.HealthCheckNodePort()
				}

				gwEndpoints := []endpointInfo{*gatewayHnsendpoint}

				if proxier.requiresUpdateLoadbalancer(lbIngressIP.healthCheckHnsID, len(gwEndpoints)) {
					hnsLoadBalancer, err = hns.updateLoadBalancer(
						lbIngressIP.healthCheckHnsID,
						sourceVip,
						lbIngressIP.ip,
						gwEndpoints,
						loadBalancerFlags{isDSR: false, useMUX: svcInfo.preserveDIP, preserveDIP: svcInfo.preserveDIP},
						Enum(svcInfo.Protocol()),
						uint16(nodeport),
						uint16(nodeport),
						queriedLoadBalancers,
					)
					if skipIteration := proxier.handleUpdateLoadbalancerFailure(err, lbIngressIP.healthCheckHnsID, lbIngressIP.ip, 1); skipIteration {
						continue
					}
				}

				if !proxier.requiresUpdateLoadbalancer(lbIngressIP.healthCheckHnsID, len(gwEndpoints)) {
					proxier.deleteExistingLoadBalancer(hns, svcInfo.winProxyOptimization, &lbIngressIP.healthCheckHnsID, lbIngressIP.ip, Enum(svcInfo.Protocol()), uint16(nodeport), uint16(nodeport), gwEndpoints, queriedLoadBalancers)

					hnsHealthCheckLoadBalancer, err := hns.getLoadBalancer(
						gwEndpoints,
						loadBalancerFlags{isDSR: false, useMUX: svcInfo.preserveDIP, preserveDIP: svcInfo.preserveDIP},
						sourceVip,
						lbIngressIP.ip,
						Enum(svcInfo.Protocol()),
						uint16(nodeport),
						uint16(nodeport),
						queriedLoadBalancers,
					)
					if err != nil {
						klog.ErrorS(err, "Healthcheck loadbalancer policy creation failed")
						continue
					}
					lbIngressIP.healthCheckHnsID = hnsHealthCheckLoadBalancer.hnsID
					klog.V(3).InfoS("Hns Health Check LoadBalancer resource created for loadBalancer Ingress resources", "ip", lbIngressIP)
				}
			} else {
				klog.V(3).InfoS("Skipped creating Hns Health Check LoadBalancer for loadBalancer Ingress resources", "ip", lbIngressIP, "allEndpointsTerminating", allEndpointsTerminating)
			}
		}
		svcInfo.policyApplied = true
		klog.V(2).InfoS("Policy successfully applied for service", "serviceInfo", svcInfo)
	}

	if proxier.healthzServer != nil {
		proxier.healthzServer.Updated(proxier.ipFamily)
	}
	metrics.SyncProxyRulesLastTimestamp.WithLabelValues(string(proxier.ipFamily)).SetToCurrentTime()

	// Update service healthchecks.  The endpoints list might include services that are
	// not "OnlyLocal", but the services list will not, and the serviceHealthServer
	// will just drop those endpoints.
	if err := proxier.serviceHealthServer.SyncServices(proxier.svcPortMap.HealthCheckNodePorts()); err != nil {
		klog.ErrorS(err, "Error syncing healthcheck services")
	}
	if err := proxier.serviceHealthServer.SyncEndpoints(proxier.endpointsMap.LocalReadyEndpoints()); err != nil {
		klog.ErrorS(err, "Error syncing healthcheck endpoints")
	}

	// remove stale endpoint refcount entries
	for epIP := range proxier.terminatedEndpoints {
		if epToDelete := queriedEndpoints[epIP]; epToDelete != nil && epToDelete.hnsID != "" {
			if refCount := proxier.endPointsRefCount.getRefCount(epToDelete.hnsID); refCount == nil || *refCount == 0 {
				klog.V(3).InfoS("Deleting unreferenced remote endpoint", "hnsID", epToDelete.hnsID)
				proxier.hns.deleteEndpoint(epToDelete.hnsID)
			}
		}
	}
	// This will cleanup stale load balancers which are pending delete
	// in last iteration
	proxier.cleanupStaleLoadbalancers()
}

// deleteExistingLoadBalancer checks whether loadbalancer delete is needed or not.
// If it is needed, the function will delete the existing loadbalancer and return true, else false.
func (proxier *Proxier) deleteExistingLoadBalancer(hns HostNetworkService, winProxyOptimization bool, lbHnsID *string, vip string, protocol, intPort, extPort uint16, endpoints []endpointInfo, queriedLoadBalancers map[loadBalancerIdentifier]*loadBalancerInfo) bool {

	if !winProxyOptimization || *lbHnsID == "" {
		// Loadbalancer delete not needed
		return false
	}

	lbID, lbIdErr := findLoadBalancerID(
		endpoints,
		vip,
		protocol,
		intPort,
		extPort,
	)

	if lbIdErr != nil {
		return proxier.deleteLoadBalancer(hns, lbHnsID)
	}

	if _, ok := queriedLoadBalancers[lbID]; ok {
		// The existing loadbalancer in the system is same as what we try to delete and recreate. So we skip deleting.
		return false
	}

	return proxier.deleteLoadBalancer(hns, lbHnsID)
}

func (proxier *Proxier) deleteLoadBalancer(hns HostNetworkService, lbHnsID *string) bool {
	klog.V(3).InfoS("Hns LoadBalancer delete triggered for loadBalancer resources", "lbHnsID", *lbHnsID)
	if err := hns.deleteLoadBalancer(*lbHnsID); err != nil {
		// This will be cleanup by cleanupStaleLoadbalancer fnction.
		proxier.mapStaleLoadbalancers[*lbHnsID] = true
	}
	*lbHnsID = ""
	return true
}
