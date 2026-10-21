package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// rawSession is a server and a raw client established on it.
type rawSession struct {
	srv *Server
	h   *recorder
	st  *memstore.Store
	r   *raw
}

func newRawSession(t *testing.T, clientFlow, serverFlow fixp.FlowType, next codec.Optional[uint64], opt func(*ServerConfig)) *rawSession {
	t.Helper()
	l := mem.Listen("server")
	h := newRecorder()
	st := memstore.New()
	srv, cancel, done := newServer(t, l, st, h, serverFlow, opt)
	t.Cleanup(func() { cancel(); <-done })
	r := dialRaw(t, l)
	r.negotiate(clientFlow, "user:alice")
	// An Idempotent flow is numbered by a Sequence, not by the Establish
	// (§4.8).
	idempotent := clientFlow == fixp.Idempotent && next.Valid
	est := next
	if idempotent {
		est = codec.Optional[uint64]{}
	}
	var ack codec.EstablishmentAck
	if _, err := ack.Decode(r.establish(1000, est, "user:alice")); err != nil {
		t.Fatalf("establishing: %v", err)
	}
	h.expect(t, "established")
	if idempotent {
		r.send(&codec.Sequence{NextSeqNo: next.Value})
	}
	return &rawSession{srv: srv, h: h, st: st, r: r}
}

func (rs *rawSession) session(t *testing.T) *Session {
	t.Helper()
	s := rs.srv.Session(rs.r.id)
	if s == nil {
		t.Fatal("no session")
	}
	return s
}

// readApps reads n application messages, skipping heartbeats, and returns
// their bodies, a Sequence's NextSeqNo marked "seq=<n>" before them.
func (r *raw) readApps(n int) []string {
	r.t.Helper()
	var got []string
	for apps := 0; apps < n; {
		h, msg := r.read()
		switch {
		case h.SchemaId != codec.SchemaID:
			got = append(got, string(msg[8:]))
			apps++
		case h.TemplateId == codec.SequenceTemplateID:
			var s codec.Sequence
			s.Decode(msg)
			got = append(got, fmt.Sprintf("seq=%d", s.NextSeqNo))
		case h.TemplateId == codec.RetransmissionTemplateID:
			var rt codec.Retransmission
			rt.Decode(msg)
			got = append(got, fmt.Sprintf("retransmission=%d+%d", rt.NextSeqNo, rt.Count))
		case h.TemplateId == codec.NotAppliedTemplateID:
			var na codec.NotApplied
			na.Decode(msg)
			got = append(got, fmt.Sprintf("notapplied=%d+%d", na.FromSeqNo, na.Count))
			apps++
		case h.TemplateId == codec.UnsequencedHeartbeatTemplateID:
		default:
			r.t.Fatalf("template %d", h.TemplateId)
		}
	}
	return got
}

// dropSequences removes the heartbeats Sequences of the same number, which
// come at any time.
func dropRepeats(got []string) []string {
	var out []string
	for i, g := range got {
		if i > 0 && g == got[i-1] {
			continue
		}
		out = append(out, g)
	}
	return out
}

func expectStrings(t *testing.T, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(dropRepeats(got)) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The server's Recoverable flow answers RetransmitRequest from its store.
func TestRetransmitServed(t *testing.T) {
	rs := newRawSession(t, fixp.Unsequenced, fixp.Recoverable, codec.Optional[uint64]{}, func(c *ServerConfig) {
		c.RetransmitBatch = 3
		c.KeepaliveInterval = time.Second
		c.RejectRetransmitRequestBeyondSent = true // §7.4.5.1's answer
	})
	s := rs.session(t)
	for i := 1; i <= 5; i++ {
		s.Send(bg, appMsg{2, fmt.Sprint(i)})
	}
	expectStrings(t, rs.r.readApps(5), "seq=1", "1", "2", "3", "4", "5")

	// Messages 2 to 5: a batch of 3, the RetransmitBatch.
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 7, FromSeqNo: 2, Count: 4})
	expectStrings(t, rs.r.readApps(3), "retransmission=2+3", "2", "3", "4")
	// Real-time messages after a batch follow a Sequence.
	s.Send(bg, appMsg{2, "6"})
	expectStrings(t, rs.r.readApps(1), "seq=6", "6")
	if n := s.Stats().MessagesRetransmitted.Load(); n != 3 {
		t.Errorf("%d retransmitted", n)
	}

	var rj codec.RestransmitReject
	// Beyond what was sent.
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 8, FromSeqNo: 7, Count: 1})
	if _, err := rj.Decode(rs.r.readTemplate(codec.RestransmitRejectTemplateID)); err != nil || rj.Code != codec.RetransmitRejectCodeOutOfRange || rj.RequestTimestamp != 8 {
		t.Fatalf("beyond: %+v, %v", rj, err)
	}
	// Another session's.
	rs.r.send(&codec.RetransmitRequest{SessionId: fixp.NewSessionID(), Timestamp: 9, FromSeqNo: 1, Count: 1})
	if _, err := rj.Decode(rs.r.readTemplate(codec.RestransmitRejectTemplateID)); err != nil || rj.Code != codec.RetransmitRejectCodeInvalidSession {
		t.Fatalf("other session: %+v, %v", rj, err)
	}
	// No longer kept.
	st, _ := rs.st.Open(bg, rs.r.id)
	st.Evict(bg, 3)
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 10, FromSeqNo: 1, Count: 3})
	if _, err := rj.Decode(rs.r.readTemplate(codec.RestransmitRejectTemplateID)); err != nil || rj.Code != codec.RetransmitRejectCodeOutOfRange || string(rj.Reason) != "no longer available" {
		t.Fatalf("evicted: %+v %q, %v", rj, rj.Reason, err)
	}
	// Still established: a Sequence and real-time messages go on.
	s.Send(bg, appMsg{2, "7"})
	expectStrings(t, rs.r.readApps(1), "seq=7", "7")
}

