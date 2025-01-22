package netlink

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// AddrAdd will add an IP address to a link device.
//
// Equivalent to: `ip addr add $addr dev $link`
//
// If `addr` is an IPv4 address and the broadcast address is not given, it
// will be automatically computed based on the IP mask if /30 or larger.
// If `net.IPv4zero` is given as the broadcast address, broadcast is disabled.
func AddrAdd(link Link, addr *Addr) error {
	return pkgHandle.AddrAdd(link, addr)
}

// AddrAdd will add an IP address to a link device.
//
// Equivalent to: `ip addr add $addr dev $link`
//
// If `addr` is an IPv4 address and the broadcast address is not given, it
// will be automatically computed based on the IP mask if /30 or larger.
// If `net.IPv4zero` is given as the broadcast address, broadcast is disabled.
func (h *Handle) AddrAdd(link Link, addr *Addr) error {
	req := h.newNetlinkRequest(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	return h.addrHandle(link, addr, req)
}

// AddrReplace will replace (or, if not present, add) an IP address on a link device.
//
// Equivalent to: `ip addr replace $addr dev $link`
//
// If `addr` is an IPv4 address and the broadcast address is not given, it
// will be automatically computed based on the IP mask if /30 or larger.
// If `net.IPv4zero` is given as the broadcast address, broadcast is disabled.
func AddrReplace(link Link, addr *Addr) error {
	return pkgHandle.AddrReplace(link, addr)
}

// AddrReplace will replace (or, if not present, add) an IP address on a link device.
//
// Equivalent to: `ip addr replace $addr dev $link`
//
// If `addr` is an IPv4 address and the broadcast address is not given, it
// will be automatically computed based on the IP mask if /30 or larger.
// If `net.IPv4zero` is given as the broadcast address, broadcast is disabled.
func (h *Handle) AddrReplace(link Link, addr *Addr) error {
	req := h.newNetlinkRequest(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE|unix.NLM_F_ACK)
	return h.addrHandle(link, addr, req)
}

// AddrDel will delete an IP address from a link device.
//
// Equivalent to: `ip addr del $addr dev $link`
func AddrDel(link Link, addr *Addr) error {
	return pkgHandle.AddrDel(link, addr)
}

// AddrDel will delete an IP address from a link device.
//
// Equivalent to: `ip addr del $addr dev $link`
func (h *Handle) AddrDel(link Link, addr *Addr) error {
	req := h.newNetlinkRequest(unix.RTM_DELADDR, unix.NLM_F_ACK)
	return h.addrHandle(link, addr, req)
}

func (h *Handle) addrHandle(link Link, addr *Addr, req *nl.NetlinkRequest) error {
	family := nl.GetIPFamily(addr.IP)
	msg := nl.NewIfAddrmsg(family)
	msg.Scope = uint8(addr.Scope)
	if link == nil {
		msg.Index = uint32(addr.LinkIndex)
	} else {
		base := link.Attrs()
		if addr.Label != "" && !strings.HasPrefix(addr.Label, base.Name) {
			return fmt.Errorf("label must begin with interface name")
		}
		h.ensureIndex(base)
		msg.Index = uint32(base.Index)
	}
	mask := addr.Mask
	if addr.Peer != nil {
		mask = addr.Peer.Mask
	}
	prefixlen, masklen := mask.Size()
	msg.Prefixlen = uint8(prefixlen)
	req.AddData(msg)

	var localAddrData []byte
	if family == FAMILY_V4 {
		localAddrData = addr.IP.To4()
	} else {
		localAddrData = addr.IP.To16()
	}

	localData := nl.NewRtAttr(unix.IFA_LOCAL, localAddrData)
	req.AddData(localData)
	var peerAddrData []byte
	if addr.Peer != nil {
		if family == FAMILY_V4 {
			peerAddrData = addr.Peer.IP.To4()
		} else {
			peerAddrData = addr.Peer.IP.To16()
		}
	} else {
		peerAddrData = localAddrData
	}

	addressData := nl.NewRtAttr(unix.IFA_ADDRESS, peerAddrData)
	req.AddData(addressData)

	if addr.Flags != 0 {
		if addr.Flags <= 0xff {
			msg.IfAddrmsg.Flags = uint8(addr.Flags)
		} else {
			b := make([]byte, 4)
			native.PutUint32(b, uint32(addr.Flags))
			flagsData := nl.NewRtAttr(unix.IFA_FLAGS, b)
			req.AddData(flagsData)
		}
	}

	if family == FAMILY_V4 {
		// Automatically set the broadcast address if it is unset and the
		// subnet is large enough to sensibly have one (/30 or larger).
		// See: RFC 3021
		if addr.Broadcast == nil && prefixlen < 31 {
			calcBroadcast := make(net.IP, masklen/8)
			for i := range localAddrData {
				calcBroadcast[i] = localAddrData[i] | ^mask[i]
			}
			addr.Broadcast = calcBroadcast
		}

		if net.IPv4zero.Equal(addr.Broadcast) {
			addr.Broadcast = nil
		}

		if addr.Broadcast != nil {
			req.AddData(nl.NewRtAttr(unix.IFA_BROADCAST, addr.Broadcast))
		}

		if addr.Label != "" {
			labelData := nl.NewRtAttr(unix.IFA_LABEL, nl.ZeroTerminated(addr.Label))
			req.AddData(labelData)
		}
	}

	// 0 is the default value for these attributes. However, 0 means "expired", while the least-surprising default
	// value should be "forever". To compensate for that, only add the attributes if at least one of the values is
	// non-zero, which means the caller has explicitly set them
	if addr.ValidLft > 0 || addr.PreferedLft > 0 {
		cachedata := nl.IfaCacheInfo{unix.IfaCacheinfo{
			Valid:    uint32(addr.ValidLft),
			Prefered: uint32(addr.PreferedLft),
		}}
		req.AddData(nl.NewRtAttr(unix.IFA_CACHEINFO, cachedata.Serialize()))
	}

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// AddrList gets a list of IP addresses in the system.
// Equivalent to: `ip addr show`.
// The list can be filtered by link and ip family.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func AddrList(link Link, family int) ([]Addr, error) {
	return pkgHandle.AddrList(link, family)
}

// AddrList gets a list of IP addresses in the system.
// Equivalent to: `ip addr show`.
// The list can be filtered by link and ip family.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) AddrList(link Link, family int) ([]Addr, error) {
	req := h.newNetlinkRequest(unix.RTM_GETADDR, unix.NLM_F_DUMP)
	msg := nl.NewIfAddrmsg(family)
	req.AddData(msg)

	msgs, executeErr := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWADDR)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}

	indexFilter := 0
	if link != nil {
		base := link.Attrs()
		h.ensureIndex(base)
		indexFilter = base.Index
	}

	var res []Addr
	for _, m := range msgs {
		addr, msgFamily, err := parseAddr(m)
		if err != nil {
			return res, err
		}

		if link != nil && addr.LinkIndex != indexFilter {
			// Ignore messages from other interfaces
			continue
		}

		if family != FAMILY_ALL && msgFamily != family {
			continue
		}

		res = append(res, addr)
	}

	return res, executeErr
}

