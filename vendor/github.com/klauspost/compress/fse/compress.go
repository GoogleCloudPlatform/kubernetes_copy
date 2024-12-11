// Copyright 2018 Klaus Post. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Based on work Copyright (c) 2013, Yann Collet, released under BSD License.

package fse

import (
	"errors"
	"fmt"
)

// Compress the input bytes. Input must be < 2GB.
// Provide a Scratch buffer to avoid memory allocations.
// Note that the output is also kept in the scratch buffer.
// If input is too hard to compress, ErrIncompressible is returned.
// If input is a single byte value repeated ErrUseRLE is returned.
func Compress(in []byte, s *Scratch) ([]byte, error) {
	if len(in) <= 1 {
		return nil, ErrIncompressible
	}
	if len(in) > (2<<30)-1 {
		return nil, errors.New("input too big, must be < 2GB")
	}
	s, err := s.prepare(in)
	if err != nil {
		return nil, err
	}

	// Create histogram, if none was provided.
	maxCount := s.maxCount
	if maxCount == 0 {
		maxCount = s.countSimple(in)
	}
	// Reset for next run.
	s.clearCount = true
	s.maxCount = 0
	if maxCount == len(in) {
		// One symbol, use RLE
		return nil, ErrUseRLE
	}
	if maxCount == 1 || maxCount < (len(in)>>7) {
		// Each symbol present maximum once or too well distributed.
		return nil, ErrIncompressible
	}
	s.optimalTableLog()
	err = s.normalizeCount()
	if err != nil {
		return nil, err
	}
	err = s.writeCount()
	if err != nil {
		return nil, err
	}

	if false {
		err = s.validateNorm()
		if err != nil {
			return nil, err
		}
	}

	err = s.buildCTable()
	if err != nil {
		return nil, err
	}
	err = s.compress(in)
	if err != nil {
		return nil, err
	}
	s.Out = s.bw.out
	// Check if we compressed.
	if len(s.Out) >= len(in) {
		return nil, ErrIncompressible
	}
	return s.Out, nil
}

// cState contains the compression state of a stream.
type cState struct {
	bw         *bitWriter
	stateTable []uint16
	state      uint16
}

// init will initialize the compression state to the first symbol of the stream.
func (c *cState) init(bw *bitWriter, ct *cTable, tableLog uint8, first symbolTransform) {
	c.bw = bw
	c.stateTable = ct.stateTable

	nbBitsOut := (first.deltaNbBits + (1 << 15)) >> 16
	im := int32((nbBitsOut << 16) - first.deltaNbBits)
	lu := (im >> nbBitsOut) + first.deltaFindState
	c.state = c.stateTable[lu]
}

// encode the output symbol provided and write it to the bitstream.
func (c *cState) encode(symbolTT symbolTransform) {
	nbBitsOut := (uint32(c.state) + symbolTT.deltaNbBits) >> 16
	dstState := int32(c.state>>(nbBitsOut&15)) + symbolTT.deltaFindState
	c.bw.addBits16NC(c.state, uint8(nbBitsOut))
	c.state = c.stateTable[dstState]
}

// encode the output symbol provided and write it to the bitstream.
func (c *cState) encodeZero(symbolTT symbolTransform) {
	nbBitsOut := (uint32(c.state) + symbolTT.deltaNbBits) >> 16
	dstState := int32(c.state>>(nbBitsOut&15)) + symbolTT.deltaFindState
	c.bw.addBits16ZeroNC(c.state, uint8(nbBitsOut))
	c.state = c.stateTable[dstState]
}

// flush will write the tablelog to the output and flush the remaining full bytes.
func (c *cState) flush(tableLog uint8) {
	c.bw.flush32()
	c.bw.addBits16NC(c.state, tableLog)
	c.bw.flush()
}

