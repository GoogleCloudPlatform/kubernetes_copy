// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"fmt"
	"math"
	"sync"
)

var (
	// fsePredef are the predefined fse tables as defined here:
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#default-distributions
	// These values are already transformed.
	fsePredef [3]fseDecoder

	// fsePredefEnc are the predefined encoder based on fse tables as defined here:
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#default-distributions
	// These values are already transformed.
	fsePredefEnc [3]fseEncoder

	// symbolTableX contain the transformations needed for each type as defined in
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#the-codes-for-literals-lengths-match-lengths-and-offsets
	symbolTableX [3][]baseOffset

	// maxTableSymbol is the biggest supported symbol for each table type
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#the-codes-for-literals-lengths-match-lengths-and-offsets
	maxTableSymbol = [3]uint8{tableLiteralLengths: maxLiteralLengthSymbol, tableOffsets: maxOffsetLengthSymbol, tableMatchLengths: maxMatchLengthSymbol}

	// bitTables is the bits table for each table.
	bitTables = [3][]byte{tableLiteralLengths: llBitsTable[:], tableOffsets: nil, tableMatchLengths: mlBitsTable[:]}
)

type tableIndex uint8

const (
	// indexes for fsePredef and symbolTableX
	tableLiteralLengths tableIndex = 0
	tableOffsets        tableIndex = 1
	tableMatchLengths   tableIndex = 2

	maxLiteralLengthSymbol = 35
	maxOffsetLengthSymbol  = 30
	maxMatchLengthSymbol   = 52
)

// baseOffset is used for calculating transformations.
type baseOffset struct {
	baseLine uint32
	addBits  uint8
}

// fillBase will precalculate base offsets with the given bit distributions.
func fillBase(dst []baseOffset, base uint32, bits ...uint8) {
	if len(bits) != len(dst) {
		panic(fmt.Sprintf("len(dst) (%d) != len(bits) (%d)", len(dst), len(bits)))
	}
	for i, bit := range bits {
		if base > math.MaxInt32 {
			panic("invalid decoding table, base overflows int32")
		}

		dst[i] = baseOffset{
			baseLine: base,
			addBits:  bit,
		}
		base += 1 << bit
	}
}

var predef sync.Once

func initPredefined() {
	predef.Do(func() {
		// Literals length codes
		tmp := make([]baseOffset, 36)
		for i := range tmp[:16] {
			tmp[i] = baseOffset{
				baseLine: uint32(i),
				addBits:  0,
			}
		}
		fillBase(tmp[16:], 16, 1, 1, 1, 1, 2, 2, 3, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)
		symbolTableX[tableLiteralLengths] = tmp

		// Match length codes
		tmp = make([]baseOffset, 53)
		for i := range tmp[:32] {
			tmp[i] = baseOffset{
				// The transformation adds the 3 length.
				baseLine: uint32(i) + 3,
				addBits:  0,
			}
		}
		fillBase(tmp[32:], 35, 1, 1, 1, 1, 2, 2, 3, 3, 4, 4, 5, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)
		symbolTableX[tableMatchLengths] = tmp

		// Offset codes
		tmp = make([]baseOffset, maxOffsetBits+1)
		tmp[1] = baseOffset{
			baseLine: 1,
			addBits:  1,
		}
		fillBase(tmp[2:], 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30)
		symbolTableX[tableOffsets] = tmp

		// Fill predefined tables and transform them.
		// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#default-distributions
		for i := range fsePredef[:] {
			f := &fsePredef[i]
			switch tableIndex(i) {
			case tableLiteralLengths:
				// https://github.com/facebook/zstd/blob/ededcfca57366461021c922720878c81a5854a0a/lib/decompress/zstd_decompress_block.c#L243
				f.actualTableLog = 6
				copy(f.norm[:], []int16{4, 3, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1, 1, 1,
					2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 2, 1, 1, 1, 1, 1,
					-1, -1, -1, -1})
				f.symbolLen = 36
			case tableOffsets:
				// https://github.com/facebook/zstd/blob/ededcfca57366461021c922720878c81a5854a0a/lib/decompress/zstd_decompress_block.c#L281
				f.actualTableLog = 5
				copy(f.norm[:], []int16{
					1, 1, 1, 1, 1, 1, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
					1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1})
				f.symbolLen = 29
			case tableMatchLengths:
				//https://github.com/facebook/zstd/blob/ededcfca57366461021c922720878c81a5854a0a/lib/decompress/zstd_decompress_block.c#L304
				f.actualTableLog = 6
				copy(f.norm[:], []int16{
					1, 4, 3, 2, 2, 2, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
					1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
					1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1,
					-1, -1, -1, -1, -1})
				f.symbolLen = 53
			}
			if err := f.buildDtable(); err != nil {
				panic(fmt.Errorf("building table %v: %v", tableIndex(i), err))
			}
			if err := f.transform(symbolTableX[i]); err != nil {
				panic(fmt.Errorf("building table %v: %v", tableIndex(i), err))
			}
			f.preDefined = true

			// Create encoder as well
			enc := &fsePredefEnc[i]
			copy(enc.norm[:], f.norm[:])
			enc.symbolLen = f.symbolLen
			enc.actualTableLog = f.actualTableLog
			if err := enc.buildCTable(); err != nil {
				panic(fmt.Errorf("building encoding table %v: %v", tableIndex(i), err))
			}
			enc.setBits(bitTables[i])
			enc.preDefined = true
		}
	})
}
