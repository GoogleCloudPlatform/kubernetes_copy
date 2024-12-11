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

package api

import (
	"sync"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/version"
	serverversion "go.etcd.io/etcd/server/v3/etcdserver/version"
)

type Capability string

const (
	AuthCapability  Capability = "auth"
	V3rpcCapability Capability = "v3rpc"
)

var (
	// capabilityMaps is a static map of version to capability map.
	capabilityMaps = map[string]map[Capability]bool{
		"3.0.0": {AuthCapability: true, V3rpcCapability: true},
		"3.1.0": {AuthCapability: true, V3rpcCapability: true},
		"3.2.0": {AuthCapability: true, V3rpcCapability: true},
		"3.3.0": {AuthCapability: true, V3rpcCapability: true},
		"3.4.0": {AuthCapability: true, V3rpcCapability: true},
		"3.5.0": {AuthCapability: true, V3rpcCapability: true},
		"3.6.0": {AuthCapability: true, V3rpcCapability: true},
	}

	enableMapMu sync.RWMutex
	// enabledMap points to a map in capabilityMaps
	enabledMap map[Capability]bool

	curVersion *semver.Version
)

func init() {
	enabledMap = map[Capability]bool{
		AuthCapability:  true,
		V3rpcCapability: true,
	}
}

// UpdateCapability updates the enabledMap when the cluster version increases.
func UpdateCapability(lg *zap.Logger, v *semver.Version) {
	if v == nil {
		// if recovered but version was never set by cluster
		return
	}
	enableMapMu.Lock()
	if curVersion != nil && !serverversion.IsValidClusterVersionChange(curVersion, v) {
		enableMapMu.Unlock()
		return
	}
	curVersion = v
	enabledMap = capabilityMaps[curVersion.String()]
	enableMapMu.Unlock()

	if lg != nil {
		lg.Info(
			"enabled capabilities for version",
			zap.String("cluster-version", version.Cluster(v.String())),
		)
	}
}

func IsCapabilityEnabled(c Capability) bool {
	enableMapMu.RLock()
	defer enableMapMu.RUnlock()
	if enabledMap == nil {
		return false
	}
	return enabledMap[c]
}

func EnableCapability(c Capability) {
	enableMapMu.Lock()
	defer enableMapMu.Unlock()
	enabledMap[c] = true
}
