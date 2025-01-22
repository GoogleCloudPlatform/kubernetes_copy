package nl

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unsafe"
)

const (
	DEFAULT_CHANGE = 0xFFFFFFFF
)

const (
	IFLA_INFO_UNSPEC = iota
	IFLA_INFO_KIND
	IFLA_INFO_DATA
	IFLA_INFO_XSTATS
	IFLA_INFO_SLAVE_KIND
	IFLA_INFO_SLAVE_DATA
	IFLA_INFO_MAX = IFLA_INFO_SLAVE_DATA
)

const (
	IFLA_VLAN_UNSPEC = iota
	IFLA_VLAN_ID
	IFLA_VLAN_FLAGS
	IFLA_VLAN_EGRESS_QOS
	IFLA_VLAN_INGRESS_QOS
	IFLA_VLAN_PROTOCOL
	IFLA_VLAN_MAX = IFLA_VLAN_PROTOCOL
)

const (
	IFLA_NETKIT_UNSPEC = iota
	IFLA_NETKIT_PEER_INFO
	IFLA_NETKIT_PRIMARY
	IFLA_NETKIT_POLICY
	IFLA_NETKIT_PEER_POLICY
	IFLA_NETKIT_MODE
	IFLA_NETKIT_SCRUB
	IFLA_NETKIT_PEER_SCRUB
	IFLA_NETKIT_MAX = IFLA_NETKIT_MODE
)

const (
	VETH_INFO_UNSPEC = iota
	VETH_INFO_PEER
	VETH_INFO_MAX = VETH_INFO_PEER
)

const (
	IFLA_VXLAN_UNSPEC = iota
	IFLA_VXLAN_ID
	IFLA_VXLAN_GROUP
	IFLA_VXLAN_LINK
	IFLA_VXLAN_LOCAL
	IFLA_VXLAN_TTL
	IFLA_VXLAN_TOS
	IFLA_VXLAN_LEARNING
	IFLA_VXLAN_AGEING
	IFLA_VXLAN_LIMIT
	IFLA_VXLAN_PORT_RANGE
	IFLA_VXLAN_PROXY
	IFLA_VXLAN_RSC
	IFLA_VXLAN_L2MISS
	IFLA_VXLAN_L3MISS
	IFLA_VXLAN_PORT
	IFLA_VXLAN_GROUP6
	IFLA_VXLAN_LOCAL6
	IFLA_VXLAN_UDP_CSUM
	IFLA_VXLAN_UDP_ZERO_CSUM6_TX
	IFLA_VXLAN_UDP_ZERO_CSUM6_RX
	IFLA_VXLAN_REMCSUM_TX
	IFLA_VXLAN_REMCSUM_RX
	IFLA_VXLAN_GBP
	IFLA_VXLAN_REMCSUM_NOPARTIAL
	IFLA_VXLAN_FLOWBASED
	IFLA_VXLAN_MAX = IFLA_VXLAN_FLOWBASED
)

const (
	BRIDGE_MODE_UNSPEC = iota
	BRIDGE_MODE_HAIRPIN
)

