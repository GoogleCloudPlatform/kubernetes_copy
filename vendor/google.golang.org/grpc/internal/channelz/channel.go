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

	"google.golang.org/grpc/connectivity"
)

// Channel represents a channel within channelz, which includes metrics and
// internal channelz data, such as channelz id, child list, etc.
type Channel struct {
	Entity
	// ID is the channelz id of this channel.
	ID int64
	// RefName is the human readable reference string of this channel.
	RefName string

	closeCalled bool
	nestedChans map[int64]string
	subChans    map[int64]string
	Parent      *Channel
	trace       *ChannelTrace
	// traceRefCount is the number of trace events that reference this channel.
	// Non-zero traceRefCount means the trace of this channel cannot be deleted.
	traceRefCount int32

	// ChannelMetrics holds connectivity state, target and call metrics for the
	// channel within channelz.
	ChannelMetrics ChannelMetrics
}

// Implemented to make Channel implement the Identifier interface used for
// nesting.
func (c *Channel) channelzIdentifier() {}

// String returns a string representation of the Channel, including its parent
// entity and ID.
func (c *Channel) String() string {
	if c.Parent == nil {
		return fmt.Sprintf("Channel #%d", c.ID)
	}
	return fmt.Sprintf("%s Channel #%d", c.Parent, c.ID)
}

func (c *Channel) id() int64 {
	return c.ID
}

// SubChans returns a copy of the map of sub-channels associated with the
// Channel.
func (c *Channel) SubChans() map[int64]string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return copyMap(c.subChans)
}

// NestedChans returns a copy of the map of nested channels associated with the
// Channel.
func (c *Channel) NestedChans() map[int64]string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return copyMap(c.nestedChans)
}

// Trace returns a copy of the Channel's trace data.
func (c *Channel) Trace() *ChannelTrace {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return c.trace.copy()
}

// ChannelMetrics holds connectivity state, target and call metrics for the
// channel within channelz.
type ChannelMetrics struct {
	// The current connectivity state of the channel.
	State atomic.Pointer[connectivity.State]
	// The target this channel originally tried to connect to.  May be absent
	Target atomic.Pointer[string]
	// The number of calls started on the channel.
	CallsStarted atomic.Int64
	// The number of calls that have completed with an OK status.
	CallsSucceeded atomic.Int64
	// The number of calls that have a completed with a non-OK status.
	CallsFailed atomic.Int64
	// The last time a call was started on the channel.
	LastCallStartedTimestamp atomic.Int64
}

// CopyFrom copies the metrics in o to c.  For testing only.
func (c *ChannelMetrics) CopyFrom(o *ChannelMetrics) {
	c.State.Store(o.State.Load())
	c.Target.Store(o.Target.Load())
	c.CallsStarted.Store(o.CallsStarted.Load())
	c.CallsSucceeded.Store(o.CallsSucceeded.Load())
	c.CallsFailed.Store(o.CallsFailed.Load())
	c.LastCallStartedTimestamp.Store(o.LastCallStartedTimestamp.Load())
}

// Equal returns true iff the metrics of c are the same as the metrics of o.
// For testing only.
func (c *ChannelMetrics) Equal(o any) bool {
	oc, ok := o.(*ChannelMetrics)
	if !ok {
		return false
	}
	if (c.State.Load() == nil) != (oc.State.Load() == nil) {
		return false
	}
	if c.State.Load() != nil && *c.State.Load() != *oc.State.Load() {
		return false
	}
	if (c.Target.Load() == nil) != (oc.Target.Load() == nil) {
		return false
	}
	if c.Target.Load() != nil && *c.Target.Load() != *oc.Target.Load() {
		return false
	}
	return c.CallsStarted.Load() == oc.CallsStarted.Load() &&
		c.CallsFailed.Load() == oc.CallsFailed.Load() &&
		c.CallsSucceeded.Load() == oc.CallsSucceeded.Load() &&
		c.LastCallStartedTimestamp.Load() == oc.LastCallStartedTimestamp.Load()
}

