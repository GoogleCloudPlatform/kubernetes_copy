// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"errors"
	"fmt"
	"math"
)

const (
	// For encoding we only support up to
	maxEncTableLog    = 8
	maxEncTablesize   = 1 << maxTableLog
	maxEncTableMask   = (1 << maxTableLog) - 1
	minEncTablelog    = 5
	maxEncSymbolValue = maxMatchLengthSymbol
)

// Scratch provides temporary storage for compression and decompression.
type fseEncoder struct {
	symbolLen      uint16 // Length of active part of the symbol table.
	actualTableLog uint8  // Selected tablelog.
	ct             cTable // Compression tables.
	maxCount       int    // count of the most probable symbol
	zeroBits       bool   // no bits has prob > 50%.
	clearCount     bool   // clear count
	useRLE         bool   // This encoder is for RLE
	preDefined     bool   // This encoder is predefined.
	reUsed         bool   // Set to know when the encoder has been reused.
	rleVal         uint8  // RLE Symbol
	maxBits        uint8  // Maximum output bits after transform.

	// TODO: Technically zstd should be fine with 64 bytes.
	count [256]uint32
	norm  [256]int16
}

// cTable contains tables used for compression.
type cTable struct {
	tableSymbol []byte
	stateTable  []uint16
	symbolTT    []symbolTransform
}

// symbolTransform contains the state transform for a symbol.
type symbolTransform struct {
	deltaNbBits    uint32
	deltaFindState int16
	outBits        uint8
}

// String prints values as a human readable string.
func (s symbolTransform) String() string {
	return fmt.Sprintf("{deltabits: %08x, findstate:%d outbits:%d}", s.deltaNbBits, s.deltaFindState, s.outBits)
}

// Histogram allows to populate the histogram and skip that step in the compression,
// It otherwise allows to inspect the histogram when compression is done.
// To indicate that you have populated the histogram call HistogramFinished
// with the value of the highest populated symbol, as well as the number of entries
// in the most populated entry. These are accepted at face value.
func (s *fseEncoder) Histogram() *[256]uint32 {
	return &s.count
}

// HistogramFinished can be called to indicate that the histogram has been populated.
// maxSymbol is the index of the highest set symbol of the next data segment.
// maxCount is the number of entries in the most populated entry.
// These are accepted at face value.
func (s *fseEncoder) HistogramFinished(maxSymbol uint8, maxCount int) {
	s.maxCount = maxCount
	s.symbolLen = uint16(maxSymbol) + 1
	s.clearCount = maxCount != 0
}

// allocCtable will allocate tables needed for compression.
// If existing tables a re big enough, they are simply re-used.
func (s *fseEncoder) allocCtable() {
	tableSize := 1 << s.actualTableLog
	// get tableSymbol that is big enough.
	if cap(s.ct.tableSymbol) < tableSize {
		s.ct.tableSymbol = make([]byte, tableSize)
	}
	s.ct.tableSymbol = s.ct.tableSymbol[:tableSize]

	ctSize := tableSize
	if cap(s.ct.stateTable) < ctSize {
		s.ct.stateTable = make([]uint16, ctSize)
	}
	s.ct.stateTable = s.ct.stateTable[:ctSize]

	if cap(s.ct.symbolTT) < 256 {
		s.ct.symbolTT = make([]symbolTransform, 256)
	}
	s.ct.symbolTT = s.ct.symbolTT[:256]
}

