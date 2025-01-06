// Code created by gotmpl. DO NOT MODIFY.
// source: internal/shared/semconvutil/httpconv.go.tmpl

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package semconvutil // import "go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful/internal/semconvutil"

import (
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
)

// HTTPClientResponse returns trace attributes for an HTTP response received by a
// client from a server. It will return the following attributes if the related
// values are defined in resp: "http.status.code",
// "http.response_content_length".
//
// This does not add all OpenTelemetry required attributes for an HTTP event,
// it assumes ClientRequest was used to create the span with a complete set of
// attributes. If a complete set of attributes can be generated using the
// request contained in resp. For example:
//
//	append(HTTPClientResponse(resp), ClientRequest(resp.Request)...)
func HTTPClientResponse(resp *http.Response) []attribute.KeyValue {
	return hc.ClientResponse(resp)
}

// HTTPClientRequest returns trace attributes for an HTTP request made by a client.
// The following attributes are always returned: "http.url", "http.method",
// "net.peer.name". The following attributes are returned if the related values
// are defined in req: "net.peer.port", "user_agent.original",
// "http.request_content_length".
func HTTPClientRequest(req *http.Request) []attribute.KeyValue {
	return hc.ClientRequest(req)
}

// HTTPClientRequestMetrics returns metric attributes for an HTTP request made by a client.
// The following attributes are always returned: "http.method", "net.peer.name".
// The following attributes are returned if the
// related values are defined in req: "net.peer.port".
func HTTPClientRequestMetrics(req *http.Request) []attribute.KeyValue {
	return hc.ClientRequestMetrics(req)
}

// HTTPClientStatus returns a span status code and message for an HTTP status code
// value received by a client.
func HTTPClientStatus(code int) (codes.Code, string) {
	return hc.ClientStatus(code)
}

// HTTPServerRequest returns trace attributes for an HTTP request received by a
// server.
//
// The server must be the primary server name if it is known. For example this
// would be the ServerName directive
// (https://httpd.apache.org/docs/2.4/mod/core.html#servername) for an Apache
// server, and the server_name directive
// (http://nginx.org/en/docs/http/ngx_http_core_module.html#server_name) for an
// nginx server. More generically, the primary server name would be the host
// header value that matches the default virtual host of an HTTP server. It
// should include the host identifier and if a port is used to route to the
// server that port identifier should be included as an appropriate port
// suffix.
//
// If the primary server name is not known, server should be an empty string.
// The req Host will be used to determine the server instead.
//
// The following attributes are always returned: "http.method", "http.scheme",
// "http.target", "net.host.name". The following attributes are returned if
// they related values are defined in req: "net.host.port", "net.sock.peer.addr",
// "net.sock.peer.port", "user_agent.original", "http.client_ip".
func HTTPServerRequest(server string, req *http.Request) []attribute.KeyValue {
	return hc.ServerRequest(server, req)
}

// HTTPServerRequestMetrics returns metric attributes for an HTTP request received by a
// server.
//
// The server must be the primary server name if it is known. For example this
// would be the ServerName directive
// (https://httpd.apache.org/docs/2.4/mod/core.html#servername) for an Apache
// server, and the server_name directive
// (http://nginx.org/en/docs/http/ngx_http_core_module.html#server_name) for an
// nginx server. More generically, the primary server name would be the host
// header value that matches the default virtual host of an HTTP server. It
// should include the host identifier and if a port is used to route to the
// server that port identifier should be included as an appropriate port
// suffix.
//
// If the primary server name is not known, server should be an empty string.
// The req Host will be used to determine the server instead.
//
// The following attributes are always returned: "http.method", "http.scheme",
// "net.host.name". The following attributes are returned if they related
// values are defined in req: "net.host.port".
func HTTPServerRequestMetrics(server string, req *http.Request) []attribute.KeyValue {
	return hc.ServerRequestMetrics(server, req)
}

