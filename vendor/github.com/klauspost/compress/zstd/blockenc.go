// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"github.com/klauspost/compress/huff0"
)

type blockEnc struct {
	size       int
	literals   []byte
	sequences  []seq
	coders     seqCoders
	litEnc     *huff0.Scratch
	dictLitEnc *huff0.Scratch
	wr         bitWriter

	extraLits         int
	output            []byte
	recentOffsets     [3]uint32
	prevRecentOffsets [3]uint32

	last   bool
	lowMem bool
}

// init should be used once the block has been created.
// If called more than once, the effect is the same as calling reset.
func (b *blockEnc) init() {
	if b.lowMem {
		// 1K literals
		if cap(b.literals) < 1<<10 {
			b.literals = make([]byte, 0, 1<<10)
		}
		const defSeqs = 20
		if cap(b.sequences) < defSeqs {
			b.sequences = make([]seq, 0, defSeqs)
		}
		// 1K
		if cap(b.output) < 1<<10 {
			b.output = make([]byte, 0, 1<<10)
		}
	} else {
		if cap(b.literals) < maxCompressedBlockSize {
			b.literals = make([]byte, 0, maxCompressedBlockSize)
		}
		const defSeqs = 2000
		if cap(b.sequences) < defSeqs {
			b.sequences = make([]seq, 0, defSeqs)
		}
		if cap(b.output) < maxCompressedBlockSize {
			b.output = make([]byte, 0, maxCompressedBlockSize)
		}
	}

	if b.coders.mlEnc == nil {
		b.coders.mlEnc = &fseEncoder{}
		b.coders.mlPrev = &fseEncoder{}
		b.coders.ofEnc = &fseEncoder{}
		b.coders.ofPrev = &fseEncoder{}
		b.coders.llEnc = &fseEncoder{}
		b.coders.llPrev = &fseEncoder{}
	}
	b.litEnc = &huff0.Scratch{WantLogLess: 4}
	b.reset(nil)
}

// initNewEncode can be used to reset offsets and encoders to the initial state.
func (b *blockEnc) initNewEncode() {
	b.recentOffsets = [3]uint32{1, 4, 8}
	b.litEnc.Reuse = huff0.ReusePolicyNone
	b.coders.setPrev(nil, nil, nil)
}

// reset will reset the block for a new encode, but in the same stream,
// meaning that state will be carried over, but the block content is reset.
// If a previous block is provided, the recent offsets are carried over.
func (b *blockEnc) reset(prev *blockEnc) {
	b.extraLits = 0
	b.literals = b.literals[:0]
	b.size = 0
	b.sequences = b.sequences[:0]
	b.output = b.output[:0]
	b.last = false
	if prev != nil {
		b.recentOffsets = prev.prevRecentOffsets
	}
	b.dictLitEnc = nil
}

// reset will reset the block for a new encode, but in the same stream,
// meaning that state will be carried over, but the block content is reset.
// If a previous block is provided, the recent offsets are carried over.
func (b *blockEnc) swapEncoders(prev *blockEnc) {
	b.coders.swap(&prev.coders)
	b.litEnc, prev.litEnc = prev.litEnc, b.litEnc
}

// blockHeader contains the information for a block header.
type blockHeader uint32

// setLast sets the 'last' indicator on a block.
func (h *blockHeader) setLast(b bool) {
	if b {
		*h = *h | 1
	} else {
		const mask = (1 << 24) - 2
		*h = *h & mask
	}
}

// setSize will store the compressed size of a block.
func (h *blockHeader) setSize(v uint32) {
	const mask = 7
	*h = (*h)&mask | blockHeader(v<<3)
}

// setType sets the block type.
func (h *blockHeader) setType(t blockType) {
	const mask = 1 | (((1 << 24) - 1) ^ 7)
	*h = (*h & mask) | blockHeader(t<<1)
}

