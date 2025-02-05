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

package roundtrip

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp" //nolint:depguard
	fuzz "github.com/google/gofuzz"
	flag "github.com/spf13/pflag"

	apitesting "k8s.io/apimachinery/pkg/api/apitesting"
	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metafuzzer "k8s.io/apimachinery/pkg/apis/meta/fuzzer"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/sets"
)

type InstallFunc func(scheme *runtime.Scheme)

// RoundTripTestForAPIGroup is convenient to call from your install package to make sure that a "bare" install of your group provides
// enough information to round trip
func RoundTripTestForAPIGroup(t *testing.T, installFn InstallFunc, fuzzingFuncs fuzzer.FuzzerFuncs) {
	scheme := runtime.NewScheme()
	installFn(scheme)

	RoundTripTestForScheme(t, scheme, fuzzingFuncs)
}

// RoundTripTestForScheme is convenient to call if you already have a scheme and want to make sure that its well-formed
func RoundTripTestForScheme(t *testing.T, scheme *runtime.Scheme, fuzzingFuncs fuzzer.FuzzerFuncs) {
	codecFactory := runtimeserializer.NewCodecFactory(scheme)
	f := fuzzer.FuzzerFor(
		fuzzer.MergeFuzzerFuncs(metafuzzer.Funcs, fuzzingFuncs),
		rand.NewSource(rand.Int63()),
		codecFactory,
	)
	RoundTripTypesWithoutProtobuf(t, scheme, codecFactory, f, nil)
}

// RoundTripProtobufTestForAPIGroup is convenient to call from your install package to make sure that a "bare" install of your group provides
// enough information to round trip
func RoundTripProtobufTestForAPIGroup(t *testing.T, installFn InstallFunc, fuzzingFuncs fuzzer.FuzzerFuncs) {
	scheme := runtime.NewScheme()
	installFn(scheme)

	RoundTripProtobufTestForScheme(t, scheme, fuzzingFuncs)
}

// RoundTripProtobufTestForScheme is convenient to call if you already have a scheme and want to make sure that its well-formed
func RoundTripProtobufTestForScheme(t *testing.T, scheme *runtime.Scheme, fuzzingFuncs fuzzer.FuzzerFuncs) {
	codecFactory := runtimeserializer.NewCodecFactory(scheme)
	fuzzer := fuzzer.FuzzerFor(
		fuzzer.MergeFuzzerFuncs(metafuzzer.Funcs, fuzzingFuncs),
		rand.NewSource(rand.Int63()),
		codecFactory,
	)
	RoundTripTypes(t, scheme, codecFactory, fuzzer, nil)
}

var FuzzIters = flag.Int("fuzz-iters", defaultFuzzIters, "How many fuzzing iterations to do.")

// globalNonRoundTrippableTypes are kinds that are effectively reserved across all GroupVersions
// They don't roundtrip
var globalNonRoundTrippableTypes = sets.NewString(
	"ExportOptions",
	"GetOptions",
	// WatchEvent does not include kind and version and can only be deserialized
	// implicitly (if the caller expects the specific object). The watch call defines
	// the schema by content type, rather than via kind/version included in each
	// object.
	"WatchEvent",
	// ListOptions is now part of the meta group
	"ListOptions",
	// Delete options is only read in metav1
	"DeleteOptions",
)

// GlobalNonRoundTrippableTypes returns the kinds that are effectively reserved across all GroupVersions.
// They don't roundtrip and thus can be excluded in any custom/downstream roundtrip tests
//
//	kinds := scheme.AllKnownTypes()
//	for gvk := range kinds {
//	    if roundtrip.GlobalNonRoundTrippableTypes().Has(gvk.Kind) {
//	        continue
//	    }
//	    t.Run(gvk.Group+"."+gvk.Version+"."+gvk.Kind, func(t *testing.T) {
//	        // roundtrip test
//	    })
//	}
func GlobalNonRoundTrippableTypes() sets.String {
	return sets.NewString(globalNonRoundTrippableTypes.List()...)
}

// RoundTripTypesWithoutProtobuf applies the round-trip test to all round-trippable Kinds
// in the scheme.  It will skip all the GroupVersionKinds in the skip list.
func RoundTripTypesWithoutProtobuf(t *testing.T, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool) {
	roundTripTypes(t, scheme, codecFactory, fuzzer, nonRoundTrippableTypes, true)
}

