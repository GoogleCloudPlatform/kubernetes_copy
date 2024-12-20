/*
Copyright 2024 The Kubernetes Authors.

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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gwebsocket "github.com/gorilla/websocket"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	constants "k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/apiserver/pkg/util/proxy/metrics"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/klog/v2"
)

// TunnelingHandler is a handler which tunnels SPDY through WebSockets.
type TunnelingHandler struct {
	// Used to communicate between upstream SPDY and downstream tunnel.
	upgradeHandler http.Handler
}

// NewTunnelingHandler is used to create the tunnel between an upstream
// SPDY connection and a downstream tunneling connection through the stored
// UpgradeAwareProxy.
func NewTunnelingHandler(upgradeHandler http.Handler) *TunnelingHandler {
	return &TunnelingHandler{upgradeHandler: upgradeHandler}
}

// ServeHTTP uses the upgradeHandler to tunnel between a downstream tunneling
// connection and an upstream SPDY connection. The tunneling connection is
// a wrapped WebSockets connection which communicates SPDY framed data. In the
// case the upstream upgrade fails, we delegate communication to the passed
// in "w" ResponseWriter.
func (h *TunnelingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	logger := klog.FromContext(req.Context())
	logger.V(4).Info("TunnelingHandler ServeHTTP")

	spdyProtocols := spdyProtocolsFromWebsocketProtocols(req)
	if len(spdyProtocols) == 0 {
		metrics.IncStreamTunnelRequest(req.Context(), strconv.Itoa(http.StatusBadRequest))
		http.Error(w, "unable to upgrade: no tunneling spdy protocols provided", http.StatusBadRequest)
		return
	}

	spdyRequest := createSPDYRequest(req, spdyProtocols...)

	// The fields "w" and "conn" are mutually exclusive. Either a successful upgrade occurs
	// and the "conn" is hijacked and used in the subsequent upgradeHandler, or
	// the upgrade failed, and "w" is the delegate used for the non-upgrade response.
	writer := &tunnelingResponseWriter{
		logger: logger,
		// "w" is used in the non-upgrade error cases called in the upgradeHandler.
		w: w,
		// "conn" is returned in the successful upgrade case when hijacked in the upgradeHandler.
		conn: &headerInterceptingConn{
			logger: logger,
			initializableConn: &tunnelingWebsocketUpgraderConn{
				w:   w,
				req: req,
			},
		},
	}

	logger.V(4).Info("Tunnel spdy through websockets using the UpgradeAwareProxy")
	h.upgradeHandler.ServeHTTP(writer, spdyRequest)
}

// createSPDYRequest modifies the passed request to remove
// WebSockets headers and add SPDY upgrade information, including
// spdy protocols acceptable to the client.
func createSPDYRequest(req *http.Request, spdyProtocols ...string) *http.Request {
	clone := utilnet.CloneRequest(req)
	// Clean up the websocket headers from the http request.
	clone.Header.Del(wsstream.WebSocketProtocolHeader)
	clone.Header.Del("Sec-Websocket-Key")
	clone.Header.Del("Sec-Websocket-Version")
	clone.Header.Del(httpstream.HeaderUpgrade)
	// Update the http request for an upstream SPDY upgrade.
	clone.Method = "POST"
	clone.Body = nil // Remove the request body which is unused.
	clone.Header.Set(httpstream.HeaderUpgrade, spdy.HeaderSpdy31)
	clone.Header.Del(httpstream.HeaderProtocolVersion)
	for i := range spdyProtocols {
		clone.Header.Add(httpstream.HeaderProtocolVersion, spdyProtocols[i])
	}
	return clone
}

// spdyProtocolsFromWebsocketProtocols returns a list of spdy protocols by filtering
// to Kubernetes websocket subprotocols prefixed with "SPDY/3.1+", then removing the prefix
func spdyProtocolsFromWebsocketProtocols(req *http.Request) []string {
	var spdyProtocols []string
	for _, protocol := range gwebsocket.Subprotocols(req) {
		if strings.HasPrefix(protocol, constants.WebsocketsSPDYTunnelingPrefix) && strings.HasSuffix(protocol, constants.KubernetesSuffix) {
			spdyProtocols = append(spdyProtocols, strings.TrimPrefix(protocol, constants.WebsocketsSPDYTunnelingPrefix))
		}
	}
	return spdyProtocols
}

var _ http.ResponseWriter = &tunnelingResponseWriter{}
var _ http.Hijacker = &tunnelingResponseWriter{}

// tunnelingResponseWriter implements the http.ResponseWriter and http.Hijacker interfaces.
// Only non-upgrade responses can be written using WriteHeader() and Write().
// Once Write or WriteHeader is called, Hijack returns an error.
// Once Hijack is called, Write, WriteHeader, and Hijack return errors.
type tunnelingResponseWriter struct {
	logger klog.Logger
	// w is used to delegate Header(), WriteHeader(), and Write() calls
	w http.ResponseWriter
	// conn is returned from Hijack()
	conn net.Conn
	// mu guards writes
	mu sync.Mutex
	// wrote tracks whether WriteHeader or Write has been called
	written bool
	// hijacked tracks whether Hijack has been called
	hijacked bool
}

// Hijack returns a delegate "net.Conn".
// An error is returned if Write(), WriteHeader(), or Hijack() was previously called.
// The returned bufio.ReadWriter is always nil.
func (w *tunnelingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.written {
		w.logger.Error(nil, "Hijack called after write")
		return nil, nil, errors.New("connection has already been written to")
	}
	if w.hijacked {
		w.logger.Error(nil, "Hijack called after hijack")
		return nil, nil, errors.New("connection has already been hijacked")
	}
	w.hijacked = true
	w.logger.V(6).Info("Hijack returning websocket tunneling net.Conn")
	return w.conn, nil, nil
}

// Header is delegated to the stored "http.ResponseWriter".
func (w *tunnelingResponseWriter) Header() http.Header {
	return w.w.Header()
}

// Write is delegated to the stored "http.ResponseWriter".
func (w *tunnelingResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hijacked {
		w.logger.Error(nil, "Write called after hijack")
		return 0, http.ErrHijacked
	}
	w.written = true
	return w.w.Write(p)
}

// WriteHeader is delegated to the stored "http.ResponseWriter".
func (w *tunnelingResponseWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.written {
		w.logger.Error(nil, "WriteHeader called after write")
		return
	}
	if w.hijacked {
		w.logger.Error(nil, "WriteHeader called after hijack")
		return
	}
	w.written = true

	if statusCode == http.StatusSwitchingProtocols {
		// 101 upgrade responses must come via the hijacked connection, not WriteHeader
		w.logger.Error(nil, "WriteHeader called with 101 upgrade")
		http.Error(w.w, "unexpected upgrade", http.StatusInternalServerError)
		return
	}

	// pass through non-upgrade responses we don't need to translate
	w.w.WriteHeader(statusCode)
}

// headerInterceptingConn wraps the tunneling "net.Conn" to drain the
// HTTP response status/headers from the upstream SPDY connection, then use
// that to decide how to initialize the delegate connection for writes.
type headerInterceptingConn struct {
	logger klog.Logger
	// initializableConn is delegated to for all net.Conn methods.
	// initializableConn.Write() is not called until response headers have been read
	// and initializableConn#InitializeWrite() has been called with the result.
	initializableConn

	lock          sync.Mutex
	headerBuffer  []byte
	initialized   bool
	initializeErr error
}

// initializableConn is a connection that will be initialized before any calls to Write are made
type initializableConn interface {
	net.Conn
	// InitializeWrite is called when the backend response headers have been read.
	// backendResponse contains the parsed headers.
	// backendResponseBytes are the raw bytes the headers were parsed from.
	InitializeWrite(backendResponse *http.Response, backendResponseBytes []byte) error
}

const maxHeaderBytes = 1 << 20

// token for normal header / body separation (\r\n\r\n, but go tolerates the leading \r being absent)
var lfCRLF = []byte("\n\r\n")

// token for header / body separation without \r (which go tolerates)
var lfLF = []byte("\n\n")

// Write intercepts to initially swallow the HTTP response, then
// delegate to the tunneling "net.Conn" once the response has been
// seen and processed.
func (h *headerInterceptingConn) Write(b []byte) (int, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	if h.initializeErr != nil {
		return 0, h.initializeErr
	}
	if h.initialized {
		return h.initializableConn.Write(b)
	}

	// Guard against excessive buffering
	if len(h.headerBuffer)+len(b) > maxHeaderBytes {
		return 0, fmt.Errorf("header size limit exceeded")
	}

	// Accumulate into headerBuffer
	h.headerBuffer = append(h.headerBuffer, b...)

	// Attempt to parse http response headers
	var headerBytes, bodyBytes []byte
	if i := bytes.Index(h.headerBuffer, lfCRLF); i != -1 {
		// headers terminated with \n\r\n
		headerBytes = h.headerBuffer[0 : i+len(lfCRLF)]
		bodyBytes = h.headerBuffer[i+len(lfCRLF):]
	} else if i := bytes.Index(h.headerBuffer, lfLF); i != -1 {
		// headers terminated with \n\n (which go tolerates)
		headerBytes = h.headerBuffer[0 : i+len(lfLF)]
		bodyBytes = h.headerBuffer[i+len(lfLF):]
	} else {
		// don't yet have a complete set of headers yet
		return len(b), nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(headerBytes)), nil)
	if err != nil {
		h.logger.Error(err, "Invalid headers")
		h.initializeErr = err
		return len(b), err
	}
	resp.Body.Close() //nolint:errcheck

	h.headerBuffer = nil
	h.initialized = true
	h.initializeErr = h.initializableConn.InitializeWrite(resp, headerBytes)
	if h.initializeErr != nil {
		return len(b), h.initializeErr
	}
	if len(bodyBytes) > 0 {
		_, err = h.initializableConn.Write(bodyBytes)
	}
	return len(b), err
}

type tunnelingWebsocketUpgraderConn struct {
	// req is the websocket request, used for upgrading
	req *http.Request
	// w is the websocket writer, used for upgrading and writing error responses
	w http.ResponseWriter

	// lock guards conn and err
	lock sync.RWMutex
	// if conn is non-nil, InitializeWrite succeeded
	conn net.Conn
	// if err is non-nil, InitializeWrite failed or Close was called before InitializeWrite
	err error
}

func (u *tunnelingWebsocketUpgraderConn) InitializeWrite(backendResponse *http.Response, backendResponseBytes []byte) (err error) {
	// make sure we close a connection we open in error cases
	var conn net.Conn
	defer func() {
		if err != nil && conn != nil {
			conn.Close() //nolint:errcheck
		}
	}()

	u.lock.Lock()
	defer u.lock.Unlock()
	if u.conn != nil {
		return fmt.Errorf("InitializeWrite already called")
	}
	if u.err != nil {
		return u.err
	}

	logger := klog.FromContext(u.req.Context())
	if backendResponse.StatusCode == http.StatusSwitchingProtocols {
		connectionHeader := strings.ToLower(backendResponse.Header.Get(httpstream.HeaderConnection))
		upgradeHeader := strings.ToLower(backendResponse.Header.Get(httpstream.HeaderUpgrade))
		if !strings.Contains(connectionHeader, strings.ToLower(httpstream.HeaderUpgrade)) || !strings.Contains(upgradeHeader, strings.ToLower(spdy.HeaderSpdy31)) {
			logger.Error(nil, "Unable to upgrade: missing upgrade headers in response", "headers", backendResponse.Header)
			u.err = fmt.Errorf("unable to upgrade: missing upgrade headers in response")
			metrics.IncStreamTunnelRequest(context.Background(), strconv.Itoa(http.StatusInternalServerError))
			http.Error(u.w, u.err.Error(), http.StatusInternalServerError)
			return u.err
		}

		// Translate the server's chosen SPDY protocol into the tunneled websocket protocol for the handshake
		var serverWebsocketProtocols []string
		if backendSPDYProtocol := strings.TrimSpace(backendResponse.Header.Get(httpstream.HeaderProtocolVersion)); backendSPDYProtocol != "" {
			serverWebsocketProtocols = []string{constants.WebsocketsSPDYTunnelingPrefix + backendSPDYProtocol}
		} else {
			serverWebsocketProtocols = []string{}
		}

		// Try to upgrade the websocket connection.
		// Beyond this point, we don't need to write errors to the response.
		var upgrader = gwebsocket.Upgrader{
			CheckOrigin:  func(r *http.Request) bool { return true },
			Subprotocols: serverWebsocketProtocols,
		}
		conn, err := upgrader.Upgrade(u.w, u.req, nil)
		if err != nil {
			logger.Error(err, "Error upgrading websocket connection")
			metrics.IncStreamTunnelRequest(context.Background(), strconv.Itoa(http.StatusInternalServerError))
			u.err = err
			return u.err
		}

		logger.V(4).Info("Websocket connection created", "protocol", conn.Subprotocol())
		metrics.IncStreamTunnelRequest(context.Background(), strconv.Itoa(http.StatusSwitchingProtocols))
		u.conn = portforward.NewTunnelingConnectionWithLogger(klog.LoggerWithName(logger, "server"), conn)
		return nil
	}

	// anything other than an upgrade should pass through the backend response
	logger.Error(nil, "SPDY upgrade failed", "status", backendResponse.Status)
	metrics.IncStreamTunnelRequest(context.Background(), strconv.Itoa(backendResponse.StatusCode))

	// try to hijack
	conn, _, err = u.w.(http.Hijacker).Hijack()
	if err != nil {
		logger.Error(err, "Unable to hijack response")
		u.err = err
		return u.err
	}
	// replay the backend response bytes to the hijacked conn
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	_, err = conn.Write(backendResponseBytes)
	if err != nil {
		u.err = err
		return u.err
	}
	u.conn = conn
	return nil
}

func (u *tunnelingWebsocketUpgraderConn) Read(b []byte) (n int, err error) {
	u.lock.RLock()
	defer u.lock.RUnlock()
	if u.conn != nil {
		return u.conn.Read(b)
	}
	if u.err != nil {
		return 0, u.err
	}
	// return empty read without blocking until we are initialized
	return 0, nil
}
func (u *tunnelingWebsocketUpgraderConn) Write(b []byte) (n int, err error) {
	u.lock.RLock()
	defer u.lock.RUnlock()
	if u.conn != nil {
		return u.conn.Write(b)
	}
	if u.err != nil {
		return 0, u.err
	}
	return 0, fmt.Errorf("Write called before Initialize")
}
func (u *tunnelingWebsocketUpgraderConn) Close() error {
	u.lock.Lock()
	defer u.lock.Unlock()
	if u.conn != nil {
		return u.conn.Close()
	}
	if u.err != nil {
		return u.err
	}
	// record that we closed so we don't write again or try to initialize
	u.err = fmt.Errorf("connection closed")
	// write a response
	http.Error(u.w, u.err.Error(), http.StatusInternalServerError)
	return nil
}
func (u *tunnelingWebsocketUpgraderConn) LocalAddr() net.Addr {
	u.lock.RLock()
	defer u.lock.RUnlock()
	if u.conn != nil {
		return u.conn.LocalAddr()
	}
	return noopAddr{}
}
func (u *tunnelingWebsocketUpgraderConn) RemoteAddr() net.Addr {
	u.lock.RLock()
	defer u.lock.RUnlock()
	if u.conn != nil {
		return u.conn.RemoteAddr()
	}
	return noopAddr{}
}
func (u *tunnelingWebsocketUpgraderConn) SetDeadline(t time.Time) error {
	u.lock.RLock()
	defer u.lock.RUnlock()
	if u.conn != nil {
		return u.conn.SetDeadline(t)
	}
	return nil
}
func (u *tunnelingWebsocketUpgraderConn) SetReadDeadline(t time.Time) error {
	u.lock.RLock()
	defer u.lock.RUnlock()
	if u.conn != nil {
		return u.conn.SetReadDeadline(t)
	}
	return nil
}
func (u *tunnelingWebsocketUpgraderConn) SetWriteDeadline(t time.Time) error {
	u.lock.RLock()
	defer u.lock.RUnlock()
	if u.conn != nil {
		return u.conn.SetWriteDeadline(t)
	}
	return nil
}

type noopAddr struct{}

func (n noopAddr) Network() string { return "" }
func (n noopAddr) String() string  { return "" }
