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
	"net"
	"sync/atomic"

	"google.golang.org/grpc/credentials"
)

// SocketMetrics defines the struct that the implementor of Socket interface
// should return from ChannelzMetric().
type SocketMetrics struct {
	// The number of streams that have been started.
	StreamsStarted atomic.Int64
	// The number of streams that have ended successfully:
	// On client side, receiving frame with eos bit set.
	// On server side, sending frame with eos bit set.
	StreamsSucceeded atomic.Int64
	// The number of streams that have ended unsuccessfully:
	// On client side, termination without receiving frame with eos bit set.
	// On server side, termination without sending frame with eos bit set.
	StreamsFailed atomic.Int64
	// The number of messages successfully sent on this socket.
	MessagesSent     atomic.Int64
	MessagesReceived atomic.Int64
	// The number of keep alives sent.  This is typically implemented with HTTP/2
	// ping messages.
	KeepAlivesSent atomic.Int64
	// The last time a stream was created by this endpoint.  Usually unset for
	// servers.
	LastLocalStreamCreatedTimestamp atomic.Int64
	// The last time a stream was created by the remote endpoint.  Usually unset
	// for clients.
	LastRemoteStreamCreatedTimestamp atomic.Int64
	// The last time a message was sent by this endpoint.
	LastMessageSentTimestamp atomic.Int64
	// The last time a message was received by this endpoint.
	LastMessageReceivedTimestamp atomic.Int64
}

// EphemeralSocketMetrics are metrics that change rapidly and are tracked
// outside of channelz.
type EphemeralSocketMetrics struct {
	// The amount of window, granted to the local endpoint by the remote endpoint.
	// This may be slightly out of date due to network latency.  This does NOT
	// include stream level or TCP level flow control info.
	LocalFlowControlWindow int64
	// The amount of window, granted to the remote endpoint by the local endpoint.
	// This may be slightly out of date due to network latency.  This does NOT
	// include stream level or TCP level flow control info.
	RemoteFlowControlWindow int64
}

// SocketType represents the type of socket.
type SocketType string

// SocketType can be one of these.
const (
	SocketTypeNormal = "NormalSocket"
	SocketTypeListen = "ListenSocket"
)

// Socket represents a socket within channelz which includes socket
// metrics and data related to socket activity and provides methods
// for managing and interacting with sockets.
type Socket struct {
	Entity
	SocketType       SocketType
	ID               int64
	Parent           Entity
	cm               *channelMap
	SocketMetrics    SocketMetrics
	EphemeralMetrics func() *EphemeralSocketMetrics

	RefName string
	// The locally bound address.  Immutable.
	LocalAddr net.Addr
	// The remote bound address.  May be absent.  Immutable.
	RemoteAddr net.Addr
	// Optional, represents the name of the remote endpoint, if different than
	// the original target name.  Immutable.
	RemoteName string
	// Immutable.
	SocketOptions *SocketOptionData
	// Immutable.
	Security credentials.ChannelzSecurityValue
}

// String returns a string representation of the Socket, including its parent
// entity, socket type, and ID.
func (ls *Socket) String() string {
	return fmt.Sprintf("%s %s #%d", ls.Parent, ls.SocketType, ls.ID)
}

func (ls *Socket) id() int64 {
	return ls.ID
}

func (ls *Socket) addChild(id int64, e entry) {
	logger.Errorf("cannot add a child (id = %d) of type %T to a listen socket", id, e)
}

func (ls *Socket) deleteChild(id int64) {
	logger.Errorf("cannot delete a child (id = %d) from a listen socket", id)
}

func (ls *Socket) triggerDelete() {
	ls.cm.deleteEntry(ls.ID)
	ls.Parent.(entry).deleteChild(ls.ID)
}

func (ls *Socket) deleteSelfIfReady() {
	logger.Errorf("cannot call deleteSelfIfReady on a listen socket")
}

func (ls *Socket) getParentID() int64 {
	return ls.Parent.id()
}
