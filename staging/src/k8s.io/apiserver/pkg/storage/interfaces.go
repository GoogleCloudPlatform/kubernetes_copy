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

package storage

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// Feature is the name of each feature in storage that we check in feature_support_checker.
type Feature = string

// RequestWatchProgress is an etcd feature that may use to check if it supported or not.
var RequestWatchProgress Feature = "RequestWatchProgress"

// Versioner abstracts setting and retrieving metadata fields from database response
// onto the object ot list. It is required to maintain storage invariants - updating an
// object twice with the same data except for the ResourceVersion and SelfLink must be
// a no-op. A resourceVersion of type uint64 is a 'raw' resourceVersion,
// intended to be sent directly to or from the backend. A resourceVersion of
// type string is a 'safe' resourceVersion, intended for consumption by users.
type Versioner interface {
	// UpdateObject sets storage metadata into an API object. Returns an error if the object
	// cannot be updated correctly. May return nil if the requested object does not need metadata
	// from database.
	UpdateObject(obj runtime.Object, resourceVersion uint64) error
	// UpdateList sets the resource version into an API list object. Returns an error if the object
	// cannot be updated correctly. May return nil if the requested object does not need metadata from
	// database. continueValue is optional and indicates that more results are available if the client
	// passes that value to the server in a subsequent call. remainingItemCount indicates the number
	// of remaining objects if the list is partial. The remainingItemCount field is omitted during
	// serialization if it is set to nil.
	UpdateList(obj runtime.Object, resourceVersion uint64, continueValue string, remainingItemCount *int64) error
	// PrepareObjectForStorage should set SelfLink and ResourceVersion to the empty value. Should
	// return an error if the specified object cannot be updated.
	PrepareObjectForStorage(obj runtime.Object) error
	// ObjectResourceVersion returns the resource version (for persistence) of the specified object.
	// Should return an error if the specified object does not have a persistable version.
	ObjectResourceVersion(obj runtime.Object) (uint64, error)

	// ParseResourceVersion takes a resource version argument and
	// converts it to the storage backend. For watch we should pass to helper.Watch().
	// Because resourceVersion is an opaque value, the default watch
	// behavior for non-zero watch is to watch the next value (if you pass
	// "1", you will see updates from "2" onwards).
	ParseResourceVersion(resourceVersion string) (uint64, error)
}

// ResponseMeta contains information about the database metadata that is associated with
// an object. It abstracts the actual underlying objects to prevent coupling with concrete
// database and to improve testability.
type ResponseMeta struct {
	// TTL is the time to live of the node that contained the returned object. It may be
	// zero or negative in some cases (objects may be expired after the requested
	// expiration time due to server lag).
	TTL int64
	// The resource version of the node that contained the returned object.
	ResourceVersion uint64
}

// IndexerFunc is a function that for a given object computes
// `<value of an index>` for a particular `<index>`.
type IndexerFunc func(obj runtime.Object) string

// IndexerFuncs is a mapping from `<index name>` to function that
// for a given object computes `<value for that index>`.
type IndexerFuncs map[string]IndexerFunc

// Everything accepts all objects.
var Everything = SelectionPredicate{
	Label: labels.Everything(),
	Field: fields.Everything(),
}

// MatchValue defines a pair (`<index name>`, `<value for that index>`).
type MatchValue struct {
	IndexName string
	Value     string
}

// Pass an UpdateFunc to Interface.GuaranteedUpdate to make an update
// that is guaranteed to succeed.
// See the comment for GuaranteedUpdate for more details.
type UpdateFunc func(input runtime.Object, res ResponseMeta) (output runtime.Object, ttl *uint64, err error)

// ValidateObjectFunc is a function to act on a given object. An error may be returned
// if the hook cannot be completed. The function may NOT transform the provided
// object.
// NOTE: the object in obj may be nil if it cannot be read from the
// storage, due to transformation or decode error.
type ValidateObjectFunc func(ctx context.Context, obj runtime.Object) error

// ValidateAllObjectFunc is a "admit everything" instance of ValidateObjectFunc.
func ValidateAllObjectFunc(ctx context.Context, obj runtime.Object) error {
	return nil
}

// Preconditions must be fulfilled before an operation (update, delete, etc.) is carried out.
type Preconditions struct {
	// Specifies the target UID.
	// +optional
	UID *types.UID `json:"uid,omitempty"`
	// Specifies the target ResourceVersion
	// +optional
	ResourceVersion *string `json:"resourceVersion,omitempty"`
}

// NewUIDPreconditions returns a Preconditions with UID set.
func NewUIDPreconditions(uid string) *Preconditions {
	u := types.UID(uid)
	return &Preconditions{UID: &u}
}