const (
	IFLA_BRPORT_UNSPEC = iota
	IFLA_BRPORT_STATE
	IFLA_BRPORT_PRIORITY
	IFLA_BRPORT_COST
	IFLA_BRPORT_MODE
	IFLA_BRPORT_GUARD
	IFLA_BRPORT_PROTECT
	IFLA_BRPORT_FAST_LEAVE
	IFLA_BRPORT_LEARNING
	IFLA_BRPORT_UNICAST_FLOOD
	IFLA_BRPORT_PROXYARP
	IFLA_BRPORT_LEARNING_SYNC
	IFLA_BRPORT_PROXYARP_WIFI
	IFLA_BRPORT_ROOT_ID
	IFLA_BRPORT_BRIDGE_ID
	IFLA_BRPORT_DESIGNATED_PORT
	IFLA_BRPORT_DESIGNATED_COST
	IFLA_BRPORT_ID
	IFLA_BRPORT_NO
	IFLA_BRPORT_TOPOLOGY_CHANGE_ACK
	IFLA_BRPORT_CONFIG_PENDING
	IFLA_BRPORT_MESSAGE_AGE_TIMER
	IFLA_BRPORT_FORWARD_DELAY_TIMER
	IFLA_BRPORT_HOLD_TIMER
	IFLA_BRPORT_FLUSH
	IFLA_BRPORT_MULTICAST_ROUTER
	IFLA_BRPORT_PAD
	IFLA_BRPORT_MCAST_FLOOD
	IFLA_BRPORT_MCAST_TO_UCAST
	IFLA_BRPORT_VLAN_TUNNEL
	IFLA_BRPORT_BCAST_FLOOD
	IFLA_BRPORT_GROUP_FWD_MASK
	IFLA_BRPORT_NEIGH_SUPPRESS
	IFLA_BRPORT_ISOLATED
	IFLA_BRPORT_BACKUP_PORT
	IFLA_BRPORT_MRP_RING_OPEN
	IFLA_BRPORT_MRP_IN_OPEN
	IFLA_BRPORT_MCAST_EHT_HOSTS_LIMIT
	IFLA_BRPORT_MCAST_EHT_HOSTS_CNT
	IFLA_BRPORT_LOCKED
	IFLA_BRPORT_MAB
	IFLA_BRPORT_MCAST_N_GROUPS
	IFLA_BRPORT_MCAST_MAX_GROUPS
	IFLA_BRPORT_MAX = IFLA_BRPORT_MCAST_MAX_GROUPS
)

const (
	IFLA_IPVLAN_UNSPEC = iota
	IFLA_IPVLAN_MODE
	IFLA_IPVLAN_FLAG
	IFLA_IPVLAN_MAX = IFLA_IPVLAN_FLAG
)

const (
	IFLA_MACVLAN_UNSPEC = iota
	IFLA_MACVLAN_MODE
	IFLA_MACVLAN_FLAGS
	IFLA_MACVLAN_MACADDR_MODE
	IFLA_MACVLAN_MACADDR
	IFLA_MACVLAN_MACADDR_DATA
	IFLA_MACVLAN_MACADDR_COUNT
	IFLA_MACVLAN_BC_QUEUE_LEN
	IFLA_MACVLAN_BC_QUEUE_LEN_USED
	IFLA_MACVLAN_MAX = IFLA_MACVLAN_BC_QUEUE_LEN_USED
)

const (
	MACVLAN_MODE_PRIVATE  = 1
	MACVLAN_MODE_VEPA     = 2
	MACVLAN_MODE_BRIDGE   = 4
	MACVLAN_MODE_PASSTHRU = 8
	MACVLAN_MODE_SOURCE   = 16
)

const (
	MACVLAN_MACADDR_ADD = iota
	MACVLAN_MACADDR_DEL
	MACVLAN_MACADDR_FLUSH
	MACVLAN_MACADDR_SET
)

const (
	IFLA_BOND_UNSPEC = iota
	IFLA_BOND_MODE
	IFLA_BOND_ACTIVE_SLAVE
	IFLA_BOND_MIIMON
	IFLA_BOND_UPDELAY
	IFLA_BOND_DOWNDELAY
	IFLA_BOND_USE_CARRIER
	IFLA_BOND_ARP_INTERVAL
	IFLA_BOND_ARP_IP_TARGET
	IFLA_BOND_ARP_VALIDATE
	IFLA_BOND_ARP_ALL_TARGETS
	IFLA_BOND_PRIMARY
	IFLA_BOND_PRIMARY_RESELECT
	IFLA_BOND_FAIL_OVER_MAC
	IFLA_BOND_XMIT_HASH_POLICY
	IFLA_BOND_RESEND_IGMP
	IFLA_BOND_NUM_PEER_NOTIF
	IFLA_BOND_ALL_SLAVES_ACTIVE
	IFLA_BOND_MIN_LINKS
	IFLA_BOND_LP_INTERVAL
	IFLA_BOND_PACKETS_PER_SLAVE
	IFLA_BOND_AD_LACP_RATE
	IFLA_BOND_AD_SELECT
	IFLA_BOND_AD_INFO
	IFLA_BOND_AD_ACTOR_SYS_PRIO
	IFLA_BOND_AD_USER_PORT_KEY
	IFLA_BOND_AD_ACTOR_SYSTEM
	IFLA_BOND_TLB_DYNAMIC_LB
)

