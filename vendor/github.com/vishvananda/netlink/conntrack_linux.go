package netlink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// ConntrackTableType Conntrack table for the netlink operation
type ConntrackTableType uint8

const (
	// ConntrackTable Conntrack table
	// https://github.com/torvalds/linux/blob/master/include/uapi/linux/netfilter/nfnetlink.h -> #define NFNL_SUBSYS_CTNETLINK		 1
	ConntrackTable = 1
	// ConntrackExpectTable Conntrack expect table
	// https://github.com/torvalds/linux/blob/master/include/uapi/linux/netfilter/nfnetlink.h -> #define NFNL_SUBSYS_CTNETLINK_EXP 2
	ConntrackExpectTable = 2
)

const (
	// backward compatibility with golang 1.6 which does not have io.SeekCurrent
	seekCurrent = 1
)

// InetFamily Family type
type InetFamily uint8

//  -L [table] [options]          List conntrack or expectation table
//  -G [table] parameters         Get conntrack or expectation

//  -I [table] parameters         Create a conntrack or expectation
//  -U [table] parameters         Update a conntrack
//  -E [table] [options]          Show events

//  -C [table]                    Show counter
//  -S                            Show statistics

// ConntrackTableList returns the flow list of a table of a specific family
// conntrack -L [table] [options]          List conntrack or expectation table
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func ConntrackTableList(table ConntrackTableType, family InetFamily) ([]*ConntrackFlow, error) {
	return pkgHandle.ConntrackTableList(table, family)
}

// ConntrackTableFlush flushes all the flows of a specified table
// conntrack -F [table]            Flush table
// The flush operation applies to all the family types
func ConntrackTableFlush(table ConntrackTableType) error {
	return pkgHandle.ConntrackTableFlush(table)
}

// ConntrackCreate creates a new conntrack flow in the desired table
// conntrack -I [table]		Create a conntrack or expectation
func ConntrackCreate(table ConntrackTableType, family InetFamily, flow *ConntrackFlow) error {
	return pkgHandle.ConntrackCreate(table, family, flow)
}

// ConntrackUpdate updates an existing conntrack flow in the desired table using the handle
// conntrack -U [table]		Update a conntrack
func ConntrackUpdate(table ConntrackTableType, family InetFamily, flow *ConntrackFlow) error {
	return pkgHandle.ConntrackUpdate(table, family, flow)
}

// ConntrackDeleteFilter deletes entries on the specified table on the base of the filter
// conntrack -D [table] parameters         Delete conntrack or expectation
//
// Deprecated: use [ConntrackDeleteFilters] instead.
func ConntrackDeleteFilter(table ConntrackTableType, family InetFamily, filter CustomConntrackFilter) (uint, error) {
	return pkgHandle.ConntrackDeleteFilters(table, family, filter)
}

// ConntrackDeleteFilters deletes entries on the specified table matching any of the specified filters
// conntrack -D [table] parameters         Delete conntrack or expectation
func ConntrackDeleteFilters(table ConntrackTableType, family InetFamily, filters ...CustomConntrackFilter) (uint, error) {
	return pkgHandle.ConntrackDeleteFilters(table, family, filters...)
}

// ConntrackTableList returns the flow list of a table of a specific family using the netlink handle passed
// conntrack -L [table] [options]          List conntrack or expectation table
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) ConntrackTableList(table ConntrackTableType, family InetFamily) ([]*ConntrackFlow, error) {
	res, executeErr := h.dumpConntrackTable(table, family)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}

	// Deserialize all the flows
	var result []*ConntrackFlow
	for _, dataRaw := range res {
		result = append(result, parseRawData(dataRaw))
	}

	return result, executeErr
}

// ConntrackTableFlush flushes all the flows of a specified table using the netlink handle passed
// conntrack -F [table]            Flush table
// The flush operation applies to all the family types
func (h *Handle) ConntrackTableFlush(table ConntrackTableType) error {
	req := h.newConntrackRequest(table, unix.AF_INET, nl.IPCTNL_MSG_CT_DELETE, unix.NLM_F_ACK)
	_, err := req.Execute(unix.NETLINK_NETFILTER, 0)
	return err
}

// ConntrackCreate creates a new conntrack flow in the desired table using the handle
// conntrack -I [table]		Create a conntrack or expectation
func (h *Handle) ConntrackCreate(table ConntrackTableType, family InetFamily, flow *ConntrackFlow) error {
	req := h.newConntrackRequest(table, family, nl.IPCTNL_MSG_CT_NEW, unix.NLM_F_ACK|unix.NLM_F_CREATE)
	attr, err := flow.toNlData()
	if err != nil {
		return err
	}

	for _, a := range attr {
		req.AddData(a)
	}

	_, err = req.Execute(unix.NETLINK_NETFILTER, 0)
	return err
}

