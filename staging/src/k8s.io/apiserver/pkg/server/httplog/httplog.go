/*
Copyright 2014 The Kubernetes Authors.

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

package httplog

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	"k8s.io/apiserver/pkg/server/routine"
	"k8s.io/klog/v2"
)

// StacktracePred returns true if a stacktrace should be logged for this status.
type StacktracePred func(httpStatus int) (logStacktrace bool)

// ShouldLogRequestPred returns true if logging should be enabled for this request
type ShouldLogRequestPred func() bool

type logger interface {
	Addf(format string, data ...interface{})
}

type respLoggerContextKeyType int

// respLoggerContextKey is used to store the respLogger pointer in the request context.
const respLoggerContextKey respLoggerContextKeyType = iota

// Add a layer on top of ResponseWriter, so we can track latency and error
// message sources.
//
// TODO now that we're using go-restful, we shouldn't need to be wrapping
// the http.ResponseWriter. We can recover panics from go-restful, and
// the logging value is questionable.
type respLogger struct {
	hijacked       bool
	statusRecorded bool
	status         int
	statusStack    string
	// mutex is used when accessing addedInfo, addedKeyValuePairs and logStacktracePred.
	// They can be modified by other goroutine when logging happens (in case of request timeout)
	mutex              sync.Mutex
	addedInfo          strings.Builder
	addedKeyValuePairs []interface{}
	startTime          time.Time

	captureErrorOutput bool

	req       *http.Request
	userAgent string
	w         http.ResponseWriter

	logStacktracePred StacktracePred
}

var _ http.ResponseWriter = &respLogger{}
var _ responsewriter.UserProvidedDecorator = &respLogger{}

func (rl *respLogger) Unwrap() http.ResponseWriter {
	return rl.w
}

// Simple logger that logs immediately when Addf is called
type passthroughLogger struct{}

// Addf logs info immediately.
func (passthroughLogger) Addf(format string, data ...interface{}) {
	klog.V(2).Info(fmt.Sprintf(format, data...))
}

// DefaultStacktracePred is the default implementation of StacktracePred.
func DefaultStacktracePred(status int) bool {
	return (status < http.StatusOK || status >= http.StatusInternalServerError) && status != http.StatusSwitchingProtocols
}

const withLoggingLevel = 3

// WithLogging wraps the handler with logging.
func WithLogging(handler http.Handler, pred StacktracePred) http.Handler {
	return withLogging(handler, pred, func() bool {
		return klog.V(withLoggingLevel).Enabled()
	})
}

func withLogging(handler http.Handler, stackTracePred StacktracePred, shouldLogRequest ShouldLogRequestPred) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !shouldLogRequest() {
			handler.ServeHTTP(w, req)
			return
		}

		ctx := req.Context()
		if old := respLoggerFromRequest(req); old != nil {
			panic("multiple WithLogging calls!")
		}

		startTime := time.Now()
		if receivedTimestamp, ok := request.ReceivedTimestampFrom(ctx); ok {
			startTime = receivedTimestamp
		}

		rl := newLoggedWithStartTime(req, w, startTime)
		rl.StacktraceWhen(stackTracePred)
		req = req.WithContext(context.WithValue(ctx, respLoggerContextKey, rl))

		var logFunc func()
		logFunc = rl.Log
		defer func() {
			if logFunc != nil {
				logFunc()
			}
		}()

		w = responsewriter.WrapForHTTP1Or2(rl)
		handler.ServeHTTP(w, req)

		// We need to ensure that the request is logged after it is processed.
		// In case the request is executed in a separate goroutine created via
		// WithRoutine handler in the handler chain (i.e. above handler.ServeHTTP()
		// would return request is completely responded), we want the logging to
		// happen in that goroutine too, so we append it to the task.
		if routine.AppendTask(ctx, &routine.Task{Func: rl.Log}) {
			logFunc = nil
		}
	})
}

// respLoggerFromContext returns the respLogger or nil.
func respLoggerFromContext(ctx context.Context) *respLogger {
	val := ctx.Value(respLoggerContextKey)
	if rl, ok := val.(*respLogger); ok {
		return rl
	}
	return nil
}

func respLoggerFromRequest(req *http.Request) *respLogger {
	return respLoggerFromContext(req.Context())
}

func newLoggedWithStartTime(req *http.Request, w http.ResponseWriter, startTime time.Time) *respLogger {
	logger := &respLogger{
		startTime:         startTime,
		req:               req,
		userAgent:         req.UserAgent(),
		w:                 w,
		logStacktracePred: DefaultStacktracePred,
	}
	return logger
}

// newLogged turns a normal response writer into a logged response writer.
func newLogged(req *http.Request, w http.ResponseWriter) *respLogger {
	return newLoggedWithStartTime(req, w, time.Now())
}

// LogOf returns the logger hiding in w. If there is not an existing logger
// then a passthroughLogger will be created which will log to stdout immediately
// when Addf is called.
func LogOf(req *http.Request, w http.ResponseWriter) logger {
	if rl := respLoggerFromRequest(req); rl != nil {
		return rl
	}
	return &passthroughLogger{}
}

// Unlogged returns the original ResponseWriter, or w if it is not our inserted logger.
func Unlogged(req *http.Request, w http.ResponseWriter) http.ResponseWriter {
	if rl := respLoggerFromRequest(req); rl != nil {
		return rl.w
	}
	return w
}

// StacktraceWhen sets the stacktrace logging predicate, which decides when to log a stacktrace.
// There's a default, so you don't need to call this unless you don't like the default.
func (rl *respLogger) StacktraceWhen(pred StacktracePred) *respLogger {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	rl.logStacktracePred = pred
	return rl
}

// StatusIsNot returns a StacktracePred which will cause stacktraces to be logged
// for any status *not* in the given list.
func StatusIsNot(statuses ...int) StacktracePred {
	statusesNoTrace := map[int]bool{}
	for _, s := range statuses {
		statusesNoTrace[s] = true
	}
	return func(status int) bool {
		_, ok := statusesNoTrace[status]
		return !ok
	}
}

// Addf adds additional data to be logged with this request.
func (rl *respLogger) Addf(format string, data ...interface{}) {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	rl.addedInfo.WriteString(fmt.Sprintf(format, data...))
}

func AddInfof(ctx context.Context, format string, data ...interface{}) {
	if rl := respLoggerFromContext(ctx); rl != nil {
		rl.Addf(format, data...)
	}
}

func (rl *respLogger) AddKeyValue(key string, value interface{}) {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	rl.addedKeyValuePairs = append(rl.addedKeyValuePairs, key, value)
}

// AddKeyValue adds a (key, value) pair to the httplog associated
// with the request.
// Use this function if you want your data to show up in httplog
// in a more structured and readable way.
func AddKeyValue(ctx context.Context, key string, value interface{}) {
	if rl := respLoggerFromContext(ctx); rl != nil {
		rl.AddKeyValue(key, value)
	}
}

// SetStacktracePredicate sets a custom stacktrace predicate for the
// logger associated with the given request context.
func SetStacktracePredicate(ctx context.Context, pred StacktracePred) {
	if rl := respLoggerFromContext(ctx); rl != nil {
		rl.StacktraceWhen(pred)
	}
}

// Log is intended to be called once at the end of your request handler, via defer
func (rl *respLogger) Log() {
	latency := time.Since(rl.startTime)
	auditID := audit.GetAuditIDTruncated(rl.req.Context())
	verb := metrics.NormalizedVerb(rl.req)

	keysAndValues := []interface{}{
		"verb", verb,
		"URI", rl.req.RequestURI,
		"latency", latency,
		// We can't get UserAgent from rl.req.UserAgent() here as it accesses headers map,
		// which can be modified in another goroutine when apiserver request times out.
		// For example authentication filter modifies request's headers,
		// This can cause apiserver to crash with unrecoverable fatal error.
		// More info about concurrent read and write for maps: https://golang.org/doc/go1.6#runtime
		"userAgent", rl.userAgent,
		"audit-ID", auditID,
		"srcIP", rl.req.RemoteAddr,
	}
	// Lock for accessing addedKeyValuePairs and addedInfo
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	keysAndValues = append(keysAndValues, rl.addedKeyValuePairs...)

	if rl.hijacked {
		keysAndValues = append(keysAndValues, "hijacked", true)
	} else {
		keysAndValues = append(keysAndValues, "resp", rl.status)
		if len(rl.statusStack) > 0 {
			keysAndValues = append(keysAndValues, "statusStack", rl.statusStack)
		}
		info := rl.addedInfo.String()
		if len(info) > 0 {
			keysAndValues = append(keysAndValues, "addedInfo", info)
		}
	}

	klog.V(withLoggingLevel).InfoSDepth(1, "HTTP", keysAndValues...)
}

// Header implements http.ResponseWriter.
func (rl *respLogger) Header() http.Header {
	return rl.w.Header()
}

// Write implements http.ResponseWriter.
func (rl *respLogger) Write(b []byte) (int, error) {
	if !rl.statusRecorded {
		rl.recordStatus(http.StatusOK) // Default if WriteHeader hasn't been called
	}
	if rl.captureErrorOutput {
		rl.Addf("logging error output: %q\n", string(b))
	}
	return rl.w.Write(b)
}

// WriteHeader implements http.ResponseWriter.
func (rl *respLogger) WriteHeader(status int) {
	rl.recordStatus(status)
	rl.w.WriteHeader(status)
}

func (rl *respLogger) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rl.hijacked = true

	// the outer ResponseWriter object returned by WrapForHTTP1Or2 implements
	// http.Hijacker if the inner object (rl.w) implements http.Hijacker.
	return rl.w.(http.Hijacker).Hijack()
}

func (rl *respLogger) recordStatus(status int) {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	rl.status = status
	rl.statusRecorded = true
	if rl.logStacktracePred(status) {
		// Only log stacks for errors
		stack := make([]byte, 50*1024)
		stack = stack[:runtime.Stack(stack, false)]
		rl.statusStack = "\n" + string(stack)
		rl.captureErrorOutput = true
	} else {
		rl.statusStack = ""
	}
}