// appendTo will append the block header to a slice.
func (h blockHeader) appendTo(b []byte) []byte {
	return append(b, uint8(h), uint8(h>>8), uint8(h>>16))
}

// String returns a string representation of the block.
func (h blockHeader) String() string {
	return fmt.Sprintf("Type: %d, Size: %d, Last:%t", (h>>1)&3, h>>3, h&1 == 1)
}

// literalsHeader contains literals header information.
type literalsHeader uint64

// setType can be used to set the type of literal block.
func (h *literalsHeader) setType(t literalsBlockType) {
	const mask = math.MaxUint64 - 3
	*h = (*h & mask) | literalsHeader(t)
}

// setSize can be used to set a single size, for uncompressed and RLE content.
func (h *literalsHeader) setSize(regenLen int) {
	inBits := bits.Len32(uint32(regenLen))
	// Only retain 2 bits
	const mask = 3
	lh := uint64(*h & mask)
	switch {
	case inBits < 5:
		lh |= (uint64(regenLen) << 3) | (1 << 60)
		if debugEncoder {
			got := int(lh>>3) & 0xff
			if got != regenLen {
				panic(fmt.Sprint("litRegenSize = ", regenLen, "(want) != ", got, "(got)"))
			}
		}
	case inBits < 12:
		lh |= (1 << 2) | (uint64(regenLen) << 4) | (2 << 60)
	case inBits < 20:
		lh |= (3 << 2) | (uint64(regenLen) << 4) | (3 << 60)
	default:
		panic(fmt.Errorf("internal error: block too big (%d)", regenLen))
	}
	*h = literalsHeader(lh)
}

// setSizes will set the size of a compressed literals section and the input length.
func (h *literalsHeader) setSizes(compLen, inLen int, single bool) {
	compBits, inBits := bits.Len32(uint32(compLen)), bits.Len32(uint32(inLen))
	// Only retain 2 bits
	const mask = 3
	lh := uint64(*h & mask)
	switch {
	case compBits <= 10 && inBits <= 10:
		if !single {
			lh |= 1 << 2
		}
		lh |= (uint64(inLen) << 4) | (uint64(compLen) << (10 + 4)) | (3 << 60)
		if debugEncoder {
			const mmask = (1 << 24) - 1
			n := (lh >> 4) & mmask
			if int(n&1023) != inLen {
				panic(fmt.Sprint("regensize:", int(n&1023), "!=", inLen, inBits))
			}
			if int(n>>10) != compLen {
				panic(fmt.Sprint("compsize:", int(n>>10), "!=", compLen, compBits))
			}
		}
	case compBits <= 14 && inBits <= 14:
		lh |= (2 << 2) | (uint64(inLen) << 4) | (uint64(compLen) << (14 + 4)) | (4 << 60)
		if single {
			panic("single stream used with more than 10 bits length.")
		}
	case compBits <= 18 && inBits <= 18:
		lh |= (3 << 2) | (uint64(inLen) << 4) | (uint64(compLen) << (18 + 4)) | (5 << 60)
		if single {
			panic("single stream used with more than 10 bits length.")
		}
	default:
		panic("internal error: block too big")
	}
	*h = literalsHeader(lh)
}

// appendTo will append the literals header to a byte slice.
func (h literalsHeader) appendTo(b []byte) []byte {
	size := uint8(h >> 60)
	switch size {
	case 1:
		b = append(b, uint8(h))
	case 2:
		b = append(b, uint8(h), uint8(h>>8))
	case 3:
		b = append(b, uint8(h), uint8(h>>8), uint8(h>>16))
	case 4:
		b = append(b, uint8(h), uint8(h>>8), uint8(h>>16), uint8(h>>24))
	case 5:
		b = append(b, uint8(h), uint8(h>>8), uint8(h>>16), uint8(h>>24), uint8(h>>32))
	default:
		panic(fmt.Errorf("internal error: literalsHeader has invalid size (%d)", size))
	}
	return b
}

