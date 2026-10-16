package session

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// skipUntil returns the next event of kind, skipping others.
func (r *recorder) skipUntil(t testing.TB, kind string) event {
	t.Helper()
	for {
		if e := r.next(t); e.kind == kind {
			return e
		}
	}
}

// The client finishes its flow; the server completes its own, then both
// are finished, terminated and finalized.
func TestFinalize(t *testing.T) {
	for _, flows := range [][2]fixp.FlowType{
		{fixp.Idempotent, fixp.Recoverable},
		{fixp.Recoverable, fixp.Recoverable},
		{fixp.Unsequenced, fixp.None},
		{fixp.None, fixp.Unsequenced},
	} {
		t.Run(fmt.Sprintf("%s/%s", flows[0], flows[1]), func(t *testing.T) {
			p := newPair(t, pairOptions{clientFlow: flows[0], serverFlow: flows[1]})
			cs, ss := p.client.Session(), p.serverSession(t)
			id := cs.ID()
			if flows[0] != fixp.None {
				cs.Send(bg, appMsg{1, "a"})
				p.srvH.skipUntil(t, "message")
			}
			if err := cs.Finish(bg); err != nil {
				t.Fatal(err)
			}
			if _, err := cs.Send(bg, appMsg{1, "late"}); !errors.Is(err, fixp.ErrNoFlow) {
				t.Errorf("Send after Finish: %v", err)
			}
			p.srvH.skipUntil(t, "peerfinished")
			p.clientH.skipUntil(t, "peerfinished")
			p.srvH.skipUntil(t, "finalized")
			p.clientH.skipUntil(t, "finalized")
			if err := <-p.cliDone; err != nil {
				t.Fatalf("client Run: %v", err)
			}
			p.cliDone <- nil
			if cs.State() != StateFinalized || ss.State() != StateFinalized {
				t.Errorf("client %s, server %s", cs.State(), ss.State())
			}
			if p.srv.Session(id) != nil {
				t.Error("finalized session still served")
			}
			for _, st := range []*memstore.Store{p.cliStore, p.srvStore} {
				ss, _ := st.Open(bg, id)
				if s := ss.State(); s.FinalizedAt.IsZero() || s.Retired || !s.OutboundFinish.Finished || !s.InboundFinish.Finished {
					t.Errorf("stored %+v", s)
				}
			}
		})
	}
}

// Both finish at once.
func TestFinalizeBothAtOnce(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Recoverable, serverFlow: fixp.Idempotent})
	cs, ss := p.client.Session(), p.serverSession(t)
	go cs.Finish(bg)
	go ss.Finish(bg)
	p.srvH.skipUntil(t, "finalized")
	p.clientH.skipUntil(t, "finalized")
	if err := <-p.cliDone; err != nil {
		t.Fatalf("client Run: %v", err)
	}
	p.cliDone <- nil
}

// The server's flow finishing first: the client answers, then finishes.
func TestFinalizeServerFirst(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Idempotent, serverFlow: fixp.Recoverable})
	ss := p.serverSession(t)
	ss.Send(bg, appMsg{2, "x"})
	p.clientH.skipUntil(t, "message")
	if err := ss.Finish(bg); err != nil {
		t.Fatal(err)
	}
	p.clientH.skipUntil(t, "peerfinished")
	p.srvH.skipUntil(t, "peerfinished")
	p.srvH.skipUntil(t, "finalized")
	p.clientH.skipUntil(t, "finalized")
}

