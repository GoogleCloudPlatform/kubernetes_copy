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
	"context"
	"encoding/json"
	errorspkg "errors"
	"expvar"
	"fmt"
	"math"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-semver/semver"
	humanize "github.com/dustin/go-humanize"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/membershippb"
	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/client/pkg/v3/verify"
	"go.etcd.io/etcd/pkg/v3/featuregate"
	"go.etcd.io/etcd/pkg/v3/idutil"
	"go.etcd.io/etcd/pkg/v3/notify"
	"go.etcd.io/etcd/pkg/v3/pbutil"
	"go.etcd.io/etcd/pkg/v3/runtime"
	"go.etcd.io/etcd/pkg/v3/schedule"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/pkg/v3/wait"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/config"
	"go.etcd.io/etcd/server/v3/etcdserver/api"
	httptypes "go.etcd.io/etcd/server/v3/etcdserver/api/etcdhttp/types"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	"go.etcd.io/etcd/server/v3/etcdserver/api/rafthttp"
	"go.etcd.io/etcd/server/v3/etcdserver/api/snap"
	stats "go.etcd.io/etcd/server/v3/etcdserver/api/v2stats"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v2store"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3alarm"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3compactor"
	"go.etcd.io/etcd/server/v3/etcdserver/apply"
	"go.etcd.io/etcd/server/v3/etcdserver/cindex"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	"go.etcd.io/etcd/server/v3/etcdserver/txn"
	serverversion "go.etcd.io/etcd/server/v3/etcdserver/version"
	"go.etcd.io/etcd/server/v3/lease"
	"go.etcd.io/etcd/server/v3/lease/leasehttp"
	serverstorage "go.etcd.io/etcd/server/v3/storage"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
	"go.etcd.io/etcd/server/v3/storage/schema"
	"go.etcd.io/raft/v3"
	"go.etcd.io/raft/v3/raftpb"
)

const (
	DefaultSnapshotCount = 10000

	// DefaultSnapshotCatchUpEntries is the number of entries for a slow follower
	// to catch-up after compacting the raft storage entries.
	// We expect the follower has a millisecond level latency with the leader.
	// The max throughput is around 10K. Keep a 5K entries is enough for helping
	// follower to catch up.
	DefaultSnapshotCatchUpEntries uint64 = 5000

	StoreClusterPrefix = "/0"
	StoreKeysPrefix    = "/1"

	// HealthInterval is the minimum time the cluster should be healthy
	// before accepting add and delete member requests.
	HealthInterval = 5 * time.Second

	purgeFileInterval = 30 * time.Second

	// max number of in-flight snapshot messages etcdserver allows to have
	// This number is more than enough for most clusters with 5 machines.
	maxInFlightMsgSnap = 16

	releaseDelayAfterSnapshot = 30 * time.Second

	// maxPendingRevokes is the maximum number of outstanding expired lease revocations.
	maxPendingRevokes = 16

	recommendedMaxRequestBytes = 10 * 1024 * 1024

	// readyPercentThreshold is a threshold used to determine
	// whether a learner is ready for a transition into a full voting member or not.
	readyPercentThreshold = 0.9

	DowngradeEnabledPath = "/downgrade/enabled"
)

var (
	// monitorVersionInterval should be smaller than the timeout
	// on the connection. Or we will not be able to reuse the connection
	// (since it will timeout).
	monitorVersionInterval = rafthttp.ConnWriteTimeout - time.Second

	recommendedMaxRequestBytesString = humanize.Bytes(uint64(recommendedMaxRequestBytes))
	storeMemberAttributeRegexp       = regexp.MustCompile(path.Join(membership.StoreMembersPrefix, "[[:xdigit:]]{1,16}", "attributes"))
)

func init() {
	expvar.Publish(
		"file_descriptor_limit",
		expvar.Func(
			func() any {
				n, _ := runtime.FDLimit()
				return n
			},
		),
	)
}

type Response struct {
	Term    uint64
	Index   uint64
	Event   *v2store.Event
	Watcher v2store.Watcher
	Err     error
}

type ServerV2 interface {
	Server
	Leader() types.ID

	ClientCertAuthEnabled() bool
}

type ServerV3 interface {
	Server
	apply.RaftStatusGetter
}

func (s *EtcdServer) ClientCertAuthEnabled() bool { return s.Cfg.ClientCertAuthEnabled }

type Server interface {
	// AddMember attempts to add a member into the cluster. It will return
	// ErrIDRemoved if member ID is removed from the cluster, or return
	// ErrIDExists if member ID exists in the cluster.
	AddMember(ctx context.Context, memb membership.Member) ([]*membership.Member, error)
	// RemoveMember attempts to remove a member from the cluster. It will
	// return ErrIDRemoved if member ID is removed from the cluster, or return
	// ErrIDNotFound if member ID is not in the cluster.
	RemoveMember(ctx context.Context, id uint64) ([]*membership.Member, error)
	// UpdateMember attempts to update an existing member in the cluster. It will
	// return ErrIDNotFound if the member ID does not exist.
	UpdateMember(ctx context.Context, updateMemb membership.Member) ([]*membership.Member, error)
	// PromoteMember attempts to promote a non-voting node to a voting node. It will
	// return ErrIDNotFound if the member ID does not exist.
	// return ErrLearnerNotReady if the member are not ready.
	// return ErrMemberNotLearner if the member is not a learner.
	PromoteMember(ctx context.Context, id uint64) ([]*membership.Member, error)

	// ClusterVersion is the cluster-wide minimum major.minor version.
	// Cluster version is set to the min version that an etcd member is
	// compatible with when first bootstrap.
	//
	// ClusterVersion is nil until the cluster is bootstrapped (has a quorum).
	//
	// During a rolling upgrades, the ClusterVersion will be updated
	// automatically after a sync. (5 second by default)
	//
	// The API/raft component can utilize ClusterVersion to determine if
	// it can accept a client request or a raft RPC.
	// NOTE: ClusterVersion might be nil when etcd 2.1 works with etcd 2.0 and
	// the leader is etcd 2.0. etcd 2.0 leader will not update clusterVersion since
	// this feature is introduced post 2.0.
	ClusterVersion() *semver.Version
	// StorageVersion is the storage schema version. It's supported starting
	// from 3.6.
	StorageVersion() *semver.Version
	Cluster() api.Cluster
	Alarms() []*pb.AlarmMember

	// LeaderChangedNotify returns a channel for application level code to be notified
	// when etcd leader changes, this function is intend to be used only in application
	// which embed etcd.
	// Caution:
	// 1. the returned channel is being closed when the leadership changes.
	// 2. so the new channel needs to be obtained for each raft term.
	// 3. user can loose some consecutive channel changes using this API.
	LeaderChangedNotify() <-chan struct{}
}

// EtcdServer is the production implementation of the Server interface
type EtcdServer struct {
	// inflightSnapshots holds count the number of snapshots currently inflight.
	inflightSnapshots int64  // must use atomic operations to access; keep 64-bit aligned.
	appliedIndex      uint64 // must use atomic operations to access; keep 64-bit aligned.
	committedIndex    uint64 // must use atomic operations to access; keep 64-bit aligned.
	term              uint64 // must use atomic operations to access; keep 64-bit aligned.
	lead              uint64 // must use atomic operations to access; keep 64-bit aligned.

	consistIndex cindex.ConsistentIndexer // consistIndex is used to get/set/save consistentIndex
	r            raftNode                 // uses 64-bit atomics; keep 64-bit aligned.

	readych chan struct{}
	Cfg     config.ServerConfig

	lgMu *sync.RWMutex
	lg   *zap.Logger

	w wait.Wait

	readMu sync.RWMutex
	// read routine notifies etcd server that it waits for reading by sending an empty struct to
	// readwaitC
	readwaitc chan struct{}
	// readNotifier is used to notify the read routine that it can process the request
	// when there is no error
	readNotifier *notifier

	// stop signals the run goroutine should shutdown.
	stop chan struct{}
	// stopping is closed by run goroutine on shutdown.
	stopping chan struct{}
	// done is closed when all goroutines from start() complete.
	done chan struct{}
	// leaderChanged is used to notify the linearizable read loop to drop the old read requests.
	leaderChanged *notify.Notifier

	errorc     chan error
	memberID   types.ID
	attributes membership.Attributes

	cluster *membership.RaftCluster

	v2store     v2store.Store
	snapshotter *snap.Snapshotter

	uberApply apply.UberApplier

	applyWait wait.WaitTime

	kv         mvcc.WatchableKV
	lessor     lease.Lessor
	bemu       sync.RWMutex
	be         backend.Backend
	beHooks    *serverstorage.BackendHooks
	authStore  auth.AuthStore
	alarmStore *v3alarm.AlarmStore

	stats  *stats.ServerStats
	lstats *stats.LeaderStats

	SyncTicker *time.Ticker
	// compactor is used to auto-compact the KV.
	compactor v3compactor.Compactor

	// peerRt used to send requests (version, lease) to peers.
	peerRt   http.RoundTripper
	reqIDGen *idutil.Generator

	// wgMu blocks concurrent waitgroup mutation while server stopping
	wgMu sync.RWMutex
	// wg is used to wait for the goroutines that depends on the server state
	// to exit when stopping the server.
	wg sync.WaitGroup

	// ctx is used for etcd-initiated requests that may need to be canceled
	// on etcd server shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	leadTimeMu      sync.RWMutex
	leadElectedTime time.Time

	firstCommitInTerm     *notify.Notifier
	clusterVersionChanged *notify.Notifier

	*AccessController
	// forceSnapshot can force snapshot be triggered after apply, independent of the snapshotCount.
	// Should only be set within apply code path. Used to force snapshot after cluster version downgrade.
	forceSnapshot     bool
	corruptionChecker CorruptionChecker
}

