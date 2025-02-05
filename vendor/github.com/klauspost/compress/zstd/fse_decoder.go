// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	tablelogAbsoluteMax = 9
)

const (
	/*!MEMORY_USAGE :
	 *  Memory usage formula : N->2^N Bytes (examples : 10 -> 1KB; 12 -> 4KB ; 16 -> 64KB; 20 -> 1MB; etc.)
	 *  Increasing memory usage improves compression ratio
	 *  Reduced memory usage can improve speed, due to cache effect
	 *  Recommended max value is 14, for 16KB, which nicely fits into Intel x86 L1 cache */
	maxMemoryUsage = tablelogAbsoluteMax + 2

	maxTableLog    = maxMemoryUsage - 2
	maxTablesize   = 1 << maxTableLog
	maxTableMask   = (1 << maxTableLog) - 1
	minTablelog    = 5
	maxSymbolValue = 255
)

// fseDecoder provides temporary storage for compression and decompression.
type fseDecoder struct {
	dt             [maxTablesize]decSymbol // Decompression table.
	symbolLen      uint16                  // Length of active part of the symbol table.
	actualTableLog uint8                   // Selected tablelog.
	maxBits        uint8                   // Maximum number of additional bits

	// used for table creation to avoid allocations.
	stateTable [256]uint16
	norm       [maxSymbolValue + 1]int16
	preDefined bool
}

// tableStep returns the next table index.
func tableStep(tableSize uint32) uint32 {
	return (tableSize >> 1) + (tableSize >> 3) + 3
}

// readNCount will read the symbol distribution so decoding tables can be constructed.
func (s *fseDecoder) readNCount(b *byteReader, maxSymbol uint16) error {
	var (
		charnum   uint16
		previous0 bool
	)
	if b.remain() < 4 {
		return errors.New("input too small")
	}
	bitStream := b.Uint32NC()
	nbBits := uint((bitStream & 0xF) + minTablelog) // extract tableLog
	if nbBits > tablelogAbsoluteMax {
		println("Invalid tablelog:", nbBits)
		return errors.New("tableLog too large")
	}
	bitStream >>= 4
	bitCount := uint(4)

	s.actualTableLog = uint8(nbBits)
	remaining := int32((1 << nbBits) + 1)
	threshold := int32(1 << nbBits)
	gotTotal := int32(0)
	nbBits++

	for remaining > 1 && charnum <= maxSymbol {
		if previous0 {
			//println("prev0")
			n0 := charnum
			for (bitStream & 0xFFFF) == 0xFFFF {
				//println("24 x 0")
				n0 += 24
				if r := b.remain(); r > 5 {
					b.advance(2)
					// The check above should make sure we can read 32 bits
					bitStream = b.Uint32NC() >> bitCount
				} else {
					// end of bit stream
					bitStream >>= 16
					bitCount += 16
				}
			}
			//printf("bitstream: %d, 0b%b", bitStream&3, bitStream)
			for (bitStream & 3) == 3 {
				n0 += 3
				bitStream >>= 2
				bitCount += 2
			}
			n0 += uint16(bitStream & 3)
			bitCount += 2

			if n0 > maxSymbolValue {
				return errors.New("maxSymbolValue too small")
			}
			//println("inserting ", n0-charnum, "zeroes from idx", charnum, "ending before", n0)
			for charnum < n0 {
				s.norm[uint8(charnum)] = 0
				charnum++
			}

			if r := b.remain(); r >= 7 || r-int(bitCount>>3) >= 4 {
				b.advance(bitCount >> 3)
				bitCount &= 7
				// The check above should make sure we can read 32 bits
				bitStream = b.Uint32NC() >> bitCount
			} else {
				bitStream >>= 2
			}
		}

		max := (2*threshold - 1) - remaining
		var count int32

		if int32(bitStream)&(threshold-1) < max {
			count = int32(bitStream) & (threshold - 1)
			if debugAsserts && nbBits < 1 {
				panic("nbBits underflow")
			}
			bitCount += nbBits - 1
		} else {
			count = int32(bitStream) & (2*threshold - 1)
			if count >= threshold {
				count -= max
			}
			bitCount += nbBits
		}

		// extra accuracy
		count--
		if count < 0 {
			// -1 means +1
			remaining += count
			gotTotal -= count
		} else {
			remaining -= count
			gotTotal += count
		}
		s.norm[charnum&0xff] = int16(count)
		charnum++
		previous0 = count == 0
		for remaining < threshold {
			nbBits--
			threshold >>= 1
		}

		if r := b.remain(); r >= 7 || r-int(bitCount>>3) >= 4 {
			b.advance(bitCount >> 3)
			bitCount &= 7
			// The check above should make sure we can read 32 bits
			bitStream = b.Uint32NC() >> (bitCount & 31)
		} else {
			bitCount -= (uint)(8 * (len(b.b) - 4 - b.off))
			b.off = len(b.b) - 4
			bitStream = b.Uint32() >> (bitCount & 31)
		}
	}
	s.symbolLen = charnum
	if s.symbolLen <= 1 {
		return fmt.Errorf("symbolLen (%d) too small", s.symbolLen)
	}
	if s.symbolLen > maxSymbolValue+1 {
		return fmt.Errorf("symbolLen (%d) too big", s.symbolLen)
	}
	if remaining != 1 {
		return fmt.Errorf("corruption detected (remaining %d != 1)", remaining)
	}
	if bitCount > 32 {
		return fmt.Errorf("corruption detected (bitCount %d > 32)", bitCount)
	}
	if gotTotal != 1<<s.actualTableLog {
		return fmt.Errorf("corruption detected (total %d != %d)", gotTotal, 1<<s.actualTableLog)
	}
	b.advance((bitCount + 7) >> 3)
	return s.buildDtable()
}

