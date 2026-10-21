package session_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// recorder is a Handler recording what it is told.
type recorder struct {
	session.BaseHandler
	mu       sync.Mutex
	logons   int
	logouts  []error
	apps     []string      // ClOrdID of each application message
	received []time.Time   // Inbound.Received of each application message
	got      chan struct{} // wakes wait; one pending wakeup is enough
}

func newRecorder() *recorder { return &recorder{got: make(chan struct{}, 1)} }

// notify wakes wait without blocking: a callback blocked on a test nobody
// waits on would outlive it, the delivery goroutine stuck.
func (r *recorder) notify() {
	select {
	case r.got <- struct{}{}:
	default:
	}
}

func (r *recorder) OnLogon(context.Context, *session.Session) {
	r.mu.Lock()
	r.logons++
	r.mu.Unlock()
	r.notify()
}

func (r *recorder) OnLogout(_ context.Context, _ *session.Session, err error) {
	r.mu.Lock()
	r.logouts = append(r.logouts, err)
	r.mu.Unlock()
	r.notify()
}

func (r *recorder) FromApp(_ context.Context, _ *session.Session, m *fix.Message, in session.Inbound) error {
	r.mu.Lock()
	r.apps = append(r.apps, string(m.Get(11)))
	r.received = append(r.received, in.Received)
	r.mu.Unlock()
	r.notify()
	return nil
}

func (r *recorder) wait(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		ok := cond()
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.got:
		case <-deadline:
			t.Fatal("timed out")
		}
	}
}

func pair(t *testing.T, beginString string) (ini, acc *session.Session, ir, ar *recorder) {
	t.Helper()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	ar = newRecorder()
	acc, err := a.Add(session.Config{
		ID:      session.SessionID{BeginString: beginString, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler: ar,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond})
	ir = newRecorder()
	ini, err = i.Add(session.Config{
		ID:      session.SessionID{BeginString: beginString, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler: ir,
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(ctx) }()
	go func() { defer wg.Done(); i.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return ini, acc, ir, ar
}

func TestLogonSendLogout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ini, acc, ir, ar := pair(t, session.FIX44)
		ir.wait(t, func() bool { return ir.logons == 1 })
		ar.wait(t, func() bool { return ar.logons == 1 })

		ctx := context.Background()
		for _, id := range []string{"A", "B", "C"} {
			r, err := ini.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, id).SetString(55, "X"))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Wait(ctx, session.Written); err != nil {
				t.Fatal(err)
			}
		}
		ar.wait(t, func() bool { return len(ar.apps) == 3 })
		if got := ar.apps; got[0] != "A" || got[1] != "B" || got[2] != "C" {
			t.Errorf("acceptor received %v", got)
		}
		r, err := acc.Send(ctx, fix.NewBuilder(session.FIX44, "8").SetString(11, "A"))
		if err != nil {
			t.Fatal(err)
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := r.Wait(wctx, session.Acknowledged); err != nil {
			t.Fatalf("Wait(Acknowledged): %v", err)
		}
		ir.wait(t, func() bool { return len(ir.apps) == 1 })

		if err := ini.Logout(ctx, "bye"); err != nil {
			t.Fatal(err)
		}
		ar.wait(t, func() bool { return len(ar.logouts) == 1 })
		if got := ini.NextSenderSeqNum(); got != 6 { // Logon, 3 orders, Logout... and a heartbeat answer
			t.Logf("initiator next sender = %d", got)
		}
	})
}
