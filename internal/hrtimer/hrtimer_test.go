package hrtimer

import (
	"sync"
	"testing"
	"time"

)

// soon is a deadline inside the precise window, far enough out that every
// caller of a test joins before it.
const soon = 5 * time.Millisecond

// requireIdle checks no wait is left registered and no slot held.
func requireIdle(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		registry.Lock()
		defer registry.Unlock()
		return len(registry.waits) == 0 && len(slots) == 0
	}, time.Second, time.Millisecond, "a wait or a slot leaked")
}

// offsetWallClock makes At read the clock off by d for one test. It shifts
// the wall and monotonic readings together, so it exercises the decisions
// At takes from the wall clock (passed, near, far, join), not a wall-only
// step's effect on the monotonic target.
func offsetWallClock(t *testing.T, d time.Duration) {
	t.Helper()
	wallNow = func() time.Time { return time.Now().Add(d) }
	t.Cleanup(func() { wallNow = time.Now })
}

func TestAt_FiresNoEarlierThanTheDeadline(t *testing.T) {
	deadline := time.Now().Add(soon)
	<-At(deadline)
	require.False(t, time.Now().Before(deadline), "fired before the deadline")
	requireIdle(t)
}

// Listings switching phase at the same boundary share one wait, one slot,
// and all wake.
func TestAt_CallersOfOneInstantShareOneWait(t *testing.T) {
	deadline := time.Now().Add(soon)
	const callers = 32
	chans := make([]<-chan struct{}, callers)
	for i := range chans {
		chans[i] = At(deadline)
	}
	for _, c := range chans[1:] {
		require.Equal(t, chans[0], c, "every caller of the instant shares the wait")
	}
	require.LessOrEqual(t, len(slots), 1, "one instant holds at most one slot")
	for _, c := range chans {
		<-c
	}
	require.False(t, time.Now().Before(deadline))
	requireIdle(t)
}

// The key is the wall instant: the same nanosecond shares whatever its
// location or monotonic reading, the next nanosecond does not.
func TestAt_KeysOnTheWallInstant(t *testing.T) {
	deadline := time.Now().Add(soon)
	elsewhere := deadline.Round(0).In(time.FixedZone("UTC+2", 2*3600))
	require.Equal(t, At(deadline), At(elsewhere), "one instant, two readings of it")
	require.NotEqual(t, At(deadline), At(deadline.Add(time.Nanosecond)), "1ns apart, two waits")
	<-At(deadline.Add(time.Nanosecond))
	requireIdle(t)
}

func TestAt_PastDeadlineIsAlreadyReached(t *testing.T) {
	c := At(time.Now().Add(-time.Second))
	select {
	case <-c:
	default:
		t.Fatal("a past deadline must be reached at once")
	}
	requireIdle(t)
}

// A caller far from the deadline waits on its own Go timer toward the
// deadline itself, never toward a clamp of it: joining it later must not
// wake the later caller early.
func TestAt_FarCallerDoesNotShortenTheDeadline(t *testing.T) {
	deadline := time.Now().Add(MaxWait + 2*time.Millisecond)
	far := At(deadline)
	registry.Lock()
	registered := len(registry.waits)
	registry.Unlock()
	require.Zero(t, registered, "a deadline beyond MaxWait is not shared")

	farWoke := wokeAt(far)
	time.Sleep(time.Until(deadline) - 3*time.Millisecond)
	nearWoke := wokeAt(At(deadline))
	require.False(t, (<-farWoke).Before(deadline), "the far caller woke early")
	require.False(t, (<-nearWoke).Before(deadline), "the near caller woke early")
	requireIdle(t)
}

// wokeAt reports, on its own goroutine, when c closes.
func wokeAt(c <-chan struct{}) <-chan time.Time {
	woke := make(chan time.Time, 1)
	go func() {
		<-c
		woke <- time.Now()
	}()
	return woke
}

// Distinct deadlines beyond MaxConcurrent fall back to Go timers, still
// shared, still never early, and every slot is given back.
func TestAt_BeyondTheBoundStillSharesAndWaits(t *testing.T) {
	base := time.Now().Add(soon)
	const deadlines = 3 * MaxConcurrent
	var wg sync.WaitGroup
	for i := range deadlines {
		deadline := base.Add(time.Duration(i) * time.Microsecond)
		first, second := At(deadline), At(deadline)
		require.Equal(t, first, second, "deadline %d is shared", i)
		for _, c := range []<-chan struct{}{first, second} {
			woke := wokeAt(c)
			wg.Go(func() {
				require.False(t, (<-woke).Before(deadline), "deadline %d woke a caller early", i)
			})
		}
	}
	registry.Lock()
	inFlight := len(registry.waits)
	registry.Unlock()
	require.Equal(t, deadlines, inFlight, "every deadline was in flight at once, most of them past the bound")
	wg.Wait()
	requireIdle(t)
}

// Callers racing a wait's completion either join it before it ends or get
// a fresh outcome; none blocks, none leaks a wait or a slot.
func TestAt_CallersRacingCompletion(t *testing.T) {
	deadline := time.Now().Add(2 * time.Millisecond)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for time.Now().Before(deadline.Add(time.Millisecond)) {
				<-At(deadline)
			}
		})
	}
	wg.Wait()
	requireIdle(t)
}

// After a forward step a caller that sees the deadline passed is not held
// by the wait still pending for it.
func TestAt_ForwardStepSkipsThePendingWait(t *testing.T) {
	deadline := time.Now().Add(soon)
	pending := At(deadline)
	offsetWallClock(t, 2*soon)
	select {
	case <-At(deadline):
	default:
		t.Fatal("a deadline the stepped clock sees passed must be reached at once")
	}
	<-pending
	requireIdle(t)
}

// After a backward step a caller joins the wait pending for the instant:
// it may end before the stepped wall clock reads the deadline, which the
// caller checks and waits again for.
func TestAt_BackwardStepJoinsThePendingWait(t *testing.T) {
	deadline := time.Now().Add(soon)
	pending := At(deadline)
	offsetWallClock(t, -time.Millisecond)
	require.Equal(t, pending, At(deadline))
	<-pending
	requireIdle(t)
}

// BenchmarkAtLateness reports how late At fires past its deadline, the
// figure the pilot's last stretch before a boundary depends on. Compare it
// with BenchmarkTimeAfterLateness on the target host.
func BenchmarkAtLateness(b *testing.B) {
	benchmarkLateness(b, func(d time.Duration) {
		<-At(time.Now().Add(d))
	})
}

func BenchmarkTimeAfterLateness(b *testing.B) {
	benchmarkLateness(b, func(d time.Duration) { <-time.After(d) })
}

func benchmarkLateness(b *testing.B, wait func(time.Duration)) {
	b.Helper()
	const d = 1500 * time.Microsecond
	var late time.Duration
	for b.Loop() {
		start := time.Now()
		wait(d)
		late += time.Since(start) - d
	}
	b.ReportMetric(float64(late.Microseconds())/float64(b.N), "µs-late/op")
}
