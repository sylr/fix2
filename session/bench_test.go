package session_test

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// counter counts the application messages it receives.
type counter struct {
	session.BaseHandler
	n atomic.Int64
}

func (c *counter) FromApp(context.Context, *session.Session, *fix2.Message, session.Inbound) error {
	c.n.Add(1)
	return nil
}

// benchPair returns a logged on initiator session and the acceptor's
// counter.
func benchPair(b *testing.B) (*session.Session, *counter) {
	b.Helper()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	recv := &counter{}
	if _, err := a.Add(session.Config{
		ID:              session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler:         recv,
		DeliveryQueue:   1 << 16,
		MaxPendingAge:   0,
		MaxLatency:      -1,
		WriteTimeout:    time.Minute,
		MaxStash:        1 << 16,
		LogoutTimeout:   100 * time.Millisecond,
		SkipChecks:      0,
		AckProbe:        session.AckProbeNever,
		HeartBtInt:      30 * time.Second,
		MaxHeldMessages: 1 << 16,
	}); err != nil {
		b.Fatal(err)
	}
	a.Listen(l)
	i := session.NewInitiator(session.InitiatorOptions{})
	s, err := i.Add(session.Config{
		ID:                 session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler:            session.BaseHandler{},
		MaxPendingMessages: 1 << 16,
		MaxPendingBytes:    64 << 20,
		LogoutTimeout:      100 * time.Millisecond,
	}, l)
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(ctx) }()
	go func() { defer wg.Done(); i.Run(ctx) }()
	b.Cleanup(func() { cancel(); wg.Wait() })
	for s.State() != session.StateLoggedOn {
		time.Sleep(time.Millisecond)
	}
	return s, recv
}

// BenchmarkSend measures sending an order: sequencing, encoding, storing
// in memory and queuing it, with the writer, the acceptor's reader and its
// delivery running alongside.
func BenchmarkSend(b *testing.B) {
	s, recv := benchPair(b)
	ctx := context.Background()
	order := fix2.NewBuilder(session.FIX44, "D")
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		order.Reset()
		order.SetString(11, "order-12345").SetString(55, "AAPL").SetString(54, "1").
			SetInt(38, 100).SetString(40, "2").SetFloat(44, 150.5).SetString(60, "20261011-12:00:00")
		if _, err := s.Send(ctx, order); err != nil {
			b.Fatal(err)
		}
		n++
	}
	b.StopTimer()
	deadline := time.Now().Add(30 * time.Second)
	for recv.n.Load() < int64(n) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := recv.n.Load(); got < int64(n) {
		b.Fatalf("acceptor received %d of %d", got, n)
	}
}

// BenchmarkReceive measures receiving an order: framing, parsing, checking
// and sequencing it, and delivering it to FromApp, the counterparty
// streaming orders in batches of 32 KiB.
func BenchmarkReceive(b *testing.B) {
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	recv := &counter{}
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"}
	if _, err := a.Add(session.Config{
		ID:            id,
		Handler:       recv,
		DeliveryQueue: 1 << 16,
		MaxLatency:    -1,
		LogoutTimeout: 100 * time.Millisecond,
		AckProbe:      session.AckProbeNever,
		HeartBtInt:    30 * time.Second,
	}); err != nil {
		b.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	b.Cleanup(func() { cancel(); <-done })
	conn, err := l.Dial(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	go io.Copy(io.Discard, conn)

	const sendingTime = "20261011-12:00:00.000"
	logon := fix2.NewBuilder(session.FIX44, "A").
		SetString(49, "INI").SetString(56, "ACC").SetInt(34, 1).SetString(52, sendingTime).
		SetString(98, "0").SetString(108, "30")
	if _, err := conn.Write(logon.Build()); err != nil {
		b.Fatal(err)
	}
	s := a.Session(id)
	for s.State() != session.StateLoggedOn {
		time.Sleep(time.Millisecond)
	}

	order := fix2.NewBuilder(session.FIX44, "D")
	seq := 1
	var buf []byte
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		seq++
		order.Reset()
		order.SetString(49, "INI").SetString(56, "ACC").SetInt(34, seq).SetString(52, sendingTime).
			SetString(11, "order-12345").SetString(55, "AAPL").SetString(54, "1").
			SetInt(38, 100).SetString(40, "2").SetFloat(44, 150.5).SetString(60, "20261011-12:00:00")
		buf = order.AppendTo(buf)
		if len(buf) >= 32<<10 {
			if _, err := conn.Write(buf); err != nil {
				b.Fatal(err)
			}
			buf = buf[:0]
		}
	}
	if len(buf) > 0 {
		if _, err := conn.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for recv.n.Load() < int64(b.N) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Microsecond)
	}
	b.StopTimer()
	if got := recv.n.Load(); got < int64(b.N) {
		b.Fatalf("received %d of %d", got, b.N)
	}
}

// BenchmarkSendWritten waits for each order to be written: the latency of
// one Send up to the writer's write.
func BenchmarkSendWritten(b *testing.B) {
	s, _ := benchPair(b)
	ctx := context.Background()
	order := fix2.NewBuilder(session.FIX44, "D")
	b.ReportAllocs()
	for b.Loop() {
		order.Reset()
		order.SetString(11, "order-12345").SetString(55, "AAPL").SetString(54, "1")
		r, err := s.Send(ctx, order)
		if err != nil {
			b.Fatal(err)
		}
		if err := r.Wait(ctx, session.Written); err != nil {
			b.Fatal(err)
		}
	}
}
