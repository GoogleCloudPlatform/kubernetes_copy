/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package net

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/http2"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"
)

// JoinPreservingTrailingSlash does a path.Join of the specified elements,
// preserving any trailing slash on the last non-empty segment
func JoinPreservingTrailingSlash(elem ...string) string {
	// do the basic path join
	result := path.Join(elem...)

	// find the last non-empty segment
	for i := len(elem) - 1; i >= 0; i-- {
		if len(elem[i]) > 0 {
			// if the last segment ended in a slash, ensure our result does as well
			if strings.HasSuffix(elem[i], "/") && !strings.HasSuffix(result, "/") {
				result += "/"
			}
			break
		}
	}

	return result
}

// IsTimeout returns true if the given error is a network timeout error
func IsTimeout(err error) bool {
	var neterr net.Error
	if errors.As(err, &neterr) {
		return neterr != nil && neterr.Timeout()
	}
	return false
}

// IsProbableEOF returns true if the given error resembles a connection termination
// scenario that would justify assuming that the watch is empty.
// These errors are what the Go http stack returns back to us which are general
// connection closure errors (strongly correlated) and callers that need to
// differentiate probable errors in connection behavior between normal "this is
// disconnected" should use the method.
func IsProbableEOF(err error) bool {
	if err == nil {
		return false
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	msg := err.Error()
	switch {
	case err == io.EOF:
		return true
	case err == io.ErrUnexpectedEOF:
		return true
	case msg == "http: can't write HTTP request on broken connection":
		return true
	case strings.Contains(msg, "http2: server sent GOAWAY and closed the connection"):
		return true
	case strings.Contains(msg, "connection reset by peer"):
		return true
	case strings.Contains(strings.ToLower(msg), "use of closed network connection"):
		return true
	}
	return false
}

var defaultTransport = http.DefaultTransport.(*http.Transport)

// SetOldTransportDefaults applies the defaults from http.DefaultTransport
// for the Proxy, Dial, and TLSHandshakeTimeout fields if unset
func SetOldTransportDefaults(t *http.Transport) *http.Transport {
	if t.Proxy == nil || isDefault(t.Proxy) {
		// http.ProxyFromEnvironment doesn't respect CIDRs and that makes it impossible to exclude things like pod and service IPs from proxy settings
		// ProxierWithNoProxyCIDR allows CIDR rules in NO_PROXY
		t.Proxy = NewProxierWithNoProxyCIDR(http.ProxyFromEnvironment)
	}
	// If no custom dialer is set, use the default context dialer
	//lint:file-ignore SA1019 Keep supporting deprecated Dial method of custom transports
	if t.DialContext == nil && t.Dial == nil {
		t.DialContext = defaultTransport.DialContext
	}
	if t.TLSHandshakeTimeout == 0 {
		t.TLSHandshakeTimeout = defaultTransport.TLSHandshakeTimeout
	}
	if t.IdleConnTimeout == 0 {
		t.IdleConnTimeout = defaultTransport.IdleConnTimeout
	}
	return t
}

// SetTransportDefaults applies the defaults from http.DefaultTransport
// for the Proxy, Dial, and TLSHandshakeTimeout fields if unset
func SetTransportDefaults(t *http.Transport) *http.Transport {
	t = SetOldTransportDefaults(t)
	// Allow clients to disable http2 if needed.
	if s := os.Getenv("DISABLE_HTTP2"); len(s) > 0 {
		//nolint:logcheck // Should be rare, not worth converting.
		klog.Info("HTTP2 has been explicitly disabled")
	} else if allowsHTTP2(t) {
		if err := configureHTTP2Transport(t); err != nil {
			//nolint:logcheck // Should be rare, not worth converting.
			klog.Warningf("Transport failed http2 configuration: %v", err)
		}
	}
	return t
}

func readIdleTimeoutSeconds() int {
	ret := 30
	// User can set the readIdleTimeout to 0 to disable the HTTP/2
	// connection health check.
	if s := os.Getenv("HTTP2_READ_IDLE_TIMEOUT_SECONDS"); len(s) > 0 {
		i, err := strconv.Atoi(s)
		if err != nil {
			//nolint:logcheck // Should be rare, not worth converting.
			klog.Warningf("Illegal HTTP2_READ_IDLE_TIMEOUT_SECONDS(%q): %v."+
				" Default value %d is used", s, err, ret)
			return ret
		}
		ret = i
	}
	return ret
}

func pingTimeoutSeconds() int {
	ret := 15
	if s := os.Getenv("HTTP2_PING_TIMEOUT_SECONDS"); len(s) > 0 {
		i, err := strconv.Atoi(s)
		if err != nil {
			//nolint:logcheck // Should be rare, not worth converting.
			klog.Warningf("Illegal HTTP2_PING_TIMEOUT_SECONDS(%q): %v."+
				" Default value %d is used", s, err, ret)
			return ret
		}
		ret = i
	}
	return ret
}

func configureHTTP2Transport(t *http.Transport) error {
	t2, err := http2.ConfigureTransports(t)
	if err != nil {
		return err
	}
	// The following enables the HTTP/2 connection health check added in
	// https://github.com/golang/net/pull/55. The health check detects and
	// closes broken transport layer connections. Without the health check,
	// a broken connection can linger too long, e.g., a broken TCP
	// connection will be closed by the Linux kernel after 13 to 30 minutes
	// by default, which caused
	// https://github.com/kubernetes/client-go/issues/374 and
	// https://github.com/kubernetes/kubernetes/issues/87615.
	t2.ReadIdleTimeout = time.Duration(readIdleTimeoutSeconds()) * time.Second
	t2.PingTimeout = time.Duration(pingTimeoutSeconds()) * time.Second
	return nil
}

func allowsHTTP2(t *http.Transport) bool {
	if t.TLSClientConfig == nil || len(t.TLSClientConfig.NextProtos) == 0 {
		// the transport expressed no NextProto preference, allow
		return true
	}
	for _, p := range t.TLSClientConfig.NextProtos {
		if p == http2.NextProtoTLS {
			// the transport explicitly allowed http/2
			return true
		}
	}
	// the transport explicitly set NextProtos and excluded http/2
	return false
}

type RoundTripperWrapper interface {
	http.RoundTripper
	WrappedRoundTripper() http.RoundTripper
}

type DialFunc func(ctx context.Context, net, addr string) (net.Conn, error)

func DialerFor(transport http.RoundTripper) (DialFunc, error) {
	if transport == nil {
		return nil, nil
	}

	switch transport := transport.(type) {
	case *http.Transport:
		// transport.DialContext takes precedence over transport.Dial
		if transport.DialContext != nil {
			return transport.DialContext, nil
		}
		// adapt transport.Dial to the DialWithContext signature
		if transport.Dial != nil {
			return func(ctx context.Context, net, addr string) (net.Conn, error) {
				return transport.Dial(net, addr)
			}, nil
		}
		// otherwise return nil
		return nil, nil
	case RoundTripperWrapper:
		return DialerFor(transport.WrappedRoundTripper())
	default:
		return nil, fmt.Errorf("unknown transport type: %T", transport)
	}
}

// CloseIdleConnectionsFor close idles connections for the Transport.
// If the Transport is wrapped it iterates over the wrapped round trippers
// until it finds one that implements the CloseIdleConnections method.
// If the Transport does not have a CloseIdleConnections method
// then this function does nothing.
func CloseIdleConnectionsFor(transport http.RoundTripper) {
	if transport == nil {
		return
	}
	type closeIdler interface {
		CloseIdleConnections()
	}

	switch transport := transport.(type) {
	case closeIdler:
		transport.CloseIdleConnections()
	case RoundTripperWrapper:
		CloseIdleConnectionsFor(transport.WrappedRoundTripper())
	default:
		//nolint:logcheck // Should be rare, not worth converting.
		klog.Warningf("unknown transport type: %T", transport)
	}
}

type TLSClientConfigHolder interface {
	TLSClientConfig() *tls.Config
}

func TLSClientConfig(transport http.RoundTripper) (*tls.Config, error) {
	if transport == nil {
		return nil, nil
	}

	switch transport := transport.(type) {
	case *http.Transport:
		return transport.TLSClientConfig, nil
	case TLSClientConfigHolder:
		return transport.TLSClientConfig(), nil
	case RoundTripperWrapper:
		return TLSClientConfig(transport.WrappedRoundTripper())
	default:
		return nil, fmt.Errorf("unknown transport type: %T", transport)
	}
}

func FormatURL(scheme string, host string, port int, path string) *url.URL {
	return &url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   path,
	}
}

