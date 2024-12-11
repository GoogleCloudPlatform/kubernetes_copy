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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/client/pkg/v3/types"
	pioutil "go.etcd.io/etcd/pkg/v3/ioutil"
	"go.etcd.io/etcd/server/v3/etcdserver/api/snap"
	"go.etcd.io/raft/v3/raftpb"
)

const (
	// connReadLimitByte limits the number of bytes
	// a single read can read out.
	//
	// 64KB should be large enough for not causing
	// throughput bottleneck as well as small enough
	// for not causing a read timeout.
	connReadLimitByte = 64 * 1024

	// snapshotLimitByte limits the snapshot size to 1TB
	snapshotLimitByte = 1 * 1024 * 1024 * 1024 * 1024
)

var (
	RaftPrefix         = "/raft"
	ProbingPrefix      = path.Join(RaftPrefix, "probing")
	RaftStreamPrefix   = path.Join(RaftPrefix, "stream")
	RaftSnapshotPrefix = path.Join(RaftPrefix, "snapshot")

	errIncompatibleVersion = errors.New("incompatible version")
	ErrClusterIDMismatch   = errors.New("cluster ID mismatch")
)

type peerGetter interface {
	Get(id types.ID) Peer
}

type writerToResponse interface {
	WriteTo(w http.ResponseWriter)
}

type pipelineHandler struct {
	lg      *zap.Logger
	localID types.ID
	tr      Transporter
	r       Raft
	cid     types.ID
}

// newPipelineHandler returns a handler for handling raft messages
// from pipeline for RaftPrefix.
//
// The handler reads out the raft message from request body,
// and forwards it to the given raft state machine for processing.
func newPipelineHandler(t *Transport, r Raft, cid types.ID) http.Handler {
	h := &pipelineHandler{
		lg:      t.Logger,
		localID: t.ID,
		tr:      t,
		r:       r,
		cid:     cid,
	}
	if h.lg == nil {
		h.lg = zap.NewNop()
	}
	return h
}

func (h *pipelineHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("X-Etcd-Cluster-ID", h.cid.String())

	if err := checkClusterCompatibilityFromHeader(h.lg, h.localID, r.Header, h.cid); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}

	addRemoteFromRequest(h.tr, r)

	// Limit the data size that could be read from the request body, which ensures that read from
	// connection will not time out accidentally due to possible blocking in underlying implementation.
	limitedr := pioutil.NewLimitedBufferReader(r.Body, connReadLimitByte)
	b, err := io.ReadAll(limitedr)
	if err != nil {
		h.lg.Warn(
			"failed to read Raft message",
			zap.String("local-member-id", h.localID.String()),
			zap.Error(err),
		)
		http.Error(w, "error reading raft message", http.StatusBadRequest)
		recvFailures.WithLabelValues(r.RemoteAddr).Inc()
		return
	}

	var m raftpb.Message
	if err := m.Unmarshal(b); err != nil {
		h.lg.Warn(
			"failed to unmarshal Raft message",
			zap.String("local-member-id", h.localID.String()),
			zap.Error(err),
		)
		http.Error(w, "error unmarshalling raft message", http.StatusBadRequest)
		recvFailures.WithLabelValues(r.RemoteAddr).Inc()
		return
	}

	receivedBytes.WithLabelValues(types.ID(m.From).String()).Add(float64(len(b)))

	if err := h.r.Process(context.TODO(), m); err != nil {
		var writerErr writerToResponse
		switch {
		case errors.As(err, &writerErr):
			writerErr.WriteTo(w)
		default:
			h.lg.Warn(
				"failed to process Raft message",
				zap.String("local-member-id", h.localID.String()),
				zap.Error(err),
			)
			http.Error(w, "error processing raft message", http.StatusInternalServerError)
			w.(http.Flusher).Flush()
			// disconnect the http stream
			panic(err)
		}
		return
	}

	// Write StatusNoContent header after the message has been processed by
	// raft, which facilitates the client to report MsgSnap status.
	w.WriteHeader(http.StatusNoContent)
}

