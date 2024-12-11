// Copyright 2016 The Snappy-Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package snapref

func load32(b []byte, i int) uint32 {
	b = b[i : i+4 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func load64(b []byte, i int) uint64 {
	b = b[i : i+8 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// emitLiteral writes a literal chunk and returns the number of bytes written.
//
// It assumes that:
//
//	dst is long enough to hold the encoded bytes
//	1 <= len(lit) && len(lit) <= 65536
func emitLiteral(dst, lit []byte) int {
	i, n := 0, uint(len(lit)-1)
	switch {
	case n < 60:
		dst[0] = uint8(n)<<2 | tagLiteral
		i = 1
	case n < 1<<8:
		dst[0] = 60<<2 | tagLiteral
		dst[1] = uint8(n)
		i = 2
	default:
		dst[0] = 61<<2 | tagLiteral
		dst[1] = uint8(n)
		dst[2] = uint8(n >> 8)
		i = 3
	}
	return i + copy(dst[i:], lit)
}

// emitCopy writes a copy chunk and returns the number of bytes written.
//
// It assumes that:
//
//	dst is long enough to hold the encoded bytes
//	1 <= offset && offset <= 65535
//	4 <= length && length <= 65535
func emitCopy(dst []byte, offset, length int) int {
	i := 0
	// The maximum length for a single tagCopy1 or tagCopy2 op is 64 bytes. The
	// threshold for this loop is a little higher (at 68 = 64 + 4), and the
	// length emitted down below is a little lower (at 60 = 64 - 4), because
	// it's shorter to encode a length 67 copy as a length 60 tagCopy2 followed
	// by a length 7 tagCopy1 (which encodes as 3+2 bytes) than to encode it as
	// a length 64 tagCopy2 followed by a length 3 tagCopy2 (which encodes as
	// 3+3 bytes). The magic 4 in the 64±4 is because the minimum length for a
	// tagCopy1 op is 4 bytes, which is why a length 3 copy has to be an
	// encodes-as-3-bytes tagCopy2 instead of an encodes-as-2-bytes tagCopy1.
	for length >= 68 {
		// Emit a length 64 copy, encoded as 3 bytes.
		dst[i+0] = 63<<2 | tagCopy2
		dst[i+1] = uint8(offset)
		dst[i+2] = uint8(offset >> 8)
		i += 3
		length -= 64
	}
	if length > 64 {
		// Emit a length 60 copy, encoded as 3 bytes.
		dst[i+0] = 59<<2 | tagCopy2
		dst[i+1] = uint8(offset)
		dst[i+2] = uint8(offset >> 8)
		i += 3
		length -= 60
	}
	if length >= 12 || offset >= 2048 {
		// Emit the remaining copy, encoded as 3 bytes.
		dst[i+0] = uint8(length-1)<<2 | tagCopy2
		dst[i+1] = uint8(offset)
		dst[i+2] = uint8(offset >> 8)
		return i + 3
	}
	// Emit the remaining copy, encoded as 2 bytes.
	dst[i+0] = uint8(offset>>8)<<5 | uint8(length-4)<<2 | tagCopy1
	dst[i+1] = uint8(offset)
	return i + 2
}

func hash(u, shift uint32) uint32 {
	return (u * 0x1e35a7bd) >> shift
}

// EncodeBlockInto exposes encodeBlock but checks dst size.
func EncodeBlockInto(dst, src []byte) (d int) {
	if MaxEncodedLen(len(src)) > len(dst) {
		return 0
	}

	// encodeBlock breaks on too big blocks, so split.
	for len(src) > 0 {
		p := src
		src = nil
		if len(p) > maxBlockSize {
			p, src = p[:maxBlockSize], p[maxBlockSize:]
		}
		if len(p) < minNonLiteralBlockSize {
			d += emitLiteral(dst[d:], p)
		} else {
			d += encodeBlock(dst[d:], p)
		}
	}
	return d
}

// encodeBlock encodes a non-empty src to a guaranteed-large-enough dst. It
// assumes that the varint-encoded length of the decompressed bytes has already
// been written.
//
// It also assumes that:
//
//	len(dst) >= MaxEncodedLen(len(src)) &&
//	minNonLiteralBlockSize <= len(src) && len(src) <= maxBlockSize
func encodeBlock(dst, src []byte) (d int) {
	// Initialize the hash table. Its size ranges from 1<<8 to 1<<14 inclusive.
	// The table element type is uint16, as s < sLimit and sLimit < len(src)
	// and len(src) <= maxBlockSize and maxBlockSize == 65536.
	const (
		maxTableSize = 1 << 14
		// tableMask is redundant, but helps the compiler eliminate bounds
		// checks.
		tableMask = maxTableSize - 1
	)
	shift := uint32(32 - 8)
	for tableSize := 1 << 8; tableSize < maxTableSize && tableSize < len(src); tableSize *= 2 {
		shift--
	}
	// In Go, all array elements are zero-initialized, so there is no advantage
	// to a smaller tableSize per se. However, it matches the C++ algorithm,
	// and in the asm versions of this code, we can get away with zeroing only
	// the first tableSize elements.
	var table [maxTableSize]uint16

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := len(src) - inputMargin

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := 0

	// The encoded form must start with a literal, as there are no previous
	// bytes to copy, so we start looking for hash matches at s == 1.
	s := 1
	nextHash := hash(load32(src, s), shift)

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		skip := 32

		nextS := s
		candidate := 0
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				goto emitRemainder
			}
			candidate = int(table[nextHash&tableMask])
			table[nextHash&tableMask] = uint16(s)
			nextHash = hash(load32(src, nextS), shift)
			if load32(src, s) == load32(src, candidate) {
				break
			}
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.
		d += emitLiteral(dst[d:], src[nextEmit:s])

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.
			base := s

			// Extend the 4-byte match as long as possible.
			//
			// This is an inlined version of:
			//	s = extendMatch(src, candidate+4, s+4)
			s += 4
			for i := candidate + 4; s < len(src) && src[i] == src[s]; i, s = i+1, s+1 {
			}

			d += emitCopy(dst[d:], base-candidate, s-base)
			nextEmit = s
			if s >= sLimit {
				goto emitRemainder
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-1 and at s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load64(src, s-1)
			prevHash := hash(uint32(x>>0), shift)
			table[prevHash&tableMask] = uint16(s - 1)
			currHash := hash(uint32(x>>8), shift)
			candidate = int(table[currHash&tableMask])
			table[currHash&tableMask] = uint16(s)
			if uint32(x>>8) != load32(src, candidate) {
				nextHash = hash(uint32(x>>16), shift)
				s++
				break
			}
		}
	}

emitRemainder:
	if nextEmit < len(src) {
		d += emitLiteral(dst[d:], src[nextEmit:])
	}
	return d
}