func parseAddr(m []byte) (addr Addr, family int, err error) {
	msg := nl.DeserializeIfAddrmsg(m)

	family = -1
	addr.LinkIndex = -1

	attrs, err1 := nl.ParseRouteAttr(m[msg.Len():])
	if err1 != nil {
		err = err1
		return
	}

	family = int(msg.Family)
	addr.LinkIndex = int(msg.Index)

	var local, dst *net.IPNet
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case unix.IFA_ADDRESS:
			dst = &net.IPNet{
				IP:   attr.Value,
				Mask: net.CIDRMask(int(msg.Prefixlen), 8*len(attr.Value)),
			}
		case unix.IFA_LOCAL:
			// iproute2 manual:
			// If a peer address is specified, the local address
			// cannot have a prefix length. The network prefix is
			// associated with the peer rather than with the local
			// address.
			n := 8 * len(attr.Value)
			local = &net.IPNet{
				IP:   attr.Value,
				Mask: net.CIDRMask(n, n),
			}
		case unix.IFA_BROADCAST:
			addr.Broadcast = attr.Value
		case unix.IFA_LABEL:
			addr.Label = string(attr.Value[:len(attr.Value)-1])
		case unix.IFA_FLAGS:
			addr.Flags = int(native.Uint32(attr.Value[0:4]))
		case unix.IFA_CACHEINFO:
			ci := nl.DeserializeIfaCacheInfo(attr.Value)
			addr.PreferedLft = int(ci.Prefered)
			addr.ValidLft = int(ci.Valid)
		}
	}

	// libnl addr.c comment:
	// IPv6 sends the local address as IFA_ADDRESS with no
	// IFA_LOCAL, IPv4 sends both IFA_LOCAL and IFA_ADDRESS
	// with IFA_ADDRESS being the peer address if they differ
	//
	// But obviously, as there are IPv6 PtP addresses, too,
	// IFA_LOCAL should also be handled for IPv6.
	if local != nil {
		if family == FAMILY_V4 && dst != nil && local.IP.Equal(dst.IP) {
			addr.IPNet = dst
		} else {
			addr.IPNet = local
			addr.Peer = dst
		}
	} else {
		addr.IPNet = dst
	}

	addr.Scope = int(msg.Scope)

	return
}

