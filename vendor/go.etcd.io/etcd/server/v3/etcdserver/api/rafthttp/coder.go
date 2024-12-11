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

package rafthttp

import "go.etcd.io/raft/v3/raftpb"

type encoder interface {
	// encode encodes the given message to an output stream.
	encode(m *raftpb.Message) error
}

type decoder interface {
	// decode decodes the message from an input stream.
	decode() (raftpb.Message, error)
}
