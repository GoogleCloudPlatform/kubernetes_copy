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

package wal

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/pkg/v3/pbutil"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
	"go.etcd.io/raft/v3"
	"go.etcd.io/raft/v3/raftpb"
)

const (
	MetadataType int64 = iota + 1
	EntryType
	StateType
	CrcType
	SnapshotType

	// warnSyncDuration is the amount of time allotted to an fsync before
	// logging a warning
	warnSyncDuration = time.Second
)

var (
	// SegmentSizeBytes is the preallocated size of each wal segment file.
	// The actual size might be larger than this. In general, the default
	// value should be used, but this is defined as an exported variable
	// so that tests can set a different segment size.
	SegmentSizeBytes int64 = 64 * 1000 * 1000 // 64MB

	ErrMetadataConflict = errors.New("wal: conflicting metadata found")
	ErrFileNotFound     = errors.New("wal: file not found")
	ErrCRCMismatch      = walpb.ErrCRCMismatch
	ErrSnapshotMismatch = errors.New("wal: snapshot mismatch")
	ErrSnapshotNotFound = errors.New("wal: snapshot not found")
	ErrSliceOutOfRange  = errors.New("wal: slice bounds out of range")
	ErrDecoderNotFound  = errors.New("wal: decoder not found")
	crcTable            = crc32.MakeTable(crc32.Castagnoli)
)

// WAL is a logical representation of the stable storage.
// WAL is either in read mode or append mode but not both.
// A newly created WAL is in append mode, and ready for appending records.
// A just opened WAL is in read mode, and ready for reading records.
// The WAL will be ready for appending after reading out all the previous records.
type WAL struct {
	lg *zap.Logger

	dir string // the living directory of the underlay files

	// dirFile is a fd for the wal directory for syncing on Rename
	dirFile *os.File

	metadata []byte           // metadata recorded at the head of each WAL
	state    raftpb.HardState // hardstate recorded at the head of WAL

	start     walpb.Snapshot // snapshot to start reading
	decoder   Decoder        // decoder to Decode records
	readClose func() error   // closer for Decode reader

	unsafeNoSync bool // if set, do not fsync

	mu      sync.Mutex
	enti    uint64   // index of the last entry saved to the wal
	encoder *encoder // encoder to encode records

	locks []*fileutil.LockedFile // the locked files the WAL holds (the name is increasing)
	fp    *filePipeline
}

