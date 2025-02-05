// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Helper code for parsing a protocol buffer

package protolazy

import (
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
)

// BufferReader is a structure encapsulating a protobuf and a current position
type BufferReader struct {
	Buf []byte
	Pos int
}

// NewBufferReader creates a new BufferRead from a protobuf
func NewBufferReader(buf []byte) BufferReader {
	return BufferReader{Buf: buf, Pos: 0}
}

var errOutOfBounds = errors.New("protobuf decoding: out of bounds")
var errOverflow = errors.New("proto: integer overflow")

func (b *BufferReader) DecodeVarintSlow() (x uint64, err error) {
	i := b.Pos
	l := len(b.Buf)

	for shift := uint(0); shift < 64; shift += 7 {
		if i >= l {
			err = io.ErrUnexpectedEOF
			return
		}
		v := b.Buf[i]
		i++
		x |= (uint64(v) & 0x7F) << shift
		if v < 0x80 {
			b.Pos = i
			return
		}
	}

	// The number is too large to represent in a 64-bit value.
	err = errOverflow
	return
}

// decodeVarint decodes a varint at the current position
func (b *BufferReader) DecodeVarint() (x uint64, err error) {
	i := b.Pos
	buf := b.Buf

	if i >= len(buf) {
		return 0, io.ErrUnexpectedEOF
	} else if buf[i] < 0x80 {
		b.Pos++
		return uint64(buf[i]), nil
	} else if len(buf)-i < 10 {
		return b.DecodeVarintSlow()
	}

	var v uint64
	// we already checked the first byte
	x = uint64(buf[i]) & 127
	i++

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 7
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 14
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 21
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 28
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 35
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 42
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 49
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 56
	if v < 128 {
		goto done
	}

	v = uint64(buf[i])
	i++
	x |= (v & 127) << 63
	if v < 128 {
		goto done
	}

	return 0, errOverflow

done:
	b.Pos = i
	return
}

// decodeVarint32 decodes a varint32 at the current position
func (b *BufferReader) DecodeVarint32() (x uint32, err error) {
	i := b.Pos
	buf := b.Buf

	if i >= len(buf) {
		return 0, io.ErrUnexpectedEOF
	} else if buf[i] < 0x80 {
		b.Pos++
		return uint32(buf[i]), nil
	} else if len(buf)-i < 5 {
		v, err := b.DecodeVarintSlow()
		return uint32(v), err
	}

	var v uint32
	// we already checked the first byte
	x = uint32(buf[i]) & 127
	i++

	v = uint32(buf[i])
	i++
	x |= (v & 127) << 7
	if v < 128 {
		goto done
	}

	v = uint32(buf[i])
	i++
	x |= (v & 127) << 14
	if v < 128 {
		goto done
	}

	v = uint32(buf[i])
	i++
	x |= (v & 127) << 21
	if v < 128 {
		goto done
	}

	v = uint32(buf[i])
	i++
	x |= (v & 127) << 28
	if v < 128 {
		goto done
	}

	return 0, errOverflow

done:
	b.Pos = i
	return
}

// skipValue skips a value in the protobuf, based on the specified tag
func (b *BufferReader) SkipValue(tag uint32) (err error) {
	wireType := tag & 0x7
	switch protowire.Type(wireType) {
	case protowire.VarintType:
		err = b.SkipVarint()
	case protowire.Fixed64Type:
		err = b.SkipFixed64()
	case protowire.BytesType:
		var n uint32
		n, err = b.DecodeVarint32()
		if err == nil {
			err = b.Skip(int(n))
		}
	case protowire.StartGroupType:
		err = b.SkipGroup(tag)
	case protowire.Fixed32Type:
		err = b.SkipFixed32()
	default:
		err = fmt.Errorf("Unexpected wire type (%d)", wireType)
	}
	return
}

// skipGroup skips a group with the specified tag.  It executes efficiently using a tag stack
func (b *BufferReader) SkipGroup(tag uint32) (err error) {
	tagStack := make([]uint32, 0, 16)
	tagStack = append(tagStack, tag)
	var n uint32
	for len(tagStack) > 0 {
		tag, err = b.DecodeVarint32()
		if err != nil {
			return err
		}
		switch protowire.Type(tag & 0x7) {
		case protowire.VarintType:
			err = b.SkipVarint()
		case protowire.Fixed64Type:
			err = b.Skip(8)
		case protowire.BytesType:
			n, err = b.DecodeVarint32()
			if err == nil {
				err = b.Skip(int(n))
			}
		case protowire.StartGroupType:
			tagStack = append(tagStack, tag)
		case protowire.Fixed32Type:
			err = b.SkipFixed32()
		case protowire.EndGroupType:
			if protoFieldNumber(tagStack[len(tagStack)-1]) == protoFieldNumber(tag) {
				tagStack = tagStack[:len(tagStack)-1]
			} else {
				err = fmt.Errorf("end group tag %d does not match begin group tag %d at pos %d",
					protoFieldNumber(tag), protoFieldNumber(tagStack[len(tagStack)-1]), b.Pos)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// skipVarint effiently skips a varint
func (b *BufferReader) SkipVarint() (err error) {
	i := b.Pos

	if len(b.Buf)-i < 10 {
		// Use DecodeVarintSlow() to check for buffer overflow, but ignore result
		if _, err := b.DecodeVarintSlow(); err != nil {
			return err
		}
		return nil
	}

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	i++

	if b.Buf[i] < 0x80 {
		goto out
	}
	return errOverflow

out:
	b.Pos = i + 1
	return nil
}

// skip skips the specified number of bytes
func (b *BufferReader) Skip(n int) (err error) {
	if len(b.Buf) < b.Pos+n {
		return io.ErrUnexpectedEOF
	}
	b.Pos += n
	return
}

// skipFixed64 skips a fixed64
func (b *BufferReader) SkipFixed64() (err error) {
	return b.Skip(8)
}

// skipFixed32 skips a fixed32
func (b *BufferReader) SkipFixed32() (err error) {
	return b.Skip(4)
}

// skipBytes skips a set of bytes
func (b *BufferReader) SkipBytes() (err error) {
	n, err := b.DecodeVarint32()
	if err != nil {
		return err
	}
	return b.Skip(int(n))
}

// Done returns whether we are at the end of the protobuf
func (b *BufferReader) Done() bool {
	return b.Pos == len(b.Buf)
}

// Remaining returns how many bytes remain
func (b *BufferReader) Remaining() int {
	return len(b.Buf) - b.Pos
}
