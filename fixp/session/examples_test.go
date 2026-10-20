package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// The examples of FIXP 1.0's section 7 the other tests do not already
// run, each test named after its example; docs/fixp-examples.md maps every
// example to its tests, or to the divergence docs/fixp.md records.

// until reads, skipping what comes before, the next session message of
// template.
func (r *raw) until(template uint16) []byte {
	r.t.Helper()
	for {
		h, msg := r.read()
		if h.SchemaId == codec.SchemaID && h.TemplateId == template {
			return msg
		}
	}
}

// terminated reads until the Terminate, and checks its code and reason.
func (r *raw) terminated(code fixp.TerminationCode, reason string) {
	r.t.Helper()
	var term codec.Terminate
	if _, err := term.Decode(r.until(codec.TerminateTemplateID)); err != nil || fixp.TerminationCode(term.Code) != code || string(term.Reason) != reason {
		r.t.Fatalf("Terminate %v %q, want %v %q: %v", fixp.TerminationCode(term.Code), term.Reason, code, reason, err)
	}
}

// until returns the next event of kind, skipping others.
func (r *recorder) until(t testing.TB, kind string) event {
	t.Helper()
	for {
		if e := r.next(t); e.kind == kind {
			return e
		}
	}
}

// rawClient is a raw peer negotiated and established on a server of
// serverFlow, its own flow clientFlow, with keepalive.
func rawClient(t *testing.T, clientFlow, serverFlow fixp.FlowType, keepalive uint32) (*raw, *recorder, *Server, *mem.Listener) {
	t.Helper()
	return rawClientWith(t, clientFlow, serverFlow, keepalive, nil)
}

// rawClientWith is rawClient, opt configuring the server.
func rawClientWith(t *testing.T, clientFlow, serverFlow fixp.FlowType, keepalive uint32, opt func(*ServerConfig)) (*raw, *recorder, *Server, *mem.Listener) {
	t.Helper()
	l := mem.Listen("server")
	h := newRecorder()
	srv, cancel, done := newServer(t, l, memstore.New(), h, serverFlow, opt)
	t.Cleanup(func() { cancel(); <-done })
	r := dialRaw(t, l)
	r.negotiate(clientFlow, "user:alice")
	// A Recoverable flow may be numbered by the Establish, accepted on a
	// first establishment too; an Idempotent one by a Sequence (§4.8).
	var next codec.Optional[uint64]
	if clientFlow == fixp.Recoverable {
		next = codec.Some(uint64(1))
	}
	var ack codec.EstablishmentAck
	if _, err := ack.Decode(r.establish(keepalive, next, "user:alice")); err != nil {
		t.Fatal(err)
	}
	h.expect(t, "established")
	if clientFlow == fixp.Idempotent {
		r.send(&codec.Sequence{NextSeqNo: 1})
	}
	return r, h, srv, l
}

// clientOfRaw runs a client of flow against raw servers, its handshake
// timeout short.
func clientOfRaw(t *testing.T, flow fixp.FlowType) (*Client, *recorder, *mem.Listener) {
	t.Helper()
	c, h, l, _ := clientOfRawWith(t, flow, nil)
	return c, h, l
}

