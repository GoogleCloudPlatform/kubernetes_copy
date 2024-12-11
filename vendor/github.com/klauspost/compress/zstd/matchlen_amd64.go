//go:build amd64 && !appengine && !noasm && gc
// +build amd64,!appengine,!noasm,gc

// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.

package zstd

// matchLen returns how many bytes match in a and b
//
// It assumes that:
//
//	len(a) <= len(b) and len(a) > 0
//
//go:noescape
func matchLen(a []byte, b []byte) int
