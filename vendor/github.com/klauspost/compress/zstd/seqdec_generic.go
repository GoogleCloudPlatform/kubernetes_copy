//go:build !amd64 || appengine || !gc || noasm
// +build !amd64 appengine !gc noasm

package zstd

import (
	"fmt"
	"io"
)

// decode sequences from the stream with the provided history but without dictionary.
func (s *sequenceDecs) decodeSyncSimple(hist []byte) (bool, error) {
	return false, nil
}

// decode sequences from the stream without the provided history.
func (s *sequenceDecs) decode(seqs []seqVals) error {
	br := s.br

	// Grab full sizes tables, to avoid bounds checks.
	llTable, mlTable, ofTable := s.litLengths.fse.dt[:maxTablesize], s.matchLengths.fse.dt[:maxTablesize], s.offsets.fse.dt[:maxTablesize]
	llState, mlState, ofState := s.litLengths.state.state, s.matchLengths.state.state, s.offsets.state.state
	s.seqSize = 0
	litRemain := len(s.literals)

	maxBlockSize := maxCompressedBlockSize
	if s.windowSize < maxBlockSize {
		maxBlockSize = s.windowSize
	}
	for i := range seqs {
		var ll, mo, ml int
		if len(br.in) > 4+((maxOffsetBits+16+16)>>3) {
			// inlined function:
			// ll, mo, ml = s.nextFast(br, llState, mlState, ofState)

			// Final will not read from stream.
			var llB, mlB, moB uint8
			ll, llB = llState.final()
			ml, mlB = mlState.final()
			mo, moB = ofState.final()

			// extra bits are stored in reverse order.
			br.fillFast()
			mo += br.getBits(moB)
			if s.maxBits > 32 {
				br.fillFast()
			}
			ml += br.getBits(mlB)
			ll += br.getBits(llB)

			if moB > 1 {
				s.prevOffset[2] = s.prevOffset[1]
				s.prevOffset[1] = s.prevOffset[0]
				s.prevOffset[0] = mo
			} else {
				// mo = s.adjustOffset(mo, ll, moB)
				// Inlined for rather big speedup
				if ll == 0 {
					// There is an exception though, when current sequence's literals_length = 0.
					// In this case, repeated offsets are shifted by one, so an offset_value of 1 means Repeated_Offset2,
					// an offset_value of 2 means Repeated_Offset3, and an offset_value of 3 means Repeated_Offset1 - 1_byte.
					mo++
				}

				if mo == 0 {
					mo = s.prevOffset[0]
				} else {
					var temp int
					if mo == 3 {
						temp = s.prevOffset[0] - 1
					} else {
						temp = s.prevOffset[mo]
					}

					if temp == 0 {
						// 0 is not valid; input is corrupted; force offset to 1
						println("WARNING: temp was 0")
						temp = 1
					}

					if mo != 1 {
						s.prevOffset[2] = s.prevOffset[1]
					}
					s.prevOffset[1] = s.prevOffset[0]
					s.prevOffset[0] = temp
					mo = temp
				}
			}
			br.fillFast()
		} else {
			if br.overread() {
				if debugDecoder {
					printf("reading sequence %d, exceeded available data\n", i)
				}
				return io.ErrUnexpectedEOF
			}
			ll, mo, ml = s.next(br, llState, mlState, ofState)
			br.fill()
		}

		if debugSequences {
			println("Seq", i, "Litlen:", ll, "mo:", mo, "(abs) ml:", ml)
		}
		// Evaluate.
		// We might be doing this async, so do it early.
		if mo == 0 && ml > 0 {
			return fmt.Errorf("zero matchoff and matchlen (%d) > 0", ml)
		}
		if ml > maxMatchLen {
			return fmt.Errorf("match len (%d) bigger than max allowed length", ml)
		}
		s.seqSize += ll + ml
		if s.seqSize > maxBlockSize {
			return fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)
		}
		litRemain -= ll
		if litRemain < 0 {
			return fmt.Errorf("unexpected literal count, want %d bytes, but only %d is available", ll, litRemain+ll)
		}
		seqs[i] = seqVals{
			ll: ll,
			ml: ml,
			mo: mo,
		}
		if i == len(seqs)-1 {
			// This is the last sequence, so we shouldn't update state.
			break
		}

		// Manually inlined, ~ 5-20% faster
		// Update all 3 states at once. Approx 20% faster.
		nBits := llState.nbBits() + mlState.nbBits() + ofState.nbBits()
		if nBits == 0 {
			llState = llTable[llState.newState()&maxTableMask]
			mlState = mlTable[mlState.newState()&maxTableMask]
			ofState = ofTable[ofState.newState()&maxTableMask]
		} else {
			bits := br.get32BitsFast(nBits)
			lowBits := uint16(bits >> ((ofState.nbBits() + mlState.nbBits()) & 31))
			llState = llTable[(llState.newState()+lowBits)&maxTableMask]

			lowBits = uint16(bits >> (ofState.nbBits() & 31))
			lowBits &= bitMask[mlState.nbBits()&15]
			mlState = mlTable[(mlState.newState()+lowBits)&maxTableMask]

			lowBits = uint16(bits) & bitMask[ofState.nbBits()&15]
			ofState = ofTable[(ofState.newState()+lowBits)&maxTableMask]
		}
	}
	s.seqSize += litRemain
	if s.seqSize > maxBlockSize {
		return fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)
	}
	err := br.close()
	if err != nil {
		printf("Closing sequences: %v, %+v\n", err, *br)
	}
	return err
}

