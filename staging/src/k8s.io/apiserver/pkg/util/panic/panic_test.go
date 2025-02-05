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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

func TestPanicWrap(t *testing.T) {
	tests := []struct {
		name               string
		senderPanicsWith   interface{}
		receiverPanicsWith interface{}
		err                error
	}{
		{
			name:               "no panic",
			senderPanicsWith:   nil,
			receiverPanicsWith: nil,
			err:                nil,
		},
		{
			name:               "sender panics",
			senderPanicsWith:   fmt.Errorf("sender error"),
			receiverPanicsWith: nil,
			err:                io.EOF,
		},
		{
			name:               "sender panics with http.ErrAbortHandler",
			senderPanicsWith:   http.ErrAbortHandler,
			receiverPanicsWith: nil,
			err:                io.EOF,
		},
		{
			name:               "receiver panics",
			senderPanicsWith:   nil,
			receiverPanicsWith: fmt.Errorf("receiver error"),
			err:                io.EOF,
		},
		{
			name:               "both sender and receiver panic",
			senderPanicsWith:   fmt.Errorf("sender error"),
			receiverPanicsWith: fmt.Errorf("receiver error"),
			err:                io.EOF,
		},
	}

	// a) the request handler spawns a new goroutine, we refer to
	// it as the sender goroutine
	// b) we refer to the goroutine in which the request handler
	// runs as the receiver goroutine
	// c) the receiver waits for the sender to return using a channel
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				// a) kick off the sender goroutine
				senderDone := make(chan interface{}, 1)
				go func() {
					defer func() {
						err := recover()
						// the panic from the sender is converted to
						// an error object and sent to the receiver
						senderDone <- FormatPanicValueWithStackTrace(err)
					}()

					if test.senderPanicsWith != nil {
						// sender panics here
						panic(test.senderPanicsWith)
					}
				}()

				select {
				// c) the receiver waits for the sender to return
				case senderErr := <-senderDone:
					func() {
						defer func() {
							receiverErr := recover()
							// the receiver wraps its own panic value and the
							// panic value it received from the sender
							// with an error object
							if err := WrapRecoveredPanic(receiverErr, senderErr); err != nil {
								panic(err)
							}
						}()

						if test.receiverPanicsWith != nil {
							// receiver panics here
							panic(test.receiverPanicsWith)
						}
					}()
				case <-time.After(wait.ForeverTestTimeout):
					t.Errorf("timed out waiting for child goroutine to return")
				}
			})

			handlerDone := make(chan struct{})
			verifierFn := func(inner http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					// this handler verifies that the panic was wrapped appropriately
					defer close(handlerDone)
					defer func() {
						errGot := recover()
						switch {
						// both sender and receiver are expected to panic
						case test.senderPanicsWith != nil && test.receiverPanicsWith != nil:
							verifyWrappedPanic(t, errGot, test.senderPanicsWith, test.receiverPanicsWith)
						// only the sender panics
						case test.senderPanicsWith != nil:
							verifyRecoveredPanicError(t, errGot, test.senderPanicsWith)
						// only the receiver panics
						case test.receiverPanicsWith != nil:
							if want, got := test.receiverPanicsWith, errGot; want != got {
								t.Errorf("expected the receiver to panic with: %v, but got: %v", want, got)
							}
						// neither the sender, nor the receiver panics
						default:
							if errGot != nil {
								t.Errorf("unexpected panic from the receiver")
							}
						}

						// rethrow the panic so net/http can deal with it.
						if errGot != nil {
							panic(errGot)
						}
					}()

					inner.ServeHTTP(w, req)
				})
			}

			server := httptest.NewUnstartedServer(verifierFn(handler))
			defer server.Close()
			server.StartTLS()

			client := server.Client()
			client.Timeout = wait.ForeverTestTimeout
			_, err := client.Get(server.URL)
			switch {
			case test.err == nil:
				if err != nil {
					t.Errorf("unexpected error from Get: %v", err)
				}
			default:
				if !errors.Is(err, test.err) {
					t.Errorf("expected error: %v, but got: %v", test.err, err)
				}
			}

			select {
			case <-handlerDone:
			case <-time.After(wait.ForeverTestTimeout):
				t.Errorf("timed out waiting for request handler to return")
			}
		})
	}
}