func RoundTripTypes(t *testing.T, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool) {
	roundTripTypes(t, scheme, codecFactory, fuzzer, nonRoundTrippableTypes, false)
}

func roundTripTypes(t *testing.T, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool, skipProtobuf bool) {
	for _, group := range groupsFromScheme(scheme) {
		t.Logf("starting group %q", group)
		internalVersion := schema.GroupVersion{Group: group, Version: runtime.APIVersionInternal}
		internalKindToGoType := scheme.KnownTypes(internalVersion)

		for kind := range internalKindToGoType {
			if globalNonRoundTrippableTypes.Has(kind) {
				continue
			}

			internalGVK := internalVersion.WithKind(kind)
			roundTripSpecificKind(t, internalGVK, scheme, codecFactory, fuzzer, nonRoundTrippableTypes, skipProtobuf)
		}

		t.Logf("finished group %q", group)
	}
}

// RoundTripExternalTypes applies the round-trip test to all external round-trippable Kinds
// in the scheme.  It will skip all the GroupVersionKinds in the nonRoundTripExternalTypes list .
func RoundTripExternalTypes(t *testing.T, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool) {
	kinds := scheme.AllKnownTypes()
	for gvk := range kinds {
		if gvk.Version == runtime.APIVersionInternal || globalNonRoundTrippableTypes.Has(gvk.Kind) {
			continue
		}
		t.Run(gvk.Group+"."+gvk.Version+"."+gvk.Kind, func(t *testing.T) {
			roundTripSpecificKind(t, gvk, scheme, codecFactory, fuzzer, nonRoundTrippableTypes, false)
		})
	}
}

// RoundTripExternalTypesWithoutProtobuf applies the round-trip test to all external round-trippable Kinds
// in the scheme.  It will skip all the GroupVersionKinds in the nonRoundTripExternalTypes list.
func RoundTripExternalTypesWithoutProtobuf(t *testing.T, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool) {
	kinds := scheme.AllKnownTypes()
	for gvk := range kinds {
		if gvk.Version == runtime.APIVersionInternal || globalNonRoundTrippableTypes.Has(gvk.Kind) {
			continue
		}
		t.Run(gvk.Group+"."+gvk.Version+"."+gvk.Kind, func(t *testing.T) {
			roundTripSpecificKind(t, gvk, scheme, codecFactory, fuzzer, nonRoundTrippableTypes, true)
		})
	}
}

func RoundTripSpecificKindWithoutProtobuf(t *testing.T, gvk schema.GroupVersionKind, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool) {
	roundTripSpecificKind(t, gvk, scheme, codecFactory, fuzzer, nonRoundTrippableTypes, true)
}

func RoundTripSpecificKind(t *testing.T, gvk schema.GroupVersionKind, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool) {
	roundTripSpecificKind(t, gvk, scheme, codecFactory, fuzzer, nonRoundTrippableTypes, false)
}

func roundTripSpecificKind(t *testing.T, gvk schema.GroupVersionKind, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, nonRoundTrippableTypes map[schema.GroupVersionKind]bool, skipProtobuf bool) {
	if nonRoundTrippableTypes[gvk] {
		t.Logf("skipping %v", gvk)
		return
	}

	// Try a few times, since runTest uses random values.
	for i := 0; i < *FuzzIters; i++ {
		if gvk.Version == runtime.APIVersionInternal {
			roundTripToAllExternalVersions(t, scheme, codecFactory, fuzzer, gvk, nonRoundTrippableTypes, skipProtobuf)
		} else {
			roundTripOfExternalType(t, scheme, codecFactory, fuzzer, gvk, skipProtobuf)
		}
		if t.Failed() {
			break
		}
	}
}

// fuzzInternalObject fuzzes an arbitrary runtime object using the appropriate
// fuzzer registered with the apitesting package.
func fuzzInternalObject(t *testing.T, fuzzer *fuzz.Fuzzer, object runtime.Object) runtime.Object {
	fuzzer.Fuzz(object)

	j, err := apimeta.TypeAccessor(object)
	if err != nil {
		t.Fatalf("Unexpected error %v for %#v", err, object)
	}
	j.SetKind("")
	j.SetAPIVersion("")

	return object
}

