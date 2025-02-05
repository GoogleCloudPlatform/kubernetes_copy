/*
Copyright 2014 The Kubernetes Authors.

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

package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/conversion/queryparams"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

// codec binds an encoder and decoder.
type codec struct {
	Encoder
	Decoder
}

// NewCodec creates a Codec from an Encoder and Decoder.
func NewCodec(e Encoder, d Decoder) Codec {
	return codec{e, d}
}

// Encode is a convenience wrapper for encoding to a []byte from an Encoder
func Encode(e Encoder, obj Object) ([]byte, error) {
	buf := &bytes.Buffer{}
	if err := e.Encode(obj, buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode is a convenience wrapper for decoding data into an Object.
func Decode(d Decoder, data []byte) (Object, error) {
	obj, _, err := d.Decode(data, nil, nil)
	return obj, err
}

// DecodeInto performs a Decode into the provided object.
func DecodeInto(d Decoder, data []byte, into Object) error {
	out, gvk, err := d.Decode(data, nil, into)
	if err != nil {
		return err
	}
	if out != into {
		return fmt.Errorf("unable to decode %s into %v", gvk, reflect.TypeOf(into))
	}
	return nil
}

// EncodeOrDie is a version of Encode which will panic instead of returning an error. For tests.
func EncodeOrDie(e Encoder, obj Object) string {
	bytes, err := Encode(e, obj)
	if err != nil {
		panic(err)
	}
	return string(bytes)
}

// UseOrCreateObject returns obj if the canonical ObjectKind returned by the provided typer matches gvk, or
// invokes the ObjectCreator to instantiate a new gvk. Returns an error if the typer cannot find the object.
func UseOrCreateObject(t ObjectTyper, c ObjectCreater, gvk schema.GroupVersionKind, obj Object) (Object, error) {
	if obj != nil {
		kinds, _, err := t.ObjectKinds(obj)
		if err != nil {
			return nil, err
		}
		for _, kind := range kinds {
			if gvk == kind {
				return obj, nil
			}
		}
	}
	return c.New(gvk)
}

// NoopEncoder converts an Decoder to a Serializer or Codec for code that expects them but only uses decoding.
type NoopEncoder struct {
	Decoder
}

var _ Serializer = NoopEncoder{}

const noopEncoderIdentifier Identifier = "noop"

func (n NoopEncoder) Encode(obj Object, w io.Writer) error {
	// There is no need to handle runtime.CacheableObject, as we don't
	// process the obj at all.
	return fmt.Errorf("encoding is not allowed for this codec: %v", reflect.TypeOf(n.Decoder))
}

// Identifier implements runtime.Encoder interface.
func (n NoopEncoder) Identifier() Identifier {
	return noopEncoderIdentifier
}

// NoopDecoder converts an Encoder to a Serializer or Codec for code that expects them but only uses encoding.
type NoopDecoder struct {
	Encoder
}

var _ Serializer = NoopDecoder{}

func (n NoopDecoder) Decode(data []byte, gvk *schema.GroupVersionKind, into Object) (Object, *schema.GroupVersionKind, error) {
	return nil, nil, fmt.Errorf("decoding is not allowed for this codec: %v", reflect.TypeOf(n.Encoder))
}

// NewParameterCodec creates a ParameterCodec capable of transforming url values into versioned objects and back.
func NewParameterCodec(scheme *Scheme) ParameterCodec {
	return &parameterCodec{
		typer:     scheme,
		convertor: scheme,
		creator:   scheme,
		defaulter: scheme,
	}
}

// parameterCodec implements conversion to and from query parameters and objects.
type parameterCodec struct {
	typer     ObjectTyper
	convertor ObjectConvertor
	creator   ObjectCreater
	defaulter ObjectDefaulter
}

var _ ParameterCodec = &parameterCodec{}

// DecodeParameters converts the provided url.Values into an object of type From with the kind of into, and then
// converts that object to into (if necessary). Returns an error if the operation cannot be completed.
func (c *parameterCodec) DecodeParameters(parameters url.Values, from schema.GroupVersion, into Object) error {
	if len(parameters) == 0 {
		return nil
	}
	targetGVKs, _, err := c.typer.ObjectKinds(into)
	if err != nil {
		return err
	}
	for i := range targetGVKs {
		if targetGVKs[i].GroupVersion() == from {
			if err := c.convertor.Convert(&parameters, into, nil); err != nil {
				return err
			}
			// in the case where we going into the same object we're receiving, default on the outbound object
			if c.defaulter != nil {
				c.defaulter.Default(into)
			}
			return nil
		}
	}

	input, err := c.creator.New(from.WithKind(targetGVKs[0].Kind))
	if err != nil {
		return err
	}
	if err := c.convertor.Convert(&parameters, input, nil); err != nil {
		return err
	}
	// if we have defaulter, default the input before converting to output
	if c.defaulter != nil {
		c.defaulter.Default(input)
	}
	return c.convertor.Convert(input, into, nil)
}

// EncodeParameters converts the provided object into the to version, then converts that object to url.Values.
// Returns an error if conversion is not possible.
func (c *parameterCodec) EncodeParameters(obj Object, to schema.GroupVersion) (url.Values, error) {
	gvks, _, err := c.typer.ObjectKinds(obj)
	if err != nil {
		return nil, err
	}
	gvk := gvks[0]
	if to != gvk.GroupVersion() {
		out, err := c.convertor.ConvertToVersion(obj, to)
		if err != nil {
			return nil, err
		}
		obj = out
	}
	return queryparams.Convert(obj)
}

type base64Serializer struct {
	Encoder
	Decoder

	identifier Identifier
}

func NewBase64Serializer(e Encoder, d Decoder) Serializer {
	return &base64Serializer{
		Encoder:    e,
		Decoder:    d,
		identifier: identifier(e),
	}
}

func identifier(e Encoder) Identifier {
	result := map[string]string{
		"name": "base64",
	}
	if e != nil {
		result["encoder"] = string(e.Identifier())
	}
	identifier, err := json.Marshal(result)
	if err != nil {
		//nolint:logcheck // Hopefully not reached, it would kill the process.
		klog.Fatalf("Failed marshaling identifier for base64Serializer: %v", err)
	}
	return Identifier(identifier)
}

func (s base64Serializer) Encode(obj Object, stream io.Writer) error {
	if co, ok := obj.(CacheableObject); ok {
		return co.CacheEncode(s.Identifier(), s.doEncode, stream)
	}
	return s.doEncode(obj, stream)
}

func (s base64Serializer) doEncode(obj Object, stream io.Writer) error {
	e := base64.NewEncoder(base64.StdEncoding, stream)
	err := s.Encoder.Encode(obj, e)
	e.Close()
	return err
}

// Identifier implements runtime.Encoder interface.
func (s base64Serializer) Identifier() Identifier {
	return s.identifier
}

func (s base64Serializer) Decode(data []byte, defaults *schema.GroupVersionKind, into Object) (Object, *schema.GroupVersionKind, error) {
	out := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	n, err := base64.StdEncoding.Decode(out, data)
	if err != nil {
		return nil, nil, err
	}
	return s.Decoder.Decode(out[:n], defaults, into)
}

// SerializerInfoForMediaType returns the first info in types that has a matching media type (which cannot
// include media-type parameters), or the first info with an empty media type, or false if no type matches.
func SerializerInfoForMediaType(types []SerializerInfo, mediaType string) (SerializerInfo, bool) {
	for _, info := range types {
		if info.MediaType == mediaType {
			return info, true
		}
	}
	for _, info := range types {
		if len(info.MediaType) == 0 {
			return info, true
		}
	}
	return SerializerInfo{}, false
}

var (
	// InternalGroupVersioner will always prefer the internal version for a given group version kind.
	InternalGroupVersioner GroupVersioner = internalGroupVersioner{}
	// DisabledGroupVersioner will reject all kinds passed to it.
	DisabledGroupVersioner GroupVersioner = disabledGroupVersioner{}
)

const (
	internalGroupVersionerIdentifier = "internal"
	disabledGroupVersionerIdentifier = "disabled"
)

type internalGroupVersioner struct{}

// KindForGroupVersionKinds returns an internal Kind if one is found, or converts the first provided kind to the internal version.
func (internalGroupVersioner) KindForGroupVersionKinds(kinds []schema.GroupVersionKind) (schema.GroupVersionKind, bool) {
	for _, kind := range kinds {
		if kind.Version == APIVersionInternal {
			return kind, true
		}
	}
	for _, kind := range kinds {
		return schema.GroupVersionKind{Group: kind.Group, Version: APIVersionInternal, Kind: kind.Kind}, true
	}
	return schema.GroupVersionKind{}, false
}

// Identifier implements GroupVersioner interface.
func (internalGroupVersioner) Identifier() string {
	return internalGroupVersionerIdentifier
}

type disabledGroupVersioner struct{}

// KindForGroupVersionKinds returns false for any input.
func (disabledGroupVersioner) KindForGroupVersionKinds(kinds []schema.GroupVersionKind) (schema.GroupVersionKind, bool) {
	return schema.GroupVersionKind{}, false
}

// Identifier implements GroupVersioner interface.
func (disabledGroupVersioner) Identifier() string {
	return disabledGroupVersionerIdentifier
}

// Assert that schema.GroupVersion and GroupVersions implement GroupVersioner
var _ GroupVersioner = schema.GroupVersion{}
var _ GroupVersioner = schema.GroupVersions{}
var _ GroupVersioner = multiGroupVersioner{}

type multiGroupVersioner struct {
	target             schema.GroupVersion
	acceptedGroupKinds []schema.GroupKind
	coerce             bool
}

// NewMultiGroupVersioner returns the provided group version for any kind that matches one of the provided group kinds.
// Kind may be empty in the provided group kind, in which case any kind will match.
func NewMultiGroupVersioner(gv schema.GroupVersion, groupKinds ...schema.GroupKind) GroupVersioner {
	if len(groupKinds) == 0 || (len(groupKinds) == 1 && groupKinds[0].Group == gv.Group) {
		return gv
	}
	return multiGroupVersioner{target: gv, acceptedGroupKinds: groupKinds}
}

// NewCoercingMultiGroupVersioner returns the provided group version for any incoming kind.
// Incoming kinds that match the provided groupKinds are preferred.
// Kind may be empty in the provided group kind, in which case any kind will match.
// Examples:
//
//	gv=mygroup/__internal, groupKinds=mygroup/Foo, anothergroup/Bar
//	KindForGroupVersionKinds(yetanother/v1/Baz, anothergroup/v1/Bar) -> mygroup/__internal/Bar (matched preferred group/kind)
//
//	gv=mygroup/__internal, groupKinds=mygroup, anothergroup
//	KindForGroupVersionKinds(yetanother/v1/Baz, anothergroup/v1/Bar) -> mygroup/__internal/Bar (matched preferred group)
//
//	gv=mygroup/__internal, groupKinds=mygroup, anothergroup
//	KindForGroupVersionKinds(yetanother/v1/Baz, yetanother/v1/Bar) -> mygroup/__internal/Baz (no preferred group/kind match, uses first kind in list)
func NewCoercingMultiGroupVersioner(gv schema.GroupVersion, groupKinds ...schema.GroupKind) GroupVersioner {
	return multiGroupVersioner{target: gv, acceptedGroupKinds: groupKinds, coerce: true}
}

// KindForGroupVersionKinds returns the target group version if any kind matches any of the original group kinds. It will
// use the originating kind where possible.
func (v multiGroupVersioner) KindForGroupVersionKinds(kinds []schema.GroupVersionKind) (schema.GroupVersionKind, bool) {
	for _, src := range kinds {
		for _, kind := range v.acceptedGroupKinds {
			if kind.Group != src.Group {
				continue
			}
			if len(kind.Kind) > 0 && kind.Kind != src.Kind {
				continue
			}
			return v.target.WithKind(src.Kind), true
		}
	}
	if v.coerce && len(kinds) > 0 {
		return v.target.WithKind(kinds[0].Kind), true
	}
	return schema.GroupVersionKind{}, false
}

// Identifier implements GroupVersioner interface.
func (v multiGroupVersioner) Identifier() string {
	groupKinds := make([]string, 0, len(v.acceptedGroupKinds))
	for _, gk := range v.acceptedGroupKinds {
		groupKinds = append(groupKinds, gk.String())
	}
	result := map[string]string{
		"name":     "multi",
		"target":   v.target.String(),
		"accepted": strings.Join(groupKinds, ","),
		"coerce":   strconv.FormatBool(v.coerce),
	}
	identifier, err := json.Marshal(result)
	if err != nil {
		//nolint:logcheck // Hopefully not reached, it would kill the process.
		klog.Fatalf("Failed marshaling Identifier for %#v: %v", v, err)
	}
	return string(identifier)
}
