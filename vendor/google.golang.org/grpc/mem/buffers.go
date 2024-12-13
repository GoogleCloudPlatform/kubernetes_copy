/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package mem provides utilities that facilitate memory reuse in byte slices
// that are used as buffers.
//
// # Experimental
//
// Notice: All APIs in this package are EXPERIMENTAL and may be changed or
// removed in a later release.
package mem

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// A Buffer represents a reference counted piece of data (in bytes) that can be
// acquired by a call to NewBuffer() or Copy(). A reference to a Buffer may be
// released by calling Free(), which invokes the free function given at creation
// only after all references are released.
//
// Note that a Buffer is not safe for concurrent access and instead each
// goroutine should use its own reference to the data, which can be acquired via
// a call to Ref().
//
// Attempts to access the underlying data after releasing the reference to the
// Buffer will panic.
type Buffer interface {
	// ReadOnlyData returns the underlying byte slice. Note that it is undefined
	// behavior to modify the contents of this slice in any way.
	ReadOnlyData() []byte
	// Ref increases the reference counter for this Buffer.
	Ref()
	// Free decrements this Buffer's reference counter and frees the underlying
	// byte slice if the counter reaches 0 as a result of this call.
	Free()
	// Len returns the Buffer's size.
	Len() int

	split(n int) (left, right Buffer)
	read(buf []byte) (int, Buffer)
}

var (
	bufferPoolingThreshold = 1 << 10

	bufferObjectPool = sync.Pool{New: func() any { return new(buffer) }}
	refObjectPool    = sync.Pool{New: func() any { return new(atomic.Int32) }}
)

// IsBelowBufferPoolingThreshold returns true if the given size is less than or
// equal to the threshold for buffer pooling. This is used to determine whether
// to pool buffers or allocate them directly.
func IsBelowBufferPoolingThreshold(size int) bool {
	return size <= bufferPoolingThreshold
}

type buffer struct {
	origData *[]byte
	data     []byte
	refs     *atomic.Int32
	pool     BufferPool
}

func newBuffer() *buffer {
	return bufferObjectPool.Get().(*buffer)
}

// NewBuffer creates a new Buffer from the given data, initializing the reference
// counter to 1. The data will then be returned to the given pool when all
// references to the returned Buffer are released. As a special case to avoid
// additional allocations, if the given buffer pool is nil, the returned buffer
// will be a "no-op" Buffer where invoking Buffer.Free() does nothing and the
// underlying data is never freed.
//
// Note that the backing array of the given data is not copied.
func NewBuffer(data *[]byte, pool BufferPool) Buffer {
	// Use the buffer's capacity instead of the length, otherwise buffers may
	// not be reused under certain conditions. For example, if a large buffer
	// is acquired from the pool, but fewer bytes than the buffering threshold
	// are written to it, the buffer will not be returned to the pool.
	if pool == nil || IsBelowBufferPoolingThreshold(cap(*data)) {
		return (SliceBuffer)(*data)
	}
	b := newBuffer()
	b.origData = data
	b.data = *data
	b.pool = pool
	b.refs = refObjectPool.Get().(*atomic.Int32)
	b.refs.Add(1)
	return b
}

// Copy creates a new Buffer from the given data, initializing the reference
// counter to 1.
//
// It acquires a []byte from the given pool and copies over the backing array
// of the given data. The []byte acquired from the pool is returned to the
// pool when all references to the returned Buffer are released.
func Copy(data []byte, pool BufferPool) Buffer {
	if IsBelowBufferPoolingThreshold(len(data)) {
		buf := make(SliceBuffer, len(data))
		copy(buf, data)
		return buf
	}

	buf := pool.Get(len(data))
	copy(*buf, data)
	return NewBuffer(buf, pool)
}

func (b *buffer) ReadOnlyData() []byte {
	if b.refs == nil {
		panic("Cannot read freed buffer")
	}
	return b.data
}

func (b *buffer) Ref() {
	if b.refs == nil {
		panic("Cannot ref freed buffer")
	}
	b.refs.Add(1)
}

func (b *buffer) Free() {
	if b.refs == nil {
		panic("Cannot free freed buffer")
	}

	refs := b.refs.Add(-1)
	switch {
	case refs > 0:
		return
	case refs == 0:
		if b.pool != nil {
			b.pool.Put(b.origData)
		}

		refObjectPool.Put(b.refs)
		b.origData = nil
		b.data = nil
		b.refs = nil
		b.pool = nil
		bufferObjectPool.Put(b)
	default:
		panic("Cannot free freed buffer")
	}
}

func (b *buffer) Len() int {
	return len(b.ReadOnlyData())
}

func (b *buffer) split(n int) (Buffer, Buffer) {
	if b.refs == nil {
		panic("Cannot split freed buffer")
	}

	b.refs.Add(1)
	split := newBuffer()
	split.origData = b.origData
	split.data = b.data[n:]
	split.refs = b.refs
	split.pool = b.pool

	b.data = b.data[:n]

	return b, split
}

func (b *buffer) read(buf []byte) (int, Buffer) {
	if b.refs == nil {
		panic("Cannot read freed buffer")
	}

	n := copy(buf, b.data)
	if n == len(b.data) {
		b.Free()
		return n, nil
	}

	b.data = b.data[n:]
	return n, b
}

func (b *buffer) String() string {
	return fmt.Sprintf("mem.Buffer(%p, data: %p, length: %d)", b, b.ReadOnlyData(), len(b.ReadOnlyData()))
}

// ReadUnsafe reads bytes from the given Buffer into the provided slice.
// It does not perform safety checks.
func ReadUnsafe(dst []byte, buf Buffer) (int, Buffer) {
	return buf.read(dst)
}

// SplitUnsafe modifies the receiver to point to the first n bytes while it
// returns a new reference to the remaining bytes. The returned Buffer
// functions just like a normal reference acquired using Ref().
func SplitUnsafe(buf Buffer, n int) (left, right Buffer) {
	return buf.split(n)
}

type emptyBuffer struct{}

func (e emptyBuffer) ReadOnlyData() []byte {
	return nil
}

func (e emptyBuffer) Ref()  {}
func (e emptyBuffer) Free() {}

func (e emptyBuffer) Len() int {
	return 0
}

func (e emptyBuffer) split(int) (left, right Buffer) {
	return e, e
}

func (e emptyBuffer) read([]byte) (int, Buffer) {
	return 0, e
}

// SliceBuffer is a Buffer implementation that wraps a byte slice. It provides
// methods for reading, splitting, and managing the byte slice.
type SliceBuffer []byte

// ReadOnlyData returns the byte slice.
func (s SliceBuffer) ReadOnlyData() []byte { return s }

// Ref is a noop implementation of Ref.
func (s SliceBuffer) Ref() {}

// Free is a noop implementation of Free.
func (s SliceBuffer) Free() {}

// Len is a noop implementation of Len.
func (s SliceBuffer) Len() int { return len(s) }

func (s SliceBuffer) split(n int) (left, right Buffer) {
	return s[:n], s[n:]
}

func (s SliceBuffer) read(buf []byte) (int, Buffer) {
	n := copy(buf, s)
	if n == len(s) {
		return n, nil
	}
	return n, s[n:]
}
