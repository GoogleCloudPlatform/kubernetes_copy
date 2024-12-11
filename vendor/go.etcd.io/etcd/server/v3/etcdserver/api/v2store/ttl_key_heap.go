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

package v2store

import "container/heap"

// An TTLKeyHeap is a min-heap of TTLKeys order by expiration time
type ttlKeyHeap struct {
	array  []*node
	keyMap map[*node]int
}

func newTTLKeyHeap() *ttlKeyHeap {
	h := &ttlKeyHeap{keyMap: make(map[*node]int)}
	heap.Init(h)
	return h
}

func (h ttlKeyHeap) Len() int {
	return len(h.array)
}

func (h ttlKeyHeap) Less(i, j int) bool {
	return h.array[i].ExpireTime.Before(h.array[j].ExpireTime)
}

func (h ttlKeyHeap) Swap(i, j int) {
	// swap node
	h.array[i], h.array[j] = h.array[j], h.array[i]

	// update map
	h.keyMap[h.array[i]] = i
	h.keyMap[h.array[j]] = j
}

func (h *ttlKeyHeap) Push(x any) {
	n, _ := x.(*node)
	h.keyMap[n] = len(h.array)
	h.array = append(h.array, n)
}

func (h *ttlKeyHeap) Pop() any {
	old := h.array
	n := len(old)
	x := old[n-1]
	// Set slice element to nil, so GC can recycle the node.
	// This is due to golang GC doesn't support partial recycling:
	// https://github.com/golang/go/issues/9618
	old[n-1] = nil
	h.array = old[0 : n-1]
	delete(h.keyMap, x)
	return x
}

func (h *ttlKeyHeap) top() *node {
	if h.Len() != 0 {
		return h.array[0]
	}
	return nil
}

func (h *ttlKeyHeap) pop() *node {
	x := heap.Pop(h)
	n, _ := x.(*node)
	return n
}

func (h *ttlKeyHeap) push(x any) {
	heap.Push(h, x)
}

func (h *ttlKeyHeap) update(n *node) {
	index, ok := h.keyMap[n]
	if ok {
		heap.Remove(h, index)
		heap.Push(h, n)
	}
}

func (h *ttlKeyHeap) remove(n *node) {
	index, ok := h.keyMap[n]
	if ok {
		heap.Remove(h, index)
	}
}
