// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import "fmt"

const (
	betterLongTableBits = 19                       // Bits used in the long match table
	betterLongTableSize = 1 << betterLongTableBits // Size of the table
	betterLongLen       = 8                        // Bytes used for table hash

	// Note: Increasing the short table bits or making the hash shorter
	// can actually lead to compression degradation since it will 'steal' more from the
	// long match table and match offsets are quite big.
	// This greatly depends on the type of input.
	betterShortTableBits = 13                        // Bits used in the short match table
	betterShortTableSize = 1 << betterShortTableBits // Size of the table
	betterShortLen       = 5                         // Bytes used for table hash

	betterLongTableShardCnt  = 1 << (betterLongTableBits - dictShardBits)    // Number of shards in the table
	betterLongTableShardSize = betterLongTableSize / betterLongTableShardCnt // Size of an individual shard

	betterShortTableShardCnt  = 1 << (betterShortTableBits - dictShardBits)     // Number of shards in the table
	betterShortTableShardSize = betterShortTableSize / betterShortTableShardCnt // Size of an individual shard
)

type prevEntry struct {
	offset int32
	prev   int32
}

// betterFastEncoder uses 2 tables, one for short matches (5 bytes) and one for long matches.
// The long match table contains the previous entry with the same hash,
// effectively making it a "chain" of length 2.
// When we find a long match we choose between the two values and select the longest.
// When we find a short match, after checking the long, we check if we can find a long at n+1
// and that it is longer (lazy matching).
type betterFastEncoder struct {
	fastBase
	table     [betterShortTableSize]tableEntry
	longTable [betterLongTableSize]prevEntry
}

type betterFastEncoderDict struct {
	betterFastEncoder
	dictTable            []tableEntry
	dictLongTable        []prevEntry
	shortTableShardDirty [betterShortTableShardCnt]bool
	longTableShardDirty  [betterLongTableShardCnt]bool
	allDirty             bool
}