// Create creates a WAL ready for appending records. The given metadata is
// recorded at the head of each WAL file, and can be retrieved with ReadAll
// after the file is Open.
func Create(lg *zap.Logger, dirpath string, metadata []byte) (*WAL, error) {
	if Exist(dirpath) {
		return nil, os.ErrExist
	}

	if lg == nil {
		lg = zap.NewNop()
	}

	// keep temporary wal directory so WAL initialization appears atomic
	tmpdirpath := filepath.Clean(dirpath) + ".tmp"
	if fileutil.Exist(tmpdirpath) {
		if err := os.RemoveAll(tmpdirpath); err != nil {
			return nil, err
		}
	}
	defer os.RemoveAll(tmpdirpath)

	if err := fileutil.CreateDirAll(lg, tmpdirpath); err != nil {
		lg.Warn(
			"failed to create a temporary WAL directory",
			zap.String("tmp-dir-path", tmpdirpath),
			zap.String("dir-path", dirpath),
			zap.Error(err),
		)
		return nil, err
	}

	p := filepath.Join(tmpdirpath, walName(0, 0))
	f, err := createNewWALFile[*fileutil.LockedFile](p, false)
	if err != nil {
		lg.Warn(
			"failed to flock an initial WAL file",
			zap.String("path", p),
			zap.Error(err),
		)
		return nil, err
	}
	if _, err = f.Seek(0, io.SeekEnd); err != nil {
		lg.Warn(
			"failed to seek an initial WAL file",
			zap.String("path", p),
			zap.Error(err),
		)
		return nil, err
	}
	if err = fileutil.Preallocate(f.File, SegmentSizeBytes, true); err != nil {
		lg.Warn(
			"failed to preallocate an initial WAL file",
			zap.String("path", p),
			zap.Int64("segment-bytes", SegmentSizeBytes),
			zap.Error(err),
		)
		return nil, err
	}

	w := &WAL{
		lg:       lg,
		dir:      dirpath,
		metadata: metadata,
	}
	w.encoder, err = newFileEncoder(f.File, 0)
	if err != nil {
		return nil, err
	}
	w.locks = append(w.locks, f)
	if err = w.saveCrc(0); err != nil {
		return nil, err
	}
	if err = w.encoder.encode(&walpb.Record{Type: MetadataType, Data: metadata}); err != nil {
		return nil, err
	}
	if err = w.SaveSnapshot(walpb.Snapshot{}); err != nil {
		return nil, err
	}

	logDirPath := w.dir
	if w, err = w.renameWAL(tmpdirpath); err != nil {
		lg.Warn(
			"failed to rename the temporary WAL directory",
			zap.String("tmp-dir-path", tmpdirpath),
			zap.String("dir-path", logDirPath),
			zap.Error(err),
		)
		return nil, err
	}

	var perr error
	defer func() {
		if perr != nil {
			w.cleanupWAL(lg)
		}
	}()

	// directory was renamed; sync parent dir to persist rename
	pdir, perr := fileutil.OpenDir(filepath.Dir(w.dir))
	if perr != nil {
		lg.Warn(
			"failed to open the parent data directory",
			zap.String("parent-dir-path", filepath.Dir(w.dir)),
			zap.String("dir-path", w.dir),
			zap.Error(perr),
		)
		return nil, perr
	}
	dirCloser := func() error {
		if perr = pdir.Close(); perr != nil {
			lg.Warn(
				"failed to close the parent data directory file",
				zap.String("parent-dir-path", filepath.Dir(w.dir)),
				zap.String("dir-path", w.dir),
				zap.Error(perr),
			)
			return perr
		}
		return nil
	}
	start := time.Now()
	if perr = fileutil.Fsync(pdir); perr != nil {
		dirCloser()
		lg.Warn(
			"failed to fsync the parent data directory file",
			zap.String("parent-dir-path", filepath.Dir(w.dir)),
			zap.String("dir-path", w.dir),
			zap.Error(perr),
		)
		return nil, perr
	}
	walFsyncSec.Observe(time.Since(start).Seconds())
	if err = dirCloser(); err != nil {
		return nil, err
	}

	return w, nil
}

// createNewWALFile creates a WAL file.
// To create a locked file, use *fileutil.LockedFile type parameter.
// To create a standard file, use *os.File type parameter.
// If forceNew is true, the file will be truncated if it already exists.
func createNewWALFile[T *os.File | *fileutil.LockedFile](path string, forceNew bool) (T, error) {
	flag := os.O_WRONLY | os.O_CREATE
	if forceNew {
		flag |= os.O_TRUNC
	}

	if _, isLockedFile := any(T(nil)).(*fileutil.LockedFile); isLockedFile {
		lockedFile, err := fileutil.LockFile(path, flag, fileutil.PrivateFileMode)
		if err != nil {
			return nil, err
		}
		return any(lockedFile).(T), nil
	}

	file, err := os.OpenFile(path, flag, fileutil.PrivateFileMode)
	if err != nil {
		return nil, err
	}
	return any(file).(T), nil
}

func (w *WAL) Reopen(lg *zap.Logger, snap walpb.Snapshot) (*WAL, error) {
	err := w.Close()
	if err != nil {
		lg.Panic("failed to close WAL during reopen", zap.Error(err))
	}
	return Open(lg, w.dir, snap)
}

func (w *WAL) SetUnsafeNoFsync() {
	w.unsafeNoSync = true
}

