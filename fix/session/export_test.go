package session

import (
	"context"
	"time"

	"github.com/sylr/fix2/internal/hrtimer"
)

// RetentionSweeps returns how many evictions s ran, for tests.
func RetentionSweeps(s *Session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ret.sweeps
}

// HeldScanned returns how many messages kept ahead of a gap
// SequenceReset-Reset handling looked at, for tests.
func HeldScanned(s *Session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heldScanned
}

// ErrStoreTimeout is errStoreTimeout, for tests.
var ErrStoreTimeout = errStoreTimeout

// WaitUntil waits until clock reaches t (see waitUntil).
func WaitUntil(ctx context.Context, clock func() time.Time, t time.Time) bool {
	return waitUntil(ctx, clock, t, hrtimer.At)
}

// GoTimers makes c's session wait for schedule boundaries on Go timers
// alone, as a session in a synctest bubble must.
func GoTimers(c *Config) {
	c.at = func(t time.Time) <-chan struct{} {
		done := make(chan struct{})
		time.AfterFunc(time.Until(t), func() { close(done) })
		return done
	}
}