// Encode improves compression...
func (e *betterFastEncoder) Encode(blk *blockEnc, src []byte) {
	const (
		// Input margin is the number of bytes we read (8)
		// and the maximum we will read ahead (2)
		inputMargin            = 8 + 2
		minNonLiteralBlockSize = 16
	)

	// Protect against e.cur wraparound.
	for e.cur >= e.bufferReset-int32(len(e.hist)) {
		if len(e.hist) == 0 {
			e.table = [betterShortTableSize]tableEntry{}
			e.longTable = [betterLongTableSize]prevEntry{}
			e.cur = e.maxMatchOff
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - e.maxMatchOff
		for i := range e.table[:] {
			v := e.table[i].offset
			if v < minOff {
				v = 0
			} else {
				v = v - e.cur + e.maxMatchOff
			}
			e.table[i].offset = v
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

	// Override src
	src = e.hist
	sLimit := int32(len(src)) - inputMargin
	// stepSize is the number of bytes to skip on every main loop iteration.
	// It should be >= 1.
	const stepSize = 1

	const kSearchStrength = 9

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := s
	cv := load6432(src, s)

	// Relative offsets
	offset1 := int32(blk.recentOffsets[0])
	offset2 := int32(blk.recentOffsets[1])

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
		var t int32
		// We allow the encoder to optionally turn off repeat offsets across blocks
		canRepeat := len(blk.sequences) > 2
		var matched, index0 int32

		for {
			if debugAsserts && canRepeat && offset1 == 0 {
				panic("offset0 was 0")
			}

			nextHashL := hashLen(cv, betterLongTableBits, betterLongLen)
			nextHashS := hashLen(cv, betterShortTableBits, betterShortLen)
			candidateL := e.longTable[nextHashL]
			candidateS := e.table[nextHashS]

			const repOff = 1
			repIndex := s - offset1 + repOff
			off := s + e.cur
			e.longTable[nextHashL] = prevEntry{offset: off, prev: candidateL.offset}
			e.table[nextHashS] = tableEntry{offset: off, val: uint32(cv)}
			index0 = s + 1

			if canRepeat {
				if repIndex >= 0 && load3232(src, repIndex) == uint32(cv>>(repOff*8)) {
					// Consider history as well.
					var seq seq
					length := 4 + e.matchlen(s+4+repOff, repIndex+4, src)

					seq.matchLen = uint32(length - zstdMinMatch)

					// We might be able to match backwards.
					// Extend as long as we can.
					start := s + repOff
					// We end the search early, so we don't risk 0 literals
					// and have to do special offset treatment.
					startLimit := nextEmit + 1

					tMin := s - e.maxMatchOff
					if tMin < 0 {
						tMin = 0
					}
					for repIndex > tMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch-1 {
						repIndex--
						start--
						seq.matchLen++
					}
					addLiterals(&seq, start)

					// rep 0
					seq.offset = 1
					if debugSequences {
						println("repeat sequence", seq, "next s:", s)
					}
					blk.sequences = append(blk.sequences, seq)

					// Index match start+1 (long) -> s - 1
					index0 := s + repOff
					s += length + repOff

					nextEmit = s
					if s >= sLimit {
						if debugEncoder {
							println("repeat ended", s, length)

						}
						break encodeLoop
					}
					// Index skipped...
					for index0 < s-1 {
						cv0 := load6432(src, index0)
						cv1 := cv0 >> 8
						h0 := hashLen(cv0, betterLongTableBits, betterLongLen)
						off := index0 + e.cur
						e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
						e.table[hashLen(cv1, betterShortTableBits, betterShortLen)] = tableEntry{offset: off + 1, val: uint32(cv1)}
						index0 += 2
					}
					cv = load6432(src, s)
					continue
				}
				const repOff2 = 1

				// We deviate from the reference encoder and also check offset 2.
				// Still slower and not much better, so disabled.
				// repIndex = s - offset2 + repOff2
				if false && repIndex >= 0 && load6432(src, repIndex) == load6432(src, s+repOff) {
					// Consider history as well.
					var seq seq
					length := 8 + e.matchlen(s+8+repOff2, repIndex+8, src)

					seq.matchLen = uint32(length - zstdMinMatch)

					// We might be able to match backwards.
					// Extend as long as we can.
					start := s + repOff2
					// We end the search early, so we don't risk 0 literals
					// and have to do special offset treatment.
					startLimit := nextEmit + 1

					tMin := s - e.maxMatchOff
					if tMin < 0 {
						tMin = 0
					}
					for repIndex > tMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch-1 {
						repIndex--
						start--
						seq.matchLen++
					}
					addLiterals(&seq, start)

					// rep 2
					seq.offset = 2
					if debugSequences {
						println("repeat sequence 2", seq, "next s:", s)
					}
					blk.sequences = append(blk.sequences, seq)

					s += length + repOff2
					nextEmit = s
					if s >= sLimit {
						if debugEncoder {
							println("repeat ended", s, length)

						}
						break encodeLoop
					}

					// Index skipped...
					for index0 < s-1 {
						cv0 := load6432(src, index0)
						cv1 := cv0 >> 8
						h0 := hashLen(cv0, betterLongTableBits, betterLongLen)
						off := index0 + e.cur
						e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
						e.table[hashLen(cv1, betterShortTableBits, betterShortLen)] = tableEntry{offset: off + 1, val: uint32(cv1)}
						index0 += 2
					}
					cv = load6432(src, s)
					// Swap offsets
					offset1, offset2 = offset2, offset1
					continue
				}
			}
			// Find the offsets of our two matches.
			coffsetL := candidateL.offset - e.cur
			coffsetLP := candidateL.prev - e.cur

			// Check if we have a long match.
			if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
				// Found a long match, at least 8 bytes.
				matched = e.matchlen(s+8, coffsetL+8, src) + 8
				t = coffsetL
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugMatches {
					println("long match")
				}

				if s-coffsetLP < e.maxMatchOff && cv == load6432(src, coffsetLP) {
					// Found a long match, at least 8 bytes.
					prevMatch := e.matchlen(s+8, coffsetLP+8, src) + 8
					if prevMatch > matched {
						matched = prevMatch
						t = coffsetLP
					}
					if debugAsserts && s <= t {
						panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
					}
					if debugAsserts && s-t > e.maxMatchOff {
						panic("s - t >e.maxMatchOff")
					}
					if debugMatches {
						println("long match")
					}
				}
				break
			}

			// Check if we have a long match on prev.
			if s-coffsetLP < e.maxMatchOff && cv == load6432(src, coffsetLP) {
				// Found a long match, at least 8 bytes.
				matched = e.matchlen(s+8, coffsetLP+8, src) + 8
				t = coffsetLP
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugMatches {
					println("long match")
				}
				break
			}

			coffsetS := candidateS.offset - e.cur

			// Check if we have a short match.
			if s-coffsetS < e.maxMatchOff && uint32(cv) == candidateS.val {
				// found a regular match
				matched = e.matchlen(s+4, coffsetS+4, src) + 4

				// See if we can find a long match at s+1
				const checkAt = 1
				cv := load6432(src, s+checkAt)
				nextHashL = hashLen(cv, betterLongTableBits, betterLongLen)
				candidateL = e.longTable[nextHashL]
				coffsetL = candidateL.offset - e.cur

				// We can store it, since we have at least a 4 byte match.
				e.longTable[nextHashL] = prevEntry{offset: s + checkAt + e.cur, prev: candidateL.offset}
				if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
					// Found a long match, at least 8 bytes.
					matchedNext := e.matchlen(s+8+checkAt, coffsetL+8, src) + 8
					if matchedNext > matched {
						t = coffsetL
						s += checkAt
						matched = matchedNext
						if debugMatches {
							println("long match (after short)")
						}
						break
					}
				}

				// Check prev long...
				coffsetL = candidateL.prev - e.cur
				if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
					// Found a long match, at least 8 bytes.
					matchedNext := e.matchlen(s+8+checkAt, coffsetL+8, src) + 8
					if matchedNext > matched {
						t = coffsetL
						s += checkAt
						matched = matchedNext
						if debugMatches {
							println("prev long match (after short)")
						}
						break
					}
				}
				t = coffsetS
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugAsserts && t < 0 {
					panic("t<0")
				}
				if debugMatches {
					println("short match")
				}
				break
			}

			// No match found, move forward in input.
			s += stepSize + ((s - nextEmit) >> (kSearchStrength - 1))
			if s >= sLimit {
				break encodeLoop
			}
			cv = load6432(src, s)
		}

		// Try to find a better match by searching for a long match at the end of the current best match
		if s+matched < sLimit {
			// Allow some bytes at the beginning to mismatch.
			// Sweet spot is around 3 bytes, but depends on input.
			// The skipped bytes are tested in Extend backwards,
			// and still picked up as part of the match if they do.
			const skipBeginning = 3

			nextHashL := hashLen(load6432(src, s+matched), betterLongTableBits, betterLongLen)
			s2 := s + skipBeginning
			cv := load3232(src, s2)
			candidateL := e.longTable[nextHashL]
			coffsetL := candidateL.offset - e.cur - matched + skipBeginning
			if coffsetL >= 0 && coffsetL < s2 && s2-coffsetL < e.maxMatchOff && cv == load3232(src, coffsetL) {
				// Found a long match, at least 4 bytes.
				matchedNext := e.matchlen(s2+4, coffsetL+4, src) + 4
				if matchedNext > matched {
					t = coffsetL
					s = s2
					matched = matchedNext
					if debugMatches {
						println("long match at end-of-match")
					}
				}
			}

			// Check prev long...
			if true {
				coffsetL = candidateL.prev - e.cur - matched + skipBeginning
				if coffsetL >= 0 && coffsetL < s2 && s2-coffsetL < e.maxMatchOff && cv == load3232(src, coffsetL) {
					// Found a long match, at least 4 bytes.
					matchedNext := e.matchlen(s2+4, coffsetL+4, src) + 4
					if matchedNext > matched {
						t = coffsetL
						s = s2
						matched = matchedNext
						if debugMatches {
							println("prev long match at end-of-match")
						}
					}
				}
			}
		}
		// A match has been found. Update recent offsets.
		offset2 = offset1
		offset1 = s - t

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		if debugAsserts && canRepeat && int(offset1) > len(src) {
			panic("invalid offset")
		}

		// Extend the n-byte match as long as possible.
		l := matched

		// Extend backwards
		tMin := s - e.maxMatchOff
		if tMin < 0 {
			tMin = 0
		}
		for t > tMin && s > nextEmit && src[t-1] == src[s-1] && l < maxMatchLength {
			s--
			t--
			l++
		}

		// Write our sequence
		var seq seq
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
		if s >= sLimit {
			break encodeLoop
		}

		// Index match start+1 (long) -> s - 1
		off := index0 + e.cur
		for index0 < s-1 {
			cv0 := load6432(src, index0)
			cv1 := cv0 >> 8
			h0 := hashLen(cv0, betterLongTableBits, betterLongLen)
			e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
			e.table[hashLen(cv1, betterShortTableBits, betterShortLen)] = tableEntry{offset: off + 1, val: uint32(cv1)}
			index0 += 2
			off += 2
		}

		cv = load6432(src, s)
		if !canRepeat {
			continue
		}

		// Check offset 2
		for {
			o2 := s - offset2
			if load3232(src, o2) != uint32(cv) {
				// Do regular search
				break
			}

			// Store this, since we have it.
			nextHashL := hashLen(cv, betterLongTableBits, betterLongLen)
			nextHashS := hashLen(cv, betterShortTableBits, betterShortLen)

			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			e.longTable[nextHashL] = prevEntry{offset: s + e.cur, prev: e.longTable[nextHashL].offset}
			e.table[nextHashS] = tableEntry{offset: s + e.cur, val: uint32(cv)}
			seq.matchLen = uint32(l) - zstdMinMatch
			seq.litLen = 0

			// Since litlen is always 0, this is offset 1.
			seq.offset = 1
			s += l
			nextEmit = s
			if debugSequences {
				println("sequence", seq, "next s:", s)
			}
			blk.sequences = append(blk.sequences, seq)

			// Swap offset 1 and 2.
			offset1, offset2 = offset2, offset1
			if s >= sLimit {
				// Finished
				break encodeLoop
			}
			cv = load6432(src, s)
		}
	}

	if int(nextEmit) < len(src) {
		blk.literals = append(blk.literals, src[nextEmit:]...)
		blk.extraLits = len(src) - int(nextEmit)
	}
	blk.recentOffsets[0] = uint32(offset1)
	blk.recentOffsets[1] = uint32(offset2)
	if debugEncoder {
		println("returning, recent offsets:", blk.recentOffsets, "extra literals:", blk.extraLits)
	}
}

