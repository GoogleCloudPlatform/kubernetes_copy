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

package queueset

import (
	"container/list"
)

// removeFromFIFOFunc removes a designated element from the list
// if that element is in the list.
// The complexity of the runtime cost is O(1).
// The returned value is the element removed, if indeed one was removed,
// otherwise `nil`.
type removeFromFIFOFunc func() *request

// walkFunc is called for each request in the list in the
// oldest -> newest order.
// ok: if walkFunc returns false then the iteration stops immediately.
// walkFunc may remove the given request from the fifo,
// but may not mutate the fifo in any other way.
type walkFunc func(*request) (ok bool)

// Internal interface to abstract out the implementation details
// of the underlying list used to maintain the requests.
//
// Note that a fifo, including the removeFromFIFOFuncs returned from Enqueue,
// is not safe for concurrent use by multiple goroutines.
type fifo interface {
	// Enqueue enqueues the specified request into the list and
	// returns a removeFromFIFOFunc function that can be used to remove the
	// request from the list
	Enqueue(*request) removeFromFIFOFunc

	// Dequeue pulls out the oldest request from the list.
	Dequeue() (*request, bool)

	// Peek returns the oldest request without removing it.
	Peek() (*request, bool)

	// Length returns the number of requests in the list.
	Length() int

	// QueueSum returns the sum of initial seats, final seats, and
	// additional latency aggregated from all requests in this queue.
	QueueSum() queueSum

	// Walk iterates through the list in order of oldest -> newest
	// and executes the specified walkFunc for each request in that order.
	//
	// if the specified walkFunc returns false the Walk function
	// stops the walk an returns immediately.
	Walk(walkFunc)
}

// the FIFO list implementation is not safe for concurrent use by multiple
// goroutines.
type requestFIFO struct {
	*list.List

	sum queueSum
}

func newRequestFIFO() fifo {
	return &requestFIFO{
		List: list.New(),
	}
}

func (l *requestFIFO) Length() int {
	return l.Len()
}

func (l *requestFIFO) QueueSum() queueSum {
	return l.sum
}

func (l *requestFIFO) Enqueue(req *request) removeFromFIFOFunc {
	e := l.PushBack(req)
	addToQueueSum(&l.sum, req)

	return func() *request {
		if e.Value == nil {
			return nil
		}
		l.Remove(e)
		e.Value = nil
		deductFromQueueSum(&l.sum, req)
		return req
	}
}

func (l *requestFIFO) Dequeue() (*request, bool) {
	return l.getFirst(true)
}

func (l *requestFIFO) Peek() (*request, bool) {
	return l.getFirst(false)
}

func (l *requestFIFO) getFirst(remove bool) (*request, bool) {
	e := l.Front()
	if e == nil {
		return nil, false
	}

	if remove {
		defer func() {
			l.Remove(e)
			e.Value = nil
		}()
	}

	request, ok := e.Value.(*request)
	if remove && ok {
		deductFromQueueSum(&l.sum, request)
	}
	return request, ok
}

func (l *requestFIFO) Walk(f walkFunc) {
	var next *list.Element
	for current := l.Front(); current != nil; current = next {
		next = current.Next() // f is allowed to remove current
		if r, ok := current.Value.(*request); ok {
			if !f(r) {
				return
			}
		}
	}
}

func addToQueueSum(sum *queueSum, req *request) {
	sum.InitialSeatsSum += req.InitialSeats()
	sum.MaxSeatsSum += req.MaxSeats()
	sum.TotalWorkSum += req.totalWork()
}

func deductFromQueueSum(sum *queueSum, req *request) {
	sum.InitialSeatsSum -= req.InitialSeats()
	sum.MaxSeatsSum -= req.MaxSeats()
	sum.TotalWorkSum -= req.totalWork()
}
