package quickfixgo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/173
//
// QuickFIX C++ and J store a message sent while not logged on and send it
// with the resends of the next logon. fix2 refuses it instead, as
// ErrNotLoggedOn documents: the sender is told, and nothing is stored or sent
// behind its back.
func TestIssue173_SendNotLoggedOnRefused(t *testing.T) {
	s, rec, l := startInitiator(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := s.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); !errors.Is(err, session.ErrNotLoggedOn) {
		t.Fatalf("Send before logon = %v, want ErrNotLoggedOn", err)
	}
	p := acceptPeer(t, l)
	m := p.logon()
	if seq := string(m.Get(34)); seq != "1" {
		t.Errorf("Logon MsgSeqNum %s, want 1: the refused message took no sequence number", seq)
	}
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.expectNothing(100 * time.Millisecond)
}
