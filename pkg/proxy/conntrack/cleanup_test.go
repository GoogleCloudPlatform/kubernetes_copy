//go:build linux
// +build linux

/*
Copyright 2023 The Kubernetes Authors.

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

package conntrack

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/proxy"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

const (
	testServiceName      = "cleanup-test"
	testServiceNamespace = "test"

	testIPFamily       = v1.IPv4Protocol
	testClusterIP      = "172.30.1.1"
	testExternalIP     = "192.168.99.100"
	testLoadBalancerIP = "1.2.3.4"

	testServingEndpointIP    = "10.240.0.4"
	testNonServingEndpointIP = "10.240.1.5"
	testDeletedEndpointIP    = "10.240.2.6"

	testPort     = 8000
	testNodePort = 32000
)

func TestCleanStaleEntries(t *testing.T) {
	// We need to construct proxy.ServicePortMap and proxy.EndpointsMap to pass to
	// CleanStaleEntries. ServicePortMap and EndpointsMap are just maps, but there are
	// no public constructors for any implementation of proxy.ServicePort and
	// proxy.EndpointsMap, so we have to either provide our own implementation of that
	// interface, or else use a proxy.ServiceChangeTracker and proxy.NewEndpointsChangeTracker
	// to construct them and fill in the maps for us.

	sct := proxy.NewServiceChangeTracker(v1.IPv4Protocol, nil, nil)
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testServiceName,
			Namespace: testServiceNamespace,
		},
		Spec: v1.ServiceSpec{
			ClusterIP:   testClusterIP,
			ExternalIPs: []string{testExternalIP},
			Ports: []v1.ServicePort{
				{
					Name:     "test-tcp",
					Port:     testPort,
					Protocol: v1.ProtocolTCP,
				},
				{
					Name:     "test-udp",
					Port:     testPort,
					NodePort: testNodePort,
					Protocol: v1.ProtocolUDP,
				},
				{
					Name:     "test-sctp",
					Port:     testPort,
					NodePort: testNodePort,
					Protocol: v1.ProtocolSCTP,
				},
			},
		},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{
					IP: testLoadBalancerIP,
				}},
			},
		},
	}

	sct.Update(nil, svc)
	svcPortMap := make(proxy.ServicePortMap)
	_ = svcPortMap.Update(sct)

	ect := proxy.NewEndpointsChangeTracker(v1.IPv4Protocol, "test-worker", nil, nil)
	eps := &discovery.EndpointSlice{
		TypeMeta:    metav1.TypeMeta{},
		AddressType: discovery.AddressTypeIPv4,
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-0", testServiceName),
			Namespace: testServiceNamespace,
			Labels:    map[string]string{discovery.LabelServiceName: testServiceName},
		},
		Endpoints: []discovery.Endpoint{
			{
				Addresses:  []string{testServingEndpointIP},
				Conditions: discovery.EndpointConditions{Serving: ptr.To(true)},
			},
			{
				Addresses:  []string{testNonServingEndpointIP},
				Conditions: discovery.EndpointConditions{Serving: ptr.To(false)},
			},
		},
		Ports: []discovery.EndpointPort{
			{
				Name:     ptr.To("test-tcp"),
				Port:     ptr.To(int32(testPort)),
				Protocol: ptr.To(v1.ProtocolTCP),
			},
			{
				Name:     ptr.To("test-udp"),
				Port:     ptr.To(int32(testPort)),
				Protocol: ptr.To(v1.ProtocolUDP),
			},
			{
				Name:     ptr.To("test-sctp"),
				Port:     ptr.To(int32(testPort)),
				Protocol: ptr.To(v1.ProtocolSCTP),
			},
		},
	}

	ect.EndpointSliceUpdate(eps, false)
	endpointsMap := make(proxy.EndpointsMap)
	_ = endpointsMap.Update(ect)

	tcpPortName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{
			Namespace: svc.Namespace,
			Name:      svc.Name,
		},
		Port:     svc.Spec.Ports[0].Name,
		Protocol: svc.Spec.Ports[0].Protocol,
	}

	udpPortName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{
			Namespace: svc.Namespace,
			Name:      svc.Name,
		},
		Port:     svc.Spec.Ports[1].Name,
		Protocol: svc.Spec.Ports[1].Protocol,
	}

	sctpPortName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{
			Namespace: svc.Namespace,
			Name:      svc.Name,
		},
		Port:     svc.Spec.Ports[2].Name,
		Protocol: svc.Spec.Ports[2].Protocol,
	}

	// Sanity-check to make sure we constructed the ServicePortMap correctly
	if len(svcPortMap) != 3 {
		t.Fatalf("expected svcPortMap to have 2 entries, got %+v", svcPortMap)
	}
	servicePort := svcPortMap[tcpPortName]
	if servicePort == nil || servicePort.String() != "172.30.1.1:8000/TCP" {
		t.Fatalf("expected svcPortMap[%q] to be \"172.30.1.1:8000/TCP\", got %q", tcpPortName.String(), servicePort.String())
	}
	servicePort = svcPortMap[udpPortName]
	if servicePort == nil || servicePort.String() != "172.30.1.1:8000/UDP" {
		t.Fatalf("expected svcPortMap[%q] to be \"172.30.1.1:8000/UDP\", got %q", udpPortName.String(), servicePort.String())
	}
	servicePort = svcPortMap[sctpPortName]
	if servicePort == nil || servicePort.String() != "172.30.1.1:8000/SCTP" {
		t.Fatalf("expected svcPortMap[%q] to be \"172.30.1.1:8000/SCTP\", got %q", sctpPortName.String(), servicePort.String())
	}

	// Sanity-check to make sure we constructed the EndpointsMap map correctly
	if len(endpointsMap) != 3 {
		t.Fatalf("expected endpointsMap to have 3 entries, got %+v", endpointsMap)
	}
	for _, svcPortName := range []proxy.ServicePortName{tcpPortName, udpPortName, sctpPortName} {
		if len(endpointsMap[svcPortName]) != 2 {
			t.Fatalf("expected endpointsMap[%q] to have 2 entries, got %+v", svcPortName.String(), endpointsMap[svcPortName])
		}
		if endpointsMap[svcPortName][0].IP() != "10.240.0.4" {
			t.Fatalf("expected endpointsMap[%q][0] IP to be \"10.240.0.4\", got \"%s\"", svcPortName.String(), endpointsMap[svcPortName][0].IP())
		}
		if endpointsMap[svcPortName][1].IP() != "10.240.1.5" {
			t.Fatalf("expected endpointsMap[%q][1] IP to be \"10.240.1.5\", got \"%s\"", svcPortName.String(), endpointsMap[svcPortName][1].IP())
		}
		if !endpointsMap[svcPortName][0].IsServing() {
			t.Fatalf("expected endpointsMap[%q][0] to be serving", svcPortName.String())
		}
		if endpointsMap[svcPortName][1].IsServing() {
			t.Fatalf("expected endpointsMap[%q][1] to be not serving", svcPortName.String())
		}
	}

	// mock existing entries before cleanup
	// we create 36 fake flow entries ( 3 Endpoints * 3 Protocols * ( 3 (ServiceIPs) + 1 (NodePort))
	var mockEntries []*netlink.ConntrackFlow
	// expectedEntries are the entries on which we will assert the cleanup logic
	var expectedEntries []*netlink.ConntrackFlow
	for _, dnatDest := range []string{testServingEndpointIP, testNonServingEndpointIP, testDeletedEndpointIP} {
		for _, proto := range []uint8{unix.IPPROTO_TCP, unix.IPPROTO_UDP, unix.IPPROTO_SCTP} {
			for _, origDest := range []string{testClusterIP, testLoadBalancerIP, testExternalIP} {
				entry := &netlink.ConntrackFlow{
					FamilyType: unix.AF_INET,
					Forward: netlink.IPTuple{
						DstIP:    netutils.ParseIPSloppy(origDest),
						Protocol: proto,
					},
					Reverse: netlink.IPTuple{
						Protocol: proto,
						SrcIP:    netutils.ParseIPSloppy(dnatDest),
					},
				}
				mockEntries = append(mockEntries, entry)
				// we do not expect deleted or non-serving UDP endpoints flows to be present after cleanup
				if !(proto == unix.IPPROTO_UDP && (dnatDest == testNonServingEndpointIP || dnatDest == testDeletedEndpointIP)) {
					expectedEntries = append(expectedEntries, entry)
				}
			}
			entry := &netlink.ConntrackFlow{
				FamilyType: unix.AF_INET,
				Forward: netlink.IPTuple{
					DstPort:  testNodePort,
					Protocol: proto,
				},
				Reverse: netlink.IPTuple{
					Protocol: proto,
					SrcIP:    netutils.ParseIPSloppy(dnatDest),
				},
			}
			mockEntries = append(mockEntries, entry)
			// we do not expect deleted or non-serving UDP endpoints entries to be present after cleanup
			if !(proto == unix.IPPROTO_UDP && (dnatDest == testNonServingEndpointIP || dnatDest == testDeletedEndpointIP)) {
				expectedEntries = append(expectedEntries, entry)
			}
		}
	}

	// add some non-DNATed mock entries which should be cleared up by reconciler
	// These will exist if the proxy don't have DROP/REJECT rule for service with
	// no endpoints, --orig-dst and --reply-src will be same for these entries.
	for _, ip := range []string{testClusterIP, testLoadBalancerIP, testExternalIP} {
		entry := &netlink.ConntrackFlow{
			FamilyType: unix.AF_INET,
			Forward: netlink.IPTuple{
				DstIP:    netutils.ParseIPSloppy(ip),
				Protocol: unix.IPPROTO_UDP,
			},
			Reverse: netlink.IPTuple{
				Protocol: unix.IPPROTO_UDP,
				SrcIP:    netutils.ParseIPSloppy(ip),
			},
		}
		mockEntries = append(mockEntries, entry)
	}

	fake := &fakeHandler{}
	fake.entries = mockEntries

	ct := newConntracker(fake)
	CleanStaleEntries(ct, testIPFamily, svcPortMap, endpointsMap)

	actualEntries, _ := ct.ListEntries(ipFamilyMap[testIPFamily])
	require.Equal(t, len(expectedEntries), len(actualEntries))

	// sort the actual flows before comparison
	sort.Slice(actualEntries, func(i, j int) bool {
		return actualEntries[i].String() < actualEntries[j].String()
	})
	// sort the expected flows before comparison
	sort.Slice(expectedEntries, func(i, j int) bool {
		return expectedEntries[i].String() < expectedEntries[j].String()
	})

	for i := 0; i < len(expectedEntries); i++ {
		require.Equal(t, expectedEntries[i], actualEntries[i])
	}
}

func TestFilterForNAT(t *testing.T) {
	testCases := []struct {
		name           string
		orig           string
		dest           string
		protocol       v1.Protocol
		expectedFilter *conntrackFilter
	}{
		{
			name:     "ipv4 + SCTP",
			orig:     "10.96.0.10",
			dest:     "10.244.0.3",
			protocol: v1.ProtocolSCTP,
			expectedFilter: &conntrackFilter{
				protocol: 132,
				original: &connectionTuple{dstIP: netutils.ParseIPSloppy("10.96.0.10")},
				reply:    &connectionTuple{srcIP: netutils.ParseIPSloppy("10.244.0.3")},
			},
		},
		{
			name:     "ipv6 + UDP",
			orig:     "2001:db8:1::2",
			dest:     "4001:ab8::2",
			protocol: v1.ProtocolUDP,
			expectedFilter: &conntrackFilter{
				protocol: 17,
				original: &connectionTuple{dstIP: netutils.ParseIPSloppy("2001:db8:1::2")},
				reply:    &connectionTuple{srcIP: netutils.ParseIPSloppy("4001:ab8::2")},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expectedFilter, filterForNAT(tc.orig, tc.dest, tc.protocol))
		})
	}
}

func TestFilterForPortNAT(t *testing.T) {
	testCases := []struct {
		name           string
		dest           string
		port           int
		protocol       v1.Protocol
		expectedFamily netlink.InetFamily
		expectedFilter *conntrackFilter
	}{
		{
			name:     "ipv4 + TCP",
			dest:     "10.96.0.10",
			port:     80,
			protocol: v1.ProtocolTCP,
			expectedFilter: &conntrackFilter{
				protocol: 6,
				original: &connectionTuple{dstPort: 80},
				reply:    &connectionTuple{srcIP: netutils.ParseIPSloppy("10.96.0.10")},
			},
		},
		{
			name:     "ipv6 + UDP",
			dest:     "2001:db8:1::2",
			port:     8000,
			protocol: v1.ProtocolUDP,
			expectedFilter: &conntrackFilter{
				protocol: 17,
				original: &connectionTuple{dstPort: 8000},
				reply:    &connectionTuple{srcIP: netutils.ParseIPSloppy("2001:db8:1::2")},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expectedFilter, filterForPortNAT(tc.dest, tc.port, tc.protocol))
		})
	}
}
