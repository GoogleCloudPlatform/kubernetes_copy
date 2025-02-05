//go:build !appengine && gc && !purego && !noasm
// +build !appengine
// +build gc
// +build !purego
// +build !noasm

#include "textflag.h"

// Registers:
#define h      AX
#define d      AX
#define p      SI // pointer to advance through b
#define n      DX
#define end    BX // loop end
#define v1     R8
#define v2     R9
#define v3     R10
#define v4     R11
#define x      R12
#define prime1 R13
#define prime2 R14
#define prime4 DI

#define round(acc, x) \
	IMULQ prime2, x   \
	ADDQ  x, acc      \
	ROLQ  $31, acc    \
	IMULQ prime1, acc

// round0 performs the operation x = round(0, x).
#define round0(x) \
	IMULQ prime2, x \
	ROLQ  $31, x    \
	IMULQ prime1, x

// mergeRound applies a merge round on the two registers acc and x.
// It assumes that prime1, prime2, and prime4 have been loaded.
#define mergeRound(acc, x) \
	round0(x)         \
	XORQ  x, acc      \
	IMULQ prime1, acc \
	ADDQ  prime4, acc

// blockLoop processes as many 32-byte blocks as possible,
// updating v1, v2, v3, and v4. It assumes that there is at least one block
// to process.
#define blockLoop() \
loop:  \
	MOVQ +0(p), x  \
	round(v1, x)   \
	MOVQ +8(p), x  \
	round(v2, x)   \
	MOVQ +16(p), x \
	round(v3, x)   \
	MOVQ +24(p), x \
	round(v4, x)   \
	ADDQ $32, p    \
	CMPQ p, end    \
	JLE  loop

// func Sum64(b []byte) uint64
TEXT ·Sum64(SB), NOSPLIT|NOFRAME, $0-32
	// Load fixed primes.
	MOVQ ·primes+0(SB), prime1
	MOVQ ·primes+8(SB), prime2
	MOVQ ·primes+24(SB), prime4

	// Load slice.
	MOVQ b_base+0(FP), p
	MOVQ b_len+8(FP), n
	LEAQ (p)(n*1), end

	// The first loop limit will be len(b)-32.
	SUBQ $32, end

	// Check whether we have at least one block.
	CMPQ n, $32
	JLT  noBlocks

	// Set up initial state (v1, v2, v3, v4).
	MOVQ prime1, v1
	ADDQ prime2, v1
	MOVQ prime2, v2
	XORQ v3, v3
	XORQ v4, v4
	SUBQ prime1, v4

	blockLoop()

	MOVQ v1, h
	ROLQ $1, h
	MOVQ v2, x
	ROLQ $7, x
	ADDQ x, h
	MOVQ v3, x
	ROLQ $12, x
	ADDQ x, h
	MOVQ v4, x
	ROLQ $18, x
	ADDQ x, h

	mergeRound(h, v1)
	mergeRound(h, v2)
	mergeRound(h, v3)
	mergeRound(h, v4)

	JMP afterBlocks

noBlocks:
	MOVQ ·primes+32(SB), h

afterBlocks:
	ADDQ n, h

	ADDQ $24, end
	CMPQ p, end
	JG   try4

loop8:
	MOVQ  (p), x
	ADDQ  $8, p
	round0(x)
	XORQ  x, h
	ROLQ  $27, h
	IMULQ prime1, h
	ADDQ  prime4, h

	CMPQ p, end
	JLE  loop8

try4:
	ADDQ $4, end
	CMPQ p, end
	JG   try1

	MOVL  (p), x
	ADDQ  $4, p
	IMULQ prime1, x
	XORQ  x, h

	ROLQ  $23, h
	IMULQ prime2, h
	ADDQ  ·primes+16(SB), h

try1:
	ADDQ $4, end
	CMPQ p, end
	JGE  finalize

loop1:
	MOVBQZX (p), x
	ADDQ    $1, p
	IMULQ   ·primes+32(SB), x
	XORQ    x, h
	ROLQ    $11, h
	IMULQ   prime1, h

	CMPQ p, end
	JL   loop1

finalize:
	MOVQ  h, x
	SHRQ  $33, x
	XORQ  x, h
	IMULQ prime2, h
	MOVQ  h, x
	SHRQ  $29, x
	XORQ  x, h
	IMULQ ·primes+16(SB), h
	MOVQ  h, x
	SHRQ  $32, x
	XORQ  x, h

	MOVQ h, ret+24(FP)
	RET

// func writeBlocks(d *Digest, b []byte) int
TEXT ·writeBlocks(SB), NOSPLIT|NOFRAME, $0-40
	// Load fixed primes needed for round.
	MOVQ ·primes+0(SB), prime1
	MOVQ ·primes+8(SB), prime2

	// Load slice.
	MOVQ b_base+8(FP), p
	MOVQ b_len+16(FP), n
	LEAQ (p)(n*1), end
	SUBQ $32, end

	// Load vN from d.
	MOVQ s+0(FP), d
	MOVQ 0(d), v1
	MOVQ 8(d), v2
	MOVQ 16(d), v3
	MOVQ 24(d), v4

	// We don't need to check the loop condition here; this function is
	// always called with at least one block of data to process.
	blockLoop()

	// Copy vN back to d.
	MOVQ v1, 0(d)
	MOVQ v2, 8(d)
	MOVQ v3, 16(d)
	MOVQ v4, 24(d)

	// The number of bytes written is p minus the old base pointer.
	SUBQ b_base+8(FP), p
	MOVQ p, ret+32(FP)

	RET
