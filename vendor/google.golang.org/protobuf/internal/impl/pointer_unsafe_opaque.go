// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"sync/atomic"
	"unsafe"
)

func (p pointer) AtomicGetPointer() pointer {
	return pointer{p: atomic.LoadPointer((*unsafe.Pointer)(p.p))}
}

func (p pointer) AtomicSetPointer(v pointer) {
	atomic.StorePointer((*unsafe.Pointer)(p.p), v.p)
}

func (p pointer) AtomicSetNilPointer() {
	atomic.StorePointer((*unsafe.Pointer)(p.p), unsafe.Pointer(nil))
}

func (p pointer) AtomicSetPointerIfNil(v pointer) pointer {
	if atomic.CompareAndSwapPointer((*unsafe.Pointer)(p.p), unsafe.Pointer(nil), v.p) {
		return v
	}
	return pointer{p: atomic.LoadPointer((*unsafe.Pointer)(p.p))}
}

type atomicV1MessageInfo struct{ p Pointer }

func (mi *atomicV1MessageInfo) Get() Pointer {
	return Pointer(atomic.LoadPointer((*unsafe.Pointer)(&mi.p)))
}

func (mi *atomicV1MessageInfo) SetIfNil(p Pointer) Pointer {
	if atomic.CompareAndSwapPointer((*unsafe.Pointer)(&mi.p), nil, unsafe.Pointer(p)) {
		return p
	}
	return mi.Get()
}
