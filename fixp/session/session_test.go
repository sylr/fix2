package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

var bg = context.Background()

func TestUnsequenced(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced})
	cs, ss := p.client.Session(), p.serverSession(t)
	if cs.State() != StateEstablished || ss.Owner() != "alice" {
		t.Fatalf("client %s, owner %q", cs.State(), ss.Owner())
	}
	for _, body := range []string{"a", "b", "c"} {
		if seq, err := cs.Send(bg, appMsg{1, body}); err != nil || seq != 0 {
			t.Fatalf("Send: %d, %v", seq, err)
		}
	}
	for _, body := range []string{"a", "b", "c"} {
		if e := p.srvH.expect(t, "message"); e.body != body || e.seq != 0 {
			t.Fatalf("server got %v", e)
		}
	}
	if _, err := ss.Send(bg, appMsg{2, "back"}); err != nil {
		t.Fatal(err)
	}
	if e := p.clientH.expect(t, "message"); e.body != "back" {
		t.Fatalf("client got %v", e)
	}
	if n := cs.Stats().MessagesSent.Load(); n != 3 {
		t.Errorf("client sent %d", n)
	}
}

func TestKeepalive(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Idempotent})
	// Idle for ten intervals: heartbeats keep both ends up.
	p.clientH.quiet(t, 500*time.Millisecond)
	p.srvH.quiet(t, 0)
	cs, ss := p.client.Session(), p.serverSession(t)
	if cs.Stats().HeartbeatsSent.Load() < 5 || ss.Stats().HeartbeatsSent.Load() < 5 {
		t.Errorf("heartbeats: client %d, server %d", cs.Stats().HeartbeatsSent.Load(), ss.Stats().HeartbeatsSent.Load())
	}
}

func TestSequenced(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Idempotent, serverFlow: fixp.Recoverable})
	cs, ss := p.client.Session(), p.serverSession(t)
	for i := range 3 {
		if seq, err := cs.Send(bg, appMsg{1, "order"}); err != nil || seq != uint64(i+1) {
			t.Fatalf("Send: %d, %v", seq, err)
		}
	}
	for i := range 3 {
		if e := p.srvH.expect(t, "message"); e.seq != uint64(i+1) {
			t.Fatalf("server got %v", e)
		}
	}
	for i := range 2 {
		if seq, err := ss.Send(bg, appMsg{2, "report"}); err != nil || seq != uint64(i+1) {
			t.Fatalf("server Send: %d, %v", seq, err)
		}
	}
	if seq, err := ss.SendApplied(bg, 1, 3); err != nil || seq != 3 {
		t.Fatalf("SendApplied: %d, %v", seq, err)
	}
	p.clientH.expect(t, "message")
	p.clientH.expect(t, "message")
	if e := p.clientH.expect(t, "applied"); e.seq != 1 || e.body != "3" {
		t.Fatalf("Applied: %v", e)
	}
	// What the stores hold.
	sst, _ := p.srvStore.Open(bg, cs.ID())
	cst, _ := p.cliStore.Open(bg, cs.ID())
	if st := sst.State(); st.NextOutbound != 4 || st.Inbound != 3 || st.EstablishedAt.IsZero() || st.Owner != "alice" {
		t.Errorf("server store %+v", st)
	}
	// Recorded once the handler returned, after the event it sent.
	eventually(t, func() bool { return cst.State().Inbound == 3 })
	if st := cst.State(); st.NextOutbound != 4 {
		t.Errorf("client store %+v", st)
	}
	var stored []string
	sst.Range(bg, 1, 3, func(seq uint64, msg []byte) error { stored = append(stored, string(msg[8:])); return nil })
	if len(stored) != 3 || stored[0] != "report" || stored[1] != "report" {
		t.Errorf("server stored %q", stored)
	}
}

