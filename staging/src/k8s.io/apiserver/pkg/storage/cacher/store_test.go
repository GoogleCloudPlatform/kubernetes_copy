/*
Copyright 2022 The Kubernetes Authors.

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

package cacher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

func TestStoreSingleKey(t *testing.T) {
	t.Run("cache.Indexer", func(t *testing.T) {
		store := newStoreIndexer(testStoreIndexers())
		testStoreSingleKey(t, store)
	})
	t.Run("btree", func(t *testing.T) {
		store := newThreadedBtreeStoreIndexer(storeElementIndexers(testStoreIndexers()), btreeDegree)
		testStoreSingleKey(t, store)
	})
}

func testStoreSingleKey(t *testing.T, store storeIndexer) {
	assertStoreEmpty(t, store, "foo")

	require.NoError(t, store.Add(testStorageElement("foo", "bar", 1)))
	assertStoreSingleKey(t, store, "foo", "bar", 1)

	require.NoError(t, store.Update(testStorageElement("foo", "baz", 2)))
	assertStoreSingleKey(t, store, "foo", "baz", 2)

	require.NoError(t, store.Update(testStorageElement("foo", "baz", 3)))
	assertStoreSingleKey(t, store, "foo", "baz", 3)

	require.NoError(t, store.Replace([]interface{}{testStorageElement("foo", "bar", 4)}, ""))
	assertStoreSingleKey(t, store, "foo", "bar", 4)

	require.NoError(t, store.Delete(testStorageElement("foo", "", 0)))
	assertStoreEmpty(t, store, "foo")

	require.NoError(t, store.Delete(testStorageElement("foo", "", 0)))
}

func TestStoreIndexerSingleKey(t *testing.T) {
	t.Run("cache.Indexer", func(t *testing.T) {
		store := newStoreIndexer(testStoreIndexers())
		testStoreIndexerSingleKey(t, store)
	})
	t.Run("btree", func(t *testing.T) {
		store := newThreadedBtreeStoreIndexer(storeElementIndexers(testStoreIndexers()), btreeDegree)
		testStoreIndexerSingleKey(t, store)
	})
}

func testStoreIndexerSingleKey(t *testing.T, store storeIndexer) {
	items, err := store.ByIndex("by_val", "bar")
	require.NoError(t, err)
	assert.Empty(t, items)

	require.NoError(t, store.Add(testStorageElement("foo", "bar", 1)))
	items, err = store.ByIndex("by_val", "bar")
	require.NoError(t, err)
	assert.Equal(t, []interface{}{
		testStorageElement("foo", "bar", 1),
	}, items)

	require.NoError(t, store.Update(testStorageElement("foo", "baz", 2)))
	items, err = store.ByIndex("by_val", "bar")
	require.NoError(t, err)
	assert.Empty(t, items)
	items, err = store.ByIndex("by_val", "baz")
	require.NoError(t, err)
	assert.Equal(t, []interface{}{
		testStorageElement("foo", "baz", 2),
	}, items)

	require.NoError(t, store.Update(testStorageElement("foo", "baz", 3)))
	items, err = store.ByIndex("by_val", "bar")
	require.NoError(t, err)
	assert.Empty(t, items)
	items, err = store.ByIndex("by_val", "baz")
	require.NoError(t, err)
	assert.Equal(t, []interface{}{
		testStorageElement("foo", "baz", 3),
	}, items)

	require.NoError(t, store.Replace([]interface{}{
		testStorageElement("foo", "bar", 4),
	}, ""))
	items, err = store.ByIndex("by_val", "bar")
	require.NoError(t, err)
	assert.Equal(t, []interface{}{
		testStorageElement("foo", "bar", 4),
	}, items)
	items, err = store.ByIndex("by_val", "baz")
	require.NoError(t, err)
	assert.Empty(t, items)

	require.NoError(t, store.Delete(testStorageElement("foo", "", 0)))
	items, err = store.ByIndex("by_val", "baz")
	require.NoError(t, err)
	assert.Empty(t, items)

	require.NoError(t, store.Delete(testStorageElement("foo", "", 0)))
}

func assertStoreEmpty(t *testing.T, store storeIndexer, nonExistingKey string) {
	item, ok, err := store.Get(testStorageElement(nonExistingKey, "", 0))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, item)

	item, ok, err = store.GetByKey(nonExistingKey)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, item)

	items := store.List()
	assert.Empty(t, items)
}

func assertStoreSingleKey(t *testing.T, store storeIndexer, expectKey, expectValue string, expectRV int) {
	item, ok, err := store.Get(testStorageElement(expectKey, "", expectRV))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, expectValue, item.(*storeElement).Object.(fakeObj).value)

	item, ok, err = store.GetByKey(expectKey)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, expectValue, item.(*storeElement).Object.(fakeObj).value)

	items := store.List()
	assert.Equal(t, []interface{}{testStorageElement(expectKey, expectValue, expectRV)}, items)
}

func testStorageElement(key, value string, rv int) *storeElement {
	return &storeElement{Key: key, Object: fakeObj{value: value, rv: rv}}
}

type fakeObj struct {
	value string
	rv    int
}

func (f fakeObj) GetObjectKind() schema.ObjectKind { return nil }
func (f fakeObj) DeepCopyObject() runtime.Object   { return nil }

var _ runtime.Object = (*fakeObj)(nil)

func testStoreIndexFunc(obj interface{}) ([]string, error) {
	return []string{obj.(fakeObj).value}, nil
}

func testStoreIndexers() *cache.Indexers {
	indexers := cache.Indexers{}
	indexers["by_val"] = testStoreIndexFunc
	return &indexers
}

func TestStoreSnapshotter(t *testing.T) {
	cache := newStoreSnapshotter()
	cache.Set(20, fakeOrderedLister{rv: 20})
	cache.Set(30, fakeOrderedLister{rv: 30})
	cache.Set(40, fakeOrderedLister{rv: 40})
	assert.Equal(t, 3, cache.snapshots.Len())

	t.Log("No snapshot from before first RV")
	snapshot, found := cache.Get(19)
	assert.False(t, found)

	t.Log("Get snapshot from first RV")
	snapshot, found = cache.Get(20)
	assert.True(t, found)
	assert.Equal(t, 20, snapshot.(fakeOrderedLister).rv)

	t.Log("Get first snapshot by larger RV")
	snapshot, found = cache.Get(21)
	assert.True(t, found)
	assert.Equal(t, 20, snapshot.(fakeOrderedLister).rv)

	t.Log("Get second snapshot by larger RV")
	snapshot, found = cache.Get(32)
	assert.True(t, found)
	assert.Equal(t, 30, snapshot.(fakeOrderedLister).rv)

	t.Log("Get third snapshot for future revision")
	snapshot, found = cache.Get(43)
	assert.True(t, found)
	assert.Equal(t, 40, snapshot.(fakeOrderedLister).rv)

	t.Log("Cleanup removes snapshot up to next RV")
	cache.Clean(20)
	assert.Equal(t, 2, cache.snapshots.Len())
	_, found = cache.Get(20)
	assert.False(t, found)
	_, found = cache.Get(21)
	assert.False(t, found)

	t.Log("Cleanup removing all RVs")
	cache.Clean(40)
	assert.Equal(t, 0, cache.snapshots.Len())
	_, found = cache.Get(40)
	assert.False(t, found)
}

type fakeOrderedLister struct {
	rv int
}

func (f fakeOrderedLister) Add(obj interface{}) error    { return nil }
func (f fakeOrderedLister) Update(obj interface{}) error { return nil }
func (f fakeOrderedLister) Delete(obj interface{}) error { return nil }
func (f fakeOrderedLister) Clone() orderedLister         { return f }
func (f fakeOrderedLister) ListPrefix(prefixKey, continueKey string, limit int) ([]interface{}, bool) {
	return nil, false
}
func (f fakeOrderedLister) Count(prefixKey, continueKey string) int { return 0 }
