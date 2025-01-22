package netlink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	SizeofLinkStats32 = 0x5c
	SizeofLinkStats64 = 0xb8
)

const (
	TUNTAP_MODE_TUN             TuntapMode = unix.IFF_TUN
	TUNTAP_MODE_TAP             TuntapMode = unix.IFF_TAP
	TUNTAP_DEFAULTS             TuntapFlag = unix.IFF_TUN_EXCL | unix.IFF_ONE_QUEUE
	TUNTAP_VNET_HDR             TuntapFlag = unix.IFF_VNET_HDR
	TUNTAP_TUN_EXCL             TuntapFlag = unix.IFF_TUN_EXCL
	TUNTAP_NO_PI                TuntapFlag = unix.IFF_NO_PI
	TUNTAP_ONE_QUEUE            TuntapFlag = unix.IFF_ONE_QUEUE
	TUNTAP_MULTI_QUEUE          TuntapFlag = unix.IFF_MULTI_QUEUE
	TUNTAP_MULTI_QUEUE_DEFAULTS TuntapFlag = TUNTAP_MULTI_QUEUE | TUNTAP_NO_PI
)

var StringToTuntapModeMap = map[string]TuntapMode{
	"tun": TUNTAP_MODE_TUN,
	"tap": TUNTAP_MODE_TAP,
}

func (ttm TuntapMode) String() string {
	switch ttm {
	case TUNTAP_MODE_TUN:
		return "tun"
	case TUNTAP_MODE_TAP:
		return "tap"
	}
	return "unknown"
}

const (
	VF_LINK_STATE_AUTO    uint32 = 0
	VF_LINK_STATE_ENABLE  uint32 = 1
	VF_LINK_STATE_DISABLE uint32 = 2
)

var macvlanModes = [...]uint32{
	0,
	nl.MACVLAN_MODE_PRIVATE,
	nl.MACVLAN_MODE_VEPA,
	nl.MACVLAN_MODE_BRIDGE,
	nl.MACVLAN_MODE_PASSTHRU,
	nl.MACVLAN_MODE_SOURCE,
}

func ensureIndex(link *LinkAttrs) {
	if link != nil && link.Index == 0 {
		newlink, _ := LinkByName(link.Name)
		if newlink != nil {
			link.Index = newlink.Attrs().Index
		}
	}
}

func (h *Handle) ensureIndex(link *LinkAttrs) {
	if link != nil && link.Index == 0 {
		newlink, _ := h.LinkByName(link.Name)
		if newlink != nil {
			link.Index = newlink.Attrs().Index
		}
	}
}

