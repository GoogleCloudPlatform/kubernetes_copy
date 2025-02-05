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

package test

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/testapigroup/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
)

type testObject struct {
	gvk schema.GroupVersionKind
}

func (d *testObject) GetObjectKind() schema.ObjectKind                { return d }
func (d *testObject) SetGroupVersionKind(gvk schema.GroupVersionKind) { d.gvk = gvk }
func (d *testObject) GroupVersionKind() schema.GroupVersionKind       { return d.gvk }
func (d *testObject) DeepCopyObject() runtime.Object {
	panic("testObject does not support DeepCopy")
}

type testMarshalable struct {
	testObject
	data []byte
	err  error
}

func (d *testMarshalable) Marshal() ([]byte, error) {
	return d.data, d.err
}

func (d *testMarshalable) DeepCopyObject() runtime.Object {
	panic("testMarshalable does not support DeepCopy")
}

type testBufferedMarshalable struct {
	testObject
	data []byte
	err  error
}

func (d *testBufferedMarshalable) Marshal() ([]byte, error) {
	return nil, fmt.Errorf("not invokable")
}

func (d *testBufferedMarshalable) MarshalTo(data []byte) (int, error) {
	copy(data, d.data)
	return len(d.data), d.err
}

func (d *testBufferedMarshalable) Size() int {
	return len(d.data)
}

func (d *testBufferedMarshalable) DeepCopyObject() runtime.Object {
	panic("testBufferedMarshalable does not support DeepCopy")
}

func TestRecognize(t *testing.T) {
	s := protobuf.NewSerializer(nil, nil, protobuf.SerializerOptions{})
	ignores := [][]byte{
		nil,
		{},
		[]byte("k8s"),
		{0x6b, 0x38, 0x73, 0x01},
	}
	for i, data := range ignores {
		if ok, _, err := s.RecognizesData(data); err != nil || ok {
			t.Errorf("%d: should not recognize data: %v", i, err)
		}
	}
	recognizes := [][]byte{
		{0x6b, 0x38, 0x73, 0x00},
		{0x6b, 0x38, 0x73, 0x00, 0x01},
	}
	for i, data := range recognizes {
		if ok, _, err := s.RecognizesData(data); err != nil || !ok {
			t.Errorf("%d: should recognize data: %v", i, err)
		}
	}
}

func TestEncode(t *testing.T) {
	obj1 := &testMarshalable{testObject: testObject{}, data: []byte{}}
	wire1 := []byte{
		0x6b, 0x38, 0x73, 0x00, // prefix
		0x0a, 0x04,
		0x0a, 0x00, // apiversion
		0x12, 0x00, // kind
		0x12, 0x00, // data
		0x1a, 0x00, // content-type
		0x22, 0x00, // content-encoding
	}
	obj2 := &testMarshalable{
		testObject: testObject{gvk: schema.GroupVersionKind{Kind: "test", Group: "other", Version: "version"}},
		data:       []byte{0x01, 0x02, 0x03},
	}
	wire2 := []byte{
		0x6b, 0x38, 0x73, 0x00, // prefix
		0x0a, 0x15,
		0x0a, 0x0d, 0x6f, 0x74, 0x68, 0x65, 0x72, 0x2f, 0x76, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, // apiversion
		0x12, 0x04, 0x74, 0x65, 0x73, 0x74, // kind
		0x12, 0x03, 0x01, 0x02, 0x03, // data
		0x1a, 0x00, // content-type
		0x22, 0x00, // content-encoding
	}

	err1 := fmt.Errorf("a test error")

	testCases := []struct {
		obj   runtime.Object
		data  []byte
		errFn func(error) bool
	}{
		{
			obj:   &testObject{},
			errFn: protobuf.IsNotMarshalable,
		},
		{
			obj:  obj1,
			data: wire1,
		},
		{
			obj:   &testMarshalable{testObject: obj1.testObject, err: err1},
			errFn: func(err error) bool { return err == err1 },
		},
		{
			// if this test fails, writing the "fast path" marshal is not the same as the "slow path"
			obj:  &testBufferedMarshalable{testObject: obj1.testObject, data: obj1.data},
			data: wire1,
		},
		{
			obj:  obj2,
			data: wire2,
		},
		{
			// if this test fails, writing the "fast path" marshal is not the same as the "slow path"
			obj:  &testBufferedMarshalable{testObject: obj2.testObject, data: obj2.data},
			data: wire2,
		},
		{
			obj:   &testBufferedMarshalable{testObject: obj1.testObject, err: err1},
			errFn: func(err error) bool { return err == err1 },
		},
	}

	for i, test := range testCases {
		s := protobuf.NewSerializer(nil, nil, protobuf.SerializerOptions{})
		data, err := runtime.Encode(s, test.obj)

		switch {
		case err == nil && test.errFn != nil:
			t.Errorf("%d: failed: %v", i, err)
			continue
		case err != nil && test.errFn == nil:
			t.Errorf("%d: failed: %v", i, err)
			continue
		case err != nil:
			if !test.errFn(err) {
				t.Errorf("%d: failed: %v", i, err)
			}
			if data != nil {
				t.Errorf("%d: should not have returned nil data", i)
			}
			continue
		}

		if test.data != nil && !bytes.Equal(test.data, data) {
			t.Errorf("%d: unexpected data:\n%s", i, hex.Dump(data))
			continue
		}

		if ok, _, err := s.RecognizesData(data); !ok || err != nil {
			t.Errorf("%d: did not recognize data generated by call: %v", i, err)
		}
	}
}

