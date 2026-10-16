package quickfixgo

import (
	"context"
	"testing"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/484
//
// Dynamic sessions could only use the in-memory store: the file and SQL
// store factories refused a session missing from the settings. The Store of
// the Config AcceptorOptions.Resolve returns must be the one the session
// uses: here one persisted from an earlier run, at sequence number 5.
func TestIssue484_DynamicSessionUsesResolvedStore(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	state, err := st.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNextSender(ctx, state.Generation, 5); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{
		Logger: testLogger(),
		Resolve: func(context.Context, *session.LogonRequest) (*session.Config, error) {
			cfg := accConfig(rec, func(c *session.Config) { c.Store = st })
			return &cfg, nil
		},
	})
	a.Listen(l)
	run(t, a.Run)
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A", "34", "5")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	if next, err := st.Open(ctx); err != nil || next.NextSender != 6 {
		t.Errorf("store: NextSender %d (%v), want 6: our Logon stored there", next.NextSender, err)
	}
}