// ConntrackUpdate updates an existing conntrack flow in the desired table using the handle
// conntrack -U [table]		Update a conntrack
func (h *Handle) ConntrackUpdate(table ConntrackTableType, family InetFamily, flow *ConntrackFlow) error {
	req := h.newConntrackRequest(table, family, nl.IPCTNL_MSG_CT_NEW, unix.NLM_F_ACK|unix.NLM_F_REPLACE)
	attr, err := flow.toNlData()
	if err != nil {
		return err
	}

	for _, a := range attr {
		req.AddData(a)
	}

	_, err = req.Execute(unix.NETLINK_NETFILTER, 0)
	return err
}

// ConntrackDeleteFilter deletes entries on the specified table on the base of the filter using the netlink handle passed
// conntrack -D [table] parameters         Delete conntrack or expectation
//
// Deprecated: use [Handle.ConntrackDeleteFilters] instead.
func (h *Handle) ConntrackDeleteFilter(table ConntrackTableType, family InetFamily, filter CustomConntrackFilter) (uint, error) {
	return h.ConntrackDeleteFilters(table, family, filter)
}

// ConntrackDeleteFilters deletes entries on the specified table matching any of the specified filters using the netlink handle passed
// conntrack -D [table] parameters         Delete conntrack or expectation
func (h *Handle) ConntrackDeleteFilters(table ConntrackTableType, family InetFamily, filters ...CustomConntrackFilter) (uint, error) {
	var errMsgs []string
	res, err := h.dumpConntrackTable(table, family)
	if err != nil {
		if !errors.Is(err, ErrDumpInterrupted) {
			return 0, err
		}
		// This allows us to at least do a best effort to try to clean the
		// entries matching the filter.
		errMsgs = append(errMsgs, err.Error())
	}

	var matched uint
	for _, dataRaw := range res {
		flow := parseRawData(dataRaw)
		for _, filter := range filters {
			if match := filter.MatchConntrackFlow(flow); match {
				req2 := h.newConntrackRequest(table, family, nl.IPCTNL_MSG_CT_DELETE, unix.NLM_F_ACK)
				// skip the first 4 byte that are the netfilter header, the newConntrackRequest is adding it already
				req2.AddRawData(dataRaw[4:])
				if _, err = req2.Execute(unix.NETLINK_NETFILTER, 0); err == nil {
					matched++
					// flow is already deleted, no need to match on other filters and continue to the next flow.
					break
				}
				errMsgs = append(errMsgs, fmt.Sprintf("failed to delete conntrack flow '%s': %s", flow.String(), err.Error()))
			}
		}
	}
	if len(errMsgs) > 0 {
		return matched, fmt.Errorf(strings.Join(errMsgs, "; "))
	}
	return matched, nil
}

func (h *Handle) newConntrackRequest(table ConntrackTableType, family InetFamily, operation, flags int) *nl.NetlinkRequest {
	// Create the Netlink request object
	req := h.newNetlinkRequest((int(table)<<8)|operation, flags)
	// Add the netfilter header
	msg := &nl.Nfgenmsg{
		NfgenFamily: uint8(family),
		Version:     nl.NFNETLINK_V0,
		ResId:       0,
	}
	req.AddData(msg)
	return req
}

func (h *Handle) dumpConntrackTable(table ConntrackTableType, family InetFamily) ([][]byte, error) {
	req := h.newConntrackRequest(table, family, nl.IPCTNL_MSG_CT_GET, unix.NLM_F_DUMP)
	return req.Execute(unix.NETLINK_NETFILTER, 0)
}

// ProtoInfo wraps an L4-protocol structure - roughly corresponds to the
// __nfct_protoinfo union found in libnetfilter_conntrack/include/internal/object.h.
// Currently, only protocol names, and TCP state is supported.
type ProtoInfo interface {
	Protocol() string
}

// ProtoInfoTCP corresponds to the `tcp` struct of the __nfct_protoinfo union.
// Only TCP state is currently supported.
type ProtoInfoTCP struct {
	State uint8
}
// Protocol returns "tcp".
func (*ProtoInfoTCP) Protocol() string {return "tcp"}
func (p *ProtoInfoTCP) toNlData() ([]*nl.RtAttr, error) {
	ctProtoInfo := nl.NewRtAttr(unix.NLA_F_NESTED | nl.CTA_PROTOINFO, []byte{})
	ctProtoInfoTCP := nl.NewRtAttr(unix.NLA_F_NESTED|nl.CTA_PROTOINFO_TCP, []byte{})
	ctProtoInfoTCPState := nl.NewRtAttr(nl.CTA_PROTOINFO_TCP_STATE, nl.Uint8Attr(p.State))
	ctProtoInfoTCP.AddChild(ctProtoInfoTCPState)
	ctProtoInfo.AddChild(ctProtoInfoTCP)

	return []*nl.RtAttr{ctProtoInfo}, nil
}

// ProtoInfoSCTP only supports the protocol name.
type ProtoInfoSCTP struct {}
// Protocol returns "sctp".
func (*ProtoInfoSCTP) Protocol() string {return "sctp"}

