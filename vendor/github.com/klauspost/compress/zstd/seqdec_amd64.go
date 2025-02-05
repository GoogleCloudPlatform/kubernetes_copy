//go:build amd64 && !appengine && !noasm && gc
// +build amd64,!appengine,!noasm,gc

package zstd

import (
	"fmt"
	"io"

	"github.com/klauspost/compress/internal/cpuinfo"
)

type decodeSyncAsmContext struct {
	llTable     []decSymbol
	mlTable     []decSymbol
	ofTable     []decSymbol
	llState     uint64
	mlState     uint64
	ofState     uint64
	iteration   int
	litRemain   int
	out         []byte
	outPosition int
	literals    []byte
	litPosition int
	history     []byte
	windowSize  int
	ll          int // set on error (not for all errors, please refer to _generate/gen.go)
	ml          int // set on error (not for all errors, please refer to _generate/gen.go)
	mo          int // set on error (not for all errors, please refer to _generate/gen.go)
}

// sequenceDecs_decodeSync_amd64 implements the main loop of sequenceDecs.decodeSync in x86 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decodeSync_amd64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_bmi2 implements the main loop of sequenceDecs.decodeSync in x86 asm with BMI2 extensions.
//
//go:noescape
func sequenceDecs_decodeSync_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_safe_amd64 does the same as above, but does not write more than output buffer.
//
//go:noescape
func sequenceDecs_decodeSync_safe_amd64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_safe_bmi2 does the same as above, but does not write more than output buffer.
//
//go:noescape
func sequenceDecs_decodeSync_safe_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// decode sequences from the stream with the provided history but without a dictionary.
func (s *sequenceDecs) decodeSyncSimple(hist []byte) (bool, error) {
	if len(s.dict) > 0 {
		return false, nil
	}
	if s.maxSyncLen == 0 && cap(s.out)-len(s.out) < maxCompressedBlockSize {
		return false, nil
	}

	// FIXME: Using unsafe memory copies leads to rare, random crashes
	// with fuzz testing. It is therefore disabled for now.
	const useSafe = true
	/*
		useSafe := false
		if s.maxSyncLen == 0 && cap(s.out)-len(s.out) < maxCompressedBlockSizeAlloc {
			useSafe = true
		}
		if s.maxSyncLen > 0 && cap(s.out)-len(s.out)-compressedBlockOverAlloc < int(s.maxSyncLen) {
			useSafe = true
		}
		if cap(s.literals) < len(s.literals)+compressedBlockOverAlloc {
			useSafe = true
		}
	*/

	br := s.br

	maxBlockSize := maxCompressedBlockSize
	if s.windowSize < maxBlockSize {
		maxBlockSize = s.windowSize
	}

	ctx := decodeSyncAsmContext{
		llTable:     s.litLengths.fse.dt[:maxTablesize],
		mlTable:     s.matchLengths.fse.dt[:maxTablesize],
		ofTable:     s.offsets.fse.dt[:maxTablesize],
		llState:     uint64(s.litLengths.state.state),
		mlState:     uint64(s.matchLengths.state.state),
		ofState:     uint64(s.offsets.state.state),
		iteration:   s.nSeqs - 1,
		litRemain:   len(s.literals),
		out:         s.out,
		outPosition: len(s.out),
		literals:    s.literals,
		windowSize:  s.windowSize,
		history:     hist,
	}

	s.seqSize = 0
	startSize := len(s.out)

	var errCode int
	if cpuinfo.HasBMI2() {
		if useSafe {
			errCode = sequenceDecs_decodeSync_safe_bmi2(s, br, &ctx)
		} else {
			errCode = sequenceDecs_decodeSync_bmi2(s, br, &ctx)
		}
	} else {
		if useSafe {
			errCode = sequenceDecs_decodeSync_safe_amd64(s, br, &ctx)
		} else {
			errCode = sequenceDecs_decodeSync_amd64(s, br, &ctx)
		}
	}
	switch errCode {
	case noError:
		break

	case errorMatchLenOfsMismatch:
		return true, fmt.Errorf("zero matchoff and matchlen (%d) > 0", ctx.ml)

	case errorMatchLenTooBig:
		return true, fmt.Errorf("match len (%d) bigger than max allowed length", ctx.ml)

	case errorMatchOffTooBig:
		return true, fmt.Errorf("match offset (%d) bigger than current history (%d)",
			ctx.mo, ctx.outPosition+len(hist)-startSize)

	case errorNotEnoughLiterals:
		return true, fmt.Errorf("unexpected literal count, want %d bytes, but only %d is available",
			ctx.ll, ctx.litRemain+ctx.ll)

	case errorOverread:
		return true, io.ErrUnexpectedEOF

	case errorNotEnoughSpace:
		size := ctx.outPosition + ctx.ll + ctx.ml
		if debugDecoder {
			println("msl:", s.maxSyncLen, "cap", cap(s.out), "bef:", startSize, "sz:", size-startSize, "mbs:", maxBlockSize, "outsz:", cap(s.out)-startSize)
		}
		return true, fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)

	default:
		return true, fmt.Errorf("sequenceDecs_decode returned erroneous code %d", errCode)
	}

	s.seqSize += ctx.litRemain
	if s.seqSize > maxBlockSize {
		return true, fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)
	}
	err := br.close()
	if err != nil {
		printf("Closing sequences: %v, %+v\n", err, *br)
		return true, err
	}

	s.literals = s.literals[ctx.litPosition:]
	t := ctx.outPosition
	s.out = s.out[:t]

	// Add final literals
	s.out = append(s.out, s.literals...)
	if debugDecoder {
		t += len(s.literals)
		if t != len(s.out) {
			panic(fmt.Errorf("length mismatch, want %d, got %d", len(s.out), t))
		}
	}

	return true, nil
}

