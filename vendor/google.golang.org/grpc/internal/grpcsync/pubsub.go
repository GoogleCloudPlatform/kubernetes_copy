/*
 *
 * Copyright 2023 gRPC authors.
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

package grpcsync

import (
	"context"
	"sync"
)

// Subscriber represents an entity that is subscribed to messages published on
// a PubSub. It wraps the callback to be invoked by the PubSub when a new
// message is published.
type Subscriber interface {
	// OnMessage is invoked when a new message is published. Implementations
	// must not block in this method.
	OnMessage(msg any)
}

// PubSub is a simple one-to-many publish-subscribe system that supports
// messages of arbitrary type. It guarantees that messages are delivered in
// the same order in which they were published.
//
// Publisher invokes the Publish() method to publish new messages, while
// subscribers interested in receiving these messages register a callback
// via the Subscribe() method.
//
// Once a PubSub is stopped, no more messages can be published, but any pending
// published messages will be delivered to the subscribers.  Done may be used
// to determine when all published messages have been delivered.
type PubSub struct {
	cs *CallbackSerializer

	// Access to the below fields are guarded by this mutex.
	mu          sync.Mutex
	msg         any
	subscribers map[Subscriber]bool
}

// NewPubSub returns a new PubSub instance.  Users should cancel the
// provided context to shutdown the PubSub.
func NewPubSub(ctx context.Context) *PubSub {
	return &PubSub{
		cs:          NewCallbackSerializer(ctx),
		subscribers: map[Subscriber]bool{},
	}
}

// Subscribe registers the provided Subscriber to the PubSub.
//
// If the PubSub contains a previously published message, the Subscriber's
// OnMessage() callback will be invoked asynchronously with the existing
// message to begin with, and subsequently for every newly published message.
//
// The caller is responsible for invoking the returned cancel function to
// unsubscribe itself from the PubSub.
func (ps *PubSub) Subscribe(sub Subscriber) (cancel func()) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.subscribers[sub] = true

	if ps.msg != nil {
		msg := ps.msg
		ps.cs.TrySchedule(func(context.Context) {
			ps.mu.Lock()
			defer ps.mu.Unlock()
			if !ps.subscribers[sub] {
				return
			}
			sub.OnMessage(msg)
		})
	}

	return func() {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		delete(ps.subscribers, sub)
	}
}

// Publish publishes the provided message to the PubSub, and invokes
// callbacks registered by subscribers asynchronously.
func (ps *PubSub) Publish(msg any) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.msg = msg
	for sub := range ps.subscribers {
		s := sub
		ps.cs.TrySchedule(func(context.Context) {
			ps.mu.Lock()
			defer ps.mu.Unlock()
			if !ps.subscribers[s] {
				return
			}
			s.OnMessage(msg)
		})
	}
}

// Done returns a channel that is closed after the context passed to NewPubSub
// is canceled and all updates have been sent to subscribers.
func (ps *PubSub) Done() <-chan struct{} {
	return ps.cs.Done()
}
