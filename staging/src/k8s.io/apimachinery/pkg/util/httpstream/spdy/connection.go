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

package spdy

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/moby/spdystream"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/klog/v2"
)

// connection maintains state about a spdystream.Connection and its associated
// streams.
type connection struct {
	conn             *spdystream.Connection
	streams          map[uint32]httpstream.Stream
	streamLock       sync.Mutex
	newStreamHandler httpstream.NewStreamHandler
	ping             func() (time.Duration, error)
}

// NewClientConnection creates a new SPDY client connection.
func NewClientConnection(conn net.Conn) (httpstream.Connection, error) {
	return NewClientConnectionWithPings(conn, 0)
}

// NewClientConnectionWithPings creates a new SPDY client connection.
//
// If pingPeriod is non-zero, a background goroutine will send periodic Ping
// frames to the server. Use this to keep idle connections through certain load
// balancers alive longer.
func NewClientConnectionWithPings(conn net.Conn, pingPeriod time.Duration) (httpstream.Connection, error) {
	spdyConn, err := spdystream.NewConnection(conn, false)
	if err != nil {
		defer conn.Close()
		return nil, err
	}

	return newConnection(spdyConn, httpstream.NoOpNewStreamHandler, pingPeriod, spdyConn.Ping), nil
}

// NewServerConnection creates a new SPDY server connection. newStreamHandler
// will be invoked when the server receives a newly created stream from the
// client.
func NewServerConnection(conn net.Conn, newStreamHandler httpstream.NewStreamHandler) (httpstream.Connection, error) {
	return NewServerConnectionWithPings(conn, newStreamHandler, 0)
}

// NewServerConnectionWithPings creates a new SPDY server connection.
// newStreamHandler will be invoked when the server receives a newly created
// stream from the client.
//
// If pingPeriod is non-zero, a background goroutine will send periodic Ping
// frames to the server. Use this to keep idle connections through certain load
// balancers alive longer.
func NewServerConnectionWithPings(conn net.Conn, newStreamHandler httpstream.NewStreamHandler, pingPeriod time.Duration) (httpstream.Connection, error) {
	spdyConn, err := spdystream.NewConnection(conn, true)
	if err != nil {
		defer conn.Close()
		return nil, err
	}

	return newConnection(spdyConn, newStreamHandler, pingPeriod, spdyConn.Ping), nil
}

// newConnection returns a new connection wrapping conn. newStreamHandler
// will be invoked when the server receives a newly created stream from the
// client.
func newConnection(conn *spdystream.Connection, newStreamHandler httpstream.NewStreamHandler, pingPeriod time.Duration, pingFn func() (time.Duration, error)) httpstream.Connection {
	c := &connection{
		conn:             conn,
		newStreamHandler: newStreamHandler,
		ping:             pingFn,
		streams:          make(map[uint32]httpstream.Stream),
	}
	go conn.Serve(c.newSpdyStream)
	if pingPeriod > 0 && pingFn != nil {
		go c.sendPings(pingPeriod)
	}
	return c
}

// createStreamResponseTimeout indicates how long to wait for the other side to
// acknowledge the new stream before timing out.
const createStreamResponseTimeout = 30 * time.Second

// Close first sends a reset for all of the connection's streams, and then
// closes the underlying spdystream.Connection.
func (c *connection) Close() error {
	c.streamLock.Lock()
	for _, s := range c.streams {
		// calling Reset instead of Close ensures that all streams are fully torn down
		s.Reset()
	}
	c.streams = make(map[uint32]httpstream.Stream, 0)
	c.streamLock.Unlock()

	// now that all streams are fully torn down, it's safe to call close on the underlying connection,
	// which should be able to terminate immediately at this point, instead of waiting for any
	// remaining graceful stream termination.
	return c.conn.Close()
}

// RemoveStreams can be used to removes a set of streams from the Connection.
func (c *connection) RemoveStreams(streams ...httpstream.Stream) {
	c.streamLock.Lock()
	for _, stream := range streams {
		// It may be possible that the provided stream is nil if timed out.
		if stream != nil {
			delete(c.streams, stream.Identifier())
		}
	}
	c.streamLock.Unlock()
}

// CreateStream creates a new stream with the specified headers and registers
// it with the connection.
func (c *connection) CreateStream(headers http.Header) (httpstream.Stream, error) {
	stream, err := c.conn.CreateStream(headers, nil, false)
	if err != nil {
		return nil, err
	}
	if err = stream.WaitTimeout(createStreamResponseTimeout); err != nil {
		return nil, err
	}

	c.registerStream(stream)
	return stream, nil
}

// registerStream adds the stream s to the connection's list of streams that
// it owns.
func (c *connection) registerStream(s httpstream.Stream) {
	c.streamLock.Lock()
	c.streams[s.Identifier()] = s
	c.streamLock.Unlock()
}

// CloseChan returns a channel that, when closed, indicates that the underlying
// spdystream.Connection has been closed.
func (c *connection) CloseChan() <-chan bool {
	return c.conn.CloseChan()
}

// newSpdyStream is the internal new stream handler used by spdystream.Connection.Serve.
// It calls connection's newStreamHandler, giving it the opportunity to accept or reject
// the stream. If newStreamHandler returns an error, the stream is rejected. If not, the
// stream is accepted and registered with the connection.
func (c *connection) newSpdyStream(stream *spdystream.Stream) {
	replySent := make(chan struct{})
	err := c.newStreamHandler(stream, replySent)
	rejectStream := (err != nil)
	if rejectStream {
		//nolint:logcheck // Hopefully this never gets triggered.
		klog.Warningf("Stream rejected: %v", err)
		stream.Reset()
		return
	}

	c.registerStream(stream)
	stream.SendReply(http.Header{}, rejectStream)
	close(replySent)
}

// SetIdleTimeout sets the amount of time the connection may remain idle before
// it is automatically closed.
func (c *connection) SetIdleTimeout(timeout time.Duration) {
	c.conn.SetIdleTimeout(timeout)
}

func (c *connection) sendPings(period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-c.conn.CloseChan():
			return
		case <-t.C:
		}
		if _, err := c.ping(); err != nil {
			//nolint:logcheck // Hopefully this never gets triggered.
			klog.V(3).Infof("SPDY Ping failed: %v", err)
			// Continue, in case this is a transient failure.
			// c.conn.CloseChan above will tell us when the connection is
			// actually closed.
		}
	}
}
