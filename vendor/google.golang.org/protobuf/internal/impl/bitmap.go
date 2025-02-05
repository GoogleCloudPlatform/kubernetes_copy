// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !race

package impl

// There is no additional data as we're not running under race detector.
type RaceDetectHookData struct{}

// Empty stubs for when not using the race detector. Calls to these from index.go should be optimized away.
func (presence) raceDetectHookPresent(num uint32)                       {}
func (presence) raceDetectHookSetPresent(num uint32, size presenceSize) {}
func (presence) raceDetectHookClearPresent(num uint32)                  {}
func (presence) raceDetectHookAllocAndCopy(src presence)                {}

// raceDetectHookPresent is called by the generated file interface
// (*proto.internalFuncs) Present to optionally read an unprotected
// shadow bitmap when race detection is enabled. In regular code it is
// a noop.
func raceDetectHookPresent(field *uint32, num uint32) {}

// raceDetectHookSetPresent is called by the generated file interface
// (*proto.internalFuncs) SetPresent to optionally write an unprotected
// shadow bitmap when race detection is enabled. In regular code it is
// a noop.
func raceDetectHookSetPresent(field *uint32, num uint32, size presenceSize) {}

// raceDetectHookClearPresent is called by the generated file interface
// (*proto.internalFuncs) ClearPresent to optionally write an unprotected
// shadow bitmap when race detection is enabled. In regular code it is
// a noop.
func raceDetectHookClearPresent(field *uint32, num uint32) {}