// NewServer creates a new EtcdServer from the supplied configuration. The
// configuration is considered static for the lifetime of the EtcdServer.
func NewServer(cfg config.ServerConfig) (srv *EtcdServer, err error) {
	b, err := bootstrap(cfg)
	if err != nil {
		cfg.Logger.Error("bootstrap failed", zap.Error(err))
		return nil, err
	}
	cfg.Logger.Info("bootstrap successfully")

	defer func() {
		if err != nil {
			b.Close()
		}
	}()

	sstats := stats.NewServerStats(cfg.Name, b.cluster.cl.String())
	lstats := stats.NewLeaderStats(cfg.Logger, b.cluster.nodeID.String())

	heartbeat := time.Duration(cfg.TickMs) * time.Millisecond
	srv = &EtcdServer{
		readych:               make(chan struct{}),
		Cfg:                   cfg,
		lgMu:                  new(sync.RWMutex),
		lg:                    cfg.Logger,
		errorc:                make(chan error, 1),
		v2store:               b.storage.st,
		snapshotter:           b.ss,
		r:                     *b.raft.newRaftNode(b.ss, b.storage.wal.w, b.cluster.cl),
		memberID:              b.cluster.nodeID,
		attributes:            membership.Attributes{Name: cfg.Name, ClientURLs: cfg.ClientURLs.StringSlice()},
		cluster:               b.cluster.cl,
		stats:                 sstats,
		lstats:                lstats,
		SyncTicker:            time.NewTicker(500 * time.Millisecond),
		peerRt:                b.prt,
		reqIDGen:              idutil.NewGenerator(uint16(b.cluster.nodeID), time.Now()),
		AccessController:      &AccessController{CORS: cfg.CORS, HostWhitelist: cfg.HostWhitelist},
		consistIndex:          b.storage.backend.ci,
		firstCommitInTerm:     notify.NewNotifier(),
		clusterVersionChanged: notify.NewNotifier(),
	}
	serverID.With(prometheus.Labels{"server_id": b.cluster.nodeID.String()}).Set(1)
	srv.cluster.SetVersionChangedNotifier(srv.clusterVersionChanged)

	srv.be = b.storage.backend.be
	srv.beHooks = b.storage.backend.beHooks
	minTTL := time.Duration((3*cfg.ElectionTicks)/2) * heartbeat

	// always recover lessor before kv. When we recover the mvcc.KV it will reattach keys to its leases.
	// If we recover mvcc.KV first, it will attach the keys to the wrong lessor before it recovers.
	srv.lessor = lease.NewLessor(srv.Logger(), srv.be, srv.cluster, lease.LessorConfig{
		MinLeaseTTL:                int64(math.Ceil(minTTL.Seconds())),
		CheckpointInterval:         cfg.LeaseCheckpointInterval,
		CheckpointPersist:          cfg.LeaseCheckpointPersist,
		ExpiredLeasesRetryInterval: srv.Cfg.ReqTimeout(),
	})

	tp, err := auth.NewTokenProvider(cfg.Logger, cfg.AuthToken,
		func(index uint64) <-chan struct{} {
			return srv.applyWait.Wait(index)
		},
		time.Duration(cfg.TokenTTL)*time.Second,
	)
	if err != nil {
		cfg.Logger.Warn("failed to create token provider", zap.Error(err))
		return nil, err
	}

	mvccStoreConfig := mvcc.StoreConfig{
		CompactionBatchLimit:    cfg.CompactionBatchLimit,
		CompactionSleepInterval: cfg.CompactionSleepInterval,
	}
	srv.kv = mvcc.New(srv.Logger(), srv.be, srv.lessor, mvccStoreConfig)
	srv.corruptionChecker = newCorruptionChecker(cfg.Logger, srv, srv.kv.HashStorage())

	srv.authStore = auth.NewAuthStore(srv.Logger(), schema.NewAuthBackend(srv.Logger(), srv.be), tp, int(cfg.BcryptCost))

	newSrv := srv // since srv == nil in defer if srv is returned as nil
	defer func() {
		// closing backend without first closing kv can cause
		// resumed compactions to fail with closed tx errors
		if err != nil {
			newSrv.kv.Close()
		}
	}()
	if num := cfg.AutoCompactionRetention; num != 0 {
		srv.compactor, err = v3compactor.New(cfg.Logger, cfg.AutoCompactionMode, num, srv.kv, srv)
		if err != nil {
			return nil, err
		}
		srv.compactor.Run()
	}

	if err = srv.restoreAlarms(); err != nil {
		return nil, err
	}
	srv.uberApply = srv.NewUberApplier()

	if srv.Cfg.EnableLeaseCheckpoint {
		// setting checkpointer enables lease checkpoint feature.
		srv.lessor.SetCheckpointer(func(ctx context.Context, cp *pb.LeaseCheckpointRequest) error {
			if !srv.ensureLeadership() {
				srv.lg.Warn("Ignore the checkpoint request because current member isn't a leader",
					zap.Uint64("local-member-id", uint64(srv.MemberID())))
				return lease.ErrNotPrimary
			}

			srv.raftRequestOnce(ctx, pb.InternalRaftRequest{LeaseCheckpoint: cp})
			return nil
		})
	}

	// Set the hook after EtcdServer finishes the initialization to avoid
	// the hook being called during the initialization process.
	srv.be.SetTxPostLockInsideApplyHook(srv.getTxPostLockInsideApplyHook())

	// TODO: move transport initialization near the definition of remote
	tr := &rafthttp.Transport{
		Logger:      cfg.Logger,
		TLSInfo:     cfg.PeerTLSInfo,
		DialTimeout: cfg.PeerDialTimeout(),
		ID:          b.cluster.nodeID,
		URLs:        cfg.PeerURLs,
		ClusterID:   b.cluster.cl.ID(),
		Raft:        srv,
		Snapshotter: b.ss,
		ServerStats: sstats,
		LeaderStats: lstats,
		ErrorC:      srv.errorc,
	}
	if err = tr.Start(); err != nil {
		return nil, err
	}
	// add all remotes into transport
	for _, m := range b.cluster.remotes {
		if m.ID != b.cluster.nodeID {
			tr.AddRemote(m.ID, m.PeerURLs)
		}
	}
	for _, m := range b.cluster.cl.Members() {
		if m.ID != b.cluster.nodeID {
			tr.AddPeer(m.ID, m.PeerURLs)
		}
	}
	srv.r.transport = tr

	return srv, nil
}

func (s *EtcdServer) Logger() *zap.Logger {
	s.lgMu.RLock()
	l := s.lg
	s.lgMu.RUnlock()
	return l
}

func (s *EtcdServer) Config() config.ServerConfig {
	return s.Cfg
}

// FeatureEnabled returns true if the feature is enabled by the etcd server, false otherwise.
func (s *EtcdServer) FeatureEnabled(f featuregate.Feature) bool {
	return s.Cfg.ServerFeatureGate.Enabled(f)
}

func tickToDur(ticks int, tickMs uint) string {
	return fmt.Sprintf("%v", time.Duration(ticks)*time.Duration(tickMs)*time.Millisecond)
}

func (s *EtcdServer) adjustTicks() {
	lg := s.Logger()
	clusterN := len(s.cluster.Members())

	// single-node fresh start, or single-node recovers from snapshot
	if clusterN == 1 {
		ticks := s.Cfg.ElectionTicks - 1
		lg.Info(
			"started as single-node; fast-forwarding election ticks",
			zap.String("local-member-id", s.MemberID().String()),
			zap.Int("forward-ticks", ticks),
			zap.String("forward-duration", tickToDur(ticks, s.Cfg.TickMs)),
			zap.Int("election-ticks", s.Cfg.ElectionTicks),
			zap.String("election-timeout", tickToDur(s.Cfg.ElectionTicks, s.Cfg.TickMs)),
		)
		s.r.advanceTicks(ticks)
		return
	}

	if !s.Cfg.InitialElectionTickAdvance {
		lg.Info("skipping initial election tick advance", zap.Int("election-ticks", s.Cfg.ElectionTicks))
		return
	}
	lg.Info("starting initial election tick advance", zap.Int("election-ticks", s.Cfg.ElectionTicks))

	// retry up to "rafthttp.ConnReadTimeout", which is 5-sec
	// until peer connection reports; otherwise:
	// 1. all connections failed, or
	// 2. no active peers, or
	// 3. restarted single-node with no snapshot
	// then, do nothing, because advancing ticks would have no effect
	waitTime := rafthttp.ConnReadTimeout
	itv := 50 * time.Millisecond
	for i := int64(0); i < int64(waitTime/itv); i++ {
		select {
		case <-time.After(itv):
		case <-s.stopping:
			return
		}

		peerN := s.r.transport.ActivePeers()
		if peerN > 1 {
			// multi-node received peer connection reports
			// adjust ticks, in case slow leader message receive
			ticks := s.Cfg.ElectionTicks - 2

			lg.Info(
				"initialized peer connections; fast-forwarding election ticks",
				zap.String("local-member-id", s.MemberID().String()),
				zap.Int("forward-ticks", ticks),
				zap.String("forward-duration", tickToDur(ticks, s.Cfg.TickMs)),
				zap.Int("election-ticks", s.Cfg.ElectionTicks),
				zap.String("election-timeout", tickToDur(s.Cfg.ElectionTicks, s.Cfg.TickMs)),
				zap.Int("active-remote-members", peerN),
			)

			s.r.advanceTicks(ticks)
			return
		}
	}
}

// Start performs any initialization of the Server necessary for it to
// begin serving requests. It must be called before Do or Process.
// Start must be non-blocking; any long-running server functionality
// should be implemented in goroutines.
func (s *EtcdServer) Start() {
	s.start()
	s.GoAttach(func() { s.adjustTicks() })
	s.GoAttach(func() { s.publishV3(s.Cfg.ReqTimeout()) })
	s.GoAttach(s.purgeFile)
	s.GoAttach(func() { monitorFileDescriptor(s.Logger(), s.stopping) })
	s.GoAttach(s.monitorClusterVersions)
	s.GoAttach(s.monitorStorageVersion)
	s.GoAttach(s.linearizableReadLoop)
	s.GoAttach(s.monitorKVHash)
	s.GoAttach(s.monitorCompactHash)
	s.GoAttach(s.monitorDowngrade)
}

// start prepares and starts server in a new goroutine. It is no longer safe to
// modify a server's fields after it has been sent to Start.
// This function is just used for testing.
func (s *EtcdServer) start() {
	lg := s.Logger()

	if s.Cfg.SnapshotCount == 0 {
		lg.Info(
			"updating snapshot-count to default",
			zap.Uint64("given-snapshot-count", s.Cfg.SnapshotCount),
			zap.Uint64("updated-snapshot-count", DefaultSnapshotCount),
		)
		s.Cfg.SnapshotCount = DefaultSnapshotCount
	}
	if s.Cfg.SnapshotCatchUpEntries == 0 {
		lg.Info(
			"updating snapshot catch-up entries to default",
			zap.Uint64("given-snapshot-catchup-entries", s.Cfg.SnapshotCatchUpEntries),
			zap.Uint64("updated-snapshot-catchup-entries", DefaultSnapshotCatchUpEntries),
		)
		s.Cfg.SnapshotCatchUpEntries = DefaultSnapshotCatchUpEntries
	}

	s.w = wait.New()
	s.applyWait = wait.NewTimeList()
	s.done = make(chan struct{})
	s.stop = make(chan struct{})
	s.stopping = make(chan struct{}, 1)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.readwaitc = make(chan struct{}, 1)
	s.readNotifier = newNotifier()
	s.leaderChanged = notify.NewNotifier()
	if s.ClusterVersion() != nil {
		lg.Info(
			"starting etcd server",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("local-server-version", version.Version),
			zap.String("cluster-id", s.Cluster().ID().String()),
			zap.String("cluster-version", version.Cluster(s.ClusterVersion().String())),
		)
		membership.ClusterVersionMetrics.With(prometheus.Labels{"cluster_version": version.Cluster(s.ClusterVersion().String())}).Set(1)
	} else {
		lg.Info(
			"starting etcd server",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("local-server-version", version.Version),
			zap.String("cluster-version", "to_be_decided"),
		)
	}

	// TODO: if this is an empty log, writes all peer infos
	// into the first entry
	go s.run()
}

