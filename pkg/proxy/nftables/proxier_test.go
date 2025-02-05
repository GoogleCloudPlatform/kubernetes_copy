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

import (
	"fmt"
	"golang.org/x/time/rate"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/lithammer/dedent"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/proxy"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/pkg/proxy/conntrack"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	proxyutiltest "k8s.io/kubernetes/pkg/proxy/util/testing"
	"k8s.io/kubernetes/pkg/util/async"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"
)

// Conventions for tests using NewFakeProxier:
//
// Pod IPs:             10.0.0.0/8
// Service ClusterIPs:  172.30.0.0/16
// Node IPs:            192.168.0.0/24
// Local Node IP:       192.168.0.2
// Service ExternalIPs: 192.168.99.0/24
// LoadBalancer IPs:    1.2.3.4, 5.6.7.8, 9.10.11.12
// Non-cluster IPs:     203.0.113.0/24
// LB Source Range:     203.0.113.0/25

const testHostname = "test-hostname"
const testNodeIP = "192.168.0.2"
const testNodeIPAlt = "192.168.1.2"
const testExternalIP = "192.168.99.11"
const testNodeIPv6 = "2001:db8::1"
const testNodeIPv6Alt = "2001:db8:1::2"
const testExternalClient = "203.0.113.2"
const testExternalClientBlocked = "203.0.113.130"

var testNodeIPs = []string{testNodeIP, testNodeIPAlt, testExternalIP, testNodeIPv6, testNodeIPv6Alt}

func NewFakeProxier(ipFamily v1.IPFamily) (*knftables.Fake, *Proxier) {
	// TODO: Call NewProxier after refactoring out the goroutine
	// invocation into a Run() method.
	nftablesFamily := knftables.IPv4Family
	podCIDR := "10.0.0.0/8"
	serviceCIDRs := "172.30.0.0/16"
	if ipFamily == v1.IPv6Protocol {
		nftablesFamily = knftables.IPv6Family
		podCIDR = "fd00:10::/64"
		serviceCIDRs = "fd00:10:96::/112"
	}
	detectLocal := proxyutil.NewDetectLocalByCIDR(podCIDR)
	nodePortAddresses := []string{fmt.Sprintf("%s/32", testNodeIP), fmt.Sprintf("%s/128", testNodeIPv6)}

	networkInterfacer := proxyutiltest.NewFakeNetwork()
	itf := net.Interface{Index: 0, MTU: 0, Name: "lo", HardwareAddr: nil, Flags: 0}
	addrs := []net.Addr{
		&net.IPNet{IP: netutils.ParseIPSloppy("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: netutils.ParseIPSloppy("::1/128"), Mask: net.CIDRMask(128, 128)},
	}
	networkInterfacer.AddInterfaceAddr(&itf, addrs)
	itf1 := net.Interface{Index: 1, MTU: 0, Name: "eth0", HardwareAddr: nil, Flags: 0}
	addrs1 := []net.Addr{
		&net.IPNet{IP: netutils.ParseIPSloppy(testNodeIP), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: netutils.ParseIPSloppy(testNodeIPAlt), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: netutils.ParseIPSloppy(testExternalIP), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: netutils.ParseIPSloppy(testNodeIPv6), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: netutils.ParseIPSloppy(testNodeIPv6Alt), Mask: net.CIDRMask(64, 128)},
	}
	networkInterfacer.AddInterfaceAddr(&itf1, addrs1)

	nft := knftables.NewFake(nftablesFamily, kubeProxyTable)

	var nodeIP net.IP
	if ipFamily == v1.IPv4Protocol {
		nodeIP = netutils.ParseIPSloppy(testNodeIP)
	} else {
		nodeIP = netutils.ParseIPSloppy(testNodeIPv6)
	}
	p := &Proxier{
		ipFamily:            ipFamily,
		svcPortMap:          make(proxy.ServicePortMap),
		serviceChanges:      proxy.NewServiceChangeTracker(ipFamily, newServiceInfo, nil),
		endpointsMap:        make(proxy.EndpointsMap),
		endpointsChanges:    proxy.NewEndpointsChangeTracker(ipFamily, testHostname, newEndpointInfo, nil),
		needFullSync:        true,
		nftables:            nft,
		masqueradeMark:      "0x4000",
		conntrack:           conntrack.NewFake(),
		localDetector:       detectLocal,
		hostname:            testHostname,
		serviceHealthServer: healthcheck.NewFakeServiceHealthServer(),
		nodeIP:              nodeIP,
		nodePortAddresses:   proxyutil.NewNodePortAddresses(ipFamily, nodePortAddresses),
		networkInterfacer:   networkInterfacer,
		staleChains:         make(map[string]time.Time),
		serviceCIDRs:        serviceCIDRs,
		logRateLimiter:      rate.NewLimiter(rate.Every(24*time.Hour), 1),
		clusterIPs:          newNFTElementStorage("set", clusterIPsSet),
		serviceIPs:          newNFTElementStorage("map", serviceIPsMap),
		firewallIPs:         newNFTElementStorage("map", firewallIPsMap),
		noEndpointServices:  newNFTElementStorage("map", noEndpointServicesMap),
		noEndpointNodePorts: newNFTElementStorage("map", noEndpointNodePortsMap),
		serviceNodePorts:    newNFTElementStorage("map", serviceNodePortsMap),
	}
	p.setInitialized(true)
	p.syncRunner = async.NewBoundedFrequencyRunner("test-sync-runner", p.syncProxyRules, 0, time.Minute, 1)

	return nft, p
}

var baseRules = dedent.Dedent(`
	add table ip kube-proxy { comment "rules for kube-proxy" ; }

	add chain ip kube-proxy cluster-ips-check
	add chain ip kube-proxy filter-prerouting { type filter hook prerouting priority -110 ; }
	add chain ip kube-proxy filter-forward { type filter hook forward priority -110 ; }
	add chain ip kube-proxy filter-input { type filter hook input priority -110 ; }
	add chain ip kube-proxy filter-output { type filter hook output priority -110 ; }
	add chain ip kube-proxy filter-output-post-dnat { type filter hook output priority -90 ; }
	add chain ip kube-proxy firewall-check
	add chain ip kube-proxy mark-for-masquerade
	add chain ip kube-proxy masquerading
	add chain ip kube-proxy nat-output { type nat hook output priority -100 ; }
	add chain ip kube-proxy nat-postrouting { type nat hook postrouting priority 100 ; }
	add chain ip kube-proxy nat-prerouting { type nat hook prerouting priority -100 ; }
	add chain ip kube-proxy nodeport-endpoints-check
	add chain ip kube-proxy reject-chain { comment "helper for @no-endpoint-services / @no-endpoint-nodeports" ; }
	add chain ip kube-proxy services
	add chain ip kube-proxy service-endpoints-check

	add counter ip kube-proxy traffic-to-invalid-ports { packets 0 bytes 0 ; comment "track packets sent to invalid ports of cluster ips" ; }

	add rule ip kube-proxy cluster-ips-check ip daddr @cluster-ips counter name traffic-to-invalid-ports reject comment "Reject traffic to invalid ports of ClusterIPs"
	add rule ip kube-proxy cluster-ips-check ip daddr { 172.30.0.0/16 } drop comment "Drop traffic to unallocated ClusterIPs"
	add rule ip kube-proxy filter-prerouting ct state new jump firewall-check
	add rule ip kube-proxy filter-forward ct state new jump service-endpoints-check
	add rule ip kube-proxy filter-forward ct state new jump cluster-ips-check
	add rule ip kube-proxy filter-input ct state new jump nodeport-endpoints-check
	add rule ip kube-proxy filter-input ct state new jump service-endpoints-check
	add rule ip kube-proxy filter-output ct state new jump service-endpoints-check
	add rule ip kube-proxy filter-output ct state new jump firewall-check
	add rule ip kube-proxy filter-output-post-dnat ct state new jump cluster-ips-check
	add rule ip kube-proxy firewall-check ip daddr . meta l4proto . th dport vmap @firewall-ips
	add rule ip kube-proxy mark-for-masquerade mark set mark or 0x4000
	add rule ip kube-proxy masquerading mark and 0x4000 == 0 return
	add rule ip kube-proxy masquerading mark set mark xor 0x4000
	add rule ip kube-proxy masquerading masquerade fully-random
	add rule ip kube-proxy nat-output jump services
	add rule ip kube-proxy nat-postrouting jump masquerading
	add rule ip kube-proxy nat-prerouting jump services
	add rule ip kube-proxy nodeport-endpoints-check ip daddr @nodeport-ips meta l4proto . th dport vmap @no-endpoint-nodeports
	add rule ip kube-proxy reject-chain reject
	add rule ip kube-proxy services ip daddr . meta l4proto . th dport vmap @service-ips
	add rule ip kube-proxy services ip daddr @nodeport-ips meta l4proto . th dport vmap @service-nodeports
	add set ip kube-proxy cluster-ips { type ipv4_addr ; comment "Active ClusterIPs" ; }
	add set ip kube-proxy nodeport-ips { type ipv4_addr ; comment "IPs that accept NodePort traffic" ; }
	add element ip kube-proxy nodeport-ips { 192.168.0.2 }
	add rule ip kube-proxy service-endpoints-check ip daddr . meta l4proto . th dport vmap @no-endpoint-services

	add map ip kube-proxy firewall-ips { type ipv4_addr . inet_proto . inet_service : verdict ; comment "destinations that are subject to LoadBalancerSourceRanges" ; }
	add map ip kube-proxy no-endpoint-nodeports { type inet_proto . inet_service : verdict ; comment "vmap to drop or reject packets to service nodeports with no endpoints" ; }
	add map ip kube-proxy no-endpoint-services { type ipv4_addr . inet_proto . inet_service : verdict ; comment "vmap to drop or reject packets to services with no endpoints" ; }
	add map ip kube-proxy service-ips { type ipv4_addr . inet_proto . inet_service : verdict ; comment "ClusterIP, ExternalIP and LoadBalancer IP traffic" ; }
	add map ip kube-proxy service-nodeports { type inet_proto . inet_service : verdict ; comment "NodePort traffic" ; }
	`)

// TestOverallNFTablesRules creates a variety of services and verifies that the generated
// rules are exactly as expected.
func TestOverallNFTablesRules(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)
	metrics.RegisterMetrics(kubeproxyconfig.ProxyModeNFTables)

	makeServiceMap(fp,
		// create ClusterIP service
		makeTestService("ns1", "svc1", func(svc *v1.Service) {
			svc.Spec.ClusterIP = "172.30.0.41"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
			}}
		}),
		// create LoadBalancer service with Local traffic policy
		makeTestService("ns2", "svc2", func(svc *v1.Service) {
			svc.Spec.Type = "LoadBalancer"
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
			svc.Spec.ClusterIP = "172.30.0.42"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
				NodePort: 3001,
			}}
			svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
				IP: "1.2.3.4",
			}}
			svc.Spec.ExternalIPs = []string{"192.168.99.22"}
			svc.Spec.HealthCheckNodePort = 30000
		}),
		// create NodePort service
		makeTestService("ns3", "svc3", func(svc *v1.Service) {
			svc.Spec.Type = "NodePort"
			svc.Spec.ClusterIP = "172.30.0.43"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
				NodePort: 3003,
			}}
		}),
		// create ExternalIP service
		makeTestService("ns4", "svc4", func(svc *v1.Service) {
			svc.Spec.Type = "NodePort"
			svc.Spec.ClusterIP = "172.30.0.44"
			svc.Spec.ExternalIPs = []string{"192.168.99.33"}
			svc.Spec.Ports = []v1.ServicePort{{
				Name:       "p80",
				Port:       80,
				Protocol:   v1.ProtocolTCP,
				TargetPort: intstr.FromInt32(80),
			}}
		}),
		// create LoadBalancer service with Cluster traffic policy, source ranges,
		// and session affinity
		makeTestService("ns5", "svc5", func(svc *v1.Service) {
			svc.Spec.Type = "LoadBalancer"
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyCluster
			svc.Spec.ClusterIP = "172.30.0.45"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
				NodePort: 3002,
			}}
			svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
				IP: "5.6.7.8",
			}}
			svc.Spec.HealthCheckNodePort = 30000
			// Extra whitespace to ensure that invalid value will not result
			// in a crash, for backward compatibility.
			svc.Spec.LoadBalancerSourceRanges = []string{" 203.0.113.0/25"}

			svc.Spec.SessionAffinity = v1.ServiceAffinityClientIP
			svc.Spec.SessionAffinityConfig = &v1.SessionAffinityConfig{
				ClientIP: &v1.ClientIPConfig{
					TimeoutSeconds: ptr.To[int32](10800),
				},
			}
		}),
		// create ClusterIP service with no endpoints
		makeTestService("ns6", "svc6", func(svc *v1.Service) {
			svc.Spec.Type = "ClusterIP"
			svc.Spec.ClusterIP = "172.30.0.46"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:       "p80",
				Port:       80,
				Protocol:   v1.ProtocolTCP,
				TargetPort: intstr.FromInt32(80),
			}}
		}),
	)
	populateEndpointSlices(fp,
		// create ClusterIP service endpoints
		makeTestEndpointSlice("ns1", "svc1", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.180.0.1"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
		// create Local LoadBalancer endpoints. Note that since we aren't setting
		// its NodeName, this endpoint will be considered non-local and ignored.
		makeTestEndpointSlice("ns2", "svc2", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.180.0.2"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
		// create NodePort service endpoints
		makeTestEndpointSlice("ns3", "svc3", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.180.0.3"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
		// create ExternalIP service endpoints
		makeTestEndpointSlice("ns4", "svc4", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.180.0.4"},
			}, {
				Addresses: []string{"10.180.0.5"},
				NodeName:  ptr.To(testHostname),
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
		// create Cluster LoadBalancer endpoints
		makeTestEndpointSlice("ns5", "svc5", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.180.0.3"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)

	fp.syncProxyRules()

	expected := baseRules + dedent.Dedent(`
		# svc1
		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80 }

		add chain ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80
		add rule ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80 ip saddr 10.180.0.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80 meta l4proto tcp dnat to 10.180.0.1:80

		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }

		# svc2
		add chain ip kube-proxy service-42NFTM6N-ns2/svc2/tcp/p80
		add rule ip kube-proxy service-42NFTM6N-ns2/svc2/tcp/p80 ip daddr 172.30.0.42 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-42NFTM6N-ns2/svc2/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-SGOXE6O3-ns2/svc2/tcp/p80__10.180.0.2/80 }
		add chain ip kube-proxy external-42NFTM6N-ns2/svc2/tcp/p80
		add rule ip kube-proxy external-42NFTM6N-ns2/svc2/tcp/p80 ip saddr 10.0.0.0/8 goto service-42NFTM6N-ns2/svc2/tcp/p80 comment "short-circuit pod traffic"
		add rule ip kube-proxy external-42NFTM6N-ns2/svc2/tcp/p80 fib saddr type local jump mark-for-masquerade comment "masquerade local traffic"
		add rule ip kube-proxy external-42NFTM6N-ns2/svc2/tcp/p80 fib saddr type local goto service-42NFTM6N-ns2/svc2/tcp/p80 comment "short-circuit local traffic"
		add chain ip kube-proxy endpoint-SGOXE6O3-ns2/svc2/tcp/p80__10.180.0.2/80
		add rule ip kube-proxy endpoint-SGOXE6O3-ns2/svc2/tcp/p80__10.180.0.2/80 ip saddr 10.180.0.2 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-SGOXE6O3-ns2/svc2/tcp/p80__10.180.0.2/80 meta l4proto tcp dnat to 10.180.0.2:80

		add element ip kube-proxy cluster-ips { 172.30.0.42 }
		add element ip kube-proxy service-ips { 172.30.0.42 . tcp . 80 : goto service-42NFTM6N-ns2/svc2/tcp/p80 }
		add element ip kube-proxy service-ips { 192.168.99.22 . tcp . 80 : goto external-42NFTM6N-ns2/svc2/tcp/p80 }
		add element ip kube-proxy service-ips { 1.2.3.4 . tcp . 80 : goto external-42NFTM6N-ns2/svc2/tcp/p80 }
		add element ip kube-proxy service-nodeports { tcp . 3001 : goto external-42NFTM6N-ns2/svc2/tcp/p80 }

		add element ip kube-proxy no-endpoint-nodeports { tcp . 3001 comment "ns2/svc2:p80" : drop }
		add element ip kube-proxy no-endpoint-services { 1.2.3.4 . tcp . 80 comment "ns2/svc2:p80" : drop }
		add element ip kube-proxy no-endpoint-services { 192.168.99.22 . tcp . 80 comment "ns2/svc2:p80" : drop }

		# svc3
		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-UEIP74TE-ns3/svc3/tcp/p80__10.180.0.3/80 }
		add chain ip kube-proxy external-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy external-4AT6LBPK-ns3/svc3/tcp/p80 jump mark-for-masquerade
		add rule ip kube-proxy external-4AT6LBPK-ns3/svc3/tcp/p80 goto service-4AT6LBPK-ns3/svc3/tcp/p80
		add chain ip kube-proxy endpoint-UEIP74TE-ns3/svc3/tcp/p80__10.180.0.3/80
		add rule ip kube-proxy endpoint-UEIP74TE-ns3/svc3/tcp/p80__10.180.0.3/80 ip saddr 10.180.0.3 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-UEIP74TE-ns3/svc3/tcp/p80__10.180.0.3/80 meta l4proto tcp dnat to 10.180.0.3:80

		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }
		add element ip kube-proxy service-nodeports { tcp . 3003 : goto external-4AT6LBPK-ns3/svc3/tcp/p80 }

		# svc4
		add chain ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 ip daddr 172.30.0.44 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 numgen random mod 2 vmap { 0 : goto endpoint-UNZV3OEC-ns4/svc4/tcp/p80__10.180.0.4/80 , 1 : goto endpoint-5RFCDDV7-ns4/svc4/tcp/p80__10.180.0.5/80 }
		add chain ip kube-proxy external-LAUZTJTB-ns4/svc4/tcp/p80
		add rule ip kube-proxy external-LAUZTJTB-ns4/svc4/tcp/p80 jump mark-for-masquerade
		add rule ip kube-proxy external-LAUZTJTB-ns4/svc4/tcp/p80 goto service-LAUZTJTB-ns4/svc4/tcp/p80
		add chain ip kube-proxy endpoint-5RFCDDV7-ns4/svc4/tcp/p80__10.180.0.5/80
		add rule ip kube-proxy endpoint-5RFCDDV7-ns4/svc4/tcp/p80__10.180.0.5/80 ip saddr 10.180.0.5 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5RFCDDV7-ns4/svc4/tcp/p80__10.180.0.5/80 meta l4proto tcp dnat to 10.180.0.5:80
		add chain ip kube-proxy endpoint-UNZV3OEC-ns4/svc4/tcp/p80__10.180.0.4/80
		add rule ip kube-proxy endpoint-UNZV3OEC-ns4/svc4/tcp/p80__10.180.0.4/80 ip saddr 10.180.0.4 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-UNZV3OEC-ns4/svc4/tcp/p80__10.180.0.4/80 meta l4proto tcp dnat to 10.180.0.4:80

		add element ip kube-proxy cluster-ips { 172.30.0.44 }
		add element ip kube-proxy service-ips { 172.30.0.44 . tcp . 80 : goto service-LAUZTJTB-ns4/svc4/tcp/p80 }
		add element ip kube-proxy service-ips { 192.168.99.33 . tcp . 80 : goto external-LAUZTJTB-ns4/svc4/tcp/p80 }

		# svc5
		add set ip kube-proxy affinity-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80 { type ipv4_addr ; flags dynamic,timeout ; timeout 10800s ; }
		add chain ip kube-proxy service-HVFWP5L3-ns5/svc5/tcp/p80
		add rule ip kube-proxy service-HVFWP5L3-ns5/svc5/tcp/p80 ip daddr 172.30.0.45 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-HVFWP5L3-ns5/svc5/tcp/p80 ip saddr @affinity-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80 goto endpoint-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80
		add rule ip kube-proxy service-HVFWP5L3-ns5/svc5/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80 }
		add chain ip kube-proxy external-HVFWP5L3-ns5/svc5/tcp/p80
		add rule ip kube-proxy external-HVFWP5L3-ns5/svc5/tcp/p80 jump mark-for-masquerade
		add rule ip kube-proxy external-HVFWP5L3-ns5/svc5/tcp/p80 goto service-HVFWP5L3-ns5/svc5/tcp/p80

		add chain ip kube-proxy endpoint-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80
		add rule ip kube-proxy endpoint-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80 ip saddr 10.180.0.3 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80 update @affinity-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80 { ip saddr }
		add rule ip kube-proxy endpoint-GTK6MW7G-ns5/svc5/tcp/p80__10.180.0.3/80 meta l4proto tcp dnat to 10.180.0.3:80

		add chain ip kube-proxy firewall-HVFWP5L3-ns5/svc5/tcp/p80
		add rule ip kube-proxy firewall-HVFWP5L3-ns5/svc5/tcp/p80 ip saddr != { 203.0.113.0/25 } drop

		add element ip kube-proxy cluster-ips { 172.30.0.45 }
		add element ip kube-proxy service-ips { 172.30.0.45 . tcp . 80 : goto service-HVFWP5L3-ns5/svc5/tcp/p80 }
		add element ip kube-proxy service-ips { 5.6.7.8 . tcp . 80 : goto external-HVFWP5L3-ns5/svc5/tcp/p80 }
		add element ip kube-proxy service-nodeports { tcp . 3002 : goto external-HVFWP5L3-ns5/svc5/tcp/p80 }
		add element ip kube-proxy firewall-ips { 5.6.7.8 . tcp . 80 : goto firewall-HVFWP5L3-ns5/svc5/tcp/p80 }

		# svc6
		add element ip kube-proxy cluster-ips { 172.30.0.46 }
		add element ip kube-proxy no-endpoint-services { 172.30.0.46 . tcp . 80 comment "ns6/svc6:p80" : goto reject-chain }
		`)

	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
}

// TestNoEndpointsReject tests that a service with no endpoints rejects connections to
// its ClusterIP, ExternalIPs, NodePort, and LoadBalancer IP.
func TestNoEndpointsReject(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)
	svcIP := "172.30.0.41"
	svcPort := 80
	svcNodePort := 3001
	svcExternalIPs := "192.168.99.11"
	svcLBIP := "1.2.3.4"
	svcPortName := proxy.ServicePortName{
		NamespacedName: makeNSN("ns1", "svc1"),
		Port:           "p80",
	}

	makeServiceMap(fp,
		makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeLoadBalancer
			svc.Spec.ClusterIP = svcIP
			svc.Spec.ExternalIPs = []string{svcExternalIPs}
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     svcPortName.Port,
				Protocol: v1.ProtocolTCP,
				Port:     int32(svcPort),
				NodePort: int32(svcNodePort),
			}}
			svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
				IP: svcLBIP,
			}}
		}),
	)
	fp.syncProxyRules()

	runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
		{
			name:     "pod to cluster IP with no endpoints",
			sourceIP: "10.0.0.2",
			destIP:   svcIP,
			destPort: svcPort,
			output:   "REJECT",
		},
		{
			name:     "external to external IP with no endpoints",
			sourceIP: testExternalClient,
			destIP:   svcExternalIPs,
			destPort: svcPort,
			output:   "REJECT",
		},
		{
			name:     "pod to NodePort with no endpoints",
			sourceIP: "10.0.0.2",
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   "REJECT",
		},
		{
			name:     "external to NodePort with no endpoints",
			sourceIP: testExternalClient,
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   "REJECT",
		},
		{
			name:     "pod to LoadBalancer IP with no endpoints",
			sourceIP: "10.0.0.2",
			destIP:   svcLBIP,
			destPort: svcPort,
			output:   "REJECT",
		},
		{
			name:     "external to LoadBalancer IP with no endpoints",
			sourceIP: testExternalClient,
			destIP:   svcLBIP,
			destPort: svcPort,
			output:   "REJECT",
		},
	})
}

