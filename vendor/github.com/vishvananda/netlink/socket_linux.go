package netlink

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	sizeofSocketID          = 0x30
	sizeofSocketRequest     = sizeofSocketID + 0x8
	sizeofSocket            = sizeofSocketID + 0x18
	sizeofUnixSocketRequest = 0x18 // 24 byte
	sizeofUnixSocket        = 0x10 // 16 byte
)

type socketRequest struct {
	Family   uint8
	Protocol uint8
	Ext      uint8
	pad      uint8
	States   uint32
	ID       SocketID
}

type writeBuffer struct {
	Bytes []byte
	pos   int
}

func (b *writeBuffer) Write(c byte) {
	b.Bytes[b.pos] = c
	b.pos++
}

func (b *writeBuffer) Next(n int) []byte {
	s := b.Bytes[b.pos : b.pos+n]
	b.pos += n
	return s
}

func (r *socketRequest) Serialize() []byte {
	b := writeBuffer{Bytes: make([]byte, sizeofSocketRequest)}
	b.Write(r.Family)
	b.Write(r.Protocol)
	b.Write(r.Ext)
	b.Write(r.pad)
	native.PutUint32(b.Next(4), r.States)
	networkOrder.PutUint16(b.Next(2), r.ID.SourcePort)
	networkOrder.PutUint16(b.Next(2), r.ID.DestinationPort)
	if r.Family == unix.AF_INET6 {
		copy(b.Next(16), r.ID.Source)
		copy(b.Next(16), r.ID.Destination)
	} else {
		copy(b.Next(16), r.ID.Source.To4())
		copy(b.Next(16), r.ID.Destination.To4())
	}
	native.PutUint32(b.Next(4), r.ID.Interface)
	native.PutUint32(b.Next(4), r.ID.Cookie[0])
	native.PutUint32(b.Next(4), r.ID.Cookie[1])
	return b.Bytes
}

func (r *socketRequest) Len() int { return sizeofSocketRequest }

// According to linux/include/uapi/linux/unix_diag.h
type unixSocketRequest struct {
	Family   uint8
	Protocol uint8
	pad      uint16
	States   uint32
	INode    uint32
	Show     uint32
	Cookie   [2]uint32
}

func (r *unixSocketRequest) Serialize() []byte {
	b := writeBuffer{Bytes: make([]byte, sizeofUnixSocketRequest)}
	b.Write(r.Family)
	b.Write(r.Protocol)
	native.PutUint16(b.Next(2), r.pad)
	native.PutUint32(b.Next(4), r.States)
	native.PutUint32(b.Next(4), r.INode)
	native.PutUint32(b.Next(4), r.Show)
	native.PutUint32(b.Next(4), r.Cookie[0])
	native.PutUint32(b.Next(4), r.Cookie[1])
	return b.Bytes
}

func (r *unixSocketRequest) Len() int { return sizeofUnixSocketRequest }

type readBuffer struct {
	Bytes []byte
	pos   int
}

func (b *readBuffer) Read() byte {
	c := b.Bytes[b.pos]
	b.pos++
	return c
}

func (b *readBuffer) Next(n int) []byte {
	s := b.Bytes[b.pos : b.pos+n]
	b.pos += n
	return s
}