func (h *Handle) LinkSetARPOff(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change |= unix.IFF_NOARP
	msg.Flags |= unix.IFF_NOARP
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func LinkSetARPOff(link Link) error {
	return pkgHandle.LinkSetARPOff(link)
}

func (h *Handle) LinkSetARPOn(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change |= unix.IFF_NOARP
	msg.Flags &= ^uint32(unix.IFF_NOARP)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func LinkSetARPOn(link Link) error {
	return pkgHandle.LinkSetARPOn(link)
}

func (h *Handle) SetPromiscOn(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_PROMISC
	msg.Flags = unix.IFF_PROMISC
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetAllmulticastOn enables the reception of all hardware multicast packets for the link device.
// Equivalent to: `ip link set $link allmulticast on`
func LinkSetAllmulticastOn(link Link) error {
	return pkgHandle.LinkSetAllmulticastOn(link)
}

// LinkSetAllmulticastOn enables the reception of all hardware multicast packets for the link device.
// Equivalent to: `ip link set $link allmulticast on`
func (h *Handle) LinkSetAllmulticastOn(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_ALLMULTI
	msg.Flags = unix.IFF_ALLMULTI
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetAllmulticastOff disables the reception of all hardware multicast packets for the link device.
// Equivalent to: `ip link set $link allmulticast off`
func LinkSetAllmulticastOff(link Link) error {
	return pkgHandle.LinkSetAllmulticastOff(link)
}

// LinkSetAllmulticastOff disables the reception of all hardware multicast packets for the link device.
// Equivalent to: `ip link set $link allmulticast off`
func (h *Handle) LinkSetAllmulticastOff(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_ALLMULTI
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetMulticastOn enables the reception of multicast packets for the link device.
// Equivalent to: `ip link set $link multicast on`
func LinkSetMulticastOn(link Link) error {
	return pkgHandle.LinkSetMulticastOn(link)
}

// LinkSetMulticastOn enables the reception of multicast packets for the link device.
// Equivalent to: `ip link set $link multicast on`
func (h *Handle) LinkSetMulticastOn(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_MULTICAST
	msg.Flags = unix.IFF_MULTICAST
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetAllmulticastOff disables the reception of multicast packets for the link device.
// Equivalent to: `ip link set $link multicast off`
func LinkSetMulticastOff(link Link) error {
	return pkgHandle.LinkSetMulticastOff(link)
}

// LinkSetAllmulticastOff disables the reception of multicast packets for the link device.
// Equivalent to: `ip link set $link multicast off`
func (h *Handle) LinkSetMulticastOff(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_MULTICAST
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func MacvlanMACAddrAdd(link Link, addr net.HardwareAddr) error {
	return pkgHandle.MacvlanMACAddrAdd(link, addr)
}

func (h *Handle) MacvlanMACAddrAdd(link Link, addr net.HardwareAddr) error {
	return h.macvlanMACAddrChange(link, []net.HardwareAddr{addr}, nl.MACVLAN_MACADDR_ADD)
}

func MacvlanMACAddrDel(link Link, addr net.HardwareAddr) error {
	return pkgHandle.MacvlanMACAddrDel(link, addr)
}

func (h *Handle) MacvlanMACAddrDel(link Link, addr net.HardwareAddr) error {
	return h.macvlanMACAddrChange(link, []net.HardwareAddr{addr}, nl.MACVLAN_MACADDR_DEL)
}

func MacvlanMACAddrFlush(link Link) error {
	return pkgHandle.MacvlanMACAddrFlush(link)
}

func (h *Handle) MacvlanMACAddrFlush(link Link) error {
	return h.macvlanMACAddrChange(link, nil, nl.MACVLAN_MACADDR_FLUSH)
}

func MacvlanMACAddrSet(link Link, addrs []net.HardwareAddr) error {
	return pkgHandle.MacvlanMACAddrSet(link, addrs)
}

func (h *Handle) MacvlanMACAddrSet(link Link, addrs []net.HardwareAddr) error {
	return h.macvlanMACAddrChange(link, addrs, nl.MACVLAN_MACADDR_SET)
}

func (h *Handle) macvlanMACAddrChange(link Link, addrs []net.HardwareAddr, mode uint32) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated(link.Type()))
	inner := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	// IFLA_MACVLAN_MACADDR_MODE = mode
	b := make([]byte, 4)
	native.PutUint32(b, mode)
	inner.AddRtAttr(nl.IFLA_MACVLAN_MACADDR_MODE, b)

	// populate message with MAC addrs, if necessary
	switch mode {
	case nl.MACVLAN_MACADDR_ADD, nl.MACVLAN_MACADDR_DEL:
		if len(addrs) == 1 {
			inner.AddRtAttr(nl.IFLA_MACVLAN_MACADDR, []byte(addrs[0]))
		}
	case nl.MACVLAN_MACADDR_SET:
		mad := inner.AddRtAttr(nl.IFLA_MACVLAN_MACADDR_DATA, nil)
		for _, addr := range addrs {
			mad.AddRtAttr(nl.IFLA_MACVLAN_MACADDR, []byte(addr))
		}
	}

	req.AddData(linkInfo)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetMacvlanMode sets the mode of a macvlan or macvtap link device.
// Note that passthrough mode cannot be set to and from and will fail.
// Equivalent to: `ip link set $link type (macvlan|macvtap) mode $mode
func LinkSetMacvlanMode(link Link, mode MacvlanMode) error {
	return pkgHandle.LinkSetMacvlanMode(link, mode)
}

// LinkSetMacvlanMode sets the mode of the macvlan or macvtap link device.
// Note that passthrough mode cannot be set to and from and will fail.
// Equivalent to: `ip link set $link type (macvlan|macvtap) mode $mode
func (h *Handle) LinkSetMacvlanMode(link Link, mode MacvlanMode) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated(link.Type()))

	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_MACVLAN_MODE, nl.Uint32Attr(macvlanModes[mode]))

	req.AddData(linkInfo)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func BridgeSetMcastSnoop(link Link, on bool) error {
	return pkgHandle.BridgeSetMcastSnoop(link, on)
}

func (h *Handle) BridgeSetMcastSnoop(link Link, on bool) error {
	bridge := link.(*Bridge)
	bridge.MulticastSnooping = &on
	return h.linkModify(bridge, unix.NLM_F_ACK)
}

func BridgeSetVlanFiltering(link Link, on bool) error {
	return pkgHandle.BridgeSetVlanFiltering(link, on)
}

func (h *Handle) BridgeSetVlanFiltering(link Link, on bool) error {
	bridge := link.(*Bridge)
	bridge.VlanFiltering = &on
	return h.linkModify(bridge, unix.NLM_F_ACK)
}

func BridgeSetVlanDefaultPVID(link Link, pvid uint16) error {
	return pkgHandle.BridgeSetVlanDefaultPVID(link, pvid)
}

func (h *Handle) BridgeSetVlanDefaultPVID(link Link, pvid uint16) error {
	bridge := link.(*Bridge)
	bridge.VlanDefaultPVID = &pvid
	return h.linkModify(bridge, unix.NLM_F_ACK)
}

func SetPromiscOn(link Link) error {
	return pkgHandle.SetPromiscOn(link)
}

func (h *Handle) SetPromiscOff(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_PROMISC
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func SetPromiscOff(link Link) error {
	return pkgHandle.SetPromiscOff(link)
}

// LinkSetUp enables the link device.
// Equivalent to: `ip link set $link up`
func LinkSetUp(link Link) error {
	return pkgHandle.LinkSetUp(link)
}

// LinkSetUp enables the link device.
// Equivalent to: `ip link set $link up`
func (h *Handle) LinkSetUp(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_UP
	msg.Flags = unix.IFF_UP
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetDown disables link device.
// Equivalent to: `ip link set $link down`
func LinkSetDown(link Link) error {
	return pkgHandle.LinkSetDown(link)
}

// LinkSetDown disables link device.
// Equivalent to: `ip link set $link down`
func (h *Handle) LinkSetDown(link Link) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_UP
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetMTU sets the mtu of the link device.
// Equivalent to: `ip link set $link mtu $mtu`
func LinkSetMTU(link Link, mtu int) error {
	return pkgHandle.LinkSetMTU(link, mtu)
}

// LinkSetMTU sets the mtu of the link device.
// Equivalent to: `ip link set $link mtu $mtu`
func (h *Handle) LinkSetMTU(link Link, mtu int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(mtu))

	data := nl.NewRtAttr(unix.IFLA_MTU, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetName sets the name of the link device.
// Equivalent to: `ip link set $link name $name`
func LinkSetName(link Link, name string) error {
	return pkgHandle.LinkSetName(link, name)
}

// LinkSetName sets the name of the link device.
// Equivalent to: `ip link set $link name $name`
func (h *Handle) LinkSetName(link Link, name string) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_IFNAME, []byte(name))
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetAlias sets the alias of the link device.
// Equivalent to: `ip link set dev $link alias $name`
func LinkSetAlias(link Link, name string) error {
	return pkgHandle.LinkSetAlias(link, name)
}

// LinkSetAlias sets the alias of the link device.
// Equivalent to: `ip link set dev $link alias $name`
func (h *Handle) LinkSetAlias(link Link, name string) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_IFALIAS, []byte(name))
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkAddAltName adds a new alternative name for the link device.
// Equivalent to: `ip link property add $link altname $name`
func LinkAddAltName(link Link, name string) error {
	return pkgHandle.LinkAddAltName(link, name)
}

// LinkAddAltName adds a new alternative name for the link device.
// Equivalent to: `ip link property add $link altname $name`
func (h *Handle) LinkAddAltName(link Link, name string) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_NEWLINKPROP, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_PROP_LIST|unix.NLA_F_NESTED, nil)
	data.AddRtAttr(unix.IFLA_ALT_IFNAME, []byte(name))

	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkDelAltName delete an alternative name for the link device.
// Equivalent to: `ip link property del $link altname $name`
func LinkDelAltName(link Link, name string) error {
	return pkgHandle.LinkDelAltName(link, name)
}

// LinkDelAltName delete an alternative name for the link device.
// Equivalent to: `ip link property del $link altname $name`
func (h *Handle) LinkDelAltName(link Link, name string) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_DELLINKPROP, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_PROP_LIST|unix.NLA_F_NESTED, nil)
	data.AddRtAttr(unix.IFLA_ALT_IFNAME, []byte(name))

	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetHardwareAddr sets the hardware address of the link device.
// Equivalent to: `ip link set $link address $hwaddr`
func LinkSetHardwareAddr(link Link, hwaddr net.HardwareAddr) error {
	return pkgHandle.LinkSetHardwareAddr(link, hwaddr)
}

// LinkSetHardwareAddr sets the hardware address of the link device.
// Equivalent to: `ip link set $link address $hwaddr`
func (h *Handle) LinkSetHardwareAddr(link Link, hwaddr net.HardwareAddr) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_ADDRESS, []byte(hwaddr))
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfHardwareAddr sets the hardware address of a vf for the link.
// Equivalent to: `ip link set $link vf $vf mac $hwaddr`
func LinkSetVfHardwareAddr(link Link, vf int, hwaddr net.HardwareAddr) error {
	return pkgHandle.LinkSetVfHardwareAddr(link, vf, hwaddr)
}

// LinkSetVfHardwareAddr sets the hardware address of a vf for the link.
// Equivalent to: `ip link set $link vf $vf mac $hwaddr`
func (h *Handle) LinkSetVfHardwareAddr(link Link, vf int, hwaddr net.HardwareAddr) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfmsg := nl.VfMac{
		Vf: uint32(vf),
	}
	copy(vfmsg.Mac[:], []byte(hwaddr))
	info.AddRtAttr(nl.IFLA_VF_MAC, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfVlan sets the vlan of a vf for the link.
// Equivalent to: `ip link set $link vf $vf vlan $vlan`
func LinkSetVfVlan(link Link, vf, vlan int) error {
	return pkgHandle.LinkSetVfVlan(link, vf, vlan)
}

// LinkSetVfVlan sets the vlan of a vf for the link.
// Equivalent to: `ip link set $link vf $vf vlan $vlan`
func (h *Handle) LinkSetVfVlan(link Link, vf, vlan int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfmsg := nl.VfVlan{
		Vf:   uint32(vf),
		Vlan: uint32(vlan),
	}
	info.AddRtAttr(nl.IFLA_VF_VLAN, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfVlanQos sets the vlan and qos priority of a vf for the link.
// Equivalent to: `ip link set $link vf $vf vlan $vlan qos $qos`
func LinkSetVfVlanQos(link Link, vf, vlan, qos int) error {
	return pkgHandle.LinkSetVfVlanQos(link, vf, vlan, qos)
}

// LinkSetVfVlanQos sets the vlan and qos priority of a vf for the link.
// Equivalent to: `ip link set $link vf $vf vlan $vlan qos $qos`
func (h *Handle) LinkSetVfVlanQos(link Link, vf, vlan, qos int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfmsg := nl.VfVlan{
		Vf:   uint32(vf),
		Vlan: uint32(vlan),
		Qos:  uint32(qos),
	}
	info.AddRtAttr(nl.IFLA_VF_VLAN, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfVlanQosProto sets the vlan, qos and protocol of a vf for the link.
// Equivalent to: `ip link set $link vf $vf vlan $vlan qos $qos proto $proto`
func LinkSetVfVlanQosProto(link Link, vf, vlan, qos, proto int) error {
	return pkgHandle.LinkSetVfVlanQosProto(link, vf, vlan, qos, proto)
}

// LinkSetVfVlanQosProto sets the vlan, qos and protocol of a vf for the link.
// Equivalent to: `ip link set $link vf $vf vlan $vlan qos $qos proto $proto`
func (h *Handle) LinkSetVfVlanQosProto(link Link, vf, vlan, qos, proto int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	vfInfo := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfVlanList := vfInfo.AddRtAttr(nl.IFLA_VF_VLAN_LIST, nil)

	vfmsg := nl.VfVlanInfo{
		VfVlan: nl.VfVlan{
			Vf:   uint32(vf),
			Vlan: uint32(vlan),
			Qos:  uint32(qos),
		},
		VlanProto: (uint16(proto)>>8)&0xFF | (uint16(proto)&0xFF)<<8,
	}

	vfVlanList.AddRtAttr(nl.IFLA_VF_VLAN_INFO, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfTxRate sets the tx rate of a vf for the link.
// Equivalent to: `ip link set $link vf $vf rate $rate`
func LinkSetVfTxRate(link Link, vf, rate int) error {
	return pkgHandle.LinkSetVfTxRate(link, vf, rate)
}

// LinkSetVfTxRate sets the tx rate of a vf for the link.
// Equivalent to: `ip link set $link vf $vf rate $rate`
func (h *Handle) LinkSetVfTxRate(link Link, vf, rate int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfmsg := nl.VfTxRate{
		Vf:   uint32(vf),
		Rate: uint32(rate),
	}
	info.AddRtAttr(nl.IFLA_VF_TX_RATE, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfRate sets the min and max tx rate of a vf for the link.
// Equivalent to: `ip link set $link vf $vf min_tx_rate $min_rate max_tx_rate $max_rate`
func LinkSetVfRate(link Link, vf, minRate, maxRate int) error {
	return pkgHandle.LinkSetVfRate(link, vf, minRate, maxRate)
}

// LinkSetVfRate sets the min and max tx rate of a vf for the link.
// Equivalent to: `ip link set $link vf $vf min_tx_rate $min_rate max_tx_rate $max_rate`
func (h *Handle) LinkSetVfRate(link Link, vf, minRate, maxRate int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfmsg := nl.VfRate{
		Vf:        uint32(vf),
		MinTxRate: uint32(minRate),
		MaxTxRate: uint32(maxRate),
	}
	info.AddRtAttr(nl.IFLA_VF_RATE, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfState enables/disables virtual link state on a vf.
// Equivalent to: `ip link set $link vf $vf state $state`
func LinkSetVfState(link Link, vf int, state uint32) error {
	return pkgHandle.LinkSetVfState(link, vf, state)
}

// LinkSetVfState enables/disables virtual link state on a vf.
// Equivalent to: `ip link set $link vf $vf state $state`
func (h *Handle) LinkSetVfState(link Link, vf int, state uint32) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfmsg := nl.VfLinkState{
		Vf:        uint32(vf),
		LinkState: state,
	}
	info.AddRtAttr(nl.IFLA_VF_LINK_STATE, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfSpoofchk enables/disables spoof check on a vf for the link.
// Equivalent to: `ip link set $link vf $vf spoofchk $check`
func LinkSetVfSpoofchk(link Link, vf int, check bool) error {
	return pkgHandle.LinkSetVfSpoofchk(link, vf, check)
}

// LinkSetVfSpoofchk enables/disables spoof check on a vf for the link.
// Equivalent to: `ip link set $link vf $vf spoofchk $check`
func (h *Handle) LinkSetVfSpoofchk(link Link, vf int, check bool) error {
	var setting uint32
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	if check {
		setting = 1
	}
	vfmsg := nl.VfSpoofchk{
		Vf:      uint32(vf),
		Setting: setting,
	}
	info.AddRtAttr(nl.IFLA_VF_SPOOFCHK, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfTrust enables/disables trust state on a vf for the link.
// Equivalent to: `ip link set $link vf $vf trust $state`
func LinkSetVfTrust(link Link, vf int, state bool) error {
	return pkgHandle.LinkSetVfTrust(link, vf, state)
}

// LinkSetVfTrust enables/disables trust state on a vf for the link.
// Equivalent to: `ip link set $link vf $vf trust $state`
func (h *Handle) LinkSetVfTrust(link Link, vf int, state bool) error {
	var setting uint32
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	if state {
		setting = 1
	}
	vfmsg := nl.VfTrust{
		Vf:      uint32(vf),
		Setting: setting,
	}
	info.AddRtAttr(nl.IFLA_VF_TRUST, vfmsg.Serialize())
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetVfNodeGUID sets the node GUID of a vf for the link.
// Equivalent to: `ip link set dev $link vf $vf node_guid $nodeguid`
func LinkSetVfNodeGUID(link Link, vf int, nodeguid net.HardwareAddr) error {
	return pkgHandle.LinkSetVfGUID(link, vf, nodeguid, nl.IFLA_VF_IB_NODE_GUID)
}

// LinkSetVfPortGUID sets the port GUID of a vf for the link.
// Equivalent to: `ip link set dev $link vf $vf port_guid $portguid`
func LinkSetVfPortGUID(link Link, vf int, portguid net.HardwareAddr) error {
	return pkgHandle.LinkSetVfGUID(link, vf, portguid, nl.IFLA_VF_IB_PORT_GUID)
}

// LinkSetVfGUID sets the node or port GUID of a vf for the link.
func (h *Handle) LinkSetVfGUID(link Link, vf int, vfGuid net.HardwareAddr, guidType int) error {
	var err error
	var guid uint64

	buf := bytes.NewBuffer(vfGuid)
	err = binary.Read(buf, binary.BigEndian, &guid)
	if err != nil {
		return err
	}

	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	data := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	info := data.AddRtAttr(nl.IFLA_VF_INFO, nil)
	vfmsg := nl.VfGUID{
		Vf:   uint32(vf),
		GUID: guid,
	}
	info.AddRtAttr(guidType, vfmsg.Serialize())
	req.AddData(data)

	_, err = req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetMaster sets the master of the link device.
// Equivalent to: `ip link set $link master $master`
func LinkSetMaster(link Link, master Link) error {
	return pkgHandle.LinkSetMaster(link, master)
}

// LinkSetMaster sets the master of the link device.
// Equivalent to: `ip link set $link master $master`
func (h *Handle) LinkSetMaster(link Link, master Link) error {
	index := 0
	if master != nil {
		masterBase := master.Attrs()
		h.ensureIndex(masterBase)
		index = masterBase.Index
	}
	if index <= 0 {
		return fmt.Errorf("Device does not exist")
	}
	return h.LinkSetMasterByIndex(link, index)
}

// LinkSetNoMaster removes the master of the link device.
// Equivalent to: `ip link set $link nomaster`
func LinkSetNoMaster(link Link) error {
	return pkgHandle.LinkSetNoMaster(link)
}

// LinkSetNoMaster removes the master of the link device.
// Equivalent to: `ip link set $link nomaster`
func (h *Handle) LinkSetNoMaster(link Link) error {
	return h.LinkSetMasterByIndex(link, 0)
}

// LinkSetMasterByIndex sets the master of the link device.
// Equivalent to: `ip link set $link master $master`
func LinkSetMasterByIndex(link Link, masterIndex int) error {
	return pkgHandle.LinkSetMasterByIndex(link, masterIndex)
}

// LinkSetMasterByIndex sets the master of the link device.
// Equivalent to: `ip link set $link master $master`
func (h *Handle) LinkSetMasterByIndex(link Link, masterIndex int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(masterIndex))

	data := nl.NewRtAttr(unix.IFLA_MASTER, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetNsPid puts the device into a new network namespace. The
// pid must be a pid of a running process.
// Equivalent to: `ip link set $link netns $pid`
func LinkSetNsPid(link Link, nspid int) error {
	return pkgHandle.LinkSetNsPid(link, nspid)
}

// LinkSetNsPid puts the device into a new network namespace. The
// pid must be a pid of a running process.
// Equivalent to: `ip link set $link netns $pid`
func (h *Handle) LinkSetNsPid(link Link, nspid int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(nspid))

	data := nl.NewRtAttr(unix.IFLA_NET_NS_PID, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetNsFd puts the device into a new network namespace. The
// fd must be an open file descriptor to a network namespace.
// Similar to: `ip link set $link netns $ns`
func LinkSetNsFd(link Link, fd int) error {
	return pkgHandle.LinkSetNsFd(link, fd)
}

// LinkSetNsFd puts the device into a new network namespace. The
// fd must be an open file descriptor to a network namespace.
// Similar to: `ip link set $link netns $ns`
func (h *Handle) LinkSetNsFd(link Link, fd int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(fd))

	data := nl.NewRtAttr(unix.IFLA_NET_NS_FD, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetXdpFd adds a bpf function to the driver. The fd must be a bpf
// program loaded with bpf(type=BPF_PROG_TYPE_XDP)
func LinkSetXdpFd(link Link, fd int) error {
	return LinkSetXdpFdWithFlags(link, fd, 0)
}

// LinkSetXdpFdWithFlags adds a bpf function to the driver with the given
// options. The fd must be a bpf program loaded with bpf(type=BPF_PROG_TYPE_XDP)
func LinkSetXdpFdWithFlags(link Link, fd, flags int) error {
	base := link.Attrs()
	ensureIndex(base)
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	addXdpAttrs(&LinkXdp{Fd: fd, Flags: uint32(flags)}, req)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetGSOMaxSegs sets the GSO maximum segment count of the link device.
// Equivalent to: `ip link set $link gso_max_segs $maxSegs`
func LinkSetGSOMaxSegs(link Link, maxSegs int) error {
	return pkgHandle.LinkSetGSOMaxSegs(link, maxSegs)
}

// LinkSetGSOMaxSegs sets the GSO maximum segment count of the link device.
// Equivalent to: `ip link set $link gso_max_segs $maxSegs`
func (h *Handle) LinkSetGSOMaxSegs(link Link, maxSize int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(maxSize))

	data := nl.NewRtAttr(unix.IFLA_GSO_MAX_SEGS, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetGSOMaxSize sets the IPv6 GSO maximum size of the link device.
// Equivalent to: `ip link set $link gso_max_size $maxSize`
func LinkSetGSOMaxSize(link Link, maxSize int) error {
	return pkgHandle.LinkSetGSOMaxSize(link, maxSize)
}

// LinkSetGSOMaxSize sets the IPv6 GSO maximum size of the link device.
// Equivalent to: `ip link set $link gso_max_size $maxSize`
func (h *Handle) LinkSetGSOMaxSize(link Link, maxSize int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(maxSize))

	data := nl.NewRtAttr(unix.IFLA_GSO_MAX_SIZE, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetGROMaxSize sets the IPv6 GRO maximum size of the link device.
// Equivalent to: `ip link set $link gro_max_size $maxSize`
func LinkSetGROMaxSize(link Link, maxSize int) error {
	return pkgHandle.LinkSetGROMaxSize(link, maxSize)
}

// LinkSetGROMaxSize sets the IPv6 GRO maximum size of the link device.
// Equivalent to: `ip link set $link gro_max_size $maxSize`
func (h *Handle) LinkSetGROMaxSize(link Link, maxSize int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(maxSize))

	data := nl.NewRtAttr(unix.IFLA_GRO_MAX_SIZE, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetGSOIPv4MaxSize sets the IPv4 GSO maximum size of the link device.
// Equivalent to: `ip link set $link gso_ipv4_max_size $maxSize`
func LinkSetGSOIPv4MaxSize(link Link, maxSize int) error {
	return pkgHandle.LinkSetGSOIPv4MaxSize(link, maxSize)
}

// LinkSetGSOIPv4MaxSize sets the IPv4 GSO maximum size of the link device.
// Equivalent to: `ip link set $link gso_ipv4_max_size $maxSize`
func (h *Handle) LinkSetGSOIPv4MaxSize(link Link, maxSize int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(maxSize))

	data := nl.NewRtAttr(unix.IFLA_GSO_IPV4_MAX_SIZE, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetGROIPv4MaxSize sets the IPv4 GRO maximum size of the link device.
// Equivalent to: `ip link set $link gro_ipv4_max_size $maxSize`
func LinkSetGROIPv4MaxSize(link Link, maxSize int) error {
	return pkgHandle.LinkSetGROIPv4MaxSize(link, maxSize)
}

// LinkSetGROIPv4MaxSize sets the IPv4 GRO maximum size of the link device.
// Equivalent to: `ip link set $link gro_ipv4_max_size $maxSize`
func (h *Handle) LinkSetGROIPv4MaxSize(link Link, maxSize int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(maxSize))

	data := nl.NewRtAttr(unix.IFLA_GRO_IPV4_MAX_SIZE, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func boolAttr(val bool) []byte {
	var v uint8
	if val {
		v = 1
	}
	return nl.Uint8Attr(v)
}

type vxlanPortRange struct {
	Lo, Hi uint16
}

func addVxlanAttrs(vxlan *Vxlan, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	if vxlan.FlowBased {
		vxlan.VxlanId = 0
	}

	data.AddRtAttr(nl.IFLA_VXLAN_ID, nl.Uint32Attr(uint32(vxlan.VxlanId)))

	if vxlan.VtepDevIndex != 0 {
		data.AddRtAttr(nl.IFLA_VXLAN_LINK, nl.Uint32Attr(uint32(vxlan.VtepDevIndex)))
	}
	if vxlan.SrcAddr != nil {
		ip := vxlan.SrcAddr.To4()
		if ip != nil {
			data.AddRtAttr(nl.IFLA_VXLAN_LOCAL, []byte(ip))
		} else {
			ip = vxlan.SrcAddr.To16()
			if ip != nil {
				data.AddRtAttr(nl.IFLA_VXLAN_LOCAL6, []byte(ip))
			}
		}
	}
	if vxlan.Group != nil {
		group := vxlan.Group.To4()
		if group != nil {
			data.AddRtAttr(nl.IFLA_VXLAN_GROUP, []byte(group))
		} else {
			group = vxlan.Group.To16()
			if group != nil {
				data.AddRtAttr(nl.IFLA_VXLAN_GROUP6, []byte(group))
			}
		}
	}

	data.AddRtAttr(nl.IFLA_VXLAN_TTL, nl.Uint8Attr(uint8(vxlan.TTL)))
	data.AddRtAttr(nl.IFLA_VXLAN_TOS, nl.Uint8Attr(uint8(vxlan.TOS)))
	data.AddRtAttr(nl.IFLA_VXLAN_LEARNING, boolAttr(vxlan.Learning))
	data.AddRtAttr(nl.IFLA_VXLAN_PROXY, boolAttr(vxlan.Proxy))
	data.AddRtAttr(nl.IFLA_VXLAN_RSC, boolAttr(vxlan.RSC))
	data.AddRtAttr(nl.IFLA_VXLAN_L2MISS, boolAttr(vxlan.L2miss))
	data.AddRtAttr(nl.IFLA_VXLAN_L3MISS, boolAttr(vxlan.L3miss))
	data.AddRtAttr(nl.IFLA_VXLAN_UDP_ZERO_CSUM6_TX, boolAttr(vxlan.UDP6ZeroCSumTx))
	data.AddRtAttr(nl.IFLA_VXLAN_UDP_ZERO_CSUM6_RX, boolAttr(vxlan.UDP6ZeroCSumRx))

	if vxlan.UDPCSum {
		data.AddRtAttr(nl.IFLA_VXLAN_UDP_CSUM, boolAttr(vxlan.UDPCSum))
	}
	if vxlan.GBP {
		data.AddRtAttr(nl.IFLA_VXLAN_GBP, []byte{})
	}
	if vxlan.FlowBased {
		data.AddRtAttr(nl.IFLA_VXLAN_FLOWBASED, boolAttr(vxlan.FlowBased))
	}
	if vxlan.NoAge {
		data.AddRtAttr(nl.IFLA_VXLAN_AGEING, nl.Uint32Attr(0))
	} else if vxlan.Age > 0 {
		data.AddRtAttr(nl.IFLA_VXLAN_AGEING, nl.Uint32Attr(uint32(vxlan.Age)))
	}
	if vxlan.Limit > 0 {
		data.AddRtAttr(nl.IFLA_VXLAN_LIMIT, nl.Uint32Attr(uint32(vxlan.Limit)))
	}
	if vxlan.Port > 0 {
		data.AddRtAttr(nl.IFLA_VXLAN_PORT, htons(uint16(vxlan.Port)))
	}
	if vxlan.PortLow > 0 || vxlan.PortHigh > 0 {
		pr := vxlanPortRange{uint16(vxlan.PortLow), uint16(vxlan.PortHigh)}

		buf := new(bytes.Buffer)
		binary.Write(buf, binary.BigEndian, &pr)

		data.AddRtAttr(nl.IFLA_VXLAN_PORT_RANGE, buf.Bytes())
	}
}

func addBondAttrs(bond *Bond, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	if bond.Mode >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_MODE, nl.Uint8Attr(uint8(bond.Mode)))
	}
	if bond.ActiveSlave >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_ACTIVE_SLAVE, nl.Uint32Attr(uint32(bond.ActiveSlave)))
	}
	if bond.Miimon >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_MIIMON, nl.Uint32Attr(uint32(bond.Miimon)))
	}
	if bond.UpDelay >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_UPDELAY, nl.Uint32Attr(uint32(bond.UpDelay)))
	}
	if bond.DownDelay >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_DOWNDELAY, nl.Uint32Attr(uint32(bond.DownDelay)))
	}
	if bond.UseCarrier >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_USE_CARRIER, nl.Uint8Attr(uint8(bond.UseCarrier)))
	}
	if bond.ArpInterval >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_ARP_INTERVAL, nl.Uint32Attr(uint32(bond.ArpInterval)))
	}
	if bond.ArpIpTargets != nil {
		msg := data.AddRtAttr(nl.IFLA_BOND_ARP_IP_TARGET, nil)
		for i := range bond.ArpIpTargets {
			ip := bond.ArpIpTargets[i].To4()
			if ip != nil {
				msg.AddRtAttr(i, []byte(ip))
				continue
			}
			ip = bond.ArpIpTargets[i].To16()
			if ip != nil {
				msg.AddRtAttr(i, []byte(ip))
			}
		}
	}
	if bond.ArpValidate >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_ARP_VALIDATE, nl.Uint32Attr(uint32(bond.ArpValidate)))
	}
	if bond.ArpAllTargets >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_ARP_ALL_TARGETS, nl.Uint32Attr(uint32(bond.ArpAllTargets)))
	}
	if bond.Primary >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_PRIMARY, nl.Uint32Attr(uint32(bond.Primary)))
	}
	if bond.PrimaryReselect >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_PRIMARY_RESELECT, nl.Uint8Attr(uint8(bond.PrimaryReselect)))
	}
	if bond.FailOverMac >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_FAIL_OVER_MAC, nl.Uint8Attr(uint8(bond.FailOverMac)))
	}
	if bond.XmitHashPolicy >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_XMIT_HASH_POLICY, nl.Uint8Attr(uint8(bond.XmitHashPolicy)))
	}
	if bond.ResendIgmp >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_RESEND_IGMP, nl.Uint32Attr(uint32(bond.ResendIgmp)))
	}
	if bond.NumPeerNotif >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_NUM_PEER_NOTIF, nl.Uint8Attr(uint8(bond.NumPeerNotif)))
	}
	if bond.AllSlavesActive >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_ALL_SLAVES_ACTIVE, nl.Uint8Attr(uint8(bond.AllSlavesActive)))
	}
	if bond.MinLinks >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_MIN_LINKS, nl.Uint32Attr(uint32(bond.MinLinks)))
	}
	if bond.LpInterval >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_LP_INTERVAL, nl.Uint32Attr(uint32(bond.LpInterval)))
	}
	if bond.PacketsPerSlave >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_PACKETS_PER_SLAVE, nl.Uint32Attr(uint32(bond.PacketsPerSlave)))
	}
	if bond.LacpRate >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_AD_LACP_RATE, nl.Uint8Attr(uint8(bond.LacpRate)))
	}
	if bond.AdSelect >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_AD_SELECT, nl.Uint8Attr(uint8(bond.AdSelect)))
	}
	if bond.AdActorSysPrio >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_AD_ACTOR_SYS_PRIO, nl.Uint16Attr(uint16(bond.AdActorSysPrio)))
	}
	if bond.AdUserPortKey >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_AD_USER_PORT_KEY, nl.Uint16Attr(uint16(bond.AdUserPortKey)))
	}
	if bond.AdActorSystem != nil {
		data.AddRtAttr(nl.IFLA_BOND_AD_ACTOR_SYSTEM, []byte(bond.AdActorSystem))
	}
	if bond.TlbDynamicLb >= 0 {
		data.AddRtAttr(nl.IFLA_BOND_TLB_DYNAMIC_LB, nl.Uint8Attr(uint8(bond.TlbDynamicLb)))
	}
}

