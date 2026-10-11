package quickfixgo

import "testing"

// https://github.com/quickfixgo/quickfix/issues/369
//
// An acceptor receiving a Logon whose MsgSeqNum is too low closed the
// connection without a word: the initiator never learnt why. As QuickFIX C++
// does, it must answer with a Logout saying so, then disconnect.
func TestIssue369_TooLowLogonAnsweredWithLogout(t *testing.T) {
	_, rec, l := startAcceptor(t, nil)
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.send("D", 0, "11", "1")
	p.send("D", 0, "11", "2")
	rec.wait(t, "orders", func() bool { return len(rec.apps) == 2 })
	p.conn.Close()
	rec.wait(t, "logout", func() bool { return len(rec.logouts) == 1 })

	// An initiator that lost its state logs on at 1, 4 expected.
	p = dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("5", "58", "MsgSeqNum too low, expecting 4 but received 1")
	p.expectEnd()
}
