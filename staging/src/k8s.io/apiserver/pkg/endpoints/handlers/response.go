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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1beta1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	endpointsrequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/util/apihelpers"
	"k8s.io/klog/v2"
)

// watchEmbeddedEncoder performs encoding of the embedded object.
//
// NOTE: watchEmbeddedEncoder is NOT thread-safe.
type watchEmbeddedEncoder struct {
	encoder runtime.Encoder

	ctx context.Context

	// target, if non-nil, configures transformation type.
	// The other options are ignored if target is nil.
	target       *schema.GroupVersionKind
	tableOptions *metav1.TableOptions
	scope        *RequestScope

	// identifier of the encoder, computed lazily
	identifier runtime.Identifier
}

func newWatchEmbeddedEncoder(ctx context.Context, encoder runtime.Encoder, target *schema.GroupVersionKind, tableOptions *metav1.TableOptions, scope *RequestScope) *watchEmbeddedEncoder {
	return &watchEmbeddedEncoder{
		encoder:      encoder,
		ctx:          ctx,
		target:       target,
		tableOptions: tableOptions,
		scope:        scope,
	}
}

// Encode implements runtime.Encoder interface.
func (e *watchEmbeddedEncoder) Encode(obj runtime.Object, w io.Writer) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(e.Identifier(), e.doEncode, w)
	}
	return e.doEncode(obj, w)
}

func (e *watchEmbeddedEncoder) doEncode(obj runtime.Object, w io.Writer) error {
	result, err := doTransformObject(e.ctx, obj, e.tableOptions, e.target, e.scope)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to transform object %v: %v", reflect.TypeOf(obj), err))
		result = obj
	}

	// When we are transforming to a table, use the original table options when
	// we should print headers only on the first object - headers should be
	// omitted on subsequent events.
	if e.tableOptions != nil && !e.tableOptions.NoHeaders {
		e.tableOptions.NoHeaders = true
		// With options change, we should recompute the identifier.
		// Clearing this will trigger lazy recompute when needed.
		e.identifier = ""
	}

	return e.encoder.Encode(result, w)
}

// Identifier implements runtime.Encoder interface.
func (e *watchEmbeddedEncoder) Identifier() runtime.Identifier {
	if e.identifier == "" {
		e.identifier = e.embeddedIdentifier()
	}
	return e.identifier
}

type watchEmbeddedEncoderIdentifier struct {
	Name      string              `json:"name,omitempty"`
	Encoder   string              `json:"encoder,omitempty"`
	Target    string              `json:"target,omitempty"`
	Options   metav1.TableOptions `json:"options,omitempty"`
	NoHeaders bool                `json:"noHeaders,omitempty"`
}

func (e *watchEmbeddedEncoder) embeddedIdentifier() runtime.Identifier {
	if e.target == nil {
		// If no conversion is performed, we effective only use
		// the embedded identifier.
		return e.encoder.Identifier()
	}
	identifier := watchEmbeddedEncoderIdentifier{
		Name:    "watch-embedded",
		Encoder: string(e.encoder.Identifier()),
		Target:  e.target.String(),
	}
	if e.target.Kind == "Table" && e.tableOptions != nil {
		identifier.Options = *e.tableOptions
		identifier.NoHeaders = e.tableOptions.NoHeaders
	}

	result, err := json.Marshal(identifier)
	if err != nil {
		klog.Fatalf("Failed marshaling identifier for watchEmbeddedEncoder: %v", err)
	}
	return runtime.Identifier(result)
}

// watchEncoder performs encoding of the watch events.
//
// NOTE: watchEncoder is NOT thread-safe.
type watchEncoder struct {
	ctx             context.Context
	kind            schema.GroupVersionKind
	embeddedEncoder runtime.Encoder
	encoder         runtime.Encoder
	framer          io.Writer

	watchListTransformerFn watchListTransformerFunction

	buffer      runtime.Splice
	eventBuffer runtime.Splice

	currentEmbeddedIdentifier runtime.Identifier
	identifiers               map[watch.EventType]runtime.Identifier
}