// Terminate unbinds; the client establishes the session again on a new
// connection, without negotiating.
func TestTerminateReestablish(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Idempotent, serverFlow: fixp.Idempotent})
	cs := p.client.Session()
	id := cs.ID()
	cs.Send(bg, appMsg{1, "one"})
	p.srvH.expect(t, "message")
	if err := cs.Terminate(bg, "bye"); err != nil {
		t.Fatal(err)
	}
	var te *fixp.TerminatedError
	if e := p.clientH.expect(t, "unbound"); !errors.As(e.err, &te) || te.ByPeer || te.Code != fixp.TerminationFinished {
		t.Fatalf("client unbound: %v", e.err)
	}
	if e := p.srvH.expect(t, "unbound"); !errors.As(e.err, &te) || !te.ByPeer || te.Reason != "bye" {
		t.Fatalf("server unbound: %v", e.err)
	}
	p.clientH.expect(t, "established")
	p.srvH.expect(t, "established")
	if cs.ID() != id || cs.Stats().Establishments.Load() != 2 {
		t.Fatalf("ID %s (was %s), %d establishments", cs.ID(), id, cs.Stats().Establishments.Load())
	}
	// The numbering goes on.
	if seq, err := cs.Send(bg, appMsg{1, "two"}); err != nil || seq != 2 {
		t.Fatalf("Send: %d, %v", seq, err)
	}
	if e := p.srvH.expect(t, "message"); e.seq != 2 || e.body != "two" {
		t.Fatalf("server got %v", e)
	}
	// The server terminates too.
	p.serverSession(t).Terminate(bg, "server's turn")
	if e := p.clientH.expect(t, "unbound"); !errors.As(e.err, &te) || !te.ByPeer || te.Reason != "server's turn" {
		t.Fatalf("client unbound: %v", e.err)
	}
	p.srvH.expect(t, "unbound")
	p.clientH.expect(t, "established")
}

// Messages sent while unbound go out on the next connection.
func TestSendWhileUnbound(t *testing.T) {
	// The server restarts on a new listener, which the client dials.
	var cur atomic.Pointer[mem.Listener]
	cur.Store(mem.Listen("server"))
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) { return cur.Load().Dial(ctx) })
	srvStore := memstore.New()
	srvH, cliH := newRecorder(), newRecorder()
	_, cancelSrv, srvDone := newServer(t, cur.Load(), srvStore, srvH, fixp.Unsequenced, nil)
	c := newClient(t, dialer, memstore.New(), cliH, fixp.Recoverable, nil)
	cancelCli, cliDone := runClient(c)
	defer func() { cancelCli(); <-cliDone }()
	cliH.expect(t, "established")
	srvH.expect(t, "established")

	cancelSrv()
	<-srvDone
	cliH.expect(t, "unbound")
	if seq, err := c.Session().Send(bg, appMsg{1, "queued"}); err != nil || seq != 1 {
		t.Fatalf("Send while unbound: %d, %v", seq, err)
	}
	// A new server on the same store: the session is known.
	l := mem.Listen("server")
	cur.Store(l)
	srvH = newRecorder()
	_, cancelSrv, srvDone = newServer(t, l, srvStore, srvH, fixp.Unsequenced, nil)
	defer func() { cancelSrv(); <-srvDone }()
	srvH.expect(t, "established")
	if e := srvH.expect(t, "message"); e.body != "queued" || e.seq != 1 {
		t.Fatalf("server got %v", e)
	}
}

func TestStopTerminates(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced})
	p.cancelCli()
	if err := <-p.cliDone; !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}
	p.cliDone <- nil // for Cleanup
	var te *fixp.TerminatedError
	if e := p.srvH.expect(t, "unbound"); !errors.As(e.err, &te) || !te.ByPeer || te.Code != fixp.TerminationFinished {
		t.Fatalf("server unbound: %v", e.err)
	}
	cs := p.client.Session()
	if cs.State() != StateStopped {
		t.Errorf("client %s", cs.State())
	}
	select {
	case <-cs.Done():
	default:
		t.Error("client session not done")
	}
	if _, err := cs.Send(bg, appMsg{1, "x"}); !errors.Is(err, ErrStopped) {
		t.Errorf("Send after stop: %v", err)
	}
}