const (
	IFLA_BOND_AD_INFO_UNSPEC = iota
	IFLA_BOND_AD_INFO_AGGREGATOR
	IFLA_BOND_AD_INFO_NUM_PORTS
	IFLA_BOND_AD_INFO_ACTOR_KEY
	IFLA_BOND_AD_INFO_PARTNER_KEY
	IFLA_BOND_AD_INFO_PARTNER_MAC
)

const (
	IFLA_BOND_SLAVE_UNSPEC = iota
	IFLA_BOND_SLAVE_STATE
	IFLA_BOND_SLAVE_MII_STATUS
	IFLA_BOND_SLAVE_LINK_FAILURE_COUNT
	IFLA_BOND_SLAVE_PERM_HWADDR
	IFLA_BOND_SLAVE_QUEUE_ID
	IFLA_BOND_SLAVE_AD_AGGREGATOR_ID
	IFLA_BOND_SLAVE_AD_ACTOR_OPER_PORT_STATE
	IFLA_BOND_SLAVE_AD_PARTNER_OPER_PORT_STATE
)

const (
	IFLA_GENEVE_UNSPEC = iota
	IFLA_GENEVE_ID     // vni
	IFLA_GENEVE_REMOTE
	IFLA_GENEVE_TTL
	IFLA_GENEVE_TOS
	IFLA_GENEVE_PORT // destination port
	IFLA_GENEVE_COLLECT_METADATA
	IFLA_GENEVE_REMOTE6
	IFLA_GENEVE_UDP_CSUM
	IFLA_GENEVE_UDP_ZERO_CSUM6_TX
	IFLA_GENEVE_UDP_ZERO_CSUM6_RX
	IFLA_GENEVE_LABEL
	IFLA_GENEVE_TTL_INHERIT
	IFLA_GENEVE_DF
	IFLA_GENEVE_INNER_PROTO_INHERIT
	IFLA_GENEVE_MAX = IFLA_GENEVE_INNER_PROTO_INHERIT
)

const (
	IFLA_GRE_UNSPEC = iota
	IFLA_GRE_LINK
	IFLA_GRE_IFLAGS
	IFLA_GRE_OFLAGS
	IFLA_GRE_IKEY
	IFLA_GRE_OKEY
	IFLA_GRE_LOCAL
	IFLA_GRE_REMOTE
	IFLA_GRE_TTL
	IFLA_GRE_TOS
	IFLA_GRE_PMTUDISC
	IFLA_GRE_ENCAP_LIMIT
	IFLA_GRE_FLOWINFO
	IFLA_GRE_FLAGS
	IFLA_GRE_ENCAP_TYPE
	IFLA_GRE_ENCAP_FLAGS
	IFLA_GRE_ENCAP_SPORT
	IFLA_GRE_ENCAP_DPORT
	IFLA_GRE_COLLECT_METADATA
	IFLA_GRE_MAX = IFLA_GRE_COLLECT_METADATA
)

const (
	GRE_CSUM    = 0x8000
	GRE_ROUTING = 0x4000
	GRE_KEY     = 0x2000
	GRE_SEQ     = 0x1000
	GRE_STRICT  = 0x0800
	GRE_REC     = 0x0700
	GRE_FLAGS   = 0x00F8
	GRE_VERSION = 0x0007
)

const (
	IFLA_VF_INFO_UNSPEC = iota
	IFLA_VF_INFO
	IFLA_VF_INFO_MAX = IFLA_VF_INFO
)

