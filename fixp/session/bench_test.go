package session_test

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/session"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
	"github.com/sylr/fix2/transport/tcp"
)

// order is a 64-byte application message: schema 100, template 1.
type order [64]byte

func newOrder() *order {
	var o order
	binary.LittleEndian.PutUint16(o[0:], uint16(len(o)-8))
	binary.LittleEndian.PutUint16(o[2:], 1)
	binary.LittleEndian.PutUint16(o[4:], 100)
	return &o
}

func (o *order) AppendTo(dst []byte) ([]byte, error) { return append(dst, o[:]...), nil }

// counter counts the application messages it receives, echoing them if
// asked.
type counter struct {
	session.BaseHandler
	n    atomic.Int64
	echo bool
}

func (c *counter) OnMessage(ctx context.Context, s *session.Session, msg []byte, _ session.Inbound) error {
	c.n.Add(1)
	if c.echo {
		_, err := s.SendRaw(ctx, msg)
		return err
	}
	return nil
}

// benchPair establishes a client of clientFlow on a server of serverFlow,
// over a new connection of tr, and returns the client's session, its handler and the server's.
func benchPair(b *testing.B, tr string, clientFlow, serverFlow fixp.FlowType, echo bool) (*session.Session, *counter, *counter) {
	b.Helper()
	l, d := listen(b, tr)
	srvH, cliH := &counter{echo: echo}, &counter{}
	big := session.Config{KeepaliveInterval: 10 * time.Second, MaxPendingMessages: 1 << 16, MaxPendingBytes: 64 << 20, MaxStoredMessages: 1 << 16}
	scfg := big
	scfg.Handler = srvH
	srv, err := session.NewServer(session.ServerConfig{
		Config: scfg, Flow: serverFlow, Store: memstore.New(),
		Authenticate: func(context.Context, *session.AuthRequest) (string, error) { return "bench", nil },
	})
	if err != nil {
		b.Fatal(err)
	}
	srv.Listen(l)
	ccfg := big
	ccfg.Handler = cliH
	c, err := session.NewClient(session.ClientConfig{Config: ccfg, Flow: clientFlow, Store: memstore.New()}, d)
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srvDone, cliDone := make(chan error, 1), make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	go func() { cliDone <- c.Run(ctx) }()
	b.Cleanup(func() { cancel(); <-cliDone; <-srvDone })
	for deadline := time.Now().Add(10 * time.Second); c.Session().State() != session.StateEstablished; {
		if time.Now().After(deadline) {
			b.Fatalf("not established: %s", c.Session().State())
		}
		time.Sleep(time.Millisecond)
	}
	return c.Session(), cliH, srvH
}

// transports are the connections the benchmarks run over: in memory, and
// TCP on loopback.
var transports = []string{"mem", "tcp"}

// listen returns a new listener of a transport, and its dialer: one per
// run, the server closing its listener as it stops.
func listen(b *testing.B, name string) (transport.Listener, transport.Dialer) {
	b.Helper()
	if name == "mem" {
		l := mem.Listen("bench")
		return l, l
	}
	l, err := tcp.Listen("127.0.0.1:0", nil)
	if err != nil {
		b.Skipf("no loopback listener: %v", err)
	}
	return l, &tcp.Dialer{Addresses: []string{l.Addr().String()}}
}

// await waits for h to count n messages, spinning, failing the benchmark
// after a while or once the session stops.
func await(b *testing.B, s *session.Session, h *counter, n int64) {
	deadline := time.Now().Add(30 * time.Second)
	for spins := 0; h.n.Load() < n; spins++ {
		if spins%4096 == 0 {
			select {
			case <-s.Done():
				b.Fatalf("session stopped: %v", s.Err())
			default:
			}
			if time.Now().After(deadline) {
				b.Fatalf("%d messages of %d", h.n.Load(), n)
			}
		}
	}
}

// BenchmarkSend measures sending a message to the server's handler:
// sequencing, storing on a Recoverable flow (its store keeping the last
// 65536), queuing, framing and writing, reading and delivery; timed until
// the last is delivered.
func BenchmarkSend(b *testing.B) {
	for _, flow := range []fixp.FlowType{fixp.Idempotent, fixp.Recoverable, fixp.Unsequenced} {
		for _, tr := range transports {
			b.Run(flow.String()+"/"+tr, func(b *testing.B) {
				s, _, srvH := benchPair(b, tr, flow, fixp.Unsequenced, false)
				ctx, stop := context.WithTimeout(context.Background(), time.Minute)
				defer stop()
				o := newOrder()
				b.SetBytes(int64(len(o)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if _, err := s.Send(ctx, o); err != nil {
						b.Fatal(err)
					}
				}
				await(b, s, srvH, int64(b.N))
			})
		}
	}
}

// BenchmarkRoundTrip measures a message and its echo, one at a time: an
// order on an Idempotent client flow, the echo on a Recoverable server
// flow.
func BenchmarkRoundTrip(b *testing.B) {
	for _, tr := range transports {
		b.Run(tr, func(b *testing.B) {
			s, cliH, _ := benchPair(b, tr, fixp.Idempotent, fixp.Recoverable, true)
			ctx, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			o := newOrder()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range int64(b.N) {
				if _, err := s.Send(ctx, o); err != nil {
					b.Fatal(err)
				}
				await(b, s, cliH, i+1)
			}
		})
	}
}