func cleanupFds(fds []*os.File) {
	for _, f := range fds {
		f.Close()
	}
}

// LinkAdd adds a new link device. The type and features of the device
// are taken from the parameters in the link object.
// Equivalent to: `ip link add $link`
func LinkAdd(link Link) error {
	return pkgHandle.LinkAdd(link)
}

// LinkAdd adds a new link device. The type and features of the device
// are taken from the parameters in the link object.
// Equivalent to: `ip link add $link`
func (h *Handle) LinkAdd(link Link) error {
	return h.linkModify(link, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
}

func LinkModify(link Link) error {
	return pkgHandle.LinkModify(link)
}

func (h *Handle) LinkModify(link Link) error {
	return h.linkModify(link, unix.NLM_F_REQUEST|unix.NLM_F_ACK)
}

func (h *Handle) linkModify(link Link, flags int) error {
	// TODO: support extra data for macvlan
	base := link.Attrs()

	// if tuntap, then the name can be empty, OS will provide a name
	tuntap, isTuntap := link.(*Tuntap)

	if base.Name == "" && !isTuntap {
		return fmt.Errorf("LinkAttrs.Name cannot be empty")
	}

	if isTuntap {
		if tuntap.Mode < unix.IFF_TUN || tuntap.Mode > unix.IFF_TAP {
			return fmt.Errorf("Tuntap.Mode %v unknown", tuntap.Mode)
		}

		queues := tuntap.Queues

		var fds []*os.File
		var req ifReq
		copy(req.Name[:15], base.Name)

		req.Flags = uint16(tuntap.Flags)

		if queues == 0 { //Legacy compatibility
			queues = 1
			if tuntap.Flags == 0 {
				req.Flags = uint16(TUNTAP_DEFAULTS)
			}
		} else {
			// For best peformance set Flags to TUNTAP_MULTI_QUEUE_DEFAULTS | TUNTAP_VNET_HDR
			// when a) KVM has support for this ABI and
			//      b) the value of the flag is queryable using the TUNGETIFF ioctl
			if tuntap.Flags == 0 {
				req.Flags = uint16(TUNTAP_MULTI_QUEUE_DEFAULTS)
			}
		}

		req.Flags |= uint16(tuntap.Mode)
		const TUN = "/dev/net/tun"
		for i := 0; i < queues; i++ {
			localReq := req
			fd, err := unix.Open(TUN, os.O_RDWR|syscall.O_CLOEXEC, 0)
			if err != nil {
				cleanupFds(fds)
				return err
			}

			_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&localReq)))
			if errno != 0 {
				// close the new fd
				unix.Close(fd)
				// and the already opened ones
				cleanupFds(fds)
				return fmt.Errorf("Tuntap IOCTL TUNSETIFF failed [%d], errno %v", i, errno)
			}

			_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TUNSETOWNER, uintptr(tuntap.Owner))
			if errno != 0 {
				cleanupFds(fds)
				return fmt.Errorf("Tuntap IOCTL TUNSETOWNER failed [%d], errno %v", i, errno)
			}

			_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TUNSETGROUP, uintptr(tuntap.Group))
			if errno != 0 {
				cleanupFds(fds)
				return fmt.Errorf("Tuntap IOCTL TUNSETGROUP failed [%d], errno %v", i, errno)
			}

			// Set the tun device to non-blocking before use. The below comment
			// taken from:
			//
			// https://github.com/mistsys/tuntap/commit/161418c25003bbee77d085a34af64d189df62bea
			//
			// Note there is a complication because in go, if a device node is
			// opened, go sets it to use nonblocking I/O. However a /dev/net/tun
			// doesn't work with epoll until after the TUNSETIFF ioctl has been
			// done. So we open the unix fd directly, do the ioctl, then put the
			// fd in nonblocking mode, an then finally wrap it in a os.File,
			// which will see the nonblocking mode and add the fd to the
			// pollable set, so later on when we Read() from it blocked the
			// calling thread in the kernel.
			//
			// See
			//   https://github.com/golang/go/issues/30426
			// which got exposed in go 1.13 by the fix to
			//   https://github.com/golang/go/issues/30624
			err = unix.SetNonblock(fd, true)
			if err != nil {
				cleanupFds(fds)
				return fmt.Errorf("Tuntap set to non-blocking failed [%d], err %v", i, err)
			}

			// create the file from the file descriptor and store it
			file := os.NewFile(uintptr(fd), TUN)
			fds = append(fds, file)

			// 1) we only care for the name of the first tap in the multi queue set
			// 2) if the original name was empty, the localReq has now the actual name
			//
			// In addition:
			// This ensures that the link name is always identical to what the kernel returns.
			// Not only in case of an empty name, but also when using name templates.
			// e.g. when the provided name is "tap%d", the kernel replaces %d with the next available number.
			if i == 0 {
				link.Attrs().Name = strings.Trim(string(localReq.Name[:]), "\x00")
			}

		}

		control := func(file *os.File, f func(fd uintptr)) error {
			name := file.Name()
			conn, err := file.SyscallConn()
			if err != nil {
				return fmt.Errorf("SyscallConn() failed on %s: %v", name, err)
			}
			if err := conn.Control(f); err != nil {
				return fmt.Errorf("Failed to get file descriptor for %s: %v", name, err)
			}
			return nil
		}

		// only persist interface if NonPersist is NOT set
		if !tuntap.NonPersist {
			var errno syscall.Errno
			if err := control(fds[0], func(fd uintptr) {
				_, _, errno = unix.Syscall(unix.SYS_IOCTL, fd, uintptr(unix.TUNSETPERSIST), 1)
			}); err != nil {
				return err
			}
			if errno != 0 {
				cleanupFds(fds)
				return fmt.Errorf("Tuntap IOCTL TUNSETPERSIST failed, errno %v", errno)
			}
		}

		h.ensureIndex(base)

		// can't set master during create, so set it afterwards
		if base.MasterIndex != 0 {
			// TODO: verify MasterIndex is actually a bridge?
			err := h.LinkSetMasterByIndex(link, base.MasterIndex)
			if err != nil {
				// un-persist (e.g. allow the interface to be removed) the tuntap
				// should not hurt if not set prior, condition might be not needed
				if !tuntap.NonPersist {
					// ignore error
					_ = control(fds[0], func(fd uintptr) {
						_, _, _ = unix.Syscall(unix.SYS_IOCTL, fd, uintptr(unix.TUNSETPERSIST), 0)
					})
				}
				cleanupFds(fds)
				return err
			}
		}

		if tuntap.Queues == 0 {
			cleanupFds(fds)
		} else {
			tuntap.Fds = fds
		}

		return nil
	}

	req := h.newNetlinkRequest(unix.RTM_NEWLINK, flags)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	// TODO: make it shorter
	if base.Flags&net.FlagUp != 0 {
		msg.Change = unix.IFF_UP
		msg.Flags = unix.IFF_UP
	}
	if base.Flags&net.FlagBroadcast != 0 {
		msg.Change |= unix.IFF_BROADCAST
		msg.Flags |= unix.IFF_BROADCAST
	}
	if base.Flags&net.FlagLoopback != 0 {
		msg.Change |= unix.IFF_LOOPBACK
		msg.Flags |= unix.IFF_LOOPBACK
	}
	if base.Flags&net.FlagPointToPoint != 0 {
		msg.Change |= unix.IFF_POINTOPOINT
		msg.Flags |= unix.IFF_POINTOPOINT
	}
	if base.Flags&net.FlagMulticast != 0 {
		msg.Change |= unix.IFF_MULTICAST
		msg.Flags |= unix.IFF_MULTICAST
	}
	if base.Index != 0 {
		msg.Index = int32(base.Index)
	}

	req.AddData(msg)

	if base.ParentIndex != 0 {
		b := make([]byte, 4)
		native.PutUint32(b, uint32(base.ParentIndex))
		data := nl.NewRtAttr(unix.IFLA_LINK, b)
		req.AddData(data)
	} else if link.Type() == "ipvlan" || link.Type() == "ipoib" {
		return fmt.Errorf("Can't create %s link without ParentIndex", link.Type())
	}

	nameData := nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(base.Name))
	req.AddData(nameData)

	if base.Alias != "" {
		alias := nl.NewRtAttr(unix.IFLA_IFALIAS, []byte(base.Alias))
		req.AddData(alias)
	}

	if base.MTU > 0 {
		mtu := nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(base.MTU)))
		req.AddData(mtu)
	}

	if base.TxQLen >= 0 {
		qlen := nl.NewRtAttr(unix.IFLA_TXQLEN, nl.Uint32Attr(uint32(base.TxQLen)))
		req.AddData(qlen)
	}

	if base.HardwareAddr != nil {
		hwaddr := nl.NewRtAttr(unix.IFLA_ADDRESS, []byte(base.HardwareAddr))
		req.AddData(hwaddr)
	}

	if base.NumTxQueues > 0 {
		txqueues := nl.NewRtAttr(unix.IFLA_NUM_TX_QUEUES, nl.Uint32Attr(uint32(base.NumTxQueues)))
		req.AddData(txqueues)
	}

	if base.NumRxQueues > 0 {
		rxqueues := nl.NewRtAttr(unix.IFLA_NUM_RX_QUEUES, nl.Uint32Attr(uint32(base.NumRxQueues)))
		req.AddData(rxqueues)
	}

	if base.GSOMaxSegs > 0 {
		gsoAttr := nl.NewRtAttr(unix.IFLA_GSO_MAX_SEGS, nl.Uint32Attr(base.GSOMaxSegs))
		req.AddData(gsoAttr)
	}

	if base.GSOMaxSize > 0 {
		gsoAttr := nl.NewRtAttr(unix.IFLA_GSO_MAX_SIZE, nl.Uint32Attr(base.GSOMaxSize))
		req.AddData(gsoAttr)
	}

	if base.GROMaxSize > 0 {
		groAttr := nl.NewRtAttr(unix.IFLA_GRO_MAX_SIZE, nl.Uint32Attr(base.GROMaxSize))
		req.AddData(groAttr)
	}

	if base.GSOIPv4MaxSize > 0 {
		gsoAttr := nl.NewRtAttr(unix.IFLA_GSO_IPV4_MAX_SIZE, nl.Uint32Attr(base.GSOIPv4MaxSize))
		req.AddData(gsoAttr)
	}

	if base.GROIPv4MaxSize > 0 {
		groAttr := nl.NewRtAttr(unix.IFLA_GRO_IPV4_MAX_SIZE, nl.Uint32Attr(base.GROIPv4MaxSize))
		req.AddData(groAttr)
	}

	if base.Group > 0 {
		groupAttr := nl.NewRtAttr(unix.IFLA_GROUP, nl.Uint32Attr(base.Group))
		req.AddData(groupAttr)
	}

	if base.Namespace != nil {
		var attr *nl.RtAttr
		switch ns := base.Namespace.(type) {
		case NsPid:
			val := nl.Uint32Attr(uint32(ns))
			attr = nl.NewRtAttr(unix.IFLA_NET_NS_PID, val)
		case NsFd:
			val := nl.Uint32Attr(uint32(ns))
			attr = nl.NewRtAttr(unix.IFLA_NET_NS_FD, val)
		}

		req.AddData(attr)
	}

	if base.Xdp != nil {
		addXdpAttrs(base.Xdp, req)
	}

	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated(link.Type()))

	switch link := link.(type) {
	case *Vlan:
		b := make([]byte, 2)
		native.PutUint16(b, uint16(link.VlanId))
		data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
		data.AddRtAttr(nl.IFLA_VLAN_ID, b)

		if link.VlanProtocol != VLAN_PROTOCOL_UNKNOWN {
			data.AddRtAttr(nl.IFLA_VLAN_PROTOCOL, htons(uint16(link.VlanProtocol)))
		}
	case *Netkit:
		if err := addNetkitAttrs(link, linkInfo, flags); err != nil {
			return err
		}
	case *Veth:
		data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
		peer := data.AddRtAttr(nl.VETH_INFO_PEER, nil)
		nl.NewIfInfomsgChild(peer, unix.AF_UNSPEC)
		peer.AddRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(link.PeerName))
		if base.TxQLen >= 0 {
			peer.AddRtAttr(unix.IFLA_TXQLEN, nl.Uint32Attr(uint32(base.TxQLen)))
		}
		if base.NumTxQueues > 0 {
			peer.AddRtAttr(unix.IFLA_NUM_TX_QUEUES, nl.Uint32Attr(uint32(base.NumTxQueues)))
		}
		if base.NumRxQueues > 0 {
			peer.AddRtAttr(unix.IFLA_NUM_RX_QUEUES, nl.Uint32Attr(uint32(base.NumRxQueues)))
		}
		if base.MTU > 0 {
			peer.AddRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(base.MTU)))
		}
		if link.PeerHardwareAddr != nil {
			peer.AddRtAttr(unix.IFLA_ADDRESS, []byte(link.PeerHardwareAddr))
		}
		if link.PeerNamespace != nil {
			switch ns := link.PeerNamespace.(type) {
			case NsPid:
				val := nl.Uint32Attr(uint32(ns))
				peer.AddRtAttr(unix.IFLA_NET_NS_PID, val)
			case NsFd:
				val := nl.Uint32Attr(uint32(ns))
				peer.AddRtAttr(unix.IFLA_NET_NS_FD, val)
			}
		}
	case *Vxlan:
		addVxlanAttrs(link, linkInfo)
	case *Bond:
		addBondAttrs(link, linkInfo)
	case *IPVlan:
		data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
		data.AddRtAttr(nl.IFLA_IPVLAN_MODE, nl.Uint16Attr(uint16(link.Mode)))
		data.AddRtAttr(nl.IFLA_IPVLAN_FLAG, nl.Uint16Attr(uint16(link.Flag)))
	case *IPVtap:
		data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
		data.AddRtAttr(nl.IFLA_IPVLAN_MODE, nl.Uint16Attr(uint16(link.Mode)))
		data.AddRtAttr(nl.IFLA_IPVLAN_FLAG, nl.Uint16Attr(uint16(link.Flag)))
	case *Macvlan:
		addMacvlanAttrs(link, linkInfo)
	case *Macvtap:
		addMacvtapAttrs(link, linkInfo)
	case *Geneve:
		addGeneveAttrs(link, linkInfo)
	case *Gretap:
		addGretapAttrs(link, linkInfo)
	case *Iptun:
		addIptunAttrs(link, linkInfo)
	case *Ip6tnl:
		addIp6tnlAttrs(link, linkInfo)
	case *Sittun:
		addSittunAttrs(link, linkInfo)
	case *Gretun:
		addGretunAttrs(link, linkInfo)
	case *Vti:
		addVtiAttrs(link, linkInfo)
	case *Vrf:
		addVrfAttrs(link, linkInfo)
	case *Bridge:
		addBridgeAttrs(link, linkInfo)
	case *GTP:
		addGTPAttrs(link, linkInfo)
	case *Xfrmi:
		addXfrmiAttrs(link, linkInfo)
	case *IPoIB:
		addIPoIBAttrs(link, linkInfo)
	case *BareUDP:
		addBareUDPAttrs(link, linkInfo)
	}

	req.AddData(linkInfo)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return err
	}

	h.ensureIndex(base)

	// can't set master during create, so set it afterwards
	if base.MasterIndex != 0 {
		// TODO: verify MasterIndex is actually a bridge?
		return h.LinkSetMasterByIndex(link, base.MasterIndex)
	}
	return nil
}

