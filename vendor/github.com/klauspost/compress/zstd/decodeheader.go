// Copyright 2020+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.

package zstd

import (
	"encoding/binary"
	"errors"
	"io"
)

// HeaderMaxSize is the maximum size of a Frame and Block Header.
// If less is sent to Header.Decode it *may* still contain enough information.
const HeaderMaxSize = 14 + 3

// Header contains information about the first frame and block within that.
type Header struct {
	// SingleSegment specifies whether the data is to be decompressed into a
	// single contiguous memory segment.
	// It implies that WindowSize is invalid and that FrameContentSize is valid.
	SingleSegment bool

	// WindowSize is the window of data to keep while decoding.
	// Will only be set if SingleSegment is false.
	WindowSize uint64

	// Dictionary ID.
	// If 0, no dictionary.
	DictionaryID uint32

	// HasFCS specifies whether FrameContentSize has a valid value.
	HasFCS bool

	// FrameContentSize is the expected uncompressed size of the entire frame.
	FrameContentSize uint64

	// Skippable will be true if the frame is meant to be skipped.
	// This implies that FirstBlock.OK is false.
	Skippable bool

	// SkippableID is the user-specific ID for the skippable frame.
	// Valid values are between 0 to 15, inclusive.
	SkippableID int

	// SkippableSize is the length of the user data to skip following
	// the header.
	SkippableSize uint32

	// HeaderSize is the raw size of the frame header.
	//
	// For normal frames, it includes the size of the magic number and
	// the size of the header (per section 3.1.1.1).
	// It does not include the size for any data blocks (section 3.1.1.2) nor
	// the size for the trailing content checksum.
	//
	// For skippable frames, this counts the size of the magic number
	// along with the size of the size field of the payload.
	// It does not include the size of the skippable payload itself.
	// The total frame size is the HeaderSize plus the SkippableSize.
	HeaderSize int

	// First block information.
	FirstBlock struct {
		// OK will be set if first block could be decoded.
		OK bool

		// Is this the last block of a frame?
		Last bool

		// Is the data compressed?
		// If true CompressedSize will be populated.
		// Unfortunately DecompressedSize cannot be determined
		// without decoding the blocks.
		Compressed bool

		// DecompressedSize is the expected decompressed size of the block.
		// Will be 0 if it cannot be determined.
		DecompressedSize int

		// CompressedSize of the data in the block.
		// Does not include the block header.
		// Will be equal to DecompressedSize if not Compressed.
		CompressedSize int
	}

	// If set there is a checksum present for the block content.
	// The checksum field at the end is always 4 bytes long.
	HasCheckSum bool
}

// Decode the header from the beginning of the stream.
// This will decode the frame header and the first block header if enough bytes are provided.
// It is recommended to provide at least HeaderMaxSize bytes.
// If the frame header cannot be read an error will be returned.
// If there isn't enough input, io.ErrUnexpectedEOF is returned.
// The FirstBlock.OK will indicate if enough information was available to decode the first block header.
func (h *Header) Decode(in []byte) error {
	_, err := h.DecodeAndStrip(in)
	return err
}

