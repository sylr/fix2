package session_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// A message held ahead of a gap is handled as of when it was received: one
// valid then is delivered, with that time, however long recovering the gap
// takes, filled by the missing message, a GapFill or a Reset-mode
// SequenceReset.
func TestHeldArrival_Delivered(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill func(p *peer)
	}{
		{"missing message", func(p *peer) {
			p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
		}},
		{"gap fill", func(p *peer) {
			p.write(from(session.FIX44, 2), "4", "43", "Y", "122", origSendingTime(), "123", "Y", "36", "3")
		}},
		{"reset past it", func(p *peer) {
			p.write(from(session.FIX44, 4), "4", "36", "4")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec, p := loggedOn(t, func(c *session.Config) { c.MaxLatency = 500 * time.Millisecond })
			arrived := time.Now()
			p.write(from(session.FIX44, 3), "D", "11", "VALID-AT-ARRIVAL")
			// Asked for what is missing: the order passed its header checks
			// and is held.
			p.expect("2", "7", "2", "16", "0")
			time.Sleep(700 * time.Millisecond)
			filled := time.Now()
			tc.fill(p)
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			rec.mu.Lock()
			app, received := rec.apps[0], rec.received[0]
			rec.mu.Unlock()
			if app != "VALID-AT-ARRIVAL" {
				t.Errorf("delivered %q, want VALID-AT-ARRIVAL", app)
			}
			if received.Before(arrived) || !received.Before(filled) {
				t.Errorf("Inbound.Received = %v, want when the order was received, between %v and %v", received, arrived, filled)
			}
			if n := s.NextTargetSeqNum(); n != 4 {
				t.Errorf("NextTargetSeqNum = %d, want 4", n)
			}
		})
	}
}

// A GapFill held ahead of a gap is not rejected for a SendingTime that was
// accurate when it was received.
func TestHeldArrival_GapFill(t *testing.T) {
	s, rec, p := loggedOn(t, func(c *session.Config) { c.MaxLatency = 500 * time.Millisecond })
	p.write(from(session.FIX44, 3), "4", "123", "Y", "36", "5")
	p.expect("2", "7", "2", "16", "0")
	time.Sleep(700 * time.Millisecond)
	p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
	p.write(from(session.FIX44, 5), "D", "11", "AFTER-GAP-FILL")
	rec.wait(t, func() bool { return len(rec.apps) == 1 })
	rec.mu.Lock()
	apps := append([]string(nil), rec.apps...)
	rec.mu.Unlock()
	if len(apps) != 1 || apps[0] != "AFTER-GAP-FILL" {
		t.Errorf("delivered %q, want AFTER-GAP-FILL", apps)
	}
	if n := s.NextTargetSeqNum(); n != 6 {
		t.Errorf("NextTargetSeqNum = %d, want 6", n)
	}
}

// A message held ahead of a gap whose header, checked in sequence only, is
// wrong logs out when its turn comes: those held before it are processed,
// those after it neither processed nor counted, to be asked for again once
// logged on. Its OrigSendingTime, after its SendingTime, was wrong when it
// was received.
func TestHeldArrival_LogoutStopsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill func(p *peer)
	}{
		// The outcome only: rejected, the message is not counted, and the
		// messages held after it would wait for it whatever the phase.
		// TestHeldArrival_HeldLogoutStopsHeld and TestHeldLogoutStopsDrain
		// check that a Logout stops advanceLocked.
		{"missing message", func(p *peer) {
			p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
		}},
		{"reset past them", func(p *peer) {
			p.write(from(session.FIX44, 6), "4", "36", "7")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec, p := loggedOn(t, nil)
			p.write(from(session.FIX44, 3), "D", "11", "BEFORE")
			p.expect("2", "7", "2", "16", "0")
			later := string(fix.AppendUTCTimestamp(nil, time.Now().Add(time.Second), fix.Milliseconds))
			p.write(from(session.FIX44, 4), "0", "43", "Y", "122", later)
			p.write(from(session.FIX44, 5), "D", "11", "MUST-STAY-HELD")
			tc.fill(p)
			p.expect("3", "45", "4", "373", "10")
			p.expect("5")
			if st := s.State(); st != session.StateLoggingOut {
				t.Errorf("State = %s, want %s", st, session.StateLoggingOut)
			}
			// Answered out of sequence, the Logout fills no gap: OnLogout
			// comes after what was delivered before it.
			p.write(from(session.FIX44, 8), "5")
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
			rec.mu.Lock()
			apps := append([]string(nil), rec.apps...)
			rec.mu.Unlock()
			if len(apps) != 1 || apps[0] != "BEFORE" {
				t.Errorf("delivered %q, want only BEFORE", apps)
			}
			if n := s.NextTargetSeqNum(); n != 4 {
				t.Errorf("NextTargetSeqNum = %d, want 4", n)
			}
		})
	}
}

// By default, a Logout held ahead of a gap is answered when its
// turn comes: what is held after it is neither processed nor counted.
func TestHeldArrival_HeldLogoutStopsHeld(t *testing.T) {
	s, rec, p := loggedOn(t, nil)
	p.write(from(session.FIX44, 3), "5")
	p.expect("2", "7", "2", "16", "0")
	p.write(from(session.FIX44, 4), "D", "11", "AFTER-LOGOUT")
	p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
	p.expect("5")
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	apps := append([]string(nil), rec.apps...)
	rec.mu.Unlock()
	if len(apps) != 0 {
		t.Errorf("delivered %q after the Logout held, want none", apps)
	}
	if n := s.NextTargetSeqNum(); n != 4 {
		t.Errorf("NextTargetSeqNum = %d, want 4", n)
	}
}

