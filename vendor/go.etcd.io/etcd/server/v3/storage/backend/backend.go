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

package backend

import (
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	humanize "github.com/dustin/go-humanize"
	"go.uber.org/zap"

	bolt "go.etcd.io/bbolt"
)

var (
	defaultBatchLimit    = 10000
	defaultBatchInterval = 100 * time.Millisecond

	defragLimit = 10000

	// InitialMmapSize is the initial size of the mmapped region. Setting this larger than
	// the potential max db size can prevent writer from blocking reader.
	// This only works for linux.
	InitialMmapSize = uint64(10 * 1024 * 1024 * 1024)

	// minSnapshotWarningTimeout is the minimum threshold to trigger a long running snapshot warning.
	minSnapshotWarningTimeout = 30 * time.Second
)

type Backend interface {
	// ReadTx returns a read transaction. It is replaced by ConcurrentReadTx in the main data path, see #10523.
	ReadTx() ReadTx
	BatchTx() BatchTx
	// ConcurrentReadTx returns a non-blocking read transaction.
	ConcurrentReadTx() ReadTx

	Snapshot() Snapshot
	Hash(ignores func(bucketName, keyName []byte) bool) (uint32, error)
	// Size returns the current size of the backend physically allocated.
	// The backend can hold DB space that is not utilized at the moment,
	// since it can conduct pre-allocation or spare unused space for recycling.
	// Use SizeInUse() instead for the actual DB size.
	Size() int64
	// SizeInUse returns the current size of the backend logically in use.
	// Since the backend can manage free space in a non-byte unit such as
	// number of pages, the returned value can be not exactly accurate in bytes.
	SizeInUse() int64
	// OpenReadTxN returns the number of currently open read transactions in the backend.
	OpenReadTxN() int64
	Defrag() error
	ForceCommit()
	Close() error

	// SetTxPostLockInsideApplyHook sets a txPostLockInsideApplyHook.
	SetTxPostLockInsideApplyHook(func())
}

type Snapshot interface {
	// Size gets the size of the snapshot.
	Size() int64
	// WriteTo writes the snapshot into the given writer.
	WriteTo(w io.Writer) (n int64, err error)
	// Close closes the snapshot.
	Close() error
}

type txReadBufferCache struct {
	mu         sync.Mutex
	buf        *txReadBuffer
	bufVersion uint64
}

type backend struct {
	// size and commits are used with atomic operations so they must be
	// 64-bit aligned, otherwise 32-bit tests will crash

	// size is the number of bytes allocated in the backend
	size int64
	// sizeInUse is the number of bytes actually used in the backend
	sizeInUse int64
	// commits counts number of commits since start
	commits int64
	// openReadTxN is the number of currently open read transactions in the backend
	openReadTxN int64
	// mlock prevents backend database file to be swapped
	mlock bool

	mu    sync.RWMutex
	bopts *bolt.Options
	db    *bolt.DB

	batchInterval time.Duration
	batchLimit    int
	batchTx       *batchTxBuffered

	readTx *readTx
	// txReadBufferCache mirrors "txReadBuffer" within "readTx" -- readTx.baseReadTx.buf.
	// When creating "concurrentReadTx":
	// - if the cache is up-to-date, "readTx.baseReadTx.buf" copy can be skipped
	// - if the cache is empty or outdated, "readTx.baseReadTx.buf" copy is required
	txReadBufferCache txReadBufferCache

	stopc chan struct{}
	donec chan struct{}

	hooks Hooks

	// txPostLockInsideApplyHook is called each time right after locking the tx.
	txPostLockInsideApplyHook func()

	lg *zap.Logger
}

type BackendConfig struct {
	// Path is the file path to the backend file.
	Path string
	// BatchInterval is the maximum time before flushing the BatchTx.
	BatchInterval time.Duration
	// BatchLimit is the maximum puts before flushing the BatchTx.
	BatchLimit int
	// BackendFreelistType is the backend boltdb's freelist type.
	BackendFreelistType bolt.FreelistType
	// MmapSize is the number of bytes to mmap for the backend.
	MmapSize uint64
	// Logger logs backend-side operations.
	Logger *zap.Logger
	// UnsafeNoFsync disables all uses of fsync.
	UnsafeNoFsync bool `json:"unsafe-no-fsync"`
	// Mlock prevents backend database file to be swapped
	Mlock bool

	// Hooks are getting executed during lifecycle of Backend's transactions.
	Hooks Hooks
}

type BackendConfigOption func(*BackendConfig)

