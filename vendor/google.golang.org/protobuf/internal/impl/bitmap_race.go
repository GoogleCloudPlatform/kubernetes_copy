// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build race

package impl

// When running under race detector, we add a presence map of bytes, that we can access
// in the hook functions so that we trigger the race detection whenever we have concurrent
// Read-Writes or Write-Writes. The race detector does not otherwise detect invalid concurrent
// access to lazy fields as all updates of bitmaps and pointers are done using atomic operations.
type RaceDetectHookData struct {
	shadowPresence *[]byte
}

// Hooks for presence bitmap operations that allocate, read and write the shadowPresence
// using non-atomic operations.
func (data *RaceDetectHookData) raceDetectHookAlloc(size presenceSize) {
	sp := make([]byte, size)
	atomicStoreShadowPresence(&data.shadowPresence, &sp)
}

func (p presence) raceDetectHookPresent(num uint32) {
	data := p.toRaceDetectData()
	if data == nil {
		return
	}
	sp := atomicLoadShadowPresence(&data.shadowPresence)
	if sp != nil {
		_ = (*sp)[num]
	}
}

func (p presence) raceDetectHookSetPresent(num uint32, size presenceSize) {
	data := p.toRaceDetectData()
	if data == nil {
		return
	}
	sp := atomicLoadShadowPresence(&data.shadowPresence)
	if sp == nil {
		data.raceDetectHookAlloc(size)
		sp = atomicLoadShadowPresence(&data.shadowPresence)
	}
	(*sp)[num] = 1
}

func (p presence) raceDetectHookClearPresent(num uint32) {
	data := p.toRaceDetectData()
	if data == nil {
		return
	}
	sp := atomicLoadShadowPresence(&data.shadowPresence)
	if sp != nil {
		(*sp)[num] = 0

	}
}

// raceDetectHookAllocAndCopy allocates a new shadowPresence slice at lazy and copies
// shadowPresence bytes from src to lazy.
func (p presence) raceDetectHookAllocAndCopy(q presence) {
	sData := q.toRaceDetectData()
	dData := p.toRaceDetectData()
	if sData == nil {
		return
	}
	srcSp := atomicLoadShadowPresence(&sData.shadowPresence)
	if srcSp == nil {
		atomicStoreShadowPresence(&dData.shadowPresence, nil)
		return
	}
	n := len(*srcSp)
	dSlice := make([]byte, n)
	atomicStoreShadowPresence(&dData.shadowPresence, &dSlice)
	for i := 0; i < n; i++ {
		dSlice[i] = (*srcSp)[i]
	}
}

// raceDetectHookPresent is called by the generated file interface
// (*proto.internalFuncs) Present to optionally read an unprotected
// shadow bitmap when race detection is enabled. In regular code it is
// a noop.
func raceDetectHookPresent(field *uint32, num uint32) {
	data := findPointerToRaceDetectData(field, num)
	if data == nil {
		return
	}
	sp := atomicLoadShadowPresence(&data.shadowPresence)
	if sp != nil {
		_ = (*sp)[num]
	}
}

// raceDetectHookSetPresent is called by the generated file interface
// (*proto.internalFuncs) SetPresent to optionally write an unprotected
// shadow bitmap when race detection is enabled. In regular code it is
// a noop.
func raceDetectHookSetPresent(field *uint32, num uint32, size presenceSize) {
	data := findPointerToRaceDetectData(field, num)
	if data == nil {
		return
	}
	sp := atomicLoadShadowPresence(&data.shadowPresence)
	if sp == nil {
		data.raceDetectHookAlloc(size)
		sp = atomicLoadShadowPresence(&data.shadowPresence)
	}
	(*sp)[num] = 1
}

// raceDetectHookClearPresent is called by the generated file interface
// (*proto.internalFuncs) ClearPresent to optionally write an unprotected
// shadow bitmap when race detection is enabled. In regular code it is
// a noop.
func raceDetectHookClearPresent(field *uint32, num uint32) {
	data := findPointerToRaceDetectData(field, num)
	if data == nil {
		return
	}
	sp := atomicLoadShadowPresence(&data.shadowPresence)
	if sp != nil {
		(*sp)[num] = 0
	}
}