// clientOfRawWith is clientOfRaw, opt configuring the client; done gives
// what Run returned.
func clientOfRawWith(t *testing.T, flow fixp.FlowType, opt func(*ClientConfig)) (*Client, *recorder, *mem.Listener, chan error) {
	t.Helper()
	l := mem.Listen("server")
	h := newRecorder()
	c := newClient(t, l, memstore.New(), h, flow, func(c *ClientConfig) {
		c.HandshakeTimeout = 100 * time.Millisecond
		if opt != nil {
			opt(c)
		}
	})
	cancel, done := runClient(c)
	ran := make(chan error, 1)
	stopped := make(chan struct{})
	go func() { err := <-done; ran <- err; close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	return c, h, l, ran
}

// answerNegotiate reads a Negotiate and accepts it, with serverFlow.
func (r *raw) answerNegotiate(serverFlow fixp.FlowType) codec.Negotiate {
	r.t.Helper()
	var n codec.Negotiate
	if _, err := n.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil {
		r.t.Fatal(err)
	}
	r.id = n.SessionId
	r.send(&codec.NegotiationResponse{SessionId: n.SessionId, RequestTimestamp: n.Timestamp, ServerFlow: codec.FlowType(serverFlow)})
	return n
}

// readEstablish reads an Establish.
func (r *raw) readEstablish() codec.Establish {
	r.t.Helper()
	var e codec.Establish
	if _, err := e.Decode(r.readTemplate(codec.EstablishTemplateID)); err != nil {
		r.t.Fatal(err)
	}
	return e
}

// §7.1.1 to §7.1.6, §7.2.1 to §7.2.3: what a server answers, field by
// field, for each pair of flows the examples negotiate and establish. The
// raw client sends a sequenced flow's NextSeqNo on the first
// establishment, as the examples do: accepted. The ack carries a
// Recoverable server flow's, not an Idempotent one's (§4.3.2).
func TestExample_7_1_and_7_2_Handshakes(t *testing.T) {
	for _, tc := range []struct {
		example            string
		client, serverFlow fixp.FlowType
	}{
		{"7.1.1, 7.2.1", fixp.Recoverable, fixp.Recoverable},
		{"7.1.2, 7.2.2", fixp.Unsequenced, fixp.Unsequenced},
		{"7.1.3, 7.2.3", fixp.Idempotent, fixp.Recoverable},
		{"Idempotent both ways", fixp.Idempotent, fixp.Idempotent},
		{"7.1.4 as titled", fixp.None, fixp.Recoverable},
		{"7.1.4 as shown, 7.1.6", fixp.None, fixp.Unsequenced},
		{"7.1.5", fixp.Unsequenced, fixp.Recoverable},
	} {
		t.Run(tc.example, func(t *testing.T) {
			l := mem.Listen("server")
			h := newRecorder()
			_, cancel, done := newServer(t, l, memstore.New(), h, tc.serverFlow, nil)
			defer func() { cancel(); <-done }()
			r := dialRaw(t, l)
			var resp codec.NegotiationResponse
			if _, err := resp.Decode(r.negotiate(tc.client, "user:alice")); err != nil {
				t.Fatal(err)
			}
			if resp.SessionId != r.id || resp.RequestTimestamp != r.ts || fixp.FlowType(resp.ServerFlow) != tc.serverFlow {
				t.Fatalf("NegotiationResponse %+v", resp)
			}
			var next codec.Optional[uint64]
			if tc.client.Sequenced() {
				next = codec.Some(uint64(1))
			}
			var ack codec.EstablishmentAck
			if _, err := ack.Decode(r.establish(1000, next, "user:alice")); err != nil {
				t.Fatal(err)
			}
			// A Recoverable server flow's only, the first establishment
			// included (§4.3.2).
			wantNext := codec.Optional[uint64]{}
			if tc.serverFlow == fixp.Recoverable {
				wantNext = codec.Some(uint64(1))
			}
			if ack.SessionId != r.id || ack.RequestTimestamp != r.ts || ack.KeepaliveInterval != 50 || ack.NextSeqNo != wantNext {
				t.Fatalf("EstablishmentAck %+v", ack)
			}
			h.expect(t, "established")
		})
	}
}

// §7.1.4 as titled: a None client flow, a Recoverable server flow; the
// server's messages come in order, the client sends none.
func TestExample_7_1_4_NoneRecoverable(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.None, serverFlow: fixp.Recoverable})
	ss := p.serverSession(t)
	for i := 1; i <= 3; i++ {
		if seq, err := ss.Send(bg, appMsg{1, fmt.Sprint(i)}); err != nil || seq != uint64(i) {
			t.Fatalf("Send: %d, %v", seq, err)
		}
	}
	for i := 1; i <= 3; i++ {
		if e := p.clientH.expect(t, "message"); e.seq != uint64(i) || e.body != fmt.Sprint(i) {
			t.Fatalf("message %v", e)
		}
	}
	if _, err := p.client.Session().Send(bg, appMsg{1, "x"}); !errors.Is(err, fixp.ErrNoFlow) {
		t.Fatalf("Send on a None flow: %v", err)
	}
}

// §7.1.7.4: a Negotiate timestamped in seconds, not nanoseconds.
func TestExample_7_1_7_4_TimestampInSeconds(t *testing.T) {
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.send(&codec.Negotiate{SessionId: r.id, Timestamp: 86400, ClientFlow: codec.FlowTypeUnsequenced, Credentials: []byte("user:alice")})
	var rej codec.NegotiationReject
	if _, err := rej.Decode(r.readTemplate(codec.NegotiationRejectTemplateID)); err != nil {
		t.Fatal(err)
	}
	if rej.SessionId != r.id || rej.RequestTimestamp != 86400 || fixp.NegotiationRejectCode(rej.Code) != fixp.NegotiationUnspecified || string(rej.Reason) != "timestamp out of range" {
		t.Fatalf("NegotiationReject %+v %q", rej, rej.Reason)
	}
}

