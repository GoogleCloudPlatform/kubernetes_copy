// Copyright 2015 The etcd Authors
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

package v2stats

import (
	"sync"
	"time"
)

const (
	queueCapacity = 200
)

// RequestStats represent the stats for a request.
// It encapsulates the sending time and the size of the request.
type RequestStats struct {
	SendingTime time.Time
	Size        int
}

type statsQueue struct {
	items        [queueCapacity]*RequestStats
	size         int
	front        int
	back         int
	totalReqSize int
	rwl          sync.RWMutex
}

func (q *statsQueue) Len() int {
	return q.size
}

func (q *statsQueue) ReqSize() int {
	return q.totalReqSize
}

// FrontAndBack gets the front and back elements in the queue
// We must grab front and back together with the protection of the lock
func (q *statsQueue) frontAndBack() (*RequestStats, *RequestStats) {
	q.rwl.RLock()
	defer q.rwl.RUnlock()
	if q.size != 0 {
		return q.items[q.front], q.items[q.back]
	}
	return nil, nil
}

// Insert function insert a RequestStats into the queue and update the records
func (q *statsQueue) Insert(p *RequestStats) {
	q.rwl.Lock()
	defer q.rwl.Unlock()

	q.back = (q.back + 1) % queueCapacity

	if q.size == queueCapacity { //dequeue
		q.totalReqSize -= q.items[q.front].Size
		q.front = (q.back + 1) % queueCapacity
	} else {
		q.size++
	}

	q.items[q.back] = p
	q.totalReqSize += q.items[q.back].Size
}

// Rate function returns the package rate and byte rate
func (q *statsQueue) Rate() (float64, float64) {
	front, back := q.frontAndBack()

	if front == nil || back == nil {
		return 0, 0
	}

	if time.Since(back.SendingTime) > time.Second {
		q.Clear()
		return 0, 0
	}

	sampleDuration := back.SendingTime.Sub(front.SendingTime)

	pr := float64(q.Len()) / float64(sampleDuration) * float64(time.Second)

	br := float64(q.ReqSize()) / float64(sampleDuration) * float64(time.Second)

	return pr, br
}

// Clear function clear up the statsQueue
func (q *statsQueue) Clear() {
	q.rwl.Lock()
	defer q.rwl.Unlock()
	q.back = -1
	q.front = 0
	q.size = 0
	q.totalReqSize = 0
}