// size returns the output size with currently set values.
func (h literalsHeader) size() int {
	return int(h >> 60)
}

func (h literalsHeader) String() string {
	return fmt.Sprintf("Type: %d, SizeFormat: %d, Size: 0x%d, Bytes:%d", literalsBlockType(h&3), (h>>2)&3, h&((1<<60)-1)>>4, h>>60)
}

// pushOffsets will push the recent offsets to the backup store.
func (b *blockEnc) pushOffsets() {
	b.prevRecentOffsets = b.recentOffsets
}

// pushOffsets will push the recent offsets to the backup store.
func (b *blockEnc) popOffsets() {
	b.recentOffsets = b.prevRecentOffsets
}

// matchOffset will adjust recent offsets and return the adjusted one,
// if it matches a previous offset.
func (b *blockEnc) matchOffset(offset, lits uint32) uint32 {
	// Check if offset is one of the recent offsets.
	// Adjusts the output offset accordingly.
	// Gives a tiny bit of compression, typically around 1%.
	if true {
		if lits > 0 {
			switch offset {
			case b.recentOffsets[0]:
				offset = 1
			case b.recentOffsets[1]:
				b.recentOffsets[1] = b.recentOffsets[0]
				b.recentOffsets[0] = offset
				offset = 2
			case b.recentOffsets[2]:
				b.recentOffsets[2] = b.recentOffsets[1]
				b.recentOffsets[1] = b.recentOffsets[0]
				b.recentOffsets[0] = offset
				offset = 3
			default:
				b.recentOffsets[2] = b.recentOffsets[1]
				b.recentOffsets[1] = b.recentOffsets[0]
				b.recentOffsets[0] = offset
				offset += 3
			}
		} else {
			switch offset {
			case b.recentOffsets[1]:
				b.recentOffsets[1] = b.recentOffsets[0]
				b.recentOffsets[0] = offset
				offset = 1
			case b.recentOffsets[2]:
				b.recentOffsets[2] = b.recentOffsets[1]
				b.recentOffsets[1] = b.recentOffsets[0]
				b.recentOffsets[0] = offset
				offset = 2
			case b.recentOffsets[0] - 1:
				b.recentOffsets[2] = b.recentOffsets[1]
				b.recentOffsets[1] = b.recentOffsets[0]
				b.recentOffsets[0] = offset
				offset = 3
			default:
				b.recentOffsets[2] = b.recentOffsets[1]
				b.recentOffsets[1] = b.recentOffsets[0]
				b.recentOffsets[0] = offset
				offset += 3
			}
		}
	} else {
		offset += 3
	}
	return offset
}

// encodeRaw can be used to set the output to a raw representation of supplied bytes.
func (b *blockEnc) encodeRaw(a []byte) {
	var bh blockHeader
	bh.setLast(b.last)
	bh.setSize(uint32(len(a)))
	bh.setType(blockTypeRaw)
	b.output = bh.appendTo(b.output[:0])
	b.output = append(b.output, a...)
	if debugEncoder {
		println("Adding RAW block, length", len(a), "last:", b.last)
	}
}

// encodeRaw can be used to set the output to a raw representation of supplied bytes.
func (b *blockEnc) encodeRawTo(dst, src []byte) []byte {
	var bh blockHeader
	bh.setLast(b.last)
	bh.setSize(uint32(len(src)))
	bh.setType(blockTypeRaw)
	dst = bh.appendTo(dst)
	dst = append(dst, src...)
	if debugEncoder {
		println("Adding RAW block, length", len(src), "last:", b.last)
	}
	return dst
}