// buildCTable will populate the compression table so it is ready to be used.
func (s *fseEncoder) buildCTable() error {
	tableSize := uint32(1 << s.actualTableLog)
	highThreshold := tableSize - 1
	var cumul [256]int16

	s.allocCtable()
	tableSymbol := s.ct.tableSymbol[:tableSize]
	// symbol start positions
	{
		cumul[0] = 0
		for ui, v := range s.norm[:s.symbolLen-1] {
			u := byte(ui) // one less than reference
			if v == -1 {
				// Low proba symbol
				cumul[u+1] = cumul[u] + 1
				tableSymbol[highThreshold] = u
				highThreshold--
			} else {
				cumul[u+1] = cumul[u] + v
			}
		}
		// Encode last symbol separately to avoid overflowing u
		u := int(s.symbolLen - 1)
		v := s.norm[s.symbolLen-1]
		if v == -1 {
			// Low proba symbol
			cumul[u+1] = cumul[u] + 1
			tableSymbol[highThreshold] = byte(u)
			highThreshold--
		} else {
			cumul[u+1] = cumul[u] + v
		}
		if uint32(cumul[s.symbolLen]) != tableSize {
			return fmt.Errorf("internal error: expected cumul[s.symbolLen] (%d) == tableSize (%d)", cumul[s.symbolLen], tableSize)
		}
		cumul[s.symbolLen] = int16(tableSize) + 1
	}
	// Spread symbols
	s.zeroBits = false
	{
		step := tableStep(tableSize)
		tableMask := tableSize - 1
		var position uint32
		// if any symbol > largeLimit, we may have 0 bits output.
		largeLimit := int16(1 << (s.actualTableLog - 1))
		for ui, v := range s.norm[:s.symbolLen] {
			symbol := byte(ui)
			if v > largeLimit {
				s.zeroBits = true
			}
			for nbOccurrences := int16(0); nbOccurrences < v; nbOccurrences++ {
				tableSymbol[position] = symbol
				position = (position + step) & tableMask
				for position > highThreshold {
					position = (position + step) & tableMask
				} /* Low proba area */
			}
		}

		// Check if we have gone through all positions
		if position != 0 {
			return errors.New("position!=0")
		}
	}

	// Build table
	table := s.ct.stateTable
	{
		tsi := int(tableSize)
		for u, v := range tableSymbol {
			// TableU16 : sorted by symbol order; gives next state value
			table[cumul[v]] = uint16(tsi + u)
			cumul[v]++
		}
	}

	// Build Symbol Transformation Table
	{
		total := int16(0)
		symbolTT := s.ct.symbolTT[:s.symbolLen]
		tableLog := s.actualTableLog
		tl := (uint32(tableLog) << 16) - (1 << tableLog)
		for i, v := range s.norm[:s.symbolLen] {
			switch v {
			case 0:
			case -1, 1:
				symbolTT[i].deltaNbBits = tl
				symbolTT[i].deltaFindState = total - 1
				total++
			default:
				maxBitsOut := uint32(tableLog) - highBit(uint32(v-1))
				minStatePlus := uint32(v) << maxBitsOut
				symbolTT[i].deltaNbBits = (maxBitsOut << 16) - minStatePlus
				symbolTT[i].deltaFindState = total - v
				total += v
			}
		}
		if total != int16(tableSize) {
			return fmt.Errorf("total mismatch %d (got) != %d (want)", total, tableSize)
		}
	}
	return nil
}

var rtbTable = [...]uint32{0, 473195, 504333, 520860, 550000, 700000, 750000, 830000}

func (s *fseEncoder) setRLE(val byte) {
	s.allocCtable()
	s.actualTableLog = 0
	s.ct.stateTable = s.ct.stateTable[:1]
	s.ct.symbolTT[val] = symbolTransform{
		deltaFindState: 0,
		deltaNbBits:    0,
	}
	if debugEncoder {
		println("setRLE: val", val, "symbolTT", s.ct.symbolTT[val])
	}
	s.rleVal = val
	s.useRLE = true
}

// setBits will set output bits for the transform.
// if nil is provided, the number of bits is equal to the index.
func (s *fseEncoder) setBits(transform []byte) {
	if s.reUsed || s.preDefined {
		return
	}
	if s.useRLE {
		if transform == nil {
			s.ct.symbolTT[s.rleVal].outBits = s.rleVal
			s.maxBits = s.rleVal
			return
		}
		s.maxBits = transform[s.rleVal]
		s.ct.symbolTT[s.rleVal].outBits = s.maxBits
		return
	}
	if transform == nil {
		for i := range s.ct.symbolTT[:s.symbolLen] {
			s.ct.symbolTT[i].outBits = uint8(i)
		}
		s.maxBits = uint8(s.symbolLen - 1)
		return
	}
	s.maxBits = 0
	for i, v := range transform[:s.symbolLen] {
		s.ct.symbolTT[i].outBits = v
		if v > s.maxBits {
			// We could assume bits always going up, but we play safe.
			s.maxBits = v
		}
	}
}

