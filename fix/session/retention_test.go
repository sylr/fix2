package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// lossRecorder records the losses OnRecoveryLoss reports.
type lossRecorder struct {
	*recorder
	losses []loss
}

type loss struct {
	begin, end uint64
	cause      error
}

func (r *lossRecorder) OnRecoveryLoss(_ context.Context, _ *session.Session, begin, end uint64, cause error) {
	r.mu.Lock()
	r.losses = append(r.losses, loss{begin, end, cause})
	r.mu.Unlock()
	r.notify()
}

// retained returns an initiator session with retention r, logged on to a
// scripted peer, and the orders it sent: MsgSeqNum 2 to n+1.
func retained(t *testing.T, r session.Retention, n int, change func(*session.Config)) (*session.Session, *lossRecorder, *peer) {
	t.Helper()
	l := mem.Listen("acceptor")
	h := &lossRecorder{recorder: newRecorder()}
	s, _ := startInitiator(t, l, func(c *session.Config) {
		c.Handler = h
		c.Retention = r
		if change != nil {
			change(c)
		}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	h.wait(t, func() bool { return h.logons == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range n {
		if _, err := s.Send(ctx, order("O")); err != nil {
			t.Fatal(err)
		}
		p.expect("D")
	}
	return s, h, p
}

// settle waits for two evictions to run, the first of which may have
// started before what the test did last.
func settle(t *testing.T, s *session.Session) {
	t.Helper()
	n := session.RetentionSweeps(s)
	waitFor(t, func() bool { return session.RetentionSweeps(s) >= n+2 })
}

// Acknowledged messages beyond Keep are evicted; asked for again, they are
// gap-filled and reported lost, while those kept are resent.
func TestRetention_EvictsAcknowledged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond, Keep: 2}
		s, h, p := retained(t, r, 10, nil) // our Logon is 1, the orders 2 to 11
		p.send("0", 0, "369", "8")
		// Acknowledged up to 8, the last 2 kept: 1 to 8 go.
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 8 })
		settle(t, s)
		if n := s.Stats().Evicted.Load(); n != 8 {
			t.Fatalf("Evicted = %d, want 8", n)
		}

		p.send("2", 0, "7", "2", "16", "0")
		p.expect("4", "34", "2", "36", "9", "123", "Y", "43", "Y")
		for _, seq := range []string{"9", "10", "11"} {
			p.expect("D", "34", seq, "43", "Y")
		}
		h.wait(t, func() bool { return len(h.losses) == 1 })
		if l := h.losses[0]; l.begin != 2 || l.end != 8 || !errors.Is(l.cause, session.ErrBeyondRetention) {
			t.Errorf("loss = %+v, want 2 to 8, ErrBeyondRetention", l)
		}
		if n := s.Stats().BeyondRetention.Load(); n != 7 {
			t.Errorf("BeyondRetention = %d, want 7", n)
		}
	})
}

// Without acknowledgement, every message is kept.
func TestRetention_KeepsUnacknowledged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond}
		s, _, _ := retained(t, r, 10, nil)
		settle(t, s)
		if n := s.Stats().Evicted.Load(); n != 0 {
			t.Fatalf("Evicted = %d, want 0", n)
		}
	})
}

// Beyond Max, messages go, acknowledged or not.
func TestRetention_Max(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond, Keep: 3, Max: 10}
		s, _, p := retained(t, r, 15, nil)
		// Our messages 1 to 16: the last 10 kept.
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 6 })
		// Acknowledged up to 12, the last 3 kept: 1 to 12 go.
		p.send("0", 0, "369", "12")
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 12 })
	})
}

// A first ResendRequest acknowledges the messages before the first it asks
// for; a later one never moves the acknowledgement forward.
func TestRetention_ResendRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond}
		s, _, p := retained(t, r, 10, nil)
		p.send("2", 0, "7", "5", "16", "5")
		p.expect("D", "34", "5", "43", "Y")
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 4 })
		settle(t, s)
		if n := s.Stats().Evicted.Load(); n != 4 {
			t.Fatalf("Evicted = %d after a ResendRequest from 5, want 4", n)
		}
		p.send("0", 0, "369", "6")
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 6 })
		p.send("2", 0, "7", "10", "16", "11")
		p.expect("D", "34", "10", "43", "Y")
		p.expect("D", "34", "11", "43", "Y")
		settle(t, s)
		if n := s.Stats().Evicted.Load(); n != 6 {
			t.Fatalf("Evicted = %d after a ResendRequest from 10, want 6", n)
		}
	})
}

// missingStore reports one message missing from a memstore.
type missingStore struct {
	*memstore.Store
	missing uint64
}

func (m missingStore) Range(ctx context.Context, gen, begin, end uint64, fn func(uint64, []byte, store.Hole) error) error {
	return m.Store.Range(ctx, gen, begin, end, func(seq uint64, raw []byte, hole store.Hole) error {
		if seq == m.missing {
			return fn(seq, nil, store.HoleMissing)
		}
		return fn(seq, raw, hole)
	})
}