// LinkDel deletes link device. Either Index or Name must be set in
// the link object for it to be deleted. The other values are ignored.
// Equivalent to: `ip link del $link`
func LinkDel(link Link) error {
	return pkgHandle.LinkDel(link)
}

// LinkDel deletes link device. Either Index or Name must be set in
// the link object for it to be deleted. The other values are ignored.
// Equivalent to: `ip link del $link`
func (h *Handle) LinkDel(link Link) error {
	base := link.Attrs()

	h.ensureIndex(base)

	req := h.newNetlinkRequest(unix.RTM_DELLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func (h *Handle) linkByNameDump(name string) (Link, error) {
	links, executeErr := h.LinkList()
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}

	for _, link := range links {
		if link.Attrs().Name == name {
			return link, executeErr
		}

		// support finding interfaces also via altnames
		for _, altName := range link.Attrs().AltNames {
			if altName == name {
				return link, executeErr
			}
		}
	}
	return nil, LinkNotFoundError{fmt.Errorf("Link %s not found", name)}
}

func (h *Handle) linkByAliasDump(alias string) (Link, error) {
	links, executeErr := h.LinkList()
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}

	for _, link := range links {
		if link.Attrs().Alias == alias {
			return link, executeErr
		}
	}
	return nil, LinkNotFoundError{fmt.Errorf("Link alias %s not found", alias)}
}

// LinkByName finds a link by name and returns a pointer to the object.
//
// If the kernel doesn't support IFLA_IFNAME, this method will fall back to
// filtering a dump of all link names. In this case, if the returned error is
// [ErrDumpInterrupted] the result may be missing or outdated.
func LinkByName(name string) (Link, error) {
	return pkgHandle.LinkByName(name)
}

// LinkByName finds a link by name and returns a pointer to the object.
//
// If the kernel doesn't support IFLA_IFNAME, this method will fall back to
// filtering a dump of all link names. In this case, if the returned error is
// [ErrDumpInterrupted] the result may be missing or outdated.
func (h *Handle) LinkByName(name string) (Link, error) {
	if h.lookupByDump {
		return h.linkByNameDump(name)
	}

	req := h.newNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	req.AddData(msg)

	attr := nl.NewRtAttr(unix.IFLA_EXT_MASK, nl.Uint32Attr(nl.RTEXT_FILTER_VF))
	req.AddData(attr)

	nameData := nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(name))
	if len(name) > 15 {
		nameData = nl.NewRtAttr(unix.IFLA_ALT_IFNAME, nl.ZeroTerminated(name))
	}
	req.AddData(nameData)

	link, err := execGetLink(req)
	if err == unix.EINVAL {
		// older kernels don't support looking up via IFLA_IFNAME
		// so fall back to dumping all links
		h.lookupByDump = true
		return h.linkByNameDump(name)
	}

	return link, err
}

// LinkByAlias finds a link by its alias and returns a pointer to the object.
// If there are multiple links with the alias it returns the first one
//
// If the kernel doesn't support IFLA_IFALIAS, this method will fall back to
// filtering a dump of all link names. In this case, if the returned error is
// [ErrDumpInterrupted] the result may be missing or outdated.
func LinkByAlias(alias string) (Link, error) {
	return pkgHandle.LinkByAlias(alias)
}

// LinkByAlias finds a link by its alias and returns a pointer to the object.
// If there are multiple links with the alias it returns the first one
//
// If the kernel doesn't support IFLA_IFALIAS, this method will fall back to
// filtering a dump of all link names. In this case, if the returned error is
// [ErrDumpInterrupted] the result may be missing or outdated.
func (h *Handle) LinkByAlias(alias string) (Link, error) {
	if h.lookupByDump {
		return h.linkByAliasDump(alias)
	}

	req := h.newNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	req.AddData(msg)

	attr := nl.NewRtAttr(unix.IFLA_EXT_MASK, nl.Uint32Attr(nl.RTEXT_FILTER_VF))
	req.AddData(attr)

	nameData := nl.NewRtAttr(unix.IFLA_IFALIAS, nl.ZeroTerminated(alias))
	req.AddData(nameData)

	link, err := execGetLink(req)
	if err == unix.EINVAL {
		// older kernels don't support looking up via IFLA_IFALIAS
		// so fall back to dumping all links
		h.lookupByDump = true
		return h.linkByAliasDump(alias)
	}

	return link, err
}

// LinkByIndex finds a link by index and returns a pointer to the object.
func LinkByIndex(index int) (Link, error) {
	return pkgHandle.LinkByIndex(index)
}

// LinkByIndex finds a link by index and returns a pointer to the object.
func (h *Handle) LinkByIndex(index int) (Link, error) {
	req := h.newNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(index)
	req.AddData(msg)
	attr := nl.NewRtAttr(unix.IFLA_EXT_MASK, nl.Uint32Attr(nl.RTEXT_FILTER_VF))
	req.AddData(attr)

	return execGetLink(req)
}

func execGetLink(req *nl.NetlinkRequest) (Link, error) {
	msgs, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok {
			if errno == unix.ENODEV {
				return nil, LinkNotFoundError{fmt.Errorf("Link not found")}
			}
		}
		return nil, err
	}

	switch {
	case len(msgs) == 0:
		return nil, LinkNotFoundError{fmt.Errorf("Link not found")}

	case len(msgs) == 1:
		return LinkDeserialize(nil, msgs[0])

	default:
		return nil, fmt.Errorf("More than one link found")
	}
}

// LinkDeserialize deserializes a raw message received from netlink into
// a link object.
func LinkDeserialize(hdr *unix.NlMsghdr, m []byte) (Link, error) {
	msg := nl.DeserializeIfInfomsg(m)

	attrs, err := nl.ParseRouteAttr(m[msg.Len():])
	if err != nil {
		return nil, err
	}

	base := NewLinkAttrs()
	base.Index = int(msg.Index)
	base.RawFlags = msg.Flags
	base.Flags = linkFlags(msg.Flags)
	base.EncapType = msg.EncapType()
	base.NetNsID = -1
	if msg.Flags&unix.IFF_ALLMULTI != 0 {
		base.Allmulti = 1
	}
	if msg.Flags&unix.IFF_MULTICAST != 0 {
		base.Multi = 1
	}

	var (
		link      Link
		stats32   *LinkStatistics32
		stats64   *LinkStatistics64
		linkType  string
		linkSlave LinkSlave
		slaveType string
	)
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case unix.IFLA_LINKINFO:
			infos, err := nl.ParseRouteAttr(attr.Value)
			if err != nil {
				return nil, err
			}
			for _, info := range infos {
				switch info.Attr.Type {
				case nl.IFLA_INFO_KIND:
					linkType = string(info.Value[:len(info.Value)-1])
					switch linkType {
					case "dummy":
						link = &Dummy{}
					case "ifb":
						link = &Ifb{}
					case "bridge":
						link = &Bridge{}
					case "vlan":
						link = &Vlan{}
					case "netkit":
						link = &Netkit{}
					case "veth":
						link = &Veth{}
					case "wireguard":
						link = &Wireguard{}
					case "vxlan":
						link = &Vxlan{}
					case "bond":
						link = &Bond{}
					case "ipvlan":
						link = &IPVlan{}
					case "ipvtap":
						link = &IPVtap{}
					case "macvlan":
						link = &Macvlan{}
					case "macvtap":
						link = &Macvtap{}
					case "geneve":
						link = &Geneve{}
					case "gretap":
						link = &Gretap{}
					case "ip6gretap":
						link = &Gretap{}
					case "ipip":
						link = &Iptun{}
					case "ip6tnl":
						link = &Ip6tnl{}
					case "sit":
						link = &Sittun{}
					case "gre":
						link = &Gretun{}
					case "ip6gre":
						link = &Gretun{}
					case "vti", "vti6":
						link = &Vti{}
					case "vrf":
						link = &Vrf{}
					case "gtp":
						link = &GTP{}
					case "xfrm":
						link = &Xfrmi{}
					case "tun":
						link = &Tuntap{}
					case "ipoib":
						link = &IPoIB{}
					case "can":
						link = &Can{}
					case "bareudp":
						link = &BareUDP{}
					default:
						link = &GenericLink{LinkType: linkType}
					}
				case nl.IFLA_INFO_DATA:
					data, err := nl.ParseRouteAttr(info.Value)
					if err != nil {
						return nil, err
					}
					switch linkType {
					case "netkit":
						parseNetkitData(link, data)
					case "vlan":
						parseVlanData(link, data)
					case "vxlan":
						parseVxlanData(link, data)
					case "bond":
						parseBondData(link, data)
					case "ipvlan":
						parseIPVlanData(link, data)
					case "ipvtap":
						parseIPVtapData(link, data)
					case "macvlan":
						parseMacvlanData(link, data)
					case "macvtap":
						parseMacvtapData(link, data)
					case "geneve":
						parseGeneveData(link, data)
					case "gretap":
						parseGretapData(link, data)
					case "ip6gretap":
						parseGretapData(link, data)
					case "ipip":
						parseIptunData(link, data)
					case "ip6tnl":
						parseIp6tnlData(link, data)
					case "sit":
						parseSittunData(link, data)
					case "gre":
						parseGretunData(link, data)
					case "ip6gre":
						parseGretunData(link, data)
					case "vti", "vti6":
						parseVtiData(link, data)
					case "vrf":
						parseVrfData(link, data)
					case "bridge":
						parseBridgeData(link, data)
					case "gtp":
						parseGTPData(link, data)
					case "xfrm":
						parseXfrmiData(link, data)
					case "tun":
						parseTuntapData(link, data)
					case "ipoib":
						parseIPoIBData(link, data)
					case "can":
						parseCanData(link, data)
					case "bareudp":
						parseBareUDPData(link, data)
					}

				case nl.IFLA_INFO_SLAVE_KIND:
					slaveType = string(info.Value[:len(info.Value)-1])
					switch slaveType {
					case "bond":
						linkSlave = &BondSlave{}
					case "vrf":
						linkSlave = &VrfSlave{}
					}

				case nl.IFLA_INFO_SLAVE_DATA:
					switch slaveType {
					case "bond":
						data, err := nl.ParseRouteAttr(info.Value)
						if err != nil {
							return nil, err
						}
						parseBondSlaveData(linkSlave, data)
					case "vrf":
						data, err := nl.ParseRouteAttr(info.Value)
						if err != nil {
							return nil, err
						}
						parseVrfSlaveData(linkSlave, data)
					}
				}
			}
		case unix.IFLA_ADDRESS:
			var nonzero bool
			for _, b := range attr.Value {
				if b != 0 {
					nonzero = true
				}
			}
			if nonzero {
				base.HardwareAddr = attr.Value[:]
			}
		case unix.IFLA_IFNAME:
			base.Name = string(attr.Value[:len(attr.Value)-1])
		case unix.IFLA_MTU:
			base.MTU = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_PROMISCUITY:
			base.Promisc = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_LINK:
			base.ParentIndex = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_MASTER:
			base.MasterIndex = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_TXQLEN:
			base.TxQLen = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_IFALIAS:
			base.Alias = string(attr.Value[:len(attr.Value)-1])
		case unix.IFLA_STATS:
			stats32 = new(LinkStatistics32)
			if err := binary.Read(bytes.NewBuffer(attr.Value[:]), nl.NativeEndian(), stats32); err != nil {
				return nil, err
			}
		case unix.IFLA_STATS64:
			stats64 = new(LinkStatistics64)
			if err := binary.Read(bytes.NewBuffer(attr.Value[:]), nl.NativeEndian(), stats64); err != nil {
				return nil, err
			}
		case unix.IFLA_XDP:
			xdp, err := parseLinkXdp(attr.Value[:])
			if err != nil {
				return nil, err
			}
			base.Xdp = xdp
		case unix.IFLA_PROTINFO | unix.NLA_F_NESTED:
			if hdr != nil && hdr.Type == unix.RTM_NEWLINK &&
				msg.Family == unix.AF_BRIDGE {
				attrs, err := nl.ParseRouteAttr(attr.Value[:])
				if err != nil {
					return nil, err
				}
				protinfo := parseProtinfo(attrs)
				base.Protinfo = &protinfo
			}
		case unix.IFLA_PROP_LIST | unix.NLA_F_NESTED:
			attrs, err := nl.ParseRouteAttr(attr.Value[:])
			if err != nil {
				return nil, err
			}

			base.AltNames = []string{}
			for _, attr := range attrs {
				if attr.Attr.Type == unix.IFLA_ALT_IFNAME {
					base.AltNames = append(base.AltNames, nl.BytesToString(attr.Value))
				}
			}
		case unix.IFLA_OPERSTATE:
			base.OperState = LinkOperState(uint8(attr.Value[0]))
		case unix.IFLA_PHYS_SWITCH_ID:
			base.PhysSwitchID = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_LINK_NETNSID:
			base.NetNsID = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_TSO_MAX_SEGS:
			base.TSOMaxSegs = native.Uint32(attr.Value[0:4])
		case unix.IFLA_TSO_MAX_SIZE:
			base.TSOMaxSize = native.Uint32(attr.Value[0:4])
		case unix.IFLA_GSO_MAX_SEGS:
			base.GSOMaxSegs = native.Uint32(attr.Value[0:4])
		case unix.IFLA_GSO_MAX_SIZE:
			base.GSOMaxSize = native.Uint32(attr.Value[0:4])
		case unix.IFLA_GRO_MAX_SIZE:
			base.GROMaxSize = native.Uint32(attr.Value[0:4])
		case unix.IFLA_GSO_IPV4_MAX_SIZE:
			base.GSOIPv4MaxSize = native.Uint32(attr.Value[0:4])
		case unix.IFLA_GRO_IPV4_MAX_SIZE:
			base.GROIPv4MaxSize = native.Uint32(attr.Value[0:4])
		case unix.IFLA_VFINFO_LIST:
			data, err := nl.ParseRouteAttr(attr.Value)
			if err != nil {
				return nil, err
			}
			vfs, err := parseVfInfoList(data)
			if err != nil {
				return nil, err
			}
			base.Vfs = vfs
		case unix.IFLA_NUM_TX_QUEUES:
			base.NumTxQueues = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_NUM_RX_QUEUES:
			base.NumRxQueues = int(native.Uint32(attr.Value[0:4]))
		case unix.IFLA_GROUP:
			base.Group = native.Uint32(attr.Value[0:4])
		case unix.IFLA_PERM_ADDRESS:
			for _, b := range attr.Value {
				if b != 0 {
					base.PermHWAddr = attr.Value[:]
					break
				}
			}
		}
	}

	if stats64 != nil {
		base.Statistics = (*LinkStatistics)(stats64)
	} else if stats32 != nil {
		base.Statistics = (*LinkStatistics)(stats32.to64())
	}

	// Links that don't have IFLA_INFO_KIND are hardware devices
	if link == nil {
		link = &Device{}
	}
	*link.Attrs() = base
	link.Attrs().Slave = linkSlave

	// If the tuntap attributes are not updated by netlink due to
	// an older driver, use sysfs
	if link != nil && linkType == "tun" {
		tuntap := link.(*Tuntap)

		if tuntap.Mode == 0 {
			ifname := tuntap.Attrs().Name
			if flags, err := readSysPropAsInt64(ifname, "tun_flags"); err == nil {

				if flags&unix.IFF_TUN != 0 {
					tuntap.Mode = unix.IFF_TUN
				} else if flags&unix.IFF_TAP != 0 {
					tuntap.Mode = unix.IFF_TAP
				}

				tuntap.NonPersist = false
				if flags&unix.IFF_PERSIST == 0 {
					tuntap.NonPersist = true
				}
			}

			// The sysfs interface for owner/group returns -1 for root user, instead of returning 0.
			// So explicitly check for negative value, before assigning the owner uid/gid.
			if owner, err := readSysPropAsInt64(ifname, "owner"); err == nil && owner > 0 {
				tuntap.Owner = uint32(owner)
			}

			if group, err := readSysPropAsInt64(ifname, "group"); err == nil && group > 0 {
				tuntap.Group = uint32(group)
			}
		}
	}

	return link, nil
}

