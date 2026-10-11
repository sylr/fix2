package quickfixgo

import (
	"context"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/238
//
// An initiator whose Logon with ResetSeqNumFlag (141=Y) was answered with
// 141=Y reset its sequence numbers a second time on the answer, and sent its
// next message with MsgSeqNum 1: the acceptor, expecting 2, logged it out.
// Both Logons are MsgSeqNum 1 of the new sequence, what follows is 2.
func TestIssue238_NextMessageAfterResetLogonIsSeq2(t *testing.T) {
	s, rec, l := startInitiator(t, func(c *session.Config) { c.ResetOnLogon = true })
	p := acceptPeer(t, l)
	p.logon("141", "Y")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.send("D", 0, "11", "1")
	rec.wait(t, "order", func() bool { return len(rec.apps) == 1 })
	p.conn.Close()

	// Reconnected: the Logon resets the sequence numbers both ways.
	p = acceptPeer(t, l)
	p.expect("A", "34", "1", "141", "Y")
	p.send("A", 0, "98", "0", "108", "30", "141", "Y")
	rec.wait(t, "second logon", func() bool { return rec.logons == 2 })
	if _, err := s.Send(context.Background(), fix2.NewBuilder(session.FIX44, "D").SetString(11, "2")); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "2")
	p.send("D", 0, "11", "3")
	rec.wait(t, "order 2 of the new sequence", func() bool { return len(rec.apps) == 2 })
	p.expectNothing(50 * time.Millisecond)
}
