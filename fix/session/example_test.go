package session_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/schedule"
	"github.com/sylr/fix2/transport/mem"
)

// broker is the acceptor's application: it receives orders.
type broker struct {
	session.BaseHandler
	orders chan string
}

func (b *broker) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
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
	order := fix.NewBuilder(session.FIX44, "D").
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

// This example creates sessions as their counterparties log on, with
// AcceptorOptions.Resolve: the acceptor is given none up front, and
// Resolve decides from each Logon whether to serve it and how. A session
// so created stays, its sequence numbers too, until Acceptor.Remove.
func ExampleAcceptor_dynamic() {
	l := mem.Listen("broker")

	known := map[string]bool{"CLIENT-A": true, "CLIENT-B": true}
	// Refused, the intruder tries again and again: only its first try is
	// told.
	resolved := make(chan string, 1)
	tell := func(what string) {
		select {
		case resolved <- what:
		default:
		}
	}
	acceptor := session.NewAcceptor(session.AcceptorOptions{
		Resolve: func(ctx context.Context, req *session.LogonRequest) (*session.Config, error) {
			// req.ID is the session the Logon is for, as the acceptor
			// names it: the counterparty is its Target.
			if !known[req.ID.TargetCompID] {
				tell(req.ID.TargetCompID + " refused")
				return nil, errors.New("unknown counterparty")
			}
			tell(req.ID.TargetCompID + " served as " + req.ID.String())
			return &session.Config{ID: req.ID, Handler: session.BaseHandler{}}, nil
		},
	})
	acceptor.Listen(l)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); acceptor.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()

	for _, client := range []string{"CLIENT-A", "INTRUDER"} {
		initiator := session.NewInitiator(session.InitiatorOptions{})
		s, err := initiator.Add(session.Config{
			ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: client, TargetCompID: "BROKER"},
			Handler: session.BaseHandler{},
		}, l)
		if err != nil {
			fmt.Println(err)
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); initiator.Run(ctx) }()
		select {
		case what := <-resolved:
			fmt.Println(what)
		case <-time.After(5 * time.Second):
			fmt.Println("no Logon from", client)
			return
		}
		for deadline := time.Now().Add(5 * time.Second); known[client] && s.State() != session.StateLoggedOn; {
			if time.Now().After(deadline) {
				fmt.Println(client, "not logged on")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}

	fmt.Println("sessions:", len(acceptor.Sessions()))
	// Output:
	// CLIENT-A served as FIX.4.4:BROKER->CLIENT-A
	// INTRUDER refused
	// sessions: 1
}

// desk is the handler of a trading desk's session: its Logons carry the
// desk's Username (553).
type desk struct {
	session.BaseHandler
	username string
}

func (d desk) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if msgType == "A" {
		b.SetString(553, d.username)
	}
	return nil
}

// This example serves two desks of one client, logging on with the same
// CompIDs: the acceptor's sessions are told apart by their Qualifier, and
// AcceptorOptions.Route picks each Logon's from what it carries, its
// Username here. Better grounds than a Username sent in the clear: over
// TLS requiring and verifying client certificates
// (tls.RequireAndVerifyClientCert), req.TLS.VerifiedChains names the
// client, and a transport authenticating its peers gives req.Principal.
func ExampleAcceptor_route() {
	l := mem.Listen("broker")

	broker := func(q string) session.SessionID {
		return session.SessionID{BeginString: session.FIX44, SenderCompID: "BROKER", TargetCompID: "CLIENT", Qualifier: q}
	}
	// Refused, the intruder tries again and again: only its first try is
	// told.
	routed := make(chan string, 1)
	tell := func(what string) {
		select {
		case routed <- what:
		default:
		}
	}
	acceptor := session.NewAcceptor(session.AcceptorOptions{
		Route: func(ctx context.Context, req *session.LogonRequest, candidates []session.SessionID) (session.SessionID, error) {
			username := string(req.Logon.Get(553))
			for _, id := range candidates {
				if id.Qualifier == username {
					tell(username + " routed to " + id.String())
					return id, nil
				}
			}
			tell(username + " refused")
			return session.SessionID{}, errors.New("unknown username")
		},
	})
	for _, q := range []string{"equities", "options"} {
		if _, err := acceptor.Add(session.Config{ID: broker(q), Handler: session.BaseHandler{}}); err != nil {
			fmt.Println(err)
			return
		}
	}
	acceptor.Listen(l)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); acceptor.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()

	for _, username := range []string{"equities", "options", "mallory"} {
		initiator := session.NewInitiator(session.InitiatorOptions{})
		s, err := initiator.Add(session.Config{
			ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "CLIENT", TargetCompID: "BROKER"},
			Handler: desk{username: username},
		}, l)
		if err != nil {
			fmt.Println(err)
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); initiator.Run(ctx) }()
		select {
		case what := <-routed:
			fmt.Println(what)
		case <-time.After(5 * time.Second):
			fmt.Println("no Logon from", username)
			return
		}
		for deadline := time.Now().Add(5 * time.Second); username != "mallory" && s.State() != session.StateLoggedOn; {
			if time.Now().After(deadline) {
				fmt.Println(username, "not logged on")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}

	for _, q := range []string{"equities", "options"} {
		fmt.Println(q, acceptor.Session(broker(q)).State())
	}
	// Output:
	// equities routed to FIX.4.4:BROKER->CLIENT:equities
	// options routed to FIX.4.4:BROKER->CLIENT:options
	// mallory refused
	// equities logged on
	// options logged on
}
