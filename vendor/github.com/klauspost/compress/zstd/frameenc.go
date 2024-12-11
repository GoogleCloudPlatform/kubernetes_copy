// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"
)

type frameHeader struct {
	ContentSize   uint64
	WindowSize    uint32
	SingleSegment bool
	Checksum      bool
	DictID        uint32
}

const maxHeaderSize = 14

func (f frameHeader) appendTo(dst []byte) []byte {
	dst = append(dst, frameMagic...)
	var fhd uint8
	if f.Checksum {
		fhd |= 1 << 2
	}
	if f.SingleSegment {
		fhd |= 1 << 5
	}

	var dictIDContent []byte
	if f.DictID > 0 {
		var tmp [4]byte
		if f.DictID < 256 {
			fhd |= 1
			tmp[0] = uint8(f.DictID)
			dictIDContent = tmp[:1]
		} else if f.DictID < 1<<16 {
			fhd |= 2
			binary.LittleEndian.PutUint16(tmp[:2], uint16(f.DictID))
			dictIDContent = tmp[:2]
		} else {
			fhd |= 3
			binary.LittleEndian.PutUint32(tmp[:4], f.DictID)
			dictIDContent = tmp[:4]
		}
	}
	var fcs uint8
	if f.ContentSize >= 256 {
		fcs++
	}
	if f.ContentSize >= 65536+256 {
		fcs++
	}
	if f.ContentSize >= 0xffffffff {
		fcs++
	}

	fhd |= fcs << 6

	dst = append(dst, fhd)
	if !f.SingleSegment {
		const winLogMin = 10
		windowLog := (bits.Len32(f.WindowSize-1) - winLogMin) << 3
		dst = append(dst, uint8(windowLog))
	}
	if f.DictID > 0 {
		dst = append(dst, dictIDContent...)
	}
	switch fcs {
	case 0:
		if f.SingleSegment {
			dst = append(dst, uint8(f.ContentSize))
		}
		// Unless SingleSegment is set, framessizes < 256 are not stored.
	case 1:
		f.ContentSize -= 256
		dst = append(dst, uint8(f.ContentSize), uint8(f.ContentSize>>8))
	case 2:
		dst = append(dst, uint8(f.ContentSize), uint8(f.ContentSize>>8), uint8(f.ContentSize>>16), uint8(f.ContentSize>>24))
	case 3:
		dst = append(dst, uint8(f.ContentSize), uint8(f.ContentSize>>8), uint8(f.ContentSize>>16), uint8(f.ContentSize>>24),
			uint8(f.ContentSize>>32), uint8(f.ContentSize>>40), uint8(f.ContentSize>>48), uint8(f.ContentSize>>56))
	default:
		panic("invalid fcs")
	}
	return dst
}

const skippableFrameHeader = 4 + 4

// calcSkippableFrame will return a total size to be added for written
// to be divisible by multiple.
// The value will always be > skippableFrameHeader.
// The function will panic if written < 0 or wantMultiple <= 0.
func calcSkippableFrame(written, wantMultiple int64) int {
	if wantMultiple <= 0 {
		panic("wantMultiple <= 0")
	}
	if written < 0 {
		panic("written < 0")
	}
	leftOver := written % wantMultiple
	if leftOver == 0 {
		return 0
	}
	toAdd := wantMultiple - leftOver
	for toAdd < skippableFrameHeader {
		toAdd += wantMultiple
	}
	return int(toAdd)
}

// skippableFrame will add a skippable frame with a total size of bytes.
// total should be >= skippableFrameHeader and < math.MaxUint32.
func skippableFrame(dst []byte, total int, r io.Reader) ([]byte, error) {
	if total == 0 {
		return dst, nil
	}
	if total < skippableFrameHeader {
		return dst, fmt.Errorf("requested skippable frame (%d) < 8", total)
	}
	if int64(total) > math.MaxUint32 {
		return dst, fmt.Errorf("requested skippable frame (%d) > max uint32", total)
	}
	dst = append(dst, 0x50, 0x2a, 0x4d, 0x18)
	f := uint32(total - skippableFrameHeader)
	dst = append(dst, uint8(f), uint8(f>>8), uint8(f>>16), uint8(f>>24))
	start := len(dst)
	dst = append(dst, make([]byte, f)...)
	_, err := io.ReadFull(r, dst[start:])
	return dst, err
}