func TestInvoker(t *testing.T) {
	builder := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			startHandler := make(chan struct{})
			invoker := NewInvoker()
			// split the hanlder by kicking off a new goroutine
			go invoker.InvokeWithPanicProtection(func() {
				// do something
				time.Sleep(time.Millisecond)

				// resume the original handler
				close(startHandler)
				panic("the worker goroutine panics")
			})

			select {
			case <-startHandler:
				invoker.WrapPanic(func() {
					// WrapPanic will wrap it if the handler panics
					handler.ServeHTTP(w, req)
				})
			case err := <-invoker.Done():
				if err != nil {
					// the handler  panics
					panic(err)
				}
			}
		})
	}

	handler := builder(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		panic("the request handler goroutine panics")
	}))
	server := httptest.NewUnstartedServer(handler)
	defer server.Close()
	server.StartTLS()

	client := server.Client()
	client.Timeout = wait.ForeverTestTimeout
	if _, err := client.Get(server.URL); !errors.Is(err, io.EOF) {
		t.Errorf("expected error: %v, but got: %v", io.EOF, err)
	}
}

func verifyWrappedPanic(t *testing.T, wrapped interface{}, senderReason, receiverReason interface{}) {
	t.Helper()

	if wrapped == nil {
		t.Errorf("expected a panic")
		return
	}
	if _, ok := wrapped.(error); !ok {
		t.Errorf("expected the panic to be wrapped with an error object, but got: %v", wrapped)
		return
	}
	var err *wrappedPanicError
	if !errors.As(wrapped.(error), &err) {
		t.Errorf("expected the panic to be wrapped with an error of type: %T, but got: %v", &wrappedPanicError{}, wrapped)
		return
	}
	if want, got := receiverReason, err.own; want != got {
		t.Errorf("expected panic reason: %v: but got: %v", want, got)
	}

	if want := fmt.Sprintf("the other goroutine panicked with: %v", senderReason); !strings.Contains(err.Error(), want) {
		t.Errorf("expected error message to conatin: %q, but got: %v", want, err.Error())
	}
	if want := fmt.Sprintf("%v", senderReason); !strings.Contains(err.Error(), want) {
		t.Errorf("expected error message to conatin: %q, but got: %v", want, err.Error())
	}

	verifyRecoveredPanicError(t, err.other, senderReason)
}

func verifyRecoveredPanicError(t *testing.T, converted interface{}, senderReason interface{}) {
	t.Helper()

	if converted == nil {
		t.Errorf("expected a panic from the sender")
		return
	}

	switch {
	// nolint:errorlint // this is a controlled test, so assert with exact
	// match and we don't expect http.ErrAbortHandler to be wrapped
	case senderReason == http.ErrAbortHandler:
		if want, got := http.ErrAbortHandler, senderReason; want != got {
			t.Errorf("expected panic reason: %v, but got: %v", want, got)
		}
	default:
		if _, ok := converted.(error); !ok {
			t.Errorf("expected the panic from the sender to be an error object, but got: %v", converted)
			return
		}

		var err *recoveredPanicError
		if !errors.As(converted.(error), &err) {
			t.Errorf("expected the panic from the sender to be of type: %T, but got: %v", &recoveredPanicError{}, converted)
			return
		}

		// nolint:errorlint // this is a controlled test, assert with exact match
		if want, got := senderReason, err.reason; want != got {
			t.Errorf("expected panic reason: %v, but got: %v", want, got)
		}

		switch {
		// nolint:errorlint // this is a controlled test, so assert with exact
		// match and we don't expect http.ErrAbortHandler to be wrapped
		case err.reason != http.ErrAbortHandler:
			if len(err.stackTrace) == 0 {
				t.Errorf("expected stack trace from the sender to be captured")
			}

		// nolint:errorlint // this is a controlled test, so assert with exact
		// match and we don't expect http.ErrAbortHandler to be wrapped
		case err.reason == http.ErrAbortHandler:
			if len(err.stackTrace) > 0 {
				t.Errorf("did not expect stack trace from the sender to be captured, but got: %s", string(err.stackTrace))
			}
		}
	}
}