func newWatchEncoder(ctx context.Context, kind schema.GroupVersionKind, embeddedEncoder runtime.Encoder, encoder runtime.Encoder, framer io.Writer, watchListTransformerFn watchListTransformerFunction) *watchEncoder {
	return &watchEncoder{
		ctx:                    ctx,
		kind:                   kind,
		embeddedEncoder:        embeddedEncoder,
		encoder:                encoder,
		framer:                 framer,
		watchListTransformerFn: watchListTransformerFn,
		buffer:                 runtime.NewSpliceBuffer(),
		eventBuffer:            runtime.NewSpliceBuffer(),
	}
}

// Encode encodes a given watch event.
// NOTE: if events object is implementing the CacheableObject interface,
//
//	the serialized version is cached in that object [not the event itself].
func (e *watchEncoder) Encode(event watch.Event) error {
	encodeFunc := func(obj runtime.Object, w io.Writer) error {
		return e.doEncode(obj, event, w)
	}
	if event.Type == watch.Bookmark {
		// Bookmark objects are small, and we don't yet support serialization for them.
		// Additionally, we need to additionally transform them to support watch-list feature
		event = e.watchListTransformerFn(event)
		return encodeFunc(event.Object, e.framer)
	}
	if co, ok := event.Object.(runtime.CacheableObject); ok {
		return co.CacheEncode(e.identifier(event.Type), encodeFunc, e.framer)
	}
	return encodeFunc(event.Object, e.framer)
}

func (e *watchEncoder) doEncode(obj runtime.Object, event watch.Event, w io.Writer) error {
	defer e.buffer.Reset()

	if err := e.embeddedEncoder.Encode(obj, e.buffer); err != nil {
		return fmt.Errorf("unable to encode watch object %T: %v", obj, err)
	}

	// ContentType is not required here because we are defaulting to the serializer type.
	outEvent := &metav1.WatchEvent{
		Type:   string(event.Type),
		Object: runtime.RawExtension{Raw: e.buffer.Bytes()},
	}
	metrics.WatchEventsSizes.WithContext(e.ctx).WithLabelValues(e.kind.Group, e.kind.Version, e.kind.Kind).Observe(float64(len(outEvent.Object.Raw)))

	defer e.eventBuffer.Reset()
	if err := e.encoder.Encode(outEvent, e.eventBuffer); err != nil {
		return fmt.Errorf("unable to encode watch object %T: %v (%#v)", outEvent, err, e)
	}

	_, err := w.Write(e.eventBuffer.Bytes())
	return err
}

type watchEncoderIdentifier struct {
	Name            string `json:"name,omitempty"`
	EmbeddedEncoder string `json:"embeddedEncoder,omitempty"`
	Encoder         string `json:"encoder,omitempty"`
	EventType       string `json:"eventType,omitempty"`
}

func (e *watchEncoder) identifier(eventType watch.EventType) runtime.Identifier {
	// We need to take into account that in embeddedEncoder includes table
	// transformer, then its identifier is dynamic. As a result, whenever
	// the identifier of embeddedEncoder changes, we need to invalidate the
	// whole identifiers cache.
	// TODO(wojtek-t): Can we optimize it somehow?
	if e.currentEmbeddedIdentifier != e.embeddedEncoder.Identifier() {
		e.currentEmbeddedIdentifier = e.embeddedEncoder.Identifier()
		e.identifiers = map[watch.EventType]runtime.Identifier{}
	}
	if _, ok := e.identifiers[eventType]; !ok {
		e.identifiers[eventType] = e.typeIdentifier(eventType)
	}
	return e.identifiers[eventType]
}

func (e *watchEncoder) typeIdentifier(eventType watch.EventType) runtime.Identifier {
	// The eventType is a non-standard pattern. This is coming from the fact
	// that we're effectively serializing the whole watch event, but storing
	// it in serializations of the Object within the watch event.
	identifier := watchEncoderIdentifier{
		Name:            "watch",
		EmbeddedEncoder: string(e.embeddedEncoder.Identifier()),
		Encoder:         string(e.encoder.Identifier()),
		EventType:       string(eventType),
	}

	result, err := json.Marshal(identifier)
	if err != nil {
		klog.Fatalf("Failed marshaling identifier for watchEncoder: %v", err)
	}
	return runtime.Identifier(result)
}