// The server, receiving a Recoverable flow, asks for what it missed, holds
// what follows, and delivers in order once the gap is filled.
func TestRecoverableGapFilled(t *testing.T) {
	rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), nil)
	r := rs.r
	r.send(appMsg{1, "1"})
	r.send(&codec.Sequence{NextSeqNo: 4})
	r.send(appMsg{1, "4"})
	r.send(appMsg{1, "5"})
	var rr codec.RetransmitRequest
	if _, err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 2 || rr.Count != 2 || rr.SessionId != r.id {
		t.Fatalf("RetransmitRequest: %+v, %v", rr, err)
	}
	if e := rs.h.expect(t, "message"); e.body != "1" {
		t.Fatalf("got %v", e)
	}
	r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: 2, Count: 2})
	r.send(appMsg{1, "2"})
	r.send(appMsg{1, "3"})
	r.send(&codec.Sequence{NextSeqNo: 6})
	r.send(appMsg{1, "6"})
	for i := 2; i <= 6; i++ {
		if e := rs.h.expect(t, "message"); e.body != fmt.Sprint(i) || e.seq != uint64(i) {
			t.Fatalf("message %d: %v", i, e)
		}
	}
	eventually(t, func() bool { st, _ := rs.st.Open(bg, r.id); return st.State().Inbound == 6 })
}

// Messages lost by the peer are skipped only with the handler's consent.
func TestRecoverableLost(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%v", skip), func(t *testing.T) {
			var lost [2]uint64
			rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), nil)
			rs.h.onLost = func(from, count uint64) error {
				lost = [2]uint64{from, count}
				if skip {
					return nil
				}
				return errors.New("cannot skip")
			}
			r := rs.r
			r.send(&codec.Sequence{NextSeqNo: 3})
			r.send(appMsg{1, "3"})
			var rr codec.RetransmitRequest
			if _, err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 1 || rr.Count != 2 {
				t.Fatalf("RetransmitRequest: %+v, %v", rr, err)
			}
			r.send(&codec.RestransmitReject{SessionId: r.id, RequestTimestamp: rr.Timestamp, Code: codec.RetransmitRejectCodeOutOfRange})
			if !skip {
				var term codec.Terminate
				if _, err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil || string(term.Reason) != "application error" {
					t.Fatalf("Terminate: %+v %q, %v", term, term.Reason, err)
				}
				rs.h.expect(t, "unbound")
				if lost != [2]uint64{1, 2} {
					t.Errorf("OnLost %v", lost)
				}
				return
			}
			if e := rs.h.expect(t, "message"); e.body != "3" {
				t.Fatalf("got %v", e)
			}
			if lost != [2]uint64{1, 2} {
				t.Errorf("OnLost %v", lost)
			}
		})
	}
}

// RequestLimitExceeded: the next request asks for fewer.
func TestRecoverableRequestLimit(t *testing.T) {
	rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), nil)
	r := rs.r
	r.send(&codec.Sequence{NextSeqNo: 9})
	r.send(appMsg{1, "9"})
	var rr codec.RetransmitRequest
	if _, err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 1 || rr.Count != 8 {
		t.Fatalf("RetransmitRequest: %+v, %v", rr, err)
	}
	r.send(&codec.RestransmitReject{SessionId: r.id, RequestTimestamp: rr.Timestamp, Code: codec.RetransmitRejectCodeRequestLimitExceeded})
	if _, err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 1 || rr.Count != 4 {
		t.Fatalf("second RetransmitRequest: %+v, %v", rr, err)
	}
	r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: 1, Count: 4})
	for i := 1; i <= 4; i++ {
		r.send(appMsg{1, fmt.Sprint(i)})
	}
	if _, err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 5 || rr.Count != 4 {
		t.Fatalf("third RetransmitRequest: %+v, %v", rr, err)
	}
	r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: 5, Count: 4})
	for i := 5; i <= 8; i++ {
		r.send(appMsg{1, fmt.Sprint(i)})
	}
	for i := 1; i <= 9; i++ {
		if e := rs.h.expect(t, "message"); e.body != fmt.Sprint(i) {
			t.Fatalf("message %d: %v", i, e)
		}
	}
}

