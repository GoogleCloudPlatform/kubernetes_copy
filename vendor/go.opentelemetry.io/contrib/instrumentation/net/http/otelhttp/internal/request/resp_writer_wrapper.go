// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package request // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/request"

import (
	"net/http"
	"sync"
)

var _ http.ResponseWriter = &RespWriterWrapper{}

// RespWriterWrapper wraps a http.ResponseWriter in order to track the number of
// bytes written, the last error, and to catch the first written statusCode.
// TODO: The wrapped http.ResponseWriter doesn't implement any of the optional
// types (http.Hijacker, http.Pusher, http.CloseNotifier, etc)
// that may be useful when using it in real life situations.
type RespWriterWrapper struct {
	http.ResponseWriter
	OnWrite func(n int64) // must not be nil

	mu          sync.RWMutex
	written     int64
	statusCode  int
	err         error
	wroteHeader bool
}

// NewRespWriterWrapper creates a new RespWriterWrapper.
//
// The onWrite attribute is a callback that will be called every time the data
// is written, with the number of bytes that were written.
func NewRespWriterWrapper(w http.ResponseWriter, onWrite func(int64)) *RespWriterWrapper {
	return &RespWriterWrapper{
		ResponseWriter: w,
		OnWrite:        onWrite,
		statusCode:     http.StatusOK, // default status code in case the Handler doesn't write anything
	}
}

// Write writes the bytes array into the [ResponseWriter], and tracks the
// number of bytes written and last error.
func (w *RespWriterWrapper) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.wroteHeader {
		w.writeHeader(http.StatusOK)
	}

	n, err := w.ResponseWriter.Write(p)
	n1 := int64(n)
	w.OnWrite(n1)
	w.written += n1
	w.err = err
	return n, err
}

// WriteHeader persists initial statusCode for span attribution.
// All calls to WriteHeader will be propagated to the underlying ResponseWriter
// and will persist the statusCode from the first call.
// Blocking consecutive calls to WriteHeader alters expected behavior and will
// remove warning logs from net/http where developers will notice incorrect handler implementations.
func (w *RespWriterWrapper) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.writeHeader(statusCode)
}

// writeHeader persists the status code for span attribution, and propagates
// the call to the underlying ResponseWriter.
// It does not acquire a lock, and therefore assumes that is being handled by a
// parent method.
func (w *RespWriterWrapper) writeHeader(statusCode int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.statusCode = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

// Flush implements [http.Flusher].
func (w *RespWriterWrapper) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.wroteHeader {
		w.writeHeader(http.StatusOK)
	}

	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// BytesWritten returns the number of bytes written.
func (w *RespWriterWrapper) BytesWritten() int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.written
}

// BytesWritten returns the HTTP status code that was sent.
func (w *RespWriterWrapper) StatusCode() int {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.statusCode
}

// Error returns the last error.
func (w *RespWriterWrapper) Error() error {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.err
}
