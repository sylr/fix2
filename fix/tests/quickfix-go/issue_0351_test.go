package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/351
//
// An initiator with ResetOnLogon reset its own sequence numbers but sent its
// Logon without ResetSeqNumFlag (141=Y): the acceptor was never asked to
// reset too.
func TestIssue351_InitiatorResetOnLogonSendsResetSeqNumFlag(t *testing.T) {
	_, _, l := startInitiator(t, func(c *session.Config) { c.ResetOnLogon = true })
	p := acceptPeer(t, l)
	p.expect("A", "34", "1", "141", "Y")
}