// HTTPServerStatus returns a span status code and message for an HTTP status code
// value returned by a server. Status codes in the 400-499 range are not
// returned as errors.
func HTTPServerStatus(code int) (codes.Code, string) {
	return hc.ServerStatus(code)
}

// httpConv are the HTTP semantic convention attributes defined for a version
// of the OpenTelemetry specification.
type httpConv struct {
	NetConv *netConv

	HTTPClientIPKey              attribute.Key
	HTTPMethodKey                attribute.Key
	HTTPRequestContentLengthKey  attribute.Key
	HTTPResponseContentLengthKey attribute.Key
	HTTPRouteKey                 attribute.Key
	HTTPSchemeHTTP               attribute.KeyValue
	HTTPSchemeHTTPS              attribute.KeyValue
	HTTPStatusCodeKey            attribute.Key
	HTTPTargetKey                attribute.Key
	HTTPURLKey                   attribute.Key
	UserAgentOriginalKey         attribute.Key
}

var hc = &httpConv{
	NetConv: nc,

	HTTPClientIPKey:              semconv.HTTPClientIPKey,
	HTTPMethodKey:                semconv.HTTPMethodKey,
	HTTPRequestContentLengthKey:  semconv.HTTPRequestContentLengthKey,
	HTTPResponseContentLengthKey: semconv.HTTPResponseContentLengthKey,
	HTTPRouteKey:                 semconv.HTTPRouteKey,
	HTTPSchemeHTTP:               semconv.HTTPSchemeHTTP,
	HTTPSchemeHTTPS:              semconv.HTTPSchemeHTTPS,
	HTTPStatusCodeKey:            semconv.HTTPStatusCodeKey,
	HTTPTargetKey:                semconv.HTTPTargetKey,
	HTTPURLKey:                   semconv.HTTPURLKey,
	UserAgentOriginalKey:         semconv.UserAgentOriginalKey,
}

// ClientResponse returns attributes for an HTTP response received by a client
// from a server. The following attributes are returned if the related values
// are defined in resp: "http.status.code", "http.response_content_length".
//
// This does not add all OpenTelemetry required attributes for an HTTP event,
// it assumes ClientRequest was used to create the span with a complete set of
// attributes. If a complete set of attributes can be generated using the
// request contained in resp. For example:
//
//	append(ClientResponse(resp), ClientRequest(resp.Request)...)
func (c *httpConv) ClientResponse(resp *http.Response) []attribute.KeyValue {
	/* The following semantic conventions are returned if present:
	http.status_code                int
	http.response_content_length    int
	*/
	var n int
	if resp.StatusCode > 0 {
		n++
	}
	if resp.ContentLength > 0 {
		n++
	}

	attrs := make([]attribute.KeyValue, 0, n)
	if resp.StatusCode > 0 {
		attrs = append(attrs, c.HTTPStatusCodeKey.Int(resp.StatusCode))
	}
	if resp.ContentLength > 0 {
		attrs = append(attrs, c.HTTPResponseContentLengthKey.Int(int(resp.ContentLength)))
	}
	return attrs
}

