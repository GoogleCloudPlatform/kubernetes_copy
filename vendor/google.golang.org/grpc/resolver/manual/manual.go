/*
 *
 * Copyright 2017 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package manual defines a resolver that can be used to manually send resolved
// addresses to ClientConn.
package manual

import (
	"sync"

	"google.golang.org/grpc/resolver"
)

// NewBuilderWithScheme creates a new manual resolver builder with the given
// scheme. Every instance of the manual resolver may only ever be used with a
// single grpc.ClientConn. Otherwise, bad things will happen.
func NewBuilderWithScheme(scheme string) *Resolver {
	return &Resolver{
		BuildCallback:       func(resolver.Target, resolver.ClientConn, resolver.BuildOptions) {},
		UpdateStateCallback: func(error) {},
		ResolveNowCallback:  func(resolver.ResolveNowOptions) {},
		CloseCallback:       func() {},
		scheme:              scheme,
	}
}

// Resolver is also a resolver builder.
// It's build() function always returns itself.
type Resolver struct {
	// BuildCallback is called when the Build method is called.  Must not be
	// nil.  Must not be changed after the resolver may be built.
	BuildCallback func(resolver.Target, resolver.ClientConn, resolver.BuildOptions)
	// UpdateStateCallback is called when the UpdateState method is called on
	// the resolver.  The value passed as argument to this callback is the value
	// returned by the resolver.ClientConn.  Must not be nil.  Must not be
	// changed after the resolver may be built.
	UpdateStateCallback func(err error)
	// ResolveNowCallback is called when the ResolveNow method is called on the
	// resolver.  Must not be nil.  Must not be changed after the resolver may
	// be built.
	ResolveNowCallback func(resolver.ResolveNowOptions)
	// CloseCallback is called when the Close method is called.  Must not be
	// nil.  Must not be changed after the resolver may be built.
	CloseCallback func()
	scheme        string

	// Fields actually belong to the resolver.
	// Guards access to below fields.
	mu sync.Mutex
	CC resolver.ClientConn
	// Storing the most recent state update makes this resolver resilient to
	// restarts, which is possible with channel idleness.
	lastSeenState *resolver.State
}

// InitialState adds initial state to the resolver so that UpdateState doesn't
// need to be explicitly called after Dial.
func (r *Resolver) InitialState(s resolver.State) {
	r.lastSeenState = &s
}

// Build returns itself for Resolver, because it's both a builder and a resolver.
func (r *Resolver) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Call BuildCallback after locking to avoid a race when UpdateState
	// or ReportError is called before Build returns.
	r.BuildCallback(target, cc, opts)
	r.CC = cc
	if r.lastSeenState != nil {
		err := r.CC.UpdateState(*r.lastSeenState)
		go r.UpdateStateCallback(err)
	}
	return r, nil
}

// Scheme returns the manual resolver's scheme.
func (r *Resolver) Scheme() string {
	return r.scheme
}

// ResolveNow is a noop for Resolver.
func (r *Resolver) ResolveNow(o resolver.ResolveNowOptions) {
	r.ResolveNowCallback(o)
}

// Close is a noop for Resolver.
func (r *Resolver) Close() {
	r.CloseCallback()
}

// UpdateState calls CC.UpdateState.
func (r *Resolver) UpdateState(s resolver.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.CC == nil {
		panic("cannot update state as grpc.Dial with resolver has not been called")
	}
	err = r.CC.UpdateState(s)
	r.lastSeenState = &s
	r.UpdateStateCallback(err)
}

// ReportError calls CC.ReportError.
func (r *Resolver) ReportError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.CC == nil {
		panic("cannot report error as grpc.Dial with resolver has not been called")
	}
	r.CC.ReportError(err)
}
