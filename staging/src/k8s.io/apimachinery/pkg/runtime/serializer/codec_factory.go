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

package serializer

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	"k8s.io/apimachinery/pkg/runtime/serializer/versioning"
)

func newSerializersForScheme(scheme *runtime.Scheme, mf json.MetaFactory, options CodecFactoryOptions) []runtime.SerializerInfo {
	jsonSerializer := json.NewSerializerWithOptions(
		mf, scheme, scheme,
		json.SerializerOptions{Yaml: false, Pretty: false, Strict: options.Strict, StreamingCollectionsEncoding: options.StreamingCollectionsEncodingToJSON},
	)
	jsonSerializerType := runtime.SerializerInfo{
		MediaType:        runtime.ContentTypeJSON,
		MediaTypeType:    "application",
		MediaTypeSubType: "json",
		EncodesAsText:    true,
		Serializer:       jsonSerializer,
		StrictSerializer: json.NewSerializerWithOptions(
			mf, scheme, scheme,
			json.SerializerOptions{Yaml: false, Pretty: false, Strict: true, StreamingCollectionsEncoding: options.StreamingCollectionsEncodingToJSON},
		),
		StreamSerializer: &runtime.StreamSerializerInfo{
			EncodesAsText: true,
			Serializer:    jsonSerializer,
			Framer:        json.Framer,
		},
	}
	if options.Pretty {
		jsonSerializerType.PrettySerializer = json.NewSerializerWithOptions(
			mf, scheme, scheme,
			json.SerializerOptions{Yaml: false, Pretty: true, Strict: options.Strict},
		)
	}

	yamlSerializer := json.NewSerializerWithOptions(
		mf, scheme, scheme,
		json.SerializerOptions{Yaml: true, Pretty: false, Strict: options.Strict},
	)
	strictYAMLSerializer := json.NewSerializerWithOptions(
		mf, scheme, scheme,
		json.SerializerOptions{Yaml: true, Pretty: false, Strict: true},
	)
	protoSerializer := protobuf.NewSerializer(scheme, scheme)
	protoRawSerializer := protobuf.NewRawSerializer(scheme, scheme)

	serializers := []runtime.SerializerInfo{
		jsonSerializerType,
		{
			MediaType:        runtime.ContentTypeYAML,
			MediaTypeType:    "application",
			MediaTypeSubType: "yaml",
			EncodesAsText:    true,
			Serializer:       yamlSerializer,
			StrictSerializer: strictYAMLSerializer,
		},
		{
			MediaType:        runtime.ContentTypeProtobuf,
			MediaTypeType:    "application",
			MediaTypeSubType: "vnd.kubernetes.protobuf",
			Serializer:       protoSerializer,
			// note, strict decoding is unsupported for protobuf,
			// fall back to regular serializing
			StrictSerializer: protoSerializer,
			StreamSerializer: &runtime.StreamSerializerInfo{
				Serializer: protoRawSerializer,
				Framer:     protobuf.LengthDelimitedFramer,
			},
		},
	}

	for _, f := range options.serializers {
		serializers = append(serializers, f(scheme, scheme))
	}

	return serializers
}

// CodecFactory provides methods for retrieving codecs and serializers for specific
// versions and content types.
type CodecFactory struct {
	scheme    *runtime.Scheme
	universal runtime.Decoder
	accepts   []runtime.SerializerInfo

	legacySerializer runtime.Serializer
}

// CodecFactoryOptions holds the options for configuring CodecFactory behavior
type CodecFactoryOptions struct {
	// Strict configures all serializers in strict mode
	Strict bool
	// Pretty includes a pretty serializer along with the non-pretty one
	Pretty bool

	StreamingCollectionsEncodingToJSON bool

	serializers []func(runtime.ObjectCreater, runtime.ObjectTyper) runtime.SerializerInfo
}

// CodecFactoryOptionsMutator takes a pointer to an options struct and then modifies it.
// Functions implementing this type can be passed to the NewCodecFactory() constructor.
type CodecFactoryOptionsMutator func(*CodecFactoryOptions)

