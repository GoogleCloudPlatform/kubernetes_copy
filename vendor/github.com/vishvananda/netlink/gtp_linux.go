package netlink

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

type PDP struct {
	Version     uint32
	TID         uint64
	PeerAddress net.IP
	MSAddress   net.IP
	Flow        uint16
	NetNSFD     uint32
	ITEI        uint32
	OTEI        uint32
}

func (pdp *PDP) String() string {
	elems := []string{}
	elems = append(elems, fmt.Sprintf("Version: %d", pdp.Version))
	if pdp.Version == 0 {
		elems = append(elems, fmt.Sprintf("TID: %d", pdp.TID))
	} else if pdp.Version == 1 {
		elems = append(elems, fmt.Sprintf("TEI: %d/%d", pdp.ITEI, pdp.OTEI))
	}
	elems = append(elems, fmt.Sprintf("MS-Address: %s", pdp.MSAddress))
	elems = append(elems, fmt.Sprintf("Peer-Address: %s", pdp.PeerAddress))
	return fmt.Sprintf("{%s}", strings.Join(elems, " "))
}

func (p *PDP) parseAttributes(attrs []syscall.NetlinkRouteAttr) error {
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.GENL_GTP_ATTR_VERSION:
			p.Version = native.Uint32(a.Value)
		case nl.GENL_GTP_ATTR_TID:
			p.TID = native.Uint64(a.Value)
		case nl.GENL_GTP_ATTR_PEER_ADDRESS:
			p.PeerAddress = net.IP(a.Value)
		case nl.GENL_GTP_ATTR_MS_ADDRESS:
			p.MSAddress = net.IP(a.Value)
		case nl.GENL_GTP_ATTR_FLOW:
			p.Flow = native.Uint16(a.Value)
		case nl.GENL_GTP_ATTR_NET_NS_FD:
			p.NetNSFD = native.Uint32(a.Value)
		case nl.GENL_GTP_ATTR_I_TEI:
			p.ITEI = native.Uint32(a.Value)
		case nl.GENL_GTP_ATTR_O_TEI:
			p.OTEI = native.Uint32(a.Value)
		}
	}
	return nil
}

func parsePDP(msgs [][]byte) ([]*PDP, error) {
	pdps := make([]*PDP, 0, len(msgs))
	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		pdp := &PDP{}
		if err := pdp.parseAttributes(attrs); err != nil {
			return nil, err
		}
		pdps = append(pdps, pdp)
	}
	return pdps, nil
}

// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) GTPPDPList() ([]*PDP, error) {
	f, err := h.GenlFamilyGet(nl.GENL_GTP_NAME)
	if err != nil {
		return nil, err
	}
	msg := &nl.Genlmsg{
		Command: nl.GENL_GTP_CMD_GETPDP,
		Version: nl.GENL_GTP_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID), unix.NLM_F_DUMP)
	req.AddData(msg)
	msgs, executeErr := req.Execute(unix.NETLINK_GENERIC, 0)
	if executeErr != nil && !errors.Is(err, ErrDumpInterrupted) {
		return nil, executeErr
	}
	pdps, err := parsePDP(msgs)
	if err != nil {
		return nil, err
	}
	return pdps, executeErr
}

// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func GTPPDPList() ([]*PDP, error) {
	return pkgHandle.GTPPDPList()
}

func gtpPDPGet(req *nl.NetlinkRequest) (*PDP, error) {
	msgs, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}
	pdps, err := parsePDP(msgs)
	if err != nil {
		return nil, err
	}
	if len(pdps) != 1 {
		return nil, fmt.Errorf("invalid reqponse for GENL_GTP_CMD_GETPDP")
	}
	return pdps[0], nil
}

func (h *Handle) GTPPDPByTID(link Link, tid int) (*PDP, error) {
	f, err := h.GenlFamilyGet(nl.GENL_GTP_NAME)
	if err != nil {
		return nil, err
	}
	msg := &nl.Genlmsg{
		Command: nl.GENL_GTP_CMD_GETPDP,
		Version: nl.GENL_GTP_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID), 0)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_VERSION, nl.Uint32Attr(0)))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_LINK, nl.Uint32Attr(uint32(link.Attrs().Index))))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_TID, nl.Uint64Attr(uint64(tid))))
	return gtpPDPGet(req)
}

