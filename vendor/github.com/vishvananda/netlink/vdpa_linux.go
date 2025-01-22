package netlink

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/vishvananda/netlink/nl"
)

type vdpaDevID struct {
	Name string
	ID   uint32
}

// VDPADev contains info about VDPA device
type VDPADev struct {
	vdpaDevID
	VendorID  uint32
	MaxVQS    uint32
	MaxVQSize uint16
	MinVQSize uint16
}

// VDPADevConfig contains configuration of the VDPA device
type VDPADevConfig struct {
	vdpaDevID
	Features           uint64
	NegotiatedFeatures uint64
	Net                VDPADevConfigNet
}

// VDPADevVStats conatins vStats for the VDPA device
type VDPADevVStats struct {
	vdpaDevID
	QueueIndex         uint32
	Vendor             []VDPADevVStatsVendor
	NegotiatedFeatures uint64
}

// VDPADevVStatsVendor conatins name and value for vendor specific vstat option
type VDPADevVStatsVendor struct {
	Name  string
	Value uint64
}

// VDPADevConfigNet conatins status and net config for the VDPA device
type VDPADevConfigNet struct {
	Status VDPADevConfigNetStatus
	Cfg    VDPADevConfigNetCfg
}

// VDPADevConfigNetStatus contains info about net status
type VDPADevConfigNetStatus struct {
	LinkUp   bool
	Announce bool
}

// VDPADevConfigNetCfg contains net config for the VDPA device
type VDPADevConfigNetCfg struct {
	MACAddr net.HardwareAddr
	MaxVQP  uint16
	MTU     uint16
}

// VDPAMGMTDev conatins info about VDPA management device
type VDPAMGMTDev struct {
	BusName           string
	DevName           string
	SupportedClasses  uint64
	SupportedFeatures uint64
	MaxVQS            uint32
}

// VDPANewDevParams contains parameters for new VDPA device
// use SetBits to configure requried features for the device
// example:
//
//	VDPANewDevParams{Features: SetBits(0, VIRTIO_NET_F_MTU, VIRTIO_NET_F_CTRL_MAC_ADDR)}
type VDPANewDevParams struct {
	MACAddr  net.HardwareAddr
	MaxVQP   uint16
	MTU      uint16
	Features uint64
}

// SetBits set provided bits in the uint64 input value
// usage example:
// features := SetBits(0, VIRTIO_NET_F_MTU, VIRTIO_NET_F_CTRL_MAC_ADDR)
func SetBits(input uint64, pos ...int) uint64 {
	for _, p := range pos {
		input |= 1 << uint64(p)
	}
	return input
}

// IsBitSet check if specific bit is set in the uint64 input value
// usage example:
// hasNetClass := IsBitSet(mgmtDev, VIRTIO_ID_NET)
func IsBitSet(input uint64, pos int) bool {
	val := input & (1 << uint64(pos))
	return val > 0
}

// VDPANewDev adds new VDPA device
// Equivalent to: `vdpa dev add name <name> mgmtdev <mgmtBus>/mgmtName [params]`
func VDPANewDev(name, mgmtBus, mgmtName string, params VDPANewDevParams) error {
	return pkgHandle.VDPANewDev(name, mgmtBus, mgmtName, params)
}

// VDPADelDev removes VDPA device
// Equivalent to: `vdpa dev del <name>`
func VDPADelDev(name string) error {
	return pkgHandle.VDPADelDev(name)
}

// VDPAGetDevList returns list of VDPA devices
// Equivalent to: `vdpa dev show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func VDPAGetDevList() ([]*VDPADev, error) {
	return pkgHandle.VDPAGetDevList()
}

// VDPAGetDevByName returns VDPA device selected by name
// Equivalent to: `vdpa dev show <name>`
func VDPAGetDevByName(name string) (*VDPADev, error) {
	return pkgHandle.VDPAGetDevByName(name)
}

// VDPAGetDevConfigList returns list of VDPA devices configurations
// Equivalent to: `vdpa dev config show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func VDPAGetDevConfigList() ([]*VDPADevConfig, error) {
	return pkgHandle.VDPAGetDevConfigList()
}

// VDPAGetDevConfigByName returns VDPA device configuration selected by name
// Equivalent to: `vdpa dev config show <name>`
func VDPAGetDevConfigByName(name string) (*VDPADevConfig, error) {
	return pkgHandle.VDPAGetDevConfigByName(name)
}

// VDPAGetDevVStats returns vstats for VDPA device
// Equivalent to: `vdpa dev vstats show <name> qidx <queueIndex>`
func VDPAGetDevVStats(name string, queueIndex uint32) (*VDPADevVStats, error) {
	return pkgHandle.VDPAGetDevVStats(name, queueIndex)
}

