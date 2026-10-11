package session_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/schedule"
	"github.com/sylr/fix2/session/transport/mem"
)

// broker is the acceptor's application: it receives orders.
type broker struct {
	session.BaseHandler
	orders chan string
}

func (b *broker) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	b.orders <- fmt.Sprintf("order %s, seq %d", m.Get(11), in.Seq)
	return nil
}

// This example runs an acceptor and an initiator in one process, over an
// in-memory connection: the initiator logs on, sends an order and waits for
// the acceptor's engine to acknowledge reading it. Over the network, use
// package tcp's Listen and Dialer instead.
func Example() {
	l := mem.Listen("broker")

	acceptor := session.NewAcceptor(session.AcceptorOptions{})
	b := &broker{orders: make(chan string, 1)}
	if _, err := acceptor.Add(session.Config{
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "BROKER", TargetCompID: "CLIENT"},
		Handler: b,
	}); err != nil {
		fmt.Println(err)
		return
	}
	acceptor.Listen(l)

	initiator := session.NewInitiator(session.InitiatorOptions{})
	client, err := initiator.Add(session.Config{
		ID:         session.SessionID{BeginString: session.FIX44, SenderCompID: "CLIENT", TargetCompID: "BROKER"},
		HeartBtInt: 30 * time.Second,
		Handler:    session.BaseHandler{},
	}, l)
	if err != nil {
		fmt.Println(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); acceptor.Run(ctx) }()
	go func() { defer wg.Done(); initiator.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()

	for client.State() != session.StateLoggedOn {
		time.Sleep(time.Millisecond)
	}
	order := fix2.NewBuilder(session.FIX44, "D").
		SetString(11, "ORD-1"). // ClOrdID
		SetString(55, "AAPL").  // Symbol
		SetString(54, "1")      // Side
	receipt, err := client.Send(ctx, order)
	if err != nil {
		fmt.Println(err)
		return
	}
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if err := receipt.Wait(wctx, session.Acknowledged); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(<-b.orders)
	fmt.Println("acknowledged:", receipt.Seq())
	// Output:
	// order ORD-1, seq 2
	// acknowledged: 2
}

// This example shows a session's configuration: its dictionaries, schedule
// and logging. Sessions log their events to a *slog.Logger: zerolog and zap
// plug in through their slog handlers, zerolog.NewSlogHandler (zerolog
// v1.35 and later) and zapslog.NewHandler (go.uber.org/zap/exp/zapslog);
// SlogObserver logs the messages themselves, redacted.
func ExampleConfig() {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		fmt.Println(err)
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := session.Config{
		ID:         session.SessionID{BeginString: session.FIX44, SenderCompID: "CLIENT", TargetCompID: "BROKER"},
		HeartBtInt: 30 * time.Second,
		// Every weekday from 17:00 to 16:55 the next day, New York time.
		Schedule: schedule.Daily{
			Start:    schedule.Clock(17, 0, 0),
			End:      schedule.Clock(16, 55, 0),
			Days:     []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday},
			Location: ny,
		},
		Logger:   logger,
		Observer: session.SlogObserver{Logger: logger},
		Handler:  session.BaseHandler{},
	}
	if err := cfg.Validate(); err != nil {
		fmt.Println(err)
		return
	}
	bad := cfg
	bad.HeartBtInt = 1500 * time.Millisecond
	bad.ID.TargetCompID = ""
	fmt.Println(bad.Validate())
	// Output:
	// session FIX.4.4:CLIENT->: invalid configuration: ID.TargetCompID: required; HeartBtInt: must be whole seconds >= 0, got 1.5s
}
