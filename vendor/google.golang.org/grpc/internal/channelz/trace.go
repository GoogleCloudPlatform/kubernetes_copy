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

package channelz

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/grpclog"
)

const (
	defaultMaxTraceEntry int32 = 30
)

var maxTraceEntry = defaultMaxTraceEntry

// SetMaxTraceEntry sets maximum number of trace entries per entity (i.e.
// channel/subchannel).  Setting it to 0 will disable channel tracing.
func SetMaxTraceEntry(i int32) {
	atomic.StoreInt32(&maxTraceEntry, i)
}

// ResetMaxTraceEntryToDefault resets the maximum number of trace entries per
// entity to default.
func ResetMaxTraceEntryToDefault() {
	atomic.StoreInt32(&maxTraceEntry, defaultMaxTraceEntry)
}

func getMaxTraceEntry() int {
	i := atomic.LoadInt32(&maxTraceEntry)
	return int(i)
}

// traceEvent is an internal representation of a single trace event
type traceEvent struct {
	// Desc is a simple description of the trace event.
	Desc string
	// Severity states the severity of this trace event.
	Severity Severity
	// Timestamp is the event time.
	Timestamp time.Time
	// RefID is the id of the entity that gets referenced in the event. RefID is 0 if no other entity is
	// involved in this event.
	// e.g. SubChannel (id: 4[]) Created. --> RefID = 4, RefName = "" (inside [])
	RefID int64
	// RefName is the reference name for the entity that gets referenced in the event.
	RefName string
	// RefType indicates the referenced entity type, i.e Channel or SubChannel.
	RefType RefChannelType
}

// TraceEvent is what the caller of AddTraceEvent should provide to describe the
// event to be added to the channel trace.
//
// The Parent field is optional. It is used for an event that will be recorded
// in the entity's parent trace.
type TraceEvent struct {
	Desc     string
	Severity Severity
	Parent   *TraceEvent
}

// ChannelTrace provides tracing information for a channel.
// It tracks various events and metadata related to the channel's lifecycle
// and operations.
type ChannelTrace struct {
	cm          *channelMap
	clearCalled bool
	// The time when the trace was created.
	CreationTime time.Time
	// A counter for the number of events recorded in the
	// trace.
	EventNum int64
	mu       sync.Mutex
	// A slice of traceEvent pointers representing the events recorded for
	// this channel.
	Events []*traceEvent
}

func (c *ChannelTrace) copy() *ChannelTrace {
	return &ChannelTrace{
		CreationTime: c.CreationTime,
		EventNum:     c.EventNum,
		Events:       append(([]*traceEvent)(nil), c.Events...),
	}
}

func (c *ChannelTrace) append(e *traceEvent) {
	c.mu.Lock()
	if len(c.Events) == getMaxTraceEntry() {
		del := c.Events[0]
		c.Events = c.Events[1:]
		if del.RefID != 0 {
			// start recursive cleanup in a goroutine to not block the call originated from grpc.
			go func() {
				// need to acquire c.cm.mu lock to call the unlocked attemptCleanup func.
				c.cm.mu.Lock()
				c.cm.decrTraceRefCount(del.RefID)
				c.cm.mu.Unlock()
			}()
		}
	}
	e.Timestamp = time.Now()
	c.Events = append(c.Events, e)
	c.EventNum++
	c.mu.Unlock()
}

func (c *ChannelTrace) clear() {
	if c.clearCalled {
		return
	}
	c.clearCalled = true
	c.mu.Lock()
	for _, e := range c.Events {
		if e.RefID != 0 {
			// caller should have already held the c.cm.mu lock.
			c.cm.decrTraceRefCount(e.RefID)
		}
	}
	c.mu.Unlock()
}

// Severity is the severity level of a trace event.
// The canonical enumeration of all valid values is here:
// https://github.com/grpc/grpc-proto/blob/9b13d199cc0d4703c7ea26c9c330ba695866eb23/grpc/channelz/v1/channelz.proto#L126.
type Severity int

const (
	// CtUnknown indicates unknown severity of a trace event.
	CtUnknown Severity = iota
	// CtInfo indicates info level severity of a trace event.
	CtInfo
	// CtWarning indicates warning level severity of a trace event.
	CtWarning
	// CtError indicates error level severity of a trace event.
	CtError
)

// RefChannelType is the type of the entity being referenced in a trace event.
type RefChannelType int

const (
	// RefUnknown indicates an unknown entity type, the zero value for this type.
	RefUnknown RefChannelType = iota
	// RefChannel indicates the referenced entity is a Channel.
	RefChannel
	// RefSubChannel indicates the referenced entity is a SubChannel.
	RefSubChannel
	// RefServer indicates the referenced entity is a Server.
	RefServer
	// RefListenSocket indicates the referenced entity is a ListenSocket.
	RefListenSocket
	// RefNormalSocket indicates the referenced entity is a NormalSocket.
	RefNormalSocket
)

var refChannelTypeToString = map[RefChannelType]string{
	RefUnknown:      "Unknown",
	RefChannel:      "Channel",
	RefSubChannel:   "SubChannel",
	RefServer:       "Server",
	RefListenSocket: "ListenSocket",
	RefNormalSocket: "NormalSocket",
}

// String returns a string representation of the RefChannelType
func (r RefChannelType) String() string {
	return refChannelTypeToString[r]
}

// AddTraceEvent adds trace related to the entity with specified id, using the
// provided TraceEventDesc.
//
// If channelz is not turned ON, this will simply log the event descriptions.
func AddTraceEvent(l grpclog.DepthLoggerV2, e Entity, depth int, desc *TraceEvent) {
	// Log only the trace description associated with the bottom most entity.
	d := fmt.Sprintf("[%s]%s", e, desc.Desc)
	switch desc.Severity {
	case CtUnknown, CtInfo:
		l.InfoDepth(depth+1, d)
	case CtWarning:
		l.WarningDepth(depth+1, d)
	case CtError:
		l.ErrorDepth(depth+1, d)
	}

	if getMaxTraceEntry() == 0 {
		return
	}
	if IsOn() {
		db.traceEvent(e.id(), desc)
	}
}
