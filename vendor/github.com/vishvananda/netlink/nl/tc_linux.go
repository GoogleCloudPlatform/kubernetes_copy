package nl

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// LinkLayer
const (
	LINKLAYER_UNSPEC = iota
	LINKLAYER_ETHERNET
	LINKLAYER_ATM
)

// ATM
const (
	ATM_CELL_PAYLOAD = 48
	ATM_CELL_SIZE    = 53
)

const TC_LINKLAYER_MASK = 0x0F

// Police
const (
	TCA_POLICE_UNSPEC = iota
	TCA_POLICE_TBF
	TCA_POLICE_RATE
	TCA_POLICE_PEAKRATE
	TCA_POLICE_AVRATE
	TCA_POLICE_RESULT
	TCA_POLICE_MAX = TCA_POLICE_RESULT
)

// Message types
const (
	TCA_UNSPEC = iota
	TCA_KIND
	TCA_OPTIONS
	TCA_STATS
	TCA_XSTATS
	TCA_RATE
	TCA_FCNT
	TCA_STATS2
	TCA_STAB
	TCA_PAD
	TCA_DUMP_INVISIBLE
	TCA_CHAIN
	TCA_HW_OFFLOAD
	TCA_INGRESS_BLOCK
	TCA_EGRESS_BLOCK
	TCA_DUMP_FLAGS
	TCA_MAX = TCA_DUMP_FLAGS
)

const (
	TCA_ACT_TAB = 1
	TCAA_MAX    = 1
)

const (
	TCA_ACT_UNSPEC = iota
	TCA_ACT_KIND
	TCA_ACT_OPTIONS
	TCA_ACT_INDEX
	TCA_ACT_STATS
	TCA_ACT_PAD
	TCA_ACT_COOKIE
	TCA_ACT_FLAGS
	TCA_ACT_HW_STATS
	TCA_ACT_USED_HW_STATS
	TCA_ACT_IN_HW_COUNT
	TCA_ACT_MAX
)

const (
	TCA_PRIO_UNSPEC = iota
	TCA_PRIO_MQ
	TCA_PRIO_MAX = TCA_PRIO_MQ
)

const (
	TCA_STATS_UNSPEC = iota
	TCA_STATS_BASIC
	TCA_STATS_RATE_EST
	TCA_STATS_QUEUE
	TCA_STATS_APP
	TCA_STATS_RATE_EST64
	TCA_STATS_PAD
	TCA_STATS_BASIC_HW
	TCA_STATS_PKT64
	TCA_STATS_MAX = TCA_STATS_PKT64
)

const (
	SizeofTcMsg          = 0x14
	SizeofTcActionMsg    = 0x04
	SizeofTcPrioMap      = 0x14
	SizeofTcRateSpec     = 0x0c
	SizeofTcNetemQopt    = 0x18
	SizeofTcNetemCorr    = 0x0c
	SizeofTcNetemReorder = 0x08
	SizeofTcNetemCorrupt = 0x08
	SizeOfTcNetemRate    = 0x10
	SizeofTcTbfQopt      = 2*SizeofTcRateSpec + 0x0c
	SizeofTcHtbCopt      = 2*SizeofTcRateSpec + 0x14
	SizeofTcHtbGlob      = 0x14
	SizeofTcU32Key       = 0x10
	SizeofTcU32Sel       = 0x10 // without keys
	SizeofTcGen          = 0x16
	SizeofTcConnmark     = SizeofTcGen + 0x04
	SizeofTcCsum         = SizeofTcGen + 0x04
	SizeofTcMirred       = SizeofTcGen + 0x08
	SizeofTcVlan         = SizeofTcGen + 0x04
	SizeofTcTunnelKey    = SizeofTcGen + 0x04
	SizeofTcSkbEdit      = SizeofTcGen
	SizeofTcPolice       = 2*SizeofTcRateSpec + 0x20
	SizeofTcSfqQopt      = 0x0b
	SizeofTcSfqRedStats  = 0x18
	SizeofTcSfqQoptV1    = SizeofTcSfqQopt + SizeofTcSfqRedStats + 0x1c
	SizeofUint32Bitfield = 0x8
)

// struct tcmsg {
//   unsigned char tcm_family;
//   unsigned char tcm__pad1;
//   unsigned short  tcm__pad2;
//   int   tcm_ifindex;
//   __u32   tcm_handle;
//   __u32   tcm_parent;
//   __u32   tcm_info;
// };

type TcMsg struct {
	Family  uint8
	Pad     [3]byte
	Ifindex int32
	Handle  uint32
	Parent  uint32
	Info    uint32
}

func (msg *TcMsg) Len() int {
	return SizeofTcMsg
}

func DeserializeTcMsg(b []byte) *TcMsg {
	return (*TcMsg)(unsafe.Pointer(&b[0:SizeofTcMsg][0]))
}

func (x *TcMsg) Serialize() []byte {
	return (*(*[SizeofTcMsg]byte)(unsafe.Pointer(x)))[:]
}

type Tcf struct {
	Install  uint64
	LastUse  uint64
	Expires  uint64
	FirstUse uint64
}

func DeserializeTcf(b []byte) *Tcf {
	const size = int(unsafe.Sizeof(Tcf{}))
	return (*Tcf)(unsafe.Pointer(&b[0:size][0]))
}

// struct tcamsg {
//   unsigned char tca_family;
//   unsigned char tca__pad1;
//   unsigned short  tca__pad2;
// };

type TcActionMsg struct {
	Family uint8
	Pad    [3]byte
}

func (msg *TcActionMsg) Len() int {
	return SizeofTcActionMsg
}

func DeserializeTcActionMsg(b []byte) *TcActionMsg {
	return (*TcActionMsg)(unsafe.Pointer(&b[0:SizeofTcActionMsg][0]))
}

func (x *TcActionMsg) Serialize() []byte {
	return (*(*[SizeofTcActionMsg]byte)(unsafe.Pointer(x)))[:]
}

const (
	TC_PRIO_MAX = 15
)

// struct tc_prio_qopt {
// 	int bands;      /* Number of bands */
// 	__u8  priomap[TC_PRIO_MAX+1]; /* Map: logical priority -> PRIO band */
// };

type TcPrioMap struct {
	Bands   int32
	Priomap [TC_PRIO_MAX + 1]uint8
}

func (msg *TcPrioMap) Len() int {
	return SizeofTcPrioMap
}

