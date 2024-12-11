//go:build !appengine && gc && !purego && !noasm
// +build !appengine
// +build gc
// +build !purego
// +build !noasm

#include "textflag.h"

// Registers:
#define digest	R1
#define h	R2 // return value
#define p	R3 // input pointer
#define n	R4 // input length
#define nblocks	R5 // n / 32
#define prime1	R7
#define prime2	R8
#define prime3	R9
#define prime4	R10
#define prime5	R11
#define v1	R12
#define v2	R13
#define v3	R14
#define v4	R15
#define x1	R20
#define x2	R21
#define x3	R22
#define x4	R23

#define round(acc, x) \
	MADD prime2, acc, x, acc \
	ROR  $64-31, acc         \
	MUL  prime1, acc

// round0 performs the operation x = round(0, x).
#define round0(x) \
	MUL prime2, x \
	ROR $64-31, x \
	MUL prime1, x

#define mergeRound(acc, x) \
	round0(x)                     \
	EOR  x, acc                   \
	MADD acc, prime4, prime1, acc

// blockLoop processes as many 32-byte blocks as possible,
// updating v1, v2, v3, and v4. It assumes that n >= 32.
#define blockLoop() \
	LSR     $5, n, nblocks  \
	PCALIGN $16             \
	loop:                   \
	LDP.P   16(p), (x1, x2) \
	LDP.P   16(p), (x3, x4) \
	round(v1, x1)           \
	round(v2, x2)           \
	round(v3, x3)           \
	round(v4, x4)           \
	SUB     $1, nblocks     \
	CBNZ    nblocks, loop

// func Sum64(b []byte) uint64
TEXT ·Sum64(SB), NOSPLIT|NOFRAME, $0-32
	LDP b_base+0(FP), (p, n)

	LDP  ·primes+0(SB), (prime1, prime2)
	LDP  ·primes+16(SB), (prime3, prime4)
	MOVD ·primes+32(SB), prime5

	CMP  $32, n
	CSEL LT, prime5, ZR, h // if n < 32 { h = prime5 } else { h = 0 }
	BLT  afterLoop

	ADD  prime1, prime2, v1
	MOVD prime2, v2
	MOVD $0, v3
	NEG  prime1, v4

	blockLoop()

	ROR $64-1, v1, x1
	ROR $64-7, v2, x2
	ADD x1, x2
	ROR $64-12, v3, x3
	ROR $64-18, v4, x4
	ADD x3, x4
	ADD x2, x4, h

	mergeRound(h, v1)
	mergeRound(h, v2)
	mergeRound(h, v3)
	mergeRound(h, v4)

afterLoop:
	ADD n, h

	TBZ   $4, n, try8
	LDP.P 16(p), (x1, x2)

	round0(x1)

	// NOTE: here and below, sequencing the EOR after the ROR (using a
	// rotated register) is worth a small but measurable speedup for small
	// inputs.
	ROR  $64-27, h
	EOR  x1 @> 64-27, h, h
	MADD h, prime4, prime1, h

	round0(x2)
	ROR  $64-27, h
	EOR  x2 @> 64-27, h, h
	MADD h, prime4, prime1, h

try8:
	TBZ    $3, n, try4
	MOVD.P 8(p), x1

	round0(x1)
	ROR  $64-27, h
	EOR  x1 @> 64-27, h, h
	MADD h, prime4, prime1, h

try4:
	TBZ     $2, n, try2
	MOVWU.P 4(p), x2

	MUL  prime1, x2
	ROR  $64-23, h
	EOR  x2 @> 64-23, h, h
	MADD h, prime3, prime2, h

try2:
	TBZ     $1, n, try1
	MOVHU.P 2(p), x3
	AND     $255, x3, x1
	LSR     $8, x3, x2

	MUL prime5, x1
	ROR $64-11, h
	EOR x1 @> 64-11, h, h
	MUL prime1, h

	MUL prime5, x2
	ROR $64-11, h
	EOR x2 @> 64-11, h, h
	MUL prime1, h

try1:
	TBZ   $0, n, finalize
	MOVBU (p), x4

	MUL prime5, x4
	ROR $64-11, h
	EOR x4 @> 64-11, h, h
	MUL prime1, h

finalize:
	EOR h >> 33, h
	MUL prime2, h
	EOR h >> 29, h
	MUL prime3, h
	EOR h >> 32, h

	MOVD h, ret+24(FP)
	RET

// func writeBlocks(s *Digest, b []byte) int
TEXT ·writeBlocks(SB), NOSPLIT|NOFRAME, $0-40
	LDP ·primes+0(SB), (prime1, prime2)

	// Load state. Assume v[1-4] are stored contiguously.
	MOVD s+0(FP), digest
	LDP  0(digest), (v1, v2)
	LDP  16(digest), (v3, v4)

	LDP b_base+8(FP), (p, n)

	blockLoop()

	// Store updated state.
	STP (v1, v2), 0(digest)
	STP (v3, v4), 16(digest)

	BIC  $31, n
	MOVD n, ret+32(FP)
	RET