func groupsFromScheme(scheme *runtime.Scheme) []string {
	ret := sets.String{}
	for gvk := range scheme.AllKnownTypes() {
		ret.Insert(gvk.Group)
	}
	return ret.List()
}

func roundTripToAllExternalVersions(t *testing.T, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, internalGVK schema.GroupVersionKind, nonRoundTrippableTypes map[schema.GroupVersionKind]bool, skipProtobuf bool) {
	object, err := scheme.New(internalGVK)
	if err != nil {
		t.Fatalf("Couldn't make a %v? %v", internalGVK, err)
	}
	if _, err := apimeta.TypeAccessor(object); err != nil {
		t.Fatalf("%q is not a TypeMeta and cannot be tested - add it to nonRoundTrippableInternalTypes: %v", internalGVK, err)
	}

	fuzzInternalObject(t, fuzzer, object)

	// find all potential serializations in the scheme.
	// TODO fix this up to handle kinds that cross registered with different names.
	for externalGVK, externalGoType := range scheme.AllKnownTypes() {
		if externalGVK.Version == runtime.APIVersionInternal {
			continue
		}
		if externalGVK.GroupKind() != internalGVK.GroupKind() {
			continue
		}
		if nonRoundTrippableTypes[externalGVK] {
			t.Logf("\tskipping  %v %v", externalGVK, externalGoType)
			continue
		}
		t.Logf("\tround tripping to %v %v", externalGVK, externalGoType)

		roundTrip(t, scheme, apitesting.TestCodec(codecFactory, externalGVK.GroupVersion()), object)

		// TODO remove this hack after we're past the intermediate steps
		if !skipProtobuf && externalGVK.Group != "kubeadm.k8s.io" {
			s := protobuf.NewSerializer(scheme, scheme, protobuf.SerializerOptions{})
			protobufCodec := codecFactory.CodecForVersions(s, s, externalGVK.GroupVersion(), nil)
			roundTrip(t, scheme, protobufCodec, object)
		}
	}
}

func roundTripOfExternalType(t *testing.T, scheme *runtime.Scheme, codecFactory runtimeserializer.CodecFactory, fuzzer *fuzz.Fuzzer, externalGVK schema.GroupVersionKind, skipProtobuf bool) {
	object, err := scheme.New(externalGVK)
	if err != nil {
		t.Fatalf("Couldn't make a %v? %v", externalGVK, err)
	}
	typeAcc, err := apimeta.TypeAccessor(object)
	if err != nil {
		t.Fatalf("%q is not a TypeMeta and cannot be tested - add it to nonRoundTrippableInternalTypes: %v", externalGVK, err)
	}

	fuzzInternalObject(t, fuzzer, object)

	typeAcc.SetKind(externalGVK.Kind)
	typeAcc.SetAPIVersion(externalGVK.GroupVersion().String())

	roundTrip(t, scheme, json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{}), object)

	// TODO remove this hack after we're past the intermediate steps
	if !skipProtobuf {
		roundTrip(t, scheme, protobuf.NewSerializer(scheme, scheme, protobuf.SerializerOptions{}), object)
	}
}