func TestSendErrors(t *testing.T) {
	l := mem.Listen("nowhere")
	c := newClient(t, l, memstore.New(), newRecorder(), fixp.Idempotent, nil)
	if _, err := c.Session().Send(bg, appMsg{1, "x"}); !errors.Is(err, ErrUnnegotiated) {
		t.Errorf("unnegotiated: %v", err)
	}

	p := newPair(t, pairOptions{clientFlow: fixp.None, serverFlow: fixp.Unsequenced})
	cs := p.client.Session()
	if _, err := cs.Send(bg, appMsg{1, "x"}); !errors.Is(err, fixp.ErrNoFlow) {
		t.Errorf("None flow: %v", err)
	}
	ss := p.serverSession(t)
	if _, err := ss.Send(bg, &codec.Sequence{NextSeqNo: 1}); !errors.Is(err, ErrSchema) {
		t.Errorf("FIXP schema: %v", err)
	}
	if _, err := ss.SendRaw(bg, []byte{1, 2, 3}); !errors.Is(err, ErrSchema) {
		t.Errorf("short: %v", err)
	}
	if _, err := ss.SendApplied(bg, 1, 1); !errors.Is(err, fixp.ErrNoFlow) {
		t.Errorf("Applied on an Unsequenced flow: %v", err)
	}
}

// From a callback, Send does not wait for room.
func TestSendFromCallbackQueueFull(t *testing.T) {
	result := make(chan error, 1)
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced, server: func(c *ServerConfig) {
		c.MaxPendingMessages = 1
	}})
	p.srvH.onMessage = func(ctx context.Context, s *Session, _ []byte, _ Inbound) error {
		// Fill the queue faster than the writer: one fits, one does not.
		var err error
		for range 1000 {
			if _, err = s.Send(ctx, appMsg{2, "x"}); err != nil {
				break
			}
		}
		result <- err
		return nil
	}
	p.client.Session().Send(bg, appMsg{1, "go"})
	if err := <-result; !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Send from a callback: %v", err)
	}
}

