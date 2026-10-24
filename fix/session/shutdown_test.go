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

// shutdownAcceptor runs an acceptor of session INI->ACC with handler h,
// adminFields by default: the session, its recorder, the listener, the
// acceptor and a channel receiving Run's result.
func shutdownAcceptor(t *testing.T, h session.Handler) (*session.Session, *recorder, *mem.Listener, *session.Acceptor, <-chan error) {
	t.Helper()
	rec := newRecorder()
	switch v := h.(type) {
	case nil:
		h = adminFields{recorder: rec}
	case *blockingApp:
		v.recorder = rec
	}
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
	ctx, cancel := context.WithCancel(context.Background())
	ran, finished := make(chan error, 1), make(chan struct{})
	go func() { defer close(finished); ran <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	})
	return s, rec, l, a, ran
}

// logOnPeer dials l and logs a scripted counterparty on.
func logOnPeer(t *testing.T, l *mem.Listener, rec *recorder) *peer {
	t.Helper()
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	rec.wait(t, func() bool { return rec.logons == 1 })
	return p
}

// shutdownAsync calls a.ShutdownWithOptions in the background.
func shutdownAsync(a *session.Acceptor, o session.LogoutOptions) <-chan error {
	done := make(chan error, 1)
	go func() { done <- a.ShutdownWithOptions(context.Background(), o) }()
	return done
}

// waitErr waits for an error from ch.
func waitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
	}
	return nil
}

// Shutdown closes the listener, logs the sessions out with its text, the
// fields ToAdmin added kept, and returns once Run did.
func TestAcceptorShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, rec, l, a, ran := shutdownAcceptor(t, nil)
		p := logOnPeer(t, l, rec)
		done := make(chan error, 1)
		go func() { done <- a.Shutdown(context.Background(), "restart, reconnect in 2 minutes") }()
		p.expect("5", "58", "restart, reconnect in 2 minutes", "20001", "X")
		p.send("5", 0)
		if err := waitErr(t, done, "Shutdown"); err != nil {
			t.Fatalf("Shutdown = %v", err)
		}
		select {
		case err := <-ran:
			if err != nil {
				t.Errorf("Run = %v", err)
			}
		default:
			t.Error("Shutdown returned before Run")
		}
		waitDone(t, s)
		if _, err := l.Dial(context.Background()); err == nil {
			t.Error("listener still open")
		}
		if err := a.Shutdown(context.Background(), "again"); err == nil {
			t.Error("Shutdown of an acceptor not running = nil")
		}
	})
}

// ShutdownWithOptions' Fields replace those ToAdmin added: none if empty.
func TestAcceptorShutdownWithOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []fix.Field
		want   []string
	}{
		{"fields", []fix.Field{{Tag: 1409, Value: []byte("4")}}, []string{"1409", "4", "20001", ""}},
		{"none", []fix.Field{}, []string{"1409", "", "20001", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, rec, l, a, _ := shutdownAcceptor(t, nil)
				p := logOnPeer(t, l, rec)
				done := shutdownAsync(a, session.LogoutOptions{Text: "restart", Fields: tc.fields})
				p.expect("5", append([]string{"58", "restart"}, tc.want...)...)
				p.send("5", 0)
				if err := waitErr(t, done, "ShutdownWithOptions"); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// ShutdownWithOptions refuses Fields the session could not send, shutting
// nothing down: the session goes on.
func TestAcceptorShutdownRefusesFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field fix.Field
	}{
		{"Text", fix.Field{Tag: 58, Value: []byte("forged")}},
		{"MsgSeqNum", fix.Field{Tag: 34, Value: []byte("9")}},
		{"CheckSum", fix.Field{Tag: 10, Value: []byte("000")}},
		{"SOH", fix.Field{Tag: 20002, Value: []byte("a\x0158=forged")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, rec, l, a, ran := shutdownAcceptor(t, nil)
				p := logOnPeer(t, l, rec)
				err := a.ShutdownWithOptions(context.Background(), session.LogoutOptions{Fields: []fix.Field{tc.field}})
				if err == nil {
					t.Fatal("ShutdownWithOptions = nil")
				}
				p.send("1", 0, "112", "T1")
				p.expect("0", "112", "T1")
				select {
				case <-ran:
					t.Error("Run returned")
				default:
				}
			})
		})
	}
}