func (w *WAL) cleanupWAL(lg *zap.Logger) {
	var err error
	if err = w.Close(); err != nil {
		lg.Panic("failed to close WAL during cleanup", zap.Error(err))
	}
	brokenDirName := fmt.Sprintf("%s.broken.%v", w.dir, time.Now().Format("20060102.150405.999999"))
	if err = os.Rename(w.dir, brokenDirName); err != nil {
		lg.Panic(
			"failed to rename WAL during cleanup",
			zap.Error(err),
			zap.String("source-path", w.dir),
			zap.String("rename-path", brokenDirName),
		)
	}
}

func (w *WAL) renameWAL(tmpdirpath string) (*WAL, error) {
	if err := os.RemoveAll(w.dir); err != nil {
		return nil, err
	}
	// On non-Windows platforms, hold the lock while renaming. Releasing
	// the lock and trying to reacquire it quickly can be flaky because
	// it's possible the process will fork to spawn a process while this is
	// happening. The fds are set up as close-on-exec by the Go runtime,
	// but there is a window between the fork and the exec where another
	// process holds the lock.
	if err := os.Rename(tmpdirpath, w.dir); err != nil {
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) {
			return w.renameWALUnlock(tmpdirpath)
		}
		return nil, err
	}
	w.fp = newFilePipeline(w.lg, w.dir, SegmentSizeBytes)
	df, err := fileutil.OpenDir(w.dir)
	w.dirFile = df
	return w, err
}

func (w *WAL) renameWALUnlock(tmpdirpath string) (*WAL, error) {
	// rename of directory with locked files doesn't work on windows/cifs;
	// close the WAL to release the locks so the directory can be renamed.
	w.lg.Info(
		"closing WAL to release flock and retry directory renaming",
		zap.String("from", tmpdirpath),
		zap.String("to", w.dir),
	)
	w.Close()

	if err := os.Rename(tmpdirpath, w.dir); err != nil {
		return nil, err
	}

	// reopen and relock
	newWAL, oerr := Open(w.lg, w.dir, walpb.Snapshot{})
	if oerr != nil {
		return nil, oerr
	}
	if _, _, _, err := newWAL.ReadAll(); err != nil {
		newWAL.Close()
		return nil, err
	}
	return newWAL, nil
}

// Open opens the WAL at the given snap.
// The snap SHOULD have been previously saved to the WAL, or the following
// ReadAll will fail.
// The returned WAL is ready to read and the first record will be the one after
// the given snap. The WAL cannot be appended to before reading out all of its
// previous records.
func Open(lg *zap.Logger, dirpath string, snap walpb.Snapshot) (*WAL, error) {
	w, err := openAtIndex(lg, dirpath, snap, true)
	if err != nil {
		return nil, fmt.Errorf("openAtIndex failed: %w", err)
	}
	if w.dirFile, err = fileutil.OpenDir(w.dir); err != nil {
		return nil, fmt.Errorf("fileutil.OpenDir failed: %w", err)
	}
	return w, nil
}

// OpenForRead only opens the wal files for read.
// Write on a read only wal panics.
func OpenForRead(lg *zap.Logger, dirpath string, snap walpb.Snapshot) (*WAL, error) {
	return openAtIndex(lg, dirpath, snap, false)
}

