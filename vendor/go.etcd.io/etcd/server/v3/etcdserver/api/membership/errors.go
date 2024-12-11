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

package membership

import (
	"errors"

	"go.etcd.io/etcd/server/v3/etcdserver/api/v2error"
)

var (
	ErrIDRemoved        = errors.New("membership: ID removed")
	ErrIDExists         = errors.New("membership: ID exists")
	ErrIDNotFound       = errors.New("membership: ID not found")
	ErrPeerURLexists    = errors.New("membership: peerURL exists")
	ErrMemberNotLearner = errors.New("membership: can only promote a learner member")
	ErrTooManyLearners  = errors.New("membership: too many learner members in cluster")
)

func isKeyNotFound(err error) bool {
	var e *v2error.Error
	return errors.As(err, &e) && e.ErrorCode == v2error.EcodeKeyNotFound
}
