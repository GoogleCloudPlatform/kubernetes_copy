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

package cache

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	restclient "k8s.io/client-go/rest"
)

// Lister is any object that knows how to perform an initial list.
//
// Ideally, all implementations of Lister should also implement ListerWithContext.
type Lister interface {
	// List should return a list type object; the Items field will be extracted, and the
	// ResourceVersion field will be used to start the watch in the right place.
	//
	// Deprecated: use ListerWithContext.ListWithContext instead.
	List(options metav1.ListOptions) (runtime.Object, error)
}

// ListerWithContext is any object that knows how to perform an initial list.
type ListerWithContext interface {
	// ListWithContext should return a list type object; the Items field will be extracted, and the
	// ResourceVersion field will be used to start the watch in the right place.
	ListWithContext(ctx context.Context, options metav1.ListOptions) (runtime.Object, error)
}

func ToListerWithContext(l Lister) ListerWithContext {
	if l, ok := l.(ListerWithContext); ok {
		return l
	}
	return listerWrapper{
		parent: l,
	}
}

type listerWrapper struct {
	parent Lister
}

func (l listerWrapper) ListWithContext(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
	return l.parent.List(options)
}

// Watcher is any object that knows how to start a watch on a resource.
//
// Ideally, all implementations of Watcher should also implement WatcherWithContext.
type Watcher interface {
	// Watch should begin a watch at the specified version.
	//
	// If Watch returns an error, it should handle its own cleanup, including
	// but not limited to calling Stop() on the watch, if one was constructed.
	// This allows the caller to ignore the watch, if the error is non-nil.
	//
	// Deprecated: use WatcherWithContext.WatchWithContext instead.
	Watch(options metav1.ListOptions) (watch.Interface, error)
}

// WatcherWithContext is any object that knows how to start a watch on a resource.
type WatcherWithContext interface {
	// WatchWithContext should begin a watch at the specified version.
	//
	// If Watch returns an error, it should handle its own cleanup, including
	// but not limited to calling Stop() on the watch, if one was constructed.
	// This allows the caller to ignore the watch, if the error is non-nil.
	WatchWithContext(ctx context.Context, options metav1.ListOptions) (watch.Interface, error)
}

func ToWatcherWithContext(w Watcher) WatcherWithContext {
	if w, ok := w.(WatcherWithContext); ok {
		return w
	}
	return watcherWrapper{
		parent: w,
	}
}

type watcherWrapper struct {
	parent Watcher
}

func (l watcherWrapper) WatchWithContext(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
	return l.parent.Watch(options)
}

// ListerWatcher is any object that knows how to perform an initial list and start a watch on a resource.
//
// Ideally, all implementations of ListerWatcher should also implement ListerWatcherWithContext.
type ListerWatcher interface {
	Lister
	Watcher
}

// ListerWatcherWithContext is any object that knows how to perform an initial list and start a watch on a resource.
type ListerWatcherWithContext interface {
	ListerWithContext
	WatcherWithContext
}

func ToListerWatcherWithContext(lw ListerWatcher) ListerWatcherWithContext {
	if lw, ok := lw.(ListerWatcherWithContext); ok {
		return lw
	}
	return listerWatcherWrapper{
		ListerWithContext:  ToListerWithContext(lw),
		WatcherWithContext: ToWatcherWithContext(lw),
	}
}

type listerWatcherWrapper struct {
	ListerWithContext
	WatcherWithContext
}

// ListFunc knows how to list resources
//
// Deprecated: use ListWithContextFunc instead.
type ListFunc func(options metav1.ListOptions) (runtime.Object, error)

// ListWithContextFunc knows how to list resources
type ListWithContextFunc func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error)

// WatchFunc knows how to watch resources
//
// Deprecated: use WatchFuncWithContext instead.
type WatchFunc func(options metav1.ListOptions) (watch.Interface, error)

// WatchFuncWithContext knows how to watch resources
type WatchFuncWithContext func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error)