func (p *Preconditions) Check(key string, obj runtime.Object) error {
	if p == nil {
		return nil
	}
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return NewInternalError(
			fmt.Errorf("can't enforce preconditions %v on un-introspectable object %v, got error: %w",
				*p,
				obj,
				err))
	}
	if p.UID != nil && *p.UID != objMeta.GetUID() {
		err := fmt.Sprintf(
			"Precondition failed: UID in precondition: %v, UID in object meta: %v",
			*p.UID,
			objMeta.GetUID())
		return NewInvalidObjError(key, err)
	}
	if p.ResourceVersion != nil && *p.ResourceVersion != objMeta.GetResourceVersion() {
		err := fmt.Sprintf(
			"Precondition failed: ResourceVersion in precondition: %v, ResourceVersion in object meta: %v",
			*p.ResourceVersion,
			objMeta.GetResourceVersion())
		return NewInvalidObjError(key, err)
	}
	return nil
}

// Interface offers a common interface for object marshaling/unmarshaling operations and
// hides all the storage-related operations behind it.
type Interface interface {
	// Returns Versioner associated with this interface.
	Versioner() Versioner

	// Create adds a new object at a key unless it already exists. 'ttl' is time-to-live
	// in seconds (0 means forever). If no error is returned and out is not nil, out will be
	// set to the read value from database.
	Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error

	// Delete removes the specified key and returns the value that existed at that spot.
	// If key didn't exist, it will return NotFound storage error.
	// If 'cachedExistingObject' is non-nil, it can be used as a suggestion about the
	// current version of the object to avoid read operation from storage to get it.
	// However, the implementations have to retry in case suggestion is stale.
	Delete(
		ctx context.Context, key string, out runtime.Object, preconditions *Preconditions,
		validateDeletion ValidateObjectFunc, cachedExistingObject runtime.Object, opts DeleteOptions) error

	// Watch begins watching the specified key. Events are decoded into API objects,
	// and any items selected by 'p' are sent down to returned watch.Interface.
	// resourceVersion may be used to specify what version to begin watching,
	// which should be the current resourceVersion, and no longer rv+1
	// (e.g. reconnecting without missing any updates).
	// If resource version is "0", this interface will get current object at given key
	// and send it in an "ADDED" event, before watch starts.
	Watch(ctx context.Context, key string, opts ListOptions) (watch.Interface, error)

	// Get unmarshals object found at key into objPtr. On a not found error, will either
	// return a zero object of the requested type, or an error, depending on 'opts.ignoreNotFound'.
	// Treats empty responses and nil response nodes exactly like a not found error.
	// The returned contents may be delayed, but it is guaranteed that they will
	// match 'opts.ResourceVersion' according 'opts.ResourceVersionMatch'.
	Get(ctx context.Context, key string, opts GetOptions, objPtr runtime.Object) error

	// GetList unmarshalls objects found at key into a *List api object (an object
	// that satisfies runtime.IsList definition).
	// If 'opts.Recursive' is false, 'key' is used as an exact match. If `opts.Recursive'
	// is true, 'key' is used as a prefix.
	// The returned contents may be delayed, but it is guaranteed that they will
	// match 'opts.ResourceVersion' according 'opts.ResourceVersionMatch'.
	GetList(ctx context.Context, key string, opts ListOptions, listObj runtime.Object) error

	// GuaranteedUpdate keeps calling 'tryUpdate()' to update key 'key' (of type 'destination')
	// retrying the update until success if there is index conflict.
	// Note that object passed to tryUpdate may change across invocations of tryUpdate() if
	// other writers are simultaneously updating it, so tryUpdate() needs to take into account
	// the current contents of the object when deciding how the update object should look.
	// If the key doesn't exist, it will return NotFound storage error if ignoreNotFound=false
	// else `destination` will be set to the zero value of it's type.
	// If the eventual successful invocation of `tryUpdate` returns an output with the same serialized
	// contents as the input, it won't perform any update, but instead set `destination` to an object with those
	// contents.
	// If 'cachedExistingObject' is non-nil, it can be used as a suggestion about the
	// current version of the object to avoid read operation from storage to get it.
	// However, the implementations have to retry in case suggestion is stale.
	//
	// Example:
	//
	// s := /* implementation of Interface */
	// err := s.GuaranteedUpdate(
	//     "myKey", &MyType{}, true, preconditions,
	//     func(input runtime.Object, res ResponseMeta) (runtime.Object, *uint64, error) {
	//       // Before each invocation of the user defined function, "input" is reset to
	//       // current contents for "myKey" in database.
	//       curr := input.(*MyType)  // Guaranteed to succeed.
	//
	//       // Make the modification
	//       curr.Counter++
	//
	//       // Return the modified object - return an error to stop iterating. Return
	//       // a uint64 to alter the TTL on the object, or nil to keep it the same value.
	//       return cur, nil, nil
	//    }, cachedExistingObject
	// )
	GuaranteedUpdate(
		ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool,
		preconditions *Preconditions, tryUpdate UpdateFunc, cachedExistingObject runtime.Object) error

	// Count returns number of different entries under the key (generally being path prefix).
	Count(key string) (int64, error)

	// ReadinessCheck checks if the storage is ready for accepting requests.
	ReadinessCheck() error

	// RequestWatchProgress requests the a watch stream progress status be sent in the
	// watch response stream as soon as possible.
	// Used for monitor watch progress even if watching resources with no changes.
	//
	// If watch is lagging, progress status might:
	// * be pointing to stale resource version. Use etcd KV request to get linearizable resource version.
	// * not be delivered at all. It's recommended to poll request progress periodically.
	//
	// Note: Only watches with matching context grpc metadata will be notified.
	// https://github.com/kubernetes/kubernetes/blob/9325a57125e8502941d1b0c7379c4bb80a678d5c/vendor/go.etcd.io/etcd/client/v3/watch.go#L1037-L1042
	//
	// TODO: Remove when storage.Interface will be separate from etc3.store.
	// Deprecated: Added temporarily to simplify exposing RequestProgress for watch cache.
	RequestWatchProgress(ctx context.Context) error
}