func (s *Socket) deserialize(b []byte) error {
	if len(b) < sizeofSocket {
		return fmt.Errorf("socket data short read (%d); want %d", len(b), sizeofSocket)
	}
	rb := readBuffer{Bytes: b}
	s.Family = rb.Read()
	s.State = rb.Read()
	s.Timer = rb.Read()
	s.Retrans = rb.Read()
	s.ID.SourcePort = networkOrder.Uint16(rb.Next(2))
	s.ID.DestinationPort = networkOrder.Uint16(rb.Next(2))
	if s.Family == unix.AF_INET6 {
		s.ID.Source = net.IP(rb.Next(16))
		s.ID.Destination = net.IP(rb.Next(16))
	} else {
		s.ID.Source = net.IPv4(rb.Read(), rb.Read(), rb.Read(), rb.Read())
		rb.Next(12)
		s.ID.Destination = net.IPv4(rb.Read(), rb.Read(), rb.Read(), rb.Read())
		rb.Next(12)
	}
	s.ID.Interface = native.Uint32(rb.Next(4))
	s.ID.Cookie[0] = native.Uint32(rb.Next(4))
	s.ID.Cookie[1] = native.Uint32(rb.Next(4))
	s.Expires = native.Uint32(rb.Next(4))
	s.RQueue = native.Uint32(rb.Next(4))
	s.WQueue = native.Uint32(rb.Next(4))
	s.UID = native.Uint32(rb.Next(4))
	s.INode = native.Uint32(rb.Next(4))
	return nil
}

func (u *UnixSocket) deserialize(b []byte) error {
	if len(b) < sizeofUnixSocket {
		return fmt.Errorf("unix diag data short read (%d); want %d", len(b), sizeofUnixSocket)
	}
	rb := readBuffer{Bytes: b}
	u.Type = rb.Read()
	u.Family = rb.Read()
	u.State = rb.Read()
	u.pad = rb.Read()
	u.INode = native.Uint32(rb.Next(4))
	u.Cookie[0] = native.Uint32(rb.Next(4))
	u.Cookie[1] = native.Uint32(rb.Next(4))
	return nil
}

// SocketGet returns the Socket identified by its local and remote addresses.
//
// If the returned error is [ErrDumpInterrupted], the search for a result may
// be incomplete and the caller should retry.
func (h *Handle) SocketGet(local, remote net.Addr) (*Socket, error) {
	var protocol uint8
	var localIP, remoteIP net.IP
	var localPort, remotePort uint16
	switch l := local.(type) {
	case *net.TCPAddr:
		r, ok := remote.(*net.TCPAddr)
		if !ok {
			return nil, ErrNotImplemented
		}
		localIP = l.IP
		localPort = uint16(l.Port)
		remoteIP = r.IP
		remotePort = uint16(r.Port)
		protocol = unix.IPPROTO_TCP
	case *net.UDPAddr:
		r, ok := remote.(*net.UDPAddr)
		if !ok {
			return nil, ErrNotImplemented
		}
		localIP = l.IP
		localPort = uint16(l.Port)
		remoteIP = r.IP
		remotePort = uint16(r.Port)
		protocol = unix.IPPROTO_UDP
	default:
		return nil, ErrNotImplemented
	}

	var family uint8
	if localIP.To4() != nil && remoteIP.To4() != nil {
		family = unix.AF_INET
	}

	if family == 0 && localIP.To16() != nil && remoteIP.To16() != nil {
		family = unix.AF_INET6
	}

	if family == 0 {
		return nil, ErrNotImplemented
	}

	req := h.newNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&socketRequest{
		Family:   family,
		Protocol: protocol,
		States:   0xffffffff,
		ID: SocketID{
			SourcePort:      localPort,
			DestinationPort: remotePort,
			Source:          localIP,
			Destination:     remoteIP,
			Cookie:          [2]uint32{nl.TCPDIAG_NOCOOKIE, nl.TCPDIAG_NOCOOKIE},
		},
	})

	msgs, err := req.Execute(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New("no message nor error from netlink")
	}
	if len(msgs) > 2 {
		return nil, fmt.Errorf("multiple (%d) matching sockets", len(msgs))
	}

	sock := &Socket{}
	if err := sock.deserialize(msgs[0]); err != nil {
		return nil, err
	}
	return sock, nil
}

// SocketGet returns the Socket identified by its local and remote addresses.
//
// If the returned error is [ErrDumpInterrupted], the search for a result may
// be incomplete and the caller should retry.
func SocketGet(local, remote net.Addr) (*Socket, error) {
	return pkgHandle.SocketGet(local, remote)
}