// ClientRequest returns attributes for an HTTP request made by a client. The
// following attributes are always returned: "http.url", "http.method",
// "net.peer.name". The following attributes are returned if the related values
// are defined in req: "net.peer.port", "user_agent.original",
// "http.request_content_length", "user_agent.original".
func (c *httpConv) ClientRequest(req *http.Request) []attribute.KeyValue {
	/* The following semantic conventions are returned if present:
	http.method                     string
	user_agent.original             string
	http.url                        string
	net.peer.name                   string
	net.peer.port                   int
	http.request_content_length     int
	*/

	/* The following semantic conventions are not returned:
	http.status_code                This requires the response. See ClientResponse.
	http.response_content_length    This requires the response. See ClientResponse.
	net.sock.family                 This requires the socket used.
	net.sock.peer.addr              This requires the socket used.
	net.sock.peer.name              This requires the socket used.
	net.sock.peer.port              This requires the socket used.
	http.resend_count               This is something outside of a single request.
	net.protocol.name               The value is the Request is ignored, and the go client will always use "http".
	net.protocol.version            The value in the Request is ignored, and the go client will always use 1.1 or 2.0.
	*/
	n := 3 // URL, peer name, proto, and method.
	var h string
	if req.URL != nil {
		h = req.URL.Host
	}
	peer, p := firstHostPort(h, req.Header.Get("Host"))
	port := requiredHTTPPort(req.URL != nil && req.URL.Scheme == "https", p)
	if port > 0 {
		n++
	}
	useragent := req.UserAgent()
	if useragent != "" {
		n++
	}
	if req.ContentLength > 0 {
		n++
	}

	attrs := make([]attribute.KeyValue, 0, n)

	attrs = append(attrs, c.method(req.Method))

	var u string
	if req.URL != nil {
		// Remove any username/password info that may be in the URL.
		userinfo := req.URL.User
		req.URL.User = nil
		u = req.URL.String()
		// Restore any username/password info that was removed.
		req.URL.User = userinfo
	}
	attrs = append(attrs, c.HTTPURLKey.String(u))

	attrs = append(attrs, c.NetConv.PeerName(peer))
	if port > 0 {
		attrs = append(attrs, c.NetConv.PeerPort(port))
	}

	if useragent != "" {
		attrs = append(attrs, c.UserAgentOriginalKey.String(useragent))
	}

	if l := req.ContentLength; l > 0 {
		attrs = append(attrs, c.HTTPRequestContentLengthKey.Int64(l))
	}

	return attrs
}

// ClientRequestMetrics returns metric attributes for an HTTP request made by a client. The
// following attributes are always returned: "http.method", "net.peer.name".
// The following attributes are returned if the related values
// are defined in req: "net.peer.port".
func (c *httpConv) ClientRequestMetrics(req *http.Request) []attribute.KeyValue {
	/* The following semantic conventions are returned if present:
	http.method                     string
	net.peer.name                   string
	net.peer.port                   int
	*/

	n := 2 // method, peer name.
	var h string
	if req.URL != nil {
		h = req.URL.Host
	}
	peer, p := firstHostPort(h, req.Header.Get("Host"))
	port := requiredHTTPPort(req.URL != nil && req.URL.Scheme == "https", p)
	if port > 0 {
		n++
	}

	attrs := make([]attribute.KeyValue, 0, n)
	attrs = append(attrs, c.method(req.Method), c.NetConv.PeerName(peer))

	if port > 0 {
		attrs = append(attrs, c.NetConv.PeerPort(port))
	}

	return attrs
}

