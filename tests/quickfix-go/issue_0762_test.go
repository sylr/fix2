package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/762
//
// An acceptor with ResetOnLogon resets its sequence numbers on every Logon,
// but answered without ResetSeqNumFlag (141=Y) unless the initiator's Logon
// carried it: the initiator was never told its counterparty started over.
func TestIssue762_AcceptorResetOnLogonAnswersResetSeqNumFlag(t *testing.T) {
	knownIssue(t, "as QuickFIX/Go and QuickFIX C++, an acceptor resetting for ResetOnLogon only echoes the initiator's ResetSeqNumFlag")
	_, rec, l := startAcceptor(t, func(c *session.Config) { c.ResetOnLogon = true })
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A", "34", "1", "141", "Y")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
}
