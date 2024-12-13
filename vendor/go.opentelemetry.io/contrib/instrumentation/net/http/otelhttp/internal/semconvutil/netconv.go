// Code created by gotmpl. DO NOT MODIFY.
// source: internal/shared/semconvutil/netconv.go.tmpl

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package semconvutil // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/semconvutil"

import (
	"net"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
)

// NetTransport returns a trace attribute describing the transport protocol of the
// passed network. See the net.Dial for information about acceptable network
// values.
func NetTransport(network string) attribute.KeyValue {
	return nc.Transport(network)
}

// netConv are the network semantic convention attributes defined for a version
// of the OpenTelemetry specification.
type netConv struct {
	NetHostNameKey     attribute.Key
	NetHostPortKey     attribute.Key
	NetPeerNameKey     attribute.Key
	NetPeerPortKey     attribute.Key
	NetProtocolName    attribute.Key
	NetProtocolVersion attribute.Key
	NetSockFamilyKey   attribute.Key
	NetSockPeerAddrKey attribute.Key
	NetSockPeerPortKey attribute.Key
	NetSockHostAddrKey attribute.Key
	NetSockHostPortKey attribute.Key
	NetTransportOther  attribute.KeyValue
	NetTransportTCP    attribute.KeyValue
	NetTransportUDP    attribute.KeyValue
	NetTransportInProc attribute.KeyValue
}

var nc = &netConv{
	NetHostNameKey:     semconv.NetHostNameKey,
	NetHostPortKey:     semconv.NetHostPortKey,
	NetPeerNameKey:     semconv.NetPeerNameKey,
	NetPeerPortKey:     semconv.NetPeerPortKey,
	NetProtocolName:    semconv.NetProtocolNameKey,
	NetProtocolVersion: semconv.NetProtocolVersionKey,
	NetSockFamilyKey:   semconv.NetSockFamilyKey,
	NetSockPeerAddrKey: semconv.NetSockPeerAddrKey,
	NetSockPeerPortKey: semconv.NetSockPeerPortKey,
	NetSockHostAddrKey: semconv.NetSockHostAddrKey,
	NetSockHostPortKey: semconv.NetSockHostPortKey,
	NetTransportOther:  semconv.NetTransportOther,
	NetTransportTCP:    semconv.NetTransportTCP,
	NetTransportUDP:    semconv.NetTransportUDP,
	NetTransportInProc: semconv.NetTransportInProc,
}

func (c *netConv) Transport(network string) attribute.KeyValue {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return c.NetTransportTCP
	case "udp", "udp4", "udp6":
		return c.NetTransportUDP
	case "unix", "unixgram", "unixpacket":
		return c.NetTransportInProc
	default:
		// "ip:*", "ip4:*", and "ip6:*" all are considered other.
		return c.NetTransportOther
	}
}

// Host returns attributes for a network host address.
func (c *netConv) Host(address string) []attribute.KeyValue {
	h, p := splitHostPort(address)
	var n int
	if h != "" {
		n++
		if p > 0 {
			n++
		}
	}

	if n == 0 {
		return nil
	}

	attrs := make([]attribute.KeyValue, 0, n)
	attrs = append(attrs, c.HostName(h))
	if p > 0 {
		attrs = append(attrs, c.HostPort(p))
	}
	return attrs
}

func (c *netConv) HostName(name string) attribute.KeyValue {
	return c.NetHostNameKey.String(name)
}

func (c *netConv) HostPort(port int) attribute.KeyValue {
	return c.NetHostPortKey.Int(port)
}

func family(network, address string) string {
	switch network {
	case "unix", "unixgram", "unixpacket":
		return "unix"
	default:
		if ip := net.ParseIP(address); ip != nil {
			if ip.To4() == nil {
				return "inet6"
			}
			return "inet"
		}
	}
	return ""
}

// Peer returns attributes for a network peer address.
func (c *netConv) Peer(address string) []attribute.KeyValue {
	h, p := splitHostPort(address)
	var n int
	if h != "" {
		n++
		if p > 0 {
			n++
		}
	}

	if n == 0 {
		return nil
	}

	attrs := make([]attribute.KeyValue, 0, n)
	attrs = append(attrs, c.PeerName(h))
	if p > 0 {
		attrs = append(attrs, c.PeerPort(p))
	}
	return attrs
}

func (c *netConv) PeerName(name string) attribute.KeyValue {
	return c.NetPeerNameKey.String(name)
}

func (c *netConv) PeerPort(port int) attribute.KeyValue {
	return c.NetPeerPortKey.Int(port)
}

func (c *netConv) SockPeerAddr(addr string) attribute.KeyValue {
	return c.NetSockPeerAddrKey.String(addr)
}

func (c *netConv) SockPeerPort(port int) attribute.KeyValue {
	return c.NetSockPeerPortKey.Int(port)
}

// splitHostPort splits a network address hostport of the form "host",
// "host%zone", "[host]", "[host%zone], "host:port", "host%zone:port",
// "[host]:port", "[host%zone]:port", or ":port" into host or host%zone and
// port.
//
// An empty host is returned if it is not provided or unparsable. A negative
// port is returned if it is not provided or unparsable.
func splitHostPort(hostport string) (host string, port int) {
	port = -1

	if strings.HasPrefix(hostport, "[") {
		addrEnd := strings.LastIndex(hostport, "]")
		if addrEnd < 0 {
			// Invalid hostport.
			return
		}
		if i := strings.LastIndex(hostport[addrEnd:], ":"); i < 0 {
			host = hostport[1:addrEnd]
			return
		}
	} else {
		if i := strings.LastIndex(hostport, ":"); i < 0 {
			host = hostport
			return
		}
	}

	host, pStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return
	}

	p, err := strconv.ParseUint(pStr, 10, 16)
	if err != nil {
		return
	}
	return host, int(p) // nolint: gosec  // Bitsize checked to be 16 above.
}

func netProtocol(proto string) (name string, version string) {
	name, version, _ = strings.Cut(proto, "/")
	name = strings.ToLower(name)
	return name, version
}
