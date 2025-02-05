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

package flowcontrol

import (
	"net/http"
	"sync"

	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"

	"k8s.io/klog/v2"
)

// readOnlyVerbs contains verbs for read-only requests.
var readOnlyVerbs = sets.NewString("get", "list", "watch", "proxy")

// watchIdentifier identifies group of watches that are similar.
// As described in the "Priority and Fairness" KEP, we consider
// watches similar if they have the same resourceType, namespace
// and name. We ignore selectors as they have to be evaluated
// when processing an even anyway.
//
// TODO: For now we only track the number of watches registered
// in our kube-apiserver. Eventually we should consider sharing
// this information with other kube-apiserver as described in the
// KEP, but this isn't part of the first version.
type watchIdentifier struct {
	apiGroup  string
	resource  string
	namespace string
	name      string
}

// ForgetWatchFunc is a function that should be called to forget
// the previously registered watch from the watch tracker.
type ForgetWatchFunc func()

// WatchTracker is an interface that allows tracking the number
// of watches in the system for the purpose of estimating the
// cost of incoming mutating requests.
type WatchTracker interface {
	// RegisterWatch registers a watch based on the provided http.Request
	// in the tracker. It returns the function that should be called
	// to forget the watcher once it is finished.
	RegisterWatch(r *http.Request) ForgetWatchFunc

	// GetInterestedWatchCount returns the number of watches that are
	// potentially interested in a request with a given RequestInfo
	// for the purpose of estimating cost of that request.
	GetInterestedWatchCount(requestInfo *request.RequestInfo) int
}

// builtinIndexes represents of set of indexes registered in
// watchcache that are indexing watches and increase speed of
// their processing.
// We define the indexes as a map from a resource to the path
// to the field in the object on which the index is built.
type builtinIndexes map[string]string

func getBuiltinIndexes() builtinIndexes {
	// The only existing indexes as of now are:
	// - spec.nodeName for pods
	// - metadata.Name for nodes, secrets and configmaps
	// However, we can ignore the latter, because the requestInfo.Name
	// is set for them (i.e. we already catch them correctly).
	return map[string]string{
		"pods": "spec.nodeName",
	}
}

// watchTracker tracks the number of watches in the system for
// the purpose of estimating the cost of incoming mutating requests.
type watchTracker struct {
	// indexes represents a set of registered indexes.
	// It can't change after creation.
	indexes builtinIndexes

	lock       sync.Mutex
	watchCount map[watchIdentifier]int
}

func NewWatchTracker() WatchTracker {
	return &watchTracker{
		indexes:    getBuiltinIndexes(),
		watchCount: make(map[watchIdentifier]int),
	}
}

const (
	unsetValue = "<unset>"
)

func getIndexValue(r *http.Request, field string) string {
	opts := metainternalversion.ListOptions{}
	if err := scheme.ParameterCodec.DecodeParameters(r.URL.Query(), metav1.SchemeGroupVersion, &opts); err != nil {
		klog.Warningf("Couldn't parse list options for %v: %v", r.URL.Query(), err)
		return unsetValue
	}
	if opts.FieldSelector == nil {
		return unsetValue
	}
	if value, ok := opts.FieldSelector.RequiresExactMatch(field); ok {
		return value
	}
	return unsetValue
}

type indexValue struct {
	resource string
	value    string
}

// RegisterWatch implements WatchTracker interface.
func (w *watchTracker) RegisterWatch(r *http.Request) ForgetWatchFunc {
	requestInfo, ok := request.RequestInfoFrom(r.Context())
	if !ok || requestInfo == nil || requestInfo.Verb != "watch" {
		return nil
	}

	var index *indexValue
	if indexField, ok := w.indexes[requestInfo.Resource]; ok {
		index = &indexValue{
			resource: requestInfo.Resource,
			value:    getIndexValue(r, indexField),
		}
	}

	identifier := &watchIdentifier{
		apiGroup:  requestInfo.APIGroup,
		resource:  requestInfo.Resource,
		namespace: requestInfo.Namespace,
		name:      requestInfo.Name,
	}

	w.lock.Lock()
	defer w.lock.Unlock()
	w.updateIndexLocked(identifier, index, 1)
	return w.forgetWatch(identifier, index)
}

func (w *watchTracker) updateIndexLocked(identifier *watchIdentifier, index *indexValue, incr int) {
	if index == nil {
		w.watchCount[*identifier] += incr
	} else {
		// For resources with defined index, for a given watch event we are
		// only processing the watchers that:
		// (a) do not specify field selector for an index field
		// (b) do specify field selector with the value equal to the value
		//     coming from the processed object
		//
		// TODO(wojtek-t): For the sake of making progress and initially
		// simplifying the implementation, we approximate (b) for all values
		// as the value for an empty string. The assumption we're making here
		// is that the difference between the actual number of watchers that
		// will be processed, i.e. (a)+(b) above and the one from our
		// approximation i.e. (a)+[(b) for field value of ""] will be small.
		// This seem to be true in almost all production clusters, which makes
		// it a reasonable first step simplification to unblock progres on it.
		if index.value == unsetValue || index.value == "" {
			w.watchCount[*identifier] += incr
		}
	}
}

func (w *watchTracker) forgetWatch(identifier *watchIdentifier, index *indexValue) ForgetWatchFunc {
	return func() {
		w.lock.Lock()
		defer w.lock.Unlock()

		w.updateIndexLocked(identifier, index, -1)
		if w.watchCount[*identifier] == 0 {
			delete(w.watchCount, *identifier)
		}
	}
}

// GetInterestedWatchCount implements WatchTracker interface.
//
// TODO(wojtek-t): As of now, requestInfo for object creation (POST) doesn't
// contain the Name field set. Figure out if we can somehow get it for the
// more accurate cost estimation.
//
// TODO(wojtek-t): Figure out how to approach DELETECOLLECTION calls.
func (w *watchTracker) GetInterestedWatchCount(requestInfo *request.RequestInfo) int {
	if requestInfo == nil || readOnlyVerbs.Has(requestInfo.Verb) {
		return 0
	}

	result := 0
	// The watches that we're interested in include:
	// - watches for all objects of a resource type (no namespace and name specified)
	// - watches for all objects of a resource type in the same namespace (no name specified)
	// - watched interested in this particular object
	identifier := &watchIdentifier{
		apiGroup: requestInfo.APIGroup,
		resource: requestInfo.Resource,
	}

	w.lock.Lock()
	defer w.lock.Unlock()

	result += w.watchCount[*identifier]

	if requestInfo.Namespace != "" {
		identifier.namespace = requestInfo.Namespace
		result += w.watchCount[*identifier]
	}

	if requestInfo.Name != "" {
		identifier.name = requestInfo.Name
		result += w.watchCount[*identifier]
	}

	return result
}
