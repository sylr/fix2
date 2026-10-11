//go:build darwin

package hrtimer

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// libSystem functions, reached through hrtimer_darwin.s as
// golang.org/x/sys/unix reaches them: without cgo.

//go:cgo_import_dynamic libc_mach_absolute_time mach_absolute_time "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_wait_until mach_wait_until "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_pthread_self pthread_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_pthread_mach_thread_np pthread_mach_thread_np "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_thread_policy_set thread_policy_set "/usr/lib/libSystem.B.dylib"

var (
	libc_mach_absolute_time_trampoline_addr     uintptr
	libc_mach_timebase_info_trampoline_addr     uintptr
	libc_mach_wait_until_trampoline_addr        uintptr
	libc_pthread_self_trampoline_addr           uintptr
	libc_pthread_mach_thread_np_trampoline_addr uintptr
	libc_thread_policy_set_trampoline_addr      uintptr
)

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_rawSyscall syscall.rawSyscall
func syscall_rawSyscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

const (
	kernSuccess = 0
	// kernAborted: a signal, the runtime's preemption signal among
	// others, interrupted mach_wait_until.
	kernAborted = 14

	threadTimeConstraintPolicy      = 2
	threadTimeConstraintPolicyCount = 4
)

// timebase converts nanoseconds to mach absolute time units: abs = ns *
// denom / numer (1/1 on Intel, 125/3 on Apple silicon).
var timebase = func() (tb struct{ numer, denom uint32 }) {
	syscall_rawSyscall(libc_mach_timebase_info_trampoline_addr, uintptr(unsafe.Pointer(&tb)), 0, 0)
	if tb.numer == 0 || tb.denom == 0 {
		tb.numer, tb.denom = 1, 1
	}
	return tb
}()

// toAbs converts d, positive, to mach absolute time units, rounding up.
func toAbs(d time.Duration) uint64 {
	n, d2 := uint64(timebase.numer), uint64(timebase.denom)
	return (uint64(d)*d2 + n - 1) / n
}

func machAbsoluteTime() uint64 {
	r, _, _ := syscall_rawSyscall(libc_mach_absolute_time_trampoline_addr, 0, 0, 0)
	return uint64(r)
}

// machWaitUntil is mach_wait_until, a variable so tests can make it fail
// or be interrupted. It returns a kern_return_t.
var machWaitUntil = func(deadline uint64) int {
	r, _, _ := syscall_syscall(libc_mach_wait_until_trampoline_addr, uintptr(deadline), 0, 0)
	return int(int32(r))
}

// sleepUntil blocks the calling goroutine's thread until deadline, a
// reading of Go's monotonic clock, which on darwin is mach absolute time.
// It never returns before deadline.
//
// It pins the goroutine to its thread and never unpins it. The thread is
// made a real-time one below, and a goroutine that exits while locked takes
// its thread down with it, so no other goroutine ever runs on a thread whose
// policy this changed: going back to the standard policy would not restore
// the thread's QoS, nor its priority. sleepUntil must therefore only run on a
// goroutine of its own that exits right after.
func sleepUntil(deadline time.Time) {
	runtime.LockOSThread()
	// macOS coalesces the timers of ordinary threads: their waits end
	// about half a millisecond late, later than a Go timer's. Those of a
	// time-constraint (real-time) thread are not coalesced, and end within
	// tens of microseconds. A failure keeps the default policy: a less
	// precise wait, not a wrong one.
	setTimeConstraint()
	kernelSleep(deadline)
	// Whatever the kernel wait did, refused or cut short, the wait must not
	// fire early: the rest of it, if any, is waited out on a Go timer.
	if rest := time.Until(deadline); rest > 0 {
		time.Sleep(rest)
	}
}

// setTimeConstraint makes the calling thread a time-constraint one: it asks
// for 100µs of computation within 1ms, which a thread that only waits never
// exceeds, so the kernel never demotes it.
func setTimeConstraint() {
	policy := struct {
		period, computation, constraint uint32
		preemptible                     int32
	}{
		computation: uint32(toAbs(100 * time.Microsecond)),
		constraint:  uint32(toAbs(time.Millisecond)),
		preemptible: 1,
	}
	syscall_syscall6(libc_thread_policy_set_trampoline_addr, threadPort(), threadTimeConstraintPolicy,
		uintptr(unsafe.Pointer(&policy)), threadTimeConstraintPolicyCount, 0, 0)
}

// threadPort returns the calling thread's Mach port. Unlike mach_thread_self,
// pthread_mach_thread_np takes no reference on it: there is none to release.
func threadPort() uintptr {
	self, _, _ := syscall_rawSyscall(libc_pthread_self_trampoline_addr, 0, 0, 0)
	port, _, _ := syscall_rawSyscall(libc_pthread_mach_thread_np_trampoline_addr, self, 0, 0)
	return port
}

// kernelSleep waits in mach_wait_until until deadline, returning early only
// when the kernel refuses the wait.
func kernelSleep(deadline time.Time) {
	// The time left is read before mach absolute time, and converted
	// rounding up, so the gap between the two readings can only move the
	// target later, never earlier.
	rest := time.Until(deadline)
	if rest <= 0 {
		return
	}
	// An absolute target makes a wait interrupted by a signal resume
	// against the same instant instead of drifting.
	target := machAbsoluteTime() + toAbs(rest)
	for machWaitUntil(target) == kernAborted {
	}
}