func (s *EtcdServer) purgeFile() {
	lg := s.Logger()
	var dberrc, serrc, werrc <-chan error
	var dbdonec, sdonec, wdonec <-chan struct{}
	if s.Cfg.MaxSnapFiles > 0 {
		dbdonec, dberrc = fileutil.PurgeFileWithoutFlock(lg, s.Cfg.SnapDir(), "snap.db", s.Cfg.MaxSnapFiles, purgeFileInterval, s.stopping)
		sdonec, serrc = fileutil.PurgeFileWithoutFlock(lg, s.Cfg.SnapDir(), "snap", s.Cfg.MaxSnapFiles, purgeFileInterval, s.stopping)
	}
	if s.Cfg.MaxWALFiles > 0 {
		wdonec, werrc = fileutil.PurgeFileWithDoneNotify(lg, s.Cfg.WALDir(), "wal", s.Cfg.MaxWALFiles, purgeFileInterval, s.stopping)
	}

	select {
	case e := <-dberrc:
		lg.Fatal("failed to purge snap db file", zap.Error(e))
	case e := <-serrc:
		lg.Fatal("failed to purge snap file", zap.Error(e))
	case e := <-werrc:
		lg.Fatal("failed to purge wal file", zap.Error(e))
	case <-s.stopping:
		if dbdonec != nil {
			<-dbdonec
		}
		if sdonec != nil {
			<-sdonec
		}
		if wdonec != nil {
			<-wdonec
		}
		return
	}
}

func (s *EtcdServer) Cluster() api.Cluster { return s.cluster }

func (s *EtcdServer) ApplyWait() <-chan struct{} { return s.applyWait.Wait(s.getCommittedIndex()) }

type ServerPeer interface {
	ServerV2
	RaftHandler() http.Handler
	LeaseHandler() http.Handler
}

func (s *EtcdServer) LeaseHandler() http.Handler {
	if s.lessor == nil {
		return nil
	}
	return leasehttp.NewHandler(s.lessor, s.ApplyWait)
}

func (s *EtcdServer) RaftHandler() http.Handler { return s.r.transport.Handler() }

type ServerPeerV2 interface {
	ServerPeer
	HashKVHandler() http.Handler
	DowngradeEnabledHandler() http.Handler
}

func (s *EtcdServer) DowngradeInfo() *serverversion.DowngradeInfo { return s.cluster.DowngradeInfo() }

type downgradeEnabledHandler struct {
	lg      *zap.Logger
	cluster api.Cluster
	server  *EtcdServer
}

func (s *EtcdServer) DowngradeEnabledHandler() http.Handler {
	return &downgradeEnabledHandler{
		lg:      s.Logger(),
		cluster: s.cluster,
		server:  s,
	}
}

func (h *downgradeEnabledHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("X-Etcd-Cluster-ID", h.cluster.ID().String())

	if r.URL.Path != DowngradeEnabledPath {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.server.Cfg.ReqTimeout())
	defer cancel()

	// serve with linearized downgrade info
	if err := h.server.linearizableReadNotify(ctx); err != nil {
		http.Error(w, fmt.Sprintf("failed linearized read: %v", err),
			http.StatusInternalServerError)
		return
	}
	enabled := h.server.DowngradeInfo().Enabled
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strconv.FormatBool(enabled)))
}

// Process takes a raft message and applies it to the server's raft state
// machine, respecting any timeout of the given context.
func (s *EtcdServer) Process(ctx context.Context, m raftpb.Message) error {
	lg := s.Logger()
	if s.cluster.IsIDRemoved(types.ID(m.From)) {
		lg.Warn(
			"rejected Raft message from removed member",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("removed-member-id", types.ID(m.From).String()),
		)
		return httptypes.NewHTTPError(http.StatusForbidden, "cannot process message from removed member")
	}
	if s.MemberID() != types.ID(m.To) {
		lg.Warn(
			"rejected Raft message to mismatch member",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("mismatch-member-id", types.ID(m.To).String()),
		)
		return httptypes.NewHTTPError(http.StatusForbidden, "cannot process message to mismatch member")
	}
	if m.Type == raftpb.MsgApp {
		s.stats.RecvAppendReq(types.ID(m.From).String(), m.Size())
	}
	return s.r.Step(ctx, m)
}

func (s *EtcdServer) IsIDRemoved(id uint64) bool { return s.cluster.IsIDRemoved(types.ID(id)) }

func (s *EtcdServer) ReportUnreachable(id uint64) { s.r.ReportUnreachable(id) }

// ReportSnapshot reports snapshot sent status to the raft state machine,
// and clears the used snapshot from the snapshot store.
func (s *EtcdServer) ReportSnapshot(id uint64, status raft.SnapshotStatus) {
	s.r.ReportSnapshot(id, status)
}

type etcdProgress struct {
	confState raftpb.ConfState
	snapi     uint64
	appliedt  uint64
	appliedi  uint64
}

// raftReadyHandler contains a set of EtcdServer operations to be called by raftNode,
// and helps decouple state machine logic from Raft algorithms.
// TODO: add a state machine interface to toApply the commit entries and do snapshot/recover
type raftReadyHandler struct {
	getLead              func() (lead uint64)
	updateLead           func(lead uint64)
	updateLeadership     func(newLeader bool)
	updateCommittedIndex func(uint64)
}

func (s *EtcdServer) run() {
	lg := s.Logger()

	sn, err := s.r.raftStorage.Snapshot()
	if err != nil {
		lg.Panic("failed to get snapshot from Raft storage", zap.Error(err))
	}

	// asynchronously accept toApply packets, dispatch progress in-order
	sched := schedule.NewFIFOScheduler(lg)

	rh := &raftReadyHandler{
		getLead:    func() (lead uint64) { return s.getLead() },
		updateLead: func(lead uint64) { s.setLead(lead) },
		updateLeadership: func(newLeader bool) {
			if !s.isLeader() {
				if s.lessor != nil {
					s.lessor.Demote()
				}
				if s.compactor != nil {
					s.compactor.Pause()
				}
			} else {
				if newLeader {
					t := time.Now()
					s.leadTimeMu.Lock()
					s.leadElectedTime = t
					s.leadTimeMu.Unlock()
				}
				if s.compactor != nil {
					s.compactor.Resume()
				}
			}
			if newLeader {
				s.leaderChanged.Notify()
			}
			// TODO: remove the nil checking
			// current test utility does not provide the stats
			if s.stats != nil {
				s.stats.BecomeLeader()
			}
		},
		updateCommittedIndex: func(ci uint64) {
			cci := s.getCommittedIndex()
			if ci > cci {
				s.setCommittedIndex(ci)
			}
		},
	}
	s.r.start(rh)

	ep := etcdProgress{
		confState: sn.Metadata.ConfState,
		snapi:     sn.Metadata.Index,
		appliedt:  sn.Metadata.Term,
		appliedi:  sn.Metadata.Index,
	}

	defer func() {
		s.wgMu.Lock() // block concurrent waitgroup adds in GoAttach while stopping
		close(s.stopping)
		s.wgMu.Unlock()
		s.cancel()
		sched.Stop()

		// wait for goroutines before closing raft so wal stays open
		s.wg.Wait()

		s.SyncTicker.Stop()

		// must stop raft after scheduler-- etcdserver can leak rafthttp pipelines
		// by adding a peer after raft stops the transport
		s.r.stop()

		s.Cleanup()

		close(s.done)
	}()

	var expiredLeaseC <-chan []*lease.Lease
	if s.lessor != nil {
		expiredLeaseC = s.lessor.ExpiredLeasesC()
	}

	for {
		select {
		case ap := <-s.r.apply():
			f := schedule.NewJob("server_applyAll", func(context.Context) { s.applyAll(&ep, &ap) })
			sched.Schedule(f)
		case leases := <-expiredLeaseC:
			s.revokeExpiredLeases(leases)
		case err := <-s.errorc:
			lg.Warn("server error", zap.Error(err))
			lg.Warn("data-dir used by this member must be removed")
			return
		case <-s.stop:
			return
		}
	}
}

func (s *EtcdServer) revokeExpiredLeases(leases []*lease.Lease) {
	s.GoAttach(func() {
		// We shouldn't revoke any leases if current member isn't a leader,
		// because the operation should only be performed by the leader. When
		// the leader gets blocked on the raft loop, such as writing WAL entries,
		// it can't process any events or messages from raft. It may think it
		// is still the leader even the leader has already changed.
		// Refer to https://github.com/etcd-io/etcd/issues/15247
		lg := s.Logger()
		if !s.ensureLeadership() {
			lg.Warn("Ignore the lease revoking request because current member isn't a leader",
				zap.Uint64("local-member-id", uint64(s.MemberID())))
			return
		}

		// Increases throughput of expired leases deletion process through parallelization
		c := make(chan struct{}, maxPendingRevokes)
		for _, curLease := range leases {
			select {
			case c <- struct{}{}:
			case <-s.stopping:
				return
			}

			f := func(lid int64) {
				s.GoAttach(func() {
					ctx := s.authStore.WithRoot(s.ctx)
					_, lerr := s.LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: lid})
					if lerr == nil {
						leaseExpired.Inc()
					} else {
						lg.Warn(
							"failed to revoke lease",
							zap.String("lease-id", fmt.Sprintf("%016x", lid)),
							zap.Error(lerr),
						)
					}

					<-c
				})
			}

			f(int64(curLease.ID))
		}
	})
}

// isActive checks if the etcd instance is still actively processing the
// heartbeat message (ticks). It returns false if no heartbeat has been
// received within 3 * tickMs.
func (s *EtcdServer) isActive() bool {
	latestTickTs := s.r.getLatestTickTs()
	threshold := 3 * time.Duration(s.Cfg.TickMs) * time.Millisecond
	return latestTickTs.Add(threshold).After(time.Now())
}

// ensureLeadership checks whether current member is still the leader.
func (s *EtcdServer) ensureLeadership() bool {
	lg := s.Logger()

	if s.isActive() {
		lg.Debug("The member is active, skip checking leadership",
			zap.Time("latestTickTs", s.r.getLatestTickTs()),
			zap.Time("now", time.Now()))
		return true
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.Cfg.ReqTimeout())
	defer cancel()
	if err := s.linearizableReadNotify(ctx); err != nil {
		lg.Warn("Failed to check current member's leadership",
			zap.Error(err))
		return false
	}

	newLeaderID := s.raftStatus().Lead
	if newLeaderID != uint64(s.MemberID()) {
		lg.Warn("Current member isn't a leader",
			zap.Uint64("local-member-id", uint64(s.MemberID())),
			zap.Uint64("new-lead", newLeaderID))
		return false
	}

	return true
}

