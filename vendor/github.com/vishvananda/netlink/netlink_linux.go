package netlink

import "github.com/vishvananda/netlink/nl"

// Family type definitions
const (
	FAMILY_ALL  = nl.FAMILY_ALL
	FAMILY_V4   = nl.FAMILY_V4
	FAMILY_V6   = nl.FAMILY_V6
	FAMILY_MPLS = nl.FAMILY_MPLS
)

// ErrDumpInterrupted is an alias for [nl.ErrDumpInterrupted].
var ErrDumpInterrupted = nl.ErrDumpInterrupted
