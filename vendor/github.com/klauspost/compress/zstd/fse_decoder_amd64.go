//go:build amd64 && !appengine && !noasm && gc
// +build amd64,!appengine,!noasm,gc

package zstd

import (
	"fmt"
)

type buildDtableAsmContext struct {
	// inputs
	stateTable *uint16
	norm       *int16
	dt         *uint64

	// outputs --- set by the procedure in the case of error;
	// for interpretation please see the error handling part below
	errParam1 uint64
	errParam2 uint64
}

// buildDtable_asm is an x86 assembly implementation of fseDecoder.buildDtable.
// Function returns non-zero exit code on error.
//
//go:noescape
func buildDtable_asm(s *fseDecoder, ctx *buildDtableAsmContext) int

// please keep in sync with _generate/gen_fse.go
const (
	errorCorruptedNormalizedCounter = 1
	errorNewStateTooBig             = 2
	errorNewStateNoBits             = 3
)

// buildDtable will build the decoding table.
func (s *fseDecoder) buildDtable() error {
	ctx := buildDtableAsmContext{
		stateTable: &s.stateTable[0],
		norm:       &s.norm[0],
		dt:         (*uint64)(&s.dt[0]),
	}
	code := buildDtable_asm(s, &ctx)

	if code != 0 {
		switch code {
		case errorCorruptedNormalizedCounter:
			position := ctx.errParam1
			return fmt.Errorf("corrupted input (position=%d, expected 0)", position)

		case errorNewStateTooBig:
			newState := decSymbol(ctx.errParam1)
			size := ctx.errParam2
			return fmt.Errorf("newState (%d) outside table size (%d)", newState, size)

		case errorNewStateNoBits:
			newState := decSymbol(ctx.errParam1)
			oldState := decSymbol(ctx.errParam2)
			return fmt.Errorf("newState (%d) == oldState (%d) and no bits", newState, oldState)

		default:
			return fmt.Errorf("buildDtable_asm returned unhandled nonzero code = %d", code)
		}
	}
	return nil
}
