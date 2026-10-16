package sqlite3_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/store/sqlite3"
	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

type broker struct {
	session.BaseHandler
	orders chan string
}

func (b *broker) FromApp(_ context.Context, _ *session.Session, m *fix.Message, in session.Inbound) error {
	b.orders <- fmt.Sprintf("order %s, seq %d", m.Get(11), in.Seq)
	return nil
}

// An initiator restarted on the same database carries on with its sequence
// numbers: the acceptor, which kept running, takes its Logon.
func TestSessionRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fix2.db")
	l := mem.Listen("broker")
	acceptor := session.NewAcceptor(session.AcceptorOptions{})
	b := &broker{orders: make(chan string, 1)}
	if _, err := acceptor.Add(session.Config{
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "BROKER", TargetCompID: "CLIENT"},
		Handler: b,
	}); err != nil {
		t.Fatal(err)
	}
	acceptor.Listen(l)
	actx, acancel := context.WithCancel(context.Background())
	var awg sync.WaitGroup
	awg.Go(func() { _ = acceptor.Run(actx) })
	defer func() { acancel(); awg.Wait() }()

	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "CLIENT", TargetCompID: "BROKER"}
	run := func(order string) uint64 {
		t.Helper()
		st, err := sqlite3.OpenFile(path, id.String())
		if err != nil {
			t.Fatal(err)
		}
		initiator := session.NewInitiator(session.InitiatorOptions{})
		client, err := initiator.Add(session.Config{ID: id, HeartBtInt: 30 * time.Second, Store: st, Handler: session.BaseHandler{}}, l)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); _ = initiator.Run(ctx) }()
		for client.State() != session.StateLoggedOn {
			if ctx.Err() != nil {
				t.Fatalf("not logged on: %v", client.Err())
			}
			time.Sleep(time.Millisecond)
		}
		receipt, err := client.Send(ctx, fix.NewBuilder(session.FIX44, "D").
			SetString(11, order).
			SetString(55, "AAPL").
			SetString(54, "1"))
		if err != nil {
			t.Fatal(err)
		}
		sent := client.NextSenderSeqNum() - 1
		if err := receipt.Wait(ctx, session.Acknowledged); err != nil {
			t.Fatal(err)
		}
		cancel()
		<-done
		return sent
	}

	first := run("ORD-1")
	if got, want := <-b.orders, fmt.Sprintf("order ORD-1, seq %d", first); got != want {
		t.Errorf("first run: %s, want %s", got, want)
	}
	second := run("ORD-2")
	if second <= first+1 {
		t.Errorf("second run sent order %d, after %d in the first: sequence numbers started over", second, first)
	}
	if got, want := <-b.orders, fmt.Sprintf("order ORD-2, seq %d", second); got != want {
		t.Errorf("second run: %s, want %s", got, want)
	}
}