// §7.1.7.5: a NegotiationResponse of another session ID is not the
// client's: it waits for its own, then negotiates again under a new ID.
func TestExample_7_1_7_5_ResponseOfAnotherSession(t *testing.T) {
	_, h, l := clientOfRaw(t, fixp.Unsequenced)
	r := acceptRaw(t, l)
	var n1 codec.Negotiate
	if _, err := n1.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil {
		t.Fatal(err)
	}
	r.send(&codec.NegotiationResponse{SessionId: fixp.NewSessionID(), RequestTimestamp: n1.Timestamp, ServerFlow: codec.FlowTypeUnsequenced})
	r.closed()
	r = acceptRaw(t, l)
	var n2 codec.Negotiate
	if _, err := n2.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil || n2.SessionId == n1.SessionId {
		t.Fatalf("Negotiate again: %+v, %v", n2, err)
	}
	h.quiet(t, 50*time.Millisecond)
}

// §7.2.5.2 on one connection: an Establish again where the session is
// established is redundant, rejected AlreadyEstablished, its reason
// bounded; the connection stays. One for another session ends it.
func TestExample_7_2_5_2_AlreadyEstablishedHere(t *testing.T) {
	r, h, _, _ := rawClientWith(t, fixp.Idempotent, fixp.Recoverable, 1000, func(c *ServerConfig) { c.MaxReasonLength = 11 })
	ts := nanos(time.Now())
	r.send(&codec.Establish{SessionId: r.id, Timestamp: ts, KeepaliveInterval: 1000, NextSeqNo: codec.Some(uint64(1)), Credentials: []byte("user:alice")})
	var rej codec.EstablishmentReject
	if _, err := rej.Decode(r.until(codec.EstablishmentRejectTemplateID)); err != nil {
		t.Fatal(err)
	}
	if rej.SessionId != r.id || rej.RequestTimestamp != ts || fixp.EstablishmentRejectCode(rej.Code) != fixp.EstablishmentAlreadyEstablished || string(rej.Reason) != "established" {
		t.Fatalf("EstablishmentReject %+v %q", rej, rej.Reason)
	}
	// Still established: the Establish ended the numbering, a Sequence
	// starts it again.
	r.send(&codec.Sequence{NextSeqNo: 1})
	r.send(appMsg{1, "a"})
	if e := h.expect(t, "message"); e.seq != 1 || e.body != "a" {
		t.Fatalf("message %v", e)
	}
	other := fixp.NewSessionID()
	r.send(&codec.Establish{SessionId: other, Timestamp: nanos(time.Now()), KeepaliveInterval: 1000})
	r.terminated(fixp.TerminationUnspecifiedError, "Establish f") // "for another session", bounded
}

