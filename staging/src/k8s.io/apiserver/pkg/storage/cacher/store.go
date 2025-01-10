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

package cacher

import (
	"fmt"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
)

const (
	// btreeDegree defines the degree of btree storage.
	// Decided based on the benchmark results (below).
	// Selected the lowest degree from three options with best runtime (16,32,128).
	//                                                     │      2       │                 4                  │                 8                  │                 16                  │                 32                  │                 64                  │                 128                 │
	//                                                     │    sec/op    │   sec/op     vs base               │   sec/op     vs base               │   sec/op     vs base                │   sec/op     vs base                │   sec/op     vs base                │   sec/op     vs base                │
	// StoreCreateList/RV=NotOlderThan-24                    473.0µ ± 11%   430.1µ ± 9%  -9.08% (p=0.005 n=10)   427.9µ ± 6%  -9.54% (p=0.002 n=10)   403.9µ ± 8%  -14.62% (p=0.000 n=10)   401.0µ ± 4%  -15.22% (p=0.000 n=10)   408.0µ ± 4%  -13.75% (p=0.000 n=10)   385.9µ ± 4%  -18.42% (p=0.000 n=10)
	// StoreCreateList/RV=ExactMatch-24                      604.7µ ±  4%   596.7µ ± 8%       ~ (p=0.529 n=10)   604.6µ ± 4%       ~ (p=0.971 n=10)   601.1µ ± 4%        ~ (p=0.853 n=10)   611.0µ ± 6%        ~ (p=0.105 n=10)   598.2µ ± 5%        ~ (p=0.579 n=10)   608.2µ ± 3%        ~ (p=0.796 n=10)
	// StoreList/List=All/Paginate=False/RV=Empty-24         729.1µ ±  5%   692.9µ ± 3%  -4.96% (p=0.002 n=10)   693.7µ ± 3%  -4.86% (p=0.000 n=10)   688.3µ ± 1%   -5.59% (p=0.000 n=10)   690.4µ ± 5%   -5.31% (p=0.002 n=10)   689.7µ ± 2%   -5.40% (p=0.000 n=10)   687.8µ ± 3%   -5.67% (p=0.000 n=10)
	// StoreList/List=All/Paginate=True/RV=Empty-24          19.51m ±  2%   19.84m ± 2%       ~ (p=0.105 n=10)   19.89m ± 3%       ~ (p=0.190 n=10)   19.64m ± 4%        ~ (p=0.853 n=10)   19.34m ± 4%        ~ (p=0.481 n=10)   20.22m ± 4%   +3.66% (p=0.007 n=10)   19.58m ± 4%        ~ (p=0.912 n=10)
	// StoreList/List=Namespace/Paginate=False/RV=Empty-24   1.672m ±  4%   1.635m ± 2%       ~ (p=0.247 n=10)   1.673m ± 5%       ~ (p=0.631 n=10)   1.657m ± 2%        ~ (p=0.971 n=10)   1.656m ± 4%        ~ (p=0.739 n=10)   1.678m ± 2%        ~ (p=0.631 n=10)   1.718m ± 8%        ~ (p=0.105 n=10)
	// geomean                                               1.467m         1.420m       -3.24%                  1.430m       -2.58%                  1.403m        -4.38%                  1.402m        -4.46%                  1.417m        -3.44%                  1.403m        -4.41%
	//
	//                                                     │       2       │                   4                   │                  8                   │                  16                  │                  32                   │                   64                   │                  128                   │
	//                                                     │     B/op      │      B/op       vs base               │     B/op       vs base               │     B/op       vs base               │     B/op       vs base                │      B/op       vs base                │      B/op       vs base                │
	// StoreCreateList/RV=NotOlderThan-24                    98.58Ki ± 11%   101.33Ki ± 13%       ~ (p=0.280 n=10)   99.80Ki ± 26%       ~ (p=0.353 n=10)   109.63Ki ± 9%       ~ (p=0.075 n=10)   112.56Ki ± 6%  +14.18% (p=0.007 n=10)   114.41Ki ± 10%  +16.05% (p=0.003 n=10)   115.06Ki ± 12%  +16.72% (p=0.011 n=10)
	// StoreCreateList/RV=ExactMatch-24                      117.1Ki ±  0%    117.5Ki ±  0%       ~ (p=0.218 n=10)   116.9Ki ±  0%       ~ (p=0.052 n=10)    117.3Ki ± 0%       ~ (p=0.353 n=10)    116.9Ki ± 0%        ~ (p=0.075 n=10)    117.0Ki ±  0%        ~ (p=0.436 n=10)    117.0Ki ±  0%        ~ (p=0.280 n=10)
	// StoreList/List=All/Paginate=False/RV=Empty-24         6.023Mi ±  0%    6.024Mi ±  0%  +0.01% (p=0.037 n=10)   6.024Mi ±  0%       ~ (p=0.493 n=10)    6.024Mi ± 0%  +0.01% (p=0.035 n=10)    6.024Mi ± 0%        ~ (p=0.247 n=10)    6.024Mi ±  0%        ~ (p=0.247 n=10)    6.024Mi ±  0%        ~ (p=0.315 n=10)
	// StoreList/List=All/Paginate=True/RV=Empty-24          64.22Mi ±  0%    64.21Mi ±  0%       ~ (p=0.075 n=10)   64.23Mi ±  0%       ~ (p=0.280 n=10)    64.21Mi ± 0%  -0.02% (p=0.002 n=10)    64.22Mi ± 0%        ~ (p=0.579 n=10)    64.22Mi ±  0%        ~ (p=0.971 n=10)    64.22Mi ±  0%        ~ (p=1.000 n=10)
	// StoreList/List=Namespace/Paginate=False/RV=Empty-24   8.177Mi ±  0%    8.178Mi ±  0%       ~ (p=0.579 n=10)   8.177Mi ±  0%       ~ (p=0.971 n=10)    8.179Mi ± 0%       ~ (p=0.579 n=10)    8.178Mi ± 0%        ~ (p=0.739 n=10)    8.179Mi ±  0%        ~ (p=0.315 n=10)    8.176Mi ±  0%        ~ (p=0.247 n=10)
	// geomean                                               2.034Mi          2.047Mi        +0.61%                  2.039Mi        +0.22%                   2.079Mi       +2.19%                   2.088Mi        +2.66%                   2.095Mi         +3.01%                   2.098Mi         +3.12%
	//
	//                                                     │      2      │                 4                  │                 8                  │                 16                 │                 32                 │                 64                 │                128                 │
	//                                                     │  allocs/op  │  allocs/op   vs base               │  allocs/op   vs base               │  allocs/op   vs base               │  allocs/op   vs base               │  allocs/op   vs base               │  allocs/op   vs base               │
	// StoreCreateList/RV=NotOlderThan-24                     560.0 ± 0%    558.0 ± 0%  -0.36% (p=0.000 n=10)    557.0 ± 0%  -0.54% (p=0.000 n=10)    558.0 ± 0%  -0.36% (p=0.000 n=10)    557.0 ± 0%  -0.54% (p=0.000 n=10)    557.0 ± 0%  -0.54% (p=0.000 n=10)    557.0 ± 0%  -0.54% (p=0.000 n=10)
	// StoreCreateList/RV=ExactMatch-24                       871.0 ± 0%    870.0 ± 0%  -0.11% (p=0.038 n=10)    870.0 ± 0%  -0.11% (p=0.004 n=10)    870.0 ± 0%  -0.11% (p=0.005 n=10)    869.0 ± 0%  -0.23% (p=0.000 n=10)    870.0 ± 0%  -0.11% (p=0.001 n=10)    870.0 ± 0%  -0.11% (p=0.000 n=10)
	// StoreList/List=All/Paginate=False/RV=Empty-24          351.0 ± 3%    358.0 ± 1%  +1.99% (p=0.034 n=10)    352.5 ± 3%       ~ (p=0.589 n=10)    358.5 ± 1%  +2.14% (p=0.022 n=10)    356.5 ± 3%       ~ (p=0.208 n=10)    355.0 ± 3%       ~ (p=0.224 n=10)    355.0 ± 3%       ~ (p=0.183 n=10)
	// StoreList/List=All/Paginate=True/RV=Empty-24          494.4k ± 0%   494.4k ± 0%       ~ (p=0.424 n=10)   494.6k ± 0%  +0.06% (p=0.000 n=10)   492.7k ± 0%  -0.34% (p=0.000 n=10)   494.5k ± 0%  +0.02% (p=0.009 n=10)   493.0k ± 0%  -0.28% (p=0.000 n=10)   494.4k ± 0%       ~ (p=0.424 n=10)
	// StoreList/List=Namespace/Paginate=False/RV=Empty-24   32.43k ± 0%   32.44k ± 0%       ~ (p=0.579 n=10)   32.43k ± 0%       ~ (p=0.971 n=10)   32.45k ± 0%       ~ (p=0.517 n=10)   32.44k ± 0%       ~ (p=0.670 n=10)   32.46k ± 0%       ~ (p=0.256 n=10)   32.41k ± 0%       ~ (p=0.247 n=10)
	// geomean                                               4.872k        4.887k       +0.31%                  4.870k       -0.03%                  4.885k       +0.28%                  4.880k       +0.17%                  4.875k       +0.06%                  4.876k       +0.08%
	btreeDegree = 16
)