// ListWatch knows how to list and watch a set of apiserver resources.
// It satisfies the ListerWatcher and ListerWatcherWithContext interfaces.
// It is a convenience function for users of NewReflector, etc.
// ListFunc or ListWithContextFunc must be set. Same for WatchFunc and WatchFuncWithContext.
// ListWithContextFunc and WatchFuncWithContext are preferred if
// a context is available, otherwise ListFunc and WatchFunc.
//
// NewFilteredListWatchFromClient sets all of the functions to ensure that callers
// which only know about ListFunc and WatchFunc continue to work.
type ListWatch struct {
	// Deprecated: use ListWithContext instead.
	ListFunc ListFunc
	// Deprecated: use WatchWithContext instead.
	WatchFunc WatchFunc

	ListWithContextFunc  ListWithContextFunc
	WatchFuncWithContext WatchFuncWithContext

	// DisableChunking requests no chunking for this list watcher.
	DisableChunking bool
}

var (
	_ ListerWatcher            = &ListWatch{}
	_ ListerWatcherWithContext = &ListWatch{}
)

// Getter interface knows how to access Get method from RESTClient.
type Getter interface {
	Get() *restclient.Request
}

// NewListWatchFromClient creates a new ListWatch from the specified client, resource, namespace and field selector.
// For backward compatibility, all function fields are populated.
func NewListWatchFromClient(c Getter, resource string, namespace string, fieldSelector fields.Selector) *ListWatch {
	optionsModifier := func(options *metav1.ListOptions) {
		options.FieldSelector = fieldSelector.String()
	}
	return NewFilteredListWatchFromClient(c, resource, namespace, optionsModifier)
}

// NewFilteredListWatchFromClient creates a new ListWatch from the specified client, resource, namespace, and option modifier.
// Option modifier is a function takes a ListOptions and modifies the consumed ListOptions. Provide customized modifier function
// to apply modification to ListOptions with a field selector, a label selector, or any other desired options.
// For backward compatibility, all function fields are populated.
func NewFilteredListWatchFromClient(c Getter, resource string, namespace string, optionsModifier func(options *metav1.ListOptions)) *ListWatch {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		optionsModifier(&options)
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Do(context.Background()).
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		optionsModifier(&options)
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Watch(context.Background())
	}
	listFuncWithContext := func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
		optionsModifier(&options)
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Do(ctx).
			Get()
	}
	watchFuncWithContext := func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		optionsModifier(&options)
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Watch(ctx)
	}
	return &ListWatch{
		ListFunc:             listFunc,
		WatchFunc:            watchFunc,
		ListWithContextFunc:  listFuncWithContext,
		WatchFuncWithContext: watchFuncWithContext,
	}
}

// List a set of apiserver resources
//
// Deprecated: use ListWatchWithContext.ListWithContext instead.
func (lw *ListWatch) List(options metav1.ListOptions) (runtime.Object, error) {
	// ListWatch is used in Reflector, which already supports pagination.
	// Don't paginate here to avoid duplication.
	if lw.ListFunc != nil {
		return lw.ListFunc(options)
	}
	return lw.ListWithContextFunc(context.Background(), options)
}

// List a set of apiserver resources
func (lw *ListWatch) ListWithContext(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
	// ListWatch is used in Reflector, which already supports pagination.
	// Don't paginate here to avoid duplication.
	if lw.ListWithContextFunc != nil {
		return lw.ListWithContextFunc(ctx, options)
	}
	return lw.ListFunc(options)
}

// Watch a set of apiserver resources
//
// Deprecated: use ListWatchWithContext.WatchWithContext instead.
func (lw *ListWatch) Watch(options metav1.ListOptions) (watch.Interface, error) {
	if lw.WatchFunc != nil {
		return lw.WatchFunc(options)
	}
	return lw.WatchFuncWithContext(context.Background(), options)
}

// Watch a set of apiserver resources
func (lw *ListWatch) WatchWithContext(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
	if lw.WatchFuncWithContext != nil {
		return lw.WatchFuncWithContext(ctx, options)
	}
	return lw.WatchFunc(options)
}
