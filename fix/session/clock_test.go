package session_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// skewed is a wall clock set apart from time.Now by an offset that may
// change. Its readings have no monotonic reading: a timer of the session
// left on Config.Now would follow the offset, and show.
type skewed struct{ off atomic.Int64 }

func newSkewed(off time.Duration) *skewed {
	c := &skewed{}
	c.set(off)
	return c
}

func (c *skewed) set(off time.Duration) { c.off.Store(int64(off)) }

func (c *skewed) now() time.Time {
	return time.Now().Add(time.Duration(c.off.Load())).Round(0)
}

// stamp parses the UTCTimestamp of tag in m.
func stamp(t *testing.T, m *fix.Message, tag int) time.Time {
	t.Helper()
	v, err := fix.ParseUTCTimestamp(m.Get(tag))
	if err != nil {
		t.Fatalf("tag %d %q: %v", tag, m.Get(tag), err)
	}
	return v
}

// near checks that got is within a second of want.
func near(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if d := got.Sub(want); d < -time.Second || d > time.Second {
		t.Errorf("%s = %s, want about %s", what, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// The session stamps what it writes with Config.Now: its Logon, messages
// sent, and on a resend, the SendingTime of each message and GapFill, a
// resent message keeping the OrigSendingTime it was first sent with.
func TestNow_Stamps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newSkewed(time.Hour)
		l := mem.Listen("acceptor")
		s, rec := startInitiator(t, l, func(c *session.Config) {
			c.Now = clock.now
			c.MaxLatency = -1
		})
		p := acceptPeer(t, l)
		logon := p.expect("A")
		near(t, "Logon SendingTime", stamp(t, logon, 52), clock.now())
		p.send("A", 0, "98", "0", "108", "30")
		rec.wait(t, func() bool { return rec.logons == 1 })

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.Send(ctx, order("O1")); err != nil {
			t.Fatal(err)
		}
		sent := stamp(t, p.expect("D", "11", "O1"), 52)
		near(t, "order SendingTime", sent, clock.now())

		// Later, on a clock moved on by another hour, the counterparty asks for
		// everything again.
		clock.set(2 * time.Hour)
		p.send("2", 0, "7", "1", "16", "0")
		gapFill := p.expect("4", "34", "1", "123", "Y", "36", "2")
		near(t, "GapFill SendingTime", stamp(t, gapFill, 52), clock.now())
		near(t, "GapFill OrigSendingTime", stamp(t, gapFill, 122), clock.now())
		resent := p.expect("D", "34", "2", "43", "Y", "11", "O1")
		near(t, "resent SendingTime", stamp(t, resent, 52), clock.now())
		if orig := stamp(t, resent, 122); !orig.Equal(sent) {
			t.Errorf("resent OrigSendingTime = %s, want the first SendingTime %s", orig.Format(time.RFC3339Nano), sent.Format(time.RFC3339Nano))
		}
	})
}

// MaxLatency judges a SendingTime received against Config.Now: one right
// by the session's clock passes however far it is from time.Now, one
// right by time.Now is rejected once the session's clock moves away.
func TestNow_MaxLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newSkewed(0)
		s, rec, p := loggedOn(t, func(c *session.Config) {
			c.Now = clock.now
			c.MaxLatency = 2 * time.Second
		})
		clock.set(10 * time.Second)

		p.sendWithHeader("D", "ACC", 2, clock.now(), "11", "ON-ITS-CLOCK")
		rec.wait(t, func() bool { return len(rec.apps) == 1 })
		rec.mu.Lock()
		app, received := rec.apps[0], rec.received[0]
		rec.mu.Unlock()
		if app != "ON-ITS-CLOCK" {
			t.Errorf("delivered %q, want ON-ITS-CLOCK", app)
		}
		near(t, "Inbound.Received", received, clock.now())

		p.sendWithHeader("D", "ACC", 3, time.Now(), "11", "ON-TIME-NOW")
		p.expect("3", "45", "3", "373", "10")
		p.expect("5")
		if n := s.NextTargetSeqNum(); n != 4 {
			t.Errorf("NextTargetSeqNum = %d, want 4: the rejected message counted", n)
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if len(rec.apps) != 1 {
			t.Errorf("delivered %q, want only ON-ITS-CLOCK", rec.apps)
		}
	})
}

// A reset stores its time by Config.Now.
func TestNow_ResetCreatedAt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newSkewed(-time.Hour)
		st := memstore.New()
		l := mem.Listen("acceptor")
		startInitiator(t, l, func(c *session.Config) {
			c.Now = clock.now
			c.Store = st
			c.ResetOnLogon = true
		})
		p := acceptPeer(t, l)
		// The initiator resets before it sends its Logon.
		p.expect("A", "34", "1", "141", "Y")
		state, err := st.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if state.Generation != 2 {
			t.Fatalf("Generation = %d, want 2: reset on logon", state.Generation)
		}
		near(t, "CreatedAt", state.CreatedAt, clock.now())
	})
}

// Schedule windows open and close by Config.Now: a window about to open by
// the session's clock, an hour ahead of time.Now, is entered when that
// clock reaches its start, and ended when it reaches its end.
func TestNow_ScheduleWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newSkewed(time.Hour)
		start := clock.now().Add(500 * time.Millisecond)
		end := start.Add(1500 * time.Millisecond)
		l := mem.Listen("acceptor")
		_, rec := startInitiator(t, l, func(c *session.Config) {
			c.Now = clock.now
			c.MaxLatency = -1
			c.Schedule = windows{{Start: start, End: end, ID: "w"}}
			session.GoTimers(c)
		})
		p := acceptPeer(t, l)
		p.expect("A")
		if now := clock.now(); now.Before(start) {
			t.Errorf("Logon at %s, before the window's start %s", now.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
		}
		p.send("A", 0, "98", "0", "108", "30")
		rec.wait(t, func() bool { return rec.logons == 1 })
		p.expect("5")
		if now := clock.now(); now.Before(end) {
			t.Errorf("logged out at %s, before the window's end %s", now.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
		}
		if late := clock.now().Sub(end); late > 2*time.Second {
			t.Errorf("logged out %v after the window's end", late)
		}
	})
}