// SocketDestroy kills the Socket identified by its local and remote addresses.
func (h *Handle) SocketDestroy(local, remote net.Addr) error {
	localTCP, ok := local.(*net.TCPAddr)
	if !ok {
		return ErrNotImplemented
	}
	remoteTCP, ok := remote.(*net.TCPAddr)
	if !ok {
		return ErrNotImplemented
	}
	localIP := localTCP.IP.To4()
	if localIP == nil {
		return ErrNotImplemented
	}
	remoteIP := remoteTCP.IP.To4()
	if remoteIP == nil {
		return ErrNotImplemented
	}

	s, err := nl.Subscribe(unix.NETLINK_INET_DIAG)
	if err != nil {
		return err
	}
	defer s.Close()
	req := h.newNetlinkRequest(nl.SOCK_DESTROY, unix.NLM_F_ACK)
	req.AddData(&socketRequest{
		Family:   unix.AF_INET,
		Protocol: unix.IPPROTO_TCP,
		ID: SocketID{
			SourcePort:      uint16(localTCP.Port),
			DestinationPort: uint16(remoteTCP.Port),
			Source:          localIP,
			Destination:     remoteIP,
			Cookie:          [2]uint32{nl.TCPDIAG_NOCOOKIE, nl.TCPDIAG_NOCOOKIE},
		},
	})

	_, err = req.Execute(unix.NETLINK_INET_DIAG, 0)
	return err
}

// SocketDestroy kills the Socket identified by its local and remote addresses.
func SocketDestroy(local, remote net.Addr) error {
	return pkgHandle.SocketDestroy(local, remote)
}

// SocketDiagTCPInfo requests INET_DIAG_INFO for TCP protocol for specified family type and return with extension TCP info.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) SocketDiagTCPInfo(family uint8) ([]*InetDiagTCPInfoResp, error) {
	// Construct the request
	req := h.newNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&socketRequest{
		Family:   family,
		Protocol: unix.IPPROTO_TCP,
		Ext:      (1 << (INET_DIAG_VEGASINFO - 1)) | (1 << (INET_DIAG_INFO - 1)),
		States:   uint32(0xfff), // all states
	})

	// Do the query and parse the result
	var result []*InetDiagTCPInfoResp
	executeErr := req.ExecuteIter(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY, func(msg []byte) bool {
		sockInfo := &Socket{}
		var err error
		if err = sockInfo.deserialize(msg); err != nil {
			return false
		}
		var attrs []syscall.NetlinkRouteAttr
		if attrs, err = nl.ParseRouteAttr(msg[sizeofSocket:]); err != nil {
			return false
		}

		var res *InetDiagTCPInfoResp
		if res, err = attrsToInetDiagTCPInfoResp(attrs, sockInfo); err != nil {
			return false
		}

		result = append(result, res)
		return true
	})

	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	return result, executeErr
}

// SocketDiagTCPInfo requests INET_DIAG_INFO for TCP protocol for specified family type and return with extension TCP info.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func SocketDiagTCPInfo(family uint8) ([]*InetDiagTCPInfoResp, error) {
	return pkgHandle.SocketDiagTCPInfo(family)
}

// SocketDiagTCP requests INET_DIAG_INFO for TCP protocol for specified family type and return related socket.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) SocketDiagTCP(family uint8) ([]*Socket, error) {
	// Construct the request
	req := h.newNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&socketRequest{
		Family:   family,
		Protocol: unix.IPPROTO_TCP,
		Ext:      (1 << (INET_DIAG_VEGASINFO - 1)) | (1 << (INET_DIAG_INFO - 1)),
		States:   uint32(0xfff), // all states
	})

	// Do the query and parse the result
	var result []*Socket
	executeErr := req.ExecuteIter(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY, func(msg []byte) bool {
		sockInfo := &Socket{}
		if err := sockInfo.deserialize(msg); err != nil {
			return false
		}
		result = append(result, sockInfo)
		return true
	})
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	return result, executeErr
}