// ProtoInfoDCCP only supports the protocol name.
type ProtoInfoDCCP struct {}
// Protocol returns "dccp".
func (*ProtoInfoDCCP) Protocol() string {return "dccp"}

// The full conntrack flow structure is very complicated and can be found in the file:
// http://git.netfilter.org/libnetfilter_conntrack/tree/include/internal/object.h
// For the time being, the structure below allows to parse and extract the base information of a flow
type IPTuple struct {
	Bytes    uint64
	DstIP    net.IP
	DstPort  uint16
	Packets  uint64
	Protocol uint8
	SrcIP    net.IP
	SrcPort  uint16
}

// toNlData generates the inner fields of a nested tuple netlink datastructure
// does not generate the "nested"-flagged outer message.
func (t *IPTuple) toNlData(family uint8) ([]*nl.RtAttr, error) {

	var srcIPsFlag, dstIPsFlag int
	if family == nl.FAMILY_V4 {
		srcIPsFlag = nl.CTA_IP_V4_SRC
		dstIPsFlag = nl.CTA_IP_V4_DST
	} else if family == nl.FAMILY_V6 {
		srcIPsFlag = nl.CTA_IP_V6_SRC
		dstIPsFlag = nl.CTA_IP_V6_DST
	} else {
		return []*nl.RtAttr{}, fmt.Errorf("couldn't generate netlink message for tuple due to unrecognized FamilyType '%d'", family)
	}

	ctTupleIP := nl.NewRtAttr(unix.NLA_F_NESTED|nl.CTA_TUPLE_IP, nil)
	ctTupleIPSrc := nl.NewRtAttr(srcIPsFlag, t.SrcIP)
	ctTupleIP.AddChild(ctTupleIPSrc)
	ctTupleIPDst := nl.NewRtAttr(dstIPsFlag, t.DstIP)
	ctTupleIP.AddChild(ctTupleIPDst)

	ctTupleProto := nl.NewRtAttr(unix.NLA_F_NESTED|nl.CTA_TUPLE_PROTO, nil)
	ctTupleProtoNum := nl.NewRtAttr(nl.CTA_PROTO_NUM, []byte{t.Protocol})
	ctTupleProto.AddChild(ctTupleProtoNum)
	ctTupleProtoSrcPort := nl.NewRtAttr(nl.CTA_PROTO_SRC_PORT, nl.BEUint16Attr(t.SrcPort))
	ctTupleProto.AddChild(ctTupleProtoSrcPort)
	ctTupleProtoDstPort := nl.NewRtAttr(nl.CTA_PROTO_DST_PORT, nl.BEUint16Attr(t.DstPort))
	ctTupleProto.AddChild(ctTupleProtoDstPort, )

	return []*nl.RtAttr{ctTupleIP, ctTupleProto}, nil
}

type ConntrackFlow struct {
	FamilyType uint8
	Forward    IPTuple
	Reverse    IPTuple
	Mark       uint32
	Zone       uint16
	TimeStart  uint64
	TimeStop   uint64
	TimeOut    uint32
	Labels     []byte
	ProtoInfo  ProtoInfo
}

func (s *ConntrackFlow) String() string {
	// conntrack cmd output:
	// udp      17 src=127.0.0.1 dst=127.0.0.1 sport=4001 dport=1234 packets=5 bytes=532 [UNREPLIED] src=127.0.0.1 dst=127.0.0.1 sport=1234 dport=4001 packets=10 bytes=1078 mark=0 labels=0x00000000050012ac4202010000000000 zone=100
	//             start=2019-07-26 01:26:21.557800506 +0000 UTC stop=1970-01-01 00:00:00 +0000 UTC timeout=30(sec)
	start := time.Unix(0, int64(s.TimeStart))
	stop := time.Unix(0, int64(s.TimeStop))
	timeout := int32(s.TimeOut)
	res := fmt.Sprintf("%s\t%d src=%s dst=%s sport=%d dport=%d packets=%d bytes=%d\tsrc=%s dst=%s sport=%d dport=%d packets=%d bytes=%d mark=0x%x ",
		nl.L4ProtoMap[s.Forward.Protocol], s.Forward.Protocol,
		s.Forward.SrcIP.String(), s.Forward.DstIP.String(), s.Forward.SrcPort, s.Forward.DstPort, s.Forward.Packets, s.Forward.Bytes,
		s.Reverse.SrcIP.String(), s.Reverse.DstIP.String(), s.Reverse.SrcPort, s.Reverse.DstPort, s.Reverse.Packets, s.Reverse.Bytes,
		s.Mark)
	if len(s.Labels) > 0 {
		res += fmt.Sprintf("labels=0x%x ", s.Labels)
	}
	if s.Zone != 0 {
		res += fmt.Sprintf("zone=%d ", s.Zone)
	}
	res += fmt.Sprintf("start=%v stop=%v timeout=%d(sec)", start, stop, timeout)
	return res
}