// §7.2.5.3: a blocked session, the application's Authenticate hook saying
// so; a client keeps trying.
func TestExample_7_2_5_3_SessionBlocked(t *testing.T) {
	l := mem.Listen("server")
	var establishes atomic.Int64
	_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, func(c *ServerConfig) {
		c.Authenticate = func(ctx context.Context, req *AuthRequest) (string, error) {
			if req.Establish {
				establishes.Add(1)
				return "", &fixp.EstablishmentRejectedError{Code: fixp.EstablishmentSessionBlocked, Reason: "blocked, call us"}
			}
			return authenticate(ctx, req)
		}
	})
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.negotiate(fixp.Unsequenced, "user:alice")
	var rej codec.EstablishmentReject
	if _, err := rej.Decode(r.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil {
		t.Fatal(err)
	}
	if rej.RequestTimestamp != r.ts || fixp.EstablishmentRejectCode(rej.Code) != fixp.EstablishmentSessionBlocked || string(rej.Reason) != "blocked, call us" {
		t.Fatalf("EstablishmentReject %+v %q", rej, rej.Reason)
	}

	c := newClient(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	ccancel, cdone := runClient(c)
	defer func() { ccancel(); <-cdone }()
	eventually(t, func() bool { return establishes.Load() >= 4 })
	select {
	case err := <-cdone:
		cdone <- err
		t.Fatalf("Run returned: %v", err)
	default:
	}
}

// §7.2.5.5: an Establish whose session ID is not one.
func TestExample_7_2_5_5_InvalidSessionID(t *testing.T) {
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.negotiate(fixp.Unsequenced, "user:alice")
	r.id = fixp.SessionID{}
	var rej codec.EstablishmentReject
	if _, err := rej.Decode(r.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil {
		t.Fatal(err)
	}
	if fixp.EstablishmentRejectCode(rej.Code) != fixp.EstablishmentUnspecified || string(rej.Reason) != "session ID is not a version 4 UUID" {
		t.Fatalf("EstablishmentReject %+v %q", rej, rej.Reason)
	}
}

// §7.2.5.6: an Establish timestamped in seconds.
func TestExample_7_2_5_6_TimestampInSeconds(t *testing.T) {
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.negotiate(fixp.Unsequenced, "user:alice")
	r.send(&codec.Establish{SessionId: r.id, Timestamp: 86400, KeepaliveInterval: 1000, Credentials: []byte("user:alice")})
	var rej codec.EstablishmentReject
	if _, err := rej.Decode(r.readTemplate(codec.EstablishmentRejectTemplateID)); err != nil {
		t.Fatal(err)
	}
	if rej.RequestTimestamp != 86400 || fixp.EstablishmentRejectCode(rej.Code) != fixp.EstablishmentUnspecified || string(rej.Reason) != "timestamp out of range" {
		t.Fatalf("EstablishmentReject %+v %q", rej, rej.Reason)
	}
}

// §7.2.5.8, §7.2.5.9: an Establish unanswered, then one answered by
// another request's ack: the client connects again each time, under the
// same session ID, without negotiating; its own ack establishes it.
func TestExample_7_2_5_8_and_9_EstablishUnanswered(t *testing.T) {
	c, h, l := clientOfRaw(t, fixp.Unsequenced)
	r := acceptRaw(t, l)
	n := r.answerNegotiate(fixp.Unsequenced)
	e1 := r.readEstablish()
	r.closed()

	r = acceptRaw(t, l)
	e2 := r.readEstablish()
	if e2.SessionId != n.SessionId || e2.Timestamp == e1.Timestamp {
		t.Fatalf("Establish again: %+v", e2)
	}
	r.send(&codec.EstablishmentAck{SessionId: e2.SessionId, RequestTimestamp: e2.Timestamp - 1, KeepaliveInterval: 1000})
	r.closed()

	r = acceptRaw(t, l)
	e3 := r.readEstablish()
	if e3.SessionId != n.SessionId {
		t.Fatalf("Establish again: %+v", e3)
	}
	r.send(&codec.EstablishmentAck{SessionId: e3.SessionId, RequestTimestamp: e3.Timestamp, KeepaliveInterval: 1000})
	h.expect(t, "established")
	if c.Session().ID() != fixp.SessionID(n.SessionId) {
		t.Fatalf("session %s", c.Session().ID())
	}
}

// §7.3.1, §7.3.2: a keepalive lapsed, a Sequence going back: the server
// terminates; the client establishes the session again, on a new
// connection, under its ID, numbering where it was.
func TestExample_7_3_1_and_7_3_2_EstablishAgain(t *testing.T) {
	for _, tc := range []struct {
		example string
		ka      uint32
		send    func(r *raw)
		reason  string
		next    uint64 // the client's, establishing again
	}{
		{"7.3.1", 20, func(*raw) {}, "keepalive interval lapsed", 1},
		{"7.3.2", 1000, func(r *raw) {
			r.send(appMsg{1, "1"})
			r.send(&codec.Sequence{NextSeqNo: 1})
		}, "Sequence 1 below the next sequence number expected, 2", 2},
	} {
		t.Run(tc.example, func(t *testing.T) {
			r, h, _, l := rawClient(t, fixp.Idempotent, fixp.Recoverable, tc.ka)
			tc.send(r)
			r.terminated(fixp.TerminationUnspecifiedError, tc.reason)
			r.closed()
			h.until(t, "unbound")
			r2 := dialRaw(t, l)
			r2.id = r.id
			var ack codec.EstablishmentAck
			if _, err := ack.Decode(r2.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil || ack.SessionId != r.id {
				t.Fatalf("EstablishmentAck %+v, %v", ack, err)
			}
			h.expect(t, "established")
			r2.send(&codec.Sequence{NextSeqNo: tc.next})
			r2.send(appMsg{1, "again"})
			if e := h.expect(t, "message"); e.seq != tc.next || e.body != "again" {
				t.Fatalf("message %v", e)
			}
		})
	}
}

// §7.3.3: an EstablishmentAck whose NextSeqNo is below what the client
// received: the server lost messages it sent; the client terminates, and
// tries again. A server answers the same of a client with
// EstablishmentReject.
func TestExample_7_3_3_NextSeqNoGoingBack(t *testing.T) {
	t.Run("client", func(t *testing.T) {
		_, h, l := clientOfRaw(t, fixp.Idempotent)
		r := acceptRaw(t, l)
		r.answerNegotiate(fixp.Recoverable)
		e := r.readEstablish()
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000, NextSeqNo: codec.Some(uint64(1))})
		h.expect(t, "established")
		for i := 1; i <= 3; i++ {
			r.send(appMsg{1, fmt.Sprint(i)})
			h.expect(t, "message")
		}
		r.c.Close()
		h.until(t, "unbound")

		r = acceptRaw(t, l)
		e = r.readEstablish()
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000, NextSeqNo: codec.Some(uint64(2))})
		r.terminated(fixp.TerminationUnspecifiedError, "NextSeqNo 2 below the next sequence number expected, 4: the peer lost messages it sent")
		r.closed()
		r = acceptRaw(t, l)
		r.readEstablish()
	})
	t.Run("server", func(t *testing.T) {
		r, h, _, l := rawClient(t, fixp.Recoverable, fixp.Unsequenced, 1000)
		for i := 1; i <= 3; i++ {
			r.send(appMsg{1, fmt.Sprint(i)})
			h.expect(t, "message")
		}
		r.c.Close()
		h.until(t, "unbound")
		r2 := dialRaw(t, l)
		r2.id = r.id
		var rej codec.EstablishmentReject
		if _, err := rej.Decode(r2.establish(1000, codec.Some(uint64(2)), "user:alice")); err != nil {
			t.Fatal(err)
		}
		if fixp.EstablishmentRejectCode(rej.Code) != fixp.EstablishmentUnspecified || string(rej.Reason) != "NextSeqNo 2 below the next sequence number expected, 4: the peer lost messages it sent" {
			t.Fatalf("EstablishmentReject %+v %q", rej, rej.Reason)
		}
	})
}

