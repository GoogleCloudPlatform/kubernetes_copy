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

// DevlinkDevEswitchAttr represents device's eswitch attributes
type DevlinkDevEswitchAttr struct {
	Mode       string
	InlineMode string
	EncapMode  string
}

// DevlinkDevAttrs represents device attributes
type DevlinkDevAttrs struct {
	Eswitch DevlinkDevEswitchAttr
}

// DevlinkDevice represents device and its attributes
type DevlinkDevice struct {
	BusName    string
	DeviceName string
	Attrs      DevlinkDevAttrs
}

// DevlinkPortFn represents port function and its attributes
type DevlinkPortFn struct {
	HwAddr  net.HardwareAddr
	State   uint8
	OpState uint8
}

// DevlinkPortFnSetAttrs represents attributes to set
type DevlinkPortFnSetAttrs struct {
	FnAttrs     DevlinkPortFn
	HwAddrValid bool
	StateValid  bool
}

// DevlinkPort represents port and its attributes
type DevlinkPort struct {
	BusName        string
	DeviceName     string
	PortIndex      uint32
	PortType       uint16
	NetdeviceName  string
	NetdevIfIndex  uint32
	RdmaDeviceName string
	PortFlavour    uint16
	Fn             *DevlinkPortFn
}

type DevLinkPortAddAttrs struct {
	Controller      uint32
	SfNumber        uint32
	PortIndex       uint32
	PfNumber        uint16
	SfNumberValid   bool
	PortIndexValid  bool
	ControllerValid bool
}

// DevlinkDeviceInfo represents devlink info
type DevlinkDeviceInfo struct {
	Driver         string
	SerialNumber   string
	BoardID        string
	FwApp          string
	FwAppBoundleID string
	FwAppName      string
	FwBoundleID    string
	FwMgmt         string
	FwMgmtAPI      string
	FwMgmtBuild    string
	FwNetlist      string
	FwNetlistBuild string
	FwPsidAPI      string
	FwUndi         string
}

// DevlinkResource represents a device resource
type DevlinkResource struct {
	Name            string
	ID              uint64
	Size            uint64
	SizeNew         uint64
	SizeMin         uint64
	SizeMax         uint64
	SizeGranularity uint64
	PendingChange   bool
	Unit            uint8
	SizeValid       bool
	OCCValid        bool
	OCCSize         uint64
	Parent          *DevlinkResource
	Children        []DevlinkResource
}

// parseAttributes parses provided Netlink Attributes and populates DevlinkResource, returns error if occured
func (dlr *DevlinkResource) parseAttributes(attrs map[uint16]syscall.NetlinkRouteAttr) error {
	var attr syscall.NetlinkRouteAttr
	var ok bool

	// mandatory attributes
	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_ID]
	if !ok {
		return fmt.Errorf("missing resource id")
	}
	dlr.ID = native.Uint64(attr.Value)

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_NAME]
	if !ok {
		return fmt.Errorf("missing resource name")
	}
	dlr.Name = nl.BytesToString(attr.Value)

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_SIZE]
	if !ok {
		return fmt.Errorf("missing resource size")
	}
	dlr.Size = native.Uint64(attr.Value)

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_SIZE_GRAN]
	if !ok {
		return fmt.Errorf("missing resource size granularity")
	}
	dlr.SizeGranularity = native.Uint64(attr.Value)

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_UNIT]
	if !ok {
		return fmt.Errorf("missing resource unit")
	}
	dlr.Unit = uint8(attr.Value[0])

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_SIZE_MIN]
	if !ok {
		return fmt.Errorf("missing resource size min")
	}
	dlr.SizeMin = native.Uint64(attr.Value)

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_SIZE_MAX]
	if !ok {
		return fmt.Errorf("missing resource size max")
	}
	dlr.SizeMax = native.Uint64(attr.Value)

	// optional attributes
	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_OCC]
	if ok {
		dlr.OCCSize = native.Uint64(attr.Value)
		dlr.OCCValid = true
	}

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_SIZE_VALID]
	if ok {
		dlr.SizeValid = uint8(attr.Value[0]) != 0
	}

	dlr.SizeNew = dlr.Size
	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_SIZE_NEW]
	if ok {
		dlr.SizeNew = native.Uint64(attr.Value)
	}

	dlr.PendingChange = dlr.Size != dlr.SizeNew

	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_LIST]
	if ok {
		// handle nested resoruces recursively
		subResources, err := nl.ParseRouteAttr(attr.Value)
		if err != nil {
			return err
		}

		for _, subresource := range subResources {
			resource := DevlinkResource{Parent: dlr}
			attrs, err := nl.ParseRouteAttrAsMap(subresource.Value)
			if err != nil {
				return err
			}
			err = resource.parseAttributes(attrs)
			if err != nil {
				return fmt.Errorf("failed to parse child resource, parent:%s. %w", dlr.Name, err)
			}
			dlr.Children = append(dlr.Children, resource)
		}
	}
	return nil
}