// compress is the main compression loop that will encode the input from the last byte to the first.
func (s *Scratch) compress(src []byte) error {
	if len(src) <= 2 {
		return errors.New("compress: src too small")
	}
	tt := s.ct.symbolTT[:256]
	s.bw.reset(s.Out)

	// Our two states each encodes every second byte.
	// Last byte encoded (first byte decoded) will always be encoded by c1.
	var c1, c2 cState

	// Encode so remaining size is divisible by 4.
	ip := len(src)
	if ip&1 == 1 {
		c1.init(&s.bw, &s.ct, s.actualTableLog, tt[src[ip-1]])
		c2.init(&s.bw, &s.ct, s.actualTableLog, tt[src[ip-2]])
		c1.encodeZero(tt[src[ip-3]])
		ip -= 3
	} else {
		c2.init(&s.bw, &s.ct, s.actualTableLog, tt[src[ip-1]])
		c1.init(&s.bw, &s.ct, s.actualTableLog, tt[src[ip-2]])
		ip -= 2
	}
	if ip&2 != 0 {
		c2.encodeZero(tt[src[ip-1]])
		c1.encodeZero(tt[src[ip-2]])
		ip -= 2
	}
	src = src[:ip]

	// Main compression loop.
	switch {
	case !s.zeroBits && s.actualTableLog <= 8:
		// We can encode 4 symbols without requiring a flush.
		// We do not need to check if any output is 0 bits.
		for ; len(src) >= 4; src = src[:len(src)-4] {
			s.bw.flush32()
			v3, v2, v1, v0 := src[len(src)-4], src[len(src)-3], src[len(src)-2], src[len(src)-1]
			c2.encode(tt[v0])
			c1.encode(tt[v1])
			c2.encode(tt[v2])
			c1.encode(tt[v3])
		}
	case !s.zeroBits:
		// We do not need to check if any output is 0 bits.
		for ; len(src) >= 4; src = src[:len(src)-4] {
			s.bw.flush32()
			v3, v2, v1, v0 := src[len(src)-4], src[len(src)-3], src[len(src)-2], src[len(src)-1]
			c2.encode(tt[v0])
			c1.encode(tt[v1])
			s.bw.flush32()
			c2.encode(tt[v2])
			c1.encode(tt[v3])
		}
	case s.actualTableLog <= 8:
		// We can encode 4 symbols without requiring a flush
		for ; len(src) >= 4; src = src[:len(src)-4] {
			s.bw.flush32()
			v3, v2, v1, v0 := src[len(src)-4], src[len(src)-3], src[len(src)-2], src[len(src)-1]
			c2.encodeZero(tt[v0])
			c1.encodeZero(tt[v1])
			c2.encodeZero(tt[v2])
			c1.encodeZero(tt[v3])
		}
	default:
		for ; len(src) >= 4; src = src[:len(src)-4] {
			s.bw.flush32()
			v3, v2, v1, v0 := src[len(src)-4], src[len(src)-3], src[len(src)-2], src[len(src)-1]
			c2.encodeZero(tt[v0])
			c1.encodeZero(tt[v1])
			s.bw.flush32()
			c2.encodeZero(tt[v2])
			c1.encodeZero(tt[v3])
		}
	}

	// Flush final state.
	// Used to initialize state when decoding.
	c2.flush(s.actualTableLog)
	c1.flush(s.actualTableLog)

	s.bw.close()
	return nil
}

// writeCount will write the normalized histogram count to header.
// This is read back by readNCount.
func (s *Scratch) writeCount() error {
	var (
		tableLog  = s.actualTableLog
		tableSize = 1 << tableLog
		previous0 bool
		charnum   uint16

		maxHeaderSize = ((int(s.symbolLen)*int(tableLog) + 4 + 2) >> 3) + 3

		// Write Table Size
		bitStream = uint32(tableLog - minTablelog)
		bitCount  = uint(4)
		remaining = int16(tableSize + 1) /* +1 for extra accuracy */
		threshold = int16(tableSize)
		nbBits    = uint(tableLog + 1)
	)
	if cap(s.Out) < maxHeaderSize {
		s.Out = make([]byte, 0, s.br.remain()+maxHeaderSize)
	}
	outP := uint(0)
	out := s.Out[:maxHeaderSize]

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
			return errors.New("internal error: remaining<1")
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

	out[outP] = byte(bitStream)
	out[outP+1] = byte(bitStream >> 8)
	outP += (bitCount + 7) / 8

	if charnum > s.symbolLen {
		return errors.New("internal error: charnum > s.symbolLen")
	}
	s.Out = out[:outP]
	return nil
}

