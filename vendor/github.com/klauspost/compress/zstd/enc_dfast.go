// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import "fmt"

const (
	dFastLongTableBits = 17                      // Bits used in the long match table
	dFastLongTableSize = 1 << dFastLongTableBits // Size of the table
	dFastLongTableMask = dFastLongTableSize - 1  // Mask for table indices. Redundant, but can eliminate bounds checks.
	dFastLongLen       = 8                       // Bytes used for table hash

	dLongTableShardCnt  = 1 << (dFastLongTableBits - dictShardBits) // Number of shards in the table
	dLongTableShardSize = dFastLongTableSize / tableShardCnt        // Size of an individual shard

	dFastShortTableBits = tableBits                // Bits used in the short match table
	dFastShortTableSize = 1 << dFastShortTableBits // Size of the table
	dFastShortTableMask = dFastShortTableSize - 1  // Mask for table indices. Redundant, but can eliminate bounds checks.
	dFastShortLen       = 5                        // Bytes used for table hash

)

type doubleFastEncoder struct {
	fastEncoder
	longTable [dFastLongTableSize]tableEntry
}

type doubleFastEncoderDict struct {
	fastEncoderDict
	longTable           [dFastLongTableSize]tableEntry
	dictLongTable       []tableEntry
	longTableShardDirty [dLongTableShardCnt]bool
}