func readSysPropAsInt64(ifname, prop string) (int64, error) {
	fname := fmt.Sprintf("/sys/class/net/%s/%s", ifname, prop)
	contents, err := ioutil.ReadFile(fname)
	if err != nil {
		return 0, err
	}

	num, err := strconv.ParseInt(strings.TrimSpace(string(contents)), 0, 64)
	if err == nil {
		return num, nil
	}

	return 0, err
}

// LinkList gets a list of link devices.
// Equivalent to: `ip link show`
func LinkList() ([]Link, error) {
	return pkgHandle.LinkList()
}

// LinkList gets a list of link devices.
// Equivalent to: `ip link show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) LinkList() ([]Link, error) {
	// NOTE(vish): This duplicates functionality in net/iface_linux.go, but we need
	//             to get the message ourselves to parse link type.
	req := h.newNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_DUMP)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	req.AddData(msg)
	attr := nl.NewRtAttr(unix.IFLA_EXT_MASK, nl.Uint32Attr(nl.RTEXT_FILTER_VF))
	req.AddData(attr)

	msgs, executeErr := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}

	var res []Link
	for _, m := range msgs {
		link, err := LinkDeserialize(nil, m)
		if err != nil {
			return nil, err
		}
		res = append(res, link)
	}

	return res, executeErr
}

// LinkUpdate is used to pass information back from LinkSubscribe()
type LinkUpdate struct {
	nl.IfInfomsg
	Header unix.NlMsghdr
	Link
}

// LinkSubscribe takes a chan down which notifications will be sent
// when links change.  Close the 'done' chan to stop subscription.
func LinkSubscribe(ch chan<- LinkUpdate, done <-chan struct{}) error {
	return linkSubscribeAt(netns.None(), netns.None(), ch, done, nil, false, 0, nil, false)
}

// LinkSubscribeAt works like LinkSubscribe plus it allows the caller
// to choose the network namespace in which to subscribe (ns).
func LinkSubscribeAt(ns netns.NsHandle, ch chan<- LinkUpdate, done <-chan struct{}) error {
	return linkSubscribeAt(ns, netns.None(), ch, done, nil, false, 0, nil, false)
}

// LinkSubscribeOptions contains a set of options to use with
// LinkSubscribeWithOptions.
type LinkSubscribeOptions struct {
	Namespace              *netns.NsHandle
	ErrorCallback          func(error)
	ListExisting           bool
	ReceiveBufferSize      int
	ReceiveBufferForceSize bool
	ReceiveTimeout         *unix.Timeval
}

// LinkSubscribeWithOptions work like LinkSubscribe but enable to
// provide additional options to modify the behavior. Currently, the
// namespace can be provided as well as an error callback.
//
// When options.ListExisting is true, options.ErrorCallback may be
// called with [ErrDumpInterrupted] to indicate that results from
// the initial dump of links may be inconsistent or incomplete.
func LinkSubscribeWithOptions(ch chan<- LinkUpdate, done <-chan struct{}, options LinkSubscribeOptions) error {
	if options.Namespace == nil {
		none := netns.None()
		options.Namespace = &none
	}
	return linkSubscribeAt(*options.Namespace, netns.None(), ch, done, options.ErrorCallback, options.ListExisting,
		options.ReceiveBufferSize, options.ReceiveTimeout, options.ReceiveBufferForceSize)
}

func linkSubscribeAt(newNs, curNs netns.NsHandle, ch chan<- LinkUpdate, done <-chan struct{}, cberr func(error), listExisting bool,
	rcvbuf int, rcvTimeout *unix.Timeval, rcvbufForce bool) error {
	s, err := nl.SubscribeAt(newNs, curNs, unix.NETLINK_ROUTE, unix.RTNLGRP_LINK)
	if err != nil {
		return err
	}
	if rcvTimeout != nil {
		if err := s.SetReceiveTimeout(rcvTimeout); err != nil {
			return err
		}
	}
	if rcvbuf != 0 {
		err = s.SetReceiveBufferSize(rcvbuf, rcvbufForce)
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
		req := pkgHandle.newNetlinkRequest(unix.RTM_GETLINK,
			unix.NLM_F_DUMP)
		msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
		req.AddData(msg)
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
				if m.Header.Flags&unix.NLM_F_DUMP_INTR != 0 && cberr != nil {
					cberr(ErrDumpInterrupted)
				}
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
				ifmsg := nl.DeserializeIfInfomsg(m.Data)
				header := unix.NlMsghdr(m.Header)
				link, err := LinkDeserialize(&header, m.Data)
				if err != nil {
					if cberr != nil {
						cberr(err)
					}
					continue
				}
				ch <- LinkUpdate{IfInfomsg: *ifmsg, Header: header, Link: link}
			}
		}
	}()

	return nil
}

func LinkSetHairpin(link Link, mode bool) error {
	return pkgHandle.LinkSetHairpin(link, mode)
}

func (h *Handle) LinkSetHairpin(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_MODE)
}

func LinkSetGuard(link Link, mode bool) error {
	return pkgHandle.LinkSetGuard(link, mode)
}

func (h *Handle) LinkSetGuard(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_GUARD)
}

// LinkSetBRSlaveGroupFwdMask set the group_fwd_mask of a bridge slave interface
func LinkSetBRSlaveGroupFwdMask(link Link, mask uint16) error {
	return pkgHandle.LinkSetBRSlaveGroupFwdMask(link, mask)
}

// LinkSetBRSlaveGroupFwdMask set the group_fwd_mask of a bridge slave interface
func (h *Handle) LinkSetBRSlaveGroupFwdMask(link Link, mask uint16) error {
	return h.setProtinfoAttrRawVal(link, nl.Uint16Attr(mask), nl.IFLA_BRPORT_GROUP_FWD_MASK)
}

func LinkSetFastLeave(link Link, mode bool) error {
	return pkgHandle.LinkSetFastLeave(link, mode)
}

func (h *Handle) LinkSetFastLeave(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_FAST_LEAVE)
}

func LinkSetLearning(link Link, mode bool) error {
	return pkgHandle.LinkSetLearning(link, mode)
}

func (h *Handle) LinkSetLearning(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_LEARNING)
}

func LinkSetRootBlock(link Link, mode bool) error {
	return pkgHandle.LinkSetRootBlock(link, mode)
}

func (h *Handle) LinkSetRootBlock(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_PROTECT)
}

func LinkSetFlood(link Link, mode bool) error {
	return pkgHandle.LinkSetFlood(link, mode)
}

func (h *Handle) LinkSetFlood(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_UNICAST_FLOOD)
}

func LinkSetIsolated(link Link, mode bool) error {
	return pkgHandle.LinkSetIsolated(link, mode)
}

func (h *Handle) LinkSetIsolated(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_ISOLATED)
}

func LinkSetBrProxyArp(link Link, mode bool) error {
	return pkgHandle.LinkSetBrProxyArp(link, mode)
}

func (h *Handle) LinkSetBrProxyArp(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_PROXYARP)
}

func LinkSetBrProxyArpWiFi(link Link, mode bool) error {
	return pkgHandle.LinkSetBrProxyArpWiFi(link, mode)
}

func (h *Handle) LinkSetBrProxyArpWiFi(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_PROXYARP_WIFI)
}

func LinkSetBrNeighSuppress(link Link, mode bool) error {
	return pkgHandle.LinkSetBrNeighSuppress(link, mode)
}

func (h *Handle) LinkSetBrNeighSuppress(link Link, mode bool) error {
	return h.setProtinfoAttr(link, mode, nl.IFLA_BRPORT_NEIGH_SUPPRESS)
}

func (h *Handle) setProtinfoAttrRawVal(link Link, val []byte, attr int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_BRIDGE)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	br := nl.NewRtAttr(unix.IFLA_PROTINFO|unix.NLA_F_NESTED, nil)
	br.AddRtAttr(attr, val)
	req.AddData(br)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return err
	}
	return nil
}
func (h *Handle) setProtinfoAttr(link Link, mode bool, attr int) error {
	return h.setProtinfoAttrRawVal(link, boolToByte(mode), attr)
}

// LinkSetTxQLen sets the transaction queue length for the link.
// Equivalent to: `ip link set $link txqlen $qlen`
func LinkSetTxQLen(link Link, qlen int) error {
	return pkgHandle.LinkSetTxQLen(link, qlen)
}

// LinkSetTxQLen sets the transaction queue length for the link.
// Equivalent to: `ip link set $link txqlen $qlen`
func (h *Handle) LinkSetTxQLen(link Link, qlen int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(qlen))

	data := nl.NewRtAttr(unix.IFLA_TXQLEN, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetGroup sets the link group id which can be used to perform mass actions
// with iproute2 as well use it as a reference in nft filters.
// Equivalent to: `ip link set $link group $id`
func LinkSetGroup(link Link, group int) error {
	return pkgHandle.LinkSetGroup(link, group)
}

// LinkSetGroup sets the link group id which can be used to perform mass actions
// with iproute2 as well use it as a reference in nft filters.
// Equivalent to: `ip link set $link group $id`
func (h *Handle) LinkSetGroup(link Link, group int) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	b := make([]byte, 4)
	native.PutUint32(b, uint32(group))

	data := nl.NewRtAttr(unix.IFLA_GROUP, b)
	req.AddData(data)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func addNetkitAttrs(nk *Netkit, linkInfo *nl.RtAttr, flag int) error {
	if nk.peerLinkAttrs.HardwareAddr != nil || nk.HardwareAddr != nil {
		return fmt.Errorf("netkit doesn't support setting Ethernet")
	}

	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	// Kernel will return error if trying to change the mode of an existing netkit device
	data.AddRtAttr(nl.IFLA_NETKIT_MODE, nl.Uint32Attr(uint32(nk.Mode)))
	data.AddRtAttr(nl.IFLA_NETKIT_POLICY, nl.Uint32Attr(uint32(nk.Policy)))
	data.AddRtAttr(nl.IFLA_NETKIT_PEER_POLICY, nl.Uint32Attr(uint32(nk.PeerPolicy)))
	data.AddRtAttr(nl.IFLA_NETKIT_SCRUB, nl.Uint32Attr(uint32(nk.Scrub)))
	data.AddRtAttr(nl.IFLA_NETKIT_PEER_SCRUB, nl.Uint32Attr(uint32(nk.PeerScrub)))

	if (flag & unix.NLM_F_EXCL) == 0 {
		// Modifying peer link attributes will not take effect
		return nil
	}

	peer := data.AddRtAttr(nl.IFLA_NETKIT_PEER_INFO, nil)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	if nk.peerLinkAttrs.Flags&net.FlagUp != 0 {
		msg.Change = unix.IFF_UP
		msg.Flags = unix.IFF_UP
	}
	if nk.peerLinkAttrs.Index != 0 {
		msg.Index = int32(nk.peerLinkAttrs.Index)
	}
	peer.AddChild(msg)
	if nk.peerLinkAttrs.Name != "" {
		peer.AddRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(nk.peerLinkAttrs.Name))
	}
	if nk.peerLinkAttrs.MTU > 0 {
		peer.AddRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(nk.peerLinkAttrs.MTU)))
	}
	if nk.peerLinkAttrs.GSOMaxSegs > 0 {
		peer.AddRtAttr(unix.IFLA_GSO_MAX_SEGS, nl.Uint32Attr(nk.peerLinkAttrs.GSOMaxSegs))
	}
	if nk.peerLinkAttrs.GSOMaxSize > 0 {
		peer.AddRtAttr(unix.IFLA_GSO_MAX_SIZE, nl.Uint32Attr(nk.peerLinkAttrs.GSOMaxSize))
	}
	if nk.peerLinkAttrs.GSOIPv4MaxSize > 0 {
		peer.AddRtAttr(unix.IFLA_GSO_IPV4_MAX_SIZE, nl.Uint32Attr(nk.peerLinkAttrs.GSOIPv4MaxSize))
	}
	if nk.peerLinkAttrs.GROIPv4MaxSize > 0 {
		peer.AddRtAttr(unix.IFLA_GRO_IPV4_MAX_SIZE, nl.Uint32Attr(nk.peerLinkAttrs.GROIPv4MaxSize))
	}
	if nk.peerLinkAttrs.Namespace != nil {
		switch ns := nk.peerLinkAttrs.Namespace.(type) {
		case NsPid:
			peer.AddRtAttr(unix.IFLA_NET_NS_PID, nl.Uint32Attr(uint32(ns)))
		case NsFd:
			peer.AddRtAttr(unix.IFLA_NET_NS_FD, nl.Uint32Attr(uint32(ns)))
		}
	}
	return nil
}

func parseNetkitData(link Link, data []syscall.NetlinkRouteAttr) {
	netkit := link.(*Netkit)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_NETKIT_PRIMARY:
			isPrimary := datum.Value[0:1][0]
			if isPrimary != 0 {
				netkit.isPrimary = true
			}
		case nl.IFLA_NETKIT_MODE:
			netkit.Mode = NetkitMode(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_NETKIT_POLICY:
			netkit.Policy = NetkitPolicy(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_NETKIT_PEER_POLICY:
			netkit.PeerPolicy = NetkitPolicy(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_NETKIT_SCRUB:
			netkit.supportsScrub = true
			netkit.Scrub = NetkitScrub(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_NETKIT_PEER_SCRUB:
			netkit.supportsScrub = true
			netkit.PeerScrub = NetkitScrub(native.Uint32(datum.Value[0:4]))
		}
	}
}

func parseVlanData(link Link, data []syscall.NetlinkRouteAttr) {
	vlan := link.(*Vlan)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_VLAN_ID:
			vlan.VlanId = int(native.Uint16(datum.Value[0:2]))
		case nl.IFLA_VLAN_PROTOCOL:
			vlan.VlanProtocol = VlanProtocol(int(ntohs(datum.Value[0:2])))
		}
	}
}

