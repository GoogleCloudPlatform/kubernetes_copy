/*
Copyright 2021 The Kubernetes Authors.

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

package request

import (
	"math"
	"net/http"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	etcdfeature "k8s.io/apiserver/pkg/storage/feature"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
)

func newListWorkEstimator(countFn objectCountGetterFunc, config *WorkEstimatorConfig, maxSeatsFn maxSeatsFunc) WorkEstimatorFunc {
	estimator := &listWorkEstimator{
		config:        config,
		countGetterFn: countFn,
		maxSeatsFn:    maxSeatsFn,
	}
	return estimator.estimate
}

type listWorkEstimator struct {
	config        *WorkEstimatorConfig
	countGetterFn objectCountGetterFunc
	maxSeatsFn    maxSeatsFunc
}

func (e *listWorkEstimator) estimate(r *http.Request, flowSchemaName, priorityLevelName string) WorkEstimate {
	minSeats := e.config.MinimumSeats
	maxSeats := e.maxSeatsFn(priorityLevelName)
	if maxSeats == 0 || maxSeats > e.config.MaximumSeatsLimit {
		maxSeats = e.config.MaximumSeatsLimit
	}

	requestInfo, ok := apirequest.RequestInfoFrom(r.Context())
	if !ok {
		// no RequestInfo should never happen, but to be on the safe side
		// let's return maximumSeats
		return WorkEstimate{InitialSeats: maxSeats}
	}

	if requestInfo.Name != "" {
		// Requests with metadata.name specified are usually executed as get
		// requests in storage layer so their width should be 1.
		// Example of such list requests:
		// /apis/certificates.k8s.io/v1/certificatesigningrequests?fieldSelector=metadata.name%3Dcsr-xxs4m
		// /api/v1/namespaces/test/configmaps?fieldSelector=metadata.name%3Dbig-deployment-1&limit=500&resourceVersion=0
		return WorkEstimate{InitialSeats: minSeats}
	}

	query := r.URL.Query()
	listOptions := metav1.ListOptions{}
	if err := metav1.Convert_url_Values_To_v1_ListOptions(&query, &listOptions, nil); err != nil {
		klog.ErrorS(err, "Failed to convert options while estimating work for the list request")

		// This request is destined to fail in the validation layer,
		// return maximumSeats for this request to be consistent.
		return WorkEstimate{InitialSeats: maxSeats}
	}

	// For watch requests, we want to adjust the cost only if they explicitly request
	// sending initial events.
	if requestInfo.Verb == "watch" {
		if listOptions.SendInitialEvents == nil || !*listOptions.SendInitialEvents {
			return WorkEstimate{InitialSeats: e.config.MinimumSeats}
		}
	}

	isListFromCache := requestInfo.Verb == "watch" || !shouldListFromStorage(query, &listOptions)

	numStored, err := e.countGetterFn(key(requestInfo))
	switch {
	case err == ObjectCountStaleErr:
		// object count going stale is indicative of degradation, so we should
		// be conservative here and allocate maximum seats to this list request.
		// NOTE: if a CRD is removed, its count will go stale first and then the
		// pruner will eventually remove the CRD from the cache.
		return WorkEstimate{InitialSeats: maxSeats}
	case err == ObjectCountNotFoundErr:
		// there are multiple scenarios in which we can see this error:
		//  a. the type is truly unknown, a typo on the caller's part.
		//  b. the count has gone stale for too long and the pruner
		//     has removed the type from the cache.
		//  c. the type is an aggregated resource that is served by a
		//     different apiserver (thus its object count is not updated)
		// we don't have a way to distinguish between those situations.
		// However, in case c, the request is delegated to a different apiserver,
		// and thus its cost for our server is minimal. To avoid the situation
		// when aggregated API calls are overestimated, we allocate the minimum
		// possible seats (see #109106 as an example when being more conservative
		// led to problems).
		return WorkEstimate{InitialSeats: minSeats}
	case err != nil:
		// we should never be here since Get returns either ObjectCountStaleErr or
		// ObjectCountNotFoundErr, return maximumSeats to be on the safe side.
		klog.ErrorS(err, "Unexpected error from object count tracker")
		return WorkEstimate{InitialSeats: maxSeats}
	}

	limit := numStored
	if listOptions.Limit > 0 && listOptions.Limit < numStored {
		limit = listOptions.Limit
	}

	var estimatedObjectsToBeProcessed int64

	switch {
	case isListFromCache:
		// TODO: For resources that implement indexes at the watchcache level,
		//  we need to adjust the cost accordingly
		estimatedObjectsToBeProcessed = numStored
	case listOptions.FieldSelector != "" || listOptions.LabelSelector != "":
		estimatedObjectsToBeProcessed = numStored + limit
	default:
		estimatedObjectsToBeProcessed = 2 * limit
	}

	// for now, our rough estimate is to allocate one seat to each 100 obejcts that
	// will be processed by the list request.
	// we will come up with a different formula for the transformation function and/or
	// fine tune this number in future iteratons.
	seats := uint64(math.Ceil(float64(estimatedObjectsToBeProcessed) / e.config.ObjectsPerSeat))

	// make sure we never return a seat of zero
	if seats < minSeats {
		seats = minSeats
	}
	if seats > maxSeats {
		seats = maxSeats
	}
	return WorkEstimate{InitialSeats: seats}
}

func key(requestInfo *apirequest.RequestInfo) string {
	groupResource := &schema.GroupResource{
		Group:    requestInfo.APIGroup,
		Resource: requestInfo.Resource,
	}
	return groupResource.String()
}

// NOTICE: Keep in sync with shouldDelegateList function in
//
//	staging/src/k8s.io/apiserver/pkg/storage/cacher/cacher.go
func shouldListFromStorage(query url.Values, opts *metav1.ListOptions) bool {
	// see https://kubernetes.io/docs/reference/using-api/api-concepts/#semantics-for-get-and-list
	switch opts.ResourceVersionMatch {
	case metav1.ResourceVersionMatchExact:
		return true
	case metav1.ResourceVersionMatchNotOlderThan:
	case "":
		// Legacy exact match
		if opts.Limit > 0 && len(opts.ResourceVersion) > 0 && opts.ResourceVersion != "0" {
			return true
		}
	default:
		return true
	}
	// Continue
	if len(opts.Continue) > 0 {
		return true
	}
	// Consistent Read
	if opts.ResourceVersion == "" {
		consistentListFromCacheEnabled := utilfeature.DefaultFeatureGate.Enabled(features.ConsistentListFromCache)
		requestWatchProgressSupported := etcdfeature.DefaultFeatureSupportChecker.Supports(storage.RequestWatchProgress)
		return !consistentListFromCacheEnabled || !requestWatchProgressSupported
	}
	return false
}