// TestClusterIPGeneral tests various basic features of a ClusterIP service
func TestClusterIPGeneral(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)

	makeServiceMap(fp,
		makeTestService("ns1", "svc1", func(svc *v1.Service) {
			svc.Spec.ClusterIP = "172.30.0.41"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "http",
				Port:     80,
				Protocol: v1.ProtocolTCP,
			}}
		}),
		makeTestService("ns2", "svc2", func(svc *v1.Service) {
			svc.Spec.ClusterIP = "172.30.0.42"
			svc.Spec.Ports = []v1.ServicePort{
				{
					Name:     "http",
					Port:     80,
					Protocol: v1.ProtocolTCP,
				},
				{
					Name:       "https",
					Port:       443,
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.FromInt32(8443),
				},
				{
					Name:     "dns-udp",
					Port:     53,
					Protocol: v1.ProtocolUDP,
				},
				{
					Name:     "dns-tcp",
					Port:     53,
					Protocol: v1.ProtocolTCP,
					// We use TargetPort on TCP but not UDP/SCTP to
					// help disambiguate the output.
					TargetPort: intstr.FromInt32(5353),
				},
				{
					Name:     "dns-sctp",
					Port:     53,
					Protocol: v1.ProtocolSCTP,
				},
			}
		}),
	)

	populateEndpointSlices(fp,
		makeTestEndpointSlice("ns1", "svc1", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.180.0.1"},
				NodeName:  ptr.To(testHostname),
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("http"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
		makeTestEndpointSlice("ns2", "svc2", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{
				{
					Addresses: []string{"10.180.0.1"},
					NodeName:  ptr.To(testHostname),
				},
				{
					Addresses: []string{"10.180.2.1"},
					NodeName:  ptr.To("host2"),
				},
			}
			eps.Ports = []discovery.EndpointPort{
				{
					Name:     ptr.To("http"),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				},
				{
					Name:     ptr.To("https"),
					Port:     ptr.To[int32](8443),
					Protocol: ptr.To(v1.ProtocolTCP),
				},
				{
					Name:     ptr.To("dns-udp"),
					Port:     ptr.To[int32](53),
					Protocol: ptr.To(v1.ProtocolUDP),
				},
				{
					Name:     ptr.To("dns-tcp"),
					Port:     ptr.To[int32](5353),
					Protocol: ptr.To(v1.ProtocolTCP),
				},
				{
					Name:     ptr.To("dns-sctp"),
					Port:     ptr.To[int32](53),
					Protocol: ptr.To(v1.ProtocolSCTP),
				},
			}
		}),
	)

	fp.syncProxyRules()

	runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
		{
			name:     "simple clusterIP",
			sourceIP: "10.180.0.2",
			destIP:   "172.30.0.41",
			destPort: 80,
			output:   "10.180.0.1:80",
			masq:     false,
		},
		{
			name:     "hairpin to cluster IP",
			sourceIP: "10.180.0.1",
			destIP:   "172.30.0.41",
			destPort: 80,
			output:   "10.180.0.1:80",
			masq:     true,
		},
		{
			name:     "clusterIP with multiple endpoints",
			sourceIP: "10.180.0.2",
			destIP:   "172.30.0.42",
			destPort: 80,
			output:   "10.180.0.1:80, 10.180.2.1:80",
			masq:     false,
		},
		{
			name:     "clusterIP with TargetPort",
			sourceIP: "10.180.0.2",
			destIP:   "172.30.0.42",
			destPort: 443,
			output:   "10.180.0.1:8443, 10.180.2.1:8443",
			masq:     false,
		},
		{
			name:     "clusterIP with TCP, UDP, and SCTP on same port (TCP)",
			sourceIP: "10.180.0.2",
			protocol: v1.ProtocolTCP,
			destIP:   "172.30.0.42",
			destPort: 53,
			output:   "10.180.0.1:5353, 10.180.2.1:5353",
			masq:     false,
		},
		{
			name:     "clusterIP with TCP, UDP, and SCTP on same port (TCP)",
			sourceIP: "10.180.0.2",
			protocol: v1.ProtocolUDP,
			destIP:   "172.30.0.42",
			destPort: 53,
			output:   "10.180.0.1:53, 10.180.2.1:53",
			masq:     false,
		},
		{
			name:     "clusterIP with TCP, UDP, and SCTP on same port (SCTP)",
			sourceIP: "10.180.0.2",
			protocol: v1.ProtocolSCTP,
			destIP:   "172.30.0.42",
			destPort: 53,
			output:   "10.180.0.1:53, 10.180.2.1:53",
			masq:     false,
		},
		{
			name:     "TCP-only port does not match UDP traffic",
			sourceIP: "10.180.0.2",
			protocol: v1.ProtocolUDP,
			destIP:   "172.30.0.42",
			destPort: 80,
			output:   "REJECT",
		},
		{
			name:     "svc1 does not accept svc2's ports",
			sourceIP: "10.180.0.2",
			destIP:   "172.30.0.41",
			destPort: 443,
			output:   "REJECT",
		},
		{
			name:     "packet to unallocated cluster ip",
			sourceIP: "10.180.0.2",
			destIP:   "172.30.0.50",
			destPort: 80,
			output:   "DROP",
		},
	})
}

func TestLoadBalancer(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)
	svcIP := "172.30.0.41"
	svcPort := 80
	svcNodePort := 3001
	svcLBIP1 := "1.2.3.4"
	svcLBIP2 := "5.6.7.8"
	svcPortName := proxy.ServicePortName{
		NamespacedName: makeNSN("ns1", "svc1"),
		Port:           "p80",
		Protocol:       v1.ProtocolTCP,
	}

	makeServiceMap(fp,
		makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *v1.Service) {
			svc.Spec.Type = "LoadBalancer"
			svc.Spec.ClusterIP = svcIP
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     svcPortName.Port,
				Port:     int32(svcPort),
				Protocol: v1.ProtocolTCP,
				NodePort: int32(svcNodePort),
			}}
			svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{
				{IP: svcLBIP1},
				{IP: svcLBIP2},
			}
			svc.Spec.LoadBalancerSourceRanges = []string{
				"192.168.0.0/24",

				// Regression test that excess whitespace gets ignored
				" 203.0.113.0/25",
			}
		}),
	)

	epIP := "10.180.0.1"
	populateEndpointSlices(fp,
		makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{epIP},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To(svcPortName.Port),
				Port:     ptr.To(int32(svcPort)),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)

	fp.syncProxyRules()

	runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
		{
			name:     "pod to cluster IP",
			sourceIP: "10.0.0.2",
			destIP:   svcIP,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     false,
		},
		{
			name:     "external to nodePort",
			sourceIP: testExternalClient,
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},
		{
			name:     "nodePort bypasses LoadBalancerSourceRanges",
			sourceIP: testExternalClientBlocked,
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},
		{
			name:     "accepted external to LB1",
			sourceIP: testExternalClient,
			destIP:   svcLBIP1,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},
		{
			name:     "accepted external to LB2",
			sourceIP: testExternalClient,
			destIP:   svcLBIP2,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},
		{
			name:     "blocked external to LB1",
			sourceIP: testExternalClientBlocked,
			destIP:   svcLBIP1,
			destPort: svcPort,
			output:   "DROP",
		},
		{
			name:     "blocked external to LB2",
			sourceIP: testExternalClientBlocked,
			destIP:   svcLBIP2,
			destPort: svcPort,
			output:   "DROP",
		},
		{
			name:     "pod to LB1 (blocked by LoadBalancerSourceRanges)",
			sourceIP: "10.0.0.2",
			destIP:   svcLBIP1,
			destPort: svcPort,
			output:   "DROP",
		},
		{
			name:     "pod to LB2 (blocked by LoadBalancerSourceRanges)",
			sourceIP: "10.0.0.2",
			destIP:   svcLBIP2,
			destPort: svcPort,
			output:   "DROP",
		},
		{
			name:     "node to LB1 (allowed by LoadBalancerSourceRanges)",
			sourceIP: testNodeIP,
			destIP:   svcLBIP1,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},
		{
			name:     "node to LB2 (allowed by LoadBalancerSourceRanges)",
			sourceIP: testNodeIP,
			destIP:   svcLBIP2,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},

		// The LB rules assume that when you connect from a node to a LB IP, that
		// something external to kube-proxy will cause the connection to be
		// SNATted to the LB IP, so if the LoadBalancerSourceRanges include the
		// node IP, then we add a rule allowing traffic from the LB IP as well...
		{
			name:     "same node to LB1, SNATted to LB1 (implicitly allowed)",
			sourceIP: svcLBIP1,
			destIP:   svcLBIP1,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},
		{
			name:     "same node to LB2, SNATted to LB2 (implicitly allowed)",
			sourceIP: svcLBIP2,
			destIP:   svcLBIP2,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP, svcPort),
			masq:     true,
		},
	})
}