const (
	IFLA_VF_UNSPEC = iota
	IFLA_VF_MAC    /* Hardware queue specific attributes */
	IFLA_VF_VLAN
	IFLA_VF_TX_RATE      /* Max TX Bandwidth Allocation */
	IFLA_VF_SPOOFCHK     /* Spoof Checking on/off switch */
	IFLA_VF_LINK_STATE   /* link state enable/disable/auto switch */
	IFLA_VF_RATE         /* Min and Max TX Bandwidth Allocation */
	IFLA_VF_RSS_QUERY_EN /* RSS Redirection Table and Hash Key query
	 * on/off switch
	 */
	IFLA_VF_STATS        /* network device statistics */
	IFLA_VF_TRUST        /* Trust state of VF */
	IFLA_VF_IB_NODE_GUID /* VF Infiniband node GUID */
	IFLA_VF_IB_PORT_GUID /* VF Infiniband port GUID */
	IFLA_VF_VLAN_LIST    /* nested list of vlans, option for QinQ */

	IFLA_VF_MAX = IFLA_VF_IB_PORT_GUID
)

const (
	IFLA_VF_VLAN_INFO_UNSPEC = iota
	IFLA_VF_VLAN_INFO        /* VLAN ID, QoS and VLAN protocol */
	__IFLA_VF_VLAN_INFO_MAX
)

const (
	IFLA_VF_LINK_STATE_AUTO    = iota /* link state of the uplink */
	IFLA_VF_LINK_STATE_ENABLE         /* link always up */
	IFLA_VF_LINK_STATE_DISABLE        /* link always down */
	IFLA_VF_LINK_STATE_MAX     = IFLA_VF_LINK_STATE_DISABLE
)

const (
	IFLA_VF_STATS_RX_PACKETS = iota
	IFLA_VF_STATS_TX_PACKETS
	IFLA_VF_STATS_RX_BYTES
	IFLA_VF_STATS_TX_BYTES
	IFLA_VF_STATS_BROADCAST
	IFLA_VF_STATS_MULTICAST
	IFLA_VF_STATS_RX_DROPPED
	IFLA_VF_STATS_TX_DROPPED
	IFLA_VF_STATS_MAX = IFLA_VF_STATS_TX_DROPPED
)

const (
	SizeofVfMac        = 0x24
	SizeofVfVlan       = 0x0c
	SizeofVfVlanInfo   = 0x10
	SizeofVfTxRate     = 0x08
	SizeofVfRate       = 0x0c
	SizeofVfSpoofchk   = 0x08
	SizeofVfLinkState  = 0x08
	SizeofVfRssQueryEn = 0x08
	SizeofVfTrust      = 0x08
	SizeofVfGUID       = 0x10
)

// struct ifla_vf_mac {
//   __u32 vf;
//   __u8 mac[32]; /* MAX_ADDR_LEN */
// };

type VfMac struct {
	Vf  uint32
	Mac [32]byte
}

func (msg *VfMac) Len() int {
	return SizeofVfMac
}

func DeserializeVfMac(b []byte) *VfMac {
	return (*VfMac)(unsafe.Pointer(&b[0:SizeofVfMac][0]))
}

func (msg *VfMac) Serialize() []byte {
	return (*(*[SizeofVfMac]byte)(unsafe.Pointer(msg)))[:]
}

// struct ifla_vf_vlan {
//   __u32 vf;
//   __u32 vlan; /* 0 - 4095, 0 disables VLAN filter */
//   __u32 qos;
// };

type VfVlan struct {
	Vf   uint32
	Vlan uint32
	Qos  uint32
}

func (msg *VfVlan) Len() int {
	return SizeofVfVlan
}

func DeserializeVfVlan(b []byte) *VfVlan {
	return (*VfVlan)(unsafe.Pointer(&b[0:SizeofVfVlan][0]))
}

