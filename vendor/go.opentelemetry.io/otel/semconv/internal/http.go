// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package internal // import "go.opentelemetry.io/otel/semconv/internal"

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SemanticConventions are the semantic convention values defined for a
// version of the OpenTelemetry specification.
type SemanticConventions struct {
	EnduserIDKey                attribute.Key
	HTTPClientIPKey             attribute.Key
	HTTPFlavorKey               attribute.Key
	HTTPHostKey                 attribute.Key
	HTTPMethodKey               attribute.Key
	HTTPRequestContentLengthKey attribute.Key
	HTTPRouteKey                attribute.Key
	HTTPSchemeHTTP              attribute.KeyValue
	HTTPSchemeHTTPS             attribute.KeyValue
	HTTPServerNameKey           attribute.Key
	HTTPStatusCodeKey           attribute.Key
	HTTPTargetKey               attribute.Key
	HTTPURLKey                  attribute.Key
	HTTPUserAgentKey            attribute.Key
	NetHostIPKey                attribute.Key
	NetHostNameKey              attribute.Key
	NetHostPortKey              attribute.Key
	NetPeerIPKey                attribute.Key
	NetPeerNameKey              attribute.Key
	NetPeerPortKey              attribute.Key
	NetTransportIP              attribute.KeyValue
	NetTransportOther           attribute.KeyValue
	NetTransportTCP             attribute.KeyValue
	NetTransportUDP             attribute.KeyValue
	NetTransportUnix            attribute.KeyValue
}

// NetAttributesFromHTTPRequest generates attributes of the net
// namespace as specified by the OpenTelemetry specification for a
// span.  The network parameter is a string that net.Dial function
// from standard library can understand.
func (sc *SemanticConventions) NetAttributesFromHTTPRequest(network string, request *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}

	switch network {
	case "tcp", "tcp4", "tcp6":
		attrs = append(attrs, sc.NetTransportTCP)
	case "udp", "udp4", "udp6":
		attrs = append(attrs, sc.NetTransportUDP)
	case "ip", "ip4", "ip6":
		attrs = append(attrs, sc.NetTransportIP)
	case "unix", "unixgram", "unixpacket":
		attrs = append(attrs, sc.NetTransportUnix)
	default:
		attrs = append(attrs, sc.NetTransportOther)
	}

	peerIP, peerName, peerPort := hostIPNamePort(request.RemoteAddr)
	if peerIP != "" {
		attrs = append(attrs, sc.NetPeerIPKey.String(peerIP))
	}
	if peerName != "" {
		attrs = append(attrs, sc.NetPeerNameKey.String(peerName))
	}
	if peerPort != 0 {
		attrs = append(attrs, sc.NetPeerPortKey.Int(peerPort))
	}

	hostIP, hostName, hostPort := "", "", 0
	for _, someHost := range []string{request.Host, request.Header.Get("Host"), request.URL.Host} {
		hostIP, hostName, hostPort = hostIPNamePort(someHost)
		if hostIP != "" || hostName != "" || hostPort != 0 {
			break
		}
	}
	if hostIP != "" {
		attrs = append(attrs, sc.NetHostIPKey.String(hostIP))
	}
	if hostName != "" {
		attrs = append(attrs, sc.NetHostNameKey.String(hostName))
	}
	if hostPort != 0 {
		attrs = append(attrs, sc.NetHostPortKey.Int(hostPort))
	}

	return attrs
}

// hostIPNamePort extracts the IP address, name and (optional) port from hostWithPort.
// It handles both IPv4 and IPv6 addresses. If the host portion is not recognized
// as a valid IPv4 or IPv6 address, the `ip` result will be empty and the
// host portion will instead be returned in `name`.
func hostIPNamePort(hostWithPort string) (ip string, name string, port int) {
	var (
		hostPart, portPart string
		parsedPort         uint64
		err                error
	)
	if hostPart, portPart, err = net.SplitHostPort(hostWithPort); err != nil {
		hostPart, portPart = hostWithPort, ""
	}
	if parsedIP := net.ParseIP(hostPart); parsedIP != nil {
		ip = parsedIP.String()
	} else {
		name = hostPart
	}
	if parsedPort, err = strconv.ParseUint(portPart, 10, 16); err == nil {
		port = int(parsedPort) // nolint: gosec  // Bit size of 16 checked above.
	}
	return
}