// doTransformResponseObject is used for handling all requests, including watch.
func doTransformObject(ctx context.Context, obj runtime.Object, opts interface{}, target *schema.GroupVersionKind, scope *RequestScope) (runtime.Object, error) {
	if _, ok := obj.(*metav1.Status); ok {
		return obj, nil
	}

	switch {
	case target == nil:
		// If we ever change that from a no-op, the identifier of
		// the watchEmbeddedEncoder has to be adjusted accordingly.
		return obj, nil

	case target.Kind == "PartialObjectMetadata":
		return asPartialObjectMetadata(obj, target.GroupVersion())

	case target.Kind == "PartialObjectMetadataList":
		return asPartialObjectMetadataList(obj, target.GroupVersion())

	case target.Kind == "Table":
		options, ok := opts.(*metav1.TableOptions)
		if !ok {
			return nil, fmt.Errorf("unexpected TableOptions, got %T", opts)
		}
		return asTable(ctx, obj, options, scope, target.GroupVersion())

	default:
		accepted, _ := negotiation.MediaTypesForSerializer(apihelpers.GetMetaInternalVersionCodecs())
		err := negotiation.NewNotAcceptableError(accepted)
		return nil, err
	}
}

// optionsForTransform will load and validate any additional query parameter options for
// a conversion or return an error.
func optionsForTransform(mediaType negotiation.MediaTypeOptions, req *http.Request) (interface{}, error) {
	switch target := mediaType.Convert; {
	case target == nil:
	case target.Kind == "Table" && (target.GroupVersion() == metav1beta1.SchemeGroupVersion || target.GroupVersion() == metav1.SchemeGroupVersion):
		opts := &metav1.TableOptions{}
		if err := metainternalversionscheme.ParameterCodec.DecodeParameters(req.URL.Query(), metav1.SchemeGroupVersion, opts); err != nil {
			return nil, err
		}
		switch errs := validation.ValidateTableOptions(opts); len(errs) {
		case 0:
			return opts, nil
		case 1:
			return nil, errors.NewBadRequest(fmt.Sprintf("Unable to convert to Table as requested: %v", errs[0].Error()))
		default:
			return nil, errors.NewBadRequest(fmt.Sprintf("Unable to convert to Table as requested: %v", errs))
		}
	}
	return nil, nil
}

// targetEncodingForTransform returns the appropriate serializer for the input media type
func targetEncodingForTransform(scope *RequestScope, mediaType negotiation.MediaTypeOptions, req *http.Request) (schema.GroupVersionKind, runtime.NegotiatedSerializer, bool) {
	switch target := mediaType.Convert; {
	case target == nil:
	case (target.Kind == "PartialObjectMetadata" || target.Kind == "PartialObjectMetadataList" || target.Kind == "Table") &&
		(target.GroupVersion() == metav1beta1.SchemeGroupVersion || target.GroupVersion() == metav1.SchemeGroupVersion):
		return *target, apihelpers.GetMetaInternalVersionCodecs(), true
	}
	return scope.Kind, scope.Serializer, false
}

// transformResponseObject takes an object loaded from storage and performs any necessary transformations.
// Will write the complete response object.
// transformResponseObject is used only for handling non-streaming requests.
func transformResponseObject(ctx context.Context, scope *RequestScope, req *http.Request, w http.ResponseWriter, statusCode int, mediaType negotiation.MediaTypeOptions, result runtime.Object) {
	options, err := optionsForTransform(mediaType, req)
	if err != nil {
		scope.err(err, w, req)
		return
	}

	// ensure that for empty lists we don't return <nil> items.
	// This is safe to modify without deep-copying the object, as
	// List objects themselves are never cached.
	if meta.IsListType(result) && meta.LenList(result) == 0 {
		if err := meta.SetList(result, []runtime.Object{}); err != nil {
			scope.err(err, w, req)
			return
		}
	}

	var obj runtime.Object
	do := func() {
		obj, err = doTransformObject(ctx, result, options, mediaType.Convert, scope)
	}
	endpointsrequest.TrackTransformResponseObjectLatency(ctx, do)

	if err != nil {
		scope.err(err, w, req)
		return
	}
	kind, serializer, _ := targetEncodingForTransform(scope, mediaType, req)
	responsewriters.WriteObjectNegotiated(serializer, scope, kind.GroupVersion(), w, req, statusCode, obj, false)
}

