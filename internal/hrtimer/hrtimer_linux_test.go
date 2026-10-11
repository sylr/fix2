//go:build linux

package hrtimer

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// stubNanosleep replaces clockNanosleep for one test.
func stubNanosleep(t *testing.T, fn func(int32, int, *unix.Timespec, *unix.Timespec) error) {
	t.Helper()
	orig := clockNanosleep
	clockNanosleep = fn
	t.Cleanup(func() { clockNanosleep = orig })
}

// A sandbox refusing clock_nanosleep must not make the wait fire early: the
// rest of it is waited out on a Go timer.
func TestSleepUntil_RefusedSyscallStillWaits(t *testing.T) {
	stubNanosleep(t, func(int32, int, *unix.Timespec, *unix.Timespec) error { return unix.EPERM })
	deadline := time.Now().Add(soon)
	<-At(deadline)
	require.False(t, time.Now().Before(deadline), "fired before the deadline")
}

// An interrupted sleep resumes against the same absolute deadline, however
// many times it is interrupted.
func TestSleepUntil_InterruptedSleepResumesTheSameDeadline(t *testing.T) {
	var mu sync.Mutex
	var targets []unix.Timespec
	stubNanosleep(t, func(clock int32, flags int, rqtp, rmtp *unix.Timespec) error {
		mu.Lock()
		targets = append(targets, *rqtp)
		first := len(targets) == 1
		mu.Unlock()
		if first {
			return unix.EINTR
		}
		return unix.ClockNanosleep(clock, flags, rqtp, rmtp)
	})
	deadline := time.Now().Add(soon)
	<-At(deadline)
	require.False(t, time.Now().Before(deadline), "fired before the deadline")
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(targets), 2, "the interrupted sleep was retried")
	for _, target := range targets[1:] {
		require.Equal(t, targets[0], target, "every retry sleeps toward the same instant")
	}
}
