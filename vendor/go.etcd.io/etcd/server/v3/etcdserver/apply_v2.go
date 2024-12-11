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

package etcdserver

import (
	"encoding/json"
	"net/http"
	"path"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"

	"go.etcd.io/etcd/server/v3/etcdserver/api"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
)

func (s *EtcdServer) applyV2Request(r *RequestV2, shouldApplyV3 membership.ShouldApplyV3) {
	if r.Method != http.MethodPut || (!storeMemberAttributeRegexp.MatchString(r.Path) && r.Path != membership.StoreClusterVersionKey()) {
		s.lg.Panic("detected disallowed v2 WAL for stage --v2-deprecation=write-only", zap.String("method", r.Method))
	}
	if storeMemberAttributeRegexp.MatchString(r.Path) {
		id := membership.MustParseMemberIDFromKey(s.lg, path.Dir(r.Path))
		var attr membership.Attributes
		if err := json.Unmarshal([]byte(r.Val), &attr); err != nil {
			s.lg.Panic("failed to unmarshal", zap.String("value", r.Val), zap.Error(err))
		}
		if s.cluster != nil {
			s.cluster.UpdateAttributes(id, attr, shouldApplyV3)
		}
	}
	// TODO remove v2 version set to avoid the conflict between v2 and v3 in etcd 3.6
	if r.Path == membership.StoreClusterVersionKey() {
		if s.cluster != nil {
			// persist to backend given v2store can be very stale
			s.cluster.SetVersion(semver.Must(semver.NewVersion(r.Val)), api.UpdateCapability, shouldApplyV3)
		}
	}
}