func TestProtobufDecode(t *testing.T) {
	wire1 := []byte{
		0x6b, 0x38, 0x73, 0x00, // prefix
		0x0a, 0x04,
		0x0a, 0x00, // apiversion
		0x12, 0x00, // kind
		0x12, 0x00, // data
		0x1a, 0x00, // content-type
		0x22, 0x00, // content-encoding
	}
	wire2 := []byte{
		0x6b, 0x38, 0x73, 0x00, // prefix
		0x0a, 0x15,
		0x0a, 0x0d, 0x6f, 0x74, 0x68, 0x65, 0x72, 0x2f, 0x76, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, // apiversion
		0x12, 0x04, 0x74, 0x65, 0x73, 0x74, // kind
		0x12, 0x07, 0x6b, 0x38, 0x73, 0x00, 0x01, 0x02, 0x03, // data
		0x1a, 0x00, // content-type
		0x22, 0x00, // content-encoding
	}

	testCases := []struct {
		obj   runtime.Object
		data  []byte
		errFn func(error) bool
	}{
		{
			obj:   &runtime.Unknown{},
			errFn: func(err error) bool { return err.Error() == "empty data" },
		},
		{
			data:  []byte{0x6b},
			errFn: func(err error) bool { return strings.Contains(err.Error(), "does not appear to be a protobuf message") },
		},
		{
			obj: &runtime.Unknown{
				Raw: []byte{},
			},
			data: wire1,
		},
		{
			obj: &runtime.Unknown{
				TypeMeta: runtime.TypeMeta{
					APIVersion: "other/version",
					Kind:       "test",
				},
				// content type is set because the prefix matches the content
				ContentType: runtime.ContentTypeProtobuf,
				Raw:         []byte{0x6b, 0x38, 0x73, 0x00, 0x01, 0x02, 0x03},
			},
			data: wire2,
		},
	}

	for i, test := range testCases {
		s := protobuf.NewSerializer(nil, nil, protobuf.SerializerOptions{})
		unk := &runtime.Unknown{}
		err := runtime.DecodeInto(s, test.data, unk)

		switch {
		case err == nil && test.errFn != nil:
			t.Errorf("%d: failed: %v", i, err)
			continue
		case err != nil && test.errFn == nil:
			t.Errorf("%d: failed: %v", i, err)
			continue
		case err != nil:
			if !test.errFn(err) {
				t.Errorf("%d: failed: %v", i, err)
			}
			continue
		}

		if !reflect.DeepEqual(unk, test.obj) {
			t.Errorf("%d: unexpected object:\n%#v", i, unk)
			continue
		}
	}
}

func TestDecodeObjects(t *testing.T) {
	obj1 := &v1.Carp{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cool",
		},
		Spec: v1.CarpSpec{
			Hostname: "coolhost",
		},
	}
	obj1wire, err := obj1.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	wire1, err := (&runtime.Unknown{
		TypeMeta: runtime.TypeMeta{Kind: "Carp", APIVersion: "v1"},
		Raw:      obj1wire,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}

	unk2 := &runtime.Unknown{
		TypeMeta: runtime.TypeMeta{Kind: "Carp", APIVersion: "v1"},
	}
	wire2 := make([]byte, len(wire1)*2)
	n, err := unk2.NestedMarshalTo(wire2, obj1, uint64(obj1.Size()))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(wire1) || !bytes.Equal(wire1, wire2[:n]) {
		t.Fatalf("unexpected wire:\n%s", hex.Dump(wire2[:n]))
	}

	wire1 = append([]byte{0x6b, 0x38, 0x73, 0x00}, wire1...)

	obj1WithKind := obj1.DeepCopyObject()
	obj1WithKind.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Carp"})
	testCases := []struct {
		obj   runtime.Object
		data  []byte
		errFn func(error) bool
	}{
		{
			obj:  obj1WithKind,
			data: wire1,
		},
	}
	scheme := runtime.NewScheme()
	for i, test := range testCases {
		scheme.AddKnownTypes(schema.GroupVersion{Version: "v1"}, &v1.Carp{})
		require.NoError(t, v1.AddToScheme(scheme))
		s := protobuf.NewSerializer(scheme, scheme, protobuf.SerializerOptions{})
		obj, err := runtime.Decode(s, test.data)

		switch {
		case err == nil && test.errFn != nil:
			t.Errorf("%d: failed: %v", i, err)
			continue
		case err != nil && test.errFn == nil:
			t.Errorf("%d: failed: %v", i, err)
			continue
		case err != nil:
			if !test.errFn(err) {
				t.Errorf("%d: failed: %v", i, err)
			}
			if obj != nil {
				t.Errorf("%d: should not have returned an object", i)
			}
			continue
		}

		if !apiequality.Semantic.DeepEqual(obj, test.obj) {
			t.Errorf("%d: unexpected object:\n%s", i, cmp.Diff(test.obj, obj))
			continue
		}
	}
}
