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
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheSemantic(t *testing.T) {
	// Cache two entries.
	//
	// Entries are comparable structs with pointers inside. Each
	// compilation leads to different pointers, so the entries can be
	// compared by value to figure out whether an entry was cached or
	// compiled anew.
	cache := NewCache(2)

	// Successful compilations get cached.
	resultTrue := cache.GetOrCompile("true")
	require.Nil(t, resultTrue.Error)
	resultTrueAgain := cache.GetOrCompile("true")
	if resultTrue != resultTrueAgain {
		t.Fatal("result of compiling `true` should have been cached")
	}

	// Unsuccessful ones don't.
	resultFailed := cache.GetOrCompile("no-such-variable")
	require.NotNil(t, resultFailed.Error)
	resultFailedAgain := cache.GetOrCompile("no-such-variable")
	if resultFailed == resultFailedAgain {
		t.Fatal("result of compiling `no-such-variable` should not have been cached")
	}

	// The cache can hold a second result.
	resultFalse := cache.GetOrCompile("false")
	require.Nil(t, resultFalse.Error)
	resultFalseAgain := cache.GetOrCompile("false")
	if resultFalse != resultFalseAgain {
		t.Fatal("result of compiling `false` should have been cached")
	}
	resultTrueAgain = cache.GetOrCompile("true")
	if resultTrue != resultTrueAgain {
		t.Fatal("result of compiling `true` should still have been cached")
	}

	// A third result pushes out the least recently used one.
	resultOther := cache.GetOrCompile("false && true")
	require.Nil(t, resultFalse.Error)
	resultOtherAgain := cache.GetOrCompile("false && true")
	if resultOther != resultOtherAgain {
		t.Fatal("result of compiling `false && true` should have been cached")
	}
	resultFalseAgain = cache.GetOrCompile("false")
	if resultFalse == resultFalseAgain {
		t.Fatal("result of compiling `false` should have been evicted from the cache")
	}

	// Cost estimation must be off (not needed by scheduler).
	if resultFalseAgain.MaxCost != math.MaxUint64 {
		t.Error("cost estimation should have been disabled, was enabled")
	}
}

func TestCacheConcurrency(t *testing.T) {
	// There's no guarantee that concurrent use of the cache would really
	// trigger the race detector in `go test -race`, but in practice
	// it does when not using the cacheMutex.
	//
	// The compileMutex ony affects performance and thus cannot be tested
	// without benchmarking.
	numWorkers := 10

	cache := NewCache(2)
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func(i int) {
			defer wg.Done()
			result := cache.GetOrCompile(fmt.Sprintf("%d == %d", i, i))
			assert.Nil(t, result.Error)
		}(i)
	}
	wg.Wait()
}
