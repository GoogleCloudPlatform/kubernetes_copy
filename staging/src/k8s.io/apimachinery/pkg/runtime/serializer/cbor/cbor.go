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

package cbor

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor/internal/modes"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	util "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/fxamacker/cbor/v2"
)

type metaFactory interface {
	// Interpret should return the version and kind of the wire-format of the object.
	Interpret(data []byte) (*schema.GroupVersionKind, error)
}

type defaultMetaFactory struct{}

func (mf *defaultMetaFactory) Interpret(data []byte) (*schema.GroupVersionKind, error) {
	var tm metav1.TypeMeta
	// The input is expected to include additional map keys besides apiVersion and kind, so use
	// lax mode for decoding into TypeMeta.
	if err := modes.DecodeLax.Unmarshal(data, &tm); err != nil {
		return nil, fmt.Errorf("unable to determine group/version/kind: %w", err)
	}
	actual := tm.GetObjectKind().GroupVersionKind()
	return &actual, nil
}

type Serializer interface {
	runtime.Serializer
	runtime.NondeterministicEncoder
	recognizer.RecognizingDecoder

	// NewSerializer returns a value of this interface type rather than exporting the serializer
	// type and returning one of those because the zero value of serializer isn't ready to
	// use. Users aren't intended to implement cbor.Serializer themselves, and this unexported
	// interface method is here to prevent that (https://go.dev/blog/module-compatibility).
	private()
}

var _ Serializer = &serializer{}

type options struct {
	strict    bool
	transcode bool
}

type Option func(*options)

// Strict configures a serializer to return a strict decoding error when it encounters map keys that
// do not correspond to a field in the target object of a decode operation. This option is disabled
// by default.
func Strict(s bool) Option {
	return func(opts *options) {
		opts.strict = s
	}
}

// Transcode configures a serializer to transcode the "raw" bytes of a decoded runtime.RawExtension
// or metav1.FieldsV1 object to JSON. This is enabled by default to support existing programs that
// depend on the assumption that objects of either type contain valid JSON.
func Transcode(s bool) Option {
	return func(opts *options) {
		opts.transcode = s
	}
}

type serializer struct {
	metaFactory metaFactory
	creater     runtime.ObjectCreater
	typer       runtime.ObjectTyper
	options     options
}

func (serializer) private() {}

// NewSerializer creates and returns a serializer configured with the provided options. The default
// options are equivalent to explicitly passing Strict(false) and Transcode(true).
func NewSerializer(creater runtime.ObjectCreater, typer runtime.ObjectTyper, options ...Option) Serializer {
	return newSerializer(&defaultMetaFactory{}, creater, typer, options...)
}

func newSerializer(metaFactory metaFactory, creater runtime.ObjectCreater, typer runtime.ObjectTyper, options ...Option) *serializer {
	s := &serializer{
		metaFactory: metaFactory,
		creater:     creater,
		typer:       typer,
	}
	s.options.transcode = true
	for _, o := range options {
		o(&s.options)
	}
	return s
}

func (s *serializer) Identifier() runtime.Identifier {
	return "cbor"
}

// Encode writes a CBOR representation of the given object.
//
// Because the CBOR data item written by a call to Encode is always enclosed in the "self-described
// CBOR" tag, its encoded form always has the prefix 0xd9d9f7. This prefix is suitable for use as a
// "magic number" for distinguishing encoded CBOR from other protocols.
//
// The default serialization behavior for any given object replicates the behavior of the JSON
// serializer as far as it is necessary to allow the CBOR serializer to be used as a drop-in
// replacement for the JSON serializer, with limited exceptions. For example, the distinction
// between integers and floating-point numbers is preserved in CBOR due to its distinct
// representations for each type.
//
// Objects implementing runtime.Unstructured will have their unstructured content encoded rather
// than following the default behavior for their dynamic type.
func (s *serializer) Encode(obj runtime.Object, w io.Writer) error {
	return s.encode(modes.Encode, obj, w)
}

func (s *serializer) EncodeNondeterministic(obj runtime.Object, w io.Writer) error {
	return s.encode(modes.EncodeNondeterministic, obj, w)
}

