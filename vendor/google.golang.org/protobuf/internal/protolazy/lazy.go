// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package protolazy contains internal data structures for lazy message decoding.
package protolazy

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
	piface "google.golang.org/protobuf/runtime/protoiface"
)

// IndexEntry is the structure for an index of the fields in a message of a
// proto (not descending to sub-messages)
type IndexEntry struct {
	FieldNum uint32
	// first byte of this tag/field
	Start uint32
	// first byte after a contiguous sequence of bytes for this tag/field, which could
	// include a single encoding of the field, or multiple encodings for the field
	End uint32
	// True if this protobuf segment includes multiple encodings of the field
	MultipleContiguous bool
}

// XXX_lazyUnmarshalInfo has information about a particular lazily decoded message
//
// Deprecated: Do not use. This will be deleted in the near future.
type XXX_lazyUnmarshalInfo struct {
	// Index of fields and their positions in the protobuf for this
	// message.  Make index be a pointer to a slice so it can be updated
	// atomically.  The index pointer is only set once (lazily when/if
	// the index is first needed), and must always be SET and LOADED
	// ATOMICALLY.
	index *[]IndexEntry
	// The protobuf associated with this lazily decoded message.  It is
	// only set during proto.Unmarshal().  It doesn't need to be set and
	// loaded atomically, since any simultaneous set (Unmarshal) and read
	// (during a get) would already be a race in the app code.
	Protobuf []byte
	// The flags present when Unmarshal was originally called for this particular message
	unmarshalFlags piface.UnmarshalInputFlags
}

// The Buffer and SetBuffer methods let v2/internal/impl interact with
// XXX_lazyUnmarshalInfo via an interface, to avoid an import cycle.

// Buffer returns the lazy unmarshal buffer.
//
// Deprecated: Do not use. This will be deleted in the near future.
func (lazy *XXX_lazyUnmarshalInfo) Buffer() []byte {
	return lazy.Protobuf
}

// SetBuffer sets the lazy unmarshal buffer.
//
// Deprecated: Do not use. This will be deleted in the near future.
func (lazy *XXX_lazyUnmarshalInfo) SetBuffer(b []byte) {
	lazy.Protobuf = b
}

// SetUnmarshalFlags is called to set a copy of the original unmarshalInputFlags.
// The flags should reflect how Unmarshal was called.
func (lazy *XXX_lazyUnmarshalInfo) SetUnmarshalFlags(f piface.UnmarshalInputFlags) {
	lazy.unmarshalFlags = f
}

// UnmarshalFlags returns the original unmarshalInputFlags.
func (lazy *XXX_lazyUnmarshalInfo) UnmarshalFlags() piface.UnmarshalInputFlags {
	return lazy.unmarshalFlags
}

// AllowedPartial returns true if the user originally unmarshalled this message with
// AllowPartial set to true
func (lazy *XXX_lazyUnmarshalInfo) AllowedPartial() bool {
	return (lazy.unmarshalFlags & piface.UnmarshalCheckRequired) == 0
}

func protoFieldNumber(tag uint32) uint32 {
	return tag >> 3
}

// buildIndex builds an index of the specified protobuf, return the index
// array and an error.
func buildIndex(buf []byte) ([]IndexEntry, error) {
	index := make([]IndexEntry, 0, 16)
	var lastProtoFieldNum uint32
	var outOfOrder bool

	var r BufferReader = NewBufferReader(buf)

	for !r.Done() {
		var tag uint32
		var err error
		var curPos = r.Pos
		// INLINED: tag, err = r.DecodeVarint32()
		{
			i := r.Pos
			buf := r.Buf

			if i >= len(buf) {
				return nil, errOutOfBounds
			} else if buf[i] < 0x80 {
				r.Pos++
				tag = uint32(buf[i])
			} else if r.Remaining() < 5 {
				var v uint64
				v, err = r.DecodeVarintSlow()
				tag = uint32(v)
			} else {
				var v uint32
				// we already checked the first byte
				tag = uint32(buf[i]) & 127
				i++

				v = uint32(buf[i])
				i++
				tag |= (v & 127) << 7
				if v < 128 {
					goto done
				}

				v = uint32(buf[i])
				i++
				tag |= (v & 127) << 14
				if v < 128 {
					goto done
				}

				v = uint32(buf[i])
				i++
				tag |= (v & 127) << 21
				if v < 128 {
					goto done
				}

				v = uint32(buf[i])
				i++
				tag |= (v & 127) << 28
				if v < 128 {
					goto done
				}

				return nil, errOutOfBounds

			done:
				r.Pos = i
			}
		}
		// DONE: tag, err = r.DecodeVarint32()

		fieldNum := protoFieldNumber(tag)
		if fieldNum < lastProtoFieldNum {
			outOfOrder = true
		}

		// Skip the current value -- will skip over an entire group as well.
		// INLINED: err = r.SkipValue(tag)
		wireType := tag & 0x7
		switch protowire.Type(wireType) {
		case protowire.VarintType:
			// INLINED: err = r.SkipVarint()
			i := r.Pos

			if len(r.Buf)-i < 10 {
				// Use DecodeVarintSlow() to skip while
				// checking for buffer overflow, but ignore result
				_, err = r.DecodeVarintSlow()
				goto out2
			}
			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			i++

			if r.Buf[i] < 0x80 {
				goto out
			}
			return nil, errOverflow
		out:
			r.Pos = i + 1
			// DONE: err = r.SkipVarint()
		case protowire.Fixed64Type:
			err = r.SkipFixed64()
		case protowire.BytesType:
			var n uint32
			n, err = r.DecodeVarint32()
			if err == nil {
				err = r.Skip(int(n))
			}
		case protowire.StartGroupType:
			err = r.SkipGroup(tag)
		case protowire.Fixed32Type:
			err = r.SkipFixed32()
		default:
			err = fmt.Errorf("Unexpected wire type (%d)", wireType)
		}
		// DONE: err = r.SkipValue(tag)

	out2:
		if err != nil {
			return nil, err
		}
		if fieldNum != lastProtoFieldNum {
			index = append(index, IndexEntry{FieldNum: fieldNum,
				Start: uint32(curPos),
				End:   uint32(r.Pos)},
			)
		} else {
			index[len(index)-1].End = uint32(r.Pos)
			index[len(index)-1].MultipleContiguous = true
		}
		lastProtoFieldNum = fieldNum
	}
	if outOfOrder {
		sort.Slice(index, func(i, j int) bool {
			return index[i].FieldNum < index[j].FieldNum ||
				(index[i].FieldNum == index[j].FieldNum &&
					index[i].Start < index[j].Start)
		})
	}
	return index, nil
}

