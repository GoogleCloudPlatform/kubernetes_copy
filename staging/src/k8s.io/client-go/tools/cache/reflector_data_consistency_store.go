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

package cache

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
)

type consistencyStore struct {
	destinationStore Store
	backingStore     Store

	lock sync.Mutex
}

var _ Store = &consistencyStore{}

func newConsistencyStore(destinationStore, backingStore Store) *consistencyStore {
	return &consistencyStore{destinationStore: destinationStore, backingStore: backingStore}
}

func (c *consistencyStore) Add(obj interface{}) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	runtimeObject, ok := obj.(runtime.Object)
	if !ok {
		panic(fmt.Sprintf("obj = %T doesn't not implement runtime.Object", obj))
	}
	copiedObj := runtimeObject.DeepCopyObject()

	if err := c.destinationStore.Add(obj); err != nil {
		return err
	}
	return c.backingStore.Add(copiedObj)
}

func (c *consistencyStore) Update(obj interface{}) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	runtimeObject, ok := obj.(runtime.Object)
	if !ok {
		panic(fmt.Sprintf("obj = %T doesn't not implement runtime.Object", obj))
	}
	copiedObj := runtimeObject.DeepCopyObject()

	if err := c.destinationStore.Update(obj); err != nil {
		return err
	}
	return c.backingStore.Update(copiedObj)
}

func (c *consistencyStore) Delete(obj interface{}) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	runtimeObject, ok := obj.(runtime.Object)
	if !ok {
		panic(fmt.Sprintf("obj = %T doesn't not implement runtime.Object", obj))
	}
	copiedObj := runtimeObject.DeepCopyObject()

	if err := c.destinationStore.Delete(obj); err != nil {
		return err
	}
	return c.backingStore.Delete(copiedObj)
}

func (c *consistencyStore) List() []interface{} {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.destinationStore.List()
}

func (c *consistencyStore) ListKeys() []string {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.destinationStore.ListKeys()
}

func (c *consistencyStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.destinationStore.Get(obj)
}

func (c *consistencyStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.destinationStore.GetByKey(key)
}

func (c *consistencyStore) Replace(items []interface{}, rv string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	var itemsCopy []interface{}
	for _, item := range items {
		runtimeObject, ok := item.(runtime.Object)
		if !ok {
			panic(fmt.Sprintf("obj = %T doesn't not implement runtime.Object", item))
		}
		itemsCopy = append(itemsCopy, runtimeObject.DeepCopyObject())
	}

	if err := c.destinationStore.Replace(items, rv); err != nil {
		return err
	}
	return c.backingStore.Replace(itemsCopy, rv)
}

func (c *consistencyStore) Resync() error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.destinationStore.Resync()
}

func (c *consistencyStore) UpdateResourceVersion(resourceVersion string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if rvu, ok := c.destinationStore.(ResourceVersionUpdater); ok {
		rvu.UpdateResourceVersion(resourceVersion)
	}
	if rvu, ok := c.backingStore.(ResourceVersionUpdater); ok {
		rvu.UpdateResourceVersion(resourceVersion)
	}
}