func parseVxlanData(link Link, data []syscall.NetlinkRouteAttr) {
	vxlan := link.(*Vxlan)
	for _, datum := range data {
		// NOTE(vish): Apparently some messages can be sent with no value.
		//             We special case GBP here to not change existing
		//             functionality. It appears that GBP sends a datum.Value
		//             of null.
		if len(datum.Value) == 0 && datum.Attr.Type != nl.IFLA_VXLAN_GBP {
			continue
		}
		switch datum.Attr.Type {
		case nl.IFLA_VXLAN_ID:
			vxlan.VxlanId = int(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_VXLAN_LINK:
			vxlan.VtepDevIndex = int(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_VXLAN_LOCAL:
			vxlan.SrcAddr = net.IP(datum.Value[0:4])
		case nl.IFLA_VXLAN_LOCAL6:
			vxlan.SrcAddr = net.IP(datum.Value[0:16])
		case nl.IFLA_VXLAN_GROUP:
			vxlan.Group = net.IP(datum.Value[0:4])
		case nl.IFLA_VXLAN_GROUP6:
			vxlan.Group = net.IP(datum.Value[0:16])
		case nl.IFLA_VXLAN_TTL:
			vxlan.TTL = int(datum.Value[0])
		case nl.IFLA_VXLAN_TOS:
			vxlan.TOS = int(datum.Value[0])
		case nl.IFLA_VXLAN_LEARNING:
			vxlan.Learning = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_PROXY:
			vxlan.Proxy = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_RSC:
			vxlan.RSC = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_L2MISS:
			vxlan.L2miss = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_L3MISS:
			vxlan.L3miss = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_UDP_CSUM:
			vxlan.UDPCSum = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_UDP_ZERO_CSUM6_TX:
			vxlan.UDP6ZeroCSumTx = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_UDP_ZERO_CSUM6_RX:
			vxlan.UDP6ZeroCSumRx = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_GBP:
			vxlan.GBP = true
		case nl.IFLA_VXLAN_FLOWBASED:
			vxlan.FlowBased = int8(datum.Value[0]) != 0
		case nl.IFLA_VXLAN_AGEING:
			vxlan.Age = int(native.Uint32(datum.Value[0:4]))
			vxlan.NoAge = vxlan.Age == 0
		case nl.IFLA_VXLAN_LIMIT:
			vxlan.Limit = int(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_VXLAN_PORT:
			vxlan.Port = int(ntohs(datum.Value[0:2]))
		case nl.IFLA_VXLAN_PORT_RANGE:
			buf := bytes.NewBuffer(datum.Value[0:4])
			var pr vxlanPortRange
			if binary.Read(buf, binary.BigEndian, &pr) != nil {
				vxlan.PortLow = int(pr.Lo)
				vxlan.PortHigh = int(pr.Hi)
			}
		}
	}
}

func parseBondData(link Link, data []syscall.NetlinkRouteAttr) {
	bond := link.(*Bond)
	for i := range data {
		switch data[i].Attr.Type {
		case nl.IFLA_BOND_MODE:
			bond.Mode = BondMode(data[i].Value[0])
		case nl.IFLA_BOND_ACTIVE_SLAVE:
			bond.ActiveSlave = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_MIIMON:
			bond.Miimon = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_UPDELAY:
			bond.UpDelay = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_DOWNDELAY:
			bond.DownDelay = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_USE_CARRIER:
			bond.UseCarrier = int(data[i].Value[0])
		case nl.IFLA_BOND_ARP_INTERVAL:
			bond.ArpInterval = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_ARP_IP_TARGET:
			bond.ArpIpTargets = parseBondArpIpTargets(data[i].Value)
		case nl.IFLA_BOND_ARP_VALIDATE:
			bond.ArpValidate = BondArpValidate(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_ARP_ALL_TARGETS:
			bond.ArpAllTargets = BondArpAllTargets(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_PRIMARY:
			bond.Primary = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_PRIMARY_RESELECT:
			bond.PrimaryReselect = BondPrimaryReselect(data[i].Value[0])
		case nl.IFLA_BOND_FAIL_OVER_MAC:
			bond.FailOverMac = BondFailOverMac(data[i].Value[0])
		case nl.IFLA_BOND_XMIT_HASH_POLICY:
			bond.XmitHashPolicy = BondXmitHashPolicy(data[i].Value[0])
		case nl.IFLA_BOND_RESEND_IGMP:
			bond.ResendIgmp = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_NUM_PEER_NOTIF:
			bond.NumPeerNotif = int(data[i].Value[0])
		case nl.IFLA_BOND_ALL_SLAVES_ACTIVE:
			bond.AllSlavesActive = int(data[i].Value[0])
		case nl.IFLA_BOND_MIN_LINKS:
			bond.MinLinks = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_LP_INTERVAL:
			bond.LpInterval = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_PACKETS_PER_SLAVE:
			bond.PacketsPerSlave = int(native.Uint32(data[i].Value[0:4]))
		case nl.IFLA_BOND_AD_LACP_RATE:
			bond.LacpRate = BondLacpRate(data[i].Value[0])
		case nl.IFLA_BOND_AD_SELECT:
			bond.AdSelect = BondAdSelect(data[i].Value[0])
		case nl.IFLA_BOND_AD_INFO:
			// TODO: implement
		case nl.IFLA_BOND_AD_ACTOR_SYS_PRIO:
			bond.AdActorSysPrio = int(native.Uint16(data[i].Value[0:2]))
		case nl.IFLA_BOND_AD_USER_PORT_KEY:
			bond.AdUserPortKey = int(native.Uint16(data[i].Value[0:2]))
		case nl.IFLA_BOND_AD_ACTOR_SYSTEM:
			bond.AdActorSystem = net.HardwareAddr(data[i].Value[0:6])
		case nl.IFLA_BOND_TLB_DYNAMIC_LB:
			bond.TlbDynamicLb = int(data[i].Value[0])
		}
	}
}

func parseBondArpIpTargets(value []byte) []net.IP {
	data, err := nl.ParseRouteAttr(value)
	if err != nil {
		return nil
	}

	targets := []net.IP{}
	for i := range data {
		target := net.IP(data[i].Value)
		if ip := target.To4(); ip != nil {
			targets = append(targets, ip)
			continue
		}
		if ip := target.To16(); ip != nil {
			targets = append(targets, ip)
		}
	}

	return targets
}

func addBondSlaveAttrs(bondSlave *BondSlave, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_SLAVE_DATA, nil)

	data.AddRtAttr(nl.IFLA_BOND_SLAVE_STATE, nl.Uint8Attr(uint8(bondSlave.State)))
	data.AddRtAttr(nl.IFLA_BOND_SLAVE_MII_STATUS, nl.Uint8Attr(uint8(bondSlave.MiiStatus)))
	data.AddRtAttr(nl.IFLA_BOND_SLAVE_LINK_FAILURE_COUNT, nl.Uint32Attr(bondSlave.LinkFailureCount))
	data.AddRtAttr(nl.IFLA_BOND_SLAVE_QUEUE_ID, nl.Uint16Attr(bondSlave.QueueId))
	data.AddRtAttr(nl.IFLA_BOND_SLAVE_AD_AGGREGATOR_ID, nl.Uint16Attr(bondSlave.AggregatorId))
	data.AddRtAttr(nl.IFLA_BOND_SLAVE_AD_ACTOR_OPER_PORT_STATE, nl.Uint8Attr(bondSlave.AdActorOperPortState))
	data.AddRtAttr(nl.IFLA_BOND_SLAVE_AD_PARTNER_OPER_PORT_STATE, nl.Uint16Attr(bondSlave.AdPartnerOperPortState))

	if mac := bondSlave.PermHardwareAddr; mac != nil {
		data.AddRtAttr(nl.IFLA_BOND_SLAVE_PERM_HWADDR, []byte(mac))
	}
}

func parseBondSlaveData(slave LinkSlave, data []syscall.NetlinkRouteAttr) {
	bondSlave := slave.(*BondSlave)
	for i := range data {
		switch data[i].Attr.Type {
		case nl.IFLA_BOND_SLAVE_STATE:
			bondSlave.State = BondSlaveState(data[i].Value[0])
		case nl.IFLA_BOND_SLAVE_MII_STATUS:
			bondSlave.MiiStatus = BondSlaveMiiStatus(data[i].Value[0])
		case nl.IFLA_BOND_SLAVE_LINK_FAILURE_COUNT:
			bondSlave.LinkFailureCount = native.Uint32(data[i].Value[0:4])
		case nl.IFLA_BOND_SLAVE_PERM_HWADDR:
			bondSlave.PermHardwareAddr = net.HardwareAddr(data[i].Value[0:6])
		case nl.IFLA_BOND_SLAVE_QUEUE_ID:
			bondSlave.QueueId = native.Uint16(data[i].Value[0:2])
		case nl.IFLA_BOND_SLAVE_AD_AGGREGATOR_ID:
			bondSlave.AggregatorId = native.Uint16(data[i].Value[0:2])
		case nl.IFLA_BOND_SLAVE_AD_ACTOR_OPER_PORT_STATE:
			bondSlave.AdActorOperPortState = uint8(data[i].Value[0])
		case nl.IFLA_BOND_SLAVE_AD_PARTNER_OPER_PORT_STATE:
			bondSlave.AdPartnerOperPortState = native.Uint16(data[i].Value[0:2])
		}
	}
}

func parseVrfSlaveData(slave LinkSlave, data []syscall.NetlinkRouteAttr) {
	vrfSlave := slave.(*VrfSlave)
	for i := range data {
		switch data[i].Attr.Type {
		case nl.IFLA_BOND_SLAVE_STATE:
			vrfSlave.Table = native.Uint32(data[i].Value[0:4])
		}
	}
}

func parseIPVlanData(link Link, data []syscall.NetlinkRouteAttr) {
	ipv := link.(*IPVlan)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_IPVLAN_MODE:
			ipv.Mode = IPVlanMode(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_IPVLAN_FLAG:
			ipv.Flag = IPVlanFlag(native.Uint32(datum.Value[0:4]))
		}
	}
}

func parseIPVtapData(link Link, data []syscall.NetlinkRouteAttr) {
	ipv := link.(*IPVtap)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_IPVLAN_MODE:
			ipv.Mode = IPVlanMode(native.Uint32(datum.Value[0:4]))
		case nl.IFLA_IPVLAN_FLAG:
			ipv.Flag = IPVlanFlag(native.Uint32(datum.Value[0:4]))
		}
	}
}

func addMacvtapAttrs(macvtap *Macvtap, linkInfo *nl.RtAttr) {
	addMacvlanAttrs(&macvtap.Macvlan, linkInfo)
}

func parseMacvtapData(link Link, data []syscall.NetlinkRouteAttr) {
	macv := link.(*Macvtap)
	parseMacvlanData(&macv.Macvlan, data)
}

func addMacvlanAttrs(macvlan *Macvlan, linkInfo *nl.RtAttr) {
	var data *nl.RtAttr

	if macvlan.Mode != MACVLAN_MODE_DEFAULT || macvlan.BCQueueLen > 0 {
		data = linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	}

	if macvlan.Mode != MACVLAN_MODE_DEFAULT {
		data.AddRtAttr(nl.IFLA_MACVLAN_MODE, nl.Uint32Attr(macvlanModes[macvlan.Mode]))
	}
	if macvlan.BCQueueLen > 0 {
		data.AddRtAttr(nl.IFLA_MACVLAN_BC_QUEUE_LEN, nl.Uint32Attr(macvlan.BCQueueLen))
	}
}

func parseMacvlanData(link Link, data []syscall.NetlinkRouteAttr) {
	macv := link.(*Macvlan)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_MACVLAN_MODE:
			switch native.Uint32(datum.Value[0:4]) {
			case nl.MACVLAN_MODE_PRIVATE:
				macv.Mode = MACVLAN_MODE_PRIVATE
			case nl.MACVLAN_MODE_VEPA:
				macv.Mode = MACVLAN_MODE_VEPA
			case nl.MACVLAN_MODE_BRIDGE:
				macv.Mode = MACVLAN_MODE_BRIDGE
			case nl.MACVLAN_MODE_PASSTHRU:
				macv.Mode = MACVLAN_MODE_PASSTHRU
			case nl.MACVLAN_MODE_SOURCE:
				macv.Mode = MACVLAN_MODE_SOURCE
			}
		case nl.IFLA_MACVLAN_MACADDR_COUNT:
			macv.MACAddrs = make([]net.HardwareAddr, 0, int(native.Uint32(datum.Value[0:4])))
		case nl.IFLA_MACVLAN_MACADDR_DATA:
			macs, err := nl.ParseRouteAttr(datum.Value[:])
			if err != nil {
				panic(fmt.Sprintf("failed to ParseRouteAttr for IFLA_MACVLAN_MACADDR_DATA: %v", err))
			}
			for _, macDatum := range macs {
				macv.MACAddrs = append(macv.MACAddrs, net.HardwareAddr(macDatum.Value[0:6]))
			}
		case nl.IFLA_MACVLAN_BC_QUEUE_LEN:
			macv.BCQueueLen = native.Uint32(datum.Value[0:4])
		case nl.IFLA_MACVLAN_BC_QUEUE_LEN_USED:
			macv.UsedBCQueueLen = native.Uint32(datum.Value[0:4])
		}
	}
}

func linkFlags(rawFlags uint32) net.Flags {
	var f net.Flags
	if rawFlags&unix.IFF_UP != 0 {
		f |= net.FlagUp
	}
	if rawFlags&unix.IFF_BROADCAST != 0 {
		f |= net.FlagBroadcast
	}
	if rawFlags&unix.IFF_LOOPBACK != 0 {
		f |= net.FlagLoopback
	}
	if rawFlags&unix.IFF_POINTOPOINT != 0 {
		f |= net.FlagPointToPoint
	}
	if rawFlags&unix.IFF_MULTICAST != 0 {
		f |= net.FlagMulticast
	}
	if rawFlags&unix.IFF_RUNNING != 0 {
		f |= net.FlagRunning
	}
	return f
}

func addGeneveAttrs(geneve *Geneve, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	if geneve.InnerProtoInherit {
		data.AddRtAttr(nl.IFLA_GENEVE_INNER_PROTO_INHERIT, []byte{})
	}

	if geneve.FlowBased {
		geneve.ID = 0
		data.AddRtAttr(nl.IFLA_GENEVE_COLLECT_METADATA, []byte{})
	}

	if ip := geneve.Remote; ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			data.AddRtAttr(nl.IFLA_GENEVE_REMOTE, ip.To4())
		} else {
			data.AddRtAttr(nl.IFLA_GENEVE_REMOTE6, []byte(ip))
		}
	}

	if geneve.ID != 0 {
		data.AddRtAttr(nl.IFLA_GENEVE_ID, nl.Uint32Attr(geneve.ID))
	}

	if geneve.Dport != 0 {
		data.AddRtAttr(nl.IFLA_GENEVE_PORT, htons(geneve.Dport))
	}

	if geneve.Ttl != 0 {
		data.AddRtAttr(nl.IFLA_GENEVE_TTL, nl.Uint8Attr(geneve.Ttl))
	}

	if geneve.Tos != 0 {
		data.AddRtAttr(nl.IFLA_GENEVE_TOS, nl.Uint8Attr(geneve.Tos))
	}

	data.AddRtAttr(nl.IFLA_GENEVE_DF, nl.Uint8Attr(uint8(geneve.Df)))
}

func parseGeneveData(link Link, data []syscall.NetlinkRouteAttr) {
	geneve := link.(*Geneve)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_GENEVE_ID:
			geneve.ID = native.Uint32(datum.Value[0:4])
		case nl.IFLA_GENEVE_REMOTE, nl.IFLA_GENEVE_REMOTE6:
			geneve.Remote = datum.Value
		case nl.IFLA_GENEVE_PORT:
			geneve.Dport = ntohs(datum.Value[0:2])
		case nl.IFLA_GENEVE_TTL:
			geneve.Ttl = uint8(datum.Value[0])
		case nl.IFLA_GENEVE_TOS:
			geneve.Tos = uint8(datum.Value[0])
		case nl.IFLA_GENEVE_COLLECT_METADATA:
			geneve.FlowBased = true
		case nl.IFLA_GENEVE_INNER_PROTO_INHERIT:
			geneve.InnerProtoInherit = true
		}
	}
}

func addGretapAttrs(gretap *Gretap, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	if gretap.FlowBased {
		// In flow based mode, no other attributes need to be configured
		data.AddRtAttr(nl.IFLA_GRE_COLLECT_METADATA, []byte{})
		return
	}

	if ip := gretap.Local; ip != nil {
		if ip.To4() != nil {
			ip = ip.To4()
		}
		data.AddRtAttr(nl.IFLA_GRE_LOCAL, []byte(ip))
	}

	if ip := gretap.Remote; ip != nil {
		if ip.To4() != nil {
			ip = ip.To4()
		}
		data.AddRtAttr(nl.IFLA_GRE_REMOTE, []byte(ip))
	}

	if gretap.IKey != 0 {
		data.AddRtAttr(nl.IFLA_GRE_IKEY, htonl(gretap.IKey))
		gretap.IFlags |= uint16(nl.GRE_KEY)
	}

	if gretap.OKey != 0 {
		data.AddRtAttr(nl.IFLA_GRE_OKEY, htonl(gretap.OKey))
		gretap.OFlags |= uint16(nl.GRE_KEY)
	}

	data.AddRtAttr(nl.IFLA_GRE_IFLAGS, htons(gretap.IFlags))
	data.AddRtAttr(nl.IFLA_GRE_OFLAGS, htons(gretap.OFlags))

	if gretap.Link != 0 {
		data.AddRtAttr(nl.IFLA_GRE_LINK, nl.Uint32Attr(gretap.Link))
	}

	data.AddRtAttr(nl.IFLA_GRE_PMTUDISC, nl.Uint8Attr(gretap.PMtuDisc))
	data.AddRtAttr(nl.IFLA_GRE_TTL, nl.Uint8Attr(gretap.Ttl))
	data.AddRtAttr(nl.IFLA_GRE_TOS, nl.Uint8Attr(gretap.Tos))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_TYPE, nl.Uint16Attr(gretap.EncapType))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_FLAGS, nl.Uint16Attr(gretap.EncapFlags))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_SPORT, htons(gretap.EncapSport))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_DPORT, htons(gretap.EncapDport))
}