// A connection that has not logged on yet when Shutdown is called is closed
// without a word, while the logged on one is logged out.
func TestAcceptorShutdownPendingConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, rec, l, a, _ := shutdownAcceptor(t, nil)
		p := logOnPeer(t, l, rec)
		pending := dialPeer(t, l)
		synctest.Wait()
		done := shutdownAsync(a, session.LogoutOptions{Text: "restart"})
		pending.expectSilentEnd()
		p.expect("5", "58", "restart")
		p.send("5", 0)
		if err := waitErr(t, done, "Shutdown"); err != nil {
			t.Fatal(err)
		}
	})
}

// shutdownFromApp shuts its acceptor down from FromApp, with FromApp's ctx,
// and reports what Shutdown returned.
type shutdownFromApp struct {
	*recorder
	a   **session.Acceptor
	ret chan error
}

func (h shutdownFromApp) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
	h.ret <- (*h.a).Shutdown(ctx, "from FromApp")
	return h.recorder.FromApp(ctx, s, m, in)
}

// Shutdown called with a handler callback's ctx returns at once: Run stops
// the session once the callback returned.
func TestAcceptorShutdownFromCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var a *session.Acceptor
		h := shutdownFromApp{recorder: newRecorder(), a: &a, ret: make(chan error, 1)}
		s, _, l, acc, ran := shutdownAcceptor(t, h)
		a = acc
		p := logOnPeer(t, l, h.recorder)
		start := time.Now()
		p.send("D", 0, "11", "O1")
		if err := waitErr(t, h.ret, "Shutdown from FromApp"); err != nil {
			t.Fatalf("Shutdown = %v", err)
		}
		if waited := time.Since(start); waited > 0 {
			t.Errorf("Shutdown from FromApp returned after %s, want at once", waited)
		}
		p.expect("5", "58", "from FromApp")
		p.send("5", 0)
		if err := waitErr(t, ran, "Run"); err != nil {
			t.Fatalf("Run = %v", err)
		}
		waitDone(t, s)
	})
}

// A second Shutdown waits for the first, whose text the Logouts say; one
// whose ctx ends first returns its error while the shutdown goes on.
func TestAcceptorShutdownTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, rec, l, a, ran := shutdownAcceptor(t, nil)
		p := logOnPeer(t, l, rec)
		first := shutdownAsync(a, session.LogoutOptions{Text: "first"})
		p.expect("5", "58", "first")
		second := shutdownAsync(a, session.LogoutOptions{Text: "second"})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := a.Shutdown(ctx, "third"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown with a ctx ending first = %v, want DeadlineExceeded", err)
		}
		p.send("5", 0)
		for _, ch := range []<-chan error{first, second} {
			if err := waitErr(t, ch, "Shutdown"); err != nil {
				t.Errorf("Shutdown = %v", err)
			}
		}
		if err := waitErr(t, ran, "Run"); err != nil {
			t.Errorf("Run = %v", err)
		}
		p.expectEnd(time.Second)
	})
}

