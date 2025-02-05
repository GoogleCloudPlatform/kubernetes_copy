// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"math"
	"sort"
	"sync/atomic"

	"google.golang.org/protobuf/internal/flags"
	"google.golang.org/protobuf/internal/protolazy"
	"google.golang.org/protobuf/proto"
	piface "google.golang.org/protobuf/runtime/protoiface"
)

type marshalOptions struct {
	flags piface.MarshalInputFlags
}

func (o marshalOptions) Options() proto.MarshalOptions {
	return proto.MarshalOptions{
		AllowPartial:  true,
		Deterministic: o.Deterministic(),
		UseCachedSize: o.UseCachedSize(),
	}
}

func (o marshalOptions) Deterministic() bool { return o.flags&piface.MarshalDeterministic != 0 }
func (o marshalOptions) UseCachedSize() bool { return o.flags&piface.MarshalUseCachedSize != 0 }

// size is protoreflect.Methods.Size.
func (mi *MessageInfo) size(in piface.SizeInput) piface.SizeOutput {
	var p pointer
	if ms, ok := in.Message.(*messageState); ok {
		p = ms.pointer()
	} else {
		p = in.Message.(*messageReflectWrapper).pointer()
	}
	size := mi.sizePointer(p, marshalOptions{
		flags: in.Flags,
	})
	return piface.SizeOutput{Size: size}
}

func (mi *MessageInfo) sizePointer(p pointer, opts marshalOptions) (size int) {
	mi.init()
	if p.IsNil() {
		return 0
	}
	if opts.UseCachedSize() && mi.sizecacheOffset.IsValid() {
		// The size cache contains the size + 1, to allow the
		// zero value to be invalid, while also allowing for a
		// 0 size to be cached.
		if size := atomic.LoadInt32(p.Apply(mi.sizecacheOffset).Int32()); size > 0 {
			return int(size - 1)
		}
	}
	return mi.sizePointerSlow(p, opts)
}

func (mi *MessageInfo) sizePointerSlow(p pointer, opts marshalOptions) (size int) {
	if flags.ProtoLegacy && mi.isMessageSet {
		size = sizeMessageSet(mi, p, opts)
		if mi.sizecacheOffset.IsValid() {
			atomic.StoreInt32(p.Apply(mi.sizecacheOffset).Int32(), int32(size+1))
		}
		return size
	}
	if mi.extensionOffset.IsValid() {
		e := p.Apply(mi.extensionOffset).Extensions()
		size += mi.sizeExtensions(e, opts)
	}

	var lazy **protolazy.XXX_lazyUnmarshalInfo
	var presence presence
	if mi.presenceOffset.IsValid() {
		presence = p.Apply(mi.presenceOffset).PresenceInfo()
		if mi.lazyOffset.IsValid() {
			lazy = p.Apply(mi.lazyOffset).LazyInfoPtr()
		}
	}

	for _, f := range mi.orderedCoderFields {
		if f.funcs.size == nil {
			continue
		}
		fptr := p.Apply(f.offset)

		if f.presenceIndex != noPresence {
			if !presence.Present(f.presenceIndex) {
				continue
			}

			if f.isLazy && fptr.AtomicGetPointer().IsNil() {
				if lazyFields(opts) {
					size += (*lazy).SizeField(uint32(f.num))
					continue
				} else {
					mi.lazyUnmarshal(p, f.num)
				}
			}
			size += f.funcs.size(fptr, f, opts)
			continue
		}

		if f.isPointer && fptr.Elem().IsNil() {
			continue
		}
		size += f.funcs.size(fptr, f, opts)
	}
	if mi.unknownOffset.IsValid() {
		if u := mi.getUnknownBytes(p); u != nil {
			size += len(*u)
		}
	}
	if mi.sizecacheOffset.IsValid() {
		if size > (math.MaxInt32 - 1) {
			// The size is too large for the int32 sizecache field.
			// We will need to recompute the size when encoding;
			// unfortunately expensive, but better than invalid output.
			atomic.StoreInt32(p.Apply(mi.sizecacheOffset).Int32(), 0)
		} else {
			// The size cache contains the size + 1, to allow the
			// zero value to be invalid, while also allowing for a
			// 0 size to be cached.
			atomic.StoreInt32(p.Apply(mi.sizecacheOffset).Int32(), int32(size+1))
		}
	}
	return size
}

// marshal is protoreflect.Methods.Marshal.
func (mi *MessageInfo) marshal(in piface.MarshalInput) (out piface.MarshalOutput, err error) {
	var p pointer
	if ms, ok := in.Message.(*messageState); ok {
		p = ms.pointer()
	} else {
		p = in.Message.(*messageReflectWrapper).pointer()
	}
	b, err := mi.marshalAppendPointer(in.Buf, p, marshalOptions{
		flags: in.Flags,
	})
	return piface.MarshalOutput{Buf: b}, err
}