// EnablePretty enables including a pretty serializer along with the non-pretty one
func EnablePretty(options *CodecFactoryOptions) {
	options.Pretty = true
}

// DisablePretty disables including a pretty serializer along with the non-pretty one
func DisablePretty(options *CodecFactoryOptions) {
	options.Pretty = false
}

// EnableStrict enables configuring all serializers in strict mode
func EnableStrict(options *CodecFactoryOptions) {
	options.Strict = true
}

// DisableStrict disables configuring all serializers in strict mode
func DisableStrict(options *CodecFactoryOptions) {
	options.Strict = false
}

// WithSerializer configures a serializer to be supported in addition to the default serializers.
func WithSerializer(f func(runtime.ObjectCreater, runtime.ObjectTyper) runtime.SerializerInfo) CodecFactoryOptionsMutator {
	return func(options *CodecFactoryOptions) {
		options.serializers = append(options.serializers, f)
	}
}

func WithStreamingCollectionEncodingToJSON() CodecFactoryOptionsMutator {
	return func(options *CodecFactoryOptions) {
		options.StreamingCollectionsEncodingToJSON = true
	}
}

// NewCodecFactory provides methods for retrieving serializers for the supported wire formats
// and conversion wrappers to define preferred internal and external versions. In the future,
// as the internal version is used less, callers may instead use a defaulting serializer and
// only convert objects which are shared internally (Status, common API machinery).
//
// Mutators can be passed to change the CodecFactoryOptions before construction of the factory.
// It is recommended to explicitly pass mutators instead of relying on defaults.
// By default, Pretty is enabled -- this is conformant with previously supported behavior.
//
// TODO: allow other codecs to be compiled in?
// TODO: accept a scheme interface
func NewCodecFactory(scheme *runtime.Scheme, mutators ...CodecFactoryOptionsMutator) CodecFactory {
	options := CodecFactoryOptions{Pretty: true}
	for _, fn := range mutators {
		fn(&options)
	}

	serializers := newSerializersForScheme(scheme, json.DefaultMetaFactory, options)
	return newCodecFactory(scheme, serializers)
}

// newCodecFactory is a helper for testing that allows a different metafactory to be specified.
func newCodecFactory(scheme *runtime.Scheme, serializers []runtime.SerializerInfo) CodecFactory {
	decoders := make([]runtime.Decoder, 0, len(serializers))
	var accepts []runtime.SerializerInfo
	alreadyAccepted := make(map[string]struct{})

	var legacySerializer runtime.Serializer
	for _, d := range serializers {
		decoders = append(decoders, d.Serializer)
		if _, ok := alreadyAccepted[d.MediaType]; ok {
			continue
		}
		alreadyAccepted[d.MediaType] = struct{}{}

		acceptedSerializerShallowCopy := d
		if d.StreamSerializer != nil {
			cloned := *d.StreamSerializer
			acceptedSerializerShallowCopy.StreamSerializer = &cloned
		}
		accepts = append(accepts, acceptedSerializerShallowCopy)

		if d.MediaType == runtime.ContentTypeJSON {
			legacySerializer = d.Serializer
		}
	}
	if legacySerializer == nil {
		legacySerializer = serializers[0].Serializer
	}

	return CodecFactory{
		scheme:    scheme,
		universal: recognizer.NewDecoder(decoders...),

		accepts: accepts,

		legacySerializer: legacySerializer,
	}
}

// WithoutConversion returns a NegotiatedSerializer that performs no conversion, even if the
// caller requests it.
func (f CodecFactory) WithoutConversion() runtime.NegotiatedSerializer {
	return WithoutConversionCodecFactory{f}
}

// SupportedMediaTypes returns the RFC2046 media types that this factory has serializers for.
func (f CodecFactory) SupportedMediaTypes() []runtime.SerializerInfo {
	return f.accepts
}

