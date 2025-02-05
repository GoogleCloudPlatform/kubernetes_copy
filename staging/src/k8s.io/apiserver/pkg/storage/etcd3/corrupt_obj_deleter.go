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

package etcd3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/klog/v2"
)

// NewStoreWithUnsafeCorruptObjectDeletion wraps the given store implementation
// and adds support for unsafe deletion of corrupt objects
func NewStoreWithUnsafeCorruptObjectDeletion(delegate storage.Interface, gr schema.GroupResource) storage.Interface {
	return &corruptObjectDeleter{
		Interface:     delegate,
		groupResource: gr,
	}
}

// WithCorruptObjErrorHandlingDecoder decorates the given decoder, it determines
// if the error returned by the given decoder represents a corrupt object (the
// object is undecodable), and then it wraps the error appropriately so the
// unsafe deleter can determine if the object is a candidate for unsafe deletion
func WithCorruptObjErrorHandlingDecoder(decoder Decoder) Decoder {
	return &corruptObjErrorInterpretingDecoder{Decoder: decoder}
}

// WithCorruptObjErrorHandlingTransformer decorates the given decoder, it
// determines if the error returned by the given transformer represents a
// corrupt object (the data from the storage is untransformable), and then it
// wraps the error appropriately so the unsafe deleter can determine
// if the object is a candidate for unsafe deletion
func WithCorruptObjErrorHandlingTransformer(transformer value.Transformer) value.Transformer {
	return &corruptObjErrorInterpretingTransformer{Transformer: transformer}
}

// corruptObjErrAggregatorFactory returns an error aggregator that aggregates
// corrupt object error(s) that the list operation encounters while
// retrieving objects from the storage.
// maxCount: it is the maximum number of error that will be aggregated
func corruptObjErrAggregatorFactory(maxCount int) func() storage.ListErrorAggregator {
	if maxCount <= 0 {
		return defaultListErrorAggregatorFactory
	}
	return func() storage.ListErrorAggregator {
		return &corruptObjErrAggregator{maxCount: maxCount}
	}
}

var errTooMany = errors.New("too many errors, the list is truncated")

// aggregate corrupt object errors from the LIST operation
type corruptObjErrAggregator struct {
	errs     []error
	abortErr error
	maxCount int
}

func (a *corruptObjErrAggregator) Aggregate(key string, err error) bool {
	if len(a.errs) >= a.maxCount {
		// add a sentinel error to indicate there are more
		a.errs = append(a.errs, errTooMany)
		return true
	}
	var corruptObjErr *corruptObjectError
	if errors.As(err, &corruptObjErr) {
		a.errs = append(a.errs, storage.NewCorruptObjError(key, corruptObjErr))
		return false
	}

	// not a corrupt object error, the list operation should abort
	a.abortErr = err
	return true
}

func (a *corruptObjErrAggregator) Err() error {
	switch {
	case len(a.errs) == 0 && a.abortErr != nil:
		return a.abortErr
	case len(a.errs) > 0:
		err := utilerrors.NewAggregate(a.errs)
		return &aggregatedStorageError{errs: err, resourcePrefix: "list"}
	default:
		return nil
	}
}

// corruptObjectDeleter facilitates unsafe deletion of corrupt objects for etcd
type corruptObjectDeleter struct {
	storage.Interface
	groupResource schema.GroupResource
}

func (s *corruptObjectDeleter) Get(ctx context.Context, key string, opts storage.GetOptions, out runtime.Object) error {
	if err := s.Interface.Get(ctx, key, opts, out); err != nil {
		var corruptObjErr *corruptObjectError
		if !errors.As(err, &corruptObjErr) {
			// this error does not represent a corrupt object
			return err
		}
		// the unsafe deleter at the registry layer will check whether
		// the given err represents a corrupt object in order to
		// initiate the unsafe deletion flow.
		return storage.NewCorruptObjError(key, corruptObjErr)
	}
	return nil
}

func (s *corruptObjectDeleter) GetList(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	err := s.Interface.GetList(ctx, key, opts, listObj)
	if err == nil {
		return nil
	}

	var aggregatedErr *aggregatedStorageError
	if errors.As(err, &aggregatedErr) {
		// we have aggregated a list of corrupt objects
		klog.V(5).ErrorS(aggregatedErr, "corrupt objects")
		return aggregatedErr.NewAPIStatusError(s.groupResource)
	}
	return err
}

