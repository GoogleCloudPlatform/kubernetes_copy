// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"fmt"
	"reflect"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/internal/encoding/messageset"
	"google.golang.org/protobuf/internal/order"
	"google.golang.org/protobuf/reflect/protoreflect"
	piface "google.golang.org/protobuf/runtime/protoiface"
)

func (mi *MessageInfo) makeOpaqueCoderMethods(t reflect.Type, si opaqueStructInfo) {
	mi.sizecacheOffset = si.sizecacheOffset
	mi.unknownOffset = si.unknownOffset
	mi.unknownPtrKind = si.unknownType.Kind() == reflect.Ptr
	mi.extensionOffset = si.extensionOffset
	mi.lazyOffset = si.lazyOffset
	mi.presenceOffset = si.presenceOffset

	mi.coderFields = make(map[protowire.Number]*coderFieldInfo)
	fields := mi.Desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)

		fs := si.fieldsByNumber[fd.Number()]
		if fd.ContainingOneof() != nil && !fd.ContainingOneof().IsSynthetic() {
			fs = si.oneofsByName[fd.ContainingOneof().Name()]
		}
		ft := fs.Type
		var wiretag uint64
		if !fd.IsPacked() {
			wiretag = protowire.EncodeTag(fd.Number(), wireTypes[fd.Kind()])
		} else {
			wiretag = protowire.EncodeTag(fd.Number(), protowire.BytesType)
		}
		var fieldOffset offset
		var funcs pointerCoderFuncs
		var childMessage *MessageInfo
		switch {
		case fd.ContainingOneof() != nil && !fd.ContainingOneof().IsSynthetic():
			fieldOffset = offsetOf(fs, mi.Exporter)
		case fd.IsWeak():
			fieldOffset = si.weakOffset
			funcs = makeWeakMessageFieldCoder(fd)
		case fd.Message() != nil && !fd.IsMap():
			fieldOffset = offsetOf(fs, mi.Exporter)
			if fd.IsList() {
				childMessage, funcs = makeOpaqueRepeatedMessageFieldCoder(fd, ft)
			} else {
				childMessage, funcs = makeOpaqueMessageFieldCoder(fd, ft)
			}
		default:
			fieldOffset = offsetOf(fs, mi.Exporter)
			childMessage, funcs = fieldCoder(fd, ft)
		}
		cf := &coderFieldInfo{
			num:        fd.Number(),
			offset:     fieldOffset,
			wiretag:    wiretag,
			ft:         ft,
			tagsize:    protowire.SizeVarint(wiretag),
			funcs:      funcs,
			mi:         childMessage,
			validation: newFieldValidationInfo(mi, si.structInfo, fd, ft),
			isPointer: (fd.Cardinality() == protoreflect.Repeated ||
				fd.Kind() == protoreflect.MessageKind ||
				fd.Kind() == protoreflect.GroupKind),
			isRequired:    fd.Cardinality() == protoreflect.Required,
			presenceIndex: noPresence,
		}

		// TODO: Use presence for all fields.
		//
		// In some cases, such as maps, presence means only "might be set" rather
		// than "is definitely set", but every field should have a presence bit to
		// permit us to skip over definitely-unset fields at marshal time.

		var hasPresence bool
		hasPresence, cf.isLazy = usePresenceForField(si, fd)

		if hasPresence {
			cf.presenceIndex, mi.presenceSize = presenceIndex(mi.Desc, fd)
		}

		mi.orderedCoderFields = append(mi.orderedCoderFields, cf)
		mi.coderFields[cf.num] = cf
	}
	for i, oneofs := 0, mi.Desc.Oneofs(); i < oneofs.Len(); i++ {
		if od := oneofs.Get(i); !od.IsSynthetic() {
			mi.initOneofFieldCoders(od, si.structInfo)
		}
	}
	if messageset.IsMessageSet(mi.Desc) {
		if !mi.extensionOffset.IsValid() {
			panic(fmt.Sprintf("%v: MessageSet with no extensions field", mi.Desc.FullName()))
		}
		if !mi.unknownOffset.IsValid() {
			panic(fmt.Sprintf("%v: MessageSet with no unknown field", mi.Desc.FullName()))
		}
		mi.isMessageSet = true
	}
	sort.Slice(mi.orderedCoderFields, func(i, j int) bool {
		return mi.orderedCoderFields[i].num < mi.orderedCoderFields[j].num
	})

	var maxDense protoreflect.FieldNumber
	for _, cf := range mi.orderedCoderFields {
		if cf.num >= 16 && cf.num >= 2*maxDense {
			break
		}
		maxDense = cf.num
	}
	mi.denseCoderFields = make([]*coderFieldInfo, maxDense+1)
	for _, cf := range mi.orderedCoderFields {
		if int(cf.num) > len(mi.denseCoderFields) {
			break
		}
		mi.denseCoderFields[cf.num] = cf
	}

	// To preserve compatibility with historic wire output, marshal oneofs last.
	if mi.Desc.Oneofs().Len() > 0 {
		sort.Slice(mi.orderedCoderFields, func(i, j int) bool {
			fi := fields.ByNumber(mi.orderedCoderFields[i].num)
			fj := fields.ByNumber(mi.orderedCoderFields[j].num)
			return order.LegacyFieldOrder(fi, fj)
		})
	}

	mi.needsInitCheck = needsInitCheck(mi.Desc)
	if mi.methods.Marshal == nil && mi.methods.Size == nil {
		mi.methods.Flags |= piface.SupportMarshalDeterministic
		mi.methods.Marshal = mi.marshal
		mi.methods.Size = mi.size
	}
	if mi.methods.Unmarshal == nil {
		mi.methods.Flags |= piface.SupportUnmarshalDiscardUnknown
		mi.methods.Unmarshal = mi.unmarshal
	}
	if mi.methods.CheckInitialized == nil {
		mi.methods.CheckInitialized = mi.checkInitialized
	}
	if mi.methods.Merge == nil {
		mi.methods.Merge = mi.merge
	}
	if mi.methods.Equal == nil {
		mi.methods.Equal = equal
	}
}
