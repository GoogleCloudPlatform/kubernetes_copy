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

package versioning

import (
	"encoding/json"
	"io"
	"reflect"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

// NewDefaultingCodecForScheme is a convenience method for callers that are using a scheme.
func NewDefaultingCodecForScheme(
	// TODO: I should be a scheme interface?
	scheme *runtime.Scheme,
	encoder runtime.Encoder,
	decoder runtime.Decoder,
	encodeVersion runtime.GroupVersioner,
	decodeVersion runtime.GroupVersioner,
) runtime.Codec {
	return NewCodec(encoder, decoder, runtime.UnsafeObjectConvertor(scheme), scheme, scheme, scheme, encodeVersion, decodeVersion, scheme.Name())
}

// NewCodec takes objects in their internal versions and converts them to external versions before
// serializing them. It assumes the serializer provided to it only deals with external versions.
// This class is also a serializer, but is generally used with a specific version.
func NewCodec(
	encoder runtime.Encoder,
	decoder runtime.Decoder,
	convertor runtime.ObjectConvertor,
	creater runtime.ObjectCreater,
	typer runtime.ObjectTyper,
	defaulter runtime.ObjectDefaulter,
	encodeVersion runtime.GroupVersioner,
	decodeVersion runtime.GroupVersioner,
	originalSchemeName string,
) runtime.Codec {
	internal := &codec{
		encoder:   encoder,
		decoder:   decoder,
		convertor: convertor,
		creater:   creater,
		typer:     typer,
		defaulter: defaulter,

		encodeVersion: encodeVersion,
		decodeVersion: decodeVersion,

		identifier: identifier(encodeVersion, encoder),

		originalSchemeName: originalSchemeName,
	}
	return internal
}

type codec struct {
	encoder   runtime.Encoder
	decoder   runtime.Decoder
	convertor runtime.ObjectConvertor
	creater   runtime.ObjectCreater
	typer     runtime.ObjectTyper
	defaulter runtime.ObjectDefaulter

	encodeVersion runtime.GroupVersioner
	decodeVersion runtime.GroupVersioner

	identifier runtime.Identifier

	// originalSchemeName is optional, but when filled in it holds the name of the scheme from which this codec originates
	originalSchemeName string
}

var _ runtime.EncoderWithAllocator = &codec{}

var identifiersMap sync.Map

type codecIdentifier struct {
	EncodeGV string `json:"encodeGV,omitempty"`
	Encoder  string `json:"encoder,omitempty"`
	Name     string `json:"name,omitempty"`
}

// identifier computes Identifier of Encoder based on codec parameters.
func identifier(encodeGV runtime.GroupVersioner, encoder runtime.Encoder) runtime.Identifier {
	result := codecIdentifier{
		Name: "versioning",
	}

	if encodeGV != nil {
		result.EncodeGV = encodeGV.Identifier()
	}
	if encoder != nil {
		result.Encoder = string(encoder.Identifier())
	}
	if id, ok := identifiersMap.Load(result); ok {
		return id.(runtime.Identifier)
	}
	identifier, err := json.Marshal(result)
	if err != nil {
		//nolint:logcheck // Let's hope this never gets reached. It would kill the process.
		klog.Fatalf("Failed marshaling identifier for codec: %v", err)
	}
	identifiersMap.Store(result, runtime.Identifier(identifier))
	return runtime.Identifier(identifier)
}

// Decode attempts a decode of the object, then tries to convert it to the internal version. If into is provided and the decoding is
// successful, the returned runtime.Object will be the value passed as into. Note that this may bypass conversion if you pass an
// into that matches the serialized version.
func (c *codec) Decode(data []byte, defaultGVK *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	// If the into object is unstructured and expresses an opinion about its group/version,
	// create a new instance of the type so we always exercise the conversion path (skips short-circuiting on `into == obj`)
	decodeInto := into
	if into != nil {
		if _, ok := into.(runtime.Unstructured); ok && !into.GetObjectKind().GroupVersionKind().GroupVersion().Empty() {
			decodeInto = reflect.New(reflect.TypeOf(into).Elem()).Interface().(runtime.Object)
		}
	}

	var strictDecodingErrs []error
	obj, gvk, err := c.decoder.Decode(data, defaultGVK, decodeInto)
	if err != nil {
		if strictErr, ok := runtime.AsStrictDecodingError(err); obj != nil && ok {
			// save the strictDecodingError and let the caller decide what to do with it
			strictDecodingErrs = append(strictDecodingErrs, strictErr.Errors()...)
		} else {
			return nil, gvk, err
		}
	}

	if d, ok := obj.(runtime.NestedObjectDecoder); ok {
		if err := d.DecodeNestedObjects(runtime.WithoutVersionDecoder{Decoder: c.decoder}); err != nil {
			if strictErr, ok := runtime.AsStrictDecodingError(err); ok {
				// save the strictDecodingError let and the caller decide what to do with it
				strictDecodingErrs = append(strictDecodingErrs, strictErr.Errors()...)
			} else {
				return nil, gvk, err

			}
		}
	}

	// aggregate the strict decoding errors into one
	var strictDecodingErr error
	if len(strictDecodingErrs) > 0 {
		strictDecodingErr = runtime.NewStrictDecodingError(strictDecodingErrs)
	}
	// if we specify a target, use generic conversion.
	if into != nil {
		// perform defaulting if requested
		if c.defaulter != nil {
			c.defaulter.Default(obj)
		}

		// Short-circuit conversion if the into object is same object
		if into == obj {
			return into, gvk, strictDecodingErr
		}

		if err := c.convertor.Convert(obj, into, c.decodeVersion); err != nil {
			return nil, gvk, err
		}

		return into, gvk, strictDecodingErr
	}

	// perform defaulting if requested
	if c.defaulter != nil {
		c.defaulter.Default(obj)
	}

	out, err := c.convertor.ConvertToVersion(obj, c.decodeVersion)
	if err != nil {
		return nil, gvk, err
	}
	return out, gvk, strictDecodingErr
}

// EncodeWithAllocator ensures the provided object is output in the appropriate group and version, invoking
// conversion if necessary. Unversioned objects (according to the ObjectTyper) are output as is.
// In addition, it allows for providing a memory allocator for efficient memory usage during object serialization.
func (c *codec) EncodeWithAllocator(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	return c.encode(obj, w, memAlloc)
}

// Encode ensures the provided object is output in the appropriate group and version, invoking
// conversion if necessary. Unversioned objects (according to the ObjectTyper) are output as is.
func (c *codec) Encode(obj runtime.Object, w io.Writer) error {
	return c.encode(obj, w, nil)
}

func (c *codec) encode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(c.Identifier(), func(obj runtime.Object, w io.Writer) error { return c.doEncode(obj, w, memAlloc) }, w)
	}
	return c.doEncode(obj, w, memAlloc)
}

