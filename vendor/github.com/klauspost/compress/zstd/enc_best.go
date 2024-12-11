// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"bytes"
	"fmt"

	"github.com/klauspost/compress"
)

const (
	bestLongTableBits = 22                     // Bits used in the long match table
	bestLongTableSize = 1 << bestLongTableBits // Size of the table
	bestLongLen       = 8                      // Bytes used for table hash

	// Note: Increasing the short table bits or making the hash shorter
	// can actually lead to compression degradation since it will 'steal' more from the
	// long match table and match offsets are quite big.
	// This greatly depends on the type of input.
	bestShortTableBits = 18                      // Bits used in the short match table
	bestShortTableSize = 1 << bestShortTableBits // Size of the table
	bestShortLen       = 4                       // Bytes used for table hash

)

type match struct {
	offset int32
	s      int32
	length int32
	rep    int32
	est    int32
}

const highScore = maxMatchLen * 8

// estBits will estimate output bits from predefined tables.
func (m *match) estBits(bitsPerByte int32) {
	mlc := mlCode(uint32(m.length - zstdMinMatch))
	var ofc uint8
	if m.rep < 0 {
		ofc = ofCode(uint32(m.s-m.offset) + 3)
	} else {
		ofc = ofCode(uint32(m.rep) & 3)
	}
	// Cost, excluding
	ofTT, mlTT := fsePredefEnc[tableOffsets].ct.symbolTT[ofc], fsePredefEnc[tableMatchLengths].ct.symbolTT[mlc]

	// Add cost of match encoding...
	m.est = int32(ofTT.outBits + mlTT.outBits)
	m.est += int32(ofTT.deltaNbBits>>16 + mlTT.deltaNbBits>>16)
	// Subtract savings compared to literal encoding...
	m.est -= (m.length * bitsPerByte) >> 10
	if m.est > 0 {
		// Unlikely gain..
		m.length = 0
		m.est = highScore
	}
}

// bestFastEncoder uses 2 tables, one for short matches (5 bytes) and one for long matches.
// The long match table contains the previous entry with the same hash,
// effectively making it a "chain" of length 2.
// When we find a long match we choose between the two values and select the longest.
// When we find a short match, after checking the long, we check if we can find a long at n+1
// and that it is longer (lazy matching).
type bestFastEncoder struct {
	fastBase
	table         [bestShortTableSize]prevEntry
	longTable     [bestLongTableSize]prevEntry
	dictTable     []prevEntry
	dictLongTable []prevEntry
}