func DefaultBackendConfig(lg *zap.Logger) BackendConfig {
	return BackendConfig{
		BatchInterval: defaultBatchInterval,
		BatchLimit:    defaultBatchLimit,
		MmapSize:      InitialMmapSize,
		Logger:        lg,
	}
}

func New(bcfg BackendConfig) Backend {
	return newBackend(bcfg)
}

func WithMmapSize(size uint64) BackendConfigOption {
	return func(bcfg *BackendConfig) {
		bcfg.MmapSize = size
	}
}

func NewDefaultBackend(lg *zap.Logger, path string, opts ...BackendConfigOption) Backend {
	bcfg := DefaultBackendConfig(lg)
	bcfg.Path = path
	for _, opt := range opts {
		opt(&bcfg)
	}

	return newBackend(bcfg)
}

func newBackend(bcfg BackendConfig) *backend {
	bopts := &bolt.Options{}
	if boltOpenOptions != nil {
		*bopts = *boltOpenOptions
	}
	bopts.InitialMmapSize = bcfg.mmapSize()
	bopts.FreelistType = bcfg.BackendFreelistType
	bopts.NoSync = bcfg.UnsafeNoFsync
	bopts.NoGrowSync = bcfg.UnsafeNoFsync
	bopts.Mlock = bcfg.Mlock
	bopts.Logger = newBoltLoggerZap(bcfg)

	db, err := bolt.Open(bcfg.Path, 0600, bopts)
	if err != nil {
		bcfg.Logger.Panic("failed to open database", zap.String("path", bcfg.Path), zap.Error(err))
	}

	// In future, may want to make buffering optional for low-concurrency systems
	// or dynamically swap between buffered/non-buffered depending on workload.
	b := &backend{
		bopts: bopts,
		db:    db,

		batchInterval: bcfg.BatchInterval,
		batchLimit:    bcfg.BatchLimit,
		mlock:         bcfg.Mlock,

		readTx: &readTx{
			baseReadTx: baseReadTx{
				buf: txReadBuffer{
					txBuffer:   txBuffer{make(map[BucketID]*bucketBuffer)},
					bufVersion: 0,
				},
				buckets: make(map[BucketID]*bolt.Bucket),
				txWg:    new(sync.WaitGroup),
				txMu:    new(sync.RWMutex),
			},
		},
		txReadBufferCache: txReadBufferCache{
			mu:         sync.Mutex{},
			bufVersion: 0,
			buf:        nil,
		},

		stopc: make(chan struct{}),
		donec: make(chan struct{}),

		lg: bcfg.Logger,
	}

	b.batchTx = newBatchTxBuffered(b)
	// We set it after newBatchTxBuffered to skip the 'empty' commit.
	b.hooks = bcfg.Hooks

	go b.run()
	return b
}

// BatchTx returns the current batch tx in coalescer. The tx can be used for read and
// write operations. The write result can be retrieved within the same tx immediately.
// The write result is isolated with other txs until the current one get committed.
func (b *backend) BatchTx() BatchTx {
	return b.batchTx
}

func (b *backend) SetTxPostLockInsideApplyHook(hook func()) {
	// It needs to lock the batchTx, because the periodic commit
	// may be accessing the txPostLockInsideApplyHook at the moment.
	b.batchTx.lock()
	defer b.batchTx.Unlock()
	b.txPostLockInsideApplyHook = hook
}

func (b *backend) ReadTx() ReadTx { return b.readTx }