// A Recoverable flow finishing with messages missing: they are asked for,
// then the finish is answered.
func TestFinishRecoverableCompletes(t *testing.T) {
	rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), nil)
	r := rs.r
	r.send(appMsg{1, "1"})
	r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(3))})
	var rr codec.RetransmitRequest
	if err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 2 || rr.Count != 2 {
		t.Fatalf("RetransmitRequest: %+v, %v", rr, err)
	}
	r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: 2, Count: 2})
	r.send(appMsg{1, "2"})
	r.send(appMsg{1, "3"})
	// The server finishes its own flow, then answers.
	r.readTemplate(codec.FinishedSendingTemplateID)
	r.readTemplate(codec.FinishedReceivingTemplateID)
	for _, want := range []string{"1", "2", "3"} {
		if e := rs.h.skipUntil(t, "message"); e.body != want {
			t.Fatalf("got %v", e)
		}
	}
	rs.h.skipUntil(t, "peerfinished")
	// Our acknowledgement, then the Terminate exchange.
	r.send(&codec.FinishedReceiving{SessionId: r.id})
	var term codec.Terminate
	if err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil || term.Code != codec.TerminationCodeFinished {
		t.Fatalf("Terminate: %+v, %v", term, err)
	}
	r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
	rs.h.skipUntil(t, "finalized")
}

// An Idempotent flow finishing with a gap: NotApplied, then the answer.
func TestFinishIdempotentGap(t *testing.T) {
	rs := newRawSession(t, fixp.Idempotent, fixp.Idempotent, codec.Some(uint64(1)), nil)
	r := rs.r
	r.send(appMsg{1, "1"})
	r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(4))})
	expectStrings(t, r.readApps(1), "seq=1", "notapplied=2+3")
	r.readTemplate(codec.FinishedSendingTemplateID)
	r.readTemplate(codec.FinishedReceivingTemplateID)
	if e := rs.h.skipUntil(t, "gap"); e.seq != 2 || e.body != "3" {
		t.Fatalf("gap %v", e)
	}
}

// A connection lost during finalization: the next resumes it.
func TestFinishResumedOnReconnect(t *testing.T) {
	l := mem.Listen("server")
	h := newRecorder()
	srv, cancel, done := newServer(t, l, memstore.New(), h, fixp.Idempotent, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	if !rawHandshake(r, fixp.Unsequenced, codec.Optional[uint64]{}) {
		t.Fatal("handshake")
	}
	h.expect(t, "established")
	r.send(&codec.FinishedSending{SessionId: r.id})
	r.readTemplate(codec.FinishedSendingTemplateID)
	r.readTemplate(codec.FinishedReceivingTemplateID)
	// Gone before acknowledging the server's flow.
	r.c.Close()
	h.skipUntil(t, "unbound")

	r2 := dialRaw(t, l)
	r2.id = r.id
	r2.establish(1000, codec.Optional[uint64]{}, "user:alice")
	// The server's FinishedSending again; ours, unanswered here, answered
	// again.
	var fs codec.FinishedSending
	if err := fs.Decode(r2.readTemplate(codec.FinishedSendingTemplateID)); err != nil || !fs.LastSeqNo.Valid || fs.LastSeqNo.Value != 0 {
		t.Fatalf("FinishedSending: %+v, %v", fs, err)
	}
	r2.send(&codec.FinishedSending{SessionId: r2.id})
	r2.readTemplate(codec.FinishedReceivingTemplateID)
	r2.send(&codec.FinishedReceiving{SessionId: r2.id})
	r2.readTemplate(codec.TerminateTemplateID)
	r2.send(&codec.Terminate{SessionId: r2.id, Code: codec.TerminationCodeFinished})
	h.skipUntil(t, "finalized")
	if srv.Session(r.id) != nil {
		t.Error("finalized session still served")
	}
}

func TestFinishViolations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clientFlow fixp.FlowType
		send       func(r *raw)
		reason     string
	}{
		{"no LastSeqNo on a sequenced flow", fixp.Idempotent, func(r *raw) {
			r.send(&codec.FinishedSending{SessionId: r.id})
		}, "FinishedSending of an Idempotent flow without LastSeqNo"},
		{"LastSeqNo below what was received", fixp.Idempotent, func(r *raw) {
			r.send(appMsg{1, "1"})
			r.send(appMsg{1, "2"})
			r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(1))})
		}, "FinishedSending's LastSeqNo 1 below the messages received"},
		{"application message after FinishedSending", fixp.Idempotent, func(r *raw) {
			r.send(appMsg{1, "1"})
			r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(1))})
			r.send(&codec.Sequence{NextSeqNo: 2})
			r.send(appMsg{1, "2"})
		}, "application message 2 after FinishedSending's LastSeqNo 1"},
		{"FinishedReceiving unasked", fixp.Idempotent, func(r *raw) {
			r.send(&codec.FinishedReceiving{SessionId: r.id})
		}, "FinishedReceiving of a flow not finishing"},
		{"another session's FinishedSending", fixp.Unsequenced, func(r *raw) {
			r.send(&codec.FinishedSending{SessionId: fixp.NewSessionID()})
		}, "FinishedSending of another session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRawSession(t, tc.clientFlow, fixp.Idempotent, codec.Some(uint64(1)), nil)
			tc.send(rs.r)
			var msg []byte
			for {
				h, m := rs.r.read()
				if h.SchemaId == codec.SchemaID && h.TemplateId == codec.TerminateTemplateID {
					msg = m
					break
				}
			}
			var term codec.Terminate
			if err := term.Decode(msg); err != nil || string(term.Reason) != tc.reason {
				t.Fatalf("Terminate: %+v %q, %v", term, term.Reason, err)
			}
		})
	}
}