// A gap the establishment reveals is asked for once established.
func TestRecoverableGapAtEstablishment(t *testing.T) {
	rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(3)), nil)
	var rr codec.RetransmitRequest
	if _, err := rr.Decode(rs.r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 1 || rr.Count != 2 {
		t.Fatalf("RetransmitRequest: %+v, %v", rr, err)
	}
}

// Messages held beyond the bound are dropped, and asked for again.
func TestRecoverableHeldBound(t *testing.T) {
	rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), func(c *ServerConfig) {
		c.MaxHeldMessages = 1
	})
	r := rs.r
	r.send(&codec.Sequence{NextSeqNo: 2})
	r.send(appMsg{1, "2"}) // held
	r.send(appMsg{1, "3"}) // dropped
	var rr codec.RetransmitRequest
	if _, err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 1 || rr.Count != 1 {
		t.Fatalf("RetransmitRequest: %+v, %v", rr, err)
	}
	r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: 1, Count: 1})
	r.send(appMsg{1, "1"})
	// 1 and 2 delivered, 3 asked for again.
	if _, err := rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 3 || rr.Count != 1 {
		t.Fatalf("second RetransmitRequest: %+v, %v", rr, err)
	}
	r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: 3, Count: 1})
	r.send(appMsg{1, "3"})
	for i := 1; i <= 3; i++ {
		if e := rs.h.expect(t, "message"); e.body != fmt.Sprint(i) {
			t.Fatalf("message %d: %v", i, e)
		}
	}
}

func TestRecoverableViolations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		send   func(r *raw)
		reason string
	}{
		{"Retransmission unasked", func(r *raw) {
			r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: 1, NextSeqNo: 1, Count: 1})
		}, "Retransmission answering no request of ours"},
		{"RetransmitReject unasked", func(r *raw) {
			r.send(&codec.RestransmitReject{SessionId: r.id, RequestTimestamp: 1})
		}, "RetransmitReject answering no request of ours"},
		{"Sequence in a batch", func(r *raw) {
			r.send(&codec.Sequence{NextSeqNo: 3})
			var rr codec.RetransmitRequest
			rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID))
			r.send(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: 1, Count: 2})
			r.send(appMsg{1, "1"})
			r.send(&codec.Sequence{NextSeqNo: 3})
		}, "Sequence before the end of a Retransmission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), nil)
			tc.send(rs.r)
			// The Terminate, maybe after the answer to a request.
			var msg []byte
			for {
				h, m := rs.r.read()
				if h.SchemaId == codec.SchemaID && h.TemplateId == codec.TerminateTemplateID {
					msg = m
					break
				}
			}
			var term codec.Terminate
			if _, err := term.Decode(msg); err != nil || string(term.Reason) != tc.reason {
				t.Fatalf("Terminate: %+v %q, %v", term, term.Reason, err)
			}
		})
	}
}

// A second RetransmitRequest while one is answered terminates with
// ReRequestInProgress.
func TestReRequestInProgress(t *testing.T) {
	rs := newRawSession(t, fixp.Unsequenced, fixp.Recoverable, codec.Optional[uint64]{}, nil)
	s := rs.session(t)
	s.Send(bg, appMsg{2, "1"})
	rs.r.readApps(1)
	// The first is answered by the writer, the second read before.
	c := func() *conn { s.mu.Lock(); defer s.mu.Unlock(); return s.conn }()
	c.retransmitting.Store(1 << 62)
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 1, FromSeqNo: 1, Count: 1})
	var term codec.Terminate
	if _, err := term.Decode(rs.r.readTemplate(codec.TerminateTemplateID)); err != nil || term.Code != codec.TerminationCodeReRequestInProgress {
		t.Fatalf("Terminate: %+v, %v", term, err)
	}
}

