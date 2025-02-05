/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package panic

import (
	"fmt"
	"net/http"
	"runtime"
)

// Invoker propagates panic from a spawned goroutine to the request handler
/* func(w http.ResponseWriter, req *http.Request) {
        startHandler := make(chan struct{})
	invoker := NewInvoker()
	go invoker.InvokeWithPanicProtection(func() {
		// decide whether we want to run the handler
                close(startHandler)
	})

	select {
	case <-startHandler:
                // WrapPanic will wrap it if the handler panics
                invoker.WrapPanic(func() {
			handler.ServeHTTP(w, req)
		})
	case err := <-invoker.Done():
                if err != nil {
			// the spawned goroutine panicked
			panic(err)
		}
	}
}
*/
type Invoker interface {
	// InvokeWithPanicProtection invokes the given func with panic
	// protection, if the function panics then:
	// a) the panic is recovered
	// b) the stack trace is caprured
	// c) both the panic value and the stacktrace are captured into an
	// error object, so it can be propagated to the parent (request
	// handler goroutine) via a channel
	// The request handler uses this function to spawn a new goroutine
	InvokeWithPanicProtection(func())

	// WrapPanic invokes the given function, and it ensures that the panic
	// from the spawned goroutine is included in the error
	WrapPanic(func())

	// Done returns nil if the spawned goroutine did not panic, otherwise
	// it returns a non nil interface{} that holds the captured panic
	// value and corresponding stack trace
	// The request handler uses this to wait for the spawned
	// goroutine to finish
	Done() <-chan interface{}
}

func NewInvoker() *invoker {
	return &invoker{ch: make(chan interface{}, 1)}
}

// FormatPanicValueWithStackTrace formats the given panic value
// specified in 'recovered' to an error object.
// If the given panic value is nil, then the function return nil.
// The returned error object includes the runtime stack
// trace from the calling goroutine.
// If the panic value is the sentinel ErrAbortHandler
// then the sentinel is returned as is.
func FormatPanicValueWithStackTrace(recovered interface{}) interface{} {
	if recovered == nil {
		return nil
	}

	// nolint:errorlint // the sentinel ErrAbortHandler panic value is not wrapped
	if recovered == http.ErrAbortHandler {
		return recovered
	}

	// Same as stdlib http server code. Manually allocate stack
	// trace buffer size to prevent excessively large logs
	const size = 64 << 10
	buf := make([]byte, size)
	buf = buf[:runtime.Stack(buf, false)]
	return &recoveredPanicError{
		reason:     recovered,
		stackTrace: buf,
	}
}

// WrapRecoveredPanic wraps the recovered panic value given in
// 'own' and the given panic value in 'other'.
// - own is the recovered panic value from the caller's goroutine
// - other is the recovered panic value from the other goroutine
// via a receiving channel in the caller's goroutine
// If both the caller goroutine and the other/spawned
// goroutine panic, then:
// a) the spawned goroutine should use FormatPanicValueWithStackTrace
// to convert the panic value to an error object and then return
// it to the caller goroutine via the channel
// b) The parent goroutine should use WrapRecoveredPanic to wrap its own
// recovered panic value and the panic value from the spawned goroutine
func WrapRecoveredPanic(own, other interface{}) interface{} {
	switch {
	// both the caller goroutine and the spawned goroutine panicked
	case own != nil && other != nil:
		return &wrappedPanicError{
			own:   own,
			other: other,
		}
	// only the spawned goroutine panicked, no need to wrap
	case other != nil:
		return other
	// only the caller goroutine panicked, no need to wrap
	case own != nil:
		return own
	default:
		return nil
	}
}

type invoker struct {
	ch chan interface{}
}

func (i *invoker) Done() <-chan interface{} { return i.ch }

func (i *invoker) InvokeWithPanicProtection(f func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			i.ch <- FormatPanicValueWithStackTrace(recovered)
		}
	}()

	f()
	i.ch <- nil
}

func (i *invoker) WrapPanic(f func()) {
	defer func() {
		own := recover()
		other := <-i.ch
		if err := WrapRecoveredPanic(own, other); err != nil {
			// one or both goroutines have panicked
			panic(err)
		}
	}()

	f()
}

// recoveredPanicError holds a recovered panic value and the
// coresponding runtime stack trace of the goroutine that panicked
type recoveredPanicError struct {
	// reason is the recovered panic value
	reason interface{}
	// the runtime stack trace of the goroutine that panicked
	stackTrace []byte
}

func (e *recoveredPanicError) Unwrap() error {
	if err, ok := e.reason.(error); ok {
		return err
	}
	return nil
}

func (e *recoveredPanicError) Error() string {
	return fmt.Sprintf("%v\n%s", e.reason, e.stackTrace)
}

type wrappedPanicError struct {
	own   interface{}
	other interface{}
}

func (e *wrappedPanicError) Unwrap() error {
	// own takes preceence, in order to maintain current behavior
	if err, ok := e.own.(error); ok {
		return err
	}
	return nil
}

func (e *wrappedPanicError) Error() string {
	return fmt.Sprintf("the other goroutine panicked with: %v\n%v", e.other, e.own)
}
