package netlink

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// BridgeVlanList gets a map of device id to bridge vlan infos.
// Equivalent to: `bridge vlan show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error) {
	return pkgHandle.BridgeVlanList()
}

// BridgeVlanList gets a map of device id to bridge vlan infos.
// Equivalent to: `bridge vlan show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error) {
	req := h.newNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_DUMP)
	msg := nl.NewIfInfomsg(unix.AF_BRIDGE)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFLA_EXT_MASK, nl.Uint32Attr(uint32(nl.RTEXT_FILTER_BRVLAN))))

	msgs, executeErr := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	ret := make(map[int32][]*nl.BridgeVlanInfo)
	for _, m := range msgs {
		msg := nl.DeserializeIfInfomsg(m)

		attrs, err := nl.ParseRouteAttr(m[msg.Len():])
		if err != nil {
			return nil, err
		}
		for _, attr := range attrs {
			switch attr.Attr.Type {
			case unix.IFLA_AF_SPEC:
				//nested attr
				nestAttrs, err := nl.ParseRouteAttr(attr.Value)
				if err != nil {
					return nil, fmt.Errorf("failed to parse nested attr %v", err)
				}
				for _, nestAttr := range nestAttrs {
					switch nestAttr.Attr.Type {
					case nl.IFLA_BRIDGE_VLAN_INFO:
						vlanInfo := nl.DeserializeBridgeVlanInfo(nestAttr.Value)
						ret[msg.Index] = append(ret[msg.Index], vlanInfo)
					}
				}
			}
		}
	}
	return ret, executeErr
}

// BridgeVlanAdd adds a new vlan filter entry
// Equivalent to: `bridge vlan add dev DEV vid VID [ pvid ] [ untagged ] [ self ] [ master ]`
func BridgeVlanAdd(link Link, vid uint16, pvid, untagged, self, master bool) error {
	return pkgHandle.BridgeVlanAdd(link, vid, pvid, untagged, self, master)
}

// BridgeVlanAdd adds a new vlan filter entry
// Equivalent to: `bridge vlan add dev DEV vid VID [ pvid ] [ untagged ] [ self ] [ master ]`
func (h *Handle) BridgeVlanAdd(link Link, vid uint16, pvid, untagged, self, master bool) error {
	return h.bridgeVlanModify(unix.RTM_SETLINK, link, vid, 0, pvid, untagged, self, master)
}

// BridgeVlanAddRange adds a new vlan filter entry
// Equivalent to: `bridge vlan add dev DEV vid VID-VIDEND [ pvid ] [ untagged ] [ self ] [ master ]`
func BridgeVlanAddRange(link Link, vid, vidEnd uint16, pvid, untagged, self, master bool) error {
	return pkgHandle.BridgeVlanAddRange(link, vid, vidEnd, pvid, untagged, self, master)
}

// BridgeVlanAddRange adds a new vlan filter entry
// Equivalent to: `bridge vlan add dev DEV vid VID-VIDEND [ pvid ] [ untagged ] [ self ] [ master ]`
func (h *Handle) BridgeVlanAddRange(link Link, vid, vidEnd uint16, pvid, untagged, self, master bool) error {
	return h.bridgeVlanModify(unix.RTM_SETLINK, link, vid, vidEnd, pvid, untagged, self, master)
}

// BridgeVlanDel adds a new vlan filter entry
// Equivalent to: `bridge vlan del dev DEV vid VID [ pvid ] [ untagged ] [ self ] [ master ]`
func BridgeVlanDel(link Link, vid uint16, pvid, untagged, self, master bool) error {
	return pkgHandle.BridgeVlanDel(link, vid, pvid, untagged, self, master)
}

// BridgeVlanDel adds a new vlan filter entry
// Equivalent to: `bridge vlan del dev DEV vid VID [ pvid ] [ untagged ] [ self ] [ master ]`
func (h *Handle) BridgeVlanDel(link Link, vid uint16, pvid, untagged, self, master bool) error {
	return h.bridgeVlanModify(unix.RTM_DELLINK, link, vid, 0, pvid, untagged, self, master)
}

// BridgeVlanDelRange adds a new vlan filter entry
// Equivalent to: `bridge vlan del dev DEV vid VID-VIDEND [ pvid ] [ untagged ] [ self ] [ master ]`
func BridgeVlanDelRange(link Link, vid, vidEnd uint16, pvid, untagged, self, master bool) error {
	return pkgHandle.BridgeVlanDelRange(link, vid, vidEnd, pvid, untagged, self, master)
}

// BridgeVlanDelRange adds a new vlan filter entry
// Equivalent to: `bridge vlan del dev DEV vid VID-VIDEND [ pvid ] [ untagged ] [ self ] [ master ]`
func (h *Handle) BridgeVlanDelRange(link Link, vid, vidEnd uint16, pvid, untagged, self, master bool) error {
	return h.bridgeVlanModify(unix.RTM_DELLINK, link, vid, vidEnd, pvid, untagged, self, master)
}

func (h *Handle) bridgeVlanModify(cmd int, link Link, vid, vidEnd uint16, pvid, untagged, self, master bool) error {
	base := link.Attrs()
	h.ensureIndex(base)
	req := h.newNetlinkRequest(cmd, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_BRIDGE)
	msg.Index = int32(base.Index)
	req.AddData(msg)

	br := nl.NewRtAttr(unix.IFLA_AF_SPEC, nil)
	var flags uint16
	if self {
		flags |= nl.BRIDGE_FLAGS_SELF
	}
	if master {
		flags |= nl.BRIDGE_FLAGS_MASTER
	}
	if flags > 0 {
		br.AddRtAttr(nl.IFLA_BRIDGE_FLAGS, nl.Uint16Attr(flags))
	}
	vlanInfo := &nl.BridgeVlanInfo{Vid: vid}
	if pvid {
		vlanInfo.Flags |= nl.BRIDGE_VLAN_INFO_PVID
	}
	if untagged {
		vlanInfo.Flags |= nl.BRIDGE_VLAN_INFO_UNTAGGED
	}

	if vidEnd != 0 {
		vlanEndInfo := &nl.BridgeVlanInfo{Vid: vidEnd}
		vlanEndInfo.Flags = vlanInfo.Flags

		vlanInfo.Flags |= nl.BRIDGE_VLAN_INFO_RANGE_BEGIN
		br.AddRtAttr(nl.IFLA_BRIDGE_VLAN_INFO, vlanInfo.Serialize())

		vlanEndInfo.Flags |= nl.BRIDGE_VLAN_INFO_RANGE_END
		br.AddRtAttr(nl.IFLA_BRIDGE_VLAN_INFO, vlanEndInfo.Serialize())
	} else { 
		br.AddRtAttr(nl.IFLA_BRIDGE_VLAN_INFO, vlanInfo.Serialize())
	}

	req.AddData(br)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}