func GetHTTPClient(req *http.Request) string {
	if ua := req.UserAgent(); len(ua) != 0 {
		return ua
	}
	return "unknown"
}

// SourceIPs splits the comma separated X-Forwarded-For header and joins it with
// the X-Real-Ip header and/or req.RemoteAddr, ignoring invalid IPs.
// The X-Real-Ip is omitted if it's already present in the X-Forwarded-For chain.
// The req.RemoteAddr is always the last IP in the returned list.
// It returns nil if all of these are empty or invalid.
func SourceIPs(req *http.Request) []net.IP {
	var srcIPs []net.IP

	hdr := req.Header
	// First check the X-Forwarded-For header for requests via proxy.
	hdrForwardedFor := hdr.Get("X-Forwarded-For")
	if hdrForwardedFor != "" {
		// X-Forwarded-For can be a csv of IPs in case of multiple proxies.
		// Use the first valid one.
		parts := strings.Split(hdrForwardedFor, ",")
		for _, part := range parts {
			ip := netutils.ParseIPSloppy(strings.TrimSpace(part))
			if ip != nil {
				srcIPs = append(srcIPs, ip)
			}
		}
	}

	// Try the X-Real-Ip header.
	hdrRealIp := hdr.Get("X-Real-Ip")
	if hdrRealIp != "" {
		ip := netutils.ParseIPSloppy(hdrRealIp)
		// Only append the X-Real-Ip if it's not already contained in the X-Forwarded-For chain.
		if ip != nil && !containsIP(srcIPs, ip) {
			srcIPs = append(srcIPs, ip)
		}
	}

	// Always include the request Remote Address as it cannot be easily spoofed.
	var remoteIP net.IP
	// Remote Address in Go's HTTP server is in the form host:port so we need to split that first.
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err == nil {
		remoteIP = netutils.ParseIPSloppy(host)
	}
	// Fallback if Remote Address was just IP.
	if remoteIP == nil {
		remoteIP = netutils.ParseIPSloppy(req.RemoteAddr)
	}

	// Don't duplicate remote IP if it's already the last address in the chain.
	if remoteIP != nil && (len(srcIPs) == 0 || !remoteIP.Equal(srcIPs[len(srcIPs)-1])) {
		srcIPs = append(srcIPs, remoteIP)
	}

	return srcIPs
}