// errNotAcceptable indicates Accept negotiation has failed
type errNotAcceptable struct {
	message string
}

func newNotAcceptableError(message string) error {
	return errNotAcceptable{message}
}

func (e errNotAcceptable) Error() string {
	return e.message
}

func (e errNotAcceptable) Status() metav1.Status {
	return metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    http.StatusNotAcceptable,
		Reason:  metav1.StatusReason("NotAcceptable"),
		Message: e.Error(),
	}
}

func asTable(ctx context.Context, result runtime.Object, opts *metav1.TableOptions, scope *RequestScope, groupVersion schema.GroupVersion) (runtime.Object, error) {
	switch groupVersion {
	case metav1beta1.SchemeGroupVersion, metav1.SchemeGroupVersion:
	default:
		return nil, newNotAcceptableError(fmt.Sprintf("no Table exists in group version %s", groupVersion))
	}

	obj, err := scope.TableConvertor.ConvertToTable(ctx, result, opts)
	if err != nil {
		return nil, err
	}

	table := (*metav1.Table)(obj)

	for i := range table.Rows {
		item := &table.Rows[i]
		switch opts.IncludeObject {
		case metav1.IncludeObject:
			item.Object.Object, err = scope.Convertor.ConvertToVersion(item.Object.Object, scope.Kind.GroupVersion())
			if err != nil {
				return nil, err
			}
		// TODO: rely on defaulting for the value here?
		case metav1.IncludeMetadata, "":
			m, err := meta.Accessor(item.Object.Object)
			if err != nil {
				return nil, err
			}
			// TODO: turn this into an internal type and do conversion in order to get object kind automatically set?
			partial := meta.AsPartialObjectMetadata(m)
			partial.GetObjectKind().SetGroupVersionKind(groupVersion.WithKind("PartialObjectMetadata"))
			item.Object.Object = partial
		case metav1.IncludeNone:
			item.Object.Object = nil
		default:
			err = errors.NewBadRequest(fmt.Sprintf("unrecognized includeObject value: %q", opts.IncludeObject))
			return nil, err
		}
	}

	return table, nil
}

func asPartialObjectMetadata(result runtime.Object, groupVersion schema.GroupVersion) (runtime.Object, error) {
	if meta.IsListType(result) {
		err := newNotAcceptableError(fmt.Sprintf("you requested PartialObjectMetadata, but the requested object is a list (%T)", result))
		return nil, err
	}
	switch groupVersion {
	case metav1beta1.SchemeGroupVersion, metav1.SchemeGroupVersion:
	default:
		return nil, newNotAcceptableError(fmt.Sprintf("no PartialObjectMetadataList exists in group version %s", groupVersion))
	}
	m, err := meta.Accessor(result)
	if err != nil {
		return nil, err
	}
	partial := meta.AsPartialObjectMetadata(m)
	partial.GetObjectKind().SetGroupVersionKind(groupVersion.WithKind("PartialObjectMetadata"))
	return partial, nil
}

func asPartialObjectMetadataList(result runtime.Object, groupVersion schema.GroupVersion) (runtime.Object, error) {
	li, ok := result.(metav1.ListInterface)
	if !ok {
		return nil, newNotAcceptableError(fmt.Sprintf("you requested PartialObjectMetadataList, but the requested object is not a list (%T)", result))
	}

	gvk := groupVersion.WithKind("PartialObjectMetadata")
	switch {
	case groupVersion == metav1beta1.SchemeGroupVersion:
		list := &metav1beta1.PartialObjectMetadataList{}
		err := meta.EachListItem(result, func(obj runtime.Object) error {
			m, err := meta.Accessor(obj)
			if err != nil {
				return err
			}
			partial := meta.AsPartialObjectMetadata(m)
			partial.GetObjectKind().SetGroupVersionKind(gvk)
			list.Items = append(list.Items, *partial)
			return nil
		})
		if err != nil {
			return nil, err
		}
		list.ResourceVersion = li.GetResourceVersion()
		list.Continue = li.GetContinue()
		list.RemainingItemCount = li.GetRemainingItemCount()
		return list, nil

	case groupVersion == metav1.SchemeGroupVersion:
		list := &metav1.PartialObjectMetadataList{}
		err := meta.EachListItem(result, func(obj runtime.Object) error {
			m, err := meta.Accessor(obj)
			if err != nil {
				return err
			}
			partial := meta.AsPartialObjectMetadata(m)
			partial.GetObjectKind().SetGroupVersionKind(gvk)
			list.Items = append(list.Items, *partial)
			return nil
		})
		if err != nil {
			return nil, err
		}
		list.ResourceVersion = li.GetResourceVersion()
		list.Continue = li.GetContinue()
		list.RemainingItemCount = li.GetRemainingItemCount()
		return list, nil

	default:
		return nil, newNotAcceptableError(fmt.Sprintf("no PartialObjectMetadataList exists in group version %s", groupVersion))
	}
}