// Cleanup removes allocated objects by EtcdServer.NewServer in
// situation that EtcdServer::Start was not called (that takes care of cleanup).
func (s *EtcdServer) Cleanup() {
	// kv, lessor and backend can be nil if running without v3 enabled
	// or running unit tests.
	if s.lessor != nil {
		s.lessor.Stop()
	}
	if s.kv != nil {
		s.kv.Close()
	}
	if s.authStore != nil {
		s.authStore.Close()
	}
	if s.be != nil {
		s.be.Close()
	}
	if s.compactor != nil {
		s.compactor.Stop()
	}
}

func (s *EtcdServer) applyAll(ep *etcdProgress, apply *toApply) {
	s.applySnapshot(ep, apply)
	s.applyEntries(ep, apply)
	backend.VerifyBackendConsistency(s.Backend(), s.Logger(), true, schema.AllBuckets...)

	proposalsApplied.Set(float64(ep.appliedi))
	s.applyWait.Trigger(ep.appliedi)

	// wait for the raft routine to finish the disk writes before triggering a
	// snapshot. or applied index might be greater than the last index in raft
	// storage, since the raft routine might be slower than toApply routine.
	<-apply.notifyc

	s.triggerSnapshot(ep)
	select {
	// snapshot requested via send()
	case m := <-s.r.msgSnapC:
		merged := s.createMergedSnapshotMessage(m, ep.appliedt, ep.appliedi, ep.confState)
		s.sendMergedSnap(merged)
	default:
	}
}

func (s *EtcdServer) applySnapshot(ep *etcdProgress, toApply *toApply) {
	if raft.IsEmptySnap(toApply.snapshot) {
		return
	}
	applySnapshotInProgress.Inc()

	lg := s.Logger()
	lg.Info(
		"applying snapshot",
		zap.Uint64("current-snapshot-index", ep.snapi),
		zap.Uint64("current-applied-index", ep.appliedi),
		zap.Uint64("incoming-leader-snapshot-index", toApply.snapshot.Metadata.Index),
		zap.Uint64("incoming-leader-snapshot-term", toApply.snapshot.Metadata.Term),
	)
	defer func() {
		lg.Info(
			"applied snapshot",
			zap.Uint64("current-snapshot-index", ep.snapi),
			zap.Uint64("current-applied-index", ep.appliedi),
			zap.Uint64("incoming-leader-snapshot-index", toApply.snapshot.Metadata.Index),
			zap.Uint64("incoming-leader-snapshot-term", toApply.snapshot.Metadata.Term),
		)
		applySnapshotInProgress.Dec()
	}()

	if toApply.snapshot.Metadata.Index <= ep.appliedi {
		lg.Panic(
			"unexpected leader snapshot from outdated index",
			zap.Uint64("current-snapshot-index", ep.snapi),
			zap.Uint64("current-applied-index", ep.appliedi),
			zap.Uint64("incoming-leader-snapshot-index", toApply.snapshot.Metadata.Index),
			zap.Uint64("incoming-leader-snapshot-term", toApply.snapshot.Metadata.Term),
		)
	}

	// wait for raftNode to persist snapshot onto the disk
	<-toApply.notifyc

	// gofail: var applyBeforeOpenSnapshot struct{}
	newbe, err := serverstorage.OpenSnapshotBackend(s.Cfg, s.snapshotter, toApply.snapshot, s.beHooks)
	if err != nil {
		lg.Panic("failed to open snapshot backend", zap.Error(err))
	}

	// We need to set the backend to consistIndex before recovering the lessor,
	// because lessor.Recover will commit the boltDB transaction, accordingly it
	// will get the old consistent_index persisted into the db in OnPreCommitUnsafe.
	// Eventually the new consistent_index value coming from snapshot is overwritten
	// by the old value.
	s.consistIndex.SetBackend(newbe)
	verifySnapshotIndex(toApply.snapshot, s.consistIndex.ConsistentIndex())

	// always recover lessor before kv. When we recover the mvcc.KV it will reattach keys to its leases.
	// If we recover mvcc.KV first, it will attach the keys to the wrong lessor before it recovers.
	if s.lessor != nil {
		lg.Info("restoring lease store")

		s.lessor.Recover(newbe, func() lease.TxnDelete { return s.kv.Write(traceutil.TODO()) })

		lg.Info("restored lease store")
	}

	lg.Info("restoring mvcc store")

	if err := s.kv.Restore(newbe); err != nil {
		lg.Panic("failed to restore mvcc store", zap.Error(err))
	}

	newbe.SetTxPostLockInsideApplyHook(s.getTxPostLockInsideApplyHook())

	lg.Info("restored mvcc store", zap.Uint64("consistent-index", s.consistIndex.ConsistentIndex()))

	// Closing old backend might block until all the txns
	// on the backend are finished.
	// We do not want to wait on closing the old backend.
	s.bemu.Lock()
	oldbe := s.be
	go func() {
		lg.Info("closing old backend file")
		defer func() {
			lg.Info("closed old backend file")
		}()
		if err := oldbe.Close(); err != nil {
			lg.Panic("failed to close old backend", zap.Error(err))
		}
	}()

	s.be = newbe
	s.bemu.Unlock()

	lg.Info("restoring alarm store")

	if err := s.restoreAlarms(); err != nil {
		lg.Panic("failed to restore alarm store", zap.Error(err))
	}

	lg.Info("restored alarm store")

	if s.authStore != nil {
		lg.Info("restoring auth store")

		s.authStore.Recover(schema.NewAuthBackend(lg, newbe))

		lg.Info("restored auth store")
	}

	lg.Info("restoring v2 store")
	if err := s.v2store.Recovery(toApply.snapshot.Data); err != nil {
		lg.Panic("failed to restore v2 store", zap.Error(err))
	}

	if err := serverstorage.AssertNoV2StoreContent(lg, s.v2store, s.Cfg.V2Deprecation); err != nil {
		lg.Panic("illegal v2store content", zap.Error(err))
	}

	lg.Info("restored v2 store")

	s.cluster.SetBackend(schema.NewMembershipBackend(lg, newbe))

	lg.Info("restoring cluster configuration")

	s.cluster.Recover(api.UpdateCapability)

	lg.Info("restored cluster configuration")
	lg.Info("removing old peers from network")

	// recover raft transport
	s.r.transport.RemoveAllPeers()

	lg.Info("removed old peers from network")
	lg.Info("adding peers from new cluster configuration")

	for _, m := range s.cluster.Members() {
		if m.ID == s.MemberID() {
			continue
		}
		s.r.transport.AddPeer(m.ID, m.PeerURLs)
	}

	lg.Info("added peers from new cluster configuration")

	ep.appliedt = toApply.snapshot.Metadata.Term
	ep.appliedi = toApply.snapshot.Metadata.Index
	ep.snapi = ep.appliedi
	ep.confState = toApply.snapshot.Metadata.ConfState

	// As backends and implementations like alarmsStore changed, we need
	// to re-bootstrap Appliers.
	s.uberApply = s.NewUberApplier()
}

func (s *EtcdServer) NewUberApplier() apply.UberApplier {
	return apply.NewUberApplier(s.lg, s.be, s.KV(), s.alarmStore, s.authStore, s.lessor, s.cluster, s, s, s.consistIndex,
		s.Cfg.WarningApplyDuration, s.Cfg.ExperimentalTxnModeWriteWithSharedBuffer, s.Cfg.QuotaBackendBytes)
}

func verifySnapshotIndex(snapshot raftpb.Snapshot, cindex uint64) {
	verify.Verify(func() {
		if cindex != snapshot.Metadata.Index {
			panic(fmt.Sprintf("consistent_index(%d) isn't equal to snapshot index (%d)", cindex, snapshot.Metadata.Index))
		}
	})
}

func verifyConsistentIndexIsLatest(lg *zap.Logger, snapshot raftpb.Snapshot, cindex uint64) {
	verify.Verify(func() {
		if cindex < snapshot.Metadata.Index {
			lg.Panic(fmt.Sprintf("consistent_index(%d) is older than snapshot index (%d)", cindex, snapshot.Metadata.Index))
		}
	})
}

func (s *EtcdServer) applyEntries(ep *etcdProgress, apply *toApply) {
	if len(apply.entries) == 0 {
		return
	}
	firsti := apply.entries[0].Index
	if firsti > ep.appliedi+1 {
		lg := s.Logger()
		lg.Panic(
			"unexpected committed entry index",
			zap.Uint64("current-applied-index", ep.appliedi),
			zap.Uint64("first-committed-entry-index", firsti),
		)
	}
	var ents []raftpb.Entry
	if ep.appliedi+1-firsti < uint64(len(apply.entries)) {
		ents = apply.entries[ep.appliedi+1-firsti:]
	}
	if len(ents) == 0 {
		return
	}
	var shouldstop bool
	if ep.appliedt, ep.appliedi, shouldstop = s.apply(ents, &ep.confState, apply.raftAdvancedC); shouldstop {
		go s.stopWithDelay(10*100*time.Millisecond, fmt.Errorf("the member has been permanently removed from the cluster"))
	}
}

func (s *EtcdServer) ForceSnapshot() {
	s.forceSnapshot = true
}

func (s *EtcdServer) triggerSnapshot(ep *etcdProgress) {
	if !s.shouldSnapshot(ep) {
		return
	}
	lg := s.Logger()
	lg.Info(
		"triggering snapshot",
		zap.String("local-member-id", s.MemberID().String()),
		zap.Uint64("local-member-applied-index", ep.appliedi),
		zap.Uint64("local-member-snapshot-index", ep.snapi),
		zap.Uint64("local-member-snapshot-count", s.Cfg.SnapshotCount),
		zap.Bool("snapshot-forced", s.forceSnapshot),
	)
	s.forceSnapshot = false

	s.snapshot(ep.appliedi, ep.confState)
	s.compactRaftLog(ep.appliedi)
	ep.snapi = ep.appliedi
}

func (s *EtcdServer) shouldSnapshot(ep *etcdProgress) bool {
	return (s.forceSnapshot && ep.appliedi != ep.snapi) || (ep.appliedi-ep.snapi > s.Cfg.SnapshotCount)
}

func (s *EtcdServer) hasMultipleVotingMembers() bool {
	return s.cluster != nil && len(s.cluster.VotingMemberIDs()) > 1
}