// TestNodePorts tests NodePort services under various combinations of the
// --nodeport-addresses flags.
func TestNodePorts(t *testing.T) {
	testCases := []struct {
		name string

		family            v1.IPFamily
		nodePortAddresses []string

		// allowAltNodeIP is true if we expect NodePort traffic on the alternate
		// node IP to be accepted
		allowAltNodeIP bool
	}{
		{
			name: "ipv4",

			family:            v1.IPv4Protocol,
			nodePortAddresses: nil,

			allowAltNodeIP: false,
		},
		{
			name: "ipv4, multiple nodeport-addresses",

			family:            v1.IPv4Protocol,
			nodePortAddresses: []string{"192.168.0.0/24", "192.168.1.0/24", "2001:db8::/64"},

			allowAltNodeIP: true,
		},
		{
			name: "ipv6",

			family:            v1.IPv6Protocol,
			nodePortAddresses: nil,

			allowAltNodeIP: false,
		},
		{
			name: "ipv6, multiple nodeport-addresses",

			family:            v1.IPv6Protocol,
			nodePortAddresses: []string{"192.168.0.0/24", "192.168.1.0/24", "2001:db8::/64", "2001:db8:1::2/128"},

			allowAltNodeIP: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nft, fp := NewFakeProxier(tc.family)

			var svcIP, epIP1, epIP2 string
			var nodeIP string
			if tc.family == v1.IPv4Protocol {
				svcIP = "172.30.0.41"
				epIP1 = "10.180.0.1"
				epIP2 = "10.180.2.1"
				nodeIP = testNodeIP
			} else {
				svcIP = "fd00:172:30::41"
				epIP1 = "fd00:10:180::1"
				epIP2 = "fd00:10:180::2:1"
				nodeIP = testNodeIPv6
			}
			if tc.nodePortAddresses != nil {
				fp.nodePortAddresses = proxyutil.NewNodePortAddresses(tc.family, tc.nodePortAddresses)
			}

			makeServiceMap(fp,
				makeTestService("ns1", "svc1", func(svc *v1.Service) {
					svc.Spec.Type = v1.ServiceTypeNodePort
					svc.Spec.ClusterIP = svcIP
					svc.Spec.Ports = []v1.ServicePort{{
						Name:     "p80",
						Port:     80,
						Protocol: v1.ProtocolTCP,
						NodePort: 3001,
					}}
				}),
			)

			populateEndpointSlices(fp,
				makeTestEndpointSlice("ns1", "svc1", 1, func(eps *discovery.EndpointSlice) {
					if tc.family == v1.IPv4Protocol {
						eps.AddressType = discovery.AddressTypeIPv4
					} else {
						eps.AddressType = discovery.AddressTypeIPv6
					}
					eps.Endpoints = []discovery.Endpoint{{
						Addresses: []string{epIP1},
						NodeName:  nil,
					}, {
						Addresses: []string{epIP2},
						NodeName:  ptr.To(testHostname),
					}}
					eps.Ports = []discovery.EndpointPort{{
						Name:     ptr.To("p80"),
						Port:     ptr.To[int32](80),
						Protocol: ptr.To(v1.ProtocolTCP),
					}}
				}),
			)

			fp.syncProxyRules()

			var podIP, externalClientIP, altNodeIP string
			if tc.family == v1.IPv4Protocol {
				podIP = "10.0.0.2"
				externalClientIP = testExternalClient
				altNodeIP = testNodeIPAlt
			} else {
				podIP = "fd00:10::2"
				externalClientIP = "2600:5200::1"
				altNodeIP = testNodeIPv6Alt
			}
			output := net.JoinHostPort(epIP1, "80") + ", " + net.JoinHostPort(epIP2, "80")

			// Basic tests are the same for all cases
			runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
				{
					name:     "pod to cluster IP",
					sourceIP: podIP,
					destIP:   svcIP,
					destPort: 80,
					output:   output,
					masq:     false,
				},
				{
					name:     "external to nodePort",
					sourceIP: externalClientIP,
					destIP:   nodeIP,
					destPort: 3001,
					output:   output,
					masq:     true,
				},
				{
					name:     "node to nodePort",
					sourceIP: nodeIP,
					destIP:   nodeIP,
					destPort: 3001,
					output:   output,
					masq:     true,
				},
			})

			if tc.allowAltNodeIP {
				runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
					{
						name:     "external to nodePort on secondary IP",
						sourceIP: externalClientIP,
						destIP:   altNodeIP,
						destPort: 3001,
						output:   output,
						masq:     true,
					},
				})
			} else {
				runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
					{
						name:     "secondary nodeIP ignores NodePorts",
						sourceIP: externalClientIP,
						destIP:   altNodeIP,
						destPort: 3001,
						output:   "",
					},
				})
			}
		})
	}
}

// TestExternalTrafficPolicyLocal tests that traffic to externally-facing IPs does not get
// masqueraded when using Local traffic policy. For traffic from external sources, that
// means it can also only be routed to local endpoints, but for traffic from internal
// sources, it gets routed to all endpoints.
func TestExternalTrafficPolicyLocal(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)

	svcIP := "172.30.0.41"
	svcPort := 80
	svcNodePort := 3001
	svcHealthCheckNodePort := 30000
	svcExternalIPs := "192.168.99.11"
	svcLBIP := "1.2.3.4"
	svcPortName := proxy.ServicePortName{
		NamespacedName: makeNSN("ns1", "svc1"),
		Port:           "p80",
	}

	makeServiceMap(fp,
		makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeLoadBalancer
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
			svc.Spec.ClusterIP = svcIP
			svc.Spec.ExternalIPs = []string{svcExternalIPs}
			svc.Spec.Ports = []v1.ServicePort{{
				Name:       svcPortName.Port,
				Port:       int32(svcPort),
				Protocol:   v1.ProtocolTCP,
				NodePort:   int32(svcNodePort),
				TargetPort: intstr.FromInt32(int32(svcPort)),
			}}
			svc.Spec.HealthCheckNodePort = int32(svcHealthCheckNodePort)
			svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
				IP: svcLBIP,
			}}
		}),
	)

	epIP1 := "10.180.0.1"
	epIP2 := "10.180.2.1"
	populateEndpointSlices(fp,
		makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{epIP1},
			}, {
				Addresses: []string{epIP2},
				NodeName:  ptr.To(testHostname),
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To(svcPortName.Port),
				Port:     ptr.To(int32(svcPort)),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)

	fp.syncProxyRules()

	runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
		{
			name:     "pod to cluster IP hits both endpoints, unmasqueraded",
			sourceIP: "10.0.0.2",
			destIP:   svcIP,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     false,
		},
		{
			name:     "pod to external IP hits both endpoints, unmasqueraded",
			sourceIP: "10.0.0.2",
			destIP:   svcExternalIPs,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     false,
		},
		{
			name:     "external to external IP hits only local endpoint, unmasqueraded",
			sourceIP: testExternalClient,
			destIP:   svcExternalIPs,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP2, svcPort),
			masq:     false,
		},
		{
			name:     "pod to LB IP hits only both endpoints, unmasqueraded",
			sourceIP: "10.0.0.2",
			destIP:   svcLBIP,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     false,
		},
		{
			name:     "external to LB IP hits only local endpoint, unmasqueraded",
			sourceIP: testExternalClient,
			destIP:   svcLBIP,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d", epIP2, svcPort),
			masq:     false,
		},
		{
			name:     "pod to NodePort hits both endpoints, unmasqueraded",
			sourceIP: "10.0.0.2",
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     false,
		},
		{
			name:     "external to NodePort hits only local endpoint, unmasqueraded",
			sourceIP: testExternalClient,
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   fmt.Sprintf("%s:%d", epIP2, svcPort),
			masq:     false,
		},
	})
}

// TestExternalTrafficPolicyCluster tests that traffic to an externally-facing IP gets
// masqueraded when using Cluster traffic policy.
func TestExternalTrafficPolicyCluster(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)

	svcIP := "172.30.0.41"
	svcPort := 80
	svcNodePort := 3001
	svcExternalIPs := "192.168.99.11"
	svcLBIP := "1.2.3.4"
	svcPortName := proxy.ServicePortName{
		NamespacedName: makeNSN("ns1", "svc1"),
		Port:           "p80",
	}

	makeServiceMap(fp,
		makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeLoadBalancer
			svc.Spec.ClusterIP = svcIP
			svc.Spec.ExternalIPs = []string{svcExternalIPs}
			svc.Spec.Ports = []v1.ServicePort{{
				Name:       svcPortName.Port,
				Port:       int32(svcPort),
				Protocol:   v1.ProtocolTCP,
				NodePort:   int32(svcNodePort),
				TargetPort: intstr.FromInt32(int32(svcPort)),
			}}
			svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
				IP: svcLBIP,
			}}
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyCluster
		}),
	)

	epIP1 := "10.180.0.1"
	epIP2 := "10.180.2.1"
	populateEndpointSlices(fp,
		makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{epIP1},
				NodeName:  nil,
			}, {
				Addresses: []string{epIP2},
				NodeName:  ptr.To(testHostname),
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To(svcPortName.Port),
				Port:     ptr.To(int32(svcPort)),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)

	fp.syncProxyRules()

	runPacketFlowTests(t, getLine(), nft, testNodeIPs, []packetFlowTest{
		{
			name:     "pod to cluster IP hits both endpoints, unmasqueraded",
			sourceIP: "10.0.0.2",
			destIP:   svcIP,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     false,
		},
		{
			name:     "pod to external IP hits both endpoints, masqueraded",
			sourceIP: "10.0.0.2",
			destIP:   svcExternalIPs,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     true,
		},
		{
			name:     "external to external IP hits both endpoints, masqueraded",
			sourceIP: testExternalClient,
			destIP:   svcExternalIPs,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     true,
		},
		{
			name:     "pod to LB IP hits both endpoints, masqueraded",
			sourceIP: "10.0.0.2",
			destIP:   svcLBIP,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     true,
		},
		{
			name:     "external to LB IP hits both endpoints, masqueraded",
			sourceIP: testExternalClient,
			destIP:   svcLBIP,
			destPort: svcPort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     true,
		},
		{
			name:     "pod to NodePort hits both endpoints, masqueraded",
			sourceIP: "10.0.0.2",
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     true,
		},
		{
			name:     "external to NodePort hits both endpoints, masqueraded",
			sourceIP: testExternalClient,
			destIP:   testNodeIP,
			destPort: svcNodePort,
			output:   fmt.Sprintf("%s:%d, %s:%d", epIP1, svcPort, epIP2, svcPort),
			masq:     true,
		},
	})
}

func makeTestService(namespace, name string, svcFunc func(*v1.Service)) *v1.Service {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{},
		},
		Spec:   v1.ServiceSpec{},
		Status: v1.ServiceStatus{},
	}
	svcFunc(svc)
	return svc
}

func addTestPort(array []v1.ServicePort, name string, protocol v1.Protocol, port, nodeport int32, targetPort int) []v1.ServicePort {
	svcPort := v1.ServicePort{
		Name:       name,
		Protocol:   protocol,
		Port:       port,
		NodePort:   nodeport,
		TargetPort: intstr.FromInt32(int32(targetPort)),
	}
	return append(array, svcPort)
}

func TestBuildServiceMapAddRemove(t *testing.T) {
	_, fp := NewFakeProxier(v1.IPv4Protocol)

	services := []*v1.Service{
		makeTestService("somewhere-else", "cluster-ip", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = "172.30.55.4"
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "something", "UDP", 1234, 4321, 0)
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "somethingelse", "UDP", 1235, 5321, 0)
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "sctpport", "SCTP", 1236, 6321, 0)
		}),
		makeTestService("somewhere-else", "node-port", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeNodePort
			svc.Spec.ClusterIP = "172.30.55.10"
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "blahblah", "UDP", 345, 678, 0)
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "moreblahblah", "TCP", 344, 677, 0)
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "muchmoreblah", "SCTP", 343, 676, 0)
		}),
		makeTestService("somewhere", "load-balancer", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeLoadBalancer
			svc.Spec.ClusterIP = "172.30.55.11"
			svc.Spec.LoadBalancerIP = "1.2.3.4"
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "foobar", "UDP", 8675, 30061, 7000)
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "baz", "UDP", 8676, 30062, 7001)
			svc.Status.LoadBalancer = v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "1.2.3.4"},
				},
			}
		}),
		makeTestService("somewhere", "only-local-load-balancer", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeLoadBalancer
			svc.Spec.ClusterIP = "172.30.55.12"
			svc.Spec.LoadBalancerIP = "5.6.7.8"
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "foobar2", "UDP", 8677, 30063, 7002)
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "baz", "UDP", 8678, 30064, 7003)
			svc.Status.LoadBalancer = v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "5.6.7.8"},
				},
			}
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
			svc.Spec.HealthCheckNodePort = 345
		}),
	}

	for i := range services {
		fp.OnServiceAdd(services[i])
	}
	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 10 {
		t.Errorf("expected service map length 10, got %v", fp.svcPortMap)
	}

	// The only-local-loadbalancer ones get added
	healthCheckNodePorts := fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 1 {
		t.Errorf("expected 1 healthcheck port, got %v", healthCheckNodePorts)
	} else {
		nsn := makeNSN("somewhere", "only-local-load-balancer")
		if port, found := healthCheckNodePorts[nsn]; !found || port != 345 {
			t.Errorf("expected healthcheck port [%q]=345: got %v", nsn, healthCheckNodePorts)
		}
	}

	// Remove some stuff
	// oneService is a modification of services[0] with removed first port.
	oneService := makeTestService("somewhere-else", "cluster-ip", func(svc *v1.Service) {
		svc.Spec.Type = v1.ServiceTypeClusterIP
		svc.Spec.ClusterIP = "172.30.55.4"
		svc.Spec.Ports = addTestPort(svc.Spec.Ports, "somethingelse", "UDP", 1235, 5321, 0)
	})

	fp.OnServiceUpdate(services[0], oneService)
	fp.OnServiceDelete(services[1])
	fp.OnServiceDelete(services[2])
	fp.OnServiceDelete(services[3])

	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 1 {
		t.Errorf("expected service map length 1, got %v", fp.svcPortMap)
	}

	healthCheckNodePorts = fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 0 {
		t.Errorf("expected 0 healthcheck ports, got %v", healthCheckNodePorts)
	}
}

func TestBuildServiceMapServiceHeadless(t *testing.T) {
	_, fp := NewFakeProxier(v1.IPv4Protocol)

	makeServiceMap(fp,
		makeTestService("somewhere-else", "headless", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = v1.ClusterIPNone
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "rpc", "UDP", 1234, 0, 0)
		}),
		makeTestService("somewhere-else", "headless-without-port", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = v1.ClusterIPNone
		}),
	)

	// Headless service should be ignored
	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 0 {
		t.Errorf("expected service map length 0, got %d", len(fp.svcPortMap))
	}

	// No proxied services, so no healthchecks
	healthCheckNodePorts := fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 0 {
		t.Errorf("expected healthcheck ports length 0, got %d", len(healthCheckNodePorts))
	}
}

func TestBuildServiceMapServiceTypeExternalName(t *testing.T) {
	_, fp := NewFakeProxier(v1.IPv4Protocol)

	makeServiceMap(fp,
		makeTestService("somewhere-else", "external-name", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeExternalName
			svc.Spec.ClusterIP = "172.30.55.4" // Should be ignored
			svc.Spec.ExternalName = "foo2.bar.com"
			svc.Spec.Ports = addTestPort(svc.Spec.Ports, "blah", "UDP", 1235, 5321, 0)
		}),
	)

	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 0 {
		t.Errorf("expected service map length 0, got %v", fp.svcPortMap)
	}
	// No proxied services, so no healthchecks
	healthCheckNodePorts := fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 0 {
		t.Errorf("expected healthcheck ports length 0, got %v", healthCheckNodePorts)
	}
}