// By default, a Logout held ahead of a gap that a Reset-mode
// SequenceReset moves past stops what is held after it, even when answering
// it reaches the NewSeqNo: the order after it is neither processed nor
// counted.
func TestHeldArrival_HeldLogoutStopsResetPast(t *testing.T) {
	s, rec, p := loggedOn(t, nil)
	p.write(from(session.FIX44, 3), "5")
	p.expect("2", "7", "2", "16", "0")
	p.write(from(session.FIX44, 4), "D", "11", "AFTER-LOGOUT")
	p.write(from(session.FIX44, 2), "4", "36", "4")
	p.expect("5")
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	apps := append([]string(nil), rec.apps...)
	rec.mu.Unlock()
	if len(apps) != 0 {
		t.Errorf("delivered %q after the Logout held, want none", apps)
	}
	if n := s.NextTargetSeqNum(); n != 4 {
		t.Errorf("NextTargetSeqNum = %d, want 4", n)
	}
}

// By default, a Logout held ahead of a gap for longer than
// MaxLatency is answered when its turn comes, not rejected for its
// SendingTime.
func TestHeldArrival_StaleLogout(t *testing.T) {
	_, rec, p := loggedOn(t, func(c *session.Config) {
		c.MaxLatency = 500 * time.Millisecond
	})
	p.write(from(session.FIX44, 3), "5")
	p.expect("2", "7", "2", "16", "0")
	time.Sleep(700 * time.Millisecond)
	p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
	p.expect("5")
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
}

// Chunked, a Logout held ahead of a gap that ends a chunk asks for nothing
// once answered: not the next chunk.
func TestHeldArrival_LogoutEndsChunk(t *testing.T) {
	_, _, p := loggedOn(t, func(c *session.Config) {
		c.ResendChunkSize = 2
	})
	p.write(from(session.FIX44, 4), "5")
	p.expect("2", "7", "2", "16", "3")
	p.write(from(session.FIX44, 6), "0")
	p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
	p.write(from(session.FIX44, 3), "0", "43", "Y", "122", origSendingTime())
	p.expect("5")
	for {
		select {
		case ev := <-p.frames:
			if ev.err != nil {
				return
			}
			if m, err := fix.Parse(ev.frame); err == nil && string(m.MsgType()) == "2" {
				t.Fatalf("got %s after our Logout", visible(ev.frame))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("connection still open")
		}
	}
}

// By default, a counterparty's Logout filling the gap ahead of a
// message held stops what is held after it, as a Logout itself held does,
// the answer to ours too: the order after it is neither processed nor
// counted. A Heartbeat filling it lets the order through.
func TestHeldArrival_InSequenceLogoutStopsHeld(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ours   bool     // we log out first
		held   bool     // the Logout, 3, is held too, the order 4
		filler string   // the message 2
		apps   []string // delivered
		next   uint64   // NextTargetSeqNum after
	}{
		{"Heartbeat", false, false, "0", []string{"AFTER-LOGOUT"}, 4},
		{"counterparty's Logout", false, false, "5", nil, 3},
		{"answer to ours", true, false, "5", nil, 3},
		{"answer to ours, held", true, true, "0", nil, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec, p := loggedOn(t, nil)
			order := 3
			if tc.held {
				p.write(from(session.FIX44, 3), "5")
				p.expect("2", "7", "2", "16", "0")
				order = 4
			}
			p.write(from(session.FIX44, order), "D", "11", "AFTER-LOGOUT")
			if !tc.held {
				p.expect("2", "7", "2", "16", "0")
			}
			done := make(chan error, 1)
			if tc.ours {
				go func() { done <- s.Logout(context.Background(), "end of day") }()
				p.expect("5")
			}
			p.write(from(session.FIX44, 2), tc.filler, "43", "Y", "122", origSendingTime())
			if tc.filler == "0" && !tc.ours {
				rec.wait(t, func() bool { return len(rec.apps) == 1 })
			} else {
				if !tc.ours {
					p.expect("5")
				}
				rec.wait(t, func() bool { return len(rec.logouts) == 1 })
			}
			if tc.ours {
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("Logout: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Logout did not return")
				}
			}
			rec.mu.Lock()
			apps := append([]string(nil), rec.apps...)
			rec.mu.Unlock()
			if !slices.Equal(apps, tc.apps) {
				t.Errorf("delivered %q, want %q", apps, tc.apps)
			}
			if n := s.NextTargetSeqNum(); n != tc.next {
				t.Errorf("NextTargetSeqNum = %d, want %d", n, tc.next)
			}
		})
	}
}

// By default, a counterparty's Logout in sequence ending a chunk
// of a chunked ResendRequest is answered: the next chunk is not asked for.
func TestHeldArrival_InSequenceLogoutEndsChunk(t *testing.T) {
	_, _, p := loggedOn(t, func(c *session.Config) {
		c.ResendChunkSize = 2
	})
	p.write(from(session.FIX44, 5), "D", "11", "AFTER-LOGOUT")
	p.expect("2", "7", "2", "16", "3")
	p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
	p.write(from(session.FIX44, 3), "5")
	p.expect("5")
	p.expectSilentEnd()
}
