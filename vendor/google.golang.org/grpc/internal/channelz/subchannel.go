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

// SubChannel is the channelz representation of a subchannel.
type SubChannel struct {
	Entity
	// ID is the channelz id of this subchannel.
	ID int64
	// RefName is the human readable reference string of this subchannel.
	RefName       string
	closeCalled   bool
	sockets       map[int64]string
	parent        *Channel
	trace         *ChannelTrace
	traceRefCount int32

	ChannelMetrics ChannelMetrics
}

func (sc *SubChannel) String() string {
	return fmt.Sprintf("%s SubChannel #%d", sc.parent, sc.ID)
}

func (sc *SubChannel) id() int64 {
	return sc.ID
}

// Sockets returns a copy of the sockets map associated with the SubChannel.
func (sc *SubChannel) Sockets() map[int64]string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return copyMap(sc.sockets)
}

// Trace returns a copy of the ChannelTrace associated with the SubChannel.
func (sc *SubChannel) Trace() *ChannelTrace {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return sc.trace.copy()
}

func (sc *SubChannel) addChild(id int64, e entry) {
	if v, ok := e.(*Socket); ok && v.SocketType == SocketTypeNormal {
		sc.sockets[id] = v.RefName
	} else {
		logger.Errorf("cannot add a child (id = %d) of type %T to a subChannel", id, e)
	}
}

func (sc *SubChannel) deleteChild(id int64) {
	delete(sc.sockets, id)
	sc.deleteSelfIfReady()
}

func (sc *SubChannel) triggerDelete() {
	sc.closeCalled = true
	sc.deleteSelfIfReady()
}

func (sc *SubChannel) getParentID() int64 {
	return sc.parent.ID
}

// deleteSelfFromTree tries to delete the subchannel from the channelz entry relation tree, which
// means deleting the subchannel reference from its parent's child list.
//
// In order for a subchannel to be deleted from the tree, it must meet the criteria that, removal of
// the corresponding grpc object has been invoked, and the subchannel does not have any children left.
//
// The returned boolean value indicates whether the channel has been successfully deleted from tree.
func (sc *SubChannel) deleteSelfFromTree() (deleted bool) {
	if !sc.closeCalled || len(sc.sockets) != 0 {
		return false
	}
	sc.parent.deleteChild(sc.ID)
	return true
}

// deleteSelfFromMap checks whether it is valid to delete the subchannel from the map, which means
// deleting the subchannel from channelz's tracking entirely. Users can no longer use id to query
// the subchannel, and its memory will be garbage collected.
//
// The trace reference count of the subchannel must be 0 in order to be deleted from the map. This is
// specified in the channel tracing gRFC that as long as some other trace has reference to an entity,
// the trace of the referenced entity must not be deleted. In order to release the resource allocated
// by grpc, the reference to the grpc object is reset to a dummy object.
//
// deleteSelfFromMap must be called after deleteSelfFromTree returns true.
//
// It returns a bool to indicate whether the channel can be safely deleted from map.
func (sc *SubChannel) deleteSelfFromMap() (delete bool) {
	return sc.getTraceRefCount() == 0
}

// deleteSelfIfReady tries to delete the subchannel itself from the channelz database.
// The delete process includes two steps:
//  1. delete the subchannel from the entry relation tree, i.e. delete the subchannel reference from
//     its parent's child list.
//  2. delete the subchannel from the map, i.e. delete the subchannel entirely from channelz. Lookup
//     by id will return entry not found error.
func (sc *SubChannel) deleteSelfIfReady() {
	if !sc.deleteSelfFromTree() {
		return
	}
	if !sc.deleteSelfFromMap() {
		return
	}
	db.deleteEntry(sc.ID)
	sc.trace.clear()
}

func (sc *SubChannel) getChannelTrace() *ChannelTrace {
	return sc.trace
}

func (sc *SubChannel) incrTraceRefCount() {
	atomic.AddInt32(&sc.traceRefCount, 1)
}

func (sc *SubChannel) decrTraceRefCount() {
	atomic.AddInt32(&sc.traceRefCount, -1)
}

func (sc *SubChannel) getTraceRefCount() int {
	i := atomic.LoadInt32(&sc.traceRefCount)
	return int(i)
}

func (sc *SubChannel) getRefName() string {
	return sc.RefName
}
