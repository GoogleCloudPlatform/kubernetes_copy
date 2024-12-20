/*
Copyright 2015 The Kubernetes Authors.

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

package wsstream

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

const WebSocketProtocolHeader = "Sec-Websocket-Protocol"

// The Websocket subprotocol "channel.k8s.io" prepends each binary message with a byte indicating
// the channel number (zero indexed) the message was sent on. Messages in both directions should
// prefix their messages with this channel byte. When used for remote execution, the channel numbers
// are by convention defined to match the POSIX file-descriptors assigned to STDIN, STDOUT, and STDERR
// (0, 1, and 2). No other conversion is performed on the raw subprotocol - writes are sent as they
// are received by the server.
//
// Example client session:
//
//	CONNECT http://server.com with subprotocol "channel.k8s.io"
//	WRITE []byte{0, 102, 111, 111, 10} # send "foo\n" on channel 0 (STDIN)
//	READ  []byte{1, 10}                # receive "\n" on channel 1 (STDOUT)
//	CLOSE
const ChannelWebSocketProtocol = "channel.k8s.io"

// The Websocket subprotocol "base64.channel.k8s.io" base64 encodes each message with a character
// indicating the channel number (zero indexed) the message was sent on. Messages in both directions
// should prefix their messages with this channel char. When used for remote execution, the channel
// numbers are by convention defined to match the POSIX file-descriptors assigned to STDIN, STDOUT,
// and STDERR ('0', '1', and '2'). The data received on the server is base64 decoded (and must be
// be valid) and data written by the server to the client is base64 encoded.
//
// Example client session:
//
//	CONNECT http://server.com with subprotocol "base64.channel.k8s.io"
//	WRITE []byte{48, 90, 109, 57, 118, 67, 103, 111, 61} # send "foo\n" (base64: "Zm9vCgo=") on channel '0' (STDIN)
//	READ  []byte{49, 67, 103, 61, 61} # receive "\n" (base64: "Cg==") on channel '1' (STDOUT)
//	CLOSE
const Base64ChannelWebSocketProtocol = "base64.channel.k8s.io"

type codecType int

const (
	rawCodec codecType = iota
	base64Codec
)

type ChannelType int

const (
	IgnoreChannel ChannelType = iota
	ReadChannel
	WriteChannel
	ReadWriteChannel
)

// IsWebSocketRequest returns true if the incoming request contains connection upgrade headers
// for WebSockets.
func IsWebSocketRequest(req *http.Request) bool {
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return false
	}
	return httpstream.IsUpgradeRequest(req)
}

// IsWebSocketRequestWithStreamCloseProtocol returns true if the request contains headers
// identifying that it is requesting a websocket upgrade with a remotecommand protocol
// version that supports the "CLOSE" signal; false otherwise.
func IsWebSocketRequestWithStreamCloseProtocol(req *http.Request) bool {
	if !IsWebSocketRequest(req) {
		return false
	}
	requestedProtocols := strings.TrimSpace(req.Header.Get(WebSocketProtocolHeader))
	for _, requestedProtocol := range strings.Split(requestedProtocols, ",") {
		if protocolSupportsStreamClose(strings.TrimSpace(requestedProtocol)) {
			return true
		}
	}

	return false
}

// IsWebSocketRequestWithTunnelingProtocol returns true if the request contains headers
// identifying that it is requesting a websocket upgrade with a tunneling protocol;
// false otherwise.
func IsWebSocketRequestWithTunnelingProtocol(req *http.Request) bool {
	if !IsWebSocketRequest(req) {
		return false
	}
	requestedProtocols := strings.TrimSpace(req.Header.Get(WebSocketProtocolHeader))
	for _, requestedProtocol := range strings.Split(requestedProtocols, ",") {
		if protocolSupportsWebsocketTunneling(strings.TrimSpace(requestedProtocol)) {
			return true
		}
	}

	return false
}

// IgnoreReceives reads from a WebSocket until it is closed, then returns. If timeout is set, the
// read and write deadlines are pushed every time a new message is received.
//
// Contextual logging: IgnoreReceivesWithLogger should be used instead of IgnoreReceives in code which uses contextual logging.
func IgnoreReceives(ws *websocket.Conn, timeout time.Duration) {
	IgnoreReceivesWithLogger(klog.Background(), ws, timeout)
}

// IgnoreReceivesWithLogger reads from a WebSocket until it is closed, then returns. If timeout is set, the
// read and write deadlines are pushed every time a new message is received.
func IgnoreReceivesWithLogger(logger klog.Logger, ws *websocket.Conn, timeout time.Duration) {
	defer runtime.HandleCrashWithLogger(logger)
	var data []byte
	for {
		resetTimeout(ws, timeout)
		if err := websocket.Message.Receive(ws, &data); err != nil {
			return
		}
	}
}

// handshake ensures the provided user protocol matches one of the allowed protocols. It returns
// no error if no protocol is specified.
func handshake(config *websocket.Config, req *http.Request, allowed []string) error {
	protocols := config.Protocol
	if len(protocols) == 0 {
		protocols = []string{""}
	}

	for _, protocol := range protocols {
		for _, allow := range allowed {
			if allow == protocol {
				config.Protocol = []string{protocol}
				return nil
			}
		}
	}

	return fmt.Errorf("requested protocol(s) are not supported: %v; supports %v", config.Protocol, allowed)
}

// ChannelProtocolConfig describes a websocket subprotocol with channels.
type ChannelProtocolConfig struct {
	Binary   bool
	Channels []ChannelType
}

// NewDefaultChannelProtocols returns a channel protocol map with the
// subprotocols "", "channel.k8s.io", "base64.channel.k8s.io" and the given
// channels.
func NewDefaultChannelProtocols(channels []ChannelType) map[string]ChannelProtocolConfig {
	return map[string]ChannelProtocolConfig{
		"":                             {Binary: true, Channels: channels},
		ChannelWebSocketProtocol:       {Binary: true, Channels: channels},
		Base64ChannelWebSocketProtocol: {Binary: false, Channels: channels},
	}
}

// Conn supports sending multiple binary channels over a websocket connection.
type Conn struct {
	protocols        map[string]ChannelProtocolConfig
	selectedProtocol string
	channels         []*websocketChannel
	codec            codecType
	ready            chan struct{}
	ws               *websocket.Conn
	timeout          time.Duration
}

// NewConn creates a WebSocket connection that supports a set of channels. Channels begin each
// web socket message with a single byte indicating the channel number (0-N). 255 is reserved for
// future use. The channel types for each channel are passed as an array, supporting the different
// duplex modes. Read and Write refer to whether the channel can be used as a Reader or Writer.
//
// The protocols parameter maps subprotocol names to ChannelProtocols. The empty string subprotocol
// name is used if websocket.Config.Protocol is empty.
func NewConn(protocols map[string]ChannelProtocolConfig) *Conn {
	return &Conn{
		ready:     make(chan struct{}),
		protocols: protocols,
	}
}

// SetIdleTimeout sets the interval for both reads and writes before timeout. If not specified,
// there is no timeout on the connection.
func (conn *Conn) SetIdleTimeout(duration time.Duration) {
	conn.timeout = duration
}

// SetWriteDeadline sets a timeout on writing to the websocket connection. The
// passed "duration" identifies how far into the future the write must complete
// by before the timeout fires.
func (conn *Conn) SetWriteDeadline(duration time.Duration) {
	conn.ws.SetWriteDeadline(time.Now().Add(duration)) //nolint:errcheck
}

// Open the connection and create channels for reading and writing. It returns
// the selected subprotocol, a slice of channels and an error.
func (conn *Conn) Open(w http.ResponseWriter, req *http.Request) (string, []io.ReadWriteCloser, error) {
	// serveHTTPComplete is channel that is closed/selected when "websocket#ServeHTTP" finishes.
	serveHTTPComplete := make(chan struct{})
	// Ensure panic in spawned goroutine is propagated into the parent goroutine.
	panicChan := make(chan any, 1)
	go func() {
		// If websocket server returns, propagate panic if necessary. Otherwise,
		// signal HTTPServe finished by closing "serveHTTPComplete".
		defer func() {
			if p := recover(); p != nil {
				panicChan <- p
			} else {
				close(serveHTTPComplete)
			}
		}()
		websocket.Server{Handshake: conn.handshake, Handler: conn.handle}.ServeHTTP(w, req)
	}()

	// In normal circumstances, "websocket.Server#ServeHTTP" calls "initialize" which closes
	// "conn.ready" and then blocks until serving is complete.
	select {
	case <-conn.ready:
		klog.FromContext(req.Context()).V(8).Info("websocket server initialized--serving")
	case <-serveHTTPComplete:
		// websocket server returned before completing initialization; cleanup and return error.
		conn.closeNonThreadSafe() //nolint:errcheck
		return "", nil, fmt.Errorf("websocket server finished before becoming ready")
	case p := <-panicChan:
		panic(p)
	}

	rwc := make([]io.ReadWriteCloser, len(conn.channels))
	for i := range conn.channels {
		rwc[i] = conn.channels[i]
	}
	return conn.selectedProtocol, rwc, nil
}

func (conn *Conn) initialize(ws *websocket.Conn) {
	negotiated := ws.Config().Protocol
	conn.selectedProtocol = negotiated[0]
	p := conn.protocols[conn.selectedProtocol]
	if p.Binary {
		conn.codec = rawCodec
	} else {
		conn.codec = base64Codec
	}
	conn.ws = ws
	conn.channels = make([]*websocketChannel, len(p.Channels))
	for i, t := range p.Channels {
		switch t {
		case ReadChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), true, false)
		case WriteChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), false, true)
		case ReadWriteChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), true, true)
		case IgnoreChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), false, false)
		}
	}

	close(conn.ready)
}

func (conn *Conn) handshake(config *websocket.Config, req *http.Request) error {
	supportedProtocols := make([]string, 0, len(conn.protocols))
	for p := range conn.protocols {
		supportedProtocols = append(supportedProtocols, p)
	}
	return handshake(config, req, supportedProtocols)
}

func (conn *Conn) resetTimeout() {
	if conn.timeout > 0 {
		conn.ws.SetDeadline(time.Now().Add(conn.timeout))
	}
}

// closeNonThreadSafe cleans up by closing streams and the websocket
// connection *without* waiting for the "ready" channel.
func (conn *Conn) closeNonThreadSafe() error {
	for _, s := range conn.channels {
		s.Close()
	}
	var err error
	if conn.ws != nil {
		err = conn.ws.Close()
	}
	return err
}

// Close is only valid after Open has been called
func (conn *Conn) Close() error {
	<-conn.ready
	return conn.closeNonThreadSafe()
}

// protocolSupportsStreamClose returns true if the passed protocol
// supports the stream close signal (currently only V5 remotecommand);
// false otherwise.
func protocolSupportsStreamClose(protocol string) bool {
	return protocol == remotecommand.StreamProtocolV5Name
}

// protocolSupportsWebsocketTunneling returns true if the passed protocol
// is a tunneled Kubernetes spdy protocol; false otherwise.
func protocolSupportsWebsocketTunneling(protocol string) bool {
	return strings.HasPrefix(protocol, portforward.WebsocketsSPDYTunnelingPrefix) && strings.HasSuffix(protocol, portforward.KubernetesSuffix)
}

// handle implements a websocket handler.
func (conn *Conn) handle(ws *websocket.Conn) {
	conn.initialize(ws)
	defer conn.Close()
	supportsStreamClose := protocolSupportsStreamClose(conn.selectedProtocol)
	// conn.handle is typically used on the server-side and thus we have a request,
	// but don't assume that and use klog.Background as fallback.
	logger := klog.Background()
	if req := ws.Request(); req != nil {
		logger = klog.FromContext(req.Context())
	}

	for {
		conn.resetTimeout()
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err != nil {
			if err != io.EOF {
				logger.Error(err, "Error on socket receive")
			}
			break
		}
		if len(data) == 0 {
			continue
		}
		if supportsStreamClose && data[0] == remotecommand.StreamClose {
			if len(data) != 2 {
				logger.Error(nil, "Single channel byte should follow stream close signal", "receivedLength", len(data)-1)
				break
			} else {
				channel := data[1]
				if int(channel) >= len(conn.channels) {
					logger.Error(nil, "Close is targeted for a channel that is not valid, possible protocol error", "channel", channel)
					break
				}
				logger.V(4).Info("Received half-close signal from client, close stream", "channel", channel)
				conn.channels[channel].Close() // After first Close, other closes are noop.
			}
			continue
		}
		channel := data[0]
		if conn.codec == base64Codec {
			channel = channel - '0'
		}
		data = data[1:]
		if int(channel) >= len(conn.channels) {
			logger.V(6).Info("Frame is targeted for a reader that is not valid, possible protocol error", "channel", channel)
			continue
		}
		if _, err := conn.channels[channel].DataFromSocket(data); err != nil {
			logger.Error(err, "Unable to write frame", "sendLength", len(data), "channel", channel, "err", err)
			continue
		}
	}
}

// write multiplexes the specified channel onto the websocket
func (conn *Conn) write(num byte, data []byte) (int, error) {
	conn.resetTimeout()
	switch conn.codec {
	case rawCodec:
		frame := make([]byte, len(data)+1)
		frame[0] = num
		copy(frame[1:], data)
		if err := websocket.Message.Send(conn.ws, frame); err != nil {
			return 0, err
		}
	case base64Codec:
		frame := string('0'+num) + base64.StdEncoding.EncodeToString(data)
		if err := websocket.Message.Send(conn.ws, frame); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

// websocketChannel represents a channel in a connection
type websocketChannel struct {
	conn *Conn
	num  byte
	r    io.Reader
	w    io.WriteCloser

	read, write bool
}

// newWebsocketChannel creates a pipe for writing to a websocket. Do not write to this pipe
// prior to the connection being opened. It may be no, half, or full duplex depending on
// read and write.
func newWebsocketChannel(conn *Conn, num byte, read, write bool) *websocketChannel {
	r, w := io.Pipe()
	return &websocketChannel{conn, num, r, w, read, write}
}

func (p *websocketChannel) Write(data []byte) (int, error) {
	if !p.write {
		return len(data), nil
	}
	return p.conn.write(p.num, data)
}

// DataFromSocket is invoked by the connection receiver to move data from the connection
// into a specific channel.
func (p *websocketChannel) DataFromSocket(data []byte) (int, error) {
	if !p.read {
		return len(data), nil
	}

	switch p.conn.codec {
	case rawCodec:
		return p.w.Write(data)
	case base64Codec:
		dst := make([]byte, len(data))
		n, err := base64.StdEncoding.Decode(dst, data)
		if err != nil {
			return 0, err
		}
		return p.w.Write(dst[:n])
	}
	return 0, nil
}

func (p *websocketChannel) Read(data []byte) (int, error) {
	if !p.read {
		return 0, io.EOF
	}
	return p.r.Read(data)
}

func (p *websocketChannel) Close() error {
	return p.w.Close()
}
