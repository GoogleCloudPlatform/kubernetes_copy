/*
Copyright 2023 The Kubernetes Authors.

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

package consistencydetector

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/go-cmp/cmp" //nolint:depguard

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

type RetrieveItemsFunc[U any] func() []U

type ListFunc[T runtime.Object] func(ctx context.Context, options metav1.ListOptions) (T, error)

// CheckDataConsistency exists solely for testing purposes.
// we cannot use checkWatchListDataConsistencyIfRequested because
// it is guarded by an environmental variable.
// we cannot manipulate the environmental variable because
// it will affect other tests in this package.
//
// The identity string can be left empty if the logger in the context already
// identifies what is being watched.
func CheckDataConsistency[T runtime.Object, U any](ctx context.Context, identity string, lastSyncedResourceVersion string, listFn ListFunc[T], listOptions metav1.ListOptions, retrieveItemsFn RetrieveItemsFunc[U]) {
	logger := klog.FromContext(ctx)
	if identity != "" {
		logger = klog.LoggerWithName(logger, identity)
	}
	if !canFormAdditionalListCall(lastSyncedResourceVersion, listOptions) {
		logger.V(4).Info("Data consistency check is enabled but the parameters (RV, ListOptions) doesn't allow for creating a valid LIST request. Skipping the data consistency check.")
		return
	}
	logger.Info("Warning: data consistency check is enabled, this will result in an additional call to the API server.")

	retrievedItems := toMetaObjectSliceOrDie(retrieveItemsFn())
	listOptions = prepareListCallOptions(lastSyncedResourceVersion, listOptions, len(retrievedItems))
	var list runtime.Object
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(_ context.Context) (done bool, err error) {
		list, err = listFn(ctx, listOptions)
		if err != nil {
			// the consistency check will only be enabled in the CI
			// and LIST calls in general will be retired by the client-go library
			// if we fail simply log and retry
			logger.Info("Failed to list data from the server, retrying until context is canceled", "err", err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		logger.Error(err, "Failed to list data from the server, the data consistency check won't be performed, stopCh was closed")
		return
	}

	rawListItems, err := meta.ExtractListWithAlloc(list)
	if err != nil {
		panic(err) // this should never happen
	}
	listItems := toMetaObjectSliceOrDie(rawListItems)

	sort.Sort(byUID(listItems))
	sort.Sort(byUID(retrievedItems))

	if !cmp.Equal(listItems, retrievedItems) {
		logger.Info("Previously received data is different than received by the standard list api call against etcd", "diff", cmp.Diff(listItems, retrievedItems))
		if identity != "" {
			msg := fmt.Sprintf("data inconsistency detected for %s, panicking!", identity)
			panic(msg)
		} else {
			panic("data inconsistency detected, panicking!")
		}
	}
}

// canFormAdditionalListCall ensures that we can form a valid LIST requests
// for checking data consistency.
func canFormAdditionalListCall(lastSyncedResourceVersion string, listOptions metav1.ListOptions) bool {
	// since we are setting ResourceVersionMatch to metav1.ResourceVersionMatchExact
	// we need to make sure that the continuation hasn't been set
	// https://github.com/kubernetes/kubernetes/blob/be4afb9ef90b19ccb6f7e595cbdb247e088b2347/staging/src/k8s.io/apimachinery/pkg/apis/meta/internalversion/validation/validation.go#L38
	if len(listOptions.Continue) > 0 {
		return false
	}

	// since we are setting ResourceVersionMatch to metav1.ResourceVersionMatchExact
	// we need to make sure that the RV is valid because the validation code forbids RV == "0"
	// https://github.com/kubernetes/kubernetes/blob/be4afb9ef90b19ccb6f7e595cbdb247e088b2347/staging/src/k8s.io/apimachinery/pkg/apis/meta/internalversion/validation/validation.go#L44
	if lastSyncedResourceVersion == "0" {
		return false
	}

	return true
}

// prepareListCallOptions changes the input list options so that
// the list call goes directly to etcd
func prepareListCallOptions(lastSyncedResourceVersion string, listOptions metav1.ListOptions, retrievedItemsCount int) metav1.ListOptions {
	// this is our legacy case:
	//
	// the watch cache skips the Limit if the ResourceVersion was set to "0"
	// thus, to compare with data retrieved directly from etcd
	// we need to skip the limit to for the list call as well.
	//
	// note that when the number of retrieved items is less than the request limit,
	// it means either the watch cache is disabled, or there is not enough data.
	// in both cases, we can use the limit because we will be able to compare
	// the data with the items retrieved from etcd.
	if listOptions.ResourceVersion == "0" && listOptions.Limit > 0 && int64(retrievedItemsCount) > listOptions.Limit {
		listOptions.Limit = 0
	}

	// set the RV and RVM so that we get the snapshot of data
	// directly from etcd.
	listOptions.ResourceVersion = lastSyncedResourceVersion
	listOptions.ResourceVersionMatch = metav1.ResourceVersionMatchExact

	return listOptions
}

type byUID []metav1.Object

func (a byUID) Len() int           { return len(a) }
func (a byUID) Less(i, j int) bool { return a[i].GetUID() < a[j].GetUID() }
func (a byUID) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

func toMetaObjectSliceOrDie[T any](s []T) []metav1.Object {
	result := make([]metav1.Object, len(s))
	for i, v := range s {
		m, err := meta.Accessor(v)
		if err != nil {
			panic(err)
		}
		result[i] = m
	}
	return result
}
