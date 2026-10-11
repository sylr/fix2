package session_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport/mem"
)

// logs is a slog.Handler sending each record's message, and its error if
// any, to a channel.
type logs chan string

func (l logs) Enabled(context.Context, slog.Level) bool { return true }
func (l logs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l logs) WithGroup(string) slog.Handler            { return l }

func (l logs) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			msg += ": " + a.Value.String()
		}
		return true
	})
	select {
	case l <- msg:
	default:
	}
	return nil
}

// wait waits for a message containing want.
func (l logs) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-l:
			if strings.Contains(msg, want) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a log containing %q", want)
		}
	}
}

var (
	dynAccID = session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"}
	dynIniID = session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
)

// dynamic runs an acceptor with no session, opts.Resolve creating them, and
// returns it with its listener and log.
func dynamic(t *testing.T, opts session.AcceptorOptions) (*session.Acceptor, *mem.Listener, logs) {
	t.Helper()
	l := mem.Listen("acceptor")
	log := make(logs, 100)
	opts.Logger = slog.New(log)
	a := session.NewAcceptor(opts)
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return a, l, log
}

// initiate runs an initiator of session INI->ACC on l, as many times as
// given, each its own Initiator, and returns their recorders and their
// sessions' log.
func initiate(t *testing.T, l *mem.Listener, n int) ([]*recorder, logs) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	log := make(logs, 100)
	var rs []*recorder
	for range n {
		i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 20 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
		r := newRecorder()
		if _, err := i.Add(session.Config{ID: dynIniID, Handler: r, Logger: slog.New(log)}, l); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() { defer wg.Done(); i.Run(ctx) }()
		rs = append(rs, r)
	}
	return rs, log
}

func TestDynamicSession(t *testing.T) {
	ar := newRecorder()
	var calls atomic.Int32
	var got session.LogonRequest
	a, l, _ := dynamic(t, session.AcceptorOptions{
		Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			calls.Add(1)
			got = *req
			got.Logon = req.Logon.Clone()
			return &session.Config{ID: req.ID, Handler: ar}, nil
		},
	})
	irs, _ := initiate(t, l, 1)
	ir := irs[0]
	ir.wait(t, func() bool { return ir.logons == 1 })
	ar.wait(t, func() bool { return ar.logons == 1 })

	if got.ID != dynAccID {
		t.Errorf("LogonRequest.ID = %v, want %v", got.ID, dynAccID)
	}
	if mt := string(got.Logon.MsgType()); mt != "A" {
		t.Errorf("LogonRequest.Logon is a %q, want a Logon", mt)
	}
	if got.RemoteAddr == nil || got.Response {
		t.Errorf("LogonRequest RemoteAddr = %v, Response = %v", got.RemoteAddr, got.Response)
	}
	acc := a.Session(dynAccID)
	if acc == nil {
		t.Fatalf("no session %v, sessions %v", dynAccID, a.Sessions())
	}
	if n := len(a.Sessions()); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}

	// The session is the acceptor's own: it is sent to and receives.
	ctx := context.Background()
	r, err := acc.Send(ctx, fix2.NewBuilder(session.FIX44, "8").SetString(11, "EXEC-1"))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.Wait(wctx, session.Acknowledged); err != nil {
		t.Fatalf("Wait(Acknowledged): %v", err)
	}
	ir.wait(t, func() bool { return len(ir.apps) == 1 && ir.apps[0] == "EXEC-1" })
	if n := calls.Load(); n != 1 {
		t.Errorf("Resolve called %d times, want 1", n)
	}
}