// toNlData generates netlink messages representing the flow.
func (s *ConntrackFlow) toNlData() ([]*nl.RtAttr, error) {
	var payload []*nl.RtAttr
	// The message structure is built as follows:
	//	<len, NLA_F_NESTED|CTA_TUPLE_ORIG>
	//		<len, NLA_F_NESTED|CTA_TUPLE_IP>
	//			<len, [CTA_IP_V4_SRC|CTA_IP_V6_SRC]>
	//			<IP>
	//			<len, [CTA_IP_V4_DST|CTA_IP_V6_DST]>
	//			<IP>
	//		<len, NLA_F_NESTED|nl.CTA_TUPLE_PROTO>
	//			<len, CTA_PROTO_NUM>
	//			<uint8>
	//			<len, CTA_PROTO_SRC_PORT>
	//			<BEuint16>
	//			<len, CTA_PROTO_DST_PORT>
	//			<BEuint16>
	// 	<len, NLA_F_NESTED|CTA_TUPLE_REPLY>
	//		<len, NLA_F_NESTED|CTA_TUPLE_IP>
	//			<len, [CTA_IP_V4_SRC|CTA_IP_V6_SRC]>
	//			<IP>
	//			<len, [CTA_IP_V4_DST|CTA_IP_V6_DST]>
	//			<IP>
	//		<len, NLA_F_NESTED|nl.CTA_TUPLE_PROTO>
	//			<len, CTA_PROTO_NUM>
	//			<uint8>
	//			<len, CTA_PROTO_SRC_PORT>
	//			<BEuint16>
	//			<len, CTA_PROTO_DST_PORT>
	//			<BEuint16>
	//	<len, CTA_STATUS>
	//	<uint64>
	//	<len, CTA_MARK>
	//	<BEuint64>
	//	<len, CTA_TIMEOUT>
	//	<BEuint64>
	//	<len, NLA_F_NESTED|CTA_PROTOINFO>
 
	// CTA_TUPLE_ORIG
	ctTupleOrig := nl.NewRtAttr(unix.NLA_F_NESTED|nl.CTA_TUPLE_ORIG, nil)
	forwardFlowAttrs, err := s.Forward.toNlData(s.FamilyType)
	if err != nil {
		return nil, fmt.Errorf("couldn't generate netlink data for conntrack forward flow: %w", err)
	}
	for _, a := range forwardFlowAttrs {
		ctTupleOrig.AddChild(a)
	}

	// CTA_TUPLE_REPLY
	ctTupleReply := nl.NewRtAttr(unix.NLA_F_NESTED|nl.CTA_TUPLE_REPLY, nil)
	reverseFlowAttrs, err := s.Reverse.toNlData(s.FamilyType)
	if err != nil {
		return nil, fmt.Errorf("couldn't generate netlink data for conntrack reverse flow: %w", err)
	}
	for _, a := range reverseFlowAttrs {
		ctTupleReply.AddChild(a)
	}

	ctMark := nl.NewRtAttr(nl.CTA_MARK, nl.BEUint32Attr(s.Mark))
	ctTimeout := nl.NewRtAttr(nl.CTA_TIMEOUT, nl.BEUint32Attr(s.TimeOut))

	payload = append(payload, ctTupleOrig, ctTupleReply, ctMark, ctTimeout)

	if s.ProtoInfo != nil {
		switch p := s.ProtoInfo.(type) {
		case *ProtoInfoTCP:
			attrs, err := p.toNlData()
			if err != nil {
				return nil, fmt.Errorf("couldn't generate netlink data for conntrack flow's TCP protoinfo: %w", err)
			}
			payload = append(payload, attrs...)
		default:
			return nil, errors.New("couldn't generate netlink data for conntrack: field 'ProtoInfo' only supports TCP or nil")
		}
	}

	return payload, nil
}