func (s *EtcdServer) isLeader() bool {
	return uint64(s.MemberID()) == s.Lead()
}

// MoveLeader transfers the leader to the given transferee.
func (s *EtcdServer) MoveLeader(ctx context.Context, lead, transferee uint64) error {
	member := s.cluster.Member(types.ID(transferee))
	if member == nil || member.IsLearner {
		return errors.ErrBadLeaderTransferee
	}

	now := time.Now()
	interval := time.Duration(s.Cfg.TickMs) * time.Millisecond

	lg := s.Logger()
	lg.Info(
		"leadership transfer starting",
		zap.String("local-member-id", s.MemberID().String()),
		zap.String("current-leader-member-id", types.ID(lead).String()),
		zap.String("transferee-member-id", types.ID(transferee).String()),
	)

	s.r.TransferLeadership(ctx, lead, transferee)
	for s.Lead() != transferee {
		select {
		case <-ctx.Done(): // time out
			return errors.ErrTimeoutLeaderTransfer
		case <-time.After(interval):
		}
	}

	// TODO: drain all requests, or drop all messages to the old leader
	lg.Info(
		"leadership transfer finished",
		zap.String("local-member-id", s.MemberID().String()),
		zap.String("old-leader-member-id", types.ID(lead).String()),
		zap.String("new-leader-member-id", types.ID(transferee).String()),
		zap.Duration("took", time.Since(now)),
	)
	return nil
}

// TryTransferLeadershipOnShutdown transfers the leader to the chosen transferee. It is only used in server graceful shutdown.
func (s *EtcdServer) TryTransferLeadershipOnShutdown() error {
	lg := s.Logger()
	if !s.isLeader() {
		lg.Info(
			"skipped leadership transfer; local server is not leader",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("current-leader-member-id", types.ID(s.Lead()).String()),
		)
		return nil
	}

	if !s.hasMultipleVotingMembers() {
		lg.Info(
			"skipped leadership transfer for single voting member cluster",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("current-leader-member-id", types.ID(s.Lead()).String()),
		)
		return nil
	}

	transferee, ok := longestConnected(s.r.transport, s.cluster.VotingMemberIDs())
	if !ok {
		return errors.ErrUnhealthy
	}

	tm := s.Cfg.ReqTimeout()
	ctx, cancel := context.WithTimeout(s.ctx, tm)
	err := s.MoveLeader(ctx, s.Lead(), uint64(transferee))
	cancel()
	return err
}

// HardStop stops the server without coordination with other members in the cluster.
func (s *EtcdServer) HardStop() {
	select {
	case s.stop <- struct{}{}:
	case <-s.done:
		return
	}
	<-s.done
}

// Stop stops the server gracefully, and shuts down the running goroutine.
// Stop should be called after a Start(s), otherwise it will block forever.
// When stopping leader, Stop transfers its leadership to one of its peers
// before stopping the server.
// Stop terminates the Server and performs any necessary finalization.
// Do and Process cannot be called after Stop has been invoked.
func (s *EtcdServer) Stop() {
	lg := s.Logger()
	if err := s.TryTransferLeadershipOnShutdown(); err != nil {
		lg.Warn("leadership transfer failed", zap.String("local-member-id", s.MemberID().String()), zap.Error(err))
	}
	s.HardStop()
}

// ReadyNotify returns a channel that will be closed when the server
// is ready to serve client requests
func (s *EtcdServer) ReadyNotify() <-chan struct{} { return s.readych }

func (s *EtcdServer) stopWithDelay(d time.Duration, err error) {
	select {
	case <-time.After(d):
	case <-s.done:
	}
	select {
	case s.errorc <- err:
	default:
	}
}

// StopNotify returns a channel that receives an empty struct
// when the server is stopped.
func (s *EtcdServer) StopNotify() <-chan struct{} { return s.done }

// StoppingNotify returns a channel that receives an empty struct
// when the server is being stopped.
func (s *EtcdServer) StoppingNotify() <-chan struct{} { return s.stopping }

func (s *EtcdServer) checkMembershipOperationPermission(ctx context.Context) error {
	if s.authStore == nil {
		// In the context of ordinary etcd process, s.authStore will never be nil.
		// This branch is for handling cases in server_test.go
		return nil
	}

	// Note that this permission check is done in the API layer,
	// so TOCTOU problem can be caused potentially in a schedule like this:
	// update membership with user A -> revoke root role of A -> toApply membership change
	// in the state machine layer
	// However, both of membership change and role management requires the root privilege.
	// So careful operation by admins can prevent the problem.
	authInfo, err := s.AuthInfoFromCtx(ctx)
	if err != nil {
		return err
	}

	return s.AuthStore().IsAdminPermitted(authInfo)
}

func (s *EtcdServer) AddMember(ctx context.Context, memb membership.Member) ([]*membership.Member, error) {
	if err := s.checkMembershipOperationPermission(ctx); err != nil {
		return nil, err
	}

	// TODO: move Member to protobuf type
	b, err := json.Marshal(memb)
	if err != nil {
		return nil, err
	}

	// by default StrictReconfigCheck is enabled; reject new members if unhealthy.
	if err := s.mayAddMember(memb); err != nil {
		return nil, err
	}

	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  uint64(memb.ID),
		Context: b,
	}

	if memb.IsLearner {
		cc.Type = raftpb.ConfChangeAddLearnerNode
	}

	return s.configure(ctx, cc)
}

func (s *EtcdServer) mayAddMember(memb membership.Member) error {
	lg := s.Logger()
	if !s.Cfg.StrictReconfigCheck {
		return nil
	}

	// protect quorum when adding voting member
	if !memb.IsLearner && !s.cluster.IsReadyToAddVotingMember() {
		lg.Warn(
			"rejecting member add request; not enough healthy members",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("requested-member-add", fmt.Sprintf("%+v", memb)),
			zap.Error(errors.ErrNotEnoughStartedMembers),
		)
		return errors.ErrNotEnoughStartedMembers
	}

	if !isConnectedFullySince(s.r.transport, time.Now().Add(-HealthInterval), s.MemberID(), s.cluster.VotingMembers()) {
		lg.Warn(
			"rejecting member add request; local member has not been connected to all peers, reconfigure breaks active quorum",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("requested-member-add", fmt.Sprintf("%+v", memb)),
			zap.Error(errors.ErrUnhealthy),
		)
		return errors.ErrUnhealthy
	}

	return nil
}

func (s *EtcdServer) RemoveMember(ctx context.Context, id uint64) ([]*membership.Member, error) {
	if err := s.checkMembershipOperationPermission(ctx); err != nil {
		return nil, err
	}

	// by default StrictReconfigCheck is enabled; reject removal if leads to quorum loss
	if err := s.mayRemoveMember(types.ID(id)); err != nil {
		return nil, err
	}

	cc := raftpb.ConfChange{
		Type:   raftpb.ConfChangeRemoveNode,
		NodeID: id,
	}
	return s.configure(ctx, cc)
}

// PromoteMember promotes a learner node to a voting node.
func (s *EtcdServer) PromoteMember(ctx context.Context, id uint64) ([]*membership.Member, error) {
	// only raft leader has information on whether the to-be-promoted learner node is ready. If promoteMember call
	// fails with ErrNotLeader, forward the request to leader node via HTTP. If promoteMember call fails with error
	// other than ErrNotLeader, return the error.
	resp, err := s.promoteMember(ctx, id)
	if err == nil {
		learnerPromoteSucceed.Inc()
		return resp, nil
	}
	if !errorspkg.Is(err, errors.ErrNotLeader) {
		learnerPromoteFailed.WithLabelValues(err.Error()).Inc()
		return resp, err
	}

	cctx, cancel := context.WithTimeout(ctx, s.Cfg.ReqTimeout())
	defer cancel()
	// forward to leader
	for cctx.Err() == nil {
		leader, err := s.waitLeader(cctx)
		if err != nil {
			return nil, err
		}
		for _, url := range leader.PeerURLs {
			resp, err := promoteMemberHTTP(cctx, url, id, s.peerRt)
			if err == nil {
				return resp, nil
			}
			// If member promotion failed, return early. Otherwise keep retry.
			if errorspkg.Is(err, errors.ErrLearnerNotReady) || errorspkg.Is(err, membership.ErrIDNotFound) || errorspkg.Is(err, membership.ErrMemberNotLearner) {
				return nil, err
			}
		}
	}

	if errorspkg.Is(cctx.Err(), context.DeadlineExceeded) {
		return nil, errors.ErrTimeout
	}
	return nil, errors.ErrCanceled
}

// promoteMember checks whether the to-be-promoted learner node is ready before sending the promote
// request to raft.
// The function returns ErrNotLeader if the local node is not raft leader (therefore does not have
// enough information to determine if the learner node is ready), returns ErrLearnerNotReady if the
// local node is leader (therefore has enough information) but decided the learner node is not ready
// to be promoted.
func (s *EtcdServer) promoteMember(ctx context.Context, id uint64) ([]*membership.Member, error) {
	if err := s.checkMembershipOperationPermission(ctx); err != nil {
		return nil, err
	}

	// check if we can promote this learner.
	if err := s.mayPromoteMember(types.ID(id)); err != nil {
		return nil, err
	}

	// build the context for the promote confChange. mark IsLearner to false and IsPromote to true.
	promoteChangeContext := membership.ConfigChangeContext{
		Member: membership.Member{
			ID: types.ID(id),
		},
		IsPromote: true,
	}

	b, err := json.Marshal(promoteChangeContext)
	if err != nil {
		return nil, err
	}

	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  id,
		Context: b,
	}

	return s.configure(ctx, cc)
}

func (s *EtcdServer) mayPromoteMember(id types.ID) error {
	lg := s.Logger()
	if err := s.isLearnerReady(lg, uint64(id)); err != nil {
		return err
	}

	if !s.Cfg.StrictReconfigCheck {
		return nil
	}
	if !s.cluster.IsReadyToPromoteMember(uint64(id)) {
		lg.Warn(
			"rejecting member promote request; not enough healthy members",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("requested-member-remove-id", id.String()),
			zap.Error(errors.ErrNotEnoughStartedMembers),
		)
		return errors.ErrNotEnoughStartedMembers
	}

	return nil
}