// watchListTransformerFunction an optional function
// applied to watchlist bookmark events that transforms
// the embedded object before sending it to a client.
type watchListTransformerFunction func(watch.Event) watch.Event

// watchListTransformer performs transformation of
// a special watchList bookmark event.
//
// The bookmark is annotated with InitialEventsListBlueprintAnnotationKey
// and contains an empty, versioned list that we must encode in the requested format
// (e.g., protobuf, JSON, CBOR) and then store as a base64-encoded string.
type watchListTransformer struct {
	initialEventsListBlueprint runtime.Object
	targetGVK                  *schema.GroupVersionKind
	negotiatedEncoder          runtime.Encoder
	buffer                     runtime.Splice
}

// createWatchListTransformerIfRequested returns a transformer function for watchlist bookmark event.
func newWatchListTransformer(initialEventsListBlueprint runtime.Object, targetGVK *schema.GroupVersionKind, negotiatedEncoder runtime.Encoder) *watchListTransformer {
	return &watchListTransformer{
		initialEventsListBlueprint: initialEventsListBlueprint,
		targetGVK:                  targetGVK,
		negotiatedEncoder:          negotiatedEncoder,
		buffer:                     runtime.NewSpliceBuffer(),
	}
}

func (e *watchListTransformer) transform(event watch.Event) watch.Event {
	if e.initialEventsListBlueprint == nil {
		return event
	}
	hasAnnotation, err := storage.HasInitialEventsEndBookmarkAnnotation(event.Object)
	if err != nil {
		return newWatchEventErrorFor(err)
	}
	if !hasAnnotation {
		return event
	}

	if err = e.encodeInitialEventsListBlueprint(event.Object); err != nil {
		return newWatchEventErrorFor(err)
	}

	return event
}

func (e *watchListTransformer) encodeInitialEventsListBlueprint(object runtime.Object) error {
	initialEventsListBlueprint, err := e.transformInitialEventsListBlueprint()
	if err != nil {
		return err
	}

	defer e.buffer.Reset()
	if err = e.negotiatedEncoder.Encode(initialEventsListBlueprint, e.buffer); err != nil {
		return err
	}
	encodedInitialEventsListBlueprint := e.buffer.Bytes()

	// the storage layer creates a deep copy of the obj before modifying it.
	// since the object has the annotation, we can modify it directly.
	objectMeta, err := meta.Accessor(object)
	if err != nil {
		return err
	}
	annotations := objectMeta.GetAnnotations()
	annotations[metav1.InitialEventsListBlueprintAnnotationKey] = base64.StdEncoding.EncodeToString(encodedInitialEventsListBlueprint)
	objectMeta.SetAnnotations(annotations)

	return nil
}

func (e *watchListTransformer) transformInitialEventsListBlueprint() (runtime.Object, error) {
	if e.targetGVK != nil && e.targetGVK.Kind == "PartialObjectMetadata" {
		return asPartialObjectMetadataList(e.initialEventsListBlueprint, e.targetGVK.GroupVersion())
	}
	return e.initialEventsListBlueprint, nil
}

func newWatchEventErrorFor(err error) watch.Event {
	return watch.Event{
		Type: watch.Error,
		Object: &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: err.Error(),
			Reason:  metav1.StatusReasonInternalError,
			Code:    http.StatusInternalServerError,
		},
	}
}