func (c *codec) doEncode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	encodeFn := c.encoder.Encode
	if memAlloc != nil {
		if encoder, supportsAllocator := c.encoder.(runtime.EncoderWithAllocator); supportsAllocator {
			encodeFn = func(obj runtime.Object, w io.Writer) error {
				return encoder.EncodeWithAllocator(obj, w, memAlloc)
			}
		} else {
			//nolint:logcheck // Hopefully not reached.
			klog.V(6).Infof("a memory allocator was provided but the encoder %s doesn't implement the runtime.EncoderWithAllocator, using regular encoder.Encode method", c.encoder.Identifier())
		}
	}
	switch obj := obj.(type) {
	case *runtime.Unknown:
		return encodeFn(obj, w)
	case runtime.Unstructured:
		// An unstructured list can contain objects of multiple group version kinds. don't short-circuit just
		// because the top-level type matches our desired destination type. actually send the object to the converter
		// to give it a chance to convert the list items if needed.
		if _, ok := obj.(*unstructured.UnstructuredList); !ok {
			// avoid conversion roundtrip if GVK is the right one already or is empty (yes, this is a hack, but the old behaviour we rely on in kubectl)
			objGVK := obj.GetObjectKind().GroupVersionKind()
			if len(objGVK.Version) == 0 {
				return encodeFn(obj, w)
			}
			targetGVK, ok := c.encodeVersion.KindForGroupVersionKinds([]schema.GroupVersionKind{objGVK})
			if !ok {
				return runtime.NewNotRegisteredGVKErrForTarget(c.originalSchemeName, objGVK, c.encodeVersion)
			}
			if targetGVK == objGVK {
				return encodeFn(obj, w)
			}
		}
	}

	gvks, isUnversioned, err := c.typer.ObjectKinds(obj)
	if err != nil {
		return err
	}

	objectKind := obj.GetObjectKind()
	old := objectKind.GroupVersionKind()
	// restore the old GVK after encoding
	defer objectKind.SetGroupVersionKind(old)

	if c.encodeVersion == nil || isUnversioned {
		if e, ok := obj.(runtime.NestedObjectEncoder); ok {
			if err := e.EncodeNestedObjects(runtime.WithVersionEncoder{Encoder: c.encoder, ObjectTyper: c.typer}); err != nil {
				return err
			}
		}
		objectKind.SetGroupVersionKind(gvks[0])
		return encodeFn(obj, w)
	}

	// Perform a conversion if necessary
	out, err := c.convertor.ConvertToVersion(obj, c.encodeVersion)
	if err != nil {
		return err
	}

	if e, ok := out.(runtime.NestedObjectEncoder); ok {
		if err := e.EncodeNestedObjects(runtime.WithVersionEncoder{Version: c.encodeVersion, Encoder: c.encoder, ObjectTyper: c.typer}); err != nil {
			return err
		}
	}

	// Conversion is responsible for setting the proper group, version, and kind onto the outgoing object
	return encodeFn(out, w)
}

// Identifier implements runtime.Encoder interface.
func (c *codec) Identifier() runtime.Identifier {
	return c.identifier
}
