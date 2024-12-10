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

package protobuf

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"reflect"

	"github.com/gogo/protobuf/proto"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	"k8s.io/apimachinery/pkg/util/framer"
	"k8s.io/klog/v2"
)

var (
	// protoEncodingPrefix serves as a magic number for an encoded protobuf message on this serializer. All
	// proto messages serialized by this schema will be preceded by the bytes 0x6b 0x38 0x73, with the fourth
	// byte being reserved for the encoding style. The only encoding style defined is 0x00, which means that
	// the rest of the byte stream is a message of type k8s.io.kubernetes.pkg.runtime.Unknown (proto2).
	//
	// See k8s.io/apimachinery/pkg/runtime/generated.proto for details of the runtime.Unknown message.
	//
	// This encoding scheme is experimental, and is subject to change at any time.
	protoEncodingPrefix = []byte{0x6b, 0x38, 0x73, 0x00}
)

type errNotMarshalable struct {
	t reflect.Type
}

func (e errNotMarshalable) Error() string {
	return fmt.Sprintf("object %v does not implement the protobuf marshalling interface and cannot be encoded to a protobuf message", e.t)
}

func (e errNotMarshalable) Status() metav1.Status {
	return metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    http.StatusNotAcceptable,
		Reason:  metav1.StatusReason("NotAcceptable"),
		Message: e.Error(),
	}
}

// IsNotMarshalable checks the type of error, returns a boolean true if error is not nil and not marshalable false otherwise
func IsNotMarshalable(err error) bool {
	_, ok := err.(errNotMarshalable)
	return err != nil && ok
}

// NewSerializer creates a Protobuf serializer that handles encoding versioned objects into the proper wire form. If a typer
// is passed, the encoded object will have group, version, and kind fields set. If typer is nil, the objects will be written
// as-is (any type info passed with the object will be used).
func NewSerializer(creater runtime.ObjectCreater, typer runtime.ObjectTyper) *Serializer {
	return &Serializer{
		prefix:  protoEncodingPrefix,
		creater: creater,
		typer:   typer,
	}
}

// Serializer handles encoding versioned objects into the proper wire form
type Serializer struct {
	prefix  []byte
	creater runtime.ObjectCreater
	typer   runtime.ObjectTyper
}

var _ runtime.Serializer = &Serializer{}
var _ runtime.EncoderWithAllocator = &Serializer{}
var _ recognizer.RecognizingDecoder = &Serializer{}

const serializerIdentifier runtime.Identifier = "protobuf"

// Decode attempts to convert the provided data into a protobuf message, extract the stored schema kind, apply the provided default
// gvk, and then load that data into an object matching the desired schema kind or the provided into. If into is *runtime.Unknown,
// the raw data will be extracted and no decoding will be performed. If into is not registered with the typer, then the object will
// be straight decoded using normal protobuf unmarshalling (the MarshalTo interface). If into is provided and the original data is
// not fully qualified with kind/version/group, the type of the into will be used to alter the returned gvk. On success or most
// errors, the method will return the calculated schema kind.
func (s *Serializer) Decode(originalData []byte, gvk *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	prefixLen := len(s.prefix)
	switch {
	case len(originalData) == 0:
		// TODO: treat like decoding {} from JSON with defaulting
		return nil, nil, fmt.Errorf("empty data")
	case len(originalData) < prefixLen || !bytes.Equal(s.prefix, originalData[:prefixLen]):
		return nil, nil, fmt.Errorf("provided data does not appear to be a protobuf message, expected prefix %v", s.prefix)
	case len(originalData) == prefixLen:
		// TODO: treat like decoding {} from JSON with defaulting
		return nil, nil, fmt.Errorf("empty body")
	}

	data := originalData[prefixLen:]
	unk := runtime.Unknown{}
	if err := unk.Unmarshal(data); err != nil {
		return nil, nil, err
	}

	actual := unk.GroupVersionKind()
	copyKindDefaults(&actual, gvk)

	if intoUnknown, ok := into.(*runtime.Unknown); ok && intoUnknown != nil {
		*intoUnknown = unk
		if ok, _, _ := s.RecognizesData(unk.Raw); ok {
			intoUnknown.ContentType = runtime.ContentTypeProtobuf
		}
		return intoUnknown, &actual, nil
	}

	if into != nil {
		types, _, err := s.typer.ObjectKinds(into)
		switch {
		case runtime.IsNotRegisteredError(err):
			pb, ok := into.(proto.Message)
			if !ok {
				return nil, &actual, errNotMarshalable{reflect.TypeOf(into)}
			}
			if err := proto.Unmarshal(unk.Raw, pb); err != nil {
				return nil, &actual, err
			}
			return into, &actual, nil
		case err != nil:
			return nil, &actual, err
		default:
			copyKindDefaults(&actual, &types[0])
			// if the result of defaulting did not set a version or group, ensure that at least group is set
			// (copyKindDefaults will not assign Group if version is already set). This guarantees that the group
			// of into is set if there is no better information from the caller or object.
			if len(actual.Version) == 0 && len(actual.Group) == 0 {
				actual.Group = types[0].Group
			}
		}
	}

	if len(actual.Kind) == 0 {
		return nil, &actual, runtime.NewMissingKindErr(fmt.Sprintf("%#v", unk.TypeMeta))
	}
	if len(actual.Version) == 0 {
		return nil, &actual, runtime.NewMissingVersionErr(fmt.Sprintf("%#v", unk.TypeMeta))
	}

	return unmarshalToObject(s.typer, s.creater, &actual, into, unk.Raw)
}

