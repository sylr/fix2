package session_test

import (
	"testing"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// QuickFIXTolerations holds what QuickFIX's acceptance tests need, a fresh
// value every call; a zero Config tolerates nothing.
func TestQuickFIXTolerations(t *testing.T) {
	want := session.Tolerations{
		TolerateUnsupportedDefaultApplVerID: true,
		TolerateLogoutBeforeGapRecovery:     true,
	}
	got := session.QuickFIXTolerations()
	if got != want {
		t.Errorf("QuickFIXTolerations() = %+v, want %+v", got, want)
	}
	got.TolerateLogoutBeforeGapRecovery = false
	if !session.QuickFIXTolerations().TolerateLogoutBeforeGapRecovery {
		t.Error("QuickFIXTolerations shares its value")
	}
	var c session.Config
	if c.Tolerations != (session.Tolerations{}) {
		t.Errorf("a zero Config tolerates %+v", c.Tolerations)
	}
}

// A Logout ahead of a gap is answered once the gap is recovered, or, with
// TolerateLogoutBeforeGapRecovery, at once, as QuickFIX does.
func TestTolerateLogoutBeforeGapRecovery(t *testing.T) {
	t.Run("strict", func(t *testing.T) {
		_, rec, p := loggedOn(t, nil)
		p.write(from(session.FIX44, 3), "5")
		p.expect("2", "7", "2", "16", "0")
		p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
		p.expect("5")
		rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	})
	t.Run("tolerated", func(t *testing.T) {
		_, rec, p := loggedOn(t, func(c *session.Config) { c.TolerateLogoutBeforeGapRecovery = true })
		p.write(from(session.FIX44, 3), "5")
		p.expect("5")
		rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	})
}

// A ResendRequest whose MsgSeqNum is too low, without PossDupFlag, logs out
// unserved, or, with ServeResendRequestSeqTooLow, is served first, as
// QuickFIX does.
func TestServeResendRequestSeqTooLow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve bool
	}{{"default", false}, {"served", true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec, p := loggedOn(t, func(c *session.Config) { c.ServeResendRequestSeqTooLow = tc.serve })
			p.send("0", 0)
			p.send("2", 2, "7", "1", "16", "0")
			if tc.serve {
				// Our Logon, an admin message, gap-filled.
				p.expect("4", "34", "1", "43", "Y", "123", "Y", "36", "2")
			}
			p.expect("5")
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
		})
	}
}

// A PossDup SequenceReset without OrigSendingTime (122) is rejected, or, with
// TolerateSequenceResetWithoutOrigSendingTime, fills its gap.
func TestTolerateSequenceResetWithoutOrigSendingTime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := loggedOn(t, func(c *session.Config) { c.TolerateSequenceResetWithoutOrigSendingTime = tc.tolerate })
			p.write(from(session.FIX44, 5), "0")
			p.expect("2", "7", "2", "16", "0")
			p.write(from(session.FIX44, 2), "4", "43", "Y", "123", "Y", "36", "5")
			if !tc.tolerate {
				p.expect("3", "45", "2", "371", "122")
				return
			}
			p.write(from(session.FIX44, 6), "1", "112", "AFTER")
			p.expect("0", "112", "AFTER")
			if got := s.NextTargetSeqNum(); got != 7 {
				t.Errorf("NextTargetSeqNum = %d, want 7", got)
			}
		})
	}
}

// A FIXT Logon answer whose DefaultApplVerID has no dictionary is refused, or,
// with TolerateUnsupportedDefaultApplVerID, accepted, its messages read with
// our own default version.
func TestTolerateUnsupportedDefaultApplVerID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			l := mem.Listen("acceptor")
			_, rec := startInitiator(t, l, func(c *session.Config) {
				c.ID.BeginString = session.FIXT11
				strictApps(c)
				c.TolerateUnsupportedDefaultApplVerID = tc.tolerate
			})
			p := acceptPeer(t, l)
			p.expect("A")
			p.write(from(session.FIXT11, 1), "A", "98", "0", "108", "30", "1137", "7")
			if !tc.tolerate {
				p.expectSilentEnd()
				rec.mu.Lock()
				defer rec.mu.Unlock()
				if rec.logons != 0 {
					t.Error("logged on")
				}
				return
			}
			rec.wait(t, func() bool { return rec.logons == 1 })
			// Without ApplVerID, read as FIX 5.0 SP2, our default: a valid
			// order is delivered, one without Side (54) rejected for it.
			order := func(seq int, id string, side ...string) {
				f := append([]string{"11", id}, side...)
				f = append(f, "60", "20240101-00:00:00.000", "38", "3", "40", "1")
				p.write(from(session.FIXT11, seq), "D", f...)
			}
			order(2, "O1", "54", "1")
			rec.wait(t, func() bool { return len(rec.apps) == 1 && rec.apps[0] == "O1" })
			order(3, "O2")
			p.expect("3", "45", "3", "371", "54", "373", "1")
		})
	}
}
