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

package etcdhttp

import (
	"encoding/json"
	errorspkg "errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/etcdserver/api"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	"go.etcd.io/etcd/server/v3/etcdserver/api/rafthttp"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	"go.etcd.io/etcd/server/v3/lease/leasehttp"
)

const (
	peerMembersPath         = "/members"
	peerMemberPromotePrefix = "/members/promote/"
)

// NewPeerHandler generates an http.Handler to handle etcd peer requests.
func NewPeerHandler(lg *zap.Logger, s etcdserver.ServerPeerV2) http.Handler {
	return newPeerHandler(lg, s, s.RaftHandler(), s.LeaseHandler(), s.HashKVHandler(), s.DowngradeEnabledHandler())
}

func newPeerHandler(
	lg *zap.Logger,
	s etcdserver.Server,
	raftHandler http.Handler,
	leaseHandler http.Handler,
	hashKVHandler http.Handler,
	downgradeEnabledHandler http.Handler,
) http.Handler {
	if lg == nil {
		lg = zap.NewNop()
	}
	peerMembersHandler := newPeerMembersHandler(lg, s.Cluster())
	peerMemberPromoteHandler := newPeerMemberPromoteHandler(lg, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/", http.NotFound)
	mux.Handle(rafthttp.RaftPrefix, raftHandler)
	mux.Handle(rafthttp.RaftPrefix+"/", raftHandler)
	mux.Handle(peerMembersPath, peerMembersHandler)
	mux.Handle(peerMemberPromotePrefix, peerMemberPromoteHandler)
	if leaseHandler != nil {
		mux.Handle(leasehttp.LeasePrefix, leaseHandler)
		mux.Handle(leasehttp.LeaseInternalPrefix, leaseHandler)
	}
	if downgradeEnabledHandler != nil {
		mux.Handle(etcdserver.DowngradeEnabledPath, downgradeEnabledHandler)
	}
	if hashKVHandler != nil {
		mux.Handle(etcdserver.PeerHashKVPath, hashKVHandler)
	}
	mux.HandleFunc(versionPath, versionHandler(s, serveVersion))
	return mux
}

func newPeerMembersHandler(lg *zap.Logger, cluster api.Cluster) http.Handler {
	return &peerMembersHandler{
		lg:      lg,
		cluster: cluster,
	}
}

type peerMembersHandler struct {
	lg      *zap.Logger
	cluster api.Cluster
}

func newPeerMemberPromoteHandler(lg *zap.Logger, s etcdserver.Server) http.Handler {
	return &peerMemberPromoteHandler{
		lg:      lg,
		cluster: s.Cluster(),
		server:  s,
	}
}

type peerMemberPromoteHandler struct {
	lg      *zap.Logger
	cluster api.Cluster
	server  etcdserver.Server
}

func (h *peerMembersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, "GET") {
		return
	}
	w.Header().Set("X-Etcd-Cluster-ID", h.cluster.ID().String())

	if r.URL.Path != peerMembersPath {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	ms := h.cluster.Members()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(ms); err != nil {
		h.lg.Warn("failed to encode membership members", zap.Error(err))
	}
}

func (h *peerMemberPromoteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, "POST") {
		return
	}
	w.Header().Set("X-Etcd-Cluster-ID", h.cluster.ID().String())

	if !strings.HasPrefix(r.URL.Path, peerMemberPromotePrefix) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, peerMemberPromotePrefix)
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("member %s not found in cluster", idStr), http.StatusNotFound)
		return
	}

	resp, err := h.server.PromoteMember(r.Context(), id)
	if err != nil {
		switch {
		case errorspkg.Is(err, membership.ErrIDNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errorspkg.Is(err, membership.ErrMemberNotLearner):
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
		case errorspkg.Is(err, errors.ErrLearnerNotReady):
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
		default:
			writeError(h.lg, w, r, err)
		}
		h.lg.Warn(
			"failed to promote a member",
			zap.String("member-id", types.ID(id).String()),
			zap.Error(err),
		)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.lg.Warn("failed to encode members response", zap.Error(err))
	}
}
