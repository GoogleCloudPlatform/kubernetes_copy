package huff0

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/fse"
)

type dTable struct {
	single []dEntrySingle
}

// single-symbols decoding
type dEntrySingle struct {
	entry uint16
}

// Uses special code for all tables that are < 8 bits.
const use8BitTables = true

// ReadTable will read a table from the input.
// The size of the input may be larger than the table definition.
// Any content remaining after the table definition will be returned.
// If no Scratch is provided a new one is allocated.
// The returned Scratch can be used for encoding or decoding input using this table.
func ReadTable(in []byte, s *Scratch) (s2 *Scratch, remain []byte, err error) {
	s, err = s.prepare(nil)
	if err != nil {
		return s, nil, err
	}
	if len(in) <= 1 {
		return s, nil, errors.New("input too small for table")
	}
	iSize := in[0]
	in = in[1:]
	if iSize >= 128 {
		// Uncompressed
		oSize := iSize - 127
		iSize = (oSize + 1) / 2
		if int(iSize) > len(in) {
			return s, nil, errors.New("input too small for table")
		}
		for n := uint8(0); n < oSize; n += 2 {
			v := in[n/2]
			s.huffWeight[n] = v >> 4
			s.huffWeight[n+1] = v & 15
		}
		s.symbolLen = uint16(oSize)
		in = in[iSize:]
	} else {
		if len(in) < int(iSize) {
			return s, nil, fmt.Errorf("input too small for table, want %d bytes, have %d", iSize, len(in))
		}
		// FSE compressed weights
		s.fse.DecompressLimit = 255
		hw := s.huffWeight[:]
		s.fse.Out = hw
		b, err := fse.Decompress(in[:iSize], s.fse)
		s.fse.Out = nil
		if err != nil {
			return s, nil, fmt.Errorf("fse decompress returned: %w", err)
		}
		if len(b) > 255 {
			return s, nil, errors.New("corrupt input: output table too large")
		}
		s.symbolLen = uint16(len(b))
		in = in[iSize:]
	}

	// collect weight stats
	var rankStats [16]uint32
	weightTotal := uint32(0)
	for _, v := range s.huffWeight[:s.symbolLen] {
		if v > tableLogMax {
			return s, nil, errors.New("corrupt input: weight too large")
		}
		v2 := v & 15
		rankStats[v2]++
		// (1 << (v2-1)) is slower since the compiler cannot prove that v2 isn't 0.
		weightTotal += (1 << v2) >> 1
	}
	if weightTotal == 0 {
		return s, nil, errors.New("corrupt input: weights zero")
	}

	// get last non-null symbol weight (implied, total must be 2^n)
	{
		tableLog := highBit32(weightTotal) + 1
		if tableLog > tableLogMax {
			return s, nil, errors.New("corrupt input: tableLog too big")
		}
		s.actualTableLog = uint8(tableLog)
		// determine last weight
		{
			total := uint32(1) << tableLog
			rest := total - weightTotal
			verif := uint32(1) << highBit32(rest)
			lastWeight := highBit32(rest) + 1
			if verif != rest {
				// last value must be a clean power of 2
				return s, nil, errors.New("corrupt input: last value not power of two")
			}
			s.huffWeight[s.symbolLen] = uint8(lastWeight)
			s.symbolLen++
			rankStats[lastWeight]++
		}
	}

	if (rankStats[1] < 2) || (rankStats[1]&1 != 0) {
		// by construction : at least 2 elts of rank 1, must be even
		return s, nil, errors.New("corrupt input: min elt size, even check failed ")
	}

	// TODO: Choose between single/double symbol decoding

	// Calculate starting value for each rank
	{
		var nextRankStart uint32
		for n := uint8(1); n < s.actualTableLog+1; n++ {
			current := nextRankStart
			nextRankStart += rankStats[n] << (n - 1)
			rankStats[n] = current
		}
	}

	// fill DTable (always full size)
	tSize := 1 << tableLogMax
	if len(s.dt.single) != tSize {
		s.dt.single = make([]dEntrySingle, tSize)
	}
	cTable := s.prevTable
	if cap(cTable) < maxSymbolValue+1 {
		cTable = make([]cTableEntry, 0, maxSymbolValue+1)
	}
	cTable = cTable[:maxSymbolValue+1]
	s.prevTable = cTable[:s.symbolLen]
	s.prevTableLog = s.actualTableLog

	for n, w := range s.huffWeight[:s.symbolLen] {
		if w == 0 {
			cTable[n] = cTableEntry{
				val:   0,
				nBits: 0,
			}
			continue
		}
		length := (uint32(1) << w) >> 1
		d := dEntrySingle{
			entry: uint16(s.actualTableLog+1-w) | (uint16(n) << 8),
		}

		rank := &rankStats[w]
		cTable[n] = cTableEntry{
			val:   uint16(*rank >> (w - 1)),
			nBits: uint8(d.entry),
		}

		single := s.dt.single[*rank : *rank+length]
		for i := range single {
			single[i] = d
		}
		*rank += length
	}

	return s, in, nil
}

