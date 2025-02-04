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

package cel

import (
	"sync"

	"k8s.io/utils/keymutex"
	"k8s.io/utils/lru"
)

// Cache is a thread-safe LRU cache for a compiled CEL expression.
type Cache struct {
	compileMutex keymutex.KeyMutex
	cacheMutex   sync.RWMutex
	cache        *lru.Cache
}

// NewCache creates a cache. The maximum number of entries determines
// how many entries are cached at most before dropping the oldest
// entry.
func NewCache(maxCacheEntries int) *Cache {
	return &Cache{
		compileMutex: keymutex.NewHashed(0),
		cache:        lru.New(maxCacheEntries),
	}
}

// GetOrCompile checks whether the cache already has a compilation result
// and returns that if available. Otherwise it compiles, stores successful
// results and returns the new result.
//
// Cost estimation is disabled.
func (c *Cache) GetOrCompile(expression string) CompilationResult {
	// Compiling a CEL expression is expensive enough that it is cheaper
	// to lock a mutex than doing it several times in parallel.
	c.compileMutex.LockKey(expression)
	//nolint:errcheck // Only returns an error for unknown keys, which isn't the case here.
	defer c.compileMutex.UnlockKey(expression)

	cached := c.get(expression)
	if cached != nil {
		return *cached
	}

	expr := GetCompiler().CompileCELExpression(expression, Options{DisableCostEstimation: true})
	if expr.Error == nil {
		c.add(expression, &expr)
	}
	return expr
}

func (c *Cache) add(expression string, expr *CompilationResult) {
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()
	c.cache.Add(expression, expr)
}

func (c *Cache) get(expression string) *CompilationResult {
	c.cacheMutex.RLock()
	defer c.cacheMutex.RUnlock()
	expr, found := c.cache.Get(expression)
	if !found {
		return nil
	}
	return expr.(*CompilationResult)
}
