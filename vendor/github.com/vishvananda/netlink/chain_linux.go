package netlink

import (
	"errors"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// ChainDel will delete a chain from the system.
func ChainDel(link Link, chain Chain) error {
	// Equivalent to: `tc chain del $chain`
	return pkgHandle.ChainDel(link, chain)
}

// ChainDel will delete a chain from the system.
// Equivalent to: `tc chain del $chain`
func (h *Handle) ChainDel(link Link, chain Chain) error {
	return h.chainModify(unix.RTM_DELCHAIN, 0, link, chain)
}

// ChainAdd will add a chain to the system.
// Equivalent to: `tc chain add`
func ChainAdd(link Link, chain Chain) error {
	return pkgHandle.ChainAdd(link, chain)
}

// ChainAdd will add a chain to the system.
// Equivalent to: `tc chain add`
func (h *Handle) ChainAdd(link Link, chain Chain) error {
	return h.chainModify(
		unix.RTM_NEWCHAIN,
		unix.NLM_F_CREATE|unix.NLM_F_EXCL,
		link,
		chain)
}

func (h *Handle) chainModify(cmd, flags int, link Link, chain Chain) error {
	req := h.newNetlinkRequest(cmd, flags|unix.NLM_F_ACK)
	index := int32(0)
	if link != nil {
		base := link.Attrs()
		h.ensureIndex(base)
		index = int32(base.Index)
	}
	msg := &nl.TcMsg{
		Family:  nl.FAMILY_ALL,
		Ifindex: index,
		Parent:  chain.Parent,
	}
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.TCA_CHAIN, nl.Uint32Attr(chain.Chain)))

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// ChainList gets a list of chains in the system.
// Equivalent to: `tc chain list`.
// The list can be filtered by link.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func ChainList(link Link, parent uint32) ([]Chain, error) {
	return pkgHandle.ChainList(link, parent)
}

// ChainList gets a list of chains in the system.
// Equivalent to: `tc chain list`.
// The list can be filtered by link.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) ChainList(link Link, parent uint32) ([]Chain, error) {
	req := h.newNetlinkRequest(unix.RTM_GETCHAIN, unix.NLM_F_DUMP)
	index := int32(0)
	if link != nil {
		base := link.Attrs()
		h.ensureIndex(base)
		index = int32(base.Index)
	}
	msg := &nl.TcMsg{
		Family:  nl.FAMILY_ALL,
		Ifindex: index,
		Parent:  parent,
	}
	req.AddData(msg)

	msgs, executeErr := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWCHAIN)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}

	var res []Chain
	for _, m := range msgs {
		msg := nl.DeserializeTcMsg(m)

		attrs, err := nl.ParseRouteAttr(m[msg.Len():])
		if err != nil {
			return nil, err
		}

		// skip chains from other interfaces
		if link != nil && msg.Ifindex != index {
			continue
		}

		var chain Chain
		for _, attr := range attrs {
			switch attr.Attr.Type {
			case nl.TCA_CHAIN:
				chain.Chain = native.Uint32(attr.Value)
				chain.Parent = parent
			}
		}
		res = append(res, chain)
	}

	return res, executeErr
}