// §7.4.1: the server's first message after an EstablishmentAck with
// NextSeqNo is numbered by it, without a Sequence. Without NextSeqNo, a
// Sequence must come first.
func TestExample_7_4_1_NumberedByTheAck(t *testing.T) {
	t.Run("NextSeqNo", func(t *testing.T) {
		_, h, l := clientOfRaw(t, fixp.Idempotent)
		r := acceptRaw(t, l)
		r.answerNegotiate(fixp.Recoverable)
		e := r.readEstablish()
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000, NextSeqNo: codec.Some(uint64(1))})
		h.expect(t, "established")
		r.send(appMsg{1, "first"})
		if e := h.expect(t, "message"); e.seq != 1 || e.body != "first" {
			t.Fatalf("message %v", e)
		}
	})
	t.Run("no NextSeqNo", func(t *testing.T) {
		_, h, l := clientOfRaw(t, fixp.Idempotent)
		r := acceptRaw(t, l)
		r.answerNegotiate(fixp.Recoverable)
		e := r.readEstablish()
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000})
		h.expect(t, "established")
		r.send(appMsg{1, "first"})
		r.terminated(fixp.TerminationUnspecifiedError, "application message before a Sequence")
	})
}

// §7.4.1.3, §7.4.3: heartbeats both ways, Sequence on a sequenced flow,
// UnsequencedHeartbeat on another, keep the session established.
func TestExample_7_4_1_3_and_7_4_3_Heartbeats(t *testing.T) {
	for _, tc := range []struct {
		example  string
		flow     fixp.FlowType
		template uint16
		send     encoder
	}{
		{"7.4.1.3", fixp.Recoverable, codec.SequenceTemplateID, &codec.Sequence{NextSeqNo: 1}},
		{"7.4.3", fixp.Unsequenced, codec.UnsequencedHeartbeatTemplateID, &codec.UnsequencedHeartbeat{}},
	} {
		t.Run(tc.example, func(t *testing.T) {
			// The raw client only answers the server's heartbeats, every
			// 50ms: with its own interval at 50ms too, the server would
			// cut it off after 75ms of silence, a runner's hiccup of 25ms.
			r, h, _, _ := rawClient(t, tc.flow, tc.flow, 1000)
			for range 10 {
				hd, msg := r.read()
				if hd.SchemaId != codec.SchemaID || hd.TemplateId != tc.template {
					t.Fatalf("template %d, want the heartbeat %d", hd.TemplateId, tc.template)
				}
				if tc.template == codec.SequenceTemplateID {
					var s codec.Sequence
					if s.Decode(msg); s.NextSeqNo != 1 {
						t.Fatalf("Sequence %d", s.NextSeqNo)
					}
				}
				r.send(tc.send)
			}
			h.quiet(t, 10*time.Millisecond)
		})
	}
}