// --------------------------------------------------------------------------------

type decodeAsmContext struct {
	llTable   []decSymbol
	mlTable   []decSymbol
	ofTable   []decSymbol
	llState   uint64
	mlState   uint64
	ofState   uint64
	iteration int
	seqs      []seqVals
	litRemain int
}

const noError = 0

// error reported when mo == 0 && ml > 0
const errorMatchLenOfsMismatch = 1

// error reported when ml > maxMatchLen
const errorMatchLenTooBig = 2

// error reported when mo > available history or mo > s.windowSize
const errorMatchOffTooBig = 3

// error reported when the sum of literal lengths exeeceds the literal buffer size
const errorNotEnoughLiterals = 4

// error reported when capacity of `out` is too small
const errorNotEnoughSpace = 5

// error reported when bits are overread.
const errorOverread = 6

// sequenceDecs_decode implements the main loop of sequenceDecs in x86 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decode_amd64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode implements the main loop of sequenceDecs in x86 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decode_56_amd64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode implements the main loop of sequenceDecs in x86 asm with BMI2 extensions.
//
//go:noescape
func sequenceDecs_decode_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode implements the main loop of sequenceDecs in x86 asm with BMI2 extensions.
//
//go:noescape
func sequenceDecs_decode_56_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// decode sequences from the stream without the provided history.
func (s *sequenceDecs) decode(seqs []seqVals) error {
	br := s.br

	maxBlockSize := maxCompressedBlockSize
	if s.windowSize < maxBlockSize {
		maxBlockSize = s.windowSize
	}

	ctx := decodeAsmContext{
		llTable:   s.litLengths.fse.dt[:maxTablesize],
		mlTable:   s.matchLengths.fse.dt[:maxTablesize],
		ofTable:   s.offsets.fse.dt[:maxTablesize],
		llState:   uint64(s.litLengths.state.state),
		mlState:   uint64(s.matchLengths.state.state),
		ofState:   uint64(s.offsets.state.state),
		seqs:      seqs,
		iteration: len(seqs) - 1,
		litRemain: len(s.literals),
	}

	if debugDecoder {
		println("decode: decoding", len(seqs), "sequences", br.remain(), "bits remain on stream")
	}

	s.seqSize = 0
	lte56bits := s.maxBits+s.offsets.fse.actualTableLog+s.matchLengths.fse.actualTableLog+s.litLengths.fse.actualTableLog <= 56
	var errCode int
	if cpuinfo.HasBMI2() {
		if lte56bits {
			errCode = sequenceDecs_decode_56_bmi2(s, br, &ctx)
		} else {
			errCode = sequenceDecs_decode_bmi2(s, br, &ctx)
		}
	} else {
		if lte56bits {
			errCode = sequenceDecs_decode_56_amd64(s, br, &ctx)
		} else {
			errCode = sequenceDecs_decode_amd64(s, br, &ctx)
		}
	}
	if errCode != 0 {
		i := len(seqs) - ctx.iteration - 1
		switch errCode {
		case errorMatchLenOfsMismatch:
			ml := ctx.seqs[i].ml
			return fmt.Errorf("zero matchoff and matchlen (%d) > 0", ml)

		case errorMatchLenTooBig:
			ml := ctx.seqs[i].ml
			return fmt.Errorf("match len (%d) bigger than max allowed length", ml)

		case errorNotEnoughLiterals:
			ll := ctx.seqs[i].ll
			return fmt.Errorf("unexpected literal count, want %d bytes, but only %d is available", ll, ctx.litRemain+ll)
		case errorOverread:
			return io.ErrUnexpectedEOF
		}

		return fmt.Errorf("sequenceDecs_decode_amd64 returned erroneous code %d", errCode)
	}

	if ctx.litRemain < 0 {
		return fmt.Errorf("literal count is too big: total available %d, total requested %d",
			len(s.literals), len(s.literals)-ctx.litRemain)
	}

	s.seqSize += ctx.litRemain
	if s.seqSize > maxBlockSize {
		return fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)
	}
	if debugDecoder {
		println("decode: ", br.remain(), "bits remain on stream. code:", errCode)
	}
	err := br.close()
	if err != nil {
		printf("Closing sequences: %v, %+v\n", err, *br)
	}
	return err
}