// roundTrip applies a single round-trip test to the given runtime object
// using the given codec.  The round-trip test ensures that an object can be
// deep-copied, converted, marshaled and back without loss of data.
//
// For internal types this means
//
//	internal -> external -> json/protobuf -> external -> internal.
//
// For external types this means
//
//	external -> json/protobuf -> external.
func roundTrip(t *testing.T, scheme *runtime.Scheme, codec runtime.Codec, object runtime.Object) {
	original := object

	// deep copy the original object
	object = object.DeepCopyObject()
	name := reflect.TypeOf(object).Elem().Name()
	if !apiequality.Semantic.DeepEqual(original, object) {
		t.Errorf("%v: DeepCopy altered the object, diff: %v", name, cmp.Diff(original, object))
		t.Errorf("%s", dump.Pretty(original))
		t.Errorf("%s", dump.Pretty(object))
		return
	}

	// encode (serialize) the deep copy using the provided codec
	data, err := runtime.Encode(codec, object)
	if err != nil {
		if runtime.IsNotRegisteredError(err) {
			t.Logf("%v: not registered: %v (%s)", name, err, dump.Pretty(object))
		} else {
			t.Errorf("%v: %v (%s)", name, err, dump.Pretty(object))
		}
		return
	}

	// ensure that the deep copy is equal to the original; neither the deep
	// copy or conversion should alter the object
	// TODO eliminate this global
	if !apiequality.Semantic.DeepEqual(original, object) {
		t.Errorf("%v: encode altered the object, diff: %v", name, cmp.Diff(original, object))
		return
	}

	// encode (serialize) a second time to verify that it was not varying
	secondData, err := runtime.Encode(codec, object)
	if err != nil {
		if runtime.IsNotRegisteredError(err) {
			t.Logf("%v: not registered: %v (%s)", name, err, dump.Pretty(object))
		} else {
			t.Errorf("%v: %v (%s)", name, err, dump.Pretty(object))
		}
		return
	}

	// serialization to the wire must be stable to ensure that we don't write twice to the DB
	// when the object hasn't changed.
	if !bytes.Equal(data, secondData) {
		t.Errorf("%v: serialization is not stable: %s", name, dump.Pretty(object))
	}

	// decode (deserialize) the encoded data back into an object
	obj2, err := runtime.Decode(codec, data)
	if err != nil {
		t.Errorf("%v: %v\nCodec: %#v\nData: %s\nSource: %s", name, err, codec, dataAsString(data), dump.Pretty(object))
		panic("failed")
	}

	// ensure that the object produced from decoding the encoded data is equal
	// to the original object
	if !apiequality.Semantic.DeepEqual(original, obj2) {
		t.Errorf("%v: diff: %v\nCodec: %#v\nSource:\n\n%s\n\nEncoded:\n\n%s\n\nFinal:\n\n%s", name, cmp.Diff(original, obj2), codec, dump.Pretty(original), dataAsString(data), dump.Pretty(obj2))
		return
	}

	// decode the encoded data into a new object (instead of letting the codec
	// create a new object)
	obj3 := reflect.New(reflect.TypeOf(object).Elem()).Interface().(runtime.Object)
	if err := runtime.DecodeInto(codec, data, obj3); err != nil {
		t.Errorf("%v: %v", name, err)
		return
	}

	// special case for kinds which are internal and external at the same time (many in meta.k8s.io are). For those
	// runtime.DecodeInto above will return the external variant and set the APIVersion and kind, while the input
	// object might be internal. Hence, we clear those values for obj3 for that case to correctly compare.
	intAndExt, err := internalAndExternalKind(scheme, object)
	if err != nil {
		t.Errorf("%v: %v", name, err)
		return
	}
	if intAndExt {
		typeAcc, err := apimeta.TypeAccessor(object)
		if err != nil {
			t.Fatalf("%v: error accessing TypeMeta: %v", name, err)
		}
		if len(typeAcc.GetAPIVersion()) == 0 {
			typeAcc, err := apimeta.TypeAccessor(obj3)
			if err != nil {
				t.Fatalf("%v: error accessing TypeMeta: %v", name, err)
			}
			typeAcc.SetAPIVersion("")
			typeAcc.SetKind("")
		}
	}

	// ensure that the new runtime object is equal to the original after being
	// decoded into
	if !apiequality.Semantic.DeepEqual(object, obj3) {
		t.Errorf("%v: diff: %v\nCodec: %#v", name, cmp.Diff(object, obj3), codec)
		return
	}

	// do structure-preserving fuzzing of the deep-copied object. If it shares anything with the original,
	// the deep-copy was actually only a shallow copy. Then original and obj3 will be different after fuzzing.
	// NOTE: we use the encoding+decoding here as an alternative, guaranteed deep-copy to compare against.
	fuzzer.ValueFuzz(object)
	if !apiequality.Semantic.DeepEqual(original, obj3) {
		t.Errorf("%v: fuzzing a copy altered the original, diff: %v", name, cmp.Diff(original, obj3))
		return
	}
}

func internalAndExternalKind(scheme *runtime.Scheme, object runtime.Object) (bool, error) {
	kinds, _, err := scheme.ObjectKinds(object)
	if err != nil {
		return false, err
	}
	internal, external := false, false
	for _, k := range kinds {
		if k.Version == runtime.APIVersionInternal {
			internal = true
		} else {
			external = true
		}
	}
	return internal && external, nil
}

// dataAsString returns the given byte array as a string; handles detecting
// protocol buffers.
func dataAsString(data []byte) string {
	dataString := string(data)
	if !strings.HasPrefix(dataString, "{") {
		dataString = "\n" + hex.Dump(data)
	}
	return dataString
}