// A handler failing a message terminates the connection.
func TestHandlerError(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Recoverable, serverFlow: fixp.Unsequenced})
	p.srvH.onMessage = func(context.Context, *Session, []byte, Inbound) error { return errors.New("cannot apply") }
	p.client.Session().Send(bg, appMsg{1, "x"})
	p.srvH.expect(t, "message")
	var te *fixp.TerminatedError
	if e := p.srvH.expect(t, "unbound"); !errors.As(e.err, &te) || te.ByPeer || te.Code != fixp.TerminationUnspecifiedError {
		t.Fatalf("server unbound: %v", e.err)
	}
	if e := p.clientH.expect(t, "unbound"); !errors.As(e.err, &te) || !te.ByPeer || te.Reason != "application error" {
		t.Fatalf("client unbound: %v", e.err)
	}
	// Not recorded as delivered: the frontier did not move.
	if st, _ := p.srvStore.Open(bg, p.client.Session().ID()); st.State().Inbound != 0 {
		t.Errorf("server Inbound %d", st.State().Inbound)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name   string
		server func(*ServerConfig)
		client func(*ClientConfig)
		flow   fixp.FlowType
		want   func(error) bool
	}{
		{"credentials", nil, func(c *ClientConfig) { c.Credentials = nil }, fixp.Unsequenced, func(err error) bool {
			var e *fixp.NegotiationRejectedError
			return errors.As(err, &e) && e.Code == fixp.NegotiationCredentials
		}},
		{"client flow", func(c *ServerConfig) { c.ClientFlows = []fixp.FlowType{fixp.Idempotent} }, nil, fixp.Unsequenced, func(err error) bool {
			var e *fixp.NegotiationRejectedError
			return errors.As(err, &e) && e.Code == fixp.NegotiationFlowTypeNotSupported
		}},
		{"None both ways", nil, nil, fixp.None, func(err error) bool {
			var e *fixp.NegotiationRejectedError
			return errors.As(err, &e) && e.Code == fixp.NegotiationFlowTypeNotSupported
		}},
		{"server flow", nil, func(c *ClientConfig) { c.ServerFlows = []fixp.FlowType{fixp.Recoverable} }, fixp.Unsequenced, func(err error) bool {
			return err != nil && errors.Is(err, err) && err.Error() == "fixp/session: the server's flow, None, is not one we accept"
		}},
		{"keepalive interval", func(c *ServerConfig) { c.MinKeepalive = time.Second }, nil, fixp.Unsequenced, func(err error) bool {
			var e *fixp.EstablishmentRejectedError
			return errors.As(err, &e) && e.Code == fixp.EstablishmentKeepaliveInterval
		}},
		{"rejection chosen by Authenticate", func(c *ServerConfig) {
			c.Authenticate = func(context.Context, *AuthRequest) (string, error) {
				return "", &fixp.NegotiationRejectedError{Code: fixp.NegotiationUnspecified, Reason: "closed today"}
			}
		}, nil, fixp.Unsequenced, func(err error) bool {
			var e *fixp.NegotiationRejectedError
			return errors.As(err, &e) && e.Code == fixp.NegotiationUnspecified && e.Reason == "closed today"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := mem.Listen("server")
			_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.None, tc.server)
			defer func() { cancel(); <-done }()
			c := newClient(t, l, memstore.New(), newRecorder(), tc.flow, tc.client)
			ctx, cancelRun := context.WithTimeout(bg, 5*time.Second)
			defer cancelRun()
			if err := c.Run(ctx); !tc.want(err) {
				t.Fatalf("Run: %v", err)
			}
			if c.Session().State() != StateStopped {
				t.Errorf("state %s", c.Session().State())
			}
		})
	}
}

