// Copied from S2 implementation.

//go:build !appengine && !noasm && gc && !noasm

#include "textflag.h"

// func matchLen(a []byte, b []byte) int
TEXT ·matchLen(SB), NOSPLIT, $0-56
	MOVQ a_base+0(FP), AX
	MOVQ b_base+24(FP), CX
	MOVQ a_len+8(FP), DX

	// matchLen
	XORL SI, SI
	CMPL DX, $0x08
	JB   matchlen_match4_standalone

matchlen_loopback_standalone:
	MOVQ (AX)(SI*1), BX
	XORQ (CX)(SI*1), BX
	JZ   matchlen_loop_standalone

#ifdef GOAMD64_v3
	TZCNTQ BX, BX
#else
	BSFQ BX, BX
#endif
	SHRL $0x03, BX
	LEAL (SI)(BX*1), SI
	JMP  gen_match_len_end

matchlen_loop_standalone:
	LEAL -8(DX), DX
	LEAL 8(SI), SI
	CMPL DX, $0x08
	JAE  matchlen_loopback_standalone

matchlen_match4_standalone:
	CMPL DX, $0x04
	JB   matchlen_match2_standalone
	MOVL (AX)(SI*1), BX
	CMPL (CX)(SI*1), BX
	JNE  matchlen_match2_standalone
	LEAL -4(DX), DX
	LEAL 4(SI), SI

matchlen_match2_standalone:
	CMPL DX, $0x02
	JB   matchlen_match1_standalone
	MOVW (AX)(SI*1), BX
	CMPW (CX)(SI*1), BX
	JNE  matchlen_match1_standalone
	LEAL -2(DX), DX
	LEAL 2(SI), SI

matchlen_match1_standalone:
	CMPL DX, $0x01
	JB   gen_match_len_end
	MOVB (AX)(SI*1), BL
	CMPB (CX)(SI*1), BL
	JNE  gen_match_len_end
	INCL SI

gen_match_len_end:
	MOVQ SI, ret+48(FP)
	RET