// This method parse the ip tuple structure
// The message structure is the following:
// <len, [CTA_IP_V4_SRC|CTA_IP_V6_SRC], 16 bytes for the IP>
// <len, [CTA_IP_V4_DST|CTA_IP_V6_DST], 16 bytes for the IP>
// <len, NLA_F_NESTED|nl.CTA_TUPLE_PROTO, 1 byte for the protocol, 3 bytes of padding>
// <len, CTA_PROTO_SRC_PORT, 2 bytes for the source port, 2 bytes of padding>
// <len, CTA_PROTO_DST_PORT, 2 bytes for the source port, 2 bytes of padding>
func parseIpTuple(reader *bytes.Reader, tpl *IPTuple) uint8 {
	for i := 0; i < 2; i++ {
		_, t, _, v := parseNfAttrTLV(reader)
		switch t {
		case nl.CTA_IP_V4_SRC, nl.CTA_IP_V6_SRC:
			tpl.SrcIP = v
		case nl.CTA_IP_V4_DST, nl.CTA_IP_V6_DST:
			tpl.DstIP = v
		}
	}
	// Get total length of nested protocol-specific info.
	_, _, protoInfoTotalLen := parseNfAttrTL(reader)
	_, t, l, v := parseNfAttrTLV(reader)
	// Track the number of bytes read.
	protoInfoBytesRead := uint16(nl.SizeofNfattr) + l
	if t == nl.CTA_PROTO_NUM {
		tpl.Protocol = uint8(v[0])
	}
	// We only parse TCP & UDP headers. Skip the others.
	if tpl.Protocol != unix.IPPROTO_TCP && tpl.Protocol != unix.IPPROTO_UDP {
		// skip the rest
		bytesRemaining := protoInfoTotalLen - protoInfoBytesRead
		reader.Seek(int64(bytesRemaining), seekCurrent)
		return tpl.Protocol
	}
	// Skip 3 bytes of padding
	reader.Seek(3, seekCurrent)
	protoInfoBytesRead += 3
	for i := 0; i < 2; i++ {
		_, t, _ := parseNfAttrTL(reader)
		protoInfoBytesRead += uint16(nl.SizeofNfattr)
		switch t {
		case nl.CTA_PROTO_SRC_PORT:
			parseBERaw16(reader, &tpl.SrcPort)
			protoInfoBytesRead += 2
		case nl.CTA_PROTO_DST_PORT:
			parseBERaw16(reader, &tpl.DstPort)
			protoInfoBytesRead += 2
		}
		// Skip 2 bytes of padding
		reader.Seek(2, seekCurrent)
		protoInfoBytesRead += 2
	}
	// Skip any remaining/unknown parts of the message
	bytesRemaining := protoInfoTotalLen - protoInfoBytesRead
	reader.Seek(int64(bytesRemaining), seekCurrent)

	return tpl.Protocol
}

func parseNfAttrTLV(r *bytes.Reader) (isNested bool, attrType, len uint16, value []byte) {
	isNested, attrType, len = parseNfAttrTL(r)

	value = make([]byte, len)
	binary.Read(r, binary.BigEndian, &value)
	return isNested, attrType, len, value
}

func parseNfAttrTL(r *bytes.Reader) (isNested bool, attrType, len uint16) {
	binary.Read(r, nl.NativeEndian(), &len)
	len -= nl.SizeofNfattr

	binary.Read(r, nl.NativeEndian(), &attrType)
	isNested = (attrType & nl.NLA_F_NESTED) == nl.NLA_F_NESTED
	attrType = attrType & (nl.NLA_F_NESTED - 1)
	return isNested, attrType, len
}

// skipNfAttrValue seeks `r` past attr of length `len`.
// Maintains buffer alignment.
// Returns length of the seek performed.
func skipNfAttrValue(r *bytes.Reader, len uint16) uint16 {
	len = (len + nl.NLA_ALIGNTO - 1) & ^(nl.NLA_ALIGNTO - 1)
	r.Seek(int64(len), seekCurrent)
	return len
}

func parseBERaw16(r *bytes.Reader, v *uint16) {
	binary.Read(r, binary.BigEndian, v)
}

func parseBERaw32(r *bytes.Reader, v *uint32) {
	binary.Read(r, binary.BigEndian, v)
}

func parseBERaw64(r *bytes.Reader, v *uint64) {
	binary.Read(r, binary.BigEndian, v)
}

func parseRaw32(r *bytes.Reader, v *uint32) {
	binary.Read(r, nl.NativeEndian(), v)
}

func parseByteAndPacketCounters(r *bytes.Reader) (bytes, packets uint64) {
	for i := 0; i < 2; i++ {
		switch _, t, _ := parseNfAttrTL(r); t {
		case nl.CTA_COUNTERS_BYTES:
			parseBERaw64(r, &bytes)
		case nl.CTA_COUNTERS_PACKETS:
			parseBERaw64(r, &packets)
		default:
			return
		}
	}
	return
}

// when the flow is alive, only the timestamp_start is returned in structure
func parseTimeStamp(r *bytes.Reader, readSize uint16) (tstart, tstop uint64) {
	var numTimeStamps int
	oneItem := nl.SizeofNfattr + 8 // 4 bytes attr header + 8 bytes timestamp
	if readSize == uint16(oneItem) {
		numTimeStamps = 1
	} else if readSize == 2*uint16(oneItem) {
		numTimeStamps = 2
	} else {
		return
	}
	for i := 0; i < numTimeStamps; i++ {
		switch _, t, _ := parseNfAttrTL(r); t {
		case nl.CTA_TIMESTAMP_START:
			parseBERaw64(r, &tstart)
		case nl.CTA_TIMESTAMP_STOP:
			parseBERaw64(r, &tstop)
		default:
			return
		}
	}
	return

}

func parseProtoInfoTCPState(r *bytes.Reader) (s uint8) {
	binary.Read(r, binary.BigEndian, &s)
	r.Seek(nl.SizeofNfattr - 1, seekCurrent)
	return s
}

