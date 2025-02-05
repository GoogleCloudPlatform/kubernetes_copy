/*
Copyright 2019 The Kubernetes Authors.

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

package promise

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/util/flowcontrol/counter"
	promiseifc "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/promise"
)

// countingPromise implements the WriteOnce interface.
// This implementation is based on a condition variable.
// This implementation tracks active goroutines:
// the given counter is decremented for a goroutine waiting for this
// variable to be set and incremented when such a goroutine is
// unblocked.
type countingPromise struct {
	lock          sync.Locker
	cond          sync.Cond
	activeCounter counter.GoRoutineCounter // counter of active goroutines
	waitingCount  int                      // number of goroutines idle due to this being unset
	isSet         bool
	value         interface{}
}

var _ promiseifc.WriteOnce = &countingPromise{}

// NewCountingWriteOnce creates a WriteOnce that uses locking and counts goroutine activity.
//
// The final three arguments are like those for a regular WriteOnce factory:
// - an optional initial value,
// - an optional "done" channel,
// - the value that is Set after the "done" channel becomes selectable.
// Note that for this implementation, the reaction to `doneCh`
// becoming selectable does not wait for a Get.
// If `doneCh != nil` then the caller promises to close it reasonably promptly
// (to the degree allowed by the Go runtime scheduler), and increment the
// goroutine counter before that.
// The WriteOnce's Get method must be called without the lock held.
// The WriteOnce's Set method must be called with the lock held.
func NewCountingWriteOnce(activeCounter counter.GoRoutineCounter, lock sync.Locker, initial interface{}, doneCh <-chan struct{}, doneVal interface{}) promiseifc.WriteOnce {
	p := &countingPromise{
		lock:          lock,
		cond:          *sync.NewCond(lock),
		activeCounter: activeCounter,
		isSet:         initial != nil,
		value:         initial,
	}
	if doneCh != nil {
		activeCounter.Add(1) // count start of the following goroutine
		go func() {
			defer activeCounter.Add(-1) // count completion of this goroutine
			defer runtime.HandleCrash()
			activeCounter.Add(-1) // count suspension for channel receive
			<-doneCh
			// Whatever goroutine unblocked the preceding receive MUST
			// have already accounted for this activation.
			lock.Lock()
			defer lock.Unlock()
			p.Set(doneVal)
		}()
	}
	return p
}

func (p *countingPromise) Get() interface{} {
	p.lock.Lock()
	defer p.lock.Unlock()
	if !p.isSet {
		p.waitingCount++
		p.activeCounter.Add(-1)
		p.cond.Wait()
	}
	return p.value
}

func (p *countingPromise) Set(value interface{}) bool {
	if p.isSet {
		return false
	}
	p.isSet = true
	p.value = value
	if p.waitingCount > 0 {
		p.activeCounter.Add(p.waitingCount)
		p.waitingCount = 0
		p.cond.Broadcast()
	}
	return true
}