// Checks whether the given IP address is contained in the list of IPs.
func containsIP(ips []net.IP, ip net.IP) bool {
	for _, v := range ips {
		if v.Equal(ip) {
			return true
		}
	}
	return false
}

// Extracts and returns the clients IP from the given request.
// Looks at X-Forwarded-For header, X-Real-Ip header and request.RemoteAddr in that order.
// Returns nil if none of them are set or is set to an invalid value.
func GetClientIP(req *http.Request) net.IP {
	ips := SourceIPs(req)
	if len(ips) == 0 {
		return nil
	}
	return ips[0]
}

// Prepares the X-Forwarded-For header for another forwarding hop by appending the previous sender's
// IP address to the X-Forwarded-For chain.
func AppendForwardedForHeader(req *http.Request) {
	// Copied from net/http/httputil/reverseproxy.go:
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		req.Header.Set("X-Forwarded-For", clientIP)
	}
}

var defaultProxyFuncPointer = fmt.Sprintf("%p", http.ProxyFromEnvironment)

// isDefault checks to see if the transportProxierFunc is pointing to the default one
func isDefault(transportProxier func(*http.Request) (*url.URL, error)) bool {
	transportProxierPointer := fmt.Sprintf("%p", transportProxier)
	return transportProxierPointer == defaultProxyFuncPointer
}

// NewProxierWithNoProxyCIDR constructs a Proxier function that respects CIDRs in NO_PROXY and delegates if
// no matching CIDRs are found
func NewProxierWithNoProxyCIDR(delegate func(req *http.Request) (*url.URL, error)) func(req *http.Request) (*url.URL, error) {
	// we wrap the default method, so we only need to perform our check if the NO_PROXY (or no_proxy) envvar has a CIDR in it
	noProxyEnv := os.Getenv("NO_PROXY")
	if noProxyEnv == "" {
		noProxyEnv = os.Getenv("no_proxy")
	}
	noProxyRules := strings.Split(noProxyEnv, ",")

	cidrs := []*net.IPNet{}
	for _, noProxyRule := range noProxyRules {
		_, cidr, _ := netutils.ParseCIDRSloppy(noProxyRule)
		if cidr != nil {
			cidrs = append(cidrs, cidr)
		}
	}

	if len(cidrs) == 0 {
		return delegate
	}

	return func(req *http.Request) (*url.URL, error) {
		ip := netutils.ParseIPSloppy(req.URL.Hostname())
		if ip == nil {
			return delegate(req)
		}

		for _, cidr := range cidrs {
			if cidr.Contains(ip) {
				return nil, nil
			}
		}

		return delegate(req)
	}
}

