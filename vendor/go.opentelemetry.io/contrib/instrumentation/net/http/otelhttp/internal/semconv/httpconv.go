// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package semconv // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/semconv"

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	semconvNew "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type newHTTPServer struct{}

// TraceRequest returns trace attributes for an HTTP request received by a
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
func (n newHTTPServer) RequestTraceAttrs(server string, req *http.Request) []attribute.KeyValue {
	count := 3 // ServerAddress, Method, Scheme

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
		count++
	}

	method, methodOriginal := n.method(req.Method)
	if methodOriginal != (attribute.KeyValue{}) {
		count++
	}

	scheme := n.scheme(req.TLS != nil)

	if peer, peerPort := splitHostPort(req.RemoteAddr); peer != "" {
		// The Go HTTP server sets RemoteAddr to "IP:port", this will not be a
		// file-path that would be interpreted with a sock family.
		count++
		if peerPort > 0 {
			count++
		}
	}

	useragent := req.UserAgent()
	if useragent != "" {
		count++
	}

	clientIP := serverClientIP(req.Header.Get("X-Forwarded-For"))
	if clientIP != "" {
		count++
	}

	if req.URL != nil && req.URL.Path != "" {
		count++
	}

	protoName, protoVersion := netProtocol(req.Proto)
	if protoName != "" && protoName != "http" {
		count++
	}
	if protoVersion != "" {
		count++
	}

	attrs := make([]attribute.KeyValue, 0, count)
	attrs = append(attrs,
		semconvNew.ServerAddress(host),
		method,
		scheme,
	)

	if hostPort > 0 {
		attrs = append(attrs, semconvNew.ServerPort(hostPort))
	}
	if methodOriginal != (attribute.KeyValue{}) {
		attrs = append(attrs, methodOriginal)
	}

	if peer, peerPort := splitHostPort(req.RemoteAddr); peer != "" {
		// The Go HTTP server sets RemoteAddr to "IP:port", this will not be a
		// file-path that would be interpreted with a sock family.
		attrs = append(attrs, semconvNew.NetworkPeerAddress(peer))
		if peerPort > 0 {
			attrs = append(attrs, semconvNew.NetworkPeerPort(peerPort))
		}
	}

	if useragent := req.UserAgent(); useragent != "" {
		attrs = append(attrs, semconvNew.UserAgentOriginal(useragent))
	}

	if clientIP != "" {
		attrs = append(attrs, semconvNew.ClientAddress(clientIP))
	}

	if req.URL != nil && req.URL.Path != "" {
		attrs = append(attrs, semconvNew.URLPath(req.URL.Path))
	}

	if protoName != "" && protoName != "http" {
		attrs = append(attrs, semconvNew.NetworkProtocolName(protoName))
	}
	if protoVersion != "" {
		attrs = append(attrs, semconvNew.NetworkProtocolVersion(protoVersion))
	}

	return attrs
}

func (n newHTTPServer) method(method string) (attribute.KeyValue, attribute.KeyValue) {
	if method == "" {
		return semconvNew.HTTPRequestMethodGet, attribute.KeyValue{}
	}
	if attr, ok := methodLookup[method]; ok {
		return attr, attribute.KeyValue{}
	}

	orig := semconvNew.HTTPRequestMethodOriginal(method)
	if attr, ok := methodLookup[strings.ToUpper(method)]; ok {
		return attr, orig
	}
	return semconvNew.HTTPRequestMethodGet, orig
}

func (n newHTTPServer) scheme(https bool) attribute.KeyValue { // nolint:revive
	if https {
		return semconvNew.URLScheme("https")
	}
	return semconvNew.URLScheme("http")
}

// TraceResponse returns trace attributes for telemetry from an HTTP response.
//
// If any of the fields in the ResponseTelemetry are not set the attribute will be omitted.
func (n newHTTPServer) ResponseTraceAttrs(resp ResponseTelemetry) []attribute.KeyValue {
	var count int

	if resp.ReadBytes > 0 {
		count++
	}
	if resp.WriteBytes > 0 {
		count++
	}
	if resp.StatusCode > 0 {
		count++
	}

	attributes := make([]attribute.KeyValue, 0, count)

	if resp.ReadBytes > 0 {
		attributes = append(attributes,
			semconvNew.HTTPRequestBodySize(int(resp.ReadBytes)),
		)
	}
	if resp.WriteBytes > 0 {
		attributes = append(attributes,
			semconvNew.HTTPResponseBodySize(int(resp.WriteBytes)),
		)
	}
	if resp.StatusCode > 0 {
		attributes = append(attributes,
			semconvNew.HTTPResponseStatusCode(resp.StatusCode),
		)
	}

	return attributes
}

