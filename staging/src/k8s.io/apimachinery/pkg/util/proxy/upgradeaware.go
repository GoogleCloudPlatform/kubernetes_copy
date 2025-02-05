/*
Copyright 2017 The Kubernetes Authors.

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

package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/httpstream"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/mxk/go-flowrate/flowrate"

	"k8s.io/klog/v2"
)

// UpgradeRequestRoundTripper provides an additional method to decorate a request
// with any authentication or other protocol level information prior to performing
// an upgrade on the server. Any response will be handled by the intercepting
// proxy.
type UpgradeRequestRoundTripper interface {
	http.RoundTripper
	// WrapRequest takes a valid HTTP request and returns a suitably altered version
	// of request with any HTTP level values required to complete the request half of
	// an upgrade on the server. It does not get a chance to see the response and
	// should bypass any request side logic that expects to see the response.
	WrapRequest(*http.Request) (*http.Request, error)
}

// UpgradeAwareHandler is a handler for proxy requests that may require an upgrade
type UpgradeAwareHandler struct {
	// UpgradeRequired will reject non-upgrade connections if true.
	UpgradeRequired bool
	// Location is the location of the upstream proxy. It is used as the location to Dial on the upstream server
	// for upgrade requests unless UseRequestLocationOnUpgrade is true.
	Location *url.URL
	// AppendLocationPath determines if the original path of the Location should be appended to the upstream proxy request path
	AppendLocationPath bool
	// Transport provides an optional round tripper to use to proxy. If nil, the default proxy transport is used
	Transport http.RoundTripper
	// UpgradeTransport, if specified, will be used as the backend transport when upgrade requests are provided.
	// This allows clients to disable HTTP/2.
	UpgradeTransport UpgradeRequestRoundTripper
	// WrapTransport indicates whether the provided Transport should be wrapped with default proxy transport behavior (URL rewriting, X-Forwarded-* header setting)
	WrapTransport bool
	// UseRequestLocation will use the incoming request URL when talking to the backend server.
	UseRequestLocation bool
	// UseLocationHost overrides the HTTP host header in requests to the backend server to use the Host from Location.
	// This will override the req.Host field of a request, while UseRequestLocation will override the req.URL field
	// of a request. The req.URL.Host specifies the server to connect to, while the req.Host field
	// specifies the Host header value to send in the HTTP request. If this is false, the incoming req.Host header will
	// just be forwarded to the backend server.
	UseLocationHost bool
	// FlushInterval controls how often the standard HTTP proxy will flush content from the upstream.
	FlushInterval time.Duration
	// MaxBytesPerSec controls the maximum rate for an upstream connection. No rate is imposed if the value is zero.
	MaxBytesPerSec int64
	// Responder is passed errors that occur while setting up proxying.
	Responder ErrorResponder
	// Reject to forward redirect response
	RejectForwardingRedirects bool
}

const defaultFlushInterval = 200 * time.Millisecond

// ErrorResponder abstracts error reporting to the proxy handler to remove the need to hardcode a particular
// error format.
type ErrorResponder interface {
	Error(w http.ResponseWriter, req *http.Request, err error)
}

// SimpleErrorResponder is the legacy implementation of ErrorResponder for callers that only
// service a single request/response per proxy.
type SimpleErrorResponder interface {
	Error(err error)
}

func NewErrorResponder(r SimpleErrorResponder) ErrorResponder {
	return simpleResponder{r}
}

type simpleResponder struct {
	responder SimpleErrorResponder
}

func (r simpleResponder) Error(w http.ResponseWriter, req *http.Request, err error) {
	r.responder.Error(err)
}

// upgradeRequestRoundTripper implements proxy.UpgradeRequestRoundTripper.
type upgradeRequestRoundTripper struct {
	http.RoundTripper
	upgrader http.RoundTripper
}

var (
	_ UpgradeRequestRoundTripper  = &upgradeRequestRoundTripper{}
	_ utilnet.RoundTripperWrapper = &upgradeRequestRoundTripper{}
)

// WrappedRoundTripper returns the round tripper that a caller would use.
func (rt *upgradeRequestRoundTripper) WrappedRoundTripper() http.RoundTripper {
	return rt.RoundTripper
}

// WriteToRequest calls the nested upgrader and then copies the returned request
// fields onto the passed request.
func (rt *upgradeRequestRoundTripper) WrapRequest(req *http.Request) (*http.Request, error) {
	resp, err := rt.upgrader.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	return resp.Request, nil
}

// onewayRoundTripper captures the provided request - which is assumed to have
// been modified by other round trippers - and then returns a fake response.
type onewayRoundTripper struct{}

// RoundTrip returns a simple 200 OK response that captures the provided request.
func (onewayRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(&bytes.Buffer{}),
		Request:    req,
	}, nil
}

// MirrorRequest is a round tripper that can be called to get back the calling request as
// the core round tripper in a chain.
var MirrorRequest http.RoundTripper = onewayRoundTripper{}

// NewUpgradeRequestRoundTripper takes two round trippers - one for the underlying TCP connection, and
// one that is able to write headers to an HTTP request. The request rt is used to set the request headers
// and that is written to the underlying connection rt.
func NewUpgradeRequestRoundTripper(connection, request http.RoundTripper) UpgradeRequestRoundTripper {
	return &upgradeRequestRoundTripper{
		RoundTripper: connection,
		upgrader:     request,
	}
}

// normalizeLocation returns the result of parsing the full URL, with scheme set to http if missing
func normalizeLocation(location *url.URL) *url.URL {
	normalized, _ := url.Parse(location.String())
	if len(normalized.Scheme) == 0 {
		normalized.Scheme = "http"
	}
	return normalized
}

// NewUpgradeAwareHandler creates a new proxy handler with a default flush interval. Responder is required for returning
// errors to the caller.
func NewUpgradeAwareHandler(location *url.URL, transport http.RoundTripper, wrapTransport, upgradeRequired bool, responder ErrorResponder) *UpgradeAwareHandler {
	return &UpgradeAwareHandler{
		Location:        normalizeLocation(location),
		Transport:       transport,
		WrapTransport:   wrapTransport,
		UpgradeRequired: upgradeRequired,
		FlushInterval:   defaultFlushInterval,
		Responder:       responder,
	}
}

func proxyRedirectsforRootPath(path string, w http.ResponseWriter, req *http.Request) bool {
	redirect := false
	method := req.Method

	// From pkg/genericapiserver/endpoints/handlers/proxy.go#ServeHTTP:
	// Redirect requests with an empty path to a location that ends with a '/'
	// This is essentially a hack for https://issue.k8s.io/4958.
	// Note: Keep this code after tryUpgrade to not break that flow.
	if len(path) == 0 && (method == http.MethodGet || method == http.MethodHead) {
		var queryPart string
		if len(req.URL.RawQuery) > 0 {
			queryPart = "?" + req.URL.RawQuery
		}
		w.Header().Set("Location", req.URL.Path+"/"+queryPart)
		w.WriteHeader(http.StatusMovedPermanently)
		redirect = true
	}
	return redirect
}

// ServeHTTP handles the proxy request
func (h *UpgradeAwareHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if h.tryUpgrade(w, req) {
		return
	}
	if h.UpgradeRequired {
		h.Responder.Error(w, req, errors.NewBadRequest("Upgrade request required"))
		return
	}

	loc := *h.Location
	loc.RawQuery = req.URL.RawQuery

	// If original request URL ended in '/', append a '/' at the end of the
	// of the proxy URL
	if !strings.HasSuffix(loc.Path, "/") && strings.HasSuffix(req.URL.Path, "/") {
		loc.Path += "/"
	}

	proxyRedirect := proxyRedirectsforRootPath(loc.Path, w, req)
	if proxyRedirect {
		return
	}

	if h.Transport == nil || h.WrapTransport {
		h.Transport = h.defaultProxyTransport(req.URL, h.Transport)
	}

	// WithContext creates a shallow clone of the request with the same context.
	newReq := req.WithContext(req.Context())
	newReq.Header = utilnet.CloneHeader(req.Header)
	if !h.UseRequestLocation {
		newReq.URL = &loc
	}
	if h.UseLocationHost {
		// exchanging req.Host with the backend location is necessary for backends that act on the HTTP host header (e.g. API gateways),
		// because req.Host has preference over req.URL.Host in filling this header field
		newReq.Host = h.Location.Host
	}

	// create the target location to use for the reverse proxy
	reverseProxyLocation := &url.URL{Scheme: h.Location.Scheme, Host: h.Location.Host}
	if h.AppendLocationPath {
		reverseProxyLocation.Path = h.Location.Path
	}

	proxy := httputil.NewSingleHostReverseProxy(reverseProxyLocation)
	proxy.Transport = h.Transport
	proxy.FlushInterval = h.FlushInterval
	proxy.ErrorLog = log.New(noSuppressPanicError{}, "", log.LstdFlags)
	if h.RejectForwardingRedirects {
		oldModifyResponse := proxy.ModifyResponse
		proxy.ModifyResponse = func(response *http.Response) error {
			code := response.StatusCode
			if code >= 300 && code <= 399 && len(response.Header.Get("Location")) > 0 {
				// close the original response
				response.Body.Close()
				msg := "the backend attempted to redirect this request, which is not permitted"
				// replace the response
				*response = http.Response{
					StatusCode:    http.StatusBadGateway,
					Status:        fmt.Sprintf("%d %s", response.StatusCode, http.StatusText(response.StatusCode)),
					Body:          io.NopCloser(strings.NewReader(msg)),
					ContentLength: int64(len(msg)),
				}
			} else {
				if oldModifyResponse != nil {
					if err := oldModifyResponse(response); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	if h.Responder != nil {
		// if an optional error interceptor/responder was provided wire it
		// the custom responder might be used for providing a unified error reporting
		// or supporting retry mechanisms by not sending non-fatal errors to the clients
		proxy.ErrorHandler = h.Responder.Error
	}
	proxy.ServeHTTP(w, newReq)
}

type noSuppressPanicError struct{}

func (noSuppressPanicError) Write(p []byte) (n int, err error) {
	// skip "suppressing panic for copyResponse error in test; copy error" error message
	// that ends up in CI tests on each kube-apiserver termination as noise and
	// everybody thinks this is fatal.
	if strings.Contains(string(p), "suppressing panic") {
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// tryUpgrade returns true if the request was handled.
func (h *UpgradeAwareHandler) tryUpgrade(w http.ResponseWriter, req *http.Request) bool {
	logger := klog.FromContext(req.Context())
	if !httpstream.IsUpgradeRequest(req) {
		logger.V(6).Info("Request was not an upgrade")
		return false
	}

	var (
		backendConn net.Conn
		rawResponse []byte
		err         error
	)

	location := *h.Location
	if h.UseRequestLocation {
		location = *req.URL
		location.Scheme = h.Location.Scheme
		location.Host = h.Location.Host
		if h.AppendLocationPath {
			location.Path = singleJoiningSlash(h.Location.Path, location.Path)
		}
	}

	clone := utilnet.CloneRequest(req)
	// Only append X-Forwarded-For in the upgrade path, since httputil.NewSingleHostReverseProxy
	// handles this in the non-upgrade path.
	utilnet.AppendForwardedForHeader(clone)
	logger.V(6).Info("Connecting to backend proxy (direct dial)", "location", &location, "header", clone.Header)
	if h.UseLocationHost {
		clone.Host = h.Location.Host
	}
	clone.URL = &location
	logger.V(6).Info("UpgradeAwareProxy: dialing for SPDY upgrade with headers", "header", clone.Header)
	backendConn, err = h.DialForUpgrade(clone)
	if err != nil {
		logger.V(6).Info("Proxy connection error", "err", err)
		h.Responder.Error(w, req, err)
		return true
	}
	defer backendConn.Close()

	// determine the http response code from the backend by reading from rawResponse+backendConn
	backendHTTPResponse, headerBytes, err := getResponse(io.MultiReader(bytes.NewReader(rawResponse), backendConn))
	if err != nil {
		logger.V(6).Info("Proxy connection error", "err", err)
		h.Responder.Error(w, req, err)
		return true
	}
	if len(headerBytes) > len(rawResponse) {
		// we read beyond the bytes stored in rawResponse, update rawResponse to the full set of bytes read from the backend
		rawResponse = headerBytes
	}

	// If the backend did not upgrade the request, return an error to the client. If the response was
	// an error, the error is forwarded directly after the connection is hijacked. Otherwise, just
	// return a generic error here.
	if backendHTTPResponse.StatusCode != http.StatusSwitchingProtocols && backendHTTPResponse.StatusCode < 400 {
		err := fmt.Errorf("invalid upgrade response: status code %d", backendHTTPResponse.StatusCode)
		logger.Error(err, "Proxy upgrade error")
		h.Responder.Error(w, req, err)
		return true
	}

	// Once the connection is hijacked, the ErrorResponder will no longer work, so
	// hijacking should be the last step in the upgrade.
	requestHijacker, ok := w.(http.Hijacker)
	if !ok {
		logger.Error(nil, "Unable to hijack response writer", "type", fmt.Sprintf("%T", w))
		h.Responder.Error(w, req, fmt.Errorf("request connection cannot be hijacked: %T", w))
		return true
	}
	requestHijackedConn, _, err := requestHijacker.Hijack()
	if err != nil {
		logger.Error(err, "Unable to hijack response")
		h.Responder.Error(w, req, fmt.Errorf("error hijacking connection: %v", err))
		return true
	}
	defer requestHijackedConn.Close()

	if backendHTTPResponse.StatusCode != http.StatusSwitchingProtocols {
		// If the backend did not upgrade the request, echo the response from the backend to the client and return, closing the connection.
		logger.V(6).Info("Proxy upgrade error", "statusCode", backendHTTPResponse.StatusCode)
		// set read/write deadlines
		deadline := time.Now().Add(10 * time.Second)
		backendConn.SetReadDeadline(deadline)
		requestHijackedConn.SetWriteDeadline(deadline)
		// write the response to the client
		err := backendHTTPResponse.Write(requestHijackedConn)
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error(err, "Error proxying data from backend to client")
		}
		// Indicate we handled the request
		return true
	}

	// Forward raw response bytes back to client.
	if len(rawResponse) > 0 {
		logger.V(6).Info("Writing to hijacked connection", "length", len(rawResponse))
		if _, err = requestHijackedConn.Write(rawResponse); err != nil {
			utilruntime.HandleErrorWithLogger(logger, err, "Error proxying response from backend to client")
		}
	}

	// Proxy the connection. This is bidirectional, so we need a goroutine
	// to copy in each direction. Once one side of the connection exits, we
	// exit the function which performs cleanup and in the process closes
	// the other half of the connection in the defer.
	writerComplete := make(chan struct{})
	readerComplete := make(chan struct{})

	go func() {
		var writer io.WriteCloser
		if h.MaxBytesPerSec > 0 {
			writer = flowrate.NewWriter(backendConn, h.MaxBytesPerSec)
		} else {
			writer = backendConn
		}
		_, err := io.Copy(writer, requestHijackedConn)
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error(err, "Error proxying data from client to backend")
		}
		close(writerComplete)
	}()

	go func() {
		var reader io.ReadCloser
		if h.MaxBytesPerSec > 0 {
			reader = flowrate.NewReader(backendConn, h.MaxBytesPerSec)
		} else {
			reader = backendConn
		}
		_, err := io.Copy(requestHijackedConn, reader)
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error(err, "Error proxying data from backend to client")
		}
		close(readerComplete)
	}()

	// Wait for one half the connection to exit. Once it does the defer will
	// clean up the other half of the connection.
	select {
	case <-writerComplete:
	case <-readerComplete:
	}
	logger.V(6).Info("Disconnecting from backend proxy", "location", &location, "header", clone.Header)

	return true
}

// FIXME: Taken from net/http/httputil/reverseproxy.go as singleJoiningSlash is not exported to be re-used.
// See-also: https://github.com/golang/go/issues/44290
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func (h *UpgradeAwareHandler) DialForUpgrade(req *http.Request) (net.Conn, error) {
	if h.UpgradeTransport == nil {
		return dial(req, h.Transport)
	}
	updatedReq, err := h.UpgradeTransport.WrapRequest(req)
	if err != nil {
		return nil, err
	}
	return dial(updatedReq, h.UpgradeTransport)
}

// getResponseCode reads a http response from the given reader, returns the response,
// the bytes read from the reader, and any error encountered
func getResponse(r io.Reader) (*http.Response, []byte, error) {
	rawResponse := bytes.NewBuffer(make([]byte, 0, 256))
	// Save the bytes read while reading the response headers into the rawResponse buffer
	resp, err := http.ReadResponse(bufio.NewReader(io.TeeReader(r, rawResponse)), nil)
	if err != nil {
		return nil, nil, err
	}
	// return the http response and the raw bytes consumed from the reader in the process
	return resp, rawResponse.Bytes(), nil
}

// dial dials the backend at req.URL and writes req to it.
func dial(req *http.Request, transport http.RoundTripper) (net.Conn, error) {
	conn, err := DialURL(req.Context(), req.URL, transport)
	if err != nil {
		return nil, fmt.Errorf("error dialing backend: %v", err)
	}

	if err = req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("error sending request: %v", err)
	}

	return conn, err
}

func (h *UpgradeAwareHandler) defaultProxyTransport(url *url.URL, internalTransport http.RoundTripper) http.RoundTripper {
	scheme := url.Scheme
	host := url.Host
	suffix := h.Location.Path
	if strings.HasSuffix(url.Path, "/") && !strings.HasSuffix(suffix, "/") {
		suffix += "/"
	}
	pathPrepend := strings.TrimSuffix(url.Path, suffix)
	rewritingTransport := &Transport{
		Scheme:       scheme,
		Host:         host,
		PathPrepend:  pathPrepend,
		RoundTripper: internalTransport,
	}
	return &corsRemovingTransport{
		RoundTripper: rewritingTransport,
	}
}

// corsRemovingTransport is a wrapper for an internal transport. It removes CORS headers
// from the internal response.
// Implements pkg/util/net.RoundTripperWrapper
type corsRemovingTransport struct {
	http.RoundTripper
}

var _ = utilnet.RoundTripperWrapper(&corsRemovingTransport{})

func (rt *corsRemovingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	removeCORSHeaders(resp)
	return resp, nil
}

func (rt *corsRemovingTransport) WrappedRoundTripper() http.RoundTripper {
	return rt.RoundTripper
}

// removeCORSHeaders strip CORS headers sent from the backend
// This should be called on all responses before returning
func removeCORSHeaders(resp *http.Response) {
	resp.Header.Del("Access-Control-Allow-Credentials")
	resp.Header.Del("Access-Control-Allow-Headers")
	resp.Header.Del("Access-Control-Allow-Methods")
	resp.Header.Del("Access-Control-Allow-Origin")
}
