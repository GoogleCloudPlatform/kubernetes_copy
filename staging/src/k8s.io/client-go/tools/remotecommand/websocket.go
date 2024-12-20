/*
Copyright 2023 The Kubernetes Authors.

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

package remotecommand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	gwebsocket "github.com/gorilla/websocket"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/remotecommand"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/transport/websocket"
	"k8s.io/klog/v2"
)

// writeDeadline defines the time that a client-side write to the websocket
// connection must complete before an i/o timeout occurs.
const writeDeadline = 60 * time.Second

var (
	_ Executor          = &wsStreamExecutor{}
	_ streamCreator     = &wsStreamCreator{}
	_ httpstream.Stream = &stream{}

	streamType2streamID = map[string]byte{
		v1.StreamTypeStdin:  remotecommand.StreamStdIn,
		v1.StreamTypeStdout: remotecommand.StreamStdOut,
		v1.StreamTypeStderr: remotecommand.StreamStdErr,
		v1.StreamTypeError:  remotecommand.StreamErr,
		v1.StreamTypeResize: remotecommand.StreamResize,
	}
)

const (
	// pingPeriod defines how often a heartbeat "ping" message is sent.
	pingPeriod = 5 * time.Second
	// pingReadDeadline defines the time waiting for a response heartbeat
	// "pong" message before a timeout error occurs for websocket reading.
	// This duration must always be greater than the "pingPeriod". By defining
	// this deadline in terms of the ping period, we are essentially saying
	// we can drop "X" (e.g. 12) pings before firing the timeout.
	pingReadDeadline = (pingPeriod * 12) + (1 * time.Second)
)

// wsStreamExecutor handles transporting standard shell streams over an httpstream connection.
type wsStreamExecutor struct {
	transport http.RoundTripper
	upgrader  websocket.ConnectionHolder
	method    string
	url       string
	// requested protocols in priority order (e.g. v5.channel.k8s.io before v4.channel.k8s.io).
	protocols []string
	// selected protocol from the handshake process; could be empty string if handshake fails.
	negotiated string
	// period defines how often a "ping" heartbeat message is sent to the other endpoint.
	heartbeatPeriod time.Duration
	// deadline defines the amount of time before "pong" response must be received.
	heartbeatDeadline time.Duration
}

func NewWebSocketExecutor(config *restclient.Config, method, url string) (Executor, error) {
	// Only supports V5 protocol for correct version skew functionality.
	// Previous api servers will proxy upgrade requests to legacy websocket
	// servers on container runtimes which support V1-V4. These legacy
	// websocket servers will not handle the new CLOSE signal.
	return NewWebSocketExecutorForProtocols(config, method, url, remotecommand.StreamProtocolV5Name)
}

// NewWebSocketExecutorForProtocols allows to execute commands via a WebSocket connection.
func NewWebSocketExecutorForProtocols(config *restclient.Config, method, url string, protocols ...string) (Executor, error) {
	transport, upgrader, err := websocket.RoundTripperFor(config)
	if err != nil {
		return nil, fmt.Errorf("error creating websocket transports: %v", err)
	}
	return &wsStreamExecutor{
		transport:         transport,
		upgrader:          upgrader,
		method:            method,
		url:               url,
		protocols:         protocols,
		heartbeatPeriod:   pingPeriod,
		heartbeatDeadline: pingReadDeadline,
	}, nil
}

// Deprecated: use StreamWithContext instead to avoid possible resource leaks.
// See https://github.com/kubernetes/kubernetes/pull/103177 for details.
func (e *wsStreamExecutor) Stream(options StreamOptions) error {
	return e.StreamWithContext(context.Background(), options)
}

// StreamWithContext upgrades an HTTPRequest to a WebSocket connection, and starts the various
// goroutines to implement the necessary streams over the connection. The "options" parameter
// defines which streams are requested. Returns an error if one occurred. This method is NOT
// safe to run concurrently with the same executor (because of the state stored in the upgrader).
func (e *wsStreamExecutor) StreamWithContext(ctx context.Context, options StreamOptions) error {
	req, err := http.NewRequestWithContext(ctx, e.method, e.url, nil)
	if err != nil {
		return err
	}
	conn, err := websocket.Negotiate(e.transport, e.upgrader, req, e.protocols...)
	if err != nil {
		return err
	}
	if conn == nil {
		panic(fmt.Errorf("websocket connection is nil"))
	}
	defer conn.Close()
	e.negotiated = conn.Subprotocol()
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Subprotocol negotiated", "protocol", e.negotiated)

	var streamer streamProtocolHandler
	switch e.negotiated {
	case remotecommand.StreamProtocolV5Name:
		streamer = newStreamProtocolV5(options)
	case remotecommand.StreamProtocolV4Name:
		streamer = newStreamProtocolV4(options)
	case remotecommand.StreamProtocolV3Name:
		streamer = newStreamProtocolV3(options)
	case remotecommand.StreamProtocolV2Name:
		streamer = newStreamProtocolV2(options)
	case "":
		logger.V(4).Info("The server did not negotiate a streaming protocol version, falling back", "protocol", remotecommand.StreamProtocolV1Name)
		fallthrough
	case remotecommand.StreamProtocolV1Name:
		streamer = newStreamProtocolV1(options)
	}

	panicChan := make(chan any, 1)
	errorChan := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				panicChan <- p
			}
		}()
		creator := newWSStreamCreator(logger, conn)
		go creator.readDemuxLoop(
			e.upgrader.DataBufferSize(),
			e.heartbeatPeriod,
			e.heartbeatDeadline,
		)
		errorChan <- streamer.stream(logger, creator)
	}()

	select {
	case p := <-panicChan:
		panic(p)
	case err := <-errorChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type wsStreamCreator struct {
	logger klog.Logger
	conn   *gwebsocket.Conn
	// Protects writing to websocket connection; reading is lock-free
	connWriteLock sync.Mutex
	// map of stream id to stream; multiple streams read/write the connection
	streams   map[byte]*stream
	streamsMu sync.Mutex
	// setStreamErr holds the error to return to anyone calling setStreams.
	// this is populated in closeAllStreamReaders
	setStreamErr error
}

func newWSStreamCreator(logger klog.Logger, conn *gwebsocket.Conn) *wsStreamCreator {
	return &wsStreamCreator{
		logger:  logger,
		conn:    conn,
		streams: map[byte]*stream{},
	}
}

func (c *wsStreamCreator) getStream(id byte) *stream {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	return c.streams[id]
}

func (c *wsStreamCreator) setStream(id byte, s *stream) error {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	if c.setStreamErr != nil {
		return c.setStreamErr
	}
	c.streams[id] = s
	return nil
}

// CreateStream uses id from passed headers to create a stream over "c.conn" connection.
// Returns a Stream structure or nil and an error if one occurred.
func (c *wsStreamCreator) CreateStream(headers http.Header) (httpstream.Stream, error) {
	streamType := headers.Get(v1.StreamType)
	id, ok := streamType2streamID[streamType]
	if !ok {
		return nil, fmt.Errorf("unknown stream type: %s", streamType)
	}
	if s := c.getStream(id); s != nil {
		return nil, fmt.Errorf("duplicate stream for type %s", streamType)
	}
	reader, writer := io.Pipe()
	s := &stream{
		logger:        klog.LoggerWithValues(c.logger, "id", id),
		headers:       headers,
		readPipe:      reader,
		writePipe:     writer,
		conn:          c.conn,
		connWriteLock: &c.connWriteLock,
		id:            id,
	}
	if err := c.setStream(id, s); err != nil {
		_ = s.writePipe.Close()
		_ = s.readPipe.Close()
		return nil, err
	}
	return s, nil
}

// readDemuxLoop is the lock-free reading processor for this endpoint of the websocket
// connection. This loop reads the connection, and demultiplexes the data
// into one of the individual stream pipes (by checking the stream id). This
// loop can *not* be run concurrently, because there can only be one websocket
// connection reader at a time (a read mutex would provide no benefit).
func (c *wsStreamCreator) readDemuxLoop(bufferSize int, period time.Duration, deadline time.Duration) {
	// Initialize and start the ping/pong heartbeat.
	h := newHeartbeat(c.logger, c.conn, period, deadline)
	// Set initial timeout for websocket connection reading.
	if err := c.conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		c.logger.Error(err, "Websocket initial setting read deadline failed")
		return
	}
	go h.start()
	// Buffer size must correspond to the same size allocated
	// for the read buffer during websocket client creation. A
	// difference can cause incomplete connection reads.
	readBuffer := make([]byte, bufferSize)
	for {
		// NextReader() only returns data messages (BinaryMessage or Text
		// Message). Even though this call will never return control frames
		// such as ping, pong, or close, this call is necessary for these
		// message types to be processed. There can only be one reader
		// at a time, so this reader loop must *not* be run concurrently;
		// there is no lock for reading. Calling "NextReader()" before the
		// current reader has been processed will close the current reader.
		// If the heartbeat read deadline times out, this "NextReader()" will
		// return an i/o error, and error handling will clean up.
		messageType, r, err := c.conn.NextReader()
		if err != nil {
			websocketErr, ok := err.(*gwebsocket.CloseError)
			if ok && websocketErr.Code == gwebsocket.CloseNormalClosure {
				err = nil // readers will get io.EOF as it's a normal closure
			} else {
				err = fmt.Errorf("next reader: %w", err)
			}
			c.closeAllStreamReaders(err)
			return
		}
		// All remote command protocols send/receive only binary data messages.
		if messageType != gwebsocket.BinaryMessage {
			c.closeAllStreamReaders(fmt.Errorf("unexpected message type: %d", messageType))
			return
		}
		// It's ok to read just a single byte because the underlying library wraps the actual
		// connection with a buffered reader anyway.
		_, err = io.ReadFull(r, readBuffer[:1])
		if err != nil {
			c.closeAllStreamReaders(fmt.Errorf("read stream id: %w", err))
			return
		}
		streamID := readBuffer[0]
		s := c.getStream(streamID)
		if s == nil {
			c.logger.Error(nil, "Unknown stream, discarding message", "id", streamID)
			continue
		}
		for {
			nr, errRead := r.Read(readBuffer)
			if nr > 0 {
				// Write the data to the stream's pipe. This can block.
				_, errWrite := s.writePipe.Write(readBuffer[:nr])
				if errWrite != nil {
					// Pipe must have been closed by the stream user.
					// Nothing to do, discard the message.
					break
				}
			}
			if errRead != nil {
				if errRead == io.EOF {
					break
				}
				c.closeAllStreamReaders(fmt.Errorf("read message: %w", err))
				return
			}
		}
	}
}

// closeAllStreamReaders closes readers in all streams.
// This unblocks all stream.Read() calls, and keeps any future streams from being created.
func (c *wsStreamCreator) closeAllStreamReaders(err error) {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	for _, s := range c.streams {
		// Closing writePipe unblocks all readPipe.Read() callers and prevents any future writes.
		_ = s.writePipe.CloseWithError(err)
	}
	// ensure callers to setStreams receive an error after this point
	if err != nil {
		c.setStreamErr = err
	} else {
		c.setStreamErr = fmt.Errorf("closed all streams")
	}
}

type stream struct {
	logger    klog.Logger
	headers   http.Header
	readPipe  *io.PipeReader
	writePipe *io.PipeWriter
	// conn is used for writing directly into the connection.
	// Is nil after Close() / Reset() to prevent future writes.
	conn *gwebsocket.Conn
	// connWriteLock protects conn against concurrent write operations. There must be a single writer and a single reader only.
	// The mutex is shared across all streams because the underlying connection is shared.
	connWriteLock *sync.Mutex
	id            byte
}

func (s *stream) Read(p []byte) (n int, err error) {
	return s.readPipe.Read(p)
}

// Write writes directly to the underlying WebSocket connection.
func (s *stream) Write(p []byte) (n int, err error) {
	s.logger.V(4).Info("Write() on stream")
	defer s.logger.V(4).Info("Write() done on stream")
	s.connWriteLock.Lock()
	defer s.connWriteLock.Unlock()
	if s.conn == nil {
		return 0, fmt.Errorf("write on closed stream %d", s.id)
	}
	err = s.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	if err != nil {
		s.logger.V(7).Info("Websocket setting write deadline failed", "err", err)
		return 0, err
	}
	// Message writer buffers the message data, so we don't need to do that ourselves.
	// Just write id and the data as two separate writes to avoid allocating an intermediate buffer.
	w, err := s.conn.NextWriter(gwebsocket.BinaryMessage)
	if err != nil {
		return 0, err
	}
	defer func() {
		if w != nil {
			w.Close()
		}
	}()
	_, err = w.Write([]byte{s.id})
	if err != nil {
		return 0, err
	}
	n, err = w.Write(p)
	if err != nil {
		return n, err
	}
	err = w.Close()
	w = nil
	return n, err
}

// Close half-closes the stream, indicating this side is finished with the stream.
func (s *stream) Close() error {
	s.logger.V(4).Info("Close() on stream")
	defer s.logger.V(4).Info("Close() done on stream")
	s.connWriteLock.Lock()
	defer s.connWriteLock.Unlock()
	if s.conn == nil {
		return fmt.Errorf("Close() on already closed stream %d", s.id)
	}
	// Communicate the CLOSE stream signal to the other websocket endpoint.
	err := s.conn.WriteMessage(gwebsocket.BinaryMessage, []byte{remotecommand.StreamClose, s.id})
	s.conn = nil
	return err
}

func (s *stream) Reset() error {
	s.logger.V(4).Info("Reset() on stream")
	defer s.logger.V(4).Info("Reset() done on stream")
	s.Close()
	return s.writePipe.Close()
}

func (s *stream) Headers() http.Header {
	return s.headers
}

func (s *stream) Identifier() uint32 {
	return uint32(s.id)
}

// heartbeat encasulates data necessary for the websocket ping/pong heartbeat. This
// heartbeat works by setting a read deadline on the websocket connection, then
// pushing this deadline into the future for every successful heartbeat. If the
// heartbeat "pong" fails to respond within the deadline, then the "NextReader()" call
// inside the "readDemuxLoop" will return an i/o error prompting a connection close
// and cleanup.
type heartbeat struct {
	logger klog.Logger
	conn   *gwebsocket.Conn
	// period defines how often a "ping" heartbeat message is sent to the other endpoint
	period time.Duration
	// closing the "closer" channel will clean up the heartbeat timers
	closer chan struct{}
	// optional data to send with "ping" message
	message []byte
	// optionally received data message with "pong" message, same as sent with ping
	pongMessage []byte
}

// newHeartbeat creates heartbeat structure encapsulating fields necessary to
// run the websocket connection ping/pong mechanism and sets up handlers on
// the websocket connection.
func newHeartbeat(logger klog.Logger, conn *gwebsocket.Conn, period time.Duration, deadline time.Duration) *heartbeat {
	h := &heartbeat{
		logger: logger,
		conn:   conn,
		period: period,
		closer: make(chan struct{}),
	}
	// Set up handler for receiving returned "pong" message from other endpoint
	// by pushing the read deadline into the future. The "msg" received could
	// be empty.
	h.conn.SetPongHandler(func(msg string) error {
		// Push the read deadline into the future.
		logger.V(8).Info("Pong message received -- resetting read deadline", "message", msg)
		err := h.conn.SetReadDeadline(time.Now().Add(deadline))
		if err != nil {
			logger.Error(err, "Websocket setting read deadline failed")
			return err
		}
		if len(msg) > 0 {
			h.pongMessage = []byte(msg)
		}
		return nil
	})
	// Set up handler to cleanup timers when this endpoint receives "Close" message.
	closeHandler := h.conn.CloseHandler()
	h.conn.SetCloseHandler(func(code int, text string) error {
		close(h.closer)
		return closeHandler(code, text)
	})
	return h
}

// setMessage is optional data sent with "ping" heartbeat. According to the websocket RFC
// this data sent with "ping" message should be returned in "pong" message.
func (h *heartbeat) setMessage(msg string) {
	h.message = []byte(msg)
}

// start the heartbeat by setting up necesssary handlers and looping by sending "ping"
// message every "period" until the "closer" channel is closed.
func (h *heartbeat) start() {
	// Loop to continually send "ping" message through websocket connection every "period".
	t := time.NewTicker(h.period)
	defer t.Stop()
	for {
		select {
		case <-h.closer:
			h.logger.V(8).Info("Closed channel -- returning")
			return
		case <-t.C:
			// "WriteControl" does not need to be protected by a mutex. According to
			// gorilla/websockets library docs: "The Close and WriteControl methods can
			// be called concurrently with all other methods."
			if err := h.conn.WriteControl(gwebsocket.PingMessage, h.message, time.Now().Add(pingReadDeadline)); err == nil {
				h.logger.V(8).Info("Websocket Ping succeeeded")
			} else {
				h.logger.Error(err, "Websocket Ping failed")
				if errors.Is(err, gwebsocket.ErrCloseSent) {
					// we continue because c.conn.CloseChan will manage closing the connection already
					continue
				} else if e, ok := err.(net.Error); ok && e.Timeout() {
					// Continue, in case this is a transient failure.
					// c.conn.CloseChan above will tell us when the connection is
					// actually closed.
					// If Temporary function hadn't been deprecated, we would have used it.
					// But most of temporary errors are timeout errors anyway.
					continue
				}
				return
			}
		}
	}
}