// On an Idempotent flow, a gap is reported with NotApplied, the handler
// told, and passed.
func TestIdempotentGap(t *testing.T) {
	for _, serverFlow := range []fixp.FlowType{fixp.Idempotent, fixp.Unsequenced} {
		t.Run(serverFlow.String(), func(t *testing.T) {
			rs := newRawSession(t, fixp.Idempotent, serverFlow, codec.Some(uint64(1)), nil)
			r := rs.r
			r.send(appMsg{1, "1"})
			r.send(&codec.Sequence{NextSeqNo: 5})
			r.send(appMsg{1, "5"})
			r.send(appMsg{1, "6"})
			if e := rs.h.expect(t, "message"); e.body != "1" {
				t.Fatalf("got %v", e)
			}
			if e := rs.h.expect(t, "gap"); e.seq != 2 || e.body != "3" {
				t.Fatalf("gap %v", e)
			}
			for _, want := range []string{"5", "6"} {
				if e := rs.h.expect(t, "message"); e.body != want {
					t.Fatalf("got %v", e)
				}
			}
			if serverFlow == fixp.Idempotent {
				expectStrings(t, r.readApps(1), "seq=1", "notapplied=2+3")
			}
			st, _ := rs.st.Open(bg, r.id)
			eventually(t, func() bool { return st.State().Inbound == 6 })
			if serverFlow == fixp.Idempotent && st.State().NextOutbound != 2 {
				t.Errorf("NotApplied not stored: next %d", st.State().NextOutbound)
			}
		})
	}
}

// A client restarted with messages stored and never written: the server
// asks for them, the client retransmits them from its store.
func TestRecoverAfterClientRestart(t *testing.T) {
	l := mem.Listen("server")
	srvH := newRecorder()
	srvStore, cliStore := memstore.New(), memstore.New()
	_, cancelSrv, srvDone := newServer(t, l, srvStore, srvH, fixp.Unsequenced, nil)
	defer func() { cancelSrv(); <-srvDone }()

	cliH := newRecorder()
	c := newClient(t, l, cliStore, cliH, fixp.Recoverable, nil)
	cancelCli, cliDone := runClient(c)
	cliH.expect(t, "established")
	srvH.expect(t, "established")
	c.Session().Send(bg, appMsg{1, "1"})
	srvH.expect(t, "message")
	// Stored, never written: the session is unbound, then the process ends.
	c.Session().Terminate(bg, "")
	cliH.expect(t, "unbound")
	srvH.expect(t, "unbound")
	cancelCli()
	<-cliDone
	stored, _ := cliStore.Open(bg, c.Session().ID())
	for i := 2; i <= 4; i++ {
		stored.Append(bg, uint64(i), []byte(fmt.Sprintf("\x01\x00\x01\x00\x21\x00\x00\x00%d", i)))
	}

	cliH = newRecorder()
	c = newClient(t, l, cliStore, cliH, fixp.Recoverable, nil)
	cancelCli, cliDone = runClient(c)
	defer func() { cancelCli(); <-cliDone }()
	cliH.expect(t, "established")
	srvH.expect(t, "established")
	for i := 2; i <= 4; i++ {
		if e := srvH.expect(t, "message"); e.seq != uint64(i) || e.body != fmt.Sprint(i) {
			t.Fatalf("message %d: %v", i, e)
		}
	}
	if seq, err := c.Session().Send(bg, appMsg{1, "5"}); err != nil || seq != 5 {
		t.Fatalf("Send: %d, %v", seq, err)
	}
	if e := srvH.expect(t, "message"); e.seq != 5 {
		t.Fatalf("got %v", e)
	}
}

// slowRangeStore returns from Range late, after its last message: the
// peer may ask again before.
type slowRangeStore struct{ *memstore.Store }

func (s slowRangeStore) Create(ctx context.Context, m store.Meta) (store.Session, error) {
	ss, err := s.Store.Create(ctx, m)
	return slowRangeSession{ss}, err
}

type slowRangeSession struct{ store.Session }

func (s slowRangeSession) Range(ctx context.Context, begin, end uint64, fn func(uint64, []byte) error) error {
	err := s.Session.Range(ctx, begin, end, fn)
	time.Sleep(100 * time.Millisecond)
	return err
}

// The next request, sent on reading the last message of a batch, is not
// one in progress.
func TestRetransmitRequestAfterBatch(t *testing.T) {
	rs := newRawSession(t, fixp.Unsequenced, fixp.Recoverable, codec.Optional[uint64]{}, func(c *ServerConfig) {
		c.Store = slowRangeStore{memstore.New()}
		c.KeepaliveInterval = time.Second
	})
	s := rs.session(t)
	for i := 1; i <= 4; i++ {
		s.Send(bg, appMsg{2, fmt.Sprint(i)})
	}
	rs.r.readApps(4)
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 1, FromSeqNo: 1, Count: 2})
	expectStrings(t, rs.r.readApps(2), "retransmission=1+2", "1", "2")
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 2, FromSeqNo: 3, Count: 2})
	expectStrings(t, rs.r.readApps(2), "retransmission=3+2", "3", "4")
}