func (msg *VfVlan) Serialize() []byte {
	return (*(*[SizeofVfVlan]byte)(unsafe.Pointer(msg)))[:]
}

func DeserializeVfVlanList(b []byte) ([]*VfVlanInfo, error) {
	var vfVlanInfoList []*VfVlanInfo
	attrs, err := ParseRouteAttr(b)
	if err != nil {
		return nil, err
	}

	for _, element := range attrs {
		if element.Attr.Type == IFLA_VF_VLAN_INFO {
			vfVlanInfoList = append(vfVlanInfoList, DeserializeVfVlanInfo(element.Value))
		}
	}

	if len(vfVlanInfoList) == 0 {
		return nil, fmt.Errorf("VF vlan list is defined but no vf vlan info elements were found")
	}

	return vfVlanInfoList, nil
}

// struct ifla_vf_vlan_info {
//   __u32 vf;
//   __u32 vlan; /* 0 - 4095, 0 disables VLAN filter */
//   __u32 qos;
//   __be16 vlan_proto; /* VLAN protocol either 802.1Q or 802.1ad */
// };

type VfVlanInfo struct {
	VfVlan
	VlanProto uint16
}

func DeserializeVfVlanInfo(b []byte) *VfVlanInfo {
	return &VfVlanInfo{
		*(*VfVlan)(unsafe.Pointer(&b[0:SizeofVfVlan][0])),
		binary.BigEndian.Uint16(b[SizeofVfVlan:SizeofVfVlanInfo]),
	}
}

func (msg *VfVlanInfo) Serialize() []byte {
	return (*(*[SizeofVfVlanInfo]byte)(unsafe.Pointer(msg)))[:]
}

// struct ifla_vf_tx_rate {
//   __u32 vf;
//   __u32 rate; /* Max TX bandwidth in Mbps, 0 disables throttling */
// };

type VfTxRate struct {
	Vf   uint32
	Rate uint32
}

func (msg *VfTxRate) Len() int {
	return SizeofVfTxRate
}

func DeserializeVfTxRate(b []byte) *VfTxRate {
	return (*VfTxRate)(unsafe.Pointer(&b[0:SizeofVfTxRate][0]))
}

func (msg *VfTxRate) Serialize() []byte {
	return (*(*[SizeofVfTxRate]byte)(unsafe.Pointer(msg)))[:]
}

//struct ifla_vf_stats {
//	__u64 rx_packets;
//	__u64 tx_packets;
//	__u64 rx_bytes;
//	__u64 tx_bytes;
//	__u64 broadcast;
//	__u64 multicast;
//};

type VfStats struct {
	RxPackets uint64
	TxPackets uint64
	RxBytes   uint64
	TxBytes   uint64
	Multicast uint64
	Broadcast uint64
	RxDropped uint64
	TxDropped uint64
}

func DeserializeVfStats(b []byte) VfStats {
	var vfstat VfStats
	stats, err := ParseRouteAttr(b)
	if err != nil {
		return vfstat
	}
	var valueVar uint64
	for _, stat := range stats {
		if err := binary.Read(bytes.NewBuffer(stat.Value), NativeEndian(), &valueVar); err != nil {
			break
		}
		switch stat.Attr.Type {
		case IFLA_VF_STATS_RX_PACKETS:
			vfstat.RxPackets = valueVar
		case IFLA_VF_STATS_TX_PACKETS:
			vfstat.TxPackets = valueVar
		case IFLA_VF_STATS_RX_BYTES:
			vfstat.RxBytes = valueVar
		case IFLA_VF_STATS_TX_BYTES:
			vfstat.TxBytes = valueVar
		case IFLA_VF_STATS_MULTICAST:
			vfstat.Multicast = valueVar
		case IFLA_VF_STATS_BROADCAST:
			vfstat.Broadcast = valueVar
		case IFLA_VF_STATS_RX_DROPPED:
			vfstat.RxDropped = valueVar
		case IFLA_VF_STATS_TX_DROPPED:
			vfstat.TxDropped = valueVar
		}
	}
	return vfstat
}