// LegacyCodec encodes output to a given API versions, and decodes output into the internal form from
// any recognized source. The returned codec will always encode output to JSON. If a type is not
// found in the list of versions an error will be returned.
//
// This method is deprecated - clients and servers should negotiate a serializer by mime-type and
// invoke CodecForVersions. Callers that need only to read data should use UniversalDecoder().
//
// TODO: make this call exist only in pkg/api, and initialize it with the set of default versions.
// All other callers will be forced to request a Codec directly.
func (f CodecFactory) LegacyCodec(version ...schema.GroupVersion) runtime.Codec {
	return versioning.NewDefaultingCodecForScheme(f.scheme, f.legacySerializer, f.universal, schema.GroupVersions(version), runtime.InternalGroupVersioner)
}

// UniversalDeserializer can convert any stored data recognized by this factory into a Go object that satisfies
// runtime.Object. It does not perform conversion. It does not perform defaulting.
func (f CodecFactory) UniversalDeserializer() runtime.Decoder {
	return f.universal
}

// UniversalDecoder returns a runtime.Decoder capable of decoding all known API objects in all known formats. Used
// by clients that do not need to encode objects but want to deserialize API objects stored on disk. Only decodes
// objects in groups registered with the scheme. The GroupVersions passed may be used to select alternate
// versions of objects to return - by default, runtime.APIVersionInternal is used. If any versions are specified,
// unrecognized groups will be returned in the version they are encoded as (no conversion). This decoder performs
// defaulting.
//
// TODO: the decoder will eventually be removed in favor of dealing with objects in their versioned form
// TODO: only accept a group versioner
func (f CodecFactory) UniversalDecoder(versions ...schema.GroupVersion) runtime.Decoder {
	var versioner runtime.GroupVersioner
	if len(versions) == 0 {
		versioner = runtime.InternalGroupVersioner
	} else {
		versioner = schema.GroupVersions(versions)
	}
	return f.CodecForVersions(nil, f.universal, nil, versioner)
}

// CodecForVersions creates a codec with the provided serializer. If an object is decoded and its group is not in the list,
// it will default to runtime.APIVersionInternal. If encode is not specified for an object's group, the object is not
// converted. If encode or decode are nil, no conversion is performed.
func (f CodecFactory) CodecForVersions(encoder runtime.Encoder, decoder runtime.Decoder, encode runtime.GroupVersioner, decode runtime.GroupVersioner) runtime.Codec {
	// TODO: these are for backcompat, remove them in the future
	if encode == nil {
		encode = runtime.DisabledGroupVersioner
	}
	if decode == nil {
		decode = runtime.InternalGroupVersioner
	}
	return versioning.NewDefaultingCodecForScheme(f.scheme, encoder, decoder, encode, decode)
}

// DecoderToVersion returns a decoder that targets the provided group version.
func (f CodecFactory) DecoderToVersion(decoder runtime.Decoder, gv runtime.GroupVersioner) runtime.Decoder {
	return f.CodecForVersions(nil, decoder, nil, gv)
}

// EncoderForVersion returns an encoder that targets the provided group version.
func (f CodecFactory) EncoderForVersion(encoder runtime.Encoder, gv runtime.GroupVersioner) runtime.Encoder {
	return f.CodecForVersions(encoder, nil, gv, nil)
}

// WithoutConversionCodecFactory is a CodecFactory that will explicitly ignore requests to perform conversion.
// This wrapper is used while code migrates away from using conversion (such as external clients) and in the future
// will be unnecessary when we change the signature of NegotiatedSerializer.
type WithoutConversionCodecFactory struct {
	CodecFactory
}

// EncoderForVersion returns an encoder that does not do conversion, but does set the group version kind of the object
// when serialized.
func (f WithoutConversionCodecFactory) EncoderForVersion(serializer runtime.Encoder, version runtime.GroupVersioner) runtime.Encoder {
	return runtime.WithVersionEncoder{
		Version:     version,
		Encoder:     serializer,
		ObjectTyper: f.CodecFactory.scheme,
	}
}

// DecoderToVersion returns an decoder that does not do conversion.
func (f WithoutConversionCodecFactory) DecoderToVersion(serializer runtime.Decoder, _ runtime.GroupVersioner) runtime.Decoder {
	return runtime.WithoutVersionDecoder{
		Decoder: serializer,
	}
}