// A handler refusing the peer's finish: the flow stays unfinished, the
// connection terminated.
func TestOnPeerFinishedRefused(t *testing.T) {
	refuse := &refuser{recorder: newRecorder()}
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), refuse, fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	rawHandshake(r, fixp.Unsequenced, codec.Optional[uint64]{})
	r.send(&codec.FinishedSending{SessionId: r.id})
	var term codec.Terminate
	if err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil || string(term.Reason) != "application error" {
		t.Fatalf("Terminate: %+v %q, %v", term, term.Reason, err)
	}
}

// Writes returning late: each side's final Terminate may be answered
// before its Write returns; both still finalize.
func TestFinalizeSlowWrites(t *testing.T) {
	l := mem.Listen("server")
	srvH, cliH := newRecorder(), newRecorder()
	_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Recoverable, nil)
	defer func() { cancelSrv(); <-srvDone }()
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		c, err := l.Dial(ctx)
		if err != nil {
			return nil, err
		}
		return slowWriteConn{c}, nil
	})
	c := newClient(t, dialer, memstore.New(), cliH, fixp.Recoverable, func(c *ClientConfig) {
		c.KeepaliveInterval = time.Second
	})
	cancel, done := runClient(c)
	defer cancel()
	cliH.expect(t, "established")
	c.Session().Finish(bg)
	srvH.skipUntil(t, "finalized")
	cliH.skipUntil(t, "finalized")
	if err := <-done; err != nil {
		t.Fatalf("client Run: %v", err)
	}
}

// A Send waiting for room fails once the flow finishes.
func TestSendWaitingFailsOnFinish(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced, client: func(c *ClientConfig) {
		c.MaxPendingMessages = 1
	}})
	cs := p.client.Session()
	p.cancelSrv()
	<-p.srvDone
	p.clientH.skipUntil(t, "unbound")
	cs.Send(bg, appMsg{1, "fills the queue"})
	result := make(chan error, 1)
	go func() {
		_, err := cs.Send(bg, appMsg{1, "waits"})
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := cs.Finish(bg); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, fixp.ErrNoFlow) {
			t.Fatalf("waiting Send: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting Send still waiting")
	}
	p.srvDone = make(chan error, 1)
	p.srvDone <- nil
}

// A peer repeating FinishedSending without reading gets one pending
// answer, not one each.
func TestFinishedSendingRepeatedCoalesced(t *testing.T) {
	rs := newRawSession(t, fixp.Unsequenced, fixp.Unsequenced, codec.Optional[uint64]{}, nil)
	for range 200 {
		rs.r.send(&codec.FinishedSending{SessionId: rs.r.id})
	}
	time.Sleep(100 * time.Millisecond)
	answers := 0
	rs.r.c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		enc, msg, err := rs.r.r.Next()
		if err != nil {
			break
		}
		if _, h, _, _ := codec.Classify(enc, msg); h.TemplateId == codec.FinishedReceivingTemplateID {
			answers++
		}
	}
	if answers == 0 || answers >= 200 {
		t.Fatalf("%d FinishedReceiving for 200 FinishedSending", answers)
	}
}