// parseProtoInfoTCP reads the entire nested protoinfo structure, but only parses the state attr.
func parseProtoInfoTCP(r *bytes.Reader, attrLen uint16) (*ProtoInfoTCP) {
	p := new(ProtoInfoTCP)
	bytesRead := 0
	for bytesRead < int(attrLen) {
		_, t, l := parseNfAttrTL(r)
		bytesRead += nl.SizeofNfattr

		switch t {
		case nl.CTA_PROTOINFO_TCP_STATE:
			p.State = parseProtoInfoTCPState(r)
			bytesRead += nl.SizeofNfattr
		default:
			bytesRead += int(skipNfAttrValue(r, l))
		}
	}

	return p
}

func parseProtoInfo(r *bytes.Reader, attrLen uint16) (p ProtoInfo) {
	bytesRead := 0
	for bytesRead < int(attrLen) {
		_, t, l := parseNfAttrTL(r)
		bytesRead += nl.SizeofNfattr

		switch t {
		case nl.CTA_PROTOINFO_TCP:
			p = parseProtoInfoTCP(r, l)
			bytesRead += int(l)
		// No inner fields of DCCP / SCTP currently supported.
		case nl.CTA_PROTOINFO_DCCP:
			p = new(ProtoInfoDCCP)
			skipped := skipNfAttrValue(r, l)
			bytesRead += int(skipped)
		case nl.CTA_PROTOINFO_SCTP:
			p = new(ProtoInfoSCTP)
			skipped := skipNfAttrValue(r, l)
			bytesRead += int(skipped)
		default:
			skipped := skipNfAttrValue(r, l)
			bytesRead += int(skipped)
		}
	}

	return p
}

func parseTimeOut(r *bytes.Reader) (ttimeout uint32) {
	parseBERaw32(r, &ttimeout)
	return
}

func parseConnectionMark(r *bytes.Reader) (mark uint32) {
	parseBERaw32(r, &mark)
	return
}

func parseConnectionLabels(r *bytes.Reader) (label []byte) {
	label = make([]byte, 16) // netfilter defines 128 bit labels value
	binary.Read(r, nl.NativeEndian(), &label)
	return
}

func parseConnectionZone(r *bytes.Reader) (zone uint16) {
	parseBERaw16(r, &zone)
	r.Seek(2, seekCurrent)
	return
}

func parseRawData(data []byte) *ConntrackFlow {
	s := &ConntrackFlow{}
	// First there is the Nfgenmsg header
	// consume only the family field
	reader := bytes.NewReader(data)
	binary.Read(reader, nl.NativeEndian(), &s.FamilyType)

	// skip rest of the Netfilter header
	reader.Seek(3, seekCurrent)
	// The message structure is the following:
	// <len, NLA_F_NESTED|CTA_TUPLE_ORIG> 4 bytes
	// <len, NLA_F_NESTED|CTA_TUPLE_IP> 4 bytes
	// flow information of the forward flow
	// <len, NLA_F_NESTED|CTA_TUPLE_REPLY> 4 bytes
	// <len, NLA_F_NESTED|CTA_TUPLE_IP> 4 bytes
	// flow information of the reverse flow
	for reader.Len() > 0 {
		if nested, t, l := parseNfAttrTL(reader); nested {
			switch t {
			case nl.CTA_TUPLE_ORIG:
				if nested, t, l = parseNfAttrTL(reader); nested && t == nl.CTA_TUPLE_IP {
					parseIpTuple(reader, &s.Forward)
				}
			case nl.CTA_TUPLE_REPLY:
				if nested, t, l = parseNfAttrTL(reader); nested && t == nl.CTA_TUPLE_IP {
					parseIpTuple(reader, &s.Reverse)
				} else {
					// Header not recognized skip it
					skipNfAttrValue(reader, l)
				}
			case nl.CTA_COUNTERS_ORIG:
				s.Forward.Bytes, s.Forward.Packets = parseByteAndPacketCounters(reader)
			case nl.CTA_COUNTERS_REPLY:
				s.Reverse.Bytes, s.Reverse.Packets = parseByteAndPacketCounters(reader)
			case nl.CTA_TIMESTAMP:
				s.TimeStart, s.TimeStop = parseTimeStamp(reader, l)
			case nl.CTA_PROTOINFO:
				s.ProtoInfo = parseProtoInfo(reader, l)
			default:
				skipNfAttrValue(reader, l)
			}
		} else {
			switch t {
			case nl.CTA_MARK:
				s.Mark = parseConnectionMark(reader)
				case nl.CTA_LABELS:
				s.Labels = parseConnectionLabels(reader)
			case nl.CTA_TIMEOUT:
				s.TimeOut = parseTimeOut(reader)
			case nl.CTA_ID, nl.CTA_STATUS, nl.CTA_USE:
				skipNfAttrValue(reader, l)
			case nl.CTA_ZONE:
				s.Zone = parseConnectionZone(reader)
			default:
				skipNfAttrValue(reader, l)
			}
		}
	}
	return s
}

