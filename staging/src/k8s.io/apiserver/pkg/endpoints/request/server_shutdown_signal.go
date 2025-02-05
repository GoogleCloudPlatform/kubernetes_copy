/*
Copyright 2023 The Kubernetes Authors.

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

package request

import (
	"context"
)

// The serverShutdownSignalKeyType type is unexported to prevent collisions
type serverShutdownSignalKeyType int

// serverShutdownSignalKey is the context key for storing the
// watch termination interface instance for a WATCH request.
const serverShutdownSignalKey serverShutdownSignalKeyType = iota

// ServerShutdownSignal is associated with the request context so
// the request handler logic has access to signals related to
// the server shutdown events
type ServerShutdownSignal interface {
	// Signaled when the apiserver is not receiving any new request
	ShuttingDown() <-chan struct{}
}

// ServerShutdownSignalFrom returns the ServerShutdownSignal instance
// associated with the request context.
// If there is no ServerShutdownSignal associated with the context,
// nil is returned.
func ServerShutdownSignalFrom(ctx context.Context) ServerShutdownSignal {
	ev, _ := ctx.Value(serverShutdownSignalKey).(ServerShutdownSignal)
	return ev
}

// WithServerShutdownSignal returns a new context that stores
// the ServerShutdownSignal interface instance.
func WithServerShutdownSignal(parent context.Context, window ServerShutdownSignal) context.Context {
	if ServerShutdownSignalFrom(parent) != nil {
		return parent // Avoid double registering.
	}

	return context.WithValue(parent, serverShutdownSignalKey, window)
}