// A session ID alone does not give a session: the Establish must
// authenticate as its owner.
func TestOwnerBinding(t *testing.T) {
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	var resp codec.NegotiationResponse
	if err := resp.Decode(r.negotiate(fixp.Unsequenced, "user:alice")); err != nil || resp.SessionId != r.id {
		t.Fatalf("negotiation: %+v, %v", resp, err)
	}
	mallory := dialRaw(t, l)
	mallory.id = r.id
	var rej codec.EstablishmentReject
	if err := rej.Decode(mallory.establish(1000, codec.Optional[uint64]{}, "user:mallory")); err != nil || rej.Code != codec.EstablishmentRejectCodeCredentials {
		t.Fatalf("mallory: %+v, %v", rej, err)
	}
	mallory.closed()
	var ack codec.EstablishmentAck
	if err := ack.Decode(r.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil || ack.KeepaliveInterval != 50 {
		t.Fatalf("alice: %+v, %v", ack, err)
	}
}

func TestDuplicateAndAlreadyEstablished(t *testing.T) {
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.negotiate(fixp.Unsequenced, "user:alice")
	again := dialRaw(t, l)
	again.id = r.id
	var rej codec.NegotiationReject
	if err := rej.Decode(again.negotiate(fixp.Unsequenced, "user:alice")); err != nil || rej.Code != codec.NegotiationRejectCodeDuplicateId {
		t.Fatalf("second Negotiate: %+v, %v", rej, err)
	}
	r.establish(1000, codec.Optional[uint64]{}, "user:alice")
	other := dialRaw(t, l)
	other.id = r.id
	var erej codec.EstablishmentReject
	if err := erej.Decode(other.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil || erej.Code != codec.EstablishmentRejectCodeAlreadyEstablished {
		t.Fatalf("second Establish: %+v, %v", erej, err)
	}
	unknown := dialRaw(t, l)
	if err := erej.Decode(unknown.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil || erej.Code != codec.EstablishmentRejectCodeUnnegotiated {
		t.Fatalf("unknown session: %+v, %v", erej, err)
	}
}

func TestNegotiateChecks(t *testing.T) {
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	var rej codec.NegotiationReject

	r := dialRaw(t, l)
	r.id = fixp.SessionID{}
	if err := rej.Decode(r.negotiate(fixp.Unsequenced, "user:alice")); err != nil || rej.Code != codec.NegotiationRejectCodeUnspecified {
		t.Fatalf("nil UUID: %+v, %v", rej, err)
	}

	r = dialRaw(t, l)
	r.send(&codec.Negotiate{SessionId: r.id, Timestamp: nanos(time.Now().Add(-time.Hour)), ClientFlow: codec.FlowTypeUnsequenced, Credentials: []byte("user:alice")})
	if err := rej.Decode(r.readTemplate(codec.NegotiationRejectTemplateID)); err != nil || rej.Code != codec.NegotiationRejectCodeUnspecified || string(rej.Reason) != "timestamp out of range" {
		t.Fatalf("old timestamp: %+v, %v", rej, err)
	}

	r = dialRaw(t, l)
	big := make([]byte, 2000)
	copy(big, "user:alice")
	r.send(&codec.Negotiate{SessionId: r.id, Timestamp: nanos(time.Now()), ClientFlow: codec.FlowTypeUnsequenced, Credentials: big})
	if err := rej.Decode(r.readTemplate(codec.NegotiationRejectTemplateID)); err != nil || rej.Code != codec.NegotiationRejectCodeCredentials {
		t.Fatalf("long credentials: %+v, %v", rej, err)
	}

	// A first message other than Negotiate or Establish closes.
	r = dialRaw(t, l)
	r.send(&codec.Sequence{NextSeqNo: 1})
	r.closed()
}

// A session negotiated and never established is retired, its ID refused.
func TestRetireUnestablished(t *testing.T) {
	l := mem.Listen("server")
	st := memstore.New()
	h := newRecorder()
	srv, cancel, done := newServer(t, l, st, h, fixp.Unsequenced, func(c *ServerConfig) {
		c.UnestablishedTimeout = 100 * time.Millisecond
	})
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.negotiate(fixp.Unsequenced, "user:alice")
	if srv.Session(r.id) == nil {
		t.Fatal("no session")
	}
	h.expect(t, "finalized")
	if srv.Session(r.id) != nil {
		t.Fatal("session not retired")
	}
	ss, err := st.Open(bg, r.id)
	if err != nil || !ss.State().Retired {
		t.Fatalf("store: %+v, %v", ss, err)
	}
	again := dialRaw(t, l)
	again.id = r.id
	var rej codec.NegotiationReject
	if err := rej.Decode(again.negotiate(fixp.Unsequenced, "user:alice")); err != nil || rej.Code != codec.NegotiationRejectCodeDuplicateId {
		t.Fatalf("Negotiate of a retired ID: %+v, %v", rej, err)
	}
}

func TestRetire(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced})
	id := p.client.Session().ID()
	if err := p.srv.Retire(bg, id); err != nil {
		t.Fatal(err)
	}
	p.srvH.expect(t, "unbound")
	p.srvH.expect(t, "finalized")
	p.clientH.expect(t, "unbound")
	// The client's next Establish is refused: the session is over.
	if err := <-p.cliDone; err == nil {
		t.Fatal("client Run returned nil")
	} else {
		var e *fixp.EstablishmentRejectedError
		if !errors.As(err, &e) || e.Code != fixp.EstablishmentUnnegotiated {
			t.Fatalf("client Run: %v", err)
		}
	}
	p.cliDone <- nil
	if err := p.srv.Retire(bg, id); !errors.Is(err, ErrNoSession) {
		t.Errorf("Retire again: %v", err)
	}
}

// The peer's silence beyond its keepalive interval ends the connection.
func TestKeepaliveLapsed(t *testing.T) {
	l := mem.Listen("server")
	h := newRecorder()
	_, cancel, done := newServer(t, l, memstore.New(), h, fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.negotiate(fixp.Unsequenced, "user:alice")
	r.establish(100, codec.Optional[uint64]{}, "user:alice")
	h.expect(t, "established")
	start := time.Now()
	var term codec.Terminate
	if err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil || term.Code != codec.TerminationCodeUnspecifiedError || string(term.Reason) != "keepalive interval lapsed" {
		t.Fatalf("Terminate: %+v, %v", term, err)
	}
	if d := time.Since(start); d < 140*time.Millisecond || d > time.Second {
		t.Errorf("terminated after %v, the interval being 100ms, leniency 1.5", d)
	}
	r.closed()
	if e := h.expect(t, "unbound"); !errors.Is(e.err, fixp.ErrKeepaliveLapsed) {
		t.Errorf("unbound: %v", e.err)
	}
}

func TestProtocolViolations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flow   fixp.FlowType
		send   func(r *raw)
		reason string
	}{
		{"Sequence on an Unsequenced flow", fixp.Unsequenced, func(r *raw) { r.send(&codec.Sequence{NextSeqNo: 1}) }, "Sequence on an Unsequenced flow"},
		{"Topic", fixp.Unsequenced, func(r *raw) { r.send(&codec.Topic{SessionId: r.id}) }, "codec: Topic or Context on a point-to-point session"},
		{"application message before Sequence", fixp.Idempotent, func(r *raw) { r.send(appMsg{1, "x"}) }, "application message before a Sequence"},
		{"Sequence going back", fixp.Idempotent, func(r *raw) {
			r.send(&codec.Sequence{NextSeqNo: 1})
			r.send(appMsg{1, "x"})
			r.send(&codec.Sequence{NextSeqNo: 1})
		}, "Sequence 1 below the next sequence number expected, 2"},
		{"application message on a None flow", fixp.None, func(r *raw) { r.send(appMsg{1, "x"}) }, "application message on a None flow"},
		{"Negotiate when established", fixp.Unsequenced, func(r *raw) {
			r.send(&codec.Negotiate{SessionId: r.id, Timestamp: nanos(time.Now())})
		}, "unexpected session message, template 1"},
		{"malformed frame", fixp.Unsequenced, func(r *raw) { r.sendBytes([]byte{0, 0, 0, 2, 0xeb, 0x50}) }, "malformed frame"},
		{"RetransmitRequest on an Idempotent flow", fixp.Idempotent, func(r *raw) {
			r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: nanos(time.Now()), FromSeqNo: 1, Count: 1})
		}, "RetransmitRequest on an Unsequenced flow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := mem.Listen("server")
			h := newRecorder()
			_, cancel, done := newServer(t, l, memstore.New(), h, fixp.Unsequenced, nil)
			defer func() { cancel(); <-done }()
			r := dialRaw(t, l)
			r.negotiate(tc.flow, "user:alice")
			r.establish(1000, codec.Optional[uint64]{}, "user:alice")
			h.expect(t, "established")
			tc.send(r)
			var term codec.Terminate
			if err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil || term.Code != codec.TerminationCodeUnspecifiedError || string(term.Reason) != tc.reason {
				t.Fatalf("Terminate: %+v %q, %v", term, term.Reason, err)
			}
			r.closed()
			e := h.next(t)
			for e.kind == "message" {
				e = h.next(t)
			}
			if e.kind != "unbound" || !errors.Is(e.err, fixp.ErrProtocol) {
				t.Errorf("unbound: %v", e)
			}
		})
	}
}

