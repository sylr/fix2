// Trampolines to the libSystem functions hrtimer_darwin.go calls, as
// golang.org/x/sys/unix reaches libSystem: without cgo.

#include "textflag.h"

TEXT libc_mach_absolute_time_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_absolute_time(SB)
GLOBL	·libc_mach_absolute_time_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_absolute_time_trampoline_addr(SB)/8, $libc_mach_absolute_time_trampoline<>(SB)

TEXT libc_mach_timebase_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_timebase_info(SB)
GLOBL	·libc_mach_timebase_info_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_timebase_info_trampoline_addr(SB)/8, $libc_mach_timebase_info_trampoline<>(SB)

TEXT libc_mach_wait_until_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_wait_until(SB)
GLOBL	·libc_mach_wait_until_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_wait_until_trampoline_addr(SB)/8, $libc_mach_wait_until_trampoline<>(SB)

TEXT libc_mach_thread_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_thread_self(SB)
GLOBL	·libc_mach_thread_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_thread_self_trampoline_addr(SB)/8, $libc_mach_thread_self_trampoline<>(SB)

TEXT libc_thread_policy_set_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_thread_policy_set(SB)
GLOBL	·libc_thread_policy_set_trampoline_addr(SB), RODATA, $8
DATA	·libc_thread_policy_set_trampoline_addr(SB)/8, $libc_thread_policy_set_trampoline<>(SB)
