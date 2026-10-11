package quickfixgo

import (
	"context"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/458
//
// A Logout answering the initiator's Logon was dropped before reaching the
// application: its Text, saying why the Logon failed (a MsgSeqNum too low,
// bad credentials), was lost, and the application could not act on it. It
// must reach the handler (FromAdmin, as fix2 documents), early enough for
// the application to fix its MsgSeqNum before the next attempt.

// i458Handler moves the next MsgSeqNum to 7 when told it is too low.
type i458Handler struct {
	*recorder
}

func (h i458Handler) FromAdmin(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) {
	h.recorder.FromAdmin(ctx, s, m, in)
	if string(m.MsgType()) == "5" && string(m.Get(58)) == "MsgSeqNum too low, expecting 7 but received 1" {
		s.SetNextSenderSeqNum(7)
	}
}

func TestIssue458_LogoutAnsweringLogonReachesHandler(t *testing.T) {
	h := i458Handler{newRecorder()}
	_, _, l := startInitiator(t, func(c *session.Config) { c.Handler = h })
	p := acceptPeer(t, l)
	p.expect("A", "34", "1")
	p.send("5", 0, "58", "MsgSeqNum too low, expecting 7 but received 1")
	h.wait(t, "the Logout", func() bool {
		return len(h.admins) == 1 && string(h.admins[0].MsgType()) == "5" &&
			string(h.admins[0].Get(58)) == "MsgSeqNum too low, expecting 7 but received 1"
	})
	p.expectEnd()

	p = acceptPeer(t, l)
	p.expect("A", "34", "7")
}
