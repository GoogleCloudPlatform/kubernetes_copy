/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package channelz

import (
	"fmt"
	"sync/atomic"
)

// Server is the channelz representation of a server.
type Server struct {
	Entity
	ID      int64
	RefName string

	ServerMetrics ServerMetrics

	closeCalled   bool
	sockets       map[int64]string
	listenSockets map[int64]string
	cm            *channelMap
}

// ServerMetrics defines a struct containing metrics for servers.
type ServerMetrics struct {
	// The number of incoming calls started on the server.
	CallsStarted atomic.Int64
	// The number of incoming calls that have completed with an OK status.
	CallsSucceeded atomic.Int64
	// The number of incoming calls that have a completed with a non-OK status.
	CallsFailed atomic.Int64
	// The last time a call was started on the server.
	LastCallStartedTimestamp atomic.Int64
}

// NewServerMetricsForTesting returns an initialized ServerMetrics.
func NewServerMetricsForTesting(started, succeeded, failed, timestamp int64) *ServerMetrics {
	sm := &ServerMetrics{}
	sm.CallsStarted.Store(started)
	sm.CallsSucceeded.Store(succeeded)
	sm.CallsFailed.Store(failed)
	sm.LastCallStartedTimestamp.Store(timestamp)
	return sm
}

// CopyFrom copies the metrics data from the provided ServerMetrics
// instance into the current instance.
func (sm *ServerMetrics) CopyFrom(o *ServerMetrics) {
	sm.CallsStarted.Store(o.CallsStarted.Load())
	sm.CallsSucceeded.Store(o.CallsSucceeded.Load())
	sm.CallsFailed.Store(o.CallsFailed.Load())
	sm.LastCallStartedTimestamp.Store(o.LastCallStartedTimestamp.Load())
}

// ListenSockets returns the listening sockets for s.
func (s *Server) ListenSockets() map[int64]string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return copyMap(s.listenSockets)
}

// String returns a printable description of s.
func (s *Server) String() string {
	return fmt.Sprintf("Server #%d", s.ID)
}

func (s *Server) id() int64 {
	return s.ID
}

func (s *Server) addChild(id int64, e entry) {
	switch v := e.(type) {
	case *Socket:
		switch v.SocketType {
		case SocketTypeNormal:
			s.sockets[id] = v.RefName
		case SocketTypeListen:
			s.listenSockets[id] = v.RefName
		}
	default:
		logger.Errorf("cannot add a child (id = %d) of type %T to a server", id, e)
	}
}

func (s *Server) deleteChild(id int64) {
	delete(s.sockets, id)
	delete(s.listenSockets, id)
	s.deleteSelfIfReady()
}

func (s *Server) triggerDelete() {
	s.closeCalled = true
	s.deleteSelfIfReady()
}

func (s *Server) deleteSelfIfReady() {
	if !s.closeCalled || len(s.sockets)+len(s.listenSockets) != 0 {
		return
	}
	s.cm.deleteEntry(s.ID)
}

func (s *Server) getParentID() int64 {
	return 0
}
