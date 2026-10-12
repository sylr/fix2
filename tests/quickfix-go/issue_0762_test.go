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
// Under CompatStrict2020 fix2 announces the reset whenever it moved the
// sequence numbers; by default it answers as QuickFIX, whose acceptance
// tests expect no ResetSeqNumFlag.
func TestIssue762_AcceptorResetOnLogonAnswersResetSeqNumFlag(t *testing.T) {
	_, rec, p := i762PriorState(t, session.CompatStrict2020)
	p.expect("A", "34", "1", "141", "Y")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
}

// By default, an acceptor with ResetOnLogon answers as QuickFIX does, without
// ResetSeqNumFlag, even when the reset moved its sequence numbers.
func TestIssue762_DefaultAcceptorResetOnLogonAnswersAsQuickFIX(t *testing.T) {
	_, rec, p := i762PriorState(t, session.CompatQuickFIX)
	if m := p.expect("A", "34", "1"); m.Get(141) != nil {
		t.Errorf("Logon answer with ResetSeqNumFlag %q", m.Get(141))
	}
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
}

// An acceptor with ResetOnLogon at the start of its sequence answers without
// ResetSeqNumFlag in either mode: nothing moved.
func TestIssue762_FreshAcceptorResetOnLogonAnswersWithoutResetSeqNumFlag(t *testing.T) {
	for _, compat := range []session.Compatibility{session.CompatQuickFIX, session.CompatStrict2020} {
		_, rec, l := startAcceptor(t, func(c *session.Config) { c.ResetOnLogon = true; c.Compatibility = compat })
		p := dialPeer(t, l)
		p.send("A", 0, "98", "0", "108", "30")
		if m := p.expect("A", "34", "1"); m.Get(141) != nil {
			t.Errorf("compatibility %d: Logon answer with ResetSeqNumFlag %q", compat, m.Get(141))
		}
		rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	}
}

// i762PriorState runs an acceptor with ResetOnLogon and compat whose
// previous session left the sequence numbers at 5, and sends it a Logon at
// 5 without ResetSeqNumFlag.
func i762PriorState(t *testing.T, compat session.Compatibility) (*session.Session, *recorder, *peer) {
	t.Helper()
	s, rec, l := startAcceptor(t, func(c *session.Config) { c.ResetOnLogon = true; c.Compatibility = compat })
	if err := s.SetNextSenderSeqNum(5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNextTargetSeqNum(5); err != nil {
		t.Fatal(err)
	}
	p := dialPeer(t, l)
	p.seq = 5
	p.send("A", 0, "98", "0", "108", "30")
	return s, rec, p
}
