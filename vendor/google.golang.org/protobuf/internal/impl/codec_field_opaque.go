// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"fmt"
	"reflect"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/internal/errors"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func makeOpaqueMessageFieldCoder(fd protoreflect.FieldDescriptor, ft reflect.Type) (*MessageInfo, pointerCoderFuncs) {
	mi := getMessageInfo(ft)
	if mi == nil {
		panic(fmt.Sprintf("invalid field: %v: unsupported message type %v", fd.FullName(), ft))
	}
	switch fd.Kind() {
	case protoreflect.MessageKind:
		return mi, pointerCoderFuncs{
			size:      sizeOpaqueMessage,
			marshal:   appendOpaqueMessage,
			unmarshal: consumeOpaqueMessage,
			isInit:    isInitOpaqueMessage,
			merge:     mergeOpaqueMessage,
		}
	case protoreflect.GroupKind:
		return mi, pointerCoderFuncs{
			size:      sizeOpaqueGroup,
			marshal:   appendOpaqueGroup,
			unmarshal: consumeOpaqueGroup,
			isInit:    isInitOpaqueMessage,
			merge:     mergeOpaqueMessage,
		}
	}
	panic("unexpected field kind")
}

func sizeOpaqueMessage(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	return protowire.SizeBytes(f.mi.sizePointer(p.AtomicGetPointer(), opts)) + f.tagsize
}

func appendOpaqueMessage(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	mp := p.AtomicGetPointer()
	calculatedSize := f.mi.sizePointer(mp, opts)
	b = protowire.AppendVarint(b, f.wiretag)
	b = protowire.AppendVarint(b, uint64(calculatedSize))
	before := len(b)
	b, err := f.mi.marshalAppendPointer(b, mp, opts)
	if measuredSize := len(b) - before; calculatedSize != measuredSize && err == nil {
		return nil, errors.MismatchedSizeCalculation(calculatedSize, measuredSize)
	}
	return b, err
}

func consumeOpaqueMessage(b []byte, p pointer, wtyp protowire.Type, f *coderFieldInfo, opts unmarshalOptions) (out unmarshalOutput, err error) {
	if wtyp != protowire.BytesType {
		return out, errUnknown
	}
	v, n := protowire.ConsumeBytes(b)
	if n < 0 {
		return out, errDecode
	}
	mp := p.AtomicGetPointer()
	if mp.IsNil() {
		mp = p.AtomicSetPointerIfNil(pointerOfValue(reflect.New(f.mi.GoReflectType.Elem())))
	}
	o, err := f.mi.unmarshalPointer(v, mp, 0, opts)
	if err != nil {
		return out, err
	}
	out.n = n
	out.initialized = o.initialized
	return out, nil
}

func isInitOpaqueMessage(p pointer, f *coderFieldInfo) error {
	mp := p.AtomicGetPointer()
	if mp.IsNil() {
		return nil
	}
	return f.mi.checkInitializedPointer(mp)
}

func mergeOpaqueMessage(dst, src pointer, f *coderFieldInfo, opts mergeOptions) {
	dstmp := dst.AtomicGetPointer()
	if dstmp.IsNil() {
		dstmp = dst.AtomicSetPointerIfNil(pointerOfValue(reflect.New(f.mi.GoReflectType.Elem())))
	}
	f.mi.mergePointer(dstmp, src.AtomicGetPointer(), opts)
}

func sizeOpaqueGroup(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	return 2*f.tagsize + f.mi.sizePointer(p.AtomicGetPointer(), opts)
}

func appendOpaqueGroup(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	b = protowire.AppendVarint(b, f.wiretag) // start group
	b, err := f.mi.marshalAppendPointer(b, p.AtomicGetPointer(), opts)
	b = protowire.AppendVarint(b, f.wiretag+1) // end group
	return b, err
}

func consumeOpaqueGroup(b []byte, p pointer, wtyp protowire.Type, f *coderFieldInfo, opts unmarshalOptions) (out unmarshalOutput, err error) {
	if wtyp != protowire.StartGroupType {
		return out, errUnknown
	}
	mp := p.AtomicGetPointer()
	if mp.IsNil() {
		mp = p.AtomicSetPointerIfNil(pointerOfValue(reflect.New(f.mi.GoReflectType.Elem())))
	}
	o, e := f.mi.unmarshalPointer(b, mp, f.num, opts)
	return o, e
}