func TestBuildServiceMapServiceUpdate(t *testing.T) {
	_, fp := NewFakeProxier(v1.IPv4Protocol)

	servicev1 := makeTestService("somewhere", "some-service", func(svc *v1.Service) {
		svc.Spec.Type = v1.ServiceTypeClusterIP
		svc.Spec.ClusterIP = "172.30.55.4"
		svc.Spec.Ports = addTestPort(svc.Spec.Ports, "something", "UDP", 1234, 4321, 0)
		svc.Spec.Ports = addTestPort(svc.Spec.Ports, "somethingelse", "TCP", 1235, 5321, 0)
	})
	servicev2 := makeTestService("somewhere", "some-service", func(svc *v1.Service) {
		svc.Spec.Type = v1.ServiceTypeLoadBalancer
		svc.Spec.ClusterIP = "172.30.55.4"
		svc.Spec.LoadBalancerIP = "1.2.3.4"
		svc.Spec.Ports = addTestPort(svc.Spec.Ports, "something", "UDP", 1234, 4321, 7002)
		svc.Spec.Ports = addTestPort(svc.Spec.Ports, "somethingelse", "TCP", 1235, 5321, 7003)
		svc.Status.LoadBalancer = v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{IP: "1.2.3.4"},
			},
		}
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
		svc.Spec.HealthCheckNodePort = 345
	})

	fp.OnServiceAdd(servicev1)

	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 2 {
		t.Errorf("expected service map length 2, got %v", fp.svcPortMap)
	}
	healthCheckNodePorts := fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 0 {
		t.Errorf("expected healthcheck ports length 0, got %v", healthCheckNodePorts)
	}

	// Change service to load-balancer
	fp.OnServiceUpdate(servicev1, servicev2)
	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 2 {
		t.Errorf("expected service map length 2, got %v", fp.svcPortMap)
	}
	healthCheckNodePorts = fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 1 {
		t.Errorf("expected healthcheck ports length 1, got %v", healthCheckNodePorts)
	}

	// No change; make sure the service map stays the same and there are
	// no health-check changes
	fp.OnServiceUpdate(servicev2, servicev2)
	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 2 {
		t.Errorf("expected service map length 2, got %v", fp.svcPortMap)
	}
	healthCheckNodePorts = fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 1 {
		t.Errorf("expected healthcheck ports length 1, got %v", healthCheckNodePorts)
	}

	// And back to ClusterIP
	fp.OnServiceUpdate(servicev2, servicev1)
	fp.svcPortMap.Update(fp.serviceChanges)
	if len(fp.svcPortMap) != 2 {
		t.Errorf("expected service map length 2, got %v", fp.svcPortMap)
	}
	healthCheckNodePorts = fp.svcPortMap.HealthCheckNodePorts()
	if len(healthCheckNodePorts) != 0 {
		t.Errorf("expected healthcheck ports length 0, got %v", healthCheckNodePorts)
	}
}

func populateEndpointSlices(proxier *Proxier, allEndpointSlices ...*discovery.EndpointSlice) {
	for i := range allEndpointSlices {
		proxier.OnEndpointSliceAdd(allEndpointSlices[i])
	}
}

func makeTestEndpointSlice(namespace, name string, sliceNum int, epsFunc func(*discovery.EndpointSlice)) *discovery.EndpointSlice {
	eps := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", name, sliceNum),
			Namespace: namespace,
			Labels:    map[string]string{discovery.LabelServiceName: name},
		},
	}
	epsFunc(eps)
	return eps
}

func makeNSN(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func makeServicePortName(ns, name, port string, protocol v1.Protocol) proxy.ServicePortName {
	return proxy.ServicePortName{
		NamespacedName: makeNSN(ns, name),
		Port:           port,
		Protocol:       protocol,
	}
}

func makeServiceMap(proxier *Proxier, allServices ...*v1.Service) {
	for i := range allServices {
		proxier.OnServiceAdd(allServices[i])
	}

	proxier.mu.Lock()
	defer proxier.mu.Unlock()
	proxier.servicesSynced = true
}

type endpointExpectation struct {
	endpoint string
	isLocal  bool
}

func checkEndpointExpectations(t *testing.T, tci int, newMap proxy.EndpointsMap, expected map[proxy.ServicePortName][]endpointExpectation) {
	if len(newMap) != len(expected) {
		t.Errorf("[%d] expected %d results, got %d: %v", tci, len(expected), len(newMap), newMap)
	}
	for x := range expected {
		if len(newMap[x]) != len(expected[x]) {
			t.Errorf("[%d] expected %d endpoints for %v, got %d", tci, len(expected[x]), x, len(newMap[x]))
		} else {
			for i := range expected[x] {
				newEp := newMap[x][i]
				if newEp.String() != expected[x][i].endpoint ||
					newEp.IsLocal() != expected[x][i].isLocal {
					t.Errorf("[%d] expected new[%v][%d] to be %v, got %v", tci, x, i, expected[x][i], newEp)
				}
			}
		}
	}
}

func TestUpdateEndpointsMap(t *testing.T) {
	emptyEndpointSlices := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, func(*discovery.EndpointSlice) {}),
	}
	subset1 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p11"),
			Port:     ptr.To[int32](11),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	subset2 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.2"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p12"),
			Port:     ptr.To[int32](12),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	namedPortLocal := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1,
			func(eps *discovery.EndpointSlice) {
				eps.AddressType = discovery.AddressTypeIPv4
				eps.Endpoints = []discovery.Endpoint{{
					Addresses: []string{"10.1.1.1"},
					NodeName:  ptr.To(testHostname),
				}}
				eps.Ports = []discovery.EndpointPort{{
					Name:     ptr.To("p11"),
					Port:     ptr.To[int32](11),
					Protocol: ptr.To(v1.ProtocolUDP),
				}}
			}),
	}
	namedPort := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, subset1),
	}
	namedPortRenamed := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1,
			func(eps *discovery.EndpointSlice) {
				eps.AddressType = discovery.AddressTypeIPv4
				eps.Endpoints = []discovery.Endpoint{{
					Addresses: []string{"10.1.1.1"},
				}}
				eps.Ports = []discovery.EndpointPort{{
					Name:     ptr.To("p11-2"),
					Port:     ptr.To[int32](11),
					Protocol: ptr.To(v1.ProtocolUDP),
				}}
			}),
	}
	namedPortRenumbered := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1,
			func(eps *discovery.EndpointSlice) {
				eps.AddressType = discovery.AddressTypeIPv4
				eps.Endpoints = []discovery.Endpoint{{
					Addresses: []string{"10.1.1.1"},
				}}
				eps.Ports = []discovery.EndpointPort{{
					Name:     ptr.To("p11"),
					Port:     ptr.To[int32](22),
					Protocol: ptr.To(v1.ProtocolUDP),
				}}
			}),
	}
	namedPortsLocalNoLocal := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1,
			func(eps *discovery.EndpointSlice) {
				eps.AddressType = discovery.AddressTypeIPv4
				eps.Endpoints = []discovery.Endpoint{{
					Addresses: []string{"10.1.1.1"},
				}, {
					Addresses: []string{"10.1.1.2"},
					NodeName:  ptr.To(testHostname),
				}}
				eps.Ports = []discovery.EndpointPort{{
					Name:     ptr.To("p11"),
					Port:     ptr.To[int32](11),
					Protocol: ptr.To(v1.ProtocolUDP),
				}, {
					Name:     ptr.To("p12"),
					Port:     ptr.To[int32](12),
					Protocol: ptr.To(v1.ProtocolUDP),
				}}
			}),
	}
	multipleSubsets := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, subset1),
		makeTestEndpointSlice("ns1", "ep1", 2, subset2),
	}
	subsetLocal := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.2"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p12"),
			Port:     ptr.To[int32](12),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	multipleSubsetsWithLocal := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, subset1),
		makeTestEndpointSlice("ns1", "ep1", 2, subsetLocal),
	}
	subsetMultiplePortsLocal := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.1"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p11"),
			Port:     ptr.To[int32](11),
			Protocol: ptr.To(v1.ProtocolUDP),
		}, {
			Name:     ptr.To("p12"),
			Port:     ptr.To[int32](12),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	subset3 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.3"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p13"),
			Port:     ptr.To[int32](13),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	multipleSubsetsMultiplePortsLocal := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, subsetMultiplePortsLocal),
		makeTestEndpointSlice("ns1", "ep1", 2, subset3),
	}
	subsetMultipleIPsPorts1 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.1"},
		}, {
			Addresses: []string{"10.1.1.2"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p11"),
			Port:     ptr.To[int32](11),
			Protocol: ptr.To(v1.ProtocolUDP),
		}, {
			Name:     ptr.To("p12"),
			Port:     ptr.To[int32](12),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	subsetMultipleIPsPorts2 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.3"},
		}, {
			Addresses: []string{"10.1.1.4"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p13"),
			Port:     ptr.To[int32](13),
			Protocol: ptr.To(v1.ProtocolUDP),
		}, {
			Name:     ptr.To("p14"),
			Port:     ptr.To[int32](14),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	subsetMultipleIPsPorts3 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.2.2.1"},
		}, {
			Addresses: []string{"10.2.2.2"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p21"),
			Port:     ptr.To[int32](21),
			Protocol: ptr.To(v1.ProtocolUDP),
		}, {
			Name:     ptr.To("p22"),
			Port:     ptr.To[int32](22),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	multipleSubsetsIPsPorts := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, subsetMultipleIPsPorts1),
		makeTestEndpointSlice("ns1", "ep1", 2, subsetMultipleIPsPorts2),
		makeTestEndpointSlice("ns2", "ep2", 1, subsetMultipleIPsPorts3),
	}
	complexSubset1 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.2.2.2"},
			NodeName:  ptr.To(testHostname),
		}, {
			Addresses: []string{"10.2.2.22"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p22"),
			Port:     ptr.To[int32](22),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexSubset2 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.2.2.3"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p23"),
			Port:     ptr.To[int32](23),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexSubset3 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.4.4.4"},
			NodeName:  ptr.To(testHostname),
		}, {
			Addresses: []string{"10.4.4.5"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p44"),
			Port:     ptr.To[int32](44),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexSubset4 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.4.4.6"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p45"),
			Port:     ptr.To[int32](45),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexSubset5 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.1"},
		}, {
			Addresses: []string{"10.1.1.11"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p11"),
			Port:     ptr.To[int32](11),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexSubset6 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.1.1.2"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p12"),
			Port:     ptr.To[int32](12),
			Protocol: ptr.To(v1.ProtocolUDP),
		}, {
			Name:     ptr.To("p122"),
			Port:     ptr.To[int32](122),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexSubset7 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.3.3.3"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p33"),
			Port:     ptr.To[int32](33),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexSubset8 := func(eps *discovery.EndpointSlice) {
		eps.AddressType = discovery.AddressTypeIPv4
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"10.4.4.4"},
			NodeName:  ptr.To(testHostname),
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     ptr.To("p44"),
			Port:     ptr.To[int32](44),
			Protocol: ptr.To(v1.ProtocolUDP),
		}}
	}
	complexBefore := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, subset1),
		nil,
		makeTestEndpointSlice("ns2", "ep2", 1, complexSubset1),
		makeTestEndpointSlice("ns2", "ep2", 2, complexSubset2),
		nil,
		makeTestEndpointSlice("ns4", "ep4", 1, complexSubset3),
		makeTestEndpointSlice("ns4", "ep4", 2, complexSubset4),
	}
	complexAfter := []*discovery.EndpointSlice{
		makeTestEndpointSlice("ns1", "ep1", 1, complexSubset5),
		makeTestEndpointSlice("ns1", "ep1", 2, complexSubset6),
		nil,
		nil,
		makeTestEndpointSlice("ns3", "ep3", 1, complexSubset7),
		makeTestEndpointSlice("ns4", "ep4", 1, complexSubset8),
		nil,
	}

	testCases := []struct {
		// previousEndpoints and currentEndpoints are used to call appropriate
		// handlers OnEndpoints* (based on whether corresponding values are nil
		// or non-nil) and must be of equal length.
		name                             string
		previousEndpoints                []*discovery.EndpointSlice
		currentEndpoints                 []*discovery.EndpointSlice
		oldEndpoints                     map[proxy.ServicePortName][]endpointExpectation
		expectedResult                   map[proxy.ServicePortName][]endpointExpectation
		expectedConntrackCleanupRequired bool
		expectedLocalEndpoints           map[types.NamespacedName]int
	}{{
		// Case[0]: nothing
		name:                             "nothing",
		oldEndpoints:                     map[proxy.ServicePortName][]endpointExpectation{},
		expectedResult:                   map[proxy.ServicePortName][]endpointExpectation{},
		expectedConntrackCleanupRequired: false,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	}, {
		// Case[1]: no change, named port, local
		name:              "no change, named port, local",
		previousEndpoints: namedPortLocal,
		currentEndpoints:  namedPortLocal,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: true},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: true},
			},
		},
		expectedConntrackCleanupRequired: false,
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
	}, {
		// Case[2]: no change, multiple subsets
		name:              "no change, multiple subsets",
		previousEndpoints: multipleSubsets,
		currentEndpoints:  multipleSubsets,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.2:12", isLocal: false},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.2:12", isLocal: false},
			},
		},
		expectedConntrackCleanupRequired: false,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	}, {
		// Case[3]: no change, multiple subsets, multiple ports, local
		name:              "no change, multiple subsets, multiple ports, local",
		previousEndpoints: multipleSubsetsMultiplePortsLocal,
		currentEndpoints:  multipleSubsetsMultiplePortsLocal,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:12", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{endpoint: "10.1.1.3:13", isLocal: false},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:12", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{endpoint: "10.1.1.3:13", isLocal: false},
			},
		},
		expectedConntrackCleanupRequired: false,
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
	}, {
		// Case[4]: no change, multiple endpoints, subsets, IPs, and ports
		name:              "no change, multiple endpoints, subsets, IPs, and ports",
		previousEndpoints: multipleSubsetsIPsPorts,
		currentEndpoints:  multipleSubsetsIPsPorts,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
				{endpoint: "10.1.1.2:11", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:12", isLocal: false},
				{endpoint: "10.1.1.2:12", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{endpoint: "10.1.1.3:13", isLocal: false},
				{endpoint: "10.1.1.4:13", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p14", v1.ProtocolUDP): {
				{endpoint: "10.1.1.3:14", isLocal: false},
				{endpoint: "10.1.1.4:14", isLocal: true},
			},
			makeServicePortName("ns2", "ep2", "p21", v1.ProtocolUDP): {
				{endpoint: "10.2.2.1:21", isLocal: false},
				{endpoint: "10.2.2.2:21", isLocal: true},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP): {
				{endpoint: "10.2.2.1:22", isLocal: false},
				{endpoint: "10.2.2.2:22", isLocal: true},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
				{endpoint: "10.1.1.2:11", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:12", isLocal: false},
				{endpoint: "10.1.1.2:12", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{endpoint: "10.1.1.3:13", isLocal: false},
				{endpoint: "10.1.1.4:13", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p14", v1.ProtocolUDP): {
				{endpoint: "10.1.1.3:14", isLocal: false},
				{endpoint: "10.1.1.4:14", isLocal: true},
			},
			makeServicePortName("ns2", "ep2", "p21", v1.ProtocolUDP): {
				{endpoint: "10.2.2.1:21", isLocal: false},
				{endpoint: "10.2.2.2:21", isLocal: true},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP): {
				{endpoint: "10.2.2.1:22", isLocal: false},
				{endpoint: "10.2.2.2:22", isLocal: true},
			},
		},
		expectedConntrackCleanupRequired: false,
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 2,
			makeNSN("ns2", "ep2"): 1,
		},
	}, {
		// Case[5]: add an Endpoints
		name:              "add an Endpoints",
		previousEndpoints: []*discovery.EndpointSlice{nil},
		currentEndpoints:  namedPortLocal,
		oldEndpoints:      map[proxy.ServicePortName][]endpointExpectation{},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: true},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
	}, {
		// Case[6]: remove an Endpoints
		name:              "remove an Endpoints",
		previousEndpoints: namedPortLocal,
		currentEndpoints:  []*discovery.EndpointSlice{nil},
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: true},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	}, {
		// Case[7]: add an IP and port
		name:              "add an IP and port",
		previousEndpoints: namedPort,
		currentEndpoints:  namedPortsLocalNoLocal,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
				{endpoint: "10.1.1.2:11", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:12", isLocal: false},
				{endpoint: "10.1.1.2:12", isLocal: true},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
	}, {
		// Case[8]: remove an IP and port
		name:              "remove an IP and port",
		previousEndpoints: namedPortsLocalNoLocal,
		currentEndpoints:  namedPort,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
				{endpoint: "10.1.1.2:11", isLocal: true},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:12", isLocal: false},
				{endpoint: "10.1.1.2:12", isLocal: true},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	}, {
		// Case[9]: add a subset
		name:              "add a subset",
		previousEndpoints: []*discovery.EndpointSlice{namedPort[0], nil},
		currentEndpoints:  multipleSubsetsWithLocal,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.2:12", isLocal: true},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
	}, {
		// Case[10]: remove a subset
		name:              "remove a subset",
		previousEndpoints: multipleSubsets,
		currentEndpoints:  []*discovery.EndpointSlice{namedPort[0], nil},
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.2:12", isLocal: false},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	}, {
		// Case[11]: rename a port
		name:              "rename a port",
		previousEndpoints: namedPort,
		currentEndpoints:  namedPortRenamed,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11-2", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	}, {
		// Case[12]: renumber a port
		name:              "renumber a port",
		previousEndpoints: namedPort,
		currentEndpoints:  namedPortRenumbered,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:22", isLocal: false},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	}, {
		// Case[13]: complex add and remove
		name:              "complex add and remove",
		previousEndpoints: complexBefore,
		currentEndpoints:  complexAfter,
		oldEndpoints: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP): {
				{endpoint: "10.2.2.22:22", isLocal: true},
				{endpoint: "10.2.2.2:22", isLocal: true},
			},
			makeServicePortName("ns2", "ep2", "p23", v1.ProtocolUDP): {
				{endpoint: "10.2.2.3:23", isLocal: true},
			},
			makeServicePortName("ns4", "ep4", "p44", v1.ProtocolUDP): {
				{endpoint: "10.4.4.4:44", isLocal: true},
				{endpoint: "10.4.4.5:44", isLocal: true},
			},
			makeServicePortName("ns4", "ep4", "p45", v1.ProtocolUDP): {
				{endpoint: "10.4.4.6:45", isLocal: true},
			},
		},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.11:11", isLocal: false},
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{endpoint: "10.1.1.2:12", isLocal: false},
			},
			makeServicePortName("ns1", "ep1", "p122", v1.ProtocolUDP): {
				{endpoint: "10.1.1.2:122", isLocal: false},
			},
			makeServicePortName("ns3", "ep3", "p33", v1.ProtocolUDP): {
				{endpoint: "10.3.3.3:33", isLocal: false},
			},
			makeServicePortName("ns4", "ep4", "p44", v1.ProtocolUDP): {
				{endpoint: "10.4.4.4:44", isLocal: true},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns4", "ep4"): 1,
		},
	}, {
		// Case[14]: change from 0 endpoint address to 1 unnamed port
		name:              "change from 0 endpoint address to 1 unnamed port",
		previousEndpoints: emptyEndpointSlices,
		currentEndpoints:  namedPort,
		oldEndpoints:      map[proxy.ServicePortName][]endpointExpectation{},
		expectedResult: map[proxy.ServicePortName][]endpointExpectation{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{endpoint: "10.1.1.1:11", isLocal: false},
			},
		},
		expectedConntrackCleanupRequired: true,
		expectedLocalEndpoints:           map[types.NamespacedName]int{},
	},
	}

	for tci, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, fp := NewFakeProxier(v1.IPv4Protocol)
			fp.hostname = testHostname

			// First check that after adding all previous versions of endpoints,
			// the fp.oldEndpoints is as we expect.
			for i := range tc.previousEndpoints {
				if tc.previousEndpoints[i] != nil {
					fp.OnEndpointSliceAdd(tc.previousEndpoints[i])
				}
			}
			fp.endpointsMap.Update(fp.endpointsChanges)
			checkEndpointExpectations(t, tci, fp.endpointsMap, tc.oldEndpoints)

			// Now let's call appropriate handlers to get to state we want to be.
			if len(tc.previousEndpoints) != len(tc.currentEndpoints) {
				t.Fatalf("[%d] different lengths of previous and current endpoints", tci)
			}

			for i := range tc.previousEndpoints {
				prev, curr := tc.previousEndpoints[i], tc.currentEndpoints[i]
				switch {
				case prev == nil:
					fp.OnEndpointSliceAdd(curr)
				case curr == nil:
					fp.OnEndpointSliceDelete(prev)
				default:
					fp.OnEndpointSliceUpdate(prev, curr)
				}
			}
			result := fp.endpointsMap.Update(fp.endpointsChanges)
			newMap := fp.endpointsMap
			checkEndpointExpectations(t, tci, newMap, tc.expectedResult)
			if result.ConntrackCleanupRequired != tc.expectedConntrackCleanupRequired {
				t.Errorf("[%d] expected conntrackCleanupRequired to be %t, got %t", tci, tc.expectedConntrackCleanupRequired, result.ConntrackCleanupRequired)
			}
			localReadyEndpoints := fp.endpointsMap.LocalReadyEndpoints()
			if !reflect.DeepEqual(localReadyEndpoints, tc.expectedLocalEndpoints) {
				t.Errorf("[%d] expected local endpoints %v, got %v", tci, tc.expectedLocalEndpoints, localReadyEndpoints)
			}
		})
	}
}