// struct ifla_vf_rate {
//   __u32 vf;
//   __u32 min_tx_rate; /* Min Bandwidth in Mbps */
//   __u32 max_tx_rate; /* Max Bandwidth in Mbps */
// };

type VfRate struct {
	Vf        uint32
	MinTxRate uint32
	MaxTxRate uint32
}

func (msg *VfRate) Len() int {
	return SizeofVfRate
}

func DeserializeVfRate(b []byte) *VfRate {
	return (*VfRate)(unsafe.Pointer(&b[0:SizeofVfRate][0]))
}

func (msg *VfRate) Serialize() []byte {
	return (*(*[SizeofVfRate]byte)(unsafe.Pointer(msg)))[:]
}

// struct ifla_vf_spoofchk {
//   __u32 vf;
//   __u32 setting;
// };

type VfSpoofchk struct {
	Vf      uint32
	Setting uint32
}

func (msg *VfSpoofchk) Len() int {
	return SizeofVfSpoofchk
}

func DeserializeVfSpoofchk(b []byte) *VfSpoofchk {
	return (*VfSpoofchk)(unsafe.Pointer(&b[0:SizeofVfSpoofchk][0]))
}

func (msg *VfSpoofchk) Serialize() []byte {
	return (*(*[SizeofVfSpoofchk]byte)(unsafe.Pointer(msg)))[:]
}

// struct ifla_vf_link_state {
//   __u32 vf;
//   __u32 link_state;
// };

type VfLinkState struct {
	Vf        uint32
	LinkState uint32
}

func (msg *VfLinkState) Len() int {
	return SizeofVfLinkState
}

func DeserializeVfLinkState(b []byte) *VfLinkState {
	return (*VfLinkState)(unsafe.Pointer(&b[0:SizeofVfLinkState][0]))
}

func (msg *VfLinkState) Serialize() []byte {
	return (*(*[SizeofVfLinkState]byte)(unsafe.Pointer(msg)))[:]
}

// struct ifla_vf_rss_query_en {
//   __u32 vf;
//   __u32 setting;
// };

type VfRssQueryEn struct {
	Vf      uint32
	Setting uint32
}

func (msg *VfRssQueryEn) Len() int {
	return SizeofVfRssQueryEn
}

func DeserializeVfRssQueryEn(b []byte) *VfRssQueryEn {
	return (*VfRssQueryEn)(unsafe.Pointer(&b[0:SizeofVfRssQueryEn][0]))
}

func (msg *VfRssQueryEn) Serialize() []byte {
	return (*(*[SizeofVfRssQueryEn]byte)(unsafe.Pointer(msg)))[:]
}

// struct ifla_vf_trust {
//   __u32 vf;
//   __u32 setting;
// };

type VfTrust struct {
	Vf      uint32
	Setting uint32
}

func (msg *VfTrust) Len() int {
	return SizeofVfTrust
}

func DeserializeVfTrust(b []byte) *VfTrust {
	return (*VfTrust)(unsafe.Pointer(&b[0:SizeofVfTrust][0]))
}

func (msg *VfTrust) Serialize() []byte {
	return (*(*[SizeofVfTrust]byte)(unsafe.Pointer(msg)))[:]
}

// struct ifla_vf_guid {
//   __u32 vf;
//   __u32 rsvd;
//   __u64 guid;
// };

type VfGUID struct {
	Vf   uint32
	Rsvd uint32
	GUID uint64
}

func (msg *VfGUID) Len() int {
	return SizeofVfGUID
}

func DeserializeVfGUID(b []byte) *VfGUID {
	return (*VfGUID)(unsafe.Pointer(&b[0:SizeofVfGUID][0]))
}

func (msg *VfGUID) Serialize() []byte {
	return (*(*[SizeofVfGUID]byte)(unsafe.Pointer(msg)))[:]
}

