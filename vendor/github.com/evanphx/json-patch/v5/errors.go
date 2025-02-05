package jsonpatch

import "fmt"

// AccumulatedCopySizeError is an error type returned when the accumulated size
// increase caused by copy operations in a patch operation has exceeded the
// limit.
type AccumulatedCopySizeError struct {
	limit       int64
	accumulated int64
}

// NewAccumulatedCopySizeError returns an AccumulatedCopySizeError.
func NewAccumulatedCopySizeError(l, a int64) *AccumulatedCopySizeError {
	return &AccumulatedCopySizeError{limit: l, accumulated: a}
}

// Error implements the error interface.
func (a *AccumulatedCopySizeError) Error() string {
	return fmt.Sprintf("Unable to complete the copy, the accumulated size increase of copy is %d, exceeding the limit %d", a.accumulated, a.limit)
}

// ArraySizeError is an error type returned when the array size has exceeded
// the limit.
type ArraySizeError struct {
	limit int
	size  int
}

// NewArraySizeError returns an ArraySizeError.
func NewArraySizeError(l, s int) *ArraySizeError {
	return &ArraySizeError{limit: l, size: s}
}

// Error implements the error interface.
func (a *ArraySizeError) Error() string {
	return fmt.Sprintf("Unable to create array of size %d, limit is %d", a.size, a.limit)
}