// DialerFunc implements Dialer for the provided function.
type DialerFunc func(req *http.Request) (net.Conn, error)

func (fn DialerFunc) Dial(req *http.Request) (net.Conn, error) {
	return fn(req)
}

// Dialer dials a host and writes a request to it.
type Dialer interface {
	// Dial connects to the host specified by req's URL, writes the request to the connection, and
	// returns the opened net.Conn.
	Dial(req *http.Request) (net.Conn, error)
}

// CloneRequest creates a shallow copy of the request along with a deep copy of the Headers.
func CloneRequest(req *http.Request) *http.Request {
	r := new(http.Request)

	// shallow clone
	*r = *req

	// deep copy headers
	r.Header = CloneHeader(req.Header)

	return r
}

// CloneHeader creates a deep copy of an http.Header.
func CloneHeader(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for key, values := range in {
		newValues := make([]string, len(values))
		copy(newValues, values)
		out[key] = newValues
	}
	return out
}

// WarningHeader contains a single RFC2616 14.46 warnings header
type WarningHeader struct {
	// Codeindicates the type of warning. 299 is a miscellaneous persistent warning
	Code int
	// Agent contains the name or pseudonym of the server adding the Warning header.
	// A single "-" is recommended when agent is unknown.
	Agent string
	// Warning text
	Text string
}

// ParseWarningHeaders extract RFC2616 14.46 warnings headers from the specified set of header values.
// Multiple comma-separated warnings per header are supported.
// If errors are encountered on a header, the remainder of that header are skipped and subsequent headers are parsed.
// Returns successfully parsed warnings and any errors encountered.
func ParseWarningHeaders(headers []string) ([]WarningHeader, []error) {
	var (
		results []WarningHeader
		errs    []error
	)
	for _, header := range headers {
		for len(header) > 0 {
			result, remainder, err := ParseWarningHeader(header)
			if err != nil {
				errs = append(errs, err)
				break
			}
			results = append(results, result)
			header = remainder
		}
	}
	return results, errs
}

var (
	codeMatcher = regexp.MustCompile(`^[0-9]{3}$`)
	wordDecoder = &mime.WordDecoder{}
)

// ParseWarningHeader extracts one RFC2616 14.46 warning from the specified header,
// returning an error if the header does not contain a correctly formatted warning.
// Any remaining content in the header is returned.
func ParseWarningHeader(header string) (result WarningHeader, remainder string, err error) {
	// https://tools.ietf.org/html/rfc2616#section-14.46
	//   updated by
	// https://tools.ietf.org/html/rfc7234#section-5.5
	//   https://tools.ietf.org/html/rfc7234#appendix-A
	//     Some requirements regarding production and processing of the Warning
	//     header fields have been relaxed, as it is not widely implemented.
	//     Furthermore, the Warning header field no longer uses RFC 2047
	//     encoding, nor does it allow multiple languages, as these aspects were
	//     not implemented.
	//
	// Format is one of:
	// warn-code warn-agent "warn-text"
	// warn-code warn-agent "warn-text" "warn-date"
	//
	// warn-code is a three digit number
	// warn-agent is unquoted and contains no spaces
	// warn-text is quoted with backslash escaping (RFC2047-encoded according to RFC2616, not encoded according to RFC7234)
	// warn-date is optional, quoted, and in HTTP-date format (no embedded or escaped quotes)
	//
	// additional warnings can optionally be included in the same header by comma-separating them:
	// warn-code warn-agent "warn-text" "warn-date"[, warn-code warn-agent "warn-text" "warn-date", ...]

	// tolerate leading whitespace
	header = strings.TrimSpace(header)

	parts := strings.SplitN(header, " ", 3)
	if len(parts) != 3 {
		return WarningHeader{}, "", errors.New("invalid warning header: fewer than 3 segments")
	}
	code, agent, textDateRemainder := parts[0], parts[1], parts[2]

	// verify code format
	if !codeMatcher.Match([]byte(code)) {
		return WarningHeader{}, "", errors.New("invalid warning header: code segment is not 3 digits between 100-299")
	}
	codeInt, _ := strconv.ParseInt(code, 10, 64)

	// verify agent presence
	if len(agent) == 0 {
		return WarningHeader{}, "", errors.New("invalid warning header: empty agent segment")
	}
	if !utf8.ValidString(agent) || hasAnyRunes(agent, unicode.IsControl) {
		return WarningHeader{}, "", errors.New("invalid warning header: invalid agent")
	}

	// verify textDateRemainder presence
	if len(textDateRemainder) == 0 {
		return WarningHeader{}, "", errors.New("invalid warning header: empty text segment")
	}

	// extract text
	text, dateAndRemainder, err := parseQuotedString(textDateRemainder)
	if err != nil {
		return WarningHeader{}, "", fmt.Errorf("invalid warning header: %v", err)
	}
	// tolerate RFC2047-encoded text from warnings produced according to RFC2616
	if decodedText, err := wordDecoder.DecodeHeader(text); err == nil {
		text = decodedText
	}
	if !utf8.ValidString(text) || hasAnyRunes(text, unicode.IsControl) {
		return WarningHeader{}, "", errors.New("invalid warning header: invalid text")
	}
	result = WarningHeader{Code: int(codeInt), Agent: agent, Text: text}

	if len(dateAndRemainder) > 0 {
		if dateAndRemainder[0] == '"' {
			// consume date
			foundEndQuote := false
			for i := 1; i < len(dateAndRemainder); i++ {
				if dateAndRemainder[i] == '"' {
					foundEndQuote = true
					remainder = strings.TrimSpace(dateAndRemainder[i+1:])
					break
				}
			}
			if !foundEndQuote {
				return WarningHeader{}, "", errors.New("invalid warning header: unterminated date segment")
			}
		} else {
			remainder = dateAndRemainder
		}
	}
	if len(remainder) > 0 {
		if remainder[0] == ',' {
			// consume comma if present
			remainder = strings.TrimSpace(remainder[1:])
		} else {
			return WarningHeader{}, "", errors.New("invalid warning header: unexpected token after warn-date")
		}
	}

	return result, remainder, nil
}

