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

package notify

import (
	"sync"
)

// Notifier is a thread safe struct that can be used to send notification about
// some event to multiple consumers.
type Notifier struct {
	mu      sync.RWMutex
	channel chan struct{}
}

// NewNotifier returns new notifier
func NewNotifier() *Notifier {
	return &Notifier{
		channel: make(chan struct{}),
	}
}

// Receive returns channel that can be used to wait for notification.
// Consumers will be informed by closing the channel.
func (n *Notifier) Receive() <-chan struct{} {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.channel
}

// Notify closes the channel passed to consumers and creates new channel to used
// for next notification.
func (n *Notifier) Notify() {
	newChannel := make(chan struct{})
	n.mu.Lock()
	channelToClose := n.channel
	n.channel = newChannel
	n.mu.Unlock()
	close(channelToClose)
}
