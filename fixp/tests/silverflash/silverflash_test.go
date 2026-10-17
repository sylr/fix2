// Package silverflash runs fix2's FIXP client against a Silverflash server,
// the FIX Trading Community's reference implementation, patched to frame
// as SOFH 1.0 does (docs/fixp-evidence/silverflash-patched), its server
// flow Idempotent. It runs only when FIX2_SILVERFLASH names the server,
// host:port:
//
//	FIX2_SILVERFLASH=127.0.0.1:7760 go test ./fixp/tests/silverflash/
//
// Three more tests characterize what fails on Silverflash's side:
// FIX2_SILVERFLASH_BURST=1, FIX2_SILVERFLASH_FINISH=1 and
// FIX2_SILVERFLASH_FRAGMENTED=1 run them, each against a server of its
// own (docs/fixp-evidence/silverflash-patched).
package silverflash

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/session"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/tcp"
)

// order is the spike's application message: schema 33, template 22, its
// body text. The server answers each with one of its own.
type order string

func (o order) AppendTo(dst []byte) ([]byte, error) {
	var h [8]byte
	binary.LittleEndian.PutUint16(h[0:], uint16(len(o)))
	binary.LittleEndian.PutUint16(h[2:], 22)
	binary.LittleEndian.PutUint16(h[4:], 33)
	return append(append(dst, h[:]...), o...), nil
}

type event struct {
	kind string
	seq  uint64
	body string
	err  error
}

type handler struct {
	session.BaseHandler
	events chan event
	wire   wire
	// Connections attempted, made, and their writes split.
	dials, connected, splits atomic.Int64
}

// wire records the FIXP session messages the server sent on established
// connections, heartbeats aside.
type wire struct {
	mu        sync.Mutex
	templates []uint16
}

func (w *wire) OnInbound(_ *session.Session, f session.Frame) {
	if f.Header.SchemaID != codec.SchemaID || f.Header.TemplateID == codec.SequenceTemplateID {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.templates = append(w.templates, f.Header.TemplateID)
}

func (w *wire) OnOutbound(*session.Session, session.Frame) {}

// received tells whether the server sent a message of that template.
func (w *wire) received(template uint16) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.templates, template)
}

func (h *handler) OnEstablished(context.Context, *session.Session) {
	h.events <- event{kind: "established"}
}

func (h *handler) OnUnbound(_ context.Context, _ *session.Session, err error) {
	h.events <- event{kind: "unbound", err: err}
}

func (h *handler) OnMessage(_ context.Context, _ *session.Session, msg []byte, in session.Inbound) error {
	h.events <- event{kind: "message", seq: in.Seq, body: string(msg[8:])}
	return nil
}

func (h *handler) OnPeerFinished(context.Context, *session.Session) error {
	h.events <- event{kind: "peerfinished"}
	return nil
}

func (h *handler) OnFinalized(context.Context, *session.Session) {
	h.events <- event{kind: "finalized"}
}

func (h *handler) next(t *testing.T, timeout time.Duration) (event, bool) {
	t.Helper()
	select {
	case e := <-h.events:
		return e, true
	case <-time.After(timeout):
		return event{}, false
	}
}

func (h *handler) expect(t *testing.T, kind string) event {
	t.Helper()
	for {
		e, ok := h.next(t, 10*time.Second)
		if !ok {
			t.Fatalf("no %s", kind)
		}
		t.Logf("event %s seq=%d %q %v", e.kind, e.seq, e.body, e.err)
		if e.kind == kind {
			return e
		}
	}
}

// fragmentingConn writes what it is given in two writes, 50ms apart, the
// first of 3 bytes, splitting the first frame's SOFH header; it counts
// the writes so split.
type fragmentingConn struct {
	transport.Conn
	splits *atomic.Int64
}

func (c fragmentingConn) Write(b []byte) (int, error) {
	if len(b) <= 3 {
		return c.Conn.Write(b)
	}
	n, err := c.Conn.Write(b[:3])
	if err != nil {
		return n, err
	}
	time.Sleep(50 * time.Millisecond)
	m, err := c.Conn.Write(b[3:])
	if err == nil {
		c.splits.Add(1)
	}
	return n + m, err
}