// normalizeCount will normalize the count of the symbols so
// the total is equal to the table size.
// If successful, compression tables will also be made ready.
func (s *fseEncoder) normalizeCount(length int) error {
	if s.reUsed {
		return nil
	}
	s.optimalTableLog(length)
	var (
		tableLog          = s.actualTableLog
		scale             = 62 - uint64(tableLog)
		step              = (1 << 62) / uint64(length)
		vStep             = uint64(1) << (scale - 20)
		stillToDistribute = int16(1 << tableLog)
		largest           int
		largestP          int16
		lowThreshold      = (uint32)(length >> tableLog)
	)
	if s.maxCount == length {
		s.useRLE = true
		return nil
	}
	s.useRLE = false
	for i, cnt := range s.count[:s.symbolLen] {
		// already handled
		// if (count[s] == s.length) return 0;   /* rle special case */

		if cnt == 0 {
			s.norm[i] = 0
			continue
		}
		if cnt <= lowThreshold {
			s.norm[i] = -1
			stillToDistribute--
		} else {
			proba := (int16)((uint64(cnt) * step) >> scale)
			if proba < 8 {
				restToBeat := vStep * uint64(rtbTable[proba])
				v := uint64(cnt)*step - (uint64(proba) << scale)
				if v > restToBeat {
					proba++
				}
			}
			if proba > largestP {
				largestP = proba
				largest = i
			}
			s.norm[i] = proba
			stillToDistribute -= proba
		}
	}

	if -stillToDistribute >= (s.norm[largest] >> 1) {
		// corner case, need another normalization method
		err := s.normalizeCount2(length)
		if err != nil {
			return err
		}
		if debugAsserts {
			err = s.validateNorm()
			if err != nil {
				return err
			}
		}
		return s.buildCTable()
	}
	s.norm[largest] += stillToDistribute
	if debugAsserts {
		err := s.validateNorm()
		if err != nil {
			return err
		}
	}
	return s.buildCTable()
}

// Secondary normalization method.
// To be used when primary method fails.
func (s *fseEncoder) normalizeCount2(length int) error {
	const notYetAssigned = -2
	var (
		distributed  uint32
		total        = uint32(length)
		tableLog     = s.actualTableLog
		lowThreshold = total >> tableLog
		lowOne       = (total * 3) >> (tableLog + 1)
	)
	for i, cnt := range s.count[:s.symbolLen] {
		if cnt == 0 {
			s.norm[i] = 0
			continue
		}
		if cnt <= lowThreshold {
			s.norm[i] = -1
			distributed++
			total -= cnt
			continue
		}
		if cnt <= lowOne {
			s.norm[i] = 1
			distributed++
			total -= cnt
			continue
		}
		s.norm[i] = notYetAssigned
	}
	toDistribute := (1 << tableLog) - distributed

	if (total / toDistribute) > lowOne {
		// risk of rounding to zero
		lowOne = (total * 3) / (toDistribute * 2)
		for i, cnt := range s.count[:s.symbolLen] {
			if (s.norm[i] == notYetAssigned) && (cnt <= lowOne) {
				s.norm[i] = 1
				distributed++
				total -= cnt
				continue
			}
		}
		toDistribute = (1 << tableLog) - distributed
	}
	if distributed == uint32(s.symbolLen)+1 {
		// all values are pretty poor;
		//   probably incompressible data (should have already been detected);
		//   find max, then give all remaining points to max
		var maxV int
		var maxC uint32
		for i, cnt := range s.count[:s.symbolLen] {
			if cnt > maxC {
				maxV = i
				maxC = cnt
			}
		}
		s.norm[maxV] += int16(toDistribute)
		return nil
	}

	if total == 0 {
		// all of the symbols were low enough for the lowOne or lowThreshold
		for i := uint32(0); toDistribute > 0; i = (i + 1) % (uint32(s.symbolLen)) {
			if s.norm[i] > 0 {
				toDistribute--
				s.norm[i]++
			}
		}
		return nil
	}

	var (
		vStepLog = 62 - uint64(tableLog)
		mid      = uint64((1 << (vStepLog - 1)) - 1)
		rStep    = (((1 << vStepLog) * uint64(toDistribute)) + mid) / uint64(total) // scale on remaining
		tmpTotal = mid
	)
	for i, cnt := range s.count[:s.symbolLen] {
		if s.norm[i] == notYetAssigned {
			var (
				end    = tmpTotal + uint64(cnt)*rStep
				sStart = uint32(tmpTotal >> vStepLog)
				sEnd   = uint32(end >> vStepLog)
				weight = sEnd - sStart
			)
			if weight < 1 {
				return errors.New("weight < 1")
			}
			s.norm[i] = int16(weight)
			tmpTotal = end
		}
	}
	return nil
}