// gatedStore makes its Ranges wait on gates: the k-th waits on before[k]
// before reading, and on after[k] before returning.
type gatedStore struct {
	*memstore.Store
	mu            sync.Mutex
	calls         int
	before, after []chan struct{}
}

func (g *gatedStore) Create(ctx context.Context, m store.Meta) (store.Session, error) {
	ss, err := g.Store.Create(ctx, m)
	return gatedSession{ss, g}, err
}

type gatedSession struct {
	store.Session
	g *gatedStore
}

func (s gatedSession) Range(ctx context.Context, begin, end uint64, fn func(uint64, []byte) error) error {
	s.g.mu.Lock()
	k := s.g.calls
	s.g.calls++
	s.g.mu.Unlock()
	if k < len(s.g.before) {
		<-s.g.before[k]
	}
	err := s.Session.Range(ctx, begin, end, fn)
	if k < len(s.g.after) {
		<-s.g.after[k]
	}
	return err
}

// A batch ending late does not release the next one's ownership: a third
// request while the second is answered is one in progress.
func TestRetransmitOwnership(t *testing.T) {
	open := make(chan struct{})
	close(open)
	g := &gatedStore{
		Store:  memstore.New(),
		before: []chan struct{}{open, make(chan struct{})},
		after:  []chan struct{}{make(chan struct{}), open},
	}
	rs := newRawSession(t, fixp.Unsequenced, fixp.Recoverable, codec.Optional[uint64]{}, func(c *ServerConfig) {
		c.Store = g
		c.KeepaliveInterval = time.Second
	})
	s := rs.session(t)
	for i := 1; i <= 4; i++ {
		s.Send(bg, appMsg{2, fmt.Sprint(i)})
	}
	rs.r.readApps(4)
	// A: answered, its Range not returned.
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 1, FromSeqNo: 1, Count: 2})
	expectStrings(t, rs.r.readApps(2), "retransmission=1+2", "1", "2")
	// B: admitted, waiting for the writer, A's Range returns.
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 2, FromSeqNo: 3, Count: 2})
	c := func() *conn { s.mu.Lock(); defer s.mu.Unlock(); return s.conn }()
	eventually(t, func() bool { return c.retransmitting.Load() == 2 })
	close(g.after[0])
	time.Sleep(50 * time.Millisecond)
	// C, while B is in progress.
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 3, FromSeqNo: 1, Count: 1})
	time.Sleep(50 * time.Millisecond)
	// B's batch, let go, yields to the Terminate.
	close(g.before[1])
	var term codec.Terminate
	if _, err := term.Decode(rs.r.readTemplate(codec.TerminateTemplateID)); err != nil || term.Code != codec.TerminationCodeReRequestInProgress {
		t.Fatalf("Terminate: %+v, %v", term, err)
	}
}

// A peer asking faster than it reads the answers is cut off.
func TestRetransmitRequestFlood(t *testing.T) {
	rs := newRawSession(t, fixp.Unsequenced, fixp.Recoverable, codec.Optional[uint64]{}, nil)
	other := fixp.NewSessionID()
	// Unread, the answers fill the transport's buffer (1 MiB), then the
	// queue: the server cuts the connection, and the writes fail.
	for i := range 100_000 {
		f, _ := frame(&codec.RetransmitRequest{SessionId: other, Timestamp: uint64(i + 1), FromSeqNo: 1, Count: 1})
		if _, err := rs.r.c.Write(f); err != nil {
			break
		}
	}
	for {
		h, msg := rs.r.read()
		if h.TemplateId == codec.TerminateTemplateID {
			var term codec.Terminate
			term.Decode(msg)
			if string(term.Reason) != "requests faster than their answers are read" {
				t.Fatalf("Terminate %q", term.Reason)
			}
			return
		}
	}
}

// failingWriteConn fails the first Write holding failOn, closing: the
// connection ends with a message taken from the queue and never written.
type failingWriteConn struct {
	transport.Conn
	failed *atomic.Bool
	failOn string
}

func (c failingWriteConn) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(c.failOn)) && c.failed.CompareAndSwap(false, true) {
		c.Conn.Close()
		return 0, errors.New("write failed")
	}
	return c.Conn.Write(b)
}

