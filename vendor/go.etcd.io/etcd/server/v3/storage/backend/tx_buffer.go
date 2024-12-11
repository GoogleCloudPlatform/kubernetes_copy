// Copyright 2017 The etcd Authors
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
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"

	"go.etcd.io/etcd/client/pkg/v3/verify"
)

const bucketBufferInitialSize = 512

// txBuffer handles functionality shared between txWriteBuffer and txReadBuffer.
type txBuffer struct {
	buckets map[BucketID]*bucketBuffer
}

func (txb *txBuffer) reset() {
	for k, v := range txb.buckets {
		if v.used == 0 {
			// demote
			delete(txb.buckets, k)
		}
		v.used = 0
	}
}

// txWriteBuffer buffers writes of pending updates that have not yet committed.
type txWriteBuffer struct {
	txBuffer
	// Map from bucket ID into information whether this bucket is edited
	// sequentially (i.e. keys are growing monotonically).
	bucket2seq map[BucketID]bool
}

func (txw *txWriteBuffer) put(bucket Bucket, k, v []byte) {
	txw.bucket2seq[bucket.ID()] = false
	txw.putInternal(bucket, k, v)
}

func (txw *txWriteBuffer) putSeq(bucket Bucket, k, v []byte) {
	// putSeq is only be called for the data in the Key bucket. The keys
	// in the Key bucket should be monotonically increasing revisions.
	verify.Verify(func() {
		b, ok := txw.buckets[bucket.ID()]
		if !ok || b.used == 0 {
			return
		}

		existingMaxKey := b.buf[b.used-1].key
		if bytes.Compare(k, existingMaxKey) <= 0 {
			panic(fmt.Sprintf("Broke the rule of monotonically increasing, existingMaxKey: %s, currentKey: %s",
				hex.EncodeToString(existingMaxKey), hex.EncodeToString(k)))
		}
	})
	txw.putInternal(bucket, k, v)
}

func (txw *txWriteBuffer) putInternal(bucket Bucket, k, v []byte) {
	b, ok := txw.buckets[bucket.ID()]
	if !ok {
		b = newBucketBuffer()
		txw.buckets[bucket.ID()] = b
	}
	b.add(k, v)
}

func (txw *txWriteBuffer) reset() {
	txw.txBuffer.reset()
	for k := range txw.bucket2seq {
		v, ok := txw.buckets[k]
		if !ok {
			delete(txw.bucket2seq, k)
		} else if v.used == 0 {
			txw.bucket2seq[k] = true
		}
	}
}

func (txw *txWriteBuffer) writeback(txr *txReadBuffer) {
	for k, wb := range txw.buckets {
		rb, ok := txr.buckets[k]
		if !ok {
			delete(txw.buckets, k)
			if seq, ok := txw.bucket2seq[k]; ok && !seq {
				wb.dedupe()
			}
			txr.buckets[k] = wb
			continue
		}
		if seq, ok := txw.bucket2seq[k]; ok && !seq && wb.used > 1 {
			// assume no duplicate keys
			sort.Sort(wb)
		}
		rb.merge(wb)
	}
	txw.reset()
	// increase the buffer version
	txr.bufVersion++
}

// txReadBuffer accesses buffered updates.
type txReadBuffer struct {
	txBuffer
	// bufVersion is used to check if the buffer is modified recently
	bufVersion uint64
}

func (txr *txReadBuffer) Range(bucket Bucket, key, endKey []byte, limit int64) ([][]byte, [][]byte) {
	if b := txr.buckets[bucket.ID()]; b != nil {
		return b.Range(key, endKey, limit)
	}
	return nil, nil
}

func (txr *txReadBuffer) ForEach(bucket Bucket, visitor func(k, v []byte) error) error {
	if b := txr.buckets[bucket.ID()]; b != nil {
		return b.ForEach(visitor)
	}
	return nil
}