func openAtIndex(lg *zap.Logger, dirpath string, snap walpb.Snapshot, write bool) (*WAL, error) {
	if lg == nil {
		lg = zap.NewNop()
	}
	names, nameIndex, err := selectWALFiles(lg, dirpath, snap)
	if err != nil {
		return nil, fmt.Errorf("[openAtIndex] selectWALFiles failed: %w", err)
	}

	rs, ls, closer, err := openWALFiles(lg, dirpath, names, nameIndex, write)
	if err != nil {
		return nil, fmt.Errorf("[openAtIndex] openWALFiles failed: %w", err)
	}

	// create a WAL ready for reading
	w := &WAL{
		lg:        lg,
		dir:       dirpath,
		start:     snap,
		decoder:   NewDecoder(rs...),
		readClose: closer,
		locks:     ls,
	}

	if write {
		// write reuses the file descriptors from read; don't close so
		// WAL can append without dropping the file lock
		w.readClose = nil
		if _, _, err := parseWALName(filepath.Base(w.tail().Name())); err != nil {
			closer()
			return nil, fmt.Errorf("[openAtIndex] parseWALName failed: %w", err)
		}
		w.fp = newFilePipeline(lg, w.dir, SegmentSizeBytes)
	}

	return w, nil
}

func selectWALFiles(lg *zap.Logger, dirpath string, snap walpb.Snapshot) ([]string, int, error) {
	names, err := readWALNames(lg, dirpath)
	if err != nil {
		return nil, -1, fmt.Errorf("readWALNames failed: %w", err)
	}

	nameIndex, ok := searchIndex(lg, names, snap.Index)
	if !ok {
		return nil, -1, fmt.Errorf("wal: file not found which matches the snapshot index '%d'", snap.Index)
	}

	if !isValidSeq(lg, names[nameIndex:]) {
		return nil, -1, fmt.Errorf("wal: file sequence numbers (starting from %d) do not increase continuously", nameIndex)
	}

	return names, nameIndex, nil
}

func openWALFiles(lg *zap.Logger, dirpath string, names []string, nameIndex int, write bool) ([]fileutil.FileReader, []*fileutil.LockedFile, func() error, error) {
	rcs := make([]io.ReadCloser, 0)
	rs := make([]fileutil.FileReader, 0)
	ls := make([]*fileutil.LockedFile, 0)
	for _, name := range names[nameIndex:] {
		p := filepath.Join(dirpath, name)
		var f *os.File
		if write {
			l, err := fileutil.TryLockFile(p, os.O_RDWR, fileutil.PrivateFileMode)
			if err != nil {
				closeAll(lg, rcs...)
				return nil, nil, nil, fmt.Errorf("[openWALFiles] fileutil.TryLockFile failed: %w", err)
			}
			ls = append(ls, l)
			rcs = append(rcs, l)
			f = l.File
		} else {
			rf, err := os.OpenFile(p, os.O_RDONLY, fileutil.PrivateFileMode)
			if err != nil {
				closeAll(lg, rcs...)
				return nil, nil, nil, fmt.Errorf("[openWALFiles] os.OpenFile failed (%q): %w", p, err)
			}
			ls = append(ls, nil)
			rcs = append(rcs, rf)
			f = rf
		}
		fileReader := fileutil.NewFileReader(f)
		rs = append(rs, fileReader)
	}

	closer := func() error { return closeAll(lg, rcs...) }

	return rs, ls, closer, nil
}

