/*
Copyright 2017 The Kubernetes Authors.

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

package handlers

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metainternalversionvalidation "k8s.io/apimachinery/pkg/apis/meta/internalversion/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/server/routine"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/tracing"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// getterFunc performs a get request with the given context and object name. The request
// may be used to deserialize an options object to pass to the getter.
type getterFunc func(ctx context.Context, name string, req *http.Request) (runtime.Object, error)

// getResourceHandler is an HTTP handler function for get requests. It delegates to the
// passed-in getterFunc to perform the actual get.
func getResourceHandler(scope *RequestScope, getter getterFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		ctx, span := tracing.Start(ctx, "Get", traceFields(req)...)
		req = req.WithContext(ctx)
		defer span.End(500 * time.Millisecond)

		namespace, name, err := scope.Namer.Name(req)
		if err != nil {
			scope.err(err, w, req)
			return
		}
		ctx = request.WithNamespace(ctx, namespace)

		outputMediaType, _, err := negotiation.NegotiateOutputMediaType(req, scope.Serializer, scope)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		result, err := getter(ctx, name, req)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		span.AddEvent("About to write a response")
		defer span.AddEvent("Writing http response done")
		transformResponseObject(ctx, scope, req, w, http.StatusOK, outputMediaType, result)
	}
}

// GetResource returns a function that handles retrieving a single resource from a rest.Storage object.
func GetResource(r rest.Getter, scope *RequestScope) http.HandlerFunc {
	return getResourceHandler(scope,
		func(ctx context.Context, name string, req *http.Request) (runtime.Object, error) {
			// check for export
			options := metav1.GetOptions{}
			if values := req.URL.Query(); len(values) > 0 {
				if len(values["export"]) > 0 {
					exportBool := true
					exportStrings := values["export"]
					err := runtime.Convert_Slice_string_To_bool(&exportStrings, &exportBool, nil)
					if err != nil {
						return nil, errors.NewBadRequest(fmt.Sprintf("the export parameter cannot be parsed: %v", err))
					}
					if exportBool {
						return nil, errors.NewBadRequest("the export parameter, deprecated since v1.14, is no longer supported")
					}
				}
				if err := metainternalversionscheme.ParameterCodec.DecodeParameters(values, scope.MetaGroupVersion, &options); err != nil {
					err = errors.NewBadRequest(err.Error())
					return nil, err
				}
			}
			tracing.SpanFromContext(ctx).AddEvent("About to Get from storage")
			return r.Get(ctx, name, &options)
		})
}

// GetResourceWithOptions returns a function that handles retrieving a single resource from a rest.Storage object.
func GetResourceWithOptions(r rest.GetterWithOptions, scope *RequestScope, isSubresource bool) http.HandlerFunc {
	return getResourceHandler(scope,
		func(ctx context.Context, name string, req *http.Request) (runtime.Object, error) {
			opts, subpath, subpathKey := r.NewGetOptions()
			span := tracing.SpanFromContext(ctx)
			span.AddEvent("About to process Get options")
			if err := getRequestOptions(req, scope, opts, subpath, subpathKey, isSubresource); err != nil {
				err = errors.NewBadRequest(err.Error())
				return nil, err
			}
			span.AddEvent("About to Get from storage")
			return r.Get(ctx, name, opts)
		})
}

// getRequestOptions parses out options and can include path information.  The path information shouldn't include the subresource.
func getRequestOptions(req *http.Request, scope *RequestScope, into runtime.Object, subpath bool, subpathKey string, isSubresource bool) error {
	if into == nil {
		return nil
	}

	query := req.URL.Query()
	if subpath {
		newQuery := make(url.Values)
		for k, v := range query {
			newQuery[k] = v
		}

		ctx := req.Context()
		requestInfo, _ := request.RequestInfoFrom(ctx)
		startingIndex := 2
		if isSubresource {
			startingIndex = 3
		}

		p := strings.Join(requestInfo.Parts[startingIndex:], "/")

		// ensure non-empty subpaths correctly reflect a leading slash
		if len(p) > 0 && !strings.HasPrefix(p, "/") {
			p = "/" + p
		}

		// ensure subpaths correctly reflect the presence of a trailing slash on the original request
		if strings.HasSuffix(requestInfo.Path, "/") && !strings.HasSuffix(p, "/") {
			p += "/"
		}

		newQuery[subpathKey] = []string{p}
		query = newQuery
	}
	return scope.ParameterCodec.DecodeParameters(query, scope.Kind.GroupVersion(), into)
}

func ListResource(r rest.Lister, rw rest.Watcher, scope *RequestScope, forceWatch bool, minRequestTimeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		// For performance tracking purposes.
		ctx, span := tracing.Start(ctx, "List", traceFields(req)...)
		req = req.WithContext(ctx)

		namespace, err := scope.Namer.Namespace(req)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		// Watches for single objects are routed to this function.
		// Treat a name parameter the same as a field selector entry.
		hasName := true
		_, name, err := scope.Namer.Name(req)
		if err != nil {
			hasName = false
		}
		ctx = request.WithNamespace(ctx, namespace)

		opts := metainternalversion.ListOptions{}
		if err := metainternalversionscheme.ParameterCodec.DecodeParameters(req.URL.Query(), scope.MetaGroupVersion, &opts); err != nil {
			err = errors.NewBadRequest(err.Error())
			scope.err(err, w, req)
			return
		}

		metainternalversion.SetListOptionsDefaults(&opts, utilfeature.DefaultFeatureGate.Enabled(features.WatchList))
		if errs := metainternalversionvalidation.ValidateListOptions(&opts, utilfeature.DefaultFeatureGate.Enabled(features.WatchList)); len(errs) > 0 {
			err := errors.NewInvalid(schema.GroupKind{Group: metav1.GroupName, Kind: "ListOptions"}, "", errs)
			scope.err(err, w, req)
			return
		}

		outputMediaType, _, err := negotiation.NegotiateOutputMediaType(req, scope.Serializer, scope)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		// transform fields
		// TODO: DecodeParametersInto should do this.
		if opts.FieldSelector != nil {
			fn := func(label, value string) (newLabel, newValue string, err error) {
				return scope.Convertor.ConvertFieldLabel(scope.Kind, label, value)
			}
			if opts.FieldSelector, err = opts.FieldSelector.Transform(fn); err != nil {
				// TODO: allow bad request to set field causes based on query parameters
				err = errors.NewBadRequest(err.Error())
				scope.err(err, w, req)
				return
			}
		}

		if hasName {
			// metadata.name is the canonical internal name.
			// SelectionPredicate will notice that this is a request for
			// a single object and optimize the storage query accordingly.
			nameSelector := fields.OneTermEqualSelector("metadata.name", name)

			// Note that fieldSelector setting explicitly the "metadata.name"
			// will result in reaching this branch (as the value of that field
			// is propagated to requestInfo as the name parameter.
			// That said, the allowed field selectors in this branch are:
			// nil, fields.Everything and field selector matching metadata.name
			// for our name.
			if opts.FieldSelector != nil && !opts.FieldSelector.Empty() {
				selectedName, ok := opts.FieldSelector.RequiresExactMatch("metadata.name")
				if !ok || name != selectedName {
					scope.err(errors.NewBadRequest("fieldSelector metadata.name doesn't match requested name"), w, req)
					return
				}
			} else {
				opts.FieldSelector = nameSelector
			}
		}

		if opts.Watch || forceWatch {
			if rw == nil {
				scope.err(errors.NewMethodNotSupported(scope.Resource.GroupResource(), "watch"), w, req)
				return
			}
			// TODO: Currently we explicitly ignore ?timeout= and use only ?timeoutSeconds=.
			timeout := time.Duration(0)
			if opts.TimeoutSeconds != nil {
				timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
			}
			if timeout == 0 && minRequestTimeout > 0 {
				timeout = time.Duration(float64(minRequestTimeout) * (rand.Float64() + 1.0))
			}

			var emptyVersionedList runtime.Object
			if isListWatchRequest(opts) {
				emptyVersionedList, err = scope.Convertor.ConvertToVersion(r.NewList(), scope.Kind.GroupVersion())
				if err != nil {
					scope.err(errors.NewInternalError(err), w, req)
					return
				}
			}

			klog.V(3).InfoS("Starting watch", "path", req.URL.Path, "resourceVersion", opts.ResourceVersion, "labels", opts.LabelSelector, "fields", opts.FieldSelector, "timeout", timeout)
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer func() { cancel() }()
			watcher, err := rw.Watch(ctx, &opts)
			if err != nil {
				scope.err(err, w, req)
				return
			}
			handler, err := serveWatchHandler(watcher, scope, outputMediaType, req, w, timeout, metrics.CleanListScope(ctx, &opts), emptyVersionedList)
			if err != nil {
				scope.err(err, w, req)
				return
			}
			// Invalidate cancel() to defer until serve() is complete.
			deferredCancel := cancel
			cancel = func() {}

			serve := func() {
				defer deferredCancel()
				requestInfo, _ := request.RequestInfoFrom(ctx)
				metrics.RecordLongRunning(req, requestInfo, metrics.APIServerComponent, func() {
					defer watcher.Stop()
					handler.ServeHTTP(w, req)
				})
			}

			// Run watch serving in a separate goroutine to allow freeing current stack memory
			t := routine.TaskFrom(req.Context())
			if t != nil {
				t.Func = serve
			} else {
				serve()
			}
			return
		}

		// Log only long List requests (ignore Watch).
		defer span.End(500 * time.Millisecond)
		span.AddEvent("About to List from storage")
		result, err := r.List(ctx, &opts)
		if err != nil {
			scope.err(err, w, req)
			return
		}
		span.AddEvent("Listing from storage done")
		defer span.AddEvent("Writing http response done", attribute.Int("count", meta.LenList(result)))
		transformResponseObject(ctx, scope, req, w, http.StatusOK, outputMediaType, result)
	}
}

func isListWatchRequest(opts metainternalversion.ListOptions) bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.WatchList) && ptr.Deref(opts.SendInitialEvents, false) && opts.AllowWatchBookmarks
}