type AddrUpdate struct {
	LinkAddress net.IPNet
	LinkIndex   int
	Flags       int
	Scope       int
	PreferedLft int
	ValidLft    int
	NewAddr     bool // true=added false=deleted
}

// AddrSubscribe takes a chan down which notifications will be sent
// when addresses change.  Close the 'done' chan to stop subscription.
func AddrSubscribe(ch chan<- AddrUpdate, done <-chan struct{}) error {
	return addrSubscribeAt(netns.None(), netns.None(), ch, done, nil, false, 0, nil, false)
}

// AddrSubscribeAt works like AddrSubscribe plus it allows the caller
// to choose the network namespace in which to subscribe (ns).
func AddrSubscribeAt(ns netns.NsHandle, ch chan<- AddrUpdate, done <-chan struct{}) error {
	return addrSubscribeAt(ns, netns.None(), ch, done, nil, false, 0, nil, false)
}

// AddrSubscribeOptions contains a set of options to use with
// AddrSubscribeWithOptions.
type AddrSubscribeOptions struct {
	Namespace              *netns.NsHandle
	ErrorCallback          func(error)
	ListExisting           bool
	ReceiveBufferSize      int
	ReceiveBufferForceSize bool
	ReceiveTimeout         *unix.Timeval
}

// AddrSubscribeWithOptions work like AddrSubscribe but enable to
// provide additional options to modify the behavior. Currently, the
// namespace can be provided as well as an error callback.
func AddrSubscribeWithOptions(ch chan<- AddrUpdate, done <-chan struct{}, options AddrSubscribeOptions) error {
	if options.Namespace == nil {
		none := netns.None()
		options.Namespace = &none
	}
	return addrSubscribeAt(*options.Namespace, netns.None(), ch, done, options.ErrorCallback, options.ListExisting,
		options.ReceiveBufferSize, options.ReceiveTimeout, options.ReceiveBufferForceSize)
}

func addrSubscribeAt(newNs, curNs netns.NsHandle, ch chan<- AddrUpdate, done <-chan struct{}, cberr func(error), listExisting bool,
	rcvbuf int, rcvTimeout *unix.Timeval, rcvBufForce bool) error {
	s, err := nl.SubscribeAt(newNs, curNs, unix.NETLINK_ROUTE, unix.RTNLGRP_IPV4_IFADDR, unix.RTNLGRP_IPV6_IFADDR)
	if err != nil {
		return err
	}
	if rcvTimeout != nil {
		if err := s.SetReceiveTimeout(rcvTimeout); err != nil {
			return err
		}
	}
	if rcvbuf != 0 {
		err = s.SetReceiveBufferSize(rcvbuf, rcvBufForce)
		if err != nil {
			return err
		}
	}
	if done != nil {
		go func() {
			<-done
			s.Close()
		}()
	}
	if listExisting {
		req := pkgHandle.newNetlinkRequest(unix.RTM_GETADDR,
			unix.NLM_F_DUMP)
		infmsg := nl.NewIfInfomsg(unix.AF_UNSPEC)
		req.AddData(infmsg)
		if err := s.Send(req); err != nil {
			return err
		}
	}
	go func() {
		defer close(ch)
		for {
			msgs, from, err := s.Receive()
			if err != nil {
				if cberr != nil {
					cberr(fmt.Errorf("Receive failed: %v",
						err))
				}
				return
			}
			if from.Pid != nl.PidKernel {
				if cberr != nil {
					cberr(fmt.Errorf("Wrong sender portid %d, expected %d", from.Pid, nl.PidKernel))
				}
				continue
			}
			for _, m := range msgs {
				if m.Header.Type == unix.NLMSG_DONE {
					continue
				}
				if m.Header.Type == unix.NLMSG_ERROR {
					error := int32(native.Uint32(m.Data[0:4]))
					if error == 0 {
						continue
					}
					if cberr != nil {
						cberr(fmt.Errorf("error message: %v",
							syscall.Errno(-error)))
					}
					continue
				}
				msgType := m.Header.Type
				if msgType != unix.RTM_NEWADDR && msgType != unix.RTM_DELADDR {
					if cberr != nil {
						cberr(fmt.Errorf("bad message type: %d", msgType))
					}
					continue
				}

				addr, _, err := parseAddr(m.Data)
				if err != nil {
					if cberr != nil {
						cberr(fmt.Errorf("could not parse address: %v", err))
					}
					continue
				}

				ch <- AddrUpdate{LinkAddress: *addr.IPNet,
					LinkIndex:   addr.LinkIndex,
					NewAddr:     msgType == unix.RTM_NEWADDR,
					Flags:       addr.Flags,
					Scope:       addr.Scope,
					PreferedLft: addr.PreferedLft,
					ValidLft:    addr.ValidLft}
			}
		}
	}()

	return nil
}
