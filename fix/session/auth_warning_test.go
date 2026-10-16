package session_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

const authWarning = "Logon accepted by BaseHandler's Authenticate, which accepts any Logon: implement Authenticate to authenticate counterparties"

// authenticating accepts the Logons with Username (553) "u", returning nil
// itself.
type authenticating struct{ *recorder }

func (authenticating) Authenticate(_ context.Context, req *session.LogonRequest) error {
	if string(req.Logon.Get(553)) != "u" {
		return errors.New("unknown user")
	}
	return nil
}

// delegating accepts the same Logons, through BaseHandler's Authenticate.
type delegating struct{ *recorder }

func (d delegating) Authenticate(ctx context.Context, req *session.LogonRequest) error {
	if string(req.Logon.Get(553)) != "u" {
		return errors.New("unknown user")
	}
	return d.recorder.BaseHandler.Authenticate(ctx, req)
}

// authAcceptor returns an acceptor's session, its handler h(rec), its
// listener and how many times it warned of Logons accepted unauthenticated.
func authAcceptor(t *testing.T, h func(*recorder) session.Handler) (*recorder, *mem.Listener, *atomic.Int64) {
	t.Helper()
	warned := new(atomic.Int64)
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	rec := newRecorder()
	if _, err := a.Add(session.Config{
		// The peer writes as ACC to INI.
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler: h(rec),
		Logger:  slog.New(countLog{msg: authWarning, n: warned}),
	}); err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return rec, l, warned
}

// logOnTo connects to l and logs on with Username user, the sequence
// numbers reset, then logs out.
func logOnTo(t *testing.T, rec *recorder, l *mem.Listener, user string, logons int) {
	t.Helper()
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30", "141", "Y", "553", user)
	p.expect("A")
	rec.wait(t, func() bool { return rec.logons == logons })
	p.send("5", 0)
	p.expect("5")
	p.conn.Close()
}

// An acceptor whose handler keeps BaseHandler's Authenticate warns once it
// logs on, once.
func TestAuthWarning_Default(t *testing.T) {
	rec, l, warned := authAcceptor(t, func(r *recorder) session.Handler { return r })
	logOnTo(t, rec, l, "", 1)
	if n := warned.Load(); n != 1 {
		t.Errorf("warned %d times, want once", n)
	}
	logOnTo(t, rec, l, "", 2)
	if n := warned.Load(); n != 1 {
		t.Errorf("warned %d times after a second Logon, want once", n)
	}
}

// A Logon refused after BaseHandler's Authenticate accepted it does not
// use up the warning, which the next Logon logging on gets.
func TestAuthWarning_RefusedLogon(t *testing.T) {
	rec, l, warned := authAcceptor(t, func(r *recorder) session.Handler { return r })
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0") // without HeartBtInt: refused
	p.expectEnd(5 * time.Second)
	if n := warned.Load(); n != 0 {
		t.Errorf("warned %d times for a Logon refused, want never", n)
	}
	logOnTo(t, rec, l, "", 1)
	if n := warned.Load(); n != 1 {
		t.Errorf("warned %d times, want once", n)
	}
}

// An acceptor whose handler authenticates does not warn; one delegating to
// BaseHandler's Authenticate does, as it cannot be told from one that
// does not authenticate.
func TestAuthWarning_Implemented(t *testing.T) {
	rec, l, warned := authAcceptor(t, func(r *recorder) session.Handler { return authenticating{r} })
	logOnTo(t, rec, l, "u", 1)
	if n := warned.Load(); n != 0 {
		t.Errorf("authenticating handler: warned %d times, want never", n)
	}
	rec, l, warned = authAcceptor(t, func(r *recorder) session.Handler { return delegating{r} })
	logOnTo(t, rec, l, "u", 1)
	if n := warned.Load(); n != 1 {
		t.Errorf("delegating handler: warned %d times, want once", n)
	}
}