// EncodeNoHist will encode a block with no history and no following blocks.
// Most notable difference is that src will not be copied for history and
// we do not need to check for max match length.
func (e *betterFastEncoder) EncodeNoHist(blk *blockEnc, src []byte) {
	e.ensureHist(len(src))
	e.Encode(blk, src)
}

// Encode improves compression...
func (e *betterFastEncoderDict) Encode(blk *blockEnc, src []byte) {
	const (
		// Input margin is the number of bytes we read (8)
		// and the maximum we will read ahead (2)
		inputMargin            = 8 + 2
		minNonLiteralBlockSize = 16
	)

	// Protect against e.cur wraparound.
	for e.cur >= e.bufferReset-int32(len(e.hist)) {
		if len(e.hist) == 0 {
			for i := range e.table[:] {
				e.table[i] = tableEntry{}
			}
			for i := range e.longTable[:] {
				e.longTable[i] = prevEntry{}
			}
			e.cur = e.maxMatchOff
			e.allDirty = true
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - e.maxMatchOff
		for i := range e.table[:] {
			v := e.table[i].offset
			if v < minOff {
				v = 0
			} else {
				v = v - e.cur + e.maxMatchOff
			}
			e.table[i].offset = v
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
		e.allDirty = true
		e.cur = e.maxMatchOff
		break
	}

	s := e.addBlock(src)
	blk.size = len(src)
	if len(src) < minNonLiteralBlockSize {
		blk.extraLits = len(src)
		blk.literals = blk.literals[:len(src)]
		copy(blk.literals, src)
		return
	}

	// Override src
	src = e.hist
	sLimit := int32(len(src)) - inputMargin
	// stepSize is the number of bytes to skip on every main loop iteration.
	// It should be >= 1.
	const stepSize = 1

	const kSearchStrength = 9

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := s
	cv := load6432(src, s)

	// Relative offsets
	offset1 := int32(blk.recentOffsets[0])
	offset2 := int32(blk.recentOffsets[1])

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
		var t int32
		// We allow the encoder to optionally turn off repeat offsets across blocks
		canRepeat := len(blk.sequences) > 2
		var matched, index0 int32

		for {
			if debugAsserts && canRepeat && offset1 == 0 {
				panic("offset0 was 0")
			}

			nextHashL := hashLen(cv, betterLongTableBits, betterLongLen)
			nextHashS := hashLen(cv, betterShortTableBits, betterShortLen)
			candidateL := e.longTable[nextHashL]
			candidateS := e.table[nextHashS]

			const repOff = 1
			repIndex := s - offset1 + repOff
			off := s + e.cur
			e.longTable[nextHashL] = prevEntry{offset: off, prev: candidateL.offset}
			e.markLongShardDirty(nextHashL)
			e.table[nextHashS] = tableEntry{offset: off, val: uint32(cv)}
			e.markShortShardDirty(nextHashS)
			index0 = s + 1

			if canRepeat {
				if repIndex >= 0 && load3232(src, repIndex) == uint32(cv>>(repOff*8)) {
					// Consider history as well.
					var seq seq
					length := 4 + e.matchlen(s+4+repOff, repIndex+4, src)

					seq.matchLen = uint32(length - zstdMinMatch)

					// We might be able to match backwards.
					// Extend as long as we can.
					start := s + repOff
					// We end the search early, so we don't risk 0 literals
					// and have to do special offset treatment.
					startLimit := nextEmit + 1

					tMin := s - e.maxMatchOff
					if tMin < 0 {
						tMin = 0
					}
					for repIndex > tMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch-1 {
						repIndex--
						start--
						seq.matchLen++
					}
					addLiterals(&seq, start)

					// rep 0
					seq.offset = 1
					if debugSequences {
						println("repeat sequence", seq, "next s:", s)
					}
					blk.sequences = append(blk.sequences, seq)

					// Index match start+1 (long) -> s - 1
					s += length + repOff

					nextEmit = s
					if s >= sLimit {
						if debugEncoder {
							println("repeat ended", s, length)

						}
						break encodeLoop
					}
					// Index skipped...
					for index0 < s-1 {
						cv0 := load6432(src, index0)
						cv1 := cv0 >> 8
						h0 := hashLen(cv0, betterLongTableBits, betterLongLen)
						off := index0 + e.cur
						e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
						e.markLongShardDirty(h0)
						h1 := hashLen(cv1, betterShortTableBits, betterShortLen)
						e.table[h1] = tableEntry{offset: off + 1, val: uint32(cv1)}
						e.markShortShardDirty(h1)
						index0 += 2
					}
					cv = load6432(src, s)
					continue
				}
				const repOff2 = 1

				// We deviate from the reference encoder and also check offset 2.
				// Still slower and not much better, so disabled.
				// repIndex = s - offset2 + repOff2
				if false && repIndex >= 0 && load6432(src, repIndex) == load6432(src, s+repOff) {
					// Consider history as well.
					var seq seq
					length := 8 + e.matchlen(s+8+repOff2, repIndex+8, src)

					seq.matchLen = uint32(length - zstdMinMatch)

					// We might be able to match backwards.
					// Extend as long as we can.
					start := s + repOff2
					// We end the search early, so we don't risk 0 literals
					// and have to do special offset treatment.
					startLimit := nextEmit + 1

					tMin := s - e.maxMatchOff
					if tMin < 0 {
						tMin = 0
					}
					for repIndex > tMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch-1 {
						repIndex--
						start--
						seq.matchLen++
					}
					addLiterals(&seq, start)

					// rep 2
					seq.offset = 2
					if debugSequences {
						println("repeat sequence 2", seq, "next s:", s)
					}
					blk.sequences = append(blk.sequences, seq)

					s += length + repOff2
					nextEmit = s
					if s >= sLimit {
						if debugEncoder {
							println("repeat ended", s, length)

						}
						break encodeLoop
					}

					// Index skipped...
					for index0 < s-1 {
						cv0 := load6432(src, index0)
						cv1 := cv0 >> 8
						h0 := hashLen(cv0, betterLongTableBits, betterLongLen)
						off := index0 + e.cur
						e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
						e.markLongShardDirty(h0)
						h1 := hashLen(cv1, betterShortTableBits, betterShortLen)
						e.table[h1] = tableEntry{offset: off + 1, val: uint32(cv1)}
						e.markShortShardDirty(h1)
						index0 += 2
					}
					cv = load6432(src, s)
					// Swap offsets
					offset1, offset2 = offset2, offset1
					continue
				}
			}
			// Find the offsets of our two matches.
			coffsetL := candidateL.offset - e.cur
			coffsetLP := candidateL.prev - e.cur

			// Check if we have a long match.
			if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
				// Found a long match, at least 8 bytes.
				matched = e.matchlen(s+8, coffsetL+8, src) + 8
				t = coffsetL
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugMatches {
					println("long match")
				}

				if s-coffsetLP < e.maxMatchOff && cv == load6432(src, coffsetLP) {
					// Found a long match, at least 8 bytes.
					prevMatch := e.matchlen(s+8, coffsetLP+8, src) + 8
					if prevMatch > matched {
						matched = prevMatch
						t = coffsetLP
					}
					if debugAsserts && s <= t {
						panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
					}
					if debugAsserts && s-t > e.maxMatchOff {
						panic("s - t >e.maxMatchOff")
					}
					if debugMatches {
						println("long match")
					}
				}
				break
			}

			// Check if we have a long match on prev.
			if s-coffsetLP < e.maxMatchOff && cv == load6432(src, coffsetLP) {
				// Found a long match, at least 8 bytes.
				matched = e.matchlen(s+8, coffsetLP+8, src) + 8
				t = coffsetLP
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugMatches {
					println("long match")
				}
				break
			}

			coffsetS := candidateS.offset - e.cur

			// Check if we have a short match.
			if s-coffsetS < e.maxMatchOff && uint32(cv) == candidateS.val {
				// found a regular match
				matched = e.matchlen(s+4, coffsetS+4, src) + 4

				// See if we can find a long match at s+1
				const checkAt = 1
				cv := load6432(src, s+checkAt)
				nextHashL = hashLen(cv, betterLongTableBits, betterLongLen)
				candidateL = e.longTable[nextHashL]
				coffsetL = candidateL.offset - e.cur

				// We can store it, since we have at least a 4 byte match.
				e.longTable[nextHashL] = prevEntry{offset: s + checkAt + e.cur, prev: candidateL.offset}
				e.markLongShardDirty(nextHashL)
				if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
					// Found a long match, at least 8 bytes.
					matchedNext := e.matchlen(s+8+checkAt, coffsetL+8, src) + 8
					if matchedNext > matched {
						t = coffsetL
						s += checkAt
						matched = matchedNext
						if debugMatches {
							println("long match (after short)")
						}
						break
					}
				}

				// Check prev long...
				coffsetL = candidateL.prev - e.cur
				if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
					// Found a long match, at least 8 bytes.
					matchedNext := e.matchlen(s+8+checkAt, coffsetL+8, src) + 8
					if matchedNext > matched {
						t = coffsetL
						s += checkAt
						matched = matchedNext
						if debugMatches {
							println("prev long match (after short)")
						}
						break
					}
				}
				t = coffsetS
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugAsserts && t < 0 {
					panic("t<0")
				}
				if debugMatches {
					println("short match")
				}
				break
			}

			// No match found, move forward in input.
			s += stepSize + ((s - nextEmit) >> (kSearchStrength - 1))
			if s >= sLimit {
				break encodeLoop
			}
			cv = load6432(src, s)
		}
		// Try to find a better match by searching for a long match at the end of the current best match
		if s+matched < sLimit {
			nextHashL := hashLen(load6432(src, s+matched), betterLongTableBits, betterLongLen)
			cv := load3232(src, s)
			candidateL := e.longTable[nextHashL]
			coffsetL := candidateL.offset - e.cur - matched
			if coffsetL >= 0 && coffsetL < s && s-coffsetL < e.maxMatchOff && cv == load3232(src, coffsetL) {
				// Found a long match, at least 4 bytes.
				matchedNext := e.matchlen(s+4, coffsetL+4, src) + 4
				if matchedNext > matched {
					t = coffsetL
					matched = matchedNext
					if debugMatches {
						println("long match at end-of-match")
					}
				}
			}

			// Check prev long...
			if true {
				coffsetL = candidateL.prev - e.cur - matched
				if coffsetL >= 0 && coffsetL < s && s-coffsetL < e.maxMatchOff && cv == load3232(src, coffsetL) {
					// Found a long match, at least 4 bytes.
					matchedNext := e.matchlen(s+4, coffsetL+4, src) + 4
					if matchedNext > matched {
						t = coffsetL
						matched = matchedNext
						if debugMatches {
							println("prev long match at end-of-match")
						}
					}
				}
			}
		}
		// A match has been found. Update recent offsets.
		offset2 = offset1
		offset1 = s - t

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		if debugAsserts && canRepeat && int(offset1) > len(src) {
			panic("invalid offset")
		}

		// Extend the n-byte match as long as possible.
		l := matched

		// Extend backwards
		tMin := s - e.maxMatchOff
		if tMin < 0 {
			tMin = 0
		}
		for t > tMin && s > nextEmit && src[t-1] == src[s-1] && l < maxMatchLength {
			s--
			t--
			l++
		}

		// Write our sequence
		var seq seq
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
		if s >= sLimit {
			break encodeLoop
		}

		// Index match start+1 (long) -> s - 1
		off := index0 + e.cur
		for index0 < s-1 {
			cv0 := load6432(src, index0)
			cv1 := cv0 >> 8
			h0 := hashLen(cv0, betterLongTableBits, betterLongLen)
			e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
			e.markLongShardDirty(h0)
			h1 := hashLen(cv1, betterShortTableBits, betterShortLen)
			e.table[h1] = tableEntry{offset: off + 1, val: uint32(cv1)}
			e.markShortShardDirty(h1)
			index0 += 2
			off += 2
		}

		cv = load6432(src, s)
		if !canRepeat {
			continue
		}

		// Check offset 2
		for {
			o2 := s - offset2
			if load3232(src, o2) != uint32(cv) {
				// Do regular search
				break
			}

			// Store this, since we have it.
			nextHashL := hashLen(cv, betterLongTableBits, betterLongLen)
			nextHashS := hashLen(cv, betterShortTableBits, betterShortLen)

			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			e.longTable[nextHashL] = prevEntry{offset: s + e.cur, prev: e.longTable[nextHashL].offset}
			e.markLongShardDirty(nextHashL)
			e.table[nextHashS] = tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.markShortShardDirty(nextHashS)
			seq.matchLen = uint32(l) - zstdMinMatch
			seq.litLen = 0

			// Since litlen is always 0, this is offset 1.
			seq.offset = 1
			s += l
			nextEmit = s
			if debugSequences {
				println("sequence", seq, "next s:", s)
			}
			blk.sequences = append(blk.sequences, seq)

			// Swap offset 1 and 2.
			offset1, offset2 = offset2, offset1
			if s >= sLimit {
				// Finished
				break encodeLoop
			}
			cv = load6432(src, s)
		}
	}

	if int(nextEmit) < len(src) {
		blk.literals = append(blk.literals, src[nextEmit:]...)
		blk.extraLits = len(src) - int(nextEmit)
	}
	blk.recentOffsets[0] = uint32(offset1)
	blk.recentOffsets[1] = uint32(offset2)
	if debugEncoder {
		println("returning, recent offsets:", blk.recentOffsets, "extra literals:", blk.extraLits)
	}
}

