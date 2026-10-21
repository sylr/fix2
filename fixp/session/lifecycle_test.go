package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// The S5 rows of docs/conformance.md: several Retransmission batches per
// RetransmitRequest, the receive-only phase after our Terminate, and
// finalization with recovery pending.

// request reads the next RetransmitRequest, skipping what comes before,
// and checks what it asks for.
func (r *raw) request(from uint64, count uint32) codec.RetransmitRequest {
	r.t.Helper()
	var rr codec.RetransmitRequest
	if _, err := rr.Decode(r.until(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != from || rr.Count != count || rr.SessionId != r.id {
		r.t.Fatalf("RetransmitRequest %d from %d, want %d from %d: %v", rr.Count, rr.FromSeqNo, count, from, err)
	}
	return rr
}

// batch sends a Retransmission under ts, then n messages from from, their
// bodies their numbers.
func (r *raw) batch(ts, from uint64, n int) {
	r.t.Helper()
	r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: ts, NextSeqNo: from, Count: uint32(n)})
	for seq := from; seq < from+uint64(n); seq++ {
		r.send(appMsg{1, fmt.Sprint(seq)})
	}
}

// none reads for d, failing on a session message of template.
func (r *raw) none(template uint16, d time.Duration) {
	r.t.Helper()
	r.c.SetReadDeadline(time.Now().Add(d))
	for {
		_, msg, err := r.r.Next()
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			return // the reader resumes after a timeout
		case err != nil:
			r.t.Fatalf("reading: %v", err)
		}
		if h, _, _ := codec.ReadHeader(msg); h.SchemaId == codec.SchemaID && h.TemplateId == template {
			r.t.Fatalf("template %d within %v", template, d)
		}
	}
}

// messages checks the next messages delivered are seqs, in order, each
// its number as body, other events skipped.
func (r *recorder) messages(t testing.TB, seqs ...uint64) {
	t.Helper()
	for _, seq := range seqs {
		if e := r.until(t, "message"); e.seq != seq || e.body != fmt.Sprint(seq) {
			t.Fatalf("message %v, want %d", e, seq)
		}
	}
}

// seqs returns from to to.
func seqs(from, to uint64) []uint64 {
	var s []uint64
	for seq := from; seq <= to; seq++ {
		s = append(s, seq)
	}
	return s
}