// DevlinkResources represents all devlink resources of a devlink device
type DevlinkResources struct {
	Bus       string
	Device    string
	Resources []DevlinkResource
}

// parseAttributes parses provided Netlink Attributes and populates DevlinkResources, returns error if occured
func (dlrs *DevlinkResources) parseAttributes(attrs map[uint16]syscall.NetlinkRouteAttr) error {
	var attr syscall.NetlinkRouteAttr
	var ok bool

	// Bus
	attr, ok = attrs[nl.DEVLINK_ATTR_BUS_NAME]
	if !ok {
		return fmt.Errorf("missing bus name")
	}
	dlrs.Bus = nl.BytesToString(attr.Value)

	// Device
	attr, ok = attrs[nl.DEVLINK_ATTR_DEV_NAME]
	if !ok {
		return fmt.Errorf("missing device name")
	}
	dlrs.Device = nl.BytesToString(attr.Value)

	// Resource List
	attr, ok = attrs[nl.DEVLINK_ATTR_RESOURCE_LIST]
	if !ok {
		return fmt.Errorf("missing resource list")
	}

	resourceAttrs, err := nl.ParseRouteAttr(attr.Value)
	if err != nil {
		return err
	}

	for _, resourceAttr := range resourceAttrs {
		resource := DevlinkResource{}
		attrs, err := nl.ParseRouteAttrAsMap(resourceAttr.Value)
		if err != nil {
			return err
		}
		err = resource.parseAttributes(attrs)
		if err != nil {
			return fmt.Errorf("failed to parse root resoruces, %w", err)
		}
		dlrs.Resources = append(dlrs.Resources, resource)
	}

	return nil
}

// DevlinkParam represents parameter of the device
type DevlinkParam struct {
	Name      string
	IsGeneric bool
	Type      uint8 // possible values are in nl.DEVLINK_PARAM_TYPE_* constants
	Values    []DevlinkParamValue
}

// DevlinkParamValue contains values of the parameter
// Data field contains specific type which can be casted by unsing info from the DevlinkParam.Type field
type DevlinkParamValue struct {
	rawData []byte
	Data    interface{}
	CMODE   uint8 // possible values are in nl.DEVLINK_PARAM_CMODE_* constants
}

// parseAttributes parses provided Netlink Attributes and populates DevlinkParam, returns error if occured
func (dlp *DevlinkParam) parseAttributes(attrs []syscall.NetlinkRouteAttr) error {
	var valuesList [][]syscall.NetlinkRouteAttr
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case nl.DEVLINK_ATTR_PARAM:
			nattrs, err := nl.ParseRouteAttr(attr.Value)
			if err != nil {
				return err
			}
			for _, nattr := range nattrs {
				switch nattr.Attr.Type {
				case nl.DEVLINK_ATTR_PARAM_NAME:
					dlp.Name = nl.BytesToString(nattr.Value)
				case nl.DEVLINK_ATTR_PARAM_GENERIC:
					dlp.IsGeneric = true
				case nl.DEVLINK_ATTR_PARAM_TYPE:
					if len(nattr.Value) == 1 {
						dlp.Type = nattr.Value[0]
					}
				case nl.DEVLINK_ATTR_PARAM_VALUES_LIST:
					nnattrs, err := nl.ParseRouteAttr(nattr.Value)
					if err != nil {
						return err
					}
					valuesList = append(valuesList, nnattrs)
				}
			}
		}
	}
	for _, valAttr := range valuesList {
		v := DevlinkParamValue{}
		if err := v.parseAttributes(valAttr, dlp.Type); err != nil {
			return err
		}
		dlp.Values = append(dlp.Values, v)
	}
	return nil
}

func (dlpv *DevlinkParamValue) parseAttributes(attrs []syscall.NetlinkRouteAttr, paramType uint8) error {
	for _, attr := range attrs {
		nattrs, err := nl.ParseRouteAttr(attr.Value)
		if err != nil {
			return err
		}
		var rawData []byte
		for _, nattr := range nattrs {
			switch nattr.Attr.Type {
			case nl.DEVLINK_ATTR_PARAM_VALUE_DATA:
				rawData = nattr.Value
			case nl.DEVLINK_ATTR_PARAM_VALUE_CMODE:
				if len(nattr.Value) == 1 {
					dlpv.CMODE = nattr.Value[0]
				}
			}
		}
		switch paramType {
		case nl.DEVLINK_PARAM_TYPE_U8:
			dlpv.Data = uint8(0)
			if rawData != nil && len(rawData) == 1 {
				dlpv.Data = uint8(rawData[0])
			}
		case nl.DEVLINK_PARAM_TYPE_U16:
			dlpv.Data = uint16(0)
			if rawData != nil {
				dlpv.Data = native.Uint16(rawData)
			}
		case nl.DEVLINK_PARAM_TYPE_U32:
			dlpv.Data = uint32(0)
			if rawData != nil {
				dlpv.Data = native.Uint32(rawData)
			}
		case nl.DEVLINK_PARAM_TYPE_STRING:
			dlpv.Data = ""
			if rawData != nil {
				dlpv.Data = nl.BytesToString(rawData)
			}
		case nl.DEVLINK_PARAM_TYPE_BOOL:
			dlpv.Data = rawData != nil
		}
	}
	return nil
}