// §7.4.4.2: a request answered by two batches under its timestamp, real-time
// messages between them, the client waiting for the second
// (PartialBatchWait, the server's keepalive interval by default); and the
// text's way (§4.6.2.2), a batch shorter than asked followed, once the
// wait passed, by a request for the rest.
func TestExample_7_4_4_2_PartialBatch(t *testing.T) {
	for _, example := range []bool{true, false} {
		t.Run(fmt.Sprintf("second batch=%v", example), func(t *testing.T) {
			_, h, l := clientOfRaw(t, fixp.Idempotent)
			r := acceptRaw(t, l)
			r.answerNegotiate(fixp.Recoverable)
			e := r.readEstablish()
			r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000, NextSeqNo: codec.Some(uint64(1))})
			h.expect(t, "established")
			r.send(appMsg{1, "1"})
			h.expect(t, "message")
			r.send(&codec.Sequence{NextSeqNo: 5})
			r.send(appMsg{1, "5"})
			rr := r.request(2, 3)
			r.batch(rr.Timestamp, 2, 1)
			h.messages(t, 2)
			if example {
				r.send(&codec.Sequence{NextSeqNo: 6})
				r.send(appMsg{1, "6"})
				r.batch(rr.Timestamp, 3, 2)
				h.messages(t, 3, 4, 5, 6)
				r.none(codec.RetransmitRequestTemplateID, 50*time.Millisecond)
				return
			}
			rr2 := r.request(3, 2)
			if rr2.Timestamp == rr.Timestamp {
				t.Fatal("the remainder asked under the first request's timestamp")
			}
			r.batch(rr2.Timestamp, 3, 2)
			h.messages(t, 3, 4, 5)
		})
	}
}

// §7.4.5.1, §7.4.5.2: a request reaching beyond what was sent, wholly or
// in part, is a protocol violation (§6.3): Terminate ReRequestOutOfBounds,
// nothing retransmitted. With RejectRetransmitRequestBeyondSent, the
// examples' RetransmitReject OutOfRange, the request refused whole, the
// session going on.
func TestExample_7_4_5_1_and_2_Beyond(t *testing.T) {
	for _, tc := range []struct {
		example     string
		from        uint64
		count       uint32
		reject      bool
		retransmits bool
	}{
		{"7.4.5.1", 6, 1, false, false},
		{"7.4.5.2", 4, 5, false, false},
		{"7.4.5.1 RejectRetransmitRequestBeyondSent", 6, 1, true, false},
		{"7.4.5.2 RejectRetransmitRequestBeyondSent", 4, 5, true, false},
		{"from 0", 0, 2, false, false},
		{"within", 4, 2, false, true},
	} {
		t.Run(tc.example, func(t *testing.T) {
			r, _, srv, _ := rawClientWith(t, fixp.Idempotent, fixp.Recoverable, 1000, func(c *ServerConfig) { c.RejectRetransmitRequestBeyondSent = tc.reject })
			s := srv.Session(r.id)
			for i := 1; i <= 5; i++ {
				s.Send(bg, appMsg{1, fmt.Sprint(i)})
			}
			r.readApps(5)
			ts := nanos(time.Now())
			r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: ts, FromSeqNo: tc.from, Count: tc.count})
			// Heartbeats at most before the answer: nothing retransmitted.
			switch {
			case tc.retransmits:
				var rt codec.Retransmission
				if _, err := rt.Decode(r.readTemplate(codec.RetransmissionTemplateID)); err != nil || rt.NextSeqNo != tc.from || rt.Count != tc.count {
					t.Fatalf("Retransmission %+v, %v", rt, err)
				}
			case tc.reject:
				var rj codec.RestransmitReject
				if _, err := rj.Decode(r.readTemplate(codec.RestransmitRejectTemplateID)); err != nil {
					t.Fatal(err)
				}
				if rj.RequestTimestamp != ts || fixp.RetransmitRejectCode(rj.Code) != fixp.RetransmitOutOfRange || string(rj.Reason) != "beyond what was sent" {
					t.Fatalf("RetransmitReject %+v %q", rj, rj.Reason)
				}
				s.Send(bg, appMsg{1, "6"})
				if got := r.readApps(1); fmt.Sprint(got) != "[seq=6 6]" {
					t.Fatalf("after the reject: %q", got)
				}
			default:
				var term codec.Terminate
				if _, err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil {
					t.Fatal(err)
				}
				if fixp.TerminationCode(term.Code) != fixp.TerminationReRequestOutOfBounds || string(term.Reason) != "RetransmitRequest beyond what was sent" {
					t.Fatalf("Terminate %v %q", fixp.TerminationCode(term.Code), term.Reason)
				}
				r.closed()
			}
		})
	}
}

