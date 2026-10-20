package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session/schedule"
)

// errOutsideWindow refuses a connection outside the session's schedule.
var errOutsideWindow = errors.New("session: outside the session's schedule window")

// precisionWindow is how long before a boundary waiting moves from a Go
// timer, which may fire a millisecond late, to hrtimer.
const precisionWindow = 5 * time.Millisecond

// waitUntil waits until clock, the wall clock if nil, reaches t or ctx is
// done, reporting whether t was reached. at waits out the last
// precisionWindow, hrtimer.At but in tests.
func waitUntil(ctx context.Context, clock func() time.Time, t time.Time, at func(time.Time) <-chan struct{}) bool {
	for {
		wall, d := t, time.Duration(0)
		if clock == nil {
			d = time.Until(t)
		} else {
			// hrtimer waits for an instant of the wall clock's: t on
			// clock, as far away, the wall clock read after clock, which
			// may take its time.
			d = t.Sub(clock())
			wall = time.Now().Add(d)
		}
		if d <= 0 {
			return true
		}
		if d > precisionWindow {
			// At most a minute at a time: the wait runs on the monotonic
			// clock, the boundary on the wall clock, which may step.
			timer := time.NewTimer(min(d-precisionWindow, time.Minute))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return false
			}
			continue
		}
		select {
		case <-at(wall):
		case <-ctx.Done():
			return false
		}
		// hrtimer waits on the monotonic clock: a wall-clock step may have
		// moved t; loop to check.
	}
}

// startSchedule runs the session's schedule until ctx is done or the
// session stops; a session without schedule is always within its window.
func (s *Session) startSchedule(ctx context.Context) {
	if s.cfg.Schedule == nil {
		return
	}
	s.mu.Lock()
	if s.schedCancel != nil || s.stopped {
		s.mu.Unlock()
		return
	}
	ctx, s.schedCancel = context.WithCancel(ctx)
	s.mu.Unlock()
	go s.runSchedule(ctx)
}

// runSchedule follows the schedule's windows: it opens each at its start,
// starting a new sequence generation if the store last ran in another
// window, and closes it at its end, logging the session out.
func (s *Session) runSchedule(ctx context.Context) {
	sched := s.cfg.Schedule
	for ctx.Err() == nil {
		now := s.now()
		w, in, ok := sched.Window(now)
		if !ok {
			s.log.LogAttrs(ctx, slog.LevelError, "the schedule has no window to open", slog.String("schedule", sched.String()))
			s.closeWindow(schedule.Window{End: now})
			return
		}
		if !in {
			s.closeWindow(w)
			s.log.LogAttrs(ctx, slog.LevelInfo, "waiting for the next session window", slog.Time("start", w.Start), slog.Time("end", w.End))
			if !waitUntil(ctx, s.cfg.Now, w.Start, s.cfg.at) {
				return
			}
			continue
		}
		s.openWindow(w)
		if !waitUntil(ctx, s.cfg.Now, w.End, s.cfg.at) {
			return
		}
		s.closeWindow(w)
	}
}

// openWindow lets the session connect within w.
func (s *Session) openWindow(w schedule.Window) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inWindow && s.window.ID == w.ID {
		return
	}
	// The store starts the window's generation when the session next
	// connects (attach), once the previous window's connection is done.
	s.window, s.inWindow = w, true
	if s.windowOpen != nil {
		close(s.windowOpen)
		s.windowOpen = nil
	}
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "session window open",
		slog.Time("start", w.Start), slog.Time("end", w.End))
}

// closeWindow stops the session connecting, logging it out if it is. next
// is the window that closed, or the next one if none was open.
func (s *Session) closeWindow(w schedule.Window) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.inWindow
	s.inWindow = false
	if c := s.c; c != nil {
		switch {
		case was:
			end := fix.AppendUTCTimestamp(nil, w.End, fix.Seconds)
			s.logoutLocked(c, fmt.Sprintf("session window ended at %s", end))
		case s.phase != phaseLogoutSent:
			// A logout in progress, as the window's end began, finishes
			// on its own.
			s.disconnectLocked(c, &LogoutError{Text: "outside the session window"})
		}
	}
	if was {
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "session window closed", slog.Time("end", w.End))
	}
}

// windowSignal returns a channel closed once the session is within a
// window.
func (s *Session) windowSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Schedule == nil || s.inWindow {
		c := make(chan struct{})
		close(c)
		return c
	}
	if s.windowOpen == nil {
		s.windowOpen = make(chan struct{})
	}
	return s.windowOpen
}