// --------------------------------------------------------------------------------

type executeAsmContext struct {
	seqs        []seqVals
	seqIndex    int
	out         []byte
	history     []byte
	literals    []byte
	outPosition int
	litPosition int
	windowSize  int
}

// sequenceDecs_executeSimple_amd64 implements the main loop of sequenceDecs.executeSimple in x86 asm.
//
// Returns false if a match offset is too big.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_executeSimple_amd64(ctx *executeAsmContext) bool

// Same as above, but with safe memcopies
//
//go:noescape
func sequenceDecs_executeSimple_safe_amd64(ctx *executeAsmContext) bool

// executeSimple handles cases when dictionary is not used.
func (s *sequenceDecs) executeSimple(seqs []seqVals, hist []byte) error {
	// Ensure we have enough output size...
	if len(s.out)+s.seqSize+compressedBlockOverAlloc > cap(s.out) {
		addBytes := s.seqSize + len(s.out) + compressedBlockOverAlloc
		s.out = append(s.out, make([]byte, addBytes)...)
		s.out = s.out[:len(s.out)-addBytes]
	}

	if debugDecoder {
		printf("Execute %d seqs with literals: %d into %d bytes\n", len(seqs), len(s.literals), s.seqSize)
	}

	var t = len(s.out)
	out := s.out[:t+s.seqSize]

	ctx := executeAsmContext{
		seqs:        seqs,
		seqIndex:    0,
		out:         out,
		history:     hist,
		outPosition: t,
		litPosition: 0,
		literals:    s.literals,
		windowSize:  s.windowSize,
	}
	var ok bool
	if cap(s.literals) < len(s.literals)+compressedBlockOverAlloc {
		ok = sequenceDecs_executeSimple_safe_amd64(&ctx)
	} else {
		ok = sequenceDecs_executeSimple_amd64(&ctx)
	}
	if !ok {
		return fmt.Errorf("match offset (%d) bigger than current history (%d)",
			seqs[ctx.seqIndex].mo, ctx.outPosition+len(hist))
	}
	s.literals = s.literals[ctx.litPosition:]
	t = ctx.outPosition

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