// §7.5.2: FinishedSending, unanswered, is repeated as the heartbeat, its
// LastSeqNo unchanged; and, answered, until finalization is complete
// (§4.7.1), the peer's flow not finished.
func TestExample_7_5_2_FinishedSendingRepeated(t *testing.T) {
	r, h, srv, _ := rawClient(t, fixp.Idempotent, fixp.Recoverable, 1000)
	s := srv.Session(r.id)
	s.Send(bg, appMsg{1, "1"})
	r.readApps(1)
	if err := s.Finish(bg); err != nil {
		t.Fatal(err)
	}
	heartbeats := func() {
		t.Helper()
		for range 3 {
			hd, msg := r.read()
			var fs codec.FinishedSending
			if _, err := fs.Decode(msg); hd.TemplateId != codec.FinishedSendingTemplateID || err != nil || fs.LastSeqNo != codec.Some(uint64(1)) {
				t.Fatalf("template %d %+v, want FinishedSending 1", hd.TemplateId, fs)
			}
		}
	}
	heartbeats()
	r.send(&codec.FinishedReceiving{SessionId: r.id})
	// A barrier: the request's answer comes after FinishedReceiving was
	// read, and so do the heartbeats after it.
	ts := nanos(time.Now())
	r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: ts, FromSeqNo: 1, Count: 1})
	var rt codec.Retransmission
	if _, err := rt.Decode(r.until(codec.RetransmissionTemplateID)); err != nil || rt.RequestTimestamp != ts {
		t.Fatalf("Retransmission %+v, %v", rt, err)
	}
	if hd, _ := r.read(); hd.SchemaId == codec.SchemaID {
		t.Fatalf("template %d, not the message retransmitted", hd.TemplateId)
	}
	heartbeats()
	h.quiet(t, 10*time.Millisecond)
}

// §7.5.4: a Terminate after the server's FinishedReceiving, before both
// flows are finished: answered, the session not finalized; the next
// establishment completes it.
func TestExample_7_5_4_TerminateBeforeFinished(t *testing.T) {
	r, h, srv, l := rawClient(t, fixp.Idempotent, fixp.Recoverable, 1000)
	r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(0))})
	r.until(codec.FinishedReceivingTemplateID)
	r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCode(fixp.TerminationFinished)})
	r.terminated(fixp.TerminationFinished, "")
	r.closed()
	h.until(t, "unbound")
	s := srv.Session(r.id)
	if st := s.State(); st != StateUnbound {
		t.Fatalf("state %s", st)
	}

	r2 := dialRaw(t, l)
	r2.id = r.id
	var ack codec.EstablishmentAck
	if _, err := ack.Decode(r2.establish(1000, codec.Some(uint64(1)), "user:alice")); err != nil {
		t.Fatal(err)
	}
	r2.until(codec.FinishedSendingTemplateID)
	r2.send(&codec.FinishedReceiving{SessionId: r2.id})
	r2.until(codec.TerminateTemplateID)
	r2.send(&codec.Terminate{SessionId: r2.id, Code: codec.TerminationCode(fixp.TerminationFinished)})
	h.until(t, "finalized")
}