// dialer counts the connections attempted and made, fragmenting them if
// asked.
type dialer struct {
	transport.Dialer
	fragment bool
	h        *handler
}

func (d dialer) Dial(ctx context.Context) (transport.Conn, error) {
	d.h.dials.Add(1)
	c, err := d.Dialer.Dial(ctx)
	if err != nil {
		return nil, err
	}
	d.h.connected.Add(1)
	if !d.fragment {
		return c, nil
	}
	return fragmentingConn{c, &d.h.splits}, nil
}

func client(t *testing.T, flow fixp.FlowType) (*session.Client, *handler, context.CancelFunc, chan error) {
	return clientWith(t, flow, false)
}

func clientWith(t *testing.T, flow fixp.FlowType, fragment bool) (*session.Client, *handler, context.CancelFunc, chan error) {
	t.Helper()
	addr := os.Getenv("FIX2_SILVERFLASH")
	if addr == "" {
		t.Skip("FIX2_SILVERFLASH not set")
	}
	h := &handler{events: make(chan event, 1024)}
	d := dialer{&tcp.Dialer{Addresses: []string{addr}, Net: &net.Dialer{}}, fragment, h}
	var logger *slog.Logger
	if os.Getenv("FIX2_SILVERFLASH_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	c, err := session.NewClient(session.ClientConfig{
		Config: session.Config{
			Handler:           h,
			KeepaliveInterval: time.Second,
			Logger:            logger,
			Observer:          &h.wire,
			Tolerations: session.Tolerations{
				// Silverflash sends no heartbeat when idle, and pads its
				// session messages (docs/fixp.md).
				ToleratePeerWithoutKeepalives: true,
				TolerateTrailingBytes:         true,
			},
		},
		Flow:  flow,
		Store: memstore.New(),
		Credentials: func(context.Context, session.CredentialsRequest) ([]byte, error) {
			return []byte("spike\x00token"), nil
		},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return c, h, cancel, done
}

// Many orders, by windows of 50 awaiting their answers, on either client
// flow. Unpaced, Silverflash answers some hundreds only
// (TestBurstCharacterization).
func TestPaced(t *testing.T) {
	for _, flow := range []fixp.FlowType{fixp.Idempotent, fixp.Recoverable} {
		t.Run(flow.String(), func(t *testing.T) { paced(t, flow) })
	}
}

// Characterizes, opt-in, Silverflash's answer to a Negotiate written in two
// writes, 50ms apart: none, its session thread dying (its log, which the
// runner checks). It clears its read buffer before each read
// (AbstractTcpChannel.readyToRead), dropping what a read leaves of a frame.
func TestFragmentedCharacterization(t *testing.T) {
	if os.Getenv("FIX2_SILVERFLASH_FRAGMENTED") == "" {
		t.Skip("FIX2_SILVERFLASH_FRAGMENTED not set")
	}
	_, h, cancel, done := clientWith(t, fixp.Idempotent, true)
	defer func() { cancel(); <-done }()
	deadline := time.After(5 * time.Second)
	for waiting := true; waiting; {
		select {
		case e := <-h.events:
			t.Fatalf("event %s: not the finding", e.kind)
		case err := <-done:
			done <- err
			t.Fatalf("Run returned: %v", err)
		case <-deadline:
			waiting = false
		}
	}
	if h.connected.Load() == 0 || h.splits.Load() == 0 {
		t.Fatalf("%d connections, %d split writes: nothing reached Silverflash split", h.connected.Load(), h.splits.Load())
	}
}

// Characterizes, opt-in, Silverflash's answer to 2000 orders sent at once:
// some hundreds only, the rest never, its consumer receiving no more (its
// log).
func TestBurstCharacterization(t *testing.T) {
	if os.Getenv("FIX2_SILVERFLASH_BURST") == "" {
		t.Skip("FIX2_SILVERFLASH_BURST not set")
	}
	c, h, cancel, done := client(t, fixp.Idempotent)
	defer func() { cancel(); <-done }()
	h.expect(t, "established")
	const n = 2000
	for i := 1; i <= n; i++ {
		if _, err := c.Session().Send(context.Background(), order(fmt.Sprintf("order %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// The answers, in order, until none for 5s, within a minute.
	answers := 0
	deadline := time.After(time.Minute)
	for idle := false; !idle; {
		select {
		case e := <-h.events:
			if e.kind != "message" || e.seq != uint64(answers+1) || e.body != fmt.Sprintf("ack %d", answers+1) {
				t.Fatalf("after %d answers: %+v", answers, e)
			}
			answers++
		case <-time.After(5 * time.Second):
			idle = true
		case <-deadline:
			t.Fatalf("%d answers, still coming after a minute", answers)
		}
	}
	t.Logf("%d answers to %d orders", answers, n)
	if answers == 0 || answers >= n {
		t.Fatalf("%d answers to %d orders: not the finding", answers, n)
	}
}

// Characterizes, opt-in, Silverflash's answer to our FinishedSending: a
// Terminate right away, without FinishedReceiving, so the session is not
// finalized. Our client re-establishes; an Establish first on a connection
// kills Silverflash's session thread (its log), and no answer comes.
func TestFinishCharacterization(t *testing.T) {
	if os.Getenv("FIX2_SILVERFLASH_FINISH") == "" {
		t.Skip("FIX2_SILVERFLASH_FINISH not set")
	}
	c, h, cancel, done := client(t, fixp.Idempotent)
	defer func() { cancel(); <-done }()
	h.expect(t, "established")
	if err := c.Session().Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := h.expect(t, "unbound")
	var te *fixp.TerminatedError
	if !errors.As(e.err, &te) || !te.ByPeer || te.Code != fixp.TerminationFinished {
		t.Fatalf("unbound: %v", e.err)
	}
	if !h.wire.received(codec.TerminateTemplateID) || h.wire.received(codec.FinishedReceivingTemplateID) {
		t.Fatal("Silverflash sent no Terminate, or a FinishedReceiving")
	}
	// Our client connects again, and is neither established nor finalized.
	deadline := time.After(10 * time.Second)
	for waiting := true; waiting; {
		select {
		case e := <-h.events:
			t.Fatalf("event %s: not the finding", e.kind)
		case err := <-done:
			done <- err
			t.Fatalf("Run returned: %v", err)
		case <-deadline:
			waiting = false
		}
	}
	if n := h.dials.Load(); n < 2 {
		t.Fatalf("%d connection attempts: no re-establishment tried", n)
	}
}

func paced(t *testing.T, flow fixp.FlowType) {
	c, h, cancel, done := client(t, flow)
	defer func() { cancel(); <-done }()
	h.expect(t, "established")
	const n, window = 2000, 50
	for i := 1; i <= n; i += window {
		for j := i; j < i+window; j++ {
			if _, err := c.Session().Send(context.Background(), order(fmt.Sprintf("order %d", j))); err != nil {
				t.Fatal(err)
			}
		}
		for j := i; j < i+window; j++ {
			e, ok := h.next(t, 10*time.Second)
			for ok && e.kind != "message" {
				t.Logf("event %s %v", e.kind, e.err)
				e, ok = h.next(t, 10*time.Second)
			}
			if !ok || e.seq != uint64(j) || e.body != fmt.Sprintf("ack %d", j) {
				t.Fatalf("answer %d: %+v", j, e)
			}
		}
	}
}

// Negotiation, establishment, orders and their answers, termination.
func TestOrders(t *testing.T) {
	c, h, cancel, done := client(t, fixp.Idempotent)
	defer func() { cancel(); <-done }()
	h.expect(t, "established")
	const n = 5
	for i := 1; i <= n; i++ {
		if seq, err := c.Session().Send(context.Background(), order(fmt.Sprintf("order %d", i))); err != nil || seq != uint64(i) {
			t.Fatalf("Send: %d, %v", seq, err)
		}
	}
	for i := 1; i <= n; i++ {
		e := h.expect(t, "message")
		if e.seq != uint64(i) {
			t.Fatalf("answer %d: %+v", i, e)
		}
	}
	c.Session().Terminate(context.Background(), "")
	e := h.expect(t, "unbound")
	var te *fixp.TerminatedError
	if !errors.As(e.err, &te) || te.ByPeer || te.Code != fixp.TerminationFinished {
		t.Fatalf("unbound: %v", e.err)
	}
	// Answered, not timed out.
	if !h.wire.received(codec.TerminateTemplateID) {
		t.Fatal("no Terminate from Silverflash")
	}
}