func parseQuotedString(quotedString string) (string, string, error) {
	if len(quotedString) == 0 {
		return "", "", errors.New("invalid quoted string: 0-length")
	}

	if quotedString[0] != '"' {
		return "", "", errors.New("invalid quoted string: missing initial quote")
	}

	quotedString = quotedString[1:]
	var remainder string
	escaping := false
	closedQuote := false
	result := &strings.Builder{}
loop:
	for i := 0; i < len(quotedString); i++ {
		b := quotedString[i]
		switch b {
		case '"':
			if escaping {
				result.WriteByte(b)
				escaping = false
			} else {
				closedQuote = true
				remainder = strings.TrimSpace(quotedString[i+1:])
				break loop
			}
		case '\\':
			if escaping {
				result.WriteByte(b)
				escaping = false
			} else {
				escaping = true
			}
		default:
			result.WriteByte(b)
			escaping = false
		}
	}

	if !closedQuote {
		return "", "", errors.New("invalid quoted string: missing closing quote")
	}
	return result.String(), remainder, nil
}

func NewWarningHeader(code int, agent, text string) (string, error) {
	if code < 0 || code > 999 {
		return "", errors.New("code must be between 0 and 999")
	}
	if len(agent) == 0 {
		agent = "-"
	} else if !utf8.ValidString(agent) || strings.ContainsAny(agent, `\"`) || hasAnyRunes(agent, unicode.IsSpace, unicode.IsControl) {
		return "", errors.New("agent must be valid UTF-8 and must not contain spaces, quotes, backslashes, or control characters")
	}
	if !utf8.ValidString(text) || hasAnyRunes(text, unicode.IsControl) {
		return "", errors.New("text must be valid UTF-8 and must not contain control characters")
	}
	return fmt.Sprintf("%03d %s %s", code, agent, makeQuotedString(text)), nil
}

func hasAnyRunes(s string, runeCheckers ...func(rune) bool) bool {
	for _, r := range s {
		for _, checker := range runeCheckers {
			if checker(r) {
				return true
			}
		}
	}
	return false
}

func makeQuotedString(s string) string {
	result := &bytes.Buffer{}
	// opening quote
	result.WriteRune('"')
	for _, c := range s {
		switch c {
		case '"', '\\':
			// escape " and \
			result.WriteRune('\\')
			result.WriteRune(c)
		default:
			// write everything else as-is
			result.WriteRune(c)
		}
	}
	// closing quote
	result.WriteRune('"')
	return result.String()
}
