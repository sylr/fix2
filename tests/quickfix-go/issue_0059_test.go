package quickfixgo

import (
	"context"
	"errors"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/59
//
// An initiator whose counterparty went away panicked on the next send. Sends
// racing the disconnection, and those after it, must fail with an error.
func TestIssue59_SendAfterCounterpartyGoneFails(t *testing.T) {
	s, rec, p := loggedOn(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan error)
	go func() {
		for {
			if _, err := s.Send(ctx, fix2.NewBuilder(session.FIX44, "D").SetString(11, "O1")); err != nil {
				done <- err
				return
			}
		}
	}()
	p.conn.Close()
	rec.wait(t, "OnLogout", func() bool { return len(rec.logouts) == 1 })
	if err := <-done; !errors.Is(err, session.ErrNotLoggedOn) {
		t.Errorf("Send racing the disconnection = %v, want ErrNotLoggedOn", err)
	}
	if _, err := s.Send(ctx, fix2.NewBuilder(session.FIX44, "D").SetString(11, "O2")); !errors.Is(err, session.ErrNotLoggedOn) {
		t.Errorf("Send after the disconnection = %v, want ErrNotLoggedOn", err)
	}
}
