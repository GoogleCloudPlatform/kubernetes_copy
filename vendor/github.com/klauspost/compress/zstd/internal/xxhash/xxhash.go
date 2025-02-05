// Package xxhash implements the 64-bit variant of xxHash (XXH64) as described
// at http://cyan4973.github.io/xxHash/.
// THIS IS VENDORED: Go to github.com/cespare/xxhash for original package.

package xxhash

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

const (
	prime1 uint64 = 11400714785074694791
	prime2 uint64 = 14029467366897019727
	prime3 uint64 = 1609587929392839161
	prime4 uint64 = 9650029242287828579
	prime5 uint64 = 2870177450012600261
)

// Store the primes in an array as well.
//
// The consts are used when possible in Go code to avoid MOVs but we need a
// contiguous array of the assembly code.
var primes = [...]uint64{prime1, prime2, prime3, prime4, prime5}

// Digest implements hash.Hash64.
type Digest struct {
	v1    uint64
	v2    uint64
	v3    uint64
	v4    uint64
	total uint64
	mem   [32]byte
	n     int // how much of mem is used
}

// New creates a new Digest that computes the 64-bit xxHash algorithm.
func New() *Digest {
	var d Digest
	d.Reset()
	return &d
}

// Reset clears the Digest's state so that it can be reused.
func (d *Digest) Reset() {
	d.v1 = primes[0] + prime2
	d.v2 = prime2
	d.v3 = 0
	d.v4 = -primes[0]
	d.total = 0
	d.n = 0
}

// Size always returns 8 bytes.
func (d *Digest) Size() int { return 8 }

// BlockSize always returns 32 bytes.
func (d *Digest) BlockSize() int { return 32 }

// Write adds more data to d. It always returns len(b), nil.
func (d *Digest) Write(b []byte) (n int, err error) {
	n = len(b)
	d.total += uint64(n)

	memleft := d.mem[d.n&(len(d.mem)-1):]

	if d.n+n < 32 {
		// This new data doesn't even fill the current block.
		copy(memleft, b)
		d.n += n
		return
	}

	if d.n > 0 {
		// Finish off the partial block.
		c := copy(memleft, b)
		d.v1 = round(d.v1, u64(d.mem[0:8]))
		d.v2 = round(d.v2, u64(d.mem[8:16]))
		d.v3 = round(d.v3, u64(d.mem[16:24]))
		d.v4 = round(d.v4, u64(d.mem[24:32]))
		b = b[c:]
		d.n = 0
	}

	if len(b) >= 32 {
		// One or more full blocks left.
		nw := writeBlocks(d, b)
		b = b[nw:]
	}

	// Store any remaining partial block.
	copy(d.mem[:], b)
	d.n = len(b)

	return
}

// Sum appends the current hash to b and returns the resulting slice.
func (d *Digest) Sum(b []byte) []byte {
	s := d.Sum64()
	return append(
		b,
		byte(s>>56),
		byte(s>>48),
		byte(s>>40),
		byte(s>>32),
		byte(s>>24),
		byte(s>>16),
		byte(s>>8),
		byte(s),
	)
}

// Sum64 returns the current hash.
func (d *Digest) Sum64() uint64 {
	var h uint64

	if d.total >= 32 {
		v1, v2, v3, v4 := d.v1, d.v2, d.v3, d.v4
		h = rol1(v1) + rol7(v2) + rol12(v3) + rol18(v4)
		h = mergeRound(h, v1)
		h = mergeRound(h, v2)
		h = mergeRound(h, v3)
		h = mergeRound(h, v4)
	} else {
		h = d.v3 + prime5
	}

	h += d.total

	b := d.mem[:d.n&(len(d.mem)-1)]
	for ; len(b) >= 8; b = b[8:] {
		k1 := round(0, u64(b[:8]))
		h ^= k1
		h = rol27(h)*prime1 + prime4
	}
	if len(b) >= 4 {
		h ^= uint64(u32(b[:4])) * prime1
		h = rol23(h)*prime2 + prime3
		b = b[4:]
	}
	for ; len(b) > 0; b = b[1:] {
		h ^= uint64(b[0]) * prime5
		h = rol11(h) * prime1
	}

	h ^= h >> 33
	h *= prime2
	h ^= h >> 29
	h *= prime3
	h ^= h >> 32

	return h
}

const (
	magic         = "xxh\x06"
	marshaledSize = len(magic) + 8*5 + 32
)

// MarshalBinary implements the encoding.BinaryMarshaler interface.
func (d *Digest) MarshalBinary() ([]byte, error) {
	b := make([]byte, 0, marshaledSize)
	b = append(b, magic...)
	b = appendUint64(b, d.v1)
	b = appendUint64(b, d.v2)
	b = appendUint64(b, d.v3)
	b = appendUint64(b, d.v4)
	b = appendUint64(b, d.total)
	b = append(b, d.mem[:d.n]...)
	b = b[:len(b)+len(d.mem)-d.n]
	return b, nil
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface.
func (d *Digest) UnmarshalBinary(b []byte) error {
	if len(b) < len(magic) || string(b[:len(magic)]) != magic {
		return errors.New("xxhash: invalid hash state identifier")
	}
	if len(b) != marshaledSize {
		return errors.New("xxhash: invalid hash state size")
	}
	b = b[len(magic):]
	b, d.v1 = consumeUint64(b)
	b, d.v2 = consumeUint64(b)
	b, d.v3 = consumeUint64(b)
	b, d.v4 = consumeUint64(b)
	b, d.total = consumeUint64(b)
	copy(d.mem[:], b)
	d.n = int(d.total % uint64(len(d.mem)))
	return nil
}

func appendUint64(b []byte, x uint64) []byte {
	var a [8]byte
	binary.LittleEndian.PutUint64(a[:], x)
	return append(b, a[:]...)
}

func consumeUint64(b []byte) ([]byte, uint64) {
	x := u64(b)
	return b[8:], x
}

func u64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }
func u32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func round(acc, input uint64) uint64 {
	acc += input * prime2
	acc = rol31(acc)
	acc *= prime1
	return acc
}

func mergeRound(acc, val uint64) uint64 {
	val = round(0, val)
	acc ^= val
	acc = acc*prime1 + prime4
	return acc
}

func rol1(x uint64) uint64  { return bits.RotateLeft64(x, 1) }
func rol7(x uint64) uint64  { return bits.RotateLeft64(x, 7) }
func rol11(x uint64) uint64 { return bits.RotateLeft64(x, 11) }
func rol12(x uint64) uint64 { return bits.RotateLeft64(x, 12) }
func rol18(x uint64) uint64 { return bits.RotateLeft64(x, 18) }
func rol23(x uint64) uint64 { return bits.RotateLeft64(x, 23) }
func rol27(x uint64) uint64 { return bits.RotateLeft64(x, 27) }
func rol31(x uint64) uint64 { return bits.RotateLeft64(x, 31) }