func parseDevLinkDeviceList(msgs [][]byte) ([]*DevlinkDevice, error) {
	devices := make([]*DevlinkDevice, 0, len(msgs))
	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		dev := &DevlinkDevice{}
		if err = dev.parseAttributes(attrs); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

func eswitchStringToMode(modeName string) (uint16, error) {
	if modeName == "legacy" {
		return nl.DEVLINK_ESWITCH_MODE_LEGACY, nil
	} else if modeName == "switchdev" {
		return nl.DEVLINK_ESWITCH_MODE_SWITCHDEV, nil
	} else {
		return 0xffff, fmt.Errorf("invalid switchdev mode")
	}
}

func parseEswitchMode(mode uint16) string {
	var eswitchMode = map[uint16]string{
		nl.DEVLINK_ESWITCH_MODE_LEGACY:    "legacy",
		nl.DEVLINK_ESWITCH_MODE_SWITCHDEV: "switchdev",
	}
	if eswitchMode[mode] == "" {
		return "unknown"
	} else {
		return eswitchMode[mode]
	}
}

func parseEswitchInlineMode(inlinemode uint8) string {
	var eswitchInlineMode = map[uint8]string{
		nl.DEVLINK_ESWITCH_INLINE_MODE_NONE:      "none",
		nl.DEVLINK_ESWITCH_INLINE_MODE_LINK:      "link",
		nl.DEVLINK_ESWITCH_INLINE_MODE_NETWORK:   "network",
		nl.DEVLINK_ESWITCH_INLINE_MODE_TRANSPORT: "transport",
	}
	if eswitchInlineMode[inlinemode] == "" {
		return "unknown"
	} else {
		return eswitchInlineMode[inlinemode]
	}
}

func parseEswitchEncapMode(encapmode uint8) string {
	var eswitchEncapMode = map[uint8]string{
		nl.DEVLINK_ESWITCH_ENCAP_MODE_NONE:  "disable",
		nl.DEVLINK_ESWITCH_ENCAP_MODE_BASIC: "enable",
	}
	if eswitchEncapMode[encapmode] == "" {
		return "unknown"
	} else {
		return eswitchEncapMode[encapmode]
	}
}

func (d *DevlinkDevice) parseAttributes(attrs []syscall.NetlinkRouteAttr) error {
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.DEVLINK_ATTR_BUS_NAME:
			d.BusName = string(a.Value[:len(a.Value)-1])
		case nl.DEVLINK_ATTR_DEV_NAME:
			d.DeviceName = string(a.Value[:len(a.Value)-1])
		case nl.DEVLINK_ATTR_ESWITCH_MODE:
			d.Attrs.Eswitch.Mode = parseEswitchMode(native.Uint16(a.Value))
		case nl.DEVLINK_ATTR_ESWITCH_INLINE_MODE:
			d.Attrs.Eswitch.InlineMode = parseEswitchInlineMode(uint8(a.Value[0]))
		case nl.DEVLINK_ATTR_ESWITCH_ENCAP_MODE:
			d.Attrs.Eswitch.EncapMode = parseEswitchEncapMode(uint8(a.Value[0]))
		}
	}
	return nil
}

func (dev *DevlinkDevice) parseEswitchAttrs(msgs [][]byte) {
	m := msgs[0]
	attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
	if err != nil {
		return
	}
	dev.parseAttributes(attrs)
}

func (h *Handle) getEswitchAttrs(family *GenlFamily, dev *DevlinkDevice) {
	msg := &nl.Genlmsg{
		Command: nl.DEVLINK_CMD_ESWITCH_GET,
		Version: nl.GENL_DEVLINK_VERSION,
	}
	req := h.newNetlinkRequest(int(family.ID), unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	req.AddData(msg)

	b := make([]byte, len(dev.BusName)+1)
	copy(b, dev.BusName)
	data := nl.NewRtAttr(nl.DEVLINK_ATTR_BUS_NAME, b)
	req.AddData(data)

	b = make([]byte, len(dev.DeviceName)+1)
	copy(b, dev.DeviceName)
	data = nl.NewRtAttr(nl.DEVLINK_ATTR_DEV_NAME, b)
	req.AddData(data)

	msgs, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return
	}
	dev.parseEswitchAttrs(msgs)
}