func parseGretapData(link Link, data []syscall.NetlinkRouteAttr) {
	gre := link.(*Gretap)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_GRE_OKEY:
			gre.IKey = ntohl(datum.Value[0:4])
		case nl.IFLA_GRE_IKEY:
			gre.OKey = ntohl(datum.Value[0:4])
		case nl.IFLA_GRE_LOCAL:
			gre.Local = net.IP(datum.Value)
		case nl.IFLA_GRE_REMOTE:
			gre.Remote = net.IP(datum.Value)
		case nl.IFLA_GRE_ENCAP_SPORT:
			gre.EncapSport = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_ENCAP_DPORT:
			gre.EncapDport = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_IFLAGS:
			gre.IFlags = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_OFLAGS:
			gre.OFlags = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_TTL:
			gre.Ttl = uint8(datum.Value[0])
		case nl.IFLA_GRE_TOS:
			gre.Tos = uint8(datum.Value[0])
		case nl.IFLA_GRE_PMTUDISC:
			gre.PMtuDisc = uint8(datum.Value[0])
		case nl.IFLA_GRE_ENCAP_TYPE:
			gre.EncapType = native.Uint16(datum.Value[0:2])
		case nl.IFLA_GRE_ENCAP_FLAGS:
			gre.EncapFlags = native.Uint16(datum.Value[0:2])
		case nl.IFLA_GRE_COLLECT_METADATA:
			gre.FlowBased = true
		}
	}
}

func addGretunAttrs(gre *Gretun, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	if gre.FlowBased {
		// In flow based mode, no other attributes need to be configured
		data.AddRtAttr(nl.IFLA_GRE_COLLECT_METADATA, []byte{})
		return
	}

	if ip := gre.Local; ip != nil {
		if ip.To4() != nil {
			ip = ip.To4()
		}
		data.AddRtAttr(nl.IFLA_GRE_LOCAL, []byte(ip))
	}

	if ip := gre.Remote; ip != nil {
		if ip.To4() != nil {
			ip = ip.To4()
		}
		data.AddRtAttr(nl.IFLA_GRE_REMOTE, []byte(ip))
	}

	if gre.IKey != 0 {
		data.AddRtAttr(nl.IFLA_GRE_IKEY, htonl(gre.IKey))
		gre.IFlags |= uint16(nl.GRE_KEY)
	}

	if gre.OKey != 0 {
		data.AddRtAttr(nl.IFLA_GRE_OKEY, htonl(gre.OKey))
		gre.OFlags |= uint16(nl.GRE_KEY)
	}

	data.AddRtAttr(nl.IFLA_GRE_IFLAGS, htons(gre.IFlags))
	data.AddRtAttr(nl.IFLA_GRE_OFLAGS, htons(gre.OFlags))

	if gre.Link != 0 {
		data.AddRtAttr(nl.IFLA_GRE_LINK, nl.Uint32Attr(gre.Link))
	}

	data.AddRtAttr(nl.IFLA_GRE_PMTUDISC, nl.Uint8Attr(gre.PMtuDisc))
	data.AddRtAttr(nl.IFLA_GRE_TTL, nl.Uint8Attr(gre.Ttl))
	data.AddRtAttr(nl.IFLA_GRE_TOS, nl.Uint8Attr(gre.Tos))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_TYPE, nl.Uint16Attr(gre.EncapType))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_FLAGS, nl.Uint16Attr(gre.EncapFlags))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_SPORT, htons(gre.EncapSport))
	data.AddRtAttr(nl.IFLA_GRE_ENCAP_DPORT, htons(gre.EncapDport))
}

func parseGretunData(link Link, data []syscall.NetlinkRouteAttr) {
	gre := link.(*Gretun)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_GRE_IKEY:
			gre.IKey = ntohl(datum.Value[0:4])
		case nl.IFLA_GRE_OKEY:
			gre.OKey = ntohl(datum.Value[0:4])
		case nl.IFLA_GRE_LOCAL:
			gre.Local = net.IP(datum.Value)
		case nl.IFLA_GRE_REMOTE:
			gre.Remote = net.IP(datum.Value)
		case nl.IFLA_GRE_IFLAGS:
			gre.IFlags = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_OFLAGS:
			gre.OFlags = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_TTL:
			gre.Ttl = uint8(datum.Value[0])
		case nl.IFLA_GRE_TOS:
			gre.Tos = uint8(datum.Value[0])
		case nl.IFLA_GRE_PMTUDISC:
			gre.PMtuDisc = uint8(datum.Value[0])
		case nl.IFLA_GRE_ENCAP_TYPE:
			gre.EncapType = native.Uint16(datum.Value[0:2])
		case nl.IFLA_GRE_ENCAP_FLAGS:
			gre.EncapFlags = native.Uint16(datum.Value[0:2])
		case nl.IFLA_GRE_ENCAP_SPORT:
			gre.EncapSport = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_ENCAP_DPORT:
			gre.EncapDport = ntohs(datum.Value[0:2])
		case nl.IFLA_GRE_COLLECT_METADATA:
			gre.FlowBased = true
		}
	}
}

func addXdpAttrs(xdp *LinkXdp, req *nl.NetlinkRequest) {
	attrs := nl.NewRtAttr(unix.IFLA_XDP|unix.NLA_F_NESTED, nil)
	b := make([]byte, 4)
	native.PutUint32(b, uint32(xdp.Fd))
	attrs.AddRtAttr(nl.IFLA_XDP_FD, b)
	if xdp.Flags != 0 {
		b := make([]byte, 4)
		native.PutUint32(b, xdp.Flags)
		attrs.AddRtAttr(nl.IFLA_XDP_FLAGS, b)
	}
	req.AddData(attrs)
}

func parseLinkXdp(data []byte) (*LinkXdp, error) {
	attrs, err := nl.ParseRouteAttr(data)
	if err != nil {
		return nil, err
	}
	xdp := &LinkXdp{}
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case nl.IFLA_XDP_FD:
			xdp.Fd = int(native.Uint32(attr.Value[0:4]))
		case nl.IFLA_XDP_ATTACHED:
			xdp.AttachMode = uint32(attr.Value[0])
			xdp.Attached = xdp.AttachMode != 0
		case nl.IFLA_XDP_FLAGS:
			xdp.Flags = native.Uint32(attr.Value[0:4])
		case nl.IFLA_XDP_PROG_ID:
			xdp.ProgId = native.Uint32(attr.Value[0:4])
		}
	}
	return xdp, nil
}

func addIptunAttrs(iptun *Iptun, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	if iptun.FlowBased {
		// In flow based mode, no other attributes need to be configured
		data.AddRtAttr(nl.IFLA_IPTUN_COLLECT_METADATA, []byte{})
		return
	}

	ip := iptun.Local.To4()
	if ip != nil {
		data.AddRtAttr(nl.IFLA_IPTUN_LOCAL, []byte(ip))
	}

	ip = iptun.Remote.To4()
	if ip != nil {
		data.AddRtAttr(nl.IFLA_IPTUN_REMOTE, []byte(ip))
	}

	if iptun.Link != 0 {
		data.AddRtAttr(nl.IFLA_IPTUN_LINK, nl.Uint32Attr(iptun.Link))
	}
	data.AddRtAttr(nl.IFLA_IPTUN_PMTUDISC, nl.Uint8Attr(iptun.PMtuDisc))
	data.AddRtAttr(nl.IFLA_IPTUN_TTL, nl.Uint8Attr(iptun.Ttl))
	data.AddRtAttr(nl.IFLA_IPTUN_TOS, nl.Uint8Attr(iptun.Tos))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_TYPE, nl.Uint16Attr(iptun.EncapType))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_FLAGS, nl.Uint16Attr(iptun.EncapFlags))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_SPORT, htons(iptun.EncapSport))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_DPORT, htons(iptun.EncapDport))
	data.AddRtAttr(nl.IFLA_IPTUN_PROTO, nl.Uint8Attr(iptun.Proto))
}

func parseIptunData(link Link, data []syscall.NetlinkRouteAttr) {
	iptun := link.(*Iptun)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_IPTUN_LOCAL:
			iptun.Local = net.IP(datum.Value[0:4])
		case nl.IFLA_IPTUN_REMOTE:
			iptun.Remote = net.IP(datum.Value[0:4])
		case nl.IFLA_IPTUN_TTL:
			iptun.Ttl = uint8(datum.Value[0])
		case nl.IFLA_IPTUN_TOS:
			iptun.Tos = uint8(datum.Value[0])
		case nl.IFLA_IPTUN_PMTUDISC:
			iptun.PMtuDisc = uint8(datum.Value[0])
		case nl.IFLA_IPTUN_ENCAP_SPORT:
			iptun.EncapSport = ntohs(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_DPORT:
			iptun.EncapDport = ntohs(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_TYPE:
			iptun.EncapType = native.Uint16(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_FLAGS:
			iptun.EncapFlags = native.Uint16(datum.Value[0:2])
		case nl.IFLA_IPTUN_COLLECT_METADATA:
			iptun.FlowBased = true
		case nl.IFLA_IPTUN_PROTO:
			iptun.Proto = datum.Value[0]
		}
	}
}

func addIp6tnlAttrs(ip6tnl *Ip6tnl, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	if ip6tnl.FlowBased {
		// In flow based mode, no other attributes need to be configured
		data.AddRtAttr(nl.IFLA_IPTUN_COLLECT_METADATA, []byte{})
		return
	}

	if ip6tnl.Link != 0 {
		data.AddRtAttr(nl.IFLA_IPTUN_LINK, nl.Uint32Attr(ip6tnl.Link))
	}

	ip := ip6tnl.Local.To16()
	if ip != nil {
		data.AddRtAttr(nl.IFLA_IPTUN_LOCAL, []byte(ip))
	}

	ip = ip6tnl.Remote.To16()
	if ip != nil {
		data.AddRtAttr(nl.IFLA_IPTUN_REMOTE, []byte(ip))
	}

	data.AddRtAttr(nl.IFLA_IPTUN_TTL, nl.Uint8Attr(ip6tnl.Ttl))
	data.AddRtAttr(nl.IFLA_IPTUN_TOS, nl.Uint8Attr(ip6tnl.Tos))
	data.AddRtAttr(nl.IFLA_IPTUN_FLAGS, nl.Uint32Attr(ip6tnl.Flags))
	data.AddRtAttr(nl.IFLA_IPTUN_PROTO, nl.Uint8Attr(ip6tnl.Proto))
	data.AddRtAttr(nl.IFLA_IPTUN_FLOWINFO, nl.Uint32Attr(ip6tnl.FlowInfo))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_LIMIT, nl.Uint8Attr(ip6tnl.EncapLimit))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_TYPE, nl.Uint16Attr(ip6tnl.EncapType))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_FLAGS, nl.Uint16Attr(ip6tnl.EncapFlags))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_SPORT, htons(ip6tnl.EncapSport))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_DPORT, htons(ip6tnl.EncapDport))
}

func parseIp6tnlData(link Link, data []syscall.NetlinkRouteAttr) {
	ip6tnl := link.(*Ip6tnl)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_IPTUN_LOCAL:
			ip6tnl.Local = net.IP(datum.Value[:16])
		case nl.IFLA_IPTUN_REMOTE:
			ip6tnl.Remote = net.IP(datum.Value[:16])
		case nl.IFLA_IPTUN_TTL:
			ip6tnl.Ttl = datum.Value[0]
		case nl.IFLA_IPTUN_TOS:
			ip6tnl.Tos = datum.Value[0]
		case nl.IFLA_IPTUN_FLAGS:
			ip6tnl.Flags = native.Uint32(datum.Value[:4])
		case nl.IFLA_IPTUN_PROTO:
			ip6tnl.Proto = datum.Value[0]
		case nl.IFLA_IPTUN_FLOWINFO:
			ip6tnl.FlowInfo = native.Uint32(datum.Value[:4])
		case nl.IFLA_IPTUN_ENCAP_LIMIT:
			ip6tnl.EncapLimit = datum.Value[0]
		case nl.IFLA_IPTUN_ENCAP_TYPE:
			ip6tnl.EncapType = native.Uint16(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_FLAGS:
			ip6tnl.EncapFlags = native.Uint16(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_SPORT:
			ip6tnl.EncapSport = ntohs(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_DPORT:
			ip6tnl.EncapDport = ntohs(datum.Value[0:2])
		case nl.IFLA_IPTUN_COLLECT_METADATA:
			ip6tnl.FlowBased = true
		}
	}
}

func addSittunAttrs(sittun *Sittun, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	if sittun.Link != 0 {
		data.AddRtAttr(nl.IFLA_IPTUN_LINK, nl.Uint32Attr(sittun.Link))
	}

	ip := sittun.Local.To4()
	if ip != nil {
		data.AddRtAttr(nl.IFLA_IPTUN_LOCAL, []byte(ip))
	}

	ip = sittun.Remote.To4()
	if ip != nil {
		data.AddRtAttr(nl.IFLA_IPTUN_REMOTE, []byte(ip))
	}

	if sittun.Ttl > 0 {
		// Would otherwise fail on 3.10 kernel
		data.AddRtAttr(nl.IFLA_IPTUN_TTL, nl.Uint8Attr(sittun.Ttl))
	}

	data.AddRtAttr(nl.IFLA_IPTUN_PROTO, nl.Uint8Attr(sittun.Proto))
	data.AddRtAttr(nl.IFLA_IPTUN_TOS, nl.Uint8Attr(sittun.Tos))
	data.AddRtAttr(nl.IFLA_IPTUN_PMTUDISC, nl.Uint8Attr(sittun.PMtuDisc))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_LIMIT, nl.Uint8Attr(sittun.EncapLimit))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_TYPE, nl.Uint16Attr(sittun.EncapType))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_FLAGS, nl.Uint16Attr(sittun.EncapFlags))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_SPORT, htons(sittun.EncapSport))
	data.AddRtAttr(nl.IFLA_IPTUN_ENCAP_DPORT, htons(sittun.EncapDport))
}

func parseSittunData(link Link, data []syscall.NetlinkRouteAttr) {
	sittun := link.(*Sittun)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_IPTUN_LOCAL:
			sittun.Local = net.IP(datum.Value[0:4])
		case nl.IFLA_IPTUN_REMOTE:
			sittun.Remote = net.IP(datum.Value[0:4])
		case nl.IFLA_IPTUN_TTL:
			sittun.Ttl = datum.Value[0]
		case nl.IFLA_IPTUN_TOS:
			sittun.Tos = datum.Value[0]
		case nl.IFLA_IPTUN_PMTUDISC:
			sittun.PMtuDisc = datum.Value[0]
		case nl.IFLA_IPTUN_PROTO:
			sittun.Proto = datum.Value[0]
		case nl.IFLA_IPTUN_ENCAP_TYPE:
			sittun.EncapType = native.Uint16(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_FLAGS:
			sittun.EncapFlags = native.Uint16(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_SPORT:
			sittun.EncapSport = ntohs(datum.Value[0:2])
		case nl.IFLA_IPTUN_ENCAP_DPORT:
			sittun.EncapDport = ntohs(datum.Value[0:2])
		}
	}
}

func addVtiAttrs(vti *Vti, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	family := FAMILY_V4
	if vti.Local.To4() == nil {
		family = FAMILY_V6
	}

	var ip net.IP

	if family == FAMILY_V4 {
		ip = vti.Local.To4()
	} else {
		ip = vti.Local
	}
	if ip != nil {
		data.AddRtAttr(nl.IFLA_VTI_LOCAL, []byte(ip))
	}

	if family == FAMILY_V4 {
		ip = vti.Remote.To4()
	} else {
		ip = vti.Remote
	}
	if ip != nil {
		data.AddRtAttr(nl.IFLA_VTI_REMOTE, []byte(ip))
	}

	if vti.Link != 0 {
		data.AddRtAttr(nl.IFLA_VTI_LINK, nl.Uint32Attr(vti.Link))
	}

	data.AddRtAttr(nl.IFLA_VTI_IKEY, htonl(vti.IKey))
	data.AddRtAttr(nl.IFLA_VTI_OKEY, htonl(vti.OKey))
}

func parseVtiData(link Link, data []syscall.NetlinkRouteAttr) {
	vti := link.(*Vti)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_VTI_LOCAL:
			vti.Local = net.IP(datum.Value)
		case nl.IFLA_VTI_REMOTE:
			vti.Remote = net.IP(datum.Value)
		case nl.IFLA_VTI_IKEY:
			vti.IKey = ntohl(datum.Value[0:4])
		case nl.IFLA_VTI_OKEY:
			vti.OKey = ntohl(datum.Value[0:4])
		}
	}
}

func addVrfAttrs(vrf *Vrf, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	b := make([]byte, 4)
	native.PutUint32(b, uint32(vrf.Table))
	data.AddRtAttr(nl.IFLA_VRF_TABLE, b)
}

func parseVrfData(link Link, data []syscall.NetlinkRouteAttr) {
	vrf := link.(*Vrf)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_VRF_TABLE:
			vrf.Table = native.Uint32(datum.Value[0:4])
		}
	}
}

