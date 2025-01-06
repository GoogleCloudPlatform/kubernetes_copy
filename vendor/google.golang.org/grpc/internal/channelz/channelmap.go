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
	"sort"
	"sync"
	"time"
)

// entry represents a node in the channelz database.
type entry interface {
	// addChild adds a child e, whose channelz id is id to child list
	addChild(id int64, e entry)
	// deleteChild deletes a child with channelz id to be id from child list
	deleteChild(id int64)
	// triggerDelete tries to delete self from channelz database. However, if
	// child list is not empty, then deletion from the database is on hold until
	// the last child is deleted from database.
	triggerDelete()
	// deleteSelfIfReady check whether triggerDelete() has been called before,
	// and whether child list is now empty. If both conditions are met, then
	// delete self from database.
	deleteSelfIfReady()
	// getParentID returns parent ID of the entry. 0 value parent ID means no parent.
	getParentID() int64
	Entity
}

// channelMap is the storage data structure for channelz.
//
// Methods of channelMap can be divided into two categories with respect to
// locking.
//
// 1. Methods acquire the global lock.
// 2. Methods that can only be called when global lock is held.
//
// A second type of method need always to be called inside a first type of method.
type channelMap struct {
	mu               sync.RWMutex
	topLevelChannels map[int64]struct{}
	channels         map[int64]*Channel
	subChannels      map[int64]*SubChannel
	sockets          map[int64]*Socket
	servers          map[int64]*Server
}

func newChannelMap() *channelMap {
	return &channelMap{
		topLevelChannels: make(map[int64]struct{}),
		channels:         make(map[int64]*Channel),
		subChannels:      make(map[int64]*SubChannel),
		sockets:          make(map[int64]*Socket),
		servers:          make(map[int64]*Server),
	}
}

func (c *channelMap) addServer(id int64, s *Server) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.cm = c
	c.servers[id] = s
}

func (c *channelMap) addChannel(id int64, cn *Channel, isTopChannel bool, pid int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cn.trace.cm = c
	c.channels[id] = cn
	if isTopChannel {
		c.topLevelChannels[id] = struct{}{}
	} else if p := c.channels[pid]; p != nil {
		p.addChild(id, cn)
	} else {
		logger.Infof("channel %d references invalid parent ID %d", id, pid)
	}
}

func (c *channelMap) addSubChannel(id int64, sc *SubChannel, pid int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sc.trace.cm = c
	c.subChannels[id] = sc
	if p := c.channels[pid]; p != nil {
		p.addChild(id, sc)
	} else {
		logger.Infof("subchannel %d references invalid parent ID %d", id, pid)
	}
}

func (c *channelMap) addSocket(s *Socket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.cm = c
	c.sockets[s.ID] = s
	if s.Parent == nil {
		logger.Infof("normal socket %d has no parent", s.ID)
	}
	s.Parent.(entry).addChild(s.ID, s)
}

// removeEntry triggers the removal of an entry, which may not indeed delete the
// entry, if it has to wait on the deletion of its children and until no other
// entity's channel trace references it.  It may lead to a chain of entry
// deletion. For example, deleting the last socket of a gracefully shutting down
// server will lead to the server being also deleted.
func (c *channelMap) removeEntry(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.findEntry(id).triggerDelete()
}

// tracedChannel represents tracing operations which are present on both
// channels and subChannels.
type tracedChannel interface {
	getChannelTrace() *ChannelTrace
	incrTraceRefCount()
	decrTraceRefCount()
	getRefName() string
}

// c.mu must be held by the caller
func (c *channelMap) decrTraceRefCount(id int64) {
	e := c.findEntry(id)
	if v, ok := e.(tracedChannel); ok {
		v.decrTraceRefCount()
		e.deleteSelfIfReady()
	}
}

// c.mu must be held by the caller.
func (c *channelMap) findEntry(id int64) entry {
	if v, ok := c.channels[id]; ok {
		return v
	}
	if v, ok := c.subChannels[id]; ok {
		return v
	}
	if v, ok := c.servers[id]; ok {
		return v
	}
	if v, ok := c.sockets[id]; ok {
		return v
	}
	return &dummyEntry{idNotFound: id}
}

// c.mu must be held by the caller
//
// deleteEntry deletes an entry from the channelMap. Before calling this method,
// caller must check this entry is ready to be deleted, i.e removeEntry() has
// been called on it, and no children still exist.
func (c *channelMap) deleteEntry(id int64) entry {
	if v, ok := c.sockets[id]; ok {
		delete(c.sockets, id)
		return v
	}
	if v, ok := c.subChannels[id]; ok {
		delete(c.subChannels, id)
		return v
	}
	if v, ok := c.channels[id]; ok {
		delete(c.channels, id)
		delete(c.topLevelChannels, id)
		return v
	}
	if v, ok := c.servers[id]; ok {
		delete(c.servers, id)
		return v
	}
	return &dummyEntry{idNotFound: id}
}