// A RetransmitRequest "should be answered by one or more Retransmission
// messages" (§4.6.1), the requester asking "another RetransmitRequest for
// the remainder" of a partial batch (§4.6.2.2): further batches under the
// request's timestamp are taken while it waits, PartialBatchWait, after
// which it asks for the remainder, taking a late batch of the first
// request still.
func TestRetransmissionBatches(t *testing.T) {
	start := func(t *testing.T, wait time.Duration) (*raw, *recorder) {
		t.Helper()
		rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), func(c *ServerConfig) {
			c.PartialBatchWait = wait
		})
		rs.r.send(appMsg{1, "1"})
		rs.r.send(&codec.Sequence{NextSeqNo: 8})
		rs.r.send(appMsg{1, "8"})
		return rs.r, rs.h
	}

	// §7.4.4.2: batches under one request, live messages between them.
	t.Run("several batches, live messages between", func(t *testing.T) {
		r, h := start(t, time.Minute)
		rr := r.request(2, 6)
		r.batch(rr.Timestamp, 2, 2)
		r.batch(rr.Timestamp, 4, 2)
		r.send(&codec.Sequence{NextSeqNo: 9})
		r.send(appMsg{1, "9"})
		r.batch(rr.Timestamp, 6, 2)
		h.messages(t, seqs(1, 9)...)
		// Answered whole, the request is over: the next gap is asked for at
		// once, not a minute later.
		r.send(&codec.Sequence{NextSeqNo: 11})
		r.send(appMsg{1, "11"})
		r.request(10, 1)
	})

	// §4.6.2.2: a partial batch, then nothing: once the wait passed, with
	// no traffic to wake the reader, the remainder is asked for.
	t.Run("partial batch, then the remainder asked for", func(t *testing.T) {
		r, h := start(t, 50*time.Millisecond)
		rr := r.request(2, 6)
		r.batch(rr.Timestamp, 2, 2)
		sent := time.Now()
		rr2 := r.request(4, 4)
		if waited := time.Since(sent); waited < 50*time.Millisecond || rr2.Timestamp == rr.Timestamp {
			t.Fatalf("asked again after %v, timestamp %d, %d before", waited, rr2.Timestamp, rr.Timestamp)
		}
		r.batch(rr2.Timestamp, 4, 4)
		h.messages(t, seqs(1, 8)...)
	})

	// The default wait is the peer's keepalive interval, 1s here.
	t.Run("default wait", func(t *testing.T) {
		r, h := start(t, 0)
		rr := r.request(2, 6)
		r.batch(rr.Timestamp, 2, 2)
		sent := time.Now()
		rr2 := r.request(4, 4)
		if waited := time.Since(sent); waited < time.Second {
			t.Fatalf("asked again after %v", waited)
		}
		r.batch(rr2.Timestamp, 4, 4)
		h.messages(t, seqs(1, 8)...)
	})

	// A negative wait asks at once.
	t.Run("no wait", func(t *testing.T) {
		r, h := start(t, -1)
		rr := r.request(2, 6)
		r.batch(rr.Timestamp, 2, 2)
		rr2 := r.request(4, 4)
		r.batch(rr2.Timestamp, 4, 4)
		h.messages(t, seqs(1, 8)...)
	})

	// The first request's batch coming after the remainder was asked for
	// is told from the second's by its timestamp, and taken; the second's
	// answer repeats messages, dropped; a RetransmitReject of the first is
	// left to the second's answer.
	t.Run("late batch of the first request", func(t *testing.T) {
		r, h := start(t, 50*time.Millisecond)
		rr := r.request(2, 6)
		r.batch(rr.Timestamp, 2, 2)
		rr2 := r.request(4, 4)
		r.batch(rr.Timestamp, 4, 2)
		r.send(&codec.RestransmitReject{SessionId: r.id, RequestTimestamp: rr.Timestamp, Code: codec.RetransmitRejectCodeOutOfRange})
		r.batch(rr2.Timestamp, 4, 4)
		h.messages(t, seqs(1, 8)...)
		h.quiet(t, 50*time.Millisecond)
	})

	// The second request rejected OutOfRange after a late batch of the
	// first delivered what it asks for, all of it or some: only what is
	// still unresolved is lost, the frontier never moving back.
	t.Run("rejection after a late batch, all delivered", func(t *testing.T) {
		r, h := start(t, 50*time.Millisecond)
		rr := r.request(2, 6)
		r.batch(rr.Timestamp, 2, 2)
		rr2 := r.request(4, 4)
		r.batch(rr.Timestamp, 4, 4)
		h.messages(t, seqs(1, 8)...)
		// The recorder refuses any loss: OnLost would terminate.
		r.send(&codec.RestransmitReject{SessionId: r.id, RequestTimestamp: rr2.Timestamp, Code: codec.RetransmitRejectCodeOutOfRange})
		r.send(&codec.Sequence{NextSeqNo: 9})
		r.send(appMsg{1, "9"})
		h.messages(t, 9)
	})
	t.Run("rejection after a late batch, some delivered", func(t *testing.T) {
		rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), func(c *ServerConfig) {
			c.PartialBatchWait = 50 * time.Millisecond
		})
		r, h := rs.r, rs.h
		lost := make(chan [2]uint64, 1)
		h.onLost = func(from, count uint64) error { lost <- [2]uint64{from, count}; return nil }
		r.send(appMsg{1, "1"})
		r.send(&codec.Sequence{NextSeqNo: 8})
		r.send(appMsg{1, "8"})
		rr := r.request(2, 6)
		r.batch(rr.Timestamp, 2, 2)
		rr2 := r.request(4, 4)
		r.batch(rr.Timestamp, 4, 2)
		h.messages(t, seqs(1, 5)...)
		r.send(&codec.RestransmitReject{SessionId: r.id, RequestTimestamp: rr2.Timestamp, Code: codec.RetransmitRejectCodeOutOfRange})
		h.messages(t, 8)
		if l := <-lost; l != [2]uint64{6, 2} {
			t.Fatalf("lost %v, want 6 to 7", l)
		}
		eventually(t, func() bool { st, _ := rs.st.Open(bg, r.id); return st.State().Inbound == 8 })
	})

	// Requests answered in part, one after another: a late batch of the
	// first is still taken after the third is asked for.
	t.Run("three requests", func(t *testing.T) {
		r, h := start(t, 50*time.Millisecond)
		a := r.request(2, 6)
		r.batch(a.Timestamp, 2, 2)
		b := r.request(4, 4)
		r.batch(b.Timestamp, 4, 1)
		c := r.request(5, 3)
		r.batch(a.Timestamp, 4, 2)
		r.batch(c.Timestamp, 5, 3)
		h.messages(t, seqs(1, 8)...)
		h.quiet(t, 50*time.Millisecond)
	})

	// The wait for the second request lapsing while a late batch of the
	// first is read: that batch keeps its request, which stays answerable;
	// the remainder is asked for once the batch is over.
	t.Run("wait lapsing within a late batch", func(t *testing.T) {
		r, h := start(t, 50*time.Millisecond)
		a := r.request(2, 6)
		r.batch(a.Timestamp, 2, 2)
		b := r.request(4, 4)
		r.batch(b.Timestamp, 4, 1)
		r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: a.Timestamp, NextSeqNo: 4, Count: 2})
		r.send(appMsg{1, "4"})
		time.Sleep(150 * time.Millisecond)
		r.send(appMsg{1, "5"})
		c := r.request(6, 2)
		r.batch(a.Timestamp, 6, 2)
		r.batch(c.Timestamp, 6, 2)
		h.messages(t, seqs(1, 8)...)
		h.quiet(t, 50*time.Millisecond)
	})

	for _, tc := range []struct {
		name   string
		send   func(r *raw, first, second uint64)
		reason string
	}{
		{"a batch of the first request out of place", func(r *raw, first, _ uint64) {
			r.batch(first, 5, 1)
		}, "Retransmission of 1 from 5, 4 from 4 asked"},
		{"a batch of no request", func(r *raw, first, second uint64) {
			r.batch(min(first, second)-1, 4, 1)
		}, "Retransmission answering no request of ours"},
		{"a batch beyond its request", func(r *raw, _, second uint64) {
			r.batch(second, 4, 5)
		}, "Retransmission of 5 from 4, 4 from 4 asked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := start(t, 50*time.Millisecond)
			rr := r.request(2, 6)
			r.batch(rr.Timestamp, 2, 2)
			rr2 := r.request(4, 4)
			r.mayEnd = true // terminated on the Retransmission, its messages unread
			tc.send(r, rr.Timestamp, rr2.Timestamp)
			r.terminated(fixp.TerminationUnspecifiedError, tc.reason)
		})
	}
}