// EncodeWithAllocator writes an object to the provided writer.
// In addition, it allows for providing a memory allocator for efficient memory usage during object serialization.
func (s *Serializer) EncodeWithAllocator(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	return s.encode(obj, w, memAlloc)
}

// Encode serializes the provided object to the given writer.
func (s *Serializer) Encode(obj runtime.Object, w io.Writer) error {
	return s.encode(obj, w, &runtime.SimpleAllocator{})
}

func (s *Serializer) encode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(s.Identifier(), func(obj runtime.Object, w io.Writer) error { return s.doEncode(obj, w, memAlloc) }, w)
	}
	return s.doEncode(obj, w, memAlloc)
}

func (s *Serializer) doEncode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	if memAlloc == nil {
		//nolint:logcheck // Hopefully not reached.
		klog.Error("a mandatory memory allocator wasn't provided, this might have a negative impact on performance, check invocations of EncodeWithAllocator method, falling back on runtime.SimpleAllocator")
		memAlloc = &runtime.SimpleAllocator{}
	}
	prefixSize := uint64(len(s.prefix))

	var unk runtime.Unknown
	switch t := obj.(type) {
	case *runtime.Unknown:
		estimatedSize := prefixSize + uint64(t.Size())
		data := memAlloc.Allocate(estimatedSize)
		i, err := t.MarshalTo(data[prefixSize:])
		if err != nil {
			return err
		}
		copy(data, s.prefix)
		_, err = w.Write(data[:prefixSize+uint64(i)])
		return err
	default:
		kind := obj.GetObjectKind().GroupVersionKind()
		unk = runtime.Unknown{
			TypeMeta: runtime.TypeMeta{
				Kind:       kind.Kind,
				APIVersion: kind.GroupVersion().String(),
			},
		}
	}

	switch t := obj.(type) {
	case bufferedMarshaller:
		// this path performs a single allocation during write only when the Allocator wasn't provided
		// it also requires the caller to implement the more efficient Size and MarshalToSizedBuffer methods
		encodedSize := uint64(t.Size())
		estimatedSize := prefixSize + estimateUnknownSize(&unk, encodedSize)
		data := memAlloc.Allocate(estimatedSize)

		i, err := unk.NestedMarshalTo(data[prefixSize:], t, encodedSize)
		if err != nil {
			return err
		}

		copy(data, s.prefix)

		_, err = w.Write(data[:prefixSize+uint64(i)])
		return err

	case proto.Marshaler:
		// this path performs extra allocations
		data, err := t.Marshal()
		if err != nil {
			return err
		}
		unk.Raw = data

		estimatedSize := prefixSize + uint64(unk.Size())
		data = memAlloc.Allocate(estimatedSize)

		i, err := unk.MarshalTo(data[prefixSize:])
		if err != nil {
			return err
		}

		copy(data, s.prefix)

		_, err = w.Write(data[:prefixSize+uint64(i)])
		return err

	default:
		// TODO: marshal with a different content type and serializer (JSON for third party objects)
		return errNotMarshalable{reflect.TypeOf(obj)}
	}
}

// Identifier implements runtime.Encoder interface.
func (s *Serializer) Identifier() runtime.Identifier {
	return serializerIdentifier
}