type snapshotHandler struct {
	lg          *zap.Logger
	tr          Transporter
	r           Raft
	snapshotter *snap.Snapshotter

	localID types.ID
	cid     types.ID
}

func newSnapshotHandler(t *Transport, r Raft, snapshotter *snap.Snapshotter, cid types.ID) http.Handler {
	h := &snapshotHandler{
		lg:          t.Logger,
		tr:          t,
		r:           r,
		snapshotter: snapshotter,
		localID:     t.ID,
		cid:         cid,
	}
	if h.lg == nil {
		h.lg = zap.NewNop()
	}
	return h
}

const unknownSnapshotSender = "UNKNOWN_SNAPSHOT_SENDER"

// ServeHTTP serves HTTP request to receive and process snapshot message.
//
// If request sender dies without closing underlying TCP connection,
// the handler will keep waiting for the request body until TCP keepalive
// finds out that the connection is broken after several minutes.
// This is acceptable because
// 1. snapshot messages sent through other TCP connections could still be
// received and processed.
// 2. this case should happen rarely, so no further optimization is done.
func (h *snapshotHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		snapshotReceiveFailures.WithLabelValues(unknownSnapshotSender).Inc()
		return
	}

	w.Header().Set("X-Etcd-Cluster-ID", h.cid.String())

	if err := checkClusterCompatibilityFromHeader(h.lg, h.localID, r.Header, h.cid); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		snapshotReceiveFailures.WithLabelValues(unknownSnapshotSender).Inc()
		return
	}

	addRemoteFromRequest(h.tr, r)

	dec := &messageDecoder{r: r.Body}
	// let snapshots be very large since they can exceed 512MB for large installations
	m, err := dec.decodeLimit(snapshotLimitByte)
	from := types.ID(m.From).String()
	if err != nil {
		msg := fmt.Sprintf("failed to decode raft message (%v)", err)
		h.lg.Warn(
			"failed to decode Raft message",
			zap.String("local-member-id", h.localID.String()),
			zap.String("remote-snapshot-sender-id", from),
			zap.Error(err),
		)
		http.Error(w, msg, http.StatusBadRequest)
		recvFailures.WithLabelValues(r.RemoteAddr).Inc()
		snapshotReceiveFailures.WithLabelValues(from).Inc()
		return
	}

	msgSize := m.Size()
	receivedBytes.WithLabelValues(from).Add(float64(msgSize))

	if m.Type != raftpb.MsgSnap {
		h.lg.Warn(
			"unexpected Raft message type",
			zap.String("local-member-id", h.localID.String()),
			zap.String("remote-snapshot-sender-id", from),
			zap.String("message-type", m.Type.String()),
		)
		http.Error(w, "wrong raft message type", http.StatusBadRequest)
		snapshotReceiveFailures.WithLabelValues(from).Inc()
		return
	}

	snapshotReceiveInflights.WithLabelValues(from).Inc()
	defer func() {
		snapshotReceiveInflights.WithLabelValues(from).Dec()
	}()

	h.lg.Info(
		"receiving database snapshot",
		zap.String("local-member-id", h.localID.String()),
		zap.String("remote-snapshot-sender-id", from),
		zap.Uint64("incoming-snapshot-index", m.Snapshot.Metadata.Index),
		zap.Int("incoming-snapshot-message-size-bytes", msgSize),
		zap.String("incoming-snapshot-message-size", humanize.Bytes(uint64(msgSize))),
	)

	// save incoming database snapshot.

	n, err := h.snapshotter.SaveDBFrom(r.Body, m.Snapshot.Metadata.Index)
	if err != nil {
		msg := fmt.Sprintf("failed to save KV snapshot (%v)", err)
		h.lg.Warn(
			"failed to save incoming database snapshot",
			zap.String("local-member-id", h.localID.String()),
			zap.String("remote-snapshot-sender-id", from),
			zap.Uint64("incoming-snapshot-index", m.Snapshot.Metadata.Index),
			zap.Error(err),
		)
		http.Error(w, msg, http.StatusInternalServerError)
		snapshotReceiveFailures.WithLabelValues(from).Inc()
		return
	}

	receivedBytes.WithLabelValues(from).Add(float64(n))

	downloadTook := time.Since(start)
	h.lg.Info(
		"received and saved database snapshot",
		zap.String("local-member-id", h.localID.String()),
		zap.String("remote-snapshot-sender-id", from),
		zap.Uint64("incoming-snapshot-index", m.Snapshot.Metadata.Index),
		zap.Int64("incoming-snapshot-size-bytes", n),
		zap.String("incoming-snapshot-size", humanize.Bytes(uint64(n))),
		zap.String("download-took", downloadTook.String()),
	)

	if err := h.r.Process(context.TODO(), m); err != nil {
		var writerErr writerToResponse
		switch {
		// Process may return writerToResponse error when doing some
		// additional checks before calling raft.Node.Step.
		case errors.As(err, &writerErr):
			writerErr.WriteTo(w)
		default:
			msg := fmt.Sprintf("failed to process raft message (%v)", err)
			h.lg.Warn(
				"failed to process Raft message",
				zap.String("local-member-id", h.localID.String()),
				zap.String("remote-snapshot-sender-id", from),
				zap.Error(err),
			)
			http.Error(w, msg, http.StatusInternalServerError)
			snapshotReceiveFailures.WithLabelValues(from).Inc()
		}
		return
	}

	// Write StatusNoContent header after the message has been processed by
	// raft, which facilitates the client to report MsgSnap status.
	w.WriteHeader(http.StatusNoContent)

	snapshotReceive.WithLabelValues(from).Inc()
	snapshotReceiveSeconds.WithLabelValues(from).Observe(time.Since(start).Seconds())
}

