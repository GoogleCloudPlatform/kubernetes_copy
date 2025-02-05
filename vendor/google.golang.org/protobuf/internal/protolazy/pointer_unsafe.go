// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package protolazy

import (
	"sync/atomic"
	"unsafe"
)

func atomicLoadIndex(p **[]IndexEntry) *[]IndexEntry {
	return (*[]IndexEntry)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(p))))
}
func atomicStoreIndex(p **[]IndexEntry, v *[]IndexEntry) {
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(p)), unsafe.Pointer(v))
}