// ServerRequest returns attributes for an HTTP request received by a server.
//
// The server must be the primary server name if it is known. For example this
// would be the ServerName directive
// (https://httpd.apache.org/docs/2.4/mod/core.html#servername) for an Apache
// server, and the server_name directive
// (http://nginx.org/en/docs/http/ngx_http_core_module.html#server_name) for an
// nginx server. More generically, the primary server name would be the host
// header value that matches the default virtual host of an HTTP server. It
// should include the host identifier and if a port is used to route to the
// server that port identifier should be included as an appropriate port
// suffix.
//
// If the primary server name is not known, server should be an empty string.
// The req Host will be used to determine the server instead.
//
// The following attributes are always returned: "http.method", "http.scheme",
// "http.target", "net.host.name". The following attributes are returned if they
// related values are defined in req: "net.host.port", "net.sock.peer.addr",
// "net.sock.peer.port", "user_agent.original", "http.client_ip",
// "net.protocol.name", "net.protocol.version".
func (c *httpConv) ServerRequest(server string, req *http.Request) []attribute.KeyValue {
	/* The following semantic conventions are returned if present:
	http.method             string
	http.scheme             string
	net.host.name           string
	net.host.port           int
	net.sock.peer.addr      string
	net.sock.peer.port      int
	user_agent.original     string
	http.client_ip          string
	net.protocol.name       string Note: not set if the value is "http".
	net.protocol.version    string
	http.target             string Note: doesn't include the query parameter.
	*/

	/* The following semantic conventions are not returned:
	http.status_code                This requires the response.
	http.request_content_length     This requires the len() of body, which can mutate it.
	http.response_content_length    This requires the response.
	http.route                      This is not available.
	net.sock.peer.name              This would require a DNS lookup.
	net.sock.host.addr              The request doesn't have access to the underlying socket.
	net.sock.host.port              The request doesn't have access to the underlying socket.

	*/
	n := 4 // Method, scheme, proto, and host name.
	var host string
	var p int
	if server == "" {
		host, p = splitHostPort(req.Host)
	} else {
		// Prioritize the primary server name.
		host, p = splitHostPort(server)
		if p < 0 {
			_, p = splitHostPort(req.Host)
		}
	}
	hostPort := requiredHTTPPort(req.TLS != nil, p)
	if hostPort > 0 {
		n++
	}
	peer, peerPort := splitHostPort(req.RemoteAddr)
	if peer != "" {
		n++
		if peerPort > 0 {
			n++
		}
	}
	useragent := req.UserAgent()
	if useragent != "" {
		n++
	}

	clientIP := serverClientIP(req.Header.Get("X-Forwarded-For"))
	if clientIP != "" {
		n++
	}

	var target string
	if req.URL != nil {
		target = req.URL.Path
		if target != "" {
			n++
		}
	}
	protoName, protoVersion := netProtocol(req.Proto)
	if protoName != "" && protoName != "http" {
		n++
	}
	if protoVersion != "" {
		n++
	}

	attrs := make([]attribute.KeyValue, 0, n)

	attrs = append(attrs, c.method(req.Method))
	attrs = append(attrs, c.scheme(req.TLS != nil))
	attrs = append(attrs, c.NetConv.HostName(host))

	if hostPort > 0 {
		attrs = append(attrs, c.NetConv.HostPort(hostPort))
	}

	if peer != "" {
		// The Go HTTP server sets RemoteAddr to "IP:port", this will not be a
		// file-path that would be interpreted with a sock family.
		attrs = append(attrs, c.NetConv.SockPeerAddr(peer))
		if peerPort > 0 {
			attrs = append(attrs, c.NetConv.SockPeerPort(peerPort))
		}
	}

	if useragent != "" {
		attrs = append(attrs, c.UserAgentOriginalKey.String(useragent))
	}

	if clientIP != "" {
		attrs = append(attrs, c.HTTPClientIPKey.String(clientIP))
	}

	if target != "" {
		attrs = append(attrs, c.HTTPTargetKey.String(target))
	}

	if protoName != "" && protoName != "http" {
		attrs = append(attrs, c.NetConv.NetProtocolName.String(protoName))
	}
	if protoVersion != "" {
		attrs = append(attrs, c.NetConv.NetProtocolVersion.String(protoVersion))
	}

	return attrs
}

