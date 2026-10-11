// Package hrtimer waits out the last stretch before a wall-clock instant
// on the kernel's high-resolution timers.
//
// A Go timer is serviced by the runtime's netpoller, which on Linux blocks
// in epoll_wait with a millisecond timeout, and the kernel may defer that
// wakeup by the thread's timer slack (50µs by default) as well: a timer
// fires up to a millisecond or so late. A deadline that matters to the
// microsecond, a trading-phase boundary, approaches on Go timers and waits
// out its last few milliseconds here.
package hrtimer

import (
	"sync"
	"time"
)

// MaxWait is the longest stretch At waits precisely. A precise wait holds
// an OS thread for its whole duration, so it is for the last stretch before
// a deadline, never for the approach: a deadline further away gets an
// ordinary Go timer.
const MaxWait = 10 * time.Millisecond

// MaxConcurrent bounds the precise waits in flight, one per distinct
// deadline. Each holds a thread of its own; unbounded, a burst of distinct
// deadlines could strain the process's thread budget (pids.max,
// RLIMIT_NPROC), and the Go runtime treats a failed thread creation as
// fatal. It bounds the waits admitted, not every thread the process runs:
// a finished wait's thread retires just after its slot is given back. A
// deadline beyond the bound gets a Go timer, still shared.
const MaxConcurrent = 8

// slots holds one token per precise wait in flight.
var slots = make(chan struct{}, MaxConcurrent)

// closed is the channel At returns for a deadline already reached.
var closed = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// wallNow is time.Now, a variable so tests can offset the wall clock.
var wallNow = time.Now

// waiter is one wait shared by every At call for its deadline.
type waiter struct {
	done chan struct{}
}

// registry holds the waits in flight, by deadline in Unix nanoseconds.
var registry = struct {
	sync.Mutex
	waits map[int64]*waiter
}{waits: make(map[int64]*waiter)}

// At returns a channel closed once deadline, a wall-clock instant, is
// reached. Every call for the same instant within MaxWait of it shares one
// wait, and one thread: listings switching phase at the same market
// boundary wake together.
//
//   - A deadline already reached returns a closed channel.
//   - A deadline within MaxWait waits precisely, while fewer than
//     MaxConcurrent distinct deadlines do; beyond that on a shared Go
//     timer.
//   - A deadline further away waits on a Go timer of its own.
//
// The wait runs on the monotonic clock toward the target the first caller
// translated deadline to, and never ends before it. It does not follow a
// wall-clock step: a step back can end it before the wall clock reads
// deadline, a step forward after. A caller waiting for the wall-clock
// instant re-reads the wall clock once the channel closes and waits again
// if a step moved the instant away; a caller that already sees it passed
// gets a closed channel without joining the stale wait.
//
// Nothing cancels the wait itself. A caller that must react to
// cancellation selects on the channel beside it, and the wait finishes on
// its own.
func At(deadline time.Time) <-chan struct{} {
	// The key is the wall instant: deadlines read from different calendars
	// share a wait when they name the same nanosecond, whatever their
	// location or monotonic reading.
	wall := deadline.Round(0)
	// One clock reading decides the remaining time and anchors the
	// monotonic target, so no pause between two readings can move the
	// target early.
	now := wallNow()
	remaining := wall.Sub(now)
	switch {
	case remaining <= 0:
		return closed
	case remaining > MaxWait:
		return afterFunc(remaining)
	}
	key := wall.UnixNano()
	registry.Lock()
	defer registry.Unlock()
	if w, ok := registry.waits[key]; ok {
		return w.done
	}
	w := &waiter{done: make(chan struct{})}
	registry.waits[key] = w
	target := now.Add(remaining)
	select {
	case slots <- struct{}{}:
		go func() {
			defer func() { <-slots }()
			sleepUntil(target)
			w.finish(key)
		}()
	default:
		time.AfterFunc(time.Until(target), func() { w.finish(key) })
	}
	return w.done
}

// finish wakes every caller of the wait and removes it, in one step under
// the registry lock: no caller can join a wait already over, and an
// instant asked for again, after a clock step, gets a wait of its own.
func (w *waiter) finish(key int64) {
	registry.Lock()
	defer registry.Unlock()
	close(w.done)
	delete(registry.waits, key)
}

// afterFunc returns a channel closed once d has elapsed on a Go timer.
func afterFunc(d time.Duration) <-chan struct{} {
	done := make(chan struct{})
	time.AfterFunc(d, func() { close(done) })
	return done
}