func (lazy *XXX_lazyUnmarshalInfo) SizeField(num uint32) (size int) {
	start, end, found, _, multipleEntries := lazy.FindFieldInProto(num)
	if multipleEntries != nil {
		for _, entry := range multipleEntries {
			size += int(entry.End - entry.Start)
		}
		return size
	}
	if !found {
		return 0
	}
	return int(end - start)
}

func (lazy *XXX_lazyUnmarshalInfo) AppendField(b []byte, num uint32) ([]byte, bool) {
	start, end, found, _, multipleEntries := lazy.FindFieldInProto(num)
	if multipleEntries != nil {
		for _, entry := range multipleEntries {
			b = append(b, lazy.Protobuf[entry.Start:entry.End]...)
		}
		return b, true
	}
	if !found {
		return nil, false
	}
	b = append(b, lazy.Protobuf[start:end]...)
	return b, true
}

func (lazy *XXX_lazyUnmarshalInfo) SetIndex(index []IndexEntry) {
	atomicStoreIndex(&lazy.index, &index)
}

// FindFieldInProto looks for field fieldNum in lazyUnmarshalInfo information
// (including protobuf), returns startOffset/endOffset/found.
func (lazy *XXX_lazyUnmarshalInfo) FindFieldInProto(fieldNum uint32) (start, end uint32, found, multipleContiguous bool, multipleEntries []IndexEntry) {
	if lazy.Protobuf == nil {
		// There is no backing protobuf for this message -- it was made from a builder
		return 0, 0, false, false, nil
	}
	index := atomicLoadIndex(&lazy.index)
	if index == nil {
		r, err := buildIndex(lazy.Protobuf)
		if err != nil {
			panic(fmt.Sprintf("findFieldInfo: error building index when looking for field %d: %v", fieldNum, err))
		}
		// lazy.index is a pointer to the slice returned by BuildIndex
		index = &r
		atomicStoreIndex(&lazy.index, index)
	}
	return lookupField(index, fieldNum)
}

// lookupField returns the offset at which the indicated field starts using
// the index, offset immediately after field ends (including all instances of
// a repeated field), and bools indicating if field was found and if there
// are multiple encodings of the field in the byte range.
//
// To hande the uncommon case where there are repeated encodings for the same
// field which are not consecutive in the protobuf (so we need to returns
// multiple start/end offsets), we also return a slice multipleEntries.  If
// multipleEntries is non-nil, then multiple entries were found, and the
// values in the slice should be used, rather than start/end/found.
func lookupField(indexp *[]IndexEntry, fieldNum uint32) (start, end uint32, found bool, multipleContiguous bool, multipleEntries []IndexEntry) {
	// The pointer indexp to the index was already loaded atomically.
	// The slice is uniquely associated with the pointer, so it doesn't
	// need to be loaded atomically.
	index := *indexp
	for i, entry := range index {
		if fieldNum == entry.FieldNum {
			if i < len(index)-1 && entry.FieldNum == index[i+1].FieldNum {
				// Handle the uncommon case where there are
				// repeated entries for the same field which
				// are not contiguous in the protobuf.
				multiple := make([]IndexEntry, 1, 2)
				multiple[0] = IndexEntry{fieldNum, entry.Start, entry.End, entry.MultipleContiguous}
				i++
				for i < len(index) && index[i].FieldNum == fieldNum {
					multiple = append(multiple, IndexEntry{fieldNum, index[i].Start, index[i].End, index[i].MultipleContiguous})
					i++
				}
				return 0, 0, false, false, multiple

			}
			return entry.Start, entry.End, true, entry.MultipleContiguous, nil
		}
		if fieldNum < entry.FieldNum {
			return 0, 0, false, false, nil
		}
	}
	return 0, 0, false, false, nil
}
