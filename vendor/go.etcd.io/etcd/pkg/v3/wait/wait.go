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

// Package wait provides utility functions for polling, listening using Go
// channel.
package wait

import (
	"log"
	"sync"
)

const (
	// To avoid lock contention we use an array of list struct (rw mutex & map)
	// for the id argument, we apply mod operation and uses its remainder to
	// index into the array and find the corresponding element.
	defaultListElementLength = 64
)

// Wait is an interface that provides the ability to wait and trigger events that
// are associated with IDs.
type Wait interface {
	// Register waits returns a chan that waits on the given ID.
	// The chan will be triggered when Trigger is called with
	// the same ID.
	Register(id uint64) <-chan any
	// Trigger triggers the waiting chans with the given ID.
	Trigger(id uint64, x any)
	IsRegistered(id uint64) bool
}

type list struct {
	e []listElement
}

type listElement struct {
	l sync.RWMutex
	m map[uint64]chan any
}

// New creates a Wait.
func New() Wait {
	res := list{
		e: make([]listElement, defaultListElementLength),
	}
	for i := 0; i < len(res.e); i++ {
		res.e[i].m = make(map[uint64]chan any)
	}
	return &res
}

func (w *list) Register(id uint64) <-chan any {
	idx := id % defaultListElementLength
	newCh := make(chan any, 1)
	w.e[idx].l.Lock()
	defer w.e[idx].l.Unlock()
	if _, ok := w.e[idx].m[id]; !ok {
		w.e[idx].m[id] = newCh
	} else {
		log.Panicf("dup id %x", id)
	}
	return newCh
}

func (w *list) Trigger(id uint64, x any) {
	idx := id % defaultListElementLength
	w.e[idx].l.Lock()
	ch := w.e[idx].m[id]
	delete(w.e[idx].m, id)
	w.e[idx].l.Unlock()
	if ch != nil {
		ch <- x
		close(ch)
	}
}

func (w *list) IsRegistered(id uint64) bool {
	idx := id % defaultListElementLength
	w.e[idx].l.RLock()
	defer w.e[idx].l.RUnlock()
	_, ok := w.e[idx].m[id]
	return ok
}

type waitWithResponse struct {
	ch <-chan any
}

func NewWithResponse(ch <-chan any) Wait {
	return &waitWithResponse{ch: ch}
}

func (w *waitWithResponse) Register(id uint64) <-chan any {
	return w.ch
}
func (w *waitWithResponse) Trigger(id uint64, x any) {}
func (w *waitWithResponse) IsRegistered(id uint64) bool {
	panic("waitWithResponse.IsRegistered() shouldn't be called")
}
