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

package mvcc

import (
	"encoding/binary"
	"fmt"
)

const (
	// revBytesLen is the byte length of a normal revision.
	// First 8 bytes is the revision.main in big-endian format. The 9th byte
	// is a '_'. The last 8 bytes is the revision.sub in big-endian format.
	revBytesLen = 8 + 1 + 8
	// markedRevBytesLen is the byte length of marked revision.
	// The first `revBytesLen` bytes represents a normal revision. The last
	// one byte is the mark.
	markedRevBytesLen      = revBytesLen + 1
	markBytePosition       = markedRevBytesLen - 1
	markTombstone     byte = 't'
)

type Revision struct {
	// Main is the main revision of a set of changes that happen atomically.
	Main int64
	// Sub is the sub revision of a change in a set of changes that happen
	// atomically. Each change has different increasing sub revision in that
	// set.
	Sub int64
}

func (a Revision) GreaterThan(b Revision) bool {
	if a.Main > b.Main {
		return true
	}
	if a.Main < b.Main {
		return false
	}
	return a.Sub > b.Sub
}

func RevToBytes(rev Revision, bytes []byte) []byte {
	return BucketKeyToBytes(newBucketKey(rev.Main, rev.Sub, false), bytes)
}

func BytesToRev(bytes []byte) Revision {
	return BytesToBucketKey(bytes).Revision
}

// BucketKey indicates modification of the key-value space.
// The set of changes that share same main revision changes the key-value space atomically.
type BucketKey struct {
	Revision
	tombstone bool
}

func newBucketKey(main, sub int64, isTombstone bool) BucketKey {
	return BucketKey{
		Revision: Revision{
			Main: main,
			Sub:  sub,
		},
		tombstone: isTombstone,
	}
}

func NewRevBytes() []byte {
	return make([]byte, revBytesLen, markedRevBytesLen)
}

func BucketKeyToBytes(rev BucketKey, bytes []byte) []byte {
	binary.BigEndian.PutUint64(bytes, uint64(rev.Main))
	bytes[8] = '_'
	binary.BigEndian.PutUint64(bytes[9:], uint64(rev.Sub))
	if rev.tombstone {
		switch len(bytes) {
		case revBytesLen:
			bytes = append(bytes, markTombstone)
		case markedRevBytesLen:
			bytes[markBytePosition] = markTombstone
		}
	}
	return bytes
}

func BytesToBucketKey(bytes []byte) BucketKey {
	if (len(bytes) != revBytesLen) && (len(bytes) != markedRevBytesLen) {
		panic(fmt.Sprintf("invalid revision length: %d", len(bytes)))
	}
	if bytes[8] != '_' {
		panic(fmt.Sprintf("invalid separator in bucket key: %q", bytes[8]))
	}
	main := int64(binary.BigEndian.Uint64(bytes[0:8]))
	sub := int64(binary.BigEndian.Uint64(bytes[9:]))
	if main < 0 || sub < 0 {
		panic(fmt.Sprintf("negative revision: main=%d sub=%d", main, sub))
	}
	return BucketKey{
		Revision: Revision{
			Main: main,
			Sub:  sub,
		},
		tombstone: isTombstone(bytes),
	}
}

// isTombstone checks whether the revision bytes is a tombstone.
func isTombstone(b []byte) bool {
	return len(b) == markedRevBytesLen && b[markBytePosition] == markTombstone
}
