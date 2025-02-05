/*
Copyright 2016 The Kubernetes Authors.

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

package filters

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	"k8s.io/apiserver/pkg/server/httplog"
)

// WithTimeoutForNonLongRunningRequests times out non-long-running requests after the time given by timeout.
func WithTimeoutForNonLongRunningRequests(handler http.Handler, longRunning apirequest.LongRunningRequestCheck) http.Handler {
	if longRunning == nil {
		return handler
	}
	timeoutFunc := func(req *http.Request) (*http.Request, bool, func(), *apierrors.StatusError) {
		// TODO unify this with apiserver.MaxInFlightLimit
		ctx := req.Context()

		requestInfo, ok := apirequest.RequestInfoFrom(ctx)
		if !ok {
			// if this happens, the handler chain isn't setup correctly because there is no request info
			return req, false, func() {}, apierrors.NewInternalError(fmt.Errorf("no request info found for request during timeout"))
		}

		if longRunning(req, requestInfo) {
			return req, true, nil, nil
		}

		postTimeoutFn := func() {
			metrics.RecordRequestTermination(req, requestInfo, metrics.APIServerComponent, http.StatusGatewayTimeout)
		}
		return req, false, postTimeoutFn, apierrors.NewTimeoutError("request did not complete within the allotted timeout", 0)
	}
	return WithTimeout(handler, timeoutFunc)
}

type timeoutFunc = func(*http.Request) (req *http.Request, longRunning bool, postTimeoutFunc func(), err *apierrors.StatusError)

// WithTimeout returns an http.Handler that runs h with a timeout
// determined by timeoutFunc. The new http.Handler calls h.ServeHTTP to handle
// each request, but if a call runs for longer than its time limit, the
// handler responds with a 504 Gateway Timeout error and the message
// provided. (If msg is empty, a suitable default message will be sent.) After
// the handler times out, writes by h to its http.ResponseWriter will return
// http.ErrHandlerTimeout. If timeoutFunc returns a nil timeout channel, no
// timeout will be enforced. recordFn is a function that will be invoked whenever
// a timeout happens.
func WithTimeout(h http.Handler, timeoutFunc timeoutFunc) http.Handler {
	return &timeoutHandler{h, timeoutFunc}
}

type timeoutHandler struct {
	handler http.Handler
	timeout timeoutFunc
}

func (t *timeoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r, longRunning, postTimeoutFn, err := t.timeout(r)
	if longRunning {
		t.handler.ServeHTTP(w, r)
		return
	}

	timeoutCh := r.Context().Done()

	// resultCh is used as both errCh and stopCh
	resultCh := make(chan interface{})
	var tw timeoutWriter
	tw, w = newTimeoutWriter(w)

	// Make a copy of request and work on it in new goroutine
	// to avoid race condition when accessing/modifying request (e.g. headers)
	rCopy := r.Clone(r.Context())
	go func() {
		defer func() {
			err := recover()
			// do not wrap the sentinel ErrAbortHandler panic value
			if err != nil && err != http.ErrAbortHandler {
				// Same as stdlib http server code. Manually allocate stack
				// trace buffer size to prevent excessively large logs
				const size = 64 << 10
				buf := make([]byte, size)
				buf = buf[:runtime.Stack(buf, false)]
				err = fmt.Sprintf("%v\n%s", err, buf)
			}
			resultCh <- err
		}()
		t.handler.ServeHTTP(w, rCopy)
	}()
	select {
	case err := <-resultCh:
		// panic if error occurs; stop otherwise
		if err != nil {
			panic(err)
		}
		return
	case <-timeoutCh:
		defer func() {
			// resultCh needs to have a reader, since the function doing
			// the work needs to send to it. This is deferred to ensure it runs
			// ever if the post timeout work itself panics.
			go func() {
				timedOutAt := time.Now()
				res := <-resultCh

				status := metrics.PostTimeoutHandlerOK
				if res != nil {
					// a non nil res indicates that there was a panic.
					status = metrics.PostTimeoutHandlerPanic
				}

				metrics.RecordRequestPostTimeout(metrics.PostTimeoutSourceTimeoutHandler, status)
				utilruntime.HandleErrorWithContext(r.Context(), nil, "Post-timeout activity", "timeElapsed", time.Since(timedOutAt), "method", r.Method, "path", r.URL.Path, "result", res)
			}()
		}()
		httplog.SetStacktracePredicate(r.Context(), func(status int) bool {
			return false
		})
		defer postTimeoutFn()
		tw.timeout(err)
	}
}

type timeoutWriter interface {
	http.ResponseWriter
	timeout(*apierrors.StatusError)
}

func newTimeoutWriter(w http.ResponseWriter) (timeoutWriter, http.ResponseWriter) {
	base := &baseTimeoutWriter{w: w, handlerHeaders: w.Header().Clone()}
	wrapped := responsewriter.WrapForHTTP1Or2(base)

	return base, wrapped
}

var _ http.ResponseWriter = &baseTimeoutWriter{}
var _ responsewriter.UserProvidedDecorator = &baseTimeoutWriter{}

type baseTimeoutWriter struct {
	w http.ResponseWriter

	// headers written by the normal handler
	handlerHeaders http.Header

	mu sync.Mutex
	// if the timeout handler has timeout
	timedOut bool
	// if this timeout writer has wrote header
	wroteHeader bool
	// if this timeout writer has been hijacked
	hijacked bool
}

func (tw *baseTimeoutWriter) Unwrap() http.ResponseWriter {
	return tw.w
}

func (tw *baseTimeoutWriter) Header() http.Header {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.timedOut {
		return http.Header{}
	}

	return tw.handlerHeaders
}

func (tw *baseTimeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if tw.hijacked {
		return 0, http.ErrHijacked
	}

	if !tw.wroteHeader {
		copyHeaders(tw.w.Header(), tw.handlerHeaders)
		tw.wroteHeader = true
	}
	return tw.w.Write(p)
}

func (tw *baseTimeoutWriter) Flush() {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.timedOut {
		return
	}

	// the outer ResponseWriter object returned by WrapForHTTP1Or2 implements
	// http.Flusher if the inner object (tw.w) implements http.Flusher.
	tw.w.(http.Flusher).Flush()
}

func (tw *baseTimeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.timedOut || tw.wroteHeader || tw.hijacked {
		return
	}

	copyHeaders(tw.w.Header(), tw.handlerHeaders)
	tw.wroteHeader = true
	tw.w.WriteHeader(code)
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
}

func (tw *baseTimeoutWriter) timeout(err *apierrors.StatusError) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	tw.timedOut = true

	// The timeout writer has not been used by the inner handler.
	// We can safely timeout the HTTP request by sending by a timeout
	// handler
	if !tw.wroteHeader && !tw.hijacked {
		tw.w.WriteHeader(http.StatusGatewayTimeout)
		enc := json.NewEncoder(tw.w)
		enc.Encode(&err.ErrStatus)
	} else {
		// The timeout writer has been used by the inner handler. There is
		// no way to timeout the HTTP request at the point. We have to shutdown
		// the connection for HTTP1 or reset stream for HTTP2.
		//
		// Note from the golang's docs:
		// If ServeHTTP panics, the server (the caller of ServeHTTP) assumes
		// that the effect of the panic was isolated to the active request.
		// It recovers the panic, logs a stack trace to the server error log,
		// and either closes the network connection or sends an HTTP/2
		// RST_STREAM, depending on the HTTP protocol. To abort a handler so
		// the client sees an interrupted response but the server doesn't log
		// an error, panic with the value ErrAbortHandler.
		//
		// We are throwing http.ErrAbortHandler deliberately so that a client is notified and to suppress a not helpful stacktrace in the logs
		panic(http.ErrAbortHandler)
	}
}

func (tw *baseTimeoutWriter) CloseNotify() <-chan bool {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.timedOut {
		done := make(chan bool)
		close(done)
		return done
	}

	// the outer ResponseWriter object returned by WrapForHTTP1Or2 implements
	// http.CloseNotifier if the inner object (tw.w) implements http.CloseNotifier.
	return tw.w.(http.CloseNotifier).CloseNotify()
}

func (tw *baseTimeoutWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.timedOut {
		return nil, nil, http.ErrHandlerTimeout
	}

	// the outer ResponseWriter object returned by WrapForHTTP1Or2 implements
	// http.Hijacker if the inner object (tw.w) implements http.Hijacker.
	conn, rw, err := tw.w.(http.Hijacker).Hijack()
	if err == nil {
		tw.hijacked = true
	}
	return conn, rw, err
}
