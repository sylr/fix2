package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/520
//
// RejectInvalidMessage=N, which turns field validation off, was honoured
// for FIX 4.x messages but not for FIXT application messages, validated
// regardless. fix2's SkipChecks must apply to them as well.
func TestIssue520_SkipChecksAppliesToFIXT(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip dictionary.Checks
	}{{"checks", 0}, {"SkipChecks", dictionary.CheckAll}} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec, l := startAcceptor(t, func(c *session.Config) {
				c.ID = session.SessionID{BeginString: session.FIXT11, SenderCompID: "ACC", TargetCompID: "INI"}
				c.Dictionaries = session.Dictionaries{
					Session:          dict(t, "FIXT11"),
					Apps:             map[string]*dictionary.Dict{"9": dict(t, "FIX50SP2")},
					DefaultApplVerID: "9",
				}
				c.SkipChecks = tc.skip
			})
			p := dialPeer(t, l)
			p.beginString = session.FIXT11
			p.send("A", 0, "98", "0", "108", "30", "1137", "9")
			p.expect("A")
			// OrderQty (38) is not a QTY.
			p.send("D", 0, "1128", "9", "11", "1", "55", "EUR/USD", "54", "1", "60", "20240101-00:00:00.000", "38", "abc", "40", "1")
			if tc.skip == 0 {
				p.expect("3", "45", "2", "371", "38", "373", "6")
				return
			}
			rec.wait(t, "order", func() bool { return len(rec.apps) == 1 })
		})
	}
}