// A client whose negotiation goes unanswered, or answered for another
// request, negotiates again under a new ID.
func TestClientNegotiationRetries(t *testing.T) {
	l := mem.Listen("server")
	h := newRecorder()
	c := newClient(t, l, memstore.New(), h, fixp.Unsequenced, func(c *ClientConfig) {
		c.HandshakeTimeout = 100 * time.Millisecond
	})
	cancel, done := runClient(c)
	defer func() { cancel(); <-done }()

	// First attempt: no answer.
	r := acceptRaw(t, l)
	var n1 codec.Negotiate
	if err := n1.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil {
		t.Fatal(err)
	}
	r.closed()

	// Second: a new ID; an answer of another request, then its own.
	r = acceptRaw(t, l)
	var n2 codec.Negotiate
	if err := n2.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil {
		t.Fatal(err)
	}
	if n2.SessionId == n1.SessionId {
		t.Fatal("same ID after a timeout")
	}
	r.send(&codec.NegotiationResponse{SessionId: n2.SessionId, RequestTimestamp: n2.Timestamp - 1, ServerFlow: codec.FlowTypeUnsequenced})
	r.send(&codec.NegotiationResponse{SessionId: n2.SessionId, RequestTimestamp: n2.Timestamp, ServerFlow: codec.FlowTypeUnsequenced})
	var est codec.Establish
	if err := est.Decode(r.readTemplate(codec.EstablishTemplateID)); err != nil || est.SessionId != n2.SessionId || est.KeepaliveInterval != 50 || est.NextSeqNo.Valid {
		t.Fatalf("Establish: %+v, %v", est, err)
	}
	r.send(&codec.EstablishmentAck{SessionId: est.SessionId, RequestTimestamp: est.Timestamp, KeepaliveInterval: 1000})
	h.expect(t, "established")
	if c.Session().ID() != n2.SessionId || c.Session().State() != StateEstablished {
		t.Fatalf("session %s, %s", c.Session().ID(), c.Session().State())
	}
}

