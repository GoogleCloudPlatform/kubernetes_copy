// +build !appengine
// +build gc
// +build !noasm

#include "textflag.h"
#include "funcdata.h"
#include "go_asm.h"

TEXT ·x86extensions(SB), NOSPLIT, $0
	// 1. determine max EAX value
	XORQ AX, AX
	CPUID

	CMPQ AX, $7
	JB   unsupported

	// 2. EAX = 7, ECX = 0 --- see Table 3-8 "Information Returned by CPUID Instruction"
	MOVQ $7, AX
	MOVQ $0, CX
	CPUID

	BTQ   $3, BX // bit 3 = BMI1
	SETCS AL

	BTQ   $8, BX // bit 8 = BMI2
	SETCS AH

	MOVB AL, bmi1+0(FP)
	MOVB AH, bmi2+1(FP)
	RET

unsupported:
	XORQ AX, AX
	MOVB AL, bmi1+0(FP)
	MOVB AL, bmi2+1(FP)
	RET
