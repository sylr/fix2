package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/379
//
// With ResetOnLogout, messages the application sent while the Logout was
// being processed took sequence numbers after the reset: the next Logon went
// out with MsgSeqNum 2, then 3, and the acceptor, expecting 1, asked for a
// resend the initiator refused. Whatever is sent around a logout, the next
// Logon is MsgSeqNum 1.
func TestIssue379_ResetOnLogoutNextLogonIsSeq1(t *testing.T) {
	for _, tc := range []struct {
		name   string
		logout func(t *testing.T, s *session.Session, p *peer)
	}{
		{"counterparty logs out", func(t *testing.T, s *session.Session, p *peer) {
			p.send("5", 0, "58", "server terminated")
			p.skipUntil("5")
		}},
		{"we log out", func(t *testing.T, s *session.Session, p *peer) {
			go s.Logout(context.Background(), "bye")
			p.skipUntil("5")
			p.send("5", 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec, l := startInitiator(t, func(c *session.Config) { c.ResetOnLogout = true })
			p := acceptPeer(t, l)
			p.logon()
			rec.wait(t, "logon", func() bool { return rec.logons == 1 })

			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				for {
					select {
					case <-stop:
						return
					default:
					}
					s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "x"))
					time.Sleep(100 * time.Microsecond)
				}
			}()
			defer func() { close(stop); <-done }()
			p.skipUntil("D")
			tc.logout(t, s, p)
			p.expectEnd()

			p = acceptPeer(t, l)
			p.expect("A", "34", "1")
		})
	}
}