// Messages beyond retention gap-filled before a resend aborts are reported.
func TestRetention_LossBeforeAbortedResend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond}
		s, h, p := retained(t, r, 10, func(c *session.Config) {
			c.Store = missingStore{Store: memstore.New(), missing: 10}
		})
		p.send("0", 0, "369", "8")
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 8 })
		p.send("2", 0, "7", "2", "16", "0")
		p.expect("4", "34", "2", "36", "9")
		p.expect("D", "34", "9", "43", "Y")
		p.expect("5")
		h.wait(t, func() bool { return len(h.losses) == 2 })
		for _, l := range h.losses {
			switch l.begin {
			case 2:
				if l.end != 8 || !errors.Is(l.cause, session.ErrBeyondRetention) {
					t.Errorf("loss = %+v, want 2 to 8, ErrBeyondRetention", l)
				}
			case 10:
			default:
				t.Errorf("unexpected loss %+v", l)
			}
		}
	})
}

// The LastMsgSeqNumProcessed of a retransmission, and one above our last
// message, are ignored.
func TestRetention_IgnoresStaleAndBogusAcks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond}
		s, _, p := retained(t, r, 10, nil)
		orig := time.Now().Add(-time.Second).UTC().Format("20060102-15:04:05.000")
		p.send("0", 0, "43", "Y", "122", orig, "369", "8")
		p.send("0", 0, "369", "1000")
		p.send("1", 0, "112", "sync")
		p.expect("0", "112", "sync") // both heartbeats handled
		settle(t, s)
		if n := s.Stats().Evicted.Load(); n != 0 {
			t.Fatalf("Evicted = %d, want 0", n)
		}
		p.send("0", 0, "369", "4")
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 4 })
	})
}

// LastMsgSeqNumProcessed counts on session-level messages too: a
// ResendRequest's moves the acknowledgement back before its BeginSeqNo does.
func TestRetention_SessionLevelLastMsgSeqNumProcessed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond}
		s, _, p := retained(t, r, 10, nil)
		p.send("2", 0, "7", "5", "16", "5", "369", "2")
		p.expect("D", "34", "5", "43", "Y")
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 2 })
		settle(t, s)
		if n := s.Stats().Evicted.Load(); n != 2 {
			t.Fatalf("Evicted = %d, want 2", n)
		}
	})
}

// LastMsgSeqNumProcessed on the counterparty's Logon acknowledges.
func TestRetention_LogonLastMsgSeqNumProcessed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond}
		l := mem.Listen("acceptor")
		s, rec := startInitiator(t, l, func(c *session.Config) { c.Retention = r })
		p := acceptPeer(t, l)
		p.expect("A")
		p.send("A", 0, "98", "0", "108", "30", "369", "1")
		rec.wait(t, func() bool { return rec.logons == 1 })
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 1 })
	})
}

// NextExpectedMsgSeqNum on the counterparty's Logon acknowledges.
func TestRetention_NextExpectedMsgSeqNum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := session.Retention{Interval: 10 * time.Millisecond}
		l := mem.Listen("acceptor")
		s, rec := startInitiator(t, l, func(c *session.Config) {
			c.Retention = r
			c.NextExpectedMsgSeqNum = true
		})
		p := acceptPeer(t, l)
		p.expect("A")
		p.send("A", 0, "98", "0", "108", "30", "789", "2")
		rec.wait(t, func() bool { return rec.logons == 1 })
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 1 })
	})
}

// notTruncater is a store that cannot evict.
type notTruncater struct{ store.Store }

func TestRetention_Validate(t *testing.T) {
	base := session.Config{
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "A", TargetCompID: "B"},
		Handler: session.BaseHandler{},
	}
	for _, tc := range []struct {
		name string
		r    session.Retention
		st   store.Store
		want string // "" for valid
	}{
		{"off", session.Retention{}, nil, ""},
		{"keep", session.Retention{Interval: time.Minute, Keep: 100}, nil, ""},
		{"max", session.Retention{Interval: time.Minute, Keep: 10, Max: 100}, nil, ""},
		{"no interval, keep", session.Retention{Keep: 100}, nil, "Retention.Interval: required"},
		{"no interval, min age", session.Retention{MinAge: time.Hour}, nil, "Retention.Interval: required"},
		{"no interval, max", session.Retention{Max: 100}, nil, "Retention.Interval: required"},
		{"negative interval", session.Retention{Interval: -time.Second}, nil, "Retention.Interval: must be >= 0"},
		{"negative min age", session.Retention{Interval: time.Minute, MinAge: -time.Second}, nil, "Retention.MinAge: must be >= 0"},
		{"negative keep", session.Retention{Interval: time.Minute, Keep: -1}, nil, "Retention.Keep: must be >= 0"},
		{"negative max", session.Retention{Interval: time.Minute, Max: -1}, nil, "Retention.Max: must be >= 0"},
		{"keep above max", session.Retention{Interval: time.Minute, Keep: 30, Max: 20}, nil, "Retention.Keep: 30 is above"},
		{"store", session.Retention{Interval: time.Minute}, notTruncater{}, "does not implement store.Truncater"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := base
				cfg.Retention, cfg.Store = tc.r, tc.st
				err := cfg.Validate()
				switch {
				case tc.want == "" && err != nil:
					t.Fatalf("Validate = %v", err)
				case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
					t.Fatalf("Validate = %v, want %q", err, tc.want)
				}
			})
		})
	}
}
