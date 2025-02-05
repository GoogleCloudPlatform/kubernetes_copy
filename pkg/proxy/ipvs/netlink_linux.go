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
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	netutils "k8s.io/utils/net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type netlinkHandle struct {
	netlink.Handle
	isIPv6 bool
}

// NewNetLinkHandle will create a new NetLinkHandle
func NewNetLinkHandle(isIPv6 bool) NetLinkHandle {
	return &netlinkHandle{netlink.Handle{}, isIPv6}
}

// EnsureAddressBind checks if address is bound to the interface and, if not, binds it. If the address is already bound, return true.
func (h *netlinkHandle) EnsureAddressBind(address, devName string) (exist bool, err error) {
	dev, err := h.LinkByName(devName)
	if err != nil {
		return false, fmt.Errorf("error get interface: %s, err: %v", devName, err)
	}
	addr := netutils.ParseIPSloppy(address)
	if addr == nil {
		return false, fmt.Errorf("error parse ip address: %s", address)
	}
	if err := h.AddrAdd(dev, &netlink.Addr{IPNet: netlink.NewIPNet(addr)}); err != nil {
		// "EEXIST" will be returned if the address is already bound to device
		if err == unix.EEXIST {
			return true, nil
		}
		return false, fmt.Errorf("error bind address: %s to interface: %s, err: %v", address, devName, err)
	}
	return false, nil
}

// UnbindAddress makes sure IP address is unbound from the network interface.
func (h *netlinkHandle) UnbindAddress(address, devName string) error {
	dev, err := h.LinkByName(devName)
	if err != nil {
		return fmt.Errorf("error get interface: %s, err: %v", devName, err)
	}
	addr := netutils.ParseIPSloppy(address)
	if addr == nil {
		return fmt.Errorf("error parse ip address: %s", address)
	}
	if err := h.AddrDel(dev, &netlink.Addr{IPNet: netlink.NewIPNet(addr)}); err != nil {
		if err != unix.ENXIO {
			return fmt.Errorf("error unbind address: %s from interface: %s, err: %v", address, devName, err)
		}
	}
	return nil
}

// EnsureDummyDevice is part of interface
func (h *netlinkHandle) EnsureDummyDevice(devName string) (bool, error) {
	_, err := h.LinkByName(devName)
	if err == nil {
		// found dummy device
		return true, nil
	}
	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{Name: devName},
	}
	return false, h.LinkAdd(dummy)
}

// DeleteDummyDevice is part of interface.
func (h *netlinkHandle) DeleteDummyDevice(devName string) error {
	link, err := h.LinkByName(devName)
	if err != nil {
		_, ok := err.(netlink.LinkNotFoundError)
		if ok {
			return nil
		}
		return fmt.Errorf("error deleting a non-exist dummy device: %s, %v", devName, err)
	}
	dummy, ok := link.(*netlink.Dummy)
	if !ok {
		return fmt.Errorf("expect dummy device, got device type: %s", link.Type())
	}
	return h.LinkDel(dummy)
}

// ListBindAddress will list all IP addresses which are bound in a given interface
func (h *netlinkHandle) ListBindAddress(devName string) ([]string, error) {
	dev, err := h.LinkByName(devName)
	if err != nil {
		return nil, fmt.Errorf("error get interface: %s, err: %v", devName, err)
	}
	addrs, err := h.AddrList(dev, 0)
	if err != nil {
		return nil, fmt.Errorf("error list bound address of interface: %s, err: %v", devName, err)
	}
	var ips []string
	for _, addr := range addrs {
		ips = append(ips, addr.IP.String())
	}
	return ips, nil
}

// GetAllLocalAddresses return all local addresses on the node.
// Only the addresses of the current family are returned.
// IPv6 link-local and loopback addresses are excluded.
func (h *netlinkHandle) GetAllLocalAddresses() (sets.Set[string], error) {
	addr, err := net.InterfaceAddrs()
	if err != nil {
		err = proxyutil.GetInterfaceAddrsByInterfaces(&addr)
		if err != nil {
			return nil, err
		}
	}
	return proxyutil.AddressSet(h.isValidForSet, addr), nil
}

// GetLocalAddresses return all local addresses for an interface.
// Only the addresses of the current family are returned.
// IPv6 link-local and loopback addresses are excluded.
func (h *netlinkHandle) GetLocalAddresses(dev string) (sets.Set[string], error) {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return nil, fmt.Errorf("Could not get interface %s: %v", dev, err)
	}
	addr, err := ifi.Addrs()
	if err != nil {
		return nil, fmt.Errorf("Can't get addresses from %s: %v", ifi.Name, err)
	}
	return proxyutil.AddressSet(h.isValidForSet, addr), nil
}

func (h *netlinkHandle) isValidForSet(ip net.IP) bool {
	if h.isIPv6 != netutils.IsIPv6(ip) {
		return false
	}
	if h.isIPv6 && ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.IsLoopback() {
		return false
	}
	return true
}

// GetAllLocalAddressesExcept return all local addresses on the node,
// except from the passed dev.  This is not the same as to take the
// diff between GetAllLocalAddresses and GetLocalAddresses since an
// address can be assigned to many interfaces. This problem raised
// https://github.com/kubernetes/kubernetes/issues/114815
func (h *netlinkHandle) GetAllLocalAddressesExcept(dev string) (sets.Set[string], error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var addr []net.Addr
	for _, iface := range ifaces {
		if iface.Name == dev {
			continue
		}
		ifadr, err := iface.Addrs()
		if err != nil {
			// This may happen if the interface was deleted. Ignore
			// but log the error.
			klog.ErrorS(err, "Reading addresses", "interface", iface.Name)
			continue
		}
		addr = append(addr, ifadr...)
	}
	return proxyutil.AddressSet(h.isValidForSet, addr), nil
}