// ServerRequestMetrics returns metric attributes for an HTTP request received
// by a server.
//
// The server must be the primary server name if it is known. For example this
// would be the ServerName directive
// (https://httpd.apache.org/docs/2.4/mod/core.html#servername) for an Apache
// server, and the server_name directive
// (http://nginx.org/en/docs/http/ngx_http_core_module.html#server_name) for an
// nginx server. More generically, the primary server name would be the host
// header value that matches the default virtual host of an HTTP server. It
// should include the host identifier and if a port is used to route to the
// server that port identifier should be included as an appropriate port
// suffix.
//
// If the primary server name is not known, server should be an empty string.
// The req Host will be used to determine the server instead.
//
// The following attributes are always returned: "http.method", "http.scheme",
// "net.host.name". The following attributes are returned if they related
// values are defined in req: "net.host.port".
func (c *httpConv) ServerRequestMetrics(server string, req *http.Request) []attribute.KeyValue {
	/* The following semantic conventions are returned if present:
	http.scheme             string
	http.route              string
	http.method             string
	http.status_code        int
	net.host.name           string
	net.host.port           int
	net.protocol.name       string Note: not set if the value is "http".
	net.protocol.version    string
	*/

	n := 3 // Method, scheme, and host name.
	var host string
	var p int
	if server == "" {
		host, p = splitHostPort(req.Host)
	} else {
		// Prioritize the primary server name.
		host, p = splitHostPort(server)
		if p < 0 {
			_, p = splitHostPort(req.Host)
		}
	}
	hostPort := requiredHTTPPort(req.TLS != nil, p)
	if hostPort > 0 {
		n++
	}
	protoName, protoVersion := netProtocol(req.Proto)
	if protoName != "" {
		n++
	}
	if protoVersion != "" {
		n++
	}

	attrs := make([]attribute.KeyValue, 0, n)

	attrs = append(attrs, c.methodMetric(req.Method))
	attrs = append(attrs, c.scheme(req.TLS != nil))
	attrs = append(attrs, c.NetConv.HostName(host))

	if hostPort > 0 {
		attrs = append(attrs, c.NetConv.HostPort(hostPort))
	}
	if protoName != "" {
		attrs = append(attrs, c.NetConv.NetProtocolName.String(protoName))
	}
	if protoVersion != "" {
		attrs = append(attrs, c.NetConv.NetProtocolVersion.String(protoVersion))
	}

	return attrs
}

func (c *httpConv) method(method string) attribute.KeyValue {
	if method == "" {
		return c.HTTPMethodKey.String(http.MethodGet)
	}
	return c.HTTPMethodKey.String(method)
}

func (c *httpConv) methodMetric(method string) attribute.KeyValue {
	method = strings.ToUpper(method)
	switch method {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace:
	default:
		method = "_OTHER"
	}
	return c.HTTPMethodKey.String(method)
}

func (c *httpConv) scheme(https bool) attribute.KeyValue { // nolint:revive
	if https {
		return c.HTTPSchemeHTTPS
	}
	return c.HTTPSchemeHTTP
}

func serverClientIP(xForwardedFor string) string {
	if idx := strings.Index(xForwardedFor, ","); idx >= 0 {
		xForwardedFor = xForwardedFor[:idx]
	}
	return xForwardedFor
}

func requiredHTTPPort(https bool, port int) int { // nolint:revive
	if https {
		if port > 0 && port != 443 {
			return port
		}
	} else {
		if port > 0 && port != 80 {
			return port
		}
	}
	return -1
}

// Return the request host and port from the first non-empty source.
func firstHostPort(source ...string) (host string, port int) {
	for _, hostport := range source {
		host, port = splitHostPort(hostport)
		if host != "" || port > 0 {
			break
		}
	}
	return
}

// ClientStatus returns a span status code and message for an HTTP status code
// value received by a client.
func (c *httpConv) ClientStatus(code int) (codes.Code, string) {
	if code < 100 || code >= 600 {
		return codes.Error, fmt.Sprintf("Invalid HTTP status code %d", code)
	}
	if code >= 400 {
		return codes.Error, ""
	}
	return codes.Unset, ""
}

// ServerStatus returns a span status code and message for an HTTP status code
// value returned by a server. Status codes in the 400-499 range are not
// returned as errors.
func (c *httpConv) ServerStatus(code int) (codes.Code, string) {
	if code < 100 || code >= 600 {
		return codes.Error, fmt.Sprintf("Invalid HTTP status code %d", code)
	}
	if code >= 500 {
		return codes.Error, ""
	}
	return codes.Unset, ""
}