// Route returns the attribute for the route.
func (n newHTTPServer) Route(route string) attribute.KeyValue {
	return semconvNew.HTTPRoute(route)
}

type newHTTPClient struct{}

// RequestTraceAttrs returns trace attributes for an HTTP request made by a client.
func (n newHTTPClient) RequestTraceAttrs(req *http.Request) []attribute.KeyValue {
	/*
	   below attributes are returned:
	   - http.request.method
	   - http.request.method.original
	   - url.full
	   - server.address
	   - server.port
	   - network.protocol.name
	   - network.protocol.version
	*/
	numOfAttributes := 3 // URL, server address, proto, and method.

	var urlHost string
	if req.URL != nil {
		urlHost = req.URL.Host
	}
	var requestHost string
	var requestPort int
	for _, hostport := range []string{urlHost, req.Header.Get("Host")} {
		requestHost, requestPort = splitHostPort(hostport)
		if requestHost != "" || requestPort > 0 {
			break
		}
	}

	eligiblePort := requiredHTTPPort(req.URL != nil && req.URL.Scheme == "https", requestPort)
	if eligiblePort > 0 {
		numOfAttributes++
	}
	useragent := req.UserAgent()
	if useragent != "" {
		numOfAttributes++
	}

	protoName, protoVersion := netProtocol(req.Proto)
	if protoName != "" && protoName != "http" {
		numOfAttributes++
	}
	if protoVersion != "" {
		numOfAttributes++
	}

	method, originalMethod := n.method(req.Method)
	if originalMethod != (attribute.KeyValue{}) {
		numOfAttributes++
	}

	attrs := make([]attribute.KeyValue, 0, numOfAttributes)

	attrs = append(attrs, method)
	if originalMethod != (attribute.KeyValue{}) {
		attrs = append(attrs, originalMethod)
	}

	var u string
	if req.URL != nil {
		// Remove any username/password info that may be in the URL.
		userinfo := req.URL.User
		req.URL.User = nil
		u = req.URL.String()
		// Restore any username/password info that was removed.
		req.URL.User = userinfo
	}
	attrs = append(attrs, semconvNew.URLFull(u))

	attrs = append(attrs, semconvNew.ServerAddress(requestHost))
	if eligiblePort > 0 {
		attrs = append(attrs, semconvNew.ServerPort(eligiblePort))
	}

	if protoName != "" && protoName != "http" {
		attrs = append(attrs, semconvNew.NetworkProtocolName(protoName))
	}
	if protoVersion != "" {
		attrs = append(attrs, semconvNew.NetworkProtocolVersion(protoVersion))
	}

	return attrs
}

// ResponseTraceAttrs returns trace attributes for an HTTP response made by a client.
func (n newHTTPClient) ResponseTraceAttrs(resp *http.Response) []attribute.KeyValue {
	/*
	   below attributes are returned:
	   - http.response.status_code
	   - error.type
	*/
	var count int
	if resp.StatusCode > 0 {
		count++
	}

	if isErrorStatusCode(resp.StatusCode) {
		count++
	}

	attrs := make([]attribute.KeyValue, 0, count)
	if resp.StatusCode > 0 {
		attrs = append(attrs, semconvNew.HTTPResponseStatusCode(resp.StatusCode))
	}

	if isErrorStatusCode(resp.StatusCode) {
		errorType := strconv.Itoa(resp.StatusCode)
		attrs = append(attrs, semconvNew.ErrorTypeKey.String(errorType))
	}
	return attrs
}

func (n newHTTPClient) ErrorType(err error) attribute.KeyValue {
	t := reflect.TypeOf(err)
	var value string
	if t.PkgPath() == "" && t.Name() == "" {
		// Likely a builtin type.
		value = t.String()
	} else {
		value = fmt.Sprintf("%s.%s", t.PkgPath(), t.Name())
	}

	if value == "" {
		return semconvNew.ErrorTypeOther
	}

	return semconvNew.ErrorTypeKey.String(value)
}

func (n newHTTPClient) method(method string) (attribute.KeyValue, attribute.KeyValue) {
	if method == "" {
		return semconvNew.HTTPRequestMethodGet, attribute.KeyValue{}
	}
	if attr, ok := methodLookup[method]; ok {
		return attr, attribute.KeyValue{}
	}

	orig := semconvNew.HTTPRequestMethodOriginal(method)
	if attr, ok := methodLookup[strings.ToUpper(method)]; ok {
		return attr, orig
	}
	return semconvNew.HTTPRequestMethodGet, orig
}

func isErrorStatusCode(code int) bool {
	return code >= 400 || code < 100
}