// Decompress1X will decompress a 1X encoded stream.
// The length of the supplied input must match the end of a block exactly.
// Before this is called, the table must be initialized with ReadTable unless
// the encoder re-used the table.
// deprecated: Use the stateless Decoder() to get a concurrent version.
func (s *Scratch) Decompress1X(in []byte) (out []byte, err error) {
	if cap(s.Out) < s.MaxDecodedSize {
		s.Out = make([]byte, s.MaxDecodedSize)
	}
	s.Out = s.Out[:0:s.MaxDecodedSize]
	s.Out, err = s.Decoder().Decompress1X(s.Out, in)
	return s.Out, err
}

// Decompress4X will decompress a 4X encoded stream.
// Before this is called, the table must be initialized with ReadTable unless
// the encoder re-used the table.
// The length of the supplied input must match the end of a block exactly.
// The destination size of the uncompressed data must be known and provided.
// deprecated: Use the stateless Decoder() to get a concurrent version.
func (s *Scratch) Decompress4X(in []byte, dstSize int) (out []byte, err error) {
	if dstSize > s.MaxDecodedSize {
		return nil, ErrMaxDecodedSizeExceeded
	}
	if cap(s.Out) < dstSize {
		s.Out = make([]byte, s.MaxDecodedSize)
	}
	s.Out = s.Out[:0:dstSize]
	s.Out, err = s.Decoder().Decompress4X(s.Out, in)
	return s.Out, err
}

// Decoder will return a stateless decoder that can be used by multiple
// decompressors concurrently.
// Before this is called, the table must be initialized with ReadTable.
// The Decoder is still linked to the scratch buffer so that cannot be reused.
// However, it is safe to discard the scratch.
func (s *Scratch) Decoder() *Decoder {
	return &Decoder{
		dt:             s.dt,
		actualTableLog: s.actualTableLog,
		bufs:           &s.decPool,
	}
}

// Decoder provides stateless decoding.
type Decoder struct {
	dt             dTable
	actualTableLog uint8
	bufs           *sync.Pool
}

func (d *Decoder) buffer() *[4][256]byte {
	buf, ok := d.bufs.Get().(*[4][256]byte)
	if ok {
		return buf
	}
	return &[4][256]byte{}
}