const (
	XDP_FLAGS_UPDATE_IF_NOEXIST = 1 << iota
	XDP_FLAGS_SKB_MODE
	XDP_FLAGS_DRV_MODE
	XDP_FLAGS_MASK = XDP_FLAGS_UPDATE_IF_NOEXIST | XDP_FLAGS_SKB_MODE | XDP_FLAGS_DRV_MODE
)

const (
	IFLA_XDP_UNSPEC   = iota
	IFLA_XDP_FD       /* fd of xdp program to attach, or -1 to remove */
	IFLA_XDP_ATTACHED /* read-only bool indicating if prog is attached */
	IFLA_XDP_FLAGS    /* xdp prog related flags */
	IFLA_XDP_PROG_ID  /* xdp prog id */
	IFLA_XDP_MAX      = IFLA_XDP_PROG_ID
)

// XDP program attach mode (used as dump value for IFLA_XDP_ATTACHED)
const (
	XDP_ATTACHED_NONE = iota
	XDP_ATTACHED_DRV
	XDP_ATTACHED_SKB
	XDP_ATTACHED_HW
)

const (
	IFLA_IPTUN_UNSPEC = iota
	IFLA_IPTUN_LINK
	IFLA_IPTUN_LOCAL
	IFLA_IPTUN_REMOTE
	IFLA_IPTUN_TTL
	IFLA_IPTUN_TOS
	IFLA_IPTUN_ENCAP_LIMIT
	IFLA_IPTUN_FLOWINFO
	IFLA_IPTUN_FLAGS
	IFLA_IPTUN_PROTO
	IFLA_IPTUN_PMTUDISC
	IFLA_IPTUN_6RD_PREFIX
	IFLA_IPTUN_6RD_RELAY_PREFIX
	IFLA_IPTUN_6RD_PREFIXLEN
	IFLA_IPTUN_6RD_RELAY_PREFIXLEN
	IFLA_IPTUN_ENCAP_TYPE
	IFLA_IPTUN_ENCAP_FLAGS
	IFLA_IPTUN_ENCAP_SPORT
	IFLA_IPTUN_ENCAP_DPORT
	IFLA_IPTUN_COLLECT_METADATA
	IFLA_IPTUN_MAX = IFLA_IPTUN_COLLECT_METADATA
)

const (
	IFLA_VTI_UNSPEC = iota
	IFLA_VTI_LINK
	IFLA_VTI_IKEY
	IFLA_VTI_OKEY
	IFLA_VTI_LOCAL
	IFLA_VTI_REMOTE
	IFLA_VTI_MAX = IFLA_VTI_REMOTE
)

const (
	IFLA_VRF_UNSPEC = iota
	IFLA_VRF_TABLE
)