// corruptObjErrorInterpretingDecoder wraps the error returned by the decorated decoder
type corruptObjErrorInterpretingDecoder struct {
	Decoder
}

func (d *corruptObjErrorInterpretingDecoder) Decode(value []byte, objPtr runtime.Object, rev int64) error {
	// TODO: right now any error is deemed as undecodable, in
	// the future, we can apply some filter, if need be.
	if err := d.Decoder.Decode(value, objPtr, rev); err != nil {
		return &corruptObjectError{err: err, errType: undecodable, revision: rev}
	}
	return nil
}

// decodeListItem decodes bytes value in array into object.
func (d *corruptObjErrorInterpretingDecoder) DecodeListItem(ctx context.Context, data []byte, rev uint64, newItemFunc func() runtime.Object) (runtime.Object, error) {
	// TODO: right now any error is deemed as undecodable, in
	// the future, we can apply some filter, if need be.
	obj, err := d.Decoder.DecodeListItem(ctx, data, rev, newItemFunc)
	if err != nil {
		err = &corruptObjectError{err: err, errType: undecodable, revision: int64(rev)}
	}
	return obj, err
}

// corruptObjErrorInterpretingTransformer wraps the error returned by the transformer
type corruptObjErrorInterpretingTransformer struct {
	value.Transformer
}

func (t *corruptObjErrorInterpretingTransformer) TransformFromStorage(ctx context.Context, data []byte, dataCtx value.Context) ([]byte, bool, error) {
	// TODO: right now any error is deemed as undecodable, in the future, we
	// can apply some filter, if need be. For example, any network error
	out, stale, err := t.Transformer.TransformFromStorage(ctx, data, dataCtx)
	if err != nil {
		err = &corruptObjectError{err: err, errType: untransformable}
	}
	return out, stale, err
}

// corruptObjectError is used internally, only by the corrupt object
// deleter, this error represents a corrup object:
// a) the data from the storage failed to transform, or
// b) the data failed to decode into an object
// NOTE: this error does not have any information to identify the object
// that is corrupt, for example the storage key associated with the object
type corruptObjectError struct {
	err      error
	errType  int
	revision int64
}

const (
	untransformable int = iota + 1
	undecodable
)

var typeToMessage = map[int]string{
	untransformable: "data from the storage is not transformable",
	undecodable:     "object not decodable",
}

func (e *corruptObjectError) Unwrap() error { return e.err }
func (e *corruptObjectError) Error() string {
	return fmt.Sprintf("%s revision=%d: %v", typeToMessage[e.errType], e.revision, e.err)
}

// aggregatedStorageError holds an aggregated list of storage.StorageError
type aggregatedStorageError struct {
	resourcePrefix string
	errs           utilerrors.Aggregate
}

func (e *aggregatedStorageError) Error() string {
	errs := e.errs.Errors()
	var b strings.Builder
	fmt.Fprintf(&b, "unable to transform or decode %d objects: {\n", len(errs))
	for _, err := range errs {
		fmt.Fprintf(&b, "\t%s\n", err.Error())
	}
	b.WriteString("}")
	return b.String()
}

// NewAPIStatusError creates a new APIStatus object from the
// aggregated list of StorageError
func (e *aggregatedStorageError) NewAPIStatusError(qualifiedResource schema.GroupResource) *apierrors.StatusError {
	var causes []metav1.StatusCause
	for _, err := range e.errs.Errors() {
		var storageErr *storage.StorageError
		if errors.As(err, &storageErr) {
			causes = append(causes, metav1.StatusCause{
				Type:  metav1.CauseTypeUnexpectedServerResponse,
				Field: storageErr.Key,
				// TODO: do we need to expose the internal error message here?
				Message: err.Error(),
			})
			continue
		}
		if errors.Is(err, errTooMany) {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeTooMany,
				Message: errTooMany.Error(),
			})
		}
	}

	return &apierrors.StatusError{
		ErrStatus: metav1.Status{
			Status: metav1.StatusFailure,
			Code:   http.StatusInternalServerError,
			Reason: metav1.StatusReasonStoreReadError,
			Details: &metav1.StatusDetails{
				Group:  qualifiedResource.Group,
				Kind:   qualifiedResource.Resource,
				Name:   e.resourcePrefix,
				Causes: causes,
			},
			Message: fmt.Sprintf("failed to read one or more %s from the storage", qualifiedResource.String()),
		},
	}
}