// DevLinkGetDeviceList provides a pointer to devlink devices and nil error,
// otherwise returns an error code.
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) DevLinkGetDeviceList() ([]*DevlinkDevice, error) {
	f, err := h.GenlFamilyGet(nl.GENL_DEVLINK_NAME)
	if err != nil {
		return nil, err
	}
	msg := &nl.Genlmsg{
		Command: nl.DEVLINK_CMD_GET,
		Version: nl.GENL_DEVLINK_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID),
		unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_DUMP)
	req.AddData(msg)
	msgs, executeErr := req.Execute(unix.NETLINK_GENERIC, 0)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	devices, err := parseDevLinkDeviceList(msgs)
	if err != nil {
		return nil, err
	}
	for _, d := range devices {
		h.getEswitchAttrs(f, d)
	}
	return devices, executeErr
}

// DevLinkGetDeviceList provides a pointer to devlink devices and nil error,
// otherwise returns an error code.
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func DevLinkGetDeviceList() ([]*DevlinkDevice, error) {
	return pkgHandle.DevLinkGetDeviceList()
}

func parseDevlinkDevice(msgs [][]byte) (*DevlinkDevice, error) {
	m := msgs[0]
	attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
	if err != nil {
		return nil, err
	}
	dev := &DevlinkDevice{}
	if err = dev.parseAttributes(attrs); err != nil {
		return nil, err
	}
	return dev, nil
}

func (h *Handle) createCmdReq(cmd uint8, bus string, device string) (*GenlFamily, *nl.NetlinkRequest, error) {
	f, err := h.GenlFamilyGet(nl.GENL_DEVLINK_NAME)
	if err != nil {
		return nil, nil, err
	}

	msg := &nl.Genlmsg{
		Command: cmd,
		Version: nl.GENL_DEVLINK_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID),
		unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	req.AddData(msg)

	b := make([]byte, len(bus)+1)
	copy(b, bus)
	data := nl.NewRtAttr(nl.DEVLINK_ATTR_BUS_NAME, b)
	req.AddData(data)

	b = make([]byte, len(device)+1)
	copy(b, device)
	data = nl.NewRtAttr(nl.DEVLINK_ATTR_DEV_NAME, b)
	req.AddData(data)

	return f, req, nil
}

// DevlinkGetDeviceByName provides a pointer to devlink device and nil error,
// otherwise returns an error code.
func (h *Handle) DevLinkGetDeviceByName(Bus string, Device string) (*DevlinkDevice, error) {
	f, req, err := h.createCmdReq(nl.DEVLINK_CMD_GET, Bus, Device)
	if err != nil {
		return nil, err
	}

	respmsg, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}
	dev, err := parseDevlinkDevice(respmsg)
	if err == nil {
		h.getEswitchAttrs(f, dev)
	}
	return dev, err
}

// DevlinkGetDeviceByName provides a pointer to devlink device and nil error,
// otherwise returns an error code.
func DevLinkGetDeviceByName(Bus string, Device string) (*DevlinkDevice, error) {
	return pkgHandle.DevLinkGetDeviceByName(Bus, Device)
}

// DevLinkSetEswitchMode sets eswitch mode if able to set successfully or
// returns an error code.
// Equivalent to: `devlink dev eswitch set $dev mode switchdev`
// Equivalent to: `devlink dev eswitch set $dev mode legacy`
func (h *Handle) DevLinkSetEswitchMode(Dev *DevlinkDevice, NewMode string) error {
	mode, err := eswitchStringToMode(NewMode)
	if err != nil {
		return err
	}

	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_ESWITCH_SET, Dev.BusName, Dev.DeviceName)
	if err != nil {
		return err
	}

	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_ESWITCH_MODE, nl.Uint16Attr(mode)))

	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	return err
}

// DevLinkSetEswitchMode sets eswitch mode if able to set successfully or
// returns an error code.
// Equivalent to: `devlink dev eswitch set $dev mode switchdev`
// Equivalent to: `devlink dev eswitch set $dev mode legacy`
func DevLinkSetEswitchMode(Dev *DevlinkDevice, NewMode string) error {
	return pkgHandle.DevLinkSetEswitchMode(Dev, NewMode)
}

func (port *DevlinkPort) parseAttributes(attrs []syscall.NetlinkRouteAttr) error {
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.DEVLINK_ATTR_BUS_NAME:
			port.BusName = string(a.Value[:len(a.Value)-1])
		case nl.DEVLINK_ATTR_DEV_NAME:
			port.DeviceName = string(a.Value[:len(a.Value)-1])
		case nl.DEVLINK_ATTR_PORT_INDEX:
			port.PortIndex = native.Uint32(a.Value)
		case nl.DEVLINK_ATTR_PORT_TYPE:
			port.PortType = native.Uint16(a.Value)
		case nl.DEVLINK_ATTR_PORT_NETDEV_NAME:
			port.NetdeviceName = string(a.Value[:len(a.Value)-1])
		case nl.DEVLINK_ATTR_PORT_NETDEV_IFINDEX:
			port.NetdevIfIndex = native.Uint32(a.Value)
		case nl.DEVLINK_ATTR_PORT_IBDEV_NAME:
			port.RdmaDeviceName = string(a.Value[:len(a.Value)-1])
		case nl.DEVLINK_ATTR_PORT_FLAVOUR:
			port.PortFlavour = native.Uint16(a.Value)
		case nl.DEVLINK_ATTR_PORT_FUNCTION:
			port.Fn = &DevlinkPortFn{}
			for nested := range nl.ParseAttributes(a.Value) {
				switch nested.Type {
				case nl.DEVLINK_PORT_FUNCTION_ATTR_HW_ADDR:
					port.Fn.HwAddr = nested.Value[:]
				case nl.DEVLINK_PORT_FN_ATTR_STATE:
					port.Fn.State = uint8(nested.Value[0])
				case nl.DEVLINK_PORT_FN_ATTR_OPSTATE:
					port.Fn.OpState = uint8(nested.Value[0])
				}
			}
		}
	}
	return nil
}

