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
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/consistencydetector"
)

// checkWatchListDataConsistencyIfRequested performs a data consistency check only when
// the KUBE_WATCHLIST_INCONSISTENCY_DETECTOR environment variable was set during a binary startup.
//
// The consistency check is meant to be enforced only in the CI, not in production.
// The check ensures that data retrieved by the watch-list api call
// is exactly the same as data received by the standard list api call against etcd.
//
// Note that this function will panic when data inconsistency is detected.
// This is intentional because we want to catch it in the CI.
//
// The identity string can be left empty if the logger in the context already
// identifies what is being watched.
func checkWatchListDataConsistencyIfRequested[T runtime.Object, U any](ctx context.Context, identity string, lastSyncedResourceVersion string, listFn consistencydetector.ListFunc[T], retrieveItemsFn consistencydetector.RetrieveItemsFunc[U]) {
	if !consistencydetector.IsDataConsistencyDetectionForWatchListEnabled() {
		return
	}
	// for informers we pass an empty ListOptions because
	// listFn might be wrapped for filtering during informer construction.
	consistencydetector.CheckDataConsistency(ctx, identity, lastSyncedResourceVersion, listFn, metav1.ListOptions{}, retrieveItemsFn)
}