// RecognizesData implements the RecognizingDecoder interface.
func (s *Serializer) RecognizesData(data []byte) (bool, bool, error) {
	return bytes.HasPrefix(data, s.prefix), false, nil
}

// copyKindDefaults defaults dst to the value in src if dst does not have a value set.
func copyKindDefaults(dst, src *schema.GroupVersionKind) {
	if src == nil {
		return
	}
	// apply kind and version defaulting from provided default
	if len(dst.Kind) == 0 {
		dst.Kind = src.Kind
	}
	if len(dst.Version) == 0 && len(src.Version) > 0 {
		dst.Group = src.Group
		dst.Version = src.Version
	}
}

// bufferedMarshaller describes a more efficient marshalling interface that can avoid allocating multiple
// byte buffers by pre-calculating the size of the final buffer needed.
type bufferedMarshaller interface {
	proto.Sizer
	runtime.ProtobufMarshaller
}

// Like bufferedMarshaller, but is able to marshal backwards, which is more efficient since it doesn't call Size() as frequently.
type bufferedReverseMarshaller interface {
	proto.Sizer
	runtime.ProtobufReverseMarshaller
}

// estimateUnknownSize returns the expected bytes consumed by a given runtime.Unknown
// object with a nil RawJSON struct and the expected size of the provided buffer. The
// returned size will not be correct if RawJSOn is set on unk.
func estimateUnknownSize(unk *runtime.Unknown, byteSize uint64) uint64 {
	size := uint64(unk.Size())
	// protobuf uses 1 byte for the tag, a varint for the length of the array (at most 8 bytes - uint64 - here),
	// and the size of the array.
	size += 1 + 8 + byteSize
	return size
}

// NewRawSerializer creates a Protobuf serializer that handles encoding versioned objects into the proper wire form. If typer
// is not nil, the object has the group, version, and kind fields set. This serializer does not provide type information for the
// encoded object, and thus is not self describing (callers must know what type is being described in order to decode).
//
// This encoding scheme is experimental, and is subject to change at any time.
func NewRawSerializer(creater runtime.ObjectCreater, typer runtime.ObjectTyper) *RawSerializer {
	return &RawSerializer{
		creater: creater,
		typer:   typer,
	}
}

// RawSerializer encodes and decodes objects without adding a runtime.Unknown wrapper (objects are encoded without identifying
// type).
type RawSerializer struct {
	creater runtime.ObjectCreater
	typer   runtime.ObjectTyper
}

var _ runtime.Serializer = &RawSerializer{}

const rawSerializerIdentifier runtime.Identifier = "raw-protobuf"

// Decode attempts to convert the provided data into a protobuf message, extract the stored schema kind, apply the provided default
// gvk, and then load that data into an object matching the desired schema kind or the provided into. If into is *runtime.Unknown,
// the raw data will be extracted and no decoding will be performed. If into is not registered with the typer, then the object will
// be straight decoded using normal protobuf unmarshalling (the MarshalTo interface). If into is provided and the original data is
// not fully qualified with kind/version/group, the type of the into will be used to alter the returned gvk. On success or most
// errors, the method will return the calculated schema kind.
func (s *RawSerializer) Decode(originalData []byte, gvk *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	if into == nil {
		return nil, nil, fmt.Errorf("this serializer requires an object to decode into: %#v", s)
	}

	if len(originalData) == 0 {
		// TODO: treat like decoding {} from JSON with defaulting
		return nil, nil, fmt.Errorf("empty data")
	}
	data := originalData

	actual := &schema.GroupVersionKind{}
	copyKindDefaults(actual, gvk)

	if intoUnknown, ok := into.(*runtime.Unknown); ok && intoUnknown != nil {
		intoUnknown.Raw = data
		intoUnknown.ContentEncoding = ""
		intoUnknown.ContentType = runtime.ContentTypeProtobuf
		intoUnknown.SetGroupVersionKind(*actual)
		return intoUnknown, actual, nil
	}

	types, _, err := s.typer.ObjectKinds(into)
	switch {
	case runtime.IsNotRegisteredError(err):
		pb, ok := into.(proto.Message)
		if !ok {
			return nil, actual, errNotMarshalable{reflect.TypeOf(into)}
		}
		if err := proto.Unmarshal(data, pb); err != nil {
			return nil, actual, err
		}
		return into, actual, nil
	case err != nil:
		return nil, actual, err
	default:
		copyKindDefaults(actual, &types[0])
		// if the result of defaulting did not set a version or group, ensure that at least group is set
		// (copyKindDefaults will not assign Group if version is already set). This guarantees that the group
		// of into is set if there is no better information from the caller or object.
		if len(actual.Version) == 0 && len(actual.Group) == 0 {
			actual.Group = types[0].Group
		}
	}

	if len(actual.Kind) == 0 {
		return nil, actual, runtime.NewMissingKindErr("<protobuf encoded body - must provide default type>")
	}
	if len(actual.Version) == 0 {
		return nil, actual, runtime.NewMissingVersionErr("<protobuf encoded body - must provide default type>")
	}

	return unmarshalToObject(s.typer, s.creater, actual, into, data)
}