func parseDevLinkAllPortList(msgs [][]byte) ([]*DevlinkPort, error) {
	ports := make([]*DevlinkPort, 0, len(msgs))
	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		port := &DevlinkPort{}
		if err = port.parseAttributes(attrs); err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, nil
}

// DevLinkGetPortList provides a pointer to devlink ports and nil error,
// otherwise returns an error code.
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) DevLinkGetAllPortList() ([]*DevlinkPort, error) {
	f, err := h.GenlFamilyGet(nl.GENL_DEVLINK_NAME)
	if err != nil {
		return nil, err
	}
	msg := &nl.Genlmsg{
		Command: nl.DEVLINK_CMD_PORT_GET,
		Version: nl.GENL_DEVLINK_VERSION,
	}
	req := h.newNetlinkRequest(int(f.ID),
		unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_DUMP)
	req.AddData(msg)
	msgs, executeErr := req.Execute(unix.NETLINK_GENERIC, 0)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	ports, err := parseDevLinkAllPortList(msgs)
	if err != nil {
		return nil, err
	}
	return ports, executeErr
}

// DevLinkGetPortList provides a pointer to devlink ports and nil error,
// otherwise returns an error code.
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func DevLinkGetAllPortList() ([]*DevlinkPort, error) {
	return pkgHandle.DevLinkGetAllPortList()
}

func parseDevlinkPortMsg(msgs [][]byte) (*DevlinkPort, error) {
	m := msgs[0]
	attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
	if err != nil {
		return nil, err
	}
	port := &DevlinkPort{}
	if err = port.parseAttributes(attrs); err != nil {
		return nil, err
	}
	return port, nil
}

// DevLinkGetPortByIndexprovides a pointer to devlink device and nil error,
// otherwise returns an error code.
func (h *Handle) DevLinkGetPortByIndex(Bus string, Device string, PortIndex uint32) (*DevlinkPort, error) {

	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_PORT_GET, Bus, Device)
	if err != nil {
		return nil, err
	}

	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_INDEX, nl.Uint32Attr(PortIndex)))

	respmsg, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}
	port, err := parseDevlinkPortMsg(respmsg)
	return port, err
}

// DevlinkGetDeviceResources returns devlink device resources
func DevlinkGetDeviceResources(bus string, device string) (*DevlinkResources, error) {
	return pkgHandle.DevlinkGetDeviceResources(bus, device)
}

// DevlinkGetDeviceResources returns devlink device resources
func (h *Handle) DevlinkGetDeviceResources(bus string, device string) (*DevlinkResources, error) {
	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_RESOURCE_DUMP, bus, device)
	if err != nil {
		return nil, err
	}

	respmsg, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}

	var resources DevlinkResources
	for _, m := range respmsg {
		attrs, err := nl.ParseRouteAttrAsMap(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		resources.parseAttributes(attrs)
	}

	return &resources, nil
}

// DevlinkGetDeviceParams returns parameters for devlink device
// Equivalent to: `devlink dev param show <bus>/<device>`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func (h *Handle) DevlinkGetDeviceParams(bus string, device string) ([]*DevlinkParam, error) {
	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_PARAM_GET, bus, device)
	if err != nil {
		return nil, err
	}
	req.Flags |= unix.NLM_F_DUMP
	respmsg, executeErr := req.Execute(unix.NETLINK_GENERIC, 0)
	if executeErr != nil && !errors.Is(executeErr, ErrDumpInterrupted) {
		return nil, executeErr
	}
	var params []*DevlinkParam
	for _, m := range respmsg {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		p := &DevlinkParam{}
		if err := p.parseAttributes(attrs); err != nil {
			return nil, err
		}
		params = append(params, p)
	}

	return params, executeErr
}

// DevlinkGetDeviceParams returns parameters for devlink device
// Equivalent to: `devlink dev param show <bus>/<device>`
//
// If the returned error is [ErrDumpInterrupted], results may be inconsistent
// or incomplete.
func DevlinkGetDeviceParams(bus string, device string) ([]*DevlinkParam, error) {
	return pkgHandle.DevlinkGetDeviceParams(bus, device)
}