// decompress1X8Bit will decompress a 1X encoded stream with tablelog <= 8.
// The cap of the output buffer will be the maximum decompressed size.
// The length of the supplied input must match the end of a block exactly.
func (d *Decoder) decompress1X8Bit(dst, src []byte) ([]byte, error) {
	if d.actualTableLog == 8 {
		return d.decompress1X8BitExactly(dst, src)
	}
	var br bitReaderBytes
	err := br.init(src)
	if err != nil {
		return dst, err
	}
	maxDecodedSize := cap(dst)
	dst = dst[:0]

	// Avoid bounds check by always having full sized table.
	dt := d.dt.single[:256]

	// Use temp table to avoid bound checks/append penalty.
	bufs := d.buffer()
	buf := &bufs[0]
	var off uint8

	switch d.actualTableLog {
	case 8:
		const shift = 0
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					br.close()
					d.bufs.Put(bufs)
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	case 7:
		const shift = 8 - 7
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					br.close()
					d.bufs.Put(bufs)
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	case 6:
		const shift = 8 - 6
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					d.bufs.Put(bufs)
					br.close()
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	case 5:
		const shift = 8 - 5
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					d.bufs.Put(bufs)
					br.close()
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	case 4:
		const shift = 8 - 4
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					d.bufs.Put(bufs)
					br.close()
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	case 3:
		const shift = 8 - 3
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					d.bufs.Put(bufs)
					br.close()
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	case 2:
		const shift = 8 - 2
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					d.bufs.Put(bufs)
					br.close()
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	case 1:
		const shift = 8 - 1
		for br.off >= 4 {
			br.fillFast()
			v := dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+0] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+1] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+2] = uint8(v.entry >> 8)

			v = dt[uint8(br.value>>(56+shift))]
			br.advance(uint8(v.entry))
			buf[off+3] = uint8(v.entry >> 8)

			off += 4
			if off == 0 {
				if len(dst)+256 > maxDecodedSize {
					d.bufs.Put(bufs)
					br.close()
					return nil, ErrMaxDecodedSizeExceeded
				}
				dst = append(dst, buf[:]...)
			}
		}
	default:
		d.bufs.Put(bufs)
		return nil, fmt.Errorf("invalid tablelog: %d", d.actualTableLog)
	}

	if len(dst)+int(off) > maxDecodedSize {
		d.bufs.Put(bufs)
		br.close()
		return nil, ErrMaxDecodedSizeExceeded
	}
	dst = append(dst, buf[:off]...)

	// br < 4, so uint8 is fine
	bitsLeft := int8(uint8(br.off)*8 + (64 - br.bitsRead))
	shift := (8 - d.actualTableLog) & 7

	for bitsLeft > 0 {
		if br.bitsRead >= 64-8 {
			for br.off > 0 {
				br.value |= uint64(br.in[br.off-1]) << (br.bitsRead - 8)
				br.bitsRead -= 8
				br.off--
			}
		}
		if len(dst) >= maxDecodedSize {
			br.close()
			d.bufs.Put(bufs)
			return nil, ErrMaxDecodedSizeExceeded
		}
		v := dt[br.peekByteFast()>>shift]
		nBits := uint8(v.entry)
		br.advance(nBits)
		bitsLeft -= int8(nBits)
		dst = append(dst, uint8(v.entry>>8))
	}
	d.bufs.Put(bufs)
	return dst, br.close()
}

