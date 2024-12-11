// Copyright 2021 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

import (
	"net"
	"time"
)

type ListenerOptions struct {
	Listener     net.Listener
	ListenConfig net.ListenConfig

	socketOpts       *SocketOpts
	tlsInfo          *TLSInfo
	skipTLSInfoCheck bool
	writeTimeout     time.Duration
	readTimeout      time.Duration
}

func newListenOpts(opts ...ListenerOption) *ListenerOptions {
	lnOpts := &ListenerOptions{}
	lnOpts.applyOpts(opts)
	return lnOpts
}

func (lo *ListenerOptions) applyOpts(opts []ListenerOption) {
	for _, opt := range opts {
		opt(lo)
	}
}

// IsTimeout returns true if the listener has a read/write timeout defined.
func (lo *ListenerOptions) IsTimeout() bool { return lo.readTimeout != 0 || lo.writeTimeout != 0 }

// IsSocketOpts returns true if the listener options includes socket options.
func (lo *ListenerOptions) IsSocketOpts() bool {
	if lo.socketOpts == nil {
		return false
	}
	return lo.socketOpts.ReusePort || lo.socketOpts.ReuseAddress
}

// IsTLS returns true if listner options includes TLSInfo.
func (lo *ListenerOptions) IsTLS() bool {
	if lo.tlsInfo == nil {
		return false
	}
	return !lo.tlsInfo.Empty()
}

// ListenerOption are options which can be applied to the listener.
type ListenerOption func(*ListenerOptions)

// WithTimeout allows for a read or write timeout to be applied to the listener.
func WithTimeout(read, write time.Duration) ListenerOption {
	return func(lo *ListenerOptions) {
		lo.writeTimeout = write
		lo.readTimeout = read
	}
}

// WithSocketOpts defines socket options that will be applied to the listener.
func WithSocketOpts(s *SocketOpts) ListenerOption {
	return func(lo *ListenerOptions) { lo.socketOpts = s }
}

// WithTLSInfo adds TLS credentials to the listener.
func WithTLSInfo(t *TLSInfo) ListenerOption {
	return func(lo *ListenerOptions) { lo.tlsInfo = t }
}

// WithSkipTLSInfoCheck when true a transport can be created with an https scheme
// without passing TLSInfo, circumventing not presented error. Skipping this check
// also requires that TLSInfo is not passed.
func WithSkipTLSInfoCheck(skip bool) ListenerOption {
	return func(lo *ListenerOptions) { lo.skipTLSInfoCheck = skip }
}