// §7.5.5: a Sequence after FinishedSending, before or after the server's
// FinishedReceiving, is a protocol violation.
func TestExample_7_5_5_SequenceAfterFinishedSending(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(fmt.Sprintf("answered=%v", answered), func(t *testing.T) {
			r, _, _, _ := rawClient(t, fixp.Idempotent, fixp.Recoverable, 1000)
			r.send(appMsg{1, "1"})
			r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(1))})
			if answered {
				r.until(codec.FinishedReceivingTemplateID)
			}
			r.send(&codec.Sequence{NextSeqNo: 2})
			r.terminated(fixp.TerminationUnspecifiedError, "Sequence after FinishedSending")
		})
	}
}

// §7.5.6, the half close: after the server's FinishedSending answered, the
// client's application messages are a protocol violation (§6.3), ignored;
// with AcceptMessagesAfterFinishedReceiving, the client's flow goes on as
// the example has it, the server asking for what it missed, and its
// retransmission delivered. After the client's FinishedSending, it still
// asks the server for what it missed.
func TestExample_7_5_6_HalfClose(t *testing.T) {
	t.Run("server finished", func(t *testing.T) {
		r, h, srv, _ := rawClient(t, fixp.Recoverable, fixp.Recoverable, 1000)
		if err := srv.Session(r.id).Finish(bg); err != nil {
			t.Fatal(err)
		}
		r.until(codec.FinishedSendingTemplateID)
		r.send(&codec.FinishedReceiving{SessionId: r.id})
		r.send(&codec.Sequence{NextSeqNo: 1})
		r.send(appMsg{1, "1"})
		r.terminated(fixp.TerminationUnspecifiedError, "application message after FinishedReceiving")
		// Ignored: not delivered.
		if e := h.next(t); e.kind != "unbound" || e.err == nil {
			t.Fatalf("event %v, want unbound on the violation", e)
		}
	})
	t.Run("server finished, AcceptMessagesAfterFinishedReceiving", func(t *testing.T) {
		r, h, srv, _ := rawClientWith(t, fixp.Recoverable, fixp.Recoverable, 1000, func(c *ServerConfig) {
			c.AcceptMessagesAfterFinishedReceiving = true
		})
		if err := srv.Session(r.id).Finish(bg); err != nil {
			t.Fatal(err)
		}
		r.until(codec.FinishedSendingTemplateID)
		r.send(&codec.FinishedReceiving{SessionId: r.id})
		// A session message ends the numbering (§3.5.4).
		r.send(&codec.Sequence{NextSeqNo: 1})
		r.send(appMsg{1, "1"})
		if e := h.expect(t, "message"); e.seq != 1 {
			t.Fatalf("message %v", e)
		}
		r.send(&codec.Sequence{NextSeqNo: 4})
		r.send(appMsg{1, "4"})
		rr := r.request(2, 2)
		r.batch(rr.Timestamp, 2, 2)
		h.messages(t, 2, 3, 4)
	})
	// Retransmitted, at our request, a message is not sent anew: delivered
	// by default too.
	t.Run("server finished, a retransmission", func(t *testing.T) {
		r, h, srv, _ := rawClient(t, fixp.Recoverable, fixp.Recoverable, 1000)
		r.send(appMsg{1, "1"})
		r.send(&codec.Sequence{NextSeqNo: 3})
		r.send(appMsg{1, "3"})
		rr := r.request(2, 1)
		if err := srv.Session(r.id).Finish(bg); err != nil {
			t.Fatal(err)
		}
		r.until(codec.FinishedSendingTemplateID)
		r.send(&codec.FinishedReceiving{SessionId: r.id})
		r.batch(rr.Timestamp, 2, 1)
		h.messages(t, 1, 2, 3)
	})
	t.Run("client finished", func(t *testing.T) {
		r, _, srv, _ := rawClient(t, fixp.Recoverable, fixp.Recoverable, 1000)
		s := srv.Session(r.id)
		for i := 1; i <= 3; i++ {
			s.Send(bg, appMsg{1, fmt.Sprint(i)})
		}
		r.readApps(3)
		r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(0))})
		r.until(codec.FinishedReceivingTemplateID)
		ts := nanos(time.Now())
		r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: ts, FromSeqNo: 1, Count: 3})
		var rt codec.Retransmission
		if _, err := rt.Decode(r.until(codec.RetransmissionTemplateID)); err != nil || rt.RequestTimestamp != ts || rt.NextSeqNo != 1 || rt.Count != 3 {
			t.Fatalf("Retransmission %+v, %v", rt, err)
		}
		got := r.readApps(3)
		if strings.Join(got, ",") != "1,2,3" {
			t.Fatalf("retransmitted %q", got)
		}
	})
}