func GTPPDPByTID(link Link, tid int) (*PDP, error) {
	return pkgHandle.GTPPDPByTID(link, tid)
}

func (h *Handle) GTPPDPByITEI(link Link, itei int) (*PDP, error) {
	f, err := h.GenlFamilyGet(nl.GENL_GTP_NAME)
	if err != nil {
		return nil, err
	}
	msg := &nl.Genlmsg{
		Command: nl.GENL_GTP_CMD_GETPDP,
		Version: nl.GENL_GTP_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID), 0)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_VERSION, nl.Uint32Attr(1)))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_LINK, nl.Uint32Attr(uint32(link.Attrs().Index))))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_I_TEI, nl.Uint32Attr(uint32(itei))))
	return gtpPDPGet(req)
}

func GTPPDPByITEI(link Link, itei int) (*PDP, error) {
	return pkgHandle.GTPPDPByITEI(link, itei)
}

func (h *Handle) GTPPDPByMSAddress(link Link, addr net.IP) (*PDP, error) {
	f, err := h.GenlFamilyGet(nl.GENL_GTP_NAME)
	if err != nil {
		return nil, err
	}
	msg := &nl.Genlmsg{
		Command: nl.GENL_GTP_CMD_GETPDP,
		Version: nl.GENL_GTP_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID), 0)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_VERSION, nl.Uint32Attr(0)))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_LINK, nl.Uint32Attr(uint32(link.Attrs().Index))))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_MS_ADDRESS, []byte(addr.To4())))
	return gtpPDPGet(req)
}

func GTPPDPByMSAddress(link Link, addr net.IP) (*PDP, error) {
	return pkgHandle.GTPPDPByMSAddress(link, addr)
}

func (h *Handle) GTPPDPAdd(link Link, pdp *PDP) error {
	f, err := h.GenlFamilyGet(nl.GENL_GTP_NAME)
	if err != nil {
		return err
	}
	msg := &nl.Genlmsg{
		Command: nl.GENL_GTP_CMD_NEWPDP,
		Version: nl.GENL_GTP_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID), unix.NLM_F_EXCL|unix.NLM_F_ACK)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_VERSION, nl.Uint32Attr(pdp.Version)))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_LINK, nl.Uint32Attr(uint32(link.Attrs().Index))))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_PEER_ADDRESS, []byte(pdp.PeerAddress.To4())))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_MS_ADDRESS, []byte(pdp.MSAddress.To4())))

	switch pdp.Version {
	case 0:
		req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_TID, nl.Uint64Attr(pdp.TID)))
		req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_FLOW, nl.Uint16Attr(pdp.Flow)))
	case 1:
		req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_I_TEI, nl.Uint32Attr(pdp.ITEI)))
		req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_O_TEI, nl.Uint32Attr(pdp.OTEI)))
	default:
		return fmt.Errorf("unsupported GTP version: %d", pdp.Version)
	}
	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	return err
}

func GTPPDPAdd(link Link, pdp *PDP) error {
	return pkgHandle.GTPPDPAdd(link, pdp)
}

func (h *Handle) GTPPDPDel(link Link, pdp *PDP) error {
	f, err := h.GenlFamilyGet(nl.GENL_GTP_NAME)
	if err != nil {
		return err
	}
	msg := &nl.Genlmsg{
		Command: nl.GENL_GTP_CMD_DELPDP,
		Version: nl.GENL_GTP_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID), unix.NLM_F_EXCL|unix.NLM_F_ACK)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_VERSION, nl.Uint32Attr(pdp.Version)))
	req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_LINK, nl.Uint32Attr(uint32(link.Attrs().Index))))

	switch pdp.Version {
	case 0:
		req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_TID, nl.Uint64Attr(pdp.TID)))
	case 1:
		req.AddData(nl.NewRtAttr(nl.GENL_GTP_ATTR_I_TEI, nl.Uint32Attr(pdp.ITEI)))
	default:
		return fmt.Errorf("unsupported GTP version: %d", pdp.Version)
	}
	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	return err
}

func GTPPDPDel(link Link, pdp *PDP) error {
	return pkgHandle.GTPPDPDel(link, pdp)
}
