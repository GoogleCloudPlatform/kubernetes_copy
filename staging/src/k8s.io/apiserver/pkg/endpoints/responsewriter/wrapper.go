/*
Copyright 2021 The Kubernetes Authors.

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

package responsewriter

import (
	"bufio"
	"net"
	"net/http"
)

// UserProvidedDecorator represents a user (client that uses this package)
// provided decorator that wraps an inner http.ResponseWriter object.
// The user-provided decorator object must return the inner (decorated)
// http.ResponseWriter object via the Unwrap function.
type UserProvidedDecorator interface {
	http.ResponseWriter

	// Unwrap returns the inner http.ResponseWriter object associated
	// with the user-provided decorator.
	Unwrap() http.ResponseWriter
}

// WrapForHTTP1Or2 accepts a user-provided decorator of an "inner" http.responseWriter
// object and potentially wraps the user-provided decorator with a new http.ResponseWriter
// object that implements http.CloseNotifier, http.Flusher, and/or http.Hijacker by
// delegating to the user-provided decorator (if it implements the relevant method) or
// the inner http.ResponseWriter (otherwise), so that the returned http.ResponseWriter
// object implements the same subset of those interfaces as the inner http.ResponseWriter.
//
// This function handles the following three cases.
//   - The inner ResponseWriter implements `http.CloseNotifier`, `http.Flusher`,
//     and `http.Hijacker` (an HTTP/1.1 sever provides such a ResponseWriter).
//   - The inner ResponseWriter implements `http.CloseNotifier` and `http.Flusher`
//     but not `http.Hijacker` (an HTTP/2 server provides such a ResponseWriter).
//   - All the other cases collapse to this one, in which the given ResponseWriter is returned.
//
// There are three applicable terms:
//   - "outer": this is the ResponseWriter object returned by the WrapForHTTP1Or2 function.
//   - "user-provided decorator" or "middle": this is the user-provided decorator
//     that decorates an inner ResponseWriter object. A user-provided decorator
//     implements the UserProvidedDecorator interface. A user-provided decorator
//     may or may not implement http.CloseNotifier, http.Flusher or http.Hijacker.
//   - "inner": the ResponseWriter that the user-provided decorator extends.
func WrapForHTTP1Or2(decorator UserProvidedDecorator) http.ResponseWriter {
	// from go net/http documentation:
	// The default HTTP/1.x and HTTP/2 ResponseWriter implementations support Flusher
	// Handlers should always test for this ability at runtime.
	//
	// The Hijacker interface is implemented by ResponseWriters that allow an HTTP handler
	// to take over the connection.
	// The default ResponseWriter for HTTP/1.x connections supports Hijacker, but HTTP/2 connections
	// intentionally do not. ResponseWriter wrappers may also not support Hijacker.
	// Handlers should always test for this ability at runtime
	//
	// The CloseNotifier interface is implemented by ResponseWriters which allow detecting
	// when the underlying connection has gone away.
	// Deprecated: the CloseNotifier interface predates Go's context package.
	// New code should use Request.Context instead.
	inner := decorator.Unwrap()
	if innerNotifierFlusher, ok := inner.(CloseNotifierFlusher); ok {
		// for HTTP/2 request, the default ResponseWriter object (http2responseWriter)
		// implements Flusher and CloseNotifier.
		outerHTTP2 := outerWithCloseNotifyAndFlush{
			UserProvidedDecorator:     decorator,
			InnerCloseNotifierFlusher: innerNotifierFlusher,
		}

		if innerHijacker, hijackable := inner.(http.Hijacker); hijackable {
			// for HTTP/1.x request the default implementation of ResponseWriter
			// also implement CloseNotifier, Flusher and Hijacker
			return &outerWithCloseNotifyFlushAndHijack{
				outerWithCloseNotifyAndFlush: outerHTTP2,
				InnerHijacker:                innerHijacker,
			}
		}

		return outerHTTP2
	}

	// we should never be here for either http/1.x or http2 request
	return decorator
}

// CloseNotifierFlusher is a combination of http.CloseNotifier and http.Flusher
// This applies to both http/1.x and http2 requests.
type CloseNotifierFlusher interface {
	http.CloseNotifier
	http.Flusher
}

// GetOriginal goes through the chain of wrapped http.ResponseWriter objects
// and returns the original http.ResponseWriter object provided to the first
// request handler in the filter chain.
func GetOriginal(w http.ResponseWriter) http.ResponseWriter {
	decorator, ok := w.(UserProvidedDecorator)
	if !ok {
		return w
	}

	inner := decorator.Unwrap()
	if inner == w {
		// infinite cycle here, we should never be here though.
		panic("http.ResponseWriter decorator chain has a cycle")
	}

	return GetOriginal(inner)
}

//nolint:staticcheck // SA1019
var _ http.CloseNotifier = outerWithCloseNotifyAndFlush{}
var _ http.Flusher = outerWithCloseNotifyAndFlush{}
var _ http.ResponseWriter = outerWithCloseNotifyAndFlush{}
var _ UserProvidedDecorator = outerWithCloseNotifyAndFlush{}

// outerWithCloseNotifyAndFlush is the outer object that extends the
// user provided decorator with http.CloseNotifier and http.Flusher only.
type outerWithCloseNotifyAndFlush struct {
	// UserProvidedDecorator is the user-provided object, it decorates
	// an inner ResponseWriter object.
	UserProvidedDecorator

	// http.CloseNotifier and http.Flusher for the inner object
	InnerCloseNotifierFlusher CloseNotifierFlusher
}

func (wr outerWithCloseNotifyAndFlush) CloseNotify() <-chan bool {
	if notifier, ok := wr.UserProvidedDecorator.(http.CloseNotifier); ok {
		return notifier.CloseNotify()
	}

	return wr.InnerCloseNotifierFlusher.CloseNotify()
}

func (wr outerWithCloseNotifyAndFlush) Flush() {
	if flusher, ok := wr.UserProvidedDecorator.(http.Flusher); ok {
		flusher.Flush()
		return
	}

	wr.InnerCloseNotifierFlusher.Flush()
}

//lint:file-ignore SA1019 Keep supporting deprecated http.CloseNotifier
var _ http.CloseNotifier = outerWithCloseNotifyFlushAndHijack{}
var _ http.Flusher = outerWithCloseNotifyFlushAndHijack{}
var _ http.Hijacker = outerWithCloseNotifyFlushAndHijack{}
var _ http.ResponseWriter = outerWithCloseNotifyFlushAndHijack{}
var _ UserProvidedDecorator = outerWithCloseNotifyFlushAndHijack{}

// outerWithCloseNotifyFlushAndHijack is the outer object that extends the
// user-provided decorator with http.CloseNotifier, http.Flusher and http.Hijacker.
// This applies to http/1.x requests only.
type outerWithCloseNotifyFlushAndHijack struct {
	outerWithCloseNotifyAndFlush

	// http.Hijacker for the inner object
	InnerHijacker http.Hijacker
}

func (wr outerWithCloseNotifyFlushAndHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := wr.UserProvidedDecorator.(http.Hijacker); ok {
		return hijacker.Hijack()
	}

	return wr.InnerHijacker.Hijack()
}