// unsafeCopy returns a copy of txReadBuffer, caller should acquire backend.readTx.RLock()
func (txr *txReadBuffer) unsafeCopy() txReadBuffer {
	txrCopy := txReadBuffer{
		txBuffer: txBuffer{
			buckets: make(map[BucketID]*bucketBuffer, len(txr.txBuffer.buckets)),
		},
		bufVersion: 0,
	}
	for bucketName, bucket := range txr.txBuffer.buckets {
		txrCopy.txBuffer.buckets[bucketName] = bucket.CopyUsed()
	}
	return txrCopy
}

type kv struct {
	key []byte
	val []byte
}

// bucketBuffer buffers key-value pairs that are pending commit.
type bucketBuffer struct {
	buf []kv
	// used tracks number of elements in use so buf can be reused without reallocation.
	used int
}

func newBucketBuffer() *bucketBuffer {
	return &bucketBuffer{buf: make([]kv, bucketBufferInitialSize), used: 0}
}

func (bb *bucketBuffer) Range(key, endKey []byte, limit int64) (keys [][]byte, vals [][]byte) {
	f := func(i int) bool { return bytes.Compare(bb.buf[i].key, key) >= 0 }
	idx := sort.Search(bb.used, f)
	if idx < 0 || idx >= bb.used {
		return nil, nil
	}
	if len(endKey) == 0 {
		if bytes.Equal(key, bb.buf[idx].key) {
			keys = append(keys, bb.buf[idx].key)
			vals = append(vals, bb.buf[idx].val)
		}
		return keys, vals
	}
	if bytes.Compare(endKey, bb.buf[idx].key) <= 0 {
		return nil, nil
	}
	for i := idx; i < bb.used && int64(len(keys)) < limit; i++ {
		if bytes.Compare(endKey, bb.buf[i].key) <= 0 {
			break
		}
		keys = append(keys, bb.buf[i].key)
		vals = append(vals, bb.buf[i].val)
	}
	return keys, vals
}

func (bb *bucketBuffer) ForEach(visitor func(k, v []byte) error) error {
	for i := 0; i < bb.used; i++ {
		if err := visitor(bb.buf[i].key, bb.buf[i].val); err != nil {
			return err
		}
	}
	return nil
}

func (bb *bucketBuffer) add(k, v []byte) {
	bb.buf[bb.used].key, bb.buf[bb.used].val = k, v
	bb.used++
	if bb.used == len(bb.buf) {
		buf := make([]kv, (3*len(bb.buf))/2)
		copy(buf, bb.buf)
		bb.buf = buf
	}
}

// merge merges data from bbsrc into bb.
func (bb *bucketBuffer) merge(bbsrc *bucketBuffer) {
	for i := 0; i < bbsrc.used; i++ {
		bb.add(bbsrc.buf[i].key, bbsrc.buf[i].val)
	}
	if bb.used == bbsrc.used {
		return
	}
	if bytes.Compare(bb.buf[(bb.used-bbsrc.used)-1].key, bbsrc.buf[0].key) < 0 {
		return
	}
	bb.dedupe()
}

// dedupe removes duplicates, using only newest update
func (bb *bucketBuffer) dedupe() {
	if bb.used <= 1 {
		return
	}
	sort.Stable(bb)
	widx := 0
	for ridx := 1; ridx < bb.used; ridx++ {
		if !bytes.Equal(bb.buf[ridx].key, bb.buf[widx].key) {
			widx++
		}
		bb.buf[widx] = bb.buf[ridx]
	}
	bb.used = widx + 1
}

func (bb *bucketBuffer) Len() int { return bb.used }
func (bb *bucketBuffer) Less(i, j int) bool {
	return bytes.Compare(bb.buf[i].key, bb.buf[j].key) < 0
}
func (bb *bucketBuffer) Swap(i, j int) { bb.buf[i], bb.buf[j] = bb.buf[j], bb.buf[i] }

func (bb *bucketBuffer) CopyUsed() *bucketBuffer {
	verify.Assert(bb.used <= len(bb.buf),
		"used (%d) should never be bigger than the length of buf (%d)", bb.used, len(bb.buf))
	bbCopy := bucketBuffer{
		buf:  make([]kv, bb.used),
		used: bb.used,
	}
	copy(bbCopy.buf, bb.buf[:bb.used])
	return &bbCopy
}
