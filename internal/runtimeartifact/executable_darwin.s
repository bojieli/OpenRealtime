//go:build darwin && (amd64 || arm64)

#include "textflag.h"

TEXT libc_csops_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_csops(SB)
GLOBL	·libcCSOpsTrampolineAddress(SB), RODATA, $8
DATA	·libcCSOpsTrampolineAddress(SB)/8, $libc_csops_trampoline<>(SB)
