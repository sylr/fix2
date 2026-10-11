//go:build darwin

package hrtimer

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// stubWait replaces machWaitUntil for one test.
func stubWait(t *testing.T, fn func(uint64) int) {
	t.Helper()
	orig := machWaitUntil
	machWaitUntil = fn
	t.Cleanup(func() { machWaitUntil = orig })
}

// A refused mach_wait_until must not make the wait fire early: the rest of
// it is waited out on a Go timer.
func TestSleepUntil_RefusedWaitStillWaits(t *testing.T) {
	stubWait(t, func(uint64) int { return 4 }) // KERN_INVALID_ARGUMENT
	deadline := time.Now().Add(soon)
	<-At(deadline)
	require.False(t, time.Now().Before(deadline), "fired before the deadline")
}

// An interrupted wait resumes against the same absolute deadline, however
// many times it is interrupted.
func TestSleepUntil_AbortedWaitResumesTheSameDeadline(t *testing.T) {
	var mu sync.Mutex
	var targets []uint64
	orig := machWaitUntil
	stubWait(t, func(target uint64) int {
		mu.Lock()
		targets = append(targets, target)
		first := len(targets) == 1
		mu.Unlock()
		if first {
			return kernAborted
		}
		return orig(target)
	})
	deadline := time.Now().Add(soon)
	<-At(deadline)
	require.False(t, time.Now().Before(deadline), "fired before the deadline")
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(targets), 2, "the interrupted wait was retried")
	for _, target := range targets[1:] {
		require.Equal(t, targets[0], target, "every retry waits toward the same instant")
	}
}

// The conversion to mach time rounds up: a wait never ends before its
// deadline because of it.
func TestToAbs_RoundsUp(t *testing.T) {
	for _, d := range []time.Duration{1, 41, 42, 999, time.Microsecond, time.Millisecond + 1} {
		abs := toAbs(d)
		back := time.Duration(abs * uint64(timebase.numer) / uint64(timebase.denom))
		require.False(t, back < d, "toAbs rounds down", d, abs, back)
	}
}

// How late precise waits end, logged: real-time threads wake within tens of
// microseconds on an idle machine. Only never early is asserted, as a
// loaded machine may be slower.
func TestSleepUntil_Lateness(t *testing.T) {
	if testing.Short() {
		t.Skip("measures 50 waits")
	}
	var late []time.Duration
	for range 50 {
		deadline := time.Now().Add(2 * time.Millisecond)
		<-At(deadline)
		d := time.Since(deadline)
		require.False(t, d < 0, "fired before the deadline", -d)
		late = append(late, d)
	}
	slices.Sort(late)
	t.Logf("lateness: p50 %v, p90 %v, max %v", late[len(late)/2], late[len(late)*9/10], late[len(late)-1])
}