type streamHandler struct {
	lg         *zap.Logger
	tr         *Transport
	peerGetter peerGetter
	r          Raft
	id         types.ID
	cid        types.ID
}

func newStreamHandler(t *Transport, pg peerGetter, r Raft, id, cid types.ID) http.Handler {
	h := &streamHandler{
		lg:         t.Logger,
		tr:         t,
		peerGetter: pg,
		r:          r,
		id:         id,
		cid:        cid,
	}
	if h.lg == nil {
		h.lg = zap.NewNop()
	}
	return h
}

func (h *streamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("X-Server-Version", version.Version)
	w.Header().Set("X-Etcd-Cluster-ID", h.cid.String())

	if err := checkClusterCompatibilityFromHeader(h.lg, h.tr.ID, r.Header, h.cid); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}

	var t streamType
	switch path.Dir(r.URL.Path) {
	case streamTypeMsgAppV2.endpoint(h.lg):
		t = streamTypeMsgAppV2
	case streamTypeMessage.endpoint(h.lg):
		t = streamTypeMessage
	default:
		h.lg.Debug(
			"ignored unexpected streaming request path",
			zap.String("local-member-id", h.tr.ID.String()),
			zap.String("remote-peer-id-stream-handler", h.id.String()),
			zap.String("path", r.URL.Path),
		)
		http.Error(w, "invalid path", http.StatusNotFound)
		return
	}

	fromStr := path.Base(r.URL.Path)
	from, err := types.IDFromString(fromStr)
	if err != nil {
		h.lg.Warn(
			"failed to parse path into ID",
			zap.String("local-member-id", h.tr.ID.String()),
			zap.String("remote-peer-id-stream-handler", h.id.String()),
			zap.String("path", fromStr),
			zap.Error(err),
		)
		http.Error(w, "invalid from", http.StatusNotFound)
		return
	}
	if h.r.IsIDRemoved(uint64(from)) {
		h.lg.Warn(
			"rejected stream from remote peer because it was removed",
			zap.String("local-member-id", h.tr.ID.String()),
			zap.String("remote-peer-id-stream-handler", h.id.String()),
			zap.String("remote-peer-id-from", from.String()),
		)
		http.Error(w, "removed member", http.StatusGone)
		return
	}
	p := h.peerGetter.Get(from)
	if p == nil {
		// This may happen in following cases:
		// 1. user starts a remote peer that belongs to a different cluster
		// with the same cluster ID.
		// 2. local etcd falls behind of the cluster, and cannot recognize
		// the members that joined after its current progress.
		if urls := r.Header.Get("X-PeerURLs"); urls != "" {
			h.tr.AddRemote(from, strings.Split(urls, ","))
		}
		h.lg.Warn(
			"failed to find remote peer in cluster",
			zap.String("local-member-id", h.tr.ID.String()),
			zap.String("remote-peer-id-stream-handler", h.id.String()),
			zap.String("remote-peer-id-from", from.String()),
			zap.String("cluster-id", h.cid.String()),
		)
		http.Error(w, "error sender not found", http.StatusNotFound)
		return
	}

	wto := h.id.String()
	if gto := r.Header.Get("X-Raft-To"); gto != wto {
		h.lg.Warn(
			"ignored streaming request; ID mismatch",
			zap.String("local-member-id", h.tr.ID.String()),
			zap.String("remote-peer-id-stream-handler", h.id.String()),
			zap.String("remote-peer-id-header", gto),
			zap.String("remote-peer-id-from", from.String()),
			zap.String("cluster-id", h.cid.String()),
		)
		http.Error(w, "to field mismatch", http.StatusPreconditionFailed)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()

	c := newCloseNotifier()
	conn := &outgoingConn{
		t:       t,
		Writer:  w,
		Flusher: w.(http.Flusher),
		Closer:  c,
		localID: h.tr.ID,
		peerID:  from,
	}
	p.attachOutgoingConn(conn)
	<-c.closeNotify()
}