// symbolTransform contains the state transform for a symbol.
type symbolTransform struct {
	deltaFindState int32
	deltaNbBits    uint32
}

// String prints values as a human readable string.
func (s symbolTransform) String() string {
	return fmt.Sprintf("dnbits: %08x, fs:%d", s.deltaNbBits, s.deltaFindState)
}

// cTable contains tables used for compression.
type cTable struct {
	tableSymbol []byte
	stateTable  []uint16
	symbolTT    []symbolTransform
}

// allocCtable will allocate tables needed for compression.
// If existing tables a re big enough, they are simply re-used.
func (s *Scratch) allocCtable() {
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
func (s *Scratch) buildCTable() error {
	tableSize := uint32(1 << s.actualTableLog)
	highThreshold := tableSize - 1
	var cumul [maxSymbolValue + 2]int16

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
				symbolTT[i].deltaFindState = int32(total - 1)
				total++
			default:
				maxBitsOut := uint32(tableLog) - highBits(uint32(v-1))
				minStatePlus := uint32(v) << maxBitsOut
				symbolTT[i].deltaNbBits = (maxBitsOut << 16) - minStatePlus
				symbolTT[i].deltaFindState = int32(total - v)
				total += v
			}
		}
		if total != int16(tableSize) {
			return fmt.Errorf("total mismatch %d (got) != %d (want)", total, tableSize)
		}
	}
	return nil
}

// countSimple will create a simple histogram in s.count.
// Returns the biggest count.
// Does not update s.clearCount.
func (s *Scratch) countSimple(in []byte) (max int) {
	for _, v := range in {
		s.count[v]++
	}
	m, symlen := uint32(0), s.symbolLen
	for i, v := range s.count[:] {
		if v == 0 {
			continue
		}
		if v > m {
			m = v
		}
		symlen = uint16(i) + 1
	}
	s.symbolLen = symlen
	return int(m)
}

// minTableLog provides the minimum logSize to safely represent a distribution.
func (s *Scratch) minTableLog() uint8 {
	minBitsSrc := highBits(uint32(s.br.remain()-1)) + 1
	minBitsSymbols := highBits(uint32(s.symbolLen-1)) + 2
	if minBitsSrc < minBitsSymbols {
		return uint8(minBitsSrc)
	}
	return uint8(minBitsSymbols)
}

// optimalTableLog calculates and sets the optimal tableLog in s.actualTableLog
func (s *Scratch) optimalTableLog() {
	tableLog := s.TableLog
	minBits := s.minTableLog()
	maxBitsSrc := uint8(highBits(uint32(s.br.remain()-1))) - 2
	if maxBitsSrc < tableLog {
		// Accuracy can be reduced
		tableLog = maxBitsSrc
	}
	if minBits > tableLog {
		tableLog = minBits
	}
	// Need a minimum to safely represent all symbol values
	if tableLog < minTablelog {
		tableLog = minTablelog
	}
	if tableLog > maxTableLog {
		tableLog = maxTableLog
	}
	s.actualTableLog = tableLog
}

var rtbTable = [...]uint32{0, 473195, 504333, 520860, 550000, 700000, 750000, 830000}

// normalizeCount will normalize the count of the symbols so
// the total is equal to the table size.
func (s *Scratch) normalizeCount() error {
	var (
		tableLog          = s.actualTableLog
		scale             = 62 - uint64(tableLog)
		step              = (1 << 62) / uint64(s.br.remain())
		vStep             = uint64(1) << (scale - 20)
		stillToDistribute = int16(1 << tableLog)
		largest           int
		largestP          int16
		lowThreshold      = (uint32)(s.br.remain() >> tableLog)
	)

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
		return s.normalizeCount2()
	}
	s.norm[largest] += stillToDistribute
	return nil
}

// Secondary normalization method.
// To be used when primary method fails.
func (s *Scratch) normalizeCount2() error {
	const notYetAssigned = -2
	var (
		distributed  uint32
		total        = uint32(s.br.remain())
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

// validateNorm validates the normalized histogram table.
func (s *Scratch) validateNorm() (err error) {
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