func TestDynamicSessionKeptAcrossConnections(t *testing.T) {
	ar := newRecorder()
	var calls atomic.Int32
	a, l, _ := dynamic(t, session.AcceptorOptions{
		Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			calls.Add(1)
			return &session.Config{ID: req.ID, Handler: ar}, nil
		},
	})
	irs, _ := initiate(t, l, 1)
	ir := irs[0]
	ar.wait(t, func() bool { return ar.logons == 1 })
	ir.wait(t, func() bool { return ir.logons == 1 })
	acc := a.Session(dynAccID)
	next := acc.NextTargetSeqNum()

	// Logged out, the initiator logs on again: the acceptor's session is
	// still there, its sequence numbers too.
	if err := acc.Logout(context.Background(), "come back"); err != nil {
		t.Fatal(err)
	}
	ar.wait(t, func() bool { return ar.logons == 2 })
	if s := a.Session(dynAccID); s != acc {
		t.Fatalf("session %v replaced on reconnecting", dynAccID)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("Resolve called %d times, want 1", n)
	}
	// Logout and Logon took a number each.
	if got := acc.NextTargetSeqNum(); got != next+2 {
		t.Errorf("next target seq = %d, want %d: sequence numbers reset", got, next+2)
	}

	// Once removed, it is resolved anew on the next Logon.
	if err := a.Remove(context.Background(), dynAccID); err != nil {
		t.Fatal(err)
	}
	ar.wait(t, func() bool { return ar.logons == 3 })
	if s := a.Session(dynAccID); s == nil || s == acc {
		t.Errorf("session after removal = %p, removed %p", s, acc)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("Resolve called %d times, want 2", n)
	}
}

func TestDynamicSessionRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    session.AcceptorOptions
		log     string
		created bool // the session stays, its connection refused
	}{{
		name: "error",
		opts: session.AcceptorOptions{Resolve: func(context.Context, *session.LogonRequest) (*session.Config, error) {
			return nil, errors.New("unknown firm")
		}},
		log: "dynamic session refused: unknown firm",
	}, {
		name: "no config",
		opts: session.AcceptorOptions{Resolve: func(context.Context, *session.LogonRequest) (*session.Config, error) {
			return nil, nil
		}},
		log: "no session for this Logon",
	}, {
		name: "no Resolve",
		log:  "no session for this Logon",
	}, {
		name: "other identities",
		opts: session.AcceptorOptions{Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			id := req.ID
			id.TargetCompID = "SOMEONE-ELSE"
			return &session.Config{ID: id, Handler: session.BaseHandler{}}, nil
		}},
		log: "does not match the Logon's identities",
	}, {
		name: "invalid config",
		opts: session.AcceptorOptions{Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			return &session.Config{ID: req.ID, Handler: session.BaseHandler{}, HeartBtInt: 1500 * time.Millisecond}, nil
		}},
		log: "HeartBtInt",
	}, {
		name: "Route",
		opts: session.AcceptorOptions{
			Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
				return &session.Config{ID: req.ID, Handler: session.BaseHandler{}}, nil
			},
			Route: func(context.Context, *session.LogonRequest, []session.SessionID) (session.SessionID, error) {
				return session.SessionID{}, errors.New("wrong certificate")
			},
		},
		log:     "routing refused: wrong certificate",
		created: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			a, l, log := dynamic(t, tc.opts)
			irs, ilog := initiate(t, l, 1)
			log.wait(t, tc.log)
			// The connection is closed: the initiator tries again.
			ilog.wait(t, "connection ended, reconnecting")
			if n := len(a.Sessions()); n != 0 && !tc.created || n != 1 && tc.created {
				t.Errorf("%d sessions after a refusal, created %v", n, tc.created)
			}
			ir := irs[0]
			ir.mu.Lock()
			defer ir.mu.Unlock()
			if ir.logons != 0 {
				t.Errorf("initiator logged on %d times", ir.logons)
			}
		})
	}
}

// A session Resolve creates is routed as any other: Route is asked of its
// first connection too.
func TestDynamicSessionRouted(t *testing.T) {
	ar := newRecorder()
	routed := make(chan []session.SessionID, 10)
	_, l, _ := dynamic(t, session.AcceptorOptions{
		Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			return &session.Config{ID: req.ID, Handler: ar}, nil
		},
		Route: func(_ context.Context, _ *session.LogonRequest, candidates []session.SessionID) (session.SessionID, error) {
			routed <- candidates
			return candidates[0], nil
		},
	})
	initiate(t, l, 1)
	ar.wait(t, func() bool { return ar.logons == 1 })
	select {
	case got := <-routed:
		if len(got) != 1 || got[0] != dynAccID {
			t.Errorf("Route asked of %v, want [%v]", got, dynAccID)
		}
	default:
		t.Fatal("Route not asked of the first connection")
	}
}

