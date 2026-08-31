//go:build darwin && (amd64 || arm64)

#include "textflag.h"

TEXT libc_proc_pidinfo_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_proc_pidinfo(SB)
GLOBL	·libcProcPIDInfoTrampolineAddress(SB), RODATA, $8
DATA	·libcProcPIDInfoTrampolineAddress(SB)/8, $libc_proc_pidinfo_trampoline<>(SB)

TEXT libc_proc_pidfdinfo_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_proc_pidfdinfo(SB)
GLOBL	·libcProcPIDFDInfoTrampolineAddress(SB), RODATA, $8
DATA	·libcProcPIDFDInfoTrampolineAddress(SB)/8, $libc_proc_pidfdinfo_trampoline<>(SB)