// executeSimple handles cases when a dictionary is not used.
func (s *sequenceDecs) executeSimple(seqs []seqVals, hist []byte) error {
	// Ensure we have enough output size...
	if len(s.out)+s.seqSize > cap(s.out) {
		addBytes := s.seqSize + len(s.out)
		s.out = append(s.out, make([]byte, addBytes)...)
		s.out = s.out[:len(s.out)-addBytes]
	}

	if debugDecoder {
		printf("Execute %d seqs with literals: %d into %d bytes\n", len(seqs), len(s.literals), s.seqSize)
	}

	var t = len(s.out)
	out := s.out[:t+s.seqSize]

	for _, seq := range seqs {
		// Add literals
		copy(out[t:], s.literals[:seq.ll])
		t += seq.ll
		s.literals = s.literals[seq.ll:]

		// Malformed input
		if seq.mo > t+len(hist) || seq.mo > s.windowSize {
			return fmt.Errorf("match offset (%d) bigger than current history (%d)", seq.mo, t+len(hist))
		}

		// Copy from history.
		if v := seq.mo - t; v > 0 {
			// v is the start position in history from end.
			start := len(hist) - v
			if seq.ml > v {
				// Some goes into the current block.
				// Copy remainder of history
				copy(out[t:], hist[start:])
				t += v
				seq.ml -= v
			} else {
				copy(out[t:], hist[start:start+seq.ml])
				t += seq.ml
				continue
			}
		}

		// We must be in the current buffer now
		if seq.ml > 0 {
			start := t - seq.mo
			if seq.ml <= t-start {
				// No overlap
				copy(out[t:], out[start:start+seq.ml])
				t += seq.ml
			} else {
				// Overlapping copy
				// Extend destination slice and copy one byte at the time.
				src := out[start : start+seq.ml]
				dst := out[t:]
				dst = dst[:len(src)]
				t += len(src)
				// Destination is the space we just added.
				for i := range src {
					dst[i] = src[i]
				}
			}
		}
	}
	// Add final literals
	copy(out[t:], s.literals)
	if debugDecoder {
		t += len(s.literals)
		if t != len(out) {
			panic(fmt.Errorf("length mismatch, want %d, got %d, ss: %d", len(out), t, s.seqSize))
		}
	}
	s.out = out

	return nil
}