func makeOpaqueRepeatedMessageFieldCoder(fd protoreflect.FieldDescriptor, ft reflect.Type) (*MessageInfo, pointerCoderFuncs) {
	if ft.Kind() != reflect.Ptr || ft.Elem().Kind() != reflect.Slice {
		panic(fmt.Sprintf("invalid field: %v: unsupported type for opaque repeated message: %v", fd.FullName(), ft))
	}
	mt := ft.Elem().Elem() // *[]*T -> *T
	mi := getMessageInfo(mt)
	if mi == nil {
		panic(fmt.Sprintf("invalid field: %v: unsupported message type %v", fd.FullName(), mt))
	}
	switch fd.Kind() {
	case protoreflect.MessageKind:
		return mi, pointerCoderFuncs{
			size:      sizeOpaqueMessageSlice,
			marshal:   appendOpaqueMessageSlice,
			unmarshal: consumeOpaqueMessageSlice,
			isInit:    isInitOpaqueMessageSlice,
			merge:     mergeOpaqueMessageSlice,
		}
	case protoreflect.GroupKind:
		return mi, pointerCoderFuncs{
			size:      sizeOpaqueGroupSlice,
			marshal:   appendOpaqueGroupSlice,
			unmarshal: consumeOpaqueGroupSlice,
			isInit:    isInitOpaqueMessageSlice,
			merge:     mergeOpaqueMessageSlice,
		}
	}
	panic("unexpected field kind")
}

func sizeOpaqueMessageSlice(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	s := p.AtomicGetPointer().PointerSlice()
	n := 0
	for _, v := range s {
		n += protowire.SizeBytes(f.mi.sizePointer(v, opts)) + f.tagsize
	}
	return n
}

func appendOpaqueMessageSlice(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	s := p.AtomicGetPointer().PointerSlice()
	var err error
	for _, v := range s {
		b = protowire.AppendVarint(b, f.wiretag)
		siz := f.mi.sizePointer(v, opts)
		b = protowire.AppendVarint(b, uint64(siz))
		before := len(b)
		b, err = f.mi.marshalAppendPointer(b, v, opts)
		if err != nil {
			return b, err
		}
		if measuredSize := len(b) - before; siz != measuredSize {
			return nil, errors.MismatchedSizeCalculation(siz, measuredSize)
		}
	}
	return b, nil
}

func consumeOpaqueMessageSlice(b []byte, p pointer, wtyp protowire.Type, f *coderFieldInfo, opts unmarshalOptions) (out unmarshalOutput, err error) {
	if wtyp != protowire.BytesType {
		return out, errUnknown
	}
	v, n := protowire.ConsumeBytes(b)
	if n < 0 {
		return out, errDecode
	}
	mp := pointerOfValue(reflect.New(f.mi.GoReflectType.Elem()))
	o, err := f.mi.unmarshalPointer(v, mp, 0, opts)
	if err != nil {
		return out, err
	}
	sp := p.AtomicGetPointer()
	if sp.IsNil() {
		sp = p.AtomicSetPointerIfNil(pointerOfValue(reflect.New(f.ft.Elem())))
	}
	sp.AppendPointerSlice(mp)
	out.n = n
	out.initialized = o.initialized
	return out, nil
}

func isInitOpaqueMessageSlice(p pointer, f *coderFieldInfo) error {
	sp := p.AtomicGetPointer()
	if sp.IsNil() {
		return nil
	}
	s := sp.PointerSlice()
	for _, v := range s {
		if err := f.mi.checkInitializedPointer(v); err != nil {
			return err
		}
	}
	return nil
}

func mergeOpaqueMessageSlice(dst, src pointer, f *coderFieldInfo, opts mergeOptions) {
	ds := dst.AtomicGetPointer()
	if ds.IsNil() {
		ds = dst.AtomicSetPointerIfNil(pointerOfValue(reflect.New(f.ft.Elem())))
	}
	for _, sp := range src.AtomicGetPointer().PointerSlice() {
		dm := pointerOfValue(reflect.New(f.mi.GoReflectType.Elem()))
		f.mi.mergePointer(dm, sp, opts)
		ds.AppendPointerSlice(dm)
	}
}

func sizeOpaqueGroupSlice(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	s := p.AtomicGetPointer().PointerSlice()
	n := 0
	for _, v := range s {
		n += 2*f.tagsize + f.mi.sizePointer(v, opts)
	}
	return n
}

func appendOpaqueGroupSlice(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	s := p.AtomicGetPointer().PointerSlice()
	var err error
	for _, v := range s {
		b = protowire.AppendVarint(b, f.wiretag) // start group
		b, err = f.mi.marshalAppendPointer(b, v, opts)
		if err != nil {
			return b, err
		}
		b = protowire.AppendVarint(b, f.wiretag+1) // end group
	}
	return b, nil
}

func consumeOpaqueGroupSlice(b []byte, p pointer, wtyp protowire.Type, f *coderFieldInfo, opts unmarshalOptions) (out unmarshalOutput, err error) {
	if wtyp != protowire.StartGroupType {
		return out, errUnknown
	}
	mp := pointerOfValue(reflect.New(f.mi.GoReflectType.Elem()))
	out, err = f.mi.unmarshalPointer(b, mp, f.num, opts)
	if err != nil {
		return out, err
	}
	sp := p.AtomicGetPointer()
	if sp.IsNil() {
		sp = p.AtomicSetPointerIfNil(pointerOfValue(reflect.New(f.ft.Elem())))
	}
	sp.AppendPointerSlice(mp)
	return out, err
}