// TestHealthCheckNodePortWhenTerminating tests that health check node ports are not enabled when all local endpoints are terminating
func TestHealthCheckNodePortWhenTerminating(t *testing.T) {
	_, fp := NewFakeProxier(v1.IPv4Protocol)
	fp.OnServiceSynced()
	fp.OnEndpointSlicesSynced()

	serviceName := "svc1"
	namespaceName := "ns1"

	fp.OnServiceAdd(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespaceName},
		Spec: v1.ServiceSpec{
			ClusterIP: "172.30.1.1",
			Selector:  map[string]string{"foo": "bar"},
			Ports:     []v1.ServicePort{{Name: "", TargetPort: intstr.FromInt32(80), Protocol: v1.ProtocolTCP}},
		},
	})

	endpointSlice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-1", serviceName),
			Namespace: namespaceName,
			Labels:    map[string]string{discovery.LabelServiceName: serviceName},
		},
		Ports: []discovery.EndpointPort{{
			Name:     ptr.To(""),
			Port:     ptr.To[int32](80),
			Protocol: ptr.To(v1.ProtocolTCP),
		}},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{{
			Addresses:  []string{"10.0.1.1"},
			Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
			NodeName:   ptr.To(testHostname),
		}, {
			Addresses:  []string{"10.0.1.2"},
			Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
			NodeName:   ptr.To(testHostname),
		}, {
			Addresses:  []string{"10.0.1.3"},
			Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
			NodeName:   ptr.To(testHostname),
		}, { // not ready endpoints should be ignored
			Addresses:  []string{"10.0.1.4"},
			Conditions: discovery.EndpointConditions{Ready: ptr.To(false)},
			NodeName:   ptr.To(testHostname),
		}},
	}

	fp.OnEndpointSliceAdd(endpointSlice)
	_ = fp.endpointsMap.Update(fp.endpointsChanges)
	localReadyEndpoints := fp.endpointsMap.LocalReadyEndpoints()
	if len(localReadyEndpoints) != 1 {
		t.Errorf("unexpected number of local ready endpoints, expected 1 but got: %d", len(localReadyEndpoints))
	}

	// set all endpoints to terminating
	endpointSliceTerminating := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-1", serviceName),
			Namespace: namespaceName,
			Labels:    map[string]string{discovery.LabelServiceName: serviceName},
		},
		Ports: []discovery.EndpointPort{{
			Name:     ptr.To(""),
			Port:     ptr.To[int32](80),
			Protocol: ptr.To(v1.ProtocolTCP),
		}},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{{
			Addresses: []string{"10.0.1.1"},
			Conditions: discovery.EndpointConditions{
				Ready:       ptr.To(false),
				Serving:     ptr.To(true),
				Terminating: ptr.To(false),
			},
			NodeName: ptr.To(testHostname),
		}, {
			Addresses: []string{"10.0.1.2"},
			Conditions: discovery.EndpointConditions{
				Ready:       ptr.To(false),
				Serving:     ptr.To(true),
				Terminating: ptr.To(true),
			},
			NodeName: ptr.To(testHostname),
		}, {
			Addresses: []string{"10.0.1.3"},
			Conditions: discovery.EndpointConditions{
				Ready:       ptr.To(false),
				Serving:     ptr.To(true),
				Terminating: ptr.To(true),
			},
			NodeName: ptr.To(testHostname),
		}, { // not ready endpoints should be ignored
			Addresses: []string{"10.0.1.4"},
			Conditions: discovery.EndpointConditions{
				Ready:       ptr.To(false),
				Serving:     ptr.To(false),
				Terminating: ptr.To(true),
			},
			NodeName: ptr.To(testHostname),
		}},
	}

	fp.OnEndpointSliceUpdate(endpointSlice, endpointSliceTerminating)
	_ = fp.endpointsMap.Update(fp.endpointsChanges)
	localReadyEndpoints = fp.endpointsMap.LocalReadyEndpoints()
	if len(localReadyEndpoints) != 0 {
		t.Errorf("unexpected number of local ready endpoints, expected 0 but got: %d", len(localReadyEndpoints))
	}
}

// TODO(thockin): add *more* tests for syncProxyRules() or break it down further and test the pieces.

// This test ensures that the iptables proxier supports translating Endpoints to
// iptables output when internalTrafficPolicy is specified
func TestInternalTrafficPolicy(t *testing.T) {
	type endpoint struct {
		ip       string
		hostname string
	}

	testCases := []struct {
		name                  string
		line                  string
		internalTrafficPolicy *v1.ServiceInternalTrafficPolicy
		endpoints             []endpoint
		flowTests             []packetFlowTest
	}{
		{
			name:                  "internalTrafficPolicy is cluster",
			line:                  getLine(),
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyCluster),
			endpoints: []endpoint{
				{"10.0.1.1", testHostname},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to ClusterIP hits all endpoints",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.1:80, 10.0.1.2:80, 10.0.1.3:80",
					masq:     false,
				},
			},
		},
		{
			name:                  "internalTrafficPolicy is local and there is one local endpoint",
			line:                  getLine(),
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			endpoints: []endpoint{
				{"10.0.1.1", testHostname},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to ClusterIP hits only local endpoint",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.1:80",
					masq:     false,
				},
			},
		},
		{
			name:                  "internalTrafficPolicy is local and there are multiple local endpoints",
			line:                  getLine(),
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			endpoints: []endpoint{
				{"10.0.1.1", testHostname},
				{"10.0.1.2", testHostname},
				{"10.0.1.3", "host2"},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to ClusterIP hits all local endpoints",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.1:80, 10.0.1.2:80",
					masq:     false,
				},
			},
		},
		{
			name:                  "internalTrafficPolicy is local and there are no local endpoints",
			line:                  getLine(),
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			endpoints: []endpoint{
				{"10.0.1.1", "host0"},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
			flowTests: []packetFlowTest{
				{
					name:     "no endpoints",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "DROP",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nft, fp := NewFakeProxier(v1.IPv4Protocol)
			fp.OnServiceSynced()
			fp.OnEndpointSlicesSynced()

			serviceName := "svc1"
			namespaceName := "ns1"

			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespaceName},
				Spec: v1.ServiceSpec{
					ClusterIP: "172.30.1.1",
					Selector:  map[string]string{"foo": "bar"},
					Ports:     []v1.ServicePort{{Name: "", Port: 80, Protocol: v1.ProtocolTCP}},
				},
			}
			if tc.internalTrafficPolicy != nil {
				svc.Spec.InternalTrafficPolicy = tc.internalTrafficPolicy
			}

			fp.OnServiceAdd(svc)

			endpointSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", serviceName),
					Namespace: namespaceName,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
			}
			for _, ep := range tc.endpoints {
				endpointSlice.Endpoints = append(endpointSlice.Endpoints, discovery.Endpoint{
					Addresses:  []string{ep.ip},
					Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
					NodeName:   ptr.To(ep.hostname),
				})
			}

			fp.OnEndpointSliceAdd(endpointSlice)
			fp.syncProxyRules()
			runPacketFlowTests(t, tc.line, nft, testNodeIPs, tc.flowTests)

			fp.OnEndpointSliceDelete(endpointSlice)
			fp.syncProxyRules()
			runPacketFlowTests(t, tc.line, nft, testNodeIPs, []packetFlowTest{
				{
					name:     "endpoints deleted",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "REJECT",
				},
			})
		})
	}
}

// TestTerminatingEndpointsTrafficPolicyLocal tests that when there are local ready and
// ready + terminating endpoints, only the ready endpoints are used.
func TestTerminatingEndpointsTrafficPolicyLocal(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "ns1"},
		Spec: v1.ServiceSpec{
			ClusterIP:             "172.30.1.1",
			Type:                  v1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
			Ports: []v1.ServicePort{
				{
					Name:       "",
					TargetPort: intstr.FromInt32(80),
					Port:       80,
					Protocol:   v1.ProtocolTCP,
				},
			},
			HealthCheckNodePort: 30000,
		},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "1.2.3.4"},
				},
			},
		},
	}

	testcases := []struct {
		name          string
		line          string
		endpointslice *discovery.EndpointSlice
		flowTests     []packetFlowTest
	}{
		{
			name: "ready endpoints exist",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.0.1.1"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(true),
							Serving:     ptr.To(true),
							Terminating: ptr.To(false),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						Addresses: []string{"10.0.1.2"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(true),
							Serving:     ptr.To(true),
							Terminating: ptr.To(false),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should be ignored for external since there are ready non-terminating endpoints
						Addresses: []string{"10.0.1.3"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should be ignored for external since there are ready non-terminating endpoints
						Addresses: []string{"10.0.1.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should be ignored for external since it's not local
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(true),
							Serving:     ptr.To(true),
							Terminating: ptr.To(false),
						},
						NodeName: ptr.To("host-1"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.1:80, 10.0.1.2:80, 10.0.1.5:80",
					masq:     false,
				},
				{
					name:     "external to LB",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "10.0.1.1:80, 10.0.1.2:80",
					masq:     false,
				},
			},
		},
		{
			name: "only terminating endpoints exist",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						// this endpoint should be used since there are only ready terminating endpoints
						Addresses: []string{"10.0.1.2"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should be used since there are only ready terminating endpoints
						Addresses: []string{"10.0.1.3"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should not be used since it is both terminating and not ready.
						Addresses: []string{"10.0.1.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should be ignored for external since it's not local
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(true),
							Serving:     ptr.To(true),
							Terminating: ptr.To(false),
						},
						NodeName: ptr.To("host-1"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.5:80",
					masq:     false,
				},
				{
					name:     "external to LB",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "10.0.1.2:80, 10.0.1.3:80",
					masq:     false,
				},
			},
		},
		{
			name: "terminating endpoints on remote node",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						// this endpoint won't be used because it's not local,
						// but it will prevent a REJECT rule from being created
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("host-1"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.5:80",
				},
				{
					name:     "external to LB, no locally-usable endpoints",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "DROP",
				},
			},
		},
		{
			name: "no usable endpoints on any node",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						// Local but not ready or serving
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// Remote and not ready or serving
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("host-1"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP, no usable endpoints",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "REJECT",
				},
				{
					name:     "external to LB, no usable endpoints",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "REJECT",
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			nft, fp := NewFakeProxier(v1.IPv4Protocol)
			fp.OnServiceSynced()
			fp.OnEndpointSlicesSynced()

			fp.OnServiceAdd(service)

			fp.OnEndpointSliceAdd(testcase.endpointslice)
			fp.syncProxyRules()
			runPacketFlowTests(t, testcase.line, nft, testNodeIPs, testcase.flowTests)

			fp.OnEndpointSliceDelete(testcase.endpointslice)
			fp.syncProxyRules()
			runPacketFlowTests(t, testcase.line, nft, testNodeIPs, []packetFlowTest{
				{
					name:     "pod to clusterIP after endpoints deleted",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "REJECT",
				},
				{
					name:     "external to LB after endpoints deleted",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "REJECT",
				},
			})
		})
	}
}

