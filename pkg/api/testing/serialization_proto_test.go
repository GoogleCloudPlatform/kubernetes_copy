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

package testing

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/gogo/protobuf/proto"
	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	_ "k8s.io/kubernetes/pkg/apis/extensions"
	_ "k8s.io/kubernetes/pkg/apis/extensions/v1beta1"
)

func TestUniversalDeserializer(t *testing.T) {
	expected := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test"}, TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}}
	d := legacyscheme.Codecs.UniversalDeserializer()
	for _, mediaType := range []string{"application/json", "application/yaml", "application/vnd.kubernetes.protobuf"} {
		info, ok := runtime.SerializerInfoForMediaType(legacyscheme.Codecs.SupportedMediaTypes(), mediaType)
		if !ok {
			t.Fatal(mediaType)
		}
		buf := &bytes.Buffer{}
		if err := info.Serializer.Encode(expected, buf); err != nil {
			t.Fatalf("%s: %v", mediaType, err)
		}
		obj, _, err := d.Decode(buf.Bytes(), &schema.GroupVersionKind{Kind: "Pod", Version: "v1"}, nil)
		if err != nil {
			t.Fatalf("%s: %v", mediaType, err)
		}
		if !apiequality.Semantic.DeepEqual(expected, obj) {
			t.Fatalf("%s: %#v", mediaType, obj)
		}
	}
}

func TestAllFieldsHaveTags(t *testing.T) {
	for gvk, obj := range legacyscheme.Scheme.AllKnownTypes() {
		if gvk.Version == runtime.APIVersionInternal {
			// internal versions are not serialized to protobuf
			continue
		}
		if gvk.Group == "componentconfig" {
			// component config is not serialized to protobuf
			continue
		}
		if err := fieldsHaveProtobufTags(obj); err != nil {
			t.Errorf("type %s as gvk %v is missing tags: %v", obj, gvk, err)
		}
	}
}

func fieldsHaveProtobufTags(obj reflect.Type) error {
	switch obj.Kind() {
	case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Array:
		return fieldsHaveProtobufTags(obj.Elem())
	case reflect.Struct:
		for i := 0; i < obj.NumField(); i++ {
			f := obj.Field(i)
			if f.Name == "TypeMeta" && f.Type.Name() == "TypeMeta" {
				// TypeMeta is not included in external protobuf because we use an envelope type with TypeMeta
				continue
			}
			if len(f.Tag.Get("json")) > 0 && len(f.Tag.Get("protobuf")) == 0 {
				return fmt.Errorf("field %s in %s has a 'json' tag but no protobuf tag", f.Name, obj)
			}
		}
	}
	return nil
}

func TestProtobufRoundTrip(t *testing.T) {
	obj := &v1.Pod{}
	fuzzer.FuzzerFor(FuzzerFuncs, rand.NewSource(benchmarkSeed), legacyscheme.Codecs).Fuzz(obj)
	// InitContainers are turned into annotations by conversion.
	obj.Spec.InitContainers = nil
	obj.Status.InitContainerStatuses = nil
	data, err := obj.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out := &v1.Pod{}
	if err := out.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	if !apiequality.Semantic.Equalities.DeepEqual(out, obj) {
		t.Logf("marshal\n%s", hex.Dump(data))
		t.Fatalf("Unmarshal is unequal\n%s", cmp.Diff(out, obj))
	}
}

// BenchmarkEncodeCodec measures the cost of performing a codec encode, which includes
// reflection (to clear APIVersion and Kind)
func BenchmarkEncodeCodecProtobuf(b *testing.B) {
	items := benchmarkItems(b)
	width := len(items)
	s := protobuf.NewSerializer(nil, nil, protobuf.SerializerOptions{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runtime.Encode(s, &items[i%width]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// BenchmarkEncodeCodecFromInternalProtobuf measures the cost of performing a codec encode,
// including conversions and any type setting. This is a "full" encode.
func BenchmarkEncodeCodecFromInternalProtobuf(b *testing.B) {
	items := benchmarkItems(b)
	width := len(items)
	encodable := make([]api.Pod, width)
	for i := range items {
		if err := legacyscheme.Scheme.Convert(&items[i], &encodable[i], nil); err != nil {
			b.Fatal(err)
		}
	}
	s := protobuf.NewSerializer(nil, nil, protobuf.SerializerOptions{})
	codec := legacyscheme.Codecs.EncoderForVersion(s, v1.SchemeGroupVersion)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runtime.Encode(codec, &encodable[i%width]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func BenchmarkEncodeProtobufGeneratedMarshal(b *testing.B) {
	items := benchmarkItems(b)
	width := len(items)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := items[i%width].Marshal(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func BenchmarkEncodeProtobufGeneratedMarshalList10(b *testing.B) {
	item := benchmarkItemsList(b, 10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := item.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func BenchmarkEncodeProtobufGeneratedMarshalList100(b *testing.B) {
	item := benchmarkItemsList(b, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := item.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func BenchmarkEncodeProtobufGeneratedMarshalList1000(b *testing.B) {
	item := benchmarkItemsList(b, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := item.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// BenchmarkDecodeCodecToInternalProtobuf measures the cost of performing a codec decode,
// including conversions and any type setting. This is a "full" decode.
func BenchmarkDecodeCodecToInternalProtobuf(b *testing.B) {
	items := benchmarkItems(b)
	width := len(items)
	s := protobuf.NewSerializer(legacyscheme.Scheme, legacyscheme.Scheme, protobuf.SerializerOptions{})
	encoder := legacyscheme.Codecs.EncoderForVersion(s, v1.SchemeGroupVersion)
	var encoded [][]byte
	for i := range items {
		data, err := runtime.Encode(encoder, &items[i])
		if err != nil {
			b.Fatal(err)
		}
		encoded = append(encoded, data)
	}

	decoder := legacyscheme.Codecs.DecoderToVersion(s, api.SchemeGroupVersion)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runtime.Decode(decoder, encoded[i%width]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// BenchmarkDecodeJSON provides a baseline for regular JSON decode performance
func BenchmarkDecodeIntoProtobuf(b *testing.B) {
	items := benchmarkItems(b)
	width := len(items)
	encoded := make([][]byte, width)
	for i := range items {
		data, err := (&items[i]).Marshal()
		if err != nil {
			b.Fatal(err)
		}
		encoded[i] = data
		validate := &v1.Pod{}
		if err := proto.Unmarshal(data, validate); err != nil {
			b.Fatalf("Failed to unmarshal %d: %v\n%#v", i, err, items[i])
		}
	}

	for i := 0; i < b.N; i++ {
		obj := v1.Pod{}
		if err := proto.Unmarshal(encoded[i%width], &obj); err != nil {
			b.Fatal(err)
		}
	}
}