// SocketDiagTCP requests INET_DIAG_INFO for TCP protocol for specified family type and return related socket.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func SocketDiagTCP(family uint8) ([]*Socket, error) {
	return pkgHandle.SocketDiagTCP(family)
}

// SocketDiagUDPInfo requests INET_DIAG_INFO for UDP protocol for specified family type and return with extension info.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) SocketDiagUDPInfo(family uint8) ([]*InetDiagUDPInfoResp, error) {
	// Construct the request
	var extensions uint8
	extensions = 1 << (INET_DIAG_VEGASINFO - 1)
	extensions |= 1 << (INET_DIAG_INFO - 1)
	extensions |= 1 << (INET_DIAG_MEMINFO - 1)

	req := h.newNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&socketRequest{
		Family:   family,
		Protocol: unix.IPPROTO_UDP,
		Ext:      extensions,
		States:   uint32(0xfff), // all states
	})

	// Do the query and parse the result
	var result []*InetDiagUDPInfoResp
	executeErr := req.ExecuteIter(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY, func(msg []byte) bool {
		sockInfo := &Socket{}
		if err := sockInfo.deserialize(msg); err != nil {
			return false
		}

		var attrs []syscall.NetlinkRouteAttr
		var err error
		if attrs, err = nl.ParseRouteAttr(msg[sizeofSocket:]); err != nil {
			return false
		}

		var res *InetDiagUDPInfoResp
		if res, err = attrsToInetDiagUDPInfoResp(attrs, sockInfo); err != nil {
			return false
		}

		result = append(result, res)
		return true
	})
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	return result, executeErr
}

// SocketDiagUDPInfo requests INET_DIAG_INFO for UDP protocol for specified family type and return with extension info.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func SocketDiagUDPInfo(family uint8) ([]*InetDiagUDPInfoResp, error) {
	return pkgHandle.SocketDiagUDPInfo(family)
}

// SocketDiagUDP requests INET_DIAG_INFO for UDP protocol for specified family type and return related socket.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) SocketDiagUDP(family uint8) ([]*Socket, error) {
	// Construct the request
	req := h.newNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&socketRequest{
		Family:   family,
		Protocol: unix.IPPROTO_UDP,
		Ext:      (1 << (INET_DIAG_VEGASINFO - 1)) | (1 << (INET_DIAG_INFO - 1)),
		States:   uint32(0xfff), // all states
	})

	// Do the query and parse the result
	var result []*Socket
	executeErr := req.ExecuteIter(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY, func(msg []byte) bool {
		sockInfo := &Socket{}
		if err := sockInfo.deserialize(msg); err != nil {
			return false
		}
		result = append(result, sockInfo)
		return true
	})
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	return result, executeErr
}

// SocketDiagUDP requests INET_DIAG_INFO for UDP protocol for specified family type and return related socket.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func SocketDiagUDP(family uint8) ([]*Socket, error) {
	return pkgHandle.SocketDiagUDP(family)
}

// UnixSocketDiagInfo requests UNIX_DIAG_INFO for unix sockets and return with extension info.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) UnixSocketDiagInfo() ([]*UnixDiagInfoResp, error) {
	// Construct the request
	var extensions uint8
	extensions = 1 << UNIX_DIAG_NAME
	extensions |= 1 << UNIX_DIAG_PEER
	extensions |= 1 << UNIX_DIAG_RQLEN
	req := h.newNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&unixSocketRequest{
		Family: unix.AF_UNIX,
		States: ^uint32(0), // all states
		Show:   uint32(extensions),
	})

	var result []*UnixDiagInfoResp
	executeErr := req.ExecuteIter(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY, func(msg []byte) bool {
		sockInfo := &UnixSocket{}
		if err := sockInfo.deserialize(msg); err != nil {
			return false
		}

		// Diagnosis also delivers sockets with AF_INET family, filter those
		if sockInfo.Family != unix.AF_UNIX {
			return false
		}

		var attrs []syscall.NetlinkRouteAttr
		var err error
		if attrs, err = nl.ParseRouteAttr(msg[sizeofSocket:]); err != nil {
			return false
		}

		var res *UnixDiagInfoResp
		if res, err = attrsToUnixDiagInfoResp(attrs, sockInfo); err != nil {
			return false
		}
		result = append(result, res)
		return true
	})
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	return result, executeErr
}

