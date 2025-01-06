/*
 *
 * Copyright 2018 gRPC authors.
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

// Package channelz defines internal APIs for enabling channelz service, entry
// registration/deletion, and accessing channelz data. It also defines channelz
// metric struct formats.
package channelz

import (
	"sync/atomic"
	"time"

	"google.golang.org/grpc/internal"
)

var (
	// IDGen is the global channelz entity ID generator.  It should not be used
	// outside this package except by tests.
	IDGen IDGenerator

	db = newChannelMap()
	// EntriesPerPage defines the number of channelz entries to be shown on a web page.
	EntriesPerPage = 50
	curState       int32
)

// TurnOn turns on channelz data collection.
func TurnOn() {
	atomic.StoreInt32(&curState, 1)
}

func init() {
	internal.ChannelzTurnOffForTesting = func() {
		atomic.StoreInt32(&curState, 0)
	}
}

// IsOn returns whether channelz data collection is on.
func IsOn() bool {
	return atomic.LoadInt32(&curState) == 1
}

// GetTopChannels returns a slice of top channel's ChannelMetric, along with a
// boolean indicating whether there's more top channels to be queried for.
//
// The arg id specifies that only top channel with id at or above it will be
// included in the result. The returned slice is up to a length of the arg
// maxResults or EntriesPerPage if maxResults is zero, and is sorted in ascending
// id order.
func GetTopChannels(id int64, maxResults int) ([]*Channel, bool) {
	return db.getTopChannels(id, maxResults)
}

// GetServers returns a slice of server's ServerMetric, along with a
// boolean indicating whether there's more servers to be queried for.
//
// The arg id specifies that only server with id at or above it will be included
// in the result. The returned slice is up to a length of the arg maxResults or
// EntriesPerPage if maxResults is zero, and is sorted in ascending id order.
func GetServers(id int64, maxResults int) ([]*Server, bool) {
	return db.getServers(id, maxResults)
}

// GetServerSockets returns a slice of server's (identified by id) normal socket's
// SocketMetrics, along with a boolean indicating whether there's more sockets to
// be queried for.
//
// The arg startID specifies that only sockets with id at or above it will be
// included in the result. The returned slice is up to a length of the arg maxResults
// or EntriesPerPage if maxResults is zero, and is sorted in ascending id order.
func GetServerSockets(id int64, startID int64, maxResults int) ([]*Socket, bool) {
	return db.getServerSockets(id, startID, maxResults)
}

// GetChannel returns the Channel for the channel (identified by id).
func GetChannel(id int64) *Channel {
	return db.getChannel(id)
}

// GetSubChannel returns the SubChannel for the subchannel (identified by id).
func GetSubChannel(id int64) *SubChannel {
	return db.getSubChannel(id)
}

// GetSocket returns the Socket for the socket (identified by id).
func GetSocket(id int64) *Socket {
	return db.getSocket(id)
}

// GetServer returns the ServerMetric for the server (identified by id).
func GetServer(id int64) *Server {
	return db.getServer(id)
}

// RegisterChannel registers the given channel c in the channelz database with
// target as its target and reference name, and adds it to the child list of its
// parent.  parent == nil means no parent.
//
// Returns a unique channelz identifier assigned to this channel.
//
// If channelz is not turned ON, the channelz database is not mutated.
func RegisterChannel(parent *Channel, target string) *Channel {
	id := IDGen.genID()

	if !IsOn() {
		return &Channel{ID: id}
	}

	isTopChannel := parent == nil

	cn := &Channel{
		ID:          id,
		RefName:     target,
		nestedChans: make(map[int64]string),
		subChans:    make(map[int64]string),
		Parent:      parent,
		trace:       &ChannelTrace{CreationTime: time.Now(), Events: make([]*traceEvent, 0, getMaxTraceEntry())},
	}
	cn.ChannelMetrics.Target.Store(&target)
	db.addChannel(id, cn, isTopChannel, cn.getParentID())
	return cn
}

// RegisterSubChannel registers the given subChannel c in the channelz database
// with ref as its reference name, and adds it to the child list of its parent
// (identified by pid).
//
// Returns a unique channelz identifier assigned to this subChannel.
//
// If channelz is not turned ON, the channelz database is not mutated.
func RegisterSubChannel(parent *Channel, ref string) *SubChannel {
	id := IDGen.genID()
	sc := &SubChannel{
		ID:      id,
		RefName: ref,
		parent:  parent,
	}

	if !IsOn() {
		return sc
	}

	sc.sockets = make(map[int64]string)
	sc.trace = &ChannelTrace{CreationTime: time.Now(), Events: make([]*traceEvent, 0, getMaxTraceEntry())}
	db.addSubChannel(id, sc, parent.ID)
	return sc
}

// RegisterServer registers the given server s in channelz database. It returns
// the unique channelz tracking id assigned to this server.
//
// If channelz is not turned ON, the channelz database is not mutated.
func RegisterServer(ref string) *Server {
	id := IDGen.genID()
	if !IsOn() {
		return &Server{ID: id}
	}

	svr := &Server{
		RefName:       ref,
		sockets:       make(map[int64]string),
		listenSockets: make(map[int64]string),
		ID:            id,
	}
	db.addServer(id, svr)
	return svr
}

// RegisterSocket registers the given normal socket s in channelz database
// with ref as its reference name, and adds it to the child list of its parent
// (identified by skt.Parent, which must be set). It returns the unique channelz
// tracking id assigned to this normal socket.
//
// If channelz is not turned ON, the channelz database is not mutated.
func RegisterSocket(skt *Socket) *Socket {
	skt.ID = IDGen.genID()
	if IsOn() {
		db.addSocket(skt)
	}
	return skt
}

// RemoveEntry removes an entry with unique channelz tracking id to be id from
// channelz database.
//
// If channelz is not turned ON, this function is a no-op.
func RemoveEntry(id int64) {
	if !IsOn() {
		return
	}
	db.removeEntry(id)
}

// IDGenerator is an incrementing atomic that tracks IDs for channelz entities.
type IDGenerator struct {
	id int64
}

// Reset resets the generated ID back to zero.  Should only be used at
// initialization or by tests sensitive to the ID number.
func (i *IDGenerator) Reset() {
	atomic.StoreInt64(&i.id, 0)
}

func (i *IDGenerator) genID() int64 {
	return atomic.AddInt64(&i.id, 1)
}

// Identifier is an opaque channelz identifier used to expose channelz symbols
// outside of grpc.  Currently only implemented by Channel since no other
// types require exposure outside grpc.
type Identifier interface {
	Entity
	channelzIdentifier()
}