type storeIndexer interface {
	Add(obj interface{}) error
	Update(obj interface{}) error
	Delete(obj interface{}) error
	List() []interface{}
	ListKeys() []string
	Get(obj interface{}) (item interface{}, exists bool, err error)
	GetByKey(key string) (item interface{}, exists bool, err error)
	Replace([]interface{}, string) error
	ByIndex(indexName, indexedValue string) ([]interface{}, error)
}

// orderedLister is interface wrapping btree implementation of store, allowing for fast prefix listing and fast lazy cloning.
type orderedLister interface {
	Count(prefix, continueKey string) (count int)
	ListPrefix(prefix, continueKey string, limit int) (items []interface{}, hasMore bool)
	Clone() orderedLister
}

func newStoreIndexer(indexers *cache.Indexers) storeIndexer {
	if utilfeature.DefaultFeatureGate.Enabled(features.BtreeWatchCache) {
		return newThreadedBtreeStoreIndexer(storeElementIndexers(indexers), btreeDegree)
	}
	return cache.NewIndexer(storeElementKey, storeElementIndexers(indexers))
}

// Computing a key of an object is generally non-trivial (it performs
// e.g. validation underneath). Similarly computing object fields and
// labels. To avoid computing them multiple times (to serve the event
// in different List/Watch requests), in the underlying store we are
// keeping structs (key, object, labels, fields).
type storeElement struct {
	Key    string
	Object runtime.Object
	Labels labels.Set
	Fields fields.Set
}

func storeElementKey(obj interface{}) (string, error) {
	elem, ok := obj.(*storeElement)
	if !ok {
		return "", fmt.Errorf("not a storeElement: %v", obj)
	}
	return elem.Key, nil
}

func storeElementObject(obj interface{}) (runtime.Object, error) {
	elem, ok := obj.(*storeElement)
	if !ok {
		return nil, fmt.Errorf("not a storeElement: %v", obj)
	}
	return elem.Object, nil
}

func storeElementIndexFunc(objIndexFunc cache.IndexFunc) cache.IndexFunc {
	return func(obj interface{}) (strings []string, e error) {
		seo, err := storeElementObject(obj)
		if err != nil {
			return nil, err
		}
		return objIndexFunc(seo)
	}
}

func storeElementIndexers(indexers *cache.Indexers) cache.Indexers {
	if indexers == nil {
		return cache.Indexers{}
	}
	ret := cache.Indexers{}
	for indexName, indexFunc := range *indexers {
		ret[indexName] = storeElementIndexFunc(indexFunc)
	}
	return ret
}