// terminatingOnFinish terminates the session as the peer's flow finishes.
type terminatingOnFinish struct{ *recorder }

func (h *terminatingOnFinish) OnPeerFinished(ctx context.Context, s *Session) error {
	s.Terminate(ctx, "bye")
	return h.recorder.OnPeerFinished(ctx, s)
}

// receiveOnly returns a raw client of a server whose session sent its
// Terminate, its answer not sent: the server's keepalive interval long, the
// connection waits for it.
func receiveOnly(t *testing.T, clientFlow, serverFlow fixp.FlowType) (*rawSession, *Session) {
	t.Helper()
	var next codec.Optional[uint64]
	if clientFlow.Sequenced() {
		next = codec.Some(uint64(1))
	}
	rs := newRawSession(t, clientFlow, serverFlow, next, func(c *ServerConfig) { c.KeepaliveInterval = 5 * time.Second })
	s := rs.session(t)
	if serverFlow == fixp.Recoverable {
		s.Send(bg, appMsg{2, "1"})
	}
	s.Terminate(bg, "bye")
	rs.r.until(codec.TerminateTemplateID)
	return rs, s
}

// After our Terminate, the peer's in-flight messages are handled (§4.4.1),
// and nothing is written (§6.3): no request, no answer, no heartbeat, no
// second Terminate, whatever the peer sends, or if it stays silent.
func TestReceiveOnlyAfterTerminate(t *testing.T) {
	t.Run("messages delivered", func(t *testing.T) {
		rs, _ := receiveOnly(t, fixp.Recoverable, fixp.Unsequenced)
		rs.r.send(appMsg{1, "1"})
		rs.r.send(appMsg{1, "2"})
		rs.h.messages(t, 1, 2)
		rs.r.send(&codec.Terminate{SessionId: rs.r.id, Code: codec.TerminationCodeFinished})
		rs.r.closedSilently()
		var te *fixp.TerminatedError
		if e := rs.h.until(t, "unbound"); !errors.As(e.err, &te) || te.ByPeer || te.Reason != "bye" || te.Err != nil {
			t.Fatalf("unbound: %v", e.err)
		}
		eventually(t, func() bool { st, _ := rs.st.Open(bg, rs.r.id); return st.State().Inbound == 2 })
	})

	t.Run("a gap: nothing asked", func(t *testing.T) {
		rs, _ := receiveOnly(t, fixp.Recoverable, fixp.Unsequenced)
		rs.r.send(appMsg{1, "1"})
		rs.r.send(&codec.Sequence{NextSeqNo: 4})
		rs.r.send(appMsg{1, "4"})
		rs.h.messages(t, 1)
		rs.r.none(codec.RetransmitRequestTemplateID, 50*time.Millisecond)
		rs.r.send(&codec.Terminate{SessionId: rs.r.id, Code: codec.TerminationCodeFinished})
		rs.r.closedSilently()
		if e := rs.h.until(t, "unbound"); e.err == nil {
			t.Fatal("unbound without error")
		}
	})

	// An Idempotent flow's gap passes, the handler told; NotApplied, an
	// application message of ours, waits for the next connection.
	t.Run("an Idempotent gap: NotApplied queued", func(t *testing.T) {
		rs, s := receiveOnly(t, fixp.Idempotent, fixp.Idempotent)
		rs.r.send(&codec.Sequence{NextSeqNo: 3})
		rs.r.send(appMsg{1, "3"})
		if e := rs.h.until(t, "gap"); e.seq != 1 || e.body != "2" {
			t.Fatalf("gap %v", e)
		}
		rs.h.messages(t, 3)
		rs.r.send(&codec.Terminate{SessionId: rs.r.id, Code: codec.TerminationCodeFinished})
		rs.r.closedSilently()
		rs.h.until(t, "unbound")
		if n := s.NextSeqNo(); n != 2 {
			t.Fatalf("next sequence number %d, NotApplied not assigned", n)
		}
	})

	// The peer's FinishedSending crossing our Terminate is recorded, not
	// answered: FinishedReceiving cannot follow our Terminate, so its flow is
	// not finished, nor the session finalized on the Terminate exchange,
	// though ours was; the next establishment completes it.
	t.Run("FinishedSending unanswered, not finalized", func(t *testing.T) {
		r, h, srv, l := rawClientWith(t, fixp.Unsequenced, fixp.Unsequenced, 1000, func(c *ServerConfig) {
			c.KeepaliveInterval = 5 * time.Second
		})
		s := srv.Session(r.id)
		if err := s.Finish(bg); err != nil {
			t.Fatal(err)
		}
		r.until(codec.FinishedSendingTemplateID)
		r.send(&codec.FinishedReceiving{SessionId: r.id})
		eventually(t, func() bool { out, _ := s.finishes(); return out.Finished })
		s.Terminate(bg, "bye")
		r.until(codec.TerminateTemplateID)
		r.send(&codec.FinishedSending{SessionId: r.id})
		r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
		r.closedSilently()
		h.until(t, "unbound")
		h.quiet(t, 50*time.Millisecond)
		if st := s.State(); st != StateUnbound {
			t.Fatalf("state %s", st)
		}
		if _, in := s.finishes(); !in.Finishing || in.Finished {
			t.Fatalf("peer's flow %+v", in)
		}
		// Established again, the peer's FinishedSending repeated is
		// answered, and the session finalized.
		r2 := dialRaw(t, l)
		r2.id = r.id
		var ack codec.EstablishmentAck
		if _, err := ack.Decode(r2.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil {
			t.Fatal(err)
		}
		r2.send(&codec.FinishedSending{SessionId: r2.id})
		r2.until(codec.FinishedReceivingTemplateID)
		r2.terminated(fixp.TerminationFinished, "finished")
		r2.send(&codec.Terminate{SessionId: r2.id, Code: codec.TerminationCodeFinished})
		h.until(t, "finalized")
	})

	// The same, our Terminate queued and not yet written as the peer's
	// FinishedSending is handled: the FinishedReceiving answering it would
	// follow the Terminate, never written; it is not queued, the flow not
	// recorded finished.
	t.Run("FinishedSending as our Terminate is queued", func(t *testing.T) {
		gate := make(chan struct{})
		g := &gatedStore{Store: memstore.New(), before: []chan struct{}{gate}}
		r, h, srv, _ := rawClientWith(t, fixp.Unsequenced, fixp.Recoverable, 1000, func(c *ServerConfig) {
			c.Store = g
			c.KeepaliveInterval = 5 * time.Second
		})
		s := srv.Session(r.id)
		s.Send(bg, appMsg{2, "1"})
		r.readApps(1)
		if err := s.Finish(bg); err != nil {
			t.Fatal(err)
		}
		r.until(codec.FinishedSendingTemplateID)
		r.send(&codec.FinishedReceiving{SessionId: r.id})
		eventually(t, func() bool { out, _ := s.finishes(); return out.Finished })
		// The writer held by a retransmission, the Terminate queued behind.
		r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: 1, FromSeqNo: 1, Count: 1})
		eventually(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.calls == 1 })
		s.Terminate(bg, "bye")
		r.send(&codec.FinishedSending{SessionId: r.id})
		eventually(t, func() bool { _, in := s.finishes(); return in.Finishing })
		time.Sleep(20 * time.Millisecond) // its answer queued, or not
		close(gate)
		r.until(codec.RetransmissionTemplateID)
		r.readApps(1)
		// Our Terminate, no FinishedReceiving before it.
		if hd, _ := r.read(); hd.SchemaId != codec.SchemaID || hd.TemplateId != codec.TerminateTemplateID {
			t.Fatalf("template %d, want our Terminate", hd.TemplateId)
		}
		r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
		r.closedSilently()
		h.until(t, "unbound")
		h.quiet(t, 50*time.Millisecond)
		if _, in := s.finishes(); !in.Finishing || in.Finished || s.State() != StateUnbound {
			t.Fatalf("peer's flow %+v, state %s", in, s.State())
		}
	})

	// The peer's counterpart: its FinishedSending answered, the answer queued
	// behind a retransmission, then its Terminate, whose urgent answer drops
	// the queue: our FinishedReceiving goes just before it, and the session
	// is finalized, the peer having heard it.
	t.Run("the peer's Terminate as our FinishedReceiving is queued", func(t *testing.T) {
		gate := make(chan struct{})
		g := &gatedStore{Store: memstore.New(), before: []chan struct{}{gate}}
		r, h, srv, _ := rawClientWith(t, fixp.Unsequenced, fixp.Recoverable, 1000, func(c *ServerConfig) {
			c.Store = g
			c.KeepaliveInterval = 5 * time.Second
		})
		s := srv.Session(r.id)
		s.Send(bg, appMsg{2, "1"})
		r.readApps(1)
		if err := s.Finish(bg); err != nil {
			t.Fatal(err)
		}
		r.until(codec.FinishedSendingTemplateID)
		r.send(&codec.FinishedReceiving{SessionId: r.id})
		eventually(t, func() bool { out, _ := s.finishes(); return out.Finished })
		r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: 1, FromSeqNo: 1, Count: 1})
		eventually(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.calls == 1 })
		r.send(&codec.FinishedSending{SessionId: r.id})
		eventually(t, func() bool { _, in := s.finishes(); return in.Finished })
		r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
		time.Sleep(20 * time.Millisecond) // the urgent answer installed
		close(gate)
		r.until(codec.FinishedReceivingTemplateID)
		r.terminated(fixp.TerminationFinished, "")
		r.closedSilently()
		h.until(t, "finalized")
	})

	// The same, our flow not finished, an application message queued behind
	// the blocked writer, then our FinishedSending, then the answer: the
	// answer would overtake them, the message then sent after it on the
	// next connection (§6.3). The Terminate goes alone, the session not
	// finalized; established again, the message, our FinishedSending and,
	// the peer's repeated, the answer go in order.
	t.Run("the peer's Terminate as our messages are queued", func(t *testing.T) {
		gate := make(chan struct{})
		g := &gatedStore{Store: memstore.New(), before: []chan struct{}{gate}}
		r, h, srv, l := rawClientWith(t, fixp.Unsequenced, fixp.Recoverable, 1000, func(c *ServerConfig) {
			c.Store = g
			c.KeepaliveInterval = 5 * time.Second
		})
		s := srv.Session(r.id)
		s.Send(bg, appMsg{2, "1"})
		r.readApps(1)
		r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: 1, FromSeqNo: 1, Count: 1})
		eventually(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.calls == 1 })
		if _, err := s.Send(bg, appMsg{2, "2"}); err != nil {
			t.Fatal(err)
		}
		r.send(&codec.FinishedSending{SessionId: r.id})
		eventually(t, func() bool { _, in := s.finishes(); return in.Finished })
		r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
		time.Sleep(20 * time.Millisecond) // the urgent answer installed
		close(gate)
		// The Terminate alone.
		if hd, _ := r.read(); hd.SchemaId != codec.SchemaID || hd.TemplateId != codec.TerminateTemplateID {
			t.Fatalf("template %d, want the Terminate alone", hd.TemplateId)
		}
		r.closedSilently()
		h.until(t, "unbound")
		h.quiet(t, 50*time.Millisecond)
		if st := s.State(); st != StateUnbound {
			t.Fatalf("state %s", st)
		}

		r2 := dialRaw(t, l)
		r2.id = r.id
		r2.establish(1000, codec.Optional[uint64]{}, "user:alice")
		var got []string
		for {
			hd, msg := r2.read()
			if hd.SchemaId != codec.SchemaID {
				got = append(got, string(msg[8:]))
				continue
			}
			if hd.TemplateId == codec.FinishedSendingTemplateID {
				break
			}
		}
		if fmt.Sprint(got) != "[2]" {
			t.Fatalf("before FinishedSending %q, want message 2", got)
		}
		r2.send(&codec.FinishedSending{SessionId: r2.id})
		r2.until(codec.FinishedReceivingTemplateID)
		r2.send(&codec.FinishedReceiving{SessionId: r2.id})
		r2.terminated(fixp.TerminationFinished, "finished")
		r2.send(&codec.Terminate{SessionId: r2.id, Code: codec.TerminationCodeFinished})
		h.until(t, "finalized")
	})

	// Terminate called while the peer's FinishedSending is answered, from
	// OnPeerFinished: it waits for the FinishedReceiving, which goes first.
	t.Run("Terminate as FinishedSending is answered", func(t *testing.T) {
		h := &terminatingOnFinish{newRecorder()}
		l := mem.Listen("server")
		srv, cancel, done := newServer(t, l, memstore.New(), h, fixp.Unsequenced, func(c *ServerConfig) {
			c.KeepaliveInterval = 5 * time.Second
		})
		t.Cleanup(func() { cancel(); <-done })
		r := dialRaw(t, l)
		r.negotiate(fixp.Unsequenced, "user:alice")
		r.establish(1000, codec.Optional[uint64]{}, "user:alice")
		h.until(t, "established")
		s := srv.Session(r.id)
		if err := s.Finish(bg); err != nil {
			t.Fatal(err)
		}
		r.until(codec.FinishedSendingTemplateID)
		r.send(&codec.FinishedReceiving{SessionId: r.id})
		r.send(&codec.FinishedSending{SessionId: r.id})
		r.until(codec.FinishedReceivingTemplateID)
		r.terminated(fixp.TerminationFinished, "bye")
		r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
		h.until(t, "finalized")
	})

	t.Run("the peer's request unanswered", func(t *testing.T) {
		rs, _ := receiveOnly(t, fixp.Unsequenced, fixp.Recoverable)
		rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 1, FromSeqNo: 1, Count: 1})
		rs.r.none(codec.RetransmissionTemplateID, 50*time.Millisecond)
		rs.r.send(&codec.Terminate{SessionId: rs.r.id, Code: codec.TerminationCodeFinished})
		rs.r.closedSilently()
	})

	// An error ends the connection at once, without a frame; silence too,
	// once the peer's keepalive interval lapsed.
	for _, tc := range []struct {
		name string
		send func(r *raw)
	}{
		{"a protocol violation", func(r *raw) {
			r.send(appMsg{1, "1"})
			r.send(&codec.Sequence{NextSeqNo: 1})
		}},
		{"a malformed frame", func(r *raw) {
			r.sendBytes([]byte{0, 0, 0, 2, 0xeb, 0x50})
		}},
		{"a malformed message", func(r *raw) {
			f, _ := sofh.AppendFrame(nil, sofh.SBE10LittleEndian, []byte{0, 0, 200, 0, 0xbc, 0x0a, 0, 0})
			r.sendBytes(f)
		}},
		{"a Terminate of another session", func(r *raw) {
			r.send(&codec.Terminate{SessionId: fixp.NewSessionID(), Code: codec.TerminationCodeFinished})
		}},
		{"silence", func(*raw) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, _ := receiveOnly(t, fixp.Recoverable, fixp.Unsequenced)
			tc.send(rs.r)
			rs.r.closedSilently()
			var te *fixp.TerminatedError
			if e := rs.h.until(t, "unbound"); !errors.As(e.err, &te) || te.ByPeer || te.Reason != "bye" || te.Err == nil {
				t.Fatalf("unbound: %v", e.err)
			}
		})
	}
}