// optimalTableLog calculates and sets the optimal tableLog in s.actualTableLog
func (s *fseEncoder) optimalTableLog(length int) {
	tableLog := uint8(maxEncTableLog)
	minBitsSrc := highBit(uint32(length)) + 1
	minBitsSymbols := highBit(uint32(s.symbolLen-1)) + 2
	minBits := uint8(minBitsSymbols)
	if minBitsSrc < minBitsSymbols {
		minBits = uint8(minBitsSrc)
	}

	maxBitsSrc := uint8(highBit(uint32(length-1))) - 2
	if maxBitsSrc < tableLog {
		// Accuracy can be reduced
		tableLog = maxBitsSrc
	}
	if minBits > tableLog {
		tableLog = minBits
	}
	// Need a minimum to safely represent all symbol values
	if tableLog < minEncTablelog {
		tableLog = minEncTablelog
	}
	if tableLog > maxEncTableLog {
		tableLog = maxEncTableLog
	}
	s.actualTableLog = tableLog
}

// validateNorm validates the normalized histogram table.
func (s *fseEncoder) validateNorm() (err error) {
	var total int
	for _, v := range s.norm[:s.symbolLen] {
		if v >= 0 {
			total += int(v)
		} else {
			total -= int(v)
		}
	}
	defer func() {
		if err == nil {
			return
		}
		fmt.Printf("selected TableLog: %d, Symbol length: %d\n", s.actualTableLog, s.symbolLen)
		for i, v := range s.norm[:s.symbolLen] {
			fmt.Printf("%3d: %5d -> %4d \n", i, s.count[i], v)
		}
	}()
	if total != (1 << s.actualTableLog) {
		return fmt.Errorf("warning: Total == %d != %d", total, 1<<s.actualTableLog)
	}
	for i, v := range s.count[s.symbolLen:] {
		if v != 0 {
			return fmt.Errorf("warning: Found symbol out of range, %d after cut", i)
		}
	}
	return nil
}

// writeCount will write the normalized histogram count to header.
// This is read back by readNCount.
func (s *fseEncoder) writeCount(out []byte) ([]byte, error) {
	if s.useRLE {
		return append(out, s.rleVal), nil
	}
	if s.preDefined || s.reUsed {
		// Never write predefined.
		return out, nil
	}

	var (
		tableLog  = s.actualTableLog
		tableSize = 1 << tableLog
		previous0 bool
		charnum   uint16

		// maximum header size plus 2 extra bytes for final output if bitCount == 0.
		maxHeaderSize = ((int(s.symbolLen) * int(tableLog)) >> 3) + 3 + 2

		// Write Table Size
		bitStream = uint32(tableLog - minEncTablelog)
		bitCount  = uint(4)
		remaining = int16(tableSize + 1) /* +1 for extra accuracy */
		threshold = int16(tableSize)
		nbBits    = uint(tableLog + 1)
		outP      = len(out)
	)
	if cap(out) < outP+maxHeaderSize {
		out = append(out, make([]byte, maxHeaderSize*3)...)
		out = out[:len(out)-maxHeaderSize*3]
	}
	out = out[:outP+maxHeaderSize]

	// stops at 1
	for remaining > 1 {
		if previous0 {
			start := charnum
			for s.norm[charnum] == 0 {
				charnum++
			}
			for charnum >= start+24 {
				start += 24
				bitStream += uint32(0xFFFF) << bitCount
				out[outP] = byte(bitStream)
				out[outP+1] = byte(bitStream >> 8)
				outP += 2
				bitStream >>= 16
			}
			for charnum >= start+3 {
				start += 3
				bitStream += 3 << bitCount
				bitCount += 2
			}
			bitStream += uint32(charnum-start) << bitCount
			bitCount += 2
			if bitCount > 16 {
				out[outP] = byte(bitStream)
				out[outP+1] = byte(bitStream >> 8)
				outP += 2
				bitStream >>= 16
				bitCount -= 16
			}
		}

		count := s.norm[charnum]
		charnum++
		max := (2*threshold - 1) - remaining
		if count < 0 {
			remaining += count
		} else {
			remaining -= count
		}
		count++ // +1 for extra accuracy
		if count >= threshold {
			count += max // [0..max[ [max..threshold[ (...) [threshold+max 2*threshold[
		}
		bitStream += uint32(count) << bitCount
		bitCount += nbBits
		if count < max {
			bitCount--
		}

		previous0 = count == 1
		if remaining < 1 {
			return nil, errors.New("internal error: remaining < 1")
		}
		for remaining < threshold {
			nbBits--
			threshold >>= 1
		}

		if bitCount > 16 {
			out[outP] = byte(bitStream)
			out[outP+1] = byte(bitStream >> 8)
			outP += 2
			bitStream >>= 16
			bitCount -= 16
		}
	}

	if outP+2 > len(out) {
		return nil, fmt.Errorf("internal error: %d > %d, maxheader: %d, sl: %d, tl: %d, normcount: %v", outP+2, len(out), maxHeaderSize, s.symbolLen, int(tableLog), s.norm[:s.symbolLen])
	}
	out[outP] = byte(bitStream)
	out[outP+1] = byte(bitStream >> 8)
	outP += int((bitCount + 7) / 8)

	if charnum > s.symbolLen {
		return nil, errors.New("internal error: charnum > s.symbolLen")
	}
	return out[:outP], nil
}

