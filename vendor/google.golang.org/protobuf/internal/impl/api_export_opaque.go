// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"strconv"
	"sync/atomic"
	"unsafe"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func (Export) UnmarshalField(msg any, fieldNum int32) {
	UnmarshalField(msg.(protoreflect.ProtoMessage).ProtoReflect(), protoreflect.FieldNumber(fieldNum))
}

// Present checks the presence set for a certain field number (zero
// based, ordered by appearance in original proto file). part is
// a pointer to the correct element in the bitmask array, num is the
// field number unaltered.  Example (field number 70 -> part =
// &m.XXX_presence[1], num = 70)
func (Export) Present(part *uint32, num uint32) bool {
	// This hook will read an unprotected shadow presence set if
	// we're unning under the race detector
	raceDetectHookPresent(part, num)
	return atomic.LoadUint32(part)&(1<<(num%32)) > 0
}

// SetPresent adds a field to the presence set. part is a pointer to
// the relevant element in the array and num is the field number
// unaltered.  size is the number of fields in the protocol
// buffer.
func (Export) SetPresent(part *uint32, num uint32, size uint32) {
	// This hook will mutate an unprotected shadow presence set if
	// we're running under the race detector
	raceDetectHookSetPresent(part, num, presenceSize(size))
	for {
		old := atomic.LoadUint32(part)
		if atomic.CompareAndSwapUint32(part, old, old|(1<<(num%32))) {
			return
		}
	}
}

// SetPresentNonAtomic is like SetPresent, but operates non-atomically.
// It is meant for use by builder methods, where the message is known not
// to be accessible yet by other goroutines.
func (Export) SetPresentNonAtomic(part *uint32, num uint32, size uint32) {
	// This hook will mutate an unprotected shadow presence set if
	// we're running under the race detector
	raceDetectHookSetPresent(part, num, presenceSize(size))
	*part |= 1 << (num % 32)
}

// ClearPresence removes a field from the presence set. part is a
// pointer to the relevant element in the presence array and num is
// the field number unaltered.
func (Export) ClearPresent(part *uint32, num uint32) {
	// This hook will mutate an unprotected shadow presence set if
	// we're running under the race detector
	raceDetectHookClearPresent(part, num)
	for {
		old := atomic.LoadUint32(part)
		if atomic.CompareAndSwapUint32(part, old, old&^(1<<(num%32))) {
			return
		}
	}
}

// interfaceToPointer takes a pointer to an empty interface whose value is a
// pointer type, and converts it into a "pointer" that points to the same
// target
func interfaceToPointer(i *any) pointer {
	return pointer{p: (*[2]unsafe.Pointer)(unsafe.Pointer(i))[1]}
}

func (p pointer) atomicGetPointer() pointer {
	return pointer{p: atomic.LoadPointer((*unsafe.Pointer)(p.p))}
}

func (p pointer) atomicSetPointer(q pointer) {
	atomic.StorePointer((*unsafe.Pointer)(p.p), q.p)
}

// AtomicCheckPointerIsNil takes an interface (which is a pointer to a
// pointer) and returns true if the pointed-to pointer is nil (using an
// atomic load).  This function is inlineable and, on x86, just becomes a
// simple load and compare.
func (Export) AtomicCheckPointerIsNil(ptr any) bool {
	return interfaceToPointer(&ptr).atomicGetPointer().IsNil()
}

// AtomicSetPointer takes two interfaces (first is a pointer to a pointer,
// second is a pointer) and atomically sets the second pointer into location
// referenced by first pointer.  Unfortunately, atomicSetPointer() does not inline
// (even on x86), so this does not become a simple store on x86.
func (Export) AtomicSetPointer(dstPtr, valPtr any) {
	interfaceToPointer(&dstPtr).atomicSetPointer(interfaceToPointer(&valPtr))
}

// AtomicLoadPointer loads the pointer at the location pointed at by src,
// and stores that pointer value into the location pointed at by dst.
func (Export) AtomicLoadPointer(ptr Pointer, dst Pointer) {
	*(*unsafe.Pointer)(unsafe.Pointer(dst)) = atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(ptr)))
}

// AtomicInitializePointer makes ptr and dst point to the same value.
//
// If *ptr is a nil pointer, it sets *ptr = *dst.
//
// If *ptr is a non-nil pointer, it sets *dst = *ptr.
func (Export) AtomicInitializePointer(ptr Pointer, dst Pointer) {
	if !atomic.CompareAndSwapPointer((*unsafe.Pointer)(ptr), unsafe.Pointer(nil), *(*unsafe.Pointer)(dst)) {
		*(*unsafe.Pointer)(unsafe.Pointer(dst)) = atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(ptr)))
	}
}

// MessageFieldStringOf returns the field formatted as a string,
// either as the field name if resolvable otherwise as a decimal string.
func (Export) MessageFieldStringOf(md protoreflect.MessageDescriptor, n protoreflect.FieldNumber) string {
	fd := md.Fields().ByNumber(n)
	if fd != nil {
		return string(fd.Name())
	}
	return strconv.Itoa(int(n))
}
