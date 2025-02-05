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

package filters

import (
	"net/http"
	"strings"
)

var (
	// health probes and metrics scraping are never rejected, we will continue
	// serving these requests after shutdown delay duration elapses.
	pathPrefixesExemptFromRetryAfter = []string{
		"/readyz",
		"/livez",
		"/healthz",
		"/metrics",
	}
)

// isRequestExemptFunc returns true if the request should not be rejected,
// with a Retry-After response, otherwise it returns false.
type isRequestExemptFunc func(*http.Request) bool

// retryAfterParams dictates how the Retry-After response is constructed
type retryAfterParams struct {
	// TearDownConnection is true when we should send a 'Connection: close'
	// header in the response so net/http can tear down the TCP connection.
	TearDownConnection bool

	// Message describes why Retry-After response has been sent by the server
	Message string
}

// shouldRespondWithRetryAfterFunc returns true if the requests should
// be rejected with a Retry-After response once certain conditions are met.
// The retryAfterParams returned contains instructions on how to
// construct the Retry-After response.
type shouldRespondWithRetryAfterFunc func() (*retryAfterParams, bool)

// WithRetryAfter rejects any incoming new request(s) with a 429
// if the specified shutdownDelayDurationElapsedFn channel is closed
//
// It includes new request(s) on a new or an existing TCP connection
// Any new request(s) arriving after shutdownDelayDurationElapsedFn is closed
// are replied with a 429 and the following response headers:
//   - 'Retry-After: N` (so client can retry after N seconds, hopefully on a new apiserver instance)
//   - 'Connection: close': tear down the TCP connection
//
// TODO: is there a way to merge WithWaitGroup and this filter?
func WithRetryAfter(handler http.Handler, shutdownDelayDurationElapsedCh <-chan struct{}) http.Handler {
	shutdownRetryAfterParams := &retryAfterParams{
		TearDownConnection: true,
		Message:            "The apiserver is shutting down, please try again later.",
	}

	// NOTE: both WithRetryAfter and WithWaitGroup must use the same exact isRequestExemptFunc 'isRequestExemptFromRetryAfter,
	// otherwise SafeWaitGroup might wait indefinitely and will prevent the server from shutting down gracefully.
	return withRetryAfter(handler, isRequestExemptFromRetryAfter, func() (*retryAfterParams, bool) {
		select {
		case <-shutdownDelayDurationElapsedCh:
			return shutdownRetryAfterParams, true
		default:
			return nil, false
		}
	})
}

func withRetryAfter(handler http.Handler, isRequestExemptFn isRequestExemptFunc, shouldRespondWithRetryAfterFn shouldRespondWithRetryAfterFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		params, send := shouldRespondWithRetryAfterFn()
		if !send || isRequestExemptFn(req) {
			handler.ServeHTTP(w, req)
			return
		}

		// If we are here this means it's time to send Retry-After response
		//
		// Copied from net/http2 library
		// "Connection" headers aren't allowed in HTTP/2 (RFC 7540, 8.1.2.2),
		// but respect "Connection" == "close" to mean sending a GOAWAY and tearing
		// down the TCP connection when idle, like we do for HTTP/1.
		if params.TearDownConnection {
			w.Header().Set("Connection", "close")
		}

		// Return a 429 status asking the client to try again after 5 seconds
		w.Header().Set("Retry-After", "5")
		http.Error(w, params.Message, http.StatusTooManyRequests)
	})
}

// isRequestExemptFromRetryAfter returns true if the given request should be exempt
// from being rejected with a 'Retry-After' response.
// NOTE: both 'WithRetryAfter' and 'WithWaitGroup' filters should use this function
// to exempt the set of requests from being rejected or tracked.
func isRequestExemptFromRetryAfter(r *http.Request) bool {
	return isKubeApiserverUserAgent(r) || hasExemptPathPrefix(r)
}

// isKubeApiserverUserAgent returns true if the user-agent matches
// the one set by the local loopback.
// NOTE: we can't look up the authenticated user information from the
// request context since the authentication filter has not executed yet.
func isKubeApiserverUserAgent(req *http.Request) bool {
	return strings.HasPrefix(req.UserAgent(), "kube-apiserver/")
}

func hasExemptPathPrefix(r *http.Request) bool {
	for _, whiteListedPrefix := range pathPrefixesExemptFromRetryAfter {
		if strings.HasPrefix(r.URL.Path, whiteListedPrefix) {
			return true
		}
	}
	return false
}
