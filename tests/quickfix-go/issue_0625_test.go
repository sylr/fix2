package quickfixgo

import (
	"context"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/625
//
// An initiator with ResetOnLogon reset its sequence numbers on its first
// Logon only: reconnecting after the acceptor dropped the connection, it
// logged on where it had left off. It must reset on every Logon, as QuickFIX/J
// and QuickFIX C++ do.
func TestIssue625_InitiatorResetOnLogonResetsOnEveryLogon(t *testing.T) {
	s, rec, l := startInitiator(t, func(c *session.Config) { c.ResetOnLogon = true })
	p := acceptPeer(t, l)
	p.logon("141", "Y")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.send("D", 0, "11", "1")
	p.send("D", 0, "11", "2")
	rec.wait(t, "orders", func() bool { return len(rec.apps) == 2 })
	if _, err := s.Send(context.Background(), fix2.NewBuilder(session.FIX44, "D").SetString(11, "3")); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "2")

	// The acceptor drops the connection, without a Logout.
	p.conn.Close()
	p = acceptPeer(t, l)
	p.expect("A", "34", "1", "141", "Y")
}