func (s *serializer) encode(mode modes.EncMode, obj runtime.Object, w io.Writer) error {
	var v interface{} = obj
	if u, ok := obj.(runtime.Unstructured); ok {
		v = u.UnstructuredContent()
	}

	if err := modes.RejectCustomMarshalers(v); err != nil {
		return err
	}

	if _, err := w.Write(selfDescribedCBOR); err != nil {
		return err
	}

	return mode.MarshalTo(v, w)
}

// gvkWithDefaults returns group kind and version defaulting from provided default
func gvkWithDefaults(actual, defaultGVK schema.GroupVersionKind) schema.GroupVersionKind {
	if len(actual.Kind) == 0 {
		actual.Kind = defaultGVK.Kind
	}
	if len(actual.Version) == 0 && len(actual.Group) == 0 {
		actual.Group = defaultGVK.Group
		actual.Version = defaultGVK.Version
	}
	if len(actual.Version) == 0 && actual.Group == defaultGVK.Group {
		actual.Version = defaultGVK.Version
	}
	return actual
}

// diagnose returns the diagnostic encoding of a well-formed CBOR data item.
func diagnose(data []byte) string {
	diag, err := modes.Diagnostic.Diagnose(data)
	if err != nil {
		// Since the input must already be well-formed CBOR, converting it to diagnostic
		// notation should not fail.
		//
		//nolint:logcheck
		util.HandleError(err)

		return hex.EncodeToString(data)
	}
	return diag
}

// unmarshal unmarshals CBOR data from the provided byte slice into a Go object. If the decoder is
// configured to report strict errors, the first error return value may be a non-nil strict decoding
// error. If the last error return value is non-nil, then the unmarshal failed entirely and the
// state of the destination object should not be relied on.
func (s *serializer) unmarshal(data []byte, into interface{}) (strict, lax error) {
	if u, ok := into.(runtime.Unstructured); ok {
		var content map[string]interface{}
		defer func() {
			switch u := u.(type) {
			case *unstructured.UnstructuredList:
				// UnstructuredList's implementation of SetUnstructuredContent
				// produces different objects than those produced by a decode using
				// UnstructuredJSONScheme:
				//
				//   1. SetUnstructuredContent retains the "items" key in the list's
				//      Object field. It is omitted from Object when decoding with
				//      UnstructuredJSONScheme.
				//   2. SetUnstructuredContent does not populate "apiVersion" and
				//      "kind" on each entry of its Items
				//      field. UnstructuredJSONScheme does, inferring the singular
				//      Kind from the list Kind.
				//   3. SetUnstructuredContent ignores entries of "items" that are
				//      not JSON objects or are objects without
				//      "kind". UnstructuredJSONScheme returns an error in either
				//      case.
				//
				// UnstructuredJSONScheme's behavior is replicated here.
				var items []interface{}
				if uncast, present := content["items"]; present {
					var cast bool
					items, cast = uncast.([]interface{})
					if !cast {
						strict, lax = nil, fmt.Errorf("items field of UnstructuredList must be encoded as an array or null if present")
						return
					}
				}
				apiVersion, _ := content["apiVersion"].(string)
				kind, _ := content["kind"].(string)
				kind = strings.TrimSuffix(kind, "List")
				var unstructureds []unstructured.Unstructured
				if len(items) > 0 {
					unstructureds = make([]unstructured.Unstructured, len(items))
				}
				for i := range items {
					object, cast := items[i].(map[string]interface{})
					if !cast {
						strict, lax = nil, fmt.Errorf("elements of the items field of UnstructuredList must be encoded as a map")
						return
					}

					// As in UnstructuredJSONScheme, only set the heuristic
					// singular GVK when both "apiVersion" and "kind" are either
					// missing, non-string, or empty.
					object["apiVersion"], _ = object["apiVersion"].(string)
					object["kind"], _ = object["kind"].(string)
					if object["apiVersion"] == "" && object["kind"] == "" {
						object["apiVersion"] = apiVersion
						object["kind"] = kind
					}

					if object["kind"] == "" {
						strict, lax = nil, runtime.NewMissingKindErr(diagnose(data))
						return
					}
					if object["apiVersion"] == "" {
						strict, lax = nil, runtime.NewMissingVersionErr(diagnose(data))
						return
					}

					unstructureds[i].Object = object
				}
				delete(content, "items")
				u.Object = content
				u.Items = unstructureds
			default:
				u.SetUnstructuredContent(content)
			}
		}()
		into = &content
	} else if err := modes.RejectCustomMarshalers(into); err != nil {
		return nil, err
	}

	if !s.options.strict {
		return nil, modes.DecodeLax.Unmarshal(data, into)
	}

	err := modes.Decode.Unmarshal(data, into)
	// TODO: UnknownFieldError is ambiguous. It only provides the index of the first problematic
	// map entry encountered and does not indicate which map the index refers to.
	var unknownField *cbor.UnknownFieldError
	if errors.As(err, &unknownField) {
		// Unlike JSON, there are no strict errors in CBOR for duplicate map keys. CBOR maps
		// with duplicate keys are considered invalid according to the spec and are rejected
		// entirely.
		return runtime.NewStrictDecodingError([]error{unknownField}), modes.DecodeLax.Unmarshal(data, into)
	}
	return nil, err
}