const (
	IFLA_BR_UNSPEC = iota
	IFLA_BR_FORWARD_DELAY
	IFLA_BR_HELLO_TIME
	IFLA_BR_MAX_AGE
	IFLA_BR_AGEING_TIME
	IFLA_BR_STP_STATE
	IFLA_BR_PRIORITY
	IFLA_BR_VLAN_FILTERING
	IFLA_BR_VLAN_PROTOCOL
	IFLA_BR_GROUP_FWD_MASK
	IFLA_BR_ROOT_ID
	IFLA_BR_BRIDGE_ID
	IFLA_BR_ROOT_PORT
	IFLA_BR_ROOT_PATH_COST
	IFLA_BR_TOPOLOGY_CHANGE
	IFLA_BR_TOPOLOGY_CHANGE_DETECTED
	IFLA_BR_HELLO_TIMER
	IFLA_BR_TCN_TIMER
	IFLA_BR_TOPOLOGY_CHANGE_TIMER
	IFLA_BR_GC_TIMER
	IFLA_BR_GROUP_ADDR
	IFLA_BR_FDB_FLUSH
	IFLA_BR_MCAST_ROUTER
	IFLA_BR_MCAST_SNOOPING
	IFLA_BR_MCAST_QUERY_USE_IFADDR
	IFLA_BR_MCAST_QUERIER
	IFLA_BR_MCAST_HASH_ELASTICITY
	IFLA_BR_MCAST_HASH_MAX
	IFLA_BR_MCAST_LAST_MEMBER_CNT
	IFLA_BR_MCAST_STARTUP_QUERY_CNT
	IFLA_BR_MCAST_LAST_MEMBER_INTVL
	IFLA_BR_MCAST_MEMBERSHIP_INTVL
	IFLA_BR_MCAST_QUERIER_INTVL
	IFLA_BR_MCAST_QUERY_INTVL
	IFLA_BR_MCAST_QUERY_RESPONSE_INTVL
	IFLA_BR_MCAST_STARTUP_QUERY_INTVL
	IFLA_BR_NF_CALL_IPTABLES
	IFLA_BR_NF_CALL_IP6TABLES
	IFLA_BR_NF_CALL_ARPTABLES
	IFLA_BR_VLAN_DEFAULT_PVID
	IFLA_BR_PAD
	IFLA_BR_VLAN_STATS_ENABLED
	IFLA_BR_MCAST_STATS_ENABLED
	IFLA_BR_MCAST_IGMP_VERSION
	IFLA_BR_MCAST_MLD_VERSION
	IFLA_BR_MAX = IFLA_BR_MCAST_MLD_VERSION
)

const (
	IFLA_GTP_UNSPEC = iota
	IFLA_GTP_FD0
	IFLA_GTP_FD1
	IFLA_GTP_PDP_HASHSIZE
	IFLA_GTP_ROLE
)

const (
	GTP_ROLE_GGSN = iota
	GTP_ROLE_SGSN
)

const (
	IFLA_XFRM_UNSPEC = iota
	IFLA_XFRM_LINK
	IFLA_XFRM_IF_ID

	IFLA_XFRM_MAX = iota - 1
)

const (
	IFLA_TUN_UNSPEC = iota
	IFLA_TUN_OWNER
	IFLA_TUN_GROUP
	IFLA_TUN_TYPE
	IFLA_TUN_PI
	IFLA_TUN_VNET_HDR
	IFLA_TUN_PERSIST
	IFLA_TUN_MULTI_QUEUE
	IFLA_TUN_NUM_QUEUES
	IFLA_TUN_NUM_DISABLED_QUEUES
	IFLA_TUN_MAX = IFLA_TUN_NUM_DISABLED_QUEUES
)

const (
	IFLA_IPOIB_UNSPEC = iota
	IFLA_IPOIB_PKEY
	IFLA_IPOIB_MODE
	IFLA_IPOIB_UMCAST
	IFLA_IPOIB_MAX = IFLA_IPOIB_UMCAST
)

const (
	IFLA_CAN_UNSPEC = iota
	IFLA_CAN_BITTIMING
	IFLA_CAN_BITTIMING_CONST
	IFLA_CAN_CLOCK
	IFLA_CAN_STATE
	IFLA_CAN_CTRLMODE
	IFLA_CAN_RESTART_MS
	IFLA_CAN_RESTART
	IFLA_CAN_BERR_COUNTER
	IFLA_CAN_DATA_BITTIMING
	IFLA_CAN_DATA_BITTIMING_CONST
	IFLA_CAN_TERMINATION
	IFLA_CAN_TERMINATION_CONST
	IFLA_CAN_BITRATE_CONST
	IFLA_CAN_DATA_BITRATE_CONST
	IFLA_CAN_BITRATE_MAX
	IFLA_CAN_MAX = IFLA_CAN_BITRATE_MAX
)

const (
	IFLA_BAREUDP_UNSPEC = iota
	IFLA_BAREUDP_PORT
	IFLA_BAREUDP_ETHERTYPE
	IFLA_BAREUDP_SRCPORT_MIN
	IFLA_BAREUDP_MULTIPROTO_MODE
	IFLA_BAREUDP_MAX = IFLA_BAREUDP_MULTIPROTO_MODE
)
