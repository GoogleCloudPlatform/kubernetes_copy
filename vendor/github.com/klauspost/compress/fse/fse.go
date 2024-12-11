// Copyright 2018 Klaus Post. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Based on work Copyright (c) 2013, Yann Collet, released under BSD License.

// Package fse provides Finite State Entropy encoding and decoding.
//
// Finite State Entropy encoding provides a fast near-optimal symbol encoding/decoding
// for byte blocks as implemented in zstd.
//
// See https://github.com/klauspost/compress/tree/master/fse for more information.
package fse

import (
	"errors"
	"fmt"
	"math/bits"
)

const (
	/*!MEMORY_USAGE :
	 *  Memory usage formula : N->2^N Bytes (examples : 10 -> 1KB; 12 -> 4KB ; 16 -> 64KB; 20 -> 1MB; etc.)
	 *  Increasing memory usage improves compression ratio
	 *  Reduced memory usage can improve speed, due to cache effect
	 *  Recommended max value is 14, for 16KB, which nicely fits into Intel x86 L1 cache */
	maxMemoryUsage     = 14
	defaultMemoryUsage = 13

	maxTableLog     = maxMemoryUsage - 2
	maxTablesize    = 1 << maxTableLog
	defaultTablelog = defaultMemoryUsage - 2
	minTablelog     = 5
	maxSymbolValue  = 255
)

var (
	// ErrIncompressible is returned when input is judged to be too hard to compress.
	ErrIncompressible = errors.New("input is not compressible")

	// ErrUseRLE is returned from the compressor when the input is a single byte value repeated.
	ErrUseRLE = errors.New("input is single value repeated")
)

// Scratch provides temporary storage for compression and decompression.
type Scratch struct {
	// Private
	count    [maxSymbolValue + 1]uint32
	norm     [maxSymbolValue + 1]int16
	br       byteReader
	bits     bitReader
	bw       bitWriter
	ct       cTable      // Compression tables.
	decTable []decSymbol // Decompression table.
	maxCount int         // count of the most probable symbol

	// Per block parameters.
	// These can be used to override compression parameters of the block.
	// Do not touch, unless you know what you are doing.

	// Out is output buffer.
	// If the scratch is re-used before the caller is done processing the output,
	// set this field to nil.
	// Otherwise the output buffer will be re-used for next Compression/Decompression step
	// and allocation will be avoided.
	Out []byte

	// DecompressLimit limits the maximum decoded size acceptable.
	// If > 0 decompression will stop when approximately this many bytes
	// has been decoded.
	// If 0, maximum size will be 2GB.
	DecompressLimit int

	symbolLen      uint16 // Length of active part of the symbol table.
	actualTableLog uint8  // Selected tablelog.
	zeroBits       bool   // no bits has prob > 50%.
	clearCount     bool   // clear count

	// MaxSymbolValue will override the maximum symbol value of the next block.
	MaxSymbolValue uint8

	// TableLog will attempt to override the tablelog for the next block.
	TableLog uint8
}

// Histogram allows to populate the histogram and skip that step in the compression,
// It otherwise allows to inspect the histogram when compression is done.
// To indicate that you have populated the histogram call HistogramFinished
// with the value of the highest populated symbol, as well as the number of entries
// in the most populated entry. These are accepted at face value.
// The returned slice will always be length 256.
func (s *Scratch) Histogram() []uint32 {
	return s.count[:]
}

// HistogramFinished can be called to indicate that the histogram has been populated.
// maxSymbol is the index of the highest set symbol of the next data segment.
// maxCount is the number of entries in the most populated entry.
// These are accepted at face value.
func (s *Scratch) HistogramFinished(maxSymbol uint8, maxCount int) {
	s.maxCount = maxCount
	s.symbolLen = uint16(maxSymbol) + 1
	s.clearCount = maxCount != 0
}

// prepare will prepare and allocate scratch tables used for both compression and decompression.
func (s *Scratch) prepare(in []byte) (*Scratch, error) {
	if s == nil {
		s = &Scratch{}
	}
	if s.MaxSymbolValue == 0 {
		s.MaxSymbolValue = 255
	}
	if s.TableLog == 0 {
		s.TableLog = defaultTablelog
	}
	if s.TableLog > maxTableLog {
		return nil, fmt.Errorf("tableLog (%d) > maxTableLog (%d)", s.TableLog, maxTableLog)
	}
	if cap(s.Out) == 0 {
		s.Out = make([]byte, 0, len(in))
	}
	if s.clearCount && s.maxCount == 0 {
		for i := range s.count {
			s.count[i] = 0
		}
		s.clearCount = false
	}
	s.br.init(in)
	if s.DecompressLimit == 0 {
		// Max size 2GB.
		s.DecompressLimit = (2 << 30) - 1
	}

	return s, nil
}

// tableStep returns the next table index.
func tableStep(tableSize uint32) uint32 {
	return (tableSize >> 1) + (tableSize >> 3) + 3
}

func highBits(val uint32) (n uint32) {
	return uint32(bits.Len32(val) - 1)
}
