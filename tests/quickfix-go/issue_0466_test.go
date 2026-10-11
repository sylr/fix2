package quickfixgo

import (
	"context"
	"testing"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/466
//
// An acceptor serving only dynamic sessions no longer listened: listeners
// were opened for the configured sessions only. An acceptor with no session
// but AcceptorOptions.Resolve must serve its listeners and log the resolved
// sessions on, whether listening before or after Run starts.
func TestIssue466_DynamicSessionLogsOn(t *testing.T) {
	for _, late := range []bool{false, true} {
		rec := newRecorder()
		l := mem.Listen("acceptor")
		a := session.NewAcceptor(session.AcceptorOptions{
			Logger: testLogger(),
			Resolve: func(context.Context, *session.LogonRequest) (*session.Config, error) {
				cfg := accConfig(rec, nil)
				return &cfg, nil
			},
		})
		if !late {
			a.Listen(l)
		}
		run(t, a.Run)
		if late {
			a.Listen(l)
		}
		p := dialPeer(t, l)
		p.send("A", 0, "98", "0", "108", "30")
		p.expect("A", "49", "ACC", "56", "INI", "34", "1")
		rec.wait(t, "logon", func() bool { return rec.logons == 1 })
		if s := a.Session(accID); s == nil {
			t.Errorf("listening late %v: no dynamic session", late)
		} else if st := s.State(); st != session.StateLoggedOn {
			t.Errorf("listening late %v: dynamic session %s, want logged on", late, st)
		}
		p.send("D", 0, "11", "1")
		rec.wait(t, "the order", func() bool { return len(rec.apps) == 1 })
	}
}