// ConcurrentReadTx creates and returns a new ReadTx, which:
// A) creates and keeps a copy of backend.readTx.txReadBuffer,
// B) references the boltdb read Tx (and its bucket cache) of current batch interval.
func (b *backend) ConcurrentReadTx() ReadTx {
	b.readTx.RLock()
	defer b.readTx.RUnlock()
	// prevent boltdb read Tx from been rolled back until store read Tx is done. Needs to be called when holding readTx.RLock().
	b.readTx.txWg.Add(1)

	// TODO: might want to copy the read buffer lazily - create copy when A) end of a write transaction B) end of a batch interval.

	// inspect/update cache recency iff there's no ongoing update to the cache
	// this falls through if there's no cache update

	// by this line, "ConcurrentReadTx" code path is already protected against concurrent "writeback" operations
	// which requires write lock to update "readTx.baseReadTx.buf".
	// Which means setting "buf *txReadBuffer" with "readTx.buf.unsafeCopy()" is guaranteed to be up-to-date,
	// whereas "txReadBufferCache.buf" may be stale from concurrent "writeback" operations.
	// We only update "txReadBufferCache.buf" if we know "buf *txReadBuffer" is up-to-date.
	// The update to "txReadBufferCache.buf" will benefit the following "ConcurrentReadTx" creation
	// by avoiding copying "readTx.baseReadTx.buf".
	b.txReadBufferCache.mu.Lock()

	curCache := b.txReadBufferCache.buf
	curCacheVer := b.txReadBufferCache.bufVersion
	curBufVer := b.readTx.buf.bufVersion

	isEmptyCache := curCache == nil
	isStaleCache := curCacheVer != curBufVer

	var buf *txReadBuffer
	switch {
	case isEmptyCache:
		// perform safe copy of buffer while holding "b.txReadBufferCache.mu.Lock"
		// this is only supposed to run once so there won't be much overhead
		curBuf := b.readTx.buf.unsafeCopy()
		buf = &curBuf
	case isStaleCache:
		// to maximize the concurrency, try unsafe copy of buffer
		// release the lock while copying buffer -- cache may become stale again and
		// get overwritten by someone else.
		// therefore, we need to check the readTx buffer version again
		b.txReadBufferCache.mu.Unlock()
		curBuf := b.readTx.buf.unsafeCopy()
		b.txReadBufferCache.mu.Lock()
		buf = &curBuf
	default:
		// neither empty nor stale cache, just use the current buffer
		buf = curCache
	}
	// txReadBufferCache.bufVersion can be modified when we doing an unsafeCopy()
	// as a result, curCacheVer could be no longer the same as
	// txReadBufferCache.bufVersion
	// if !isEmptyCache && curCacheVer != b.txReadBufferCache.bufVersion
	// then the cache became stale while copying "readTx.baseReadTx.buf".
	// It is safe to not update "txReadBufferCache.buf", because the next following
	// "ConcurrentReadTx" creation will trigger a new "readTx.baseReadTx.buf" copy
	// and "buf" is still used for the current "concurrentReadTx.baseReadTx.buf".
	if isEmptyCache || curCacheVer == b.txReadBufferCache.bufVersion {
		// continue if the cache is never set or no one has modified the cache
		b.txReadBufferCache.buf = buf
		b.txReadBufferCache.bufVersion = curBufVer
	}

	b.txReadBufferCache.mu.Unlock()

	// concurrentReadTx is not supposed to write to its txReadBuffer
	return &concurrentReadTx{
		baseReadTx: baseReadTx{
			buf:     *buf,
			txMu:    b.readTx.txMu,
			tx:      b.readTx.tx,
			buckets: b.readTx.buckets,
			txWg:    b.readTx.txWg,
		},
	}
}

// ForceCommit forces the current batching tx to commit.
func (b *backend) ForceCommit() {
	b.batchTx.Commit()
}

func (b *backend) Snapshot() Snapshot {
	b.batchTx.Commit()

	b.mu.RLock()
	defer b.mu.RUnlock()
	tx, err := b.db.Begin(false)
	if err != nil {
		b.lg.Fatal("failed to begin tx", zap.Error(err))
	}

	stopc, donec := make(chan struct{}), make(chan struct{})
	dbBytes := tx.Size()
	go func() {
		defer close(donec)
		// sendRateBytes is based on transferring snapshot data over a 1 gigabit/s connection
		// assuming a min tcp throughput of 100MB/s.
		var sendRateBytes int64 = 100 * 1024 * 1024
		warningTimeout := time.Duration(int64((float64(dbBytes) / float64(sendRateBytes)) * float64(time.Second)))
		if warningTimeout < minSnapshotWarningTimeout {
			warningTimeout = minSnapshotWarningTimeout
		}
		start := time.Now()
		ticker := time.NewTicker(warningTimeout)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.lg.Warn(
					"snapshotting taking too long to transfer",
					zap.Duration("taking", time.Since(start)),
					zap.Int64("bytes", dbBytes),
					zap.String("size", humanize.Bytes(uint64(dbBytes))),
				)

			case <-stopc:
				snapshotTransferSec.Observe(time.Since(start).Seconds())
				return
			}
		}
	}()

	return &snapshot{tx, stopc, donec}
}

func (b *backend) Hash(ignores func(bucketName, keyName []byte) bool) (uint32, error) {
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))

	b.mu.RLock()
	defer b.mu.RUnlock()
	err := b.db.View(func(tx *bolt.Tx) error {
		c := tx.Cursor()
		for next, _ := c.First(); next != nil; next, _ = c.Next() {
			b := tx.Bucket(next)
			if b == nil {
				return fmt.Errorf("cannot get hash of bucket %s", next)
			}
			h.Write(next)
			b.ForEach(func(k, v []byte) error {
				if ignores != nil && !ignores(next, k) {
					h.Write(k)
					h.Write(v)
				}
				return nil
			})
		}
		return nil
	})

	if err != nil {
		return 0, err
	}

	return h.Sum32(), nil
}