// ReadAll reads out records of the current WAL.
// If opened in write mode, it must read out all records until EOF. Or an error
// will be returned.
// If opened in read mode, it will try to read all records if possible.
// If it cannot read out the expected snap, it will return ErrSnapshotNotFound.
// If loaded snap doesn't match with the expected one, it will return
// all the records and error ErrSnapshotMismatch.
// TODO: detect not-last-snap error.
// TODO: maybe loose the checking of match.
// After ReadAll, the WAL will be ready for appending new records.
//
// ReadAll suppresses WAL entries that got overridden (i.e. a newer entry with the same index
// exists in the log). Such a situation can happen in cases described in figure 7. of the
// RAFT paper (http://web.stanford.edu/~ouster/cgi-bin/papers/raft-atc14.pdf).
//
// ReadAll may return uncommitted yet entries, that are subject to be overridden.
// Do not apply entries that have index > state.commit, as they are subject to change.
func (w *WAL) ReadAll() (metadata []byte, state raftpb.HardState, ents []raftpb.Entry, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	rec := &walpb.Record{}

	if w.decoder == nil {
		return nil, state, nil, ErrDecoderNotFound
	}
	decoder := w.decoder

	var match bool
	for err = decoder.Decode(rec); err == nil; err = decoder.Decode(rec) {
		switch rec.Type {
		case EntryType:
			e := MustUnmarshalEntry(rec.Data)
			// 0 <= e.Index-w.start.Index - 1 < len(ents)
			if e.Index > w.start.Index {
				// prevent "panic: runtime error: slice bounds out of range [:13038096702221461992] with capacity 0"
				up := e.Index - w.start.Index - 1
				if up > uint64(len(ents)) {
					// return error before append call causes runtime panic
					return nil, state, nil, ErrSliceOutOfRange
				}
				// The line below is potentially overriding some 'uncommitted' entries.
				ents = append(ents[:up], e)
			}
			w.enti = e.Index

		case StateType:
			state = MustUnmarshalState(rec.Data)

		case MetadataType:
			if metadata != nil && !bytes.Equal(metadata, rec.Data) {
				state.Reset()
				return nil, state, nil, ErrMetadataConflict
			}
			metadata = rec.Data

		case CrcType:
			crc := decoder.LastCRC()
			// current crc of decoder must match the crc of the record.
			// do no need to match 0 crc, since the decoder is a new one at this case.
			if crc != 0 && rec.Validate(crc) != nil {
				state.Reset()
				return nil, state, nil, ErrCRCMismatch
			}
			decoder.UpdateCRC(rec.Crc)

		case SnapshotType:
			var snap walpb.Snapshot
			pbutil.MustUnmarshal(&snap, rec.Data)
			if snap.Index == w.start.Index {
				if snap.Term != w.start.Term {
					state.Reset()
					return nil, state, nil, ErrSnapshotMismatch
				}
				match = true
			}

		default:
			state.Reset()
			return nil, state, nil, fmt.Errorf("unexpected block type %d", rec.Type)
		}
	}

	switch w.tail() {
	case nil:
		// We do not have to read out all entries in read mode.
		// The last record maybe a partial written one, so
		// `io.ErrUnexpectedEOF` might be returned.
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			state.Reset()
			return nil, state, nil, err
		}
	default:
		// We must read all the entries if WAL is opened in write mode.
		if !errors.Is(err, io.EOF) {
			state.Reset()
			return nil, state, nil, err
		}
		// decodeRecord() will return io.EOF if it detects a zero record,
		// but this zero record may be followed by non-zero records from
		// a torn write. Overwriting some of these non-zero records, but
		// not all, will cause CRC errors on WAL open. Since the records
		// were never fully synced to disk in the first place, it's safe
		// to zero them out to avoid any CRC errors from new writes.
		if _, err = w.tail().Seek(w.decoder.LastOffset(), io.SeekStart); err != nil {
			return nil, state, nil, err
		}
		if err = fileutil.ZeroToEnd(w.tail().File); err != nil {
			return nil, state, nil, err
		}
	}

	err = nil
	if !match {
		err = ErrSnapshotNotFound
	}

	// close decoder, disable reading
	if w.readClose != nil {
		w.readClose()
		w.readClose = nil
	}
	w.start = walpb.Snapshot{}

	w.metadata = metadata

	if w.tail() != nil {
		// create encoder (chain crc with the decoder), enable appending
		w.encoder, err = newFileEncoder(w.tail().File, w.decoder.LastCRC())
		if err != nil {
			return nil, state, nil, err
		}
	}
	w.decoder = nil

	return metadata, state, ents, err
}

