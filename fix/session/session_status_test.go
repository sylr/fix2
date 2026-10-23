package session_test

import (
	"testing"
	"testing/synctest"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/tests/dicts/fixlatest"
	"github.com/sylr/fix2/transport/mem"
)

// statusDicts makes the session dictionary FIX Latest's, whose Logout has
// SessionStatus (1409).
func statusDicts(c *session.Config) { c.Dictionaries.Session = fixlatest.Dictionary() }

// count returns how many fields of m have tag.
func count(m *fix.Message, tag int) int {
	n := 0
	for _, f := range m.Fields() {
		if f.Tag == tag {
			n++
		}
	}
	return n
}

// The session sets SessionStatus on the Logouts it sends for a sequence
// number it cannot accept, where the session dictionary's Logout has the
// field: 9 for a MsgSeqNum too low, 10 for a NextExpectedMsgSeqNum too high.
func TestLogoutSessionStatus(t *testing.T) {
	t.Run("MsgSeqNum too low", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, p := fixtSession(t, "9", statusDicts)
			p.write(from(session.FIXT11, 1), "0")
			p.expect("5", "1409", "9")
		})
	})
	t.Run("NextExpectedMsgSeqNum too high", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := mem.Listen("acceptor")
			startInitiator(t, l, func(c *session.Config) {
				fixtApps(c)
				statusDicts(c)
				c.NextExpectedMsgSeqNum = true
			})
			p := acceptPeer(t, l)
			p.expect("A")
			p.write(from(session.FIXT11, 1), "A", "98", "0", "108", "30", "1137", "9", "789", "99")
			p.expect("5", "1409", "10")
		})
	})
	t.Run("not in the dictionary", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// FIXT11.xml's Logout has no SessionStatus.
			_, _, p := fixtSession(t, "9", nil)
			p.write(from(session.FIXT11, 1), "0")
			if m := p.expect("5"); m.Get(1409) != nil {
				t.Errorf("SessionStatus %q sent", m.Get(1409))
			}
		})
	})
}

// The SessionStatus the session sets replaces the one ToAdmin adds to the
// connection's Logouts, which those it does not set keep.
func TestLogoutSessionStatusOverridesToAdmin(t *testing.T) {
	handler := func(c *session.Config) {
		statusDicts(c)
		c.Handler = adminFields{recorder: c.Handler.(*dataRecorder).recorder, logout: func(b *fix.Builder) {
			b.SetString(1409, "4").SetString(20001, "X")
		}}
	}
	t.Run("ours", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, p := fixtSession(t, "9", handler)
			p.write(from(session.FIXT11, 1), "0")
			m := p.expect("5", "1409", "9", "20001", "X")
			if n := count(m, 1409); n != 1 {
				t.Errorf("%d SessionStatus fields, want 1", n)
			}
		})
	})
	t.Run("ToAdmin's", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, p := fixtSession(t, "9", handler)
			p.write(from(session.FIXT11, 2), "5")
			p.expect("5", "1409", "4", "20001", "X")
		})
	})
}
