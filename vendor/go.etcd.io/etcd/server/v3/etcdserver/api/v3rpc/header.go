// Copyright 2016 The etcd Authors
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

package v3rpc

import (
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/etcdserver/apply"
)

type header struct {
	clusterID int64
	memberID  int64
	sg        apply.RaftStatusGetter
	rev       func() int64
}

func newHeader(s *etcdserver.EtcdServer) header {
	return header{
		clusterID: int64(s.Cluster().ID()),
		memberID:  int64(s.MemberID()),
		sg:        s,
		rev:       func() int64 { return s.KV().Rev() },
	}
}

// fill populates pb.ResponseHeader using etcdserver information
func (h *header) fill(rh *pb.ResponseHeader) {
	if rh == nil {
		panic("unexpected nil resp.Header")
	}
	rh.ClusterId = uint64(h.clusterID)
	rh.MemberId = uint64(h.memberID)
	rh.RaftTerm = h.sg.Term()
	if rh.Revision == 0 {
		rh.Revision = h.rev()
	}
}