// check whether the learner catches up with leader or not.
// Note: it will return nil if member is not found in cluster or if member is not learner.
// These two conditions will be checked before toApply phase later.
func (s *EtcdServer) isLearnerReady(lg *zap.Logger, id uint64) error {
	if err := s.waitAppliedIndex(); err != nil {
		return err
	}

	rs := s.raftStatus()

	// leader's raftStatus.Progress is not nil
	if rs.Progress == nil {
		return errors.ErrNotLeader
	}

	var learnerMatch uint64
	isFound := false
	leaderID := rs.ID
	for memberID, progress := range rs.Progress {
		if id == memberID {
			// check its status
			learnerMatch = progress.Match
			isFound = true
			break
		}
	}

	// We should return an error in API directly, to avoid the request
	// being unnecessarily delivered to raft.
	if !isFound {
		return membership.ErrIDNotFound
	}

	leaderMatch := rs.Progress[leaderID].Match

	learnerReadyPercent := float64(learnerMatch) / float64(leaderMatch)

	// the learner's Match not caught up with leader yet
	if learnerReadyPercent < readyPercentThreshold {
		lg.Error(
			"rejecting promote learner: learner is not ready",
			zap.Float64("learner-ready-percent", learnerReadyPercent),
			zap.Float64("ready-percent-threshold", readyPercentThreshold),
		)
		return errors.ErrLearnerNotReady
	}

	return nil
}

func (s *EtcdServer) mayRemoveMember(id types.ID) error {
	if !s.Cfg.StrictReconfigCheck {
		return nil
	}

	lg := s.Logger()
	member := s.cluster.Member(id)
	// no need to check quorum when removing non-voting member
	if member != nil && member.IsLearner {
		return nil
	}

	if !s.cluster.IsReadyToRemoveVotingMember(uint64(id)) {
		lg.Warn(
			"rejecting member remove request; not enough healthy members",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("requested-member-remove-id", id.String()),
			zap.Error(errors.ErrNotEnoughStartedMembers),
		)
		return errors.ErrNotEnoughStartedMembers
	}

	// downed member is safe to remove since it's not part of the active quorum
	if t := s.r.transport.ActiveSince(id); id != s.MemberID() && t.IsZero() {
		return nil
	}

	// protect quorum if some members are down
	m := s.cluster.VotingMembers()
	active := numConnectedSince(s.r.transport, time.Now().Add(-HealthInterval), s.MemberID(), m)
	if (active - 1) < 1+((len(m)-1)/2) {
		lg.Warn(
			"rejecting member remove request; local member has not been connected to all peers, reconfigure breaks active quorum",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("requested-member-remove", id.String()),
			zap.Int("active-peers", active),
			zap.Error(errors.ErrUnhealthy),
		)
		return errors.ErrUnhealthy
	}

	return nil
}

func (s *EtcdServer) UpdateMember(ctx context.Context, memb membership.Member) ([]*membership.Member, error) {
	b, merr := json.Marshal(memb)
	if merr != nil {
		return nil, merr
	}

	if err := s.checkMembershipOperationPermission(ctx); err != nil {
		return nil, err
	}
	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeUpdateNode,
		NodeID:  uint64(memb.ID),
		Context: b,
	}
	return s.configure(ctx, cc)
}

func (s *EtcdServer) setCommittedIndex(v uint64) {
	atomic.StoreUint64(&s.committedIndex, v)
}

func (s *EtcdServer) getCommittedIndex() uint64 {
	return atomic.LoadUint64(&s.committedIndex)
}

func (s *EtcdServer) setAppliedIndex(v uint64) {
	atomic.StoreUint64(&s.appliedIndex, v)
}

func (s *EtcdServer) getAppliedIndex() uint64 {
	return atomic.LoadUint64(&s.appliedIndex)
}

func (s *EtcdServer) setTerm(v uint64) {
	atomic.StoreUint64(&s.term, v)
}

func (s *EtcdServer) getTerm() uint64 {
	return atomic.LoadUint64(&s.term)
}

func (s *EtcdServer) setLead(v uint64) {
	atomic.StoreUint64(&s.lead, v)
}

func (s *EtcdServer) getLead() uint64 {
	return atomic.LoadUint64(&s.lead)
}

func (s *EtcdServer) LeaderChangedNotify() <-chan struct{} {
	return s.leaderChanged.Receive()
}

// FirstCommitInTermNotify returns channel that will be unlocked on first
// entry committed in new term, which is necessary for new leader to answer
// read-only requests (leader is not able to respond any read-only requests
// as long as linearizable semantic is required)
func (s *EtcdServer) FirstCommitInTermNotify() <-chan struct{} {
	return s.firstCommitInTerm.Receive()
}

// MemberId returns the ID of the local member.
// Deprecated: Please use (*EtcdServer) MemberID instead.
//
//revive:disable:var-naming
func (s *EtcdServer) MemberId() types.ID { return s.MemberID() }

//revive:enable:var-naming

func (s *EtcdServer) MemberID() types.ID { return s.memberID }

func (s *EtcdServer) Leader() types.ID { return types.ID(s.getLead()) }

func (s *EtcdServer) Lead() uint64 { return s.getLead() }

func (s *EtcdServer) CommittedIndex() uint64 { return s.getCommittedIndex() }

func (s *EtcdServer) AppliedIndex() uint64 { return s.getAppliedIndex() }

func (s *EtcdServer) Term() uint64 { return s.getTerm() }

type confChangeResponse struct {
	membs        []*membership.Member
	raftAdvanceC <-chan struct{}
	err          error
}

// configure sends a configuration change through consensus and
// then waits for it to be applied to the server. It
// will block until the change is performed or there is an error.
func (s *EtcdServer) configure(ctx context.Context, cc raftpb.ConfChange) ([]*membership.Member, error) {
	lg := s.Logger()
	cc.ID = s.reqIDGen.Next()
	ch := s.w.Register(cc.ID)

	start := time.Now()
	if err := s.r.ProposeConfChange(ctx, cc); err != nil {
		s.w.Trigger(cc.ID, nil)
		return nil, err
	}

	select {
	case x := <-ch:
		if x == nil {
			lg.Panic("failed to configure")
		}
		resp := x.(*confChangeResponse)
		// etcdserver need to ensure the raft has already been notified
		// or advanced before it responds to the client. Otherwise, the
		// following config change request may be rejected.
		// See https://github.com/etcd-io/etcd/issues/15528.
		<-resp.raftAdvanceC
		lg.Info(
			"applied a configuration change through raft",
			zap.String("local-member-id", s.MemberID().String()),
			zap.String("raft-conf-change", cc.Type.String()),
			zap.String("raft-conf-change-node-id", types.ID(cc.NodeID).String()),
		)
		return resp.membs, resp.err

	case <-ctx.Done():
		s.w.Trigger(cc.ID, nil) // GC wait
		return nil, s.parseProposeCtxErr(ctx.Err(), start)

	case <-s.stopping:
		return nil, errors.ErrStopped
	}
}

// publishV3 registers server information into the cluster using v3 request. The
// information is the JSON representation of this server's member struct, updated
// with the static clientURLs of the server.
// The function keeps attempting to register until it succeeds,
// or its server is stopped.
func (s *EtcdServer) publishV3(timeout time.Duration) {
	req := &membershippb.ClusterMemberAttrSetRequest{
		Member_ID: uint64(s.MemberID()),
		MemberAttributes: &membershippb.Attributes{
			Name:       s.attributes.Name,
			ClientUrls: s.attributes.ClientURLs,
		},
	}
	lg := s.Logger()
	for {
		select {
		case <-s.stopping:
			lg.Warn(
				"stopped publish because server is stopping",
				zap.String("local-member-id", s.MemberID().String()),
				zap.String("local-member-attributes", fmt.Sprintf("%+v", s.attributes)),
				zap.Duration("publish-timeout", timeout),
			)
			return

		default:
		}

		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		_, err := s.raftRequest(ctx, pb.InternalRaftRequest{ClusterMemberAttrSet: req})
		cancel()
		switch err {
		case nil:
			close(s.readych)
			lg.Info(
				"published local member to cluster through raft",
				zap.String("local-member-id", s.MemberID().String()),
				zap.String("local-member-attributes", fmt.Sprintf("%+v", s.attributes)),
				zap.String("cluster-id", s.cluster.ID().String()),
				zap.Duration("publish-timeout", timeout),
			)
			return

		default:
			lg.Warn(
				"failed to publish local member to cluster through raft",
				zap.String("local-member-id", s.MemberID().String()),
				zap.String("local-member-attributes", fmt.Sprintf("%+v", s.attributes)),
				zap.Duration("publish-timeout", timeout),
				zap.Error(err),
			)
		}
	}
}

func (s *EtcdServer) sendMergedSnap(merged snap.Message) {
	atomic.AddInt64(&s.inflightSnapshots, 1)

	lg := s.Logger()
	fields := []zap.Field{
		zap.String("from", s.MemberID().String()),
		zap.String("to", types.ID(merged.To).String()),
		zap.Int64("bytes", merged.TotalSize),
		zap.String("size", humanize.Bytes(uint64(merged.TotalSize))),
	}

	now := time.Now()
	s.r.transport.SendSnapshot(merged)
	lg.Info("sending merged snapshot", fields...)

	s.GoAttach(func() {
		select {
		case ok := <-merged.CloseNotify():
			// delay releasing inflight snapshot for another 30 seconds to
			// block log compaction.
			// If the follower still fails to catch up, it is probably just too slow
			// to catch up. We cannot avoid the snapshot cycle anyway.
			if ok {
				select {
				case <-time.After(releaseDelayAfterSnapshot):
				case <-s.stopping:
				}
			}

			atomic.AddInt64(&s.inflightSnapshots, -1)

			lg.Info("sent merged snapshot", append(fields, zap.Duration("took", time.Since(now)))...)

		case <-s.stopping:
			lg.Warn("canceled sending merged snapshot; server stopping", fields...)
			return
		}
	})
}

// toApply takes entries received from Raft (after it has been committed) and
// applies them to the current state of the EtcdServer.
// The given entries should not be empty.
func (s *EtcdServer) apply(
	es []raftpb.Entry,
	confState *raftpb.ConfState,
	raftAdvancedC <-chan struct{},
) (appliedt uint64, appliedi uint64, shouldStop bool) {
	s.lg.Debug("Applying entries", zap.Int("num-entries", len(es)))
	for i := range es {
		e := es[i]
		index := s.consistIndex.ConsistentIndex()
		s.lg.Debug("Applying entry",
			zap.Uint64("consistent-index", index),
			zap.Uint64("entry-index", e.Index),
			zap.Uint64("entry-term", e.Term),
			zap.Stringer("entry-type", e.Type))

		// We need to toApply all WAL entries on top of v2store
		// and only 'unapplied' (e.Index>backend.ConsistentIndex) on the backend.
		shouldApplyV3 := membership.ApplyV2storeOnly
		if e.Index > index {
			shouldApplyV3 = membership.ApplyBoth
			// set the consistent index of current executing entry
			s.consistIndex.SetConsistentApplyingIndex(e.Index, e.Term)
		}
		switch e.Type {
		case raftpb.EntryNormal:
			// gofail: var beforeApplyOneEntryNormal struct{}
			s.applyEntryNormal(&e, shouldApplyV3)
			s.setAppliedIndex(e.Index)
			s.setTerm(e.Term)

		case raftpb.EntryConfChange:
			// gofail: var beforeApplyOneConfChange struct{}
			var cc raftpb.ConfChange
			pbutil.MustUnmarshal(&cc, e.Data)
			removedSelf, err := s.applyConfChange(cc, confState, shouldApplyV3)
			s.setAppliedIndex(e.Index)
			s.setTerm(e.Term)
			shouldStop = shouldStop || removedSelf
			s.w.Trigger(cc.ID, &confChangeResponse{s.cluster.Members(), raftAdvancedC, err})

		default:
			lg := s.Logger()
			lg.Panic(
				"unknown entry type; must be either EntryNormal or EntryConfChange",
				zap.String("type", e.Type.String()),
			)
		}
		appliedi, appliedt = e.Index, e.Term
	}
	return appliedt, appliedi, shouldStop
}