// Encode improves compression...
func (e *bestFastEncoder) Encode(blk *blockEnc, src []byte) {
	const (
		// Input margin is the number of bytes we read (8)
		// and the maximum we will read ahead (2)
		inputMargin            = 8 + 4
		minNonLiteralBlockSize = 16
	)

	// Protect against e.cur wraparound.
	for e.cur >= e.bufferReset-int32(len(e.hist)) {
		if len(e.hist) == 0 {
			e.table = [bestShortTableSize]prevEntry{}
			e.longTable = [bestLongTableSize]prevEntry{}
			e.cur = e.maxMatchOff
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - e.maxMatchOff
		for i := range e.table[:] {
			v := e.table[i].offset
			v2 := e.table[i].prev
			if v < minOff {
				v = 0
				v2 = 0
			} else {
				v = v - e.cur + e.maxMatchOff
				if v2 < minOff {
					v2 = 0
				} else {
					v2 = v2 - e.cur + e.maxMatchOff
				}
			}
			e.table[i] = prevEntry{
				offset: v,
				prev:   v2,
			}
		}
		for i := range e.longTable[:] {
			v := e.longTable[i].offset
			v2 := e.longTable[i].prev
			if v < minOff {
				v = 0
				v2 = 0
			} else {
				v = v - e.cur + e.maxMatchOff
				if v2 < minOff {
					v2 = 0
				} else {
					v2 = v2 - e.cur + e.maxMatchOff
				}
			}
			e.longTable[i] = prevEntry{
				offset: v,
				prev:   v2,
			}
		}
		e.cur = e.maxMatchOff
		break
	}

	// Add block to history
	s := e.addBlock(src)
	blk.size = len(src)

	// Check RLE first
	if len(src) > zstdMinMatch {
		ml := matchLen(src[1:], src)
		if ml == len(src)-1 {
			blk.literals = append(blk.literals, src[0])
			blk.sequences = append(blk.sequences, seq{litLen: 1, matchLen: uint32(len(src)-1) - zstdMinMatch, offset: 1 + 3})
			return
		}
	}

	if len(src) < minNonLiteralBlockSize {
		blk.extraLits = len(src)
		blk.literals = blk.literals[:len(src)]
		copy(blk.literals, src)
		return
	}

	// Use this to estimate literal cost.
	// Scaled by 10 bits.
	bitsPerByte := int32((compress.ShannonEntropyBits(src) * 1024) / len(src))
	// Huffman can never go < 1 bit/byte
	if bitsPerByte < 1024 {
		bitsPerByte = 1024
	}

	// Override src
	src = e.hist
	sLimit := int32(len(src)) - inputMargin
	const kSearchStrength = 10

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := s

	// Relative offsets
	offset1 := int32(blk.recentOffsets[0])
	offset2 := int32(blk.recentOffsets[1])
	offset3 := int32(blk.recentOffsets[2])

	addLiterals := func(s *seq, until int32) {
		if until == nextEmit {
			return
		}
		blk.literals = append(blk.literals, src[nextEmit:until]...)
		s.litLen = uint32(until - nextEmit)
	}

	if debugEncoder {
		println("recent offsets:", blk.recentOffsets)
	}

encodeLoop:
	for {
		// We allow the encoder to optionally turn off repeat offsets across blocks
		canRepeat := len(blk.sequences) > 2

		if debugAsserts && canRepeat && offset1 == 0 {
			panic("offset0 was 0")
		}

		const goodEnough = 250

		cv := load6432(src, s)

		nextHashL := hashLen(cv, bestLongTableBits, bestLongLen)
		nextHashS := hashLen(cv, bestShortTableBits, bestShortLen)
		candidateL := e.longTable[nextHashL]
		candidateS := e.table[nextHashS]

		// Set m to a match at offset if it looks like that will improve compression.
		improve := func(m *match, offset int32, s int32, first uint32, rep int32) {
			delta := s - offset
			if delta >= e.maxMatchOff || delta <= 0 || load3232(src, offset) != first {
				return
			}
			// Try to quick reject if we already have a long match.
			if m.length > 16 {
				left := len(src) - int(m.s+m.length)
				// If we are too close to the end, keep as is.
				if left <= 0 {
					return
				}
				checkLen := m.length - (s - m.s) - 8
				if left > 2 && checkLen > 4 {
					// Check 4 bytes, 4 bytes from the end of the current match.
					a := load3232(src, offset+checkLen)
					b := load3232(src, s+checkLen)
					if a != b {
						return
					}
				}
			}
			l := 4 + e.matchlen(s+4, offset+4, src)
			if m.rep <= 0 {
				// Extend candidate match backwards as far as possible.
				// Do not extend repeats as we can assume they are optimal
				// and offsets change if s == nextEmit.
				tMin := s - e.maxMatchOff
				if tMin < 0 {
					tMin = 0
				}
				for offset > tMin && s > nextEmit && src[offset-1] == src[s-1] && l < maxMatchLength {
					s--
					offset--
					l++
				}
			}
			if debugAsserts {
				if offset >= s {
					panic(fmt.Sprintf("offset: %d - s:%d - rep: %d - cur :%d - max: %d", offset, s, rep, e.cur, e.maxMatchOff))
				}
				if !bytes.Equal(src[s:s+l], src[offset:offset+l]) {
					panic(fmt.Sprintf("second match mismatch: %v != %v, first: %08x", src[s:s+4], src[offset:offset+4], first))
				}
			}
			cand := match{offset: offset, s: s, length: l, rep: rep}
			cand.estBits(bitsPerByte)
			if m.est >= highScore || cand.est-m.est+(cand.s-m.s)*bitsPerByte>>10 < 0 {
				*m = cand
			}
		}

		best := match{s: s, est: highScore}
		improve(&best, candidateL.offset-e.cur, s, uint32(cv), -1)
		improve(&best, candidateL.prev-e.cur, s, uint32(cv), -1)
		improve(&best, candidateS.offset-e.cur, s, uint32(cv), -1)
		improve(&best, candidateS.prev-e.cur, s, uint32(cv), -1)

		if canRepeat && best.length < goodEnough {
			if s == nextEmit {
				// Check repeats straight after a match.
				improve(&best, s-offset2, s, uint32(cv), 1|4)
				improve(&best, s-offset3, s, uint32(cv), 2|4)
				if offset1 > 1 {
					improve(&best, s-(offset1-1), s, uint32(cv), 3|4)
				}
			}

			// If either no match or a non-repeat match, check at + 1
			if best.rep <= 0 {
				cv32 := uint32(cv >> 8)
				spp := s + 1
				improve(&best, spp-offset1, spp, cv32, 1)
				improve(&best, spp-offset2, spp, cv32, 2)
				improve(&best, spp-offset3, spp, cv32, 3)
				if best.rep < 0 {
					cv32 = uint32(cv >> 24)
					spp += 2
					improve(&best, spp-offset1, spp, cv32, 1)
					improve(&best, spp-offset2, spp, cv32, 2)
					improve(&best, spp-offset3, spp, cv32, 3)
				}
			}
		}
		// Load next and check...
		e.longTable[nextHashL] = prevEntry{offset: s + e.cur, prev: candidateL.offset}
		e.table[nextHashS] = prevEntry{offset: s + e.cur, prev: candidateS.offset}
		index0 := s + 1

		// Look far ahead, unless we have a really long match already...
		if best.length < goodEnough {
			// No match found, move forward on input, no need to check forward...
			if best.length < 4 {
				s += 1 + (s-nextEmit)>>(kSearchStrength-1)
				if s >= sLimit {
					break encodeLoop
				}
				continue
			}

			candidateS = e.table[hashLen(cv>>8, bestShortTableBits, bestShortLen)]
			cv = load6432(src, s+1)
			cv2 := load6432(src, s+2)
			candidateL = e.longTable[hashLen(cv, bestLongTableBits, bestLongLen)]
			candidateL2 := e.longTable[hashLen(cv2, bestLongTableBits, bestLongLen)]

			// Short at s+1
			improve(&best, candidateS.offset-e.cur, s+1, uint32(cv), -1)
			// Long at s+1, s+2
			improve(&best, candidateL.offset-e.cur, s+1, uint32(cv), -1)
			improve(&best, candidateL.prev-e.cur, s+1, uint32(cv), -1)
			improve(&best, candidateL2.offset-e.cur, s+2, uint32(cv2), -1)
			improve(&best, candidateL2.prev-e.cur, s+2, uint32(cv2), -1)
			if false {
				// Short at s+3.
				// Too often worse...
				improve(&best, e.table[hashLen(cv2>>8, bestShortTableBits, bestShortLen)].offset-e.cur, s+3, uint32(cv2>>8), -1)
			}

			// Start check at a fixed offset to allow for a few mismatches.
			// For this compression level 2 yields the best results.
			// We cannot do this if we have already indexed this position.
			const skipBeginning = 2
			if best.s > s-skipBeginning {
				// See if we can find a better match by checking where the current best ends.
				// Use that offset to see if we can find a better full match.
				if sAt := best.s + best.length; sAt < sLimit {
					nextHashL := hashLen(load6432(src, sAt), bestLongTableBits, bestLongLen)
					candidateEnd := e.longTable[nextHashL]

					if off := candidateEnd.offset - e.cur - best.length + skipBeginning; off >= 0 {
						improve(&best, off, best.s+skipBeginning, load3232(src, best.s+skipBeginning), -1)
						if off := candidateEnd.prev - e.cur - best.length + skipBeginning; off >= 0 {
							improve(&best, off, best.s+skipBeginning, load3232(src, best.s+skipBeginning), -1)
						}
					}
				}
			}
		}

		if debugAsserts {
			if best.offset >= best.s {
				panic(fmt.Sprintf("best.offset > s: %d >= %d", best.offset, best.s))
			}
			if best.s < nextEmit {
				panic(fmt.Sprintf("s %d < nextEmit %d", best.s, nextEmit))
			}
			if best.offset < s-e.maxMatchOff {
				panic(fmt.Sprintf("best.offset < s-e.maxMatchOff: %d < %d", best.offset, s-e.maxMatchOff))
			}
			if !bytes.Equal(src[best.s:best.s+best.length], src[best.offset:best.offset+best.length]) {
				panic(fmt.Sprintf("match mismatch: %v != %v", src[best.s:best.s+best.length], src[best.offset:best.offset+best.length]))
			}
		}

		// We have a match, we can store the forward value
		s = best.s
		if best.rep > 0 {
			var seq seq
			seq.matchLen = uint32(best.length - zstdMinMatch)
			addLiterals(&seq, best.s)

			// Repeat. If bit 4 is set, this is a non-lit repeat.
			seq.offset = uint32(best.rep & 3)
			if debugSequences {
				println("repeat sequence", seq, "next s:", best.s, "off:", best.s-best.offset)
			}
			blk.sequences = append(blk.sequences, seq)

			// Index old s + 1 -> s - 1
			s = best.s + best.length
			nextEmit = s

			// Index skipped...
			end := s
			if s > sLimit+4 {
				end = sLimit + 4
			}
			off := index0 + e.cur
			for index0 < end {
				cv0 := load6432(src, index0)
				h0 := hashLen(cv0, bestLongTableBits, bestLongLen)
				h1 := hashLen(cv0, bestShortTableBits, bestShortLen)
				e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
				e.table[h1] = prevEntry{offset: off, prev: e.table[h1].offset}
				off++
				index0++
			}

			switch best.rep {
			case 2, 4 | 1:
				offset1, offset2 = offset2, offset1
			case 3, 4 | 2:
				offset1, offset2, offset3 = offset3, offset1, offset2
			case 4 | 3:
				offset1, offset2, offset3 = offset1-1, offset1, offset2
			}
			if s >= sLimit {
				if debugEncoder {
					println("repeat ended", s, best.length)
				}
				break encodeLoop
			}
			continue
		}

		// A 4-byte match has been found. Update recent offsets.
		// We'll later see if more than 4 bytes.
		t := best.offset
		offset1, offset2, offset3 = s-t, offset1, offset2

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		if debugAsserts && int(offset1) > len(src) {
			panic("invalid offset")
		}

		// Write our sequence
		var seq seq
		l := best.length
		seq.litLen = uint32(s - nextEmit)
		seq.matchLen = uint32(l - zstdMinMatch)
		if seq.litLen > 0 {
			blk.literals = append(blk.literals, src[nextEmit:s]...)
		}
		seq.offset = uint32(s-t) + 3
		s += l
		if debugSequences {
			println("sequence", seq, "next s:", s)
		}
		blk.sequences = append(blk.sequences, seq)
		nextEmit = s

		// Index old s + 1 -> s - 1 or sLimit
		end := s
		if s > sLimit-4 {
			end = sLimit - 4
		}

		off := index0 + e.cur
		for index0 < end {
			cv0 := load6432(src, index0)
			h0 := hashLen(cv0, bestLongTableBits, bestLongLen)
			h1 := hashLen(cv0, bestShortTableBits, bestShortLen)
			e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
			e.table[h1] = prevEntry{offset: off, prev: e.table[h1].offset}
			index0++
			off++
		}
		if s >= sLimit {
			break encodeLoop
		}
	}

	if int(nextEmit) < len(src) {
		blk.literals = append(blk.literals, src[nextEmit:]...)
		blk.extraLits = len(src) - int(nextEmit)
	}
	blk.recentOffsets[0] = uint32(offset1)
	blk.recentOffsets[1] = uint32(offset2)
	blk.recentOffsets[2] = uint32(offset3)
	if debugEncoder {
		println("returning, recent offsets:", blk.recentOffsets, "extra literals:", blk.extraLits)
	}
}

// EncodeNoHist will encode a block with no history and no following blocks.
// Most notable difference is that src will not be copied for history and
// we do not need to check for max match length.
func (e *bestFastEncoder) EncodeNoHist(blk *blockEnc, src []byte) {
	e.ensureHist(len(src))
	e.Encode(blk, src)
}

// Reset will reset and set a dictionary if not nil
func (e *bestFastEncoder) Reset(d *dict, singleBlock bool) {
	e.resetBase(d, singleBlock)
	if d == nil {
		return
	}
	// Init or copy dict table
	if len(e.dictTable) != len(e.table) || d.id != e.lastDictID {
		if len(e.dictTable) != len(e.table) {
			e.dictTable = make([]prevEntry, len(e.table))
		}
		end := int32(len(d.content)) - 8 + e.maxMatchOff
		for i := e.maxMatchOff; i < end; i += 4 {
			const hashLog = bestShortTableBits

			cv := load6432(d.content, i-e.maxMatchOff)
			nextHash := hashLen(cv, hashLog, bestShortLen)      // 0 -> 4
			nextHash1 := hashLen(cv>>8, hashLog, bestShortLen)  // 1 -> 5
			nextHash2 := hashLen(cv>>16, hashLog, bestShortLen) // 2 -> 6
			nextHash3 := hashLen(cv>>24, hashLog, bestShortLen) // 3 -> 7
			e.dictTable[nextHash] = prevEntry{
				prev:   e.dictTable[nextHash].offset,
				offset: i,
			}
			e.dictTable[nextHash1] = prevEntry{
				prev:   e.dictTable[nextHash1].offset,
				offset: i + 1,
			}
			e.dictTable[nextHash2] = prevEntry{
				prev:   e.dictTable[nextHash2].offset,
				offset: i + 2,
			}
			e.dictTable[nextHash3] = prevEntry{
				prev:   e.dictTable[nextHash3].offset,
				offset: i + 3,
			}
		}
		e.lastDictID = d.id
	}

	// Init or copy dict table
	if len(e.dictLongTable) != len(e.longTable) || d.id != e.lastDictID {
		if len(e.dictLongTable) != len(e.longTable) {
			e.dictLongTable = make([]prevEntry, len(e.longTable))
		}
		if len(d.content) >= 8 {
			cv := load6432(d.content, 0)
			h := hashLen(cv, bestLongTableBits, bestLongLen)
			e.dictLongTable[h] = prevEntry{
				offset: e.maxMatchOff,
				prev:   e.dictLongTable[h].offset,
			}

			end := int32(len(d.content)) - 8 + e.maxMatchOff
			off := 8 // First to read
			for i := e.maxMatchOff + 1; i < end; i++ {
				cv = cv>>8 | (uint64(d.content[off]) << 56)
				h := hashLen(cv, bestLongTableBits, bestLongLen)
				e.dictLongTable[h] = prevEntry{
					offset: i,
					prev:   e.dictLongTable[h].offset,
				}
				off++
			}
		}
		e.lastDictID = d.id
	}
	// Reset table to initial state
	copy(e.longTable[:], e.dictLongTable)

	e.cur = e.maxMatchOff
	// Reset table to initial state
	copy(e.table[:], e.dictTable)
}
