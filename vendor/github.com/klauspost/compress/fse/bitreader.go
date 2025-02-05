// Copyright 2018 Klaus Post. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Based on work Copyright (c) 2013, Yann Collet, released under BSD License.

package fse

import (
	"encoding/binary"
	"errors"
	"io"
)

// bitReader reads a bitstream in reverse.
// The last set bit indicates the start of the stream and is used
// for aligning the input.
type bitReader struct {
	in       []byte
	off      uint // next byte to read is at in[off - 1]
	value    uint64
	bitsRead uint8
}

// init initializes and resets the bit reader.
func (b *bitReader) init(in []byte) error {
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
	b.bitsRead += 8 - uint8(highBits(uint32(v)))
	return nil
}

// getBits will return n bits. n can be 0.
func (b *bitReader) getBits(n uint8) uint16 {
	if n == 0 || b.bitsRead >= 64 {
		return 0
	}
	return b.getBitsFast(n)
}

// getBitsFast requires that at least one bit is requested every time.
// There are no checks if the buffer is filled.
func (b *bitReader) getBitsFast(n uint8) uint16 {
	const regMask = 64 - 1
	v := uint16((b.value << (b.bitsRead & regMask)) >> ((regMask + 1 - n) & regMask))
	b.bitsRead += n
	return v
}

// fillFast() will make sure at least 32 bits are available.
// There must be at least 4 bytes available.
func (b *bitReader) fillFast() {
	if b.bitsRead < 32 {
		return
	}
	// 2 bounds checks.
	v := b.in[b.off-4:]
	v = v[:4]
	low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
	b.value = (b.value << 32) | uint64(low)
	b.bitsRead -= 32
	b.off -= 4
}

// fill() will make sure at least 32 bits are available.
func (b *bitReader) fill() {
	if b.bitsRead < 32 {
		return
	}
	if b.off > 4 {
		v := b.in[b.off-4:]
		v = v[:4]
		low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
		b.value = (b.value << 32) | uint64(low)
		b.bitsRead -= 32
		b.off -= 4
		return
	}
	for b.off > 0 {
		b.value = (b.value << 8) | uint64(b.in[b.off-1])
		b.bitsRead -= 8
		b.off--
	}
}

// fillFastStart() assumes the bitreader is empty and there is at least 8 bytes to read.
func (b *bitReader) fillFastStart() {
	// Do single re-slice to avoid bounds checks.
	b.value = binary.LittleEndian.Uint64(b.in[b.off-8:])
	b.bitsRead = 0
	b.off -= 8
}

// finished returns true if all bits have been read from the bit stream.
func (b *bitReader) finished() bool {
	return b.bitsRead >= 64 && b.off == 0
}

// close the bitstream and returns an error if out-of-buffer reads occurred.
func (b *bitReader) close() error {
	// Release reference.
	b.in = nil
	if b.bitsRead > 64 {
		return io.ErrUnexpectedEOF
	}
	return nil
}