// GetOptions provides the options that may be provided for storage get operations.
type GetOptions struct {
	// IgnoreNotFound determines what is returned if the requested object is not found. If
	// true, a zero object is returned. If false, an error is returned.
	IgnoreNotFound bool
	// ResourceVersion provides a resource version constraint to apply to the get operation
	// as a "not older than" constraint: the result contains data at least as new as the provided
	// ResourceVersion. The newest available data is preferred, but any data not older than this
	// ResourceVersion may be served.
	ResourceVersion string
}

// ListOptions provides the options that may be provided for storage list operations.
type ListOptions struct {
	// ResourceVersion provides a resource version constraint to apply to the list operation
	// as a "not older than" constraint: the result contains data at least as new as the provided
	// ResourceVersion. The newest available data is preferred, but any data not older than this
	// ResourceVersion may be served.
	ResourceVersion string
	// ResourceVersionMatch provides the rule for how the resource version constraint applies. If set
	// to the default value "" the legacy resource version semantic apply.
	ResourceVersionMatch metav1.ResourceVersionMatch
	// Predicate provides the selection rules for the list operation.
	Predicate SelectionPredicate
	// Recursive determines whether the list or watch is defined for a single object located at the
	// given key, or for the whole set of objects with the given key as a prefix.
	Recursive bool
	// ProgressNotify determines whether storage-originated bookmark (progress notify) events should
	// be delivered to the users. The option is ignored for non-watch requests.
	//
	// Firstly, note that this field is different from the Predicate.AllowWatchBookmarks field.
	// Secondly, this field is intended for internal clients only such as the watch cache.
	//
	// This means that external clients do not have the ability to set this field directly.
	// For example by setting the allowWatchBookmarks query parameter.
	//
	// The motivation for this approach is the fact that the frequency
	// of bookmark events from a storage like etcd might be very high.
	// As the number of watch requests increases, the server load would also increase.
	//
	// Furthermore, the server is not obligated to provide bookmark events at all,
	// as described in https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/956-watch-bookmark#proposal
	ProgressNotify bool
	// SendInitialEvents, when set together with Watch option,
	// begin the watch stream with synthetic init events to build the
	// whole state of all resources followed by a synthetic "Bookmark"
	// event containing a ResourceVersion after which the server
	// continues streaming events.
	SendInitialEvents *bool
}

// DeleteOptions provides the options that may be provided for storage delete operations.
type DeleteOptions struct {
	// IgnoreStoreReadError, if enabled, will ignore store read error
	// such as transformation or decode failure and go ahead with the
	// deletion of the object.
	// NOTE: for normal deletion flow it should always be false, it may be
	// enabled by the caller only to facilitate unsafe deletion of corrupt
	// object which otherwise can not be deleted using the normal flow
	IgnoreStoreReadError bool
}

// ListErrorAggregator aggregates the error(s) that the LIST operation
// encounters while retrieving object(s) from the storage
type ListErrorAggregator interface {
	// Aggregate aggregates the given error from list operation
	// key: it identifies the given object in the storage.
	// err: it represents the error the list operation encountered while
	// retrieving the given object from the storage.
	// done: true if the aggregation is done and the list operation should
	// abort, otherwise the list operation will continue
	Aggregate(key string, err error) bool

	// Err returns the aggregated error
	Err() error
}
