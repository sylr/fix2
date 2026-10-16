package quickfixgo

import (
	"errors"
	"testing"

	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/38
//
// OnLogout was only called on a Logout: a counterparty hanging up left the
// application believing the session up. A disconnection must be reported
// too.
func TestIssue38_OnLogoutWhenCounterpartyHangsUp(t *testing.T) {
	_, rec, p := loggedOn(t, nil)
	p.conn.Close()
	rec.wait(t, "OnLogout", func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	err := rec.logouts[0]
	rec.mu.Unlock()
	var le *session.LogoutError
	if !errors.As(err, &le) || le.ByCounterparty {
		t.Errorf("OnLogout reason %v, want a *LogoutError not by the counterparty", err)
	}
}