// A client resumes the session its store holds.
func TestClientResumes(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Idempotent, serverFlow: fixp.Unsequenced})
	cs := p.client.Session()
	id := cs.ID()
	cs.Send(bg, appMsg{1, "a"})
	p.srvH.expect(t, "message")
	p.cancelCli()
	<-p.cliDone
	p.srvH.expect(t, "unbound")

	h := newRecorder()
	c := newClient(t, p.l, p.cliStore, h, fixp.Idempotent, nil)
	if c.Session().ID() != id || c.Session().State() != StateUnbound || c.Session().NextSeqNo() != 2 {
		t.Fatalf("resumed %s %s next %d", c.Session().ID(), c.Session().State(), c.Session().NextSeqNo())
	}
	p.cancelCli, p.cliDone = runClient(c)
	h.expect(t, "established")
	p.srvH.expect(t, "established")
	c.Session().Send(bg, appMsg{1, "b"})
	if e := p.srvH.expect(t, "message"); e.seq != 2 || e.body != "b" {
		t.Fatalf("server got %v", e)
	}
}

func TestConfigErrors(t *testing.T) {
	var ce *ConfigError
	if _, err := NewServer(ServerConfig{}); !errors.As(err, &ce) {
		t.Errorf("empty server config: %v", err)
	}
	if _, err := NewClient(ClientConfig{Config: Config{Handler: BaseHandler{}}}, mem.Listen("x")); !errors.As(err, &ce) || ce.Field != "Store" {
		t.Errorf("client without store: %v", err)
	}
	if _, err := NewClient(ClientConfig{Config: Config{Handler: BaseHandler{}}, Store: memstore.New(), Flow: 7}, mem.Listen("x")); !errors.As(err, &ce) || ce.Field != "Flow" {
		t.Errorf("bad flow: %v", err)
	}
	two := memstore.New()
	two.Create(bg, store.Meta{ID: fixp.NewSessionID(), Role: store.Client})
	two.Create(bg, store.Meta{ID: fixp.NewSessionID(), Role: store.Client})
	if _, err := NewClient(ClientConfig{Config: Config{Handler: BaseHandler{}}, Store: two}, mem.Listen("x")); err == nil {
		t.Error("two client sessions in the store")
	}
}