// EndUserAttributesFromHTTPRequest generates attributes of the
// enduser namespace as specified by the OpenTelemetry specification
// for a span.
func (sc *SemanticConventions) EndUserAttributesFromHTTPRequest(request *http.Request) []attribute.KeyValue {
	if username, _, ok := request.BasicAuth(); ok {
		return []attribute.KeyValue{sc.EnduserIDKey.String(username)}
	}
	return nil
}

// HTTPClientAttributesFromHTTPRequest generates attributes of the
// http namespace as specified by the OpenTelemetry specification for
// a span on the client side.
func (sc *SemanticConventions) HTTPClientAttributesFromHTTPRequest(request *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}

	// remove any username/password info that may be in the URL
	// before adding it to the attributes
	userinfo := request.URL.User
	request.URL.User = nil

	attrs = append(attrs, sc.HTTPURLKey.String(request.URL.String()))

	// restore any username/password info that was removed
	request.URL.User = userinfo

	return append(attrs, sc.httpCommonAttributesFromHTTPRequest(request)...)
}

func (sc *SemanticConventions) httpCommonAttributesFromHTTPRequest(request *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	if ua := request.UserAgent(); ua != "" {
		attrs = append(attrs, sc.HTTPUserAgentKey.String(ua))
	}
	if request.ContentLength > 0 {
		attrs = append(attrs, sc.HTTPRequestContentLengthKey.Int64(request.ContentLength))
	}

	return append(attrs, sc.httpBasicAttributesFromHTTPRequest(request)...)
}

func (sc *SemanticConventions) httpBasicAttributesFromHTTPRequest(request *http.Request) []attribute.KeyValue {
	// as these attributes are used by HTTPServerMetricAttributesFromHTTPRequest, they should be low-cardinality
	attrs := []attribute.KeyValue{}

	if request.TLS != nil {
		attrs = append(attrs, sc.HTTPSchemeHTTPS)
	} else {
		attrs = append(attrs, sc.HTTPSchemeHTTP)
	}

	if request.Host != "" {
		attrs = append(attrs, sc.HTTPHostKey.String(request.Host))
	} else if request.URL != nil && request.URL.Host != "" {
		attrs = append(attrs, sc.HTTPHostKey.String(request.URL.Host))
	}

	flavor := ""
	if request.ProtoMajor == 1 {
		flavor = fmt.Sprintf("1.%d", request.ProtoMinor)
	} else if request.ProtoMajor == 2 {
		flavor = "2"
	}
	if flavor != "" {
		attrs = append(attrs, sc.HTTPFlavorKey.String(flavor))
	}

	if request.Method != "" {
		attrs = append(attrs, sc.HTTPMethodKey.String(request.Method))
	} else {
		attrs = append(attrs, sc.HTTPMethodKey.String(http.MethodGet))
	}

	return attrs
}

// HTTPServerMetricAttributesFromHTTPRequest generates low-cardinality attributes
// to be used with server-side HTTP metrics.
func (sc *SemanticConventions) HTTPServerMetricAttributesFromHTTPRequest(serverName string, request *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	if serverName != "" {
		attrs = append(attrs, sc.HTTPServerNameKey.String(serverName))
	}
	return append(attrs, sc.httpBasicAttributesFromHTTPRequest(request)...)
}

