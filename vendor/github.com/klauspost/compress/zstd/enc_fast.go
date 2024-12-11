// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"fmt"
)

const (
	tableBits        = 15                               // Bits used in the table
	tableSize        = 1 << tableBits                   // Size of the table
	tableShardCnt    = 1 << (tableBits - dictShardBits) // Number of shards in the table
	tableShardSize   = tableSize / tableShardCnt        // Size of an individual shard
	tableFastHashLen = 6
	tableMask        = tableSize - 1 // Mask for table indices. Redundant, but can eliminate bounds checks.
	maxMatchLength   = 131074
)

type tableEntry struct {
	val    uint32
	offset int32
}

type fastEncoder struct {
	fastBase
	table [tableSize]tableEntry
}

type fastEncoderDict struct {
	fastEncoder
	dictTable       []tableEntry
	tableShardDirty [tableShardCnt]bool
	allDirty        bool
}

// Encode mimmics functionality in zstd_fast.c
func (e *fastEncoder) Encode(blk *blockEnc, src []byte) {
	const (
		inputMargin            = 8
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)

	// Protect against e.cur wraparound.
	for e.cur >= e.bufferReset-int32(len(e.hist)) {
		if len(e.hist) == 0 {
			for i := range e.table[:] {
				e.table[i] = tableEntry{}
			}
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
	// It should be >= 2.
	const stepSize = 2

	// TEMPLATE
	const hashLog = tableBits
	// seems global, but would be nice to tweak.
	const kSearchStrength = 6

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
		// t will contain the match offset when we find one.
		// When existing the search loop, we have already checked 4 bytes.
		var t int32

		// We will not use repeat offsets across blocks.
		// By not using them for the first 3 matches
		canRepeat := len(blk.sequences) > 2

		for {
			if debugAsserts && canRepeat && offset1 == 0 {
				panic("offset0 was 0")
			}

			nextHash := hashLen(cv, hashLog, tableFastHashLen)
			nextHash2 := hashLen(cv>>8, hashLog, tableFastHashLen)
			candidate := e.table[nextHash]
			candidate2 := e.table[nextHash2]
			repIndex := s - offset1 + 2

			e.table[nextHash] = tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.table[nextHash2] = tableEntry{offset: s + e.cur + 1, val: uint32(cv >> 8)}

			if canRepeat && repIndex >= 0 && load3232(src, repIndex) == uint32(cv>>16) {
				// Consider history as well.
				var seq seq
				length := 4 + e.matchlen(s+6, repIndex+4, src)
				seq.matchLen = uint32(length - zstdMinMatch)

				// We might be able to match backwards.
				// Extend as long as we can.
				start := s + 2
				// We end the search early, so we don't risk 0 literals
				// and have to do special offset treatment.
				startLimit := nextEmit + 1

				sMin := s - e.maxMatchOff
				if sMin < 0 {
					sMin = 0
				}
				for repIndex > sMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch {
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
				s += length + 2
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
			coffset0 := s - (candidate.offset - e.cur)
			coffset1 := s - (candidate2.offset - e.cur) + 1
			if coffset0 < e.maxMatchOff && uint32(cv) == candidate.val {
				// found a regular match
				t = candidate.offset - e.cur
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				break
			}

			if coffset1 < e.maxMatchOff && uint32(cv>>8) == candidate2.val {
				// found a regular match
				t = candidate2.offset - e.cur
				s++
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugAsserts && t < 0 {
					panic("t<0")
				}
				break
			}
			s += stepSize + ((s - nextEmit) >> (kSearchStrength - 1))
			if s >= sLimit {
				break encodeLoop
			}
			cv = load6432(src, s)
		}
		// A 4-byte match has been found. We'll later see if more than 4 bytes.
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

		// Write our sequence.
		var seq seq
		seq.litLen = uint32(s - nextEmit)
		seq.matchLen = uint32(l - zstdMinMatch)
		if seq.litLen > 0 {
			blk.literals = append(blk.literals, src[nextEmit:s]...)
		}
		// Don't use repeat offsets
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
		cv = load6432(src, s)

		// Check offset 2
		if o2 := s - offset2; canRepeat && load3232(src, o2) == uint32(cv) {
			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			// Store this, since we have it.
			nextHash := hashLen(cv, hashLog, tableFastHashLen)
			e.table[nextHash] = tableEntry{offset: s + e.cur, val: uint32(cv)}
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
				break encodeLoop
			}
			// Prepare next loop.
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
func (e *fastEncoder) EncodeNoHist(blk *blockEnc, src []byte) {
	const (
		inputMargin            = 8
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)
	if debugEncoder {
		if len(src) > maxCompressedBlockSize {
			panic("src too big")
		}
	}

	// Protect against e.cur wraparound.
	if e.cur >= e.bufferReset {
		for i := range e.table[:] {
			e.table[i] = tableEntry{}
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

	sLimit := int32(len(src)) - inputMargin
	// stepSize is the number of bytes to skip on every main loop iteration.
	// It should be >= 2.
	const stepSize = 2

	// TEMPLATE
	const hashLog = tableBits
	// seems global, but would be nice to tweak.
	const kSearchStrength = 6

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
		// t will contain the match offset when we find one.
		// When existing the search loop, we have already checked 4 bytes.
		var t int32

		// We will not use repeat offsets across blocks.
		// By not using them for the first 3 matches

		for {
			nextHash := hashLen(cv, hashLog, tableFastHashLen)
			nextHash2 := hashLen(cv>>8, hashLog, tableFastHashLen)
			candidate := e.table[nextHash]
			candidate2 := e.table[nextHash2]
			repIndex := s - offset1 + 2

			e.table[nextHash] = tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.table[nextHash2] = tableEntry{offset: s + e.cur + 1, val: uint32(cv >> 8)}

			if len(blk.sequences) > 2 && load3232(src, repIndex) == uint32(cv>>16) {
				// Consider history as well.
				var seq seq
				length := 4 + e.matchlen(s+6, repIndex+4, src)

				seq.matchLen = uint32(length - zstdMinMatch)

				// We might be able to match backwards.
				// Extend as long as we can.
				start := s + 2
				// We end the search early, so we don't risk 0 literals
				// and have to do special offset treatment.
				startLimit := nextEmit + 1

				sMin := s - e.maxMatchOff
				if sMin < 0 {
					sMin = 0
				}
				for repIndex > sMin && start > startLimit && src[repIndex-1] == src[start-1] {
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
				s += length + 2
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
			coffset0 := s - (candidate.offset - e.cur)
			coffset1 := s - (candidate2.offset - e.cur) + 1
			if coffset0 < e.maxMatchOff && uint32(cv) == candidate.val {
				// found a regular match
				t = candidate.offset - e.cur
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugAsserts && t < 0 {
					panic(fmt.Sprintf("t (%d) < 0, candidate.offset: %d, e.cur: %d, coffset0: %d, e.maxMatchOff: %d", t, candidate.offset, e.cur, coffset0, e.maxMatchOff))
				}
				break
			}

			if coffset1 < e.maxMatchOff && uint32(cv>>8) == candidate2.val {
				// found a regular match
				t = candidate2.offset - e.cur
				s++
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugAsserts && t < 0 {
					panic("t<0")
				}
				break
			}
			s += stepSize + ((s - nextEmit) >> (kSearchStrength - 1))
			if s >= sLimit {
				break encodeLoop
			}
			cv = load6432(src, s)
		}
		// A 4-byte match has been found. We'll later see if more than 4 bytes.
		offset2 = offset1
		offset1 = s - t

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		if debugAsserts && t < 0 {
			panic(fmt.Sprintf("t (%d) < 0 ", t))
		}
		// Extend the 4-byte match as long as possible.
		l := e.matchlen(s+4, t+4, src) + 4

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

		// Write our sequence.
		var seq seq
		seq.litLen = uint32(s - nextEmit)
		seq.matchLen = uint32(l - zstdMinMatch)
		if seq.litLen > 0 {
			blk.literals = append(blk.literals, src[nextEmit:s]...)
		}
		// Don't use repeat offsets
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
		cv = load6432(src, s)

		// Check offset 2
		if o2 := s - offset2; len(blk.sequences) > 2 && load3232(src, o2) == uint32(cv) {
			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			// Store this, since we have it.
			nextHash := hashLen(cv, hashLog, tableFastHashLen)
			e.table[nextHash] = tableEntry{offset: s + e.cur, val: uint32(cv)}
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
				break encodeLoop
			}
			// Prepare next loop.
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
func (e *fastEncoderDict) Encode(blk *blockEnc, src []byte) {
	const (
		inputMargin            = 8
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)
	if e.allDirty || len(src) > 32<<10 {
		e.fastEncoder.Encode(blk, src)
		e.allDirty = true
		return
	}
	// Protect against e.cur wraparound.
	for e.cur >= e.bufferReset-int32(len(e.hist)) {
		if len(e.hist) == 0 {
			e.table = [tableSize]tableEntry{}
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
	// It should be >= 2.
	const stepSize = 2

	// TEMPLATE
	const hashLog = tableBits
	// seems global, but would be nice to tweak.
	const kSearchStrength = 7

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
		// t will contain the match offset when we find one.
		// When existing the search loop, we have already checked 4 bytes.
		var t int32

		// We will not use repeat offsets across blocks.
		// By not using them for the first 3 matches
		canRepeat := len(blk.sequences) > 2

		for {
			if debugAsserts && canRepeat && offset1 == 0 {
				panic("offset0 was 0")
			}

			nextHash := hashLen(cv, hashLog, tableFastHashLen)
			nextHash2 := hashLen(cv>>8, hashLog, tableFastHashLen)
			candidate := e.table[nextHash]
			candidate2 := e.table[nextHash2]
			repIndex := s - offset1 + 2

			e.table[nextHash] = tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.markShardDirty(nextHash)
			e.table[nextHash2] = tableEntry{offset: s + e.cur + 1, val: uint32(cv >> 8)}
			e.markShardDirty(nextHash2)

			if canRepeat && repIndex >= 0 && load3232(src, repIndex) == uint32(cv>>16) {
				// Consider history as well.
				var seq seq
				length := 4 + e.matchlen(s+6, repIndex+4, src)

				seq.matchLen = uint32(length - zstdMinMatch)

				// We might be able to match backwards.
				// Extend as long as we can.
				start := s + 2
				// We end the search early, so we don't risk 0 literals
				// and have to do special offset treatment.
				startLimit := nextEmit + 1

				sMin := s - e.maxMatchOff
				if sMin < 0 {
					sMin = 0
				}
				for repIndex > sMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch {
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
				s += length + 2
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
			coffset0 := s - (candidate.offset - e.cur)
			coffset1 := s - (candidate2.offset - e.cur) + 1
			if coffset0 < e.maxMatchOff && uint32(cv) == candidate.val {
				// found a regular match
				t = candidate.offset - e.cur
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				break
			}

			if coffset1 < e.maxMatchOff && uint32(cv>>8) == candidate2.val {
				// found a regular match
				t = candidate2.offset - e.cur
				s++
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugAsserts && t < 0 {
					panic("t<0")
				}
				break
			}
			s += stepSize + ((s - nextEmit) >> (kSearchStrength - 1))
			if s >= sLimit {
				break encodeLoop
			}
			cv = load6432(src, s)
		}
		// A 4-byte match has been found. We'll later see if more than 4 bytes.
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

		// Write our sequence.
		var seq seq
		seq.litLen = uint32(s - nextEmit)
		seq.matchLen = uint32(l - zstdMinMatch)
		if seq.litLen > 0 {
			blk.literals = append(blk.literals, src[nextEmit:s]...)
		}
		// Don't use repeat offsets
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
		cv = load6432(src, s)

		// Check offset 2
		if o2 := s - offset2; canRepeat && load3232(src, o2) == uint32(cv) {
			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			// Store this, since we have it.
			nextHash := hashLen(cv, hashLog, tableFastHashLen)
			e.table[nextHash] = tableEntry{offset: s + e.cur, val: uint32(cv)}
			e.markShardDirty(nextHash)
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
				break encodeLoop
			}
			// Prepare next loop.
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
func (e *fastEncoder) Reset(d *dict, singleBlock bool) {
	e.resetBase(d, singleBlock)
	if d != nil {
		panic("fastEncoder: Reset with dict")
	}
}

// ResetDict will reset and set a dictionary if not nil
func (e *fastEncoderDict) Reset(d *dict, singleBlock bool) {
	e.resetBase(d, singleBlock)
	if d == nil {
		return
	}

	// Init or copy dict table
	if len(e.dictTable) != len(e.table) || d.id != e.lastDictID {
		if len(e.dictTable) != len(e.table) {
			e.dictTable = make([]tableEntry, len(e.table))
		}
		if true {
			end := e.maxMatchOff + int32(len(d.content)) - 8
			for i := e.maxMatchOff; i < end; i += 2 {
				const hashLog = tableBits

				cv := load6432(d.content, i-e.maxMatchOff)
				nextHash := hashLen(cv, hashLog, tableFastHashLen)     // 0 -> 6
				nextHash1 := hashLen(cv>>8, hashLog, tableFastHashLen) // 1 -> 7
				e.dictTable[nextHash] = tableEntry{
					val:    uint32(cv),
					offset: i,
				}
				e.dictTable[nextHash1] = tableEntry{
					val:    uint32(cv >> 8),
					offset: i + 1,
				}
			}
		}
		e.lastDictID = d.id
		e.allDirty = true
	}

	e.cur = e.maxMatchOff
	dirtyShardCnt := 0
	if !e.allDirty {
		for i := range e.tableShardDirty {
			if e.tableShardDirty[i] {
				dirtyShardCnt++
			}
		}
	}

	const shardCnt = tableShardCnt
	const shardSize = tableShardSize
	if e.allDirty || dirtyShardCnt > shardCnt*4/6 {
		//copy(e.table[:], e.dictTable)
		e.table = *(*[tableSize]tableEntry)(e.dictTable)
		for i := range e.tableShardDirty {
			e.tableShardDirty[i] = false
		}
		e.allDirty = false
		return
	}
	for i := range e.tableShardDirty {
		if !e.tableShardDirty[i] {
			continue
		}

		//copy(e.table[i*shardSize:(i+1)*shardSize], e.dictTable[i*shardSize:(i+1)*shardSize])
		*(*[shardSize]tableEntry)(e.table[i*shardSize:]) = *(*[shardSize]tableEntry)(e.dictTable[i*shardSize:])
		e.tableShardDirty[i] = false
	}
	e.allDirty = false
}

func (e *fastEncoderDict) markAllShardsDirty() {
	e.allDirty = true
}

func (e *fastEncoderDict) markShardDirty(entryNum uint32) {
	e.tableShardDirty[entryNum/tableShardSize] = true
}
