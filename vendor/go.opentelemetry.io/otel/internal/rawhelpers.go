// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package internal // import "go.opentelemetry.io/otel/internal"

import (
	"math"
	"unsafe"
)

func BoolToRaw(b bool) uint64 { // nolint:revive  // b is not a control flag.
	if b {
		return 1
	}
	return 0
}

func RawToBool(r uint64) bool {
	return r != 0
}

func Int64ToRaw(i int64) uint64 {
	// Assumes original was a valid int64 (overflow not checked).
	return uint64(i) // nolint: gosec
}

func RawToInt64(r uint64) int64 {
	// Assumes original was a valid int64 (overflow not checked).
	return int64(r) // nolint: gosec
}

func Float64ToRaw(f float64) uint64 {
	return math.Float64bits(f)
}

func RawToFloat64(r uint64) float64 {
	return math.Float64frombits(r)
}

func RawPtrToFloat64Ptr(r *uint64) *float64 {
	// Assumes original was a valid *float64 (overflow not checked).
	return (*float64)(unsafe.Pointer(r)) // nolint: gosec
}

func RawPtrToInt64Ptr(r *uint64) *int64 {
	// Assumes original was a valid *int64 (overflow not checked).
	return (*int64)(unsafe.Pointer(r)) // nolint: gosec
}