func strFromPointer(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// String returns a string representation of the ChannelMetrics, including its
// state, target, and call metrics.
func (c *ChannelMetrics) String() string {
	return fmt.Sprintf("State: %v, Target: %s, CallsStarted: %v, CallsSucceeded: %v, CallsFailed: %v, LastCallStartedTimestamp: %v",
		c.State.Load(), strFromPointer(c.Target.Load()), c.CallsStarted.Load(), c.CallsSucceeded.Load(), c.CallsFailed.Load(), c.LastCallStartedTimestamp.Load(),
	)
}

// NewChannelMetricForTesting creates a new instance of ChannelMetrics with
// specified initial values for testing purposes.
func NewChannelMetricForTesting(state connectivity.State, target string, started, succeeded, failed, timestamp int64) *ChannelMetrics {
	c := &ChannelMetrics{}
	c.State.Store(&state)
	c.Target.Store(&target)
	c.CallsStarted.Store(started)
	c.CallsSucceeded.Store(succeeded)
	c.CallsFailed.Store(failed)
	c.LastCallStartedTimestamp.Store(timestamp)
	return c
}

func (c *Channel) addChild(id int64, e entry) {
	switch v := e.(type) {
	case *SubChannel:
		c.subChans[id] = v.RefName
	case *Channel:
		c.nestedChans[id] = v.RefName
	default:
		logger.Errorf("cannot add a child (id = %d) of type %T to a channel", id, e)
	}
}

func (c *Channel) deleteChild(id int64) {
	delete(c.subChans, id)
	delete(c.nestedChans, id)
	c.deleteSelfIfReady()
}

func (c *Channel) triggerDelete() {
	c.closeCalled = true
	c.deleteSelfIfReady()
}

func (c *Channel) getParentID() int64 {
	if c.Parent == nil {
		return -1
	}
	return c.Parent.ID
}

// deleteSelfFromTree tries to delete the channel from the channelz entry relation tree, which means
// deleting the channel reference from its parent's child list.
//
// In order for a channel to be deleted from the tree, it must meet the criteria that, removal of the
// corresponding grpc object has been invoked, and the channel does not have any children left.
//
// The returned boolean value indicates whether the channel has been successfully deleted from tree.
func (c *Channel) deleteSelfFromTree() (deleted bool) {
	if !c.closeCalled || len(c.subChans)+len(c.nestedChans) != 0 {
		return false
	}
	// not top channel
	if c.Parent != nil {
		c.Parent.deleteChild(c.ID)
	}
	return true
}

// deleteSelfFromMap checks whether it is valid to delete the channel from the map, which means
// deleting the channel from channelz's tracking entirely. Users can no longer use id to query the
// channel, and its memory will be garbage collected.
//
// The trace reference count of the channel must be 0 in order to be deleted from the map. This is
// specified in the channel tracing gRFC that as long as some other trace has reference to an entity,
// the trace of the referenced entity must not be deleted. In order to release the resource allocated
// by grpc, the reference to the grpc object is reset to a dummy object.
//
// deleteSelfFromMap must be called after deleteSelfFromTree returns true.
//
// It returns a bool to indicate whether the channel can be safely deleted from map.
func (c *Channel) deleteSelfFromMap() (delete bool) {
	return c.getTraceRefCount() == 0
}

// deleteSelfIfReady tries to delete the channel itself from the channelz database.
// The delete process includes two steps:
//  1. delete the channel from the entry relation tree, i.e. delete the channel reference from its
//     parent's child list.
//  2. delete the channel from the map, i.e. delete the channel entirely from channelz. Lookup by id
//     will return entry not found error.
func (c *Channel) deleteSelfIfReady() {
	if !c.deleteSelfFromTree() {
		return
	}
	if !c.deleteSelfFromMap() {
		return
	}
	db.deleteEntry(c.ID)
	c.trace.clear()
}

func (c *Channel) getChannelTrace() *ChannelTrace {
	return c.trace
}

func (c *Channel) incrTraceRefCount() {
	atomic.AddInt32(&c.traceRefCount, 1)
}

func (c *Channel) decrTraceRefCount() {
	atomic.AddInt32(&c.traceRefCount, -1)
}

func (c *Channel) getTraceRefCount() int {
	i := atomic.LoadInt32(&c.traceRefCount)
	return int(i)
}

func (c *Channel) getRefName() string {
	return c.RefName
}