// UnixSocketDiagInfo requests UNIX_DIAG_INFO for unix sockets and return with extension info.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func UnixSocketDiagInfo() ([]*UnixDiagInfoResp, error) {
	return pkgHandle.UnixSocketDiagInfo()
}

// UnixSocketDiag requests UNIX_DIAG_INFO for unix sockets.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) UnixSocketDiag() ([]*UnixSocket, error) {
	// Construct the request
	req := h.newNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	req.AddData(&unixSocketRequest{
		Family: unix.AF_UNIX,
		States: ^uint32(0), // all states
	})

	var result []*UnixSocket
	executeErr := req.ExecuteIter(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY, func(msg []byte) bool {
		sockInfo := &UnixSocket{}
		if err := sockInfo.deserialize(msg); err != nil {
			return false
		}

		// Diagnosis also delivers sockets with AF_INET family, filter those
		if sockInfo.Family == unix.AF_UNIX {
			result = append(result, sockInfo)
		}
		return true
	})
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	return result, executeErr
}

// UnixSocketDiag requests UNIX_DIAG_INFO for unix sockets.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func UnixSocketDiag() ([]*UnixSocket, error) {
	return pkgHandle.UnixSocketDiag()
}

func attrsToInetDiagTCPInfoResp(attrs []syscall.NetlinkRouteAttr, sockInfo *Socket) (*InetDiagTCPInfoResp, error) {
	info := &InetDiagTCPInfoResp{
		InetDiagMsg: sockInfo,
	}
	for _, a := range attrs {
		switch a.Attr.Type {
		case INET_DIAG_INFO:
			info.TCPInfo = &TCPInfo{}
			if err := info.TCPInfo.deserialize(a.Value); err != nil {
				return nil, err
			}
		case INET_DIAG_BBRINFO:
			info.TCPBBRInfo = &TCPBBRInfo{}
			if err := info.TCPBBRInfo.deserialize(a.Value); err != nil {
				return nil, err
			}
		}
	}

	return info, nil
}

func attrsToInetDiagUDPInfoResp(attrs []syscall.NetlinkRouteAttr, sockInfo *Socket) (*InetDiagUDPInfoResp, error) {
	info := &InetDiagUDPInfoResp{
		InetDiagMsg: sockInfo,
	}
	for _, a := range attrs {
		switch a.Attr.Type {
		case INET_DIAG_MEMINFO:
			info.Memory = &MemInfo{}
			if err := info.Memory.deserialize(a.Value); err != nil {
				return nil, err
			}
		}
	}

	return info, nil
}

func attrsToUnixDiagInfoResp(attrs []syscall.NetlinkRouteAttr, sockInfo *UnixSocket) (*UnixDiagInfoResp, error) {
	info := &UnixDiagInfoResp{
		DiagMsg: sockInfo,
	}
	for _, a := range attrs {
		switch a.Attr.Type {
		case UNIX_DIAG_NAME:
			name := string(a.Value[:a.Attr.Len])
			info.Name = &name
		case UNIX_DIAG_PEER:
			peer := native.Uint32(a.Value)
			info.Peer = &peer
		case UNIX_DIAG_RQLEN:
			info.Queue = &QueueInfo{
				RQueue: native.Uint32(a.Value[:4]),
				WQueue: native.Uint32(a.Value[4:]),
			}
			// default:
			// 	fmt.Println("unknown unix attribute type", a.Attr.Type, "with data", a.Value)
		}
	}

	return info, nil
}