// Approximate symbol cost, as fractional value, using fixed-point format (accuracyLog fractional bits)
// note 1 : assume symbolValue is valid (<= maxSymbolValue)
// note 2 : if freq[symbolValue]==0, @return a fake cost of tableLog+1 bits *
func (s *fseEncoder) bitCost(symbolValue uint8, accuracyLog uint32) uint32 {
	minNbBits := s.ct.symbolTT[symbolValue].deltaNbBits >> 16
	threshold := (minNbBits + 1) << 16
	if debugAsserts {
		if !(s.actualTableLog < 16) {
			panic("!s.actualTableLog < 16")
		}
		// ensure enough room for renormalization double shift
		if !(uint8(accuracyLog) < 31-s.actualTableLog) {
			panic("!uint8(accuracyLog) < 31-s.actualTableLog")
		}
	}
	tableSize := uint32(1) << s.actualTableLog
	deltaFromThreshold := threshold - (s.ct.symbolTT[symbolValue].deltaNbBits + tableSize)
	// linear interpolation (very approximate)
	normalizedDeltaFromThreshold := (deltaFromThreshold << accuracyLog) >> s.actualTableLog
	bitMultiplier := uint32(1) << accuracyLog
	if debugAsserts {
		if s.ct.symbolTT[symbolValue].deltaNbBits+tableSize > threshold {
			panic("s.ct.symbolTT[symbolValue].deltaNbBits+tableSize > threshold")
		}
		if normalizedDeltaFromThreshold > bitMultiplier {
			panic("normalizedDeltaFromThreshold > bitMultiplier")
		}
	}
	return (minNbBits+1)*bitMultiplier - normalizedDeltaFromThreshold
}

// Returns the cost in bits of encoding the distribution in count using ctable.
// Histogram should only be up to the last non-zero symbol.
// Returns an -1 if ctable cannot represent all the symbols in count.
func (s *fseEncoder) approxSize(hist []uint32) uint32 {
	if int(s.symbolLen) < len(hist) {
		// More symbols than we have.
		return math.MaxUint32
	}
	if s.useRLE {
		// We will never reuse RLE encoders.
		return math.MaxUint32
	}
	const kAccuracyLog = 8
	badCost := (uint32(s.actualTableLog) + 1) << kAccuracyLog
	var cost uint32
	for i, v := range hist {
		if v == 0 {
			continue
		}
		if s.norm[i] == 0 {
			return math.MaxUint32
		}
		bitCost := s.bitCost(uint8(i), kAccuracyLog)
		if bitCost > badCost {
			return math.MaxUint32
		}
		cost += v * bitCost
	}
	return cost >> kAccuracyLog
}

// maxHeaderSize returns the maximum header size in bits.
// This is not exact size, but we want a penalty for new tables anyway.
func (s *fseEncoder) maxHeaderSize() uint32 {
	if s.preDefined {
		return 0
	}
	if s.useRLE {
		return 8
	}
	return (((uint32(s.symbolLen) * uint32(s.actualTableLog)) >> 3) + 3) * 8
}

// cState contains the compression state of a stream.
type cState struct {
	bw         *bitWriter
	stateTable []uint16
	state      uint16
}

// init will initialize the compression state to the first symbol of the stream.
func (c *cState) init(bw *bitWriter, ct *cTable, first symbolTransform) {
	c.bw = bw
	c.stateTable = ct.stateTable
	if len(c.stateTable) == 1 {
		// RLE
		c.stateTable[0] = uint16(0)
		c.state = 0
		return
	}
	nbBitsOut := (first.deltaNbBits + (1 << 15)) >> 16
	im := int32((nbBitsOut << 16) - first.deltaNbBits)
	lu := (im >> nbBitsOut) + int32(first.deltaFindState)
	c.state = c.stateTable[lu]
}

// flush will write the tablelog to the output and flush the remaining full bytes.
func (c *cState) flush(tableLog uint8) {
	c.bw.flush32()
	c.bw.addBits16NC(c.state, tableLog)
}