// DevlinkGetDeviceParamByName returns specific parameter for devlink device
// Equivalent to: `devlink dev param show <bus>/<device> name <param>`
func (h *Handle) DevlinkGetDeviceParamByName(bus string, device string, param string) (*DevlinkParam, error) {
	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_PARAM_GET, bus, device)
	if err != nil {
		return nil, err
	}
	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PARAM_NAME, nl.ZeroTerminated(param)))
	respmsg, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}
	if len(respmsg) == 0 {
		return nil, fmt.Errorf("unexpected response")
	}
	attrs, err := nl.ParseRouteAttr(respmsg[0][nl.SizeofGenlmsg:])
	if err != nil {
		return nil, err
	}
	p := &DevlinkParam{}
	if err := p.parseAttributes(attrs); err != nil {
		return nil, err
	}
	return p, nil
}

// DevlinkGetDeviceParamByName returns specific parameter for devlink device
// Equivalent to: `devlink dev param show <bus>/<device> name <param>`
func DevlinkGetDeviceParamByName(bus string, device string, param string) (*DevlinkParam, error) {
	return pkgHandle.DevlinkGetDeviceParamByName(bus, device, param)
}

// DevlinkSetDeviceParam set specific parameter for devlink device
// Equivalent to: `devlink dev param set <bus>/<device> name <param> cmode <cmode> value <value>`
// cmode argument should contain valid cmode value as uint8, modes are define in nl.DEVLINK_PARAM_CMODE_* constants
// value argument should have one of the following types: uint8, uint16, uint32, string, bool
func (h *Handle) DevlinkSetDeviceParam(bus string, device string, param string, cmode uint8, value interface{}) error {
	// retrive the param type
	p, err := h.DevlinkGetDeviceParamByName(bus, device, param)
	if err != nil {
		return fmt.Errorf("failed to get device param: %v", err)
	}
	paramType := p.Type

	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_PARAM_SET, bus, device)
	if err != nil {
		return err
	}
	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PARAM_TYPE, nl.Uint8Attr(paramType)))
	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PARAM_NAME, nl.ZeroTerminated(param)))
	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PARAM_VALUE_CMODE, nl.Uint8Attr(cmode)))

	var valueAsBytes []byte
	switch paramType {
	case nl.DEVLINK_PARAM_TYPE_U8:
		v, ok := value.(uint8)
		if !ok {
			return fmt.Errorf("unepected value type required: uint8, actual: %T", value)
		}
		valueAsBytes = nl.Uint8Attr(v)
	case nl.DEVLINK_PARAM_TYPE_U16:
		v, ok := value.(uint16)
		if !ok {
			return fmt.Errorf("unepected value type required: uint16, actual: %T", value)
		}
		valueAsBytes = nl.Uint16Attr(v)
	case nl.DEVLINK_PARAM_TYPE_U32:
		v, ok := value.(uint32)
		if !ok {
			return fmt.Errorf("unepected value type required: uint32, actual: %T", value)
		}
		valueAsBytes = nl.Uint32Attr(v)
	case nl.DEVLINK_PARAM_TYPE_STRING:
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("unepected value type required: string, actual: %T", value)
		}
		valueAsBytes = nl.ZeroTerminated(v)
	case nl.DEVLINK_PARAM_TYPE_BOOL:
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("unepected value type required: bool, actual: %T", value)
		}
		if v {
			valueAsBytes = []byte{}
		}
	default:
		return fmt.Errorf("unsupported parameter type: %d", paramType)
	}
	if valueAsBytes != nil {
		req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PARAM_VALUE_DATA, valueAsBytes))
	}
	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	return err
}

// DevlinkSetDeviceParam set specific parameter for devlink device
// Equivalent to: `devlink dev param set <bus>/<device> name <param> cmode <cmode> value <value>`
// cmode argument should contain valid cmode value as uint8, modes are define in nl.DEVLINK_PARAM_CMODE_* constants
// value argument should have one of the following types: uint8, uint16, uint32, string, bool
func DevlinkSetDeviceParam(bus string, device string, param string, cmode uint8, value interface{}) error {
	return pkgHandle.DevlinkSetDeviceParam(bus, device, param, cmode, value)
}

// DevLinkGetPortByIndex provides a pointer to devlink portand nil error,
// otherwise returns an error code.
func DevLinkGetPortByIndex(Bus string, Device string, PortIndex uint32) (*DevlinkPort, error) {
	return pkgHandle.DevLinkGetPortByIndex(Bus, Device, PortIndex)
}