// Conntrack parameters and options:
//   -n, --src-nat ip                      source NAT ip
//   -g, --dst-nat ip                      destination NAT ip
//   -j, --any-nat ip                      source or destination NAT ip
//   -m, --mark mark                       Set mark
//   -c, --secmark secmark                 Set selinux secmark
//   -e, --event-mask eventmask            Event mask, eg. NEW,DESTROY
//   -z, --zero                            Zero counters while listing
//   -o, --output type[,...]               Output format, eg. xml
//   -l, --label label[,...]               conntrack labels

// Common parameters and options:
//   -s, --src, --orig-src ip              Source address from original direction
//   -d, --dst, --orig-dst ip              Destination address from original direction
//   -r, --reply-src ip            Source address from reply direction
//   -q, --reply-dst ip            Destination address from reply direction
//   -p, --protonum proto          Layer 4 Protocol, eg. 'tcp'
//   -f, --family proto            Layer 3 Protocol, eg. 'ipv6'
//   -t, --timeout timeout         Set timeout
//   -u, --status status           Set status, eg. ASSURED
//   -w, --zone value              Set conntrack zone
//   --orig-zone value             Set zone for original direction
//   --reply-zone value            Set zone for reply direction
//   -b, --buffer-size             Netlink socket buffer size
//   --mask-src ip                 Source mask address
//   --mask-dst ip                 Destination mask address

// Layer 4 Protocol common parameters and options:
// TCP, UDP, SCTP, UDPLite and DCCP
//    --sport, --orig-port-src port    Source port in original direction
//    --dport, --orig-port-dst port    Destination port in original direction

// Filter types
type ConntrackFilterType uint8

const (
	ConntrackOrigSrcIP     = iota                // -orig-src ip    Source address from original direction
	ConntrackOrigDstIP                           // -orig-dst ip    Destination address from original direction
	ConntrackReplySrcIP                          // --reply-src ip  Reply Source IP
	ConntrackReplyDstIP                          // --reply-dst ip  Reply Destination IP
	ConntrackReplyAnyIP                          // Match source or destination reply IP
	ConntrackOrigSrcPort                         // --orig-port-src port    Source port in original direction
	ConntrackOrigDstPort                         // --orig-port-dst port    Destination port in original direction
	ConntrackMatchLabels                         // --label label1,label2   Labels used in entry
	ConntrackUnmatchLabels                       // --label label1,label2   Labels not used in entry
	ConntrackNatSrcIP      = ConntrackReplySrcIP // deprecated use instead ConntrackReplySrcIP
	ConntrackNatDstIP      = ConntrackReplyDstIP // deprecated use instead ConntrackReplyDstIP
	ConntrackNatAnyIP      = ConntrackReplyAnyIP // deprecated use instead ConntrackReplyAnyIP
)

type CustomConntrackFilter interface {
	// MatchConntrackFlow applies the filter to the flow and returns true if the flow matches
	// the filter or false otherwise
	MatchConntrackFlow(flow *ConntrackFlow) bool
}

type ConntrackFilter struct {
	ipNetFilter map[ConntrackFilterType]*net.IPNet
	portFilter  map[ConntrackFilterType]uint16
	protoFilter uint8
	labelFilter map[ConntrackFilterType][][]byte
	zoneFilter  *uint16
}

// AddIPNet adds a IP subnet to the conntrack filter
func (f *ConntrackFilter) AddIPNet(tp ConntrackFilterType, ipNet *net.IPNet) error {
	if ipNet == nil {
		return fmt.Errorf("Filter attribute empty")
	}
	if f.ipNetFilter == nil {
		f.ipNetFilter = make(map[ConntrackFilterType]*net.IPNet)
	}
	if _, ok := f.ipNetFilter[tp]; ok {
		return errors.New("Filter attribute already present")
	}
	f.ipNetFilter[tp] = ipNet
	return nil
}

// AddIP adds an IP to the conntrack filter
func (f *ConntrackFilter) AddIP(tp ConntrackFilterType, ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("Filter attribute empty")
	}
	return f.AddIPNet(tp, NewIPNet(ip))
}

// AddPort adds a Port to the conntrack filter if the Layer 4 protocol allows it
func (f *ConntrackFilter) AddPort(tp ConntrackFilterType, port uint16) error {
	switch f.protoFilter {
	// TCP, UDP, DCCP, SCTP, UDPLite
	case 6, 17, 33, 132, 136:
	default:
		return fmt.Errorf("Filter attribute not available without a valid Layer 4 protocol: %d", f.protoFilter)
	}

	if f.portFilter == nil {
		f.portFilter = make(map[ConntrackFilterType]uint16)
	}
	if _, ok := f.portFilter[tp]; ok {
		return errors.New("Filter attribute already present")
	}
	f.portFilter[tp] = port
	return nil
}