// A malformed Terminate is no answer: the session is not finalized on it.
func TestMalformedTerminateNotFinal(t *testing.T) {
	rs := newRawSession(t, fixp.Unsequenced, fixp.Unsequenced, codec.Optional[uint64]{}, nil)
	r := rs.r
	r.send(&codec.FinishedSending{SessionId: r.id})
	r.readTemplate(codec.FinishedSendingTemplateID)
	r.readTemplate(codec.FinishedReceivingTemplateID)
	r.send(&codec.FinishedReceiving{SessionId: r.id})
	r.readTemplate(codec.TerminateTemplateID)
	// A Terminate of another session.
	r.send(&codec.Terminate{SessionId: fixp.NewSessionID(), Code: codec.TerminationCodeFinished})
	rs.h.skipUntil(t, "unbound")
	rs.h.quiet(t, 100*time.Millisecond)
	if s := rs.srv.Session(r.id); s == nil || s.State() == StateFinalized {
		t.Fatal("finalized on a Terminate of another session")
	}
}

// Nothing of the peer's beyond its FinishedSending's LastSeqNo.
func TestBeyondLastSeqNo(t *testing.T) {
	rs := newRawSession(t, fixp.Idempotent, fixp.Unsequenced, codec.Some(uint64(1)), nil)
	rs.r.send(appMsg{1, "1"})
	rs.r.send(&codec.FinishedSending{SessionId: rs.r.id, LastSeqNo: codec.Some(uint64(2))})
	rs.r.send(&codec.Sequence{NextSeqNo: 5})
	var msg []byte
	for {
		h, m := rs.r.read()
		if h.TemplateId == codec.TerminateTemplateID {
			msg = m
			break
		}
	}
	var term codec.Terminate
	if err := term.Decode(msg); err != nil || string(term.Reason) != "Sequence 5 beyond FinishedSending's LastSeqNo 2" {
		t.Fatalf("Terminate: %q, %v", term.Reason, err)
	}
}

// failTerminateListener accepts connections whose Write of a Terminate
// waits, then fails, writing nothing.
type failTerminateListener struct{ *mem.Listener }

func (l failTerminateListener) Accept(ctx context.Context) (transport.Conn, error) {
	c, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return failTerminateConn{c}, nil
}

type failTerminateConn struct{ transport.Conn }

func (c failTerminateConn) Write(b []byte) (int, error) {
	if len(b) >= 14 && binary.LittleEndian.Uint16(b[10:]) == codec.SchemaID && binary.LittleEndian.Uint16(b[8:]) == codec.TerminateTemplateID {
		time.Sleep(100 * time.Millisecond)
		return 0, errors.New("write failed")
	}
	return c.Conn.Write(b)
}

// Our final Terminate failing to go out, the peer's read meanwhile: the
// exchange is not complete, the session not finalized.
func TestFinalTerminateWriteFails(t *testing.T) {
	l := mem.Listen("server")
	h := newRecorder()
	srv, cancel, done := newServer(t, failTerminateListener{l}, memstore.New(), h, fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	rawHandshake(r, fixp.Unsequenced, codec.Optional[uint64]{})
	h.skipUntil(t, "established")
	r.send(&codec.FinishedSending{SessionId: r.id})
	r.readTemplate(codec.FinishedSendingTemplateID)
	r.readTemplate(codec.FinishedReceivingTemplateID)
	// Both finished: the server's Terminate is being written, and fails;
	// ours comes meanwhile.
	r.send(&codec.FinishedReceiving{SessionId: r.id})
	time.Sleep(20 * time.Millisecond)
	r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
	h.skipUntil(t, "unbound")
	h.quiet(t, 100*time.Millisecond)
	if s := srv.Session(r.id); s == nil || s.State() == StateFinalized || s.State() == StateFinalizing {
		t.Fatal("finalized though our Terminate never went out")
	}
}

type refuser struct{ *recorder }

func (r *refuser) OnPeerFinished(context.Context, *Session) error { return errors.New("not yet") }

var _ = time.Second