// VDPAGetMGMTDevList returns list of mgmt devices
// Equivalent to: `vdpa mgmtdev show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func VDPAGetMGMTDevList() ([]*VDPAMGMTDev, error) {
	return pkgHandle.VDPAGetMGMTDevList()
}

// VDPAGetMGMTDevByBusAndName returns mgmt devices selected by bus and name
// Equivalent to: `vdpa mgmtdev show <bus>/<name>`
func VDPAGetMGMTDevByBusAndName(bus, name string) (*VDPAMGMTDev, error) {
	return pkgHandle.VDPAGetMGMTDevByBusAndName(bus, name)
}

type vdpaNetlinkMessage []syscall.NetlinkRouteAttr

func (id *vdpaDevID) parseIDAttribute(attr syscall.NetlinkRouteAttr) {
	switch attr.Attr.Type {
	case nl.VDPA_ATTR_DEV_NAME:
		id.Name = nl.BytesToString(attr.Value)
	case nl.VDPA_ATTR_DEV_ID:
		id.ID = native.Uint32(attr.Value)
	}
}

func (netStatus *VDPADevConfigNetStatus) parseStatusAttribute(value []byte) {
	a := native.Uint16(value)
	netStatus.Announce = (a & VIRTIO_NET_S_ANNOUNCE) > 0
	netStatus.LinkUp = (a & VIRTIO_NET_S_LINK_UP) > 0
}

func (d *VDPADev) parseAttributes(attrs vdpaNetlinkMessage) {
	for _, a := range attrs {
		d.parseIDAttribute(a)
		switch a.Attr.Type {
		case nl.VDPA_ATTR_DEV_VENDOR_ID:
			d.VendorID = native.Uint32(a.Value)
		case nl.VDPA_ATTR_DEV_MAX_VQS:
			d.MaxVQS = native.Uint32(a.Value)
		case nl.VDPA_ATTR_DEV_MAX_VQ_SIZE:
			d.MaxVQSize = native.Uint16(a.Value)
		case nl.VDPA_ATTR_DEV_MIN_VQ_SIZE:
			d.MinVQSize = native.Uint16(a.Value)
		}
	}
}

func (c *VDPADevConfig) parseAttributes(attrs vdpaNetlinkMessage) {
	for _, a := range attrs {
		c.parseIDAttribute(a)
		switch a.Attr.Type {
		case nl.VDPA_ATTR_DEV_NET_CFG_MACADDR:
			c.Net.Cfg.MACAddr = a.Value
		case nl.VDPA_ATTR_DEV_NET_STATUS:
			c.Net.Status.parseStatusAttribute(a.Value)
		case nl.VDPA_ATTR_DEV_NET_CFG_MAX_VQP:
			c.Net.Cfg.MaxVQP = native.Uint16(a.Value)
		case nl.VDPA_ATTR_DEV_NET_CFG_MTU:
			c.Net.Cfg.MTU = native.Uint16(a.Value)
		case nl.VDPA_ATTR_DEV_FEATURES:
			c.Features = native.Uint64(a.Value)
		case nl.VDPA_ATTR_DEV_NEGOTIATED_FEATURES:
			c.NegotiatedFeatures = native.Uint64(a.Value)
		}
	}
}

func (s *VDPADevVStats) parseAttributes(attrs vdpaNetlinkMessage) {
	for _, a := range attrs {
		s.parseIDAttribute(a)
		switch a.Attr.Type {
		case nl.VDPA_ATTR_DEV_QUEUE_INDEX:
			s.QueueIndex = native.Uint32(a.Value)
		case nl.VDPA_ATTR_DEV_VENDOR_ATTR_NAME:
			s.Vendor = append(s.Vendor, VDPADevVStatsVendor{Name: nl.BytesToString(a.Value)})
		case nl.VDPA_ATTR_DEV_VENDOR_ATTR_VALUE:
			if len(s.Vendor) == 0 {
				break
			}
			s.Vendor[len(s.Vendor)-1].Value = native.Uint64(a.Value)
		case nl.VDPA_ATTR_DEV_NEGOTIATED_FEATURES:
			s.NegotiatedFeatures = native.Uint64(a.Value)
		}
	}
}

