package netlink

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	sizeofXDPSocketRequest = 1 + 1 + 2 + 4 + 4 + 2*4
	sizeofXDPSocket        = 0x10
)

// https://elixir.bootlin.com/linux/v6.2/source/include/uapi/linux/xdp_diag.h#L12
type xdpSocketRequest struct {
	Family   uint8
	Protocol uint8
	pad      uint16
	Ino      uint32
	Show     uint32
	Cookie   [2]uint32
}

func (r *xdpSocketRequest) Serialize() []byte {
	b := writeBuffer{Bytes: make([]byte, sizeofSocketRequest)}
	b.Write(r.Family)
	b.Write(r.Protocol)
	native.PutUint16(b.Next(2), r.pad)
	native.PutUint32(b.Next(4), r.Ino)
	native.PutUint32(b.Next(4), r.Show)
	native.PutUint32(b.Next(4), r.Cookie[0])
	native.PutUint32(b.Next(4), r.Cookie[1])
	return b.Bytes
}

func (r *xdpSocketRequest) Len() int { return sizeofXDPSocketRequest }

func (s *XDPSocket) deserialize(b []byte) error {
	if len(b) < sizeofXDPSocket {
		return fmt.Errorf("XDP socket data short read (%d); want %d", len(b), sizeofXDPSocket)
	}
	rb := readBuffer{Bytes: b}
	s.Family = rb.Read()
	s.Type = rb.Read()
	s.pad = native.Uint16(rb.Next(2))
	s.Ino = native.Uint32(rb.Next(4))
	s.Cookie[0] = native.Uint32(rb.Next(4))
	s.Cookie[1] = native.Uint32(rb.Next(4))
	return nil
}

// SocketXDPGetInfo returns the XDP socket identified by its inode number and/or
// socket cookie. Specify the cookie as SOCK_ANY_COOKIE if
//
// If the returned error is [ErrDumpInterrupted], the caller should retry.
func SocketXDPGetInfo(ino uint32, cookie uint64) (*XDPDiagInfoResp, error) {
	// We have a problem here: dumping AF_XDP sockets currently does not support
	// filtering. We thus need to dump all XSKs and then only filter afterwards
	// :(
	xsks, err := SocketDiagXDP()
	if err != nil {
		return nil, err
	}
	checkCookie := cookie != SOCK_ANY_COOKIE && cookie != 0
	crumblingCookie := [2]uint32{uint32(cookie), uint32(cookie >> 32)}
	checkIno := ino != 0
	var xskinfo *XDPDiagInfoResp
	for _, xsk := range xsks {
		if checkIno && xsk.XDPDiagMsg.Ino != ino {
			continue
		}
		if checkCookie && xsk.XDPDiagMsg.Cookie != crumblingCookie {
			continue
		}
		if xskinfo != nil {
			return nil, errors.New("multiple matching XDP sockets")
		}
		xskinfo = xsk
	}
	if xskinfo == nil {
		return nil, errors.New("no matching XDP socket")
	}
	return xskinfo, nil
}

// SocketDiagXDP requests XDP_DIAG_INFO for XDP family sockets.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func SocketDiagXDP() ([]*XDPDiagInfoResp, error) {
	var result []*XDPDiagInfoResp
	err := socketDiagXDPExecutor(func(m syscall.NetlinkMessage) error {
		sockInfo := &XDPSocket{}
		if err := sockInfo.deserialize(m.Data); err != nil {
			return err
		}
		attrs, err := nl.ParseRouteAttr(m.Data[sizeofXDPSocket:])
		if err != nil {
			return err
		}

		res, err := attrsToXDPDiagInfoResp(attrs, sockInfo)
		if err != nil {
			return err
		}

		result = append(result, res)
		return nil
	})
	if err != nil && !errors.Is(err, ErrDumpInterrupted) {
		return nil, err
	}
	return result, err
}

// socketDiagXDPExecutor requests XDP_DIAG_INFO for XDP family sockets.
func socketDiagXDPExecutor(receiver func(syscall.NetlinkMessage) error) error {
	s, err := nl.Subscribe(unix.NETLINK_INET_DIAG)
	if err != nil {
		return err
	}
	defer s.Close()

	req := nl.NewNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&xdpSocketRequest{
		Family: unix.AF_XDP,
		Show:   XDP_SHOW_INFO | XDP_SHOW_RING_CFG | XDP_SHOW_UMEM | XDP_SHOW_STATS,
	})
	if err := s.Send(req); err != nil {
		return err
	}

	dumpIntr := false
loop:
	for {
		msgs, from, err := s.Receive()
		if err != nil {
			return err
		}
		if from.Pid != nl.PidKernel {
			return fmt.Errorf("Wrong sender portid %d, expected %d", from.Pid, nl.PidKernel)
		}
		if len(msgs) == 0 {
			return errors.New("no message nor error from netlink")
		}

		for _, m := range msgs {
			if m.Header.Flags&unix.NLM_F_DUMP_INTR != 0 {
				dumpIntr = true
			}
			switch m.Header.Type {
			case unix.NLMSG_DONE:
				break loop
			case unix.NLMSG_ERROR:
				error := int32(native.Uint32(m.Data[0:4]))
				return syscall.Errno(-error)
			}
			if err := receiver(m); err != nil {
				return err
			}
		}
	}
	if dumpIntr {
		return ErrDumpInterrupted
	}
	return nil
}

func attrsToXDPDiagInfoResp(attrs []syscall.NetlinkRouteAttr, sockInfo *XDPSocket) (*XDPDiagInfoResp, error) {
	resp := &XDPDiagInfoResp{
		XDPDiagMsg: sockInfo,
		XDPInfo:    &XDPInfo{},
	}
	for _, a := range attrs {
		switch a.Attr.Type {
		case XDP_DIAG_INFO:
			resp.XDPInfo.Ifindex = native.Uint32(a.Value[0:4])
			resp.XDPInfo.QueueID = native.Uint32(a.Value[4:8])
		case XDP_DIAG_UID:
			resp.XDPInfo.UID = native.Uint32(a.Value[0:4])
		case XDP_DIAG_RX_RING:
			resp.XDPInfo.RxRingEntries = native.Uint32(a.Value[0:4])
		case XDP_DIAG_TX_RING:
			resp.XDPInfo.TxRingEntries = native.Uint32(a.Value[0:4])
		case XDP_DIAG_UMEM_FILL_RING:
			resp.XDPInfo.UmemFillRingEntries = native.Uint32(a.Value[0:4])
		case XDP_DIAG_UMEM_COMPLETION_RING:
			resp.XDPInfo.UmemCompletionRingEntries = native.Uint32(a.Value[0:4])
		case XDP_DIAG_UMEM:
			umem := &XDPDiagUmem{}
			if err := umem.deserialize(a.Value); err != nil {
				return nil, err
			}
			resp.XDPInfo.Umem = umem
		case XDP_DIAG_STATS:
			stats := &XDPDiagStats{}
			if err := stats.deserialize(a.Value); err != nil {
				return nil, err
			}
			resp.XDPInfo.Stats = stats
		}
	}
	return resp, nil
}