func addBridgeAttrs(bridge *Bridge, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	if bridge.MulticastSnooping != nil {
		data.AddRtAttr(nl.IFLA_BR_MCAST_SNOOPING, boolToByte(*bridge.MulticastSnooping))
	}
	if bridge.AgeingTime != nil {
		data.AddRtAttr(nl.IFLA_BR_AGEING_TIME, nl.Uint32Attr(*bridge.AgeingTime))
	}
	if bridge.HelloTime != nil {
		data.AddRtAttr(nl.IFLA_BR_HELLO_TIME, nl.Uint32Attr(*bridge.HelloTime))
	}
	if bridge.VlanFiltering != nil {
		data.AddRtAttr(nl.IFLA_BR_VLAN_FILTERING, boolToByte(*bridge.VlanFiltering))
	}
	if bridge.VlanDefaultPVID != nil {
		data.AddRtAttr(nl.IFLA_BR_VLAN_DEFAULT_PVID, nl.Uint16Attr(*bridge.VlanDefaultPVID))
	}
	if bridge.GroupFwdMask != nil {
		data.AddRtAttr(nl.IFLA_BR_GROUP_FWD_MASK, nl.Uint16Attr(*bridge.GroupFwdMask))
	}
}

func parseBridgeData(bridge Link, data []syscall.NetlinkRouteAttr) {
	br := bridge.(*Bridge)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_BR_AGEING_TIME:
			ageingTime := native.Uint32(datum.Value[0:4])
			br.AgeingTime = &ageingTime
		case nl.IFLA_BR_HELLO_TIME:
			helloTime := native.Uint32(datum.Value[0:4])
			br.HelloTime = &helloTime
		case nl.IFLA_BR_MCAST_SNOOPING:
			mcastSnooping := datum.Value[0] == 1
			br.MulticastSnooping = &mcastSnooping
		case nl.IFLA_BR_VLAN_FILTERING:
			vlanFiltering := datum.Value[0] == 1
			br.VlanFiltering = &vlanFiltering
		case nl.IFLA_BR_VLAN_DEFAULT_PVID:
			vlanDefaultPVID := native.Uint16(datum.Value[0:2])
			br.VlanDefaultPVID = &vlanDefaultPVID
		case nl.IFLA_BR_GROUP_FWD_MASK:
			mask := native.Uint16(datum.Value[0:2])
			br.GroupFwdMask = &mask
		}
	}
}

func addGTPAttrs(gtp *GTP, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_GTP_FD0, nl.Uint32Attr(uint32(gtp.FD0)))
	data.AddRtAttr(nl.IFLA_GTP_FD1, nl.Uint32Attr(uint32(gtp.FD1)))
	data.AddRtAttr(nl.IFLA_GTP_PDP_HASHSIZE, nl.Uint32Attr(131072))
	if gtp.Role != nl.GTP_ROLE_GGSN {
		data.AddRtAttr(nl.IFLA_GTP_ROLE, nl.Uint32Attr(uint32(gtp.Role)))
	}
}

func parseGTPData(link Link, data []syscall.NetlinkRouteAttr) {
	gtp := link.(*GTP)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_GTP_FD0:
			gtp.FD0 = int(native.Uint32(datum.Value))
		case nl.IFLA_GTP_FD1:
			gtp.FD1 = int(native.Uint32(datum.Value))
		case nl.IFLA_GTP_PDP_HASHSIZE:
			gtp.PDPHashsize = int(native.Uint32(datum.Value))
		case nl.IFLA_GTP_ROLE:
			gtp.Role = int(native.Uint32(datum.Value))
		}
	}
}

func parseVfInfoList(data []syscall.NetlinkRouteAttr) ([]VfInfo, error) {
	var vfs []VfInfo

	for i, element := range data {
		if element.Attr.Type != nl.IFLA_VF_INFO {
			return nil, fmt.Errorf("Incorrect element type in vf info list: %d", element.Attr.Type)
		}
		vfAttrs, err := nl.ParseRouteAttr(element.Value)
		if err != nil {
			return nil, err
		}

		vf, err := parseVfInfo(vfAttrs, i)
		if err != nil {
			return nil, err
		}
		vfs = append(vfs, vf)
	}
	return vfs, nil
}

func parseVfInfo(data []syscall.NetlinkRouteAttr, id int) (VfInfo, error) {
	vf := VfInfo{ID: id}
	for _, element := range data {
		switch element.Attr.Type {
		case nl.IFLA_VF_MAC:
			mac := nl.DeserializeVfMac(element.Value[:])
			vf.Mac = mac.Mac[:6]
		case nl.IFLA_VF_VLAN:
			vl := nl.DeserializeVfVlan(element.Value[:])
			vf.Vlan = int(vl.Vlan)
			vf.Qos = int(vl.Qos)
		case nl.IFLA_VF_VLAN_LIST:
			vfVlanInfoList, err := nl.DeserializeVfVlanList(element.Value[:])
			if err != nil {
				return vf, err
			}
			vf.VlanProto = int(vfVlanInfoList[0].VlanProto)
		case nl.IFLA_VF_TX_RATE:
			txr := nl.DeserializeVfTxRate(element.Value[:])
			vf.TxRate = int(txr.Rate)
		case nl.IFLA_VF_SPOOFCHK:
			sp := nl.DeserializeVfSpoofchk(element.Value[:])
			vf.Spoofchk = sp.Setting != 0
		case nl.IFLA_VF_LINK_STATE:
			ls := nl.DeserializeVfLinkState(element.Value[:])
			vf.LinkState = ls.LinkState
		case nl.IFLA_VF_RATE:
			vfr := nl.DeserializeVfRate(element.Value[:])
			vf.MaxTxRate = vfr.MaxTxRate
			vf.MinTxRate = vfr.MinTxRate
		case nl.IFLA_VF_STATS:
			vfstats := nl.DeserializeVfStats(element.Value[:])
			vf.RxPackets = vfstats.RxPackets
			vf.TxPackets = vfstats.TxPackets
			vf.RxBytes = vfstats.RxBytes
			vf.TxBytes = vfstats.TxBytes
			vf.Multicast = vfstats.Multicast
			vf.Broadcast = vfstats.Broadcast
			vf.RxDropped = vfstats.RxDropped
			vf.TxDropped = vfstats.TxDropped

		case nl.IFLA_VF_RSS_QUERY_EN:
			result := nl.DeserializeVfRssQueryEn(element.Value)
			vf.RssQuery = result.Setting

		case nl.IFLA_VF_TRUST:
			result := nl.DeserializeVfTrust(element.Value)
			vf.Trust = result.Setting
		}
	}
	return vf, nil
}

func addXfrmiAttrs(xfrmi *Xfrmi, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_XFRM_LINK, nl.Uint32Attr(uint32(xfrmi.ParentIndex)))
	if xfrmi.Ifid != 0 {
		data.AddRtAttr(nl.IFLA_XFRM_IF_ID, nl.Uint32Attr(xfrmi.Ifid))
	}
}

func parseXfrmiData(link Link, data []syscall.NetlinkRouteAttr) {
	xfrmi := link.(*Xfrmi)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_XFRM_LINK:
			xfrmi.ParentIndex = int(native.Uint32(datum.Value))
		case nl.IFLA_XFRM_IF_ID:
			xfrmi.Ifid = native.Uint32(datum.Value)
		}
	}
}

func ioctlBondSlave(cmd uintptr, link Link, master *Bond) error {
	fd, err := getSocketUDP()
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	ifreq := newIocltSlaveReq(link.Attrs().Name, master.Attrs().Name)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), cmd, uintptr(unsafe.Pointer(ifreq)))
	if errno != 0 {
		return fmt.Errorf("errno=%v", errno)
	}
	return nil
}

// LinkSetBondSlaveActive sets specified slave to ACTIVE in an `active-backup` bond link via ioctl interface.
//
//	Multiple calls keeps the status unchanged(shown in the unit test).
func LinkSetBondSlaveActive(link Link, master *Bond) error {
	err := ioctlBondSlave(unix.SIOCBONDCHANGEACTIVE, link, master)
	if err != nil {
		return fmt.Errorf("Failed to set slave %q active in %q, %v", link.Attrs().Name, master.Attrs().Name, err)
	}
	return nil
}

// LinkSetBondSlave add slave to bond link via ioctl interface.
func LinkSetBondSlave(link Link, master *Bond) error {
	err := ioctlBondSlave(unix.SIOCBONDENSLAVE, link, master)
	if err != nil {
		return fmt.Errorf("Failed to enslave %q to %q, %v", link.Attrs().Name, master.Attrs().Name, err)
	}
	return nil
}

// LinkSetBondSlave removes specified slave from bond link via ioctl interface.
func LinkDelBondSlave(link Link, master *Bond) error {
	err := ioctlBondSlave(unix.SIOCBONDRELEASE, link, master)
	if err != nil {
		return fmt.Errorf("Failed to del slave %q from %q, %v", link.Attrs().Name, master.Attrs().Name, err)
	}
	return nil
}

// LinkSetBondSlaveQueueId modify bond slave queue-id.
func (h *Handle) LinkSetBondSlaveQueueId(link Link, queueId uint16) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_SLAVE_DATA, nil)
	data.AddRtAttr(nl.IFLA_BOND_SLAVE_QUEUE_ID, nl.Uint16Attr(queueId))

	req.AddData(linkInfo)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// LinkSetBondSlaveQueueId modify bond slave queue-id.
func LinkSetBondSlaveQueueId(link Link, queueId uint16) error {
	return pkgHandle.LinkSetBondSlaveQueueId(link, queueId)
}

func vethStatsSerialize(stats ethtoolStats) ([]byte, error) {
	statsSize := int(unsafe.Sizeof(stats)) + int(stats.nStats)*int(unsafe.Sizeof(uint64(0)))
	b := make([]byte, 0, statsSize)
	buf := bytes.NewBuffer(b)
	err := binary.Write(buf, nl.NativeEndian(), stats)
	return buf.Bytes()[:statsSize], err
}

type vethEthtoolStats struct {
	Cmd    uint32
	NStats uint32
	Peer   uint64
	// Newer kernels have XDP stats in here, but we only care
	// to extract the peer ifindex here.
}

func vethStatsDeserialize(b []byte) (vethEthtoolStats, error) {
	var stats = vethEthtoolStats{}
	err := binary.Read(bytes.NewReader(b), nl.NativeEndian(), &stats)
	return stats, err
}

// VethPeerIndex get veth peer index.
func VethPeerIndex(link *Veth) (int, error) {
	fd, err := getSocketUDP()
	if err != nil {
		return -1, err
	}
	defer syscall.Close(fd)

	ifreq, sSet := newIocltStringSetReq(link.Name)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), SIOCETHTOOL, uintptr(unsafe.Pointer(ifreq)))
	if errno != 0 {
		return -1, fmt.Errorf("SIOCETHTOOL request for %q failed, errno=%v", link.Attrs().Name, errno)
	}

	stats := ethtoolStats{
		cmd:    ETHTOOL_GSTATS,
		nStats: sSet.data[0],
	}

	buffer, err := vethStatsSerialize(stats)
	if err != nil {
		return -1, err
	}

	ifreq.Data = uintptr(unsafe.Pointer(&buffer[0]))
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), SIOCETHTOOL, uintptr(unsafe.Pointer(ifreq)))
	if errno != 0 {
		return -1, fmt.Errorf("SIOCETHTOOL request for %q failed, errno=%v", link.Attrs().Name, errno)
	}

	vstats, err := vethStatsDeserialize(buffer)
	if err != nil {
		return -1, err
	}

	return int(vstats.Peer), nil
}

func parseTuntapData(link Link, data []syscall.NetlinkRouteAttr) {
	tuntap := link.(*Tuntap)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_TUN_OWNER:
			tuntap.Owner = native.Uint32(datum.Value)
		case nl.IFLA_TUN_GROUP:
			tuntap.Group = native.Uint32(datum.Value)
		case nl.IFLA_TUN_TYPE:
			tuntap.Mode = TuntapMode(uint8(datum.Value[0]))
		case nl.IFLA_TUN_PERSIST:
			tuntap.NonPersist = false
			if uint8(datum.Value[0]) == 0 {
				tuntap.NonPersist = true
			}
		}
	}
}

func parseIPoIBData(link Link, data []syscall.NetlinkRouteAttr) {
	ipoib := link.(*IPoIB)
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.IFLA_IPOIB_PKEY:
			ipoib.Pkey = uint16(native.Uint16(datum.Value))
		case nl.IFLA_IPOIB_MODE:
			ipoib.Mode = IPoIBMode(native.Uint16(datum.Value))
		case nl.IFLA_IPOIB_UMCAST:
			ipoib.Umcast = uint16(native.Uint16(datum.Value))
		}
	}
}

func parseCanData(link Link, data []syscall.NetlinkRouteAttr) {
	can := link.(*Can)
	for _, datum := range data {

		switch datum.Attr.Type {
		case nl.IFLA_CAN_BITTIMING:
			can.BitRate = native.Uint32(datum.Value)
			can.SamplePoint = native.Uint32(datum.Value[4:])
			can.TimeQuanta = native.Uint32(datum.Value[8:])
			can.PropagationSegment = native.Uint32(datum.Value[12:])
			can.PhaseSegment1 = native.Uint32(datum.Value[16:])
			can.PhaseSegment2 = native.Uint32(datum.Value[20:])
			can.SyncJumpWidth = native.Uint32(datum.Value[24:])
			can.BitRatePreScaler = native.Uint32(datum.Value[28:])
		case nl.IFLA_CAN_BITTIMING_CONST:
			can.Name = string(datum.Value[:16])
			can.TimeSegment1Min = native.Uint32(datum.Value[16:])
			can.TimeSegment1Max = native.Uint32(datum.Value[20:])
			can.TimeSegment2Min = native.Uint32(datum.Value[24:])
			can.TimeSegment2Max = native.Uint32(datum.Value[28:])
			can.SyncJumpWidthMax = native.Uint32(datum.Value[32:])
			can.BitRatePreScalerMin = native.Uint32(datum.Value[36:])
			can.BitRatePreScalerMax = native.Uint32(datum.Value[40:])
			can.BitRatePreScalerInc = native.Uint32(datum.Value[44:])
		case nl.IFLA_CAN_CLOCK:
			can.ClockFrequency = native.Uint32(datum.Value)
		case nl.IFLA_CAN_STATE:
			can.State = native.Uint32(datum.Value)
		case nl.IFLA_CAN_CTRLMODE:
			can.Mask = native.Uint32(datum.Value)
			can.Flags = native.Uint32(datum.Value[4:])
		case nl.IFLA_CAN_BERR_COUNTER:
			can.TxError = native.Uint16(datum.Value)
			can.RxError = native.Uint16(datum.Value[2:])
		case nl.IFLA_CAN_RESTART_MS:
			can.RestartMs = native.Uint32(datum.Value)
		case nl.IFLA_CAN_DATA_BITTIMING_CONST:
		case nl.IFLA_CAN_RESTART:
		case nl.IFLA_CAN_DATA_BITTIMING:
		case nl.IFLA_CAN_TERMINATION:
		case nl.IFLA_CAN_TERMINATION_CONST:
		case nl.IFLA_CAN_BITRATE_CONST:
		case nl.IFLA_CAN_DATA_BITRATE_CONST:
		case nl.IFLA_CAN_BITRATE_MAX:
		}
	}
}

func addIPoIBAttrs(ipoib *IPoIB, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_IPOIB_PKEY, nl.Uint16Attr(uint16(ipoib.Pkey)))
	data.AddRtAttr(nl.IFLA_IPOIB_MODE, nl.Uint16Attr(uint16(ipoib.Mode)))
	data.AddRtAttr(nl.IFLA_IPOIB_UMCAST, nl.Uint16Attr(uint16(ipoib.Umcast)))
}

func addBareUDPAttrs(bareudp *BareUDP, linkInfo *nl.RtAttr) {
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)

	data.AddRtAttr(nl.IFLA_BAREUDP_PORT, nl.Uint16Attr(nl.Swap16(bareudp.Port)))
	data.AddRtAttr(nl.IFLA_BAREUDP_ETHERTYPE, nl.Uint16Attr(nl.Swap16(bareudp.EtherType)))
	if bareudp.SrcPortMin != 0 {
		data.AddRtAttr(nl.IFLA_BAREUDP_SRCPORT_MIN, nl.Uint16Attr(bareudp.SrcPortMin))
	}
	if bareudp.MultiProto {
		data.AddRtAttr(nl.IFLA_BAREUDP_MULTIPROTO_MODE, []byte{})
	}
}

func parseBareUDPData(link Link, data []syscall.NetlinkRouteAttr) {
	bareudp := link.(*BareUDP)
	for _, attr := range data {
		switch attr.Attr.Type {
		case nl.IFLA_BAREUDP_PORT:
			bareudp.Port = binary.BigEndian.Uint16(attr.Value)
		case nl.IFLA_BAREUDP_ETHERTYPE:
			bareudp.EtherType = binary.BigEndian.Uint16(attr.Value)
		case nl.IFLA_BAREUDP_SRCPORT_MIN:
			bareudp.SrcPortMin = native.Uint16(attr.Value)
		case nl.IFLA_BAREUDP_MULTIPROTO_MODE:
			bareudp.MultiProto = true
		}
	}
}