// AddProtocol adds the Layer 4 protocol to the conntrack filter
func (f *ConntrackFilter) AddProtocol(proto uint8) error {
	if f.protoFilter != 0 {
		return errors.New("Filter attribute already present")
	}
	f.protoFilter = proto
	return nil
}

// AddLabels adds the provided list (zero or more) of labels to the conntrack filter
// ConntrackFilterType here can be either:
//  1. ConntrackMatchLabels: This matches every flow that has a label value (len(flow.Labels) > 0)
//     against the list of provided labels. If `flow.Labels` contains ALL the provided labels
//     it is considered a match. This can be used when you want to match flows that contain
//     one or more labels.
//  2. ConntrackUnmatchLabels:  This matches every flow that has a label value (len(flow.Labels) > 0)
//     against the list of provided labels. If `flow.Labels` does NOT contain ALL the provided labels
//     it is considered a match. This can be used when you want to match flows that don't contain
//     one or more labels.
func (f *ConntrackFilter) AddLabels(tp ConntrackFilterType, labels [][]byte) error {
	if len(labels) == 0 {
		return errors.New("Invalid length for provided labels")
	}
	if f.labelFilter == nil {
		f.labelFilter = make(map[ConntrackFilterType][][]byte)
	}
	if _, ok := f.labelFilter[tp]; ok {
		return errors.New("Filter attribute already present")
	}
	f.labelFilter[tp] = labels
	return nil
}

// AddZone adds a zone to the conntrack filter
func (f *ConntrackFilter) AddZone(zone uint16) error {
	if f.zoneFilter != nil {
		return errors.New("Filter attribute already present")
	}
	f.zoneFilter = &zone
	return nil
}

// MatchConntrackFlow applies the filter to the flow and returns true if the flow matches the filter
// false otherwise
func (f *ConntrackFilter) MatchConntrackFlow(flow *ConntrackFlow) bool {
	if len(f.ipNetFilter) == 0 && len(f.portFilter) == 0 && f.protoFilter == 0 && len(f.labelFilter) == 0 && f.zoneFilter == nil {
		// empty filter always not match
		return false
	}

	// -p, --protonum proto          Layer 4 Protocol, eg. 'tcp'
	if f.protoFilter != 0 && flow.Forward.Protocol != f.protoFilter {
		// different Layer 4 protocol always not match
		return false
	}

	// Conntrack zone filter
	if f.zoneFilter != nil && *f.zoneFilter != flow.Zone {
		return false
	}

	match := true

	// IP conntrack filter
	if len(f.ipNetFilter) > 0 {
		// -orig-src ip   Source address from original direction
		if elem, found := f.ipNetFilter[ConntrackOrigSrcIP]; found {
			match = match && elem.Contains(flow.Forward.SrcIP)
		}

		// -orig-dst ip   Destination address from original direction
		if elem, found := f.ipNetFilter[ConntrackOrigDstIP]; match && found {
			match = match && elem.Contains(flow.Forward.DstIP)
		}

		// -src-nat ip    Source NAT ip
		if elem, found := f.ipNetFilter[ConntrackReplySrcIP]; match && found {
			match = match && elem.Contains(flow.Reverse.SrcIP)
		}

		// -dst-nat ip    Destination NAT ip
		if elem, found := f.ipNetFilter[ConntrackReplyDstIP]; match && found {
			match = match && elem.Contains(flow.Reverse.DstIP)
		}

		// Match source or destination reply IP
		if elem, found := f.ipNetFilter[ConntrackReplyAnyIP]; match && found {
			match = match && (elem.Contains(flow.Reverse.SrcIP) || elem.Contains(flow.Reverse.DstIP))
		}
	}

	// Layer 4 Port filter
	if len(f.portFilter) > 0 {
		// -orig-port-src port	Source port from original direction
		if elem, found := f.portFilter[ConntrackOrigSrcPort]; match && found {
			match = match && elem == flow.Forward.SrcPort
		}

		// -orig-port-dst port	Destination port from original direction
		if elem, found := f.portFilter[ConntrackOrigDstPort]; match && found {
			match = match && elem == flow.Forward.DstPort
		}
	}

	// Label filter
	if len(f.labelFilter) > 0 {
		if len(flow.Labels) > 0 {
			// --label label1,label2 in conn entry;
			// every label passed should be contained in flow.Labels for a match to be true
			if elem, found := f.labelFilter[ConntrackMatchLabels]; match && found {
				for _, label := range elem {
					match = match && (bytes.Contains(flow.Labels, label))
				}
			}
			// --label label1,label2 in conn entry;
			// every label passed should be not contained in flow.Labels for a match to be true
			if elem, found := f.labelFilter[ConntrackUnmatchLabels]; match && found {
				for _, label := range elem {
					match = match && !(bytes.Contains(flow.Labels, label))
				}
			}
		} else {
			// flow doesn't contain labels, so it doesn't contain or notContain any provided matches
			match = false
		}
	}

	return match
}

var _ CustomConntrackFilter = (*ConntrackFilter)(nil)