// applyEntryNormal applies an EntryNormal type raftpb request to the EtcdServer
func (s *EtcdServer) applyEntryNormal(e *raftpb.Entry, shouldApplyV3 membership.ShouldApplyV3) {
	var ar *apply.Result
	if shouldApplyV3 {
		defer func() {
			// The txPostLockInsideApplyHook will not get called in some cases,
			// in which we should move the consistent index forward directly.
			newIndex := s.consistIndex.ConsistentIndex()
			if newIndex < e.Index {
				s.consistIndex.SetConsistentIndex(e.Index, e.Term)
			}
		}()
	}

	// raft state machine may generate noop entry when leader confirmation.
	// skip it in advance to avoid some potential bug in the future
	if len(e.Data) == 0 {
		s.firstCommitInTerm.Notify()

		// promote lessor when the local member is leader and finished
		// applying all entries from the last term.
		if s.isLeader() {
			s.lessor.Promote(s.Cfg.ElectionTimeout())
		}
		return
	}

	var raftReq pb.InternalRaftRequest
	if !pbutil.MaybeUnmarshal(&raftReq, e.Data) { // backward compatible
		var r pb.Request
		rp := &r
		pbutil.MustUnmarshal(rp, e.Data)
		s.lg.Debug("applyEntryNormal", zap.Stringer("V2request", rp))
		s.applyV2Request((*RequestV2)(rp), shouldApplyV3)
		s.w.Trigger(r.ID, Response{})
		return
	}
	s.lg.Debug("applyEntryNormal", zap.Stringer("raftReq", &raftReq))

	if raftReq.V2 != nil {
		req := (*RequestV2)(raftReq.V2)
		s.applyV2Request(req, shouldApplyV3)
		s.w.Trigger(req.ID, Response{})
		return
	}

	id := raftReq.ID
	if id == 0 {
		if raftReq.Header == nil {
			s.lg.Panic("applyEntryNormal, could not find a header")
		}
		id = raftReq.Header.ID
	}

	needResult := s.w.IsRegistered(id)
	if needResult || !noSideEffect(&raftReq) {
		if !needResult && raftReq.Txn != nil {
			removeNeedlessRangeReqs(raftReq.Txn)
		}
		ar = s.applyInternalRaftRequest(&raftReq, shouldApplyV3)
	}

	// do not re-toApply applied entries.
	if !shouldApplyV3 {
		return
	}

	if ar == nil {
		return
	}

	if !errorspkg.Is(ar.Err, errors.ErrNoSpace) || len(s.alarmStore.Get(pb.AlarmType_NOSPACE)) > 0 {
		s.w.Trigger(id, ar)
		return
	}

	lg := s.Logger()
	lg.Warn(
		"message exceeded backend quota; raising alarm",
		zap.Int64("quota-size-bytes", s.Cfg.QuotaBackendBytes),
		zap.String("quota-size", humanize.Bytes(uint64(s.Cfg.QuotaBackendBytes))),
		zap.Error(ar.Err),
	)

	s.GoAttach(func() {
		a := &pb.AlarmRequest{
			MemberID: uint64(s.MemberID()),
			Action:   pb.AlarmRequest_ACTIVATE,
			Alarm:    pb.AlarmType_NOSPACE,
		}
		s.raftRequest(s.ctx, pb.InternalRaftRequest{Alarm: a})
		s.w.Trigger(id, ar)
	})
}

func (s *EtcdServer) applyInternalRaftRequest(r *pb.InternalRaftRequest, shouldApplyV3 membership.ShouldApplyV3) *apply.Result {
	if r.ClusterVersionSet == nil && r.ClusterMemberAttrSet == nil && r.DowngradeInfoSet == nil {
		if !shouldApplyV3 {
			return nil
		}
		return s.uberApply.Apply(r)
	}
	membershipApplier := apply.NewApplierMembership(s.lg, s.cluster, s)
	op := "unknown"
	defer func(start time.Time) {
		txn.ApplySecObserve("v3", op, true, time.Since(start))
		txn.WarnOfExpensiveRequest(s.lg, s.Cfg.WarningApplyDuration, start, &pb.InternalRaftStringer{Request: r}, nil, nil)
	}(time.Now())
	switch {
	case r.ClusterVersionSet != nil:
		op = "ClusterVersionSet" // Implemented in 3.5.x
		membershipApplier.ClusterVersionSet(r.ClusterVersionSet, shouldApplyV3)
		return &apply.Result{}
	case r.ClusterMemberAttrSet != nil:
		op = "ClusterMemberAttrSet" // Implemented in 3.5.x
		membershipApplier.ClusterMemberAttrSet(r.ClusterMemberAttrSet, shouldApplyV3)
	case r.DowngradeInfoSet != nil:
		op = "DowngradeInfoSet" // Implemented in 3.5.x
		membershipApplier.DowngradeInfoSet(r.DowngradeInfoSet, shouldApplyV3)
	default:
		s.lg.Panic("not implemented apply", zap.Stringer("raft-request", r))
		return nil
	}
	return &apply.Result{}
}

func noSideEffect(r *pb.InternalRaftRequest) bool {
	return r.Range != nil || r.AuthUserGet != nil || r.AuthRoleGet != nil || r.AuthStatus != nil
}

func removeNeedlessRangeReqs(txn *pb.TxnRequest) {
	f := func(ops []*pb.RequestOp) []*pb.RequestOp {
		j := 0
		for i := 0; i < len(ops); i++ {
			if _, ok := ops[i].Request.(*pb.RequestOp_RequestRange); ok {
				continue
			}
			ops[j] = ops[i]
			j++
		}

		return ops[:j]
	}

	txn.Success = f(txn.Success)
	txn.Failure = f(txn.Failure)
}

// applyConfChange applies a ConfChange to the server. It is only
// invoked with a ConfChange that has already passed through Raft
func (s *EtcdServer) applyConfChange(cc raftpb.ConfChange, confState *raftpb.ConfState, shouldApplyV3 membership.ShouldApplyV3) (bool, error) {
	lg := s.Logger()
	if err := s.cluster.ValidateConfigurationChange(cc); err != nil {
		lg.Error("Validation on configuration change failed", zap.Bool("shouldApplyV3", bool(shouldApplyV3)), zap.Error(err))
		cc.NodeID = raft.None
		s.r.ApplyConfChange(cc)

		// The txPostLock callback will not get called in this case,
		// so we should set the consistent index directly.
		if s.consistIndex != nil && membership.ApplyBoth == shouldApplyV3 {
			applyingIndex, applyingTerm := s.consistIndex.ConsistentApplyingIndex()
			s.consistIndex.SetConsistentIndex(applyingIndex, applyingTerm)
		}
		return false, err
	}

	*confState = *s.r.ApplyConfChange(cc)
	s.beHooks.SetConfState(confState)
	switch cc.Type {
	case raftpb.ConfChangeAddNode, raftpb.ConfChangeAddLearnerNode:
		confChangeContext := new(membership.ConfigChangeContext)
		if err := json.Unmarshal(cc.Context, confChangeContext); err != nil {
			lg.Panic("failed to unmarshal member", zap.Error(err))
		}
		if cc.NodeID != uint64(confChangeContext.Member.ID) {
			lg.Panic(
				"got different member ID",
				zap.String("member-id-from-config-change-entry", types.ID(cc.NodeID).String()),
				zap.String("member-id-from-message", confChangeContext.Member.ID.String()),
			)
		}
		if confChangeContext.IsPromote {
			s.cluster.PromoteMember(confChangeContext.Member.ID, shouldApplyV3)
		} else {
			s.cluster.AddMember(&confChangeContext.Member, shouldApplyV3)

			if confChangeContext.Member.ID != s.MemberID() {
				s.r.transport.AddPeer(confChangeContext.Member.ID, confChangeContext.PeerURLs)
			}
		}

	case raftpb.ConfChangeRemoveNode:
		id := types.ID(cc.NodeID)
		s.cluster.RemoveMember(id, shouldApplyV3)
		if id == s.MemberID() {
			return true, nil
		}
		s.r.transport.RemovePeer(id)

	case raftpb.ConfChangeUpdateNode:
		m := new(membership.Member)
		if err := json.Unmarshal(cc.Context, m); err != nil {
			lg.Panic("failed to unmarshal member", zap.Error(err))
		}
		if cc.NodeID != uint64(m.ID) {
			lg.Panic(
				"got different member ID",
				zap.String("member-id-from-config-change-entry", types.ID(cc.NodeID).String()),
				zap.String("member-id-from-message", m.ID.String()),
			)
		}
		s.cluster.UpdateRaftAttributes(m.ID, m.RaftAttributes, shouldApplyV3)
		if m.ID != s.MemberID() {
			s.r.transport.UpdatePeer(m.ID, m.PeerURLs)
		}
	}
	return false, nil
}

// TODO: non-blocking snapshot
func (s *EtcdServer) snapshot(snapi uint64, confState raftpb.ConfState) {
	d := GetMembershipInfoInV2Format(s.Logger(), s.cluster)
	// commit kv to write metadata (for example: consistent index) to disk.
	//
	// This guarantees that Backend's consistent_index is >= index of last snapshot.
	//
	// KV().commit() updates the consistent index in backend.
	// All operations that update consistent index must be called sequentially
	// from applyAll function.
	// So KV().Commit() cannot run in parallel with toApply. It has to be called outside
	// the go routine created below.
	s.KV().Commit()

	lg := s.Logger()

	// For backward compatibility, generate v2 snapshot from v3 state.
	snap, err := s.r.raftStorage.CreateSnapshot(snapi, &confState, d)
	if err != nil {
		// the snapshot was done asynchronously with the progress of raft.
		// raft might have already got a newer snapshot.
		if errorspkg.Is(err, raft.ErrSnapOutOfDate) {
			return
		}
		lg.Panic("failed to create snapshot", zap.Error(err))
	}

	verifyConsistentIndexIsLatest(lg, snap, s.consistIndex.ConsistentIndex())

	// SaveSnap saves the snapshot to file and appends the corresponding WAL entry.
	if err = s.r.storage.SaveSnap(snap); err != nil {
		lg.Panic("failed to save snapshot", zap.Error(err))
	}
	if err = s.r.storage.Release(snap); err != nil {
		lg.Panic("failed to release wal", zap.Error(err))
	}

	lg.Info(
		"saved snapshot",
		zap.Uint64("snapshot-index", snap.Metadata.Index),
	)
}