func (b *backend) Size() int64 {
	return atomic.LoadInt64(&b.size)
}

func (b *backend) SizeInUse() int64 {
	return atomic.LoadInt64(&b.sizeInUse)
}

func (b *backend) run() {
	defer close(b.donec)
	t := time.NewTimer(b.batchInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-b.stopc:
			b.batchTx.CommitAndStop()
			return
		}
		if b.batchTx.safePending() != 0 {
			b.batchTx.Commit()
		}
		t.Reset(b.batchInterval)
	}
}

func (b *backend) Close() error {
	close(b.stopc)
	<-b.donec
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.db.Close()
}

// Commits returns total number of commits since start
func (b *backend) Commits() int64 {
	return atomic.LoadInt64(&b.commits)
}

func (b *backend) Defrag() error {
	return b.defrag()
}

func (b *backend) defrag() error {
	now := time.Now()
	isDefragActive.Set(1)
	defer isDefragActive.Set(0)

	// TODO: make this non-blocking?
	// lock batchTx to ensure nobody is using previous tx, and then
	// close previous ongoing tx.
	b.batchTx.LockOutsideApply()
	defer b.batchTx.Unlock()

	// lock database after lock tx to avoid deadlock.
	b.mu.Lock()
	defer b.mu.Unlock()

	// block concurrent read requests while resetting tx
	b.readTx.Lock()
	defer b.readTx.Unlock()

	// Create a temporary file to ensure we start with a clean slate.
	// Snapshotter.cleanupSnapdir cleans up any of these that are found during startup.
	dir := filepath.Dir(b.db.Path())
	temp, err := os.CreateTemp(dir, "db.tmp.*")
	if err != nil {
		return err
	}

	options := bolt.Options{}
	if boltOpenOptions != nil {
		options = *boltOpenOptions
	}
	options.OpenFile = func(_ string, _ int, _ os.FileMode) (file *os.File, err error) {
		// gofail: var defragOpenFileError string
		// return nil, fmt.Errorf(defragOpenFileError)
		return temp, nil
	}
	// Don't load tmp db into memory regardless of opening options
	options.Mlock = false
	tdbp := temp.Name()
	tmpdb, err := bolt.Open(tdbp, 0600, &options)
	if err != nil {
		temp.Close()
		if rmErr := os.Remove(temp.Name()); rmErr != nil && b.lg != nil {
			b.lg.Error(
				"failed to remove temporary file",
				zap.String("path", temp.Name()),
				zap.Error(rmErr),
			)
		}

		return err
	}

	dbp := b.db.Path()
	size1, sizeInUse1 := b.Size(), b.SizeInUse()
	if b.lg != nil {
		b.lg.Info(
			"defragmenting",
			zap.String("path", dbp),
			zap.Int64("current-db-size-bytes", size1),
			zap.String("current-db-size", humanize.Bytes(uint64(size1))),
			zap.Int64("current-db-size-in-use-bytes", sizeInUse1),
			zap.String("current-db-size-in-use", humanize.Bytes(uint64(sizeInUse1))),
		)
	}

	// Commit/stop and then reset current transactions (including the readTx)
	b.batchTx.unsafeCommit(true)
	b.batchTx.tx = nil

	// gofail: var defragBeforeCopy struct{}
	err = defragdb(b.db, tmpdb, defragLimit)
	if err != nil {
		tmpdb.Close()
		if rmErr := os.RemoveAll(tmpdb.Path()); rmErr != nil {
			b.lg.Error("failed to remove db.tmp after defragmentation completed", zap.Error(rmErr))
		}

		// restore the bbolt transactions if defragmentation fails
		b.batchTx.tx = b.unsafeBegin(true)
		b.readTx.tx = b.unsafeBegin(false)

		return err
	}

	err = b.db.Close()
	if err != nil {
		b.lg.Fatal("failed to close database", zap.Error(err))
	}
	err = tmpdb.Close()
	if err != nil {
		b.lg.Fatal("failed to close tmp database", zap.Error(err))
	}
	// gofail: var defragBeforeRename struct{}
	err = os.Rename(tdbp, dbp)
	if err != nil {
		b.lg.Fatal("failed to rename tmp database", zap.Error(err))
	}

	b.db, err = bolt.Open(dbp, 0600, b.bopts)
	if err != nil {
		b.lg.Fatal("failed to open database", zap.String("path", dbp), zap.Error(err))
	}
	b.batchTx.tx = b.unsafeBegin(true)

	b.readTx.reset()
	b.readTx.tx = b.unsafeBegin(false)

	size := b.readTx.tx.Size()
	db := b.readTx.tx.DB()
	atomic.StoreInt64(&b.size, size)
	atomic.StoreInt64(&b.sizeInUse, size-(int64(db.Stats().FreePageN)*int64(db.Info().PageSize)))

	took := time.Since(now)
	defragSec.Observe(took.Seconds())

	size2, sizeInUse2 := b.Size(), b.SizeInUse()
	if b.lg != nil {
		b.lg.Info(
			"finished defragmenting directory",
			zap.String("path", dbp),
			zap.Int64("current-db-size-bytes-diff", size2-size1),
			zap.Int64("current-db-size-bytes", size2),
			zap.String("current-db-size", humanize.Bytes(uint64(size2))),
			zap.Int64("current-db-size-in-use-bytes-diff", sizeInUse2-sizeInUse1),
			zap.Int64("current-db-size-in-use-bytes", sizeInUse2),
			zap.String("current-db-size-in-use", humanize.Bytes(uint64(sizeInUse2))),
			zap.Duration("took", took),
		)
	}
	return nil
}