// decompress1X8Bit will decompress a 1X encoded stream with tablelog <= 8.
// The cap of the output buffer will be the maximum decompressed size.
// The length of the supplied input must match the end of a block exactly.
func (d *Decoder) decompress1X8BitExactly(dst, src []byte) ([]byte, error) {
	var br bitReaderBytes
	err := br.init(src)
	if err != nil {
		return dst, err
	}
	maxDecodedSize := cap(dst)
	dst = dst[:0]

	// Avoid bounds check by always having full sized table.
	dt := d.dt.single[:256]

	// Use temp table to avoid bound checks/append penalty.
	bufs := d.buffer()
	buf := &bufs[0]
	var off uint8

	const shift = 56

	//fmt.Printf("mask: %b, tl:%d\n", mask, d.actualTableLog)
	for br.off >= 4 {
		br.fillFast()
		v := dt[uint8(br.value>>shift)]
		br.advance(uint8(v.entry))
		buf[off+0] = uint8(v.entry >> 8)

		v = dt[uint8(br.value>>shift)]
		br.advance(uint8(v.entry))
		buf[off+1] = uint8(v.entry >> 8)

		v = dt[uint8(br.value>>shift)]
		br.advance(uint8(v.entry))
		buf[off+2] = uint8(v.entry >> 8)

		v = dt[uint8(br.value>>shift)]
		br.advance(uint8(v.entry))
		buf[off+3] = uint8(v.entry >> 8)

		off += 4
		if off == 0 {
			if len(dst)+256 > maxDecodedSize {
				d.bufs.Put(bufs)
				br.close()
				return nil, ErrMaxDecodedSizeExceeded
			}
			dst = append(dst, buf[:]...)
		}
	}

	if len(dst)+int(off) > maxDecodedSize {
		d.bufs.Put(bufs)
		br.close()
		return nil, ErrMaxDecodedSizeExceeded
	}
	dst = append(dst, buf[:off]...)

	// br < 4, so uint8 is fine
	bitsLeft := int8(uint8(br.off)*8 + (64 - br.bitsRead))
	for bitsLeft > 0 {
		if br.bitsRead >= 64-8 {
			for br.off > 0 {
				br.value |= uint64(br.in[br.off-1]) << (br.bitsRead - 8)
				br.bitsRead -= 8
				br.off--
			}
		}
		if len(dst) >= maxDecodedSize {
			d.bufs.Put(bufs)
			br.close()
			return nil, ErrMaxDecodedSizeExceeded
		}
		v := dt[br.peekByteFast()]
		nBits := uint8(v.entry)
		br.advance(nBits)
		bitsLeft -= int8(nBits)
		dst = append(dst, uint8(v.entry>>8))
	}
	d.bufs.Put(bufs)
	return dst, br.close()
}