// timingOutConn times every Read out at once once timeouts is set,
// counting them: a transport whose timeout is sticky, or spurious.
type timingOutConn struct {
	transport.Conn
	timeouts *atomic.Bool
	reads    *atomic.Int64
}

func (c timingOutConn) Read(b []byte) (int, error) {
	if c.timeouts.Load() {
		c.reads.Add(1)
		return 0, os.ErrDeadlineExceeded
	}
	return c.Conn.Read(b)
}

// A timeout before the deadline the reader set, or with none set, is the
// transport's: the connection ends at once, a read error, without reading
// again, keepalives watched or not.
func TestUnscheduledTimeout(t *testing.T) {
	for _, tolerate := range []bool{false, true} {
		t.Run(fmt.Sprintf("ToleratePeerWithoutKeepalives=%v", tolerate), func(t *testing.T) {
			l := mem.Listen("server")
			srvH := newRecorder()
			_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Unsequenced, nil)
			t.Cleanup(func() { cancelSrv(); <-srvDone })
			timeouts, reads := &atomic.Bool{}, &atomic.Int64{}
			dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
				c, err := l.Dial(ctx)
				if err != nil {
					return nil, err
				}
				return timingOutConn{c, timeouts, reads}, nil
			})
			cliH := newRecorder()
			c := newClient(t, dialer, memstore.New(), cliH, fixp.Unsequenced, func(c *ClientConfig) {
				c.KeepaliveInterval = time.Second
				c.ToleratePeerWithoutKeepalives = tolerate
				// Watched, the server's keepalives never lapse here: a
				// Sequence late on a loaded machine, past 1.5 times its
				// 50ms, would end the connection before the timeout.
				c.KeepaliveLeniency = 20
				c.ReconnectMin, c.ReconnectMax = time.Minute, time.Minute
			})
			cancel, done := runClient(c)
			t.Cleanup(func() { cancel(); <-done })
			cliH.expect(t, "established")
			timeouts.Store(true)
			e := cliH.until(t, "unbound")
			if errors.Is(e.err, fixp.ErrKeepaliveLapsed) || !errors.Is(e.err, os.ErrDeadlineExceeded) {
				t.Fatalf("unbound: %v", e.err)
			}
			if n := reads.Load(); n != 1 {
				t.Fatalf("%d reads timed out, want 1", n)
			}
		})
	}
}

