package netlink

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

type GenlOp struct {
	ID    uint32
	Flags uint32
}

type GenlMulticastGroup struct {
	ID   uint32
	Name string
}

type GenlFamily struct {
	ID      uint16
	HdrSize uint32
	Name    string
	Version uint32
	MaxAttr uint32
	Ops     []GenlOp
	Groups  []GenlMulticastGroup
}

func parseOps(b []byte) ([]GenlOp, error) {
	attrs, err := nl.ParseRouteAttr(b)
	if err != nil {
		return nil, err
	}
	ops := make([]GenlOp, 0, len(attrs))
	for _, a := range attrs {
		nattrs, err := nl.ParseRouteAttr(a.Value)
		if err != nil {
			return nil, err
		}
		var op GenlOp
		for _, na := range nattrs {
			switch na.Attr.Type {
			case nl.GENL_CTRL_ATTR_OP_ID:
				op.ID = native.Uint32(na.Value)
			case nl.GENL_CTRL_ATTR_OP_FLAGS:
				op.Flags = native.Uint32(na.Value)
			}
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func parseMulticastGroups(b []byte) ([]GenlMulticastGroup, error) {
	attrs, err := nl.ParseRouteAttr(b)
	if err != nil {
		return nil, err
	}
	groups := make([]GenlMulticastGroup, 0, len(attrs))
	for _, a := range attrs {
		nattrs, err := nl.ParseRouteAttr(a.Value)
		if err != nil {
			return nil, err
		}
		var g GenlMulticastGroup
		for _, na := range nattrs {
			switch na.Attr.Type {
			case nl.GENL_CTRL_ATTR_MCAST_GRP_NAME:
				g.Name = nl.BytesToString(na.Value)
			case nl.GENL_CTRL_ATTR_MCAST_GRP_ID:
				g.ID = native.Uint32(na.Value)
			}
		}
		groups = append(groups, g)
	}
	return groups, nil
}

func (f *GenlFamily) parseAttributes(attrs []syscall.NetlinkRouteAttr) error {
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.GENL_CTRL_ATTR_FAMILY_NAME:
			f.Name = nl.BytesToString(a.Value)
		case nl.GENL_CTRL_ATTR_FAMILY_ID:
			f.ID = native.Uint16(a.Value)
		case nl.GENL_CTRL_ATTR_VERSION:
			f.Version = native.Uint32(a.Value)
		case nl.GENL_CTRL_ATTR_HDRSIZE:
			f.HdrSize = native.Uint32(a.Value)
		case nl.GENL_CTRL_ATTR_MAXATTR:
			f.MaxAttr = native.Uint32(a.Value)
		case nl.GENL_CTRL_ATTR_OPS:
			ops, err := parseOps(a.Value)
			if err != nil {
				return err
			}
			f.Ops = ops
		case nl.GENL_CTRL_ATTR_MCAST_GROUPS:
			groups, err := parseMulticastGroups(a.Value)
			if err != nil {
				return err
			}
			f.Groups = groups
		}
	}

	return nil
}

func parseFamilies(msgs [][]byte) ([]*GenlFamily, error) {
	families := make([]*GenlFamily, 0, len(msgs))
	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		family := &GenlFamily{}
		if err := family.parseAttributes(attrs); err != nil {
			return nil, err
		}

		families = append(families, family)
	}
	return families, nil
}

// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) GenlFamilyList() ([]*GenlFamily, error) {
	msg := &nl.Genlmsg{
		Command: nl.GENL_CTRL_CMD_GETFAMILY,
		Version: nl.GENL_CTRL_VERSION,
	}
	req := h.newNetlinkRequest(nl.GENL_ID_CTRL, unix.NLM_F_DUMP)
	req.AddData(msg)
	msgs, executeErr := req.Execute(unix.NETLINK_GENERIC, 0)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	families, err := parseFamilies(msgs)
	if err != nil {
		return nil, err
	}
	return families, executeErr
}

// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func GenlFamilyList() ([]*GenlFamily, error) {
	return pkgHandle.GenlFamilyList()
}

func (h *Handle) GenlFamilyGet(name string) (*GenlFamily, error) {
	msg := &nl.Genlmsg{
		Command: nl.GENL_CTRL_CMD_GETFAMILY,
		Version: nl.GENL_CTRL_VERSION,
	}
	req := h.newNetlinkRequest(nl.GENL_ID_CTRL, 0)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.GENL_CTRL_ATTR_FAMILY_NAME, nl.ZeroTerminated(name)))
	msgs, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}
	families, err := parseFamilies(msgs)
	if err != nil {
		return nil, err
	}
	if len(families) != 1 {
		return nil, fmt.Errorf("invalid response for GENL_CTRL_CMD_GETFAMILY")
	}
	return families[0], nil
}

func GenlFamilyGet(name string) (*GenlFamily, error) {
	return pkgHandle.GenlFamilyGet(name)
}
