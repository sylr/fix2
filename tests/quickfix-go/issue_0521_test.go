package quickfixgo

import (
	"context"
	"testing"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/521
//
// The same as #466: dynamic sessions could not log on. With a configured
// session and Resolve on the same listener, both kinds must log on side by
// side: the configured one matched, the others resolved.
func TestIssue521_DynamicAndStaticSessionsLogOn(t *testing.T) {
	static := newRecorder()
	dynamic := newRecorder()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{
		Logger: testLogger(),
		Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			cfg := accConfig(dynamic, func(c *session.Config) { c.ID = req.ID })
			return &cfg, nil
		},
	})
	if _, err := a.Add(accConfig(static, nil)); err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	run(t, a.Run)

	for n, sender := range []string{"INI", "DYN1", "DYN2"} {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		conn, err := l.Dial(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		p := newPeer(t, conn, session.FIX44, sender, "ACC")
		p.send("A", 0, "98", "0", "108", "30")
		p.expect("A", "49", "ACC", "56", sender)
		if n == 0 {
			static.wait(t, "static logon", func() bool { return static.logons == 1 })
		} else {
			dynamic.wait(t, "dynamic logon", func() bool { return dynamic.logons == n })
		}
	}
	if got := len(a.Sessions()); got != 3 {
		t.Errorf("%d sessions, want 3", got)
	}
}