func defragdb(odb, tmpdb *bolt.DB, limit int) error {
	// gofail: var defragdbFail string
	// return fmt.Errorf(defragdbFail)

	// open a tx on tmpdb for writes
	tmptx, err := tmpdb.Begin(true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmptx.Rollback()
		}
	}()

	// open a tx on old db for read
	tx, err := odb.Begin(false)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	c := tx.Cursor()

	count := 0
	for next, _ := c.First(); next != nil; next, _ = c.Next() {
		b := tx.Bucket(next)
		if b == nil {
			return fmt.Errorf("backend: cannot defrag bucket %s", next)
		}

		tmpb, berr := tmptx.CreateBucketIfNotExists(next)
		if berr != nil {
			return berr
		}
		tmpb.FillPercent = 0.9 // for bucket2seq write in for each

		if err = b.ForEach(func(k, v []byte) error {
			count++
			if count > limit {
				err = tmptx.Commit()
				if err != nil {
					return err
				}
				tmptx, err = tmpdb.Begin(true)
				if err != nil {
					return err
				}
				tmpb = tmptx.Bucket(next)
				tmpb.FillPercent = 0.9 // for bucket2seq write in for each

				count = 0
			}
			return tmpb.Put(k, v)
		}); err != nil {
			return err
		}
	}

	return tmptx.Commit()
}

func (b *backend) begin(write bool) *bolt.Tx {
	b.mu.RLock()
	tx := b.unsafeBegin(write)
	b.mu.RUnlock()

	size := tx.Size()
	db := tx.DB()
	stats := db.Stats()
	atomic.StoreInt64(&b.size, size)
	atomic.StoreInt64(&b.sizeInUse, size-(int64(stats.FreePageN)*int64(db.Info().PageSize)))
	atomic.StoreInt64(&b.openReadTxN, int64(stats.OpenTxN))

	return tx
}

func (b *backend) unsafeBegin(write bool) *bolt.Tx {
	// gofail: var beforeStartDBTxn struct{}
	tx, err := b.db.Begin(write)
	// gofail: var afterStartDBTxn struct{}
	if err != nil {
		b.lg.Fatal("failed to begin tx", zap.Error(err))
	}
	return tx
}

func (b *backend) OpenReadTxN() int64 {
	return atomic.LoadInt64(&b.openReadTxN)
}

type snapshot struct {
	*bolt.Tx
	stopc chan struct{}
	donec chan struct{}
}

func (s *snapshot) Close() error {
	close(s.stopc)
	<-s.donec
	return s.Tx.Rollback()
}

func newBoltLoggerZap(bcfg BackendConfig) bolt.Logger {
	lg := bcfg.Logger.Named("bbolt")
	return &zapBoltLogger{lg.WithOptions(zap.AddCallerSkip(1)).Sugar()}
}

type zapBoltLogger struct {
	*zap.SugaredLogger
}

func (zl *zapBoltLogger) Warning(args ...any) {
	zl.SugaredLogger.Warn(args...)
}

func (zl *zapBoltLogger) Warningf(format string, args ...any) {
	zl.SugaredLogger.Warnf(format, args...)
}
