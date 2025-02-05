// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

// byteReader provides a byte reader that reads
// little endian values from a byte stream.
// The input stream is manually advanced.
// The reader performs no bounds checks.
type byteReader struct {
	b   []byte
	off int
}

// advance the stream b n bytes.
func (b *byteReader) advance(n uint) {
	b.off += int(n)
}

// overread returns whether we have advanced too far.
func (b *byteReader) overread() bool {
	return b.off > len(b.b)
}

// Int32 returns a little endian int32 starting at current offset.
func (b byteReader) Int32() int32 {
	b2 := b.b[b.off:]
	b2 = b2[:4]
	v3 := int32(b2[3])
	v2 := int32(b2[2])
	v1 := int32(b2[1])
	v0 := int32(b2[0])
	return v0 | (v1 << 8) | (v2 << 16) | (v3 << 24)
}

// Uint8 returns the next byte
func (b *byteReader) Uint8() uint8 {
	v := b.b[b.off]
	return v
}

// Uint32 returns a little endian uint32 starting at current offset.
func (b byteReader) Uint32() uint32 {
	if r := b.remain(); r < 4 {
		// Very rare
		v := uint32(0)
		for i := 1; i <= r; i++ {
			v = (v << 8) | uint32(b.b[len(b.b)-i])
		}
		return v
	}
	b2 := b.b[b.off:]
	b2 = b2[:4]
	v3 := uint32(b2[3])
	v2 := uint32(b2[2])
	v1 := uint32(b2[1])
	v0 := uint32(b2[0])
	return v0 | (v1 << 8) | (v2 << 16) | (v3 << 24)
}

// Uint32NC returns a little endian uint32 starting at current offset.
// The caller must be sure if there are at least 4 bytes left.
func (b byteReader) Uint32NC() uint32 {
	b2 := b.b[b.off:]
	b2 = b2[:4]
	v3 := uint32(b2[3])
	v2 := uint32(b2[2])
	v1 := uint32(b2[1])
	v0 := uint32(b2[0])
	return v0 | (v1 << 8) | (v2 << 16) | (v3 << 24)
}

// unread returns the unread portion of the input.
func (b byteReader) unread() []byte {
	return b.b[b.off:]
}

// remain will return the number of bytes remaining.
func (b byteReader) remain() int {
	return len(b.b) - b.off
}