// A stuck FromApp holds neither Shutdown nor Run past the sessions' stop
// bound, LogoutTimeout plus a second; a ctx ending before ends Shutdown's
// wait. The session is done once FromApp returned.
func TestAcceptorShutdownStuckCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBlockingApp(t)
		s, _, l, a, ran := shutdownAcceptor(t, h)
		p := logOnPeer(t, l, h.recorder)
		p.send("D", 0, "11", "O1")
		h.waitEntered(t)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- a.Shutdown(ctx, "restart") }()
		p.expect("5", "58", "restart")
		p.send("5", 0)
		if err := waitErr(t, done, "Shutdown"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
		}
		if err := a.Shutdown(context.Background(), "again"); err != nil {
			t.Errorf("Shutdown waiting for Run = %v", err)
		}
		if err := waitErr(t, ran, "Run"); err != nil {
			t.Errorf("Run = %v", err)
		}
		select {
		case <-s.Done():
			t.Fatal("Done closed while FromApp runs")
		default:
		}
		h.unblock()
		waitDone(t, s)
	})
}

// A Logout's options replace the fields ToAdmin added on that Logout only,
// and options the session could not send fail without logging out.
func TestLogoutWithOptions(t *testing.T) {
	t.Run("fields", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, p := loggedOn(t, withAdminFields(adminFields{}))
			done := make(chan error, 1)
			go func() {
				done <- s.LogoutWithOptions(context.Background(), session.LogoutOptions{Text: "bye", Fields: []fix.Field{{Tag: 1409, Value: []byte("4")}}})
			}()
			p.expect("5", "58", "bye", "1409", "4", "20001", "")
			p.send("5", 0)
			if err := waitErr(t, done, "LogoutWithOptions"); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, p := loggedOn(t, withAdminFields(adminFields{}))
			err := s.LogoutWithOptions(context.Background(), session.LogoutOptions{Fields: []fix.Field{{Tag: 58, Value: []byte("forged")}}})
			var rerr *fix.ReservedTagError
			if !errors.As(err, &rerr) || rerr.Tag != 58 {
				t.Fatalf("LogoutWithOptions = %v, want a ReservedTagError for 58", err)
			}
			p.send("1", 0, "112", "T1")
			p.expect("0", "112", "T1")
		})
	})
}

// removeAndShutdown removes its session, then shuts its acceptor down, from
// ToAdmin, on the goroutine of the connection Run waits for.
type removeAndShutdown struct {
	*recorder
	a   **session.Acceptor
	ret chan error
}

func (h removeAndShutdown) ToAdmin(ctx context.Context, s *session.Session, msgType string, _ *fix.Builder) error {
	if msgType == "A" {
		(*h.a).Remove(ctx, s.ID())
		h.ret <- (*h.a).Shutdown(ctx, "from ToAdmin")
	}
	return nil
}

// Shutdown called from a callback of a session removed returns at once too:
// Run waits for the connection whose goroutine runs it.
func TestAcceptorShutdownFromRemovedSessionCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var a *session.Acceptor
		h := removeAndShutdown{recorder: newRecorder(), a: &a, ret: make(chan error, 1)}
		_, _, l, acc, ran := shutdownAcceptor(t, h)
		a = acc
		p := dialPeer(t, l)
		p.send("A", 0, "98", "0", "108", "30")
		if err := waitErr(t, h.ret, "Shutdown from ToAdmin"); err != nil {
			t.Fatalf("Shutdown = %v", err)
		}
		// The session was removed before ToAdmin returned: its Logon is
		// never answered.
		p.expectSilentEnd()
		if err := waitErr(t, ran, "Run"); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

// A listener added while Run shuts down is closed at once, not left bound.
func TestAcceptorListenDuringShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, rec, l, a, _ := shutdownAcceptor(t, nil)
		p := logOnPeer(t, l, rec)
		done := shutdownAsync(a, session.LogoutOptions{Text: "restart"})
		// Run waits for the answer to its Logout.
		p.expect("5", "58", "restart")
		late := mem.Listen("late")
		a.Listen(late)
		if _, err := late.Dial(context.Background()); err == nil {
			t.Error("listener added during the shutdown still open")
		}
		p.send("5", 0)
		if err := waitErr(t, done, "Shutdown"); err != nil {
			t.Fatal(err)
		}
	})
}
