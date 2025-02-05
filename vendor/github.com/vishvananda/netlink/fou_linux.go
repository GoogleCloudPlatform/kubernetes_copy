//go:build linux
// +build linux

package netlink

import (
	"encoding/binary"
	"errors"
	"log"
	"net"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	FOU_GENL_NAME = "fou"
)

const (
	FOU_CMD_UNSPEC uint8 = iota
	FOU_CMD_ADD
	FOU_CMD_DEL
	FOU_CMD_GET
	FOU_CMD_MAX = FOU_CMD_GET
)

const (
	FOU_ATTR_UNSPEC = iota
	FOU_ATTR_PORT
	FOU_ATTR_AF
	FOU_ATTR_IPPROTO
	FOU_ATTR_TYPE
	FOU_ATTR_REMCSUM_NOPARTIAL
	FOU_ATTR_LOCAL_V4
	FOU_ATTR_LOCAL_V6
	FOU_ATTR_PEER_V4
	FOU_ATTR_PEER_V6
	FOU_ATTR_PEER_PORT
	FOU_ATTR_IFINDEX
	FOU_ATTR_MAX = FOU_ATTR_REMCSUM_NOPARTIAL
)

const (
	FOU_ENCAP_UNSPEC = iota
	FOU_ENCAP_DIRECT
	FOU_ENCAP_GUE
	FOU_ENCAP_MAX = FOU_ENCAP_GUE
)

var fouFamilyId int

func FouFamilyId() (int, error) {
	if fouFamilyId != 0 {
		return fouFamilyId, nil
	}

	fam, err := GenlFamilyGet(FOU_GENL_NAME)
	if err != nil {
		return -1, err
	}

	fouFamilyId = int(fam.ID)
	return fouFamilyId, nil
}

func FouAdd(f Fou) error {
	return pkgHandle.FouAdd(f)
}

func (h *Handle) FouAdd(f Fou) error {
	fam_id, err := FouFamilyId()
	if err != nil {
		return err
	}

	// setting ip protocol conflicts with encapsulation type GUE
	if f.EncapType == FOU_ENCAP_GUE && f.Protocol != 0 {
		return errors.New("GUE encapsulation doesn't specify an IP protocol")
	}

	req := h.newNetlinkRequest(fam_id, unix.NLM_F_ACK)

	// int to byte for port
	bp := make([]byte, 2)
	binary.BigEndian.PutUint16(bp[0:2], uint16(f.Port))

	attrs := []*nl.RtAttr{
		nl.NewRtAttr(FOU_ATTR_PORT, bp),
		nl.NewRtAttr(FOU_ATTR_TYPE, []byte{uint8(f.EncapType)}),
		nl.NewRtAttr(FOU_ATTR_AF, []byte{uint8(f.Family)}),
		nl.NewRtAttr(FOU_ATTR_IPPROTO, []byte{uint8(f.Protocol)}),
	}
	raw := []byte{FOU_CMD_ADD, 1, 0, 0}
	for _, a := range attrs {
		raw = append(raw, a.Serialize()...)
	}

	req.AddRawData(raw)

	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	return err
}

func FouDel(f Fou) error {
	return pkgHandle.FouDel(f)
}

func (h *Handle) FouDel(f Fou) error {
	fam_id, err := FouFamilyId()
	if err != nil {
		return err
	}

	req := h.newNetlinkRequest(fam_id, unix.NLM_F_ACK)

	// int to byte for port
	bp := make([]byte, 2)
	binary.BigEndian.PutUint16(bp[0:2], uint16(f.Port))

	attrs := []*nl.RtAttr{
		nl.NewRtAttr(FOU_ATTR_PORT, bp),
		nl.NewRtAttr(FOU_ATTR_AF, []byte{uint8(f.Family)}),
	}
	raw := []byte{FOU_CMD_DEL, 1, 0, 0}
	for _, a := range attrs {
		raw = append(raw, a.Serialize()...)
	}

	req.AddRawData(raw)

	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return err
	}

	return nil
}

// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func FouList(fam int) ([]Fou, error) {
	return pkgHandle.FouList(fam)
}

// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) FouList(fam int) ([]Fou, error) {
	fam_id, err := FouFamilyId()
	if err != nil {
		return nil, err
	}

	req := h.newNetlinkRequest(fam_id, unix.NLM_F_DUMP)

	attrs := []*nl.RtAttr{
		nl.NewRtAttr(FOU_ATTR_AF, []byte{uint8(fam)}),
	}
	raw := []byte{FOU_CMD_GET, 1, 0, 0}
	for _, a := range attrs {
		raw = append(raw, a.Serialize()...)
	}

	req.AddRawData(raw)

	msgs, executeErr := req.Execute(unix.NETLINK_GENERIC, 0)
	if executeErr != nil && !errors.Is(err, ErrDumpInterrupted) {
		return nil, executeErr
	}

	fous := make([]Fou, 0, len(msgs))
	for _, m := range msgs {
		f, err := deserializeFouMsg(m)
		if err != nil {
			return fous, err
		}

		fous = append(fous, f)
	}

	return fous, executeErr
}

func deserializeFouMsg(msg []byte) (Fou, error) {
	fou := Fou{}

	for attr := range nl.ParseAttributes(msg[4:]) {
		switch attr.Type {
		case FOU_ATTR_AF:
			fou.Family = int(attr.Value[0])
		case FOU_ATTR_PORT:
			fou.Port = int(networkOrder.Uint16(attr.Value))
		case FOU_ATTR_IPPROTO:
			fou.Protocol = int(attr.Value[0])
		case FOU_ATTR_TYPE:
			fou.EncapType = int(attr.Value[0])
		case FOU_ATTR_LOCAL_V4, FOU_ATTR_LOCAL_V6:
			fou.Local = net.IP(attr.Value)
		case FOU_ATTR_PEER_V4, FOU_ATTR_PEER_V6:
			fou.Peer = net.IP(attr.Value)
		case FOU_ATTR_PEER_PORT:
			fou.PeerPort = int(networkOrder.Uint16(attr.Value))
		case FOU_ATTR_IFINDEX:
			fou.IfIndex = int(native.Uint16(attr.Value))
		default:
			log.Printf("unknown fou attribute from kernel: %+v %v", attr, attr.Type&nl.NLA_TYPE_MASK)
		}
	}

	return fou, nil
}