// Decompress4X will decompress a 4X encoded stream.
// The length of the supplied input must match the end of a block exactly.
// The *capacity* of the dst slice must match the destination size of
// the uncompressed data exactly.
func (d *Decoder) decompress4X8bit(dst, src []byte) ([]byte, error) {
	if d.actualTableLog == 8 {
		return d.decompress4X8bitExactly(dst, src)
	}

	var br [4]bitReaderBytes
	start := 6
	for i := 0; i < 3; i++ {
		length := int(src[i*2]) | (int(src[i*2+1]) << 8)
		if start+length >= len(src) {
			return nil, errors.New("truncated input (or invalid offset)")
		}
		err := br[i].init(src[start : start+length])
		if err != nil {
			return nil, err
		}
		start += length
	}
	err := br[3].init(src[start:])
	if err != nil {
		return nil, err
	}

	// destination, offset to match first output
	dstSize := cap(dst)
	dst = dst[:dstSize]
	out := dst
	dstEvery := (dstSize + 3) / 4

	shift := (56 + (8 - d.actualTableLog)) & 63

	const tlSize = 1 << 8
	single := d.dt.single[:tlSize]

	// Use temp table to avoid bound checks/append penalty.
	buf := d.buffer()
	var off uint8
	var decoded int

	// Decode 4 values from each decoder/loop.
	const bufoff = 256
	for {
		if br[0].off < 4 || br[1].off < 4 || br[2].off < 4 || br[3].off < 4 {
			break
		}

		{
			// Interleave 2 decodes.
			const stream = 0
			const stream2 = 1
			br1 := &br[stream]
			br2 := &br[stream2]
			br1.fillFast()
			br2.fillFast()

			v := single[uint8(br1.value>>shift)].entry
			v2 := single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off] = uint8(v >> 8)
			buf[stream2][off] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+1] = uint8(v >> 8)
			buf[stream2][off+1] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+2] = uint8(v >> 8)
			buf[stream2][off+2] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+3] = uint8(v >> 8)
			buf[stream2][off+3] = uint8(v2 >> 8)
		}

		{
			const stream = 2
			const stream2 = 3
			br1 := &br[stream]
			br2 := &br[stream2]
			br1.fillFast()
			br2.fillFast()

			v := single[uint8(br1.value>>shift)].entry
			v2 := single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off] = uint8(v >> 8)
			buf[stream2][off] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+1] = uint8(v >> 8)
			buf[stream2][off+1] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+2] = uint8(v >> 8)
			buf[stream2][off+2] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+3] = uint8(v >> 8)
			buf[stream2][off+3] = uint8(v2 >> 8)
		}

		off += 4

		if off == 0 {
			if bufoff > dstEvery {
				d.bufs.Put(buf)
				return nil, errors.New("corruption detected: stream overrun 1")
			}
			// There must at least be 3 buffers left.
			if len(out)-bufoff < dstEvery*3 {
				d.bufs.Put(buf)
				return nil, errors.New("corruption detected: stream overrun 2")
			}
			//copy(out, buf[0][:])
			//copy(out[dstEvery:], buf[1][:])
			//copy(out[dstEvery*2:], buf[2][:])
			*(*[bufoff]byte)(out) = buf[0]
			*(*[bufoff]byte)(out[dstEvery:]) = buf[1]
			*(*[bufoff]byte)(out[dstEvery*2:]) = buf[2]
			*(*[bufoff]byte)(out[dstEvery*3:]) = buf[3]
			out = out[bufoff:]
			decoded += bufoff * 4
		}
	}
	if off > 0 {
		ioff := int(off)
		if len(out) < dstEvery*3+ioff {
			d.bufs.Put(buf)
			return nil, errors.New("corruption detected: stream overrun 3")
		}
		copy(out, buf[0][:off])
		copy(out[dstEvery:], buf[1][:off])
		copy(out[dstEvery*2:], buf[2][:off])
		copy(out[dstEvery*3:], buf[3][:off])
		decoded += int(off) * 4
		out = out[off:]
	}

	// Decode remaining.
	// Decode remaining.
	remainBytes := dstEvery - (decoded / 4)
	for i := range br {
		offset := dstEvery * i
		endsAt := offset + remainBytes
		if endsAt > len(out) {
			endsAt = len(out)
		}
		br := &br[i]
		bitsLeft := br.remaining()
		for bitsLeft > 0 {
			if br.finished() {
				d.bufs.Put(buf)
				return nil, io.ErrUnexpectedEOF
			}
			if br.bitsRead >= 56 {
				if br.off >= 4 {
					v := br.in[br.off-4:]
					v = v[:4]
					low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
					br.value |= uint64(low) << (br.bitsRead - 32)
					br.bitsRead -= 32
					br.off -= 4
				} else {
					for br.off > 0 {
						br.value |= uint64(br.in[br.off-1]) << (br.bitsRead - 8)
						br.bitsRead -= 8
						br.off--
					}
				}
			}
			// end inline...
			if offset >= endsAt {
				d.bufs.Put(buf)
				return nil, errors.New("corruption detected: stream overrun 4")
			}

			// Read value and increment offset.
			v := single[uint8(br.value>>shift)].entry
			nBits := uint8(v)
			br.advance(nBits)
			bitsLeft -= uint(nBits)
			out[offset] = uint8(v >> 8)
			offset++
		}
		if offset != endsAt {
			d.bufs.Put(buf)
			return nil, fmt.Errorf("corruption detected: short output block %d, end %d != %d", i, offset, endsAt)
		}
		decoded += offset - dstEvery*i
		err = br.close()
		if err != nil {
			d.bufs.Put(buf)
			return nil, err
		}
	}
	d.bufs.Put(buf)
	if dstSize != decoded {
		return nil, errors.New("corruption detected: short output block")
	}
	return dst, nil
}

