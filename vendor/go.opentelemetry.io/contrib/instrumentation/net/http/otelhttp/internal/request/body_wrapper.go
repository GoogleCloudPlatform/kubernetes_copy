// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package request // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/request"

import (
	"io"
	"sync"
)

var _ io.ReadCloser = &BodyWrapper{}

// BodyWrapper wraps a http.Request.Body (an io.ReadCloser) to track the number
// of bytes read and the last error.
type BodyWrapper struct {
	io.ReadCloser
	OnRead func(n int64) // must not be nil

	mu   sync.Mutex
	read int64
	err  error
}

// NewBodyWrapper creates a new BodyWrapper.
//
// The onRead attribute is a callback that will be called every time the data
// is read, with the number of bytes being read.
func NewBodyWrapper(body io.ReadCloser, onRead func(int64)) *BodyWrapper {
	return &BodyWrapper{
		ReadCloser: body,
		OnRead:     onRead,
	}
}

// Read reads the data from the io.ReadCloser, and stores the number of bytes
// read and the error.
func (w *BodyWrapper) Read(b []byte) (int, error) {
	n, err := w.ReadCloser.Read(b)
	n1 := int64(n)

	w.updateReadData(n1, err)
	w.OnRead(n1)
	return n, err
}

func (w *BodyWrapper) updateReadData(n int64, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.read += n
	if err != nil {
		w.err = err
	}
}

// Closes closes the io.ReadCloser.
func (w *BodyWrapper) Close() error {
	return w.ReadCloser.Close()
}

// BytesRead returns the number of bytes read up to this point.
func (w *BodyWrapper) BytesRead() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.read
}

// Error returns the last error.
func (w *BodyWrapper) Error() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.err
}
