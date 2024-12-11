// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package netutil

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"slices"
	"syscall"

	"go.etcd.io/etcd/pkg/v3/cpuutil"
)

var errNoDefaultRoute = fmt.Errorf("could not find default route")
var errNoDefaultHost = fmt.Errorf("could not find default host")
var errNoDefaultInterface = fmt.Errorf("could not find default interface")

// GetDefaultHost obtains the first IP address of machine from the routing table and returns the IP address as string.
// An IPv4 address is preferred to an IPv6 address for backward compatibility.
func GetDefaultHost() (string, error) {
	rmsgs, rerr := getDefaultRoutes()
	if rerr != nil {
		return "", rerr
	}

	// prioritize IPv4
	if rmsg, ok := rmsgs[syscall.AF_INET]; ok {
		if host, err := chooseHost(syscall.AF_INET, rmsg); host != "" || err != nil {
			return host, err
		}
		delete(rmsgs, syscall.AF_INET)
	}

	// sort so choice is deterministic
	var families []uint8
	for family := range rmsgs {
		families = append(families, family)
	}
	slices.Sort(families)

	for _, family := range families {
		if host, err := chooseHost(family, rmsgs[family]); host != "" || err != nil {
			return host, err
		}
	}

	return "", errNoDefaultHost
}

func chooseHost(family uint8, rmsg *syscall.NetlinkMessage) (string, error) {
	host, oif, err := parsePREFSRC(rmsg)
	if host != "" || err != nil {
		return host, err
	}

	// prefsrc not detected, fall back to getting address from iface
	ifmsg, ierr := getIfaceAddr(oif, family)
	if ierr != nil {
		return "", ierr
	}

	attrs, aerr := syscall.ParseNetlinkRouteAttr(ifmsg)
	if aerr != nil {
		return "", aerr
	}

	for _, attr := range attrs {
		// search for RTA_DST because ipv6 doesn't have RTA_SRC
		if attr.Attr.Type == syscall.RTA_DST {
			return net.IP(attr.Value).String(), nil
		}
	}

	return "", nil
}

func getDefaultRoutes() (map[uint8]*syscall.NetlinkMessage, error) {
	dat, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}

	msgs, msgErr := syscall.ParseNetlinkMessage(dat)
	if msgErr != nil {
		return nil, msgErr
	}

	routes := make(map[uint8]*syscall.NetlinkMessage)
	rtmsg := syscall.RtMsg{}
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWROUTE {
			continue
		}
		buf := bytes.NewBuffer(m.Data[:syscall.SizeofRtMsg])
		if rerr := binary.Read(buf, cpuutil.ByteOrder(), &rtmsg); rerr != nil {
			continue
		}
		if rtmsg.Dst_len == 0 && rtmsg.Table == syscall.RT_TABLE_MAIN {
			// zero-length Dst_len implies default route
			msg := m
			routes[rtmsg.Family] = &msg
		}
	}

	if len(routes) > 0 {
		return routes, nil
	}

	return nil, errNoDefaultRoute
}

// Used to get an address of interface.
func getIfaceAddr(idx uint32, family uint8) (*syscall.NetlinkMessage, error) {
	dat, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, int(family))
	if err != nil {
		return nil, err
	}

	msgs, msgErr := syscall.ParseNetlinkMessage(dat)
	if msgErr != nil {
		return nil, msgErr
	}

	ifaddrmsg := syscall.IfAddrmsg{}
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWADDR {
			continue
		}
		buf := bytes.NewBuffer(m.Data[:syscall.SizeofIfAddrmsg])
		if rerr := binary.Read(buf, cpuutil.ByteOrder(), &ifaddrmsg); rerr != nil {
			continue
		}
		if ifaddrmsg.Index == idx {
			return &m, nil
		}
	}

	return nil, fmt.Errorf("could not find address for interface index %v", idx)
}

// Used to get a name of interface.
func getIfaceLink(idx uint32) (*syscall.NetlinkMessage, error) {
	dat, err := syscall.NetlinkRIB(syscall.RTM_GETLINK, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}

	msgs, msgErr := syscall.ParseNetlinkMessage(dat)
	if msgErr != nil {
		return nil, msgErr
	}

	ifinfomsg := syscall.IfInfomsg{}
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWLINK {
			continue
		}
		buf := bytes.NewBuffer(m.Data[:syscall.SizeofIfInfomsg])
		if rerr := binary.Read(buf, cpuutil.ByteOrder(), &ifinfomsg); rerr != nil {
			continue
		}
		if ifinfomsg.Index == int32(idx) {
			return &m, nil
		}
	}

	return nil, fmt.Errorf("could not find link for interface index %v", idx)
}

// GetDefaultInterfaces gets names of interfaces and returns a map[interface]families.
func GetDefaultInterfaces() (map[string]uint8, error) {
	interfaces := make(map[string]uint8)
	rmsgs, rerr := getDefaultRoutes()
	if rerr != nil {
		return interfaces, rerr
	}

	for family, rmsg := range rmsgs {
		_, oif, err := parsePREFSRC(rmsg)
		if err != nil {
			return interfaces, err
		}

		ifmsg, ierr := getIfaceLink(oif)
		if ierr != nil {
			return interfaces, ierr
		}

		attrs, aerr := syscall.ParseNetlinkRouteAttr(ifmsg)
		if aerr != nil {
			return interfaces, aerr
		}

		for _, attr := range attrs {
			if attr.Attr.Type == syscall.IFLA_IFNAME {
				// key is an interface name
				// possible values: 2 - AF_INET, 10 - AF_INET6, 12 - dualstack
				interfaces[string(attr.Value[:len(attr.Value)-1])] += family
			}
		}
	}
	if len(interfaces) > 0 {
		return interfaces, nil
	}
	return interfaces, errNoDefaultInterface
}

// parsePREFSRC returns preferred source address and output interface index (RTA_OIF).
func parsePREFSRC(m *syscall.NetlinkMessage) (host string, oif uint32, err error) {
	var attrs []syscall.NetlinkRouteAttr
	attrs, err = syscall.ParseNetlinkRouteAttr(m)
	if err != nil {
		return "", 0, err
	}

	for _, attr := range attrs {
		if attr.Attr.Type == syscall.RTA_PREFSRC {
			host = net.IP(attr.Value).String()
		}
		if attr.Attr.Type == syscall.RTA_OIF {
			oif = cpuutil.ByteOrder().Uint32(attr.Value)
		}
		if host != "" && oif != uint32(0) {
			break
		}
	}

	if oif == 0 {
		err = errNoDefaultRoute
	}
	return host, oif, err
}