// Decompress4X will decompress a 4X encoded stream.
// The length of the supplied input must match the end of a block exactly.
// The *capacity* of the dst slice must match the destination size of
// the uncompressed data exactly.
func (d *Decoder) decompress4X8bitExactly(dst, src []byte) ([]byte, error) {
	var br [4]bitReaderBytes
	start := 6
	for i := 0; i < 3; i++ {
		length := int(src[i*2]) | (int(src[i*2+1]) << 8)
		if start+length >= len(src) {
			return nil, errors.New("truncated input (or invalid offset)")
		}
		err := br[i].init(src[start : start+length])
		if err != nil {
			return nil, err
		}
		start += length
	}
	err := br[3].init(src[start:])
	if err != nil {
		return nil, err
	}

	// destination, offset to match first output
	dstSize := cap(dst)
	dst = dst[:dstSize]
	out := dst
	dstEvery := (dstSize + 3) / 4

	const shift = 56
	const tlSize = 1 << 8
	single := d.dt.single[:tlSize]

	// Use temp table to avoid bound checks/append penalty.
	buf := d.buffer()
	var off uint8
	var decoded int

	// Decode 4 values from each decoder/loop.
	const bufoff = 256
	for {
		if br[0].off < 4 || br[1].off < 4 || br[2].off < 4 || br[3].off < 4 {
			break
		}

		{
			// Interleave 2 decodes.
			const stream = 0
			const stream2 = 1
			br1 := &br[stream]
			br2 := &br[stream2]
			br1.fillFast()
			br2.fillFast()

			v := single[uint8(br1.value>>shift)].entry
			v2 := single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off] = uint8(v >> 8)
			buf[stream2][off] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+1] = uint8(v >> 8)
			buf[stream2][off+1] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+2] = uint8(v >> 8)
			buf[stream2][off+2] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+3] = uint8(v >> 8)
			buf[stream2][off+3] = uint8(v2 >> 8)
		}

		{
			const stream = 2
			const stream2 = 3
			br1 := &br[stream]
			br2 := &br[stream2]
			br1.fillFast()
			br2.fillFast()

			v := single[uint8(br1.value>>shift)].entry
			v2 := single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off] = uint8(v >> 8)
			buf[stream2][off] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+1] = uint8(v >> 8)
			buf[stream2][off+1] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+2] = uint8(v >> 8)
			buf[stream2][off+2] = uint8(v2 >> 8)

			v = single[uint8(br1.value>>shift)].entry
			v2 = single[uint8(br2.value>>shift)].entry
			br1.bitsRead += uint8(v)
			br1.value <<= v & 63
			br2.bitsRead += uint8(v2)
			br2.value <<= v2 & 63
			buf[stream][off+3] = uint8(v >> 8)
			buf[stream2][off+3] = uint8(v2 >> 8)
		}

		off += 4

		if off == 0 {
			if bufoff > dstEvery {
				d.bufs.Put(buf)
				return nil, errors.New("corruption detected: stream overrun 1")
			}
			// There must at least be 3 buffers left.
			if len(out)-bufoff < dstEvery*3 {
				d.bufs.Put(buf)
				return nil, errors.New("corruption detected: stream overrun 2")
			}

			//copy(out, buf[0][:])
			//copy(out[dstEvery:], buf[1][:])
			//copy(out[dstEvery*2:], buf[2][:])
			// copy(out[dstEvery*3:], buf[3][:])
			*(*[bufoff]byte)(out) = buf[0]
			*(*[bufoff]byte)(out[dstEvery:]) = buf[1]
			*(*[bufoff]byte)(out[dstEvery*2:]) = buf[2]
			*(*[bufoff]byte)(out[dstEvery*3:]) = buf[3]
			out = out[bufoff:]
			decoded += bufoff * 4
		}
	}
	if off > 0 {
		ioff := int(off)
		if len(out) < dstEvery*3+ioff {
			return nil, errors.New("corruption detected: stream overrun 3")
		}
		copy(out, buf[0][:off])
		copy(out[dstEvery:], buf[1][:off])
		copy(out[dstEvery*2:], buf[2][:off])
		copy(out[dstEvery*3:], buf[3][:off])
		decoded += int(off) * 4
		out = out[off:]
	}

	// Decode remaining.
	remainBytes := dstEvery - (decoded / 4)
	for i := range br {
		offset := dstEvery * i
		endsAt := offset + remainBytes
		if endsAt > len(out) {
			endsAt = len(out)
		}
		br := &br[i]
		bitsLeft := br.remaining()
		for bitsLeft > 0 {
			if br.finished() {
				d.bufs.Put(buf)
				return nil, io.ErrUnexpectedEOF
			}
			if br.bitsRead >= 56 {
				if br.off >= 4 {
					v := br.in[br.off-4:]
					v = v[:4]
					low := (uint32(v[0])) | (uint32(v[1]) << 8) | (uint32(v[2]) << 16) | (uint32(v[3]) << 24)
					br.value |= uint64(low) << (br.bitsRead - 32)
					br.bitsRead -= 32
					br.off -= 4
				} else {
					for br.off > 0 {
						br.value |= uint64(br.in[br.off-1]) << (br.bitsRead - 8)
						br.bitsRead -= 8
						br.off--
					}
				}
			}
			// end inline...
			if offset >= endsAt {
				d.bufs.Put(buf)
				return nil, errors.New("corruption detected: stream overrun 4")
			}

			// Read value and increment offset.
			v := single[br.peekByteFast()].entry
			nBits := uint8(v)
			br.advance(nBits)
			bitsLeft -= uint(nBits)
			out[offset] = uint8(v >> 8)
			offset++
		}
		if offset != endsAt {
			d.bufs.Put(buf)
			return nil, fmt.Errorf("corruption detected: short output block %d, end %d != %d", i, offset, endsAt)
		}

		decoded += offset - dstEvery*i
		err = br.close()
		if err != nil {
			d.bufs.Put(buf)
			return nil, err
		}
	}
	d.bufs.Put(buf)
	if dstSize != decoded {
		return nil, errors.New("corruption detected: short output block")
	}
	return dst, nil
}