// encodeLits can be used if the block is only litLen.
func (b *blockEnc) encodeLits(lits []byte, raw bool) error {
	var bh blockHeader
	bh.setLast(b.last)
	bh.setSize(uint32(len(lits)))

	// Don't compress extremely small blocks
	if len(lits) < 8 || (len(lits) < 32 && b.dictLitEnc == nil) || raw {
		if debugEncoder {
			println("Adding RAW block, length", len(lits), "last:", b.last)
		}
		bh.setType(blockTypeRaw)
		b.output = bh.appendTo(b.output)
		b.output = append(b.output, lits...)
		return nil
	}

	var (
		out            []byte
		reUsed, single bool
		err            error
	)
	if b.dictLitEnc != nil {
		b.litEnc.TransferCTable(b.dictLitEnc)
		b.litEnc.Reuse = huff0.ReusePolicyAllow
		b.dictLitEnc = nil
	}
	if len(lits) >= 1024 {
		// Use 4 Streams.
		out, reUsed, err = huff0.Compress4X(lits, b.litEnc)
	} else if len(lits) > 16 {
		// Use 1 stream
		single = true
		out, reUsed, err = huff0.Compress1X(lits, b.litEnc)
	} else {
		err = huff0.ErrIncompressible
	}
	if err == nil && len(out)+5 > len(lits) {
		// If we are close, we may still be worse or equal to raw.
		var lh literalsHeader
		lh.setSizes(len(out), len(lits), single)
		if len(out)+lh.size() >= len(lits) {
			err = huff0.ErrIncompressible
		}
	}
	switch err {
	case huff0.ErrIncompressible:
		if debugEncoder {
			println("Adding RAW block, length", len(lits), "last:", b.last)
		}
		bh.setType(blockTypeRaw)
		b.output = bh.appendTo(b.output)
		b.output = append(b.output, lits...)
		return nil
	case huff0.ErrUseRLE:
		if debugEncoder {
			println("Adding RLE block, length", len(lits))
		}
		bh.setType(blockTypeRLE)
		b.output = bh.appendTo(b.output)
		b.output = append(b.output, lits[0])
		return nil
	case nil:
	default:
		return err
	}
	// Compressed...
	// Now, allow reuse
	b.litEnc.Reuse = huff0.ReusePolicyAllow
	bh.setType(blockTypeCompressed)
	var lh literalsHeader
	if reUsed {
		if debugEncoder {
			println("Reused tree, compressed to", len(out))
		}
		lh.setType(literalsBlockTreeless)
	} else {
		if debugEncoder {
			println("New tree, compressed to", len(out), "tree size:", len(b.litEnc.OutTable))
		}
		lh.setType(literalsBlockCompressed)
	}
	// Set sizes
	lh.setSizes(len(out), len(lits), single)
	bh.setSize(uint32(len(out) + lh.size() + 1))

	// Write block headers.
	b.output = bh.appendTo(b.output)
	b.output = lh.appendTo(b.output)
	// Add compressed data.
	b.output = append(b.output, out...)
	// No sequences.
	b.output = append(b.output, 0)
	return nil
}

// encodeRLE will encode an RLE block.
func (b *blockEnc) encodeRLE(val byte, length uint32) {
	var bh blockHeader
	bh.setLast(b.last)
	bh.setSize(length)
	bh.setType(blockTypeRLE)
	b.output = bh.appendTo(b.output)
	b.output = append(b.output, val)
}

// fuzzFseEncoder can be used to fuzz the FSE encoder.
func fuzzFseEncoder(data []byte) int {
	if len(data) > maxSequences || len(data) < 2 {
		return 0
	}
	enc := fseEncoder{}
	hist := enc.Histogram()
	maxSym := uint8(0)
	for i, v := range data {
		v = v & 63
		data[i] = v
		hist[v]++
		if v > maxSym {
			maxSym = v
		}
	}
	if maxSym == 0 {
		// All 0
		return 0
	}
	maxCount := func(a []uint32) int {
		var max uint32
		for _, v := range a {
			if v > max {
				max = v
			}
		}
		return int(max)
	}
	cnt := maxCount(hist[:maxSym])
	if cnt == len(data) {
		// RLE
		return 0
	}
	enc.HistogramFinished(maxSym, cnt)
	err := enc.normalizeCount(len(data))
	if err != nil {
		return 0
	}
	_, err = enc.writeCount(nil)
	if err != nil {
		panic(err)
	}
	return 1
}