// The session's timers keep the monotonic clock, whatever Config.Now says:
// a heartbeat goes out after a heartbeat interval of silence, a
// TestRequest after one and a fifth. A clock behind would send them at
// once, one ahead never.
func TestNow_TimersUnaffected(t *testing.T) {
	for _, off := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(off.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := newSkewed(off)
				l := mem.Listen("acceptor")
				s, rec := startInitiator(t, l, func(c *session.Config) {
					c.Now = clock.now
					c.MaxLatency = -1
					c.HeartBtInt = time.Second
				})
				p := acceptPeer(t, l)
				p.expect("A")
				answered := time.Now()
				p.send("A", 0, "98", "0", "108", "1")
				rec.wait(t, func() bool { return rec.logons == 1 })
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				sending := time.Now()
				if _, err := s.Send(ctx, order("O1")); err != nil {
					t.Fatal(err)
				}
				p.expect("D", "11", "O1")
				p.expect("0")
				if d := time.Since(sending); d < 800*time.Millisecond {
					t.Errorf("Heartbeat %v after the last message was sent, want a heartbeat interval", d)
				}
				p.expect("1")
				if d := time.Since(answered); d < 1100*time.Millisecond {
					t.Errorf("TestRequest %v after the counterparty's last message, want about 1.2s", d)
				}
				rec.mu.Lock()
				defer rec.mu.Unlock()
				if len(rec.logouts) != 0 {
					t.Errorf("logged out: %v", rec.logouts)
				}
			})
		})
	}
}

// MaxPendingAge dates a message waiting to be written on the monotonic
// clock: one held behind a write that takes long is found too old after
// MaxPendingAge, not at once (a clock behind) nor never (a clock ahead).
func TestNow_MaxPendingAge(t *testing.T) {
	for _, off := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(off.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := newSkewed(off)
				st := newSyncStore()
				s, rec, _ := loggedOn(t, func(c *session.Config) {
					c.Now = clock.now
					c.MaxLatency = -1
					c.Store = st
					c.MaxPendingAge = 500 * time.Millisecond
				})
				// The writer syncs O1 for 3s: O2 waits behind it.
				st.delay.Store(int64(3 * time.Second))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := s.Send(ctx, order("O1")); err != nil {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond)
				queued := time.Now()
				if _, err := s.Send(ctx, order("O2")); err != nil {
					t.Fatal(err)
				}
				rec.wait(t, func() bool { return len(rec.logouts) == 1 })
				waited := time.Since(queued)
				rec.mu.Lock()
				reason := rec.logouts[0]
				rec.mu.Unlock()
				if !errors.Is(reason, session.ErrSlowConsumer) {
					t.Fatalf("logged out for %v, want ErrSlowConsumer", reason)
				}
				if waited < 400*time.Millisecond || waited > 2500*time.Millisecond {
					t.Errorf("found too old after %v, want MaxPendingAge, 500ms", waited)
				}
			})
		})
	}
}

// A schedule boundary is not passed early by a clock slow to answer: the
// wait is measured from the clock's reading, not from before it.
func TestNow_WaitUntilSlowClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newSkewed(time.Hour)
		slow := func() time.Time {
			time.Sleep(100 * time.Millisecond)
			return clock.now()
		}
		deadline := clock.now().Add(150 * time.Millisecond)
		if !session.WaitUntil(context.Background(), slow, deadline) {
			t.Fatal("WaitUntil gave up")
		}
		if now := clock.now(); now.Before(deadline) {
			t.Errorf("returned at %s, before %s", now.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
		}
	})
}

// A malformed message received dates the counterparty's last word on the
// monotonic clock too: the TestRequest still waits a heartbeat interval and
// a fifth of silence.
func TestNow_TimersUnaffectedByMalformed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newSkewed(-time.Hour)
		l := mem.Listen("acceptor")
		_, rec := startInitiator(t, l, func(c *session.Config) {
			c.Now = clock.now
			c.MaxLatency = -1
			c.HeartBtInt = time.Second
		})
		p := acceptPeer(t, l)
		p.expect("A")
		p.send("A", 0, "98", "0", "108", "1")
		rec.wait(t, func() bool { return rec.logons == 1 })
		read := time.Now()
		p.writeRaw(rawFrame("35=D|49=ACC|56=INI|34=2|52="+sendingTime()+"|11=O1|abc=1|", ""))
		p.expect("3", "45", "2")
		// Heartbeats may come first.
		for msgType := ""; msgType != "1"; {
			select {
			case ev := <-p.frames:
				if ev.err != nil {
					t.Fatalf("connection ended (%v), expected a TestRequest", ev.err)
				}
				m, err := fix.Parse(ev.frame)
				if err != nil {
					t.Fatalf("invalid message %s: %v", visible(ev.frame), err)
				}
				msgType = string(m.MsgType())
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for a TestRequest")
			}
		}
		if d := time.Since(read); d < 1100*time.Millisecond {
			t.Errorf("TestRequest %v after the malformed message, want about 1.2s", d)
		}
	})
}
