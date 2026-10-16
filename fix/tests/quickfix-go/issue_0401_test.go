package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/schedule"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/401
//
// A connection arriving outside the session's schedule left the acceptor's
// reading goroutine blocked forever on a channel the session no longer
// read. Such a connection must be closed, the session must take the next
// one once its window opens, and Run must still end promptly.

// i401Schedule is a single window.
type i401Schedule struct{ w schedule.Window }

func (s i401Schedule) String() string { return "issue 401" }

func (s i401Schedule) Window(t time.Time) (schedule.Window, bool, bool) {
	if !t.Before(s.w.End) {
		return schedule.Window{}, false, false
	}
	return s.w, !t.Before(s.w.Start), true
}

func TestIssue401_ConnectionOutsideScheduleIsClosed(t *testing.T) {
	now := time.Now()
	sched := i401Schedule{schedule.Window{Start: now.Add(300 * time.Millisecond), End: now.Add(time.Hour), ID: "w"}}
	rec := newRecorder()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{Logger: testLogger()})
	s, err := a.Add(accConfig(rec, func(c *session.Config) { c.Schedule = sched }))
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()

	// Before the window: refused.
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	select {
	case ev := <-p.frames:
		if ev.err == nil {
			if time.Now().Before(sched.w.Start) {
				t.Fatalf("before the window: got %s, want the connection closed", visible(ev.frame))
			}
			cancel()
			<-done
			t.Skip("the window opened before the first Logon was handled")
		}
	case <-time.After(timeout):
		t.Fatal("connection still open")
	}
	if st := s.State(); st != session.StateDisconnected {
		t.Fatalf("before the window: %s, want disconnected", st)
	}

	// Within it: logged on.
	waitFor(t, "the window", func() bool { return time.Now().After(sched.w.Start) })
	waitFor(t, "a logon", func() bool {
		p = dialPeer(t, l)
		p.send("A", 0, "98", "0", "108", "30")
		ev := <-p.frames
		return ev.err == nil
	})
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })

	cancel()
	p.expect("5")
	p.send("5", 0)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run still running 1s after its context was cancelled")
	}
}