// checkClusterCompatibilityFromHeader checks the cluster compatibility of
// the local member from the given header.
// It checks whether the version of local member is compatible with
// the versions in the header, and whether the cluster ID of local member
// matches the one in the header.
func checkClusterCompatibilityFromHeader(lg *zap.Logger, localID types.ID, header http.Header, cid types.ID) error {
	remoteName := header.Get("X-Server-From")

	remoteServer := serverVersion(header)
	remoteVs := ""
	if remoteServer != nil {
		remoteVs = remoteServer.String()
	}

	remoteMinClusterVer := minClusterVersion(header)
	remoteMinClusterVs := ""
	if remoteMinClusterVer != nil {
		remoteMinClusterVs = remoteMinClusterVer.String()
	}

	localServer, localMinCluster, err := checkVersionCompatibility(remoteName, remoteServer, remoteMinClusterVer)

	localVs := ""
	if localServer != nil {
		localVs = localServer.String()
	}
	localMinClusterVs := ""
	if localMinCluster != nil {
		localMinClusterVs = localMinCluster.String()
	}

	if err != nil {
		lg.Warn(
			"failed to check version compatibility",
			zap.String("local-member-id", localID.String()),
			zap.String("local-member-cluster-id", cid.String()),
			zap.String("local-member-server-version", localVs),
			zap.String("local-member-server-minimum-cluster-version", localMinClusterVs),
			zap.String("remote-peer-server-name", remoteName),
			zap.String("remote-peer-server-version", remoteVs),
			zap.String("remote-peer-server-minimum-cluster-version", remoteMinClusterVs),
			zap.Error(err),
		)
		return errIncompatibleVersion
	}
	if gcid := header.Get("X-Etcd-Cluster-ID"); gcid != cid.String() {
		lg.Warn(
			"request cluster ID mismatch",
			zap.String("local-member-id", localID.String()),
			zap.String("local-member-cluster-id", cid.String()),
			zap.String("local-member-server-version", localVs),
			zap.String("local-member-server-minimum-cluster-version", localMinClusterVs),
			zap.String("remote-peer-server-name", remoteName),
			zap.String("remote-peer-server-version", remoteVs),
			zap.String("remote-peer-server-minimum-cluster-version", remoteMinClusterVs),
			zap.String("remote-peer-cluster-id", gcid),
		)
		return ErrClusterIDMismatch
	}
	return nil
}

type closeNotifier struct {
	done chan struct{}
}

func newCloseNotifier() *closeNotifier {
	return &closeNotifier{
		done: make(chan struct{}),
	}
}

func (n *closeNotifier) Close() error {
	close(n.done)
	return nil
}

func (n *closeNotifier) closeNotify() <-chan struct{} { return n.done }
