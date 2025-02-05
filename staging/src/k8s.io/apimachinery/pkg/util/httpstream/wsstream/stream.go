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
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// The WebSocket subprotocol "binary.k8s.io" will only send messages to the
// client and ignore messages sent to the server. The received messages are
// the exact bytes written to the stream. Zero byte messages are possible.
const binaryWebSocketProtocol = "binary.k8s.io"

// The WebSocket subprotocol "base64.binary.k8s.io" will only send messages to the
// client and ignore messages sent to the server. The received messages are
// a base64 version of the bytes written to the stream. Zero byte messages are
// possible.
const base64BinaryWebSocketProtocol = "base64.binary.k8s.io"

// ReaderProtocolConfig describes a websocket subprotocol with one stream.
type ReaderProtocolConfig struct {
	Binary bool
}

// NewDefaultReaderProtocols returns a stream protocol map with the
// subprotocols "", "channel.k8s.io", "base64.channel.k8s.io".
func NewDefaultReaderProtocols() map[string]ReaderProtocolConfig {
	return map[string]ReaderProtocolConfig{
		"":                            {Binary: true},
		binaryWebSocketProtocol:       {Binary: true},
		base64BinaryWebSocketProtocol: {Binary: false},
	}
}

// Reader supports returning an arbitrary byte stream over a websocket channel.
type Reader struct {
	logger           klog.Logger
	err              chan error
	r                io.Reader
	ping             bool
	timeout          time.Duration
	protocols        map[string]ReaderProtocolConfig
	selectedProtocol string

	handleCrash func(ctx context.Context, additionalHandlers ...func(context.Context, interface{})) // overridable for testing
}

// NewReader creates a WebSocket pipe that will copy the contents of r to a provided
// WebSocket connection. If ping is true, a zero length message will be sent to the client
// before the stream begins reading.
//
// The protocols parameter maps subprotocol names to StreamProtocols. The empty string
// subprotocol name is used if websocket.Config.Protocol is empty.
//
//logcheck:context // NewReaderWithLogger should be used instead of NewReader in code which supports contextual logging.
func NewReader(r io.Reader, ping bool, protocols map[string]ReaderProtocolConfig) *Reader {
	return NewReaderWithLogger(klog.Background(), r, ping, protocols)
}

// NewReaderWithLogger creates a WebSocket pipe that will copy the contents of r to a provided
// WebSocket connection. If ping is true, a zero length message will be sent to the client
// before the stream begins reading.
//
// The protocols parameter maps subprotocol names to StreamProtocols. The empty string
// subprotocol name is used if websocket.Config.Protocol is empty.
func NewReaderWithLogger(logger klog.Logger, r io.Reader, ping bool, protocols map[string]ReaderProtocolConfig) *Reader {
	return &Reader{
		logger:      logger,
		r:           r,
		err:         make(chan error),
		ping:        ping,
		protocols:   protocols,
		handleCrash: runtime.HandleCrashWithContext,
	}
}

// SetIdleTimeout sets the interval for both reads and writes before timeout. If not specified,
// there is no timeout on the reader.
func (r *Reader) SetIdleTimeout(duration time.Duration) {
	r.timeout = duration
}

func (r *Reader) handshake(config *websocket.Config, req *http.Request) error {
	supportedProtocols := make([]string, 0, len(r.protocols))
	for p := range r.protocols {
		supportedProtocols = append(supportedProtocols, p)
	}
	return handshake(config, req, supportedProtocols)
}

// Copy the reader to the response. The created WebSocket is closed after this
// method completes.
func (r *Reader) Copy(w http.ResponseWriter, req *http.Request) error {
	go func() {
		defer r.handleCrash(req.Context())
		websocket.Server{Handshake: r.handshake, Handler: r.handle}.ServeHTTP(w, req)
	}()
	return <-r.err
}

// handle implements a WebSocket handler.
func (r *Reader) handle(ws *websocket.Conn) {
	// Close the connection when the client requests it, or when we finish streaming, whichever happens first
	closeConnOnce := &sync.Once{}
	closeConn := func() {
		closeConnOnce.Do(func() {
			ws.Close()
		})
	}

	negotiated := ws.Config().Protocol
	r.selectedProtocol = negotiated[0]
	defer close(r.err)
	defer closeConn()

	go func() {
		defer runtime.HandleCrashWithLogger(r.logger)
		// This blocks until the connection is closed.
		// Client should not send anything.
		IgnoreReceivesWithLogger(r.logger, ws, r.timeout)
		// Once the client closes, we should also close
		closeConn()
	}()

	r.err <- messageCopy(ws, r.r, !r.protocols[r.selectedProtocol].Binary, r.ping, r.timeout)
}

func resetTimeout(ws *websocket.Conn, timeout time.Duration) {
	if timeout > 0 {
		ws.SetDeadline(time.Now().Add(timeout))
	}
}

func messageCopy(ws *websocket.Conn, r io.Reader, base64Encode, ping bool, timeout time.Duration) error {
	buf := make([]byte, 2048)
	if ping {
		resetTimeout(ws, timeout)
		if base64Encode {
			if err := websocket.Message.Send(ws, ""); err != nil {
				return err
			}
		} else {
			if err := websocket.Message.Send(ws, []byte{}); err != nil {
				return err
			}
		}
	}
	for {
		resetTimeout(ws, timeout)
		n, err := r.Read(buf)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if n > 0 {
			if base64Encode {
				if err := websocket.Message.Send(ws, base64.StdEncoding.EncodeToString(buf[:n])); err != nil {
					return err
				}
			} else {
				if err := websocket.Message.Send(ws, buf[:n]); err != nil {
					return err
				}
			}
		}
	}
}