// DevLinkPortAdd adds a devlink port and returns a port on success
// otherwise returns nil port and an error code.
func (h *Handle) DevLinkPortAdd(Bus string, Device string, Flavour uint16, Attrs DevLinkPortAddAttrs) (*DevlinkPort, error) {
	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_PORT_NEW, Bus, Device)
	if err != nil {
		return nil, err
	}

	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_FLAVOUR, nl.Uint16Attr(Flavour)))

	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_PCI_PF_NUMBER, nl.Uint16Attr(Attrs.PfNumber)))
	if Flavour == nl.DEVLINK_PORT_FLAVOUR_PCI_SF && Attrs.SfNumberValid {
		req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_PCI_SF_NUMBER, nl.Uint32Attr(Attrs.SfNumber)))
	}
	if Attrs.PortIndexValid {
		req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_INDEX, nl.Uint32Attr(Attrs.PortIndex)))
	}
	if Attrs.ControllerValid {
		req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_CONTROLLER_NUMBER, nl.Uint32Attr(Attrs.Controller)))
	}
	respmsg, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}
	port, err := parseDevlinkPortMsg(respmsg)
	return port, err
}

// DevLinkPortAdd adds a devlink port and returns a port on success
// otherwise returns nil port and an error code.
func DevLinkPortAdd(Bus string, Device string, Flavour uint16, Attrs DevLinkPortAddAttrs) (*DevlinkPort, error) {
	return pkgHandle.DevLinkPortAdd(Bus, Device, Flavour, Attrs)
}

// DevLinkPortDel deletes a devlink port and returns success or error code.
func (h *Handle) DevLinkPortDel(Bus string, Device string, PortIndex uint32) error {
	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_PORT_DEL, Bus, Device)
	if err != nil {
		return err
	}

	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_INDEX, nl.Uint32Attr(PortIndex)))
	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	return err
}

// DevLinkPortDel deletes a devlink port and returns success or error code.
func DevLinkPortDel(Bus string, Device string, PortIndex uint32) error {
	return pkgHandle.DevLinkPortDel(Bus, Device, PortIndex)
}

// DevlinkPortFnSet sets one or more port function attributes specified by the attribute mask.
// It returns 0 on success or error code.
func (h *Handle) DevlinkPortFnSet(Bus string, Device string, PortIndex uint32, FnAttrs DevlinkPortFnSetAttrs) error {
	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_PORT_SET, Bus, Device)
	if err != nil {
		return err
	}

	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_INDEX, nl.Uint32Attr(PortIndex)))

	fnAttr := nl.NewRtAttr(nl.DEVLINK_ATTR_PORT_FUNCTION|unix.NLA_F_NESTED, nil)

	if FnAttrs.HwAddrValid {
		fnAttr.AddRtAttr(nl.DEVLINK_PORT_FUNCTION_ATTR_HW_ADDR, []byte(FnAttrs.FnAttrs.HwAddr))
	}

	if FnAttrs.StateValid {
		fnAttr.AddRtAttr(nl.DEVLINK_PORT_FN_ATTR_STATE, nl.Uint8Attr(FnAttrs.FnAttrs.State))
	}
	req.AddData(fnAttr)

	_, err = req.Execute(unix.NETLINK_GENERIC, 0)
	return err
}

// DevlinkPortFnSet sets one or more port function attributes specified by the attribute mask.
// It returns 0 on success or error code.
func DevlinkPortFnSet(Bus string, Device string, PortIndex uint32, FnAttrs DevlinkPortFnSetAttrs) error {
	return pkgHandle.DevlinkPortFnSet(Bus, Device, PortIndex, FnAttrs)
}

// devlinkInfoGetter is function that is responsible for getting devlink info message
// this is introduced for test purpose
type devlinkInfoGetter func(bus, device string) ([]byte, error)

// DevlinkGetDeviceInfoByName returns devlink info for selected device,
// otherwise returns an error code.
// Equivalent to: `devlink dev info $dev`
func (h *Handle) DevlinkGetDeviceInfoByName(Bus string, Device string, getInfoMsg devlinkInfoGetter) (*DevlinkDeviceInfo, error) {
	info, err := h.DevlinkGetDeviceInfoByNameAsMap(Bus, Device, getInfoMsg)
	if err != nil {
		return nil, err
	}

	return parseInfoData(info), nil
}

// DevlinkGetDeviceInfoByName returns devlink info for selected device,
// otherwise returns an error code.
// Equivalent to: `devlink dev info $dev`
func DevlinkGetDeviceInfoByName(Bus string, Device string) (*DevlinkDeviceInfo, error) {
	return pkgHandle.DevlinkGetDeviceInfoByName(Bus, Device, pkgHandle.getDevlinkInfoMsg)
}

// DevlinkGetDeviceInfoByNameAsMap returns devlink info for selected device as a map,
// otherwise returns an error code.
// Equivalent to: `devlink dev info $dev`
func (h *Handle) DevlinkGetDeviceInfoByNameAsMap(Bus string, Device string, getInfoMsg devlinkInfoGetter) (map[string]string, error) {
	response, err := getInfoMsg(Bus, Device)
	if err != nil {
		return nil, err
	}

	info, err := parseInfoMsg(response)
	if err != nil {
		return nil, err
	}

	return info, nil
}