// ValidSnapshotEntries returns all the valid snapshot entries in the wal logs in the given directory.
// Snapshot entries are valid if their index is less than or equal to the most recent committed hardstate.
func ValidSnapshotEntries(lg *zap.Logger, walDir string) ([]walpb.Snapshot, error) {
	var snaps []walpb.Snapshot
	var state raftpb.HardState
	var err error

	rec := &walpb.Record{}
	names, err := readWALNames(lg, walDir)
	if err != nil {
		return nil, err
	}

	// open wal files in read mode, so that there is no conflict
	// when the same WAL is opened elsewhere in write mode
	rs, _, closer, err := openWALFiles(lg, walDir, names, 0, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closer != nil {
			closer()
		}
	}()

	// create a new decoder from the readers on the WAL files
	decoder := NewDecoder(rs...)

	for err = decoder.Decode(rec); err == nil; err = decoder.Decode(rec) {
		switch rec.Type {
		case SnapshotType:
			var loadedSnap walpb.Snapshot
			pbutil.MustUnmarshal(&loadedSnap, rec.Data)
			snaps = append(snaps, loadedSnap)
		case StateType:
			state = MustUnmarshalState(rec.Data)
		case CrcType:
			crc := decoder.LastCRC()
			// current crc of decoder must match the crc of the record.
			// do no need to match 0 crc, since the decoder is a new one at this case.
			if crc != 0 && rec.Validate(crc) != nil {
				return nil, ErrCRCMismatch
			}
			decoder.UpdateCRC(rec.Crc)
		}
	}
	// We do not have to read out all the WAL entries
	// as the decoder is opened in read mode.
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}

	// filter out any snaps that are newer than the committed hardstate
	n := 0
	for _, s := range snaps {
		if s.Index <= state.Commit {
			snaps[n] = s
			n++
		}
	}
	snaps = snaps[:n:n]
	return snaps, nil
}

// Verify reads through the given WAL and verifies that it is not corrupted.
// It creates a new decoder to read through the records of the given WAL.
// It does not conflict with any open WAL, but it is recommended not to
// call this function after opening the WAL for writing.
// If it cannot read out the expected snap, it will return ErrSnapshotNotFound.
// If the loaded snap doesn't match with the expected one, it will
// return error ErrSnapshotMismatch.
func Verify(lg *zap.Logger, walDir string, snap walpb.Snapshot) (*raftpb.HardState, error) {
	var metadata []byte
	var err error
	var match bool
	var state raftpb.HardState

	rec := &walpb.Record{}

	if lg == nil {
		lg = zap.NewNop()
	}
	names, nameIndex, err := selectWALFiles(lg, walDir, snap)
	if err != nil {
		return nil, err
	}

	// open wal files in read mode, so that there is no conflict
	// when the same WAL is opened elsewhere in write mode
	rs, _, closer, err := openWALFiles(lg, walDir, names, nameIndex, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closer != nil {
			closer()
		}
	}()

	// create a new decoder from the readers on the WAL files
	decoder := NewDecoder(rs...)

	for err = decoder.Decode(rec); err == nil; err = decoder.Decode(rec) {
		switch rec.Type {
		case MetadataType:
			if metadata != nil && !bytes.Equal(metadata, rec.Data) {
				return nil, ErrMetadataConflict
			}
			metadata = rec.Data
		case CrcType:
			crc := decoder.LastCRC()
			// Current crc of decoder must match the crc of the record.
			// We need not match 0 crc, since the decoder is a new one at this point.
			if crc != 0 && rec.Validate(crc) != nil {
				return nil, ErrCRCMismatch
			}
			decoder.UpdateCRC(rec.Crc)
		case SnapshotType:
			var loadedSnap walpb.Snapshot
			pbutil.MustUnmarshal(&loadedSnap, rec.Data)
			if loadedSnap.Index == snap.Index {
				if loadedSnap.Term != snap.Term {
					return nil, ErrSnapshotMismatch
				}
				match = true
			}
		// We ignore all entry and state type records as these
		// are not necessary for validating the WAL contents
		case EntryType:
		case StateType:
			pbutil.MustUnmarshal(&state, rec.Data)
		default:
			return nil, fmt.Errorf("unexpected block type %d", rec.Type)
		}
	}

	// We do not have to read out all the WAL entries
	// as the decoder is opened in read mode.
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}

	if !match {
		return nil, ErrSnapshotNotFound
	}

	return &state, nil
}