// A message the writer took from the queue and failed to write is
// retransmitted when asked for: it is not beyond what was sent.
func TestUnwrittenMessageRetransmitted(t *testing.T) {
	l := mem.Listen("server")
	srvH := newRecorder()
	_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Unsequenced, nil)
	defer func() { cancelSrv(); <-srvDone }()
	var dials atomic.Int32
	failed := &atomic.Bool{}
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		c, err := l.Dial(ctx)
		if err != nil || dials.Add(1) > 1 {
			return c, err
		}
		// The write holding message 2 fails, message 1 with it or not.
		return failingWriteConn{Conn: c, failed: failed, failOn: "message 2"}, nil
	})
	cliH := newRecorder()
	c := newClient(t, dialer, memstore.New(), cliH, fixp.Recoverable, func(c *ClientConfig) {
		c.KeepaliveInterval = time.Second
	})
	cancelCli, cliDone := runClient(c)
	defer func() { cancelCli(); <-cliDone }()
	cliH.expect(t, "established")
	for i := 1; i <= 3; i++ {
		if _, err := c.Session().Send(bg, appMsg{1, fmt.Sprintf("message %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// One unbinding, the failed write's: the retransmission succeeds.
	unbound := 0
	for i := 1; i <= 3; i++ {
		e := srvH.next(t)
		for e.kind != "message" {
			if e.kind == "unbound" {
				unbound++
			}
			e = srvH.next(t)
		}
		if e.seq != uint64(i) || e.body != fmt.Sprintf("message %d", i) {
			t.Fatalf("message %d: %v", i, e)
		}
	}
	if unbound != 1 {
		t.Fatalf("%d unbindings, want the failed write's alone", unbound)
	}
}

// markedSlowConn delays the writes holding slowOn.
type markedSlowConn struct {
	transport.Conn
	slowOn string
	delay  time.Duration
}

func (c markedSlowConn) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(c.slowOn)) {
		time.Sleep(c.delay)
	}
	return c.Conn.Write(b)
}

// The peer's messages coming while what precedes our Terminate is still
// being written are delivered: only once our Terminate goes is the rest
// ignored, waiting for its answer.
func TestMessagesBeforeOurTerminate(t *testing.T) {
	l := mem.Listen("server")
	srvH := newRecorder()
	srvH.onMessage = func(ctx context.Context, s *Session, msg []byte, _ Inbound) error {
		if string(msg[8:]) != "ping" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
		_, err := s.Send(ctx, appMsg{1, "pong"})
		return err
	}
	_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Unsequenced, nil)
	defer func() { cancelSrv(); <-srvDone }()
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		c, err := l.Dial(ctx)
		if err != nil {
			return nil, err
		}
		return markedSlowConn{Conn: c, slowOn: "slow", delay: 300 * time.Millisecond}, nil
	})
	cliH := newRecorder()
	c := newClient(t, dialer, memstore.New(), cliH, fixp.Unsequenced, func(c *ClientConfig) {
		c.KeepaliveInterval = time.Second
	})
	cancelCli, cliDone := runClient(c)
	defer func() { cancelCli(); <-cliDone }()
	cliH.expect(t, "established")
	if _, err := c.Session().Send(bg, appMsg{1, "ping"}); err != nil {
		t.Fatal(err)
	}
	srvH.until(t, "message") // the pong follows in 100ms
	if _, err := c.Session().Send(bg, appMsg{1, "slow"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Session().Terminate(bg, ""); err != nil {
		t.Fatal(err)
	}
	if e := cliH.next(t); e.kind != "message" || e.body != "pong" {
		t.Fatalf("event %v, not the pong", e)
	}
}

// partialWriteConn writes the write holding "three" but its last frame,
// then fails; the write holding "one" waits, the others queue behind.
type partialWriteConn struct {
	transport.Conn
}

func (c partialWriteConn) Write(b []byte) (int, error) {
	switch {
	case bytes.Contains(b, []byte("three")):
		last := sofh.HeaderLen + 8 + len("three")
		n, _ := c.Conn.Write(b[:len(b)-last])
		c.Conn.Close()
		return n, errors.New("write cut")
	case bytes.Contains(b, []byte("one")):
		time.Sleep(100 * time.Millisecond)
	}
	return c.Conn.Write(b)
}

// outbound records the application messages written.
type outbound struct {
	mu   sync.Mutex
	msgs []string
}

func (o *outbound) OnInbound(*Session, Frame) {}
func (o *outbound) OnOutbound(_ *Session, f Frame) {
	if f.Header.SchemaID == 33 {
		o.mu.Lock()
		o.msgs = append(o.msgs, string(f.Msg[8:]))
		o.mu.Unlock()
	}
}

// A write cut short: the frames it wrote whole are observed and counted,
// the one cut not.
func TestPartialWriteObserved(t *testing.T) {
	l := mem.Listen("server")
	srvH := newRecorder()
	_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Unsequenced, nil)
	defer func() { cancelSrv(); <-srvDone }()
	var dials atomic.Int32
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		c, err := l.Dial(ctx)
		if err != nil || dials.Add(1) > 1 {
			return c, err
		}
		return partialWriteConn{c}, nil
	})
	cliH := newRecorder()
	obs := &outbound{}
	c := newClient(t, dialer, memstore.New(), cliH, fixp.Unsequenced, func(c *ClientConfig) {
		c.KeepaliveInterval = time.Second
		c.Observer = obs
	})
	cancelCli, cliDone := runClient(c)
	defer func() { cancelCli(); <-cliDone }()
	cliH.expect(t, "established")
	for _, m := range []string{"one", "two", "three"} {
		if _, err := c.Session().Send(bg, appMsg{1, m}); err != nil {
			t.Fatal(err)
		}
		if m == "one" {
			time.Sleep(20 * time.Millisecond) // its write begun, waiting
		}
	}
	cliH.until(t, "unbound")
	obs.mu.Lock()
	got := strings.Join(obs.msgs, ",")
	obs.mu.Unlock()
	if got != "one,two" || c.Session().Stats().MessagesSent.Load() != 2 {
		t.Fatalf("observed %q, %d counted", got, c.Session().Stats().MessagesSent.Load())
	}
}

