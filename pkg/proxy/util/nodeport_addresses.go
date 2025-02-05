/*
Copyright 2022 The Kubernetes Authors.

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

package util

import (
	"fmt"
	"net"

	"k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"
)

// NodePortAddresses is used to handle the --nodeport-addresses flag
type NodePortAddresses struct {
	cidrStrings []string

	cidrs                []*net.IPNet
	containsIPv4Loopback bool
	matchAll             bool
}

// RFC 5735 127.0.0.0/8 - This block is assigned for use as the Internet host loopback address
var ipv4LoopbackStart = net.IPv4(127, 0, 0, 0)

// NewNodePortAddresses takes an IP family and the `--nodeport-addresses` value (which is
// assumed to contain only valid CIDRs, potentially of both IP families) and returns a
// NodePortAddresses object for the given family. If there are no CIDRs of the given
// family then the CIDR "0.0.0.0/0" or "::/0" will be added (even if there are CIDRs of
// the other family).
func NewNodePortAddresses(family v1.IPFamily, cidrStrings []string) *NodePortAddresses {
	npa := &NodePortAddresses{}

	// Filter CIDRs to correct family
	for _, str := range cidrStrings {
		if (family == v1.IPv4Protocol) == netutils.IsIPv4CIDRString(str) {
			npa.cidrStrings = append(npa.cidrStrings, str)
		}
	}
	if len(npa.cidrStrings) == 0 {
		if family == v1.IPv4Protocol {
			npa.cidrStrings = []string{IPv4ZeroCIDR}
		} else {
			npa.cidrStrings = []string{IPv6ZeroCIDR}
		}
	}

	// Now parse
	for _, str := range npa.cidrStrings {
		_, cidr, _ := netutils.ParseCIDRSloppy(str)

		if netutils.IsIPv4CIDR(cidr) {
			if cidr.IP.IsLoopback() || cidr.Contains(ipv4LoopbackStart) {
				npa.containsIPv4Loopback = true
			}
		}

		if IsZeroCIDR(str) {
			// Ignore everything else
			npa.cidrs = []*net.IPNet{cidr}
			npa.matchAll = true
			break
		}

		npa.cidrs = append(npa.cidrs, cidr)
	}

	return npa
}

func (npa *NodePortAddresses) String() string {
	return fmt.Sprintf("%v", npa.cidrStrings)
}

// MatchAll returns true if npa matches all node IPs (of npa's given family)
func (npa *NodePortAddresses) MatchAll() bool {
	return npa.matchAll
}

// net.InterfaceAddrs() on linux may return err mistakely
// before https://go-review.googlesource.com/c/go/+/635196
func GetInterfaceAddrsByInterfaces(addr *[]net.Addr) error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("error listing all interface from host, error: %v", err)
	}
	for _, iface := range ifaces {
		ifadr, err := iface.Addrs()
		if err != nil {
			// This may happen if the interface was deleted. Ignore
			// but log the error.
			klog.ErrorS(err, "GetAllLocalAddresses reading addresses", "interface", iface.Name)
			continue
		}
		*addr = append(*addr, ifadr...)
	}
	return nil
}

// GetNodeIPs return all matched node IP addresses for npa's CIDRs. If no matching
// IPs are found, it returns an empty list.
// NetworkInterfacer is injected for test purpose.
func (npa *NodePortAddresses) GetNodeIPs(nw NetworkInterfacer) ([]net.IP, error) {
	addrs, err := nw.InterfaceAddrs()
	if err != nil {
		err = GetInterfaceAddrsByInterfaces(&addrs)
		if err != nil {
			return nil, err
		}
	}

	// Use a map to dedup matches
	addresses := make(map[string]net.IP)
	for _, cidr := range npa.cidrs {
		for _, addr := range addrs {
			var ip net.IP
			// nw.InterfaceAddrs may return net.IPAddr or net.IPNet on windows, and it will return net.IPNet on linux.
			switch v := addr.(type) {
			case *net.IPAddr:
				ip = v.IP
			case *net.IPNet:
				ip = v.IP
			default:
				continue
			}

			if cidr.Contains(ip) {
				addresses[ip.String()] = ip
			}
		}
	}

	ips := make([]net.IP, 0, len(addresses))
	for _, ip := range addresses {
		ips = append(ips, ip)
	}

	return ips, nil
}

// ContainsIPv4Loopback returns true if npa's CIDRs contain an IPv4 loopback address.
func (npa *NodePortAddresses) ContainsIPv4Loopback() bool {
	return npa.containsIPv4Loopback
}