// TestTerminatingEndpointsTrafficPolicyCluster tests that when there are cluster-wide
// ready and ready + terminating endpoints, only the ready endpoints are used.
func TestTerminatingEndpointsTrafficPolicyCluster(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "ns1"},
		Spec: v1.ServiceSpec{
			ClusterIP:             "172.30.1.1",
			Type:                  v1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyCluster,
			Ports: []v1.ServicePort{
				{
					Name:       "",
					TargetPort: intstr.FromInt32(80),
					Port:       80,
					Protocol:   v1.ProtocolTCP,
				},
			},
			HealthCheckNodePort: 30000,
		},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "1.2.3.4"},
				},
			},
		},
	}

	testcases := []struct {
		name          string
		line          string
		endpointslice *discovery.EndpointSlice
		flowTests     []packetFlowTest
	}{
		{
			name: "ready endpoints exist",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.0.1.1"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(true),
							Serving:     ptr.To(true),
							Terminating: ptr.To(false),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						Addresses: []string{"10.0.1.2"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(true),
							Serving:     ptr.To(true),
							Terminating: ptr.To(false),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should be ignored since there are ready non-terminating endpoints
						Addresses: []string{"10.0.1.3"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("another-host"),
					},
					{
						// this endpoint should be ignored since it is not "serving"
						Addresses: []string{"10.0.1.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("another-host"),
					},
					{
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(true),
							Serving:     ptr.To(true),
							Terminating: ptr.To(false),
						},
						NodeName: ptr.To("another-host"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.1:80, 10.0.1.2:80, 10.0.1.5:80",
					masq:     false,
				},
				{
					name:     "external to LB",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "10.0.1.1:80, 10.0.1.2:80, 10.0.1.5:80",
					masq:     true,
				},
			},
		},
		{
			name: "only terminating endpoints exist",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						// this endpoint should be used since there are only ready terminating endpoints
						Addresses: []string{"10.0.1.2"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should be used since there are only ready terminating endpoints
						Addresses: []string{"10.0.1.3"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// this endpoint should not be used since it is both terminating and not ready.
						Addresses: []string{"10.0.1.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("another-host"),
					},
					{
						// this endpoint should be used since there are only ready terminating endpoints
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("another-host"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.2:80, 10.0.1.3:80, 10.0.1.5:80",
					masq:     false,
				},
				{
					name:     "external to LB",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "10.0.1.2:80, 10.0.1.3:80, 10.0.1.5:80",
					masq:     true,
				},
			},
		},
		{
			name: "terminating endpoints on remote node",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(true),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("host-1"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "10.0.1.5:80",
					masq:     false,
				},
				{
					name:     "external to LB",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "10.0.1.5:80",
					masq:     true,
				},
			},
		},
		{
			name: "no usable endpoints on any node",
			line: getLine(),
			endpointslice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", "svc1"),
					Namespace: "ns1",
					Labels:    map[string]string{discovery.LabelServiceName: "svc1"},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						// Local, not ready or serving
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To(testHostname),
					},
					{
						// Remote, not ready or serving
						Addresses: []string{"10.0.1.5"},
						Conditions: discovery.EndpointConditions{
							Ready:       ptr.To(false),
							Serving:     ptr.To(false),
							Terminating: ptr.To(true),
						},
						NodeName: ptr.To("host-1"),
					},
				},
			},
			flowTests: []packetFlowTest{
				{
					name:     "pod to clusterIP",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "REJECT",
				},
				{
					name:     "external to LB",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "REJECT",
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {

			nft, fp := NewFakeProxier(v1.IPv4Protocol)
			fp.OnServiceSynced()
			fp.OnEndpointSlicesSynced()

			fp.OnServiceAdd(service)

			fp.OnEndpointSliceAdd(testcase.endpointslice)
			fp.syncProxyRules()
			runPacketFlowTests(t, testcase.line, nft, testNodeIPs, testcase.flowTests)

			fp.OnEndpointSliceDelete(testcase.endpointslice)
			fp.syncProxyRules()
			runPacketFlowTests(t, testcase.line, nft, testNodeIPs, []packetFlowTest{
				{
					name:     "pod to clusterIP after endpoints deleted",
					sourceIP: "10.0.0.2",
					destIP:   "172.30.1.1",
					destPort: 80,
					output:   "REJECT",
				},
				{
					name:     "external to LB after endpoints deleted",
					sourceIP: testExternalClient,
					destIP:   "1.2.3.4",
					destPort: 80,
					output:   "REJECT",
				},
			})
		})
	}
}

func TestInternalExternalMasquerade(t *testing.T) {
	// (Put the test setup code in an internal function so we can have it here at the
	// top, before the test cases that will be run against it.)
	setupTest := func(fp *Proxier) {
		makeServiceMap(fp,
			makeTestService("ns1", "svc1", func(svc *v1.Service) {
				svc.Spec.Type = "LoadBalancer"
				svc.Spec.ClusterIP = "172.30.0.41"
				svc.Spec.Ports = []v1.ServicePort{{
					Name:     "p80",
					Port:     80,
					Protocol: v1.ProtocolTCP,
					NodePort: int32(3001),
				}}
				svc.Spec.HealthCheckNodePort = 30001
				svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
					IP: "1.2.3.4",
				}}
			}),
			makeTestService("ns2", "svc2", func(svc *v1.Service) {
				svc.Spec.Type = "LoadBalancer"
				svc.Spec.ClusterIP = "172.30.0.42"
				svc.Spec.Ports = []v1.ServicePort{{
					Name:     "p80",
					Port:     80,
					Protocol: v1.ProtocolTCP,
					NodePort: int32(3002),
				}}
				svc.Spec.HealthCheckNodePort = 30002
				svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
				svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
					IP: "5.6.7.8",
				}}
			}),
			makeTestService("ns3", "svc3", func(svc *v1.Service) {
				svc.Spec.Type = "LoadBalancer"
				svc.Spec.ClusterIP = "172.30.0.43"
				svc.Spec.Ports = []v1.ServicePort{{
					Name:     "p80",
					Port:     80,
					Protocol: v1.ProtocolTCP,
					NodePort: int32(3003),
				}}
				svc.Spec.HealthCheckNodePort = 30003
				svc.Spec.InternalTrafficPolicy = ptr.To(v1.ServiceInternalTrafficPolicyLocal)
				svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
					IP: "9.10.11.12",
				}}
			}),
		)

		populateEndpointSlices(fp,
			makeTestEndpointSlice("ns1", "svc1", 1, func(eps *discovery.EndpointSlice) {
				eps.AddressType = discovery.AddressTypeIPv4
				eps.Endpoints = []discovery.Endpoint{
					{
						Addresses: []string{"10.180.0.1"},
						NodeName:  ptr.To(testHostname),
					},
					{
						Addresses: []string{"10.180.1.1"},
						NodeName:  ptr.To("remote"),
					},
				}
				eps.Ports = []discovery.EndpointPort{{
					Name:     ptr.To("p80"),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}}
			}),
			makeTestEndpointSlice("ns2", "svc2", 1, func(eps *discovery.EndpointSlice) {
				eps.AddressType = discovery.AddressTypeIPv4
				eps.Endpoints = []discovery.Endpoint{
					{
						Addresses: []string{"10.180.0.2"},
						NodeName:  ptr.To(testHostname),
					},
					{
						Addresses: []string{"10.180.1.2"},
						NodeName:  ptr.To("remote"),
					},
				}
				eps.Ports = []discovery.EndpointPort{{
					Name:     ptr.To("p80"),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}}
			}),
			makeTestEndpointSlice("ns3", "svc3", 1, func(eps *discovery.EndpointSlice) {
				eps.AddressType = discovery.AddressTypeIPv4
				eps.Endpoints = []discovery.Endpoint{
					{
						Addresses: []string{"10.180.0.3"},
						NodeName:  ptr.To(testHostname),
					},
					{
						Addresses: []string{"10.180.1.3"},
						NodeName:  ptr.To("remote"),
					},
				}
				eps.Ports = []discovery.EndpointPort{{
					Name:     ptr.To("p80"),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}}
			}),
		)

		fp.syncProxyRules()
	}

	// We use the same flowTests for all of the testCases. The "output" and "masq"
	// values here represent the normal case (working localDetector, no masqueradeAll)
	flowTests := []packetFlowTest{
		{
			name:     "pod to ClusterIP",
			sourceIP: "10.0.0.2",
			destIP:   "172.30.0.41",
			destPort: 80,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     false,
		},
		{
			name:     "pod to NodePort",
			sourceIP: "10.0.0.2",
			destIP:   testNodeIP,
			destPort: 3001,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "pod to LB",
			sourceIP: "10.0.0.2",
			destIP:   "1.2.3.4",
			destPort: 80,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "node to ClusterIP",
			sourceIP: testNodeIP,
			destIP:   "172.30.0.41",
			destPort: 80,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "node to NodePort",
			sourceIP: testNodeIP,
			destIP:   testNodeIP,
			destPort: 3001,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "node to LB",
			sourceIP: testNodeIP,
			destIP:   "1.2.3.4",
			destPort: 80,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "external to ClusterIP",
			sourceIP: testExternalClient,
			destIP:   "172.30.0.41",
			destPort: 80,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "external to NodePort",
			sourceIP: testExternalClient,
			destIP:   testNodeIP,
			destPort: 3001,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "external to LB",
			sourceIP: testExternalClient,
			destIP:   "1.2.3.4",
			destPort: 80,
			output:   "10.180.0.1:80, 10.180.1.1:80",
			masq:     true,
		},
		{
			name:     "pod to ClusterIP with eTP:Local",
			sourceIP: "10.0.0.2",
			destIP:   "172.30.0.42",
			destPort: 80,

			// externalTrafficPolicy does not apply to ClusterIP traffic, so same
			// as "Pod to ClusterIP"
			output: "10.180.0.2:80, 10.180.1.2:80",
			masq:   false,
		},
		{
			name:     "pod to NodePort with eTP:Local",
			sourceIP: "10.0.0.2",
			destIP:   testNodeIP,
			destPort: 3002,

			// See the comment below in the "pod to LB with eTP:Local" case.
			// It doesn't actually make sense to short-circuit here, since if
			// you connect directly to a NodePort from outside the cluster,
			// you only get the local endpoints. But it's simpler for us and
			// slightly more convenient for users to have this case get
			// short-circuited too.
			output: "10.180.0.2:80, 10.180.1.2:80",
			masq:   false,
		},
		{
			name:     "pod to LB with eTP:Local",
			sourceIP: "10.0.0.2",
			destIP:   "5.6.7.8",
			destPort: 80,

			// The short-circuit rule is supposed to make this behave the same
			// way it would if the packet actually went out to the LB and then
			// came back into the cluster. So it gets routed to all endpoints,
			// not just local ones. In reality, if the packet actually left
			// the cluster, it would have to get masqueraded, but since we can
			// avoid doing that in the short-circuit case, and not masquerading
			// is more useful, we avoid masquerading.
			output: "10.180.0.2:80, 10.180.1.2:80",
			masq:   false,
		},
		{
			name:     "node to ClusterIP with eTP:Local",
			sourceIP: testNodeIP,
			destIP:   "172.30.0.42",
			destPort: 80,

			// externalTrafficPolicy does not apply to ClusterIP traffic, so same
			// as "node to ClusterIP"
			output: "10.180.0.2:80, 10.180.1.2:80",
			masq:   true,
		},
		{
			name:     "node to NodePort with eTP:Local",
			sourceIP: testNodeIP,
			destIP:   testNodeIP,
			destPort: 3001,

			// The traffic gets short-circuited, ignoring externalTrafficPolicy, so
			// same as "node to NodePort" above.
			output: "10.180.0.1:80, 10.180.1.1:80",
			masq:   true,
		},
		{
			name:     "node to LB with eTP:Local",
			sourceIP: testNodeIP,
			destIP:   "5.6.7.8",
			destPort: 80,

			// The traffic gets short-circuited, ignoring externalTrafficPolicy, so
			// same as "node to LB" above.
			output: "10.180.0.2:80, 10.180.1.2:80",
			masq:   true,
		},
		{
			name:     "external to ClusterIP with eTP:Local",
			sourceIP: testExternalClient,
			destIP:   "172.30.0.42",
			destPort: 80,

			// externalTrafficPolicy does not apply to ClusterIP traffic, so same
			// as "external to ClusterIP" above.
			output: "10.180.0.2:80, 10.180.1.2:80",
			masq:   true,
		},
		{
			name:     "external to NodePort with eTP:Local",
			sourceIP: testExternalClient,
			destIP:   testNodeIP,
			destPort: 3002,

			// externalTrafficPolicy applies; only the local endpoint is
			// selected, and we don't masquerade.
			output: "10.180.0.2:80",
			masq:   false,
		},
		{
			name:     "external to LB with eTP:Local",
			sourceIP: testExternalClient,
			destIP:   "5.6.7.8",
			destPort: 80,

			// externalTrafficPolicy applies; only the local endpoint is
			// selected, and we don't masquerade.
			output: "10.180.0.2:80",
			masq:   false,
		},
		{
			name:     "pod to ClusterIP with iTP:Local",
			sourceIP: "10.0.0.2",
			destIP:   "172.30.0.43",
			destPort: 80,

			// internalTrafficPolicy applies; only the local endpoint is
			// selected.
			output: "10.180.0.3:80",
			masq:   false,
		},
		{
			name:     "pod to NodePort with iTP:Local",
			sourceIP: "10.0.0.2",
			destIP:   testNodeIP,
			destPort: 3003,

			// internalTrafficPolicy does not apply to NodePort traffic, so same as
			// "pod to NodePort" above.
			output: "10.180.0.3:80, 10.180.1.3:80",
			masq:   true,
		},
		{
			name:     "pod to LB with iTP:Local",
			sourceIP: "10.0.0.2",
			destIP:   "9.10.11.12",
			destPort: 80,

			// internalTrafficPolicy does not apply to LoadBalancer traffic, so
			// same as "pod to LB" above.
			output: "10.180.0.3:80, 10.180.1.3:80",
			masq:   true,
		},
		{
			name:     "node to ClusterIP with iTP:Local",
			sourceIP: testNodeIP,
			destIP:   "172.30.0.43",
			destPort: 80,

			// internalTrafficPolicy applies; only the local endpoint is selected.
			// Traffic is masqueraded as in the "node to ClusterIP" case because
			// internalTrafficPolicy does not affect masquerading.
			output: "10.180.0.3:80",
			masq:   true,
		},
		{
			name:     "node to NodePort with iTP:Local",
			sourceIP: testNodeIP,
			destIP:   testNodeIP,
			destPort: 3003,

			// internalTrafficPolicy does not apply to NodePort traffic, so same as
			// "node to NodePort" above.
			output: "10.180.0.3:80, 10.180.1.3:80",
			masq:   true,
		},
		{
			name:     "node to LB with iTP:Local",
			sourceIP: testNodeIP,
			destIP:   "9.10.11.12",
			destPort: 80,

			// internalTrafficPolicy does not apply to LoadBalancer traffic, so
			// same as "node to LB" above.
			output: "10.180.0.3:80, 10.180.1.3:80",
			masq:   true,
		},
		{
			name:     "external to ClusterIP with iTP:Local",
			sourceIP: testExternalClient,
			destIP:   "172.30.0.43",
			destPort: 80,

			// internalTrafficPolicy applies; only the local endpoint is selected.
			// Traffic is masqueraded as in the "external to ClusterIP" case
			// because internalTrafficPolicy does not affect masquerading.
			output: "10.180.0.3:80",
			masq:   true,
		},
		{
			name:     "external to NodePort with iTP:Local",
			sourceIP: testExternalClient,
			destIP:   testNodeIP,
			destPort: 3003,

			// internalTrafficPolicy does not apply to NodePort traffic, so same as
			// "external to NodePort" above.
			output: "10.180.0.3:80, 10.180.1.3:80",
			masq:   true,
		},
		{
			name:     "external to LB with iTP:Local",
			sourceIP: testExternalClient,
			destIP:   "9.10.11.12",
			destPort: 80,

			// internalTrafficPolicy does not apply to LoadBalancer traffic, so
			// same as "external to LB" above.
			output: "10.180.0.3:80, 10.180.1.3:80",
			masq:   true,
		},
	}

	type packetFlowTestOverride struct {
		output *string
		masq   *bool
	}

	testCases := []struct {
		name          string
		line          string
		masqueradeAll bool
		localDetector bool
		overrides     map[string]packetFlowTestOverride
	}{
		{
			name:          "base",
			line:          getLine(),
			masqueradeAll: false,
			localDetector: true,
			overrides:     nil,
		},
		{
			name:          "no LocalTrafficDetector",
			line:          getLine(),
			masqueradeAll: false,
			localDetector: false,
			overrides: map[string]packetFlowTestOverride{
				// With no LocalTrafficDetector, all traffic to a
				// ClusterIP is assumed to be from a pod, and thus to not
				// require masquerading.
				"node to ClusterIP": {
					masq: ptr.To(false),
				},
				"node to ClusterIP with eTP:Local": {
					masq: ptr.To(false),
				},
				"node to ClusterIP with iTP:Local": {
					masq: ptr.To(false),
				},
				"external to ClusterIP": {
					masq: ptr.To(false),
				},
				"external to ClusterIP with eTP:Local": {
					masq: ptr.To(false),
				},
				"external to ClusterIP with iTP:Local": {
					masq: ptr.To(false),
				},

				// And there's no eTP:Local short-circuit for pod traffic,
				// so pods get only the local endpoints.
				"pod to NodePort with eTP:Local": {
					output: ptr.To("10.180.0.2:80"),
				},
				"pod to LB with eTP:Local": {
					output: ptr.To("10.180.0.2:80"),
				},
			},
		},
		{
			name:          "masqueradeAll",
			line:          getLine(),
			masqueradeAll: true,
			localDetector: true,
			overrides: map[string]packetFlowTestOverride{
				// All "to ClusterIP" traffic gets masqueraded when using
				// --masquerade-all.
				"pod to ClusterIP": {
					masq: ptr.To(true),
				},
				"pod to ClusterIP with eTP:Local": {
					masq: ptr.To(true),
				},
				"pod to ClusterIP with iTP:Local": {
					masq: ptr.To(true),
				},
			},
		},
		{
			name:          "masqueradeAll, no LocalTrafficDetector",
			line:          getLine(),
			masqueradeAll: true,
			localDetector: false,
			overrides: map[string]packetFlowTestOverride{
				// As in "masqueradeAll"
				"pod to ClusterIP": {
					masq: ptr.To(true),
				},
				"pod to ClusterIP with eTP:Local": {
					masq: ptr.To(true),
				},
				"pod to ClusterIP with iTP:Local": {
					masq: ptr.To(true),
				},

				// As in "no LocalTrafficDetector"
				"pod to NodePort with eTP:Local": {
					output: ptr.To("10.180.0.2:80"),
				},
				"pod to LB with eTP:Local": {
					output: ptr.To("10.180.0.2:80"),
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nft, fp := NewFakeProxier(v1.IPv4Protocol)
			fp.masqueradeAll = tc.masqueradeAll
			if !tc.localDetector {
				fp.localDetector = proxyutil.NewNoOpLocalDetector()
			}
			setupTest(fp)

			// Merge base flowTests with per-test-case overrides
			tcFlowTests := make([]packetFlowTest, len(flowTests))
			overridesApplied := 0
			for i := range flowTests {
				tcFlowTests[i] = flowTests[i]
				if overrides, set := tc.overrides[flowTests[i].name]; set {
					overridesApplied++
					if overrides.masq != nil {
						if tcFlowTests[i].masq == *overrides.masq {
							t.Errorf("%q override value for masq is same as base value", flowTests[i].name)
						}
						tcFlowTests[i].masq = *overrides.masq
					}
					if overrides.output != nil {
						if tcFlowTests[i].output == *overrides.output {
							t.Errorf("%q override value for output is same as base value", flowTests[i].name)
						}
						tcFlowTests[i].output = *overrides.output
					}
				}
			}
			if overridesApplied != len(tc.overrides) {
				t.Errorf("%d overrides did not match any test case name!", len(tc.overrides)-overridesApplied)
			}
			runPacketFlowTests(t, tc.line, nft, testNodeIPs, tcFlowTests)
		})
	}
}