// A queue never empty, each message waiting for a slow Sync, still
// writes within a quarter of a keepalive interval: the peer sees traffic.
// On synctest's fake clock: on the real one, a heartbeat or a Sync late
// under load lapses an end's interval, which is not what this checks.
func TestBatchAgeBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := mem.Listen("server")
		srvH := &countingHandler{}
		_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Unsequenced, nil)
		defer func() { cancelSrv(); <-srvDone }()
		cliH := newRecorder()
		c := newClient(t, l, slowSyncStore{memstore.New(), 10 * time.Millisecond}, cliH, fixp.Recoverable, func(c *ClientConfig) {
			c.KeepaliveInterval = 100 * time.Millisecond
			c.KeepaliveLeniency = 1.5
		})
		cancelCli, cliDone := runClient(c)
		defer func() { cancelCli(); <-cliDone }()
		cliH.expect(t, "established")
		ctx, stop := context.WithTimeout(bg, time.Second)
		defer stop()
		// A message a millisecond: some come during each Sync, the queue
		// never empty, 64 KiB seconds away.
		for ctx.Err() == nil {
			c.Session().Send(ctx, appMsg{1, "x"})
			time.Sleep(time.Millisecond)
		}
		if srvH.unbound.Load() != 0 || srvH.n.Load() == 0 {
			t.Fatalf("%d unbindings, %d messages received", srvH.unbound.Load(), srvH.n.Load())
		}
	})
}

// countingHandler counts the messages and unbindings.
type countingHandler struct {
	BaseHandler
	n, unbound atomic.Int64
}

func (h *countingHandler) OnMessage(context.Context, *Session, []byte, Inbound) error {
	h.n.Add(1)
	return nil
}

func (h *countingHandler) OnUnbound(context.Context, *Session, error) { h.unbound.Add(1) }

// The peer's Terminate coming while what precedes ours is written: one
// Terminate goes, the answer to theirs, then the connection closes.
func TestTerminatesCrossingDuringFlush(t *testing.T) {
	l := mem.Listen("server")
	cliH := newRecorder()
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		c, err := l.Dial(ctx)
		if err != nil {
			return nil, err
		}
		return markedSlowConn{Conn: c, slowOn: "slow", delay: 300 * time.Millisecond}, nil
	})
	c := newClient(t, dialer, memstore.New(), cliH, fixp.Unsequenced, func(c *ClientConfig) {
		c.HandshakeTimeout = time.Second
		c.KeepaliveInterval = time.Second
	})
	cancel, done := runClient(c)
	defer func() { cancel(); <-done }()
	r := acceptRaw(t, l)
	r.answerNegotiate(fixp.Unsequenced)
	e := r.readEstablish()
	r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000})
	cliH.expect(t, "established")
	if _, err := c.Session().Send(bg, appMsg{1, "slow"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Session().Terminate(bg, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the write of "slow" waiting
	r.send(&codec.Terminate{SessionId: r.id, Code: codec.TerminationCode(fixp.TerminationFinished)})
	terminates := 0
	r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, msg, err := r.r.Next()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("connection not closed")
			}
			break
		}
		if h, _, _ := codec.ReadHeader(msg); h.SchemaId == codec.SchemaID && h.TemplateId == codec.TerminateTemplateID {
			terminates++
		}
	}
	if terminates != 1 {
		t.Fatalf("%d Terminates", terminates)
	}
}

// slowSyncStore makes its sessions' Sync wait: the writer, syncing a
// message, finds the next one queued when it returns.
type slowSyncStore struct {
	store.Store
	delay time.Duration
}

type slowSyncSession struct {
	store.Session
	delay time.Duration
}

