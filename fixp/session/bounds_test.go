package session

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/sofh"
)

// Nulls at the wire's bounds: what we accept and what we send never holds
// a required field's null (SBE §4.15).

// The message header's fields are required uint16s: a null Version is no
// newer version, a null BlockLength no padded block. Checked before
// anything acts on the message: an Applied's sequence number is not
// admitted ("template 17: ...", not "Applied: ...").
func TestMalformedHeader(t *testing.T) {
	nullBlock := func(t testing.TB) []byte {
		msg, _ := (&codec.Sequence{NextSeqNo: 1}).AppendTo(nil)
		binary.LittleEndian.PutUint16(msg[0:], math.MaxUint16)
		msg = append(msg, make([]byte, math.MaxUint16-codec.SequenceBlockLength)...)
		f, err := sofh.AppendFrame(nil, sofh.SBE10LittleEndian, msg)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, tc := range []struct {
		name   string
		frame  func(t testing.TB) []byte
		reason string
	}{
		{"Sequence's Version null", func(t testing.TB) []byte {
			return rawFrame(t, &codec.Sequence{NextSeqNo: 1}, math.MaxUint16, 0)
		}, "template 8: header's Version null"},
		{"Sequence's BlockLength null", nullBlock, "template 8: header's BlockLength null"},
		{"Applied's Version null", func(t testing.TB) []byte {
			return rawFrame(t, &codec.Applied{FromSeqNo: 1, Count: 1}, math.MaxUint16, 0)
		}, "template 17: header's Version null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, _ := rawClient(t, fixp.Idempotent, fixp.Recoverable, 1000)
			r.sendBytes(tc.frame(t))
			r.terminated(fixp.TerminationUnspecifiedError, tc.reason)
		})
	}
}

// Keepalive intervals end at 2^32-2 ms: 2^32-1 is the null of a required
// uint32, which our own decoder refuses.
func TestKeepaliveIntervalBound(t *testing.T) {
	const ms = time.Millisecond
	for _, tc := range []struct {
		ka, max time.Duration
		field   string
	}{
		{(1<<32 - 2) * ms, (1<<32 - 2) * ms, ""},
		{(1<<32 - 1) * ms, (1<<32 - 2) * ms, "KeepaliveInterval"},
		{time.Second, (1<<32 - 1) * ms, "MaxKeepalive"},
	} {
		_, err := ClientConfig{Config: Config{Handler: BaseHandler{}, KeepaliveInterval: tc.ka, MaxKeepalive: tc.max}, Store: memstore.New()}.resolve()
		var ce *ConfigError
		if tc.field == "" && err != nil || tc.field != "" && (!errors.As(err, &ce) || ce.Field != tc.field) {
			t.Errorf("KeepaliveInterval %v, MaxKeepalive %v: %v, want %q", tc.ka, tc.max, err, tc.field)
		}
	}
	if millis(maxKeepalive) == math.MaxUint32 {
		t.Error("the longest keepalive interval encodes as null")
	}
}