func (d *VDPAMGMTDev) parseAttributes(attrs vdpaNetlinkMessage) {
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.VDPA_ATTR_MGMTDEV_BUS_NAME:
			d.BusName = nl.BytesToString(a.Value)
		case nl.VDPA_ATTR_MGMTDEV_DEV_NAME:
			d.DevName = nl.BytesToString(a.Value)
		case nl.VDPA_ATTR_MGMTDEV_SUPPORTED_CLASSES:
			d.SupportedClasses = native.Uint64(a.Value)
		case nl.VDPA_ATTR_DEV_SUPPORTED_FEATURES:
			d.SupportedFeatures = native.Uint64(a.Value)
		case nl.VDPA_ATTR_DEV_MGMTDEV_MAX_VQS:
			d.MaxVQS = native.Uint32(a.Value)
		}
	}
}

func (h *Handle) vdpaRequest(command uint8, extraFlags int, attrs []*nl.RtAttr) ([]vdpaNetlinkMessage, error) {
	f, err := h.GenlFamilyGet(nl.VDPA_GENL_NAME)
	if err != nil {
		return nil, err
	}
	req := h.newNetlinkRequest(int(f.ID), unix.NLM_F_ACK|extraFlags)
	req.AddData(&nl.Genlmsg{
		Command: command,
		Version: nl.VDPA_GENL_VERSION,
	})
	for _, a := range attrs {
		req.AddData(a)
	}

	resp, executeErr := req.Execute(unix.NETLINK_GENERIC, 0)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	messages := make([]vdpaNetlinkMessage, 0, len(resp))
	for _, m := range resp {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		messages = append(messages, attrs)
	}
	return messages, executeErr
}

// dump all devices if dev is nil
//
// If dev is nil and the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) vdpaDevGet(dev *string) ([]*VDPADev, error) {
	var extraFlags int
	var attrs []*nl.RtAttr
	if dev != nil {
		attrs = append(attrs, nl.NewRtAttr(nl.VDPA_ATTR_DEV_NAME, nl.ZeroTerminated(*dev)))
	} else {
		extraFlags = extraFlags | unix.NLM_F_DUMP
	}
	messages, executeErr := h.vdpaRequest(nl.VDPA_CMD_DEV_GET, extraFlags, attrs)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	devs := make([]*VDPADev, 0, len(messages))
	for _, m := range messages {
		d := &VDPADev{}
		d.parseAttributes(m)
		devs = append(devs, d)
	}
	return devs, executeErr
}

// dump all devices if dev is nil
//
// If dev is nil, and the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) vdpaDevConfigGet(dev *string) ([]*VDPADevConfig, error) {
	var extraFlags int
	var attrs []*nl.RtAttr
	if dev != nil {
		attrs = append(attrs, nl.NewRtAttr(nl.VDPA_ATTR_DEV_NAME, nl.ZeroTerminated(*dev)))
	} else {
		extraFlags = extraFlags | unix.NLM_F_DUMP
	}
	messages, executeErr := h.vdpaRequest(nl.VDPA_CMD_DEV_CONFIG_GET, extraFlags, attrs)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	cfgs := make([]*VDPADevConfig, 0, len(messages))
	for _, m := range messages {
		cfg := &VDPADevConfig{}
		cfg.parseAttributes(m)
		cfgs = append(cfgs, cfg)
	}
	return cfgs, executeErr
}

// dump all devices if dev is nil
//
// If dev is nil and the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) vdpaMGMTDevGet(bus, dev *string) ([]*VDPAMGMTDev, error) {
	var extraFlags int
	var attrs []*nl.RtAttr
	if dev != nil {
		attrs = append(attrs,
			nl.NewRtAttr(nl.VDPA_ATTR_MGMTDEV_DEV_NAME, nl.ZeroTerminated(*dev)),
		)
		if bus != nil {
			attrs = append(attrs,
				nl.NewRtAttr(nl.VDPA_ATTR_MGMTDEV_BUS_NAME, nl.ZeroTerminated(*bus)),
			)
		}
	} else {
		extraFlags = extraFlags | unix.NLM_F_DUMP
	}
	messages, executeErr := h.vdpaRequest(nl.VDPA_CMD_MGMTDEV_GET, extraFlags, attrs)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	cfgs := make([]*VDPAMGMTDev, 0, len(messages))
	for _, m := range messages {
		cfg := &VDPAMGMTDev{}
		cfg.parseAttributes(m)
		cfgs = append(cfgs, cfg)
	}
	return cfgs, executeErr
}

