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

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/types"
)

type failureType struct {
	source string
	action string
}

type peerStatus struct {
	lg     *zap.Logger
	local  types.ID
	id     types.ID
	mu     sync.Mutex // protect variables below
	active bool
	since  time.Time
}

func newPeerStatus(lg *zap.Logger, local, id types.ID) *peerStatus {
	if lg == nil {
		lg = zap.NewNop()
	}
	return &peerStatus{lg: lg, local: local, id: id}
}

func (s *peerStatus) activate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		s.lg.Info("peer became active", zap.String("peer-id", s.id.String()))
		s.active = true
		s.since = time.Now()

		activePeers.WithLabelValues(s.local.String(), s.id.String()).Inc()
	}
}

func (s *peerStatus) deactivate(failure failureType, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := fmt.Sprintf("failed to %s %s on %s (%s)", failure.action, s.id, failure.source, reason)
	if s.active {
		s.lg.Warn("peer became inactive (message send to peer failed)", zap.String("peer-id", s.id.String()), zap.Error(errors.New(msg)))
		s.active = false
		s.since = time.Time{}

		activePeers.WithLabelValues(s.local.String(), s.id.String()).Dec()
		disconnectedPeers.WithLabelValues(s.local.String(), s.id.String()).Inc()
		return
	}

	if s.lg != nil {
		s.lg.Debug("peer deactivated again", zap.String("peer-id", s.id.String()), zap.Error(errors.New(msg)))
	}
}

func (s *peerStatus) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *peerStatus) activeSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.since
}