// Test calling syncProxyRules() multiple times with various changes
func TestSyncProxyRulesRepeated(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)

	// Helper function to make it look like time has passed (from the point of view of
	// the stale-chain-deletion code).
	ageStaleChains := func() {
		for chain, t := range fp.staleChains {
			fp.staleChains[chain] = t.Add(-2 * time.Second)
		}
	}

	// Create initial state
	var svc2 *v1.Service

	makeServiceMap(fp,
		makeTestService("ns1", "svc1", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = "172.30.0.41"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
			}}
		}),
		makeTestService("ns2", "svc2", func(svc *v1.Service) {
			svc2 = svc
			svc.Spec.Type = v1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = "172.30.0.42"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p8080",
				Port:     8080,
				Protocol: v1.ProtocolTCP,
			}}
		}),
	)

	populateEndpointSlices(fp,
		makeTestEndpointSlice("ns1", "svc1", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.0.1.1"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
		makeTestEndpointSlice("ns2", "svc2", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.0.2.1"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p8080"),
				Port:     ptr.To[int32](8080),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)

	fp.syncProxyRules()

	expected := baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.42 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.42 . tcp . 8080 : goto service-MHHHYRWA-ns2/svc2/tcp/p8080 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-MHHHYRWA-ns2/svc2/tcp/p8080
		add rule ip kube-proxy service-MHHHYRWA-ns2/svc2/tcp/p8080 ip daddr 172.30.0.42 tcp dport 8080 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-MHHHYRWA-ns2/svc2/tcp/p8080 numgen random mod 1 vmap { 0 : goto endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080 }
		add chain ip kube-proxy endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080
		add rule ip kube-proxy endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080 ip saddr 10.0.2.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080 meta l4proto tcp dnat to 10.0.2.1:8080
                `)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())

	// Add a new service and its endpoints
	makeServiceMap(fp,
		makeTestService("ns3", "svc3", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = "172.30.0.43"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
			}}
		}),
	)
	var eps3 *discovery.EndpointSlice
	populateEndpointSlices(fp,
		makeTestEndpointSlice("ns3", "svc3", 1, func(eps *discovery.EndpointSlice) {
			eps3 = eps
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.0.3.1"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)
	fp.syncProxyRules()

	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.42 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.42 . tcp . 8080 : goto service-MHHHYRWA-ns2/svc2/tcp/p8080 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-MHHHYRWA-ns2/svc2/tcp/p8080
		add rule ip kube-proxy service-MHHHYRWA-ns2/svc2/tcp/p8080 ip daddr 172.30.0.42 tcp dport 8080 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-MHHHYRWA-ns2/svc2/tcp/p8080 numgen random mod 1 vmap { 0 : goto endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080 }
		add chain ip kube-proxy endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080
		add rule ip kube-proxy endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080 ip saddr 10.0.2.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080 meta l4proto tcp dnat to 10.0.2.1:8080

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 }
		add chain ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 ip saddr 10.0.3.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 meta l4proto tcp dnat to 10.0.3.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// add 1 element to cluster-ips and service-ips = 2 operations
	// add+flush 2 chains for service and endpoint, add 2 rules in each = 8 operations
	// 10 operations total.
	if nft.LastTransaction.NumOperations() != 10 {
		t.Errorf("Expected 10 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	// Delete a service; its chains will be flushed, but not immediately deleted.
	fp.OnServiceDelete(svc2)
	fp.syncProxyRules()
	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-MHHHYRWA-ns2/svc2/tcp/p8080
		add chain ip kube-proxy endpoint-7RVP4LUQ-ns2/svc2/tcp/p8080__10.0.2.1/8080

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 }
		add chain ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 ip saddr 10.0.3.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 meta l4proto tcp dnat to 10.0.3.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// delete 1 element from cluster-ips and service-ips = 2 operations
	// flush 2 chains for service and endpoint = 2 operations
	// 4 operations total.
	if nft.LastTransaction.NumOperations() != 4 {
		t.Errorf("Expected 4 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	// Fake the passage of time and confirm that the stale chains get deleted.
	ageStaleChains()
	fp.syncProxyRules()
	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 }
		add chain ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 ip saddr 10.0.3.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 meta l4proto tcp dnat to 10.0.3.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// delete stale chains happens in a separate transaction, nothing else changed => last transaction will have 0 operations.
	if nft.LastTransaction.NumOperations() != 0 {
		t.Errorf("Expected 0 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	// Add a service, sync, then add its endpoints.
	makeServiceMap(fp,
		makeTestService("ns4", "svc4", func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = "172.30.0.44"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
			}}
		}),
	)
	fp.syncProxyRules()
	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy cluster-ips { 172.30.0.44 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 }
		add chain ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 ip saddr 10.0.3.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 meta l4proto tcp dnat to 10.0.3.1:80

		add element ip kube-proxy no-endpoint-services { 172.30.0.44 . tcp . 80 comment "ns4/svc4:p80" : goto reject-chain }
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// add 1 element to cluster-ips and no-endpoint-services = 2 operations
	if nft.LastTransaction.NumOperations() != 2 {
		t.Errorf("Expected 2 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	populateEndpointSlices(fp,
		makeTestEndpointSlice("ns4", "svc4", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.0.4.1"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)
	fp.syncProxyRules()
	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy cluster-ips { 172.30.0.44 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.44 . tcp . 80 : goto service-LAUZTJTB-ns4/svc4/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 }
		add chain ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 ip saddr 10.0.3.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80 meta l4proto tcp dnat to 10.0.3.1:80

		add chain ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 ip daddr 172.30.0.44 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 }
		add chain ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 ip saddr 10.0.4.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 meta l4proto tcp dnat to 10.0.4.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// add 1 element to service-ips, remove 1 element from no-endpoint-services = 2 operations
	// add+flush 2 chains for service and endpoint, add 2 rules in each = 8 operations
	if nft.LastTransaction.NumOperations() != 10 {
		t.Errorf("Expected 10 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	// Change an endpoint of an existing service.
	eps3update := eps3.DeepCopy()
	eps3update.Endpoints[0].Addresses[0] = "10.0.3.2"
	fp.OnEndpointSliceUpdate(eps3, eps3update)
	fp.syncProxyRules()

	// The old endpoint chain (for 10.0.3.1) will not be deleted yet.
	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy cluster-ips { 172.30.0.44 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.44 . tcp . 80 : goto service-LAUZTJTB-ns4/svc4/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 }
		add chain ip kube-proxy endpoint-2OCDJSZQ-ns3/svc3/tcp/p80__10.0.3.1/80
		add chain ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80
		add rule ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 ip saddr 10.0.3.2 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 meta l4proto tcp dnat to 10.0.3.2:80

		add chain ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 ip daddr 172.30.0.44 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 }
		add chain ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 ip saddr 10.0.4.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 meta l4proto tcp dnat to 10.0.4.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// add+flush 2 chains for service and endpoint, add 2 rules in each = 8 operations
	// flush old endpoint chain = 1 operation
	if nft.LastTransaction.NumOperations() != 9 {
		t.Errorf("Expected 9 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	// (Ensure the old svc3 chain gets deleted in the next sync.)
	ageStaleChains()

	// Add an endpoint to a service.
	eps3update2 := eps3update.DeepCopy()
	eps3update2.Endpoints = append(eps3update2.Endpoints, discovery.Endpoint{Addresses: []string{"10.0.3.3"}})
	fp.OnEndpointSliceUpdate(eps3update, eps3update2)
	fp.syncProxyRules()

	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy cluster-ips { 172.30.0.44 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.44 . tcp . 80 : goto service-LAUZTJTB-ns4/svc4/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 2 vmap { 0 : goto endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 , 1 : goto endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80 }
		add chain ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80
		add rule ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 ip saddr 10.0.3.2 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 meta l4proto tcp dnat to 10.0.3.2:80
		add chain ip kube-proxy endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80
		add rule ip kube-proxy endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80 ip saddr 10.0.3.3 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80 meta l4proto tcp dnat to 10.0.3.3:80

		add chain ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 ip daddr 172.30.0.44 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 }
		add chain ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 ip saddr 10.0.4.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 meta l4proto tcp dnat to 10.0.4.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// add+flush 3 chains for 1 service and 2 endpoints, add 2 rules in each = 12 operations
	if nft.LastTransaction.NumOperations() != 12 {
		t.Errorf("Expected 12 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	// Empty a service's endpoints; its chains will be flushed, but not immediately deleted.
	eps3update3 := eps3update2.DeepCopy()
	eps3update3.Endpoints = []discovery.Endpoint{}
	fp.OnEndpointSliceUpdate(eps3update2, eps3update3)
	fp.syncProxyRules()
	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy cluster-ips { 172.30.0.44 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy no-endpoint-services { 172.30.0.43 . tcp . 80 comment "ns3/svc3:p80" : goto reject-chain }
		add element ip kube-proxy service-ips { 172.30.0.44 . tcp . 80 : goto service-LAUZTJTB-ns4/svc4/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add chain ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80
		add chain ip kube-proxy endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80

		add chain ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 ip daddr 172.30.0.44 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 }
		add chain ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 ip saddr 10.0.4.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 meta l4proto tcp dnat to 10.0.4.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// remove 1 element from service-ips, add 1 element to no-endpoint-services = 2 operations
	// flush 3 chains = 3 operations
	if nft.LastTransaction.NumOperations() != 5 {
		t.Errorf("Expected 5 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	expectedStaleChains := sets.NewString("service-4AT6LBPK-ns3/svc3/tcp/p80", "endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80", "endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80")
	gotStaleChains := sets.StringKeySet(fp.staleChains)
	if !expectedStaleChains.Equal(gotStaleChains) {
		t.Errorf("expected stale chains %v, got %v", expectedStaleChains, gotStaleChains)
	}
	// Restore endpoints to non-empty immediately; its chains will be restored, and deleted from staleChains.
	fp.OnEndpointSliceUpdate(eps3update3, eps3update2)
	fp.syncProxyRules()
	expected = baseRules + dedent.Dedent(`
		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy cluster-ips { 172.30.0.43 }
		add element ip kube-proxy cluster-ips { 172.30.0.44 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.43 . tcp . 80 : goto service-4AT6LBPK-ns3/svc3/tcp/p80 }
		add element ip kube-proxy service-ips { 172.30.0.44 . tcp . 80 : goto service-LAUZTJTB-ns4/svc4/tcp/p80 }

		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 }
		add chain ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 ip saddr 10.0.1.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5TPGNJF2-ns1/svc1/tcp/p80__10.0.1.1/80 meta l4proto tcp dnat to 10.0.1.1:80

		add chain ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 ip daddr 172.30.0.43 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-4AT6LBPK-ns3/svc3/tcp/p80 numgen random mod 2 vmap { 0 : goto endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 , 1 : goto endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80 }
		add chain ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80
		add rule ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 ip saddr 10.0.3.2 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-SWWHDC7X-ns3/svc3/tcp/p80__10.0.3.2/80 meta l4proto tcp dnat to 10.0.3.2:80
		add chain ip kube-proxy endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80
		add rule ip kube-proxy endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80 ip saddr 10.0.3.3 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-TQ2QKHCZ-ns3/svc3/tcp/p80__10.0.3.3/80 meta l4proto tcp dnat to 10.0.3.3:80

		add chain ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 ip daddr 172.30.0.44 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-LAUZTJTB-ns4/svc4/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 }
		add chain ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 ip saddr 10.0.4.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-WAHRBT2B-ns4/svc4/tcp/p80__10.0.4.1/80 meta l4proto tcp dnat to 10.0.4.1:80
		`)
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	// remove 1 element from no-endpoint-services, add 1 element to service-ips = 2 operations
	// add+flush 3 chains for 1 service and 2 endpoints, add 2 rules in each = 12 operations
	if nft.LastTransaction.NumOperations() != 14 {
		t.Errorf("Expected 14 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}

	if len(fp.staleChains) != 0 {
		t.Errorf("unexpected stale chains: %v", fp.staleChains)
	}

	// Empty a service's endpoints and restore it after stale chains age.
	// - its chains will be flushed, but not immediately deleted in the first sync.
	// - its chains will be deleted first, then recreated in the second sync.
	fp.OnEndpointSliceUpdate(eps3update2, eps3update3)
	fp.syncProxyRules()
	ageStaleChains()
	fp.OnEndpointSliceUpdate(eps3update3, eps3update2)
	fp.syncProxyRules()
	// The second change counteracts the first one, so same expected rules as last time
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())

	// Sync with no new changes, so same expected rules as last time
	fp.syncProxyRules()
	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
	if nft.LastTransaction.NumOperations() != 0 {
		t.Errorf("Expected 0 trasaction operations, got %d", nft.LastTransaction.NumOperations())
	}
}

func TestNoEndpointsMetric(t *testing.T) {
	type endpoint struct {
		ip       string
		hostname string
	}

	metrics.RegisterMetrics(kubeproxyconfig.ProxyModeNFTables)
	testCases := []struct {
		name                                                string
		internalTrafficPolicy                               *v1.ServiceInternalTrafficPolicy
		externalTrafficPolicy                               v1.ServiceExternalTrafficPolicy
		endpoints                                           []endpoint
		expectedSyncProxyRulesNoLocalEndpointsTotalInternal int
		expectedSyncProxyRulesNoLocalEndpointsTotalExternal int
	}{
		{
			name:                  "internalTrafficPolicy is set and there are local endpoints",
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			endpoints: []endpoint{
				{"10.0.1.1", testHostname},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
		},
		{
			name:                  "externalTrafficPolicy is set and there are local endpoints",
			externalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
			endpoints: []endpoint{
				{"10.0.1.1", testHostname},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
		},
		{
			name:                  "both policies are set and there are local endpoints",
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			externalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
			endpoints: []endpoint{
				{"10.0.1.1", testHostname},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
		},
		{
			name:                  "internalTrafficPolicy is set and there are no local endpoints",
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			endpoints: []endpoint{
				{"10.0.1.1", "host0"},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
			expectedSyncProxyRulesNoLocalEndpointsTotalInternal: 1,
		},
		{
			name:                  "externalTrafficPolicy is set and there are no local endpoints",
			externalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
			endpoints: []endpoint{
				{"10.0.1.1", "host0"},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
			expectedSyncProxyRulesNoLocalEndpointsTotalExternal: 1,
		},
		{
			name:                  "both policies are set and there are no local endpoints",
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			externalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
			endpoints: []endpoint{
				{"10.0.1.1", "host0"},
				{"10.0.1.2", "host1"},
				{"10.0.1.3", "host2"},
			},
			expectedSyncProxyRulesNoLocalEndpointsTotalInternal: 1,
			expectedSyncProxyRulesNoLocalEndpointsTotalExternal: 1,
		},
		{
			name:                  "both policies are set and there are no endpoints at all",
			internalTrafficPolicy: ptr.To(v1.ServiceInternalTrafficPolicyLocal),
			externalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
			endpoints:             []endpoint{},
			expectedSyncProxyRulesNoLocalEndpointsTotalInternal: 0,
			expectedSyncProxyRulesNoLocalEndpointsTotalExternal: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, fp := NewFakeProxier(v1.IPv4Protocol)
			fp.OnServiceSynced()
			fp.OnEndpointSlicesSynced()

			serviceName := "svc1"
			namespaceName := "ns1"

			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespaceName},
				Spec: v1.ServiceSpec{
					ClusterIP: "172.30.1.1",
					Selector:  map[string]string{"foo": "bar"},
					Ports:     []v1.ServicePort{{Name: "", Port: 80, Protocol: v1.ProtocolTCP, NodePort: 123}},
				},
			}
			if tc.internalTrafficPolicy != nil {
				svc.Spec.InternalTrafficPolicy = tc.internalTrafficPolicy
			}
			if tc.externalTrafficPolicy != "" {
				svc.Spec.Type = v1.ServiceTypeNodePort
				svc.Spec.ExternalTrafficPolicy = tc.externalTrafficPolicy
			}

			fp.OnServiceAdd(svc)

			endpointSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-1", serviceName),
					Namespace: namespaceName,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{{
					Name:     ptr.To(""),
					Port:     ptr.To[int32](80),
					Protocol: ptr.To(v1.ProtocolTCP),
				}},
				AddressType: discovery.AddressTypeIPv4,
			}
			for _, ep := range tc.endpoints {
				endpointSlice.Endpoints = append(endpointSlice.Endpoints, discovery.Endpoint{
					Addresses:  []string{ep.ip},
					Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
					NodeName:   ptr.To(ep.hostname),
				})
			}

			fp.OnEndpointSliceAdd(endpointSlice)
			fp.syncProxyRules()
			syncProxyRulesNoLocalEndpointsTotalInternal, err := testutil.GetGaugeMetricValue(metrics.SyncProxyRulesNoLocalEndpointsTotal.WithLabelValues("internal", string(fp.ipFamily)))
			if err != nil {
				t.Errorf("failed to get %s value, err: %v", metrics.SyncProxyRulesNoLocalEndpointsTotal.Name, err)
			}

			if tc.expectedSyncProxyRulesNoLocalEndpointsTotalInternal != int(syncProxyRulesNoLocalEndpointsTotalInternal) {
				t.Errorf("sync_proxy_rules_no_endpoints_total metric mismatch(internal): got=%d, expected %d", int(syncProxyRulesNoLocalEndpointsTotalInternal), tc.expectedSyncProxyRulesNoLocalEndpointsTotalInternal)
			}

			syncProxyRulesNoLocalEndpointsTotalExternal, err := testutil.GetGaugeMetricValue(metrics.SyncProxyRulesNoLocalEndpointsTotal.WithLabelValues("external", string(fp.ipFamily)))
			if err != nil {
				t.Errorf("failed to get %s value(external), err: %v", metrics.SyncProxyRulesNoLocalEndpointsTotal.Name, err)
			}

			if tc.expectedSyncProxyRulesNoLocalEndpointsTotalExternal != int(syncProxyRulesNoLocalEndpointsTotalExternal) {
				t.Errorf("sync_proxy_rules_no_endpoints_total metric mismatch(internal): got=%d, expected %d", int(syncProxyRulesNoLocalEndpointsTotalExternal), tc.expectedSyncProxyRulesNoLocalEndpointsTotalExternal)
			}
		})
	}
}

func TestLoadBalancerIngressRouteTypeProxy(t *testing.T) {
	testCases := []struct {
		name          string
		ipModeEnabled bool
		svcIP         string
		svcLBIP       string
		ipMode        *v1.LoadBalancerIPMode
		expectedRule  bool
	}{
		/* LoadBalancerIPMode disabled */
		{
			name:          "LoadBalancerIPMode disabled, ipMode Proxy",
			ipModeEnabled: false,
			svcIP:         "10.20.30.41",
			svcLBIP:       "1.2.3.4",
			ipMode:        ptr.To(v1.LoadBalancerIPModeProxy),
			expectedRule:  true,
		},
		{
			name:          "LoadBalancerIPMode disabled, ipMode VIP",
			ipModeEnabled: false,
			svcIP:         "10.20.30.42",
			svcLBIP:       "1.2.3.5",
			ipMode:        ptr.To(v1.LoadBalancerIPModeVIP),
			expectedRule:  true,
		},
		{
			name:          "LoadBalancerIPMode disabled, ipMode nil",
			ipModeEnabled: false,
			svcIP:         "10.20.30.43",
			svcLBIP:       "1.2.3.6",
			ipMode:        nil,
			expectedRule:  true,
		},
		/* LoadBalancerIPMode enabled */
		{
			name:          "LoadBalancerIPMode enabled, ipMode Proxy",
			ipModeEnabled: true,
			svcIP:         "10.20.30.41",
			svcLBIP:       "1.2.3.4",
			ipMode:        ptr.To(v1.LoadBalancerIPModeProxy),
			expectedRule:  false,
		},
		{
			name:          "LoadBalancerIPMode enabled, ipMode VIP",
			ipModeEnabled: true,
			svcIP:         "10.20.30.42",
			svcLBIP:       "1.2.3.5",
			ipMode:        ptr.To(v1.LoadBalancerIPModeVIP),
			expectedRule:  true,
		},
		{
			name:          "LoadBalancerIPMode enabled, ipMode nil",
			ipModeEnabled: true,
			svcIP:         "10.20.30.43",
			svcLBIP:       "1.2.3.6",
			ipMode:        nil,
			expectedRule:  true,
		},
	}

	svcPort := 80
	svcNodePort := 3001
	svcPortName := proxy.ServicePortName{
		NamespacedName: makeNSN("ns1", "svc1"),
		Port:           "p80",
		Protocol:       v1.ProtocolTCP,
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if !testCase.ipModeEnabled {
				featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.31"))
			}
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.LoadBalancerIPMode, testCase.ipModeEnabled)
			nft, fp := NewFakeProxier(v1.IPv4Protocol)
			makeServiceMap(fp,
				makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *v1.Service) {
					svc.Spec.Type = "LoadBalancer"
					svc.Spec.ClusterIP = testCase.svcIP
					svc.Spec.Ports = []v1.ServicePort{{
						Name:     svcPortName.Port,
						Port:     int32(svcPort),
						Protocol: v1.ProtocolTCP,
						NodePort: int32(svcNodePort),
					}}
					svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
						IP:     testCase.svcLBIP,
						IPMode: testCase.ipMode,
					}}
				}),
			)

			populateEndpointSlices(fp,
				makeTestEndpointSlice("ns1", "svc1", 1, func(eps *discovery.EndpointSlice) {
					eps.AddressType = discovery.AddressTypeIPv4
					eps.Endpoints = []discovery.Endpoint{{
						Addresses: []string{"10.180.0.1"},
					}}
					eps.Ports = []discovery.EndpointPort{{
						Name:     ptr.To("p80"),
						Port:     ptr.To[int32](80),
						Protocol: ptr.To(v1.ProtocolTCP),
					}}
				}),
			)

			fp.syncProxyRules()

			element := nft.Table.Maps["service-ips"].FindElement(testCase.svcLBIP, "tcp", "80")
			ruleExists := element != nil
			if ruleExists != testCase.expectedRule {
				t.Errorf("unexpected rule for %s", testCase.svcLBIP)
			}
		})
	}
}

func Test_servicePortChainNameBase(t *testing.T) {
	testCases := []struct {
		name     string
		spn      proxy.ServicePortName
		protocol string
		expected string
	}{
		{
			name: "simple",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "testing",
					Name:      "service",
				},
				Port: "http",
			},
			protocol: "tcp",
			expected: "P4ZYZVCF-testing/service/tcp/http",
		},
		{
			name: "different port, different hash",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "testing",
					Name:      "service",
				},
				Port: "https",
			},
			protocol: "tcp",
			expected: "LZBRENCP-testing/service/tcp/https",
		},
		{
			name: "max length",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx",
					Name:      "very-long-service-name-why-would-you-even-do-this-i-mean-really",
				},
				Port: "port-443-providing-the-hypertext-transmission-protocol-with-tls",
			},
			protocol: "sctp",
			expected: "KR6NACJP-very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx/very-long-service-name-why-would-you-even-do-this-i-mean-really/sctp/port-443-providing-the-hypertext-transmission-protocol-with-tls",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			name := servicePortChainNameBase(&tc.spn, tc.protocol)
			if name != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, name)
			}
		})
	}
}

func Test_servicePortEndpointChainNameBase(t *testing.T) {
	testCases := []struct {
		name     string
		spn      proxy.ServicePortName
		protocol string
		endpoint string
		expected string
	}{
		{
			name: "simple",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "testing",
					Name:      "service",
				},
				Port: "http",
			},
			protocol: "tcp",
			endpoint: "10.180.0.1:80",
			expected: "JO2XBXZR-testing/service/tcp/http__10.180.0.1/80",
		},
		{
			name: "different endpoint, different hash",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "testing",
					Name:      "service",
				},
				Port: "http",
			},
			protocol: "tcp",
			endpoint: "10.180.0.2:80",
			expected: "5S6H3H22-testing/service/tcp/http__10.180.0.2/80",
		},
		{
			name: "ipv6",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "testing",
					Name:      "service",
				},
				Port: "http",
			},
			protocol: "tcp",
			endpoint: "[fd80:abcd:12::a1b2:c3d4:e5f6:9999]:80",
			expected: "U7E2ET36-testing/service/tcp/http__fd80.abcd.12..a1b2.c3d4.e5f6.9999/80",
		},
		{
			name: "max length without truncation",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx",
					Name:      "very-long-service-name-why-would-you-even-do-this-i-mean-really",
				},
				Port: "port-443-providing-the-hypertext-transmission-protocol-with-tls",
			},
			protocol: "sctp",
			endpoint: "[1234:5678:9abc:def0::abc:1234]:443",
			expected: "5YS7AFEA-very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx/very-long-service-name-why-would-you-even-do-this-i-mean-really/sctp/port-443-providing-the-hypertext-transmission-protocol-with-tls__1234.5678.9abc.def0..abc.1234/443",
		},
		{
			name: "truncated, 1",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx",
					Name:      "very-long-service-name-why-would-you-even-do-this-i-mean-really",
				},
				Port: "port-443-providing-the-hypertext-transmission-protocol-with-tls",
			},
			protocol: "sctp",
			endpoint: "[1234:5678:9abc:def0::abcd:1234:5678]:443",
			expected: "CI6C53Q3-very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx/very-long-service-name-why-would-you-even-do-this-i-mean-really/sctp/port-443-providing-the-hypertext-transmission-protocol-with-tls__1234.5678.9abc.def0..abcd.1234...",
		},
		{
			name: "truncated, 2 (different IP, which is not visible in the result)",
			spn: proxy.ServicePortName{
				NamespacedName: types.NamespacedName{
					Namespace: "very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx",
					Name:      "very-long-service-name-why-would-you-even-do-this-i-mean-really",
				},
				Port: "port-443-providing-the-hypertext-transmission-protocol-with-tls",
			},
			protocol: "sctp",
			endpoint: "[1234:5678:9abc:def0::abcd:1234:8765]:443",
			expected: "2FLXFK6X-very-long-namespace-name-abcdefghijklmnopqrstuvwxyz0123456789xx/very-long-service-name-why-would-you-even-do-this-i-mean-really/sctp/port-443-providing-the-hypertext-transmission-protocol-with-tls__1234.5678.9abc.def0..abcd.1234...",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			name := servicePortEndpointChainNameBase(&tc.spn, tc.protocol, tc.endpoint)
			if name != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, name)
			}
		})
	}
}

func TestProxier_OnServiceCIDRsChanged(t *testing.T) {
	var proxier *Proxier

	proxier = &Proxier{ipFamily: v1.IPv4Protocol}
	proxier.OnServiceCIDRsChanged([]string{"172.30.0.0/16", "fd00:10:96::/112"})
	assert.Equal(t, "172.30.0.0/16", proxier.serviceCIDRs)

	proxier.OnServiceCIDRsChanged([]string{"172.30.0.0/16", "172.50.0.0/16", "fd00:10:96::/112", "fd00:172:30::/112"})
	assert.Equal(t, "172.30.0.0/16,172.50.0.0/16", proxier.serviceCIDRs)

	proxier = &Proxier{ipFamily: v1.IPv6Protocol}
	proxier.OnServiceCIDRsChanged([]string{"172.30.0.0/16", "fd00:10:96::/112"})
	assert.Equal(t, "fd00:10:96::/112", proxier.serviceCIDRs)

	proxier.OnServiceCIDRsChanged([]string{"172.30.0.0/16", "172.50.0.0/16", "fd00:10:96::/112", "fd00:172:30::/112"})
	assert.Equal(t, "fd00:10:96::/112,fd00:172:30::/112", proxier.serviceCIDRs)
}

// TestBadIPs tests that "bad" IPs and CIDRs in Services/Endpoints are rewritten to
// be "good" in the input provided to nft
func TestBadIPs(t *testing.T) {
	nft, fp := NewFakeProxier(v1.IPv4Protocol)
	metrics.RegisterMetrics(kubeproxyconfig.ProxyModeNFTables)

	makeServiceMap(fp,
		makeTestService("ns1", "svc1", func(svc *v1.Service) {
			svc.Spec.Type = "LoadBalancer"
			svc.Spec.ClusterIP = "172.30.0.041"
			svc.Spec.Ports = []v1.ServicePort{{
				Name:     "p80",
				Port:     80,
				Protocol: v1.ProtocolTCP,
				NodePort: 3001,
			}}
			svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
				IP: "1.2.3.004",
			}}
			svc.Spec.ExternalIPs = []string{"192.168.099.022"}
			svc.Spec.LoadBalancerSourceRanges = []string{"203.0.113.000/025"}
		}),
	)
	populateEndpointSlices(fp,
		makeTestEndpointSlice("ns1", "svc1", 1, func(eps *discovery.EndpointSlice) {
			eps.AddressType = discovery.AddressTypeIPv4
			eps.Endpoints = []discovery.Endpoint{{
				Addresses: []string{"10.180.00.001"},
			}}
			eps.Ports = []discovery.EndpointPort{{
				Name:     ptr.To("p80"),
				Port:     ptr.To[int32](80),
				Protocol: ptr.To(v1.ProtocolTCP),
			}}
		}),
	)

	fp.syncProxyRules()

	expected := baseRules + dedent.Dedent(`
		# svc1
		add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 172.30.0.41 tcp dport 80 ip saddr != 10.0.0.0/8 jump mark-for-masquerade
		add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 1 vmap { 0 : goto endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80 }

		add chain ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80 jump mark-for-masquerade
		add rule ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80 goto service-ULMVA6XW-ns1/svc1/tcp/p80

		add chain ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80
		add rule ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80 ip saddr 10.180.0.1 jump mark-for-masquerade
		add rule ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.180.0.1/80 meta l4proto tcp dnat to 10.180.0.1:80

		add chain ip kube-proxy firewall-ULMVA6XW-ns1/svc1/tcp/p80
		add rule ip kube-proxy firewall-ULMVA6XW-ns1/svc1/tcp/p80 ip saddr != { 203.0.113.0/25 } drop

		add element ip kube-proxy cluster-ips { 172.30.0.41 }
		add element ip kube-proxy service-ips { 172.30.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 192.168.99.22 . tcp . 80 : goto external-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-ips { 1.2.3.4 . tcp . 80 : goto external-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy service-nodeports { tcp . 3001 : goto external-ULMVA6XW-ns1/svc1/tcp/p80 }
		add element ip kube-proxy firewall-ips { 1.2.3.4 . tcp . 80 : goto firewall-ULMVA6XW-ns1/svc1/tcp/p80 }
		`)

	assertNFTablesTransactionEqual(t, getLine(), expected, nft.Dump())
}