// HTTPServerAttributesFromHTTPRequest generates attributes of the
// http namespace as specified by the OpenTelemetry specification for
// a span on the server side. Currently, only basic authentication is
// supported.
func (sc *SemanticConventions) HTTPServerAttributesFromHTTPRequest(serverName, route string, request *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		sc.HTTPTargetKey.String(request.RequestURI),
	}

	if serverName != "" {
		attrs = append(attrs, sc.HTTPServerNameKey.String(serverName))
	}
	if route != "" {
		attrs = append(attrs, sc.HTTPRouteKey.String(route))
	}
	if values := request.Header["X-Forwarded-For"]; len(values) > 0 {
		addr := values[0]
		if i := strings.Index(addr, ","); i > 0 {
			addr = addr[:i]
		}
		attrs = append(attrs, sc.HTTPClientIPKey.String(addr))
	}

	return append(attrs, sc.httpCommonAttributesFromHTTPRequest(request)...)
}

// HTTPAttributesFromHTTPStatusCode generates attributes of the http
// namespace as specified by the OpenTelemetry specification for a
// span.
func (sc *SemanticConventions) HTTPAttributesFromHTTPStatusCode(code int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		sc.HTTPStatusCodeKey.Int(code),
	}
	return attrs
}

type codeRange struct {
	fromInclusive int
	toInclusive   int
}

func (r codeRange) contains(code int) bool {
	return r.fromInclusive <= code && code <= r.toInclusive
}

var validRangesPerCategory = map[int][]codeRange{
	1: {
		{http.StatusContinue, http.StatusEarlyHints},
	},
	2: {
		{http.StatusOK, http.StatusAlreadyReported},
		{http.StatusIMUsed, http.StatusIMUsed},
	},
	3: {
		{http.StatusMultipleChoices, http.StatusUseProxy},
		{http.StatusTemporaryRedirect, http.StatusPermanentRedirect},
	},
	4: {
		{http.StatusBadRequest, http.StatusTeapot}, // yes, teapot is so useful…
		{http.StatusMisdirectedRequest, http.StatusUpgradeRequired},
		{http.StatusPreconditionRequired, http.StatusTooManyRequests},
		{http.StatusRequestHeaderFieldsTooLarge, http.StatusRequestHeaderFieldsTooLarge},
		{http.StatusUnavailableForLegalReasons, http.StatusUnavailableForLegalReasons},
	},
	5: {
		{http.StatusInternalServerError, http.StatusLoopDetected},
		{http.StatusNotExtended, http.StatusNetworkAuthenticationRequired},
	},
}

// SpanStatusFromHTTPStatusCode generates a status code and a message
// as specified by the OpenTelemetry specification for a span.
func SpanStatusFromHTTPStatusCode(code int) (codes.Code, string) {
	spanCode, valid := validateHTTPStatusCode(code)
	if !valid {
		return spanCode, fmt.Sprintf("Invalid HTTP status code %d", code)
	}
	return spanCode, ""
}

// SpanStatusFromHTTPStatusCodeAndSpanKind generates a status code and a message
// as specified by the OpenTelemetry specification for a span.
// Exclude 4xx for SERVER to set the appropriate status.
func SpanStatusFromHTTPStatusCodeAndSpanKind(code int, spanKind trace.SpanKind) (codes.Code, string) {
	spanCode, valid := validateHTTPStatusCode(code)
	if !valid {
		return spanCode, fmt.Sprintf("Invalid HTTP status code %d", code)
	}
	category := code / 100
	if spanKind == trace.SpanKindServer && category == 4 {
		return codes.Unset, ""
	}
	return spanCode, ""
}

// validateHTTPStatusCode validates the HTTP status code and returns
// corresponding span status code. If the `code` is not a valid HTTP status
// code, returns span status Error and false.
func validateHTTPStatusCode(code int) (codes.Code, bool) {
	category := code / 100
	ranges, ok := validRangesPerCategory[category]
	if !ok {
		return codes.Error, false
	}
	ok = false
	for _, crange := range ranges {
		ok = crange.contains(code)
		if ok {
			break
		}
	}
	if !ok {
		return codes.Error, false
	}
	if category > 0 && category < 4 {
		return codes.Unset, true
	}
	return codes.Error, true
}