func DeserializeTcPrioMap(b []byte) *TcPrioMap {
	return (*TcPrioMap)(unsafe.Pointer(&b[0:SizeofTcPrioMap][0]))
}

func (x *TcPrioMap) Serialize() []byte {
	return (*(*[SizeofTcPrioMap]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_TBF_UNSPEC = iota
	TCA_TBF_PARMS
	TCA_TBF_RTAB
	TCA_TBF_PTAB
	TCA_TBF_RATE64
	TCA_TBF_PRATE64
	TCA_TBF_BURST
	TCA_TBF_PBURST
	TCA_TBF_MAX = TCA_TBF_PBURST
)

// struct tc_ratespec {
//   unsigned char cell_log;
//   __u8    linklayer; /* lower 4 bits */
//   unsigned short  overhead;
//   short   cell_align;
//   unsigned short  mpu;
//   __u32   rate;
// };

type TcRateSpec struct {
	CellLog   uint8
	Linklayer uint8
	Overhead  uint16
	CellAlign int16
	Mpu       uint16
	Rate      uint32
}

func (msg *TcRateSpec) Len() int {
	return SizeofTcRateSpec
}

func DeserializeTcRateSpec(b []byte) *TcRateSpec {
	return (*TcRateSpec)(unsafe.Pointer(&b[0:SizeofTcRateSpec][0]))
}

func (x *TcRateSpec) Serialize() []byte {
	return (*(*[SizeofTcRateSpec]byte)(unsafe.Pointer(x)))[:]
}

/**
* NETEM
 */

const (
	TCA_NETEM_UNSPEC = iota
	TCA_NETEM_CORR
	TCA_NETEM_DELAY_DIST
	TCA_NETEM_REORDER
	TCA_NETEM_CORRUPT
	TCA_NETEM_LOSS
	TCA_NETEM_RATE
	TCA_NETEM_ECN
	TCA_NETEM_RATE64
	TCA_NETEM_MAX = TCA_NETEM_RATE64
)

// struct tc_netem_qopt {
//	__u32	latency;	/* added delay (us) */
//	__u32   limit;		/* fifo limit (packets) */
//	__u32	loss;		/* random packet loss (0=none ~0=100%) */
//	__u32	gap;		/* re-ordering gap (0 for none) */
//	__u32   duplicate;	/* random packet dup  (0=none ~0=100%) */
// 	__u32	jitter;		/* random jitter in latency (us) */
// };

type TcNetemQopt struct {
	Latency   uint32
	Limit     uint32
	Loss      uint32
	Gap       uint32
	Duplicate uint32
	Jitter    uint32
}

func (msg *TcNetemQopt) Len() int {
	return SizeofTcNetemQopt
}

func DeserializeTcNetemQopt(b []byte) *TcNetemQopt {
	return (*TcNetemQopt)(unsafe.Pointer(&b[0:SizeofTcNetemQopt][0]))
}

func (x *TcNetemQopt) Serialize() []byte {
	return (*(*[SizeofTcNetemQopt]byte)(unsafe.Pointer(x)))[:]
}

// struct tc_netem_corr {
//  __u32   delay_corr; /* delay correlation */
//  __u32   loss_corr;  /* packet loss correlation */
//  __u32   dup_corr;   /* duplicate correlation  */
// };

type TcNetemCorr struct {
	DelayCorr uint32
	LossCorr  uint32
	DupCorr   uint32
}

func (msg *TcNetemCorr) Len() int {
	return SizeofTcNetemCorr
}

func DeserializeTcNetemCorr(b []byte) *TcNetemCorr {
	return (*TcNetemCorr)(unsafe.Pointer(&b[0:SizeofTcNetemCorr][0]))
}

func (x *TcNetemCorr) Serialize() []byte {
	return (*(*[SizeofTcNetemCorr]byte)(unsafe.Pointer(x)))[:]
}

// struct tc_netem_reorder {
//  __u32   probability;
//  __u32   correlation;
// };

type TcNetemReorder struct {
	Probability uint32
	Correlation uint32
}

func (msg *TcNetemReorder) Len() int {
	return SizeofTcNetemReorder
}

func DeserializeTcNetemReorder(b []byte) *TcNetemReorder {
	return (*TcNetemReorder)(unsafe.Pointer(&b[0:SizeofTcNetemReorder][0]))
}

func (x *TcNetemReorder) Serialize() []byte {
	return (*(*[SizeofTcNetemReorder]byte)(unsafe.Pointer(x)))[:]
}

// struct tc_netem_corrupt {
//  __u32   probability;
//  __u32   correlation;
// };

type TcNetemCorrupt struct {
	Probability uint32
	Correlation uint32
}

func (msg *TcNetemCorrupt) Len() int {
	return SizeofTcNetemCorrupt
}

func DeserializeTcNetemCorrupt(b []byte) *TcNetemCorrupt {
	return (*TcNetemCorrupt)(unsafe.Pointer(&b[0:SizeofTcNetemCorrupt][0]))
}

func (x *TcNetemCorrupt) Serialize() []byte {
	return (*(*[SizeofTcNetemCorrupt]byte)(unsafe.Pointer(x)))[:]
}

// TcNetemRate is a struct that represents the rate of a netem qdisc
type TcNetemRate struct {
	Rate           uint32
	PacketOverhead int32
	CellSize       uint32
	CellOverhead   int32
}

func (msg *TcNetemRate) Len() int {
	return SizeofTcRateSpec
}

func DeserializeTcNetemRate(b []byte) *TcNetemRate {
	return (*TcNetemRate)(unsafe.Pointer(&b[0:SizeofTcRateSpec][0]))
}

func (msg *TcNetemRate) Serialize() []byte {
	return (*(*[SizeOfTcNetemRate]byte)(unsafe.Pointer(msg)))[:]
}

// struct tc_tbf_qopt {
//   struct tc_ratespec rate;
//   struct tc_ratespec peakrate;
//   __u32   limit;
//   __u32   buffer;
//   __u32   mtu;
// };

type TcTbfQopt struct {
	Rate     TcRateSpec
	Peakrate TcRateSpec
	Limit    uint32
	Buffer   uint32
	Mtu      uint32
}

func (msg *TcTbfQopt) Len() int {
	return SizeofTcTbfQopt
}

func DeserializeTcTbfQopt(b []byte) *TcTbfQopt {
	return (*TcTbfQopt)(unsafe.Pointer(&b[0:SizeofTcTbfQopt][0]))
}

func (x *TcTbfQopt) Serialize() []byte {
	return (*(*[SizeofTcTbfQopt]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_HTB_UNSPEC = iota
	TCA_HTB_PARMS
	TCA_HTB_INIT
	TCA_HTB_CTAB
	TCA_HTB_RTAB
	TCA_HTB_DIRECT_QLEN
	TCA_HTB_RATE64
	TCA_HTB_CEIL64
	TCA_HTB_MAX = TCA_HTB_CEIL64
)

//struct tc_htb_opt {
//	struct tc_ratespec	rate;
//	struct tc_ratespec	ceil;
//	__u32	buffer;
//	__u32	cbuffer;
//	__u32	quantum;
//	__u32	level;		/* out only */
//	__u32	prio;
//};

type TcHtbCopt struct {
	Rate    TcRateSpec
	Ceil    TcRateSpec
	Buffer  uint32
	Cbuffer uint32
	Quantum uint32
	Level   uint32
	Prio    uint32
}

func (msg *TcHtbCopt) Len() int {
	return SizeofTcHtbCopt
}

func DeserializeTcHtbCopt(b []byte) *TcHtbCopt {
	return (*TcHtbCopt)(unsafe.Pointer(&b[0:SizeofTcHtbCopt][0]))
}

func (x *TcHtbCopt) Serialize() []byte {
	return (*(*[SizeofTcHtbCopt]byte)(unsafe.Pointer(x)))[:]
}

type TcHtbGlob struct {
	Version      uint32
	Rate2Quantum uint32
	Defcls       uint32
	Debug        uint32
	DirectPkts   uint32
}

func (msg *TcHtbGlob) Len() int {
	return SizeofTcHtbGlob
}

func DeserializeTcHtbGlob(b []byte) *TcHtbGlob {
	return (*TcHtbGlob)(unsafe.Pointer(&b[0:SizeofTcHtbGlob][0]))
}

func (x *TcHtbGlob) Serialize() []byte {
	return (*(*[SizeofTcHtbGlob]byte)(unsafe.Pointer(x)))[:]
}

// HFSC

type Curve struct {
	m1 uint32
	d  uint32
	m2 uint32
}

type HfscCopt struct {
	Rsc Curve
	Fsc Curve
	Usc Curve
}

func (c *Curve) Attrs() (uint32, uint32, uint32) {
	return c.m1, c.d, c.m2
}

func (c *Curve) Set(m1 uint32, d uint32, m2 uint32) {
	c.m1 = m1
	c.d = d
	c.m2 = m2
}

func DeserializeHfscCurve(b []byte) *Curve {
	return &Curve{
		m1: binary.LittleEndian.Uint32(b[0:4]),
		d:  binary.LittleEndian.Uint32(b[4:8]),
		m2: binary.LittleEndian.Uint32(b[8:12]),
	}
}

func SerializeHfscCurve(c *Curve) (b []byte) {
	t := make([]byte, binary.MaxVarintLen32)
	binary.LittleEndian.PutUint32(t, c.m1)
	b = append(b, t[:4]...)
	binary.LittleEndian.PutUint32(t, c.d)
	b = append(b, t[:4]...)
	binary.LittleEndian.PutUint32(t, c.m2)
	b = append(b, t[:4]...)
	return b
}

type TcHfscOpt struct {
	Defcls uint16
}

func (x *TcHfscOpt) Serialize() []byte {
	return (*(*[2]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_U32_UNSPEC = iota
	TCA_U32_CLASSID
	TCA_U32_HASH
	TCA_U32_LINK
	TCA_U32_DIVISOR
	TCA_U32_SEL
	TCA_U32_POLICE
	TCA_U32_ACT
	TCA_U32_INDEV
	TCA_U32_PCNT
	TCA_U32_MARK
	TCA_U32_MAX = TCA_U32_MARK
)

// struct tc_u32_key {
//   __be32    mask;
//   __be32    val;
//   int   off;
//   int   offmask;
// };

type TcU32Key struct {
	Mask    uint32 // big endian
	Val     uint32 // big endian
	Off     int32
	OffMask int32
}

func (msg *TcU32Key) Len() int {
	return SizeofTcU32Key
}

func DeserializeTcU32Key(b []byte) *TcU32Key {
	return (*TcU32Key)(unsafe.Pointer(&b[0:SizeofTcU32Key][0]))
}

func (x *TcU32Key) Serialize() []byte {
	return (*(*[SizeofTcU32Key]byte)(unsafe.Pointer(x)))[:]
}

// struct tc_u32_sel {
//   unsigned char   flags;
//   unsigned char   offshift;
//   unsigned char   nkeys;
//
//   __be16      offmask;
//   __u16     off;
//   short     offoff;
//
//   short     hoff;
//   __be32      hmask;
//   struct tc_u32_key keys[0];
// };

const (
	TC_U32_TERMINAL  = 1 << iota
	TC_U32_OFFSET    = 1 << iota
	TC_U32_VAROFFSET = 1 << iota
	TC_U32_EAT       = 1 << iota
)

type TcU32Sel struct {
	Flags    uint8
	Offshift uint8
	Nkeys    uint8
	Pad      uint8
	Offmask  uint16 // big endian
	Off      uint16
	Offoff   int16
	Hoff     int16
	Hmask    uint32 // big endian
	Keys     []TcU32Key
}

func (msg *TcU32Sel) Len() int {
	return SizeofTcU32Sel + int(msg.Nkeys)*SizeofTcU32Key
}

func DeserializeTcU32Sel(b []byte) *TcU32Sel {
	x := &TcU32Sel{}
	copy((*(*[SizeofTcU32Sel]byte)(unsafe.Pointer(x)))[:], b)
	next := SizeofTcU32Sel
	var i uint8
	for i = 0; i < x.Nkeys; i++ {
		x.Keys = append(x.Keys, *DeserializeTcU32Key(b[next:]))
		next += SizeofTcU32Key
	}
	return x
}

func (x *TcU32Sel) Serialize() []byte {
	// This can't just unsafe.cast because it must iterate through keys.
	buf := make([]byte, x.Len())
	copy(buf, (*(*[SizeofTcU32Sel]byte)(unsafe.Pointer(x)))[:])
	next := SizeofTcU32Sel
	for _, key := range x.Keys {
		keyBuf := key.Serialize()
		copy(buf[next:], keyBuf)
		next += SizeofTcU32Key
	}
	return buf
}

type TcGen struct {
	Index   uint32
	Capab   uint32
	Action  int32
	Refcnt  int32
	Bindcnt int32
}

func (msg *TcGen) Len() int {
	return SizeofTcGen
}

func DeserializeTcGen(b []byte) *TcGen {
	return (*TcGen)(unsafe.Pointer(&b[0:SizeofTcGen][0]))
}

func (x *TcGen) Serialize() []byte {
	return (*(*[SizeofTcGen]byte)(unsafe.Pointer(x)))[:]
}

// #define tc_gen \
//   __u32                 index; \
//   __u32                 capab; \
//   int                   action; \
//   int                   refcnt; \
//   int                   bindcnt

const (
	TCA_ACT_GACT = 5
)

const (
	TCA_GACT_UNSPEC = iota
	TCA_GACT_TM
	TCA_GACT_PARMS
	TCA_GACT_PROB
	TCA_GACT_MAX = TCA_GACT_PROB
)

type TcGact TcGen

const (
	TCA_ACT_BPF = 13
)

const (
	TCA_ACT_BPF_UNSPEC = iota
	TCA_ACT_BPF_TM
	TCA_ACT_BPF_PARMS
	TCA_ACT_BPF_OPS_LEN
	TCA_ACT_BPF_OPS
	TCA_ACT_BPF_FD
	TCA_ACT_BPF_NAME
	TCA_ACT_BPF_MAX = TCA_ACT_BPF_NAME
)

const (
	TCA_BPF_FLAG_ACT_DIRECT uint32 = 1 << iota
)

const (
	TCA_BPF_UNSPEC = iota
	TCA_BPF_ACT
	TCA_BPF_POLICE
	TCA_BPF_CLASSID
	TCA_BPF_OPS_LEN
	TCA_BPF_OPS
	TCA_BPF_FD
	TCA_BPF_NAME
	TCA_BPF_FLAGS
	TCA_BPF_FLAGS_GEN
	TCA_BPF_TAG
	TCA_BPF_ID
	TCA_BPF_MAX = TCA_BPF_ID
)

type TcBpf TcGen

const (
	TCA_ACT_CONNMARK = 14
)

const (
	TCA_CONNMARK_UNSPEC = iota
	TCA_CONNMARK_PARMS
	TCA_CONNMARK_TM
	TCA_CONNMARK_MAX = TCA_CONNMARK_TM
)

// struct tc_connmark {
//   tc_gen;
//   __u16 zone;
// };

type TcConnmark struct {
	TcGen
	Zone uint16
}

func (msg *TcConnmark) Len() int {
	return SizeofTcConnmark
}

func DeserializeTcConnmark(b []byte) *TcConnmark {
	return (*TcConnmark)(unsafe.Pointer(&b[0:SizeofTcConnmark][0]))
}

func (x *TcConnmark) Serialize() []byte {
	return (*(*[SizeofTcConnmark]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_CSUM_UNSPEC = iota
	TCA_CSUM_PARMS
	TCA_CSUM_TM
	TCA_CSUM_PAD
	TCA_CSUM_MAX = TCA_CSUM_PAD
)

// struct tc_csum {
//   tc_gen;
//   __u32 update_flags;
// }

type TcCsum struct {
	TcGen
	UpdateFlags uint32
}

func (msg *TcCsum) Len() int {
	return SizeofTcCsum
}

func DeserializeTcCsum(b []byte) *TcCsum {
	return (*TcCsum)(unsafe.Pointer(&b[0:SizeofTcCsum][0]))
}

func (x *TcCsum) Serialize() []byte {
	return (*(*[SizeofTcCsum]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_ACT_MIRRED = 8
)

const (
	TCA_MIRRED_UNSPEC = iota
	TCA_MIRRED_TM
	TCA_MIRRED_PARMS
	TCA_MIRRED_MAX = TCA_MIRRED_PARMS
)

// struct tc_mirred {
// 	tc_gen;
// 	int                     eaction;   /* one of IN/EGRESS_MIRROR/REDIR */
// 	__u32                   ifindex;  /* ifindex of egress port */
// };

type TcMirred struct {
	TcGen
	Eaction int32
	Ifindex uint32
}

func (msg *TcMirred) Len() int {
	return SizeofTcMirred
}

func DeserializeTcMirred(b []byte) *TcMirred {
	return (*TcMirred)(unsafe.Pointer(&b[0:SizeofTcMirred][0]))
}

func (x *TcMirred) Serialize() []byte {
	return (*(*[SizeofTcMirred]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_VLAN_UNSPEC = iota
	TCA_VLAN_TM
	TCA_VLAN_PARMS
	TCA_VLAN_PUSH_VLAN_ID
	TCA_VLAN_PUSH_VLAN_PROTOCOL
	TCA_VLAN_PAD
	TCA_VLAN_PUSH_VLAN_PRIORITY
	TCA_VLAN_PUSH_ETH_DST
	TCA_VLAN_PUSH_ETH_SRC
	TCA_VLAN_MAX
)

//struct tc_vlan {
//	tc_gen;
//	int v_action;
//};

type TcVlan struct {
	TcGen
	Action int32
}

func (msg *TcVlan) Len() int {
	return SizeofTcVlan
}

func DeserializeTcVlan(b []byte) *TcVlan {
	return (*TcVlan)(unsafe.Pointer(&b[0:SizeofTcVlan][0]))
}

func (x *TcVlan) Serialize() []byte {
	return (*(*[SizeofTcVlan]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_TUNNEL_KEY_UNSPEC = iota
	TCA_TUNNEL_KEY_TM
	TCA_TUNNEL_KEY_PARMS
	TCA_TUNNEL_KEY_ENC_IPV4_SRC
	TCA_TUNNEL_KEY_ENC_IPV4_DST
	TCA_TUNNEL_KEY_ENC_IPV6_SRC
	TCA_TUNNEL_KEY_ENC_IPV6_DST
	TCA_TUNNEL_KEY_ENC_KEY_ID
	TCA_TUNNEL_KEY_PAD
	TCA_TUNNEL_KEY_ENC_DST_PORT
	TCA_TUNNEL_KEY_NO_CSUM
	TCA_TUNNEL_KEY_ENC_OPTS
	TCA_TUNNEL_KEY_ENC_TOS
	TCA_TUNNEL_KEY_ENC_TTL
	TCA_TUNNEL_KEY_MAX
)

type TcTunnelKey struct {
	TcGen
	Action int32
}

func (x *TcTunnelKey) Len() int {
	return SizeofTcTunnelKey
}

func DeserializeTunnelKey(b []byte) *TcTunnelKey {
	return (*TcTunnelKey)(unsafe.Pointer(&b[0:SizeofTcTunnelKey][0]))
}

func (x *TcTunnelKey) Serialize() []byte {
	return (*(*[SizeofTcTunnelKey]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_SKBEDIT_UNSPEC = iota
	TCA_SKBEDIT_TM
	TCA_SKBEDIT_PARMS
	TCA_SKBEDIT_PRIORITY
	TCA_SKBEDIT_QUEUE_MAPPING
	TCA_SKBEDIT_MARK
	TCA_SKBEDIT_PAD
	TCA_SKBEDIT_PTYPE
	TCA_SKBEDIT_MASK
	TCA_SKBEDIT_MAX
)

type TcSkbEdit struct {
	TcGen
}

func (x *TcSkbEdit) Len() int {
	return SizeofTcSkbEdit
}

func DeserializeSkbEdit(b []byte) *TcSkbEdit {
	return (*TcSkbEdit)(unsafe.Pointer(&b[0:SizeofTcSkbEdit][0]))
}

func (x *TcSkbEdit) Serialize() []byte {
	return (*(*[SizeofTcSkbEdit]byte)(unsafe.Pointer(x)))[:]
}

// struct tc_police {
// 	__u32			index;
// 	int			action;
// 	__u32			limit;
// 	__u32			burst;
// 	__u32			mtu;
// 	struct tc_ratespec	rate;
// 	struct tc_ratespec	peakrate;
// 	int				refcnt;
// 	int				bindcnt;
// 	__u32			capab;
// };

type TcPolice struct {
	Index    uint32
	Action   int32
	Limit    uint32
	Burst    uint32
	Mtu      uint32
	Rate     TcRateSpec
	PeakRate TcRateSpec
	Refcnt   int32
	Bindcnt  int32
	Capab    uint32
}

func (msg *TcPolice) Len() int {
	return SizeofTcPolice
}

func DeserializeTcPolice(b []byte) *TcPolice {
	return (*TcPolice)(unsafe.Pointer(&b[0:SizeofTcPolice][0]))
}

func (x *TcPolice) Serialize() []byte {
	return (*(*[SizeofTcPolice]byte)(unsafe.Pointer(x)))[:]
}

const (
	TCA_FW_UNSPEC = iota
	TCA_FW_CLASSID
	TCA_FW_POLICE
	TCA_FW_INDEV
	TCA_FW_ACT
	TCA_FW_MASK
	TCA_FW_MAX = TCA_FW_MASK
)

const (
	TCA_MATCHALL_UNSPEC = iota
	TCA_MATCHALL_CLASSID
	TCA_MATCHALL_ACT
	TCA_MATCHALL_FLAGS
)

const (
	TCA_FQ_UNSPEC             = iota
	TCA_FQ_PLIMIT             // limit of total number of packets in queue
	TCA_FQ_FLOW_PLIMIT        // limit of packets per flow
	TCA_FQ_QUANTUM            // RR quantum
	TCA_FQ_INITIAL_QUANTUM    // RR quantum for new flow
	TCA_FQ_RATE_ENABLE        // enable/disable rate limiting
	TCA_FQ_FLOW_DEFAULT_RATE  // obsolete do not use
	TCA_FQ_FLOW_MAX_RATE      // per flow max rate
	TCA_FQ_BUCKETS_LOG        // log2(number of buckets)
	TCA_FQ_FLOW_REFILL_DELAY  // flow credit refill delay in usec
	TCA_FQ_ORPHAN_MASK        // mask applied to orphaned skb hashes
	TCA_FQ_LOW_RATE_THRESHOLD // per packet delay under this rate
	TCA_FQ_CE_THRESHOLD       // DCTCP-like CE-marking threshold
	TCA_FQ_TIMER_SLACK        // timer slack
	TCA_FQ_HORIZON            // time horizon in us
	TCA_FQ_HORIZON_DROP       // drop packets beyond horizon, or cap their EDT
)

const (
	TCA_FQ_CODEL_UNSPEC = iota
	TCA_FQ_CODEL_TARGET
	TCA_FQ_CODEL_LIMIT
	TCA_FQ_CODEL_INTERVAL
	TCA_FQ_CODEL_ECN
	TCA_FQ_CODEL_FLOWS
	TCA_FQ_CODEL_QUANTUM
	TCA_FQ_CODEL_CE_THRESHOLD
	TCA_FQ_CODEL_DROP_BATCH_SIZE
	TCA_FQ_CODEL_MEMORY_LIMIT
)

const (
	TCA_HFSC_UNSPEC = iota
	TCA_HFSC_RSC
	TCA_HFSC_FSC
	TCA_HFSC_USC
)

const (
	TCA_FLOWER_UNSPEC = iota
	TCA_FLOWER_CLASSID
	TCA_FLOWER_INDEV
	TCA_FLOWER_ACT
	TCA_FLOWER_KEY_ETH_DST       /* ETH_ALEN */
	TCA_FLOWER_KEY_ETH_DST_MASK  /* ETH_ALEN */
	TCA_FLOWER_KEY_ETH_SRC       /* ETH_ALEN */
	TCA_FLOWER_KEY_ETH_SRC_MASK  /* ETH_ALEN */
	TCA_FLOWER_KEY_ETH_TYPE      /* be16 */
	TCA_FLOWER_KEY_IP_PROTO      /* u8 */
	TCA_FLOWER_KEY_IPV4_SRC      /* be32 */
	TCA_FLOWER_KEY_IPV4_SRC_MASK /* be32 */
	TCA_FLOWER_KEY_IPV4_DST      /* be32 */
	TCA_FLOWER_KEY_IPV4_DST_MASK /* be32 */
	TCA_FLOWER_KEY_IPV6_SRC      /* struct in6_addr */
	TCA_FLOWER_KEY_IPV6_SRC_MASK /* struct in6_addr */
	TCA_FLOWER_KEY_IPV6_DST      /* struct in6_addr */
	TCA_FLOWER_KEY_IPV6_DST_MASK /* struct in6_addr */
	TCA_FLOWER_KEY_TCP_SRC       /* be16 */
	TCA_FLOWER_KEY_TCP_DST       /* be16 */
	TCA_FLOWER_KEY_UDP_SRC       /* be16 */
	TCA_FLOWER_KEY_UDP_DST       /* be16 */

	TCA_FLOWER_FLAGS
	TCA_FLOWER_KEY_VLAN_ID       /* be16 */
	TCA_FLOWER_KEY_VLAN_PRIO     /* u8   */
	TCA_FLOWER_KEY_VLAN_ETH_TYPE /* be16 */

	TCA_FLOWER_KEY_ENC_KEY_ID        /* be32 */
	TCA_FLOWER_KEY_ENC_IPV4_SRC      /* be32 */
	TCA_FLOWER_KEY_ENC_IPV4_SRC_MASK /* be32 */
	TCA_FLOWER_KEY_ENC_IPV4_DST      /* be32 */
	TCA_FLOWER_KEY_ENC_IPV4_DST_MASK /* be32 */
	TCA_FLOWER_KEY_ENC_IPV6_SRC      /* struct in6_addr */
	TCA_FLOWER_KEY_ENC_IPV6_SRC_MASK /* struct in6_addr */
	TCA_FLOWER_KEY_ENC_IPV6_DST      /* struct in6_addr */
	TCA_FLOWER_KEY_ENC_IPV6_DST_MASK /* struct in6_addr */

	TCA_FLOWER_KEY_TCP_SRC_MASK  /* be16 */
	TCA_FLOWER_KEY_TCP_DST_MASK  /* be16 */
	TCA_FLOWER_KEY_UDP_SRC_MASK  /* be16 */
	TCA_FLOWER_KEY_UDP_DST_MASK  /* be16 */
	TCA_FLOWER_KEY_SCTP_SRC_MASK /* be16 */
	TCA_FLOWER_KEY_SCTP_DST_MASK /* be16 */

	TCA_FLOWER_KEY_SCTP_SRC /* be16 */
	TCA_FLOWER_KEY_SCTP_DST /* be16 */

	TCA_FLOWER_KEY_ENC_UDP_SRC_PORT      /* be16 */
	TCA_FLOWER_KEY_ENC_UDP_SRC_PORT_MASK /* be16 */
	TCA_FLOWER_KEY_ENC_UDP_DST_PORT      /* be16 */
	TCA_FLOWER_KEY_ENC_UDP_DST_PORT_MASK /* be16 */

	TCA_FLOWER_KEY_FLAGS      /* be32 */
	TCA_FLOWER_KEY_FLAGS_MASK /* be32 */

	TCA_FLOWER_KEY_ICMPV4_CODE      /* u8 */
	TCA_FLOWER_KEY_ICMPV4_CODE_MASK /* u8 */
	TCA_FLOWER_KEY_ICMPV4_TYPE      /* u8 */
	TCA_FLOWER_KEY_ICMPV4_TYPE_MASK /* u8 */
	TCA_FLOWER_KEY_ICMPV6_CODE      /* u8 */
	TCA_FLOWER_KEY_ICMPV6_CODE_MASK /* u8 */
	TCA_FLOWER_KEY_ICMPV6_TYPE      /* u8 */
	TCA_FLOWER_KEY_ICMPV6_TYPE_MASK /* u8 */

	TCA_FLOWER_KEY_ARP_SIP      /* be32 */
	TCA_FLOWER_KEY_ARP_SIP_MASK /* be32 */
	TCA_FLOWER_KEY_ARP_TIP      /* be32 */
	TCA_FLOWER_KEY_ARP_TIP_MASK /* be32 */
	TCA_FLOWER_KEY_ARP_OP       /* u8 */
	TCA_FLOWER_KEY_ARP_OP_MASK  /* u8 */
	TCA_FLOWER_KEY_ARP_SHA      /* ETH_ALEN */
	TCA_FLOWER_KEY_ARP_SHA_MASK /* ETH_ALEN */
	TCA_FLOWER_KEY_ARP_THA      /* ETH_ALEN */
	TCA_FLOWER_KEY_ARP_THA_MASK /* ETH_ALEN */

	TCA_FLOWER_KEY_MPLS_TTL   /* u8 - 8 bits */
	TCA_FLOWER_KEY_MPLS_BOS   /* u8 - 1 bit */
	TCA_FLOWER_KEY_MPLS_TC    /* u8 - 3 bits */
	TCA_FLOWER_KEY_MPLS_LABEL /* be32 - 20 bits */

	TCA_FLOWER_KEY_TCP_FLAGS      /* be16 */
	TCA_FLOWER_KEY_TCP_FLAGS_MASK /* be16 */

	TCA_FLOWER_KEY_IP_TOS      /* u8 */
	TCA_FLOWER_KEY_IP_TOS_MASK /* u8 */
	TCA_FLOWER_KEY_IP_TTL      /* u8 */
	TCA_FLOWER_KEY_IP_TTL_MASK /* u8 */

	TCA_FLOWER_KEY_CVLAN_ID       /* be16 */
	TCA_FLOWER_KEY_CVLAN_PRIO     /* u8   */
	TCA_FLOWER_KEY_CVLAN_ETH_TYPE /* be16 */

	TCA_FLOWER_KEY_ENC_IP_TOS      /* u8 */
	TCA_FLOWER_KEY_ENC_IP_TOS_MASK /* u8 */
	TCA_FLOWER_KEY_ENC_IP_TTL      /* u8 */
	TCA_FLOWER_KEY_ENC_IP_TTL_MASK /* u8 */

	TCA_FLOWER_KEY_ENC_OPTS
	TCA_FLOWER_KEY_ENC_OPTS_MASK

	__TCA_FLOWER_MAX
)

const TCA_CLS_FLAGS_SKIP_HW = 1 << 0 /* don't offload filter to HW */
const TCA_CLS_FLAGS_SKIP_SW = 1 << 1 /* don't use filter in SW */

// struct tc_sfq_qopt {
// 	unsigned	quantum;	/* Bytes per round allocated to flow */
// 	int		perturb_period;	/* Period of hash perturbation */
// 	__u32		limit;		/* Maximal packets in queue */
// 	unsigned	divisor;	/* Hash divisor  */
// 	unsigned	flows;		/* Maximal number of flows  */
// };

type TcSfqQopt struct {
	Quantum uint8
	Perturb int32
	Limit   uint32
	Divisor uint8
	Flows   uint8
}

func (x *TcSfqQopt) Len() int {
	return SizeofTcSfqQopt
}

func DeserializeTcSfqQopt(b []byte) *TcSfqQopt {
	return (*TcSfqQopt)(unsafe.Pointer(&b[0:SizeofTcSfqQopt][0]))
}

func (x *TcSfqQopt) Serialize() []byte {
	return (*(*[SizeofTcSfqQopt]byte)(unsafe.Pointer(x)))[:]
}

//	struct tc_sfqred_stats {
//		__u32           prob_drop;      /* Early drops, below max threshold */
//		__u32           forced_drop;	/* Early drops, after max threshold */
//		__u32           prob_mark;      /* Marked packets, below max threshold */
//		__u32           forced_mark;    /* Marked packets, after max threshold */
//		__u32           prob_mark_head; /* Marked packets, below max threshold */
//		__u32           forced_mark_head;/* Marked packets, after max threshold */
//	};
type TcSfqRedStats struct {
	ProbDrop       uint32
	ForcedDrop     uint32
	ProbMark       uint32
	ForcedMark     uint32
	ProbMarkHead   uint32
	ForcedMarkHead uint32
}

func (x *TcSfqRedStats) Len() int {
	return SizeofTcSfqRedStats
}

func DeserializeTcSfqRedStats(b []byte) *TcSfqRedStats {
	return (*TcSfqRedStats)(unsafe.Pointer(&b[0:SizeofTcSfqRedStats][0]))
}

func (x *TcSfqRedStats) Serialize() []byte {
	return (*(*[SizeofTcSfqRedStats]byte)(unsafe.Pointer(x)))[:]
}

//	struct tc_sfq_qopt_v1 {
//		struct tc_sfq_qopt v0;
//		unsigned int	depth;		/* max number of packets per flow */
//		unsigned int	headdrop;
//
// /* SFQRED parameters */
//
//	__u32		limit;		/* HARD maximal flow queue length (bytes) */
//	__u32		qth_min;	/* Min average length threshold (bytes) */
//	__u32		qth_max;	/* Max average length threshold (bytes) */
//	unsigned char   Wlog;		/* log(W)		*/
//	unsigned char   Plog;		/* log(P_max/(qth_max-qth_min))	*/
//	unsigned char   Scell_log;	/* cell size for idle damping */
//	unsigned char	flags;
//	__u32		max_P;		/* probability, high resolution */
//
// /* SFQRED stats */
//
//		struct tc_sfqred_stats stats;
//	};
type TcSfqQoptV1 struct {
	TcSfqQopt
	Depth    uint32
	HeadDrop uint32
	Limit    uint32
	QthMin   uint32
	QthMax   uint32
	Wlog     byte
	Plog     byte
	ScellLog byte
	Flags    byte
	MaxP     uint32
	TcSfqRedStats
}

func (x *TcSfqQoptV1) Len() int {
	return SizeofTcSfqQoptV1
}

func DeserializeTcSfqQoptV1(b []byte) *TcSfqQoptV1 {
	return (*TcSfqQoptV1)(unsafe.Pointer(&b[0:SizeofTcSfqQoptV1][0]))
}

func (x *TcSfqQoptV1) Serialize() []byte {
	return (*(*[SizeofTcSfqQoptV1]byte)(unsafe.Pointer(x)))[:]
}

// IPProto represents Flower ip_proto attribute
type IPProto uint8

const (
	IPPROTO_TCP    IPProto = unix.IPPROTO_TCP
	IPPROTO_UDP    IPProto = unix.IPPROTO_UDP
	IPPROTO_SCTP   IPProto = unix.IPPROTO_SCTP
	IPPROTO_ICMP   IPProto = unix.IPPROTO_ICMP
	IPPROTO_ICMPV6 IPProto = unix.IPPROTO_ICMPV6
)

func (i IPProto) Serialize() []byte {
	arr := make([]byte, 1)
	arr[0] = byte(i)
	return arr
}

func (i IPProto) String() string {
	switch i {
	case IPPROTO_TCP:
		return "tcp"
	case IPPROTO_UDP:
		return "udp"
	case IPPROTO_SCTP:
		return "sctp"
	case IPPROTO_ICMP:
		return "icmp"
	case IPPROTO_ICMPV6:
		return "icmpv6"
	}
	return fmt.Sprintf("%d", i)
}

const (
	MaxOffs        = 128
	SizeOfPeditSel = 24
	SizeOfPeditKey = 24

	TCA_PEDIT_KEY_EX_HTYPE = 1
	TCA_PEDIT_KEY_EX_CMD   = 2
)

const (
	TCA_PEDIT_UNSPEC = iota
	TCA_PEDIT_TM
	TCA_PEDIT_PARMS
	TCA_PEDIT_PAD
	TCA_PEDIT_PARMS_EX
	TCA_PEDIT_KEYS_EX
	TCA_PEDIT_KEY_EX
)

// /* TCA_PEDIT_KEY_EX_HDR_TYPE_NETWROK is a special case for legacy users. It
//   - means no specific header type - offset is relative to the network layer
//     */
type PeditHeaderType uint16

const (
	TCA_PEDIT_KEY_EX_HDR_TYPE_NETWORK = iota
	TCA_PEDIT_KEY_EX_HDR_TYPE_ETH
	TCA_PEDIT_KEY_EX_HDR_TYPE_IP4
	TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	TCA_PEDIT_KEY_EX_HDR_TYPE_TCP
	TCA_PEDIT_KEY_EX_HDR_TYPE_UDP
	__PEDIT_HDR_TYPE_MAX
)

type PeditCmd uint16

const (
	TCA_PEDIT_KEY_EX_CMD_SET = 0
	TCA_PEDIT_KEY_EX_CMD_ADD = 1
)

type TcPeditSel struct {
	TcGen
	NKeys uint8
	Flags uint8
}

func DeserializeTcPeditKey(b []byte) *TcPeditKey {
	return (*TcPeditKey)(unsafe.Pointer(&b[0:SizeOfPeditKey][0]))
}

func DeserializeTcPedit(b []byte) (*TcPeditSel, []TcPeditKey) {
	x := &TcPeditSel{}
	copy((*(*[SizeOfPeditSel]byte)(unsafe.Pointer(x)))[:SizeOfPeditSel], b)

	var keys []TcPeditKey

	next := SizeOfPeditKey
	var i uint8
	for i = 0; i < x.NKeys; i++ {
		keys = append(keys, *DeserializeTcPeditKey(b[next:]))
		next += SizeOfPeditKey
	}

	return x, keys
}

type TcPeditKey struct {
	Mask    uint32
	Val     uint32
	Off     uint32
	At      uint32
	OffMask uint32
	Shift   uint32
}

type TcPeditKeyEx struct {
	HeaderType PeditHeaderType
	Cmd        PeditCmd
}

type TcPedit struct {
	Sel    TcPeditSel
	Keys   []TcPeditKey
	KeysEx []TcPeditKeyEx
	Extend uint8
}

func (p *TcPedit) Encode(parent *RtAttr) {
	parent.AddRtAttr(TCA_ACT_KIND, ZeroTerminated("pedit"))
	actOpts := parent.AddRtAttr(TCA_ACT_OPTIONS, nil)

	bbuf := bytes.NewBuffer(make([]byte, 0, int(unsafe.Sizeof(p.Sel)+unsafe.Sizeof(p.Keys))))

	bbuf.Write((*(*[SizeOfPeditSel]byte)(unsafe.Pointer(&p.Sel)))[:])

	for i := uint8(0); i < p.Sel.NKeys; i++ {
		bbuf.Write((*(*[SizeOfPeditKey]byte)(unsafe.Pointer(&p.Keys[i])))[:])
	}
	actOpts.AddRtAttr(TCA_PEDIT_PARMS_EX, bbuf.Bytes())

	exAttrs := actOpts.AddRtAttr(int(TCA_PEDIT_KEYS_EX|NLA_F_NESTED), nil)
	for i := uint8(0); i < p.Sel.NKeys; i++ {
		keyAttr := exAttrs.AddRtAttr(int(TCA_PEDIT_KEY_EX|NLA_F_NESTED), nil)

		htypeBuf := make([]byte, 2)
		cmdBuf := make([]byte, 2)

		NativeEndian().PutUint16(htypeBuf, uint16(p.KeysEx[i].HeaderType))
		NativeEndian().PutUint16(cmdBuf, uint16(p.KeysEx[i].Cmd))

		keyAttr.AddRtAttr(TCA_PEDIT_KEY_EX_HTYPE, htypeBuf)
		keyAttr.AddRtAttr(TCA_PEDIT_KEY_EX_CMD, cmdBuf)
	}
}

func (p *TcPedit) SetEthDst(mac net.HardwareAddr) {
	u32 := NativeEndian().Uint32(mac)
	u16 := NativeEndian().Uint16(mac[4:])

	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	tKey.Val = u32

	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_ETH
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++

	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = uint32(u16)
	tKey.Mask = 0xffff0000
	tKey.Off = 4
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_ETH
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++
}

func (p *TcPedit) SetEthSrc(mac net.HardwareAddr) {
	u16 := NativeEndian().Uint16(mac)
	u32 := NativeEndian().Uint32(mac[2:])

	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	tKey.Val = uint32(u16) << 16
	tKey.Mask = 0x0000ffff
	tKey.Off = 4

	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_ETH
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++

	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Mask = 0
	tKey.Off = 8

	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_ETH
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++
}

func (p *TcPedit) SetIPv6Src(ip6 net.IP) {
	u32 := NativeEndian().Uint32(ip6[:4])

	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 8
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++

	u32 = NativeEndian().Uint32(ip6[4:8])
	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 12
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++

	u32 = NativeEndian().Uint32(ip6[8:12])
	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 16
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++

	u32 = NativeEndian().Uint32(ip6[12:16])
	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 20
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++
}

func (p *TcPedit) SetDstIP(ip net.IP) {
	if ip.To4() != nil {
		p.SetIPv4Dst(ip)
	} else {
		p.SetIPv6Dst(ip)
	}
}

func (p *TcPedit) SetSrcIP(ip net.IP) {
	if ip.To4() != nil {
		p.SetIPv4Src(ip)
	} else {
		p.SetIPv6Src(ip)
	}
}

func (p *TcPedit) SetIPv6Dst(ip6 net.IP) {
	u32 := NativeEndian().Uint32(ip6[:4])

	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 24
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++

	u32 = NativeEndian().Uint32(ip6[4:8])
	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 28
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++

	u32 = NativeEndian().Uint32(ip6[8:12])
	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 32
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++

	u32 = NativeEndian().Uint32(ip6[12:16])
	tKey = TcPeditKey{}
	tKeyEx = TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 36
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP6
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)

	p.Sel.NKeys++
}

func (p *TcPedit) SetIPv4Src(ip net.IP) {
	u32 := NativeEndian().Uint32(ip[:4])

	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 12
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP4
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++
}

func (p *TcPedit) SetIPv4Dst(ip net.IP) {
	u32 := NativeEndian().Uint32(ip[:4])

	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	tKey.Val = u32
	tKey.Off = 16
	tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_IP4
	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++
}

// SetDstPort only tcp and udp are supported to set port
func (p *TcPedit) SetDstPort(dstPort uint16, protocol uint8) {
	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	switch protocol {
	case unix.IPPROTO_TCP:
		tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_TCP
	case unix.IPPROTO_UDP:
		tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_UDP
	default:
		return
	}

	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	tKey.Val = uint32(Swap16(dstPort)) << 16
	tKey.Mask = 0x0000ffff
	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++
}

// SetSrcPort only tcp and udp are supported to set port
func (p *TcPedit) SetSrcPort(srcPort uint16, protocol uint8) {
	tKey := TcPeditKey{}
	tKeyEx := TcPeditKeyEx{}

	switch protocol {
	case unix.IPPROTO_TCP:
		tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_TCP
	case unix.IPPROTO_UDP:
		tKeyEx.HeaderType = TCA_PEDIT_KEY_EX_HDR_TYPE_UDP
	default:
		return
	}

	tKeyEx.Cmd = TCA_PEDIT_KEY_EX_CMD_SET

	tKey.Val = uint32(Swap16(srcPort))
	tKey.Mask = 0xffff0000
	p.Keys = append(p.Keys, tKey)
	p.KeysEx = append(p.KeysEx, tKeyEx)
	p.Sel.NKeys++
}
