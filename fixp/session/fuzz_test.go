package session

import (
	"context"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// FuzzServer writes arbitrary bytes to a server, as a client's first
// frames and what follows them: it must neither panic nor keep the
// connection beyond its timeouts.
func FuzzServer(f *testing.F) {
	id := fixp.SessionID{0x7e, 0xbd, 0x6d, 0xbd, 0xee, 0x34, 0x49, 0x72, 0xad, 0xcf, 0x9b, 0xab, 0x3e, 0xd0, 0x5e, 0xc1}
	ts := uint64(1_700_000_000_000_000_000)
	neg := mustFrame(&codec.Negotiate{SessionId: id, Timestamp: ts, ClientFlow: codec.FlowTypeIdempotent, Credentials: []byte("user:alice")})
	est := mustFrame(&codec.Establish{SessionId: id, Timestamp: ts, KeepaliveInterval: 100, NextSeqNo: codec.Some(uint64(1)), Credentials: []byte("user:alice")})
	seq := mustFrame(&codec.Sequence{NextSeqNo: 1})
	app, _ := frame(appMsg{1, "order"})
	term := mustFrame(&codec.Terminate{SessionId: id, Code: codec.TerminationCodeFinished})
	cat := func(frames ...[]byte) []byte {
		var b []byte
		for _, f := range frames {
			b = append(b, f...)
		}
		return b
	}
	f.Add(neg)
	f.Add(cat(neg, est))
	f.Add(cat(neg, est, seq, app, app))
	f.Add(cat(neg, est, seq, app, term))
	f.Add(cat(neg, est, mustFrame(&codec.UnsequencedHeartbeat{}), app))
	f.Add(cat(neg, est, mustFrame(&codec.Applied{FromSeqNo: 1, Count: 1})))
	f.Add(est)
	f.Fuzz(fuzzServer)
}

func fuzzServer(t *testing.T, b []byte) {
	watchdog := time.AfterFunc(5*time.Second, func() {
		pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		panic("fuzzServer: an iteration takes more than 5s")
	})
	defer watchdog.Stop()
	term := mustFrame(&codec.Terminate{Code: codec.TerminationCodeFinished})
	l := mem.Listen("fuzz")
	srv, err := NewServer(ServerConfig{
		// Short timeouts: an incomplete handshake waits for nothing long.
		Config:       Config{Handler: BaseHandler{}, KeepaliveInterval: 20 * time.Millisecond, HandshakeTimeout: 10 * time.Millisecond},
		Flow:         fixp.Recoverable,
		Store:        memstore.New(),
		Authenticate: authenticate,
		MinKeepalive: time.Millisecond,
		MaxClockSkew: 100 * 365 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() { cancel(); <-done }()
	c, err := l.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetWriteDeadline(time.Now().Add(time.Second))
	c.Write(b)
	// The server answers, then closes: read until it does.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	for {
		if _, err := c.Read(buf); err != nil {
			if isTimeout(err) {
				// Established and alive: what it read was valid. A
				// Terminate ends it.
				c.SetWriteDeadline(time.Now().Add(time.Second))
				c.Write(term)
			}
			return
		}
	}
}

func TestFuzzServerRepeated(t *testing.T) {
	id := fixp.NewSessionID()
	neg := mustFrame(&codec.Negotiate{SessionId: id, Timestamp: nanos(time.Now()), ClientFlow: codec.FlowTypeIdempotent, Credentials: []byte("user:alice")})
	est := mustFrame(&codec.Establish{SessionId: id, Timestamp: nanos(time.Now()), KeepaliveInterval: 100, NextSeqNo: codec.Some(uint64(1)), Credentials: []byte("user:alice")})
	seq := mustFrame(&codec.Sequence{NextSeqNo: 1})
	app, _ := frame(appMsg{1, "order"})
	inputs := [][]byte{neg, append(append([]byte{}, neg...), est...), append(append(append(append([]byte{}, neg...), est...), seq...), app...)}
	before := runtime.NumGoroutine()
	for i := range 30 {
		start := time.Now()
		fuzzServer(t, inputs[i%len(inputs)])
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("iteration %d took %v", i, d)
		}
	}
	// What the iterations started ends with them.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
		t.Fatalf("%d goroutines, %d before", n, before)
	}
}