// encode will encode the block and append the output in b.output.
// Previous offset codes must be pushed if more blocks are expected.
func (b *blockEnc) encode(org []byte, raw, rawAllLits bool) error {
	if len(b.sequences) == 0 {
		return b.encodeLits(b.literals, rawAllLits)
	}
	if len(b.sequences) == 1 && len(org) > 0 && len(b.literals) <= 1 {
		// Check common RLE cases.
		seq := b.sequences[0]
		if seq.litLen == uint32(len(b.literals)) && seq.offset-3 == 1 {
			// Offset == 1 and 0 or 1 literals.
			b.encodeRLE(org[0], b.sequences[0].matchLen+zstdMinMatch+seq.litLen)
			return nil
		}
	}

	// We want some difference to at least account for the headers.
	saved := b.size - len(b.literals) - (b.size >> 6)
	if saved < 16 {
		if org == nil {
			return errIncompressible
		}
		b.popOffsets()
		return b.encodeLits(org, rawAllLits)
	}

	var bh blockHeader
	var lh literalsHeader
	bh.setLast(b.last)
	bh.setType(blockTypeCompressed)
	// Store offset of the block header. Needed when we know the size.
	bhOffset := len(b.output)
	b.output = bh.appendTo(b.output)

	var (
		out            []byte
		reUsed, single bool
		err            error
	)
	if b.dictLitEnc != nil {
		b.litEnc.TransferCTable(b.dictLitEnc)
		b.litEnc.Reuse = huff0.ReusePolicyAllow
		b.dictLitEnc = nil
	}
	if len(b.literals) >= 1024 && !raw {
		// Use 4 Streams.
		out, reUsed, err = huff0.Compress4X(b.literals, b.litEnc)
	} else if len(b.literals) > 16 && !raw {
		// Use 1 stream
		single = true
		out, reUsed, err = huff0.Compress1X(b.literals, b.litEnc)
	} else {
		err = huff0.ErrIncompressible
	}

	if err == nil && len(out)+5 > len(b.literals) {
		// If we are close, we may still be worse or equal to raw.
		var lh literalsHeader
		lh.setSize(len(b.literals))
		szRaw := lh.size()
		lh.setSizes(len(out), len(b.literals), single)
		szComp := lh.size()
		if len(out)+szComp >= len(b.literals)+szRaw {
			err = huff0.ErrIncompressible
		}
	}
	switch err {
	case huff0.ErrIncompressible:
		lh.setType(literalsBlockRaw)
		lh.setSize(len(b.literals))
		b.output = lh.appendTo(b.output)
		b.output = append(b.output, b.literals...)
		if debugEncoder {
			println("Adding literals RAW, length", len(b.literals))
		}
	case huff0.ErrUseRLE:
		lh.setType(literalsBlockRLE)
		lh.setSize(len(b.literals))
		b.output = lh.appendTo(b.output)
		b.output = append(b.output, b.literals[0])
		if debugEncoder {
			println("Adding literals RLE")
		}
	case nil:
		// Compressed litLen...
		if reUsed {
			if debugEncoder {
				println("reused tree")
			}
			lh.setType(literalsBlockTreeless)
		} else {
			if debugEncoder {
				println("new tree, size:", len(b.litEnc.OutTable))
			}
			lh.setType(literalsBlockCompressed)
			if debugEncoder {
				_, _, err := huff0.ReadTable(out, nil)
				if err != nil {
					panic(err)
				}
			}
		}
		lh.setSizes(len(out), len(b.literals), single)
		if debugEncoder {
			printf("Compressed %d literals to %d bytes", len(b.literals), len(out))
			println("Adding literal header:", lh)
		}
		b.output = lh.appendTo(b.output)
		b.output = append(b.output, out...)
		b.litEnc.Reuse = huff0.ReusePolicyAllow
		if debugEncoder {
			println("Adding literals compressed")
		}
	default:
		if debugEncoder {
			println("Adding literals ERROR:", err)
		}
		return err
	}
	// Sequence compression

	// Write the number of sequences
	switch {
	case len(b.sequences) < 128:
		b.output = append(b.output, uint8(len(b.sequences)))
	case len(b.sequences) < 0x7f00: // TODO: this could be wrong
		n := len(b.sequences)
		b.output = append(b.output, 128+uint8(n>>8), uint8(n))
	default:
		n := len(b.sequences) - 0x7f00
		b.output = append(b.output, 255, uint8(n), uint8(n>>8))
	}
	if debugEncoder {
		println("Encoding", len(b.sequences), "sequences")
	}
	b.genCodes()
	llEnc := b.coders.llEnc
	ofEnc := b.coders.ofEnc
	mlEnc := b.coders.mlEnc
	err = llEnc.normalizeCount(len(b.sequences))
	if err != nil {
		return err
	}
	err = ofEnc.normalizeCount(len(b.sequences))
	if err != nil {
		return err
	}
	err = mlEnc.normalizeCount(len(b.sequences))
	if err != nil {
		return err
	}

	// Choose the best compression mode for each type.
	// Will evaluate the new vs predefined and previous.
	chooseComp := func(cur, prev, preDef *fseEncoder) (*fseEncoder, seqCompMode) {
		// See if predefined/previous is better
		hist := cur.count[:cur.symbolLen]
		nSize := cur.approxSize(hist) + cur.maxHeaderSize()
		predefSize := preDef.approxSize(hist)
		prevSize := prev.approxSize(hist)

		// Add a small penalty for new encoders.
		// Don't bother with extremely small (<2 byte gains).
		nSize = nSize + (nSize+2*8*16)>>4
		switch {
		case predefSize <= prevSize && predefSize <= nSize || forcePreDef:
			if debugEncoder {
				println("Using predefined", predefSize>>3, "<=", nSize>>3)
			}
			return preDef, compModePredefined
		case prevSize <= nSize:
			if debugEncoder {
				println("Using previous", prevSize>>3, "<=", nSize>>3)
			}
			return prev, compModeRepeat
		default:
			if debugEncoder {
				println("Using new, predef", predefSize>>3, ". previous:", prevSize>>3, ">", nSize>>3, "header max:", cur.maxHeaderSize()>>3, "bytes")
				println("tl:", cur.actualTableLog, "symbolLen:", cur.symbolLen, "norm:", cur.norm[:cur.symbolLen], "hist", cur.count[:cur.symbolLen])
			}
			return cur, compModeFSE
		}
	}

	// Write compression mode
	var mode uint8
	if llEnc.useRLE {
		mode |= uint8(compModeRLE) << 6
		llEnc.setRLE(b.sequences[0].llCode)
		if debugEncoder {
			println("llEnc.useRLE")
		}
	} else {
		var m seqCompMode
		llEnc, m = chooseComp(llEnc, b.coders.llPrev, &fsePredefEnc[tableLiteralLengths])
		mode |= uint8(m) << 6
	}
	if ofEnc.useRLE {
		mode |= uint8(compModeRLE) << 4
		ofEnc.setRLE(b.sequences[0].ofCode)
		if debugEncoder {
			println("ofEnc.useRLE")
		}
	} else {
		var m seqCompMode
		ofEnc, m = chooseComp(ofEnc, b.coders.ofPrev, &fsePredefEnc[tableOffsets])
		mode |= uint8(m) << 4
	}

	if mlEnc.useRLE {
		mode |= uint8(compModeRLE) << 2
		mlEnc.setRLE(b.sequences[0].mlCode)
		if debugEncoder {
			println("mlEnc.useRLE, code: ", b.sequences[0].mlCode, "value", b.sequences[0].matchLen)
		}
	} else {
		var m seqCompMode
		mlEnc, m = chooseComp(mlEnc, b.coders.mlPrev, &fsePredefEnc[tableMatchLengths])
		mode |= uint8(m) << 2
	}
	b.output = append(b.output, mode)
	if debugEncoder {
		printf("Compression modes: 0b%b", mode)
	}
	b.output, err = llEnc.writeCount(b.output)
	if err != nil {
		return err
	}
	start := len(b.output)
	b.output, err = ofEnc.writeCount(b.output)
	if err != nil {
		return err
	}
	if false {
		println("block:", b.output[start:], "tablelog", ofEnc.actualTableLog, "maxcount:", ofEnc.maxCount)
		fmt.Printf("selected TableLog: %d, Symbol length: %d\n", ofEnc.actualTableLog, ofEnc.symbolLen)
		for i, v := range ofEnc.norm[:ofEnc.symbolLen] {
			fmt.Printf("%3d: %5d -> %4d \n", i, ofEnc.count[i], v)
		}
	}
	b.output, err = mlEnc.writeCount(b.output)
	if err != nil {
		return err
	}

	// Maybe in block?
	wr := &b.wr
	wr.reset(b.output)

	var ll, of, ml cState

	// Current sequence
	seq := len(b.sequences) - 1
	s := b.sequences[seq]
	llEnc.setBits(llBitsTable[:])
	mlEnc.setBits(mlBitsTable[:])
	ofEnc.setBits(nil)

	llTT, ofTT, mlTT := llEnc.ct.symbolTT[:256], ofEnc.ct.symbolTT[:256], mlEnc.ct.symbolTT[:256]

	// We have 3 bounds checks here (and in the loop).
	// Since we are iterating backwards it is kinda hard to avoid.
	llB, ofB, mlB := llTT[s.llCode], ofTT[s.ofCode], mlTT[s.mlCode]
	ll.init(wr, &llEnc.ct, llB)
	of.init(wr, &ofEnc.ct, ofB)
	wr.flush32()
	ml.init(wr, &mlEnc.ct, mlB)

	// Each of these lookups also generates a bounds check.
	wr.addBits32NC(s.litLen, llB.outBits)
	wr.addBits32NC(s.matchLen, mlB.outBits)
	wr.flush32()
	wr.addBits32NC(s.offset, ofB.outBits)
	if debugSequences {
		println("Encoded seq", seq, s, "codes:", s.llCode, s.mlCode, s.ofCode, "states:", ll.state, ml.state, of.state, "bits:", llB, mlB, ofB)
	}
	seq--
	// Store sequences in reverse...
	for seq >= 0 {
		s = b.sequences[seq]

		ofB := ofTT[s.ofCode]
		wr.flush32() // tablelog max is below 8 for each, so it will fill max 24 bits.
		//of.encode(ofB)
		nbBitsOut := (uint32(of.state) + ofB.deltaNbBits) >> 16
		dstState := int32(of.state>>(nbBitsOut&15)) + int32(ofB.deltaFindState)
		wr.addBits16NC(of.state, uint8(nbBitsOut))
		of.state = of.stateTable[dstState]

		// Accumulate extra bits.
		outBits := ofB.outBits & 31
		extraBits := uint64(s.offset & bitMask32[outBits])
		extraBitsN := outBits

		mlB := mlTT[s.mlCode]
		//ml.encode(mlB)
		nbBitsOut = (uint32(ml.state) + mlB.deltaNbBits) >> 16
		dstState = int32(ml.state>>(nbBitsOut&15)) + int32(mlB.deltaFindState)
		wr.addBits16NC(ml.state, uint8(nbBitsOut))
		ml.state = ml.stateTable[dstState]

		outBits = mlB.outBits & 31
		extraBits = extraBits<<outBits | uint64(s.matchLen&bitMask32[outBits])
		extraBitsN += outBits

		llB := llTT[s.llCode]
		//ll.encode(llB)
		nbBitsOut = (uint32(ll.state) + llB.deltaNbBits) >> 16
		dstState = int32(ll.state>>(nbBitsOut&15)) + int32(llB.deltaFindState)
		wr.addBits16NC(ll.state, uint8(nbBitsOut))
		ll.state = ll.stateTable[dstState]

		outBits = llB.outBits & 31
		extraBits = extraBits<<outBits | uint64(s.litLen&bitMask32[outBits])
		extraBitsN += outBits

		wr.flush32()
		wr.addBits64NC(extraBits, extraBitsN)

		if debugSequences {
			println("Encoded seq", seq, s)
		}

		seq--
	}
	ml.flush(mlEnc.actualTableLog)
	of.flush(ofEnc.actualTableLog)
	ll.flush(llEnc.actualTableLog)
	wr.close()
	b.output = wr.out

	// Maybe even add a bigger margin.
	if len(b.output)-3-bhOffset >= b.size {
		// Discard and encode as raw block.
		b.output = b.encodeRawTo(b.output[:bhOffset], org)
		b.popOffsets()
		b.litEnc.Reuse = huff0.ReusePolicyNone
		return nil
	}

	// Size is output minus block header.
	bh.setSize(uint32(len(b.output)-bhOffset) - 3)
	if debugEncoder {
		println("Rewriting block header", bh)
	}
	_ = bh.appendTo(b.output[bhOffset:bhOffset])
	b.coders.setPrev(llEnc, mlEnc, ofEnc)
	return nil
}