// DecodeAndStrip will decode the header from the beginning of the stream
// and on success return the remaining bytes.
// This will decode the frame header and the first block header if enough bytes are provided.
// It is recommended to provide at least HeaderMaxSize bytes.
// If the frame header cannot be read an error will be returned.
// If there isn't enough input, io.ErrUnexpectedEOF is returned.
// The FirstBlock.OK will indicate if enough information was available to decode the first block header.
func (h *Header) DecodeAndStrip(in []byte) (remain []byte, err error) {
	*h = Header{}
	if len(in) < 4 {
		return nil, io.ErrUnexpectedEOF
	}
	h.HeaderSize += 4
	b, in := in[:4], in[4:]
	if string(b) != frameMagic {
		if string(b[1:4]) != skippableFrameMagic || b[0]&0xf0 != 0x50 {
			return nil, ErrMagicMismatch
		}
		if len(in) < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		h.HeaderSize += 4
		h.Skippable = true
		h.SkippableID = int(b[0] & 0xf)
		h.SkippableSize = binary.LittleEndian.Uint32(in)
		return in[4:], nil
	}

	// Read Window_Descriptor
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#window_descriptor
	if len(in) < 1 {
		return nil, io.ErrUnexpectedEOF
	}
	fhd, in := in[0], in[1:]
	h.HeaderSize++
	h.SingleSegment = fhd&(1<<5) != 0
	h.HasCheckSum = fhd&(1<<2) != 0
	if fhd&(1<<3) != 0 {
		return nil, errors.New("reserved bit set on frame header")
	}

	if !h.SingleSegment {
		if len(in) < 1 {
			return nil, io.ErrUnexpectedEOF
		}
		var wd byte
		wd, in = in[0], in[1:]
		h.HeaderSize++
		windowLog := 10 + (wd >> 3)
		windowBase := uint64(1) << windowLog
		windowAdd := (windowBase / 8) * uint64(wd&0x7)
		h.WindowSize = windowBase + windowAdd
	}

	// Read Dictionary_ID
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#dictionary_id
	if size := fhd & 3; size != 0 {
		if size == 3 {
			size = 4
		}
		if len(in) < int(size) {
			return nil, io.ErrUnexpectedEOF
		}
		b, in = in[:size], in[size:]
		h.HeaderSize += int(size)
		switch len(b) {
		case 1:
			h.DictionaryID = uint32(b[0])
		case 2:
			h.DictionaryID = uint32(b[0]) | (uint32(b[1]) << 8)
		case 4:
			h.DictionaryID = uint32(b[0]) | (uint32(b[1]) << 8) | (uint32(b[2]) << 16) | (uint32(b[3]) << 24)
		}
	}

	// Read Frame_Content_Size
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#frame_content_size
	var fcsSize int
	v := fhd >> 6
	switch v {
	case 0:
		if h.SingleSegment {
			fcsSize = 1
		}
	default:
		fcsSize = 1 << v
	}

	if fcsSize > 0 {
		h.HasFCS = true
		if len(in) < fcsSize {
			return nil, io.ErrUnexpectedEOF
		}
		b, in = in[:fcsSize], in[fcsSize:]
		h.HeaderSize += int(fcsSize)
		switch len(b) {
		case 1:
			h.FrameContentSize = uint64(b[0])
		case 2:
			// When FCS_Field_Size is 2, the offset of 256 is added.
			h.FrameContentSize = uint64(b[0]) | (uint64(b[1]) << 8) + 256
		case 4:
			h.FrameContentSize = uint64(b[0]) | (uint64(b[1]) << 8) | (uint64(b[2]) << 16) | (uint64(b[3]) << 24)
		case 8:
			d1 := uint32(b[0]) | (uint32(b[1]) << 8) | (uint32(b[2]) << 16) | (uint32(b[3]) << 24)
			d2 := uint32(b[4]) | (uint32(b[5]) << 8) | (uint32(b[6]) << 16) | (uint32(b[7]) << 24)
			h.FrameContentSize = uint64(d1) | (uint64(d2) << 32)
		}
	}

	// Frame Header done, we will not fail from now on.
	if len(in) < 3 {
		return in, nil
	}
	tmp := in[:3]
	bh := uint32(tmp[0]) | (uint32(tmp[1]) << 8) | (uint32(tmp[2]) << 16)
	h.FirstBlock.Last = bh&1 != 0
	blockType := blockType((bh >> 1) & 3)
	// find size.
	cSize := int(bh >> 3)
	switch blockType {
	case blockTypeReserved:
		return in, nil
	case blockTypeRLE:
		h.FirstBlock.Compressed = true
		h.FirstBlock.DecompressedSize = cSize
		h.FirstBlock.CompressedSize = 1
	case blockTypeCompressed:
		h.FirstBlock.Compressed = true
		h.FirstBlock.CompressedSize = cSize
	case blockTypeRaw:
		h.FirstBlock.DecompressedSize = cSize
		h.FirstBlock.CompressedSize = cSize
	default:
		panic("Invalid block type")
	}

	h.FirstBlock.OK = true
	return in, nil
}

// AppendTo will append the encoded header to the dst slice.
// There is no error checking performed on the header values.
func (h *Header) AppendTo(dst []byte) ([]byte, error) {
	if h.Skippable {
		magic := [4]byte{0x50, 0x2a, 0x4d, 0x18}
		magic[0] |= byte(h.SkippableID & 0xf)
		dst = append(dst, magic[:]...)
		f := h.SkippableSize
		return append(dst, uint8(f), uint8(f>>8), uint8(f>>16), uint8(f>>24)), nil
	}
	f := frameHeader{
		ContentSize:   h.FrameContentSize,
		WindowSize:    uint32(h.WindowSize),
		SingleSegment: h.SingleSegment,
		Checksum:      h.HasCheckSum,
		DictID:        h.DictionaryID,
	}
	return f.appendTo(dst), nil
}