func (mi *MessageInfo) marshalAppendPointer(b []byte, p pointer, opts marshalOptions) ([]byte, error) {
	mi.init()
	if p.IsNil() {
		return b, nil
	}
	if flags.ProtoLegacy && mi.isMessageSet {
		return marshalMessageSet(mi, b, p, opts)
	}
	var err error
	// The old marshaler encodes extensions at beginning.
	if mi.extensionOffset.IsValid() {
		e := p.Apply(mi.extensionOffset).Extensions()
		// TODO: Special handling for MessageSet?
		b, err = mi.appendExtensions(b, e, opts)
		if err != nil {
			return b, err
		}
	}

	var lazy **protolazy.XXX_lazyUnmarshalInfo
	var presence presence
	if mi.presenceOffset.IsValid() {
		presence = p.Apply(mi.presenceOffset).PresenceInfo()
		if mi.lazyOffset.IsValid() {
			lazy = p.Apply(mi.lazyOffset).LazyInfoPtr()
		}
	}

	for _, f := range mi.orderedCoderFields {
		if f.funcs.marshal == nil {
			continue
		}
		fptr := p.Apply(f.offset)

		if f.presenceIndex != noPresence {
			if !presence.Present(f.presenceIndex) {
				continue
			}
			if f.isLazy {
				// Be careful, this field needs to be read atomically, like for a get
				if f.isPointer && fptr.AtomicGetPointer().IsNil() {
					if lazyFields(opts) {
						b, _ = (*lazy).AppendField(b, uint32(f.num))
						continue
					} else {
						mi.lazyUnmarshal(p, f.num)
					}
				}

				b, err = f.funcs.marshal(b, fptr, f, opts)
				if err != nil {
					return b, err
				}
				continue
			} else if f.isPointer && fptr.Elem().IsNil() {
				continue
			}
			b, err = f.funcs.marshal(b, fptr, f, opts)
			if err != nil {
				return b, err
			}
			continue
		}

		if f.isPointer && fptr.Elem().IsNil() {
			continue
		}
		b, err = f.funcs.marshal(b, fptr, f, opts)
		if err != nil {
			return b, err
		}
	}
	if mi.unknownOffset.IsValid() && !mi.isMessageSet {
		if u := mi.getUnknownBytes(p); u != nil {
			b = append(b, (*u)...)
		}
	}
	return b, nil
}

// fullyLazyExtensions returns true if we should attempt to keep extensions lazy over size and marshal.
func fullyLazyExtensions(opts marshalOptions) bool {
	// When deterministic marshaling is requested, force an unmarshal for lazy
	// extensions to produce a deterministic result, instead of passing through
	// bytes lazily that may or may not match what Go Protobuf would produce.
	return opts.flags&piface.MarshalDeterministic == 0
}

// lazyFields returns true if we should attempt to keep fields lazy over size and marshal.
func lazyFields(opts marshalOptions) bool {
	// When deterministic marshaling is requested, force an unmarshal for lazy
	// fields to produce a deterministic result, instead of passing through
	// bytes lazily that may or may not match what Go Protobuf would produce.
	return opts.flags&piface.MarshalDeterministic == 0
}

func (mi *MessageInfo) sizeExtensions(ext *map[int32]ExtensionField, opts marshalOptions) (n int) {
	if ext == nil {
		return 0
	}
	for _, x := range *ext {
		xi := getExtensionFieldInfo(x.Type())
		if xi.funcs.size == nil {
			continue
		}
		if fullyLazyExtensions(opts) {
			// Don't expand the extension, instead use the buffer to calculate size
			if lb := x.lazyBuffer(); lb != nil {
				// We got hold of the buffer, so it's still lazy.
				n += len(lb)
				continue
			}
		}
		n += xi.funcs.size(x.Value(), xi.tagsize, opts)
	}
	return n
}

func (mi *MessageInfo) appendExtensions(b []byte, ext *map[int32]ExtensionField, opts marshalOptions) ([]byte, error) {
	if ext == nil {
		return b, nil
	}

	switch len(*ext) {
	case 0:
		return b, nil
	case 1:
		// Fast-path for one extension: Don't bother sorting the keys.
		var err error
		for _, x := range *ext {
			xi := getExtensionFieldInfo(x.Type())
			if fullyLazyExtensions(opts) {
				// Don't expand the extension if it's still in wire format, instead use the buffer content.
				if lb := x.lazyBuffer(); lb != nil {
					b = append(b, lb...)
					continue
				}
			}
			b, err = xi.funcs.marshal(b, x.Value(), xi.wiretag, opts)
		}
		return b, err
	default:
		// Sort the keys to provide a deterministic encoding.
		// Not sure this is required, but the old code does it.
		keys := make([]int, 0, len(*ext))
		for k := range *ext {
			keys = append(keys, int(k))
		}
		sort.Ints(keys)
		var err error
		for _, k := range keys {
			x := (*ext)[int32(k)]
			xi := getExtensionFieldInfo(x.Type())
			if fullyLazyExtensions(opts) {
				// Don't expand the extension if it's still in wire format, instead use the buffer content.
				if lb := x.lazyBuffer(); lb != nil {
					b = append(b, lb...)
					continue
				}
			}
			b, err = xi.funcs.marshal(b, x.Value(), xi.wiretag, opts)
			if err != nil {
				return b, err
			}
		}
		return b, nil
	}
}