var errIncompressible = errors.New("incompressible")

func (b *blockEnc) genCodes() {
	if len(b.sequences) == 0 {
		// nothing to do
		return
	}
	if len(b.sequences) > math.MaxUint16 {
		panic("can only encode up to 64K sequences")
	}
	// No bounds checks after here:
	llH := b.coders.llEnc.Histogram()
	ofH := b.coders.ofEnc.Histogram()
	mlH := b.coders.mlEnc.Histogram()
	for i := range llH {
		llH[i] = 0
	}
	for i := range ofH {
		ofH[i] = 0
	}
	for i := range mlH {
		mlH[i] = 0
	}

	var llMax, ofMax, mlMax uint8
	for i := range b.sequences {
		seq := &b.sequences[i]
		v := llCode(seq.litLen)
		seq.llCode = v
		llH[v]++
		if v > llMax {
			llMax = v
		}

		v = ofCode(seq.offset)
		seq.ofCode = v
		ofH[v]++
		if v > ofMax {
			ofMax = v
		}

		v = mlCode(seq.matchLen)
		seq.mlCode = v
		mlH[v]++
		if v > mlMax {
			mlMax = v
			if debugAsserts && mlMax > maxMatchLengthSymbol {
				panic(fmt.Errorf("mlMax > maxMatchLengthSymbol (%d), matchlen: %d", mlMax, seq.matchLen))
			}
		}
	}
	maxCount := func(a []uint32) int {
		var max uint32
		for _, v := range a {
			if v > max {
				max = v
			}
		}
		return int(max)
	}
	if debugAsserts && mlMax > maxMatchLengthSymbol {
		panic(fmt.Errorf("mlMax > maxMatchLengthSymbol (%d)", mlMax))
	}
	if debugAsserts && ofMax > maxOffsetBits {
		panic(fmt.Errorf("ofMax > maxOffsetBits (%d)", ofMax))
	}
	if debugAsserts && llMax > maxLiteralLengthSymbol {
		panic(fmt.Errorf("llMax > maxLiteralLengthSymbol (%d)", llMax))
	}

	b.coders.mlEnc.HistogramFinished(mlMax, maxCount(mlH[:mlMax+1]))
	b.coders.ofEnc.HistogramFinished(ofMax, maxCount(ofH[:ofMax+1]))
	b.coders.llEnc.HistogramFinished(llMax, maxCount(llH[:llMax+1]))
}