func (s *fseDecoder) mustReadFrom(r io.Reader) {
	fatalErr := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	// 	dt             [maxTablesize]decSymbol // Decompression table.
	//	symbolLen      uint16                  // Length of active part of the symbol table.
	//	actualTableLog uint8                   // Selected tablelog.
	//	maxBits        uint8                   // Maximum number of additional bits
	//	// used for table creation to avoid allocations.
	//	stateTable [256]uint16
	//	norm       [maxSymbolValue + 1]int16
	//	preDefined bool
	fatalErr(binary.Read(r, binary.LittleEndian, &s.dt))
	fatalErr(binary.Read(r, binary.LittleEndian, &s.symbolLen))
	fatalErr(binary.Read(r, binary.LittleEndian, &s.actualTableLog))
	fatalErr(binary.Read(r, binary.LittleEndian, &s.maxBits))
	fatalErr(binary.Read(r, binary.LittleEndian, &s.stateTable))
	fatalErr(binary.Read(r, binary.LittleEndian, &s.norm))
	fatalErr(binary.Read(r, binary.LittleEndian, &s.preDefined))
}

// decSymbol contains information about a state entry,
// Including the state offset base, the output symbol and
// the number of bits to read for the low part of the destination state.
// Using a composite uint64 is faster than a struct with separate members.
type decSymbol uint64

func newDecSymbol(nbits, addBits uint8, newState uint16, baseline uint32) decSymbol {
	return decSymbol(nbits) | (decSymbol(addBits) << 8) | (decSymbol(newState) << 16) | (decSymbol(baseline) << 32)
}

func (d decSymbol) nbBits() uint8 {
	return uint8(d)
}

func (d decSymbol) addBits() uint8 {
	return uint8(d >> 8)
}

func (d decSymbol) newState() uint16 {
	return uint16(d >> 16)
}

func (d decSymbol) baselineInt() int {
	return int(d >> 32)
}

func (d *decSymbol) setNBits(nBits uint8) {
	const mask = 0xffffffffffffff00
	*d = (*d & mask) | decSymbol(nBits)
}

func (d *decSymbol) setAddBits(addBits uint8) {
	const mask = 0xffffffffffff00ff
	*d = (*d & mask) | (decSymbol(addBits) << 8)
}

func (d *decSymbol) setNewState(state uint16) {
	const mask = 0xffffffff0000ffff
	*d = (*d & mask) | decSymbol(state)<<16
}

func (d *decSymbol) setExt(addBits uint8, baseline uint32) {
	const mask = 0xffff00ff
	*d = (*d & mask) | (decSymbol(addBits) << 8) | (decSymbol(baseline) << 32)
}

// decSymbolValue returns the transformed decSymbol for the given symbol.
func decSymbolValue(symb uint8, t []baseOffset) (decSymbol, error) {
	if int(symb) >= len(t) {
		return 0, fmt.Errorf("rle symbol %d >= max %d", symb, len(t))
	}
	lu := t[symb]
	return newDecSymbol(0, lu.addBits, 0, lu.baseLine), nil
}

// setRLE will set the decoder til RLE mode.
func (s *fseDecoder) setRLE(symbol decSymbol) {
	s.actualTableLog = 0
	s.maxBits = symbol.addBits()
	s.dt[0] = symbol
}

// transform will transform the decoder table into a table usable for
// decoding without having to apply the transformation while decoding.
// The state will contain the base value and the number of bits to read.
func (s *fseDecoder) transform(t []baseOffset) error {
	tableSize := uint16(1 << s.actualTableLog)
	s.maxBits = 0
	for i, v := range s.dt[:tableSize] {
		add := v.addBits()
		if int(add) >= len(t) {
			return fmt.Errorf("invalid decoding table entry %d, symbol %d >= max (%d)", i, v.addBits(), len(t))
		}
		lu := t[add]
		if lu.addBits > s.maxBits {
			s.maxBits = lu.addBits
		}
		v.setExt(lu.addBits, lu.baseLine)
		s.dt[i] = v
	}
	return nil
}

type fseState struct {
	dt    []decSymbol
	state decSymbol
}

// Initialize and decodeAsync first state and symbol.
func (s *fseState) init(br *bitReader, tableLog uint8, dt []decSymbol) {
	s.dt = dt
	br.fill()
	s.state = dt[br.getBits(tableLog)]
}

// final returns the current state symbol without decoding the next.
func (s decSymbol) final() (int, uint8) {
	return s.baselineInt(), s.addBits()
}