// DevlinkGetDeviceInfoByNameAsMap returns devlink info for selected device as a map,
// otherwise returns an error code.
// Equivalent to: `devlink dev info $dev`
func DevlinkGetDeviceInfoByNameAsMap(Bus string, Device string) (map[string]string, error) {
	return pkgHandle.DevlinkGetDeviceInfoByNameAsMap(Bus, Device, pkgHandle.getDevlinkInfoMsg)
}

// GetDevlinkInfo returns devlink info for target device,
// otherwise returns an error code.
func (d *DevlinkDevice) GetDevlinkInfo() (*DevlinkDeviceInfo, error) {
	return pkgHandle.DevlinkGetDeviceInfoByName(d.BusName, d.DeviceName, pkgHandle.getDevlinkInfoMsg)
}

// GetDevlinkInfoAsMap returns devlink info for target device as a map,
// otherwise returns an error code.
func (d *DevlinkDevice) GetDevlinkInfoAsMap() (map[string]string, error) {
	return pkgHandle.DevlinkGetDeviceInfoByNameAsMap(d.BusName, d.DeviceName, pkgHandle.getDevlinkInfoMsg)
}

func (h *Handle) getDevlinkInfoMsg(bus, device string) ([]byte, error) {
	_, req, err := h.createCmdReq(nl.DEVLINK_CMD_INFO_GET, bus, device)
	if err != nil {
		return nil, err
	}

	response, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, err
	}

	if len(response) < 1 {
		return nil, fmt.Errorf("getDevlinkInfoMsg: message too short")
	}

	return response[0], nil
}

func parseInfoMsg(msg []byte) (map[string]string, error) {
	if len(msg) < nl.SizeofGenlmsg {
		return nil, fmt.Errorf("parseInfoMsg: message too short")
	}

	info := make(map[string]string)
	err := collectInfoData(msg[nl.SizeofGenlmsg:], info)

	if err != nil {
		return nil, err
	}

	return info, nil
}

func collectInfoData(msg []byte, data map[string]string) error {
	attrs, err := nl.ParseRouteAttr(msg)
	if err != nil {
		return err
	}

	for _, attr := range attrs {
		switch attr.Attr.Type {
		case nl.DEVLINK_ATTR_INFO_DRIVER_NAME:
			data["driver"] = parseInfoValue(attr.Value)
		case nl.DEVLINK_ATTR_INFO_SERIAL_NUMBER:
			data["serialNumber"] = parseInfoValue(attr.Value)
		case nl.DEVLINK_ATTR_INFO_VERSION_RUNNING, nl.DEVLINK_ATTR_INFO_VERSION_FIXED,
			nl.DEVLINK_ATTR_INFO_VERSION_STORED:
			key, value, err := getNestedInfoData(attr.Value)
			if err != nil {
				return err
			}
			data[key] = value
		}
	}

	if len(data) == 0 {
		return fmt.Errorf("collectInfoData: could not read attributes")
	}

	return nil
}

func getNestedInfoData(msg []byte) (string, string, error) {
	nestedAttrs, err := nl.ParseRouteAttr(msg)

	var key, value string

	if err != nil {
		return "", "", err
	}

	if len(nestedAttrs) != 2 {
		return "", "", fmt.Errorf("getNestedInfoData: too few attributes in nested structure")
	}

	for _, nestedAttr := range nestedAttrs {
		switch nestedAttr.Attr.Type {
		case nl.DEVLINK_ATTR_INFO_VERSION_NAME:
			key = parseInfoValue(nestedAttr.Value)
		case nl.DEVLINK_ATTR_INFO_VERSION_VALUE:
			value = parseInfoValue(nestedAttr.Value)
		}
	}

	if key == "" {
		return "", "", fmt.Errorf("getNestedInfoData: key not found")
	}

	if value == "" {
		return "", "", fmt.Errorf("getNestedInfoData: value not found")
	}

	return key, value, nil
}

func parseInfoData(data map[string]string) *DevlinkDeviceInfo {
	info := new(DevlinkDeviceInfo)
	for key, value := range data {
		switch key {
		case "driver":
			info.Driver = value
		case "serialNumber":
			info.SerialNumber = value
		case "board.id":
			info.BoardID = value
		case "fw.app":
			info.FwApp = value
		case "fw.app.bundle_id":
			info.FwAppBoundleID = value
		case "fw.app.name":
			info.FwAppName = value
		case "fw.bundle_id":
			info.FwBoundleID = value
		case "fw.mgmt":
			info.FwMgmt = value
		case "fw.mgmt.api":
			info.FwMgmtAPI = value
		case "fw.mgmt.build":
			info.FwMgmtBuild = value
		case "fw.netlist":
			info.FwNetlist = value
		case "fw.netlist.build":
			info.FwNetlistBuild = value
		case "fw.psid.api":
			info.FwPsidAPI = value
		case "fw.undi":
			info.FwUndi = value
		}
	}
	return info
}

func parseInfoValue(value []byte) string {
	v := strings.ReplaceAll(string(value), "\x00", "")
	return strings.TrimSpace(v)
}