func (s *serializer) Decode(data []byte, gvk *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	// A preliminary pass over the input to obtain the actual GVK is redundant on a successful
	// decode into Unstructured.
	if _, ok := into.(runtime.Unstructured); ok {
		if _, unmarshalErr := s.unmarshal(data, into); unmarshalErr != nil {
			actual, interpretErr := s.metaFactory.Interpret(data)
			if interpretErr != nil {
				return nil, nil, interpretErr
			}

			if gvk != nil {
				*actual = gvkWithDefaults(*actual, *gvk)
			}

			return nil, actual, unmarshalErr
		}

		actual := into.GetObjectKind().GroupVersionKind()
		if len(actual.Kind) == 0 {
			return nil, &actual, runtime.NewMissingKindErr(diagnose(data))
		}
		if len(actual.Version) == 0 {
			return nil, &actual, runtime.NewMissingVersionErr(diagnose(data))
		}

		return into, &actual, nil
	}

	actual, err := s.metaFactory.Interpret(data)
	if err != nil {
		return nil, nil, err
	}

	if gvk != nil {
		*actual = gvkWithDefaults(*actual, *gvk)
	}

	if into != nil {
		types, _, err := s.typer.ObjectKinds(into)
		if err != nil {
			return nil, actual, err
		}
		*actual = gvkWithDefaults(*actual, types[0])
	}

	if len(actual.Kind) == 0 {
		return nil, actual, runtime.NewMissingKindErr(diagnose(data))
	}
	if len(actual.Version) == 0 {
		return nil, actual, runtime.NewMissingVersionErr(diagnose(data))
	}

	obj, err := runtime.UseOrCreateObject(s.typer, s.creater, *actual, into)
	if err != nil {
		return nil, actual, err
	}

	strict, err := s.unmarshal(data, obj)
	if err != nil {
		return nil, actual, err
	}

	if s.options.transcode {
		if err := transcodeRawTypes(obj); err != nil {
			return nil, actual, err
		}
	}

	return obj, actual, strict
}

// selfDescribedCBOR is the CBOR encoding of the head of tag number 55799. This tag, specified in
// RFC 8949 Section 3.4.6 "Self-Described CBOR", encloses all output from the encoder, has no
// special semantics, and is used as a magic number to recognize CBOR-encoded data items.
//
// See https://www.rfc-editor.org/rfc/rfc8949.html#name-self-described-cbor.
var selfDescribedCBOR = []byte{0xd9, 0xd9, 0xf7}

func (s *serializer) RecognizesData(data []byte) (ok, unknown bool, err error) {
	return bytes.HasPrefix(data, selfDescribedCBOR), false, nil
}

// NewSerializerInfo returns a default SerializerInfo for CBOR using the given creater and typer.
func NewSerializerInfo(creater runtime.ObjectCreater, typer runtime.ObjectTyper) runtime.SerializerInfo {
	return runtime.SerializerInfo{
		MediaType:        "application/cbor",
		MediaTypeType:    "application",
		MediaTypeSubType: "cbor",
		Serializer:       NewSerializer(creater, typer),
		StrictSerializer: NewSerializer(creater, typer, Strict(true)),
		StreamSerializer: &runtime.StreamSerializerInfo{
			Framer:     NewFramer(),
			Serializer: NewSerializer(creater, typer, Transcode(false)),
		},
	}
}