// Finalization waits for both flows (docs/fixp.md): the peer's
// FinishedReceiving coming while its own flow is being recovered, before
// a batch, within one, or between two, terminates nothing until every
// message up to its LastSeqNo is delivered, then answered.
func TestFinishedReceivingDuringRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(r *raw, ts uint64, finishedReceiving func())
	}{
		{"before the batch", func(r *raw, ts uint64, fr func()) {
			fr()
			r.batch(ts, 2, 2)
		}},
		{"within the batch", func(r *raw, ts uint64, fr func()) {
			r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: ts, NextSeqNo: 2, Count: 2})
			r.send(appMsg{1, "2"})
			fr()
			r.send(appMsg{1, "3"})
		}},
		{"between batches", func(r *raw, ts uint64, fr func()) {
			r.batch(ts, 2, 1)
			fr()
			r.batch(ts, 3, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRawSession(t, fixp.Recoverable, fixp.Recoverable, codec.Some(uint64(1)), func(c *ServerConfig) {
				c.PartialBatchWait = time.Minute
			})
			r := rs.r
			s := rs.session(t)
			if err := s.Finish(bg); err != nil {
				t.Fatal(err)
			}
			r.until(codec.FinishedSendingTemplateID)
			r.send(appMsg{1, "1"})
			r.send(&codec.Sequence{NextSeqNo: 4})
			r.send(appMsg{1, "4"})
			r.send(&codec.FinishedSending{SessionId: r.id, LastSeqNo: codec.Some(uint64(4))})
			rr := r.request(2, 2)
			tc.answer(r, rr.Timestamp, func() {
				r.send(&codec.FinishedReceiving{SessionId: r.id})
				// Neither answered nor terminated: messages 2 and 3 missing.
				r.none(codec.TerminateTemplateID, 50*time.Millisecond)
				if st := s.State(); st != StateEstablished {
					t.Fatalf("state %s", st)
				}
			})
			r.until(codec.FinishedReceivingTemplateID)
			r.terminated(fixp.TerminationFinished, "finished")
			r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCodeFinished})
			rs.h.messages(t, 1, 2, 3, 4)
			rs.h.until(t, "peerfinished")
			rs.h.until(t, "finalized")
			st, _ := rs.st.Open(bg, r.id)
			if state := st.State(); state.Inbound != 4 || !state.InboundFinish.Finished || !state.OutboundFinish.Finished {
				t.Fatalf("stored %+v", state)
			}
		})
	}
}