// cut closes current file written and creates a new one ready to append.
// cut first creates a temp wal file and writes necessary headers into it.
// Then cut atomically rename temp wal file to a wal file.
func (w *WAL) cut() error {
	// close old wal file; truncate to avoid wasting space if an early cut
	off, serr := w.tail().Seek(0, io.SeekCurrent)
	if serr != nil {
		return serr
	}

	if err := w.tail().Truncate(off); err != nil {
		return err
	}

	if err := w.sync(); err != nil {
		return err
	}

	fpath := filepath.Join(w.dir, walName(w.seq()+1, w.enti+1))

	// create a temp wal file with name sequence + 1, or truncate the existing one
	newTail, err := w.fp.Open()
	if err != nil {
		return err
	}

	// update writer and save the previous crc
	w.locks = append(w.locks, newTail)
	prevCrc := w.encoder.crc.Sum32()
	w.encoder, err = newFileEncoder(w.tail().File, prevCrc)
	if err != nil {
		return err
	}

	if err = w.saveCrc(prevCrc); err != nil {
		return err
	}

	if err = w.encoder.encode(&walpb.Record{Type: MetadataType, Data: w.metadata}); err != nil {
		return err
	}

	if err = w.saveState(&w.state); err != nil {
		return err
	}

	// atomically move temp wal file to wal file
	if err = w.sync(); err != nil {
		return err
	}

	off, err = w.tail().Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}

	if err = os.Rename(newTail.Name(), fpath); err != nil {
		return err
	}
	start := time.Now()
	if err = fileutil.Fsync(w.dirFile); err != nil {
		return err
	}
	walFsyncSec.Observe(time.Since(start).Seconds())

	// reopen newTail with its new path so calls to Name() match the wal filename format
	newTail.Close()

	if newTail, err = fileutil.LockFile(fpath, os.O_WRONLY, fileutil.PrivateFileMode); err != nil {
		return err
	}
	if _, err = newTail.Seek(off, io.SeekStart); err != nil {
		return err
	}

	w.locks[len(w.locks)-1] = newTail

	prevCrc = w.encoder.crc.Sum32()
	w.encoder, err = newFileEncoder(w.tail().File, prevCrc)
	if err != nil {
		return err
	}

	w.lg.Info("created a new WAL segment", zap.String("path", fpath))
	return nil
}

func (w *WAL) sync() error {
	if w.encoder != nil {
		if err := w.encoder.flush(); err != nil {
			return err
		}
	}

	if w.unsafeNoSync {
		return nil
	}

	start := time.Now()
	err := fileutil.Fdatasync(w.tail().File)

	took := time.Since(start)
	if took > warnSyncDuration {
		w.lg.Warn(
			"slow fdatasync",
			zap.Duration("took", took),
			zap.Duration("expected-duration", warnSyncDuration),
		)
	}
	walFsyncSec.Observe(took.Seconds())

	return err
}

func (w *WAL) Sync() error {
	return w.sync()
}

// ReleaseLockTo releases the locks, which has smaller index than the given index
// except the largest one among them.
// For example, if WAL is holding lock 1,2,3,4,5,6, ReleaseLockTo(4) will release
// lock 1,2 but keep 3. ReleaseLockTo(5) will release 1,2,3 but keep 4.
func (w *WAL) ReleaseLockTo(index uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.locks) == 0 {
		return nil
	}

	var smaller int
	found := false
	for i, l := range w.locks {
		_, lockIndex, err := parseWALName(filepath.Base(l.Name()))
		if err != nil {
			return err
		}
		if lockIndex >= index {
			smaller = i - 1
			found = true
			break
		}
	}

	// if no lock index is greater than the release index, we can
	// release lock up to the last one(excluding).
	if !found {
		smaller = len(w.locks) - 1
	}

	if smaller <= 0 {
		return nil
	}

	for i := 0; i < smaller; i++ {
		if w.locks[i] == nil {
			continue
		}
		w.locks[i].Close()
	}
	w.locks = w.locks[smaller:]

	return nil
}

