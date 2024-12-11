// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdserver

import (
	"fmt"
	"time"

	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	"go.etcd.io/etcd/server/v3/etcdserver/api/rafthttp"
)

// isConnectedToQuorumSince checks whether the local member is connected to the
// quorum of the cluster since the given time.
func isConnectedToQuorumSince(transport rafthttp.Transporter, since time.Time, self types.ID, members []*membership.Member) bool {
	return numConnectedSince(transport, since, self, members) >= (len(members)/2)+1
}

// isConnectedSince checks whether the local member is connected to the
// remote member since the given time.
func isConnectedSince(transport rafthttp.Transporter, since time.Time, remote types.ID) bool {
	t := transport.ActiveSince(remote)
	return !t.IsZero() && t.Before(since)
}

// isConnectedFullySince checks whether the local member is connected to all
// members in the cluster since the given time.
func isConnectedFullySince(transport rafthttp.Transporter, since time.Time, self types.ID, members []*membership.Member) bool {
	return numConnectedSince(transport, since, self, members) == len(members)
}

// numConnectedSince counts how many members are connected to the local member
// since the given time.
func numConnectedSince(transport rafthttp.Transporter, since time.Time, self types.ID, members []*membership.Member) int {
	connectedNum := 0
	for _, m := range members {
		if m.ID == self || isConnectedSince(transport, since, m.ID) {
			connectedNum++
		}
	}
	return connectedNum
}

// longestConnected chooses the member with longest active-since-time.
// It returns false, if nothing is active.
func longestConnected(tp rafthttp.Transporter, membs []types.ID) (types.ID, bool) {
	var longest types.ID
	var oldest time.Time
	for _, id := range membs {
		tm := tp.ActiveSince(id)
		if tm.IsZero() { // inactive
			continue
		}

		if oldest.IsZero() { // first longest candidate
			oldest = tm
			longest = id
		}

		if tm.Before(oldest) {
			oldest = tm
			longest = id
		}
	}
	if uint64(longest) == 0 {
		return longest, false
	}
	return longest, true
}

type notifier struct {
	c   chan struct{}
	err error
}

func newNotifier() *notifier {
	return &notifier{
		c: make(chan struct{}),
	}
}

func (nc *notifier) notify(err error) {
	nc.err = err
	close(nc.c)
}

// panicAlternativeStringer wraps a fmt.Stringer, and if calling String() panics, calls the alternative instead.
// This is needed to ensure logging slow v2 requests does not panic, which occurs when running integration tests
// with the embedded server with github.com/golang/protobuf v1.4.0+. See https://github.com/etcd-io/etcd/issues/12197.
type panicAlternativeStringer struct {
	stringer    fmt.Stringer
	alternative func() string
}

func (n panicAlternativeStringer) String() (s string) {
	defer func() {
		if err := recover(); err != nil {
			s = n.alternative()
		}
	}()
	s = n.stringer.String()
	return s
}