// ResetDict will reset and set a dictionary if not nil
func (e *betterFastEncoder) Reset(d *dict, singleBlock bool) {
	e.resetBase(d, singleBlock)
	if d != nil {
		panic("betterFastEncoder: Reset with dict")
	}
}

// ResetDict will reset and set a dictionary if not nil
func (e *betterFastEncoderDict) Reset(d *dict, singleBlock bool) {
	e.resetBase(d, singleBlock)
	if d == nil {
		return
	}
	// Init or copy dict table
	if len(e.dictTable) != len(e.table) || d.id != e.lastDictID {
		if len(e.dictTable) != len(e.table) {
			e.dictTable = make([]tableEntry, len(e.table))
		}
		end := int32(len(d.content)) - 8 + e.maxMatchOff
		for i := e.maxMatchOff; i < end; i += 4 {
			const hashLog = betterShortTableBits

			cv := load6432(d.content, i-e.maxMatchOff)
			nextHash := hashLen(cv, hashLog, betterShortLen)      // 0 -> 4
			nextHash1 := hashLen(cv>>8, hashLog, betterShortLen)  // 1 -> 5
			nextHash2 := hashLen(cv>>16, hashLog, betterShortLen) // 2 -> 6
			nextHash3 := hashLen(cv>>24, hashLog, betterShortLen) // 3 -> 7
			e.dictTable[nextHash] = tableEntry{
				val:    uint32(cv),
				offset: i,
			}
			e.dictTable[nextHash1] = tableEntry{
				val:    uint32(cv >> 8),
				offset: i + 1,
			}
			e.dictTable[nextHash2] = tableEntry{
				val:    uint32(cv >> 16),
				offset: i + 2,
			}
			e.dictTable[nextHash3] = tableEntry{
				val:    uint32(cv >> 24),
				offset: i + 3,
			}
		}
		e.lastDictID = d.id
		e.allDirty = true
	}

	// Init or copy dict table
	if len(e.dictLongTable) != len(e.longTable) || d.id != e.lastDictID {
		if len(e.dictLongTable) != len(e.longTable) {
			e.dictLongTable = make([]prevEntry, len(e.longTable))
		}
		if len(d.content) >= 8 {
			cv := load6432(d.content, 0)
			h := hashLen(cv, betterLongTableBits, betterLongLen)
			e.dictLongTable[h] = prevEntry{
				offset: e.maxMatchOff,
				prev:   e.dictLongTable[h].offset,
			}

			end := int32(len(d.content)) - 8 + e.maxMatchOff
			off := 8 // First to read
			for i := e.maxMatchOff + 1; i < end; i++ {
				cv = cv>>8 | (uint64(d.content[off]) << 56)
				h := hashLen(cv, betterLongTableBits, betterLongLen)
				e.dictLongTable[h] = prevEntry{
					offset: i,
					prev:   e.dictLongTable[h].offset,
				}
				off++
			}
		}
		e.lastDictID = d.id
		e.allDirty = true
	}

	// Reset table to initial state
	{
		dirtyShardCnt := 0
		if !e.allDirty {
			for i := range e.shortTableShardDirty {
				if e.shortTableShardDirty[i] {
					dirtyShardCnt++
				}
			}
		}
		const shardCnt = betterShortTableShardCnt
		const shardSize = betterShortTableShardSize
		if e.allDirty || dirtyShardCnt > shardCnt*4/6 {
			copy(e.table[:], e.dictTable)
			for i := range e.shortTableShardDirty {
				e.shortTableShardDirty[i] = false
			}
		} else {
			for i := range e.shortTableShardDirty {
				if !e.shortTableShardDirty[i] {
					continue
				}

				copy(e.table[i*shardSize:(i+1)*shardSize], e.dictTable[i*shardSize:(i+1)*shardSize])
				e.shortTableShardDirty[i] = false
			}
		}
	}
	{
		dirtyShardCnt := 0
		if !e.allDirty {
			for i := range e.shortTableShardDirty {
				if e.shortTableShardDirty[i] {
					dirtyShardCnt++
				}
			}
		}
		const shardCnt = betterLongTableShardCnt
		const shardSize = betterLongTableShardSize
		if e.allDirty || dirtyShardCnt > shardCnt*4/6 {
			copy(e.longTable[:], e.dictLongTable)
			for i := range e.longTableShardDirty {
				e.longTableShardDirty[i] = false
			}
		} else {
			for i := range e.longTableShardDirty {
				if !e.longTableShardDirty[i] {
					continue
				}

				copy(e.longTable[i*shardSize:(i+1)*shardSize], e.dictLongTable[i*shardSize:(i+1)*shardSize])
				e.longTableShardDirty[i] = false
			}
		}
	}
	e.cur = e.maxMatchOff
	e.allDirty = false
}

func (e *betterFastEncoderDict) markLongShardDirty(entryNum uint32) {
	e.longTableShardDirty[entryNum/betterLongTableShardSize] = true
}

func (e *betterFastEncoderDict) markShortShardDirty(entryNum uint32) {
	e.shortTableShardDirty[entryNum/betterShortTableShardSize] = true
}
