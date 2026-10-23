package session_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// adminFields is a recorder whose ToAdmin adds a Username to Logons and
// field 20001 to Logouts, or sets logout's fields on Logouts, or fails for
// fail's MsgType.
type adminFields struct {
	*recorder
	logout func(*fix.Builder)
	fail   string
}

func (h adminFields) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if msgType == h.fail {
		return errors.New("refused")
	}
	switch {
	case msgType == "A":
		b.SetString(553, "trader")
	case h.logout != nil:
		h.logout(b)
	default:
		b.SetString(20001, "X")
	}
	return nil
}

// withAdminFields sets an adminFields handler on c.
func withAdminFields(h adminFields) func(*session.Config) {
	return func(c *session.Config) {
		h.recorder = c.Handler.(*recorder)
		c.Handler = h
	}
}

// The fields ToAdmin adds to a Logout go on every Logout of the connection:
// ours, and the answer to the counterparty's.
func TestToAdminLogoutFields(t *testing.T) {
	t.Run("ours", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, p := loggedOn(t, withAdminFields(adminFields{}))
			done := make(chan error, 1)
			go func() { done <- s.Logout(context.Background(), "bye") }()
			p.expect("5", "58", "bye", "20001", "X")
			p.send("5", 0)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, p := loggedOn(t, withAdminFields(adminFields{}))
			p.send("5", 0)
			if m := p.expect("5", "20001", "X"); m.Get(58) != nil {
				t.Errorf("answer has Text %q", m.Get(58))
			}
		})
	})
	t.Run("sequence too low", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, p := loggedOn(t, withAdminFields(adminFields{}))
			p.send("0", 1)
			p.expect("5", "20001", "X")
		})
	})
}

// A Logout's Text is the reason logging out: a ToAdmin setting it on a
// Logout cancels the Logon, as its error does.
func TestToAdminLogoutTextRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := mem.Listen("acceptor")
		startInitiator(t, l, withAdminFields(adminFields{logout: func(b *fix.Builder) { b.SetString(58, "forged") }}))
		acceptPeer(t, l).expectNoLogon()
	})
}

// An initiator's ToAdmin failing for a Logout cancels the Logon.
func TestToAdminLogoutErrorCancelsLogon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := mem.Listen("acceptor")
		startInitiator(t, l, withAdminFields(adminFields{fail: "5"}))
		acceptPeer(t, l).expectNoLogon()
	})
}

// startAcceptorWith runs an acceptor of session INI->ACC, as dialPeer's
// counterparty sees it, with h as its handler.
func startAcceptorWith(t *testing.T, h adminFields) (*session.Session, *recorder, *mem.Listener) {
	t.Helper()
	rec := newRecorder()
	h.recorder = rec
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	s, err := a.Add(session.Config{
		ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler:       h,
		LogoutTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	runUntilCleanup(t, a.Run)
	return s, rec, l
}

// An acceptor adds the fields ToAdmin gives a Logout to its Logouts, and
// refuses the connection without a word when ToAdmin fails for one.
func TestAcceptorToAdminLogout(t *testing.T) {
	t.Run("fields", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, rec, l := startAcceptorWith(t, adminFields{})
			p := dialPeer(t, l)
			p.send("A", 0, "98", "0", "108", "30")
			p.expect("A", "553", "trader")
			rec.wait(t, func() bool { return rec.logons == 1 })
			p.send("5", 0)
			p.expect("5", "20001", "X")
		})
	})
	t.Run("error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, l := startAcceptorWith(t, adminFields{fail: "5"})
			p := dialPeer(t, l)
			p.send("A", 0, "98", "0", "108", "30")
			p.expectSilentEnd()
		})
	})
}

// The answer to a Logon resetting the sequence numbers on an established
// connection carries the fields ToAdmin adds to a Logon, as the first
// answer does.
func TestResetAnswerToAdminFields(t *testing.T) {
	t.Run("acceptor", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, rec, l := startAcceptorWith(t, adminFields{})
			p := dialPeer(t, l)
			p.send("A", 0, "98", "0", "108", "30")
			p.expect("A", "553", "trader")
			rec.wait(t, func() bool { return rec.logons == 1 })
			p.seq = 1
			p.send("A", 0, "98", "0", "108", "30", "141", "Y")
			p.expect("A", "34", "1", "141", "Y", "553", "trader")
		})
	})
	t.Run("initiator", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, p := loggedOn(t, withAdminFields(adminFields{}))
			p.seq = 1
			p.send("A", 0, "98", "0", "108", "30", "141", "Y")
			p.expect("A", "34", "1", "141", "Y", "553", "trader")
		})
	})
}
