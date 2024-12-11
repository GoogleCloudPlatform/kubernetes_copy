// Copyright 2018 Klaus Post. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Based on work Copyright (c) 2013, Yann Collet, released under BSD License.

package huff0

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// bitReader reads a bitstream in reverse.
// The last set bit indicates the start of the stream and is used
// for aligning the input.
type bitReaderBytes struct {
	in       []byte
	off      uint // next byte to read is at in[off - 1]
	value    uint64
	bitsRead uint8
}

// init initializes and resets the bit reader.
func (b *bitReaderBytes) init(in []byte) error {
	if len(in) < 1 {
		return errors.New("corrupt stream: too short")
	}
	b.in = in
	b.off = uint(len(in))
	// The highest bit of the last byte indicates where to start
	v := in[len(in)-1]
	if v == 0 {
		return errors.New("corrupt stream, did not find end of stream")
	}
	b.bitsRead = 64
	b.value = 0
	if len(in) >= 8 {
		b.fillFastStart()
	} else {
		b.fill()
		b.fill()
	}
	b.advance(8 - uint8(highBit32(uint32(v))))
	return nil
}

// peekBitsFast requires that at least one bit is requested every time.
// There are no checks if the buffer is filled.
func (b *bitReaderBytes) peekByteFast() uint8 {
	got := uint8(b.value >> 56)
	return got
}

func (b *bitReaderBytes) advance(n uint8) {
	b.bitsRead += n
	b.value <<= n & 63
}

// fillFast() will make sure at least 32 bits are available.
// There must be at least 4 bytes available.
func (b *bitReaderBytes) fillFast() {
	if b.bitsRead < 32 {
		return
	}

	// 2 bounds checks.
	v := b.in[b.off-4 : b.off]
	low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
	b.value |= uint64(low) << (b.bitsRead - 32)
	b.bitsRead -= 32
	b.off -= 4
}

// fillFastStart() assumes the bitReaderBytes is empty and there is at least 8 bytes to read.
func (b *bitReaderBytes) fillFastStart() {
	// Do single re-slice to avoid bounds checks.
	b.value = binary.LittleEndian.Uint64(b.in[b.off-8:])
	b.bitsRead = 0
	b.off -= 8
}

// fill() will make sure at least 32 bits are available.
func (b *bitReaderBytes) fill() {
	if b.bitsRead < 32 {
		return
	}
	if b.off > 4 {
		v := b.in[b.off-4 : b.off]
		low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
		b.value |= uint64(low) << (b.bitsRead - 32)
		b.bitsRead -= 32
		b.off -= 4
		return
	}
	for b.off > 0 {
		b.value |= uint64(b.in[b.off-1]) << (b.bitsRead - 8)
		b.bitsRead -= 8
		b.off--
	}
}

// finished returns true if all bits have been read from the bit stream.
func (b *bitReaderBytes) finished() bool {
	return b.off == 0 && b.bitsRead >= 64
}

func (b *bitReaderBytes) remaining() uint {
	return b.off*8 + uint(64-b.bitsRead)
}

// close the bitstream and returns an error if out-of-buffer reads occurred.
func (b *bitReaderBytes) close() error {
	// Release reference.
	b.in = nil
	if b.remaining() > 0 {
		return fmt.Errorf("corrupt input: %d bits remain on stream", b.remaining())
	}
	if b.bitsRead > 64 {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// bitReaderShifted reads a bitstream in reverse.
// The last set bit indicates the start of the stream and is used
// for aligning the input.
type bitReaderShifted struct {
	in       []byte
	off      uint // next byte to read is at in[off - 1]
	value    uint64
	bitsRead uint8
}

// init initializes and resets the bit reader.
func (b *bitReaderShifted) init(in []byte) error {
	if len(in) < 1 {
		return errors.New("corrupt stream: too short")
	}
	b.in = in
	b.off = uint(len(in))
	// The highest bit of the last byte indicates where to start
	v := in[len(in)-1]
	if v == 0 {
		return errors.New("corrupt stream, did not find end of stream")
	}
	b.bitsRead = 64
	b.value = 0
	if len(in) >= 8 {
		b.fillFastStart()
	} else {
		b.fill()
		b.fill()
	}
	b.advance(8 - uint8(highBit32(uint32(v))))
	return nil
}

// peekBitsFast requires that at least one bit is requested every time.
// There are no checks if the buffer is filled.
func (b *bitReaderShifted) peekBitsFast(n uint8) uint16 {
	return uint16(b.value >> ((64 - n) & 63))
}

func (b *bitReaderShifted) advance(n uint8) {
	b.bitsRead += n
	b.value <<= n & 63
}

// fillFast() will make sure at least 32 bits are available.
// There must be at least 4 bytes available.
func (b *bitReaderShifted) fillFast() {
	if b.bitsRead < 32 {
		return
	}

	// 2 bounds checks.
	v := b.in[b.off-4 : b.off]
	low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
	b.value |= uint64(low) << ((b.bitsRead - 32) & 63)
	b.bitsRead -= 32
	b.off -= 4
}

// fillFastStart() assumes the bitReaderShifted is empty and there is at least 8 bytes to read.
func (b *bitReaderShifted) fillFastStart() {
	// Do single re-slice to avoid bounds checks.
	b.value = binary.LittleEndian.Uint64(b.in[b.off-8:])
	b.bitsRead = 0
	b.off -= 8
}

// fill() will make sure at least 32 bits are available.
func (b *bitReaderShifted) fill() {
	if b.bitsRead < 32 {
		return
	}
	if b.off > 4 {
		v := b.in[b.off-4 : b.off]
		low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
		b.value |= uint64(low) << ((b.bitsRead - 32) & 63)
		b.bitsRead -= 32
		b.off -= 4
		return
	}
	for b.off > 0 {
		b.value |= uint64(b.in[b.off-1]) << ((b.bitsRead - 8) & 63)
		b.bitsRead -= 8
		b.off--
	}
}

func (b *bitReaderShifted) remaining() uint {
	return b.off*8 + uint(64-b.bitsRead)
}

// close the bitstream and returns an error if out-of-buffer reads occurred.
func (b *bitReaderShifted) close() error {
	// Release reference.
	b.in = nil
	if b.remaining() > 0 {
		return fmt.Errorf("corrupt input: %d bits remain on stream", b.remaining())
	}
	if b.bitsRead > 64 {
		return io.ErrUnexpectedEOF
	}
	return nil
}