// Counts we send never hold the null 2^32-1: RetransmitBatch is bounded
// below it, a RetransmitRequest asks 2^32-2 at most, a gap NotApplied
// cannot report is refused before anything is sent or recorded, SendApplied
// refuses its arguments' nulls.
func TestCountsNeverNull(t *testing.T) {
	// RetransmitBatch's int holds 2^32-1 on 64-bit platforms only.
	wide := strconv.IntSize >= 64
	t.Run("RetransmitBatch", func(t *testing.T) {
		if !wide {
			t.Skip("RetransmitBatch's int holds no 2^32-1")
		}
		for _, tc := range []struct {
			batch uint64
			ok    bool
		}{{math.MaxUint32 - 1, true}, {math.MaxUint32, false}} {
			_, err := ServerConfig{Config: Config{Handler: BaseHandler{}, RetransmitBatch: int(tc.batch)}, Store: memstore.New(), Authenticate: authenticate}.resolve()
			var ce *ConfigError
			if tc.ok && err != nil || !tc.ok && (!errors.As(err, &ce) || ce.Field != "RetransmitBatch") {
				t.Errorf("RetransmitBatch %d: %v", tc.batch, err)
			}
		}
	})
	t.Run("RetransmitRequest", func(t *testing.T) {
		if !wide {
			t.Skip("RetransmitBatch's int holds no 2^32-2")
		}
		batch := uint64(math.MaxUint32 - 1)
		rs := newRawSession(t, fixp.Recoverable, fixp.Unsequenced, codec.Some(uint64(1)), func(c *ServerConfig) { c.RetransmitBatch = int(batch) })
		rs.r.send(&codec.Sequence{NextSeqNo: math.MaxUint32 + 10})
		var rr codec.RetransmitRequest
		if _, err := rr.Decode(rs.r.readTemplate(codec.RetransmitRequestTemplateID)); err != nil || rr.FromSeqNo != 1 || rr.Count != math.MaxUint32-1 {
			t.Fatalf("RetransmitRequest %+v, %v", rr, err)
		}
	})
	t.Run("NotApplied", func(t *testing.T) {
		for _, tc := range []struct {
			gap uint64
			ok  bool
		}{{math.MaxUint32 - 1, true}, {math.MaxUint32, false}} {
			r, h, srv, _ := rawClient(t, fixp.Idempotent, fixp.Idempotent, 1000) // Sequence 1 sent
			r.send(&codec.Sequence{NextSeqNo: 1 + tc.gap})
			if !tc.ok {
				r.terminated(fixp.TerminationUnspecifiedError, fmt.Sprintf("a gap of %d messages, from 1", tc.gap))
				h.until(t, "unbound")
				s := srv.Session(r.id)
				s.mu.Lock()
				st := s.st
				s.mu.Unlock()
				if in := st.State().Inbound; in != 0 {
					t.Fatalf("frontier moved to %d", in)
				}
				continue
			}
			var na codec.NotApplied
			if _, err := na.Decode(r.until(codec.NotAppliedTemplateID)); err != nil || na.FromSeqNo != 1 || uint64(na.Count) != tc.gap {
				t.Fatalf("NotApplied %+v, %v", na, err)
			}
		}
	})
	t.Run("SendApplied", func(t *testing.T) {
		p := newPair(t, pairOptions{clientFlow: fixp.Idempotent, serverFlow: fixp.Idempotent})
		cs := p.client.Session()
		for _, tc := range []struct {
			from  uint64
			count uint32
		}{{math.MaxUint64, 1}, {1, math.MaxUint32}} {
			if _, err := cs.SendApplied(bg, tc.from, tc.count); !errors.Is(err, ErrInvalidApplied) {
				t.Fatalf("SendApplied(%d, %d): %v", tc.from, tc.count, err)
			}
		}
		if _, err := cs.SendApplied(bg, math.MaxUint64-1, math.MaxUint32-1); err != nil {
			t.Fatal(err)
		}
		if e := p.srvH.until(t, "applied"); e.seq != math.MaxUint64-1 || e.body != fmt.Sprint(uint32(math.MaxUint32-1)) {
			t.Fatalf("applied %v", e)
		}
	})
}

// An Applied for messages assigned and queued, not yet out of the queue, is
// beyond what was sent: the writer's watermark judges it, as it does a
// RetransmitRequest.
func TestAppliedBeyondWritten(t *testing.T) {
	var log syncBuffer
	r, h, srv, _ := rawClientWith(t, fixp.Idempotent, fixp.Idempotent, 1000, func(c *ServerConfig) {
		c.Logger = slog.New(slog.NewTextHandler(&log, nil))
	})
	s := srv.Session(r.id)
	// Larger than the connection's buffer: the writer blocks on it, the
	// raw peer not reading.
	if _, err := s.Send(bg, appMsg{1, strings.Repeat("x", 2<<20)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.written.Load() == 1 })
	if seq, err := s.Send(bg, appMsg{1, "2"}); err != nil || seq != 2 {
		t.Fatalf("Send: %d, %v", seq, err)
	}
	r.send(&codec.Applied{FromSeqNo: 1, Count: 1})
	h.until(t, "applied")
	if strings.Contains(log.String(), "beyond what was sent") {
		t.Fatalf("Applied of message 1, written, warned: %s", log.String())
	}
	r.send(&codec.Applied{FromSeqNo: 2, Count: 1})
	h.until(t, "applied")
	if !strings.Contains(log.String(), "Applied beyond what was sent") {
		t.Fatalf("Applied of message 2, queued, not warned: %s", log.String())
	}
	if w := s.written.Load(); w != 1 {
		t.Fatalf("written %d: the writer was not blocked", w)
	}
}
