package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/345
//
// After a Logon with ResetSeqNumFlag (141=Y) answered with 141=Y, the
// initiator, which had set the flag itself in ToAdmin, reset again on the
// answer and sent its next message, a Heartbeat, with MsgSeqNum 1. Per FIX,
// both Logons are MsgSeqNum 1 and new messages continue from 2, on both
// sides.
func TestIssue345_InitiatorContinuesAtSeq2AfterResetLogon(t *testing.T) {
	_, rec, l := startInitiator(t, func(c *session.Config) { c.ResetOnLogon = true })
	p := acceptPeer(t, l)
	p.expect("A", "34", "1", "141", "Y")
	p.send("A", 0, "98", "0", "108", "30", "141", "Y")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.send("1", 0, "112", "hb")
	p.expect("0", "34", "2", "112", "hb")
}

func TestIssue345_AcceptorContinuesAtSeq2AfterResetLogon(t *testing.T) {
	_, rec, l := startAcceptor(t, nil)
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30", "141", "Y")
	p.expect("A", "34", "1", "141", "Y")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.send("1", 0, "112", "hb")
	p.expect("0", "34", "2", "112", "hb")
}