// unmarshalToObject is the common code between decode in the raw and normal serializer.
func unmarshalToObject(typer runtime.ObjectTyper, creater runtime.ObjectCreater, actual *schema.GroupVersionKind, into runtime.Object, data []byte) (runtime.Object, *schema.GroupVersionKind, error) {
	// use the target if necessary
	obj, err := runtime.UseOrCreateObject(typer, creater, *actual, into)
	if err != nil {
		return nil, actual, err
	}

	pb, ok := obj.(proto.Message)
	if !ok {
		return nil, actual, errNotMarshalable{reflect.TypeOf(obj)}
	}
	if err := proto.Unmarshal(data, pb); err != nil {
		return nil, actual, err
	}
	if actual != nil {
		obj.GetObjectKind().SetGroupVersionKind(*actual)
	}
	return obj, actual, nil
}

// Encode serializes the provided object to the given writer. Overrides is ignored.
func (s *RawSerializer) Encode(obj runtime.Object, w io.Writer) error {
	return s.encode(obj, w, &runtime.SimpleAllocator{})
}

// EncodeWithAllocator writes an object to the provided writer.
// In addition, it allows for providing a memory allocator for efficient memory usage during object serialization.
func (s *RawSerializer) EncodeWithAllocator(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	return s.encode(obj, w, memAlloc)
}

func (s *RawSerializer) encode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(s.Identifier(), func(obj runtime.Object, w io.Writer) error { return s.doEncode(obj, w, memAlloc) }, w)
	}
	return s.doEncode(obj, w, memAlloc)
}

func (s *RawSerializer) doEncode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	if memAlloc == nil {
		//nolint:logcheck // Hopefully not reached.
		klog.Error("a mandatory memory allocator wasn't provided, this might have a negative impact on performance, check invocations of EncodeWithAllocator method, falling back on runtime.SimpleAllocator")
		memAlloc = &runtime.SimpleAllocator{}
	}
	switch t := obj.(type) {
	case bufferedReverseMarshaller:
		// this path performs a single allocation during write only when the Allocator wasn't provided
		// it also requires the caller to implement the more efficient Size and MarshalToSizedBuffer methods
		encodedSize := uint64(t.Size())
		data := memAlloc.Allocate(encodedSize)

		n, err := t.MarshalToSizedBuffer(data)
		if err != nil {
			return err
		}
		_, err = w.Write(data[:n])
		return err

	case bufferedMarshaller:
		// this path performs a single allocation during write only when the Allocator wasn't provided
		// it also requires the caller to implement the more efficient Size and MarshalTo methods
		encodedSize := uint64(t.Size())
		data := memAlloc.Allocate(encodedSize)

		n, err := t.MarshalTo(data)
		if err != nil {
			return err
		}
		_, err = w.Write(data[:n])
		return err

	case proto.Marshaler:
		// this path performs extra allocations
		data, err := t.Marshal()
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err

	default:
		return errNotMarshalable{reflect.TypeOf(obj)}
	}
}

// Identifier implements runtime.Encoder interface.
func (s *RawSerializer) Identifier() runtime.Identifier {
	return rawSerializerIdentifier
}

// LengthDelimitedFramer is exported variable of type lengthDelimitedFramer
var LengthDelimitedFramer = lengthDelimitedFramer{}

// Provides length delimited frame reader and writer methods
type lengthDelimitedFramer struct{}

// NewFrameWriter implements stream framing for this serializer
func (lengthDelimitedFramer) NewFrameWriter(w io.Writer) io.Writer {
	return framer.NewLengthDelimitedFrameWriter(w)
}

// NewFrameReader implements stream framing for this serializer
func (lengthDelimitedFramer) NewFrameReader(r io.ReadCloser) io.ReadCloser {
	return framer.NewLengthDelimitedFrameReader(r)
}