// Close closes the current WAL file and directory.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.fp != nil {
		w.fp.Close()
		w.fp = nil
	}

	if w.tail() != nil {
		if err := w.sync(); err != nil {
			return err
		}
	}
	for _, l := range w.locks {
		if l == nil {
			continue
		}
		if err := l.Close(); err != nil {
			w.lg.Error("failed to close WAL", zap.Error(err))
		}
	}

	return w.dirFile.Close()
}

func (w *WAL) saveEntry(e *raftpb.Entry) error {
	// TODO: add MustMarshalTo to reduce one allocation.
	b := pbutil.MustMarshal(e)
	rec := &walpb.Record{Type: EntryType, Data: b}
	if err := w.encoder.encode(rec); err != nil {
		return err
	}
	w.enti = e.Index
	return nil
}

func (w *WAL) saveState(s *raftpb.HardState) error {
	if raft.IsEmptyHardState(*s) {
		return nil
	}
	w.state = *s
	b := pbutil.MustMarshal(s)
	rec := &walpb.Record{Type: StateType, Data: b}
	return w.encoder.encode(rec)
}

func (w *WAL) Save(st raftpb.HardState, ents []raftpb.Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// short cut, do not call sync
	if raft.IsEmptyHardState(st) && len(ents) == 0 {
		return nil
	}

	mustSync := raft.MustSync(st, w.state, len(ents))

	// TODO(xiangli): no more reference operator
	for i := range ents {
		if err := w.saveEntry(&ents[i]); err != nil {
			return err
		}
	}
	if err := w.saveState(&st); err != nil {
		return err
	}

	curOff, err := w.tail().Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if curOff < SegmentSizeBytes {
		if mustSync {
			// gofail: var walBeforeSync struct{}
			err = w.sync()
			// gofail: var walAfterSync struct{}
			return err
		}
		return nil
	}

	return w.cut()
}

func (w *WAL) SaveSnapshot(e walpb.Snapshot) error {
	if err := walpb.ValidateSnapshotForWrite(&e); err != nil {
		return err
	}

	b := pbutil.MustMarshal(&e)

	w.mu.Lock()
	defer w.mu.Unlock()

	rec := &walpb.Record{Type: SnapshotType, Data: b}
	if err := w.encoder.encode(rec); err != nil {
		return err
	}
	// update enti only when snapshot is ahead of last index
	if w.enti < e.Index {
		w.enti = e.Index
	}
	return w.sync()
}

func (w *WAL) saveCrc(prevCrc uint32) error {
	return w.encoder.encode(&walpb.Record{Type: CrcType, Crc: prevCrc})
}

func (w *WAL) tail() *fileutil.LockedFile {
	if len(w.locks) > 0 {
		return w.locks[len(w.locks)-1]
	}
	return nil
}

func (w *WAL) seq() uint64 {
	t := w.tail()
	if t == nil {
		return 0
	}
	seq, _, err := parseWALName(filepath.Base(t.Name()))
	if err != nil {
		w.lg.Fatal("failed to parse WAL name", zap.String("name", t.Name()), zap.Error(err))
	}
	return seq
}

func closeAll(lg *zap.Logger, rcs ...io.ReadCloser) error {
	stringArr := make([]string, 0)
	for _, f := range rcs {
		if err := f.Close(); err != nil {
			lg.Warn("failed to close: ", zap.Error(err))
			stringArr = append(stringArr, err.Error())
		}
	}
	if len(stringArr) == 0 {
		return nil
	}
	return errors.New(strings.Join(stringArr, ", "))
}