// VDPANewDev adds new VDPA device
// Equivalent to: `vdpa dev add name <name> mgmtdev <mgmtBus>/mgmtName [params]`
func (h *Handle) VDPANewDev(name, mgmtBus, mgmtName string, params VDPANewDevParams) error {
	attrs := []*nl.RtAttr{
		nl.NewRtAttr(nl.VDPA_ATTR_DEV_NAME, nl.ZeroTerminated(name)),
		nl.NewRtAttr(nl.VDPA_ATTR_MGMTDEV_DEV_NAME, nl.ZeroTerminated(mgmtName)),
	}
	if mgmtBus != "" {
		attrs = append(attrs, nl.NewRtAttr(nl.VDPA_ATTR_MGMTDEV_BUS_NAME, nl.ZeroTerminated(mgmtBus)))
	}
	if len(params.MACAddr) != 0 {
		attrs = append(attrs, nl.NewRtAttr(nl.VDPA_ATTR_DEV_NET_CFG_MACADDR, params.MACAddr))
	}
	if params.MaxVQP > 0 {
		attrs = append(attrs, nl.NewRtAttr(nl.VDPA_ATTR_DEV_NET_CFG_MAX_VQP, nl.Uint16Attr(params.MaxVQP)))
	}
	if params.MTU > 0 {
		attrs = append(attrs, nl.NewRtAttr(nl.VDPA_ATTR_DEV_NET_CFG_MTU, nl.Uint16Attr(params.MTU)))
	}
	if params.Features > 0 {
		attrs = append(attrs, nl.NewRtAttr(nl.VDPA_ATTR_DEV_FEATURES, nl.Uint64Attr(params.Features)))
	}
	_, err := h.vdpaRequest(nl.VDPA_CMD_DEV_NEW, 0, attrs)
	return err
}

// VDPADelDev removes VDPA device
// Equivalent to: `vdpa dev del <name>`
func (h *Handle) VDPADelDev(name string) error {
	_, err := h.vdpaRequest(nl.VDPA_CMD_DEV_DEL, 0, []*nl.RtAttr{
		nl.NewRtAttr(nl.VDPA_ATTR_DEV_NAME, nl.ZeroTerminated(name))})
	return err
}

// VDPAGetDevList returns list of VDPA devices
// Equivalent to: `vdpa dev show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) VDPAGetDevList() ([]*VDPADev, error) {
	return h.vdpaDevGet(nil)
}

// VDPAGetDevByName returns VDPA device selected by name
// Equivalent to: `vdpa dev show <name>`
func (h *Handle) VDPAGetDevByName(name string) (*VDPADev, error) {
	devs, err := h.vdpaDevGet(&name)
	if err != nil {
		return nil, err
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("device not found")
	}
	return devs[0], nil
}

// VDPAGetDevConfigList returns list of VDPA devices configurations
// Equivalent to: `vdpa dev config show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) VDPAGetDevConfigList() ([]*VDPADevConfig, error) {
	return h.vdpaDevConfigGet(nil)
}

// VDPAGetDevConfigByName returns VDPA device configuration selected by name
// Equivalent to: `vdpa dev config show <name>`
func (h *Handle) VDPAGetDevConfigByName(name string) (*VDPADevConfig, error) {
	cfgs, err := h.vdpaDevConfigGet(&name)
	if err != nil {
		return nil, err
	}
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("configuration not found")
	}
	return cfgs[0], nil
}

// VDPAGetDevVStats returns vstats for VDPA device
// Equivalent to: `vdpa dev vstats show <name> qidx <queueIndex>`
func (h *Handle) VDPAGetDevVStats(name string, queueIndex uint32) (*VDPADevVStats, error) {
	messages, err := h.vdpaRequest(nl.VDPA_CMD_DEV_VSTATS_GET, 0, []*nl.RtAttr{
		nl.NewRtAttr(nl.VDPA_ATTR_DEV_NAME, nl.ZeroTerminated(name)),
		nl.NewRtAttr(nl.VDPA_ATTR_DEV_QUEUE_INDEX, nl.Uint32Attr(queueIndex)),
	})
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("stats not found")
	}
	stats := &VDPADevVStats{}
	stats.parseAttributes(messages[0])
	return stats, nil
}

// VDPAGetMGMTDevList returns list of mgmt devices
// Equivalent to: `vdpa mgmtdev show`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) VDPAGetMGMTDevList() ([]*VDPAMGMTDev, error) {
	return h.vdpaMGMTDevGet(nil, nil)
}

// VDPAGetMGMTDevByBusAndName returns mgmt devices selected by bus and name
// Equivalent to: `vdpa mgmtdev show <bus>/<name>`
func (h *Handle) VDPAGetMGMTDevByBusAndName(bus, name string) (*VDPAMGMTDev, error) {
	var busPtr *string
	if bus != "" {
		busPtr = &bus
	}
	devs, err := h.vdpaMGMTDevGet(busPtr, &name)
	if err != nil {
		return nil, err
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("mgmtdev not found")
	}
	return devs[0], nil
}