func (s slowSyncStore) Create(ctx context.Context, m store.Meta) (store.Session, error) {
	ss, err := s.Store.Create(ctx, m)
	if err != nil {
		return nil, err
	}
	return slowSyncSession{ss, s.delay}, nil
}

func (s slowSyncStore) Open(ctx context.Context, id fixp.SessionID) (store.Session, error) {
	ss, err := s.Store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return slowSyncSession{ss, s.delay}, nil
}

func (s slowSyncSession) Sync(ctx context.Context) error {
	time.Sleep(s.delay)
	return s.Session.Sync(ctx)
}

// Messages written in one write that fails stay retransmittable, however
// short the retention: the store evicts what was written, not what was
// taken out of the queue.
func TestRetentionAfterFailedWrite(t *testing.T) {
	l := mem.Listen("server")
	srvH := newRecorder()
	_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Unsequenced, nil)
	defer func() { cancelSrv(); <-srvDone }()
	var dials atomic.Int32
	failed := &atomic.Bool{}
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		c, err := l.Dial(ctx)
		if err != nil || dials.Add(1) > 1 {
			return c, err
		}
		return failingWriteConn{Conn: c, failed: failed, failOn: "message 1"}, nil
	})
	cliH := newRecorder()
	c := newClient(t, dialer, slowSyncStore{memstore.New(), 50 * time.Millisecond}, cliH, fixp.Recoverable, func(c *ClientConfig) {
		c.KeepaliveInterval = time.Second
		c.MaxStoredMessages = 1
	})
	cancelCli, cliDone := runClient(c)
	defer func() { cancelCli(); <-cliDone }()
	cliH.expect(t, "established")
	for i := 1; i <= 2; i++ {
		if _, err := c.Session().Send(bg, appMsg{1, fmt.Sprintf("message %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Within a few connections: a message lost would make them endless.
	deadline := time.Now().Add(5 * time.Second)
	for i := 1; i <= 2; {
		if time.Now().After(deadline) {
			t.Fatalf("message %d not delivered", i)
		}
		e := srvH.next(t)
		if e.kind != "message" {
			continue
		}
		if e.seq != uint64(i) || e.body != fmt.Sprintf("message %d", i) {
			t.Fatalf("message %d: %v", i, e)
		}
		i++
	}
	if !failed.Load() {
		t.Fatal("no write failed")
	}
}

func TestRecoveryViolations(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		clientFlow, serverFlow fixp.FlowType
		send                   func(r *raw)
		reason                 string
	}{
		{"NotApplied of a Recoverable flow", fixp.Idempotent, fixp.Recoverable, func(r *raw) {
			r.send(&codec.Sequence{NextSeqNo: 1})
			r.send(&codec.NotApplied{FromSeqNo: 1, Count: 1})
		}, "NotApplied of our Recoverable flow"},
		{"RetransmitReject of another session", fixp.Recoverable, fixp.Unsequenced, func(r *raw) {
			r.send(&codec.Sequence{NextSeqNo: 3})
			var rr codec.RetransmitRequest
			rr.Decode(r.readTemplate(codec.RetransmitRequestTemplateID))
			r.send(&codec.RestransmitReject{SessionId: fixp.NewSessionID(), RequestTimestamp: rr.Timestamp})
		}, "RetransmitReject answering no request of ours"},
		{"application message after a RetransmitRequest", fixp.Idempotent, fixp.Recoverable, func(r *raw) {
			r.send(&codec.Sequence{NextSeqNo: 1})
			r.send(appMsg{1, "1"})
			r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: 1, FromSeqNo: 5, Count: 1})
			r.send(appMsg{1, "2"})
		}, "application message before a Sequence"},
		{"a gap NotApplied cannot report", fixp.Idempotent, fixp.Idempotent, func(r *raw) {
			r.send(&codec.Sequence{NextSeqNo: 1 << 40})
		}, "a gap of 1099511627775 messages, from 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A request beyond what was sent answered, not terminated: the
			// violation tested is the one after it.
			rs := newRawSession(t, tc.clientFlow, tc.serverFlow, codec.Optional[uint64]{}, func(c *ServerConfig) { c.RejectRetransmitRequestBeyondSent = true })
			tc.send(rs.r)
			// The Terminate, maybe after the answer to a request.
			var msg []byte
			for {
				h, m := rs.r.read()
				if h.SchemaId == codec.SchemaID && h.TemplateId == codec.TerminateTemplateID {
					msg = m
					break
				}
			}
			var term codec.Terminate
			if _, err := term.Decode(msg); err != nil || string(term.Reason) != tc.reason {
				t.Fatalf("Terminate: %+v %q, %v", term, term.Reason, err)
			}
		})
	}
}