func (c *channelMap) traceEvent(id int64, desc *TraceEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	child := c.findEntry(id)
	childTC, ok := child.(tracedChannel)
	if !ok {
		return
	}
	childTC.getChannelTrace().append(&traceEvent{Desc: desc.Desc, Severity: desc.Severity, Timestamp: time.Now()})
	if desc.Parent != nil {
		parent := c.findEntry(child.getParentID())
		var chanType RefChannelType
		switch child.(type) {
		case *Channel:
			chanType = RefChannel
		case *SubChannel:
			chanType = RefSubChannel
		}
		if parentTC, ok := parent.(tracedChannel); ok {
			parentTC.getChannelTrace().append(&traceEvent{
				Desc:      desc.Parent.Desc,
				Severity:  desc.Parent.Severity,
				Timestamp: time.Now(),
				RefID:     id,
				RefName:   childTC.getRefName(),
				RefType:   chanType,
			})
			childTC.incrTraceRefCount()
		}
	}
}

type int64Slice []int64

func (s int64Slice) Len() int           { return len(s) }
func (s int64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s int64Slice) Less(i, j int) bool { return s[i] < s[j] }

func copyMap(m map[int64]string) map[int64]string {
	n := make(map[int64]string)
	for k, v := range m {
		n[k] = v
	}
	return n
}

func (c *channelMap) getTopChannels(id int64, maxResults int) ([]*Channel, bool) {
	if maxResults <= 0 {
		maxResults = EntriesPerPage
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	l := int64(len(c.topLevelChannels))
	ids := make([]int64, 0, l)

	for k := range c.topLevelChannels {
		ids = append(ids, k)
	}
	sort.Sort(int64Slice(ids))
	idx := sort.Search(len(ids), func(i int) bool { return ids[i] >= id })
	end := true
	var t []*Channel
	for _, v := range ids[idx:] {
		if len(t) == maxResults {
			end = false
			break
		}
		if cn, ok := c.channels[v]; ok {
			t = append(t, cn)
		}
	}
	return t, end
}

func (c *channelMap) getServers(id int64, maxResults int) ([]*Server, bool) {
	if maxResults <= 0 {
		maxResults = EntriesPerPage
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]int64, 0, len(c.servers))
	for k := range c.servers {
		ids = append(ids, k)
	}
	sort.Sort(int64Slice(ids))
	idx := sort.Search(len(ids), func(i int) bool { return ids[i] >= id })
	end := true
	var s []*Server
	for _, v := range ids[idx:] {
		if len(s) == maxResults {
			end = false
			break
		}
		if svr, ok := c.servers[v]; ok {
			s = append(s, svr)
		}
	}
	return s, end
}

func (c *channelMap) getServerSockets(id int64, startID int64, maxResults int) ([]*Socket, bool) {
	if maxResults <= 0 {
		maxResults = EntriesPerPage
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	svr, ok := c.servers[id]
	if !ok {
		// server with id doesn't exist.
		return nil, true
	}
	svrskts := svr.sockets
	ids := make([]int64, 0, len(svrskts))
	sks := make([]*Socket, 0, min(len(svrskts), maxResults))
	for k := range svrskts {
		ids = append(ids, k)
	}
	sort.Sort(int64Slice(ids))
	idx := sort.Search(len(ids), func(i int) bool { return ids[i] >= startID })
	end := true
	for _, v := range ids[idx:] {
		if len(sks) == maxResults {
			end = false
			break
		}
		if ns, ok := c.sockets[v]; ok {
			sks = append(sks, ns)
		}
	}
	return sks, end
}

func (c *channelMap) getChannel(id int64) *Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channels[id]
}

func (c *channelMap) getSubChannel(id int64) *SubChannel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.subChannels[id]
}

func (c *channelMap) getSocket(id int64) *Socket {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sockets[id]
}

func (c *channelMap) getServer(id int64) *Server {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.servers[id]
}

type dummyEntry struct {
	// dummyEntry is a fake entry to handle entry not found case.
	idNotFound int64
	Entity
}

func (d *dummyEntry) String() string {
	return fmt.Sprintf("non-existent entity #%d", d.idNotFound)
}

func (d *dummyEntry) ID() int64 { return d.idNotFound }

func (d *dummyEntry) addChild(id int64, e entry) {
	// Note: It is possible for a normal program to reach here under race
	// condition.  For example, there could be a race between ClientConn.Close()
	// info being propagated to addrConn and http2Client. ClientConn.Close()
	// cancel the context and result in http2Client to error. The error info is
	// then caught by transport monitor and before addrConn.tearDown() is called
	// in side ClientConn.Close(). Therefore, the addrConn will create a new
	// transport. And when registering the new transport in channelz, its parent
	// addrConn could have already been torn down and deleted from channelz
	// tracking, and thus reach the code here.
	logger.Infof("attempt to add child of type %T with id %d to a parent (id=%d) that doesn't currently exist", e, id, d.idNotFound)
}

func (d *dummyEntry) deleteChild(id int64) {
	// It is possible for a normal program to reach here under race condition.
	// Refer to the example described in addChild().
	logger.Infof("attempt to delete child with id %d from a parent (id=%d) that doesn't currently exist", id, d.idNotFound)
}

func (d *dummyEntry) triggerDelete() {
	logger.Warningf("attempt to delete an entry (id=%d) that doesn't currently exist", d.idNotFound)
}

func (*dummyEntry) deleteSelfIfReady() {
	// code should not reach here. deleteSelfIfReady is always called on an existing entry.
}

func (*dummyEntry) getParentID() int64 {
	return 0
}

// Entity is implemented by all channelz types.
type Entity interface {
	isEntity()
	fmt.Stringer
	id() int64
}
