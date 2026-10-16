package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/127
//
// An initiator configured without a data dictionary panicked on the first
// application message it received: its validator dereferenced the missing
// dictionary. Without dictionaries, messages are not validated, and
// delivered.
func TestIssue127_NoDictionaryDelivers(t *testing.T) {
	t.Run("FIX.4.4", func(t *testing.T) {
		_, rec, p := loggedOn(t, nil)
		p.send("D", 0, "11", "1", "55", "EUR/USD", "54", "1", "60", "20240101-00:00:00.000", "40", "1")
		rec.wait(t, "order", func() bool { return len(rec.apps) == 1 })
	})
	t.Run("FIXT.1.1", func(t *testing.T) {
		_, rec, l := startInitiator(t, func(c *session.Config) {
			c.ID = session.SessionID{BeginString: session.FIXT11, SenderCompID: "INI", TargetCompID: "ACC"}
			c.Dictionaries.DefaultApplVerID = "9"
		})
		p := acceptPeer(t, l)
		p.beginString = session.FIXT11
		p.logon("1137", "9")
		rec.wait(t, "logon", func() bool { return rec.logons == 1 })
		p.send("D", 0, "11", "1", "55", "EUR/USD", "54", "1", "60", "20240101-00:00:00.000", "40", "1")
		rec.wait(t, "order", func() bool { return len(rec.apps) == 1 })
	})
}
