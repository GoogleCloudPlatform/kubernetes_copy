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

package mem

import (
	"sort"
	"sync"

	"google.golang.org/grpc/internal"
)

// BufferPool is a pool of buffers that can be shared and reused, resulting in
// decreased memory allocation.
type BufferPool interface {
	// Get returns a buffer with specified length from the pool.
	Get(length int) *[]byte

	// Put returns a buffer to the pool.
	Put(*[]byte)
}

var defaultBufferPoolSizes = []int{
	256,
	4 << 10,  // 4KB (go page size)
	16 << 10, // 16KB (max HTTP/2 frame size used by gRPC)
	32 << 10, // 32KB (default buffer size for io.Copy)
	1 << 20,  // 1MB
}

var defaultBufferPool BufferPool

func init() {
	defaultBufferPool = NewTieredBufferPool(defaultBufferPoolSizes...)

	internal.SetDefaultBufferPoolForTesting = func(pool BufferPool) {
		defaultBufferPool = pool
	}

	internal.SetBufferPoolingThresholdForTesting = func(threshold int) {
		bufferPoolingThreshold = threshold
	}
}

// DefaultBufferPool returns the current default buffer pool. It is a BufferPool
// created with NewBufferPool that uses a set of default sizes optimized for
// expected workflows.
func DefaultBufferPool() BufferPool {
	return defaultBufferPool
}

// NewTieredBufferPool returns a BufferPool implementation that uses multiple
// underlying pools of the given pool sizes.
func NewTieredBufferPool(poolSizes ...int) BufferPool {
	sort.Ints(poolSizes)
	pools := make([]*sizedBufferPool, len(poolSizes))
	for i, s := range poolSizes {
		pools[i] = newSizedBufferPool(s)
	}
	return &tieredBufferPool{
		sizedPools: pools,
	}
}

// tieredBufferPool implements the BufferPool interface with multiple tiers of
// buffer pools for different sizes of buffers.
type tieredBufferPool struct {
	sizedPools   []*sizedBufferPool
	fallbackPool simpleBufferPool
}

func (p *tieredBufferPool) Get(size int) *[]byte {
	return p.getPool(size).Get(size)
}

func (p *tieredBufferPool) Put(buf *[]byte) {
	p.getPool(cap(*buf)).Put(buf)
}

func (p *tieredBufferPool) getPool(size int) BufferPool {
	poolIdx := sort.Search(len(p.sizedPools), func(i int) bool {
		return p.sizedPools[i].defaultSize >= size
	})

	if poolIdx == len(p.sizedPools) {
		return &p.fallbackPool
	}

	return p.sizedPools[poolIdx]
}

// sizedBufferPool is a BufferPool implementation that is optimized for specific
// buffer sizes. For example, HTTP/2 frames within gRPC have a default max size
// of 16kb and a sizedBufferPool can be configured to only return buffers with a
// capacity of 16kb. Note that however it does not support returning larger
// buffers and in fact panics if such a buffer is requested. Because of this,
// this BufferPool implementation is not meant to be used on its own and rather
// is intended to be embedded in a tieredBufferPool such that Get is only
// invoked when the required size is smaller than or equal to defaultSize.
type sizedBufferPool struct {
	pool        sync.Pool
	defaultSize int
}

func (p *sizedBufferPool) Get(size int) *[]byte {
	buf := p.pool.Get().(*[]byte)
	b := *buf
	clear(b[:cap(b)])
	*buf = b[:size]
	return buf
}

func (p *sizedBufferPool) Put(buf *[]byte) {
	if cap(*buf) < p.defaultSize {
		// Ignore buffers that are too small to fit in the pool. Otherwise, when
		// Get is called it will panic as it tries to index outside the bounds
		// of the buffer.
		return
	}
	p.pool.Put(buf)
}

func newSizedBufferPool(size int) *sizedBufferPool {
	return &sizedBufferPool{
		pool: sync.Pool{
			New: func() any {
				buf := make([]byte, size)
				return &buf
			},
		},
		defaultSize: size,
	}
}

var _ BufferPool = (*simpleBufferPool)(nil)

// simpleBufferPool is an implementation of the BufferPool interface that
// attempts to pool buffers with a sync.Pool. When Get is invoked, it tries to
// acquire a buffer from the pool but if that buffer is too small, it returns it
// to the pool and creates a new one.
type simpleBufferPool struct {
	pool sync.Pool
}

func (p *simpleBufferPool) Get(size int) *[]byte {
	bs, ok := p.pool.Get().(*[]byte)
	if ok && cap(*bs) >= size {
		*bs = (*bs)[:size]
		return bs
	}

	// A buffer was pulled from the pool, but it is too small. Put it back in
	// the pool and create one large enough.
	if ok {
		p.pool.Put(bs)
	}

	b := make([]byte, size)
	return &b
}

func (p *simpleBufferPool) Put(buf *[]byte) {
	p.pool.Put(buf)
}

var _ BufferPool = NopBufferPool{}

// NopBufferPool is a buffer pool that returns new buffers without pooling.
type NopBufferPool struct{}

// Get returns a buffer with specified length from the pool.
func (NopBufferPool) Get(length int) *[]byte {
	b := make([]byte, length)
	return &b
}

// Put returns a buffer to the pool.
func (NopBufferPool) Put(*[]byte) {
}
