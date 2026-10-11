//go:build linux

package hrtimer

import (
	"errors"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// clockNanosleep is unix.ClockNanosleep, a variable so tests can make it
// fail or be interrupted.
var clockNanosleep = unix.ClockNanosleep

// sleepUntil blocks the calling goroutine's thread until deadline, a
// reading of Go's monotonic clock, which on Linux is CLOCK_MONOTONIC. It
// never returns before deadline.
//
// It pins the goroutine to its thread and never unpins it. The thread's
// timer slack is lowered below, and a goroutine that exits while locked
// takes its thread down with it, so no other goroutine ever runs on a
// thread whose slack this changed. sleepUntil must therefore only run on a
// goroutine of its own that exits right after.
func sleepUntil(deadline time.Time) {
	runtime.LockOSThread()
	// Timer slack lets the kernel defer the wakeup, by 50µs by default, to
	// coalesce it with others; 1ns asks for the wakeup as soon as the timer
	// expires. A failure keeps the default slack: a less precise wait, not
	// a wrong one.
	_ = unix.Prctl(unix.PR_SET_TIMERSLACK, 1, 0, 0, 0)
	kernelSleep(deadline)
	// Whatever the kernel wait did, refused by a sandbox (seccomp's EPERM
	// or ENOSYS) or cut short, the wait must not fire early: the rest of
	// it, if any, is waited out on a Go timer.
	if rest := time.Until(deadline); rest > 0 {
		time.Sleep(rest)
	}
}

// kernelSleep waits in clock_nanosleep until deadline, returning early only
// when the kernel refuses the wait.
func kernelSleep(deadline time.Time) {
	// The time left is read before the kernel clock, so the gap between the
	// two readings can only move the kernel deadline later, never earlier.
	// Translating it to CLOCK_MONOTONIC also takes the time spent starting
	// this goroutine out of the wait instead of waiting it again.
	rest := time.Until(deadline)
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
		return
	}
	// An absolute deadline makes a wait interrupted by a signal (EINTR: the
	// runtime's preemption signal among others) resume against the same
	// instant instead of drifting by the time already spent.
	target := unix.NsecToTimespec(unix.TimespecToNsec(now) + rest.Nanoseconds())
	for {
		err := clockNanosleep(unix.CLOCK_MONOTONIC, unix.TIMER_ABSTIME, &target, nil)
		if !errors.Is(err, unix.EINTR) {
			return
		}
	}
}