func (s *EtcdServer) compactRaftLog(snapi uint64) {
	lg := s.Logger()

	// When sending a snapshot, etcd will pause compaction.
	// After receives a snapshot, the slow follower needs to get all the entries right after
	// the snapshot sent to catch up. If we do not pause compaction, the log entries right after
	// the snapshot sent might already be compacted. It happens when the snapshot takes long time
	// to send and save. Pausing compaction avoids triggering a snapshot sending cycle.
	if atomic.LoadInt64(&s.inflightSnapshots) != 0 {
		lg.Info("skip compaction since there is an inflight snapshot")
		return
	}

	// keep some in memory log entries for slow followers.
	compacti := uint64(1)
	if snapi > s.Cfg.SnapshotCatchUpEntries {
		compacti = snapi - s.Cfg.SnapshotCatchUpEntries
	}

	err := s.r.raftStorage.Compact(compacti)
	if err != nil {
		// the compaction was done asynchronously with the progress of raft.
		// raft log might already been compact.
		if errorspkg.Is(err, raft.ErrCompacted) {
			return
		}
		lg.Panic("failed to compact", zap.Error(err))
	}
	lg.Info(
		"compacted Raft logs",
		zap.Uint64("compact-index", compacti),
	)
}

// CutPeer drops messages to the specified peer.
func (s *EtcdServer) CutPeer(id types.ID) {
	tr, ok := s.r.transport.(*rafthttp.Transport)
	if ok {
		tr.CutPeer(id)
	}
}

// MendPeer recovers the message dropping behavior of the given peer.
func (s *EtcdServer) MendPeer(id types.ID) {
	tr, ok := s.r.transport.(*rafthttp.Transport)
	if ok {
		tr.MendPeer(id)
	}
}

func (s *EtcdServer) PauseSending() { s.r.pauseSending() }

func (s *EtcdServer) ResumeSending() { s.r.resumeSending() }

func (s *EtcdServer) ClusterVersion() *semver.Version {
	if s.cluster == nil {
		return nil
	}
	return s.cluster.Version()
}

func (s *EtcdServer) StorageVersion() *semver.Version {
	// `applySnapshot` sets a new backend instance, so we need to acquire the bemu lock.
	s.bemu.RLock()
	defer s.bemu.RUnlock()

	v, err := schema.DetectSchemaVersion(s.lg, s.be.ReadTx())
	if err != nil {
		s.lg.Warn("Failed to detect schema version", zap.Error(err))
		return nil
	}
	return &v
}

// monitorClusterVersions every monitorVersionInterval checks if it's the leader and updates cluster version if needed.
func (s *EtcdServer) monitorClusterVersions() {
	lg := s.Logger()
	monitor := serverversion.NewMonitor(lg, NewServerVersionAdapter(s))
	for {
		select {
		case <-s.firstCommitInTerm.Receive():
		case <-time.After(monitorVersionInterval):
		case <-s.stopping:
			lg.Info("server has stopped; stopping cluster version's monitor")
			return
		}

		if s.Leader() != s.MemberID() {
			continue
		}
		err := monitor.UpdateClusterVersionIfNeeded()
		if err != nil {
			s.lg.Error("Failed to monitor cluster version", zap.Error(err))
		}
	}
}

// monitorStorageVersion every monitorVersionInterval updates storage version if needed.
func (s *EtcdServer) monitorStorageVersion() {
	lg := s.Logger()
	monitor := serverversion.NewMonitor(lg, NewServerVersionAdapter(s))
	for {
		select {
		case <-time.After(monitorVersionInterval):
		case <-s.clusterVersionChanged.Receive():
		case <-s.stopping:
			lg.Info("server has stopped; stopping storage version's monitor")
			return
		}
		monitor.UpdateStorageVersionIfNeeded()
	}
}

func (s *EtcdServer) monitorKVHash() {
	t := s.Cfg.CorruptCheckTime
	if t == 0 {
		return
	}
	checkTicker := time.NewTicker(t)
	defer checkTicker.Stop()

	lg := s.Logger()
	lg.Info(
		"enabled corruption checking",
		zap.String("local-member-id", s.MemberID().String()),
		zap.Duration("interval", t),
	)
	for {
		select {
		case <-s.stopping:
			lg.Info("server has stopped; stopping kv hash's monitor")
			return
		case <-checkTicker.C:
		}
		backend.VerifyBackendConsistency(s.be, lg, false, schema.AllBuckets...)
		if !s.isLeader() {
			continue
		}
		if err := s.corruptionChecker.PeriodicCheck(); err != nil {
			lg.Warn("failed to check hash KV", zap.Error(err))
		}
	}
}

func (s *EtcdServer) monitorCompactHash() {
	if !s.Cfg.CompactHashCheckEnabled {
		return
	}
	t := s.Cfg.CompactHashCheckTime
	for {
		select {
		case <-time.After(t):
		case <-s.stopping:
			lg := s.Logger()
			lg.Info("server has stopped; stopping compact hash's monitor")
			return
		}
		if !s.isLeader() {
			continue
		}
		s.corruptionChecker.CompactHashCheck()
	}
}

func (s *EtcdServer) updateClusterVersionV3(ver string) {
	lg := s.Logger()

	if s.cluster.Version() == nil {
		lg.Info(
			"setting up initial cluster version using v3 API",
			zap.String("cluster-version", version.Cluster(ver)),
		)
	} else {
		lg.Info(
			"updating cluster version using v3 API",
			zap.String("from", version.Cluster(s.cluster.Version().String())),
			zap.String("to", version.Cluster(ver)),
		)
	}

	req := membershippb.ClusterVersionSetRequest{Ver: ver}

	ctx, cancel := context.WithTimeout(s.ctx, s.Cfg.ReqTimeout())
	_, err := s.raftRequest(ctx, pb.InternalRaftRequest{ClusterVersionSet: &req})
	cancel()

	switch {
	case errorspkg.Is(err, nil):
		lg.Info("cluster version is updated", zap.String("cluster-version", version.Cluster(ver)))
		return

	case errorspkg.Is(err, errors.ErrStopped):
		lg.Warn("aborting cluster version update; server is stopped", zap.Error(err))
		return

	default:
		lg.Warn("failed to update cluster version", zap.Error(err))
	}
}

// monitorDowngrade every DowngradeCheckTime checks if it's the leader and cancels downgrade if needed.
func (s *EtcdServer) monitorDowngrade() {
	monitor := serverversion.NewMonitor(s.Logger(), NewServerVersionAdapter(s))
	t := s.Cfg.DowngradeCheckTime
	if t == 0 {
		return
	}
	for {
		select {
		case <-time.After(t):
		case <-s.stopping:
			return
		}

		if !s.isLeader() {
			continue
		}
		monitor.CancelDowngradeIfNeeded()
	}
}

func (s *EtcdServer) parseProposeCtxErr(err error, start time.Time) error {
	switch {
	case errorspkg.Is(err, context.Canceled):
		return errors.ErrCanceled

	case errorspkg.Is(err, context.DeadlineExceeded):
		s.leadTimeMu.RLock()
		curLeadElected := s.leadElectedTime
		s.leadTimeMu.RUnlock()
		prevLeadLost := curLeadElected.Add(-2 * time.Duration(s.Cfg.ElectionTicks) * time.Duration(s.Cfg.TickMs) * time.Millisecond)
		if start.After(prevLeadLost) && start.Before(curLeadElected) {
			return errors.ErrTimeoutDueToLeaderFail
		}
		lead := types.ID(s.getLead())
		switch lead {
		case types.ID(raft.None):
			// TODO: return error to specify it happens because the cluster does not have leader now
		case s.MemberID():
			if !isConnectedToQuorumSince(s.r.transport, start, s.MemberID(), s.cluster.Members()) {
				return errors.ErrTimeoutDueToConnectionLost
			}
		default:
			if !isConnectedSince(s.r.transport, start, lead) {
				return errors.ErrTimeoutDueToConnectionLost
			}
		}
		return errors.ErrTimeout

	default:
		return err
	}
}

func (s *EtcdServer) KV() mvcc.WatchableKV { return s.kv }
func (s *EtcdServer) Backend() backend.Backend {
	s.bemu.RLock()
	defer s.bemu.RUnlock()
	return s.be
}

func (s *EtcdServer) AuthStore() auth.AuthStore { return s.authStore }

func (s *EtcdServer) restoreAlarms() error {
	as, err := v3alarm.NewAlarmStore(s.lg, schema.NewAlarmBackend(s.lg, s.be))
	if err != nil {
		return err
	}
	s.alarmStore = as
	return nil
}

// GoAttach creates a goroutine on a given function and tracks it using
// the etcdserver waitgroup.
// The passed function should interrupt on s.StoppingNotify().
func (s *EtcdServer) GoAttach(f func()) {
	s.wgMu.RLock() // this blocks with ongoing close(s.stopping)
	defer s.wgMu.RUnlock()
	select {
	case <-s.stopping:
		lg := s.Logger()
		lg.Warn("server has stopped; skipping GoAttach")
		return
	default:
	}

	// now safe to add since waitgroup wait has not started yet
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

func (s *EtcdServer) Alarms() []*pb.AlarmMember {
	return s.alarmStore.Get(pb.AlarmType_NONE)
}

// IsLearner returns if the local member is raft learner
func (s *EtcdServer) IsLearner() bool {
	return s.cluster.IsLocalMemberLearner()
}

// IsMemberExist returns if the member with the given id exists in cluster.
func (s *EtcdServer) IsMemberExist(id types.ID) bool {
	return s.cluster.IsMemberExist(id)
}

// raftStatus returns the raft status of this etcd node.
func (s *EtcdServer) raftStatus() raft.Status {
	return s.r.Node.Status()
}

func (s *EtcdServer) Version() *serverversion.Manager {
	return serverversion.NewManager(s.Logger(), NewServerVersionAdapter(s))
}

func (s *EtcdServer) getTxPostLockInsideApplyHook() func() {
	return func() {
		applyingIdx, applyingTerm := s.consistIndex.ConsistentApplyingIndex()
		if applyingIdx > s.consistIndex.UnsafeConsistentIndex() {
			s.consistIndex.SetConsistentIndex(applyingIdx, applyingTerm)
		}
	}
}

func (s *EtcdServer) CorruptionChecker() CorruptionChecker {
	return s.corruptionChecker
}