// Two connections logging on at once to a session no one has yet: the
// second waits for the first's Resolve, which is called once, then is
// refused as a second connection to a logged on session.
func TestDynamicSessionRace(t *testing.T) {
	ar := newRecorder()
	var calls atomic.Int32
	release := make(chan struct{})
	a, l, log := dynamic(t, session.AcceptorOptions{
		Resolve: func(ctx context.Context, req *session.LogonRequest) (*session.Config, error) {
			calls.Add(1)
			// Or the acceptor stops, should the test fail first.
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &session.Config{ID: req.ID, Handler: ar}, nil
		},
	})
	irs, _ := initiate(t, l, 2)
	log.wait(t, "Logon waiting for another connection's dynamic session")
	close(release)
	ar.wait(t, func() bool { return ar.logons == 1 })
	log.wait(t, "connection refused: session "+dynAccID.String()+": session: already connected")

	if n := calls.Load(); n != 1 {
		t.Errorf("Resolve called %d times, want 1", n)
	}
	if n := len(a.Sessions()); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
	waitFor(t, func() bool {
		logons := 0
		for _, r := range irs {
			r.mu.Lock()
			logons += r.logons
			r.mu.Unlock()
		}
		return logons == 1
	})
	// The session's store is the one Resolve gave, still open.
	r, err := a.Session(dynAccID).Send(context.Background(), fix2.NewBuilder(session.FIX44, "8").SetString(11, "EXEC-1"))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(wctx, session.Acknowledged); err != nil {
		t.Fatalf("Wait(Acknowledged): %v", err)
	}
}

// A session added with Acceptor.Add while Resolve creates it for a Logon
// is the one the Logon gets, at once: Resolve's is dropped, its store
// closed, and the Logon is not refused for the session being added
// already.
func TestDynamicSessionRaceAdd(t *testing.T) {
	added, resolved := newRecorder(), newRecorder()
	entered, release := make(chan struct{}), make(chan struct{})
	resolvedStore := memstore.New()
	a, l, log := dynamic(t, session.AcceptorOptions{
		Resolve: func(ctx context.Context, req *session.LogonRequest) (*session.Config, error) {
			close(entered)
			// Or the acceptor stops, should the test fail first.
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &session.Config{ID: req.ID, Handler: resolved, Store: resolvedStore}, nil
		},
	})
	irs, _ := initiate(t, l, 1)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve not called")
	}
	acc, err := a.Add(session.Config{ID: dynAccID, Handler: added})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	added.wait(t, func() bool { return added.logons == 1 })
	ir := irs[0]
	ir.wait(t, func() bool { return ir.logons == 1 })

	if s := a.Session(dynAccID); s != acc {
		t.Errorf("session %v is not the one added", dynAccID)
	}
	if n := len(a.Sessions()); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
	resolved.mu.Lock()
	if resolved.logons != 0 {
		t.Errorf("Resolve's session logged on %d times", resolved.logons)
	}
	resolved.mu.Unlock()
	if _, err := resolvedStore.Open(context.Background()); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Resolve's store reopened: %v, want %v", err, store.ErrClosed)
	}
	// The Logon was served by its first connection.
	for done := false; !done; {
		select {
		case msg := <-log:
			if strings.Contains(msg, "connection refused") {
				t.Errorf("acceptor logged %q", msg)
			}
		default:
			done = true
		}
	}
	// The added session's store is open.
	r, err := acc.Send(context.Background(), fix2.NewBuilder(session.FIX44, "8").SetString(11, "EXEC-1"))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(wctx, session.Acknowledged); err != nil {
		t.Fatalf("Wait(Acknowledged): %v", err)
	}
}

// waitFor waits for cond.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
