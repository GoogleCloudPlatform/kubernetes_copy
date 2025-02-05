/*
Copyright 2015 The Kubernetes Authors.

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

package container

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Cache stores the PodStatus for the pods. It represents *all* the visible
// pods/containers in the container runtime. All cache entries are at least as
// new or newer than the global timestamp (set by UpdateTime()), while
// individual entries may be slightly newer than the global timestamp. If a pod
// has no states known by the runtime, Cache returns an empty PodStatus object
// with ID populated.
//
// Cache provides two methods to retrieve the PodStatus: the non-blocking Get()
// and the blocking GetNewerThan() method. The component responsible for
// populating the cache is expected to call Delete() to explicitly free the
// cache entries.
type Cache interface {
	Get(types.UID) (*PodStatus, error)
	// Set updates the cache by setting the PodStatus for the pod only
	// if the data is newer than the cache based on the provided
	// time stamp. Returns if the cache was updated.
	Set(types.UID, *PodStatus, error, time.Time) (updated bool)
	// GetNewerThan is a blocking call that only returns the status
	// when it is newer than the given time.
	GetNewerThan(types.UID, time.Time) (*PodStatus, error)
	Delete(types.UID)
	UpdateTime(time.Time)
}

type data struct {
	// Status of the pod.
	status *PodStatus
	// Error got when trying to inspect the pod.
	err error
	// Time when the data was last modified.
	modified time.Time
}

type subRecord struct {
	time time.Time
	ch   chan *data
}

// cache implements Cache.
type cache struct {
	// Lock which guards all internal data structures.
	lock sync.RWMutex
	// Map that stores the pod statuses.
	pods map[types.UID]*data
	// A global timestamp represents how fresh the cached data is. All
	// cache content is at the least newer than this timestamp. Note that the
	// timestamp is nil after initialization, and will only become non-nil when
	// it is ready to serve the cached statuses.
	timestamp *time.Time
	// Map that stores the subscriber records.
	subscribers map[types.UID][]*subRecord
}

// NewCache creates a pod cache.
func NewCache() Cache {
	return &cache{pods: map[types.UID]*data{}, subscribers: map[types.UID][]*subRecord{}}
}

// Get returns the PodStatus for the pod; callers are expected not to
// modify the objects returned.
func (c *cache) Get(id types.UID) (*PodStatus, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	d := c.get(id)
	return d.status, d.err
}

func (c *cache) GetNewerThan(id types.UID, minTime time.Time) (*PodStatus, error) {
	ch := c.subscribe(id, minTime)
	d := <-ch
	return d.status, d.err
}

// Set sets the PodStatus for the pod only if the data is newer than the cache
func (c *cache) Set(id types.UID, status *PodStatus, err error, timestamp time.Time) (updated bool) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.pods[id] = &data{status: status, err: err, modified: timestamp}
	c.notify(id, timestamp)
	return true
}

// Delete removes the entry of the pod.
func (c *cache) Delete(id types.UID) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.pods, id)
}

// UpdateTime modifies the global timestamp of the cache and notify
// subscribers if needed.
func (c *cache) UpdateTime(timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.timestamp = &timestamp
	// Notify all the subscribers if the condition is met.
	for id := range c.subscribers {
		c.notify(id, *c.timestamp)
	}
}

func makeDefaultData(id types.UID) *data {
	return &data{status: &PodStatus{ID: id}, err: nil}
}

func (c *cache) get(id types.UID) *data {
	d, ok := c.pods[id]
	if !ok {
		// Cache should store *all* pod/container information known by the
		// container runtime. A cache miss indicates that there are no states
		// regarding the pod last time we queried the container runtime.
		// What this *really* means is that there are no visible pod/containers
		// associated with this pod. Simply return an default (mostly empty)
		// PodStatus to reflect this.
		return makeDefaultData(id)
	}
	return d
}

// getIfNewerThan returns the data it is newer than the given time.
// Otherwise, it returns nil. The caller should acquire the lock.
func (c *cache) getIfNewerThan(id types.UID, minTime time.Time) *data {
	d, ok := c.pods[id]
	globalTimestampIsNewer := (c.timestamp != nil && c.timestamp.After(minTime))
	if !ok && globalTimestampIsNewer {
		// Status is not cached, but the global timestamp is newer than
		// minTime, return the default status.
		return makeDefaultData(id)
	}
	if ok && (d.modified.After(minTime) || globalTimestampIsNewer) {
		// Status is cached, return status if either of the following is true.
		//   * status was modified after minTime
		//   * the global timestamp of the cache is newer than minTime.
		return d
	}
	// The pod status is not ready.
	return nil
}

// notify sends notifications for pod with the given id, if the requirements
// are met. Note that the caller should acquire the lock.
func (c *cache) notify(id types.UID, timestamp time.Time) {
	list, ok := c.subscribers[id]
	if !ok {
		// No one to notify.
		return
	}
	newList := []*subRecord{}
	for i, r := range list {
		if timestamp.Before(r.time) {
			// Doesn't meet the time requirement; keep the record.
			newList = append(newList, list[i])
			continue
		}
		r.ch <- c.get(id)
		close(r.ch)
	}
	if len(newList) == 0 {
		delete(c.subscribers, id)
	} else {
		c.subscribers[id] = newList
	}
}

func (c *cache) subscribe(id types.UID, timestamp time.Time) chan *data {
	ch := make(chan *data, 1)
	c.lock.Lock()
	defer c.lock.Unlock()
	d := c.getIfNewerThan(id, timestamp)
	if d != nil {
		// If the cache entry is ready, send the data and return immediately.
		ch <- d
		return ch
	}
	// Add the subscription record.
	c.subscribers[id] = append(c.subscribers[id], &subRecord{time: timestamp, ch: ch})
	return ch
}