// matches will compare a decoding table to a coding table.
// Errors are written to the writer.
// Nothing will be written if table is ok.
func (s *Scratch) matches(ct cTable, w io.Writer) {
	if s == nil || len(s.dt.single) == 0 {
		return
	}
	dt := s.dt.single[:1<<s.actualTableLog]
	tablelog := s.actualTableLog
	ok := 0
	broken := 0
	for sym, enc := range ct {
		errs := 0
		broken++
		if enc.nBits == 0 {
			for _, dec := range dt {
				if uint8(dec.entry>>8) == byte(sym) {
					fmt.Fprintf(w, "symbol %x has decoder, but no encoder\n", sym)
					errs++
					break
				}
			}
			if errs == 0 {
				broken--
			}
			continue
		}
		// Unused bits in input
		ub := tablelog - enc.nBits
		top := enc.val << ub
		// decoder looks at top bits.
		dec := dt[top]
		if uint8(dec.entry) != enc.nBits {
			fmt.Fprintf(w, "symbol 0x%x bit size mismatch (enc: %d, dec:%d).\n", sym, enc.nBits, uint8(dec.entry))
			errs++
		}
		if uint8(dec.entry>>8) != uint8(sym) {
			fmt.Fprintf(w, "symbol 0x%x decoder output mismatch (enc: %d, dec:%d).\n", sym, sym, uint8(dec.entry>>8))
			errs++
		}
		if errs > 0 {
			fmt.Fprintf(w, "%d errros in base, stopping\n", errs)
			continue
		}
		// Ensure that all combinations are covered.
		for i := uint16(0); i < (1 << ub); i++ {
			vval := top | i
			dec := dt[vval]
			if uint8(dec.entry) != enc.nBits {
				fmt.Fprintf(w, "symbol 0x%x bit size mismatch (enc: %d, dec:%d).\n", vval, enc.nBits, uint8(dec.entry))
				errs++
			}
			if uint8(dec.entry>>8) != uint8(sym) {
				fmt.Fprintf(w, "symbol 0x%x decoder output mismatch (enc: %d, dec:%d).\n", vval, sym, uint8(dec.entry>>8))
				errs++
			}
			if errs > 20 {
				fmt.Fprintf(w, "%d errros, stopping\n", errs)
				break
			}
		}
		if errs == 0 {
			ok++
			broken--
		}
	}
	if broken > 0 {
		fmt.Fprintf(w, "%d broken, %d ok\n", broken, ok)
	}
}
