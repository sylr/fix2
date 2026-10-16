package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/716
//
// With EnableNextExpectedMsgSeqNum, an initiator's Logon announced
// NextExpectedMsgSeqNum (789) one past the MsgSeqNum it expects: 2 on a first
// Logon. 789 is the next MsgSeqNum expected: 1 on a first Logon, and on an
// acceptor's answer, sent once the initiator's Logon is processed, 2.
func TestIssue716_InitiatorLogonAnnouncesNextExpected(t *testing.T) {
	_, rec, l := startInitiator(t, func(c *session.Config) { c.NextExpectedMsgSeqNum = true })
	p := acceptPeer(t, l)
	p.expect("A", "34", "1", "789", "1")
	p.send("A", 0, "98", "0", "108", "30", "789", "2")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.send("D", 0, "11", "1")
	p.send("D", 0, "11", "2")
	rec.wait(t, "orders", func() bool { return len(rec.apps) == 2 })

	// Reconnected, having received messages 1 to 3.
	p.conn.Close()
	p = acceptPeer(t, l)
	p.expect("A", "34", "2", "789", "4")
}

func TestIssue716_AcceptorLogonAnnouncesNextExpected(t *testing.T) {
	_, rec, l := startAcceptor(t, func(c *session.Config) { c.NextExpectedMsgSeqNum = true })
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30", "789", "1")
	p.expect("A", "34", "1", "789", "2")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
}
