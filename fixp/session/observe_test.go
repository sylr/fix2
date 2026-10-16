package session

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/fixp/store/memstore"
)

// frames records what an Observer sees.
type frames struct {
	mu      sync.Mutex
	in, out []Frame
}

func (f *frames) OnInbound(_ *Session, fr Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fr.Msg = bytes.Clone(fr.Msg) // borrowed
	f.in = append(f.in, fr)
}

func (f *frames) OnOutbound(_ *Session, fr Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fr.Msg = bytes.Clone(fr.Msg)
	f.out = append(f.out, fr)
}

// apps returns the sequence numbers of the application messages of schema
// 33 seen.
func apps(fs []Frame) []uint64 {
	var seqs []uint64
	for _, f := range fs {
		if f.Header.SchemaID == 33 {
			seqs = append(seqs, f.Seq)
		}
	}
	return seqs
}

func (f *frames) snapshot() (in, out []Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Frame(nil), f.in...), append([]Frame(nil), f.out...)
}

func TestObserver(t *testing.T) {
	cliObs, srvObs := &frames{}, &frames{}
	p := newPair(t, pairOptions{clientFlow: fixp.Idempotent, serverFlow: fixp.Unsequenced,
		client: func(c *ClientConfig) { c.Observer = cliObs },
		server: func(c *ServerConfig) { c.Observer = srvObs },
	})
	for i := range 3 {
		p.client.Session().Send(bg, appMsg{1, fmt.Sprint(i)})
	}
	for range 3 {
		p.srvH.skipUntil(t, "message")
	}
	eventually(t, func() bool {
		_, out := cliObs.snapshot()
		return len(apps(out)) == 3
	})
	_, out := cliObs.snapshot()
	in, _ := srvObs.snapshot()
	if fmt.Sprint(apps(out)) != "[1 2 3]" || fmt.Sprint(apps(in)) != "[1 2 3]" {
		t.Fatalf("client wrote %v, server read %v", apps(out), apps(in))
	}
	for _, f := range append(out, in...) {
		if f.Connection != 1 || f.Time.IsZero() {
			t.Fatalf("frame %+v", f)
		}
	}
	// The Sequence before them, observed as a session message.
	if h := out[0].Header; h.SchemaID != codec.SchemaID || h.TemplateID != codec.SequenceTemplateID {
		t.Errorf("first written: %+v", h)
	}
}

// Credentials never reach an observer, whenever they come.
func TestObserverRedacts(t *testing.T) {
	obs := &frames{}
	rs := newRawSession(t, fixp.Unsequenced, fixp.Unsequenced, codec.Optional[uint64]{}, func(c *ServerConfig) {
		c.Observer = obs
	})
	rs.r.send(&codec.Negotiate{SessionId: rs.r.id, Timestamp: 1, Credentials: []byte("secret")})
	rs.r.readTemplate(codec.TerminateTemplateID)
	eventually(t, func() bool { in, _ := obs.snapshot(); return len(in) > 0 })
	in, _ := obs.snapshot()
	for _, f := range in {
		if bytes.Contains(f.Msg, []byte("secret")) {
			t.Fatalf("credentials observed: %q", f.Msg)
		}
	}
	var n codec.Negotiate
	if err := n.Decode(in[len(in)-1].Msg); err != nil || len(n.Credentials) != 6 || n.SessionId != rs.r.id {
		t.Fatalf("redacted Negotiate: %+v, %v", n, err)
	}
}

func TestRedact(t *testing.T) {
	for _, m := range []encoder{
		&codec.Negotiate{Credentials: []byte("secret")},
		&codec.NegotiationResponse{Credentials: []byte("secret")},
		&codec.Establish{Credentials: []byte("secret")},
	} {
		b, _ := m.AppendTo(nil)
		r := redact(b)
		if bytes.Contains(r, []byte("secret")) || !bytes.Contains(b, []byte("secret")) || len(r) != len(b) {
			t.Errorf("%T: %q from %q", m, r, b)
		}
	}
	b, _ := (&codec.Establish{Credentials: []byte("secret")}).AppendTo(nil)
	if r := redact(b[:len(b)-2]); bytes.Contains(r, []byte("secr")) {
		t.Errorf("truncated Establish: %q", r)
	}
	other, _ := appMsg{1, "secret"}.AppendTo(nil)
	if r := redact(other); !bytes.Equal(r, other) {
		t.Errorf("another schema's message changed: %q", r)
	}
}

// A Recoverable flow keeps its last MaxStoredMessages messages: earlier
// ones are no longer available.
func TestRetention(t *testing.T) {
	st := memstore.New()
	rs := newRawSession(t, fixp.Unsequenced, fixp.Recoverable, codec.Optional[uint64]{}, func(c *ServerConfig) {
		c.Store = st
		c.MaxStoredMessages = 10
		c.KeepaliveInterval = time.Second
	})
	s := rs.session(t)
	for i := 1; i <= 30; i++ {
		s.Send(bg, appMsg{2, fmt.Sprint(i)})
	}
	rs.r.readApps(30)
	ss, _ := st.Open(bg, rs.r.id)
	if err := ss.Range(bg, 1, 1, func(uint64, []byte) error { return nil }); !errors.Is(err, store.ErrEvicted) {
		t.Fatalf("message 1: %v", err)
	}
	if err := ss.Range(bg, 21, 30, func(uint64, []byte) error { return nil }); err != nil {
		t.Fatalf("the last 10: %v", err)
	}
	var rj codec.RestransmitReject
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 1, FromSeqNo: 1, Count: 2})
	if err := rj.Decode(rs.r.readTemplate(codec.RestransmitRejectTemplateID)); err != nil || string(rj.Reason) != "no longer available" {
		t.Fatalf("RetransmitReject: %+v %q, %v", rj, rj.Reason, err)
	}
	rs.r.send(&codec.RetransmitRequest{SessionId: rs.r.id, Timestamp: 2, FromSeqNo: 25, Count: 2})
	expectStrings(t, rs.r.readApps(2), "retransmission=25+2", "25", "26")
}
