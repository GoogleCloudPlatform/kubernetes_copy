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

package pullmanager

import "sync"

// namedLockSet stores named locks in order to allow to partition context access
// such that callers that are mutually exclusive based on a string value can
// access the same context at the same time, compared to a global lock that
// would create an unnecessary bottleneck.
type namedLockSet struct {
	globalLock sync.Mutex
	locks      map[string]*sync.Mutex
}

func NewNamedLockSet() *namedLockSet {
	return &namedLockSet{
		globalLock: sync.Mutex{},
		locks:      map[string]*sync.Mutex{},
	}
}

func (n *namedLockSet) Lock(name string) {
	func() {
		n.globalLock.Lock()
		defer n.globalLock.Unlock()
		if _, ok := n.locks[name]; !ok {
			n.locks[name] = &sync.Mutex{}
		}
	}()
	// This call cannot be guarded by the global lock as it would block the access
	// to the other locks
	n.locks[name].Lock()
}

// Unlock unlocks the named lock. Can only be called after a previous Lock() call
// for the same named lock.
func (n *namedLockSet) Unlock(name string) {
	// cannot be locked by the global lock as it would deadlock once GlobalLock() gets activated
	if _, ok := n.locks[name]; ok {
		n.locks[name].Unlock()
	}
}

// GlobalLock first locks access to the named locks and then locks all of the
// set locks
func (n *namedLockSet) GlobalLock() {
	n.globalLock.Lock()
	for _, l := range n.locks {
		l.Lock()
	}
}

// GlobalUnlock should only be called after GlobalLock(). It unlocks all the locks
// of the set and then it also unlocks the global lock gating access to the set locks.
func (n *namedLockSet) GlobalUnlock() {
	for _, l := range n.locks {
		l.Unlock()
	}
	n.globalLock.Unlock()
}
