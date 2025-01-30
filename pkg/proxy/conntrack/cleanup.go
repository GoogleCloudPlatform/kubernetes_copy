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
	"errors"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/proxy"
	netutils "k8s.io/utils/net"
)

// CleanStaleEntries scans conntrack table and removes any entries
// for a service that do not correspond to a serving endpoint.
func CleanStaleEntries(ct Interface, ipFamily v1.IPFamily,
	svcPortMap proxy.ServicePortMap, endpointsMap proxy.EndpointsMap) {

	start := time.Now()
	klog.V(4).InfoS("Started to reconcile conntrack entries", "ipFamily", ipFamily)

	entries, err := ct.ListEntries(ipFamilyMap[ipFamily])
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			klog.V(2).ErrorS(err, "received a partial result, continuing to clean with partial result")
		} else {
			klog.ErrorS(err, "Failed to list conntrack entries")
			return
		}
	}

	// serviceIPEndpointIPs maps service IPs (ClusterIP, LoadBalancerIPs and ExternalIPs)
	// to the set of serving endpoint IPs.
	serviceIPEndpointIPs := make(map[string]sets.Set[string])
	// serviceNodePortEndpointIPs maps service NodePort to the set of serving endpoint IPs.
	serviceNodePortEndpointIPs := make(map[int]sets.Set[string])

	for svcName, svc := range svcPortMap {
		// we are only interested in UDP services
		if svc.Protocol() != v1.ProtocolUDP {
			continue
		}

		endpointIPs := sets.New[string]()
		for _, endpoint := range endpointsMap[svcName] {
			// We need to remove all the conntrack entries for a Service (IP or NodePort)
			// that are not pointing to a serving endpoint.
			// We map all the serving endpoint IPs to the service and clear all the conntrack
			// entries which are destined for the service and are not DNATed to these endpoints.
			// Changes to the service should not affect existing flows, so we do not take
			// traffic policies, topology, or terminating status of the service into account.
			// This ensures that the behavior of UDP services remains consistent with TCP
			// services.
			if endpoint.IsServing() {
				endpointIPs.Insert(endpoint.IP())
			}
		}

		serviceIPEndpointIPs[svc.ClusterIP().String()] = endpointIPs
		for _, loadBalancerIP := range svc.LoadBalancerVIPs() {
			serviceIPEndpointIPs[loadBalancerIP.String()] = endpointIPs
		}
		for _, externalIP := range svc.ExternalIPs() {
			serviceIPEndpointIPs[externalIP.String()] = endpointIPs
		}
		if svc.NodePort() != 0 {
			serviceNodePortEndpointIPs[svc.NodePort()] = endpointIPs
		}
	}

	var filters []netlink.CustomConntrackFilter
	for _, entry := range entries {
		// we only deal with UDP protocol entries
		if entry.Forward.Protocol != unix.IPPROTO_UDP {
			continue
		}

		origDst := entry.Forward.DstIP.String()
		origPortDst := int(entry.Forward.DstPort)
		replySrc := entry.Reverse.SrcIP.String()

		// if the original destination (--orig-dst) of the entry is service IP (ClusterIP,
		// LoadBalancerIPs or ExternalIPs) and the reply source (--reply-src) is not IP of
		// any serving endpoint, we clear the entry.
		if _, ok := serviceIPEndpointIPs[origDst]; ok {
			if !serviceIPEndpointIPs[origDst].Has(replySrc) {
				filters = append(filters, filterForNAT(origDst, replySrc, v1.ProtocolUDP))
			}
		}

		// if the original port destination (--orig-port-dst) of the flow is service
		// NodePort and the reply source (--reply-src) is not IP of any serving endpoint,
		// we clear the entry.
		if _, ok := serviceNodePortEndpointIPs[origPortDst]; ok {
			if !serviceNodePortEndpointIPs[origPortDst].Has(replySrc) {
				filters = append(filters, filterForPortNAT(replySrc, origPortDst, v1.ProtocolUDP))
			}
		}
	}

	if n, err := ct.ClearEntries(ipFamilyMap[ipFamily], filters...); err != nil {
		klog.ErrorS(err, "Failed to clear all conntrack entries", "ipFamily", ipFamily, "entriesDeleted", n, "took", time.Since(start))
	} else {
		klog.V(4).InfoS("Finished reconciling conntrack entries", "ipFamily", ipFamily, "entriesDeleted", n, "took", time.Since(start))
	}
}

// ipFamilyMap maps v1.IPFamily to the corresponding unix constant.
var ipFamilyMap = map[v1.IPFamily]uint8{
	v1.IPv4Protocol: unix.AF_INET,
	v1.IPv6Protocol: unix.AF_INET6,
}

// protocolMap maps v1.Protocol to the Assigned Internet Protocol Number.
// https://www.iana.org/assignments/protocol-numbers/protocol-numbers.xhtml
var protocolMap = map[v1.Protocol]uint8{
	v1.ProtocolTCP:  unix.IPPROTO_TCP,
	v1.ProtocolUDP:  unix.IPPROTO_UDP,
	v1.ProtocolSCTP: unix.IPPROTO_SCTP,
}

// filterForNAT returns *conntrackFilter to delete the conntrack entries for connections
// specified by the destination IP (original direction) and source IP (reply direction).
func filterForNAT(origin, dest string, protocol v1.Protocol) *conntrackFilter {
	klog.V(4).InfoS("Adding conntrack filter for cleanup", "org-dst", origin, "reply-src", dest, "protocol", protocol)
	return &conntrackFilter{
		protocol: protocolMap[protocol],
		original: &connectionTuple{
			dstIP: netutils.ParseIPSloppy(origin),
		},
		reply: &connectionTuple{
			srcIP: netutils.ParseIPSloppy(dest),
		},
	}
}

// filterForPortNAT returns *conntrackFilter to delete the conntrack entries for connections
// specified by the destination Port (original direction) and source IP (reply direction).
func filterForPortNAT(dest string, port int, protocol v1.Protocol) *conntrackFilter {
	klog.V(4).InfoS("Adding conntrack filter for cleanup", "org-port-dst", port, "reply-src", dest, "protocol", protocol)
	return &conntrackFilter{
		protocol: protocolMap[protocol],
		original: &connectionTuple{
			dstPort: uint16(port),
		},
		reply: &connectionTuple{
			srcIP: netutils.ParseIPSloppy(dest),
		},
	}
}