// Encode mimmics functionality in zstd_dfast.c
func (e *doubleFastEncoder) Encode(blk *blockEnc, src []byte) {
	const (
		// Input margin is the number of bytes we read (8)
		// and the maximum we will read ahead (2)
		inputMargin            = 8 + 2
		minNonLiteralBlockSize = 16
	)

	// Protect against e.cur wraparound.
	for e.cur >= e.bufferReset-int32(len(e.hist)) {
		if len(e.hist) == 0 {
			e.table = [dFastShortTableSize]tableEntry{}
			e.longTable = [dFastLongTableSize]tableEntry{}
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
			if v < minOff {
				v = 0
			} else {
				v = v - e.cur + e.maxMatchOff
			}
			e.longTable[i].offset = v
		}
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

	const kSearchStrength = 8

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

		for {
			if debugAsserts && canRepeat && offset1 == 0 {
				panic("offset0 was 0")
			}

			nextHashL := hashLen(cv, dFastLongTableBits, dFastLongLen)
			nextHashS := hashLen(cv, dFastShortTableBits, dFastShortLen)
			candidateL := e.longTable[nextHashL]
			candidateS := e.table[nextHashS]

			const repOff = 1
			repIndex := s - offset1 + repOff
			entry := tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.longTable[nextHashL] = entry
			e.table[nextHashS] = entry

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
					s += length + repOff
					nextEmit = s
					if s >= sLimit {
						if debugEncoder {
							println("repeat ended", s, length)

						}
						break encodeLoop
					}
					cv = load6432(src, s)
					continue
				}
			}
			// Find the offsets of our two matches.
			coffsetL := s - (candidateL.offset - e.cur)
			coffsetS := s - (candidateS.offset - e.cur)

			// Check if we have a long match.
			if coffsetL < e.maxMatchOff && uint32(cv) == candidateL.val {
				// Found a long match, likely at least 8 bytes.
				// Reference encoder checks all 8 bytes, we only check 4,
				// but the likelihood of both the first 4 bytes and the hash matching should be enough.
				t = candidateL.offset - e.cur
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

			// Check if we have a short match.
			if coffsetS < e.maxMatchOff && uint32(cv) == candidateS.val {
				// found a regular match
				// See if we can find a long match at s+1
				const checkAt = 1
				cv := load6432(src, s+checkAt)
				nextHashL = hashLen(cv, dFastLongTableBits, dFastLongLen)
				candidateL = e.longTable[nextHashL]
				coffsetL = s - (candidateL.offset - e.cur) + checkAt

				// We can store it, since we have at least a 4 byte match.
				e.longTable[nextHashL] = tableEntry{offset: s + checkAt + e.cur, val: uint32(cv)}
				if coffsetL < e.maxMatchOff && uint32(cv) == candidateL.val {
					// Found a long match, likely at least 8 bytes.
					// Reference encoder checks all 8 bytes, we only check 4,
					// but the likelihood of both the first 4 bytes and the hash matching should be enough.
					t = candidateL.offset - e.cur
					s += checkAt
					if debugMatches {
						println("long match (after short)")
					}
					break
				}

				t = candidateS.offset - e.cur
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

		// A 4-byte match has been found. Update recent offsets.
		// We'll later see if more than 4 bytes.
		offset2 = offset1
		offset1 = s - t

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		if debugAsserts && canRepeat && int(offset1) > len(src) {
			panic("invalid offset")
		}

		// Extend the 4-byte match as long as possible.
		l := e.matchlen(s+4, t+4, src) + 4

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

		// Index match start+1 (long) and start+2 (short)
		index0 := s - l + 1
		// Index match end-2 (long) and end-1 (short)
		index1 := s - 2

		cv0 := load6432(src, index0)
		cv1 := load6432(src, index1)
		te0 := tableEntry{offset: index0 + e.cur, val: uint32(cv0)}
		te1 := tableEntry{offset: index1 + e.cur, val: uint32(cv1)}
		e.longTable[hashLen(cv0, dFastLongTableBits, dFastLongLen)] = te0
		e.longTable[hashLen(cv1, dFastLongTableBits, dFastLongLen)] = te1
		cv0 >>= 8
		cv1 >>= 8
		te0.offset++
		te1.offset++
		te0.val = uint32(cv0)
		te1.val = uint32(cv1)
		e.table[hashLen(cv0, dFastShortTableBits, dFastShortLen)] = te0
		e.table[hashLen(cv1, dFastShortTableBits, dFastShortLen)] = te1

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
			nextHashS := hashLen(cv, dFastShortTableBits, dFastShortLen)
			nextHashL := hashLen(cv, dFastLongTableBits, dFastLongLen)

			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			entry := tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.longTable[nextHashL] = entry
			e.table[nextHashS] = entry
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
func (e *doubleFastEncoder) EncodeNoHist(blk *blockEnc, src []byte) {
	const (
		// Input margin is the number of bytes we read (8)
		// and the maximum we will read ahead (2)
		inputMargin            = 8 + 2
		minNonLiteralBlockSize = 16
	)

	// Protect against e.cur wraparound.
	if e.cur >= e.bufferReset {
		for i := range e.table[:] {
			e.table[i] = tableEntry{}
		}
		for i := range e.longTable[:] {
			e.longTable[i] = tableEntry{}
		}
		e.cur = e.maxMatchOff
	}

	s := int32(0)
	blk.size = len(src)
	if len(src) < minNonLiteralBlockSize {
		blk.extraLits = len(src)
		blk.literals = blk.literals[:len(src)]
		copy(blk.literals, src)
		return
	}

	// Override src
	sLimit := int32(len(src)) - inputMargin
	// stepSize is the number of bytes to skip on every main loop iteration.
	// It should be >= 1.
	const stepSize = 1

	const kSearchStrength = 8

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
		for {

			nextHashL := hashLen(cv, dFastLongTableBits, dFastLongLen)
			nextHashS := hashLen(cv, dFastShortTableBits, dFastShortLen)
			candidateL := e.longTable[nextHashL]
			candidateS := e.table[nextHashS]

			const repOff = 1
			repIndex := s - offset1 + repOff
			entry := tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.longTable[nextHashL] = entry
			e.table[nextHashS] = entry

			if len(blk.sequences) > 2 {
				if load3232(src, repIndex) == uint32(cv>>(repOff*8)) {
					// Consider history as well.
					var seq seq
					//length := 4 + e.matchlen(s+4+repOff, repIndex+4, src)
					length := 4 + int32(matchLen(src[s+4+repOff:], src[repIndex+4:]))

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
					for repIndex > tMin && start > startLimit && src[repIndex-1] == src[start-1] {
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
					s += length + repOff
					nextEmit = s
					if s >= sLimit {
						if debugEncoder {
							println("repeat ended", s, length)

						}
						break encodeLoop
					}
					cv = load6432(src, s)
					continue
				}
			}
			// Find the offsets of our two matches.
			coffsetL := s - (candidateL.offset - e.cur)
			coffsetS := s - (candidateS.offset - e.cur)

			// Check if we have a long match.
			if coffsetL < e.maxMatchOff && uint32(cv) == candidateL.val {
				// Found a long match, likely at least 8 bytes.
				// Reference encoder checks all 8 bytes, we only check 4,
				// but the likelihood of both the first 4 bytes and the hash matching should be enough.
				t = candidateL.offset - e.cur
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d). cur: %d", s, t, e.cur))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugMatches {
					println("long match")
				}
				break
			}

			// Check if we have a short match.
			if coffsetS < e.maxMatchOff && uint32(cv) == candidateS.val {
				// found a regular match
				// See if we can find a long match at s+1
				const checkAt = 1
				cv := load6432(src, s+checkAt)
				nextHashL = hashLen(cv, dFastLongTableBits, dFastLongLen)
				candidateL = e.longTable[nextHashL]
				coffsetL = s - (candidateL.offset - e.cur) + checkAt

				// We can store it, since we have at least a 4 byte match.
				e.longTable[nextHashL] = tableEntry{offset: s + checkAt + e.cur, val: uint32(cv)}
				if coffsetL < e.maxMatchOff && uint32(cv) == candidateL.val {
					// Found a long match, likely at least 8 bytes.
					// Reference encoder checks all 8 bytes, we only check 4,
					// but the likelihood of both the first 4 bytes and the hash matching should be enough.
					t = candidateL.offset - e.cur
					s += checkAt
					if debugMatches {
						println("long match (after short)")
					}
					break
				}

				t = candidateS.offset - e.cur
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

		// A 4-byte match has been found. Update recent offsets.
		// We'll later see if more than 4 bytes.
		offset2 = offset1
		offset1 = s - t

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		// Extend the 4-byte match as long as possible.
		//l := e.matchlen(s+4, t+4, src) + 4
		l := int32(matchLen(src[s+4:], src[t+4:])) + 4

		// Extend backwards
		tMin := s - e.maxMatchOff
		if tMin < 0 {
			tMin = 0
		}
		for t > tMin && s > nextEmit && src[t-1] == src[s-1] {
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

		// Index match start+1 (long) and start+2 (short)
		index0 := s - l + 1
		// Index match end-2 (long) and end-1 (short)
		index1 := s - 2

		cv0 := load6432(src, index0)
		cv1 := load6432(src, index1)
		te0 := tableEntry{offset: index0 + e.cur, val: uint32(cv0)}
		te1 := tableEntry{offset: index1 + e.cur, val: uint32(cv1)}
		e.longTable[hashLen(cv0, dFastLongTableBits, dFastLongLen)] = te0
		e.longTable[hashLen(cv1, dFastLongTableBits, dFastLongLen)] = te1
		cv0 >>= 8
		cv1 >>= 8
		te0.offset++
		te1.offset++
		te0.val = uint32(cv0)
		te1.val = uint32(cv1)
		e.table[hashLen(cv0, dFastShortTableBits, dFastShortLen)] = te0
		e.table[hashLen(cv1, dFastShortTableBits, dFastShortLen)] = te1

		cv = load6432(src, s)

		if len(blk.sequences) <= 2 {
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
			nextHashS := hashLen(cv1>>8, dFastShortTableBits, dFastShortLen)
			nextHashL := hashLen(cv, dFastLongTableBits, dFastLongLen)

			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			//l := 4 + e.matchlen(s+4, o2+4, src)
			l := 4 + int32(matchLen(src[s+4:], src[o2+4:]))

			entry := tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.longTable[nextHashL] = entry
			e.table[nextHashS] = entry
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
	if debugEncoder {
		println("returning, recent offsets:", blk.recentOffsets, "extra literals:", blk.extraLits)
	}

	// We do not store history, so we must offset e.cur to avoid false matches for next user.
	if e.cur < e.bufferReset {
		e.cur += int32(len(src))
	}
}

// Encode will encode the content, with a dictionary if initialized for it.
func (e *doubleFastEncoderDict) Encode(blk *blockEnc, src []byte) {
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
				e.longTable[i] = tableEntry{}
			}
			e.markAllShardsDirty()
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
			if v < minOff {
				v = 0
			} else {
				v = v - e.cur + e.maxMatchOff
			}
			e.longTable[i].offset = v
		}
		e.markAllShardsDirty()
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

	const kSearchStrength = 8

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

		for {
			if debugAsserts && canRepeat && offset1 == 0 {
				panic("offset0 was 0")
			}

			nextHashL := hashLen(cv, dFastLongTableBits, dFastLongLen)
			nextHashS := hashLen(cv, dFastShortTableBits, dFastShortLen)
			candidateL := e.longTable[nextHashL]
			candidateS := e.table[nextHashS]

			const repOff = 1
			repIndex := s - offset1 + repOff
			entry := tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.longTable[nextHashL] = entry
			e.markLongShardDirty(nextHashL)
			e.table[nextHashS] = entry
			e.markShardDirty(nextHashS)

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
					s += length + repOff
					nextEmit = s
					if s >= sLimit {
						if debugEncoder {
							println("repeat ended", s, length)

						}
						break encodeLoop
					}
					cv = load6432(src, s)
					continue
				}
			}
			// Find the offsets of our two matches.
			coffsetL := s - (candidateL.offset - e.cur)
			coffsetS := s - (candidateS.offset - e.cur)

			// Check if we have a long match.
			if coffsetL < e.maxMatchOff && uint32(cv) == candidateL.val {
				// Found a long match, likely at least 8 bytes.
				// Reference encoder checks all 8 bytes, we only check 4,
				// but the likelihood of both the first 4 bytes and the hash matching should be enough.
				t = candidateL.offset - e.cur
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

			// Check if we have a short match.
			if coffsetS < e.maxMatchOff && uint32(cv) == candidateS.val {
				// found a regular match
				// See if we can find a long match at s+1
				const checkAt = 1
				cv := load6432(src, s+checkAt)
				nextHashL = hashLen(cv, dFastLongTableBits, dFastLongLen)
				candidateL = e.longTable[nextHashL]
				coffsetL = s - (candidateL.offset - e.cur) + checkAt

				// We can store it, since we have at least a 4 byte match.
				e.longTable[nextHashL] = tableEntry{offset: s + checkAt + e.cur, val: uint32(cv)}
				e.markLongShardDirty(nextHashL)
				if coffsetL < e.maxMatchOff && uint32(cv) == candidateL.val {
					// Found a long match, likely at least 8 bytes.
					// Reference encoder checks all 8 bytes, we only check 4,
					// but the likelihood of both the first 4 bytes and the hash matching should be enough.
					t = candidateL.offset - e.cur
					s += checkAt
					if debugMatches {
						println("long match (after short)")
					}
					break
				}

				t = candidateS.offset - e.cur
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

		// A 4-byte match has been found. Update recent offsets.
		// We'll later see if more than 4 bytes.
		offset2 = offset1
		offset1 = s - t

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		if debugAsserts && canRepeat && int(offset1) > len(src) {
			panic("invalid offset")
		}

		// Extend the 4-byte match as long as possible.
		l := e.matchlen(s+4, t+4, src) + 4

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

		// Index match start+1 (long) and start+2 (short)
		index0 := s - l + 1
		// Index match end-2 (long) and end-1 (short)
		index1 := s - 2

		cv0 := load6432(src, index0)
		cv1 := load6432(src, index1)
		te0 := tableEntry{offset: index0 + e.cur, val: uint32(cv0)}
		te1 := tableEntry{offset: index1 + e.cur, val: uint32(cv1)}
		longHash1 := hashLen(cv0, dFastLongTableBits, dFastLongLen)
		longHash2 := hashLen(cv1, dFastLongTableBits, dFastLongLen)
		e.longTable[longHash1] = te0
		e.longTable[longHash2] = te1
		e.markLongShardDirty(longHash1)
		e.markLongShardDirty(longHash2)
		cv0 >>= 8
		cv1 >>= 8
		te0.offset++
		te1.offset++
		te0.val = uint32(cv0)
		te1.val = uint32(cv1)
		hashVal1 := hashLen(cv0, dFastShortTableBits, dFastShortLen)
		hashVal2 := hashLen(cv1, dFastShortTableBits, dFastShortLen)
		e.table[hashVal1] = te0
		e.markShardDirty(hashVal1)
		e.table[hashVal2] = te1
		e.markShardDirty(hashVal2)

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
			nextHashL := hashLen(cv, dFastLongTableBits, dFastLongLen)
			nextHashS := hashLen(cv, dFastShortTableBits, dFastShortLen)

			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			entry := tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.longTable[nextHashL] = entry
			e.markLongShardDirty(nextHashL)
			e.table[nextHashS] = entry
			e.markShardDirty(nextHashS)
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
	// If we encoded more than 64K mark all dirty.
	if len(src) > 64<<10 {
		e.markAllShardsDirty()
	}
}

// ResetDict will reset and set a dictionary if not nil
func (e *doubleFastEncoder) Reset(d *dict, singleBlock bool) {
	e.fastEncoder.Reset(d, singleBlock)
	if d != nil {
		panic("doubleFastEncoder: Reset with dict not supported")
	}
}

// ResetDict will reset and set a dictionary if not nil
func (e *doubleFastEncoderDict) Reset(d *dict, singleBlock bool) {
	allDirty := e.allDirty
	e.fastEncoderDict.Reset(d, singleBlock)
	if d == nil {
		return
	}

	// Init or copy dict table
	if len(e.dictLongTable) != len(e.longTable) || d.id != e.lastDictID {
		if len(e.dictLongTable) != len(e.longTable) {
			e.dictLongTable = make([]tableEntry, len(e.longTable))
		}
		if len(d.content) >= 8 {
			cv := load6432(d.content, 0)
			e.dictLongTable[hashLen(cv, dFastLongTableBits, dFastLongLen)] = tableEntry{
				val:    uint32(cv),
				offset: e.maxMatchOff,
			}
			end := int32(len(d.content)) - 8 + e.maxMatchOff
			for i := e.maxMatchOff + 1; i < end; i++ {
				cv = cv>>8 | (uint64(d.content[i-e.maxMatchOff+7]) << 56)
				e.dictLongTable[hashLen(cv, dFastLongTableBits, dFastLongLen)] = tableEntry{
					val:    uint32(cv),
					offset: i,
				}
			}
		}
		e.lastDictID = d.id
		allDirty = true
	}
	// Reset table to initial state
	e.cur = e.maxMatchOff

	dirtyShardCnt := 0
	if !allDirty {
		for i := range e.longTableShardDirty {
			if e.longTableShardDirty[i] {
				dirtyShardCnt++
			}
		}
	}

	if allDirty || dirtyShardCnt > dLongTableShardCnt/2 {
		//copy(e.longTable[:], e.dictLongTable)
		e.longTable = *(*[dFastLongTableSize]tableEntry)(e.dictLongTable)
		for i := range e.longTableShardDirty {
			e.longTableShardDirty[i] = false
		}
		return
	}
	for i := range e.longTableShardDirty {
		if !e.longTableShardDirty[i] {
			continue
		}

		// copy(e.longTable[i*dLongTableShardSize:(i+1)*dLongTableShardSize], e.dictLongTable[i*dLongTableShardSize:(i+1)*dLongTableShardSize])
		*(*[dLongTableShardSize]tableEntry)(e.longTable[i*dLongTableShardSize:]) = *(*[dLongTableShardSize]tableEntry)(e.dictLongTable[i*dLongTableShardSize:])

		e.longTableShardDirty[i] = false
	}
}

func (e *doubleFastEncoderDict) markLongShardDirty(entryNum uint32) {
	e.longTableShardDirty[entryNum/dLongTableShardSize] = true
}
